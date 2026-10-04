package openairealtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

func TestOpenAIRealtimeReadyGA(t *testing.T) {
	for _, session := range []string{
		`{"type":"realtime","object":"realtime.session","id":"s","model":"gpt-realtime-snapshot"}`,
		`{"ty\u0070e":"realtime","object":"realtime.session","i\u0064":"` + strings.Repeat(`\u0078`, 512) + `"}`,
	} {
		raw := []byte(`{"type":"session.created","session":` + session + `}`)
		if err := ValidateReady(raw); err != nil {
			t.Fatal(err)
		}
		e, err := Inspect(raw)
		if err != nil || e.Source != "session" || e.Started || e.Terminal || e.Transcription != nil || e.Usage != canonical.UnavailableUsage() {
			t.Fatalf("%+v %v", e, err)
		}
	}
	for _, raw := range []string{
		`{"type":"session.updated","session":{"type":"realtime","object":"realtime.session","id":"s"}}`,
		`{"type":"error","error":{"code":"invalid_value"}}`, `[]`, `{`,
		`{"type":"session.created","session":{"id":"s","input_audio_format":"pcm16"}}`,
		`{"type":"session.created","session":{"id":"s","type":"transcription","object":"realtime.session"}}`,
		`{"type":"session.created","session":{"id":"s","type":"realtime","object":"other"}}`,
		`{"type":"session.created","session":{"id":"s","type":"realtime"}}`,
		`{"type":"session.created","type":"session.created","session":{"id":"s","type":"realtime","object":"realtime.session"}}`,
		`{"type":"session.created","session":{"id":"s","type":"realtime","object":"realtime.session"},"session":{}}`,
	} {
		if err := ValidateReady([]byte(raw)); err == nil {
			t.Fatalf("接受非 GA ready: %s", raw)
		}
	}
	for _, field := range []string{`"id":""`, `"id":null`, `"id":"\ud800"`, `"id":"` + strings.Repeat("x", 513) + `"`, `"id":"s","i\u0064":"s"`, `"id":"s","type":"realtime"`, `"id":"s","object":"realtime.session"`} {
		raw := []byte(`{"type":"session.created","session":{"type":"realtime","object":"realtime.session",` + field + `}}`)
		if err := ValidateReady(raw); err == nil {
			t.Fatalf("接受模糊身份 %s", field)
		}
	}
}

func TestOpenAIRealtimeTranscriptionConfiguration(t *testing.T) {
	for _, typ := range []string{"session.created", "session.updated"} {
		for _, tc := range []struct {
			fields string
			state  int
		}{
			{`"audio":{"input":{"transcription":{"model":"gpt-4o-mini-transcribe"}}}`, 1},
			{`"audio":{"input":{"transcription":{"model":"future-model","language":"zh"}}}`, 1},
			{`"audio":{"input":{"transcription":null}}`, 0},
			{`"audio":{"input":{}}`, -1}, {`"audio":null`, -1},
			{`"input_audio_transcription":{"model":"legacy"}`, -1},
			{`"audio":{"input":{"transcription":{}}}`, -1},
			{`"audio":{"input":{"transcription":{"model":""}}}`, -1},
			{`"audio":{"input":{"transcription":{"model":"\ud800"}}}`, -1},
			{`"audio":{"input":{"transcription":false}}`, -1},
			{`"audio":{"input":{"transcription":null,"transcription":{"model":"m"}}}`, -1},
			{`"audio":{"input":{"transcription":{"model":"a","model":"b"}}}`, -1},
			{`"audio":{"input":{"transcription":null}},"audio":{}`, -1},
		} {
			e, err := Inspect([]byte(`{"type":"` + typ + `","session":{"type":"realtime","object":"realtime.session","id":"s",` + tc.fields + `}}`))
			if err != nil || e.Started || e.Terminal || (e.Transcription == nil) != (tc.state < 0) || e.Transcription != nil && *e.Transcription != (tc.state == 1) {
				t.Fatalf("%s: %+v %v", tc.fields, e, err)
			}
		}
	}
}

func TestOpenAIRealtimeTranscriptionParts(t *testing.T) {
	for _, tc := range []struct {
		typ, part                  string
		started, terminal, pending bool
		index                      int64
	}{
		{"input_audio_buffer.committed", "", false, false, true, -1},
		{"conversation.item.input_audio_transcription.delta", "", true, false, true, -1},
		{"conversation.item.input_audio_transcription.delta", `,"content_index":0`, true, false, false, 0},
		{"conversation.item.input_audio_transcription.segment", `,"content_index":2`, true, false, false, 2},
		{"conversation.item.input_audio_transcription.completed", `,"content_index":2147483647`, false, true, false, 2147483647},
		{"conversation.item.input_audio_transcription.failed", `,"content_index":1`, false, true, false, 1},
	} {
		e, err := Inspect([]byte(`{"type":"` + tc.typ + `","item_id":"same-item"` + tc.part + `,"transcript":"private","delta":"private"}`))
		if err != nil || e.Source != "transcription" || e.ID != "same-item" || e.Started != tc.started || e.Terminal != tc.terminal || e.ItemPending != tc.pending || (e.ContentIndex == nil) != (tc.index < 0) || e.ContentIndex != nil && *e.ContentIndex != tc.index {
			t.Fatalf("%+v %v", e, err)
		}
		if tc.terminal && e.Diagnostic != "usage_missing" {
			t.Fatal(e)
		}
		if strings.HasSuffix(tc.typ, "failed") && (e.Failure == nil || e.Status != "failed") {
			t.Fatal(e)
		}
	}
	for _, part := range []string{``, `,"content_index":null`, `,"content_index":-1`, `,"content_index":1.0`, `,"content_index":2147483648`, `,"content_index":0,"content_index":1`} {
		if _, err := Inspect([]byte(`{"type":"conversation.item.input_audio_transcription.completed","item_id":"i"` + part + `}`)); err == nil {
			t.Fatalf("接受非法 part %s", part)
		}
	}
	for _, typ := range []string{"response.output_audio_transcript.delta", "response.output_audio_transcript.done", "session.finished", "future.event"} {
		e, err := Inspect([]byte(`{"type":"` + typ + `","item_id":"i","content_index":0,"usage":{"input_tokens":99,"output_tokens":99}}`))
		if err != nil || e.Source != "" || e.Started || e.Terminal || e.ItemPending || e.Usage != canonical.UnavailableUsage() {
			t.Fatalf("%+v %v", e, err)
		}
	}
}

func TestOpenAIRealtimeFailureSafety(t *testing.T) {
	for _, tc := range []struct {
		fields string
		class  canonical.ErrorClass
		retry  bool
	}{
		{`"code":"invalid_value"`, canonical.ClassBadRequest, false},
		{`"type":"invalid_request_error"`, canonical.ClassBadRequest, false},
		{`"type":"server_error"`, canonical.ClassUpstreamUnavailable, true},
		{`"code":"rate_limit_exceeded"`, canonical.ClassRateLimit, true},
		{`"code":"insufficient_quota","type":"invalid_request_error"`, canonical.ClassQuota, true},
		{`"code":"context_length_exceeded","type":"rate_limit_error"`, canonical.ClassContextLength, false},
		{`"type":"authentication_error"`, canonical.ClassAuth, true},
		{`"code":"content_filter"`, canonical.ClassContentFilter, false},
		{`"code":"secret","type":"unknown"`, canonical.ClassInternal, false},
		{`"code":"rate_limit_exceeded","code":"secret"`, canonical.ClassInternal, false},
		{`"code":"invalid_value","type":"server_error","type":"secret"`, canonical.ClassInternal, false},
	} {
		e, err := Inspect([]byte(`{"type":"error","error":{` + tc.fields + `,"message":"secret rate limit auth","param":"secret"}}`))
		if err != nil || e.Failure == nil || e.Failure.Class != tc.class || e.Failure.Retryable != tc.retry || e.Failure.UpstreamStatus != 0 || e.Failure.Param != "" || e.Failure.Unwrap() != nil || strings.Contains(fmt.Sprint(e.Failure), "secret") {
			t.Fatalf("%+v %v", e.Failure, err)
		}
	}
	for _, code := range []uint16{1000, 1001, 1006, 1008, 1011, 1013, 4001} {
		e := ClassifyClose(code, "To many requests. secret rate_limit_exceeded")
		if code == 1000 || code == 1001 {
			if e != nil {
				t.Fatal(e)
			}
			continue
		}
		if e == nil || e.Retryable || e.Class != canonical.ClassInternal || e.UpstreamStatus != 0 || strings.Contains(fmt.Sprint(e), "secret") {
			t.Fatal(e)
		}
	}
}

func TestOpenAIRealtimeErrorNullableCode(t *testing.T) {
	e, err := Inspect([]byte(`{"type":"error","error":{"type":"invalid_request_error","code":null,"message":"private"}}`))
	if err != nil || e.Failure == nil || e.Failure.Class != canonical.ClassBadRequest || e.Failure.Retryable {
		t.Fatalf("空 code 不得抹掉明确 type: %+v %v", e.Failure, err)
	}
}

func TestOpenAIRealtimeInvalidPartNeverEscapes(t *testing.T) {
	e, err := Inspect([]byte(`{"type":"conversation.item.input_audio_transcription.completed","item_id":"i","content_index":2147483648}`))
	if err == nil || e.ContentIndex != nil {
		t.Fatalf("非法 part 不得逃逸到观测记录: %+v %v", e, err)
	}
}

func TestOpenAIRealtimeReadOnlyOwnedFacts(t *testing.T) {
	raw := []byte(`{"ty\u0070e":"response.done","response":{"i\u0064":"owned","status":"completed","usage":{"input_tokens":13,"output_tokens":9,"input_token_details":{"audio_tokens":13}}},"unknown":{"audio":"private"}}`)
	before := bytes.Clone(raw)
	e, err := Inspect(raw)
	if err != nil || !bytes.Equal(before, raw) {
		t.Fatalf("%+v %v", e, err)
	}
	clear(raw)
	if e.Type != "response.done" || e.ID != "owned" || e.Status != "completed" || e.Details.AudioInput == nil || *e.Details.AudioInput != 13 {
		t.Fatal("观测值借用了帧缓冲")
	}
	for _, bad := range []string{`{}`, `{"type":"response.done","response":{"id":"\ud800"}}`, `{"type":"response.done","response":{"id":"a","id":"b"}}`, `{"type":"response.created","response":{"id":"` + strings.Repeat("x", 513) + `"}}`, `{"type":"response.done","response":{"id":"r","status":"ok","status":"bad"}}`} {
		if _, err := Inspect([]byte(bad)); err == nil {
			t.Fatalf("接受模糊关联 %s", bad)
		}
	}
	// 反射约束只守隐私边界，不能让今后新增字段把 raw 或动态容器带进账本。
	var check func(reflect.Type)
	check = func(typ reflect.Type) {
		switch typ.Kind() {
		case reflect.Slice, reflect.Map, reflect.Interface:
			t.Fatalf("长驻观测含动态负载: %s", typ)
		case reflect.Struct:
			for i := 0; i < typ.NumField(); i++ {
				if typ.Field(i).Name != "Failure" {
					check(typ.Field(i).Type)
				}
			}
		case reflect.Pointer:
			check(typ.Elem())
		}
	}
	check(reflect.TypeOf(e))
}

func TestOpenAIRealtimeLargeUnknown(t *testing.T) {
	raw := []byte(`{"type":"conversation.item.input_audio_transcription.delta","item_id":"i","delta":"` + strings.Repeat("A", 4<<20) + `","future":[{"x":"\\\"{}"}]}`)
	before := bytes.Clone(raw)
	r := testing.Benchmark(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			e, err := Inspect(raw)
			if err != nil || e.ID != "i" || e.ContentIndex != nil {
				b.Fatalf("%+v %v", e, err)
			}
		}
	})
	if r.AllocedBytesPerOp() > 32<<10 || !bytes.Equal(before, raw) {
		t.Fatalf("复制或改写负载: %d B/op", r.AllocedBytesPerOp())
	}
	t.Logf("4 MiB delta: %d bytes/op, %d allocs/op", r.AllocedBytesPerOp(), r.AllocsPerOp())
}

func FuzzOpenAIRealtimeReadOnly(f *testing.F) {
	for _, s := range []string{`{"type":"session.created","session":{"type":"realtime","object":"realtime.session","id":"s"}}`, `{"type":"response.done","response":{"id":"r","usage":{"input_tokens":1,"output_tokens":2}}}`, `{"type":"conversation.item.input_audio_transcription.completed","item_id":"i","content_index":0,"usage":{"type":"duration","seconds":1.25}}`, `{"ty\u0070e":"error","error":{"code":"invalid_value"}}`, `{"type":"response.done","response":{"id":"\ud800"}}`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 64<<10 {
			t.Skip()
		}
		before := bytes.Clone(raw)
		e, err := Inspect(raw)
		if !bytes.Equal(before, raw) || !json.Valid(raw) && err == nil {
			t.Fatal("观测改写或接受非法 JSON")
		}
		if e.Usage.Validate() != nil || len(e.ID) > 512 || len(e.Type) > 128 || len(e.Status) > 128 || e.ContentIndex != nil && (*e.ContentIndex < 0 || *e.ContentIndex > 2147483647) {
			t.Fatal("越界观测")
		}
		if err := ValidateReady(raw); err == nil && (e.Type != "session.created" || e.ID == "") {
			t.Fatal("ready 身份丢失")
		}
	})
}
