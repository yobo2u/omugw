package dashscopenative

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/config"
	nativewire "github.com/yobo2u/omugw/internal/protocol/dashscopenative"
	"github.com/yobo2u/omugw/internal/protocol/openaichat"
	"github.com/yobo2u/omugw/internal/provider"
	"github.com/yobo2u/omugw/internal/transport/httpx"
	"github.com/yobo2u/omugw/internal/transport/sse"
)

// nativeSSEUpstream 是逐帧可控的 Native SSE 假上游。
//
// 逐帧 flush 而不是一次写完：一次性写出的话，「同步 transforming reader」与
// 「先攒完再转换」两种实现产出的字节完全一样，Read 契约的用例就什么都没钉住。
type nativeSSEUpstream struct {
	*httptest.Server

	mu   sync.Mutex
	seen upstreamCall
}

// upstreamCall 是假上游记下的一次出站调用。
//
// 路径与请求头一起留档：门是由 target 声明的部署事实，敲错门的实现只在路径上
// 露馅，而请求头断言对它一无所知。
type upstreamCall struct {
	path   string
	header http.Header
	body   []byte
	count  int
}

// upstreamHeaders 描述假上游要声明的响应头形态。
//
// 存在的理由只有一个：证明 translator **重写**了响应头。上游本来就说
// text/event-stream、本来就没有 Content-Length 时，一个根本没重写的实现照样能
// 让下游断言通过——那几条断言就什么都没钉住。
type upstreamHeaders struct {
	// contentType 为空时用 text/event-stream。
	contentType string

	// declareLength 让假上游按**实际**字节数声明 Content-Length。
	//
	// 长度必须精确：over 声明会让 net/http 挂着等永远不来的字节，under 声明会
	// 让它截断——那时测的是传输层的行为，不再是本层的头重写。
	declareLength bool
}

// newNativeSSEUpstream 按 Native 形态逐帧写出 datas。
func newNativeSSEUpstream(t *testing.T, datas []string) *nativeSSEUpstream {
	t.Helper()
	return newNativeSSEUpstreamWith(t, datas, upstreamHeaders{})
}

// newNativeSSEUpstreamWith 按 Native 形态逐帧写出 datas，并按 hdr 声明响应头：
// event:result + :HTTP_STATUS/200 注释行 + data 负载。注释行必须在，它是真实
// 上游的帧形态，顺带证明本层依赖的 sse.Reader 会跳过心跳注释。
func newNativeSSEUpstreamWith(t *testing.T, datas []string, hdr upstreamHeaders) *nativeSSEUpstream {
	t.Helper()
	frames := make([][]byte, 0, len(datas))
	total := 0
	for i, d := range datas {
		f := fmt.Appendf(nil, "id:%d\nevent:result\n:HTTP_STATUS/200\ndata:%s\n\n", i+1, compactFrame(t, d))
		frames = append(frames, f)
		total += len(f)
	}

	up := &nativeSSEUpstream{}
	up.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("读取出站请求体失败: %v", err)
			return
		}
		up.mu.Lock()
		up.seen = upstreamCall{path: r.URL.Path, header: r.Header.Clone(), body: body, count: up.seen.count + 1}
		up.mu.Unlock()

		f, ok := w.(http.Flusher)
		if !ok {
			t.Error("httptest 的 ResponseWriter 应支持 Flush")
			return
		}
		contentType := hdr.contentType
		if contentType == "" {
			contentType = "text/event-stream"
		}
		w.Header().Set("Content-Type", contentType)
		if hdr.declareLength {
			w.Header().Set("Content-Length", strconv.Itoa(total))
		}
		w.WriteHeader(http.StatusOK)
		for _, frame := range frames {
			if _, err := w.Write(frame); err != nil {
				t.Errorf("写出 SSE 帧失败: %v", err)
				return
			}
			f.Flush()
		}
	}))
	return up
}

// compactFrame 把用例里为可读性折行的 JSON 压成单行。
//
// SSE 的 data 字段一行一条，多行负载会被 sse.Reader 用 "\n" 拼回去——那不是
// 真实上游的形态，用例会因为自己造的换行而失败，看起来却像被测代码的锅。
// 刻意不吞压缩错误：畸形用例负载必须当场暴露，而不是伪装成上游故障。
func compactFrame(t *testing.T, data string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(data)); err != nil {
		// 故意畸形的负载（首帧畸形用例）原样发出。
		if strings.Contains(data, "\n") {
			t.Fatalf("畸形用例负载不得跨行: %q", data)
		}
		return data
	}
	return buf.String()
}

// request 返回上游收到的这次调用记录。
func (u *nativeSSEUpstream) request() upstreamCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.seen
}

// streamChatRequest 构造最小流式 Chat 出站请求。
func streamChatRequest(t *testing.T, baseURL string, includeUsage bool) provider.Request {
	t.Helper()
	raw := `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`
	if includeUsage {
		raw = `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true,` +
			`"stream_options":{"include_usage":true}}`
	}
	return streamChatRequestRaw(t, baseURL, raw)
}

// streamChatRequestN 构造带 n 的流式 Chat 出站请求。
func streamChatRequestN(t *testing.T, baseURL string, n int) provider.Request {
	t.Helper()
	return streamChatRequestRaw(t, baseURL, fmt.Sprintf(
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true,"n":%d}`, n))
}

// callTranslateStream 直接调用 translateStream 绕过公开 Call 的 rejectUnmappable 前置规则。
// 尽管当前公开入口拦截 stream+n>1，内部多候选状态机与校验机制仍作为深度防御保留，
// 并为未来重新开放提供测试覆盖，本 helper 供此类内部防御用例直测流式转换器。
func callTranslateStream(t *testing.T, p *Provider, req provider.Request) (*httpx.Response, error) {
	t.Helper()
	proj, err := openaichat.Project(req.Raw)
	if err != nil {
		t.Fatalf("openaichat.Project 失败: %v", err)
	}
	return p.translateStream(context.Background(), req, proj)
}

// streamChatRequestRaw 按给定 Chat 线格式构造流式出站请求，Raw 与 Canonical 同源。
func streamChatRequestRaw(t *testing.T, baseURL, raw string) provider.Request {
	t.Helper()
	req := chatRequestWithBody(t, baseURL, raw)
	req.Stream = true
	return req
}

// chatChunk 是下游 chat.completion.chunk 的结构化视图。
//
// 断言走结构化解码而不是 strings.Contains：子串断言分不清 `"index":1` 落在哪一条
// chunk 的哪一个候选上，两条候选的 delta 互换后每个子串仍然都在，测试照绿。
type chatChunk struct {
	ID      string           `json:"id"`
	Object  string           `json:"object"`
	Created int64            `json:"created"`
	Model   string           `json:"model"`
	Choices []chunkChoice    `json:"choices"`
	Usage   *completionUsage `json:"usage"`
}

type chunkChoice struct {
	Index        int             `json:"index"`
	Delta        chunkDelta      `json:"delta"`
	FinishReason *string         `json:"finish_reason"`
	Raw          json.RawMessage `json:"-"`
}

type chunkDelta struct {
	Role             string          `json:"role"`
	Content          string          `json:"content"`
	ReasoningContent string          `json:"reasoning_content"`
	ToolCalls        []chunkToolCall `json:"tool_calls"`
}

type chunkToolCall struct {
	Index    int            `json:"index"`
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Function *chunkFunction `json:"function"`
}

type chunkFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// parseChatStream 把转换后的字节拆成 chunk 序列，并报告是否以 [DONE] 收尾。
//
// 自己按 `data: ` 前缀拆而不是复用 sse.Reader：本层产出的分帧格式本身就是被测
// 对象，用生产解析器读它，一个把两条 chunk 挤进一帧的实现照样能被读回来。
func parseChatStream(t *testing.T, out []byte) (chunks []chatChunk, done bool) {
	t.Helper()
	s := string(out)
	if s == "" {
		return nil, false
	}
	// 每条事件必须是完整的 `data: <payload>\n\n`，末尾不留残帧。
	if !strings.HasSuffix(s, "\n\n") {
		t.Fatalf("SSE 输出必须以空行收尾，实际 %q", s)
	}
	for _, frame := range strings.Split(strings.TrimSuffix(s, "\n\n"), "\n\n") {
		payload, ok := strings.CutPrefix(frame, "data: ")
		if !ok {
			t.Fatalf("每条事件都应是 `data: ` 前缀的单行帧，实际 %q", frame)
		}
		if strings.Contains(payload, "\n") {
			t.Fatalf("chunk 负载不应跨行，实际 %q", payload)
		}
		if payload == "[DONE]" {
			if done {
				t.Fatalf("[DONE] 只应出现一次: %q", s)
			}
			done = true
			continue
		}
		if done {
			t.Fatalf("[DONE] 之后不得再有事件: %q", s)
		}
		var c chatChunk
		if err := json.Unmarshal([]byte(payload), &c); err != nil {
			t.Fatalf("chunk 不是合法 JSON: %v (payload=%s)", err, payload)
		}
		chunks = append(chunks, c)
	}
	return chunks, done
}

// readAllStream 读净并关闭转换后的响应体。
func readAllStream(t *testing.T, resp *httpx.Response) []byte {
	t.Helper()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取转换后流失败: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("关闭转换后流失败: %v", err)
	}
	return out
}

// TestStreamTransformBasic 钉死 Native SSE → Chat SSE：稳定标识、增量内容、[DONE] 收尾。
func TestStreamTransformBasic(t *testing.T) {
	up := newNativeSSEUpstream(t, []string{
		`{"output":{"choices":[{"finish_reason":"null","message":{"role":"assistant","content":"你"}}]},` +
			`"usage":{"input_tokens":5,"output_tokens":1},"request_id":"r"}`,
		`{"output":{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"好"}}]},` +
			`"usage":{"input_tokens":5,"output_tokens":2},"request_id":"r"}`,
	})
	defer up.Close()

	p, clk := newClockedProvider(t)
	req := streamChatRequest(t, up.URL, false)
	resp, err := p.Call(context.Background(), req)
	if err != nil {
		t.Fatalf("流式转换应成功: %v", err)
	}
	out := readAllStream(t, resp)
	chunks, done := parseChatStream(t, out)

	if !done {
		t.Fatalf("应以 [DONE] 收尾: %s", out)
	}
	// 一帧一条 chunk：帧数与 chunk 数必须一一对应，多发或合并都被这条挡住。
	if len(chunks) != 2 {
		t.Fatalf("两个 Native 帧应产出两条 chunk，实际 %d 条: %s", len(chunks), out)
	}
	// created 只求值一次：每条 chunk 各取一次时钟，客户端看到的时间戳会跳。
	wantCreated := clk.at(2).Unix()
	for i, c := range chunks {
		if c.Object != "chat.completion.chunk" {
			t.Errorf("chunk[%d].object 应为 chat.completion.chunk，实际 %q", i, c.Object)
		}
		if c.ID != "r" {
			t.Errorf("chunk[%d].id 应取上游 request_id，实际 %q", i, c.ID)
		}
		if c.Model != "qwen-plus" {
			t.Errorf("chunk[%d].model 应为上游模型名，实际 %q", i, c.Model)
		}
		if c.Created != wantCreated {
			t.Errorf("chunk[%d].created 应为注入时钟 %d，实际 %d", i, wantCreated, c.Created)
		}
		if len(c.Choices) != 1 {
			t.Fatalf("chunk[%d] 应恰有一个候选，实际 %d", i, len(c.Choices))
		}
		if c.Choices[0].Index != 0 {
			t.Errorf("chunk[%d] 候选序号应为 0，实际 %d", i, c.Choices[0].Index)
		}
	}
	if got := chunks[0].Choices[0].Delta.Content; got != "你" {
		t.Errorf("首帧增量应原样透传，实际 %q", got)
	}
	if got := chunks[1].Choices[0].Delta.Content; got != "好" {
		t.Errorf("次帧增量应原样透传，实际 %q", got)
	}
	// role 只发一次：每帧都发 role 会让部分客户端每帧新起一条消息。
	if got := chunks[0].Choices[0].Delta.Role; got != "assistant" {
		t.Errorf("首帧应发 role=assistant，实际 %q", got)
	}
	if got := chunks[1].Choices[0].Delta.Role; got != "" {
		t.Errorf("非首帧不得重复发 role，实际 %q", got)
	}
	if fr := chunks[0].Choices[0].FinishReason; fr != nil {
		t.Errorf("生成中的帧 finish_reason 应为 null，实际 %v", *fr)
	}
	if fr := chunks[1].Choices[0].FinishReason; fr == nil || *fr != "stop" {
		t.Errorf("末帧 finish_reason 应为 stop，实际 %v", fr)
	}
	// 未请求 include_usage 时不得夹带 usage chunk。
	for i, c := range chunks {
		if c.Usage != nil {
			t.Errorf("未请求 include_usage 时 chunk[%d] 不应带 usage: %+v", i, c.Usage)
		}
	}
}

// TestStreamOutboundRequest 钉死流式出站请求的增量开关、SSE 头与鉴权。
//
// incremental_output 漏了，上游每帧回全量文本，按 delta 逐帧拼接会把内容
// 重复放大成 O(n²)；SSE 头漏了，上游按非流式返回，整条流的语义变掉。
func TestStreamOutboundRequest(t *testing.T) {
	up := newNativeSSEUpstream(t, []string{
		`{"output":{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"x"}}]},` +
			`"request_id":"r"}`,
	})
	defer up.Close()

	p, _ := newClockedProvider(t)
	req := streamChatRequest(t, up.URL, false)
	req.Header = make(http.Header)
	req.Header.Set("X-DashScope-WorkSpace", "ws-1")
	req.Header.Set("X-Custom-Header", "custom")
	req.Header.Set("Authorization", "Bearer client-token")

	resp, err := p.Call(context.Background(), req)
	if err != nil {
		t.Fatalf("流式转换应成功: %v", err)
	}
	readAllStream(t, resp)

	got := up.request()
	if got.count != 1 {
		t.Fatalf("应恰好出门一次，实际 %d 次", got.count)
	}
	// 门由 target.NativeEndpoint 声明，路径必须逐字对上文本生成端点。期望值写死
	// 成字面量而不是引用生产常量：引用它等于拿被测代码算期望值，路径被改错时
	// 两边一起变，断言永远成立却什么都没证明。
	if got.path != "/api/v1/services/aigc/text-generation/generation" {
		t.Errorf("门 text-generation 应打到文本生成端点，实际 %q", got.path)
	}
	header, body := got.header, got.body
	var out nativeRequest
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("出站请求体不是合法 Native 信封: %v (body=%s)", err, body)
	}
	assertParam(t, out.Parameters, "incremental_output", true)
	assertParam(t, out.Parameters, "result_format", "message")
	if out.Model != "qwen-plus" {
		t.Errorf("出站 model 应改写为上游模型名，实际 %q", out.Model)
	}
	if v := header.Get("X-DashScope-SSE"); v != "enable" {
		t.Errorf("流式必须声明 X-DashScope-SSE: enable，实际 %q", v)
	}
	if v := header.Get("Accept"); v != "text/event-stream" {
		t.Errorf("流式出站 Accept 应为 text/event-stream，实际 %q", v)
	}
	if v := header.Get("Content-Type"); v != "application/json" {
		t.Errorf("出站 Content-Type 应为 application/json，实际 %q", v)
	}
	if v := header.Get("Authorization"); v != "Bearer sk-test" {
		t.Errorf("出站鉴权必须用网关自己的凭据，实际 %q", v)
	}
	if v := header.Get("X-DashScope-WorkSpace"); v != "ws-1" {
		t.Errorf("白名单语义头应透传，实际 %q", v)
	}
	if v := header.Get("X-Custom-Header"); v != "" {
		t.Errorf("白名单外的头不得透传，实际 %q", v)
	}
}

// TestStreamResponseIsEventStream 钉死返回的响应是无定长的 text/event-stream。
//
// 上游刻意声明 application/json 与一个精确的 Content-Length：两者都必须被
// translator 换掉，重写才真的被证明。上游本来就说 text/event-stream、本来就没有
// Content-Length 时，一个根本没重写的实现照样能让下面三条断言全过。
//
// 留着上游那个 Content-Length 的后果是实打实的：转换后的 Chat SSE 与 Native 原体
// 长度不同，下游会按旧长度截断，或挂起等一批永远不会来的字节。
func TestStreamResponseIsEventStream(t *testing.T) {
	up := newNativeSSEUpstreamWith(t, []string{
		`{"output":{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"x"}}]},` +
			`"request_id":"r"}`,
	}, upstreamHeaders{contentType: "application/json", declareLength: true})
	defer up.Close()

	p, clk := newClockedProvider(t)
	resp, err := p.Call(context.Background(), streamChatRequest(t, up.URL, false))
	if err != nil {
		t.Fatalf("流式转换应成功: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("关闭响应体失败: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("状态码应保持 200，实际 %d", resp.StatusCode)
	}
	// 三条断言各自钉住一行重写，缺一行就红：上游给的是 application/json、
	// 一个非空 Content-Length、以及一个非 -1 的 ContentLength。
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type 应被重写为 text/event-stream，实际 %q", ct)
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		t.Errorf("流式必须删掉上游的 Content-Length 头，实际 %q", cl)
	}
	if resp.ContentLength != -1 {
		t.Errorf("流式 ContentLength 应重置为 -1，实际 %d", resp.ContentLength)
	}
	// 上游时序是换凭据重试的依据，被覆盖成转换开始的时刻就失真了。
	if !resp.UpstreamFirstByte.Equal(clk.at(1)) {
		t.Errorf("UpstreamFirstByte 应保留为 %v，实际 %v", clk.at(1), resp.UpstreamFirstByte)
	}
	if resp.Latency != clockStep {
		t.Errorf("Latency 应保留为一个时钟步长 %v，实际 %v", clockStep, resp.Latency)
	}
}

// TestStreamReadNeverZeroNil 钉死 Read 永不返回 (0, nil)——空 delta 保活帧也得推进。
//
// 下游 relayStream 用 bufio.Scanner 再解析一次本层输出，连续空读会触发
// io.ErrNoProgress，变成一个语焉不详的流内错误。
func TestStreamReadNeverZeroNil(t *testing.T) {
	up := newNativeSSEUpstream(t, []string{
		`{"output":{"choices":[{"finish_reason":"null","message":{"role":"assistant","content":""}}]},` +
			`"usage":{"input_tokens":1,"output_tokens":0},"request_id":"r"}`,
		`{"output":{"choices":[{"finish_reason":"null","message":{"role":"assistant","content":""}}]},` +
			`"usage":{"input_tokens":1,"output_tokens":0},"request_id":"r"}`,
		`{"output":{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"x"}}]},` +
			`"usage":{"input_tokens":1,"output_tokens":1},"request_id":"r"}`,
	})
	defer up.Close()

	p, _ := newClockedProvider(t)
	resp, err := p.Call(context.Background(), streamChatRequest(t, up.URL, true))
	if err != nil {
		t.Fatalf("流式转换应成功: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("关闭响应体失败: %v", err)
		}
	}()

	buf := make([]byte, 1) // 任意小缓冲，强制分批
	var got bytes.Buffer
	for {
		n, err := resp.Body.Read(buf)
		if n == 0 && err == nil {
			t.Fatal("Read 不得返回 (0, nil)")
		}
		got.Write(buf[:n])
		if err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("正常流应以 io.EOF 收尾，实际 %v", err)
			}
			break
		}
	}
	// 逐字节读出来的内容必须仍是完整可解的流，否则分批交付丢了字节。
	chunks, done := parseChatStream(t, got.Bytes())
	if !done {
		t.Fatalf("逐字节读出的流应以 [DONE] 收尾: %s", got.Bytes())
	}
	// 三个 Native 帧 + 一条 usage chunk（本用例请求了 include_usage）。
	// 空 delta 帧被吞掉的实现会少一条，而那正是 (0, nil) 的来源。
	if len(chunks) != 4 {
		t.Fatalf("三帧加 usage chunk 应产出四条 chunk，实际 %d 条: %s", len(chunks), got.Bytes())
	}
	for i := range 3 {
		if len(chunks[i].Choices) != 1 {
			t.Errorf("chunk[%d] 应恰有一个候选，实际 %d", i, len(chunks[i].Choices))
		}
	}
}

// secondClock 是每次调用步进一整秒的确定性时钟。
//
// created 是 Unix 秒。共用 250ms 步长的 stepClock 时，「每条 chunk 各取一次时钟」
// 与「只取一次」在同一秒内产出相同的 created——那条稳定性断言因此什么都没钉住。
type secondClock struct {
	calls atomic.Int64
}

func (c *secondClock) Now() time.Time {
	n := c.calls.Add(1) - 1
	return clockBase.Add(time.Duration(n) * time.Second)
}

// TestStreamCreatedEvaluatedOnce 钉死 created 全流只求值一次。
//
// 每条 chunk 各取一次时钟的话，客户端看到的时间戳会一路往前跳，同一次回复
// 因此像是好几次不同时刻的响应。
func TestStreamCreatedEvaluatedOnce(t *testing.T) {
	up := newNativeSSEUpstream(t, []string{
		`{"output":{"choices":[{"finish_reason":"null","message":{"role":"assistant","content":"a"}}]},` +
			`"usage":{"input_tokens":1,"output_tokens":1},"request_id":"r"}`,
		`{"output":{"choices":[{"finish_reason":"null","message":{"role":"assistant","content":"b"}}]},` +
			`"usage":{"input_tokens":1,"output_tokens":2},"request_id":"r"}`,
		`{"output":{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"c"}}]},` +
			`"usage":{"input_tokens":1,"output_tokens":3},"request_id":"r"}`,
	})
	defer up.Close()

	clk := &secondClock{}
	p := New(httpx.New(config.Default().Timeouts, clk.Now), clk.Now)
	resp, err := p.Call(context.Background(), streamChatRequest(t, up.URL, true))
	if err != nil {
		t.Fatalf("流式转换应成功: %v", err)
	}
	chunks, done := parseChatStream(t, readAllStream(t, resp))
	if !done {
		t.Fatal("应以 [DONE] 收尾")
	}
	if len(chunks) < 4 {
		t.Fatalf("三帧加 usage chunk 应产出四条 chunk，实际 %d 条", len(chunks))
	}
	// usage chunk 也必须共用同一个 created，它同样是这次回复的一部分。
	first := chunks[0].Created
	for i, c := range chunks {
		if c.Created != first {
			t.Errorf("chunk[%d].created = %d，应与首条一致的 %d", i, c.Created, first)
		}
	}
}

// TestStreamTinyBufferPreservesBytes 钉死任意小缓冲下分批交付不丢字节、不重复。
func TestStreamTinyBufferPreservesBytes(t *testing.T) {
	frames := []string{
		`{"output":{"choices":[{"finish_reason":"null","message":{"role":"assistant","content":"一二三"}}]},` +
			`"usage":{"input_tokens":1,"output_tokens":1},"request_id":"r"}`,
		`{"output":{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"四"}}]},` +
			`"usage":{"input_tokens":1,"output_tokens":2},"request_id":"r"}`,
	}
	up := newNativeSSEUpstream(t, frames)
	defer up.Close()

	// 两次调用各用一个全新时钟：共用一个的话 created 会随调用递增，
	// 逐字节与整读的字节天然不同，比对就变成了比对时钟而不是比对分批交付。
	bulk, _ := newClockedProvider(t)
	whole, err := bulk.Call(context.Background(), streamChatRequest(t, up.URL, true))
	if err != nil {
		t.Fatalf("流式转换应成功: %v", err)
	}
	want := readAllStream(t, whole)

	p, _ := newClockedProvider(t)
	piecewise, err := p.Call(context.Background(), streamChatRequest(t, up.URL, true))
	if err != nil {
		t.Fatalf("流式转换应成功: %v", err)
	}
	defer func() {
		if err := piecewise.Body.Close(); err != nil {
			t.Errorf("关闭响应体失败: %v", err)
		}
	}()

	var got bytes.Buffer
	buf := make([]byte, 3) // 刻意小于一个 UTF-8 汉字，切在多字节字符中间
	for {
		n, err := piecewise.Body.Read(buf)
		got.Write(buf[:n])
		if err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("正常流应以 io.EOF 收尾，实际 %v", err)
			}
			break
		}
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Errorf("分批读出的字节应与整读一致\n分批: %q\n整读: %q", got.Bytes(), want)
	}
}

// TestStreamFirstFrameFailsInCall 钉死首帧异常一律在 Call 内报错（此时下游无首字节，可 failover）。
//
// 逐项钉死分类而不是只断言「有错」：畸形字节被记成 bad_request 会把客户端引去
// 改一个本来合法的请求，而真正的上游故障既不进故障统计也换不到另一个凭据重试。
func TestStreamFirstFrameFailsInCall(t *testing.T) {
	cases := []struct {
		name      string
		datas     []string
		wantClass canonical.ErrorClass
	}{
		{"畸形 JSON", []string{`{not-json`}, canonical.ClassUpstreamUnavailable},
		{"零候选", []string{`{"output":{"choices":[]},"request_id":"req-empty"}`}, canonical.ClassUpstreamUnavailable},
		// 错误信封走 DashScope 错误码分类：InvalidApiKey 换个凭据可能成功。
		{"错误信封", []string{`{"code":"InvalidApiKey","message":"bad key","request_id":"req-auth"}`}, canonical.ClassAuth},
		{"空流", nil, canonical.ClassUpstreamUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newNativeSSEUpstream(t, tc.datas)
			defer up.Close()

			p, _ := newClockedProvider(t)
			resp, err := p.Call(context.Background(), streamChatRequest(t, up.URL, true))
			if err == nil {
				t.Fatal("首帧异常必须在 Call 内报错")
			}
			// 失败时返回响应，调用方会以为拿到了可读的流，failover 分支永远走不到。
			if resp != nil {
				t.Errorf("失败时不得返回响应，实际 %+v", resp)
			}
			var cerr *canonical.Error
			if !errors.As(err, &cerr) {
				t.Fatalf("错误链应包含 *canonical.Error，实际 %T", err)
			}
			if cerr.Class != tc.wantClass {
				t.Errorf("应分类为 %s，实际 %v", tc.wantClass, cerr)
			}
		})
	}
}

// TestStreamFirstFrameCandidateCountMismatchFailsInCall 钉死上游首帧候选数与期望不符时在 Call 内报错，
// 防止上游在特定参数下静默将 n 压回 1 或返回多余候选导致语义丢失被误判为成功。
func TestStreamFirstFrameCandidateCountMismatchFailsInCall(t *testing.T) {
	for _, tc := range []struct {
		name string
		want int
		got  int
	}{
		{name: "上游少回", want: 2, got: 1},
		{name: "上游多回", want: 1, got: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			choices := strings.Repeat(`{"finish_reason":"stop","message":{"role":"assistant","content":"A"}},`, tc.got)
			choices = strings.TrimSuffix(choices, ",")
			up := newNativeSSEUpstream(t, []string{
				`{"output":{"choices":[` + choices + `]},"request_id":"r-mismatch"}`,
			})
			defer up.Close()

			p, _ := newClockedProvider(t)
			resp, err := callTranslateStream(t, p, streamChatRequestN(t, up.URL, tc.want))
			if err == nil {
				t.Fatalf("请求 %d 个候选而上游首帧返回 %d 个时必须报错", tc.want, tc.got)
			}
			if resp != nil {
				t.Errorf("候选数不符时不得返回响应，实际 %+v", resp)
			}
			if cerr := canonical.AsError(err); cerr.Class != canonical.ClassUpstreamUnavailable {
				t.Errorf("候选数不符应分类为 upstream_unavailable，实际 %v", cerr)
			}
		})
	}
}

// TestStreamFirstFrameZeroCandidatesWithZeroNRejectedInCall 钉死即便 n=0，首帧空 choices 也必须在 Call 内报错且关闭上游，
// 防止空 choices 被透传至首字节后导致无法 failover。
func TestStreamFirstFrameZeroCandidatesWithZeroNRejectedInCall(t *testing.T) {
	closed := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Error("httptest 的 ResponseWriter 应支持 Flush")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if _, err := io.WriteString(w, "event:result\n:HTTP_STATUS/200\ndata:{\"output\":{\"choices\":[]},\"request_id\":\"r-zero\"}\n\n"); err != nil {
			t.Errorf("写出 SSE 帧失败: %v", err)
			return
		}
		f.Flush()
		select {
		case <-r.Context().Done():
			close(closed)
		case <-time.After(30 * time.Second):
		}
	}))
	defer up.Close()

	p, _ := newClockedProvider(t)
	resp, err := p.Call(context.Background(), streamChatRequestN(t, up.URL, 0))
	if err == nil {
		t.Fatal("首帧零候选必须在 Call 内报错")
	}
	if resp != nil {
		t.Errorf("首帧零候选不得返回响应，实际 %+v", resp)
		if closeErr := resp.Body.Close(); closeErr != nil {
			t.Errorf("关闭意外返回的响应体失败: %v", closeErr)
		}
	}
	if cerr := canonical.AsError(err); cerr.Class != canonical.ClassUpstreamUnavailable {
		t.Errorf("首帧零候选应分类为 upstream_unavailable，实际 %v", cerr)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("首帧零候选报错后必须立即关闭上游 body，实际连接仍挂着")
	}
}

// TestStreamZeroNWithHealthyUpstreamSucceeds 钉死 n=0 且上游正常返回 1 个候选时流式调用成功，
// 防止将 wantChoices 误设为 0 导致健康上游被误判为 upstream_unavailable 并触发无谓的 failover 与重复计费。
func TestStreamZeroNWithHealthyUpstreamSucceeds(t *testing.T) {
	up := newNativeSSEUpstream(t, []string{
		`{"output":{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]},"request_id":"r-zero-n"}`,
	})
	defer up.Close()

	p, _ := newClockedProvider(t)
	resp, err := p.Call(context.Background(), streamChatRequestN(t, up.URL, 0))
	if err != nil {
		t.Fatalf("n=0 且上游返回 1 候选时流式调用应成功，实际报错: %v", err)
	}
	out := readAllStream(t, resp)
	chunks, done := parseChatStream(t, out)
	if !done {
		t.Fatalf("应以 [DONE] 收尾: %s", out)
	}
	if len(chunks) != 1 {
		t.Fatalf("应产出 1 条 chunk，实际 %d 条: %s", len(chunks), out)
	}
	if got := chunks[0].Choices[0].Delta.Content; got != "ok" {
		t.Errorf("增量内容应为 ok，实际 %q", got)
	}
}

// TestStreamFirstFrameFailureClosesUpstream 钉死首帧失败时上游连接被立即关闭。
//
// 漏关会把连接钉在池子里，故障上游因此一路把连接池吃干净，症状却只是
// 「所有请求都变慢了」。上游 handler 等自己的请求上下文被取消来证明这件事：
// 客户端未读完就关闭响应体，net/http 会断开连接并取消服务端上下文。
func TestStreamFirstFrameFailureClosesUpstream(t *testing.T) {
	closed := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Error("httptest 的 ResponseWriter 应支持 Flush")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if _, err := io.WriteString(w, "event:result\n:HTTP_STATUS/200\ndata:{not-json\n\n"); err != nil {
			t.Errorf("写出 SSE 帧失败: %v", err)
			return
		}
		f.Flush()
		// 兜底超时不可省：一旦被测代码回归成「不关 body」，没有它这个 handler
		// 会永远挂着，httptest 的 Close 随之死等——用例就从「干净地失败」退化
		// 成「CI 超时」，而超时不会告诉任何人原因。
		select {
		case <-r.Context().Done():
			close(closed)
		case <-time.After(30 * time.Second):
		}
	}))
	defer up.Close()

	p, _ := newClockedProvider(t)
	resp, err := p.Call(context.Background(), streamChatRequest(t, up.URL, true))
	if resp != nil {
		t.Errorf("首帧失败时不得返回响应，实际 %+v", resp)
		if closeErr := resp.Body.Close(); closeErr != nil {
			t.Errorf("关闭意外返回的响应体失败: %v", closeErr)
		}
	}
	if err == nil {
		t.Fatal("首帧畸形必须报错")
	}

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("首帧失败后必须立即关闭上游 body，实际连接仍挂着")
	}
}

// TestStreamCloseClosesUpstreamBodyOnce 钉死 Close 立即关闭上游 body 且可重复调用。
//
// 直接构造 transformReader 而不是走 httptest：真实连接的 Close 由 net/http 兜着，
// 测试既数不到次数、也证明不了「Close 不去读上游」。
func TestStreamCloseClosesUpstreamBodyOnce(t *testing.T) {
	body := &countingBody{reader: strings.NewReader("")}
	r := &transformReader{body: body}

	if err := r.Close(); err != nil {
		t.Fatalf("Close 应成功: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("重复 Close 应幂等且成功: %v", err)
	}
	if n := body.closes.Load(); n != 1 {
		t.Errorf("上游 body 应恰好被关闭一次，实际 %d 次", n)
	}
	// Close 不得顺带读上游：客户端已经断开，再读只会白等一个没人要的流。
	if n := body.reads.Load(); n != 0 {
		t.Errorf("Close 不得读取上游，实际读了 %d 次", n)
	}
}

// countingBody 是可数读写次数、可注入关闭错误的上游 body 替身。
type countingBody struct {
	reader   io.Reader
	closeErr error
	reads    atomic.Int64
	closes   atomic.Int64
}

func (c *countingBody) Read(p []byte) (int, error) {
	c.reads.Add(1)
	return c.reader.Read(p)
}

func (c *countingBody) Close() error {
	c.closes.Add(1)
	return c.closeErr
}

// TestStreamClosePropagatesCloseError 钉死 Close 把底层关闭错误原样上抛且只关一次。
//
// 吞掉它等于宣称连接干净收尾，而实际上这条连接处于异常状态——调用方据此把它
// 当成正常归还，坏连接因此留在池子里，下一个请求才撞上，症状与本次故障完全对不上。
func TestStreamClosePropagatesCloseError(t *testing.T) {
	wantErr := errors.New("连接被重置")
	body := &countingBody{reader: strings.NewReader(""), closeErr: wantErr}
	r := &transformReader{body: body}

	// 原始错误必须可 errors.Is 追回：只留一句中文描述，排查时无从判断是超时、
	// 重置还是 TLS 失败。
	if err := r.Close(); !errors.Is(err, wantErr) {
		t.Fatalf("首次 Close 应上抛底层错误 %v，实际 %v", wantErr, err)
	}
	// 幂等：重复 Close 不得再碰底层，也不得把同一个错误报第二遍——defer 与
	// 显式 Close 并存时，第二次报错会把一次正常收尾染成失败。
	if err := r.Close(); err != nil {
		t.Fatalf("重复 Close 应幂等且返回 nil，实际 %v", err)
	}
	if n := body.closes.Load(); n != 1 {
		t.Errorf("上游 body 应恰好被关闭一次，实际 %d 次", n)
	}
	if n := body.reads.Load(); n != 0 {
		t.Errorf("Close 不得读取上游，实际读了 %d 次", n)
	}
}

// TestCloseAfterFailurePreservesErrorMetadata 钉死「首帧失败 + 关闭也失败」时元数据不丢。
//
// 这条路径最容易被 canonical.Wrapf 悄悄掏空：Wrapf 只留 Class 与 Message，
// Retryable、UpstreamStatus/Code/RequestID、RetryAfter、RateLimit、Param 全变零值。
// 于是一个带 Retry-After 的 429 恰好在「连关闭都失败」的时刻退化成没有退避信息、
// request_id 也查不回来的错误——而这正是最需要这些字段的时刻：客户端不知道该等
// 多久，运维也无法凭 request_id 向上游提工单。
func TestCloseAfterFailurePreservesErrorMetadata(t *testing.T) {
	closeErr := errors.New("连接被重置")
	body := &countingBody{reader: strings.NewReader(""), closeErr: closeErr}

	// 主因自带一个底层 cause：浅拷贝会把未导出的 cause 一并带走，而用
	// canonical.Newf 重造的实现会把整条 cause 链掐断——排查时只剩一句中文描述，
	// 判不出到底是超时、重置还是 TLS 失败。
	underlying := errors.New("上游连接被限流器切断")
	// 每个字段都取非零值：留零值的字段无法区分「被保住了」与「本来就是零」。
	cause := canonical.Wrapf(underlying, canonical.ClassRateLimit, "rate limit")
	cause.Retryable = true
	cause.UpstreamStatus = http.StatusTooManyRequests
	cause.UpstreamCode = "Throttling.RateQuota"
	cause.UpstreamRequestID = "req-429"
	cause.RetryAfter = 7 * time.Second
	cause.RateLimit = &canonical.RateLimitInfo{
		LimitRequests:     100,
		RemainingRequests: 0,
		ResetRequests:     3 * time.Second,
		LimitTokens:       5000,
		RemainingTokens:   12,
		ResetTokens:       4 * time.Second,
	}
	cause.Param = "model"

	err := closeAfterFailure(body, cause)
	var got *canonical.Error
	if !errors.As(err, &got) {
		t.Fatalf("应返回 *canonical.Error，实际 %T: %v", err, err)
	}

	// 分类不得被关闭失败改写：改成 upstream_unavailable 会让限流看起来像上游宕机，
	// 凭据池的冷却策略也随之走错分支。
	if got.Class != canonical.ClassRateLimit {
		t.Errorf("主因分类应保持 rate_limit，实际 %q", got.Class)
	}
	if !got.Retryable {
		t.Error("Retryable 应保持 true，丢了它限流就换不到另一个凭据重试")
	}
	if got.UpstreamStatus != http.StatusTooManyRequests {
		t.Errorf("UpstreamStatus 应保持 429，实际 %d", got.UpstreamStatus)
	}
	if got.UpstreamCode != "Throttling.RateQuota" {
		t.Errorf("UpstreamCode 应保留，实际 %q", got.UpstreamCode)
	}
	if got.UpstreamRequestID != "req-429" {
		t.Errorf("UpstreamRequestID 应保留，实际 %q", got.UpstreamRequestID)
	}
	if got.RetryAfter != 7*time.Second {
		t.Errorf("RetryAfter 应保留，实际 %v", got.RetryAfter)
	}
	if got.Param != "model" {
		t.Errorf("Param 应保留，实际 %q", got.Param)
	}
	if got.RateLimit == nil {
		t.Fatal("RateLimit 应保留，丢了它下游拿不到额度头")
	}
	if *got.RateLimit != *cause.RateLimit {
		t.Errorf("RateLimit 应逐项保留，实际 %+v", *got.RateLimit)
	}

	// 关闭失败必须在消息里留痕：完全不提它，连接层的异常就再没有任何记录。
	if !strings.Contains(got.Message, "rate limit") {
		t.Errorf("消息应保留主因，实际 %q", got.Message)
	}
	if !strings.Contains(got.Message, "关闭上游响应") {
		t.Errorf("消息应点明关闭也失败，实际 %q", got.Message)
	}
	// cause 链必须整条保留：拷贝换了指针是必然的，但底层原因不能因此断掉，
	// 否则排查时只剩一句中文描述，判不出到底是超时、重置还是 TLS 失败。
	if !errors.Is(err, underlying) {
		t.Error("应能 errors.Is 追回主因的底层错误")
	}
	if n := body.closes.Load(); n != 1 {
		t.Errorf("上游 body 应恰好被关闭一次，实际 %d 次", n)
	}
}

// TestCloseAfterFailureNormalizesNonCanonicalCause 钉死非 canonical 主因也被归一且可追回。
func TestCloseAfterFailureNormalizesNonCanonicalCause(t *testing.T) {
	closeErr := errors.New("连接被重置")
	cause := errors.New("裸错误")
	body := &countingBody{reader: strings.NewReader(""), closeErr: closeErr}

	err := closeAfterFailure(body, cause)
	var got *canonical.Error
	if !errors.As(err, &got) {
		t.Fatalf("应返回 *canonical.Error，实际 %T: %v", err, err)
	}
	// 来源不明的错误按 internal 且不可重试处理：重试只会放大故障。
	if got.Class != canonical.ClassInternal {
		t.Errorf("裸错误应归一为 internal，实际 %q", got.Class)
	}
	if got.Retryable {
		t.Error("来源不明的错误不得可重试")
	}
	if !strings.Contains(got.Message, "关闭上游响应") {
		t.Errorf("消息应点明关闭也失败，实际 %q", got.Message)
	}
	if !errors.Is(err, cause) {
		t.Error("应能 errors.Is 追回原始主因")
	}
	if n := body.closes.Load(); n != 1 {
		t.Errorf("上游 body 应恰好被关闭一次，实际 %d 次", n)
	}
}

// TestCloseAfterFailureCloseSucceeds 钉死关闭成功时主因原样返回，不被改写。
func TestCloseAfterFailureCloseSucceeds(t *testing.T) {
	body := &countingBody{reader: strings.NewReader("")}
	cause := canonical.Newf(canonical.ClassUpstreamUnavailable, "首帧畸形")

	err := closeAfterFailure(body, cause)
	// 同一个指针：关闭成功时连拷贝都不该发生，任何改写都是多余的失真。
	if err != error(cause) {
		t.Errorf("关闭成功时应原样返回主因，实际 %v", err)
	}
	if n := body.closes.Load(); n != 1 {
		t.Errorf("上游 body 应恰好被关闭一次，实际 %d 次", n)
	}
}

// TestStreamFirstFrameUnexpectedEventFailsClosed 钉死非 result 的具名事件 fail-closed。
func TestStreamFirstFrameUnexpectedEventFailsClosed(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event:error\ndata:{\"code\":\"Throttling\",\"message\":\"busy\"}\n\n")
	}))
	defer up.Close()

	p, _ := newClockedProvider(t)
	resp, err := p.Call(context.Background(), streamChatRequest(t, up.URL, true))
	if err == nil {
		t.Fatal("非 result 事件必须 fail-closed")
	}
	if resp != nil {
		t.Errorf("失败时不得返回响应，实际 %+v", resp)
	}
	cerr := canonical.AsError(err)
	if cerr.Class != canonical.ClassUpstreamUnavailable && cerr.Class != canonical.ClassRateLimit {
		t.Fatalf("应归为上游侧错误，实际 %v", cerr)
	}
}

// TestStreamUpstreamErrorStatusFailover 钉死上游非 2xx 在 Call 内解码为可 failover 的错误。
func TestStreamUpstreamErrorStatusFailover(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"code":"Throttling","message":"rate limit","request_id":"req-429"}`)
	}))
	defer up.Close()

	p, _ := newClockedProvider(t)
	resp, err := p.Call(context.Background(), streamChatRequest(t, up.URL, true))
	if err == nil {
		t.Fatal("上游 429 应返回错误")
	}
	if resp != nil {
		t.Errorf("失败时不得返回响应，实际 %+v", resp)
	}
	cerr := canonical.AsError(err)
	if cerr.Class != canonical.ClassRateLimit {
		t.Fatalf("应分类为 rate_limit，实际 %v", cerr)
	}
	if !cerr.Retryable {
		t.Errorf("限流必须可 failover，实际 Retryable=false")
	}
	if cerr.UpstreamRequestID != "req-429" {
		t.Errorf("应保留 request_id，实际 %q", cerr.UpstreamRequestID)
	}
}

// TestStreamUnknownDoorFailsClosed 钉死未知门在出门前 fail-closed。
func TestStreamUnknownDoorFailsClosed(t *testing.T) {
	up := newNativeSSEUpstream(t, []string{
		`{"output":{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"x"}}]},` +
			`"request_id":"r"}`,
	})
	defer up.Close()

	p, _ := newClockedProvider(t)
	req := streamChatRequest(t, up.URL, false)
	req.Target.NativeEndpoint = "unknown-door"

	resp, err := p.Call(context.Background(), req)
	if err == nil {
		t.Fatal("未知门必须报错")
	}
	if resp != nil {
		t.Errorf("失败时不得返回响应，实际 %+v", resp)
	}
	if cerr := canonical.AsError(err); cerr.Class != canonical.ClassInternal {
		t.Fatalf("门是部署事实，应分类为 internal，实际 %v", cerr)
	}
	if got := up.request(); got.count != 0 {
		t.Errorf("未知门不得触达上游，实际出门 %d 次", got.count)
	}
}

// TestStreamMultiCandidate 钉死一帧一条 chunk、每候选一个 choices 条目、全 finish 后才 [DONE]。
func TestStreamMultiCandidate(t *testing.T) {
	up := newNativeSSEUpstream(t, []string{
		`{"output":{"choices":[
		   {"finish_reason":"null","message":{"role":"assistant","content":"A1"}},
		   {"finish_reason":"null","message":{"role":"assistant","content":"B1"}}]},
		   "usage":{"input_tokens":5,"output_tokens":2},"request_id":"r"}`,
		`{"output":{"choices":[
		   {"finish_reason":"null","message":{"role":"assistant","content":"A2"}},
		   {"finish_reason":"stop","message":{"role":"assistant","content":"B2"}}]},
		   "usage":{"input_tokens":5,"output_tokens":4},"request_id":"r"}`,
		`{"output":{"choices":[
		   {"finish_reason":"stop","message":{"role":"assistant","content":"A3"}},
		   {"finish_reason":"null","message":{"role":"assistant","content":""}}]},
		   "usage":{"input_tokens":5,"output_tokens":6},"request_id":"r"}`,
	})
	defer up.Close()

	p, _ := newClockedProvider(t)
	resp, err := callTranslateStream(t, p, streamChatRequestN(t, up.URL, 2))
	if err != nil {
		t.Fatalf("多候选流式转换应成功: %v", err)
	}
	out := readAllStream(t, resp)
	chunks, done := parseChatStream(t, out)

	if !done {
		t.Fatalf("全部候选 finish 后应有 [DONE]: %s", out)
	}
	// 一帧一条 chunk：按候选拆成多条会让帧数翻倍，这条断言是唯一的拦截点。
	if len(chunks) != 3 {
		t.Fatalf("三个 Native 帧应产出三条 chunk，实际 %d 条: %s", len(chunks), out)
	}

	// 逐条核对候选序号与内容的配对：序号错乱时两条候选的增量会张冠李戴，
	// 而每个子串仍然「都在 body 里」。
	wantContent := [3][2]string{{"A1", "B1"}, {"A2", "B2"}, {"A3", ""}}
	for i, c := range chunks {
		if len(c.Choices) != 2 {
			t.Fatalf("chunk[%d] 应有两个候选条目，实际 %d", i, len(c.Choices))
		}
		for j, ch := range c.Choices {
			if ch.Index != j {
				t.Errorf("chunk[%d].choices[%d].index = %d，应为数组下标 %d", i, j, ch.Index, j)
			}
			if ch.Delta.Content != wantContent[i][j] {
				t.Errorf("chunk[%d] 候选 %d 增量 = %q，期望 %q", i, j, ch.Delta.Content, wantContent[i][j])
			}
			// role 只在各候选自己的首帧出现，与另一候选无关。
			wantRole := ""
			if i == 0 {
				wantRole = "assistant"
			}
			if ch.Delta.Role != wantRole {
				t.Errorf("chunk[%d] 候选 %d 的 role = %q，期望 %q", i, j, ch.Delta.Role, wantRole)
			}
		}
	}

	// 候选 1 在第二帧就 stop，候选 0 到第三帧才 stop——[DONE] 必须等最后一个。
	if fr := chunks[1].Choices[1].FinishReason; fr == nil || *fr != "stop" {
		t.Errorf("候选 1 应在第二帧 finish，实际 %v", fr)
	}
	if fr := chunks[1].Choices[0].FinishReason; fr != nil {
		t.Errorf("候选 0 第二帧仍在生成，finish_reason 应为 null，实际 %q", *fr)
	}
	if fr := chunks[2].Choices[0].FinishReason; fr == nil || *fr != "stop" {
		t.Errorf("候选 0 应在第三帧 finish，实际 %v", fr)
	}
}

// TestStreamPartialFinishHasNoDone 钉死只有部分候选 finish 时不得合成 [DONE]。
//
// 替其余候选编造结束，等于替模型宣布它没说过的结束：客户端把半截回复当完整的收下。
func TestStreamPartialFinishHasNoDone(t *testing.T) {
	up := newNativeSSEUpstream(t, []string{
		`{"output":{"choices":[
		   {"finish_reason":"null","message":{"role":"assistant","content":"A"}},
		   {"finish_reason":"null","message":{"role":"assistant","content":"B"}}]},"request_id":"r"}`,
		`{"output":{"choices":[
		   {"finish_reason":"stop","message":{"role":"assistant","content":"A2"}},
		   {"finish_reason":"null","message":{"role":"assistant","content":"B2"}}]},"request_id":"r"}`,
	})
	defer up.Close()

	p, _ := newClockedProvider(t)
	resp, err := callTranslateStream(t, p, streamChatRequestN(t, up.URL, 2))
	if err != nil {
		t.Fatalf("流式转换应成功: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("关闭响应体失败: %v", err)
		}
	}()

	out, err := io.ReadAll(resp.Body)
	if err == nil {
		t.Fatalf("候选未全部 finish 就断流应产生流内错误，实际读净: %s", out)
	}
	var cerr *canonical.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("错误链应包含 *canonical.Error，实际 %T", err)
	}
	if cerr.Class != canonical.ClassUpstreamUnavailable {
		t.Errorf("截断的流应分类为 upstream_unavailable，实际 %v", cerr)
	}
	if strings.Contains(string(out), "[DONE]") {
		t.Errorf("不得为未 finish 的候选合成 [DONE]: %s", out)
	}
}

// TestStreamCandidateCountChangeIsError 钉死帧内候选数变化按流内错误处理。
//
// 缩水时补齐会凭空造出候选，扩张时新候选没有 role 历史——两种猜测都让客户端
// 收到一份自相矛盾却语法合法的流。
func TestStreamCandidateCountChangeIsError(t *testing.T) {
	head := `{"output":{"choices":[
		   {"finish_reason":"null","message":{"role":"assistant","content":"A"}},
		   {"finish_reason":"null","message":{"role":"assistant","content":"B"}}]},"request_id":"r"}`
	cases := []struct {
		name   string
		second string
	}{
		{"缩水", `{"output":{"choices":[
		   {"finish_reason":"stop","message":{"role":"assistant","content":"A"}}]},"request_id":"r"}`},
		{"扩张", `{"output":{"choices":[
		   {"finish_reason":"stop","message":{"role":"assistant","content":"A"}},
		   {"finish_reason":"stop","message":{"role":"assistant","content":"B"}},
		   {"finish_reason":"stop","message":{"role":"assistant","content":"C"}}]},"request_id":"r"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newNativeSSEUpstream(t, []string{head, tc.second})
			defer up.Close()

			p, _ := newClockedProvider(t)
			resp, err := callTranslateStream(t, p, streamChatRequestN(t, up.URL, 2))
			if err != nil {
				t.Fatalf("首帧正常，Call 应成功: %v", err)
			}
			defer func() {
				if err := resp.Body.Close(); err != nil {
					t.Errorf("关闭响应体失败: %v", err)
				}
			}()

			out, err := io.ReadAll(resp.Body)
			if err == nil {
				t.Fatalf("候选数变化应产生流内错误，实际读净: %s", out)
			}
			var cerr *canonical.Error
			if !errors.As(err, &cerr) {
				t.Fatalf("错误链应包含 *canonical.Error，实际 %T", err)
			}
			if cerr.Class != canonical.ClassUpstreamUnavailable {
				t.Errorf("契约违例应分类为 upstream_unavailable，实际 %v", cerr)
			}
			// 首帧已经产出，错误必须发生在它之后而不是把整条流吞掉。
			if !strings.Contains(string(out), `"A"`) {
				t.Errorf("出错前的正常帧仍应交付，实际 %s", out)
			}
			if strings.Contains(string(out), "[DONE]") {
				t.Errorf("出错时不得合成 [DONE]: %s", out)
			}
		})
	}
}

// TestStreamUsageChunkOnlyWhenRequested 钉死 usage chunk 只在 include_usage 时输出。
func TestStreamUsageChunkOnlyWhenRequested(t *testing.T) {
	frames := []string{
		`{"output":{"choices":[{"finish_reason":"null","message":{"role":"assistant","content":"你"}}]},` +
			`"usage":{"input_tokens":5,"output_tokens":1},"request_id":"r"}`,
		`{"output":{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"好"}}]},` +
			`"usage":{"input_tokens":5,"output_tokens":9,"prompt_tokens_details":{"cached_tokens":2}},"request_id":"r"}`,
	}

	t.Run("include_usage=true", func(t *testing.T) {
		up := newNativeSSEUpstream(t, frames)
		defer up.Close()

		p, _ := newClockedProvider(t)
		resp, err := p.Call(context.Background(), streamChatRequest(t, up.URL, true))
		if err != nil {
			t.Fatalf("流式转换应成功: %v", err)
		}
		out := readAllStream(t, resp)
		chunks, done := parseChatStream(t, out)
		if !done {
			t.Fatalf("应以 [DONE] 收尾: %s", out)
		}
		if len(chunks) != 3 {
			t.Fatalf("两帧加一条 usage chunk 应为三条，实际 %d 条: %s", len(chunks), out)
		}
		// usage chunk 必须紧邻 [DONE] 之前，且 choices 为空——OpenAI 客户端
		// 按这个形态识别它，夹带候选会被当成又一段增量内容。
		last := chunks[2]
		if len(last.Choices) != 0 {
			t.Errorf("usage chunk 的 choices 应为空，实际 %+v", last.Choices)
		}
		if last.Usage == nil {
			t.Fatalf("usage chunk 必须带 usage: %s", out)
		}
		// 取末帧累计值：取首帧或求和都会把用量算错，而计费就靠这个数。
		if last.Usage.PromptTokens != 5 || last.Usage.CompletionTokens != 9 || last.Usage.TotalTokens != 14 {
			t.Errorf("usage 应取末帧累计值 in=5/out=9，实际 %+v", last.Usage)
		}
		// 内容 chunk 不得夹带 usage：客户端会把中途的累计值当成最终账单。
		for i := range 2 {
			if chunks[i].Usage != nil {
				t.Errorf("内容 chunk[%d] 不应带 usage: %+v", i, chunks[i].Usage)
			}
		}
	})

	t.Run("include_usage=false", func(t *testing.T) {
		up := newNativeSSEUpstream(t, frames)
		defer up.Close()

		p, _ := newClockedProvider(t)
		resp, err := p.Call(context.Background(), streamChatRequest(t, up.URL, false))
		if err != nil {
			t.Fatalf("流式转换应成功: %v", err)
		}
		out := readAllStream(t, resp)
		chunks, done := parseChatStream(t, out)
		if !done {
			t.Fatalf("应以 [DONE] 收尾: %s", out)
		}
		if len(chunks) != 2 {
			t.Fatalf("未请求 usage 时应只有两条内容 chunk，实际 %d 条: %s", len(chunks), out)
		}
		for i, c := range chunks {
			if c.Usage != nil {
				t.Errorf("未请求 include_usage 时 chunk[%d] 不应带 usage: %+v", i, c.Usage)
			}
		}
	})

	t.Run("上游未给 usage", func(t *testing.T) {
		up := newNativeSSEUpstream(t, []string{
			`{"output":{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"x"}}]},` +
				`"request_id":"r"}`,
		})
		defer up.Close()

		p, _ := newClockedProvider(t)
		resp, err := p.Call(context.Background(), streamChatRequest(t, up.URL, true))
		if err != nil {
			t.Fatalf("流式转换应成功: %v", err)
		}
		out := readAllStream(t, resp)
		chunks, done := parseChatStream(t, out)
		if !done {
			t.Fatalf("应以 [DONE] 收尾: %s", out)
		}
		// 上游没给用量就一个字都不能编：硬造一份「0 token 且权威」的记录，
		// 等于把一次未知用量的调用计成免费。
		if len(chunks) != 1 {
			t.Fatalf("上游无 usage 时不应伪造 usage chunk，实际 %d 条: %s", len(chunks), out)
		}
		if chunks[0].Usage != nil {
			t.Errorf("不应伪造 usage: %+v", chunks[0].Usage)
		}
	})
}

// TestStreamUsageCallbackPerFrame 钉死每成功解码一帧带 usage 就同步回调一次，末值为最终值。
func TestStreamUsageCallbackPerFrame(t *testing.T) {
	up := newNativeSSEUpstream(t, []string{
		`{"output":{"choices":[{"finish_reason":"null","message":{"role":"assistant","content":"a"}}]},` +
			`"usage":{"input_tokens":5,"output_tokens":1},"request_id":"r"}`,
		// 中间一帧不带 usage：不得为它编一份，也不得因此漏掉后面那帧。
		`{"output":{"choices":[{"finish_reason":"null","message":{"role":"assistant","content":"b"}}]},` +
			`"request_id":"r"}`,
		`{"output":{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"c"}}]},` +
			`"usage":{"input_tokens":5,"output_tokens":7,"output_tokens_details":{"reasoning_tokens":3}},"request_id":"r"}`,
	})
	defer up.Close()

	p, _ := newClockedProvider(t)
	var got []canonical.Usage
	req := streamChatRequest(t, up.URL, false)
	req.OnDashScopeUsage = func(u canonical.Usage) { got = append(got, u) }

	resp, err := p.Call(context.Background(), req)
	if err != nil {
		t.Fatalf("流式转换应成功: %v", err)
	}
	readAllStream(t, resp)

	if len(got) != 2 {
		t.Fatalf("带 usage 的两帧应各回调一次，实际 %d 次: %+v", len(got), got)
	}
	if got[0].InputTokens != 5 || got[0].OutputTokens != 1 {
		t.Errorf("首次回调应为首帧累计值 in=5/out=1，实际 %+v", got[0])
	}
	last := got[len(got)-1]
	if last.OutputTokens != 7 || last.ReasoningTokens != 3 {
		t.Errorf("末次回调应为末帧累计值 out=7/reasoning=3，实际 %+v", last)
	}
	if last.Fidelity != canonical.FidelityAuthoritative {
		t.Errorf("上游给的数字可用于计费，应为 authoritative，实际 %q", last.Fidelity)
	}
}

// TestStreamToolCallFragments 钉死跨帧工具参数：ID/name 只发一次，片段原样续发。
//
// 片段被提前解析、闭合或归一，一次本来能跨帧拼完整的调用就被毁了；而每帧重发
// ID 会让客户端把同一次调用当成多次。两者都不会报错，只会让工具调用悄悄失效。
func TestStreamToolCallFragments(t *testing.T) {
	up := newNativeSSEUpstream(t, []string{
		// 后续帧刻意**重复**上游的 id 与 name：这是必须被抑制的那一份。上游
		// 只在首帧给身份时，一个每帧都转发身份的实现照样能通过断言——那条
		// 断言就什么都没钉住。
		`{"output":{"choices":[{"finish_reason":"null","message":{"role":"assistant","content":"",
		   "tool_calls":[{"id":"call_9","function":{"name":"get_weather","arguments":"{\"ci"}}]}}]},"request_id":"r"}`,
		`{"output":{"choices":[{"finish_reason":"null","message":{"role":"assistant","content":"",
		   "tool_calls":[{"id":"call_9","function":{"name":"get_weather","arguments":"ty\":\"h"}}]}}]},"request_id":"r"}`,
		`{"output":{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":"",
		   "tool_calls":[{"id":"call_9","function":{"name":"get_weather","arguments":"z\"}"}}]}}]},"request_id":"r"}`,
	})
	defer up.Close()

	p, _ := newClockedProvider(t)
	resp, err := p.Call(context.Background(), streamChatRequest(t, up.URL, false))
	if err != nil {
		t.Fatalf("流式转换应成功: %v", err)
	}
	out := readAllStream(t, resp)
	chunks, done := parseChatStream(t, out)
	if !done {
		t.Fatalf("应以 [DONE] 收尾: %s", out)
	}
	if len(chunks) != 3 {
		t.Fatalf("三个 Native 帧应产出三条 chunk，实际 %d 条: %s", len(chunks), out)
	}

	// 片段逐字拼回来必须等于上游发的完整参数，中途多一个字节都不行。
	wantFragments := []string{`{"ci`, `ty":"h`, `z"}`}
	var joined strings.Builder
	for i, c := range chunks {
		if len(c.Choices) != 1 {
			t.Fatalf("chunk[%d] 应恰有一个候选，实际 %d", i, len(c.Choices))
		}
		calls := c.Choices[0].Delta.ToolCalls
		if len(calls) != 1 {
			t.Fatalf("chunk[%d] 应恰有一次工具调用增量，实际 %d", i, len(calls))
		}
		call := calls[0]
		if call.Index != 0 {
			t.Errorf("chunk[%d] 工具序号应为数组下标 0，实际 %d", i, call.Index)
		}
		if call.Function == nil {
			t.Fatalf("chunk[%d] 工具增量应带 function: %s", i, out)
		}
		if call.Function.Arguments != wantFragments[i] {
			t.Errorf("chunk[%d] 参数片段 = %q，应原样透传 %q", i, call.Function.Arguments, wantFragments[i])
		}
		joined.WriteString(call.Function.Arguments)

		// ID 与 name 一律取上游原值且只在该调用首帧出现。
		if i == 0 {
			if call.ID != "call_9" || call.Function.Name != "get_weather" {
				t.Errorf("首帧应带上游原始 ID 与名称，实际 id=%q name=%q", call.ID, call.Function.Name)
			}
			if call.Type != "function" {
				t.Errorf("首帧工具增量 type 应为 function，实际 %q", call.Type)
			}
			continue
		}
		if call.ID != "" || call.Function.Name != "" {
			t.Errorf("chunk[%d] 不得重复发工具 ID/名称，实际 id=%q name=%q", i, call.ID, call.Function.Name)
		}
	}
	if got := joined.String(); got != `{"city":"hz"}` {
		t.Errorf("跨帧片段拼回的参数 = %q，期望 %q", got, `{"city":"hz"}`)
	}
	if fr := chunks[2].Choices[0].FinishReason; fr == nil || *fr != "tool_calls" {
		t.Errorf("末帧 finish_reason 应为 tool_calls，实际 %v", fr)
	}
}

// TestStreamToolCallArgumentsNotValidated 钉死永不闭合的参数片段照样原样交付。
//
// 上游中途截断时，网关不得替它判「这不是合法 JSON」而丢帧：那会把一次可诊断的
// 上游截断变成客户端看不到任何工具调用的静默失败。
func TestStreamToolCallArgumentsNotValidated(t *testing.T) {
	up := newNativeSSEUpstream(t, []string{
		`{"output":{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":"",
		   "tool_calls":[{"id":"call_1","function":{"name":"f","arguments":"{invalid"}}]}}]},"request_id":"r"}`,
	})
	defer up.Close()

	p, _ := newClockedProvider(t)
	resp, err := p.Call(context.Background(), streamChatRequest(t, up.URL, false))
	if err != nil {
		t.Fatalf("未闭合参数不应让 Call 失败: %v", err)
	}
	out := readAllStream(t, resp)
	chunks, done := parseChatStream(t, out)
	if !done || len(chunks) != 1 {
		t.Fatalf("应产出一条 chunk 并以 [DONE] 收尾: %s", out)
	}
	calls := chunks[0].Choices[0].Delta.ToolCalls
	if len(calls) != 1 || calls[0].Function == nil {
		t.Fatalf("工具调用增量应被交付: %s", out)
	}
	if calls[0].Function.Arguments != "{invalid" {
		t.Errorf("未闭合片段应原样交付，实际 %q", calls[0].Function.Arguments)
	}
}

// TestStreamMultiToolCallsPerCandidate 钉死同一候选的多个工具各自按下标保持身份。
func TestStreamMultiToolCallsPerCandidate(t *testing.T) {
	up := newNativeSSEUpstream(t, []string{
		`{"output":{"choices":[{"finish_reason":"null","message":{"role":"assistant","content":"",
		   "tool_calls":[
		     {"id":"call_a","function":{"name":"fa","arguments":"{\"x\""}},
		     {"id":"call_b","function":{"name":"fb","arguments":"{\"y\""}}]}}]},"request_id":"r"}`,
		// 次帧同样重复上游身份，逼实现自己记住「已经发过」。
		`{"output":{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":"",
		   "tool_calls":[
		     {"id":"call_a","function":{"name":"fa","arguments":":1}"}},
		     {"id":"call_b","function":{"name":"fb","arguments":":2}"}}]}}]},"request_id":"r"}`,
	})
	defer up.Close()

	p, _ := newClockedProvider(t)
	resp, err := p.Call(context.Background(), streamChatRequest(t, up.URL, false))
	if err != nil {
		t.Fatalf("流式转换应成功: %v", err)
	}
	out := readAllStream(t, resp)
	chunks, done := parseChatStream(t, out)
	if !done || len(chunks) != 2 {
		t.Fatalf("应产出两条 chunk 并以 [DONE] 收尾: %s", out)
	}

	first := chunks[0].Choices[0].Delta.ToolCalls
	if len(first) != 2 {
		t.Fatalf("首帧应有两次工具调用增量，实际 %d", len(first))
	}
	// 序号与身份必须一一对应：错位会把 fa 的参数续到 fb 上。
	if first[0].Index != 0 || first[0].ID != "call_a" || first[0].Function.Name != "fa" {
		t.Errorf("工具 0 身份错误: %+v", first[0])
	}
	if first[1].Index != 1 || first[1].ID != "call_b" || first[1].Function.Name != "fb" {
		t.Errorf("工具 1 身份错误: %+v", first[1])
	}

	second := chunks[1].Choices[0].Delta.ToolCalls
	if len(second) != 2 {
		t.Fatalf("次帧应有两次工具调用增量，实际 %d", len(second))
	}
	for i, want := range []string{":1}", ":2}"} {
		if second[i].Index != i {
			t.Errorf("次帧工具 %d 序号 = %d", i, second[i].Index)
		}
		if second[i].ID != "" || second[i].Function.Name != "" {
			t.Errorf("次帧工具 %d 不得重发身份: %+v", i, second[i])
		}
		if second[i].Function.Arguments != want {
			t.Errorf("次帧工具 %d 参数 = %q，期望 %q", i, second[i].Function.Arguments, want)
		}
	}
}

// TestStreamReasoningContentDelta 钉死 reasoning_content 增量逐帧透传且不与正文混淆。
//
// 丢了它思考过程静默消失，客户端只看到结论；混进 content 则会让思考文本被当成
// 正式回复展示给终端用户。
func TestStreamReasoningContentDelta(t *testing.T) {
	up := newNativeSSEUpstream(t, []string{
		`{"output":{"choices":[{"finish_reason":"null","message":{"role":"assistant",
		   "content":"","reasoning_content":"先想"}}]},"request_id":"r"}`,
		`{"output":{"choices":[{"finish_reason":"null","message":{"role":"assistant",
		   "content":"","reasoning_content":"一下"}}]},"request_id":"r"}`,
		`{"output":{"choices":[{"finish_reason":"stop","message":{"role":"assistant",
		   "content":"答案"}}]},"request_id":"r"}`,
	})
	defer up.Close()

	p, _ := newClockedProvider(t)
	resp, err := p.Call(context.Background(), streamChatRequest(t, up.URL, false))
	if err != nil {
		t.Fatalf("流式转换应成功: %v", err)
	}
	out := readAllStream(t, resp)
	chunks, done := parseChatStream(t, out)
	if !done || len(chunks) != 3 {
		t.Fatalf("应产出三条 chunk 并以 [DONE] 收尾: %s", out)
	}

	wantReasoning := []string{"先想", "一下", ""}
	wantContent := []string{"", "", "答案"}
	for i, c := range chunks {
		d := c.Choices[0].Delta
		if d.ReasoningContent != wantReasoning[i] {
			t.Errorf("chunk[%d].delta.reasoning_content = %q，期望 %q", i, d.ReasoningContent, wantReasoning[i])
		}
		if d.Content != wantContent[i] {
			t.Errorf("chunk[%d].delta.content = %q，期望 %q", i, d.Content, wantContent[i])
		}
	}
}

// TestStreamMidstreamMalformedFrame 钉死首帧之后的畸形帧变成流内 *canonical.Error。
//
// 此时下游已有首字节，不得重试（原则 2.4）；错误也不能是裸 error——Gateway 会
// 把它按 internal 编成下游错误事件，一次上游中断因此看起来像网关自己的 bug。
func TestStreamMidstreamMalformedFrame(t *testing.T) {
	up := newNativeSSEUpstream(t, []string{
		`{"output":{"choices":[{"finish_reason":"null","message":{"role":"assistant","content":"好"}}]},"request_id":"r"}`,
		`{not-json`,
	})
	defer up.Close()

	p, _ := newClockedProvider(t)
	resp, err := p.Call(context.Background(), streamChatRequest(t, up.URL, false))
	if err != nil {
		t.Fatalf("首帧正常，Call 应成功: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("关闭响应体失败: %v", err)
		}
	}()

	var got bytes.Buffer
	buf := make([]byte, 64)
	var readErr error
	for {
		n, err := resp.Body.Read(buf)
		if n == 0 && err == nil {
			t.Fatal("Read 不得返回 (0, nil)")
		}
		got.Write(buf[:n])
		if err != nil {
			readErr = err
			break
		}
	}
	if readErr == nil || errors.Is(readErr, io.EOF) {
		t.Fatalf("畸形帧应报错而不是干净收尾，实际 %v", readErr)
	}
	var cerr *canonical.Error
	if !errors.As(readErr, &cerr) {
		t.Fatalf("错误链应包含 *canonical.Error，实际 %T", readErr)
	}
	if cerr.Class != canonical.ClassUpstreamUnavailable {
		t.Errorf("上游畸形字节应分类为 upstream_unavailable，实际 %v", cerr)
	}
	if strings.Contains(got.String(), "[DONE]") {
		t.Errorf("出错时不得合成 [DONE]: %s", got.String())
	}
	// 出错前已经产出的内容仍要交付：吞掉它等于把已经生成的字白白丢掉。
	if !strings.Contains(got.String(), `"好"`) {
		t.Errorf("出错前的正常帧仍应交付，实际 %s", got.String())
	}

	// 错误必须粘住：再读一次仍是同一个错误，否则调用方会看到 (0, nil) 死循环。
	n, again := resp.Body.Read(buf)
	if n != 0 || again == nil {
		t.Fatalf("报错后再读应仍返回错误，实际 n=%d err=%v", n, again)
	}
	if !errors.As(again, &cerr) {
		t.Fatalf("再次读到的错误也应是 *canonical.Error，实际 %T", again)
	}
}

// TestStreamErrorIsSticky 钉死报错后的 Read 一直返回同一个错误。
//
// 不粘住的话，下一次 Read 会从「候选数已乱」的状态继续往下读上游：畸形帧之后
// 若跟着一个正常帧，客户端会在一个已经报错的流里又收到内容；上游恰好没有下一帧
// 时则退化成 (0, nil)，触发 relayStream 的 io.ErrNoProgress。用直接构造的
// transformReader 断言，因为这条契约与上游还剩几帧无关。
func TestStreamErrorIsSticky(t *testing.T) {
	body := &countingBody{reader: strings.NewReader(
		"event:result\ndata:{\"output\":{\"choices\":[{\"finish_reason\":\"null\"," +
			"\"message\":{\"role\":\"assistant\",\"content\":\"续\"}}]},\"request_id\":\"r\"}\n\n")}
	r := &transformReader{
		native: sse.NewReader(body),
		body:   body,
		// 首帧候选数为 2，而上游后续帧只有 1 个候选——第二帧必然报契约违例。
		first: &nativewire.Result{
			RequestID: "r",
			Choices: []nativewire.Choice{
				{Role: "assistant", Content: "A"},
				{Role: "assistant", Content: "B"},
			},
		},
		cands:   &candidateState{},
		id:      "r",
		model:   "qwen-plus",
		created: 1,
		now:     func() time.Time { return clockBase },
	}
	defer func() {
		if err := r.Close(); err != nil {
			t.Errorf("关闭失败: %v", err)
		}
	}()

	buf := make([]byte, 4096)
	// 首帧先产出字节。
	if n, err := r.Read(buf); n == 0 || err != nil {
		t.Fatalf("首帧应产出字节，实际 n=%d err=%v", n, err)
	}
	n, first := r.Read(buf)
	if first == nil {
		t.Fatalf("候选数缩水应报错，实际 n=%d", n)
	}
	var cerr *canonical.Error
	if !errors.As(first, &cerr) {
		t.Fatalf("应为 *canonical.Error，实际 %T", first)
	}

	// 再读三次：不粘住的实现会在上游耗尽后返回 (0, nil) 或换成 EOF，
	// 把一次上游契约违例悄悄粉饰成正常收尾。
	for i := range 3 {
		n, again := r.Read(buf)
		if n != 0 {
			t.Fatalf("第 %d 次重读不得再产出字节，实际 %d", i+1, n)
		}
		if again == nil {
			t.Fatalf("第 %d 次重读返回了 (0, nil)", i+1)
		}
		if !errors.Is(again, first) {
			t.Errorf("第 %d 次重读应返回同一个错误，实际 %v", i+1, again)
		}
	}
}

// TestStreamMidstreamErrorEnvelope 钉死流中的 Native 错误信封被识别为流内错误。
//
// 宽松解码会把纯错误信封解成「零候选的成功帧」，上游给出的 code 与 request_id
// 就此丢掉，客户端只看到一条空 chunk。
func TestStreamMidstreamErrorEnvelope(t *testing.T) {
	up := newNativeSSEUpstream(t, []string{
		`{"output":{"choices":[{"finish_reason":"null","message":{"role":"assistant","content":"好"}}]},"request_id":"r"}`,
		`{"code":"InternalError","message":"upstream exploded","request_id":"req-boom"}`,
	})
	defer up.Close()

	p, _ := newClockedProvider(t)
	resp, err := p.Call(context.Background(), streamChatRequest(t, up.URL, false))
	if err != nil {
		t.Fatalf("首帧正常，Call 应成功: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("关闭响应体失败: %v", err)
		}
	}()

	out, err := io.ReadAll(resp.Body)
	if err == nil {
		t.Fatalf("流中错误信封应报错，实际读净: %s", out)
	}
	cerr := canonical.AsError(err)
	if cerr.Class != canonical.ClassUpstreamUnavailable {
		t.Errorf("InternalError 应分类为 upstream_unavailable，实际 %v", cerr)
	}
	// request_id 是向阿里云提工单时的唯一凭据，丢了这次故障再也查不回来。
	if cerr.UpstreamRequestID != "req-boom" {
		t.Errorf("应保留上游 request_id，实际 %q", cerr.UpstreamRequestID)
	}
	if strings.Contains(string(out), "[DONE]") {
		t.Errorf("出错时不得合成 [DONE]: %s", out)
	}
}

// TestStreamReaderErrorIsCanonicalized 钉死 sse.Reader 的裸错误被归一成 *canonical.Error。
//
// sse.Reader 的扫描错误（单帧超过 8 MiB 上限）是 fmt.Errorf 造的裸 error。原样
// 上抛的话，Gateway 的 relay 会按 internal 把它编成下游错误事件——一次上游发疯
// 因此看起来像网关自己的 bug，既误导排查也让告警指向错误的团队。
func TestStreamReaderErrorIsCanonicalized(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Error("httptest 的 ResponseWriter 应支持 Flush")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		first := `{"output":{"choices":[{"finish_reason":"null",` +
			`"message":{"role":"assistant","content":"好"}}]},"request_id":"r"}`
		if _, err := fmt.Fprintf(w, "event:result\n:HTTP_STATUS/200\ndata:%s\n\n", first); err != nil {
			t.Errorf("写出首帧失败: %v", err)
			return
		}
		f.Flush()
		// 第二帧超过 sse.MaxEventBytes（8 MiB），扫描器报 bufio.ErrTooLong。
		if _, err := io.WriteString(w, "event:result\ndata:"); err != nil {
			t.Errorf("写出超长帧头失败: %v", err)
			return
		}
		// 这次写的错误刻意丢弃，也刻意不 t.Errorf：客户端一旦判定 ErrTooLong
		// 就会关闭连接，服务端此刻写到一半必然收到 connection reset——那正是
		// 本用例期望的结果。把它当失败上报会让用例随内核缓冲的时序随机翻红，
		// 而真正要证明的事（裸错误被归一）由下面的断言负责。
		_, _ = w.Write(bytes.Repeat([]byte("x"), 9<<20))
		f.Flush()
	}))
	defer up.Close()

	p, _ := newClockedProvider(t)
	resp, err := p.Call(context.Background(), streamChatRequest(t, up.URL, false))
	if err != nil {
		t.Fatalf("首帧正常，Call 应成功: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("关闭响应体失败: %v", err)
		}
	}()

	out, err := io.ReadAll(resp.Body)
	if err == nil {
		t.Fatalf("超长帧应报错，实际读净: %s", out)
	}
	var cerr *canonical.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("sse.Reader 的裸错误必须被归一为 *canonical.Error，实际 %T: %v", err, err)
	}
	if cerr.Class != canonical.ClassUpstreamUnavailable {
		t.Errorf("上游流读取失败应分类为 upstream_unavailable，实际 %v", cerr)
	}
	if strings.Contains(string(out), "[DONE]") {
		t.Errorf("出错时不得合成 [DONE]: %s", out)
	}
}

// TestStreamReadZeroLengthBuffer 钉死零长缓冲按 io.Reader 约定返回 (0, nil) 且不消费帧。
func TestStreamReadZeroLengthBuffer(t *testing.T) {
	up := newNativeSSEUpstream(t, []string{
		`{"output":{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"x"}}]},"request_id":"r"}`,
	})
	defer up.Close()

	p, _ := newClockedProvider(t)
	resp, err := p.Call(context.Background(), streamChatRequest(t, up.URL, false))
	if err != nil {
		t.Fatalf("流式转换应成功: %v", err)
	}

	n, err := resp.Body.Read(nil)
	if n != 0 || err != nil {
		t.Fatalf("零长缓冲应返回 (0, nil)，实际 n=%d err=%v", n, err)
	}
	// 零长读不得吃掉帧：吃掉了下面这条流就少一条 chunk。
	out := readAllStream(t, resp)
	chunks, done := parseChatStream(t, out)
	if !done || len(chunks) != 1 {
		t.Fatalf("零长读后完整流仍应产出一条 chunk 与 [DONE]: %s", out)
	}
}

// TestStreamFinishedCandidatePayloadIsError 钉死已结束候选再来负载即契约违例。
//
// 已 finish 的候选仍会占着后续帧的数组位置（候选数恒定），所以空占位必须放行；
// 但带上内容就说明上游的候选边界乱了。静默接收会把这段增量接在一条客户端认为
// 已经结束的消息后面——客户端要么把它当成新一轮回复，要么直接丢掉，而两边都
// 看不到任何错误。四种负载各来一遍，防的是实现只挡住其中一种。
func TestStreamFinishedCandidatePayloadIsError(t *testing.T) {
	// 首帧两个候选，候选 1 立刻 stop；次帧候选 1 带着各种负载回来。
	head := `{"output":{"choices":[
	   {"finish_reason":"null","message":{"role":"assistant","content":"A"}},
	   {"finish_reason":"stop","message":{"role":"assistant","content":"B"}}]},"request_id":"r"}`
	cases := []struct {
		name    string
		payload string
	}{
		{"content", `{"finish_reason":"null","message":{"role":"assistant","content":"B2"}}`},
		{"reasoning", `{"finish_reason":"null","message":{"role":"assistant","content":"","reasoning_content":"又想"}}`},
		{"tool_calls", `{"finish_reason":"null","message":{"role":"assistant","content":"",
		   "tool_calls":[{"id":"call_x","function":{"name":"f","arguments":"{}"}}]}}`},
		{"重复 finish", `{"finish_reason":"stop","message":{"role":"assistant","content":""}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			second := `{"output":{"choices":[` +
				`{"finish_reason":"null","message":{"role":"assistant","content":"A2"}},` +
				tc.payload + `]},"request_id":"r"}`
			up := newNativeSSEUpstream(t, []string{head, second})
			defer up.Close()

			p, _ := newClockedProvider(t)
			resp, err := callTranslateStream(t, p, streamChatRequestN(t, up.URL, 2))
			if err != nil {
				t.Fatalf("首帧正常，Call 应成功: %v", err)
			}
			defer func() {
				if err := resp.Body.Close(); err != nil {
					t.Errorf("关闭响应体失败: %v", err)
				}
			}()

			out, readErr := io.ReadAll(resp.Body)
			if readErr == nil {
				t.Fatalf("已结束候选再来负载应产生流内错误，实际读净: %s", out)
			}
			var cerr *canonical.Error
			if !errors.As(readErr, &cerr) {
				t.Fatalf("错误链应包含 *canonical.Error，实际 %T: %v", readErr, readErr)
			}
			if cerr.Class != canonical.ClassUpstreamUnavailable {
				t.Errorf("契约违例应分类为 upstream_unavailable，实际 %v", cerr)
			}
			// 错误必须点名是哪个候选，否则多候选时无从定位。
			if !strings.Contains(cerr.Message, "候选 1") {
				t.Errorf("错误应点名越界的候选序号，实际 %q", cerr.Message)
			}
			if strings.Contains(string(out), "[DONE]") {
				t.Errorf("出错时不得合成 [DONE]: %s", out)
			}
			// 出错前的首帧仍应交付：吞掉它等于把已经生成的字白白丢掉。
			if !strings.Contains(string(out), `"A"`) {
				t.Errorf("出错前的正常帧仍应交付，实际 %s", out)
			}
			// 错误粘住：再读一次仍是同一个错误，而不是 (0, nil) 或干净 EOF。
			buf := make([]byte, 64)
			n, again := resp.Body.Read(buf)
			if n != 0 || again == nil {
				t.Fatalf("报错后再读应仍返回错误，实际 n=%d err=%v", n, again)
			}
			if !errors.Is(again, readErr) {
				t.Errorf("重读应返回同一个错误，实际 %v", again)
			}
		})
	}
}

// TestStreamFinishedCandidateEmptyPlaceholderIsFine 钉死已结束候选的空占位不报错。
//
// 与上一条成对：把空占位也判成违例，任何候选早于其他候选结束的正常流都会被
// 判成上游故障——n>1 时这是常态，不是异常。
func TestStreamFinishedCandidateEmptyPlaceholderIsFine(t *testing.T) {
	up := newNativeSSEUpstream(t, []string{
		`{"output":{"choices":[
		   {"finish_reason":"null","message":{"role":"assistant","content":"A"}},
		   {"finish_reason":"stop","message":{"role":"assistant","content":"B"}}]},"request_id":"r"}`,
		`{"output":{"choices":[
		   {"finish_reason":"stop","message":{"role":"assistant","content":"A2"}},
		   {"finish_reason":"null","message":{"role":"assistant","content":""}}]},"request_id":"r"}`,
	})
	defer up.Close()

	p, _ := newClockedProvider(t)
	resp, err := callTranslateStream(t, p, streamChatRequestN(t, up.URL, 2))
	if err != nil {
		t.Fatalf("流式转换应成功: %v", err)
	}
	out := readAllStream(t, resp)
	chunks, done := parseChatStream(t, out)
	if !done {
		t.Fatalf("全部候选 finish 后应有 [DONE]: %s", out)
	}
	if len(chunks) != 2 {
		t.Fatalf("两个 Native 帧应产出两条 chunk，实际 %d 条: %s", len(chunks), out)
	}
	// 空占位不得凭空长出 finish_reason：那等于替模型宣布它没说过的结束。
	if fr := chunks[1].Choices[1].FinishReason; fr != nil {
		t.Errorf("空占位不应生出 finish_reason，实际 %q", *fr)
	}
	if c := chunks[1].Choices[1].Delta.Content; c != "" {
		t.Errorf("空占位不应生出内容，实际 %q", c)
	}
}

// TestStreamToolCallShrinkIsError 钉死同一候选的工具调用数缩水即契约违例。
//
// 工具身份靠数组下标维系。缩水意味着下标与此前帧对不上，续发的参数片段会被拼到
// 另一次调用身上——客户端拿到一个参数被张冠李戴却语法合法的工具调用，照常执行。
func TestStreamToolCallShrinkIsError(t *testing.T) {
	up := newNativeSSEUpstream(t, []string{
		`{"output":{"choices":[{"finish_reason":"null","message":{"role":"assistant","content":"",
		   "tool_calls":[
		     {"id":"call_a","function":{"name":"fa","arguments":"{\"x\""}},
		     {"id":"call_b","function":{"name":"fb","arguments":"{\"y\""}}]}}]},"request_id":"r"}`,
		`{"output":{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":"",
		   "tool_calls":[{"id":"call_a","function":{"name":"fa","arguments":":1}"}}]}}]},"request_id":"r"}`,
	})
	defer up.Close()

	p, _ := newClockedProvider(t)
	resp, err := p.Call(context.Background(), streamChatRequest(t, up.URL, false))
	if err != nil {
		t.Fatalf("首帧正常，Call 应成功: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("关闭响应体失败: %v", err)
		}
	}()

	out, readErr := io.ReadAll(resp.Body)
	if readErr == nil {
		t.Fatalf("工具调用数缩水应产生流内错误，实际读净: %s", out)
	}
	var cerr *canonical.Error
	if !errors.As(readErr, &cerr) {
		t.Fatalf("错误链应包含 *canonical.Error，实际 %T: %v", readErr, readErr)
	}
	if cerr.Class != canonical.ClassUpstreamUnavailable {
		t.Errorf("契约违例应分类为 upstream_unavailable，实际 %v", cerr)
	}
	// 错误要同时点名候选与缩水前后的数量，否则多候选多工具时无从定位。
	if !strings.Contains(cerr.Message, "候选 0") {
		t.Errorf("错误应点名候选序号，实际 %q", cerr.Message)
	}
	if strings.Contains(string(out), "[DONE]") {
		t.Errorf("出错时不得合成 [DONE]: %s", out)
	}
	// 出错前的首帧（两次工具调用的身份）仍应交付。
	if !strings.Contains(string(out), "call_b") {
		t.Errorf("出错前的正常帧仍应交付，实际 %s", out)
	}
}

// TestStreamUnexpectedEventNameFailsClosed 钉死非 result 的具名事件一律 fail-closed。
//
// 负载刻意是一条**完全正常**的成功帧、且不带任何错误码：判据必须是事件名本身。
// 靠「解不出来」或「有 code」兜底的实现在这里会照单全收，把一条上游用来表达
// 别的意思的事件（心跳、任务状态、未来新增的事件类型）当成模型输出交付下游。
func TestStreamUnexpectedEventNameFailsClosed(t *testing.T) {
	for _, name := range []string{"heartbeat", "task-status", "unknown"} {
		t.Run(name, func(t *testing.T) {
			closed := make(chan struct{})
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				f, ok := w.(http.Flusher)
				if !ok {
					t.Error("httptest 的 ResponseWriter 应支持 Flush")
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				payload := `{"output":{"choices":[{"finish_reason":"stop",` +
					`"message":{"role":"assistant","content":"正常内容"}}]},` +
					`"usage":{"input_tokens":1,"output_tokens":1},"request_id":"r"}`
				if _, err := fmt.Fprintf(w, "event:%s\n:HTTP_STATUS/200\ndata:%s\n\n", name, payload); err != nil {
					t.Errorf("写出事件失败: %v", err)
					return
				}
				f.Flush()
				// 等自己的请求上下文被取消，以此证明 body 确实被关掉了：
				// 漏关会把连接钉在池子里，故障上游能一路把连接池吃干净。
				//
				// 兜底超时不可省：一旦被测代码回归成「不关 body」，没有它这个
				// handler 会永远挂着，httptest 的 Close 随之死等——用例就从
				// 「干净地失败」退化成「CI 超时」，而超时不会告诉任何人原因。
				select {
				case <-r.Context().Done():
					close(closed)
				case <-time.After(30 * time.Second):
				}
			}))
			defer up.Close()

			p, _ := newClockedProvider(t)
			resp, err := p.Call(context.Background(), streamChatRequest(t, up.URL, false))
			// 先处置响应再判错误：返回响应意味着调用方会以为拿到了可读的流，
			// failover 分支永远走不到。这里顺手关掉它，好让上游 handler 立刻
			// 退出——否则回归时用例要白等一个兜底超时才收得了尾。
			if resp != nil {
				t.Errorf("失败时不得返回响应，实际 %+v", resp)
				if closeErr := resp.Body.Close(); closeErr != nil {
					t.Errorf("关闭意外返回的响应体失败: %v", closeErr)
				}
			}
			if err == nil {
				t.Fatal("非 result 事件必须 fail-closed")
			}
			var cerr *canonical.Error
			if !errors.As(err, &cerr) {
				t.Fatalf("错误链应包含 *canonical.Error，实际 %T", err)
			}
			if cerr.Class != canonical.ClassUpstreamUnavailable {
				t.Errorf("未知事件应分类为 upstream_unavailable，实际 %v", cerr)
			}
			// 错误要点名收到的事件名，否则排查时不知道上游到底发了什么。
			if !strings.Contains(cerr.Message, name) {
				t.Errorf("错误应点名事件名 %q，实际 %q", name, cerr.Message)
			}

			select {
			case <-closed:
			case <-time.After(5 * time.Second):
				t.Fatal("fail-closed 后必须立即关闭上游 body，实际连接仍挂着")
			}
		})
	}
}

// TestStreamMidstreamUnexpectedEventFailsClosed 钉死首帧之后的未知事件同样按流内错误处理。
//
// 与首帧版成对：只在首帧预读时检查事件名的实现，在流中会把任意具名事件的
// 负载当成模型输出继续拼下去。
func TestStreamMidstreamUnexpectedEventFailsClosed(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Error("httptest 的 ResponseWriter 应支持 Flush")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		first := `{"output":{"choices":[{"finish_reason":"null",` +
			`"message":{"role":"assistant","content":"好"}}]},"request_id":"r"}`
		if _, err := fmt.Fprintf(w, "event:result\n:HTTP_STATUS/200\ndata:%s\n\n", first); err != nil {
			t.Errorf("写出首帧失败: %v", err)
			return
		}
		f.Flush()
		// 第二条事件负载完全正常，只有事件名不是 result。
		second := `{"output":{"choices":[{"finish_reason":"stop",` +
			`"message":{"role":"assistant","content":"后续"}}]},"request_id":"r"}`
		if _, err := fmt.Fprintf(w, "event:heartbeat\n:HTTP_STATUS/200\ndata:%s\n\n", second); err != nil {
			t.Errorf("写出第二条事件失败: %v", err)
			return
		}
		f.Flush()
	}))
	defer up.Close()

	p, _ := newClockedProvider(t)
	resp, err := p.Call(context.Background(), streamChatRequest(t, up.URL, false))
	if err != nil {
		t.Fatalf("首帧正常，Call 应成功: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("关闭响应体失败: %v", err)
		}
	}()

	out, readErr := io.ReadAll(resp.Body)
	if readErr == nil {
		t.Fatalf("流中未知事件应报错，实际读净: %s", out)
	}
	var cerr *canonical.Error
	if !errors.As(readErr, &cerr) {
		t.Fatalf("错误链应包含 *canonical.Error，实际 %T", readErr)
	}
	if cerr.Class != canonical.ClassUpstreamUnavailable {
		t.Errorf("未知事件应分类为 upstream_unavailable，实际 %v", cerr)
	}
	// 未知事件的负载绝不能被当成内容交付下游。
	if strings.Contains(string(out), "后续") {
		t.Errorf("未知事件的负载不得交付给下游: %s", out)
	}
	if strings.Contains(string(out), "[DONE]") {
		t.Errorf("出错时不得合成 [DONE]: %s", out)
	}
}

// TestDecodeFrameErrorStatusCodePrecedence 钉死 status_code 与 code 的判据与优先级。
//
// 两条边界各自会造成一类静默事故：把带 status_code:200 的正常帧判成错误，一整条
// 好流会在第一帧就被判成故障；把带 4xx/5xx 的错误帧放行，DecodeFrame 会宽松地把
// 它解成「零候选的成功帧」，上游给出的 code 与 request_id 就此丢掉。
func TestDecodeFrameErrorStatusCodePrecedence(t *testing.T) {
	// 时钟取固定值：RetryAfter 之类的推导依赖它，用真实时间只能断言「差不多」。
	now := clockBase

	cases := []struct {
		name string
		data string
		// wantErr 为 false 表示这是正常帧，必须返回 nil。
		wantErr           bool
		wantClass         canonical.ErrorClass
		wantStatus        int
		wantCode          string
		wantRequestID     string
		wantRetryableTrue bool
		// wantMessage 钉住上游文案原样带回：判据结构体刻意不收 message，
		// 文案全靠 DecodeError 从原始字节里解——丢了它下游只剩一个错误码。
		wantMessage string
	}{
		{
			name: "status_code 200 且无 code 是正常帧",
			data: `{"status_code":200,"output":{"choices":[{"finish_reason":"null",` +
				`"message":{"role":"assistant","content":"x"}}]},"request_id":"r"}`,
			wantErr: false,
		},
		{
			// status_code 压过 code：SDK 包装层会把自己的字段一起塞进帧里，
			// 让一个非空 code 否决掉上游明确给出的 200，好流会被判成故障。
			name: "status_code 200 压过非空 code",
			data: `{"status_code":200,"code":"Success","message":"ok",` +
				`"output":{"choices":[{"finish_reason":"stop",` +
				`"message":{"role":"assistant","content":"x"}}]},"request_id":"r"}`,
			wantErr: false,
		},
		{
			name: "status_code 399 仍算正常帧",
			data: `{"status_code":399,"code":"Whatever","output":{"choices":[{"finish_reason":"null",` +
				`"message":{"role":"assistant","content":"x"}}]},"request_id":"r"}`,
			wantErr: false,
		},
		{
			name:              "status_code 429 是限流错误帧",
			data:              `{"status_code":429,"code":"Throttling.RateQuota","message":"too fast","request_id":"req-429"}`,
			wantErr:           true,
			wantClass:         canonical.ClassRateLimit,
			wantStatus:        429,
			wantCode:          "Throttling.RateQuota",
			wantRequestID:     "req-429",
			wantRetryableTrue: true,
			wantMessage:       "too fast",
		},
		{
			// 400 是「已达 4xx」这条边界本身，去掉 >= 会让它漏网。
			name:          "status_code 400 是错误帧",
			data:          `{"status_code":400,"message":"bad","request_id":"req-400"}`,
			wantErr:       true,
			wantClass:     canonical.ClassBadRequest,
			wantStatus:    400,
			wantRequestID: "req-400",
		},
		{
			name:          "status_code 500 且无 code 归上游不可用",
			data:          `{"status_code":500,"message":"boom","request_id":"req-500"}`,
			wantErr:       true,
			wantClass:     canonical.ClassUpstreamUnavailable,
			wantStatus:    500,
			wantRequestID: "req-500",
		},
		{
			// 缺 status_code 时退回看 code，且必须兜底成 500 而不是 0——
			// 给 0 会让 DecodeError 落进 WebSocket 那条分支。
			name:              "无 status_code 时鉴权 code 归 auth",
			data:              `{"code":"InvalidApiKey","message":"bad key","request_id":"req-auth"}`,
			wantErr:           true,
			wantClass:         canonical.ClassAuth,
			wantStatus:        500,
			wantCode:          "InvalidApiKey",
			wantRequestID:     "req-auth",
			wantRetryableTrue: true,
		},
		{
			name: "无 status_code 且无 code 是正常帧",
			data: `{"output":{"choices":[{"finish_reason":"null",` +
				`"message":{"role":"assistant","content":"x"}}]},"request_id":"r"}`,
			wantErr: false,
		},
		{
			// 畸形字节交给 DecodeFrame 报错：在这里抢先分类会把畸形与错误信封
			// 混成一档，上游真正的错误码反而看不见。
			name:    "畸形 JSON 不在此处分类",
			data:    `{not-json`,
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decodeFrameError(tc.data, now)
			if !tc.wantErr {
				if got != nil {
					t.Fatalf("应判为正常帧返回 nil，实际 %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("应判为错误帧，实际返回 nil")
			}
			if got.Class != tc.wantClass {
				t.Errorf("分类 = %q，期望 %q", got.Class, tc.wantClass)
			}
			if got.UpstreamStatus != tc.wantStatus {
				t.Errorf("UpstreamStatus = %d，期望 %d", got.UpstreamStatus, tc.wantStatus)
			}
			if tc.wantCode != "" && got.UpstreamCode != tc.wantCode {
				t.Errorf("UpstreamCode = %q，期望 %q", got.UpstreamCode, tc.wantCode)
			}
			// request_id 是向阿里云提工单时的唯一凭据，丢了这次故障再也查不回来。
			if got.UpstreamRequestID != tc.wantRequestID {
				t.Errorf("UpstreamRequestID = %q，期望 %q", got.UpstreamRequestID, tc.wantRequestID)
			}
			// Retryable 是「换个凭据或 Provider 可能成功」，限流与鉴权恰是这一档。
			if tc.wantRetryableTrue && !got.Retryable {
				t.Errorf("%s 必须可 failover，实际 Retryable=false", tc.wantClass)
			}
			if tc.wantMessage != "" && got.Message != tc.wantMessage {
				t.Errorf("Message = %q，应原样带回上游文案 %q", got.Message, tc.wantMessage)
			}
		})
	}
}

// TestStreamMidstreamStatusCode200FrameIsDelivered 钉死带 status_code:200 的
// SDK 包装帧在真实流里被正常交付，而不是被当成错误吞掉。
//
// 与上面的表驱动成对：表测的是判据函数，这条测的是它接在流水线上的效果——
// 判据写反时，一条本来完好的流会在首帧就 fail-closed，客户端一个字都收不到。
func TestStreamMidstreamStatusCode200FrameIsDelivered(t *testing.T) {
	up := newNativeSSEUpstream(t, []string{
		`{"status_code":200,"code":"","output":{"choices":[{"finish_reason":"null",` +
			`"message":{"role":"assistant","content":"你"}}]},"request_id":"r"}`,
		`{"status_code":200,"output":{"choices":[{"finish_reason":"stop",` +
			`"message":{"role":"assistant","content":"好"}}]},"request_id":"r"}`,
	})
	defer up.Close()

	p, _ := newClockedProvider(t)
	resp, err := p.Call(context.Background(), streamChatRequest(t, up.URL, false))
	if err != nil {
		t.Fatalf("带 status_code:200 的正常帧应放行: %v", err)
	}
	out := readAllStream(t, resp)
	chunks, done := parseChatStream(t, out)
	if !done {
		t.Fatalf("应以 [DONE] 收尾: %s", out)
	}
	if len(chunks) != 2 {
		t.Fatalf("两个 Native 帧应产出两条 chunk，实际 %d 条: %s", len(chunks), out)
	}
	if c := chunks[0].Choices[0].Delta.Content; c != "你" {
		t.Errorf("首帧内容应原样交付，实际 %q", c)
	}
	if c := chunks[1].Choices[0].Delta.Content; c != "好" {
		t.Errorf("次帧内容应原样交付，实际 %q", c)
	}
}
