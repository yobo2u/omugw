package dashscopenative

import (
	"errors"
	"reflect"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

// TestDecodeResultFinishReasonNulls 钉死 JSON null 与字符串 "null" 都归一为「生成中」。
//
// 官方非流式文档写 JSON null，流式示例却写字符串 "null"，两种形态同义。
// 只认其中一种的话，另一种会被当成一个「未知的结束原因」原样带下去——
// 下游据此判定流已结束，剩下的增量全部丢掉，响应照样 200。
func TestDecodeResultFinishReasonNulls(t *testing.T) {
	for _, body := range []string{
		`{"output":{"choices":[{"finish_reason":null,"message":{"role":"assistant","content":"a"}}]},"request_id":"r"}`,
		`{"output":{"choices":[{"finish_reason":"null","message":{"role":"assistant","content":"a"}}]},"request_id":"r"}`,
		`{"output":{"choices":[{"message":{"role":"assistant","content":"a"}}]},"request_id":"r"}`,
	} {
		res, err := DecodeResult([]byte(body))
		if err != nil {
			t.Fatalf("解码失败: %v\n负载: %s", err, body)
		}
		if len(res.Choices) != 1 {
			t.Fatalf("应解出一个候选: %+v\n负载: %s", res.Choices, body)
		}
		if res.Choices[0].FinishReason != "" {
			t.Errorf("null/\"null\"/缺省应归一为生成中，实际 %q\n负载: %s",
				res.Choices[0].FinishReason, body)
		}
		if res.Choices[0].Role != "assistant" || res.Choices[0].Content != "a" {
			t.Errorf("角色与文本应原样解出: %+v", res.Choices[0])
		}
	}
}

// TestDecodeResultFinishReasonPreserved 钉死三种真结束原因原样保留。
func TestDecodeResultFinishReasonPreserved(t *testing.T) {
	for _, want := range []string{"stop", "length", "tool_calls"} {
		body := `{"output":{"choices":[{"finish_reason":"` + want +
			`","message":{"role":"assistant","content":""}}]},"request_id":"r"}`
		res, err := DecodeResult([]byte(body))
		if err != nil {
			t.Fatalf("解码失败: %v", err)
		}
		if res.Choices[0].FinishReason != want {
			t.Errorf("finish_reason 应原样保留 %q，实际 %q", want, res.Choices[0].FinishReason)
		}
	}
}

// TestDecodeResultUsage 钉死 usage 映射与缺失 total_tokens 不伪造。
func TestDecodeResultUsage(t *testing.T) {
	res, err := DecodeResult([]byte(`{"output":{"choices":[{"finish_reason":"stop",
	  "message":{"role":"assistant","content":"ok"}}]},
	  "usage":{"input_tokens":5,"output_tokens":3},
	  "request_id":"r"}`))
	if err != nil {
		t.Fatal(err)
	}
	u := res.Usage
	if u == nil || u.Fidelity != canonical.FidelityAuthoritative {
		t.Fatalf("usage 应为 authoritative: %+v", u)
	}
	if u.InputTokens != 5 || u.OutputTokens != 3 {
		t.Errorf("token 映射错误: %+v", u)
	}
	if res.RequestID != "r" {
		t.Errorf("request_id 应解出: %q", res.RequestID)
	}
}

// TestDecodeResultToolCalls 钉死工具调用的 id/name/arguments 一一对应，
// 且未闭合的 arguments 片段照样解得出来。
//
// 流式帧会把 arguments 切断（这里的 `{"loc` 就是官方增量的真实形态），在这一层
// 校验 JSON 合法性会让整帧解码失败，一次本来能拼完整的工具调用变成上游故障。
func TestDecodeResultToolCalls(t *testing.T) {
	res, err := DecodeResult([]byte(`{"output":{"choices":[{"finish_reason":"tool_calls",
	  "message":{"role":"assistant","content":"","tool_calls":[
	    {"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"loc"}},
	    {"id":"call_2","type":"function","function":{"name":"get_time","arguments":"{}"}}]}}]},
	  "request_id":"r"}`))
	if err != nil {
		t.Fatalf("未闭合 arguments 片段不应导致解码失败: %v", err)
	}
	want := []ToolCall{
		{ID: "call_1", Name: "get_weather", Arguments: `{"loc`},
		{ID: "call_2", Name: "get_time", Arguments: `{}`},
	}
	got := res.Choices[0].ToolCalls
	if len(got) != len(want) {
		t.Fatalf("工具调用数量错误: %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("工具调用 %d 应为 %+v，实际 %+v", i, want[i], got[i])
		}
	}
}

// TestDecodeResultReasoningContent 钉死思考内容落在独立字段，不与正文混流。
//
// 混进 Content 的话，下游把思考过程当成回复正文发给客户端，模型的内部推理
// 就这样泄露出去，而响应仍是一个格式合法的 200。
func TestDecodeResultReasoningContent(t *testing.T) {
	res, err := DecodeResult([]byte(`{"output":{"choices":[{"finish_reason":"stop",
	  "message":{"role":"assistant","content":"答案","reasoning_content":"先想一想"}}]},
	  "request_id":"r"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Choices[0].ReasoningContent != "先想一想" {
		t.Errorf("reasoning_content 应解出: %q", res.Choices[0].ReasoningContent)
	}
	if res.Choices[0].Content != "答案" {
		t.Errorf("正文不应被思考内容污染: %q", res.Choices[0].Content)
	}

	absent, err := DecodeResult([]byte(`{"output":{"choices":[{"finish_reason":"stop",
	  "message":{"role":"assistant","content":"答案"}}]},"request_id":"r"}`))
	if err != nil {
		t.Fatal(err)
	}
	if absent.Choices[0].ReasoningContent != "" {
		t.Errorf("缺省时应为空串: %q", absent.Choices[0].ReasoningContent)
	}
}

// TestDecodeResultLogprobs 钉死 logprobs 原始字节原样保留，缺省为 nil。
//
// 本网关不解释这份数据，重新编解码一轮会丢掉字段顺序与上游新增的键，
// 客户端拿到的概率明细与上游给的对不上，却没有任何错误可看。
func TestDecodeResultLogprobs(t *testing.T) {
	const raw = `{"content":[{"token":"你","logprob":-0.5,"top_logprobs":[]}]}`
	res, err := DecodeResult([]byte(`{"output":{"choices":[{"finish_reason":"stop",
	  "message":{"role":"assistant","content":"你","logprobs":` + raw + `}}]},
	  "request_id":"r"}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Choices[0].Logprobs) != raw {
		t.Errorf("logprobs 应原样保留 %s，实际 %s", raw, res.Choices[0].Logprobs)
	}

	absent, err := DecodeResult([]byte(`{"output":{"choices":[{"finish_reason":"stop",
	  "message":{"role":"assistant","content":"你"}}]},"request_id":"r"}`))
	if err != nil {
		t.Fatal(err)
	}
	if absent.Choices[0].Logprobs != nil {
		t.Errorf("缺省时应为 nil，实际 %s", absent.Choices[0].Logprobs)
	}
}

// TestDecodeResultMultimodalContentArray 钉死 VL 系模型的数组 content 拼成文本。
//
// content 在 qwen-vl / qwen-audio 上是数组而非字符串。只认字符串的话整条 content
// 解成空串，模型回复凭空消失，响应却仍是 200；拼接时插入分隔符则会在回复里
// 凭空多出字符。非 text 键（image_hw）不是回复内容，混进来同样是污染。
func TestDecodeResultMultimodalContentArray(t *testing.T) {
	res, err := DecodeResult([]byte(`{"output":{"choices":[{"finish_reason":"stop",
	  "message":{"role":"assistant","content":[
	    {"text":"你"},{"text":"好"},{"image_hw":[1,2]}]}}]},
	  "request_id":"r"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Choices[0].Content != "你好" {
		t.Errorf("数组 content 应拼成 %q，实际 %q", "你好", res.Choices[0].Content)
	}
}

// TestDecodeResultMalformedContentIsUpstreamUnavailable 钉死畸形 content 报错，
// 而不是静默解成空串。
//
// content 是已建模的已知字段，官方只有字符串与「对象数组」两种形态。解不出来
// 时返回空串加 nil error，等于把模型的整段回复丢掉再回一个 200：客户端收到一条
// 空回复，网关的错误率、failover 与告警全都看不见这次损失。已知字段的值畸形
// 只能是上游或链路的问题，必须按 upstream_unavailable 记，让它可重试、可观测。
//
// 注意与「未知字段」的区别：顶层多出来的 SDK 包装字段照样容忍，这里收紧的只是
// 已知字段的值形态。
func TestDecodeResultMalformedContentIsUpstreamUnavailable(t *testing.T) {
	for _, tt := range []struct {
		name    string
		content string
	}{
		{"数字", `123`},
		{"布尔", `true`},
		{"对象", `{"text":"hi"}`},
		{"字符串数组", `["hi","there"]`},
		{"数字数组", `[1,2]`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := `{"output":{"choices":[{"finish_reason":"stop",
			  "message":{"role":"assistant","content":` + tt.content + `}}]},
			  "request_id":"r"}`

			res, err := DecodeResult([]byte(body))

			if err == nil {
				t.Fatalf("畸形 content 应报错，实际解成 %+v", res.Choices)
			}
			var cerr *canonical.Error
			if !errors.As(err, &cerr) {
				t.Fatalf("应是 canonical 类型化错误: %T", err)
			}
			if cerr.Class != canonical.ClassUpstreamUnavailable {
				t.Errorf("应归类为 upstream_unavailable，实际 %q", cerr.Class)
			}
		})
	}
}

// TestDecodeResultEmptyContentStaysValid 钉死「合法的空回复」不被误判成畸形。
//
// 发生 Function Calling 时官方明确 content 为空，流式最后一帧也常是空串。
// 把这几种形态一起判成上游故障，会让每一次工具调用与正常收尾的流都失败。
func TestDecodeResultEmptyContentStaysValid(t *testing.T) {
	for _, tt := range []struct {
		name    string
		message string
	}{
		{"content 缺省", `{"role":"assistant"}`},
		{"content 为 JSON null", `{"role":"assistant","content":null}`},
		{"content 为空串", `{"role":"assistant","content":""}`},
		{"content 为空数组", `{"role":"assistant","content":[]}`},
		{"工具调用时 content 为 null", `{"role":"assistant","content":null,
		  "tool_calls":[{"id":"c1","type":"function",
		    "function":{"name":"f","arguments":"{}"}}]}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := `{"output":{"choices":[{"finish_reason":"stop",
			  "message":` + tt.message + `}]},"request_id":"r"}`

			res, err := DecodeResult([]byte(body))

			if err != nil {
				t.Fatalf("合法的空 content 不应报错: %v", err)
			}
			if res.Choices[0].Content != "" {
				t.Errorf("应解成空串，实际 %q", res.Choices[0].Content)
			}
		})
	}
}

// TestDecodeResultPreservesChoiceOrderAndCount 钉死多候选按原数量、原顺序解出。
//
// Native 没有官方 index 字段，候选身份完全由数组下标决定。请求侧
// （encode_request.go）会发 parameters.n，上游因此可能返回多个候选：漏掉靠后的
// 候选，客户端点名的第 n 个回复就凭空消失；顺序错位则会把 A 的正文配上 B 的
// 结束原因——两种损失都不报错，响应仍是一个格式合法的 200。
//
// 两个候选的正文、结束原因、有无工具调用都刻意取不同值：任何一处相同都会让
// 「只取第一个」或「前后调换」照样通过。
func TestDecodeResultPreservesChoiceOrderAndCount(t *testing.T) {
	res, err := DecodeResult([]byte(`{"output":{"choices":[
	  {"finish_reason":"stop","message":{"role":"assistant","content":"第一个"}},
	  {"finish_reason":"tool_calls","message":{"role":"assistant","content":"第二个",
	    "tool_calls":[{"id":"c2","type":"function",
	      "function":{"name":"f2","arguments":"{}"}}]}}]},
	  "request_id":"r"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Choices) != 2 {
		t.Fatalf("应解出 2 个候选，实际 %d 个: %+v", len(res.Choices), res.Choices)
	}

	first := res.Choices[0]
	if first.Content != "第一个" {
		t.Errorf("候选 0 正文应为 %q，实际 %q", "第一个", first.Content)
	}
	if first.FinishReason != "stop" {
		t.Errorf("候选 0 结束原因应为 %q，实际 %q", "stop", first.FinishReason)
	}
	if len(first.ToolCalls) != 0 {
		t.Errorf("候选 0 不应有工具调用: %+v", first.ToolCalls)
	}

	second := res.Choices[1]
	if second.Content != "第二个" {
		t.Errorf("候选 1 正文应为 %q，实际 %q", "第二个", second.Content)
	}
	if second.FinishReason != "tool_calls" {
		t.Errorf("候选 1 结束原因应为 %q，实际 %q", "tool_calls", second.FinishReason)
	}
	if len(second.ToolCalls) != 1 || second.ToolCalls[0].ID != "c2" {
		t.Errorf("候选 1 的工具调用应原样解出: %+v", second.ToolCalls)
	}
}

// TestDecodeResultUsageAbsentVersusZero 钉死「没给用量」与「用量确实是 0」不混淆。
//
// 缺省时造一份零值 Usage，等于把一次用量未知的调用记成权威的免费调用，账单
// 从此少算且无从追溯；反过来把上游明确给出的全 0 判成不可信，则会让一次
// 本可计费的调用退出计费口径。
func TestDecodeResultUsageAbsentVersusZero(t *testing.T) {
	absent, err := DecodeResult([]byte(`{"output":{"choices":[{"finish_reason":"stop",
	  "message":{"role":"assistant","content":"ok"}}]},"request_id":"r"}`))
	if err != nil {
		t.Fatal(err)
	}
	if absent.Usage != nil {
		t.Errorf("usage 缺省应为 nil，实际 %+v", absent.Usage)
	}

	zero, err := DecodeResult([]byte(`{"output":{"choices":[{"finish_reason":"stop",
	  "message":{"role":"assistant","content":"ok"}}]},
	  "usage":{"input_tokens":0,"output_tokens":0},"request_id":"r"}`))
	if err != nil {
		t.Fatal(err)
	}
	if zero.Usage == nil || zero.Usage.Fidelity != canonical.FidelityAuthoritative {
		t.Fatalf("全零用量仍应是权威值: %+v", zero.Usage)
	}
	if zero.Usage.InputTokens != 0 || zero.Usage.OutputTokens != 0 {
		t.Errorf("全零用量不应被改写: %+v", zero.Usage)
	}
}

// TestDecodeResultUsageDetails 钉死缓存读与推理 token 的明细映射。
//
// cached_tokens 与普通 input token 计价不同，丢掉它账单就按未命中缓存算；
// reasoning_tokens 丢掉则看不出输出里有多少是思考消耗的。
func TestDecodeResultUsageDetails(t *testing.T) {
	res, err := DecodeResult([]byte(`{"output":{"choices":[{"finish_reason":"stop",
	  "message":{"role":"assistant","content":"ok"}}]},
	  "usage":{"input_tokens":26,"output_tokens":66,"total_tokens":92,
	    "prompt_tokens_details":{"cached_tokens":12},
	    "output_tokens_details":{"reasoning_tokens":30}},
	  "request_id":"r"}`))
	if err != nil {
		t.Fatal(err)
	}
	u := res.Usage
	if u == nil {
		t.Fatal("usage 应解出")
	}
	if u.CacheReadInputTokens != 12 {
		t.Errorf("cached_tokens 应映射到 CacheReadInputTokens: %+v", u)
	}
	if u.ReasoningTokens != 30 {
		t.Errorf("reasoning_tokens 应映射到 ReasoningTokens: %+v", u)
	}
}

// TestDecodeFrameCumulativeUsage 钉死流式帧携带累计 usage 且 request_id 稳定。
func TestDecodeFrameCumulativeUsage(t *testing.T) {
	res, err := DecodeFrame(`{"output":{"choices":[{"finish_reason":"stop",
	  "message":{"role":"assistant","content":""}}]},
	  "usage":{"input_tokens":26,"output_tokens":66,"total_tokens":92},
	  "request_id":"d30a9914"}`)
	if err != nil {
		t.Fatal(err)
	}
	if res.RequestID != "d30a9914" {
		t.Errorf("request_id 应解出: %q", res.RequestID)
	}
	if res.Usage == nil || res.Usage.OutputTokens != 66 {
		t.Errorf("累计 usage 应解出: %+v", res.Usage)
	}
}

// TestDecodeFrameMatchesDecodeResult 钉死同一份负载在两个入口解出同一个结果。
//
// 两边各写一套解码逻辑的话，只在一侧被想起来的归一规则（例如字符串 "null"）
// 会漂移：同一份字节流式解出「已结束」、非流式解出「生成中」，两条路径的行为
// 从此不一致，而两边各自的测试都是绿的。
func TestDecodeFrameMatchesDecodeResult(t *testing.T) {
	const payload = `{"output":{"choices":[{"finish_reason":"null","message":{
	  "role":"assistant","content":"我是","reasoning_content":"想",
	  "logprobs":{"content":[]},
	  "tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"a"}}]}}]},
	  "usage":{"input_tokens":26,"output_tokens":1,"total_tokens":27,
	    "prompt_tokens_details":{"cached_tokens":0}},
	  "request_id":"d30a9914"}`

	fromBody, err := DecodeResult([]byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	fromFrame, err := DecodeFrame(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromBody, fromFrame) {
		t.Errorf("两个入口结果应完全一致:\n非流式 %+v\n流式   %+v", fromBody, fromFrame)
	}
}

// TestDecodeResultToleratesSDKWrapperFields 钉死 SDK 包装字段不影响解码。
//
// status_code / code / message 是 SDK 层加的，裸 HTTP 不出现。用严格模式解会
// 让一个本来成功的响应被判成上游故障，请求平白失败。
func TestDecodeResultToleratesSDKWrapperFields(t *testing.T) {
	res, err := DecodeResult([]byte(`{"status_code":200,"code":"","message":"",
	  "output":{"choices":[{"finish_reason":"stop",
	    "message":{"role":"assistant","content":"ok"}}]},
	  "request_id":"r"}`))
	if err != nil {
		t.Fatalf("SDK 包装字段不应导致解码失败: %v", err)
	}
	if res.Choices[0].Content != "ok" {
		t.Errorf("正文应正常解出: %q", res.Choices[0].Content)
	}
}

// TestDecodeResultMalformedIsUpstreamUnavailable 钉死畸形字节记在上游账上。
//
// 记成 bad_request 会把客户端引去改一个本来合法的请求，真正的上游故障既不进
// 故障统计，也换不到另一个凭据或 Provider 重试。
func TestDecodeResultMalformedIsUpstreamUnavailable(t *testing.T) {
	_, err := DecodeResult([]byte(`{"output":`))
	if err == nil {
		t.Fatal("畸形负载应报错")
	}
	var cerr *canonical.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("应是 canonical 类型化错误: %T", err)
	}
	if cerr.Class != canonical.ClassUpstreamUnavailable {
		t.Errorf("应归类为 upstream_unavailable，实际 %q", cerr.Class)
	}
}
