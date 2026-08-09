package router

import (
	"errors"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/degrade"
)

func target(kind degrade.Provider, endpoint, upstream string) Target {
	return Target{
		Kind:           kind,
		Endpoint:       endpoint,
		BaseURL:        "https://" + endpoint + ".example.com",
		UpstreamModel:  upstream,
		CredentialPool: endpoint,
	}
}

func mustNew(t *testing.T, rules ...Rule) *Router {
	t.Helper()
	r, err := New(rules)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestExactMatchWins(t *testing.T) {
	r := mustNew(t,
		Rule{Match: "qwen-max", Targets: []Target{target(degrade.ProviderDashScopeNative, "dashscope", "qwen-max")}},
		Rule{Match: "qwen-*", Targets: []Target{target(degrade.ProviderDashScopeCompatible, "ds-compat", "qwen-turbo")}},
	)

	got, err := r.Resolve("qwen-max")
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Endpoint != "dashscope" {
		t.Errorf("精确规则应优先于前缀规则，实际命中 %q", got[0].Endpoint)
	}
}

// TestLongestPrefixWins 固化「最长前缀优先」。
//
// 不排序的话，qwen-* 与 qwen-vl-* 谁先命中取决于配置书写顺序——
// 那是一种看不见的耦合，改一行配置的位置就会改变路由结果。
func TestLongestPrefixWins(t *testing.T) {
	// 故意把短前缀写在前面。
	r := mustNew(t,
		Rule{Match: "qwen-*", Targets: []Target{target(degrade.ProviderDashScopeCompatible, "generic", "qwen-turbo")}},
		Rule{Match: "qwen-vl-*", Targets: []Target{target(degrade.ProviderDashScopeNative, "vl", "qwen-vl-max")}},
	)

	got, err := r.Resolve("qwen-vl-plus")
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Endpoint != "vl" {
		t.Errorf("应命中更长的前缀 qwen-vl-*，实际 %q", got[0].Endpoint)
	}

	got, err = r.Resolve("qwen-turbo")
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Endpoint != "generic" {
		t.Errorf("应命中 qwen-*，实际 %q", got[0].Endpoint)
	}
}

func TestFallback(t *testing.T) {
	r := mustNew(t,
		Rule{Match: "gpt-5", Targets: []Target{target(degrade.ProviderOpenAICompat, "openai", "gpt-5")}},
		Rule{Match: "*", Targets: []Target{target(degrade.ProviderOpenAICompat, "catch-all", "gpt-4o-mini")}},
	)

	got, err := r.Resolve("something-nobody-configured")
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Endpoint != "catch-all" {
		t.Errorf("应命中兜底规则，实际 %q", got[0].Endpoint)
	}
}

// TestUnknownModelDoesNotLeakConfig 固化一处安全细节。
//
// 在错误消息里列出可用模型，等于把部署的模型清单暴露给任何一个能发请求的人。
// 想知道有什么模型，走 /v1/models 那条受鉴权保护的路。
func TestUnknownModelDoesNotLeakConfig(t *testing.T) {
	r := mustNew(t,
		Rule{Match: "internal-secret-model", Targets: []Target{target(degrade.ProviderOpenAICompat, "x", "y")}},
	)

	_, err := r.Resolve("nope")
	if err == nil {
		t.Fatal("未知模型应当报错")
	}
	if strings.Contains(err.Error(), "internal-secret-model") {
		t.Errorf("错误消息泄露了配置中的模型名: %v", err)
	}

	var cerr *canonical.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("应返回 *canonical.Error，实际 %T", err)
	}
	// 从客户端视角这是「你要的模型我这儿没有」，属于请求问题。
	if cerr.Class != canonical.ClassBadRequest {
		t.Errorf("分类 = %q, 期望 bad_request", cerr.Class)
	}
}

// TestKindAndEndpointAreSeparate 是这个包最核心的一处设计。
//
// OpenAI、DeepSeek、Kimi 都是 openai.compat：矩阵看它们完全一样（同一套线格式），
// 而路由必须分得清该发去哪一个。只留一个字段，要么矩阵得为每个上游各存一份
// 重复声明，要么路由无从选择。
func TestKindAndEndpointAreSeparate(t *testing.T) {
	r := mustNew(t, Rule{Match: "fast", Targets: []Target{
		target(degrade.ProviderOpenAICompat, "deepseek", "deepseek-chat"),
		target(degrade.ProviderOpenAICompat, "kimi", "moonshot-v1-8k"),
		target(degrade.ProviderDashScopeNative, "dashscope", "qwen-turbo"),
	}})

	targets, err := r.Resolve("fast")
	if err != nil {
		t.Fatal(err)
	}

	// 矩阵只关心协议族，因此去重。
	kinds := Kinds(targets)
	if len(kinds) != 2 {
		t.Fatalf("协议族去重后应为 2 个，实际 %d 个: %v", len(kinds), kinds)
	}

	// 而路由层保留全部上游，且顺序即 failover 顺序。
	compat := OfKind(targets, degrade.ProviderOpenAICompat)
	if len(compat) != 2 {
		t.Fatalf("openai.compat 族下应有 2 个上游，实际 %d 个", len(compat))
	}
	if compat[0].Endpoint != "deepseek" || compat[1].Endpoint != "kimi" {
		t.Errorf("同族内应保持配置顺序（即 failover 顺序），实际 %v", compat)
	}
}

// TestUpstreamModelRenaming 覆盖网关的一项基本价值：改名。
func TestUpstreamModelRenaming(t *testing.T) {
	r := mustNew(t, Rule{Match: "fast", Targets: []Target{
		target(degrade.ProviderDashScopeCompatible, "ds", "qwen-turbo"),
	}})

	got, _ := r.Resolve("fast")
	if got[0].UpstreamModel != "qwen-turbo" {
		t.Errorf("上游模型名 = %q, 期望 qwen-turbo", got[0].UpstreamModel)
	}
}

func TestNewRejectsBadRules(t *testing.T) {
	ok := target(degrade.ProviderOpenAICompat, "x", "y")

	tests := map[string][]Rule{
		"没有规则":   {},
		"空候选":    {{Match: "m"}},
		"重复精确规则": {{Match: "m", Targets: []Target{ok}}, {Match: "m", Targets: []Target{ok}}},
		"两条兜底":   {{Match: "*", Targets: []Target{ok}}, {Match: "*", Targets: []Target{ok}}},
		// 中缀通配的匹配顺序难以预期，而路由结果不可预期意味着请求会去到
		// 谁也说不清的地方。
		"中缀通配":  {{Match: "gpt-*-mini", Targets: []Target{ok}}},
		"空前缀":   {{Match: "*", Targets: []Target{ok}}, {Match: "*x", Targets: []Target{ok}}},
		"候选缺字段": {{Match: "m", Targets: []Target{{Kind: degrade.ProviderOpenAICompat}}}},
	}

	for name, rules := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := New(rules); err == nil {
				t.Error("应当拒绝这份配置")
			}
		})
	}
}

// TestModelsOnlyListsExact 固化一处克制。
//
// 前缀与兜底规则匹配的是无穷集合，列不出来。硬要列就得编一份清单，
// 而那份清单与实际能用的模型必然对不上——那比不列更糟。
func TestModelsOnlyListsExact(t *testing.T) {
	r := mustNew(t,
		Rule{Match: "b-model", Targets: []Target{target(degrade.ProviderOpenAICompat, "x", "y")}},
		Rule{Match: "a-model", Targets: []Target{target(degrade.ProviderOpenAICompat, "x", "y")}},
		Rule{Match: "qwen-*", Targets: []Target{target(degrade.ProviderDashScopeNative, "d", "q")}},
		Rule{Match: "*", Targets: []Target{target(degrade.ProviderOpenAICompat, "x", "y")}},
	)

	got := r.Models()
	if len(got) != 2 {
		t.Fatalf("应只列出 2 个精确模型，实际 %v", got)
	}
	// 顺序必须稳定，否则 /v1/models 每次返回的排列都不同。
	if got[0] != "a-model" || got[1] != "b-model" {
		t.Errorf("模型清单应按字典序，实际 %v", got)
	}
}

func TestEmptyModelIsRejected(t *testing.T) {
	r := mustNew(t, Rule{Match: "*", Targets: []Target{target(degrade.ProviderOpenAICompat, "x", "y")}})

	// 即使有兜底规则，空模型名也该被拒——它多半是客户端漏填了字段。
	if _, err := r.Resolve(""); err == nil {
		t.Error("空模型名应当被拒绝，而不是落到兜底规则")
	}
}
