package discovery

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/protocol/openaiwire"
	"github.com/yobo2u/omugw/internal/transport/httpx"
)

// OpenAIProber 枚举 openai.compat 上游的模型。
//
// 契约见 docs/research/openai-list-models-contract.md：GET /v1/models，
// Bearer 鉴权，`{object:"list", data:[...]}`，**没有分页参数**——官方 SDK
// 里那句「no pagination actually occurs yet」是前向兼容的占位，不是当前行为。
// 因此这里一次 GET 拿全量，不发 limit / after。
type OpenAIProber struct{}

// openAIList 是列表响应。
//
// 只解出 ID 与 owned_by：其余字段（created / shutdown_date）在清单里没有
// 消费者。OpenAI 明确声明「往响应对象里加新属性」属于向后兼容变更，所以
// 解码必须容忍未知字段——用 DisallowUnknownFields 会让上游一次例行更新
// 就把发现打挂。
type openAIList struct {
	Data []struct {
		ID      string `json:"id"`
		OwnedBy string `json:"owned_by"`
	} `json:"data"`
}

// Probe 拉取一次完整清单。
func (p *OpenAIProber) Probe(ctx context.Context, client *httpx.Client, baseURL, secret string) ([]Model, error) {
	url := strings.TrimSuffix(baseURL, "/") + "/v1/models"

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassInternal, "构造模型列表请求失败")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+secret)

	// httpx.Do 自己往 ctx 上叠整体超时并把 cancel 挂在 Body 上，
	// 所以请求不预先绑 ctx，且 Body 必须关闭，否则那个 cancel 永远不释放。
	resp, err := client.Do(ctx, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := readCapped(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, openaiwire.DecodeError(resp.StatusCode, body, resp.Header, time.Now())
	}

	var list openAIList
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassUpstreamUnavailable,
			"模型列表响应不是合法 JSON")
	}

	out := make([]Model, 0, len(list.Data))
	for _, m := range list.Data {
		// 没有 ID 的条目无法被任何客户端引用，收进清单只会制造噪声。
		if m.ID == "" {
			continue
		}
		out = append(out, Model{ID: m.ID, OwnedBy: m.OwnedBy})
	}
	return out, nil
}
