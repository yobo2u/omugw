package dashscopeinference

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

func TestInferenceLocalFailureEncoding(t *testing.T) {
	for _, tc := range []struct {
		kind          LocalFailureKind
		code, message string
	}{
		{LocalPolicy, "Gateway.InvalidTask", "invalid task envelope or binding"},
		{LocalUnsupportedTask, "Gateway.UnsupportedTask", "task contract not implemented"},
		{LocalStartTimeout, "Gateway.TaskStartTimeout", "task did not start within gateway budget"},
		{LocalDrainTimeout, "Gateway.TaskDrainTimeout", "task did not finish within gateway budget"},
	} {
		want := `{"header":{"event":"task-failed","task_id":"a","error_code":"` + tc.code + `","error_message":"` + tc.message + `"},"payload":{}}`
		n, err := TaskFailureSize("a", tc.kind)
		if err != nil || n != len(want) {
			t.Fatalf("size = %d, %v; want %d", n, err, len(want))
		}
		guard := bytes.Repeat([]byte{'!'}, n+2)
		if err := PutTaskFailure(guard[1:n+1], "a", tc.kind); err != nil {
			t.Fatal(err)
		}
		if string(guard[1:n+1]) != want || guard[0] != '!' || guard[n+1] != '!' {
			t.Fatalf("encoding = %s", guard)
		}
		for _, size := range []int{0, n - 1, n + 1} {
			dst := bytes.Repeat([]byte{'!'}, size)
			before := bytes.Clone(dst)
			if err := PutTaskFailure(dst, "a", tc.kind); err == nil || !bytes.Equal(dst, before) {
				t.Errorf("nonexact dst written: %d", size)
			}
		}
	}
	for _, id := range []string{"任务-😀", "a\"\\\n\r\t\b\f\x00\x1f", strings.Repeat("\x01", 512)} {
		n, err := TaskFailureSize(id, LocalPolicy)
		if err != nil || n > 4096 {
			t.Fatalf("escaped size = %d, %v", n, err)
		}
		dst := make([]byte, n)
		if err := PutTaskFailure(dst, id, LocalPolicy); err != nil {
			t.Fatal(err)
		}
		var envelope struct {
			Header struct {
				TaskID string `json:"task_id"`
			} `json:"header"`
		}
		if err := json.Unmarshal(dst, &envelope); err != nil || envelope.Header.TaskID != id {
			t.Fatalf("ID round trip: %q, %v", dst, err)
		}
	}
	want := `{"header":{"event":"task-failed","task_id":"a\"\\\n\u0000","error_code":"Gateway.InvalidTask","error_message":"invalid task envelope or binding"},"payload":{}}`
	n, err := TaskFailureSize("a\"\\\n\x00", LocalPolicy)
	if err != nil || n != len(want) {
		t.Fatalf("precise escaped size: %d, %v", n, err)
	}
	dst := make([]byte, n)
	if err := PutTaskFailure(dst, "a\"\\\n\x00", LocalPolicy); err != nil || string(dst) != want {
		t.Fatalf("precise escaped encoding: %s, %v", dst, err)
	}
	for _, tc := range []struct {
		id   string
		kind LocalFailureKind
	}{{"", LocalPolicy}, {strings.Repeat("x", 513), LocalPolicy}, {"\xff", LocalPolicy}, {"a", LocalFailureKind(255)}} {
		if size, err := TaskFailureSize(tc.id, tc.kind); err == nil || size != 0 {
			t.Errorf("invalid encoding size: %d, %v", size, err)
		}
		dst := bytes.Repeat([]byte{'!'}, 4096)
		before := bytes.Clone(dst)
		if err := PutTaskFailure(dst, tc.id, tc.kind); err == nil || !bytes.Equal(dst, before) {
			t.Error("invalid encoding wrote bytes")
		}
	}
	if allocs := testing.AllocsPerRun(100, func() { _, _ = TaskFailureSize("a\"\\\n\x00", LocalPolicy) }); allocs != 0 {
		t.Fatalf("size allocated payload: %g", allocs)
	}
	if allocs := testing.AllocsPerRun(100, func() { _ = PutTaskFailure(dst, "a\"\\\n\x00", LocalPolicy) }); allocs != 0 {
		t.Fatalf("put allocated payload: %g", allocs)
	}
}

func TestInferenceCloseClassification(t *testing.T) {
	for _, code := range []uint16{0, 1000, 1001, 1002, 1005, 1006, 1008, 1009, 1011, 1013, 4000, 65535} {
		for _, reason := range []string{"", "InvalidApiKey", "Throttling", strings.Repeat("secret-reason", 10000)} {
			got := ClassifyClose(code, reason)
			if code == 1000 || code == 1001 {
				if got != nil {
					t.Errorf("normal close %d: %+v", code, got)
				}
				continue
			}
			if got == nil || got.Class != canonical.ClassInternal || got.Retryable || got.UpstreamCode != "" || got.UpstreamRequestID != "" || got.UpstreamStatus != 0 || got.Param != "" || got.Unwrap() != nil || reason != "" && strings.Contains(got.Message, reason) {
				t.Errorf("unsafe close %d: %+v", code, got)
			}
		}
	}
}
