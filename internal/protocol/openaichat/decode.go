package openaichat

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/yobo2u/omugw/internal/canonical"
)

// Decoded 是解码结果。
//
// 与 Responses 不同，Chat 是无状态协议——没有 previous_response_id / store，
// 所以只带出 Canonical 请求与内联负载大小。
type Decoded struct {
	Request canonical.Request

	// InlineBytes 是本次请求内联（base64）多模态负载的总字节数。
	InlineBytes int64

	// webSearch 与 parallelToolCalls 是 OpenAI 特有开关，Canonical 没有对应字段。
	// 能力识别必须由解码器完成：异构出站路径读不得 Extensions——
	// 那是同源快通道专属的原样回填通道。
	webSearch         bool
	parallelToolCalls bool
}

// Decode 把 Chat Completions 请求线格式解成 Canonical。
//
// 与 Responses 解码同样的严格哲学：解不出来的字段一律报错，不静默丢弃。
func Decode(body []byte) (*Decoded, error) {
	var w Request
	dec := json.NewDecoder(strings.NewReader(string(body)))
	// 拒绝未知字段是刻意的严格：一个我们没建模的新参数若被静默忽略，
	// 客户端会以为它生效了。报错至少让人知道网关落后了。
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassBadRequest,
			"无法解析 Chat Completions 请求")
	}

	if w.Model == "" {
		return nil, canonical.Newf(canonical.ClassBadRequest, "缺少 model")
	}

	out := &Decoded{}

	r := canonical.Request{
		Model:           w.Model,
		MaxOutputTokens: maxOutputTokens(w.MaxTokens, w.MaxCompletionTokens),
		Temperature:     w.Temperature,
		TopP:            w.TopP,
		Seed:            w.Seed,
		Metadata:        w.Metadata,
	}
	if w.Stream != nil {
		r.Stream = *w.Stream
	}

	if stop, err := decodeStop(w.Stop); err != nil {
		return nil, err
	} else {
		r.StopSequences = stop
	}

	system, msgs, inline, err := decodeMessages(w.Messages)
	if err != nil {
		return nil, err
	}
	r.System = system
	r.Messages = msgs
	out.InlineBytes = inline

	if r.Tools, err = decodeTools(w.Tools); err != nil {
		return nil, err
	}
	if r.ToolChoice, err = decodeToolChoice(w.ToolChoice); err != nil {
		return nil, err
	}
	if r.ResponseFormat, err = decodeFormat(w.ResponseFormat); err != nil {
		return nil, err
	}
	if r.Reasoning, err = decodeReasoning(w.ReasoningEffort); err != nil {
		return nil, err
	}
	if r.Modalities, err = decodeModalities(w.Modalities); err != nil {
		return nil, err
	}

	// parallel_tool_calls 无 Canonical 对应字段——它是 OpenAI 特有的开关。
	// 存进 Extensions 供同源快通道原样回填；能力识别则不依赖它——
	// 异构出站读不得 Extensions，由解码器直接报告。
	if w.ParallelToolCalls != nil {
		raw, _ := json.Marshal(map[string]bool{"parallel_tool_calls": *w.ParallelToolCalls})
		r.Extensions.Set(canonical.ExtOpenAI, raw)
		// 只有显式 true 才报告：缺省与显式 false 都不构成并行调用请求。
		out.parallelToolCalls = *w.ParallelToolCalls
	}

	// web_search_options：出现且非 null（哪怕是 {}）即开启搜索，缺省或 null 为关闭。
	// 严格解码先行：这个对象在出站前被整体删除，未建模的子字段必须 400，
	// 既不许静默吞掉，也不许默认开启搜索。
	if len(w.WebSearchOptions) > 0 && string(w.WebSearchOptions) != "null" {
		if _, err := decodeWebSearchOptions(w.WebSearchOptions); err != nil {
			return nil, err
		}
		out.webSearch = true
	}

	if err := r.Validate(); err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassBadRequest, "请求不合法")
	}
	out.Request = r
	return out, nil
}

// Capabilities 报告这次请求用到了哪些能力，供降级矩阵裁决。
//
// Chat 无状态，没有 Responses 那样的会话读写端，直接复用 Canonical 的推导，
// 再补上两项 OpenAI 特有开关：它们没有 Canonical 字段，异构出站又读不得
// Extensions，能力识别只能在解码阶段完成。结果统一按 AllCapabilities 的顺序
// 输出——golden 文件依赖这一点稳定。
func (d *Decoded) Capabilities() []canonical.Capability {
	caps := d.Request.UsedCapabilities()
	if !d.webSearch && !d.parallelToolCalls {
		return caps
	}
	seen := make(map[canonical.Capability]bool, len(caps)+2)
	for _, c := range caps {
		seen[c] = true
	}
	if d.webSearch {
		seen[canonical.CapWebSearch] = true
	}
	if d.parallelToolCalls {
		seen[canonical.CapParallelToolCalls] = true
	}
	out := make([]canonical.Capability, 0, len(seen))
	for _, c := range canonical.AllCapabilities() {
		if seen[c] {
			out = append(out, c)
		}
	}
	return out
}

// maxOutputTokens 在新旧两个字段里取非空者；两者都设时以新名为准。
func maxOutputTokens(legacy, modern *int) *int {
	if modern != nil {
		return modern
	}
	return legacy
}

// decodeStop 处理 stop 的两种形态：裸字符串与字符串数组。
func decodeStop(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return []string{one}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassBadRequest,
			"stop 既不是字符串也不是字符串数组")
	}
	return many, nil
}

// decodeMessages 把消息数组解成 Canonical，顺带分离出 system 指令与内联负载。
//
// system / developer 角色的内容提取到 System（各出站协议各自还原），其余角色
// 进 Messages。
func decodeMessages(raw json.RawMessage) ([]canonical.Part, []canonical.Message, int64, error) {
	if len(raw) == 0 {
		return nil, nil, 0, canonical.Newf(canonical.ClassBadRequest, "缺少 messages")
	}
	var msgs []Message
	if err := json.Unmarshal(raw, &msgs); err != nil {
		return nil, nil, 0, canonical.Wrapf(err, canonical.ClassBadRequest,
			"messages 不是数组")
	}
	if len(msgs) == 0 {
		return nil, nil, 0, canonical.Newf(canonical.ClassBadRequest, "messages 为空")
	}

	var system []canonical.Part
	var out []canonical.Message
	var inline int64

	for i, m := range msgs {
		parts, n, err := decodeContent(m.Content)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("messages[%d]: %w", i, err)
		}
		inline += n

		switch m.Role {
		case "system", "developer":
			system = append(system, parts...)

		case "user":
			out = append(out, canonical.Message{Role: canonical.RoleUser, Parts: parts, Name: m.Name})

		case "assistant":
			// assistant 的 tool_calls 是模型发起的调用，归一成 PartToolCall。
			for _, tc := range m.ToolCalls {
				parts = append(parts, canonical.Part{
					Kind:     canonical.PartToolCall,
					ToolCall: &canonical.ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: toolArgs(tc.Function.Arguments)},
				})
			}
			if m.Refusal != "" {
				parts = append(parts, canonical.Refusal(m.Refusal))
			}
			out = append(out, canonical.Message{Role: canonical.RoleAssistant, Parts: parts, Name: m.Name})

		case "tool":
			if m.ToolCallID == "" {
				return nil, nil, 0, fmt.Errorf("messages[%d]: tool 消息缺少 tool_call_id", i)
			}
			out = append(out, canonical.Message{
				Role: canonical.RoleTool,
				Parts: []canonical.Part{{
					Kind:       canonical.PartToolResult,
					ToolResult: &canonical.ToolResult{CallID: m.ToolCallID, Content: parts},
				}},
			})

		default:
			return nil, nil, 0, canonical.Newf(canonical.ClassBadRequest,
				"messages[%d] 的角色 %q 无法识别", i, m.Role)
		}
	}
	return system, out, inline, nil
}

// toolArgs 把工具调用的 JSON 字符串参数转成 RawMessage；空串视为无参数。
func toolArgs(s string) json.RawMessage {
	if s == "" {
		return nil
	}
	return json.RawMessage(s)
}

// decodeContent 处理 content 的三种形态：null、字符串、内容块数组。
func decodeContent(raw json.RawMessage) ([]canonical.Part, int64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, 0, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil, 0, nil
		}
		return []canonical.Part{canonical.Text(s)}, 0, nil
	}
	var parts []ContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, 0, canonical.Wrapf(err, canonical.ClassBadRequest,
			"content 既不是字符串也不是内容块数组")
	}
	var out []canonical.Part
	var inline int64
	for _, p := range parts {
		part, n, err := decodePart(p)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, part)
		inline += n
	}
	return out, inline, nil
}

// decodePart 解码单个内容块，返回对应 Part 与内联字节数。
func decodePart(p ContentPart) (canonical.Part, int64, error) {
	switch p.Type {
	case "text":
		return canonical.Text(p.Text), 0, nil

	case "image_url":
		if p.ImageURL == nil || p.ImageURL.URL == "" {
			return canonical.Part{}, 0, canonical.Newf(canonical.ClassBadRequest,
				"image_url 缺少 url")
		}
		// data: URI 是内联负载，要计入大小上限；http(s) URL 直接透传，
		// 网关不代下载（原则 2.6），字节根本不经过这里。
		if data, mime, ok := decodeDataURI(p.ImageURL.URL); ok {
			return canonical.ImageData(data, mime), int64(len(data)), nil
		}
		return canonical.ImageURL(p.ImageURL.URL, ""), 0, nil

	case "input_audio":
		if p.InputAudio == nil {
			return canonical.Part{}, 0, canonical.Newf(canonical.ClassBadRequest,
				"input_audio 缺少负载")
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

	case "file":
		if p.File == nil {
			return canonical.Part{}, 0, canonical.Newf(canonical.ClassBadRequest,
				"file 缺少负载")
		}
		if p.File.FileID != "" {
			// 文件引用绑定 Provider。跨 Provider 时降级矩阵会据此拒绝。
			return canonical.Part{
				Kind: canonical.PartMedia,
				Media: &canonical.Media{
					Kind:    canonical.MediaFile,
					FileRef: &canonical.FileRef{Provider: "openai", ID: p.File.FileID},
				},
			}, 0, nil
		}
		if p.File.FileData != "" {
			data, mime, ok := decodeDataURI(p.File.FileData)
			if !ok {
				return canonical.Part{}, 0, canonical.Newf(canonical.ClassBadRequest,
					"file.file_data 不是合法的 data URI")
			}
			return canonical.Part{
				Kind: canonical.PartMedia,
				Media: &canonical.Media{
					Kind:     canonical.MediaFile,
					MIMEType: mime,
					Data:     data,
				},
			}, int64(len(data)), nil
		}
		return canonical.Part{}, 0, canonical.Newf(canonical.ClassBadRequest,
			"file 既无 file_id 也无 file_data")

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

// decodeTools 解码工具声明。只认 function 类型。
func decodeTools(raw json.RawMessage) ([]canonical.Tool, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var tools []Tool
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassBadRequest, "tools 不是数组")
	}
	out := make([]canonical.Tool, 0, len(tools))
	for i, t := range tools {
		if t.Type != "function" {
			return nil, canonical.Newf(canonical.ClassBadRequest,
				"tools[%d] 的类型 %q 不支持", i, t.Type)
		}
		tool := canonical.Tool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  t.Function.Parameters,
		}
		if t.Function.Strict != nil {
			tool.Strict = *t.Function.Strict
		}
		if tool.Name == "" {
			return nil, canonical.Newf(canonical.ClassBadRequest, "tools[%d] 缺少函数名", i)
		}
		out = append(out, tool)
	}
	return out, nil
}

// decodeToolChoice 解码工具选择策略：字符串或 {type:"function",function:{name}}。
func decodeToolChoice(raw json.RawMessage) (*canonical.ToolChoice, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto", "none", "required":
			return &canonical.ToolChoice{Mode: canonical.ToolChoiceMode(s)}, nil
		default:
			return nil, canonical.Newf(canonical.ClassBadRequest,
				"tool_choice %q 无法识别", s)
		}
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassBadRequest,
			"tool_choice 形态无法识别")
	}
	if obj.Type != "function" || obj.Function.Name == "" {
		return nil, canonical.Newf(canonical.ClassBadRequest,
			"tool_choice 对象缺少 function.name")
	}
	return &canonical.ToolChoice{Mode: canonical.ToolChoiceSpecific, Name: obj.Function.Name}, nil
}

// decodeFormat 解码 response_format。
func decodeFormat(raw json.RawMessage) (*canonical.ResponseFormat, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var f ResponseFormat
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassBadRequest, "response_format 无法解析")
	}
	switch f.Type {
	case "text":
		return &canonical.ResponseFormat{Kind: canonical.FormatText}, nil
	case "json_object":
		return &canonical.ResponseFormat{Kind: canonical.FormatJSONObject}, nil
	case "json_schema":
		if f.JSONSchema == nil || len(f.JSONSchema.Schema) == 0 {
			return nil, canonical.Newf(canonical.ClassBadRequest,
				"response_format 为 json_schema 但缺少 schema")
		}
		out := &canonical.ResponseFormat{
			Kind:   canonical.FormatJSONSchema,
			Name:   f.JSONSchema.Name,
			Schema: f.JSONSchema.Schema,
		}
		if f.JSONSchema.Strict != nil {
			out.Strict = *f.JSONSchema.Strict
		}
		return out, nil
	default:
		return nil, canonical.Newf(canonical.ClassBadRequest,
			"不支持的 response_format.type %q", f.Type)
	}
}

// decodeReasoning 把 reasoning_effort 档位解成 Canonical Reasoning。
func decodeReasoning(effort *string) (*canonical.Reasoning, error) {
	if effort == nil {
		return nil, nil
	}
	switch canonical.ReasoningEffort(*effort) {
	case canonical.EffortLow, canonical.EffortMedium, canonical.EffortHigh:
		return &canonical.Reasoning{Effort: canonical.ReasoningEffort(*effort)}, nil
	default:
		return nil, canonical.Newf(canonical.ClassBadRequest,
			"不支持的 reasoning_effort %q", *effort)
	}
}

// decodeWebSearchOptions 严格解码搜索选项。
//
// 另起一个 DisallowUnknownFields 的解码器：外层的严格模式只管顶层字段，
// RawMessage 子树绕过了它——而这个对象在出站前会被整体删除，
// 未知子字段必须在入站就拒掉，不能等它悄悄消失。
func decodeWebSearchOptions(raw json.RawMessage) (*WebSearchOptions, error) {
	var wso WebSearchOptions
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wso); err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassBadRequest,
			"web_search_options 无法解析")
	}
	if wso.SearchContextSize != "" {
		switch wso.SearchContextSize {
		case "low", "medium", "high":
		default:
			return nil, canonical.Newf(canonical.ClassBadRequest,
				"不支持的 search_context_size %q", wso.SearchContextSize)
		}
	}
	if loc := wso.UserLocation; loc != nil {
		if loc.Type != "approximate" {
			return nil, canonical.Newf(canonical.ClassBadRequest,
				"不支持的 user_location.type %q", loc.Type)
		}
		if loc.Approximate == nil {
			return nil, canonical.Newf(canonical.ClassBadRequest,
				"user_location 缺少 approximate")
		}
	}
	return &wso, nil
}

// decodeModalities 解码输出模态列表。
func decodeModalities(mods []string) ([]canonical.Modality, error) {
	if len(mods) == 0 {
		return nil, nil
	}
	out := make([]canonical.Modality, 0, len(mods))
	for _, m := range mods {
		switch canonical.Modality(m) {
		case canonical.ModalityText, canonical.ModalityAudio:
			out = append(out, canonical.Modality(m))
		default:
			return nil, canonical.Newf(canonical.ClassBadRequest,
				"不支持的模态 %q", m)
		}
	}
	return out, nil
}
