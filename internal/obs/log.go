// Package obs 提供日志、指标与审计基座。
package obs

import (
	"io"
	"log/slog"
	"strings"

	"github.com/yobo2u/omugw/internal/config"
)

// NewLogger 按配置构造 slog.Logger。
func NewLogger(cfg config.Log, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{
		Level:       parseLevel(cfg.Level),
		ReplaceAttr: redactAttr,
	}

	var h slog.Handler
	if cfg.Format == "text" {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}
	return slog.New(h)
}

func parseLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// 会被打码的日志字段名。
//
// 网关天生要经手凭据和用户内容，一次 debug 级别的日志就可能把 API Key
// 写进日志系统。宁可多列不可漏列。
var sensitiveKeys = map[string]bool{
	"authorization":  true,
	"api_key":        true,
	"apikey":         true,
	"x_api_key":      true,
	"token":          true,
	"access_token":   true,
	"refresh_token":  true,
	"secret":         true,
	"password":       true,
	"credential":     true,
	"cookie":         true,
	"prompt":         true,
	"messages":       true,
	"input":          true,
	"content":        true,
	"thinking":       true,
	"signature":      true,
	"audio":          true,
	"tool_arguments": true,
}

// redactAttr 对敏感字段打码。
//
// 除了凭据，用户内容（prompt / messages / thinking）也在名单里：这些是最终
// 用户的数据，默认不该进日志。确实需要排查内容问题时，应当走独立的、
// 有访问控制的审计通道，而不是把它们混进普通日志。
func redactAttr(_ []string, a slog.Attr) slog.Attr {
	if sensitiveKeys[strings.ToLower(a.Key)] {
		return slog.String(a.Key, "<redacted>")
	}
	return a
}
