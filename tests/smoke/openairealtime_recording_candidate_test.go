package smoke_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/yobo2u/omugw/internal/testkit"
)

// 从原始直连轨迹重新执行只读脚本核验，不信任 confirmed/coverage 等可伪造摘要。
// upstream-accepted 是关联后继 ack/终态形成的组级预期，并非云端 socket 抓包。
func oaCandidate(r oaRecording) (testkit.Fixture, error) {
	var empty testkit.Fixture
	if r.Failure != "" || r.Status != 101 || r.Model != oaModel || r.Started.IsZero() {
		return empty, errors.New("失败或缺少握手，仅保存原始轨迹")
	}
	var checked oaRecording
	for _, rec := range r.Records {
		if rec.Direction != "send" && rec.Direction != "receive" || rec.Kind != "message" && rec.Kind != "close" {
			return empty, errors.New("非法轨迹")
		}
		if err := checked.add(rec, ""); err != nil {
			return empty, err
		}
	}
	i := 0
	d := oaNewDriver(r.Scenario, r.Input)
	d.send = func(b []byte) error {
		if i >= len(r.Records) {
			return errors.New("missing_send")
		}
		rec := r.Records[i]
		if rec.Direction != "send" || rec.Kind != "message" || !bytes.Equal(rec.Payload, b) {
			return errors.New("authored_request_mismatch")
		}
		i++
		return nil
	}
	d.recv = func() (map[string]any, error) {
		if i >= len(r.Records) {
			return nil, errors.New("missing_receive")
		}
		rec := r.Records[i]
		if rec.Direction != "receive" || rec.Kind != "message" {
			return nil, errors.New("missing_ack")
		}
		i++
		var e map[string]any
		_ = json.Unmarshal(rec.Payload, &e)
		return e, nil
	}
	if err := d.run(); err != nil {
		return empty, errors.New("缺少配置/实体/业务证据，仅保存原始轨迹")
	}
	closeIndex := i
	if len(r.Records)-i == 2 {
		a, b := r.Records[i], r.Records[i+1]
		if a.Kind != "close" || b.Kind != "close" || a.Direction != "send" || b.Direction != "receive" || a.Code != 1000 || b.Code != 1000 || a.Reason != b.Reason {
			return empty, errors.New("关闭握手证据不符")
		}
	} else if len(r.Records)-i == 1 {
		a := r.Records[i]
		if a.Kind != "close" || a.Direction != "receive" || a.Code != 1000 {
			return empty, errors.New("缺少对侧关闭证据")
		}
	} else {
		return empty, errors.New("缺少关闭或有未核验尾消息")
	}
	req := testkit.Request{Method: http.MethodGet, Path: "/v1/realtime", Query: url.Values{"model": {r.Model}}.Encode()}
	kind := "recorded"
	if r.Synthetic {
		kind = "synthetic-negative"
	}
	s := &testkit.WSSession{Version: 1, ClientProtocol: "openai.realtime", ClientVersion: "GA-2026-10-04", Upstream: req, UpstreamExpectedStatus: 101, Provenance: testkit.WSProvenance{Kind: kind, RecordedAt: r.Started.Format(time.RFC3339), UpstreamProtocol: "openai.realtime", UpstreamVersion: "GA-2026-10-04; observed model in session.created", Model: r.Model, SampleSHA256: map[string]string{}}, Samples: map[string]testkit.WSSample{}, Outcome: testkit.WSOutcome{Kind: "completed"}}
	f := testkit.Fixture{Name: "openairealtime-" + r.Scenario, Note: "独立直连候选，待人工审核。作者请求及后继协议证据建立 upstream-accepted；非云端抓包。严格保留本次字节与实体，线性序列不宣称所有并发调度。来源/SHA 不认证真实性，非生产投放。", Request: req, Response: testkit.Response{Status: 101, Headers: r.Headers, WS: s}}
	addSample := func(name string, b []byte) {
		h := oaDigest(b)
		s.Samples[name] = testkit.WSSample{Data: b, SHA256: h}
		s.Provenance.SampleSHA256[name] = h
	}
	if r.Input != nil {
		addSample("input.pcm_s16le.24000.mono", r.Input.Data)
	}
	if r.Scenario == "text-tools-vision" {
		addSample("image.generated.png", oaImage())
	}
	last := ""
	var received []string
	for n, rec := range r.Records[:closeIndex+1] {
		points := []testkit.WSPoint{testkit.WSClientSend, testkit.WSUpstreamReceive}
		sources := []string{"authored", "upstream-accepted"}
		if rec.Direction == "receive" {
			points = []testkit.WSPoint{testkit.WSUpstreamSend, testkit.WSClientReceive}
			sources = []string{"recorded", "golden"}
		}
		from := ""
		for j, point := range points {
			node := testkit.WSNode{ID: fmt.Sprintf("oa_%03d_%d", n, j), Point: point, Source: sources[j], Kind: rec.Kind}
			if r.Synthetic {
				node.Source = "synthetic"
			}
			if last != "" {
				node.After = []string{last}
			}
			if rec.Kind == "message" {
				node.Message = &testkit.WSMessage{Opcode: rec.Opcode, Payload: bytes.Clone(rec.Payload)}
				node.Match = "bytes"
				if j == 1 {
					node.ForwardedFrom = from
				}
				if point == testkit.WSClientReceive {
					received = append(received, node.ID)
					var e map[string]any
					_ = json.Unmarshal(rec.Payload, &e)
					if oaString(e, "type") == "response.done" {
						s.Outcome.Terminal = append(s.Outcome.Terminal, testkit.WSTerminal{Node: node.ID, Namespace: "response", Symbol: oaString(e, "response", "id"), IDPointer: "/response/id", StatePointer: "/response/status", State: oaString(e, "response", "status")})
					}
				}
			} else {
				code := rec.Code
				node.CloseCode = &code
				node.CloseReason = rec.Reason
			}
			s.Nodes = append(s.Nodes, node)
			last = node.ID
			if j == 0 {
				from = node.ID
			}
		}
	}
	if r.Scenario == "vad-interrupt" {
		s.Outcome.Kind = "interrupted"
		// schema v1 的 interrupted 不挂终态摘要；cancelled 仍在消息字面断言中。
		s.Outcome.Terminal = nil
	}
	var caps []string
	for cap, ok := range d.coverage {
		if ok {
			caps = append(caps, cap)
		}
	}
	sort.Strings(caps)
	for _, cap := range caps {
		s.Coverage = append(s.Coverage, testkit.WSCoverage{Capability: cap, Nodes: append([]string(nil), received...), Note: "脚本核验的关联序列；来源见 provenance，合成通过不证明真实上游；内容核验边界见 S2 证据表。"})
	}
	digest, err := testkit.WSContractDigest(*s)
	if err != nil {
		return empty, errors.New("候选摘要失败")
	}
	s.Provenance.SourceSHA256 = digest
	if err := testkit.ValidateWSSession(f, testkit.DefaultWSLimits()); err != nil {
		return empty, fmt.Errorf("候选未通过 testkit: %w", err)
	}
	return f, nil
}
