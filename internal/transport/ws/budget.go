package ws

import (
	"errors"
	"sync"
)

// ErrBufferLimit 是共享容量不足，不是对端违反单条消息上限。
var ErrBufferLimit = errors.New("ws: 共享缓冲容量不足")

// BufferBudget 统计仍由传输层或 Message 调用方持有的 payload 容量，
// 包括扩容时并存的新旧数组与掩码副本；不代表 Go 堆或进程 RSS。
// 零值不提供额度，必须经 NewBufferBudget 构造；开始使用后不得复制。
type BufferBudget struct {
	mu          sync.Mutex
	limit, used int64
}

func NewBufferBudget(limit int64) (*BufferBudget, error) {
	if limit <= 0 {
		return nil, errors.New("ws: 缓冲预算必须为正数")
	}
	return &BufferBudget{limit: limit}, nil
}

func (b *BufferBudget) Used() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

// 不能在持有半条消息时等待别的读者归还：两边都等会把预算变成死锁。
func (b *BufferBudget) acquire(n int64) error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if n < 0 || n > b.limit-b.used {
		return ErrBufferLimit
	}
	b.used += n
	return nil
}

func (b *BufferBudget) release(n int64) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.used -= n
	b.mu.Unlock()
}

// Message 的 payload 所有权直到 Release 才结束。Release 后本对象、浅拷贝和
// 所有 Payload 别名均不得再读写；公开切片无法撤销调用方已取得的别名。
// 多个 Release（包括浅拷贝）共享一次归还，不允许与 payload 的使用并发。
type Message struct {
	Opcode  Opcode
	Payload []byte
	owned   *messageOwnership
}

type messageOwnership struct {
	once   sync.Once
	budget *BufferBudget
	size   int64
}

func (m *Message) Release() {
	if m == nil || m.owned == nil {
		return
	}
	m.owned.once.Do(func() {
		m.Payload = nil
		m.owned.budget.release(m.owned.size)
	})
}
