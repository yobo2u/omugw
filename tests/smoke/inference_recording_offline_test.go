package smoke_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/testkit"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

// 费用数字全部合成，只测门禁算术，不是可供控制器复用的费用授权。
func inferenceOfflineConfig(t *testing.T, slot int) inferenceRecordingConfig {
	t.Helper()
	// macOS的/var为系统链接；测试输入显式使用真实绝对路径，不放宽录制器的拒链接约束。
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sample := filepath.Join(root, "public.pcm")
	if err := os.WriteFile(sample, []byte{1, 0, 2, 0, 3, 0, 4, 0}, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := inferenceReadFile(sample, 96000); err != nil {
		t.Fatalf("离线样本路径 %s: %v", sample, err)
	}
	m := inferenceRecordingManifest{Batch: "s3-audio-20261004", Region: "cn-beijing", Endpoint: "wss://offline-fixture.cn-beijing.maas.aliyuncs.com/api-ws/v1/inference", SamplePath: sample, SampleSHA256: inferenceSHA([]byte{1, 0, 2, 0, 3, 0, 4, 0}), PriceSource: "https://help.aliyun.com/zh/model-studio/model-pricing", PriceCheckedAt: time.Now().UTC().Format(time.RFC3339), WorstCaseFen: 6}
	m.Slots = [6]inferenceRecordingSlot{
		{Slot: 1, Model: "qwen-audio-3.0-asr-flash-streaming", MaxTasks: 2, MaxInputAudioSeconds: 6, WorstCaseFen: 1},
		{Slot: 2, Model: "qwen-audio-3.0-tts-flash", Voice: "longanlingxi", MaxTasks: 2, MaxInputCharacters: 80, WorstCaseFen: 1},
		{Slot: 3, Model: "sambert-zhichu-v1", MaxTasks: 1, MaxInputCharacters: 40, WorstCaseFen: 1},
		{Slot: 4, Model: "qwen-audio-3.1-asr-flash-message", MaxTasks: 1, MaxInputAudioSeconds: 3, WorstCaseTokens: 1000, WorstCaseFen: 1},
		{Slot: 5, Model: "qwen-audio-3.1-asr-flash-streaming", MaxTasks: 2, MaxInputAudioSeconds: 6, WorstCaseTokens: 2000, WorstCaseFen: 1},
		{Slot: 6, Model: "qwen-audio-3.0-tts-flash", Voice: "omugw-invalid-voice-s3", MaxTasks: 1, MaxInputCharacters: 40, WorstCaseFen: 1},
	}
	for slot := 1; slot <= 6; slot++ {
		m.CostEvidence = append(m.CostEvidence, inferenceSyntheticCost(m, slot))
	}
	b, _ := json.Marshal(m)
	mp := filepath.Join(root, "manifest.json")
	if err := os.WriteFile(mp, b, 0600); err != nil {
		t.Fatal(err)
	}
	s := m.Slots[slot-1]
	return inferenceRecordingConfig{Root: root, Output: filepath.Join(root, ".local/ws-recordings/s3-audio-20261004", fmt.Sprintf("slot-%d", slot)), Batch: m.Batch, Slot: slot, Scenario: inferenceScenarioName(slot), Endpoint: m.Endpoint, Model: s.Model, Voice: s.Voice, SamplePath: sample, SampleSHA256: m.SampleSHA256, Key: "offline-secret", ManifestPath: mp, Manifest: m, synthetic: true}
}

func TestInferenceRecorderOfflineLimits(t *testing.T) {
	t.Run("音频实际长度和文本发送前拒绝", func(t *testing.T) {
		for _, size := range []int{0, 3, 96002} {
			c := inferenceOfflineConfig(t, 1)
			b := make([]byte, size)
			if err := os.WriteFile(c.SamplePath, b, 0600); err != nil {
				t.Fatal(err)
			}
			c.SampleSHA256 = inferenceSHA(b)
			c.Manifest.SampleSHA256 = c.SampleSHA256
			if err := inferenceReserve(c); err == nil {
				t.Fatal("不合规PCM长度被接受")
			}
			_, err := inferenceDrive(c, b, func(ws.Opcode, []byte) error { t.Fatal("音频预检前发送"); return nil }, func() (inferenceRecord, error) { t.Fatal("音频预检前接收"); return inferenceRecord{}, nil })
			if err == nil {
				t.Fatal("音频长度绕过driver")
			}
		}
		c := inferenceOfflineConfig(t, 2)
		if _, err := inferenceRequest(c, "continue-task", "s3-2-1", strings.Repeat("字", 41)); err == nil {
			t.Fatal("单任务超过40字符")
		}
	})
	t.Run("非私有批次目录不能承载原文", func(t *testing.T) {
		c := inferenceOfflineConfig(t, 1)
		if err := os.MkdirAll(filepath.Dir(c.Output), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Dir(c.Output), 0755); err != nil {
			t.Fatal(err)
		}
		if err := inferenceReserve(c); err == nil {
			t.Fatal("非0700批次被接受")
		}
	})
	t.Run("一次物理dial且失败不复活", func(t *testing.T) {
		c := inferenceOfflineConfig(t, 1)
		if err := inferenceReserve(c); err != nil {
			t.Fatal(err)
		}
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set("Set-Cookie", "offline-secret")
			w.WriteHeader(401)
			_, _ = w.Write([]byte("offline-secret"))
		}))
		defer srv.Close()
		dial := func(ctx context.Context, _ string, opts ws.DialOptions) (*ws.Conn, *http.Response, error) {
			return ws.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), opts)
		}
		r, err := inferenceCaptureWithDial(context.Background(), c, dial)
		if err == nil || r.Outcome != "handshake_failed" || r.Status != 401 {
			t.Fatalf("未保存失败状态: %v", err)
		}
		if err := inferenceSave(c.Output, r); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(c.Output, "recording.json"))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte("offline-secret")) {
			t.Fatal("握手秘密落盘")
		}
		// 即使人工删除本槽输出，另一个持久dial文件也禁止第二次物理调用。
		if err := os.RemoveAll(c.Output); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(c.Output, 0700); err != nil {
			t.Fatal(err)
		}
		if _, err := inferenceCaptureWithDial(context.Background(), c, dial); err == nil {
			t.Fatal("失败后重拨")
		}
		if calls.Load() != 1 {
			t.Fatalf("物理调用%d", calls.Load())
		}
	})
	t.Run("更短ctx包含reader及关闭join", func(t *testing.T) {
		c := inferenceOfflineConfig(t, 1)
		if err := inferenceReserve(c); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer close(done)
			calls.Add(1)
			conn, err := ws.Accept(w, r, ws.AcceptOptions{MaxPayload: 1 << 20, Idle: time.Second})
			if err != nil {
				return
			}
			finish, _ := conn.ArmCloseDeadline(time.Now().Add(time.Second))
			defer finish()
			_, _, _ = conn.ReadMessage()
			_, _, _ = conn.ReadMessage()
		}))
		defer srv.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
		defer cancel()
		start := time.Now()
		r, err := inferenceCaptureWithDial(ctx, c, func(ctx context.Context, _ string, opts ws.DialOptions) (*ws.Conn, *http.Response, error) {
			d, _ := ctx.Deadline()
			if d.After(start.Add(121 * time.Millisecond)) {
				t.Error("截止被重置")
			}
			return ws.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), opts)
		})
		if err == nil || r.Outcome == "completed" || time.Since(start) > 620*time.Millisecond {
			t.Fatalf("超时/关闭未有界: %v", err)
		}
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("server未退出")
		}
		if calls.Load() != 1 {
			t.Fatal("重拨")
		}
	})
	t.Run("45秒上限在握手前固定", func(t *testing.T) {
		c := inferenceOfflineConfig(t, 1)
		if err := inferenceReserve(c); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		calls := 0
		_, err := inferenceCaptureWithDial(context.Background(), c, func(ctx context.Context, _ string, opts ws.DialOptions) (*ws.Conn, *http.Response, error) {
			calls++
			d, ok := ctx.Deadline()
			if !ok || d.Before(start.Add(44*time.Second)) || d.After(start.Add(46*time.Second)) {
				t.Error("未固定整会话45秒")
			}
			if opts.MaxPayload != 1<<20 || opts.WriteTimeout > time.Second {
				t.Error("传输上限")
			}
			return nil, nil, errors.New("本地注入拨号失败")
		})
		if err == nil || calls != 1 {
			t.Fatal("失败没有消耗一次调用")
		}
	})
	t.Run("manifest完整但合成费用仍不能授权", func(t *testing.T) {
		c := inferenceOfflineConfig(t, 1)
		// 只有预算数字，没有结构化依据；synthetic位也不能绕过费用验证。
		c.Manifest.CostEvidence = nil
		b, _ := json.Marshal(c.Manifest)
		if err := os.WriteFile(c.ManifestPath, b, 0600); err != nil {
			t.Fatal(err)
		}
		env := map[string]string{"OMUGW_RECORD_DS_INFERENCE": "1", "OMUGW_DS_INFERENCE_SLOT": "1", "OMUGW_DS_INFERENCE_OUTPUT": c.Output, "OMUGW_DS_INFERENCE_MANIFEST": c.ManifestPath, "OMUGW_SMOKE": "1"}
		_, err := inferenceRecordConfig(c.Root, func(k string) string {
			if k == "DASHSCOPE_API_KEY" {
				t.Fatal("缺费用证明仍读凭据")
			}
			return env[k]
		})
		if err == nil || !strings.Contains(err.Error(), "费用依据") {
			t.Fatalf("缺证明未停: %v", err)
		}
	})
	t.Run("样本篡改及symlink拨号前拒绝", func(t *testing.T) {
		for _, link := range []bool{false, true} {
			c := inferenceOfflineConfig(t, 1)
			if err := inferenceReserve(c); err != nil {
				t.Fatal(err)
			}
			if link {
				if err := os.Remove(c.SamplePath); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(c.ManifestPath, c.SamplePath); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(c.SamplePath, []byte{9, 9}, 0600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := inferenceCaptureWithDial(context.Background(), c, func(context.Context, string, ws.DialOptions) (*ws.Conn, *http.Response, error) {
				t.Fatal("无效sample仍拨号")
				return nil, nil, nil
			})
			if err == nil {
				t.Fatal("样本校验失败未停")
			}
		}
	})
	t.Run("默认和普通smoke不读凭据", func(t *testing.T) {
		for _, value := range []string{"", "true", "0"} {
			_, err := inferenceRecordConfig(t.TempDir(), func(k string) string {
				if k != "OMUGW_RECORD_DS_INFERENCE" {
					t.Fatalf("关闭仍读取 %s", k)
				}
				return value
			})
			if !errors.Is(err, errInferenceDisabled) {
				t.Fatalf("未关闭: %v", err)
			}
		}
	})
	t.Run("六槽持久且失败不退款", func(t *testing.T) {
		c := inferenceOfflineConfig(t, 1)
		for slot := 1; slot <= 6; slot++ {
			x := c
			x.Slot = slot
			x.Scenario = inferenceScenarioName(slot)
			x.Model = c.Manifest.Slots[slot-1].Model
			x.Voice = c.Manifest.Slots[slot-1].Voice
			x.Output = filepath.Join(filepath.Dir(c.Output), fmt.Sprintf("slot-%d", slot))
			if err := inferenceReserve(x); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(x.Output); err != nil {
				t.Fatal(err)
			}
			if err := inferenceReserve(x); err == nil {
				t.Fatal("删输出复活已消耗槽")
			}
		}
		c.Slot = 7
		if err := inferenceReserve(c); err == nil {
			t.Fatal("第七槽")
		}
		c.Slot = 1
		c.Batch = "renamed"
		c.Output = filepath.Join(c.Root, ".local/ws-recordings/renamed/slot-1")
		if err := inferenceReserve(c); err == nil {
			t.Fatal("改名绕槽")
		}
	})
	t.Run("并发只一名胜者", func(t *testing.T) {
		c := inferenceOfflineConfig(t, 1)
		var wins atomic.Int32
		var wg sync.WaitGroup
		for i := 0; i < 12; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if inferenceReserve(c) == nil {
					wins.Add(1)
				}
			}()
		}
		wg.Wait()
		if wins.Load() != 1 {
			t.Fatalf("wins=%d", wins.Load())
		}
	})
	t.Run("输出与父目录链接拒绝", func(t *testing.T) {
		for _, parent := range []bool{false, true} {
			c := inferenceOfflineConfig(t, 1)
			path := c.Output
			if parent {
				path = filepath.Join(c.Root, ".local")
			}
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(t.TempDir(), path); err != nil {
				t.Fatal(err)
			}
			if err := inferenceReserve(c); err == nil {
				t.Fatal("链接被放行")
			}
		}
	})
	t.Run("manifest及样本拒绝", func(t *testing.T) {
		for _, change := range []func(*inferenceRecordingConfig){
			func(c *inferenceRecordingConfig) { c.Manifest.WorstCaseFen = 101 },
			func(c *inferenceRecordingConfig) { c.Manifest.Slots[0].MaxTasks = 3 },
			func(c *inferenceRecordingConfig) { c.Manifest.Slots[0].MaxInputAudioSeconds = 7 },
			func(c *inferenceRecordingConfig) { c.Manifest.Slots[1].MaxInputCharacters = 81 },
			func(c *inferenceRecordingConfig) { c.Manifest.Slots[3].WorstCaseTokens = -1 },
			func(c *inferenceRecordingConfig) { c.Manifest.Slots[0].WorstCaseFen = 0 },
			func(c *inferenceRecordingConfig) { c.Manifest.Region = "ap-southeast-1" },
			func(c *inferenceRecordingConfig) {
				c.Manifest.Endpoint = "wss://dashscope.aliyuncs.com/api-ws/v1/inference"
			},
			func(c *inferenceRecordingConfig) { c.Manifest.Slots[0].Model = "guess-latest" },
			func(c *inferenceRecordingConfig) { c.SampleSHA256 = strings.Repeat("0", 64) },
			func(c *inferenceRecordingConfig) { c.SamplePath = "relative.pcm" },
			func(c *inferenceRecordingConfig) { c.Manifest.PriceSource = "" },
		} {
			c := inferenceOfflineConfig(t, 1)
			change(&c)
			if err := inferenceReserve(c); err == nil {
				t.Fatal("非法配置占槽")
			}
			if _, err := os.Stat(filepath.Dir(c.Output)); !os.IsNotExist(err) {
				t.Fatal("预检失败仍分配目录")
			}
		}
		c := inferenceOfflineConfig(t, 1)
		c.synthetic = false
		c.Manifest.CostEvidence = nil
		if err := inferenceReserve(c); err == nil {
			t.Fatal("合成数字冒充真实token费用证明")
		}
	})
	t.Run("消息轨迹节点音频发送前拒绝", func(t *testing.T) {
		for _, tc := range []struct {
			r   inferenceRecording
			rec inferenceRecord
		}{
			{inferenceRecording{}, inferenceRecord{Direction: "send", Opcode: ws.OpBinary, Payload: make([]byte, (1<<20)+1)}},
			{inferenceRecording{rawBytes: 16 << 20}, inferenceRecord{Direction: "send", Opcode: ws.OpBinary, Payload: []byte{1}}},
			{inferenceRecording{Records: make([]inferenceRecord, 1024)}, inferenceRecord{Direction: "send", Opcode: ws.OpBinary, Payload: []byte{1}}},
			{inferenceRecording{audioBytes: 8 << 20}, inferenceRecord{Direction: "receive", Opcode: ws.OpBinary, Payload: []byte{1}}},
		} {
			if err := tc.r.checkRecord(tc.rec, ""); err == nil {
				t.Fatal("超限未拒绝")
			}
		}
	})
}

// 本地对端独立断言字面协议，不调用录制器的请求构造或解码来产生预期。
func inferenceScript(t *testing.T, slot int, ending string, c *ws.Conn) error {
	t.Helper()
	read := func(action, id string) error {
		op, b, err := c.ReadMessage()
		if err != nil {
			return err
		}
		if op != ws.OpText {
			return fmt.Errorf("需要text")
		}
		var m map[string]any
		if json.Unmarshal(b, &m) != nil {
			return fmt.Errorf("JSON")
		}
		h, _ := m["header"].(map[string]any)
		if h["action"] != action || h["task_id"] != id {
			return fmt.Errorf("动作/ID错误: %s", b)
		}
		p, _ := m["payload"].(map[string]any)
		if action == "run-task" {
			models := []string{"qwen-audio-3.0-asr-flash-streaming", "qwen-audio-3.0-tts-flash", "sambert-zhichu-v1", "qwen-audio-3.1-asr-flash-message", "qwen-audio-3.1-asr-flash-streaming", "qwen-audio-3.0-tts-flash"}
			if p["model"] != models[slot-1] || p["task_group"] != "audio" {
				return fmt.Errorf("model/三元组")
			}
			wantTask, wantFunction := "asr", "recognition"
			if slot == 2 || slot == 3 || slot == 6 {
				wantTask, wantFunction = "tts", "SpeechSynthesizer"
			}
			if p["task"] != wantTask || p["function"] != wantFunction {
				return fmt.Errorf("任务三元组不符")
			}
			param, _ := p["parameters"].(map[string]any)
			if param["format"] != "pcm" || param["sample_rate"] != float64(16000) {
				return fmt.Errorf("音频参数")
			}
			if slot == 3 {
				if h["streaming"] != "out" {
					return fmt.Errorf("out")
				}
				in, _ := p["input"].(map[string]any)
				if in["text"] != "这是网关测试。请确认声音清晰。" {
					return fmt.Errorf("out文本")
				}
			} else if h["streaming"] != "duplex" {
				return fmt.Errorf("duplex")
			}
			if slot == 2 || slot == 6 {
				param, _ := p["parameters"].(map[string]any)
				want := "longanlingxi"
				if slot == 6 {
					want = "omugw-invalid-voice-s3"
				}
				if param["voice"] != want {
					return fmt.Errorf("voice")
				}
			}
		}
		return nil
	}
	send := func(event, id, usage string) error {
		return c.WriteMessage(ws.OpText, []byte(fmt.Sprintf(`{ "header":{"event":%q,"task_id":%q}, "payload":{"usage":%s,"output":{"event":"sentence-end","text":"测试"}} }`, event, id, usage)))
	}
	tasks := 1
	if slot == 1 || slot == 2 || slot == 5 {
		tasks = 2
	}
	for n := 1; n <= tasks; n++ {
		id := fmt.Sprintf("s3-%d-%d", slot, n)
		if err := read("run-task", id); err != nil {
			return err
		}
		if slot == 6 {
			if ending == "started-failed" {
				if err := send("task-started", id, "null"); err != nil {
					return err
				}
				for _, action := range []string{"continue-task", "continue-task", "finish-task"} {
					if err := read(action, id); err != nil {
						return err
					}
				}
			}
			if err := c.WriteMessage(ws.OpText, []byte(`{"header":{"event":"task-failed","task_id":"s3-6-1","error_code":"InvalidParameter"},"payload":{}}`)); err != nil {
				return err
			}
			switch ending {
			case "peer", "started-failed":
				return c.Close(4003, "literal peer reason")
			case "normal-peer":
				return c.Close(1000, "")
			case "eof":
				return nil
			case "silent":
				_, _, err := c.ReadMessage()
				var ce *ws.CloseError
				if !errors.As(err, &ce) || ce.Code != 1000 {
					return fmt.Errorf("silent close: %v", err)
				}
				return nil
			}
		}
		if err := send("task-started", id, "null"); err != nil {
			return err
		}
		if slot == 1 || slot == 4 || slot == 5 {
			op, b, err := c.ReadMessage()
			if err != nil {
				return err
			}
			if op != ws.OpBinary || !bytes.Equal(b, []byte{1, 0, 2, 0, 3, 0, 4, 0}) {
				return fmt.Errorf("音频字节")
			}
			if slot != 5 || n != 2 {
				if err := read("finish-task", id); err != nil {
					return err
				}
			}
		} else if slot == 2 {
			for _, text := range []string{"这是网关测试。", "请确认声音清晰。"} {
				op, b, err := c.ReadMessage()
				if err != nil {
					return err
				}
				if op != ws.OpText || !bytes.Contains(b, []byte(`"action":"continue-task"`)) || !bytes.Contains(b, []byte(text)) {
					return fmt.Errorf("continue文本")
				}
			}
			if err := read("finish-task", id); err != nil {
				return err
			}
		}
		usage := `{"duration":0.25}`
		if slot == 2 || slot == 3 {
			usage = `{"characters":15}`
		}
		if slot == 4 || slot == 5 {
			usage = `{"input_tokens":6,"output_tokens":7,"total_tokens":13,"duration":0.25}`
		}
		if err := send("result-generated", id, usage); err != nil {
			return err
		}
		if slot == 5 && n == 2 {
			_, _, err := c.ReadMessage()
			var ce *ws.CloseError
			if !errors.As(err, &ce) || ce.Code != 1000 {
				return fmt.Errorf("interrupt close")
			}
			return nil
		}
		if slot == 2 || slot == 3 {
			if err := c.WriteMessage(ws.OpBinary, []byte{9, 8, 7, 6}); err != nil {
				return err
			}
		}
		if err := send("task-finished", id, usage); err != nil {
			return err
		}
	}
	if ending == "early-peer" {
		return c.Close(1000, "upstream complete")
	}
	_, _, err := c.ReadMessage()
	var ce *ws.CloseError
	if !errors.As(err, &ce) || ce.Code != 1000 {
		return fmt.Errorf("close: %v", err)
	}
	return nil
}

func inferenceLocalCapture(t *testing.T, cfg inferenceRecordingConfig, ending string) inferenceRecording {
	t.Helper()
	if err := inferenceReserve(cfg); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer offline-secret" || r.URL.RawQuery != "" {
			done <- fmt.Errorf("握手鉴权/query")
			return
		}
		c, err := ws.Accept(w, r, ws.AcceptOptions{MaxPayload: 1 << 20, Idle: 3 * time.Second})
		if err != nil {
			done <- err
			return
		}
		finish, _ := c.ArmCloseDeadline(time.Now().Add(4 * time.Second))
		defer finish()
		err = inferenceScript(t, cfg.Slot, ending, c)
		if ending == "eof" {
			finish()
		}
		done <- err
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := inferenceCaptureWithDial(ctx, cfg, func(ctx context.Context, _ string, opts ws.DialOptions) (*ws.Conn, *http.Response, error) {
		return ws.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/api-ws/v1/inference", opts)
	})
	if err != nil {
		t.Fatalf("capture: %v outcome=%s", err, r.Outcome)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("server未join")
	}
	if calls.Load() != 1 {
		t.Fatalf("dial=%d", calls.Load())
	}
	return r
}

func TestInferenceRecorderOfflineScenarios(t *testing.T) {
	for slot := 1; slot <= 6; slot++ {
		t.Run(fmt.Sprint(slot), func(t *testing.T) {
			cfg := inferenceOfflineConfig(t, slot)
			r := inferenceLocalCapture(t, cfg, "peer")
			want := "completed"
			if slot == 5 {
				want = "interrupted"
			}
			if slot == 6 {
				want = "failed"
			}
			if r.Outcome != want {
				t.Fatalf("outcome=%s", r.Outcome)
			}
			if slot == 5 {
				var firstTerminal, secondSnapshot, secondTerminal bool
				for _, rec := range r.Records {
					if rec.Direction != "receive" || rec.Opcode != ws.OpText {
						continue
					}
					e, err := inferenceDecode(rec.Payload, r.Model)
					if err != nil {
						t.Fatal(err)
					}
					firstTerminal = firstTerminal || e.TaskID == "s3-5-1" && e.Event == "task-finished" && e.Usage
					secondSnapshot = secondSnapshot || e.TaskID == "s3-5-2" && e.Event == "result-generated" && e.TotalTokens == 13
					secondTerminal = secondTerminal || e.TaskID == "s3-5-2" && e.Event == "task-finished"
				}
				if !firstTerminal || !secondSnapshot || secondTerminal {
					t.Fatal("中断抹去已结或伪造未结证据")
				}
			}
			if r.Ended.Sub(r.Started) > 45*time.Second {
				t.Fatal("超过整会话预算")
			}
			f, err := inferenceCandidate(r)
			if err != nil {
				t.Fatal(err)
			}
			if f.Response.WS.Outcome.Kind != want {
				t.Fatal("候选结局")
			}
			if err := inferenceSave(cfg.Output, r); err != nil {
				t.Fatal(err)
			}
			if _, err := testkit.ReadWSFixture(filepath.Join(cfg.Output, "candidate.json"), testkit.DefaultWSLimits()); err != nil {
				t.Fatal(err)
			}
			if err := inferenceSave(cfg.Output, r); err == nil {
				t.Fatal("覆盖既有证据")
			}
		})
	}
}

func TestInferenceRecorderOfflineEvidence(t *testing.T) {
	t.Run("保存不能把新candidate贴到旧raw", func(t *testing.T) {
		c := inferenceOfflineConfig(t, 3)
		r := inferenceLocalCapture(t, c, "peer")
		for i := range r.Records {
			if r.Records[i].Opcode == ws.OpBinary && r.Records[i].Direction == "receive" {
				r.Records[i].Payload = []byte{1, 2, 3, 4}
				r.Records[i].SHA256 = inferenceSHA(r.Records[i].Payload)
				break
			}
		}
		if err := inferenceSave(c.Output, r); err == nil {
			t.Fatal("篡改candidate脱离已刷盘raw")
		}
	})
	t.Run("收尾超限不能静默丢尾消息", func(t *testing.T) {
		c := inferenceOfflineConfig(t, 3)
		if err := inferenceReserve(c); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer close(done)
			conn, err := ws.Accept(w, r, ws.AcceptOptions{MaxPayload: 1 << 20, Idle: time.Second})
			if err != nil {
				return
			}
			finish, _ := conn.ArmCloseDeadline(time.Now().Add(2 * time.Second))
			defer finish()
			_, _, _ = conn.ReadMessage()
			_ = conn.WriteMessage(ws.OpText, []byte(`{"header":{"event":"task-started","task_id":"s3-3-1"},"payload":{}}`))
			_ = conn.WriteMessage(ws.OpText, []byte(`{"header":{"event":"task-finished","task_id":"s3-3-1"},"payload":{"usage":{"characters":15}}}`))
			_ = conn.WriteMessage(ws.OpText, []byte(`{"header":{"event":"late","task_id":"s3-3-1"},"payload":{"secret":"offline-secret"}}`))
			_, _, _ = conn.ReadMessage()
		}))
		defer srv.Close()
		r, err := inferenceCaptureWithDial(context.Background(), c, func(ctx context.Context, _ string, opts ws.DialOptions) (*ws.Conn, *http.Response, error) {
			return ws.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), opts)
		})
		if err == nil || r.Failure == "" {
			t.Fatal("被拒绝尾消息没有污染证据")
		}
		if _, err := inferenceCandidate(r); err == nil {
			t.Fatal("裁剪轨迹生成candidate")
		}
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("server未join")
		}
	})
	t.Run("候选partial用量与绑定不能自证", func(t *testing.T) {
		c := inferenceOfflineConfig(t, 4)
		r := inferenceLocalCapture(t, c, "peer")
		for _, mutate := range []func(*inferenceRecord){
			func(rec *inferenceRecord) {
				rec.Payload = bytes.ReplaceAll(rec.Payload, []byte(`"output_tokens":7,`), nil)
			},
			func(rec *inferenceRecord) {
				rec.Payload = bytes.ReplaceAll(rec.Payload, []byte(`"event":"task-finished"`), []byte(`"event":"task-finished","event":"task-finished"`))
			},
			func(rec *inferenceRecord) {
				rec.Payload = bytes.ReplaceAll(rec.Payload, []byte(`"payload":{"usage":`), []byte(`"payload":{"task":"tts","usage":`))
			},
		} {
			x := r
			x.Records = append([]inferenceRecord(nil), r.Records...)
			for i := range x.Records {
				mutate(&x.Records[i])
				x.Records[i].SHA256 = inferenceSHA(x.Records[i].Payload)
			}
			if _, err := inferenceCandidate(x); err == nil {
				t.Fatal("坏协议字段生成候选")
			}
		}
		x := r
		x.Records = append([]inferenceRecord(nil), r.Records...)
		x.Records[0].Payload = bytes.Clone(x.Records[0].Payload)
		x.Records[0].Payload[0] = '['
		if _, err := inferenceCandidate(x); err == nil {
			t.Fatal("SHA篡改")
		}
	})
	t.Run("终态后抢先peerclose仍保全", func(t *testing.T) {
		for n := 0; n < 12; n++ {
			c := inferenceOfflineConfig(t, 3)
			r := inferenceLocalCapture(t, c, "early-peer")
			if r.TransportEnd != "peer_close" || r.Outcome != "completed" {
				t.Fatalf("丢peer close: %s/%s", r.TransportEnd, r.Outcome)
			}
			if _, err := inferenceCandidate(r); err != nil {
				t.Fatal(err)
			}
		}
	})
	t.Run("重复键原文仍作为失败材料保留", func(t *testing.T) {
		var r inferenceRecording
		raw := []byte(`{"header":{"event":"task-finished","event":"task-started","task_id":"x"},"payload":{}}`)
		if err := r.add(inferenceRecord{Direction: "receive", Opcode: ws.OpText, Payload: raw}, "offline-secret"); err != nil {
			t.Fatal("不应丢弃不含秘密的原始重复键材料", err)
		}
		if !bytes.Equal(r.Records[0].Payload, raw) {
			t.Fatal("原文被重编码")
		}
		if _, err := inferenceDecode(r.Records[0].Payload, "qwen-audio-3.0-asr-flash-streaming"); err == nil {
			t.Fatal("重复键升格成证据")
		}
	})
	t.Run("failed关闭四种事实", func(t *testing.T) {
		for _, ending := range []string{"peer", "started-failed", "normal-peer", "eof", "silent"} {
			t.Run(ending, func(t *testing.T) {
				c := inferenceOfflineConfig(t, 6)
				r := inferenceLocalCapture(t, c, ending)
				if r.Outcome != "failed" {
					t.Fatal("关闭抹掉业务失败")
				}
				if ending == "eof" && r.TransportEnd != "eof" || ending == "silent" && r.PassiveEnd != "silent" {
					t.Fatalf("关闭事实 %+v", r)
				}
				if ending == "normal-peer" || ending == "eof" || ending == "silent" {
					if _, err := inferenceCandidate(r); err == nil {
						t.Fatal("不足的关闭证据生成candidate")
					}
				}
			})
		}
	})
	t.Run("解码重复关键键与partial用量", func(t *testing.T) {
		for _, raw := range []string{
			`{"header":{"event":"task-finished","event":"task-started","task_id":"x"},"payload":{}}`,
			`{"header":{"event":"task-finished","task_id":"x","task_\u0069d":"y"},"payload":{}}`,
			`{"header":{"event":"result-generated","task_id":"x"},"payload":{"usage":{"input_tokens":1,"total_tokens":1}}}`,
			`{"header":{"event":"task-finished","task_id":"x"},"payload":{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":3}}}`,
		} {
			if _, err := inferenceDecode([]byte(raw), "qwen-audio-3.1-asr-flash-message"); err == nil {
				t.Fatal("虚假证据未拒绝")
			}
		}
	})
	t.Run("缺终态篡改与分开预算", func(t *testing.T) {
		c := inferenceOfflineConfig(t, 1)
		r := inferenceLocalCapture(t, c, "peer")
		for i, rec := range r.Records {
			if bytes.Contains(rec.Payload, []byte("task-finished")) {
				r.Records = append(r.Records[:i], r.Records[i+1:]...)
				break
			}
		}
		if _, err := inferenceCandidate(r); err == nil {
			t.Fatal("缺终态仍completed")
		}
	})
	t.Run("合法raw超过candidate预算不裁剪", func(t *testing.T) {
		c := inferenceOfflineConfig(t, 3)
		r := inferenceLocalCapture(t, c, "peer")
		if _, err := inferenceCandidate(r); err != nil {
			t.Fatal(err)
		}
		var big inferenceRecording
		for _, rec := range r.Records {
			if bytes.Contains(rec.Payload, []byte("task-finished")) {
				for i := 0; i < 3; i++ {
					if err := big.add(inferenceRecord{Direction: "receive", Opcode: ws.OpBinary, At: rec.At, Payload: make([]byte, 1<<20)}, ""); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := big.add(rec, ""); err != nil {
				t.Fatal(err)
			}
		}
		r.Records = big.Records
		r.rawBytes = big.rawBytes
		r.encodedBytes = big.encodedBytes
		r.audioBytes = big.audioBytes
		r.streamed = false
		if _, err := inferenceCandidate(r); err == nil {
			t.Fatal("未独立拒绝4MiB candidate轨迹")
		}
		out, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(out, 0700); err != nil {
			t.Fatal(err)
		}
		if err := inferenceSave(out, r); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(out, "candidate-unavailable.txt")); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(out, "records.jsonl"))
		if err != nil || bytes.Count(b, []byte("\n")) != len(r.Records) {
			t.Fatal("raw被删事件")
		}
	})
	t.Run("凭据不落盘", func(t *testing.T) {
		for _, b := range [][]byte{[]byte(`{"error":"offline\u002dsecret"}`), []byte(`{"Authorization":"x"}`), []byte("offline-secret")} {
			var r inferenceRecording
			if err := r.add(inferenceRecord{Direction: "receive", Opcode: ws.OpText, Payload: b}, "offline-secret"); err == nil {
				t.Fatal("泄露未拦")
			}
		}
	})
}
