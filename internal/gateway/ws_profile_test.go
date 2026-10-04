package gateway

import (
	"bytes"
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
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/obs"
	"github.com/yobo2u/omugw/internal/protocol/openaiwire"
	"github.com/yobo2u/omugw/internal/provider"
	oaws "github.com/yobo2u/omugw/internal/provider/openairealtime"
	"github.com/yobo2u/omugw/internal/router"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

// 同一个真实 close 在不同协议下必须独立结算，不能把 DS 的已知文案当通用规则。
func TestWSProfilesDoNotShareCloseClassification(t *testing.T) {
	for _, profile := range []struct {
		name string
		p    wsProfile
		want canonical.ErrorClass
	}{
		{"dashscope", dashScopeRealtimeProfile(), canonical.ClassRateLimit},
		{"independent", wsProfile{classifyClose: func(uint16, string) *canonical.Error {
			return canonical.Newf(canonical.ClassInternal, "独立协议未分类关闭")
		}}, canonical.ClassInternal},
	} {
		for _, phase := range []string{"attempt", "attempt-registry-race", "relay", "registry-race"} {
			t.Run(profile.name+"/"+phase, func(t *testing.T) {
				const reason = "To many requests. private reason"
				var classifications atomic.Int32
				classify := func(code uint16, raw string) *canonical.Error {
					classifications.Add(1)
					if code != 1011 || raw != reason {
						t.Error("分类晚于 Release 或用了另一条关闭")
					}
					return profile.p.classifyClose(code, raw)
				}
				b := wsTestBudget(t, 1<<20)
				r := newWSRegistry(1)
				s, err := r.Register(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				up, server := wsTestLink(t, true, b, 1<<18, 0, nil)
				if !s.Attach(up) {
					t.Fatal("未登记上游")
				}
				var got error
				if strings.HasPrefix(phase, "attempt") {
					server.send(t, ws.OpClose, ws.EncodeClosePayload(1011, reason))
					_, rawErr := up.ReadOwnedMessage(context.Background())
					failure := wsUpstreamResult(rawErr, classify)
					got = wsAttemptError(failure)
					var shutdown chan error
					if phase == "attempt-registry-race" {
						shutdown = make(chan error, 1)
						go func() { shutdown <- r.Shutdown(context.Background()) }()
						awaitWSTest(t, s.shutdown.finished)
					}
					retireWSAttempt(s, up, failure)
					if shutdown != nil {
						s.Done()
						if err := receiveWSTest(t, shutdown); err != nil {
							t.Fatal(err)
						}
					}
				} else {
					var gate *wsTestGate
					down, client := wsTestLink(t, false, b, 1<<18, 0, func(c net.Conn) net.Conn {
						gate = newWSTestGate(c, false)
						return gate
					})
					t.Cleanup(gate.unblock)
					if !s.Attach(down) {
						t.Fatal("未登记下游")
					}
					server.send(t, ws.OpText, []byte(wsTestInitial))
					initial, err := up.ReadOwnedMessage(context.Background())
					if err != nil {
						t.Fatal(err)
					}
					u := newWSUsage(nil, "test", "test")
					done := make(chan error, 1)
					go func() {
						done <- relayWS(s.Context(), down, up, initial, &dashScopeWSObserver{usage: u}, classify, 0)
					}()
					_ = client.read(t)
					if phase == "registry-race" {
						gate.armed.Store(true)
					}
					server.send(t, ws.OpClose, ws.EncodeClosePayload(1011, reason))
					var shutdown chan error
					if phase == "registry-race" {
						awaitWSTest(t, gate.entered)
						shutdown = make(chan error, 1)
						go func() { shutdown <- r.Shutdown(context.Background()) }()
						awaitWSTest(t, s.shutdown.started)
						gate.unblock()
					}
					assertWSTestClose(t, client, 1011, reason)
					got = receiveWSTest(t, done)
					if !u.finished {
						t.Error("relay 返回前没有结束观测")
					}
					s.Done()
					if shutdown != nil {
						if err := receiveWSTest(t, shutdown); err != nil {
							t.Fatal(err)
						}
					}
				}
				s.Done()
				var failure *canonical.Error
				if !errors.As(got, &failure) || failure.Class != profile.want {
					t.Errorf("协议分类串线: got=%v want=%s", got, profile.want)
				}
				if got != nil && strings.Contains(got.Error(), "private") {
					t.Error("分类持有原关闭原因")
				}
				if b.Used() != 0 {
					t.Errorf("关闭所有权未归还: %d", b.Used())
				}
				if classifications.Load() != 1 {
					t.Errorf("安全分类被重复计算或绕过: %d", classifications.Load())
				}
			})
		}
	}
}

// Dial 可与封口同时交回连接和受控 close 错误；放弃 Attach 不能遗失旧错误所有权。
func TestWSProfileRejectedAttachReleasesDialClose(t *testing.T) {
	d := wsHandlerDeps(t, "http://127.0.0.1:0")
	up, server := wsTestLink(t, true, d.Budget, 1<<18, 0, nil)
	server.send(t, ws.OpClose, ws.EncodeClosePayload(1011, "To many requests. private reason"))
	_, closeErr := up.ReadOwnedMessage(context.Background())
	defer releaseWSRelayError(closeErr)
	if d.Budget.Used() == 0 {
		t.Fatal("没有取得受控 CloseError")
	}
	entered := make(chan struct{})
	d.Providers["ep"] = wsDialFunc(func(ctx context.Context, _ provider.Request) (*ws.Conn, *http.Response, error) {
		close(entered)
		notice := ctx.Value(wsShutdownKey{}).(*wsShutdownNotice)
		<-notice.finished
		return up, nil, closeErr
	})
	w := &wsRejectHijacker{ResponseRecorder: httptest.NewRecorder()}
	done := make(chan struct{})
	go func() {
		defer close(done)
		NewDashScopeRealtimeHandler(d).ServeHTTP(w, wsHandlerRequest())
	}()
	awaitWSTest(t, entered)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := d.Registry.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	awaitWSTest(t, done)
	if d.Budget.Used() != 0 {
		t.Errorf("Attach 失败覆盖旧 close 导致预算泄漏: %d", d.Budget.Used())
	}
	if w.Code != 503 || w.calls != 0 {
		t.Fatal("关停被上游分类覆盖或仍尝试升级")
	}
	for _, st := range d.Pools["pool"].Stats() {
		if st.ConsecutiveFails != 0 || st.Picks > 1 {
			t.Fatal("关停借用未正确结算")
		}
	}
}

type wsTestObserver struct {
	payloads []string
	finished int
}

func (o *wsTestObserver) Observe(payload []byte) error {
	o.payloads = append(o.payloads, string(payload))
	if string(payload) == `{"fixture":"invalid"}` {
		return errWSRelayPolicy
	}
	return nil
}

func (o *wsTestObserver) Finish() { o.finished++ }

// 测试协议故意不使用 DS 事件包络；只有每项 profile 接线均正确才能完成两段真实 TCP。
// 坐标借用已有 OpenAI 枚举，但此夹具不是 GA observer/构造器或真实能力证据。
func TestWSProfileBindsHandlerProtocolFacts(t *testing.T) {
	const ready = ` {"fixture":"ready","future":[null,false,0]} `
	const unknown = `{"type":"future.event","unknown":true}`
	const reason = "To many requests. private reason"
	for _, mode := range []string{"success", "headers", "path", "bad-ready", "binary-ready", "close-before-ready", "observer-reject", "foreign-target"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			upDone := make(chan error, 2)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var upstreamErr error
				defer func() { upDone <- upstreamErr }()
				calls.Add(1)
				if r.URL.Path != "/v1/realtime" || r.URL.Query().Get("model") != "real-model" || r.Header.Get("Authorization") != "Bearer synthetic-a" {
					upstreamErr = errors.New("出站路径/真模型/鉴权未正确绑定")
					return
				}
				c, err := ws.Accept(w, r, ws.AcceptOptions{MaxPayload: 1 << 20, Idle: time.Second, WriteTimeout: time.Second})
				if err != nil {
					upstreamErr = err
					return
				}
				defer c.Close(1000, "")
				if mode == "close-before-ready" {
					_ = c.Close(1011, reason)
					return
				}
				op, first := ws.OpText, ready
				if mode == "bad-ready" {
					first = wsTestInitial
				}
				if mode == "binary-ready" {
					op = ws.OpBinary
				}
				if err := c.WriteMessage(op, []byte(first)); err != nil {
					upstreamErr = err
					return
				}
				if mode != "success" && mode != "observer-reject" {
					return
				}
				// 第二条事件依赖 101 后的客户端输入，误预读两条就会因果死锁。
				op, payload, err := c.ReadMessage()
				if err != nil || op != ws.OpBinary || !bytes.Equal(payload, []byte{0, 255, 128}) {
					upstreamErr = errors.New("下游 binary 未原样交付")
					return
				}
				if mode == "observer-reject" {
					_ = c.WriteMessage(ws.OpText, []byte(`{"fixture":"invalid"}`))
					_, _, _ = c.ReadMessage()
					return
				}
				_ = c.WriteMessage(ws.OpText, []byte(unknown))
				_ = c.WriteMessage(op, payload)
				_ = c.Close(1011, reason)
			}))
			defer up.Close()
			d := wsHandlerDeps(t, up.URL)
			target := router.Target{Kind: degrade.ProviderOpenAIRealtime, Endpoint: "ep", BaseURL: up.URL, UpstreamModel: "real-model", CredentialPool: "pool"}
			if mode == "foreign-target" {
				target.Kind = degrade.ProviderDashScopeWSRealtime
			}
			var err error
			d.Router, err = router.New([]router.Rule{{Match: "real-model", Targets: []router.Target{target}}})
			if err != nil {
				t.Fatal(err)
			}
			if mode != "foreign-target" {
				d.Providers["ep"] = oaws.New(d.Timeouts, d.Limits, d.Budget)
			}
			observer := &wsTestObserver{}
			p := wsProfile{
				inbound:  degrade.Inbound{Protocol: degrade.ProtoOpenAIRealtime, Endpoint: degrade.EndpointOpenAIRealtime},
				outbound: degrade.ProviderOpenAIRealtime,
				validateHeaders: func(h http.Header) error {
					if h.Get("X-Test-Profile") != "allowed" {
						return canonical.Newf(canonical.ClassBadRequest, "测试协议头不合法")
					}
					return nil
				},
				checkReady: func(payload []byte) error {
					if string(payload) != ready {
						return errWSRelayPolicy
					}
					return nil
				},
				encodeError: openaiwire.EncodeError,
				newObserver: func(m *obs.Metrics, inbound, outbound string) wsEventObserver {
					if m != d.Metrics || inbound != "openai.realtime" || outbound != "openai.realtime" {
						t.Error("观测身份串线")
					}
					return observer
				},
				classifyClose: func(uint16, string) *canonical.Error {
					return canonical.Newf(canonical.ClassInternal, "测试协议未知关闭")
				},
			}
			d.Matrix = degrade.NewMatrix()
			// 两条测试路径都全兑现，确保 foreign-target 是被身份隔离而非未投放碰巧挡住。
			for _, outbound := range []degrade.Provider{degrade.ProviderOpenAIRealtime, degrade.ProviderDashScopeWSRealtime} {
				caps := degrade.ExpressibleSet(p.inbound.Protocol)
				if err := d.Matrix.Add(degrade.NewRoute(p.inbound.Protocol, outbound).Pass(caps...).Redeem(p.inbound.Endpoint, caps...).MarkHomogeneous().Build()); err != nil {
					t.Fatal(err)
				}
			}
			h := &WSHandler{d: d, profile: p}
			if err := checkWSDoor(d.Matrix, h); err != nil {
				t.Fatal("独立测试整门批准未使用 profile", err)
			}
			// 仅另一出站被批准不能替当前门顶账。
			foreignOnly := degrade.NewMatrix()
			caps := degrade.ExpressibleSet(p.inbound.Protocol)
			if err := foreignOnly.Add(degrade.NewRoute(p.inbound.Protocol, degrade.ProviderDashScopeWSRealtime).Pass(caps...).Redeem(p.inbound.Endpoint, caps...).Build()); err != nil {
				t.Fatal(err)
			}
			if err := checkWSDoor(foreignOnly, h); err == nil {
				t.Fatal("checkWSDoor 错绑 DS 出站")
			}
			done := make(chan struct{}, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer func() { done <- struct{}{} }()
				h.ServeHTTP(w, r)
			}))
			defer srv.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			path := "/v1/realtime"
			if mode == "path" {
				path = "/api-ws/v1/realtime"
			}
			header := http.Header{"Authorization": {"Bearer synthetic-key"}, "X-Test-Profile": {"allowed"}}
			if mode == "headers" {
				header.Del("X-Test-Profile")
			}
			c, resp, dialErr := ws.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+path+"?model=real-model", ws.DialOptions{Header: header, MaxPayload: 1 << 20, WriteTimeout: time.Second})
			if resp != nil && resp.Body != nil {
				defer resp.Body.Close()
			}
			if c != nil {
				defer c.Close(1000, "")
			}
			active := mode == "success" || mode == "observer-reject"
			if active {
				if dialErr != nil {
					t.Fatal("独立协议未能升级", dialErr)
				}
				wsReadLiteral(t, c, ready)
				if err := c.WriteMessage(ws.OpBinary, []byte{0, 255, 128}); err != nil {
					t.Fatal(err)
				}
				code, closeReason := uint16(1008), "invalid event correlation"
				if mode == "success" {
					wsReadLiteral(t, c, unknown)
					m, err := c.ReadOwnedMessage(ctx)
					if err != nil {
						releaseWSRelayError(err)
						t.Fatal("缺少上游 binary")
					}
					ok := m.Opcode == ws.OpBinary && bytes.Equal(m.Payload, []byte{0, 255, 128})
					m.Release()
					if !ok {
						t.Fatal("上游 binary 未原样交付")
					}
					code, closeReason = 1011, reason
				}
				m, err := c.ReadOwnedMessage(ctx)
				if m != nil {
					m.Release()
				}
				var closed *ws.CloseError
				if !errors.As(err, &closed) {
					t.Fatal("策略拒绝未结束，或原 close 丢失")
				}
				ok := closed.Code == code && closed.Reason == closeReason
				closed.Release()
				if !ok {
					t.Fatal("线上关闭被改写")
				}
			} else {
				if dialErr == nil || resp == nil || resp.StatusCode == 101 {
					t.Fatal("错误协议事实被接受")
				}
				body, err := io.ReadAll(resp.Body)
				if err != nil || !bytes.Contains(body, []byte(`"error":`)) || bytes.Contains(body, []byte("private")) {
					t.Fatal("未使用安全的 profile 错误信封")
				}
			}
			awaitWSTest(t, done)
			if calls.Load() > 0 {
				if err := receiveWSTest(t, upDone); err != nil {
					t.Fatal(err)
				}
			}
			wantCalls := int32(1)
			if mode == "headers" || mode == "path" || mode == "foreign-target" {
				wantCalls = 0
			}
			if calls.Load() != wantCalls || d.Budget.Used() != 0 {
				t.Fatal("多拨/漏拨上游或预算未归还")
			}
			for _, st := range d.Pools["pool"].Stats() {
				if st.ConsecutiveFails != 0 || st.Picks > int(wantCalls) {
					t.Fatal("独立协议 close 被 DS 限流污染或出现额外凭据借用")
				}
			}
			if active {
				last := unknown
				if mode == "observer-reject" {
					last = `{"fixture":"invalid"}`
				}
				if !reflect.DeepEqual(observer.payloads, []string{ready, last}) || observer.finished != 1 {
					t.Fatal("初始/后续上游观测遗漏、binary 被误观测或未 join")
				}
			} else if len(observer.payloads) != 0 || observer.finished != 0 {
				t.Fatal("未就绪请求进入 relay 观测")
			}
			if err := d.Registry.Shutdown(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWSProfileCloseDirectionAndIncompleteMessage(t *testing.T) {
	for _, fromUpstream := range []bool{false, true} {
		t.Run(fmt.Sprint("upstream=", fromUpstream), func(t *testing.T) {
			b := wsTestBudget(t, 4096)
			down, client := wsTestLink(t, false, b, 1024, 0, nil)
			up, server := wsTestLink(t, true, b, 1024, 0, nil)
			var calls atomic.Int32
			classify := func(uint16, string) *canonical.Error { calls.Add(1); return nil }
			done := make(chan error, 1)
			go func() { done <- relayWS(context.Background(), down, up, nil, nil, classify, 0) }()
			source, other := client, server
			if fromUpstream {
				source, other = server, client
			}
			if err := ws.WriteFrame(source.Conn, ws.Frame{Opcode: ws.OpBinary, Payload: []byte{1}}, source.masked); err != nil {
				t.Fatal(err)
			}
			source.send(t, ws.OpClose, ws.EncodeClosePayload(1000, "private incomplete"))
			assertWSTestClose(t, other, 1000, "private incomplete")
			if err := receiveWSTest(t, done); !errors.Is(err, errWSRelayIncomplete) {
				t.Fatal("profile 无分类不能抹掉未完成分片", err)
			}
			want := int32(0)
			if fromUpstream {
				want = 1
			}
			if calls.Load() != want || b.Used() != 0 {
				t.Fatal("下游 close 错用上游分类或遗留所有权")
			}
		})
	}
}
