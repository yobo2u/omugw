package openairealtime

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
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
	"github.com/yobo2u/omugw/internal/protocol/openaiwire"
	"github.com/yobo2u/omugw/internal/provider"
	"github.com/yobo2u/omugw/internal/router"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

var _ provider.StreamProvider = (*Provider)(nil)

func request(base string) provider.Request {
	return provider.Request{
		Target:     router.Target{Kind: degrade.ProviderOpenAIRealtime, BaseURL: base, UpstreamModel: "gpt-realtime-2.1"},
		Credential: credential.Credential{Secret: "upstream-secret"},
		Inbound:    degrade.Inbound{Protocol: degrade.ProtoOpenAIRealtime, Endpoint: degrade.EndpointOpenAIRealtime},
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

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestOpenAIRealtimeProviderHandshake(t *testing.T) {
	for _, auth := range []http.Header{{"Authorization": {"Bearer client-secret"}}, {"api-key": {"client-secret"}}} {
		t.Run(fmt.Sprint(reflect.ValueOf(auth).MapKeys()), func(t *testing.T) {
			seen, done := make(chan *http.Request, 1), make(chan error, 1)
			// 非规范空白和未知字段钉住 Provider 不窥探、重编码或吃掉首条应用消息。
			const initial = "{ \"type\":\"session.created\", \"unknown\": [null,false,0,{}] }\n"
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen <- r
				c, err := ws.Accept(w, r, ws.AcceptOptions{})
				if err != nil {
					done <- err
					return
				}
				defer c.Close(ws.CloseNormal, "")
				done <- c.WriteMessage(ws.OpText, []byte(initial))
			}))
			defer s.Close()
			r := request(s.URL)
			r.Header = http.Header{
				"oPeNaI-SaFeTy-IdEnTiFiEr": {"hashed-user"}, "User-Agent": {"client-agent"},
				"OpenAI-Organization": {"org-secret"}, "OpenAI-Project": {"project-secret"},
				"Origin": {"https://sdk.test"}, "Cookie": {"cookie-secret"},
				"Sec-WebSocket-Extensions": {"permessage-deflate"},
				"X-DashScope-WorkSpace":    {"tenant-secret"}, "X-DashScope-DataInspection": {"enable"},
				"X-Unknown": {"extra-secret"}, "Host": {"foreign-secret.test"},
				"Sec-WebSocket-Key": {"client-secret"}, "Connection": {"client-secret"},
				// 非 ASCII 近形头不能绕过预检规则成为白名单成员。
				"OpenAI-ſafety-Identifier": nil,
			}
			for k, v := range auth {
				r.Header[k] = v
			}
			before := r.Header.Clone()
			p, budget := newProvider(t)
			ctx := testContext(t)
			c, resp, err := p.Dial(ctx, r)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close(ws.CloseNormal, "")
			if p.Kind() != degrade.ProviderOpenAIRealtime || resp.StatusCode != 101 {
				t.Fatal("未使用 OpenAI 同协议握手")
			}
			got := <-seen
			if got.Method != "GET" || got.URL.RequestURI() != "/v1/realtime?model=gpt-realtime-2.1" || got.Host != strings.TrimPrefix(s.URL, "http://") {
				t.Fatalf("错误握手目标: %s %s", got.Method, got.URL)
			}
			want := map[string]string{"Authorization": "Bearer upstream-secret", "User-Agent": "omugw", "Openai-Safety-Identifier": "hashed-user", "Upgrade": "websocket", "Connection": "Upgrade", "Sec-Websocket-Version": "13"}
			for k, v := range want {
				if !reflect.DeepEqual(got.Header.Values(k), []string{v}) {
					t.Errorf("头 %s 未按白名单重建", k)
				}
			}
			for k := range got.Header {
				if _, ok := want[k]; !ok && k != "Sec-Websocket-Key" {
					t.Errorf("透传了非白名单头: %s", k)
				}
			}
			if !reflect.DeepEqual(before, r.Header) {
				t.Fatal("修改了调用方头")
			}
			msg, err := c.ReadOwnedMessage(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if msg.Opcode != ws.OpText || string(msg.Payload) != initial || budget.Used() == 0 {
				t.Fatal("首消息字节或共享预算未保全")
			}
			msg.Release()
			if budget.Used() != 0 {
				t.Fatal("消息释放未归还预算")
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOpenAIRealtimeProviderRejectsBetaAndSubprotocol(t *testing.T) {
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
		{"Beta", http.Header{"OpenAI-Beta": {"realtime=v1"}}},
		{"Beta大小写", http.Header{"oPeNaI-bEtA": {"other-secret"}}},
		{"Beta空值", http.Header{"OpenAI-Beta": {""}}},
		{"Beta空列表", http.Header{"OpenAI-Beta": nil}},
		{"Beta重复", http.Header{"OpenAI-Beta": {"a", "b"}}},
		{"Beta大小写重复", http.Header{"OpenAI-Beta": {""}, "openai-beta": {"realtime=v1"}}},
		{"浏览器密钥", http.Header{"Sec-WebSocket-Protocol": {"openai-insecure-api-key.client-secret"}}},
		{"任意子协议", http.Header{"sec-websocket-protocol": {"realtime"}}},
		{"空子协议", http.Header{"Sec-WebSocket-Protocol": {""}}},
		{"空子协议列表", http.Header{"Sec-WebSocket-Protocol": nil}},
		{"重复子协议", http.Header{"Sec-WebSocket-Protocol": {"realtime", "secret"}}},
		{"鉴权重复", http.Header{"Authorization": {"a", "b"}}},
		{"鉴权大小写重复", http.Header{"Authorization": {"a"}, "authorization": {"b"}}},
		{"双鉴权", http.Header{"Authorization": {"a"}, "Api-Key": {"b"}}},
		{"ApiKey重复", http.Header{"Api-Key": {"a", "b"}}},
		{"ApiKey大小写重复", http.Header{"Api-Key": {"a"}, "api-key": {"b"}}},
		{"鉴权控制字符", http.Header{"Authorization": {"client-secret\r\nX: y"}}},
		{"ApiKey控制字符", http.Header{"Api-Key": {"client-secret\t"}}},
		{"安全标识重复", http.Header{"OpenAI-Safety-Identifier": {"a", "b"}}},
		{"安全标识大小写重复", http.Header{"OpenAI-Safety-Identifier": {"a"}, "openai-safety-identifier": {"b"}}},
		{"安全标识空列表", http.Header{"OpenAI-Safety-Identifier": nil}},
		{"安全标识换行", http.Header{"OpenAI-Safety-Identifier": {"secret\r\nX: y"}}},
		{"安全标识TAB", http.Header{"OpenAI-Safety-Identifier": {"secret\t"}}},
		{"安全标识DEL", http.Header{"OpenAI-Safety-Identifier": {"secret\x7f"}}},
		{"安全标识Unicode控制字符", http.Header{"OpenAI-Safety-Identifier": {"secret\u0085"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateHeaders(tt.h); err == nil || canonical.AsError(err).Class != canonical.ClassBadRequest {
				t.Fatalf("非法头未在预检拒绝: %v", err)
			}
			r := request(s.URL)
			r.Header = tt.h
			c, resp, err := p.Dial(testContext(t), r)
			assertRejected(t, c, resp, err)
		})
	}
	for _, h := range []http.Header{nil, {"Origin": {"https://sdk.test"}, "Sec-WebSocket-Extensions": {"permessage-deflate"}}, {"Api-Key": {"client-secret"}}, {"OpenAI-Safety-Identifier": {""}}, {"OpenAI-Organization": {"a", "b"}, "OpenAI-Project": {"x"}}} {
		if err := ValidateHeaders(h); err != nil {
			t.Fatalf("合法头被拒绝: %v", err)
		}
	}
	for _, change := range []func(*provider.Request){
		func(r *provider.Request) { r.Target.Kind = degrade.ProviderDashScopeWSRealtime },
		func(r *provider.Request) { r.Inbound.Protocol = degrade.ProtoDashScopeRealtime },
		func(r *provider.Request) { r.Inbound.Endpoint = degrade.EndpointDashScopeRealtime },
		func(r *provider.Request) { r.Inbound = degrade.Inbound{} },
		func(r *provider.Request) { r.Target.UpstreamModel = "" },
		func(r *provider.Request) { r.Credential.Secret = "" },
		func(r *provider.Request) { r.Credential.Secret = "secret\r\nX: y" },
	} {
		r := request(s.URL)
		change(&r)
		c, resp, err := p.Dial(testContext(t), r)
		assertRejected(t, c, resp, err)
	}
	for _, invalid := range []*Provider{
		New(config.Default().Timeouts, config.DefaultWebSocket(), nil),
		New(config.Timeouts{}, config.DefaultWebSocket(), budget),
		New(config.Default().Timeouts, config.WebSocket{}, budget),
		New(config.Default().Timeouts, config.WebSocket{MaxMessageBytes: 4, MaxSessions: 1, MaxBufferedBytes: 8}, budget),
	} {
		c, resp, err := invalid.Dial(testContext(t), request(s.URL))
		assertRejected(t, c, resp, err)
	}
	if dials.Load() != 0 {
		t.Fatalf("非法请求发生物理拨号: %d", dials.Load())
	}
}

func TestOpenAIRealtimeProviderURL(t *testing.T) {
	for _, tt := range []struct{ suffix, path string }{
		{"", "/v1/realtime"}, {"/", "/v1/realtime"},
		{"/prefix/", "/prefix/v1/realtime"}, {"/v1", "/v1/v1/realtime"},
		{"/prefix/v1/realtime", "/prefix/v1/realtime"}, {"/prefix/v1/realtime/", "/prefix/v1/realtime"},
		{"/a%2Fb", "/a%2Fb/v1/realtime"}, {"/a%2Fb/v1/realtime", "/a%2Fb/v1/realtime"},
	} {
		for _, scheme := range []string{"http", "ws"} {
			t.Run(scheme+tt.suffix, func(t *testing.T) {
				seen := make(chan *http.Request, 1)
				s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen <- r; w.WriteHeader(401) }))
				defer s.Close()
				r := request(strings.Replace(s.URL, "http:", scheme+":", 1) + tt.suffix)
				r.Target.UpstreamModel = "model /+?&=#中文"
				p, _ := newProvider(t)
				_, resp, err := p.Dial(testContext(t), r)
				if err == nil || resp == nil || resp.StatusCode != 401 {
					t.Fatalf("未到达本地上游: %v", err)
				}
				got := <-seen
				if got.URL.EscapedPath() != tt.path || got.URL.RawQuery != "model=model+%2F%2B%3F%26%3D%23%E4%B8%AD%E6%96%87" || len(got.URL.Query()["model"]) != 1 || len(got.URL.Query()) != 1 {
					t.Fatalf("固定路径或唯一模型转义错误: %s", got.URL)
				}
			})
		}
	}
	for _, scheme := range []string{"https", "wss"} {
		got, err := upstreamURL(scheme+"://example.test/a%2Fb/v1/realtime", "gpt+realtime")
		if err != nil || got != "wss://example.test/a%2Fb/v1/realtime?model=gpt%2Brealtime" {
			t.Fatalf("TLS或前缀转义错误: %q %v", got, err)
		}
	}
	var dials atomic.Int32
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	s.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			dials.Add(1)
		}
	}
	s.Start()
	defer s.Close()
	p, _ := newProvider(t)
	for _, base := range []string{
		s.URL + "?secret", s.URL + "?", s.URL + "#secret", s.URL + "#",
		strings.Replace(s.URL, "://", "://user:secret@", 1),
		"ws:secret", "ws:///secret", "ftp://example.test/secret", "ws://bad host/secret", "ws://example.test/%zz",
	} {
		c, resp, err := p.Dial(testContext(t), request(base))
		assertRejected(t, c, resp, err)
	}
	if dials.Load() != 0 {
		t.Fatal("非法URL发生拨号")
	}
	t.Run("TLS证书必须受信", func(t *testing.T) {
		var hits atomic.Int32
		tlsServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.WriteHeader(401) }))
		tlsServer.Config.ErrorLog = log.New(io.Discard, "", 0)
		tlsServer.StartTLS()
		defer tlsServer.Close()
		for _, scheme := range []string{"https", "wss"} {
			c, resp, err := p.Dial(testContext(t), request(strings.Replace(tlsServer.URL, "https:", scheme+":", 1)))
			assertRejected(t, c, resp, err)
		}
		if hits.Load() != 0 {
			t.Fatal("未信任证书的TLS连接抵达HTTP处理器")
		}
	})
}

func assertSafe(t *testing.T, text string) {
	t.Helper()
	for _, bad := range []string{"secret", "body-marker", "param-marker", "request-marker", "unknown-code", "http://", "https://", "ws://", "wss://", "127.0.0.1"} {
		if strings.Contains(text, bad) {
			t.Fatalf("不安全错误输出包含 %q", bad)
		}
	}
}

func assertRejected(t *testing.T, c *ws.Conn, resp *http.Response, err error) {
	t.Helper()
	if c != nil {
		c.Close(ws.CloseNormal, "")
		t.Fatal("拒绝请求仍返回连接")
	}
	if err == nil || resp != nil {
		t.Fatalf("拒绝结果错误: resp=%v err=%v", resp, err)
	}
	ce := canonical.AsError(err)
	if ce.Retryable || errors.Unwrap(err) != nil {
		t.Fatal("本地失败不可重试或携带原始cause")
	}
	_, body, h := openaiwire.EncodeError(ce)
	assertSafe(t, string(body)+fmt.Sprint(h))
}

func TestOpenAIRealtimeProviderFailureBounds(t *testing.T) {
	const envelope = `{"error":{"message":"body-marker secret http://example.test","type":"unknown-secret","code":"unknown-code","param":"param-marker"},"request_id":"request-marker"}`
	const quota = `{"error":{"code":"insufficient_quota"}}`
	for _, tt := range []struct {
		name   string
		status int
		body   string
		class  canonical.ErrorClass
		retry  bool
	}{
		{"401", 401, envelope, canonical.ClassAuth, true},
		{"403", 403, envelope, canonical.ClassAuth, true},
		{"429", 429, envelope, canonical.ClassRateLimit, true},
		{"未知503", 503, envelope, canonical.ClassUpstreamUnavailable, true},
		{"HTML502", 502, "<html>body-marker secret</html>", canonical.ClassUpstreamUnavailable, true},
		{"未知599", 599, envelope, canonical.ClassUpstreamUnavailable, true},
		{"quota", 400, quota, canonical.ClassQuota, true},
		{"context", 400, `{"error":{"code":"context_length_exceeded"}}`, canonical.ClassContextLength, false},
		{"filter", 400, `{"error":{"code":"content_filter"}}`, canonical.ClassContentFilter, false},
		{"未知400", 400, envelope, canonical.ClassBadRequest, false},
		{"重定向", 302, quota, canonical.ClassInternal, false},
		{"意外200", 200, quota, canonical.ClassInternal, false},
		{"坏101", 101, quota, canonical.ClassInternal, false},
		{"恰好64KiB", 400, strings.Repeat(" ", (64<<10)-len(quota)) + quota, canonical.ClassQuota, true},
		{"超过64KiB一字节", 400, strings.Repeat(" ", (64<<10)-len(quota)+1) + quota, canonical.ClassBadRequest, false},
		{"超大体", 503, strings.Repeat("body-marker secret", 8192), canonical.ClassUpstreamUnavailable, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var redirected atomic.Int32
			dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
			defer dst.Close()
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "7")
				w.Header().Set("X-RateLimit-Remaining-Requests", "0")
				w.Header().Set("X-RateLimit-Reset-Tokens", "secret")
				w.Header().Set("X-Request-ID", "request-marker")
				w.Header().Set("Location", dst.URL+"/secret")
				w.Header().Set("Set-Cookie", "secret")
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer s.Close()
			p, budget := newProvider(t)
			c, resp, err := p.Dial(testContext(t), request(s.URL))
			if c != nil {
				c.Close(ws.CloseNormal, "")
				t.Fatal("失败握手返回连接")
			}
			if err == nil || resp == nil {
				t.Fatalf("失败丢失状态: %v", err)
			}
			ce := canonical.AsError(err)
			if ce.Class != tt.class || ce.Retryable != tt.retry || ce.UpstreamStatus != tt.status || resp.StatusCode != tt.status {
				t.Fatalf("分类错误: %+v status=%d", ce, resp.StatusCode)
			}
			wantHeaders := map[string]string{}
			if tt.status >= 400 {
				wantHeaders = map[string]string{"Retry-After": "7", "X-RateLimit-Remaining-Requests": "0"}
				if ce.RetryAfter != 7*time.Second {
					t.Fatal("Retry-After丢失")
				}
			}
			if !reflect.DeepEqual(ce.Headers(), wantHeaders) {
				t.Fatalf("清洗后限流头错误: %v", ce.Headers())
			}
			if ce.Param != "" || ce.UpstreamRequestID != "" || errors.Unwrap(err) != nil || resp.Request != nil || resp.TLS != nil || len(resp.Trailer) != 0 {
				t.Fatal("原始元数据未清除")
			}
			_, encoded, headers := openaiwire.EncodeError(ce)
			assertSafe(t, string(encoded)+fmt.Sprint(headers)+fmt.Sprintf("%+v", resp)+ce.Message+ce.UpstreamCode+ce.Param)
			data, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil || len(data) != 0 || redirected.Load() != 0 || budget.Used() != 0 {
				t.Fatalf("原始体、重定向或预算残留: len=%d err=%v redirects=%d used=%d", len(data), readErr, redirected.Load(), budget.Used())
			}
		})
	}
	for _, size := range []int{(64 << 10) - 1, 64 << 10, (64 << 10) + 1} {
		t.Run(fmt.Sprintf("头边界%d", size), func(t *testing.T) {
			s := rawServer(t, func(c net.Conn, r *http.Request) {
				const head = "HTTP/1.1 401 unknown-secret\r\nContent-Length: 0\r\nX-Padding: "
				_, _ = io.WriteString(c, head+strings.Repeat("a", size-len(head)-4)+"\r\n\r\n")
			})
			p, _ := newProvider(t)
			c, resp, err := p.Dial(testContext(t), request(s.URL))
			if size > 64<<10 {
				assertRejected(t, c, resp, err)
				return
			}
			if err == nil || resp == nil || resp.StatusCode != 401 || canonical.AsError(err).Class != canonical.ClassAuth {
				t.Fatalf("合法边界头应完成HTTP解析: %v", err)
			}
			assertSafe(t, fmt.Sprintf("%+v", resp))
		})
	}
	for _, extra := range []string{"Sec-WebSocket-Protocol: secret\r\n", "Sec-WebSocket-Extensions: permessage-deflate\r\n", "Sec-WebSocket-Accept: secret\r\n"} {
		t.Run(strings.Split(extra, ":")[0], func(t *testing.T) {
			s := rawServer(t, func(c net.Conn, r *http.Request) {
				digest := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
				_, _ = io.WriteString(c, "HTTP/1.1 101 secret\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: "+base64.StdEncoding.EncodeToString(digest[:])+"\r\n"+extra+"\r\n")
			})
			p, _ := newProvider(t)
			c, resp, err := p.Dial(testContext(t), request(s.URL))
			if c != nil {
				c.Close(ws.CloseNormal, "")
				t.Fatal("接受了非法协商")
			}
			if err == nil || resp == nil || resp.StatusCode != 101 || canonical.AsError(err).Retryable {
				t.Fatalf("坏101处理错误: %v", err)
			}
			assertSafe(t, fmt.Sprintf("%+v", resp)+err.Error())
		})
	}
	t.Run("到达体上限即关闭原socket", func(t *testing.T) {
		closed := make(chan error, 1)
		s := rawServer(t, func(c net.Conn, r *http.Request) {
			_, _ = io.WriteString(c, "HTTP/1.1 503 Unavailable\r\nContent-Length: 999999999\r\n\r\n"+strings.Repeat("a", (64<<10)+1))
			var b [1]byte
			_, err := c.Read(b[:])
			closed <- err
		})
		p, _ := newProvider(t)
		start := time.Now()
		_, resp, err := p.Dial(testContext(t), request(s.URL))
		if err == nil || resp == nil || time.Since(start) > time.Second {
			t.Fatalf("体上限未及时停止: %v", err)
		}
		if err := <-closed; !errors.Is(err, io.EOF) {
			t.Fatalf("原socket未关闭: %v", err)
		}
	})
	t.Run("消息上限沿用共享预算", func(t *testing.T) {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := ws.Accept(w, r, ws.AcceptOptions{})
			if err != nil {
				t.Error(err)
				return
			}
			defer c.Close(ws.CloseNormal, "")
			_ = c.WriteMessage(ws.OpBinary, []byte("12345"))
		}))
		defer s.Close()
		_, budget := newProvider(t)
		p := New(config.Default().Timeouts, config.WebSocket{MaxMessageBytes: 4, MaxSessions: 1, MaxBufferedBytes: 12}, budget)
		ctx := testContext(t)
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
			t.Fatalf("限额未接线: %v used=%d", err, budget.Used())
		}
	})
}

// Hijack 后显式 join，防止失败断言把测试后台 TCP 连接遗留给下一用例。
func rawServer(t *testing.T, serve func(net.Conn, *http.Request)) *httptest.Server {
	t.Helper()
	done := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		c, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		serve(c, r)
	}))
	t.Cleanup(func() {
		s.Close()
		select {
		case <-done:
		case <-time.After(4 * time.Second):
			t.Error("TCP服务端未退出")
		}
	})
	return s
}

func TestOpenAIRealtimeProviderTimeoutWiring(t *testing.T) {
	t.Run("握手共同截止时间", func(t *testing.T) {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
		defer s.Close()
		p, _ := newProvider(t)
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		defer cancel()
		start := time.Now()
		c, resp, err := p.Dial(ctx, request(s.URL))
		if c != nil {
			c.Close(ws.CloseNormal, "")
			t.Fatal("超时返回连接")
		}
		ce := canonical.AsError(err)
		if resp != nil || ce == nil || ce.Class != canonical.ClassUpstreamUnavailable || !ce.Retryable || time.Since(start) > time.Second {
			t.Fatalf("握手截止未生效: %v", err)
		}
		assertSafe(t, err.Error())
	})
	t.Run("取消不当成超时重试", func(t *testing.T) {
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
		timeouts.Connect = 40 * time.Millisecond
		_, b := newProvider(t)
		p := New(timeouts, config.DefaultWebSocket(), b)
		start := time.Now()
		c, resp, err := p.Dial(testContext(t), request("wss://"+ln.Addr().String()))
		assertRejected(t, c, resp, err)
		if time.Since(start) > time.Second {
			t.Fatal("connect未覆盖TLS")
		}
	})
	for _, tc := range []struct {
		name          string
		connect, idle time.Duration
		write         bool
	}{
		{"idle读取", time.Second, 40 * time.Millisecond, false},
		{"写取connect", 40 * time.Millisecond, time.Second, true},
		{"写取idle", time.Second, 40 * time.Millisecond, true},
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
			ctx := testContext(t)
			c, _, err := p.Dial(ctx, request(s.URL))
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close(ws.CloseNormal, "")
			// 兜底仅供测试失败收尾，断言必须早于此期限。
			timer := time.AfterFunc(2*time.Second, func() { c.Close(ws.CloseNormal, "") })
			defer timer.Stop()
			start := time.Now()
			if tc.write {
				err = c.WriteMessage(ws.OpBinary, make([]byte, 32<<20))
				var ne net.Error
				if !errors.As(err, &ne) || !ne.Timeout() {
					t.Fatalf("慢读端未触发有限写期限: %v", err)
				}
			} else {
				msg, readErr := c.ReadOwnedMessage(ctx)
				if msg != nil {
					msg.Release()
				}
				if !errors.Is(readErr, ws.ErrIdleTimeout) {
					t.Fatalf("idle未接线: %v", readErr)
				}
			}
			if time.Since(start) > 500*time.Millisecond || budget.Used() != 0 {
				t.Fatalf("期限或预算未收敛: used=%d", budget.Used())
			}
		})
	}
	t.Run("total和握手取消不侵入会话", func(t *testing.T) {
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
		timeouts := config.Timeouts{Connect: 40 * time.Millisecond, FirstByte: 80 * time.Millisecond, Total: 120 * time.Millisecond, Idle: 120 * time.Millisecond}
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
		time.Sleep(2 * timeouts.Total)
		close(send)
		msg, err := c.ReadOwnedMessage(testContext(t))
		if err != nil {
			t.Fatal(err)
		}
		if string(msg.Payload) != "alive" {
			t.Fatal("会话负载改变")
		}
		msg.Release()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if budget.Used() != 0 {
			t.Fatal("预算泄漏")
		}
	})
}
