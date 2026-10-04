package smoke_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/transport/ws"
)

// 将review的overlay反例固定为本地TCP验收；script只描述独立对端的字面消息。
func inferenceReviewCapture(t *testing.T, slot int, script func(*ws.Conn) error) (inferenceRecordingConfig, inferenceRecording, error) {
	t.Helper()
	cfg := inferenceOfflineConfig(t, slot)
	if err := inferenceReserve(cfg); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := ws.Accept(w, r, ws.AcceptOptions{MaxPayload: 1 << 20, Idle: 15 * time.Second})
		if err != nil {
			done <- err
			return
		}
		finish, err := c.ArmCloseDeadline(time.Now().Add(18 * time.Second))
		if err != nil {
			done <- err
			return
		}
		defer finish()
		scriptErr := script(c)
		finish()
		done <- scriptErr
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r, err := inferenceCaptureWithDial(ctx, cfg, func(ctx context.Context, _ string, opts ws.DialOptions) (*ws.Conn, *http.Response, error) {
		return ws.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), opts)
	})
	select {
	case e := <-done:
		if e != nil {
			t.Error(e)
		}
	case <-time.After(time.Second):
		t.Fatal("server未join")
	}
	return cfg, r, err
}

func TestInferenceRecorderOfflineReviewTail(t *testing.T) {
	t.Run("失败尾部绑定实际第二task", func(t *testing.T) {
		cfg, r, err := inferenceReviewCapture(t, 1, func(c *ws.Conn) error {
			read := func(want string) error {
				_, b, e := c.ReadMessage()
				if e != nil {
					return e
				}
				if !bytes.Contains(b, []byte(want)) {
					return errors.New("独立脚本收到错输入")
				}
				return nil
			}
			if err := read(`"task_id":"s3-1-1"`); err != nil {
				return err
			}
			if err := c.WriteMessage(ws.OpText, []byte(`{"header":{"event":"task-started","task_id":"s3-1-1"},"payload":{}}`)); err != nil {
				return err
			}
			if op, _, err := c.ReadMessage(); err != nil || op != ws.OpBinary {
				return errors.New("需要第一task音频")
			}
			if err := read(`"action":"finish-task"`); err != nil {
				return err
			}
			if err := c.WriteMessage(ws.OpText, []byte(`{"header":{"event":"task-finished","task_id":"s3-1-1"},"payload":{"usage":{"duration":0.25}}}`)); err != nil {
				return err
			}
			if err := read(`"task_id":"s3-1-2"`); err != nil {
				return err
			}
			for _, raw := range []string{`{"header":{"event":"task-failed","task_id":"s3-1-2"},"payload":{"usage":{"duration":0.1}}}`, `{"header":{"event":"late","task_id":"s3-1-2","streaming":"duplex"},"payload":{"model":"qwen-audio-3.0-asr-flash-streaming","task":"asr"}}`} {
				if err := c.WriteMessage(ws.OpText, []byte(raw)); err != nil {
					return err
				}
			}
			return c.Close(4003, "peer")
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := inferenceCandidate(r); err != nil {
			t.Fatal(err)
		}
		if err := inferenceSave(cfg.Output, r); err != nil {
			t.Fatal(err)
		}
	})
	for _, tc := range []struct {
		name, tail string
		valid      bool
	}{
		{"组合绑定", `{"header":{"event":"late","task_id":"s3-6-1","streaming":"out"},"payload":{"task":"asr","model":"other-model"}}`, false},
		{"model", `{"header":{"event":"late","task_id":"s3-6-1"},"payload":{"model":"other-model"}}`, false},
		{"task", `{"header":{"event":"late","task_id":"s3-6-1"},"payload":{"task":"asr"}}`, false},
		{"streaming", `{"header":{"event":"late","task_id":"s3-6-1","streaming":"out"},"payload":{}}`, false},
		{"failed回退", `{"header":{"event":"task-failed","task_id":"s3-6-1"},"payload":{"usage":{"characters":3}}}`, false},
		{"failed增长冲突", `{"header":{"event":"task-failed","task_id":"s3-6-1"},"payload":{"usage":{"characters":7}}}`, false},
		{"failed缺失冲突", `{"header":{"event":"task-failed","task_id":"s3-6-1"},"payload":{"usage":null}}`, false},
		{"failed同快照", `{"header":{"event":"task-failed","task_id":"s3-6-1","streaming":"duplex"},"payload":{"model":"qwen-audio-3.0-tts-flash","task":"tts","usage":{"characters":6}}}`, true},
		{"匹配未知尾部", `{"header":{"event":"late","task_id":"s3-6-1","streaming":"duplex"},"payload":{"task":"tts","model":"qwen-audio-3.0-tts-flash"}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, r, err := inferenceReviewCapture(t, 6, func(c *ws.Conn) error {
				if _, _, err := c.ReadMessage(); err != nil {
					return err
				}
				for _, b := range []string{`{"header":{"event":"task-failed","task_id":"s3-6-1","error_code":"InvalidParameter"},"payload":{"usage":{"characters":6}}}`, tc.tail} {
					if err := c.WriteMessage(ws.OpText, []byte(b)); err != nil {
						return err
					}
				}
				return c.Close(4003, "peer")
			})
			if err != nil {
				t.Fatal(err)
			}
			_, candidateErr := inferenceCandidate(r)
			if (candidateErr == nil) != tc.valid {
				t.Errorf("candidate valid=%v want=%v", candidateErr == nil, tc.valid)
			}
			if err := inferenceSave(cfg.Output, r); err != nil {
				t.Fatal(err)
			}
			_, err = os.Stat(filepath.Join(cfg.Output, "candidate.json"))
			if (err == nil) != tc.valid {
				t.Errorf("save生成候选=%v want=%v", err == nil, tc.valid)
			}
			if !tc.valid {
				if _, err := os.Stat(filepath.Join(cfg.Output, "candidate-unavailable.txt")); err != nil {
					t.Error("缺少候选不可用声明")
				}
			}
			found := false
			for _, rec := range r.Records {
				if bytes.Equal(rec.Payload, []byte(tc.tail)) {
					found = true
				}
			}
			if !found {
				t.Fatal("raw尾部被删除")
			}
		})
	}
}

func TestInferenceRecorderOfflineReviewClose(t *testing.T) {
	_, r, err := inferenceReviewCapture(t, 3, func(c *ws.Conn) error {
		if _, _, err := c.ReadMessage(); err != nil {
			return err
		}
		if err := c.WriteMessage(ws.OpText, []byte(`{"header":{"event":"task-started","task_id":"s3-3-1"},"payload":{}}`)); err != nil {
			return err
		}
		for i := 0; i < 1021; i++ {
			if err := c.WriteMessage(ws.OpText, []byte(`{"header":{"event":"progress","task_id":"s3-3-1"},"payload":{}}`)); err != nil {
				return err
			}
		}
		if err := c.WriteMessage(ws.OpText, []byte(`{"header":{"event":"task-finished","task_id":"s3-3-1"},"payload":{"usage":{"characters":15}}}`)); err != nil {
			return err
		}
		_, _, err := c.ReadMessage()
		var ce *ws.CloseError
		if errors.As(err, &ce) {
			defer ce.Release()
			return fmt.Errorf("peer在1024记录满后仍收到额外close %d", ce.Code)
		}
		if err == nil {
			return errors.New("满预算后还有应用消息")
		}
		return nil
	})
	if err == nil || r.Failure == "" || len(r.Records) != 1024 {
		t.Fatalf("records=%d failure=%s err=%v", len(r.Records), r.Failure, err)
	}
	if !errors.Is(err, errInferenceCloseBudget) || r.Failure != "close_budget_exhausted" || r.TransportEnd != "local_budget_abort" {
		t.Fatalf("没有明确预算失败: %s/%s %v", r.Failure, r.TransportEnd, err)
	}
	for _, rec := range r.Records {
		if rec.Direction == "send" && rec.Opcode == ws.OpClose {
			t.Fatal("将预留虚构成已发送close")
		}
	}
}

func TestInferenceRecorderOfflineReviewCloseEncodedBudget(t *testing.T) {
	for _, r := range []inferenceRecording{
		{encodedBytes: inferenceTraceLimit - 512},
		{rawBytes: inferenceTraceLimit - 122},
		{Records: make([]inferenceRecord, 1023)},
	} {
		if err := r.checkCloseBudget(); !errors.Is(err, errInferenceCloseBudget) {
			t.Fatal("未预留本地和peer关闭两份证据")
		}
	}
	r := inferenceRecording{Records: make([]inferenceRecord, 1022), encodedBytes: inferenceTraceLimit - (1024 + 123*6), rawBytes: inferenceTraceLimit - 123}
	if err := r.checkCloseBudget(); err != nil {
		t.Fatal(err)
	}
	if len(r.Records) != 1022 {
		t.Fatal("预检被当成已发送")
	}
	// 已发本地close后，普通尾消息也不能吃掉唯一peer-close预留。
	r = inferenceRecording{Records: make([]inferenceRecord, 1023), awaitingPeer: true}
	if err := r.add(inferenceRecord{Direction: "receive", Opcode: ws.OpText, Payload: []byte(`{"header":{},"payload":{}}`)}, ""); err == nil {
		t.Fatal("尾消息挤占peer关闭额度")
	}
	if err := r.add(inferenceRecord{Direction: "receive", Opcode: ws.OpClose, CloseCode: 1000, PeerClose: true}, ""); err != nil {
		t.Fatal(err)
	}
}
