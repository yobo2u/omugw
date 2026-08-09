package testkit

import (
	"errors"
	"io"
	"strings"

	"github.com/yobo2u/omugw/internal/transport/sse"
)

// SSEEvent 是 fixture 中的一条事件。
//
// 直接复用生产代码的类型，而不是另立一个结构：fixture 的可信度建立在
// 「回放的就是生产会读到的」这个前提上，两个各自演进的类型会悄悄拆掉这个前提。
type SSEEvent = sse.Event

// DoneSentinel 是 OpenAI 系协议的流结束标记。
const DoneSentinel = sse.DoneSentinel

// ParseSSE 把整条流解析成事件切片。
//
// 一次读完只适合测试。生产代码必须用 sse.Reader 逐事件读取——网关如果先攒完
// 再转发，流式就名存实亡。
func ParseSSE(r io.Reader) ([]SSEEvent, error) {
	rd := sse.NewReader(r)

	var out []SSEEvent
	for {
		ev, err := rd.Next()
		if err == nil {
			out = append(out, ev)
			continue
		}
		if errors.Is(err, io.EOF) {
			// 末尾缺空行时，最后一条事件与 io.EOF 一起返回。
			if ev.Data != "" || ev.Event != "" || ev.ID != "" {
				out = append(out, ev)
			}
			return out, nil
		}
		return nil, err
	}
}

// WriteSSE 把事件写成 SSE 字节流。
func WriteSSE(w io.Writer, evs []SSEEvent) error {
	sw := sse.NewWriterTo(w)
	for _, ev := range evs {
		if err := sw.Write(ev); err != nil {
			return err
		}
	}
	return nil
}

// EncodeSSE 把事件序列化成字符串，便于写进 golden 文件。
func EncodeSSE(evs []SSEEvent) string {
	var b strings.Builder
	_ = WriteSSE(&b, evs)
	return b.String()
}

// DataEvents 过滤掉 [DONE] 哨兵，只保留承载数据的事件。
//
// 比对两条流的语义内容时用它——哨兵的有无是协议差异，不是内容差异。
func DataEvents(evs []SSEEvent) []SSEEvent {
	out := make([]SSEEvent, 0, len(evs))
	for _, ev := range evs {
		if ev.IsDone() {
			continue
		}
		out = append(out, ev)
	}
	return out
}
