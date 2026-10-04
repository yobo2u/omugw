//go:build smoke

package smoke_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/transport/ws"
)

func TestDSRealtimeRecorderOfflineBeijingOnly(t *testing.T) {
	root := t.TempDir()
	env := map[string]string{
		"OMUGW_RECORD_DSREALTIME": "1", "OMUGW_RECORD_SCENARIO": "tts-commit",
		"OMUGW_RECORD_OUTPUT":        filepath.Join(root, ".local/recordings/dsrealtime/batch/run"),
		"OMUGW_SMOKE_MODEL_REALTIME": "qwen3-tts-flash-realtime", "DASHSCOPE_API_KEY": "offline-secret",
	}
	for _, host := range []string{"dashscope.aliyuncs.com", "dashscope-intl.aliyuncs.com", "dashscope-us.aliyuncs.com"} {
		t.Run(host, func(t *testing.T) {
			env["OMUGW_SMOKE_WS_URL"] = "wss://" + host + "/api-ws/v1/realtime"
			_, enabled, err := dsRecordConfig(root, func(k string) string { return env[k] })
			if !enabled || (err == nil) != (host == "dashscope.aliyuncs.com") {
				t.Fatal("拨号前预检未精确限制北京域名")
			}
		})
	}
}

func TestDSRealtimeRecorderOfflineInterruptEvidenceSameResponse(t *testing.T) {
	for _, name := range []string{"same_response", "wrong_audio_id", "empty_audio", "bad_base64", "wrong_done_id", "no_created", "done_before_audio", "cancel_before_audio", "reused_terminal_id"} {
		t.Run(name, func(t *testing.T) {
			r := dsInterruptTrace()
			switch name {
			case "wrong_audio_id":
				r.Records[6].Payload = bytes.ReplaceAll(r.Records[6].Payload, []byte(`"r1"`), []byte(`"wrong"`))
			case "empty_audio":
				r.Records[6].Payload = []byte(`{"type":"response.audio.delta","response_id":"r1","delta":""}`)
			case "bad_base64":
				r.Records[6].Payload = []byte(`{"type":"response.audio.delta","response_id":"r1","delta":"!"}`)
			case "wrong_done_id":
				r.Records[8].Payload = bytes.ReplaceAll(r.Records[8].Payload, []byte(`"r1"`), []byte(`"wrong"`))
			case "no_created":
				r.Records = dsRecordOrder(r.Records, 0, 1, 2, 3, 4, 6, 7, 8, 9, 10, 11)
			case "done_before_audio":
				r.Records = dsRecordOrder(r.Records, 0, 1, 2, 3, 4, 5, 8, 6, 7, 8, 9, 10, 11)
			case "cancel_before_audio":
				r.Records = dsRecordOrder(r.Records, 0, 1, 2, 3, 4, 5, 7, 6, 8, 9, 10, 11)
			case "reused_terminal_id":
				r.Records = dsRecordOrder(r.Records, 0, 1, 2, 3, 4, 5, 8, 5, 6, 7, 8, 9, 10, 11)
			}
			want := name == "same_response"
			var events []dsEvidenceEvent
			var cancelIndex int
			var cancelRequest map[string]any
			for i, rec := range r.Records {
				var e map[string]any
				_ = json.Unmarshal(rec.Payload, &e)
				if rec.Kind == "message" && rec.Direction == "receive" {
					events = append(events, dsEvidenceEvent{Node: fmt.Sprintf("n%04d_1", i), Value: e})
				}
				if rec.Direction == "send" && dsString(e, "type") == "response.cancel" {
					cancelIndex = i
					cancelRequest = e
				}
			}
			if dsRequestWitness(r.Records, cancelIndex, cancelRequest) != want {
				t.Error("取消后继见证未绑定同响应的活跃音频链")
			}
			found := false
			for _, c := range dsCoverage(r, events) {
				if c.Capability == "realtime_interrupt_turns" {
					found = true
				}
			}
			if found != want {
				t.Error("打断Coverage拼接了不同实体/已终结响应/无效音频")
			}
			_, err := dsCandidate(r)
			if (err == nil) != want {
				t.Error("候选未拒绝无因果证据的cancel请求")
			}
		})
	}
}

func dsInterruptTrace() dsRecording {
	r := dsRecording{Scenario: "vad-interrupt", Model: "qwen3.5-omni-flash-realtime", Started: time.Now().UTC(), Confirmed: true}
	for _, step := range []struct{ direction, raw string }{
		{"receive", `{"type":"session.created","session":{"id":"s1"}}`},
		{"send", `{"type":"session.update","session":{"turn_detection":{"type":"server_vad"}}}`},
		{"receive", `{"type":"session.updated","session":{"id":"s1","turn_detection":{"type":"server_vad"}}}`},
		{"send", `{"type":"input_audio_buffer.append","audio":"AAAAAA=="}`},
		{"receive", `{"type":"input_audio_buffer.committed","item_id":"in1"}`},
		{"receive", `{"type":"response.created","response":{"id":"r1"}}`},
		{"receive", `{"type":"response.audio.delta","response_id":"r1","delta":"AAAAAA=="}`},
		{"send", `{"type":"response.cancel"}`},
		{"receive", `{"type":"response.done","response":{"id":"r1","status":"cancelled"}}`},
		{"receive", `{"type":"conversation.item.input_audio_transcription.completed","item_id":"in1","transcript":"请描述图片中的颜色和形状。"}`},
	} {
		r.Records = append(r.Records, dsRecord{Direction: step.direction, Kind: "message", Opcode: ws.OpText, Payload: []byte(step.raw)})
	}
	r.Records = append(r.Records, dsRecord{Direction: "send", Kind: "close", CloseCode: 1000}, dsRecord{Direction: "receive", Kind: "close", CloseCode: 1000})
	return r
}

func TestDSRealtimeRecorderOfflineInterruptRejectsForeignAudio(t *testing.T) {
	cancelled := make(chan bool, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		c, err := ws.Accept(w, req, ws.AcceptOptions{MaxPayload: 1 << 20, Idle: time.Second, WriteTimeout: time.Second})
		if err != nil {
			cancelled <- false
			return
		}
		defer c.Close(1000, "")
		p := dsOfflinePeer{t: t, c: c}
		p.update("vad-interrupt")
		p.read("input_audio_buffer.append")
		p.write(`{"type":"input_audio_buffer.committed","item_id":"in1"}`)
		p.write(`{"type":"response.created","response":{"id":"r1"}}`)
		p.write(`{"type":"response.audio.delta","response_id":"WRONG_RESPONSE","delta":"AAAAAA=="}`)
		op, b, err := c.ReadMessage()
		var e map[string]any
		_ = json.Unmarshal(b, &e)
		sent := err == nil && op == ws.OpText && dsString(e, "type") == "response.cancel"
		cancelled <- sent
		if sent {
			p.write(`{"type":"response.done","response":{"id":"r1","status":"cancelled"}}`)
			p.write(`{"type":"conversation.item.input_audio_transcription.completed","item_id":"in1","transcript":"请描述图片中的颜色和形状。"}`)
			_, _, _ = c.ReadMessage()
		}
	}))
	defer server.Close()
	r := dsCapture(context.Background(), dsConfig{URL: "ws" + strings.TrimPrefix(server.URL, "http"), Model: "qwen3.5-omni-flash-realtime", Scenario: "vad-interrupt", Key: "offline-secret", Duration: 2 * time.Second, Responses: 1, Sample: &dsAudioSample{Data: []byte{0, 1, 2, 3}, Rate: 16000, Format: "pcm_s16le"}})
	if <-cancelled || r.Failure == "" {
		t.Fatal("错response_id音频触发了cancel或被当成成功")
	}
	if _, err := dsCandidate(r); err == nil {
		t.Fatal("错实体打断仍生成候选")
	}
}

func TestDSRealtimeRecorderOfflineAudioAfterTerminalRejected(t *testing.T) {
	d := dsDriver{cfg: dsConfig{Scenario: "tts-server-commit", Responses: 1}, r: &dsRecording{}, responses: map[string]bool{}, responseDone: map[string]bool{}}
	for _, raw := range []string{`{"type":"response.created","response":{"id":"r1"}}`, `{"type":"response.done","response":{"id":"r1","status":"completed"}}`} {
		if _, err := d.observe(dsIncoming{op: ws.OpText, payload: []byte(raw)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.observe(dsIncoming{op: ws.OpText, payload: []byte(`{"type":"response.audio.delta","response_id":"r1","delta":"AAAAAA=="}`)}); err == nil {
		t.Fatal("已终结响应的音频仍被接纳")
	}
}

func TestDSRealtimeRecorderOfflineServerCommitEvidence(t *testing.T) {
	for _, name := range []string{"automatic", "finish_only", "finish_before_done", "wrong_audio_id", "wrong_done_id", "empty_audio", "no_created", "done_before_audio"} {
		t.Run(name, func(t *testing.T) {
			r := dsAutomaticCommitTrace()
			switch name {
			case "finish_only":
				r.Records = dsRecordOrder(r.Records, 0, 1, 2, 3, 7, 4, 5, 6, 8, 9)
			case "finish_before_done":
				r.Records = dsRecordOrder(r.Records, 0, 1, 2, 3, 4, 5, 7, 6, 8, 9)
			case "wrong_audio_id":
				r.Records[5].Payload = bytes.ReplaceAll(r.Records[5].Payload, []byte(`"r1"`), []byte(`"wrong"`))
			case "wrong_done_id":
				r.Records[6].Payload = bytes.ReplaceAll(r.Records[6].Payload, []byte(`"r1"`), []byte(`"wrong"`))
			case "empty_audio":
				r.Records[5].Payload = []byte(`{"type":"response.audio.delta","response_id":"r1","delta":""}`)
			case "no_created":
				r.Records = dsRecordOrder(r.Records, 0, 1, 2, 3, 5, 6, 7, 8, 9)
			case "done_before_audio":
				r.Records = dsRecordOrder(r.Records, 0, 1, 2, 3, 4, 6, 5, 7, 8, 9)
			}
			f, err := dsCandidate(r)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, c := range f.Response.WS.Coverage {
				if c.Capability == "realtime_commit_modes" {
					found = true
				}
			}
			if found != (name == "automatic") {
				t.Fatal("自动提交Coverage没有要求finish前同响应的非空音频和终态")
			}
		})
	}
}

func dsAutomaticCommitTrace() dsRecording {
	r := dsSyntheticRecording()
	r.Scenario = "tts-server-commit"
	old := r.Records
	r.Records = dsRecordOrder(old, 0, 1, 2, 3)
	for i := range r.Records {
		r.Records[i].Payload = bytes.ReplaceAll(r.Records[i].Payload, []byte(`"mode":"commit"`), []byte(`"mode":"server_commit"`))
	}
	r.Records = append(r.Records, dsRecord{Direction: "receive", Kind: "message", Opcode: ws.OpText, Payload: []byte(`{"type":"response.created","response":{"id":"r1"}}`)})
	r.Records = append(r.Records, old[6], old[7], old[8], old[9], old[10])
	return r
}

func dsRecordOrder(records []dsRecord, indices ...int) []dsRecord {
	var out []dsRecord
	for _, i := range indices {
		out = append(out, records[i])
	}
	return out
}

func TestDSRealtimeRecorderOfflineServerCommitNoAutomaticResponse(t *testing.T) {
	finishedEarly := make(chan bool, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		c, err := ws.Accept(w, req, ws.AcceptOptions{MaxPayload: 1 << 20, Idle: time.Second, WriteTimeout: time.Second})
		if err != nil {
			finishedEarly <- false
			return
		}
		defer c.Close(1000, "")
		p := dsOfflinePeer{t: t, c: c}
		p.write(`{"type":"session.created","session":{"id":"s1"}}`)
		e := p.read("session.update")
		b, _ := json.Marshal(map[string]any{"type": "session.updated", "session": e["session"]})
		p.write(string(b))
		p.read("input_text_buffer.append")
		op, b, err := c.ReadMessage()
		var next map[string]any
		_ = json.Unmarshal(b, &next)
		early := err == nil && op == ws.OpText && dsString(next, "type") == "session.finish"
		finishedEarly <- early
		if early {
			p.write(`{"type":"response.created","response":{"id":"r1"}}`)
			p.write(`{"type":"response.audio.delta","response_id":"r1","delta":"AAAAAA=="}`)
			p.write(`{"type":"response.done","response":{"id":"r1","status":"completed"}}`)
			p.write(`{"type":"session.finished"}`)
		}
	}))
	defer server.Close()
	r := dsCapture(context.Background(), dsConfig{URL: "ws" + strings.TrimPrefix(server.URL, "http"), Model: "qwen3-tts-flash-realtime", Scenario: "tts-server-commit", Key: "offline-secret", Duration: 200 * time.Millisecond, Responses: 1})
	if <-finishedEarly || r.Failure == "" {
		t.Fatal("未观察自动响应就发送finish并冒充成功")
	}
	if _, err := dsCandidate(r); err == nil {
		t.Fatal("无自动提交证据仍生成成功候选")
	}
}
