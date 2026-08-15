package degrade_test

import (
	"testing"

	"github.com/yobo2u/omugw/internal/degrade"
)

// TestExternalPackageReadsIdentityThroughAccessors 站在包外验证只读访问器够用。
//
// 它待在 degrade_test 外部包里是全部意义所在：包内测试看得见私有字段，证明不了
// 「外面的人只靠导出的东西也能把事做完」。选路偏好、启动期对账、文档生成都在
// 包外，它们只需要读身份与快通道事实——一旦这条测试要改成读字段才能过，
// 就说明访问器漏了某个真实用途，而不是说明字段该重新导出。
func TestExternalPackageReadsIdentityThroughAccessors(t *testing.T) {
	m, err := degrade.Phase1()
	if err != nil {
		t.Fatal(err)
	}

	var fastPaths int
	for _, r := range m.Routes() {
		if r.InProtocol() == "" {
			t.Error("已注册路径的入站协议不该是零值")
		}
		if r.OutProvider() == "" {
			t.Errorf("路径 %s 的出站 Provider 不该是零值", r.InProtocol())
		}
		if r.IsHomogeneous() {
			fastPaths++
		}
	}
	// 一条快通道都读不到，说明访问器恒假；这类退化不会让上面的循环失败，
	// 却会让包外的选路排序悄悄丢掉「同源优先」。
	if fastPaths == 0 {
		t.Error("包外读不到任何同源快通道路径，IsHomogeneous 可能恒为假")
	}
}

// TestExternalPackageSeesFastPathDecideRanking 把快通道事实与它的后果绑在一起。
//
// 上一条只证明「读得到」，这条证明「读到的那个 bool 真的是选路依据」：
// openai.realtime 入站下两个候选中只有同源那条排得进第一，把快通道读成恒假的
// 实现会在这里把顺序整个换掉——而那正是从前直接写字段能造成的破坏。
func TestExternalPackageSeesFastPathDecideRanking(t *testing.T) {
	m, err := degrade.Phase1()
	if err != nil {
		t.Fatal(err)
	}

	const in = degrade.ProtoOpenAIRealtime
	ranked := m.RankDesign(in, []degrade.Provider{
		degrade.ProviderOpenAIRealtime,
		degrade.ProviderDashScopeWSRealtime,
	})
	if len(ranked) == 0 {
		t.Fatal("入站协议 openai.realtime 应当有已注册的候选路径")
	}

	first, ok := m.Route(in, ranked[0])
	if !ok {
		t.Fatalf("排在最前的候选 %s 没有注册路径", ranked[0])
	}
	if !first.IsHomogeneous() {
		t.Errorf("排在最前的候选 %s 应是同源快通道，同源优先没有生效", ranked[0])
	}
}
