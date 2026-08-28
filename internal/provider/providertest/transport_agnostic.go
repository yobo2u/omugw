package providertest

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
)

// runTransportAgnostic 跑对任何出站适配器都成立的不变量。
//
// 这一族不引用 httptest 之外的 HTTP 概念（method、Accept、路径拼接都不在这里），
// 是为了 Phase 1 将要接入的三条 WebSocket 出站能原样复用——届时新增 WS driver
// 与 WS 专属族，这四条不必重写。
func runTransportAgnostic(t *testing.T, s Subject) {
	t.Run("身份", func(t *testing.T) {
		h := okServer(t)
		p := s.New(t, h.deps)
		if p.Kind() != s.Kind {
			t.Errorf("Kind() = %q，期望 %q", p.Kind(), s.Kind)
		}
	})

	// 凭据隔离是安全约束，不是风格问题：把客户端发来的 Authorization 转给上游，
	// 等于把一份密钥泄露给一个没有理由知道它的第三方。
	t.Run("凭据隔离", func(t *testing.T) {
		h := okServer(t)

		clientHeader := http.Header{}
		clientHeader.Set("Authorization", "Bearer "+clientSecret)

		if _, err := h.call(t, s, callOpts{header: clientHeader}); err != nil {
			t.Fatal(err)
		}

		auth := h.got.header.Get("Authorization")
		if auth != "Bearer "+gatewaySecret {
			t.Errorf("Authorization = %q，期望网关自己的凭据", auth)
		}
		if strings.Contains(auth, clientSecret) {
			t.Error("客户端密钥被转发给了上游")
		}
	})

	// 路由目标缺上游模型名是网关自己的装配错误，不是客户端的错。
	// 判成 bad_request 会误导客户端去改一个它没写错的请求。
	t.Run("装配错误归网关", func(t *testing.T) {
		h := okServer(t)
		_, err := h.call(t, s, callOpts{emptyUpstreamModel: true})
		assertClass(t, err, canonical.ClassInternal)
	})

	// 请求体不合法要在适配器边界就拦下，而不是把一段垃圾原样打给上游，
	// 再让上游给出一条与网关无关的错——那条错会把排查引到错误的方向。
	t.Run("客户端错误归客户端", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			body string
		}{
			{"非 JSON", `not json at all`},
			{"JSON 数组", `[1,2,3]`},
			{"JSON 字符串", `"a string"`},
			{"缺 model", `{"input":"hi"}`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				h := okServer(t)
				_, err := h.call(t, s, callOpts{body: tc.body})
				assertClass(t, err, canonical.ClassBadRequest)
			})
		}
	})
}

// assertClass 断言错误是 *canonical.Error 且分类正确。
func assertClass(t *testing.T, err error, want canonical.ErrorClass) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望 %q 错误，实际为 nil", want)
	}
	var cerr *canonical.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("应返回 *canonical.Error，实际为 %T: %v", err, err)
	}
	if cerr.Class != want {
		t.Errorf("分类 = %q，期望 %q", cerr.Class, want)
	}
}
