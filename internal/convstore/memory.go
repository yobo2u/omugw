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
	// totalBytes 是当前保存的全部轮次的负载合计，随写入与删除同步增减。
	// 每次写入重新遍历整个 map 统计会让写入退化成 O(n)。
	totalBytes int64
}

type entry struct {
	turn     Turn
	expireAt time.Time
	// bytes 是这一轮计入全局预算的字节数，删除时按它回退。
	// 不重算而是记下来：Messages 在回收时可能已被改写，重算会算出另一个数，
	// 让 totalBytes 缓慢漂移直到把 Store 卡死在一个虚高的水位上。
	bytes int64
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
func (s *MemoryStore) Append(ctx context.Context, prevID string, msgs []canonical.Message, model string) (string, error) {
	id, err := NewResponseID()
	if err != nil {
		return "", err
	}
	if err := s.AppendWithID(ctx, Turn{
		ID: id, PrevID: prevID, Messages: msgs, Model: model,
	}); err != nil {
		return "", err
	}
	return id, nil
}

// AppendWithID 追加调用方已经对外公布 response.id 的一轮。
func (s *MemoryStore) AppendWithID(_ context.Context, turn Turn) error {
	if turn.ID == "" {
		return fmt.Errorf("convstore: response id 不得为空")
	}
	if len(turn.Messages) == 0 && len(turn.Opaque) == 0 {
		return fmt.Errorf("convstore: 不能追加空的一轮")
	}
	if turn.InlineBytes < 0 {
		return fmt.Errorf("convstore: 内联字节数不得为负数")
	}
	for i, m := range turn.Messages {
		if err := m.Validate(); err != nil {
			return fmt.Errorf("convstore: messages[%d]: %w", i, err)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	if _, exists := s.turns[turn.ID]; exists {
		return fmt.Errorf("convstore: response id 已存在")
	}

	// 先回收过期条目再判全局额度：不清理就拒绝，会让一个早已没人访问、
	// 只是还没被 GC 扫到的 Store 提前对所有写入关门。
	s.collectExpiredLocked(now)

	size := turnBytes(turn)
	if s.limits.MaxTurns > 0 && len(s.turns)+1 > s.limits.MaxTurns {
		return fmt.Errorf("%w: 已保存 %d 轮，全局上限 %d",
			ErrTooLarge, len(s.turns), s.limits.MaxTurns)
	}
	if s.limits.MaxTotalBytes > 0 && s.totalBytes+size > s.limits.MaxTotalBytes {
		return fmt.Errorf("%w: 累计 %d 字节，全局上限 %d",
			ErrTooLarge, s.totalBytes+size, s.limits.MaxTotalBytes)
	}

	depth := 1
	totalMessages := len(turn.Messages)
	for cur := turn.PrevID; cur != ""; {
		prev, ok := s.liveLocked(cur, now)
		if !ok || prev.Owner != turn.Owner {
			return ErrNotFound
		}
		depth++
		if depth > s.limits.MaxChainDepth {
			return fmt.Errorf("%w: 已达 %d 轮上限", ErrChainTooLong, s.limits.MaxChainDepth)
		}
		totalMessages += len(prev.Messages)
		if totalMessages > s.limits.MaxMessages {
			return fmt.Errorf("%w: %d 条，上限 %d", ErrTooLarge,
				totalMessages, s.limits.MaxMessages)
		}
		cur = prev.PrevID
	}
	if totalMessages > s.limits.MaxMessages {
		return fmt.Errorf("%w: %d 条，上限 %d", ErrTooLarge,
			totalMessages, s.limits.MaxMessages)
	}
	if turn.PrevID != "" {
		// 沿链续期。只延长被实际使用的那条分支，让废弃的分叉自然过期。
		s.touchChainLocked(turn.PrevID, now)
	}

	s.turns[turn.ID] = &entry{
		turn: Turn{
			ID:          turn.ID,
			PrevID:      turn.PrevID,
			Owner:       turn.Owner,
			Messages:    cloneMessages(turn.Messages),
			Opaque:      append([]byte(nil), turn.Opaque...),
			InlineBytes: turn.InlineBytes,
			Model:       turn.Model,
			CreatedAt:   now,
		},
		expireAt: now.Add(s.limits.TTL),
		bytes:    size,
	}
	s.totalBytes += size
	if turn.PrevID != "" {
		s.children[turn.PrevID] = append(s.children[turn.PrevID], turn.ID)
	}
	return nil
}

// History 回溯出完整历史，按时间正序。
func (s *MemoryStore) History(ctx context.Context, id string) ([]canonical.Message, error) {
	turns, err := s.Turns(ctx, id)
	if err != nil {
		return nil, err
	}
	total := 0
	for _, turn := range turns {
		total += len(turn.Messages)
	}
	out := make([]canonical.Message, 0, total)
	for _, turn := range turns {
		out = append(out, turn.Messages...)
	}
	return out, nil
}

// Turns 回溯出完整轮次链，按时间正序。
func (s *MemoryStore) Turns(_ context.Context, id string) ([]Turn, error) {
	return s.turnsForOwner(id, "", false)
}

// TurnsOwned 只返回指定调用方自己的会话链。
func (s *MemoryStore) TurnsOwned(_ context.Context, id, owner string) ([]Turn, error) {
	return s.turnsForOwner(id, owner, true)
}

func (s *MemoryStore) turnsForOwner(id, owner string, enforceOwner bool) ([]Turn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()

	// 先逆序收集，再整体反转。逐轮 prepend 在长会话上是 O(n²)。
	var chain []*entry
	cur := id
	for cur != "" {
		e, ok := s.liveLocked(cur, now)
		if !ok || enforceOwner && e.Owner != owner {
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

	out := make([]Turn, 0, len(chain))
	for i := len(chain) - 1; i >= 0; i-- {
		turn := chain[i].turn
		turn.Messages = cloneMessages(turn.Messages)
		turn.Opaque = append([]byte(nil), turn.Opaque...)
		out = append(out, turn)
	}

	s.touchChainLocked(id, now)
	return out, nil
}

// Limits 返回构造 Store 时固定下来的资源上限。
func (s *MemoryStore) Limits() Limits { return s.limits }

// Turn 取出单独一轮。
func (s *MemoryStore) Turn(_ context.Context, id string) (Turn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.liveLocked(id, s.now())
	if !ok {
		return Turn{}, ErrNotFound
	}
	out := *t
	out.Messages = cloneMessages(t.Messages)
	out.Opaque = append([]byte(nil), t.Opaque...)
	return out, nil
}

// cloneMessages 在 Store 的输入输出边界复制全部可变负载。
// 只复制 Message 或 Part 外层切片仍会留下 Data、JSON 与嵌套内容块的别名，
// 调用方随后改自己的对象就能绕过 Store 的锁改写历史。
func cloneMessages(src []canonical.Message) []canonical.Message {
	if src == nil {
		return nil
	}
	out := make([]canonical.Message, len(src))
	for i, msg := range src {
		out[i] = msg
		out[i].Parts = cloneParts(msg.Parts)
	}
	return out
}

func cloneParts(src []canonical.Part) []canonical.Part {
	if src == nil {
		return nil
	}
	out := make([]canonical.Part, len(src))
	for i, part := range src {
		out[i] = part
		if part.Thinking != nil {
			v := *part.Thinking
			out[i].Thinking = &v
		}
		if part.Media != nil {
			v := *part.Media
			v.Data = append([]byte(nil), part.Media.Data...)
			if part.Media.FileRef != nil {
				ref := *part.Media.FileRef
				v.FileRef = &ref
			}
			if part.Media.Audio != nil {
				audio := *part.Media.Audio
				v.Audio = &audio
			}
			out[i].Media = &v
		}
		if part.ToolCall != nil {
			v := *part.ToolCall
			v.Arguments = append([]byte(nil), part.ToolCall.Arguments...)
			out[i].ToolCall = &v
		}
		if part.ToolResult != nil {
			v := *part.ToolResult
			v.Content = cloneParts(part.ToolResult.Content)
			out[i].ToolResult = &v
		}
	}
	return out
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

	return s.collectExpiredLocked(s.now())
}

// collectExpiredLocked 清掉已过期的条目，返回清理数量。
func (s *MemoryStore) collectExpiredLocked(now time.Time) int {
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

// turnBytes 估算一轮计入全局预算的负载大小。
//
// 把文本也算进来，而不只是 InlineBytes：纯文本的 InlineBytes 恒为 0，只按
// 媒体记账的话，成批的大段文本请求会绕过整个字节闸门。
func turnBytes(turn Turn) int64 {
	total := turn.InlineBytes + int64(len(turn.Opaque)) + int64(len(turn.Model))
	for _, m := range turn.Messages {
		total += int64(len(m.Role))
		total += partsBytes(m.Parts)
	}
	return total
}

func partsBytes(parts []canonical.Part) int64 {
	var total int64
	for _, p := range parts {
		total += int64(len(p.Text))
		if p.Thinking != nil {
			total += int64(len(p.Thinking.Text)) + int64(len(p.Thinking.Signature))
		}
		if p.Media != nil {
			total += int64(len(p.Media.Data)) + int64(len(p.Media.URL))
		}
		if p.ToolCall != nil {
			total += int64(len(p.ToolCall.Arguments)) + int64(len(p.ToolCall.Name))
		}
		if p.ToolResult != nil {
			total += partsBytes(p.ToolResult.Content)
		}
	}
	return total
}

// Len 返回当前保存的轮数，供观测使用。
func (s *MemoryStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.turns)
}

// RunGC 按固定间隔清理无人再访问的过期分支，直到进程上下文结束。
func (s *MemoryStore) RunGC(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.GC()
		}
	}
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
		s.totalBytes -= e.bytes

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
