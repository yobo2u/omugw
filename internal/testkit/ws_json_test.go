package testkit

import (
	"encoding/json"
	"strings"
	"testing"
)

// 深度边界必须在容器开头拦截，不能依赖解码后才计算深度。
func TestWSJSONDepthBoundary(t *testing.T) {
	for _, tt := range []struct {
		raw   string
		limit int
		valid bool
	}{
		{`0`, 1, true}, {`false`, 1, true}, {`null`, 1, true},
		{strings.Repeat(`[`, 64) + `0` + strings.Repeat(`]`, 64), 64, true},
		{strings.Repeat(`[`, 65) + `0` + strings.Repeat(`]`, 65), 64, false},
		{`{"a":{"b":0}}`, 1, false}, {`{"a":{"b":0}}`, 2, true},
		{`{"a":0,"\u0061":false}`, 64, false},
		{`{"a":0} {}`, 64, false}, {`[`, 64, false},
	} {
		if _, err := strictWSJSON([]byte(tt.raw), tt.limit); (err == nil) != tt.valid {
			t.Fatalf("depth=%d valid=%v error=%v", tt.limit, tt.valid, err)
		}
	}
}

// 未配对 surrogate 不能被解码成合法 U+FFFD，键名与字符串值都必须在有损解码前拒绝。
func TestWSJSONRejectsUnpairedSurrogates(t *testing.T) {
	for _, tt := range []struct{ name, raw string }{
		{"high", `"\ud800"`}, {"high-upper-bound", `"\uDBFF"`},
		{"low", `"\udc00"`}, {"low-upper-bound", `"\uDFFF"`},
		{"high-before-text", `"\ud800x"`}, {"high-before-scalar", `"\ud800\u0061"`},
		{"two-highs", `"\ud800\ud800"`}, {"reversed", `"\udc00\ud800"`},
		{"high-before-literal-escape", `"\ud800\\udc00"`},
		{"high-before-utf8", `"\ud800😀"`}, {"extra-low", `"\ud83d\ude00\udc00"`},
		{"key", `{"\ud800":0}`}, {"nested-value", `{"v":["\udc00"]}`},
		{"truncated-escape", `"\ud80"`}, {"bad-hex", `"\ud80g"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := strictWSJSON([]byte(tt.raw), 64); err == nil {
				t.Fatal("未配对或非法 Unicode escape 被接纳")
			}
		})
	}
}

func TestWSJSONPreservesValidUnicode(t *testing.T) {
	for _, tt := range []struct{ name, raw, want string }{
		{"replacement-escape", `"\ufffd"`, "�"}, {"replacement-utf8", `"�"`, "�"},
		{"pair", `"\ud83d\ude00"`, "😀"}, {"uppercase-pair", `"\uD83D\uDE00"`, "😀"},
		{"pair-lower-bound", `"\uD800\uDC00"`, "\U00010000"},
		{"pair-upper-bound", `"\uDBFF\uDFFF"`, "\U0010ffff"},
		{"utf8", `"中文😀"`, "中文😀"}, {"scalar-escape", `"\u0061\ud7ff\ue000"`, "a\ud7ff\ue000"},
		{"escaped-backslash", `"\\ud800"`, `\ud800`},
		{"escaped-quote", `"\"\u0061\\\""`, "\"a\\\""},
		{"control-escapes", `"\b\f\n\r\t\/"`, "\b\f\n\r\t/"},
		{"paired-and-scalar", `"\ud83d\ude00\u0061\ufffd"`, "😀a�"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := strictWSJSON([]byte(tt.raw), 64)
			if err != nil || got != tt.want {
				t.Fatalf("合法 Unicode 未保全: %v", err)
			}
		})
	}
	got, err := strictWSJSON([]byte(`{"\ud83d\ude00":"\ufffd","literal":"\\ud800"}`), 64)
	if err != nil {
		t.Fatal(err)
	}
	object, ok := got.(map[string]any)
	if !ok || object["😀"] != "�" || object["literal"] != `\ud800` {
		t.Fatal("合法键名或字面 escape 未保全")
	}
}

// 共享入口收紧后，外层 fixture、base64 消息、字段预期和契约摘要均不能另走有损路径。
func TestWSJSONConsumersRejectUnpairedSurrogates(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*Fixture)
	}{
		{"payload", func(f *Fixture) {
			f.Response.WS.Nodes[0].Message.Payload = []byte(`{"type":"response.create","text":"\ud800"}`)
		}},
		{"equal-value", func(f *Fixture) {
			f.Response.WS.Nodes[0].Fields = []WSFieldRule{{Pointer: "/type", Mode: "equal", Value: json.RawMessage(`"\udc00"`)}}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := syntheticEnvelope()
			tt.mutate(&f)
			if err := ValidateWSSession(f, DefaultWSLimits()); err == nil {
				t.Error("静态校验接纳未配对 surrogate")
			}
			raw, err := json.Marshal(f)
			if err != nil {
				t.Fatal(err)
			}
			path := writeWSInput(t, t.TempDir(), "fixture.json", raw)
			if _, err := ReadWSFixture(path, DefaultWSLimits()); err == nil {
				t.Error("fixture 读取接纳未配对 surrogate")
			}
		})
	}
	t.Run("outer-fixture", func(t *testing.T) {
		// 仅替换原 note 字面值，不能让未知字段校验抢先掩盖根因。
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(wsFixtureBytes(t), &envelope); err != nil {
			t.Fatal(err)
		}
		envelope["note"] = json.RawMessage(`"\ud800"`)
		raw, err := json.Marshal(envelope)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ReadWSFixture(writeWSInput(t, t.TempDir(), "fixture.json", raw), DefaultWSLimits()); err == nil {
			t.Fatal("fixture 外层字符串被有损解码")
		}
	})
	t.Run("contract-digest", func(t *testing.T) {
		f := syntheticEnvelope()
		f.Response.WS.Upstream.Body = json.RawMessage(`{"text":"\ud800"}`)
		if _, err := WSContractDigest(*f.Response.WS); err == nil {
			t.Fatal("契约摘要将非法 surrogate 当合法值规范化")
		}
	})
	t.Run("valid-consumers", func(t *testing.T) {
		f := syntheticEnvelope()
		f.Response.WS.Nodes[0].Message.Payload = []byte(`{"type":"response.create","text":"\ufffd\ud83d\ude00"}`)
		if err := ValidateWSSession(f, DefaultWSLimits()); err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ReadWSFixture(writeWSInput(t, t.TempDir(), "fixture.json", raw), DefaultWSLimits()); err != nil {
			t.Fatal(err)
		}
		f.Response.WS.Upstream.Body = json.RawMessage(`{"text":"\ufffd\ud83d\ude00"}`)
		if _, err := WSContractDigest(*f.Response.WS); err != nil {
			t.Fatal(err)
		}
	})
}

// pointer 只能精确定位，false/0/null 与不存在必须可区分。
func TestWSJSONPointer(t *testing.T) {
	value, err := strictWSJSON([]byte(`{"a/b":{"~key":[false,0,null]}}`), 64)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		pointer string
		want    any
		exists  bool
	}{
		{"/a~1b/~0key/0", false, true}, {"/a~1b/~0key/1", json.Number("0"), true}, {"/a~1b/~0key/2", nil, true},
		{"/a~1b/~0key/3", nil, false}, {"/a~1b/~0key/-1", nil, false}, {"/a~1b/~0key/01", nil, false},
		{"/a~1b/~0key/+1", nil, false}, {"/a~2b", nil, false}, {"/a~", nil, false}, {"a", nil, false},
	} {
		got, exists := wsJSONPointer(value, tt.pointer)
		if exists != tt.exists || got != tt.want {
			t.Fatalf("pointer %s 存在性或值错误", tt.pointer)
		}
	}
}
