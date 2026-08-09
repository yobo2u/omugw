package obs

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/yobo2u/omugw/internal/canonical"
)

// Metrics 是网关的核心指标集。
type Metrics struct {
	Requests       *prometheus.CounterVec
	Duration       *prometheus.HistogramVec
	FirstByte      *prometheus.HistogramVec
	UpstreamError  *prometheus.CounterVec
	Degradations   *prometheus.CounterVec
	Emulations     *prometheus.CounterVec
	NotImplemented *prometheus.CounterVec
	Tokens         *prometheus.CounterVec
	StreamAborted  *prometheus.CounterVec
}

// NewMetrics 注册全部指标。
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "omugw_requests_total",
			Help: "按入站协议、出站 Provider 与结果分类统计的请求数。",
		}, []string{"inbound", "outbound", "outcome"}),

		Duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "omugw_request_duration_seconds",
			Help:    "请求全程耗时。",
			Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 300},
		}, []string{"inbound", "outbound"}),

		// 首字节耗时单列，且带 fast_path 标签。
		//
		// TTFT 是这个网关的核心卖点，也是「同源快通道 vs Canonical 转换」
		// 差异的唯一可观测证据。没有这个指标，快通道到底有没有生效只能靠猜。
		FirstByte: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "omugw_first_byte_seconds",
			Help:    "从收到请求到发出首字节的耗时。",
			Buckets: []float64{.01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 120},
		}, []string{"inbound", "outbound", "fast_path"}),

		UpstreamError: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "omugw_upstream_errors_total",
			Help: "按统一错误分类统计的上游错误数。",
		}, []string{"outbound", "class", "retryable"}),

		// 降级计数直接对应降级矩阵里的 DEGRADE 格子。
		// 某项能力被降级的次数突然上涨，说明有客户端在用一条有损路径。
		Degradations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "omugw_degradations_total",
			Help: "按能力统计的降级次数（转换路径丢弃了该能力的部分语义）。",
		}, []string{"inbound", "outbound", "capability"}),

		// 模拟计数对应矩阵里的 EMULATE 格子。
		//
		// 它与降级计数是两回事：降级意味着客户端少拿到了东西，模拟意味着
		// 客户端拿全了、但那份完整性由网关垫着。这个数字直接告诉运维
		// 「重启会影响多少请求」——它正是重启时会出事的那批。
		Emulations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "omugw_emulations_total",
			Help: "按能力统计的网关模拟次数（上游不提供，由网关自行实现）。",
		}, []string{"inbound", "outbound", "capability"}),

		// 打到 PLANNED 路径上的请求数。
		//
		// 单列是因为它衡量的不是故障，而是**期望与现实的差距**：
		// 有人在用一条还没建好的路。这个数字上涨说明该排期了，不是该修 bug。
		NotImplemented: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "omugw_not_implemented_total",
			Help: "打到已设计但尚未实现的转换路径上的请求数。",
		}, []string{"inbound", "outbound"}),

		// token 计数必须带 fidelity 标签。
		//
		// 把 estimated 和 authoritative 加在同一个计数器里，得到的数字既不能
		// 用来计费也不能用来容量规划——它只是两种不同东西的和。
		Tokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "omugw_tokens_total",
			Help: "按可信等级与类别统计的 token 数。只有 fidelity=authoritative 可用于计费。",
		}, []string{"outbound", "fidelity", "kind"}),

		// 首字节之后中断的流。这类请求不可重试且 usage 不可知，
		// 是计费缺口的直接来源，必须单独可观测。
		StreamAborted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "omugw_stream_aborted_total",
			Help: "首字节发出后被中断的流式请求数（不可重试，用量不可知）。",
		}, []string{"inbound", "outbound", "class"}),
	}

	reg.MustRegister(
		m.Requests, m.Duration, m.FirstByte,
		m.UpstreamError, m.Degradations, m.Emulations, m.NotImplemented,
		m.Tokens, m.StreamAborted,
	)
	return m
}

// ObserveVerdict 记录一次能力裁决的结果。
//
// 降级与模拟分开计数：前者意味着客户端少拿到了东西，后者意味着客户端拿全了、
// 但那份完整性由网关垫着。把两者合并会让运维看不出「重启会影响多少请求」。
func (m *Metrics) ObserveVerdict(inbound, outbound string, degraded, emulated []string) {
	for _, c := range degraded {
		m.Degradations.WithLabelValues(inbound, outbound, c).Inc()
	}
	for _, c := range emulated {
		m.Emulations.WithLabelValues(inbound, outbound, c).Inc()
	}
}

// ObserveNotImplemented 记录一次打到 PLANNED 路径上的请求。
func (m *Metrics) ObserveNotImplemented(inbound, outbound string) {
	m.NotImplemented.WithLabelValues(inbound, outbound).Inc()
}

// ObserveUsage 按可信等级记录 token 用量。
func (m *Metrics) ObserveUsage(outbound string, u canonical.Usage) {
	if u.Fidelity == canonical.FidelityUnavailable || u.Fidelity == canonical.FidelityUnknown {
		// 不可知的用量不记数字。记 0 会让「没数据」和「真的是 0」混在一起。
		return
	}
	f := string(u.Fidelity)

	add := func(kind string, n int64) {
		if n > 0 {
			m.Tokens.WithLabelValues(outbound, f, kind).Add(float64(n))
		}
	}
	add("input", u.InputTokens)
	add("output", u.OutputTokens)
	add("reasoning", u.ReasoningTokens)
	add("cache_read", u.CacheReadInputTokens)
	add("cache_write", u.CacheWriteInputTokens)
	add("audio_input", u.AudioInputTokens)
	add("audio_output", u.AudioOutputTokens)
}

// ObserveError 记录一次上游错误。
func (m *Metrics) ObserveError(outbound string, e *canonical.Error) {
	if e == nil {
		return
	}
	retryable := "false"
	if e.Retryable {
		retryable = "true"
	}
	m.UpstreamError.WithLabelValues(outbound, string(e.Class), retryable).Inc()
}
