package testkit

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// SSEEvent 是一个 Server-Sent Event。
type SSEEvent struct {
	Event string `json:"event,omitempty"`
	Data  string `json:"data"`
	ID    string `json:"id,omitempty"`
	Retry string `json:"retry,omitempty"`
}

// DoneSentinel 是 OpenAI 系协议的流结束标记。
// Anthropic 不用它——这个差异必须在转换时处理，漏掉会让 OpenAI 客户端一直等下去。
const DoneSentinel = "[DONE]"

// ParseSSE 按 W3C SSE 规范解析事件流。
//
// 刻意实现完整规范而不是简单的 strings.Split：上游真的会发多行 data、
// 注释行、以及不带空格的 "data:" 前缀。用简化解析器录制出来的 fixture
// 会与真实字节流有出入，那样的回放测试是自欺欺人。
func ParseSSE(r io.Reader) ([]SSEEvent, error) {
	sc := bufio.NewScanner(r)
	// 单条 SSE 消息可能很大（含 base64 音频块），默认 64KB 上限不够。
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	var (
		out  []SSEEvent
		cur  SSEEvent
		data []string
		open bool
	)

	flush := func() {
		if !open {
			return
		}
		cur.Data = strings.Join(data, "\n")
		out = append(out, cur)
		cur, data, open = SSEEvent{}, nil, false
	}

	for sc.Scan() {
		line := strings.TrimSuffix(sc.Text(), "\r")

		if line == "" {
			flush()
			continue
		}
		// 注释行。冒号开头的行被忽略，常用于心跳保活。
		if strings.HasPrefix(line, ":") {
			continue
		}

		field, value, found := strings.Cut(line, ":")
		if !found {
			// 无冒号的行，整行是字段名，值为空。
			field, value = line, ""
		}
		// 规范要求剥掉值前的**一个**空格，多余的空格属于数据。
		value = strings.TrimPrefix(value, " ")

		open = true
		switch field {
		case "event":
			cur.Event = value
		case "data":
			data = append(data, value)
		case "id":
			cur.ID = value
		case "retry":
			cur.Retry = value
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("testkit: 解析 SSE 失败: %w", err)
	}
	// 流末尾没有空行时，最后一个事件仍然有效。
	flush()

	return out, nil
}

// WriteSSE 把事件写成 SSE 字节流。
func WriteSSE(w io.Writer, evs []SSEEvent) error {
	for _, ev := range evs {
		if err := writeOne(w, ev); err != nil {
			return err
		}
	}
	return nil
}

func writeOne(w io.Writer, ev SSEEvent) error {
	var b strings.Builder
	if ev.ID != "" {
		fmt.Fprintf(&b, "id: %s\n", ev.ID)
	}
	if ev.Event != "" {
		fmt.Fprintf(&b, "event: %s\n", ev.Event)
	}
	if ev.Retry != "" {
		fmt.Fprintf(&b, "retry: %s\n", ev.Retry)
	}
	// 多行 data 必须逐行加前缀，否则接收端会把换行后的内容当成新字段。
	for _, line := range strings.Split(ev.Data, "\n") {
		fmt.Fprintf(&b, "data: %s\n", line)
	}
	b.WriteString("\n")

	_, err := io.WriteString(w, b.String())
	return err
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
		if strings.TrimSpace(ev.Data) == DoneSentinel {
			continue
		}
		out = append(out, ev)
	}
	return out
}
