//go:build smoke

package smoke_test

import (
	"encoding/json"
	"net/http"
	"testing"

	nativewire "github.com/yobo2u/omugw/internal/protocol/dashscopenative"
	"github.com/yobo2u/omugw/internal/testkit"
)

// TestChatDSNativeSmoke 真实打 DashScope，证明已兑现的八项能力在真实模型上可用。
//
// 凭据由 smokeAPIKey 的双重 opt-in 把关：缺 OMUGW_SMOKE=1 或 DASHSCOPE_API_KEY
// 一律在发出任何请求之前跳过。防的是离线 CI 与本地开发机在带 -tags=smoke 时
// 意外向真实云端发请求产生账单与网络副作用。
//
// 没有音频探针：audio_input 本期未兑现，其 live 负例是录制会话的
// TestRecordChatDSNativeAudioInputStays501；本测试不得调用任何额度耗尽的音频模型。
func TestChatDSNativeSmoke(t *testing.T) {
	apiKey := smokeAPIKey(t)

	// 组合放第一个：若单一真实模型扛不住组合，尽早发现，
	// 不要等其余探针付完钱才知道。
	//
	// t.Run 的返回值必须收下：子测试里的 Fatal 只终止子测试自己的 goroutine，
	// 丢掉返回值时父测试会若无其事地继续往下跑——「早失败省钱」的意图落空，
	// 后面三个探针照样各打一次真实付费调用。
	if !t.Run("combined", func(t *testing.T) {
		var combined caseMeta
		var found bool
		for _, c := range recordCases {
			if c.name == "combined" {
				combined, found = c, true
				break
			}
		}
		if !found {
			t.Fatal("recordCases 缺少 combined 用例，降级期望无从对账")
		}

		model := modelForRole(modelRoleCombined)
		built := buildSmokeGateway(t, nativewire.DoorMultimodalGeneration, model, apiKey)
		rec := doSmokeChat(t, built, bodyCombined(model))
		if rec.Code != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200: %s", rec.Code, rec.Body.String())
		}
		// 复用录制侧那份逐项对账，而不是自己 strings.Contains：子串检查只问
		// 「这两项在不在」，头里多出第三项（一次把本该 PASS 的能力误判成
		// DEGRADE 的改动）照样全绿。assertDegradationHeader 走的是集合相等。
		assertDegradationHeader(t, combined, rec)

		var resp downstreamCompletion
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("下游响应不是合法 JSON: %v (%s)", err, rec.Body.String())
		}
		// usage 字段的存在本身就是「用量权威、可计费」的判据：编码器在估算或
		// 不可用时整体省略它，绝不发一份失真的数字。存在之外还要数值自洽——
		// 一份 total 对不上 prompt+completion 的用量同样不可计费。
		if resp.Usage == nil {
			t.Fatalf("下游缺少 usage，本次调用的用量不可计费: %s", rec.Body.String())
		}
		assertAuthoritativeUsage(t, *resp.Usage)
	}) {
		t.Fatal("组合探针失败，停止其余真实探针——继续跑只会在同一个已知故障上" +
			"再产生三次付费调用")
	}

	t.Run("basic", func(t *testing.T) {
		model := modelForRole(modelRoleText)
		built := buildSmokeGateway(t, nativewire.DoorTextGeneration, model, apiKey)
		rec := doSmokeChat(t, built, bodyBasic(model))
		if rec.Code != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200: %s", rec.Code, rec.Body.String())
		}
		var resp downstreamCompletion
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("下游响应不是合法 JSON: %v (%s)", err, rec.Body.String())
		}
		if resp.Object != "chat.completion" {
			t.Errorf("下游 object = %q，期望 chat.completion", resp.Object)
		}
		// 只断言 choices 非空会放过一份「结构对了但一个字都没有」的回复——
		// 那种响应在客户端眼里与转换失败没有区别。
		if len(resp.Choices) == 0 || resp.Choices[0].Message.Content == "" {
			t.Errorf("下游 choices 为空或内容为空: %s", rec.Body.String())
		}
		if resp.Usage == nil {
			t.Fatalf("下游缺少 usage，本次调用的用量不可计费: %s", rec.Body.String())
		}
		assertAuthoritativeUsage(t, *resp.Usage)
	})

	t.Run("streaming", func(t *testing.T) {
		model := modelForRole(modelRoleText)
		built := buildSmokeGateway(t, nativewire.DoorTextGeneration, model, apiKey)
		rec := doSmokeChat(t, built, bodyStreaming(model))
		if rec.Code != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200: %s", rec.Code, rec.Body.String())
		}
		events, err := testkit.ParseSSE(rec.Body)
		if err != nil {
			t.Fatalf("下游 SSE 无法解析: %v", err)
		}
		var sawDone, sawUsageChunk bool
		for _, ev := range events {
			// 流式的 200 在首帧之前就写出去了：一条转换半途失败的流照样顶着 200
			// 收场，错误只以流内 error 事件的形式出现。
			if ev.Event == "error" {
				t.Fatalf("下游流内出现 error 事件: %s", ev.Data)
			}
			if ev.IsDone() {
				sawDone = true
				continue
			}
			var chunk downstreamChunk
			if err := json.Unmarshal([]byte(ev.Data), &chunk); err != nil {
				t.Fatalf("chunk 不是合法 JSON: %v (%s)", err, ev.Data)
			}
			// 空 choices 且带 usage 就是末尾账单帧。
			if len(chunk.Choices) == 0 && chunk.Usage != nil {
				sawUsageChunk = true
			}
		}
		// [DONE] 由 translator 在全部候选 finish 后合成，缺了它 OpenAI 客户端
		// 会一直等一个永远不会到来的哨兵。
		if !sawDone {
			t.Fatal("下游流缺少 [DONE] 哨兵")
		}
		if !sawUsageChunk {
			t.Fatal("客户端要了 include_usage，下游流却没有末尾 usage chunk")
		}
	})

	// 推理用专属的 reasoning 角色模型：普通文本模型不产 reasoning_content，
	// 拿它来跑等于把「深度思考落没落到客户端」换成一个必然为否的问题。
	t.Run("reasoning", func(t *testing.T) {
		model := modelForRole(modelRoleReasoning)
		built := buildSmokeGateway(t, nativewire.DoorTextGeneration, model, apiKey)
		rec := doSmokeChat(t, built, bodyReasoning(model))
		if rec.Code != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200: %s", rec.Code, rec.Body.String())
		}
		events, err := testkit.ParseSSE(rec.Body)
		if err != nil {
			t.Fatalf("下游 SSE 无法解析: %v", err)
		}
		var reasoningFrames int
		for _, ev := range events {
			// error 事件必须致命而不是跳过：流式的 200 在首帧之前就写出去了，
			// 一条思考帧发完才中断的流会攒够 reasoningFrames 再以 error 收场，
			// 跳过它等于把一次半截的转换判成通过。
			if ev.Event == "error" {
				t.Fatalf("下游流内出现 error 事件，转换未走完: %s", ev.Data)
			}
			if ev.IsDone() {
				continue
			}
			var chunk downstreamChunk
			// 解不开的帧只是不计数，不能让它冒充证据：一条整流皆乱码的响应
			// 会让 reasoningFrames 停在 0，本用例照样失败。
			if err := json.Unmarshal([]byte(ev.Data), &chunk); err != nil {
				continue
			}
			for _, ch := range chunk.Choices {
				if ch.Delta.ReasoningContent != "" {
					reasoningFrames++
				}
			}
		}
		// 按模型行为断言「思考落到了客户端」，不对具体文案断言：
		// 钉死措辞会让一次无关的模型更新变成 smoke 失败。
		if reasoningFrames == 0 {
			t.Error("下游流没有任何带 reasoning_content 的 chunk，深度思考没有落到客户端")
		}
	})
}
