//go:build smoke

package smoke_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	nativewire "github.com/yobo2u/omugw/internal/protocol/dashscopenative"
	"github.com/yobo2u/omugw/internal/testkit"
	"github.com/yobo2u/omugw/internal/transport/sse"
)

// hopByHopHeaders 是不得跨代理转发的逐跳头（RFC 9110 §7.6.1）。
//
// 原样转发它们会让上游按本来只对这一跳有效的约定行事：转发一个继承自
// 下游连接的 Transfer-Encoding 或 Upgrade，足以把一次普通的 POST 变成
// 协议层面对不上的畸形请求，而症状是上游莫名其妙的 400。
var hopByHopHeaders = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
}

const (
	// recordingProxyTotalTimeout 是代理调用真实上游的整体上限。
	//
	// 必须容得下一整条真实流式回答：Client.Timeout 覆盖读 body 的全过程，
	// 卡太紧会把正常的长回答截成半截 fixture，而录制器只看到一个 EOF，
	// 分不出「上游说完了」和「我们把它掐了」。
	recordingProxyTotalTimeout = 5 * time.Minute
	// recordingProxyConnectTimeout 是建连与 TLS 握手上限，连不上要立刻失败。
	recordingProxyConnectTimeout = 10 * time.Second
	// recordingProxyHeaderTimeout 是等上游响应头的上限，独立于整体超时。
	recordingProxyHeaderTimeout = 60 * time.Second
)

// forwardableHeaders 复制可跨代理转发的请求头，逐跳头与 Accept-Encoding 一律剥掉。
//
// 逐值复制而不是取首值：Accept 一类的头允许重复出现，只取第一条会静默
// 改写请求语义，而上游照样返回 200，差异要到比对响应内容时才暴露。
func forwardableHeaders(src http.Header) http.Header {
	skip := make(map[string]bool, len(hopByHopHeaders))
	maps.Copy(skip, hopByHopHeaders)
	// 显式转发 Accept-Encoding 会让 Go Transport 放弃透明解压，导致 gzip
	// 压缩字节仍贴着 application/json 标签落盘进 fixture 并回传给下游；
	// 单独剥除以由代理 Transport 自行协商并透明解压。
	skip["accept-encoding"] = true
	// Connection 里点名的头同样是逐跳的，漏掉这一步等于只剥了固定名单。
	for _, v := range src.Values("Connection") {
		for _, name := range strings.Split(v, ",") {
			if n := strings.ToLower(strings.TrimSpace(name)); n != "" {
				skip[n] = true
			}
		}
	}

	out := make(http.Header, len(src))
	for k, vs := range src {
		if skip[strings.ToLower(k)] {
			continue
		}
		out[k] = slices.Clone(vs)
	}
	return out
}

// safeResponseHeaders 只挑出允许留存的响应头。
//
// 上游响应头里混着 request_id、Set-Cookie 与各家自有的追踪字段，整体照抄
// 会把它们一路带进 fixture 存盘。这里采用白名单：新增一个上游头时默认不留存，
// 而不是等谁想起来往黑名单里补一笔。
func safeResponseHeaders(h http.Header) map[string]string {
	if ct := h.Get("Content-Type"); ct != "" {
		return map[string]string{"content-type": ct}
	}
	return nil
}

// isSSEResponse 判断这次交互是否走事件流。
//
// 两个判据取并集：上游未必如实标 Content-Type，而 DashScope Native 把
// 「是否流式」放在请求头上——只认其中一个，另一种情形就会被当成整块 JSON
// 一次读完，流式录制就此退化成攒完再存。
func isSSEResponse(reqHeader, respHeader http.Header) bool {
	if mediaType, _, err := mime.ParseMediaType(respHeader.Get("Content-Type")); err == nil {
		if mediaType == "text/event-stream" {
			return true
		}
	}
	return strings.EqualFold(reqHeader.Get(nativewire.SSEHeader), "enable")
}

// newRecordingProxy 启动一个转发型录制代理：网关把它当作上游，它把请求转给
// originBaseURL，捕获脱敏后的请求与真实响应，同时即时回放给网关。
//
// 「即时回放」是硬要求：代理若先把整条流攒完再吐给网关，被录制的就不再是
// 网关在生产里会遇到的到达节奏，而缓冲类 bug 只在分片边界上现形。
func newRecordingProxy(t testing.TB, originBaseURL string) (*httptest.Server, *recordingState) {
	t.Helper()

	if _, err := url.Parse(originBaseURL); err != nil {
		t.Fatalf("源站 URL %q 不合法: %v", originBaseURL, err)
	}

	proxy := &recordingProxy{
		origin: strings.TrimSuffix(originBaseURL, "/"),
		state:  &recordingState{},
		client: &http.Client{
			Timeout: recordingProxyTotalTimeout,
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: recordingProxyConnectTimeout}).DialContext,
				TLSHandshakeTimeout:   recordingProxyConnectTimeout,
				ResponseHeaderTimeout: recordingProxyHeaderTimeout,
			},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(proxy.serve))
	t.Cleanup(srv.Close)

	return srv, proxy.state
}

// recordingProxy 把一次录制所需的目的地、出站客户端与捕获状态绑在一起。
type recordingProxy struct {
	origin string
	client *http.Client
	state  *recordingState
}

// serve 处理一次代理请求：转发、捕获、即时回放。
//
// 全程不调用 t.Fatal：这里跑在服务端 goroutine 上，跨 goroutine 的 FailNow
// 会让测试拿到一次 panic 而不是一条断言失败，真正的原因反而被埋掉。
// 失败一律存进 state，由测试 goroutine 从快照里取出来报。
func (p *recordingProxy) serve(w http.ResponseWriter, r *http.Request) {
	state := p.state
	if !state.claim() {
		http.Error(w, "录制代理只允许捕获一次", http.StatusConflict)
		return
	}

	reqBody, err := io.ReadAll(r.Body)
	if err != nil {
		state.storeErr(fmt.Errorf("读取下游请求体失败: %w", err))
		http.Error(w, "read request error", http.StatusInternalServerError)
		return
	}
	if err := r.Body.Close(); err != nil {
		state.storeErr(fmt.Errorf("关闭下游请求体失败: %w", err))
	}

	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, p.origin+r.URL.RequestURI(),
		bytes.NewReader(reqBody))
	if err != nil {
		state.storeErr(fmt.Errorf("构造上游请求失败: %w", err))
		http.Error(w, "build upstream request error", http.StatusInternalServerError)
		return
	}
	// Authorization 与流式头必须原样带上——脱敏只发生在落盘的那一份，
	// 真正打出去的那一份脱了敏就换不回一个 200。
	outReq.Header = forwardableHeaders(r.Header)

	// 请求预期先于调用登记：上游失败时这份证据仍然要在，否则排查只剩一句
	// 「调用失败」，连当时发的是什么都查不回来。
	state.setUpstream(testkit.UpstreamExpectation{
		Method:  r.Method,
		Path:    r.URL.Path,
		Headers: testkit.SanitizeHeaders(r.Header),
		Body:    json.RawMessage(reqBody),
	})

	resp, err := p.client.Do(outReq)
	if err != nil {
		state.storeErr(fmt.Errorf("调用上游失败: %w", err))
		http.Error(w, "upstream call error", http.StatusBadGateway)
		return
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			state.storeErr(fmt.Errorf("关闭上游响应体失败: %w", err))
		}
	}()

	state.setResponseHead(resp.StatusCode, safeResponseHeaders(resp.Header))

	// 仅 200 响应才允许进入事件流中继：流式请求遇到错误时上游仍会返回非 200
	// 的 JSON；若将非 200 JSON 送进 SSE Reader 会丢弃 code 与 request_id 并产出
	// 空事件，彻底破坏快照与回放中的诊断信息。
	if resp.StatusCode == http.StatusOK && isSSEResponse(r.Header, resp.Header) {
		relayRecordedSSE(w, resp, state)
		return
	}
	relayRecordedJSON(w, resp, state)
}

// relayRecordedJSON 捕获并回放非流式响应。
func relayRecordedJSON(w http.ResponseWriter, resp *http.Response, state *recordingState) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		state.storeErr(fmt.Errorf("读取上游响应体失败: %w", err))
		http.Error(w, "read upstream body error", http.StatusBadGateway)
		return
	}
	state.setResponseBody(body)

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := w.Write(body); err != nil {
		state.storeErr(fmt.Errorf("向下游写出响应体失败: %w", err))
	}
}

// relayRecordedSSE 逐事件捕获并回放流式响应。
//
// 用生产 sse.Reader/Writer 而不是测试辅助的一次性解析：录制下来的必须是生产
// 代码读得懂、也写得出的那一份，两套实现各自演进会让 fixture 的可信度落空。
func relayRecordedSSE(w http.ResponseWriter, resp *http.Response, state *recordingState) {
	// NewWriter 顺带写好流式必需的响应头，因此必须早于 WriteHeader。
	sw, err := sse.NewWriter(w)
	if err != nil {
		state.storeErr(fmt.Errorf("构造下游 SSE writer 失败: %w", err))
		http.Error(w, "sse writer error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(resp.StatusCode)

	// 每条事件收到即转出：Write 内部逐条 flush，网关因此拿到与真实上游一致
	// 的到达节奏，而不是一次性倒出来的一坨。
	relay := func(ev testkit.SSEEvent) bool {
		state.appendEvent(ev)
		if err := sw.Write(ev); err != nil {
			state.storeErr(fmt.Errorf("向下游写出事件失败: %w", err))
			return false
		}
		return true
	}

	rd := sse.NewReader(resp.Body)
	for {
		ev, err := rd.Next()
		if err == nil {
			if !relay(ev) {
				return
			}
			continue
		}
		if errors.Is(err, io.EOF) {
			// 流末尾缺空行时，最后一条事件与 io.EOF 一起返回；丢掉它就等于
			// 让客户端少收最后一个 token，而这正是真实上游的常见收尾方式。
			if ev.Data != "" || ev.Event != "" || ev.ID != "" {
				relay(ev)
			}
			return
		}
		if responseRequestEnded(resp, err) {
			return
		}
		state.storeErr(fmt.Errorf("读取上游 SSE 失败: %w", err))
		return
	}
}

func responseRequestEnded(resp *http.Response, err error) bool {
	if resp.Request == nil {
		return false
	}
	ctxErr := resp.Request.Context().Err()
	return ctxErr != nil && errors.Is(err, ctxErr)
}
