package config

import (
	"strings"
	"testing"
)

// fullGateway 返回一份配齐的网关配置。
func fullGateway() Config {
	c := Default()
	c.Auth = Auth{Keys: []AuthKey{{ID: "default", Key: "omugw-key-0123456789"}}}
	c.Credentials = map[string][]CredentialSpec{
		"openai": {{ID: "primary", Secret: "sk-test"}},
	}
	c.Providers = []ProviderSpec{{
		Endpoint:       "openai",
		Kind:           "openai.compat",
		BaseURL:        "https://api.openai.com",
		CredentialPool: "openai",
	}}
	c.Models = []ModelSpec{{
		Match:   "gpt-5",
		Targets: []TargetSpec{{Endpoint: "openai", UpstreamModel: "gpt-5"}},
	}}
	return c
}

func TestFullGatewayConfigIsValid(t *testing.T) {
	if err := fullGateway().Validate(); err != nil {
		t.Fatalf("配齐的网关配置应当合法: %v", err)
	}
}

// TestInfraOnlyConfigIsValid 覆盖「只提供健康检查」的合法形态。
// 这正是 M0 的现状：声明层建好了，上游还没配。
func TestInfraOnlyConfigIsValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("不配网关部分应当合法（仅健康检查模式）: %v", err)
	}
}

// TestPartialGatewayConfigIsRejected 是这一节最重要的一条。
//
// 一份写了 providers 却漏了 models 的配置，作者显然是想让网关工作的。
// 静默降级成健康检查模式，只会让人对着一个「一直返回 404」的网关查半天。
func TestPartialGatewayConfigIsRejected(t *testing.T) {
	tests := map[string]func(*Config){
		"只有 auth":       func(c *Config) { c.Credentials, c.Providers, c.Models = nil, nil, nil },
		"缺 models":      func(c *Config) { c.Models = nil },
		"缺 providers":   func(c *Config) { c.Providers = nil },
		"缺 credentials": func(c *Config) { c.Credentials = nil },
		"缺 auth":        func(c *Config) { c.Auth = Auth{} },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			c := fullGateway()
			mutate(&c)

			err := c.Validate()
			if err == nil {
				t.Fatal("配了一半的网关配置应当被拒绝")
			}
			if !strings.Contains(err.Error(), "不完整") {
				t.Errorf("错误应说明配置不完整，实际: %v", err)
			}
		})
	}
}

// TestDanglingReferencesAreCaughtAtStartup 固化「交叉引用在启动时炸掉」。
//
// 一个指向不存在 endpoint 的模型规则，在启动时是一行配置错误，
// 在运行时是一个语焉不详的 500。
func TestDanglingReferencesAreCaughtAtStartup(t *testing.T) {
	t.Run("模型引用不存在的 endpoint", func(t *testing.T) {
		c := fullGateway()
		c.Models[0].Targets[0].Endpoint = "typo-endpoint"

		err := c.Validate()
		if err == nil {
			t.Fatal("应当拒绝")
		}
		// 错误里要列出可用值，否则用户只能靠猜。
		if !strings.Contains(err.Error(), "openai") {
			t.Errorf("错误应列出可用的 endpoint，实际: %v", err)
		}
	})

	t.Run("provider 引用不存在的凭据池", func(t *testing.T) {
		c := fullGateway()
		c.Providers[0].CredentialPool = "nonexistent"

		if err := c.Validate(); err == nil {
			t.Fatal("应当拒绝")
		}
	})
}

// TestAuthIsMandatory 固化一条安全底线。
// 一个不鉴权的 AI 网关等于把上游凭据免费送人。
func TestAuthIsMandatory(t *testing.T) {
	c := fullGateway()
	c.Auth.Keys = nil

	if err := c.Validate(); err == nil {
		t.Fatal("网关必须鉴权")
	}
}

// TestShortAuthKeyIsRejected 防止「password」这类密钥。
// 网关背后是真金白银的上游额度，短密钥可以被暴力枚举。
func TestShortAuthKeyIsRejected(t *testing.T) {
	c := fullGateway()
	c.Auth.Keys[0].Key = "short"

	err := c.Validate()
	if err == nil {
		t.Fatal("过短的密钥应当被拒绝")
	}
	if !strings.Contains(err.Error(), "16") {
		t.Errorf("错误应说明长度要求，实际: %v", err)
	}
}

func TestDuplicateIDsAreRejected(t *testing.T) {
	t.Run("重复的 auth key id", func(t *testing.T) {
		c := fullGateway()
		c.Auth.Keys = append(c.Auth.Keys, AuthKey{ID: "default", Key: "another-long-key-here"})
		if err := c.Validate(); err == nil {
			t.Fatal("应当拒绝")
		}
	})

	t.Run("重复的凭据 id", func(t *testing.T) {
		c := fullGateway()
		c.Credentials["openai"] = append(c.Credentials["openai"],
			CredentialSpec{ID: "primary", Secret: "sk-other"})
		if err := c.Validate(); err == nil {
			t.Fatal("应当拒绝")
		}
	})

	t.Run("重复的 provider endpoint", func(t *testing.T) {
		c := fullGateway()
		c.Providers = append(c.Providers, c.Providers[0])
		if err := c.Validate(); err == nil {
			t.Fatal("应当拒绝")
		}
	})
}

// TestConvStoreDefaultsToDisabled 固化 ADR 里那条默认值。
//
// 内存态会话在多副本部署下是错的，而这是个开源项目，用户很可能那样部署，
// 然后撞上「会话时有时无」——最难查的一类 bug。
func TestConvStoreDefaultsToDisabled(t *testing.T) {
	if Default().ConvStore.Enabled {
		t.Error("会话存储必须默认关闭")
	}
}

func TestConvStoreValidationOnlyAppliesWhenEnabled(t *testing.T) {
	c := fullGateway()
	c.ConvStore = ConvStore{Enabled: false} // 全零值

	if err := c.Validate(); err != nil {
		t.Errorf("关闭时不该校验其余字段: %v", err)
	}

	c.ConvStore.Enabled = true
	if err := c.Validate(); err == nil {
		t.Error("开启时零值配置应当被拒绝")
	}
}
