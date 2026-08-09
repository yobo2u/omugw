package canonical

import "fmt"

// Role 是消息角色。
//
// 刻意不包含 "developer"：OpenAI 把 system 重命名为 developer，Anthropic 用顶层
// system 参数，DashScope 用 system role。这些差异由各协议解码器归一到 RoleSystem，
// 编码器再按目标协议还原。
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message 是一轮对话消息。
type Message struct {
	Role  Role   `json:"role"`
	Parts []Part `json:"parts"`

	// Name 是可选的参与者标识，仅 OpenAI 系协议使用。
	Name string `json:"name,omitempty"`
}

// Validate 校验消息自身的一致性。
func (m Message) Validate() error {
	switch m.Role {
	case RoleSystem, RoleUser, RoleAssistant, RoleTool:
	default:
		return fmt.Errorf("canonical: unknown role %q", m.Role)
	}
	if len(m.Parts) == 0 {
		return fmt.Errorf("canonical: message with role %q has no parts", m.Role)
	}
	for i, p := range m.Parts {
		if err := p.Validate(); err != nil {
			return fmt.Errorf("canonical: message[role=%s].parts[%d]: %w", m.Role, i, err)
		}
		if m.Role == RoleTool && p.Kind != PartToolResult {
			return fmt.Errorf("canonical: tool message may only carry tool_result parts, got %q at index %d", p.Kind, i)
		}
	}
	return nil
}

// TextContent 拼接消息中所有文本块，供日志与审计使用。
// 它会跳过 thinking——推理内容不应进入普通日志。
func (m Message) TextContent() string {
	var out []byte
	for _, p := range m.Parts {
		if p.Kind == PartText {
			if len(out) > 0 {
				out = append(out, '\n')
			}
			out = append(out, p.Text...)
		}
	}
	return string(out)
}

// UserText 构造一条纯文本用户消息。
func UserText(s string) Message {
	return Message{Role: RoleUser, Parts: []Part{Text(s)}}
}

// AssistantText 构造一条纯文本助手消息。
func AssistantText(s string) Message {
	return Message{Role: RoleAssistant, Parts: []Part{Text(s)}}
}
