// Package openairealtime 只读观测 GA 已有依据的小事实，不把负载保全误作跨协议理解。
package openairealtime

import (
	"bytes"
	"errors"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/protocol/realtimejson"
)

// MaxIDBytes 防止去重键随长连接中的上游输入无限膨胀。
const MaxIDBytes = 512

var errEnvelope = errors.New("openai realtime: invalid observation envelope")

// Event 的字符串均独立持有；不允许 raw、音频或转写全文进入长驻观测状态。
type Event struct {
	Type, ID, Source, Status string
	Started, Terminal        bool
	ContentIndex             *int64
	ItemPending              bool
	Usage                    canonical.Usage
	Details                  TokenDetails
	Seconds                  *float64
	// Duration 保留已确认的单位；Seconds=nil 不能把不可用的秒误记成 token。
	Duration      bool
	Diagnostic    string
	Failure       *canonical.Error
	Transcription *bool
}

// Inspect 失败只表示无法安全关联观测，不改写或接管调用方的原始应用消息。
func Inspect(raw []byte) (Event, error) {
	e := Event{Usage: canonical.UnavailableUsage()}
	root, err := realtimejson.Parse(raw)
	if err != nil {
		return e, errEnvelope
	}
	e.Type, err = textField(root, "type", 128, true)
	if err != nil {
		return e, errEnvelope
	}
	switch e.Type {
	case "session.created", "session.updated":
		session, err := root.Field("session")
		if err != nil {
			return e, errEnvelope
		}
		e.ID, err = sessionIdentity(session)
		if err != nil {
			return e, errEnvelope
		}
		// session 仅提供有效配置，GA 没有 session 计费终态，不能凭空建 pending。
		e.Source = "session"
		e.Transcription = transcriptionConfig(session)
	case "response.created", "response.done":
		response, err := root.Field("response")
		if err != nil {
			return e, errEnvelope
		}
		e.ID, err = textField(response, "id", MaxIDBytes, true)
		if err != nil {
			return e, errEnvelope
		}
		e.Status, err = textField(response, "status", 128, false)
		if err != nil {
			return e, errEnvelope
		}
		e.Source = "response"
		e.Started, e.Terminal = e.Type == "response.created", e.Type == "response.done"
		if e.Terminal {
			inspectUsage(response, &e, false)
			if e.Status == "failed" {
				details, _ := response.Field("status_details")
				e.Failure = inspectFailure(details)
			}
		}
	case "input_audio_buffer.committed":
		e.ID, err = textField(root, "item_id", MaxIDBytes, true)
		if err != nil {
			return e, errEnvelope
		}
		// commit 本身不证明启用了 ASR；由 observer 结合已确认配置决定是否登记。
		e.Source, e.ItemPending = "transcription", true
	case "conversation.item.input_audio_transcription.delta", "conversation.item.input_audio_transcription.segment",
		"conversation.item.input_audio_transcription.completed", "conversation.item.input_audio_transcription.failed":
		e.ID, err = textField(root, "item_id", MaxIDBytes, true)
		if err != nil {
			return e, errEnvelope
		}
		e.Terminal = e.Type == "conversation.item.input_audio_transcription.completed" || e.Type == "conversation.item.input_audio_transcription.failed"
		index, err := countField(root, "content_index")
		if err != nil || index == nil && e.Terminal || index != nil && *index > 2147483647 {
			return e, errEnvelope
		}
		e.ContentIndex = index
		e.Source, e.Started = "transcription", !e.Terminal
		// delta 可没有 part；nil 不等于 content_index=0，也不虚构另一个 part。
		e.ItemPending = e.ContentIndex == nil
		if e.Terminal {
			e.Status = "completed"
			if e.Type == "conversation.item.input_audio_transcription.failed" {
				e.Status = "failed"
				e.Failure = inspectFailure(root)
			}
			inspectUsage(root, &e, true)
		}
	case "error":
		e.Failure = inspectFailure(root)
	}
	return e, nil
}

// ValidateReady 只认 GA 身份；模型 alias 回显 snapshot 不构成本地别名不匹配。
// 操作码与 error 首事件的协调归调用方，不能在此假设所有 []byte 都来自文本消息。
func ValidateReady(raw []byte) error {
	e, err := Inspect(raw)
	if err != nil || e.Type != "session.created" {
		return errEnvelope
	}
	return nil
}

func sessionIdentity(v realtimejson.Value) (string, error) {
	typ, err := textField(v, "type", 128, true)
	if err != nil || typ != "realtime" {
		return "", errEnvelope
	}
	object, err := textField(v, "object", 128, true)
	if err != nil || object != "realtime.session" {
		return "", errEnvelope
	}
	return textField(v, "id", MaxIDBytes, true)
}

func transcriptionConfig(session realtimejson.Value) *bool {
	v := session
	for _, name := range [...]string{"audio", "input", "transcription"} {
		var err error
		v, err = v.Field(name)
		if err != nil || v == nil {
			return nil
		}
	}
	enabled := false
	if !bytes.Equal(v, []byte("null")) {
		if _, err := textField(v, "model", MaxIDBytes, true); err != nil {
			return nil
		}
		enabled = true
	}
	return &enabled
}

func textField(v realtimejson.Value, name string, limit int, required bool) (string, error) {
	raw, err := v.Field(name)
	if err != nil {
		return "", errEnvelope
	}
	if raw == nil && !required {
		return "", nil
	}
	s, err := raw.Text(limit)
	if err != nil || required && s == "" {
		return "", errEnvelope
	}
	return s, nil
}

func inspectFailure(root realtimejson.Value) *canonical.Error {
	unknown := canonical.Newf(canonical.ClassInternal, "unverified OpenAI Realtime upstream error")
	v, err := root.Field("error")
	if err != nil {
		return unknown
	}
	// 两个分类字段都先排除重复/损坏，不能让 code 的早返回掩盖歧义 type。
	rawCode, errCode := v.Field("code")
	code := ""
	if errCode == nil && rawCode != nil && !bytes.Equal(rawCode, []byte("null")) {
		code, errCode = rawCode.Text(128)
	}
	typ, errType := textField(v, "type", 128, false)
	if errCode != nil || errType != nil {
		return unknown
	}
	class := codeClass(code)
	known := code
	if class == "" {
		known = typ
		switch typ {
		case "invalid_request_error":
			class = canonical.ClassBadRequest
		case "authentication_error", "permission_error":
			class = canonical.ClassAuth
		case "rate_limit_error":
			class = canonical.ClassRateLimit
		case "insufficient_quota":
			class = canonical.ClassQuota
		case "server_error":
			class = canonical.ClassUpstreamUnavailable
		}
	}
	if class == "" {
		return unknown
	}
	// 不制造 HTTP 状态，不透传任意 message/code/param，更不按错误文案猜重试。
	failure := canonical.Newf(class, "OpenAI Realtime upstream error")
	failure.UpstreamCode = known
	return failure
}

func codeClass(code string) canonical.ErrorClass {
	switch code {
	case "invalid_value":
		return canonical.ClassBadRequest
	case "invalid_api_key":
		return canonical.ClassAuth
	case "context_length_exceeded", "string_above_max_length":
		return canonical.ClassContextLength
	case "content_filter", "content_policy_violation":
		return canonical.ClassContentFilter
	case "insufficient_quota", "billing_hard_limit_reached":
		return canonical.ClassQuota
	case "rate_limit_exceeded":
		return canonical.ClassRateLimit
	}
	return ""
}

// ClassifyClose 不借用 DashScope 实录中的限流文案；未核验的 GA 关闭均不可重试。
func ClassifyClose(code uint16, _ string) *canonical.Error {
	if code == 1000 || code == 1001 {
		return nil
	}
	return canonical.Newf(canonical.ClassInternal, "unclassified OpenAI Realtime connection close")
}
