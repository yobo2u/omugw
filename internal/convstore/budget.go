package convstore

import (
	"math"
	"unsafe"

	"github.com/yobo2u/omugw/internal/canonical"
)

// 按将要保留的结构和所有变长字段记账，不能让大量短内容块以近零成本入库。
// Sizeof 只取类型布局，不访问裸指针；每个分配留对齐余量，每轮留索引余量。
// 这是存储保留量的保守估算，不是整个进程的 RSS 上限。
func turnBytes(turn Turn) int64 {
	var b retainedBudget
	b.add(int64(unsafe.Sizeof(entry{})) + 256)
	b.add(turn.InlineBytes)
	for _, s := range []string{turn.ID, turn.PrevID, turn.Owner, turn.Model} {
		b.allocation(len(s))
	}
	b.allocation(len(turn.Opaque))
	for _, m := range turn.Messages {
		b.add(int64(unsafe.Sizeof(canonical.Message{})))
		b.allocation(len(m.Role))
		b.allocation(len(m.Name))
		b.parts(m.Parts)
	}
	if len(turn.Messages) > 0 {
		b.add(32)
	}
	return b.total
}

type retainedBudget struct{ total int64 }

// 饱和而不是回绕；内部调用也不能借 InlineBytes 的大数把预算变成负数。
func (b *retainedBudget) add(n int64) {
	if n < 0 || n > math.MaxInt64-b.total {
		b.total = math.MaxInt64
		return
	}
	b.total += n
}

func (b *retainedBudget) allocation(n int) {
	if n > 0 {
		b.add(int64(n))
		b.add(32)
	}
}

func (b *retainedBudget) parts(parts []canonical.Part) {
	if len(parts) > 0 {
		b.add(32)
	}
	for _, p := range parts {
		b.add(int64(unsafe.Sizeof(canonical.Part{})))
		b.allocation(len(p.Kind))
		b.allocation(len(p.Text))
		if p.Thinking != nil {
			b.add(int64(unsafe.Sizeof(canonical.Thinking{})) + 32)
			b.allocation(len(p.Thinking.Text))
			b.allocation(len(p.Thinking.Signature))
		}
		if m := p.Media; m != nil {
			b.add(int64(unsafe.Sizeof(canonical.Media{})) + 32)
			for _, s := range []string{string(m.Kind), m.MIMEType, m.Detail, m.URL} {
				b.allocation(len(s))
			}
			b.allocation(len(m.Data))
			if m.FileRef != nil {
				b.add(int64(unsafe.Sizeof(canonical.FileRef{})) + 32)
				b.allocation(len(m.FileRef.Provider))
				b.allocation(len(m.FileRef.ID))
			}
			if m.Audio != nil {
				b.add(int64(unsafe.Sizeof(canonical.AudioFmt{})) + 32)
				b.allocation(len(m.Audio.Encoding))
			}
		}
		if p.ToolCall != nil {
			b.add(int64(unsafe.Sizeof(canonical.ToolCall{})) + 32)
			b.allocation(len(p.ToolCall.ID))
			b.allocation(len(p.ToolCall.Name))
			b.allocation(len(p.ToolCall.Arguments))
		}
		if p.ToolResult != nil {
			b.add(int64(unsafe.Sizeof(canonical.ToolResult{})) + 32)
			b.allocation(len(p.ToolResult.CallID))
			b.parts(p.ToolResult.Content)
		}
	}
}
