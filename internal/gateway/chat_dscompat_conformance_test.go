package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/testkit"
)

// chatDSCompatRouteFixtures 是 Chat -> DashScope Compatible wire-compatible 路径的 fixture 目录。
const chatDSCompatRouteFixtures = "../../testdata/routes/openai.chat__dashscope.compatible"

// chatDSCompatDegradedHeaders 按用例名钉死降级头必须包含的能力项。
// 独立于 golden 断言：golden 整体漂移时，降级语义的丢失仍要在这里单独咬住。
var chatDSCompatDegradedHeaders = map[string]string{
	"structured_output": "structured_output=",
	"web_search":        "web_search=",
}

// TestChatDSCompatNullSearchOptionsDoNotReachUpstream：null 表示不开搜索，但
// web_search_options 仍属于 OpenAI 入站协议，目标协议不认识它。假上游刻意拒绝
// 该字段，防止 Provider 单测与网关真实装配之间出现假绿。
func TestChatDSCompatNullSearchOptionsDoNotReachUpstream(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if _, ok := fields["web_search_options"]; ok {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-null","object":"chat.completion","choices":[]}`)
	})
	hs := newChatDSCompatHarness(t, up)

	rec := hs.do(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"web_search_options":null}`, true)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望删除目标协议不认识的 null 字段后成功: %s", rec.Code, rec.Body.String())
	}
}

// TestChatDSCompatRouteConformance 回放 wire-compatible 路径的全部 fixture。
//
// 与同源直通的回放不同：除了客户端响应 golden，还必须逐项断言上游实际收到的
// method / path / 鉴权 / 请求体——只比客户端响应会漏掉「Provider 根本没做映射、
// fixture 仍返回成功」这种假绿。fixture 的 upstream 声明就是为这件事存在的，
// 缺失即失败。
func TestChatDSCompatRouteConformance(t *testing.T) {
	for _, f := range testkit.LoadDir(t, chatDSCompatRouteFixtures) {
		t.Run(caseName(f.Name), func(t *testing.T) {
			if f.Upstream == nil {
				t.Fatal("异构路径的 fixture 必须带 upstream 断言，否则上游映射无从对账")
			}

			var (
				gotMethod  string
				gotPath    string
				gotHeader  http.Header
				gotBody    []byte
				gotBodyErr error
			)
			// 读 body 的错误只记录、不在这里终止：闭包跑在 httptest 服务器的
			// goroutine 上，t.Fatal 在非测试 goroutine 里只会结束该 goroutine。
			up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				gotMethod = r.Method
				gotPath = r.URL.Path
				gotHeader = r.Header.Clone()
				gotBody, gotBodyErr = io.ReadAll(r.Body)
				writeFixtureResponse(t, w, f)
			})
			hs := newChatDSCompatHarness(t, up)

			body, err := json.Marshal(f.Request.Body)
			if err != nil {
				t.Fatal(err)
			}
			rec := hs.do(t, string(body), true)

			if rec.Code != http.StatusOK {
				t.Fatalf("状态码 = %d，期望 200: %s", rec.Code, rec.Body.String())
			}

			if gotBodyErr != nil {
				t.Fatalf("读取上游收到的 body 失败: %v", gotBodyErr)
			}
			if gotMethod != f.Upstream.Method {
				t.Errorf("上游收到 method %q，期望 %q", gotMethod, f.Upstream.Method)
			}
			if gotPath != f.Upstream.Path {
				t.Errorf("上游收到路径 %q，期望 %q", gotPath, f.Upstream.Path)
			}
			// harness 的第一个凭据池名是 "a"，secret 是 "sk-a"。
			if auth := gotHeader.Get("Authorization"); auth != "Bearer sk-a" {
				t.Errorf("上游收到 Authorization %q，期望网关凭据 Bearer sk-a", auth)
			}
			testkit.AssertJSONEqual(t, f.Upstream.Body, gotBody, "上游收到的请求体语义不符")

			// 降级头独立于 golden 断言：该降级的必须逐项可见，不该降级的不许出现。
			wantDegraded := chatDSCompatDegradedHeaders[caseName(f.Name)]
			gotDegraded := rec.Header().Get(degrade.DegradationHeader)
			if wantDegraded == "" {
				if gotDegraded != "" {
					t.Errorf("该用例不应有降级头，实际 %q", gotDegraded)
				}
			} else if !strings.Contains(gotDegraded, wantDegraded) {
				t.Errorf("%s 应包含 %q，实际 %q", degrade.DegradationHeader, wantDegraded, gotDegraded)
			}

			golden := filepath.Join(chatDSCompatRouteFixtures, "golden", caseName(f.Name)+".txt")
			testkit.Golden(t, golden, []byte(renderResult(rec)))
		})
	}
}
