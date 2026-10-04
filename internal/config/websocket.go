package config

import (
	"fmt"

	"github.com/yobo2u/omugw/internal/transport/ws"
)

// WebSocket 把单消息、在途会话和全进程 payload 分开限额，防止长连接累计占满内存。
type WebSocket struct {
	MaxMessageBytes  int64 `yaml:"max_message_bytes"`
	MaxSessions      int   `yaml:"max_sessions"`
	MaxBufferedBytes int64 `yaml:"max_buffered_bytes"`
}

func DefaultWebSocket() WebSocket {
	return WebSocket{MaxMessageBytes: 32 << 20, MaxSessions: 128, MaxBufferedBytes: 256 << 20}
}

func (w WebSocket) Validate() error {
	if w.MaxMessageBytes <= 0 || w.MaxMessageBytes > 64<<20 {
		return fmt.Errorf("config: websocket.max_message_bytes 必须为正数且不大于 64 MiB")
	}
	if w.MaxSessions <= 0 || w.MaxSessions > 4096 {
		return fmt.Errorf("config: websocket.max_sessions 必须为正数且不大于 4096")
	}
	if w.MaxBufferedBytes <= 0 || w.MaxBufferedBytes > 2<<30 {
		return fmt.Errorf("config: websocket.max_buffered_bytes 必须为正数且不大于 2 GiB")
	}
	// 上限先校验，乘加不会溢出；双向各持一条完整消息时仍须容纳出站掩码。
	// 聚合会话、分片扩容峰值及控制帧另行计额，此下界不保证任意并发均满额。
	if w.MaxBufferedBytes < 2*w.MaxMessageBytes+ws.MaskWorkspaceBytes(w.MaxMessageBytes) {
		return fmt.Errorf("config: websocket.max_buffered_bytes 不得小于两倍 max_message_bytes 加掩码工作区")
	}
	return nil
}
