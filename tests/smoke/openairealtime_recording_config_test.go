package smoke_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenAIRealtimeRecorderOfflineConfig(t *testing.T) {
	root := t.TempDir()
	if _, on, err := oaRecordConfig(root, func(k string) string {
		if k != "OMUGW_RECORD_OPENAI_REALTIME" {
			t.Fatal("默认关闭仍读取凭据")
		}
		return ""
	}); on || err != nil {
		t.Fatal("默认未关闭")
	}
	env := map[string]string{
		"OMUGW_RECORD_OPENAI_REALTIME": "1", "OPENAI_API_KEY": "offline-secret",
		"OMUGW_RECORD_SCENARIO": "text-tools-vision", "OMUGW_SMOKE_WS_URL": "wss://api.openai.com/v1/realtime",
		"OMUGW_SMOKE_MODEL_REALTIME": "gpt-realtime-2.1", "OMUGW_RECORD_OUTPUT": filepath.Join(root, ".local/recordings/openairealtime/batch/run"),
	}
	get := func(k string) string { return env[k] }
	c, on, err := oaRecordConfig(root, get)
	if err != nil || !on || c.Duration != 60*time.Second {
		t.Fatalf("配置失败: %v", err)
	}
	for _, tc := range []struct{ key, value string }{
		{"OMUGW_SMOKE_OPENAI_KEY", "other-secret"}, {"OPENAI_API_KEY", "bad\nkey"},
		{"OMUGW_SMOKE_WS_URL", "wss://api.openai.com/v1/realtime?"},
		{"OMUGW_SMOKE_WS_URL", "wss://api.openai.com/v1/realtime#"},
		{"OMUGW_SMOKE_WS_URL", "wss://evil.invalid/v1/realtime"},
		{"OMUGW_SMOKE_MODEL_REALTIME", "gpt-realtime"}, {"OMUGW_RECORD_SCENARIO", "all"},
		{"OMUGW_RECORD_OUTPUT", filepath.Join(root, "testdata/routes/run")},
	} {
		old := env[tc.key]
		env[tc.key] = tc.value
		if _, _, err := oaRecordConfig(root, get); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("未安全拒绝 %s", tc.key)
		}
		env[tc.key] = old
	}
}

func TestOpenAIRealtimeRecorderOfflineSlotsAndPrivacy(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".local/recordings/openairealtime")
	for i := 0; i < 8; i++ {
		out := filepath.Join(root, "batch", string(rune('a'+i)))
		if err := oaReserve(root, out); err != nil {
			t.Fatal(err)
		}
		if err := oaReserve(root, out); err == nil {
			t.Fatal("重复目录")
		}
		if err := os.Remove(out); err != nil {
			t.Fatal(err)
		}
	}
	if err := oaReserve(root, filepath.Join(root, "batch", "ninth")); err == nil {
		t.Fatal("删除目录回收了槽")
	}
	for _, raw := range []string{
		`{"type":"error","message":"offline\u002dsecret"}`,
		`{"type":"error","message":"offline\u002dsecret","message":"public"}`,
		`{"type":"error","\u0061pi_key":"other"}`, `{"Authorization":"other","Authorization":null}`,
		`{"a":1,"a":2}`, `{"type":"error","cookie":"other"}`,
	} {
		if oaSafeJSON([]byte(raw), "offline-secret") {
			t.Fatal("秘密/重复键进入轨迹")
		}
	}
	if !oaSafeJSON([]byte(`{"type":"session.created"}`), "offline-secret") {
		t.Fatal("公开消息被拒绝")
	}
}

func TestOpenAIRealtimeRecorderOfflineSample(t *testing.T) {
	dir := t.TempDir()
	data := []byte{1, 2, 3, 4}
	s := oaSample{File: "public.pcm", Origin: "https://example.org/public-audio", License: "CC0-1.0", Public: true, Format: "pcm_s16le", Rate: 24000, Channels: 1, Transcript: "公开测试。", SHA256: oaDigest(data)}
	if err := os.WriteFile(filepath.Join(dir, s.File), data, 0600); err != nil {
		t.Fatal(err)
	}
	write := func() {
		b, _ := json.Marshal(s)
		if err := os.WriteFile(filepath.Join(dir, "sample.json"), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	if _, err := oaLoadSample(filepath.Join(dir, "sample.json")); err != nil {
		t.Fatal(err)
	}
	s.Public = false
	write()
	if _, err := oaLoadSample(filepath.Join(dir, "sample.json")); err == nil {
		t.Fatal("非公开音频")
	}
	s.Public = true
	s.Rate = 16000
	write()
	if _, err := oaLoadSample(filepath.Join(dir, "sample.json")); err == nil {
		t.Fatal("错采样率")
	}
	s.Rate = 24000
	s.SHA256 = strings.Repeat("0", 64)
	write()
	if _, err := oaLoadSample(filepath.Join(dir, "sample.json")); err == nil {
		t.Fatal("摘要不符")
	}
}

func TestOpenAIRealtimeRecorderOfflineSampleMetadataSecret(t *testing.T) {
	root := t.TempDir()
	data := []byte{1, 2, 3, 4}
	s := oaSample{File: "public.pcm", Origin: "https://example.org/public-audio", License: "CC0-1.0", Public: true, Format: "pcm_s16le", Rate: 24000, Channels: 1, Transcript: "offline-secret", SHA256: oaDigest(data)}
	b, _ := json.Marshal(s)
	if err := os.WriteFile(filepath.Join(root, "sample.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "public.pcm"), data, 0600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"OMUGW_RECORD_OPENAI_REALTIME": "1", "OPENAI_API_KEY": "offline-secret", "OMUGW_RECORD_SCENARIO": "audio-manual", "OMUGW_SMOKE_WS_URL": oaURL, "OMUGW_SMOKE_MODEL_REALTIME": oaModel, "OMUGW_RECORD_OUTPUT": filepath.Join(root, ".local/recordings/openairealtime/batch/run"), "OMUGW_RECORD_AUDIO_SAMPLE": filepath.Join(root, "sample.json")}
	if _, _, err := oaRecordConfig(root, func(k string) string { return env[k] }); err == nil {
		t.Fatal("样本元数据泄漏凭据")
	}
}
