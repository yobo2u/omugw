package gateway

import (
	"github.com/yobo2u/omugw/internal/obs"
	"github.com/yobo2u/omugw/internal/protocol/openairealtime"
)

type openAIWSObserver struct {
	usage                                    *wsUsage
	transcriptionKnown, transcriptionEnabled bool
}

func (o *openAIWSObserver) Observe(payload []byte) error {
	if o.usage != nil && o.usage.finished {
		return errWSUsageFinished
	}
	e, err := openairealtime.Inspect(payload)
	if err != nil {
		return errWSRelayPolicy
	}
	if o.usage == nil {
		return nil
	}
	if e.Source == "session" {
		// 只信上游的完整有效配置；未知回显必须清除旧 enabled，不能沿用猜测。
		o.transcriptionKnown = e.Transcription != nil
		o.transcriptionEnabled = o.transcriptionKnown && *e.Transcription
		if !o.transcriptionKnown {
			o.usage.diagnostic("transcription_config_unknown")
		}
	}
	if e.Type == "input_audio_buffer.committed" && !o.transcriptionEnabled {
		if !o.transcriptionKnown {
			o.usage.diagnostic("transcription_config_unknown")
		}
		return nil
	}
	return o.usage.Observe(openAIUsageEvent(e))
}

func (o *openAIWSObserver) Finish() {
	if o.usage != nil {
		o.usage.Finish()
	}
}

func openAIUsageEvent(e openairealtime.Event) wsUsageEvent {
	v := wsUsageEvent{
		Source: e.Source, ID: e.ID, Started: e.Started, Terminal: e.Terminal, ItemPending: e.ItemPending,
		Usage: e.Usage, Seconds: e.Seconds, Diagnostic: e.Diagnostic, Failure: e.Failure,
		Details: obs.WSTokenDetails{
			TextInput: wsTokenCount(e.Details.TextInput), AudioInput: wsTokenCount(e.Details.AudioInput), ImageInput: wsTokenCount(e.Details.ImageInput),
			CachedInput: wsTokenCount(e.Details.CachedInput), CachedTextInput: wsTokenCount(e.Details.CachedTextInput),
			CachedAudioInput: wsTokenCount(e.Details.CachedAudioInput), CachedImageInput: wsTokenCount(e.Details.CachedImageInput),
			TextOutput: wsTokenCount(e.Details.TextOutput), AudioOutput: wsTokenCount(e.Details.AudioOutput),
		},
	}
	if e.ContentIndex != nil {
		v.Part, v.HasPart = *e.ContentIndex, true
	}
	return v
}

func wsTokenCount(n *int64) obs.WSTokenCount {
	if n == nil {
		return obs.WSTokenCount{}
	}
	return obs.WSTokenCount{Value: *n, Present: true}
}
