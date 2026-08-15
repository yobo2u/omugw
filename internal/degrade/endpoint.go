package degrade

import "github.com/yobo2u/omugw/internal/protocol/dashscopenative"

// Endpoint 是入站协议族下的一扇门：客户端能直接敲响的上游端点路径。
// 同源直通下，入站门与出站端点是同一个路径。
//
// 零值没有任何含义：不是通配，不是默认门。它在任何处出现都按「没有这扇门」
// 对待——Redeem 它会在 Build 失败，Check 它会在端点闸门得 501，
// Preservation 它的可用列恒为零。
type Endpoint string

// Inbound 钉住一次请求的入站坐标：从哪个协议进来，敲的哪扇门。
// 矩阵按这对坐标裁决。
type Inbound struct {
	Protocol Protocol
	Endpoint Endpoint
}

const (
	// OpenAI 两扇门从 build.go 的字面量迁到命名常量，
	// 登记（rules）与注册（Mux）引用同一份事实。
	EndpointOpenAIChat      Endpoint = "/v1/chat/completions"
	EndpointOpenAIResponses Endpoint = "/v1/responses"
)

const (
	// DashScope 两扇门复用协议包的路径常量，线格式事实的单一来源留在协议包，
	// 矩阵引用而不复写。
	EndpointDashScopeTextGeneration Endpoint = Endpoint(dashscopenative.TextGenerationPath)
	EndpointDashScopeMultimodal     Endpoint = Endpoint(dashscopenative.MultimodalGenerationPath)
)

// Protocol 报告这扇门归哪个入站协议所有，第二个返回值说明这门是否已知。
//
// 门不是一段自由字符串，它带着线格式：/v1/responses 收的是 Responses 请求，
// 由 Responses 解码器把守。把它兑现在 openai.chat 路径上，矩阵就会按 chat 的
// 可表达性去裁决一个 Responses 请求——错绑在运行时只表现为语焉不详的字段丢失，
// 所以归属要在 Build 就能问出来。
//
// 只覆盖四扇已知门，不是准入名单：未知门报「不知道」而放行，测试用的合成门与
// 还没提成常量的新端点都照常可以兑现。猜一个归属比不知道更糟——那会把一扇还没
// 设计的门焊死在某个协议上。
//
// 就地 switch 而不是全局注册表：四个常量都在本文件里，注册表只会多出
// 「谁先注册」的初始化顺序问题，换不来任何东西。
func (e Endpoint) Protocol() (Protocol, bool) {
	switch e {
	case EndpointOpenAIChat:
		return ProtoOpenAIChat, true
	case EndpointOpenAIResponses:
		return ProtoOpenAIResponses, true
	case EndpointDashScopeTextGeneration, EndpointDashScopeMultimodal:
		return ProtoDashScopeNative, true
	default:
		return "", false
	}
}
