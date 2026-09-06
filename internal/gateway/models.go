package gateway

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/discovery"
	"github.com/yobo2u/omugw/internal/protocol/openaiwire"
	"github.com/yobo2u/omugw/internal/router"
)

// ModelsHandler 服务受鉴权的 GET /v1/models。
//
// 它是网关自身的管理面读接口，不是一条转换路径：不转发请求体，不产生能力
// 损失，因此不进降级矩阵，也不需要 Redeem。Router.Resolve 的错误消息刻意
// 不列可用模型，并把想看清单的人指到这里——那条指引在这扇门建好之前是空头
// 支票（请求会落进框架 404）。
type ModelsHandler struct {
	auth     *Authenticator
	router   *router.Router
	registry *discovery.Registry
}

// NewModelsHandler 构造处理器。registry 可为 nil，表示未启用发现。
func NewModelsHandler(a *Authenticator, r *router.Router, reg *discovery.Registry) *ModelsHandler {
	return &ModelsHandler{auth: a, router: r, registry: reg}
}

// modelObject 是 OpenAI list 信封里的一项。
//
// 字段照抄 OpenAI 契约，好让现成的 OpenAI 客户端能直接读。created 对拿不到
// 时间的来源填 0——DashScope Native 的列表里根本没有这个字段，编一个时间戳
// 会让客户端拿它去做「模型是否更新」的判断，而那个判断依据是假的。
type modelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type modelList struct {
	Object string        `json:"object"`
	Data   []modelObject `json:"data"`
}

func (h *ModelsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, err := h.auth.Authenticate(r); err != nil {
		status, body, headers := openaiwire.EncodeError(canonical.AsError(err))
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
		return
	}

	payload := modelList{Object: "list", Data: h.models()}
	// 字段全是字符串与整数，Marshal 不会失败。
	body, _ := json.Marshal(payload)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// models 合并配置模型与发现模型。
//
// 配置项优先：配置是权威（它决定请求真的能去哪），发现只是补充（上游声称
// 自己有什么）。同一个 ID 两边都有时，保留配置那份的归属标注。
func (h *ModelsHandler) models() []modelObject {
	seen := map[string]modelObject{}

	for _, name := range h.router.Models() {
		seen[name] = modelObject{ID: name, Object: "model", OwnedBy: "omugw"}
	}

	if h.registry != nil {
		for _, m := range h.registry.Snapshot() {
			if _, dup := seen[m.ID]; dup {
				continue
			}
			seen[m.ID] = modelObject{ID: m.ID, Object: "model", OwnedBy: m.OwnedBy}
		}
	}

	out := make([]modelObject, 0, len(seen))
	for _, m := range seen {
		out = append(out, m)
	}
	// 顺序随 map 遍历漂移的清单没法用来做变更对比。
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
