package testkit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

// ValidateWSSession 只验证离线契约自洽，不把来源标签升级为真实云端能力证据。
func ValidateWSSession(f Fixture, limits WSLimits) error {
	if err := limits.validate(); err != nil {
		return err
	}
	if f.Response.WS == nil {
		return fmt.Errorf("缺少 WS 会话")
	}
	if strings.TrimSpace(f.Name) == "" || strings.TrimSpace(f.Note) == "" {
		return fmt.Errorf("WS fixture 缺少 name 或 note")
	}
	if err := f.validateWSEnvelope(); err != nil {
		return err
	}
	s := f.Response.WS
	if s.Version != 1 || strings.TrimSpace(s.ClientProtocol) == "" || strings.TrimSpace(s.ClientVersion) == "" {
		return fmt.Errorf("WS schema 或客户端契约版本不合法")
	}
	for _, headers := range []map[string]string{f.Request.Headers, s.Upstream.Headers, f.Response.Headers} {
		if err := validateSecretHeaders(f.Name, "WS 握手头", headers); err != nil {
			return err
		}
	}
	if s.Outcome.Kind == "handshake_failed" {
		if f.Response.Status < 200 || f.Response.Status > 599 || s.UpstreamExpectedStatus < 200 || s.UpstreamExpectedStatus > 599 ||
			len(s.Nodes) != 0 || len(s.Outcome.Terminal) != 0 {
			return fmt.Errorf("握手失败必须声明 HTTP 错误状态且不含应用轨迹或终态")
		}
		value, err := strictWSJSON(s.UpstreamError, limits.JSONDepth)
		if err != nil {
			return fmt.Errorf("握手失败缺少合法字面错误对象")
		}
		if object, ok := value.(map[string]any); !ok || len(object) == 0 {
			return fmt.Errorf("握手错误必须为非空对象")
		}
	} else {
		if s.UpstreamError != nil {
			return fmt.Errorf("成功握手不能带错误对象")
		}
		if s.Outcome.Kind != "completed" && s.Outcome.Kind != "failed" && s.Outcome.Kind != "interrupted" {
			return fmt.Errorf("WS outcome 不合法")
		}
	}
	if len(s.Nodes) > limits.Nodes {
		return fmt.Errorf("WS 节点超过预算")
	}
	graph, err := newWSGraph(s.Nodes, limits.Edges)
	if err != nil {
		return err
	}
	payloads := make([]any, len(s.Nodes))
	closePoints := make(map[WSPoint]bool)
	var traceBytes int64
	rules := 0
	bindings := make(map[wsBinding]int)
	sentIDs := make(map[wsActualID]wsBinding)
	for i, n := range s.Nodes {
		var prepared []wsMatchRule
		if strings.TrimSpace(n.Source) == "" {
			return fmt.Errorf("WS 节点缺少来源")
		}
		if closePoints[n.Point] {
			return fmt.Errorf("WS 同点 close 后不能再有节点")
		}
		switch n.Kind {
		case "message":
			if n.Message == nil || n.Message.Payload == nil || n.CloseCode != nil || n.CloseReason != "" {
				return fmt.Errorf("WS message 必须仅有消息负载")
			}
			if n.Message.Opcode != ws.OpText && n.Message.Opcode != ws.OpBinary {
				return fmt.Errorf("WS message 必须为完整 text 或 binary")
			}
			if int64(len(n.Message.Payload)) > limits.MessageBytes {
				return fmt.Errorf("WS 消息超过解码预算")
			}
			if err := addWSTraceBytes(&traceBytes, int64(len(n.Message.Payload)), limits.TraceBytes); err != nil {
				return err
			}
			if n.Message.Opcode == ws.OpText && (len(n.Message.Payload) == 0 || !utf8.Valid(n.Message.Payload)) {
				return fmt.Errorf("WS text 必须为非空 UTF8")
			}
			// 与消费者共用无状态预检，防止 loader 交付运行时必然拒绝的字段契约。
			payloads[i], prepared, err = prepareWSMatchNode(n, limits)
			if err != nil {
				return err
			}
		case "close":
			if n.ForwardedFrom != "" {
				return fmt.Errorf("WS schema v1 forwarded_from 仅支持 message")
			}
			if n.CloseCode == nil || n.Message != nil || len(n.Fields) != 0 || n.Match != "" {
				return fmt.Errorf("WS close 必须仅有关闭负载")
			}
			if !validWSCloseCode(*n.CloseCode) || !utf8.ValidString(n.CloseReason) || len(n.CloseReason) > 123 {
				return fmt.Errorf("WS close code 或 reason 不合法")
			}
			if err := addWSTraceBytes(&traceBytes, int64(2+len(n.CloseReason)), limits.TraceBytes); err != nil {
				return err
			}
			closePoints[n.Point] = true
		default:
			return fmt.Errorf("WS 节点 kind 不合法")
		}
		if len(n.Fields) > limits.FieldRules-rules {
			return fmt.Errorf("WS 字段规则超过预算")
		}
		rules += len(n.Fields)
		for _, rule := range prepared {
			field := rule.field
			if field.Mode != "bind" {
				continue
			}
			key := wsBinding{field.Namespace, field.Symbol}
			if _, exists := bindings[key]; exists {
				return fmt.Errorf("WS 实体不能重复绑定")
			}
			if len(bindings) >= limits.Bindings {
				return fmt.Errorf("WS 绑定超过预算")
			}
			bindings[key] = i
			if n.Point == WSClientSend || n.Point == WSUpstreamSend {
				// 接收占位不是真实 ID；仅作者将原样发送的字面值可提前证明冲突。
				literal, _ := wsJSONPointer(payloads[i], field.Pointer)
				actual := wsActualID{key.namespace, literal.(string)}
				if owner, exists := sentIDs[actual]; exists && owner != key {
					return fmt.Errorf("WS 发送字面 ID 在同命名空间不能复用")
				}
				sentIDs[actual] = key
			}
		}
	}
	// 先建立全图再检查引用，避免文件顺序被误用为跨流因果关系。
	for i, n := range s.Nodes {
		for _, field := range n.Fields {
			if field.Mode == "reference" {
				bound, ok := bindings[wsBinding{field.Namespace, field.Symbol}]
				if !ok || !graph.precedes(bound, i) {
					return fmt.Errorf("WS reference 缺少因果在先的绑定")
				}
			}
		}
		if n.ForwardedFrom != "" {
			from, ok := graph.index[n.ForwardedFrom]
			if !ok || !graph.precedes(from, i) {
				return fmt.Errorf("转发来源必须存在且因果在先")
			}
			origin := s.Nodes[from]
			if origin.Kind != n.Kind || !((origin.Point == WSClientSend && n.Point == WSUpstreamReceive) || (origin.Point == WSUpstreamSend && n.Point == WSClientReceive)) {
				return fmt.Errorf("转发来源点或 kind 不合法")
			}
		}
	}
	capabilities := make(map[string]bool)
	for _, c := range canonical.AllCapabilities() {
		capabilities[string(c)] = true
	}
	for _, coverage := range s.Coverage {
		if !capabilities[coverage.Capability] || len(coverage.Nodes) == 0 || strings.TrimSpace(coverage.Note) == "" {
			return fmt.Errorf("WS coverage 缺少合法能力、节点或说明")
		}
		for _, id := range coverage.Nodes {
			if _, ok := graph.index[id]; !ok {
				return fmt.Errorf("WS coverage 引用不存在")
			}
		}
	}
	if s.Outcome.Kind == "completed" {
		for _, n := range s.Nodes {
			if n.CloseCode != nil && *n.CloseCode != 1000 {
				return fmt.Errorf("completed 必须预期 1000 close")
			}
		}
		if !wsObservableClosePair(s.Nodes, graph, false) {
			return fmt.Errorf("completed 缺少主动发送与对侧接收 close 对")
		}
	}
	if s.Outcome.Kind == "completed" && len(s.Outcome.Terminal) == 0 {
		return fmt.Errorf("completed 缺少业务终态")
	}
	if (s.Outcome.Kind == "interrupted" || s.Outcome.Kind == "handshake_failed") && len(s.Outcome.Terminal) != 0 {
		return fmt.Errorf("该结局不能声明业务终态")
	}
	if s.Outcome.Kind == "interrupted" && len(closePoints) == 0 {
		return fmt.Errorf("interrupted 必须预期有效 close，不能用 raw EOF 充当结局")
	}
	if s.Outcome.Kind == "failed" && !wsObservableClosePair(s.Nodes, graph, true) {
		return fmt.Errorf("failed 缺少非 1000 的主动发送与对侧接收 close 对")
	}
	for _, terminal := range s.Outcome.Terminal {
		i, ok := graph.index[terminal.Node]
		if !ok || s.Nodes[i].Kind != "message" || s.Nodes[i].Point != WSClientReceive || terminal.Namespace == "" || terminal.Symbol == "" || terminal.State == "" {
			return fmt.Errorf("业务终态必须来自客户端接收消息且声明实体与状态")
		}
		if payloads[i] == nil && (s.Nodes[i].Match == "" || s.Nodes[i].Match == "bytes") {
			payloads[i], err = strictWSJSON(s.Nodes[i].Message.Payload, limits.JSONDepth)
			if err != nil {
				return fmt.Errorf("业务终态需要合法 JSON 字面预期")
			}
		}
		id, idOK := wsJSONPointer(payloads[i], terminal.IDPointer)
		state, stateOK := wsJSONPointer(payloads[i], terminal.StatePointer)
		if !idOK || !stateOK {
			return fmt.Errorf("业务终态 pointer 不存在或不合法")
		}
		idRule := wsRuleAt(s.Nodes[i].Fields, terminal.IDPointer)
		if idRule != nil && (idRule.Mode == "bind" || idRule.Mode == "reference") {
			if idRule.Namespace != terminal.Namespace || idRule.Symbol != terminal.Symbol {
				return fmt.Errorf("业务终态实体与绑定不符")
			}
		} else {
			if idRule != nil {
				id, err = strictWSJSON(idRule.Value, limits.JSONDepth)
				if err != nil {
					return err
				}
			}
			if id != terminal.Symbol {
				return fmt.Errorf("业务终态实体与字面预期不符")
			}
		}
		if rule := wsRuleAt(s.Nodes[i].Fields, terminal.StatePointer); rule != nil && rule.Mode == "equal" {
			state, err = strictWSJSON(rule.Value, limits.JSONDepth)
			if err != nil {
				return err
			}
		}
		if state != terminal.State {
			return fmt.Errorf("业务终态状态与字面预期不符")
		}
	}
	return validateWSProvenance(*s, limits, &traceBytes)
}

// 主动关闭立即释放 TCP，只检查另一端实际收到的同码关闭，防止要求无法观测的自动回应。
func wsObservableClosePair(nodes []WSNode, graph *wsGraph, failed bool) bool {
	closes := make(map[WSPoint]int)
	for i, n := range nodes {
		if n.Kind == "close" {
			closes[n.Point] = i
		}
	}
	for _, pair := range [][2]WSPoint{{WSClientSend, WSUpstreamReceive}, {WSUpstreamSend, WSClientReceive}} {
		send, sendOK := closes[pair[0]]
		receive, receiveOK := closes[pair[1]]
		if !sendOK || !receiveOK {
			continue
		}
		code := *nodes[send].CloseCode
		if code == *nodes[receive].CloseCode && (code != 1000) == failed && graph.precedes(send, receive) {
			return true
		}
	}
	return false
}

type wsBinding struct{ namespace, symbol string }

func wsRuleAt(fields []WSFieldRule, pointer string) *WSFieldRule {
	for i := range fields {
		if fields[i].Pointer == pointer {
			return &fields[i]
		}
	}
	return nil
}

func addWSTraceBytes(total *int64, size, limit int64) error {
	if size > limit-*total {
		return fmt.Errorf("WS 轨迹解码负载超过预算")
	}
	*total += size
	return nil
}

func validWSCloseCode(code uint16) bool {
	return (code >= 1000 && code <= 1014 && code != 1004 && code != 1005 && code != 1006) || (code >= 3000 && code <= 4999)
}

// RFC 6901 转义必须逐项校验，防止错误 ID pointer 被当成缺席后静默跳过。
func wsJSONPointer(value any, pointer string) (any, bool) {
	if pointer == "" {
		return value, true
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, false
	}
	for _, raw := range strings.Split(pointer[1:], "/") {
		var token strings.Builder
		for i := 0; i < len(raw); i++ {
			if raw[i] != '~' {
				token.WriteByte(raw[i])
				continue
			}
			i++
			if i >= len(raw) || (raw[i] != '0' && raw[i] != '1') {
				return nil, false
			}
			if raw[i] == '0' {
				token.WriteByte('~')
			} else {
				token.WriteByte('/')
			}
		}
		key := token.String()
		switch current := value.(type) {
		case map[string]any:
			var ok bool
			value, ok = current[key]
			if !ok {
				return nil, false
			}
		case []any:
			if key == "" || (len(key) > 1 && key[0] == '0') {
				return nil, false
			}
			for _, digit := range key {
				if digit < '0' || digit > '9' {
					return nil, false
				}
			}
			index, err := strconv.Atoi(key)
			if err != nil || index >= len(current) {
				return nil, false
			}
			value = current[index]
		default:
			return nil, false
		}
	}
	return value, true
}

type wsGraph struct {
	index   map[string]int
	parents [][]int
}

// 点内 FIFO 是不可撤销的隐式边，不能只对作者手写的 after 做拓扑检查。
func newWSGraph(nodes []WSNode, edgeLimit int) (*wsGraph, error) {
	g := &wsGraph{index: make(map[string]int, len(nodes)), parents: make([][]int, len(nodes))}
	last := make(map[WSPoint]int)
	edges := 0
	for i, n := range nodes {
		if strings.TrimSpace(n.ID) == "" {
			return nil, fmt.Errorf("WS 节点缺少 ID")
		}
		if _, ok := g.index[n.ID]; ok {
			return nil, fmt.Errorf("WS 节点 ID 重复")
		}
		g.index[n.ID] = i
		switch n.Point {
		case WSClientSend, WSUpstreamReceive, WSUpstreamSend, WSClientReceive:
		default:
			return nil, fmt.Errorf("WS 观测点不合法")
		}
		if len(n.After) > edgeLimit-edges {
			return nil, fmt.Errorf("WS after 引用超过预算")
		}
		edges += len(n.After)
		if previous, ok := last[n.Point]; ok {
			g.parents[i] = append(g.parents[i], previous)
		}
		last[n.Point] = i
	}
	children := make([][]int, len(nodes))
	indegrees := make([]int, len(nodes))
	for i, n := range nodes {
		for _, id := range n.After {
			parent, ok := g.index[id]
			if !ok {
				return nil, fmt.Errorf("WS after 引用不存在")
			}
			g.parents[i] = append(g.parents[i], parent)
		}
		for _, parent := range g.parents[i] {
			children[parent] = append(children[parent], i)
			indegrees[i]++
		}
	}
	var ready []int
	for i, count := range indegrees {
		if count == 0 {
			ready = append(ready, i)
		}
	}
	for head := 0; head < len(ready); head++ {
		for _, child := range children[ready[head]] {
			indegrees[child]--
			if indegrees[child] == 0 {
				ready = append(ready, child)
			}
		}
	}
	if len(ready) != len(nodes) {
		return nil, fmt.Errorf("WS after 与 FIFO 合图存在循环")
	}
	return g, nil
}

func (g *wsGraph) precedes(before, after int) bool {
	seen := make([]bool, len(g.parents))
	pending := append([]int(nil), g.parents[after]...)
	for len(pending) > 0 {
		i := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if i == before {
			return true
		}
		if !seen[i] {
			seen[i] = true
			pending = append(pending, g.parents[i]...)
		}
	}
	return false
}

func validateWSProvenance(s WSSession, limits WSLimits, traceBytes *int64) error {
	p := s.Provenance
	if p.Kind != "synthetic" && p.Kind != "synthetic-negative" && p.Kind != "recorded" {
		return fmt.Errorf("WS 来源 kind 不合法")
	}
	for _, n := range s.Nodes {
		want := "synthetic"
		if p.Kind == "recorded" {
			want = map[WSPoint]string{WSClientSend: "authored", WSUpstreamReceive: "upstream-accepted", WSUpstreamSend: "recorded", WSClientReceive: "golden"}[n.Point]
		}
		if n.Source != want {
			return fmt.Errorf("WS 观测点来源与 provenance 不符")
		}
	}
	if len(p.SampleSHA256) != len(s.Samples) {
		return fmt.Errorf("WS 样本来源账本不对齐")
	}
	for name, sample := range s.Samples {
		if strings.TrimSpace(name) == "" || sample.Data == nil {
			return fmt.Errorf("WS 样本必须有名称与非 null 字节")
		}
		if err := addWSTraceBytes(traceBytes, int64(len(sample.Data)), limits.TraceBytes); err != nil {
			return err
		}
		digest := sha256.Sum256(sample.Data)
		want := hex.EncodeToString(digest[:])
		if sample.SHA256 != want || p.SampleSHA256[name] != want {
			return fmt.Errorf("WS 样本摘要不符")
		}
	}
	if p.Kind == "recorded" {
		if _, err := time.Parse(time.RFC3339, p.RecordedAt); err != nil {
			return fmt.Errorf("recorded 缺少合法录制时间")
		}
		if strings.TrimSpace(p.Model) == "" || strings.TrimSpace(p.UpstreamProtocol) == "" || strings.TrimSpace(p.UpstreamVersion) == "" || p.SourceSHA256 == "" {
			return fmt.Errorf("recorded 缺少模型、契约版本或摘要")
		}
	}
	if p.SourceSHA256 != "" {
		digest, err := wsContractDigest(s, limits.JSONDepth)
		if err != nil {
			return err
		}
		if digest != p.SourceSHA256 {
			return fmt.Errorf("WS 上游契约摘要不符")
		}
	}
	return nil
}

// WSContractDigest 覆盖独立上游契约段，不包含自指摘要及客户端黄金预期。
// 摘要一致只证明当前账本自洽，不能证明字节确实来自云端。
func WSContractDigest(s WSSession) (string, error) {
	return wsContractDigest(s, DefaultWSLimits().JSONDepth)
}

func wsContractDigest(s WSSession, depthLimit int) (string, error) {
	contract := struct {
		Upstream Request           `json:"upstream"`
		Status   int               `json:"upstream_expected_status"`
		Error    json.RawMessage   `json:"upstream_error,omitempty"`
		Nodes    []WSNode          `json:"nodes"`
		Outcome  WSOutcome         `json:"outcome"`
		Samples  map[string]string `json:"sample_sha256,omitempty"`
	}{Upstream: s.Upstream, Status: s.UpstreamExpectedStatus, Error: s.UpstreamError, Outcome: s.Outcome, Samples: s.Provenance.SampleSHA256}
	for _, n := range s.Nodes {
		if n.Point == WSUpstreamReceive || n.Point == WSUpstreamSend {
			contract.Nodes = append(contract.Nodes, n)
		}
	}
	raw, err := json.Marshal(contract)
	if err != nil {
		return "", fmt.Errorf("WS 契约不能编码")
	}
	// 重新编码对象键排序，避免 RawMessage 的空白及字段顺序造成伪篡改告警。
	value, err := strictWSJSON(raw, depthLimit)
	if err != nil {
		return "", fmt.Errorf("WS 契约 JSON 不合法")
	}
	raw, err = json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("WS 契约不能稳定编码")
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}
