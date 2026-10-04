//go:build smoke

package smoke_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/transport/ws"
)

const dsValidToolOutput = `[{"type":"function_call","name":"test_color","call_id":"call_a","arguments":"{}"},{"type":"function_call","name":"test_shape","call_id":"call_b","arguments":"{}"}]`

// 第二项和尾部故意损坏，防止验证第一项后就提交部分结果。
func dsToolSetCases() []struct {
	name, output string
	valid        bool
} {
	return []struct {
		name, output string
		valid        bool
	}{
		{"valid", dsValidToolOutput, true},
		{"with_message", strings.Replace(dsValidToolOutput, `[{`, `[{"type":"message","role":"assistant","content":[{"type":"text","text":"蓝色方块"}]},{`, 1), true},
		{"same_call_id_different_names", strings.Replace(dsValidToolOutput, "call_b", "call_a", 1), false},
		{"duplicate_name", strings.Replace(dsValidToolOutput, "test_shape", "test_color", 1), false},
		{"missing_tool", `[{"type":"function_call","name":"test_color","call_id":"call_a","arguments":"{}"}]`, false},
		{"unknown_name", strings.Replace(dsValidToolOutput, "test_shape", "other", 1), false},
		{"extra_tool", strings.TrimSuffix(dsValidToolOutput, "]") + `,{"type":"function_call","name":"other","call_id":"call_c","arguments":"{}"}]`, false},
		{"empty_call_id", strings.Replace(dsValidToolOutput, "call_b", "", 1), false},
		{"wrong_call_id_type", strings.Replace(dsValidToolOutput, `"call_b"`, `123`, 1), false},
		{"bad_arguments_json", strings.Replace(dsValidToolOutput, `"call_b","arguments":"{}"`, `"call_b","arguments":"{"`, 1), false},
		{"wrong_arguments_type", strings.Replace(dsValidToolOutput, `"call_b","arguments":"{}"`, `"call_b","arguments":{}`, 1), false},
		{"null_arguments", strings.Replace(dsValidToolOutput, `"call_b","arguments":"{}"`, `"call_b","arguments":"null"`, 1), false},
		{"missing_arguments", strings.Replace(dsValidToolOutput, `"call_b","arguments":"{}"`, `"call_b"`, 1), false},
		{"wrong_item_type", strings.Replace(dsValidToolOutput, `{"type":"function_call","name":"test_shape"`, `{"type":"function_call_output","name":"test_shape"`, 1), false},
		{"non_object_item", strings.TrimSuffix(dsValidToolOutput, "]") + `,null]`, false},
		{"output_not_array", `{}`, false},
	}
}

func dsToolSetDone(output string) string {
	return `{"type":"response.done","response":{"id":"r2","status":"completed","output":` + output + `,"usage":{"output_tokens":30}}}`
}

func TestDSRealtimeRecorderOfflineToolSetPreflight(t *testing.T) {
	for _, tc := range dsToolSetCases() {
		t.Run(tc.name, func(t *testing.T) {
			r := dsIdentityCapture(t, "text-tools", func(p *dsOfflinePeer) {
				p.update("text-tools")
				p.read("conversation.item.create")
				p.write(dsObservedServerItem)
				p.read("response.create")
				p.write(`{"type":"response.done","response":{"id":"r1","status":"completed","output":[]}}`)
				p.read("conversation.item.create")
				p.write(`{"type":"conversation.item.created","item":{"id":"server_user_2","type":"message","role":"user","content":[{"type":"input_text","text":"先复述刚才口令，再在同一轮调用 test_color 和 test_shape，不要猜测结果。"}]}}`)
				p.read("response.create")
				p.write(dsToolSetDone(tc.output))
				// ack 只让旧驱动暴露全部错误发送；断言针对录制器发出的消息与结果。
				for n := 0; p.err == nil; n++ {
					op, b, err := p.c.ReadMessage()
					if err != nil {
						p.err = err
						return
					}
					var e map[string]any
					if op != ws.OpText || json.Unmarshal(b, &e) != nil {
						t.Error("客户端非JSON消息")
						return
					}
					switch dsString(e, "type") {
					case "conversation.item.create":
						item := dsMap(e, "item")
						item["id"] = fmt.Sprintf("server_result_%d", n)
						ack, _ := json.Marshal(map[string]any{"type": "conversation.item.created", "item": item})
						p.write(string(ack))
					case "response.create":
						p.write(`{"type":"response.done","response":{"id":"r3","status":"completed"}}`)
					default:
						t.Error("工具阶段发出意外消息")
						return
					}
				}
			})
			var results []string
			responses := 0
			for _, rec := range r.Records {
				if rec.Direction != "send" || rec.Kind != "message" {
					continue
				}
				var e map[string]any
				_ = json.Unmarshal(rec.Payload, &e)
				if dsString(e, "type") == "response.create" {
					responses++
				}
				if dsString(e, "item", "type") == "function_call_output" {
					results = append(results, dsString(e, "item", "call_id")+"="+dsString(e, "item", "output"))
				}
			}
			if !tc.valid {
				if len(results) != 0 || responses != 2 {
					t.Errorf("非法工具集合已部分发送: results=%v response.create=%d", results, responses)
				}
				if r.Failure != "invalid_tool_set" {
					t.Errorf("failure=%q; want invalid_tool_set", r.Failure)
				}
				return
			}
			if r.Failure != "" || strings.Join(results, ",") != "call_a=蓝色,call_b=方块" || responses != 3 {
				t.Fatalf("合法工具闭环失败: failure=%s results=%v responses=%d", r.Failure, results, responses)
			}
			f, err := dsCandidate(r)
			if err != nil {
				t.Fatal(err)
			}
			dsReplayCandidate(t, f)
		})
	}
}

func TestDSRealtimeRecorderOfflineToolSetEvidence(t *testing.T) {
	for _, tc := range dsToolSetCases() {
		t.Run(tc.name, func(t *testing.T) {
			r := dsToolSetTrace(tc.output)
			if tc.name == "same_call_id_different_names" {
				// 模拟旧驱动确实向同call_id回传两份不同结果，ack也一致，不靠错ack拒绝它。
				for i := range r.Records {
					r.Records[i].Payload = bytes.ReplaceAll(r.Records[i].Payload, []byte("call_b"), []byte("call_a"))
				}
			}
			var events []dsEvidenceEvent
			for i, rec := range r.Records {
				var e map[string]any
				_ = json.Unmarshal(rec.Payload, &e)
				if rec.Direction == "receive" && rec.Kind == "message" {
					events = append(events, dsEvidenceEvent{Node: fmt.Sprintf("n%04d_1", i), Value: e})
				}
				if rec.Direction == "send" && dsString(e, "item", "type") == "function_call_output" && dsRequestWitness(r.Scenario, r.Records, i, e) != tc.valid {
					t.Error("工具结果见证未验证完整工具集合")
				}
			}
			caps := map[string]bool{}
			for _, c := range dsCoverage(r, events) {
				caps[c.Capability] = true
			}
			if caps["tool_calling"] != tc.valid || caps["parallel_tool_calls"] != tc.valid {
				t.Errorf("工具Coverage与完整集合校验不一致: %v", caps)
			}
			f, err := dsCandidate(r)
			if (err == nil) != tc.valid {
				t.Fatalf("candidate err=%v; want valid=%v", err, tc.valid)
			}
			if tc.valid {
				dsReplayCandidate(t, f)
			}
		})
	}
}

func TestDSRealtimeRecorderOfflineToolResultWitnessMapping(t *testing.T) {
	for _, name := range []string{"foreign_id", "wrong_result", "duplicate_result"} {
		t.Run(name, func(t *testing.T) {
			r := dsToolSetTrace(dsValidToolOutput)
			switch name {
			case "foreign_id", "wrong_result":
				from, to := "call_a", "foreign"
				if name == "wrong_result" {
					from, to = "蓝色", "方块"
				}
				for _, i := range []int{4, 5} {
					r.Records[i].Payload = bytes.ReplaceAll(r.Records[i].Payload, []byte(from), []byte(to))
				}
			case "duplicate_result":
				for _, i := range []int{6, 7} {
					r.Records[i].Payload = bytes.ReplaceAll(r.Records[i].Payload, []byte("call_b"), []byte("call_a"))
					r.Records[i].Payload = bytes.ReplaceAll(r.Records[i].Payload, []byte("方块"), []byte("蓝色"))
				}
			}
			if _, err := dsCandidate(r); err == nil {
				t.Fatal("自洽ack掩盖了不属于工具集合或重复的结果提交")
			}
		})
	}
}

func dsToolSetTrace(output string) dsRecording {
	r := dsSyntheticRecording()
	r.Scenario = "text-tools"
	r.Records = r.Records[:3]
	for _, step := range []struct{ direction, raw string }{
		{"receive", dsToolSetDone(output)},
		{"send", `{"type":"conversation.item.create","item":{"type":"function_call_output","call_id":"call_a","output":"蓝色"}}`},
		{"receive", `{"type":"conversation.item.created","item":{"id":"result_a","type":"function_call_output","call_id":"call_a","output":"蓝色"}}`},
		{"send", `{"type":"conversation.item.create","item":{"type":"function_call_output","call_id":"call_b","output":"方块"}}`},
		{"receive", `{"type":"conversation.item.created","item":{"id":"result_b","type":"function_call_output","call_id":"call_b","output":"方块"}}`},
		{"send", `{"type":"response.create"}`},
		{"receive", `{"type":"response.done","response":{"id":"r3","status":"completed"}}`},
	} {
		r.Records = append(r.Records, dsRecord{Direction: step.direction, Kind: "message", Opcode: ws.OpText, Payload: []byte(step.raw)})
	}
	r.Records = append(r.Records, dsRecord{Direction: "receive", Kind: "close", CloseCode: 1000})
	return r
}
