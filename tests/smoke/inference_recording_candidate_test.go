package smoke_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/yobo2u/omugw/internal/testkit"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

func inferenceCandidate(r inferenceRecording) (testkit.Fixture, error) {
	var empty testkit.Fixture
	bad := errors.New("候选缺少原样终态/关闭证据或超过独立预算")
	if r.Started.IsZero() || r.Ended.Before(r.Started) || r.Ended.Sub(r.Started) > inferenceDuration || r.Status != 101 || r.Failure != "" || r.Slot < 1 || r.Slot > 6 {
		return empty, bad
	}
	limits := testkit.DefaultWSLimits()
	trace := int64(len(r.Sample))
	checked := inferenceRecording{}
	for _, rec := range r.Records {
		if rec.SHA256 != inferenceSHA(rec.Payload) || rec.At > r.Ended.Sub(r.Started) || rec.IncompleteMessage {
			return empty, bad
		}
		// 四点候选必须保留完整消息对；不能超限后悄悄只留一半。
		trace += 2 * int64(len(rec.Payload)+len(rec.CloseReason)+2)
		if trace > limits.TraceBytes {
			return empty, bad
		}
		if err := checked.add(rec, ""); err != nil {
			return empty, bad
		}
	}
	if r.SampleSHA256 != inferenceSHA(r.Sample) {
		return empty, bad
	}
	models := [6]string{"qwen-audio-3.0-asr-flash-streaming", "qwen-audio-3.0-tts-flash", "sambert-zhichu-v1", "qwen-audio-3.1-asr-flash-message", "qwen-audio-3.1-asr-flash-streaming", "qwen-audio-3.0-tts-flash"}
	voices := [6]string{"", "longanlingxi", "", "", "", "omugw-invalid-voice-s3"}
	tasks := [6]int64{2, 2, 1, 1, 2, 1}
	slot := r.Manifest.Slots[r.Slot-1]
	if r.Scenario != inferenceScenarioName(r.Slot) || r.Model != models[r.Slot-1] || r.Voice != voices[r.Slot-1] || slot.Model != r.Model || slot.Voice != r.Voice || slot.Slot != r.Slot || slot.MaxTasks != tasks[r.Slot-1] || r.Manifest.SampleSHA256 != r.SampleSHA256 {
		return empty, bad
	}
	c := inferenceRecordingConfig{Slot: r.Slot, Scenario: r.Scenario, Model: r.Model, Voice: r.Voice, SampleSHA256: r.SampleSHA256, Manifest: r.Manifest}
	i := 0
	outcome, err := inferenceDrive(c, r.Sample, func(op ws.Opcode, b []byte) error {
		if i >= len(r.Records) {
			return bad
		}
		rec := r.Records[i]
		if rec.Direction != "send" || rec.Opcode != op || !bytes.Equal(rec.Payload, b) {
			return bad
		}
		i++
		return nil
	}, func() (inferenceRecord, error) {
		if i >= len(r.Records) {
			return inferenceRecord{}, bad
		}
		rec := r.Records[i]
		if rec.Direction != "receive" || rec.Opcode == ws.OpClose {
			return inferenceRecord{}, bad
		}
		i++
		return rec, nil
	})
	if err != nil || outcome != r.Outcome {
		return empty, bad
	}
	peer := false
	local := false
	var failed, lastUsage inferenceEvent
	if outcome == "failed" {
		// drive已验证到这条真实failed；不能把第二task的失败硬绑成第一task。
		failed, err = inferenceDecode(r.Records[i-1].Payload, r.Model)
		if err != nil || failed.Event != "task-failed" {
			return empty, bad
		}
		for _, rec := range r.Records[:i] {
			if rec.Direction == "receive" && rec.Opcode == ws.OpText {
				e, eerr := inferenceDecode(rec.Payload, r.Model)
				if eerr == nil && e.TaskID == failed.TaskID {
					if inferenceAdvanceUsage(&lastUsage, e) != nil {
						return empty, bad
					}
				}
			}
		}
	}
	for _, rec := range r.Records[i:] {
		if rec.Opcode != ws.OpClose {
			// failed后的尾消息保留但不补终态；本槽无效音色没有已启动的binary契约。
			if outcome != "failed" || rec.Direction != "receive" || rec.Opcode != ws.OpText {
				return empty, bad
			}
			e, err := inferenceBoundEvent(c, rec.Payload, failed.TaskID)
			if err != nil || e.Event == "task-finished" || e.Event == "task-started" {
				return empty, bad
			}
			if inferenceAdvanceUsage(&lastUsage, e) != nil {
				return empty, bad
			}
			// 终态已冻结：同值重复可保全；增长、回退、presence改变均不能重结成新证据。
			if (e.Event == "task-failed" || e.Usage) && !inferenceSameUsage(e, failed) {
				return empty, bad
			}
			continue
		}
		if rec.Direction == "receive" && rec.PeerClose && !peer {
			peer = true
			if outcome == "failed" && rec.CloseCode == 1000 || outcome != "failed" && rec.CloseCode != 1000 {
				return empty, bad
			}
		} else if rec.Direction == "send" && !rec.PeerClose && !local {
			local = true
		} else {
			return empty, bad
		}
	}
	if !peer {
		return empty, bad
	}
	kind := "recorded"
	if r.Synthetic {
		kind = "synthetic-negative"
	}
	req := testkit.Request{Method: http.MethodGet, Path: "/api-ws/v1/inference"}
	s := &testkit.WSSession{Version: 1, ClientProtocol: "dashscope.inference", ClientVersion: "audio-2026-10-04", Upstream: req, UpstreamExpectedStatus: 101, Provenance: testkit.WSProvenance{Kind: kind, RecordedAt: r.Started.Format(time.RFC3339), UpstreamProtocol: "dashscope.inference", UpstreamVersion: "audio-2026-10-04", Model: r.Model, SampleSHA256: map[string]string{"input.pcm_s16le.16000.mono": r.SampleSHA256}}, Samples: map[string]testkit.WSSample{"input.pcm_s16le.16000.mono": {Data: r.Sample, SHA256: r.SampleSHA256}}, Outcome: testkit.WSOutcome{Kind: outcome}}
	f := testkit.Fixture{Name: "inference-" + r.Scenario, Note: "独立直连待审核候选；authored是本地完整写出，upstream-accepted为后继task事件的契约预期而非云端抓包；local close仅client.send，peer close单独recorded/golden。时间与原始SHA见私有records.jsonl。严格字节和线性观测序列不代表所有调度，不兑现能力。", Request: req, Response: testkit.Response{Status: 101, WS: s}}
	last := ""
	for n, rec := range r.Records {
		points := []testkit.WSPoint{testkit.WSClientSend, testkit.WSUpstreamReceive}
		sources := []string{"authored", "upstream-accepted"}
		if rec.Direction == "receive" {
			points = []testkit.WSPoint{testkit.WSUpstreamSend, testkit.WSClientReceive}
			sources = []string{"recorded", "golden"}
		} else if rec.Opcode == ws.OpClose {
			points = points[:1]
			sources = sources[:1]
		}
		from := ""
		for j, point := range points {
			node := testkit.WSNode{ID: fmt.Sprintf("s3_%03d_%d", n, j), Point: point, Source: sources[j], Kind: "message"}
			if r.Synthetic {
				node.Source = "synthetic"
			}
			if last != "" {
				node.After = []string{last}
			}
			if rec.Opcode == ws.OpClose {
				code := rec.CloseCode
				node.Kind = "close"
				node.CloseCode = &code
				node.CloseReason = rec.CloseReason
			} else {
				node.Message = &testkit.WSMessage{Opcode: rec.Opcode, Payload: rec.Payload}
				node.Match = "bytes"
				if j == 1 {
					node.ForwardedFrom = from
				}
			}
			s.Nodes = append(s.Nodes, node)
			from = node.ID
			last = node.ID
			if point == testkit.WSClientReceive && rec.Opcode == ws.OpText && outcome != "interrupted" {
				e, err := inferenceDecode(rec.Payload, r.Model)
				if err == nil && (e.Event == "task-finished" || e.Event == "task-failed") {
					s.Outcome.Terminal = append(s.Outcome.Terminal, testkit.WSTerminal{Node: node.ID, Namespace: "task", Symbol: e.TaskID, IDPointer: "/header/task_id", StatePointer: "/header/event", State: e.Event})
				}
			}
		}
	}
	digest, err := testkit.WSContractDigest(*s)
	if err != nil {
		return empty, bad
	}
	s.Provenance.SourceSHA256 = digest
	if err := testkit.ValidateWSSession(f, limits); err != nil {
		return empty, fmt.Errorf("候选testkit校验失败: %w", err)
	}
	// 先流式计文件大小再允许保存；8MiB文件预算独立于4MiB解码轨迹。
	count := &inferenceCountWriter{limit: limits.FileBytes}
	if json.NewEncoder(count).Encode(f) != nil {
		return empty, bad
	}
	return f, nil
}

type inferenceCountWriter struct{ n, limit int64 }

func (w *inferenceCountWriter) Write(b []byte) (int, error) {
	if int64(len(b)) > w.limit-w.n {
		return 0, errors.New("候选文件超限")
	}
	w.n += int64(len(b))
	return len(b), nil
}

func inferenceSaveCandidate(root *os.Root, path string, f testkit.Fixture) error {
	file, err := inferenceExclusive(root, "candidate.json")
	if err != nil {
		return err
	}
	enc := json.NewEncoder(io.MultiWriter(file, &inferenceCountWriter{limit: testkit.DefaultWSLimits().FileBytes}))
	err = enc.Encode(f)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err != nil || syncErr != nil || closeErr != nil {
		return errors.New("候选写入失败")
	}
	if _, err := testkit.ReadWSFixture(path, testkit.DefaultWSLimits()); err != nil {
		return errors.New("保存候选未通过loader")
	}
	return nil
}
