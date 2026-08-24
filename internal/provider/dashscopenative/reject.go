package dashscopenative

import (
	"fmt"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/protocol/openaichat"
)

// rejectUnmappable 拦截 DashScope Native 无法表达的语义。
//
// 投影只提供 N 与无落点字段的 presence；流式、推理与 tools 全取 Canonical，
// 绝不从投影复制第二份语义源。
func rejectUnmappable(proj *openaichat.Projection, req *canonical.Request) error {
	// 拦截无落点字段，防止 falsy 或空值等显式提交的语义在转换后静默丢失。
	if proj.FrequencyPenaltyPresent {
		return unsupported("frequency_penalty")
	}
	if proj.LogitBiasPresent {
		return unsupported("logit_bias")
	}
	if proj.ServiceTierPresent {
		return unsupported("service_tier")
	}
	if proj.StorePresent {
		return unsupported("store")
	}
	if proj.UserPresent {
		return unsupported("user")
	}
	if proj.MetadataPresent {
		return unsupported("metadata")
	}
	if proj.AudioPresent {
		return unsupported("audio")
	}

	// 统一从 Canonical 提取语义，防止与投影形成互相漂移的双事实源。
	activeReasoning := req.Reasoning != nil && req.Reasoning.Effort != canonical.EffortNone
	hasTools := len(req.Tools) > 0
	nGreaterThanOne := proj.N != nil && *proj.N > 1

	// 拦截非流式思考模式，防止触发上游硬约束导致行为未定义。
	if activeReasoning && !req.Stream {
		return unsupported("reasoning_effort")
	}

	// 拦截带 tools 时的多重生成，防止 Native 静默将 n 强制回落为 1 导致候选丢失。
	if nGreaterThanOne && hasTools {
		return unsupported("n")
	}

	// 拦截思考模式下的多重生成，防止未文档化的强制回落，fail-closed。
	if nGreaterThanOne && activeReasoning {
		return unsupported("n")
	}

	return nil
}

func unsupported(param string) error {
	return &canonical.Error{
		Class:   canonical.ClassUnsupported,
		Message: fmt.Sprintf("字段 %s 在 DashScope Native 无落点", param),
		Param:   param,
	}
}
