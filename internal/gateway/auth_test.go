package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yobo2u/omugw/internal/config"
)

func TestAuthenticateUnique(t *testing.T) {
	a := NewAuthenticator([]config.AuthKey{{ID: "caller", Key: "synthetic-key"}})
	for _, tc := range []struct {
		name   string
		header http.Header
		ok     bool
	}{
		{"bearer", http.Header{"Authorization": {"Bearer synthetic-key"}}, true},
		{"api-key", http.Header{"Api-Key": {"synthetic-key"}}, true},
		{"duplicate", http.Header{"Authorization": {"Bearer synthetic-key", "Bearer synthetic-key"}}, false},
		{"mixed-case", http.Header{"Authorization": {"Bearer synthetic-key"}, "authorization": {"Bearer synthetic-key"}}, false},
		{"two-sources", http.Header{"Authorization": {"Bearer synthetic-key"}, "Api-Key": {"synthetic-key"}}, false},
		{"control", http.Header{"Api-Key": {"synthetic-key\n"}}, false},
		{"cookie", http.Header{"Cookie": {"api-key=synthetic-key"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.Header = tc.header
			caller, err := a.AuthenticateUnique(r)
			if (err == nil) != tc.ok || tc.ok && caller.ID != "caller" {
				t.Fatal("唯一请求头鉴权未守住来源边界")
			}
		})
	}
}
