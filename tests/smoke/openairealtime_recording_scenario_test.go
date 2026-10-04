package smoke_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strconv"
	"strings"
	"unicode"
)

type oaOutput struct {
	id, kind, name, call string
	parts                map[int]*oaPart
	arguments            string
	argumentsDone        bool
	itemDone             bool
}
type oaPart struct {
	kind, text string
	audio      int
}

// committed、conversation 回显与 response 输出共用身份表，不能把用户输入重新绑定成助手输出。
type oaItemIdentity struct {
	kind, role, name, call string
	output                 *oaOutput
}

type oaDriver struct {
	scenario                                                   string
	sample                                                     *oaSample
	send                                                       func([]byte) error
	recv                                                       func() (map[string]any, error)
	serial                                                     int
	session, model, active, input, vadStart, vadStop, cancelID string
	responseIDs                                                map[string]bool
	itemIDs                                                    map[string]*oaItemIdentity
	outputs                                                    map[int]*oaOutput
	waitingResponse                                            bool
	asr, confirmed                                             bool
	audioBytes                                                 int
	vadStartMS                                                 float64
	lastText                                                   string
	lastDone                                                   map[string]any
	coverage                                                   map[string]bool
}

func oaNewDriver(s string, sample *oaSample) *oaDriver {
	return &oaDriver{scenario: s, sample: sample, responseIDs: map[string]bool{}, itemIDs: map[string]*oaItemIdentity{}, coverage: map[string]bool{}}
}
func (d *oaDriver) write(kind string, body map[string]any) error {
	if body == nil {
		body = map[string]any{}
	}
	d.serial++
	body["type"] = kind
	body["event_id"] = fmt.Sprintf("oa_event_%03d", d.serial)
	b, err := json.Marshal(body)
	if err != nil {
		return errors.New("request_encoding")
	}
	return d.send(b)
}
func oaIndex(e map[string]any, k string) (int, bool) {
	v, ok := e[k].(float64)
	return int(v), ok && v >= 0 && v <= 100 && v == float64(int(v))
}

func (d *oaDriver) observe(e map[string]any) error {
	t := oaString(e, "type")
	if !d.confirmed && (strings.HasPrefix(t, "input_audio_buffer.") || strings.HasPrefix(t, "conversation.") || strings.HasPrefix(t, "response.")) {
		return errors.New("business_before_config")
	}
	switch t {
	case "error", "conversation.item.input_audio_transcription.failed":
		return errors.New("upstream_error")
	case "session.finished":
		return errors.New("non_ga_terminal")
	case "session.created", "session.updated":
		s := oaMap(e, "session")
		id := oaString(s, "id")
		model := oaString(s, "model")
		if id == "" || oaString(s, "object") != "realtime.session" || oaString(s, "type") != "realtime" || model != oaModel {
			return errors.New("session_identity")
		}
		if d.session == "" {
			if t != "session.created" {
				return errors.New("missing_ready")
			}
			d.session = id
			d.model = model
		} else if t == "session.created" || id != d.session || model != d.model || d.confirmed {
			return errors.New("session_identity")
		}
	case "input_audio_buffer.speech_started":
		ms, ok := e["audio_start_ms"].(float64)
		if d.scenario != "vad-interrupt" || d.vadStart != "" || oaString(e, "item_id") == "" || !ok || ms < 0 || ms > 8000 || ms != float64(int(ms)) {
			return errors.New("vad_identity")
		}
		d.vadStartMS = ms
		d.vadStart = oaString(e, "item_id")
	case "input_audio_buffer.speech_stopped":
		ms, ok := e["audio_end_ms"].(float64)
		if d.vadStart == "" || oaString(e, "item_id") != d.vadStart || d.vadStop != "" || !ok || ms < d.vadStartMS || ms > 8000 || ms != float64(int(ms)) {
			return errors.New("vad_identity")
		}
		d.vadStop = d.vadStart
	case "input_audio_buffer.committed":
		id := oaString(e, "item_id")
		if d.sample == nil || id == "" || d.input != "" || d.scenario == "vad-interrupt" && (id != d.vadStop || id != d.vadStart) {
			return errors.New("commit_identity")
		}
		if _, err := d.bindItem(map[string]any{"id": id, "type": "message", "role": "user"}); err != nil {
			return err
		}
		d.input = id
	case "conversation.item.added", "conversation.item.done":
		item := oaMap(e, "item")
		identity, err := d.bindItem(item)
		if err != nil {
			return err
		}
		if t == "conversation.item.done" && identity.kind == "function_call" && (identity.output == nil || !identity.output.toolTerminalMatches(item)) {
			return errors.New("tool_item_terminal_mismatch")
		}
	case "conversation.item.input_audio_transcription.completed":
		part, ok := oaIndex(e, "content_index")
		if !ok || part != 0 || d.input == "" || oaString(e, "item_id") != d.input || d.asr || strings.TrimSpace(oaString(e, "transcript")) == "" {
			return errors.New("asr_identity")
		}
		d.asr = true
		// 内容不符保留实录但不宣称识别正确；ASR 也不证明生成模型听到了相同内容。
		if oaText(oaString(e, "transcript")) == oaText(d.sample.Transcript) {
			d.coverage["speech_recognition"] = true
		}
	case "response.created":
		id := oaString(e, "response", "id")
		if id == "" || d.responseIDs[id] || d.active != "" || len(d.responseIDs) >= 4 || (!d.waitingResponse && !(d.scenario == "vad-interrupt" && d.input != "" && len(d.responseIDs) == 0)) {
			return errors.New("response_identity_or_limit")
		}
		d.waitingResponse = false
		d.responseIDs[id] = true
		d.active = id
		d.outputs = map[int]*oaOutput{}
		d.lastText = ""
	case "response.output_item.added":
		i, ok := oaIndex(e, "output_index")
		item := oaMap(e, "item")
		id := oaString(item, "id")
		kind := oaString(item, "type")
		if !ok || d.active == "" || oaString(e, "response_id") != d.active || id == "" || d.outputs[i] != nil || kind != "message" && kind != "function_call" {
			return errors.New("output_identity")
		}
		if kind == "message" && oaString(item, "role") != "assistant" {
			return errors.New("output_role")
		}
		arguments, hasArguments := item["arguments"].(string)
		if kind == "function_call" && (!hasArguments || oaString(item, "name") == "" || oaString(item, "call_id") == "") {
			return errors.New("tool_identity")
		}
		identity, err := d.bindItem(item)
		if err != nil {
			return err
		}
		if identity.output != nil {
			return errors.New("output_identity")
		}
		o := &oaOutput{id: id, kind: kind, name: oaString(item, "name"), call: oaString(item, "call_id"), parts: map[int]*oaPart{}, arguments: arguments}
		identity.output = o
		d.outputs[i] = o
	case "response.output_item.done":
		i, ok := oaIndex(e, "output_index")
		o := d.outputs[i]
		item := oaMap(e, "item")
		if !ok || o == nil || d.active == "" || oaString(e, "response_id") != d.active || oaString(e, "event_id") == "" || oaString(item, "id") != o.id || oaString(item, "type") != o.kind {
			return errors.New("output_identity")
		}
		if _, err := d.bindItem(item); err != nil {
			return err
		}
		if o.kind == "function_call" && !o.toolTerminalMatches(item) {
			return errors.New("tool_item_terminal_mismatch")
		}
		o.itemDone = true
	case "response.function_call_arguments.delta", "response.function_call_arguments.done":
		i, ok := oaIndex(e, "output_index")
		o := d.outputs[i]
		if !ok || o == nil || o.kind != "function_call" || d.active == "" || oaString(e, "response_id") != d.active || oaString(e, "item_id") != o.id || oaString(e, "call_id") != o.call || oaString(e, "event_id") == "" {
			return errors.New("tool_arguments_identity")
		}
		// GA delta 不要求 name；done 必须有 name。若 delta 额外回显也不能与绑定冲突。
		if name, present := e["name"]; (present || t == "response.function_call_arguments.done") && name != o.name {
			return errors.New("tool_arguments_identity")
		}
		if t == "response.function_call_arguments.delta" {
			delta, ok := e["delta"].(string)
			if !ok || o.argumentsDone || len(delta) > oaMaxBytes-len(o.arguments) {
				return errors.New("tool_arguments_sequence")
			}
			o.arguments += delta
		} else {
			arguments, ok := e["arguments"].(string)
			if !ok || arguments != o.arguments || !json.Valid([]byte(arguments)) {
				return errors.New("tool_arguments_mismatch")
			}
			o.argumentsDone = true
		}
	case "response.content_part.added":
		o, part, err := d.partEntity(e, false)
		if err != nil {
			return err
		}
		kind := oaString(e, "part", "type")
		if o.kind != "message" || o.parts[part] != nil || kind != "text" && kind != "audio" {
			return errors.New("part_identity")
		}
		// GA content_part 事件用 text/audio，assistant item 用 output_text/output_audio。
		// 仅在观测副本上关联这两种官方结构，原始消息始终不改写。
		o.parts[part] = &oaPart{kind: "output_" + kind}
	case "response.output_text.delta", "response.output_audio.delta":
		o, part, err := d.partEntity(e, true)
		if err != nil {
			return err
		}
		p := o.parts[part]
		if t == "response.output_text.delta" {
			if p.kind != "output_text" {
				return errors.New("part_type")
			}
			p.text += oaString(e, "delta")
			d.lastText += oaString(e, "delta")
		} else {
			b, err := base64.StdEncoding.DecodeString(oaString(e, "delta"))
			if p.kind != "output_audio" || err != nil || len(b) == 0 || len(b)%2 != 0 || len(b) > oaMaxAudio-d.audioBytes {
				return errors.New("audio_encoding_or_limit")
			}
			p.audio += len(b)
			d.audioBytes += len(b)
		}
	case "response.done":
		r := oaMap(e, "response")
		id := oaString(r, "id")
		status := oaString(r, "status")
		if d.active == "" || id != d.active || status != "completed" && status != "cancelled" || d.cancelID != "" && (id != d.cancelID || status != "cancelled") || status == "cancelled" && d.cancelID != id {
			return errors.New("response_terminal_identity")
		}
		out, ok := r["output"].([]any)
		if !ok || len(out) != len(d.outputs) || len(out) == 0 {
			return errors.New("missing_output")
		}
		for i, v := range out {
			m, _ := v.(map[string]any)
			o := d.outputs[i]
			if o == nil || oaString(m, "id") != o.id || oaString(m, "type") != o.kind {
				return errors.New("terminal_output_identity")
			}
			if _, err := d.bindItem(m); err != nil {
				return err
			}
			if o.kind == "function_call" {
				if !o.itemDone || !o.toolTerminalMatches(m) {
					return errors.New("terminal_tool_identity")
				}
			} else {
				parts, ok := m["content"].([]any)
				if !ok || len(parts) != len(o.parts) {
					return errors.New("terminal_part_identity")
				}
				for j, v := range parts {
					m, _ := v.(map[string]any)
					p := o.parts[j]
					if p == nil || oaString(m, "type") != p.kind || p.kind == "output_text" && oaString(m, "text") != p.text {
						return errors.New("terminal_part_identity")
					}
				}
			}
		}
		tokens, ok := oaMap(r, "usage")["output_tokens"].(float64)
		if !ok || tokens < 0 || tokens > 128 || tokens != float64(int(tokens)) {
			return errors.New("output_token_limit_or_missing")
		}
		d.active = ""
		d.lastDone = e
		if d.lastText != "" {
			d.coverage["text_generation"] = true
			d.coverage["streaming"] = true
		}
	default:
		// 已关联实体的补充事件也不得偷换 response/item/part；未知事件保留但不举证。
		if strings.HasPrefix(t, "response.") && e["response_id"] != nil {
			if d.active == "" || oaString(e, "response_id") != d.active {
				return errors.New("response_identity")
			}
			if e["output_index"] != nil || e["item_id"] != nil || e["item"] != nil {
				i, ok := oaIndex(e, "output_index")
				o := d.outputs[i]
				if !ok || o == nil || e["item_id"] != nil && oaString(e, "item_id") != o.id {
					return errors.New("output_identity")
				}
				if item := oaMap(e, "item"); item != nil {
					if oaString(item, "id") != o.id || oaString(item, "type") != o.kind || o.kind == "function_call" && (oaString(item, "name") != o.name || oaString(item, "call_id") != o.call) {
						return errors.New("output_identity")
					}
					if _, err := d.bindItem(item); err != nil {
						return err
					}
				}
			}
			if e["content_index"] != nil {
				o, index, err := d.partEntity(e, true)
				if err != nil {
					return err
				}
				p := o.parts[index]
				if t == "response.output_text.done" && (p.kind != "output_text" || oaString(e, "text") != p.text) || t == "response.output_audio.done" && p.kind != "output_audio" {
					return errors.New("part_completion_mismatch")
				}
				if t == "response.content_part.done" && ("output_"+oaString(e, "part", "type") != p.kind || p.kind == "output_text" && oaString(e, "part", "text") != p.text) {
					return errors.New("part_completion_mismatch")
				}
			}
		}
	}
	return nil
}

// 最终合法 response.done 不能洗掉参数流、参数 done 或 item done 的冲突。
func (o *oaOutput) toolTerminalMatches(item map[string]any) bool {
	arguments, ok := item["arguments"].(string)
	return o.argumentsDone && ok && arguments == o.arguments && oaString(item, "name") == o.name && oaString(item, "call_id") == o.call
}

func (d *oaDriver) bindItem(item map[string]any) (*oaItemIdentity, error) {
	id := oaString(item, "id")
	identity := oaItemIdentity{kind: oaString(item, "type"), role: oaString(item, "role"), name: oaString(item, "name"), call: oaString(item, "call_id")}
	if id == "" {
		return nil, errors.New("conversation_item_identity")
	}
	switch identity.kind {
	case "message":
		if identity.role != "user" && identity.role != "assistant" && identity.role != "system" {
			return nil, errors.New("conversation_item_identity")
		}
	case "function_call":
		if identity.name == "" || identity.call == "" {
			return nil, errors.New("conversation_item_identity")
		}
	case "function_call_output":
		if identity.call == "" {
			return nil, errors.New("conversation_item_identity")
		}
	default:
		return nil, errors.New("conversation_item_identity")
	}
	if existing := d.itemIDs[id]; existing != nil {
		if existing.kind != identity.kind || existing.role != identity.role || existing.name != identity.name || existing.call != identity.call {
			return nil, errors.New("conversation_item_identity")
		}
		return existing, nil
	}
	d.itemIDs[id] = &identity
	return &identity, nil
}

func (d *oaDriver) partEntity(e map[string]any, existing bool) (*oaOutput, int, error) {
	i, ok := oaIndex(e, "output_index")
	j, jok := oaIndex(e, "content_index")
	o := d.outputs[i]
	if !ok || !jok || o == nil || d.active == "" || oaString(e, "response_id") != d.active || oaString(e, "item_id") != o.id || existing && o.parts[j] == nil {
		return nil, 0, errors.New("part_identity")
	}
	return o, j, nil
}
func (d *oaDriver) next() (map[string]any, error) {
	e, err := d.recv()
	if err != nil {
		return nil, err
	}
	if err := d.observe(e); err != nil {
		return nil, err
	}
	return e, nil
}
func (d *oaDriver) wait(kind string) (map[string]any, error) {
	for {
		e, err := d.next()
		if err != nil {
			return nil, err
		}
		if oaString(e, "type") == kind {
			return e, nil
		}
	}
}

func (d *oaDriver) config() map[string]any {
	s := map[string]any{"type": "realtime", "max_output_tokens": 128}
	if d.scenario == "text-tools-vision" {
		s["output_modalities"] = []string{"text"}
		s["instructions"] = "公开无隐私协议测试；简短回答。"
		s["reasoning"] = map[string]any{"effort": "minimal"}
		s["parallel_tool_calls"] = true
		var tools []any
		for _, x := range []struct{ name, desc string }{{"test_color", "返回公开测试颜色。"}, {"test_shape", "返回公开测试形状。"}} {
			tools = append(tools, map[string]any{"type": "function", "name": x.name, "description": x.desc, "parameters": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}})
		}
		s["tools"] = tools
		s["audio"] = map[string]any{"input": map[string]any{"turn_detection": nil}}
	} else {
		s["output_modalities"] = []string{"audio"}
		s["instructions"] = "公开测试，请从一数到二十。"
		var vad any
		if d.scenario == "vad-interrupt" {
			vad = map[string]any{"type": "server_vad", "threshold": 0.5, "prefix_padding_ms": 300, "silence_duration_ms": 500, "create_response": true, "interrupt_response": false}
		}
		s["audio"] = map[string]any{"input": map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}, "transcription": map[string]any{"model": "gpt-4o-mini-transcribe"}, "turn_detection": vad}, "output": map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}, "voice": "marin"}}
	}
	return s
}

func (d *oaDriver) run() error {
	e, err := d.next()
	if err != nil {
		return err
	}
	if oaString(e, "type") != "session.created" {
		return errors.New("first_event_not_ready")
	}
	// 未核验的 snapshot 也只留原始轨迹；不能由相似前缀猜测模型与权限。
	if err := d.write("session.update", map[string]any{"session": d.config()}); err != nil {
		return err
	}
	e, err = d.wait("session.updated")
	if err != nil {
		return err
	}
	if !oaMatches(d.config(), oaMap(e, "session")) {
		return errors.New("unconfirmed_config")
	}
	d.confirmed = true
	d.coverage["realtime_session"] = true
	switch d.scenario {
	case "text-tools-vision":
		d.coverage["reasoning"] = true
		return d.textToolsVision()
	case "audio-manual", "vad-interrupt":
		return d.audio()
	default:
		return errors.New("unknown_scenario")
	}
}

func (d *oaDriver) item(item map[string]any) error {
	id := oaString(item, "id")
	if id == "" || d.itemIDs[id] != nil {
		return errors.New("duplicate_authored_item")
	}
	if _, err := d.bindItem(item); err != nil {
		return err
	}
	if err := d.write("conversation.item.create", map[string]any{"item": item}); err != nil {
		return err
	}
	e, err := d.wait("conversation.item.added")
	if err != nil {
		return err
	}
	if !oaMatches(item, oaMap(e, "item")) {
		return errors.New("item_ack_mismatch")
	}
	return nil
}
func (d *oaDriver) textItem(id, text string) error {
	return d.item(map[string]any{"id": id, "type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}})
}
func (d *oaDriver) response(choice string) error {
	if d.active != "" || d.waitingResponse || len(d.responseIDs) >= 4 {
		return errors.New("response_limit")
	}
	if err := d.write("response.create", map[string]any{"response": map[string]any{"max_output_tokens": 128, "tool_choice": choice}}); err != nil {
		return err
	}
	d.waitingResponse = true
	_, err := d.wait("response.done")
	return err
}

func (d *oaDriver) textToolsVision() error {
	if err := d.textItem("oa_memory", "记住公开口令：青色灯塔。只回复已记住。"); err != nil {
		return err
	}
	if err := d.response("none"); err != nil {
		return err
	}
	if err := d.textItem("oa_recall", "只复述刚才口令，不调用工具。"); err != nil {
		return err
	}
	if err := d.response("none"); err != nil {
		return err
	}
	if oaText(d.lastText) != "青色灯塔" {
		return errors.New("memory_not_recalled")
	}
	d.coverage["stateful_conversation"] = true
	if err := d.textItem("oa_tools", "在同一轮调用 test_color 和 test_shape，各一次。"); err != nil {
		return err
	}
	if err := d.response("required"); err != nil {
		return err
	}
	out, _ := oaMap(d.lastDone, "response")["output"].([]any)
	if len(out) != 2 {
		return errors.New("invalid_tool_set")
	}
	calls := map[string]string{}
	ids := map[string]bool{}
	for _, v := range out {
		m, _ := v.(map[string]any)
		name, call := oaString(m, "name"), oaString(m, "call_id")
		var args map[string]any
		if oaString(m, "type") != "function_call" || name != "test_color" && name != "test_shape" || calls[name] != "" || call == "" || ids[call] || json.Unmarshal([]byte(oaString(m, "arguments")), &args) != nil || args == nil || len(args) != 0 {
			return errors.New("invalid_tool_set")
		}
		calls[name] = call
		ids[call] = true
	}
	// 整个集合校验完毕后才发第一个结果，不能边验证边执行。
	for i, x := range []struct{ name, value string }{{"test_color", "绿色"}, {"test_shape", "三角形"}} {
		if err := d.item(map[string]any{"id": "oa_result_" + strconv.Itoa(i), "type": "function_call_output", "call_id": calls[x.name], "output": x.value}); err != nil {
			return err
		}
	}
	if err := d.item(map[string]any{"id": "oa_image", "type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "只描述图片中的颜色和形状，不要沿用工具值。"}, map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(oaImage()), "detail": "high"}}}); err != nil {
		return err
	}
	if err := d.response("none"); err != nil {
		return err
	}
	d.coverage["tool_calling"] = true
	d.coverage["parallel_tool_calls"] = true
	// 图像与音频内容必须另行人工核验，不能因消息有字节或非空文本就填 Coverage。
	return nil
}

func (d *oaDriver) audio() error {
	if d.sample == nil || len(d.sample.Data) < 4 || len(d.sample.Data) > oaMaxAudio || len(d.sample.Data)%2 != 0 || oaDigest(d.sample.Data) != d.sample.SHA256 {
		return errors.New("invalid_sample")
	}
	pcm := d.sample.Data
	half := (len(pcm) / 4) * 2
	for _, b := range [][]byte{pcm[:half], pcm[half:]} {
		if err := d.write("input_audio_buffer.append", map[string]any{"audio": base64.StdEncoding.EncodeToString(b)}); err != nil {
			return err
		}
	}
	if d.scenario == "audio-manual" {
		if err := d.write("input_audio_buffer.commit", nil); err != nil {
			return err
		}
		if _, err := d.wait("input_audio_buffer.committed"); err != nil {
			return err
		}
		if err := d.response("none"); err != nil {
			return err
		}
		if d.audioBytes == 0 {
			return errors.New("missing_audio")
		}
	} else {
		e, err := d.wait("response.output_audio.delta")
		if err != nil {
			return err
		}
		id := d.active
		item := oaString(e, "item_id")
		part, _ := oaIndex(e, "content_index")
		if id == "" || d.input == "" {
			return errors.New("inactive_cancel")
		}
		if err := d.write("response.cancel", map[string]any{"response_id": id}); err != nil {
			return err
		}
		d.cancelID = id
		if _, err := d.wait("response.done"); err != nil {
			return err
		}
		if err := d.write("conversation.item.truncate", map[string]any{"item_id": item, "content_index": part, "audio_end_ms": 0}); err != nil {
			return err
		}
		e, err = d.wait("conversation.item.truncated")
		if err != nil {
			return err
		}
		if !oaMatches(map[string]any{"item_id": item, "content_index": part, "audio_end_ms": 0}, e) {
			return errors.New("truncate_ack_mismatch")
		}
		d.coverage["realtime_server_vad"] = true
		d.coverage["realtime_interrupt_turns"] = true
	}
	for !d.asr {
		if _, err := d.next(); err != nil {
			return err
		}
	}
	return nil
}

func oaText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.IsPunct(r) {
			return -1
		}
		return r
	}, s)
}
func oaImage() []byte {
	im := image.NewRGBA(image.Rect(0, 0, 160, 120))
	for y := 0; y < 120; y++ {
		for x := 0; x < 160; x++ {
			c := color.RGBA{255, 255, 255, 255}
			if (x-40)*(x-40)+(y-60)*(y-60) < 400 {
				c = color.RGBA{255, 0, 0, 255}
			}
			if x >= 95 && x < 135 && y >= 40 && y < 80 {
				c = color.RGBA{0, 0, 255, 255}
			}
			im.SetRGBA(x, y, c)
		}
	}
	var b bytes.Buffer
	_ = png.Encode(&b, im)
	return b.Bytes()
}
