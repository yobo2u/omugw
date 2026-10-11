package dashscopecompat

import (
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/testkit"
)

// responsesInput 构造一次打 Responses 门的调用。
func responsesInput(raw string) callInput {
	return callInput{
		raw:             raw,
		upstreamModel:   "qwen-up",
		inboundEndpoint: degrade.EndpointOpenAIResponses,
	}
}

func asCanonical(t *testing.T, err error) *canonical.Error {
	t.Helper()
	var cerr *canonical.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("期望 *canonical.Error，实际 %T: %v", err, err)
	}
	return cerr
}

// TestResponsesDoorDecodesFlatEnvelope 防的是上游的原话被吞掉：这扇门返回
// 扁平信封 {request_id, code, message}，按 OpenAI 嵌套信封解码会取不到 code
// 与 message，客户端只能看到一句 Bad Request。
//
// 用例取自 2026-10-11 实测的续轮未命中响应。它必须是 bad_request 且不可重试：
// 这不是凭据故障，换凭据只会更糟。
func TestResponsesDoorDecodesFlatEnvelope(t *testing.T) {
	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"request_id":"req-1","code":"InvalidParameter",`+
			`"message":"Not found previous_response_id: resp_x."}`)
	})

	_, err := call(t, srv, responsesInput(`{"model":"m","input":"hi","previous_response_id":"resp_x"}`))
	e := asCanonical(t, err)

	if e.Class != canonical.ClassBadRequest {
		t.Errorf("分类 = %s，期望 bad_request", e.Class)
	}
	if e.Retryable {
		t.Error("续轮未命中不得标为可重试")
	}
	if e.Message != "Not found previous_response_id: resp_x." {
		t.Errorf("message = %q，上游原话丢了", e.Message)
	}
	if e.UpstreamCode != "InvalidParameter" {
		t.Errorf("upstream code = %q，期望 InvalidParameter", e.UpstreamCode)
	}
	if e.UpstreamRequestID != "req-1" {
		t.Errorf("request_id = %q，期望 req-1", e.UpstreamRequestID)
	}
	if e.UpstreamStatus != http.StatusBadRequest {
		t.Errorf("upstream status = %d，期望 400", e.UpstreamStatus)
	}
}

// TestResponsesDoorFallsBackToNestedEnvelope 防的是把两种形状写成二选一：
// 官方文档里嵌套信封同样出现过，取不到扁平字段时必须回落。
func TestResponsesDoorFallsBackToNestedEnvelope(t *testing.T) {
	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"slow down","type":"rate_limit_error","code":"rate_limit_exceeded"}}`)
	})

	_, err := call(t, srv, responsesInput(`{"model":"m","input":"hi"}`))
	e := asCanonical(t, err)

	if e.Class != canonical.ClassRateLimit {
		t.Errorf("分类 = %s，期望 rate_limit", e.Class)
	}
	if e.Message != "slow down" {
		t.Errorf("message = %q，期望 slow down", e.Message)
	}
}

// TestResponsesDoorNonJSONErrorUsesStatus 防的是 5xx 的 HTML 页面让解码崩掉：
// 此时状态码是唯一可靠的信息。
func TestResponsesDoorNonJSONErrorUsesStatus(t *testing.T) {
	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `<html>bad gateway</html>`)
	})

	_, err := call(t, srv, responsesInput(`{"model":"m","input":"hi"}`))
	e := asCanonical(t, err)

	if e.Class != canonical.ClassUpstreamUnavailable {
		t.Errorf("分类 = %s，期望 upstream_unavailable", e.Class)
	}
}

// TestResponsesDoorFlatAuthErrorIsRetryable 防的是把所有扁平错误都当参数错误：
// 鉴权失败换一把凭据可能成功，必须保持可重试。
func TestResponsesDoorFlatAuthErrorIsRetryable(t *testing.T) {
	srv, _ := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"request_id":"req-2","code":"InvalidApiKey","message":"Invalid API-key provided."}`)
	})

	_, err := call(t, srv, responsesInput(`{"model":"m","input":"hi"}`))
	e := asCanonical(t, err)

	if e.Class != canonical.ClassAuth || !e.Retryable {
		t.Errorf("分类 = %s retryable = %v，期望 auth 且可重试", e.Class, e.Retryable)
	}
}

// TestResponsesDoorHitsResponsesEndpoint 防的是请求被送去 Chat 端点：
// 官方 base_url 以 /v1 结尾，入站门是 /v1/responses，去重后必须落在
// {base}/responses。
func TestResponsesDoorHitsResponsesEndpoint(t *testing.T) {
	srv, got := okServer(t)
	in := responsesInput(`{"model":"logical","input":"hi"}`)
	in.baseURL = srv.URL + "/compatible-mode/v1"

	if _, err := call(t, srv, in); err != nil {
		t.Fatal(err)
	}
	if got.path != "/compatible-mode/v1/responses" {
		t.Errorf("上游路径 = %q，期望 /compatible-mode/v1/responses", got.path)
	}
}

// TestResponsesDoorOnlyRewritesModel 防的是直通变成重编码：除 model 外每个
// 字段都必须原样到达上游，包括会话字段与网关不建模的参数。
func TestResponsesDoorOnlyRewritesModel(t *testing.T) {
	srv, got := okServer(t)
	const raw = `{"model":"logical","input":"hi","previous_response_id":"resp_1",` +
		`"store":true,"metadata":{"trace":"abc"},"reasoning":{"effort":"low","summary":"detailed"},` +
		`"parallel_tool_calls":false,"text":{"format":{"type":"json_object"}},` +
		`"tools":[{"type":"function","name":"f","strict":true,"parameters":{"type":"object"}},{"type":"web_search"}]}`

	if _, err := call(t, srv, responsesInput(raw)); err != nil {
		t.Fatal(err)
	}

	const want = `{"model":"qwen-up","input":"hi","previous_response_id":"resp_1",` +
		`"store":true,"metadata":{"trace":"abc"},"reasoning":{"effort":"low","summary":"detailed"},` +
		`"parallel_tool_calls":false,"text":{"format":{"type":"json_object"}},` +
		`"tools":[{"type":"function","name":"f","strict":true,"parameters":{"type":"object"}},{"type":"web_search"}]}`
	testkit.AssertJSONEqual(t, []byte(want), got.body, "Responses 门只应改写 model")
}

// TestResponsesDoorOmittedStoreIsNotInjected 防的是网关替客户端决定留存：
// 省略 store 时由上游默认值生效（实测默认存储 7 天），网关写入任何值都是
// 在改客户端没表达过的意图。
func TestResponsesDoorOmittedStoreIsNotInjected(t *testing.T) {
	srv, got := okServer(t)
	if _, err := call(t, srv, responsesInput(`{"model":"logical","input":"hi"}`)); err != nil {
		t.Fatal(err)
	}
	if _, ok := upstreamFields(t, got)["store"]; ok {
		t.Errorf("上游请求里出现了 store，网关不该注入: %s", got.body)
	}
}

// TestResponsesDoorRenamesPreviewSearch 防的是一次不发生的搜索返回 200：
// 实测上游接受 web_search_preview 但不执行搜索，只有 web_search 生效。
// tool_choice 必须同步改名，否则它会指向一个改名后不存在的工具。
// 其余工具与搜索工具自带的选项保持原样。
func TestResponsesDoorRenamesPreviewSearch(t *testing.T) {
	srv, got := okServer(t)
	const raw = `{"model":"logical","input":"hi",` +
		`"tools":[{"type":"web_search_preview","search_context_size":"low"},` +
		`{"type":"function","name":"f","parameters":{"type":"object"}}],` +
		`"tool_choice":{"type":"web_search_preview"}}`

	if _, err := call(t, srv, responsesInput(raw)); err != nil {
		t.Fatal(err)
	}

	const want = `{"model":"qwen-up","input":"hi",` +
		`"tools":[{"type":"web_search","search_context_size":"low"},` +
		`{"type":"function","name":"f","parameters":{"type":"object"}}],` +
		`"tool_choice":{"type":"web_search"}}`
	testkit.AssertJSONEqual(t, []byte(want), got.body, "预览版搜索类型应被改写")
}

// TestResponsesDoorLeavesStringToolChoiceAlone 防的是改名逻辑误伤字符串形态
// 的 tool_choice（auto / required / none）。
func TestResponsesDoorLeavesStringToolChoiceAlone(t *testing.T) {
	srv, got := okServer(t)
	const raw = `{"model":"logical","input":"hi","tool_choice":"required",` +
		`"tools":[{"type":"function","name":"f","parameters":{"type":"object"}}]}`
	if _, err := call(t, srv, responsesInput(raw)); err != nil {
		t.Fatal(err)
	}
	if choice := string(upstreamFields(t, got)["tool_choice"]); choice != `"required"` {
		t.Errorf("tool_choice = %s，期望原样 \"required\"", choice)
	}
}

// TestResponsesDoorDoesNotApplyChatSearchMapping 防的是两扇门的修补串台：
// enable_search 是 Chat 门的映射产物，Responses 门不得注入。
func TestResponsesDoorDoesNotApplyChatSearchMapping(t *testing.T) {
	srv, got := okServer(t)
	if _, err := call(t, srv, responsesInput(`{"model":"logical","input":"hi","tools":[{"type":"web_search"}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, ok := upstreamFields(t, got)["enable_search"]; ok {
		t.Errorf("Responses 门不应出现 enable_search: %s", got.body)
	}
}

// TestResponsesDoorMissingUpstreamModel 防的是把逻辑模型名发给上游。
func TestResponsesDoorMissingUpstreamModel(t *testing.T) {
	srv, got := okServer(t)
	in := responsesInput(`{"model":"logical","input":"hi"}`)
	in.upstreamModel = ""

	_, err := call(t, srv, in)
	if e := asCanonical(t, err); e.Class != canonical.ClassInternal {
		t.Errorf("分类 = %s，期望 internal", e.Class)
	}
	if got.body != nil {
		t.Error("缺少上游模型名时不应触达上游")
	}
}
