// Package credential 是上游凭据池。
//
// 它是 canonical.Error.Retryable 那条定义的第一个真实消费者。回顾一下：
// Retryable 的意思是「**换一个凭据或 Provider** 重试可能成功」，而不是
// 「上游临时故障」。于是这里的规则就直接了当——Retryable 为真就冷却当前凭据
// 并换下一个；为假则一概不冷却，因为那不是凭据的错。
//
// Core 只实现 API Key。OAuth 与订阅账号池由 omsub 承担：它们要处理令牌刷新、
// 封禁冷却与客户端指纹，这些逻辑一旦渗进 core，Apache-2.0 Clean Core 的
// 商业化叙事就被污染了。
package credential

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/yobo2u/omugw/internal/canonical"
)

// Credential 是一份上游凭据。
type Credential struct {
	// ID 是稳定标识，出现在日志与指标里。**绝不能由密钥派生**——
	// 一个从密钥哈希出来的 ID 看着无害，却让持有日志的人能验证密钥猜测。
	ID string

	Provider string

	// Secret 是密钥本身。它不参与任何 String/Error/日志输出。
	Secret string

	// Weight 是同优先级内的加权轮询权重，非正数视为 1。
	Weight int

	// Priority 越小越优先。只有高优先级全部不可用时才会用到低优先级——
	// 典型用法是「先用便宜的额度，用完再用贵的」。
	Priority int
}

// String 刻意不暴露密钥。
//
// 这不是洁癖：Credential 会被塞进日志字段、错误消息、调试打印，
// 只要有一处默认格式化输出了 Secret，密钥就进了日志系统。
func (c Credential) String() string {
	return fmt.Sprintf("credential(%s/%s)", c.Provider, c.ID)
}

// Policy 是冷却策略。
type Policy struct {
	// BaseCooldown 是没有 Retry-After 可依时的起始冷却时长。
	BaseCooldown time.Duration

	// MaxCooldown 是冷却时长上限。
	MaxCooldown time.Duration

	// AuthCooldown 是鉴权失败的冷却时长。
	//
	// 显著长于其他类别：一个错的 API Key 不会自己变对。反复拿它去试，
	// 除了刷错误率没有任何作用。
	AuthCooldown time.Duration

	// FailureThreshold 是连续失败多少次后判定为不健康。
	FailureThreshold int
}

// DefaultPolicy 返回一组保守的默认策略。
func DefaultPolicy() Policy {
	return Policy{
		BaseCooldown:     2 * time.Second,
		MaxCooldown:      5 * time.Minute,
		AuthCooldown:     15 * time.Minute,
		FailureThreshold: 3,
	}
}

type entry struct {
	cred Credential

	// cooldownUntil 之前不参与选取。
	cooldownUntil time.Time

	// consecutiveFails 只统计**可重试**的失败。不可重试的失败是请求本身的
	// 问题，与凭据健康无关——把它们计进来，一个发了一堆超长请求的客户端
	// 就能把整个凭据池打成不健康。
	consecutiveFails int

	// backoff 是当前的退避时长，成功后归零。
	backoff time.Duration

	// picks 是加权轮询的计数游标。
	picks int
}

func (e *entry) available(now time.Time) bool {
	return !now.Before(e.cooldownUntil)
}

// Pool 是单个 Provider 的凭据池。
type Pool struct {
	provider string
	policy   Policy
	now      func() time.Time

	mu      sync.Mutex
	entries []*entry
}

// NewPool 创建凭据池。now 可注入以便测试，传 nil 用 time.Now。
func NewPool(provider string, creds []Credential, p Policy, now func() time.Time) (*Pool, error) {
	if len(creds) == 0 {
		return nil, fmt.Errorf("credential: Provider %q 没有任何凭据", provider)
	}
	if now == nil {
		now = time.Now
	}

	seen := map[string]bool{}
	entries := make([]*entry, 0, len(creds))
	for i, c := range creds {
		if c.ID == "" {
			return nil, fmt.Errorf("credential: %s 的第 %d 份凭据缺少 ID", provider, i)
		}
		if seen[c.ID] {
			// 重复 ID 会让日志与指标里的两份凭据混成一份，冷却也会互相干扰。
			return nil, fmt.Errorf("credential: %s 存在重复的凭据 ID %q", provider, c.ID)
		}
		if c.Secret == "" {
			return nil, fmt.Errorf("credential: %s/%s 缺少密钥", provider, c.ID)
		}
		seen[c.ID] = true

		if c.Weight <= 0 {
			c.Weight = 1
		}
		c.Provider = provider
		entries = append(entries, &entry{cred: c})
	}

	// 按优先级升序排列，同优先级保持配置顺序——让选取结果可预期。
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].cred.Priority < entries[j].cred.Priority
	})

	return &Pool{provider: provider, policy: p, now: now, entries: entries}, nil
}

// Lease 是一次凭据借用。调用方必须以 Succeed 或 Fail 归还。
type Lease struct {
	Credential Credential

	pool *Pool
	e    *entry
	done bool
}

// Acquire 借出一份可用凭据。
//
// exclude 是本次请求已经试过的凭据 ID——failover 时传进来，避免在同一个请求里
// 反复撞同一份坏凭据。
func (p *Pool) Acquire(exclude map[string]bool) (*Lease, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := p.now()

	var (
		best     *entry
		bestPrio int
	)
	for _, e := range p.entries {
		if exclude[e.cred.ID] || !e.available(now) {
			continue
		}
		if best == nil || e.cred.Priority < bestPrio {
			best, bestPrio = e, e.cred.Priority
			continue
		}
		if e.cred.Priority != bestPrio {
			continue
		}
		// 同优先级内加权轮询：选「已用次数 / 权重」最小的那个。
		// 用比值而不是绝对次数，权重才真正起作用。
		if float64(e.picks)/float64(e.cred.Weight) <
			float64(best.picks)/float64(best.cred.Weight) {
			best = e
		}
	}

	if best == nil {
		return nil, p.exhaustedLocked(now, exclude)
	}

	best.picks++
	return &Lease{Credential: best.cred, pool: p, e: best}, nil
}

// exhaustedLocked 构造「无可用凭据」的错误。
//
// 带上最早恢复时间，这样客户端拿到的 Retry-After 是有依据的数字，而不是一个
// 拍脑袋的常量——后者要么让客户端白等，要么让它提前来撞墙。
func (p *Pool) exhaustedLocked(now time.Time, exclude map[string]bool) error {
	var earliest time.Time
	for _, e := range p.entries {
		if exclude[e.cred.ID] {
			continue
		}
		if earliest.IsZero() || e.cooldownUntil.Before(earliest) {
			earliest = e.cooldownUntil
		}
	}

	err := canonical.Newf(canonical.ClassRateLimit,
		"Provider %s 的凭据全部处于冷却中", p.provider)
	if !earliest.IsZero() && earliest.After(now) {
		err.RetryAfter = earliest.Sub(now)
	}
	// 换一份凭据也没用了——池子空了。留给上层去换 Provider。
	err.Retryable = false
	return err
}

// Succeed 归还凭据并清除失败计数。
func (l *Lease) Succeed() {
	if l.done {
		return
	}
	l.done = true

	l.pool.mu.Lock()
	defer l.pool.mu.Unlock()

	l.e.consecutiveFails = 0
	l.e.backoff = 0
	l.e.cooldownUntil = time.Time{}
}

// Fail 归还凭据并按错误分类决定是否冷却。
//
// **不可重试的错误一概不冷却。** 上下文超长、内容被拦截、参数非法——这些换
// 哪份凭据都一样失败，罚凭据毫无道理，而且会让一个发了一堆超长请求的客户端
// 把整个池子打成不健康。
func (l *Lease) Fail(err error) {
	if l.done {
		return
	}
	l.done = true

	cerr := canonical.AsError(err)
	if cerr == nil || !cerr.Retryable {
		return
	}

	l.pool.mu.Lock()
	defer l.pool.mu.Unlock()

	e := l.e
	e.consecutiveFails++

	l.pool.applyCooldownLocked(e, cerr)
}

func (p *Pool) applyCooldownLocked(e *entry, cerr *canonical.Error) {
	now := p.now()

	// 上游明说了什么时候可以再来，就听它的——比任何本地退避算法都准。
	if cerr.RetryAfter > 0 {
		d := cerr.RetryAfter
		if d > p.policy.MaxCooldown {
			d = p.policy.MaxCooldown
		}
		e.cooldownUntil = now.Add(d)
		e.backoff = d
		return
	}

	switch cerr.Class {
	case canonical.ClassAuth:
		// 一个错的 Key 不会自己变对，长冷却。
		e.cooldownUntil = now.Add(p.policy.AuthCooldown)
		e.backoff = p.policy.AuthCooldown
		return

	case canonical.ClassQuota:
		// 配额恢复通常以小时计，直接顶到上限，不做爬坡。
		e.cooldownUntil = now.Add(p.policy.MaxCooldown)
		e.backoff = p.policy.MaxCooldown
		return
	}

	// 其余可重试错误走指数退避。
	next := e.backoff * 2
	if next <= 0 {
		next = p.policy.BaseCooldown
	}
	if next > p.policy.MaxCooldown {
		next = p.policy.MaxCooldown
	}
	e.backoff = next

	// 连续失败未达阈值时只做很短的退避，避免一次偶发抖动就把凭据打入冷宫。
	if e.consecutiveFails < p.policy.FailureThreshold {
		e.cooldownUntil = now.Add(next / 4)
		return
	}
	e.cooldownUntil = now.Add(next)
}

// Stat 是一份凭据的可观测状态。**不含密钥。**
type Stat struct {
	ID               string
	Priority         int
	Weight           int
	Available        bool
	CooldownUntil    time.Time
	ConsecutiveFails int
	Picks            int
}

// Stats 返回池内全部凭据的状态快照。
func (p *Pool) Stats() []Stat {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := p.now()
	out := make([]Stat, 0, len(p.entries))
	for _, e := range p.entries {
		out = append(out, Stat{
			ID:               e.cred.ID,
			Priority:         e.cred.Priority,
			Weight:           e.cred.Weight,
			Available:        e.available(now),
			CooldownUntil:    e.cooldownUntil,
			ConsecutiveFails: e.consecutiveFails,
			Picks:            e.picks,
		})
	}
	return out
}

// AvailableCount 返回当前可用的凭据数，供指标使用。
func (p *Pool) AvailableCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := p.now()
	var n int
	for _, e := range p.entries {
		if e.available(now) {
			n++
		}
	}
	return n
}
