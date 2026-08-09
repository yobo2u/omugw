package convstore

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/canonical"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestStore(t *testing.T) (*MemoryStore, *fakeClock) {
	t.Helper()
	clk := &fakeClock{t: time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)}
	return NewMemoryStore(DefaultLimits(), clk.Now), clk
}

func turn(user, assistant string) []canonical.Message {
	return []canonical.Message{
		canonical.UserText(user),
		canonical.AssistantText(assistant),
	}
}

func TestAppendAndHistory(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	first, err := s.Append(ctx, "", turn("你好", "你好，有什么可以帮你"), "qwen-max")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Append(ctx, first, turn("上海天气", "今天多云"), "qwen-max")
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.History(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("历史应有 4 条消息，实际 %d 条", len(got))
	}
	// 顺序必须是时间正序——回放给上游时顺序错了，模型看到的是一段颠倒的对话。
	if got[0].TextContent() != "你好" || got[3].TextContent() != "今天多云" {
		t.Errorf("历史顺序有误: %v", []string{
			got[0].TextContent(), got[1].TextContent(),
			got[2].TextContent(), got[3].TextContent(),
		})
	}
}

// TestUnknownPrevIDFailsLoudly 固化一处刻意的严格。
//
// 引用一个不存在的前驱时静默当成新会话开始，客户端会以为上下文还在，
// 实际上模型收到的是一段没头没尾的对话——答非所问，却完全无从排查。
func TestUnknownPrevIDFailsLoudly(t *testing.T) {
	s, _ := newTestStore(t)

	_, err := s.Append(context.Background(), "resp_nonexistent", turn("a", "b"), "m")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("引用不存在的前驱应返回 ErrNotFound，实际 %v", err)
	}
}

// TestForkSharesPrefix 覆盖 Responses 的合法用法：从同一个响应分叉出两条对话。
// 链式存储让两条分支共享前缀，而不是各复制一份。
func TestForkSharesPrefix(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	root, err := s.Append(ctx, "", turn("共同前缀", "好的"), "m")
	if err != nil {
		t.Fatal(err)
	}
	branchA, err := s.Append(ctx, root, turn("分支甲", "回甲"), "m")
	if err != nil {
		t.Fatal(err)
	}
	branchB, err := s.Append(ctx, root, turn("分支乙", "回乙"), "m")
	if err != nil {
		t.Fatal(err)
	}

	hа, err := s.History(ctx, branchA)
	if err != nil {
		t.Fatal(err)
	}
	hb, err := s.History(ctx, branchB)
	if err != nil {
		t.Fatal(err)
	}

	if hа[0].TextContent() != "共同前缀" || hb[0].TextContent() != "共同前缀" {
		t.Error("两条分支应共享前缀")
	}
	if hа[2].TextContent() != "分支甲" || hb[2].TextContent() != "分支乙" {
		t.Error("两条分支的后续应各自独立")
	}
	// 只存了 3 轮，不是 4 轮——前缀没有被复制。
	if s.Len() != 3 {
		t.Errorf("应存储 3 轮（前缀共享），实际 %d 轮", s.Len())
	}
}

func TestExpiry(t *testing.T) {
	s, clk := newTestStore(t)
	ctx := context.Background()

	id, err := s.Append(ctx, "", turn("a", "b"), "m")
	if err != nil {
		t.Fatal(err)
	}

	clk.Advance(DefaultLimits().TTL + time.Minute)

	if _, err := s.History(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("过期后应返回 ErrNotFound，实际 %v", err)
	}
}

// TestTouchExtendsOnlyActiveBranch 验证续期只沿被实际使用的分支进行，
// 让废弃的分叉自然过期，而不是被一次无关的访问拖着一起续命。
func TestTouchExtendsOnlyActiveBranch(t *testing.T) {
	s, clk := newTestStore(t)
	ctx := context.Background()
	ttl := DefaultLimits().TTL

	root, _ := s.Append(ctx, "", turn("root", "ok"), "m")
	active, _ := s.Append(ctx, root, turn("活跃", "ok"), "m")
	abandoned, _ := s.Append(ctx, root, turn("废弃", "ok"), "m")

	// 在过期前访问活跃分支，续期。
	clk.Advance(ttl - time.Minute)
	if _, err := s.History(ctx, active); err != nil {
		t.Fatal(err)
	}

	// 再走过原本的过期点：活跃分支还在，废弃分支已过期。
	clk.Advance(2 * time.Minute)
	if _, err := s.History(ctx, active); err != nil {
		t.Errorf("活跃分支应已续期，实际 %v", err)
	}
	if _, err := s.History(ctx, abandoned); !errors.Is(err, ErrNotFound) {
		t.Errorf("废弃分支应已过期，实际 %v", err)
	}
}

// TestChainDepthLimit 固化「上限必须存在」。没有它，一个跑了三天的 agent
// 循环就能把网关内存吃光——不需要恶意。
func TestChainDepthLimit(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxChainDepth = 3
	s := NewMemoryStore(limits, nil)
	ctx := context.Background()

	id := ""
	for i := 0; i < 3; i++ {
		var err error
		id, err = s.Append(ctx, id, turn("q", "a"), "m")
		if err != nil {
			t.Fatalf("第 %d 轮不该失败: %v", i+1, err)
		}
	}

	_, err := s.Append(ctx, id, turn("q", "a"), "m")
	if !errors.Is(err, ErrChainTooLong) {
		t.Fatalf("超过深度上限应返回 ErrChainTooLong，实际 %v", err)
	}
}

// TestMessageLimitRejectsRatherThanTruncates 固化「超限拒绝而不是驱逐」。
// 静默丢掉中间几轮，模型会收到一段缺了上下文的对话——比直接报错难查得多。
func TestMessageLimitRejectsRatherThanTruncates(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxMessages = 5
	s := NewMemoryStore(limits, nil)
	ctx := context.Background()

	id, _ := s.Append(ctx, "", turn("q1", "a1"), "m")
	id, _ = s.Append(ctx, id, turn("q2", "a2"), "m")
	id, _ = s.Append(ctx, id, turn("q3", "a3"), "m") // 累计 6 条 > 5

	got, err := s.History(ctx, id)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("超过消息上限应返回 ErrTooLarge，实际 err=%v len=%d", err, len(got))
	}
	if got != nil {
		t.Error("超限时不得返回被截断的历史")
	}
}

// TestDeleteCascades 保证删除会带走后继。留下指向已删前驱的孤儿轮，
// 客户端会拿着一个看起来有效的 ID 反复撞 ErrNotFound。
func TestDeleteCascades(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	root, _ := s.Append(ctx, "", turn("a", "b"), "m")
	child, _ := s.Append(ctx, root, turn("c", "d"), "m")
	grand, _ := s.Append(ctx, child, turn("e", "f"), "m")

	if err := s.Delete(ctx, root); err != nil {
		t.Fatal(err)
	}
	for name, id := range map[string]string{"root": root, "child": child, "grand": grand} {
		if _, err := s.Turn(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s 应已被级联删除，实际 %v", name, err)
		}
	}
	if s.Len() != 0 {
		t.Errorf("级联删除后应为空，实际 %d 轮", s.Len())
	}
}

// TestGCReclaimsUnreachable 覆盖「没人再访问的链，惰性过期永远碰不到」。
func TestGCReclaimsUnreachable(t *testing.T) {
	s, clk := newTestStore(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if _, err := s.Append(ctx, "", turn("q", "a"), "m"); err != nil {
			t.Fatal(err)
		}
	}
	if s.Len() != 5 {
		t.Fatalf("应有 5 轮，实际 %d", s.Len())
	}

	clk.Advance(DefaultLimits().TTL + time.Second)
	if n := s.GC(); n != 5 {
		t.Errorf("GC 应回收 5 轮，实际 %d", n)
	}
	if s.Len() != 0 {
		t.Errorf("GC 后应为空，实际 %d 轮", s.Len())
	}
}

// TestIDsAreUnguessable 保证会话 ID 不可猜测。
// 会话 ID 会出现在客户端手里，可预测的 ID 意味着别人能读到你的对话历史。
func TestIDsAreUnguessable(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		id, err := newID()
		if err != nil {
			t.Fatal(err)
		}
		if seen[id] {
			t.Fatalf("ID 重复: %s", id)
		}
		seen[id] = true
		if !strings.HasPrefix(id, "resp_") {
			t.Errorf("ID 应带 resp_ 前缀: %s", id)
		}
		if len(id) < 24 {
			t.Errorf("ID 太短，容易被枚举: %s", id)
		}
	}
}

// TestConcurrentForkIsSafe 覆盖真实用法：客户端从同一个 previous_response_id
// 并发分叉出多轮对话。
func TestConcurrentForkIsSafe(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	root, err := s.Append(ctx, "", turn("root", "ok"), "m")
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := s.Append(ctx, root, turn("并发", "回"), "m")
			if err != nil {
				errs <- err
				return
			}
			if _, err := s.History(ctx, id); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("并发分叉失败: %v", err)
	}
	if s.Len() != 51 {
		t.Errorf("应有 51 轮（1 根 + 50 分支），实际 %d", s.Len())
	}
}

func TestAppendRejectsEmptyTurn(t *testing.T) {
	s, _ := newTestStore(t)
	if _, err := s.Append(context.Background(), "", nil, "m"); err == nil {
		t.Fatal("空的一轮应当被拒绝")
	}
}

func TestAppendValidatesMessages(t *testing.T) {
	s, _ := newTestStore(t)
	bad := []canonical.Message{{Role: canonical.RoleUser}} // 没有 parts
	if _, err := s.Append(context.Background(), "", bad, "m"); err == nil {
		t.Fatal("非法消息应当被拒绝，而不是存进去等回放时才炸")
	}
}
