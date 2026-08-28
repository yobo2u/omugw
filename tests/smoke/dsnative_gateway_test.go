//go:build smoke

package smoke_test

import (
	"encoding/json"
	"fmt"
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
	"github.com/yobo2u/omugw/internal/gateway"
	"github.com/yobo2u/omugw/internal/obs"
	nativewire "github.com/yobo2u/omugw/internal/protocol/dashscopenative"
)

const testGatewayAuthKey = "sk-omugw-smoke-gateway-key-local-12345"

type capturedRequest struct {
	auth  string
	path  string
	body  []byte
	model string
	err   error
}

// gatewayRecordingTimeouts 是录制期网关的四层超时。
//
// 数值按真实上游而不是本地假源站取：深度思考模型的首字节要以分钟计，一条完整
// 的流式回答更长。照本地量级配（秒级）会让每一次真实录制都在半途被网关自己
// 掐断，而落盘的将是一份「上游只说了半句」的证据——它语法合法、能通过校验，
// 却把网关的超时误记成了模型的回答。
//
// 关系约束由 config.Timeouts.Validate 兜底：connect < first_byte <= total，
// 且 idle <= total。
func gatewayRecordingTimeouts() config.Timeouts {
	return config.Timeouts{
		Connect:   10 * time.Second,
		FirstByte: 120 * time.Second,
		Total:     5 * time.Minute,
		Idle:      60 * time.Second,
	}
}

// buildRecordingGateway 从配置装配录制用网关实例。
func buildRecordingGateway(t testing.TB, baseURL string, door nativewire.Door, model, upstreamSecret string) *gateway.Built {
	t.Helper()

	cfg := config.Default()
	cfg.Auth = config.Auth{
		Keys: []config.AuthKey{
			{ID: "smoke-recorder-test", Key: testGatewayAuthKey},
		},
	}
	cfg.Credentials = map[string][]config.CredentialSpec{
		"pool-ds-native": {
			{ID: "cred-1", Secret: upstreamSecret},
		},
	}
	cfg.Providers = []config.ProviderSpec{
		{
			Endpoint:       "ep-ds-native",
			Kind:           string(degrade.ProviderDashScopeNative),
			BaseURL:        baseURL,
			CredentialPool: "pool-ds-native",
		},
	}
	cfg.Models = []config.ModelSpec{
		{
			Match: "*",
			Targets: []config.TargetSpec{
				{
					Endpoint:       "ep-ds-native",
					UpstreamModel:  model,
					NativeEndpoint: string(door),
				},
			},
		},
	}
	cfg.Timeouts = gatewayRecordingTimeouts()

	if err := cfg.Validate(); err != nil {
		t.Fatalf("校验网关配置失败: %v", err)
	}

	reg := prometheus.NewRegistry()
	metrics := obs.NewMetrics(reg)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	built, err := gateway.Build(cfg, recordMatrix(t), metrics, logger)
	if err != nil {
		t.Fatalf("构建网关失败: %v", err)
	}
	return built
}

// TestRecordingGatewayReachesNativeTextDoor 验证网关从 Chat 入站到 Native 假上游的完整本地链路。
func TestRecordingGatewayReachesNativeTextDoor(t *testing.T) {
	const (
		fakeUpstreamSecret = "sk-fake-upstream-secret-98765"
		selectedModel      = "qwen-max"
		selectedDoor       = nativewire.DoorTextGeneration
	)

	reqChan := make(chan capturedRequest, 1)
	fakeNativeResp := `{"output":{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"你好！我是通义千问。"}}]},"usage":{"input_tokens":10,"output_tokens":8},"request_id":"req-smoke-local-001"}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			reqChan <- capturedRequest{err: fmt.Errorf("read body: %w", err)}
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}
		var reqPayload struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &reqPayload); err != nil {
			reqChan <- capturedRequest{err: fmt.Errorf("unmarshal payload: %w", err)}
			http.Error(w, "unmarshal error", http.StatusBadRequest)
			return
		}
		reqChan <- capturedRequest{
			auth:  r.Header.Get("Authorization"),
			path:  r.URL.Path,
			body:  body,
			model: reqPayload.Model,
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, fakeNativeResp)
	}))
	defer upstream.Close()

	built := buildRecordingGateway(t, upstream.URL, selectedDoor, selectedModel, fakeUpstreamSecret)

	req := httptest.NewRequest(http.MethodPost, string(degrade.EndpointOpenAIChat),
		strings.NewReader(`{"model":"qwen-max","messages":[{"role":"user","content":"你好"}]}`))
	req.Header.Set("Authorization", "Bearer "+testGatewayAuthKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	built.Mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200: %s", rec.Code, rec.Body.String())
	}

	select {
	case captured := <-reqChan:
		if captured.err != nil {
			t.Fatalf("假上游捕获请求失败: %v", captured.err)
		}
		if expectedAuth := "Bearer " + fakeUpstreamSecret; captured.auth != expectedAuth {
			t.Errorf("上游 Authorization = %q，期望 %q", captured.auth, expectedAuth)
		}
		if captured.path != nativewire.TextGenerationPath {
			t.Errorf("上游路径 = %q，期望 %q", captured.path, nativewire.TextGenerationPath)
		}
		if captured.model != selectedModel {
			t.Errorf("上游 model = %q，期望 %q", captured.model, selectedModel)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("等待上游收到请求超时")
	}

	var chatResp struct {
		Object  string `json:"object"`
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &chatResp); err != nil {
		t.Fatalf("解析响应 JSON 失败: %v (%s)", err, rec.Body.String())
	}
	if chatResp.Object != "chat.completion" {
		t.Errorf("响应 object = %q，期望 chat.completion: %s", chatResp.Object, rec.Body.String())
	}
	if chatResp.ID != "req-smoke-local-001" {
		t.Errorf("响应 id = %q，期望 req-smoke-local-001", chatResp.ID)
	}
	if len(chatResp.Choices) != 1 || chatResp.Choices[0].Message.Content != "你好！我是通义千问。" {
		t.Errorf("响应 choices 不匹配: %s", rec.Body.String())
	}
}
