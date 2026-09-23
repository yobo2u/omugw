package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

// TestUpstreamEOFIsRetryable 防的是 transport 自己取消派生 context 后，
// 把上游断连误判成调用方取消并返回 400。
func TestUpstreamEOFIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = New(testTimeouts(), nil).Do(ctx, req)
	if err == nil {
		t.Fatal("上游断连应返回错误")
	}
	cerr := canonical.AsError(err)
	if cerr.Class != canonical.ClassUpstreamUnavailable || !cerr.Retryable {
		t.Fatalf("上游断连被分类为 %s, retryable=%v", cerr.Class, cerr.Retryable)
	}
}
