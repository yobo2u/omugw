//go:build smoke

package smoke_test

import (
	"bytes"
	"encoding/base64"
	"image/png"
	"strings"
	"testing"
)

// TestInlineVisionImageMeetsUpstreamMinimumDimensions 验证内联视觉测试样本的尺寸满足上游模型的最小尺寸约束。
//
// 上游 DashScope 模型对视觉输入的图像尺寸有硬性下限要求（宽高均需大于 10 像素）。
// 若样本尺寸过小（如 1×1 的占位图），上游会在真实请求时返回 400 错误：
// "The image length and width do not meet the model restrictions. [height:1 or width:1 must be larger than 10]"。
// 此测试作为纯离线回归防护，验证 tinyPNGDataURI 的真实图像尺寸。
func TestInlineVisionImageMeetsUpstreamMinimumDimensions(t *testing.T) {
	const prefix = "data:image/png;base64,"
	if !strings.HasPrefix(tinyPNGDataURI, prefix) {
		t.Fatalf("tinyPNGDataURI 缺少期望的 %q 前缀: %q", prefix, tinyPNGDataURI)
	}

	rawB64 := strings.TrimPrefix(tinyPNGDataURI, prefix)
	pngBytes, err := base64.StdEncoding.DecodeString(rawB64)
	if err != nil {
		t.Fatalf("base64 解码 tinyPNGDataURI 失败: %v", err)
	}

	cfg, err := png.DecodeConfig(bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatalf("PNG 解码 tinyPNGDataURI 失败: %v", err)
	}

	if cfg.Width <= 10 {
		t.Errorf("图片宽度必须严格大于 10 像素以满足上游约束，实际为 %d", cfg.Width)
	}
	if cfg.Height <= 10 {
		t.Errorf("图片高度必须严格大于 10 像素以满足上游约束，实际为 %d", cfg.Height)
	}
}
