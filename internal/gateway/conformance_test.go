package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/testkit"
)

// 路径级 fixture 的目录。文件名即用例名；以能力名命名的那些是 ADR-0001 要求的
// 有损格子举证。
const routeFixtures = "../../testdata/routes/openai.responses__openai.compat"

// TestRouteConformance 回放路径 fixture。
//
// 这条测试的存在是 ADR-0001 转正门槛的另一半：门槛只检查 fixture **存在**，
// 而存在却从不回放的 fixture 是纯粹的摆设——它能让一条根本跑不通的路径通过
// 转正检查。所以每个 fixture 都要真的走一遍完整链路，输出与 golden 比对。
//
// fixture 的语义：request 是**客户端发给网关**的内容，response 是被打桩的
// 上游返回。同源直通下两者形状接近，但不是同一件事——网关会改写鉴权与模型名。
func TestRouteConformance(t *testing.T) {
	for _, f := range testkit.LoadDir(t, routeFixtures) {
		t.Run(caseName(f.Name), func(t *testing.T) {
			up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
				writeFixtureResponse(t, w, f)
			})
			hs := newHarness(t, true, up)

			body, err := json.Marshal(f.Request.Body)
			if err != nil {
				t.Fatal(err)
			}
			rec := hs.do(t, string(body), true)

			golden := filepath.Join(routeFixtures, "golden", caseName(f.Name)+".txt")
			testkit.Golden(t, golden, []byte(renderResult(rec)))
		})
	}
}

// TestGatedEmulationIsRejectedByDefault 单独固化 EMULATE 那条 fixture 的语义。
//
// 服务端会话由网关侧 ConversationStore 提供，而它**默认关闭**——内存态会话在
// 多副本部署下是错的。因此带 previous_response_id 的请求在默认配置下会被矩阵
// 拦下，**根本不会打到上游**。这条测试断言的正是「没打到上游」。
func TestGatedEmulationIsRejectedByDefault(t *testing.T) {
	f := testkit.Load(t, filepath.Join(routeFixtures, "stateful_conversation.json"))

	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		writeFixtureResponse(t, w, f)
	})
	hs := newHarness(t, true, up)

	body, _ := json.Marshal(f.Request.Body)
	rec := hs.do(t, string(body), true)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("状态码 = %d, 期望 422（开关未开启）: %s", rec.Code, rec.Body.String())
	}
	if n := up.calls.Load(); n != 0 {
		t.Errorf("请求打到了上游 %d 次——开关未开启时不该发出去", n)
	}
	// 错误必须说清是「开关没开」而不是「这条路不支持」，
	// 否则运维会去查一个根本没问题的转换路径。
	if !strings.Contains(rec.Body.String(), "convstore") {
		t.Errorf("错误应指明是哪个开关未开启: %s", rec.Body.String())
	}
}

// TestEmulationWorksWhenEnabled 覆盖开关打开后的行为。
func TestEmulationWorksWhenEnabled(t *testing.T) {
	f := testkit.Load(t, filepath.Join(routeFixtures, "stateful_conversation.json"))

	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		writeFixtureResponse(t, w, f)
	})
	hs := newHarness(t, true, up)
	hs.matrix.WithAvailability(degrade.Availability{
		degrade.FeatureConversationStore: true,
	})

	body, _ := json.Marshal(f.Request.Body)
	rec := hs.do(t, string(body), true)

	if rec.Code != 200 {
		t.Fatalf("开关开启后应当正常转发，状态码 = %d: %s", rec.Code, rec.Body.String())
	}
	// 客户端有权知道这份完整性是网关垫出来的——它带着网关自己的可用性边界。
	if got := rec.Header().Get(EmulationHeader); !strings.Contains(got, "stateful_conversation") {
		t.Errorf("%s = %q, 应告知客户端该能力由网关模拟", EmulationHeader, got)
	}
}

// caseName 从 fixture 名里取出用例部分。
func caseName(full string) string {
	if _, after, ok := strings.Cut(full, "/"); ok {
		return after
	}
	return full
}

// writeFixtureResponse 把 fixture 里录制的上游响应写出去。
func writeFixtureResponse(t *testing.T, w http.ResponseWriter, f testkit.Fixture) {
	t.Helper()

	for k, v := range f.Response.Headers {
		w.Header().Set(k, v)
	}

	if f.Response.SSE == nil {
		w.WriteHeader(f.Response.Status)
		_, _ = w.Write(f.Response.Body)
		return
	}

	w.WriteHeader(f.Response.Status)
	flusher, _ := w.(http.Flusher)

	// 按 frames 复现上游的真实分片节奏——转换器的缓冲 bug 只在分片边界上
	// 才会暴露。
	events := f.Response.SSE.Events
	frames := f.Response.SSE.Frames
	if len(frames) == 0 {
		frames = make([]int, len(events))
		for i := range frames {
			frames[i] = 1
		}
	}

	var i int
	for _, n := range frames {
		var buf strings.Builder
		if err := testkit.WriteSSE(&buf, events[i:i+n]); err != nil {
			t.Fatal(err)
		}
		i += n
		_, _ = w.Write([]byte(buf.String()))
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// renderResult 把网关的响应渲染成便于 golden 比对的文本。
//
// 状态码与关键响应头也进 golden：一次 200 变 502、或者限流头悄悄消失，
// 都是必须被看见的回归。
func renderResult(rec *httptest.ResponseRecorder) string {
	var b strings.Builder
	b.WriteString("status: ")
	b.WriteString(http.StatusText(rec.Code))
	b.WriteString("\n")

	for _, k := range []string{
		"Content-Type",
		"X-Ratelimit-Remaining-Tokens",
		degrade.DegradationHeader,
		EmulationHeader,
	} {
		if v := rec.Header().Get(k); v != "" {
			b.WriteString(k)
			b.WriteString(": ")
			b.WriteString(v)
			b.WriteString("\n")
		}
	}

	b.WriteString("---\n")
	b.WriteString(rec.Body.String())
	return b.String()
}
