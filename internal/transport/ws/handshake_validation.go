package ws

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
)

var errUpgradeVersion = errors.New("不支持或重复的 WebSocket 版本")

// ValidateUpgrade 只预检、不读 body、不写响应也不接管连接，供调用方在触上游前拒绝坏请求。
// Origin 与扩展提议不意味着协商成功；Accept 不选择或回显任何扩展。
func ValidateUpgrade(r *http.Request) error {
	if r == nil || r.Method != http.MethodGet || r.ProtoMajor != 1 || r.ProtoMinor != 1 {
		return fmt.Errorf("%w: 升级要求 HTTP/1.1 GET", ErrHandshake)
	}
	if !headerContainsToken(r.Header, "Connection", "upgrade") || !headerContainsToken(r.Header, "Upgrade", "websocket") {
		return fmt.Errorf("%w: 缺少 WebSocket 升级 token", ErrHandshake)
	}
	if versions := r.Header.Values("Sec-WebSocket-Version"); len(versions) != 1 || versions[0] != "13" {
		return fmt.Errorf("%w: %w", ErrHandshake, errUpgradeVersion)
	}
	keys := r.Header.Values("Sec-WebSocket-Key")
	if len(keys) != 1 {
		return fmt.Errorf("%w: nonce 必须唯一", ErrHandshake)
	}
	nonce, err := base64.StdEncoding.Strict().DecodeString(keys[0])
	if err != nil || len(nonce) != 16 || base64.StdEncoding.EncodeToString(nonce) != keys[0] {
		return fmt.Errorf("%w: nonce 必须是合法的 16 字节 base64", ErrHandshake)
	}
	if r.ContentLength != 0 || (r.Body != nil && r.Body != http.NoBody) || len(r.TransferEncoding) != 0 || len(r.Header.Values("Transfer-Encoding")) != 0 {
		return fmt.Errorf("%w: 升级请求不能携带 body 或 transfer-encoding", ErrHandshake)
	}
	return nil
}

func validateUpgradeResponse(resp *http.Response, key string) error {
	if resp.ProtoMajor != 1 || resp.ProtoMinor != 1 || !headerContainsToken(resp.Header, "Connection", "upgrade") || !headerContainsToken(resp.Header, "Upgrade", "websocket") {
		return fmt.Errorf("%w: 101 缺少完整的 HTTP/1.1 升级协商", ErrHandshake)
	}
	values := resp.Header.Values("Sec-WebSocket-Accept")
	if len(values) != 1 || values[0] != acceptKey(key) {
		return fmt.Errorf("%w: Accept 摘要不唯一或不匹配", ErrHandshake)
	}
	if len(resp.Header.Values("Sec-WebSocket-Extensions")) != 0 || len(resp.Header.Values("Sec-WebSocket-Protocol")) != 0 {
		return fmt.Errorf("%w: 上游选择了未提议的扩展或子协议", ErrHandshake)
	}
	return nil
}

// 原 socket 必须先关闭再 Close body，避免 net/http 为复用连接而排空无限流。
// 诊断仅携带固定描述；响应原文只在有界内存 Body 中交给错误分类调用方。
func retainHandshakeErrorBody(ctx context.Context, conn net.Conn, resp *http.Response, limit int64) error {
	original := resp.Body
	body, readErr := io.ReadAll(io.LimitReader(original, limit))
	truncated := false
	if readErr == nil {
		var extra [1]byte
		n, err := original.Read(extra[:])
		truncated = n != 0
		if err != nil && !errors.Is(err, io.EOF) {
			readErr = err
		}
	}
	_ = conn.Close()
	_ = original.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	if truncated {
		return fmt.Errorf("%w: 上游未升级协议，状态码 %d，body 超限已截断", ErrHandshake, resp.StatusCode)
	}
	if readErr != nil {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%w: 读取失败响应被取消: %w", ErrHandshake, err)
		}
		return fmt.Errorf("%w: 上游未升级协议，状态码 %d，body 不完整", ErrHandshake, resp.StatusCode)
	}
	return fmt.Errorf("%w: 上游未升级协议，状态码 %d", ErrHandshake, resp.StatusCode)
}
