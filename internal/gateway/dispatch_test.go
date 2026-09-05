package gateway

import (
	"net/http"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/provider"
	"github.com/yobo2u/omugw/internal/router"
)

// TestDispatchPreservesProviderErrorAfterCredentialExhaustion 钉死最后一次真实调用的
// 错误不能被「本请求已试完全部凭据」覆盖。后者只是 dispatch 的循环终止条件；若把
// 它回给客户端，模型拒绝参数等真实根因就会被伪装成凭据池冷却。
func TestDispatchPreservesProviderErrorAfterCredentialExhaustion(t *testing.T) {
	const providerMessage = "上游拒绝流式多候选"
	failing := &staticProvider{
		kind: degrade.ProviderDashScopeNative,
		err:  canonical.Newf(canonical.ClassUpstreamUnavailable, providerMessage),
	}
	target := nativeTarget("a", "http://127.0.0.1:1", "text-generation")
	rig := newUsageRig(t, usageRigConfig{
		matrix:  testLocalMatrix(t, canonical.CapTextGeneration),
		targets: []router.Target{target},
		provs:   map[string]provider.Provider{target.Endpoint: failing},
	})

	rec := rig.do(t, `{"model":"m","messages":[{"role":"user","content":"你好"}]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("状态码 = %d，期望 502；响应体: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, providerMessage) {
		t.Fatalf("响应丢失 provider 原因 %q: %s", providerMessage, body)
	}
}

// TestDispatchPreservesProviderErrorAcrossUnavailableTargets 钉死后续候选连一次真实调用
// 都没做成时，不能用它的池状态覆盖前一个候选已经返回的 provider 原因。
func TestDispatchPreservesProviderErrorAcrossUnavailableTargets(t *testing.T) {
	const providerMessage = "首个上游拒绝请求"
	firstProvider := &staticProvider{
		kind: degrade.ProviderDashScopeNative,
		err:  canonical.Newf(canonical.ClassUpstreamUnavailable, providerMessage),
	}
	secondProvider := &staticProvider{kind: degrade.ProviderDashScopeNative}
	first := nativeTarget("a", "http://127.0.0.1:1", "text-generation")
	second := nativeTarget("b", "http://127.0.0.1:1", "text-generation")
	rig := newUsageRig(t, usageRigConfig{
		matrix:  testLocalMatrix(t, canonical.CapTextGeneration),
		targets: []router.Target{first, second},
		provs: map[string]provider.Provider{
			first.Endpoint:  firstProvider,
			second.Endpoint: secondProvider,
		},
	})

	lease, err := rig.h.deps.Pools[second.CredentialPool].Acquire(nil)
	if err != nil {
		t.Fatalf("预取第二候选凭据失败: %v", err)
	}
	lease.Fail(canonical.Newf(canonical.ClassRateLimit, "预置冷却"))

	rec := rig.do(t, `{"model":"m","messages":[{"role":"user","content":"你好"}]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("状态码 = %d，期望 502；响应体: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, providerMessage) {
		t.Fatalf("响应丢失首个 provider 原因 %q: %s", providerMessage, body)
	}
	if calls := secondProvider.calls.Load(); calls != 0 {
		t.Fatalf("第二候选 provider 被调用 %d 次，期望 0——本用例要证的是调用前池不可用", calls)
	}
}

// TestDispatchCandidateMismatchDoesNotRetryCredentialsOrCooldown 钉死不可重试的候选不匹配错误
// （ClassUpstreamUnavailable + Retryable=false）在拥有多份凭据的 Provider 下只调用一次，
// 既不换凭据重试，也不冷却凭据池中的任何凭据；无后续候选时原样返回 502 并在响应体保留根因。
func TestDispatchCandidateMismatchDoesNotRetryCredentialsOrCooldown(t *testing.T) {
	const mismatchMessage = "DashScope Native 候选目标 (text-generation) 不支持请求包含的多模态内容"
	failing := &staticProvider{
		kind: degrade.ProviderDashScopeNative,
		err: &canonical.Error{
			Class:     canonical.ClassUpstreamUnavailable,
			Message:   mismatchMessage,
			Retryable: false,
		},
	}
	target := nativeTarget("a", "http://127.0.0.1:1", "text-generation")
	rig := newUsageRig(t, usageRigConfig{
		matrix:  testLocalMatrix(t, canonical.CapTextGeneration),
		targets: []router.Target{target},
		provs:   map[string]provider.Provider{target.Endpoint: failing},
		credIDs: []string{"k1", "k2"},
	})

	rec := rig.do(t, `{"model":"m","messages":[{"role":"user","content":"你好"}]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("状态码 = %d，期望 502；响应体: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, mismatchMessage) {
		t.Fatalf("响应丢失候选不匹配原因 %q: %s", mismatchMessage, body)
	}
	if calls := failing.calls.Load(); calls != 1 {
		t.Fatalf("适配器被调用 %d 次，期望 1（不可重试错误不得在同一 Provider 内换凭据重试）", calls)
	}

	pool, ok := rig.h.deps.Pools[target.CredentialPool]
	if !ok {
		t.Fatalf("未找到凭据池 %q", target.CredentialPool)
	}
	stats := pool.Stats()
	if len(stats) != 2 {
		t.Fatalf("凭据池统计数量 = %d，期望 2", len(stats))
	}
	for _, s := range stats {
		if !s.Available {
			t.Errorf("凭据 %s 变为不可用，期望保持可用", s.ID)
		}
		if s.ConsecutiveFails != 0 {
			t.Errorf("凭据 %s ConsecutiveFails = %d，期望 0", s.ID, s.ConsecutiveFails)
		}
		if !s.CooldownUntil.IsZero() {
			t.Errorf("凭据 %s CooldownUntil = %v，期望未冷却 (零值)", s.ID, s.CooldownUntil)
		}
	}
}

// TestDispatchCandidateMismatchAdvancesToNextTarget 钉死不可重试的候选不匹配错误
// （ClassUpstreamUnavailable + Retryable=false）在首个 Provider 失败后，外层候选循环
// 仍会推进到下一个候选目标并成功完成请求，同时保持首个 Provider 凭据池不被冷却。
func TestDispatchCandidateMismatchAdvancesToNextTarget(t *testing.T) {
	const mismatchMessage = "首个候选目标不匹配"
	firstProvider := &staticProvider{
		kind: degrade.ProviderDashScopeNative,
		err: &canonical.Error{
			Class:     canonical.ClassUpstreamUnavailable,
			Message:   mismatchMessage,
			Retryable: false,
		},
	}
	secondProvider := &staticProvider{
		kind: degrade.ProviderDashScopeNative,
		body: `{"id":"chatcmpl-1","object":"chat.completion","choices":[]}`,
	}
	first := nativeTarget("a", "http://127.0.0.1:1", "text-generation")
	second := nativeTarget("b", "http://127.0.0.1:1", "text-generation")
	rig := newUsageRig(t, usageRigConfig{
		matrix:  testLocalMatrix(t, canonical.CapTextGeneration),
		targets: []router.Target{first, second},
		provs: map[string]provider.Provider{
			first.Endpoint:  firstProvider,
			second.Endpoint: secondProvider,
		},
		credIDs: []string{"k1", "k2"},
	})

	rec := rig.do(t, `{"model":"m","messages":[{"role":"user","content":"你好"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；响应体: %s", rec.Code, rec.Body.String())
	}
	if calls := firstProvider.calls.Load(); calls != 1 {
		t.Fatalf("首个候选适配器被调用 %d 次，期望 1（不进行池内凭据重试）", calls)
	}
	if calls := secondProvider.calls.Load(); calls != 1 {
		t.Fatalf("第二候选适配器被调用 %d 次，期望 1（应成功 failover 到后续候选）", calls)
	}

	pool, ok := rig.h.deps.Pools[first.CredentialPool]
	if !ok {
		t.Fatalf("未找到首个凭据池 %q", first.CredentialPool)
	}
	stats := pool.Stats()
	if len(stats) != 2 {
		t.Fatalf("首个凭据池统计数量 = %d，期望 2", len(stats))
	}
	for _, s := range stats {
		if !s.Available {
			t.Errorf("首个池凭据 %s 变为不可用，期望保持可用", s.ID)
		}
		if s.ConsecutiveFails != 0 {
			t.Errorf("首个池凭据 %s ConsecutiveFails = %d，期望 0", s.ID, s.ConsecutiveFails)
		}
		if !s.CooldownUntil.IsZero() {
			t.Errorf("首个池凭据 %s CooldownUntil = %v，期望未冷却 (零值)", s.ID, s.CooldownUntil)
		}
	}
}
