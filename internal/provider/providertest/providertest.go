// Package providertest 是出站适配器的共享契约套件。
//
// 每个适配器的测试包填一份 Subject 声明自己的差异，由本包统一断言全部不变量。
// 契约集中在这里而不是散落在各包的测试函数里，才有单一位置能回答
// 「一个出站适配器必须满足什么」——否则漏掉哪一条谁都不知道。
package providertest

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/provider"
	"github.com/yobo2u/omugw/internal/transport/httpx"
)

// Subject 是一个待测适配器的声明。
//
// 用具名结构体而不是位置参数：将来加第七、第八项声明时，不必回到每个调用点
// 数位置。理由与 dashscopecompat 测试里 callInput 的收敛一致。
type Subject struct {
	// Name 出现在子测试名里，便于定位是哪个适配器失败。
	Name string

	// Kind 是该适配器应当返回的协议族。
	Kind degrade.Provider

	// New 构造适配器。套件按传输族注入不同的依赖。
	New func(t *testing.T, deps Deps) provider.Provider

	// InboundProtocol 是契约里这个适配器服务的入站协议。
	//
	// 请求带的是入站坐标而不是裸路径，坐标缺了协议这一维就不是坐标了：
	// 半填的 Inbound 会让适配器看到一个现实中不存在的「无协议的门」。
	// 与 Kind（出站族）成对，一个说从哪进来，一个说往哪出去。
	InboundProtocol degrade.Protocol

	// DefaultPath 是请求未带 Inbound.Endpoint 时应落到的端点。
	DefaultPath string

	// ForwardedHeaders 是允许原样带给上游的客户端头。
	//
	// 零值即最严：留空表示一个客户端头都不许转发。要放宽必须显式列出来——
	// 默认宽松会让「忘记声明」和「确实不转发」不可区分。
	ForwardedHeaders []string

	// StreamHeaders 是流式时除 Accept 之外必须落位的额外头。
	// DashScope Native 把「是否流式」放在 X-DashScope-SSE 上，丢了它
	// 上游会按非流式返回，整条流的语义就变了。
	StreamHeaders map[string]string

	// ValidBody 是该协议下一份合法的请求体，含 model 字段。
	//
	// 各协议请求体形状不同（Native 是 input.messages 嵌套，Responses 是顶层
	// input，Chat 是顶层 messages），所以由 Subject 自带而非套件内置常量。
	ValidBody string

	// RateLimitEnvelope 是上游 429 的错误信封样例。
	// 各协议族信封不同形（DashScope 扁平 {code,message}，OpenAI 系嵌套），
	// 套件只断言解码结果，不关心信封长什么样。
	RateLimitEnvelope string
}

// Deps 是套件注入给适配器构造函数的依赖。
//
// 存在这一层间接而不是直接传 *httpx.Client：套件要控制 httptest 服务器与
// 固定时钟，且将来 WebSocket driver 接入时只扩展 Deps，不改任何调用点。
type Deps struct {
	HTTPClient *httpx.Client
	Now        func() time.Time
}

// validate 检查必填项。返回的错误列出全部缺失项，而不是遇到第一个就返回——
// 让填写的人一次看到所有要补的东西。
func validate(s Subject) error {
	var missing []string
	if s.Kind == "" {
		missing = append(missing, "Kind")
	}
	if s.New == nil {
		missing = append(missing, "New")
	}
	// 漏填入站协议不会让任何一条不变量变红：套件照跑，只是每次调用都递给适配器
	// 一个半填的 Inbound。那种坐标现实中不存在，于是整轮契约是在一个假坐标上过的。
	if s.InboundProtocol == "" {
		missing = append(missing, "InboundProtocol")
	}
	if s.ValidBody == "" {
		missing = append(missing, "ValidBody")
	}
	if s.RateLimitEnvelope == "" {
		missing = append(missing, "RateLimitEnvelope")
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("Subject %q 缺少必填项: %s", s.Name, strings.Join(missing, ", "))
}

// Run 跑全部不变量族。
//
// 必填项缺失时立即失败并列出缺哪些，而不是跳过对应的族——跳过等于把「没跑」
// 伪装成「跑过了」，而消除那种隐形缺口正是这个套件存在的理由。
func Run(t *testing.T, s Subject) {
	t.Helper()

	if err := validate(s); err != nil {
		t.Fatal(err)
	}

	name := s.Name
	if name == "" {
		name = string(s.Kind)
	}

	t.Run(name, func(t *testing.T) {
		runTransportAgnostic(t, s)
		runHTTPContract(t, s)
	})
}
