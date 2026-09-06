package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/protocol/dashscopewire"
	"github.com/yobo2u/omugw/internal/transport/httpx"
)

// DashScopeNativeProber 枚举 dashscope.native 上游的模型。
//
// 契约见 docs/research/dashscope-native-model-discovery.md：
// GET /api/v1/models，Bearer 鉴权，**自有信封**而不是 OpenAI 那套——
// 模型 ID 在 output.models[].model，分页是页码式（page_no / page_size），
// 默认每页 20 条。一次 GET 不是全量，必须翻页。
type DashScopeNativeProber struct{}

// dashScopePageSize 是每页请求条数。
//
// 官方在列表接口上**没有**文档化 page_size 上限（兄弟接口 permissions 写的
// 200 不能直接搬过来）。取 100 是文档示例中出现过的值，既少翻几页，
// 又不去赌一个没写明的上界。
const dashScopePageSize = 100

// dashScopeMaxPages 是翻页硬上限。
//
// 防的是上游把 total 报大（或报成一个永远追不上的数）导致这个循环停不下来：
// 一个后台刷新任务卡在死循环里，表现出来是「发现好像一直在跑但快照永不更新」，
// 比直接失败难查得多。
const dashScopeMaxPages = 50

type dashScopeList struct {
	Output struct {
		Total  int `json:"total"`
		Models []struct {
			Model    string `json:"model"`
			Provider string `json:"provider"`
		} `json:"models"`
	} `json:"output"`
}

// Probe 翻页拉取完整清单。
func (p *DashScopeNativeProber) Probe(ctx context.Context, client *httpx.Client, baseURL, secret string) ([]Model, error) {
	base := strings.TrimSuffix(baseURL, "/") + "/api/v1/models"

	var out []Model
	for page := 1; page <= dashScopeMaxPages; page++ {
		list, err := p.fetchPage(ctx, client, base, secret, page)
		if err != nil {
			return nil, err
		}

		// 空页是「翻到头了」的可靠信号，比信 total 更稳。
		if len(list.Output.Models) == 0 {
			break
		}
		for _, m := range list.Output.Models {
			if m.Model == "" {
				continue
			}
			out = append(out, Model{ID: m.Model, OwnedBy: m.Provider})
		}

		if list.Output.Total > 0 && len(out) >= list.Output.Total {
			break
		}
	}
	return out, nil
}

func (p *DashScopeNativeProber) fetchPage(
	ctx context.Context, client *httpx.Client, base, secret string, page int,
) (*dashScopeList, error) {
	url := fmt.Sprintf("%s?page_no=%d&page_size=%d", base, page, dashScopePageSize)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassInternal, "构造模型列表请求失败")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+secret)

	// Body 必须关闭：httpx 把整体超时的 cancel 挂在它上面。翻页会跑很多轮，
	// 漏一次就攒一个泄漏的 context。
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
		// DashScope 是扁平信封，与 OpenAI 的嵌套 error 对象不同族，
		// 用错解码器会把一个有分类的上游错误降级成「无法解析」。
		return nil, dashscopewire.DecodeError(resp.StatusCode, body, resp.Header, time.Now())
	}

	var list dashScopeList
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassUpstreamUnavailable,
			"模型列表响应不是合法 JSON")
	}
	return &list, nil
}

// maxListBody 是列表响应的读取上限。
//
// 与各出站适配器的错误体上限同理：一个故障上游可能在 200 里塞进一整个 HTML
// 页面。发现是后台任务，没人盯着它，更需要自己设上界。
const maxListBody = 4 << 20

func readCapped(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxListBody))
	if err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassUpstreamUnavailable, "读取模型列表响应失败")
	}
	return body, nil
}
