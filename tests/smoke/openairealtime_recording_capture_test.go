package smoke_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/yobo2u/omugw/internal/transport/ws"
)

type oaRecord struct {
	Direction string    `json:"direction"`
	Kind      string    `json:"kind"`
	Opcode    ws.Opcode `json:"opcode,omitempty"`
	Payload   []byte    `json:"payload,omitempty"`
	Code      uint16    `json:"close_code,omitempty"`
	Reason    string    `json:"close_reason,omitempty"`
}
type oaRecording struct {
	Scenario  string            `json:"scenario"`
	Model     string            `json:"requested_model"`
	Started   time.Time         `json:"started_at"`
	Synthetic bool              `json:"synthetic"`
	Status    int               `json:"handshake_status"`
	Headers   map[string]string `json:"safe_response_headers,omitempty"`
	Failure   string            `json:"failure,omitempty"`
	Records   []oaRecord        `json:"records"`
	Input     *oaSample         `json:"input_sample,omitempty"`
}

func oaSafeHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	for k, v := range map[string]string{"Upgrade": "websocket", "Connection": "Upgrade"} {
		if len(h.Values(k)) == 1 && http.CanonicalHeaderKey(h.Get(k)) == http.CanonicalHeaderKey(v) {
			out[k] = v
		}
	}
	return out
}

// 预算只计算直连 send+receive 原字节，四点派生副本由 testkit 另行限额。
func (r *oaRecording) add(rec oaRecord, key string) error {
	n := len(rec.Payload)
	if rec.Kind == "close" {
		b, _ := json.Marshal(map[string]any{"reason": rec.Reason})
		if !oaSafeJSON(b, key) || rec.Code == 1005 {
			return errors.New("unsafe_close")
		}
		n = 2 + len(rec.Reason)
	} else if rec.Opcode != ws.OpText || !oaSafeJSON(rec.Payload, key) {
		return errors.New("unsafe_event")
	}
	total := n
	for _, x := range r.Records {
		if x.Kind == "close" {
			total += 2 + len(x.Reason)
		} else {
			total += len(x.Payload)
		}
	}
	if n > oaMaxBytes || total > oaMaxBytes || len(r.Records) >= oaMaxRecords {
		return errors.New("trace_limit")
	}
	r.Records = append(r.Records, rec)
	return nil
}

func oaCapture(parent context.Context, cfg oaConfig) oaRecording {
	r := oaRecording{Scenario: cfg.Scenario, Model: cfg.Model, Started: time.Now().UTC(), Synthetic: cfg.URL != oaURL, Input: cfg.Sample}
	if cfg.Sample != nil {
		copy := *cfg.Sample
		copy.File = "input.pcm"
		r.Input = &copy
	}
	if cfg.Duration <= time.Second || cfg.Duration > 60*time.Second {
		r.Failure = "invalid_duration"
		return r
	}
	total := r.Started.Add(cfg.Duration)
	ctx, cancel := context.WithDeadline(parent, total.Add(-time.Second))
	defer cancel()
	u, err := url.Parse(cfg.URL)
	if err != nil {
		r.Failure = "invalid_url"
		return r
	}
	u.RawQuery = url.Values{"model": {cfg.Model}}.Encode()
	c, resp, err := ws.Dial(ctx, u.String(), ws.DialOptions{Header: http.Header{"Authorization": {"Bearer " + cfg.Key}}, MaxPayload: oaMaxBytes, Idle: 15 * time.Second, ConnectTimeout: 5 * time.Second, WriteTimeout: time.Second, MaxHandshakeBytes: 64 << 10, MaxErrorBodyBytes: 4096})
	if resp != nil {
		r.Status = resp.StatusCode
		r.Headers = oaSafeHeaders(resp.Header)
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
	}
	if err != nil {
		r.Failure = "handshake_failed"
		return r
	}
	read := func(readCtx context.Context) (map[string]any, error) {
		m, err := c.ReadOwnedMessage(readCtx)
		if err != nil {
			var ce *ws.CloseError
			if errors.As(err, &ce) {
				defer ce.Release()
				if ce.IncompleteMessage {
					return nil, errors.New("incomplete_message")
				}
				if e := r.add(oaRecord{Direction: "receive", Kind: "close", Code: ce.Code, Reason: ce.Reason}, cfg.Key); e != nil {
					return nil, e
				}
				return nil, errors.New("peer_close")
			}
			return nil, errors.New("read_failed_or_timeout")
		}
		defer m.Release()
		b := bytes.Clone(m.Payload)
		if err := r.add(oaRecord{Direction: "receive", Kind: "message", Opcode: m.Opcode, Payload: b}, cfg.Key); err != nil {
			return nil, err
		}
		var e map[string]any
		if json.Unmarshal(b, &e) != nil {
			return nil, errors.New("invalid_event")
		}
		return e, nil
	}
	d := oaNewDriver(cfg.Scenario, cfg.Sample)
	d.recv = func() (map[string]any, error) { return read(ctx) }
	d.send = func(b []byte) error {
		if ctx.Err() != nil {
			return errors.New("timeout")
		}
		// 先在副本试算预算，只有真正写成功的消息才纳入轨迹。
		preview := r
		preview.Records = append([]oaRecord(nil), r.Records...)
		rec := oaRecord{Direction: "send", Kind: "message", Opcode: ws.OpText, Payload: b}
		if err := preview.add(rec, cfg.Key); err != nil {
			return err
		}
		if err := c.WriteMessage(ws.OpText, b); err != nil {
			return errors.New("write_failed")
		}
		r.Records = preview.Records
		return nil
	}
	if err := d.run(); err != nil {
		r.Failure = err.Error()
	}
	code := uint16(1000)
	if r.Failure != "" {
		code = 1001
	}
	deadline := minTime(time.Now().Add(time.Second), total)
	if p, ok := parent.Deadline(); ok {
		deadline = minTime(deadline, p)
	}
	sent, finish, _ := c.BeginClose(code, "", deadline)
	if sent {
		if err := r.add(oaRecord{Direction: "send", Kind: "close", Code: code}, cfg.Key); err != nil {
			r.Failure = err.Error()
		}
	}
	if r.Failure == "" {
		closeCtx, closeCancel := context.WithDeadline(parent, deadline)
		_, err := read(closeCtx)
		closeCancel()
		if err == nil || err.Error() != "peer_close" || len(r.Records) == 0 || r.Records[len(r.Records)-1].Code != 1000 {
			r.Failure = "close_not_observed"
		}
	}
	finish()
	return r
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// 失败原始轨迹优先落盘；安全拒收的消息不保存，分类说明缺口而不改写消息。
func oaSave(dir string, r oaRecording) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return errors.New("轨迹编码失败")
	}
	if err := oaWrite(dir, "recording.json", b); err != nil {
		return err
	}
	if r.Input != nil {
		if err := oaWrite(dir, "input.pcm", r.Input.Data); err != nil {
			return err
		}
	}
	if r.Failure != "" {
		return errors.New("失败轨迹已保存")
	}
	f, err := oaCandidate(r)
	if err != nil {
		return err
	}
	b, err = json.MarshalIndent(f, "", "  ")
	if err != nil {
		return errors.New("候选编码失败")
	}
	return oaWrite(dir, "candidate.json", b)
}
