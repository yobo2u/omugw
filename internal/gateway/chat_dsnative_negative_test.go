package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

// nativeOKUpstream 返回一份能让 Native 转换器走完全程的非流式响应。
func nativeOKUpstream(t *testing.T) *upstream {
	return jsonUpstream(t, `{"output":{"choices":[{"finish_reason":"stop",`+
		`"message":{"role":"assistant","content":"ok"}}]},`+
		`"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7},"request_id":"r-neg"}`)
}

// TestChatDSNativeAudioInputNotRedeemedIs501 钉死 audio_input 本期未投放：
// 501 说「等实现」，不说「改请求」；矩阵裁决阶段拦下，上游零调用。
//
// 错误信封由 openaiwire 编码：ClassNotImplemented 映射成 server_error，
// code 为空被 omitempty 省略——状态码、type、code 缺席、点名能力四点齐断，
// 缺一处都说明这个 501 是从别处（上游错误或路由兜底）漏过来的。
func TestChatDSNativeAudioInputNotRedeemedIs501(t *testing.T) {
	up := newUpstream(t, func(http.ResponseWriter, *http.Request) {})
	hs := newChatDSNativeHarness(t, "multimodal-generation", up)

	rec := hs.do(t, `{"model":"m","messages":[{"role":"user","content":[`+
		`{"type":"text","text":"这段音频里说了什么？"},`+
		`{"type":"input_audio","input_audio":{"data":"UklGRiQAAABXQVZFZm10IBAAAAABAAEAQB8AAIA+AAACABAAZGF0YQAAAAA=","format":"wav"}}]}]}`, true)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("状态码 = %d，期望 501: %s", rec.Code, rec.Body.String())
	}
	if n := up.calls.Load(); n != 0 {
		t.Errorf("请求打到了上游 %d 次——未投放能力必须在矩阵裁决阶段拦下", n)
	}

	var env struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("错误信封不是合法 JSON: %v (%s)", err, rec.Body.String())
	}
	if env.Error.Type != "server_error" {
		t.Errorf("error.type = %q，期望 server_error（not_implemented 的 OpenAI 线格式）", env.Error.Type)
	}
	if env.Error.Code != "" {
		t.Errorf("error.code = %q，期望省略（ClassNotImplemented 无 code）", env.Error.Code)
	}
	if !strings.Contains(env.Error.Message, string(canonical.CapAudioInput)) {
		t.Errorf("错误应点名能力 audio_input: %s", env.Error.Message)
	}
}

// TestChatDSNativeRejectsFileInput 固化 file_input 在这条路上是 REJECT：
// 422 说「改请求」，矩阵闸门拦下，一个字节都不出门。
func TestChatDSNativeRejectsFileInput(t *testing.T) {
	up := newUpstream(t, func(http.ResponseWriter, *http.Request) {})
	hs := newChatDSNativeHarness(t, "multimodal-generation", up)

	rec := hs.do(t, `{"model":"m","messages":[{"role":"user","content":[
		{"type":"text","text":"处理这个文件"},
		{"type":"file","file":{"file_id":"file-abc"}}]}]}`, true)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("状态码 = %d，期望 422: %s", rec.Code, rec.Body.String())
	}
	if n := up.calls.Load(); n != 0 {
		t.Errorf("请求打到了上游 %d 次——REJECT 必须在矩阵闸门拦下", n)
	}
	if !strings.Contains(rec.Body.String(), string(canonical.CapFileInput)) {
		t.Errorf("错误应点名能力 file_input: %s", rec.Body.String())
	}
}

// TestChatDSNativeRejectsAudioOutput 固化 audio_output 在这条路上是 REJECT：
// Chat 入站表达不出 Qwen-Omni 的输出格式参数。
func TestChatDSNativeRejectsAudioOutput(t *testing.T) {
	up := newUpstream(t, func(http.ResponseWriter, *http.Request) {})
	hs := newChatDSNativeHarness(t, "text-generation", up)

	rec := hs.do(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],
		"modalities":["text","audio"]}`, true)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("状态码 = %d，期望 422: %s", rec.Code, rec.Body.String())
	}
	if n := up.calls.Load(); n != 0 {
		t.Errorf("请求打到了上游 %d 次——REJECT 必须在矩阵闸门拦下", n)
	}
	if !strings.Contains(rec.Body.String(), string(canonical.CapAudioOutput)) {
		t.Errorf("错误应点名能力 audio_output: %s", rec.Body.String())
	}
}

// TestChatDSNativeRejectsNoLandingFields 固化无落点字段显式提交即 422 且点名，
// 在 Provider 出门前拦下（上游零调用）。
func TestChatDSNativeRejectsNoLandingFields(t *testing.T) {
	tests := []struct {
		name  string
		field string
		param string
	}{
		{"frequency_penalty", `"frequency_penalty":0.5`, "frequency_penalty"},
		{"logit_bias", `"logit_bias":{"a":1}`, "logit_bias"},
		{"service_tier", `"service_tier":"default"`, "service_tier"},
		{"store", `"store":true`, "store"},
		{"user", `"user":"u-1"`, "user"},
		{"metadata", `"metadata":{"k":"v"}`, "metadata"},
		{"audio", `"audio":{"voice":"alloy","format":"wav"}`, "audio"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			up := newUpstream(t, func(http.ResponseWriter, *http.Request) {})
			hs := newChatDSNativeHarness(t, "text-generation", up)

			rec := hs.do(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],`+tt.field+`}`, true)

			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("状态码 = %d，期望 422: %s", rec.Code, rec.Body.String())
			}
			if n := up.calls.Load(); n != 0 {
				t.Errorf("请求打到了上游 %d 次——无落点字段必须在出门前拦下", n)
			}
			var env struct {
				Error struct {
					Type  string `json:"type"`
					Code  string `json:"code"`
					Param string `json:"param"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("错误信封不是合法 JSON: %v (%s)", err, rec.Body.String())
			}
			if env.Error.Type != "invalid_request_error" {
				t.Errorf("error.type = %q，期望 invalid_request_error", env.Error.Type)
			}
			if env.Error.Code != "unsupported_capability" {
				t.Errorf("error.code = %q，期望 unsupported_capability", env.Error.Code)
			}
			if env.Error.Param != tt.param {
				t.Errorf("error.param = %q，期望点名 %s", env.Error.Param, tt.param)
			}
		})
	}
}

// TestChatDSNativeRejectsNonStreamReasoning 固化非流式 + 非 none 推理即 422：
// 官方硬约束，思考模式不允许非流式调用。
func TestChatDSNativeRejectsNonStreamReasoning(t *testing.T) {
	up := newUpstream(t, func(http.ResponseWriter, *http.Request) {})
	hs := newChatDSNativeHarness(t, "text-generation", up)

	rec := hs.do(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],
		"reasoning_effort":"high"}`, true)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("状态码 = %d，期望 422: %s", rec.Code, rec.Body.String())
	}
	if n := up.calls.Load(); n != 0 {
		t.Errorf("请求打到了上游 %d 次——硬约束必须在出门前拦下", n)
	}
	var env struct {
		Error struct {
			Type  string `json:"type"`
			Code  string `json:"code"`
			Param string `json:"param"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("错误信封不是合法 JSON: %v (%s)", err, rec.Body.String())
	}
	if env.Error.Type != "invalid_request_error" {
		t.Errorf("error.type = %q，期望 invalid_request_error", env.Error.Type)
	}
	if env.Error.Code != "unsupported_capability" {
		t.Errorf("error.code = %q，期望 unsupported_capability", env.Error.Code)
	}
	if env.Error.Param != "reasoning_effort" {
		t.Errorf("error.param = %q，期望点名 reasoning_effort", env.Error.Param)
	}
}

// TestChatDSNativeNoneReasoningPasses 是 none 的正例：非流式 + reasoning_effort:none
// 必须放行并真的打到上游。缺了正例，一条把 none 也拒掉的回归改不出红。
func TestChatDSNativeNoneReasoningPasses(t *testing.T) {
	up := nativeOKUpstream(t)
	hs := newChatDSNativeHarness(t, "text-generation", up)

	rec := hs.do(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],
		"reasoning_effort":"none"}`, true)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200: %s", rec.Code, rec.Body.String())
	}
	if n := up.calls.Load(); n != 1 {
		t.Errorf("上游应收到 1 次请求，实际 %d", n)
	}
}

// TestChatDSNativeRejectsNWithTools 固化 n>1 + tools 即 422：
// Native 带 tools 时会把 n 静默压回 1，候选丢失必须入站拦截。
func TestChatDSNativeRejectsNWithTools(t *testing.T) {
	up := newUpstream(t, func(http.ResponseWriter, *http.Request) {})
	hs := newChatDSNativeHarness(t, "text-generation", up)

	rec := hs.do(t, `{"model":"m","n":2,
		"tools":[{"type":"function","function":{"name":"f",
		"parameters":{"type":"object"}}}],
		"messages":[{"role":"user","content":"hi"}]}`, true)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("状态码 = %d，期望 422: %s", rec.Code, rec.Body.String())
	}
	if n := up.calls.Load(); n != 0 {
		t.Errorf("请求打到了上游 %d 次——n>1 + tools 必须在出门前拦下", n)
	}
	var env struct {
		Error struct {
			Type  string `json:"type"`
			Code  string `json:"code"`
			Param string `json:"param"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("错误信封不是合法 JSON: %v (%s)", err, rec.Body.String())
	}
	if env.Error.Type != "invalid_request_error" {
		t.Errorf("error.type = %q，期望 invalid_request_error", env.Error.Type)
	}
	if env.Error.Code != "unsupported_capability" {
		t.Errorf("error.code = %q，期望 unsupported_capability", env.Error.Code)
	}
	if env.Error.Param != "n" {
		t.Errorf("error.param = %q，期望点名 n", env.Error.Param)
	}
}

// TestChatDSNativeRejectsStreamWithMultipleCandidates 钉死 stream=true 搭 n>1
// 在触达上游前被拒成 422：Native 流式无法返回多候选，放过去只会被上游静默
// 压回 n=1，候选丢失而客户端无感。错误信封由 openaiwire 决定：
// ClassUnsupported 映射成 invalid_request_error，code 为 unsupported_capability，
// param 点名 n，上游零调用。与装配层回归 TestBuildNativeRejectsStreamWithMultipleCandidates
// 互为表里：那里证真实装配，这里证转换器语义。非流式 n>1 不受该守卫影响，
// 照常放行——那条路由 multi_candidate_nonstream 的回放举证，本文件不再重复。
func TestChatDSNativeRejectsStreamWithMultipleCandidates(t *testing.T) {
	up := newUpstream(t, func(http.ResponseWriter, *http.Request) {})
	hs := newChatDSNativeHarness(t, "text-generation", up)

	rec := hs.do(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],
		"stream":true,"n":2}`, true)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("状态码 = %d，期望 422: %s", rec.Code, rec.Body.String())
	}
	if n := up.calls.Load(); n != 0 {
		t.Errorf("请求打到了上游 %d 次——stream=true 搭 n>1 必须在出门前拦下", n)
	}

	var env struct {
		Error struct {
			Type  string `json:"type"`
			Code  string `json:"code"`
			Param string `json:"param"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("错误信封不是合法 JSON: %v (%s)", err, rec.Body.String())
	}
	if env.Error.Type != "invalid_request_error" {
		t.Errorf("error.type = %q，期望 invalid_request_error", env.Error.Type)
	}
	if env.Error.Code != "unsupported_capability" {
		t.Errorf("error.code = %q，期望 unsupported_capability", env.Error.Code)
	}
	if env.Error.Param != "n" {
		t.Errorf("error.param = %q，期望点名 n", env.Error.Param)
	}
}
