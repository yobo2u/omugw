package testkit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/yobo2u/omugw/internal/transport/ws"
)

// WSMatcher 只确认完整匹配的实体，防止失败消息或并发发送留下半次绑定。
type WSMatcher struct {
	mu       sync.Mutex
	limits   WSLimits
	bindings map[wsBinding]string
	owners   map[wsActualID]wsBinding
}

type wsActualID struct{ namespace, id string }

type wsMatchRule struct {
	field WSFieldRule
	value any
}

func NewWSMatcher(limits WSLimits) (*WSMatcher, error) {
	if err := limits.validate(); err != nil {
		return nil, err
	}
	return &WSMatcher{limits: limits, bindings: make(map[wsBinding]string), owners: make(map[wsActualID]wsBinding)}, nil
}

// Match 不回显消息、pointer 或实体值，避免测试失败将负载中的敏感内容写进日志。
func (m *WSMatcher) Match(node WSNode, actual WSMessage) error {
	if m == nil {
		return fmt.Errorf("WS matcher 未初始化")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.limits.validate(); err != nil {
		return err
	}
	expected, rules, err := prepareWSMatchNode(node, m.limits)
	if err != nil {
		return err
	}
	if err := validateWSMatchMessage(actual, m.limits.MessageBytes); err != nil {
		return err
	}
	if actual.Opcode != node.Message.Opcode {
		return fmt.Errorf("WS 消息 opcode 不符")
	}
	if node.Match != "json" {
		if !bytes.Equal(node.Message.Payload, actual.Payload) {
			return fmt.Errorf("WS 消息字节不符")
		}
		return nil
	}
	observed, err := strictWSJSON(actual.Payload, m.limits.JSONDepth)
	if err != nil {
		return err
	}
	pending := make(map[wsBinding]string)
	owners := make(map[wsActualID]wsBinding)
	for _, rule := range rules {
		value := rule.value
		if rule.field.Mode != "equal" {
			actualValue, exists := wsJSONPointer(observed, rule.field.Pointer)
			id, ok := actualValue.(string)
			if !exists || !ok || id == "" {
				return fmt.Errorf("WS 实体值必须为已存在的非空字符串")
			}
			key := wsBinding{rule.field.Namespace, rule.field.Symbol}
			if rule.field.Mode == "reference" {
				// 只查已提交状态，不能用本消息暂存的 bind 伪造因果在先的引用。
				bound, exists := m.bindings[key]
				if !exists || bound != id {
					return fmt.Errorf("WS 实体引用未确认或不符")
				}
			} else {
				if _, exists := m.bindings[key]; exists {
					return fmt.Errorf("WS 实体不能重复绑定")
				}
				if _, exists := pending[key]; exists {
					return fmt.Errorf("WS 实体不能重复绑定")
				}
				actualID := wsActualID{key.namespace, id}
				if _, exists := m.owners[actualID]; exists {
					return fmt.Errorf("WS 同命名空间的实际 ID 不能复用")
				}
				if _, exists := owners[actualID]; exists {
					return fmt.Errorf("WS 同命名空间的实际 ID 不能复用")
				}
				if len(pending) >= m.limits.Bindings-len(m.bindings) {
					return fmt.Errorf("WS 实体绑定超过预算")
				}
				pending[key], owners[actualID] = id, key
			}
			value = id
		}
		expected = replaceWSJSONValue(expected, rule.field.Pointer, value)
	}
	// json.Number 保留十进制词法，不经 float64，也不把缺席、null 和零值互相折叠。
	if !reflect.DeepEqual(expected, observed) {
		return fmt.Errorf("WS JSON 固定字段或值不符")
	}
	for key, id := range pending {
		m.bindings[key] = id
		m.owners[wsActualID{key.namespace, id}] = key
	}
	return nil
}

// Materialize 不生成 ID、不提交 bind、不应用 equal 转换；只有成功发送后的 Match 才能确认实体。
func (m *WSMatcher) Materialize(node WSNode) (WSMessage, error) {
	if m == nil {
		return WSMessage{}, fmt.Errorf("WS matcher 未初始化")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.limits.validate(); err != nil {
		return WSMessage{}, err
	}
	_, rules, err := prepareWSMatchNode(node, m.limits)
	if err != nil {
		return WSMessage{}, err
	}
	references := make(map[string][]byte)
	for _, rule := range rules {
		if rule.field.Mode != "reference" {
			continue
		}
		id, exists := m.bindings[wsBinding{rule.field.Namespace, rule.field.Symbol}]
		if !exists {
			return WSMessage{}, fmt.Errorf("WS 发送引用缺少已确认绑定")
		}
		// 只编码一个字符串 token，防止 ID 中的引号或反斜线注入额外字段。
		raw, err := json.Marshal(id)
		if err != nil {
			return WSMessage{}, fmt.Errorf("WS 实体字符串不能编码")
		}
		references[rule.field.Pointer] = raw
	}
	payload := bytes.Clone(node.Message.Payload)
	if len(references) != 0 {
		spans, err := wsJSONValueSpans(node.Message.Payload, references)
		if err != nil {
			return WSMessage{}, err
		}
		edits := make([]wsSpanEdit, 0, len(references))
		for pointer, raw := range references {
			span, exists := spans[pointer]
			if !exists || !span.scalar {
				return WSMessage{}, fmt.Errorf("WS 引用缺少可替换的标量 span")
			}
			edits = append(edits, wsSpanEdit{span, raw})
		}
		payload, err = replaceWSValueSpans(node.Message.Payload, edits, m.limits.MessageBytes)
		if err != nil {
			return WSMessage{}, err
		}
	}
	return WSMessage{Opcode: node.Message.Opcode, Payload: payload}, nil
}

func validateWSMatchMessage(message WSMessage, limit int64) error {
	if message.Opcode != ws.OpText && message.Opcode != ws.OpBinary {
		return fmt.Errorf("WS 匹配只接受完整 text 或 binary 消息")
	}
	if int64(len(message.Payload)) > limit {
		return fmt.Errorf("WS 匹配消息超过解码预算")
	}
	if message.Opcode == ws.OpText && (len(message.Payload) == 0 || !utf8.Valid(message.Payload)) {
		return fmt.Errorf("WS 匹配 text 必须为非空 UTF8")
	}
	return nil
}

func prepareWSMatchNode(node WSNode, limits WSLimits) (any, []wsMatchRule, error) {
	if node.Kind != "message" || node.Message == nil || node.Message.Payload == nil || node.CloseCode != nil || node.CloseReason != "" {
		return nil, nil, fmt.Errorf("WS 匹配节点必须仅含 message")
	}
	if err := validateWSMatchMessage(*node.Message, limits.MessageBytes); err != nil {
		return nil, nil, err
	}
	switch node.Match {
	case "", "bytes":
		if len(node.Fields) != 0 {
			return nil, nil, fmt.Errorf("WS bytes 匹配不能带字段规则")
		}
		return nil, nil, nil
	case "json":
		if node.Message.Opcode != ws.OpText {
			return nil, nil, fmt.Errorf("WS JSON 匹配只适用 text")
		}
	default:
		return nil, nil, fmt.Errorf("WS 匹配模式不合法")
	}
	value, err := strictWSJSON(node.Message.Payload, limits.JSONDepth)
	if err != nil {
		return nil, nil, err
	}
	rules, err := prepareWSMatchRules(value, node.Fields, limits)
	if err != nil {
		return nil, nil, err
	}
	if node.Point == WSClientSend || node.Point == WSUpstreamSend {
		for _, rule := range rules {
			literal, _ := wsJSONPointer(value, rule.field.Pointer)
			if rule.field.Mode == "equal" && !reflect.DeepEqual(literal, rule.value) {
				return nil, nil, fmt.Errorf("WS 发送 equal 必须与字面负载一致")
			}
		}
	}
	return value, rules, nil
}

func prepareWSMatchRules(value any, fields []WSFieldRule, limits WSLimits) ([]wsMatchRule, error) {
	if len(fields) > limits.FieldRules {
		return nil, fmt.Errorf("WS 匹配字段规则超过预算")
	}
	pointers := make(map[string]bool, len(fields))
	rules := make([]wsMatchRule, 0, len(fields))
	for _, field := range fields {
		literal, exists := wsJSONPointer(value, field.Pointer)
		if !exists {
			return nil, fmt.Errorf("WS 字段 pointer 不合法或不存在")
		}
		if pointers[field.Pointer] {
			return nil, fmt.Errorf("WS 字段 pointer 重复或相互覆盖")
		}
		pointers[field.Pointer] = true
		rule := wsMatchRule{field: field}
		switch field.Mode {
		case "equal":
			if field.Namespace != "" || field.Symbol != "" || field.Value == nil || int64(len(field.Value)) > limits.MessageBytes {
				return nil, fmt.Errorf("WS equal 必须仅声明有界字面 value")
			}
			var err error
			rule.value, err = strictWSJSON(field.Value, limits.JSONDepth)
			if err != nil {
				return nil, err
			}
		case "bind", "reference":
			if field.Namespace == "" || field.Symbol == "" || field.Value != nil {
				return nil, fmt.Errorf("WS ID 规则必须仅声明 namespace 与 symbol")
			}
			if id, ok := literal.(string); !ok || id == "" || !wsIDPointer(field.Pointer) {
				return nil, fmt.Errorf("WS ID 规则必须指向限定 ID 名的非空字符串")
			}
		default:
			return nil, fmt.Errorf("WS 字段规则模式不合法")
		}
		rules = append(rules, rule)
	}
	// 查每一级祖先，防止排序中插入兄弟键后漏掉覆盖关系，也避免规则间平方级比较。
	for pointer := range pointers {
		for ancestor := pointer; ancestor != ""; {
			ancestor = ancestor[:strings.LastIndex(ancestor, "/")]
			if pointers[ancestor] {
				return nil, fmt.Errorf("WS 字段 pointer 重复或相互覆盖")
			}
		}
	}
	return rules, nil
}

func wsIDPointer(pointer string) bool {
	if pointer == "" {
		return false
	}
	switch pointer[strings.LastIndex(pointer, "/")+1:] {
	case "event_id", "session_id", "response_id", "item_id", "call_id", "task_id", "id":
		return true
	default:
		return false
	}
}

// 路径已由 RFC 6901 查验，替换只改存在的值，不创建键或扩充数组。
func replaceWSJSONValue(root any, pointer string, value any) any {
	if pointer == "" {
		return value
	}
	last := strings.LastIndex(pointer, "/")
	parent, _ := wsJSONPointer(root, pointer[:last])
	key := strings.ReplaceAll(strings.ReplaceAll(pointer[last+1:], "~1", "/"), "~0", "~")
	switch current := parent.(type) {
	case map[string]any:
		current[key] = value
	case []any:
		index, _ := strconv.Atoi(key)
		current[index] = value
	}
	return root
}

// AssertWSForwardedPayload 仅豁免显式 equal 的标量值 token，其余字节连空白和键序都不能变化。
func AssertWSForwardedPayload(sent, received WSMessage, rewrites []WSFieldRule) error {
	limits := DefaultWSLimits()
	for _, message := range []WSMessage{sent, received} {
		if err := validateWSMatchMessage(message, limits.MessageBytes); err != nil {
			return err
		}
	}
	if sent.Opcode != received.Opcode {
		return fmt.Errorf("WS 转发 opcode 不符")
	}
	if len(rewrites) == 0 {
		if !bytes.Equal(sent.Payload, received.Payload) {
			return fmt.Errorf("WS 转发负载字节不符")
		}
		return nil
	}
	if sent.Opcode != ws.OpText {
		return fmt.Errorf("WS binary 转发不支持 JSON span 改写")
	}
	original, err := strictWSJSON(sent.Payload, limits.JSONDepth)
	if err != nil {
		return err
	}
	observed, err := strictWSJSON(received.Payload, limits.JSONDepth)
	if err != nil {
		return err
	}
	rules, err := prepareWSMatchRules(original, rewrites, limits)
	if err != nil {
		return err
	}
	wanted := make(map[string][]byte, len(rules))
	for _, rule := range rules {
		if rule.field.Mode != "equal" {
			return fmt.Errorf("WS 转发只允许显式 equal 改写")
		}
		actual, exists := wsJSONPointer(observed, rule.field.Pointer)
		if !exists || !reflect.DeepEqual(actual, rule.value) {
			return fmt.Errorf("WS 转发改写值不符合字面预期")
		}
		wanted[rule.field.Pointer] = nil
	}
	sentSpans, err := wsJSONValueSpans(sent.Payload, wanted)
	if err != nil {
		return err
	}
	receivedSpans, err := wsJSONValueSpans(received.Payload, wanted)
	if err != nil {
		return err
	}
	edits := make([]wsSpanEdit, 0, len(wanted))
	for pointer := range wanted {
		from, fromOK := sentSpans[pointer]
		to, toOK := receivedSpans[pointer]
		if !fromOK || !toOK || !from.scalar || !to.scalar {
			return fmt.Errorf("WS 转发不支持 object 或 array span 改写")
		}
		edits = append(edits, wsSpanEdit{from, received.Payload[to.start:to.end]})
	}
	rewritten, err := replaceWSValueSpans(sent.Payload, edits, limits.MessageBytes)
	if err != nil {
		return err
	}
	if !bytes.Equal(rewritten, received.Payload) {
		return fmt.Errorf("WS 转发在声明的标量 span 外发生字节变化")
	}
	return nil
}

type wsValueSpan struct {
	start, end int
	scalar     bool
}

type wsSpanEdit struct {
	span  wsValueSpan
	value []byte
}

// 只收集已严格校验 JSON 中被规则选中的 token offset，避免为所有未知字段建立 span 索引。
func wsJSONValueSpans(raw []byte, wanted map[string][]byte) (map[string]wsValueSpan, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	spans := make(map[string]wsValueSpan, len(wanted))
	var visit func(string) error
	visit = func(pointer string) error {
		start := int(d.InputOffset())
		token, err := d.Token()
		if err != nil {
			return fmt.Errorf("WS JSON span 读取失败")
		}
		end := int(d.InputOffset())
		// InputOffset 在键或上一值末尾；冒号、逗号与空白不属于下一个值 token。
		for start < end && strings.ContainsRune(" \t\r\n:,", rune(raw[start])) {
			start++
		}
		delim, container := token.(json.Delim)
		if container {
			switch delim {
			case '{':
				for d.More() {
					keyToken, err := d.Token()
					if err != nil {
						return fmt.Errorf("WS JSON span 键读取失败")
					}
					key, ok := keyToken.(string)
					if !ok {
						return fmt.Errorf("WS JSON span 键不合法")
					}
					key = strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
					if err := visit(pointer + "/" + key); err != nil {
						return err
					}
				}
			case '[':
				for i := 0; d.More(); i++ {
					if err := visit(pointer + "/" + strconv.Itoa(i)); err != nil {
						return err
					}
				}
			default:
				return fmt.Errorf("WS JSON span 分隔符不合法")
			}
			if _, err := d.Token(); err != nil {
				return fmt.Errorf("WS JSON span 未闭合")
			}
			end = int(d.InputOffset())
		}
		if _, selected := wanted[pointer]; selected {
			spans[pointer] = wsValueSpan{start, end, !container}
		}
		return nil
	}
	if err := visit(""); err != nil {
		return nil, err
	}
	return spans, nil
}

func replaceWSValueSpans(raw []byte, edits []wsSpanEdit, limit int64) ([]byte, error) {
	sort.Slice(edits, func(i, j int) bool { return edits[i].span.start < edits[j].span.start })
	size, previous := int64(len(raw)), 0
	for _, edit := range edits {
		span := edit.span
		if !span.scalar || span.start < previous || span.end <= span.start || span.end > len(raw) {
			return nil, fmt.Errorf("WS 标量 span 不合法或相互覆盖")
		}
		size -= int64(span.end - span.start)
		previous = span.end
	}
	// 先扣除所有旧 token 再计入新值，避免合法的长短 ID 交换被中间大小误拒绝。
	for _, edit := range edits {
		if int64(len(edit.value)) > limit-size {
			return nil, fmt.Errorf("WS span 替换后消息超过预算")
		}
		size += int64(len(edit.value))
	}
	if int64(int(size)) != size {
		return nil, fmt.Errorf("WS span 替换大小不可表示")
	}
	result := make([]byte, 0, int(size))
	previous = 0
	for _, edit := range edits {
		result = append(result, raw[previous:edit.span.start]...)
		result = append(result, edit.value...)
		previous = edit.span.end
	}
	return append(result, raw[previous:]...), nil
}
