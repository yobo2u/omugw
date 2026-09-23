package credential

import (
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/canonical"
)

// TestRetryAfterSurvivesOlderLeaseSuccess 防的是旧请求晚到的成功响应，
// 清掉新请求刚写入的上游 Retry-After。
func TestRetryAfterSurvivesOlderLeaseSuccess(t *testing.T) {
	p, _ := newPool(t, key("a"))
	older, err := p.Acquire(nil)
	if err != nil {
		t.Fatal(err)
	}
	newer, err := p.Acquire(nil)
	if err != nil {
		t.Fatal(err)
	}
	rateLimit := canonical.Newf(canonical.ClassRateLimit, "local 429")
	rateLimit.RetryAfter = 5 * time.Minute
	newer.Fail(rateLimit)
	before := p.Stats()[0]

	older.Succeed()

	after := p.Stats()[0]
	if after.Available || after.CooldownUntil.Before(before.CooldownUntil) {
		t.Fatal("旧租约成功清除了较新的 Retry-After")
	}
}
