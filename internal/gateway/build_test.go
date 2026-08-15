package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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
