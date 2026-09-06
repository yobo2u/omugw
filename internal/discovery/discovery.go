// Package discovery 向已配置的上游询问它们各自的模型清单。
//
// 它只回答一个问题：「这个上游此刻声称自己有哪些模型」。发现结果**只**充实
// 受鉴权的 GET /v1/models，绝不进入选路——上游目录不等于本网关可调用的集合
// （DashScope 的列表接口明说返回的是「平台上可用的模型」，OpenAI 的列表也不
// 带「这把 key 能否调用」的位）。若发现即建路由，一次上游目录变更就能让网关
// 无声地把请求发去一个没人配置过、降级矩阵也没审视过的目的地。
//
// 因此这个包的失败模式是刻意收窄的：最坏后果是「清单少了几行」，
// 而不是「请求去错了地方」。
package discovery

import (
	"context"
	"sort"
	"sync"

	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/transport/httpx"
)

// Model 是发现到的一个上游模型。
//
// 字段取三家的交集加来源标注，不做更多建模：这份数据唯一的去处是 /v1/models
// 的清单，多存的字段没有消费者，却会在上游改字段时变成维护负担。
type Model struct {
	// ID 是上游模型 ID。OpenAI 取 data[].id，DashScope Native 取
	// output.models[].model。
	ID string

	// Endpoint 是这个模型来自哪个已配置的 provider endpoint。
	Endpoint string

	// OwnedBy 是归属方。OpenAI 直接有；DashScope Native 没有等价字段，
	// 用 provider（模型作者）兜底，可能为空。
	//
	// 它是**归属**不是**授权**：owned_by 说不了这把凭据能不能调用该模型。
	OwnedBy string
}

// Prober 是一个协议族的枚举实现。
//
// 按协议族而不是按 endpoint 分：OpenAI、DeepSeek、Kimi 都是 openai.compat，
// 枚举接口完全一样，没有理由各写一份。
type Prober interface {
	// Probe 向一个上游拉取模型清单。
	//
	// baseURL 与 secret 随调用传入而不在构造时固定：同一个协议族下有多个
	// endpoint，各自的地址与凭据都不同。
	//
	// client 走网关统一的上游客户端而不是 http.DefaultClient：目录查询同样
	// 需要独立的 connect 一层，否则一个建连挂住的上游要拖满整个发现超时才
	// 失败，而那本该在几秒内就判死。
	Probe(ctx context.Context, client *httpx.Client, baseURL, secret string) ([]Model, error)
}

// Probers 返回各协议族的探测器。
//
// dashscope.compatible **刻意不在表内**：官方文档没有 compatible-mode 的
// 列表接口（见 docs/research/dashscope-compatible-get-v1-models.md）。
// 即使线上探测「碰巧能通」，那也是未文档化的行为，不该被当成契约实现——
// 上游哪天关掉它，网关这边表现为一个没人能解释的清单缺口。
func Probers() map[degrade.Provider]Prober {
	return map[degrade.Provider]Prober{
		degrade.ProviderOpenAICompat:    &OpenAIProber{},
		degrade.ProviderDashScopeNative: &DashScopeNativeProber{},
	}
}

// Registry 持有各 endpoint 的最近一次成功快照。
type Registry struct {
	mu   sync.RWMutex
	snap map[string][]Model
}

// NewRegistry 构造空注册表。
func NewRegistry() *Registry {
	return &Registry{snap: map[string][]Model{}}
}

// Put 覆盖某个 endpoint 的快照。
func (r *Registry) Put(endpoint string, models []Model) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.snap[endpoint] = models
}

// Snapshot 返回全部 endpoint 的模型，按 ID 字典序去重。
//
// 去重是必须的：同一个模型 ID 可能同时出现在多个 endpoint 上（两把不同的
// OpenAI 凭据指向同一个上游），清单里列两遍只会让客户端困惑。排序是为了
// 让响应稳定可 diff——顺序随 map 遍历漂移的清单，没法用来做变更对比。
func (r *Registry) Snapshot() []Model {
	r.mu.RLock()
	defer r.mu.RUnlock()

	seen := map[string]Model{}
	for _, models := range r.snap {
		for _, m := range models {
			if _, dup := seen[m.ID]; !dup {
				seen[m.ID] = m
			}
		}
	}

	out := make([]Model, 0, len(seen))
	for _, m := range seen {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
