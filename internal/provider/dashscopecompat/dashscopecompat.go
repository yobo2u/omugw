// Package dashscopecompat 是 DashScope Compatible（OpenAI 线格式）的出站适配器。
//
// wire-compatible 而语义异构：请求与响应仍走 Chat Completions 线格式，因此不做
// Canonical 重编码；但上游语义与 OpenAI 并不等同——模型能力、结构化输出保证、
// Web Search 参数都有差异。所以这条路**不是**同源快通道，不得 MarkHomogeneous：
// wire-compatible 只说明不需要重编码，不能推导为语义零损失，X-Omugw-Degraded
// 仍由矩阵生成。
//
// 对客户端原始 JSON 只做两处定点修补：改写模型名；把非 null 的
// web_search_options 映射成 enable_search: true。其余字段保持原始字节——
// 当前 IR 不承载 n、presence/frequency penalty、logprobs 等全部 Chat 参数，
// 经 Canonical 往返会制造静默损失。
package dashscopecompat

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/protocol/openaiwire"
	"github.com/yobo2u/omugw/internal/provider"
	"github.com/yobo2u/omugw/internal/transport/httpx"
)

// ChatCompletionsPath 是 DashScope Compatible Chat 的上游端点。
const ChatCompletionsPath = "/v1/chat/completions"

// Provider 只负责 OpenAI Chat compatible wire 的出站。
type Provider struct {
	client *httpx.Client
	now    func() time.Time
}

// New 构造适配器。now 可注入以便测试，传 nil 用 time.Now。
func New(c *httpx.Client, now func() time.Time) *Provider {
	if now == nil {
		now = time.Now
	}
	return &Provider{client: c, now: now}
}

// Kind 返回协议族。
func (p *Provider) Kind() degrade.Provider { return degrade.ProviderDashScopeCompatible }

// Call 把请求发给 DashScope Compatible。
//
// 原始 JSON 只做两处定点修补，响应原样返回给网关转发。Canonical 已在网关层
// 完成请求校验与能力裁决，这里不消费——本路径没有 Canonical 出站编码器。
func (p *Provider) Call(ctx context.Context, req provider.Request) (*httpx.Response, error) {
	body, err := patch(req.Raw, req.Target.UpstreamModel)
	if err != nil {
		return nil, err
	}

	url := strings.TrimSuffix(req.Target.BaseURL, "/") + p.pathFor(req)
	hreq, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassInternal, "构造上游请求失败")
	}

	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", acceptFor(req.Stream))
	// 网关用**自己的**凭据。客户端发来的 Authorization 到此为止——
	// 转发它等于把客户端密钥泄给上游，而上游没有理由知道它。
	hreq.Header.Set("Authorization", "Bearer "+req.Credential.Secret)

	resp, err := p.client.Do(ctx, hreq)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode >= 400 {
		return nil, p.decodeError(resp)
	}
	return resp, nil
}

// pathFor 优先用请求携带的路径，留空退回本适配器唯一的端点。
func (p *Provider) pathFor(req provider.Request) string {
	if req.Path != "" {
		return req.Path
	}
	return ChatCompletionsPath
}

func acceptFor(stream bool) string {
	if stream {
		return "text/event-stream"
	}
	return "application/json"
}

// decodeError 读出错误体并按 OpenAI 信封解码——DashScope Compatible 的
// 非 2xx 与 OpenAI 同形。读之前设上限：一个故障上游可能在 5xx 里塞进
// 一整个 HTML 页面，甚至更糟。
func (p *Provider) decodeError(resp *httpx.Response) error {
	defer resp.Body.Close()

	const maxErrorBody = 64 << 10
	body := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for len(body) < maxErrorBody {
		n, err := resp.Body.Read(buf)
		body = append(body, buf[:n]...)
		if err != nil {
			break
		}
	}
	return openaiwire.DecodeError(resp.StatusCode, body, resp.Header, p.now())
}

// patch 对客户端原始 JSON 做仅有的两处修改：改写模型名；把非 null 的
// web_search_options 映射成 enable_search: true 并删除原字段。
//
// 解成 map[string]json.RawMessage 再写回，而不是整体反序列化：这样除修补点
// 之外的每个字段都保持原始字节，包括网关不认识的新参数。键序会随重新序列化
// 改变，语义不变——一致性断言按语义比对，不做字节比对。
//
// 两者同时出现时映射覆盖客户端自带的 enable_search：发了 web_search_options
// 就是客户端的显式搜索请求，而 enable_search 是 DashScope 原生参数，
// OpenAI 客户端本不会发它。
func patch(raw []byte, upstreamModel string) ([]byte, error) {
	if upstreamModel == "" {
		return nil, canonical.Newf(canonical.ClassInternal, "路由目标缺少上游模型名")
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassBadRequest, "请求体不是 JSON 对象")
	}
	if _, ok := fields["model"]; !ok {
		return nil, canonical.Newf(canonical.ClassBadRequest, "请求体缺少 model")
	}

	patched, err := json.Marshal(upstreamModel)
	if err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassInternal, "序列化模型名失败")
	}
	fields["model"] = patched

	// 仅当客户端发了非 null 对象时映射：缺省或 null 是「不要搜索」
	//（解码器已按此语义识别能力），此时不得替它注入 enable_search——
	// 替客户端开搜索会改输出内容并额外计费，而响应里看不出网关做过这件事。
	// search_context_size / user_location 没有 DashScope 落点，不猜测映射——
	// 损失登记在降级矩阵，随响应头告知客户端。
	if wso, ok := fields["web_search_options"]; ok && string(wso) != "null" {
		delete(fields, "web_search_options")
		fields["enable_search"] = json.RawMessage("true")
	}

	out, err := json.Marshal(fields)
	if err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassInternal, "重新序列化请求体失败")
	}
	return out, nil
}
