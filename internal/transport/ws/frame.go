// Package ws 是网关的 WebSocket 传输层（RFC 6455）。
//
// 自己实现而不引第三方库，是为了守住「仅三个直接依赖」这条 Clean Core 的线。
// 帧编解码本身是纯映射，适合用穷举表钉死；真正的复杂度在连接生命周期，
// 那部分无论用不用库都要自己处理。
//
// 分成 frame（纯函数）与 conn（有状态）两层：前者与网络无关，能离线穷举；
// 混在一起就只能靠起真实连接来测，慢且不稳。
package ws

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

// Opcode 是帧类型（RFC 6455 §5.2）。
type Opcode byte

const (
	OpContinuation Opcode = 0x0
	OpText         Opcode = 0x1
	OpBinary       Opcode = 0x2
	OpClose        Opcode = 0x8
	OpPing         Opcode = 0x9
	OpPong         Opcode = 0xA
)

func (o Opcode) String() string {
	switch o {
	case OpContinuation:
		return "continuation"
	case OpText:
		return "text"
	case OpBinary:
		return "binary"
	case OpClose:
		return "close"
	case OpPing:
		return "ping"
	case OpPong:
		return "pong"
	default:
		return "opcode(0x" + hex.EncodeToString([]byte{byte(o)}) + ")"
	}
}

// isControl 报告是否为控制帧。控制帧受两条额外约束：负载 ≤ 125 且不得分片。
func (o Opcode) isControl() bool { return o&0x8 != 0 }

// 关闭状态码（RFC 6455 §7.4.1）。只登记网关会主动用到的几个。
const (
	CloseNormal        = 1000
	CloseGoingAway     = 1001
	CloseProtocolError = 1002
	CloseTooLarge      = 1009
	CloseInternalError = 1011

	// CloseNoStatus 是「对端关了但没给状态码」。它只能由本地推断得出，
	// 绝不能编码进线上——RFC 明确禁止发送 1005。
	CloseNoStatus = 1005
)

// maxControlPayload 是控制帧负载上限（RFC 6455 §5.5）。
const maxControlPayload = 125

var (
	// ErrProtocol 表示对端违反了 RFC 6455。
	//
	// 与 ErrTooLarge 分开：协议错误说明对端实现有问题或链路被篡改，
	// 超限只是这一帧太大。前者该断连，后者可以只拒这条消息。
	ErrProtocol = errors.New("ws: 协议错误")

	// ErrTooLarge 表示负载超过本地上限。
	ErrTooLarge = errors.New("ws: 负载超过上限")
)

// Frame 是一个 WebSocket 帧。
type Frame struct {
	FIN     bool
	Opcode  Opcode
	Payload []byte
}

// WriteFrame 写一个帧。masked 为 true 时按 RFC 6455 §5.3 掩码负载。
//
// 掩码不是可选优化：客户端到服务端的每一帧都必须掩码，否则服务端会以
// 1002 断开。方向由调用方决定——网关对上游是客户端（要掩码），
// 对下游是服务端（不得掩码）。
func WriteFrame(w io.Writer, f Frame, masked bool) error {
	if f.Opcode.isControl() {
		if len(f.Payload) > maxControlPayload {
			return fmt.Errorf("%w: 控制帧负载 %d 字节超过上限 %d",
				ErrProtocol, len(f.Payload), maxControlPayload)
		}
		if !f.FIN {
			return fmt.Errorf("%w: 控制帧不得分片", ErrProtocol)
		}
	}

	var hdr []byte

	first := byte(f.Opcode)
	if f.FIN {
		first |= 0x80
	}
	hdr = append(hdr, first)

	n := len(f.Payload)
	var maskBit byte
	if masked {
		maskBit = 0x80
	}
	switch {
	case n < 126:
		hdr = append(hdr, maskBit|byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, maskBit|126)
		var b [2]byte
		binary.BigEndian.PutUint16(b[:], uint16(n))
		hdr = append(hdr, b[:]...)
	default:
		hdr = append(hdr, maskBit|127)
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(n))
		hdr = append(hdr, b[:]...)
	}

	body := f.Payload
	if masked {
		var key [4]byte
		if _, err := rand.Read(key[:]); err != nil {
			return fmt.Errorf("ws: 生成掩码失败: %w", err)
		}
		hdr = append(hdr, key[:]...)

		// 复制后异或，不原地改调用方的切片——上层可能还要复用那段负载。
		body = make([]byte, n)
		for i := 0; i < n; i++ {
			body[i] = f.Payload[i] ^ key[i%4]
		}
	}

	if _, err := w.Write(hdr); err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	_, err := w.Write(body)
	return err
}

// ReadFrame 读一个帧，返回已解掩码的负载。
//
// limit 是负载上限。长度字段是对端说了算的 64 位数，不设上限的话，
// 一个声称负载 8 EiB 的帧只需十个字节就能让网关 OOM。
func ReadFrame(r io.Reader, limit int64) (Frame, error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		// 干净 EOF 与「帧读到一半断了」必须区分：前者是对端正常关闭，
		// 后者说明链路异常。io.ReadFull 已经替我们分好了。
		return Frame{}, err
	}

	fin := h[0]&0x80 != 0
	if h[0]&0x70 != 0 {
		// RSV1-3 置位说明对端启用了未协商的扩展（如 permessage-deflate）。
		// 按普通帧解析会交出一段压缩后的乱码，宁可报错。
		return Frame{}, fmt.Errorf("%w: RSV 位置位但未协商扩展", ErrProtocol)
	}

	opcode := Opcode(h[0] & 0x0F)
	switch opcode {
	case OpContinuation, OpText, OpBinary, OpClose, OpPing, OpPong:
	default:
		return Frame{}, fmt.Errorf("%w: 未定义的 opcode 0x%x", ErrProtocol, byte(opcode))
	}

	masked := h[1]&0x80 != 0
	n := int64(h[1] & 0x7F)

	if opcode.isControl() {
		if n > maxControlPayload {
			return Frame{}, fmt.Errorf("%w: 控制帧负载 %d 字节超过上限 %d",
				ErrProtocol, n, maxControlPayload)
		}
		if !fin {
			return Frame{}, fmt.Errorf("%w: 控制帧不得分片", ErrProtocol)
		}
	}

	switch n {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return Frame{}, unexpectedEOF(err)
		}
		n = int64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return Frame{}, unexpectedEOF(err)
		}
		v := binary.BigEndian.Uint64(b[:])
		if v > 1<<62 {
			// 最高位置位在 RFC 里就是非法的，同时也防 int64 溢出。
			return Frame{}, fmt.Errorf("%w: 负载长度最高位置位", ErrProtocol)
		}
		n = int64(v)
	}

	// 先判上限再分配。反过来写就等于让对端决定我们分配多少内存。
	if n > limit {
		return Frame{}, fmt.Errorf("%w: 负载 %d 字节超过上限 %d", ErrTooLarge, n, limit)
	}

	var key [4]byte
	if masked {
		if _, err := io.ReadFull(r, key[:]); err != nil {
			return Frame{}, unexpectedEOF(err)
		}
	}

	payload := make([]byte, n)
	if n > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return Frame{}, unexpectedEOF(err)
		}
	}
	if masked {
		for i := range payload {
			payload[i] ^= key[i%4]
		}
	}

	return Frame{FIN: fin, Opcode: opcode, Payload: payload}, nil
}

// unexpectedEOF 把「帧读到一半断了」统一成 io.ErrUnexpectedEOF。
//
// 头两字节之后的任何 EOF 都意味着帧不完整。原样抛 io.EOF 会让上层
// 误以为对端干净关闭，从而漏掉一次真实的链路异常。
func unexpectedEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// Reader 按消息读取，自动重组分片。
//
// 之所以是有状态类型而不是自由函数：控制帧可以插在分片之间（RFC 6455 §5.4），
// 返回那个 ping 之后，未读完的分片必须留着等下次调用继续拼。自由函数每次
// 从零开始，那半条消息就丢了——而丢的往往是一段音频，客户端听到的是断续。
type Reader struct {
	r     io.Reader
	limit int64

	// 以下三项是跨调用存活的分片状态。
	partial   []byte
	msgOp     Opcode
	fragments bool
}

// NewReader 构造消息读取器。limit 是单条消息重组后的负载上限。
func NewReader(r io.Reader, limit int64) *Reader {
	return &Reader{r: r, limit: limit}
}

// ReadMessage 读一条完整消息。
//
// 控制帧一经收到就立即返回，不打断正在进行的分片重组——ping 必须马上看见
// 并回 pong，否则对端会认为链路已死而断开，而那时数据分片其实还在正常传输。
//
// limit 按**重组后的总量**计算。逐帧检查会被绕过：一万个各 1 MiB 的合法
// 分片，每帧都过关，重组出来是 10 GiB。
func (rd *Reader) ReadMessage() (Opcode, []byte, error) {
	for {
		f, err := ReadFrame(rd.r, rd.limit)
		if err != nil {
			return 0, nil, err
		}

		if f.Opcode.isControl() {
			return f.Opcode, f.Payload, nil
		}

		if f.Opcode == OpContinuation {
			if !rd.fragments {
				return 0, nil, fmt.Errorf("%w: 收到续帧但没有起始帧", ErrProtocol)
			}
		} else {
			if rd.fragments {
				return 0, nil, fmt.Errorf("%w: 分片未结束时收到新的起始帧", ErrProtocol)
			}
			rd.msgOp = f.Opcode
			rd.fragments = true
		}

		if int64(len(rd.partial))+int64(len(f.Payload)) > rd.limit {
			rd.reset()
			return 0, nil, fmt.Errorf("%w: 分片重组后超过上限 %d", ErrTooLarge, rd.limit)
		}
		rd.partial = append(rd.partial, f.Payload...)

		if f.FIN {
			op, payload := rd.msgOp, rd.partial
			rd.reset()
			return op, payload, nil
		}
	}
}

// reset 清空分片状态。
//
// 单独抽出来是因为它有两个调用点（正常收尾与超限中止），而漏掉任何一个
// 都会让下一条消息带上前一条的残留字节——那种脏数据极难从现象倒推。
func (rd *Reader) reset() {
	rd.partial = nil
	rd.msgOp = 0
	rd.fragments = false
}

// EncodeClosePayload 编码 close 帧负载：大端状态码 + UTF-8 原因。
//
// 原因会被截断到控制帧上限内。超长的 close 负载会让对端以协议错误断开——
// 那会把一次本可以说清楚的正常关闭，变成一次谁也看不懂的异常断连。
func EncodeClosePayload(code uint16, reason string) []byte {
	out := make([]byte, 2, 2+len(reason))
	binary.BigEndian.PutUint16(out, code)

	// 留 2 字节给状态码。按 rune 边界截断，避免切出半个 UTF-8 字符——
	// 那会让对端的 UTF-8 校验失败，同样引发协议错误。
	limit := maxControlPayload - 2
	if len(reason) > limit {
		for limit > 0 && !utf8.RuneStart(reason[limit]) {
			limit--
		}
		reason = reason[:limit]
	}
	return append(out, reason...)
}

// DecodeClosePayload 解析 close 帧负载。
//
// 空负载是合法的，表示对端没给状态码，按 1005 报告。实测中 DashScope
// 用 1011 + 文本原因表达过载（"To many requests..."），网关要能读出来
// 并映射成可重试的上游错误，而不是当成一次无缘无故的断连。
func DecodeClosePayload(payload []byte) (uint16, string, error) {
	if len(payload) == 0 {
		return CloseNoStatus, "", nil
	}
	if len(payload) == 1 {
		return 0, "", fmt.Errorf("%w: close 负载只有 1 字节，状态码不完整", ErrProtocol)
	}

	code := binary.BigEndian.Uint16(payload)
	reason := string(payload[2:])
	if !utf8.ValidString(reason) {
		return 0, "", fmt.Errorf("%w: close 原因不是合法 UTF-8", ErrProtocol)
	}
	return code, reason, nil
}
