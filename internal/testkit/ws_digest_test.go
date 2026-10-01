package testkit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
)

// 独立手写上游契约预期，不能用被测摘要函数填充 recorded 的输入。
const recordedContractJSON = `{"nodes":[{"after":["c-send"],"forwarded_from":"c-send","id":"u-receive","kind":"message","match":"json","message":{"opcode":1,"payload":"eyJ0eXBlIjoicmVzcG9uc2UuY3JlYXRlIn0="},"point":"upstream.receive","source":"upstream-accepted"},{"after":["u-receive"],"fields":[{"mode":"bind","namespace":"response","pointer":"/response/id","symbol":"r-1"}],"id":"u-send","kind":"message","match":"json","message":{"opcode":1,"payload":"eyJ0eXBlIjoicmVzcG9uc2UuZG9uZSIsInJlc3BvbnNlIjp7ImlkIjoici0xIiwic3RhdHVzIjoiY29tcGxldGVkIn19"},"point":"upstream.send","source":"recorded"},{"after":["c-close"],"close_code":1000,"id":"u-close-receive","kind":"close","point":"upstream.receive","source":"upstream-accepted"},{"after":["u-close-receive"],"close_code":1000,"id":"u-close","kind":"close","point":"upstream.send","source":"recorded"}],"outcome":{"kind":"completed","terminal":[{"id_pointer":"/response/id","namespace":"response","node":"c-receive","state":"completed","state_pointer":"/response/status","symbol":"r-1"}]},"upstream":{"method":"GET","path":"/v1/realtime","query":"model=synthetic-model"},"upstream_expected_status":101}`

func independentlyRecordedFixture() Fixture {
	f := syntheticEnvelope()
	s := f.Response.WS
	s.Provenance.Kind = "recorded"
	s.Provenance.RecordedAt = "2026-10-01T00:00:00Z"
	sources := map[WSPoint]string{WSClientSend: "authored", WSUpstreamReceive: "upstream-accepted", WSUpstreamSend: "recorded", WSClientReceive: "golden"}
	for i := range s.Nodes {
		s.Nodes[i].Source = sources[s.Nodes[i].Point]
	}
	digest := sha256.Sum256([]byte(recordedContractJSON))
	s.Provenance.SourceSHA256 = hex.EncodeToString(digest[:])
	return f
}

// 摘要边界固定在上游契约段；客户端 golden 改动不能改写来源账本。
func TestWSContractDigest(t *testing.T) {
	f := independentlyRecordedFixture()
	if err := f.Validate(); err != nil {
		t.Fatalf("独立 recorded 账本不合法: %v", err)
	}
	want := f.Response.WS.Provenance.SourceSHA256
	got, err := WSContractDigest(*f.Response.WS)
	if err != nil || got != want {
		t.Fatalf("摘要与手写契约不符: %v, got=%s want=%s", err, got, want)
	}
	for _, tt := range []struct {
		name    string
		mutate  func(*WSSession)
		changed bool
	}{
		{"source digest excluded", func(s *WSSession) { s.Provenance.SourceSHA256 = "irrelevant" }, false},
		{"client payload excluded", func(s *WSSession) { s.Nodes[0].Message.Payload = []byte(`{"different":true}`) }, false},
		{"golden payload excluded", func(s *WSSession) { s.Nodes[3].Message.Payload = []byte(`{"different":true}`) }, false},
		{"coverage excluded", func(s *WSSession) { s.Coverage[0].Note = "另一个说明" }, false},
		{"upstream payload", func(s *WSSession) { s.Nodes[1].Message.Payload = []byte(`{"different":true}`) }, true},
		{"upstream after", func(s *WSSession) { s.Nodes[2].After = []string{"c-send"} }, true},
		{"upstream query", func(s *WSSession) { s.Upstream.Query = "model=other" }, true},
		{"upstream status", func(s *WSSession) { s.UpstreamExpectedStatus = 403 }, true},
		{"terminal", func(s *WSSession) { s.Outcome.Terminal[0].State = "failed" }, true},
		{"samples", func(s *WSSession) { s.Provenance.SampleSHA256 = map[string]string{"a": "changed"} }, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := independentlyRecordedFixture()
			tt.mutate(f.Response.WS)
			digest, err := WSContractDigest(*f.Response.WS)
			if err != nil || (digest != want) != tt.changed {
				t.Fatalf("摘要边界错误: %v", err)
			}
		})
	}
	first := WSSession{UpstreamError: json.RawMessage(`{"error":{"code":0,"enabled":false}}`)}
	second := WSSession{UpstreamError: json.RawMessage(`{ "error": {"enabled":false, "code":0} }`)}
	a, err := WSContractDigest(first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := WSContractDigest(second)
	if err != nil || a != b {
		t.Fatal("字面对象顺序或空白改变了稳定摘要")
	}
	first.UpstreamError = json.RawMessage(`{"error":0,"error":false}`)
	if _, err := WSContractDigest(first); err == nil {
		t.Fatal("摘要规范化吞掉重复键")
	}
}

// recorded 是可伪造的自洽账本，本测试只证明拒绝缺失/篡改信息，绝不证明真实支持。
func TestWSProvenanceRecordedLedger(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*WSSession)
	}{
		{"timestamp", func(s *WSSession) { s.Provenance.RecordedAt = "" }},
		{"version", func(s *WSSession) { s.Provenance.UpstreamVersion = "" }},
		{"protocol", func(s *WSSession) { s.Provenance.UpstreamProtocol = "" }},
		{"model", func(s *WSSession) { s.Provenance.Model = "" }},
		{"digest", func(s *WSSession) { s.Provenance.SourceSHA256 = "" }},
		{"source", func(s *WSSession) { s.Nodes[1].Source = "authored" }},
		{"tampered", func(s *WSSession) { s.Nodes[1].Message.Payload = []byte(`{"type":"different"}`) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := independentlyRecordedFixture()
			tt.mutate(f.Response.WS)
			if err := f.Validate(); err == nil {
				t.Fatal("无效 recorded 账本被接纳")
			}
		})
	}
}
