package smoke_test

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 所有单价、上限、摘录和取整均为SYNTHETIC；只验证载体与精确算术，不是云端计费事实。
func inferenceSyntheticCost(m inferenceRecordingManifest, slot int) inferenceCostEvidence {
	s := m.Slots[slot-1]
	source := func(label string) inferenceCostSource {
		q := "SYNTHETIC TEST ONLY: " + label
		return inferenceCostSource{URL: m.PriceSource, Excerpt: q, SHA256: inferenceSHA([]byte(q))}
	}
	e := inferenceCostEvidence{Slot: slot, Region: m.Region, Model: s.Model, Voice: s.Voice, SampleSHA256: m.SampleSHA256, MaxTasks: s.MaxTasks, PriceSource: m.PriceSource, CheckedAt: m.PriceCheckedAt, Unit: "characters", Rounding: "ceil_quantity_per_task_then_slot_fen", RoundingSource: source("rounding"), OutputUnbilled: func() *inferenceCostSource { x := source("output unbilled"); return &x }()}
	max := s.MaxInputCharacters / s.MaxTasks
	basis := "bounded_input"
	if slot == 1 {
		e.Unit = "seconds"
		max = s.MaxInputAudioSeconds / s.MaxTasks
	}
	if slot == 4 || slot == 5 {
		e.Unit = "tokens"
		max = s.WorstCaseTokens / s.MaxTasks / 2
		basis = "server_limit"
		e.OutputUnbilled = nil
	}
	e.Components = []inferenceCostComponent{{Direction: "input", MaxPerTask: inferenceRational{max, 1}, PriceFenPerUnit: inferenceRational{1, 1_000_000}, Quantum: inferenceRational{1, 1}, MaxBasis: basis, PriceSource: source("price"), MaximumSource: source("maximum")}}
	if e.Unit == "tokens" {
		out := e.Components[0]
		out.Direction = "output"
		e.Components = append(e.Components, out)
	}
	return e
}

func inferenceCostEnv(t *testing.T, c inferenceRecordingConfig) func(string) string {
	t.Helper()
	b, err := json.Marshal(c.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.ManifestPath, b, 0600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"OMUGW_RECORD_DS_INFERENCE": "1", "OMUGW_DS_INFERENCE_SLOT": string(rune('0' + c.Slot)), "OMUGW_DS_INFERENCE_OUTPUT": c.Output, "OMUGW_DS_INFERENCE_MANIFEST": c.ManifestPath, "DASHSCOPE_API_KEY": "offline-secret"}
	return func(k string) string { return env[k] }
}

func TestInferenceRecorderOfflineCost(t *testing.T) {
	t.Run("一分的有理数边界不能舍成一分", func(t *testing.T) {
		c := inferenceOfflineConfig(t, 3)
		e := inferenceSyntheticCost(c.Manifest, 3)
		e.Components[0].PriceFenPerUnit = inferenceRational{9_007_199_254_740_993, 360_287_970_189_639_680}
		c.Manifest.CostEvidence = []inferenceCostEvidence{e}
		// 40字符乘单价严格大于1分，float64会把2^53+1舍回2^53。
		if _, err := inferenceAuthorizeCost(c.Manifest, 3, time.Now()); err == nil {
			t.Fatal("用float舍掉了一分以上的差额")
		}
		c.Manifest.Slots[2].WorstCaseFen = 2
		c.Manifest.WorstCaseFen = 7
		a, err := inferenceAuthorizeCost(c.Manifest, 3, time.Now())
		if err != nil || a.CalculatedFen != 2 {
			t.Fatalf("fen=%d err=%v", a.CalculatedFen, err)
		}
	})
	t.Run("非token本槽可达其他未知槽不阻断", func(t *testing.T) {
		for _, slot := range []int{1, 2, 3, 6} {
			c := inferenceOfflineConfig(t, slot)
			c.synthetic = false
			c.Manifest.Slots[3].WorstCaseTokens = 0
			c.Manifest.Slots[4].WorstCaseTokens = 0
			c.Manifest.CostEvidence = []inferenceCostEvidence{inferenceSyntheticCost(c.Manifest, slot)}
			got, err := inferenceRecordConfig(c.Root, inferenceCostEnv(t, c))
			if err != nil {
				t.Fatal(err)
			}
			if got.synthetic {
				t.Fatal("真实配置路径被偷换成synthetic旁路")
			}
			if err := inferenceReserve(got); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(c.Output), "dial-"+string(rune('0'+slot)))); !os.IsNotExist(err) {
				t.Fatal("预检触发Dial")
			}
		}
	})
	t.Run("精确有理数及分槽取整", func(t *testing.T) {
		for _, tc := range []struct {
			rule string
			want int64
		}{{"ceil_quantity_per_task_then_slot_fen", 3}, {"ceil_quantity_per_task_then_task_fen", 4}} {
			c := inferenceOfflineConfig(t, 2)
			c.Manifest.Slots[1].WorstCaseFen = 4
			c.Manifest.WorstCaseFen = 9
			e := inferenceSyntheticCost(c.Manifest, 2)
			e.Rounding = tc.rule
			e.Components[0].Quantum = inferenceRational{3, 1}
			e.Components[0].PriceFenPerUnit = inferenceRational{1, 30}
			c.Manifest.CostEvidence = []inferenceCostEvidence{e}
			// ceil(40/3)*3/30=7/5分/task；两task槽取整3分，逐task取整4分。
			a, err := inferenceAuthorizeCost(c.Manifest, 2, time.Now())
			if err != nil || a.CalculatedFen != tc.want {
				t.Fatalf("fen=%d want=%d err=%v", a.CalculatedFen, tc.want, err)
			}
		}
	})
	t.Run("token完整结构正例仍只是合成", func(t *testing.T) {
		c := inferenceOfflineConfig(t, 4)
		c.synthetic = false
		c.Manifest.CostEvidence = []inferenceCostEvidence{inferenceSyntheticCost(c.Manifest, 4)}
		if _, err := inferenceRecordConfig(c.Root, inferenceCostEnv(t, c)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("冻结批次参考日不代替本槽新核对日", func(t *testing.T) {
		c := inferenceOfflineConfig(t, 1)
		c.synthetic = false
		c.Manifest.CostEvidence = []inferenceCostEvidence{inferenceSyntheticCost(c.Manifest, 1)}
		c.Manifest.PriceCheckedAt = time.Now().Add(-48 * time.Hour).Format(time.RFC3339)
		if _, err := inferenceRecordConfig(c.Root, inferenceCostEnv(t, c)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("缺失矛盾及溢出零reserve", func(t *testing.T) {
		cases := []struct {
			name   string
			change func(*inferenceRecordingConfig)
		}{
			{"缺token证据", func(c *inferenceRecordingConfig) { c.Manifest.CostEvidence = nil }},
			{"token零未知", func(c *inferenceRecordingConfig) { c.Manifest.Slots[3].WorstCaseTokens = 0 }},
			{"模型错绑", func(c *inferenceRecordingConfig) { c.Manifest.CostEvidence[0].Model = "other" }},
			{"地域错绑", func(c *inferenceRecordingConfig) { c.Manifest.CostEvidence[0].Region = "ap-southeast-1" }},
			{"槽错绑", func(c *inferenceRecordingConfig) { c.Manifest.CostEvidence[0].Slot = 5 }},
			{"非官方来源", func(c *inferenceRecordingConfig) {
				c.Manifest.CostEvidence[0].Components[0].MaximumSource.URL = "https://example.com/claims"
			}},
			{"缺最大量依据", func(c *inferenceRecordingConfig) { c.Manifest.CostEvidence[0].Components[1].MaximumSource.Excerpt = "" }},
			{"不能用输入时长猜token", func(c *inferenceRecordingConfig) { c.Manifest.CostEvidence[0].Components[0].MaxBasis = "bounded_input" }},
			{"摘录摘要不符", func(c *inferenceRecordingConfig) {
				c.Manifest.CostEvidence[0].Components[0].PriceSource.SHA256 = strings.Repeat("0", 64)
			}},
			{"过期", func(c *inferenceRecordingConfig) {
				c.Manifest.CostEvidence[0].CheckedAt = time.Now().Add(-25 * time.Hour).Format(time.RFC3339)
			}},
			{"费用不足", func(c *inferenceRecordingConfig) {
				c.Manifest.CostEvidence[0].Components[0].PriceFenPerUnit = inferenceRational{1, 1}
			}},
			{"最大量矛盾", func(c *inferenceRecordingConfig) {
				c.Manifest.CostEvidence[0].Components[0].MaxPerTask = inferenceRational{9999, 1}
			}},
			{"缺输出价格", func(c *inferenceRecordingConfig) {
				c.Manifest.CostEvidence[0].Components = c.Manifest.CostEvidence[0].Components[:1]
			}},
			{"单位错", func(c *inferenceRecordingConfig) { c.Manifest.CostEvidence[0].Unit = "seconds" }},
			{"分母0", func(c *inferenceRecordingConfig) {
				c.Manifest.CostEvidence[0].Components[0].PriceFenPerUnit.Denominator = 0
			}},
			{"负单价", func(c *inferenceRecordingConfig) {
				c.Manifest.CostEvidence[0].Components[0].PriceFenPerUnit.Numerator = -1
			}},
			{"重复方向", func(c *inferenceRecordingConfig) { c.Manifest.CostEvidence[0].Components[1].Direction = "input" }},
			{"未知取整", func(c *inferenceRecordingConfig) { c.Manifest.CostEvidence[0].Rounding = "verified" }},
			{"数量和溢出", func(c *inferenceRecordingConfig) {
				c.Manifest.Slots[3].WorstCaseTokens = math.MaxInt64
				for i := range c.Manifest.CostEvidence[0].Components {
					c.Manifest.CostEvidence[0].Components[i].MaxPerTask = inferenceRational{math.MaxInt64, 1}
				}
			}},
			{"费用乘法溢出", func(c *inferenceRecordingConfig) {
				c.Manifest.CostEvidence[0].Components[0].PriceFenPerUnit = inferenceRational{math.MaxInt64, 1}
			}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				c := inferenceOfflineConfig(t, 4)
				c.synthetic = false
				c.Manifest.CostEvidence = []inferenceCostEvidence{inferenceSyntheticCost(c.Manifest, 4)}
				tc.change(&c)
				if err := inferenceReserve(c); err == nil {
					t.Fatal("无证据或矛盾仍占槽")
				}
				if _, err := os.Stat(filepath.Dir(c.Output)); !os.IsNotExist(err) {
					t.Fatal("费用预检前分配目录")
				}
			})
		}
	})
	t.Run("verified布尔不能代替证据", func(t *testing.T) {
		c := inferenceOfflineConfig(t, 4)
		c.Manifest.CostEvidence = nil
		b, _ := json.Marshal(c.Manifest)
		b = append(b[:len(b)-1], []byte(`,"CostEvidence":[{"Slot":4,"Verified":true}]}`)...)
		get := inferenceCostEnv(t, c)
		if err := os.WriteFile(c.ManifestPath, b, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := inferenceRecordConfig(c.Root, get); err == nil {
			t.Fatal("verified=true获得授权")
		}
	})
	t.Run("补充其他槽证据不改已冻结预算", func(t *testing.T) {
		c := inferenceOfflineConfig(t, 1)
		c.synthetic = false
		c.Manifest.CostEvidence = []inferenceCostEvidence{inferenceSyntheticCost(c.Manifest, 1)}
		if err := inferenceReserve(c); err != nil {
			t.Fatal(err)
		}
		d := c
		d.Slot = 3
		d.Scenario = inferenceScenarioName(3)
		d.Model = d.Manifest.Slots[2].Model
		d.Voice = ""
		d.Output = filepath.Join(filepath.Dir(c.Output), "slot-3")
		d.Manifest.CostEvidence = append(append([]inferenceCostEvidence(nil), c.Manifest.CostEvidence...), inferenceSyntheticCost(c.Manifest, 3))
		if err := inferenceReserve(d); err != nil {
			t.Fatal(err)
		}
		if err := inferenceClaimDial(c); err != nil {
			t.Fatal(err)
		}
		if err := inferenceClaimDial(c); err == nil {
			t.Fatal("一次认领可重用")
		}
		// 只换摘录并更新其hash也不能取代已经reserve的费用证据。
		d.Manifest.CostEvidence[1].RoundingSource.Excerpt += " changed"
		d.Manifest.CostEvidence[1].RoundingSource.SHA256 = inferenceSHA([]byte(d.Manifest.CostEvidence[1].RoundingSource.Excerpt))
		if err := inferenceClaimDial(d); err == nil {
			t.Fatal("费用证据在reserve后被偷换")
		}
	})
	t.Run("未知token槽后补证明无需重置批次", func(t *testing.T) {
		c := inferenceOfflineConfig(t, 1)
		c.synthetic = false
		c.Manifest.Slots[3].WorstCaseTokens = 0
		c.Manifest.CostEvidence = []inferenceCostEvidence{inferenceSyntheticCost(c.Manifest, 1)}
		if err := inferenceReserve(c); err != nil {
			t.Fatal(err)
		}
		d := c
		d.Slot = 4
		d.Scenario = inferenceScenarioName(4)
		d.Model = d.Manifest.Slots[3].Model
		d.Output = filepath.Join(filepath.Dir(c.Output), "slot-4")
		d.Manifest.Slots[3].WorstCaseTokens = 1000
		d.Manifest.CostEvidence = append(append([]inferenceCostEvidence(nil), c.Manifest.CostEvidence...), inferenceSyntheticCost(d.Manifest, 4))
		if err := inferenceReserve(d); err != nil {
			t.Fatal(err)
		}
		if err := inferenceClaimDial(c); err != nil {
			t.Fatal("其他槽补token证据破坏已占槽身份", err)
		}
		if err := inferenceClaimDial(d); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("费用证明不能提高既有持久分币预算", func(t *testing.T) {
		c := inferenceOfflineConfig(t, 1)
		c.Manifest.CostEvidence = []inferenceCostEvidence{inferenceSyntheticCost(c.Manifest, 1)}
		if err := inferenceReserve(c); err != nil {
			t.Fatal(err)
		}
		d := c
		d.Slot = 2
		d.Scenario = inferenceScenarioName(2)
		d.Model = d.Manifest.Slots[1].Model
		d.Voice = d.Manifest.Slots[1].Voice
		d.Output = filepath.Join(filepath.Dir(c.Output), "slot-2")
		d.Manifest.Slots[1].WorstCaseFen = 2
		d.Manifest.WorstCaseFen = 7
		d.Manifest.CostEvidence = []inferenceCostEvidence{inferenceSyntheticCost(d.Manifest, 2)}
		if err := inferenceReserve(d); err == nil || !strings.Contains(err.Error(), "冻结") {
			t.Fatalf("未核对既有预算: %v", err)
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(c.Output), "reserve-2.json")); !os.IsNotExist(err) {
			t.Fatal("预算错配仍消耗新槽")
		}
	})
}
