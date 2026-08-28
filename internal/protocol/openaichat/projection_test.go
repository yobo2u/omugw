// 严格 Chat 视图的入站边界测试：stream_options 子解码与具名出站投影。
//
// 防的是同一类失效：外层 DisallowUnknownFields 管不到 RawMessage 子树，
// 而 Canonical 承载不了 n / presence_penalty 这类采样选项——两处都会让
// 客户端显式提交的字段悄无声息地消失，请求照样返回 200。
package openaichat

import (
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

// TestStreamOptionsRejectsUnknownSubfield 钉死严格子解码。
//
// stream_options 是 json.RawMessage，外层 DisallowUnknownFields 进不到子树；
// 若不补严格子解码器，未知子字段会被静默吞掉。按 web_search_options 的先例，
// 未知子字段必须 400。
func TestStreamOptionsRejectsUnknownSubfield(t *testing.T) {
	_, err := Decode([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],
	  "stream":true,"stream_options":{"include_usage":true,"bogus":1}}`))
	if err == nil {
		t.Fatal("stream_options 未知子字段应被拒绝")
	}
	if canonical.AsError(err).Class != canonical.ClassBadRequest {
		t.Fatalf("应为 400，实际 %v", err)
	}
}

// TestStreamOptionsIncludeUsageIsDecoded 钉死 include_usage 被解出，供 usage chunk 决策。
func TestStreamOptionsIncludeUsageIsDecoded(t *testing.T) {
	d, err := Decode([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],
	  "stream":true,"stream_options":{"include_usage":true}}`))
	if err != nil {
		t.Fatalf("合法 stream_options 应被接受: %v", err)
	}
	if !d.StreamOptionsIncludeUsage() {
		t.Fatal("include_usage=true 应被解出")
	}
}

// TestProjectionEnumeratesAcceptedFields 钉死投影必须枚举全部已接受字段，
// 且显式暴露 Canonical 未承载的字段，供异构出站映射与 422 判定。
func TestProjectionEnumeratesAcceptedFields(t *testing.T) {
	p, err := Project([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],
	  "n":2,"presence_penalty":0.5,"logprobs":true,"top_logprobs":3,
	  "parallel_tool_calls":false,"frequency_penalty":0.9,"user":"u-1",
	  "store":true,"service_tier":"default","metadata":{"k":"v"},
	  "audio":{"voice":"alloy","format":"wav"}}`))
	if err != nil {
		t.Fatalf("合法字段应被投影接受: %v", err)
	}
	if p.N == nil || *p.N != 2 {
		t.Errorf("N 应解出 2，实际 %+v", p.N)
	}
	if p.PresencePenalty == nil || *p.PresencePenalty != 0.5 {
		t.Errorf("PresencePenalty 应解出，实际 %+v", p.PresencePenalty)
	}
	if !p.FrequencyPenaltyPresent || !p.UserPresent || !p.StorePresent ||
		!p.ServiceTierPresent || !p.MetadataPresent || !p.AudioPresent {
		t.Errorf("无落点字段的显式提交必须可识别: %+v", p)
	}
}

// TestProjectionRejectsUnknownField 钉死投影不静默丢弃未知字段。
func TestProjectionRejectsUnknownField(t *testing.T) {
	if _, err := Project([]byte(`{"model":"m","messages":[],"totally_new":1}`)); err == nil {
		t.Fatal("投影未知字段应被拒绝")
	}
}

// TestProjectionPresenceUsesObjectKeys 钉死「显式提交」按 JSON key 是否存在判断，
// 不能把空串、空对象、零值或 null 误判为缺省。
func TestProjectionPresenceUsesObjectKeys(t *testing.T) {
	p, err := Project([]byte(`{"model":"m","messages":[],"frequency_penalty":0,
	  "logit_bias":{},"service_tier":"","store":false,"user":"",
	  "metadata":{},"audio":null}`))
	if err != nil {
		t.Fatal(err)
	}
	if !p.FrequencyPenaltyPresent || !p.LogitBiasPresent || !p.ServiceTierPresent ||
		!p.StorePresent || !p.UserPresent || !p.MetadataPresent || !p.AudioPresent {
		t.Fatalf("显式零值/null 也必须算提交: %+v", p)
	}
}
