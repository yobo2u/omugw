// Package dashscoperealtime 只为同契约 DashScope Realtime 构造固定目标的干净握手。
package dashscoperealtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/protocol/dashscopewire"
	"github.com/yobo2u/omugw/internal/provider"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

const maxHandshakeBytes = 64 << 10

type Provider struct {
	timeouts config.Timeouts
	limits   config.WebSocket
	budget   *ws.BufferBudget
}

func New(timeouts config.Timeouts, limits config.WebSocket, budget *ws.BufferBudget) *Provider {
	return &Provider{timeouts: timeouts, limits: limits, budget: budget}
}

func (p *Provider) Kind() degrade.Provider { return degrade.ProviderDashScopeWSRealtime }

// ValidateHeaders 供 handler 在取得凭据和拨号前拒绝歧义头；Dial 再检一次防绕过。
// Origin 和压缩提议不影响准入；子协议可能携带浏览器密钥，必须拒绝而不是静默丢弃。
func ValidateHeaders(h http.Header) error {
	seen := map[string]bool{}
	authSources := 0
	for name, values := range h {
		name = strings.ToLower(name)
		switch name {
		case "sec-websocket-protocol":
			return canonical.Newf(canonical.ClassBadRequest, "DashScope Realtime 不支持子协议")
		case "authorization", "api-key", "x-dashscope-workspace", "x-dashscope-datainspection":
			if seen[name] || len(values) != 1 || hasControl(values[0]) {
				return canonical.Newf(canonical.ClassBadRequest, "DashScope Realtime 鉴权或租户头必须唯一且无控制字符")
			}
			seen[name] = true
			if name == "authorization" || name == "api-key" {
				authSources++
			}
		}
	}
	if authSources > 1 {
		return canonical.Newf(canonical.ClassBadRequest, "DashScope Realtime 鉴权来源不唯一")
	}
	return nil
}

func hasControl(s string) bool { return strings.ContainsFunc(s, unicode.IsControl) }

func (p *Provider) Dial(ctx context.Context, req provider.Request) (*ws.Conn, *http.Response, error) {
	if req.Target.Kind != p.Kind() || req.Inbound.Protocol != degrade.ProtoDashScopeRealtime || req.Inbound.Endpoint != degrade.EndpointDashScopeRealtime {
		return nil, nil, canonical.Newf(canonical.ClassUnsupported, "DashScope Realtime 出站不支持该入站坐标或目标协议")
	}
	if err := ValidateHeaders(req.Header); err != nil {
		return nil, nil, err
	}
	if p.limits.Validate() != nil || p.timeouts.Validate() != nil || p.budget == nil {
		return nil, nil, canonical.Newf(canonical.ClassInternal, "DashScope Realtime 缺少有效限额、超时或共享预算")
	}
	if req.Credential.Secret == "" || hasControl(req.Credential.Secret) {
		return nil, nil, canonical.Newf(canonical.ClassInternal, "DashScope Realtime 上游凭据非法")
	}
	u, err := upstreamURL(req.Target.BaseURL, req.Target.UpstreamModel)
	if err != nil {
		return nil, nil, err
	}
	h := http.Header{"Authorization": {"Bearer " + req.Credential.Secret}, "User-Agent": {"omugw"}}
	for name, values := range req.Header {
		if strings.EqualFold(name, "X-DashScope-WorkSpace") || strings.EqualFold(name, "X-DashScope-DataInspection") {
			h.Set(name, values[0])
		}
	}
	// first_byte 由网关跨候选共用；本层只给 TCP/TLS、空闲和写入各自的期限。
	c, resp, err := ws.Dial(ctx, u, ws.DialOptions{
		Header: h, MaxPayload: p.limits.MaxMessageBytes, Budget: p.budget,
		ConnectTimeout: p.timeouts.Connect, Idle: p.timeouts.Idle,
		WriteTimeout:      min(p.timeouts.Connect, p.timeouts.Idle),
		MaxHandshakeBytes: maxHandshakeBytes, MaxErrorBodyBytes: maxHandshakeBytes,
	})
	if err == nil {
		return c, resp, nil
	}
	resp, err = sanitizeFailure(ctx, resp)
	return nil, resp, err
}

func upstreamURL(base, model string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(base, "#") {
		return "", canonical.Newf(canonical.ClassInternal, "DashScope Realtime 上游地址非法")
	}
	switch u.Scheme {
	case "http", "ws":
		u.Scheme = "ws"
	case "https", "wss":
		u.Scheme = "wss"
	default:
		return "", canonical.Newf(canonical.ClassInternal, "DashScope Realtime 上游地址协议非法")
	}
	if model == "" {
		return "", canonical.Newf(canonical.ClassInternal, "DashScope Realtime 路由目标缺少模型名")
	}
	// 用转义路径拼接，避免部署前缀中的 %2F 被解码成路由分隔符。
	path := strings.TrimRight(u.EscapedPath(), "/")
	endpoint := string(degrade.EndpointDashScopeRealtime)
	if !strings.HasSuffix(path, endpoint) {
		path += endpoint
	}
	u.Path, _ = url.PathUnescape(path)
	u.RawPath = path
	u.RawQuery = url.Values{"model": {model}}.Encode()
	return u.String(), nil
}

// 错误体只参与分类，不向调用方暴露；返回的 Response 同样不能夹带 Location、URL
// 或任意上游头。错误无 cause，避免后续 unwrap/log 把原始网络错误重新带出去。
func sanitizeFailure(ctx context.Context, resp *http.Response) (*http.Response, error) {
	e := canonical.Newf(canonical.ClassInternal, "DashScope Realtime 上游握手失败")
	if resp == nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			e.Class, e.Retryable = canonical.ClassUpstreamUnavailable, true
		}
		return nil, e
	}
	var body []byte
	if resp.Body != nil {
		body, _ = io.ReadAll(io.LimitReader(resp.Body, maxHandshakeBytes))
		_ = resp.Body.Close()
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 600 {
		e = dashscopewire.DecodeError(resp.StatusCode, body, resp.Header, time.Now())
	}
	e.Message = "DashScope Realtime 上游握手失败"
	e.UpstreamCode = "RealtimeHandshakeFailed"
	e.UpstreamRequestID = ""
	e.UpstreamStatus = resp.StatusCode
	h := make(http.Header)
	for k, v := range e.Headers() {
		h.Set(k, v)
	}
	return &http.Response{
		StatusCode: resp.StatusCode, Status: fmt.Sprintf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode)),
		Header: h, Body: http.NoBody,
	}, e
}
