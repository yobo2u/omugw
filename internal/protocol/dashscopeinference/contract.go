package dashscopeinference

import "errors"

type UsageUnit string

const (
	UnitUnknown    UsageUnit = "unknown"
	UnitTokens     UsageUnit = "tokens"
	UnitCharacters UsageUnit = "characters"
	UnitSeconds    UsageUnit = "seconds"
)

type UsageMode uint8

const (
	ModeUnverified UsageMode = iota
	ModeCumulative
)

type ModelContract struct {
	Unit                                   UsageUnit
	Mode                                   UsageMode
	SentenceEndOnly, CompatibilityDuration bool
}

var errTaskContract = errors.New("dashscope inference: task contract not implemented")

type modelSpec struct {
	contract        ModelContract
	task, streaming string
}

// LookupModelContract 是精确 wire 契约表，不是可调用目录；别名也不推断快照后缀。
func LookupModelContract(model string) ModelContract { return lookupModel(model).contract }

// 官方依据（2026-10-04 核对，研究索引 docs/research/2026-10-04-dashscope-inference-s3-contract.md）：
// https://help.aliyun.com/zh/model-studio/qwen-audio-asr-streaming-server-events
// https://help.aliyun.com/zh/model-studio/qwen-asr-message-server-events
// https://help.aliyun.com/zh/model-studio/cosyvoice-server-events
// https://help.aliyun.com/zh/model-studio/qwen-audio-tts-server-events
// https://help.aliyun.com/zh/model-studio/sambert-server-events
// https://help.aliyun.com/zh/model-studio/paraformer-server-events
// https://help.aliyun.com/zh/model-studio/fun-asr-server-events
// 未证实累计口径的型号保留原单位，但不能据此发布权威数字。
func lookupModel(model string) modelSpec {
	switch model {
	case "qwen-audio-3.0-asr-flash-streaming":
		return modelSpec{ModelContract{Unit: UnitSeconds, Mode: ModeCumulative}, "asr", "duplex"}
	case "qwen-audio-3.1-asr-flash-streaming", "qwen-audio-3.1-asr-flash-message":
		return modelSpec{ModelContract{Unit: UnitTokens, Mode: ModeCumulative, CompatibilityDuration: true}, "asr", "duplex"}
	case "cosyvoice-v1", "cosyvoice-v2", "cosyvoice-v3-plus", "cosyvoice-v3-flash", "cosyvoice-v3.5-plus", "cosyvoice-v3.5-flash",
		"qwen-audio-3.0-tts-plus", "qwen-audio-3.0-tts-flash":
		return modelSpec{ModelContract{Unit: UnitCharacters, Mode: ModeCumulative, SentenceEndOnly: true}, "tts", "duplex"}
	case "sambert-zhichu-v1":
		return modelSpec{ModelContract{Unit: UnitCharacters, Mode: ModeCumulative}, "tts", "out"}
	case "paraformer-realtime-v1", "paraformer-realtime-v2", "paraformer-realtime-8k-v1", "paraformer-realtime-8k-v2",
		"fun-asr-realtime", "fun-asr-realtime-2025-11-07", "fun-asr-realtime-2026-02-28", "fun-asr-realtime-2025-09-15",
		"fun-asr-flash-8k-realtime", "fun-asr-flash-8k-realtime-2026-01-28":
		return modelSpec{ModelContract{Unit: UnitSeconds, Mode: ModeUnverified}, "asr", "duplex"}
	case "qwen-audio-3.1-tts-flash":
		return modelSpec{ModelContract{Unit: UnitTokens, Mode: ModeUnverified}, "tts", "duplex"}
	default:
		return modelSpec{contract: ModelContract{Unit: UnitUnknown, Mode: ModeUnverified}}
	}
}

// ValidateTaskContract 只准入已实现的任务三元组；未知模型不借名称猜绑定或路由。
// streaming 依据各族客户端事件，Sambert 只支持 out，其余已列型号仅 duplex：
// https://help.aliyun.com/zh/model-studio/sambert-client-events
// https://help.aliyun.com/zh/model-studio/cosyvoice-client-events
// https://help.aliyun.com/zh/model-studio/qwen-audio-tts-client-events
// https://help.aliyun.com/zh/model-studio/qwen-audio-asr-streaming-client-events
// https://help.aliyun.com/zh/model-studio/qwen-asr-message-client-events
// https://help.aliyun.com/zh/model-studio/paraformer-client-events
// https://help.aliyun.com/zh/model-studio/fun-asr-client-events
func ValidateTaskContract(f ClientFacts) error {
	b := f.Binding
	if f.Action != "run-task" || !validTaskID(f.TaskID) {
		return errTaskContract
	}
	for _, field := range [...]struct {
		text  TextField
		limit int
	}{{b.Streaming, 16}, {b.Model, maxIDBytes}, {b.TaskGroup, 128}, {b.Task, 128}, {b.Function, 128}} {
		if field.text.Presence != Value || !identityText(field.text.Value, field.limit) {
			return errTaskContract
		}
	}
	if b.TaskGroup.Value != "audio" {
		return errTaskContract
	}
	switch b.Task.Value {
	case "asr":
		if b.Function.Value != "recognition" || b.Streaming.Value != "duplex" {
			return errTaskContract
		}
	case "tts":
		if b.Function.Value != "SpeechSynthesizer" || b.Streaming.Value != "duplex" && b.Streaming.Value != "out" {
			return errTaskContract
		}
	default:
		return errTaskContract
	}
	// 这两种任务有独立就绪/终态契约，不能借合法 audio 三元组混入未知型号保全。
	if b.Model.Value == "multimodal-dialog" || b.Model.Value == "tingwu-meeting-realtime" {
		return errTaskContract
	}
	spec := lookupModel(b.Model.Value)
	if spec.task != "" && (b.Task.Value != spec.task || b.Streaming.Value != spec.streaming) {
		return errTaskContract
	}
	return nil
}
