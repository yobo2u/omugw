package ws

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"
)

// 控制帧可插在分片中；关闭必须保留未完成证据，但不改关闭码、文本、自动回应或释放。
func TestConnCloseIncompleteMessageEvidence(t *testing.T) {
	for _, role := range []Role{RoleClient, RoleServer} {
		for _, op := range []Opcode{OpText, OpBinary} {
			for _, empty := range []bool{false, true} {
				name := "client"
				if role == RoleServer {
					name = "server"
				}
				if op == OpText {
					name += "/text"
				} else {
					name += "/binary"
				}
				if empty {
					name += "/empty"
				}
				t.Run(name, func(t *testing.T) {
					a, b := tcpPair(t)
					_ = a.SetDeadline(time.Now().Add(time.Second))
					_ = b.SetDeadline(time.Now().Add(time.Second))
					conn := NewConn(a, role, 1024, 0)
					payload := []byte("partial")
					if empty {
						payload = nil
					}
					for _, f := range []Frame{{FIN: false, Opcode: op, Payload: payload}, {FIN: true, Opcode: OpPing, Payload: []byte("ping")}, {FIN: true, Opcode: OpPong, Payload: []byte("pong")}, {FIN: true, Opcode: OpClose, Payload: EncodeClosePayload(1000, "peer-reason")}} {
						if err := WriteFrame(b, f, role == RoleServer); err != nil {
							t.Fatal(err)
						}
					}
					_, _, err := conn.ReadMessage()
					var closed *CloseError
					if !errors.As(err, &closed) || closed.Code != 1000 || closed.Reason != "peer-reason" || closed.Error() != `ws: 对端关闭连接 (code=1000 reason="peer-reason")` {
						t.Fatal("关闭码、原因或既有 Error 文本改变")
					}
					if !closed.IncompleteMessage {
						t.Error("CloseError 丢失实际未完成分片证据")
					}
					pong, err := ReadFrame(b, 1024)
					if err != nil || pong.Opcode != OpPong || !bytes.Equal(pong.Payload, []byte("ping")) {
						t.Fatal("分片期间 ping 未自动回应")
					}
					reply, err := ReadFrame(b, 1024)
					if err != nil || reply.Opcode != OpClose || !bytes.Equal(reply.Payload, EncodeClosePayload(1000, "")) {
						t.Fatal("未完成分片改变了自动关闭回应")
					}
					if _, err := ReadFrame(b, 1024); !errors.Is(err, io.EOF) {
						t.Fatal("关闭没有释放 TCP")
					}
				})
			}
		}
	}
}

// 完整合法分片夹杂心跳后不再有 partial；无分片关闭同样不能捏造未完成证据。
func TestConnCompleteFragmentsBeforeClose(t *testing.T) {
	for _, role := range []Role{RoleClient, RoleServer} {
		for _, op := range []Opcode{OpText, OpBinary} {
			t.Run(fmt.Sprintf("role-%d/opcode-%d", role, op), func(t *testing.T) {
				a, b := tcpPair(t)
				_ = a.SetDeadline(time.Now().Add(time.Second))
				_ = b.SetDeadline(time.Now().Add(time.Second))
				conn := NewConn(a, role, 1024, 0)
				for _, f := range []Frame{{FIN: false, Opcode: op, Payload: []byte("hel")}, {FIN: true, Opcode: OpPing, Payload: []byte("heartbeat")}, {FIN: true, Opcode: OpPong}, {FIN: true, Opcode: OpContinuation, Payload: []byte("lo")}, {FIN: true, Opcode: OpClose, Payload: EncodeClosePayload(1000, "")}} {
					if err := WriteFrame(b, f, role == RoleServer); err != nil {
						t.Fatal(err)
					}
				}
				gotOp, payload, err := conn.ReadMessage()
				if err != nil || gotOp != op || string(payload) != "hello" {
					t.Fatal("合法分片被心跳打断")
				}
				_, _, err = conn.ReadMessage()
				var closed *CloseError
				if !errors.As(err, &closed) || closed.Code != 1000 {
					t.Fatal("完整消息后正常关闭失败")
				}
				if closed.IncompleteMessage {
					t.Fatal("完整消息仍有未完成证据")
				}
			})
		}
	}
	for _, role := range []Role{RoleClient, RoleServer} {
		a, b := tcpPair(t)
		_ = a.SetDeadline(time.Now().Add(time.Second))
		if err := WriteFrame(b, Frame{FIN: true, Opcode: OpClose, Payload: EncodeClosePayload(1000, "")}, role == RoleServer); err != nil {
			t.Fatal(err)
		}
		_, _, err := NewConn(a, role, 1024, 0).ReadMessage()
		var closed *CloseError
		if !errors.As(err, &closed) {
			t.Fatal("无 partial close 失败")
		}
		if closed.IncompleteMessage {
			t.Fatal("无分片 close 捏造未完成证据")
		}
	}
}
