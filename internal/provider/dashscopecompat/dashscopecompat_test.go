package dashscopecompat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/credential"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/provider"
	"github.com/yobo2u/omugw/internal/router"
	"github.com/yobo2u/omugw/internal/transport/httpx"
)

var refTime = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

// captured 记录上游实际收到了什么。
type captured struct {
	body   []byte
	header http.Header
	method string
	path   string
}

func serve(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *captured) {
	t.Helper()
	got := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.body, _ = io.ReadAll(r.Body)
		got.header = r.Header.Clone()
		got.method = r.Method
		got.path = r.URL.Path
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

// okServer 是最常见的桩：收下请求，回一个空的 2xx JSON。
func okServer(t *testing.T) (*httptest.Server, *captured) {
	t.Helper()
	return serve(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
}

// callInput 是一次适配器调用的测试输入，是 provider.Request 在测试侧的投影。
//
// 收成一个具名类型而不是继续加形参：调用点写 `call(t, srv, "raw", "m", false)`
// 时，读的人无从判断末尾那个 false 是 stream 还是别的开关，加第六项时更是要
// 逐个调用点数位置。零值即缺省——path 留空验证缺省退回，baseURL 留空用测试服务器。
type callInput struct {
	raw           string
	upstreamModel string
	stream        bool

	path    string
	baseURL string

	// header 是客户端原始请求头。适配器不得把它们转给上游。
	header http.Header
}

// call 发起一次适配器调用，成功时把响应体的关闭登记进 t.Cleanup。
//
// 在这里登记而不是让每个调用点自己关：httpx 的 Body 上挂着空闲计时器与整体
// 超时的 cancel，漏关会把它们一起漏掉；而且 httptest.Server.Close 会等未完成的
// 连接，漏关能让收尾挂住。t.Cleanup 是 LIFO，serve 先登记 srv.Close，
// 这里后登记 Body.Close，于是先关体、后关服务器——顺序正是要的那个。
func call(t *testing.T, srv *httptest.Server, in callInput) (*httpx.Response, error) {
	t.Helper()

	p := New(httpx.New(config.Default().Timeouts, func() time.Time { return refTime }),
		func() time.Time { return refTime })

	baseURL := in.baseURL
	if baseURL == "" {
		baseURL = srv.URL
	}

	resp, err := p.Call(context.Background(), provider.Request{
		Target: router.Target{
			Kind:           degrade.ProviderDashScopeCompatible,
			Endpoint:       "test",
			BaseURL:        baseURL,
			UpstreamModel:  in.upstreamModel,
			CredentialPool: "test",
		},
		Credential: credential.Credential{ID: "k1", Secret: "sk-gateway-own-key"},
		Raw:        []byte(in.raw),
		Stream:     in.stream,
		Path:       in.path,
		Header:     in.header,
	})
	if err == nil && resp != nil {
		t.Cleanup(func() { resp.Body.Close() })
	}
	return resp, err
}

// upstreamFields 把上游实际收到的请求体解成字段表，供语义比对。
//
// 按语义比对而不是按字节：重新序列化会改变键序，钉死字节等于钉死
// encoding/json 的实现细节，而那不是我们要保证的契约。
func upstreamFields(t *testing.T, got *captured) map[string]json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(got.body, &fields); err != nil {
		t.Fatalf("上游收到的不是 JSON 对象: %s", got.body)
	}
	return fields
}

// TestOfficialBaseURLDoesNotRepeatVersion：官方 base_url 已经包含
// /compatible-mode/v1，适配器不得再把入站路径开头的 /v1 重复拼进去。
func TestOfficialBaseURLDoesNotRepeatVersion(t *testing.T) {
	srv, got := okServer(t)

	if _, err := call(t, srv, callInput{
		raw:           `{"model":"m","messages":[]}`,
		upstreamModel: "m",
		baseURL:       srv.URL + "/compatible-mode/v1",
		path:          ChatCompletionsPath,
	}); err != nil {
		t.Fatal(err)
	}

	want := "/compatible-mode/v1/chat/completions"
	if got.path != want {
		t.Errorf("path = %q，期望官方 base_url 只保留一个版本段 %q", got.path, want)
	}
}
