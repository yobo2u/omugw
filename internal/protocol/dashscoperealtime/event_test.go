package dashscoperealtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

func TestInspectRealtimeEvent(t *testing.T) {
	unavailable := canonical.UnavailableUsage()
	tests := []struct {
		name, raw, typ, id, source, status string
		started, terminal                  bool
		usage                              canonical.Usage
		characters                         *int64
		diagnostic                         string
	}{
		{"session", `{"type":"session.created","session":{"id":"s","model":"ignored"}}`, "session.created", "s", "session", "", true, false, unavailable, nil, ""},
		{"created", `{"type":"response.created","response":{"id":"r","status":"in_progress"}}`, "response.created", "r", "response", "in_progress", true, false, unavailable, nil, ""},
		{"done", `{"type":"response.done","response":{"id":"r","status":"completed","usage":{"total_tokens":377,"input_tokens":336,"output_tokens":41,"input_tokens_details":{"text_tokens":228,"audio_tokens":108},"output_tokens_details":{"text_tokens":9,"audio_tokens":32},"plugins":{"search":{"count":1}}}}}`, "response.done", "r", "response", "completed", false, true, canonical.Usage{Fidelity: canonical.FidelityAuthoritative, InputTokens: 336, OutputTokens: 41, AudioInputTokens: 108, AudioOutputTokens: 32}, nil, ""},
		{"cancelled", `{"type":"response.done","response":{"id":"r","status":"cancelled","usage":{"input_tokens":3,"output_tokens":1}}}`, "response.done", "r", "response", "cancelled", false, true, canonical.Usage{Fidelity: canonical.FidelityAuthoritative, InputTokens: 3, OutputTokens: 1}, nil, ""},
		{"failed_usage", `{"type":"response.done","response":{"id":"r","status":"failed","usage":{"input_tokens":3,"output_tokens":0}}}`, "response.done", "r", "response", "failed", false, true, canonical.Usage{Fidelity: canonical.FidelityAuthoritative, InputTokens: 3}, nil, ""},
		{"committed", `{"type":"input_audio_buffer.committed","item_id":"i","previous_item_id":"ignored"}`, "input_audio_buffer.committed", "i", "transcription", "", true, false, unavailable, nil, ""},
		{"transcription", `{"type":"conversation.item.input_audio_transcription.completed","item_id":"i","transcript":"private"}`, "conversation.item.input_audio_transcription.completed", "i", "transcription", "completed", false, true, unavailable, nil, "usage_missing"},
		{"transcription_failed", `{"type":"conversation.item.input_audio_transcription.failed","item_id":"i","error":{"code":"unknown","message":"private"}}`, "conversation.item.input_audio_transcription.failed", "i", "transcription", "failed", false, true, unavailable, nil, "usage_missing"},
		{"transcription_unverified", `{"type":"conversation.item.input_audio_transcription.completed","item_id":"i","usage":{"input_tokens":3,"output_tokens":1}}`, "conversation.item.input_audio_transcription.completed", "i", "transcription", "completed", false, true, unavailable, nil, "usage_unverified"},
		{"finished", `{"type":"session.finished"}`, "session.finished", "", "session", "", false, true, unavailable, nil, "usage_missing"},
		{"unknown", ` {"nested":{"type":"response.done","usage":{"input_tokens":99}},"type":"future.event","usage":{"characters":99}} `, "future.event", "", "", "", false, false, unavailable, nil, ""},
		{"escaped_keys", `{"ty\u0070e":"response.done","respo\u006ese":{"i\u0064":"r\u0031","usage":{"input_tokens":0,"output_tokens":0}}}`, "response.done", "r1", "response", "", false, true, canonical.Usage{Fidelity: canonical.FidelityAuthoritative}, nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := []byte(tt.raw)
			before := bytes.Clone(raw)
			got, err := Inspect(raw)
			if err != nil {
				t.Fatal(err)
			}
			if got.Type != tt.typ || got.ID != tt.id || got.Source != tt.source || got.Status != tt.status || got.Started != tt.started || got.Terminal != tt.terminal || got.Usage != tt.usage || got.Characters != tt.characters || got.Diagnostic != tt.diagnostic {
				t.Fatalf("event = %+v", got)
			}
			if !bytes.Equal(raw, before) {
				t.Fatal("窥探改写了原始负载")
			}
		})
	}
}

func TestInspectRealtimeUsageShapes(t *testing.T) {
	for _, tt := range []struct {
		name, usage, diagnostic string
		input, output, chars    int64
		characters              bool
	}{
		{"zero", `{"input_tokens":0,"output_tokens":0}`, "", 0, 0, 0, false},
		{"max", `{"input_tokens":9223372036854775807,"output_tokens":0}`, "", math.MaxInt64, 0, 0, false},
		{"characters", `{"characters":25}`, "", 0, 0, 25, true},
		{"characters_zero", `{"characters":0}`, "", 0, 0, 0, true},
		{"characters_max", `{"characters":9223372036854775807}`, "", 0, 0, math.MaxInt64, true},
		{"characters_negative", `{"characters":-1}`, "usage_invalid", 0, 0, 0, false},
		{"characters_overflow", `{"characters":9223372036854775808}`, "usage_invalid", 0, 0, 0, false},
		{"characters_float", `{"characters":1.0}`, "usage_invalid", 0, 0, 0, false},
		{"characters_null", `{"characters":null}`, "usage_invalid", 0, 0, 0, false},
		{"negative", `{"input_tokens":-1,"output_tokens":2}`, "usage_invalid", 0, 0, 0, false},
		{"overflow", `{"input_tokens":9223372036854775808,"output_tokens":2}`, "usage_invalid", 0, 0, 0, false},
		{"sum_overflow", `{"input_tokens":9223372036854775807,"output_tokens":1}`, "usage_invalid", 0, 0, 0, false},
		{"fraction", `{"input_tokens":1.2,"output_tokens":2}`, "usage_invalid", 0, 0, 0, false},
		{"exponent", `{"input_tokens":1e2,"output_tokens":2}`, "usage_invalid", 0, 0, 0, false},
		{"string", `{"input_tokens":"1","output_tokens":2}`, "usage_invalid", 0, 0, 0, false},
		{"missing_count", `{"input_tokens":1}`, "usage_invalid", 0, 0, 0, false},
		{"null_count", `{"input_tokens":null,"output_tokens":2}`, "usage_invalid", 0, 0, 0, false},
		{"audio_negative", `{"input_tokens":1,"output_tokens":2,"input_tokens_details":{"audio_tokens":-1}}`, "usage_invalid", 0, 0, 0, false},
		{"audio_overflow", `{"input_tokens":1,"output_tokens":2,"output_tokens_details":{"audio_tokens":9223372036854775808}}`, "usage_invalid", 0, 0, 0, false},
		{"audio_gt_total", `{"input_tokens":1,"output_tokens":2,"output_tokens_details":{"audio_tokens":3}}`, "usage_invalid", 0, 0, 0, false},
		{"null_details", `{"input_tokens":1,"output_tokens":2,"output_tokens_details":null}`, "usage_invalid", 0, 0, 0, false},
		{"wrong_total", `{"input_tokens":1,"output_tokens":2,"total_tokens":99}`, "usage_invalid", 0, 0, 0, false},
		{"duplicate", `{"input_tokens":1,"input_tokens":2,"output_tokens":2}`, "usage_invalid", 0, 0, 0, false},
		{"duplicate_characters", `{"characters":1,"charact\u0065rs":2}`, "usage_invalid", 0, 0, 0, false},
		{"unverified", `{"prompt_tokens":1,"completion_tokens":2}`, "usage_unverified", 0, 0, 0, false},
		{"empty", `{}`, "usage_unverified", 0, 0, 0, false},
		{"array", `[]`, "usage_unverified", 0, 0, 0, false},
		{"null", `null`, "usage_missing", 0, 0, 0, false},
		{"missing", ``, "usage_missing", 0, 0, 0, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			usage := ""
			if tt.usage != "" {
				usage = `,"usage":` + tt.usage
			}
			raw := []byte(`{"type":"response.done","response":{"id":"r"` + usage + `}}`)
			before := bytes.Clone(raw)
			e, err := Inspect(raw)
			if err != nil || e.Diagnostic != tt.diagnostic {
				t.Fatalf("event=%+v err=%v", e, err)
			}
			want := canonical.UnavailableUsage()
			if tt.diagnostic == "" && !tt.characters {
				want = canonical.Usage{Fidelity: canonical.FidelityAuthoritative, InputTokens: tt.input, OutputTokens: tt.output}
			}
			if e.Usage != want || (e.Characters != nil) != tt.characters || (tt.characters && *e.Characters != tt.chars) {
				t.Fatalf("usage=%+v characters=%v", e.Usage, e.Characters)
			}
			if !bytes.Equal(raw, before) {
				t.Fatal("负载发生变更")
			}
		})
	}
}

func TestInspectRealtimeDualUsage(t *testing.T) {
	// 第三次实录的两个单位独立保全；25 不是从输入文字长度或 token 推算。
	const tokens = `"input_tokens":8,"output_tokens":32,"total_tokens":40,"input_tokens_details":{"text_tokens":8},"output_tokens_details":{"text_tokens":0,"audio_tokens":32}`
	wantTokens := canonical.Usage{Fidelity: canonical.FidelityAuthoritative, InputTokens: 8, OutputTokens: 32, AudioOutputTokens: 32}
	for _, tt := range []struct {
		name, fields, diagnostic string
		usage                    canonical.Usage
		chars                    int64
	}{
		{"observed", `"characters":25,` + tokens, "", wantTokens, 25},
		{"zero", `"characters":0,"input_tokens":0,"output_tokens":0,"total_tokens":0,"input_tokens_details":{"text_tokens":0},"output_tokens_details":{"text_tokens":0,"audio_tokens":0}`, "", canonical.Usage{Fidelity: canonical.FidelityAuthoritative}, 0},
		{"missing_characters", tokens, "", wantTokens, -1},
		{"null_characters", `"characters":null,` + tokens, "usage_invalid", wantTokens, -1},
		{"negative_characters", `"characters":-1,` + tokens, "usage_invalid", wantTokens, -1},
		{"overflow_characters", `"characters":9223372036854775808,` + tokens, "usage_invalid", wantTokens, -1},
		{"duplicate_characters", `"characters":25,"charact\u0065rs":26,` + tokens, "usage_invalid", wantTokens, -1},
		{"missing_tokens", `"characters":25`, "", canonical.UnavailableUsage(), 25},
		{"missing_output", `"characters":25,"input_tokens":8`, "usage_invalid", canonical.UnavailableUsage(), 25},
		{"null_input", `"characters":25,"input_tokens":null,"output_tokens":32`, "usage_invalid", canonical.UnavailableUsage(), 25},
		{"negative_output", `"characters":25,"input_tokens":8,"output_tokens":-1`, "usage_invalid", canonical.UnavailableUsage(), 25},
		{"overflow_output", `"characters":25,"input_tokens":8,"output_tokens":9223372036854775808`, "usage_invalid", canonical.UnavailableUsage(), 25},
		{"sum_overflow", `"characters":25,"input_tokens":9223372036854775807,"output_tokens":1`, "usage_invalid", canonical.UnavailableUsage(), 25},
		{"null_total", `"characters":25,"input_tokens":8,"output_tokens":32,"total_tokens":null`, "usage_invalid", canonical.UnavailableUsage(), 25},
		{"wrong_total", `"characters":25,"input_tokens":8,"output_tokens":32,"total_tokens":41`, "usage_invalid", canonical.UnavailableUsage(), 25},
		{"only_total", `"characters":25,"total_tokens":40`, "usage_invalid", canonical.UnavailableUsage(), 25},
		{"only_details", `"characters":25,"output_tokens_details":{"audio_tokens":32}`, "usage_invalid", canonical.UnavailableUsage(), 25},
		{"duplicate_tokens", `"characters":25,"input_tokens":8,"input_tokens":9,"output_tokens":32`, "usage_invalid", canonical.UnavailableUsage(), 25},
		{"both_invalid", `"characters":null,"input_tokens":null`, "usage_invalid", canonical.UnavailableUsage(), -1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw := []byte(`{"type":"response.done","response":{"id":"r","status":"completed","usage":{` + tt.fields + `}}}`)
			before := bytes.Clone(raw)
			e, err := Inspect(raw)
			if err != nil || e.Usage != tt.usage || e.Diagnostic != tt.diagnostic || (e.Characters != nil) != (tt.chars >= 0) || e.Characters != nil && *e.Characters != tt.chars {
				t.Fatalf("event=%+v characters=%v err=%v", e, e.Characters, err)
			}
			if !bytes.Equal(raw, before) {
				t.Fatal("双单位观测改写了原始负载")
			}
		})
	}
	for _, details := range []string{`null`, `{}`, `{"future_tokens":32}`, `{"audio_tokens":null}`, `{"audio_tokens":-1}`, `{"audio_tokens":9223372036854775808}`, `{"audio_tokens":33}`, `{"text_tokens":1,"audio_tokens":32}`, `{"text_tokens":0,"audio_tokens":31}`, `{"audio_tokens":32,"audio_tokens":0}`} {
		t.Run("invalid_details_"+details, func(t *testing.T) {
			e, err := Inspect([]byte(`{"type":"response.done","response":{"id":"r","usage":{"characters":25,"input_tokens":8,"output_tokens":32,"output_tokens_details":` + details + `}}}`))
			if err != nil || e.Diagnostic != "usage_invalid" || e.Usage != canonical.UnavailableUsage() || e.Characters == nil || *e.Characters != 25 {
				t.Fatalf("非法 token 明细污染了独立字符组：%+v %v", e, err)
			}
		})
	}
}

func TestInspectRealtimeBoundaries(t *testing.T) {
	for _, raw := range []string{
		``, `{`, `[]`, `null`, `{}`, `{"type":1}`, `{"type":"error"} {}`,
		`{"type":"future","ignored":[1,]}`, `{"type":"future","ignored":"\x01"}`,
		`{"type":"response.done","response":{}}`,
		`{"type":"response.created","response":{"id":null}}`,
		`{"type":"input_audio_buffer.committed"}`,
		`{"type":"session.created","session":{"id":""}}`,
		`{"type":"response.done","response":{"id":"a","id":"b"}}`,
		`{"type":"response.done","type":"future","response":{"id":"a"}}`,
		`{"type":"response.done","response":{"id":"a"},"response":{"id":"b"}}`,
		`{"type":"response.done","response":{"id":"\ud800"}}`,
		`{"type":"response.done","response":{"id":"` + string([]byte{0xff}) + `"}}`,
		`{"type":"response.done","response":{"id":"` + strings.Repeat("x", 513) + `"}}`,
		`{"type":"response.done","response":{"id":"` + strings.Repeat("界", 171) + `"}}`,
		`{"type":"` + strings.Repeat("x", 129) + `"}`,
	} {
		if _, err := Inspect([]byte(raw)); err == nil {
			t.Errorf("应拒绝不安全包络: %.80q", raw)
		} else if strings.Contains(err.Error(), raw) && raw != "" {
			t.Errorf("错误包含原始包络: %v", err)
		}
	}
	for _, id := range []string{strings.Repeat("x", 512), strings.Repeat(`\u0078`, 512), `r\/\ud83d\ude00`} {
		if _, err := Inspect([]byte(`{"type":"response.created","response":{"id":"` + id + `"}}`)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInspectRealtimeFailureSafety(t *testing.T) {
	for _, tt := range []struct {
		fields string
		class  canonical.ErrorClass
	}{
		{`"code":"invalid_value"`, canonical.ClassBadRequest},
		{`"type":"invalid_request_error"`, canonical.ClassBadRequest},
		{`"code":"unknown-secret","type":"unverified-secret"`, canonical.ClassInternal},
	} {
		raw := []byte(`{"type":"error","error":{` + tt.fields + `,"message":"secret body","param":"secret param"}}`)
		before := bytes.Clone(raw)
		e, err := Inspect(raw)
		if err != nil || e.Failure == nil || e.Failure.Class != tt.class || e.Failure.Retryable || strings.Contains(fmt.Sprint(e.Failure), "secret") || !bytes.Equal(before, raw) {
			t.Fatalf("event=%+v error=%v", e, err)
		}
	}
	for _, tt := range []struct {
		code   uint16
		reason string
		class  canonical.ErrorClass
	}{
		{1000, "secret", ""}, {1001, "secret", ""},
		{1011, "To many requests", canonical.ClassRateLimit},
		{1011, "To many requests. Your requests are being throttled secret", canonical.ClassRateLimit},
		{1008, "To many requests", canonical.ClassInternal},
		{1011, "Too many requests", canonical.ClassInternal},
		{1011, "To many requestsSECRET", canonical.ClassInternal},
		{1011, "InvalidApiKey secret", canonical.ClassInternal},
		{1006, "Arrearage secret", canonical.ClassInternal},
	} {
		e := ClassifyClose(tt.code, tt.reason)
		if tt.class == "" {
			if e != nil {
				t.Fatal(e)
			}
		} else if e == nil || e.Class != tt.class || e.Retryable != (tt.class == canonical.ClassRateLimit) || strings.Contains(fmt.Sprint(e), "secret") || strings.Contains(fmt.Sprint(e), tt.reason) {
			t.Fatalf("close %d: %v", tt.code, e)
		}
	}
}

func TestInspectRealtimeLargeDelta(t *testing.T) {
	// 按字节分配量断言，防一次大字符串分配躲过 AllocsPerRun 的次数统计。
	raw := []byte(`{"type":"response.audio.delta","delta":"` + strings.Repeat("A", 4<<20) + `","unknown":{"array":["\\\"{}",true,null,1e2]}}`)
	before := bytes.Clone(raw)
	result := testing.Benchmark(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			e, err := Inspect(raw)
			if err != nil || e.Type != "response.audio.delta" {
				b.Fatalf("event=%+v error=%v", e, err)
			}
		}
	})
	if result.AllocedBytesPerOp() > 32<<10 {
		t.Fatalf("大 delta 分配量 = %d bytes/op", result.AllocedBytesPerOp())
	}
	if !bytes.Equal(raw, before) {
		t.Fatal("窥探破坏了 audio 字节")
	}
	t.Logf("4 MiB delta: %d bytes/op, %d allocs/op", result.AllocedBytesPerOp(), result.AllocsPerOp())
}

func TestInspectRealtimeOwnsScalarsAndIgnoresModel(t *testing.T) {
	for _, model := range []string{"qwen3-omni-flash-realtime", "qwen3-tts-flash-realtime"} {
		for _, usage := range []string{`{"characters":25}`, `{"input_tokens":3,"output_tokens":2}`} {
			raw := []byte(`{"model":"` + model + `","type":"response.done","response":{"id":"owned","status":"completed","usage":` + usage + `}}`)
			e, err := Inspect(raw)
			if err != nil || e.Diagnostic != "" {
				t.Fatalf("%+v %v", e, err)
			}
			clear(raw)
			if e.ID != "owned" || e.Type != "response.done" || e.Status != "completed" {
				t.Fatal("返回的标量借用了已归还的帧缓冲")
			}
			if strings.Contains(usage, "characters") {
				if e.Characters == nil || *e.Characters != 25 || e.Usage.Fidelity != canonical.FidelityUnavailable {
					t.Fatal(e)
				}
			} else if e.Characters != nil || e.Usage.InputTokens != 3 || e.Usage.OutputTokens != 2 || e.Usage.Fidelity != canonical.FidelityAuthoritative {
				t.Fatal(e)
			}
		}
	}
}

func FuzzInspectRealtimeReadOnly(f *testing.F) {
	for _, seed := range []string{
		`{"type":"response.done","response":{"id":"r","usage":{"input_tokens":1,"output_tokens":2}}}`,
		`{"type":"response.done","response":{"id":"r","usage":{"characters":0}}}`,
		`{"type":"response.done","response":{"id":"r","usage":{"characters":25,"input_tokens":8,"output_tokens":32,"total_tokens":40,"input_tokens_details":{"text_tokens":8},"output_tokens_details":{"text_tokens":0,"audio_tokens":32}}}}`,
		`{"ignored":[{"x":"\\\"{}"},true,null,1e2],"type":"future"}`,
		`{"ty\u0070e":"response.created","response":{"id":"\ud83d\ude00"}}`,
		`{"type":"response.done","response":{"id":"\ud800"}}`,
		`{"type":"error","error":{"code":"invalid_value"}}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 64<<10 {
			t.Skip()
		}
		before := bytes.Clone(raw)
		e, err := Inspect(raw)
		if !bytes.Equal(raw, before) {
			t.Fatal("输入被改写")
		}
		if !json.Valid(raw) && err == nil {
			t.Fatal("非法 JSON 被接受")
		}
		if e.Usage.Validate() != nil || len(e.ID) > 512 || len(e.Type) > 128 || len(e.Status) > 128 {
			t.Fatal("返回观测值越界")
		}
		if e.Characters != nil && *e.Characters < 0 {
			t.Fatal("字符数不得为负")
		}
	})
}
