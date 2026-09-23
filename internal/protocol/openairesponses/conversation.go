package openairesponses

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/transport/sse"
)

// ExpandConversationRequest 把本地保存的历史放回 input，并移除上游会话状态请求。
// 保留当前 input 的原始条目，防止同源路径在重编码时丢掉 Canonical 尚未建模的字段。
func ExpandConversationRequest(body []byte, history []json.RawMessage) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassBadRequest, "请求体不是 JSON 对象")
	}

	if len(history) > 0 {
		currentItems, err := inputItems(fields)
		if err != nil {
			return nil, err
		}
		items := append(append([]json.RawMessage(nil), history...), currentItems...)
		fields["input"], err = json.Marshal(items)
		if err != nil {
			return nil, canonical.Wrapf(err, canonical.ClassInternal, "序列化会话历史失败")
		}
	}

	delete(fields, "previous_response_id")
	fields["store"] = json.RawMessage("false")
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassInternal, "重写会话请求失败")
	}
	return out, nil
}

// ConversationInputItems 取出本轮原始 input 条目，供响应完成时与输出一起入库。
func ConversationInputItems(body []byte) ([]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassBadRequest, "请求体不是 JSON 对象")
	}
	return inputItems(fields)
}

func inputItems(fields map[string]json.RawMessage) ([]json.RawMessage, error) {
	current, ok := fields["input"]
	if !ok {
		return nil, canonical.Newf(canonical.ClassBadRequest, "请求体缺少 input")
	}
	return normalizeInputItems(current)
}

// EncodeConversationHistory 为没有原始条目的旧轮次生成 Responses input 条目。
func EncodeConversationHistory(messages []canonical.Message) ([]json.RawMessage, error) {
	return encodeHistory(messages)
}

func normalizeInputItems(raw json.RawMessage) ([]json.RawMessage, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err == nil {
		return items, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassBadRequest,
			"input 既不是字符串也不是条目数组")
	}
	item, err := json.Marshal(map[string]any{
		"type": "message", "role": "user", "content": text,
	})
	if err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassInternal, "序列化当前输入失败")
	}
	return []json.RawMessage{item}, nil
}

func encodeHistory(messages []canonical.Message) ([]json.RawMessage, error) {
	var out []json.RawMessage
	for i, msg := range messages {
		items, err := encodeHistoryMessage(msg)
		if err != nil {
			return nil, fmt.Errorf("history[%d]: %w", i, err)
		}
		out = append(out, items...)
	}
	return out, nil
}

func encodeHistoryMessage(msg canonical.Message) ([]json.RawMessage, error) {
	var out []json.RawMessage
	var content []map[string]any

	flush := func() error {
		if len(content) == 0 {
			return nil
		}
		item, err := json.Marshal(map[string]any{
			"type": "message", "role": string(msg.Role), "content": content,
		})
		if err != nil {
			return canonical.Wrapf(err, canonical.ClassInternal, "序列化历史消息失败")
		}
		out = append(out, item)
		content = nil
		return nil
	}

	for j, part := range msg.Parts {
		switch part.Kind {
		case canonical.PartText:
			kind := partInputText
			if msg.Role == canonical.RoleAssistant {
				kind = partOutputText
			}
			content = append(content, map[string]any{"type": kind, "text": part.Text})

		case canonical.PartRefusal:
			content = append(content, map[string]any{"type": partRefusal, "text": part.Text})

		case canonical.PartMedia:
			encoded, err := encodeHistoryMedia(part.Media)
			if err != nil {
				return nil, fmt.Errorf("parts[%d]: %w", j, err)
			}
			content = append(content, encoded)

		case canonical.PartToolCall:
			if err := flush(); err != nil {
				return nil, err
			}
			if part.ToolCall == nil {
				return nil, canonical.Newf(canonical.ClassInternal,
					"parts[%d] 标记为 tool_call 但没有负载", j)
			}
			args := string(part.ToolCall.Arguments)
			if args == "" {
				args = "{}"
			}
			item, err := json.Marshal(map[string]any{
				"type": itemFuncCall, "call_id": part.ToolCall.ID,
				"name": part.ToolCall.Name, "arguments": args,
			})
			if err != nil {
				return nil, canonical.Wrapf(err, canonical.ClassInternal, "序列化历史工具调用失败")
			}
			out = append(out, item)

		case canonical.PartToolResult:
			if err := flush(); err != nil {
				return nil, err
			}
			if part.ToolResult == nil {
				return nil, canonical.Newf(canonical.ClassInternal,
					"parts[%d] 标记为 tool_result 但没有负载", j)
			}
			var text strings.Builder
			for _, resultPart := range part.ToolResult.Content {
				if resultPart.Kind != canonical.PartText {
					return nil, canonical.Newf(canonical.ClassUnsupported,
						"会话历史中的工具结果包含非文本内容")
				}
				text.WriteString(resultPart.Text)
			}
			item, err := json.Marshal(map[string]any{
				"type": itemFuncOutput, "call_id": part.ToolResult.CallID,
				"output": text.String(),
			})
			if err != nil {
				return nil, canonical.Wrapf(err, canonical.ClassInternal, "序列化历史工具结果失败")
			}
			out = append(out, item)

		case canonical.PartThinking:
			return nil, canonical.Newf(canonical.ClassUnsupported,
				"会话历史中的推理条目尚无法无损回放")

		default:
			return nil, canonical.Newf(canonical.ClassInternal,
				"会话历史包含未知内容块 %q", part.Kind)
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return out, nil
}

func encodeHistoryMedia(media *canonical.Media) (map[string]any, error) {
	if media == nil {
		return nil, canonical.Newf(canonical.ClassInternal, "媒体块缺少负载")
	}
	switch media.Kind {
	case canonical.MediaImage:
		url := media.URL
		if len(media.Data) > 0 {
			if media.MIMEType == "" {
				return nil, canonical.Newf(canonical.ClassUnsupported,
					"内联图片缺少 MIME 类型")
			}
			url = "data:" + media.MIMEType + ";base64," + base64.StdEncoding.EncodeToString(media.Data)
		}
		out := map[string]any{"type": partInputImage, "image_url": url}
		if media.Detail != "" {
			out["detail"] = media.Detail
		}
		return out, nil

	case canonical.MediaFile:
		if media.FileRef == nil {
			return nil, canonical.Newf(canonical.ClassInternal, "文件块缺少引用")
		}
		return map[string]any{"type": partInputFile, "file_id": media.FileRef.ID}, nil

	case canonical.MediaAudio:
		format := strings.TrimPrefix(media.MIMEType, "audio/")
		if format == "" && media.Audio != nil {
			format = media.Audio.Encoding
		}
		if len(media.Data) == 0 || format == "" {
			return nil, canonical.Newf(canonical.ClassUnsupported,
				"音频历史缺少内联数据或格式")
		}
		return map[string]any{
			"type": partInputAudio,
			"input_audio": map[string]any{
				"data": base64.StdEncoding.EncodeToString(media.Data), "format": format,
			},
		}, nil

	default:
		return nil, canonical.Newf(canonical.ClassUnsupported,
			"Responses 会话历史无法承载媒体类型 %q", media.Kind)
	}
}

// StoredOutput 同时保留 Canonical 消息与同源路径要逐字回放的原始输出条目。
type StoredOutput struct {
	Messages    []canonical.Message
	Items       []json.RawMessage
	InlineBytes int64
}

// RewriteStoredResponse 替换 response.id，并抽取下一轮需要回放的助手输出。
func RewriteStoredResponse(body []byte, id, previousID string, store bool) ([]byte, StoredOutput, error) {
	var stored StoredOutput
	var err error
	if store {
		stored, err = storedOutput(body)
		if err != nil {
			return nil, StoredOutput{}, err
		}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, StoredOutput{}, canonical.Wrapf(err, canonical.ClassUpstreamUnavailable,
			"上游 Responses 响应不是 JSON 对象")
	}
	if fields == nil {
		return nil, StoredOutput{}, canonical.Newf(canonical.ClassUpstreamUnavailable,
			"上游 Responses 响应不是 JSON 对象")
	}
	rewriteConversationFields(fields, id, previousID, store)
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, StoredOutput{}, canonical.Wrapf(err, canonical.ClassInternal, "重写 response.id 失败")
	}
	return out, stored, nil
}

// RewriteStoredStreamEvent 让一条 Responses SSE 事件使用本地 response.id。
// completed/incomplete 事件会同时返回其中的助手输出，供会话存储原子追加。
func RewriteStoredStreamEvent(ev sse.Event, id, previousID string, store bool) (sse.Event, StoredOutput, bool, error) {
	if ev.Data == "" || ev.Data == "[DONE]" {
		return ev, StoredOutput{}, false, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(ev.Data), &fields); err != nil {
		return ev, StoredOutput{}, false, canonical.Wrapf(err, canonical.ClassUpstreamUnavailable,
			"上游 Responses 事件不是 JSON 对象")
	}
	if fields == nil {
		return ev, StoredOutput{}, false, canonical.Newf(canonical.ClassUpstreamUnavailable,
			"上游 Responses 事件不是 JSON 对象")
	}
	if _, ok := fields["response_id"]; ok {
		fields["response_id"], _ = json.Marshal(id)
	}

	var responseBody []byte
	if raw, ok := fields["response"]; ok {
		var response map[string]json.RawMessage
		if err := json.Unmarshal(raw, &response); err != nil {
			return ev, StoredOutput{}, false, canonical.Wrapf(err, canonical.ClassUpstreamUnavailable,
				"上游 Responses 事件的 response 不是对象")
		}
		if response == nil {
			return ev, StoredOutput{}, false, canonical.Newf(canonical.ClassUpstreamUnavailable,
				"上游 Responses 事件的 response 不是对象")
		}
		rewriteConversationFields(response, id, previousID, store)
		patched, err := json.Marshal(response)
		if err != nil {
			return ev, StoredOutput{}, false, canonical.Wrapf(err, canonical.ClassInternal,
				"重写流式 response.id 失败")
		}
		fields["response"] = patched
		responseBody = patched
	}

	eventType := ev.Event
	if raw := fields["type"]; len(raw) > 0 {
		_ = json.Unmarshal(raw, &eventType)
	}
	terminal := eventType == evCompleted || eventType == evIncomplete
	var stored StoredOutput
	var err error
	if terminal && store {
		if len(responseBody) == 0 {
			return ev, StoredOutput{}, false, canonical.Newf(canonical.ClassUpstreamUnavailable,
				"Responses 终止事件缺少 response")
		}
		stored, err = storedOutput(responseBody)
		if err != nil {
			return ev, StoredOutput{}, false, err
		}
	}
	patched, err := json.Marshal(fields)
	if err != nil {
		return ev, StoredOutput{}, false, canonical.Wrapf(err, canonical.ClassInternal,
			"重写 Responses 流事件失败")
	}
	ev.Data = string(patched)
	return ev, stored, terminal, nil
}

func rewriteConversationFields(fields map[string]json.RawMessage, id, previousID string, store bool) {
	fields["id"], _ = json.Marshal(id)
	if previousID == "" {
		fields["previous_response_id"] = json.RawMessage("null")
	} else {
		fields["previous_response_id"], _ = json.Marshal(previousID)
	}
	fields["store"], _ = json.Marshal(store)
}

func storedOutput(body []byte) (StoredOutput, error) {
	var envelope struct {
		Status string            `json:"status"`
		Output []json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return StoredOutput{}, canonical.Wrapf(err, canonical.ClassUpstreamUnavailable,
			"无法解析上游 Responses 响应")
	}
	if envelope.Status != statusCompleted && envelope.Status != statusIncomplete {
		return StoredOutput{}, canonical.Newf(canonical.ClassUpstreamUnavailable,
			"上游 Responses 响应状态 %q 不能写入会话", envelope.Status)
	}

	var (
		messages []canonical.Message
		inline   int64
	)
	for i, raw := range envelope.Output {
		var item storeOutputItem
		if err := json.Unmarshal(raw, &item); err != nil {
			return StoredOutput{}, canonical.Wrapf(err, canonical.ClassUpstreamUnavailable,
				"无法解析 output[%d]", i)
		}
		switch item.Type {
		case outMessage:
			parts := make([]canonical.Part, 0, len(item.Content))
			for j, part := range item.Content {
				switch part.Type {
				case partOutputText:
					parts = append(parts, canonical.Text(part.Text))
				case partRefusal:
					text := part.Refusal
					if text == "" {
						text = part.Text
					}
					parts = append(parts, canonical.Refusal(text))
				default:
					return StoredOutput{}, canonical.Newf(canonical.ClassUnsupported,
						"output[%d].content[%d] 类型 %q 无法写入会话", i, j, part.Type)
				}
			}
			if len(parts) > 0 {
				messages = append(messages, canonical.Message{Role: canonical.RoleAssistant, Parts: parts})
			}

		case outFuncCall:
			args := json.RawMessage(item.Arguments)
			if item.Arguments == "" {
				args = json.RawMessage("{}")
			}
			if !json.Valid(args) {
				if envelope.Status == statusIncomplete && item.Status != statusCompleted {
					continue
				}
				return StoredOutput{}, canonical.Newf(canonical.ClassUpstreamUnavailable,
					"output[%d] 的工具参数不是合法 JSON", i)
			}
			messages = append(messages, canonical.Message{
				Role: canonical.RoleAssistant,
				Parts: []canonical.Part{{
					Kind: canonical.PartToolCall,
					ToolCall: &canonical.ToolCall{
						ID: item.CallID, Name: item.Name, Arguments: args,
					},
				}},
			})

		case outReasoning:
			var text strings.Builder
			for _, summary := range item.Summary {
				text.WriteString(summary.Text)
			}
			if text.Len() > 0 {
				messages = append(messages, canonical.Message{
					Role: canonical.RoleAssistant,
					Parts: []canonical.Part{{
						Kind:     canonical.PartThinking,
						Thinking: &canonical.Thinking{Text: text.String()},
					}},
				})
			}

		case outImageGeneration:
			if item.Result != "" {
				data, err := base64.StdEncoding.DecodeString(item.Result)
				if err != nil {
					return StoredOutput{}, canonical.Wrapf(err, canonical.ClassUpstreamUnavailable,
						"output[%d] 的生成图片不是合法 base64", i)
				}
				inline += int64(len(data))
			}

		default:
			// 未建模条目仍由 Items 原样保存并在同源下一轮回放；这里不猜它的语义。
		}
	}
	return StoredOutput{Messages: messages, Items: envelope.Output, InlineBytes: inline}, nil
}

type storeOutputItem struct {
	Type      string            `json:"type"`
	Status    string            `json:"status"`
	Content   []storeOutputPart `json:"content"`
	CallID    string            `json:"call_id"`
	Name      string            `json:"name"`
	Arguments string            `json:"arguments"`
	Result    string            `json:"result"`
	Summary   []SummaryPart     `json:"summary"`
}

type storeOutputPart struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Refusal string `json:"refusal"`
}
