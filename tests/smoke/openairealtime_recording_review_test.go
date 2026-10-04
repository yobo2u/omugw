package smoke_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// 保持完整成功轨迹及样本字节/摘要，只篡改声明，防止候选给错误格式贴上确定标签。
func TestOpenAIRealtimeRecorderOfflineCandidateSampleMetadata(t *testing.T) {
	for _, scenario := range []string{"audio-manual", "vad-interrupt"} {
		t.Run(scenario, func(t *testing.T) {
			good, _ := oaRunPeer(t, scenario, func(s string) string { return s }, false)
			if good.Failure != "" || good.Input == nil {
				t.Fatalf("没有完整成功录制: %s", good.Failure)
			}
			if _, err := oaCandidate(good); err != nil {
				t.Fatalf("有效元数据不能派生候选: %v", err)
			}
			for _, tc := range []struct {
				name string
				edit func(*oaSample)
			}{
				{"public", func(s *oaSample) { s.Public = false }},
				{"origin_empty", func(s *oaSample) { s.Origin = "" }},
				{"origin_scheme", func(s *oaSample) { s.Origin = "http://example.org/audio" }},
				{"origin_host", func(s *oaSample) { s.Origin = "https:///audio" }},
				{"origin_user", func(s *oaSample) { s.Origin = "https://user@example.org/audio" }},
				{"origin_query", func(s *oaSample) { s.Origin += "?token=value" }},
				{"origin_fragment", func(s *oaSample) { s.Origin += "#fragment" }},
				{"origin_invalid", func(s *oaSample) { s.Origin = "://%" }},
				{"license", func(s *oaSample) { s.License = "" }},
				{"format", func(s *oaSample) { s.Format = "wav" }},
				{"rate", func(s *oaSample) { s.Rate = 16000 }},
				{"channels", func(s *oaSample) { s.Channels = 2 }},
				{"transcript_empty", func(s *oaSample) { s.Transcript = "" }},
				{"transcript_large", func(s *oaSample) { s.Transcript = strings.Repeat("a", 1025) }},
			} {
				t.Run(tc.name, func(t *testing.T) {
					copy := good
					sample := *good.Input
					sample.Data = bytes.Clone(good.Input.Data)
					copy.Input = &sample
					tc.edit(copy.Input)
					if !bytes.Equal(copy.Input.Data, good.Input.Data) || copy.Input.SHA256 != good.Input.SHA256 {
						t.Fatal("元数据反例改变了样本或摘要")
					}
					if _, err := oaCandidate(copy); err == nil {
						t.Fatal("候选未重新核验样本元数据")
					}
				})
			}
			if _, err := oaCandidate(good); err != nil {
				t.Fatalf("篡改副本污染有效录制: %v", err)
			}
		})
	}
}

// 每个反例都保留最后的合法 response.done，防止终态覆盖中途错关联。
func TestOpenAIRealtimeRecorderOfflineToolArgumentEvidence(t *testing.T) {
	for _, tc := range []struct{ name, event, old, replacement string }{
		{"delta_call", "args_delta_1a", `"call_id":"call1"`, `"call_id":"other"`},
		{"delta_missing_call", "args_delta_1a", `"call_id":"call1",`, ``},
		{"delta_missing_payload", "args_delta_1a", `,"delta":"{"`, ``},
		{"delta_bad_payload", "args_delta_1a", `"delta":"{"`, `"delta":null`},
		{"delta_optional_name", "args_delta_1a", `"call_id":"call1"`, `"call_id":"call1","name":"unknown"`},
		{"delta_missing_event_id", "args_delta_1a", `"event_id":"args_delta_1a",`, ``},
		{"delta_missing_response", "args_delta_1a", `"response_id":"r3",`, ``},
		{"delta_missing_item", "args_delta_1a", `"item_id":"fc1",`, ``},
		{"delta_missing_index", "args_delta_1a", `"output_index":0,`, ``},
		{"done_call", "args_done_1", `"call_id":"call1"`, `"call_id":"other"`},
		{"done_missing_call", "args_done_1", `"call_id":"call1",`, ``},
		{"done_missing_event_id", "args_done_1", `"event_id":"args_done_1",`, ``},
		{"done_missing_response", "args_done_1", `"response_id":"r3",`, ``},
		{"done_missing_item", "args_done_1", `"item_id":"fc1",`, ``},
		{"done_missing_index", "args_done_1", `"output_index":0,`, ``},
		{"done_name", "args_done_1", `"name":"test_color"`, `"name":"unknown"`},
		{"done_missing_name", "args_done_1", `"name":"test_color",`, ``},
		{"done_missing_arguments", "args_done_1", `,"arguments":"{}"`, ``},
		{"done_bad_arguments", "args_done_1", `"arguments":"{}"`, `"arguments":null`},
		{"accumulation_mismatch", "args_delta_1b", `"delta":"}"`, `"delta":" }"`},
		{"done_arguments_mismatch", "args_done_1", `"arguments":"{}"`, `"arguments":"{ }"`},
		{"item_arguments_mismatch", "tool_done_1", `"arguments":"{}"`, `"arguments":"{ }"`},
		{"item_missing_arguments", "tool_done_1", `,"arguments":"{}"`, ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			matched := false
			r, sent := oaRunPeer(t, "text-tools-vision", func(raw string) string {
				if strings.Contains(raw, `"event_id":"`+tc.event+`"`) {
					matched = true
					return strings.Replace(raw, tc.old, tc.replacement, 1)
				}
				return raw
			}, false)
			if !matched {
				t.Fatal("反例没有抵达目标参数事件")
			}
			results := 0
			for _, e := range sent {
				if oaString(e, "item", "type") == "function_call_output" {
					results++
				}
			}
			if results != 0 {
				t.Errorf("参数证据失败前应零工具结果发送，实际=%d", results)
			}
			if r.Failure == "" {
				t.Error("中途参数错关联被最终合法终态掩盖")
			}
			if _, err := oaCandidate(r); err == nil {
				t.Error("错关联生成候选")
			}
		})
	}
}

func TestOpenAIRealtimeRecorderOfflineToolCandidateRevalidatesArguments(t *testing.T) {
	r, _ := oaRunPeer(t, "text-tools-vision", func(s string) string { return s }, false)
	if r.Failure != "" {
		t.Fatal(r.Failure)
	}
	for _, event := range []string{"args_delta_1a", "args_done_1", "tool_done_1"} {
		t.Run(event, func(t *testing.T) {
			copy := r
			copy.Records = append([]oaRecord(nil), r.Records...)
			matched := false
			for i, rec := range copy.Records {
				if bytes.Contains(rec.Payload, []byte(`"event_id":"`+event+`"`)) {
					copy.Records[i].Payload = bytes.Replace(rec.Payload, []byte(`"call_id":"call1"`), []byte(`"call_id":"other"`), 1)
					matched = true
				}
			}
			if !matched {
				t.Fatal("未篡改完整成功轨迹")
			}
			if _, err := oaCandidate(copy); err == nil {
				t.Fatal("候选未重验参数事件")
			}
		})
	}
}

func TestOpenAIRealtimeRecorderOfflineAudioItemRoles(t *testing.T) {
	for _, scenario := range []string{"audio-manual", "vad-interrupt"} {
		t.Run(scenario, func(t *testing.T) {
			// 在真实上游脚本中复现用户 item 和 assistant item 共用 ID。
			r, _ := oaRunPeer(t, scenario, func(s string) string { return strings.ReplaceAll(s, "audio1", "input1") }, false)
			if r.Failure == "" {
				t.Error("录制允许 user/assistant 共用 item ID")
			}
			// 完整成功轨迹中连同发送的 truncate 一并替换，排除仅请求字节不符的伪绿。
			good, _ := oaRunPeer(t, scenario, func(s string) string { return s }, false)
			if good.Failure != "" {
				t.Fatal(good.Failure)
			}
			for _, withEcho := range []bool{false, true} {
				copy := good
				copy.Records = nil
				for _, rec := range good.Records {
					if !withEcho && bytes.Contains(rec.Payload, []byte(`"item":{"id":"input1"`)) {
						continue
					}
					rec.Payload = bytes.ReplaceAll(rec.Payload, []byte("audio1"), []byte("input1"))
					copy.Records = append(copy.Records, rec)
				}
				if _, err := oaCandidate(copy); err == nil {
					t.Errorf("篡改完整轨迹绕过 item 角色核验，input回显=%v", withEcho)
				}
			}
		})
	}
}

func TestOpenAIRealtimeRecorderOfflineToolRequiresBothTerminals(t *testing.T) {
	r, _ := oaRunPeer(t, "text-tools-vision", func(s string) string { return s }, false)
	if r.Failure != "" {
		t.Fatal(r.Failure)
	}
	for _, event := range []string{"args_done_1", "tool_done_1"} {
		t.Run(event, func(t *testing.T) {
			copy := r
			copy.Records = nil
			for _, rec := range r.Records {
				if !bytes.Contains(rec.Payload, []byte(`"event_id":"`+event+`"`)) {
					copy.Records = append(copy.Records, rec)
				}
			}
			if len(copy.Records) != len(r.Records)-1 {
				t.Fatal("缺失事件反例不准确")
			}
			if _, err := oaCandidate(copy); err == nil {
				t.Fatal("最终response.done替代了缺失的参数/item终态")
			}
		})
	}
}

func TestOpenAIRealtimeRecorderOfflineToolLateMalformedItemTerminal(t *testing.T) {
	r, _ := oaRunPeer(t, "text-tools-vision", func(s string) string { return s }, false)
	if r.Failure != "" {
		t.Fatal(r.Failure)
	}
	for _, field := range []string{"response_id", "event_id", "output_index", "item"} {
		t.Run(field, func(t *testing.T) {
			copy := r
			copy.Records = nil
			inserted := false
			for _, rec := range r.Records {
				copy.Records = append(copy.Records, rec)
				if bytes.Contains(rec.Payload, []byte(`"event_id":"tool_done_1"`)) {
					var e map[string]any
					_ = json.Unmarshal(rec.Payload, &e)
					delete(e, field)
					bad := rec
					bad.Payload, _ = json.Marshal(e)
					copy.Records = append(copy.Records, bad)
					inserted = true
				}
			}
			if !inserted {
				t.Fatal("未注入迟到损坏终态")
			}
			if _, err := oaCandidate(copy); err == nil {
				t.Fatal("已见合法 item.done 使后续损坏终态被忽略")
			}
		})
	}
}

func TestOpenAIRealtimeRecorderOfflineToolOptionalDeltaName(t *testing.T) {
	r, _ := oaRunPeer(t, "text-tools-vision", func(raw string) string {
		if strings.Contains(raw, `"event_id":"args_delta_1a"`) {
			return strings.Replace(raw, `"call_id":"call1"`, `"call_id":"call1","name":"test_color"`, 1)
		}
		return raw
	}, false)
	if r.Failure != "" {
		t.Fatal(r.Failure)
	}
	f, err := oaCandidate(r)
	if err != nil {
		t.Fatal(err)
	}
	oaReplay(t, f)
}

func TestOpenAIRealtimeRecorderOfflineAudioItemLifecycleRoleConflict(t *testing.T) {
	for _, scenario := range []string{"audio-manual", "vad-interrupt"} {
		for _, id := range []string{"input1", "audio1"} {
			t.Run(scenario+"/"+id, func(t *testing.T) {
				r, _ := oaRunPeer(t, scenario, func(raw string) string {
					if strings.Contains(raw, `"type":"conversation.item.done","item":{"id":"`+id+`"`) {
						from, to := "user", "assistant"
						if id == "audio1" {
							from, to = to, from
						}
						return strings.Replace(raw, `"role":"`+from+`"`, `"role":"`+to+`"`, 1)
					}
					return raw
				}, false)
				if r.Failure == "" {
					t.Fatal("同 item 的生命周期回显更换角色仍接受")
				}
			})
		}
	}
}

func TestOpenAIRealtimeRecorderOfflineToolTerminalArgumentMismatch(t *testing.T) {
	// 累积、参数 done 和 item done 都一致，但 response 终态恢复为另一份合法 JSON。
	r, sent := oaRunPeer(t, "text-tools-vision", func(raw string) string {
		var e map[string]any
		_ = json.Unmarshal([]byte(raw), &e)
		switch oaString(e, "event_id") {
		case "args_delta_1b":
			return strings.Replace(raw, `"delta":"}"`, `"delta":" }"`, 1)
		case "args_done_1", "tool_done_1":
			return strings.Replace(raw, `"arguments":"{}"`, `"arguments":"{ }"`, 1)
		}
		return raw
	}, false)
	for _, e := range sent {
		if oaString(e, "item", "type") == "function_call_output" {
			t.Error("参数终态冲突后仍发送工具结果")
		}
	}
	if r.Failure == "" {
		t.Fatal("response 终态未核对已确认参数")
	}
}
