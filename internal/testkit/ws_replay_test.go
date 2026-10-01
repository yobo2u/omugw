package testkit

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/transport/ws"
)

// 漏掉任一观测点、二进制重编码或把本地 EOF 当成 close 证据都必须破坏回放。
func TestWSReplayFourPoints(t *testing.T) {
	for _, side := range []WSPoint{WSClientSend, WSUpstreamSend} {
		t.Run(string(side), func(t *testing.T) {
			f := wsReplayFixture(side, "completed", 1000)
			f.Response.WS.Nodes[0].Message = &WSMessage{Opcode: ws.OpBinary, Payload: []byte{0, 255, 16}}
			f.Response.WS.Nodes[0].Match = "bytes"
			f.Response.WS.Nodes[1].Message = &WSMessage{Opcode: ws.OpBinary, Payload: []byte{0, 255, 16}}
			f.Response.WS.Nodes[1].Match = "bytes"
			peers, u := wsReplayBridge(t, f, DefaultWSLimits(), nil)
			result, err := ReplayWS(context.Background(), f, peers, 1, DefaultWSLimits())
			if err != nil {
				t.Fatalf("四点回放失败: %v", err)
			}
			if !reflect.DeepEqual(result.Nodes, f.Response.WS.Nodes) || !reflect.DeepEqual(result.Outcome, f.Response.WS.Outcome) || len(result.SendOrder) != 3 || u.Err() != nil {
				t.Fatal("四点证据、归一结果或关闭结局缺失")
			}
		})
	}
}

// 比较控制器发令顺序而非 TCP 到达顺序，防止把墙钟调度冒充可复现种子。
func TestWSReplayTwoSeeds(t *testing.T) {
	f := wsReplayFixture(WSClientSend, "completed", 1000)
	f.Response.WS.Nodes[2].After = nil
	var results [2]WSReplayResult
	for i, seed := range []uint64{1, 2} {
		peers, _ := wsReplayBridge(t, f, DefaultWSLimits(), nil)
		var err error
		results[i], err = ReplayWS(context.Background(), f, peers, seed, DefaultWSLimits())
		if err != nil {
			t.Fatalf("独立双向发令回放失败: %v", err)
		}
	}
	if reflect.DeepEqual(results[0].SendOrder, results[1].SendOrder) || !reflect.DeepEqual(results[0].Nodes, results[1].Nodes) || !reflect.DeepEqual(results[0].Outcome, results[1].Outcome) {
		t.Fatal("不同种子没有改变发令顺序或污染了按节点归一的证据")
	}
}

// 实际桥接消息独立变坏时，完整匹配不能被 fixture 自证或规范化吞掉。
func TestWSReplayDoesNotApproveBrokenBridge(t *testing.T) {
	for _, mode := range []string{"missing_field", "changed_model", "wrong_opcode", "content", "extra", "tail_extra", "missing", "wrong_id", "wrong_state", "raw_eof", "wrong_close", "duplicate_key", "missing_zero", "null_zero", "false_zero"} {
		t.Run(mode, func(t *testing.T) {
			f := wsReplayFixture(WSUpstreamSend, "completed", 1000)
			if strings.HasSuffix(mode, "_zero") {
				for _, i := range []int{0, 1} {
					f.Response.WS.Nodes[i].Message.Payload = []byte(`{"type":"command","model":"synthetic-model","count":0}`)
				}
			}
			limits := DefaultWSLimits()
			limits.Replay = 150 * time.Millisecond
			mutate := func(point WSPoint, n int, msg WSMessage) []WSMessage {
				if point == WSClientSend && n == 0 {
					switch mode {
					case "missing_field":
						msg.Payload = []byte(`{"type":"command"}`)
					case "changed_model":
						msg.Payload = []byte(`{"type":"command","model":"synthetic-private"}`)
					case "wrong_opcode":
						msg.Opcode = ws.OpBinary
					case "content":
						msg.Payload = []byte("synthetic-private")
					case "extra":
						return []WSMessage{msg, {Opcode: ws.OpText, Payload: []byte("synthetic-private")}}
					case "missing":
						return nil
					case "duplicate_key":
						msg.Payload = []byte(`{"type":"command","model":"synthetic-private","model":"synthetic-model"}`)
					case "missing_zero":
						msg.Payload = []byte(`{"type":"command","model":"synthetic-model"}`)
					case "null_zero":
						msg.Payload = []byte(`{"type":"command","model":"synthetic-model","count":null}`)
					case "false_zero":
						msg.Payload = []byte(`{"type":"command","model":"synthetic-model","count":false}`)
					}
				}
				if point == WSUpstreamSend && n == 0 {
					switch mode {
					case "wrong_id":
						msg.Payload = []byte(`{"id":"synthetic-private","state":"done"}`)
					case "wrong_state":
						msg.Payload = []byte(`{"id":"response-1","state":"synthetic-private"}`)
					case "tail_extra":
						return []WSMessage{msg, {Opcode: ws.OpText, Payload: []byte("synthetic-private")}}
					}
				}
				return []WSMessage{msg}
			}
			peers, _ := wsReplayBridgeMode(t, f, limits, mutate, mode)
			result, err := ReplayWS(context.Background(), f, peers, 1, limits)
			if err == nil || result.Outcome.Kind != "" {
				t.Fatal("坏 bridge 被批准为业务结局")
			}
			if strings.Contains(err.Error(), "synthetic-private") {
				t.Fatal("失败诊断回显了实际负载")
			}
			if mode != "missing" && mode != "raw_eof" && !strings.Contains(err.Error(), "ws.nodes[") {
				t.Fatalf("错误没有安全节点路径: %v", err)
			}
		})
	}
}

// 完成必须同时有接收业务证据与可观测 close；failed/interrupted 不能混成 completed。
func TestWSReplayTermination(t *testing.T) {
	for _, tc := range []struct {
		kind string
		code uint16
	}{{"completed", 1000}, {"failed", 1011}, {"interrupted", 1000}} {
		for _, side := range []WSPoint{WSClientSend, WSUpstreamSend} {
			t.Run(tc.kind+"/"+string(side), func(t *testing.T) {
				f := wsReplayFixture(side, tc.kind, tc.code)
				peers, _ := wsReplayBridge(t, f, DefaultWSLimits(), nil)
				result, err := ReplayWS(context.Background(), f, peers, 2, DefaultWSLimits())
				if err != nil || result.Outcome.Kind != tc.kind {
					t.Fatalf("显式传输结局未保全: %v", err)
				}
			})
		}
	}
	for _, mode := range []string{"no_terminal", "terminal_state", "terminal_id", "terminal_send", "unknown_close", "no_close", "nil_peer", "same_peer", "invalid_limit", "handshake_failed"} {
		t.Run(mode, func(t *testing.T) {
			f := wsReplayFixture(WSClientSend, "completed", 1000)
			limits := DefaultWSLimits()
			peers, _ := wsReplayBridge(t, f, limits, nil)
			switch mode {
			case "no_terminal":
				f.Response.WS.Outcome.Terminal = nil
			case "terminal_state":
				f.Response.WS.Outcome.Terminal[0].State = "other"
			case "terminal_id":
				f.Response.WS.Outcome.Terminal[0].Symbol = "other"
			case "terminal_send":
				f.Response.WS.Outcome.Terminal[0].Node = "u-send"
			case "unknown_close":
				*f.Response.WS.Nodes[4].CloseCode = 1006
			case "no_close":
				f.Response.WS.Nodes = f.Response.WS.Nodes[:4]
			case "nil_peer":
				peers.Client = nil
			case "same_peer":
				peers.Upstream = peers.Client
			case "invalid_limit":
				limits.Replay = 0
			case "handshake_failed":
				f.Response.Status = 401
				f.Response.WS.UpstreamExpectedStatus = 401
				f.Response.WS.UpstreamError = []byte(`{"error":true}`)
				f.Response.WS.Nodes = nil
				f.Response.WS.Outcome = WSOutcome{Kind: "handshake_failed"}
			}
			result, err := ReplayWS(context.Background(), f, peers, 1, limits)
			if err == nil || result.Outcome.Kind != "" {
				t.Fatal("没有完整证据仍批准结局")
			}
		})
	}
}

// 所有业务消息已到达仍须等待对侧 close，不能把主动端本地 EOF 当完整收尾。
func TestWSReplayWaitsForClose(t *testing.T) {
	for _, kind := range []string{"completed", "interrupted"} {
		t.Run(kind, func(t *testing.T) {
			f := wsReplayFixture(WSClientSend, kind, 1000)
			limits := DefaultWSLimits()
			limits.Replay = 100 * time.Millisecond
			forwarded := make(chan struct{}, 1)
			mutate := func(point WSPoint, n int, msg WSMessage) []WSMessage {
				if point == WSUpstreamSend && n == 0 {
					forwarded <- struct{}{}
				}
				return []WSMessage{msg}
			}
			peers, _ := wsReplayBridgeMode(t, f, limits, mutate, "drop_close")
			result, err := ReplayWS(context.Background(), f, peers, 1, limits)
			if !errors.Is(err, context.DeadlineExceeded) || result.Outcome.Kind != "" {
				t.Fatal("消息齐全但关闭缺失仍批准结局或没有按预算结束")
			}
			select {
			case <-forwarded:
			default:
				t.Fatal("没有到达业务回复便提前超时，未覆盖等待关闭边界")
			}
		})
	}
}

// JSON 等值不能豁免转发字节；只有显式 equal 的标量 token 可改写。
func TestWSReplayForwardedBytes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
		rewrite bool
		wantOK  bool
	}{
		{"unchanged", `{"type":"command","model":"synthetic-model"}`, false, true},
		{"key_order", `{"model":"synthetic-model","type":"command"}`, false, false},
		{"whitespace", `{ "type":"command","model":"synthetic-model"}`, false, false},
		{"declared_model", `{"type":"command","model":"upstream-model"}`, true, true},
		{"wrong_declared_model", `{"type":"command","model":"synthetic-private"}`, true, false},
		{"outside_declared_span", `{"type": "command","model":"upstream-model"}`, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := wsReplayFixture(WSClientSend, "completed", 1000)
			if tc.rewrite {
				f.Response.WS.Nodes[1].Fields = []WSFieldRule{{Pointer: "/model", Mode: "equal", Value: []byte(`"upstream-model"`)}}
			}
			mutate := func(point WSPoint, n int, msg WSMessage) []WSMessage {
				if point == WSClientSend && n == 0 {
					msg.Payload = []byte(tc.payload)
				}
				return []WSMessage{msg}
			}
			peers, _ := wsReplayBridge(t, f, DefaultWSLimits(), mutate)
			result, err := ReplayWS(context.Background(), f, peers, 1, DefaultWSLimits())
			if tc.wantOK {
				if err != nil || result.Outcome.Kind != "completed" {
					t.Fatalf("字节保全或声明改写被拒绝: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "ws.nodes[1].message.payload") || strings.Contains(err.Error(), "synthetic-private") {
				t.Fatal("未声明的字节变化得到批准或失败路径泄露实际负载")
			}
		})
	}
}

// 重复轮次和跨消息实体引用不能被每轮重置 matcher 或未确认的发送绑定掩盖。
func TestWSReplayMultiRoundBindings(t *testing.T) {
	f := wsReplayFixture(WSClientSend, "completed", 1000)
	nodes := f.Response.WS.Nodes
	nodes[2].Message = &WSMessage{Opcode: ws.OpText, Payload: []byte(`{"id":"recorded-id","state":"done"}`)}
	nodes[2].Fields = []WSFieldRule{{Pointer: "/id", Mode: "bind", Namespace: "upstream", Symbol: "answer"}}
	nodes[3].Message = &WSMessage{Opcode: ws.OpText, Payload: []byte(`{"id":"golden-id","state":"done"}`)}
	nodes[3].Fields = []WSFieldRule{{Pointer: "/id", Mode: "bind", Namespace: "client", Symbol: "answer"}}
	// 本例独立验证两命名空间动态实体，bridge 故意重写 ID，不能谎称字节透传。
	nodes[3].ForwardedFrom = ""
	f.Response.WS.Outcome.Terminal[0].Symbol = "answer"
	f.Response.WS.Outcome.Terminal[0].Namespace = "client"
	ref := WSFieldRule{Pointer: "/id", Mode: "reference", Namespace: "client", Symbol: "answer"}
	next := []WSNode{
		wsReplayMessage("c-ref", WSClientSend, ws.OpText, `{"id":"placeholder","type":"continue"}`, "json", "c-receive"),
		wsReplayMessage("u-ref", WSUpstreamReceive, ws.OpText, `{"id":"placeholder","type":"continue"}`, "json", "c-ref"),
		wsReplayMessage("u-binary", WSUpstreamSend, ws.OpBinary, "\x00\xff\x10", "bytes", "u-ref"),
		wsReplayMessage("c-binary", WSClientReceive, ws.OpBinary, "\x00\xff\x10", "bytes", "u-binary"),
	}
	next[0].Fields = []WSFieldRule{ref}
	next[1].Fields = []WSFieldRule{ref}
	nodes[4].After = []string{"c-binary"}
	f.Response.WS.Nodes = append(append(nodes[:4:4], next...), nodes[4:]...)
	for _, mode := range []string{"good", "bad_reference", "false_forwarded"} {
		t.Run(mode, func(t *testing.T) {
			mutate := func(point WSPoint, n int, msg WSMessage) []WSMessage {
				if point == WSUpstreamSend && n == 0 {
					msg.Payload = []byte(`{"id":"runtime-id","state":"done"}`)
				}
				if mode == "bad_reference" && point == WSClientSend && n == 1 {
					msg.Payload = []byte(`{"id":"wrong-id","type":"continue"}`)
				}
				return []WSMessage{msg}
			}
			if mode == "false_forwarded" {
				f.Response.WS.Nodes[3].ForwardedFrom = "u-send"
			}
			peers, _ := wsReplayBridge(t, f, DefaultWSLimits(), mutate)
			result, err := ReplayWS(context.Background(), f, peers, 1, DefaultWSLimits())
			if mode == "good" {
				if err != nil || len(result.Nodes) != 10 {
					t.Fatalf("多轮动态实体回放失败: %v", err)
				}
			} else if err == nil {
				t.Fatal("错误的跨轮引用仍通过")
			}
		})
	}
}

// 已到达的消息可能比 WriteMessage 的成功结果早，绑定不能提前提交，接收也不能误报额外。
func TestWSReplayReceiveBeforeWriteResult(t *testing.T) {
	limits := DefaultWSLimits()
	f := wsReplayFixture(WSClientSend, "completed", 1000)
	schedule, err := NewWSSchedule(f.Response.WS.Nodes, 1)
	if err != nil {
		t.Fatal("手写调度初始化失败")
	}
	matcher, err := NewWSMatcher(limits)
	if err != nil {
		t.Fatal("matcher 初始化失败")
	}
	controller := &wsReplayController{nodes: schedule.nodes, graph: schedule.graph, schedule: schedule, matcher: matcher, limits: limits,
		completed: make([]bool, 6), observed: make([]*WSMessage, 6), endpoints: [2]*wsReplayEndpoint{{inFlight: 0}, {inFlight: -1, receives: []int{1, 5}}}}
	if schedule.Start("c-send") != nil {
		t.Fatal("发送启动失败")
	}
	a, b := wsReplayTCPPair(t)
	defer a.Close()
	defer b.Close()
	client := ws.NewConn(a, ws.RoleClient, limits.MessageBytes, 0)
	upstream := ws.NewConn(b, ws.RoleServer, limits.MessageBytes, 0)
	message := *f.Response.WS.Nodes[0].Message
	if client.WriteMessage(message.Opcode, message.Payload) != nil {
		t.Fatal("实际消息发送失败")
	}
	op, payload, err := upstream.ReadMessage()
	if err != nil {
		t.Fatal("实际消息接收失败")
	}
	controller.endpoints[1].pending = &wsReplayRead{message: WSMessage{Opcode: op, Payload: payload}}
	if err := controller.consumePending(); err != nil || controller.completed[1] || controller.endpoints[1].pending == nil {
		t.Fatal("write-result 尚未提交时误报或提前消费接收")
	}
	if err := controller.finishWrite(controller.endpoints[0], wsReplayWritten{job: wsReplayWrite{index: 0, message: message}}); err != nil {
		t.Fatal("成功发送结果提交失败")
	}
	if err := controller.consumePending(); err != nil || !controller.completed[1] || controller.endpoints[1].pending != nil {
		t.Fatal("发送提交后没有消费早到的实际接收证据")
	}
}

// 实际负载即使完全正确，也不能在祖先发送尚未发令时提前匹配接收证据。
func TestWSReplayRejectsPrematureReceive(t *testing.T) {
	f := wsReplayFixture(WSClientSend, "completed", 1000)
	limits := DefaultWSLimits()
	schedule, err := NewWSSchedule(f.Response.WS.Nodes, 1)
	if err != nil {
		t.Fatal("手写调度初始化失败")
	}
	matcher, err := NewWSMatcher(limits)
	if err != nil {
		t.Fatal("matcher 初始化失败")
	}
	a, b := wsReplayTCPPair(t)
	defer a.Close()
	defer b.Close()
	client := ws.NewConn(a, ws.RoleClient, limits.MessageBytes, 0)
	server := ws.NewConn(b, ws.RoleServer, limits.MessageBytes, 0)
	message := *f.Response.WS.Nodes[3].Message
	if server.WriteMessage(message.Opcode, message.Payload) != nil {
		t.Fatal("实际早到消息发送失败")
	}
	op, payload, err := client.ReadMessage()
	if err != nil {
		t.Fatal("实际早到消息接收失败")
	}
	controller := &wsReplayController{nodes: schedule.nodes, graph: schedule.graph, schedule: schedule, matcher: matcher, limits: limits,
		completed: make([]bool, 6), observed: make([]*WSMessage, 6), endpoints: [2]*wsReplayEndpoint{
			{inFlight: -1, receives: []int{3}, pending: &wsReplayRead{message: WSMessage{Opcode: op, Payload: payload}}},
			{inFlight: -1},
		}}
	if err := controller.consumePending(); err == nil || !strings.Contains(err.Error(), "ws.nodes[3]") || controller.completed[3] {
		t.Fatal("尚未发令的祖先发送被正确负载绕过")
	}
}

// raw TCP EOF 不得因为 fixture 声明 interrupted 而被视作有效关闭。
func TestWSReplayInterruptedRejectsRawEOF(t *testing.T) {
	f := wsReplayFixture(WSClientSend, "interrupted", 1000)
	peers, _ := wsReplayBridgeMode(t, f, DefaultWSLimits(), nil, "raw_eof")
	result, err := ReplayWS(context.Background(), f, peers, 1, DefaultWSLimits())
	if err == nil || result.Outcome.Kind != "" {
		t.Fatal("raw TCP EOF 被误当 interrupted 的 close 证据")
	}
}

// 只发送关闭帧不能证明 interrupted；主动端收到异常关闭也不能冒充自动 1000。
func TestWSReplayCloseEvidence(t *testing.T) {
	t.Run("send_only_interrupted", func(t *testing.T) {
		f := wsReplayFixture(WSClientSend, "interrupted", 1000)
		f.Response.WS.Nodes = f.Response.WS.Nodes[:5]
		code := uint16(1000)
		f.Response.WS.Nodes = append(f.Response.WS.Nodes, WSNode{ID: "other-close", Point: WSUpstreamSend, Source: "synthetic", Kind: "close", After: []string{"c-receive"}, CloseCode: &code})
		peers, _ := wsReplayBridge(t, f, DefaultWSLimits(), nil)
		result, err := ReplayWS(context.Background(), f, peers, 1, DefaultWSLimits())
		if err == nil || result.Outcome.Kind != "" {
			t.Fatal("仅主动关闭、没有接收 close 证据仍批准 interrupted")
		}
	})
	t.Run("observed_abnormal_reply", func(t *testing.T) {
		// 普通主动 Close 立即关 TCP，不能保证观察到对端回应；此处只针对实际
		// 已收到的异常 close，验证 controller 不吞掉证据，不伪造完整正向轨迹。
		a, b := wsReplayTCPPair(t)
		defer a.Close()
		defer b.Close()
		conn := ws.NewConn(a, ws.RoleClient, 64, 0)
		if ws.WriteFrame(b, ws.Frame{FIN: true, Opcode: ws.OpClose, Payload: ws.EncodeClosePayload(1011, "")}, false) != nil {
			t.Fatal("本地异常关闭注入失败")
		}
		_, _, err := conn.ReadMessage()
		var closeErr *ws.CloseError
		if !errors.As(err, &closeErr) || closeErr.Code != 1011 {
			t.Fatal("未观测到实际异常 close")
		}
		controller := &wsReplayController{endpoints: [2]*wsReplayEndpoint{
			{inFlight: -1, localClose: true, pending: &wsReplayRead{err: err}}, {inFlight: -1},
		}}
		if controller.consumePending() == nil {
			t.Fatal("已经观测到的非 1000 close 仍被吞成正常回应")
		}
	})
}

// 调用端预先给 Dial/Accept 注入小预算；事后 matcher 不能代替帧层分配前拒绝。
func TestWSReplayMessageLimit(t *testing.T) {
	for _, side := range []WSPoint{WSClientSend, WSUpstreamSend} {
		t.Run(string(side), func(t *testing.T) {
			f := wsReplayFixture(side, "completed", 1000)
			limits := DefaultWSLimits()
			limits.MessageBytes = 64
			peers, _ := wsReplayBridgeMode(t, f, limits, nil, "overlimit_"+string(side))
			_, err := ReplayWS(context.Background(), f, peers, 1, limits)
			if err == nil || !strings.Contains(err.Error(), "接收失败") {
				t.Fatalf("实际超限帧没有被连接层拒绝: %v", err)
			}
		})
	}
}

// 未声明的实际码与 reason 必须拒绝，不能因 Close 自动回 1000 或有效码范围而批准。
func TestWSReplayUnexpectedClose(t *testing.T) {
	for _, mode := range []string{"unknown_actual_close", "wrong_reason"} {
		t.Run(mode, func(t *testing.T) {
			f := wsReplayFixture(WSClientSend, "completed", 1000)
			peers, _ := wsReplayBridgeMode(t, f, DefaultWSLimits(), nil, mode)
			result, err := ReplayWS(context.Background(), f, peers, 1, DefaultWSLimits())
			if err == nil || result.Outcome.Kind != "" || !strings.Contains(err.Error(), "ws.nodes[5].close_code") || strings.Contains(err.Error(), "synthetic-private") {
				t.Fatal("实际异常关闭被批准或 reason 泄露")
			}
		})
	}
}

// 实际动态 ID 可能比录制字面值长，必须计入回放轨迹总预算而非只验 fixture 静态负载。
func TestWSReplayActualTraceLimit(t *testing.T) {
	f := wsReplayFixture(WSClientSend, "completed", 1000)
	f.Response.WS.Nodes[3].ForwardedFrom = ""
	f.Response.WS.Nodes[3].Fields = []WSFieldRule{{Pointer: "/id", Mode: "bind", Namespace: "client", Symbol: "answer"}}
	f.Response.WS.Outcome.Terminal[0].Namespace = "client"
	f.Response.WS.Outcome.Terminal[0].Symbol = "answer"
	limits := DefaultWSLimits()
	// 四个字面消息各 44、44、34、34 字节，两个 close 各 2 字节，总计 160。
	limits.TraceBytes = 160
	for _, tc := range []struct {
		name, payload, path string
	}{
		{"message", `{"id":"synthetic-private-long-runtime-id","state":"done"}`, "ws.nodes[3].message.payload"},
		{"close", `{"id":"response-00001","state":"done"}`, ".close_code"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutate := func(point WSPoint, n int, msg WSMessage) []WSMessage {
				if point == WSUpstreamSend && n == 0 {
					msg.Payload = []byte(tc.payload)
				}
				return []WSMessage{msg}
			}
			peers, _ := wsReplayBridge(t, f, limits, mutate)
			result, err := ReplayWS(context.Background(), f, peers, 1, limits)
			if err == nil || result.Outcome.Kind != "" || !strings.Contains(err.Error(), tc.path) || strings.Contains(err.Error(), "synthetic-private") {
				t.Fatal("动态实际负载或关闭帧逃逸总预算，或错误回显实际 ID")
			}
		})
	}
}

// 样本也占静态轨迹预算；实际 ID 扩长时不能重新从零累计而绕过已占用的样本字节。
func TestWSReplayTraceIncludesSamples(t *testing.T) {
	f := wsReplayFixture(WSClientSend, "completed", 1000)
	f.Response.WS.Nodes[3].ForwardedFrom = ""
	f.Response.WS.Nodes[3].Fields = []WSFieldRule{{Pointer: "/id", Mode: "bind", Namespace: "client", Symbol: "answer"}}
	f.Response.WS.Outcome.Terminal[0].Namespace = "client"
	f.Response.WS.Outcome.Terminal[0].Symbol = "answer"
	// "abc" 的手工固定 SHA-256，不从运行结果生成预期账本。
	const digest = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	f.Response.WS.Samples = map[string]WSSample{"sample": {Data: []byte("abc"), SHA256: digest}}
	f.Response.WS.Provenance.SampleSHA256 = map[string]string{"sample": digest}
	limits := DefaultWSLimits()
	limits.TraceBytes = 163
	mutate := func(point WSPoint, n int, msg WSMessage) []WSMessage {
		if point == WSUpstreamSend && n == 0 {
			msg.Payload = []byte(`{"id":"response-01","state":"done"}`)
		}
		return []WSMessage{msg}
	}
	peers, _ := wsReplayBridge(t, f, limits, mutate)
	result, err := ReplayWS(context.Background(), f, peers, 1, limits)
	if err == nil || result.Outcome.Kind != "" || !strings.Contains(err.Error(), ".close_code") {
		t.Fatal("实际轨迹忽略了样本占用的总预算")
	}
}

// 写真实 TCP 时卡在可控屏障，取消必须直接关闭 socket，而不是等 writer 把锁归还。
func TestWSReplayReleasesBlockedPeers(t *testing.T) {
	for _, parent := range []bool{true, false} {
		t.Run(map[bool]string{true: "parent", false: "limit"}[parent], func(t *testing.T) {
			f := wsReplayFixture(WSClientSend, "completed", 1000)
			limits := DefaultWSLimits()
			client, remote, gate := wsReplayBlockedPair(t, limits.MessageBytes)
			t.Cleanup(func() { _ = gate.Close() })
			upRaw, other := wsReplayTCPPair(t)
			up := ws.NewConn(upRaw, ws.RoleServer, limits.MessageBytes, 0)
			t.Cleanup(func() { _ = other.Close(); _ = remote.Close(); _ = up.Close(1000, "") })
			ctx := context.Background()
			cancel := func() {}
			if parent {
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
			} else {
				limits.Replay = 100 * time.Millisecond
			}
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := ReplayWS(ctx, f, WSReplayEndpoints{Client: client, Upstream: up}, 1, limits)
				done <- err
			}()
			select {
			case <-gate.entered:
			case <-time.After(time.Second):
				t.Fatal("writer 未进入实际 TCP 写屏障")
			}
			// 另一端持续有正确负载早到，但发送祖先尚未完成；超过容量 1 的读队列
			// 不能阻止取消唤醒 socket 与 channel 投递，亦不能提前确认这些接收。
			otherWS := ws.NewConn(other, ws.RoleClient, limits.MessageBytes, 0)
			for i := 0; i < 4; i++ {
				message := f.Response.WS.Nodes[1].Message
				if otherWS.WriteMessage(message.Opcode, message.Payload) != nil {
					t.Fatal("本地读队列压力注入失败")
				}
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("取消没有保留 deadline 或意外成功")
				}
			case <-time.After(time.Second):
				t.Fatal("取消未在一秒内归还全部 workers")
			}
			select {
			case <-gate.closed:
			default:
				t.Fatal("取消没有关闭阻塞 writer 的 socket")
			}
			_ = other.SetReadDeadline(time.Now().Add(time.Second))
			_, _, err := otherWS.ReadMessage()
			if err == nil {
				t.Fatal("取消未关闭另一侧 socket")
			}
		})
	}
}

// 已关闭上游不会再发 1011；独立下游的同码 close 不能补造上游的发送证据。
func TestWSReplayClosedEndpointDoesNotInventClose(t *testing.T) {
	f := wsReplayFixture(WSUpstreamSend, "failed", 1011)
	f.Response.WS.Nodes = f.Response.WS.Nodes[4:]
	f.Response.WS.Nodes[0].After = nil
	limits := DefaultWSLimits()
	limits.Replay = 200 * time.Millisecond
	if ValidateWSSession(f, limits) != nil {
		t.Fatal("手写关闭 fixture 无效，未覆盖运行态发送证据")
	}
	upRaw, upRemote := wsReplayTCPPair(t)
	clientRaw, clientRemote := wsReplayTCPPair(t)
	defer upRaw.Close()
	defer upRemote.Close()
	defer clientRaw.Close()
	defer clientRemote.Close()
	up := ws.NewConn(upRaw, ws.RoleServer, limits.MessageBytes, 0)
	observer := ws.NewConn(upRemote, ws.RoleClient, limits.MessageBytes, 0)
	client := ws.NewConn(clientRaw, ws.RoleClient, limits.MessageBytes, 0)
	down := ws.NewConn(clientRemote, ws.RoleServer, limits.MessageBytes, 0)
	if up.Close(1000, "") != nil {
		t.Fatal("预先关闭上游失败")
	}
	_, _, observedErr := observer.ReadMessage()
	var observedClose *ws.CloseError
	if !errors.As(observedErr, &observedClose) || observedClose.Code != 1000 {
		t.Fatal("上游没有实际发送预先的 1000 close")
	}
	if down.Close(1011, "") != nil {
		t.Fatal("独立下游关闭注入失败")
	}
	result, err := ReplayWS(context.Background(), f, WSReplayEndpoints{Client: client, Upstream: up}, 1, limits)
	if err == nil || result.Outcome.Kind != "" {
		t.Fatal("已关闭上游的幂等 Close 补造了不存在的 1011，坏 bridge 得到批准")
	}
}

// 回放接管后被动 close 自动回应已关 TCP，后续指定发送不能被幂等成功伪证。
func TestWSReplayPassiveCloseDoesNotInventLaterSend(t *testing.T) {
	f := wsReplayFixture(WSUpstreamSend, "interrupted", 1000)
	recvCode, phantomCode, clientCode := uint16(1000), uint16(1011), uint16(1000)
	f.Response.WS.Nodes = []WSNode{
		{ID: "up-receive", Point: WSUpstreamReceive, Source: "synthetic", Kind: "close", CloseCode: &recvCode},
		{ID: "up-send", Point: WSUpstreamSend, Source: "synthetic", Kind: "close", CloseCode: &phantomCode, After: []string{"up-receive"}},
		{ID: "client-send", Point: WSClientSend, Source: "synthetic", Kind: "close", CloseCode: &clientCode, After: []string{"up-send"}},
	}
	limits := DefaultWSLimits()
	limits.Replay = 200 * time.Millisecond
	if ValidateWSSession(f, limits) != nil {
		t.Fatal("手写关闭 fixture 无效，未覆盖运行态发送证据")
	}
	upRaw, upRemote := wsReplayTCPPair(t)
	clientRaw, clientRemote := wsReplayTCPPair(t)
	defer upRaw.Close()
	defer upRemote.Close()
	defer clientRaw.Close()
	defer clientRemote.Close()
	up := ws.NewConn(upRaw, ws.RoleServer, limits.MessageBytes, 0)
	client := ws.NewConn(clientRaw, ws.RoleClient, limits.MessageBytes, 0)
	remote := ws.NewConn(upRemote, ws.RoleClient, limits.MessageBytes, 0)
	if remote.Close(1000, "") != nil {
		t.Fatal("被动关闭注入失败")
	}
	result, err := ReplayWS(context.Background(), f, WSReplayEndpoints{Client: client, Upstream: up}, 1, limits)
	if err == nil || result.Outcome.Kind != "" {
		t.Fatal("被动关闭自动回应后的不存在发送仍获批准")
	}
}

// 关闭帧及主动端 EOF 可以早于 write-result 投递；只能等真实 sent 结果，不能误报缺证。
func TestWSReplayCloseBeforeWriteResult(t *testing.T) {
	for _, side := range []WSPoint{WSClientSend, WSUpstreamSend} {
		t.Run(string(side), func(t *testing.T) {
			f := wsReplayFixture(side, "interrupted", 1000)
			nodes := f.Response.WS.Nodes[4:]
			nodes[0].After = nil
			limits := DefaultWSLimits()
			schedule, err := NewWSSchedule(nodes, 1)
			if err != nil {
				t.Fatal("关闭调度初始化失败")
			}
			matcher, err := NewWSMatcher(limits)
			if err != nil {
				t.Fatal("matcher 初始化失败")
			}
			a, b := wsReplayTCPPair(t)
			defer a.Close()
			defer b.Close()
			controller := &wsReplayController{nodes: schedule.nodes, graph: schedule.graph, schedule: schedule, matcher: matcher, limits: limits,
				completed: make([]bool, 2), observed: make([]*WSMessage, 2), endpoints: [2]*wsReplayEndpoint{
					{conn: ws.NewConn(a, ws.RoleClient, limits.MessageBytes, 0), inFlight: -1, writes: make(chan wsReplayWrite, 1), written: make(chan wsReplayWritten, 1)},
					{conn: ws.NewConn(b, ws.RoleServer, limits.MessageBytes, 0), inFlight: -1, writes: make(chan wsReplayWrite, 1), written: make(chan wsReplayWritten, 1)},
				}}
			sender := controller.endpoint(side)
			receiver := controller.endpoint(nodes[1].Point)
			receiver.receives = []int{1}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			var workers sync.WaitGroup
			workers.Add(1)
			go sender.write(ctx, controller.nodes, &workers)
			defer func() { cancel(); workers.Wait() }()
			if controller.dispatch(ctx) != nil {
				t.Fatal("实际 close 发令失败")
			}
			_, _, received := receiver.conn.ReadMessage()
			var closed *ws.CloseError
			if !errors.As(received, &closed) || closed.Code != 1000 {
				t.Fatal("没有实际接收完整关闭帧")
			}
			_, _, ended := sender.conn.ReadMessage()
			if ended == nil {
				t.Fatal("主动端 socket 没有关闭")
			}
			receiver.pending = &wsReplayRead{err: received}
			sender.pending = &wsReplayRead{err: ended}
			if controller.consumePending() != nil || controller.completed[1] || sender.readEnded {
				t.Fatal("真实 write-result 尚未确认便批准或误拒关闭证据")
			}
			var result wsReplayWritten
			select {
			case result = <-sender.written:
			case <-ctx.Done():
				t.Fatal("实际关闭 writer 没有归还结果")
			}
			if !result.closeSent || result.err != nil || controller.finishWrite(sender, result) != nil {
				t.Fatal("真实发送结果不能确认关闭动作")
			}
			if controller.consumePending() != nil || !schedule.Done() || !sender.readEnded || !receiver.readEnded || controller.trace != 4 {
				t.Fatal("实际发送确认后未消费关闭证据或累计预算不符")
			}
		})
	}
}

// false 的 close 结果不能修改预算、localClose 或节点完成状态，nil error 不是发帧证明。
func TestWSReplayUnsentCloseDoesNotComplete(t *testing.T) {
	f := wsReplayFixture(WSUpstreamSend, "interrupted", 1000)
	nodes := f.Response.WS.Nodes[4:]
	nodes[0].After = nil
	schedule, err := NewWSSchedule(nodes, 1)
	if err != nil || schedule.Start("close-send") != nil {
		t.Fatal("关闭调度初始化失败")
	}
	controller := &wsReplayController{nodes: schedule.nodes, graph: schedule.graph, schedule: schedule, limits: DefaultWSLimits(), completed: make([]bool, 2)}
	ep := &wsReplayEndpoint{inFlight: 0}
	if err := controller.finishWrite(ep, wsReplayWritten{job: wsReplayWrite{index: 0}}); err == nil || !strings.Contains(err.Error(), "ws.nodes[0].close_code") {
		t.Fatal("未发送 close 没有可见节点失败")
	}
	if controller.trace != 0 || ep.localClose || controller.completed[0] || !schedule.InFlight("close-send") {
		t.Fatal("未发送 close 污染了证据、预算或生命周期状态")
	}
}

// 早到接收的必要 receive 祖先不能由未来观测补齐，哪怕全部发送均已成功。
func TestWSReplayReceiveAncestorMustAlreadyBeObserved(t *testing.T) {
	f := wsReplayFixture(WSClientSend, "completed", 1000)
	f.Response.WS.Nodes[2].After = nil
	f.Response.WS.Nodes[3].After = []string{"u-send", "u-receive"}
	limits := DefaultWSLimits()
	if ValidateWSSession(f, limits) != nil {
		t.Fatal("手写接收因果 fixture 无效")
	}
	schedule, err := NewWSSchedule(f.Response.WS.Nodes, 1)
	if err != nil {
		t.Fatal("手写调度初始化失败")
	}
	matcher, err := NewWSMatcher(limits)
	if err != nil {
		t.Fatal("matcher 初始化失败")
	}
	controller := &wsReplayController{nodes: schedule.nodes, graph: schedule.graph, schedule: schedule, matcher: matcher, limits: limits,
		completed: make([]bool, 6), observed: make([]*WSMessage, 6), endpoints: [2]*wsReplayEndpoint{
			{inFlight: -1, receives: []int{3}}, {inFlight: -1, receives: []int{1, 5}},
		}}
	for _, i := range []int{0, 2} {
		if schedule.Start(f.Response.WS.Nodes[i].ID) != nil {
			t.Fatal("发送启动失败")
		}
		ep := controller.endpoint(f.Response.WS.Nodes[i].Point)
		ep.inFlight = i
		if controller.finishWrite(ep, wsReplayWritten{job: wsReplayWrite{index: i, message: *f.Response.WS.Nodes[i].Message}}) != nil {
			t.Fatal("发送完成失败")
		}
	}
	a, b := wsReplayTCPPair(t)
	defer a.Close()
	defer b.Close()
	client := ws.NewConn(a, ws.RoleClient, limits.MessageBytes, 0)
	server := ws.NewConn(b, ws.RoleServer, limits.MessageBytes, 0)
	message := f.Response.WS.Nodes[3].Message
	if server.WriteMessage(message.Opcode, message.Payload) != nil {
		t.Fatal("早到接收注入失败")
	}
	op, payload, err := client.ReadMessage()
	if err != nil {
		t.Fatal("早到消息实际接收失败")
	}
	controller.endpoints[0].pending = &wsReplayRead{message: WSMessage{Opcode: op, Payload: payload}}
	if err := controller.consumePending(); err == nil || !strings.Contains(err.Error(), "ws.nodes[3]") || controller.completed[3] {
		t.Fatal("缺少实际接收祖先的早到消息被缓存等待未来补证")
	}
}

// 同端 close 正在发送不能掩盖预期接收 close 缺少 receive 祖先；本地 EOF 宽限只管无剩余接收。
func TestWSReplaySendingCloseDoesNotHideReceiveAncestor(t *testing.T) {
	f := wsReplayFixture(WSClientSend, "interrupted", 1000)
	code := uint16(1000)
	f.Response.WS.Nodes = []WSNode{
		{ID: "send-close", Point: WSClientSend, Source: "synthetic", Kind: "close", CloseCode: &code},
		wsReplayMessage("unobserved", WSUpstreamReceive, ws.OpText, `{"marker":true}`, "json"),
		{ID: "receive-close", Point: WSClientReceive, Source: "synthetic", Kind: "close", CloseCode: &code, After: []string{"send-close", "unobserved"}},
	}
	limits := DefaultWSLimits()
	if ValidateWSSession(f, limits) != nil {
		t.Fatal("手写关闭因果 fixture 无效")
	}
	schedule, err := NewWSSchedule(f.Response.WS.Nodes, 1)
	if err != nil || schedule.Start("send-close") != nil {
		t.Fatal("关闭调度初始化失败")
	}
	a, b := wsReplayTCPPair(t)
	defer a.Close()
	defer b.Close()
	client := ws.NewConn(a, ws.RoleClient, limits.MessageBytes, 0)
	server := ws.NewConn(b, ws.RoleServer, limits.MessageBytes, 0)
	if server.Close(1000, "") != nil {
		t.Fatal("早到关闭注入失败")
	}
	_, _, observed := client.ReadMessage()
	var closed *ws.CloseError
	if !errors.As(observed, &closed) || closed.Code != 1000 {
		t.Fatal("未实际接收关闭帧")
	}
	controller := &wsReplayController{nodes: schedule.nodes, graph: schedule.graph, schedule: schedule, completed: make([]bool, 3), endpoints: [2]*wsReplayEndpoint{
		{inFlight: 0, receives: []int{2}, pending: &wsReplayRead{err: observed}}, {inFlight: -1},
	}}
	if err := controller.consumePending(); err == nil || !strings.Contains(err.Error(), "ws.nodes[2]") {
		t.Fatal("同端 close in-flight 隐藏了尚未观测的必要接收祖先")
	}
}

func wsReplayMessage(id string, point WSPoint, op ws.Opcode, payload, match string, after ...string) WSNode {
	return WSNode{ID: id, Point: point, Source: "synthetic", After: after, Kind: "message", Message: &WSMessage{Opcode: op, Payload: []byte(payload)}, Match: match}
}

func wsReplayFixture(side WSPoint, kind string, code uint16) Fixture {
	f := Fixture{Name: "synthetic-replay", Note: "合成四点与关闭机制，非真实能力证据", Request: Request{Method: "GET", Path: "/v1/realtime"}, Response: Response{Status: 101}}
	nodes := []WSNode{
		wsReplayMessage("c-send", WSClientSend, ws.OpText, `{"type":"command","model":"synthetic-model"}`, "json"),
		wsReplayMessage("u-receive", WSUpstreamReceive, ws.OpText, `{"type":"command","model":"synthetic-model"}`, "json", "c-send"),
		wsReplayMessage("u-send", WSUpstreamSend, ws.OpText, `{"id":"response-1","state":"done"}`, "json", "u-receive"),
		wsReplayMessage("c-receive", WSClientReceive, ws.OpText, `{"id":"response-1","state":"done"}`, "json", "u-send"),
	}
	nodes[1].ForwardedFrom = "c-send"
	nodes[3].ForwardedFrom = "u-send"
	receive := WSUpstreamReceive
	if side == WSUpstreamSend {
		receive = WSClientReceive
	}
	sendCode, recvCode := code, code
	nodes = append(nodes, WSNode{ID: "close-send", Point: side, Source: "synthetic", After: []string{"c-receive"}, Kind: "close", CloseCode: &sendCode}, WSNode{ID: "close-receive", Point: receive, Source: "synthetic", After: []string{"close-send"}, Kind: "close", CloseCode: &recvCode})
	outcome := WSOutcome{Kind: kind}
	if kind == "completed" {
		outcome.Terminal = []WSTerminal{{Node: "c-receive", Namespace: "response", Symbol: "response-1", IDPointer: "/id", StatePointer: "/state", State: "done"}}
	}
	f.Response.WS = &WSSession{Version: 1, ClientProtocol: "synthetic", ClientVersion: "1", Upstream: Request{Method: "GET", Path: "/v1/realtime"}, UpstreamExpectedStatus: 101, Provenance: WSProvenance{Kind: "synthetic"}, Nodes: nodes, Outcome: outcome}
	return f
}

type wsReplayMutation func(WSPoint, int, WSMessage) []WSMessage

func wsReplayBridge(t *testing.T, f Fixture, l WSLimits, mutate wsReplayMutation) (WSReplayEndpoints, *WSReplayUpstream) {
	t.Helper()
	return wsReplayBridgeMode(t, f, l, mutate, "")
}

func wsReplayBridgeMode(t *testing.T, f Fixture, l WSLimits, mutate wsReplayMutation, mode string) (WSReplayEndpoints, *WSReplayUpstream) {
	t.Helper()
	u, err := NewWSReplayUpstream(f, l)
	if err != nil {
		t.Fatal("手写 fixture 未通过构造校验")
	}
	upSrv := httptest.NewServer(u)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var bridgeUp *ws.Conn
	var rawUp net.Conn
	if mode == "overlimit_"+string(WSClientSend) {
		rawUp, err = net.Dial("tcp", strings.TrimPrefix(upSrv.URL, "http://"))
		if err != nil {
			t.Fatal("原始上游 TCP 拨号失败")
		}
		r, _ := http.NewRequest("GET", upSrv.URL+"/v1/realtime", nil)
		r.Header = http.Header{"Connection": {"Upgrade"}, "Upgrade": {"websocket"}, "Sec-WebSocket-Version": {"13"}, "Sec-WebSocket-Key": {"AAECAwQFBgcICQoLDA0ODw=="}}
		if r.Write(rawUp) != nil {
			t.Fatal("原始上游握手发送失败")
		}
		resp, e := http.ReadResponse(bufio.NewReader(rawUp), r)
		if e != nil || resp.StatusCode != 101 {
			t.Fatal("原始上游未升级")
		}
		bridgeUp = ws.NewConn(rawUp, ws.RoleClient, l.MessageBytes, 0)
	} else {
		bridgeUp, _, err = ws.Dial(ctx, "ws"+strings.TrimPrefix(upSrv.URL, "http")+"/v1/realtime", ws.DialOptions{MaxPayload: l.MessageBytes})
		if err != nil {
			t.Fatal("bridge 上游握手失败")
		}
	}
	up, err := u.Connection(ctx)
	if err != nil {
		t.Fatal("bridge 上游领取失败")
	}
	ready := make(chan *ws.Conn, 1)
	var rawDown net.Conn
	downSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := ws.Accept(&wsReplayCaptureHijacker{ResponseWriter: w, capture: func(c net.Conn) { rawDown = c }}, r, ws.AcceptOptions{MaxPayload: l.MessageBytes})
		if err != nil {
			ready <- nil
			return
		}
		ready <- c
	}))
	client, _, err := ws.Dial(ctx, "ws"+strings.TrimPrefix(downSrv.URL, "http")+"/v1/realtime", ws.DialOptions{MaxPayload: l.MessageBytes})
	if err != nil {
		t.Fatal("bridge 下游握手失败")
	}
	var bridgeDown *ws.Conn
	select {
	case bridgeDown = <-ready:
	case <-ctx.Done():
		t.Fatal("bridge 未接受下游")
	}
	if bridgeDown == nil {
		t.Fatal("bridge 下游接管失败")
	}
	var wg sync.WaitGroup
	forward := func(from, to *ws.Conn, point WSPoint) {
		defer wg.Done()
		for n := 0; ; n++ {
			op, payload, err := from.ReadMessage()
			if err != nil {
				var closed *ws.CloseError
				if errors.As(err, &closed) {
					if mode == "drop_close" {
						return
					}
					code := closed.Code
					if mode == "wrong_close" {
						code = 1011
					}
					reason := closed.Reason
					if mode == "unknown_actual_close" {
						code = 2999
					}
					if mode == "wrong_reason" {
						reason = "synthetic-private"
					}
					_ = to.Close(code, reason)
				} else {
					_ = to.Close(1011, "")
				}
				return
			}
			if mode == "raw_eof" && point == WSClientSend {
				_ = rawDown.Close()
				return
			}
			if mode == "overlimit_"+string(WSClientSend) && point == WSClientSend {
				_ = ws.WriteFrame(rawUp, ws.Frame{FIN: true, Opcode: ws.OpBinary, Payload: bytes.Repeat([]byte{1}, 128)}, true)
				return
			}
			if mode == "overlimit_"+string(WSUpstreamSend) && point == WSUpstreamSend {
				_ = ws.WriteFrame(rawDown, ws.Frame{FIN: true, Opcode: ws.OpBinary, Payload: bytes.Repeat([]byte{1}, 128)}, false)
				return
			}
			messages := []WSMessage{{Opcode: op, Payload: payload}}
			if mutate != nil {
				messages = mutate(point, n, messages[0])
			}
			for _, msg := range messages {
				if to.WriteMessage(msg.Opcode, msg.Payload) != nil {
					return
				}
			}
		}
	}
	wg.Add(2)
	go forward(bridgeDown, bridgeUp, WSClientSend)
	go forward(bridgeUp, bridgeDown, WSUpstreamSend)
	t.Cleanup(func() {
		_ = client.Close(1000, "")
		_ = up.Close(1000, "")
		_ = bridgeDown.Close(1000, "")
		_ = bridgeUp.Close(1000, "")
		_ = u.Close()
		wg.Wait()
		downSrv.Close()
		upSrv.Close()
	})
	return WSReplayEndpoints{Client: client, Upstream: up}, u
}

type wsReplayCaptureHijacker struct {
	http.ResponseWriter
	capture func(net.Conn)
}

func (w *wsReplayCaptureHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, rw, e := w.ResponseWriter.(http.Hijacker).Hijack()
	if e == nil {
		w.capture(c)
	}
	return c, rw, e
}

func wsReplayTCPPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("本地 TCP 监听失败")
	}
	defer listener.Close()
	a, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal("本地 TCP 拨号失败")
	}
	b, err := listener.Accept()
	if err != nil {
		_ = a.Close()
		t.Fatal("本地 TCP 接受失败")
	}
	return a, b
}

type wsReplayWriteGate struct {
	net.Conn
	entered, closed chan struct{}
	once            sync.Once
	closeOnce       sync.Once
}

func (g *wsReplayWriteGate) Write(p []byte) (int, error) {
	g.once.Do(func() { close(g.entered) })
	<-g.closed
	return 0, net.ErrClosed
}
func (g *wsReplayWriteGate) Close() error {
	g.closeOnce.Do(func() { close(g.closed) })
	return g.Conn.Close()
}
func wsReplayBlockedPair(t *testing.T, limit int64) (*ws.Conn, net.Conn, *wsReplayWriteGate) {
	t.Helper()
	a, b := wsReplayTCPPair(t)
	gate := &wsReplayWriteGate{Conn: a, entered: make(chan struct{}), closed: make(chan struct{})}
	return ws.NewConn(gate, ws.RoleClient, limit, 0), b, gate
}
