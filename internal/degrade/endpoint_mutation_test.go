package degrade

import (
	"errors"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

// wantNotImplemented 断言这次裁决以 not_implemented/501 收场，并把错误交回去继续验消息。
//
// 501 与 422 的区别是这套闸门的全部意义所在：前者说「等实现」，后者说「改请求」。
// 只断言「失败了」会让一个把 501 说成 422 的实现照样绿，而客户端会照着一个
// 永远不会变的结论去改请求。
func wantNotImplemented(t *testing.T, err error, what string) *canonical.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("%s：应当失败，实际放行了", what)
	}
	var cerr *canonical.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("%s：应返回 *canonical.Error，实际为 %T", what, err)
	}
	if cerr.Class != canonical.ClassNotImplemented || cerr.HTTPStatus() != 501 {
		t.Fatalf("%s：应为 not_implemented/501（等实现），实际 %q/%d",
			what, cerr.Class, cerr.HTTPStatus())
	}
	return cerr
}

// TestEndpointGateActuallyBites 咬住端点闸门的三条腿：跨门并集、未开的门、
// 空能力集不豁免。
//
// 三条腿必须待在一起，否则每一条单独看都能被一个错误的实现骗过：只验跨门并集，
// 一个整个跳过端点闸门的 Check 照样绿（逐项能力闸门会替它补刀）；只验未开门带
// 能力的情形，同上；只验「未开门 + 空能力集失败」，一个「空能力集一律拒绝」的
// 实现也能骗过它。最后那条腿因此配了对照组——同样的空能力集打在已开的门上必须
// 成功，失败才归因得到「这扇门没开」。
func TestEndpointGateActuallyBites(t *testing.T) {
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}
	r := mustRoute(t, m, ProtoDashScopeNative, ProviderDashScopeNative)

	// 前提检查：tool_calling 确实兑现在文本门上。它若不成立，下面那条断言
	// 就不再是「跨门」，测试会在一个空转的前提上装作自己还咬着人。
	if !r.Redeems(EndpointDashScopeTextGeneration, canonical.CapToolCalling) {
		t.Fatal("前提没了：tool_calling 在文本门未兑现，跨门并集这条腿无从测起")
	}
	if r.Redeems(EndpointDashScopeMultimodal, canonical.CapToolCalling) {
		t.Fatal("前提没了：tool_calling 已兑现在多模态门上，跨门并集这条腿无从测起")
	}

	// 腿一：带着文本门的 tool_calling 敲多模态门，必须 501。
	// 若兑现查询退化成「这条路径某扇门兑现过就算」，请求会被送进一扇根本没有
	// 这项能力的门——这正是 8/18 并集口径的运行时形态。
	_, err = m.Check(
		Inbound{Protocol: ProtoDashScopeNative, Endpoint: EndpointDashScopeMultimodal},
		ProviderDashScopeNative,
		[]canonical.Capability{canonical.CapTextGeneration, canonical.CapToolCalling})
	cerr := wantNotImplemented(t, err, "多模态门收到 tool_calling")
	if !strings.Contains(cerr.Message, string(canonical.CapToolCalling)) ||
		!strings.Contains(cerr.Message, string(EndpointDashScopeMultimodal)) {
		t.Errorf("错误应同时点名能力与端点，否则运维查不出是哪扇门缺哪项: %s", cerr.Message)
	}

	const unopened = Endpoint("/api/v1/services/aigc/never-opened")

	// 腿二：没开的门，带着已兑现的能力也得 501，且消息点名端点。
	_, err = m.Check(Inbound{Protocol: ProtoDashScopeNative, Endpoint: unopened},
		ProviderDashScopeNative,
		[]canonical.Capability{canonical.CapTextGeneration})
	cerr = wantNotImplemented(t, err, "未开门带已兑现的能力")
	if !strings.Contains(cerr.Message, string(unopened)) {
		t.Errorf("错误消息应点名端点: %s", cerr.Message)
	}

	// 腿三对照组：同样的空能力集打在已开的门上必须放行。
	// 少了这一组，下面那条断言就分不清失败来自「门没开」还是「caps 是空的」。
	if _, err := m.Check(
		Inbound{Protocol: ProtoDashScopeNative, Endpoint: EndpointDashScopeMultimodal},
		ProviderDashScopeNative, nil); err != nil {
		t.Fatalf("已开的门收到空能力集应当放行，实际被拦: %v", err)
	}

	// 腿三：请求带了什么能力是一回事，敲的门开没开是另一回事——后者是入口约束，
	// 不因 caps 为空而豁免。
	_, err = m.Check(Inbound{Protocol: ProtoDashScopeNative, Endpoint: unopened},
		ProviderDashScopeNative, nil)
	cerr = wantNotImplemented(t, err, "未开门收到空能力集")
	if !strings.Contains(cerr.Message, string(unopened)) {
		t.Errorf("错误消息应点名端点: %s", cerr.Message)
	}
}

// TestEndpointScoreIsNotRouteAggregate 咬住「路径级并集聚合分」变异。
//
// 两扇门各报 5/18，且都不等于并集的 8/18——并集那 8 项没有任何一扇真实存在的门
// 同时提供，给它记分就是为一个不存在的门背书。分数只是症状，兑现集合才是病灶，
// 所以两头都验：算错分要红，把别扇门的能力记到这扇门上也要红。
func TestEndpointScoreIsNotRouteAggregate(t *testing.T) {
	// 并集那 8 项写成字面量，来源是设计文档「分数」一节。
	// 从矩阵反推期望值的话，期望会跟着变异一起动，这个锁就永远咬不住人。
	unionCaps := []canonical.Capability{
		canonical.CapTextGeneration,
		canonical.CapStreaming,
		canonical.CapToolCalling,
		canonical.CapReasoning,
		canonical.CapWebSearch,
		canonical.CapVisionInput,
		canonical.CapAudioInput,
		canonical.CapVideoInput,
	}
	const union = 8.0 / 18.0

	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}
	r := mustRoute(t, m, ProtoDashScopeNative, ProviderDashScopeNative)

	for _, ep := range []Endpoint{EndpointDashScopeTextGeneration, EndpointDashScopeMultimodal} {
		got := r.Preservation(m.Availability(), ep).AvailableScore()
		if want := 5.0 / 18.0; got != want {
			t.Errorf("门 %s 可用分 = %.3f，期望 %.3f（18 项中 5 项已投放）", ep, got, want)
		}
		if got == union {
			t.Errorf("门 %s 报出了并集分 %.3f——并集不对应任何一扇真实存在的门", ep, union)
		}

		var missing []canonical.Capability
		for _, c := range unionCaps {
			if !r.Redeems(ep, c) {
				missing = append(missing, c)
			}
		}
		if len(missing) != 3 {
			t.Errorf("门 %s 应恰好缺并集 8 项里的 3 项，实际缺 %v——"+
				"一扇门持有全部 8 项，就是并集口径回来了", ep, missing)
		}
	}

	// 设计列是路径级的，与敲哪扇门无关：同源直通最终仍是零损失。
	// 兑现下沉到端点，压低的只该是当前可用那一列。
	if got := r.Preservation(m.Availability(), Endpoint("")).DesignScore(); got != 1.0 {
		t.Errorf("设计分 = %.3f，期望 1.000", got)
	}
}
