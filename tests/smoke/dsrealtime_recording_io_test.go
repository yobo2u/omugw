//go:build smoke

package smoke_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	dsPublicText = "请描述图片中的颜色和形状。"
	dsMaxAudio   = 16000 * 2 * 8
	dsMaxMessage = 1 << 20
	dsMaxTrace   = 1 << 20
	dsMaxRecords = 500
)

type dsConfig struct {
	Root, Output, URL, Model, Scenario, Key string
	Duration                                time.Duration
	Responses                               int
	Sample                                  *dsAudioSample
}

// 配置必须完整且目标为官方直连端点，防止把网关输出拿来证明网关正确。
func dsRecordConfig(root string, get func(string) string) (dsConfig, bool, error) {
	var c dsConfig
	if get("OMUGW_RECORD_DSREALTIME") != "1" {
		return c, false, nil
	}
	c = dsConfig{Root: filepath.Join(root, ".local/recordings/dsrealtime"), Output: get("OMUGW_RECORD_OUTPUT"), URL: get("OMUGW_SMOKE_WS_URL"), Model: get("OMUGW_SMOKE_MODEL_REALTIME"), Scenario: get("OMUGW_RECORD_SCENARIO"), Key: get("DASHSCOPE_API_KEY"), Duration: 60 * time.Second, Responses: 1}
	if c.Key == "" || strings.ContainsAny(c.Key, "\r\n\x00\t ") {
		return c, true, errors.New("DASHSCOPE_API_KEY 缺失或格式非法")
	}
	u, err := url.Parse(c.URL)
	if err != nil || u.Scheme != "wss" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Path != "/api-ws/v1/realtime" || u.RawPath != "" ||
		(u.Host != "dashscope.aliyuncs.com" && u.Host != "dashscope-intl.aliyuncs.com" && u.Host != "dashscope-us.aliyuncs.com") {
		return c, true, errors.New("OMUGW_SMOKE_WS_URL 必须是无 query 的官方 Realtime wss 端点")
	}
	if !regexp.MustCompile(`^qwen[a-z0-9.-]{1,100}realtime(?:-[0-9-]+)?$`).MatchString(c.Model) {
		return c, true, errors.New("必须显式指定安全的 Realtime 模型名")
	}
	switch c.Scenario {
	case "tts-commit", "tts-server-commit":
		if !strings.Contains(c.Model, "tts") {
			return c, true, errors.New("TTS 场景需要 TTS 模型")
		}
	case "text-tools":
		c.Responses = 3
	case "audio-image", "vad-interrupt":
		sample, err := dsLoadSample(get("OMUGW_RECORD_AUDIO_SAMPLE"))
		if err != nil {
			return c, true, err
		}
		c.Sample = &sample
	default:
		return c, true, errors.New("必须显式选择一个录制场景")
	}
	if !strings.HasPrefix(c.Scenario, "tts-") && !strings.Contains(c.Model, "qwen3.5-omni-") {
		return c, true, errors.New("Omni 场景需要支持新式音频格式的 qwen3.5-omni 模型")
	}
	if !filepath.IsAbs(c.Output) || !dsOutputRelative(c.Root, c.Output) {
		return c, true, errors.New("输出必须是 ignored 录制根下的 batch/run 新目录")
	}
	return c, true, nil
}

func dsOutputRelative(root, out string) bool {
	rel, err := filepath.Rel(root, out)
	parts := strings.Split(rel, string(filepath.Separator))
	if err != nil || len(parts) != 2 {
		return false
	}
	for _, p := range parts {
		if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`).MatchString(p) {
			return false
		}
	}
	return true
}

// 同批次只保留八个不可回收的尝试槽；失败也占槽，避免重跑测试偷偷放大花费。
func dsReserveOutput(root, out string) error {
	if !dsOutputRelative(root, out) {
		return errors.New("输出目录越界")
	}
	batch := filepath.Dir(out)
	plain := []string{root, batch}
	if filepath.Base(root) == "dsrealtime" && filepath.Base(filepath.Dir(root)) == "recordings" && filepath.Base(filepath.Dir(filepath.Dir(root))) == ".local" {
		plain = append(plain, filepath.Dir(root), filepath.Dir(filepath.Dir(root)))
	}
	for _, dir := range plain {
		info, err := os.Lstat(dir)
		if err == nil && !info.IsDir() {
			return errors.New("录制目录不得是链接或非目录")
		}
		if err != nil && !os.IsNotExist(err) {
			return errors.New("无法核验录制目录")
		}
	}
	if err := os.MkdirAll(batch, 0700); err != nil {
		return errors.New("无法建立候选目录")
	}
	real, err := filepath.EvalSymlinks(batch)
	// macOS 的 /var 是系统级链接，比较时只拒绝根内的链接。
	realRoot, rootErr := filepath.EvalSymlinks(root)
	rel, _ := filepath.Rel(root, batch)
	if err != nil || rootErr != nil || real != filepath.Join(realRoot, rel) {
		return errors.New("候选目录不得跟随符号链接")
	}
	if err := os.Mkdir(out, 0700); err != nil {
		return errors.New("候选目录已存在或无法独占创建")
	}
	for i := 1; i <= 8; i++ {
		f, err := os.OpenFile(filepath.Join(batch, fmt.Sprintf(".attempt-%d", i)), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err == nil {
			return f.Close()
		}
		if !os.IsExist(err) {
			return errors.New("无法保留录制次数槽")
		}
	}
	return errors.New("本批次八次尝试已用完")
}

func dsWriteExclusive(dir, name string, data []byte) error {
	if filepath.Base(name) != name {
		return errors.New("输出文件名非法")
	}
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("输出已存在或不能独占创建")
	}
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		return errors.New("候选写入失败")
	}
	return nil
}

func dsSafeHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	for name, value := range map[string]string{"Upgrade": "websocket", "Connection": "Upgrade"} {
		if len(h.Values(name)) == 1 && strings.EqualFold(h.Get(name), value) {
			out[strings.ToLower(name)] = value
		}
	}
	return out
}

// 消息只来源于固定公开样本；遇到鉴权回显就整条拒收，不能改字节后仍声称原样录制。
func dsSafePayload(b []byte, key string) bool {
	if key != "" && bytes.Contains(b, []byte(key)) {
		return false
	}
	// map 解码会覆盖重复键；先逐 token 查凭据，避免被覆盖的转义值仍泄漏到原始轨迹。
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.UseNumber()
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return false
		}
		if value, ok := token.(string); ok && key != "" && strings.Contains(value, key) {
			return false
		}
	}
	var v any
	if json.Unmarshal(b, &v) != nil {
		return false
	}
	var safe func(any, int) bool
	safe = func(v any, depth int) bool {
		if depth > 48 {
			return false
		}
		switch x := v.(type) {
		case string:
			return key == "" || !strings.Contains(x, key)
		case []any:
			for _, e := range x {
				if !safe(e, depth+1) {
					return false
				}
			}
		case map[string]any:
			for k, e := range x {
				switch strings.ToLower(strings.ReplaceAll(k, "-", "_")) {
				case "authorization", "api_key", "cookie", "set_cookie", "access_token", "secret":
					return false
				}
				if !safe(e, depth+1) {
					return false
				}
			}
		}
		return true
	}
	return safe(v, 0)
}

func dsDigest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

type dsAudioSample struct {
	Origin     string `json:"origin"`
	File       string `json:"file"`
	Format     string `json:"format"`
	Rate       int    `json:"sample_rate"`
	Channels   int    `json:"channels"`
	Transcript string `json:"transcript"`
	SHA256     string `json:"sha256"`
	Data       []byte `json:"-"`
}

// 不接受任意个人录音；只有有格式、短公开句及哈希账本的落盘 PCM 才可再次上传。
func dsLoadSample(path string) (dsAudioSample, error) {
	var s dsAudioSample
	b, err := dsReadBounded(path, 4096)
	if err != nil || json.Unmarshal(b, &s) != nil {
		return s, errors.New("缺少合法音频样本账本")
	}
	if s.Origin != "dsrealtime-tts-public-sample-v1" || s.File != "audio.pcm" || s.Format != "pcm_s16le" || s.Rate != 16000 || s.Channels != 1 || s.Transcript != dsPublicText {
		return s, errors.New("音频来源、公开短句或格式不符合录制约束")
	}
	s.Data, err = dsReadBounded(filepath.Join(filepath.Dir(path), s.File), dsMaxAudio)
	if err != nil || len(s.Data) == 0 || len(s.Data)%2 != 0 || dsDigest(s.Data) != s.SHA256 {
		return s, errors.New("音频缺失、超八秒或摘要不符")
	}
	return s, nil
}

func dsReadBounded(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("样本必须是有界普通文件")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("不能读取样本")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() {
		return nil, errors.New("样本读取身份改变")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, errors.New("样本读取超限")
	}
	return b, nil
}

func dsSaveAudio(dir string, r dsRecording) error {
	if !r.Confirmed || r.Failure != "" || !strings.HasPrefix(r.Scenario, "tts-") || len(r.Audio) == 0 || len(r.Audio) > dsMaxAudio || len(r.Audio)%2 != 0 {
		return errors.New("没有可导出的已确认有界 TTS PCM")
	}
	s := dsAudioSample{Origin: "dsrealtime-tts-public-sample-v1", File: "audio.pcm", Format: "pcm_s16le", Rate: 16000, Channels: 1, Transcript: dsPublicText, SHA256: dsDigest(r.Audio)}
	b, _ := json.MarshalIndent(s, "", "  ")
	if err := dsWriteExclusive(dir, s.File, r.Audio); err != nil {
		return err
	}
	return dsWriteExclusive(dir, "audio-sample.json", b)
}
