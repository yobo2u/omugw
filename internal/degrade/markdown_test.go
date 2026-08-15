package degrade

import (
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

// twoDoorMatrix 取 Phase 1 矩阵，并断言 Native 路径确实开了两扇门。
//
// 多门是主表「见端点细分」分支唯一的可达条件。真实矩阵已经是多门，就直接用它——
// 另造一份只会让文档断言验的是一个假矩阵。这道断言是这些用例的前提检查：
// 哪天多模态门被撤掉，下面的用例会因为「前提没了」而不是「断言不满足」失败，
// 归因一眼可见。
func twoDoorMatrix(t *testing.T) *Matrix {
	t.Helper()
	m, err := Phase1()
	if err != nil {
		t.Fatal(err)
	}
	if eps := mustRoute(t, m, ProtoDashScopeNative, ProviderDashScopeNative).Endpoints(); len(eps) != 2 {
		t.Fatalf("Native 路径应有两扇门，实际 %v——多门分支没有可达条件了", eps)
	}
	return m
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

// TestNoPathLevelAvailableAggregateInDoc 固化并集口径已被否决：文档里不存在
// 路径级「当前可用」聚合分。
//
// 三条断言咬同一件事的三个面，缺一个就有实现能骗过去：主表 Native 那一行的
// 可用格必须是「见端点细分」（只验文档某处含这四个字，主表照样可以印一个并集分）；
// 两条端点行各自报自己的分（只验主表指路，分数可以在小节里变成并集）；
// 0.444 全文不得出现（前两条都对，脚注里仍可能有人写下这个数）。
//
// 与 TestMarkdownShowsEndpointBreakdown 分工：那条管「每扇门列了哪些能力」，
// 这条管「可用分只属于门，不属于路径」。
func TestNoPathLevelAvailableAggregateInDoc(t *testing.T) {
	doc := twoDoorMatrix(t).Markdown()

	// 主表行带反引号，端点细分行不带——用它区分两张表，
	// 免得断言落在另一张表上还以为自己验过了。
	mainRow := findRow(t, doc, "| `dashscope.native` | `dashscope.native` |")
	if got := lastCell(mainRow); got != "见端点细分" {
		t.Errorf("多门路径主表的可用格 = %q，应为「见端点细分」——"+
			"任何数字都是一扇不存在的门的分数\n整行: %s", got, mainRow)
	}

	for _, ep := range []Endpoint{EndpointDashScopeTextGeneration, EndpointDashScopeMultimodal} {
		row := findRow(t, doc, "| dashscope.native | dashscope.native | "+string(ep)+" |")
		if got, want := lastCell(row), "0.278（18 项中 5 项已投放）"; got != want {
			t.Errorf("门 %s 的可用格 = %q，期望 %q\n整行: %s", ep, got, want, row)
		}
	}

	if strings.Contains(doc, "0.444") {
		t.Error("文档不得出现并集分 0.444——两门并集不对应任何一扇真实存在的门")
	}
}

// findRow 取出以 prefix 开头的唯一一行。
//
// 命中零行或多行都直接判死：前者说明断言的目标行改了形状，后者说明 prefix
// 分辨不了两张表——两种情况下继续断言都是在验一个自己也说不清是谁的东西。
func findRow(t *testing.T, doc, prefix string) string {
	t.Helper()
	var hits []string
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(line, prefix) {
			hits = append(hits, line)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("以 %q 开头的行应恰有一条，实际 %d 条: %v", prefix, len(hits), hits)
	}
	return hits[0]
}

// lastCell 返回表格行最后一格的内容。可用列永远是最后一列。
func lastCell(row string) string {
	cells := strings.Split(strings.TrimSuffix(strings.TrimSpace(row), "|"), "|")
	return strings.TrimSpace(cells[len(cells)-1])
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
// 刻意不用 Phase1：真实矩阵里 Native 已经是两扇门，而单门是另一条呈现分支，
// 只有自己造样本才测得到。拿真实矩阵当「单门」样本，等于把一个会随投放进度变的
// 事实钉进断言里——测试会在别人做对事情的时候变红，然后被当成噪音改掉。
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
