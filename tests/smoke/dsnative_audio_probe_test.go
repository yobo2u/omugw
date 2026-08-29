//go:build smoke

package smoke_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/degrade"
	nativewire "github.com/yobo2u/omugw/internal/protocol/dashscopenative"
)

// TestAudioInputNotRedeemedStays501BeforeUpstream 离线钉死部分投放设计：
// audio_input 的设计处置仍是 PASS，但本期不兑现——请求必须被矩阵裁决以 501
// 拦下，假源站一次都不该被打到。
//
// 防的是录制矩阵替生产兑现：recordMatrix 一旦把 audio_input 兑回去，
// 请求会穿过网关打到源站，状态码与代理计数同时失守。
//
// 门刻意选 multimodal-generation——audio_input 若被兑现该走的门。
// 若 501 其实来自媒体过滤的无门可承载，拿到的是 422 而不是 501，
// 两种拒绝不得混淆。
func TestAudioInputNotRedeemedStays501BeforeUpstream(t *testing.T) {
	origin := newStubOrigin(t, stubOriginConfig{
		stream:   false,
		jsonBody: nativeJSONResponse(1),
	})
	proxy, state := newRecordingProxy(t, origin.URL)
	built := buildRecordingGateway(t, proxy.URL, nativewire.DoorMultimodalGeneration,
		modelForRole(modelRoleText), preflightUpstreamSecret)

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
		t.Errorf("录制代理收到 %d 次请求，期望 0——未兑现能力不得触达上游", snap.Requests)
	}

	// 501 的线格式由 openaiwire 决定：ClassNotImplemented 映射成 server_error，
	// code 为空被 omitempty 省略。三点齐断，缺一处都说明这个 501 是从别处漏过来的。
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
		t.Errorf("error.type = %q，期望 server_error", env.Error.Type)
	}
	if env.Error.Code != "" {
		t.Errorf("error.code = %q，期望省略（ClassNotImplemented 无 code）", env.Error.Code)
	}
	if !strings.Contains(env.Error.Message, "audio_input") {
		t.Errorf("错误应点名能力 audio_input: %s", env.Error.Message)
	}
}
