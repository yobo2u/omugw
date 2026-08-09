package sse

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func readAll(t *testing.T, r *Reader) []Event {
	t.Helper()
	var out []Event
	for {
		ev, err := r.Next()
		if err == nil {
			out = append(out, ev)
			continue
		}
		if errors.Is(err, io.EOF) {
			// 末尾缺空行时，最后一条事件与 io.EOF 一起返回。
			if ev.Data != "" || ev.Event != "" {
				out = append(out, ev)
			}
			return out
		}
		t.Fatalf("读取失败: %v", err)
	}
}

func TestReaderHandlesRealWorldQuirks(t *testing.T) {
	// 每一处古怪都是上游真的会发出来的。
	raw := "event: message_start\n" +
		"data:{\"type\":\"message_start\"}\n" +
		"\n" +
		": keep-alive\n" +
		"\n" +
		"event: content_block_delta\n" +
		"data: line one\n" +
		"data: line two\n" +
		"\n" +
		"data: [DONE]\n"

	evs := readAll(t, NewReader(strings.NewReader(raw)))
	if len(evs) != 3 {
		t.Fatalf("解析出 %d 条事件，期望 3 条: %+v", len(evs), evs)
	}
	if evs[0].Data != `{"type":"message_start"}` {
		t.Errorf("无空格的 data: 前缀解析有误: %q", evs[0].Data)
	}
	if evs[1].Data != "line one\nline two" {
		t.Errorf("多行 data 应以换行连接，实际 %q", evs[1].Data)
	}
	if !evs[2].IsDone() {
		t.Errorf("末尾缺空行时最后一条事件仍应有效，实际 %+v", evs[2])
	}
}

// TestReaderIsIncremental 是这个包存在的理由。
//
// 逐事件返回，而不是读到 EOF 再解析。网关如果先攒完再转发，流式就名存实亡
// ——客户端要等到最后一个 token 才看到第一个字。
func TestReaderIsIncremental(t *testing.T) {
	pr, pw := io.Pipe()
	r := NewReader(pr)

	go func() {
		_, _ = io.WriteString(pw, "data: first\n\n")
		// 刻意不关闭：如果 Reader 要读到 EOF 才肯返回，下面就会卡死。
	}()

	done := make(chan Event, 1)
	go func() {
		ev, err := r.Next()
		if err != nil {
			t.Errorf("读取首条事件失败: %v", err)
			close(done)
			return
		}
		done <- ev
	}()

	select {
	case ev, ok := <-done:
		if !ok {
			t.Fatal("读取失败")
		}
		if ev.Data != "first" {
			t.Errorf("首条事件 = %q", ev.Data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Reader 在流未结束时阻塞——说明它在等 EOF，流式失效")
	}
	_ = pw.Close()
}

// TestReaderPreservesExtraSpaces 固化规范细节：只剥掉一个前导空格。
// base64 音频块对这类细节很敏感。
func TestReaderPreservesExtraSpaces(t *testing.T) {
	evs := readAll(t, NewReader(strings.NewReader("data:  two spaces\n\n")))
	if len(evs) != 1 || evs[0].Data != " two spaces" {
		t.Errorf("应只剥掉一个前导空格，实际 %+v", evs)
	}
}

// TestReaderRejectsOversizedEvent 固化「上限必须存在」。
// 一个故障或恶意的上游可以发一条永不结束的事件把网关内存吃光。
func TestReaderRejectsOversizedEvent(t *testing.T) {
	huge := "data: " + strings.Repeat("x", MaxEventBytes+1024) + "\n\n"

	_, err := NewReader(strings.NewReader(huge)).Next()
	if err == nil {
		t.Fatal("超大事件应当报错")
	}
	if !strings.Contains(err.Error(), "上限") {
		t.Errorf("错误信息应说明是超限，实际: %v", err)
	}
}

func TestRoundTrip(t *testing.T) {
	want := []Event{
		{Event: "a", Data: `{"x":1}`},
		{Event: "b", Data: "multi\nline"},
		{ID: "42", Data: "with id"},
		{Data: DoneSentinel},
	}

	var buf strings.Builder
	w := NewWriterTo(&buf)
	for _, ev := range want {
		if err := w.Write(ev); err != nil {
			t.Fatal(err)
		}
	}

	got := readAll(t, NewReader(strings.NewReader(buf.String())))
	if len(got) != len(want) {
		t.Fatalf("往返后事件数 = %d, 期望 %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("事件 %d 往返后不一致:\n got %+v\nwant %+v", i, got[i], want[i])
		}
	}
}

// TestWriterFlushesEachEvent 固化「每条事件立即 flush」。
//
// 不 flush 的「流式」响应会把全部内容攒到最后一次性吐出——看起来能用，
// 实际上把这个网关的核心卖点悄悄废掉了。
func TestWriterFlushesEachEvent(t *testing.T) {
	rec := &countingRecorder{ResponseRecorder: httptest.NewRecorder()}

	w, err := NewWriter(rec)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := w.Write(Event{Data: "chunk"}); err != nil {
			t.Fatal(err)
		}
	}

	if n := rec.flushes(); n < 3 {
		t.Errorf("写出 3 条事件只 flush 了 %d 次，流式失效", n)
	}
}

// TestNewWriterRejectsNonFlusher 固化「宁可报错也不降级成缓冲写」。
func TestNewWriterRejectsNonFlusher(t *testing.T) {
	if _, err := NewWriter(nonFlusher{rec: httptest.NewRecorder()}); err == nil {
		t.Fatal("不支持 Flush 的 ResponseWriter 应当直接报错，而不是静默降级")
	}
}

func TestWriterSetsStreamingHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	if _, err := NewWriter(rec); err != nil {
		t.Fatal(err)
	}

	for k, want := range map[string]string{
		"Content-Type":  "text/event-stream",
		"Cache-Control": "no-cache",
		// 中间的反代会替我们把流重新攒起来，必须显式关掉。
		"X-Accel-Buffering": "no",
	} {
		if got := rec.Header().Get(k); got != want {
			t.Errorf("%s = %q, 期望 %q", k, got, want)
		}
	}
}

// TestCommentIsIgnoredByReader 验证心跳不会被当成事件。
//
// 长时间无内容时需要心跳：反代会掐掉看起来空闲的连接，而一个正在思考的
// 推理模型确实可以几十秒不吐字。
func TestCommentIsIgnoredByReader(t *testing.T) {
	var buf strings.Builder
	w := NewWriterTo(&buf)
	_ = w.Comment("keep-alive")
	_ = w.Write(Event{Data: "real"})

	evs := readAll(t, NewReader(strings.NewReader(buf.String())))
	if len(evs) != 1 || evs[0].Data != "real" {
		t.Errorf("心跳不应被当成事件，实际 %+v", evs)
	}
}

type countingRecorder struct {
	*httptest.ResponseRecorder
	mu sync.Mutex
	n  int
}

func (c *countingRecorder) Flush() {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	c.ResponseRecorder.Flush()
}

func (c *countingRecorder) flushes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// nonFlusher 是一个**不**实现 http.Flusher 的 ResponseWriter。
//
// 刻意用具名字段而不是嵌入：嵌入会把 ResponseRecorder 的 Flush 方法提升上来，
// 于是这个「假人」反而满足了它本该不满足的接口，测试就成了空转。
type nonFlusher struct{ rec *httptest.ResponseRecorder }

func (n nonFlusher) Header() http.Header         { return n.rec.Header() }
func (n nonFlusher) Write(b []byte) (int, error) { return n.rec.Write(b) }
func (n nonFlusher) WriteHeader(c int)           { n.rec.WriteHeader(c) }
