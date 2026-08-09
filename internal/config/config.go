// Package config 负责配置加载与校验。
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config 是网关的全部配置。
type Config struct {
	Server   Server   `yaml:"server"`
	Timeouts Timeouts `yaml:"timeouts"`
	Log      Log      `yaml:"log"`
	Limits   Limits   `yaml:"limits"`

	Auth        Auth                        `yaml:"auth"`
	Credentials map[string][]CredentialSpec `yaml:"credentials"`
	Providers   []ProviderSpec              `yaml:"providers"`
	Models      []ModelSpec                 `yaml:"models"`
	ConvStore   ConvStore                   `yaml:"convstore"`
}

// Server 是监听相关配置。
type Server struct {
	Addr string `yaml:"addr"`
	// MetricsAddr 与 Addr 分开监听：metrics 端点不应暴露在对外的网关端口上。
	MetricsAddr string `yaml:"metrics_addr"`
}

// Timeouts 是四层超时（原则 2.7）。
//
// 分四层不是过度设计。只有一个总超时的话，一个思考 3 分钟的推理请求和一个
// 挂死的连接长得一模一样——要么把前者误杀，要么让后者拖住连接池。
type Timeouts struct {
	// Connect 是建立 TCP + TLS 连接的上限。
	Connect time.Duration `yaml:"connect"`

	// FirstByte 是发出请求到收到第一个响应字节的上限。
	//
	// 这一层是流式 failover 的边界：超过它还没有首字节，可以安全地换凭据重试；
	// 一旦首字节到达，无论后面发生什么都不得重试（原则 2.4）。
	FirstByte time.Duration `yaml:"first_byte"`

	// Total 是整个请求的上限，含流式传输全程。
	Total time.Duration `yaml:"total"`

	// Idle 是流式传输中两个数据块之间的最大间隔。
	// 它才是「上游挂死」的真正判据——Total 到期只说明响应很长。
	Idle time.Duration `yaml:"idle"`
}

// Log 是日志配置。
type Log struct {
	Level  string `yaml:"level"`  // debug | info | warn | error
	Format string `yaml:"format"` // json | text
}

// Limits 是资源上限。
type Limits struct {
	// MaxInlineBytes 是单个请求中内联（base64）多模态负载的总大小上限。
	//
	// 上限必须存在（原则 2.6）：没有它，一个塞满 base64 视频的请求就能把网关
	// 的内存吃光。URL 形态不受此限——那种情况下字节根本不经过网关。
	MaxInlineBytes int64 `yaml:"max_inline_bytes"`

	// MaxRequestBytes 是请求体总大小上限。
	MaxRequestBytes int64 `yaml:"max_request_bytes"`
}

// Default 返回默认配置。
func Default() Config {
	return Config{
		Server: Server{
			Addr:        ":8080",
			MetricsAddr: "127.0.0.1:9090",
		},
		Timeouts: Timeouts{
			Connect:   10 * time.Second,
			FirstByte: 120 * time.Second, // 推理模型的首字节可能很慢
			Total:     30 * time.Minute,  // 长流式生成
			Idle:      60 * time.Second,
		},
		Log: Log{Level: "info", Format: "json"},
		Limits: Limits{
			MaxInlineBytes:  20 << 20, // 20 MiB
			MaxRequestBytes: 32 << 20, // 32 MiB
		},
		ConvStore: DefaultConvStore(),
	}
}

// Load 从 YAML 文件读取配置，缺失的字段用默认值补齐。
//
// 字符串值中的 ${VAR} 会被展开成环境变量，这样 API Key 之类的东西可以留在
// 环境里而不必写进配置文件——配置文件是会被提交进 git 的。
func Load(path string) (Config, error) {
	cfg := Default()

	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("config: 读取 %s 失败: %w", path, err)
	}

	// 先解析成节点树再展开，而不是在原始文本上做字符串替换。
	//
	// 后者看起来更简单，但会把注释里的 ${...} 也当成变量引用——一份带说明性
	// 注释的示例配置会直接启动失败。展开必须发生在 YAML 结构已知之后，
	// 且只作用于字符串标量。
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return Config{}, fmt.Errorf("config: 解析 %s 失败: %w", path, err)
	}

	// 空文件解析出零值节点，此时全部采用默认值。
	if root.Kind == 0 {
		if err := cfg.Validate(); err != nil {
			return Config{}, err
		}
		return cfg, nil
	}

	var missing []string
	expandNode(&root, &missing)
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("config: 配置引用了未定义的环境变量: %s",
			strings.Join(missing, ", "))
	}

	if err := root.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("config: 解析 %s 失败: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// expandNode 递归展开字符串标量中的 ${VAR}。
//
// 未定义的变量记入 missing 而不是展开成空串：一个空的 API Key 会在运行时变成
// 难以定位的 401，在启动时失败要好得多。
func expandNode(n *yaml.Node, missing *[]string) {
	if n == nil {
		return
	}
	// 只碰字符串标量。数字、布尔、null 不做展开，避免把 "$" 开头的字面量弄坏。
	if n.Kind == yaml.ScalarNode && n.Tag == "!!str" {
		n.Value = os.Expand(n.Value, func(name string) string {
			v, ok := os.LookupEnv(name)
			if !ok {
				*missing = append(*missing, name)
				return ""
			}
			return v
		})
		return
	}
	for _, c := range n.Content {
		expandNode(c, missing)
	}
}

// Validate 校验配置的自洽性。
func (c Config) Validate() error {
	if c.Server.Addr == "" {
		return fmt.Errorf("config: server.addr 不得为空")
	}
	if err := c.Timeouts.Validate(); err != nil {
		return err
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("config: 未知的 log.level %q", c.Log.Level)
	}
	switch c.Log.Format {
	case "json", "text":
	default:
		return fmt.Errorf("config: 未知的 log.format %q", c.Log.Format)
	}
	if c.Limits.MaxInlineBytes <= 0 {
		return fmt.Errorf("config: limits.max_inline_bytes 必须为正数")
	}
	if c.Limits.MaxRequestBytes < c.Limits.MaxInlineBytes {
		return fmt.Errorf("config: limits.max_request_bytes (%d) 小于 max_inline_bytes (%d)，"+
			"内联负载永远无法通过",
			c.Limits.MaxRequestBytes, c.Limits.MaxInlineBytes)
	}

	// 网关部分要么完整配置，要么完全不配。
	//
	// 「完全不配」是合法的：那是只提供健康检查的基础设施模式，与 M0 的现状
	// 一致。而「配了一半」必须失败——一份写了 providers 却漏了 models 的配置，
	// 作者显然是想让网关工作的，静默降级成健康检查模式只会让人对着一个
	// 「一直返回 404」的网关查半天。
	if c.gatewayConfigured() {
		return c.validateGateway()
	}
	if c.partiallyConfigured() {
		return fmt.Errorf("config: 网关配置不完整——" +
			"auth.keys / credentials / providers / models 必须同时配齐，或全部留空（仅健康检查模式）")
	}
	return nil
}

// gatewayConfigured 报告网关部分是否配齐。
func (c Config) gatewayConfigured() bool {
	return len(c.Auth.Keys) > 0 && len(c.Credentials) > 0 &&
		len(c.Providers) > 0 && len(c.Models) > 0
}

// partiallyConfigured 报告网关部分是否配了一半。
func (c Config) partiallyConfigured() bool {
	set := 0
	for _, ok := range []bool{
		len(c.Auth.Keys) > 0, len(c.Credentials) > 0,
		len(c.Providers) > 0, len(c.Models) > 0,
	} {
		if ok {
			set++
		}
	}
	return set > 0 && set < 4
}

// Validate 校验四层超时的相对关系。
func (t Timeouts) Validate() error {
	for name, d := range map[string]time.Duration{
		"connect":    t.Connect,
		"first_byte": t.FirstByte,
		"total":      t.Total,
		"idle":       t.Idle,
	} {
		if d <= 0 {
			return fmt.Errorf("config: timeouts.%s 必须为正数", name)
		}
	}
	if t.Connect >= t.FirstByte {
		return fmt.Errorf("config: timeouts.connect (%v) 应小于 first_byte (%v)，"+
			"否则首字节超时永远不会触发", t.Connect, t.FirstByte)
	}
	if t.FirstByte > t.Total {
		return fmt.Errorf("config: timeouts.first_byte (%v) 不得大于 total (%v)，"+
			"否则流式 failover 的判定窗口失效", t.FirstByte, t.Total)
	}
	if t.Idle > t.Total {
		return fmt.Errorf("config: timeouts.idle (%v) 不得大于 total (%v)", t.Idle, t.Total)
	}
	return nil
}
