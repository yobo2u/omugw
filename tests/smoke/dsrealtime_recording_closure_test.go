//go:build smoke

package smoke_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/transport/ws"
)

// 第三次失败实录的业务形状；动态 ID 简化，音频缩为合成 PCM，关闭完全由本地 TCP 产生。
// 完整 response.done 保留双单位、空 conversation_id/transcript 与 output 嵌套形状。
const dsObservedTTSDone = `{"event_id":"event_AZziwzQFSgjCYbhrArhxl","type":"response.done","response":{"id":"r1","object":"realtime.response","conversation_id":"","status":"completed","modalities":["text","audio"],"voice":"Cherry","output":[{"id":"i1","object":"realtime.item","type":"message","status":"completed","role":"assistant","content":[{"type":"audio","transcript":""}]}],"usage":{"characters":25,"total_tokens":40,"input_tokens":8,"output_tokens":32,"input_tokens_details":{"text_tokens":8},"output_tokens_details":{"text_tokens":0,"audio_tokens":32}}}}`

func TestDSRealtimeRecorderOfflineTTSClosure(t *testing.T) {
	for _, mode := range []string{"client_close", "peer_close", "raw_eof", "no_reply", "late_message", "abnormal_close"} {
		t.Run(mode, func(t *testing.T) {
			done := make(chan error, 1)
			release := make(chan struct{})
			businessFinished := make(chan time.Time, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				wire := &dsCloseHijacker{ResponseWriter: w}
				c, err := ws.Accept(wire, req, ws.AcceptOptions{MaxPayload: dsMaxMessage, Idle: 3 * time.Second, WriteTimeout: time.Second})
				if err != nil {
					done <- err
					return
				}
				defer c.Close(1000, "")
				p := dsOfflinePeer{t: t, c: c}
				p.write(dsObservedTTSCreated)
				p.read("session.update")
				p.write(dsObservedTTSUpdated)
				p.read("input_text_buffer.append")
				p.read("input_text_buffer.commit")
				p.write(`{"type":"input_text_buffer.committed","item_id":""}`)
				p.write(`{"type":"response.created","response":{"id":"r1","object":"realtime.response","conversation_id":"","status":"in_progress","voice":"Cherry","output":[]}}`)
				p.write(`{"type":"response.output_item.added","response_id":"r1","output_index":0,"item":{"id":"i1","object":"realtime.item","type":"message","status":"in_progress","role":"assistant","content":[]}}`)
				p.write(`{"type":"response.content_part.added","response_id":"r1","item_id":"i1","output_index":0,"content_index":0,"part":{"type":"audio","text":""}}`)
				for i := 0; i < 8; i++ {
					p.write(`{"type":"response.audio.delta","response_id":"r1","item_id":"i1","output_index":0,"content_index":0,"delta":"AAAAAA=="}`)
				}
				p.write(`{"type":"response.audio.done","response_id":"r1","item_id":"i1","output_index":0,"content_index":0}`)
				p.write(`{"type":"response.content_part.done","response_id":"r1","item_id":"i1","output_index":0,"content_index":0,"part":{"type":"audio","text":""}}`)
				p.write(`{"type":"response.output_item.done","response_id":"r1","output_index":0,"item":{"id":"i1","object":"realtime.item","type":"message","status":"completed","role":"assistant","output":{"usage":{"input_tokens":8,"output_tokens":32,"total_tokens":40,"input_tokens_details":{"text_tokens":8},"output_tokens_details":{"audio_tokens":32,"text_tokens":0}}}}}`)
				p.write(dsObservedTTSDone)
				p.read("session.finish")
				p.write(`{"event_id":"event_ZAPsdN5wyqym4yAvGURU3","type":"session.finished"}`)
				businessFinished <- time.Now()
				if p.err == nil {
					switch mode {
					case "raw_eof":
						_ = c.Close(1006, "")
					case "no_reply":
						<-release
					case "peer_close":
						p.err = c.Close(1000, "peer-finished")
					case "late_message":
						p.write(`{"type":"future.late"}`)
					case "abnormal_close":
						p.err = c.Close(1011, "offline-abnormal")
					default:
						// 裸读物理帧，避免服务端 Conn 的自动回应掩盖客户端的第二帧。
						f, err := ws.ReadFrame(wire.rw.Reader, 125)
						if err != nil || f.Opcode != ws.OpClose || string(f.Payload) != "\x03\xe8" {
							p.err = errors.New("客户端没有主动发送真实正常 close")
						} else if err := ws.WriteFrame(wire.conn, ws.Frame{FIN: true, Opcode: ws.OpClose, Payload: ws.EncodeClosePayload(1000, "")}, false); err != nil {
							p.err = err
						} else if _, err := ws.ReadFrame(wire.rw.Reader, 125); err == nil {
							p.err = errors.New("录制器物理发送了重复 close 或发送后数据")
						}
					}
				}
				done <- p.err
			}))
			defer server.Close()
			start := time.Now()
			r := dsCapture(context.Background(), dsConfig{URL: "ws" + strings.TrimPrefix(server.URL, "http"), Model: "qwen3-tts-flash-realtime", Scenario: "tts-commit", Key: "offline-secret", Duration: 2500 * time.Millisecond, Responses: 1})
			elapsed := time.Since(start)
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			success := mode == "client_close" || mode == "peer_close"
			if (r.Failure == "") != success {
				t.Fatalf("mode=%s failure=%q elapsed=%v", mode, r.Failure, elapsed)
			}
			if elapsed > 2*time.Second {
				t.Errorf("业务已结束却等到整个会话期限：%v", elapsed)
			}
			closeElapsed := time.Since(<-businessFinished)
			if closeElapsed > time.Second+100*time.Millisecond {
				t.Errorf("close+capture worker join 重新续期：%v", closeElapsed)
			}
			t.Logf("close+join=%v（共同预算1秒，断言另留100ms调度容差）", closeElapsed)
			f, err := dsCandidate(r)
			if !success {
				if err == nil {
					t.Fatal("失败收尾生成了成功候选")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var sent, received int
			for _, rec := range r.Records {
				if rec.Kind == "close" {
					if rec.Direction == "send" {
						sent++
					} else {
						received++
					}
				}
			}
			if received != 1 || sent > 1 || mode == "client_close" && sent != 1 {
				t.Fatalf("重复/缺失关闭记录：send=%d receive=%d", sent, received)
			}
			if len(r.Audio) != 32 || f.Response.WS.Outcome.Kind != "completed" {
				t.Fatal("实录业务形状未完整处理")
			}
			for _, n := range f.Response.WS.Nodes {
				if n.Message != nil && strings.Contains(string(n.Message.Payload), `"type":"response.done"`) && string(n.Message.Payload) != dsObservedTTSDone {
					t.Fatal("候选改写了双单位终态字节")
				}
			}
			dsReplayCandidate(t, f)
		})
	}
}

type dsCloseHijacker struct {
	http.ResponseWriter
	conn net.Conn
	rw   *bufio.ReadWriter
}

func (w *dsCloseHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, rw, err := w.ResponseWriter.(http.Hijacker).Hijack()
	w.conn, w.rw = c, rw
	return c, rw, err
}

func TestDSRealtimeRecorderOfflineAlreadyClosedHandshake(t *testing.T) {
	// 先在真实 TCP 上完成自动回应，再调用主动关闭；ErrClosed 不能掩盖已收到的 close，
	// 也不能凭自动回应的惯常行为补写一条 send。
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	peer, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	_ = peer.SetDeadline(time.Now().Add(2 * time.Second))
	c := ws.NewConn(client, ws.RoleClient, dsMaxMessage, time.Second)
	defer c.Close(1000, "")
	if err := ws.WriteFrame(peer, ws.Frame{FIN: true, Opcode: ws.OpClose, Payload: ws.EncodeClosePayload(1000, "peer-original")}, false); err != nil {
		t.Fatal(err)
	}
	op, b, readErr := c.ReadMessage()
	f, err := ws.ReadFrame(peer, 125)
	if err != nil || f.Opcode != ws.OpClose {
		t.Fatalf("没有自动回应：%v", err)
	}
	in := make(chan dsIncoming, 1)
	in <- dsIncoming{op, b, readErr}
	r := dsRecording{}
	d := dsDriver{ctx: context.Background(), c: c, in: in, r: &r}
	if err := d.closeHandshake(); err != nil {
		t.Fatal(err)
	}
	if len(r.Records) != 1 || r.Records[0].Direction != "receive" || r.Records[0].CloseReason != "peer-original" {
		b, _ := json.Marshal(r.Records)
		t.Fatalf("已关闭 socket 补造/改写记录：%s", b)
	}
	if _, err := ws.ReadFrame(peer, 125); err == nil {
		t.Fatal("幂等收尾重复写了帧")
	}
}

// 保留真实 TCP 读侧，只卡住本端写入：复现主动写持锁时对端 close 导致 transport 中止写。
type dsCloseRaceConn struct {
	net.Conn
	writing chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (c *dsCloseRaceConn) Write([]byte) (int, error) {
	close(c.writing)
	<-c.closed
	return 0, net.ErrClosed
}

func (c *dsCloseRaceConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func TestDSRealtimeRecorderOfflineCloseWriteLosesRace(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	raw, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	peer, err := listener.Accept()
	if err != nil {
		raw.Close()
		t.Fatal(err)
	}
	defer peer.Close()
	gate := &dsCloseRaceConn{Conn: raw, writing: make(chan struct{}), closed: make(chan struct{})}
	defer gate.Close()
	c := ws.NewConn(gate, ws.RoleClient, dsMaxMessage, time.Second)
	in := make(chan dsIncoming, 1)
	readerDone := make(chan struct{})
	go func() { defer close(readerDone); op, b, err := c.ReadMessage(); in <- dsIncoming{op, b, err} }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r := dsRecording{}
	d := dsDriver{ctx: ctx, c: c, r: &r, in: in}
	done := make(chan error, 1)
	go func() { done <- d.closeHandshake() }()
	select {
	case <-gate.writing:
	case <-ctx.Done():
		t.Fatal("主动写未开始")
	}
	if err := ws.WriteFrame(peer, ws.Frame{FIN: true, Opcode: ws.OpClose, Payload: ws.EncodeClosePayload(1000, "independent-peer")}, false); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Error(err)
	}
	<-readerDone
	if len(r.Records) != 1 || r.Records[0].Direction != "receive" || r.Records[0].CloseReason != "independent-peer" {
		t.Fatalf("竞争丢失了实际 receive 或补造 send：%+v", r.Records)
	}
}

func TestDSRealtimeRecorderOfflineCloseSharesParentDeadline(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	raw, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	peer, err := listener.Accept()
	if err != nil {
		raw.Close()
		t.Fatal(err)
	}
	defer peer.Close()
	gate := &dsCloseRaceConn{Conn: raw, writing: make(chan struct{}), closed: make(chan struct{})}
	defer gate.Close()
	c := ws.NewConn(gate, ws.RoleClient, dsMaxMessage, 0)
	in := make(chan dsIncoming, 1)
	readerDone := make(chan struct{})
	go func() { defer close(readerDone); op, b, err := c.ReadMessage(); in <- dsIncoming{op, b, err} }()
	deadline := time.Now().Add(120 * time.Millisecond)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	r := dsRecording{}
	d := dsDriver{ctx: ctx, c: c, in: in, r: &r}
	done := make(chan error, 1)
	go func() { done <- d.closeHandshake() }()
	<-gate.writing
	timer := time.NewTimer(time.Until(deadline.Add(100 * time.Millisecond)))
	defer timer.Stop()
	select {
	case err := <-done:
		if err == nil {
			t.Error("慢写没有实际 peer close 却成功")
		}
	case <-timer.C:
		t.Error("父期限不足一秒，close 却重新获得一秒")
		_ = gate.Close()
		<-done
	}
	<-readerDone
	if len(r.Records) != 0 {
		t.Fatal("未实际发送或接收却补造 close 记录", r.Records)
	}
	if f, err := ws.ReadFrame(peer, 125); err == nil {
		t.Fatal("超时后仍补发物理帧", f)
	}
}
