package discovery

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/credential"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/transport/httpx"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testPool(t *testing.T) *credential.Pool {
	t.Helper()
	p, err := credential.NewPool("test",
		[]credential.Credential{{ID: "primary", Secret: "sk-test"}},
		credential.DefaultPolicy(), nil)
	if err != nil {
		t.Fatalf("构造凭据池失败: %v", err)
	}
	return p
}

func testDiscoveryConfig() config.Discovery {
	return config.Discovery{
		Enabled:         true,
		RefreshInterval: time.Minute,
		Timeout:         5 * time.Second,
	}
}

func testClient() *httpx.Client {
	return httpx.New(config.Timeouts{
		Connect:   time.Second,
		FirstByte: 2 * time.Second,
		Total:     5 * time.Second,
		Idle:      2 * time.Second,
	}, nil)
}

// TestOpenAIProberReadsListEnvelope 固化 OpenAI 契约：一次 GET 拿全量，
// 不发分页参数。
func TestOpenAIProberReadsListEnvelope(t *testing.T) {
	var gotPath, gotAuth, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotQuery = r.URL.Path, r.Header.Get("Authorization"), r.URL.RawQuery
		_, _ = w.Write([]byte(`{"object":"list","data":[
			{"id":"gpt-5","object":"model","created":1,"owned_by":"openai"},
			{"id":"gpt-5-mini","object":"model","created":2,"owned_by":"openai"}
		]}`))
	}))
	defer srv.Close()

	models, err := (&OpenAIProber{}).Probe(context.Background(), testClient(), srv.URL, "sk-test")
	if err != nil {
		t.Fatalf("探测应当成功: %v", err)
	}

	if gotPath != "/v1/models" {
		t.Errorf("路径 = %q，期望 /v1/models", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("鉴权头 = %q，期望 Bearer sk-test", gotAuth)
	}
	// 官方契约上这个操作没有分页参数，发了就是在编造契约。
	if gotQuery != "" {
		t.Errorf("不应发送任何查询参数，实际 %q", gotQuery)
	}
	if len(models) != 2 || models[0].ID != "gpt-5" || models[0].OwnedBy != "openai" {
		t.Fatalf("解析结果不符: %+v", models)
	}
}

// TestOpenAIProberToleratesUnknownFields：OpenAI 明说「往响应对象里加新属性」
// 属于向后兼容变更。严格解码会让上游一次例行更新就把发现打挂。
func TestOpenAIProberToleratesUnknownFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","brand_new_top_field":1,"data":[
			{"id":"gpt-5","object":"model","created":1,"owned_by":"openai",
			 "shutdown_date":"2026-10-23","brand_new_item_field":{"x":1}}
		]}`))
	}))
	defer srv.Close()

	models, err := (&OpenAIProber{}).Probe(context.Background(), testClient(), srv.URL, "sk-test")
	if err != nil {
		t.Fatalf("未知字段不应导致失败: %v", err)
	}
	if len(models) != 1 || models[0].ID != "gpt-5" {
		t.Fatalf("解析结果不符: %+v", models)
	}
}

// TestOpenAIProberClassifiesUpstreamError 固化错误信封走 openaiwire 解码。
func TestOpenAIProberClassifiesUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key","type":"authentication_error"}}`))
	}))
	defer srv.Close()

	_, err := (&OpenAIProber{}).Probe(context.Background(), testClient(), srv.URL, "sk-bad")
	if err == nil {
		t.Fatal("401 应当报错")
	}
	if !strings.Contains(err.Error(), "bad key") {
		t.Errorf("应还原上游错误消息，实际: %v", err)
	}
}

// TestDashScopeNativeProberPaginates 固化页码分页：一次 GET 不是全量。
func TestDashScopeNativeProberPaginates(t *testing.T) {
	var pages []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/models" {
			t.Errorf("路径 = %q，期望 /api/v1/models", r.URL.Path)
		}
		page := r.URL.Query().Get("page_no")
		pages = append(pages, page)

		switch page {
		case "1":
			_, _ = fmt.Fprint(w, `{"success":true,"output":{"total":3,"page_no":1,"models":[
				{"model":"qwen3-max","provider":"qwen"},
				{"model":"qwen-plus","provider":"qwen"}
			]}}`)
		default:
			_, _ = fmt.Fprint(w, `{"success":true,"output":{"total":3,"page_no":2,"models":[
				{"model":"qwen-vl-max","provider":"qwen"}
			]}}`)
		}
	}))
	defer srv.Close()

	models, err := (&DashScopeNativeProber{}).Probe(context.Background(), testClient(), srv.URL, "sk-test")
	if err != nil {
		t.Fatalf("探测应当成功: %v", err)
	}
	if len(pages) != 2 {
		t.Fatalf("应当翻 2 页，实际请求了 %d 页: %v", len(pages), pages)
	}
	if len(models) != 3 {
		t.Fatalf("应当收集 3 个模型，实际 %d: %+v", len(models), models)
	}
	if models[0].ID != "qwen3-max" || models[0].OwnedBy != "qwen" {
		t.Errorf("首个模型解析不符: %+v", models[0])
	}
}

// TestDashScopeNativeProberStopsOnEmptyPage：空页是「翻到头了」的可靠信号，
// 比信 total 更稳——上游把 total 报大时不能因此死循环。
func TestDashScopeNativeProberStopsOnEmptyPage(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			_, _ = fmt.Fprint(w, `{"output":{"total":9999,"models":[{"model":"qwen-plus"}]}}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"output":{"total":9999,"models":[]}}`)
	}))
	defer srv.Close()

	models, err := (&DashScopeNativeProber{}).Probe(context.Background(), testClient(), srv.URL, "sk-test")
	if err != nil {
		t.Fatalf("探测应当成功: %v", err)
	}
	if calls != 2 {
		t.Fatalf("应当在空页处停下，实际请求 %d 次", calls)
	}
	if len(models) != 1 {
		t.Fatalf("应当只收集 1 个模型，实际 %+v", models)
	}
}

// TestCompatibleHasNoProber 固化边界：dashscope.compatible 没有官方列表接口，
// 探测表里就不该有它。
func TestCompatibleHasNoProber(t *testing.T) {
	probers := Probers()
	if _, ok := probers[degrade.ProviderDashScopeCompatible]; ok {
		t.Fatal("dashscope.compatible 没有官方模型列表接口，不得登记探测器")
	}
	for _, kind := range []degrade.Provider{
		degrade.ProviderOpenAICompat, degrade.ProviderDashScopeNative,
	} {
		if _, ok := probers[kind]; !ok {
			t.Errorf("%s 应当有探测器", kind)
		}
	}
}

// TestRefreshSkipsProviderWithoutProber：没有探测器不是错误，是跳过。
func TestRefreshSkipsProviderWithoutProber(t *testing.T) {
	reg := NewRegistry()
	r := NewRefresher(reg, []Endpoint{{
		Name:    "ds-compat",
		Kind:    degrade.ProviderDashScopeCompatible,
		BaseURL: "https://example.invalid",
		Pool:    testPool(t),
	}}, Probers(), testClient(), testDiscoveryConfig(), quietLogger())

	r.RefreshOnce(context.Background())

	if got := reg.Snapshot(); len(got) != 0 {
		t.Fatalf("跳过的 endpoint 不该产生快照，实际 %+v", got)
	}
}

// TestRefreshKeepsPreviousSnapshotOnFailure 是这一节最重要的一条。
//
// 清空会让一次上游抖动直接表现为「客户端看到的模型清单突然少了一半」。
func TestRefreshKeepsPreviousSnapshotOnFailure(t *testing.T) {
	var fail bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"boom","type":"server_error"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-5","owned_by":"openai"}]}`))
	}))
	defer srv.Close()

	reg := NewRegistry()
	r := NewRefresher(reg, []Endpoint{{
		Name:    "openai",
		Kind:    degrade.ProviderOpenAICompat,
		BaseURL: srv.URL,
		Pool:    testPool(t),
	}}, Probers(), testClient(), testDiscoveryConfig(), quietLogger())

	r.RefreshOnce(context.Background())
	if got := reg.Snapshot(); len(got) != 1 {
		t.Fatalf("首轮应当成功，实际 %+v", got)
	}

	fail = true
	r.RefreshOnce(context.Background())

	got := reg.Snapshot()
	if len(got) != 1 || got[0].ID != "gpt-5" {
		t.Fatalf("失败后应保留上一次快照，实际 %+v", got)
	}
}

// TestRefreshTagsEndpointAndSnapshotIsSortedAndDeduped：同一个模型 ID 可能
// 同时出现在多个 endpoint 上，列两遍只会让客户端困惑；顺序随 map 漂移的清单
// 没法用来做变更对比。
func TestRefreshTagsEndpointAndSnapshotIsSortedAndDeduped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[
			{"id":"gpt-5-mini","owned_by":"openai"},
			{"id":"gpt-5","owned_by":"openai"}
		]}`))
	}))
	defer srv.Close()

	reg := NewRegistry()
	r := NewRefresher(reg, []Endpoint{
		{Name: "openai-a", Kind: degrade.ProviderOpenAICompat, BaseURL: srv.URL, Pool: testPool(t)},
		{Name: "openai-b", Kind: degrade.ProviderOpenAICompat, BaseURL: srv.URL, Pool: testPool(t)},
	}, Probers(), testClient(), testDiscoveryConfig(), quietLogger())

	r.RefreshOnce(context.Background())

	got := reg.Snapshot()
	if len(got) != 2 {
		t.Fatalf("两个 endpoint 返回同一组模型，去重后应为 2 条，实际 %+v", got)
	}
	if got[0].ID != "gpt-5" || got[1].ID != "gpt-5-mini" {
		t.Fatalf("快照应按 ID 字典序，实际 %+v", got)
	}
	if got[0].Endpoint == "" {
		t.Error("发现结果应标注来源 endpoint")
	}
}
