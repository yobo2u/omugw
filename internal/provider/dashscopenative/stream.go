package dashscopenative

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/yobo2u/omugw/internal/canonical"
	nativewire "github.com/yobo2u/omugw/internal/protocol/dashscopenative"
	"github.com/yobo2u/omugw/internal/protocol/dashscopewire"
	"github.com/yobo2u/omugw/internal/protocol/openaichat"
	"github.com/yobo2u/omugw/internal/provider"
	"github.com/yobo2u/omugw/internal/transport/httpx"
	"github.com/yobo2u/omugw/internal/transport/sse"
)

// nativeResultEvent 是 Native 流唯一的事件名。别的具名事件一律 fail-closed：
// 上游用它表达的多半是异常，当成正常帧解码只会把故障静默吞掉。
const nativeResultEvent = "result"

// translateStream 在 Call 内建立整条流式转换：出站请求、首帧预读、转换器装配。
//
// 首帧必须在 Call 之内读到并解码，是刻意的：此时下游一个字节都还没收到，
// 失败仍是一个可 failover 的 *canonical.Error。放到 Read 里才发现首帧是错误帧，
// 客户端已经拿到 200 与流头，换凭据重试的机会就永远没有了（原则 2.4）。
func (p *Provider) translateStream(ctx context.Context, req provider.Request, proj *openaichat.Projection) (*httpx.Response, error) {
	door := nativewire.Door(req.Target.NativeEndpoint)
	if door.Path() == "" {
		return nil, canonical.Newf(canonical.ClassInternal, "未知的 DashScope Native 门 %q", req.Target.NativeEndpoint)
	}

	extra := nativewire.ChatSampling{
		N:                   proj.N,
		PresencePenalty:     proj.PresencePenalty,
		Logprobs:            proj.Logprobs,
		TopLogprobs:         proj.TopLogprobs,
		ParallelToolCalls:   proj.ParallelToolCalls,
		MaxTokens:           proj.MaxTokens,
		MaxCompletionTokens: proj.MaxCompletionTokens,
		WebSearch:           proj.WebSearch,
		// 流式必须显式开增量：不开的话上游每帧回全量文本，按 delta 逐帧拼接
		// 会把内容重复放大成 O(n²)，而请求照样 200。
		IncrementalOutput: true,
	}

	body, err := nativewire.EncodeRequest(req.Canonical, extra, door, req.Target.UpstreamModel)
	if err != nil {
		return nil, err
	}

	url := strings.TrimSuffix(req.Target.BaseURL, "/") + door.Path()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassInternal, "构造 HTTP 请求失败")
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Authorization", "Bearer "+req.Credential.Secret)
	for _, h := range semanticHeaders {
		if v := req.Header.Get(h); v != "" {
			httpReq.Header.Set(h, v)
		}
	}
	// Native 把「是否流式」放在头上而非请求体里。丢了它上游按非流式返回，
	// 整条流的语义变掉，而这里等到的会是一个永远等不到 SSE 帧的响应。
	httpReq.Header.Set(nativewire.SSEHeader, "enable")

	// 传输层错误原样上抛：httpx.Client.Do 已经把连接、首字节、整体三种超时
	// 分别归好类了，再包一层会把「客户端主动断开」也改写成上游故障。
	httpResp, err := p.client.Do(ctx, httpReq)
	if err != nil {
		return nil, err
	}
	if httpResp.StatusCode >= 400 {
		return nil, p.decodeNativeError(httpResp)
	}

	native := sse.NewReader(httpResp.Body)
	first, err := nextNativeResult(native, p.now())
	if err != nil {
		return nil, closeAfterFailure(httpResp.Body, streamError(err))
	}
	// 零候选一律拒收：Chat 侧空 choices 是合法形态，客户端会当成「模型什么都
	// 没说」而正常收下一个 200——真实原因就此消失，且流进入 relay 后已过首字节无法 failover。
	// 仅当显式请求多候选（n > 1）时才调高期望候选数，缺省/0/负数均保持默认的 1 个候选，
	// 防止健康的上游单候选响应被误判为 upstream_unavailable 并触发无谓的 failover 与重复计费。
	wantChoices := 1
	if proj.N != nil && *proj.N > 1 {
		wantChoices = *proj.N
	}
	if len(first.Choices) == 0 || len(first.Choices) != wantChoices {
		return nil, closeAfterFailure(httpResp.Body, canonical.Newf(canonical.ClassUpstreamUnavailable,
			"上游首帧候选数为 %d，期望 %d（request_id=%q）", len(first.Choices), wantChoices, first.RequestID))
	}

	httpResp.Body = &transformReader{
		native:       native,
		body:         httpResp.Body,
		first:        first,
		cands:        &candidateState{},
		includeUsage: proj.StreamOptionsIncludeUsage,
		id:           first.RequestID,
		model:        req.Target.UpstreamModel,
		// created 只求值一次：每条 chunk 各取一次时钟，客户端看到的时间戳会跳。
		created: p.now().Unix(),
		usageCB: req.OnDashScopeUsage,
		now:     p.now,
	}
	httpResp.Header.Set("Content-Type", "text/event-stream")
	// 头与字段必须同时改：留着上游那个旧的 Content-Length，下游会按旧长度
	// 截断，或挂起等一批永远不会来的字节。
	httpResp.Header.Del("Content-Length")
	httpResp.ContentLength = -1

	return httpResp, nil
}

// closeAfterFailure 关掉再也不会被消费的上游 body，并上抛首帧失败的原因。
//
// 关必须发生：漏关会把连接钉在池子里，故障上游因此一路把连接池吃干净，
// 症状却只是「所有请求都变慢了」。关闭失败只挂在原因后面而不取代它——
// 首帧压根没拿到才是根因，报连接层的次要错误会把排查引偏。
//
// 用浅拷贝而不是 Wrapf 重造：Wrapf 只留 Class 与 Message，会把 Retryable、
// UpstreamStatus/Code/RequestID、RetryAfter、RateLimit、Param 全部丢成零值。
// 后果是实打实的——一个带 Retry-After 的 429 在「顺带关闭也失败」这条路径上
// 会退化成没有退避信息、request_id 也查不回来的错误，而这恰恰是最需要这些
// 字段的时刻。拷贝后只改 Message，其余字段（含未导出的 cause）原样带走。
func closeAfterFailure(body io.ReadCloser, cause error) error {
	closeErr := body.Close()
	if closeErr == nil {
		return cause
	}
	// AsError 对已是 canonical 的错误原样返回，非 canonical 的归一一次；
	// 两种情况都先取到一个带完整可见元数据的 *canonical.Error 再拷贝。
	cerr := canonical.AsError(cause)
	withClose := *cerr
	withClose.Message = fmt.Sprintf("%s（关闭上游响应亦失败: %v）", cerr.Message, closeErr)
	return &withClose
}

// nextNativeResult 读下一条可用的 Native result 帧。心跳注释行由 sse.Reader 跳过。
//
// io.EOF 原样上抛，由调用方交给 streamError 归一：首帧预读与流中都是「候选没
// 收尾流就断了」，两处各写一份语义只会让同一件事有两种说法。
func nextNativeResult(native *sse.Reader, now time.Time) (*nativewire.Result, error) {
	ev, err := native.Next()
	if err != nil {
		return nil, err
	}
	if ev.Event != "" && ev.Event != nativeResultEvent {
		return nil, canonical.Newf(canonical.ClassUpstreamUnavailable,
			"上游 SSE 事件 %q 不是 result 帧", ev.Event)
	}
	// 错误信封先于成功解码：DecodeFrame 是宽松的，一个纯错误信封会被它解成
	// 「零候选的成功帧」，上游给出的 code 与 request_id 就此丢掉。
	if cerr := decodeFrameError(ev.Data, now); cerr != nil {
		return nil, cerr
	}
	return nativewire.DecodeFrame(ev.Data)
}

// transformReader 是同步 transforming io.ReadCloser：从 Native SSE 逐帧读，
// 按需编码成 Chat SSE。无 goroutine、无锁、无跨请求状态。
//
// 同步是硬要求：起 goroutine 泵流意味着错误要跨 goroutine 传递，客户端提前断开
// 时那个泵还在读上游——既泄漏连接，也让错误落进没人接的 channel 里静默消失。
type transformReader struct {
	native *sse.Reader
	body   io.ReadCloser

	// first 是 Call 内预读的首帧，由 replayed 保证恰好重放一次。
	first    *nativewire.Result
	replayed bool

	// pending 是待输出缓冲，跨多次 Read 按调用方给的任意缓冲大小分批交付。
	pending []byte

	cands        *candidateState
	includeUsage bool
	id           string
	model        string
	created      int64
	lastUsage    *canonical.Usage
	usageCB      func(canonical.Usage)

	// finished 表示终止字节（可选 usage chunk 与 [DONE]）已排进 pending，
	// 不再消费上游帧；pending 排空后即 io.EOF。
	finished bool

	// err 让错误粘住：报错后再被读一次仍返回同一个错误，而不是 (0, nil)。
	err    error
	closed bool
	now    func() time.Time
}

// Read 交付转换后的 Chat SSE 字节。
//
// 绝不返回 (0, nil)：下游 relayStream 用 bufio.Scanner 再解析一次本层输出，
// 连续空读会触发 io.ErrNoProgress，一条正常的流因此变成语焉不详的流内错误。
// 空 delta 保活帧与纯 usage 帧因此必须继续推进，直到有字节可交或遇到终止/错误。
func (r *transformReader) Read(p []byte) (int, error) {
	// 零长缓冲是 io.Reader 明文允许的唯一 (0, nil)。
	if len(p) == 0 {
		return 0, nil
	}
	if r.err != nil {
		return 0, r.err
	}
	for {
		if len(r.pending) > 0 {
			n := copy(p, r.pending)
			r.pending = r.pending[n:]
			return n, nil
		}
		if r.finished {
			return 0, io.EOF
		}
		if err := r.consumeFrame(); err != nil {
			r.err = err
			return 0, err
		}
	}
}

// Close 立即关闭上游 body，且不读一个字节。
//
// 客户端断开后再去读上游，等的是一条没人要的流；而漏关会把连接连同 httpx 挂在
// Body 上的整体超时 cancel 一起悬着，症状表现为「网关越跑越慢」。
func (r *transformReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	return r.body.Close()
}

// consumeFrame 消费一个 Native 帧，把它编码成恰好一条 Chat chunk 排进 pending。
func (r *transformReader) consumeFrame() error {
	res, err := r.nextFrame()
	if err != nil {
		return streamError(err)
	}

	choices, err := r.cands.step(res.Choices)
	if err != nil {
		return err
	}
	// 用量按帧同步回调，后值覆盖前值：与 Read 同 goroutine，不需要锁。
	if res.Usage != nil {
		r.lastUsage = res.Usage
		if r.usageCB != nil {
			r.usageCB(*res.Usage)
		}
	}

	if err := r.queueChunk(choices, nil); err != nil {
		return err
	}
	if !r.cands.allFinished() {
		return nil
	}
	return r.queueTerminal()
}

// nextFrame 取下一个 Native 帧：先重放 Call 内预读的首帧，其后走 SSE reader。
func (r *transformReader) nextFrame() (*nativewire.Result, error) {
	if !r.replayed {
		r.replayed = true
		return r.first, nil
	}
	return nextNativeResult(r.native, r.now())
}

// queueTerminal 排入收尾字节：可选的 usage chunk，然后 [DONE]。
//
// usage chunk 只在客户端要过且上游真给过时才发。上游没给就不发、不伪造——
// 硬造一份「0 token 且权威」的记录等于把一次未知用量的调用计成免费。
func (r *transformReader) queueTerminal() error {
	if r.includeUsage && r.lastUsage != nil {
		// 空 choices 是 OpenAI 客户端识别「这是账单不是内容」的唯一标志。
		if err := r.queueChunk(nil, r.lastUsage); err != nil {
			return err
		}
	}
	var buf bytes.Buffer
	// Native 流没有文档化的终止标记，[DONE] 由本层合成：漏了它 OpenAI 客户端
	// 会一直等下去，因为它在等一个永远不会到来的哨兵。
	if err := sse.NewWriterTo(&buf).WriteDone(); err != nil {
		return canonical.Wrapf(err, canonical.ClassInternal, "写出流结束哨兵失败")
	}
	r.pending = append(r.pending, buf.Bytes()...)
	r.finished = true
	return nil
}

// queueChunk 把一条 chunk 编码并按 SSE 分帧排进 pending。
//
// id/model/created 一律由这里从 reader 取，不由调用方传：同一次回复的三元组
// 必须逐条 chunk 完全一致，交给调用方各填一次，漏填的那条会带着 created:0
// 发出去，而客户端只会看到一个时间戳诡异却语法合法的 chunk。
func (r *transformReader) queueChunk(choices []openaichat.ChunkChoice, usage *canonical.Usage) error {
	chunk, err := openaichat.EncodeChunk(openaichat.ChunkInput{
		ID:      r.id,
		Model:   r.model,
		Created: r.created,
		Choices: choices,
		Usage:   usage,
	})
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := sse.NewWriterTo(&buf).Write(sse.Event{Data: string(chunk)}); err != nil {
		return canonical.Wrapf(err, canonical.ClassInternal, "写出 Chat SSE 事件失败")
	}
	r.pending = append(r.pending, buf.Bytes()...)
	return nil
}

// streamError 把流中断的一切原因归一成 *canonical.Error。
//
// 两种来源都必须经过这里。EOF：候选还没全部 finish 上游就断了，合成一个干净的
// [DONE] 会让客户端把半截回复当成完整回复收下，错误率什么都看不见。裸 error：
// sse.Reader 的扫描错误是 fmt.Errorf 造的，原样上抛会被 Gateway 的 relay 按
// internal 编成下游错误事件，一次上游中断因此看起来像网关自己的 bug。
func streamError(err error) error {
	if errors.Is(err, io.EOF) {
		return canonical.Wrapf(err, canonical.ClassUpstreamUnavailable, "上游流在全部候选完成前结束")
	}
	var cerr *canonical.Error
	if errors.As(err, &cerr) {
		return err
	}
	return canonical.Wrapf(err, canonical.ClassUpstreamUnavailable, "上游流读取失败")
}

// frameEnvelope 只解判据需要的两个字段。
//
// 刻意不收 message：错误文案由 dashscopewire.DecodeError 从原始字节里自己解，
// 在这里再存一份就有了两个事实来源，两边迟早说法不一。
//
// status_code 用指针：SDK 包装的正常帧会带 status_code:200，与「没有这个字段」
// 都不是错误，但两者都必须与 4xx/5xx 区分开。
type frameEnvelope struct {
	Code       string `json:"code"`
	StatusCode *int   `json:"status_code"`
}

// decodeFrameError 检出流中的 Native 错误帧，非错误帧返回 nil。
//
// 判据分两级，**status_code 存在时一律压过 code**：
//
//   - status_code 存在且 < 400：正常帧，**哪怕 code 非空也放行**。SDK 包装层
//     会把自己的字段一起塞进帧里，其中的 code 未必表示失败；让一个非空 code
//     否决掉上游明确给出的 200，会让一整条正常的流在第一帧就被判成故障。
//   - status_code 存在且 >= 400：错误帧，按该状态码分类。
//   - status_code 缺省：退回看顶层 code——正常 result 帧不带它，非空即错误帧。
//     此时没有状态码可依，兜底成 500。
func decodeFrameError(data string, now time.Time) *canonical.Error {
	var env frameEnvelope
	if err := json.Unmarshal([]byte(data), &env); err != nil {
		// 解不出来的字节交给 DecodeFrame 报错：在这里抢先分类会把畸形
		// 与错误信封混成一档，上游真正的错误码反而看不见。
		return nil
	}
	status := http.StatusInternalServerError
	if env.StatusCode != nil {
		if *env.StatusCode < http.StatusBadRequest {
			return nil
		}
		status = *env.StatusCode
	} else if env.Code == "" {
		return nil
	}
	// 状态码兜底成 500 而不是 0：DecodeError 的 code 分支认不出的错误码还要
	// 靠状态码兜底分类，给 0 会让它落进 WebSocket 那条分支。
	return dashscopewire.DecodeError(status, []byte(data), nil, now)
}
