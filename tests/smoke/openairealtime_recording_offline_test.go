package smoke_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/testkit"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

// 字面上游脚本独立于录制器，不回显录制器生成的配置来构造期望。
const oaOfflineBase = `"id":"sess_1","object":"realtime.session","type":"realtime","model":"gpt-realtime-2.1"`
const oaOfflineTools = `[{"type":"function","name":"test_color","description":"返回公开测试颜色。","parameters":{"type":"object","properties":{},"additionalProperties":false}},{"type":"function","name":"test_shape","description":"返回公开测试形状。","parameters":{"type":"object","properties":{},"additionalProperties":false}}]`

type oaPeer struct {
	c      *ws.Conn
	err    error
	mutate func(string) string
	sent   []map[string]any
}

func (p *oaPeer) write(s string) {
	if p.err == nil {
		p.err = p.c.WriteMessage(ws.OpText, []byte(p.mutate(s)))
	}
}
func (p *oaPeer) read(kind string) map[string]any {
	if p.err != nil {
		return nil
	}
	op, b, err := p.c.ReadMessage()
	if err != nil {
		p.err = err
		return nil
	}
	var e map[string]any
	if op != ws.OpText || json.Unmarshal(b, &e) != nil || oaString(e, "type") != kind {
		p.err = errors.New("客户端序列不符")
	}
	p.sent = append(p.sent, e)
	return e
}
func (p *oaPeer) item(id, text string) {
	e := p.read("conversation.item.create")
	if !oaMatches(map[string]any{"id": id, "type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}}, oaMap(e, "item")) {
		p.err = errors.New("用户输入不符")
	}
	p.write(fmt.Sprintf(`{"type":"conversation.item.added","item":{"id":%q,"type":"message","role":"user","content":[{"type":"input_text","text":%q}]}}`, id, text))
}
func (p *oaPeer) start(id string) {
	p.write(fmt.Sprintf(`{"type":"response.created","response":{"id":%q,"status":"in_progress"}}`, id))
}
func (p *oaPeer) text(id, text string) {
	p.start(id)
	p.write(fmt.Sprintf(`{"type":"response.output_item.added","response_id":%q,"output_index":0,"item":{"id":"out_%s","type":"message","role":"assistant"}}`, id, id))
	p.write(fmt.Sprintf(`{"type":"response.content_part.added","response_id":%q,"item_id":"out_%s","output_index":0,"content_index":0,"part":{"type":"text","text":""}}`, id, id))
	p.write(fmt.Sprintf(`{"type":"response.output_text.delta","response_id":%q,"item_id":"out_%s","output_index":0,"content_index":0,"delta":%q}`, id, id, text))
	p.write(fmt.Sprintf(`{"type":"response.output_text.done","response_id":%q,"item_id":"out_%s","output_index":0,"content_index":0,"text":%q}`, id, id, text))
	p.write(fmt.Sprintf(`{"type":"response.content_part.done","response_id":%q,"item_id":"out_%s","output_index":0,"content_index":0,"part":{"type":"text","text":%q}}`, id, id, text))
	p.write(fmt.Sprintf(`{"type":"response.output_item.done","response_id":%q,"output_index":0,"item":{"id":"out_%s","type":"message","role":"assistant","content":[{"type":"output_text","text":%q}]},"event_id":"item_done_%s"}`, id, id, text, id))
	p.write(fmt.Sprintf(`{"type":"response.done","response":{"id":%q,"status":"completed","output":[{"id":"out_%s","type":"message","role":"assistant","content":[{"type":"output_text","text":%q}]}],"usage":{"input_tokens":5,"output_tokens":10,"total_tokens":15}}}`, id, id, text))
}

func oaOfflineScript(p *oaPeer, scenario string) {
	p.write(`{"type":"session.created","session":{` + oaOfflineBase + `}}`)
	e := p.read("session.update")
	var config string
	if scenario == "text-tools-vision" {
		config = `"output_modalities":["text"],"max_output_tokens":128,"instructions":"公开无隐私协议测试；简短回答。","reasoning":{"effort":"minimal"},"parallel_tool_calls":true,"tools":` + oaOfflineTools + `,"audio":{"input":{"turn_detection":null}}`
	} else {
		vad := `null`
		if scenario == "vad-interrupt" {
			vad = `{"type":"server_vad","threshold":0.5,"prefix_padding_ms":300,"silence_duration_ms":500,"create_response":true,"interrupt_response":false}`
		}
		config = `"output_modalities":["audio"],"max_output_tokens":128,"instructions":"公开测试，请从一数到二十。","audio":{"input":{"format":{"type":"audio/pcm","rate":24000},"transcription":{"model":"gpt-4o-mini-transcribe"},"turn_detection":` + vad + `},"output":{"format":{"type":"audio/pcm","rate":24000},"voice":"marin"}}`
	}
	var want map[string]any
	_ = json.Unmarshal([]byte(`{"type":"realtime",`+config+`}`), &want)
	if !oaMatches(want, oaMap(e, "session")) {
		p.err = errors.New("GA 请求配置不符")
	}
	p.write(`{"type":"session.updated","session":{` + oaOfflineBase + `,` + config + `}}`)
	if scenario == "text-tools-vision" {
		p.item("oa_memory", "记住公开口令：青色灯塔。只回复已记住。")
		p.read("response.create")
		p.text("r1", "已记住")
		p.item("oa_recall", "只复述刚才口令，不调用工具。")
		p.read("response.create")
		p.text("r2", "青色灯塔")
		p.item("oa_tools", "在同一轮调用 test_color 和 test_shape，各一次。")
		p.read("response.create")
		p.start("r3")
		p.write(`{"type":"response.output_item.added","response_id":"r3","output_index":0,"item":{"id":"fc1","type":"function_call","name":"test_color","call_id":"call1","arguments":""}}`)
		p.write(`{"type":"response.output_item.added","response_id":"r3","output_index":1,"item":{"id":"fc2","type":"function_call","name":"test_shape","call_id":"call2","arguments":""}}`)
		p.write(`{"type":"response.function_call_arguments.delta","event_id":"args_delta_1a","response_id":"r3","item_id":"fc1","output_index":0,"call_id":"call1","delta":"{"}`)
		p.write(`{"type":"response.function_call_arguments.delta","event_id":"args_delta_2","response_id":"r3","item_id":"fc2","output_index":1,"call_id":"call2","delta":"{}"}`)
		p.write(`{"type":"response.function_call_arguments.delta","event_id":"args_delta_1b","response_id":"r3","item_id":"fc1","output_index":0,"call_id":"call1","delta":"}"}`)
		p.write(`{"type":"response.function_call_arguments.done","event_id":"args_done_1","response_id":"r3","item_id":"fc1","output_index":0,"call_id":"call1","name":"test_color","arguments":"{}"}`)
		p.write(`{"type":"response.function_call_arguments.done","event_id":"args_done_2","response_id":"r3","item_id":"fc2","output_index":1,"call_id":"call2","name":"test_shape","arguments":"{}"}`)
		p.write(`{"type":"response.output_item.done","event_id":"tool_done_1","response_id":"r3","output_index":0,"item":{"id":"fc1","type":"function_call","name":"test_color","call_id":"call1","arguments":"{}"}}`)
		p.write(`{"type":"response.output_item.done","event_id":"tool_done_2","response_id":"r3","output_index":1,"item":{"id":"fc2","type":"function_call","name":"test_shape","call_id":"call2","arguments":"{}"}}`)
		p.write(`{"type":"response.done","response":{"id":"r3","status":"completed","output":[{"id":"fc1","type":"function_call","name":"test_color","call_id":"call1","arguments":"{}"},{"id":"fc2","type":"function_call","name":"test_shape","call_id":"call2","arguments":"{}"}],"usage":{"input_tokens":5,"output_tokens":20,"total_tokens":25}}}`)
		for i, v := range []string{"绿色", "三角形"} {
			e = p.read("conversation.item.create")
			if oaString(e, "item", "call_id") != fmt.Sprintf("call%d", i+1) || oaString(e, "item", "output") != v {
				p.err = errors.New("工具结果关联不符")
			}
			p.write(fmt.Sprintf(`{"type":"conversation.item.added","item":{"id":"oa_result_%d","type":"function_call_output","call_id":"call%d","output":%q}}`, i, i+1, v))
		}
		e = p.read("conversation.item.create")
		parts, _ := oaMap(e, "item")["content"].([]any)
		if len(parts) != 2 {
			p.err = errors.New("缺少图片")
		}
		// 只复制已单独核验的图像字节；服务端业务回答仍为独立字面证据。
		if len(parts) == 2 {
			im, _ := parts[1].(map[string]any)
			if oaString(im, "detail") != "high" || !strings.HasPrefix(oaString(im, "image_url"), "data:image/png;base64,") {
				p.err = errors.New("图片类型/detail不符")
			}
		}
		b, _ := json.Marshal(map[string]any{"type": "conversation.item.added", "item": oaMap(e, "item")})
		p.write(string(b))
		p.read("response.create")
		p.text("r4", "红色圆形和蓝色方块")
		return
	}
	for i := 0; i < 2; i++ {
		e = p.read("input_audio_buffer.append")
		if oaString(e, "audio") == "" {
			p.err = errors.New("空音频")
		}
	}
	if scenario == "audio-manual" {
		p.read("input_audio_buffer.commit")
	} else {
		p.write(`{"type":"input_audio_buffer.speech_started","item_id":"input1","audio_start_ms":0}`)
		p.write(`{"type":"input_audio_buffer.speech_stopped","item_id":"input1","audio_end_ms":100}`)
	}
	p.write(`{"type":"input_audio_buffer.committed","item_id":"input1"}`)
	p.write(`{"type":"conversation.item.added","item":{"id":"input1","type":"message","role":"user","content":[{"type":"input_audio","transcript":null}]}}`)
	p.write(`{"type":"conversation.item.done","item":{"id":"input1","type":"message","role":"user","content":[{"type":"input_audio","transcript":null}]}}`)
	if scenario == "audio-manual" {
		p.read("response.create")
	}
	p.start("r1")
	p.write(`{"type":"conversation.item.added","item":{"id":"audio1","type":"message","role":"assistant","content":[]}}`)
	p.write(`{"type":"response.output_item.added","response_id":"r1","output_index":0,"item":{"id":"audio1","type":"message","role":"assistant"}}`)
	p.write(`{"type":"response.content_part.added","response_id":"r1","item_id":"audio1","output_index":0,"content_index":0,"part":{"type":"audio","transcript":""}}`)
	p.write(`{"type":"response.output_audio.delta","response_id":"r1","item_id":"audio1","output_index":0,"content_index":0,"delta":"AAECAw=="}`)
	status := "completed"
	if scenario == "vad-interrupt" {
		e = p.read("response.cancel")
		if oaString(e, "response_id") != "r1" {
			p.err = errors.New("取消目标不符")
		}
		status = "cancelled"
	}
	p.write(`{"type":"conversation.item.done","item":{"id":"audio1","type":"message","role":"assistant","content":[{"type":"output_audio","transcript":"一二三"}]}}`)
	p.write(fmt.Sprintf(`{"type":"response.done","response":{"id":"r1","status":%q,"output":[{"id":"audio1","type":"message","role":"assistant","content":[{"type":"output_audio","transcript":"一二三"}]}],"usage":{"input_tokens":5,"output_tokens":10,"total_tokens":15}}}`, status))
	if scenario == "vad-interrupt" {
		e = p.read("conversation.item.truncate")
		// ID 冲突反例也同步脚本的截断坐标，防止仅由 peer 拒绝请求而伪绿。
		var expected map[string]any
		_ = json.Unmarshal([]byte(p.mutate(`{"item_id":"audio1","content_index":0,"audio_end_ms":0}`)), &expected)
		if !oaMatches(expected, e) {
			p.err = errors.New("截断实体不符")
		}
		p.write(`{"type":"conversation.item.truncated","item_id":"audio1","content_index":0,"audio_end_ms":0}`)
	}
	// 转写故意晚于生成与截断，防止 response.done 提前结束会话。
	p.write(`{"type":"conversation.item.input_audio_transcription.completed","item_id":"input1","content_index":0,"transcript":"公开测试。","usage":{"type":"duration","seconds":0.1}}`)
}

func oaRunPeer(t *testing.T, scenario string, mutate func(string) string, noClose bool) (oaRecording, []map[string]any) {
	t.Helper()
	done := make(chan *oaPeer, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := ws.Accept(w, r, ws.AcceptOptions{MaxPayload: oaMaxBytes, Idle: 3 * time.Second, WriteTimeout: time.Second})
		if err != nil {
			done <- &oaPeer{err: err}
			return
		}
		defer c.Close(1000, "")
		p := &oaPeer{c: c, mutate: mutate}
		oaOfflineScript(p, scenario)
		if noClose {
			time.Sleep(1200 * time.Millisecond)
		} else if p.err == nil {
			_, _, _ = c.ReadMessage()
		}
		done <- p
	}))
	defer srv.Close()
	cfg := oaConfig{URL: "ws" + strings.TrimPrefix(srv.URL, "http"), Model: oaModel, Scenario: scenario, Key: "offline-secret", Duration: 3 * time.Second}
	if scenario != "text-tools-vision" {
		data := make([]byte, 4800)
		cfg.Sample = &oaSample{Data: data, SHA256: oaDigest(data), Format: "pcm_s16le", Rate: 24000, Channels: 1, Transcript: "公开测试。", Public: true, Origin: "https://example.org/public-audio", License: "CC0-1.0", File: "public.pcm"}
	}
	r := oaCapture(context.Background(), cfg)
	p := <-done
	if r.Failure == "" && p.err != nil {
		t.Fatalf("脚本错误: %v", p.err)
	}
	return r, p.sent
}

func TestOpenAIRealtimeRecorderOfflineScenarios(t *testing.T) {
	for _, s := range []string{"text-tools-vision", "audio-manual", "vad-interrupt"} {
		t.Run(s, func(t *testing.T) {
			r, _ := oaRunPeer(t, s, func(s string) string { return s }, false)
			if r.Failure != "" {
				t.Fatalf("录制失败: %s", r.Failure)
			}
			f, err := oaCandidate(r)
			if err != nil {
				t.Fatal(err)
			}
			if f.Response.WS.Provenance.Kind != "synthetic-negative" {
				t.Fatal("本地脚本冒充实录")
			}
			for _, c := range f.Response.WS.Coverage {
				if c.Capability == "audio_input" || c.Capability == "audio_output" || c.Capability == "speech_synthesis" || c.Capability == "vision_input" || c.Capability == "image_detail" {
					t.Fatal("未核验内容冒充能力")
				}
			}
			dir := t.TempDir()
			if err := oaSave(dir, r); err != nil {
				t.Fatal(err)
			}
			loaded, err := testkit.ReadWSFixture(filepath.Join(dir, "candidate.json"), testkit.DefaultWSLimits())
			if err != nil {
				t.Fatal(err)
			}
			oaReplay(t, loaded)
			// 候选必须重新核验原始业务，不能相信可被改写的 confirmed 标志。
			r.Records = r.Records[:len(r.Records)-1]
			if _, err := oaCandidate(r); err == nil {
				t.Fatal("无对侧 close 仍交付候选")
			}
		})
	}
}

func TestOpenAIRealtimeRecorderOfflineNegativeScripts(t *testing.T) {
	for _, tc := range []struct {
		name, scenario, old, new string
		noClose                  bool
	}{
		{"lost_config", "text-tools-vision", `"max_output_tokens":128`, `"omitted":128`, false},
		{"wrong_session", "text-tools-vision", `"type":"session.updated","session":{"id":"sess_1"`, `"type":"session.updated","session":{"id":"sess_other"`, false},
		{"wrong_model", "text-tools-vision", `"model":"gpt-realtime-2.1"`, `"model":"different-model"`, false},
		{"wrong_rate", "audio-manual", `"rate":24000`, `"rate":16000`, false},
		{"invalid_vad_times", "vad-interrupt", `"audio_end_ms":100`, `"audio_end_ms":-1`, false},
		{"missing_asr_config", "audio-manual", `"transcription":{"model":"gpt-4o-mini-transcribe"}`, `"transcription":null`, false},
		{"wrong_tool_id", "text-tools-vision", `"call_id":"call2"`, `"call_id":"call1"`, false},
		{"wrong_tool_name", "text-tools-vision", `"name":"test_shape","call_id"`, `"name":"unknown","call_id"`, false},
		{"no_memory", "text-tools-vision", `"delta":"青色灯塔"`, `"delta":"没记住"`, false},
		{"wrong_part", "audio-manual", `"content_index":0,"delta"`, `"content_index":1,"delta"`, false},
		{"wrong_output_item", "audio-manual", `"item_id":"audio1","output_index":0,"content_index":0,"delta"`, `"item_id":"other","output_index":0,"content_index":0,"delta"`, false},
		{"wrong_done_item", "text-tools-vision", `"type":"response.output_item.done","response_id":"r1","output_index":0,"item":{"id":"out_r1"`, `"type":"response.output_item.done","response_id":"r1","output_index":0,"item":{"id":"other"`, false},
		{"wrong_asr_item", "audio-manual", `"item_id":"input1","content_index":0,"transcript"`, `"item_id":"other","content_index":0,"transcript"`, false},
		{"wrong_asr_part", "audio-manual", `"content_index":0,"transcript":"公开测试。"`, `"content_index":1,"transcript":"公开测试。"`, false},
		{"not_cancelled", "vad-interrupt", `"status":"cancelled"`, `"status":"completed"`, false},
		{"wrong_truncate", "vad-interrupt", `"type":"conversation.item.truncated","item_id":"audio1","content_index":0`, `"type":"conversation.item.truncated","item_id":"audio1","content_index":1`, false},
		{"finish_only", "audio-manual", `"type":"response.done"`, `"type":"session.finished"`, false},
		{"secret_escaped", "text-tools-vision", `"type":"session.created"`, `"message":"offline\u002dsecret","type":"session.created"`, false},
		{"no_close", "text-tools-vision", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, sent := oaRunPeer(t, tc.scenario, func(s string) string {
				if tc.old == "" {
					return s
				}
				return strings.ReplaceAll(s, tc.old, tc.new)
			}, tc.noClose)
			if r.Failure == "" {
				t.Fatal("负例没有失败")
			}
			if _, err := oaCandidate(r); err == nil {
				t.Fatal("失败轨迹变成候选")
			}
			if strings.HasPrefix(tc.name, "wrong_tool") {
				for _, s := range sent {
					if oaString(s, "item", "type") == "function_call_output" {
						t.Fatal("校验完整工具集合前发送结果")
					}
				}
			}
			dir := t.TempDir()
			_ = oaSave(dir, r)
			if _, err := os.Stat(filepath.Join(dir, "recording.json")); err != nil {
				t.Fatal("失败未落盘")
			}
			if _, err := os.Stat(filepath.Join(dir, "candidate.json")); !os.IsNotExist(err) {
				t.Fatal("失败生成候选")
			}
			b, _ := os.ReadFile(filepath.Join(dir, "recording.json"))
			var saved oaRecording
			if json.Unmarshal(b, &saved) != nil {
				t.Fatal("失败轨迹无法读取")
			}
			for _, rec := range saved.Records {
				if strings.Contains(string(rec.Payload), "offline") {
					t.Fatal("秘密进入磁盘")
				}
			}
		})
	}
}

func oaReplay(t *testing.T, f testkit.Fixture) {
	t.Helper()
	l := testkit.DefaultWSLimits()
	up, err := testkit.NewWSReplayUpstream(f, l)
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	srv := httptest.NewServer(up)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	c, _, err := ws.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+f.Request.Path+"?"+f.Request.Query, ws.DialOptions{MaxPayload: l.MessageBytes, Idle: time.Second, WriteTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(1000, "")
	p, err := up.Connection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testkit.ReplayWS(ctx, f, testkit.WSReplayEndpoints{Client: c, Upstream: p}, 1, l); err != nil {
		t.Fatal(err)
	}
	if err := up.Close(); err != nil {
		t.Fatal(err)
	}
	if err := up.Err(); err != nil {
		t.Fatal(err)
	}
}
