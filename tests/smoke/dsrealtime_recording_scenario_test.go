//go:build smoke

package smoke_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/yobo2u/omugw/internal/transport/ws"
)

type dsRecord struct {
	Direction   string    `json:"direction"`
	Kind        string    `json:"kind"`
	Opcode      ws.Opcode `json:"opcode,omitempty"`
	Payload     []byte    `json:"payload,omitempty"`
	CloseCode   uint16    `json:"close_code,omitempty"`
	CloseReason string    `json:"close_reason,omitempty"`
}

type dsRecording struct {
	Scenario  string            `json:"scenario"`
	Model     string            `json:"requested_model"`
	Started   time.Time         `json:"started_at"`
	Status    int               `json:"handshake_status"`
	Headers   map[string]string `json:"safe_response_headers,omitempty"`
	Confirmed bool              `json:"configuration_confirmed"`
	Failure   string            `json:"failure,omitempty"`
	Records   []dsRecord        `json:"records"`
	Audio     []byte            `json:"-"`
	Input     *dsAudioSample    `json:"input_sample,omitempty"`
	Image     []byte            `json:"-"`
}

type dsIncoming struct {
	op      ws.Opcode
	payload []byte
	err     error
}
type dsDriver struct {
	ctx                context.Context
	c                  *ws.Conn
	cfg                dsConfig
	r                  *dsRecording
	in                 <-chan dsIncoming
	serial, traceBytes int
	responses          map[string]bool
	responseDone       map[string]bool
	activeResponse     string
	audioResponse      string
	cancelTarget       string
	inputItem          string
	asrItems           map[string]bool
}

// 唯一拨号点；所有失败直接保存分类并退出，绝不切模型、切地域或重试。
func dsCapture(parent context.Context, cfg dsConfig) dsRecording {
	r := dsRecording{Scenario: cfg.Scenario, Model: cfg.Model, Started: time.Now().UTC(), Input: cfg.Sample}
	if cfg.Sample != nil {
		sample := *cfg.Sample
		sample.File = "input.pcm"
		r.Input = &sample
	}
	if cfg.Scenario == "audio-image" {
		r.Image = dsPublicJPEG()
	}
	ctx, cancel := context.WithTimeout(parent, min(cfg.Duration, 59*time.Second))
	defer cancel()
	u, err := url.Parse(cfg.URL)
	if err != nil {
		r.Failure = "configuration"
		return r
	}
	q := u.Query()
	q.Set("model", cfg.Model)
	u.RawQuery = q.Encode()
	c, resp, err := ws.Dial(ctx, u.String(), ws.DialOptions{Header: http.Header{"Authorization": {"Bearer " + cfg.Key}}, MaxPayload: dsMaxMessage, Idle: 15 * time.Second, ConnectTimeout: 5 * time.Second, WriteTimeout: time.Second, MaxHandshakeBytes: 64 << 10, MaxErrorBodyBytes: 4096})
	if resp != nil {
		r.Status = resp.StatusCode
		r.Headers = dsSafeHeaders(resp.Header)
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
	}
	if err != nil {
		r.Failure = "handshake_failed"
		return r
	}
	stop := make(chan struct{})
	closed := make(chan bool, 1)
	go func() {
		select {
		case <-ctx.Done():
			sent, _ := c.CloseWithResult(1001, "")
			closed <- sent
		case <-stop:
			closed <- false
		}
	}()
	in := make(chan dsIncoming, 1)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			op, b, err := c.ReadMessage()
			select {
			case in <- dsIncoming{op, b, err}:
			case <-stop:
				return
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	d := dsDriver{ctx: ctx, c: c, cfg: cfg, r: &r, in: in, responses: map[string]bool{}, responseDone: map[string]bool{}}
	if err := d.run(); err != nil {
		r.Failure = err.Error()
	} else if len(r.Records) > 0 && r.Records[len(r.Records)-1].Kind != "close" {
		if err := d.closeHandshake(); err != nil {
			r.Failure = err.Error()
		}
	}
	// 先封闭 worker，再关连接唤醒读取，所有后台工作返回前必须 join。
	close(stop)
	sent, _ := c.CloseWithResult(1000, "")
	watchSent := <-closed
	<-readDone
	if watchSent {
		r.Records = append(r.Records, dsRecord{Direction: "send", Kind: "close", CloseCode: 1001})
	} else if sent {
		r.Records = append(r.Records, dsRecord{Direction: "send", Kind: "close", CloseCode: 1000})
	}
	if ctx.Err() != nil {
		r.Failure = "timeout_or_cancelled"
	}
	return r
}

// CloseWithResult 会立即释放 socket；录制器先显式发控制帧并独立读回应，才有跨端证据。
// 等待被一秒及会话总期限双重约束；失败仍由 capture 的关闭/worker join 兜底。
func (d *dsDriver) closeHandshake() error {
	if err := d.c.WriteMessage(ws.OpClose, ws.EncodeClosePayload(1000, "")); err != nil {
		return errors.New("close_write_failed")
	}
	d.r.Records = append(d.r.Records, dsRecord{Direction: "send", Kind: "close", CloseCode: 1000})
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-d.ctx.Done():
		return errors.New("timeout_or_cancelled")
	case <-timer.C:
		return errors.New("close_not_observed")
	case incoming := <-d.in:
		e, err := d.observe(incoming)
		if err != nil {
			return err
		}
		if dsString(e, "type") != "recorder.peer_close" {
			return errors.New("late_message_during_close")
		}
		if d.r.Records[len(d.r.Records)-1].CloseCode != 1000 {
			return errors.New("abnormal_peer_close")
		}
		return nil
	}
}

func (d *dsDriver) send(kind string, body map[string]any) error {
	if d.ctx.Err() != nil {
		return errors.New("timeout_or_cancelled")
	}
	d.serial++
	if body == nil {
		body = map[string]any{}
	}
	body["type"], body["event_id"] = kind, fmt.Sprintf("record_%03d", d.serial)
	b, err := json.Marshal(body)
	if err != nil || len(b) > dsMaxMessage || !dsSafePayload(b, d.cfg.Key) {
		return errors.New("unsafe_request")
	}
	if len(d.r.Records) >= dsMaxRecords || len(b) > dsMaxTrace-d.traceBytes {
		return errors.New("trace_limit")
	}
	if err := d.c.WriteMessage(ws.OpText, b); err != nil {
		return errors.New("write_failed")
	}
	d.traceBytes += len(b)
	d.r.Records = append(d.r.Records, dsRecord{Direction: "send", Kind: "message", Opcode: ws.OpText, Payload: b})
	return nil
}

func (d *dsDriver) receive() (map[string]any, error) {
	select {
	case <-d.ctx.Done():
		return nil, errors.New("timeout_or_cancelled")
	case m := <-d.in:
		return d.observe(m)
	}
}

func (d *dsDriver) observe(m dsIncoming) (map[string]any, error) {
	if m.err != nil {
		var ce *ws.CloseError
		if errors.As(m.err, &ce) {
			defer ce.Release()
			if ce.IncompleteMessage {
				return nil, errors.New("incomplete_message")
			}
			if ce.Code == 1005 || !dsSafePayload([]byte(fmt.Sprintf(`{"reason":%q}`, ce.Reason)), d.cfg.Key) {
				return nil, errors.New("unsafe_close")
			}
			d.r.Records = append(d.r.Records, dsRecord{Direction: "receive", Kind: "close", CloseCode: ce.Code, CloseReason: ce.Reason})
			return map[string]any{"type": "recorder.peer_close"}, nil
		}
		return nil, errors.New("read_failed_or_raw_eof")
	}
	if m.op != ws.OpText || !dsSafePayload(m.payload, d.cfg.Key) {
		return nil, errors.New("unsafe_or_non_json_event")
	}
	if len(d.r.Records) >= dsMaxRecords || len(m.payload) > dsMaxTrace-d.traceBytes {
		return nil, errors.New("trace_limit")
	}
	d.traceBytes += len(m.payload)
	d.r.Records = append(d.r.Records, dsRecord{Direction: "receive", Kind: "message", Opcode: m.op, Payload: m.payload})
	var e map[string]any
	_ = json.Unmarshal(m.payload, &e)
	typ := dsString(e, "type")
	if typ == "error" {
		return nil, errors.New("upstream_error")
	}
	if typ == "conversation.item.input_audio_transcription.failed" {
		return nil, errors.New("transcription_failed")
	}
	if typ == "input_audio_buffer.committed" {
		d.inputItem = dsString(e, "item_id")
		if d.inputItem == "" {
			return nil, errors.New("missing_input_item")
		}
	}
	if typ == "conversation.item.input_audio_transcription.completed" {
		id, text := dsString(e, "item_id"), dsString(e, "transcript")
		if id == "" || strings.TrimSpace(text) == "" || len(text) > 1024 {
			return nil, errors.New("invalid_transcription")
		}
		if d.asrItems == nil {
			d.asrItems = map[string]bool{}
		}
		d.asrItems[id] = true
	}
	if typ == "response.created" || typ == "response.done" {
		id := dsString(e, "response", "id")
		if id == "" {
			return nil, errors.New("missing_response_id")
		}
		d.responses[id] = true
		if len(d.responses) > d.cfg.Responses {
			return nil, errors.New("response_limit")
		}
		if typ == "response.created" {
			if d.activeResponse != "" || d.responseDone[id] {
				return nil, errors.New("response_already_active_or_finished")
			}
			d.activeResponse = id
		}
		if typ == "response.done" {
			if !strings.HasPrefix(d.cfg.Scenario, "tts-") {
				if tokens, ok := dsMap(e, "response", "usage")["output_tokens"].(float64); ok && (tokens < 0 || tokens > 128) {
					return nil, errors.New("output_token_limit")
				}
			}
			if d.responseDone[id] {
				return nil, errors.New("duplicate_response_terminal")
			}
			if d.activeResponse != "" && d.activeResponse != id {
				return nil, errors.New("response_identity_mismatch")
			}
			status := dsString(e, "response", "status")
			if d.cancelTarget != "" && (id != d.cancelTarget || status != "cancelled") {
				return nil, errors.New("cancel_target_not_cancelled")
			}
			if status != "completed" && !(d.cfg.Scenario == "vad-interrupt" && status == "cancelled" && id == d.cancelTarget && id == d.audioResponse && id == d.activeResponse) {
				return nil, errors.New("response_not_completed")
			}
			d.responseDone[id] = true
			d.activeResponse = ""
		}
	}
	if typ == "response.audio.delta" {
		id := dsString(e, "response_id")
		if id == "" || id != d.activeResponse || d.responseDone[id] {
			return nil, errors.New("audio_response_not_active")
		}
		b, err := base64.StdEncoding.DecodeString(dsString(e, "delta"))
		if err != nil || len(b) == 0 || len(b) > dsMaxAudio-len(d.r.Audio) {
			return nil, errors.New("output_audio_limit_or_encoding")
		}
		d.r.Audio = append(d.r.Audio, b...)
		d.audioResponse = id
		if d.cfg.Scenario == "vad-interrupt" && d.cancelTarget == "" {
			if err := d.send("response.cancel", nil); err != nil {
				return nil, err
			}
			// 官方 cancel 不带 response_id；只绑定本次成功写出时仍活跃的同实体音频响应。
			d.cancelTarget = id
		}
	}
	return e, nil
}

func (d *dsDriver) wait(kind string) (map[string]any, error) {
	for {
		e, err := d.receive()
		if err != nil {
			return nil, err
		}
		if dsString(e, "type") == kind {
			return e, nil
		}
		if dsString(e, "type") == "recorder.peer_close" {
			return nil, errors.New("early_peer_close")
		}
	}
}

func (d *dsDriver) run() error {
	if _, err := d.wait("session.created"); err != nil {
		return err
	}
	session := d.session()
	if err := d.send("session.update", map[string]any{"session": session}); err != nil {
		return err
	}
	e, err := d.wait("session.updated")
	if err != nil {
		return err
	}
	if !dsEchoMatches(session, dsMap(e, "session")) {
		return errors.New("unconfirmed_config")
	}
	d.r.Confirmed = true
	switch d.cfg.Scenario {
	case "tts-commit", "tts-server-commit":
		return d.tts()
	case "text-tools":
		return d.textTools()
	case "audio-image", "vad-interrupt":
		return d.audio()
	default:
		return errors.New("unknown_scenario")
	}
}

func (d *dsDriver) session() map[string]any {
	if strings.HasPrefix(d.cfg.Scenario, "tts-") {
		mode := "commit"
		if d.cfg.Scenario == "tts-server-commit" {
			mode = "server_commit"
		}
		return map[string]any{"mode": mode, "voice": "Cherry", "response_format": "pcm", "sample_rate": 16000, "language_type": "Chinese"}
	}
	s := map[string]any{"modalities": []string{"text"}, "turn_detection": nil, "max_tokens": 128, "instructions": "这是公开的无隐私测试。简短作答，不超过二十个字。"}
	if d.cfg.Scenario == "text-tools" {
		s["tools"] = []any{
			map[string]any{"type": "function", "function": map[string]any{"name": "test_color", "description": "返回测试图形颜色；与 test_shape 同轮调用。", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}},
			map[string]any{"type": "function", "function": map[string]any{"name": "test_shape", "description": "返回测试图形形状；与 test_color 同轮调用。", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}},
		}
	} else {
		s["audio"] = map[string]any{"input": map[string]any{"format": map[string]any{"type": "pcm", "sample_rate": 16000}}, "output": map[string]any{"format": map[string]any{"type": "pcm", "sample_rate": 16000}}}
		if d.cfg.Scenario == "vad-interrupt" {
			s["modalities"] = []string{"text", "audio"}
			s["turn_detection"] = map[string]any{"type": "server_vad", "threshold": 0.5, "silence_duration_ms": 500}
			s["instructions"] = "这是公开测试，请从一数到二十。"
		}
	}
	return s
}

// 要求回显包含每一项请求配置，尤其格式/采样率/输出上限；只看 updated 名称不算采纳。
func dsEchoMatches(want, got map[string]any) bool {
	for k, w := range want {
		g, ok := got[k]
		if !ok {
			return false
		}
		if nested, ok := w.(map[string]any); ok {
			gm, ok := g.(map[string]any)
			if !ok || !dsEchoMatches(nested, gm) {
				return false
			}
			continue
		}
		wb, _ := json.Marshal(w)
		gb, _ := json.Marshal(g)
		if !bytes.Equal(wb, gb) {
			return false
		}
	}
	return true
}

func (d *dsDriver) tts() error {
	if err := d.send("input_text_buffer.append", map[string]any{"text": dsPublicText}); err != nil {
		return err
	}
	if d.cfg.Scenario == "tts-commit" {
		if err := d.send("input_text_buffer.commit", nil); err != nil {
			return err
		}
		if _, err := d.wait("input_text_buffer.committed"); err != nil {
			return err
		}
	}
	if _, err := d.wait("response.done"); err != nil {
		return err
	}
	if d.cfg.Scenario == "tts-server-commit" && len(dsAutomaticCommitResponse(d.r.Records)) == 0 {
		return errors.New("automatic_commit_not_observed")
	}
	if err := d.send("session.finish", nil); err != nil {
		return err
	}
	if _, err := d.wait("session.finished"); err != nil {
		return err
	}
	if len(d.responseDone) != 1 || len(d.r.Audio) == 0 {
		return errors.New("missing_tts_audio_or_terminal")
	}
	_, err := d.wait("recorder.peer_close")
	return err
}

func (d *dsDriver) textItem(id, text string) error {
	if err := d.send("conversation.item.create", map[string]any{"item": map[string]any{"id": id, "type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}}}); err != nil {
		return err
	}
	for {
		e, err := d.wait("conversation.item.created")
		if err != nil {
			return err
		}
		if dsString(e, "item", "id") == id {
			return nil
		}
	}
}

func (d *dsDriver) textTools() error {
	if err := d.textItem("record_user_1", "记住测试口令：蓝色方块。只回复已记住，不调用工具。"); err != nil {
		return err
	}
	if err := d.send("response.create", nil); err != nil {
		return err
	}
	if _, err := d.wait("response.done"); err != nil {
		return err
	}
	if err := d.textItem("record_user_2", "先复述刚才口令，再在同一轮调用 test_color 和 test_shape，不要猜测结果。"); err != nil {
		return err
	}
	if err := d.send("response.create", nil); err != nil {
		return err
	}
	e, err := d.wait("response.done")
	if err != nil {
		return err
	}
	output, _ := dsMap(e, "response")["output"].([]any)
	seen := map[string]bool{}
	for _, v := range output {
		item, _ := v.(map[string]any)
		if dsString(item, "type") != "function_call" {
			continue
		}
		name, id := dsString(item, "name"), dsString(item, "call_id")
		if id == "" || seen[name] || (name != "test_color" && name != "test_shape") {
			return errors.New("unexpected_tool_call")
		}
		seen[name] = true
		result := "蓝色"
		if name == "test_shape" {
			result = "方块"
		}
		if err := d.send("conversation.item.create", map[string]any{"item": map[string]any{"id": "record_result_" + name, "type": "function_call_output", "call_id": id, "output": result}}); err != nil {
			return err
		}
		for {
			ack, err := d.wait("conversation.item.created")
			if err != nil {
				return err
			}
			if dsString(ack, "item", "call_id") == id {
				break
			}
		}
	}
	if len(seen) != 2 {
		return errors.New("missing_parallel_tools")
	}
	if err := d.send("response.create", nil); err != nil {
		return err
	}
	_, err = d.wait("response.done")
	return err
}

func (d *dsDriver) audio() error {
	if d.cfg.Sample == nil {
		return errors.New("missing_audio_sample")
	}
	pcm := d.cfg.Sample.Data
	if d.cfg.Scenario == "vad-interrupt" {
		pcm = append(bytes.Clone(pcm), make([]byte, 16000*2*7/10)...)
	}
audioLoop:
	for offset := 0; offset < len(pcm); offset += 3200 {
		b := pcm[offset:min(offset+3200, len(pcm))]
		if err := d.send("input_audio_buffer.append", map[string]any{"audio": base64.StdEncoding.EncodeToString(b)}); err != nil {
			return err
		}
		if d.cfg.Scenario == "vad-interrupt" {
			timer := time.NewTimer(100 * time.Millisecond)
		waitTick:
			for {
				select {
				case <-timer.C:
					break waitTick
				case <-d.ctx.Done():
					timer.Stop()
					return errors.New("timeout_or_cancelled")
				case m := <-d.in:
					e, err := d.observe(m)
					if err != nil {
						timer.Stop()
						return err
					}
					if dsString(e, "type") == "response.done" {
						timer.Stop()
						if err := d.checkCancelled(e); err != nil {
							return err
						}
						return d.waitTranscription()
					}
					if d.inputItem != "" {
						timer.Stop()
						break audioLoop
					}
					if dsString(e, "type") == "recorder.peer_close" {
						timer.Stop()
						return errors.New("early_peer_close")
					}
				}
			}
		}
	}
	if d.cfg.Scenario == "audio-image" {
		if err := d.send("input_image_buffer.append", map[string]any{"image": base64.StdEncoding.EncodeToString(d.r.Image)}); err != nil {
			return err
		}
		if err := d.send("input_audio_buffer.commit", nil); err != nil {
			return err
		}
		if _, err := d.wait("input_audio_buffer.committed"); err != nil {
			return err
		}
		if err := d.send("response.create", nil); err != nil {
			return err
		}
	}
	e, err := d.wait("response.done")
	if err != nil {
		return err
	}
	if d.cfg.Scenario == "vad-interrupt" {
		if err := d.checkCancelled(e); err != nil {
			return err
		}
	}
	return d.waitTranscription()
}

// ASR 可以晚于 response.done；必须按 committed 的 item_id 等待，不能把异步结果漏掉。
func (d *dsDriver) waitTranscription() error {
	if d.inputItem == "" {
		return errors.New("missing_input_item")
	}
	for !d.asrItems[d.inputItem] {
		if _, err := d.wait("conversation.item.input_audio_transcription.completed"); err != nil {
			return err
		}
	}
	return nil
}

func (d *dsDriver) checkCancelled(e map[string]any) error {
	id := dsString(e, "response", "id")
	if d.cancelTarget == "" || id != d.cancelTarget || id != d.audioResponse || !d.responseDone[id] || dsString(e, "response", "status") != "cancelled" {
		return errors.New("interrupt_not_observed")
	}
	return nil
}

func dsPublicJPEG() []byte {
	im := image.NewRGBA(image.Rect(0, 0, 320, 240))
	for y := 0; y < 240; y++ {
		for x := 0; x < 320; x++ {
			c := color.RGBA{255, 255, 255, 255}
			if (x-80)*(x-80)+(y-120)*(y-120) < 40*40 {
				c = color.RGBA{255, 0, 0, 255}
			}
			if x >= 190 && x < 270 && y >= 80 && y < 160 {
				c = color.RGBA{0, 0, 255, 255}
			}
			im.SetRGBA(x, y, c)
		}
	}
	var b bytes.Buffer
	_ = jpeg.Encode(&b, im, &jpeg.Options{Quality: 80})
	return b.Bytes()
}

func dsMap(e map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		e, _ = e[k].(map[string]any)
	}
	return e
}
func dsString(e map[string]any, keys ...string) string {
	if len(keys) == 0 {
		return ""
	}
	s, _ := dsMap(e, keys[:len(keys)-1]...)[keys[len(keys)-1]].(string)
	return s
}
