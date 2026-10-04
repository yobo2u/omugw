// Package dashscoperealtime 只读窥探 DashScope Realtime 已有公开依据的观测字段。
// 它不重建消息，也不因 OpenAI 的同名字段或模型名猜测计费单位。
package dashscoperealtime

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"

	"github.com/yobo2u/omugw/internal/canonical"
)

// MaxIDBytes 防止长连接的去重键随上游输入无限膨胀。
const MaxIDBytes = 512

type Event struct {
	Type, ID, Source, Status string
	Started, Terminal        bool
	Usage                    canonical.Usage
	Characters               *int64
	Diagnostic               string
	Failure                  *canonical.Error
}

// Inspect 不持有 raw；即使解析失败，调用方仍拥有未被改写的完整负载。
// error 表示包络无法安全关联；未知业务事件与不可核验的用量只观测，不替客户端裁决。
func Inspect(raw []byte) (Event, error) {
	e := Event{Usage: canonical.UnavailableUsage()}
	if !json.Valid(raw) {
		return e, errEnvelope
	}
	root := jsonValue(bytes.TrimSpace(raw))
	typ, err := root.field("type")
	if err != nil {
		return e, errEnvelope
	}
	e.Type, err = typ.text(128)
	if err != nil || e.Type == "" {
		return e, errEnvelope
	}
	var container jsonValue
	idField := "id"
	switch e.Type {
	case "session.created":
		e.Source, e.Started = "session", true
		container, err = root.field("session")
	case "session.finished":
		// 官方事件没有 session.id，账本必须关联先前唯一的 session.created。
		e.Source, e.Terminal = "session", true
		e.Diagnostic = unverifiedUsage(root)
		return e, nil
	case "response.created", "response.done":
		e.Source = "response"
		e.Started, e.Terminal = e.Type == "response.created", e.Type == "response.done"
		container, err = root.field("response")
	case "input_audio_buffer.committed", "conversation.item.input_audio_transcription.completed", "conversation.item.input_audio_transcription.failed":
		e.Source = "transcription"
		e.Started = e.Type == "input_audio_buffer.committed"
		e.Terminal = !e.Started
		container, idField = root, "item_id"
		if e.Terminal {
			e.Status = "completed"
			if e.Type == "conversation.item.input_audio_transcription.failed" {
				e.Status = "failed"
				e.Failure = inspectFailure(root)
			}
			e.Diagnostic = unverifiedUsage(root)
		}
	case "error":
		e.Failure = inspectFailure(root)
		return e, nil
	default:
		return e, nil
	}
	if err != nil {
		return e, errEnvelope
	}
	id, err := container.field(idField)
	if err != nil {
		return e, errEnvelope
	}
	e.ID, err = id.text(MaxIDBytes)
	if err != nil || e.ID == "" {
		return e, errEnvelope
	}
	if e.Source == "response" {
		status, err := container.field("status")
		if err != nil {
			return e, errEnvelope
		}
		if status != nil {
			e.Status, err = status.text(128)
			if err != nil {
				return e, errEnvelope
			}
		}
		if e.Terminal {
			usage, err := container.field("usage")
			if err != nil {
				e.Diagnostic = "usage_invalid"
			} else {
				e.Usage, e.Characters, e.Diagnostic = inspectUsage(usage)
			}
		}
	}
	return e, nil
}

func unverifiedUsage(v jsonValue) string {
	u, err := v.field("usage")
	if err == nil && (u == nil || bytes.Equal(u, []byte("null"))) {
		return "usage_missing"
	}
	return "usage_unverified"
}

func inspectUsage(v jsonValue) (canonical.Usage, *int64, string) {
	unavailable := canonical.UnavailableUsage()
	if v == nil || bytes.Equal(v, []byte("null")) {
		return unavailable, nil, "usage_missing"
	}
	if v[0] != '{' {
		return unavailable, nil, "usage_unverified"
	}
	var values [6]jsonValue
	for i, name := range [...]string{"characters", "input_tokens", "output_tokens", "total_tokens", "input_tokens_details", "output_tokens_details"} {
		var err error
		values[i], err = v.field(name)
		if err != nil {
			return unavailable, nil, "usage_invalid"
		}
	}
	if values[0] != nil {
		for _, tokenField := range values[1:] {
			if tokenField != nil {
				return unavailable, nil, "usage_ambiguous"
			}
		}
		characters, ok := values[0].count()
		if !ok {
			return unavailable, nil, "usage_invalid"
		}
		return unavailable, &characters, ""
	}
	if values[1] == nil && values[2] == nil && values[3] == nil && values[4] == nil && values[5] == nil {
		return unavailable, nil, "usage_unverified"
	}
	input, okIn := values[1].count()
	output, okOut := values[2].count()
	if !okIn || !okOut || input > math.MaxInt64-output {
		return unavailable, nil, "usage_invalid"
	}
	if values[3] != nil {
		total, ok := values[3].count()
		if !ok || total != input+output {
			return unavailable, nil, "usage_invalid"
		}
	}
	audioIn, okIn := audioTokens(values[4], input)
	audioOut, okOut := audioTokens(values[5], output)
	if !okIn || !okOut {
		return unavailable, nil, "usage_invalid"
	}
	return canonical.Usage{Fidelity: canonical.FidelityAuthoritative, InputTokens: input, OutputTokens: output, AudioInputTokens: audioIn, AudioOutputTokens: audioOut}, nil, ""
}

func audioTokens(v jsonValue, total int64) (int64, bool) {
	if v == nil {
		return 0, true
	}
	var audio, text int64
	for _, name := range [...]string{"audio_tokens", "text_tokens"} {
		raw, err := v.field(name)
		if err != nil {
			return 0, false
		}
		if raw == nil {
			continue
		}
		n, ok := raw.count()
		if !ok || n > total {
			return 0, false
		}
		if name == "audio_tokens" {
			audio = n
		} else {
			text = n
		}
	}
	return audio, audio <= total-text
}

func inspectFailure(root jsonValue) *canonical.Error {
	// 官方示例只确认这两个值；不复用 HTTP/message 关键字分类以免误冷却凭据。
	failure := canonical.Newf(canonical.ClassInternal, "unverified Realtime upstream error")
	v, err := root.field("error")
	if err != nil {
		return failure
	}
	for _, name := range [...]string{"code", "type"} {
		raw, err := v.field(name)
		if err != nil {
			return failure
		}
		s, err := raw.text(128)
		if err == nil && (name == "code" && s == "invalid_value" || name == "type" && s == "invalid_request_error") {
			failure = canonical.Newf(canonical.ClassBadRequest, "Realtime upstream rejected request")
			failure.UpstreamCode = s
			return failure
		}
	}
	return failure
}

// ClassifyClose 只认已实测的限流组合，不能用 reason 中的 auth/quota 单词猜测凭据状态。
func ClassifyClose(code uint16, reason string) *canonical.Error {
	if code == 1000 || code == 1001 {
		return nil
	}
	if code == 1011 && (reason == "To many requests" || strings.HasPrefix(reason, "To many requests.")) {
		return canonical.Newf(canonical.ClassRateLimit, "Realtime upstream throttled connection")
	}
	return canonical.Newf(canonical.ClassInternal, "unclassified Realtime connection close")
}
