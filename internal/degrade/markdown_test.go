package degrade

import (
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

// twoDoorMatrix 造一份 Native 路径开了两扇门的矩阵。
//
// 多门是主表「见端点细分」分支唯一的可达条件，而 Phase 1 当前只有单门路径——
// 不在测试里造出多门，那条分支与它指向的小节就都没人验证过，
// 「指向一个不存在的小节」这种错误正是这样溜过去的。
func twoDoorMatrix(t *testing.T) *Matrix {
	t.Helper()
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}
	// 第二扇门在 Build 之前挂上：路径封口后不可再兑现，
	// 而这扇门也要跟真实投放一样过完整套 Build 校验。
	return rebuiltMatrix(t, m, func(r *Route) {
		if r.In != ProtoDashScopeNative || r.Out != ProviderDashScopeNative {
			return
		}
		r.Redeem(EndpointDashScopeMultimodal,
			canonical.CapTextGeneration,
			canonical.CapStreaming,
			canonical.CapVisionInput,
			canonical.CapAudioInput,
			canonical.CapVideoInput,
		)
	})
}

// TestMarkdownShowsEndpointBreakdown 固化端点细分小节的呈现：
// 每扇已开门逐行列出端点、已投放能力与端点相对可用分。
func TestMarkdownShowsEndpointBreakdown(t *testing.T) {
	doc := twoDoorMatrix(t).Markdown()

	if !strings.Contains(doc, "### 端点细分") {
		t.Fatal("文档应包含端点细分小节")
	}
	for _, ep := range []string{
		string(EndpointDashScopeTextGeneration),
		string(EndpointDashScopeMultimodal),
	} {
		if !strings.Contains(doc, ep) {
			t.Errorf("端点细分应列出 %s", ep)
		}
	}
	if n := strings.Count(doc, "0.278（18 项中 5 项已投放）"); n != 2 {
		t.Errorf("两扇门应各报 0.278（18 项中 5 项已投放），实际出现 %d 次", n)
	}

	// 逐门的整行必须精确成形：已投放能力按 AllCapabilities 顺序展开，
	// 分数是这扇门自己的。行错了，读者会照着一个不存在的能力集去发请求。
	for _, want := range []string{
		"| dashscope.native | dashscope.native | " +
			string(EndpointDashScopeTextGeneration) +
			" | text_generation, streaming, tool_calling, reasoning, web_search" +
			" | 0.278（18 项中 5 项已投放） |",
		"| dashscope.native | dashscope.native | " +
			string(EndpointDashScopeMultimodal) +
			" | text_generation, streaming, vision_input, audio_input, video_input" +
			" | 0.278（18 项中 5 项已投放） |",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("端点细分缺少这一行:\n%s", want)
		}
	}
}

// TestMarkdownNeverShowsRouteAggregateAvailable 固化并集口径已被否决：
// 多门路径的主表可用列只指路到端点细分，绝不出现 8/18 = 0.444 的并集分。
func TestMarkdownNeverShowsRouteAggregateAvailable(t *testing.T) {
	doc := twoDoorMatrix(t).Markdown()

	if strings.Contains(doc, "0.444") {
		t.Fatal("文档不得出现并集分 0.444——两门并集不对应任何一扇真实存在的门")
	}
	if !strings.Contains(doc, "见端点细分") {
		t.Fatal("多门路径的主表可用列应为「见端点细分」")
	}
}

// TestMarkdownIsDeterministic 防的是「文档每次生成都不一样」。
//
// 门与路径都从 map 推导，遍历顺序天然随机；输出不排序，
// TestDegradationMatrixDocIsCurrent 就会时绿时红，最后被当成 flaky 关掉。
func TestMarkdownIsDeterministic(t *testing.T) {
	for i := range 8 {
		if got, want := twoDoorMatrix(t).Markdown(), twoDoorMatrix(t).Markdown(); got != want {
			t.Fatalf("第 %d 次生成与上一次不一致，端点细分的排序不稳定", i)
		}
	}
}

// singleDoorMatrix 造一份只有一条路径、且只开一扇门的矩阵。
//
// 刻意不用 Phase1：那份矩阵眼下恰好全是单门，但任务 5 会给 Native 开第二扇门。
// 拿它当「单门」的样本，等于把一个正要变的事实钉进断言里——测试会在别人做对
// 事情的时候变红，然后被当成噪音改掉。
func singleDoorMatrix(t *testing.T) *Matrix {
	t.Helper()
	m := NewMatrix()
	if err := m.Add(NewRoute(ProtoDashScopeNative, ProviderDashScopeNative).
		MarkHomogeneous().
		Pass(ExpressibleSet(ProtoDashScopeNative)...).
		Redeem(EndpointDashScopeTextGeneration,
			canonical.CapTextGeneration,
			canonical.CapStreaming,
			canonical.CapToolCalling,
			canonical.CapReasoning,
			canonical.CapWebSearch,
		).
		Build()); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestMarkdownSingleDoorRouteKeepsInlineScore 固化单门路径的主表呈现。
//
// 端点细分小节是给多门路径指路用的；单门路径的可用分本来就只有一个来源，
// 把它也赶进小节，等于让读者为一件本来一眼可见的事多翻一次页。
func TestMarkdownSingleDoorRouteKeepsInlineScore(t *testing.T) {
	doc := singleDoorMatrix(t).Markdown()

	if strings.Contains(doc, "见端点细分") {
		t.Error("单门路径的主表不该出现「见端点细分」")
	}
	if !strings.Contains(doc, "0.278（18 项中 5 项已投放）") {
		t.Error("单门路径的主表应直接给出该门的可用分")
	}

	// 小节仍然要有：读者不必反推哪条路径开了哪扇门。
	if !strings.Contains(doc, "### 端点细分") {
		t.Fatal("已有开门的矩阵应包含端点细分小节")
	}
	if !strings.Contains(doc, "| "+string(EndpointDashScopeTextGeneration)+" | ") {
		t.Errorf("端点细分应列出已开门 %s", EndpointDashScopeTextGeneration)
	}
}

// TestMarkdownOmitsEndpointBreakdownWhenNoDoorIsOpen 防的是「空小节」。
//
// 一条门都没开时印一张只有表头的表，读者会以为是渲染坏了，
// 而真相是「还没有任何东西投放」。
func TestMarkdownOmitsEndpointBreakdownWhenNoDoorIsOpen(t *testing.T) {
	m := NewMatrix()
	if err := m.Add(NewRoute(ProtoOpenAIChat, ProviderOpenAICompat).
		MarkHomogeneous().
		Pass(ExpressibleSet(ProtoOpenAIChat)...).
		Build()); err != nil {
		t.Fatal(err)
	}

	if doc := m.Markdown(); strings.Contains(doc, "### 端点细分") {
		t.Error("零开门的矩阵不该输出端点细分小节")
	}
}
