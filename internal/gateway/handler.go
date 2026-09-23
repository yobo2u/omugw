// Package gateway 把各层串成可服务的 HTTP 处理器。
//
// 这里是整条链路唯一汇合的地方：鉴权 → 解码 → 路由 → 降级矩阵裁决 →
// 凭据池 → 出站适配器 → 回写。每一层都在自己的包里被单独测过，这里只负责
// 把它们按正确的顺序接起来，以及一件别处做不了的事——**跟踪下游首字节**，
// 因为只有它知道客户端到底看到了什么。
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/convstore"
	"github.com/yobo2u/omugw/internal/credential"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/obs"
	"github.com/yobo2u/omugw/internal/protocol/dashscopenative"
	"github.com/yobo2u/omugw/internal/protocol/dashscopewire"
	"github.com/yobo2u/omugw/internal/protocol/openaichat"
	"github.com/yobo2u/omugw/internal/protocol/openairesponses"
	"github.com/yobo2u/omugw/internal/protocol/openaiwire"
	"github.com/yobo2u/omugw/internal/provider"
	"github.com/yobo2u/omugw/internal/router"
	"github.com/yobo2u/omugw/internal/transport/httpx"
	"github.com/yobo2u/omugw/internal/transport/sse"
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

	// RequestReadTimeout 限制客户端交付完整请求体的时间。只在 Handler 层设限制
	// 才能覆盖测试服务器与嵌入式使用；生产 http.Server 还会设置同值作为兜底。
	RequestReadTimeout time.Duration
	ConversationStore  convstore.Store

	// Pools 按凭据池名索引。
	Pools map[string]*credential.Pool

	// Providers 按 endpoint 名索引出站适配器。
	Providers map[string]provider.Provider
}

// decodedRequest 是入站解码后的统一视图：Canonical 请求 + 能力清单 + 内联负载大小。
//
// 各入站协议的解码器各自产出它，好让 serve/dispatch/relay 这条主链路对具体协议
// 无感——新增一条入站协议不必改主链路，只需再给一个 inbound。
type decodedRequest struct {
	Request            canonical.Request
	caps               []canonical.Capability
	InlineBytes        int64
	ReplayInlineBytes  int64
	PreviousResponseID string
	WantsStore         bool

	// RequiresConversationStore 表示调用方显式依赖服务端会话，缺了它请求就
	// 不成立。WantsStore 只是协议默认值，够不上这个判据。
	RequiresConversationStore bool
}

// Capabilities 报告这次请求用到的能力，供降级矩阵裁决。
func (d *decodedRequest) Capabilities() []canonical.Capability { return d.caps }

// inbound 把一条入站协议在网关数据面上的全部协议相关行为收拢到一处：解码请求、
// 抽取用量、以及同源直通时对应的上游端点路径。
type inbound struct {
	protocol degrade.Protocol

	// decode 把原始请求体解成统一视图。带上 *http.Request 是因为有的协议把
	// 关键信号放在头上而非体里——DashScope Native 用 X-DashScope-SSE 头声明
	// 流式，请求体里没有等价字段，只看 body 判不出是否流式。
	decode func(r *http.Request, raw []byte) (*decodedRequest, error)

	// usageJSON 从非流式响应体抽取用量；usageEvent 从流式事件抽取用量。
	// 两者形状因协议而异——Responses 是 input_tokens + response.completed 事件，
	// Chat 是 prompt_tokens + chat.completion.chunk，DashScope Native 是
	// input_tokens 且每一帧都携带——不能共用一套解析。
	usageJSON  func(body []byte) canonical.Usage
	usageEvent func(ev sse.Event) (canonical.Usage, bool)

	// streamTerminal 判定协议级正常结束。nil 表示该协议暂未声明判据；一旦声明，
	// HTTP EOF 之前没见到终止事件就必须按上游中断处理，不能把半截回答记成成功。
	streamTerminal func(ev sse.Event) bool

	// upstreamPath 返回同源直通时打到的上游端点。网关对上游说的是客户端那套
	// 线格式，所以路径由入站协议决定，而不是由出站 Provider 决定。多数协议是
	// 固定值；DashScope Native 一个入站协议对应多个上游端点（文本 / 图像 /
	// 语音…），路径随请求走，直接取请求路径。
	upstreamPath func(r *http.Request) string

	// encodeError 把统一错误编成入站协议的线格式回给客户端。OpenAI 系是嵌套
	// {"error":...}，DashScope Native 是扁平 {code,message,request_id}——客户端
	// 按自己说的协议解析错误，给错信封它就读不懂。
	encodeError func(e *canonical.Error) (status int, body []byte, headers map[string]string)
}

// Handler 处理某一条入站协议的请求。
type Handler struct {
	deps Deps
	in   inbound
}

// NewResponsesHandler 构造 /v1/responses 的处理器。
func NewResponsesHandler(d Deps) *Handler { return newHandler(d, responsesInbound()) }

// NewChatHandler 构造 /v1/chat/completions 的处理器。
func NewChatHandler(d Deps) *Handler { return newHandler(d, chatInbound()) }

// NewDashScopeNativeHandler 构造 DashScope Native 端点的处理器。
func NewDashScopeNativeHandler(d Deps) *Handler { return newHandler(d, dashScopeNativeInbound()) }

func newHandler(d Deps, in inbound) *Handler {
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Handler{deps: d, in: in}
}

// responsesInbound 把 Responses 协议接入主链路。
func responsesInbound() inbound {
	return inbound{
		protocol: degrade.ProtoOpenAIResponses,
		decode: func(_ *http.Request, raw []byte) (*decodedRequest, error) {
			d, err := openairesponses.Decode(raw)
			if err != nil {
				return nil, err
			}
			return &decodedRequest{
				Request: d.Request, caps: d.Capabilities(), InlineBytes: d.InlineBytes,
				ReplayInlineBytes:         d.ReplayInlineBytes,
				PreviousResponseID:        d.PreviousResponseID,
				WantsStore:                d.WantsStore,
				RequiresConversationStore: d.RequiresStatefulConversation,
			}, nil
		},
		usageJSON:  extractUsage,
		usageEvent: parseUsageEvent,
		streamTerminal: func(ev sse.Event) bool {
			switch ev.Event {
			case "response.completed", "response.incomplete", "response.failed":
				return true
			default:
				return false
			}
		},
		upstreamPath: func(*http.Request) string { return string(degrade.EndpointOpenAIResponses) },
		encodeError:  openaiwire.EncodeError,
	}
}

// chatInbound 把 Chat Completions 协议接入主链路。
func chatInbound() inbound {
	return inbound{
		protocol: degrade.ProtoOpenAIChat,
		decode: func(_ *http.Request, raw []byte) (*decodedRequest, error) {
			d, err := openaichat.Decode(raw)
			if err != nil {
				return nil, err
			}
			return &decodedRequest{Request: d.Request, caps: d.Capabilities(), InlineBytes: d.InlineBytes}, nil
		},
		usageJSON:  extractChatUsage,
		usageEvent: parseChatUsageEvent,
		streamTerminal: func(ev sse.Event) bool {
			return ev.Data == "[DONE]"
		},
		upstreamPath: func(*http.Request) string { return string(degrade.EndpointOpenAIChat) },
		encodeError:  openaiwire.EncodeError,
	}
}

// dashScopeNativeInbound 把 DashScope Native 协议接入主链路。
//
// 一个入站协议对应上游一堆端点（文本生成 / 多模态 / embedding / 图像 / 语音…），
// 所以这里不写死路径，直通时按请求路径原样打到上游同名端点。本期只保证文本
// 生成的解码与用量抽取是准确的；其余端点字节照样透传，解码只做尽力而为的
// 能力识别。
func dashScopeNativeInbound() inbound {
	return inbound{
		protocol: degrade.ProtoDashScopeNative,
		decode: func(r *http.Request, raw []byte) (*decodedRequest, error) {
			d, err := dashscopenative.Decode(raw)
			if err != nil {
				return nil, err
			}
			// 流式由 X-DashScope-SSE 头声明，体里没有等价字段。
			if r.Header.Get(dashscopenative.SSEHeader) == "enable" {
				d.Request.Stream = true
			}
			return &decodedRequest{Request: d.Request, caps: d.Capabilities(), InlineBytes: d.InlineBytes}, nil
		},
		usageJSON:  extractDashScopeUsage,
		usageEvent: parseDashScopeUsageEvent,
		streamTerminal: func(ev sse.Event) bool {
			if ev.Event != "" && ev.Event != "result" {
				return false
			}
			var frame struct {
				Output struct {
					Choices []struct {
						FinishReason *string `json:"finish_reason"`
					} `json:"choices"`
				} `json:"output"`
			}
			if err := json.Unmarshal([]byte(ev.Data), &frame); err != nil || len(frame.Output.Choices) == 0 {
				return false
			}
			for _, choice := range frame.Output.Choices {
				if choice.FinishReason == nil || *choice.FinishReason == "" || *choice.FinishReason == "null" {
					return false
				}
			}
			return true
		},
		upstreamPath: func(r *http.Request) string { return r.URL.Path },
		encodeError:  dashscopewire.EncodeError,
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.deps.RequestReadTimeout > 0 {
		// 慢速请求体发生在业务 handler 已经开始之后，ReadHeaderTimeout 管不到它。
		// ResponseController 在真实 net/http 连接上设置读取期限；不支持该能力的
		// 纯内存 ResponseWriter 会返回错误，此时仍由调用方的请求上下文兜底。
		_ = http.NewResponseController(w).SetReadDeadline(
			time.Now().Add(h.deps.RequestReadTimeout),
		)
	}
	tw := &tracked{ResponseWriter: w}
	start := h.deps.Now()

	outcome, outbound, err := h.serve(tw, r)

	if err != nil {
		h.fail(tw, err)
	}

	h.deps.Metrics.Requests.WithLabelValues(string(h.in.protocol), outbound, outcome).Inc()
	h.deps.Metrics.Duration.WithLabelValues(string(h.in.protocol), outbound).
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

	decoded, err := h.in.decode(r, raw)
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

	// 入站坐标只算一次，矩阵与 Provider 共用同一份：分别算两次，日后任一侧的
	// 取法改了，裁决依据与实际打出去的门就会悄悄分家——那种错在响应里看不出来。
	inbound := degrade.Inbound{
		Protocol: h.in.protocol,
		Endpoint: degrade.Endpoint(h.in.upstreamPath(r)),
	}

	// 路由给出候选，矩阵按入站坐标（协议 + 门）与能力裁决。两者分工，不互相包含。
	kind, verdict, err := h.deps.Matrix.BestOutbound(
		inbound, router.Kinds(targets), decoded.Capabilities())
	if err != nil {
		if canonical.AsError(err).Class == canonical.ClassNotImplemented {
			h.deps.Metrics.ObserveNotImplemented(string(h.in.protocol), "planned")
			return "not_implemented", outbound, err
		}
		return "unsupported", outbound, err
	}
	outbound = string(kind)

	h.observeVerdict(kind, verdict)

	conversation, err := h.prepareConversation(r, caller, decoded, raw)
	if err != nil {
		return "bad_request", outbound, err
	}
	if conversation != nil {
		raw = conversation.raw
	}

	candidates := router.OfKind(targets, kind)
	if kind == degrade.ProviderDashScopeNative {
		candidates = filterNativeTargets(candidates, decoded.Capabilities())
		if len(candidates) == 0 {
			return "unsupported", outbound, canonical.Newf(
				canonical.ClassUnsupported,
				"模型路由没有可承载该媒体的 DashScope Native 门",
			)
		}
	}

	return h.dispatch(w, r, dispatchInput{
		caller:       caller,
		raw:          raw,
		decoded:      decoded,
		targets:      candidates,
		kind:         kind,
		inbound:      inbound,
		headers:      verdictHeaders(verdict),
		conversation: conversation,
	})
}

type conversationRequest struct {
	raw         []byte
	prevID      string
	responseID  string
	owner       string
	store       bool
	inlineBytes int64
	current     []canonical.Message
	currentWire []json.RawMessage
	stored      bool
}

type dispatchInput struct {
	caller  Caller
	raw     []byte
	decoded *decodedRequest
	targets []router.Target
	kind    degrade.Provider

	// inbound 是 serve 裁决时用的那一份入站坐标，原样交给 Provider。
	inbound degrade.Inbound

	headers      map[string]string
	conversation *conversationRequest
}

// dispatch 在候选上游之间做 failover。
//
// **只在下游首字节之前重试**（原则 2.4）。一旦向客户端发出过任何字节，
// 无论错误多么可重试都必须就此打住——重试会让客户端收到重复内容。
func (h *Handler) dispatch(w *tracked, r *http.Request, in dispatchInput) (string, string, error) {
	var lastSetupErr error
	var lastCallErr error

	for _, target := range in.targets {
		pool, ok := h.deps.Pools[target.CredentialPool]
		if !ok {
			lastSetupErr = canonical.Newf(canonical.ClassInternal,
				"路由目标 %s 引用了不存在的凭据池 %q", target.Endpoint, target.CredentialPool)
			continue
		}
		prov, ok := h.deps.Providers[target.Endpoint]
		if !ok {
			lastSetupErr = canonical.Newf(canonical.ClassInternal,
				"没有为 endpoint %q 注册出站适配器", target.Endpoint)
			continue
		}

		tried := map[string]bool{}
		for {
			lease, err := pool.Acquire(tried)
			if err != nil {
				lastSetupErr = err
				break // 这个上游的凭据用尽，换下一个上游
			}
			tried[lease.Credential.ID] = true

			// DashScope 专用 usage 回调，**只为 Native 注入**，其他 Provider 一律 nil。
			//
			// 三个变量都声明在这一次具体尝试之内，不提到循环外：提出去之后，一次
			// 失败尝试回调过的用量会活到下一次尝试——那次尝试可能换了凭据、甚至
			// 换了上游，它的账上却挂着上一条链路的数字。两条路都返回 200，多出来
			// 的那笔账在响应里看不出任何痕迹。
			//
			// 回调与 relay 同 goroutine 同步执行，不需要锁。
			var dsUsage canonical.Usage
			var hasDSUsage bool
			var onDSUsage func(canonical.Usage)
			if in.kind == degrade.ProviderDashScopeNative {
				onDSUsage = func(u canonical.Usage) { dsUsage, hasDSUsage = u, true }
			}

			resp, err := prov.Call(r.Context(), provider.Request{
				Target:           target,
				Credential:       lease.Credential,
				Raw:              in.raw,
				Canonical:        &in.decoded.Request,
				Stream:           in.decoded.Request.Stream,
				Inbound:          in.inbound,
				Header:           r.Header,
				OnDashScopeUsage: onDSUsage,
			})
			if err != nil {
				lease.Fail(err)
				cerr := canonical.AsError(err)
				h.deps.Metrics.ObserveError(string(in.kind), cerr)
				lastCallErr = err

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

			// Provider 返回响应只说明上游响应头到了。真正的 failover 边界仍由
			// tracked.wrote 决定；非流式响应体可能在任何下游字节写出前就截断。
			h.deps.Metrics.FirstByte.WithLabelValues(
				string(h.in.protocol), string(in.kind), "true",
			).Observe(resp.Latency.Seconds())

			usage, rerr := h.relay(r.Context(), w, resp, in)

			// 用量优先级：relay 给出的结果永远优先，回调只在它交白卷时补位。
			//
			// 两个前提缺一不可。**relay 无错误返回**：流中断时 usage 已被 relay
			// 刻意抹成不可知——上游不会再送 usage，回调里躺着的是中断前那半截
			// 数字，拿它记账等于把一次残缺的调用按完整的收费。**relay 未取得
			// 用量**：Chat 响应体或 include_usage chunk 里的数字是按入站协议解出
			// 来的权威值，用回调覆盖它就是让出站适配器改写记账口径。
			//
			// 补位这一档不是可有可无的：客户端没要 include_usage 时，转换后的
			// Chat 流不带 usage chunk，而 Native 每帧都携带累计用量——没有回调，
			// 这次调用在账上就是免费的。
			if rerr == nil && usage.Fidelity == canonical.FidelityUnavailable && hasDSUsage {
				usage = dsUsage
			}
			h.deps.Metrics.ObserveUsage(string(in.kind), usage)

			if rerr != nil {
				cerr := canonical.AsError(rerr)
				if !w.wrote {
					lease.Fail(rerr)
					h.deps.Metrics.ObserveError(string(in.kind), cerr)
					lastCallErr = rerr
					if cerr.Retryable {
						continue
					}
					break
				}

				lease.Fail(rerr)
				h.deps.Metrics.StreamAborted.WithLabelValues(
					string(h.in.protocol), string(in.kind), string(cerr.Class)).Inc()
				h.deps.Log.Warn("响应转发中断",
					"endpoint", target.Endpoint,
					"class", string(cerr.Class),
					"caller", in.caller.ID,
				)
				// 已经开始回写，fail 会识别出这一点并只记日志。
				return "stream_aborted", string(in.kind), rerr
			}
			lease.Succeed()
			return "ok", string(in.kind), nil
		}
	}

	// 池与装配错误只能解释为何没有调用；只要 provider 真正返回过错误，客户端
	// 就应看到最后一次真实调用的原因，而不是后续候选的循环终止条件。
	if lastCallErr != nil {
		return "upstream_error", string(in.kind), lastCallErr
	}
	if lastSetupErr == nil {
		lastSetupErr = canonical.Newf(canonical.ClassUpstreamUnavailable, "没有可用的上游")
	}
	return "upstream_error", string(in.kind), lastSetupErr
}

// relay 按流式与否选择转发方式。
func (h *Handler) relay(ctx context.Context, w *tracked, resp *httpx.Response, in dispatchInput) (canonical.Usage, error) {
	var jsonTransform func([]byte) ([]byte, error)
	var streamTransform func(sse.Event) (sse.Event, error)
	if in.conversation != nil {
		jsonTransform = func(body []byte) ([]byte, error) {
			patched, output, err := openairesponses.RewriteStoredResponse(
				body, in.conversation.responseID, in.conversation.prevID,
				in.conversation.store)
			if err != nil {
				return nil, err
			}
			if in.conversation.store {
				if err := h.storeConversation(ctx, in.conversation, output,
					in.decoded.Request.Model); err != nil {
					return nil, err
				}
			}
			return patched, nil
		}
		streamTransform = func(ev sse.Event) (sse.Event, error) {
			patched, output, terminal, err := openairesponses.RewriteStoredStreamEvent(
				ev, in.conversation.responseID, in.conversation.prevID,
				in.conversation.store)
			if err != nil {
				return ev, err
			}
			if terminal && in.conversation.store {
				if err := h.storeConversation(ctx, in.conversation, output,
					in.decoded.Request.Model); err != nil {
					return ev, err
				}
			}
			return patched, nil
		}
	}
	if in.decoded.Request.Stream {
		return relayStream(w, resp, in.headers, h.in.usageEvent,
			h.in.streamTerminal, streamTransform, h.in.encodeError)
	}
	return relayJSON(w, resp, in.headers, h.in.usageJSON, jsonTransform)
}

func (h *Handler) prepareConversation(r *http.Request, caller Caller,
	decoded *decodedRequest, raw []byte) (*conversationRequest, error) {
	if decoded.PreviousResponseID == "" && !decoded.WantsStore {
		return nil, nil
	}
	if h.deps.ConversationStore == nil {
		// 没装配存储时，只有显式依赖会话的请求才算失败。省略 store 的请求
		// 走协议默认值，本来就没要求网关保管——为它报错等于把一个能正常
		// 完成的普通请求变成故障。
		if decoded.RequiresConversationStore {
			return nil, canonical.Newf(canonical.ClassInternal,
				"convstore 已声明可用但没有装配存储实例")
		}
		return nil, nil
	}

	current := append([]canonical.Message(nil), decoded.Request.Messages...)
	currentWire, err := openairesponses.ConversationInputItems(raw)
	if err != nil {
		return nil, err
	}
	var (
		history       []canonical.Message
		historyWire   []json.RawMessage
		historyInline int64
		turnCount     int
	)
	if decoded.PreviousResponseID != "" {
		turns, err := h.deps.ConversationStore.TurnsOwned(
			r.Context(), decoded.PreviousResponseID, caller.ID)
		if err != nil {
			return nil, conversationError(err)
		}
		turnCount = len(turns)
		if len(turns) == 0 {
			return nil, canonical.Newf(canonical.ClassInternal, "convstore 返回了空会话链")
		}
		if turns[len(turns)-1].Model != decoded.Request.Model {
			return nil, canonical.Newf(canonical.ClassBadRequest,
				"previous_response_id 属于模型 %q，不能用模型 %q 继续",
				turns[len(turns)-1].Model, decoded.Request.Model)
		}
		for _, turn := range turns {
			history = append(history, turn.Messages...)
			historyInline += turn.InlineBytes
			var items []json.RawMessage
			if len(turn.Opaque) > 0 {
				if err := json.Unmarshal(turn.Opaque, &items); err != nil {
					return nil, canonical.Wrapf(err, canonical.ClassInternal,
						"convstore 中的 Responses 原始历史损坏")
				}
			} else {
				items, err = openairesponses.EncodeConversationHistory(turn.Messages)
				if err != nil {
					return nil, err
				}
			}
			historyWire = append(historyWire, items...)
		}
	}

	limits := h.deps.ConversationStore.Limits()
	if decoded.WantsStore && turnCount >= limits.MaxChainDepth {
		return nil, conversationError(convstore.ErrChainTooLong)
	}
	if decoded.WantsStore && len(history)+len(current) >= limits.MaxMessages {
		return nil, conversationError(convstore.ErrTooLarge)
	}
	if historyInline+decoded.InlineBytes > h.deps.Limits.MaxInlineBytes {
		return nil, canonical.Newf(canonical.ClassBadRequest,
			"会话累计内联多模态负载 %d 字节，超过上限 %d",
			historyInline+decoded.InlineBytes, h.deps.Limits.MaxInlineBytes)
	}

	patched, err := openairesponses.ExpandConversationRequest(raw, historyWire)
	if err != nil {
		return nil, err
	}
	if int64(len(patched)) > h.deps.Limits.MaxRequestBytes {
		return nil, canonical.Newf(canonical.ClassBadRequest,
			"展开会话历史后的请求体超过上限 %d 字节", h.deps.Limits.MaxRequestBytes)
	}
	responseID, err := convstore.NewResponseID()
	if err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassInternal, "生成本地 response.id 失败")
	}
	decoded.Request.Messages = append(history, current...)
	return &conversationRequest{
		raw: patched, prevID: decoded.PreviousResponseID,
		responseID: responseID, owner: caller.ID, store: decoded.WantsStore,
		inlineBytes: decoded.ReplayInlineBytes, current: current, currentWire: currentWire,
	}, nil
}

func (h *Handler) storeConversation(ctx context.Context, conversation *conversationRequest,
	output openairesponses.StoredOutput, model string) error {
	if !conversation.store || conversation.stored {
		return nil
	}
	messages := make([]canonical.Message, 0, len(conversation.current)+len(output.Messages))
	messages = append(messages, conversation.current...)
	messages = append(messages, output.Messages...)
	items := make([]json.RawMessage, 0, len(conversation.currentWire)+len(output.Items))
	items = append(items, conversation.currentWire...)
	items = append(items, output.Items...)
	opaque, err := json.Marshal(items)
	if err != nil {
		return canonical.Wrapf(err, canonical.ClassInternal, "序列化 Responses 会话轮次失败")
	}
	if err := h.deps.ConversationStore.AppendWithID(ctx, convstore.Turn{
		ID: conversation.responseID, PrevID: conversation.prevID,
		Owner: conversation.owner, Messages: messages, Opaque: opaque,
		InlineBytes: conversation.inlineBytes + output.InlineBytes, Model: model,
	}); err != nil {
		return conversationError(err)
	}
	conversation.stored = true
	return nil
}

func conversationError(err error) error {
	if errors.Is(err, convstore.ErrNotFound) {
		return canonical.Wrapf(err, canonical.ClassBadRequest,
			"previous_response_id 不存在或已过期")
	}
	if errors.Is(err, convstore.ErrChainTooLong) || errors.Is(err, convstore.ErrTooLarge) {
		return canonical.Wrapf(err, canonical.ClassBadRequest, "会话历史超过资源上限")
	}
	return canonical.Wrapf(err, canonical.ClassInternal, "会话存储失败")
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
	h.deps.Metrics.ObserveVerdict(string(h.in.protocol), string(kind), degraded, emulated)
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

	status, body, headers := h.in.encodeError(cerr)
	for k, v := range headers {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
