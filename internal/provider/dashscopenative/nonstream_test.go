package dashscopenative

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/credential"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/protocol/openaichat"
	"github.com/yobo2u/omugw/internal/provider"
	"github.com/yobo2u/omugw/internal/router"
	"github.com/yobo2u/omugw/internal/transport/httpx"
)

// 注入时钟的锚点与步长。取固定值而非 time.Now：created 与 Latency 都由时钟推导，
// 用真实时间只能断言「差不多」，那种断言在映射被接错时照样通过。
var (
	clockBase = time.Unix(1755216000, 0).UTC()
	clockStep = 250 * time.Millisecond
)

// stepClock 是逐次调用固定步进的确定性时钟。
//
// httpx.Client 与 Provider 共用同一个实例，是为了让 Latency 与 created 都落在
// 可精确预测的刻度上——各给一个时钟，两边的时间线就再也对不上，超时与时序类
// 的回归也就没有任何断言能抓住。
type stepClock struct {
	calls atomic.Int64
}

// Now 返回 base + n*step，n 为本次之前已发生的调用次数。
//
// 用原子计数而不是裸 int：httptest 的请求处理跑在服务端自己的 goroutine 上，
// 与被测代码并发；同一个时钟又被 httpx.Client 与 Provider 两处共用，裸自增在
// -race 下必被判定为数据竞争。
func (c *stepClock) Now() time.Time {
	n := c.calls.Add(1) - 1
	return clockBase.Add(time.Duration(n) * clockStep)
}

// at 给出第 n 次调用（0 起）应当返回的时刻，供测试算期望值。
func (c *stepClock) at(n int64) time.Time {
	return clockBase.Add(time.Duration(n) * clockStep)
}

// newClockedProvider 构造共用一个注入时钟的 Provider。
func newClockedProvider(t *testing.T) (*Provider, *stepClock) {
	t.Helper()
	clk := &stepClock{}
	return New(httpx.New(config.Default().Timeouts, clk.Now), clk.Now), clk
}

func mustCanonical(t *testing.T, raw string) *canonical.Request {
	t.Helper()
	d, err := openaichat.Decode([]byte(raw))
	if err != nil {
		t.Fatalf("mustCanonical decode failed: %v", err)
	}
	return &d.Request
}

func mustProject(t *testing.T, raw []byte) *openaichat.Projection {
	t.Helper()
	p, err := openaichat.Project(raw)
	if err != nil {
		t.Fatalf("mustProject failed: %v", err)
	}
	return p
}

func chatRequest(t *testing.T, baseURL string) provider.Request {
	t.Helper()
	return chatRequestWithBody(t, baseURL, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
}

// chatRequestWithBody 按给定 Chat 线格式构造出站请求，Raw 与 Canonical 同源。
func chatRequestWithBody(t *testing.T, baseURL, raw string) provider.Request {
	t.Helper()
	return provider.Request{
		Target: router.Target{
			Kind:           degrade.ProviderDashScopeNative,
			Endpoint:       "e",
			BaseURL:        baseURL,
			UpstreamModel:  "qwen-plus",
			CredentialPool: "p",
			NativeEndpoint: "text-generation",
		},
		Credential: credential.Credential{ID: "k", Secret: "sk-test"},
		Raw:        []byte(raw),
		Canonical:  mustCanonical(t, raw),
		Stream:     false,
		Inbound:    degrade.Inbound{Protocol: degrade.ProtoOpenAIChat, Endpoint: degrade.EndpointOpenAIChat},
	}
}

// chatCompletion 是下游 chat.completion 的结构化视图。
//
// 断言走结构化解码而不是 strings.Contains：子串断言分不清 `"index":0` 出现在
// 第几条候选，也分不清 finish_reason 与 content 的配对关系——两条候选的字段
// 互换后，每个子串仍然都能在 body 里找到，测试照绿。
type chatCompletion struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []completionChoice `json:"choices"`
	Usage   *completionUsage   `json:"usage"`
}

type completionChoice struct {
	Index        int             `json:"index"`
	Message      completionMsg   `json:"message"`
	FinishReason *string         `json:"finish_reason"`
	Logprobs     json.RawMessage `json:"logprobs"`
}

type completionMsg struct {
	Role             string           `json:"role"`
	Content          string           `json:"content"`
	ReasoningContent string           `json:"reasoning_content"`
	ToolCalls        []completionCall `json:"tool_calls"`
}

type completionCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function completionFunction `json:"function"`
}

type completionFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type completionUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// drainCompletion 读净并关闭响应体，再结构化解码。
//
// 读与关都检查错误：漏关会把连接钉死在池子里，而忽略 ReadAll 的错误会让一个
// 被截断的 body 当成完整响应参与断言——断言过了，实际返回的却是半截 JSON。
func drainCompletion(t *testing.T, resp *httpx.Response) ([]byte, chatCompletion) {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取转换后响应体失败: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("关闭转换后响应体失败: %v", err)
	}
	var got chatCompletion
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("转换后响应体不是合法 chat.completion: %v (body=%s)", err, body)
	}
	return body, got
}

// mustClose 关掉本用例不再消费的成功响应。
func mustClose(t *testing.T, resp *httpx.Response) {
	t.Helper()
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("关闭响应体失败: %v", err)
	}
}

// nativeBody 是转换测试共用的上游 Native 成功响应。
const nativeBody = `{"output":{"choices":[{"finish_reason":"stop",` +
	`"message":{"role":"assistant","content":"你好"}}]},` +
	`"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7},"request_id":"req-abc"}`

// TestNonStreamTransform 钉死非流式在 Call 内完整转换：Native 进、chat.completion 出。
func TestNonStreamTransform(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// 上游刻意声明 text/plain：与下游期望值不同，重置才真的被证明。
		// 上游本来就说 application/json 时，一个根本没重置 Content-Type 的实现
		// 也能让下游断言通过——那条断言就什么都没钉住。
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, nativeBody)
	}))
	defer up.Close()

	p, clk := newClockedProvider(t)
	var usageCalls int
	var gotUsage canonical.Usage
	req := chatRequest(t, up.URL)
	req.OnDashScopeUsage = func(u canonical.Usage) { usageCalls++; gotUsage = u }

	resp, err := p.translateNonStream(context.Background(), req, mustProject(t, req.Raw))
	if err != nil {
		t.Fatalf("非流式转换应成功: %v", err)
	}
	body, got := drainCompletion(t, resp)

	// 时钟调用序：Do 前 start=第 0 次、响应头到达 first=第 1 次，
	// 转换阶段的 p.now() 是第 2 次。三者都由同一个注入时钟推导，可精确断言。
	wantFirstByte := clk.at(1)
	wantCreated := clk.at(2).Unix()

	if got.ID != "req-abc" {
		t.Errorf("id 必须原样取上游 request_id，实际 %q", got.ID)
	}
	if got.Object != "chat.completion" {
		t.Errorf("object 应为 chat.completion，实际 %q", got.Object)
	}
	if got.Model != "qwen-plus" {
		t.Errorf("model 应为路由目标的上游模型名 qwen-plus，实际 %q", got.Model)
	}
	if got.Created != wantCreated {
		t.Errorf("created 应取注入时钟 %d，实际 %d", wantCreated, got.Created)
	}
	if len(got.Choices) != 1 {
		t.Fatalf("应恰好一条候选，实际 %d", len(got.Choices))
	}
	if c := got.Choices[0]; c.Index != 0 || c.Message.Role != "assistant" || c.Message.Content != "你好" {
		t.Errorf("候选内容映射错误: %+v", c)
	}
	if fr := got.Choices[0].FinishReason; fr == nil || *fr != "stop" {
		t.Errorf("finish_reason 应为 stop，实际 %v", fr)
	}
	if got.Usage == nil {
		t.Fatalf("usage 应被编码进 chat.completion")
	}
	if got.Usage.PromptTokens != 5 || got.Usage.CompletionTokens != 2 || got.Usage.TotalTokens != 7 {
		t.Errorf("usage 逐项映射错误: %+v", got.Usage)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("状态码应保持 200，实际 %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type 应为 application/json，实际 %q", ct)
	}
	if resp.ContentLength != int64(len(body)) {
		t.Errorf("ContentLength 应重置为新 body 长度 %d，实际 %d", len(body), resp.ContentLength)
	}
	// 头与字段必须同时重置：只改 ContentLength 而留着上游那个旧的 Content-Length
	// 头，下游会按旧长度截断或挂起等永远不来的字节。
	if cl := resp.Header.Get("Content-Length"); cl != strconv.Itoa(len(body)) {
		t.Errorf("Content-Length 头应重置为 %d，实际 %q", len(body), cl)
	}
	// 长度必须与 Native 原体不同，重置才真的被证明——两者恰好等长时，
	// 一个根本没重置的实现也能让上面那条断言通过。
	if len(body) == len(nativeBody) {
		t.Errorf("转换后 body 长度不应与 Native 原体相同（均为 %d），无法证明重置", len(body))
	}

	// 上游时序必须原样保留：它是换凭据重试的依据，被覆盖成转换完成的时刻，
	// 观测到的首字节耗时就把网关自己的转换耗时算进了上游头上。
	if !resp.UpstreamFirstByte.Equal(wantFirstByte) {
		t.Errorf("UpstreamFirstByte 应为 %v，实际 %v", wantFirstByte, resp.UpstreamFirstByte)
	}
	if resp.Latency != clockStep {
		t.Errorf("Latency 应为注入时钟的一个步长 %v，实际 %v", clockStep, resp.Latency)
	}

	if usageCalls != 1 {
		t.Errorf("OnDashScopeUsage 应恰调用一次，实际 %d", usageCalls)
	}
	if gotUsage.OutputTokens != 2 || gotUsage.InputTokens != 5 ||
		gotUsage.Fidelity != canonical.FidelityAuthoritative {
		t.Errorf("回调用量应为 authoritative 且 in=5/out=2: %+v", gotUsage)
	}
}

// nativeRequest 是出站 Native 信封的结构化视图，用于逐字段核对投影落点。
type nativeRequest struct {
	Model      string         `json:"model"`
	Input      nativeInput    `json:"input"`
	Parameters map[string]any `json:"parameters"`
}

type nativeInput struct {
	Messages []nativeMessage `json:"messages"`
}

type nativeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// TestNonStreamOutboundRequest 钉死出站 Native 请求的路径、信封与全部投影字段。
//
// 出站字节是这条异构路径唯一会到达上游的东西。只断言「转换成功」而不看发出去
// 的是什么，一个把 n 漏掉、把 max_tokens 发成两份、或敲错门的实现同样能绿。
func TestNonStreamOutboundRequest(t *testing.T) {
	var gotPath string
	var gotHeader http.Header
	var gotBody []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotHeader = r.Header.Clone()
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("读取出站请求体失败: %v", err)
		}
		gotBody = b
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, nativeBody)
	}))
	defer up.Close()

	p, _ := newClockedProvider(t)
	req := chatRequestWithBody(t, up.URL, `{"model":"m","messages":[{"role":"user","content":"hi"}],`+
		`"n":2,"presence_penalty":0.5,"logprobs":true,"top_logprobs":3,`+
		`"parallel_tool_calls":false,"max_tokens":128,"web_search_options":{}}`)

	resp, err := p.translateNonStream(context.Background(), req, mustProject(t, req.Raw))
	if err != nil {
		t.Fatalf("非流式转换应成功: %v", err)
	}
	mustClose(t, resp)

	if gotPath != "/api/v1/services/aigc/text-generation/generation" {
		t.Errorf("门 text-generation 应打到文本生成端点，实际 %q", gotPath)
	}

	var out nativeRequest
	if err := json.Unmarshal(gotBody, &out); err != nil {
		t.Fatalf("出站请求体不是合法 Native 信封: %v (body=%s)", err, gotBody)
	}
	// model 取路由目标而不是客户端提交的逻辑名——改名正是网关的职责所在。
	if out.Model != "qwen-plus" {
		t.Errorf("出站 model 应改写为上游模型名 qwen-plus，实际 %q", out.Model)
	}
	if len(out.Input.Messages) != 1 ||
		out.Input.Messages[0].Role != "user" || out.Input.Messages[0].Content != "hi" {
		t.Errorf("出站 input.messages 映射错误: %+v", out.Input.Messages)
	}

	// result_format 必须是 message：缺了它上游回旧版 text 形态，
	// 解码器读不到 message.content，模型回复凭空消失而请求仍是 200。
	assertParam(t, out.Parameters, "result_format", "message")
	assertParam(t, out.Parameters, "n", float64(2))
	assertParam(t, out.Parameters, "presence_penalty", 0.5)
	assertParam(t, out.Parameters, "logprobs", true)
	assertParam(t, out.Parameters, "top_logprobs", float64(3))
	// 显式 false 必须原样保留，不能被缺省注入的 true 覆盖掉。
	assertParam(t, out.Parameters, "parallel_tool_calls", false)
	assertParam(t, out.Parameters, "max_tokens", float64(128))
	assertParam(t, out.Parameters, "enable_search", true)
	// 客户端提交的是 max_tokens，就不许再造一个 max_completion_tokens：
	// 两个上限同时发，取哪个是上游的实现细节，输出长度因此不可预期。
	if _, ok := out.Parameters["max_completion_tokens"]; ok {
		t.Errorf("客户端只提交 max_tokens 时不得同时发 max_completion_tokens: %+v", out.Parameters)
	}
	if _, ok := out.Parameters["incremental_output"]; ok {
		t.Errorf("非流式不得发 incremental_output: %+v", out.Parameters)
	}

	if ct := gotHeader.Get("Content-Type"); ct != "application/json" {
		t.Errorf("出站 Content-Type 应为 application/json，实际 %q", ct)
	}
	if ac := gotHeader.Get("Accept"); ac != "application/json" {
		t.Errorf("非流式出站 Accept 应为 application/json，实际 %q", ac)
	}
	if auth := gotHeader.Get("Authorization"); auth != "Bearer sk-test" {
		t.Errorf("出站鉴权必须用网关自己的凭据，实际 %q", auth)
	}
	if sse := gotHeader.Get("X-DashScope-SSE"); sse != "" {
		t.Errorf("非流式不得声明 SSE，实际 %q", sse)
	}
}

// assertParam 逐项核对出站 parameters。JSON 数字统一解成 float64，
// 期望值也按同一口径给，避免类型不等造成的假失败。
func assertParam(t *testing.T, params map[string]any, key string, want any) {
	t.Helper()
	got, ok := params[key]
	if !ok {
		t.Errorf("出站 parameters 缺少 %s（实际 %+v）", key, params)
		return
	}
	if got != want {
		t.Errorf("出站 parameters.%s 应为 %v(%T)，实际 %v(%T)", key, want, want, got, got)
	}
}

// TestNonStreamUpstreamErrorFailover 钉死上游非 2xx 在 Call 内解码为 *canonical.Error（可 failover）。
func TestNonStreamUpstreamErrorFailover(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"code":"Throttling","message":"rate limit","request_id":"req-429"}`)
	}))
	defer up.Close()

	p, _ := newClockedProvider(t)
	req := chatRequest(t, up.URL)
	_, err := p.translateNonStream(context.Background(), req, mustProject(t, req.Raw))
	if err == nil {
		t.Fatal("上游 429 应返回错误")
	}
	cerr := canonical.AsError(err)
	if cerr.Class != canonical.ClassRateLimit {
		t.Fatalf("应分类为 rate_limit，实际 %v", cerr)
	}
	// Retryable 是「换个凭据或 Provider 可能成功」，限流恰是这一档。
	// 丢成 false，上游一限流整条请求就直接失败，凭据池里的其他 key 白放着。
	if !cerr.Retryable {
		t.Errorf("限流必须可 failover，实际 Retryable=false")
	}
	if cerr.UpstreamStatus != http.StatusTooManyRequests {
		t.Errorf("UpstreamStatus 应保留 429，实际 %d", cerr.UpstreamStatus)
	}
	// request_id 是向阿里云提工单时的唯一凭据，丢了这次故障就再也查不回来。
	if cerr.UpstreamRequestID != "req-429" {
		t.Errorf("UpstreamRequestID 应取 Native 信封的 request_id，实际 %q", cerr.UpstreamRequestID)
	}
}

// TestNonStreamMultiChoice 钉死多候选非流式：条数、序号顺序、逐条内容与 Native 专有字段。
func TestNonStreamMultiChoice(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output":{"choices":[
			{"finish_reason":"stop","message":{"role":"assistant","content":"一",
			  "reasoning_content":"想了想","logprobs":{"content":[{"token":"一"}]}}},
			{"finish_reason":"tool_calls","message":{"role":"assistant","content":"二",
			  "tool_calls":[{"id":"call_9","function":{"name":"get_weather","arguments":"{\"city\":\"hz\"}"}}]}}
		]},"request_id":"req-multi"}`)
	}))
	defer up.Close()

	p, _ := newClockedProvider(t)
	req := chatRequest(t, up.URL)
	resp, err := p.translateNonStream(context.Background(), req, mustProject(t, req.Raw))
	if err != nil {
		t.Fatalf("非流式转换应成功: %v", err)
	}
	_, got := drainCompletion(t, resp)

	// 条数与序号一起断言：Native 没有官方 index 字段，序号由数组下标推导，
	// 顺序错乱时两条候选的内容会张冠李戴，而每个子串仍然「都在 body 里」。
	if len(got.Choices) != 2 {
		t.Fatalf("应恰好两条候选，实际 %d", len(got.Choices))
	}
	first, second := got.Choices[0], got.Choices[1]
	if first.Index != 0 || second.Index != 1 {
		t.Fatalf("候选序号应按上游顺序为 0/1，实际 %d/%d", first.Index, second.Index)
	}
	if first.Message.Content != "一" || second.Message.Content != "二" {
		t.Errorf("候选内容与顺序不匹配: %q / %q", first.Message.Content, second.Message.Content)
	}
	if fr := first.FinishReason; fr == nil || *fr != "stop" {
		t.Errorf("首条 finish_reason 应为 stop，实际 %v", fr)
	}
	if fr := second.FinishReason; fr == nil || *fr != "tool_calls" {
		t.Errorf("次条 finish_reason 应为 tool_calls，实际 %v", fr)
	}

	// reasoning_content 是 Native 专有字段，落在 Chat 的同名扩展键上。
	// 丢了它，思考过程静默消失，客户端只看到结论。
	if first.Message.ReasoningContent != "想了想" {
		t.Errorf("reasoning_content 应保留，实际 %q", first.Message.ReasoningContent)
	}
	if second.Message.ReasoningContent != "" {
		t.Errorf("未提供 reasoning_content 的候选不应凭空生出内容，实际 %q", second.Message.ReasoningContent)
	}
	// logprobs 原样透传：重新编解码一轮会丢掉字段顺序与上游新增的键。
	if len(first.Logprobs) == 0 || string(first.Logprobs) != `{"content":[{"token":"一"}]}` {
		t.Errorf("logprobs 应原样透传，实际 %s", first.Logprobs)
	}

	if len(second.Message.ToolCalls) != 1 {
		t.Fatalf("次条应恰有一次工具调用，实际 %d", len(second.Message.ToolCalls))
	}
	call := second.Message.ToolCalls[0]
	// ID 与 name 一律取上游原值：网关自造工具 ID，续轮时客户端回填的
	// tool_call_id 与上游对不上，整轮调用被上游丢弃而请求仍是 200。
	if call.ID != "call_9" || call.Function.Name != "get_weather" {
		t.Errorf("工具调用 ID/名称应原样保留: %+v", call)
	}
	if call.Type != "function" {
		t.Errorf("工具调用 type 应为 function，实际 %q", call.Type)
	}
	if call.Function.Arguments != `{"city":"hz"}` {
		t.Errorf("工具参数应原样保留，实际 %q", call.Function.Arguments)
	}
	if first.Message.ToolCalls != nil {
		t.Errorf("未调用工具的候选不应凭空生出 tool_calls: %+v", first.Message.ToolCalls)
	}
}

// TestNonStreamNoUsage 钉死 usage 缺失时回调不被调用，且 chat.completion 无 usage 字段。
func TestNonStreamNoUsage(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output":{"choices":[{"finish_reason":"stop",`+
			`"message":{"role":"assistant","content":"1"}}]},"request_id":"req-nousage"}`)
	}))
	defer up.Close()

	p, _ := newClockedProvider(t)
	var usageCalls int
	req := chatRequest(t, up.URL)
	req.OnDashScopeUsage = func(canonical.Usage) { usageCalls++ }

	resp, err := p.translateNonStream(context.Background(), req, mustProject(t, req.Raw))
	if err != nil {
		t.Fatalf("非流式转换应成功: %v", err)
	}
	_, got := drainCompletion(t, resp)

	// 上游没给用量就一个字都不能编：硬造一份「0 token 且权威」的记录，
	// 等于把一次未知用量的调用计成免费。
	if usageCalls != 0 {
		t.Errorf("usage 缺失时回调不应被调用，实际 %d 次", usageCalls)
	}
	if got.Usage != nil {
		t.Errorf("usage 缺失时 chat.completion 不应有 usage 字段: %+v", got.Usage)
	}
	if got.ID != "req-nousage" {
		t.Errorf("id 仍应取 request_id，实际 %q", got.ID)
	}
}

// TestNonStreamInvalidToolJSON 钉死非法工具参数即 upstream_unavailable，且不触发用量回调。
func TestNonStreamInvalidToolJSON(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output":{"choices":[{"finish_reason":"tool_calls",`+
			`"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1",`+
			`"function":{"name":"f","arguments":"{invalid}"}}]}}]},`+
			`"usage":{"input_tokens":1,"output_tokens":1},"request_id":"req-invalid"}`)
	}))
	defer up.Close()

	p, _ := newClockedProvider(t)
	var usageCalls int
	req := chatRequest(t, up.URL)
	req.OnDashScopeUsage = func(canonical.Usage) { usageCalls++ }

	_, err := p.translateNonStream(context.Background(), req, mustProject(t, req.Raw))
	if err == nil {
		t.Fatal("invalid tool JSON 应返回错误")
	}
	cerr := canonical.AsError(err)
	// 这段字节来自上游，畸形只能是上游或链路的问题。记成 bad_request 会把
	// 客户端引去改一个本来合法的请求，真正的上游故障却换不到另一个凭据重试。
	if cerr.Class != canonical.ClassUpstreamUnavailable {
		t.Fatalf("应分类为 upstream_unavailable，实际 %v", cerr)
	}
	// 报错必须点到候选与工具的下标，否则多候选多工具时无从定位是哪一个坏了。
	if !strings.Contains(cerr.Message, "choices[0]") || !strings.Contains(cerr.Message, "tool_calls[0]") {
		t.Errorf("错误必须点名候选与工具下标，实际 %q", cerr.Message)
	}
	// 回调发生在编码成功之后：失败路径上调用它，会把一次并未交付的调用计了费。
	if usageCalls != 0 {
		t.Errorf("失败时不应调用 usage 回调，实际 %d 次", usageCalls)
	}
}

// TestNonStreamUnknownDoor 钉死未知门 fail-closed 且一个字节都不出门。
//
// 起真实上游并计数，而不是指向一个不存在的地址：后者「没打通」与「压根没发」
// 长得一模一样，未知门被静默当成默认门发出去时，测试照样绿。
func TestNonStreamUnknownDoor(t *testing.T) {
	var calls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, nativeBody)
	}))
	defer up.Close()

	p, _ := newClockedProvider(t)
	req := chatRequest(t, up.URL)
	req.Target.NativeEndpoint = "unknown-door"

	_, err := p.translateNonStream(context.Background(), req, mustProject(t, req.Raw))
	if err == nil {
		t.Fatal("unknown door 应返回错误")
	}
	cerr := canonical.AsError(err)
	// 门是部署事实，由配置声明。走到这里的未知门只能是网关自己配错了，
	// 记成 bad_request 会让客户端去改一个本来就对的请求。
	if cerr.Class != canonical.ClassInternal {
		t.Fatalf("应分类为 internal，实际 %v", cerr)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("未知门不得触达上游，实际出门 %d 次", got)
	}
}

// TestNonStreamHeaders 钉死语义头白名单与凭据隔离，并消费掉成功响应。
func TestNonStreamHeaders(t *testing.T) {
	var gotHeader http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, nativeBody)
	}))
	defer up.Close()

	p, _ := newClockedProvider(t)
	req := chatRequest(t, up.URL)
	req.Header = make(http.Header)
	req.Header.Set("X-DashScope-WorkSpace", "ws-1")
	req.Header.Set("X-DashScope-DataInspection", "enable")
	req.Header.Set("X-DashScope-Async", "enable")
	req.Header.Set("X-Custom-Header", "custom")
	// 客户端自己的 token 到网关为止：转发它等于把客户端凭据泄露给上游。
	req.Header.Set("Authorization", "Bearer client-token")

	resp, err := p.translateNonStream(context.Background(), req, mustProject(t, req.Raw))
	if err != nil {
		t.Fatalf("非流式转换应成功: %v", err)
	}
	// 成功响应的所有权在调用方：不消费就直接返回，连接会一直挂在池子里。
	_, got := drainCompletion(t, resp)
	if got.Object != "chat.completion" {
		t.Errorf("成功响应仍应是 chat.completion，实际 %q", got.Object)
	}

	// 白名单内的语义头必须原样带走：WorkSpace 是租户边界，丢了请求会落到别的租户。
	for name, want := range map[string]string{
		"X-DashScope-WorkSpace":      "ws-1",
		"X-DashScope-DataInspection": "enable",
		"X-DashScope-Async":          "enable",
	} {
		if v := gotHeader.Get(name); v != want {
			t.Errorf("语义头 %s 应透传为 %q，实际 %q", name, want, v)
		}
	}
	if auth := gotHeader.Get("Authorization"); auth != "Bearer sk-test" {
		t.Errorf("出站鉴权必须是网关凭据而非客户端 token，实际 %q", auth)
	}
	if v := gotHeader.Get("X-Custom-Header"); v != "" {
		t.Errorf("白名单外的任意头不得透传，实际 %q", v)
	}
	if v := gotHeader.Get("X-DashScope-SSE"); v != "" {
		t.Errorf("非流式不应设置 SSE 头，实际 %q", v)
	}
}

// fillingBody 是永不结束的响应体：每次 Read 都把整个缓冲填满并计数。
//
// 用它而不是 httptest：真实服务端写多少字节由内核缓冲与连接状态决定，
// 「读了多少」根本不可断言——那样的用例只能证明「没 panic」，一个把上限
// 写成 64MiB 甚至完全不设限的实现照样能过。
type fillingBody struct {
	read   atomic.Int64
	closes atomic.Int64
}

func (f *fillingBody) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	f.read.Add(int64(len(p)))
	return len(p), nil
}

func (f *fillingBody) Close() error {
	f.closes.Add(1)
	return nil
}

// TestDecodeErrorCapsReadVolumeAt64KiB 钉死错误体读取恰好截断在 64KiB，且 Body 被关闭。
//
// 直接喂给错误解码器：一个故障上游可以无限地吐字节，不设上限时这次上游故障
// 会升级成网关自己的内存事故，而症状只是「网关 OOM 了」。
func TestDecodeErrorCapsReadVolumeAt64KiB(t *testing.T) {
	body := &fillingBody{}
	resp := &httpx.Response{
		Response: &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{},
			Body:       body,
		},
	}

	p, _ := newClockedProvider(t)
	err := p.decodeNativeError(resp)
	if err == nil {
		t.Fatal("非 2xx 必须返回错误")
	}
	cerr := canonical.AsError(err)
	if cerr.Class != canonical.ClassBadRequest {
		t.Errorf("400 且无 code 时应分类为 bad_request，实际 %v", cerr)
	}

	// 期望值写死成字面量而不是引用生产常量：引用它等于拿被测代码算期望值，
	// 上限被改成 128KiB 时两边一起变，断言永远成立却什么都没证明。
	const wantCap = int64(64 << 10)
	if got := body.read.Load(); got != wantCap {
		t.Errorf("错误体读取应恰好截断在 %d 字节，实际读了 %d", wantCap, got)
	}
	if got := body.closes.Load(); got != 1 {
		t.Errorf("错误路径必须恰好关闭一次 Body，实际 %d 次", got)
	}
}

// successBody 是可数关闭次数、可注入关闭错误的成功响应体。
//
// 直接构造而不是走 httptest：真实连接的 Close 由 net/http 自己兜着，测试既数
// 不到次数，也没法让它失败——「漏关」与「关了」在那种用例里长得一模一样。
type successBody struct {
	reader   io.Reader
	closes   atomic.Int64
	closeErr error
}

func newSuccessBody(payload string, closeErr error) *successBody {
	return &successBody{reader: strings.NewReader(payload), closeErr: closeErr}
}

func (s *successBody) Read(p []byte) (int, error) { return s.reader.Read(p) }

func (s *successBody) Close() error {
	s.closes.Add(1)
	return s.closeErr
}

// newSuccessResponse 包一个 200 的 httpx.Response，直接喂给 translateNonStream
// 之后的读取阶段（经 readNativeBody）。
func newSuccessResponse(body io.ReadCloser) *httpx.Response {
	return &httpx.Response{
		Response: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{},
			Body:       body,
		},
	}
}

// TestReadNativeBodyClosesExactlyOnce 钉死成功路径的 Body 恰好被关闭一次。
//
// 漏关会把 httpx 挂在 Body 上的整体超时 cancel 一起漏掉，连接与上下文一直悬着；
// 关两次则可能在真实 Body 上二次释放。两个方向都必须被一条断言同时挡住。
func TestReadNativeBodyClosesExactlyOnce(t *testing.T) {
	body := newSuccessBody(nativeBody, nil)

	got, err := readNativeBody(newSuccessResponse(body))
	if err != nil {
		t.Fatalf("正常读取应成功: %v", err)
	}
	if string(got) != nativeBody {
		t.Errorf("应原样读出上游字节，实际 %s", got)
	}
	if n := body.closes.Load(); n != 1 {
		t.Errorf("成功路径必须恰好关闭一次 Body，实际 %d 次", n)
	}
}

// TestReadNativeBodyCloseFailureIsUpstreamUnavailable 钉死「读成功但关闭失败」被上报。
//
// 关闭失败说明连接处于异常状态，这次响应不值得信任。吞掉它等于把一个坏连接上
// 读到的字节当成正常响应交付下游，而错误率与 failover 都看不见这件事。
func TestReadNativeBodyCloseFailureIsUpstreamUnavailable(t *testing.T) {
	wantErr := errors.New("连接被重置")
	body := newSuccessBody(nativeBody, wantErr)

	got, err := readNativeBody(newSuccessResponse(body))
	if err == nil {
		t.Fatal("关闭失败必须返回错误")
	}
	if got != nil {
		t.Errorf("失败时不得返回字节，实际 %s", got)
	}
	cerr := canonical.AsError(err)
	if cerr.Class != canonical.ClassUpstreamUnavailable {
		t.Fatalf("应分类为 upstream_unavailable，实际 %v", cerr)
	}
	// 原始错误必须可 errors.Is 追回：只留一句中文描述，排查时无从判断是超时、
	// 重置还是 TLS 失败。
	if !errors.Is(err, wantErr) {
		t.Errorf("应保留底层关闭错误，实际 %v", err)
	}
	if n := body.closes.Load(); n != 1 {
		t.Errorf("失败路径同样只应关闭一次，实际 %d 次", n)
	}
}

// TestReadNativeBodyReadErrorTakesPrecedence 钉死读取错误优先于关闭错误。
//
// 两者同时发生时，读取失败才是根因——字节压根没拿全。反过来报关闭错误，会把
// 排查引向连接层，而真正的截断没人看见。
func TestReadNativeBodyReadErrorTakesPrecedence(t *testing.T) {
	readErr := errors.New("读取被截断")
	closeErr := errors.New("连接被重置")
	body := &failingReadBody{readErr: readErr, closeErr: closeErr}

	if _, err := readNativeBody(newSuccessResponse(body)); err == nil {
		t.Fatal("读取失败必须返回错误")
	} else if !errors.Is(err, readErr) {
		t.Errorf("应上报读取错误而非关闭错误，实际 %v", err)
	}
	if n := body.closes.Load(); n != 1 {
		t.Errorf("读取失败时仍必须关闭一次 Body，实际 %d 次", n)
	}
}

// failingReadBody 的 Read 与 Close 都失败，用于验证两种错误的优先级。
type failingReadBody struct {
	readErr  error
	closeErr error
	closes   atomic.Int64
}

func (f *failingReadBody) Read([]byte) (int, error) { return 0, f.readErr }

func (f *failingReadBody) Close() error {
	f.closes.Add(1)
	return f.closeErr
}

// TestNonStreamZeroChoicesFailsClosed 钉死「200 但零候选」不得交付成 choices:[]。
//
// DashScope 的异步受理等形态会回一个语法完整、output.choices 为空的 200。
// 编成空数组交付，客户端会当成模型什么都没说而正常收下——真实原因就此消失，
// 既进不了错误率，也换不到另一个凭据或 Provider 重试。
func TestNonStreamZeroChoicesFailsClosed(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output":{"choices":[]},`+
			`"usage":{"input_tokens":3,"output_tokens":0},"request_id":"req-empty"}`)
	}))
	defer up.Close()

	p, _ := newClockedProvider(t)
	var usageCalls int
	req := chatRequest(t, up.URL)
	req.OnDashScopeUsage = func(canonical.Usage) { usageCalls++ }

	resp, err := p.translateNonStream(context.Background(), req, mustProject(t, req.Raw))
	if err == nil {
		t.Fatal("零候选的 200 必须 fail-closed")
	}
	if resp != nil {
		t.Errorf("失败时不得返回响应，实际 %+v", resp)
	}
	cerr := canonical.AsError(err)
	// 字节来自上游，形态不符只能是上游或链路的问题；记成 bad_request 会把
	// 客户端引去改一个本来合法的请求。
	if cerr.Class != canonical.ClassUpstreamUnavailable {
		t.Fatalf("应分类为 upstream_unavailable，实际 %v", cerr)
	}
	// request_id 必须点名：这类响应在上游日志里只能靠它捞回来。
	if !strings.Contains(cerr.Message, "req-empty") {
		t.Errorf("错误必须带上 request_id 以便追踪，实际 %q", cerr.Message)
	}
	// 用量回调发生在交付路径上，未交付就不得计费。
	if usageCalls != 0 {
		t.Errorf("失败时不应调用 usage 回调，实际 %d 次", usageCalls)
	}
}

// TestNonStreamMissingOutputFailsClosed 钉死连 output 都没有的 200 同样 fail-closed。
//
// 与空数组是两种线格式形态（缺键 vs 空值），但下游后果相同，必须同样拒收。
func TestNonStreamMissingOutputFailsClosed(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"request_id":"req-async","task_id":"t-1","task_status":"PENDING"}`)
	}))
	defer up.Close()

	p, _ := newClockedProvider(t)
	req := chatRequest(t, up.URL)

	resp, err := p.translateNonStream(context.Background(), req, mustProject(t, req.Raw))
	if err == nil {
		t.Fatal("缺 output 的 200 必须 fail-closed")
	}
	if resp != nil {
		t.Errorf("失败时不得返回响应，实际 %+v", resp)
	}
	cerr := canonical.AsError(err)
	if cerr.Class != canonical.ClassUpstreamUnavailable {
		t.Fatalf("应分类为 upstream_unavailable，实际 %v", cerr)
	}
	if !strings.Contains(cerr.Message, "req-async") {
		t.Errorf("错误必须带上 request_id 以便追踪，实际 %q", cerr.Message)
	}
}
