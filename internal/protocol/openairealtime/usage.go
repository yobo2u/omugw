package openairealtime

import (
	"bytes"
	"math"
	"strconv"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/protocol/realtimejson"
)

// TokenDetails 的 nil 表示未提供/不可核验，不能用零值冒充真实上游零用量。
// image token 不是 ImageCount；cache 子集也不是另一笔可相加的输入。
type TokenDetails struct {
	TextInput, AudioInput, ImageInput                                *int64
	CachedInput, CachedTextInput, CachedAudioInput, CachedImageInput *int64
	TextOutput, AudioOutput                                          *int64
}

func inspectUsage(container realtimejson.Value, e *Event, transcription bool) {
	v, err := container.Field("usage")
	if err != nil {
		e.Diagnostic = "usage_invalid"
		return
	}
	if v == nil || bytes.Equal(v, []byte("null")) {
		e.Diagnostic = "usage_missing"
		return
	}
	if v[0] != '{' {
		e.Diagnostic = "usage_unverified"
		return
	}
	if transcription {
		typ, err := textField(v, "type", 128, false)
		if err != nil {
			e.Diagnostic = "usage_invalid"
			return
		}
		switch typ {
		case "tokens":
		case "duration":
			e.Duration = true
			e.Seconds, e.Diagnostic = duration(v)
			return
		default:
			e.Diagnostic = "usage_unverified"
			return
		}
	}
	e.Usage, e.Diagnostic = tokenTotals(v)
	if e.Diagnostic != "" {
		return
	}
	details, ok := tokenDetails(v, e.Usage, transcription)
	if !ok {
		// 明细是独立验证域；任何坏明细都不发布，但合法总量仍是权威值。
		e.Diagnostic = "usage_invalid"
		return
	}
	e.Details = details
	if details.AudioInput != nil {
		e.Usage.AudioInputTokens = *details.AudioInput
	}
	if details.AudioOutput != nil {
		e.Usage.AudioOutputTokens = *details.AudioOutput
	}
	if details.CachedInput != nil {
		e.Usage.CacheReadInputTokens = *details.CachedInput
	}
}

func tokenTotals(v realtimejson.Value) (canonical.Usage, string) {
	unavailable := canonical.UnavailableUsage()
	input, errIn := countField(v, "input_tokens")
	output, errOut := countField(v, "output_tokens")
	total, errTotal := countField(v, "total_tokens")
	if errIn != nil || errOut != nil || errTotal != nil {
		return unavailable, "usage_invalid"
	}
	if input == nil && output == nil && total == nil {
		for _, name := range [...]string{"input_token_details", "output_token_details"} {
			field, err := v.Field(name)
			if err != nil || field != nil {
				return unavailable, "usage_invalid"
			}
		}
		return unavailable, "usage_unverified"
	}
	if input == nil || output == nil || *input > math.MaxInt64-*output || total != nil && *total != *input+*output {
		return unavailable, "usage_invalid"
	}
	return canonical.Usage{Fidelity: canonical.FidelityAuthoritative, InputTokens: *input, OutputTokens: *output}, ""
}

func countField(v realtimejson.Value, name string) (*int64, error) {
	raw, err := v.Field(name)
	if err != nil {
		return nil, errEnvelope
	}
	if raw == nil {
		return nil, nil
	}
	n, ok := raw.Count()
	if !ok {
		return nil, errEnvelope
	}
	return &n, nil
}

func tokenDetails(v realtimejson.Value, u canonical.Usage, transcription bool) (TokenDetails, bool) {
	var d TokenDetails
	in, err := v.Field("input_token_details")
	if err != nil {
		return d, false
	}
	if in != nil {
		var ok bool
		d.TextInput, d.AudioInput, d.ImageInput, ok = modalities(in, u.InputTokens, !transcription)
		if !ok {
			return d, false
		}
		if !transcription {
			d.CachedInput, err = countField(in, "cached_tokens")
			if err != nil || d.CachedInput != nil && *d.CachedInput > u.InputTokens {
				return d, false
			}
			cache, err := in.Field("cached_tokens_details")
			if err != nil {
				return d, false
			}
			if cache != nil {
				// cached 总量未提供时，只能证实其分项不超过 input，不推算总 cache。
				d.CachedTextInput, err = countField(cache, "text_tokens")
				if err != nil {
					return d, false
				}
				d.CachedAudioInput, err = countField(cache, "audio_tokens")
				if err != nil {
					return d, false
				}
				d.CachedImageInput, err = countField(cache, "image_tokens")
				if err != nil {
					return d, false
				}
				limit := u.InputTokens
				if d.CachedInput != nil {
					limit = *d.CachedInput
				}
				if !partition(limit, d.CachedInput != nil, d.CachedTextInput, d.CachedAudioInput, d.CachedImageInput) ||
					!subset(d.CachedTextInput, d.TextInput) || !subset(d.CachedAudioInput, d.AudioInput) || !subset(d.CachedImageInput, d.ImageInput) {
					return d, false
				}
			}
		}
	}
	if !transcription {
		out, err := v.Field("output_token_details")
		if err != nil {
			return d, false
		}
		if out != nil {
			var ok bool
			d.TextOutput, d.AudioOutput, _, ok = modalities(out, u.OutputTokens, false)
			if !ok {
				return d, false
			}
		}
	}
	return d, true
}

func modalities(v realtimejson.Value, parent int64, image bool) (text, audio, images *int64, ok bool) {
	var err error
	text, err = countField(v, "text_tokens")
	if err != nil {
		return
	}
	audio, err = countField(v, "audio_tokens")
	if err != nil {
		return
	}
	if image {
		images, err = countField(v, "image_tokens")
		if err != nil {
			return
		}
		ok = partition(parent, true, text, audio, images)
	} else {
		ok = partition(parent, true, text, audio)
	}
	return
}

// partition 只在已知维度齐全时要求相等；缺项绝不按差额补齐，累加也不能溢出。
func partition(parent int64, checkComplete bool, parts ...*int64) bool {
	remaining, complete := parent, true
	for _, n := range parts {
		if n == nil {
			complete = false
			continue
		}
		if *n > remaining {
			return false
		}
		remaining -= *n
	}
	return !checkComplete || !complete || remaining == 0
}

func subset(child, parent *int64) bool {
	return child == nil || parent == nil || *child <= *parent
}

func duration(v realtimejson.Value) (*float64, string) {
	raw, err := v.Field("seconds")
	// 限制数字文本解码分配；不能把任意长 token 或字符串交给 ParseFloat。
	if err != nil || len(raw) == 0 || len(raw) > 128 || raw[0] != '-' && (raw[0] < '0' || raw[0] > '9') {
		return nil, "usage_invalid"
	}
	// JSON 已验证；先检查指数前的有效数字，防非零值下溢后冒充显式零。
	nonzero := false
	for _, c := range raw {
		if c == 'e' || c == 'E' {
			break
		}
		if c >= '1' && c <= '9' {
			nonzero = true
		}
	}
	if !nonzero {
		// -0 及带指数的数学零仍是合法零，统一符号以免污染后续计量。
		zero := 0.0
		return &zero, ""
	}
	if raw[0] == '-' {
		return nil, "usage_invalid"
	}
	n, err := strconv.ParseFloat(string(raw), 64)
	if err != nil || math.IsInf(n, 0) || math.IsNaN(n) || n <= 0 {
		return nil, "usage_invalid"
	}
	return &n, ""
}
