//go:build smoke

package smoke_test

import (
	"encoding/json"
	"os"
	"strings"

	nativewire "github.com/yobo2u/omugw/internal/protocol/dashscopenative"
)

// modelRole 标识不同能力用例所需的模型角色，支持按角色通过环境变量覆盖具体模型名。
type modelRole string

const (
	modelRoleText     modelRole = "text"
	modelRoleVL       modelRole = "vl"
	modelRoleCombined modelRole = "combined"
)

// modelForRole 解析指定模型角色的模型名，优先从环境变量读取，缺省使用设计默认模型。
// 对未知或不可能的角色返回空字符串，由上层调用方进行显式断言或校验。
//
// 本期没有任何音频角色：qwen-audio-turbo 免费额度耗尽，Qwen-Omni 只走 OpenAI
// 兼容线格式。在这张表里留下一个音频模型名，等于给一个永远不会成功的调用
// 留了个账单入口。
func modelForRole(role modelRole) string {
	switch role {
	case modelRoleText:
		if v := strings.TrimSpace(os.Getenv("OMUGW_SMOKE_MODEL_TEXT")); v != "" {
			return v
		}
		return "qwen-plus"
	case modelRoleVL:
		if v := strings.TrimSpace(os.Getenv("OMUGW_SMOKE_MODEL_VL")); v != "" {
			return v
		}
		return "qwen-vl-max"
	case modelRoleCombined:
		if v := strings.TrimSpace(os.Getenv("OMUGW_SMOKE_MODEL_COMBINED")); v != "" {
			return v
		}
		return "qwen-vl-max"
	default:
		return ""
	}
}

// caseMeta 描述录制用例的元数据定义。
type caseMeta struct {
	name           string
	door           nativewire.Door
	modelRole      modelRole
	stream         bool
	expectDegraded bool
	note           string

	// body 按选定模型名产出该用例的客户端请求体。
	//
	// 收成构造器而不是常量：模型名由 modelForRole 在录制时才解析（可被环境变量
	// 覆盖），写死在字面量里的那份会与真正发出去的模型分家——上游按 A 计费，
	// 证据上却写着 B。
	body func(model string) json.RawMessage
}
