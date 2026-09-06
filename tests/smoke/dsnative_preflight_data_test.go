//go:build smoke

package smoke_test

import (
	"fmt"
	"strings"

	"github.com/yobo2u/omugw/internal/testkit"
)

// ============================================================================
// 出站映射预检
//
// 本节把 11 份客户端请求体逐一送进真实网关（gateway.Build），上游换成本地假
// 源站，捕获出站字节后断言关键映射。**这只是映射预检，不是 ADR-0001 意义上的
// 举证**：假源站的响应是手写的，它既不能证明某个真实模型支持某项能力，也不
// 构成转正所需的 fixture。它防的是另一件事——一份在编码期就会被拒的请求体，
// 或一处早已漂移的字段映射，不该等到花着钱打真实上游时才发现。
//
// 假响应一律不落盘。
// ============================================================================

// nativeJSONResponse 按候选数产出一份足够走完非流式转换的 Native 响应。
//
// 候选数必须跟着请求走：n=2 的请求配一份单候选响应，转换照样成功，
// 「n 有没有生效」这个问题就永远得不到回答。
func nativeJSONResponse(candidates int) string {
	choices := make([]string, 0, candidates)
	for i := range candidates {
		choices = append(choices, fmt.Sprintf(
			`{"finish_reason":"stop","message":{"role":"assistant","content":"预检候选 %d"}}`, i))
	}
	return `{"output":{"choices":[` + strings.Join(choices, ",") +
		`]},"usage":{"input_tokens":11,"output_tokens":7},"request_id":"req-preflight-json"}`
}

// nativeSSEEvents 按候选数产出一条完整的 Native 事件流。
//
// 每个候选都必须在末帧拿到 finish_reason：translator 要全部候选 finish 才收尾，
// 漏掉一个，转换器会一直等下去，最后以「流在候选完成前结束」失败。
func nativeSSEEvents(candidates int) []testkit.SSEEvent {
	frame := func(content, finish string) testkit.SSEEvent {
		choices := make([]string, 0, candidates)
		for range candidates {
			choices = append(choices, fmt.Sprintf(
				`{"finish_reason":%q,"message":{"role":"assistant","content":%q}}`, finish, content))
		}
		return testkit.SSEEvent{
			Event: "result",
			Data: `{"output":{"choices":[` + strings.Join(choices, ",") +
				`]},"usage":{"input_tokens":11,"output_tokens":7},"request_id":"req-preflight-sse"}`,
		}
	}
	return []testkit.SSEEvent{frame("预检", ""), frame("增量", "stop")}
}

// nativeToolSSEEvents 产出一条把工具参数切成两帧的 Native 事件流。
//
// 分帧是刻意的：translator 对 arguments 片段原样续发、不校验合法性，
// 一条帧内参数就已闭合的流跑不到那段跨帧拼接的代码。
func nativeToolSSEEvents() []testkit.SSEEvent {
	return []testkit.SSEEvent{
		{
			Event: "result",
			Data: `{"output":{"choices":[{"finish_reason":"","message":{"role":"assistant",` +
				`"content":"","tool_calls":[{"id":"call_preflight_1","type":"function",` +
				`"function":{"name":"get_weather","arguments":"{\"city\":\"杭"}}]}}]},` +
				`"usage":{"input_tokens":21,"output_tokens":3},"request_id":"req-preflight-tool"}`,
		},
		{
			Event: "result",
			Data: `{"output":{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant",` +
				`"content":"","tool_calls":[{"id":"","type":"function",` +
				`"function":{"name":"","arguments":"州\"}"}}]}}]},` +
				`"usage":{"input_tokens":21,"output_tokens":9},"request_id":"req-preflight-tool"}`,
		},
	}
}

// nativeReasoningSSEEvents 产出一条带思考内容的 Native 事件流。
//
// 单独一份而不是复用通用流：reasoning 用例要证的是 reasoning_content 落到了
// 客户端，而通用流一个思考字节都没有——拿它跑，那条断言永远不会被真正执行。
func nativeReasoningSSEEvents() []testkit.SSEEvent {
	frame := func(reasoning, content, finish string) testkit.SSEEvent {
		return testkit.SSEEvent{
			Event: "result",
			Data: fmt.Sprintf(`{"output":{"choices":[{"finish_reason":%q,`+
				`"message":{"role":"assistant","reasoning_content":%q,"content":%q}}]},`+
				`"usage":{"input_tokens":13,"output_tokens":9},"request_id":"req-preflight-reasoning"}`,
				finish, reasoning, content),
		}
	}
	return []testkit.SSEEvent{frame("先设未知数", "", ""), frame("", "鸡 3 只，兔 5 只。", "stop")}
}
