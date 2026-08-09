// Package passthrough 是同源快通道的出站适配器（原则 2.2）。
//
// 入站协议与出站 Provider 属于同一线格式时，网关唯一需要做的就是改写鉴权与
// 模型名——其余字节原样转发。这既保住了 TTFT，也保住了我们**没有建模**的字段：
// 一个上游刚发布、网关还不认识的新参数，走这条路能正常工作。
package passthrough

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/protocol/openaiwire"
	"github.com/yobo2u/omugw/internal/provider"
	"github.com/yobo2u/omugw/internal/transport/httpx"
)

// Provider 是 OpenAI 系的同源直通适配器。
type Provider struct {
	kind   degrade.Provider
	path   string
	client *httpx.Client
	now    func() time.Time
}

// New 构造直通适配器。path 是上游端点路径，例如 "/v1/responses"。
func New(kind degrade.Provider, path string, c *httpx.Client, now func() time.Time) *Provider {
	if now == nil {
		now = time.Now
	}
	return &Provider{kind: kind, path: path, client: c, now: now}
}

// Kind 返回协议族。
func (p *Provider) Kind() degrade.Provider { return p.kind }

// Call 把请求原样转给上游，只改写鉴权与模型名。
func (p *Provider) Call(ctx context.Context, req provider.Request) (*httpx.Response, error) {
	body, err := rewriteModel(req.Raw, req.Target.UpstreamModel)
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
	// 转发它等于把网关的 API Key 泄露给上游，而上游没有理由知道它。
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

func acceptFor(stream bool) string {
	if stream {
		return "text/event-stream"
	}
	return "application/json"
}

// pathFor 优先用请求携带的上游路径，缺省退回装配时的默认路径。
//
// 同一个 openai.compat 适配器要同时服务 Responses 与 Chat 两个上游端点，
// 路径只能随请求走；保留默认值是为了不影响既有单上游的装配。
func (p *Provider) pathFor(req provider.Request) string {
	if req.Path != "" {
		return req.Path
	}
	return p.path
}

// decodeError 读出错误体并解成统一错误。
//
// 读之前设上限：一个故障上游可能在 500 里塞进一整个 HTML 页面，甚至更糟。
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

// rewriteModel 把请求体里的 model 换成上游真实模型名。
//
// 解成 map[string]json.RawMessage 再写回，而不是整体反序列化：这样除 model
// 之外的每个字段都保持原始字节，包括网关不认识的新参数。用结构体往返一次，
// 那些字段就悄悄消失了——而客户端不会收到任何提示。
func rewriteModel(raw []byte, upstream string) ([]byte, error) {
	if upstream == "" {
		return nil, canonical.Newf(canonical.ClassInternal, "路由目标缺少上游模型名")
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassBadRequest, "请求体不是 JSON 对象")
	}

	cur, ok := fields["model"]
	if !ok {
		return nil, canonical.Newf(canonical.ClassBadRequest, "请求体缺少 model")
	}

	// 逻辑名与上游名相同时不动它，连重新序列化都省掉——真正的零改动转发。
	var name string
	if err := json.Unmarshal(cur, &name); err == nil && name == upstream {
		return raw, nil
	}

	patched, err := json.Marshal(upstream)
	if err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassInternal, "序列化模型名失败")
	}
	fields["model"] = patched

	out, err := json.Marshal(fields)
	if err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassInternal, "重新序列化请求体失败")
	}
	return out, nil
}

// Describe 返回适配器的可读摘要，用于启动日志。
func (p *Provider) Describe() string {
	return fmt.Sprintf("passthrough(kind=%s path=%s)", p.kind, p.path)
}
