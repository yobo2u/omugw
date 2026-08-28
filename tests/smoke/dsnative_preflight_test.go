//go:build smoke

package smoke_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/degrade"
	nativewire "github.com/yobo2u/omugw/internal/protocol/dashscopenative"
	"github.com/yobo2u/omugw/internal/testkit"
	"github.com/yobo2u/omugw/internal/transport/sse"
)

// upstreamCapture 是假源站捕获到的一次出站请求，或它自身失败的原因。
type upstreamCapture struct {
	path      string
	auth      string
	sseHeader string
	accept    string
	body      []byte
	err       error
}

// preflightUpstreamSecret 是预检期间注入凭据池的假密钥，不指向任何真实账户。
const preflightUpstreamSecret = "sk-fake-preflight-upstream-secret-0246813579"

// newPreflightUpstream 启动假 Native 源站：捕获出站请求，按流式与否回一份
// 形态正确的响应。全程不调用 t.Fatal——它跑在服务端 goroutine 上。
func newPreflightUpstream(t testing.TB, stream bool, events []testkit.SSEEvent, jsonBody string) (*httptest.Server, <-chan upstreamCapture) {
	t.Helper()

	ch := make(chan upstreamCapture, 4)
	send := func(c upstreamCapture) {
		select {
		case ch <- c:
		default:
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			send(upstreamCapture{err: fmt.Errorf("读取出站请求体失败: %w", err)})
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}
		send(upstreamCapture{
			path:      r.URL.Path,
			auth:      r.Header.Get("Authorization"),
			sseHeader: r.Header.Get(nativewire.SSEHeader),
			accept:    r.Header.Get("Accept"),
			body:      body,
		})

		if !stream {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			if _, err := io.WriteString(w, jsonBody); err != nil {
				send(upstreamCapture{err: fmt.Errorf("写出假响应失败: %w", err)})
			}
			return
		}

		sw, err := sse.NewWriter(w)
		if err != nil {
			send(upstreamCapture{err: fmt.Errorf("构造假源站 SSE writer 失败: %w", err)})
			http.Error(w, "sse writer error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		for _, ev := range events {
			if err := sw.Write(ev); err != nil {
				send(upstreamCapture{err: fmt.Errorf("写出假事件失败: %w", err)})
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	return srv, ch
}

// TestPreflightCaseBodiesMapToNativeUpstream 把 13 份客户端请求体逐一送过真实
// 网关，断言它们既过得了编码期，也映射成了预期的 Native 出站体。
//
// 假上游的响应只求「够转换器走完」，不代表任何真实模型的能力支持——见本节顶部。
func TestPreflightCaseBodiesMapToNativeUpstream(t *testing.T) {
	// 用例名与 recordCases 逐一对账：断言表写漏一个用例，或改名后忘了同步，
	// 都会让某项能力从此不再被预检，而测试照样全绿。
	assertions := preflightAssertions()
	names := make([]string, 0, len(recordCases))
	for _, c := range recordCases {
		names = append(names, c.name)
		if _, ok := assertions[c.name]; !ok {
			t.Fatalf("用例 %q 没有对应的预检断言", c.name)
		}
	}
	for name := range assertions {
		if !slices.Contains(names, name) {
			t.Fatalf("预检断言 %q 不在 recordCases 里", name)
		}
	}

	for _, c := range recordCases {
		t.Run(c.name, func(t *testing.T) {
			model := modelForRole(c.modelRole)
			if model == "" {
				t.Fatalf("模型角色 %q 解析为空字符串", string(c.modelRole))
			}

			events := nativeSSEEvents(assertions[c.name].candidates)
			if c.name == "tool_calling" {
				events = nativeToolSSEEvents()
			}
			upstream, capturedCh := newPreflightUpstream(t, c.stream, events,
				nativeJSONResponse(assertions[c.name].candidates))

			built := buildRecordingGateway(t, upstream.URL, c.door, model, preflightUpstreamSecret)

			req := httptest.NewRequest(http.MethodPost, string(degrade.EndpointOpenAIChat),
				bytes.NewReader(c.body(preflightClientModel)))
			req.Header.Set("Authorization", "Bearer "+testGatewayAuthKey)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			built.Mux.ServeHTTP(rec, req)

			// 网关自己先别把请求拒掉：4xx 说明请求体在编码期就不合法，
			// 5xx 说明映射链路有 bug——两种情况下上游都还没被问到过。
			if rec.Code != http.StatusOK {
				t.Fatalf("网关状态码 = %d，期望 200（上游之前不得出 4xx/5xx）: %s",
					rec.Code, rec.Body.String())
			}

			var captured upstreamCapture
			select {
			case captured = <-capturedCh:
			case <-time.After(5 * time.Second):
				t.Fatal("等待假上游收到请求超时")
			}
			if captured.err != nil {
				t.Fatalf("假上游失败: %v", captured.err)
			}

			// 门决定路径，且只由配置声明的门决定——推断出来的路径会把含媒体的
			// 请求打到文本门，症状是一个语焉不详的上游 400。
			if want := c.door.Path(); captured.path != want {
				t.Errorf("出站路径 = %q，期望门 %q 的 %q", captured.path, string(c.door), want)
			}
			if want := "Bearer " + preflightUpstreamSecret; captured.auth != want {
				t.Errorf("出站 Authorization = %q，期望网关注入的凭据", captured.auth)
			}

			// 流式头是 Native 声明流式的唯一途径，体里没有等价字段。
			if c.stream {
				if captured.sseHeader != "enable" {
					t.Errorf("出站 %s = %q，期望 enable", nativewire.SSEHeader, captured.sseHeader)
				}
			} else if captured.sseHeader != "" {
				t.Errorf("非流式用例的出站 %s = %q，期望不发", nativewire.SSEHeader, captured.sseHeader)
			}

			// 下游必须真的转换成功。只看状态码是不够的：流式的 200 在首帧之前
			// 就写出去了，一条转换半途失败的流照样顶着 200 收场——错误只会以
			// 流内 error 事件的形式出现，而缺了这一步谁也不会去看它。
			assertDownstreamSucceeded(t, rec, c.stream, assertions[c.name].candidates)

			env, msgs := decodeOutbound(t, captured.body)

			// 模型改写是路由的核心职责：客户端说的名字与上游认的名字本就不同。
			if env.Model != model {
				t.Errorf("出站 model = %q，期望被改写成路由选定的 %q", env.Model, model)
			}
			if env.Model == preflightClientModel {
				t.Error("出站 model 仍是客户端提交的名字，模型改写没有发生")
			}
			if len(msgs) == 0 {
				t.Fatal("出站 input.messages 为空")
			}
			// result_format 决定上游回信封的形态，缺了它整条解码链路都对不上。
			wantParam(t, env.Parameters, "result_format", "message")
			// 增量输出只在流式发：非流式发出去等于向上游要一种它不该给的回法。
			if c.stream {
				wantParam(t, env.Parameters, "incremental_output", true)
			} else {
				wantNoParam(t, env.Parameters, "incremental_output")
			}

			assertions[c.name].assert(t, env, msgs)
		})
	}
}

// assertDownstreamSucceeded 断言下游收到的是一份转换完整的 Chat 响应。
//
// 流式一路查到 [DONE]：Native 流没有文档化的终止标记，那个哨兵由 translator
// 合成，只有整条流转换成功才会出现。中途失败的流会以 error 事件收场，
// 而它的状态码早已是 200——不查流内，那种失败在这一层完全不可见。
func assertDownstreamSucceeded(t *testing.T, rec *httptest.ResponseRecorder, stream bool, candidates int) {
	t.Helper()

	if !stream {
		var resp struct {
			Object  string            `json:"object"`
			Choices []json.RawMessage `json:"choices"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("下游响应不是合法 JSON: %v (%s)", err, rec.Body.String())
		}
		if resp.Object != "chat.completion" {
			t.Errorf("下游 object = %q，期望 chat.completion", resp.Object)
		}
		if len(resp.Choices) != candidates {
			t.Errorf("下游 choices 数 = %d，期望与上游候选数一致的 %d", len(resp.Choices), candidates)
		}
		return
	}

	events, err := testkit.ParseSSE(rec.Body)
	if err != nil {
		t.Fatalf("下游 SSE 无法解析: %v (%s)", err, rec.Body.String())
	}
	var sawDone bool
	for _, ev := range events {
		if ev.Event == "error" {
			t.Fatalf("下游流内出现 error 事件，转换未成功: %s", ev.Data)
		}
		if ev.IsDone() {
			sawDone = true
		}
	}
	if !sawDone {
		t.Fatalf("下游流缺少 [DONE] 哨兵，转换未走完: %s", rec.Body.String())
	}
}
