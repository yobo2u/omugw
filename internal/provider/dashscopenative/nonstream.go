package dashscopenative

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/yobo2u/omugw/internal/canonical"
	nativewire "github.com/yobo2u/omugw/internal/protocol/dashscopenative"
	"github.com/yobo2u/omugw/internal/protocol/dashscopewire"
	"github.com/yobo2u/omugw/internal/protocol/openaichat"
	"github.com/yobo2u/omugw/internal/provider"
	"github.com/yobo2u/omugw/internal/transport/httpx"
)

// semanticHeaders 是 DashScope Native 允许透传的语义头白名单。
// 必须与 passthrough 保持一致，不透传 Authorization 等敏感头。
var semanticHeaders = []string{
	"X-DashScope-WorkSpace",
	"X-DashScope-DataInspection",
	"X-DashScope-Async",
}

// maxErrorBody 是上游错误体的读取上限。
//
// 故障上游可能在 4xx/5xx 里塞进一整个 HTML 错误页，甚至更大的东西。不设上限
// 时 io.ReadAll 会把它整个读进内存——一次上游故障就升级成网关自己的内存事故，
// 而错误分类只需要开头那点 JSON。
const maxErrorBody = 64 << 10

// translateNonStream 在 Call 返回前完成整条非流式转换：Native 进、chat.completion 出。
//
// 全程不向下游写一个字节，是刻意的：一旦下游收到首字节就再也不能重试（原则 2.4），
// 而这里的每一步——编码、上游调用、解码、重编码——都可能失败。全部收在 Call
// 之内，失败就还是一个可 failover 的 *canonical.Error。
func (p *Provider) translateNonStream(ctx context.Context, req provider.Request, proj *openaichat.Projection) (*httpx.Response, error) {
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
		IncrementalOutput:   false,
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
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+req.Credential.Secret)
	for _, h := range semanticHeaders {
		if v := req.Header.Get(h); v != "" {
			httpReq.Header.Set(h, v)
		}
	}

	// 传输层错误原样上抛：httpx.Client.Do 已经把连接、首字节、整体三种超时
	// 分别归好类了。在这里再包一层 upstream_unavailable，会把「客户端主动断开」
	// 那一档也改写成上游故障——换凭据重试一个已经没人等的请求。
	httpResp, err := p.client.Do(ctx, httpReq)
	if err != nil {
		return nil, err
	}

	if httpResp.StatusCode >= 400 {
		return nil, p.decodeNativeError(httpResp)
	}

	respBody, err := readNativeBody(httpResp)
	if err != nil {
		return nil, err
	}

	res, err := nativewire.DecodeResult(respBody)
	if err != nil {
		return nil, err
	}

	// 候选条数不符一律拒收（零候选、欠交付、过交付）：
	//
	// 空数组在 Chat 线格式里是合法的，客户端会当成「模型什么都没说」而正常收下
	// 一个 200——真实原因（异步任务受理、上游内部截断、信封形态不符）就此消失，
	// 既进不了错误率，也换不到另一个凭据或 Provider 重试。
	// 欠交付是上游吞了候选（客户端按 n 计费却拿不全），过交付是上游越界注入。
	// 两者均属于确定性响应契约违例，不可重试（Retryable=false）。
	// 仅当显式请求多候选（n > 1）时才调高期望候选数，缺省/0/负数均保持默认的 1 个候选，
	// 防止健康的上游单候选响应被误判为 upstream_unavailable。
	wantChoices := 1
	if proj.N != nil && *proj.N > 1 {
		wantChoices = *proj.N
	}
	if len(res.Choices) != wantChoices {
		return nil, &canonical.Error{
			Class:             canonical.ClassUpstreamUnavailable,
			Message:           fmt.Sprintf("上游响应候选数为 %d，期望 %d（request_id=%q）", len(res.Choices), wantChoices, res.RequestID),
			Retryable:         false,
			UpstreamRequestID: res.RequestID,
		}
	}

	choices := make([]openaichat.CompletionChoice, 0, len(res.Choices))
	for i, c := range res.Choices {
		var parts []canonical.Part
		if c.ReasoningContent != "" {
			parts = append(parts, canonical.Part{
				Kind:     canonical.PartThinking,
				Thinking: &canonical.Thinking{Text: c.ReasoningContent},
			})
		}
		if c.Content != "" {
			parts = append(parts, canonical.Text(c.Content))
		}
		for j, tc := range c.ToolCalls {
			if !json.Valid([]byte(tc.Arguments)) {
				return nil, canonical.Newf(canonical.ClassUpstreamUnavailable, "choices[%d].tool_calls[%d] 参数不是合法 JSON", i, j)
			}
			parts = append(parts, canonical.Part{
				Kind: canonical.PartToolCall,
				ToolCall: &canonical.ToolCall{
					ID:        tc.ID,
					Name:      tc.Name,
					Arguments: json.RawMessage(tc.Arguments),
				},
			})
		}

		var fr *string
		if c.FinishReason != "" {
			fr = &c.FinishReason
		}

		choices = append(choices, openaichat.CompletionChoice{
			Message: canonical.Message{
				Role:  canonical.RoleAssistant,
				Parts: parts,
			},
			FinishReason: fr,
			Logprobs:     c.Logprobs,
		})
	}

	compIn := openaichat.CompletionInput{
		ID:      res.RequestID,
		Model:   req.Target.UpstreamModel,
		Created: p.now().Unix(),
		Choices: choices,
		Usage:   res.Usage,
	}

	chatBody, err := openaichat.EncodeCompletion(compIn)
	if err != nil {
		return nil, err
	}

	if req.OnDashScopeUsage != nil && res.Usage != nil {
		req.OnDashScopeUsage(*res.Usage)
	}

	httpResp.Body = io.NopCloser(bytes.NewReader(chatBody))
	httpResp.Header.Set("Content-Type", "application/json")
	httpResp.Header.Set("Content-Length", strconv.Itoa(len(chatBody)))
	httpResp.ContentLength = int64(len(chatBody))

	return httpResp, nil
}

// readNativeBody 读净并关闭上游成功响应体。
//
// Close 必须发生且只发生一次，无论读取是否成功：这条路径之后会把 Body 换成
// 重编码后的字节，原来那个再没人碰得到——漏关就等于把 httpx 挂在 Body 上的
// 整体超时 cancel 一起漏掉，连接与上下文一直悬着，症状表现为「网关越跑越慢」。
//
// 两种错误的优先级不能颠倒：读取失败说明字节本身就没拿全，此时的 Close 结果
// 是次要的，报读取错误才指向真正的故障；只有读取成功、单纯关闭失败时，才把
// 关闭错误抛出去——那说明连接处于异常状态，这次响应不值得信任。
func readNativeBody(resp *httpx.Response) ([]byte, error) {
	body, readErr := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if readErr != nil {
		return nil, canonical.Wrapf(readErr, canonical.ClassUpstreamUnavailable, "读取上游响应失败")
	}
	if closeErr != nil {
		return nil, canonical.Wrapf(closeErr, canonical.ClassUpstreamUnavailable, "关闭上游响应失败")
	}
	return body, nil
}

// decodeNativeError 把上游非 2xx 响应解成统一错误，供上层 failover。
//
// 无条件关 Body：这条路径不再返回响应，漏关会把连接钉在池子里，故障上游因此
// 一路把连接池吃干净，症状却表现为「所有请求都变慢了」。
//
// 读取截断到 maxErrorBody；读取错误刻意丢弃：拿到多少解多少，一个字节都读不到
// 也仍有状态码可依。因为读不全就放弃分类，等于把一次可 failover 的 429 降级成
// 来源不明的 internal，重试机会白白丢掉。
func (p *Provider) decodeNativeError(resp *httpx.Response) error {
	defer resp.Body.Close()
	errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	return dashscopewire.DecodeError(resp.StatusCode, errBody, resp.Header, p.now())
}
