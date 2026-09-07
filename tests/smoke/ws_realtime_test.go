//go:build smoke

package smoke_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/transport/ws"
)

// realtimeWSURL 返回 DashScope Realtime 的 WebSocket 端点。
func realtimeWSURL(t testing.TB, model string) string {
	t.Helper()

	base := strings.TrimSpace(os.Getenv("OMUGW_SMOKE_WS_URL"))
	if base == "" {
		base = "wss://dashscope.aliyuncs.com/api-ws/v1/realtime"
	}
	return base + "?model=" + model
}

// realtimeModel 取实时模型名。
//
// 默认选 qwen3.5 系列：只有它支持新式 audio.input.format 字段，而这条
// smoke 要验证的正是「网关能声明 24 kHz 从而免去重采样」这个前提。
func realtimeModel(t testing.TB) string {
	t.Helper()

	if v := strings.TrimSpace(os.Getenv("OMUGW_SMOKE_MODEL_REALTIME")); v != "" {
		return v
	}
	return "qwen3.5-omni-flash-realtime"
}

// TestWSTransportInteropsWithRealUpstream 是传输层唯一无法离线证明的一条。
//
// 离线测试里客户端与服务端都是自己写的，两边一起算错也会互相认可。这条
// 用真实 DashScope 端点作对手，证明握手摘要、掩码方向与帧格式确实与第三方
// 实现互通——RFC 里最容易写错的三处，恰好都是「自测通过、联调失败」型的错误。
//
// 只做 session 协商，不发音频，因此不产生 token 计费。
func TestWSTransportInteropsWithRealUpstream(t *testing.T) {
	key := smokeAPIKey(t)
	model := realtimeModel(t)

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+key)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, resp, err := ws.Dial(ctx, realtimeWSURL(t, model), ws.DialOptions{
		Header:     hdr,
		MaxPayload: 8 << 20,
		Idle:       20 * time.Second,
	})
	if err != nil {
		t.Fatalf("与真实上游握手失败: %v", err)
	}
	defer conn.Close(ws.CloseNormal, "")

	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("状态码 = %d，期望 101", resp.StatusCode)
	}

	// session.created 是服务端主动推的第一条事件。收到它就说明帧层双向可用：
	// 我们的握手被接受，且我们能正确解出对方的帧。
	op, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("读取首条事件失败: %v", err)
	}
	if op != ws.OpText {
		t.Fatalf("首条事件 opcode = %v，期望文本帧", op)
	}

	var created struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &created); err != nil {
		t.Fatalf("首条事件不是合法 JSON: %v", err)
	}
	if created.Type != "session.created" {
		t.Fatalf("首条事件 type = %q，期望 session.created", created.Type)
	}
}

// TestRealtimeNewFormatFieldsAvoidResampling 钉住本次设计的事实前提。
//
// 探针发现：中间代模型收到新式字段后**照样回 session.updated**，但回显的是
// legacy 的 input_audio_format。也就是说「服务端没报错」根本不能证明它按
// 24 kHz 收音——不查回显就透传，等于把 24 kHz 音频喂给按 16 kHz 解析的上游，
// 听感是变速乱码，而且不报任何错。
//
// 这条测试把那个判据固化下来：回显里必须出现 audio.input.format.sample_rate，
// 且等于我们声明的值。一旦上游改变行为，这里会红，而不是等到音频出问题。
func TestRealtimeNewFormatFieldsAvoidResampling(t *testing.T) {
	key := smokeAPIKey(t)
	model := realtimeModel(t)

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+key)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, _, err := ws.Dial(ctx, realtimeWSURL(t, model), ws.DialOptions{
		Header:     hdr,
		MaxPayload: 8 << 20,
		Idle:       20 * time.Second,
	})
	if err != nil {
		t.Fatalf("与真实上游握手失败: %v", err)
	}
	defer conn.Close(ws.CloseNormal, "")

	update, err := json.Marshal(map[string]any{
		"event_id": "smoke_probe",
		"type":     "session.update",
		"session": map[string]any{
			"modalities": []string{"text", "audio"},
			"audio": map[string]any{
				"input":  map[string]any{"format": map[string]any{"type": "pcm", "sample_rate": 24000}},
				"output": map[string]any{"format": map[string]any{"type": "pcm", "sample_rate": 24000}},
			},
		},
	})
	if err != nil {
		t.Fatalf("构造 session.update 失败: %v", err)
	}
	if err := conn.WriteMessage(ws.OpText, update); err != nil {
		t.Fatalf("发送 session.update 失败: %v", err)
	}

	var echoed struct {
		Type    string `json:"type"`
		Session struct {
			Audio struct {
				Input struct {
					Format struct {
						Type       string `json:"type"`
						SampleRate int    `json:"sample_rate"`
					} `json:"format"`
				} `json:"input"`
			} `json:"audio"`
		} `json:"session"`
		Error json.RawMessage `json:"error"`
	}

	// session.created 先到，session.updated 随后；中间可能还有别的事件。
	for i := 0; i < 8; i++ {
		op, payload, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("读取事件失败: %v", err)
		}
		if op != ws.OpText {
			continue
		}
		if err := json.Unmarshal(payload, &echoed); err != nil {
			t.Fatalf("事件不是合法 JSON: %v", err)
		}

		switch echoed.Type {
		case "session.updated":
			got := echoed.Session.Audio.Input.Format.SampleRate
			if got != 24000 {
				t.Fatalf("回显的 input sample_rate = %d，期望 24000。"+
					"该模型静默降级为 legacy 语义，此路径必须重采样", got)
			}
			return
		case "error":
			t.Fatalf("上游拒绝了新式音频格式字段: %s", echoed.Error)
		}
	}
	t.Fatal("八条事件内未收到 session.updated")
}
