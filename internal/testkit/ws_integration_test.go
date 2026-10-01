package testkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/credential"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/gateway"
	"github.com/yobo2u/omugw/internal/obs"
	"github.com/yobo2u/omugw/internal/provider"
	"github.com/yobo2u/omugw/internal/provider/dashscopecompat"
	dsnative "github.com/yobo2u/omugw/internal/provider/dashscopenative"
	"github.com/yobo2u/omugw/internal/provider/passthrough"
	"github.com/yobo2u/omugw/internal/router"
	"github.com/yobo2u/omugw/internal/transport/httpx"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

const wsIntegrationData = "../../testdata/testkit/ws"

// 手写发令与终态预期，防止从 ReplayWS 输出回填答案；此处链式因果不依赖跨端反馈顺序。
func TestWSIntegrationSamples(t *testing.T) {
	for _, tc := range []struct {
		file     string
		order    []string
		nodes    int
		terminal []WSTerminal
	}{
		{"hello.json", []string{"hello.c", "hello.u.reply", "close.c"}, 6, []WSTerminal{{Node: "hello.c.reply", Namespace: "response", Symbol: "hello-1", IDPointer: "/id", StatePointer: "/state", State: "done"}}},
		{"tool-id.json", []string{"tool.c", "tool.u.call", "tool.c.result", "tool.u.done", "close.c"}, 10, []WSTerminal{{Node: "tool.c.done", Namespace: "response", Symbol: "tool-response", IDPointer: "/id", StatePointer: "/state", State: "done"}}},
		{"binary.json", []string{"binary.c", "binary.u.reply", "binary.u.done", "close.c"}, 8, []WSTerminal{{Node: "binary.c.done", Namespace: "response", Symbol: "binary-1", IDPointer: "/id", StatePointer: "/state", State: "done"}}},
		{"two-tasks.json", []string{"a.start.c", "a.done.u", "b.start.c", "b.done.u", "close.c"}, 10, []WSTerminal{
			{Node: "a.done.c", Namespace: "task", Symbol: "task-a", IDPointer: "/id", StatePointer: "/state", State: "done"},
			{Node: "b.done.c", Namespace: "task", Symbol: "task-b", IDPointer: "/id", StatePointer: "/state", State: "done"},
		}},
	} {
		t.Run(tc.file, func(t *testing.T) {
			f := readWSIntegrationSample(t, tc.file)
			for _, seed := range []uint64{1, 2} {
				peers, u := wsReplayBridge(t, f, DefaultWSLimits(), nil)
				result, err := ReplayWS(context.Background(), f, peers, seed, DefaultWSLimits())
				result, err = finishWSIntegration(result, err, u)
				if err != nil || result.Outcome.Kind != "completed" || len(result.Nodes) != tc.nodes || !reflect.DeepEqual(result.SendOrder, tc.order) || !reflect.DeepEqual(result.Outcome.Terminal, tc.terminal) {
					t.Fatal("合成样例未取得完整消息、业务终态与关闭证据")
				}
				if tc.file == "two-tasks.json" {
					for _, pair := range []struct {
						index   int
						payload string
					}{
						{0, `{"type":"task.start","id":"task-a"}`},
						{4, `{"type":"task.start","id":"task-b"}`},
					} {
						if string(result.Nodes[pair.index].Message.Payload) != pair.payload {
							t.Fatal("同连接两 task 的独立实体预期丢失")
						}
					}
				}
				if tc.file == "binary.json" {
					for _, i := range []int{0, 1, 2, 3} {
						if result.Nodes[i].Message.Opcode != ws.OpBinary || !bytes.Equal(result.Nodes[i].Message.Payload, []byte{0, 255, 16}) {
							t.Fatal("二进制样例的 opcode 或字节被重编码")
						}
					}
				}
			}
		})
	}
}

// 断开上游对象的错误汇总会使 driver Done 假绿；迟到握手通过真实本地 HTTP 入站。
func TestWSIntegrationRejectsLateUpstreamError(t *testing.T) {
	f := readWSIntegrationSample(t, "hello.json")
	peers, u := wsReplayBridge(t, f, DefaultWSLimits(), nil)
	result, err := ReplayWS(context.Background(), f, peers, 1, DefaultWSLimits())
	if err != nil || result.Outcome.Kind != "completed" {
		t.Fatal("未取得 driver 完成证据，无法覆盖迟到错误")
	}
	srv := httptest.NewServer(u)
	defer srv.Close()
	client := &http.Client{Timeout: time.Second}
	resp, e := client.Get(srv.URL + "/unexpected?token=synthetic-private")
	if e != nil {
		t.Fatal("本地迟到握手未送达")
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 400 || u.Err() == nil {
		t.Fatal("错配握手没有进入上游首次错误槽")
	}
	result, err = finishWSIntegration(result, err, u)
	if err == nil || result.Outcome.Kind != "" || strings.Contains(err.Error(), "synthetic-private") {
		t.Fatal("driver Done 掩盖了 WSReplayUpstream.Err 或关闭错误")
	}
}

// 错实体与白名单外改写必须由实际接收证据拒绝，不能只验证静态 fixture 自洽。
func TestWSIntegrationRejectsBrokenForwarding(t *testing.T) {
	for _, mode := range []string{"wrong_reference", "unlisted_change", "binary_change", "second_task_wrong_id", "second_task_wrong_state"} {
		t.Run(mode, func(t *testing.T) {
			file := "tool-id.json"
			if mode == "binary_change" {
				file = "binary.json"
			} else if strings.HasPrefix(mode, "second_task_") {
				file = "two-tasks.json"
			}
			f := readWSIntegrationSample(t, file)
			mutate := func(point WSPoint, n int, msg WSMessage) []WSMessage {
				if point == WSClientSend {
					switch {
					case mode == "wrong_reference" && n == 1:
						msg.Payload = []byte(`{"type":"tool.result","call_id":"synthetic-private","result":0}`)
					case mode == "unlisted_change" && n == 0:
						msg.Payload = []byte(`{"model":"synthetic-model", "type":"tool.request"}`)
					case mode == "binary_change" && n == 0:
						msg.Payload = []byte{0, 255, 17}
					}
				}
				if point == WSUpstreamSend && n == 1 {
					switch mode {
					case "second_task_wrong_id":
						msg.Payload = []byte(`{"type":"task.done","id":"task-a","state":"done"}`)
					case "second_task_wrong_state":
						msg.Payload = []byte(`{"type":"task.done","id":"task-b","state":"failed"}`)
					}
				}
				return []WSMessage{msg}
			}
			peers, u := wsReplayBridge(t, f, DefaultWSLimits(), mutate)
			result, err := ReplayWS(context.Background(), f, peers, 1, DefaultWSLimits())
			result, err = finishWSIntegration(result, err, u)
			if err == nil || result.Outcome.Kind != "" || !strings.Contains(err.Error(), "ws.nodes[") || strings.Contains(err.Error(), "synthetic-private") {
				t.Fatal("错误引用、未声明转发变化或二进制变化被批准")
			}
		})
	}
}

// 专用有界 reader 不能因来源标签或文件存在而跳过来源、引用、摘要与图检查。
func TestWSIntegrationInvalidSamples(t *testing.T) {
	for _, file := range []string{"bad-source.json", "bad-reference.json", "bad-sample-hash.json", "cyclic.json"} {
		t.Run(file, func(t *testing.T) {
			path := filepath.Join(wsIntegrationData, "invalid", file)
			if _, err := os.Stat(path); err != nil {
				t.Fatal("非法样例文件缺失，不能以读取失败冒充拒绝轨迹")
			}
			_, err := ReadWSFixture(path, DefaultWSLimits())
			want := map[string]string{
				"bad-source.json":      "来源与 provenance 不符",
				"bad-reference.json":   "reference 缺少因果在先的绑定",
				"bad-sample-hash.json": "样本摘要不符",
				"cyclic.json":          "合图存在循环",
			}[file]
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatal("非法轨迹没有因指定契约违规被拒绝，不能以文件或语法错误冒充证据")
			}
		})
	}
	fixtures, err := ReadWSFixtureDir(filepath.Join(wsIntegrationData, "valid"), DefaultWSLimits())
	if err != nil || len(fixtures) != 4 {
		t.Fatal("合法样例目录加载失败或遗漏样例")
	}
	for _, f := range fixtures {
		if f.Response.WS.Provenance.Kind != "synthetic-negative" || !strings.Contains(f.Note, "非上游能力证据") {
			t.Fatal("合成样例冒充真实上游来源")
		}
	}
	for _, mode := range []string{"file", "directory", "message", "nodes", "rules", "trace"} {
		t.Run("budget_"+mode, func(t *testing.T) {
			limits := DefaultWSLimits()
			switch mode {
			case "file":
				limits.FileBytes = 32
			case "directory":
				limits.DirectoryBytes = 32
			case "message":
				limits.MessageBytes = 1
			case "nodes":
				limits.Nodes = 1
			case "rules":
				limits.FieldRules = 1
			case "trace":
				limits.TraceBytes = 1
			}
			_, err := ReadWSFixtureDir(filepath.Join(wsIntegrationData, "valid"), limits)
			want := map[string]string{"file": "文件字节预算", "directory": "文件字节预算", "message": "消息超过解码预算", "nodes": "节点超过预算", "rules": "字段规则超过预算", "trace": "轨迹解码负载超过预算"}[mode]
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatal("有界 reader 没有因指定预算拒绝超限样例")
			}
		})
	}
}

// 就绪门闩必须先于 Close，防止 unclaimed 分支偶然只测到尚未入槽的 Accept 竞态。
func TestWSIntegrationCloseReadyUnclaimed(t *testing.T) {
	f := readWSIntegrationSample(t, "hello.json")
	u, err := NewWSReplayUpstream(f, DefaultWSLimits())
	if err != nil {
		t.Fatal("合成上游构造失败")
	}
	srv := httptest.NewServer(u)
	t.Cleanup(func() { _ = u.Close(); srv.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	peer, _, err := ws.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/realtime", ws.DialOptions{MaxPayload: DefaultWSLimits().MessageBytes})
	if err != nil {
		t.Fatal("本地握手失败")
	}
	t.Cleanup(func() { _ = peer.Close(1000, "") })
	select {
	case <-u.ready:
	case <-ctx.Done():
		t.Fatal("未等待到接受连接入槽")
	}
	if u.Err() != nil {
		t.Fatal("连接就绪时已有上游错配")
	}
	// 不调用 Connection：测试的是已经入槽、从未领取的连接所有权。
	if u.Close() != nil || u.Err() != nil {
		t.Fatal("未领取连接正常关闭产生错误")
	}
	observed := readWSUpstreamTest(t, peer)
	var closed *ws.CloseError
	if !errors.As(observed.err, &closed) || closed.Code != 1000 {
		t.Fatal("未领取连接没有向实际对端发送关闭并释放")
	}
}

func readWSIntegrationSample(t *testing.T, file string) Fixture {
	t.Helper()
	f, err := ReadWSFixture(filepath.Join(wsIntegrationData, "valid", file), DefaultWSLimits())
	if err != nil {
		t.Fatal("合成样例加载失败")
	}
	return f
}

// driver 只拥有 Conn；外层 owner 必须汇总握手与 Close，失败时不能保留 completed 结果。
func finishWSIntegration(result WSReplayResult, replayErr error, u *WSReplayUpstream) (WSReplayResult, error) {
	closeErr := u.Close()
	if replayErr != nil {
		return WSReplayResult{}, replayErr
	}
	if u.Err() != nil || closeErr != nil {
		return WSReplayResult{}, errors.New("WS 整合上游握手或释放失败")
	}
	return result, nil
}

// 旧 HTTP/SSE 分支仍须走全部路径原 golden；WS 例外不能放宽 upstream 的三个必填字段。
func TestHTTPFixturesUnaffectedByWSSupport(t *testing.T) {
	entries, err := os.ReadDir("../../testdata/routes")
	if err != nil {
		t.Fatal("旧 fixture 根目录读取失败")
	}
	wantCounts := map[string]int{"dashscope.native__dashscope.native": 6, "openai.chat__dashscope.compatible": 10, "openai.chat__dashscope.native": 13, "openai.chat__openai.compat": 2, "openai.responses__openai.compat": 3}
	seen := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		seen++
		dir := filepath.Join("../../testdata/routes", entry.Name())
		fixtures := LoadDir(t, dir)
		goldens, err := filepath.Glob(filepath.Join(dir, "golden", "*.txt"))
		if err != nil || len(fixtures) != wantCounts[entry.Name()] || len(goldens) != len(fixtures) {
			t.Fatal("旧 fixture/golden 清单缺失或多出，不能跳过样例后仍称全部回归")
		}
		for _, f := range fixtures {
			t.Run(f.Name, func(t *testing.T) {
				if f.Response.WS != nil {
					t.Fatal("合成 WS 样例混进真实路径证据")
				}
				if (entry.Name() == "openai.chat__dashscope.compatible" || entry.Name() == "openai.chat__dashscope.native") && f.Upstream == nil {
					t.Fatal("旧异构路径缺少独立 upstream，不能用 WS 例外豁免")
				}
				if f.Upstream != nil {
					for _, field := range []string{"method", "path", "body"} {
						bad := f
						incomplete := *f.Upstream
						bad.Upstream = &incomplete
						switch field {
						case "method":
							incomplete.Method = ""
						case "path":
							incomplete.Path = ""
						case "body":
							incomplete.Body = nil
						}
						if bad.Validate() == nil {
							t.Fatal("WS 握手例外放宽了旧 HTTP upstream 必填契约")
						}
					}
				}
				h := httpIntegrationGateway(t, entry.Name(), f)
				req := httptest.NewRequest(f.Request.Method, f.Request.Path, bytes.NewReader(f.Request.Body))
				for k, v := range f.Request.Headers {
					if v != "<redacted>" {
						req.Header.Set(k, v)
					}
				}
				req.Header.Set("Authorization", "Bearer synthetic-client-key-0123456789")
				if req.Header.Get("Content-Type") == "" {
					req.Header.Set("Content-Type", "application/json")
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				name := f.Name
				if _, suffix, found := strings.Cut(f.Name, "/"); found {
					name = suffix
				}
				Golden(t, filepath.Join(dir, "golden", name+".txt"), []byte(renderHTTPIntegration(rec)))
			})
		}
	}
	if seen != len(wantCounts) {
		t.Fatal("旧 HTTP/SSE 路径被漏掉")
	}
	// provider 归档没有路径 golden，但仍必须按 method/path 实际回放完整 SSE 事件与原响应。
	archives, err := filepath.Glob("../../testdata/fixtures/*/*.json")
	if err != nil || len(archives) != 2 {
		t.Fatal("旧 provider 归档缺失")
	}
	for _, path := range archives {
		f := Load(t, path)
		t.Run(f.Name, func(t *testing.T) {
			srv := Server(t, f)
			req, err := http.NewRequest(f.Request.Method, srv.URL+f.Request.Path, bytes.NewReader(f.Request.Body))
			if err != nil {
				t.Fatal("归档回放请求构造失败")
			}
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal("归档回放失败")
			}
			defer resp.Body.Close()
			if resp.StatusCode != f.Response.Status {
				t.Fatal("归档状态码改变")
			}
			if f.Response.SSE != nil {
				got, err := ParseSSE(resp.Body)
				if err != nil || !reflect.DeepEqual(got, f.Response.SSE.Events) {
					t.Fatal("旧 SSE 事件回放改变")
				}
			} else {
				got, err := io.ReadAll(resp.Body)
				if err != nil || !bytes.Equal(got, f.Response.Body) {
					t.Fatal("旧 HTTP 字面响应改变")
				}
			}
		})
	}
}

// 只替换慢且不可重现的真实上游；网关矩阵、Provider、请求转换和下游编码均用现有实现。
func httpIntegrationGateway(t *testing.T, route string, f Fixture) *gateway.Handler {
	t.Helper()
	parts := strings.Split(route, "__")
	if len(parts) != 2 {
		t.Fatal("路径目录格式错误")
	}
	kind := degrade.Provider(parts[1])
	var requestModel struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(f.Request.Body, &requestModel) != nil || requestModel.Model == "" {
		t.Fatal("旧 fixture 请求缺少模型")
	}
	model := "upstream-model"
	if f.Upstream != nil {
		var expectedModel struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(f.Upstream.Body, &expectedModel) != nil || expectedModel.Model == "" {
			t.Fatal("旧 upstream 缺少模型")
		}
		model = expectedModel.Model
	}
	replayFixture := f
	if f.Upstream != nil {
		replayFixture.Request.Method, replayFixture.Request.Path = f.Upstream.Method, f.Upstream.Path
	}
	var calls atomic.Int32
	handler := Handler(func(_, _ string) { t.Error("旧回放收到未声明请求") }, func(_ string, _ ...any) { t.Error("旧回放写响应失败") }, replayFixture)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error("旧上游读取请求体失败")
			w.WriteHeader(500)
			return
		}
		if f.Upstream != nil {
			if r.Method != f.Upstream.Method || r.URL.Path != f.Upstream.Path {
				t.Error("旧异构上游 method/path 不符")
			}
			if !httpIntegrationJSONEqual(f.Upstream.Body, body) {
				t.Error("旧异构上游请求体不符")
			}
		} else {
			if r.Method != f.Request.Method || r.URL.Path != f.Request.Path {
				t.Error("旧同源上游 method/path 不符")
			}
			var want, got map[string]json.RawMessage
			if json.Unmarshal(f.Request.Body, &want) != nil || json.Unmarshal(body, &got) != nil {
				t.Error("旧同源请求体不是 JSON")
				return
			}
			if string(got["model"]) != `"upstream-model"` {
				t.Error("旧同源模型改写失效")
			}
			delete(want, "model")
			delete(got, "model")
			wantRaw, _ := json.Marshal(want)
			gotRaw, _ := json.Marshal(got)
			if !httpIntegrationJSONEqual(wantRaw, gotRaw) {
				t.Error("旧同源请求体字段丢失")
			}
		}
		if r.Header.Get("Authorization") != "Bearer synthetic-upstream-key" {
			t.Error("旧上游凭据改写失效")
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(up.Close)
	t.Cleanup(func() {
		if f.Name == "openai.responses__openai.compat/stateful_conversation" {
			if calls.Load() != 0 {
				t.Error("默认关闭的会话模拟触达上游")
			}
		} else if calls.Load() != 1 {
			t.Error("旧 fixture 未恰好触达一次上游")
		}
	})
	matrix, err := degrade.Phase1()
	if err != nil {
		t.Fatal("矩阵构造失败")
	}
	pool, err := credential.NewPool("synthetic", []credential.Credential{{ID: "synthetic", Secret: "synthetic-upstream-key"}}, credential.DefaultPolicy(), nil)
	if err != nil {
		t.Fatal("合成凭据池构造失败")
	}
	door := ""
	if kind == degrade.ProviderDashScopeNative {
		door = "multimodal-generation"
		if f.Upstream != nil && f.Upstream.Path == "/api/v1/services/aigc/text-generation/generation" {
			door = "text-generation"
		}
	}
	routes, err := router.New([]router.Rule{{Match: requestModel.Model, Targets: []router.Target{{Kind: kind, Endpoint: "synthetic", BaseURL: up.URL, UpstreamModel: model, CredentialPool: "synthetic", NativeEndpoint: door}}}})
	if err != nil {
		t.Fatal("旧回放路由构造失败")
	}
	now := func() time.Time { return time.Unix(1755216000, 0).UTC() }
	client := httpx.New(config.Default().Timeouts, now)
	var p provider.Provider
	switch kind {
	case degrade.ProviderOpenAICompat:
		p = passthrough.New(kind, "/sentinel", client, now)
	case degrade.ProviderDashScopeCompatible:
		p = dashscopecompat.New(client, now)
	case degrade.ProviderDashScopeNative:
		p = dsnative.New(client, now)
	default:
		t.Fatal("旧路径出现未声明 Provider")
	}
	deps := gateway.Deps{Matrix: matrix, Router: routes, Auth: gateway.NewAuthenticator([]config.AuthKey{{ID: "synthetic", Key: "synthetic-client-key-0123456789"}}),
		Limits: config.Default().Limits, Metrics: obs.NewMetrics(prometheus.NewRegistry()), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Pools: map[string]*credential.Pool{"synthetic": pool}, Providers: map[string]provider.Provider{"synthetic": p}, Now: now}
	switch parts[0] {
	case "openai.responses":
		return gateway.NewResponsesHandler(deps)
	case "openai.chat":
		return gateway.NewChatHandler(deps)
	case "dashscope.native":
		return gateway.NewDashScopeNativeHandler(deps)
	default:
		t.Fatal("旧路径出现未声明入站协议")
	}
	return nil
}

// 回调不能 Fatal 或回显实际请求体：坏 Provider 也应归还 HTTP 响应并由测试主流程失败。
func httpIntegrationJSONEqual(want, got []byte) bool {
	var a, b any
	return json.Unmarshal(want, &a) == nil && json.Unmarshal(got, &b) == nil && reflect.DeepEqual(a, b)
}

func renderHTTPIntegration(rec *httptest.ResponseRecorder) string {
	var b strings.Builder
	fmt.Fprintf(&b, "status: %s\n", http.StatusText(rec.Code))
	for _, key := range []string{"Content-Type", "X-Ratelimit-Remaining-Tokens", degrade.DegradationHeader, gateway.EmulationHeader} {
		if value := rec.Header().Get(key); value != "" {
			fmt.Fprintf(&b, "%s: %s\n", key, value)
		}
	}
	b.WriteString("---\n")
	b.WriteString(rec.Body.String())
	return b.String()
}
