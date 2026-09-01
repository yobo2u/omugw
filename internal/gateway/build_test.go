package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/obs"
	"github.com/yobo2u/omugw/internal/protocol/dashscopenative"
)

func TestBuiltMux_DashScopeNativeFallback_WithUpstream(t *testing.T) {
	m, err := degrade.Phase1()
	if err != nil {
		t.Fatalf("加载 Phase1 矩阵失败: %v", err)
	}

	reg := prometheus.NewRegistry()
	metrics := obs.NewMetrics(reg)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	var upstreamCalls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"output":{"text":"ok"}}`))
	}))
	defer upstream.Close()

	cfg := config.Config{
		Auth: config.Auth{Keys: []config.AuthKey{{ID: "test", Key: "sk-test-1234567890"}}},
		Credentials: map[string][]config.CredentialSpec{
			"pool1": {{ID: "1", Secret: "sec1"}},
		},
		Providers: []config.ProviderSpec{
			{Endpoint: "ep1", Kind: "dashscope.native", BaseURL: upstream.URL, CredentialPool: "pool1"},
		},
		Models: []config.ModelSpec{
			{Match: "*", Targets: []config.TargetSpec{{Endpoint: "ep1", UpstreamModel: "test-model"}}},
		},
		Timeouts: config.Timeouts{
			Connect: time.Second, FirstByte: 2 * time.Second, Total: 3 * time.Second, Idle: time.Second,
		},
		Limits: config.Limits{
			MaxRequestBytes: 1024 * 1024,
			MaxInlineBytes:  1024 * 1024,
		},
	}

	built, err := Build(cfg, m, metrics, log)
	if err != nil {
		t.Fatalf("构建失败: %v", err)
	}

	tests := []struct {
		name           string
		method         string
		path           string
		expectedStatus int
		expectedCode   string
		expectedMsg    string
		expectUpstream bool
	}{
		{
			// 本用例的请求体是纯文本消息，能力集只有 text_generation——
			// 多模态门已兑现它，请求应当被精确路由接住并打到上游，
			// 而不是被命名空间兜底改写成 501。
			name:           "已投放的多模态端点由精确路由处理，打到上游",
			method:         "POST",
			path:           dashscopenative.MultimodalGenerationPath,
			expectedStatus: http.StatusOK,
			expectedCode:   "",
			expectUpstream: true,
		},
		{
			name:           "未投放的 embedding 端点返回 501",
			method:         "POST",
			path:           "/api/v1/services/embeddings/text-embedding/text-embedding",
			expectedStatus: http.StatusNotImplemented,
			expectedCode:   "Unsupported",
			expectedMsg:    "DashScope Native 端点 /api/v1/services/embeddings/text-embedding/text-embedding 尚未实现",
		},
		{
			name:           "未投放的 rerank 端点返回 501",
			method:         "POST",
			path:           "/api/v1/services/rerank/text-rerank/text-rerank",
			expectedStatus: http.StatusNotImplemented,
			expectedCode:   "Unsupported",
			expectedMsg:    "DashScope Native 端点 /api/v1/services/rerank/text-rerank/text-rerank 尚未实现",
		},
		{
			name:           "已投放的文本端点由精确路由处理，打到上游",
			method:         "POST",
			path:           dashscopenative.TextGenerationPath,
			expectedStatus: http.StatusOK,
			expectedCode:   "",
			expectUpstream: true,
		},
		{
			// 兜底只认 POST。非 POST 请求必须落回框架 404，不能因为兜底的存在
			// 变成 405——405 是在暗示「换个方法就能用」，而这些端点根本不存在。
			name:           "Native 命名空间下的非 POST 请求返回 404",
			method:         "GET",
			path:           "/api/v1/services/aigc/multimodal-generation/generation",
			expectedStatus: http.StatusNotFound,
			expectedCode:   "",
		},
		{
			// 精确注册的文本端点同理：GET 打过去也是 404，而不是 405。
			// 顺带证明兜底没有把已投放端点的方法约束吃掉。
			name:           "已投放端点的非 POST 请求同样返回 404",
			method:         "GET",
			path:           dashscopenative.TextGenerationPath,
			expectedStatus: http.StatusNotFound,
			expectedCode:   "",
		},
		{
			name:           "非 Native 命名空间返回 404",
			method:         "POST",
			path:           "/api/v2/something",
			expectedStatus: http.StatusNotFound,
			expectedCode:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstreamCalls = 0
			req := httptest.NewRequest(tt.method, tt.path, bytes.NewReader([]byte(`{"model":"test-model","input":{"messages":[{"role":"user","content":"hello"}]}}`)))
			req.Header.Set("Authorization", "Bearer sk-test-1234567890")
			rec := httptest.NewRecorder()

			built.Mux.ServeHTTP(rec, req)

			if rec.Code != tt.expectedStatus {
				t.Errorf("期望状态码 %d，实际 %d", tt.expectedStatus, rec.Code)
			}

			if tt.expectUpstream {
				if upstreamCalls != 1 {
					t.Errorf("期望 1 次上游调用，实际 %d", upstreamCalls)
				}
			} else {
				if upstreamCalls != 0 {
					t.Errorf("期望 0 次上游调用，实际 %d", upstreamCalls)
				}
			}

			if tt.expectedCode != "" {
				if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
					t.Errorf("期望 Content-Type application/json，实际 %q", ct)
				}

				var body struct {
					Code string `json:"code"`
					Msg  string `json:"message"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("解析响应体失败: %v", err)
				}
				if body.Code != tt.expectedCode {
					t.Errorf("期望 code %q，实际 %q", tt.expectedCode, body.Code)
				}

				if tt.expectedMsg != "" && body.Msg != tt.expectedMsg {
					t.Errorf("期望 message %q，实际 %q", tt.expectedMsg, body.Msg)
				}

			}
		})
	}

	// 验证指标
	metricsFamilies, err := reg.Gather()
	if err != nil {
		t.Fatalf("收集指标失败: %v", err)
	}
	var notImplementedCount float64
	for _, mf := range metricsFamilies {
		if mf.GetName() == "omugw_not_implemented_total" {
			for _, m := range mf.GetMetric() {
				var inbound, outbound string
				for _, lp := range m.GetLabel() {
					if lp.GetName() == "inbound" {
						inbound = lp.GetValue()
					}
					if lp.GetName() == "outbound" {
						outbound = lp.GetValue()
					}
				}
				if inbound == string(degrade.ProtoDashScopeNative) && outbound == "planned" {
					notImplementedCount += m.GetCounter().GetValue()
				} else {
					t.Errorf("意外的指标标签: inbound=%s, outbound=%s", inbound, outbound)
				}
			}
		}
	}
	// 只剩 embedding 与 rerank 两个用例走兜底：多模态门投放后不再计入，
	// 这个数字下降本身就是「门真的开了」的旁证。
	if notImplementedCount != 2 {
		t.Errorf("期望 2 次未实现指标，实际 %v", notImplementedCount)
	}

}

func TestBuiltMux_HealthOnlyMode_NoFallback(t *testing.T) {
	m := degrade.NewMatrix()
	reg := prometheus.NewRegistry()
	metrics := obs.NewMetrics(reg)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// 空配置 = 仅健康检查模式
	cfg := config.Config{}

	built, err := Build(cfg, m, metrics, log)
	if err != nil {
		t.Fatalf("构建失败: %v", err)
	}

	req := httptest.NewRequest("POST", "/api/v1/services/aigc/multimodal-generation/generation", nil)
	rec := httptest.NewRecorder()

	built.Mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("仅健康检查模式期望 404，实际 %d", rec.Code)
	}
}

// buildTestConfig 组装一份最小可用的全量配置：对账发生在「模型路由已配置」
// 之后，空配置会在对账之前提前返回，测不到闸门。
func buildTestConfig(upstreamURL string) config.Config {
	return config.Config{
		Auth: config.Auth{Keys: []config.AuthKey{{ID: "test", Key: "sk-test-1234567890"}}},
		Credentials: map[string][]config.CredentialSpec{
			"pool1": {{ID: "1", Secret: "sec1"}},
		},
		Providers: []config.ProviderSpec{
			{Endpoint: "ep1", Kind: "openai.compat", BaseURL: upstreamURL, CredentialPool: "pool1"},
		},
		Models: []config.ModelSpec{
			{Match: "*", Targets: []config.TargetSpec{{Endpoint: "ep1", UpstreamModel: "m"}}},
		},
		Timeouts: config.Timeouts{
			Connect: time.Second, FirstByte: 2 * time.Second, Total: 3 * time.Second, Idle: time.Second,
		},
		Limits: config.Limits{MaxRequestBytes: 1024 * 1024, MaxInlineBytes: 1024 * 1024},
	}
}

// TestEveryOpenDoorReachesAHandler 防的是「对账名单与真实注册各说各话」。
//
// 对账表若是手写的第二份清单，它证明的只是「名单与矩阵一致」，而不是
// 「请求真能走到处理器」：把某个 mux.Handle 删掉、名单照旧，对账依然全绿，
// 而那扇门的请求已经悄悄落进 501 兜底。所以要问的不是名单，是 Mux 本身。
func TestEveryOpenDoorReachesAHandler(t *testing.T) {
	m, err := degrade.Phase1()
	if err != nil {
		t.Fatal(err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":{"text":"ok"}}`))
	}))
	defer upstream.Close()

	cfg := buildTestConfig(upstream.URL)
	cfg.Providers[0].Kind = "dashscope.native"
	built, err := Build(cfg, m, obs.NewMetrics(prometheus.NewRegistry()),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("构建失败: %v", err)
	}

	var doors int
	for _, r := range m.Routes() {
		for _, ep := range r.Endpoints() {
			doors++
			// 兜底注册的是命名空间前缀；精确注册的门会拿到自己的 pattern。
			// 命中兜底就说明这扇门根本没注册处理器。
			_, pattern := built.Mux.Handler(httptest.NewRequest("POST", string(ep), nil))
			if pattern != "POST "+string(ep) {
				t.Errorf("已开门 %s 命中的是 %q，不是它自己的精确注册——请求会落进兜底",
					ep, pattern)
			}
		}
	}
	if doors == 0 {
		t.Fatal("矩阵一扇门都没开，这条测试会空转")
	}
}

// TestBuildFailsWhenRedeemedEndpointUnregistered 防「兑现过的门忘了注册」：
// 请求会落进 501 兜底，兑现承诺悄悄落空。
func TestBuildFailsWhenRedeemedEndpointUnregistered(t *testing.T) {
	m := degrade.NewMatrix()
	if err := m.Add(degrade.NewRoute(degrade.ProtoOpenAIChat, degrade.ProviderOpenAICompat).
		MarkHomogeneous().
		Pass(degrade.ExpressibleSet(degrade.ProtoOpenAIChat)...).
		Redeem(degrade.Endpoint("/v1/unregistered"), canonical.CapTextGeneration).
		Build()); err != nil {
		t.Fatal(err)
	}

	_, err := Build(buildTestConfig("http://127.0.0.1:0"), m,
		obs.NewMetrics(prometheus.NewRegistry()),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("兑现了未注册端点的矩阵应当让启动失败")
	}
	if !strings.Contains(err.Error(), "没有注册处理器") {
		t.Errorf("错误应指出端点未注册处理器: %v", err)
	}
}

// TestBuildFailsWhenRegisteredEndpointUnredeemed 防「注册了的门没人兑现」：
// 那是一处永远返回 501 的空头承诺。
func TestBuildFailsWhenRegisteredEndpointUnredeemed(t *testing.T) {
	// 只兑现 chat 门：registered 名单里的 responses 门与 Native 文本门没人兑现。
	m := degrade.NewMatrix()
	if err := m.Add(degrade.NewRoute(degrade.ProtoOpenAIChat, degrade.ProviderOpenAICompat).
		MarkHomogeneous().
		Pass(degrade.ExpressibleSet(degrade.ProtoOpenAIChat)...).
		Redeem(degrade.EndpointOpenAIChat, degrade.ExpressibleSet(degrade.ProtoOpenAIChat)...).
		Build()); err != nil {
		t.Fatal(err)
	}

	_, err := Build(buildTestConfig("http://127.0.0.1:0"), m,
		obs.NewMetrics(prometheus.NewRegistry()),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil {
		t.Fatal("注册了处理器却无路径兑现的端点应当让启动失败")
	}
	if !strings.Contains(err.Error(), "没有任何路径兑现它") {
		t.Errorf("错误应指出端点无人兑现: %v", err)
	}
}

// sharedSyntheticDoor 是一段两个协议都可能挂上去的路径。
//
// 用未知合成门而不是 /v1/chat/completions：已知门的归属在 degrade.Build
// 阶段就会被拦下，构造不出「同路径跨协议」的矩阵，那条闸门证明的也是另一件事。
// 未知门按设计放行，正好把对账自己的坐标是否够用单独暴露出来。
const sharedSyntheticDoor = degrade.Endpoint("/v1/shared-synthetic")

// openSyntheticDoorOn 造一条在合成门上兑现了一项能力的路径。
func openSyntheticDoorOn(t *testing.T, in degrade.Protocol) *degrade.Matrix {
	t.Helper()
	m := degrade.NewMatrix()
	if err := m.Add(degrade.NewRoute(in, degrade.ProviderOpenAICompat).
		MarkHomogeneous().
		Pass(degrade.ExpressibleSet(in)...).
		Redeem(sharedSyntheticDoor, canonical.CapTextGeneration).
		Build()); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestReconcileRejectsDoorOpenedUnderAnotherProtocol 防「同路径顶账」的第一个方向：
// 矩阵在 openai.chat 下开了这扇门，注册的却是 responses 的处理器。
//
// 只按路径对账，两份名单看上去严丝合缝，启动全绿；而请求进来时 chat 兑现的
// 那格能力，由一套 Responses 解码器把守——错配在运行时只表现为字段丢失。
func TestReconcileRejectsDoorOpenedUnderAnotherProtocol(t *testing.T) {
	m := openSyntheticDoorOn(t, degrade.ProtoOpenAIChat)

	err := reconcileDoors(m, []degrade.Inbound{
		{Protocol: degrade.ProtoOpenAIResponses, Endpoint: sharedSyntheticDoor},
	})
	if err == nil {
		t.Fatal("openai.chat 兑现的门被 openai.responses 的注册顶了账，对账却判绿")
	}
	if !strings.Contains(err.Error(), "没有注册处理器") {
		t.Errorf("错误应指出兑现的门没有处理器: %v", err)
	}
	if !strings.Contains(err.Error(), string(degrade.ProtoOpenAIChat)) {
		t.Errorf("错误应点名兑现方的协议 %s，否则读者不知道是哪一半错了: %v",
			degrade.ProtoOpenAIChat, err)
	}
	if !strings.Contains(err.Error(), string(sharedSyntheticDoor)) {
		t.Errorf("错误应点名端点 %s: %v", sharedSyntheticDoor, err)
	}
}

// TestReconcileRejectsHandlerRegisteredUnderAnotherProtocol 防第二个方向：
// 这扇门在 openai.chat 下确实开着，却多注册了一个 responses 的处理器。
//
// 按路径对账时，chat 那格兑现会把 responses 那行注册一并算成「有人兑现」，
// 于是一处永远返回 501 的空头承诺被判成绿。
func TestReconcileRejectsHandlerRegisteredUnderAnotherProtocol(t *testing.T) {
	m := openSyntheticDoorOn(t, degrade.ProtoOpenAIChat)

	err := reconcileDoors(m, []degrade.Inbound{
		{Protocol: degrade.ProtoOpenAIChat, Endpoint: sharedSyntheticDoor},
		{Protocol: degrade.ProtoOpenAIResponses, Endpoint: sharedSyntheticDoor},
	})
	if err == nil {
		t.Fatal("openai.responses 的注册借 openai.chat 的兑现顶了账，对账却判绿")
	}
	if !strings.Contains(err.Error(), "没有任何路径兑现它") {
		t.Errorf("错误应指出注册的门无人兑现: %v", err)
	}
	if !strings.Contains(err.Error(), string(degrade.ProtoOpenAIResponses)) {
		t.Errorf("错误应点名注册方的协议 %s: %v", degrade.ProtoOpenAIResponses, err)
	}
	if !strings.Contains(err.Error(), string(sharedSyntheticDoor)) {
		t.Errorf("错误应点名端点 %s: %v", sharedSyntheticDoor, err)
	}
}

// TestReconcileAcceptsMatchingProtocol 是上面两条的对照组：坐标两半都对上，
// 同一份矩阵与同一扇门必须放行。否则那两条测试只证明了「对账永远报错」。
func TestReconcileAcceptsMatchingProtocol(t *testing.T) {
	m := openSyntheticDoorOn(t, degrade.ProtoOpenAIChat)

	if err := reconcileDoors(m, []degrade.Inbound{
		{Protocol: degrade.ProtoOpenAIChat, Endpoint: sharedSyntheticDoor},
	}); err != nil {
		t.Fatalf("协议与端点都对上的注册不该被拒: %v", err)
	}
}

// TestDoorInboundDerivesProtocolFromHandler 防的是「注册清单自报身份」。
//
// 协议坐标若与处理器并排手写，它就只是一枚贴纸：换掉处理器而贴纸照旧，
// 这行注册报出来的仍是原来那个协议，对账两边严丝合缝，请求却已经交给了
// 另一套解码器。所以坐标的协议这一半只能从处理器自己身上取。
//
// 这里故意把 chat 那扇门交给 Responses 处理器：报出来的必须是处理器实际
// 把守的 openai.responses，而不是端点看上去应该属于的 openai.chat。
func TestDoorInboundDerivesProtocolFromHandler(t *testing.T) {
	d := door{endpoint: degrade.EndpointOpenAIChat, handler: NewResponsesHandler(Deps{})}

	if got := d.inbound().Protocol; got != degrade.ProtoOpenAIResponses {
		t.Errorf("注册报出的协议是 %s，但把守这扇门的是 %s 的处理器——坐标在替处理器说话",
			got, degrade.ProtoOpenAIResponses)
	}
	if got := d.inbound().Endpoint; got != degrade.EndpointOpenAIChat {
		t.Errorf("注册报出的端点是 %s，应为 %s", got, degrade.EndpointOpenAIChat)
	}

	// 派生出的坐标还得真能让对账红：矩阵在 openai.chat 下开了这扇门，
	// 而这行注册报的是 responses，两个方向的漂移都该被咬住。
	m := degrade.NewMatrix()
	if err := m.Add(degrade.NewRoute(degrade.ProtoOpenAIChat, degrade.ProviderOpenAICompat).
		MarkHomogeneous().
		Pass(degrade.ExpressibleSet(degrade.ProtoOpenAIChat)...).
		Redeem(degrade.EndpointOpenAIChat, canonical.CapTextGeneration).
		Build()); err != nil {
		t.Fatal(err)
	}

	if err := reconcileDoors(m, []degrade.Inbound{d.inbound()}); err == nil {
		t.Fatal("端点配错了处理器，对账却判绿")
	}
}

// TestBuildReconcilesPhase1Doors 把对账钉在真实启动路径上：Phase1 矩阵配上
// build.go 里那四行注册，协议坐标必须两两对上。
//
// 它咬的是注册清单本身——某一行漏写或写错入站协议，这里就红。
func TestBuildReconcilesPhase1Doors(t *testing.T) {
	m, err := degrade.Phase1()
	if err != nil {
		t.Fatal(err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":{"text":"ok"}}`))
	}))
	defer upstream.Close()

	cfg := buildTestConfig(upstream.URL)
	cfg.Providers[0].Kind = "dashscope.native"
	if _, err := Build(cfg, m, obs.NewMetrics(prometheus.NewRegistry()),
		slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("Phase1 的四扇门与四行注册应当对得上: %v", err)
	}
}

// TestHealthOnlyModeSkipsDoorReconciliation 锁住仅健康检查形态：没配 models
// 就没有任何注册，此时拿一份开着四扇门的矩阵去对账只会凭空拒绝启动。
//
// 空矩阵测不出这件事——它两边都是空的，怎么排都绿。
func TestHealthOnlyModeSkipsDoorReconciliation(t *testing.T) {
	m, err := degrade.Phase1()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := Build(config.Config{}, m, obs.NewMetrics(prometheus.NewRegistry()),
		slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("仅健康检查形态不该因为矩阵开着门而启动失败: %v", err)
	}
}

// nativeUpstreamRecorder 记录假上游收到了什么。
//
// 互斥保护不是形式：httptest 的处理器跑在服务端 goroutine 上，断言跑在测试
// goroutine 上，裸字段读写是数据竞争——-race 下会红，不加锁时只是碰运气。
type nativeUpstreamRecorder struct {
	mu    sync.Mutex
	calls int
	path  string
	body  []byte
	sse   string
}

func (rec *nativeUpstreamRecorder) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.calls++
	rec.path = r.URL.Path
	rec.body = body
	rec.sse = r.Header.Get(dashscopenative.SSEHeader)
}

func (rec *nativeUpstreamRecorder) snapshot() (calls int, path string, body []byte, sse string) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.calls, rec.path, append([]byte(nil), rec.body...), rec.sse
}

// nativeUpstream 起一个记录请求并回放固定响应体的假 DashScope 上游。
func nativeUpstream(t *testing.T, respBody string) (*httptest.Server, *nativeUpstreamRecorder) {
	t.Helper()
	rec := &nativeUpstreamRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// nativeGatewayConfig 组装一份最小可用的 dashscope.native 配置。
//
// 从 config.Default() 起手而不是裸 Config 字面量：这份配置还要交给
// config.Validate 走真实启动闸门，缺 server.addr / log / limits 会先被别的
// 校验拦下，测不到 native_endpoint 那一条。
func nativeGatewayConfig(upstreamURL, door string) config.Config {
	cfg := config.Default()
	cfg.Auth = config.Auth{Keys: []config.AuthKey{{ID: "test", Key: "sk-test-1234567890"}}}
	cfg.Credentials = map[string][]config.CredentialSpec{"pool1": {{ID: "1", Secret: "sec1"}}}
	cfg.Providers = []config.ProviderSpec{
		{Endpoint: "ep1", Kind: "dashscope.native", BaseURL: upstreamURL, CredentialPool: "pool1"},
	}
	cfg.Models = []config.ModelSpec{{Match: "*", Targets: []config.TargetSpec{
		{Endpoint: "ep1", UpstreamModel: "test-model", NativeEndpoint: door},
	}}}
	cfg.Timeouts = config.Timeouts{
		Connect: time.Second, FirstByte: 2 * time.Second, Total: 5 * time.Second, Idle: time.Second,
	}
	return cfg
}

// chatToNativeMatrix 造一份把 openai.chat → dashscope.native 兑现在 Chat 门上的矩阵。
//
// 生产矩阵（Phase1）此刻还没兑现这条异构路径——那是后续任务的事，本任务不许
// 提前投放。但装配对不对必须现在就能证：所以在测试里单独搭一份矩阵，只兑现本次
// 请求真正用到的能力（text_generation 与 streaming），其余照常不投放。
//
// 另外三条路径是为了满足启动期双向对账：build.go 固定注册四扇门，少开一扇
// 就会以「注册了却没人兑现」拒绝启动，那与本测试要证的事无关。
func chatToNativeMatrix(t *testing.T) *degrade.Matrix {
	t.Helper()
	m := degrade.NewMatrix()

	if err := m.Add(degrade.NewRoute(degrade.ProtoOpenAIChat, degrade.ProviderDashScopeNative).
		Pass(degrade.ExpressibleSet(degrade.ProtoOpenAIChat)...).
		Redeem(degrade.EndpointOpenAIChat, canonical.CapTextGeneration, canonical.CapStreaming).
		Build()); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(degrade.NewRoute(degrade.ProtoOpenAIResponses, degrade.ProviderOpenAICompat).
		MarkHomogeneous().
		Pass(degrade.ExpressibleSet(degrade.ProtoOpenAIResponses)...).
		Redeem(degrade.EndpointOpenAIResponses, canonical.CapTextGeneration).
		Build()); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(degrade.NewRoute(degrade.ProtoDashScopeNative, degrade.ProviderDashScopeNative).
		MarkHomogeneous().
		Pass(degrade.ExpressibleSet(degrade.ProtoDashScopeNative)...).
		Redeem(degrade.EndpointDashScopeTextGeneration, canonical.CapTextGeneration).
		Redeem(degrade.EndpointDashScopeMultimodal, canonical.CapTextGeneration).
		Build()); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestBuildNativeTranslatesChatInbound 钉死 Build 为 dashscope.native 装配的是
// Composite 适配器，且把配置里的门接线到了 router.Target。
//
// 只断言「构造出来的对象类型对」是不够的——那种断言在装配退回 passthrough 时
// 才红，而在门丢失时照绿。所以这里让一个真实的 Chat 请求走完 Mux：
// 打到的必须是文本生成门的上游路径、发出去的必须是 Native 信封、
// 回给客户端的必须是 chat.completion。三者任一不成立，装配就是错的：
// 装成 passthrough 会把 Chat 字节原样打到 /v1/chat/completions；
// 丢掉 NativeEndpoint 会让 Composite 拿到空门而报 500。
func TestBuildNativeTranslatesChatInbound(t *testing.T) {
	up, rec := nativeUpstream(t, `{"output":{"choices":[{"finish_reason":"stop",`+
		`"message":{"role":"assistant","content":"你好"}}]},`+
		`"usage":{"input_tokens":3,"output_tokens":2},"request_id":"req-build-1"}`)

	cfg := nativeGatewayConfig(up.URL, "text-generation")
	built, err := Build(cfg, chatToNativeMatrix(t), obs.NewMetrics(prometheus.NewRegistry()),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("构建失败: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, string(degrade.EndpointOpenAIChat),
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"你好"}]}`))
	req.Header.Set("Authorization", "Bearer sk-test-1234567890")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	built.Mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200: %s", w.Code, w.Body.String())
	}

	calls, path, body, sse := rec.snapshot()
	if calls != 1 {
		t.Fatalf("期望 1 次上游调用，实际 %d", calls)
	}
	if path != dashscopenative.TextGenerationPath {
		t.Errorf("上游路径 = %q，期望文本生成门 %q——门没从配置接线到 Target，或装配的不是 Composite",
			path, dashscopenative.TextGenerationPath)
	}
	if sse != "" {
		t.Errorf("非流式请求不得声明 SSE，实际 %q", sse)
	}

	// 出站必须是 Native 信封而不是原样转发的 Chat 信封：passthrough 装配下
	// 顶层是 messages，Composite 转换后才有 input.messages。
	var sent struct {
		Model string `json:"model"`
		Input struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		} `json:"input"`
		Messages json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("上游请求体不是 JSON: %v (%s)", err, body)
	}
	if len(sent.Messages) > 0 {
		t.Errorf("上游收到的是 Chat 信封（顶层 messages），说明装配的仍是直通: %s", body)
	}
	if len(sent.Input.Messages) != 1 || sent.Input.Messages[0].Content != "你好" {
		t.Errorf("上游未收到 Native input.messages: %s", body)
	}
	if sent.Model != "test-model" {
		t.Errorf("上游模型名 = %q，期望改写成 test-model", sent.Model)
	}

	// 回给客户端的必须是重编码后的 chat.completion——直通会把 Native 原样吐回。
	var got struct {
		Object  string `json:"object"`
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是 JSON: %v (%s)", err, w.Body.String())
	}
	if got.Object != "chat.completion" {
		t.Errorf("响应 object = %q，期望 chat.completion: %s", got.Object, w.Body.String())
	}
	if got.ID != "req-build-1" {
		t.Errorf("响应 id = %q，期望取上游 request_id", got.ID)
	}
	if got.Model != "test-model" {
		t.Errorf("响应 model = %q，期望上游模型名", got.Model)
	}
	if len(got.Choices) != 1 || got.Choices[0].Message.Content != "你好" {
		t.Errorf("响应候选内容不对: %s", w.Body.String())
	}
}

// TestBuildNativeRejectsStreamWithMultipleCandidates 防的是「流式下请求多候选（stream=true, n>1）在 DashScope Native 语义静默丢失」。
//
// DashScope Native 在流式传输下无法返回多候选（会静默退化回 n=1 导致候选丢失），
// 适配器在出站守卫阶段必须直接以 422 拒绝请求，且错误信封需点名 code=unsupported_capability 与 param=n，
// 并保证上游零调用（零字节出门）。
func TestBuildNativeRejectsStreamWithMultipleCandidates(t *testing.T) {
	up, rec := nativeUpstream(t, `{}`)

	cfg := nativeGatewayConfig(up.URL, "text-generation")
	built, err := Build(cfg, chatToNativeMatrix(t), obs.NewMetrics(prometheus.NewRegistry()),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("构建失败: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, string(degrade.EndpointOpenAIChat),
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"你好"}],"stream":true,"n":2}`))
	req.Header.Set("Authorization", "Bearer sk-test-1234567890")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	built.Mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("状态码 = %d，期望 422: %s", w.Code, w.Body.String())
	}

	calls, _, _, _ := rec.snapshot()
	if calls != 0 {
		t.Errorf("上游被调用了 %d 次，期望 0 次（422 应在出站前被拦截）", calls)
	}

	var errResp struct {
		Error struct {
			Type  string `json:"type"`
			Param string `json:"param"`
			Code  string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("解析 OpenAI 错误信封失败: %v (%s)", err, w.Body.String())
	}
	if errResp.Error.Type != "invalid_request_error" {
		t.Errorf("error.type = %q，期望 invalid_request_error", errResp.Error.Type)
	}
	if errResp.Error.Code != "unsupported_capability" {
		t.Errorf("error.code = %q，期望 unsupported_capability", errResp.Error.Code)
	}
	if errResp.Error.Param != "n" {
		t.Errorf("error.param = %q，期望 n", errResp.Error.Param)
	}
}

// TestBuildNativeKeepsNativeInboundPassthrough 钉死同源直通没有被 Composite 装配吃掉。
//
// Composite 的 Native 分支复用内部 passthrough：字节原样转发，出站端点由**入站门**
// 决定而不是配置里的 native_endpoint。所以这里刻意把配置的门固定成 text-generation，
// 却往多模态门打请求——若 Composite 错用了配置的门，多模态请求会被打到文本生成端点，
// 而响应仍是 200，客户端看不出任何异常。
func TestBuildNativeKeepsNativeInboundPassthrough(t *testing.T) {
	const nativeResp = `{"output":{"text":"ok"},"usage":{"input_tokens":1,"output_tokens":1},` +
		`"request_id":"req-native"}`

	m, err := degrade.Phase1()
	if err != nil {
		t.Fatal(err)
	}
	up, rec := nativeUpstream(t, nativeResp)
	built, err := Build(nativeGatewayConfig(up.URL, "text-generation"), m,
		obs.NewMetrics(prometheus.NewRegistry()),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("构建失败: %v", err)
	}

	for _, door := range []string{
		dashscopenative.TextGenerationPath,
		dashscopenative.MultimodalGenerationPath,
	} {
		t.Run(door, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, door, strings.NewReader(
				`{"model":"m","input":{"messages":[{"role":"user","content":"hello"}]}}`))
			req.Header.Set("Authorization", "Bearer sk-test-1234567890")
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			built.Mux.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("状态码 = %d，期望 200: %s", w.Code, w.Body.String())
			}
			_, path, body, _ := rec.snapshot()
			if path != door {
				t.Errorf("上游路径 = %q，期望入站门 %q", path, door)
			}
			// 请求体除模型名外原样：出现 parameters.result_format 就说明走了重编码。
			if strings.Contains(string(body), "result_format") {
				t.Errorf("同源直通不该重编码请求体: %s", body)
			}
			if !strings.Contains(string(body), `"input"`) {
				t.Errorf("上游请求体丢了 input 段: %s", body)
			}
			if w.Body.String() != nativeResp {
				t.Errorf("同源直通响应应逐字回放，实际 %s", w.Body.String())
			}
		})
	}
}

// TestBuildNativeDoorIsNotSwallowed 钉死门的两道启动闸门都真的拦得住。
//
// 缺门归 config.Validate（启动路径 config.Load → Validate → gateway.Build 的第一道），
// 不在 Build 里抄第二份；而**枚举写错**必须被装配这一侧咬住——router.Target 校验
// 只有在 Build 把 NativeEndpoint 接线过去之后才看得见这个值。装配漏接线时，
// 一个非法的门会被无声吞掉，网关照常启动，直到第一个请求打进来才炸。
func TestBuildNativeDoorIsNotSwallowed(t *testing.T) {
	t.Run("缺门由 config.Validate 拒绝", func(t *testing.T) {
		cfg := nativeGatewayConfig("https://dashscope.example.com", "")
		err := cfg.Validate()
		if err == nil {
			t.Fatal("dashscope.native target 缺 native_endpoint，启动校验必须失败")
		}
		if !strings.Contains(err.Error(), "native_endpoint") {
			t.Errorf("错误应点名 native_endpoint: %v", err)
		}
	})

	t.Run("非法枚举不被装配吞掉", func(t *testing.T) {
		cfg := nativeGatewayConfig("https://dashscope.example.com", "embedding")
		_, err := Build(cfg, chatToNativeMatrix(t), obs.NewMetrics(prometheus.NewRegistry()),
			slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err == nil {
			t.Fatal("非法的 native_endpoint 必须让启动失败——装配把它接线到 Target 才拦得住")
		}
		if !strings.Contains(err.Error(), "native_endpoint") {
			t.Errorf("错误应点名 native_endpoint: %v", err)
		}
	})
}
