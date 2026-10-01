package testkit

import (
	"encoding/json"
	"strings"
	"testing"
)

// 结构指针的 null 等同缺席，而合法 false/0 字面值不得作为缺席处理。
func TestWSReadRequiredFields(t *testing.T) {
	paths := []string{
		"/name", "/note", "/request", "/request/method", "/request/path",
		"/response", "/response/status", "/response/ws",
		"/response/ws/version", "/response/ws/client_protocol", "/response/ws/client_version",
		"/response/ws/upstream", "/response/ws/upstream/method", "/response/ws/upstream/path",
		"/response/ws/upstream_expected_status", "/response/ws/provenance", "/response/ws/provenance/kind",
		"/response/ws/nodes", "/response/ws/nodes/0/id", "/response/ws/nodes/0/point", "/response/ws/nodes/0/source",
		"/response/ws/nodes/0/kind", "/response/ws/nodes/0/message", "/response/ws/nodes/0/message/opcode", "/response/ws/nodes/0/message/payload",
		"/response/ws/nodes/4/close_code", "/response/ws/outcome", "/response/ws/outcome/kind", "/response/ws/outcome/terminal",
		"/response/ws/outcome/terminal/0/node", "/response/ws/outcome/terminal/0/namespace", "/response/ws/outcome/terminal/0/symbol",
		"/response/ws/outcome/terminal/0/id_pointer", "/response/ws/outcome/terminal/0/state_pointer", "/response/ws/outcome/terminal/0/state",
		"/response/ws/coverage/0/capability", "/response/ws/coverage/0/nodes", "/response/ws/coverage/0/note",
	}
	for _, pointer := range paths {
		for _, remove := range []bool{false, true} {
			mode := "null"
			if remove {
				mode = "missing"
			}
			t.Run(pointer+"/"+mode, func(t *testing.T) {
				var value any
				if err := json.Unmarshal(wsFixtureBytes(t), &value); err != nil {
					t.Fatal(err)
				}
				last := 0
				for i := range pointer {
					if pointer[i] == '/' {
						last = i
					}
				}
				parent, ok := wsJSONPointer(value, pointer[:last])
				if !ok {
					t.Fatal("测试输入 pointer 无效")
				}
				object, ok := parent.(map[string]any)
				if !ok {
					t.Fatal("测试输入父节点不是对象")
				}
				if remove {
					delete(object, pointer[last+1:])
				} else {
					object[pointer[last+1:]] = nil
				}
				raw, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				path := writeWSInput(t, t.TempDir(), "fixture.json", raw)
				if _, err := ReadWSFixture(path, DefaultWSLimits()); err == nil {
					t.Fatal("必需字段缺席或 null 被接纳")
				}
			})
		}
	}
}

// 根 pointer 合法但字段必须显式存在，不能让结构解码零值补出一个根规则。
func TestWSReadRootPointerPresence(t *testing.T) {
	const raw = `{"name":"root","note":"合成根 pointer 机制测试","request":{"method":"GET","path":"/v1/realtime"},"response":{"status":101,"ws":{"version":1,"client_protocol":"openai.realtime","client_version":"synthetic-v1","upstream":{"method":"GET","path":"/v1/realtime"},"upstream_expected_status":101,"provenance":{"kind":"synthetic-negative"},"nodes":[{"id":"bind","point":"upstream.send","source":"synthetic","kind":"message","message":{"opcode":1,"payload":"InIi"},"match":"json","fields":[{"pointer":"","mode":"bind","namespace":"response","symbol":"r"}]},{"id":"cs","point":"client.send","source":"synthetic","kind":"close","close_code":1000},{"id":"ur","point":"upstream.receive","source":"synthetic","kind":"close","close_code":1000},{"id":"us","point":"upstream.send","source":"synthetic","kind":"close","close_code":1000},{"id":"cr","point":"client.receive","source":"synthetic","kind":"close","close_code":1000}],"outcome":{"kind":"interrupted"}}}}`
	for _, mode := range []string{"present", "missing", "null"} {
		t.Run(mode, func(t *testing.T) {
			input := raw
			if mode == "missing" {
				input = strings.Replace(input, `"pointer":"",`, "", 1)
			} else if mode == "null" {
				input = strings.Replace(input, `"pointer":""`, `"pointer":null`, 1)
			}
			_, err := ReadWSFixture(writeWSInput(t, t.TempDir(), "root.json", []byte(input)), DefaultWSLimits())
			if (err == nil) != (mode == "present") {
				t.Fatalf("root pointer 存在性未保留: %v", err)
			}
		})
	}
}

// literal 只验证声明，不做动态匹配；false/0/null 都是合法字面预期。
func TestWSReadLiteralValues(t *testing.T) {
	for _, value := range []string{"false", "0", "null"} {
		f := syntheticEnvelope()
		f.Response.WS.Nodes[0].Message.Payload = []byte(`{"value":` + value + `}`)
		f.Response.WS.Nodes[0].Fields = []WSFieldRule{{Pointer: "/value", Mode: "equal", Value: json.RawMessage(value)}}
		raw, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ReadWSFixture(writeWSInput(t, t.TempDir(), "fixture.json", raw), DefaultWSLimits()); err != nil {
			t.Fatal(err)
		}
	}
}
