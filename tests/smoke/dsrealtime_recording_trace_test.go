//go:build smoke

package smoke_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yobo2u/omugw/internal/testkit"
)

// 只从独立编写的发送与直连接收构造同契约预期；不导入 gateway/provider。
// upstream-accepted 是后继协议证据约束下的请求预期，不声称抓到了云端 socket。
func dsCandidate(r dsRecording) (testkit.Fixture, error) {
	var empty testkit.Fixture
	if r.Failure != "" || !r.Confirmed || len(r.Records) == 0 {
		return empty, errors.New("失败或配置未确认，仅保留原始轨迹")
	}
	s := &testkit.WSSession{Version: 1, ClientProtocol: "dashscope.realtime", ClientVersion: "official-raw-2026-10-04", Upstream: testkit.Request{Method: http.MethodGet, Path: "/api-ws/v1/realtime", Query: url.Values{"model": {r.Model}}.Encode()}, UpstreamExpectedStatus: 101,
		Provenance: testkit.WSProvenance{Kind: "recorded", RecordedAt: r.Started.Format(time.RFC3339), UpstreamProtocol: "dashscope.realtime", UpstreamVersion: "official-raw-2026-10-04; requested-alias, see session.created for observed model", Model: r.Model, SampleSHA256: map[string]string{}}, Samples: map[string]testkit.WSSample{}, Outcome: testkit.WSOutcome{Kind: "interrupted"}}
	f := testkit.Fixture{Name: "dsrealtime-" + r.Scenario, Note: "独立直连候选，待人工审核。请求为作者脚本；upstream-accepted 由后继事件提供组级证据，不是云端抓包。仅保留本次实际序列，不承诺并发调度。模型别名以 session.created 回显为准。", Request: s.Upstream, Response: testkit.Response{Status: 101, Headers: r.Headers, WS: s}}
	addSample := func(name string, b []byte) {
		if len(b) > 0 {
			h := dsDigest(b)
			s.Samples[name] = testkit.WSSample{Data: b, SHA256: h}
			s.Provenance.SampleSHA256[name] = h
		}
	}
	if r.Input != nil {
		addSample("input.pcm_s16le.16000.mono", r.Input.Data)
	}
	addSample("image.generated.jpeg", r.Image)
	addSample("output.pcm_s16le.16000.mono", r.Audio)
	bindings := map[string]string{}
	var last string
	var terminal []testkit.WSTerminal
	var observedClose bool
	var events []dsEvidenceEvent
	// RFC close 的回应只在原始轨迹保留；回放使用主动 close→接收 close 一对，
	// 避免让 transport 的自动回应与第二个显式 close 争抢同一个已关闭连接。
	closeReply := -1
	for i, rec := range r.Records {
		if rec.Kind != "close" || rec.Direction != "send" {
			continue
		}
		if i+1 >= len(r.Records) {
			return empty, errors.New("缺少真实 close 回应，仅保留原始轨迹")
		}
		reply := r.Records[i+1]
		if reply.Kind != "close" || reply.Direction != "receive" || rec.CloseCode != 1000 || reply.CloseCode != rec.CloseCode {
			return empty, errors.New("close 回应缺失、错码或被额外消息隔断")
		}
		closeReply = i + 1
	}
	for i, rec := range r.Records {
		if i == closeReply {
			continue
		}
		var payload map[string]any
		if rec.Kind == "message" {
			if json.Unmarshal(rec.Payload, &payload) != nil {
				return empty, errors.New("轨迹消息不是 JSON")
			}
		}
		if rec.Direction == "send" && rec.Kind == "message" && !dsRequestWitness(r.Scenario, r.Records, i, payload) {
			return empty, errors.New("缺少请求的后继协议证据")
		}
		points := []testkit.WSPoint{testkit.WSClientSend, testkit.WSUpstreamReceive}
		sources := []string{"authored", "upstream-accepted"}
		if rec.Direction == "receive" {
			points = []testkit.WSPoint{testkit.WSUpstreamSend, testkit.WSClientReceive}
			sources = []string{"recorded", "golden"}
		}
		from := ""
		for j, point := range points {
			n := testkit.WSNode{ID: fmt.Sprintf("n%04d_%d", i, j), Point: point, Source: sources[j], Kind: rec.Kind}
			if last != "" {
				n.After = []string{last}
			}
			if rec.Kind == "message" {
				n.Message = &testkit.WSMessage{Opcode: rec.Opcode, Payload: append([]byte(nil), rec.Payload...)}
				n.Match = "json"
				n.Fields = dsIDRules(payload, bindings)
				if j == 1 {
					n.ForwardedFrom = from
				}
			} else {
				code := rec.CloseCode
				n.CloseCode = &code
				n.CloseReason = rec.CloseReason
			}
			s.Nodes = append(s.Nodes, n)
			last = n.ID
			if j == 0 {
				from = n.ID
			}
			if point == testkit.WSClientReceive && rec.Kind == "message" {
				events = append(events, dsEvidenceEvent{Node: n.ID, Value: payload})
				if dsString(payload, "type") == "response.done" && dsString(payload, "response", "status") == "completed" {
					id := dsString(payload, "response", "id")
					if id != "" {
						terminal = append(terminal, testkit.WSTerminal{Node: n.ID, Namespace: "response", Symbol: bindings["response\x00"+id], IDPointer: "/response/id", StatePointer: "/response/status", State: "completed"})
					}
				}
			}
		}
		if rec.Kind == "close" {
			observedClose = true
			if rec.CloseCode != 1000 {
				return empty, errors.New("非正常上游 close 仅保留原始轨迹")
			}
		}
	}
	if observedClose && len(terminal) > 0 {
		s.Outcome = testkit.WSOutcome{Kind: "completed", Terminal: terminal}
	}
	s.Coverage = dsCoverage(r, events)
	digest, err := testkit.WSContractDigest(*s)
	if err != nil {
		return empty, errors.New("候选契约无法计算摘要")
	}
	s.Provenance.SourceSHA256 = digest
	if err := testkit.ValidateWSSession(f, testkit.DefaultWSLimits()); err != nil {
		return empty, fmt.Errorf("候选未通过 testkit 验证: %w", err)
	}
	return f, nil
}

// 只给存在真实后继见证的发送建立请求预期；发送成功本身不是业务 ack。
func dsRequestWitness(scenario string, records []dsRecord, index int, request map[string]any) bool {
	typ := dsString(request, "type")
	if scenario == "tts-server-commit" && (typ == "input_text_buffer.append" || typ == "session.finish") {
		for _, i := range dsAutomaticCommitEvidence(records) {
			if i == index {
				return true
			}
		}
		return false
	}
	if typ == "conversation.item.create" {
		if dsString(request, "item", "type") == "function_call_output" && !dsToolResultWitness(records, index) {
			return false
		}
		return dsItemRequestWitness(records, index)
	}
	if typ == "response.cancel" {
		nodes, cancelIndex := dsInterruptEvidence(records)
		return len(nodes) > 0 && cancelIndex == index
	}
	for _, rec := range records[index+1:] {
		if rec.Direction != "receive" || rec.Kind != "message" {
			continue
		}
		var e map[string]any
		if json.Unmarshal(rec.Payload, &e) != nil {
			continue
		}
		t := dsString(e, "type")
		switch typ {
		case "session.update":
			if t == "session.updated" && dsSessionEchoMatches(scenario, dsMap(request, "session"), dsMap(e, "session")) {
				return true
			}
		case "response.create":
			if t == "response.created" || t == "response.done" {
				return true
			}
		case "input_audio_buffer.append", "input_image_buffer.append", "input_audio_buffer.commit":
			if t == "input_audio_buffer.committed" {
				return true
			}
		case "input_text_buffer.append":
			if t == "input_text_buffer.committed" || t == "response.done" && dsString(e, "response", "status") == "completed" {
				return true
			}
		case "input_text_buffer.commit":
			if t == "input_text_buffer.committed" {
				return true
			}
		case "session.finish":
			if t == "session.finished" {
				return true
			}
		}
	}
	return false
}

// 复用 testkit 的实体绑定/引用，所有非 ID 字段仍为严格 JSON；负载不做重编码。
func dsIDRules(value any, bindings map[string]string) []testkit.WSFieldRule {
	var rules []testkit.WSFieldRule
	newHere := map[string]bool{}
	var visit func(any, string)
	visit = func(v any, p string) {
		switch x := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				path := p + "/" + strings.ReplaceAll(strings.ReplaceAll(k, "~", "~0"), "/", "~1")
				ns := ""
				switch k {
				case "event_id":
					ns = "event"
				case "response_id":
					ns = "response"
				case "session_id":
					ns = "session"
				// previous_item_id 不在 testkit 的动态 ID 契约内，保留严格字面匹配，
				// 防止生成非法引用规则；原始前驱关系不能被忽略或改写。
				case "item_id":
					ns = "item"
				case "call_id":
					ns = "call"
				case "id":
					if p == "/session" {
						ns = "session"
					} else if p == "/response" {
						ns = "response"
					} else if p == "/item" || strings.Contains(p, "/output/") {
						ns = "item"
					}
				}
				if id, ok := x[k].(string); ok && id != "" && ns != "" {
					key := ns + "\x00" + id
					if newHere[key] {
						continue
					}
					symbol, exists := bindings[key]
					mode := "reference"
					if !exists {
						symbol = fmt.Sprintf("%s_%d", ns, len(bindings)+1)
						bindings[key] = symbol
						mode = "bind"
						newHere[key] = true
					}
					rules = append(rules, testkit.WSFieldRule{Pointer: path, Mode: mode, Namespace: ns, Symbol: symbol})
				} else {
					visit(x[k], path)
				}
			}
		case []any:
			for i, e := range x {
				visit(e, p+"/"+strconv.Itoa(i))
			}
		}
	}
	visit(value, "")
	return rules
}

type dsEvidenceEvent struct {
	Node  string
	Value map[string]any
}

// Coverage 仅列可定位事件的保守证据；图像理解、会话记忆等仍需人工审阅正文语义。
func dsCoverage(r dsRecording, events []dsEvidenceEvent) []testkit.WSCoverage {
	byType := map[string][]string{}
	var done, audio, text, asr, parallel, tools []string
	for _, e := range events {
		t := dsString(e.Value, "type")
		byType[t] = append(byType[t], e.Node)
		switch t {
		case "response.done":
			if dsString(e.Value, "response", "status") == "completed" {
				done = append(done, e.Node)
			}
			if _, ok := dsValidatedToolResults(e.Value); ok {
				tools = append(tools, e.Node)
				parallel = append(parallel, e.Node)
			}
		case "response.audio.delta":
			if dsString(e.Value, "delta") != "" {
				audio = append(audio, e.Node)
			}
		case "response.text.delta":
			if dsString(e.Value, "delta") != "" {
				text = append(text, e.Node)
			}
		case "conversation.item.input_audio_transcription.completed":
			if dsString(e.Value, "transcript") != "" && dsString(e.Value, "item_id") != "" {
				asr = append(asr, e.Node)
			}
		}
	}
	var coverage []testkit.WSCoverage
	add := func(cap, note string, nodes ...[]string) {
		var all []string
		for _, n := range nodes {
			if len(n) == 0 {
				return
			}
			all = append(all, n...)
		}
		coverage = append(coverage, testkit.WSCoverage{Capability: cap, Nodes: all, Note: note})
	}
	add("realtime_session", "配置逐字段回显核对，仍需核对实际模型别名和原始结构", byType["session.created"], byType["session.updated"])
	add("text_generation", "非空文本 delta 与完成终态", text, done)
	if len(done) > 0 {
		if len(audio) > 0 {
			add("streaming", "实际音频分片与业务终态", audio, done)
		} else {
			add("streaming", "实际文本分片与业务终态", text, done)
		}
		add("audio_output", "非空音频分片、明确 PCM 格式采样率与完成终态", audio, done)
	}
	if strings.HasPrefix(r.Scenario, "tts-") {
		add("speech_synthesis", "固定公开短句、Cherry、明确 PCM 格式/采样率与非空音频", audio, done)
		if r.Scenario == "tts-commit" {
			add("realtime_commit_modes", "commit 模式真实 committed、音频和 done；不单独证明另一模式", byType["input_text_buffer.committed"], audio, done)
		} else {
			add("realtime_commit_modes", "server_commit 在finish前自动创建响应并返回同实体非空音频，再取得同ID completed终态、finished及正常close；done可在finish后", dsEvidenceNodes(r.Records, dsAutomaticCommitEvidence(r.Records)))
		}
	}
	if r.Input != nil {
		add("audio_input", "落盘短句 PCM、提交及转写；转写内容仍须人工与固定短句核对", byType["input_audio_buffer.committed"], asr)
		add("speech_recognition", "真实非空 transcription.completed，不拿音频波形充当识别证明", asr)
	}
	if r.Scenario == "text-tools" {
		add("tool_calling", "完整预期工具集合通过形状/名称/唯一call_id校验，结果回传另由请求见证核对", tools, done)
		add("parallel_tool_calls", "同一completed response.done内两个预期名称各一且call_id非空唯一，非两个串行响应", parallel)
	}
	if r.Scenario == "vad-interrupt" {
		add("realtime_server_vad", "实际 speech_started/stopped 与 committed", byType["input_audio_buffer.speech_started"], byType["input_audio_buffer.speech_stopped"], byType["input_audio_buffer.committed"])
		nodes, _ := dsInterruptEvidence(r.Records)
		add("realtime_interrupt_turns", "同一活跃response的非空音频、cancel目标与cancelled终态完整关联，非本地断连", dsEvidenceNodes(r.Records, nodes))
	}
	return coverage
}
