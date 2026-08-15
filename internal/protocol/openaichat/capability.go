// 能力识别与搜索选项的严格解码。
//
// 单独成文是因为这两段属于「入站边界向矩阵报告能力」这一件事，与 decode.go
// 里逐字段的线格式还原不是同一职责。
package openaichat

import (
	"encoding/json"
	"strings"

	"github.com/yobo2u/omugw/internal/canonical"
)

// Capabilities 报告这次请求用到了哪些能力，供降级矩阵裁决。
//
// Chat 无状态，没有 Responses 那样的会话读写端，直接复用 Canonical 的推导，
// 再补上两项 OpenAI 特有开关：它们没有 Canonical 字段，异构出站又读不得
// Extensions，能力识别只能在解码阶段完成。结果统一按 AllCapabilities 的顺序
// 输出——golden 文件依赖这一点稳定。
func (d *Decoded) Capabilities() []canonical.Capability {
	caps := d.Request.UsedCapabilities()
	if !d.webSearch && !d.parallelToolCalls {
		return caps
	}
	seen := make(map[canonical.Capability]bool, len(caps)+2)
	for _, c := range caps {
		seen[c] = true
	}
	if d.webSearch {
		seen[canonical.CapWebSearch] = true
	}
	if d.parallelToolCalls {
		seen[canonical.CapParallelToolCalls] = true
	}
	out := make([]canonical.Capability, 0, len(seen))
	for _, c := range canonical.AllCapabilities() {
		if seen[c] {
			out = append(out, c)
		}
	}
	return out
}

// decodeWebSearchOptions 严格解码搜索选项。
//
// 另起一个 DisallowUnknownFields 的解码器：外层的严格模式只管顶层字段，
// RawMessage 子树绕过了它——而这个对象在出站前会被整体删除，
// 未知子字段必须在入站就拒掉，不能等它悄悄消失。
func decodeWebSearchOptions(raw json.RawMessage) (*WebSearchOptions, error) {
	var wso WebSearchOptions
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wso); err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassBadRequest,
			"web_search_options 无法解析")
	}
	if size := wso.SearchContextSize; size != nil {
		switch *size {
		case "low", "medium", "high":
		default:
			return nil, canonical.Newf(canonical.ClassBadRequest,
				"不支持的 search_context_size %q", *size)
		}
	}
	if loc := wso.UserLocation; loc != nil {
		if loc.Type != "approximate" {
			return nil, canonical.Newf(canonical.ClassBadRequest,
				"不支持的 user_location.type %q", loc.Type)
		}
		if loc.Approximate == nil {
			return nil, canonical.Newf(canonical.ClassBadRequest,
				"user_location 缺少 approximate")
		}
	}
	return &wso, nil
}
