package smoke_test

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
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

func TestOpenAIRealtimeRecorderOfflineRawBudget(t *testing.T) {
	// 512 KiB 单条放两次恰好 1 MiB，派生副本不得提前抢占这份预算。
	b := []byte(`{"padding":"` + strings.Repeat("a", (1<<19)-14) + `"}`)
	if len(b) != 1<<19 {
		t.Fatal("字面预算长度错误")
	}
	r := oaRecording{}
	for _, direction := range []string{"send", "receive"} {
		if err := r.add(oaRecord{Direction: direction, Kind: "message", Opcode: ws.OpText, Payload: b}, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.add(oaRecord{Direction: "send", Kind: "close", Code: 1000}, ""); err == nil {
		t.Fatal("close 字节未计入总轨迹")
	}
	r = oaRecording{}
	for i := 0; i < 500; i++ {
		if err := r.add(oaRecord{Kind: "message", Opcode: ws.OpText, Payload: []byte(`{}`)}, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.add(oaRecord{Kind: "message", Opcode: ws.OpText, Payload: []byte(`{}`)}, ""); err == nil {
		t.Fatal("第501条未拒绝")
	}
	if len(r.Records) != 500 {
		t.Fatal("超预算记录仍入账")
	}
}

func TestOpenAIRealtimeRecorderOfflineSingleDialDeadlineAndHeaders(t *testing.T) {
	var calls atomic.Int32
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		defer close(done)
		c, err := ws.Accept(w, r, ws.AcceptOptions{MaxPayload: oaMaxBytes, Idle: time.Second, WriteTimeout: time.Second})
		if err != nil {
			return
		}
		defer c.Close(1000, "")
		_, _, _ = c.ReadMessage()
	}))
	defer srv.Close()
	start := time.Now()
	r := oaCapture(context.Background(), oaConfig{URL: "ws" + strings.TrimPrefix(srv.URL, "http"), Model: oaModel, Scenario: "text-tools-vision", Key: "offline-secret", Duration: 1100 * time.Millisecond})
	<-done
	if calls.Load() != 1 || r.Failure == "" || time.Since(start) > 1500*time.Millisecond {
		t.Fatal("缺少单拨号/绝对期限")
	}
	b, _ := json.Marshal(oaSafeHeaders(http.Header{"Upgrade": {"websocket"}, "Connection": {"Upgrade"}, "Set-Cookie": {"offline-secret"}, "X-Request-Id": {"offline-secret"}, "Authorization": {"Bearer offline-secret"}}))
	if bytes.Contains(b, []byte("secret")) {
		t.Fatal("握手凭据泄漏")
	}
}

func TestOpenAIRealtimeRecorderOfflineRecordedRolesAndTampering(t *testing.T) {
	r, _ := oaRunPeer(t, "text-tools-vision", func(s string) string { return s }, false)
	if r.Failure != "" {
		t.Fatal(r.Failure)
	}
	// 只测试 recorded schema 分支，不落地为真实证据；来源标签不能认证云端。
	r.Synthetic = false
	f, err := oaCandidate(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range f.Response.WS.Nodes {
		want := map[testkit.WSPoint]string{testkit.WSClientSend: "authored", testkit.WSUpstreamReceive: "upstream-accepted", testkit.WSUpstreamSend: "recorded", testkit.WSClientReceive: "golden"}[n.Point]
		if n.Source != want {
			t.Fatal("四点来源错配")
		}
	}
	oaReplay(t, f)
	for _, tc := range []struct{ name, old, new string }{
		{"tool_ack", `"call_id":"call1","output":"绿色"`, `"call_id":"unknown","output":"绿色"`},
		{"wrong_response", `"id":"r4","status":"completed"`, `"id":"r3","status":"completed"`},
		{"token_limit", `"output_tokens":10`, `"output_tokens":129`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copy := r
			copy.Records = append([]oaRecord(nil), r.Records...)
			changed := false
			for i, rec := range copy.Records {
				if rec.Direction == "receive" && bytes.Contains(rec.Payload, []byte(tc.old)) {
					copy.Records[i].Payload = bytes.ReplaceAll(rec.Payload, []byte(tc.old), []byte(tc.new))
					changed = true
				}
			}
			if !changed {
				t.Fatal("负例未命中")
			}
			if _, err := oaCandidate(copy); err == nil {
				t.Fatal("篡改被当成证据")
			}
		})
	}
	for _, n := range f.Response.WS.Nodes {
		if n.Message != nil {
			n.Message.Payload[0] = ' '
			break
		}
	}
	if err := testkit.ValidateWSSession(f, testkit.DefaultWSLimits()); err == nil {
		t.Fatal("契约摘要未抓到篡改")
	}
}

func TestOpenAIRealtimeRecorderOfflineResourceAndEvidenceLimits(t *testing.T) {
	d := oaNewDriver("text-tools-vision", nil)
	d.confirmed = true
	d.responseIDs = map[string]bool{"r1": true, "r2": true, "r3": true, "r4": true}
	d.waitingResponse = true
	if err := d.observe(map[string]any{"type": "response.created", "response": map[string]any{"id": "r5"}}); err == nil {
		t.Fatal("第五个response未阻止")
	}
	data := make([]byte, oaMaxAudio+2)
	d = oaNewDriver("audio-manual", &oaSample{Data: data, SHA256: oaDigest(data), Public: true, Origin: "https://example.org/public-audio", License: "CC0-1.0", Format: "pcm_s16le", Rate: 24000, Channels: 1, Transcript: "公开测试。"})
	d.send = func([]byte) error { t.Fatal("超八秒仍写音频"); return nil }
	if err := d.audio(); err == nil {
		t.Fatal("超八秒未拒绝")
	}
	r, _ := oaRunPeer(t, "audio-manual", func(s string) string {
		return strings.ReplaceAll(s, `"transcript":"公开测试。"`, `"transcript":"不相关的非空转写"`)
	}, false)
	if r.Failure != "" {
		t.Fatal(r.Failure)
	}
	f, err := oaCandidate(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Response.WS.Coverage {
		if c.Capability == "speech_recognition" || c.Capability == "audio_input" || c.Capability == "speech_synthesis" {
			t.Fatal("不相关非空转写冒充内容一致")
		}
	}
}

func TestOpenAIRealtimeRecorderOfflineNoBusinessBeforeConfig(t *testing.T) {
	d := oaNewDriver("vad-interrupt", &oaSample{})
	for _, e := range []map[string]any{
		{"type": "input_audio_buffer.speech_started", "item_id": "i1", "audio_start_ms": float64(0)},
		{"type": "input_audio_buffer.committed", "item_id": "i1"},
	} {
		if err := d.observe(e); err == nil {
			t.Fatal("未确认配置即接纳业务证据")
		}
	}
}

func TestOpenAIRealtimeRecorderOfflineSymlinksAndExclusiveFiles(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, ".local/recordings/openairealtime")
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, ".local")); err != nil {
		t.Fatal(err)
	}
	if err := oaReserve(root, filepath.Join(root, "batch/run")); err == nil {
		t.Fatal("跟随.local链接")
	}
	out := t.TempDir()
	if err := oaWrite(out, "recording.json", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := oaWrite(out, "recording.json", []byte(`{"overwritten":true}`)); err == nil {
		t.Fatal("覆盖历史失败")
	}
	i, _ := os.Stat(filepath.Join(out, "recording.json"))
	if i.Mode().Perm() != 0600 {
		t.Fatal("文件权限不私有")
	}
}

func TestOpenAIRealtimeRecorderOfflineHandshakeFailureAndEOF(t *testing.T) {
	for _, mode := range []string{"http_error", "raw_eof", "secret_close"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			done := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				defer close(done)
				if mode == "http_error" {
					w.Header().Set("Set-Cookie", "offline-secret")
					w.WriteHeader(401)
					_, _ = w.Write([]byte(`{"error":{"message":"offline\u002dsecret"}}`))
					return
				}
				if mode == "raw_eof" {
					c, bw, err := w.(http.Hijacker).Hijack()
					if err == nil {
						accept := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
						_, _ = fmt.Fprintf(bw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(accept[:]))
						_ = bw.Flush()
						_ = c.Close()
					}
					return
				}
				c, err := ws.Accept(w, r, ws.AcceptOptions{MaxPayload: oaMaxBytes, WriteTimeout: time.Second})
				if err != nil {
					return
				}
				_ = c.Close(1000, "offline-secret")
			}))
			defer srv.Close()
			r := oaCapture(context.Background(), oaConfig{URL: "ws" + strings.TrimPrefix(srv.URL, "http"), Model: oaModel, Scenario: "text-tools-vision", Key: "offline-secret", Duration: 2 * time.Second})
			<-done
			if calls.Load() != 1 || r.Failure == "" {
				t.Fatal("失败被重试或伪装完成")
			}
			if mode == "raw_eof" && r.Status != 101 {
				t.Fatal("未在升级后构造原始EOF")
			}
			dir := t.TempDir()
			_ = oaSave(dir, r)
			b, err := os.ReadFile(filepath.Join(dir, "recording.json"))
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(b, []byte("offline")) {
				t.Fatal("握手错误或close秘密落盘")
			}
			if _, err := os.Stat(filepath.Join(dir, "candidate.json")); !os.IsNotExist(err) {
				t.Fatal("错误/EOF冒充有效关闭")
			}
		})
	}
}
