package testkit

import (
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

// 去掉任一去敏分支、覆盖重复 query 或原地改写输入，都会使这些字面预期失败。
func TestWSHandshakeSanitization(t *testing.T) {
	t.Run("secrets_and_repeated_query", func(t *testing.T) {
		input := Request{
			Method: http.MethodGet, Path: "/v1/realtime",
			Query: "token=synthetic-token-a&token=synthetic-token-b&key=synthetic-key-a&key=synthetic-key-b&access_token=synthetic-access-a&access_token=synthetic-access-b&TOKEN=synthetic-case&api_key=synthetic-api&model=qwen+realtime&tag=a&tag=a&tag=b",
			Headers: map[string]string{
				"aUtHoRiZaTiOn": "Bearer synthetic-authorization", "X-API-KEY": "synthetic-header-key",
				"cOoKiE": "session=synthetic-cookie", "X-Safe": "version-1",
				"SEC-WEBSOCKET-PROTOCOL": "realtime, openai-insecure-api-key.synthetic-subprotocol, openai-beta.realtime-v1",
			},
		}
		before, _ := json.Marshal(input)
		got, err := SanitizeWSHandshake(input)
		if err != nil {
			t.Fatal(err)
		}
		want := Request{
			Method: http.MethodGet, Path: "/v1/realtime",
			Query: "model=qwen+realtime&tag=a&tag=a&tag=b",
			Headers: map[string]string{
				"authorization": "<redacted>", "x-api-key": "<redacted>", "cookie": "<redacted>",
				"x-safe": "version-1", "sec-websocket-protocol": "realtime, openai-beta.realtime-v1",
			},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatal("握手去敏未保留安全字段或未剥离全部凭据")
		}
		after, _ := json.Marshal(input)
		if string(before) != string(after) {
			t.Fatal("去敏改写了调用方持有的握手")
		}
		again, err := SanitizeWSHandshake(got)
		if err != nil || !reflect.DeepEqual(again, want) {
			t.Fatal("已去敏握手再次处理后发生变化")
		}
	})

	t.Run("existing_secret_headers_are_not_weakened", func(t *testing.T) {
		input := Request{Method: http.MethodGet, Path: "/v1/realtime", Headers: make(map[string]string)}
		for name := range secretHeaders {
			input.Headers[strings.ToUpper(name)] = "synthetic-private-header"
		}
		got, err := SanitizeWSHandshake(input)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Headers) != len(input.Headers) {
			t.Fatal("去敏丢失了敏感头存在性")
		}
		for name := range secretHeaders {
			if got.Headers[name] != "<redacted>" {
				t.Fatal("既有敏感头未去敏")
			}
		}
		if err := validateSecretHeaders("synthetic", "request", got.Headers); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("auth_only_protocol_is_removed", func(t *testing.T) {
		got, err := SanitizeWSHandshake(Request{Method: http.MethodGet, Path: "/v1/realtime", Headers: map[string]string{
			"Sec-WebSocket-Protocol": "openai-insecure-api-key.synthetic-private",
		}})
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Headers) != 0 {
			t.Fatal("去掉凭据后仍留下子协议头")
		}
	})

	t.Run("sensitive_header_case_aliases", func(t *testing.T) {
		got, err := SanitizeWSHandshake(Request{Method: http.MethodGet, Path: "/v1/realtime", Headers: map[string]string{
			"authorization": "synthetic-one", "Authorization": "synthetic-two",
			"Sec-WebSocket-Protocol": "realtime, OPENAI-INSECURE-API-KEY.synthetic-three",
		}})
		if err != nil {
			t.Fatal(err)
		}
		if got.Headers["authorization"] != "<redacted>" || got.Headers["sec-websocket-protocol"] != "realtime" {
			t.Fatal("大小写别名绕过凭据去敏")
		}
	})

	for _, tc := range []struct {
		name string
		r    Request
		path string
	}{
		{"url_userinfo", Request{Method: "GET", Path: "wss://synthetic-user:synthetic-private@localhost/v1/realtime"}, "request.path"},
		{"path_query", Request{Method: "GET", Path: "/v1/realtime?token=synthetic-private"}, "request.path"},
		{"path_fragment", Request{Method: "GET", Path: "/v1/realtime#synthetic-private"}, "request.path"},
		{"malformed_query", Request{Method: "GET", Path: "/v1/realtime", Query: "token=synthetic-private&model=%zz"}, "request.query"},
		{"query_semicolon", Request{Method: "GET", Path: "/v1/realtime", Query: "token=synthetic-private;model=x"}, "request.query"},
		{"unknown_auth_protocol", Request{Method: "GET", Path: "/v1/realtime", Headers: map[string]string{"Sec-WebSocket-Protocol": "realtime, bearer.synthetic-private"}}, "request.headers"},
		{"unknown_variable_protocol", Request{Method: "GET", Path: "/v1/realtime", Headers: map[string]string{"Sec-WebSocket-Protocol": "realtime, vendor-token.synthetic-private"}}, "request.headers"},
		{"opaque_secret_protocol", Request{Method: "GET", Path: "/v1/realtime", Headers: map[string]string{"Sec-WebSocket-Protocol": "synthetic-private"}}, "request.headers"},
		{"empty_auth_protocol", Request{Method: "GET", Path: "/v1/realtime", Headers: map[string]string{"Sec-WebSocket-Protocol": "openai-insecure-api-key."}}, "request.headers"},
		{"empty_protocol_token", Request{Method: "GET", Path: "/v1/realtime", Headers: map[string]string{"Sec-WebSocket-Protocol": "realtime,,openai-beta.realtime-v1"}}, "request.headers"},
		{"safe_header_alias_collision", Request{Method: "GET", Path: "/v1/realtime", Headers: map[string]string{"X-Safe": "one", "x-safe": "two"}}, "request.headers"},
		{"auth_only_protocol_alias_collision", Request{Method: "GET", Path: "/v1/realtime", Headers: map[string]string{"Sec-WebSocket-Protocol": "openai-insecure-api-key.synthetic-one", "sec-websocket-protocol": "openai-insecure-api-key.synthetic-two"}}, "request.headers"},
		{"invalid_header_name", Request{Method: "GET", Path: "/v1/realtime", Headers: map[string]string{"X Bad": "synthetic-private"}}, "request.headers"},
		{"invalid_header_value", Request{Method: "GET", Path: "/v1/realtime", Headers: map[string]string{"X-Safe": "synthetic-private\r\nInjected: yes"}}, "request.headers"},
		{"wrong_method", Request{Method: "POST", Path: "/v1/realtime"}, "request.method"},
		{"empty_path", Request{Method: "GET"}, "request.path"},
		{"body_is_not_a_handshake", Request{Method: "GET", Path: "/v1/realtime", Body: json.RawMessage(`{"secret":"synthetic-private"}`)}, "request.body"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SanitizeWSHandshake(tc.r)
			assertWSHandshakeError(t, err, tc.path)
			if !reflect.DeepEqual(got, Request{}) {
				t.Fatal("失败的去敏返回了可能未清洗的握手")
			}
		})
	}
}

// 错路由、Get 丢多值、contains 误判与 nonce 字面比较都会破坏下面的实际请求断言。
func TestWSHandshakeMatch(t *testing.T) {
	want := Request{Method: "GET", Path: "/v1/realtime", Query: "model=qwen+realtime&tag=a&tag=a&tag=b", Headers: map[string]string{
		"Authorization": "<redacted>", "X-Safe": "version-1", "OpenAI-Beta": "realtime=v1",
		"Sec-WebSocket-Protocol":   "realtime, openai-beta.realtime-v1",
		"Sec-WebSocket-Extensions": "permessage-deflate; client_max_window_bits, x-test-extension",
		"Sec-WebSocket-Key":        "dGhlIHNhbXBsZSBub25jZQ==", "Sec-WebSocket-Version": "13",
		"Connection": "Upgrade", "Upgrade": "websocket",
	}}
	t.Run("valid_multivalue_and_fresh_nonce", func(t *testing.T) {
		got := newWSHandshakeRequest(t)
		if err := MatchWSHandshake(want, got); err != nil {
			t.Fatal(err)
		}
		// 占位不能验密钥正确性；本地驱动必须另查自己注入的合成字面凭据。
		if got.Header.Get("Authorization") != "Bearer synthetic-outbound" {
			t.Fatal("独立的合成凭据断言失败")
		}
		got.Header.Set("Authorization", "Bearer synthetic-wrong-outbound")
		if err := MatchWSHandshake(want, got); err != nil {
			t.Fatal("redacted 占位错误地承担了密钥正确性校验")
		}
	})

	t.Run("automatic_headers_need_not_be_recorded", func(t *testing.T) {
		minimal := Request{Method: "GET", Path: "/v1/realtime", Query: want.Query}
		got := newWSHandshakeRequest(t)
		got.Header.Del("Sec-WebSocket-Protocol")
		got.Header.Del("Sec-WebSocket-Extensions")
		if err := MatchWSHandshake(minimal, got); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("unrecorded_negotiation_is_not_ignored", func(t *testing.T) {
		minimal := Request{Method: "GET", Path: "/v1/realtime", Query: want.Query}
		for _, name := range []string{"Sec-WebSocket-Protocol", "Sec-WebSocket-Extensions"} {
			got := newWSHandshakeRequest(t)
			if name == "Sec-WebSocket-Protocol" {
				got.Header.Del("Sec-WebSocket-Extensions")
			} else {
				got.Header.Del("Sec-WebSocket-Protocol")
			}
			assertWSHandshakeError(t, MatchWSHandshake(minimal, got), "request.headers")
		}
	})

	t.Run("query_duplicate_multiplicity_and_empty_value", func(t *testing.T) {
		w := Request{Method: "GET", Path: "/v1/realtime", Query: "empty=&model=qwen+realtime&tag=a&tag=a&tag=b", Headers: want.Headers}
		got := newWSHandshakeRequest(t)
		got.URL.RawQuery += "&empty="
		if err := MatchWSHandshake(w, got); err != nil {
			t.Fatal(err)
		}
		got.URL.RawQuery = "empty=&model=qwen+realtime&tag=a&tag=b&tag=b"
		assertWSHandshakeError(t, MatchWSHandshake(w, got), "request.query")
		got.URL.RawQuery = "model=qwen+realtime&tag=a&tag=a&tag=b"
		assertWSHandshakeError(t, MatchWSHandshake(w, got), "request.query")
	})

	t.Run("quoted_extension_comma_is_one_value", func(t *testing.T) {
		w := Request{Method: "GET", Path: "/v1/realtime", Query: want.Query, Headers: map[string]string{
			"Sec-WebSocket-Extensions": `x-test; parameter="a,b", permessage-deflate`,
		}}
		got := newWSHandshakeRequest(t)
		got.Header.Del("Sec-WebSocket-Protocol")
		got.Header["Sec-Websocket-Extensions"] = []string{"permessage-deflate", `x-test; parameter="a,b"`}
		if err := MatchWSHandshake(w, got); err != nil {
			t.Fatal(err)
		}
		got.Header["Sec-Websocket-Extensions"] = []string{"permessage-deflate", `x-test; parameter="a`, `b"`}
		assertWSHandshakeError(t, MatchWSHandshake(w, got), "request.headers")
	})

	t.Run("sensitive_query_and_auth_protocol_are_scrubbed", func(t *testing.T) {
		got := newWSHandshakeRequest(t)
		got.URL.RawQuery += "&token=synthetic-private&token=synthetic-private-again&KEY=synthetic-private-key"
		got.Header.Add("Sec-WebSocket-Protocol", "openai-insecure-api-key.synthetic-private")
		if err := MatchWSHandshake(want, got); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got.URL.RawQuery, "synthetic-private") || len(got.Header.Values("Sec-WebSocket-Protocol")) != 3 {
			t.Fatal("匹配改写了实际请求")
		}
	})

	t.Run("case_insensitive_actual_header_names", func(t *testing.T) {
		got := newWSHandshakeRequest(t)
		got.Header["aUtHoRiZaTiOn"] = got.Header["Authorization"]
		delete(got.Header, "Authorization")
		got.Header["sEc-WeBsOcKeT-pRoToCoL"] = got.Header["Sec-Websocket-Protocol"]
		delete(got.Header, "Sec-Websocket-Protocol")
		if err := MatchWSHandshake(want, got); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("host_uses_real_request_host", func(t *testing.T) {
		w := Request{Method: "GET", Path: "/v1/realtime", Query: want.Query, Headers: map[string]string{"Host": "fixture.local"}}
		got := newWSHandshakeRequest(t)
		got.Header.Del("Sec-WebSocket-Protocol")
		got.Header.Del("Sec-WebSocket-Extensions")
		if err := MatchWSHandshake(w, got); err != nil {
			t.Fatal(err)
		}
		got.Host = "other.local"
		assertWSHandshakeError(t, MatchWSHandshake(w, got), "request.headers")
	})

	for _, tc := range []struct {
		name   string
		mutate func(*http.Request)
		path   string
	}{
		{"wrong_method", func(r *http.Request) { r.Method = "POST" }, "request.method"},
		{"wrong_path", func(r *http.Request) { r.URL.Path = "/synthetic-private" }, "request.path"},
		{"escaped_slash_is_not_same_route", func(r *http.Request) { r.URL.RawPath = "/v1%2Frealtime" }, "request.path"},
		{"wrong_model", func(r *http.Request) { r.URL.RawQuery = "model=synthetic-private&tag=a&tag=a&tag=b" }, "request.query"},
		{"missing_query_value", func(r *http.Request) { r.URL.RawQuery = "model=qwen+realtime&tag=a&tag=b" }, "request.query"},
		{"extra_query_value", func(r *http.Request) { r.URL.RawQuery += "&tag=a" }, "request.query"},
		{"extra_query_key", func(r *http.Request) { r.URL.RawQuery += "&synthetic-private=value" }, "request.query"},
		{"empty_query_value", func(r *http.Request) { r.URL.RawQuery += "&tag=" }, "request.query"},
		{"malformed_query", func(r *http.Request) { r.URL.RawQuery += "&token=synthetic-private&x=%zz" }, "request.query"},
		{"url_userinfo", func(r *http.Request) { r.URL.User = url.UserPassword("synthetic-user", "synthetic-private") }, "request.path"},
		{"url_fragment", func(r *http.Request) { r.URL.Fragment = "synthetic-private" }, "request.path"},
		{"missing_safe_header", func(r *http.Request) { r.Header.Del("X-Safe") }, "request.headers"},
		{"wrong_safe_header", func(r *http.Request) { r.Header.Set("OpenAI-Beta", "synthetic-private") }, "request.headers"},
		{"extra_safe_header_value", func(r *http.Request) { r.Header.Add("X-Safe", "version-1") }, "request.headers"},
		{"missing_authorization", func(r *http.Request) { r.Header.Del("Authorization") }, "request.headers"},
		{"empty_authorization", func(r *http.Request) { r.Header.Set("Authorization", "") }, "request.headers"},
		{"placeholder_not_a_credential", func(r *http.Request) { r.Header.Set("Authorization", "<redacted>") }, "request.headers"},
		{"extra_authorization_value", func(r *http.Request) { r.Header.Add("Authorization", "Bearer synthetic-private") }, "request.headers"},
		{"safe_header_control_character", func(r *http.Request) { r.Header.Set("X-Safe", "synthetic-private\r\nInjected: yes") }, "request.headers"},
		{"missing_protocol_token", func(r *http.Request) { r.Header.Set("Sec-WebSocket-Protocol", "realtime") }, "request.headers"},
		{"extra_protocol_token", func(r *http.Request) { r.Header.Add("Sec-WebSocket-Protocol", "realtime") }, "request.headers"},
		{"protocol_values_are_case_sensitive", func(r *http.Request) { r.Header.Set("Sec-WebSocket-Protocol", "Realtime, openai-beta.realtime-v1") }, "request.headers"},
		{"unknown_sensitive_protocol", func(r *http.Request) { r.Header.Add("Sec-WebSocket-Protocol", "vendor-token.synthetic-private") }, "request.headers"},
		{"missing_extension_token", func(r *http.Request) { r.Header.Set("Sec-WebSocket-Extensions", "x-test-extension") }, "request.headers"},
		{"extra_extension_token", func(r *http.Request) { r.Header.Add("Sec-WebSocket-Extensions", "synthetic-private") }, "request.headers"},
		{"missing_connection", func(r *http.Request) { r.Header.Del("Connection") }, "request.headers"},
		{"substring_not_upgrade", func(r *http.Request) { r.Header.Set("Connection", "notupgrade") }, "request.headers"},
		{"connection_later_value_checked", func(r *http.Request) { r.Header.Add("Connection", "bad token") }, "request.headers"},
		{"missing_upgrade", func(r *http.Request) { r.Header.Del("Upgrade") }, "request.headers"},
		{"wrong_upgrade", func(r *http.Request) { r.Header.Set("Upgrade", "synthetic-private") }, "request.headers"},
		{"extra_upgrade", func(r *http.Request) { r.Header.Add("Upgrade", "synthetic-private") }, "request.headers"},
		{"missing_version", func(r *http.Request) { r.Header.Del("Sec-WebSocket-Version") }, "request.headers"},
		{"wrong_version", func(r *http.Request) { r.Header.Set("Sec-WebSocket-Version", "12") }, "request.headers"},
		{"duplicate_version", func(r *http.Request) { r.Header.Add("Sec-WebSocket-Version", "13") }, "request.headers"},
		{"missing_nonce", func(r *http.Request) { r.Header.Del("Sec-WebSocket-Key") }, "request.headers"},
		{"invalid_nonce", func(r *http.Request) { r.Header.Set("Sec-WebSocket-Key", "synthetic-private") }, "request.headers"},
		{"short_nonce", func(r *http.Request) { r.Header.Set("Sec-WebSocket-Key", "c2hvcnQ=") }, "request.headers"},
		{"duplicate_nonce", func(r *http.Request) { r.Header.Add("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==") }, "request.headers"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := newWSHandshakeRequest(t)
			tc.mutate(got)
			assertWSHandshakeError(t, MatchWSHandshake(want, got), tc.path)
		})
	}

	t.Run("unsafe_expected_credentials_are_rejected", func(t *testing.T) {
		w := Request{Method: "GET", Path: "/v1/realtime", Query: want.Query, Headers: map[string]string{"AUTHORIZATION": "Bearer synthetic-private"}}
		assertWSHandshakeError(t, MatchWSHandshake(w, newWSHandshakeRequest(t)), "request.headers")
	})
	t.Run("nil_actual_request", func(t *testing.T) {
		assertWSHandshakeError(t, MatchWSHandshake(want, nil), "request")
	})
	t.Run("nil_actual_url", func(t *testing.T) {
		got := newWSHandshakeRequest(t)
		got.URL = nil
		assertWSHandshakeError(t, MatchWSHandshake(want, got), "request.path")
	})
}

// Unicode 空白不是 HTTP OWS，不能被修剪成合法自动头或协商 token 而制造回放伪绿。
func TestWSHandshakeHTTPWhitespace(t *testing.T) {
	want := Request{Method: "GET", Path: "/v1/realtime", Query: "model=qwen+realtime&tag=a&tag=a&tag=b", Headers: map[string]string{
		"Sec-WebSocket-Protocol":   "realtime, openai-beta.realtime-v1",
		"Sec-WebSocket-Extensions": "permessage-deflate; client_max_window_bits, x-test-extension",
	}}
	for _, space := range []struct {
		name  string
		value string
		valid bool
	}{
		{"sp_htab", " \t", true},
		{"nbsp", "\u00a0", false},
		{"em_space", "\u2003", false},
	} {
		t.Run(space.name, func(t *testing.T) {
			for _, header := range []struct {
				name  string
				value string
			}{
				{"Connection", "Upgrade"},
				{"Upgrade", "WebSocket"},
				{"Sec-WebSocket-Version", "13"},
				{"Sec-WebSocket-Key", "AAECAwQFBgcICQoLDA0ODw=="},
				{"Sec-WebSocket-Protocol", "realtime, " + space.value + "openai-beta.realtime-v1"},
				{"Sec-WebSocket-Extensions", "permessage-deflate; client_max_window_bits, " + space.value + "x-test-extension"},
			} {
				t.Run(header.name, func(t *testing.T) {
					got := newWSHandshakeRequest(t)
					got.Header.Set(header.name, space.value+header.value+space.value)
					err := MatchWSHandshake(want, got)
					if space.valid {
						if err != nil {
							t.Fatal("合法 SP/HTAB OWS 被拒绝")
						}
					} else {
						assertWSHandshakeError(t, err, "request.headers."+strings.ToLower(header.name))
					}
				})
			}
			t.Run("sanitize_protocol", func(t *testing.T) {
				input := Request{Method: "GET", Path: "/v1/realtime", Headers: map[string]string{
					"Sec-WebSocket-Protocol": space.value + "realtime, " + space.value + "openai-insecure-api-key.synthetic-private, " + space.value + "openai-beta.realtime-v1" + space.value,
				}}
				got, err := SanitizeWSHandshake(input)
				if space.valid {
					if err != nil || got.Headers["sec-websocket-protocol"] != "realtime, openai-beta.realtime-v1" {
						t.Fatal("合法 OWS 的固定协议或凭据去敏被破坏")
					}
				} else {
					assertWSHandshakeError(t, err, "request.headers.sec-websocket-protocol")
					if !reflect.DeepEqual(got, Request{}) {
						t.Fatal("非法 Unicode OWS 去敏返回了握手内容")
					}
				}
			})
		})
	}
}

func newWSHandshakeRequest(t *testing.T) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodGet, "http://fixture.local/v1/realtime?tag=b&model=qwen%20realtime&tag=a&tag=a", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header = http.Header{
		"Authorization": {"Bearer synthetic-outbound"}, "X-Safe": {"version-1"}, "Openai-Beta": {"realtime=v1"},
		"Connection": {"keep-alive", "UpGrAdE"}, "Upgrade": {"WebSocket"},
		"Sec-Websocket-Version": {"13"}, "Sec-Websocket-Key": {"AAECAwQFBgcICQoLDA0ODw=="},
		"Sec-Websocket-Protocol":   {"openai-beta.realtime-v1", "realtime"},
		"Sec-Websocket-Extensions": {"x-test-extension", "permessage-deflate; client_max_window_bits"},
	}
	return r
}

func assertWSHandshakeError(t *testing.T, err error, field string) {
	t.Helper()
	if err == nil {
		t.Fatal("非法握手或不匹配握手被放行")
	}
	if !strings.HasPrefix(err.Error(), field) {
		t.Fatal("错误未定位预期字段路径")
	}
	switch err.Error() {
	case "request", "request.method", "request.path", "request.query", "request.body", "request.headers",
		"request.headers.connection", "request.headers.upgrade", "request.headers.sec-websocket-key",
		"request.headers.sec-websocket-version", "request.headers.sec-websocket-protocol", "request.headers.sec-websocket-extensions":
	default:
		t.Fatal("错误含字段路径之外的内容")
	}
	for _, content := range []string{"synthetic-", "Bearer", "%zz", "Injected", "vendor-token", "dGhlIHNhbXBsZSBub25jZQ=="} {
		if strings.Contains(err.Error(), content) {
			t.Fatal("握手错误泄漏了输入内容")
		}
	}
}
