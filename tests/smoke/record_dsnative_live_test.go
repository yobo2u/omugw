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
	nativewire "github.com/yobo2u/omugw/internal/protocol/dashscopenative"
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

// TestRecordChatDSNativeAudioInputStays501 是录制会话的 live 负例：
// 真实凭据在手，audio_input 请求也必须被矩阵闸门以 501 拦下——
// 录制代理一次请求都不该收到，任何音频模型（qwen-audio-turbo 等）
// 自始至终不会被调用。
//
// 保留成独立探针而不是 recordCases 条目：它证明的是「闸门关着」，
// 应落盘的证据恰恰是上游调用为零——放进 recordCases 会要求产出一份
// audio_input fixture，而本期名单里没有它。
//
// 门选 multimodal-generation（audio_input 若被兑现该走的门），模型角色
// 刻意选文本：本探针不得调用任何音频模型，而矩阵会在模型被问到之前拦下请求。
func TestRecordChatDSNativeAudioInputStays501(t *testing.T) {
	if !*record {
		t.Skip("跳过 live 探针：未指定 -record 标志")
	}
	if os.Getenv("OMUGW_SMOKE") != "1" {
		t.Skip("跳过 live 探针：未设置 OMUGW_SMOKE=1 环境变量")
	}
	apiKey := strings.TrimSpace(os.Getenv("DASHSCOPE_API_KEY"))
	if apiKey == "" {
		t.Skip("跳过 live 探针：未设置 DASHSCOPE_API_KEY 环境变量")
	}

	baseURL := recordBaseURL(t)
	proxy, state := newRecordingProxy(t, baseURL)
	built := buildRecordingGateway(t, proxy.URL, nativewire.DoorMultimodalGeneration,
		modelForRole(modelRoleText), apiKey)

	req := httptest.NewRequest(http.MethodPost, string(degrade.EndpointOpenAIChat),
		bytes.NewReader(bodyAudioInput(recordClientModel)))
	req.Header.Set("Authorization", "Bearer "+testGatewayAuthKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	built.Mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("状态码 = %d，期望 501: %s", rec.Code, rec.Body.String())
	}
	if snap := state.Snapshot(); snap.Requests != 0 {
		t.Fatalf("live 探针触达上游 %d 次——audio_input 必须在矩阵裁决阶段被拦下", snap.Requests)
	}
	if !strings.Contains(rec.Body.String(), "audio_input") {
		t.Errorf("501 错误应点名 audio_input，证明它来自矩阵裁决而非上游错误: %s", rec.Body.String())
	}
}
