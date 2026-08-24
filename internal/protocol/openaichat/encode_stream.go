package openaichat

import (
	"encoding/json"

	"github.com/yobo2u/omugw/internal/canonical"
)

// ChunkInput 是拼装一条 chat.completion.chunk 的 Canonical 素材。
// 一个 Native 帧编码成一条 chunk，其中该帧出现的每个候选各占一个 choices 条目。
type ChunkInput struct {
	ID      string
	Model   string
	Created int64
	Choices []ChunkChoice
	Usage   *canonical.Usage // nil = 本 chunk 不带 usage
}

// ChunkChoice 是一个候选的增量。
type ChunkChoice struct {
	Index        int
	Delta        ChunkDelta
	FinishReason string // ""=finish_reason 输出 null
}

// ChunkDelta 是增量内容。工具 arguments 保持字符串片段，允许 JSON 跨帧切断。
type ChunkDelta struct {
	Role             string // ""=省略 role
	Content          string
	ReasoningContent string
	ToolCalls        []ToolCallDelta
}

// ToolCallDelta 是一个工具调用的增量。ID/Name 只在该调用首帧出现。
type ToolCallDelta struct {
	Index     int
	ID        string
	Name      string
	Arguments string // JSON 片段，可能不闭合
}

// DoneSentinel 是流结束哨兵，由流式侧在全部候选 finish 后合成。
const DoneSentinel = "[DONE]"

// respChunk 是 chat.completion.chunk 的顶层线格式。
type respChunk struct {
	ID      string            `json:"id"`
	Object  string            `json:"object"`
	Created int64             `json:"created"`
	Model   string            `json:"model"`
	Choices []respChunkChoice `json:"choices"`
	Usage   *respUsage        `json:"usage,omitempty"`
}

// respChunkChoice 是一个候选的线格式。
type respChunkChoice struct {
	Index        int            `json:"index"`
	Delta        respChunkDelta `json:"delta"`
	FinishReason *string        `json:"finish_reason"`
}

// respChunkDelta 是增量内容的线格式。
type respChunkDelta struct {
	Role             string              `json:"role,omitempty"`
	Content          string              `json:"content,omitempty"`
	ReasoningContent string              `json:"reasoning_content,omitempty"`
	ToolCalls        []respChunkToolCall `json:"tool_calls,omitempty"`
}

// respChunkToolCall 是工具调用的增量线格式。
type respChunkToolCall struct {
	Index    int                `json:"index"`
	ID       string             `json:"id,omitempty"`
	Type     string             `json:"type,omitempty"`
	Function *respChunkFunction `json:"function,omitempty"`
}

// respChunkFunction 是工具调用的函数体增量线格式。
type respChunkFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// EncodeChunk 编码一条 chat.completion.chunk 的 JSON（不含 SSE 分帧）。
func EncodeChunk(in ChunkInput) ([]byte, error) {
	choices := make([]respChunkChoice, 0, len(in.Choices))
	for _, c := range in.Choices {
		var fr *string
		if c.FinishReason != "" {
			// 必须拷贝一份，否则循环变量地址相同
			reason := c.FinishReason
			fr = &reason
		}

		var toolCalls []respChunkToolCall
		if len(c.Delta.ToolCalls) > 0 {
			toolCalls = make([]respChunkToolCall, 0, len(c.Delta.ToolCalls))
			for _, tc := range c.Delta.ToolCalls {
				var typ string
				if tc.ID != "" {
					typ = "function"
				}

				var fn *respChunkFunction
				if tc.Name != "" || tc.Arguments != "" {
					fn = &respChunkFunction{
						Name:      tc.Name,
						Arguments: tc.Arguments,
					}
				}

				toolCalls = append(toolCalls, respChunkToolCall{
					Index:    tc.Index,
					ID:       tc.ID,
					Type:     typ,
					Function: fn,
				})
			}
		}

		choices = append(choices, respChunkChoice{
			Index: c.Index,
			Delta: respChunkDelta{
				Role:             c.Delta.Role,
				Content:          c.Delta.Content,
				ReasoningContent: c.Delta.ReasoningContent,
				ToolCalls:        toolCalls,
			},
			FinishReason: fr,
		})
	}

	var usage *respUsage
	if in.Usage != nil {
		u, err := encodeUsage(in.Usage)
		if err != nil {
			return nil, err
		}
		usage = u
	}

	out := respChunk{
		ID:      in.ID,
		Object:  "chat.completion.chunk",
		Created: in.Created,
		Model:   in.Model,
		Choices: choices,
		Usage:   usage,
	}

	raw, err := json.Marshal(out)
	if err != nil {
		return nil, canonical.Wrapf(err, canonical.ClassInternal, "chat completion chunk 序列化失败")
	}
	return raw, nil
}
