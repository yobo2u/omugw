//go:build smoke

package smoke_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/yobo2u/omugw/internal/testkit"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

// 取自实录的消息形状，ID 简化后接到合成轨迹；只证明候选构建，不能充当云端证据。
func TestDSRealtimeCandidatePreviousItemOffline(t *testing.T) {
	r := dsSyntheticRecording()
	insert := []dsRecord{
		{Direction: "receive", Kind: "message", Opcode: ws.OpText, Payload: []byte(`{"event_id":"added","type":"response.output_item.added","response_id":"r1","output_index":0,"item":{"id":"i2","object":"realtime.item","type":"message","status":"in_progress","role":"assistant","content":[]}}`)},
		{Direction: "receive", Kind: "message", Opcode: ws.OpText, Payload: []byte(`{"event_id":"created","type":"conversation.item.created","previous_item_id":"i1","item":{"id":"i2","object":"realtime.item","type":"message","status":"in_progress","role":"assistant","content":[]}}`)},
	}
	r.Records = append(r.Records[:6:6], append(insert, r.Records[6:]...)...)
	f, err := dsCandidate(r)
	if err != nil {
		t.Fatal(err)
	}
	matcher, err := testkit.NewWSMatcher(testkit.DefaultWSLimits())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range f.Response.WS.Nodes {
		if n.Message == nil {
			continue
		}
		if n.ID == "n0007_1" {
			if !bytes.Equal(n.Message.Payload, insert[1].Payload) {
				t.Fatal("previous_item_id 原始字节被改写")
			}
			wrong := *n.Message
			wrong.Payload = bytes.Replace(wrong.Payload, []byte(`"previous_item_id":"i1"`), []byte(`"previous_item_id":"other"`), 1)
			if err := matcher.Match(n, wrong); err == nil {
				t.Fatal("previous_item_id 错链被忽略")
			}
		}
		if err := matcher.Match(n, *n.Message); err != nil {
			t.Fatal(err)
		}
	}
	dsReplayCandidate(t, f)
}

// 显式只读入口用于原始实录诊断；默认测试不依赖 ignored 私有文件。
func TestDSRealtimeCandidateOriginalOffline(t *testing.T) {
	path := os.Getenv("OMUGW_DSREALTIME_OFFLINE_RECORDING")
	if path == "" {
		t.Skip("未指定离线录制")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	f, err := dsLoadTextCandidate(root, path, os.Getenv("OMUGW_DSREALTIME_OFFLINE_SHA256"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := dsReadBounded(path, 2*dsMaxTrace)
	if err != nil {
		t.Fatal(err)
	}
	var r dsRecording
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal("录制不是合法 JSON")
	}
	t.Logf("recording sha256=%s records=%d", dsDigest(b), len(r.Records))
	for _, c := range f.Response.WS.Coverage {
		if c.Capability == "stateful_conversation" {
			t.Fatal("工具结果后的复述不能充当先前记忆证据")
		}
	}
	// 若已经独占恢复，必须回放落盘候选并证明其与原录制的现算结果完全一致。
	file := filepath.Join(filepath.Dir(path), "candidate.json")
	if _, err := os.Lstat(file); err == nil {
		loaded, err := testkit.ReadWSFixture(file, testkit.DefaultWSLimits())
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.MarshalIndent(f, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		saved, err := dsReadBounded(file, testkit.DefaultWSLimits().FileBytes)
		if err != nil || !bytes.Equal(saved, encoded) {
			t.Fatal("落盘候选与原录制重建结果不符")
		}
		f = loaded
		t.Logf("回放落盘候选 sha256=%s", dsDigest(saved))
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	dsReplayCandidate(t, f)
}

// 旧候选只读校验并回放；样本使用候选自身已有字节，不能重写历史文件。
func TestDSRealtimeCandidatesExistingOffline(t *testing.T) {
	batch := os.Getenv("OMUGW_DSREALTIME_OFFLINE_BATCH")
	if batch == "" {
		t.Skip("未指定离线批次")
	}
	for _, run := range []string{"tts-commit-v3", "tts-server-commit-v2"} {
		t.Run(run, func(t *testing.T) {
			path := filepath.Join(batch, run, "candidate.json")
			f, err := testkit.ReadWSFixture(path, testkit.DefaultWSLimits())
			if err != nil {
				t.Fatal(err)
			}
			b, err := dsReadBounded(path, testkit.DefaultWSLimits().FileBytes)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := dsReadBounded(filepath.Join(batch, run, "recording.json"), 2*dsMaxTrace)
			if err != nil {
				t.Fatal(err)
			}
			var r dsRecording
			if err := json.Unmarshal(raw, &r); err != nil {
				t.Fatal(err)
			}
			r.Audio = f.Response.WS.Samples["output.pcm_s16le.16000.mono"].Data
			rebuilt, err := dsCandidate(r)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.MarshalIndent(rebuilt, "", "  ")
			if err != nil || !bytes.Equal(encoded, b) {
				t.Fatal("新代码与旧候选契约不一致")
			}
			dsReplayCandidate(t, f)
			t.Logf("原候选未改写 sha256=%s", dsDigest(b))
		})
	}
	for _, run := range []string{"text-tools", "text-tools-v2", "tts-commit", "tts-commit-v2", "tts-server-commit"} {
		t.Run(run, func(t *testing.T) {
			b, err := dsReadBounded(filepath.Join(batch, run, "recording.json"), 2*dsMaxTrace)
			if err != nil {
				t.Fatal(err)
			}
			var r dsRecording
			if err := json.Unmarshal(b, &r); err != nil {
				t.Fatal(err)
			}
			if _, err := dsCandidate(r); err == nil {
				t.Fatal("历史失败被新代码转换为成功候选")
			}
			t.Logf("历史失败保持拒绝 sha256=%s failure=%s", dsDigest(b), r.Failure)
		})
	}
}
