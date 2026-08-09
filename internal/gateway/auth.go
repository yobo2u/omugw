package gateway

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/config"
)

// Caller 是一个已通过鉴权的调用方。
type Caller struct {
	// ID 出现在日志与指标里。密钥本身绝不出现在任何地方。
	ID string
}

// Authenticator 校验网关自身的 API Key。
type Authenticator struct {
	keys []config.AuthKey
}

// NewAuthenticator 构造鉴权器。
func NewAuthenticator(keys []config.AuthKey) *Authenticator {
	return &Authenticator{keys: keys}
}

// Authenticate 校验请求头中的凭据。
//
// 逐个做常数时间比较，且**不提前返回**：一旦按长度或前缀短路，攻击者就能
// 通过响应时间的差异一个字节一个字节地把密钥试出来。多比几次的开销可以忽略，
// 而时间侧信道一旦漏出去就补不回来。
func (a *Authenticator) Authenticate(r *http.Request) (Caller, error) {
	presented := bearerToken(r)
	if presented == "" {
		return Caller{}, canonical.Newf(canonical.ClassAuth,
			"缺少 Authorization: Bearer <key>")
	}

	var matched Caller
	found := false
	for _, k := range a.keys {
		if subtle.ConstantTimeCompare([]byte(presented), []byte(k.Key)) == 1 {
			matched = Caller{ID: k.ID}
			found = true
		}
	}
	if !found {
		// 错误消息不透露任何关于正确密钥的信息——长度、前缀、是否存在同名 ID
		// 都不说。任何一点差异都是可枚举的信号。
		return Caller{}, canonical.Newf(canonical.ClassAuth, "无效的 API Key")
	}
	return matched, nil
}

// bearerToken 从请求中取出凭据。
//
// 同时接受 Authorization 与 api-key 两种头：前者是 OpenAI 与 Anthropic 的写法，
// 后者是 Azure 系客户端的习惯。多认一种头不会削弱安全性，却能省掉一大堆
// 「为什么我的客户端连不上」。
func bearerToken(r *http.Request) string {
	if v := r.Header.Get("Authorization"); v != "" {
		if after, ok := strings.CutPrefix(v, "Bearer "); ok {
			return strings.TrimSpace(after)
		}
		return strings.TrimSpace(v)
	}
	if v := r.Header.Get("Api-Key"); v != "" {
		return strings.TrimSpace(v)
	}
	return ""
}
