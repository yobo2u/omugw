//go:build smoke

package smoke_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/testkit"
)

func dsRecoveryTestRecording(t *testing.T) dsRecording {
	t.Helper()
	r := dsIdentityCapture(t, "text-tools", func(p *dsOfflinePeer) {
		p.update("text-tools")
		dsOfflineTools(p)
	})
	if r.Failure != "" {
		t.Fatal(r.Failure)
	}
	return r
}

func dsRecoveryTestPath(t *testing.T, b []byte) (string, string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, ".local/recordings/dsrealtime/batch/run")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "recording.json")
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	return root, path
}

func TestDSRealtimeCandidateRecoveryOfflineExclusive(t *testing.T) {
	b, err := json.Marshal(dsRecoveryTestRecording(t))
	if err != nil {
		t.Fatal(err)
	}
	root, path := dsRecoveryTestPath(t, b)
	batch := filepath.Dir(filepath.Dir(path))
	for i := 1; i <= 8; i++ {
		if err := dsWriteExclusive(batch, fmt.Sprintf(".attempt-%d", i), nil); err != nil {
			t.Fatal(err)
		}
	}
	f, err := dsRecoverTextCandidate(root, path, dsDigest(b))
	if err != nil {
		t.Fatal(err)
	}
	if f.Response.WS.Outcome.Kind != "completed" || len(f.Response.WS.Outcome.Terminal) != 3 {
		t.Fatal("恢复未保留三轮终态")
	}
	file := filepath.Join(filepath.Dir(path), "candidate.json")
	loaded, err := testkit.ReadWSFixture(file, testkit.DefaultWSLimits())
	if err != nil {
		t.Fatal(err)
	}
	dsReplayCandidate(t, loaded)
	before, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dsRecoverTextCandidate(root, path, dsDigest(b)); err == nil {
		t.Fatal("重复恢复覆盖已有候选")
	}
	after, _ := os.ReadFile(file)
	raw, _ := os.ReadFile(path)
	info, _ := os.Stat(file)
	if !bytes.Equal(before, after) || !bytes.Equal(raw, b) || info.Mode().Perm() != 0600 {
		t.Fatal("候选或录制被覆盖，或候选权限不私有")
	}
	entries, _ := os.ReadDir(batch)
	if len(entries) != 9 {
		t.Fatal("恢复新增尝试槽或目录")
	}
	for i := 1; i <= 8; i++ {
		slot, err := os.ReadFile(filepath.Join(batch, fmt.Sprintf(".attempt-%d", i)))
		if err != nil || len(slot) != 0 {
			t.Fatal("尝试槽被改写")
		}
	}
}

func TestDSRealtimeCandidateRecoveryOfflineRejects(t *testing.T) {
	original, _ := json.Marshal(dsRecoveryTestRecording(t))
	for _, name := range []string{"failed", "unconfirmed", "handshake", "not_text", "input_sample", "missing_close", "missing_reply", "close_code", "missing_done", "invalid_direction", "audio", "too_many_records", "too_large", "unknown_field", "trailing_json", "bad_hash", "no_hash", "outside", "wrong_name", "linked_file", "linked_run", "linked_root", "linked_local", "existing_candidate", "linked_candidate"} {
		t.Run(name, func(t *testing.T) {
			var r dsRecording
			_ = json.Unmarshal(original, &r)
			switch name {
			case "failed":
				r.Failure = "read_failed_or_raw_eof"
			case "unconfirmed":
				r.Confirmed = false
			case "handshake":
				r.Status = 403
			case "not_text":
				r.Scenario = "tts-commit"
			case "input_sample":
				r.Input = &dsAudioSample{File: "secret.pcm"}
			case "missing_close":
				r.Records = r.Records[:len(r.Records)-2]
			case "missing_reply":
				r.Records = r.Records[:len(r.Records)-1]
			case "close_code":
				r.Records[len(r.Records)-1].CloseCode = 1001
			case "missing_done":
				r.Records = append(r.Records[:len(r.Records)-3], r.Records[len(r.Records)-2:]...)
			case "invalid_direction":
				r.Records[0].Direction = "unknown"
			case "audio":
				r.Records[0].Payload = []byte(`{"type":"response.audio.delta","delta":"AAAAAA=="}`)
			case "too_many_records":
				r.Records = make([]dsRecord, dsMaxRecords+1)
			}
			b, _ := json.Marshal(r)
			switch name {
			case "too_large":
				b = bytes.Repeat([]byte(" "), 2*dsMaxTrace+1)
			case "unknown_field":
				b = append([]byte(`{"extra":true,`), b[1:]...)
			case "trailing_json":
				b = append(b, []byte(` {}`)...)
			}
			root, path := dsRecoveryTestPath(t, b)
			dir := filepath.Dir(path)
			hash := dsDigest(b)
			switch name {
			case "bad_hash":
				hash = strings.Repeat("0", 64)
			case "no_hash":
				hash = ""
			case "outside":
				root = t.TempDir()
			case "wrong_name":
				other := filepath.Join(dir, "other.json")
				if err := os.Rename(path, other); err != nil {
					t.Fatal(err)
				}
				path = other
			case "linked_file", "linked_run", "linked_root", "linked_local":
				link := path
				switch name {
				case "linked_run":
					link = dir
				case "linked_root":
					link = filepath.Join(root, ".local/recordings/dsrealtime")
				case "linked_local":
					link = filepath.Join(root, ".local")
				}
				if err := os.Rename(link, link+"-target"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(link+"-target", link); err != nil {
					t.Fatal(err)
				}
			case "existing_candidate":
				if err := dsWriteExclusive(dir, "candidate.json", []byte("sentinel")); err != nil {
					t.Fatal(err)
				}
			case "linked_candidate":
				if err := os.Symlink(path, filepath.Join(dir, "candidate.json")); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := dsRecoverTextCandidate(root, path, hash); err == nil {
				t.Fatal("非法输入仍产出候选")
			}
			if name != "existing_candidate" && name != "linked_candidate" {
				if _, err := os.Lstat(filepath.Join(dir, "candidate.json")); !os.IsNotExist(err) {
					t.Fatal("失败恢复留下候选")
				}
			}
			if raw, err := os.ReadFile(path); err != nil || !bytes.Equal(raw, b) {
				t.Fatal("恢复改写原始录制")
			}
		})
	}
}
