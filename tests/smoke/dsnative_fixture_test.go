//go:build smoke

package smoke_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/testkit"
)

// fixtureDir 是 Chat -> DashScope Native 路径录制产出的 fixture 存储目录。
const fixtureDir = "../../testdata/routes/openai.chat__dashscope.native"

// fixtureNamePrefix 是本路径 fixture 的 name 前缀，从目录名派生。
//
// 不写死字面量：目录名由 degrade.FixtureDir 决定，两处各写一遍时改了一处就会
// 让 fixture 的 name 与它所在的路径分家，而两者都合法、校验也过得去。
var fixtureNamePrefix = filepath.Base(filepath.Clean(fixtureDir)) + "/"

// buildRecordedFixture 把客户端请求、捕获的上游请求与真实响应拼成一份 fixture。
//
// 纯组装，不落盘也不断言：落不落盘由调用方按断言结果决定。
func buildRecordedFixture(c caseMeta, clientHeaders http.Header, clientBody []byte,
	snap recordingSnapshot) testkit.Fixture {
	upstream := cloneUpstream(snap.Upstream)
	return testkit.Fixture{
		Name: fixtureNamePrefix + c.name,
		Note: c.note,
		Request: testkit.Request{
			Method:  http.MethodPost,
			Path:    string(degrade.EndpointOpenAIChat),
			Headers: testkit.SanitizeHeaders(clientHeaders),
			Body:    json.RawMessage(bytes.Clone(clientBody)),
		},
		Upstream: &upstream,
		Response: cloneResponse(snap.Response),
	}
}

// saveRecordedFixture 只在断言全绿时落盘，并报告是否真的写了文件。
//
// 把判据收成入参而不是就地读 t.Failed()：这道闸门本身必须能被离线测到——
// 一道从没被验证过的「失败就不落盘」，正是最容易在重构里悄悄失效的那种保护。
func saveRecordedFixture(t *testing.T, dir string, f testkit.Fixture, assertionsPassed bool) bool {
	t.Helper()

	if !assertionsPassed {
		t.Logf("用例 %s 有断言未通过，不落盘——没通过断言的证据比没有证据更糟", f.Name)
		return false
	}
	path := filepath.Join(dir, strings.TrimPrefix(f.Name, fixtureNamePrefix)+".json")
	if err := testkit.Save(path, f); err != nil {
		t.Fatalf("落盘 fixture %s 失败: %v", path, err)
	}
	return true
}

// assertRecordingReachedUpstream 断言这次录制确实完整地打到了真实上游。
//
// 先于一切内容断言：代理没被打到、被打了两次、或上游给了非 200，都会让后面
// 那些断言在一份根本不存在的证据上报出一串误导性的失败。
func assertRecordingReachedUpstream(t *testing.T, snap recordingSnapshot) {
	t.Helper()

	if snap.Err != nil {
		t.Fatalf("录制代理出错: %v", snap.Err)
	}
	if snap.Requests != 1 {
		t.Fatalf("录制代理收到 %d 次请求，期望恰好 1 次", snap.Requests)
	}
	if snap.Upstream.Method == "" || snap.Upstream.Path == "" || len(snap.Upstream.Body) == 0 {
		t.Fatal("没有捕获到完整的上游请求")
	}
	if snap.Response.Status == 0 {
		t.Fatal("没有捕获到上游响应")
	}
	if snap.Response.Status != http.StatusOK {
		// 上游错误体常带请求 ID，对排查有用；它不含凭据，可以原样打出来。
		// 打的是**响应**体，不是请求头——真实 Key 只存在于后者。
		t.Fatalf("上游返回 %d，本次不落盘。上游响应体: %s",
			snap.Response.Status, upstreamErrorDetail(snap))
	}
}

// upstreamErrorDetail 取出上游错误响应的可打印内容。
func upstreamErrorDetail(snap recordingSnapshot) string {
	if len(snap.Response.Body) > 0 {
		return string(snap.Response.Body)
	}
	if snap.Response.SSE != nil && len(snap.Response.SSE.Events) > 0 {
		return testkit.EncodeSSE(snap.Response.SSE.Events)
	}
	return "(空)"
}
