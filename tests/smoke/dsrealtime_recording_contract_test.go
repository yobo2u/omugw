//go:build smoke

package smoke_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/transport/ws"
)

// 两次失败实录的 base64 已独立解码；字面副本让离线回归不依赖私有目录，且不改历史轨迹。
const dsObservedTTSCreated = `{"event_id":"event_PMzeieect6etdwbMqx8lt","type":"session.created","session":{"object":"realtime.session","mode":"server_commit","model":"qwen3-tts-flash-realtime","voice":"Cherry","response_format":"pcm","sample_rate":24000,"id":"sess_TU2mrgIW8oIMfIjV8tSKT"}}`
const dsObservedTTSUpdated = `{"event_id":"event_HrIkC4JUVRyXUyCMuPeAJ","type":"session.updated","session":{"id":"sess_TU2mrgIW8oIMfIjV8tSKT","object":"realtime.session","model":"qwen3-tts-flash-realtime","voice":"Cherry","mode":"commit","response_format":"pcm","sample_rate":16000,"language_type":"chinese"}}`
const dsObservedTextCreated = `{"event_id":"event_DcTmhRfnwTDlMLmSzkT4p","type":"session.created","session":{"object":"realtime.session","model":"qwen3.5-omni-flash-realtime","modalities":["text","audio"],"voice":"Tina","input_audio_format":"pcm","output_audio_format":"pcm","input_audio_transcription":{"model":"qwen3-asr-flash-realtime"},"turn_detection":{"type":"server_vad","threshold":0.5,"prefix_padding_ms":300,"silence_duration_ms":800,"create_response":true,"interrupt_response":true},"id":"sess_YalnlqhHpMF9dI76AgvfB"}}`
const dsObservedTextUpdated = `{"event_id":"event_B2xlyydgryCSaKyN4YWe4","type":"session.updated","session":{"id":"sess_YalnlqhHpMF9dI76AgvfB","object":"realtime.session","model":"qwen3.5-omni-flash-realtime","modalities":["text"],"instructions":"这是公开的无隐私测试。简短作答，不超过二十个字。","voice":"Tina","input_audio_format":"pcm","output_audio_format":"pcm","input_audio_transcription":{"model":"qwen3-asr-flash-realtime"},"tools":[{"function":{"name":"test_color","description":"返回测试图形颜色；与 test_shape 同轮调用。","parameters":{"properties":{},"type":"object"}},"type":"function"},{"function":{"name":"test_shape","description":"返回测试图形形状；与 test_color 同轮调用。","parameters":{"properties":{},"type":"object"}},"type":"function"}],"max_tokens":128}}`

func TestDSRealtimeRecorderOfflineObservedConfigContract(t *testing.T) {
	ttsExact := strings.Replace(dsObservedTTSUpdated, `"chinese"`, `"Chinese"`, 1)
	// 音频形状是手写负例，不把文本实录的缺字段推断成音频 manual 已生效。
	audio := `{"type":"session.updated","session":{"model":"qwen3.5-omni-flash-realtime","modalities":["text"],"instructions":"这是公开的无隐私测试。简短作答，不超过二十个字。","max_tokens":128,"turn_detection":null,"audio":{"input":{"format":{"type":"pcm","sample_rate":16000}},"output":{"format":{"type":"pcm","sample_rate":16000}}}}}`
	for _, tc := range []struct {
		name, scenario, created, updated string
		confirmed                        bool
	}{
		{"tts_observed_lowercase", "tts-commit", dsObservedTTSCreated, dsObservedTTSUpdated, true},
		{"tts_exact", "tts-commit", dsObservedTTSCreated, ttsExact, true},
		{"tts_other_language_case", "tts-commit", dsObservedTTSCreated, strings.Replace(ttsExact, `"Chinese"`, `"CHINESE"`, 1), false},
		{"tts_missing_language", "tts-commit", dsObservedTTSCreated, strings.Replace(ttsExact, `,"language_type":"Chinese"`, "", 1), false},
		{"tts_voice_case", "tts-commit", dsObservedTTSCreated, strings.Replace(ttsExact, `"Cherry"`, `"cherry"`, 1), false},
		{"tts_format_case", "tts-commit", dsObservedTTSCreated, strings.Replace(ttsExact, `"pcm"`, `"PCM"`, 1), false},
		{"tts_mode_case", "tts-commit", dsObservedTTSCreated, strings.Replace(ttsExact, `"commit"`, `"COMMIT"`, 1), false},
		{"tts_missing_rate", "tts-commit", dsObservedTTSCreated, strings.Replace(ttsExact, `,"sample_rate":16000`, "", 1), false},
		{"tts_wrong_rate", "tts-commit", dsObservedTTSCreated, strings.Replace(ttsExact, `16000`, `24000`, 1), false},
		{"created_missing_model", "tts-commit", strings.Replace(dsObservedTTSCreated, `,"model":"qwen3-tts-flash-realtime"`, "", 1), ttsExact, false},
		{"created_wrong_model", "tts-commit", strings.Replace(dsObservedTTSCreated, `qwen3-tts-flash-realtime`, `other-model`, 1), ttsExact, false},
		{"updated_missing_model", "tts-commit", dsObservedTTSCreated, strings.Replace(ttsExact, `,"model":"qwen3-tts-flash-realtime"`, "", 1), false},
		{"updated_model_case", "tts-commit", dsObservedTTSCreated, strings.Replace(ttsExact, `qwen3-tts-flash-realtime`, `Qwen3-tts-flash-realtime`, 1), false},
		{"text_observed_omission", "text-tools", dsObservedTextCreated, dsObservedTextUpdated, true},
		{"text_server_vad_unchanged", "text-tools", dsObservedTextCreated, strings.Replace(dsObservedTextUpdated, `"max_tokens":128`, `"max_tokens":128,"turn_detection":{"type":"server_vad"}`, 1), true},
		{"text_missing_limit", "text-tools", dsObservedTextCreated, strings.Replace(dsObservedTextUpdated, `,"max_tokens":128`, "", 1), false},
		{"text_wrong_limit", "text-tools", dsObservedTextCreated, strings.Replace(dsObservedTextUpdated, `"max_tokens":128`, `"max_tokens":129`, 1), false},
		{"text_modalities_case", "text-tools", dsObservedTextCreated, strings.Replace(dsObservedTextUpdated, `"text"`, `"Text"`, 1), false},
		{"text_tool_name_case", "text-tools", dsObservedTextCreated, strings.Replace(dsObservedTextUpdated, `"test_color"`, `"TEST_COLOR"`, 1), false},
		{"audio_explicit_null", "audio-image", dsObservedTextCreated, audio, true},
		{"audio_missing_turn_detection", "audio-image", dsObservedTextCreated, strings.Replace(audio, `,"turn_detection":null`, "", 1), false},
		{"audio_server_vad", "audio-image", dsObservedTextCreated, strings.Replace(audio, `"turn_detection":null`, `"turn_detection":{"type":"server_vad"}`, 1), false},
		{"audio_missing_input_rate", "audio-image", dsObservedTextCreated, strings.Replace(audio, `,"sample_rate":16000`, "", 1), false},
		{"audio_wrong_output_rate", "audio-image", dsObservedTextCreated, strings.Replace(audio, `"output":{"format":{"type":"pcm","sample_rate":16000}}`, `"output":{"format":{"type":"pcm","sample_rate":24000}}`, 1), false},
		{"audio_format_case", "audio-image", dsObservedTextCreated, strings.Replace(audio, `"pcm"`, `"PCM"`, 1), false},
		{"audio_legacy_only", "audio-image", dsObservedTextCreated, strings.Replace(audio, `"audio":{"input":{"format":{"type":"pcm","sample_rate":16000}},"output":{"format":{"type":"pcm","sample_rate":16000}}}`, `"input_audio_format":"pcm","output_audio_format":"pcm"`, 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var request []byte
			inputs := make(chan int, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				count := 0
				defer func() { inputs <- count }()
				c, err := ws.Accept(w, req, ws.AcceptOptions{MaxPayload: 1 << 20, Idle: time.Second, WriteTimeout: time.Second})
				if err != nil {
					t.Error(err)
					return
				}
				defer c.Close(1000, "")
				p := dsOfflinePeer{t: t, c: c}
				p.write(tc.created)
				for {
					op, b, err := c.ReadMessage()
					if err != nil {
						return
					}
					var e map[string]any
					if op != ws.OpText || json.Unmarshal(b, &e) != nil {
						t.Error("本地配置脚本收到非 JSON 消息")
						return
					}
					if dsString(e, "type") == "session.update" {
						request = bytes.Clone(b)
						p.write(tc.updated)
					} else {
						count++
						// 在第一条业务输入后立即停止，只验证配置门禁，不生成合成能力结论。
						if count == 1 {
							p.write(`{"type":"error","error":{"code":"offline_stop"}}`)
						}
					}
				}
			}))
			defer server.Close()
			cfg := dsConfig{URL: "ws" + strings.TrimPrefix(server.URL, "http"), Model: "qwen3-tts-flash-realtime", Scenario: tc.scenario, Key: "offline-secret", Duration: 2 * time.Second, Responses: 1}
			if tc.scenario == "text-tools" || tc.scenario == "audio-image" {
				cfg.Model = "qwen3.5-omni-flash-realtime"
			}
			if tc.scenario == "audio-image" {
				cfg.Sample = &dsAudioSample{Data: []byte{0, 1, 2, 3}, Rate: 16000, Format: "pcm_s16le"}
			}
			r := dsCapture(context.Background(), cfg)
			count := <-inputs
			if r.Confirmed != tc.confirmed || (count > 0) != tc.confirmed {
				t.Errorf("配置门禁: confirmed=%v inputs=%d failure=%s; want confirmed=%v", r.Confirmed, count, r.Failure, tc.confirmed)
			}
			wantFailure := "unconfirmed_config"
			if tc.confirmed {
				wantFailure = "upstream_error"
			}
			if r.Failure != wantFailure {
				t.Errorf("错误分类=%s; want %s", r.Failure, wantFailure)
			}
			if tc.scenario == "text-tools" && len(request) > 0 {
				var e map[string]any
				_ = json.Unmarshal(request, &e)
				if _, present := dsMap(e, "session")["turn_detection"]; present {
					t.Error("文本请求仍携带不需要的音频 turn_detection")
				}
			}
			// 核验落盘编码往返后的原字节，不能把比较规则写回请求/回显。
			b, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			var saved dsRecording
			if err := json.Unmarshal(b, &saved); err != nil {
				t.Fatal(err)
			}
			seenUpdated := false
			for _, rec := range saved.Records {
				if rec.Kind != "message" {
					continue
				}
				var e map[string]any
				_ = json.Unmarshal(rec.Payload, &e)
				switch dsString(e, "type") {
				case "session.created":
					if !bytes.Equal(rec.Payload, []byte(tc.created)) {
						t.Error("首事件原字节改变")
					}
				case "session.updated":
					seenUpdated = true
					if !bytes.Equal(rec.Payload, []byte(tc.updated)) {
						t.Error("回显原字节改变")
					}
				case "session.update":
					if !bytes.Equal(rec.Payload, request) {
						t.Error("请求原字节改变")
					}
					if tc.scenario == "tts-commit" && dsString(e, "session", "language_type") != "Chinese" {
						t.Error("为迁就回显改写了请求语言")
					}
				}
			}
			if len(request) > 0 && !seenUpdated {
				t.Error("配置回显未保存")
			}
			if _, err := dsCandidate(r); err == nil {
				t.Error("未完成的业务轨迹被转成成功候选")
			}
		})
	}
}

func TestDSRealtimeRecorderOfflinePresenceAndNonSessionRemainStrict(t *testing.T) {
	if dsEchoMatches(map[string]any{"turn_detection": nil}, map[string]any{}) {
		t.Fatal("null 被全局等同于 missing")
	}
	if dsEchoMatches(map[string]any{"language_type": "Chinese"}, map[string]any{"language_type": "chinese"}) {
		t.Fatal("通用字段比较被改成不区分大小写")
	}
	for _, scenario := range []string{"text-tools", "audio-image", "vad-interrupt"} {
		if dsSessionEchoMatches(scenario, map[string]any{"language_type": "Chinese"}, map[string]any{"language_type": "chinese"}) {
			t.Fatal("TTS 语言特例扩散到 Omni")
		}
	}
	if dsSessionEchoMatches("tts-commit", map[string]any{"nested": map[string]any{"language_type": "Chinese"}}, map[string]any{"nested": map[string]any{"language_type": "chinese"}}) {
		t.Fatal("TTS 顶层语言特例扩散到嵌套字段")
	}
	r := dsSyntheticRecording()
	r.Records[1].Payload = []byte(`{"type":"conversation.item.create","item":{"language_type":"Chinese"}}`)
	r.Records[2].Payload = []byte(`{"type":"conversation.item.created","item":{"language_type":"chinese"}}`)
	if _, err := dsCandidate(r); err == nil {
		t.Fatal("TTS 会话特例泄漏到 item 后继见证")
	}
}
