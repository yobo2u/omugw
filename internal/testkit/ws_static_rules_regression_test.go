package testkit

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/transport/ws"
)

// 加载器必须先拒绝不可能执行的规则，不能等发送或消费者匹配才发现契约漂移。
func TestWSStaticRulesRejectBeforeIO(t *testing.T) {
	for _, mode := range []string{"content_bind", "root_bind", "ancestor_overlap", "send_id_collision", "send_equal_mismatch"} {
		t.Run(mode, func(t *testing.T) {
			f := wsReplayFixture(WSClientSend, "completed", 1000)
			n := &f.Response.WS.Nodes[0]
			switch mode {
			case "content_bind":
				n.Message.Payload = []byte(`{"text":"fixed"}`)
				n.Fields = []WSFieldRule{wsMatchID("/text", "bind", "task", "a")}
			case "root_bind":
				n.Message.Payload = []byte(`"fixed"`)
				n.Fields = []WSFieldRule{wsMatchID("", "bind", "task", "a")}
			case "ancestor_overlap":
				n.Message.Payload = []byte(`{"session":{"model":"logical"}}`)
				n.Fields = []WSFieldRule{{Pointer: "/session", Mode: "equal", Value: json.RawMessage(`{"model":"logical"}`)}, {Pointer: "/session/model", Mode: "equal", Value: json.RawMessage(`"logical"`)}}
			case "send_id_collision":
				n.Message.Payload = []byte(`{"id":"same"}`)
				n.Fields = []WSFieldRule{wsMatchID("/id", "bind", "task", "a")}
				f.Response.WS.Nodes[2].Fields = []WSFieldRule{wsMatchID("/id", "bind", "task", "b")}
				f.Response.WS.Nodes[2].Message.Payload = []byte(`{"id":"same","state":"done"}`)
			case "send_equal_mismatch":
				n.Fields = []WSFieldRule{{Pointer: "/model", Mode: "equal", Value: json.RawMessage(`"other-model"`)}}
			}
			raw, err := json.Marshal(f)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ReadWSFixture(writeWSInput(t, t.TempDir(), "fixture.json", raw), DefaultWSLimits()); err == nil {
				t.Error("loader 接纳了不可执行字段规则")
			}
			if err := ValidateWSSession(f, DefaultWSLimits()); err == nil {
				t.Error("validator 接纳了不可执行字段规则")
			}
			if u, err := NewWSReplayUpstream(f, DefaultWSLimits()); err == nil {
				_ = u.Close()
				t.Error("上游构造器接纳了不可执行字段规则")
			}
			// 双端真实 TCP 检查初始化失败不启动 writer，不能先写出负载再拒绝。
			c, cr := wsReplayTCPPair(t)
			u, ur := wsReplayTCPPair(t)
			defer c.Close()
			defer cr.Close()
			defer u.Close()
			defer ur.Close()
			limits := DefaultWSLimits()
			limits.Replay = 100 * time.Millisecond
			peers := WSReplayEndpoints{Client: ws.NewConn(c, ws.RoleClient, limits.MessageBytes, 0), Upstream: ws.NewConn(u, ws.RoleServer, limits.MessageBytes, 0)}
			if _, err := ReplayWS(context.Background(), f, peers, 1, limits); err == nil || !strings.Contains(err.Error(), "fixture") {
				t.Error("回放没有在初始化时拒绝非法 fixture")
			}
			for _, remote := range []net.Conn{cr, ur} {
				_ = remote.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
				var b [1]byte
				n, err := remote.Read(b[:])
				var timeout net.Error
				if n != 0 || (!errors.As(err, &timeout) || !timeout.Timeout()) {
					t.Error("非法 fixture 在拒绝前已经写 TCP 或接管 socket")
				}
			}
		})
	}
}

// 根 equal、接收占位与隔离命名空间不能被发送字面冲突检查误伤。
func TestWSStaticRulesLoaderPositiveBoundaries(t *testing.T) {
	for _, mode := range []string{"root_equal", "receive_equal", "send_equal", "namespace_isolation", "receive_placeholders"} {
		t.Run(mode, func(t *testing.T) {
			f := wsReplayFixture(WSClientSend, "completed", 1000)
			n := &f.Response.WS.Nodes[0]
			switch mode {
			case "root_equal":
				n.Fields = []WSFieldRule{{Pointer: "", Mode: "equal", Value: json.RawMessage(n.Message.Payload)}}
			case "receive_equal":
				f.Response.WS.Nodes[1].Fields = []WSFieldRule{{Pointer: "/model", Mode: "equal", Value: json.RawMessage(`"other-model"`)}}
			case "send_equal":
				n.Fields = []WSFieldRule{{Pointer: "/model", Mode: "equal", Value: json.RawMessage(`"synthetic-model"`)}}
			case "namespace_isolation":
				n.Message.Payload = []byte(`{"id":"same"}`)
				n.Fields = []WSFieldRule{wsMatchID("/id", "bind", "client", "a")}
				f.Response.WS.Nodes[2].Message.Payload = []byte(`{"id":"same","state":"done"}`)
				f.Response.WS.Nodes[2].Fields = []WSFieldRule{wsMatchID("/id", "bind", "upstream", "b")}
			case "receive_placeholders":
				for _, i := range []int{1, 3} {
					f.Response.WS.Nodes[i].Fields = []WSFieldRule{wsMatchID("/id", "bind", "task", f.Response.WS.Nodes[i].ID)}
					f.Response.WS.Nodes[i].Message.Payload = []byte(`{"id":"placeholder","state":"done"}`)
				}
				f.Response.WS.Outcome.Terminal[0].Namespace = "task"
				f.Response.WS.Outcome.Terminal[0].Symbol = "c-receive"
			}
			raw, err := json.Marshal(f)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ReadWSFixture(writeWSInput(t, t.TempDir(), "fixture.json", raw), DefaultWSLimits()); err != nil {
				t.Fatalf("合法边界未通过专用 loader: %v", err)
			}
		})
	}
}
