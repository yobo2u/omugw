package testkit

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/transport/ws"
)

// 防止合成机制测试借用生产编码器生成预期，再用同一错误逻辑证明自己正确。
func syntheticEnvelope() Fixture {
	closeCode := uint16(1000)
	return Fixture{
		Name:    "synthetic-ws-envelope",
		Note:    "合成轨迹只验证 WS 信封与传输结局，不证明真实上游能力",
		Request: Request{Method: "GET", Path: "/v1/realtime", Query: "model=synthetic-model"},
		Response: Response{
			Status: 101,
			WS: &WSSession{
				Version: 1, ClientProtocol: "openai.realtime", ClientVersion: "synthetic-v1",
				Upstream:               Request{Method: "GET", Path: "/v1/realtime", Query: "model=synthetic-model"},
				UpstreamExpectedStatus: 101,
				Provenance: WSProvenance{
					Kind: "synthetic", UpstreamProtocol: "openai.realtime",
					UpstreamVersion: "synthetic-v1", Model: "synthetic-model",
				},
				Coverage: []WSCoverage{{
					Capability: "text_generation", Nodes: []string{"u-send", "c-receive"},
					Note: "只验证合成终态消息的机制，不作为能力投放证据",
				}},
				Nodes: []WSNode{
					{
						ID: "c-send", Point: WSClientSend, Source: "synthetic", Kind: "message", Match: "json",
						Message: &WSMessage{Opcode: ws.OpText, Payload: []byte(`{"type":"response.create"}`)},
					},
					{
						ID: "u-receive", Point: WSUpstreamReceive, Source: "synthetic", After: []string{"c-send"},
						Kind: "message", Match: "json", ForwardedFrom: "c-send",
						Message: &WSMessage{Opcode: ws.OpText, Payload: []byte(`{"type":"response.create"}`)},
					},
					{
						ID: "u-send", Point: WSUpstreamSend, Source: "synthetic", After: []string{"u-receive"},
						Kind: "message", Match: "json",
						Message: &WSMessage{Opcode: ws.OpText, Payload: []byte(`{"type":"response.done","response":{"id":"r-1","status":"completed"}}`)},
						Fields:  []WSFieldRule{{Pointer: "/response/id", Mode: "bind", Namespace: "response", Symbol: "r-1"}},
					},
					{
						ID: "c-receive", Point: WSClientReceive, Source: "synthetic", After: []string{"u-send"},
						Kind: "message", Match: "json", ForwardedFrom: "u-send",
						Message: &WSMessage{Opcode: ws.OpText, Payload: []byte(`{"type":"response.done","response":{"id":"r-1","status":"completed"}}`)},
						Fields:  []WSFieldRule{{Pointer: "/response/id", Mode: "ref", Namespace: "response", Symbol: "r-1"}},
					},
					{ID: "c-close", Point: WSClientSend, Source: "synthetic", After: []string{"c-receive"}, Kind: "close", CloseCode: &closeCode},
					{ID: "u-close-receive", Point: WSUpstreamReceive, Source: "synthetic", After: []string{"c-close"}, Kind: "close", CloseCode: &closeCode},
					{ID: "u-close", Point: WSUpstreamSend, Source: "synthetic", After: []string{"u-close-receive"}, Kind: "close", CloseCode: &closeCode},
					{ID: "c-close-receive", Point: WSClientReceive, Source: "synthetic", After: []string{"u-close"}, Kind: "close", CloseCode: &closeCode},
				},
				Outcome: WSOutcome{Kind: "completed", Terminal: []WSTerminal{{
					Node: "c-receive", Namespace: "response", Symbol: "r-1",
					IDPointer: "/response/id", StatePointer: "/response/status", State: "completed",
				}}},
			},
		},
	}
}

// 防止 WS 被当作空 HTTP 响应接纳，或 query 混进门路径导致端点举证对账失效。
func TestWSFixtureEnvelope(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Fixture)
		wantErr bool
	}{
		{"valid synthetic", func(*Fixture) {}, false},
		{"body and ws", func(f *Fixture) { f.Response.Body = json.RawMessage(`{}`) }, true},
		{"empty body and ws", func(f *Fixture) { f.Response.Body = json.RawMessage{} }, true},
		{"sse and ws", func(f *Fixture) { f.Response.SSE = &SSEBody{} }, true},
		{"body and sse", func(f *Fixture) {
			f.Response.WS = nil
			f.Response.Status = 200
			f.Response.Body = json.RawMessage(`{}`)
			f.Response.SSE = &SSEBody{}
		}, true},
		{"all three", func(f *Fixture) {
			f.Response.Body = json.RawMessage(`{}`)
			f.Response.SSE = &SSEBody{}
		}, true},
		{"legacy upstream", func(f *Fixture) {
			f.Upstream = &UpstreamExpectation{Method: "POST", Path: "/v1/chat/completions", Body: json.RawMessage(`{}`)}
		}, true},
		{"downstream missing method", func(f *Fixture) { f.Request.Method = "" }, true},
		{"downstream post", func(f *Fixture) { f.Request.Method = "POST" }, true},
		{"downstream missing path", func(f *Fixture) { f.Request.Path = "" }, true},
		{"downstream path with query", func(f *Fixture) { f.Request.Path += "?token=synthetic-secret" }, true},
		{"downstream status zero", func(f *Fixture) { f.Response.Status = 0 }, true},
		{"downstream non upgrade", func(f *Fixture) { f.Response.Status = 200 }, true},
		{"upstream missing method", func(f *Fixture) { f.Response.WS.Upstream.Method = "" }, true},
		{"upstream post", func(f *Fixture) { f.Response.WS.Upstream.Method = "POST" }, true},
		{"upstream missing path", func(f *Fixture) { f.Response.WS.Upstream.Path = "" }, true},
		{"upstream path with query", func(f *Fixture) { f.Response.WS.Upstream.Path += "?token=synthetic-secret" }, true},
		{"upstream status zero", func(f *Fixture) { f.Response.WS.UpstreamExpectedStatus = 0 }, true},
		{"upstream non upgrade", func(f *Fixture) { f.Response.WS.UpstreamExpectedStatus = 200 }, true},
		{"handshake failed", func(f *Fixture) {
			f.Response.Status = 401
			f.Response.WS.UpstreamExpectedStatus = 401
			f.Response.WS.UpstreamError = json.RawMessage(`{"error":{"code":"synthetic_error"}}`)
			f.Response.WS.Nodes = nil
			f.Response.WS.Outcome = WSOutcome{Kind: "handshake_failed"}
		}, false},
		{"handshake failed downstream upgrade", func(f *Fixture) {
			f.Response.WS.UpstreamExpectedStatus = 401
			f.Response.WS.UpstreamError = json.RawMessage(`{"error":{"code":"synthetic_error"}}`)
			f.Response.WS.Nodes = nil
			f.Response.WS.Outcome = WSOutcome{Kind: "handshake_failed"}
		}, true},
		{"handshake failed upstream upgrade", func(f *Fixture) {
			f.Response.Status = 401
			f.Response.WS.UpstreamError = json.RawMessage(`{"error":{"code":"synthetic_error"}}`)
			f.Response.WS.Nodes = nil
			f.Response.WS.Outcome = WSOutcome{Kind: "handshake_failed"}
		}, true},
		{"downstream raw secret", func(f *Fixture) { f.Request.Headers = map[string]string{"Authorization": "Bearer synthetic-secret"} }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := syntheticEnvelope()
			tt.mutate(&f)
			err := f.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate 错误 = %v，预期拒绝 = %v", err, tt.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), "synthetic-secret") {
				t.Fatal("校验错误泄露了请求内容")
			}
		})
	}
}

// 直接消费磁盘字段名，防止新增 schema 只能在 Go 字面量中使用、加载后静默丢字段。
func TestWSFixtureJSONSchema(t *testing.T) {
	const raw = `{
		"name":"schema", "note":"合成 schema 机制测试",
		"request":{"method":"GET","path":"/v1/realtime"},
		"response":{"status":101,"ws":{
			"version":1,"client_protocol":"openai.realtime","client_version":"synthetic-v1",
			"upstream":{"method":"GET","path":"/v1/realtime"},
			"upstream_expected_status":101,"upstream_error":{"error":false},
			"provenance":{"kind":"synthetic","recorded_at":"2026-10-01T00:00:00Z",
				"upstream_protocol":"openai.realtime","upstream_version":"synthetic-v1","model":"synthetic-model",
				"source_sha256":"source-digest","sample_sha256":{"sample":"sample-digest"}},
			"samples":{"sample":{"data":"AAEC","sha256":"sample-digest"}},
			"coverage":[{"capability":"text_generation","nodes":["receive"],"note":"合成 schema"}],
			"nodes":[{"id":"receive","point":"client.receive","source":"synthetic","after":["send"],
				"kind":"message","message":{"opcode":2,"payload":"AAEC"},"match":"json",
				"fields":[{"pointer":"/id","mode":"ref","namespace":"response","symbol":"r","value":false}],
				"forwarded_from":"send"},
				{"id":"close","point":"client.send","source":"synthetic","kind":"close","close_code":1000,"close_reason":"done"}],
			"outcome":{"kind":"completed","terminal":[{"node":"receive","namespace":"response","symbol":"r",
				"id_pointer":"/id","state_pointer":"/state","state":"completed"}]}
		}}
	}`
	var f Fixture
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		t.Fatal(err)
	}
	if f.Response.WS == nil {
		t.Fatal("response.ws 加载后丢失")
	}
	encoded, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	var want, got any
	if err := json.Unmarshal([]byte(raw), &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("schema 往返后字段或 base64 负载发生变化")
	}
	if !reflect.DeepEqual(f.Response.WS.Samples["sample"].Data, []byte{0, 1, 2}) ||
		!reflect.DeepEqual(f.Response.WS.Nodes[0].Message.Payload, []byte{0, 1, 2}) {
		t.Fatal("sample 与 message 必须使用标准 base64 字节承载")
	}
	if f.Response.WS.Nodes[1].CloseCode == nil || *f.Response.WS.Nodes[1].CloseCode != 1000 {
		t.Fatal("关闭码加载后丢失")
	}
	plain, err := json.Marshal(Response{Status: http.StatusOK})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), `"ws"`) {
		t.Fatal("普通 HTTP 响应不应引入 ws 字段")
	}
}
