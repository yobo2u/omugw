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
	WSUsageRecords *prometheus.CounterVec
	WSTokens       *prometheus.CounterVec
	WSCharacters   *prometheus.CounterVec
	WSDiagnostics  *prometheus.CounterVec
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

		// 单独计记录数，才能区分权威零值、字符计价与根本没拿到用量。
		WSUsageRecords: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "omugw_ws_usage_records_total",
			Help: "按协议、来源、原始单位与可信等级统计的 WebSocket 用量记录数。",
		}, []string{"protocol", "source", "unit", "fidelity"}),
		WSTokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "omugw_ws_tokens_total",
			Help: "WebSocket 已结用量的 token 分项；音频分项已包含在输入输出总数内。",
		}, []string{"protocol", "source", "fidelity", "kind"}),
		WSCharacters: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "omugw_ws_characters_total",
			Help: "WebSocket 上游权威字符计量，不能换算或叠加到 token。",
		}, []string{"protocol", "source", "fidelity"}),
		WSDiagnostics: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "omugw_ws_diagnostics_total",
			Help: "WebSocket 用量观测诊断，不包含 ID、模型或上游正文。",
		}, []string{"protocol", "reason"}),
	}

	reg.MustRegister(
		m.Requests, m.Duration, m.FirstByte,
		m.UpstreamError, m.Degradations, m.Emulations, m.NotImplemented,
		m.Tokens, m.StreamAborted,
		m.WSUsageRecords, m.WSTokens, m.WSCharacters, m.WSDiagnostics,
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

// ObserveWSUsage 保留来源与可信等级；未知不是数值零。
func (m *Metrics) ObserveWSUsage(protocol, source string, u canonical.Usage) {
	if !wsProtocol(protocol) || !wsSource(source) || u.Validate() != nil {
		return
	}
	f := string(u.Fidelity)
	m.WSUsageRecords.WithLabelValues(protocol, source, "tokens", f).Inc()
	if u.Fidelity == canonical.FidelityUnavailable {
		return
	}
	for _, item := range [...]struct {
		kind string
		n    int64
	}{
		{"input", u.InputTokens}, {"output", u.OutputTokens},
		{"audio_input", u.AudioInputTokens}, {"audio_output", u.AudioOutputTokens},
	} {
		if item.n > 0 {
			m.WSTokens.WithLabelValues(protocol, source, f, item.kind).Add(float64(item.n))
		}
	}
}

// ObserveWSCharacters 不调用 ObserveWSUsage，防止产生一份虚构的零 token 账。
func (m *Metrics) ObserveWSCharacters(protocol, source string, characters int64) {
	if !wsProtocol(protocol) || !wsSource(source) || characters < 0 {
		return
	}
	f := string(canonical.FidelityAuthoritative)
	m.WSUsageRecords.WithLabelValues(protocol, source, "characters", f).Inc()
	m.WSCharacters.WithLabelValues(protocol, source, f).Add(float64(characters))
}

// ObserveWSDiagnostic 的白名单避免把上游错误文本扩成无限标签集。
func (m *Metrics) ObserveWSDiagnostic(protocol, reason string) {
	if !wsProtocol(protocol) {
		return
	}
	switch reason {
	case "usage_missing", "usage_unverified", "usage_invalid", "usage_ambiguous",
		"usage_conflict", "usage_unfinished", "ledger_limit", "invalid_event":
	default:
		reason = "unknown"
	}
	m.WSDiagnostics.WithLabelValues(protocol, reason).Inc()
}

func wsProtocol(protocol string) bool {
	return protocol == "dashscope.realtime" || protocol == "openai.realtime" || protocol == "dashscope.inference"
}

func wsSource(source string) bool {
	return source == "response" || source == "transcription" || source == "session"
}
