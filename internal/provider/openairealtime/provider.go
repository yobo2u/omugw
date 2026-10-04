// Package openairealtime 只接受同协议 GA 坐标，防止跨协议或 Beta 握手混入原字节通道。
package openairealtime

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
	"github.com/yobo2u/omugw/internal/protocol/openaiwire"
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

func (p *Provider) Kind() degrade.Provider { return degrade.ProviderOpenAIRealtime }

// ValidateHeaders 供 handler 在借用凭据前预检；Dial 再检以防直接调用绕过。
// 按头名存在性拒绝 Beta/子协议，空值也不能被静默转换为 GA 或泄漏浏览器密钥。
func ValidateHeaders(h http.Header) error {
	seen := map[string]bool{}
	authSources := 0
	for name, values := range h {
		name = strings.ToLower(name)
		switch name {
		case "openai-beta", "sec-websocket-protocol":
			return canonical.Newf(canonical.ClassBadRequest, "OpenAI Realtime GA 不支持 Beta 或子协议")
		case "authorization", "api-key", "openai-safety-identifier":
			if seen[name] || len(values) != 1 || hasControl(values[0]) {
				return canonical.Newf(canonical.ClassBadRequest, "OpenAI Realtime 鉴权或安全标识头必须唯一且无控制字符")
			}
			seen[name] = true
			if name == "authorization" || name == "api-key" {
				authSources++
			}
		}
	}
	if authSources > 1 {
		return canonical.Newf(canonical.ClassBadRequest, "OpenAI Realtime 鉴权来源不唯一")
	}
	return nil
}

func hasControl(s string) bool { return strings.ContainsFunc(s, unicode.IsControl) }

func (p *Provider) Dial(ctx context.Context, req provider.Request) (*ws.Conn, *http.Response, error) {
	if req.Target.Kind != p.Kind() || req.Inbound.Protocol != degrade.ProtoOpenAIRealtime || req.Inbound.Endpoint != degrade.EndpointOpenAIRealtime {
		return nil, nil, canonical.Newf(canonical.ClassUnsupported, "OpenAI Realtime 出站不支持该入站坐标或目标协议")
	}
	if err := ValidateHeaders(req.Header); err != nil {
		return nil, nil, err
	}
	if p.limits.Validate() != nil || p.timeouts.Validate() != nil || p.budget == nil {
		return nil, nil, canonical.Newf(canonical.ClassInternal, "OpenAI Realtime 缺少有效限额、超时或共享预算")
	}
	if req.Credential.Secret == "" || hasControl(req.Credential.Secret) {
		return nil, nil, canonical.Newf(canonical.ClassInternal, "OpenAI Realtime 上游凭据非法")
	}
	u, err := upstreamURL(req.Target.BaseURL, req.Target.UpstreamModel)
	if err != nil {
		return nil, nil, err
	}
	h := http.Header{"Authorization": {"Bearer " + req.Credential.Secret}, "User-Agent": {"omugw"}}
	for name, values := range req.Header {
		// 与预检使用同一归一规则，防止 Unicode 折叠扩大已经校验的白名单。
		if strings.ToLower(name) == "openai-safety-identifier" {
			h.Set("OpenAI-Safety-Identifier", values[0])
		}
	}
	// first_byte 由协调器跨候选共用，不能在本层重置；HTTP total 不约束长会话。
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
		return "", canonical.Newf(canonical.ClassInternal, "OpenAI Realtime 上游地址非法")
	}
	switch u.Scheme {
	case "http", "ws":
		u.Scheme = "ws"
	case "https", "wss":
		u.Scheme = "wss"
	default:
		return "", canonical.Newf(canonical.ClassInternal, "OpenAI Realtime 上游地址协议非法")
	}
	if model == "" {
		return "", canonical.Newf(canonical.ClassInternal, "OpenAI Realtime 路由目标缺少模型名")
	}
	// 保留转义前缀，防止 %2F 被解释为新的部署层级；固定完整后缀不重复追加。
	path := strings.TrimRight(u.EscapedPath(), "/")
	endpoint := string(degrade.EndpointOpenAIRealtime)
	if !strings.HasSuffix(path, endpoint) {
		path += endpoint
	}
	u.Path, _ = url.PathUnescape(path)
	u.RawPath = path
	u.RawQuery = url.Values{"model": {model}}.Encode()
	return u.String(), nil
}

// 仅失败 HTTP 信封参与分类；错误、Response、unwrap 都不能带回原文或地址。
// 坏 101 与重定向不从 body 猜重试；WS close 留给协议观察器，不能套 HTTP 分类。
func sanitizeFailure(ctx context.Context, resp *http.Response) (*http.Response, error) {
	e := canonical.Newf(canonical.ClassInternal, "OpenAI Realtime 上游握手失败")
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
		e = openaiwire.DecodeError(resp.StatusCode, body, resp.Header, time.Now())
	}
	e.Message = "OpenAI Realtime 上游握手失败"
	e.UpstreamCode = "realtime_handshake_failed"
	e.Param, e.UpstreamRequestID = "", ""
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
