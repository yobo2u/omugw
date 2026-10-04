package config

import "fmt"

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
	// 消息上限先校验，乘二不会溢出；预算至少能容纳双向各一条在途消息。
	if w.MaxBufferedBytes < 2*w.MaxMessageBytes {
		return fmt.Errorf("config: websocket.max_buffered_bytes 不得小于 max_message_bytes 的两倍")
	}
	return nil
}
