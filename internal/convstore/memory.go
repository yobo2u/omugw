package convstore

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"sync"
	"time"

	"github.com/yobo2u/omugw/internal/canonical"
)

// MemoryStore 是内存态实现。
//
// **可用性边界，必须与降级矩阵里 EMULATE 的说明保持一致：**
//
//   - 单副本正确。多副本部署时会话不共享——请求被负载均衡到另一个副本会拿到
//     ErrNotFound。要跨副本必须换成 Redis 或等价实现（Phase 2）。
//   - 进程重启后全部丢失。
//   - 容量受 Limits 约束，超限拒绝而不是驱逐——静默丢掉一段历史会让模型收到
//     一个缺了中间几轮的对话，那比直接报错难查得多。
type MemoryStore struct {
	limits Limits
	now    func() time.Time

	mu    sync.RWMutex
	turns map[string]*entry
	// children 用于 Delete 时级联清理后继。
	children map[string][]string
}

type entry struct {
	turn     Turn
	expireAt time.Time
}

// NewMemoryStore 创建内存态会话存储。now 可注入以便测试，传 nil 用 time.Now。
func NewMemoryStore(limits Limits, now func() time.Time) *MemoryStore {
	if now == nil {
		now = time.Now
	}
	return &MemoryStore{
		limits:   limits,
		now:      now,
		turns:    map[string]*entry{},
		children: map[string][]string{},
	}
}

// Append 追加一轮对话。
func (s *MemoryStore) Append(_ context.Context, prevID string, msgs []canonical.Message, model string) (string, error) {
	if len(msgs) == 0 {
		return "", fmt.Errorf("convstore: 不能追加空的一轮")
	}
	for i, m := range msgs {
		if err := m.Validate(); err != nil {
			return "", fmt.Errorf("convstore: messages[%d]: %w", i, err)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()

	depth := 1
	if prevID != "" {
		prev, ok := s.liveLocked(prevID, now)
		if !ok {
			return "", ErrNotFound
		}
		d, err := s.depthLocked(prevID, now)
		if err != nil {
			return "", err
		}
		depth = d + 1
		if depth > s.limits.MaxChainDepth {
			return "", fmt.Errorf("%w: 已达 %d 轮上限", ErrChainTooLong, s.limits.MaxChainDepth)
		}
		// 沿链续期。只延长被实际使用的那条分支，让废弃的分叉自然过期。
		s.touchChainLocked(prev.ID, now)
	}

	id, err := newID()
	if err != nil {
		return "", err
	}

	s.turns[id] = &entry{
		turn: Turn{
			ID:        id,
			PrevID:    prevID,
			Messages:  append([]canonical.Message(nil), msgs...),
			Model:     model,
			CreatedAt: now,
		},
		expireAt: now.Add(s.limits.TTL),
	}
	if prevID != "" {
		s.children[prevID] = append(s.children[prevID], id)
	}
	return id, nil
}

// History 回溯出完整历史，按时间正序。
func (s *MemoryStore) History(_ context.Context, id string) ([]canonical.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()

	// 先逆序收集，再整体反转。逐轮 prepend 在长会话上是 O(n²)。
	var chain []*entry
	cur := id
	for cur != "" {
		e, ok := s.liveLocked(cur, now)
		if !ok {
			return nil, ErrNotFound
		}
		chain = append(chain, s.turns[cur])
		if len(chain) > s.limits.MaxChainDepth {
			return nil, fmt.Errorf("%w: 超过 %d 轮", ErrChainTooLong, s.limits.MaxChainDepth)
		}
		cur = e.PrevID
	}

	total := 0
	for _, e := range chain {
		total += len(e.turn.Messages)
	}
	if total > s.limits.MaxMessages {
		return nil, fmt.Errorf("%w: %d 条，上限 %d", ErrTooLarge, total, s.limits.MaxMessages)
	}

	out := make([]canonical.Message, 0, total)
	for i := len(chain) - 1; i >= 0; i-- {
		out = append(out, chain[i].turn.Messages...)
	}

	s.touchChainLocked(id, now)
	return out, nil
}

// Turn 取出单独一轮。
func (s *MemoryStore) Turn(_ context.Context, id string) (Turn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.liveLocked(id, s.now())
	if !ok {
		return Turn{}, ErrNotFound
	}
	return *t, nil
}

// Delete 删除一轮及其全部后继。
func (s *MemoryStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.turns[id]; !ok {
		return ErrNotFound
	}
	s.deleteSubtreeLocked(id)
	return nil
}

// GC 清理已过期的条目，返回清理数量。
//
// 单独暴露而不是只靠惰性过期：一条从此再没人访问的会话链，惰性过期永远不会
// 碰它，内存就一直挂着。调用方应当定期跑它。
func (s *MemoryStore) GC() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	var dead []string
	for id, e := range s.turns {
		if !e.expireAt.After(now) {
			dead = append(dead, id)
		}
	}
	for _, id := range dead {
		// 可能已被前一轮的级联删除带走。
		if _, ok := s.turns[id]; ok {
			s.deleteSubtreeLocked(id)
		}
	}
	return len(dead)
}

// Len 返回当前保存的轮数，供观测使用。
func (s *MemoryStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.turns)
}

// liveLocked 取出一轮，顺带做惰性过期。
func (s *MemoryStore) liveLocked(id string, now time.Time) (*Turn, bool) {
	e, ok := s.turns[id]
	if !ok {
		return nil, false
	}
	if !e.expireAt.After(now) {
		s.deleteSubtreeLocked(id)
		return nil, false
	}
	return &e.turn, true
}

// depthLocked 计算某一轮所在链的深度。
func (s *MemoryStore) depthLocked(id string, now time.Time) (int, error) {
	depth := 0
	cur := id
	for cur != "" {
		t, ok := s.liveLocked(cur, now)
		if !ok {
			return 0, ErrNotFound
		}
		depth++
		if depth > s.limits.MaxChainDepth {
			return 0, fmt.Errorf("%w: 超过 %d 轮", ErrChainTooLong, s.limits.MaxChainDepth)
		}
		cur = t.PrevID
	}
	return depth, nil
}

// touchChainLocked 沿链续期。
func (s *MemoryStore) touchChainLocked(id string, now time.Time) {
	deadline := now.Add(s.limits.TTL)
	cur := id
	for i := 0; cur != "" && i <= s.limits.MaxChainDepth; i++ {
		e, ok := s.turns[cur]
		if !ok {
			return
		}
		if e.expireAt.Before(deadline) {
			e.expireAt = deadline
		}
		cur = e.turn.PrevID
	}
}

// deleteSubtreeLocked 删除一轮及其全部后继。
//
// 必须级联：留下一个指向已删除前驱的孤儿轮，会让 History 返回 ErrNotFound，
// 客户端却拿着一个看起来有效的 ID——那比直接删掉更难排查。
func (s *MemoryStore) deleteSubtreeLocked(id string) {
	stack := []string{id}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		e, ok := s.turns[cur]
		if !ok {
			continue
		}
		stack = append(stack, s.children[cur]...)
		delete(s.children, cur)
		delete(s.turns, cur)

		// 从父节点的子列表里摘掉自己，避免父节点残留悬空引用。
		if p := e.turn.PrevID; p != "" {
			kids := s.children[p]
			for i, k := range kids {
				if k == cur {
					s.children[p] = append(kids[:i], kids[i+1:]...)
					break
				}
			}
		}
	}
}

// newID 生成响应 ID。
//
// 用密码学随机数而不是自增计数：会话 ID 会出现在客户端手里，可猜测的 ID
// 意味着别人能拿到你的对话历史。
func newID() (string, error) {
	var b [18]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("convstore: 生成会话 ID 失败: %w", err)
	}
	return "resp_" + base64.RawURLEncoding.EncodeToString(b[:]), nil
}
