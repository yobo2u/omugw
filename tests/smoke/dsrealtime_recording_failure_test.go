//go:build smoke

package smoke_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/transport/ws"
)

func TestDSRealtimeRecorderOfflineFailureAndSecretSuppression(t *testing.T) {
	for _, mode := range []string{"handshake", "error", "secret_echo", "legacy_rate", "raw_eof", "large_message"} {
		t.Run(mode, func(t *testing.T) {
			var calls, inputs atomic.Int32
			done := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				defer close(done)
				calls.Add(1)
				if mode == "handshake" {
					w.Header().Set("X-Request-Id", "offline-secret")
					http.Error(w, "offline-secret", 401)
					return
				}
				c, err := ws.Accept(w, req, ws.AcceptOptions{MaxPayload: 2 << 20, Idle: time.Second, WriteTimeout: time.Second})
				if err != nil {
					return
				}
				defer c.Close(1000, "")
				_ = c.WriteMessage(ws.OpText, []byte(`{"type":"session.created","session":{"id":"s1"}}`))
				_, _, _ = c.ReadMessage()
				switch mode {
				case "error":
					_ = c.WriteMessage(ws.OpText, []byte(`{"type":"error","error":{"code":"invalid_value","message":"public fixture error"}}`))
				case "secret_echo":
					_ = c.WriteMessage(ws.OpText, []byte(`{"type":"error","error":{"message":"offline\u002dsecret"}}`))
				case "legacy_rate":
					_ = c.WriteMessage(ws.OpText, []byte(`{"type":"session.updated","session":{"input_audio_format":"pcm16"}}`))
				case "raw_eof":
					_ = c.Close(1006, "")
					return
				case "large_message":
					_ = c.WriteMessage(ws.OpText, []byte(strings.Repeat("x", (1<<20)+1)))
				}
				if op, _, err := c.ReadMessage(); err == nil && op == ws.OpText {
					inputs.Add(1)
				}
			}))
			defer server.Close()
			r := dsCapture(context.Background(), dsConfig{URL: "ws" + strings.TrimPrefix(server.URL, "http"), Model: "qwen3-tts-flash-realtime", Scenario: "tts-commit", Key: "offline-secret", Duration: 2 * time.Second, Responses: 1})
			<-done
			if calls.Load() != 1 || inputs.Load() != 0 || r.Failure == "" {
				t.Fatal("失败后重试/继续输入或伪成功")
			}
			if _, err := dsCandidate(r); err == nil {
				t.Fatal("失败候选被接受")
			}
			b, _ := json.Marshal(r)
			if strings.Contains(string(b), "offline-secret") {
				t.Fatal("原始头或错误文本泄密")
			}
			for _, rec := range r.Records {
				if !dsSafePayload(rec.Payload, "offline-secret") && rec.Kind == "message" {
					t.Fatal("转义凭据进入base64轨迹")
				}
			}
		})
	}
}
