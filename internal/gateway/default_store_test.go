package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestOmittedStoreIsNotGatedByConvStore 防的是「默认配置下网关整条 Responses
// 入站不可用」。
//
// store 省略时按 OpenAI 协议默认值 true 处理是对的，但那只是**写入端的默认**，
// 不等于调用方在依赖网关保管的历史。把省略也算成要求服务端会话，会让
// convstore 默认关闭的部署里几乎每个请求都撞上 422——而多数 SDK 根本不发这个
// 字段，它们只是想要一次普通的生成。
//
// 默认配置就是绝大多数人的实际部署（ADR-0002），因此这条路必须在默认配置下
// 通到上游。
func TestOmittedStoreIsNotGatedByConvStore(t *testing.T) {
	up := jsonUpstream(t, `{"id":"upstream","object":"response","status":"completed",`+
		`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`)
	// 默认可用性：convstore 关闭，与 DefaultAvailability 一致，不做任何开关改写。
	hs := newHarness(t, true, up)

	rec := hs.do(t, `{"model":"logical-fast","input":"你好"}`, true)

	if rec.Code != http.StatusOK {
		t.Fatalf("省略 store 的普通请求被拒: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := up.calls.Load(); got != 1 {
		t.Fatalf("请求没有打到上游，上游调用=%d", got)
	}
}

// TestExplicitStoreTrueStillRequiresConvStore 固化另一半：显式 store:true 是
// 真的要求网关保管这一轮，开关没开时必须拒绝，并说清是哪个开关。
//
// 两条测试必须成对存在。只测放行，会让「默认配置下干脆不再门控服务端会话」
// 这种过度修复照样通过。
func TestExplicitStoreTrueStillRequiresConvStore(t *testing.T) {
	up := jsonUpstream(t, `{"id":"upstream","object":"response","status":"completed",`+
		`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`)
	hs := newHarness(t, true, up)

	rec := hs.do(t, `{"model":"logical-fast","input":"你好","store":true}`, true)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("显式 store:true 未被开关拦下: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := up.calls.Load(); got != 0 {
		t.Fatalf("开关未开却打到了上游 %d 次", got)
	}
	if body := rec.Body.String(); !strings.Contains(body, "convstore") {
		t.Fatalf("错误没有指明是哪个开关未开启: %s", body)
	}
}

// TestOmittedStoreIsNotPersistedWhenConvStoreDisabled 防的是「矩阵放行之后
// handler 仍按默认 true 去写存储」。
//
// 默认配置下根本没有装配 ConversationStore，若写入路径仍被触发，请求会变成
// 500——把一个已经修好的 422 换成一个更难查的内部错误。
func TestOmittedStoreIsNotPersistedWhenConvStoreDisabled(t *testing.T) {
	up := jsonUpstream(t, `{"id":"upstream","object":"response","status":"completed",`+
		`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`)
	hs := newHarness(t, true, up)
	if hs.h.deps.ConversationStore != nil {
		t.Fatal("默认 harness 不该装配会话存储")
	}

	rec := hs.do(t, `{"model":"logical-fast","input":"你好"}`, true)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d: %s", rec.Code, rec.Body.String())
	}
	// 没有本地会话垫层时，响应里的 id 应当仍是上游那一个——网关没有改写它，
	// 也就不存在一个客户端能拿去续聊、实际却查不到的假 ID。
	var meta struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	if meta.ID != "upstream" {
		t.Fatalf("未启用会话存储却改写了 response.id: %q", meta.ID)
	}
}
