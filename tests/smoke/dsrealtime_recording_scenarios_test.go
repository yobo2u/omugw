//go:build smoke

package smoke_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/testkit"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

// 本地 TCP 的手写脚本与真实服务无关，专门抓错场景事件、错字段和漏读异步 ASR。
type dsOfflinePeer struct {
	t   *testing.T
	c   *ws.Conn
	err error
}

func (p *dsOfflinePeer) read(kind string) map[string]any {
	p.t.Helper()
	if p.err != nil {
		return nil
	}
	op, b, err := p.c.ReadMessage()
	if err != nil {
		p.err = err
		return nil
	}
	var e map[string]any
	if op != ws.OpText || json.Unmarshal(b, &e) != nil || dsString(e, "type") != kind {
		p.err = fmt.Errorf("脚本期待 %s", kind)
	}
	return e
}
func (p *dsOfflinePeer) write(raw string) {
	p.t.Helper()
	if p.err == nil {
		p.err = p.c.WriteMessage(ws.OpText, []byte(raw))
	}
}
func (p *dsOfflinePeer) update(scenario string) {
	p.write(`{"type":"session.created","session":{"id":"s1","model":"qwen3.5-omni-flash-realtime"}}`)
	e := p.read("session.update")
	s := dsMap(e, "session")
	if s["max_tokens"] != float64(128) {
		p.t.Error("缺少128输出token硬限")
	}
	if scenario != "text-tools" && !dsEchoMatches(map[string]any{"format": map[string]any{"type": "pcm", "sample_rate": 16000}}, dsMap(s, "audio", "input")) {
		p.t.Error("输入格式/采样率错误")
	}
	if scenario == "text-tools" {
		tools, _ := s["tools"].([]any)
		if len(tools) != 2 {
			p.t.Error("缺少双工具定义")
		}
		for _, v := range tools {
			tool, _ := v.(map[string]any)
			if dsString(tool, "function", "name") == "" {
				p.t.Error("错用扁平OpenAI工具结构")
			}
		}
	}
	// 这里回显配置只驱动录制器的状态机；上述字面断言负责守请求合同。
	s["model"] = "qwen3.5-omni-flash-realtime"
	b, _ := json.Marshal(map[string]any{"type": "session.updated", "session": s})
	p.write(string(b))
}

func TestDSRealtimeRecorderOfflineOmniScenarios(t *testing.T) {
	for _, scenario := range []string{"text-tools", "audio-image", "vad-interrupt"} {
		t.Run(scenario, func(t *testing.T) {
			done := make(chan error, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				c, err := ws.Accept(w, req, ws.AcceptOptions{MaxPayload: 1 << 20, Idle: 2 * time.Second, WriteTimeout: time.Second})
				if err != nil {
					done <- err
					return
				}
				defer c.Close(1000, "")
				p := dsOfflinePeer{t: t, c: c}
				p.update(scenario)
				switch scenario {
				case "text-tools":
					dsOfflineTools(&p)
				case "audio-image":
					dsOfflineImage(&p)
				case "vad-interrupt":
					dsOfflineVAD(&p)
				}
				// 等真实客户端 close，防止主动关服务端掩盖录制器的关闭方向错误。
				if p.err == nil {
					_, _, _ = c.ReadMessage()
				}
				done <- p.err
			}))
			defer server.Close()
			cfg := dsConfig{URL: "ws" + strings.TrimPrefix(server.URL, "http"), Model: "qwen3.5-omni-flash-realtime", Scenario: scenario, Key: "offline-secret", Duration: 3 * time.Second, Responses: 1}
			if scenario == "text-tools" {
				cfg.Responses = 3
			} else {
				cfg.Sample = &dsAudioSample{Data: []byte{0, 1, 2, 3}, Rate: 16000, Format: "pcm_s16le"}
			}
			r := dsCapture(context.Background(), cfg)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if r.Failure != "" {
				t.Fatalf("场景失败: %s", r.Failure)
			}
			f, err := dsCandidate(r)
			if err != nil {
				t.Fatal(err)
			}
			if err := testkit.ValidateWSSession(f, testkit.DefaultWSLimits()); err != nil {
				t.Fatal(err)
			}
			dsReplayCandidate(t, f)
			if f.Response.WS.Outcome.Kind != "completed" && scenario != "vad-interrupt" {
				t.Fatal("未独立观察正常close应答")
			}
			var sent, received bool
			for _, rec := range r.Records {
				if rec.Kind == "close" {
					if rec.Direction == "send" {
						sent = true
					}
					if rec.Direction == "receive" {
						received = true
					}
				}
			}
			if !sent || !received {
				t.Fatal("缺少真实双向close轨迹")
			}
			caps := map[string]bool{}
			for _, c := range f.Response.WS.Coverage {
				caps[c.Capability] = true
			}
			if scenario == "text-tools" && (!caps["tool_calling"] || !caps["parallel_tool_calls"]) {
				t.Fatal("双调用缺少定位证据")
			}
			if scenario != "text-tools" && !caps["speech_recognition"] {
				t.Fatal("漏录晚于done的ASR")
			}
			if scenario == "vad-interrupt" && (!caps["realtime_server_vad"] || !caps["realtime_interrupt_turns"]) {
				t.Fatal("VAD/取消轨迹未关联")
			}
		})
	}
}

func dsReplayCandidate(t *testing.T, f testkit.Fixture) {
	t.Helper()
	limits := testkit.DefaultWSLimits()
	upstream, err := testkit.NewWSReplayUpstream(f, limits)
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	server := httptest.NewServer(upstream)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	client, _, err := ws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+f.Request.Path+"?"+f.Request.Query, ws.DialOptions{MaxPayload: limits.MessageBytes, Idle: time.Second, WriteTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close(1000, "")
	peer, err := upstream.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testkit.ReplayWS(ctx, f, testkit.WSReplayEndpoints{Client: client, Upstream: peer}, 1, limits); err != nil {
		t.Fatal(err)
	}
	if err := upstream.Close(); err != nil {
		t.Fatal(err)
	}
	if err := upstream.Err(); err != nil {
		t.Fatal(err)
	}
}

func dsOfflineTools(p *dsOfflinePeer) {
	e := p.read("conversation.item.create")
	if dsString(e, "item", "id") != "record_user_1" {
		p.t.Error("缺少第一轮记忆输入")
	}
	p.write(`{"type":"conversation.item.created","item":{"id":"record_user_1","type":"message","role":"user","content":[{"type":"input_text","text":"记住测试口令：蓝色方块。只回复已记住，不调用工具。"}]}}`)
	p.read("response.create")
	p.write(`{"type":"response.done","response":{"id":"r1","status":"completed","output":[],"usage":{"output_tokens":3}}}`)
	p.read("conversation.item.create")
	p.write(`{"type":"conversation.item.created","item":{"id":"record_user_2","type":"message","role":"user","content":[{"type":"input_text","text":"先复述刚才口令，再在同一轮调用 test_color 和 test_shape，不要猜测结果。"}]}}`)
	p.read("response.create")
	p.write(`{"type":"response.text.delta","response_id":"r2","delta":"蓝色方块"}`)
	p.write(`{"type":"response.done","response":{"id":"r2","status":"completed","output":[{"type":"function_call","name":"test_color","call_id":"call_a","arguments":"{}"},{"type":"function_call","name":"test_shape","call_id":"call_b","arguments":"{}"}],"usage":{"output_tokens":30}}}`)
	for _, v := range []struct{ name, id, result string }{{"test_color", "call_a", "蓝色"}, {"test_shape", "call_b", "方块"}} {
		e = p.read("conversation.item.create")
		if dsString(e, "item", "call_id") != v.id || dsString(e, "item", "output") != v.result {
			p.t.Error("工具结果关联错误")
		}
		p.write(`{"type":"conversation.item.created","item":{"id":"record_result_` + v.name + `","type":"function_call_output","call_id":"` + v.id + `","output":"` + v.result + `"}}`)
	}
	p.read("response.create")
	p.write(`{"type":"response.done","response":{"id":"r3","status":"completed","usage":{"output_tokens":5}}}`)
}

func dsOfflineImage(p *dsOfflinePeer) {
	e := p.read("input_audio_buffer.append")
	if dsString(e, "audio") != "AAECAw==" {
		p.t.Error("输入未引用给定PCM")
	}
	e = p.read("input_image_buffer.append")
	b, err := base64.StdEncoding.DecodeString(dsString(e, "image"))
	if err != nil || len(dsString(e, "image")) > 256*1024 {
		p.t.Error("图片超限或编码不正确")
	}
	im, err := jpeg.Decode(bytes.NewReader(b))
	if err != nil || im.Bounds().Dx() != 320 {
		p.t.Error("不是程序JPEG")
	}
	p.read("input_audio_buffer.commit")
	p.write(`{"type":"input_audio_buffer.committed","item_id":"in1"}`)
	p.read("response.create")
	p.write(`{"type":"response.text.delta","response_id":"r1","delta":"红色圆形和蓝色方块。"}`)
	p.write(`{"type":"response.done","response":{"id":"r1","status":"completed","usage":{"output_tokens":12}}}`)
	p.write(`{"type":"conversation.item.input_audio_transcription.completed","item_id":"in1","transcript":"请描述图片中的颜色和形状。"}`)
}

func dsOfflineVAD(p *dsOfflinePeer) {
	p.read("input_audio_buffer.append")
	p.write(`{"type":"input_audio_buffer.speech_started","item_id":"in1","audio_start_ms":0}`)
	p.write(`{"type":"input_audio_buffer.speech_stopped","item_id":"in1","audio_end_ms":100}`)
	p.write(`{"type":"input_audio_buffer.committed","item_id":"in1"}`)
	p.write(`{"type":"response.created","response":{"id":"r1"}}`)
	p.write(`{"type":"response.audio.delta","response_id":"r1","delta":"AAAAAA=="}`)
	p.read("response.cancel")
	p.write(`{"type":"response.done","response":{"id":"r1","status":"cancelled","usage":{"output_tokens":1}}}`)
	p.write(`{"type":"conversation.item.input_audio_transcription.completed","item_id":"in1","transcript":"请描述图片中的颜色和形状。"}`)
}

func TestDSRealtimeRecorderOfflineCoverageNeedsEvents(t *testing.T) {
	r := dsRecording{Scenario: "audio-image", Confirmed: true, Input: &dsAudioSample{}}
	c := dsCoverage(r, []dsEvidenceEvent{{Node: "n1", Value: map[string]any{"type": "session.updated"}}})
	if len(c) != 0 {
		t.Fatal("只有updated或输入样本就声明能力")
	}
	r.Scenario = "text-tools"
	e := map[string]any{"type": "response.done", "response": map[string]any{"id": "r1", "status": "completed", "output": []any{map[string]any{"type": "function_call", "call_id": "same"}, map[string]any{"type": "function_call", "call_id": "same"}}}}
	for _, c := range dsCoverage(r, []dsEvidenceEvent{{Node: "n1", Value: e}}) {
		if c.Capability == "parallel_tool_calls" {
			t.Fatal("重复call_id冒充双工具")
		}
	}
}
