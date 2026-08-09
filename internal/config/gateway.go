package config

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Auth 是网关自身的鉴权配置。
type Auth struct {
	Keys []AuthKey `yaml:"keys"`
}

// AuthKey 是一把网关 API Key。
type AuthKey struct {
	// ID 出现在日志与指标里，用于区分调用方。**不得由 Key 派生**——
	// 一个从密钥哈希出来的 ID 看着无害，却让持有日志的人能验证密钥猜测。
	ID string `yaml:"id"`

	// Key 是密钥本身，绝不进日志。
	Key string `yaml:"key"`
}

// CredentialSpec 是一份上游凭据。
type CredentialSpec struct {
	ID       string `yaml:"id"`
	Secret   string `yaml:"secret"`
	Weight   int    `yaml:"weight"`
	Priority int    `yaml:"priority"`
}

// ProviderSpec 是一个上游端点。
type ProviderSpec struct {
	// Endpoint 是这个上游的稳定标识。
	Endpoint string `yaml:"endpoint"`

	// Kind 是协议族，用于查降级矩阵。
	//
	// 与 Endpoint 分开是必须的：OpenAI、DeepSeek、Kimi 都是 openai.compat，
	// 矩阵看它们完全一样，而路由必须分得清该发去哪一个。
	Kind string `yaml:"kind"`

	BaseURL string `yaml:"base_url"`

	// CredentialPool 指向 credentials 下的哪一组。
	CredentialPool string `yaml:"credential_pool"`
}

// ModelSpec 是一条模型路由规则。
type ModelSpec struct {
	// Match 支持三种显式形态：精确、"前缀*"、"*" 兜底。
	// 不支持中缀通配——匹配顺序不可预期意味着请求会去到谁也说不清的地方。
	Match   string       `yaml:"match"`
	Targets []TargetSpec `yaml:"targets"`
}

// TargetSpec 是规则下的一个候选上游。
type TargetSpec struct {
	Endpoint      string `yaml:"endpoint"`
	UpstreamModel string `yaml:"upstream_model"`
}

// ConvStore 是网关侧会话存储的配置。
type ConvStore struct {
	// Enabled 默认 false。
	//
	// 内存态会话在多副本部署下是错的：请求被负载均衡到另一个副本就找不到会话。
	// 这是个开源项目，用户很可能那样部署，然后撞上「会话时有时无」——
	// 最难查的一类 bug。默认关闭，想用的人显式承担代价。
	Enabled bool `yaml:"enabled"`

	TTL           time.Duration `yaml:"ttl"`
	MaxChainDepth int           `yaml:"max_chain_depth"`
	MaxMessages   int           `yaml:"max_messages"`
	GCInterval    time.Duration `yaml:"gc_interval"`
}

// DefaultConvStore 返回默认配置：关闭。
func DefaultConvStore() ConvStore {
	return ConvStore{
		Enabled:       false,
		TTL:           2 * time.Hour,
		MaxChainDepth: 200,
		MaxMessages:   1000,
		GCInterval:    5 * time.Minute,
	}
}

// validateGateway 做网关配置的交叉引用校验。
//
// 这些错误必须在启动时炸掉，而不是等第一个请求打进来：一个指向不存在
// endpoint 的模型规则，在启动时是一行配置错误，在运行时是一个语焉不详的 500。
func (c Config) validateGateway() error {
	if err := c.validateAuth(); err != nil {
		return err
	}

	pools := map[string]bool{}
	for name, creds := range c.Credentials {
		if len(creds) == 0 {
			return fmt.Errorf("config: 凭据池 %q 为空", name)
		}
		seen := map[string]bool{}
		for i, cr := range creds {
			if cr.ID == "" {
				return fmt.Errorf("config: 凭据池 %q 的第 %d 份凭据缺少 id", name, i)
			}
			if seen[cr.ID] {
				return fmt.Errorf("config: 凭据池 %q 存在重复的凭据 id %q", name, cr.ID)
			}
			seen[cr.ID] = true
			if cr.Secret == "" {
				return fmt.Errorf("config: 凭据 %s/%s 缺少 secret", name, cr.ID)
			}
		}
		pools[name] = true
	}

	endpoints := map[string]ProviderSpec{}
	for i, p := range c.Providers {
		switch {
		case p.Endpoint == "":
			return fmt.Errorf("config: providers[%d] 缺少 endpoint", i)
		case p.Kind == "":
			return fmt.Errorf("config: provider %q 缺少 kind", p.Endpoint)
		case p.BaseURL == "":
			return fmt.Errorf("config: provider %q 缺少 base_url", p.Endpoint)
		case p.CredentialPool == "":
			return fmt.Errorf("config: provider %q 缺少 credential_pool", p.Endpoint)
		}
		if _, dup := endpoints[p.Endpoint]; dup {
			return fmt.Errorf("config: 重复的 provider endpoint %q", p.Endpoint)
		}
		if !pools[p.CredentialPool] {
			return fmt.Errorf("config: provider %q 引用了不存在的凭据池 %q（可用: %s）",
				p.Endpoint, p.CredentialPool, keysOf(pools))
		}
		endpoints[p.Endpoint] = p
	}

	if len(c.Models) == 0 {
		return fmt.Errorf("config: 没有配置任何模型路由，网关无法处理请求")
	}
	for i, m := range c.Models {
		if m.Match == "" {
			return fmt.Errorf("config: models[%d] 缺少 match", i)
		}
		if len(m.Targets) == 0 {
			return fmt.Errorf("config: 模型规则 %q 没有候选上游", m.Match)
		}
		for j, t := range m.Targets {
			if t.Endpoint == "" {
				return fmt.Errorf("config: 规则 %q 的候选 %d 缺少 endpoint", m.Match, j)
			}
			if _, ok := endpoints[t.Endpoint]; !ok {
				return fmt.Errorf("config: 规则 %q 引用了不存在的 endpoint %q（可用: %s）",
					m.Match, t.Endpoint, keysOfProviders(endpoints))
			}
			if t.UpstreamModel == "" {
				return fmt.Errorf("config: 规则 %q 的候选 %q 缺少 upstream_model", m.Match, t.Endpoint)
			}
		}
	}

	return c.ConvStore.validate()
}

func (c Config) validateAuth() error {
	if len(c.Auth.Keys) == 0 {
		// 一个不鉴权的 AI 网关等于把上游凭据免费送人。宁可启动失败。
		return fmt.Errorf("config: auth.keys 为空——网关必须鉴权，否则等于把上游凭据公开")
	}
	seen := map[string]bool{}
	for i, k := range c.Auth.Keys {
		if k.ID == "" {
			return fmt.Errorf("config: auth.keys[%d] 缺少 id", i)
		}
		if seen[k.ID] {
			return fmt.Errorf("config: 重复的 auth key id %q", k.ID)
		}
		seen[k.ID] = true
		if k.Key == "" {
			return fmt.Errorf("config: auth key %q 缺少 key", k.ID)
		}
		if len(k.Key) < 16 {
			// 短密钥可以被暴力枚举，而网关背后是真金白银的上游额度。
			return fmt.Errorf("config: auth key %q 长度不足 16 字符", k.ID)
		}
	}
	return nil
}

func (s ConvStore) validate() error {
	if !s.Enabled {
		return nil
	}
	if s.TTL <= 0 {
		return fmt.Errorf("config: convstore.ttl 必须为正数")
	}
	if s.MaxChainDepth <= 0 {
		return fmt.Errorf("config: convstore.max_chain_depth 必须为正数")
	}
	if s.MaxMessages <= 0 {
		return fmt.Errorf("config: convstore.max_messages 必须为正数")
	}
	if s.GCInterval <= 0 {
		return fmt.Errorf("config: convstore.gc_interval 必须为正数")
	}
	return nil
}

func keysOf(m map[string]bool) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return "无"
	}
	return strings.Join(out, ", ")
}

func keysOfProviders(m map[string]ProviderSpec) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return "无"
	}
	return strings.Join(out, ", ")
}
