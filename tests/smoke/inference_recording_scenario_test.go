package smoke_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"unicode/utf8"

	"github.com/yobo2u/omugw/internal/transport/ws"
)

type inferenceEvent struct {
	Event, TaskID                                      string
	Usage                                              bool
	Unit                                               string
	InputTokens, OutputTokens, TotalTokens, Characters int64
	Seconds                                            float64
}

func inferenceDecode(b []byte, model string) (inferenceEvent, error) {
	var e inferenceEvent
	bad := errors.New("原生事件或用量不能作为证据")
	v, err := inferenceJSON(b, "")
	if err != nil {
		return e, bad
	}
	m, ok := v.(map[string]any)
	if !ok {
		return e, bad
	}
	h, ok := m["header"].(map[string]any)
	if !ok {
		return e, bad
	}
	p, ok := m["payload"].(map[string]any)
	if !ok {
		return e, bad
	}
	e.Event, _ = h["event"].(string)
	e.TaskID, _ = h["task_id"].(string)
	if e.Event == "" || len(e.Event) > 128 || e.TaskID == "" || len(e.TaskID) > 512 {
		return inferenceEvent{}, bad
	}
	if e.Event != "result-generated" && e.Event != "task-finished" && e.Event != "task-failed" {
		return e, nil
	}
	if p["usage"] == nil {
		return e, nil
	}
	u, ok := p["usage"].(map[string]any)
	if !ok {
		return inferenceEvent{}, bad
	}
	integer := func(key string) (int64, bool) {
		n, ok := u[key].(json.Number)
		if !ok {
			return 0, false
		}
		v, err := n.Int64()
		return v, err == nil && v >= 0
	}
	switch model {
	case "qwen-audio-3.1-asr-flash-streaming", "qwen-audio-3.1-asr-flash-message":
		a, aok := integer("input_tokens")
		b, bok := integer("output_tokens")
		c, cok := integer("total_tokens")
		if !aok || !bok || !cok || a > math.MaxInt64-b || a+b != c || u["characters"] != nil {
			return inferenceEvent{}, bad
		}
		e.Unit = "tokens"
		e.InputTokens = a
		e.OutputTokens = b
		e.TotalTokens = c
	case "qwen-audio-3.0-asr-flash-streaming":
		n, ok := u["duration"].(json.Number)
		if !ok {
			return inferenceEvent{}, bad
		}
		f, err := n.Float64()
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) || f < 0 || u["characters"] != nil || u["input_tokens"] != nil || u["output_tokens"] != nil || u["total_tokens"] != nil {
			return inferenceEvent{}, bad
		}
		e.Unit = "seconds"
		e.Seconds = f
	case "qwen-audio-3.0-tts-flash", "sambert-zhichu-v1":
		n, ok := integer("characters")
		if !ok || u["duration"] != nil || u["input_tokens"] != nil || u["output_tokens"] != nil || u["total_tokens"] != nil {
			return inferenceEvent{}, bad
		}
		if model == "qwen-audio-3.0-tts-flash" && e.Event == "result-generated" {
			out, _ := p["output"].(map[string]any)
			if out["event"] != "sentence-end" {
				return e, nil
			}
		}
		e.Unit = "characters"
		e.Characters = n
	default:
		return inferenceEvent{}, bad
	}
	e.Usage = true
	return e, nil
}

// 正常阶段与failed尾部共用这一窄检查，防终态之后仍用同ID偷换模型或任务契约。
func inferenceBoundEvent(c inferenceRecordingConfig, b []byte, id string) (inferenceEvent, error) {
	e, err := inferenceDecode(b, c.Model)
	bad := errors.New("事件重申了矛盾的任务绑定")
	if err != nil || e.TaskID != id {
		return inferenceEvent{}, bad
	}
	v, _ := inferenceJSON(b, "")
	m := v.(map[string]any)
	p := m["payload"].(map[string]any)
	h := m["header"].(map[string]any)
	streaming, task, function := "duplex", "asr", "recognition"
	if c.Slot == 2 || c.Slot == 3 || c.Slot == 6 {
		task, function = "tts", "SpeechSynthesizer"
	}
	if c.Slot == 3 {
		streaming = "out"
	}
	if got, present := h["streaming"]; present && got != streaming {
		return inferenceEvent{}, bad
	}
	for field, want := range map[string]string{"model": c.Model, "task_group": "audio", "task": task, "function": function} {
		if got, present := p[field]; present && got != want {
			return inferenceEvent{}, bad
		}
	}
	return e, nil
}

func inferenceAdvanceUsage(last *inferenceEvent, e inferenceEvent) error {
	if !e.Usage {
		return nil
	}
	if last.Usage && (e.Unit != last.Unit || e.InputTokens < last.InputTokens || e.OutputTokens < last.OutputTokens || e.TotalTokens < last.TotalTokens || e.Characters < last.Characters || e.Seconds < last.Seconds) {
		return errors.New("累计用量回退或单位冲突")
	}
	*last = e
	return nil
}

func inferenceSameUsage(a, b inferenceEvent) bool {
	return a.Usage == b.Usage && a.Unit == b.Unit && a.InputTokens == b.InputTokens && a.OutputTokens == b.OutputTokens && a.TotalTokens == b.TotalTokens && a.Characters == b.Characters && a.Seconds == b.Seconds
}

func inferenceRequest(c inferenceRecordingConfig, action, id, text string) ([]byte, error) {
	if utf8.RuneCountInString(text) > 40 {
		return nil, errors.New("单任务文本超过40字符")
	}
	streaming := "duplex"
	if c.Slot == 3 {
		streaming = "out"
	}
	p := map[string]any{"input": map[string]any{}}
	if text != "" {
		p["input"] = map[string]any{"text": text}
	}
	if action == "run-task" {
		task, function := "asr", "recognition"
		params := map[string]any{"format": "pcm", "sample_rate": 16000}
		if c.Slot == 2 || c.Slot == 3 || c.Slot == 6 {
			task, function = "tts", "SpeechSynthesizer"
			if c.Voice != "" {
				params["voice"] = c.Voice
			}
		}
		p["task_group"], p["task"], p["function"], p["model"], p["parameters"] = "audio", task, function, c.Model, params
	}
	return json.Marshal(map[string]any{"header": map[string]any{"action": action, "task_id": id, "streaming": streaming}, "payload": p})
}

// 请求、就绪、用量和终态独立编写；不借被测网关或Inspector给自己出预期。
func inferenceDrive(c inferenceRecordingConfig, sample []byte, send func(ws.Opcode, []byte) error, recv func() (inferenceRecord, error)) (string, error) {
	bad := errors.New("场景缺少匹配就绪、终态或原单位证据")
	if c.Slot < 1 || c.Slot > 6 || len(sample) < 2 || len(sample) > inferenceSampleLimit || len(sample)%2 != 0 || inferenceSHA(sample) != c.SampleSHA256 {
		return "invalid_evidence", bad
	}
	tasks := int(c.Manifest.Slots[c.Slot-1].MaxTasks)
	if tasks < 1 || tasks > 2 || int64(utf8.RuneCountInString(inferenceTextA+inferenceTextB))*int64(tasks) > c.Manifest.Slots[c.Slot-1].MaxInputCharacters && (c.Slot == 2 || c.Slot == 3 || c.Slot == 6) {
		return "invalid_evidence", bad
	}
	write := func(action, id, text string) error {
		b, err := inferenceRequest(c, action, id, text)
		if err != nil {
			return err
		}
		return send(ws.OpText, b)
	}
	for n := 1; n <= tasks; n++ {
		id := fmt.Sprintf("s3-%d-%d", c.Slot, n)
		text := ""
		if c.Slot == 3 {
			text = inferenceTextA + inferenceTextB
		}
		if err := write("run-task", id, text); err != nil {
			return "interrupted", err
		}
		started := false
		var last inferenceEvent
		for {
			r, err := recv()
			if err != nil {
				return "interrupted", err
			}
			if r.Opcode == ws.OpBinary {
				if !started || c.Slot != 2 && c.Slot != 3 && c.Slot != 6 {
					return "invalid_evidence", bad
				}
				continue
			}
			if r.Opcode != ws.OpText {
				return "interrupted", bad
			}
			e, err := inferenceBoundEvent(c, r.Payload, id)
			if err != nil {
				return "invalid_evidence", bad
			}
			if inferenceAdvanceUsage(&last, e) != nil {
				return "invalid_evidence", bad
			}
			switch e.Event {
			case "task-failed":
				return "failed", nil
			case "task-started":
				if started {
					return "invalid_evidence", bad
				}
				started = true
				if c.Slot == 1 || c.Slot == 4 || c.Slot == 5 {
					if int64(len(sample))*int64(tasks) > c.Manifest.Slots[c.Slot-1].MaxInputAudioSeconds*32000 {
						return "invalid_evidence", bad
					}
					if err := send(ws.OpBinary, sample); err != nil {
						return "interrupted", err
					}
				} else if c.Slot == 2 || c.Slot == 6 {
					for _, text := range []string{inferenceTextA, inferenceTextB} {
						if err := write("continue-task", id, text); err != nil {
							return "interrupted", err
						}
					}
				}
				if c.Slot != 3 && !(c.Slot == 5 && n == 2) {
					if err := write("finish-task", id, ""); err != nil {
						return "interrupted", err
					}
				}
			case "result-generated":
				if !started {
					return "invalid_evidence", bad
				}
				if c.Slot == 5 && n == 2 && e.Usage {
					return "interrupted", nil
				}
			case "task-finished":
				if !started || c.Slot == 6 || c.Slot == 5 && n == 2 {
					return "invalid_evidence", bad
				}
				goto nextTask
			}
		}
	nextTask:
	}
	return "completed", nil
}
