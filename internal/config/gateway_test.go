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

// TestBaseURLContractIsEnforced 固化 base_url 的契约：只允许「scheme + host +
// 可选路径前缀」。带 query 或 fragment 时，出站追加端点路径会被吞进 query 或
// fragment，请求打不到真实端点——启动看着成功，运行时才表现为 404 或鉴权失败。
func TestBaseURLContractIsEnforced(t *testing.T) {
	for name, baseURL := range map[string]string{
		"带 query":    "https://dashscope.aliyuncs.com/compatible-mode/v1?trace=1",
		"带 fragment": "https://dashscope.aliyuncs.com/compatible-mode/v1#frag",
		"缺少 scheme":  "dashscope.aliyuncs.com/compatible-mode/v1",
		"缺少 host":    "https:///compatible-mode/v1",
		"不是合法 URL":   "https://exa mple.com",
		"非 http 协议":  "ftp://dashscope.aliyuncs.com/v1",
	} {
		t.Run(name, func(t *testing.T) {
			c := fullGateway()
			c.Providers[0].BaseURL = baseURL

			err := c.Validate()
			if err == nil {
				t.Fatalf("base_url %q 应当在启动时被拒绝", baseURL)
			}
			if !strings.Contains(err.Error(), "openai") {
				t.Errorf("错误应点名出问题的 provider，实际: %v", err)
			}
		})
	}
}

// TestBaseURLAcceptsOfficialPathPrefix：官方 base_url 自带 /compatible-mode/v1
// 路径前缀，这是合法形态，不能被上面的契约误伤。
func TestBaseURLAcceptsOfficialPathPrefix(t *testing.T) {
	c := fullGateway()
	c.Providers[0].BaseURL = "https://dashscope.aliyuncs.com/compatible-mode/v1"

	if err := c.Validate(); err != nil {
		t.Fatalf("官方带路径前缀的 base_url 应当合法: %v", err)
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

// TestNativeEndpointValidation 钉死 native_endpoint 的三条启动期校验。
//
// 门是部署事实：漏声明会让 Provider 在运行时对着两个 Native 上游路径二选一，
// 只能靠模型名或本次请求是否含媒体去猜——猜错的表现是打到错误端点后返回一个
// 语焉不详的上游 400，而不是一行启动期配置错误。
func TestNativeEndpointValidation(t *testing.T) {
	// 构造一份最小合法配置：一个 dashscope.native provider + 一个指向它的 target。
	base := func(nativeEndpoint string) Config {
		c := fullGateway()
		c.Providers[0].Kind = "dashscope.native"
		c.Providers[0].BaseURL = "https://dashscope.aliyuncs.com"
		c.Models[0].Targets[0].NativeEndpoint = nativeEndpoint
		return c
	}

	t.Run("native kind 缺 native_endpoint 应失败", func(t *testing.T) {
		c := base("")
		if err := c.validateGateway(); err == nil {
			t.Fatal("dashscope.native target 必须声明 native_endpoint")
		}
	})
	t.Run("非 native kind 带 native_endpoint 应失败", func(t *testing.T) {
		c := base("text-generation")
		c.Providers[0].Kind = "openai.compat"
		if err := c.validateGateway(); err == nil {
			t.Fatal("非 dashscope.native target 不得声明 native_endpoint")
		}
	})
	t.Run("未知枚举应失败", func(t *testing.T) {
		c := base("embedding")
		if err := c.validateGateway(); err == nil {
			t.Fatal("未知 native_endpoint 枚举必须在启动期拒绝")
		}
	})
	t.Run("合法枚举应通过", func(t *testing.T) {
		c := base("text-generation")
		if err := c.validateGateway(); err != nil {
			t.Fatalf("text-generation 应合法: %v", err)
		}
	})
	t.Run("多模态门应通过", func(t *testing.T) {
		c := base("multimodal-generation")
		if err := c.validateGateway(); err != nil {
			t.Fatalf("multimodal-generation 应合法: %v", err)
		}
	})
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
