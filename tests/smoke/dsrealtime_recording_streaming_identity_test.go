//go:build smoke

package smoke_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/testkit"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

// text-tools-v2 的真实 ack 原字节；后续轮次是独立手写脚本，不能冒充真实成功录制。
const dsObservedServerItem = `{"event_id":"event_Vu2wDlHEWjqFxkFf5CgXe","type":"conversation.item.created","item":{"id":"item_Ji44D7s85jVY5djzY5naZ","object":"realtime.item","type":"message","status":"completed","role":"user","content":[{"type":"input_text","text":"记住测试口令：蓝色方块。只回复已记住，不调用工具。"}]}}`

func TestDSRealtimeRecorderOfflineObservedStreamingFinish(t *testing.T) {
	for _, name := range []string{"streaming", "wrong_done_id", "finished_before_done", "no_done"} {
		t.Run(name, func(t *testing.T) {
			r := dsIdentityCapture(t, "tts-server-commit", func(p *dsOfflinePeer) {
				p.write(dsObservedTTSCreated)
				p.read("session.update")
				p.write(strings.Replace(dsObservedTTSUpdated, `"mode":"commit"`, `"mode":"server_commit"`, 1))
				p.read("input_text_buffer.append")
				// 来自实录的事件形状与同实体关系；ID简化，PCM缩短，finish之后的尾部纯合成。
				p.write(`{"type":"response.created","response":{"id":"r1","object":"realtime.response","conversation_id":"","status":"in_progress","voice":"Cherry","output":[]}}`)
				p.write(`{"type":"response.output_item.added","response_id":"r1","output_index":0,"item":{"id":"i1","object":"realtime.item","type":"message","status":"in_progress","role":"assistant","content":[]}}`)
				p.write(`{"type":"response.content_part.added","response_id":"r1","item_id":"i1","output_index":0,"content_index":0,"part":{"type":"audio","text":""}}`)
				p.write(`{"type":"response.audio.delta","response_id":"r1","delta":"AAAAAA=="}`)
				p.write(`{"type":"response.audio.delta","response_id":"r1","delta":"AQIDBA=="}`)
				p.read("session.finish")
				if name == "finished_before_done" || name == "no_done" {
					p.write(`{"type":"session.finished"}`)
				}
				if name != "no_done" {
					id := "r1"
					if name == "wrong_done_id" {
						id = "wrong"
					}
					p.write(`{"type":"response.done","response":{"id":"` + id + `","status":"completed"}}`)
					p.write(`{"type":"session.finished"}`)
				}
			})
			if name != "streaming" {
				if r.Failure == "" {
					t.Fatal("不完整或错ID终态被当成成功")
				}
				if _, err := dsCandidate(r); err == nil {
					t.Fatal("失败流产出成功candidate")
				}
				return
			}
			if r.Failure != "" || !bytes.Equal(r.Audio, []byte{0, 0, 0, 0, 1, 2, 3, 4}) {
				t.Fatalf("已自动启动的流没有经finish完成: failure=%s bytes=%d", r.Failure, len(r.Audio))
			}
			f, err := dsCandidate(r)
			if err != nil {
				t.Fatal(err)
			}
			dsReplayCandidate(t, f)
		})
	}
}

func TestDSRealtimeRecorderOfflineServerItemIdentity(t *testing.T) {
	for _, tc := range []struct {
		name               string
		stage              int
		from, to           string
		creates, responses int
	}{
		{"server_id", -1, "", "", 4, 3},
		{"content_metadata", 0, `"type":"input_text"`, `"type":"input_text","server_note":"accepted"`, 4, 3},
		{"wrong_content", 0, "蓝色方块", "红色圆形", 1, 0},
		{"wrong_role", 0, `"role":"user"`, `"role":"assistant"`, 1, 0},
		{"wrong_type", 0, `"type":"message"`, `"type":"function_call_output"`, 1, 0},
		{"missing_id", 0, `"id":"item_Ji44D7s85jVY5djzY5naZ",`, "", 1, 0},
		{"reused_id", 1, "server_user_2", "item_Ji44D7s85jVY5djzY5naZ", 2, 1},
		{"duplicate_ack", 0, "", "", 1, 1},
		{"wrong_call_id", 2, `"call_id":"call_a"`, `"call_id":"call_b"`, 3, 2},
		{"wrong_result_type", 2, "function_call_output", "message", 3, 2},
		{"wrong_output", 2, "蓝色", "红色", 3, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := dsIdentityCapture(t, "text-tools", func(p *dsOfflinePeer) {
				p.update("text-tools")
				dsOfflineToolsAck(p, func(stage int, raw string) string {
					if stage == tc.stage {
						if tc.name == "duplicate_ack" {
							p.write(raw)
							return raw
						}
						return strings.Replace(raw, tc.from, tc.to, 1)
					}
					return raw
				})
			})
			counts := map[string]int{}
			for _, rec := range r.Records {
				var e map[string]any
				_ = json.Unmarshal(rec.Payload, &e)
				if rec.Direction == "send" {
					counts[dsString(e, "type")]++
				}
			}
			if counts["conversation.item.create"] != tc.creates || counts["response.create"] != tc.responses {
				t.Fatalf("错误ack推进或重复提交: counts=%v", counts)
			}
			want := tc.responses == 3
			if (r.Failure == "") != want {
				t.Fatalf("failure=%s want success=%v", r.Failure, want)
			}
			if !want && r.Failure != "item_ack_mismatch_or_reused_id" {
				t.Fatalf("错误ack应当立即失败，不能等待超时: %s", r.Failure)
			}
			f, err := dsCandidate(r)
			if (err == nil) != want {
				t.Fatalf("candidate err=%v", err)
			}
			if want {
				dsReplayCandidate(t, f)
				bound := false
				rawAck := false
				for _, n := range f.Response.WS.Nodes {
					if n.Message == nil || !bytes.Contains(n.Message.Payload, []byte("item_Ji44D7s85jVY5djzY5naZ")) {
						continue
					}
					for _, rule := range n.Fields {
						if rule.Pointer == "/item/id" && rule.Namespace == "item" && n.Point == testkit.WSUpstreamSend && rule.Mode == "bind" {
							bound = true
						}
					}
					if bytes.Equal(n.Message.Payload, []byte(dsObservedServerItem)) {
						rawAck = true
					}
				}
				if !bound {
					t.Fatal("候选未绑定上游真实item ID")
				}
				if tc.name == "server_id" && !rawAck {
					t.Fatal("候选改写了真实ack字节")
				}
				if tc.name == "server_id" {
					for _, field := range []struct{ from, to string }{{`"call_id":"call_a"`, `"call_id":"wrong"`}, {`"output":"蓝色"`, `"output":"红色"`}, {`"type":"function_call_output"`, `"type":"message"`}} {
						bad := r
						bad.Records = append([]dsRecord(nil), r.Records...)
						for i, rec := range bad.Records {
							if rec.Direction == "receive" && bytes.Contains(rec.Payload, []byte(`"id":"server_result_test_color"`)) {
								bad.Records[i].Payload = bytes.Replace(rec.Payload, []byte(field.from), []byte(field.to), 1)
							}
						}
						if _, err := dsCandidate(bad); err == nil {
							t.Fatal("候选未校验工具结果ack的call_id/output/type")
						}
					}
				}
			}
		})
	}
}

func TestDSRealtimeRecorderOfflineItemWitnessPending(t *testing.T) {
	for _, name := range []string{"server_id", "explicit_id", "explicit_wrong_id", "wrong_content", "wrong_role", "wrong_type", "missing_id", "overlapping_pending", "reused_id", "duplicate_ack", "wrong_then_correct", "response_before_ack"} {
		t.Run(name, func(t *testing.T) {
			r := dsSyntheticRecording()
			r.Scenario = "text-tools"
			r.Records = r.Records[:3]
			for _, step := range []struct{ direction, raw string }{
				{"send", `{"type":"conversation.item.create","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"one"}]}}`},
				{"receive", `{"type":"conversation.item.created","item":{"id":"server1","type":"message","role":"user","content":[{"type":"input_text","text":"one"}]}}`},
				{"send", `{"type":"conversation.item.create","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"two"}]}}`},
				{"receive", `{"type":"conversation.item.created","item":{"id":"server2","type":"message","role":"user","content":[{"type":"input_text","text":"two"}]}}`},
				{"send", `{"type":"response.create"}`},
				{"receive", `{"type":"response.done","response":{"id":"r1","status":"completed"}}`},
			} {
				r.Records = append(r.Records, dsRecord{Direction: step.direction, Kind: "message", Opcode: ws.OpText, Payload: []byte(step.raw)})
			}
			r.Records = append(r.Records, dsRecord{Direction: "receive", Kind: "close", CloseCode: 1000})
			replace := func(i int, from, to string) {
				r.Records[i].Payload = bytes.Replace(r.Records[i].Payload, []byte(from), []byte(to), 1)
			}
			switch name {
			case "explicit_id":
				replace(3, `"item":{`, `"item":{"id":"server1",`)
			case "explicit_wrong_id":
				replace(3, `"item":{`, `"item":{"id":"author1",`)
			case "wrong_content":
				replace(4, "one", "wrong")
			case "wrong_role":
				replace(4, `"role":"user"`, `"role":"assistant"`)
			case "wrong_type":
				replace(4, `"type":"message"`, `"type":"function_call_output"`)
			case "missing_id":
				replace(4, `"id":"server1",`, "")
			case "overlapping_pending":
				r.Records = dsRecordOrder(r.Records, 0, 1, 2, 3, 5, 4, 6, 7, 8, 9)
			case "reused_id":
				replace(6, "server2", "server1")
			case "duplicate_ack":
				r.Records = dsRecordOrder(r.Records, 0, 1, 2, 3, 4, 4, 5, 6, 7, 8, 9)
			case "wrong_then_correct":
				r.Records = dsRecordOrder(r.Records, 0, 1, 2, 3, 4, 4, 5, 6, 7, 8, 9)
				replace(4, "one", "wrong")
			case "response_before_ack":
				r.Records = dsRecordOrder(r.Records, 0, 1, 2, 3, 7, 4, 5, 6, 8, 9)
			}
			want := name == "server_id" || name == "explicit_id"
			f, err := dsCandidate(r)
			if (err == nil) != want {
				t.Fatalf("candidate err=%v; want valid=%v", err, want)
			}
			if want {
				dsReplayCandidate(t, f)
			}
		})
	}
}

// 本地连接保留真正close握手；不向私有实录目录写入合成尾部。
func dsIdentityCapture(t *testing.T, scenario string, script func(*dsOfflinePeer)) dsRecording {
	t.Helper()
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		defer close(done)
		c, err := ws.Accept(w, req, ws.AcceptOptions{MaxPayload: 1 << 20, Idle: time.Second, WriteTimeout: time.Second})
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close(1000, "")
		p := dsOfflinePeer{t: t, c: c}
		script(&p)
		if p.err == nil {
			_, _, _ = c.ReadMessage()
		}
	}))
	defer server.Close()
	model, responses := "qwen3-tts-flash-realtime", 1
	if scenario == "text-tools" {
		model, responses = "qwen3.5-omni-flash-realtime", 3
	}
	r := dsCapture(context.Background(), dsConfig{URL: "ws" + strings.TrimPrefix(server.URL, "http"), Model: model, Scenario: scenario, Key: "offline-secret", Duration: time.Second, Responses: responses})
	<-done
	return r
}
