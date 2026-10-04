package smoke_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	inferenceBatch        = "s3-audio-20261004"
	inferenceDuration     = 45 * time.Second
	inferenceMessageLimit = 1 << 20
	inferenceTraceLimit   = 16 << 20
	inferenceAudioLimit   = 8 << 20
	inferenceNodeLimit    = 1024
	inferenceSampleLimit  = 3 * 16000 * 2
	inferenceTextA        = "这是网关测试。"
	inferenceTextB        = "请确认声音清晰。"
)

var errInferenceDisabled = errors.New("Inference 专用录制开关未开启")

type inferenceRecordingSlot struct {
	Slot                                                                              int
	Model, Voice                                                                      string
	MaxTasks, MaxInputAudioSeconds, MaxInputCharacters, WorstCaseTokens, WorstCaseFen int64
}

type inferenceRecordingManifest struct {
	Batch, Region, Endpoint, SamplePath, SampleSHA256, PriceSource, PriceCheckedAt string
	WorstCaseFen                                                                   int64
	Slots                                                                          [6]inferenceRecordingSlot
	CostEvidence                                                                   []inferenceCostEvidence `json:",omitempty"`
}

type inferenceRecordingConfig struct {
	Root, Output, Batch, Scenario, Endpoint, Model, Voice, SamplePath, SampleSHA256, Key, ManifestPath string
	Slot                                                                                               int
	Manifest                                                                                           inferenceRecordingManifest
	// 仅本地测试注入；live 入口无条件拒绝，不能靠合成数字签署费用授权。
	synthetic bool
}

func inferenceScenarioName(slot int) string {
	if slot < 1 || slot > 6 {
		return ""
	}
	return [6]string{"asr-two-tasks", "tts-two-tasks", "sambert-out", "asr-message-tokens", "asr-interrupted", "invalid-voice"}[slot-1]
}

func inferenceRecordConfig(root string, getenv func(string) string) (inferenceRecordingConfig, error) {
	var c inferenceRecordingConfig
	if getenv("OMUGW_RECORD_DS_INFERENCE") != "1" {
		return c, errInferenceDisabled
	}
	slot, err := strconv.Atoi(getenv("OMUGW_DS_INFERENCE_SLOT"))
	if err != nil || slot < 1 || slot > 6 {
		return c, errors.New("必须显式指定1到6单槽")
	}
	c = inferenceRecordingConfig{Root: root, Batch: inferenceBatch, Slot: slot, Scenario: inferenceScenarioName(slot), Output: getenv("OMUGW_DS_INFERENCE_OUTPUT"), ManifestPath: getenv("OMUGW_DS_INFERENCE_MANIFEST")}
	b, err := inferenceReadFile(c.ManifestPath, inferenceManifestLimit)
	if err != nil {
		return c, errors.New("缺少有界绝对路径 manifest")
	}
	v, err := inferenceJSON(b, "")
	if err != nil {
		return c, errors.New("manifest JSON 不合法")
	}
	m, ok := v.(map[string]any)
	if !ok {
		return c, errors.New("manifest 必须为对象")
	}
	slots, ok := m["Slots"].([]any)
	if !ok || len(slots) != 6 {
		return c, errors.New("manifest 必须恰好六槽")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&c.Manifest) != nil {
		return c, errors.New("manifest 字段不合法")
	}
	c.Endpoint, c.SamplePath, c.SampleSHA256 = c.Manifest.Endpoint, c.Manifest.SamplePath, c.Manifest.SampleSHA256
	c.Model, c.Voice = c.Manifest.Slots[slot-1].Model, c.Manifest.Slots[slot-1].Voice
	// 在缺费用证明时连凭据都不读，阻止用已有key补齐未知地域与费用前提。
	if err := inferenceValidate(c); err != nil {
		return c, err
	}
	c.Key = getenv("DASHSCOPE_API_KEY")
	if !inferenceKeyValid(c.Key) {
		return c, errors.New("凭据缺失或格式不合法")
	}
	return c, nil
}

func inferenceKeyValid(s string) bool {
	return len(s) >= 8 && len(s) <= 4096 && strings.IndexFunc(s, func(r rune) bool { return r <= 32 || r >= 127 }) < 0
}

func inferenceValidate(c inferenceRecordingConfig) error {
	m := c.Manifest
	if !filepath.IsAbs(c.Root) || filepath.Clean(c.Root) != c.Root || c.Batch != inferenceBatch || c.Slot < 1 || c.Slot > 6 || c.Scenario != inferenceScenarioName(c.Slot) || c.Output != filepath.Join(c.Root, ".local/ws-recordings", inferenceBatch, fmt.Sprintf("slot-%d", c.Slot)) {
		return errors.New("固定批次、槽或独占路径不符")
	}
	if m.Batch != inferenceBatch || m.Region != "cn-beijing" || c.Endpoint != m.Endpoint || !regexp.MustCompile(`^wss://[a-z0-9][a-z0-9-]{0,62}\.cn-beijing\.maas\.aliyuncs\.com/api-ws/v1/inference$`).MatchString(m.Endpoint) {
		return errors.New("必须明确北京 workspace 无query端点")
	}
	if c.Model != m.Slots[c.Slot-1].Model || c.Voice != m.Slots[c.Slot-1].Voice || c.SamplePath != m.SamplePath || c.SampleSHA256 != m.SampleSHA256 {
		return errors.New("配置与manifest身份不符")
	}
	if !filepath.IsAbs(c.ManifestPath) || !filepath.IsAbs(m.SamplePath) || filepath.Ext(m.SamplePath) != ".pcm" || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(m.SampleSHA256) {
		return errors.New("样本路径或SHA不合法")
	}
	p, err := url.Parse(m.PriceSource)
	if err != nil || p.Scheme != "https" || p.Host != "help.aliyun.com" || p.Path != "/zh/model-studio/model-pricing" || p.RawQuery != "" || p.Fragment != "" || p.User != nil {
		return errors.New("缺少固定官方价格来源")
	}
	checked, err := time.Parse(time.RFC3339, m.PriceCheckedAt)
	if err != nil || checked.After(time.Now()) {
		return errors.New("批次价格参考时间非法")
	}
	models := [6]string{"qwen-audio-3.0-asr-flash-streaming", "qwen-audio-3.0-tts-flash", "sambert-zhichu-v1", "qwen-audio-3.1-asr-flash-message", "qwen-audio-3.1-asr-flash-streaming", "qwen-audio-3.0-tts-flash"}
	voices := [6]string{"", "longanlingxi", "", "", "", "omugw-invalid-voice-s3"}
	tasks := [6]int64{2, 2, 1, 1, 2, 1}
	var sumTasks, sumSeconds, sumAudio, sumText, sumFen int64
	for i, s := range m.Slots {
		if s.Slot != i+1 || s.Model != models[i] || s.Voice != voices[i] || s.MaxTasks != tasks[i] || s.WorstCaseFen < 1 || s.WorstCaseFen > 100 {
			return errors.New("固定模型音色任务数或费用不符")
		}
		asr := i == 0 || i == 3 || i == 4
		if asr {
			if s.MaxInputAudioSeconds < 1 || s.MaxInputAudioSeconds > s.MaxTasks*3 || s.MaxInputCharacters != 0 {
				return errors.New("ASR输入上限不符")
			}
		} else if s.MaxInputAudioSeconds != 0 || s.MaxInputCharacters < int64(utf8.RuneCountInString(inferenceTextA+inferenceTextB))*s.MaxTasks || s.MaxInputCharacters > s.MaxTasks*40 {
			return errors.New("TTS文本上限不符")
		}
		if i == 3 || i == 4 {
			if s.WorstCaseTokens < 0 {
				return errors.New("token费用上界未知或越界")
			}
		} else if s.WorstCaseTokens != 0 {
			return errors.New("非token槽不接受token预算")
		}
		sumTasks += s.MaxTasks
		sumSeconds += 45
		sumAudio += s.MaxInputAudioSeconds
		sumText += s.MaxInputCharacters
		sumFen += s.WorstCaseFen
	}
	if sumTasks > 10 || sumSeconds > 270 || sumAudio > 18 || sumText > 240 || m.WorstCaseFen < sumFen || m.WorstCaseFen > 100 {
		return errors.New("整批预算不足或越界")
	}
	b, err := inferenceReadFile(m.SamplePath, inferenceSampleLimit)
	if err != nil || len(b) < 2 || len(b)%2 != 0 || inferenceSHA(b) != m.SampleSHA256 {
		return errors.New("PCM16LE/16k/mono样本长度或SHA不符")
	}
	for _, i := range []int{0, 3, 4} {
		if int64(len(b))*m.Slots[i].MaxTasks > m.Slots[i].MaxInputAudioSeconds*32000 {
			return errors.New("样本超manifest音频预算")
		}
	}
	if c.Key != "" && (!inferenceKeyValid(c.Key) || bytes.Contains(b, []byte(c.Key))) {
		return errors.New("凭据格式或样本安全检查失败")
	}
	metadata, _ := json.Marshal(m)
	if len(metadata) > inferenceManifestLimit {
		return errors.New("manifest超过独立64KiB上限")
	}
	if _, err := inferenceJSON(metadata, c.Key); err != nil {
		return errors.New("manifest含凭据或不安全字段")
	}
	_, err = inferenceAuthorizeCost(m, c.Slot, time.Now())
	return err
}
