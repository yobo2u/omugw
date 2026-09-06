//go:build smoke

package smoke_test

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/gateway"
	"github.com/yobo2u/omugw/internal/obs"
	nativewire "github.com/yobo2u/omugw/internal/protocol/dashscopenative"
)

// smokeAPIKey 取真实凭据；缺失时跳过全部真实冒烟——不得用假上游顶替。
// 必须同时满足 OMUGW_SMOKE=1 与非空 DASHSCOPE_API_KEY 双重开关，
// 防的是本地环境碰巧带有 DASHSCOPE_API_KEY 的开发者在运行带有 -tags=smoke 的测试时
// 意外向真实云端发请求产生非预期账单与网络副作用。
// 绝不以任何形式打印或记录该密钥。
func smokeAPIKey(t testing.TB) string {
	t.Helper()
	if os.Getenv("OMUGW_SMOKE") != "1" {
		t.Skip("跳过真实冒烟：未设置 OMUGW_SMOKE=1 环境变量")
	}
	key := strings.TrimSpace(os.Getenv("DASHSCOPE_API_KEY"))
	if key == "" {
		t.Skip("跳过真实冒烟：未设置 DASHSCOPE_API_KEY 环境变量")
	}
	return key
}

// buildSmokeGateway 组装直打真实 DashScope 的冒烟网关。
//
// 用生产 Phase1 矩阵而不是录制脚手架：smoke 证的是真实部署形态。
// 任务 18 兑现后，生产矩阵在 Chat 门上兑现的正是本期那 8 项；
// 若哪天兑现回退，由矩阵测试白名单与 TestSmokeGatewayBuildsWithProductionMatrix
// 的显式坐标断言（ImplementedAt）咬住。
func buildSmokeGateway(t testing.TB, door nativewire.Door, model, upstreamSecret string) *gateway.Built {
	t.Helper()

	cfg := config.Default()
	cfg.Auth = config.Auth{
		Keys: []config.AuthKey{{ID: "smoke-test", Key: testGatewayAuthKey}},
	}
	cfg.Credentials = map[string][]config.CredentialSpec{
		"pool-ds-native": {{ID: "cred-1", Secret: upstreamSecret}},
	}
	cfg.Providers = []config.ProviderSpec{{
		Endpoint:       "ep-ds-native",
		Kind:           string(degrade.ProviderDashScopeNative),
		BaseURL:        recordBaseURL(t),
		CredentialPool: "pool-ds-native",
	}}
	cfg.Models = []config.ModelSpec{{
		Match: "*",
		Targets: []config.TargetSpec{{
			Endpoint:       "ep-ds-native",
			UpstreamModel:  model,
			NativeEndpoint: string(door),
		}},
	}}
	cfg.Timeouts = gatewayRecordingTimeouts()

	if err := cfg.Validate(); err != nil {
		t.Fatalf("校验网关配置失败: %v", err)
	}

	m, err := degrade.Phase1()
	if err != nil {
		t.Fatalf("构造生产矩阵失败: %v", err)
	}

	reg := prometheus.NewRegistry()
	metrics := obs.NewMetrics(reg)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	built, err := gateway.Build(cfg, m, metrics, logger)
	if err != nil {
		t.Fatalf("构建网关失败: %v", err)
	}
	return built
}

// doSmokeChat 打一次 Chat 请求，返回记录的响应。
func doSmokeChat(t *testing.T, built *gateway.Built, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, string(degrade.EndpointOpenAIChat),
		bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testGatewayAuthKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	built.Mux.ServeHTTP(rec, req)
	return rec
}

// TestSmokeGatewayBuildsWithProductionMatrix 离线钉死冒烟网关能用生产矩阵装配：
// 显式断言生产 Phase1 矩阵中 openai.chat -> dashscope.native 路径在 EndpointOpenAIChat 门已实现投放。
// 且 Build 过程不触网、装配必须成功。
func TestSmokeGatewayBuildsWithProductionMatrix(t *testing.T) {
	m, err := degrade.Phase1()
	if err != nil {
		t.Fatalf("构造生产矩阵失败: %v", err)
	}
	r, ok := m.Route(degrade.ProtoOpenAIChat, degrade.ProviderDashScopeNative)
	if !ok {
		t.Fatal("生产矩阵缺少 openai.chat -> dashscope.native 路径")
	}
	if !r.ImplementedAt(degrade.EndpointOpenAIChat) {
		t.Fatalf("路径 %s -> %s 在端点 %s 上未标记为已实现",
			degrade.ProtoOpenAIChat, degrade.ProviderDashScopeNative, degrade.EndpointOpenAIChat)
	}

	built := buildSmokeGateway(t, nativewire.DoorTextGeneration, "qwen-plus",
		"sk-fake-smoke-build-check")
	if built == nil || built.Mux == nil {
		t.Fatal("网关装配返回空结构体或空 Mux")
	}
}

// TestSmokeAPIKeyDualOptIn 离线钉死 smokeAPIKey 的双重 opt-in 契约：
// OMUGW_SMOKE=1 与 DASHSCOPE_API_KEY 缺任一均跳过，防止带 key 的环境误跑真实请求。
func TestSmokeAPIKeyDualOptIn(t *testing.T) {
	t.Run("缺少 OMUGW_SMOKE 跳过", func(t *testing.T) {
		t.Setenv("OMUGW_SMOKE", "")
		t.Setenv("DASHSCOPE_API_KEY", "sk-test-fake")
		var key string
		var skipped bool
		t.Run("sub", func(subT *testing.T) {
			defer func() {
				skipped = subT.Skipped()
			}()
			key = smokeAPIKey(subT)
		})
		if !skipped {
			t.Fatal("缺少 OMUGW_SMOKE 时必须触发 t.Skip")
		}
		if key != "" {
			t.Errorf("OMUGW_SMOKE 为空时不应返回 key，实际 %q", key)
		}
	})

	t.Run("缺少 DASHSCOPE_API_KEY 跳过", func(t *testing.T) {
		t.Setenv("OMUGW_SMOKE", "1")
		t.Setenv("DASHSCOPE_API_KEY", "   ")
		var key string
		var skipped bool
		t.Run("sub", func(subT *testing.T) {
			defer func() {
				skipped = subT.Skipped()
			}()
			key = smokeAPIKey(subT)
		})
		if !skipped {
			t.Fatal("缺少 DASHSCOPE_API_KEY 时必须触发 t.Skip")
		}
		if key != "" {
			t.Errorf("DASHSCOPE_API_KEY 为空时不应返回 key，实际 %q", key)
		}
	})

	t.Run("双重开关就绪时返回凭据", func(t *testing.T) {
		t.Setenv("OMUGW_SMOKE", "1")
		t.Setenv("DASHSCOPE_API_KEY", "sk-valid-key")
		var key string
		var skipped bool
		t.Run("sub", func(subT *testing.T) {
			defer func() {
				skipped = subT.Skipped()
			}()
			key = smokeAPIKey(subT)
		})
		if skipped {
			t.Fatal("双重开关就绪时不应触发 t.Skip")
		}
		if key != "sk-valid-key" {
			t.Errorf("双重开关就绪时期望返回 key，实际 %q", key)
		}
	})
}
