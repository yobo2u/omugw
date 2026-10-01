package testkit

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/transport/ws"
)

func wsMatchNode(payload string, fields ...WSFieldRule) WSNode {
	return WSNode{ID: "message", Point: WSClientReceive, Source: "synthetic", Kind: "message", Match: "json",
		Message: &WSMessage{Opcode: ws.OpText, Payload: []byte(payload)}, Fields: fields}
}

func wsMatchText(payload string) WSMessage {
	return WSMessage{Opcode: ws.OpText, Payload: []byte(payload)}
}

func wsMatchEqual(pointer, value string) WSFieldRule {
	return WSFieldRule{Pointer: pointer, Mode: "equal", Value: json.RawMessage(value)}
}

func wsMatchID(pointer, mode, namespace, symbol string) WSFieldRule {
	return WSFieldRule{Pointer: pointer, Mode: mode, Namespace: namespace, Symbol: symbol}
}

func wsTestMatcher(t *testing.T, limits WSLimits) *WSMatcher {
	t.Helper()
	m, err := NewWSMatcher(limits)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// 固定字段必须逐值比较，防止零值、缺席和大整数被规范化成同一个预期。
func TestWSMatchPreservesValues(t *testing.T) {
	t.Run("bytes", func(t *testing.T) {
		m := wsTestMatcher(t, DefaultWSLimits())
		for _, opcode := range []ws.Opcode{ws.OpText, ws.OpBinary} {
			n := wsMatchNode("opaque UTF8 文本")
			n.Match = ""
			n.Message.Opcode = opcode
			if err := m.Match(n, *n.Message); err != nil {
				t.Fatal(err)
			}
			truncated := WSMessage{Opcode: opcode, Payload: n.Message.Payload[:len(n.Message.Payload)-1]}
			if err := m.Match(n, truncated); err == nil {
				t.Fatal("截断负载未被拒绝")
			}
			wrongOpcode := *n.Message
			wrongOpcode.Opcode = ws.OpPing
			if err := m.Match(n, wrongOpcode); err == nil {
				t.Fatal("错误 opcode 未被拒绝")
			}
		}
		n := wsMatchNode("")
		n.Match, n.Message.Opcode = "bytes", ws.OpBinary
		if err := m.Match(n, WSMessage{Opcode: ws.OpBinary, Payload: []byte{}}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("presence-and-types", func(t *testing.T) {
		values := []string{`{}`, `{"v":null}`, `{"v":false}`, `{"v":0}`, `{"v":""}`, `{"v":[]}`, `{"v":{}}`}
		m := wsTestMatcher(t, DefaultWSLimits())
		for i, expected := range values {
			for j, actual := range values {
				if err := m.Match(wsMatchNode(expected), wsMatchText(actual)); (err == nil) != (i == j) {
					t.Fatalf("presence pair %d/%d: %v", i, j, err)
				}
			}
		}
	})
	t.Run("fixed-content", func(t *testing.T) {
		m := wsTestMatcher(t, DefaultWSLimits())
		n := wsMatchNode(`{"text":"hello","usage":3,"sample_rate":24000}`)
		if err := m.Match(n, wsMatchText(" { \"sample_rate\" : 24000, \"usage\":3, \"text\":\"hello\" } ")); err != nil {
			t.Fatal(err)
		}
		for _, actual := range []string{
			`{"text":"hello","usage":3,"sample_rate":24000,"extra":false}`,
			`{"text":"hello","usage":0,"sample_rate":24000}`,
			`{"text":"hello","usage":3,"sample_rate":16000}`,
			`{"text":"changed","usage":3,"sample_rate":24000}`,
			`{"text":"hello","usage":3}`,
		} {
			if err := m.Match(n, wsMatchText(actual)); err == nil {
				t.Fatal("名单外语义改动未被拒绝")
			}
		}
		if err := m.Match(wsMatchNode(`{"n":9007199254740992}`), wsMatchText(`{"n":9007199254740993}`)); err == nil {
			t.Fatal("相邻大整数被 float64 舍入吞掉")
		}
		if err := m.Match(wsMatchNode(`{"n":123456789012345678901234567890}`), wsMatchText(`{"n":123456789012345678901234567890}`)); err != nil {
			t.Fatal(err)
		}
		for _, pair := range [][2]string{{`{"n":1}`, `{"n":1.0}`}, {`{"n":0}`, `{"n":-0}`}, {`{"n":1e400}`, `{"n":2e400}`}} {
			if err := m.Match(wsMatchNode(pair[0]), wsMatchText(pair[1])); err == nil {
				t.Fatal("数字的精确词法被折叠")
			}
		}
	})
	t.Run("strict-json", func(t *testing.T) {
		m := wsTestMatcher(t, DefaultWSLimits())
		for _, invalid := range []string{
			`{"v":0,"v":0}`, `{"v":0,"\u0076":0}`, `{} {}`, `{"v":`,
			strings.Repeat(`[`, 65) + `0` + strings.Repeat(`]`, 65),
		} {
			if err := m.Match(wsMatchNode(invalid), wsMatchText(invalid)); err == nil {
				t.Fatal("非法 fixture JSON 未被拒绝")
			}
			if err := m.Match(wsMatchNode(`{"v":0}`), wsMatchText(invalid)); err == nil {
				t.Fatal("非法实际 JSON 未被拒绝")
			}
		}
		atLimit := strings.Repeat(`[`, 64) + `0` + strings.Repeat(`]`, 64)
		if err := m.Match(wsMatchNode(atLimit), wsMatchText(atLimit)); err != nil {
			t.Fatal(err)
		}
	})
}

// 引用纪律不能因客户端或上游生成 ID 而改变，也不能在失败后留下半次绑定。
func TestWSBindingConsistency(t *testing.T) {
	t.Run("namespace-bijection", func(t *testing.T) {
		m := wsTestMatcher(t, DefaultWSLimits())
		for _, namespace := range []string{"call", "task", "item"} {
			bind := wsMatchNode(`{"id":"fixture"}`, wsMatchID("/id", "bind", namespace, "first"))
			bind.Point = WSUpstreamSend
			if err := m.Match(bind, wsMatchText(`{"id":"actual-1"}`)); err != nil {
				t.Fatal(err)
			}
			ref := wsMatchNode(`{"id":"fixture"}`, wsMatchID("/id", "reference", namespace, "first"))
			if err := m.Match(ref, wsMatchText(`{"id":"wrong-ID"}`)); err == nil {
				t.Fatal("错误 ID 引用未被拒绝")
			}
			if err := m.Match(ref, wsMatchText(`{"id":"actual-1"}`)); err != nil {
				t.Fatal("错号匹配污染了已确认绑定", err)
			}
			bind.Fields[0].Symbol = "second"
			if err := m.Match(bind, wsMatchText(`{"id":"actual-1"}`)); err == nil {
				t.Fatal("同命名空间的两个符号共用了实际 ID")
			}
			bind.Fields[0].Symbol = "first"
			if err := m.Match(bind, wsMatchText(`{"id":"replacement"}`)); err == nil {
				t.Fatal("已有实体被重绑")
			}
		}
		unknown := wsMatchNode(`{"id":"fixture"}`, wsMatchID("/id", "reference", "call", "unknown"))
		if err := m.Match(unknown, wsMatchText(`{"id":"actual-1"}`)); err == nil {
			t.Fatal("未知引用被当成首次绑定")
		}
	})
	t.Run("binding-budget", func(t *testing.T) {
		limits := DefaultWSLimits()
		limits.Bindings = 2
		m := wsTestMatcher(t, limits)
		for i, symbol := range []string{"a", "b", "c"} {
			n := wsMatchNode(`{"id":"fixture"}`, wsMatchID("/id", "bind", "task", symbol))
			if err := m.Match(n, wsMatchText(`{"id":"`+symbol+`"}`)); (err == nil) != (i < 2) {
				t.Fatalf("绑定预算 index=%d error=%v", i, err)
			}
		}
	})
	t.Run("transaction", func(t *testing.T) {
		limits := DefaultWSLimits()
		limits.Bindings = 1
		m := wsTestMatcher(t, limits)
		bind := wsMatchNode(`{"task_id":"fixture","usage":7}`, wsMatchID("/task_id", "bind", "task", "pending"))
		if err := m.Match(bind, wsMatchText(`{"task_id":"actual","usage":0}`)); err == nil {
			t.Fatal("错误 usage 未被拒绝")
		}
		ref := wsMatchNode(`{"task_id":"fixture"}`, wsMatchID("/task_id", "reference", "task", "pending"))
		if err := m.Match(ref, wsMatchText(`{"task_id":"actual"}`)); err == nil {
			t.Fatal("失败匹配留下了部分绑定")
		}
		bind.Fields[0].Symbol = "committed"
		if err := m.Match(bind, wsMatchText(`{"task_id":"actual","usage":7}`)); err != nil {
			t.Fatal("失败匹配消耗了预算或反向 ID", err)
		}
	})
	t.Run("pending-bijection-and-reference", func(t *testing.T) {
		m := wsTestMatcher(t, DefaultWSLimits())
		n := wsMatchNode(`{"call_id":"a","item_id":"b"}`,
			wsMatchID("/call_id", "bind", "call", "a"), wsMatchID("/item_id", "bind", "call", "b"))
		if err := m.Match(n, wsMatchText(`{"call_id":"same","item_id":"same"}`)); err == nil {
			t.Fatal("同一次匹配的两个实体共用了实际 ID")
		}
		ref := wsMatchNode(`{"call_id":"a"}`, wsMatchID("/call_id", "reference", "call", "a"))
		if err := m.Match(ref, wsMatchText(`{"call_id":"same"}`)); err == nil {
			t.Fatal("失败的多字段匹配提交了第一个绑定")
		}
		n.Fields[1] = wsMatchID("/item_id", "reference", "call", "a")
		if err := m.Match(n, wsMatchText(`{"call_id":"same","item_id":"same"}`)); err == nil {
			t.Fatal("未确认的新绑定被同消息引用")
		}
	})
	t.Run("all-or-none-at-binding-budget", func(t *testing.T) {
		limits := DefaultWSLimits()
		limits.Bindings = 1
		m := wsTestMatcher(t, limits)
		n := wsMatchNode(`{"call_id":"a","task_id":"b"}`,
			wsMatchID("/call_id", "bind", "call", "a"), wsMatchID("/task_id", "bind", "task", "b"))
		if err := m.Match(n, wsMatchText(`{"call_id":"actual-a","task_id":"actual-b"}`)); err == nil {
			t.Fatal("同一次匹配提交了超过预算的实体")
		}
		ref := wsMatchNode(`{"call_id":"a"}`, wsMatchID("/call_id", "reference", "call", "a"))
		if err := m.Match(ref, wsMatchText(`{"call_id":"actual-a"}`)); err == nil {
			t.Fatal("预算失败留下第一个绑定")
		}
		n = wsMatchNode(`{"call_id":"a"}`, wsMatchID("/call_id", "bind", "call", "a"))
		if err := m.Match(n, wsMatchText(`{"call_id":"actual-a"}`)); err != nil {
			t.Fatal("预算失败消耗了容量", err)
		}
	})
	t.Run("sent-bind-is-matched-not-generated", func(t *testing.T) {
		m := wsTestMatcher(t, DefaultWSLimits())
		n := wsMatchNode(`{"task_id":"literal","fixed":0}`, wsMatchID("/task_id", "bind", "task", "x"))
		n.Point = WSClientSend
		if err := m.Match(n, wsMatchText(`{"task_id":"actually-written","fixed":0}`)); err != nil {
			t.Fatal(err)
		}
		ref := wsMatchNode(`{"task_id":"literal"}`, wsMatchID("/task_id", "reference", "task", "x"))
		if err := m.Match(ref, wsMatchText(`{"task_id":"literal"}`)); err == nil {
			t.Fatal("Match 绑定了 fixture ID 而非实际发送值")
		}
		if err := m.Match(ref, wsMatchText(`{"task_id":"actually-written","fixed":0}`)); err == nil {
			t.Fatal("引用消息吞掉了额外字段")
		}
		if err := m.Match(ref, wsMatchText(`{"task_id":"actually-written"}`)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("client-send-literal", func(t *testing.T) {
		m := wsTestMatcher(t, DefaultWSLimits())
		send := wsMatchNode(`{"task_id":"client-authored"}`, wsMatchID("/task_id", "bind", "task", "client-task"))
		send.Point = WSClientSend
		materialized, err := m.Materialize(send)
		if err != nil || !bytes.Equal(materialized.Payload, []byte(`{"task_id":"client-authored"}`)) {
			t.Fatal("bind 不应生成 ID", err)
		}
		ref := wsMatchNode(`{"task_id":"fixture"}`, wsMatchID("/task_id", "reference", "task", "client-task"))
		ref.Point = WSUpstreamReceive
		if err := m.Match(ref, wsMatchText(`{"task_id":"client-authored"}`)); err == nil {
			t.Fatal("Materialize 提前提交了发送端绑定")
		}
		if err := m.Match(send, materialized); err != nil {
			t.Fatal(err)
		}
		if err := m.Match(ref, wsMatchText(`{"task_id":"other-task"}`)); err == nil {
			t.Fatal("客户端 ID 被接收端错号重绑")
		}
		if err := m.Match(ref, wsMatchText(`{"task_id":"client-authored"}`)); err != nil {
			t.Fatal(err)
		}
	})
}

func TestWSOnlyDeclaredRewrite(t *testing.T) {
	t.Run("overlap-separated-by-sibling", func(t *testing.T) {
		m := wsTestMatcher(t, DefaultWSLimits())
		n := wsMatchNode(`{"a":{"id":"fixed"},"a-variant":0}`,
			wsMatchEqual("/a", `{"id":"fixed"}`), wsMatchEqual("/a-variant", `0`), wsMatchEqual("/a/id", `"fixed"`))
		if err := m.Match(n, *n.Message); err == nil {
			t.Fatal("兄弟键排序遮住了祖先 pointer 的重叠")
		}
		if _, err := m.Materialize(n); err == nil {
			t.Fatal("Materialize 接受了被兄弟键遮住的重叠 pointer")
		}
	})
	m := wsTestMatcher(t, DefaultWSLimits())
	n := wsMatchNode(`{"session":{"model":"logical","sample_rate":24000},"unknown":false}`,
		wsMatchEqual("/session/model", `"upstream-model"`))
	if err := m.Match(n, wsMatchText(`{"session":{"model":"upstream-model","sample_rate":24000},"unknown":false}`)); err != nil {
		t.Fatal(err)
	}
	for _, payload := range []string{
		`{"session":{"model":"logical","sample_rate":24000},"unknown":false}`,
		`{"session":{"model":"upstream-model","sample_rate":16000},"unknown":false}`,
		`{"session":{"model":"upstream-model","sample_rate":24000}}`,
	} {
		if err := m.Match(n, wsMatchText(payload)); err == nil {
			t.Fatal("equal 被当成 ignore 或吞掉其它字段")
		}
	}
	for _, fields := range [][]WSFieldRule{
		{wsMatchEqual("/session/model", `"logical"`), wsMatchEqual("/session/model", `"logical"`)},
		{wsMatchEqual("/session", `{"model":"logical","sample_rate":24000}`), wsMatchEqual("/session/model", `"logical"`)},
		{wsMatchEqual("", `{}`), wsMatchEqual("/session/model", `"logical"`)},
	} {
		invalid := n
		invalid.Fields = fields
		if err := m.Match(invalid, *invalid.Message); err == nil {
			t.Fatal("重复或重叠 pointer 未被拒绝")
		}
	}
	for _, pointer := range []string{"/usage", "/sample_rate", "/text"} {
		invalid := wsMatchNode(`{"usage":"x","sample_rate":"x","text":"x"}`, wsMatchID(pointer, "bind", "task", "x"))
		if err := m.Match(invalid, *invalid.Message); err == nil {
			t.Fatal("ID 模式被用于普通内容")
		}
	}
	for _, value := range []string{`0`, `{}`, `null`, `false`, `""`} {
		invalid := wsMatchNode(`{"id":"fixture"}`, wsMatchID("/id", "bind", "task", "x"))
		if err := m.Match(invalid, wsMatchText(`{"id":`+value+`}`)); err == nil {
			t.Fatal("ID 模式匹配了非空字符串以外的值")
		}
		invalid.Message.Payload = []byte(`{"id":` + value + `}`)
		if err := m.Match(invalid, wsMatchText(`{"id":"actual"}`)); err == nil {
			t.Fatal("非法 fixture ID 未被拒绝")
		}
	}
	for _, pointer := range []string{"/a~2b", "/a~", "a", "/items/*/id", "/items/01/id", "/items/-/id", "/items/+0/id"} {
		invalid := wsMatchNode(`{"items":[{"id":"x"}]}`, wsMatchID(pointer, "bind", "item", "x"))
		if err := m.Match(invalid, *invalid.Message); err == nil {
			t.Fatal("非法 pointer 未被拒绝")
		}
	}
	escaped := wsMatchNode(`{"a/b":{"~key":[{"id":"fixture"}]}}`, wsMatchID("/a~1b/~0key/0/id", "bind", "item", "escaped"))
	if err := m.Match(escaped, wsMatchText(`{"a/b":{"~key":[{"id":"actual"}]}}`)); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"string", "literal", "ref", "ignore"} {
		invalid := wsMatchNode(`{"id":"x"}`, WSFieldRule{Pointer: "/id", Mode: mode})
		if err := m.Match(invalid, *invalid.Message); err == nil {
			t.Fatal("旧模式未被拒绝")
		}
	}
	null := wsMatchNode(`{"value":false}`, wsMatchEqual("/value", `null`))
	if err := m.Match(null, wsMatchText(`{"value":null}`)); err != nil {
		t.Fatal(err)
	}
	if err := m.Match(null, wsMatchText(`{}`)); err == nil {
		t.Fatal("equal null 吞掉了缺席字段")
	}
	if err := m.Match(wsMatchNode(`"logical"`, wsMatchEqual("", `"upstream-model"`)), wsMatchText(`"upstream-model"`)); err != nil {
		t.Fatal("RFC6901 根 pointer 不可用", err)
	}
	array := wsMatchNode(`{"items":[{"id":"fixture"},0]}`, wsMatchEqual("/items/1", `false`))
	if err := m.Match(array, wsMatchText(`{"items":[{"id":"fixture"},false]}`)); err != nil {
		t.Fatal("数组 pointer 替换未保留其它值", err)
	}
	if err := m.Match(array, wsMatchText(`{"items":[{"id":"changed"},false]}`)); err == nil {
		t.Fatal("数组 pointer 重写吞掉其它元素的改动")
	}
	for _, name := range []string{"event_id", "session_id", "response_id", "item_id", "call_id", "task_id", "id"} {
		n := wsMatchNode(`{"`+name+`":"fixture"}`, wsMatchID("/"+name, "bind", "allowed", name))
		if err := m.Match(n, wsMatchText(`{"`+name+`":"actual-`+name+`"}`)); err != nil {
			t.Fatal("限定 ID 终端名不可用", err)
		}
	}
	rootID := wsMatchNode(`"fixture"`, wsMatchID("", "bind", "task", "root"))
	if err := m.Match(rootID, wsMatchText(`"actual"`)); err == nil {
		t.Fatal("根 pointer 没有 ID 终端名却用于 bind")
	}
}

// 只能排除明确声明的标量 token，不能让重编码吞掉未知键、空白或键序。
func TestWSForwardedPayloadKeepsUnknownBytes(t *testing.T) {
	sent := wsMatchText(" {\n \"unknown\": [0, false, \"logical\"], \"session\" : {\"model\" : \"logical\", \"rate\":24000}\n} ")
	received := wsMatchText(" {\n \"unknown\": [0, false, \"logical\"], \"session\" : {\"model\" : \"upstream-model\", \"rate\":24000}\n} ")
	rules := []WSFieldRule{wsMatchEqual("/session/model", `"upstream-model"`)}
	if err := AssertWSForwardedPayload(sent, sent, nil); err != nil {
		t.Fatal(err)
	}
	if err := AssertWSForwardedPayload(sent, received, rules); err != nil {
		t.Fatal(err)
	}
	for name, payload := range map[string]string{
		"remarshal":               `{"unknown":[0,false,"logical"],"session":{"model":"upstream-model","rate":24000}}`,
		"key-order":               " {\n \"session\" : {\"model\" : \"upstream-model\", \"rate\":24000}, \"unknown\": [0, false, \"logical\"]\n} ",
		"unlisted-byte":           strings.Replace(string(received.Payload), "24000", "16000", 1),
		"unknown-string":          strings.Replace(string(received.Payload), `"logical"`, `"changed"`, 1),
		"unknown-number-spelling": strings.Replace(string(received.Payload), "[0,", "[0.0,", 1),
		"around-token-whitespace": strings.Replace(string(received.Payload), `"model" : `, `"model": `, 1),
		"deleted-field":           strings.Replace(string(received.Payload), `, "rate":24000`, "", 1),
		"wrong-target":            strings.Replace(string(received.Payload), "upstream-model", "wrong-model", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if err := AssertWSForwardedPayload(sent, wsMatchText(payload), rules); err == nil {
				t.Fatal("名单外 byte 改动未被拒绝")
			}
		})
	}
	if err := AssertWSForwardedPayload(sent, received, nil); err == nil {
		t.Fatal("未定义规则时不是全字节比较")
	}
	for _, fields := range [][]WSFieldRule{
		{wsMatchEqual("/session/model", `"upstream-model"`), wsMatchEqual("/session/model", `"upstream-model"`)},
		{wsMatchEqual("/session", `{"model":"upstream-model","rate":24000}`), rules[0]},
		{wsMatchEqual("/session", `{"model":"upstream-model","rate":24000}`)},
		{wsMatchEqual("/unknown", `[0,false,"logical"]`)},
		{wsMatchID("/session/model", "bind", "model", "x")},
		{wsMatchID("/session/model", "reference", "model", "x")},
		{wsMatchEqual("/missing", `null`)},
	} {
		if err := AssertWSForwardedPayload(sent, received, fields); err == nil {
			t.Fatal("不支持的 span 或规则未被拒绝")
		}
	}
	overlapPayload := wsMatchText(`{"a":{"id":"fixed"},"a-variant":0}`)
	overlapping := []WSFieldRule{wsMatchEqual("/a", `{"id":"fixed"}`), wsMatchEqual("/a-variant", `0`), wsMatchEqual("/a/id", `"fixed"`)}
	if err := AssertWSForwardedPayload(overlapPayload, overlapPayload, overlapping); err == nil {
		t.Fatal("转发允许了被兄弟键遮住的重叠 pointer")
	}
	duplicate := wsMatchText(`{"model":"a","model":"b"}`)
	if err := AssertWSForwardedPayload(duplicate, duplicate, []WSFieldRule{wsMatchEqual("/model", `"b"`)}); err == nil {
		t.Fatal("重复键通过了 span 比较")
	}
	binary := WSMessage{Opcode: ws.OpBinary, Payload: []byte{0, 255, 1}}
	if err := AssertWSForwardedPayload(binary, binary, nil); err != nil {
		t.Fatal(err)
	}
	if err := AssertWSForwardedPayload(binary, binary, rules); err == nil {
		t.Fatal("binary 使用了 JSON span 改写")
	}
	wrongOpcode := received
	wrongOpcode.Opcode = ws.OpBinary
	if err := AssertWSForwardedPayload(sent, wrongOpcode, rules); err == nil {
		t.Fatal("转发改变了 opcode")
	}
	t.Run("escaped-array-and-root-spans", func(t *testing.T) {
		from := wsMatchText(`{"a/b":{"~key":[false,null,0]},"z":"keep"}`)
		to := wsMatchText(`{"a/b":{"~key":[true,"x",42]},"z":"keep"}`)
		fields := []WSFieldRule{wsMatchEqual("/a~1b/~0key/2", `42`), wsMatchEqual("/a~1b/~0key/0", `true`), wsMatchEqual("/a~1b/~0key/1", `"x"`)}
		if err := AssertWSForwardedPayload(from, to, fields); err != nil {
			t.Fatal(err)
		}
		if err := AssertWSForwardedPayload(wsMatchText(" \"old\" \n"), wsMatchText(" \"new\" \n"), []WSFieldRule{wsMatchEqual("", `"new"`)}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("escaped-scalar-token-and-type-rewrite", func(t *testing.T) {
		for _, pair := range [][3]string{
			{` {"model":"comma,: quote\" slash\\ unicode\u0061","keep":1e400} `, ` {"model":"upstream-model","keep":1e400} `, `"upstream-model"`},
			{`{"model":null}`, `{"model":false}`, `false`},
			{`{"model":false}`, `{"model":9007199254740993}`, `9007199254740993`},
			{`{"model":"logical"}`, `{"model":[]}`, `[]`},
			{`{"model":{}}`, `{"model":"upstream-model"}`, `"upstream-model"`},
		} {
			err := AssertWSForwardedPayload(wsMatchText(pair[0]), wsMatchText(pair[1]), []WSFieldRule{wsMatchEqual("/model", pair[2])})
			wantOK := pair[2] != `[]` && pair[0] != `{"model":{}}`
			if (err == nil) != wantOK {
				t.Fatalf("标量边界 span 比较错误: %v", err)
			}
		}
	})
}

func TestWSMaterializeOnlyConfirmedReferences(t *testing.T) {
	t.Run("untouched-bytes-have-independent-storage", func(t *testing.T) {
		m := wsTestMatcher(t, DefaultWSLimits())
		for _, payload := range [][]byte{[]byte{0, 255, 1}, []byte{}} {
			n := WSNode{Kind: "message", Message: &WSMessage{Opcode: ws.OpBinary, Payload: payload}}
			got, err := m.Materialize(n)
			if err != nil || got.Opcode != ws.OpBinary || !bytes.Equal(got.Payload, payload) {
				t.Fatal(err)
			}
			if len(got.Payload) != 0 {
				got.Payload[0] = 42
				if payload[0] != 0 {
					t.Fatal("bytes Materialize 与 fixture 共用负载")
				}
			}
		}
	})
	m := wsTestMatcher(t, DefaultWSLimits())
	bind := wsMatchNode(`{"id":"fixture"}`, wsMatchID("/id", "bind", "item", "first"))
	ref := wsMatchNode(` { "id": "fixture", "extra": [0,false,null] } `, wsMatchID("/id", "reference", "item", "first"))
	if _, err := m.Materialize(ref); err == nil {
		t.Fatal("reference 生成了不存在的 ID")
	}
	if err := m.Match(bind, wsMatchText(`{"id":"real-1"}`)); err != nil {
		t.Fatal(err)
	}
	got, err := m.Materialize(ref)
	if err != nil || got.Opcode != ws.OpText || string(got.Payload) != ` { "id": "real-1", "extra": [0,false,null] } ` {
		t.Fatal("引用替换破坏了其它原始 byte", err)
	}
	if err := m.Match(ref, got); err != nil {
		t.Fatal(err)
	}
	got.Payload[0] = 'x'
	if string(ref.Message.Payload) != ` { "id": "fixture", "extra": [0,false,null] } ` {
		t.Fatal("Materialize 返回值与 fixture 共用负载")
	}
	equal := wsMatchNode(`{"model":"logical"}`, wsMatchEqual("/model", `"upstream-model"`))
	unchanged, err := m.Materialize(equal)
	if err != nil || string(unchanged.Payload) != `{"model":"logical"}` {
		t.Fatal("Materialize 不应计算转换预期", err)
	}
	// 绑定中的转义字符只能编码为单一字符串值，不能注入字段或破坏 JSON。
	escapedBind := wsMatchNode(`{"id":"fixture"}`, wsMatchID("/id", "bind", "item", "escaped"))
	if err := m.Match(escapedBind, wsMatchText(`{"id":"quote\"\\line\n"}`)); err != nil {
		t.Fatal(err)
	}
	escapedRef := wsMatchNode(`{"id":"fixture"}`, wsMatchID("/id", "reference", "item", "escaped"))
	escaped, err := m.Materialize(escapedRef)
	if err != nil || string(escaped.Payload) != `{"id":"quote\"\\line\n"}` {
		t.Fatal("引用未正确转义", err)
	}
}

// 解码器不能把错误 surrogate 折叠为合法 replacement character 后认可固定文本或实体。
func TestWSMatchRejectsSurrogateCollisions(t *testing.T) {
	t.Run("fixed-text", func(t *testing.T) {
		m := wsTestMatcher(t, DefaultWSLimits())
		n := wsMatchNode(`{"text":"\ufffd"}`)
		if err := m.Match(n, wsMatchText(`{"text":"\ud800"}`)); err == nil {
			t.Fatal("未配对 surrogate 与固定 U+FFFD 错误匹配")
		}
	})
	t.Run("wrong-reference", func(t *testing.T) {
		m := wsTestMatcher(t, DefaultWSLimits())
		bind := wsMatchNode(`{"task_id":"fixture"}`, wsMatchID("/task_id", "bind", "task", "first"))
		if err := m.Match(bind, wsMatchText(`{"task_id":"\ufffd"}`)); err != nil {
			t.Fatal(err)
		}
		ref := wsMatchNode(`{"task_id":"fixture"}`, wsMatchID("/task_id", "reference", "task", "first"))
		if err := m.Match(ref, wsMatchText(`{"task_id":"\ud800"}`)); err == nil {
			t.Fatal("错误 surrogate ID 被当成已确认引用")
		}
		if err := m.Match(ref, wsMatchText(`{"task_id":"�"}`)); err != nil {
			t.Fatal("错误引用污染已确认的合法 ID", err)
		}
	})
	t.Run("equal-forward-target", func(t *testing.T) {
		sent := wsMatchText(`{"model":"logical"}`)
		rules := []WSFieldRule{wsMatchEqual("/model", `"\ufffd"`)}
		if err := AssertWSForwardedPayload(sent, wsMatchText(`{"model":"\ud800"}`), rules); err == nil {
			t.Fatal("转发 equal 错误认可 surrogate 目标")
		}
	})
	t.Run("valid-unicode-controls", func(t *testing.T) {
		for _, pair := range [][2]string{{`"\ufffd"`, `"�"`}, {`"\ud83d\ude00"`, `"😀"`}, {`"\u0061"`, `"a"`}} {
			m := wsTestMatcher(t, DefaultWSLimits())
			if err := m.Match(wsMatchNode(`{"text":`+pair[0]+`}`), wsMatchText(`{"text":`+pair[1]+`}`)); err != nil {
				t.Fatal("合法 Unicode 固定文本不应被拒绝", err)
			}
			bind := wsMatchNode(`{"task_id":"fixture"}`, wsMatchID("/task_id", "bind", "task", "first"))
			if err := m.Match(bind, wsMatchText(`{"task_id":`+pair[0]+`}`)); err != nil {
				t.Fatal(err)
			}
			ref := wsMatchNode(`{"task_id":"fixture"}`, wsMatchID("/task_id", "reference", "task", "first"))
			if err := m.Match(ref, wsMatchText(`{"task_id":`+pair[1]+`}`)); err != nil {
				t.Fatal("合法 Unicode 引用不应被拒绝", err)
			}
			if err := AssertWSForwardedPayload(wsMatchText(`{"model":"logical"}`), wsMatchText(`{"model":`+pair[1]+`}`), []WSFieldRule{wsMatchEqual("/model", pair[0])}); err != nil {
				t.Fatal("合法 Unicode equal 目标不应被拒绝", err)
			}
		}
	})
	t.Run("invalid-fixture-and-equal-value", func(t *testing.T) {
		m := wsTestMatcher(t, DefaultWSLimits())
		invalid := wsMatchNode(`{"text":"\udc00"}`)
		if err := m.Match(invalid, wsMatchText(`{"text":"�"}`)); err == nil {
			t.Fatal("fixture surrogate 被合法实际文本掩盖")
		}
		if _, err := m.Materialize(invalid); err == nil {
			t.Fatal("非法 fixture 被物化")
		}
		equal := wsMatchNode(`{"model":"logical"}`, wsMatchEqual("/model", `"\udc00"`))
		if err := m.Match(equal, wsMatchText(`{"model":"�"}`)); err == nil {
			t.Fatal("非法 equal value 被接纳")
		}
		if err := AssertWSForwardedPayload(*equal.Message, wsMatchText(`{"model":"�"}`), equal.Fields); err == nil {
			t.Fatal("转发非法 equal value 被接纳")
		}
	})
	t.Run("invalid-bind-does-not-commit", func(t *testing.T) {
		m := wsTestMatcher(t, DefaultWSLimits())
		bind := wsMatchNode(`{"task_id":"fixture"}`, wsMatchID("/task_id", "bind", "task", "first"))
		if err := m.Match(bind, wsMatchText(`{"task_id":"\ud800"}`)); err == nil {
			t.Fatal("非法 bind ID 被接纳")
		}
		ref := wsMatchNode(`{"task_id":"fixture"}`, wsMatchID("/task_id", "reference", "task", "first"))
		if err := m.Match(ref, wsMatchText(`{"task_id":"�"}`)); err == nil {
			t.Fatal("非法 bind 留下实体状态")
		}
		if err := m.Match(bind, wsMatchText(`{"task_id":"�"}`)); err != nil {
			t.Fatal("非法 bind 消耗预算或实际 ID", err)
		}
	})
}

func TestWSMatcherErrorsDoNotExposePayload(t *testing.T) {
	const secret = "sensitive-task-value"
	m := wsTestMatcher(t, DefaultWSLimits())
	bind := wsMatchNode(`{"task_id":"fixture"}`, wsMatchID("/task_id", "bind", "task", secret))
	if err := m.Match(bind, wsMatchText(`{"task_id":"confirmed"}`)); err != nil {
		t.Fatal(err)
	}
	ref := wsMatchNode(`{"task_id":"fixture"}`, wsMatchID("/task_id", "reference", "task", secret))
	for _, err := range []error{
		m.Match(ref, wsMatchText(`{"task_id":"`+secret+`"}`)),
		m.Match(wsMatchNode(`{"text":"fixture"}`), wsMatchText(`{"text":"`+secret+`"}`)),
		m.Match(wsMatchNode(`{"text":"fixture"}`), wsMatchText(`{"`+secret+`":0,"`+secret+`":0}`)),
		AssertWSForwardedPayload(wsMatchText(`{"text":"fixture"}`), wsMatchText(`{"text":"`+secret+`"}`), nil),
	} {
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Fatal("错误缺席或暴露了敏感内容")
		}
	}
}

func TestWSMatcherRejectsInvalidRulesAndLimits(t *testing.T) {
	invalidLimits := DefaultWSLimits()
	invalidLimits.Bindings = 0
	if _, err := NewWSMatcher(invalidLimits); err == nil {
		t.Fatal("非正预算关闭了限制")
	}
	m := wsTestMatcher(t, DefaultWSLimits())
	for _, fields := range [][]WSFieldRule{
		{{Pointer: "/id", Mode: "bind", Symbol: "x"}},
		{{Pointer: "/id", Mode: "reference", Namespace: "task"}},
		{{Pointer: "/id", Mode: "bind", Namespace: "task", Symbol: "x", Value: json.RawMessage(`"x"`)}},
		{{Pointer: "/id", Mode: "equal"}},
		{{Pointer: "/id", Mode: "equal", Namespace: "task", Value: json.RawMessage(`"x"`)}},
		{{Pointer: "/id", Mode: "equal", Value: json.RawMessage(`{"x":0,"x":1}`)}},
	} {
		n := wsMatchNode(`{"id":"x"}`, fields...)
		if err := m.Match(n, *n.Message); err == nil {
			t.Fatal("非法规则未被拒绝")
		}
		if _, err := m.Materialize(n); err == nil {
			t.Fatal("Materialize 未验证规则")
		}
	}
	for _, mode := range []string{"bytes", "", "unknown"} {
		n := wsMatchNode(`{"id":"x"}`, wsMatchID("/id", "bind", "task", "x"))
		n.Match = mode
		if err := m.Match(n, *n.Message); err == nil {
			t.Fatal("非 JSON 模式接受字段规则")
		}
		if _, err := m.Materialize(n); err == nil {
			t.Fatal("非 JSON Materialize 接受字段规则")
		}
	}
	binaryJSON := wsMatchNode(`{}`)
	binaryJSON.Message.Opcode = ws.OpBinary
	if err := m.Match(binaryJSON, *binaryJSON.Message); err == nil {
		t.Fatal("binary 接受 JSON 匹配")
	}
	for _, n := range []WSNode{
		{}, {Kind: "close", Message: &WSMessage{Opcode: ws.OpText, Payload: []byte(`{}`)}},
		wsMatchNode(""), wsMatchNode(string([]byte{255})),
	} {
		if err := m.Match(n, wsMatchText(`{}`)); err == nil {
			t.Fatal("非法消息节点未被拒绝")
		}
		if _, err := m.Materialize(n); err == nil {
			t.Fatal("非法消息节点被物化")
		}
	}
	t.Run("message-and-field-budgets", func(t *testing.T) {
		limits := DefaultWSLimits()
		limits.MessageBytes = 8
		small := wsTestMatcher(t, limits)
		if err := small.Match(wsMatchNode(`"short"`), wsMatchText(`"too-long"`)); err == nil {
			t.Fatal("实际消息超限未被拒绝")
		}
		if _, err := small.Materialize(wsMatchNode(`"too-long"`)); err == nil {
			t.Fatal("fixture 消息超限未被拒绝")
		}
		limits = DefaultWSLimits()
		limits.FieldRules = 1
		small = wsTestMatcher(t, limits)
		n := wsMatchNode(`{"a":0,"b":0}`, wsMatchEqual("/a", `0`), wsMatchEqual("/b", `0`))
		if err := small.Match(n, *n.Message); err == nil {
			t.Fatal("字段规则超限未被拒绝")
		}
		limits = DefaultWSLimits()
		limits.JSONDepth = 1
		small = wsTestMatcher(t, limits)
		if err := small.Match(wsMatchNode(`{"a":[]}`), wsMatchText(`{"a":[]}`)); err == nil {
			t.Fatal("注入深度预算未生效")
		}
	})
	t.Run("materialized-final-size", func(t *testing.T) {
		limits := DefaultWSLimits()
		limits.MessageBytes = 48
		small := wsTestMatcher(t, limits)
		for _, entry := range []struct{ symbol, actual string }{{"long", `{"id":"abcdefghijklmnopqrst"}`}, {"short", `{"id":"x"}`}} {
			bind := wsMatchNode(`{"id":"fixture"}`, wsMatchID("/id", "bind", "item", entry.symbol))
			if err := small.Match(bind, wsMatchText(entry.actual)); err != nil {
				t.Fatal(err)
			}
		}
		ref := wsMatchNode(`{"call_id":"x","item_id":"abcdefghijklmnopqrst"}`,
			wsMatchID("/call_id", "reference", "item", "long"), wsMatchID("/item_id", "reference", "item", "short"))
		got, err := small.Materialize(ref)
		if err != nil || string(got.Payload) != `{"call_id":"abcdefghijklmnopqrst","item_id":"x"}` {
			t.Fatal("同长度交换不应被中间大小而非最终大小拒绝", err)
		}
	})
	t.Run("materialized-size", func(t *testing.T) {
		limits := DefaultWSLimits()
		limits.MessageBytes = 32
		small := wsTestMatcher(t, limits)
		bind := wsMatchNode(`{"id":"x"}`, wsMatchID("/id", "bind", "item", "x"))
		if err := small.Match(bind, wsMatchText(`{"id":"abcdefghijklmnop"}`)); err != nil {
			t.Fatal(err)
		}
		ref := wsMatchNode(`{"id":"x","pad":"123456789"}`, wsMatchID("/id", "reference", "item", "x"))
		if _, err := small.Materialize(ref); err == nil {
			t.Fatal("引用替换后消息超过预算")
		}
	})
}
