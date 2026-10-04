// Package dashscopeinference 只提取任务包络的小事实，未知音频和文本仍由调用方原样保全。
package dashscopeinference

import (
	"errors"
	"math"
	"strconv"
	"unicode"
	"unicode/utf8"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/protocol/realtimejson"
)

const maxIDBytes = 512

var errEnvelope = errors.New("dashscope inference: invalid task envelope or binding")

// Presence 防止缺失、显式 null 和真实零计量被折成同一笔用量。
type Presence uint8

const (
	Missing Presence = iota
	Null
	Value
	Invalid
)

type TextField struct {
	Presence Presence
	Value    string
}

type BindingFields struct {
	Streaming, Model, TaskGroup, Task, Function TextField
}

// Facts 只持有独立字符串和标量，不能把借用的扫描视图带进任务表。
type ClientFacts struct {
	Action, TaskID string
	Binding        BindingFields
}

type UsageSnapshot struct {
	Presence                                           Presence
	InputTokens, OutputTokens, TotalTokens, Characters int64
	Seconds                                            float64
}

type ServerFacts struct {
	Event, TaskID string
	Binding       BindingFields
	Usage         UsageSnapshot
	Failure       *canonical.Error
}

func InspectClient(raw []byte) (ClientFacts, error) {
	header, payload, err := envelope(raw)
	if err != nil {
		return ClientFacts{}, errEnvelope
	}
	action, err := textField(header, "action", 128, false)
	if err != nil || action.Presence != Value {
		return ClientFacts{}, errEnvelope
	}
	id, err := textField(header, "task_id", maxIDBytes, true)
	if err != nil || id.Presence != Value {
		return ClientFacts{}, errEnvelope
	}
	binding, err := inspectBinding(header, payload, action.Value == "run-task")
	if err != nil {
		return ClientFacts{}, errEnvelope
	}
	return ClientFacts{Action: action.Value, TaskID: id.Value, Binding: binding}, nil
}

func InspectServer(raw []byte, contract ModelContract) (ServerFacts, error) {
	header, payload, err := envelope(raw)
	if err != nil {
		return ServerFacts{}, errEnvelope
	}
	event, err := textField(header, "event", 128, false)
	if err != nil || event.Presence != Value {
		return ServerFacts{}, errEnvelope
	}
	id, err := textField(header, "task_id", maxIDBytes, true)
	if err != nil || id.Presence != Value {
		return ServerFacts{}, errEnvelope
	}
	binding, err := inspectBinding(header, payload, false)
	if err != nil {
		return ServerFacts{}, errEnvelope
	}
	f := ServerFacts{Event: event.Value, TaskID: id.Value, Binding: binding}
	if f.Event == "task-failed" {
		code, err := textField(header, "error_code", 128, false)
		if err != nil {
			return ServerFacts{}, errEnvelope
		}
		class := canonical.ClassInternal
		if code.Value == "InvalidParameter" {
			class = canonical.ClassBadRequest
		}
		// 错误码只参与已核实的分类，不能把原文、ID 或 message 留进错误与日志。
		f.Failure = &canonical.Error{Class: class, Message: "DashScope Inference upstream task failed"}
	}
	switch f.Event {
	case "result-generated", "task-finished", "task-failed":
		f.Usage = inspectUsage(payload, contract, f.Event == "result-generated")
	}
	return f, nil
}

// VerifiedTaskID 只恢复错误关联键；其他字段损坏时仍不能把请求视为合法准入。
func VerifiedTaskID(raw []byte) (string, bool) {
	root, err := realtimejson.Parse(raw)
	if err != nil {
		return "", false
	}
	header, err := root.Field("header")
	if err != nil || !object(header) {
		return "", false
	}
	id, err := textField(header, "task_id", maxIDBytes, true)
	if err != nil || id.Presence != Value {
		return "", false
	}
	return id.Value, true
}

func envelope(raw []byte) (realtimejson.Value, realtimejson.Value, error) {
	root, err := realtimejson.Parse(raw)
	if err != nil {
		return nil, nil, errEnvelope
	}
	header, err := root.Field("header")
	if err != nil || !object(header) {
		return nil, nil, errEnvelope
	}
	payload, err := root.Field("payload")
	if err != nil || !object(payload) {
		return nil, nil, errEnvelope
	}
	return header, payload, nil
}

func object(v realtimejson.Value) bool { return len(v) != 0 && v[0] == '{' }

func textField(v realtimejson.Value, key string, limit int, id bool) (TextField, error) {
	raw, err := v.Field(key)
	if err != nil {
		return TextField{}, errEnvelope
	}
	if raw == nil {
		return TextField{Presence: Missing}, nil
	}
	s, err := raw.Text(limit)
	if err != nil || s == "" || !id && !identityText(s, limit) {
		return TextField{}, errEnvelope
	}
	return TextField{Presence: Value, Value: s}, nil
}

func identityText(s string, limit int) bool {
	if s == "" || len(s) > limit || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func inspectBinding(header, payload realtimejson.Value, required bool) (BindingFields, error) {
	var b BindingFields
	for _, field := range [...]struct {
		container realtimejson.Value
		key       string
		limit     int
		dst       *TextField
	}{
		{header, "streaming", 16, &b.Streaming},
		{payload, "model", maxIDBytes, &b.Model},
		{payload, "task_group", 128, &b.TaskGroup},
		{payload, "task", 128, &b.Task},
		{payload, "function", 128, &b.Function},
	} {
		v, err := textField(field.container, field.key, field.limit, false)
		if err != nil || required && v.Presence != Value {
			return BindingFields{}, errEnvelope
		}
		*field.dst = v
	}
	return b, nil
}

func inspectUsage(payload realtimejson.Value, contract ModelContract, intermediate bool) UsageSnapshot {
	// 未证实口径不能仅因字段同名就发布累计值；未读取保持 Missing。
	if contract.Mode != ModeCumulative || contract.Unit != UnitTokens && contract.Unit != UnitCharacters && contract.Unit != UnitSeconds {
		return UsageSnapshot{}
	}
	if intermediate && contract.SentenceEndOnly {
		output, err := payload.Field("output")
		if err != nil {
			return UsageSnapshot{Presence: Invalid}
		}
		if !object(output) {
			return UsageSnapshot{}
		}
		event, err := textField(output, "event", 128, false)
		if err != nil {
			return UsageSnapshot{Presence: Invalid}
		}
		if event.Value != "sentence-end" {
			return UsageSnapshot{}
		}
	}
	v, err := payload.Field("usage")
	if err != nil {
		return UsageSnapshot{Presence: Invalid}
	}
	if p := presence(v); p != Value {
		return UsageSnapshot{Presence: p}
	}
	if !object(v) {
		return UsageSnapshot{Presence: Invalid}
	}
	// 只核指定层的五个计费字段，不递归猜未知对象里的同名字段。
	var fields [5]realtimejson.Value
	for i, name := range [...]string{"input_tokens", "output_tokens", "total_tokens", "characters", "duration"} {
		fields[i], err = v.Field(name)
		if err != nil {
			return UsageSnapshot{Presence: Invalid}
		}
		allowed := i < 3 && contract.Unit == UnitTokens || i == 3 && contract.Unit == UnitCharacters || i == 4 && (contract.Unit == UnitSeconds || contract.Unit == UnitTokens && contract.CompatibilityDuration)
		if !allowed && presence(fields[i]) == Value {
			return UsageSnapshot{Presence: Invalid}
		}
	}
	switch contract.Unit {
	case UnitTokens:
		if fields[0] == nil && fields[1] == nil && fields[2] == nil {
			return UsageSnapshot{}
		}
		input, inOK := fields[0].Count()
		output, outOK := fields[1].Count()
		total, totalOK := fields[2].Count()
		if !inOK || !outOK || !totalOK || input > math.MaxInt64-output || total != input+output {
			return UsageSnapshot{Presence: Invalid}
		}
		return UsageSnapshot{Presence: Value, InputTokens: input, OutputTokens: output, TotalTokens: total}
	case UnitCharacters:
		if p := presence(fields[3]); p != Value {
			return UsageSnapshot{Presence: p}
		}
		n, ok := fields[3].Count()
		if !ok {
			return UsageSnapshot{Presence: Invalid}
		}
		return UsageSnapshot{Presence: Value, Characters: n}
	default:
		if p := presence(fields[4]); p != Value {
			return UsageSnapshot{Presence: p}
		}
		n, ok := duration(fields[4])
		if !ok {
			return UsageSnapshot{Presence: Invalid}
		}
		return UsageSnapshot{Presence: Value, Seconds: n}
	}
}

func presence(v realtimejson.Value) Presence {
	if v == nil {
		return Missing
	}
	if string(v) == "null" {
		return Null
	}
	return Value
}

func duration(v realtimejson.Value) (float64, bool) {
	// 限数字文本长度，防超长数值导致复制或 ParseFloat 错误驻留原负载。
	if len(v) == 0 || len(v) > 128 || v[0] != '-' && (v[0] < '0' || v[0] > '9') {
		return 0, false
	}
	nonzero := false
	for _, c := range v {
		if c == 'e' || c == 'E' {
			break
		}
		if c >= '1' && c <= '9' {
			nonzero = true
		}
	}
	if !nonzero {
		return 0, true
	}
	if v[0] == '-' {
		return 0, false
	}
	n, err := strconv.ParseFloat(string(v), 64)
	// 非零数下溢不能伪装成上游明确的零计量。
	return n, err == nil && !math.IsInf(n, 0) && !math.IsNaN(n) && n > 0
}
