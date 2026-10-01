package testkit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"sync"

	"github.com/yobo2u/omugw/internal/transport/ws"
)

// WSReplayEndpoints 的 Client 必须是下游客户端角色，Upstream 必须是上游服务端角色。
// 调用方必须在 Dial/Accept 时设置 MaxPayload=limits.MessageBytes；Conn 无 getter，
// 驱动不能在分配负载后才补验预算，也不接管握手失败的 HTTP 证据。
type WSReplayEndpoints struct {
	Client   *ws.Conn
	Upstream *ws.Conn
}

// WSReplayResult 只在全部实际证据通过后给出结局；Nodes 按 fixture 顺序及符号归一，
// SendOrder 仅记录控制器发令次序；seed 只固定就绪集合策略，完整次序仍依赖读写反馈，
// 不把网络到达时间或任意轨迹的完整历史伪装成可复现调度。
type WSReplayResult struct {
	Nodes     []WSNode
	SendOrder []string
	Outcome   WSOutcome
}

type wsReplayRead struct {
	message WSMessage
	err     error
}

type wsReplayWrite struct {
	index   int
	message WSMessage
}

type wsReplayWritten struct {
	job       wsReplayWrite
	closeSent bool
	err       error
}

type wsReplayEndpoint struct {
	conn       *ws.Conn
	reads      chan wsReplayRead
	writes     chan wsReplayWrite
	written    chan wsReplayWritten
	pending    *wsReplayRead
	inFlight   int
	readEnded  bool
	localClose bool
	receives   []int
	next       int
}

type wsReplayController struct {
	nodes     []WSNode
	graph     *wsGraph
	schedule  *WSSchedule
	matcher   *WSMatcher
	limits    WSLimits
	endpoints [2]*wsReplayEndpoint
	completed []bool
	observed  []*WSMessage
	trace     int64
	order     []string
}

// ReplayWS 成功初始化后消费两端 socket：成功、错误、取消均关闭并 join 全部工作者。
// 初始化失败不接管资源，调用方仍须释放端点；本驱动不决定供应商的 VAD 或用量政策。
func ReplayWS(ctx context.Context, f Fixture, peers WSReplayEndpoints, seed uint64, limits WSLimits) (WSReplayResult, error) {
	if ctx == nil || peers.Client == nil || peers.Upstream == nil || peers.Client == peers.Upstream {
		return WSReplayResult{}, fmt.Errorf("WS 回放需要独立双端与 context")
	}
	if err := limits.validate(); err != nil {
		return WSReplayResult{}, fmt.Errorf("WS 回放预算不合法")
	}
	if err := ValidateWSSession(f, limits); err != nil {
		// 校验器可能携带作者填写的 name/headers，不能直接进入诊断。
		return WSReplayResult{}, fmt.Errorf("WS 回放 fixture 不合法")
	}
	session := f.Response.WS
	if session.Outcome.Kind == "handshake_failed" {
		return WSReplayResult{}, fmt.Errorf("WS 回放端点不能证明 HTTP 握手失败")
	}
	if session.Outcome.Kind == "interrupted" && !hasWSReplayCloseReceive(session.Nodes) {
		return WSReplayResult{}, fmt.Errorf("WS interrupted 缺少接收关闭证据")
	}
	schedule, err := NewWSSchedule(session.Nodes, seed)
	if err != nil {
		return WSReplayResult{}, fmt.Errorf("WS 回放因果图不合法")
	}
	matcher, err := NewWSMatcher(limits)
	if err != nil {
		return WSReplayResult{}, fmt.Errorf("WS 回放 matcher 初始化失败")
	}
	c := &wsReplayController{
		nodes: schedule.nodes, graph: schedule.graph, schedule: schedule, matcher: matcher,
		limits: limits, completed: make([]bool, len(session.Nodes)), observed: make([]*WSMessage, len(session.Nodes)),
	}
	// 样本已经加载且校验计入静态总量；实际轨迹不能把这部分内存占用重新清零。
	for _, sample := range session.Samples {
		if err := addWSTraceBytes(&c.trace, int64(len(sample.Data)), limits.TraceBytes); err != nil {
			return WSReplayResult{}, fmt.Errorf("WS 回放样本超过轨迹预算")
		}
	}
	for i, conn := range []*ws.Conn{peers.Client, peers.Upstream} {
		c.endpoints[i] = &wsReplayEndpoint{conn: conn, reads: make(chan wsReplayRead, 1),
			writes: make(chan wsReplayWrite, 1), written: make(chan wsReplayWritten, 1), inFlight: -1}
	}
	for i, n := range c.nodes {
		if n.Point == WSClientReceive || n.Point == WSUpstreamReceive {
			ep := c.endpoint(n.Point)
			ep.receives = append(ep.receives, i)
		}
	}
	// WithTimeout 自动取 parent 与本地预算的更早者，不给已有 deadline 延寿。
	run, cancel := context.WithTimeout(ctx, limits.Replay)
	var workers sync.WaitGroup
	for _, ep := range c.endpoints {
		workers.Add(3)
		go ep.read(run, &workers)
		go ep.write(run, c.nodes, &workers)
		go func(ep *wsReplayEndpoint) {
			defer workers.Done()
			<-run.Done()
			// 不等 controller 或 writer 归还锁，Conn.Close 会直接唤醒阻塞业务写。
			_ = ep.conn.Close(ws.CloseNormal, "")
		}(ep)
	}
	defer func() { cancel(); workers.Wait() }()

	if err := c.drive(run); err != nil {
		return WSReplayResult{}, err
	}
	if err := c.verifyOutcome(session.Outcome); err != nil {
		return WSReplayResult{}, err
	}
	if err := run.Err(); err != nil {
		return WSReplayResult{}, fmt.Errorf("WS 回放已取消: %w", err)
	}
	result := WSReplayResult{SendOrder: slices.Clone(c.order), Outcome: WSOutcome{
		Kind: session.Outcome.Kind, Terminal: slices.Clone(session.Outcome.Terminal)}, Nodes: make([]WSNode, len(c.nodes))}
	for i, n := range c.nodes {
		// 原始值已经匹配、转发比对及终态检查；此处只归一证据，不补造未收到的消息。
		result.Nodes[i] = cloneWSScheduleNode(n)
	}
	return result, nil
}

func (ep *wsReplayEndpoint) read(ctx context.Context, workers *sync.WaitGroup) {
	defer workers.Done()
	for {
		op, payload, err := ep.conn.ReadMessage()
		select {
		case ep.reads <- wsReplayRead{message: WSMessage{Opcode: op, Payload: payload}, err: err}:
		case <-ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}

func (ep *wsReplayEndpoint) write(ctx context.Context, nodes []WSNode, workers *sync.WaitGroup) {
	defer workers.Done()
	for {
		var job wsReplayWrite
		select {
		case <-ctx.Done():
			return
		case job = <-ep.writes:
		}
		if ctx.Err() != nil {
			return
		}
		n := nodes[job.index]
		var err error
		var closeSent bool
		if n.Kind == "close" {
			closeSent, err = ep.conn.CloseWithResult(*n.CloseCode, n.CloseReason)
		} else {
			err = ep.conn.WriteMessage(job.message.Opcode, job.message.Payload)
		}
		select {
		case ep.written <- wsReplayWritten{job: job, closeSent: closeSent, err: err}:
		case <-ctx.Done():
			return
		}
	}
}

func (c *wsReplayController) endpoint(point WSPoint) *wsReplayEndpoint {
	if point == WSClientSend || point == WSClientReceive {
		return c.endpoints[0]
	}
	return c.endpoints[1]
}

func (c *wsReplayController) drive(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("WS 回放已取消: %w", err)
		}
		if err := c.consumePending(); err != nil {
			return err
		}
		if err := c.dispatch(ctx); err != nil {
			return err
		}
		if c.schedule.Done() && c.endpoints[0].readEnded && c.endpoints[1].readEnded {
			return nil
		}
		// 每端仅一个待消费槽；槽未释放时停止领取该端读 channel，形成有界背压。
		var reads [2]<-chan wsReplayRead
		for i, ep := range c.endpoints {
			if ep.pending == nil && !ep.readEnded {
				reads[i] = ep.reads
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("WS 回放已取消: %w", ctx.Err())
		case read := <-reads[0]:
			c.endpoints[0].pending = &read
		case read := <-reads[1]:
			c.endpoints[1].pending = &read
		case result := <-c.endpoints[0].written:
			if err := c.finishWrite(c.endpoints[0], result); err != nil {
				return err
			}
		case result := <-c.endpoints[1].written:
			if err := c.finishWrite(c.endpoints[1], result); err != nil {
				return err
			}
		}
	}
}

func (c *wsReplayController) dispatch(ctx context.Context) error {
	for _, n := range c.schedule.Ready() {
		if n.Point != WSClientSend && n.Point != WSUpstreamSend {
			continue
		}
		ep := c.endpoint(n.Point)
		if ep.inFlight != -1 {
			continue
		}
		i := c.graph.index[n.ID]
		job := wsReplayWrite{index: i}
		if n.Kind == "message" {
			var err error
			job.message, err = c.matcher.Materialize(n)
			if err != nil {
				return fmt.Errorf("ws.nodes[%d].message: %w", i, err)
			}
		}
		if err := c.schedule.Start(n.ID); err != nil {
			return fmt.Errorf("ws.nodes[%d]: 发送调度失败", i)
		}
		ep.inFlight = i
		c.order = append(c.order, n.ID)
		select {
		case ep.writes <- job:
		case <-ctx.Done():
			return fmt.Errorf("WS 回放已取消: %w", ctx.Err())
		}
	}
	return nil
}

func (c *wsReplayController) finishWrite(ep *wsReplayEndpoint, result wsReplayWritten) error {
	i := result.job.index
	if i != ep.inFlight || !c.schedule.InFlight(c.nodes[i].ID) {
		return fmt.Errorf("ws.nodes[%d]: 发送完成状态不符", i)
	}
	if result.err != nil {
		// CloseError 与底层网络错误会带 reason/地址，不能回显。
		return fmt.Errorf("ws.nodes[%d]: 发送失败", i)
	}
	n := c.nodes[i]
	if n.Kind == "message" {
		if err := c.matchMessage(i, result.job.message); err != nil {
			return err
		}
	} else {
		// 已关闭或写锁占用时的幂等 nil 不能记账、放行 EOF 或完成本次发送。
		if !result.closeSent {
			return fmt.Errorf("ws.nodes[%d].close_code: 本次没有实际发送关闭帧", i)
		}
		if err := c.matchCloseBytes(i, n.CloseReason); err != nil {
			return err
		}
		ep.localClose = true
	}
	ep.inFlight = -1
	return c.complete(i)
}

func (c *wsReplayController) complete(i int) error {
	if err := c.schedule.Complete(c.nodes[i].ID); err != nil {
		return fmt.Errorf("ws.nodes[%d]: 完成调度失败", i)
	}
	c.completed[i] = true
	return nil
}

func (c *wsReplayController) consumePending() error {
	for {
		progress := false
		for _, ep := range c.endpoints {
			read := ep.pending
			if read == nil {
				continue
			}
			i := len(c.nodes)
			if ep.next < len(ep.receives) {
				i = ep.receives[ep.next]
			}
			// 主动 Close 马上关 TCP，不能要求本端看到自动 1000 回应，亦不能拿其
			// EOF 消费任何尚未匹配的 receive 节点。先等 close 写结果，不误判抢先 EOF。
			if read.err != nil && ep.next == len(ep.receives) && ep.inFlight != -1 && c.nodes[ep.inFlight].Kind == "close" {
				continue
			}
			if read.err != nil && ep.localClose && ep.next == len(ep.receives) {
				var reply *ws.CloseError
				if errors.As(read.err, &reply) {
					if reply.Code != ws.CloseNormal || reply.Reason != "" {
						return fmt.Errorf("ws.nodes[%d].close_code: 主动端收到异常关闭", i)
					}
				} else if !errors.Is(read.err, net.ErrClosed) && !errors.Is(read.err, io.EOF) {
					return fmt.Errorf("ws.nodes[%d]: 主动端接收器失败", i)
				}
				ep.pending = nil
				ep.readEnded = true
				progress = true
				continue
			}
			if i == len(c.nodes) {
				return fmt.Errorf("ws.nodes[%d]: 尾部额外消息或未声明的关闭", i)
			}
			n := c.nodes[i]
			var closed *ws.CloseError
			if read.err != nil && !errors.As(read.err, &closed) {
				return fmt.Errorf("ws.nodes[%d]: 接收失败，不能把 TCP EOF 或超时当 close", i)
			}
			if n.Kind == "close" {
				if closed == nil || closed.Code != *n.CloseCode || closed.Reason != n.CloseReason {
					return fmt.Errorf("ws.nodes[%d].close_code: 关闭证据不符或存在额外消息", i)
				}
			} else if read.err != nil {
				return fmt.Errorf("ws.nodes[%d].message: 消息缺失或提前关闭", i)
			}
			ready := false
			for _, available := range c.schedule.Ready() {
				if available.ID == n.ID {
					ready = true
					break
				}
			}
			if !ready {
				// 只宽限已发令发送的 write-result 竞争；未来接收不能补造先前消息的因果。
				if !c.onlySendingAncestorsInFlight(i) {
					return fmt.Errorf("ws.nodes[%d]: 接收早于必要因果证据", i)
				}
				continue
			}
			if err := c.schedule.Start(n.ID); err != nil {
				return fmt.Errorf("ws.nodes[%d]: 接收调度失败", i)
			}
			if n.Kind == "message" {
				if err := c.matchMessage(i, read.message); err != nil {
					return err
				}
			} else {
				if err := c.matchCloseBytes(i, closed.Reason); err != nil {
					return err
				}
				ep.readEnded = true
			}
			if err := c.complete(i); err != nil {
				return err
			}
			ep.next++
			ep.pending = nil
			progress = true
		}
		if !progress {
			return nil
		}
	}
}

func (c *wsReplayController) onlySendingAncestorsInFlight(i int) bool {
	seen := make([]bool, len(c.nodes))
	pending := slices.Clone(c.graph.parents[i])
	for len(pending) != 0 {
		parent := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if seen[parent] || c.completed[parent] {
			continue
		}
		seen[parent] = true
		n := c.nodes[parent]
		if (n.Point != WSClientSend && n.Point != WSUpstreamSend) || !c.schedule.InFlight(n.ID) {
			return false
		}
		pending = append(pending, c.graph.parents[parent]...)
	}
	return true
}

// 关闭负载也占轨迹预算，防止动态 ID 恰好耗尽消息预算后仍放行额外关闭字节。
func (c *wsReplayController) matchCloseBytes(i int, reason string) error {
	if err := addWSTraceBytes(&c.trace, int64(2+len(reason)), c.limits.TraceBytes); err != nil {
		return fmt.Errorf("ws.nodes[%d].close_code: 实际轨迹超过预算", i)
	}
	return nil
}

func (c *wsReplayController) matchMessage(i int, actual WSMessage) error {
	if err := addWSTraceBytes(&c.trace, int64(len(actual.Payload)), c.limits.TraceBytes); err != nil {
		return fmt.Errorf("ws.nodes[%d].message.payload: 实际轨迹超过预算", i)
	}
	if err := c.matcher.Match(c.nodes[i], actual); err != nil {
		return fmt.Errorf("ws.nodes[%d].message.payload: %w", i, err)
	}
	if from := c.nodes[i].ForwardedFrom; from != "" {
		origin := c.observed[c.graph.index[from]]
		if origin == nil {
			return fmt.Errorf("ws.nodes[%d].forwarded_from: 缺少已确认来源", i)
		}
		// bind/reference 只能验证关联，不能豁免字节保全；仅 equal 声明改写 span。
		var rewrites []WSFieldRule
		for _, field := range c.nodes[i].Fields {
			if field.Mode == "equal" {
				rewrites = append(rewrites, field)
			}
		}
		if err := AssertWSForwardedPayload(*origin, actual, rewrites); err != nil {
			return fmt.Errorf("ws.nodes[%d].message.payload: 转发字节或声明改写不符", i)
		}
	}
	c.observed[i] = &actual
	return nil
}

func hasWSReplayCloseReceive(nodes []WSNode) bool {
	for _, n := range nodes {
		if n.Kind == "close" && (n.Point == WSClientReceive || n.Point == WSUpstreamReceive) {
			return true
		}
	}
	return false
}

func (c *wsReplayController) verifyOutcome(outcome WSOutcome) error {
	if !c.schedule.Done() {
		return fmt.Errorf("WS 回放尚未收齐全部证据")
	}
	if (outcome.Kind == "completed" || outcome.Kind == "failed") && !wsObservableClosePair(c.nodes, c.graph, outcome.Kind == "failed") {
		return fmt.Errorf("WS 回放缺少匹配的跨端关闭证据")
	}
	for index, terminal := range outcome.Terminal {
		i, exists := c.graph.index[terminal.Node]
		if !exists || !c.completed[i] || c.observed[i] == nil || c.nodes[i].Point != WSClientReceive {
			return fmt.Errorf("ws.outcome.terminal[%d].node: 缺少匹配的接收证据", index)
		}
		value, err := strictWSJSON(c.observed[i].Payload, c.limits.JSONDepth)
		if err != nil {
			return fmt.Errorf("ws.outcome.terminal[%d]: 实际终态不是严格 JSON", index)
		}
		id, idOK := wsJSONPointer(value, terminal.IDPointer)
		state, stateOK := wsJSONPointer(value, terminal.StatePointer)
		wantedID := terminal.Symbol
		if rule := wsRuleAt(c.nodes[i].Fields, terminal.IDPointer); rule != nil && (rule.Mode == "bind" || rule.Mode == "reference") {
			c.matcher.mu.Lock()
			bound, ok := c.matcher.bindings[wsBinding{terminal.Namespace, terminal.Symbol}]
			c.matcher.mu.Unlock()
			if !ok || rule.Namespace != terminal.Namespace || rule.Symbol != terminal.Symbol {
				return fmt.Errorf("ws.outcome.terminal[%d].id_pointer: 实体尚未确认", index)
			}
			wantedID = bound
		}
		if !idOK || id != wantedID {
			return fmt.Errorf("ws.outcome.terminal[%d].id_pointer: 实际实体不符", index)
		}
		if !stateOK || state != terminal.State {
			return fmt.Errorf("ws.outcome.terminal[%d].state_pointer: 实际业务状态不符", index)
		}
	}
	return nil
}
