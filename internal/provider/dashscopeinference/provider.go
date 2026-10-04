// Package dashscopeinference 只构造固定连接握手，模型由升级后的任务消息声明。
package dashscopeinference

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

func (p *Provider) Kind() degrade.Provider { return degrade.ProviderDashScopeWSInference }

// ValidateHeaders 供 handler 在借凭据前预检；Dial 重检，防止其他调用方绕过。
// Origin 与扩展提议不影响准入；子协议可能携带密钥，必须显式拒绝。
func ValidateHeaders(h http.Header) error {
	seen := map[string]bool{}
	authSources := 0
	for name, values := range h {
		name = strings.ToLower(name)
		switch name {
		case "sec-websocket-protocol":
			return canonical.Newf(canonical.ClassBadRequest, "DashScope Inference 不支持子协议")
		case "authorization", "api-key", "x-dashscope-workspace", "x-dashscope-datainspection":
			if seen[name] || len(values) != 1 || hasControl(values[0]) {
				return canonical.Newf(canonical.ClassBadRequest, "DashScope Inference 鉴权或租户头必须唯一且无控制字符")
			}
			seen[name] = true
			if name == "authorization" || name == "api-key" {
				authSources++
			}
		}
	}
	if authSources > 1 {
		return canonical.Newf(canonical.ClassBadRequest, "DashScope Inference 鉴权来源不唯一")
	}
	return nil
}

func hasControl(s string) bool { return strings.ContainsFunc(s, unicode.IsControl) }

func (p *Provider) Dial(ctx context.Context, req provider.Request) (*ws.Conn, *http.Response, error) {
	if req.Target.Kind != p.Kind() || req.Inbound.Protocol != degrade.ProtoDashScopeInference || req.Inbound.Endpoint != degrade.EndpointDashScopeInference {
		return nil, nil, canonical.Newf(canonical.ClassUnsupported, "DashScope Inference 出站不支持该入站坐标或目标协议")
	}
	if req.Target.UpstreamModel != "" {
		return nil, nil, canonical.Newf(canonical.ClassInternal, "DashScope Inference 握手目标不得携带模型名")
	}
	if err := ValidateHeaders(req.Header); err != nil {
		return nil, nil, err
	}
	if p.limits.Validate() != nil || p.timeouts.Validate() != nil || p.budget == nil {
		return nil, nil, canonical.Newf(canonical.ClassInternal, "DashScope Inference 缺少有效限额、超时或共享预算")
	}
	if req.Credential.Secret == "" || hasControl(req.Credential.Secret) {
		return nil, nil, canonical.Newf(canonical.ClassInternal, "DashScope Inference 上游凭据非法")
	}
	u, err := upstreamURL(req.Target.BaseURL)
	if err != nil {
		return nil, nil, err
	}
	h := http.Header{"Authorization": {"Bearer " + req.Credential.Secret}, "User-Agent": {"omugw"}}
	for name, values := range req.Header {
		if strings.EqualFold(name, "X-DashScope-WorkSpace") || strings.EqualFold(name, "X-DashScope-DataInspection") {
			h.Set(name, values[0])
		}
	}
	// first_byte 由协调器跨候选共用；total 不得截断升级后的长会话。
	c, resp, err := ws.Dial(ctx, u, ws.DialOptions{
		Header: h, MaxPayload: p.limits.MaxMessageBytes, Budget: p.budget,
		ConnectTimeout: p.timeouts.Connect, Idle: p.timeouts.Idle,
		WriteTimeout:      min(p.timeouts.Connect, p.timeouts.Idle),
		MaxHandshakeBytes: maxHandshakeBytes, MaxErrorBodyBytes: maxHandshakeBytes,
	})
	if err == nil {
		// task-started 依赖 101 后客户端发 run-task，本层不能等待首事件。
		return c, resp, nil
	}
	resp, err = sanitizeFailure(ctx, resp)
	return nil, resp, err
}

func upstreamURL(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(base, "#") {
		return "", canonical.Newf(canonical.ClassInternal, "DashScope Inference 上游地址非法")
	}
	switch u.Scheme {
	case "http", "ws":
		u.Scheme = "ws"
	case "https", "wss":
		u.Scheme = "wss"
	default:
		return "", canonical.Newf(canonical.ClassInternal, "DashScope Inference 上游地址协议非法")
	}
	// 保留转义部署前缀，防止 %2F 被解码成额外的路径分隔符。
	path := strings.TrimRight(u.EscapedPath(), "/")
	endpoint := string(degrade.EndpointDashScopeInference)
	if !strings.HasSuffix(path, endpoint) {
		path += endpoint
	}
	u.Path, _ = url.PathUnescape(path)
	u.RawPath = path
	return u.String(), nil
}

// 失败体仅供有界分类并在此释放；重新构造响应，避免 URL、原始头或密钥被日志带出。
// 不保留 cause，防止调用方 unwrap 后重新暴露网络错误中的不可信内容。
func sanitizeFailure(ctx context.Context, resp *http.Response) (*http.Response, error) {
	e := canonical.Newf(canonical.ClassInternal, "DashScope Inference 上游握手失败")
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
	e.Message = "DashScope Inference 上游握手失败"
	e.UpstreamCode = "InferenceHandshakeFailed"
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
