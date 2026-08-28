//go:build smoke

package smoke_test

import (
	"bytes"
	"flag"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/degrade"
)

// record 控制是否向真实 DashScope 发送请求并落盘 fixture。
// 必须显式传 -record 才能执行，防止常规 go test 误打真实端点产生账单与网络副作用。
var record = flag.Bool("record", false, "录制真实 DashScope fixture")

// dashscopeDefaultBaseURL 是真实 DashScope 的默认站点。
const dashscopeDefaultBaseURL = "https://dashscope.aliyuncs.com"

// recordBaseURL 解析录制的目的站点，可用 OMUGW_SMOKE_BASE_URL 覆盖。
//
// 覆盖值先校验再用：一个少了 scheme 的 "dashscope.aliyuncs.com" 会被
// url.Parse 当成合法的相对路径收下，代理照样启动，直到第一次转发才以一个
// 与配置错误毫无关系的报错失败。这里在花钱之前就把它拦住。
func recordBaseURL(t testing.TB) string {
	t.Helper()

	raw := strings.TrimSpace(os.Getenv("OMUGW_SMOKE_BASE_URL"))
	if raw == "" {
		return dashscopeDefaultBaseURL
	}

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("OMUGW_SMOKE_BASE_URL %q 不是合法 URL: %v", raw, err)
	}
	// 只认 http/https 的绝对 URL：其余 scheme（file / 空）代理根本转发不了。
	if u.Scheme != "http" && u.Scheme != "https" {
		t.Fatalf("OMUGW_SMOKE_BASE_URL %q 的 scheme = %q，期望 http 或 https", raw, u.Scheme)
	}
	if u.Host == "" {
		t.Fatalf("OMUGW_SMOKE_BASE_URL %q 缺少主机名", raw)
	}
	return strings.TrimSuffix(raw, "/")
}

// TestRecordChatDSNative 是录制真实 DashScope Native fixture 的执行入口。
// 为防止误调用产生实际开销或凭据泄露，在此强制校验 flag 与前置环境变量，
// 且绝不把 API Key 输出到测试日志中。
func TestRecordChatDSNative(t *testing.T) {
	if !*record {
		t.Skip("跳过真实录制：未指定 -record 标志")
	}
	if os.Getenv("OMUGW_SMOKE") != "1" {
		t.Skip("跳过真实录制：未设置 OMUGW_SMOKE=1 环境变量")
	}
	apiKey := strings.TrimSpace(os.Getenv("DASHSCOPE_API_KEY"))
	if apiKey == "" {
		t.Skip("跳过真实录制：未设置 DASHSCOPE_API_KEY 环境变量")
	}

	baseURL := recordBaseURL(t)
	outDir := filepath.Clean(fixtureDir)

	// 逐用例开子测试：单条用例失败不该带走其余十二条已经花钱打出来的证据，
	// 重录一条也不必把整批重打一遍（`-run TestRecordChatDSNative/basic`）。
	for _, c := range recordCases {
		t.Run(c.name, func(t *testing.T) {
			model := modelForRole(c.modelRole)
			if model == "" {
				t.Fatalf("模型角色 %q 解析为空字符串", string(c.modelRole))
			}

			// 一个用例一个代理：录制状态只服务一次捕获，复用会让后一次的
			// 响应配着前一次的请求存盘。
			proxy, state := newRecordingProxy(t, baseURL)
			built := buildRecordingGateway(t, proxy.URL, c.door, model, apiKey)

			clientBody := c.body(recordClientModel)
			req := httptest.NewRequest(http.MethodPost, string(degrade.EndpointOpenAIChat),
				bytes.NewReader(clientBody))
			req.Header.Set("Authorization", "Bearer "+testGatewayAuthKey)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			built.Mux.ServeHTTP(rec, req)

			snap := state.Snapshot()
			assertRecordingReachedUpstream(t, snap)
			assertRecordedCase(t, c, model, rec, snap)

			f := buildRecordedFixture(c, req.Header, clientBody, snap)
			saveRecordedFixture(t, outDir, f, !t.Failed())
		})
	}
}
