package canonical

import (
	"net/http"
	"testing"
)

// TestRemainingOnlyRateLimitHeaderRoundTrips 防的是额度只返回 Remaining 时，
// 因 Limit 缺失或 Remaining 为零而把整个限流信息判空。
func TestRemainingOnlyRateLimitHeaderRoundTrips(t *testing.T) {
	for _, value := range []string{"0", "5"} {
		t.Run(value, func(t *testing.T) {
			h := http.Header{}
			h.Set("X-RateLimit-Remaining-Requests", value)
			rl := ParseRateLimitHeaders(h)
			if rl == nil {
				t.Fatal("只含 Remaining 的限流头被丢弃")
			}
			err := Newf(ClassRateLimit, "slow down")
			err.RateLimit = rl
			if got := err.Headers()["X-RateLimit-Remaining-Requests"]; got != value {
				t.Fatalf("Remaining 往返后 = %q, 期望 %q", got, value)
			}
		})
	}
}
