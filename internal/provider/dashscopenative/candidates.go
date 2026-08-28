package dashscopenative

import (
	"github.com/yobo2u/omugw/internal/canonical"
	nativewire "github.com/yobo2u/omugw/internal/protocol/dashscopenative"
	"github.com/yobo2u/omugw/internal/protocol/openaichat"
)

// candidateState 是多候选流的跨帧状态。
//
// Native 一帧同时携带**全部候选**，候选序号只能取 output.choices 的数组下标
// ——Native 没有官方 index 字段。跨帧顺序与长度是否稳定未文档化，因此这里对
// 任何长度变化都 fail-closed：补齐或截断都要靠猜，而猜错的后果是两条候选的
// 内容张冠李戴，客户端却只看到一个正常的 200。
type candidateState struct {
	// seen 按数组下标记录各候选的跨帧状态，长度即已确立的候选数。
	seen []candidate
}

// candidate 是单个候选的跨帧状态。
type candidate struct {
	roleSent bool
	finished bool

	// tools 按工具调用的数组下标记录「身份是否已发」。ID 与 name 只在该调用
	// 首帧发一次，之后只续发 arguments 片段。
	tools []bool
}

// step 消费一个 Native 帧的候选数组，产出该帧对应的 Chat chunk choices。
//
// 一帧一条 chunk，帧内每个候选各占一个 choices 条目。不按候选拆成多条 chunk：
// 那会成倍放大帧数且没有收益。
func (s *candidateState) step(frame []nativewire.Choice) ([]openaichat.ChunkChoice, error) {
	if err := s.reconcile(len(frame)); err != nil {
		return nil, err
	}

	out := make([]openaichat.ChunkChoice, 0, len(frame))
	for i := range frame {
		choice, err := s.stepOne(i, frame[i])
		if err != nil {
			return nil, err
		}
		out = append(out, choice)
	}
	return out, nil
}

// reconcile 校验并确立本帧的候选数。
//
// 首帧确立数量，之后任何增减都是契约违例。缩水时补齐会凭空造出候选，扩张时
// 新候选没有 role 历史——两种猜测都让客户端收到一份自相矛盾却语法合法的流。
func (s *candidateState) reconcile(n int) error {
	if s.seen == nil {
		if n == 0 {
			return canonical.Newf(canonical.ClassUpstreamUnavailable, "上游首帧没有任何候选")
		}
		s.seen = make([]candidate, n)
		return nil
	}
	if n != len(s.seen) {
		return canonical.Newf(canonical.ClassUpstreamUnavailable,
			"上游帧候选数从 %d 变为 %d", len(s.seen), n)
	}
	return nil
}

// stepOne 把一个候选的本帧增量编成 Chat chunk 的一个 choices 条目。
func (s *candidateState) stepOne(idx int, c nativewire.Choice) (openaichat.ChunkChoice, error) {
	cand := &s.seen[idx]
	// 已结束的候选仍会占着后续帧的数组位置——候选数恒定，先结束的那个不会
	// 从数组里消失。空占位放行，带负载则是契约违例：把内容接在一条客户端
	// 认为已经结束的消息后面，或重复发一次 finish，都让流自相矛盾。
	if cand.finished {
		if !isEmptyChoice(c) {
			return openaichat.ChunkChoice{}, canonical.Newf(canonical.ClassUpstreamUnavailable,
				"候选 %d 已结束却仍有后续增量", idx)
		}
		return openaichat.ChunkChoice{Index: idx}, nil
	}

	delta := openaichat.ChunkDelta{
		// 文本与 reasoning 已经是 Native 的增量输出，原样透传。
		Content:          c.Content,
		ReasoningContent: c.ReasoningContent,
	}
	if !cand.roleSent {
		cand.roleSent = true
		delta.Role = string(canonical.RoleAssistant)
	}

	tools, err := cand.stepTools(idx, c.ToolCalls)
	if err != nil {
		return openaichat.ChunkChoice{}, err
	}
	delta.ToolCalls = tools

	if c.FinishReason != "" {
		cand.finished = true
	}
	return openaichat.ChunkChoice{
		Index:        idx,
		Delta:        delta,
		FinishReason: c.FinishReason,
	}, nil
}

// isEmptyChoice 报告这个候选在本帧完全没有内容，只是占着数组位置。
func isEmptyChoice(c nativewire.Choice) bool {
	return c.Content == "" && c.ReasoningContent == "" && c.FinishReason == "" && len(c.ToolCalls) == 0
}

// stepTools 编本帧的工具调用增量。
//
// 参数片段**原样**续发：不 json.Valid、不闭合、不归一。官方明说 arguments 不
// 保证合法，流式还会把它切成未闭合片段——在这里校验会让一次本来能跨帧拼完整
// 的调用被当成上游故障丢掉。这也是不能借道 canonical.Accumulator 的原因，
// 它闭合时要求参数是合法 JSON。
func (c *candidate) stepTools(idx int, calls []nativewire.ToolCall) ([]openaichat.ToolCallDelta, error) {
	if len(calls) == 0 {
		return nil, nil
	}
	// 工具调用数只增不减：缩水意味着数组下标与此前帧对不上，续发的参数片段
	// 会被拼到另一次调用的身上。
	if len(calls) < len(c.tools) {
		return nil, canonical.Newf(canonical.ClassUpstreamUnavailable,
			"候选 %d 的工具调用数从 %d 缩为 %d", idx, len(c.tools), len(calls))
	}
	for len(c.tools) < len(calls) {
		c.tools = append(c.tools, false)
	}

	out := make([]openaichat.ToolCallDelta, 0, len(calls))
	for i, call := range calls {
		d := openaichat.ToolCallDelta{Index: i, Arguments: call.Arguments}
		if !c.tools[i] {
			c.tools[i] = true
			// ID 与 name 只发一次：每帧重发会让客户端把同一次调用当成多次。
			// 一律取上游原值，网关自造 ID 会让续轮时客户端回填的
			// tool_call_id 与上游对不上，整轮调用被静默丢弃。
			d.ID = call.ID
			d.Name = call.Name
		}
		out = append(out, d)
	}
	return out, nil
}

// allFinished 报告是否每个已确立的候选都发出过非空 finish_reason。
//
// 全部完成才允许收尾。Native 末帧只给部分候选 finish 时不替其余候选编造——
// 那等于替模型宣布它没说过的结束。
func (s *candidateState) allFinished() bool {
	if len(s.seen) == 0 {
		return false
	}
	for i := range s.seen {
		if !s.seen[i].finished {
			return false
		}
	}
	return true
}
