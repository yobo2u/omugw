package testkit

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// Update 控制是否重写 golden 文件。用 `go test ./... -update` 触发。
//
// 注意：重写之后**必须人工审阅 diff**。golden 测试的价值全在于「这次输出
// 变了，是我有意改的吗」这一问，无脑 -update 会把它变成一个永远通过的空转。
var Update = flag.Bool("update", false, "重写 golden 文件（重写后须人工审阅 diff）")

// Golden 比对字节输出与 golden 文件。
func Golden(t *testing.T, path string, got []byte) {
	t.Helper()

	if *Update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("testkit: 创建 golden 目录失败: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("testkit: 写入 golden 文件失败: %v", err)
		}
		t.Logf("testkit: 已更新 %s（请审阅 diff）", path)
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("testkit: 读取 golden 文件 %s 失败（首次生成请加 -update）: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("与 golden 文件 %s 不一致 (-want +got):\n%s",
			path, cmp.Diff(string(want), string(got)))
	}
}

// GoldenJSON 比对结构化输出。
//
// 先规范化成缩进 JSON 再比对，这样 diff 是逐字段的而不是一整行——
// 一个 500 字段的请求体，逐行 diff 根本没法看。
func GoldenJSON(t *testing.T, path string, v any) {
	t.Helper()

	got, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("testkit: 序列化失败: %v", err)
	}
	Golden(t, path, append(got, '\n'))
}

// GoldenSSE 比对事件流。
func GoldenSSE(t *testing.T, path string, evs []SSEEvent) {
	t.Helper()
	Golden(t, path, []byte(EncodeSSE(evs)))
}

// AssertJSONEqual 断言两段 JSON 语义相等，忽略键序与空白。
//
// 用于「同一请求走快通道与走 Canonical 转换，结果应当一致」这类对照测试——
// 两条路径的字段输出顺序不同是正常的，语义不同才是 bug。
func AssertJSONEqual(t *testing.T, want, got []byte, msg string) {
	t.Helper()

	var w, g any
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("testkit: 期望值不是合法 JSON: %v", err)
	}
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("testkit: 实际值不是合法 JSON: %v", err)
	}
	if diff := cmp.Diff(w, g); diff != "" {
		t.Errorf("%s (-want +got):\n%s", msg, diff)
	}
}
