package dashscopecompat

import (
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
