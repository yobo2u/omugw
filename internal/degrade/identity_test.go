package degrade

import (
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

// TestRouteIdentityAccessorsReportConstructorArguments 锁住只读访问器与
// NewRoute 的对应关系。
//
// 访问器是身份字段私有化之后外部唯一的读法，它要是把 in 与 out 报反了，
// 启动期对账、日志、文档三处会一起指错路径，而每一处单独看都自洽。
func TestRouteIdentityAccessorsReportConstructorArguments(t *testing.T) {
	// 刻意选一条「入站与出站不同族」的坐标：同族的 in/out 字符串相近，
	// 报反了肉眼与断言都容易放过。
	r := NewRoute(ProtoOpenAIChat, ProviderAnthropicMessages)

	if got := r.InProtocol(); got != ProtoOpenAIChat {
		t.Errorf("InProtocol() = %q，应为 %q", got, ProtoOpenAIChat)
	}
	if got := r.OutProvider(); got != ProviderAnthropicMessages {
		t.Errorf("OutProvider() = %q，应为 %q", got, ProviderAnthropicMessages)
	}
}

// TestIsHomogeneousReportsMarkBeforeBuildAndSealsAfter 锁住快通道事实的两段：
// Build 之前 MarkHomogeneous 说了算，Build 之后谁也改不动。
//
// 两段必须在同一条路径上验：只验前一段，一个「永远返回 true」的访问器也能过；
// 只验后一段，一个「永远返回 false」的访问器同样能过。
func TestIsHomogeneousReportsMarkBeforeBuildAndSealsAfter(t *testing.T) {
	r := NewRoute(ProtoDashScopeNative, ProviderDashScopeNative).
		Pass(ExpressibleSet(ProtoDashScopeNative)...).
		Redeem(EndpointDashScopeTextGeneration, canonical.CapTextGeneration)

	if r.IsHomogeneous() {
		t.Fatal("未标记的新路径不该报成同源快通道")
	}

	r.MarkHomogeneous()
	if !r.IsHomogeneous() {
		t.Fatal("Build 之前 MarkHomogeneous 应当立即生效")
	}

	built, err := r.Build()
	if err != nil {
		t.Fatal(err)
	}
	if !built.IsHomogeneous() {
		t.Fatal("Build 不该丢掉封口前标记的快通道事实")
	}

	// 封口之后再标记：对一条从未标记过的路径调用，若闸门失效就会翻成 true。
	other, err := NewRoute(ProtoOpenAIChat, ProviderOpenAICompat).
		Pass(ExpressibleSet(ProtoOpenAIChat)...).
		Redeem(EndpointOpenAIChat, canonical.CapTextGeneration).
		Build()
	if err != nil {
		t.Fatal(err)
	}
	if other.IsHomogeneous() {
		t.Fatal("测试前提不成立：这条路径不该已被标记为快通道")
	}
	other.MarkHomogeneous()
	if other.IsHomogeneous() {
		t.Error("MarkHomogeneous 在 Build 之后仍然生效，封口被绕过")
	}
}

// TestDerivedRouteCarriesIdentityAndFastPathFact 保证派生路径的身份换成了新
// 坐标，而快通道事实按既有约定继承。
//
// Derive 是唯一一处「新路径的身份不来自调用点直觉」的地方：基准路径的 in/out
// 与派生参数同时在场，抄错一个就会让派生路径顶着基准的身份进矩阵，撞上重复键
// 或替基准路径背书。
func TestDerivedRouteCarriesIdentityAndFastPathFact(t *testing.T) {
	base := NewRoute(ProtoOpenAIChat, ProviderOpenAICompat).
		MarkHomogeneous().
		Pass(ExpressibleSet(ProtoOpenAIChat)...)

	derived := base.Derive(ProtoOpenAIResponses, ProviderAnthropicMessages)

	if got := derived.InProtocol(); got != ProtoOpenAIResponses {
		t.Errorf("派生路径 InProtocol() = %q，应为 %q", got, ProtoOpenAIResponses)
	}
	if got := derived.OutProvider(); got != ProviderAnthropicMessages {
		t.Errorf("派生路径 OutProvider() = %q，应为 %q", got, ProviderAnthropicMessages)
	}
	if !derived.IsHomogeneous() {
		t.Error("派生路径应继承基准路径的快通道标记")
	}
}
