package dashscopenative

import (
	"encoding/base64"
	"strings"

	"github.com/yobo2u/omugw/internal/canonical"
)

// encodeMessages 把 System 与 Messages 编成 input.messages。
// System 提取为首条 system 消息——Native 没有顶层 system 字段，丢了它模型就换了人格。
func encodeMessages(canon *canonical.Request, door Door) ([]outMessage, error) {
	var out []outMessage
	if len(canon.System) > 0 {
		content, err := encodeTextContent(canon.System)
		if err != nil {
			return nil, err
		}
		out = append(out, outMessage{Role: string(canonical.RoleSystem), Content: content})
	}
	for _, m := range canon.Messages {
		encoded, err := encodeMessage(m, door)
		if err != nil {
			return nil, err
		}
		out = append(out, encoded...)
	}
	return out, nil
}

// encodeMessage 编一条消息。tool 角色可能携带多个结果块，因此返回切片。
//
// 只有 user 的 content 按门取形态：Native 线格式里 system 与 assistant 的
// content 是字符串，给它们发块数组，上游读不出这两条消息却照常作答——
// 请求 200，系统人格与助手历史双双失踪，响应里看不出任何痕迹。
func encodeMessage(m canonical.Message, door Door) ([]outMessage, error) {
	switch m.Role {
	case canonical.RoleSystem:
		content, err := encodeTextContent(m.Parts)
		if err != nil {
			return nil, err
		}
		return []outMessage{{Role: string(m.Role), Content: content}}, nil
	case canonical.RoleUser:
		content, err := encodeContent(m.Parts, door)
		if err != nil {
			return nil, err
		}
		return []outMessage{{Role: string(m.Role), Content: content}}, nil
	case canonical.RoleAssistant:
		return encodeAssistant(m)
	case canonical.RoleTool:
		return encodeToolResults(m)
	default:
		return nil, canonical.Newf(canonical.ClassBadRequest, "无法编码的角色 %q", string(m.Role))
	}
}

// encodeAssistant 编助手消息：文本与 tool_calls 留在同一条消息里。
// 拆成两条会让上游读不出这一轮的因果，续轮时模型不知道那次调用是谁发起的。
// content 一律按文本门形态编，两扇门都是——助手历史里的媒体块在 Native
// 没有落点，宁可显式报错也不发一条上游读不出的 content。
func encodeAssistant(m canonical.Message) ([]outMessage, error) {
	var content []canonical.Part
	var calls []outCall
	for _, p := range m.Parts {
		if p.Kind != canonical.PartToolCall {
			content = append(content, p)
			continue
		}
		calls = append(calls, outCall{
			ID:       p.ToolCall.ID,
			Type:     "function",
			Function: outFunction{Name: p.ToolCall.Name, Arguments: encodeArguments(p.ToolCall.Arguments)},
		})
	}
	encoded, err := encodeTextContent(content)
	if err != nil {
		return nil, err
	}
	return []outMessage{{Role: string(canonical.RoleAssistant), Content: encoded, ToolCalls: calls}}, nil
}

// encodeToolResults 编工具结果，逐块保留 tool_call_id。
// 丢了关联，上游无从判断这条结果回应的是哪次调用——多工具并行时会张冠李戴，
// 而请求本身仍然成功。
func encodeToolResults(m canonical.Message) ([]outMessage, error) {
	out := make([]outMessage, 0, len(m.Parts))
	for _, p := range m.Parts {
		if p.ToolResult.CallID == "" {
			return nil, canonical.Newf(canonical.ClassBadRequest, "tool_result 缺少 call_id")
		}
		// 工具结果一律编成字符串：Native 的 tool 消息 content 是字符串，
		// 结果里的媒体块没有落点，宁可显式报错也不静默丢掉。
		content, err := encodeTextContent(p.ToolResult.Content)
		if err != nil {
			return nil, err
		}
		out = append(out, outMessage{
			Role:       string(canonical.RoleTool),
			Content:    content,
			ToolCallID: p.ToolResult.CallID,
		})
	}
	return out, nil
}

// encodeArguments 把完整 JSON 参数转成 OpenAI 风格的字符串。
// 无参数时发 "{}"：空串会让上游把这次调用当成参数缺失而拒绝整轮。
func encodeArguments(raw []byte) string {
	if len(raw) == 0 {
		return "{}"
	}
	return string(raw)
}

// encodeContent 按门决定 content 形态：文本门是单个字符串，多模态门是内容块数组。
func encodeContent(parts []canonical.Part, door Door) (any, error) {
	if door == DoorTextGeneration {
		return encodeTextContent(parts)
	}
	return encodeBlockContent(parts)
}

// encodeTextContent 把文本块连成单个字符串。
//
// 错误信息不提门：多模态门的 system / assistant / tool 三个槽位同样走这里，
// 说成「文本门」会把排查引到门配错的方向，而真正的问题是那个槽位只收字符串。
func encodeTextContent(parts []canonical.Part) (any, error) {
	var texts []string
	for _, p := range parts {
		if p.Kind != canonical.PartText {
			return nil, canonical.Newf(canonical.ClassBadRequest,
				"字符串 content 无法编码 %q 内容块", string(p.Kind))
		}
		texts = append(texts, p.Text)
	}
	return strings.Join(texts, "\n"), nil
}

// encodeBlockContent 把内容块编成 Native 的单键块数组，顺序原样保留——
// 图文顺序变了，模型对「这张图」的指代就落到了另一张图上。
func encodeBlockContent(parts []canonical.Part) (any, error) {
	blocks := make([]map[string]string, 0, len(parts))
	for _, p := range parts {
		switch p.Kind {
		case canonical.PartText:
			blocks = append(blocks, map[string]string{"text": p.Text})
		case canonical.PartMedia:
			block, err := encodeMedia(p.Media)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, block)
		default:
			return nil, canonical.Newf(canonical.ClassBadRequest,
				"多模态门无法编码 %q 内容块", string(p.Kind))
		}
	}
	return blocks, nil
}

// encodeMedia 把一份媒体负载编成单键内容块。
//
// 三条硬约束（原则 2.6）：URL 原样透传，网关不代下载；内联字节编成 data URI；
// FileRef 绑定具体 Provider，跨 Provider 搬运一律显式报错。落不了地的媒体类别
// 由 mediaKey 挡下（本路径只认 image 与 audio），报错而不是塞进别的键。
func encodeMedia(m *canonical.Media) (map[string]string, error) {
	key, err := mediaKey(m.Kind)
	if err != nil {
		return nil, err
	}
	switch {
	case m.URL != "":
		return map[string]string{key: m.URL}, nil
	case len(m.Data) > 0:
		uri, err := dataURI(m)
		if err != nil {
			return nil, err
		}
		return map[string]string{key: uri}, nil
	default:
		return nil, canonical.Newf(canonical.ClassUnsupported,
			"文件引用绑定 %q，不得跨 Provider 搬运", m.FileRef.Provider)
	}
}

// mediaKey 给出媒体类别对应的内容块键名。
//
// 只认 image 与 audio。video 不在其中：Chat 线格式表达不出视频输入，本条路径
// 也把它声明为不可能——上游端点认识 video 键，不构成在这条路上编码它的授权。
// 真编出去等于绕开降级矩阵开了一条没登记的暗路。
func mediaKey(k canonical.MediaKind) (string, error) {
	switch k {
	case canonical.MediaImage:
		return "image", nil
	case canonical.MediaAudio:
		return "audio", nil
	default:
		return "", canonical.Newf(canonical.ClassUnsupported,
			"DashScope Native 内容块没有 %q 的落点", string(k))
	}
}

// dataURI 把内联字节编成 data URI，MIME 只认客户端提交的那一个。
//
// 缺 MIME 时报错而不是兜底：拼出来的 "data:;base64,..." 语法合法却没有类型，
// 上游无从判断解码方式，整块负载被当成不认识的字符串丢掉，请求仍是 200；
// 而按采样参数猜一个 MIME 是凭空造事实——猜错时同样静默失败。
func dataURI(m *canonical.Media) (string, error) {
	if m.MIMEType == "" {
		return "", canonical.Newf(canonical.ClassBadRequest,
			"内联 %q 负载缺少 MIME 类型", string(m.Kind))
	}
	return "data:" + m.MIMEType + ";base64," + base64.StdEncoding.EncodeToString(m.Data), nil
}
