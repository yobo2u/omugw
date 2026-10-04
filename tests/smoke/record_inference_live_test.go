//go:build smoke

package smoke_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/yobo2u/omugw/internal/transport/ws"
)

func inferenceCapture(ctx context.Context, c inferenceRecordingConfig) (inferenceRecording, error) {
	if c.synthetic {
		return inferenceRecording{}, errors.New("合成配置不能访问live")
	}
	if err := inferenceValidate(c); err != nil {
		return inferenceRecording{}, err
	}
	return inferenceCaptureWithDial(ctx, c, ws.Dial)
}

func TestRecordDashScopeInference(t *testing.T) {
	if os.Getenv("OMUGW_RECORD_DS_INFERENCE") != "1" {
		t.Skip("需要独立S3录制开关")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal("仓库根定位失败")
	}
	c, err := inferenceRecordConfig(root, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	if err := inferenceReserve(c); err != nil {
		t.Fatal(err)
	}
	r, captureErr := inferenceCapture(context.Background(), c)
	if err := inferenceSave(c.Output, r); err != nil {
		t.Fatal(err)
	}
	if captureErr != nil {
		t.Fatal(captureErr)
	}
	t.Log("单槽完成，私有原始材料已保存；候选、计费与能力证据均须控制器独立审核")
}
