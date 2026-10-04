package dashscoperealtime

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
		Target:     router.Target{Kind: degrade.ProviderDashScopeWSRealtime, BaseURL: base, UpstreamModel: "qwen model/+?&=#中文"},
		Credential: credential.Credential{Secret: "upstream-secret"},
		Inbound:    degrade.Inbound{Protocol: degrade.ProtoDashScopeRealtime, Endpoint: degrade.EndpointDashScopeRealtime},
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

func TestRealtimeProviderHandshake(t *testing.T) {
	for _, tt := range []struct{ suffix, path string }{
		{"", "/api-ws/v1/realtime"},
		{"/", "/api-ws/v1/realtime"},
		{"/prefix/", "/prefix/api-ws/v1/realtime"},
		{"/prefix/api-ws/v1/realtime", "/prefix/api-ws/v1/realtime"},
		{"/prefix/api-ws/v1/realtime/", "/prefix/api-ws/v1/realtime"},
		{"/a%2Fb", "/a%2Fb/api-ws/v1/realtime"},
	} {
		for _, scheme := range []string{"http", "ws"} {
			t.Run(scheme+tt.suffix, func(t *testing.T) {
				seen := make(chan *http.Request, 1)
				done := make(chan error, 1)
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					seen <- r
					c, err := ws.Accept(w, r, ws.AcceptOptions{})
					if err != nil {
						done <- err
						return
					}
					defer c.Close(ws.CloseNormal, "")
					done <- c.WriteMessage(ws.OpText, []byte(`{"type":"session.created"}`))
				}))
				defer s.Close()
				req := request(strings.Replace(s.URL, "http:", scheme+":", 1) + tt.suffix)
				req.Header = http.Header{
					"Authorization": {"Bearer client-secret"}, "User-Agent": {"client-agent"},
					"x-dashscope-workspace": {"tenant-a"}, "X-Dashscope-Datainspection": {"enable"},
					"Origin": {"https://client.test"}, "Sec-Websocket-Extensions": {"permessage-deflate"},
					"Cookie": {"secret-cookie"}, "X-Dashscope-Async": {"enable"},
					"Openai-Organization": {"org"}, "Openai-Project": {"project"},
					"Openai-Safety-Identifier": {"safety"}, "X-Unknown": {"secret-extra"},
				}
				before := req.Header.Clone()
				p, budget := newProvider(t)
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				c, resp, err := p.Dial(ctx, req)
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close(ws.CloseNormal, "")
				if resp.StatusCode != 101 || p.Kind() != degrade.ProviderDashScopeWSRealtime {
					t.Fatalf("错误的握手或kind: %+v", resp)
				}
				r := <-seen
				if r.Method != "GET" || r.URL.EscapedPath() != tt.path || len(r.URL.Query()) != 1 || r.URL.Query().Get("model") != req.Target.UpstreamModel {
					t.Fatalf("错误的请求目标: %s %s", r.Method, r.URL)
				}
				wantHeaders := map[string]string{"Authorization": "Bearer upstream-secret", "User-Agent": "omugw", "X-Dashscope-Workspace": "tenant-a", "X-Dashscope-Datainspection": "enable", "Upgrade": "websocket", "Connection": "Upgrade", "Sec-Websocket-Version": "13"}
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
				msg, err := c.ReadOwnedMessage(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if string(msg.Payload) != `{"type":"session.created"}` || budget.Used() == 0 {
					t.Fatalf("共享预算或首帧未保全: %+v used=%d", msg, budget.Used())
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
}

func TestRealtimeProviderRejectsUnsafeHeadersAndURL(t *testing.T) {
	var hits atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.WriteHeader(503) }))
	defer s.Close()
	p, _ := newProvider(t)
	for _, tt := range []struct {
		name   string
		header http.Header
	}{
		{"子协议密钥", http.Header{"Sec-Websocket-Protocol": {"openai-insecure-api-key.client-secret"}}},
		{"任意子协议", http.Header{"sec-websocket-protocol": {"realtime"}}},
		{"空子协议头", http.Header{"Sec-Websocket-Protocol": {""}}},
		{"重复鉴权", http.Header{"Authorization": {"Bearer a", "Bearer b"}}},
		{"大小写重复鉴权", http.Header{"Authorization": {"Bearer a"}, "authorization": {"Bearer b"}}},
		{"双鉴权来源", http.Header{"Authorization": {"Bearer a"}, "Api-Key": {"b"}}},
		{"重复ApiKey", http.Header{"Api-Key": {"a", "b"}}},
		{"鉴权控制字符", http.Header{"Authorization": {"Bearer client-secret\r\nX: y"}}},
		{"租户重复", http.Header{"X-Dashscope-Workspace": {"a", "b"}}},
		{"租户大小写重复", http.Header{"X-Dashscope-Workspace": {"a"}, "x-dashscope-workspace": {"b"}}},
		{"审查重复", http.Header{"X-Dashscope-Datainspection": {"enable", "disable"}}},
		{"租户空值列表", http.Header{"X-Dashscope-Workspace": nil}},
		{"租户换行", http.Header{"X-Dashscope-Workspace": {"a\r\nclient-secret"}}},
		{"审查TAB", http.Header{"X-Dashscope-Datainspection": {"enable\t"}}},
		{"租户DEL", http.Header{"X-Dashscope-Workspace": {"a\x7f"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateHeaders(tt.header); err == nil {
				t.Fatal("预检接受了非法头")
			}
			r := request(s.URL)
			r.Header = tt.header
			c, resp, err := p.Dial(context.Background(), r)
			assertRejected(t, c, resp, err)
		})
	}
	for _, suffix := range []string{"?secret", "?", "#secret", "#"} {
		t.Run("URL"+suffix, func(t *testing.T) {
			c, resp, err := p.Dial(context.Background(), request(s.URL+suffix))
			assertRejected(t, c, resp, err)
		})
	}
	for _, base := range []string{strings.Replace(s.URL, "://", "://user:secret@", 1), "ws:secret", "ftp://example.test", "ws:///secret", "ws://bad host/secret"} {
		t.Run("URL"+base, func(t *testing.T) {
			c, resp, err := p.Dial(context.Background(), request(base))
			assertRejected(t, c, resp, err)
		})
	}
	for _, tt := range []struct {
		name   string
		change func(*provider.Request)
	}{
		{"错误kind", func(r *provider.Request) { r.Target.Kind = degrade.ProviderOpenAIRealtime }},
		{"错误协议", func(r *provider.Request) { r.Inbound.Protocol = degrade.ProtoOpenAIRealtime }},
		{"错误端点", func(r *provider.Request) { r.Inbound.Endpoint = degrade.EndpointOpenAIRealtime }},
		{"缺少协议", func(r *provider.Request) { r.Inbound = degrade.Inbound{} }},
		{"缺少模型", func(r *provider.Request) { r.Target.UpstreamModel = "" }},
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
	_, budget := newProvider(t)
	for _, invalid := range []*Provider{
		New(config.Default().Timeouts, config.DefaultWebSocket(), nil),
		New(config.Default().Timeouts, config.WebSocket{}, budget),
		New(config.Timeouts{}, config.DefaultWebSocket(), budget),
	} {
		c, resp, err := invalid.Dial(context.Background(), request(s.URL))
		assertRejected(t, c, resp, err)
	}
	if hits.Load() != 0 {
		t.Fatalf("非法请求触达了上游: %d", hits.Load())
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
		t.Fatal("非法请求得到了连接")
	}
	if resp != nil || err == nil {
		t.Fatalf("拒绝结果: resp=%v err=%v", resp, err)
	}
	ce := canonical.AsError(err)
	if ce.Retryable {
		t.Fatalf("本地非法请求不可重试: %v", err)
	}
	assertSafe(t, fmt.Sprintf("%+v", ce))
}

func assertSafe(t *testing.T, text string) {
	t.Helper()
	for _, bad := range []string{"secret", "http://", "https://", "ws://", "wss://", "127.0.0.1", "body-marker"} {
		if strings.Contains(text, bad) {
			t.Fatalf("错误携带敏感数据 %q: %s", bad, text)
		}
	}
}

func TestRealtimeProviderFailure(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
		class  canonical.ErrorClass
		retry  bool
	}{
		{"401", 401, `{"message":"body-marker secret http://example.test","request_id":"secret"}`, canonical.ClassAuth, true},
		{"403", 403, `{"code":"unknown-secret","message":"body-marker"}`, canonical.ClassAuth, true},
		{"429", 429, `{"code":"Throttling.secret","message":"body-marker"}`, canonical.ClassRateLimit, true},
		{"503", 503, `{"code":"InternalError.secret","message":"body-marker"}`, canonical.ClassUpstreamUnavailable, true},
		{"余额不足", 400, `{"code":"Arrearage","message":"body-marker"}`, canonical.ClassQuota, true},
		{"未知400", 400, `{"code":"unknown-secret","message":"body-marker"}`, canonical.ClassBadRequest, false},
		{"重定向", 302, `{"code":"ServiceUnavailable","message":"body-marker"}`, canonical.ClassInternal, false},
		{"意外200", 200, "body-marker secret", canonical.ClassInternal, false},
		{"畸形101", 101, "", canonical.ClassInternal, false},
		{"超大错误体", 503, strings.Repeat("body-marker secret", 8192), canonical.ClassUpstreamUnavailable, true},
		{"截断体不可升级分类", 400, `{"message":"` + strings.Repeat(" ", 64<<10) + `","code":"Arrearage"}`, canonical.ClassBadRequest, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var redirected atomic.Int32
			destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
			defer destination.Close()
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "7")
				w.Header().Set("X-RateLimit-Remaining-Requests", "0")
				w.Header().Set("Location", destination.URL+"/secret")
				w.Header().Set("X-Secret", "secret")
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer s.Close()
			p, _ := newProvider(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			c, resp, err := p.Dial(ctx, request(s.URL))
			if c != nil {
				c.Close(ws.CloseNormal, "")
				t.Fatal("失败握手返回连接")
			}
			if err == nil || resp == nil {
				t.Fatalf("失败必须带status: resp=%v err=%v", resp, err)
			}
			ce := canonical.AsError(err)
			if ce.Class != tt.class || ce.Retryable != tt.retry || ce.UpstreamStatus != tt.status || resp.StatusCode != tt.status {
				t.Fatalf("错误分类: %+v status=%d", ce, resp.StatusCode)
			}
			if tt.status >= 400 && ce.RetryAfter != 7*time.Second {
				t.Fatalf("Retry-After丢失: %+v", ce)
			}
			if ce.UpstreamRequestID != "" || errors.Unwrap(err) != nil {
				t.Fatalf("未清除不可信错误元数据: %+v", ce)
			}
			_, body, headers := dashscopewire.EncodeError(ce)
			assertSafe(t, string(body)+fmt.Sprint(headers)+fmt.Sprintf("%+v", resp)+fmt.Sprintf("%+v", ce))
			data, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil || len(data) != 0 {
				t.Fatalf("Provider传播了原始错误体: bytes=%d err=%v", len(data), readErr)
			}
			if redirected.Load() != 0 {
				t.Fatal("跟随了重定向")
			}
		})
	}
}

func TestRealtimeProviderBounds(t *testing.T) {
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
		_, b := newProvider(t)
		p := New(config.Default().Timeouts, config.WebSocket{MaxMessageBytes: 4, MaxSessions: 1, MaxBufferedBytes: 12}, b)
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
		if !errors.Is(err, ws.ErrMessageTooLarge) || b.Used() != 0 {
			t.Fatalf("消息上限未接线或预算泄漏: err=%v used=%d", err, b.Used())
		}
	})
	t.Run("握手头上限", func(t *testing.T) {
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
	t.Run("明确握手超时", func(t *testing.T) {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
		defer s.Close()
		p, _ := newProvider(t)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		c, resp, err := p.Dial(ctx, request(s.URL))
		if c != nil {
			c.Close(ws.CloseNormal, "")
			t.Fatal("超时返回连接")
		}
		ce := canonical.AsError(err)
		if resp != nil || ce == nil || ce.Class != canonical.ClassUpstreamUnavailable || !ce.Retryable {
			t.Fatalf("明确超时分类错误: %v", err)
		}
		assertSafe(t, fmt.Sprintf("%+v", ce))
	})
	t.Run("取消不是可重试超时", func(t *testing.T) {
		p, _ := newProvider(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		c, resp, err := p.Dial(ctx, request("http://127.0.0.1:1"))
		assertRejected(t, c, resp, err)
	})
}

func TestRealtimeProviderTimeoutWiring(t *testing.T) {
	t.Run("连接期限覆盖TLS", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
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
		timeouts.Connect = 30 * time.Millisecond
		_, budget := newProvider(t)
		p := New(timeouts, config.DefaultWebSocket(), budget)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		start := time.Now()
		c, resp, err := p.Dial(ctx, request("https://"+ln.Addr().String()))
		assertRejected(t, c, resp, err)
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("连接期限没有覆盖TLS: %v", elapsed)
		}
	})
	t.Run("空闲读取期限", func(t *testing.T) {
		stop := make(chan struct{})
		done := make(chan error, 1)
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := ws.Accept(w, r, ws.AcceptOptions{})
			if err != nil {
				done <- err
				return
			}
			defer c.Close(ws.CloseNormal, "")
			<-stop
			done <- nil
		}))
		defer s.Close()
		defer func() {
			close(stop)
			if err := <-done; err != nil {
				t.Error(err)
			}
		}()
		timeouts := config.Default().Timeouts
		timeouts.Idle = 30 * time.Millisecond
		_, budget := newProvider(t)
		p := New(timeouts, config.DefaultWebSocket(), budget)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		c, _, err := p.Dial(ctx, request(s.URL))
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
			t.Fatalf("空闲期限未生效: %v", err)
		}
	})
	for _, tc := range []struct {
		name          string
		connect, idle time.Duration
	}{
		{"写期限取connect", 30 * time.Millisecond, time.Second},
		{"写期限取idle", time.Second, 30 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stop, done := make(chan struct{}), make(chan error, 1)
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, err := ws.Accept(w, r, ws.AcceptOptions{})
				if err != nil {
					done <- err
					return
				}
				defer c.Close(ws.CloseNormal, "")
				<-stop
				done <- nil
			}))
			defer s.Close()
			defer func() {
				close(stop)
				if err := <-done; err != nil {
					t.Error(err)
				}
			}()
			timeouts := config.Default().Timeouts
			timeouts.Connect, timeouts.Idle = tc.connect, tc.idle
			_, budget := newProvider(t)
			p := New(timeouts, config.DefaultWebSocket(), budget)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			c, _, err := p.Dial(ctx, request(s.URL))
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close(ws.CloseNormal, "")
			// 硬截止仅供测试失效时收尾，不能成为断言成功的依据。
			timer := time.AfterFunc(2*time.Second, func() { c.Close(ws.CloseNormal, "") })
			defer timer.Stop()
			start := time.Now()
			err = c.WriteMessage(ws.OpBinary, make([]byte, 32<<20))
			var netErr net.Error
			if !errors.As(err, &netErr) || !netErr.Timeout() || time.Since(start) > 500*time.Millisecond {
				t.Fatalf("写期限未取两者最小值: %v", err)
			}
		})
	}
	t.Run("total和握手ctx不侵入会话", func(t *testing.T) {
		send, done := make(chan struct{}), make(chan error, 1)
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := ws.Accept(w, r, ws.AcceptOptions{})
			if err != nil {
				done <- err
				return
			}
			defer c.Close(ws.CloseNormal, "")
			<-send
			done <- c.WriteMessage(ws.OpText, []byte("alive"))
		}))
		defer s.Close()
		timeouts := config.Timeouts{Connect: 30 * time.Millisecond, FirstByte: 60 * time.Millisecond, Total: 90 * time.Millisecond, Idle: 90 * time.Millisecond}
		_, budget := newProvider(t)
		p := New(timeouts, config.DefaultWebSocket(), budget)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		c, _, err := p.Dial(ctx, request(s.URL))
		cancel()
		if err != nil {
			close(send)
			<-done
			t.Fatal(err)
		}
		defer c.Close(ws.CloseNormal, "")
		<-time.After(2 * timeouts.Total)
		close(send)
		readCtx, readCancel := context.WithTimeout(context.Background(), time.Second)
		defer readCancel()
		msg, err := c.ReadOwnedMessage(readCtx)
		if err != nil {
			t.Fatal(err)
		}
		defer msg.Release()
		if string(msg.Payload) != "alive" {
			t.Fatalf("会话负载被改变: %q", msg.Payload)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

func TestRealtimeProviderSecureURL(t *testing.T) {
	for _, scheme := range []string{"https", "wss"} {
		got, err := upstreamURL(scheme+"://example.test/prefix/api-ws/v1/realtime", "qwen+voice")
		if err != nil || got != "wss://example.test/prefix/api-ws/v1/realtime?model=qwen%2Bvoice" {
			t.Fatalf("TLS scheme或model转义被改变: %q %v", got, err)
		}
	}
}
