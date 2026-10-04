//go:build smoke

package smoke_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/testkit"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

// 恢复入口不拨号、不读取凭据、不保留尝试槽；完整回放另走只读离线验证入口。
func TestRecoverDSRealtimeTextCandidateOffline(t *testing.T) {
	path := os.Getenv("OMUGW_DSREALTIME_RECOVER_RECORDING")
	if path == "" {
		t.Skip("未指定离线恢复录制")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal("无法定位仓库")
	}
	f, err := dsRecoverTextCandidate(root, path, os.Getenv("OMUGW_DSREALTIME_RECOVER_SHA256"))
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(filepath.Dir(path), "candidate.json")
	_, err = testkit.ReadWSFixture(file, testkit.DefaultWSLimits())
	if err != nil {
		t.Fatal(err)
	}
	b, err := dsReadBounded(file, testkit.DefaultWSLimits().FileBytes)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("离线独占恢复: nodes=%d terminals=%d candidate_sha256=%s contract_sha256=%s", len(f.Response.WS.Nodes), len(f.Response.WS.Outcome.Terminal), dsDigest(b), f.Response.WS.Provenance.SourceSHA256)
}

func dsRecoveryPath(root, path string) error {
	base := filepath.Join(root, ".local/recordings/dsrealtime")
	dir := filepath.Dir(path)
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Base(path) != "recording.json" || !dsOutputRelative(base, dir) {
		return errors.New("离线输入必须是 ignored 录制根下 batch/run/recording.json 的绝对路径")
	}
	// 从仓库根逐层拒绝链接；系统级 /var 链接不属于仓库内路径。
	for p := dir; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil || !info.IsDir() {
			return errors.New("离线录制目录不存在或包含链接")
		}
		if p == root {
			return nil
		}
	}
}

func dsLoadTextCandidate(root, path, expectedHash string) (testkit.Fixture, error) {
	var empty testkit.Fixture
	if err := dsRecoveryPath(root, path); err != nil {
		return empty, err
	}
	hash, err := hex.DecodeString(expectedHash)
	if err != nil || len(hash) != 32 || expectedHash != strings.ToLower(expectedHash) {
		return empty, errors.New("必须指定原始录制的 SHA256")
	}
	// base64 膨胀后仍有固定文件预算；不读取录制引用的样本或任何其他文件。
	b, err := dsReadBounded(path, 2*dsMaxTrace)
	if err != nil {
		return empty, err
	}
	if dsDigest(b) != expectedHash {
		return empty, errors.New("原始录制 SHA256 不符")
	}
	var r dsRecording
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&r) != nil || decoder.Decode(new(any)) != io.EOF {
		return empty, errors.New("录制结构非法、含未知字段或尾随 JSON")
	}
	if r.Scenario != "text-tools" || r.Input != nil || r.Status != 101 || r.Failure != "" || !r.Confirmed || len(r.Records) < 2 || len(r.Records) > dsMaxRecords {
		return empty, errors.New("只恢复无失败、已确认的完整纯文本工具录制")
	}
	traceBytes := 0
	for i, rec := range r.Records {
		if rec.Direction != "send" && rec.Direction != "receive" {
			return empty, errors.New("录制方向非法")
		}
		if i >= len(r.Records)-2 {
			want := "send"
			if i == len(r.Records)-1 {
				want = "receive"
			}
			if rec.Direction != want || rec.Kind != "close" || rec.CloseCode != 1000 || len(rec.Payload) != 0 || rec.Opcode != 0 {
				return empty, errors.New("缺少真实双向正常 close 尾部")
			}
			continue
		}
		traceBytes += len(rec.Payload)
		if rec.Kind != "message" || rec.Opcode != ws.OpText || rec.CloseCode != 0 || rec.CloseReason != "" || len(rec.Payload) > dsMaxMessage || traceBytes > dsMaxTrace || !dsSafePayload(rec.Payload, "") {
			return empty, errors.New("录制消息不是有界安全文本")
		}
		var e map[string]any
		if json.Unmarshal(rec.Payload, &e) != nil {
			return empty, errors.New("录制消息不是 JSON 对象")
		}
		typ := dsString(e, "type")
		if typ == "" || typ == "error" || strings.HasPrefix(typ, "response.audio") || strings.HasPrefix(typ, "input_audio_buffer.") || strings.HasPrefix(typ, "input_image_buffer.") || strings.HasPrefix(typ, "input_text_buffer.") {
			return empty, errors.New("离线恢复只接受纯文本工具消息")
		}
	}
	f, err := dsCandidate(r)
	if err != nil {
		return empty, err
	}
	terminals := map[string]bool{}
	for _, terminal := range f.Response.WS.Outcome.Terminal {
		terminals[terminal.Symbol] = true
	}
	if f.Response.WS.Outcome.Kind != "completed" || len(f.Response.WS.Outcome.Terminal) != 3 || len(terminals) != 3 {
		return empty, errors.New("纯文本工具录制缺少三轮独立完成终态")
	}
	return f, nil
}

func dsRecoverTextCandidate(root, path, expectedHash string) (testkit.Fixture, error) {
	f, err := dsLoadTextCandidate(root, path, expectedHash)
	if err != nil {
		return testkit.Fixture{}, err
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil || int64(len(b)) > testkit.DefaultWSLimits().FileBytes {
		return testkit.Fixture{}, errors.New("候选无法有界编码")
	}
	if err := dsRecoveryPath(root, path); err != nil {
		return testkit.Fixture{}, err
	}
	// 固定目录描述符并独占建文件，避免写出时被替换的父目录将输出引向别处。
	dir, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return testkit.Fixture{}, errors.New("无法固定离线候选目录")
	}
	defer dir.Close()
	out, err := dir.OpenFile("candidate.json", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return testkit.Fixture{}, errors.New("候选已存在或无法独占创建")
	}
	_, writeErr := out.Write(b)
	closeErr := out.Close()
	if writeErr != nil || closeErr != nil {
		return testkit.Fixture{}, errors.New("离线候选写入失败")
	}
	return f, nil
}
