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
