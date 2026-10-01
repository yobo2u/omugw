package testkit

import (
	"bytes"
	"fmt"
	"slices"
	"sort"
)

// WSSchedule 仅由回放控制器串行调用，避免接收工作者直接竞逐调度状态。
// 它只确认节点已消费，不把发送结束当业务成功，也不承诺网络到达次序可重放。
type WSSchedule struct {
	graph     *wsGraph
	nodes     []WSNode
	progress  []wsNodeProgress
	remaining []int
	children  [][]int
	ready     []int
	rng       wsScheduleRNG
	completed int
	ordering  string
}

type wsNodeProgress uint8

const (
	wsNodePending wsNodeProgress = iota
	wsNodeInFlight
	wsNodeCompleted
)

// NewWSSchedule 复用静态图校验，防止调度与 fixture 对隐式 FIFO 各持一套因果规则。
// 消息与来源契约仍由 ValidateWSSession 守门，此处只校验和冻结调度所需的图。
func NewWSSchedule(nodes []WSNode, seed uint64) (*WSSchedule, error) {
	limits := DefaultWSLimits()
	if len(nodes) > limits.Nodes {
		return nil, fmt.Errorf("WS 调度节点超过预算")
	}
	graph, err := newWSGraph(nodes, limits.Edges)
	if err != nil {
		return nil, err
	}
	s := &WSSchedule{
		graph: graph, nodes: make([]WSNode, len(nodes)),
		progress: make([]wsNodeProgress, len(nodes)), remaining: make([]int, len(nodes)),
		children: make([][]int, len(nodes)), rng: wsScheduleRNG(seed),
	}
	for i, node := range nodes {
		s.nodes[i] = cloneWSScheduleNode(node)
		s.remaining[i] = len(graph.parents[i])
		for _, parent := range graph.parents[i] {
			s.children[parent] = append(s.children[parent], i)
		}
		if s.remaining[i] == 0 {
			s.ready = append(s.ready, i)
		}
	}
	s.ordering = wsScheduleOrdering(s.remaining, s.children)
	s.orderReady()
	return s, nil
}

// Ready 不消耗随机状态且深拷贝嵌套负载，防止轮询次数或调用者改写改变同种子调度。
func (s *WSSchedule) Ready() []WSNode {
	if s == nil || s.graph == nil {
		return nil
	}
	nodes := make([]WSNode, len(s.ready))
	for i, index := range s.ready {
		nodes[i] = cloneWSScheduleNode(s.nodes[index])
	}
	return nodes
}

// Start 只移出就绪集合，不释放后继；Write 尚未成功不能被当作因果完成。
func (s *WSSchedule) Start(id string) error {
	if s == nil || s.graph == nil {
		return fmt.Errorf("WS 调度器未初始化")
	}
	i, exists := s.graph.index[id]
	if !exists {
		return fmt.Errorf("WS 调度节点不存在")
	}
	if s.progress[i] != wsNodePending || s.remaining[i] != 0 {
		return fmt.Errorf("WS 调度节点未就绪或已开始")
	}
	for position, index := range s.ready {
		if index == i {
			s.ready = slices.Delete(s.ready, position, position+1)
			break
		}
	}
	s.progress[i] = wsNodeInFlight
	return nil
}

func (s *WSSchedule) InFlight(id string) bool {
	if s == nil || s.graph == nil {
		return false
	}
	i, exists := s.graph.index[id]
	return exists && s.progress[i] == wsNodeInFlight
}

// Complete 不能跳过 Start 或重复消费，防止失败发送被重试、重复边被多次释放。
// 失败与取消由驱动终结回放，不提供任何回退至 pending 的重发入口。
func (s *WSSchedule) Complete(id string) error {
	if s == nil || s.graph == nil {
		return fmt.Errorf("WS 调度器未初始化")
	}
	i, exists := s.graph.index[id]
	if !exists {
		return fmt.Errorf("WS 调度节点不存在")
	}
	if s.progress[i] != wsNodeInFlight {
		return fmt.Errorf("WS 调度节点尚未开始或已完成")
	}
	s.progress[i] = wsNodeCompleted
	s.completed++
	added := false
	for _, child := range s.children[i] {
		s.remaining[child]--
		if s.remaining[child] == 0 {
			s.ready = append(s.ready, child)
			added = true
		}
	}
	if added {
		s.orderReady()
	}
	return nil
}

// Done 必须等接收与 close 等全部节点消费，不能只用发送列表耗尽来判成功。
func (s *WSSchedule) Done() bool {
	return s != nil && s.graph != nil && s.completed == len(s.nodes)
}

// Ordering 只报告原图为 serial 或 multiple，不把已探索的种子数量冒充拓扑总数。
func (s *WSSchedule) Ordering() string {
	if s == nil || s.graph == nil {
		return ""
	}
	return s.ordering
}

func (s *WSSchedule) orderReady() {
	sort.Slice(s.ready, func(i, j int) bool {
		return s.nodes[s.ready[i]].ID < s.nodes[s.ready[j]].ID
	})
	for i := len(s.ready) - 1; i > 0; i-- {
		j := int(s.rng.next() % uint64(i+1))
		s.ready[i], s.ready[j] = s.ready[j], s.ready[i]
	}
}

// SplitMix64 的固定整数步骤让零种子与平台无关，不借用全局随机源或墙上时钟。
type wsScheduleRNG uint64

func (r *wsScheduleRNG) next() uint64 {
	*r += 0x9e3779b97f4a7c15
	value := uint64(*r)
	value = (value ^ (value >> 30)) * 0xbf58476d1ce4e5b9
	value = (value ^ (value >> 27)) * 0x94d049bb133111eb
	return value ^ (value >> 31)
}

// 合法 DAG 的 Kahn 集合一旦同时有两个节点，就有分支；无需指数枚举全部次序。
func wsScheduleOrdering(remaining []int, children [][]int) string {
	counts := slices.Clone(remaining)
	var ready []int
	for i, count := range counts {
		if count == 0 {
			ready = append(ready, i)
		}
	}
	for head := 0; head < len(ready); head++ {
		if len(ready)-head > 1 {
			return "multiple"
		}
		for _, child := range children[ready[head]] {
			counts[child]--
			if counts[child] == 0 {
				ready = append(ready, child)
			}
		}
	}
	return "serial"
}

func cloneWSScheduleNode(node WSNode) WSNode {
	node.After = slices.Clone(node.After)
	if node.Message != nil {
		message := *node.Message
		message.Payload = bytes.Clone(message.Payload)
		node.Message = &message
	}
	if node.CloseCode != nil {
		code := *node.CloseCode
		node.CloseCode = &code
	}
	node.Fields = slices.Clone(node.Fields)
	for i := range node.Fields {
		node.Fields[i].Value = bytes.Clone(node.Fields[i].Value)
	}
	return node
}
