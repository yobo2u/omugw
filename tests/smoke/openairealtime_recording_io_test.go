package smoke_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	oaURL        = "wss://api.openai.com/v1/realtime"
	oaModel      = "gpt-realtime-2.1"
	oaMaxBytes   = 1 << 20
	oaMaxAudio   = 24000 * 2 * 8
	oaMaxRecords = 500
)

type oaConfig struct {
	Root, Output, URL, Model, Scenario, Key string
	Duration                                time.Duration
	Sample                                  *oaSample
}

func oaRecordConfig(root string, get func(string) string) (oaConfig, bool, error) {
	var c oaConfig
	if get("OMUGW_RECORD_OPENAI_REALTIME") != "1" {
		return c, false, nil
	}
	c = oaConfig{Root: filepath.Join(root, ".local/recordings/openairealtime"), Output: get("OMUGW_RECORD_OUTPUT"), URL: get("OMUGW_SMOKE_WS_URL"), Model: get("OMUGW_SMOKE_MODEL_REALTIME"), Scenario: get("OMUGW_RECORD_SCENARIO"), Duration: 60 * time.Second}
	a, b := get("OMUGW_SMOKE_OPENAI_KEY"), get("OPENAI_API_KEY")
	if a != "" && b != "" && a != b {
		return c, true, errors.New("两种 OpenAI 凭据来源冲突")
	}
	c.Key = a
	if c.Key == "" {
		c.Key = b
	}
	if c.Key == "" || strings.IndexFunc(c.Key, func(r rune) bool { return r <= 32 || r >= 127 }) >= 0 {
		return c, true, errors.New("OpenAI 凭据缺失或格式非法")
	}
	if c.URL != oaURL || c.Model != oaModel {
		return c, true, errors.New("必须显式指定固定官方无 query 端点及 gpt-realtime-2.1")
	}
	if !filepath.IsAbs(c.Output) || !oaOutputRelative(c.Root, c.Output) {
		return c, true, errors.New("输出必须是私有录制根下独占 batch/run")
	}
	switch c.Scenario {
	case "text-tools-vision":
	case "audio-manual", "vad-interrupt":
		s, err := oaLoadSample(get("OMUGW_RECORD_AUDIO_SAMPLE"))
		if err != nil {
			return c, true, err
		}
		c.Sample = &s
		metadata, _ := json.Marshal(s)
		if !oaSafeJSON(metadata, c.Key) || bytes.Contains(s.Data, []byte(c.Key)) {
			return c, true, errors.New("公开样本安全检查失败")
		}
	default:
		return c, true, errors.New("必须显式选择单个录制场景")
	}
	return c, true, nil
}

func oaOutputRelative(root, out string) bool {
	r, err := filepath.Rel(root, out)
	if err != nil {
		return false
	}
	p := strings.Split(r, string(filepath.Separator))
	if len(p) != 2 {
		return false
	}
	for _, v := range p {
		if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`).MatchString(v) {
			return false
		}
	}
	return true
}

// 槽文件独占且永不回收；只操作 OpenAI 根，不借用 S1 的八个已消耗槽。
func oaReserve(root, out string) error {
	if !oaOutputRelative(root, out) {
		return errors.New("录制目录越界")
	}
	for _, dir := range []string{filepath.Dir(filepath.Dir(root)), filepath.Dir(root), root, filepath.Dir(out)} {
		info, err := os.Lstat(dir)
		if err == nil && !info.IsDir() {
			return errors.New("录制目录不得为链接或文件")
		}
		if err != nil && !os.IsNotExist(err) {
			return errors.New("录制目录无法核验")
		}
	}
	if err := os.MkdirAll(filepath.Dir(out), 0700); err != nil {
		return errors.New("不能创建录制目录")
	}
	if err := os.Mkdir(out, 0700); err != nil {
		return errors.New("录制目录必须独占")
	}
	for i := 1; i <= 8; i++ {
		f, err := os.OpenFile(filepath.Join(filepath.Dir(out), fmt.Sprintf(".attempt-%d", i)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err == nil {
			return f.Close()
		}
		if !os.IsExist(err) {
			return errors.New("不能独占尝试槽")
		}
	}
	return errors.New("本批次八次不可回收槽已耗尽")
}

func oaWrite(dir, name string, b []byte) error {
	if filepath.Base(name) != name {
		return errors.New("非法文件名")
	}
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("输出已存在或不能独占创建")
	}
	_, we := f.Write(b)
	ce := f.Close()
	if we != nil || ce != nil {
		return errors.New("写入失败")
	}
	return nil
}

// 逐 token 拒绝重复键和秘密字段，防止 map 覆盖旧值及 JSON 转义绕过隐私检查。
func oaSafeJSON(b []byte, key string) bool {
	if len(b) > oaMaxBytes || !json.Valid(b) || key != "" && bytes.Contains(b, []byte(key)) {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var scan func(int) bool
	safe := func(s string) bool { return key == "" || !strings.Contains(s, key) }
	scan = func(depth int) bool {
		if depth > 48 {
			return false
		}
		token, err := d.Token()
		if err != nil {
			return false
		}
		switch v := token.(type) {
		case string:
			return safe(v)
		case json.Delim:
			switch v {
			case '{':
				seen := map[string]bool{}
				for d.More() {
					t, err := d.Token()
					k, ok := t.(string)
					if err != nil || !ok || !safe(k) || seen[k] {
						return false
					}
					seen[k] = true
					switch strings.ToLower(strings.ReplaceAll(k, "-", "_")) {
					case "authorization", "api_key", "apikey", "cookie", "set_cookie", "access_token", "secret", "client_secret":
						return false
					}
					if !scan(depth + 1) {
						return false
					}
				}
			case '[':
				for d.More() {
					if !scan(depth + 1) {
						return false
					}
				}
			default:
				return false
			}
			_, err = d.Token()
			return err == nil
		}
		return true
	}
	return scan(0)
}

func oaDigest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

type oaSample struct {
	File       string `json:"file"`
	Origin     string `json:"origin"`
	License    string `json:"license"`
	Public     bool   `json:"public"`
	Format     string `json:"format"`
	Rate       int    `json:"sample_rate"`
	Channels   int    `json:"channels"`
	Transcript string `json:"transcript"`
	SHA256     string `json:"sha256"`
	Data       []byte `json:"-"`
}

func oaLoadSample(path string) (oaSample, error) {
	var s oaSample
	b, err := oaReadBounded(path, 4096)
	if err != nil || !oaSafeJSON(b, "") || json.Unmarshal(b, &s) != nil {
		return s, errors.New("缺少合法公开音频元数据")
	}
	u, err := url.Parse(s.Origin)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !s.Public || s.License == "" || s.Format != "pcm_s16le" || s.Rate != 24000 || s.Channels != 1 || len(s.Transcript) == 0 || len(s.Transcript) > 1024 || filepath.Base(s.File) != s.File || s.File == "." {
		return s, errors.New("音频必须是来源明确的公开 24k 单声道 PCM 样本")
	}
	s.Data, err = oaReadBounded(filepath.Join(filepath.Dir(path), s.File), oaMaxAudio)
	if err != nil || len(s.Data) == 0 || len(s.Data)%2 != 0 || oaDigest(s.Data) != s.SHA256 {
		return s, errors.New("音频摘要不符、空白或超八秒")
	}
	return s, nil
}

func oaReadBounded(path string, max int64) ([]byte, error) {
	i, err := os.Lstat(path)
	if err != nil || !i.Mode().IsRegular() || i.Size() > max {
		return nil, errors.New("样本必须是有界普通文件")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("无法读取样本")
	}
	defer f.Close()
	j, err := f.Stat()
	if err != nil || !os.SameFile(i, j) || !j.Mode().IsRegular() {
		return nil, errors.New("样本身份改变")
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil || int64(len(b)) > max {
		return nil, errors.New("样本超限")
	}
	return b, nil
}

func oaMap(m map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		m, _ = m[k].(map[string]any)
	}
	return m
}
func oaString(m map[string]any, keys ...string) string {
	s, _ := oaMap(m, keys[:len(keys)-1]...)[keys[len(keys)-1]].(string)
	return s
}
func oaMatches(want, got map[string]any) bool {
	for k, w := range want {
		g, ok := got[k]
		if !ok {
			return false
		}
		if m, ok := w.(map[string]any); ok {
			gm, ok := g.(map[string]any)
			if !ok || !oaMatches(m, gm) {
				return false
			}
		} else {
			a, _ := json.Marshal(w)
			b, _ := json.Marshal(g)
			if !bytes.Equal(a, b) {
				return false
			}
		}
	}
	return true
}
