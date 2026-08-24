package dashscopenative

import (
	"encoding/json"

	"github.com/yobo2u/omugw/internal/canonical"
)

// Door 是 DashScope Native 的门（端点族）。门是部署事实，由配置声明，
// 不按模型名或本次请求是否含媒体推断。
type Door string

const (
	DoorTextGeneration       Door = "text-generation"
	DoorMultimodalGeneration Door = "multimodal-generation"
)

// Path 返回门对应的上游端点路径。未知门返回空串，由调用方报错。
func (d Door) Path() string {
	switch d {
	case DoorTextGeneration:
		return TextGenerationPath
	case DoorMultimodalGeneration:
		return MultimodalGenerationPath
	default:
		return ""
	}
}

// ChatSampling 承载 Canonical 不承载、而 Native 出站需要的 Chat 采样选项。
// 由 translator 从 openaichat 严格投影填入——协议包不互相 import，
// 桥接在 provider 层。
type ChatSampling struct {
	N               *int
	PresencePenalty *float64
	Logprobs        *bool
	TopLogprobs     *int
	// ParallelToolCalls 为 nil 表示客户端未提交，编码器按 OpenAI 默认注入 true。
	ParallelToolCalls *bool
	// MaxTokens / MaxCompletionTokens 保留客户端实际提交的字段，不同时制造两个限制。
	MaxTokens           *int
	MaxCompletionTokens *int
	// WebSearch 为真时编码器发 parameters.enable_search:true（只保留开关）。
	WebSearch bool
	// IncrementalOutput 仅流式 translator 置 true；false 时不发该字段。
	IncrementalOutput bool
}

// outRequest 是出站信封。刻意不复用入站 Request：入站结构为同源直通留了宽松的
// RawMessage 口子，拿它当出站结构会把「没编码的字段」变成合法的空值发出去。
type outRequest struct {
	Model      string         `json:"model"`
	Input      outInput       `json:"input"`
	Parameters map[string]any `json:"parameters"`
}

type outInput struct {
	Messages []outMessage `json:"messages"`
}

// outMessage 的 Content 是 any：文本门要字符串，多模态门要内容块数组，
// 两种形态必须由编码器按门显式决定，不能让结构体替它选。
type outMessage struct {
	Role       string    `json:"role"`
	Content    any       `json:"content"`
	ToolCalls  []outCall `json:"tool_calls,omitempty"`
	ToolCallID string    `json:"tool_call_id,omitempty"`
}

// outCall 是 OpenAI 风格的工具调用回放。Arguments 必须是 JSON **字符串**，
// 不是对象——发成对象上游读不出参数，工具续轮会静默丢掉整轮调用。
type outCall struct {
	ID       string      `json:"id"`
	Type     string      `json:"type"`
	Function outFunction `json:"function"`
}

type outFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// outTool 是工具声明。Parameters 用 RawMessage 原样透传：JSON Schema 重新
// 编解码一轮会丢掉字段顺序与上游可能识别的扩展关键字。
type outTool struct {
	Type     string          `json:"type"`
	Function outToolFunction `json:"function"`
}

type outToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// outToolChoice 是点名某个工具的选择策略。用结构体而非 map：map 的键序由
// 运行时决定，出站字节因此不稳定，golden 与录制对账会失效。
type outToolChoice struct {
	Type     string            `json:"type"`
	Function outToolChoiceName `json:"function"`
}

type outToolChoiceName struct {
	Name string `json:"name"`
}

// outFormat 是结构化输出约束。Strict 不带 omitempty：显式 false 与未声明
// 在 schema 校验上是两回事，抹掉它等于把严格校验悄悄关掉。
type outFormat struct {
	Type       string         `json:"type"`
	JSONSchema *outJSONSchema `json:"json_schema,omitempty"`
}

type outJSONSchema struct {
	Name   string          `json:"name,omitempty"`
	Schema json.RawMessage `json:"schema,omitempty"`
	Strict bool            `json:"strict"`
}

// EncodeRequest 把 Chat 请求编码成 DashScope Native 出站信封。
// 出站信封固定为 {model, input:{messages}, parameters:{result_format:message,...}}。
//
// 全程不读 Extensions：那是同源快通道的原样回填通道，异构路径从里面猜语义
// 恰恰是降级矩阵要防的事。Canonical 之外的 Chat 采样选项一律由 extra 显式传入。
func EncodeRequest(canon *canonical.Request, extra ChatSampling, door Door, upstreamModel string) ([]byte, error) {
	if canon == nil {
		return nil, canonical.Newf(canonical.ClassInternal, "缺少 Canonical 请求")
	}
	if door.Path() == "" {
		return nil, canonical.Newf(canonical.ClassInternal, "未知的 DashScope Native 门 %q", string(door))
	}
	if upstreamModel == "" {
		return nil, canonical.Newf(canonical.ClassInternal, "缺少上游模型名")
	}
	// 先过 Canonical 自校验：Part 的判别字段与负载不一致时，后面每一处
	// 按 Kind 取负载的地方都会读到 nil，静默编出一条缺内容的消息。
	//
	// 失败按 internal 记：入站解码器已经校验过客户端提交的内容，能走到这里
	// 的不自洽 Canonical 只可能是网关自己造出来的。报成 400 会让客户端去改
	// 一个本来就对的请求，真正的 bug 却没人查。
	if err := canon.Validate(); err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassInternal, "请求无法编码为 DashScope Native")
	}

	msgs, err := encodeMessages(canon, door)
	if err != nil {
		return nil, err
	}
	// 一条消息都没有时 input.messages 会编成 null，上游拿不到任何对话内容，
	// 却仍是一个语法合法的请求。
	//
	// 这是客户端可达的形态，所以报 400 而不是 500：Chat 侧
	// messages:[{"role":"system","content":""}] 数组非空、过得了解码器的空数组
	// 检查，但空 content 解出零个 Part、system 又不进 Messages，落地就是双空的
	// Canonical——Validate 只查自洽性，不查非空。记成 500 会把客户端的输入问题
	// 混进网关故障告警里。
	if len(msgs) == 0 {
		return nil, canonical.Newf(canonical.ClassBadRequest, "请求没有任何可发送的消息内容")
	}
	params, err := encodeParameters(canon, extra)
	if err != nil {
		return nil, err
	}
	out := outRequest{
		Model:      upstreamModel,
		Input:      outInput{Messages: msgs},
		Parameters: params,
	}
	body, err := json.Marshal(out)
	if err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassInternal, "DashScope Native 请求序列化失败")
	}
	return body, nil
}

// encodeParameters 逐字段映射采样与行为参数。
//
// 用 map 而不是结构体：Native 的参数省略与显式零值语义不同（temperature=0 是
// 合法取值），omitempty 会把显式零值一起抹掉，静默变成上游默认值。
func encodeParameters(canon *canonical.Request, extra ChatSampling) (map[string]any, error) {
	p := map[string]any{"result_format": "message"}

	// 指针字段逐个判 nil 而不是靠 omitempty：显式零值是合法取值
	// （temperature=0 要求确定性输出），被抹掉请求照样成功，模型却换回了
	// 默认温度——输出变随机，没有任何错误可看。
	putIf(p, "temperature", canon.Temperature)
	putIf(p, "top_p", canon.TopP)
	putIf(p, "seed", canon.Seed)
	putIf(p, "n", extra.N)
	putIf(p, "presence_penalty", extra.PresencePenalty)
	putIf(p, "logprobs", extra.Logprobs)
	putIf(p, "top_logprobs", extra.TopLogprobs)

	// 缺省注入 true：Native 默认 false 而 OpenAI 默认 true，不注入会让并行调用
	// 静默退化成串行——请求照样 200，丢掉的并行没人看得见。显式 false 原样保留。
	if extra.ParallelToolCalls != nil {
		p["parallel_tool_calls"] = *extra.ParallelToolCalls
	} else {
		p["parallel_tool_calls"] = true
	}
	// 二者择一：两个上限一起发等于给上游两个互相冲突的限制，取哪个是上游的
	// 实现细节，输出长度因此不可预期。Canonical 的 MaxOutputTokens 是同一个
	// 客户端字段的投影，不得再据它制造第三个限制。
	switch {
	case extra.MaxCompletionTokens != nil:
		p["max_completion_tokens"] = *extra.MaxCompletionTokens
	case extra.MaxTokens != nil:
		p["max_tokens"] = *extra.MaxTokens
	}
	if err := encodeReasoning(p, canon.Reasoning); err != nil {
		return nil, err
	}
	if tools := encodeTools(canon.Tools); len(tools) > 0 {
		p["tools"] = tools
	}
	choice, err := encodeToolChoice(canon.ToolChoice)
	if err != nil {
		return nil, err
	}
	if choice != nil {
		p["tool_choice"] = choice
	}
	format, err := encodeResponseFormat(canon.ResponseFormat)
	if err != nil {
		return nil, err
	}
	if format != nil {
		p["response_format"] = format
	}
	if stop := encodeStop(canon.StopSequences); stop != nil {
		p["stop"] = stop
	}
	// 只保留搜索开关：位置、上下文大小等选项在 Native 没有落点，
	// 这份损失登记在降级矩阵里，不在编码器里假装还在。
	if extra.WebSearch {
		p["enable_search"] = true
	}
	// 流式必须显式开增量：不开的话上游每帧回全量文本，按 delta 逐帧拼接
	// 会把内容重复放大成 O(n²)。非流式一律不发。
	if extra.IncrementalOutput {
		p["incremental_output"] = true
	}
	return p, nil
}

// encodeStop 编停止词。单元素发字符串、多元素发数组，是 Native 的线格式事实。
func encodeStop(stop []string) any {
	switch len(stop) {
	case 0:
		return nil
	case 1:
		return stop[0]
	default:
		return stop
	}
}

// encodeResponseFormat 编结构化输出约束。text 是默认形态，显式发出去没有意义。
// 未知形态是内部错：解码器只放行三种取值，第四种只能来自网关自己的 bug，
// 静默省略等于把客户端要求的 schema 约束凭空撤掉，请求照样 200。
func encodeResponseFormat(f *canonical.ResponseFormat) (any, error) {
	if f == nil {
		return nil, nil
	}
	switch f.Kind {
	case canonical.FormatText:
		return nil, nil
	case canonical.FormatJSONObject:
		return outFormat{Type: string(canonical.FormatJSONObject)}, nil
	case canonical.FormatJSONSchema:
		return outFormat{
			Type: string(canonical.FormatJSONSchema),
			JSONSchema: &outJSONSchema{
				Name:   f.Name,
				Schema: f.Schema,
				Strict: f.Strict,
			},
		}, nil
	default:
		return nil, canonical.Newf(canonical.ClassInternal,
			"未知的 response_format 形态 %q", string(f.Kind))
	}
}

// encodeTools 编工具声明为 OpenAI 风格的 function 列表。
func encodeTools(tools []canonical.Tool) []outTool {
	out := make([]outTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, outTool{
			Type: "function",
			Function: outToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}
	return out
}

// encodeToolChoice 编工具选择策略。specific 必须带上工具名——退化成裸
// "required" 会让模型自由挑一个工具，客户端点名的那个未必被调用。
// 未知模式是内部错：原样发出去上游拒整轮，是最好的下场；真正危险的是
// 它恰好撞上某个上游认识的词，工具约束就变成了另一回事。
func encodeToolChoice(c *canonical.ToolChoice) (any, error) {
	if c == nil || c.Mode == "" {
		return nil, nil
	}
	switch c.Mode {
	case canonical.ToolChoiceSpecific:
		return outToolChoice{Type: "function", Function: outToolChoiceName{Name: c.Name}}, nil
	case canonical.ToolChoiceAuto, canonical.ToolChoiceNone, canonical.ToolChoiceRequired:
		return string(c.Mode), nil
	default:
		return nil, canonical.Newf(canonical.ClassInternal,
			"未知的 tool_choice 模式 %q", string(c.Mode))
	}
}

// putIf 只在客户端提交过该字段时写入，保住「显式零值」与「未设置」的区别。
func putIf[T any](p map[string]any, key string, v *T) {
	if v != nil {
		p[key] = *v
	}
}

// encodeReasoning 写推理档位。两个字段互斥：none 是「显式关思考」的开关，
// 当成 effort 档位发出去上游会拒绝整个请求；两个都发则自相矛盾。
// thinking_budget 一律不发——Canonical 的 MaxTokens 与它口径不同，换算是猜的。
// 未知档位是内部错：解码器只放行五个档位，第六个只能来自网关自己的 bug。
func encodeReasoning(p map[string]any, r *canonical.Reasoning) error {
	if r == nil || r.Effort == "" {
		return nil
	}
	switch r.Effort {
	case canonical.EffortNone:
		p["enable_thinking"] = false
	case canonical.EffortMinimal, canonical.EffortLow, canonical.EffortMedium, canonical.EffortHigh:
		p["reasoning_effort"] = string(r.Effort)
	default:
		return canonical.Newf(canonical.ClassInternal,
			"未知的 reasoning_effort 档位 %q", string(r.Effort))
	}
	return nil
}
