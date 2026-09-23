package openairesponses

import (
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/transport/sse"
)

// TestRewriteStoredResponseRejectsNull 防的是合法 JSON null 解成 nil map 后写字段崩溃。
func TestRewriteStoredResponseRejectsNull(t *testing.T) {
	_, _, err := RewriteStoredResponse([]byte("null"), "resp_local", "", false)
	if err == nil {
		t.Fatal("null 响应应当失败")
	}
	if got := canonical.AsError(err).Class; got != canonical.ClassUpstreamUnavailable {
		t.Fatalf("错误分类 = %q，期望 %q", got, canonical.ClassUpstreamUnavailable)
	}
}

// TestRewriteStoredStreamEventRejectsNullResponse 防的是终止事件把 response:null
// 解成 nil map 后写本地会话字段崩溃。
func TestRewriteStoredStreamEventRejectsNullResponse(t *testing.T) {
	ev := sse.Event{
		Event: "response.completed",
		Data:  `{"type":"response.completed","response":null}`,
	}
	_, _, _, err := RewriteStoredStreamEvent(ev, "resp_local", "", false)
	if err == nil {
		t.Fatal("response:null 事件应当失败")
	}
	if got := canonical.AsError(err).Class; got != canonical.ClassUpstreamUnavailable {
		t.Fatalf("错误分类 = %q，期望 %q", got, canonical.ClassUpstreamUnavailable)
	}
}

func TestIncompleteFunctionCallIsPreservedWithoutProjection(t *testing.T) {
	body := []byte(`{"id":"resp_up","object":"response","status":"incomplete",` +
		`"incomplete_details":{"reason":"max_output_tokens"},"output":[{` +
		`"type":"function_call","id":"fc_1","call_id":"call_1","name":"f",` +
		`"arguments":"{\"x\":"}]}`)

	_, stored, err := RewriteStoredResponse(body, "resp_local", "", true)
	if err != nil {
		t.Fatalf("合法 incomplete 响应被改成错误: %v", err)
	}
	if len(stored.Items) != 1 || len(stored.Messages) != 0 {
		t.Fatalf("未完成调用应只保留原始条目: %+v", stored)
	}

	ev := sse.Event{
		Event: "response.incomplete",
		Data:  `{"type":"response.incomplete","response":` + string(body) + `}`,
	}
	_, streamStored, terminal, err := RewriteStoredStreamEvent(ev, "resp_local", "", true)
	if err != nil {
		t.Fatalf("合法 incomplete 终止事件被改成错误: %v", err)
	}
	if !terminal || len(streamStored.Items) != 1 || len(streamStored.Messages) != 0 {
		t.Fatalf("流式未完成调用没有保留原始条目: terminal=%v stored=%+v", terminal, streamStored)
	}
}

func TestStoredImageGenerationCountsInlineBytes(t *testing.T) {
	body := []byte(`{"status":"completed","output":[{` +
		`"type":"image_generation_call","id":"ig_1","status":"completed","result":"QUJD"}]}`)
	_, stored, err := RewriteStoredResponse(body, "resp_local", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if stored.InlineBytes != 3 {
		t.Fatalf("生成图片内联字节 = %d，期望 3", stored.InlineBytes)
	}
}
