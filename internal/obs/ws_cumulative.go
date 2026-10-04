package obs

import (
	"math"

	"github.com/yobo2u/omugw/internal/canonical"
)

// WSUsageUnit 与记录的 presence 同在，未知不能变成零 token。
type WSUsageUnit string

const (
	UnitUnknown    WSUsageUnit = "unknown"
	UnitTokens     WSUsageUnit = "tokens"
	UnitCharacters WSUsageUnit = "characters"
	UnitSeconds    WSUsageUnit = "seconds"
)

type WSUsageDelta struct {
	Unit                                  WSUsageUnit
	InputTokens, OutputTokens, Characters int64
	Seconds                               float64
}

// ObserveWSUsageDelta 只发布已核验的累计差额，不冒充任务已完整结算。
func (m *Metrics) ObserveWSUsageDelta(protocol, source string, d WSUsageDelta) {
	if !wsProtocol(protocol) || !wsSource(source) || d.InputTokens < 0 || d.OutputTokens < 0 || d.Characters < 0 || d.Seconds < 0 || math.IsNaN(d.Seconds) || math.IsInf(d.Seconds, 0) {
		return
	}
	f := string(canonical.FidelityAuthoritative)
	switch d.Unit {
	case UnitTokens:
		if d.Characters != 0 || d.Seconds != 0 || d.InputTokens > math.MaxInt64-d.OutputTokens {
			return
		}
		m.WSTokens.WithLabelValues(protocol, source, f, "input").Add(float64(d.InputTokens))
		m.WSTokens.WithLabelValues(protocol, source, f, "output").Add(float64(d.OutputTokens))
	case UnitCharacters:
		if d.InputTokens != 0 || d.OutputTokens != 0 || d.Seconds != 0 {
			return
		}
		m.WSCharacters.WithLabelValues(protocol, source, f).Add(float64(d.Characters))
	case UnitSeconds:
		if d.InputTokens != 0 || d.OutputTokens != 0 || d.Characters != 0 {
			return
		}
		m.WSAudioInputSeconds.WithLabelValues(protocol, source, f).Add(d.Seconds)
	}
}

// ObserveWSUsageRecord 的可信等级说明完整性；此前权威增量不因中断被撤销。
func (m *Metrics) ObserveWSUsageRecord(protocol, source string, unit WSUsageUnit, fidelity canonical.Fidelity) {
	if !wsProtocol(protocol) || !wsSource(source) {
		return
	}
	switch fidelity {
	case canonical.FidelityAuthoritative, canonical.FidelityEstimated, canonical.FidelityUnavailable:
	default:
		return
	}
	switch unit {
	case UnitUnknown:
		if fidelity != canonical.FidelityUnavailable {
			return
		}
	case UnitTokens, UnitCharacters, UnitSeconds:
	default:
		return
	}
	m.WSUsageRecords.WithLabelValues(protocol, source, string(unit), string(fidelity)).Inc()
}
