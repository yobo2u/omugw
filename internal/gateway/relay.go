package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/transport/httpx"
	"github.com/yobo2u/omugw/internal/transport/sse"
)

// tracked 包装 ResponseWriter，记录是否已向客户端发出过任何字节。
//
// 这个布尔值就是 failover 的禁区（原则 2.4）。它必须由**下游**写入来驱动，
// 而不是上游响应的到达——网关完全可以收到上游响应头、发现是 429、于是换一条
// 路重试，此时客户端什么都还没看到，重试是安全的。
type tracked struct {
	http.ResponseWriter
	wrote bool
}

func (t *tracked) Write(p []byte) (int, error) {
	t.wrote = true
	return t.ResponseWriter.Write(p)
}

func (t *tracked) WriteHeader(code int) {
	t.wrote = true
	t.ResponseWriter.WriteHeader(code)
}

func (t *tracked) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// relayJSON 转发非流式响应。usageFromJSON 按入站协议从响应体里抽取用量。
func relayJSON(w *tracked, resp *httpx.Response, extra map[string]string,
	usageFromJSON func(body []byte) canonical.Usage) (canonical.Usage, error) {
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return canonical.UnavailableUsage(),
			canonical.Wrapf(err, canonical.ClassUpstreamUnavailable, "读取上游响应失败")
	}

	usage := usageFromJSON(body)

	copyUpstreamHeaders(w.Header(), resp.Header)
	for k, v := range extra {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)

	return usage, nil
}

// relayStream 转发流式响应。
//
// 走事件级而非字节级：原则 2.4 要求首字节之后失败必须发一个终止错误事件收尾，
// 而纯 io.Copy 做不到；计费也需要从 response.completed 里取出 usage。
// 代价只是帧的空白格式被规范化——data 负载逐字保留，语义完全一致。
func relayStream(w *tracked, resp *httpx.Response, extra map[string]string,
	usageFromEvent func(ev sse.Event) (canonical.Usage, bool),
	encodeError func(e *canonical.Error) (int, []byte, map[string]string)) (canonical.Usage, error) {
	defer resp.Body.Close()

	copyUpstreamHeaders(w.Header(), resp.Header)
	for k, v := range extra {
		w.Header().Set(k, v)
	}

	sw, err := sse.NewWriter(w)
	if err != nil {
		return canonical.UnavailableUsage(),
			canonical.Wrapf(err, canonical.ClassInternal, "无法建立流式输出")
	}
	w.WriteHeader(resp.StatusCode)

	usage := canonical.UnavailableUsage()
	reader := sse.NewReader(resp.Body)

	for {
		ev, err := reader.Next()

		if ev.Data != "" || ev.Event != "" {
			if u, ok := usageFromEvent(ev); ok {
				usage = u
			}
			if werr := sw.Write(ev); werr != nil {
				// 客户端断开。不是上游的错，也不必再往下读。
				return usage, canonical.Wrapf(werr, canonical.ClassBadRequest,
					"客户端已断开")
			}
		}

		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) {
			return usage, nil
		}

		// 首字节之后上游断了。**不能重试**——客户端已经收到内容，重试会让它
		// 看到重复的字。发一个终止事件收尾，并把用量标记为不可知：
		// 上游不会再送 usage 了，任何非零数字都是编造的。错误负载按入站协议
		// 的线格式编码，客户端才读得懂。
		cerr := canonical.AsError(err)
		_, errBody, _ := encodeError(cerr)
		_ = sw.Write(sse.Event{Event: "error", Data: string(errBody)})
		return canonical.UnavailableUsage(), cerr
	}
}

// copyUpstreamHeaders 把上游的限流额度信息透给客户端。
//
// 这些头是客户端 SDK 退避算法的输入，属于协议契约的一部分。
// 但只透与限流有关的那些——上游的 Set-Cookie、Server 之类既无用又会泄露
// 上游的部署细节。
func copyUpstreamHeaders(dst, src http.Header) {
	for _, k := range []string{
		"X-Ratelimit-Limit-Requests",
		"X-Ratelimit-Remaining-Requests",
		"X-Ratelimit-Reset-Requests",
		"X-Ratelimit-Limit-Tokens",
		"X-Ratelimit-Remaining-Tokens",
		"X-Ratelimit-Reset-Tokens",
		"Retry-After",
	} {
		if v := src.Get(k); v != "" {
			dst.Set(k, v)
		}
	}
}

// usageEnvelope 是 Responses 的用量结构。
type usageEnvelope struct {
	Usage *struct {
		InputTokens       int64 `json:"input_tokens"`
		OutputTokens      int64 `json:"output_tokens"`
		InputTokenDetails *struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"input_tokens_details"`
		OutputTokenDetails *struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		} `json:"output_tokens_details"`
	} `json:"usage"`
}

// extractUsage 从非流式响应体里取出用量。
func extractUsage(body []byte) canonical.Usage {
	var env usageEnvelope
	if err := json.Unmarshal(body, &env); err != nil || env.Usage == nil {
		// 解不出来就是不可知。编一个 0 会让「没数据」和「真的是 0」混在一起，
		// 而计费要靠这个数。
		return canonical.UnavailableUsage()
	}
	return toCanonicalUsage(env)
}

// parseUsageEvent 从流式事件里取出用量。
//
// Responses 把最终用量放在 response.completed 事件的 response.usage 下。
func parseUsageEvent(ev sse.Event) (canonical.Usage, bool) {
	if ev.Event != "response.completed" || ev.Data == "" {
		return canonical.Usage{}, false
	}
	var wrapper struct {
		Response json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal([]byte(ev.Data), &wrapper); err != nil || len(wrapper.Response) == 0 {
		return canonical.Usage{}, false
	}
	var env usageEnvelope
	if err := json.Unmarshal(wrapper.Response, &env); err != nil || env.Usage == nil {
		return canonical.Usage{}, false
	}
	return toCanonicalUsage(env), true
}

func toCanonicalUsage(env usageEnvelope) canonical.Usage {
	u := canonical.Usage{
		// 数字直接来自上游响应，可用于计费。
		Fidelity:     canonical.FidelityAuthoritative,
		InputTokens:  env.Usage.InputTokens,
		OutputTokens: env.Usage.OutputTokens,
	}
	if d := env.Usage.InputTokenDetails; d != nil {
		// OpenAI 的 cached_tokens 等价于「命中缓存的读取」，不是写入。
		// 记成 write 会让缓存成本被高估数倍。
		u.CacheReadInputTokens = d.CachedTokens
	}
	if d := env.Usage.OutputTokenDetails; d != nil {
		u.ReasoningTokens = d.ReasoningTokens
	}
	return u
}

// chatUsageEnvelope 是 Chat Completions 的用量结构。
//
// 与 Responses 的口径不同：Chat 用 prompt_tokens / completion_tokens，
// 缓存与推理明细挂在各自的 *_details 下。两套字段名不能混用，否则计费口径错。
type chatUsageEnvelope struct {
	Usage *struct {
		PromptTokens        int64 `json:"prompt_tokens"`
		CompletionTokens    int64 `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails *struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
}

// extractChatUsage 从非流式 Chat 响应体里取出用量。
func extractChatUsage(body []byte) canonical.Usage {
	var env chatUsageEnvelope
	if err := json.Unmarshal(body, &env); err != nil || env.Usage == nil {
		// 解不出来就是不可知。编一个 0 会让「没数据」和「真的是 0」混在一起。
		return canonical.UnavailableUsage()
	}
	return toCanonicalChatUsage(env)
}

// parseChatUsageEvent 从流式事件里取出用量。
//
// Chat 的流式用量放在最后一个 chat.completion.chunk 的 usage 字段里
// （需客户端带 stream_options.include_usage），事件本身没有名字，只有 data。
// 中间分片的 usage 为 null，解出来 Usage 为空即跳过。
func parseChatUsageEvent(ev sse.Event) (canonical.Usage, bool) {
	if ev.Data == "" || ev.Data == "[DONE]" {
		return canonical.Usage{}, false
	}
	var env chatUsageEnvelope
	if err := json.Unmarshal([]byte(ev.Data), &env); err != nil || env.Usage == nil {
		return canonical.Usage{}, false
	}
	return toCanonicalChatUsage(env), true
}

func toCanonicalChatUsage(env chatUsageEnvelope) canonical.Usage {
	u := canonical.Usage{
		// 数字直接来自上游响应，可用于计费。
		Fidelity:     canonical.FidelityAuthoritative,
		InputTokens:  env.Usage.PromptTokens,
		OutputTokens: env.Usage.CompletionTokens,
	}
	if d := env.Usage.PromptTokensDetails; d != nil {
		// cached_tokens 是「命中缓存的读取」，不是写入。
		u.CacheReadInputTokens = d.CachedTokens
	}
	if d := env.Usage.CompletionTokensDetails; d != nil {
		u.ReasoningTokens = d.ReasoningTokens
	}
	return u
}

// dashScopeUsageEnvelope 是 DashScope Native 的用量结构。
//
// 口径是 input_tokens / output_tokens；与 Responses 同名但挂在顶层 usage 下。
// 流式时每一帧的 data 都携带累计用量，取最后一帧即全量。
type dashScopeUsageEnvelope struct {
	Usage *struct {
		InputTokens         int64 `json:"input_tokens"`
		OutputTokens        int64 `json:"output_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

// extractDashScopeUsage 从非流式 DashScope 响应体里取出用量。
func extractDashScopeUsage(body []byte) canonical.Usage {
	var env dashScopeUsageEnvelope
	if err := json.Unmarshal(body, &env); err != nil || env.Usage == nil {
		return canonical.UnavailableUsage()
	}
	return toCanonicalDashScopeUsage(env)
}

// parseDashScopeUsageEvent 从流式事件里取出用量。
//
// DashScope 的 SSE 帧是 event: result + data: 完整响应信封，用量逐帧累计，
// 故任何带 usage 的帧都可取，relay 保留最后一帧即全量。
func parseDashScopeUsageEvent(ev sse.Event) (canonical.Usage, bool) {
	if ev.Data == "" {
		return canonical.Usage{}, false
	}
	var env dashScopeUsageEnvelope
	if err := json.Unmarshal([]byte(ev.Data), &env); err != nil || env.Usage == nil {
		return canonical.Usage{}, false
	}
	return toCanonicalDashScopeUsage(env), true
}

func toCanonicalDashScopeUsage(env dashScopeUsageEnvelope) canonical.Usage {
	u := canonical.Usage{
		// 数字直接来自上游响应，可用于计费。
		Fidelity:     canonical.FidelityAuthoritative,
		InputTokens:  env.Usage.InputTokens,
		OutputTokens: env.Usage.OutputTokens,
	}
	if d := env.Usage.PromptTokensDetails; d != nil {
		// cached_tokens 是「命中缓存的读取」，不是写入。
		u.CacheReadInputTokens = d.CachedTokens
	}
	return u
}
