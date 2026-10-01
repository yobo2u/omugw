package testkit

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/transport/ws"
)

// 发送开始不代表前置动作已成功；提前释放接收或重发节点都会掩盖失败回放。
func TestWSCausalScheduling(t *testing.T) {
	chain := []WSNode{
		wsScheduleMessage("configure", WSClientSend),
		wsScheduleMessage("accepted", WSUpstreamReceive, "configure"),
		wsScheduleMessage("updated", WSUpstreamSend, "accepted"),
		wsScheduleMessage("confirmed", WSClientReceive, "updated"),
	}
	t.Run("configuration-chain", func(t *testing.T) {
		s := mustWSSchedule(t, chain, 1)
		if s.Done() || s.InFlight("unknown") {
			t.Fatal("未消费轨迹被判完成或未知节点被判已开始")
		}
		if err := s.Start("unknown"); err == nil {
			t.Fatal("未知节点被开始")
		}
		if err := s.Complete("unknown"); err == nil {
			t.Fatal("未知节点被完成")
		}
		for i, id := range []string{"configure", "accepted", "updated", "confirmed"} {
			assertWSReady(t, s, id)
			if err := s.Complete(id); err == nil {
				t.Fatal("尚未开始的节点被完成")
			}
			if i+1 < len(chain) {
				if err := s.Start(chain[i+1].ID); err == nil {
					t.Fatal("未就绪的后继被开始")
				}
			}
			if err := s.Start(id); err != nil {
				t.Fatal(err)
			}
			if !s.InFlight(id) || s.Done() {
				t.Fatal("开始未记为 in-flight 或被提前判为完成")
			}
			assertWSReady(t, s)
			if err := s.Start(id); err == nil {
				t.Fatal("in-flight 节点可被重发")
			}
			if i+1 < len(chain) {
				if err := s.Start(chain[i+1].ID); err == nil {
					t.Fatal("仅开始、未完成的前置释放了后继")
				}
			}
			if err := s.Complete(id); err != nil {
				t.Fatal(err)
			}
			if s.InFlight(id) {
				t.Fatal("完成后仍为 in-flight")
			}
			if err := s.Complete(id); err == nil {
				t.Fatal("重复 Complete 未拒绝")
			}
			if err := s.Start(id); err == nil {
				t.Fatal("已完成节点可被重发")
			}
			if got := s.Done(); got != (i == len(chain)-1) {
				t.Fatalf("完成 %s 后 Done=%v", id, got)
			}
		}
		assertWSReady(t, s)
	})
	t.Run("seeded-independent-sends", func(t *testing.T) {
		nodes := []WSNode{wsScheduleMessage("upstream-send", WSUpstreamSend), wsScheduleMessage("client-send", WSClientSend)}
		first := wsScheduleSequence(t, nodes, 1)
		second := wsScheduleSequence(t, nodes, 2)
		if slices.Equal(first, second) {
			t.Fatalf("seeds 1/2 未探索不同发送次序: %v", first)
		}
		for _, sequence := range [][]string{first, second} {
			sorted := slices.Clone(sequence)
			slices.Sort(sorted)
			if !slices.Equal(sorted, []string{"client-send", "upstream-send"}) {
				t.Fatalf("遗漏或重复发送: %v", sequence)
			}
		}
		for _, seed := range []uint64{0, 1, 2, ^uint64(0)} {
			want := wsScheduleSequence(t, nodes, seed)
			for range 8 {
				if got := wsScheduleSequence(t, nodes, seed); !slices.Equal(got, want) {
					t.Fatalf("seed %d 纯调度不可重放: %v != %v", seed, got, want)
				}
			}
			reversed := slices.Clone(nodes)
			slices.Reverse(reversed)
			if got := wsScheduleSequence(t, reversed, seed); !slices.Equal(got, want) {
				t.Fatalf("跨点文件顺序影响种子调度: %v != %v", got, want)
			}
		}
	})
	t.Run("strict-chain-has-one-order", func(t *testing.T) {
		for _, seed := range []uint64{0, 1, 2, ^uint64(0)} {
			if got := wsScheduleSequence(t, chain, seed); !slices.Equal(got, []string{"configure", "accepted", "updated", "confirmed"}) {
				t.Fatalf("严格链被打乱: %v", got)
			}
		}
	})
}

// 两个接收点可以交错完成，但一个点自己的下一条不能越过尚未完成的前一条。
func TestWSScheduleDoesNotRequireReceiveOrderAcrossPoints(t *testing.T) {
	nodes := []WSNode{
		wsScheduleMessage("c-send-1", WSClientSend),
		wsScheduleMessage("u-send-1", WSUpstreamSend),
		wsScheduleMessage("c-send-2", WSClientSend),
		wsScheduleMessage("u-send-2", WSUpstreamSend),
		wsScheduleMessage("u-receive-1", WSUpstreamReceive, "c-send-1"),
		wsScheduleMessage("c-receive-1", WSClientReceive, "u-send-1"),
		wsScheduleMessage("u-receive-2", WSUpstreamReceive, "c-send-2"),
		wsScheduleMessage("c-receive-2", WSClientReceive, "u-send-2"),
	}
	for _, first := range []string{"u-receive-1", "c-receive-1"} {
		t.Run(first, func(t *testing.T) {
			s := mustWSSchedule(t, nodes, 2)
			for _, id := range []string{"c-send-1", "u-send-1"} {
				if err := s.Start(id); err != nil {
					t.Fatal(err)
				}
			}
			assertWSReady(t, s)
			for _, id := range []string{"c-send-2", "u-send-2", "u-receive-1", "c-receive-1"} {
				if err := s.Start(id); err == nil {
					t.Fatal("仅 in-flight 的发送释放了点内或跨点后继")
				}
			}
			for _, id := range []string{"c-send-1", "u-send-1"} {
				if err := s.Complete(id); err != nil {
					t.Fatal(err)
				}
			}
			assertWSReadySet(t, s, "c-send-2", "u-send-2", "u-receive-1", "c-receive-1")
			for _, id := range []string{"u-send-2", "c-send-2"} {
				startCompleteWSNode(t, s, id)
			}
			if s.Done() {
				t.Fatal("仅消费发送就判 Done")
			}
			assertWSReadySet(t, s, "u-receive-1", "c-receive-1")
			for _, id := range []string{"u-receive-1", "c-receive-1"} {
				if err := s.Start(id); err != nil {
					t.Fatal(err)
				}
			}
			assertWSReady(t, s)
			for _, id := range []string{"u-receive-2", "c-receive-2"} {
				if err := s.Start(id); err == nil {
					t.Fatal("接收点 FIFO 越过尚未完成的第一条")
				}
			}
			second, next, last := "c-receive-1", "u-receive-2", "c-receive-2"
			if first == "c-receive-1" {
				second, next, last = "u-receive-1", "c-receive-2", "u-receive-2"
			}
			if err := s.Complete(first); err != nil {
				t.Fatal(err)
			}
			assertWSReady(t, s, next)
			if !s.InFlight(second) {
				t.Fatal("跨点完成吞掉另一流的 in-flight 期望")
			}
			if err := s.Start(next); err != nil {
				t.Fatal(err)
			}
			if err := s.Complete(second); err != nil {
				t.Fatal(err)
			}
			assertWSReady(t, s, last)
			startCompleteWSNode(t, s, last)
			if s.Done() {
				t.Fatal("最后一条仍 in-flight 时判 Done")
			}
			if err := s.Complete(next); err != nil {
				t.Fatal(err)
			}
			if !s.Done() {
				t.Fatal("两个接收点全部消费后仍未完成")
			}
			assertWSReady(t, s)
		})
	}
}

// 调度必须单独守住图入口，不能假定所有调用者已经经过 fixture 校验。
func TestWSScheduleRejectsInvalidGraph(t *testing.T) {
	for _, tt := range []struct {
		name  string
		nodes []WSNode
	}{
		{"duplicate-id", []WSNode{wsScheduleMessage("same", WSClientSend), wsScheduleMessage("same", WSUpstreamSend)}},
		{"missing-id", []WSNode{wsScheduleMessage(" \t", WSClientSend)}},
		{"invalid-point", []WSNode{wsScheduleMessage("a", WSPoint("other"))}},
		{"dangling-after", []WSNode{wsScheduleMessage("a", WSClientSend, "missing")}},
		{"self-cycle", []WSNode{wsScheduleMessage("a", WSClientSend, "a")}},
		{"explicit-cycle", []WSNode{wsScheduleMessage("a", WSClientSend, "b"), wsScheduleMessage("b", WSUpstreamSend, "a")}},
		{"fifo-cycle", []WSNode{wsScheduleMessage("a", WSClientSend, "b"), wsScheduleMessage("b", WSClientSend)}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if s, err := NewWSSchedule(tt.nodes, 1); err == nil || s != nil {
				t.Fatal("非法因果图得到可用调度器")
			}
		})
	}
	t.Run("node-budget", func(t *testing.T) {
		nodes := make([]WSNode, DefaultWSLimits().Nodes+1)
		for i := range nodes {
			nodes[i] = wsScheduleMessage(fmt.Sprint(i), WSClientSend)
		}
		if s, err := NewWSSchedule(nodes, 1); err == nil || s != nil {
			t.Fatal("超预算节点未拒绝")
		}
	})
	t.Run("after-budget", func(t *testing.T) {
		nodes := []WSNode{wsScheduleMessage("a", WSClientSend), wsScheduleMessage("b", WSUpstreamSend)}
		nodes[1].After = make([]string, DefaultWSLimits().Edges+1)
		for i := range nodes[1].After {
			nodes[1].After[i] = "a"
		}
		if s, err := NewWSSchedule(nodes, 1); err == nil || s != nil {
			t.Fatal("超预算 after 未拒绝")
		}
	})
}

// after 可以向文件后方引用，汇合点却必须等全部前置完成，不能依赖数组顺序。
func TestWSScheduleWaitsForEveryParent(t *testing.T) {
	nodes := []WSNode{
		wsScheduleMessage("join", WSClientReceive, "client", "upstream"),
		wsScheduleMessage("upstream", WSUpstreamSend),
		wsScheduleMessage("client", WSClientSend),
	}
	for _, first := range []string{"client", "upstream"} {
		t.Run(first, func(t *testing.T) {
			s := mustWSSchedule(t, nodes, 1)
			assertWSReadySet(t, s, "client", "upstream")
			for _, id := range []string{"client", "upstream"} {
				if err := s.Start(id); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Complete(first); err != nil {
				t.Fatal(err)
			}
			assertWSReady(t, s)
			if err := s.Start("join"); err == nil {
				t.Fatal("汇合点只等一个前置就被开始")
			}
			last := "upstream"
			if first == "upstream" {
				last = "client"
			}
			if !s.InFlight(last) {
				t.Fatal("汇合点错误吞掉尚未完成的前置")
			}
			if err := s.Complete(last); err != nil {
				t.Fatal(err)
			}
			assertWSReady(t, s, "join")
			startCompleteWSNode(t, s, "join")
			if !s.Done() {
				t.Fatal("全部前置完成后汇合点没有推进")
			}
		})
	}
}

// 输入或 Ready 的嵌套负载被改写，不能改变控制器随后消费的节点或因果图。
func TestWSScheduleSnapshots(t *testing.T) {
	nodes := wsScheduleSnapshotNodes()
	want := wsScheduleSnapshotNodes()
	s := mustWSSchedule(t, nodes, 1)
	nodes[0].ID = "changed"
	nodes[0].Point = WSClientReceive
	nodes[0].Source = "changed"
	nodes[0].Kind = "changed"
	nodes[0].Match = "changed"
	nodes[0].Message.Opcode = ws.OpBinary
	nodes[0].Message.Payload[0] = '!'
	nodes[0].Fields[0].Pointer = "/changed"
	nodes[0].Fields[0].Mode = "changed"
	nodes[0].Fields[0].Namespace = "changed"
	nodes[0].Fields[0].Symbol = "changed"
	nodes[0].Fields[0].Value[0] = '9'
	nodes[1].After[0] = "missing"
	nodes[1].ForwardedFrom = "changed"
	*nodes[2].CloseCode = 1011
	nodes[2].CloseReason = "changed"

	assertNode := func(got, expected WSNode) {
		t.Helper()
		if !reflect.DeepEqual(got, expected) {
			t.Fatal("调度节点与独立输入快照不同")
		}
	}
	ready := s.Ready()
	if len(ready) != 1 {
		t.Fatalf("Ready 节点数=%d，预期 1", len(ready))
	}
	assertNode(ready[0], want[0])
	ready[0].ID = "output-change"
	ready[0].Message.Opcode = ws.OpBinary
	ready[0].Message.Payload[0] = '?'
	ready[0].Fields[0].Pointer = "/output-change"
	ready[0].Fields[0].Value[0] = '8'
	assertNode(s.Ready()[0], want[0])
	startCompleteWSNode(t, s, "root")

	for _, node := range s.Ready() {
		switch node.ID {
		case "child":
			assertNode(node, want[1])
			node.After[0] = "output-change"
			node.Message.Payload = append(node.Message.Payload, 'x')
		case "close":
			assertNode(node, want[2])
			*node.CloseCode = 1001
		default:
			t.Fatal("节点快照丢失或出现未知节点")
		}
	}
	for _, node := range s.Ready() {
		if node.ID == "child" {
			assertNode(node, want[1])
		} else {
			assertNode(node, want[2])
		}
	}
	startCompleteWSNode(t, s, "child")
	startCompleteWSNode(t, s, "close")
	if !s.Done() {
		t.Fatal("快照被改写后原始图不能完成")
	}
}

// 查询不能偷偷推进 PRNG，否则日志或轮询次数会改变同一种子的纯调度。
func TestWSScheduleReadyQueriesDoNotChangeOrder(t *testing.T) {
	nodes := []WSNode{
		wsScheduleMessage("c-send", WSClientSend), wsScheduleMessage("u-send", WSUpstreamSend),
		wsScheduleMessage("u-receive", WSUpstreamReceive, "c-send"), wsScheduleMessage("c-receive", WSClientReceive, "u-send"),
	}
	for _, seed := range []uint64{0, 1, 2, ^uint64(0)} {
		s := mustWSSchedule(t, nodes, seed)
		var got []string
		for !s.Done() {
			ready := s.Ready()
			if len(ready) == 0 || len(got) >= len(nodes) {
				t.Fatal("合法图悬停")
			}
			for range 12 {
				if !reflect.DeepEqual(s.Ready(), ready) {
					t.Fatal("只读 Ready 改变次序或节点")
				}
			}
			got = append(got, ready[0].ID)
			startCompleteWSNode(t, s, ready[0].ID)
		}
		if want := wsScheduleSequence(t, nodes, seed); !slices.Equal(got, want) {
			t.Fatalf("额外查询改变调度: %v != %v", got, want)
		}
	}
}

// 只判图是否有独立分支，不对大图虚构或枚举拓扑次序总数。
func TestWSScheduleOrdering(t *testing.T) {
	for _, tt := range []struct {
		name  string
		nodes []WSNode
		want  string
	}{
		{"empty", nil, "serial"},
		{"single", []WSNode{wsScheduleMessage("a", WSClientSend)}, "serial"},
		{"fifo", []WSNode{wsScheduleMessage("z", WSClientSend), wsScheduleMessage("a", WSClientSend)}, "serial"},
		{"explicit-chain", []WSNode{wsScheduleMessage("a", WSClientSend), wsScheduleMessage("b", WSUpstreamSend, "a")}, "serial"},
		{"independent", []WSNode{wsScheduleMessage("a", WSClientSend), wsScheduleMessage("b", WSUpstreamSend)}, "multiple"},
		{"later-fork", []WSNode{wsScheduleMessage("a", WSClientSend), wsScheduleMessage("b", WSUpstreamSend, "a"), wsScheduleMessage("c", WSClientReceive, "a"), wsScheduleMessage("d", WSUpstreamReceive, "b", "c")}, "multiple"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := mustWSSchedule(t, tt.nodes, 1)
			consumed := 0
			for {
				if got := s.Ordering(); got != tt.want {
					t.Fatalf("Ordering=%s，预期 %s", got, tt.want)
				}
				if s.Done() {
					break
				}
				ready := s.Ready()
				if len(ready) == 0 || consumed >= len(tt.nodes) {
					t.Fatal("合法图悬停或重复消费")
				}
				startCompleteWSNode(t, s, ready[0].ID)
				consumed++
			}
		})
	}
}

// FIFO 与显式 after 指向同一前置时，重复边不能留下永不归零的计数。
func TestWSScheduleAtNodeBudget(t *testing.T) {
	nodes := make([]WSNode, DefaultWSLimits().Nodes)
	for i := range nodes {
		nodes[i] = wsScheduleMessage(fmt.Sprintf("%04d", i), WSClientSend)
		if i > 0 {
			nodes[i].After = []string{nodes[i-1].ID, nodes[i-1].ID}
		}
	}
	s := mustWSSchedule(t, nodes, 2)
	if s.Ordering() != "serial" {
		t.Fatal("4096 节点严格 FIFO 被判有分支")
	}
	for _, node := range nodes {
		assertWSReady(t, s, node.ID)
		startCompleteWSNode(t, s, node.ID)
	}
	if !s.Done() {
		t.Fatal("冗余边阻止合法预算边界图完成")
	}
}

func TestWSScheduleUninitialized(t *testing.T) {
	for _, s := range []*WSSchedule{nil, new(WSSchedule)} {
		if len(s.Ready()) != 0 || s.InFlight("a") || s.Done() || s.Ordering() != "" {
			t.Fatal("未初始化调度器产生成功状态")
		}
		if s.Start("a") == nil || s.Complete("a") == nil {
			t.Fatal("未初始化调度器允许状态变更")
		}
	}
	s := mustWSSchedule(t, nil, 0)
	if !s.Done() || len(s.Ready()) != 0 || s.InFlight("a") {
		t.Fatal("合法空图与未初始化状态混淆")
	}
	if s.Start("a") == nil || s.Complete("a") == nil {
		t.Fatal("空图允许未知节点")
	}
}

func TestWSScheduleErrorsDoNotExposeInput(t *testing.T) {
	secret := "schedule-secret-sentinel"
	s := mustWSSchedule(t, []WSNode{wsScheduleMessage(secret, WSClientSend), wsScheduleMessage("blocked-"+secret, WSUpstreamSend, secret)}, 1)
	check := func(err error) {
		t.Helper()
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Fatal("调度错误缺失或回显敏感输入")
		}
	}
	check(s.Start("unknown-" + secret))
	check(s.Complete(secret))
	check(s.Start("blocked-" + secret))
	if err := s.Start(secret); err != nil {
		t.Fatal(err)
	}
	check(s.Start(secret))
	if err := s.Complete(secret); err != nil {
		t.Fatal(err)
	}
	check(s.Complete(secret))
	check(s.Start(secret))
	_, err := NewWSSchedule([]WSNode{wsScheduleMessage(secret, WSClientSend, "missing-"+secret)}, 1)
	check(err)
}

func wsScheduleMessage(id string, point WSPoint, after ...string) WSNode {
	return WSNode{ID: id, Point: point, Source: "synthetic", After: after, Kind: "message", Message: &WSMessage{Opcode: ws.OpText, Payload: []byte(`{"type":"event"}`)}}
}

func mustWSSchedule(t *testing.T, nodes []WSNode, seed uint64) *WSSchedule {
	t.Helper()
	s, err := NewWSSchedule(nodes, seed)
	if err != nil {
		t.Fatal(err)
	}
	if s == nil {
		t.Fatal("没有错误却返回 nil 调度器")
	}
	return s
}

func startCompleteWSNode(t *testing.T, s *WSSchedule, id string) {
	t.Helper()
	if err := s.Start(id); err != nil {
		t.Fatal(err)
	}
	if err := s.Complete(id); err != nil {
		t.Fatal(err)
	}
}

func wsScheduleSequence(t *testing.T, nodes []WSNode, seed uint64) []string {
	t.Helper()
	s := mustWSSchedule(t, nodes, seed)
	var sequence []string
	for !s.Done() {
		ready := s.Ready()
		if len(ready) == 0 || len(sequence) >= len(nodes) {
			t.Fatal("纯调度悬停或重复消费")
		}
		sequence = append(sequence, ready[0].ID)
		startCompleteWSNode(t, s, ready[0].ID)
	}
	return sequence
}

func assertWSReady(t *testing.T, s *WSSchedule, want ...string) {
	t.Helper()
	var got []string
	for _, node := range s.Ready() {
		got = append(got, node.ID)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Ready=%v，预期 %v", got, want)
	}
}

func assertWSReadySet(t *testing.T, s *WSSchedule, want ...string) {
	t.Helper()
	var got []string
	for _, node := range s.Ready() {
		got = append(got, node.ID)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("Ready 集合=%v，预期 %v", got, want)
	}
}

func wsScheduleSnapshotNodes() []WSNode {
	code := uint16(1000)
	return []WSNode{
		{ID: "root", Point: WSClientSend, Source: "synthetic", Kind: "message", Message: &WSMessage{Opcode: ws.OpText, Payload: []byte(`{"type":"session.update","value":1}`)}, Match: "json", Fields: []WSFieldRule{{Pointer: "/value", Mode: "equal", Value: json.RawMessage(`1`)}}},
		{ID: "child", Point: WSUpstreamReceive, Source: "synthetic", After: []string{"root"}, Kind: "message", Message: &WSMessage{Opcode: ws.OpBinary, Payload: []byte{}}, ForwardedFrom: "root"},
		{ID: "close", Point: WSClientSend, Source: "synthetic", Kind: "close", CloseCode: &code, CloseReason: "complete"},
	}
}
