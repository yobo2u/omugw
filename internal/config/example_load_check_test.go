package config

import (
	"os"
	"testing"
)

// TestExampleConfigLoads 固化示例配置可直接启动。
//
// 示例文件是新用户的第一份配置。它一旦加载失败，人对着一个「照文档抄却起不来」
// 的网关是查不出原因的——尤其 ${...} 未定义变量是硬失败，注释掉的段落若被误
// 取消注释就会撞上这条。
func TestExampleConfigLoads(t *testing.T) {
	t.Setenv("OMUGW_API_KEY", "omugw-example-key-0123456789")
	t.Setenv("OPENAI_API_KEY", "sk-example")

	if _, err := os.Stat("../../config.example.yaml"); err != nil {
		t.Skipf("示例配置不存在: %v", err)
	}
	cfg, err := Load("../../config.example.yaml")
	if err != nil {
		t.Fatalf("示例配置应当可以直接加载: %v", err)
	}
	if len(cfg.Models) == 0 {
		t.Error("示例应当带一条可用的模型路由")
	}
	if cfg.Discovery.Enabled {
		t.Error("示例里的发现必须默认关闭")
	}
}
