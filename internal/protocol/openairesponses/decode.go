package openairesponses

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/yobo2u/omugw/internal/canonical"
)

// Decoded 是解码结果。
//
// 除了 Canonical 请求本身，还带出两样调用方必须知道、而 canonical.Request
// 里不该塞的东西：会话引用与内联负载大小。前者决定要不要查 ConversationStore，
// 后者用于入口处的大小上限检查（原则 2.6）。
type Decoded struct {
	Request canonical.Request

	// PreviousResponseID 非空表示客户端在依赖服务端保管的历史。
	PreviousResponseID string

	// WantsStore 表示客户端显式要求把本轮存下来（store: true）。
	WantsStore bool

	// InlineBytes 是本次请求内联（base64）多模态负载的总字节数。
	InlineBytes int64
}

// Decode 把 Responses 请求线格式解成 Canonical。
//
// 解不出来的字段一律报错，不静默丢弃：一个被悄悄忽略的 tool_choice 不会有任何
// 症状，只会让模型没调用它本该调用的工具，而这种问题在生产上极难定位。
func Decode(body []byte) (*Decoded, error) {
	var w Request
	dec := json.NewDecoder(strings.NewReader(string(body)))
	// 拒绝未知字段是刻意的严格：Responses 还在演进，一个我们没实现的新参数
	// 若被静默忽略，客户端会以为它生效了。报错至少让人知道网关落后了。
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassBadRequest,
			"无法解析 Responses 请求")
	}

	if w.Model == "" {
		return nil, canonical.Newf(canonical.ClassBadRequest, "缺少 model")
	}

	out := &Decoded{
		PreviousResponseID: w.PreviousResponseID,
		WantsStore:         w.Store != nil && *w.Store,
	}

	r := canonical.Request{
		Model:           w.Model,
		MaxOutputTokens: w.MaxOutputToks,
		Temperature:     w.Temperature,
		TopP:            w.TopP,
		Metadata:        w.Metadata,
	}
	if w.Stream != nil {
		r.Stream = *w.Stream
	}

	if w.Instructions != "" {
		// instructions 是顶层参数而非一条消息。归到 System 之后，各出站协议
		// 各自还原：Anthropic 放顶层 system，DashScope 用 system role。
		r.System = []canonical.Part{canonical.Text(w.Instructions)}
	}

	msgs, inline, err := decodeInput(w.Input)
	if err != nil {
		return nil, err
	}
	r.Messages = msgs
	out.InlineBytes = inline

	if r.Tools, err = decodeTools(w.Tools); err != nil {
		return nil, err
	}
	if r.ToolChoice, err = decodeToolChoice(w.ToolChoice); err != nil {
		return nil, err
	}
	if r.Reasoning, err = decodeReasoning(w.Reasoning); err != nil {
		return nil, err
	}
	if r.ResponseFormat, err = decodeFormat(w.Text); err != nil {
		return nil, err
	}

	// parallel_tool_calls 无 Canonical 对应字段——它是 OpenAI 特有的开关。
	// 存进 Extensions 供同源快通道原样回填；异构路径由降级矩阵处置。
	if w.ParallelToolCalls != nil {
		raw, _ := json.Marshal(map[string]bool{"parallel_tool_calls": *w.ParallelToolCalls})
		r.Extensions.Set(canonical.ExtOpenAI, raw)
	}

	if err := r.Validate(); err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassBadRequest, "请求不合法")
	}
	out.Request = r
	return out, nil
}

// decodeInput 处理 input 的两种形态：裸字符串与条目数组。
func decodeInput(raw json.RawMessage) ([]canonical.Message, int64, error) {
	if len(raw) == 0 {
		return nil, 0, canonical.Newf(canonical.ClassBadRequest, "缺少 input")
	}

	// 形态一：裸字符串，等价于单条用户消息。
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil, 0, canonical.Newf(canonical.ClassBadRequest, "input 为空字符串")
		}
		return []canonical.Message{canonical.UserText(s)}, 0, nil
	}

	// 形态二：条目数组。
	var items []InputItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, 0, canonical.Wrapf(err, canonical.ClassBadRequest,
			"input 既不是字符串也不是条目数组")
	}
	if len(items) == 0 {
		return nil, 0, canonical.Newf(canonical.ClassBadRequest, "input 数组为空")
	}

	var (
		msgs   []canonical.Message
		inline int64
	)
	for i, it := range items {
		m, n, err := decodeItem(it)
		if err != nil {
			return nil, 0, fmt.Errorf("input[%d]: %w", i, err)
		}
		inline += n
		msgs = append(msgs, m...)
	}
	return msgs, inline, nil
}

// decodeItem 把一个 input 条目转成零条或多条 Canonical 消息。
func decodeItem(it InputItem) ([]canonical.Message, int64, error) {
	// 没有 type 但有 role，是消息的简写形态。
	kind := it.Type
	if kind == "" && it.Role != "" {
		kind = itemMessage
	}

	switch kind {
	case itemMessage:
		role, err := decodeRole(it.Role)
		if err != nil {
			return nil, 0, err
		}
		parts, inline, err := decodeContent(it.Content)
		if err != nil {
			return nil, 0, err
		}
		return []canonical.Message{{Role: role, Parts: parts}}, inline, nil

	case itemFuncCall:
		if it.CallID == "" || it.Name == "" {
			return nil, 0, canonical.Newf(canonical.ClassBadRequest,
				"function_call 缺少 call_id 或 name")
		}
		args := json.RawMessage(it.Arguments)
		if it.Arguments != "" && !json.Valid(args) {
			// 半截 JSON 转给上游只会换来一个难以定位的解析错误。
			return nil, 0, canonical.Newf(canonical.ClassBadRequest,
				"function_call %q 的 arguments 不是合法 JSON", it.Name)
		}
		return []canonical.Message{{
			Role: canonical.RoleAssistant,
			Parts: []canonical.Part{{
				Kind:     canonical.PartToolCall,
				ToolCall: &canonical.ToolCall{ID: it.CallID, Name: it.Name, Arguments: args},
			}},
		}}, 0, nil

	case itemFuncOutput:
		if it.CallID == "" {
			return nil, 0, canonical.Newf(canonical.ClassBadRequest,
				"function_call_output 缺少 call_id")
		}
		return []canonical.Message{{
			Role: canonical.RoleTool,
			Parts: []canonical.Part{{
				Kind: canonical.PartToolResult,
				ToolResult: &canonical.ToolResult{
					CallID:  it.CallID,
					Content: []canonical.Part{canonical.Text(it.Output)},
				},
			}},
		}}, 0, nil

	case itemReasoning:
		// 客户端回传的推理条目。OpenAI 的推理内容是加密/删节的，网关不去解析
		// 它——原样丢弃比假装理解安全。同源快通道走字节透传，不经过这里。
		return nil, 0, nil

	default:
		return nil, 0, canonical.Newf(canonical.ClassBadRequest,
			"不支持的 input 条目类型 %q", kind)
	}
}

func decodeRole(role string) (canonical.Role, error) {
	switch role {
	case "user":
		return canonical.RoleUser, nil
	case "assistant":
		return canonical.RoleAssistant, nil
	case "system", "developer":
		// OpenAI 把 system 改名为 developer，两者归一到 RoleSystem，
		// 由各出站编码器按目标协议还原。
		return canonical.RoleSystem, nil
	default:
		return "", canonical.Newf(canonical.ClassBadRequest, "不支持的 role %q", role)
	}
}

// decodeContent 处理内容的两种形态：裸字符串与内容块数组。
func decodeContent(raw json.RawMessage) ([]canonical.Part, int64, error) {
	if len(raw) == 0 {
		return nil, 0, canonical.Newf(canonical.ClassBadRequest, "消息缺少 content")
	}

	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []canonical.Part{canonical.Text(s)}, 0, nil
	}

	var parts []ContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, 0, canonical.Wrapf(err, canonical.ClassBadRequest,
			"content 既不是字符串也不是内容块数组")
	}

	var (
		out    []canonical.Part
		inline int64
	)
	for i, p := range parts {
		cp, n, err := decodePart(p)
		if err != nil {
			return nil, 0, fmt.Errorf("content[%d]: %w", i, err)
		}
		inline += n
		out = append(out, cp)
	}
	return out, inline, nil
}

func decodePart(p ContentPart) (canonical.Part, int64, error) {
	switch p.Type {
	case partInputText, partOutputText:
		return canonical.Text(p.Text), 0, nil

	case partRefusal:
		return canonical.Refusal(p.Text), 0, nil

	case partInputImage:
		if p.ImageURL == "" {
			return canonical.Part{}, 0, canonical.Newf(canonical.ClassBadRequest,
				"input_image 缺少 image_url")
		}
		// data: URI 是内联负载，要计入大小上限；http(s) URL 直接透传，
		// 网关不代下载（原则 2.6），字节根本不经过这里。
		if data, mime, ok := decodeDataURI(p.ImageURL); ok {
			return canonical.ImageData(data, mime), int64(len(data)), nil
		}
		return canonical.ImageURL(p.ImageURL, ""), 0, nil

	case partInputAudio:
		if p.InputAudio == nil {
			return canonical.Part{}, 0, canonical.Newf(canonical.ClassBadRequest,
				"input_audio 缺少 input_audio 负载")
		}
		data, err := base64.StdEncoding.DecodeString(p.InputAudio.Data)
		if err != nil {
			return canonical.Part{}, 0, canonical.Wrapf(err, canonical.ClassBadRequest,
				"input_audio 的 base64 无法解码")
		}
		return canonical.Part{
			Kind: canonical.PartMedia,
			Media: &canonical.Media{
				Kind:     canonical.MediaAudio,
				MIMEType: "audio/" + p.InputAudio.Format,
				Data:     data,
			},
		}, int64(len(data)), nil

	case partInputFile:
		if p.FileID == "" {
			return canonical.Part{}, 0, canonical.Newf(canonical.ClassBadRequest,
				"input_file 缺少 file_id")
		}
		// 文件引用绑定 Provider。这里如实记下它属于谁，跨 Provider 时
		// 降级矩阵会据此拒绝——网关不代下载再上传。
		return canonical.Part{
			Kind: canonical.PartMedia,
			Media: &canonical.Media{
				Kind:    canonical.MediaFile,
				FileRef: &canonical.FileRef{Provider: "openai", ID: p.FileID},
			},
		}, 0, nil

	default:
		return canonical.Part{}, 0, canonical.Newf(canonical.ClassBadRequest,
			"不支持的内容块类型 %q", p.Type)
	}
}

// decodeDataURI 解析 data:<mime>;base64,<payload>。
func decodeDataURI(s string) (data []byte, mime string, ok bool) {
	if !strings.HasPrefix(s, "data:") {
		return nil, "", false
	}
	meta, payload, found := strings.Cut(s[len("data:"):], ",")
	if !found || !strings.HasSuffix(meta, ";base64") {
		return nil, "", false
	}
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, "", false
	}
	return raw, strings.TrimSuffix(meta, ";base64"), true
}

func decodeTools(tools []Tool) ([]canonical.Tool, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]canonical.Tool, 0, len(tools))
	for i, t := range tools {
		if t.Type != "function" {
			// 内建工具（web_search / computer_use / image_generation）不在
			// Canonical 里表达——它们的 schema 各家不兼容，勉强映射只会让模型
			// 收到一个读不懂的定义。降级矩阵按能力处置，这里直接拒绝。
			return nil, canonical.Newf(canonical.ClassUnsupported,
				"tools[%d]: 内建工具 %q 不做跨 Provider 映射", i, t.Type)
		}
		if t.Name == "" {
			return nil, canonical.Newf(canonical.ClassBadRequest, "tools[%d]: 缺少 name", i)
		}
		ct := canonical.Tool{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.Parameters,
		}
		if t.Strict != nil {
			ct.Strict = *t.Strict
		}
		out = append(out, ct)
	}
	return out, nil
}

// decodeToolChoice 处理 tool_choice 的两种形态：字符串枚举与具名对象。
func decodeToolChoice(raw json.RawMessage) (*canonical.ToolChoice, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto":
			return &canonical.ToolChoice{Mode: canonical.ToolChoiceAuto}, nil
		case "none":
			return &canonical.ToolChoice{Mode: canonical.ToolChoiceNone}, nil
		case "required":
			return &canonical.ToolChoice{Mode: canonical.ToolChoiceRequired}, nil
		default:
			return nil, canonical.Newf(canonical.ClassBadRequest,
				"不支持的 tool_choice %q", s)
		}
	}

	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassBadRequest, "无法解析 tool_choice")
	}
	if obj.Type != "function" || obj.Name == "" {
		return nil, canonical.Newf(canonical.ClassBadRequest,
			"具名 tool_choice 必须是 {\"type\":\"function\",\"name\":\"...\"}")
	}
	return &canonical.ToolChoice{Mode: canonical.ToolChoiceSpecific, Name: obj.Name}, nil
}

func decodeReasoning(r *Reasoning) (*canonical.Reasoning, error) {
	if r == nil {
		return nil, nil
	}
	out := &canonical.Reasoning{Visible: r.Summary != ""}

	switch r.Effort {
	case "":
	case "minimal":
		out.Effort = canonical.EffortMinimal
	case "low":
		out.Effort = canonical.EffortLow
	case "medium":
		out.Effort = canonical.EffortMedium
	case "high":
		out.Effort = canonical.EffortHigh
	default:
		return nil, canonical.Newf(canonical.ClassBadRequest,
			"不支持的 reasoning.effort %q", r.Effort)
	}
	return out, nil
}

func decodeFormat(t *TextOpts) (*canonical.ResponseFormat, error) {
	if t == nil || t.Format == nil {
		return nil, nil
	}
	f := t.Format

	out := &canonical.ResponseFormat{Name: f.Name, Schema: f.Schema}
	if f.Strict != nil {
		out.Strict = *f.Strict
	}

	switch f.Type {
	case "text":
		out.Kind = canonical.FormatText
	case "json_object":
		out.Kind = canonical.FormatJSONObject
	case "json_schema":
		out.Kind = canonical.FormatJSONSchema
		if len(f.Schema) == 0 {
			return nil, canonical.Newf(canonical.ClassBadRequest,
				"text.format 为 json_schema 但缺少 schema")
		}
	default:
		return nil, canonical.Newf(canonical.ClassBadRequest,
			"不支持的 text.format.type %q", f.Type)
	}
	return out, nil
}

// Capabilities 报告这次请求用到了哪些能力，供降级矩阵裁决。
//
// 在 canonical.Request.UsedCapabilities 之上补两项协议特有的判断：
//
//   - previous_response_id 是服务端会话的**读取**端，一出现就是在依赖保管的历史
//   - store 是**写入**端，且只有显式 true 才算。OpenAI 的默认值是 true，
//     把省略也算进来会让默认配置下几乎每个请求都被拒；而多数 SDK 不显式发送它，
//     说明调用方并不在意
func (d *Decoded) Capabilities() []canonical.Capability {
	caps := d.Request.UsedCapabilities()

	if d.PreviousResponseID != "" || d.WantsStore {
		for _, c := range caps {
			if c == canonical.CapStatefulConversation {
				return caps
			}
		}
		caps = append(caps, canonical.CapStatefulConversation)
	}
	return caps
}
