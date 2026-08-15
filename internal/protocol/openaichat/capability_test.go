// 能力识别的入站边界测试：web_search_options 与 parallel_tool_calls。
//
// 这两项是 OpenAI 特有开关，Canonical 没有对应字段。识别必须在解码器完成：
// 异构出站路径读不得 Extensions，矩阵只信 Capabilities() 的报告。
package openaichat

import (
	"errors"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

func TestDecodeReportsWebSearch(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra string
		want  bool
	}{
		{"空对象即开启", `,"web_search_options":{}`, true},
		{"带参数即开启", `,"web_search_options":{"search_context_size":"high",` +
			`"user_location":{"type":"approximate","approximate":{` +
			`"country":"CN","city":"上海","region":"上海市",` +
			`"timezone":"Asia/Shanghai"}}}`, true},
		{"缺省为关闭", ``, false},
		{"null 为关闭", `,"web_search_options":null`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"m","messages":[{"role":"user","content":"hi"}]` + tc.extra + `}`
			d, err := Decode([]byte(body))
			if err != nil {
				t.Fatalf("Decode 失败: %v", err)
			}
			if got := hasCap(d.Capabilities(), canonical.CapWebSearch); got != tc.want {
				t.Errorf("CapWebSearch = %v，期望 %v（caps=%v）", got, tc.want, d.Capabilities())
			}
		})
	}
}

func TestDecodeReportsParallelToolCallsOnlyWhenTrue(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra string
		want  bool
	}{
		{"显式 true 报告", `,"parallel_tool_calls":true`, true},
		{"显式 false 不报告", `,"parallel_tool_calls":false`, false},
		{"缺省不报告", ``, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"m","messages":[{"role":"user","content":"hi"}],` +
				`"tools":[{"type":"function","function":{"name":"f"}}]` + tc.extra + `}`
			d := mustDecode(t, body)
			if got := hasCap(d.Capabilities(), canonical.CapParallelToolCalls); got != tc.want {
				t.Errorf("CapParallelToolCalls = %v，期望 %v（caps=%v）", got, tc.want, d.Capabilities())
			}
		})
	}
}

// TestDecodeCapabilitiesKeepAllCapabilitiesOrder 钉死输出顺序：
// golden 与矩阵日志都依赖能力清单稳定，追加识别不得打乱 AllCapabilities 顺序。
func TestDecodeCapabilitiesKeepAllCapabilitiesOrder(t *testing.T) {
	d := mustDecode(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],`+
		`"stream":true,"tools":[{"type":"function","function":{"name":"f"}}],`+
		`"parallel_tool_calls":true,"web_search_options":{}}`)

	want := []canonical.Capability{
		canonical.CapTextGeneration,
		canonical.CapStreaming,
		canonical.CapToolCalling,
		canonical.CapParallelToolCalls,
		canonical.CapWebSearch,
	}
	got := d.Capabilities()
	if len(got) != len(want) {
		t.Fatalf("能力数 = %d，期望 %d：%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 项 = %q，期望 %q（完整 %v）", i, got[i], want[i], got)
		}
	}
}

// TestDecodeRejectsUnknownWebSearchSubfield 钉死嵌套严格：
// 该对象在异构出站前被整体删除，未建模的子字段必须 400，不得静默吞掉。
func TestDecodeRejectsUnknownWebSearchSubfield(t *testing.T) {
	_, err := Decode([]byte(`{"model":"m","messages":[{"role":"user","content":"x"}],
	  "web_search_options":{"mystery_field":1}}`))
	if err == nil {
		t.Fatal("未知子字段应当被拒绝")
	}
	var cerr *canonical.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("应返回 *canonical.Error，实际为 %T", err)
	}
	if cerr.Class != canonical.ClassBadRequest {
		t.Errorf("错误分类应为 bad_request，实际为 %q", cerr.Class)
	}
}

// TestDecodeRejectsMalformedWebSearchOptions 形态非法一律 400，
// 不得默认为开启搜索。
func TestDecodeRejectsMalformedWebSearchOptions(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","messages":[{"role":"user","content":"x"}],"web_search_options":"yes"}`,
		`{"model":"m","messages":[{"role":"user","content":"x"}],"web_search_options":[1,2]}`,
		`{"model":"m","messages":[{"role":"user","content":"x"}],"web_search_options":{"search_context_size":"ultra"}}`,
		// 显式空串不等于缺省：客户端确实写了这个字段，只是写错了。
		// 若与缺省同流，非法值会被当成「没设置」放过，搜索档位悄悄丢失。
		`{"model":"m","messages":[{"role":"user","content":"x"}],"web_search_options":{"search_context_size":""}}`,
		`{"model":"m","messages":[{"role":"user","content":"x"}],"web_search_options":{"user_location":{"type":"exact","approximate":{"city":"上海"}}}}`,
		`{"model":"m","messages":[{"role":"user","content":"x"}],"web_search_options":{"user_location":{"type":"approximate"}}}`,
	} {
		if _, err := Decode([]byte(body)); err == nil {
			t.Errorf("形态非法的 web_search_options 应当被拒绝: %s", body)
		}
	}
}
