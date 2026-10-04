package dashscopeinference

import (
	"fmt"
	"testing"
)

func TestInferenceRejectsExcludedModels(t *testing.T) {
	for _, model := range []string{"multimodal-dialog", "tingwu-meeting-realtime", "future"} {
		for _, task := range []struct{ name, function, streaming string }{
			{"asr", "recognition", "duplex"},
			{"tts", "SpeechSynthesizer", "duplex"},
			{"tts", "SpeechSynthesizer", "out"},
		} {
			t.Run(model+"/"+task.name+"/"+task.streaming, func(t *testing.T) {
				raw := []byte(fmt.Sprintf(`{"header":{"action":"run-task","task_id":"a","streaming":%q},"payload":{"model":%q,"task_group":"audio","task":%q,"function":%q}}`, task.streaming, model, task.name, task.function))
				facts, err := InspectClient(raw)
				if err != nil {
					t.Fatalf("valid audio envelope rejected: %v", err)
				}
				err = ValidateTaskContract(facts)
				if model == "future" && err != nil {
					t.Fatalf("unknown future model rejected: %v", err)
				}
				if model != "future" && err == nil {
					t.Fatal("excluded model admitted through valid audio binding")
				}
			})
		}
	}
}

func TestInferenceExactContracts(t *testing.T) {
	for _, tc := range []struct {
		models                    []string
		contract                  ModelContract
		task, function, streaming string
	}{
		{[]string{"qwen-audio-3.0-asr-flash-streaming"}, ModelContract{Unit: UnitSeconds, Mode: ModeCumulative}, "asr", "recognition", "duplex"},
		{[]string{"qwen-audio-3.1-asr-flash-streaming", "qwen-audio-3.1-asr-flash-message"}, ModelContract{Unit: UnitTokens, Mode: ModeCumulative, CompatibilityDuration: true}, "asr", "recognition", "duplex"},
		{[]string{"cosyvoice-v1", "cosyvoice-v2", "cosyvoice-v3-plus", "cosyvoice-v3-flash", "cosyvoice-v3.5-plus", "cosyvoice-v3.5-flash", "qwen-audio-3.0-tts-plus", "qwen-audio-3.0-tts-flash"}, ModelContract{Unit: UnitCharacters, Mode: ModeCumulative, SentenceEndOnly: true}, "tts", "SpeechSynthesizer", "duplex"},
		{[]string{"sambert-zhichu-v1"}, ModelContract{Unit: UnitCharacters, Mode: ModeCumulative}, "tts", "SpeechSynthesizer", "out"},
		{[]string{"paraformer-realtime-v1", "paraformer-realtime-v2", "paraformer-realtime-8k-v1", "paraformer-realtime-8k-v2", "fun-asr-realtime", "fun-asr-realtime-2025-11-07", "fun-asr-realtime-2026-02-28", "fun-asr-realtime-2025-09-15", "fun-asr-flash-8k-realtime", "fun-asr-flash-8k-realtime-2026-01-28"}, ModelContract{Unit: UnitSeconds, Mode: ModeUnverified}, "asr", "recognition", "duplex"},
		{[]string{"qwen-audio-3.1-tts-flash"}, ModelContract{Unit: UnitTokens, Mode: ModeUnverified}, "tts", "SpeechSynthesizer", "duplex"},
	} {
		for _, model := range tc.models {
			t.Run(model, func(t *testing.T) {
				if got := LookupModelContract(model); got != tc.contract {
					t.Errorf("contract = %+v; want %+v", got, tc.contract)
				}
				for _, near := range []string{model + "-latest", model + "-2026-10-04", "prefix-" + model} {
					if got := LookupModelContract(near); got != (ModelContract{Unit: UnitUnknown}) {
						t.Errorf("guessed %s: %+v", near, got)
					}
				}
				f := taskFacts(model, tc.task, tc.function, tc.streaming)
				if err := ValidateTaskContract(f); err != nil {
					t.Fatalf("valid binding: %v", err)
				}
				f.Binding.Streaming.Value = "out"
				if tc.streaming == "out" {
					f.Binding.Streaming.Value = "duplex"
				}
				if err := ValidateTaskContract(f); err == nil {
					t.Error("accepted wrong known streaming")
				}
				f = taskFacts(model, "asr", "recognition", "duplex")
				if tc.task == "asr" {
					f = taskFacts(model, "tts", "SpeechSynthesizer", "duplex")
				}
				if err := ValidateTaskContract(f); err == nil {
					t.Error("accepted wrong known task")
				}
			})
		}
	}
	for _, model := range []string{"", "gummy-realtime-v1", "sambert-other-v1", "future-model"} {
		if got := LookupModelContract(model); got != (ModelContract{Unit: UnitUnknown}) {
			t.Errorf("unknown = %+v", got)
		}
	}
	for _, f := range []ClientFacts{taskFacts("future", "asr", "recognition", "duplex"), taskFacts("future", "tts", "SpeechSynthesizer", "duplex"), taskFacts("future", "tts", "SpeechSynthesizer", "out")} {
		if err := ValidateTaskContract(f); err != nil {
			t.Errorf("unknown exact model may preserve valid task: %v", err)
		}
	}
	bad := []ClientFacts{taskFacts("future", "asr", "recognition", "out"), taskFacts("future", "tts", "SpeechSynthesizer", "in"), taskFacts("future", "tts", "speechsynthesizer", "duplex"), taskFacts("future", "multimodal-dialog", "generation", "duplex"), taskFacts("future", "tingwu", "recognition", "duplex"), {}}
	f := taskFacts("future", "asr", "recognition", "duplex")
	f.Binding.TaskGroup.Value = "multimodal"
	bad = append(bad, f)
	for _, presence := range []Presence{Missing, Null, Invalid} {
		f = taskFacts("future", "asr", "recognition", "duplex")
		f.Binding.Model.Presence = presence
		bad = append(bad, f)
	}
	for _, f := range bad {
		if err := ValidateTaskContract(f); err == nil {
			t.Errorf("unsupported binding accepted: %+v", f)
		}
	}
}

func taskFacts(model, task, function, streaming string) ClientFacts {
	return ClientFacts{Action: "run-task", TaskID: "a", Binding: BindingFields{
		Streaming: TextField{Value, streaming}, Model: TextField{Value, model}, TaskGroup: TextField{Value, "audio"}, Task: TextField{Value, task}, Function: TextField{Value, function},
	}}
}
