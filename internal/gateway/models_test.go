package gateway

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/obs"
)

// modelsConfig 返回一份配齐的、指向给定上游的网关配置。
func modelsConfig(upstreamURL string) config.Config {
	return config.Config{
		Auth: config.Auth{Keys: []config.AuthKey{{ID: "test", Key: "sk-test-1234567890"}}},
		Credentials: map[string][]config.CredentialSpec{
			"pool1": {{ID: "1", Secret: "sec1"}},
		},
		Providers: []config.ProviderSpec{
			{Endpoint: "ep1", Kind: "openai.compat", BaseURL: upstreamURL, CredentialPool: "pool1"},
		},
		Models: []config.ModelSpec{
			{Match: "gpt-5", Targets: []config.TargetSpec{{Endpoint: "ep1", UpstreamModel: "gpt-5"}}},
			{Match: "qwen-*", Targets: []config.TargetSpec{{Endpoint: "ep1", UpstreamModel: "qwen-plus"}}},
		},
		Timeouts: config.Timeouts{
			Connect: time.Second, FirstByte: 2 * time.Second, Total: 3 * time.Second, Idle: time.Second,
		},
		Limits: config.Limits{MaxRequestBytes: 1 << 20, MaxInlineBytes: 1 << 20},
	}
}

func buildForModels(t *testing.T, cfg config.Config) *Built {
	t.Helper()

	m, err := degrade.Phase1()
	if err != nil {
		t.Fatalf("加载 Phase1 矩阵失败: %v", err)
	}
	built, err := Build(cfg, m, obs.NewMetrics(prometheus.NewRegistry()),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("构建失败: %v", err)
	}
	return built
}

func getModels(t *testing.T, built *Built, key string) (int, []string) {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	built.Mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		return rec.Code, nil
	}

	var payload struct {
		Object string `json:"object"`
		Data   []struct {
			ID     string `json:"id"`
			Object string `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if payload.Object != "list" {
		t.Errorf("信封 object = %q，期望 list", payload.Object)
	}

	ids := make([]string, 0, len(payload.Data))
	for _, m := range payload.Data {
		if m.Object != "model" {
			t.Errorf("条目 object = %q，期望 model", m.Object)
		}
		ids = append(ids, m.ID)
	}
	return rec.Code, ids
}

// TestModelsRequiresAuth：这扇门后面是部署的模型清单，等于部署拓扑的一部分。
// Router.Resolve 刻意不在错误里列模型，就是为了不把它免费送人。
func TestModelsRequiresAuth(t *testing.T) {
	built := buildForModels(t, modelsConfig("https://upstream.invalid"))

	for name, key := range map[string]string{
		"没有凭据": "",
		"错误凭据": "sk-wrong-0987654321",
	} {
		t.Run(name, func(t *testing.T) {
			if code, _ := getModels(t, built, key); code != http.StatusUnauthorized {
				t.Fatalf("状态码 = %d，期望 401", code)
			}
		})
	}
}

// TestModelsListsOnlyExactConfigRulesWithoutDiscovery 固化未启用发现时的行为：
// 只列精确规则。前缀与兜底匹配的是无穷集合，硬要列就得编一份与实际对不上的清单。
func TestModelsListsOnlyExactConfigRulesWithoutDiscovery(t *testing.T) {
	built := buildForModels(t, modelsConfig("https://upstream.invalid"))

	code, ids := getModels(t, built, "sk-test-1234567890")
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", code)
	}
	if len(ids) != 1 || ids[0] != "gpt-5" {
		t.Fatalf("清单 = %v，期望只有精确规则 gpt-5", ids)
	}
}

// TestModelsMergesDiscoveredModels 覆盖启用发现后的合并、去重与排序。
func TestModelsMergesDiscoveredModels(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("发现应打到 /v1/models，实际 %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[
			{"id":"gpt-5-mini","object":"model","created":1,"owned_by":"openai"},
			{"id":"gpt-5","object":"model","created":2,"owned_by":"openai"},
			{"id":"o4","object":"model","created":3,"owned_by":"openai"}
		]}`))
	}))
	defer upstream.Close()

	cfg := modelsConfig(upstream.URL)
	cfg.Discovery = config.Discovery{
		Enabled:         true,
		RefreshInterval: time.Minute,
		Timeout:         5 * time.Second,
	}

	built := buildForModels(t, cfg)
	if built.Discovery == nil {
		t.Fatal("启用发现时应当装配刷新器")
	}
	built.Discovery.RefreshOnce(t.Context())

	code, ids := getModels(t, built, "sk-test-1234567890")
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", code)
	}

	want := []string{"gpt-5", "gpt-5-mini", "o4"}
	if len(ids) != len(want) {
		t.Fatalf("清单 = %v，期望 %v", ids, want)
	}
	for i := range want {
		// 顺序随 map 漂移的清单没法用来做变更对比。
		if ids[i] != want[i] {
			t.Fatalf("清单 = %v，期望按字典序的 %v", ids, want)
		}
	}
}

// TestDiscoveryIsDisabledByDefault：不配 discovery 时不该装配任何后台刷新。
func TestDiscoveryIsDisabledByDefault(t *testing.T) {
	built := buildForModels(t, modelsConfig("https://upstream.invalid"))
	if built.Discovery != nil {
		t.Fatal("未启用发现时不应装配刷新器")
	}
}

// TestDiscoveryNeverCreatesRoutes 是这一节最重要的一条。
//
// 发现到的模型只出现在清单里，**不可被请求**。若发现即建路由，一次上游目录
// 变更就能让网关无声地把请求发去一个没人配置过、降级矩阵也没审视过的目的地。
func TestDiscoveryNeverCreatesRoutes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"o4","owned_by":"openai"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response"}`))
	}))
	defer upstream.Close()

	cfg := modelsConfig(upstream.URL)
	cfg.Discovery = config.Discovery{
		Enabled: true, RefreshInterval: time.Minute, Timeout: 5 * time.Second,
	}
	// 去掉兜底规则以外的通配，确保未配置的模型无路可走。
	cfg.Models = []config.ModelSpec{
		{Match: "gpt-5", Targets: []config.TargetSpec{{Endpoint: "ep1", UpstreamModel: "gpt-5"}}},
	}

	built := buildForModels(t, cfg)
	built.Discovery.RefreshOnce(t.Context())

	// 先确认它确实进了清单。
	_, ids := getModels(t, built, "sk-test-1234567890")
	found := false
	for _, id := range ids {
		if id == "o4" {
			found = true
		}
	}
	if !found {
		t.Fatalf("发现到的模型应当出现在清单里，实际 %v", ids)
	}

	// 但它不可被请求：路由里没有它。
	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"o4","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer sk-test-1234567890")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	built.Mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("发现到但未配置路由的模型应当被拒，状态码 = %d，body = %s",
			rec.Code, rec.Body.String())
	}
}
