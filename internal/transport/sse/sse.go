// Package sse 实现 Server-Sent Events 的流式读写。
//
// 与「把整条流读完再解析」的写法相对：那样做对测试辅助尚可，对代理是致命的
// ——网关如果先攒完再转发，流式就名存实亡，客户端要等到最后一个 token 才看到
// 第一个字。这里的 Reader 逐事件返回，Writer 逐事件 flush。
package sse

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// DoneSentinel 是 OpenAI 系协议的流结束标记。
//
// Anthropic 不用它——这个差异必须在转换时处理。漏掉会让 OpenAI 客户端
// 一直等下去，因为它在等一个永远不会到来的 [DONE]。
const DoneSentinel = "[DONE]"

// MaxEventBytes 是单条事件的大小上限。
//
// 上限必须存在：Realtime 的音频块是 base64 编码的，单条可以很大，而一个
// 恶意或故障的上游可以发一条永不结束的「事件」把网关内存吃光。
const MaxEventBytes = 8 << 20 // 8 MiB

// Event 是一条 Server-Sent Event。
//
// 带 JSON 标签是为了让 fixture 直接以它入库——测试辅助与生产代码共用同一个
// 类型和同一个解析器，两份实现漂移的可能性从此消失。fixture 的可信度本就
// 建立在「回放的就是生产会读到的」这个前提上。
type Event struct {
	Event string `json:"event,omitempty"`
	Data  string `json:"data"`
	ID    string `json:"id,omitempty"`
	Retry string `json:"retry,omitempty"`
}

// IsDone 报告这是否是 OpenAI 系的流结束哨兵。
func (e Event) IsDone() bool { return strings.TrimSpace(e.Data) == DoneSentinel }

// Reader 逐事件读取 SSE 流。
type Reader struct {
	sc  *bufio.Scanner
	err error
}

// NewReader 包装一个字节流。
func NewReader(r io.Reader) *Reader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), MaxEventBytes)
	return &Reader{sc: sc}
}

// Next 返回下一条事件。流正常结束时返回 io.EOF。
//
// 空行是事件分隔符；流末尾没有空行时，最后一条事件仍然有效——真实上游确实会
// 这样收尾，丢掉它会让客户端少收到最后一个 token。
func (r *Reader) Next() (Event, error) {
	if r.err != nil {
		return Event{}, r.err
	}

	var (
		ev   Event
		data []string
		open bool
	)

	for r.sc.Scan() {
		line := strings.TrimSuffix(r.sc.Text(), "\r")

		if line == "" {
			if !open {
				// 连续空行，跳过。
				continue
			}
			ev.Data = strings.Join(data, "\n")
			return ev, nil
		}
		// 冒号开头是注释，常用于心跳保活。
		if strings.HasPrefix(line, ":") {
			continue
		}

		field, value, found := strings.Cut(line, ":")
		if !found {
			field, value = line, ""
		}
		// 规范要求剥掉值前的**一个**空格，多余的空格属于数据。
		value = strings.TrimPrefix(value, " ")

		open = true
		switch field {
		case "event":
			ev.Event = value
		case "data":
			data = append(data, value)
		case "id":
			ev.ID = value
		case "retry":
			ev.Retry = value
		}
	}

	if err := r.sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			r.err = fmt.Errorf("sse: 单条事件超过 %d 字节上限", MaxEventBytes)
		} else {
			r.err = fmt.Errorf("sse: 读取失败: %w", err)
		}
		return Event{}, r.err
	}

	if open {
		// 末尾缺空行，但事件是完整的。
		ev.Data = strings.Join(data, "\n")
		r.err = io.EOF
		return ev, nil
	}

	r.err = io.EOF
	return Event{}, io.EOF
}

// Writer 逐事件写出 SSE 流。
type Writer struct {
	w  io.Writer
	f  http.Flusher
	bw *bufio.Writer
}

// NewWriter 包装一个 ResponseWriter，并设置流式响应必需的头。
//
// 不支持 Flush 的 ResponseWriter 会直接报错而不是降级成缓冲写——一个不 flush
// 的「流式」响应会把全部内容攒到最后一次性吐出，看起来能用，实际上把这个网关
// 的核心卖点悄悄废掉了。
func NewWriter(w http.ResponseWriter) (*Writer, error) {
	f, ok := w.(http.Flusher)
	if !ok {
		return nil, errors.New("sse: ResponseWriter 不支持 Flush，无法流式输出")
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// 关掉 nginx 一类反代的响应缓冲，否则它们会替我们把流重新攒起来。
	h.Set("X-Accel-Buffering", "no")

	return &Writer{w: w, f: f, bw: bufio.NewWriter(w)}, nil
}

// NewWriterTo 包装任意 io.Writer，用于测试与非 HTTP 场景。
func NewWriterTo(w io.Writer) *Writer {
	return &Writer{w: w, bw: bufio.NewWriter(w)}
}

// Write 写出一条事件并立即 flush。
func (w *Writer) Write(ev Event) error {
	if ev.ID != "" {
		fmt.Fprintf(w.bw, "id: %s\n", ev.ID)
	}
	if ev.Event != "" {
		fmt.Fprintf(w.bw, "event: %s\n", ev.Event)
	}
	if ev.Retry != "" {
		fmt.Fprintf(w.bw, "retry: %s\n", ev.Retry)
	}
	// 多行 data 必须逐行加前缀，否则接收端会把换行后的内容当成新字段。
	for _, line := range strings.Split(ev.Data, "\n") {
		fmt.Fprintf(w.bw, "data: %s\n", line)
	}
	w.bw.WriteString("\n")

	if err := w.bw.Flush(); err != nil {
		return fmt.Errorf("sse: 写出事件失败: %w", err)
	}
	if w.f != nil {
		w.f.Flush()
	}
	return nil
}

// WriteDone 写出 OpenAI 系的流结束哨兵。
func (w *Writer) WriteDone() error { return w.Write(Event{Data: DoneSentinel}) }

// Comment 写出一条注释行，用于心跳保活。
//
// 长时间没有内容时需要它：中间的反代和负载均衡会掐掉看起来空闲的连接，
// 而一个正在思考的推理模型确实可以几十秒不吐字。
func (w *Writer) Comment(text string) error {
	fmt.Fprintf(w.bw, ": %s\n\n", text)
	if err := w.bw.Flush(); err != nil {
		return fmt.Errorf("sse: 写出心跳失败: %w", err)
	}
	if w.f != nil {
		w.f.Flush()
	}
	return nil
}
