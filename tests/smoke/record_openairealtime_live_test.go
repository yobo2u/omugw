//go:build smoke

package smoke_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/yobo2u/omugw/internal/testkit"
)

// 普通 smoke 开关不启用收费录制；一次执行只允许一个场景、一个会话，无重试。
func TestRecordOpenAIRealtime(t *testing.T) {
	if os.Getenv("OMUGW_RECORD_OPENAI_REALTIME") != "1" {
		t.Skip("独立 OpenAI 录制默认关闭")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal("无法定位仓库")
	}
	cfg, _, err := oaRecordConfig(root, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	if err := oaReserve(cfg.Root, cfg.Output); err != nil {
		t.Fatal(err)
	}
	r := oaCapture(context.Background(), cfg)
	if err := oaSave(cfg.Output, r); err != nil {
		t.Fatal("录制或候选验证失败；检查私有原始轨迹的固定分类")
	}
	if _, err := testkit.ReadWSFixture(filepath.Join(cfg.Output, "candidate.json"), testkit.DefaultWSLimits()); err != nil {
		t.Fatal("候选有界加载失败")
	}
	t.Logf("候选待人工审核；场景=%s，原始记录=%d，每次一会话，无重试", cfg.Scenario, len(r.Records))
}
