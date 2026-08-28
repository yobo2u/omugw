//go:build smoke

package smoke_test

import (
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	nativewire "github.com/yobo2u/omugw/internal/protocol/dashscopenative"
)

// TestRecordingProxyPreservesUpstreamJSONErrorWhenRequestIndicatesSSE 钉住一条
// 录制不变量：上游用非 200 的 JSON 回绝时，落进快照与回给下游的都必须是那份
// 原样的 JSON 诊断，而不是一条空事件流。
//
// 防的是「只凭请求头判流式」引出的连锁反应。DashScope Native 把「是否流式」
// 放在请求头上，代理据此认定这次交互走事件流——可上游是否真的开流，要到看见
// 响应状态码才算数。请求头说 enable、上游却回 400 JSON 时，三步叠起来把诊断
// 抹干净：sse.Reader 拿 JSON 当事件解析，`{"code"` 不是任何已知字段而被丢弃，
// 于是没有一条非空事件被追加，快照里既没有 body 也没有事件；NewWriter 又把
// 下游的 Content-Type 改写成 text/event-stream，配着一个空 body 发出去。
// 结果是调用方拿到一个「状态码 400、内容为空」的事件流：上游本来给足了
// code 与 request_id，够定位到具体是哪次调用的哪个凭据出了问题，全丢了。
//
// 这条测试要求的是：SSE 中继只在上游真的以 200 开流时才启用。
func TestRecordingProxyPreservesUpstreamJSONErrorWhenRequestIndicatesSSE(t *testing.T) {
	// Given: 一个以 400 + application/json 回绝的源站，body 是 DashScope 的
	// 标准错误信封。三个字段各有分工：code 供程序分支，message 供人读，
	// request_id 是找上游对账时唯一的把手。
	const (
		originErrorBody = `{"code":"InvalidApiKey","message":"Invalid API-key provided.","request_id":"req-err-sse-001"}`
		wantCode        = "InvalidApiKey"
		wantRequestID   = "req-err-sse-001"
		gatewayBody     = `{"model":"qwen-plus","input":{"messages":[{"role":"user","content":"你好"}]}}`
	)

	// 容量 2 且非阻塞发送：源站写响应失败时还要再报一次，写满就丢弃，
	// 绝不能让服务端 goroutine 卡在 channel 上把测试拖成超时。
	originCh := make(chan originCapture, 2)
	send := func(c originCapture) {
		select {
		case originCh <- c:
		default:
		}
	}

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			send(originCapture{err: fmt.Errorf("读取源站请求体失败: %w", err)})
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}
		// 记下流式指示器：这次交互究竟有没有走上「请求头说 enable」那条岔路，
		// 只有源站的观察能作证。
		send(originCapture{sseHeader: r.Header.Get(nativewire.SSEHeader)})

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		if _, err := io.WriteString(w, originErrorBody); err != nil {
			send(originCapture{err: fmt.Errorf("写出源站响应失败: %w", err)})
		}
	}))
	defer origin.Close()

	proxy, state := newRecordingProxy(t, origin.URL)

	// When: 下游发起一次「请求头声明流式」的普通 JSON 请求。
	//
	// 只设这两个头就够了：Content-Type 说明请求体是 JSON，SSEHeader 是唯一
	// 让代理误判成事件流的输入。不带凭据，失败输出里因此不可能混进任何密钥。
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		proxy.URL+nativewire.TextGenerationPath, strings.NewReader(gatewayBody))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(nativewire.SSEHeader, "enable")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("请求录制代理失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	downstream, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取代理响应体失败: %v", err)
	}

	// Then: 源站确实看到了 enable。这一条先于其余断言，因为流式指示器没抵达
	// 上游时，后面每一条都会不劳而获地变绿——被验的那条岔路根本没被走到。
	select {
	case captured := <-originCh:
		if captured.err != nil {
			t.Fatalf("假源站失败: %v", captured.err)
		}
		if captured.sseHeader != "enable" {
			t.Fatalf("源站 %s = %q，期望 enable：流式指示器没送到上游，"+
				"这条测试就没在验它声称要验的东西", nativewire.SSEHeader, captured.sseHeader)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("等待假源站收到请求超时")
	}

	snap := state.Snapshot()
	if snap.Err != nil {
		t.Fatalf("代理处理器出错: %v", snap.Err)
	}
	if snap.Response.Status != http.StatusBadRequest {
		t.Fatalf("快照 response.status = %d，期望 400", snap.Response.Status)
	}

	// Then: 上游的诊断整份留在快照里。下面三条查的是同一份丢失的 body，只是
	// 换三个角度呈现，一起留着是为了让失败输出自己说清丢的是哪一层：原文比对
	// 给出字节，json.Valid 给出它还算不算合法 JSON，解码给出 code 与
	// request_id 这两个真正拿去排查的标识符。
	if got := string(snap.Response.Body); got != originErrorBody {
		t.Errorf("快照 response.body = %q，期望上游原样的 %q；"+
			"非 200 的 JSON 被当成事件流解析后，整份诊断没有任何一格落盘",
			got, originErrorBody)
	}
	if !json.Valid(snap.Response.Body) {
		t.Errorf("快照 response.body 不是合法 JSON（%d 字节），前 64 字节为 %.64q",
			len(snap.Response.Body), snap.Response.Body)
	}
	var decoded struct {
		Code      string `json:"code"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(snap.Response.Body, &decoded); err != nil {
		t.Errorf("快照 response.body 解码失败: %v；上游给出的 code=%q 与 "+
			"request_id=%q 一并丢失，排查时既分不清是哪类错误，也对不上是哪次调用",
			err, wantCode, wantRequestID)
	} else {
		if decoded.Code != wantCode {
			t.Errorf("快照 response.body 的 code = %q，期望 %q", decoded.Code, wantCode)
		}
		if decoded.RequestID != wantRequestID {
			t.Errorf("快照 response.body 的 request_id = %q，期望 %q",
				decoded.RequestID, wantRequestID)
		}
	}

	// Then: 快照里没有事件流。这一条是附带封口而非本测试的主判据——上游诊断
	// 是否还在，由上面的 body / code / request_id 三条决定；这里只防将来的
	// 修复改成「照样走 SSE，只是额外补一份 body」。
	//
	// 修复被撤销时它会报红，报的是一条三格皆空的事件，来路是这样：那行 JSON
	// 被当成 SSE 字段行切在第一个冒号上，切出的字段名 `{"code"` 不匹配任何已知
	// 字段，值被丢弃，但 open 已经置位。于是首次 Next 返回的是一个 data 为空的
	// 空壳，且错误为 nil——relay 因此照单全收，既追加进快照也写给了下游。真正的
	// io.EOF 要到下一次 Next 才返回，那时事件三格皆空，EOF 分支的非空判据
	// 不成立：记下这条空事件的是首次 Next，不是 EOF 分支。
	if snap.Response.SSE != nil && len(snap.Response.SSE.Events) > 0 {
		t.Errorf("快照 response.sse 捕获到 %d 条事件，非 200 的 JSON 错误不该被录成事件流: %+v",
			len(snap.Response.SSE.Events), snap.Response.SSE.Events)
	}

	// Then: 回给下游的同样是那份 JSON 诊断。录制对了但回放成空事件流，
	// 依赖代理做在线断言的用例照样只能看到一个没有内容的 400。
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("下游状态码 = %d，期望 400: %.64q", resp.StatusCode, downstream)
	}
	// 三个分支互斥：解析失败时 mediaType 是零值，让它再去撞「不是
	// application/json」会把一个头变成两条失败，读的人得先分辨哪条是原因、
	// 哪条是它的回声。
	downstreamCT := resp.Header.Get("Content-Type")
	mediaType, _, ctErr := mime.ParseMediaType(downstreamCT)
	switch {
	case ctErr != nil:
		t.Errorf("下游 Content-Type %q 解析失败: %v", downstreamCT, ctErr)
	case mediaType == "text/event-stream":
		t.Errorf("下游 Content-Type = %q：请求头上的 enable 让一个 400 JSON 错误"+
			"走了 SSE 中继，NewWriter 顺手把内容类型改写成了事件流", downstreamCT)
	case mediaType != "application/json":
		t.Errorf("下游 Content-Type = %q，期望原样透传上游的 application/json", downstreamCT)
	}
	if got := string(downstream); got != originErrorBody {
		t.Errorf("下游响应体 = %q，期望 %q；调用方拿到的 400 里没有 code，"+
			"也没有 request_id，回到上游对账时无从查起", got, originErrorBody)
	}
}
