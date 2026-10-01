package testkit

import (
	"encoding/json"
	"testing"

	"github.com/yobo2u/omugw/internal/transport/ws"
)

// 非法 null、字面类型和关闭配对不能被“有接收节点”掩盖。
func TestWSTraceValidationBoundaries(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*Fixture)
		valid  bool
	}{
		{"synthetic-negative", func(f *Fixture) { f.Response.WS.Provenance.Kind = "synthetic-negative" }, true},
		{"opaque text", func(f *Fixture) {
			f.Response.WS.Nodes[0].Match = "bytes"
			f.Response.WS.Nodes[0].Message.Payload = []byte("opaque")
		}, true},
		{"zero binary", func(f *Fixture) {
			f.Response.WS.Nodes[0].Match = "bytes"
			f.Response.WS.Nodes[0].Message = &WSMessage{Opcode: ws.OpBinary, Payload: []byte{}}
		}, true},
		{"null binary", func(f *Fixture) {
			f.Response.WS.Nodes[0].Match = "bytes"
			f.Response.WS.Nodes[0].Message = &WSMessage{Opcode: ws.OpBinary}
		}, false},
		{"literal terminal", func(f *Fixture) { f.Response.WS.Nodes[2].Fields = nil; f.Response.WS.Nodes[3].Fields = nil }, true},
		{"bound state terminal", func(f *Fixture) {
			f.Response.WS.Nodes[3].Fields = append(f.Response.WS.Nodes[3].Fields, WSFieldRule{Pointer: "/response/status", Mode: "bind", Namespace: "state", Symbol: "completed"})
		}, true},
		{"completed non1000 close", func(f *Fixture) {
			c := uint16(1001)
			for i := 4; i < len(f.Response.WS.Nodes); i++ {
				f.Response.WS.Nodes[i].CloseCode = &c
			}
		}, false},
		{"failed close", func(f *Fixture) {
			c := uint16(1011)
			for i := 4; i < len(f.Response.WS.Nodes); i++ {
				f.Response.WS.Nodes[i].CloseCode = &c
			}
			f.Response.WS.Outcome = WSOutcome{Kind: "failed"}
		}, true},
		{"failed unpaired close", func(f *Fixture) {
			c := uint16(1011)
			f.Response.WS.Nodes[4].CloseCode = &c
			f.Response.WS.Outcome = WSOutcome{Kind: "failed"}
		}, false},
		{"interrupted", func(f *Fixture) { f.Response.WS.Outcome = WSOutcome{Kind: "interrupted"} }, true},
		{"interrupted one close", func(f *Fixture) {
			f.Response.WS.Nodes = f.Response.WS.Nodes[:5]
			f.Response.WS.Outcome = WSOutcome{Kind: "interrupted"}
		}, true},
		{"interrupted no close", func(f *Fixture) {
			f.Response.WS.Nodes = f.Response.WS.Nodes[:4]
			f.Response.WS.Outcome = WSOutcome{Kind: "interrupted"}
		}, false},
		{"failed same endpoint pair", func(f *Fixture) {
			c := uint16(1011)
			for i := 4; i < len(f.Response.WS.Nodes); i++ {
				f.Response.WS.Nodes[i].CloseCode = &c
			}
			f.Response.WS.Nodes[5].Point = WSClientReceive
			f.Response.WS.Outcome = WSOutcome{Kind: "failed"}
		}, false},
		{"failed no close", func(f *Fixture) {
			f.Response.WS.Nodes = f.Response.WS.Nodes[:4]
			f.Response.WS.Outcome = WSOutcome{Kind: "failed"}
		}, false},
		{"null bound id", func(f *Fixture) {
			f.Response.WS.Nodes[2].Message.Payload = []byte(`{"response":{"id":null,"status":"completed"}}`)
		}, false},
		{"object bound id", func(f *Fixture) {
			f.Response.WS.Nodes[2].Message.Payload = []byte(`{"response":{"id":{},"status":"completed"}}`)
		}, false},
		{"terminal state object", func(f *Fixture) {
			f.Response.WS.Nodes[3].Message.Payload = []byte(`{"response":{"id":"r-1","status":{}}}`)
		}, false},
		{"literal state object", func(f *Fixture) {
			f.Response.WS.Nodes[3].Fields = append(f.Response.WS.Nodes[3].Fields, WSFieldRule{Pointer: "/response/status", Mode: "equal", Value: json.RawMessage(`{}`)})
		}, false},
		{"sample empty", func(f *Fixture) {
			f.Response.WS.Samples = map[string]WSSample{"empty": {Data: []byte{}, SHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}}
			f.Response.WS.Provenance.SampleSHA256 = map[string]string{"empty": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}
		}, true},
		{"null sample", func(f *Fixture) {
			f.Response.WS.Samples = map[string]WSSample{"empty": {SHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}}
			f.Response.WS.Provenance.SampleSHA256 = map[string]string{"empty": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}
		}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := syntheticEnvelope()
			tt.mutate(&f)
			if err := f.Validate(); (err == nil) != tt.valid {
				t.Fatalf("合法=%v 预期=%v, error=%v", err == nil, tt.valid, err)
			}
		})
	}
}

// 终态若只认 bytes，合法 JSON 的同契约旁路也必须能声明固定终态。
func TestWSTerminalBytesJSON(t *testing.T) {
	f := syntheticEnvelope()
	for i := 2; i <= 3; i++ {
		f.Response.WS.Nodes[i].Fields = nil
		f.Response.WS.Nodes[i].Match = "bytes"
	}
	if err := f.Validate(); err != nil {
		t.Fatalf("固定字节 JSON 终态被拒绝: %v", err)
	}
}

// 预算上界必须可达，绑定和样本不能另走不计费的旁路。
func TestWSValidationExactBudgets(t *testing.T) {
	f := syntheticEnvelope()
	s := f.Response.WS
	next := s.Nodes[2]
	next.ID = "extra-bind"
	next.After = []string{"u-send"}
	next.ForwardedFrom = ""
	next.Fields = []WSFieldRule{{Pointer: "/response/id", Mode: "bind", Namespace: "other", Symbol: "r-2"}}
	s.Nodes = append(s.Nodes[:4], append([]WSNode{next}, s.Nodes[4:]...)...)
	limits := DefaultWSLimits()
	limits.Bindings = 1
	if err := ValidateWSSession(f, limits); err == nil {
		t.Fatal("绑定预算被绕过")
	}
	limits.Bindings = 2
	if err := ValidateWSSession(f, limits); err != nil {
		t.Fatal(err)
	}
	f = syntheticEnvelope()
	limits = DefaultWSLimits()
	limits.Nodes = 6
	limits.Edges = 5
	limits.FieldRules = 2
	limits.Bindings = 1
	var total int64
	var largest int64
	for _, n := range f.Response.WS.Nodes {
		if n.Message != nil {
			size := int64(len(n.Message.Payload))
			total += size
			if size > largest {
				largest = size
			}
		} else {
			total += 2 + int64(len(n.CloseReason))
		}
	}
	limits.MessageBytes = largest
	limits.TraceBytes = total
	if err := ValidateWSSession(f, limits); err != nil {
		t.Fatal(err)
	}
	f.Response.WS.Samples = map[string]WSSample{"zero": {Data: []byte{0}, SHA256: "6e340b9cffb37a989ca544e6bb780a2c78901d3fb33738768511a30617afa01d"}}
	f.Response.WS.Provenance.SampleSHA256 = map[string]string{"zero": "6e340b9cffb37a989ca544e6bb780a2c78901d3fb33738768511a30617afa01d"}
	if err := ValidateWSSession(f, limits); err == nil {
		t.Fatal("样本未计入轨迹预算")
	}
	limits.TraceBytes++
	if err := ValidateWSSession(f, limits); err != nil {
		t.Fatal(err)
	}
}
