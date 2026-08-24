package dashscopenative

import (
	"encoding/json"
	"strings"

	"github.com/yobo2u/omugw/internal/canonical"
)

// Result 是 Native 成功响应解码后的投影。非流式裸 body 与流式 SSE data 负载
// 同构（output.choices[] + usage + request_id），共用这一结构。
//
// 两边各写一个解码器的话，「JSON null 与字符串 "null" 都算生成中」这类只在
// 一侧被想起来的归一规则会漂移，同一份字节在流式与非流式下解出两种结果。
type Result struct {
	RequestID string
	Choices   []Choice
	Usage     *canonical.Usage
}

// Choice 是一个候选。候选序号取数组下标；Native 没有官方 index 字段，
// 跨帧顺序与长度是否稳定未文档化，由流式侧按契约违例处理。
type Choice struct {
	Role             string
	Content          string
	ReasoningContent string
	ToolCalls        []ToolCall

	// FinishReason 是归一后的取值：""=生成中，stop/length/tool_calls 原样保留。
	FinishReason string

	// Logprobs 原样透传。重新编解码一轮会丢掉字段顺序与上游可能新增的键，
	// 而这份数据本网关只负责搬运，不解释。
	Logprobs json.RawMessage
}

// ToolCall 保留 Native wire 的工具参数字符串。流式帧里 Arguments 可能只是未闭合
// JSON 片段，因此不能提前塞进「完整且已闭合 JSON」契约的 canonical.ToolCall。
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// wireResult 是响应信封的线格式形态。
//
// 刻意不加 DisallowUnknownFields：SDK 包装层会多带 status_code / code / message，
// 上游也随时可能新增字段，严格模式会把一个本来解得出来的成功响应整个判成故障。
type wireResult struct {
	Output    wireOutput `json:"output"`
	Usage     *wireUsage `json:"usage"`
	RequestID string     `json:"request_id"`
}

type wireOutput struct {
	Choices []wireChoice `json:"choices"`
}

// wireChoice 的 FinishReason 用 string 收：JSON null 解进 string 是空操作，
// 正好落在「生成中」这一档，与字符串 "null" 归一到同一个结果。
type wireChoice struct {
	FinishReason string      `json:"finish_reason"`
	Message      wireMessage `json:"message"`
}

type wireMessage struct {
	Role string `json:"role"`

	// Content 有两种形态：文本模型给字符串，qwen-vl / qwen-audio 给数组。
	// 用 string 建模会让数组形态整条响应解不出来，模型回复凭空消失。
	Content          json.RawMessage `json:"content"`
	ReasoningContent string          `json:"reasoning_content"`
	ToolCalls        []wireToolCall  `json:"tool_calls"`

	// Logprobs 收原始字节：本网关不解释这份数据，重新编解码一轮会丢掉字段顺序
	// 与上游新增的键，客户端拿到的概率明细与上游给的对不上，却没有错误可看。
	Logprobs json.RawMessage `json:"logprobs"`
}

// wireToolCall 的 Arguments 是 JSON **字符串**而非对象。官方明说它不保证合法，
// 流式还会把它切成未闭合片段——在这一层校验合法性会让整帧解码失败，
// 一次本来能跨帧拼完整的工具调用被当成上游故障丢掉。
type wireToolCall struct {
	ID       string           `json:"id"`
	Function wireToolFunction `json:"function"`
}

type wireToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// wireUsage 用指针承载：usage 整体缺省与「各项都是 0」是两回事，
// 前者无从计费，后者是上游给出的权威零用量。
type wireUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`

	PromptTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`

	OutputTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

// DecodeResult 解码非流式裸 body。
func DecodeResult(body []byte) (*Result, error) { return decodeResult(body) }

// DecodeFrame 解码一条流式 SSE data 负载。与非流式裸 body 同构，共用解码器。
func DecodeFrame(data string) (*Result, error) { return decodeResult([]byte(data)) }

// decodeResult 是两个入口共用的解码实现。
//
// 解析失败按 upstream_unavailable 记：这段字节来自上游，畸形只能是上游或链路的
// 问题。记成 bad_request 会把客户端引去改一个本来合法的请求，真正的上游故障
// 却不进故障统计、也换不到另一个凭据或 Provider 重试。
func decodeResult(raw []byte) (*Result, error) {
	var w wireResult
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassUpstreamUnavailable,
			"无法解析 DashScope Native 响应")
	}

	res := &Result{RequestID: w.RequestID}
	for _, c := range w.Output.Choices {
		content, err := decodeContentText(c.Message.Content)
		if err != nil {
			return nil, err
		}
		res.Choices = append(res.Choices, Choice{
			Role:             c.Message.Role,
			Content:          content,
			ReasoningContent: c.Message.ReasoningContent,
			ToolCalls:        decodeToolCalls(c.Message.ToolCalls),
			FinishReason:     normalizeFinishReason(c.FinishReason),
			Logprobs:         c.Message.Logprobs,
		})
	}
	res.Usage = decodeUsage(w.Usage)
	return res, nil
}

// decodeContentText 取出回复正文。字符串形态直接用；数组形态（qwen-vl /
// qwen-audio）只把各元素的 text 值首尾相接。
//
// 不插分隔符：拼接处本来就是模型输出的连续文本，插进去的任何字符都会出现在
// 客户端看到的回复里。image_hw 之类的非 text 键不是回复内容，混进来同样是污染。
//
// 两种形态都解不出来时报错而不是退回空串：content 是已建模的已知字段，静默清空
// 等于把模型的整段回复丢掉再回一个 200——客户端收到空回复，网关的错误率与
// failover 却什么都没看见。缺省与 JSON null 不是畸形，是「本轮没有正文」的合法
// 形态（Function Calling 时官方就是这么给的），必须先放行。
func decodeContentText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", canonical.Wrapf(err, canonical.ClassUpstreamUnavailable,
			"无法解析 DashScope Native 响应的 message.content")
	}
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String(), nil
}

// decodeToolCalls 逐项搬运工具调用，参数字符串原样保留。
func decodeToolCalls(calls []wireToolCall) []ToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]ToolCall, 0, len(calls))
	for _, c := range calls {
		out = append(out, ToolCall{
			ID:        c.ID,
			Name:      c.Function.Name,
			Arguments: c.Function.Arguments,
		})
	}
	return out
}

// normalizeFinishReason 把「还在生成」的两种写法归一成空串。
//
// 官方非流式文档写 JSON null，流式示例却写字符串 "null"。漏掉任一种，下游都会
// 把它当成一个真实的结束原因：流被判定为已结束，后续增量静默丢弃，请求照样 200。
func normalizeFinishReason(s string) string {
	if s == "null" {
		return ""
	}
	return s
}

// decodeUsage 映射用量。上游给了就是权威值，可用于计费。
//
// usage 整体缺省时返回 nil 而不是零值 Usage：零值 Fidelity 非法，硬造一份
// 「0 token 且权威」的记录等于把一次未知用量的调用计成免费。
func decodeUsage(u *wireUsage) *canonical.Usage {
	if u == nil {
		return nil
	}
	return &canonical.Usage{
		Fidelity:     canonical.FidelityAuthoritative,
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
		// 缓存读单列：它与普通 input token 计价不同，并进 InputTokens 就再也
		// 分不出来，命中缓存的那部分会按未命中的价钱结算。
		CacheReadInputTokens: u.PromptTokensDetails.CachedTokens,
		ReasoningTokens:      u.OutputTokensDetails.ReasoningTokens,
	}
}
