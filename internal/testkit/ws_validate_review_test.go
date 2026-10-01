package testkit

import (
	"encoding/json"
	"testing"
)

// 静态 schema 必须与 matcher 的既定词汇一致，不能保留未发布版本的旧别名。
func TestWSStaticSchemaV1(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*Fixture)
		valid  bool
	}{
		{"equal", func(f *Fixture) {
			f.Response.WS.Nodes[0].Fields = []WSFieldRule{{Pointer: "/type", Mode: "equal", Value: json.RawMessage(`"response.create"`)}}
		}, true},
		{"reference", func(f *Fixture) { f.Response.WS.Nodes[3].Fields[0].Mode = "reference" }, true},
		{"default bytes binary", func(f *Fixture) {
			f.Response.WS.Nodes[0].Match = ""
			f.Response.WS.Nodes[0].Message.Opcode = 2
			f.Response.WS.Nodes[0].Message.Payload = []byte{}
		}, true},
		{"default bytes text", func(f *Fixture) {
			f.Response.WS.Nodes[0].Match = ""
			f.Response.WS.Nodes[0].Message.Payload = []byte("opaque")
		}, true},
		{"default bytes terminal", func(f *Fixture) {
			for i := 2; i <= 3; i++ {
				f.Response.WS.Nodes[i].Match = ""
				f.Response.WS.Nodes[i].Fields = nil
			}
		}, true},
		{"default bytes rules", func(f *Fixture) { f.Response.WS.Nodes[2].Match = "" }, false},
		{"legacy literal", func(f *Fixture) {
			f.Response.WS.Nodes[0].Fields = []WSFieldRule{{Pointer: "/type", Mode: "literal", Value: json.RawMessage(`"response.create"`)}}
		}, false},
		{"legacy ref", func(f *Fixture) { f.Response.WS.Nodes[3].Fields[0].Mode = "ref" }, false},
		{"equal terminal", func(f *Fixture) {
			f.Response.WS.Nodes[3].Fields = []WSFieldRule{{Pointer: "/response/id", Mode: "equal", Value: json.RawMessage(`"r-1"`)}, {Pointer: "/response/status", Mode: "equal", Value: json.RawMessage(`"completed"`)}}
		}, true},
		{"reference wrong terminal", func(f *Fixture) {
			f.Response.WS.Nodes[3].Fields[0].Mode = "reference"
			f.Response.WS.Outcome.Terminal[0].Symbol = "other"
		}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := syntheticEnvelope()
			tt.mutate(&f)
			if err := f.Validate(); (err == nil) != tt.valid {
				t.Fatalf("合法=%v 预期=%v: %v", err == nil, tt.valid, err)
			}
		})
	}
}

// 主动 Close 后立即关 TCP，只能用主动发送与对侧接收举证，不能补造主动端的自动回应。
func TestWSObservableClosePairs(t *testing.T) {
	for _, direction := range []struct {
		name          string
		send, receive WSPoint
	}{
		{"client", WSClientSend, WSUpstreamReceive},
		{"upstream", WSUpstreamSend, WSClientReceive},
	} {
		for _, outcome := range []string{"completed", "failed", "interrupted"} {
			for _, variant := range []string{"paired", "mismatched code", "same endpoint", "missing receive", "missing send", "no causal edge"} {
				t.Run(outcome+"/"+direction.name+"/"+variant, func(t *testing.T) {
					f := syntheticEnvelope()
					s := f.Response.WS
					s.Nodes = s.Nodes[:4]
					for i := 2; i <= 3; i++ {
						s.Nodes[i].Fields = nil
					}
					s.Outcome.Kind = outcome
					if outcome != "completed" {
						s.Outcome.Terminal = nil
					}
					code := uint16(1000)
					if outcome != "completed" {
						code = 1011
					}
					receivedCode := code
					send := WSNode{ID: "send-close", Point: direction.send, Source: "synthetic", Kind: "close", CloseCode: &code, After: []string{"c-receive"}}
					receive := WSNode{ID: "receive-close", Point: direction.receive, Source: "synthetic", Kind: "close", CloseCode: &receivedCode, After: []string{"send-close"}}
					switch variant {
					case "mismatched code":
						receivedCode = 1001
					case "same endpoint":
						if direction.send == WSClientSend {
							receive.Point = WSClientReceive
						} else {
							receive.Point = WSUpstreamReceive
						}
					case "no causal edge":
						receive.After = nil
					}
					if variant != "missing send" {
						s.Nodes = append(s.Nodes, send)
					} else {
						receive.After = []string{"c-receive"}
					}
					if variant != "missing receive" {
						s.Nodes = append(s.Nodes, receive)
					}
					// interrupted 保持本轮既有“至少有效 close”纪律；配对约束属于 completed/failed。
					valid := variant == "paired" || outcome == "interrupted"
					if err := f.Validate(); (err == nil) != valid {
						t.Fatalf("合法=%v 预期=%v: %v", err == nil, valid, err)
					}
				})
			}
		}
	}
	t.Run("completed non1000", func(t *testing.T) {
		f := syntheticEnvelope()
		code := uint16(1001)
		for i := 4; i < len(f.Response.WS.Nodes); i++ {
			f.Response.WS.Nodes[i].CloseCode = &code
		}
		if err := f.Validate(); err == nil {
			t.Fatal("completed 接纳非1000 close")
		}
	})
}
