//go:build darwin || linux

package testkit

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// 等待 writer 的 FIFO 不得卡在预算之前；独立进程保证失败也能回收，不为通过而打开写端。
func TestFixtureFIFORejectedWithoutWriter(t *testing.T) {
	if path := os.Getenv("OMUGW_FIXTURE_FIFO_TEST_PATH"); path != "" {
		fmt.Println("fixture-loader-ready")
		var err error
		switch os.Getenv("OMUGW_FIXTURE_FIFO_TEST_MODE") {
		case "file":
			_, err = ReadWSFixture(path, DefaultWSLimits())
		case "dir":
			_, err = ReadWSFixtureDir(filepath.Dir(path), DefaultWSLimits())
		case "legacy":
			Load(t, path)
			t.Fatal("旧 Load 接纳 FIFO")
		}
		if err == nil {
			t.Fatal("FIFO 被接纳")
		}
		return
	}
	for _, mode := range []string{"file", "dir", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "blocked.json")
			if err := syscall.Mkfifo(path, 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestFixtureFIFORejectedWithoutWriter$", "-test.timeout=3s")
			cmd.Env = append(os.Environ(), "OMUGW_FIXTURE_FIFO_TEST_PATH="+path, "OMUGW_FIXTURE_FIFO_TEST_MODE="+mode, "GORACE=atexit_sleep_ms=0")
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			ready := make(chan bool, 1)
			output := make(chan string, 1)
			go func() {
				scanner := bufio.NewScanner(stdout)
				ready <- scanner.Scan() && scanner.Text() == "fixture-loader-ready"
				var lines strings.Builder
				for scanner.Scan() {
					lines.WriteString(scanner.Text())
					lines.WriteByte('\n')
				}
				output <- lines.String()
				done <- cmd.Wait()
			}()
			joined := false
			defer func() {
				if !joined {
					_ = cmd.Process.Kill()
					<-done
				}
			}()
			select {
			case ok := <-ready:
				if !ok {
					t.Fatal("子进程未到 loader 门闩")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("子进程启动未到门闩")
			}
			select {
			case err := <-done:
				joined = true
				out := <-output
				if mode == "legacy" {
					if err == nil || !strings.Contains(out, "普通文件") {
						t.Fatalf("Load 未明确拒绝 FIFO: %s", out)
					}
				} else if err != nil {
					t.Fatalf("loader 子进程错误: %v %s", err, out)
				}
			case <-time.After(300 * time.Millisecond):
				t.Fatal("FIFO 无 writer 时 loader 阻塞在 Open")
			}
		})
	}
}

// file、dir 与旧有界入口必须拒绝末级符号链接，不能读它指向的内容。
func TestFixtureFileRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	regular := writeWSInput(t, t.TempDir(), "ordinary.json", wsFixtureBytes(t))
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(regular, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadWSFixture(link, DefaultWSLimits()); err == nil {
		t.Error("直接文件入口跟随符号链接")
	}
	if _, err := ReadWSFixtureDir(dir, DefaultWSLimits()); err == nil {
		t.Error("目录入口跟随符号链接")
	}
	if _, err := readFixtureBounded(link, 8<<20); err == nil {
		t.Error("旧共享入口跟随符号链接")
	}
}

// 在共享 helper 的真实预检/打开边界强制替换；不依赖偶然调度来覆盖 TOCTOU。
func TestFixtureDescriptorRejectsReplacement(t *testing.T) {
	if path := os.Getenv("OMUGW_FIXTURE_REPLACE_TEST_PATH"); path != "" {
		before, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+".replacement", []byte("different"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		switch os.Getenv("OMUGW_FIXTURE_REPLACE_TEST_MODE") {
		case "fifo":
			err = syscall.Mkfifo(path, 0600)
		case "symlink":
			err = os.Symlink(path+".target", path)
		case "regular":
			err = os.Rename(path+".replacement", path)
		}
		if err != nil {
			t.Fatal(err)
		}
		file, err := openFixtureAfterCheck(path, before)
		if file != nil {
			_ = file.Close()
		}
		if err == nil {
			t.Fatal("预检后替换文件未拒绝")
		}
		return
	}
	for _, mode := range []string{"fifo", "symlink", "regular"} {
		t.Run(mode, func(t *testing.T) {
			path := writeWSInput(t, t.TempDir(), "fixture.json", []byte("ordinary"))
			writeWSInput(t, filepath.Dir(path), "fixture.json.target", []byte("target"))
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestFixtureDescriptorRejectsReplacement$", "-test.timeout=2s")
			cmd.Env = append(os.Environ(), "OMUGW_FIXTURE_REPLACE_TEST_PATH="+path, "OMUGW_FIXTURE_REPLACE_TEST_MODE="+mode, "GORACE=atexit_sleep_ms=0")
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("预检后替换未安全拒绝: %v %s", err, output)
			}
		})
	}
}
