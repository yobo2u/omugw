package dashscopecompat

import (
	"bytes"
	"encoding/json"
	"net/http"
	"time"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/protocol/dashscopewire"
	"github.com/yobo2u/omugw/internal/protocol/openaiwire"
)

// decodeErrorBody 按入站门挑错误信封。
//
// Responses 门的上游返回 DashScope 的扁平信封 {request_id, code, message}
// （2026-10-11 实测），与 Chat 门的 OpenAI 嵌套信封不同形。拿嵌套解码器去读
// 扁平体不会报错，只会静默取到空串——客户端看到的是一句 Bad Request，而
// 靠 code 才认得出的分类（限流、欠费、内容审核）会全部退化成按状态码猜。
//
// 先认扁平、取不到再回落嵌套：官方文档里两种形状并存，写成二选一迟早撞上
// 另一种。
func decodeErrorBody(ep degrade.Endpoint, status int, body []byte, h http.Header, now time.Time) *canonical.Error {
	if ep == degrade.EndpointOpenAIResponses && isFlatEnvelope(body) {
		return dashscopewire.DecodeError(status, body, h, now)
	}
	return openaiwire.DecodeError(status, body, h, now)
}

// isFlatEnvelope 判断错误体是否带顶层 code 或 message。
//
// 解析失败一律算「不是」：5xx 的 HTML 页面、空体、顶层 code 不是字符串的
// 怪形状，都交给回落分支按状态码处理。
func isFlatEnvelope(body []byte) bool {
	var probe struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	return probe.Code != "" || probe.Message != ""
}

const (
	typePreviewSearch = `"web_search_preview"`
	typeSearch        = `"web_search"`
)

// patchResponses 是 Responses 门的定点修补：改写模型名；把预览版内建搜索的
// 类型改成正式版。其余字段保持原始字节——会话字段（store、
// previous_response_id）尤其不能碰，本路径的会话由上游承担。
//
// 改名的理由：实测上游接受 web_search_preview 但不执行搜索，只有 web_search
// 生效。不改就是一次没发生的搜索配一个 200。
func patchResponses(raw []byte, upstreamModel string) ([]byte, error) {
	fields, err := withModel(raw, upstreamModel)
	if err != nil {
		return nil, err
	}

	if tools, ok := fields["tools"]; ok {
		fields["tools"] = renamePreviewSearchTools(tools)
	}
	// tool_choice 必须跟着改：工具改了名而选择器没改，它就指向一个不存在的工具。
	if choice, ok := fields["tool_choice"]; ok {
		fields["tool_choice"] = renamePreviewSearchType(choice)
	}

	out, err := json.Marshal(fields)
	if err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassInternal, "重新序列化请求体失败")
	}
	return out, nil
}

// renamePreviewSearchTools 逐项改名。形状不是数组（null 等）时原样返回：
// 解码器已在网关入口校验过请求，这里不重复裁决。
func renamePreviewSearchTools(raw json.RawMessage) json.RawMessage {
	var tools []json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return raw
	}
	changed := false
	for i, tool := range tools {
		renamed := renamePreviewSearchType(tool)
		if !bytes.Equal(renamed, tool) {
			tools[i] = renamed
			changed = true
		}
	}
	if !changed {
		return raw
	}
	out, err := json.Marshal(tools)
	if err != nil {
		return raw
	}
	return out
}

// renamePreviewSearchType 在对象的 type 恰为预览版搜索时改名，其余情形
// （字符串形态的 tool_choice、别的工具类型、非对象）原样返回。
func renamePreviewSearchType(raw json.RawMessage) json.RawMessage {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return raw
	}
	if !bytes.Equal(bytes.TrimSpace(obj["type"]), []byte(typePreviewSearch)) {
		return raw
	}
	obj["type"] = json.RawMessage(typeSearch)
	out, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	return out
}
