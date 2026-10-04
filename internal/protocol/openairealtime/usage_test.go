package openairealtime

import (
	"reflect"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

func number(n int64) *int64 { return &n }

func responseUsage(t *testing.T, usage string) Event {
	t.Helper()
	fields := ""
	if usage != "" {
		fields = `,"usage":` + usage
	}
	e, err := Inspect([]byte(`{"type":"response.done","response":{"id":"r","status":"completed"` + fields + `}}`))
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestOpenAIRealtimeResponseUsage(t *testing.T) {
	const usage = `{"input_tokens":132,"output_tokens":121,"total_tokens":253,"input_token_details":{"text_tokens":119,"audio_tokens":13,"image_tokens":0,"cached_tokens":64,"cached_tokens_details":{"text_tokens":64,"audio_tokens":0,"image_tokens":0}},"output_token_details":{"text_tokens":30,"audio_tokens":91,"reasoning_tokens":999}}`
	want := canonical.Usage{Fidelity: canonical.FidelityAuthoritative, InputTokens: 132, OutputTokens: 121, AudioInputTokens: 13, AudioOutputTokens: 91, CacheReadInputTokens: 64}
	wantDetails := TokenDetails{TextInput: number(119), AudioInput: number(13), ImageInput: number(0), CachedInput: number(64), CachedTextInput: number(64), CachedAudioInput: number(0), CachedImageInput: number(0), TextOutput: number(30), AudioOutput: number(91)}
	for _, status := range []string{"completed", "cancelled", "failed", "incomplete"} {
		e, err := Inspect([]byte(`{"type":"response.done","response":{"id":"r","status":"` + status + `","usage":` + usage + `}}`))
		if err != nil || e.Usage != want || !reflect.DeepEqual(e.Details, wantDetails) || e.Diagnostic != "" || e.Source != "response" || e.ID != "r" || e.Status != status || !e.Terminal || e.Started || e.Seconds != nil {
			t.Fatalf("%+v %v", e, err)
		}
	}
	e := responseUsage(t, `{"input_tokens":10,"output_tokens":2,"input_token_details":{"text_tokens":2,"audio_tokens":3,"image_tokens":5,"cached_tokens":4,"cached_tokens_details":{"text_tokens":1,"audio_tokens":1,"image_tokens":2}},"output_token_details":{"text_tokens":2,"audio_tokens":0}}`)
	if e.Diagnostic != "" || e.Details.ImageInput == nil || *e.Details.ImageInput != 5 || e.Details.CachedImageInput == nil || *e.Details.CachedImageInput != 2 || e.Usage.TotalTokens() != 12 || e.Usage.ImageCount != 0 {
		t.Fatal(e)
	}
	e, err := Inspect([]byte(`{"type":"response.created","response":{"id":"r","status":"in_progress","usage":{"input_tokens":99,"output_tokens":99}}}`))
	if err != nil || !e.Started || e.Terminal || e.Usage != canonical.UnavailableUsage() {
		t.Fatalf("%+v %v", e, err)
	}
}

func TestOpenAIRealtimeUsagePresenceAndInvalid(t *testing.T) {
	for _, tc := range []struct {
		usage, diag   string
		authoritative bool
	}{
		{``, "usage_missing", false}, {`null`, "usage_missing", false}, {`[]`, "usage_unverified", false}, {`{}`, "usage_unverified", false},
		{`{"input_tokens":0,"output_tokens":0,"total_tokens":0}`, "", true},
		{`{"input_tokens":0,"output_tokens":0,"input_tokens_details":{"audio_tokens":9},"output_tokens_details":{"audio_tokens":9}}`, "", true},
		{`{"input_tokens":0,"output_tokens":0,"input_token_details":{}}`, "", true},
		{`{"input_tokens":0,"output_tokens":0,"input_token_details":{"future_tokens":999}}`, "", true},
		{`{"input_tokens":0,"output_tokens":0,"input_token_details":{"audio_tokens":0}}`, "", true},
		{`{"input_tokens":0,"output_tokens":0,"input_token_details":null}`, "usage_invalid", true},
		{`{"input_tokens":0,"output_tokens":0,"input_token_details":{"audio_tokens":null}}`, "usage_invalid", true},
		{`{"input_tokens":0,"output_tokens":0,"input_token_details":{"audio_tokens":-1}}`, "usage_invalid", true},
		{`{"input_tokens":0,"output_tokens":0,"input_token_details":{"audio_tokens":1.0}}`, "usage_invalid", true},
		{`{"input_tokens":0,"output_tokens":0,"input_token_details":{"audio_tokens":9223372036854775808}}`, "usage_invalid", true},
		{`{"input_tokens":0,"output_tokens":0,"input_token_details":{"audio_tokens":0,"audio_tokens":0}}`, "usage_invalid", true},
		{`{"input_tokens":0,"output_tokens":0,"input_token_details":{},"input_token_details":{}}`, "usage_invalid", true},
		{`{"input_tokens":0}`, "usage_invalid", false}, {`{"input_tokens":null,"output_tokens":0}`, "usage_invalid", false},
		{`{"input_tokens":-1,"output_tokens":0}`, "usage_invalid", false}, {`{"input_tokens":1.0,"output_tokens":0}`, "usage_invalid", false},
		{`{"input_tokens":1e2,"output_tokens":0}`, "usage_invalid", false}, {`{"input_tokens":9223372036854775808,"output_tokens":0}`, "usage_invalid", false},
		{`{"input_tokens":9223372036854775807,"output_tokens":1}`, "usage_invalid", false},
		{`{"input_tokens":0,"inpu\u0074_tokens":0,"output_tokens":0}`, "usage_invalid", false},
		{`{"input_tokens":0,"output_tokens":0,"total_tokens":1}`, "usage_invalid", false},
		{`{"input_tokens":0,"output_tokens":0,"total_tokens":null}`, "usage_invalid", false},
		{`{"input_tokens":0,"output_tokens":0,"total_tokens":0,"total_tokens":0}`, "usage_invalid", false},
	} {
		e := responseUsage(t, tc.usage)
		if e.Diagnostic != tc.diag || (e.Usage.Fidelity == canonical.FidelityAuthoritative) != tc.authoritative {
			t.Fatalf("%s: %+v", tc.usage, e)
		}
		if tc.diag != "" && e.Details != (TokenDetails{}) {
			t.Fatalf("发布非法明细: %+v", e.Details)
		}
	}
	e := responseUsage(t, `{"input_tokens":132,"output_tokens":121,"input_token_details":{"audio_tokens":13}}`)
	if e.Diagnostic != "" || e.Details.TextInput != nil || e.Details.AudioInput == nil || *e.Details.AudioInput != 13 || e.Details.CachedInput != nil || e.Details.AudioOutput != nil {
		t.Fatal(e)
	}
	e = responseUsage(t, `{"input_tokens":9223372036854775807,"output_tokens":0}`)
	if e.Usage.InputTokens != 9223372036854775807 || e.Diagnostic != "" {
		t.Fatal(e)
	}
	if e, err := Inspect([]byte(`{"type":"response.done","response":{"id":"r","usage":{},"usage":{}}}`)); err != nil || e.Diagnostic != "usage_invalid" {
		t.Fatalf("%+v %v", e, err)
	}
}

func TestOpenAIRealtimeDetailsAreSubsets(t *testing.T) {
	for _, details := range []string{
		`"input_token_details":{"audio_tokens":11}`,
		`"output_token_details":{"audio_tokens":3}`,
		`"input_token_details":{"text_tokens":2,"audio_tokens":3,"image_tokens":4}`,
		`"input_token_details":{"text_tokens":8,"audio_tokens":8}`,
		`"output_token_details":{"text_tokens":0,"audio_tokens":1,"reasoning_tokens":1}`,
		`"input_token_details":{"cached_tokens":11}`,
		`"input_token_details":{"cached_tokens":4,"cached_tokens_details":{"audio_tokens":5}}`,
		`"input_token_details":{"text_tokens":1,"cached_tokens":4,"cached_tokens_details":{"text_tokens":2}}`,
		`"input_token_details":{"image_tokens":1,"cached_tokens":4,"cached_tokens_details":{"image_tokens":2}}`,
		`"input_token_details":{"audio_tokens":1,"cached_tokens":4,"cached_tokens_details":{"audio_tokens":2}}`,
		`"input_token_details":{"cached_tokens":4,"cached_tokens_details":{"text_tokens":1,"audio_tokens":1,"image_tokens":1}}`,
		`"input_token_details":{"cached_tokens":4,"cached_tokens_details":{"text_tokens":3,"audio_tokens":3}}`,
		`"input_token_details":{"cached_tokens_details":null}`,
	} {
		e := responseUsage(t, `{"input_tokens":10,"output_tokens":2,`+details+`}`)
		if e.Diagnostic != "usage_invalid" || e.Usage != (canonical.Usage{Fidelity: canonical.FidelityAuthoritative, InputTokens: 10, OutputTokens: 2}) || e.Details != (TokenDetails{}) {
			t.Fatalf("%s: %+v", details, e)
		}
	}
}

func TestOpenAIRealtimePartialDetailsNeverInferParents(t *testing.T) {
	e := responseUsage(t, `{"input_tokens":10,"output_tokens":2,"input_token_details":{"text_tokens":2,"audio_tokens":3,"cached_tokens_details":{"text_tokens":1,"audio_tokens":1,"image_tokens":2}}}`)
	want := TokenDetails{TextInput: number(2), AudioInput: number(3), CachedTextInput: number(1), CachedAudioInput: number(1), CachedImageInput: number(2)}
	if e.Diagnostic != "" || !reflect.DeepEqual(e.Details, want) || e.Usage.CacheReadInputTokens != 0 || e.Usage.ImageCount != 0 {
		t.Fatalf("缺省父项/模态不得按明细和推算: %+v", e)
	}
}

func TestOpenAIRealtimeTranscriptionUsageUnits(t *testing.T) {
	for _, tc := range []struct {
		usage, diag string
		seconds     *float64
		tokens      bool
	}{
		{`{"type":"tokens","input_tokens":13,"output_tokens":9,"total_tokens":22,"input_token_details":{"text_tokens":0,"audio_tokens":13}}`, "", nil, true},
		{`{"type":"duration","seconds":1.25}`, "", floatNumber(1.25), false},
		{`{"type":"duration","seconds":0}`, "", floatNumber(0), false},
		{`{"type":"duration","seconds":1e-2}`, "", floatNumber(0.01), false},
		{`{"type":"duration","seconds":-1}`, "usage_invalid", nil, false},
		{`{"type":"duration","seconds":1e999}`, "usage_invalid", nil, false},
		{`{"type":"duration","seconds":"NaN"}`, "usage_invalid", nil, false},
		{`{"type":"duration","seconds":null}`, "usage_invalid", nil, false},
		{`{"type":"duration"}`, "usage_invalid", nil, false},
		{`{"type":"duration","seconds":0,"seconds":1}`, "usage_invalid", nil, false},
		{`{"input_tokens":13,"output_tokens":9}`, "usage_unverified", nil, false},
		{`{"type":"future","input_tokens":13,"output_tokens":9}`, "usage_unverified", nil, false},
		{`{"type":"tokens","type":"duration","seconds":1}`, "usage_invalid", nil, false},
		{`null`, "usage_missing", nil, false},
	} {
		e, err := Inspect([]byte(`{"type":"conversation.item.input_audio_transcription.completed","item_id":"i","content_index":0,"transcript":"private","usage":` + tc.usage + `}`))
		if err != nil || e.Source != "transcription" || e.Diagnostic != tc.diag || !reflect.DeepEqual(e.Seconds, tc.seconds) {
			t.Fatalf("%s: %+v %v", tc.usage, e, err)
		}
		want := canonical.UnavailableUsage()
		if tc.tokens {
			want = canonical.Usage{Fidelity: canonical.FidelityAuthoritative, InputTokens: 13, OutputTokens: 9, AudioInputTokens: 13}
		}
		if e.Usage != want {
			t.Fatal(e)
		}
		if tc.tokens && !reflect.DeepEqual(e.Details, TokenDetails{TextInput: number(0), AudioInput: number(13)}) {
			t.Fatal(e.Details)
		}
	}
	// ASR 两模态完整时校验和，不能把 response 的 image 维度混进来凑数。
	e, err := Inspect([]byte(`{"type":"conversation.item.input_audio_transcription.completed","item_id":"i","content_index":0,"usage":{"type":"tokens","input_tokens":13,"output_tokens":9,"input_token_details":{"text_tokens":0,"audio_tokens":12,"image_tokens":1}}}`))
	if err != nil || e.Diagnostic != "usage_invalid" || e.Usage.Fidelity != canonical.FidelityAuthoritative || e.Details != (TokenDetails{}) {
		t.Fatalf("%+v %v", e, err)
	}
}

func floatNumber(n float64) *float64 { return &n }
