//go:build smoke

package smoke_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/testkit"
	"github.com/yobo2u/omugw/internal/transport/sse"
)

// ============================================================================
// 录制装配预检
//
// 本节把真实录制的那套装配与断言（assertRecordedCase / buildRecordedFixture /
// saveRecordedFixture）在**本地假上游**上跑一遍，产物一律写进 t.TempDir。
//
// 它不产生任何 ADR-0001 意义上的证据——假源站的响应是手写的。它防的是另一件
// 事：录制器本身的 bug（断言写反、fixture 拼错、失败仍落盘）不该等到花着钱
// 打真实上游、且十一份证据已经写进仓库之后才被发现。
// ============================================================================

// stubOriginConfig 描述本地假源站要回什么。
type stubOriginConfig struct {
	stream   bool
	events   []testkit.SSEEvent
	jsonBody string
}

// newStubOrigin 启动一个本地假 DashScope 源站，供录制代理转发到它。
//
// 与 newPreflightUpstream 的区别是这个不捕获出站请求：本节的证据取自录制
// 代理的快照，源站只负责给一份形态正确的响应。
func newStubOrigin(t testing.TB, cfg stubOriginConfig) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}
		if !cfg.stream {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, cfg.jsonBody)
			return
		}
		sw, err := sse.NewWriter(w)
		if err != nil {
			http.Error(w, "sse writer error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		for _, ev := range cfg.events {
			if err := sw.Write(ev); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	return srv
}

// stubOriginFor 按用例产出一份能让转换器走完全程的假源站配置。
func stubOriginFor(c caseMeta) stubOriginConfig {
	candidates := preflightAssertions()[c.name].candidates
	cfg := stubOriginConfig{stream: c.stream, jsonBody: nativeJSONResponse(candidates)}
	switch c.name {
	case "tool_calling":
		cfg.events = nativeToolSSEEvents()
	case "reasoning":
		cfg.events = nativeReasoningSSEEvents()
	default:
		cfg.events = nativeSSEEvents(candidates)
	}
	return cfg
}

// runStubRecording 用本地假源站跑一遍完整的录制链路，返回录制器看到的东西。
func runStubRecording(t *testing.T, c caseMeta, model string) (
	*httptest.ResponseRecorder, recordingSnapshot, http.Header, []byte) {
	t.Helper()

	origin := newStubOrigin(t, stubOriginFor(c))
	proxy, state := newRecordingProxy(t, origin.URL)
	built := buildRecordingGateway(t, proxy.URL, c.door, model, preflightUpstreamSecret)

	clientBody := c.body(recordClientModel)
	req := httptest.NewRequest(http.MethodPost, string(degrade.EndpointOpenAIChat),
		bytes.NewReader(clientBody))
	req.Header.Set("Authorization", "Bearer "+testGatewayAuthKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	built.Mux.ServeHTTP(rec, req)

	return rec, state.Snapshot(), req.Header, clientBody
}

// TestRecorderAssemblesValidFixtureForEveryCase 把十一个用例的装配链路在本地
// 跑一遍：断言全通过、fixture 拼装合法、落盘产物能被严格校验器接受。
//
// 覆盖全部十一个用例而不是挑两个：真正会写错的恰恰是那几个形态特殊的
// （多模态门、n=2、跨帧工具参数），只跑 basic 与 streaming 等于把它们放过。
func TestRecorderAssemblesValidFixtureForEveryCase(t *testing.T) {
	for _, c := range recordCases {
		t.Run(c.name, func(t *testing.T) {
			model := modelForRole(c.modelRole)
			if model == "" {
				t.Fatalf("模型角色 %q 解析为空字符串", string(c.modelRole))
			}

			rec, snap, clientHeaders, clientBody := runStubRecording(t, c, model)

			assertRecordingReachedUpstream(t, snap)
			assertRecordedCase(t, c, model, rec, snap)

			f := buildRecordedFixture(c, clientHeaders, clientBody, snap)
			if err := f.Validate(); err != nil {
				t.Fatalf("拼装出的 fixture 不合法: %v", err)
			}

			// 写进 t.TempDir：仓库里的 fixture 目录只许由真实录制产生，
			// 一份本地假证据混进去就再也分不出哪些是真的。
			dir := t.TempDir()
			if !saveRecordedFixture(t, dir, f, !t.Failed()) {
				t.Fatal("断言全绿却没有落盘")
			}

			path := filepath.Join(dir, c.name+".json")
			// 用严格校验器复检这份产物：录制器与校验器对同一份 fixture 的
			// 理解必须一致，否则录出来的东西过不了自己的验收。
			assertFixtureMatchesCase(t, c, testkit.Load(t, path))
			assertNoSecretsInRawFixture(t, path)
		})
	}
}

// TestRecorderSkipsSaveWhenAssertionsFail 证明「断言没过就不落盘」这道闸门
// 真的关得住。
//
// 这条闸门是整个录制器最不能出错的地方：它一旦失效，一份被断言判定为不可信的
// 证据会照样写进仓库，而此后所有回放都会把它当成事实。
func TestRecorderSkipsSaveWhenAssertionsFail(t *testing.T) {
	c := recordCases[0]
	model := modelForRole(c.modelRole)
	_, snap, clientHeaders, clientBody := runStubRecording(t, c, model)

	f := buildRecordedFixture(c, clientHeaders, clientBody, snap)
	dir := t.TempDir()

	if saveRecordedFixture(t, dir, f, false) {
		t.Fatal("断言未通过时仍然落了盘")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取临时目录失败: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("断言未通过时目录里出现了 %d 个文件", len(entries))
	}

	// 反证：同一份 fixture 在断言通过时必须落得下去。少了这一步，一个
	// 永远返回 false 的 saveRecordedFixture 也能让上面那段全绿。
	if !saveRecordedFixture(t, dir, f, true) {
		t.Fatal("断言通过时反而没落盘")
	}
	if _, err := os.Stat(filepath.Join(dir, c.name+".json")); err != nil {
		t.Fatalf("断言通过后 fixture 文件不存在: %v", err)
	}
}
