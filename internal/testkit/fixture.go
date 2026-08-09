// Package testkit 是一致性测试的基座。
//
// 它存在的理由：网关要覆盖 (入站协议 × 出站 Provider × 能力) 的组合矩阵，
// 而这个矩阵的规模会随着每加一个协议而相乘。靠人工写断言维护不住，靠打真实
// 上游又慢又贵还不稳定。
//
// 做法是：真实上游的响应**录制一次**，脱敏后入库 testdata/fixtures/，
// 此后所有回归测试都离线回放。CI 不需要任何 API Key。
package testkit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fixture 是一次上游交互的录制。
type Fixture struct {
	Name string `json:"name"`
	// Note 记录这条 fixture 覆盖的是什么场景，例如「工具参数跨分片切断」。
	// 没有它，半年后没人知道某个奇怪的 fixture 为什么长这样。
	Note     string   `json:"note,omitempty"`
	Request  Request  `json:"request"`
	Response Response `json:"response"`
}

// Request 是发往上游的请求。
type Request struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Query   string            `json:"query,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    json.RawMessage   `json:"body,omitempty"`
}

// Response 是上游返回。Body 与 SSE 互斥。
type Response struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    json.RawMessage   `json:"body,omitempty"`
	SSE     *SSEBody          `json:"sse,omitempty"`
}

// SSEBody 是录制下来的事件流。
type SSEBody struct {
	Events []SSEEvent `json:"events"`

	// Frames 定义回放时的写入边界：每个元素是一次 Write 涵盖的事件数。
	//
	// 保留它是因为上游的真实分片方式会影响转换器的行为——一次 Write 里塞进
	// 三个事件、或者把一个事件切成两次 Write，都能暴露出缓冲逻辑的 bug。
	// 为空则逐事件写出。
	Frames []int `json:"frames,omitempty"`
}

// secretHeaders 是绝不入库的请求头。
//
// 录制脚本会打真实上游，凭据必然出现在请求头里。这份名单是 fixture 不泄密的
// 唯一保障，宁可多列不可漏列。
var secretHeaders = map[string]bool{
	"authorization":         true,
	"x-api-key":             true,
	"api-key":               true,
	"x-goog-api-key":        true,
	"x-dashscope-api-key":   true,
	"cookie":                true,
	"set-cookie":            true,
	"proxy-authorization":   true,
	"x-amz-security-token":  true,
	"openai-organization":   true,
	"x-request-id":          true,
	"x-dashscope-requestid": true,
}

// SanitizeHeaders 剥掉凭据类请求头，并把键名归一成小写。
//
// 被剥掉的头会留下一个 "<redacted>" 占位而不是直接消失——测试需要知道
// 「这里本来有个 Authorization」，才能验证网关确实注入了鉴权。
func SanitizeHeaders(h http.Header) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, vs := range h {
		lk := strings.ToLower(k)
		if secretHeaders[lk] {
			out[lk] = "<redacted>"
			continue
		}
		if len(vs) > 0 {
			out[lk] = vs[0]
		}
	}
	return out
}

// Load 读取一个 fixture 文件。
func Load(t *testing.T, path string) Fixture {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("testkit: 读取 fixture %s 失败: %v", path, err)
	}
	var f Fixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("testkit: 解析 fixture %s 失败: %v", path, err)
	}
	if err := f.Validate(); err != nil {
		t.Fatalf("testkit: fixture %s 不合法: %v", path, err)
	}
	return f
}

// LoadDir 读取一个目录下的全部 fixture，按文件名排序。
func LoadDir(t *testing.T, dir string) []Fixture {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("testkit: 读取 fixture 目录 %s 失败: %v", dir, err)
	}
	var out []Fixture
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		out = append(out, Load(t, filepath.Join(dir, e.Name())))
	}
	if len(out) == 0 {
		t.Fatalf("testkit: 目录 %s 中没有 fixture", dir)
	}
	return out
}

// Save 把 fixture 写入文件。录制脚本用它，普通测试不该调用。
func Save(path string, f Fixture) error {
	if err := f.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}

// Validate 检查 fixture 自身的一致性，并做一次兜底的泄密检查。
func (f Fixture) Validate() error {
	if f.Name == "" {
		return fmt.Errorf("fixture 缺少 name")
	}
	if f.Response.Status == 0 {
		return fmt.Errorf("fixture %q 缺少 response.status", f.Name)
	}
	if f.Response.Body != nil && f.Response.SSE != nil {
		return fmt.Errorf("fixture %q 同时有 body 和 sse，两者互斥", f.Name)
	}
	// 兜底：即使录制脚本忘了脱敏，这里也要拦下来。
	for k, v := range f.Request.Headers {
		if secretHeaders[strings.ToLower(k)] && v != "<redacted>" {
			return fmt.Errorf("fixture %q 的请求头 %q 未脱敏", f.Name, k)
		}
	}
	if f.Response.SSE != nil {
		total := 0
		for _, n := range f.Response.SSE.Frames {
			if n <= 0 {
				return fmt.Errorf("fixture %q 的 sse.frames 含非正数", f.Name)
			}
			total += n
		}
		if total > 0 && total != len(f.Response.SSE.Events) {
			return fmt.Errorf("fixture %q 的 sse.frames 合计 %d，与事件数 %d 不符",
				f.Name, total, len(f.Response.SSE.Events))
		}
	}
	return nil
}

// Server 启动一个回放这些 fixture 的 httptest 服务器。
//
// 请求按 method + path 匹配。**匹配不上就让测试失败**而不是返回 404：
// 一个没被录制的请求说明被测代码打了预期之外的端点，静默放过会让这类 bug
// 一直活到线上。
func Server(t *testing.T, fixtures ...Fixture) *httptest.Server {
	t.Helper()

	h := Handler(func(method, path string) {
		t.Errorf("testkit: 收到未录制的请求 %s %s", method, path)
	}, func(format string, args ...any) {
		t.Errorf(format, args...)
	}, fixtures...)

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	return srv
}

// Handler 构造回放处理器。onMiss 在收到未录制的请求时被调用，onError 在写出
// 响应失败时被调用。
//
// 之所以把回调抽出来而不是直接用 *testing.T：否则「未录制的请求会让测试失败」
// 这条行为本身就没法被测试——而它恰恰是这个框架最需要保证的性质。
func Handler(onMiss func(method, path string), onError func(string, ...any), fixtures ...Fixture) http.Handler {
	index := make(map[string]Fixture, len(fixtures))
	for _, f := range fixtures {
		index[key(f.Request.Method, f.Request.Path)] = f
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := index[key(r.Method, r.URL.Path)]
		if !ok {
			if onMiss != nil {
				onMiss(r.Method, r.URL.Path)
			}
			http.Error(w, "no fixture for this request", http.StatusInternalServerError)
			return
		}
		writeResponse(w, f.Response, onError)
	})
}

func key(method, path string) string {
	if method == "" {
		method = http.MethodPost
	}
	return method + " " + path
}

func writeResponse(w http.ResponseWriter, resp Response, onError func(string, ...any)) {
	for k, v := range resp.Headers {
		w.Header().Set(k, v)
	}

	if resp.SSE == nil {
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(resp.Status)
		if len(resp.Body) > 0 {
			_, _ = w.Write(resp.Body)
		}
		return
	}

	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "text/event-stream")
	}
	w.WriteHeader(resp.Status)

	flusher, _ := w.(http.Flusher)
	for _, batch := range frames(resp.SSE) {
		var buf bytes.Buffer
		if err := WriteSSE(&buf, batch); err != nil {
			if onError != nil {
				onError("testkit: 写出 SSE 失败: %v", err)
			}
			return
		}
		if _, err := w.Write(buf.Bytes()); err != nil {
			return
		}
		// 每帧后 flush，复现真实的分片到达节奏——转换器的缓冲 bug 只在
		// 分片边界上才会暴露。
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// frames 按 Frames 定义把事件切成写入批次。
func frames(sse *SSEBody) [][]SSEEvent {
	if len(sse.Frames) == 0 {
		out := make([][]SSEEvent, 0, len(sse.Events))
		for _, ev := range sse.Events {
			out = append(out, []SSEEvent{ev})
		}
		return out
	}
	var (
		out [][]SSEEvent
		i   int
	)
	for _, n := range sse.Frames {
		out = append(out, sse.Events[i:i+n])
		i += n
	}
	return out
}
