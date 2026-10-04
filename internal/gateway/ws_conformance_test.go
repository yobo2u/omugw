package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/obs"
	"github.com/yobo2u/omugw/internal/testkit"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

// 全部预期为独立字面契约，synthetic 只证明接线机制，不进入生产 routes 或兑现名单。
func wsConformanceFixture(t *testing.T, failed bool) testkit.Fixture {
	t.Helper()
	nodes := []testkit.WSNode{}
	pair := func(send, receive string, point testkit.WSPoint, after string, op ws.Opcode, body string) {
		other := testkit.WSClientReceive
		if point == testkit.WSClientSend {
			other = testkit.WSUpstreamReceive
		}
		a := []string(nil)
		if after != "" {
			a = []string{after}
		}
		nodes = append(nodes,
			testkit.WSNode{ID: send, Point: point, Source: "synthetic", After: a, Kind: "message", Message: &testkit.WSMessage{Opcode: op, Payload: []byte(body)}},
			testkit.WSNode{ID: receive, Point: other, Source: "synthetic", After: []string{send}, Kind: "message", Message: &testkit.WSMessage{Opcode: op, Payload: []byte(body)}, ForwardedFrom: send})
	}
	pair("ready.u", "ready.c", testkit.WSUpstreamSend, "", ws.OpText, wsTestInitial)
	pair("update.c", "update.u", testkit.WSClientSend, "ready.c", ws.OpText, `{"type":"session.update","session":{"unknown":null,"zero":0,"empty":[],"enabled":false}}`)
	pair("unknown.u", "unknown.c", testkit.WSUpstreamSend, "update.u", ws.OpText, `{"type":"future.event", "unknown":{"null":null,"zero":0,"empty":{},"text":"保留"}}`)
	pair("binary.c", "binary.u", testkit.WSClientSend, "unknown.c", ws.OpBinary, "\x00\xff\x10")
	pair("echo.u", "echo.c", testkit.WSUpstreamSend, "binary.u", ws.OpBinary, "\x00\xff\x10")
	pair("error.u", "error.c", testkit.WSUpstreamSend, "echo.c", ws.OpText, `{"type":"error","error":{"type":"invalid_request_error","code":"invalid_value","message":"synthetic literal"},"future":false}`)
	status, outcome := "completed", "completed"
	if failed {
		status, outcome = "failed", "failed"
	}
	body := `{"type":"response.done","response":{"id":"r","status":"` + status + `","usage":{"input_tokens":3,"output_tokens":2}}}`
	pair("done.u", "done.c", testkit.WSUpstreamSend, "error.c", ws.OpText, body)
	code, send, receive, point, other := uint16(1000), "close.c", "close.u", testkit.WSClientSend, testkit.WSUpstreamReceive
	if failed {
		code, send, receive, point, other = 1011, "close.u", "close.c", testkit.WSUpstreamSend, testkit.WSClientReceive
	}
	nodes = append(nodes,
		testkit.WSNode{ID: send, Point: point, Source: "synthetic", After: []string{"done.c"}, Kind: "close", CloseCode: &code},
		testkit.WSNode{ID: receive, Point: other, Source: "synthetic", After: []string{send}, Kind: "close", CloseCode: &code})
	f := testkit.Fixture{Name: "gateway-ws-synthetic", Note: "合成接线因果回放，非上游能力证据", Request: testkit.Request{
		Method: "GET", Path: "/api-ws/v1/realtime", Query: "model=real-model", Headers: map[string]string{"Authorization": "<redacted>"}},
		Response: testkit.Response{Status: 101, WS: &testkit.WSSession{
			Version: 1, ClientProtocol: "dashscope.realtime", ClientVersion: "synthetic-s1",
			Upstream:               testkit.Request{Method: "GET", Path: "/api-ws/v1/realtime", Query: "model=real-model", Headers: map[string]string{"Authorization": "<redacted>", "User-Agent": "omugw", "X-DashScope-WorkSpace": "synthetic-workspace"}},
			UpstreamExpectedStatus: 101, Provenance: testkit.WSProvenance{Kind: "synthetic-negative"}, Nodes: nodes,
			Coverage: []testkit.WSCoverage{{Capability: "streaming", Nodes: []string{"ready.u", "ready.c", "done.u", "done.c"}, Note: "就绪预读与应用终态原样保全"}},
			Outcome:  testkit.WSOutcome{Kind: outcome, Terminal: []testkit.WSTerminal{{Node: "done.c", Namespace: "response", Symbol: "r", IDPointer: "/response/id", StatePointer: "/response/status", State: status}}},
		}}}
	digest, err := testkit.WSContractDigest(*f.Response.WS)
	if err != nil {
		t.Fatal(err)
	}
	f.Response.WS.Provenance.SourceSHA256 = digest
	if err := testkit.ValidateWSSession(f, testkit.DefaultWSLimits()); err != nil {
		t.Fatal(err)
	}
	return f
}

// prelude 已在真实连接独立逐字节核验；tail 仅删这两个节点、关联边与 coverage。
// 不为后续节点制造绑定，也不修改原 fixture，更不能从实际输出反生成预期。
func wsConformanceTail(t *testing.T, f testkit.Fixture) testkit.Fixture {
	t.Helper()
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	var tail testkit.Fixture
	if err := json.Unmarshal(raw, &tail); err != nil {
		t.Fatal(err)
	}
	removed := map[string]bool{"ready.u": true, "ready.c": true}
	s := tail.Response.WS
	s.Nodes = s.Nodes[2:]
	for i := range s.Nodes {
		n := &s.Nodes[i]
		if removed[n.ForwardedFrom] || len(n.Fields) != 0 {
			t.Fatal("prelude 不得承载后续动态规则")
		}
		after := []string(nil)
		for _, id := range n.After {
			if !removed[id] {
				after = append(after, id)
			}
		}
		n.After = after
	}
	for i := range s.Coverage {
		ids := []string(nil)
		for _, id := range s.Coverage[i].Nodes {
			if !removed[id] {
				ids = append(ids, id)
			}
		}
		s.Coverage[i].Nodes = ids
	}
	s.Provenance.SourceSHA256, err = testkit.WSContractDigest(*s)
	if err != nil {
		t.Fatal(err)
	}
	if err := testkit.ValidateWSSession(tail, testkit.DefaultWSLimits()); err != nil {
		t.Fatal(err)
	}
	return tail
}

func TestWSConformanceReplay(t *testing.T) {
	for _, failed := range []bool{false, true} {
		name := "completed"
		if failed {
			name = "failed"
		}
		t.Run(name, func(t *testing.T) {
			f := wsConformanceFixture(t, failed)
			limits := testkit.DefaultWSLimits()
			u, err := testkit.NewWSReplayUpstream(f, limits)
			if err != nil {
				t.Fatal(err)
			}
			us := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer sec1" || r.Header.Get("Cookie") != "" || r.Header.Get("Origin") != "" {
					t.Error("出站凭据未替换或额外秘密头泄漏")
				}
				u.ServeHTTP(w, r)
			}))
			defer us.Close()
			defer u.Close()
			cfg := buildTestConfig(us.URL)
			cfg.Providers[0].Kind = string(degrade.ProviderDashScopeWSRealtime)
			cfg.Models[0].Targets[0].UpstreamModel = "real-model"
			cfg.WebSocket = config.WebSocket{MaxMessageBytes: limits.MessageBytes, MaxBufferedBytes: 8 << 20, MaxSessions: 4}
			d := wsHandlerDeps(t, us.URL)
			reg := prometheus.NewRegistry()
			d.Metrics = obs.NewMetrics(reg)
			b, err := buildWithWS(cfg, wsHandlerMatrix(t, true), d.Metrics, d.Log, degrade.EndpointDashScopeRealtime)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{}, 1)
			gs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer func() { done <- struct{}{} }()
				b.Mux.ServeHTTP(w, r)
			}))
			defer gs.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			defer b.ShutdownWebSockets(ctx)
			type prelude struct {
				c   *ws.Conn
				err error
			}
			ready := make(chan prelude, 1)
			// Dial 还在等待网关 101 时先发唯一 session.created，不能等双端齐备。
			go func() {
				c, err := u.Connection(ctx)
				if err == nil {
					err = c.WriteMessage(ws.OpText, []byte(wsTestInitial))
				}
				ready <- prelude{c, err}
			}()
			c, resp, dialErr := ws.Dial(ctx, "ws"+strings.TrimPrefix(gs.URL, "http")+"/api-ws/v1/realtime?model=real-model", ws.DialOptions{
				Header: http.Header{"Authorization": {"Bearer sk-test-1234567890"}, "X-Dashscope-Workspace": {"synthetic-workspace"}, "Origin": {"https://client.example"}, "Cookie": {"private=synthetic"}}, MaxPayload: limits.MessageBytes})
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			up := receiveWSTest(t, ready)
			if dialErr != nil || up.err != nil {
				if c != nil {
					_ = c.Close(1000, "")
				}
				t.Fatal("因果回放握手/prelude 失败")
			}
			defer c.Close(1000, "")
			wsReadLiteral(t, c, wsTestInitial)
			result, replayErr := testkit.ReplayWS(ctx, wsConformanceTail(t, f), testkit.WSReplayEndpoints{Client: c, Upstream: up.c}, 1, limits)
			if err := b.ShutdownWebSockets(ctx); err != nil {
				t.Fatal(err)
			}
			awaitWSTest(t, done)
			closeErr := u.Close()
			if replayErr != nil || closeErr != nil || u.Err() != nil {
				t.Fatal("回放 driver/上游握手/关闭证据未全部通过", replayErr, closeErr, u.Err())
			}
			closeID := "close.c"
			if failed {
				closeID = "close.u"
			}
			wantOrder := []string{"update.c", "unknown.u", "binary.c", "echo.u", "error.u", "done.u", closeID}
			if result.Outcome.Kind != name || len(result.Nodes) != 14 || !reflect.DeepEqual(result.SendOrder, wantOrder) {
				t.Fatal("独立字面因果/结局预期不符")
			}
			if b.wsBudget.Used() != 0 {
				t.Fatal("因果回放结束后共享预算未归零")
			}
			if wsUsageMetricSum(t, reg, "omugw_tokens_total", map[string]string{"kind": "input", "fidelity": "authoritative"}) != 3 || wsUsageMetricSum(t, reg, "omugw_tokens_total", map[string]string{"kind": "output", "fidelity": "authoritative"}) != 2 {
				t.Fatal("handler 未接入权威用量或断流清零了用量")
			}
			if wsUsageMetricSum(t, reg, "omugw_requests_total", map[string]string{"inbound": "dashscope.realtime", "outbound": "dashscope.ws.realtime"}) != 1 {
				t.Fatal("会话请求计数不符")
			}
			families, err := reg.Gather()
			if err != nil {
				t.Fatal(err)
			}
			for _, metricName := range []string{"omugw_first_byte_seconds", "omugw_request_duration_seconds"} {
				var count uint64
				for _, family := range families {
					if family.GetName() == metricName {
						for _, metric := range family.Metric {
							count += metric.GetHistogram().GetSampleCount()
						}
					}
				}
				if count != 1 {
					t.Fatal("首字节/全程耗时观测缺失或重复")
				}
			}
			if err := testkit.ValidateWSSession(f, limits); err != nil || len(f.Response.WS.Nodes) != 16 {
				t.Fatal("tail 视图修改了原始 fixture")
			}
		})
	}
}
