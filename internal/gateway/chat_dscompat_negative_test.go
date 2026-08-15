package gateway

import (
	"net/http"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

// TestChatDSCompatRejectsFileInput 固化 file_input 在这条路上是 REJECT：
// 422 说「改请求」，且必须在矩阵闸门就拦下，一个字节都不出门。
func TestChatDSCompatRejectsFileInput(t *testing.T) {
	up := newUpstream(t, func(http.ResponseWriter, *http.Request) {})
	hs := newChatDSCompatHarness(t, up)

	rec := hs.do(t, `{"model":"m","messages":[{"role":"user","content":[
		{"type":"text","text":"处理这个文件"},
		{"type":"file","file":{"file_id":"file-abc"}}]}]}`, true)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("状态码 = %d，期望 422: %s", rec.Code, rec.Body.String())
	}
	if n := up.calls.Load(); n != 0 {
		t.Errorf("请求打到了上游 %d 次——REJECT 必须在矩阵闸门拦下", n)
	}
	if !strings.Contains(rec.Body.String(), string(canonical.CapFileInput)) {
		t.Errorf("错误应点名能力 file_input: %s", rec.Body.String())
	}
}

// TestChatDSCompatRejectsAudioOutput 固化 audio_output 在这条路上是 REJECT：
// 兼容模式不返回音频，想要音频输出请走 Realtime 或 Native 端点。
func TestChatDSCompatRejectsAudioOutput(t *testing.T) {
	up := newUpstream(t, func(http.ResponseWriter, *http.Request) {})
	hs := newChatDSCompatHarness(t, up)

	rec := hs.do(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],
		"modalities":["text","audio"]}`, true)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("状态码 = %d，期望 422: %s", rec.Code, rec.Body.String())
	}
	if n := up.calls.Load(); n != 0 {
		t.Errorf("请求打到了上游 %d 次——REJECT 必须在矩阵闸门拦下", n)
	}
	if !strings.Contains(rec.Body.String(), string(canonical.CapAudioOutput)) {
		t.Errorf("错误应点名能力 audio_output: %s", rec.Body.String())
	}
}

// TestChatDSCompatPlannedCandidatesStay501 固化「路由只包含尚未实现候选时维持 501」：
// dashscope.compatible 转正后，Chat 入站的未实现哨兵指向 anthropic.messages——
// 它仍是 PLANNED，501 说「等实现」，上游一个字节都不该收到。
func TestChatDSCompatPlannedCandidatesStay501(t *testing.T) {
	up := newUpstream(t, func(http.ResponseWriter, *http.Request) {})
	hs := newChatHarness(t, false, up)

	rec := hs.do(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, true)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("状态码 = %d，期望 501: %s", rec.Code, rec.Body.String())
	}
	if n := up.calls.Load(); n != 0 {
		t.Errorf("请求打到了上游 %d 次——PLANNED 路径不得出门", n)
	}
}

// TestChatDSCompatInvalidWebSearchOptionsIs400 固化形态非法的搜索选项在入站解码就拒：
// 不得默认为开启搜索，更不得打到上游。
func TestChatDSCompatInvalidWebSearchOptionsIs400(t *testing.T) {
	up := newUpstream(t, func(http.ResponseWriter, *http.Request) {})
	hs := newChatDSCompatHarness(t, up)

	for _, body := range []string{
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"web_search_options":{"unknown_field":1}}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"web_search_options":"yes"}`,
	} {
		rec := hs.do(t, body, true)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("状态码 = %d，期望 400: %s", rec.Code, rec.Body.String())
		}
	}
	if n := up.calls.Load(); n != 0 {
		t.Errorf("请求打到了上游 %d 次——入站解码失败不得出门", n)
	}
}
