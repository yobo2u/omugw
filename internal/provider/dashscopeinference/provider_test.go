package dashscopeinference

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/credential"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/protocol/dashscopewire"
	"github.com/yobo2u/omugw/internal/provider"
	"github.com/yobo2u/omugw/internal/router"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

var _ provider.StreamProvider = (*Provider)(nil)

func request(base string) provider.Request {
	return provider.Request{
		Target:     router.Target{Kind: degrade.ProviderDashScopeWSInference, BaseURL: base},
		Credential: credential.Credential{Secret: "upstream-secret"},
		Inbound:    degrade.Inbound{Protocol: degrade.ProtoDashScopeInference, Endpoint: degrade.EndpointDashScopeInference},
	}
}

func newProvider(t *testing.T) (*Provider, *ws.BufferBudget) {
	t.Helper()
	b, err := ws.NewBufferBudget(256 << 20)
	if err != nil {
		t.Fatal(err)
	}
	return New(config.Default().Timeouts, config.DefaultWebSocket(), b), b
}

func TestInferenceProviderHandshake(t *testing.T) {
	for _, tt := range []struct{ suffix, path string }{
		{"", "/api-ws/v1/inference"},
		{"/", "/api-ws/v1/inference"},
		{"/prefix/", "/prefix/api-ws/v1/inference"},
		{"/api-ws/v1/inference", "/api-ws/v1/inference"},
		{"/prefix/api-ws/v1/inference/", "/prefix/api-ws/v1/inference"},
		{"/a%2Fb", "/a%2Fb/api-ws/v1/inference"},
	} {
		for _, scheme := range []string{"http", "ws"} {
			t.Run(scheme+tt.suffix, func(t *testing.T) {
				seen, done := make(chan *http.Request, 1), make(chan error, 1)
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					seen <- r
					c, err := ws.Accept(w, r, ws.AcceptOptions{MaxPayload: 1024, Idle: time.Second})
					if err != nil {
						done <- err
						return
					}
					defer c.Close(ws.CloseNormal, "")
					// 上游等客户端发消息，防止 Dial 偷读 task-started 形成因果死锁。
					msg, err := c.ReadOwnedMessage(ctx)
					if err == nil {
						msg.Release()
						err = c.WriteMessage(ws.OpBinary, []byte("audio"))
					}
					done <- err
				}))
				defer s.Close()
				req := request(strings.Replace(s.URL, "http:", scheme+":", 1) + tt.suffix)
				req.Header = http.Header{
					"Authorization": {"Bearer client-secret"}, "User-Agent": {"client-agent"},
					"x-dashscope-workspace": {"tenant-a"}, "X-Dashscope-Datainspection": {"enable"},
					"Origin": {"https://client.test"}, "Sec-Websocket-Extensions": {"permessage-deflate"},
					"Cookie": {"secret-cookie"}, "X-Unknown": {"secret-extra"},
					"X-Dashscope-Async": {"enable"}, "Openai-Organization": {"secret-org"},
				}
				if scheme == "ws" {
					delete(req.Header, "Authorization")
					req.Header.Set("Api-Key", "client-secret")
				}
				before := req.Header.Clone()
				p, budget := newProvider(t)
				c, resp, err := p.Dial(ctx, req)
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close(ws.CloseNormal, "")
				if resp == nil || resp.StatusCode != 101 || p.Kind() != degrade.ProviderDashScopeWSInference {
					t.Fatalf("错误的握手或 kind: %+v", resp)
				}
				r := <-seen
				if r.Method != "GET" || r.RequestURI != tt.path || r.URL.RawQuery != "" || r.URL.ForceQuery {
					t.Fatalf("固定路径不得重复或夹带 query: %s %s", r.Method, r.RequestURI)
				}
				wantHeaders := map[string]string{
					"Authorization": "Bearer upstream-secret", "User-Agent": "omugw",
					"X-Dashscope-Workspace": "tenant-a", "X-Dashscope-Datainspection": "enable",
					"Upgrade": "websocket", "Connection": "Upgrade", "Sec-Websocket-Version": "13",
				}
				for k, v := range wantHeaders {
					if r.Header.Get(k) != v || len(r.Header.Values(k)) != 1 {
						t.Errorf("头 %s=%q", k, r.Header.Values(k))
					}
				}
				for k := range r.Header {
					if _, ok := wantHeaders[k]; !ok && k != "Sec-Websocket-Key" {
						t.Errorf("转发了非白名单头: %s", k)
					}
				}
				if !reflect.DeepEqual(req.Header, before) {
					t.Fatal("修改了调用方头")
				}
				if err := c.WriteMessage(ws.OpText, []byte(`{"header":{"action":"run-task"}}`)); err != nil {
					t.Fatal(err)
				}
				msg, err := c.ReadOwnedMessage(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if msg.Opcode != ws.OpBinary || string(msg.Payload) != "audio" || budget.Used() == 0 {
					t.Errorf("消息或共享预算未保全: %+v used=%d", msg, budget.Used())
				}
				msg.Release()
				if budget.Used() != 0 {
					t.Fatal("消息释放后预算未归还")
				}
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			})
		}
	}
	t.Run("安全scheme保留", func(t *testing.T) {
		for _, scheme := range []string{"https", "wss"} {
			got, err := upstreamURL(scheme + "://upstream.test/prefix/api-ws/v1/inference")
			if err != nil || got != "wss://upstream.test/prefix/api-ws/v1/inference" {
				t.Fatalf("TLS 或固定路径丢失: %q %v", got, err)
			}
		}
	})
}

func TestInferenceProviderRejectsUnsafeInput(t *testing.T) {
	var dials atomic.Int32
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	s.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			dials.Add(1)
		}
	}
	s.Start()
	defer s.Close()
	p, budget := newProvider(t)
	for _, tt := range []struct {
		name string
		h    http.Header
	}{
		{"子协议密钥", http.Header{"Sec-Websocket-Protocol": {"insecure-api-key.client-secret"}}},
		{"任意子协议", http.Header{"sec-websocket-protocol": {"inference"}}},
		{"空子协议", http.Header{"Sec-Websocket-Protocol": nil}},
		{"重复auth", http.Header{"Authorization": {"Bearer a", "Bearer b"}}},
		{"大小写重复auth", http.Header{"Authorization": {"a"}, "authorization": {"b"}}},
		{"双鉴权", http.Header{"Authorization": {"a"}, "Api-Key": {"b"}}},
		{"重复key", http.Header{"Api-Key": {"a", "b"}}},
		{"auth注入", http.Header{"Authorization": {"secret\r\nX: injected"}}},
		{"租户重复", http.Header{"X-Dashscope-Workspace": {"a", "b"}}},
		{"租户大小写重复", http.Header{"X-Dashscope-Workspace": {"a"}, "x-dashscope-workspace": {"b"}}},
		{"租户空列表", http.Header{"X-Dashscope-Workspace": nil}},
		{"审查重复", http.Header{"X-Dashscope-Datainspection": {"enable", "disable"}}},
		{"租户注入", http.Header{"X-Dashscope-Workspace": {"secret\r\nX: injected"}}},
		{"审查TAB", http.Header{"X-Dashscope-Datainspection": {"enable\t"}}},
		{"租户DEL", http.Header{"X-Dashscope-Workspace": {"secret\x7f"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateHeaders(tt.h); err == nil {
				t.Fatal("预检接受非法头")
			}
			r := request(s.URL)
			r.Header = tt.h
			c, resp, err := p.Dial(context.Background(), r)
			assertRejected(t, c, resp, err)
		})
	}
	for _, base := range []string{
		s.URL + "?secret", s.URL + "?", s.URL + "#secret", s.URL + "#",
		strings.Replace(s.URL, "://", "://user:secret@", 1),
		strings.Replace(s.URL, "http:", "ftp:", 1), "ws:secret", "ws:///secret", "ws://bad host/secret",
	} {
		t.Run("URL_"+base, func(t *testing.T) {
			c, resp, err := p.Dial(context.Background(), request(base))
			assertRejected(t, c, resp, err)
		})
	}
	for _, tt := range []struct {
		name   string
		change func(*provider.Request)
	}{
		{"错误kind", func(r *provider.Request) { r.Target.Kind = degrade.ProviderDashScopeWSRealtime }},
		{"错误协议", func(r *provider.Request) { r.Inbound.Protocol = degrade.ProtoDashScopeRealtime }},
		{"错误端点", func(r *provider.Request) { r.Inbound.Endpoint = degrade.EndpointDashScopeRealtime }},
		{"空入站", func(r *provider.Request) { r.Inbound = degrade.Inbound{} }},
		{"非空model", func(r *provider.Request) { r.Target.UpstreamModel = "paraformer-realtime-v2" }},
		{"空白model", func(r *provider.Request) { r.Target.UpstreamModel = " " }},
		{"凭据注入", func(r *provider.Request) { r.Credential.Secret = "secret\r\nX: injected" }},
		{"缺少凭据", func(r *provider.Request) { r.Credential.Secret = "" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := request(s.URL)
			tt.change(&r)
			c, resp, err := p.Dial(context.Background(), r)
			assertRejected(t, c, resp, err)
		})
	}
	for _, invalid := range []*Provider{
		New(config.Default().Timeouts, config.DefaultWebSocket(), nil),
		New(config.Timeouts{}, config.DefaultWebSocket(), budget),
		New(config.Default().Timeouts, config.WebSocket{}, budget),
	} {
		c, resp, err := invalid.Dial(context.Background(), request(s.URL))
		assertRejected(t, c, resp, err)
	}
	if dials.Load() != 0 {
		t.Fatalf("非法请求产生了物理连接: %d", dials.Load())
	}
	for _, h := range []http.Header{nil, {"Origin": {"https://sdk.test"}, "Sec-Websocket-Extensions": {"permessage-deflate"}}, {"Api-Key": {"client-secret"}}} {
		if err := ValidateHeaders(h); err != nil {
			t.Fatalf("合法头被拒绝: %v", err)
		}
	}
}

func assertRejected(t *testing.T, c *ws.Conn, resp *http.Response, err error) {
	t.Helper()
	if c != nil {
		c.Close(ws.CloseNormal, "")
		t.Fatal("非法请求返回连接")
	}
	if resp != nil || err == nil {
		t.Fatalf("拒绝结果: resp=%v err=%v", resp, err)
	}
	ce := canonical.AsError(err)
	if ce.Retryable || errors.Unwrap(err) != nil {
		t.Fatalf("本地拒绝不得可重试或保留 cause: %v", err)
	}
	assertSafe(t, fmt.Sprintf("%+v", ce))
}

func assertSafe(t *testing.T, text string) {
	t.Helper()
	for _, bad := range []string{"secret", "http://", "https://", "ws://", "wss://", "127.0.0.1", "body-marker"} {
		if strings.Contains(text, bad) {
			t.Fatalf("错误泄露 %q: %s", bad, text)
		}
	}
}

func TestInferenceProviderBounds(t *testing.T) {
	prefix, tail := `{"message":"body-marker secret`, `","code":"Arrearage"}`
	exact := prefix + strings.Repeat(" ", (64<<10)-len(prefix)-len(tail)) + tail
	for _, tt := range []struct {
		name   string
		status int
		body   string
		class  canonical.ErrorClass
		retry  bool
	}{
		{"401", 401, `{"message":"body-marker secret","request_id":"secret"}`, canonical.ClassAuth, true},
		{"429", 429, `{"code":"Throttling.secret","message":"body-marker"}`, canonical.ClassRateLimit, true},
		{"503", 503, `{"code":"InternalError.secret","message":"body-marker"}`, canonical.ClassUpstreamUnavailable, true},
		{"未知400", 400, `{"code":"unknown-secret","message":"body-marker"}`, canonical.ClassBadRequest, false},
		{"重定向", 302, `{"code":"ServiceUnavailable","message":"body-marker"}`, canonical.ClassInternal, false},
		{"意外200", 200, "body-marker secret", canonical.ClassInternal, false},
		{"畸形101", 101, "", canonical.ClassInternal, false},
		{"恰好64KiB仍分类", 400, exact, canonical.ClassQuota, true},
		{"超出64KiB不读尾部code", 400, " " + exact, canonical.ClassBadRequest, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var redirects atomic.Int32
			dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirects.Add(1) }))
			defer dest.Close()
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "7")
				w.Header().Set("X-RateLimit-Remaining-Requests", "0")
				w.Header().Set("Location", dest.URL+"/secret")
				w.Header().Set("X-Secret", "secret")
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer s.Close()
			p, _ := newProvider(t)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			c, resp, err := p.Dial(ctx, request(s.URL))
			if c != nil {
				c.Close(ws.CloseNormal, "")
				t.Fatal("失败握手返回连接")
			}
			if resp == nil || err == nil {
				t.Fatalf("失败必须保留状态: resp=%v err=%v", resp, err)
			}
			ce := canonical.AsError(err)
			if ce.Class != tt.class || ce.Retryable != tt.retry || ce.UpstreamStatus != tt.status || resp.StatusCode != tt.status {
				t.Fatalf("错误分类: %+v status=%d", ce, resp.StatusCode)
			}
			if tt.status >= 400 && (ce.RetryAfter != 7*time.Second || resp.Header.Get("Retry-After") != "7" || resp.Header.Get("X-Ratelimit-Remaining-Requests") != "0") {
				t.Fatalf("安全退避头丢失: %+v", ce)
			}
			if ce.UpstreamRequestID != "" || ce.Param != "" || errors.Unwrap(err) != nil || resp.Request != nil || resp.TLS != nil {
				t.Fatalf("原始元数据未清理: %+v %+v", ce, resp)
			}
			_, body, headers := dashscopewire.EncodeError(ce)
			assertSafe(t, string(body)+fmt.Sprint(headers)+fmt.Sprintf("%+v %+v", ce, resp))
			data, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil || len(data) != 0 || resp.Body != http.NoBody || redirects.Load() != 0 {
				t.Fatalf("失败体未释放或发生重定向: bytes=%d err=%v redirects=%d", len(data), readErr, redirects.Load())
			}
		})
	}
	t.Run("超限失败流必须关闭而非排空", func(t *testing.T) {
		closed := make(chan struct{})
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer close(closed)
			w.WriteHeader(503)
			_, _ = io.WriteString(w, strings.Repeat("x", (64<<10)+1))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}))
		defer s.Close()
		p, _ := newProvider(t)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		c, resp, err := p.Dial(ctx, request(s.URL))
		if c != nil {
			c.Close(ws.CloseNormal, "")
			t.Fatal("失败返回连接")
		}
		if err == nil || resp == nil || resp.Body != http.NoBody || ctx.Err() != nil {
			t.Fatalf("等待排空或泄漏失败体: %+v %v ctx=%v", resp, err, ctx.Err())
		}
		select {
		case <-closed:
		case <-ctx.Done():
			t.Fatal("失败 socket 未关闭")
		}
	})
	t.Run("握手头64KiB上限", func(t *testing.T) {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Large", strings.Repeat("a", 64<<10))
			w.WriteHeader(503)
		}))
		defer s.Close()
		p, _ := newProvider(t)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		c, resp, err := p.Dial(ctx, request(s.URL))
		assertRejected(t, c, resp, err)
	})
	t.Run("消息上限", func(t *testing.T) {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := ws.Accept(w, r, ws.AcceptOptions{})
			if err != nil {
				t.Error(err)
				return
			}
			defer c.Close(ws.CloseNormal, "")
			if err := c.WriteMessage(ws.OpBinary, []byte("12345")); err != nil {
				t.Error(err)
			}
		}))
		defer s.Close()
		_, budget := newProvider(t)
		p := New(config.Default().Timeouts, config.WebSocket{MaxMessageBytes: 4, MaxSessions: 1, MaxBufferedBytes: 12}, budget)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		c, _, err := p.Dial(ctx, request(s.URL))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close(ws.CloseNormal, "")
		msg, err := c.ReadOwnedMessage(ctx)
		if msg != nil {
			msg.Release()
		}
		if !errors.Is(err, ws.ErrMessageTooLarge) || budget.Used() != 0 {
			t.Fatalf("消息限额未接线或预算泄漏: %v used=%d", err, budget.Used())
		}
	})
	t.Run("共同握手期限", func(t *testing.T) {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
		defer s.Close()
		p, _ := newProvider(t)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		c, resp, err := p.Dial(ctx, request(s.URL))
		if c != nil {
			c.Close(ws.CloseNormal, "")
			t.Fatal("超时返回连接")
		}
		ce := canonical.AsError(err)
		if resp != nil || ce == nil || ce.Class != canonical.ClassUpstreamUnavailable || !ce.Retryable {
			t.Fatalf("明确超时分类丢失: %v", err)
		}
		assertSafe(t, fmt.Sprintf("%+v", ce))
	})
	t.Run("取消不可重试", func(t *testing.T) {
		p, _ := newProvider(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		c, resp, err := p.Dial(ctx, request("http://127.0.0.1:1"))
		assertRejected(t, c, resp, err)
	})
	t.Run("connect覆盖TLS", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		stop, done := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(done)
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
			<-stop
		}()
		defer func() { close(stop); ln.Close(); <-done }()
		timeouts := config.Default().Timeouts
		timeouts.Connect = 50 * time.Millisecond
		_, budget := newProvider(t)
		p := New(timeouts, config.DefaultWebSocket(), budget)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		start := time.Now()
		c, resp, err := p.Dial(ctx, request("https://"+ln.Addr().String()))
		assertRejected(t, c, resp, err)
		if time.Since(start) > time.Second {
			t.Fatal("TLS 未受 connect 限制")
		}
	})
	t.Run("idle读取期限", func(t *testing.T) {
		base := silentUpstream(t)
		timeouts := config.Default().Timeouts
		timeouts.Idle = 50 * time.Millisecond
		_, budget := newProvider(t)
		p := New(timeouts, config.DefaultWebSocket(), budget)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		c, _, err := p.Dial(ctx, request(base))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close(ws.CloseNormal, "")
		start := time.Now()
		msg, err := c.ReadOwnedMessage(ctx)
		if msg != nil {
			msg.Release()
		}
		if !errors.Is(err, ws.ErrIdleTimeout) || time.Since(start) > time.Second {
			t.Fatalf("idle 未接线: %v", err)
		}
	})
	for _, tt := range []struct {
		name          string
		connect, idle time.Duration
	}{
		{"W取connect", 50 * time.Millisecond, time.Second},
		{"W取idle", time.Second, 50 * time.Millisecond},
	} {
		t.Run(tt.name, func(t *testing.T) {
			base := silentUpstream(t)
			timeouts := config.Default().Timeouts
			timeouts.Connect, timeouts.Idle = tt.connect, tt.idle
			_, budget := newProvider(t)
			p := New(timeouts, config.DefaultWebSocket(), budget)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			c, _, err := p.Dial(ctx, request(base))
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close(ws.CloseNormal, "")
			// 测试收尾期限不能冒充有限写期限生效。
			timer := time.AfterFunc(2*time.Second, func() { c.Close(ws.CloseNormal, "") })
			defer timer.Stop()
			start := time.Now()
			err = c.WriteMessage(ws.OpBinary, make([]byte, 32<<20))
			var netErr net.Error
			if !errors.As(err, &netErr) || !netErr.Timeout() || time.Since(start) > 750*time.Millisecond {
				t.Fatalf("W 未取 min(connect,idle): %v", err)
			}
			if budget.Used() != 0 {
				t.Fatal("失败写泄漏预算")
			}
		})
	}
}

// 慢读端使用真实 TCP 缓冲；关停时 join handler，避免故障测试遗留连接。
func silentUpstream(t *testing.T) string {
	t.Helper()
	stop, done := make(chan struct{}), make(chan error, 1)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := ws.Accept(w, r, ws.AcceptOptions{})
		if err == nil {
			<-stop
			_ = c.Close(ws.CloseNormal, "")
		}
		done <- err
	}))
	t.Cleanup(func() {
		close(stop)
		s.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(2 * time.Second):
			t.Error("上游 handler 未收尾")
		}
	})
	return s.URL
}
