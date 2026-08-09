package httpx

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/config"
)

func testTimeouts() config.Timeouts {
	return config.Timeouts{
		Connect:   200 * time.Millisecond,
		FirstByte: 400 * time.Millisecond,
		Total:     5 * time.Second,
		Idle:      300 * time.Millisecond,
	}
}

func get(t *testing.T, c *Client, url string) (*Response, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c.Do(context.Background(), req)
}

func TestDoReturnsUpstreamTiming(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(50 * time.Millisecond)
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	resp, err := get(t, New(testTimeouts(), nil), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.Latency < 40*time.Millisecond {
		t.Errorf("上游首字节耗时 = %v，明显偏低，说明没在量真实时间", resp.Latency)
	}
	if resp.UpstreamFirstByte.IsZero() {
		t.Error("上游首字节时刻未记录——它是换凭据重试的依据")
	}
}

// TestFirstByteTimeoutIsSeparateFromTotal 是这个包存在的理由。
//
// 上游迟迟不给响应头时，必须在 FirstByte 上限触发，而不是等到 Total。
// 两者不分开，一个思考三分钟的推理请求和一个挂死的连接就长得一模一样。
func TestFirstByteTimeoutIsSeparateFromTotal(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release // 永不返回响应头，直到测试结束
	}))
	defer func() { close(release); srv.Close() }()

	tm := testTimeouts()
	start := time.Now()
	_, err := get(t, New(tm, nil), srv.URL)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("上游不返回响应头，应当超时")
	}
	// 必须在首字节上限附近触发，而不是拖到整体上限。
	if elapsed > tm.Total/2 {
		t.Errorf("耗时 %v，说明触发的是整体超时而非首字节超时（上限 %v）", elapsed, tm.FirstByte)
	}

	var cerr *canonical.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("应返回 *canonical.Error，实际 %T", err)
	}
	if cerr.Class != canonical.ClassUpstreamUnavailable {
		t.Errorf("分类 = %q, 期望 upstream_unavailable", cerr.Class)
	}
	// 上游此刻不可用，换个凭据或 Provider 有意义。
	if !cerr.Retryable {
		t.Error("首字节超时应当可重试")
	}
	if !strings.Contains(cerr.Message, "响应头") {
		t.Errorf("错误消息应说明卡在哪一层，实际: %s", cerr.Message)
	}
}

// TestIdleTimeoutCatchesStalledStream 固化「空闲才是上游挂死的真正判据」。
//
// 一条已经开始输出、然后卡住不动的流，整体超时到期前是发现不了的——
// 而那可能是几十分钟。
func TestIdleTimeoutCatchesStalledStream(t *testing.T) {
	stall := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "first chunk")
		f.Flush()
		<-stall // 开了头就不动了
	}))
	defer func() { close(stall); srv.Close() }()

	tm := testTimeouts()
	resp, err := get(t, New(tm, nil), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// 首块能读到。
	buf := make([]byte, 64)
	if n, err := resp.Body.Read(buf); err != nil || n == 0 {
		t.Fatalf("首块应当能读到: n=%d err=%v", n, err)
	}

	// 关键：**直接阻塞在下一次 Read 上**，不在中间手动 sleep。
	//
	// 早先的版本先 sleep 再 Read，恰好绕开了「Read 阻塞住」这件事，
	// 于是一个只在 Read 之前检查空闲时长的实现也能通过——而那个实现对它
	// 唯一要防的场景是完全失效的。真实的流转发就是阻塞在这里等下一块。
	start := time.Now()
	_, err = resp.Body.Read(buf)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("流已空闲超过上限，阻塞中的读取应当被打断")
	}
	if elapsed > tm.Idle*3 {
		t.Errorf("阻塞了 %v 才返回，空闲超时没有打断 Read（上限 %v）", elapsed, tm.Idle)
	}
	var cerr *canonical.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("应返回 *canonical.Error，实际 %T: %v", err, err)
	}
	if !strings.Contains(cerr.Message, "空闲") {
		t.Errorf("错误消息应说明是空闲超时，实际: %s", cerr.Message)
	}
}

// TestIdleTimerResetsOnEachChunk 保证持续输出的长流不会被空闲超时误杀。
//
// 这是上一条测试的反面：只测「卡住会报错」而不测「不卡不会报错」，
// 很容易写出一个把所有长流都掐掉的实现。
func TestIdleTimerResetsOnEachChunk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f, _ := w.(http.Flusher)
		// 间隔小于空闲上限，但总时长远超它。
		for i := 0; i < 6; i++ {
			_, _ = io.WriteString(w, "chunk\n")
			f.Flush()
			time.Sleep(100 * time.Millisecond)
		}
	}))
	defer srv.Close()

	tm := testTimeouts() // Idle = 300ms，总时长约 600ms
	resp, err := get(t, New(tm, nil), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("持续输出的流不该被空闲超时掐断: %v", err)
	}
	if n := strings.Count(string(body), "chunk"); n != 6 {
		t.Errorf("收到 %d 块，期望 6 块", n)
	}
}

// TestClientTimeoutDoesNotCapStreaming 固化「不设 http.Client.Timeout」。
//
// 标准库那把大锤会把读 body 的时间一起算进去，于是一个正常的长流会被拦腰砍断。
func TestClientTimeoutDoesNotCapStreaming(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f, _ := w.(http.Flusher)
		for i := 0; i < 5; i++ {
			_, _ = io.WriteString(w, "x")
			f.Flush()
			time.Sleep(120 * time.Millisecond)
		}
	}))
	defer srv.Close()

	// 首字节上限 400ms，而整条流要跑 600ms。若首字节上限被误用成整体上限，
	// 这里就会失败。
	c := New(testTimeouts(), nil)
	resp, err := get(t, c, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("流被提前掐断: %v", err)
	}
	if len(body) != 5 {
		t.Errorf("收到 %d 字节，期望 5", len(body))
	}
}

// TestTotalTimeoutStillApplies 保证去掉大锤之后整体上限仍然有效。
func TestTotalTimeoutStillApplies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f, _ := w.(http.Flusher)
		// 持续输出，永不结束——空闲超时抓不到它，只能靠整体上限。
		for {
			if _, err := io.WriteString(w, "x"); err != nil {
				return
			}
			f.Flush()
			time.Sleep(20 * time.Millisecond)
		}
	}))
	defer srv.Close()

	tm := testTimeouts()
	tm.Total = 500 * time.Millisecond
	tm.Idle = 400 * time.Millisecond

	resp, err := get(t, New(tm, nil), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	start := time.Now()
	_, err = io.ReadAll(resp.Body)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("永不结束的流应当被整体上限掐断")
	}
	if elapsed > 2*time.Second {
		t.Errorf("耗时 %v，整体上限（%v）没有生效", elapsed, tm.Total)
	}
}

// TestClientCancellationIsNotUpstreamFault 固化一处分类细节。
//
// 客户端主动断开时，把它记成「上游不可用」会污染上游健康度统计，
// 进而让一个健康的 Provider 被误判为故障。
func TestClientCancellationIsNotUpstreamFault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := New(testTimeouts(), nil).Do(ctx, req)
	wg.Wait()

	if err == nil {
		t.Fatal("请求被取消时应当报错")
	}
	var cerr *canonical.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("应返回 *canonical.Error，实际 %T", err)
	}
	if cerr.Class == canonical.ClassUpstreamUnavailable {
		t.Error("客户端主动断开不该记成上游不可用——那会污染上游健康度统计")
	}
	if cerr.Retryable {
		t.Error("客户端都走了，重试没有意义")
	}
}

// TestCloseIsIdempotent 保证重复 Close 不会 panic 或重复释放 context。
func TestCloseIsIdempotent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	resp, err := get(t, New(testTimeouts(), nil), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("第 %d 次 Close 报错: %v", i+1, err)
		}
	}
}
