package credential

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/canonical"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newPool(t *testing.T, creds ...Credential) (*Pool, *clock) {
	t.Helper()
	clk := &clock{t: time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)}
	p, err := NewPool("openai", creds, DefaultPolicy(), clk.Now)
	if err != nil {
		t.Fatal(err)
	}
	return p, clk
}

func key(id string) Credential { return Credential{ID: id, Secret: "sk-" + id} }

// TestSecretNeverAppearsInOutput 是这个包的第一道防线。
//
// Credential 会被塞进日志字段、错误消息、调试打印。只要有一处默认格式化
// 输出了 Secret，密钥就进了日志系统——而日志系统的访问控制通常比密钥管理宽松。
func TestSecretNeverAppearsInOutput(t *testing.T) {
	c := Credential{ID: "primary", Provider: "openai", Secret: "sk-live-must-not-leak"}

	for _, s := range []string{
		c.String(),
		fmt.Sprintf("%v", c),
		fmt.Sprintf("%s", c),
	} {
		if strings.Contains(s, "sk-live-must-not-leak") {
			t.Errorf("密钥泄露到格式化输出: %s", s)
		}
	}
	if !strings.Contains(c.String(), "primary") {
		t.Error("应保留 ID，否则日志失去排障价值")
	}
}

// TestStatsExcludeSecret 覆盖可观测通道。
func TestStatsExcludeSecret(t *testing.T) {
	p, _ := newPool(t, key("a"))
	for _, s := range p.Stats() {
		if strings.Contains(fmt.Sprintf("%+v", s), "sk-") {
			t.Errorf("状态快照泄露密钥: %+v", s)
		}
	}
}

func TestWeightedRoundRobin(t *testing.T) {
	a := key("a")
	a.Weight = 3
	b := key("b")
	b.Weight = 1

	p, _ := newPool(t, a, b)

	counts := map[string]int{}
	for i := 0; i < 40; i++ {
		l, err := p.Acquire(nil)
		if err != nil {
			t.Fatal(err)
		}
		counts[l.Credential.ID]++
		l.Succeed()
	}

	// 3:1 的权重，允许一点误差，但方向必须对。
	if counts["a"] <= counts["b"]*2 {
		t.Errorf("加权轮询未生效: a=%d b=%d（权重 3:1）", counts["a"], counts["b"])
	}
}

// TestPriorityTiers 固化「高优先级用尽才降级」。
// 典型用法是先用便宜的额度，用完再用贵的。
func TestPriorityTiers(t *testing.T) {
	cheap := key("cheap")
	cheap.Priority = 0
	expensive := key("expensive")
	expensive.Priority = 10

	p, clk := newPool(t, expensive, cheap) // 故意乱序传入

	l, err := p.Acquire(nil)
	if err != nil {
		t.Fatal(err)
	}
	if l.Credential.ID != "cheap" {
		t.Fatalf("应先用低优先级数值的凭据，实际 %q", l.Credential.ID)
	}
	l.Fail(canonical.Newf(canonical.ClassRateLimit, "限流"))

	// 便宜的进冷却后才轮到贵的。
	clk.Advance(time.Millisecond)
	l2, err := p.Acquire(nil)
	if err != nil {
		t.Fatal(err)
	}
	if l2.Credential.ID != "expensive" {
		t.Errorf("高优先级冷却后应降级，实际 %q", l2.Credential.ID)
	}
}

// TestNonRetryableErrorDoesNotCooldown 是这个包最重要的一条规则。
//
// Retryable 的定义是「换一个凭据可能成功」。上下文超长、内容被拦截、参数非法
// ——这些换哪份凭据都一样失败，罚凭据毫无道理。真罚了的话，一个连发超长请求
// 的客户端就能把整个池子打成不健康，殃及所有其他调用方。
func TestNonRetryableErrorDoesNotCooldown(t *testing.T) {
	p, _ := newPool(t, key("only"))

	for i := 0; i < 10; i++ {
		l, err := p.Acquire(nil)
		if err != nil {
			t.Fatalf("第 %d 次借用失败: %v", i+1, err)
		}
		l.Fail(canonical.Newf(canonical.ClassContextLength, "上下文超长"))
	}

	if n := p.AvailableCount(); n != 1 {
		t.Errorf("不可重试的失败不该冷却凭据，可用数 = %d", n)
	}
	if s := p.Stats()[0]; s.ConsecutiveFails != 0 {
		t.Errorf("不可重试的失败不该计入连续失败，实际 %d", s.ConsecutiveFails)
	}
}

// TestRetryAfterBeatsLocalBackoff 固化「上游说了就听上游的」。
// 上游明说什么时候可以再来，比任何本地退避算法都准。
func TestRetryAfterBeatsLocalBackoff(t *testing.T) {
	p, clk := newPool(t, key("a"))

	l, _ := p.Acquire(nil)
	err := canonical.Newf(canonical.ClassRateLimit, "限流")
	err.RetryAfter = 90 * time.Second
	l.Fail(err)

	if p.AvailableCount() != 0 {
		t.Fatal("限流后应进入冷却")
	}

	clk.Advance(89 * time.Second)
	if p.AvailableCount() != 0 {
		t.Error("Retry-After 未到就恢复了，会立刻再撞一次限流")
	}
	clk.Advance(2 * time.Second)
	if p.AvailableCount() != 1 {
		t.Error("Retry-After 已过应当恢复")
	}
}

// TestAuthFailureGetsLongCooldown 固化「错的 Key 不会自己变对」。
func TestAuthFailureGetsLongCooldown(t *testing.T) {
	p, clk := newPool(t, key("a"))

	l, _ := p.Acquire(nil)
	l.Fail(canonical.Newf(canonical.ClassAuth, "invalid api key"))

	clk.Advance(DefaultPolicy().MaxCooldown + time.Minute)
	if p.AvailableCount() != 0 {
		t.Error("鉴权失败的冷却应显著长于普通退避——反复拿错的 Key 去试只是刷错误率")
	}

	clk.Advance(DefaultPolicy().AuthCooldown)
	if p.AvailableCount() != 1 {
		t.Error("鉴权冷却期满后应恢复，配置更新可能已经修好了")
	}
}

// TestTransientFailureBacksOffGently 保证一次偶发抖动不会把凭据打入冷宫。
func TestTransientFailureBacksOffGently(t *testing.T) {
	p, clk := newPool(t, key("a"))

	l, _ := p.Acquire(nil)
	l.Fail(canonical.Newf(canonical.ClassUpstreamUnavailable, "502"))

	// 首次失败只做很短的退避。
	clk.Advance(DefaultPolicy().BaseCooldown)
	if p.AvailableCount() != 1 {
		t.Error("单次抖动后应很快恢复")
	}
}

// TestRepeatedFailuresEscalate 覆盖上一条的反面：真坏了要退得越来越久。
func TestRepeatedFailuresEscalate(t *testing.T) {
	p, clk := newPool(t, key("a"))

	var last time.Duration
	for i := 0; i < 5; i++ {
		l, err := p.Acquire(nil)
		if err != nil {
			// 冷却中，推进时间再来。
			clk.Advance(DefaultPolicy().MaxCooldown)
			continue
		}
		l.Fail(canonical.Newf(canonical.ClassUpstreamUnavailable, "502"))

		s := p.Stats()[0]
		d := s.CooldownUntil.Sub(clk.Now())
		if i > 0 && d < last {
			t.Errorf("第 %d 次失败的冷却 %v 短于上一次 %v，退避没有升级", i+1, d, last)
		}
		last = d
		clk.Advance(d + time.Millisecond)
	}
}

func TestSuccessResetsBackoff(t *testing.T) {
	p, clk := newPool(t, key("a"))

	l, _ := p.Acquire(nil)
	l.Fail(canonical.Newf(canonical.ClassUpstreamUnavailable, "502"))
	clk.Advance(time.Hour)

	l2, _ := p.Acquire(nil)
	l2.Succeed()

	if s := p.Stats()[0]; s.ConsecutiveFails != 0 || !s.CooldownUntil.IsZero() {
		t.Errorf("成功后应清除失败状态，实际 %+v", s)
	}
}

// TestExhaustedPoolReportsEarliestRecovery 固化「Retry-After 要有依据」。
//
// 拍脑袋的常量要么让客户端白等，要么让它提前来撞墙。
func TestExhaustedPoolReportsEarliestRecovery(t *testing.T) {
	p, _ := newPool(t, key("a"), key("b"))

	for _, d := range []time.Duration{60 * time.Second, 20 * time.Second} {
		l, err := p.Acquire(nil)
		if err != nil {
			t.Fatal(err)
		}
		e := canonical.Newf(canonical.ClassRateLimit, "限流")
		e.RetryAfter = d
		l.Fail(e)
	}

	_, err := p.Acquire(nil)
	if err == nil {
		t.Fatal("全部冷却时应当报错")
	}

	var cerr *canonical.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("应返回 *canonical.Error，实际 %T", err)
	}
	// 应当报较早的那个，而不是较晚的——报晚了客户端白等 40 秒。
	if cerr.RetryAfter < 19*time.Second || cerr.RetryAfter > 21*time.Second {
		t.Errorf("Retry-After = %v, 期望约 20s（最早恢复的那份凭据）", cerr.RetryAfter)
	}
	// 池子空了，换凭据已无意义，留给上层去换 Provider。
	if cerr.Retryable {
		t.Error("凭据全部冷却时不应再标记为可重试")
	}
}

// TestExcludeSkipsTriedCredentials 覆盖同一请求内的 failover。
func TestExcludeSkipsTriedCredentials(t *testing.T) {
	p, _ := newPool(t, key("a"), key("b"))

	l1, err := p.Acquire(nil)
	if err != nil {
		t.Fatal(err)
	}
	tried := map[string]bool{l1.Credential.ID: true}

	l2, err := p.Acquire(tried)
	if err != nil {
		t.Fatal(err)
	}
	if l2.Credential.ID == l1.Credential.ID {
		t.Error("failover 时不该重复借出同一份凭据")
	}

	tried[l2.Credential.ID] = true
	if _, err := p.Acquire(tried); err == nil {
		t.Error("两份都试过了，应当报无可用凭据")
	}
}

// TestLeaseIsIdempotent 保证重复归还不会重复计数。
func TestLeaseIsIdempotent(t *testing.T) {
	p, _ := newPool(t, key("a"))

	l, _ := p.Acquire(nil)
	l.Fail(canonical.Newf(canonical.ClassUpstreamUnavailable, "502"))
	l.Fail(canonical.Newf(canonical.ClassUpstreamUnavailable, "502"))
	l.Succeed()

	if s := p.Stats()[0]; s.ConsecutiveFails != 1 {
		t.Errorf("重复归还不该重复计数，连续失败 = %d, 期望 1", s.ConsecutiveFails)
	}
}

func TestNewPoolRejectsBadConfig(t *testing.T) {
	tests := map[string][]Credential{
		"空池":    {},
		"缺少 ID": {{Secret: "sk-x"}},
		"缺少密钥":  {{ID: "a"}},
		"重复 ID": {{ID: "a", Secret: "sk-1"}, {ID: "a", Secret: "sk-2"}},
	}
	for name, creds := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := NewPool("openai", creds, DefaultPolicy(), nil); err == nil {
				t.Error("应当拒绝这份配置")
			}
		})
	}
}

func TestConcurrentAcquireIsSafe(t *testing.T) {
	p, _ := newPool(t, key("a"), key("b"), key("c"))

	var wg sync.WaitGroup
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := p.Acquire(nil)
			if err != nil {
				return
			}
			l.Succeed()
		}()
	}
	wg.Wait()

	var total int
	for _, s := range p.Stats() {
		total += s.Picks
	}
	if total != 60 {
		t.Errorf("借出次数合计 = %d, 期望 60（说明有竞态丢失计数）", total)
	}
}
