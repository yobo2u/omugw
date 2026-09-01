package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/degrade"
	nativewire "github.com/yobo2u/omugw/internal/protocol/dashscopenative"
	"github.com/yobo2u/omugw/internal/testkit"
)

const chatDSNativeRouteFixtures = "../../testdata/routes/openai.chat__dashscope.native"

// chatDSNativeCaseNames 返回本期真实证据的精确名单，供 fixture 与 golden 双向对账。
// 两侧共用同一份声明，防止各自维护数量相同但名字不同的假绿。
func chatDSNativeCaseNames() []string {
	return []string{
		"basic",
		"combined",
		"multi_candidate_nonstream",
		"parallel_tool_calls",
		"parallel_tool_calls_default",
		"reasoning",
		"streaming",
		"structured_output",
		"tool_calling",
		"vision_input",
		"web_search",
	}
}

func assertChatDSNativeCaseNames(t *testing.T, kind string, got []string) {
	t.Helper()
	want := chatDSNativeCaseNames()
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("%s 名单 = %v，期望 %v", kind, got, want)
	}
}

// chatDSNativeDegradedHeaders 按用例名钉死降级头必须包含的能力项，独立于 golden。
var chatDSNativeDegradedHeaders = map[string][]string{
	"parallel_tool_calls": {"parallel_tool_calls="},
	"structured_output":   {"structured_output="},
	"web_search":          {"web_search="},
	"combined":            {"structured_output=", "web_search="},
}

// doorFromUpstreamPath 按 fixture 声明的上游路径反推应装配的门。
func doorFromUpstreamPath(t *testing.T, path string) string {
	t.Helper()
	switch path {
	case nativewire.TextGenerationPath:
		return "text-generation"
	case nativewire.MultimodalGenerationPath:
		return "multimodal-generation"
	default:
		t.Fatalf("fixture 上游路径 %q 不是已知 Native 门", path)
		return ""
	}
}

// extractUpstreamModel 从 fixture 的预期上游请求体中提取 model 字段。
// 供 conformance 回放使用，避免在内存中篡改 fixture 的预期上游请求体
// 来迎合 harness 的默认值，防止 fixture laundering。
func extractUpstreamModel(t *testing.T, body []byte) string {
	t.Helper()
	var m struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("无法从上游请求体提取 model: %v", err)
	}
	if m.Model == "" {
		t.Fatal("上游请求体缺少 model 字段")
	}
	return m.Model
}

// TestChatDSNativeRouteConformance 回放全部 fixture：除客户端响应 golden 外，
// 必须逐条断言上游实际收到的 method/path/鉴权/请求体——只比客户端响应会漏掉
// 「Provider 没做映射、fixture 仍返回成功」的假绿。
func TestChatDSNativeRouteConformance(t *testing.T) {
	entries, err := os.ReadDir(chatDSNativeRouteFixtures)
	if err != nil {
		t.Fatal(err)
	}
	fileNames := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			if entry.Name() != "golden" {
				t.Fatalf("fixture 目录混入非预期目录 %q", entry.Name())
			}
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".json") {
			t.Fatalf("fixture 目录混入非 .json 条目 %q", entry.Name())
		}
		fileNames = append(fileNames, strings.TrimSuffix(entry.Name(), ".json"))
	}
	assertChatDSNativeCaseNames(t, "fixture 文件", fileNames)

	fixtures := make([]testkit.Fixture, 0, len(fileNames))
	for _, name := range chatDSNativeCaseNames() {
		f := testkit.Load(t, filepath.Join(chatDSNativeRouteFixtures, name+".json"))
		if got := caseName(f.Name); got != name {
			t.Fatalf("fixture 文件 %q 的内部 name = %q，期望 %q", name+".json", got, name)
		}
		fixtures = append(fixtures, f)
	}

	for _, f := range fixtures {
		t.Run(caseName(f.Name), func(t *testing.T) {
			if f.Upstream == nil {
				t.Fatal("异构路径的 fixture 必须带 upstream 断言，否则上游映射无从对账")
			}
			var (
				gotMethod string
				gotPath   string
				gotHeader http.Header
				gotBody   []byte
				gotErr    error
			)
			up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				gotMethod = r.Method
				gotPath = r.URL.Path
				gotHeader = r.Header.Clone()
				gotBody, gotErr = io.ReadAll(r.Body)
				writeFixtureResponse(t, w, f)
			})

			model := extractUpstreamModel(t, f.Upstream.Body)
			hs := newChatDSNativeHarnessWithModel(t, doorFromUpstreamPath(t, f.Upstream.Path), model, up)

			body, err := json.Marshal(f.Request.Body)
			if err != nil {
				t.Fatal(err)
			}
			rec := hs.do(t, string(body), true)
			if rec.Code != http.StatusOK {
				t.Fatalf("状态码 = %d，期望 200: %s", rec.Code, rec.Body.String())
			}
			if gotErr != nil {
				t.Fatalf("读取上游收到的 body 失败: %v", gotErr)
			}
			if gotMethod != f.Upstream.Method {
				t.Errorf("上游收到 method %q，期望 %q", gotMethod, f.Upstream.Method)
			}
			if gotPath != f.Upstream.Path {
				t.Errorf("上游收到路径 %q，期望 %q", gotPath, f.Upstream.Path)
			}
			if auth := gotHeader.Get("Authorization"); auth != "Bearer sk-a" {
				t.Errorf("上游收到 Authorization %q，期望网关凭据 Bearer sk-a", auth)
			}

			wantAccept := f.Upstream.Headers["accept"]
			gotAccept := gotHeader.Get("Accept")
			if !strings.EqualFold(gotAccept, wantAccept) {
				t.Errorf("上游收到 Accept: %q，期望 %q", gotAccept, wantAccept)
			}

			wantContentType := f.Upstream.Headers["content-type"]
			gotContentType := gotHeader.Get("Content-Type")
			if !strings.EqualFold(gotContentType, wantContentType) {
				t.Errorf("上游收到 Content-Type: %q，期望 %q", gotContentType, wantContentType)
			}

			wantSSE := f.Upstream.Headers[strings.ToLower(nativewire.SSEHeader)]
			gotSSE := gotHeader.Get(nativewire.SSEHeader)
			if !strings.EqualFold(gotSSE, wantSSE) {
				t.Errorf("上游收到 %s: %q，期望 %q", nativewire.SSEHeader, gotSSE, wantSSE)
			}

			testkit.AssertJSONEqual(t, f.Upstream.Body, gotBody, "上游收到的请求体语义不符")

			wantDegraded := chatDSNativeDegradedHeaders[caseName(f.Name)]
			gotDegraded := rec.Header().Get(degrade.DegradationHeader)
			if len(wantDegraded) == 0 {
				if gotDegraded != "" {
					t.Errorf("该用例不应有降级头，实际 %q", gotDegraded)
				}
			} else {
				for _, want := range wantDegraded {
					if !strings.Contains(gotDegraded, want) {
						t.Errorf("%s 应包含 %q，实际 %q", degrade.DegradationHeader, want, gotDegraded)
					}
				}
			}

			golden := filepath.Join(chatDSNativeRouteFixtures, "golden", caseName(f.Name)+".txt")
			testkit.Golden(t, golden, []byte(renderResult(rec)))
		})
	}
}

// TestChatDSNativeGoldenCount 钉死 golden 数量与 fixture 数量一致，且名单完全匹配。
func TestChatDSNativeGoldenCount(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join(chatDSNativeRouteFixtures, "golden"))
	if err != nil {
		t.Fatal(err)
	}
	gotNames := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".txt") {
			t.Fatalf("golden 目录混入非 .txt 条目 %q", e.Name())
		}
		gotNames = append(gotNames, strings.TrimSuffix(e.Name(), ".txt"))
	}
	assertChatDSNativeCaseNames(t, "golden", gotNames)
}
