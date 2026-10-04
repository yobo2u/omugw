package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/credential"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/obs"
	"github.com/yobo2u/omugw/internal/provider"
	"github.com/yobo2u/omugw/internal/router"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

type WSDeps struct {
	Matrix          *degrade.Matrix
	Router          *router.Router
	Auth            *Authenticator
	Metrics         *obs.Metrics
	Log             *slog.Logger
	Pools           map[string]*credential.Pool
	Providers       map[string]provider.StreamProvider
	Timeouts        config.Timeouts
	Limits          config.WebSocket
	Budget          *ws.BufferBudget
	Registry        *wsRegistry
	InferenceTarget *router.Target
}

// WSHandler 单独持有升级承诺，不借 HTTP tracked.wrote 猜测 Hijack 后的状态。
type WSHandler struct {
	d       WSDeps
	profile wsProfile
}

func NewDashScopeRealtimeHandler(d WSDeps) *WSHandler {
	return &WSHandler{d: d, profile: dashScopeRealtimeProfile()}
}

func (h *WSHandler) inbound() degrade.Inbound { return h.profile.inbound }
func (*WSHandler) method() string             { return http.MethodGet }

func (h *WSHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	outbound, committed, err := h.serve(w, r, start)
	if err != nil && !committed {
		h.writeWSError(w, err)
	}
	outcome := wsOutcome(err)
	if m := h.d.Metrics; m != nil {
		m.Requests.WithLabelValues(string(h.inbound().Protocol), outbound, outcome).Inc()
		m.Duration.WithLabelValues(string(h.inbound().Protocol), outbound).Observe(time.Since(start).Seconds())
	}
	if h.d.Log != nil {
		// 不记录请求 URL、模型、网络原错或 close reason，固定标签已足够定位阶段。
		h.d.Log.Debug("WebSocket 请求结束", "inbound", string(h.inbound().Protocol), "outbound", outbound, "outcome", outcome, "committed", committed)
	}
}

func (h *WSHandler) serve(w http.ResponseWriter, r *http.Request, start time.Time) (string, bool, error) {
	model, err := h.preflight(w, r)
	if err != nil {
		return "", false, err
	}
	session, err := h.d.Registry.Register(r.Context())
	if err != nil {
		return "", false, err
	}
	// dispatch 的预读与 relay 的全部 worker 均已 join 才会抵达 Done。
	defer session.Done()
	handshake, cancel := context.WithDeadline(session.Context(), start.Add(h.d.Timeouts.FirstByte))
	defer cancel()
	ready, outbound, err := h.connect(handshake, session, model, r.Header)
	if err != nil {
		return outbound, false, err
	}
	var result error
	defer func() { settleWSLease(ready.lease, result) }()
	if err := wsHandshakeContextError(handshake); err != nil {
		if ready.initial != nil {
			ready.initial.Release()
		}
		result = err
		session.shutdown.termination.report(wsRelayResult{err: err})
		_ = session.shutdown.termination.close()
		return outbound, false, err
	}
	deadline, _ := handshake.Deadline()
	// 从进入 Accept 起即 committed；失败也不能再写 HTTP、更不能再拨上游。
	committed := true
	downstream, err := acceptWS(handshake, w, r, ws.AcceptOptions{
		MaxPayload: h.d.Limits.MaxMessageBytes, Budget: h.d.Budget, Idle: h.d.Timeouts.Idle,
		WriteTimeout: min(h.d.Timeouts.Connect, h.d.Timeouts.Idle), HandshakeDeadline: deadline,
	})
	// 取消可与 Accept 成功交付竞争；拿到的每一段都先交给 registry，不能遗失。
	if downstream != nil && !session.Attach(downstream) {
		if ready.initial != nil {
			ready.initial.Release()
		}
		result = wsSessionError(session)
		return outbound, committed, result
	}
	if err != nil {
		if ready.initial != nil {
			ready.initial.Release()
		}
		result = errWSRelayDownstream
		session.shutdown.termination.report(wsRelayResult{err: result})
		_ = session.shutdown.termination.close()
		return outbound, committed, result
	}
	if h.d.Metrics != nil {
		h.d.Metrics.FirstByte.WithLabelValues(string(h.inbound().Protocol), outbound, "true").Observe(time.Since(start).Seconds())
	}
	// 握手 ctx 不得套住业务会话；使用保留 registry 私有 Value 的 session.Context。
	cancel()
	options := wsRelayOptions{ClassifyClose: h.profile.classifyClose,
		Timeouts: h.d.Timeouts, Budget: h.d.Budget, MaxMessageBytes: h.d.Limits.MaxMessageBytes}
	if h.inbound().Protocol == degrade.ProtoDashScopeInference {
		// connect 已验证这个值副本，不能从 run model 重新选择连接或凭据。
		binding, _ := newWSInferenceBinding(ready.target)
		options.Policy = newWSInferencePolicy(binding, h.d.Router, h.d.Matrix, h.d.Metrics, h.d.Timeouts, nil)
	} else {
		options.Observer = h.profile.newObserver(h.d.Metrics, string(h.inbound().Protocol), outbound)
	}
	result = relayWS(session.Context(), downstream, ready.conn, ready.initial, options)
	return outbound, committed, result
}

func (h *WSHandler) preflight(w http.ResponseWriter, r *http.Request) (string, error) {
	if _, err := h.d.Auth.AuthenticateUnique(r); err != nil {
		return "", err
	}
	if err := h.profile.validateHeaders(r.Header); err != nil {
		return "", err
	}
	if ws.ValidateUpgrade(r) != nil || r.URL.Path != string(h.inbound().Endpoint) {
		return "", canonical.Newf(canonical.ClassBadRequest, "无效的 WebSocket 升级请求")
	}
	if _, ok := w.(http.Hijacker); !ok {
		return "", canonical.Newf(canonical.ClassInternal, "WebSocket 升级不可用")
	}
	// 只有固定 Inference 坐标允许无首事件升级；枚举不是可替换的就绪插件。
	if h.profile.readyMode == wsReadyHandshake {
		if h.inbound() != (degrade.Inbound{Protocol: degrade.ProtoDashScopeInference, Endpoint: degrade.EndpointDashScopeInference}) || h.profile.outbound != degrade.ProviderDashScopeWSInference {
			return "", canonical.Newf(canonical.ClassInternal, "WebSocket 就绪契约错配")
		}
		if r.URL.RawQuery != "" || r.URL.ForceQuery {
			return "", canonical.Newf(canonical.ClassBadRequest, "Inference 握手不接受 query")
		}
		return "", nil
	}
	if h.profile.readyMode != wsReadyEvent || h.inbound().Protocol == degrade.ProtoDashScopeInference || h.profile.checkReady == nil || h.profile.newObserver == nil {
		return "", canonical.Newf(canonical.ClassInternal, "WebSocket 就绪契约错配")
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	models := query["model"]
	if err != nil || len(query) != 1 || len(models) != 1 || strings.TrimSpace(models[0]) == "" || strings.ContainsFunc(models[0], unicode.IsControl) {
		return "", canonical.Newf(canonical.ClassBadRequest, "Realtime 只接受唯一非空 model 参数")
	}
	return models[0], nil
}

// 保留本地 429/503；不能让 canonical.AsError 把 registry 状态折成 500。
func (h *WSHandler) writeWSError(w http.ResponseWriter, err error) {
	e := safeWSError(err)
	status, body, headers := h.profile.encodeError(e)
	var local *wsRegistryError
	if errors.As(err, &local) {
		status = local.HTTPStatus()
	}
	if errors.Is(err, errWSShutdown) {
		status = http.StatusServiceUnavailable
	}
	for k, v := range headers {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func safeWSError(err error) *canonical.Error {
	e := canonical.Newf(canonical.ClassInternal, "Realtime 请求未完成")
	var known *canonical.Error
	if errors.As(err, &known) {
		e.Class, e.Retryable = known.Class, known.Retryable
		e.RetryAfter, e.RateLimit = known.RetryAfter, known.RateLimit
	}
	switch {
	case errors.Is(err, errWSRegistryFull):
		e.Class = canonical.ClassRateLimit
	case errors.Is(err, errWSRegistrySealed), errors.Is(err, errWSShutdown), errors.Is(err, context.DeadlineExceeded):
		e.Class = canonical.ClassUpstreamUnavailable
	case errors.Is(err, context.Canceled):
		e.Class = canonical.ClassBadRequest
	}
	return e
}

func wsOutcome(err error) string {
	if err == nil {
		return "ok"
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, errWSShutdown) {
		return "cancelled"
	}
	if errors.Is(err, errWSRegistryFull) || errors.Is(err, errWSRegistrySealed) {
		return "capacity"
	}
	switch c := safeWSError(err).Class; c {
	case canonical.ClassAuth, canonical.ClassRateLimit, canonical.ClassQuota, canonical.ClassContextLength,
		canonical.ClassContentFilter, canonical.ClassUpstreamUnavailable, canonical.ClassBadRequest,
		canonical.ClassUnsupported, canonical.ClassNotImplemented:
		return string(c)
	default:
		return "internal"
	}
}
