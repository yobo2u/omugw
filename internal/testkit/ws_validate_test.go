package testkit

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/transport/ws"
)

// 防止错误轨迹靠合法握手信封被接纳，终态预期只用手写消息。
func TestWSTraceValidation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Fixture)
	}{
		{"schema", func(f *Fixture) { f.Response.WS.Version = 2 }},
		{"note", func(f *Fixture) { f.Note = " " }},
		{"client protocol", func(f *Fixture) { f.Response.WS.ClientProtocol = "" }},
		{"client version", func(f *Fixture) { f.Response.WS.ClientVersion = "" }},
		{"duplicate id", func(f *Fixture) { f.Response.WS.Nodes[1].ID = "c-send" }},
		{"empty id", func(f *Fixture) { f.Response.WS.Nodes[1].ID = "" }},
		{"point", func(f *Fixture) { f.Response.WS.Nodes[1].Point = "elsewhere" }},
		{"source", func(f *Fixture) { f.Response.WS.Nodes[1].Source = "" }},
		{"dangling after", func(f *Fixture) { f.Response.WS.Nodes[1].After = []string{"missing"} }},
		{"cycle", func(f *Fixture) { f.Response.WS.Nodes[0].After = []string{"u-receive"} }},
		{"fifo contradiction", func(f *Fixture) { f.Response.WS.Nodes[0].After = []string{"c-close"} }},
		{"double payload", func(f *Fixture) { c := uint16(1000); f.Response.WS.Nodes[0].CloseCode = &c }},
		{"missing message", func(f *Fixture) { f.Response.WS.Nodes[0].Message = nil }},
		{"empty text", func(f *Fixture) { f.Response.WS.Nodes[0].Message.Payload = nil }},
		{"invalid utf8", func(f *Fixture) { f.Response.WS.Nodes[0].Message.Payload = []byte{255} }},
		{"invalid json", func(f *Fixture) { f.Response.WS.Nodes[0].Message.Payload = []byte(`{`) }},
		{"duplicate payload key", func(f *Fixture) { f.Response.WS.Nodes[0].Message.Payload = []byte(`{"x":0,"x":false}`) }},
		{"control opcode", func(f *Fixture) { f.Response.WS.Nodes[0].Message.Opcode = ws.OpPing }},
		{"fragment opcode", func(f *Fixture) { f.Response.WS.Nodes[0].Message.Opcode = ws.OpContinuation }},
		{"unknown kind", func(f *Fixture) { f.Response.WS.Nodes[0].Kind = "error" }},
		{"unknown match", func(f *Fixture) { f.Response.WS.Nodes[0].Match = "audio" }},
		{"bytes with rules", func(f *Fixture) { f.Response.WS.Nodes[2].Match = "bytes" }},
		{"close missing code", func(f *Fixture) { f.Response.WS.Nodes[4].CloseCode = nil }},
		{"close message", func(f *Fixture) { f.Response.WS.Nodes[4].Message = &WSMessage{} }},
		{"close fields", func(f *Fixture) { f.Response.WS.Nodes[4].Fields = []WSFieldRule{{Pointer: "/x", Mode: "bind"}} }},
		{"close reserved code", func(f *Fixture) { c := uint16(1006); f.Response.WS.Nodes[4].CloseCode = &c }},
		{"close invalid utf8", func(f *Fixture) { f.Response.WS.Nodes[4].CloseReason = string([]byte{255}) }},
		{"close too long", func(f *Fixture) { f.Response.WS.Nodes[4].CloseReason = strings.Repeat("x", 124) }},
		{"missing close", func(f *Fixture) { f.Response.WS.Nodes = f.Response.WS.Nodes[:7] }},
		{"message after close", func(f *Fixture) {
			n := f.Response.WS.Nodes[0]
			n.ID = "late"
			n.After = nil
			f.Response.WS.Nodes = append(f.Response.WS.Nodes, n)
		}},
		{"query in path", func(f *Fixture) { f.Response.WS.Upstream.Path += "?token=do-not-log" }},
		{"coverage capability", func(f *Fixture) { f.Response.WS.Coverage[0].Capability = "unknown" }},
		{"coverage reference", func(f *Fixture) { f.Response.WS.Coverage[0].Nodes = []string{"missing"} }},
		{"coverage empty", func(f *Fixture) { f.Response.WS.Coverage[0].Nodes = nil }},
		{"coverage note", func(f *Fixture) { f.Response.WS.Coverage[0].Note = "" }},
		{"no outcome", func(f *Fixture) { f.Response.WS.Outcome.Kind = "" }},
		{"unknown outcome", func(f *Fixture) { f.Response.WS.Outcome.Kind = "eof" }},
		{"no terminal", func(f *Fixture) { f.Response.WS.Outcome.Terminal = nil }},
		{"terminal dangling", func(f *Fixture) { f.Response.WS.Outcome.Terminal[0].Node = "missing" }},
		{"terminal send", func(f *Fixture) { f.Response.WS.Outcome.Terminal[0].Node = "u-send" }},
		{"terminal inbound request", func(f *Fixture) {
			f.Response.WS.Outcome.Terminal[0].Node = "u-receive"
			f.Response.WS.Nodes[1].Message.Payload = []byte(`{"response":{"id":"r-1","status":"completed"}}`)
		}},
		{"terminal wrong state", func(f *Fixture) { f.Response.WS.Outcome.Terminal[0].State = "failed" }},
		{"terminal missing id", func(f *Fixture) { f.Response.WS.Outcome.Terminal[0].IDPointer = "/response/missing" }},
		{"terminal missing state", func(f *Fixture) { f.Response.WS.Outcome.Terminal[0].StatePointer = "/response/missing" }},
		{"terminal malformed pointer", func(f *Fixture) { f.Response.WS.Outcome.Terminal[0].IDPointer = "/response/~2" }},
		{"terminal wrong symbol", func(f *Fixture) { f.Response.WS.Outcome.Terminal[0].Symbol = "other" }},
		{"forward missing", func(f *Fixture) { f.Response.WS.Nodes[1].ForwardedFrom = "missing" }},
		{"forward wrong direction", func(f *Fixture) { f.Response.WS.Nodes[1].ForwardedFrom = "u-send" }},
		{"field missing pointer", func(f *Fixture) { f.Response.WS.Nodes[2].Fields[0].Pointer = "/missing" }},
		{"field no symbol", func(f *Fixture) { f.Response.WS.Nodes[2].Fields[0].Symbol = "" }},
		{"field unknown mode", func(f *Fixture) { f.Response.WS.Nodes[2].Fields[0].Mode = "normalize" }},
		{"ref without binding", func(f *Fixture) { f.Response.WS.Nodes[2].Fields = nil }},
		{"ref without causal binding", func(f *Fixture) { f.Response.WS.Nodes[3].After = nil; f.Response.WS.Nodes[3].ForwardedFrom = "" }},
		{"failed normal close", func(f *Fixture) { f.Response.WS.Outcome = WSOutcome{Kind: "failed"} }},
		{"interrupted terminal", func(f *Fixture) { f.Response.WS.Outcome.Kind = "interrupted" }},
		{"handshake with nodes", func(f *Fixture) {
			f.Response.Status = 403
			f.Response.WS.UpstreamExpectedStatus = 403
			f.Response.WS.UpstreamError = json.RawMessage(`{"error":false}`)
			f.Response.WS.Outcome = WSOutcome{Kind: "handshake_failed"}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := syntheticEnvelope()
			tt.mutate(&f)
			if err := f.Validate(); err == nil {
				t.Fatal("非法 WS 轨迹被接纳")
			} else if strings.Contains(err.Error(), "do-not-log") {
				t.Fatal("错误泄露 query")
			}
		})
	}
}

// 标签不能让合成消息冒充上游采纳；摘要也只检查账本一致性。
func TestWSProvenanceDoesNotProveLiveSupport(t *testing.T) {
	for _, mutate := range []func(*Fixture){
		func(f *Fixture) { f.Response.WS.Provenance.Kind = "unknown" },
		func(f *Fixture) { f.Response.WS.Nodes[1].Source = "upstream-accepted" },
		func(f *Fixture) { f.Response.WS.Nodes[2].Source = "recorded" },
		func(f *Fixture) { f.Response.WS.Provenance.Kind = "recorded" },
		func(f *Fixture) {
			f.Response.WS.Samples = map[string]WSSample{"pcm": {Data: []byte{0}, SHA256: strings.Repeat("0", 64)}}
		},
		func(f *Fixture) {
			f.Response.WS.Provenance.SampleSHA256 = map[string]string{"missing": strings.Repeat("0", 64)}
		},
		func(f *Fixture) { f.Response.WS.Provenance.SourceSHA256 = strings.Repeat("0", 64) },
	} {
		f := syntheticEnvelope()
		mutate(&f)
		if err := f.Validate(); err == nil {
			t.Fatal("不一致的来源账本被接纳")
		}
	}
}
