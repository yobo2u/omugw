//go:build smoke

package smoke_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/yobo2u/omugw/internal/testkit"
)

// 单次显式场景只开一个直连会话。一般 smoke 开关不能启用这个收费入口。
func TestRecordDSRealtime(t *testing.T) {
	if os.Getenv("OMUGW_RECORD_DSREALTIME") != "1" {
		t.Skip("未显式开启独立 Realtime 录制")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal("无法定位仓库")
	}
	cfg, _, err := dsRecordConfig(root, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	if err := dsReserveOutput(cfg.Root, cfg.Output); err != nil {
		t.Fatal(err)
	}
	r := dsCapture(context.Background(), cfg)
	if r.Input != nil {
		if err := dsWriteExclusive(cfg.Output, "input.pcm", r.Input.Data); err != nil {
			t.Fatal(err)
		}
	}
	// 失败也留下真实记录；从不保存原始握手头、握手错误 body 或底层错误文本。
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatal("轨迹编码失败")
	}
	if err := dsWriteExclusive(cfg.Output, "recording.json", b); err != nil {
		t.Fatal(err)
	}
	if len(r.Image) > 0 {
		if err := dsWriteExclusive(cfg.Output, "image.jpeg", r.Image); err != nil {
			t.Fatal(err)
		}
	}
	if r.Failure != "" {
		t.Fatalf("录制未完成，分类=%s；已保存私有原始轨迹", r.Failure)
	}
	if cfg.Scenario == "tts-commit" || cfg.Scenario == "tts-server-commit" {
		if err := dsSaveAudio(cfg.Output, r); err != nil {
			t.Fatal(err)
		}
	}
	f, err := dsCandidate(r)
	if err != nil {
		t.Fatal(err)
	}
	b, err = json.MarshalIndent(f, "", "  ")
	if err != nil {
		t.Fatal("候选编码失败")
	}
	if err := dsWriteExclusive(cfg.Output, "candidate.json", b); err != nil {
		t.Fatal(err)
	}
	if _, err := testkit.ReadWSFixture(filepath.Join(cfg.Output, "candidate.json"), testkit.DefaultWSLimits()); err != nil {
		t.Fatal("候选文件未通过有界加载验证")
	}
	t.Logf("候选待审核：scenario=%s messages=%d outcome=%s；每次一会话，无重试", cfg.Scenario, len(r.Records), f.Response.WS.Outcome.Kind)
}
