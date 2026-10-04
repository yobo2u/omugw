package smoke_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/transport/ws"
)

// 摘录全为合成材料；将完整六槽的编码长度推到公开上限，防缩进/HTML转义吞掉保存额度。
func inferenceFullMetadataConfig(t *testing.T, seed string) inferenceRecordingConfig {
	t.Helper()
	c := inferenceOfflineConfig(t, 3)
	samplePath := filepath.Join(c.Root, "fixed-<>&.pcm")
	if err := os.Rename(c.SamplePath, samplePath); err != nil {
		t.Fatal(err)
	}
	c.SamplePath, c.Manifest.SamplePath = samplePath, samplePath
	var sources []*inferenceCostSource
	for i := range c.Manifest.CostEvidence {
		e := &c.Manifest.CostEvidence[i]
		sources = append(sources, &e.RoundingSource)
		if e.OutputUnbilled != nil {
			sources = append(sources, e.OutputUnbilled)
		}
		for j := range e.Components {
			sources = append(sources, &e.Components[j].PriceSource, &e.Components[j].MaximumSource)
		}
	}
	set := func(s *inferenceCostSource, text string) {
		s.Excerpt = text
		s.SHA256 = inferenceSHA([]byte(text))
	}
	const prefix = "SYNTHETIC TEST ONLY: "
	for _, s := range sources {
		set(s, prefix)
	}
	b, _ := json.Marshal(c.Manifest)
	encodedSeed, _ := json.Marshal(seed)
	n := ((64 << 10) - len(b)) / (len(sources) * (len(encodedSeed) - 2))
	for _, s := range sources {
		set(s, prefix+strings.Repeat(seed, min(n, (2048-len(prefix))/len(seed))))
	}
	b, _ = json.Marshal(c.Manifest)
	remaining := (64 << 10) - len(b)
	for _, s := range sources {
		add := min(remaining, 2048-len(s.Excerpt))
		set(s, s.Excerpt+strings.Repeat("x", add))
		remaining -= add
	}
	// ASCII摘录全满时，用有界官方最大量来源路径补齐空隙。
	for i := range c.Manifest.CostEvidence {
		for j := range c.Manifest.CostEvidence[i].Components {
			s := &c.Manifest.CostEvidence[i].Components[j].MaximumSource
			add := min(remaining, 1024-len(s.URL))
			s.URL += strings.Repeat("x", add)
			remaining -= add
		}
	}
	for _, s := range sources {
		if len(s.Excerpt) > 2048 {
			t.Fatal("测试摘录超限")
		}
	}
	b, _ = json.Marshal(c.Manifest)
	if len(b) != 64<<10 || len(c.Manifest.CostEvidence) != 6 {
		t.Fatal("未构造到六槽完整编码边界")
	}
	// 走真实配置解析/费用准入；Dial仍只能由下方离线注入函数提供。
	got, err := inferenceRecordConfig(c.Root, inferenceCostEnv(t, c))
	if err != nil || got.synthetic {
		t.Fatalf("完整manifest准入失败: %v", err)
	}
	return got
}

func TestInferenceRecorderOfflineMetadata(t *testing.T) {
	for _, seed := range []string{"x", "<>&"} {
		for _, ending := range []string{"handshake_failed", "completed"} {
			t.Run(seed+"/"+ending, func(t *testing.T) {
				c := inferenceFullMetadataConfig(t, seed)
				var r inferenceRecording
				if ending == "completed" {
					r = inferenceLocalCapture(t, c, "peer")
				} else {
					if err := inferenceReserve(c); err != nil {
						t.Fatal(err)
					}
					calls := 0
					var err error
					r, err = inferenceCaptureWithDial(context.Background(), c, func(context.Context, string, ws.DialOptions) (*ws.Conn, *http.Response, error) {
						calls++
						return nil, nil, errors.New("offline injected failure; no socket")
					})
					if err == nil || calls != 1 || r.Failure != "capture_failed" {
						t.Fatalf("未记录失败尝试: calls=%d err=%v", calls, err)
					}
				}
				if r.Outcome != ending {
					t.Fatalf("outcome=%s want=%s", r.Outcome, ending)
				}
				if _, err := os.Stat(filepath.Join(filepath.Dir(c.Output), "dial-3")); err != nil {
					t.Fatal(err)
				}
				raw, err := os.ReadFile(filepath.Join(c.Output, "records.jsonl"))
				if err != nil {
					t.Fatal(err)
				}
				if err := inferenceSave(c.Output, r); err != nil {
					t.Fatalf("合法64KiB完整证据在消耗槽后无法保存: %v", err)
				}
				b, err := os.ReadFile(filepath.Join(c.Output, "recording.json"))
				if err != nil || len(b) > 65<<10 {
					t.Fatalf("摘要未有界保存: bytes=%d err=%v", len(b), err)
				}
				var summary inferenceRecording
				if err := json.Unmarshal(b, &summary); err != nil {
					t.Fatal(err)
				}
				want := c.Manifest
				want.SamplePath, want.Endpoint = "", ""
				if !reflect.DeepEqual(summary.Manifest, want) || summary.RawSHA256 != inferenceSHA(raw) || summary.Outcome != ending || summary.Failure != r.Failure || !summary.Started.Equal(r.Started) || !summary.Ended.Equal(r.Ended) || summary.TransportEnd != r.TransportEnd || summary.PeerCloseAt != r.PeerCloseAt {
					t.Fatal("费用摘录/摘要事实或raw摘要丢失")
				}
				after, err := os.ReadFile(filepath.Join(c.Output, "records.jsonl"))
				if err != nil || !bytes.Equal(raw, after) {
					t.Fatal("保存改写了raw")
				}
				name := "candidate.json"
				if ending == "handshake_failed" {
					name = "candidate-unavailable.txt"
				}
				if _, err := os.Stat(filepath.Join(c.Output, name)); err != nil {
					t.Fatal(err)
				}
				t.Logf("manifest=65536 summary=%d raw=%d outcome=%s", len(b), len(raw), ending)
			})
		}
	}
}

func TestInferenceRecorderOfflineMetadataBounds(t *testing.T) {
	t.Run("最长固定字段包络仍可保存", func(t *testing.T) {
		c := inferenceFullMetadataConfig(t, "<>&")
		if err := inferenceReserve(c); err != nil {
			t.Fatal(err)
		}
		text := strings.Repeat("x", 64)
		stamp := time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.FixedZone("max-offset", 23*3600+59*60))
		// 用比实际枚举更宽的包络覆盖身份/Outcome、时间、整数和SHA固定开销；不冒充捕获证据。
		r := inferenceRecording{Manifest: c.Manifest, Started: stamp, Ended: stamp, Outcome: text, Scenario: text, Model: text, Voice: text, SampleSHA256: text, TransportEnd: text, PassiveEnd: text, Failure: text, Slot: math.MinInt, Status: math.MinInt, FailedAt: math.MinInt64, PeerCloseAt: math.MinInt64}
		if err := inferenceSave(c.Output, r); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(c.Output, "recording.json"))
		if err != nil || len(b) > 65<<10 {
			t.Fatalf("固定包络不能有界保存: %d %v", len(b), err)
		}
		var saved inferenceRecording
		if err := json.Unmarshal(b, &saved); err != nil || saved.Outcome != text || !saved.Started.Equal(stamp) || saved.FailedAt != math.MinInt64 || len(saved.RawSHA256) != 64 {
			t.Fatal("包络字段被截断", err)
		}
		m, _ := json.Marshal(saved.Manifest)
		if overhead := len(b) - len(m); overhead > 984 {
			t.Fatalf("新增字段突破固定开销证明: %d", overhead)
		}
		t.Logf("summary=%d manifest=%d overhead=%d", len(b), len(m), len(b)-len(m))
	})
	t.Run("HTML膨胀越界在占槽和Dial前拒绝", func(t *testing.T) {
		c := inferenceFullMetadataConfig(t, "<>&")
		// 原文件仍远小于64KiB，但同一编码口径的manifest比上限大6B。
		s := &c.Manifest.CostEvidence[0].Components[0].MaximumSource
		s.Excerpt += "<"
		s.SHA256 = inferenceSHA([]byte(s.Excerpt))
		var raw bytes.Buffer
		enc := json.NewEncoder(&raw)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(c.Manifest); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(c.Manifest)
		if raw.Len() >= 64<<10 || len(b) != (64<<10)+6 {
			t.Fatal("未构造到文件小而编码超限的边界")
		}
		get := inferenceCostEnv(t, c)
		if err := os.WriteFile(c.ManifestPath, raw.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := inferenceRecordConfig(c.Root, func(k string) string {
			if k == "DASHSCOPE_API_KEY" {
				t.Fatal("超编码预算仍读取凭据")
			}
			return get(k)
		}); err == nil || !strings.Contains(err.Error(), "64KiB") {
			t.Fatal("配置未拒绝编码超限", err)
		}
		for _, run := range []func(inferenceRecordingConfig) error{inferenceReserve, inferenceClaimDial} {
			if err := run(c); err == nil || !strings.Contains(err.Error(), "64KiB") {
				t.Fatal("直接入口未拒绝编码超限", err)
			}
		}
		if _, err := inferenceCaptureWithDial(context.Background(), c, func(context.Context, string, ws.DialOptions) (*ws.Conn, *http.Response, error) {
			t.Fatal("超编码预算仍Dial")
			return nil, nil, nil
		}); err == nil {
			t.Fatal("capture未拒绝超限")
		}
		if _, err := os.Stat(filepath.Dir(c.Output)); !os.IsNotExist(err) {
			t.Fatal("超限预检已创建reserve/dial目录")
		}
		t.Logf("input=%d compact=%d zero reserve/Dial", raw.Len(), len(b))
	})
}
