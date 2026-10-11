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

// responsesDSCompatRouteFixtures 是 Responses -> DashScope Compatible 路径的
// fixture 目录。全部是真实上游录制（tests/smoke/record_responses_dscompat_live_test.go）。
const responsesDSCompatRouteFixtures = "../../testdata/routes/openai.responses__dashscope.compatible"

// responsesDSCompatDegradedHeaders 按用例名钉死降级头必须包含的能力项；
// 不在表里的用例不许出现降级头。独立于 golden：golden 整体漂移时，降级语义
// 的丢失仍要在这里单独咬住。
var responsesDSCompatDegradedHeaders = map[string]string{
	"image_detail":                  "image_detail=",
	"web_search":                    "web_search=",
	"web_search-preview":            "web_search=",
	"structured_output":             "structured_output=",
	"structured_output-json_object": "structured_output=",
	"structured_output-tool-strict": "structured_output=",
}

// responsesDSCompatStatus 是客户端应得的状态码；缺省 200。
var responsesDSCompatStatus = map[string]int{
	"stateful-store-false":     http.StatusBadRequest,
	"error-required-two-tools": http.StatusBadRequest,
}

// TestResponsesDSCompatRouteConformance 回放本路径的全部真实录制。
//
// 除客户端响应 golden 外，逐项断言上游实际收到的 method / path / 鉴权 /
// 请求体——只比客户端响应会漏掉「适配器没做修补、fixture 仍返回成功」这种
// 假绿。会话字段的原样转发也靠这条断言守住：stateful-continue 的 upstream
// 声明里有 previous_response_id，被网关展开历史就对不上。
func TestResponsesDSCompatRouteConformance(t *testing.T) {
	for _, f := range testkit.LoadDir(t, responsesDSCompatRouteFixtures) {
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
			hs := newResponsesDSCompatHarness(t, up)

			body, err := json.Marshal(f.Request.Body)
			if err != nil {
				t.Fatal(err)
			}
			rec := hs.do(t, string(body), true)

			wantStatus := responsesDSCompatStatus[caseName(f.Name)]
			if wantStatus == 0 {
				wantStatus = http.StatusOK
			}
			if rec.Code != wantStatus {
				t.Fatalf("状态码 = %d，期望 %d: %s", rec.Code, wantStatus, rec.Body.String())
			}
			if got := up.calls.Load(); got != 1 {
				t.Fatalf("上游被调用 %d 次，期望恰 1 次（4xx 不得重试）", got)
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

			wantDegraded := responsesDSCompatDegradedHeaders[caseName(f.Name)]
			gotDegraded := rec.Header().Get(degrade.DegradationHeader)
			if wantDegraded == "" {
				if gotDegraded != "" {
					t.Errorf("该用例不应有降级头，实际 %q", gotDegraded)
				}
			} else if !strings.Contains(gotDegraded, wantDegraded) {
				t.Errorf("%s 应包含 %q，实际 %q", degrade.DegradationHeader, wantDegraded, gotDegraded)
			}
			// 会话不是网关垫的，任何用例都不许出现模拟头。
			if got := rec.Header().Get(EmulationHeader); got != "" {
				t.Errorf("本路径不经 convstore，不应有 %s: %q", EmulationHeader, got)
			}

			golden := filepath.Join(responsesDSCompatRouteFixtures, "golden", caseName(f.Name)+".txt")
			testkit.Golden(t, golden, []byte(renderResult(rec)))
		})
	}
}
