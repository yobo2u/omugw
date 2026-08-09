// Package convstore 是网关侧的会话历史存储。
//
// 存在的理由：OpenAI Responses 的客户端可以只发 previous_response_id 而不带
// 历史，但 Anthropic Messages 和 DashScope Native 都是无状态协议——它们要求
// 每次请求携带完整上下文。这道鸿沟不可能靠字段映射填平，只能由网关**自己保管
// 历史**再回放给上游。
//
// 这就是降级矩阵里 EMULATE 那一档的实体：客户端拿到的能力是完整的，但这份
// 完整性是网关垫出来的，因而带上了网关自己的可用性边界。Phase 1 是内存态，
// 边界写在 MemoryStore 的文档里，也写进了矩阵的处置说明——两处必须一致，
// 否则用户会按一个不存在的保证去做重试决策。
package convstore

import (
	"context"
	"errors"
	"time"

	"github.com/yobo2u/omugw/internal/canonical"
)

var (
	// ErrNotFound 表示引用的响应不存在或已过期。
	//
	// 两种情况刻意不区分：从客户端视角它们没有差别，而区分开来会泄露
	// 「这个 ID 曾经存在过」，让人能探测别人的会话 ID 空间。
	ErrNotFound = errors.New("convstore: 会话不存在或已过期")

	// ErrChainTooLong 表示会话链超过深度上限。
	ErrChainTooLong = errors.New("convstore: 会话链过长")

	// ErrTooLarge 表示会话累计消息数超过上限。
	ErrTooLarge = errors.New("convstore: 会话消息数超过上限")
)

// Turn 是会话链上的一环：一次请求-响应产生的新消息，加上它的前驱。
//
// 采用链式而不是「整段历史存一份」，是为了对齐 Responses 的实际语义：
// 客户端可以从任意一个历史响应分叉出新的对话（同一个 previous_response_id
// 被引用两次），此时两条分支共享前缀却各自延伸。整段存储会在分叉时把前缀
// 复制一份，既费内存，也让「这两条对话本是同源」这件事丢失。
type Turn struct {
	// ID 是这一轮的响应标识，对应 Responses 的 response.id。
	ID string

	// PrevID 是前驱响应的 ID。空表示这是会话的第一轮。
	PrevID string

	// Messages 是这一轮新增的消息（用户输入 + 助手输出）。
	// 不含前驱的历史——那要靠 PrevID 回溯。
	Messages []canonical.Message

	// Model 记录这一轮用的模型。分叉时用它判断上下文是否仍然兼容。
	Model string

	CreatedAt time.Time
}

// Store 是会话历史存储。
//
// 实现必须是并发安全的：同一个会话可能被多个请求同时读取（客户端从一个
// previous_response_id 并发分叉出多轮对话是合法用法）。
type Store interface {
	// Append 追加一轮对话。prevID 为空表示新会话。
	//
	// 返回新一轮的 ID。prevID 指向不存在或已过期的响应时返回 ErrNotFound——
	// 绝不静默地当成新会话开始，那会让客户端以为上下文还在，
	// 实际上模型看到的是一段没头没尾的对话。
	Append(ctx context.Context, prevID string, msgs []canonical.Message, model string) (string, error)

	// History 回溯出到 id 为止的完整历史，按时间正序。
	History(ctx context.Context, id string) ([]canonical.Message, error)

	// Turn 取出单独一轮，用于判断模型是否变更等。
	Turn(ctx context.Context, id string) (Turn, error)

	// Delete 删除一轮及其全部后继。
	Delete(ctx context.Context, id string) error
}

// Limits 是会话存储的资源上限。
//
// 上限必须存在。没有它，一个不断追加的长会话就能把网关的内存吃光——
// 而且这不需要恶意，一个跑了三天的 agent 循环就够了。
type Limits struct {
	// MaxChainDepth 是单个会话的最大轮数。
	MaxChainDepth int

	// MaxMessages 是回溯出的历史消息数上限。
	MaxMessages int

	// TTL 是一轮对话的存活时间，从最后一次被读取或延伸算起。
	TTL time.Duration
}

// DefaultLimits 返回一组保守的默认上限。
func DefaultLimits() Limits {
	return Limits{
		MaxChainDepth: 200,
		MaxMessages:   1000,
		TTL:           2 * time.Hour,
	}
}
