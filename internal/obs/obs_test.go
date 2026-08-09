package obs

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/config"
)

// TestLoggerRedactsSecrets 是日志泄密的最后一道防线。
// 网关天生经手凭据，一次 debug 日志就可能把 API Key 写进日志系统。
func TestLoggerRedactsSecrets(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(config.Log{Level: "debug", Format: "json"}, &buf)

	log.Info("upstream call",
		"authorization", "Bearer sk-live-should-never-appear",
		"api_key", "sk-ant-secret",
		"model", "qwen-max",
	)

	out := buf.String()
	for _, secret := range []string{"sk-live-should-never-appear", "sk-ant-secret"} {
		if strings.Contains(out, secret) {
			t.Errorf("日志泄露了凭据: %s", out)
		}
	}
	if !strings.Contains(out, "qwen-max") {
		t.Error("非敏感字段应保留，否则日志失去排障价值")
	}

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("JSON 格式日志不可解析: %v", err)
	}
	if rec["authorization"] != "<redacted>" {
		t.Errorf("authorization = %v, 期望 <redacted>", rec["authorization"])
	}
}

// TestLoggerRedactsUserContent 固化「用户内容默认不进普通日志」。
func TestLoggerRedactsUserContent(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(config.Log{Level: "debug", Format: "json"}, &buf)

	log.Debug("request", "messages", "用户的私密问题", "thinking", "模型的推理过程")

	out := buf.String()
	if strings.Contains(out, "用户的私密问题") || strings.Contains(out, "模型的推理过程") {
		t.Errorf("用户内容不应进入普通日志: %s", out)
	}
}

// TestObserveUsageSkipsUnavailable 固化原则 2.5：
// 不可知的用量不记数字。记 0 会让「没数据」和「真的是 0」混为一谈。
func TestObserveUsageSkipsUnavailable(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	m.ObserveUsage("dashscope.native", canonical.UnavailableUsage())

	if n := countSamples(t, reg, "omugw_tokens_total"); n != 0 {
		t.Errorf("不可知的用量不应产生样本，实际 %d 个", n)
	}
}

// TestObserveUsageSeparatesFidelity 保证 estimated 与 authoritative 不会被
// 加进同一个计数器——那样得到的数字既不能计费也不能做容量规划。
func TestObserveUsageSeparatesFidelity(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	m.ObserveUsage("openai.compat", canonical.Usage{
		Fidelity: canonical.FidelityAuthoritative, InputTokens: 100, OutputTokens: 20,
	})
	m.ObserveUsage("openai.compat", canonical.Usage{
		Fidelity: canonical.FidelityEstimated, InputTokens: 999,
	})

	got := map[string]float64{}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != "omugw_tokens_total" {
			continue
		}
		for _, mm := range f.GetMetric() {
			var fid, kind string
			for _, l := range mm.GetLabel() {
				switch l.GetName() {
				case "fidelity":
					fid = l.GetValue()
				case "kind":
					kind = l.GetValue()
				}
			}
			got[fid+"/"+kind] = mm.GetCounter().GetValue()
		}
	}

	if got["authoritative/input"] != 100 {
		t.Errorf("authoritative/input = %v, 期望 100", got["authoritative/input"])
	}
	if got["estimated/input"] != 999 {
		t.Errorf("estimated/input = %v, 期望 999", got["estimated/input"])
	}
	if _, mixed := got["/input"]; mixed {
		t.Error("出现了无 fidelity 标签的 token 计数")
	}
}

func TestObserveErrorLabelsRetryability(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	m.ObserveError("anthropic.messages", canonical.Newf(canonical.ClassQuota, "quota"))
	m.ObserveError("anthropic.messages", canonical.Newf(canonical.ClassContentFilter, "blocked"))

	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, f := range families {
		if f.GetName() != "omugw_upstream_errors_total" {
			continue
		}
		for _, mm := range f.GetMetric() {
			var class, retryable string
			for _, l := range mm.GetLabel() {
				switch l.GetName() {
				case "class":
					class = l.GetValue()
				case "retryable":
					retryable = l.GetValue()
				}
			}
			seen[class] = retryable
		}
	}

	// 配额问题换个凭据可能成功；内容拦截换谁都失败。
	if seen["quota"] != "true" {
		t.Errorf("quota 的 retryable 标签 = %q, 期望 true", seen["quota"])
	}
	if seen["content_filter"] != "false" {
		t.Errorf("content_filter 的 retryable 标签 = %q, 期望 false", seen["content_filter"])
	}
}

// TestVerdictSeparatesDegradeFromEmulate 固化两者不能合并计数。
//
// 降级 = 客户端少拿到了东西；模拟 = 客户端拿全了但由网关垫着。
// 合并之后运维就看不出「重启会影响多少请求」——而那正是模拟能力的风险所在。
func TestVerdictSeparatesDegradeFromEmulate(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	m.ObserveVerdict("openai.responses", "dashscope.native",
		[]string{"structured_output"}, []string{"stateful_conversation"})

	if n := countSamples(t, reg, "omugw_degradations_total"); n != 1 {
		t.Errorf("降级计数样本数 = %d, 期望 1", n)
	}
	if n := countSamples(t, reg, "omugw_emulations_total"); n != 1 {
		t.Errorf("模拟计数样本数 = %d, 期望 1", n)
	}
}

// TestNotImplementedIsCountedSeparately 固化「打到 PLANNED 路径」不是故障。
//
// 它衡量的是期望与现实的差距——有人在用一条还没建好的路。这个数字上涨说明
// 该排期了，不是该修 bug，所以不能混进 upstream_errors。
func TestNotImplementedIsCountedSeparately(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)

	m.ObserveNotImplemented("openai.responses", "anthropic.messages")

	if n := countSamples(t, reg, "omugw_not_implemented_total"); n != 1 {
		t.Errorf("未实现计数样本数 = %d, 期望 1", n)
	}
	if n := countSamples(t, reg, "omugw_upstream_errors_total"); n != 0 {
		t.Error("未实现不是上游错误，不该计入 upstream_errors")
	}
}

func countSamples(t *testing.T, reg *prometheus.Registry, name string) int {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() == name {
			return len(f.GetMetric())
		}
	}
	return 0
}
