// Package gateway 把各层串成可服务的 HTTP 处理器。
//
// 这里是整条链路唯一汇合的地方：鉴权 → 解码 → 路由 → 降级矩阵裁决 →
// 凭据池 → 出站适配器 → 回写。每一层都在自己的包里被单独测过，这里只负责
// 把它们按正确的顺序接起来，以及一件别处做不了的事——**跟踪下游首字节**，
// 因为只有它知道客户端到底看到了什么。
package gateway

import (
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/credential"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/obs"
	"github.com/yobo2u/omugw/internal/protocol/openairesponses"
	"github.com/yobo2u/omugw/internal/protocol/openaiwire"
	"github.com/yobo2u/omugw/internal/provider"
	"github.com/yobo2u/omugw/internal/router"
	"github.com/yobo2u/omugw/internal/transport/httpx"
)

// Deps 是 Handler 的依赖。
type Deps struct {
	Matrix  *degrade.Matrix
	Router  *router.Router
	Auth    *Authenticator
	Limits  config.Limits
	Metrics *obs.Metrics
	Log     *slog.Logger
	Now     func() time.Time

	// Pools 按凭据池名索引。
	Pools map[string]*credential.Pool

	// Providers 按 endpoint 名索引出站适配器。
	Providers map[string]provider.Provider
}

// Handler 处理 Responses 入站请求。
type Handler struct {
	deps    Deps
	inbound degrade.Protocol
}

// NewResponsesHandler 构造 /v1/responses 的处理器。
func NewResponsesHandler(d Deps) *Handler {
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Handler{deps: d, inbound: degrade.ProtoOpenAIResponses}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tw := &tracked{ResponseWriter: w}
	start := h.deps.Now()

	outcome, outbound, err := h.serve(tw, r)

	if err != nil {
		h.fail(tw, err)
	}

	h.deps.Metrics.Requests.WithLabelValues(string(h.inbound), outbound, outcome).Inc()
	h.deps.Metrics.Duration.WithLabelValues(string(h.inbound), outbound).
		Observe(h.deps.Now().Sub(start).Seconds())
}

// serve 执行一次请求，返回结果分类、出站标识与错误。
func (h *Handler) serve(w *tracked, r *http.Request) (outcome, outbound string, err error) {
	outbound = "none"

	caller, err := h.deps.Auth.Authenticate(r)
	if err != nil {
		return "auth_failed", outbound, err
	}

	raw, err := h.readBody(r)
	if err != nil {
		return "bad_request", outbound, err
	}

	decoded, err := openairesponses.Decode(raw)
	if err != nil {
		return "bad_request", outbound, err
	}

	// 内联负载上限（原则 2.6）。没有它，一个塞满 base64 视频的请求就能把
	// 网关内存吃光——而这不需要恶意，一次误操作就够了。
	if decoded.InlineBytes > h.deps.Limits.MaxInlineBytes {
		return "bad_request", outbound, canonical.Newf(canonical.ClassBadRequest,
			"内联多模态负载 %d 字节，超过上限 %d",
			decoded.InlineBytes, h.deps.Limits.MaxInlineBytes)
	}

	targets, err := h.deps.Router.Resolve(decoded.Request.Model)
	if err != nil {
		return "bad_request", outbound, err
	}

	// 路由给出候选，矩阵按能力与保留度裁决。两者分工，不互相包含。
	kind, verdict, err := h.deps.Matrix.BestOutbound(
		h.inbound, router.Kinds(targets), decoded.Capabilities())
	if err != nil {
		if canonical.AsError(err).Class == canonical.ClassNotImplemented {
			h.deps.Metrics.ObserveNotImplemented(string(h.inbound), "planned")
			return "not_implemented", outbound, err
		}
		return "unsupported", outbound, err
	}
	outbound = string(kind)

	h.observeVerdict(kind, verdict)

	return h.dispatch(w, r, dispatchInput{
		caller:  caller,
		raw:     raw,
		decoded: decoded,
		targets: router.OfKind(targets, kind),
		kind:    kind,
		headers: verdictHeaders(verdict),
	})
}

type dispatchInput struct {
	caller  Caller
	raw     []byte
	decoded *openairesponses.Decoded
	targets []router.Target
	kind    degrade.Provider
	headers map[string]string
}

// dispatch 在候选上游之间做 failover。
//
// **只在下游首字节之前重试**（原则 2.4）。一旦向客户端发出过任何字节，
// 无论错误多么可重试都必须就此打住——重试会让客户端收到重复内容。
func (h *Handler) dispatch(w *tracked, r *http.Request, in dispatchInput) (string, string, error) {
	var lastErr error

	for _, target := range in.targets {
		pool, ok := h.deps.Pools[target.CredentialPool]
		if !ok {
			lastErr = canonical.Newf(canonical.ClassInternal,
				"路由目标 %s 引用了不存在的凭据池 %q", target.Endpoint, target.CredentialPool)
			continue
		}
		prov, ok := h.deps.Providers[target.Endpoint]
		if !ok {
			lastErr = canonical.Newf(canonical.ClassInternal,
				"没有为 endpoint %q 注册出站适配器", target.Endpoint)
			continue
		}

		tried := map[string]bool{}
		for {
			lease, err := pool.Acquire(tried)
			if err != nil {
				lastErr = err
				break // 这个上游的凭据用尽，换下一个上游
			}
			tried[lease.Credential.ID] = true

			resp, err := prov.Call(r.Context(), provider.Request{
				Target:     target,
				Credential: lease.Credential,
				Raw:        in.raw,
				Canonical:  &in.decoded.Request,
				Stream:     in.decoded.Request.Stream,
			})
			if err != nil {
				lease.Fail(err)
				cerr := canonical.AsError(err)
				h.deps.Metrics.ObserveError(string(in.kind), cerr)
				lastErr = err

				h.deps.Log.Warn("上游调用失败",
					"endpoint", target.Endpoint,
					"credential", lease.Credential.ID,
					"class", string(cerr.Class),
					"retryable", cerr.Retryable,
					"caller", in.caller.ID,
				)

				if cerr.Retryable {
					continue // 换一份凭据
				}
				break // 换凭据也没用，换下一个上游
			}

			lease.Succeed()

			// 走到这里就跨过了下游首字节的门槛：此后任何失败都只能收尾，
			// 不能重试。
			h.deps.Metrics.FirstByte.WithLabelValues(
				string(h.inbound), string(in.kind), "true",
			).Observe(resp.Latency.Seconds())

			usage, rerr := h.relay(w, resp, in)
			h.deps.Metrics.ObserveUsage(string(in.kind), usage)

			if rerr != nil {
				cerr := canonical.AsError(rerr)
				h.deps.Metrics.StreamAborted.WithLabelValues(
					string(h.inbound), string(in.kind), string(cerr.Class)).Inc()
				h.deps.Log.Warn("响应转发中断",
					"endpoint", target.Endpoint,
					"class", string(cerr.Class),
					"caller", in.caller.ID,
				)
				// 已经开始回写，fail 会识别出这一点并只记日志。
				return "stream_aborted", string(in.kind), rerr
			}
			return "ok", string(in.kind), nil
		}
	}

	if lastErr == nil {
		lastErr = canonical.Newf(canonical.ClassUpstreamUnavailable, "没有可用的上游")
	}
	return "upstream_error", string(in.kind), lastErr
}

// relay 按流式与否选择转发方式。
func (h *Handler) relay(w *tracked, resp *httpx.Response, in dispatchInput) (canonical.Usage, error) {
	if in.decoded.Request.Stream {
		return relayStream(w, resp, in.headers)
	}
	return relayJSON(w, resp, in.headers)
}

// readBody 读取请求体，带大小上限。
func (h *Handler) readBody(r *http.Request) ([]byte, error) {
	limited := io.LimitReader(r.Body, h.deps.Limits.MaxRequestBytes+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassBadRequest, "读取请求体失败")
	}
	if int64(len(raw)) > h.deps.Limits.MaxRequestBytes {
		return nil, canonical.Newf(canonical.ClassBadRequest,
			"请求体超过上限 %d 字节", h.deps.Limits.MaxRequestBytes)
	}
	return raw, nil
}

// observeVerdict 记录降级与模拟。
//
// 两者分开计数：降级意味着客户端少拿到了东西，模拟意味着客户端拿全了、
// 但那份完整性由网关垫着。合并会让运维看不出「重启会影响多少请求」。
func (h *Handler) observeVerdict(kind degrade.Provider, v degrade.Verdict) {
	degraded := make([]string, 0, len(v.Degraded))
	for _, d := range v.Degraded {
		degraded = append(degraded, string(d.Capability))
	}
	emulated := make([]string, 0, len(v.Emulated))
	for _, e := range v.Emulated {
		emulated = append(emulated, string(e.Capability))
	}
	h.deps.Metrics.ObserveVerdict(string(h.inbound), string(kind), degraded, emulated)
}

// verdictHeaders 把裁决结果编成响应头。
//
// 客户端有权知道它请求的能力被降了级，或者是由网关垫出来的——
// 这会影响它的重试与降级决策。
func verdictHeaders(v degrade.Verdict) map[string]string {
	h := map[string]string{}
	if s := v.Header(); s != "" {
		h[degrade.DegradationHeader] = s
	}
	if len(v.Emulated) > 0 {
		parts := make([]string, 0, len(v.Emulated))
		for _, e := range v.Emulated {
			parts = append(parts, string(e.Capability))
		}
		h[EmulationHeader] = joinComma(parts)
	}
	return h
}

// EmulationHeader 告知客户端哪些能力是由网关模拟提供的。
const EmulationHeader = "X-Omugw-Emulated"

func joinComma(s []string) string {
	out := ""
	for i, x := range s {
		if i > 0 {
			out += ", "
		}
		out += x
	}
	return out
}

// fail 把错误编成入站协议的线格式回给客户端。
//
// 已经发出过字节时只能记日志——响应头早写出去了，此时再写一个错误体只会
// 产出一段损坏的响应。流式路径已经在流内发过终止事件了。
func (h *Handler) fail(w *tracked, err error) {
	cerr := canonical.AsError(err)

	if w.wrote {
		h.deps.Log.Error("响应已开始后发生错误，无法改写状态码",
			"class", string(cerr.Class), "error", cerr.Message)
		return
	}

	status, body, headers := openaiwire.EncodeError(cerr)
	for k, v := range headers {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
