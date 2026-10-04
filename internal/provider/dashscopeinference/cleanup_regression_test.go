package dashscopeinference

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/provider"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

const cleanupFaultEnv = "OMUGW_INFERENCE_TEST_CLEANUP_FAULT"

// 兜底晚于最长的 2s 预算 + race 500ms 验收上界，不能替代验收证据。
const independentCleanupDelay = 3 * time.Second

// 子进程必须由断言失败自行退出；外层杀进程只防测试失控，绝不能算清理成功。
func TestInferenceProviderCleanupRegression(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"超限失败流必须关闭而非排空", "共同握手期限"} {
		for _, fault := range []string{"leak", "stall"} {
			t.Run(name+"/"+fault, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, executable, "-test.v", "-test.run=^TestInferenceProviderBounds$/^"+regexp.QuoteMeta(name)+"$")
				cmd.Env = append(os.Environ(), cleanupFaultEnv+"="+fault)
				output, err := cmd.CombinedOutput()
				if ctx.Err() != nil {
					t.Fatalf("故障用例清理未在固定上限内退出: %v\n%s", ctx.Err(), output)
				}
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 1 {
					t.Fatalf("故障必须命中断言失败，不能通过或崩溃: %v\n%s", err, output)
				}
				marker := "被测 socket 未释放"
				if fault == "stall" {
					marker = "期限验收失败"
				}
				if !strings.Contains(string(output), marker) || !strings.Contains(string(output), "INFERENCE_CLEANUP_JOINED") || strings.Contains(string(output), "panic:") {
					t.Fatalf("未证明具名失败与 handler/socket 清理 join: %s", output)
				}
				t.Logf("局部故障得到预期失败并完成清理:\n%s", output)
			})
		}
	}
}

// 仅故障子进程替换测试调用边界：保留真实本地 TCP，模拟失败返回漏关或拨号不返回。
// 不给生产 Dial 增加 hook，避免为测试修改实际握手行为。
func dialForCleanupProbe(t *testing.T, p *Provider, ctx context.Context, req provider.Request, entered <-chan struct{}, failureBody bool) (*ws.Conn, *http.Response, error) {
	t.Helper()
	fault := os.Getenv(cleanupFaultEnv)
	if fault == "" {
		return p.Dial(ctx, req)
	}
	if fault != "leak" && fault != "stall" {
		t.Fatalf("未知局部故障: %q", fault)
	}
	u, err := url.Parse(req.Target.BaseURL)
	if err != nil || u.Hostname() != "127.0.0.1" {
		t.Fatal("故障注入只允许本地测试服务端")
	}
	c, err := net.DialTimeout("tcp", u.Host, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// 持有引用到测试结束，防 GC 终结器提前关闭泄漏连接，掩盖反例。
	t.Cleanup(func() { runtime.KeepAlive(c) })
	r, err := http.NewRequest(http.MethodGet, req.Target.BaseURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := r.Write(c); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("故障注入未进入服务端 handler")
	}
	if fault == "stall" {
		_, _ = io.Copy(io.Discard, c)
	} else if !failureBody {
		<-ctx.Done()
	}
	e := canonical.Newf(canonical.ClassUpstreamUnavailable, "局部故障注入")
	if failureBody {
		return nil, &http.Response{StatusCode: 503, Body: http.NoBody}, e
	}
	return nil, nil, e
}

type failureUpstream struct {
	URL      string
	entered  chan struct{}
	released chan struct{}
	forced   atomic.Bool
}

// 在 Dial 前启动独立兜底；失败断言先观察真实释放，cleanup 才放行 handler 并强关 TCP。
func newFailureUpstream(t *testing.T, failureBody bool) *failureUpstream {
	t.Helper()
	u := &failureUpstream{entered: make(chan struct{}), released: make(chan struct{})}
	stop, handlerDone := make(chan struct{}), make(chan struct{})
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(u.entered)
		defer close(handlerDone)
		if failureBody {
			w.WriteHeader(503)
			_, _ = io.WriteString(w, strings.Repeat("x", (64<<10)+1))
			w.(http.Flusher).Flush()
		}
		select {
		case <-r.Context().Done():
			if !u.forced.Load() {
				close(u.released)
			}
		case <-stop:
		}
	}))
	var mu sync.Mutex
	connections := make(map[net.Conn]bool)
	stopping := false
	s.Config.ConnState = func(c net.Conn, state http.ConnState) {
		mu.Lock()
		forceClose := false
		switch state {
		case http.StateNew:
			connections[c] = true
			forceClose = stopping
		case http.StateClosed:
			delete(connections, c)
		}
		mu.Unlock()
		// 覆盖清理快照与 Accept 的竞争，避免快照之后进来的连接漏掉强关。
		if forceClose {
			_ = c.Close()
		}
	}
	s.Start()
	u.URL = s.URL
	cleanupNow, cleanupDone := make(chan struct{}), make(chan struct{})
	timer := time.NewTimer(independentCleanupDelay)
	go func() {
		defer close(cleanupDone)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-cleanupNow:
		}
		// 必须先标记，防关闭 socket 触发 request context 后冒充被测释放。
		u.forced.Store(true)
		close(stop)
		mu.Lock()
		stopping = true
		active := make([]net.Conn, 0, len(connections))
		for c := range connections {
			active = append(active, c)
		}
		mu.Unlock()
		for _, c := range active {
			_ = c.Close()
		}
		s.Close()
		select {
		case <-u.entered:
			<-handlerDone
		default:
		}
	}()
	t.Cleanup(func() {
		close(cleanupNow)
		select {
		case <-cleanupDone:
			t.Log("INFERENCE_CLEANUP_JOINED")
		case <-time.After(time.Second):
			t.Error("独立清理未 join，不能算有界退出成功")
		}
	})
	return u
}

func assertNetworkDeadline(t *testing.T, start time.Time, budget time.Duration) {
	t.Helper()
	if elapsed := time.Since(start); elapsed > budget+networkSchedulingTolerance {
		t.Fatalf("期限验收失败: elapsed=%v budget=%v tolerance=%v", elapsed, budget, networkSchedulingTolerance)
	}
}

func (u *failureUpstream) assertReleasedBy(t *testing.T, deadline time.Time) {
	t.Helper()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-u.released:
		if u.forced.Load() || time.Now().After(deadline) {
			t.Fatal("被测 socket 未释放: 超出绝对截止或已触发清理兜底")
		}
	case <-timer.C:
		t.Fatal("被测 socket 未释放: 验收截止已到")
	}
}
