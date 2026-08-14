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
