package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadAppliesDefaults(t *testing.T) {
	cfg, err := Load(write(t, "server:\n  addr: \":9999\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Addr != ":9999" {
		t.Errorf("addr = %q", cfg.Server.Addr)
	}
	if cfg.Timeouts.FirstByte != Default().Timeouts.FirstByte {
		t.Error("未指定的字段应使用默认值")
	}
	if cfg.Limits.MaxInlineBytes != 20<<20 {
		t.Errorf("max_inline_bytes 默认值 = %d", cfg.Limits.MaxInlineBytes)
	}
}

func TestLoadExpandsEnv(t *testing.T) {
	t.Setenv("OMUGW_TEST_ADDR", ":7777")

	cfg, err := Load(write(t, "server:\n  addr: \"${OMUGW_TEST_ADDR}\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Addr != ":7777" {
		t.Errorf("环境变量未展开: %q", cfg.Server.Addr)
	}
}

// TestLoadIgnoresEnvSyntaxInComments 是一条回归测试。
//
// 早先的实现在**原始文件文本**上做 os.Expand，于是注释里用来说明用法的
// ${ENV_VAR} 也被当成了变量引用——一份带说明注释的示例配置直接启动失败。
// 展开必须发生在 YAML 解析之后，且只作用于字符串标量。
func TestLoadIgnoresEnvSyntaxInComments(t *testing.T) {
	body := "# 字符串值支持 ${ENV_VAR} 展开，未定义会启动失败\n" +
		"server:\n" +
		"  addr: \":8080\" # 也可以写成 ${SOME_VAR}\n"

	cfg, err := Load(write(t, body))
	if err != nil {
		t.Fatalf("注释中的 ${...} 不应被当成变量引用: %v", err)
	}
	if cfg.Server.Addr != ":8080" {
		t.Errorf("addr = %q", cfg.Server.Addr)
	}
}

// TestLoadDoesNotExpandNonStringScalars 保证展开不会碰到数字等标量。
func TestLoadDoesNotExpandNonStringScalars(t *testing.T) {
	cfg, err := Load(write(t, "limits:\n  max_inline_bytes: 1048576\n  max_request_bytes: 2097152\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Limits.MaxInlineBytes != 1048576 {
		t.Errorf("max_inline_bytes = %d", cfg.Limits.MaxInlineBytes)
	}
}

// TestLoadEmptyFileUsesDefaults 覆盖空文件解析出零值节点的边界。
func TestLoadEmptyFileUsesDefaults(t *testing.T) {
	cfg, err := Load(write(t, ""))
	if err != nil {
		t.Fatalf("空配置文件应当采用全部默认值: %v", err)
	}
	if cfg.Server.Addr != Default().Server.Addr {
		t.Errorf("addr = %q, 期望默认值", cfg.Server.Addr)
	}
}

// TestLoadRejectsUndefinedEnv 固化「启动时失败好过运行时 401」。
// 展开成空串会让一个缺失的 API Key 变成难以定位的鉴权错误。
func TestLoadRejectsUndefinedEnv(t *testing.T) {
	_, err := Load(write(t, "server:\n  addr: \"${OMUGW_DEFINITELY_NOT_SET}\"\n"))
	if err == nil {
		t.Fatal("引用未定义的环境变量应当启动失败")
	}
	if !strings.Contains(err.Error(), "OMUGW_DEFINITELY_NOT_SET") {
		t.Errorf("错误信息应指出是哪个变量，实际: %v", err)
	}
}

// TestTimeoutsValidate 固化原则 2.7 的四层超时约束。
func TestTimeoutsValidate(t *testing.T) {
	base := Default().Timeouts

	tests := []struct {
		name    string
		mutate  func(*Timeouts)
		wantErr string
	}{
		{
			name:    "connect 不小于 first_byte 时首字节超时永不触发",
			mutate:  func(tm *Timeouts) { tm.Connect = tm.FirstByte },
			wantErr: "first_byte",
		},
		{
			name:    "first_byte 大于 total 会让流式 failover 判定窗口失效",
			mutate:  func(tm *Timeouts) { tm.FirstByte = tm.Total + time.Second },
			wantErr: "total",
		},
		{
			name:    "idle 大于 total",
			mutate:  func(tm *Timeouts) { tm.Idle = tm.Total + time.Second },
			wantErr: "idle",
		},
		{
			name:    "零值超时",
			mutate:  func(tm *Timeouts) { tm.Idle = 0 },
			wantErr: "idle",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tm := base
			tc.mutate(&tm)
			err := tm.Validate()
			if err == nil {
				t.Fatal("应当校验失败")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("错误信息应提到 %q，实际: %v", tc.wantErr, err)
			}
		})
	}

	if err := base.Validate(); err != nil {
		t.Errorf("默认超时配置应当合法: %v", err)
	}
}

// TestLimitsMustBeConsistent 防止一个内联负载永远无法通过的配置组合。
func TestLimitsMustBeConsistent(t *testing.T) {
	cfg := Default()
	cfg.Limits.MaxRequestBytes = 1 << 20
	cfg.Limits.MaxInlineBytes = 8 << 20

	err := cfg.Validate()
	if err == nil {
		t.Fatal("max_request_bytes 小于 max_inline_bytes 应当校验失败")
	}
	if !strings.Contains(err.Error(), "永远无法通过") {
		t.Errorf("错误信息应说明后果，实际: %v", err)
	}
}

func TestValidateRejectsUnknownLogLevel(t *testing.T) {
	cfg := Default()
	cfg.Log.Level = "verbose"
	if err := cfg.Validate(); err == nil {
		t.Fatal("未知的日志级别应当校验失败")
	}
}
