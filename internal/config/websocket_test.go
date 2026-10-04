package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWebSocketConfig(t *testing.T) {
	want := WebSocket{MaxMessageBytes: 32 << 20, MaxSessions: 128, MaxBufferedBytes: 256 << 20}
	if got := DefaultWebSocket(); got != want || Default().WebSocket != want {
		t.Fatalf("默认限额: got=%+v config=%+v want=%+v", got, Default().WebSocket, want)
	}
	for _, tt := range []struct {
		name   string
		limits WebSocket
		valid  bool
	}{
		{"默认", want, true},
		{"最小", WebSocket{1, 1, 2}, true},
		{"最大", WebSocket{64 << 20, 4096, 2 << 30}, true},
		{"恰好两倍", WebSocket{64 << 20, 1, 128 << 20}, true},
		{"消息零", WebSocket{0, 128, 256 << 20}, false},
		{"消息负", WebSocket{-1, 128, 256 << 20}, false},
		{"消息超限", WebSocket{64<<20 + 1, 128, 256 << 20}, false},
		{"会话零", WebSocket{32 << 20, 0, 256 << 20}, false},
		{"会话负", WebSocket{32 << 20, -1, 256 << 20}, false},
		{"会话超限", WebSocket{32 << 20, 4097, 256 << 20}, false},
		{"预算零", WebSocket{32 << 20, 128, 0}, false},
		{"预算负", WebSocket{32 << 20, 128, -1}, false},
		{"预算超限", WebSocket{32 << 20, 128, 2<<30 + 1}, false},
		{"不足两倍", WebSocket{32 << 20, 128, 64<<20 - 1}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.limits.Validate(); (err == nil) != tt.valid {
				t.Fatalf("Validate()=%v valid=%v", err, tt.valid)
			}
			c := Default()
			c.WebSocket = tt.limits
			if err := c.Validate(); (err == nil) != tt.valid {
				t.Fatalf("Config.Validate()=%v valid=%v", err, tt.valid)
			}
		})
	}
	t.Run("YAML缺省与显式零", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		for _, tt := range []struct {
			yaml     string
			valid    bool
			sessions int
		}{
			{"{}", true, 128},
			{"websocket:\n  max_sessions: 8\n", true, 8},
			{"websocket:\n  max_sessions: 0\n", false, 0},
			{"websocket:\n  max_message_bytes: 7\n  max_buffered_bytes: 13\n", false, 0},
		} {
			if err := os.WriteFile(path, []byte(tt.yaml), 0600); err != nil {
				t.Fatal(err)
			}
			c, err := Load(path)
			if (err == nil) != tt.valid {
				t.Fatalf("Load(%q)=%v", tt.yaml, err)
			}
			if tt.valid && (c.WebSocket.MaxSessions != tt.sessions || c.WebSocket.MaxMessageBytes != 32<<20 || c.WebSocket.MaxBufferedBytes != 256<<20) {
				t.Fatalf("YAML默认值未保全: %+v", c.WebSocket)
			}
		}
	})
}

func TestWebSocketProviderURLConfig(t *testing.T) {
	for _, kind := range []string{"dashscope.ws.realtime", "dashscope.ws.inference", "openai.realtime", "openai.compat", "dashscope.compatible"} {
		for _, scheme := range []string{"http", "https", "ws", "wss"} {
			t.Run(kind+"/"+scheme, func(t *testing.T) {
				c := fullGateway()
				c.Providers[0].Kind = kind
				c.Providers[0].BaseURL = scheme + "://example.test/prefix"
				valid := scheme == "http" || scheme == "https" || kind == "openai.realtime" || strings.HasPrefix(kind, "dashscope.ws.")
				if err := c.Validate(); (err == nil) != valid {
					t.Fatalf("Validate()=%v valid=%v", err, valid)
				}
			})
		}
	}
	for _, raw := range []string{
		"wss://secret@example.test", "wss://example.test?secret", "wss://example.test?",
		"wss://example.test#secret", "wss://example.test#", "wss:secret", "wss:///secret", "wss://bad host/secret", "ftp://example.test/secret",
	} {
		t.Run(raw, func(t *testing.T) {
			c := fullGateway()
			c.Providers[0].Kind = "dashscope.ws.realtime"
			c.Providers[0].BaseURL = raw
			err := c.Validate()
			if err == nil {
				t.Fatal("不安全WS地址应在启动时拒绝")
			}
			if strings.Contains(err.Error(), raw) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("错误泄漏地址: %v", err)
			}
		})
	}
	// 既有 HTTP 允许的 userinfo 不能因增添 WS 配置被一并收紧。
	c := fullGateway()
	c.Providers[0].BaseURL = "https://user:password@example.test/prefix"
	if err := c.Validate(); err != nil {
		t.Fatalf("既有HTTP契约被改变: %v", err)
	}
}
