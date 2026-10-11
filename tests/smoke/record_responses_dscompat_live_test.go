//go:build smoke

package smoke_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/testkit"
)

const (
	rdcRecordEnv    = "OMUGW_RECORD_DSCOMPAT_RESPONSES"
	rdcModelEnv     = "OMUGW_RECORD_DSCOMPAT_RESPONSES_MODEL"
	rdcOnlyEnv      = "OMUGW_RECORD_DSCOMPAT_RESPONSES_ONLY"
	rdcKeyEnv       = "DASHSCOPE_API_KEY"
	rdcDefaultBase  = "https://dashscope.aliyuncs.com/compatible-mode/v1"
	rdcDefaultModel = "qwen3.8-flash"
)

// TestRecordResponsesDSCompat 向真实 DashScope Compatible 的 Responses 端点
// 录制 fixture。
//
// 必须显式设置 OMUGW_RECORD_DSCOMPAT_RESPONSES=1 才执行：它会产生账单。
// 凭据只从环境变量读，绝不写进日志、错误信息或产物——失败信息只报状态码与
// 响应体，而响应体里没有凭据。
func TestRecordResponsesDSCompat(t *testing.T) {
	if os.Getenv(rdcRecordEnv) != "1" {
		t.Skipf("跳过真实录制：未设置 %s=1", rdcRecordEnv)
	}
	key := strings.TrimSpace(os.Getenv(rdcKeyEnv))
	if key == "" {
		t.Fatalf("缺少环境变量 %s", rdcKeyEnv)
	}
	model := strings.TrimSpace(os.Getenv(rdcModelEnv))
	if model == "" {
		model = rdcDefaultModel
	}
	// 逗号分隔的用例名；留空录全部。单个用例重录时用它，免得其余用例跟着漂移。
	only := map[string]bool{}
	for _, name := range strings.Split(os.Getenv(rdcOnlyEnv), ",") {
		if name = strings.TrimSpace(name); name != "" {
			only[name] = true
		}
	}

	client := &http.Client{Timeout: 4 * time.Minute}
	send := func(t *testing.T, body string) (int, string, []byte) {
		t.Helper()
		payload, err := rdcSetModel(body, model)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest(http.MethodPost, rdcDefaultBase+"/responses", bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			// 只报错误类别：*url.Error 的文本含 URL 不含请求头，但不值得赌。
			t.Fatalf("请求上游失败（传输层）")
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("读取上游响应失败")
		}
		return resp.StatusCode, resp.Header.Get("Content-Type"), raw
	}

	for _, c := range rdcCases() {
		if len(only) > 0 && !only[c.name] {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			var seed rdcSeed
			if c.seed != "" {
				status, _, raw := send(t, c.seed)
				if status != http.StatusOK {
					t.Fatalf("种子轮状态码 = %d: %s", status, raw)
				}
				var parsed struct {
					ID     string          `json:"id"`
					Output json.RawMessage `json:"output"`
				}
				if err := json.Unmarshal(raw, &parsed); err != nil || parsed.ID == "" {
					t.Fatalf("种子轮响应没有 id: %s", raw)
				}
				seed = rdcSeed{ID: parsed.ID, Output: parsed.Output}
				// 防的是花钱录一份没有工具结果的 tool-result fixture：种子轮
				// 若没产出 function_call，后续请求不会带任何 function_call_output。
				if c.name == "tool_calling-result" && !rdcHasFunctionCall(seed.Output) {
					t.Fatalf("种子轮 output 没有 function_call，不录制: %s", raw)
				}
			}

			clientBody := c.build(seed)
			expect := clientBody
			if c.upstream != nil {
				expect = c.upstream(clientBody)
			}
			status, contentType, raw := send(t, expect)
			// 状态码与预期不符就不落盘：一份意外的 4xx 入库后会被当成证据。
			if status != c.status() {
				t.Fatalf("状态码 = %d，期望 %d: %s", status, c.status(), raw)
			}

			f, err := rdcFixture(c, clientBody, status, contentType, raw)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(rdcFixtureDir, c.name+".json")
			if err := testkit.Save(path, f); err != nil {
				t.Fatalf("保存 fixture 失败: %v", err)
			}
			t.Logf("已录制 %s（model=%s status=%d bytes=%d）", c.name, model, status, len(raw))
		})
	}
}
