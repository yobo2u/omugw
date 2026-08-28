//go:build smoke

package smoke_test

import (
	"bytes"
	"encoding/json"
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

// TestRecordedFixturesAreValid 校验录制产出的 fixture 是否合法且完备。
//
// 只在目录整个不存在时跳过（尚未录制）。目录一旦出现，就必须是十三份一个不多
// 一个不少的完整证据——「录到一半」在回放侧看起来与「本来就只有这些」毫无区别。
func TestRecordedFixturesAreValid(t *testing.T) {
	dir := filepath.Clean(fixtureDir)
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skip("跳过校验：fixture 目录尚未生成")
		}
		t.Fatalf("读取 fixture 目录 %s 失败: %v", dir, err)
	}
	if !info.IsDir() {
		t.Fatalf("fixture 路径 %s 不是目录", dir)
	}

	byName := loadRecordedFixtures(t, dir)
	for _, c := range recordCases {
		t.Run(c.name, func(t *testing.T) {
			assertFixtureMatchesCase(t, c, byName[c.name])
		})
	}
}

// loadRecordedFixtures 读入目录下的全部 fixture，并与用例名单双向对账。
func loadRecordedFixtures(t *testing.T, dir string) map[string]testkit.Fixture {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取 fixture 目录 %s 失败: %v", dir, err)
	}

	want := make([]string, 0, len(recordCases))
	for _, c := range recordCases {
		want = append(want, c.name)
	}

	byName := make(map[string]testkit.Fixture, len(recordCases))
	got := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			// 多出来的非 fixture 文件同样要拦：一个手写进来的 .txt 说明
			// 这个目录已经不只是录制产物了。
			t.Errorf("fixture 目录混入了非 fixture 条目 %q", e.Name())
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		got = append(got, name)
		path := filepath.Join(dir, e.Name())
		// Load 内部已跑过 Fixture.Validate，形态与脱敏的兜底检查在那里完成。
		byName[name] = testkit.Load(t, path)
		assertNoSecretsInRawFixture(t, path)
	}

	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("fixture 文件名单 = %v，期望恰好 %v——录到一半在回放侧与"+
			"「本来就只有这些」毫无区别", got, want)
	}
	return byName
}

// assertNoSecretsInRawFixture 扫描 fixture 原始字节，确认没有凭据残留。
//
// 扫原始文件而不是解析后的结构：凭据可能藏在任何一个未建模的角落，
// 而结构化视图只看得见建模过的那些字段。失败时只报文件名——把命中的那段
// 打出来，等于把刚拦下的密钥又抄进了 CI 日志。
func assertNoSecretsInRawFixture(t *testing.T, path string) {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 fixture %s 失败: %v", path, err)
	}
	name := filepath.Base(path)
	if bytes.Contains(raw, []byte("Bearer sk-")) {
		t.Errorf("fixture %s 含未脱敏的 Bearer 凭据", name)
	}
	if key := strings.TrimSpace(os.Getenv("DASHSCOPE_API_KEY")); key != "" &&
		bytes.Contains(raw, []byte(key)) {
		t.Errorf("fixture %s 含当前环境的 DASHSCOPE_API_KEY", name)
	}
}

// assertFixtureMatchesCase 断言一份 fixture 与它所对应用例的声明完全自洽。
func assertFixtureMatchesCase(t *testing.T, c caseMeta, f testkit.Fixture) {
	t.Helper()

	if want := fixtureNamePrefix + c.name; f.Name != want {
		t.Errorf("fixture name = %q，期望 %q", f.Name, want)
	}
	assertFixtureRequest(t, f.Request)
	assertFixtureUpstream(t, c, f.Upstream)
	assertFixtureResponse(t, c, f.Response)
}

// assertFixtureRequest 断言 fixture 里的客户端请求形态。
func assertFixtureRequest(t *testing.T, req testkit.Request) {
	t.Helper()

	if req.Method != http.MethodPost {
		t.Errorf("request.method = %q，期望 POST", req.Method)
	}
	if want := string(degrade.EndpointOpenAIChat); req.Path != want {
		t.Errorf("request.path = %q，期望 %q", req.Path, want)
	}
	if !json.Valid(req.Body) {
		t.Errorf("request.body 不是合法 JSON")
	}
	if got := req.Headers["authorization"]; got != "<redacted>" {
		t.Errorf("request.headers[authorization] 未按预期脱敏")
	}
}

// assertFixtureUpstream 断言 fixture 里的上游请求形态。
//
// 异构路径的 fixture 必须带 upstream：只比客户端响应会漏掉「Provider 没做
// 映射、fixture 仍返回成功」这一类假绿。
func assertFixtureUpstream(t *testing.T, c caseMeta, up *testkit.UpstreamExpectation) {
	t.Helper()

	if up == nil {
		t.Fatal("fixture 缺少 upstream 断言，上游映射无从对账")
	}
	if up.Method != http.MethodPost {
		t.Errorf("upstream.method = %q，期望 POST", up.Method)
	}
	if want := c.door.Path(); up.Path != want {
		t.Errorf("upstream.path = %q，期望门 %q 的 %q", up.Path, string(c.door), want)
	}
	if !json.Valid(up.Body) {
		t.Errorf("upstream.body 不是合法 JSON")
	}
	if got := up.Headers["authorization"]; got != "<redacted>" {
		t.Errorf("upstream.headers[authorization] 未按预期脱敏")
	}

	sseKey := strings.ToLower(nativewire.SSEHeader)
	if c.stream {
		if got := up.Headers[sseKey]; !strings.EqualFold(got, "enable") {
			t.Errorf("upstream.headers[%s] = %q，期望 enable", sseKey, got)
		}
		return
	}
	if got, ok := up.Headers[sseKey]; ok && got != "" {
		t.Errorf("非流式 fixture 的 upstream.headers[%s] = %q，期望不存在", sseKey, got)
	}
}

// assertFixtureResponse 断言 fixture 里的上游响应形态与流式声明一致。
func assertFixtureResponse(t *testing.T, c caseMeta, resp testkit.Response) {
	t.Helper()

	if resp.Status != http.StatusOK {
		t.Errorf("response.status = %d，期望 200", resp.Status)
	}
	// 响应头一律只留 content-type：白名单之外的头（request_id / Set-Cookie /
	// 各家追踪字段）不该随证据一起入库。
	keys := make([]string, 0, len(resp.Headers))
	for k := range resp.Headers {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if want := []string{"content-type"}; !slices.Equal(keys, want) {
		t.Errorf("response.headers 的键 = %v，期望恰好 %v", keys, want)
	}

	if !c.stream {
		if resp.SSE != nil {
			t.Errorf("非流式 fixture 带了 sse，共 %d 条事件", len(resp.SSE.Events))
		}
		if len(resp.Body) == 0 {
			t.Fatal("非流式 fixture 缺少 response.body")
		}
		if !json.Valid(resp.Body) {
			t.Error("非流式 fixture 的 response.body 不是合法 JSON")
		}
		return
	}

	if resp.Body != nil {
		t.Error("流式 fixture 不该同时有 response.body（与 sse 互斥）")
	}
	if resp.SSE == nil {
		t.Fatal("流式 fixture 缺少 response.sse")
	}
	if len(resp.SSE.Events) == 0 {
		t.Fatal("流式 fixture 的事件流为空")
	}
	total := 0
	for i, n := range resp.SSE.Frames {
		if n <= 0 {
			t.Errorf("sse.frames[%d] = %d，期望正数", i, n)
		}
		total += n
	}
	if total != len(resp.SSE.Events) {
		t.Errorf("sse.frames 合计 = %d，与事件数 %d 不符", total, len(resp.SSE.Events))
	}
}
