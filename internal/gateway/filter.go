package gateway

import (
	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/router"
)

// filterNativeTargets 过滤 DashScope Native 候选上游。
//
// 文本生成门（text-generation）只接受字符串 content，无法承载图片或音频。
// 过滤防止了含媒体的请求被错误地发往文本门而导致上游报错。
// 过滤过程严格保留配置中声明的 failover 顺序。
func filterNativeTargets(targets []router.Target, caps []canonical.Capability) []router.Target {
	hasMedia := false
	for _, c := range caps {
		if c == canonical.CapVisionInput || c == canonical.CapAudioInput {
			hasMedia = true
			break
		}
	}

	if !hasMedia {
		return targets
	}

	var filtered []router.Target
	for _, t := range targets {
		if t.NativeEndpoint == "multimodal-generation" {
			filtered = append(filtered, t)
		}
	}
	return filtered
}
