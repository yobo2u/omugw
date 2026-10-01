package testkit

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// SanitizeWSHandshake 不原地清洗，防止调用方误把占位凭据当成可以发送的握手。
// 子协议只保留已知固定协议 token；未知变量 token 宁可拒绝，不能猜测它不含凭据。
func SanitizeWSHandshake(r Request) (Request, error) {
	if _, err := wsHandshakePath(r); err != nil {
		return Request{}, err
	}
	query, err := wsHandshakeQuery(r.Query)
	if err != nil {
		return Request{}, err
	}
	out := Request{Method: r.Method, Path: r.Path, Query: query.Encode()}
	if len(r.Headers) != 0 {
		out.Headers = make(map[string]string, len(r.Headers))
	}
	seen := make(map[string]bool, len(r.Headers))
	for name, value := range r.Headers {
		if !wsHTTPToken(name) || !wsHeaderValue(value) {
			return Request{}, errors.New("request.headers")
		}
		name = strings.ToLower(name)
		if secretHeaders[name] {
			out.Headers[name] = "<redacted>"
			continue
		}
		if seen[name] {
			return Request{}, errors.New("request.headers")
		}
		seen[name] = true
		if name == "sec-websocket-protocol" {
			tokens, err := wsProtocolTokens([]string{value})
			if err != nil {
				return Request{}, err
			}
			if len(tokens) == 0 {
				continue
			}
			value = strings.Join(tokens, ", ")
		}
		out.Headers[name] = value
	}
	return out, nil
}

// MatchWSHandshake 的占位只检查非空真实头的存在性，不能证明鉴权正确。
// 本地驱动必须独立核对合成凭据与下游不泄漏；此函数不发送任何请求或占位凭据。
func MatchWSHandshake(want Request, got *http.Request) error {
	if err := validateSecretHeaders("", "", want.Headers); err != nil {
		return errors.New("request.headers")
	}
	expected, err := SanitizeWSHandshake(want)
	if err != nil {
		return err
	}
	if got == nil {
		return errors.New("request")
	}
	if got.Method != expected.Method {
		return errors.New("request.method")
	}
	if got.URL == nil || got.URL.User != nil || got.URL.Fragment != "" || got.URL.Opaque != "" {
		return errors.New("request.path")
	}
	path, err := wsHandshakePath(expected)
	if err != nil {
		return err
	}
	if got.URL.EscapedPath() != path {
		return errors.New("request.path")
	}
	wantQuery, err := wsHandshakeQuery(expected.Query)
	if err != nil {
		return err
	}
	gotQuery, err := wsHandshakeQuery(got.URL.RawQuery)
	if err != nil {
		return err
	}
	if len(wantQuery) != len(gotQuery) {
		return errors.New("request.query")
	}
	for key, values := range wantQuery {
		if !wsEqualValues(values, gotQuery[key]) {
			// 不输出键名，防止误放在 query 键位置的凭据泄漏。
			return errors.New("request.query")
		}
	}

	// 不依赖 Header.Get 的首值，也不依赖手工构造的 Header 已经规范化键名。
	headers := make(map[string][]string, len(got.Header))
	for name, values := range got.Header {
		if !wsHTTPToken(name) {
			return errors.New("request.headers")
		}
		for _, value := range values {
			if !wsHeaderValue(value) {
				return errors.New("request.headers")
			}
		}
		name = strings.ToLower(name)
		headers[name] = append(headers[name], values...)
	}
	if err := wsRequiredHandshakeHeaders(headers); err != nil {
		return err
	}
	if values, exists := headers["sec-websocket-protocol"]; exists {
		tokens, err := wsProtocolTokens(values)
		if err != nil {
			return err
		}
		headers["sec-websocket-protocol"] = tokens
	}
	for _, name := range []string{"sec-websocket-protocol", "sec-websocket-extensions"} {
		if _, recorded := expected.Headers[name]; !recorded && len(headers[name]) != 0 {
			return errors.New("request.headers")
		}
	}
	for name, value := range expected.Headers {
		values := headers[name]
		if name == "host" {
			values = []string{got.Host}
		}
		switch name {
		case "connection", "upgrade", "sec-websocket-key", "sec-websocket-version":
			// 自动头只验有效性，避免每次拨号的新 nonce 与录制值产生伪失败。
			continue
		case "sec-websocket-protocol":
			if !wsEqualValues(strings.Split(value, ", "), values) {
				return errors.New("request.headers.sec-websocket-protocol")
			}
		case "sec-websocket-extensions":
			wantTokens, err := wsCommaValues([]string{value})
			if err != nil {
				return errors.New("request.headers.sec-websocket-extensions")
			}
			gotTokens, err := wsCommaValues(values)
			if err != nil || !wsEqualValues(wantTokens, gotTokens) {
				return errors.New("request.headers.sec-websocket-extensions")
			}
		default:
			if secretHeaders[name] {
				if len(values) != 1 || strings.TrimSpace(values[0]) == "" || strings.TrimSpace(values[0]) == "<redacted>" {
					return errors.New("request.headers")
				}
			} else if !wsEqualValues([]string{value}, values) {
				return errors.New("request.headers")
			}
		}
	}
	return nil
}

func wsHandshakePath(r Request) (string, error) {
	if r.Method != http.MethodGet {
		return "", errors.New("request.method")
	}
	if len(r.Body) != 0 {
		return "", errors.New("request.body")
	}
	u, err := url.Parse(r.Path)
	if err != nil || !strings.HasPrefix(r.Path, "/") || strings.HasPrefix(r.Path, "//") ||
		strings.ContainsAny(r.Path, "?#") || u.IsAbs() || u.Host != "" || u.User != nil || u.Opaque != "" {
		return "", errors.New("request.path")
	}
	return u.EscapedPath(), nil
}

func wsHandshakeQuery(raw string) (url.Values, error) {
	query, err := url.ParseQuery(raw)
	if err != nil {
		return nil, errors.New("request.query")
	}
	for key := range query {
		switch strings.ToLower(key) {
		case "token", "key", "access_token", "api_key":
			delete(query, key)
		}
	}
	return query, nil
}

func wsProtocolTokens(values []string) ([]string, error) {
	parts, err := wsCommaValues(values)
	if err != nil {
		return nil, errors.New("request.headers.sec-websocket-protocol")
	}
	tokens := make([]string, 0, len(parts))
	for _, token := range parts {
		if !wsHTTPToken(token) {
			return nil, errors.New("request.headers.sec-websocket-protocol")
		}
		if strings.HasPrefix(strings.ToLower(token), "openai-insecure-api-key.") && len(token) > len("openai-insecure-api-key.") {
			continue
		}
		switch token {
		case "realtime", "openai-beta.realtime-v1":
			tokens = append(tokens, token)
		default:
			return nil, errors.New("request.headers.sec-websocket-protocol")
		}
	}
	return tokens, nil
}

func wsRequiredHandshakeHeaders(headers map[string][]string) error {
	connection, err := wsCommaValues(headers["connection"])
	if err != nil || len(connection) == 0 {
		return errors.New("request.headers.connection")
	}
	upgrade := false
	for _, token := range connection {
		if !wsHTTPToken(token) {
			return errors.New("request.headers.connection")
		}
		upgrade = upgrade || strings.EqualFold(token, "upgrade")
	}
	if !upgrade {
		return errors.New("request.headers.connection")
	}
	values := headers["upgrade"]
	if len(values) != 1 || !strings.EqualFold(strings.TrimSpace(values[0]), "websocket") {
		return errors.New("request.headers.upgrade")
	}
	values = headers["sec-websocket-version"]
	if len(values) != 1 || strings.TrimSpace(values[0]) != "13" {
		return errors.New("request.headers.sec-websocket-version")
	}
	values = headers["sec-websocket-key"]
	if len(values) != 1 {
		return errors.New("request.headers.sec-websocket-key")
	}
	nonce, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(values[0]))
	if err != nil || len(nonce) != 16 {
		return errors.New("request.headers.sec-websocket-key")
	}
	return nil
}

// 保留列表的全部值与重复项；引号内逗号不能被误拆成另一个扩展协商项。
func wsCommaValues(values []string) ([]string, error) {
	var tokens []string
	for _, value := range values {
		start := 0
		quoted, escaped := false, false
		for i := 0; i <= len(value); i++ {
			if i == len(value) || (value[i] == ',' && !quoted) {
				token := strings.TrimSpace(value[start:i])
				if token == "" || quoted || escaped {
					return nil, errors.New("request.headers")
				}
				tokens = append(tokens, token)
				start = i + 1
				continue
			}
			if escaped {
				escaped = false
			} else if quoted && value[i] == '\\' {
				escaped = true
			} else if value[i] == '"' {
				quoted = !quoted
			}
		}
	}
	return tokens, nil
}

func wsEqualValues(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

func wsHTTPToken(token string) bool {
	if token == "" {
		return false
	}
	for i := 0; i < len(token); i++ {
		c := token[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c))) {
			return false
		}
	}
	return true
}

func wsHeaderValue(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] == 127 || value[i] < 32 && value[i] != '\t' {
			return false
		}
	}
	return true
}
