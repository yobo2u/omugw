package smoke_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/yobo2u/omugw/internal/transport/ws"
)

type inferenceRecord struct {
	Direction         string
	At                time.Duration
	Opcode            ws.Opcode
	Payload           []byte
	CloseCode         uint16
	CloseReason       string
	PeerClose         bool
	SHA256            string
	IncompleteMessage bool
}

type inferenceRecording struct {
	Records                              []inferenceRecord
	Started, Ended                       time.Time
	Outcome                              string
	Slot                                 int
	Scenario, Model, Voice, SampleSHA256 string
	Sample                               []byte
	Manifest                             inferenceRecordingManifest
	Status                               int
	Synthetic                            bool
	TransportEnd, PassiveEnd             string
	FailedAt, PeerCloseAt                time.Duration
	Failure                              string
	RawSHA256                            string
	rawBytes, audioBytes, encodedBytes   int64
	stream                               *os.File
	streamed                             bool
	rawOutput                            string
	awaitingPeer                         bool
}

// Validate以json.Marshal限制完整manifest为64KiB；summary同编码且清空两个路径字段，只会缩小。
// 摘要19字段的语法开销223B（含Manifest键），9个固定ASCII/hex字符串各≤64B（含引号594B），
// 两个RFC3339Nano时间≤74B、4个整数≤80B、两个null=8B、bool≤5B：额外≤984B。
// 独立预留1KiB；路径/来源的HTML转义已计入manifest额度，不能再用缩进放大。
const inferenceMetadataLimit = inferenceManifestLimit + 1024

var errInferenceCloseBudget = errors.New("关闭证据预算不足")

func inferenceRecordUpper(rec inferenceRecord) int64 {
	return int64((len(rec.Payload)+2)/3*4 + len(rec.CloseReason)*6 + 512)
}

// 本地发送与最坏123字节peer reason各占一条；只检查额度，不伪造发送记录。
func (r *inferenceRecording) checkCloseBudget() error {
	if len(r.Records) > inferenceNodeLimit-2 || r.rawBytes > inferenceTraceLimit-123 || r.encodedBytes > inferenceTraceLimit-(512+512+123*6) {
		return errInferenceCloseBudget
	}
	return nil
}

func (r *inferenceRecording) checkRecord(rec inferenceRecord, key string) error {
	n := int64(len(rec.Payload) + len(rec.CloseReason))
	if len(rec.Payload) > inferenceMessageLimit || len(r.Records) >= inferenceNodeLimit || n > inferenceTraceLimit-r.rawBytes || rec.Direction != "send" && rec.Direction != "receive" || rec.At < 0 {
		return errors.New("原始消息/节点/轨迹超限或非法")
	}
	if rec.Opcode == ws.OpBinary && rec.Direction == "receive" && int64(len(rec.Payload)) > inferenceAudioLimit-r.audioBytes {
		return errors.New("响应音频超过8MiB")
	}
	if inferenceRecordUpper(rec) > inferenceTraceLimit-r.encodedBytes {
		return errors.New("原始编码轨迹超过16MiB")
	}
	if r.awaitingPeer && rec.Opcode != ws.OpClose && (len(r.Records) >= inferenceNodeLimit-1 || n > inferenceTraceLimit-r.rawBytes-123 || inferenceRecordUpper(rec) > inferenceTraceLimit-r.encodedBytes-(512+123*6)) {
		return errInferenceCloseBudget
	}
	if key != "" && (bytes.Contains(rec.Payload, []byte(key)) || bytes.Contains([]byte(rec.CloseReason), []byte(key))) {
		return errors.New("原始材料包含凭据，拒绝保存")
	}
	if rec.Opcode == ws.OpText {
		if _, err := inferenceScanJSON(rec.Payload, key, false); err != nil {
			return errors.New("原始文本安全检查失败")
		}
	}
	return nil
}

func (r *inferenceRecording) add(rec inferenceRecord, key string) error {
	if err := r.checkRecord(rec, key); err != nil {
		return err
	}
	rec.SHA256 = inferenceSHA(rec.Payload)
	// 先计编码后的保守上界，再允许JSON分配，原始文件也不能越16MiB。
	b, err := json.Marshal(rec)
	if err != nil {
		return errors.New("原始记录编码失败")
	}
	if r.stream != nil {
		if _, err := r.stream.Write(append(b, '\n')); err != nil {
			return errors.New("原始记录写入失败")
		}
		if r.stream.Sync() != nil {
			return errors.New("原始记录同步失败")
		}
	}
	r.encodedBytes += int64(len(b) + 1)
	r.rawBytes += int64(len(rec.Payload) + len(rec.CloseReason))
	if rec.Opcode == ws.OpBinary && rec.Direction == "receive" {
		r.audioBytes += int64(len(rec.Payload))
	}
	rec.Payload = bytes.Clone(rec.Payload)
	r.Records = append(r.Records, rec)
	return nil
}

type inferenceDial func(context.Context, string, ws.DialOptions) (*ws.Conn, *http.Response, error)
type inferenceReadResult struct {
	msg *ws.Message
	err error
	at  time.Duration
}

// 实际公网ws.Dial只存在smoke文件。本函数由离线测试注入本地TCP，且仍消耗持久单次dial。
func inferenceCaptureWithDial(parent context.Context, c inferenceRecordingConfig, dial inferenceDial) (r inferenceRecording, retErr error) {
	r = inferenceRecording{Started: time.Now(), Slot: c.Slot, Scenario: c.Scenario, Model: c.Model, Voice: c.Voice, SampleSHA256: c.SampleSHA256, Manifest: c.Manifest, Synthetic: c.synthetic, Outcome: "interrupted"}
	defer func() {
		r.Ended = time.Now()
		if retErr != nil && r.Failure == "" {
			r.Failure = "capture_failed"
		}
	}()
	if err := inferenceValidate(c); err != nil {
		return r, err
	}
	if !inferenceKeyValid(c.Key) {
		return r, errors.New("缺少合法凭据")
	}
	sample, err := inferenceReadFile(c.SamplePath, inferenceSampleLimit)
	if err != nil || inferenceSHA(sample) != c.SampleSHA256 {
		return r, errors.New("发送前样本身份不符")
	}
	r.Sample = sample
	dir, err := inferenceOpenDir(c.Output, false)
	if err != nil {
		return r, err
	}
	defer dir.Close()
	r.stream, err = inferenceExclusive(dir, "records.jsonl")
	if err != nil {
		return r, err
	}
	r.streamed = true
	r.rawOutput = c.Output
	defer func() {
		if r.stream.Sync() != nil && retErr == nil {
			retErr = errors.New("原始轨迹同步失败")
		}
		if r.stream.Close() != nil && retErr == nil {
			retErr = errors.New("原始轨迹关闭失败")
		}
		r.stream = nil
	}()
	if err := r.stream.Sync(); err != nil {
		return r, errors.New("raw创建同步失败")
	}
	dirFile, err := dir.Open(".")
	if err != nil {
		return r, errors.New("raw目录同步失败")
	}
	syncErr := dirFile.Sync()
	dirFile.Close()
	if syncErr != nil {
		return r, errors.New("raw目录同步失败")
	}
	if err := inferenceClaimDial(c); err != nil {
		return r, err
	}
	ctx, cancel := context.WithDeadline(parent, r.Started.Add(inferenceDuration))
	defer cancel()
	conn, resp, err := dial(ctx, c.Endpoint, ws.DialOptions{Header: http.Header{"Authorization": {"Bearer " + c.Key}}, MaxPayload: inferenceMessageLimit, MaxHandshakeBytes: 64 << 10, MaxErrorBodyBytes: 64 << 10, ConnectTimeout: 5 * time.Second, WriteTimeout: time.Second, Idle: inferenceDuration})
	// 完全不保留响应头/失败体；错误也固定化，服务端回显秘密不能进入报告。
	if resp != nil {
		r.Status = resp.StatusCode
		if resp.Body != nil {
			resp.Body.Close()
		}
	}
	if err != nil {
		r.Outcome = "handshake_failed"
		return r, errors.New("独立握手失败，槽已消耗")
	}
	deadline, _ := ctx.Deadline()
	finish, err := conn.ArmCloseDeadline(deadline)
	if err != nil {
		return r, errors.New("期限守卫失败")
	}
	defer finish()
	readCtx, stopRead := context.WithCancel(ctx)
	results := make(chan inferenceReadResult)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			m, err := conn.ReadOwnedMessage(readCtx)
			result := inferenceReadResult{m, err, time.Since(r.Started)}
			select {
			case results <- result:
			case <-readCtx.Done():
				m.Release()
				var ce *ws.CloseError
				if errors.As(err, &ce) {
					ce.Release()
				}
				return
			}
			if err != nil {
				return
			}
		}
	}()
	defer func() {
		stopRead()
		finish()
		select {
		case <-done:
		case <-time.After(time.Second):
			retErr = errors.New("reader未在强制释放后join")
		}
	}()
	readEnded := false
	receive := func(until time.Time) (inferenceRecord, error) {
		if readEnded {
			return inferenceRecord{}, io.EOF
		}
		timer := time.NewTimer(time.Until(until))
		defer timer.Stop()
		select {
		case x := <-results:
			if x.err != nil {
				readEnded = true
				var ce *ws.CloseError
				if errors.As(x.err, &ce) {
					defer ce.Release()
					rec := inferenceRecord{Direction: "receive", At: x.at, Opcode: ws.OpClose, CloseCode: ce.Code, CloseReason: ce.Reason, PeerClose: true, IncompleteMessage: ce.IncompleteMessage}
					if err := r.add(rec, c.Key); err != nil {
						retErr = errors.New("关闭证据拒绝，不能生成候选")
						return rec, err
					}
					r.TransportEnd = "peer_close"
					r.PeerCloseAt = x.at
					return rec, io.EOF
				}
				if errors.Is(x.err, io.EOF) || errors.Is(x.err, io.ErrUnexpectedEOF) {
					r.TransportEnd = "eof"
				} else if ctx.Err() != nil {
					r.TransportEnd = "deadline"
				} else {
					r.TransportEnd = "read_error"
				}
				return inferenceRecord{}, io.EOF
			}
			defer x.msg.Release()
			rec := inferenceRecord{Direction: "receive", At: x.at, Opcode: x.msg.Opcode, Payload: x.msg.Payload}
			if err := r.add(rec, c.Key); err != nil {
				retErr = errors.New("原始记录拒绝，不能裁剪成候选")
				return inferenceRecord{}, err
			}
			// 返回的别名归recording持有，不能在Message.Release后悬空。
			return r.Records[len(r.Records)-1], nil
		case <-timer.C:
			return inferenceRecord{}, context.DeadlineExceeded
		case <-ctx.Done():
			return inferenceRecord{}, ctx.Err()
		}
	}
	send := func(op ws.Opcode, b []byte) error {
		rec := inferenceRecord{Direction: "send", Opcode: op, Payload: b}
		if err := r.checkRecord(rec, c.Key); err != nil {
			return err
		}
		if int64((len(b)+2)/3*4+512) > inferenceTraceLimit-r.encodedBytes {
			return errors.New("发送前轨迹预算不足")
		}
		if err := conn.WriteMessage(op, b); err != nil {
			return errors.New("应用写失败")
		}
		rec.At = time.Since(r.Started)
		return r.add(rec, c.Key)
	}
	r.Outcome, err = inferenceDrive(c, sample, send, func() (inferenceRecord, error) { return receive(deadline) })
	if err != nil {
		r.Failure = "scenario_incomplete"
	}
	// failed后被动一秒与主动一秒共用45秒绝对期限；EOF、静默、peer各自保留。
	if r.Outcome == "failed" {
		r.FailedAt = r.Records[len(r.Records)-1].At
		passive := minInferenceTime(deadline, r.Started.Add(r.FailedAt).Add(time.Second))
		for !readEnded {
			_, e := receive(passive)
			if errors.Is(e, context.DeadlineExceeded) {
				r.PassiveEnd = "silent"
				break
			}
			if e != nil {
				break
			}
		}
		if r.PassiveEnd == "" {
			r.PassiveEnd = r.TransportEnd
		}
	}
	if !readEnded {
		if err := r.checkCloseBudget(); err != nil {
			r.Failure = "close_budget_exhausted"
			r.TransportEnd = "local_budget_abort"
			if r.Outcome == "completed" {
				r.Outcome = "interrupted"
			}
			return r, err
		}
		r.awaitingPeer = true
		closeDeadline := minInferenceTime(deadline, time.Now().Add(time.Second))
		sent, join, _ := conn.BeginClose(1000, "", closeDeadline)
		if sent {
			if e := r.add(inferenceRecord{Direction: "send", Opcode: ws.OpClose, At: time.Since(r.Started), CloseCode: 1000}, c.Key); e != nil {
				r.Failure = "close_record_failed"
				retErr = e
			}
		}
		// reader可能已接到peer close并自动封写；BeginClose失败也必须消费其独立接收证据。
		for !readEnded {
			_, e := receive(closeDeadline)
			if e != nil {
				break
			}
		}
		join()
	}
	if r.Outcome == "completed" && r.TransportEnd != "peer_close" {
		r.Outcome = "interrupted"
		r.Failure = "missing_peer_close"
	}
	if err != nil {
		return r, errors.New("场景未完成，原始材料已保留")
	}
	return r, retErr
}

func minInferenceTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// 用于离线保存时同样exclusive；capture已逐条刷盘，不能重写原文件或删事件塞入候选预算。
func inferenceSave(output string, r inferenceRecording) error {
	if r.streamed && output != r.rawOutput {
		return errors.New("不能改变已录raw的保存位置")
	}
	d, err := inferenceOpenDir(output, false)
	if err != nil {
		return err
	}
	defer d.Close()
	if !r.streamed {
		f, err := inferenceExclusive(d, "records.jsonl")
		if err != nil {
			return err
		}
		checked := inferenceRecording{stream: f}
		for _, rec := range r.Records {
			if err := checked.add(rec, ""); err != nil {
				f.Close()
				return err
			}
		}
		if f.Sync() != nil {
			f.Close()
			return errors.New("raw同步失败")
		}
		if f.Close() != nil {
			return errors.New("raw关闭失败")
		}
	}
	r.RawSHA256, err = inferenceVerifyRaw(d, r.Records)
	if err != nil {
		return err
	}
	// 元数据不含Key/Endpoint/样本路径/响应头，原始数据独立持久化。
	summary := r
	summary.Records = nil
	summary.Sample = nil
	summary.Manifest.SamplePath = ""
	summary.Manifest.Endpoint = ""
	b, err := json.Marshal(summary)
	if err != nil || len(b) > inferenceMetadataLimit {
		return errors.New("元数据超限")
	}
	if err := inferenceWrite(d, "recording.json", b); err != nil {
		return err
	}
	f, err := inferenceCandidate(r)
	if err != nil {
		if e := inferenceWrite(d, "candidate-unavailable.txt", []byte("候选未生成：证据不完整、schema不能表达或超过独立预算。原始轨迹保留，需人工审核。\n")); e != nil {
			return e
		}
		return nil
	}
	return inferenceSaveCandidate(d, filepath.Join(output, "candidate.json"), f)
}

// 与已经刷盘的逐行原文对账，防内存重写后把候选挂在另一条raw证据上。
func inferenceVerifyRaw(root *os.Root, records []inferenceRecord) (string, error) {
	bad := errors.New("原始证据文件与待保存轨迹不符")
	info, err := root.Lstat("records.jsonl")
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > inferenceTraceLimit {
		return "", bad
	}
	f, err := root.OpenFile("records.jsonl", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", bad
	}
	defer f.Close()
	now, err := f.Stat()
	if err != nil || !os.SameFile(info, now) || !now.Mode().IsRegular() {
		return "", bad
	}
	h := sha256.New()
	scan := bufio.NewScanner(io.TeeReader(io.LimitReader(f, inferenceTraceLimit+1), h))
	scan.Buffer(make([]byte, 4096), 2<<20)
	for _, rec := range records {
		if len(rec.Payload) > inferenceMessageLimit || rec.SHA256 != inferenceSHA(rec.Payload) {
			return "", bad
		}
		b, err := json.Marshal(rec)
		if err != nil || !scan.Scan() || !bytes.Equal(b, scan.Bytes()) {
			return "", bad
		}
	}
	if scan.Scan() || scan.Err() != nil {
		return "", bad
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
