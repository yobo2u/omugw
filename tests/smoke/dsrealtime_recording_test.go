//go:build smoke

package smoke_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/testkit"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

func TestDSRealtimeRecorderOfflineDefaultsAndLimits(t *testing.T) {
	root := t.TempDir()
	if _, enabled, err := dsRecordConfig(root, func(string) string { return "" }); enabled || err != nil {
		t.Fatal("默认必须跳过且不读取凭据")
	}
	env := map[string]string{
		"OMUGW_RECORD_DSREALTIME": "1", "OMUGW_RECORD_SCENARIO": "tts-commit",
		"OMUGW_RECORD_OUTPUT":        filepath.Join(root, ".local/recordings/dsrealtime/batch/run"),
		"OMUGW_SMOKE_WS_URL":         "wss://dashscope.aliyuncs.com/api-ws/v1/realtime",
		"OMUGW_SMOKE_MODEL_REALTIME": "qwen3-tts-flash-realtime", "DASHSCOPE_API_KEY": "offline-secret",
	}
	get := func(k string) string { return env[k] }
	c, enabled, err := dsRecordConfig(root, get)
	if err != nil || !enabled || c.Duration > 60*time.Second || c.Responses > 3 {
		t.Fatalf("显式场景配置失败: %v", err)
	}
	for _, tc := range []struct{ key, value string }{
		{"OMUGW_RECORD_SCENARIO", "all"},
		{"OMUGW_RECORD_OUTPUT", filepath.Join(root, "testdata/routes/run")},
		{"OMUGW_SMOKE_WS_URL", "wss://dashscope.aliyuncs.com/api-ws/v1/realtime?api_key=offline-secret"},
		{"OMUGW_SMOKE_WS_URL", "wss://evil.invalid/api-ws/v1/realtime"},
		{"OMUGW_SMOKE_MODEL_REALTIME", "qwen3-tts-flash-realtime&key=offline-secret"},
		{"DASHSCOPE_API_KEY", "key\r\ninjected: secret"},
	} {
		old := env[tc.key]
		env[tc.key] = tc.value
		_, _, err := dsRecordConfig(root, get)
		if err == nil || strings.Contains(err.Error(), "offline-secret") {
			t.Fatalf("未安全拒绝 %s", tc.key)
		}
		env[tc.key] = old
	}
}

func TestDSRealtimeRecorderOfflineExclusiveBatch(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 8; i++ {
		out := filepath.Join(root, "batch", string(rune('a'+i)))
		if err := dsReserveOutput(root, out); err != nil {
			t.Fatal(err)
		}
		if err := dsReserveOutput(root, out); err == nil {
			t.Fatal("覆盖已有候选目录")
		}
		if err := dsWriteExclusive(out, "sentinel.json", []byte(`{"ok":true}`)); err != nil {
			t.Fatal(err)
		}
		if err := dsWriteExclusive(out, "sentinel.json", []byte(`{}`)); err == nil {
			t.Fatal("覆盖已有文件")
		}
		info, _ := os.Stat(filepath.Join(out, "sentinel.json"))
		if info.Mode().Perm() != 0600 {
			t.Fatal("候选不是私有文件")
		}
	}
	if err := dsReserveOutput(root, filepath.Join(root, "batch", "ninth")); err == nil {
		t.Fatal("第九次录制未拦截")
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := dsReserveOutput(root, filepath.Join(root, "link", "run")); err == nil {
		t.Fatal("输出跟随符号链接")
	}
	linkedRoot := filepath.Join(t.TempDir(), "root")
	if err := os.Symlink(t.TempDir(), linkedRoot); err != nil {
		t.Fatal(err)
	}
	if err := dsReserveOutput(linkedRoot, filepath.Join(linkedRoot, "batch", "run")); err == nil {
		t.Fatal("录制根链接可绕过 ignored 目录边界")
	}
}

func TestDSRealtimeRecorderOfflinePrivacy(t *testing.T) {
	h := dsSafeHeaders(http.Header{
		"Upgrade": {"websocket"}, "Connection": {"Upgrade"}, "Authorization": {"Bearer offline-secret"},
		"Set-Cookie": {"offline-secret"}, "X-Request-Id": {"offline-secret"}, "Server": {"offline-secret"},
	})
	b, _ := json.Marshal(h)
	if strings.Contains(string(b), "offline-secret") || len(h) != 2 {
		t.Fatal("握手头未采用白名单")
	}
	for _, p := range []string{`{"type":"error","message":"offline-secret"}`, `{"type":"error","api_key":"other"}`, `{"type":"error","message":"offline\u002dsecret"}`, `{"type":"error","message":"offline\u002dsecret","message":"public"}`} {
		if dsSafePayload([]byte(p), "offline-secret") {
			t.Fatal("凭据进入轨迹")
		}
	}
}

func TestDSRealtimeRecorderOfflineCandidateCausality(t *testing.T) {
	r := dsSyntheticRecording()
	f, err := dsCandidate(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := testkit.ValidateWSSession(f, testkit.DefaultWSLimits()); err != nil {
		t.Fatal(err)
	}
	s := f.Response.WS
	matcher, err := testkit.NewWSMatcher(testkit.DefaultWSLimits())
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]testkit.WSNode{}
	for _, n := range s.Nodes {
		if n.Message != nil {
			if err := matcher.Match(n, *n.Message); err != nil {
				t.Fatal(err)
			}
			if n.ForwardedFrom != "" {
				from := byID[n.ForwardedFrom]
				if err := testkit.AssertWSForwardedPayload(*from.Message, *n.Message, nil); err != nil {
					t.Fatal(err)
				}
				changed := testkit.WSMessage{Opcode: ws.OpBinary, Payload: n.Message.Payload}
				if err := testkit.AssertWSForwardedPayload(*from.Message, changed, nil); err == nil {
					t.Fatal("opcode 错配被放过")
				}
			}
		}
		byID[n.ID] = n
	}
	if s.Outcome.Kind != "completed" || len(s.Outcome.Terminal) != 1 {
		t.Fatal("真实终态未关联")
	}
	for _, n := range s.Nodes {
		want := map[testkit.WSPoint]string{testkit.WSClientSend: "authored", testkit.WSUpstreamReceive: "upstream-accepted", testkit.WSUpstreamSend: "recorded", testkit.WSClientReceive: "golden"}[n.Point]
		if n.Source != want {
			t.Fatal("来源角色错配")
		}
	}
	var request, accepted *testkit.WSNode
	for i := range s.Nodes {
		n := &s.Nodes[i]
		if n.Message != nil && bytes.Contains(n.Message.Payload, []byte(`"session.update"`)) {
			if n.Point == testkit.WSClientSend {
				request = n
			}
			if n.Point == testkit.WSUpstreamReceive {
				accepted = n
			}
		}
	}
	if request == nil || accepted == nil || accepted.ForwardedFrom != request.ID || !bytes.Equal(request.Message.Payload, accepted.Message.Payload) {
		t.Fatal("独立请求字节/来源未保全")
	}
	path := filepath.Join(t.TempDir(), "candidate.json")
	b, _ := json.Marshal(f)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := testkit.ReadWSFixture(path, testkit.DefaultWSLimits()); err != nil {
		t.Fatal(err)
	}
	s.Nodes[0].Message.Payload = []byte(`{"type":"changed"}`)
	if err := testkit.ValidateWSSession(f, testkit.DefaultWSLimits()); err == nil {
		t.Fatal("摘要没抓住篡改")
	}
}

func TestDSRealtimeRecorderOfflineNoInventedTerminal(t *testing.T) {
	for _, kind := range []string{"raw_eof", "timeout", "upstream_error", "unconfirmed_config"} {
		r := dsSyntheticRecording()
		r.Failure = kind
		r.Records = r.Records[:3]
		if _, err := dsCandidate(r); err == nil {
			t.Fatalf("%s 被伪造成成功 fixture", kind)
		}
	}
	r := dsSyntheticRecording()
	r.Records = r.Records[:len(r.Records)-1]
	r.Records = append(r.Records, dsRecord{Direction: "send", Kind: "close", CloseCode: 1000})
	if _, err := dsCandidate(r); err == nil {
		t.Fatal("本地 close 写成功不能交付缺少对侧接收的回放候选")
	}
	r.Records = r.Records[:len(r.Records)-1]
	if _, err := dsCandidate(r); err == nil {
		t.Fatal("raw EOF 伪装有效 close")
	}
}

// 手写的上游只验证录制器，不作为真实来源提交或投放。
func dsSyntheticRecording() dsRecording {
	r := dsRecording{Scenario: "tts-commit", Model: "qwen3-tts-flash-realtime", Started: time.Now().UTC(), Confirmed: true}
	for _, v := range []struct{ direction, body string }{
		{"receive", `{"type":"session.created","event_id":"e1","session":{"id":"s1","model":"qwen3-tts-flash-realtime"}}`},
		{"send", `{"type":"session.update","event_id":"c1","session":{"mode":"commit","voice":"Cherry","response_format":"pcm","sample_rate":16000}}`},
		{"receive", `{"type":"session.updated","event_id":"e2","session":{"id":"s1","mode":"commit","voice":"Cherry","response_format":"pcm","sample_rate":16000}}`},
		{"send", `{"type":"input_text_buffer.append","event_id":"c2","text":"请描述图片中的颜色和形状。"}`},
		{"send", `{"type":"input_text_buffer.commit","event_id":"c3"}`},
		{"receive", `{"type":"input_text_buffer.committed","event_id":"e3","item_id":"i1"}`},
		{"receive", `{"type":"response.audio.delta","event_id":"e4","response_id":"r1","delta":"AAAAAA=="}`},
		{"receive", `{"type":"response.done","event_id":"e5","response":{"id":"r1","status":"completed","usage":{"characters":14}}}`},
		{"send", `{"type":"session.finish","event_id":"c4"}`},
		{"receive", `{"type":"session.finished","event_id":"e6"}`},
	} {
		r.Records = append(r.Records, dsRecord{Direction: v.direction, Kind: "message", Opcode: ws.OpText, Payload: []byte(v.body)})
	}
	r.Records = append(r.Records, dsRecord{Direction: "receive", Kind: "close", CloseCode: 1000})
	return r
}

func TestDSRealtimeRecorderOfflineSingleDialAndDeadline(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		c, err := ws.Accept(w, req, ws.AcceptOptions{MaxPayload: 1 << 20, WriteTimeout: time.Second})
		if err != nil {
			return
		}
		defer c.Close(1000, "")
		_, _, _ = c.ReadMessage()
	}))
	defer server.Close()
	c := dsConfig{URL: "ws" + strings.TrimPrefix(server.URL, "http"), Model: "offline", Scenario: "tts-commit", Key: "offline-secret", Duration: 40 * time.Millisecond, Responses: 1}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	r := dsCapture(ctx, c)
	if calls.Load() != 1 || r.Failure == "" || time.Since(start) > time.Second {
		t.Fatal("会话没有单次拨号/硬期限")
	}
	if _, err := dsCandidate(r); err == nil {
		t.Fatal("超时轨迹产出了可用候选")
	}
}

func TestDSRealtimeRecorderOfflineMediaBounds(t *testing.T) {
	if _, err := dsLoadSample(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Fatal("不存在样本也能录音")
	}
	r := dsSyntheticRecording()
	r.Audio = make([]byte, 16000*2*9)
	if err := dsSaveAudio(t.TempDir(), r); err == nil {
		t.Fatal("超过八秒仍导出语音样本")
	}
	r.Audio = []byte{0, 1, 2, 3}
	dir := t.TempDir()
	if err := dsSaveAudio(dir, r); err != nil {
		t.Fatal(err)
	}
	s, err := dsLoadSample(filepath.Join(dir, "audio-sample.json"))
	if err != nil || s.Rate != 16000 || s.Format != "pcm_s16le" || !bytes.Equal(s.Data, r.Audio) {
		t.Fatal("样本格式/采样率/文件丢失")
	}
	if err := os.WriteFile(filepath.Join(dir, "audio.pcm"), []byte{3, 2, 1, 0}, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := dsLoadSample(filepath.Join(dir, "audio-sample.json")); err == nil {
		t.Fatal("篡改PCM未被拒绝")
	}
}

func TestDSRealtimeRecorderOfflineRejectsUnconfirmedRateAndLimits(t *testing.T) {
	for _, tc := range []struct {
		name, event string
		count       int
		wantError   bool
	}{
		{"first", `{"type":"response.created","response":{"id":"r1"}}`, 0, false},
		{"second", `{"type":"response.created","response":{"id":"r2"}}`, 1, true},
		{"token_overrun", `{"type":"response.done","response":{"id":"r1","status":"completed","usage":{"output_tokens":129}}}`, 0, true},
		{"empty_asr", `{"type":"conversation.item.input_audio_transcription.completed","item_id":"i1","transcript":""}`, 0, true},
		{"failed_asr", `{"type":"conversation.item.input_audio_transcription.failed","item_id":"i1"}`, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := dsDriver{cfg: dsConfig{Responses: 1, Scenario: "audio-image"}, r: &dsRecording{}, responses: map[string]bool{}, responseDone: map[string]bool{}}
			if tc.count > 0 {
				d.responses["r1"] = true
			}
			_, err := d.observe(dsIncoming{op: ws.OpText, payload: []byte(tc.event)})
			if (err != nil) != tc.wantError {
				t.Fatalf("上限/失败未反映到录制结果: %v", err)
			}
		})
	}
	want := map[string]any{"audio": map[string]any{"input": map[string]any{"format": map[string]any{"type": "pcm", "sample_rate": 16000}}}}
	if dsEchoMatches(want, map[string]any{"input_audio_format": "pcm16"}) {
		t.Fatal("legacy 回显冒充新式采样率采纳")
	}
	d := dsDriver{cfg: dsConfig{Responses: 1}, r: &dsRecording{}, responses: map[string]bool{}, responseDone: map[string]bool{}, traceBytes: dsMaxTrace}
	if _, err := d.observe(dsIncoming{op: ws.OpText, payload: []byte(`{"type":"session.created"}`)}); err == nil {
		t.Fatal("轨迹超限未停止")
	}
	d.traceBytes = 0
	d.r.Records = make([]dsRecord, dsMaxRecords)
	if _, err := d.observe(dsIncoming{op: ws.OpText, payload: []byte(`{"type":"session.created"}`)}); err == nil {
		t.Fatal("消息次数超限未停止")
	}
}

func TestDSRealtimeRecorderOfflineTTSConversation(t *testing.T) {
	for _, scenario := range []string{"tts-commit", "tts-server-commit"} {
		t.Run(scenario, func(t *testing.T) {
			serverDone := make(chan error, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				c, err := ws.Accept(w, req, ws.AcceptOptions{MaxPayload: 1 << 20, Idle: time.Second, WriteTimeout: time.Second})
				if err != nil {
					serverDone <- err
					return
				}
				defer c.Close(1000, "")
				write := func(b string) {
					if err == nil {
						err = c.WriteMessage(ws.OpText, []byte(b))
					}
				}
				read := func(kind string) map[string]any {
					op, b, e := c.ReadMessage()
					if e != nil {
						err = e
						return nil
					}
					var v map[string]any
					if json.Unmarshal(b, &v) != nil || op != ws.OpText || dsString(v, "type") != kind {
						t.Error("录制器发错事件")
						return nil
					}
					return v
				}
				write(`{"type":"session.created","session":{"id":"s1","model":"qwen3-tts-flash-realtime"}}`)
				v := read("session.update")
				mode := "commit"
				if scenario == "tts-server-commit" {
					mode = "server_commit"
				}
				if dsString(v, "session", "mode") != mode || dsString(v, "session", "voice") != "Cherry" {
					t.Error("TTS 配置错误")
				}
				write(`{"type":"session.updated","session":{"id":"s1","mode":"` + mode + `","voice":"Cherry","language_type":"Chinese","response_format":"pcm","sample_rate":16000}}`)
				v = read("input_text_buffer.append")
				if dsString(v, "text") != "请描述图片中的颜色和形状。" {
					t.Error("上传非固定公开短句")
				}
				if scenario == "tts-commit" {
					read("input_text_buffer.commit")
					write(`{"type":"input_text_buffer.committed","item_id":"i1"}`)
				} else {
					read("session.finish")
				}
				write(`{"type":"response.created","response":{"id":"r1"}}`)
				write(`{"type":"response.audio.delta","response_id":"r1","delta":"AAAAAA=="}`)
				write(`{"type":"response.done","response":{"id":"r1","status":"completed","usage":{"characters":14}}}`)
				if scenario == "tts-commit" {
					read("session.finish")
				}
				write(`{"type":"session.finished"}`)
				if err == nil {
					err = c.Close(1000, "")
				}
				serverDone <- err
			}))
			defer server.Close()
			r := dsCapture(context.Background(), dsConfig{URL: "ws" + strings.TrimPrefix(server.URL, "http"), Model: "qwen3-tts-flash-realtime", Scenario: scenario, Key: "offline-secret", Duration: 2 * time.Second, Responses: 1})
			if err := <-serverDone; err != nil {
				t.Fatal(err)
			}
			if r.Failure != "" || !r.Confirmed || len(r.Audio) != 4 {
				t.Fatalf("TTS 未完成: %s", r.Failure)
			}
			f, err := dsCandidate(r)
			if err != nil {
				t.Fatal(err)
			}
			if f.Response.WS.Outcome.Kind != "completed" {
				t.Fatal("漏掉真实上游 close")
			}
			dsReplayCandidate(t, f)
		})
	}
}
