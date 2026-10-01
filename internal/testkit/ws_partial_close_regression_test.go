package testkit

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/transport/ws"
)

// 正常关闭不能让桥额外发送的半条 text/binary 从完整轨迹证据中消失。
func TestWSReplayRejectsPartialBeforeNormalClose(t *testing.T) {
	for _, side := range []WSPoint{WSClientSend, WSUpstreamSend} {
		for _, op := range []ws.Opcode{ws.OpText, ws.OpBinary} {
			name := string(side) + "/text"
			if op == ws.OpBinary {
				name = string(side) + "/binary"
			}
			t.Run(name, func(t *testing.T) {
				f := wsReplayFixture(side, "completed", 1000)
				limits := DefaultWSLimits()
				limits.Replay = time.Second
				c, down := wsReplayTCPPair(t)
				u, up := wsReplayTCPPair(t)
				defer c.Close()
				defer down.Close()
				defer u.Close()
				defer up.Close()
				client := ws.NewConn(c, ws.RoleClient, limits.MessageBytes, 0)
				upstream := ws.NewConn(u, ws.RoleServer, limits.MessageBytes, 0)
				bridgeDown := ws.NewConn(down, ws.RoleServer, limits.MessageBytes, 0)
				bridgeUp := ws.NewConn(up, ws.RoleClient, limits.MessageBytes, 0)
				var workers sync.WaitGroup
				workers.Add(2)
				injected := make(chan error, 1)
				forward := func(from, to *ws.Conn, raw net.Conn, point WSPoint) {
					defer workers.Done()
					for {
						msgOp, payload, err := from.ReadMessage()
						if err != nil {
							var closed *ws.CloseError
							if errors.As(err, &closed) && point == side {
								err = ws.WriteFrame(raw, ws.Frame{FIN: false, Opcode: op, Payload: []byte("synthetic-private-partial")}, point == WSClientSend)
								if err == nil {
									err = to.Close(closed.Code, closed.Reason)
								}
								injected <- err
							}
							return
						}
						if to.WriteMessage(msgOp, payload) != nil {
							return
						}
					}
				}
				go forward(bridgeDown, bridgeUp, up, WSClientSend)
				go forward(bridgeUp, bridgeDown, down, WSUpstreamSend)
				result, err := ReplayWS(context.Background(), f, WSReplayEndpoints{Client: client, Upstream: upstream}, 1, limits)
				_ = down.Close()
				_ = up.Close()
				workers.Wait()
				select {
				case injectedErr := <-injected:
					if injectedErr != nil {
						t.Fatal("实际分片与关闭注入失败")
					}
				default:
					t.Fatal("没有到达关闭注入，未覆盖 partial 边界")
				}
				if err == nil || result.Outcome.Kind != "" {
					t.Fatal("未完成额外消息被 normal close 掩盖为 completed")
				}
				if !strings.Contains(err.Error(), "ws.nodes[") || strings.Contains(err.Error(), "synthetic-private") {
					t.Fatal("失败没有安全节点路径或回显了负载")
				}
			})
		}
	}
}

// 主动端自动回应特殊分支也须拒绝真实 partial 证据，不能只检查普通 receive-close。
func TestWSReplayLocalCloseRejectsIncompleteReply(t *testing.T) {
	for _, role := range []ws.Role{ws.RoleClient, ws.RoleServer} {
		for _, op := range []ws.Opcode{ws.OpText, ws.OpBinary} {
			a, b := wsReplayTCPPair(t)
			_ = a.SetDeadline(time.Now().Add(time.Second))
			defer a.Close()
			defer b.Close()
			conn := ws.NewConn(a, role, 1024, 0)
			if err := ws.WriteFrame(b, ws.Frame{FIN: false, Opcode: op, Payload: []byte("partial")}, role == ws.RoleServer); err != nil {
				t.Fatal(err)
			}
			if err := ws.WriteFrame(b, ws.Frame{FIN: true, Opcode: ws.OpClose, Payload: ws.EncodeClosePayload(1000, "")}, role == ws.RoleServer); err != nil {
				t.Fatal(err)
			}
			_, _, received := conn.ReadMessage()
			var closed *ws.CloseError
			if !errors.As(received, &closed) || closed.Code != 1000 {
				t.Fatal("实际 partial-close 未读取")
			}
			// 只隔离 controller 的已确认本地 close 分支；错误来自真实 Reader，不伪造元数据。
			ep := &wsReplayEndpoint{localClose: true, inFlight: -1, pending: &wsReplayRead{err: received}}
			controller := &wsReplayController{endpoints: [2]*wsReplayEndpoint{ep, {inFlight: -1}}}
			if err := controller.consumePending(); err == nil || ep.readEnded {
				t.Error("主动端正常回应吞掉未完成消息")
			}
		}
	}
}
