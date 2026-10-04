package dashscopeinference

import (
	"bytes"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

const runEnvelope = `{"header":{"action":"run-task","task_id":"任务-a","streaming":"duplex"},"payload":{"model":"qwen-audio-3.1-asr-flash-streaming","task_group":"audio","task":"asr","function":"recognition","input":{}}}`

func TestInferenceInspectEnvelope(t *testing.T) {
	want := ClientFacts{Action: "run-task", TaskID: "任务-a", Binding: BindingFields{
		Streaming: TextField{Value, "duplex"}, Model: TextField{Value, "qwen-audio-3.1-asr-flash-streaming"},
		TaskGroup: TextField{Value, "audio"}, Task: TextField{Value, "asr"}, Function: TextField{Value, "recognition"},
	}}
	facts, err := InspectClient([]byte(runEnvelope))
	if err != nil || facts != want {
		t.Fatalf("run = %+v, %v; want %+v", facts, err, want)
	}
	for _, action := range []string{"continue-task", "finish-task", "future-action"} {
		raw := fmt.Sprintf(`{"header":{"action":%q,"task_id":"not-a-uuid"},"payload":{}}`, action)
		got, err := InspectClient([]byte(raw))
		if err != nil || got != (ClientFacts{Action: action, TaskID: "not-a-uuid"}) {
			t.Errorf("optional binding: %+v, %v", got, err)
		}
	}
	// 缺失、null、错型不能折叠成默认绑定，重复键不能 last-wins。
	bad := []string{
		``, `null`, `[]`, `{}`, `{"header":null,"payload":{}}`, `{"header":[],"payload":{}}`,
		`{"header":{"action":"finish-task","task_id":"a"}}`,
		`{"header":{"action":"finish-task","task_id":"a"},"payload":null}`,
		`{"header":{"action":"finish-task","task_id":"a"},"payload":[]}`,
		strings.Replace(runEnvelope, `"header":`, `"he\u0061der":{},"header":`, 1),
		strings.Replace(runEnvelope, `"payload":`, `"pay\u006coad":{},"payload":`, 1),
		strings.Replace(runEnvelope, `"task_id":"任务-a"`, `"task_id":"a","task_\u0069d":"b"`, 1),
		strings.Replace(runEnvelope, `"action":"run-task"`, `"action":"run-task","act\u0069on":"finish-task"`, 1),
		strings.Replace(runEnvelope, `"model":`, `"mo\u0064el":"other","model":`, 1),
		strings.Replace(runEnvelope, `"task_id":"任务-a"`, `"task_id":"\ud800"`, 1),
		strings.Replace(runEnvelope, `"task_id":"任务-a"`, `"task_id":"\udc00"`, 1),
		strings.Replace(runEnvelope, `"task_id":"任务-a"`, "\"task_id\":\"\xff\"", 1),
	}
	for _, field := range []string{`"action":"run-task"`, `"task_id":"任务-a"`, `"streaming":"duplex"`,
		`"model":"qwen-audio-3.1-asr-flash-streaming"`, `"task_group":"audio"`, `"task":"asr"`, `"function":"recognition"`} {
		key := strings.SplitN(field, ":", 2)[0]
		for _, val := range []string{`null`, `""`, `42`, `true`, `{}`, `[]`} {
			bad = append(bad, strings.Replace(runEnvelope, field, key+":"+val, 1))
		}
		missing := strings.Replace(runEnvelope, field+",", "", 1)
		if missing == runEnvelope {
			missing = strings.Replace(runEnvelope, ","+field, "", 1)
		}
		if missing == runEnvelope {
			t.Fatalf("missing-field fixture did not remove %s", key)
		}
		bad = append(bad, missing)
		if key != `"task_id"` {
			bad = append(bad, strings.Replace(runEnvelope, field, key+`:"bad\nvalue"`, 1))
		}
	}
	for i, raw := range bad {
		got, err := InspectClient([]byte(raw))
		if err == nil || got != (ClientFacts{}) {
			t.Errorf("bad envelope %d: %+v, %v", i, got, err)
		}
	}
	for _, key := range []string{"streaming", "model", "task_group", "task", "function"} {
		for _, val := range []string{`null`, `""`, `17`} {
			header, payload := `"action":"finish-task","task_id":"a"`, ""
			if key == "streaming" {
				header += `,"streaming":` + val
			} else {
				payload = fmt.Sprintf(`%q:%s`, key, val)
			}
			raw := []byte(`{"header":{` + header + `},"payload":{` + payload + `}}`)
			if got, err := InspectClient(raw); err == nil || got != (ClientFacts{}) {
				t.Errorf("explicit invalid optional field %s: %+v, %v", key, got, err)
			}
		}
	}
	for _, tc := range []struct {
		key, old string
		limit    int
	}{
		{"task_id", "任务-a", 512}, {"model", "qwen-audio-3.1-asr-flash-streaming", 512},
		{"action", "run-task", 128}, {"streaming", "duplex", 16},
		{"task_group", "audio", 128}, {"task", "asr", 128}, {"function", "recognition", 128},
	} {
		for _, n := range []int{tc.limit, tc.limit + 1} {
			raw := strings.Replace(runEnvelope, fmt.Sprintf(`%q:%q`, tc.key, tc.old), fmt.Sprintf(`%q:%q`, tc.key, strings.Repeat("a", n)), 1)
			got, err := InspectClient([]byte(raw))
			if (err == nil) != (n == tc.limit) || err != nil && got != (ClientFacts{}) {
				t.Errorf("%s %d bytes: %+v, %v", tc.key, n, got, err)
			}
		}
	}
	for _, id := range []string{`"not-a-uuid"`, `"\ud83d\ude00"`, `"a\n\u0000b"`, `"` + strings.Repeat("é", 256) + `"`} {
		raw := []byte(`{"header":{"task_id":` + id + `,"action":null},"payload":17}`)
		got, ok := VerifiedTaskID(raw)
		var wantID string
		if err := json.Unmarshal([]byte(id), &wantID); err != nil {
			t.Fatal(err)
		}
		if !ok || got != wantID {
			t.Errorf("recover ID = %q, %v", got, ok)
		}
	}
	for _, raw := range []string{`{}`, `null`, `{"header":null}`, `{"header":{"task_id":""}}`,
		`{"header":{"task_id":null}}`, `{"header":{"task_id":1}}`, `{"header":{"task_id":"\ud800"}}`,
		`{"header":{"task_id":"a","task_\u0069d":"b"}}`, `{"header":{"task_id":"a"},"he\u0061der":{}}`,
		`{"header":{"task_id":"` + strings.Repeat("a", 513) + `"}}`, `{"header":{"task_id":"a"}} null`} {
		if id, ok := VerifiedTaskID([]byte(raw)); ok || id != "" {
			t.Errorf("unsafe recovered ID: %q, %v", id, ok)
		}
	}

	t.Run("server", func(t *testing.T) {
		raw := `{"header":{"event":"task-started","task_id":"a"},"payload":{}}`
		got, err := InspectServer([]byte(raw), ModelContract{})
		if err != nil || got != (ServerFacts{Event: "task-started", TaskID: "a"}) {
			t.Fatalf("server = %+v, %v", got, err)
		}
		for _, bad := range []string{
			strings.Replace(raw, `"event":"task-started"`, `"event":null`, 1),
			strings.Replace(raw, `"event":"task-started"`, `"event":"task-started","ev\u0065nt":"task-failed"`, 1),
			strings.Replace(raw, `"payload":{}`, `"payload":{"model":null}`, 1),
			strings.Replace(raw, `"payload":{}`, `"payload":{"model":"a","mo\u0064el":"b"}`, 1),
			strings.Replace(raw, `"task_id":"a"`, `"task_id":"\ud800"`, 1),
		} {
			if got, err := InspectServer([]byte(bad), ModelContract{}); err == nil || got != (ServerFacts{}) {
				t.Errorf("bad server = %+v, %v", got, err)
			}
		}
	})
}

func TestInferenceInspectUsage(t *testing.T) {
	tokens := ModelContract{Unit: UnitTokens, Mode: ModeCumulative}
	characters := ModelContract{Unit: UnitCharacters, Mode: ModeCumulative}
	seconds := ModelContract{Unit: UnitSeconds, Mode: ModeCumulative}
	for _, tc := range []struct {
		name, payload, event string
		contract             ModelContract
		want                 UsageSnapshot
	}{
		{"tokens", `"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}`, "task-finished", tokens, UsageSnapshot{Presence: Value, InputTokens: 2, OutputTokens: 3, TotalTokens: 5}},
		{"zero", `"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}`, "task-finished", tokens, UsageSnapshot{Presence: Value}},
		{"missing", ``, "task-finished", tokens, UsageSnapshot{Presence: Missing}},
		{"null", `"usage":null`, "task-finished", tokens, UsageSnapshot{Presence: Null}},
		{"empty", `"usage":{}`, "task-finished", tokens, UsageSnapshot{Presence: Missing}},
		{"characters", `"usage":{"characters":13}`, "result-generated", characters, UsageSnapshot{Presence: Value, Characters: 13}},
		{"characters-zero", `"usage":{"characters":0}`, "task-finished", characters, UsageSnapshot{Presence: Value}},
		{"characters-null", `"usage":{"characters":null}`, "task-finished", characters, UsageSnapshot{Presence: Null}},
		{"duration", `"usage":{"duration":1.25}`, "result-generated", seconds, UsageSnapshot{Presence: Value, Seconds: 1.25}},
		{"duration-zero", `"usage":{"duration":0}`, "task-finished", seconds, UsageSnapshot{Presence: Value}},
		{"duration-null", `"usage":{"duration":null}`, "task-finished", seconds, UsageSnapshot{Presence: Null}},
		{"nested-not-billing", `"usage":{"other":{"characters":9},"duration":2}`, "task-finished", seconds, UsageSnapshot{Presence: Value, Seconds: 2}},
		{"null-other-unit", `"usage":{"characters":null,"duration":2}`, "task-finished", seconds, UsageSnapshot{Presence: Value, Seconds: 2}},
		{"unknown-event", `"usage":{"characters":13}`, "future-event", characters, UsageSnapshot{}},
		{"started-not-usage", `"usage":{"characters":13}`, "task-started", characters, UsageSnapshot{}},
		{"unknown-model", `"usage":{"characters":13}`, "task-finished", ModelContract{Unit: UnitUnknown}, UsageSnapshot{}},
		{"unverified-mode", `"usage":{"duration":13}`, "task-finished", ModelContract{Unit: UnitSeconds}, UsageSnapshot{}},
		{"unverified-token-mode", `"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}`, "task-finished", LookupModelContract("qwen-audio-3.1-tts-flash"), UsageSnapshot{}},
		{"sentence-not-ended", `"output":{"event":"sentence-begin"},"usage":{"characters":13}`, "result-generated", LookupModelContract("cosyvoice-v2"), UsageSnapshot{}},
		{"sentence-ended", `"output":{"event":"sentence-end"},"usage":{"characters":13}`, "result-generated", LookupModelContract("cosyvoice-v2"), UsageSnapshot{Presence: Value, Characters: 13}},
		{"terminal-no-sentence", `"usage":{"characters":13}`, "task-finished", LookupModelContract("cosyvoice-v2"), UsageSnapshot{Presence: Value, Characters: 13}},
		{"failed-no-sentence", `"usage":{"characters":13}`, "task-failed", LookupModelContract("cosyvoice-v2"), UsageSnapshot{Presence: Value, Characters: 13}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(fmt.Sprintf(`{"header":{"event":%q,"task_id":"a"},"payload":{%s}}`, tc.event, tc.payload))
			got, err := InspectServer(raw, tc.contract)
			if err != nil || got.Usage != tc.want {
				t.Fatalf("usage = %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		name, usage string
		contract    ModelContract
	}{
		{"partial", `{"input_tokens":2,"output_tokens":3}`, tokens},
		{"null-member", `{"input_tokens":2,"output_tokens":null,"total_tokens":5}`, tokens},
		{"wrong-sum", `{"input_tokens":2,"output_tokens":3,"total_tokens":6}`, tokens},
		{"sum-overflow", `{"input_tokens":9223372036854775807,"output_tokens":1,"total_tokens":0}`, tokens},
		{"int-overflow", `{"characters":9223372036854775808}`, characters},
		{"negative", `{"characters":-1}`, characters},
		{"float-count", `{"characters":1.0}`, characters},
		{"exponent-count", `{"characters":1e2}`, characters},
		{"string-count", `{"characters":"2"}`, characters},
		{"duplicate", `{"characters":1,"charact\u0065rs":2}`, characters},
		{"duplicate-token", `{"input_tokens":2,"output_tokens":3,"total_tokens":5,"total_\u0074okens":5}`, tokens},
		{"duration-negative", `{"duration":-1}`, seconds},
		{"duration-overflow", `{"duration":1e400}`, seconds},
		{"duration-underflow", `{"duration":1e-400}`, seconds},
		{"duration-string", `{"duration":"1"}`, seconds},
		{"duration-duplicate", `{"duration":1,"dur\u0061tion":1}`, seconds},
		{"mixed-tokens", `{"characters":0,"input_tokens":2,"output_tokens":3,"total_tokens":5}`, tokens},
		{"mixed-duration", `{"duration":0,"input_tokens":2,"output_tokens":3,"total_tokens":5}`, tokens},
		{"mixed-characters", `{"characters":2,"input_tokens":0}`, characters},
		{"mixed-seconds", `{"duration":2,"characters":false}`, seconds},
		{"wrong-container", `[]`, tokens},
		{"scalar-container", `42`, tokens},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(`{"header":{"event":"task-finished","task_id":"a"},"payload":{"usage":` + tc.usage + `}}`)
			got, err := InspectServer(raw, tc.contract)
			if err != nil || got.Event != "task-finished" || got.TaskID != "a" || got.Usage != (UsageSnapshot{Presence: Invalid}) {
				t.Fatalf("invalid usage must preserve event: %+v, %v", got, err)
			}
		})
	}
	for _, model := range []string{"qwen-audio-3.1-asr-flash-streaming", "qwen-audio-3.1-asr-flash-message"} {
		raw := []byte(`{"header":{"event":"result-generated","task_id":"a"},"payload":{"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5,"duration":19}}}`)
		got, err := InspectServer(raw, LookupModelContract(model))
		if err != nil || got.Usage != (UsageSnapshot{Presence: Value, InputTokens: 2, OutputTokens: 3, TotalTokens: 5}) {
			t.Errorf("compatibility duration: %+v, %v", got, err)
		}
	}
	raw := []byte(`{"header":{"event":"task-finished","task_id":"a"},"payload":{"usage":{"characters":1},"us\u0061ge":{"characters":2}}}`)
	if got, err := InspectServer(raw, characters); err != nil || got.Usage.Presence != Invalid {
		t.Errorf("duplicate usage: %+v, %v", got, err)
	}
}

func TestInferenceInspectBounded(t *testing.T) {
	for _, server := range []bool{false, true} {
		raw := []byte(strings.Replace(runEnvelope, `"input":{}`, `"input":{"blob":"`+strings.Repeat("x", 4<<20)+`"}`, 1))
		if server {
			raw = bytes.Replace(raw, []byte(`"action":"run-task"`), []byte(`"event":"result-generated"`), 1)
			raw = bytes.Replace(raw, []byte(`"input"`), []byte(`"output"`), 1)
		}
		before := bytes.Clone(raw)
		var client ClientFacts
		var upstream ServerFacts
		inspect := func() {
			var err error
			if server {
				upstream, err = InspectServer(raw, ModelContract{})
			} else {
				client, err = InspectClient(raw)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		inspect()
		runtime.GC()
		var start, end runtime.MemStats
		runtime.ReadMemStats(&start)
		inspect()
		runtime.ReadMemStats(&end)
		if delta := end.TotalAlloc - start.TotalAlloc; delta > 256<<10 {
			t.Fatalf("4MiB payload allocated %d bytes", delta)
		} else {
			t.Logf("server=%v: 4MiB allocation=%d bytes", server, delta)
		}
		if !bytes.Equal(raw, before) {
			t.Fatal("inspector changed raw bytes")
		}
		for i := range raw {
			raw[i] = 'x'
		}
		raw = nil
		runtime.GC()
		if server {
			if upstream.Event != "result-generated" || upstream.TaskID != "任务-a" || upstream.Binding.Model.Value != "qwen-audio-3.1-asr-flash-streaming" {
				t.Fatalf("borrowed facts: %+v", upstream)
			}
		} else if client.Action != "run-task" || client.TaskID != "任务-a" || client.Binding.Model.Value != "qwen-audio-3.1-asr-flash-streaming" {
			t.Fatalf("borrowed facts: %+v", client)
		}
	}
	for _, raw := range []string{`{`, `{"header":`, `{"header":{"task_id":"a"},"payload":{}} trailing`,
		`{"header":{"task_id":"a","action":"finish-task"},"payload":{"unknown":` + strings.Repeat("[", 10001) + "0" + strings.Repeat("]", 10001) + `}}`} {
		if got, err := InspectClient([]byte(raw)); err == nil || got != (ClientFacts{}) {
			t.Errorf("malformed client: %+v, %v", got, err)
		}
		if got, err := InspectServer([]byte(raw), ModelContract{}); err == nil || got != (ServerFacts{}) {
			t.Errorf("malformed server: %+v, %v", got, err)
		}
		if _, ok := VerifiedTaskID([]byte(raw)); ok {
			t.Error("malformed recovered ID")
		}
	}
	nested := strings.Replace(runEnvelope, `"input":{}`, `"input":{"unknown":`+strings.Repeat("[", 100)+`{"model":"ignored","model":"also ignored"}`+strings.Repeat("]", 100)+`}`, 1)
	if _, err := InspectClient([]byte(nested)); err != nil {
		t.Fatalf("unknown nested payload: %v", err)
	}
}

func TestInferenceInspectEscapedKeysBounded(t *testing.T) {
	for _, layer := range []string{"output", "payload", "usage"} {
		t.Run(layer, func(t *testing.T) {
			// 单个 blob 不会触发逐键比较；唯一转义扩展键才能守住扫描路径的分配上界。
			var extension strings.Builder
			extension.Grow((4 << 20) + 32)
			for i := 0; extension.Len() < 4<<20; i++ {
				fmt.Fprintf(&extension, `,"x\u0078%06d":0`, i)
			}
			output, usage := `{"event":"sentence-end"}`, `{"characters":1}`
			if layer == "output" {
				output = `{"event":"sentence-end"` + extension.String() + `}`
			}
			if layer == "usage" {
				usage = `{"characters":1` + extension.String() + `}`
			}
			payload := `{"output":` + output + `,"usage":` + usage
			if layer == "payload" {
				payload += extension.String()
			}
			raw := []byte(`{"header":{"event":"result-generated","task_id":"a"},"payload":` + payload + `}}`)
			before := bytes.Clone(raw)
			contract := LookupModelContract("cosyvoice-v2")
			if !contract.SentenceEndOnly || contract.Mode != ModeCumulative {
				t.Fatal("test must exercise the known sentence-end contract")
			}
			inspect := func() {
				got, err := InspectServer(raw, contract)
				want := ServerFacts{Event: "result-generated", TaskID: "a", Usage: UsageSnapshot{Presence: Value, Characters: 1}}
				if err != nil || got != want {
					t.Fatalf("sentence-end usage = %+v, %v", got, err)
				}
			}
			inspect()
			runtime.GC()
			var start, end runtime.MemStats
			runtime.ReadMemStats(&start)
			inspect()
			runtime.ReadMemStats(&end)
			if !bytes.Equal(raw, before) {
				t.Fatal("escaped-key scan changed raw bytes")
			}
			allocated := end.TotalAlloc - start.TotalAlloc
			t.Logf("layer=%s rawBytes=%d allocated=%d budget=%d", layer, len(raw), allocated, 256<<10)
			if allocated > 256<<10 {
				t.Fatalf("4MiB escaped-key %s allocated %d bytes, exceeds 256KiB", layer, allocated)
			}
		})
	}
}

func TestInferenceServerFailure(t *testing.T) {
	for _, code := range []string{"InvalidParameter", "InvalidApiKey", "Throttling", "secret-code", ""} {
		extra := ""
		if code != "" {
			extra = fmt.Sprintf(`,"error_code":%q`, code)
		}
		raw := []byte(`{"header":{"event":"task-failed","task_id":"private-id","error_message":"private-message"` + extra + `},"payload":{}}`)
		got, err := InspectServer(raw, ModelContract{})
		wantClass := canonical.ClassInternal
		if code == "InvalidParameter" {
			wantClass = canonical.ClassBadRequest
		}
		if err != nil || got.Failure == nil || got.Failure.Class != wantClass || got.Failure.Retryable {
			t.Fatalf("failure = %+v, %v", got, err)
		}
		f := got.Failure
		if f.UpstreamCode != "" || f.UpstreamRequestID != "" || f.UpstreamStatus != 0 || f.Param != "" || f.Unwrap() != nil || strings.Contains(f.Message, "private") || strings.Contains(f.Message, code) && code != "" {
			t.Fatalf("unsafe failure: %+v", f)
		}
	}
	for _, value := range []string{`null`, `17`, `""`, `"\ud800"`, `"bad\ncode"`, `"` + strings.Repeat("x", 129) + `"`} {
		raw := []byte(`{"header":{"event":"task-failed","task_id":"a","error_code":` + value + `},"payload":{}}`)
		if got, err := InspectServer(raw, ModelContract{}); err == nil || got != (ServerFacts{}) {
			t.Errorf("invalid error code: %+v, %v", got, err)
		}
	}
}
