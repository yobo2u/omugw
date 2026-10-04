package smoke_test

import (
	"errors"
	"math/big"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const inferenceManifestLimit = 64 << 10

// 分子/分母只表达精确有理数；不接受float舍入、隐式单位换算或verified布尔授权。
type inferenceRational struct{ Numerator, Denominator int64 }
type inferenceCostSource struct{ URL, Excerpt, SHA256 string }
type inferenceCostComponent struct {
	Direction                            string
	MaxPerTask, PriceFenPerUnit, Quantum inferenceRational
	MaxBasis                             string
	PriceSource, MaximumSource           inferenceCostSource
}
type inferenceCostEvidence struct {
	Slot                                   int
	Region, Model, Voice, SampleSHA256     string
	MaxTasks                               int64
	PriceSource, CheckedAt, Unit, Rounding string
	RoundingSource                         inferenceCostSource
	OutputUnbilled                         *inferenceCostSource `json:",omitempty"`
	Components                             []inferenceCostComponent
}
type inferenceCostAuthorization struct {
	CalculatedFen int64
	Evidence      inferenceCostEvidence
}

func inferenceOfficialSource(s inferenceCostSource) bool {
	u, err := url.Parse(s.URL)
	return err == nil && len(s.URL) <= 1024 && u.Scheme == "https" && u.Host == "help.aliyun.com" && u.User == nil && u.RawPath == "" && u.RawQuery == "" && !u.ForceQuery && !strings.Contains(s.URL, "#") && strings.HasPrefix(u.Path, "/zh/model-studio/") && len(strings.TrimSpace(s.Excerpt)) > 0 && len(s.Excerpt) <= 2048 && utf8.ValidString(s.Excerpt) && s.SHA256 == inferenceSHA([]byte(s.Excerpt))
}

func inferencePositiveRational(q inferenceRational) (*big.Rat, error) {
	if q.Numerator <= 0 || q.Denominator <= 0 {
		return nil, errors.New("费用有理数必须分子分母均为正")
	}
	return new(big.Rat).SetFrac(big.NewInt(q.Numerator), big.NewInt(q.Denominator)), nil
}

// 输入已为正有理数；中间值使用big.Int，避免加一/乘task/累加两路token时溢出。
func inferenceCeil(q *big.Rat) *big.Int {
	n, rem := new(big.Int), new(big.Int)
	n.QuoRem(q.Num(), q.Denom(), rem)
	if rem.Sign() != 0 {
		n.Add(n, big.NewInt(1))
	}
	return n
}

// 只验证提交载体的身份、完整性与算术；摘录是否忠实/适用由控制器对照官方材料核验。
// 缺少本槽证据拒绝，其他槽尚未知不阻断本槽；synthetic从不旁路这个裁决。
func inferenceAuthorizeCost(m inferenceRecordingManifest, slot int, now time.Time) (inferenceCostAuthorization, error) {
	var a inferenceCostAuthorization
	bad := errors.New("本槽费用依据缺失、矛盾或不足")
	if slot < 1 || slot > 6 || len(m.CostEvidence) > 6 {
		return a, bad
	}
	seen := [6]bool{}
	found := false
	for _, e := range m.CostEvidence {
		if e.Slot < 1 || e.Slot > 6 || seen[e.Slot-1] {
			return a, bad
		}
		seen[e.Slot-1] = true
		if e.Slot == slot {
			a.Evidence = e
			found = true
		}
	}
	if !found {
		return a, bad
	}
	e := a.Evidence
	s := m.Slots[slot-1]
	models := [6]string{"qwen-audio-3.0-asr-flash-streaming", "qwen-audio-3.0-tts-flash", "sambert-zhichu-v1", "qwen-audio-3.1-asr-flash-message", "qwen-audio-3.1-asr-flash-streaming", "qwen-audio-3.0-tts-flash"}
	if e.Model != models[slot-1] || e.PriceSource != "https://help.aliyun.com/zh/model-studio/model-pricing" {
		return a, bad
	}
	if s.Slot != slot || s.MaxTasks < 1 || s.MaxTasks > 2 || s.WorstCaseFen < 1 || s.WorstCaseFen > 100 || e.Region != m.Region || e.Region != "cn-beijing" || e.Model != s.Model || e.Voice != s.Voice || e.SampleSHA256 != m.SampleSHA256 || e.MaxTasks != s.MaxTasks || e.PriceSource != m.PriceSource {
		return a, bad
	}
	checked, err := time.Parse(time.RFC3339, e.CheckedAt)
	if err != nil || checked.After(now) || now.Sub(checked) > 24*time.Hour {
		return a, bad
	}
	unit := [6]string{"seconds", "characters", "characters", "tokens", "tokens", "characters"}[slot-1]
	if e.Unit != unit || !inferenceOfficialSource(e.RoundingSource) {
		return a, bad
	}
	if e.Rounding != "ceil_quantity_per_task_then_slot_fen" && e.Rounding != "ceil_quantity_per_task_then_task_fen" {
		return a, bad
	}
	if unit == "tokens" {
		if len(e.Components) != 2 || e.OutputUnbilled != nil || s.WorstCaseTokens <= 0 {
			return a, bad
		}
	} else if len(e.Components) != 1 || e.OutputUnbilled == nil || !inferenceOfficialSource(*e.OutputUnbilled) || e.OutputUnbilled.URL != e.PriceSource {
		return a, bad
	}
	perTaskFen, quantities := new(big.Rat), new(big.Rat)
	directions := map[string]bool{}
	for _, c := range e.Components {
		if directions[c.Direction] || c.Direction != "input" && c.Direction != "output" || unit != "tokens" && c.Direction != "input" {
			return a, bad
		}
		directions[c.Direction] = true
		if !inferenceOfficialSource(c.PriceSource) || c.PriceSource.URL != e.PriceSource || !inferenceOfficialSource(c.MaximumSource) {
			return a, bad
		}
		maximum, err := inferencePositiveRational(c.MaxPerTask)
		if err != nil {
			return a, bad
		}
		rate, err := inferencePositiveRational(c.PriceFenPerUnit)
		if err != nil {
			return a, bad
		}
		quantum, err := inferencePositiveRational(c.Quantum)
		if err != nil {
			return a, bad
		}
		if unit == "tokens" {
			if c.MaxBasis != "server_limit" || !maximum.IsInt() || !quantum.IsInt() {
				return a, bad
			}
		} else {
			if c.MaxBasis != "bounded_input" {
				return a, bad
			}
			limit := s.MaxInputAudioSeconds
			if unit == "characters" {
				limit = s.MaxInputCharacters
				if !maximum.IsInt() || !quantum.IsInt() {
					return a, bad
				}
			}
			// 按manifest整个允许输入上限计算，不以当前短样本偷偷降低已经声明的最坏量。
			if maximum.Cmp(new(big.Rat).SetFrac(big.NewInt(limit), big.NewInt(s.MaxTasks))) != 0 {
				return a, bad
			}
		}
		quantities.Add(quantities, maximum)
		rounded := new(big.Rat).Mul(new(big.Rat).SetInt(inferenceCeil(new(big.Rat).Quo(maximum, quantum))), quantum)
		perTaskFen.Add(perTaskFen, new(big.Rat).Mul(rounded, rate))
	}
	if !directions["input"] || unit == "tokens" && !directions["output"] {
		return a, bad
	}
	if unit == "tokens" {
		total := new(big.Rat).Mul(quantities, new(big.Rat).SetInt64(s.MaxTasks))
		if !total.IsInt() || !total.Num().IsInt64() || total.Num().Int64() > s.WorstCaseTokens {
			return a, bad
		}
	}
	var fen *big.Int
	if e.Rounding == "ceil_quantity_per_task_then_task_fen" {
		fen = new(big.Int).Mul(inferenceCeil(perTaskFen), big.NewInt(s.MaxTasks))
	} else {
		fen = inferenceCeil(new(big.Rat).Mul(perTaskFen, new(big.Rat).SetInt64(s.MaxTasks)))
	}
	if !fen.IsInt64() || fen.Sign() <= 0 || fen.Cmp(big.NewInt(s.WorstCaseFen)) > 0 || fen.Cmp(big.NewInt(m.WorstCaseFen)) > 0 {
		return a, bad
	}
	a.CalculatedFen = fen.Int64()
	return a, nil
}
