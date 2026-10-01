package testkit

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeWSInput(t *testing.T, dir, name string, raw []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func wsFixtureBytes(t *testing.T) []byte {
	t.Helper()
	raw, err := json.Marshal(syntheticEnvelope())
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// 防止大小、base64 解码或 JSON 的重复键在专用入口被绕过。
func TestWSReadBudget(t *testing.T) {
	dir := t.TempDir()
	raw := wsFixtureBytes(t)
	path := writeWSInput(t, dir, "a.json", raw)
	valid := DefaultWSLimits()
	if f, err := ReadWSFixture(path, valid); err != nil || f.Name != "synthetic-ws-envelope" {
		t.Fatalf("合法文件读取失败: %v", err)
	}
	for _, tt := range []struct {
		name   string
		change func(*WSLimits)
	}{
		{"file", func(l *WSLimits) { l.FileBytes = int64(len(raw) - 1) }},
		{"message", func(l *WSLimits) { l.MessageBytes = 10 }},
		{"trace", func(l *WSLimits) { l.TraceBytes = 10 }},
		{"nodes", func(l *WSLimits) { l.Nodes = 5 }},
		{"edges", func(l *WSLimits) { l.Edges = 4 }},
		{"rules", func(l *WSLimits) { l.FieldRules = 1 }},
		{"depth", func(l *WSLimits) { l.JSONDepth = 2 }},
		{"negative", func(l *WSLimits) { l.FileBytes = -1 }},
		{"zero", func(l *WSLimits) { l.MessageBytes = 0 }},
		{"replay", func(l *WSLimits) { l.Replay = 0 }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			limits := valid
			tt.change(&limits)
			if _, err := ReadWSFixture(path, limits); err == nil {
				t.Fatal("超预算或非正预算被接纳")
			}
		})
	}
	for _, tt := range []struct{ name, raw string }{
		{"no ws", `{"name":"http","response":{"status":200,"body":{}}}`},
		{"null ws", `{"name":"http","response":{"status":200,"ws":null}}`},
		{"second object", string(raw) + ` {}`},
		{"unknown", strings.Replace(string(raw), `"version":1`, `"unknown":1,"version":1`, 1)},
		{"case alias", strings.Replace(string(raw), `"version":1`, `"Version":1,"version":1`, 1)},
		{"duplicate", strings.Replace(string(raw), `"version":1`, `"version":1,"version":1`, 1)},
		{"duplicate escaped", strings.Replace(string(raw), `"version":1`, `"version":1,"\u0076ersion":1`, 1)},
		{"deep raw", strings.Replace(string(raw), `"upstream_expected_status":101`, `"upstream_expected_status":101,"upstream_error":`+strings.Repeat(`[`, 65)+`0`+strings.Repeat(`]`, 65), 1)},
		{"null message", strings.Replace(string(raw), `"message":{"opcode":1,"payload":"eyJ0eXBlIjoicmVzcG9uc2UuY3JlYXRlIn0="}`, `"message":null`, 1)},
		{"missing payload", strings.Replace(string(raw), `"payload":"eyJ0eXBlIjoicmVzcG9uc2UuY3JlYXRlIn0="`, `"payload":null`, 1)},
		{"null close", strings.Replace(string(raw), `"close_code":1000`, `"close_code":null`, 1)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := writeWSInput(t, t.TempDir(), "fixture.json", []byte(tt.raw))
			if _, err := ReadWSFixture(p, valid); err == nil {
				t.Fatal("不合法文件被接纳")
			}
		})
	}
	t.Run("directory", func(t *testing.T) {
		d := t.TempDir()
		f := syntheticEnvelope()
		f.Name = "second"
		b, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		writeWSInput(t, d, "b.json", b)
		writeWSInput(t, d, "a.json", raw)
		writeWSInput(t, d, "ignore.txt", []byte("not json"))
		fixtures, err := ReadWSFixtureDir(d, valid)
		if err != nil || len(fixtures) != 2 || fixtures[0].Name != "synthetic-ws-envelope" || fixtures[1].Name != "second" {
			t.Fatalf("目录排序失败: %v", err)
		}
		limits := valid
		limits.DirectoryBytes = int64(len(raw) + len(b) - 1)
		if _, err := ReadWSFixtureDir(d, limits); err == nil {
			t.Fatal("超目录预算被接纳")
		}
		if err := os.Symlink(path, filepath.Join(d, "c.json")); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadWSFixtureDir(d, valid); err == nil {
			t.Fatal("目录符号链接被接纳")
		}
	})
	if _, err := ReadWSFixtureDir(t.TempDir(), valid); err == nil {
		t.Fatal("空目录被接纳")
	}
	t.Run("exact file and directory", func(t *testing.T) {
		limits := valid
		limits.FileBytes = int64(len(raw))
		limits.DirectoryBytes = int64(len(raw))
		if _, err := ReadWSFixtureDir(dir, limits); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("symlink directory", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(dir, link); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadWSFixtureDir(link, valid); err == nil {
			t.Fatal("符号链接目录被接纳")
		}
	})
}

// Load 的 Fatal 通过独立进程观察，防止测试框架吞掉拒绝行为。
func TestWSLegacyLoadBounded(t *testing.T) {
	if path := os.Getenv("OMUGW_WS_LOAD_TEST"); path != "" {
		Load(t, path)
		return
	}
	for _, tt := range []struct {
		name     string
		raw      []byte
		rejected bool
		hint     string
	}{
		{"ws", wsFixtureBytes(t), true, "ReadWSFixture"},
		{"null ws", []byte(`{"name":"null","response":{"status":200,"ws":null}}`), true, "ReadWSFixture"},
		{"duplicate masked ws", []byte(`{"name":"masked","response":{"status":200,"ws":{},"ws":null}}`), true, "ReadWSFixture"},
		{"duplicate response ws", []byte(`{"name":"masked","response":{"status":200,"ws":{}},"response":{"status":200}}`), true, "ReadWSFixture"},
		{"unknown body ws key", []byte(`{"name":"http","response":{"status":200,"body":{"response":{"ws":false}}}}`), false, ""},
		{"large number http", []byte(`{"name":"http","response":{"status":200,"body":{"value":1e999}}}`), false, ""},
		{"large http", append([]byte(`{"name":"large","response":{"status":200}}`), []byte(strings.Repeat(" ", 8<<20))...), true, "预算"},
		{"http unknown", []byte(`{"name":"http","unknown":false,"response":{"status":200,"body":{}}}`), false, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := writeWSInput(t, t.TempDir(), "fixture.json", tt.raw)
			cmd := exec.Command(os.Args[0], "-test.run=^TestWSLegacyLoadBounded$")
			cmd.Env = append(os.Environ(), "OMUGW_WS_LOAD_TEST="+path)
			output, err := cmd.CombinedOutput()
			if (err != nil) != tt.rejected {
				t.Fatalf("Load 拒绝=%v, 预期=%v: %s", err != nil, tt.rejected, output)
			}
			if tt.hint != "" && !strings.Contains(string(output), tt.hint) {
				t.Fatalf("Load 未指示预期入口或预算: %s", output)
			}
		})
	}
}
