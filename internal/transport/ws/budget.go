package ws

import (
	"errors"
	"sync"
	"unicode/utf8"
)

// ErrBufferLimit 是共享容量不足，不是对端违反单条消息上限。
var ErrBufferLimit = errors.New("ws: 共享缓冲容量不足")

// BufferBudget 统计仍由传输层、Message 或 CloseError 调用方持有的 payload 容量，
// 包括扩容时的新旧数组、掩码副本和关闭原因字符串；不代表 Go 堆或进程 RSS。
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

// AllocateMessage 先预占精确容量再让编码器直填，避免本地错误先在预算外造一份负载。
// fill 不得保留切片供交付后修改；成功后的所有权与读入 Message 相同，须 Release。
func AllocateMessage(b *BufferBudget, op Opcode, size int, fill func([]byte) error) (*Message, error) {
	// 工厂只构造完整业务消息，不能先分配/填充再把分片、控制或保留 opcode 冒充完整消息。
	if op != OpText && op != OpBinary {
		return nil, ErrProtocol
	}
	if size < 0 {
		return nil, ErrBufferLimit
	}
	if fill == nil {
		return nil, ErrProtocol
	}
	if err := b.acquire(int64(size)); err != nil {
		return nil, err
	}
	m := &Message{Opcode: op, Payload: make([]byte, size), owned: &messageOwnership{budget: b, size: int64(size)}}
	if err := fill(m.Payload); err != nil {
		m.Release()
		return nil, err
	}
	if op == OpText && !utf8.Valid(m.Payload) {
		m.Release()
		return nil, ErrInvalidUTF8
	}
	return m, nil
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
