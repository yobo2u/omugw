// Package router 把客户端请求的逻辑模型名解析成候选上游。
//
// 它与降级矩阵是分工关系，不是包含关系：路由回答「这个模型能去哪几个上游」，
// 矩阵回答「这几个上游里哪个最不丢语义、且真能承载本次请求的能力」。
// 两者合在一起会让「按能力选路」和「按配置选路」纠缠在一个函数里，
// 谁都测不清楚。
package router

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/degrade"
)

// Target 是一个候选上游。
type Target struct {
	// Kind 是协议族，用于查降级矩阵。
	//
	// 与 Endpoint 分开是必须的：OpenAI、DeepSeek、Kimi 都是 openai.compat，
	// 矩阵看它们完全一样（同一套线格式），而路由必须分得清该发去哪一个。
	// 只留一个字段，要么矩阵得为每个上游各存一份重复声明，要么路由无从选择。
	Kind degrade.Provider

	// Endpoint 是这个上游的稳定标识，出现在日志与指标里。
	Endpoint string

	// BaseURL 是上游根地址。
	BaseURL string

	// UpstreamModel 是上游真实模型名。客户端请求 "fast"，上游可能叫
	// "qwen-turbo"——这层改名正是网关的价值之一。
	UpstreamModel string

	// CredentialPool 指向 internal/credential 里的哪个池。
	CredentialPool string
}

// String 便于日志输出。
func (t Target) String() string {
	return fmt.Sprintf("%s/%s→%s", t.Kind, t.Endpoint, t.UpstreamModel)
}

// Rule 是一条路由规则。
type Rule struct {
	// Match 是匹配式。三种形态，且必须显式声明是哪一种：
	//
	//	"gpt-5"    精确匹配
	//	"qwen-*"   前缀匹配（最长前缀胜）
	//	"*"        兜底
	//
	// 不做隐式的模糊匹配。一个意外命中的通配规则，比一句明确的
	// 「未知模型」难查得多——前者会把请求发去一个谁也没想到的上游。
	Match string

	Targets []Target
}

// Router 解析逻辑模型名。
type Router struct {
	mu sync.RWMutex

	exact    map[string][]Target
	prefixes []prefixRule
	fallback []Target
}

type prefixRule struct {
	prefix  string
	targets []Target
}

// New 按规则构造路由器。
func New(rules []Rule) (*Router, error) {
	r := &Router{exact: map[string][]Target{}}

	for i, rule := range rules {
		if len(rule.Targets) == 0 {
			return nil, fmt.Errorf("router: 规则 %d (%q) 没有任何候选上游", i, rule.Match)
		}
		for j, t := range rule.Targets {
			if err := t.validate(); err != nil {
				return nil, fmt.Errorf("router: 规则 %q 的候选 %d: %w", rule.Match, j, err)
			}
		}

		switch {
		case rule.Match == "*":
			if r.fallback != nil {
				return nil, fmt.Errorf("router: 兜底规则只能有一条")
			}
			r.fallback = rule.Targets

		case strings.HasSuffix(rule.Match, "*"):
			p := strings.TrimSuffix(rule.Match, "*")
			if p == "" {
				return nil, fmt.Errorf("router: 前缀规则不能为空前缀，用 \"*\" 表示兜底")
			}
			r.prefixes = append(r.prefixes, prefixRule{prefix: p, targets: rule.Targets})

		case strings.Contains(rule.Match, "*"):
			// 只支持后缀通配。中缀通配的匹配顺序难以预期，而路由结果不可预期
			// 意味着请求会去到谁也说不清的地方。
			return nil, fmt.Errorf("router: 规则 %q 不支持中缀通配，只能是精确、前缀* 或 *", rule.Match)

		default:
			if _, dup := r.exact[rule.Match]; dup {
				return nil, fmt.Errorf("router: 模型 %q 有重复的精确规则", rule.Match)
			}
			r.exact[rule.Match] = rule.Targets
		}
	}

	// 最长前缀优先。不排序的话，"qwen-*" 和 "qwen-vl-*" 谁先命中取决于配置
	// 书写顺序——那是一种看不见的耦合。
	sort.SliceStable(r.prefixes, func(i, j int) bool {
		return len(r.prefixes[i].prefix) > len(r.prefixes[j].prefix)
	})

	if len(r.exact) == 0 && len(r.prefixes) == 0 && r.fallback == nil {
		return nil, fmt.Errorf("router: 没有任何路由规则")
	}
	return r, nil
}

func (t Target) validate() error {
	if t.Kind == "" {
		return fmt.Errorf("缺少协议族 kind")
	}
	if t.Endpoint == "" {
		return fmt.Errorf("缺少 endpoint 标识")
	}
	if t.BaseURL == "" {
		return fmt.Errorf("缺少 base_url")
	}
	if t.UpstreamModel == "" {
		return fmt.Errorf("缺少 upstream_model")
	}
	if t.CredentialPool == "" {
		return fmt.Errorf("缺少 credential_pool")
	}
	return nil
}

// Resolve 解析逻辑模型名，返回候选上游。
//
// 匹配顺序：精确 → 最长前缀 → 兜底。找不到时返回 bad_request 而不是 404——
// 从客户端视角这是「你要的模型我这儿没有」，属于请求问题。
//
// 错误消息刻意不列出可用模型：那等于把部署的模型清单暴露给任何一个能发请求的
// 人。想知道有什么模型，走 /v1/models 那条受鉴权保护的路。
func (r *Router) Resolve(model string) ([]Target, error) {
	if model == "" {
		return nil, canonical.Newf(canonical.ClassBadRequest, "缺少 model")
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	if t, ok := r.exact[model]; ok {
		return t, nil
	}
	for _, p := range r.prefixes {
		if strings.HasPrefix(model, p.prefix) {
			return p.targets, nil
		}
	}
	if r.fallback != nil {
		return r.fallback, nil
	}

	return nil, canonical.Newf(canonical.ClassBadRequest, "未知模型 %q", model)
}

// Kinds 抽出候选的协议族，去重后按降级矩阵能接受的形态返回。
//
// 同一个协议族可能对应多个 Endpoint（OpenAI 与 DeepSeek 都是 openai.compat），
// 矩阵只关心协议族，所以这里去重。
func Kinds(targets []Target) []degrade.Provider {
	seen := map[degrade.Provider]bool{}
	out := make([]degrade.Provider, 0, len(targets))
	for _, t := range targets {
		if seen[t.Kind] {
			continue
		}
		seen[t.Kind] = true
		out = append(out, t.Kind)
	}
	return out
}

// OfKind 从候选中筛出属于某个协议族的那些，保持原有顺序。
//
// 顺序即 failover 顺序：矩阵选出协议族之后，同族内按配置顺序逐个尝试。
func OfKind(targets []Target, kind degrade.Provider) []Target {
	out := make([]Target, 0, len(targets))
	for _, t := range targets {
		if t.Kind == kind {
			out = append(out, t)
		}
	}
	return out
}

// Models 返回全部显式配置的模型名，供 /v1/models 端点使用。
//
// 只列精确规则：前缀与兜底规则匹配的是无穷集合，列不出来，
// 硬要列就得编一份清单，那份清单与实际能用的模型必然对不上。
func (r *Router) Models() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]string, 0, len(r.exact))
	for m := range r.exact {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}
