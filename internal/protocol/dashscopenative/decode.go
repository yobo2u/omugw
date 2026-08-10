package dashscopenative

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/yobo2u/omugw/internal/canonical"
)

// Decoded 是解码结果。
type Decoded struct {
	Request canonical.Request

	// InlineBytes 是本次请求内联（base64）多模态负载的总字节数。
	InlineBytes int64

	// webSearch 记录 parameters.enable_search，供 Capabilities 补报 CapWebSearch
	// （canonical.UsedCapabilities 不覆盖这项）。
	webSearch bool
}

// Decode 把 DashScope Native 请求解成 Canonical。
//
// 宽松解码：不拒绝未知字段。同源直通要透传没建模的字段，严格模式会把它们挡在
// 门外，正好违背直通的契约。只校验路由必需的 model。
func Decode(body []byte) (*Decoded, error) {
	var w Request
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassBadRequest,
			"无法解析 DashScope Native 请求")
	}
	if w.Model == "" {
		return nil, canonical.Newf(canonical.ClassBadRequest, "缺少 model")
	}

	out := &Decoded{}
	r := canonical.Request{Model: w.Model}

	system, msgs, inline, err := decodeMessages(w.Input.Messages)
	if err != nil {
		return nil, err
	}
	r.System = system
	r.Messages = msgs
	out.InlineBytes = inline

	if tools := decodeTools(w.Parameters.Tools); len(tools) > 0 {
		r.Tools = tools
	}
	if w.Parameters.EnableThinking != nil && *w.Parameters.EnableThinking {
		r.Reasoning = &canonical.Reasoning{}
	}
	if w.Parameters.EnableSearch != nil && *w.Parameters.EnableSearch {
		out.webSearch = true
	}

	// 刻意不调用 canonical.Request.Validate()：同源直通把原始字节原样转发，
	// 这里只是尽力而为的投影，用于路由与能力识别。让一个不完整的投影去否决
	// 本来合法的直通请求（例如 assistant 空 content 的工具调用续轮），恰恰
	// 违背直通的契约。真正非法的请求由上游拒绝。
	out.Request = r
	return out, nil
}

// Capabilities 报告这次请求用到的能力。
//
// 在 canonical.UsedCapabilities 之上补一项：enable_search 对应 CapWebSearch，
// 而 UsedCapabilities 不识别它。
func (d *Decoded) Capabilities() []canonical.Capability {
	caps := d.Request.UsedCapabilities()
	if d.webSearch {
		found := false
		for _, c := range caps {
			if c == canonical.CapWebSearch {
				found = true
				break
			}
		}
		if !found {
			caps = append(caps, canonical.CapWebSearch)
		}
	}
	return caps
}

// decodeMessages 解消息，顺带统计内联负载。system 角色提取到 System。
func decodeMessages(msgs []Message) ([]canonical.Part, []canonical.Message, int64, error) {
	var system []canonical.Part
	var out []canonical.Message
	var inline int64

	for _, m := range msgs {
		parts, n, err := decodeContent(m.Content)
		if err != nil {
			return nil, nil, 0, err
		}
		inline += n

		switch m.Role {
		case "system":
			system = append(system, parts...)
		case "user":
			out = append(out, canonical.Message{Role: canonical.RoleUser, Parts: parts})
		case "assistant":
			out = append(out, canonical.Message{Role: canonical.RoleAssistant, Parts: parts})
		default:
			// tool 等其余角色：直通路径不需要精细建模，记成普通消息即可。
			out = append(out, canonical.Message{Role: canonical.RoleTool, Parts: parts})
		}
	}
	return system, out, inline, nil
}

// decodeContent 处理 content 的两种形态：字符串与内容块数组。
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
	// 逐元素解：先收成 RawMessage 数组，再一块一块解。整段一次性解进
	// []ContentPart 的话，任何一块的类型不匹配（例如 video 传数组）都会让
	// 整个数组解析失败，连同兄弟块一起丢——图片的内联字节因此不计入，
	// 入口的内联上限就被绕过了。损失必须局限在解不出来的那一块。
	var rawParts []json.RawMessage
	if err := json.Unmarshal(raw, &rawParts); err != nil {
		// 既不是字符串也不是数组——宽松起见跳过，直通会把原始字节带走。
		return nil, 0, nil
	}
	var out []canonical.Part
	var inline int64
	for _, rp := range rawParts {
		var p ContentPart
		if err := json.Unmarshal(rp, &p); err != nil {
			continue // 只丢解不出来的这一块，不牵连兄弟块
		}
		part, n := decodePart(p)
		if part.Kind != "" {
			out = append(out, part)
		}
		inline += n
	}
	return out, inline, nil
}

// decodePart 解单个内容块，返回 Part 与内联字节数。识别不出的类型返回空 Kind。
//
// DashScope 内容块有两种形态：带 type 的 {"type":"image","image":...}，以及纯键式
// 的 {"image":"..."} / {"text":"..."}（官方多模态示例用后者，没有 type 字段）。
// 只认 type 会把纯键式块整个丢掉——图片/视频能力漏报，data URI 的字节数也不
// 计入，入口的内联上限就被绕过了。两种形态都得识别。
func decodePart(p ContentPart) (canonical.Part, int64) {
	if p.Type == "" {
		switch {
		case p.Image != "":
			return mediaPart(canonical.MediaImage, p.Image)
		case p.Audio != "":
			return mediaPart(canonical.MediaAudio, p.Audio)
		case len(p.Video) > 0:
			return videoPart(p.Video)
		case p.File != "":
			return mediaPart(canonical.MediaFile, p.File)
		case p.Text != "":
			return canonical.Text(p.Text), 0
		default:
			return canonical.Part{}, 0
		}
	}
	switch p.Type {
	case "text":
		return canonical.Text(p.Text), 0
	case "image":
		return mediaPart(canonical.MediaImage, p.Image)
	case "audio":
		return mediaPart(canonical.MediaAudio, p.Audio)
	case "video":
		return videoPart(p.Video)
	case "file":
		return mediaPart(canonical.MediaFile, p.File)
	default:
		return canonical.Part{}, 0
	}
}

// videoPart 解 video 的两种形态：单个字符串，或图像列表（视频帧）数组。
//
// 数组形态每一帧都可能是 data URI，字节数必须逐帧累加——只算第一帧会让
// 其余帧绕过内联上限。Part 本身只留第一帧，够用于能力识别。
func videoPart(raw json.RawMessage) (canonical.Part, int64) {
	if len(raw) == 0 {
		return canonical.Part{}, 0
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return mediaPart(canonical.MediaVideo, one)
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil || len(many) == 0 {
		return canonical.Part{}, 0
	}
	first, inline := mediaPart(canonical.MediaVideo, many[0])
	for _, ref := range many[1:] {
		_, n := mediaPart(canonical.MediaVideo, ref)
		inline += n
	}
	return first, inline
}

// mediaPart 把负载归一成 Media：data: URI 计内联字节，其余按 URL 透传。
func mediaPart(kind canonical.MediaKind, ref string) (canonical.Part, int64) {
	if ref == "" {
		return canonical.Part{}, 0
	}
	if data, mime, ok := decodeDataURI(ref); ok {
		return canonical.Part{
			Kind: canonical.PartMedia,
			Media: &canonical.Media{
				Kind:     kind,
				MIMEType: mime,
				Data:     data,
			},
		}, int64(len(data))
	}
	return canonical.Part{
		Kind:  canonical.PartMedia,
		Media: &canonical.Media{Kind: kind, URL: ref},
	}, 0
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

// decodeTools 解工具声明。DashScope 工具是 OpenAI 风格的 function 列表。
func decodeTools(raw json.RawMessage) []canonical.Tool {
	if len(raw) == 0 {
		return nil
	}
	var tools []struct {
		Type     string `json:"type"`
		Function struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil
	}
	var out []canonical.Tool
	for _, t := range tools {
		if t.Function.Name == "" {
			continue
		}
		out = append(out, canonical.Tool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  t.Function.Parameters,
		})
	}
	return out
}
