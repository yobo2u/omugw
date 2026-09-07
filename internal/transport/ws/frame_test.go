package ws

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
)

// TestFrameRoundTripCoversAllLengthBranches 钉住 RFC 6455 的三种负载长度编码。
//
// 这三个分支是帧格式最容易写错的地方：边界差一位，接收方就会把长度字段
// 当成负载内容读，此后整条流全部错位——而错位的表现是「连接莫名其妙断开」，
// 从现象倒推回长度编码要花很久。
func TestFrameRoundTripCoversAllLengthBranches(t *testing.T) {
	for name, size := range map[string]int{
		"零长度":        0,
		"7 位上界 125":  125,
		"16 位下界 126": 126,
		"16 位上界":     65535,
		"64 位下界":     65536,
	} {
		t.Run(name, func(t *testing.T) {
			payload := make([]byte, size)
			for i := range payload {
				payload[i] = byte(i % 251)
			}

			for _, masked := range []bool{false, true} {
				var buf bytes.Buffer
				in := Frame{FIN: true, Opcode: OpBinary, Payload: payload}
				if err := WriteFrame(&buf, in, masked); err != nil {
					t.Fatalf("masked=%v 写入失败: %v", masked, err)
				}

				got, err := ReadFrame(&buf, maxTestPayload)
				if err != nil {
					t.Fatalf("masked=%v 读取失败: %v", masked, err)
				}
				if !got.FIN || got.Opcode != OpBinary {
					t.Errorf("masked=%v 头部不符: FIN=%v opcode=%v", masked, got.FIN, got.Opcode)
				}
				if !bytes.Equal(got.Payload, payload) {
					t.Errorf("masked=%v 负载往返不一致（长度 %d -> %d）",
						masked, len(payload), len(got.Payload))
				}
			}
		})
	}
}

// TestClientFrameIsMaskedOnTheWire 固化「客户端必须掩码」。
//
// 不是可选优化：RFC 6455 §5.1 要求客户端到服务端的每一帧都带掩码，
// 未掩码的帧服务端必须以 1002 关闭连接。真实上游确实会照此拒绝。
func TestClientFrameIsMaskedOnTheWire(t *testing.T) {
	payload := []byte("session.update")

	var masked bytes.Buffer
	if err := WriteFrame(&masked, Frame{FIN: true, Opcode: OpText, Payload: payload}, true); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	raw := masked.Bytes()

	if raw[1]&0x80 == 0 {
		t.Fatal("客户端帧必须置 MASK 位")
	}
	// 掩码后的负载不应与明文相同，否则等于没掩码。
	if bytes.Contains(raw, payload) {
		t.Error("掩码后的负载不应在线上以明文出现")
	}

	var plain bytes.Buffer
	if err := WriteFrame(&plain, Frame{FIN: true, Opcode: OpText, Payload: payload}, false); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if plain.Bytes()[1]&0x80 != 0 {
		t.Error("服务端帧不得置 MASK 位")
	}
}

// TestFrameWireFormatIsExact 用一份手算的字节序列钉住线格式。
//
// 往返测试证明不了「与别人互通」——自己的编码器和解码器可以一起错。
// 这里的期望值按 RFC 6455 §5.2 手工推导，任何一位漂移都会被抓住。
func TestFrameWireFormatIsExact(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, Frame{FIN: true, Opcode: OpText, Payload: []byte("hi")}, false); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	want := []byte{
		0x81, // FIN=1 RSV=000 opcode=0x1
		0x02, // MASK=0 len=2
		'h', 'i',
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("线格式 = % x，期望 % x", buf.Bytes(), want)
	}
}

// TestReadFrameRejectsReservedBits：RSV1-3 未协商扩展时必须为 0。
//
// 收到置位的 RSV 说明对端启用了我们没协商过的扩展（如 permessage-deflate），
// 此时按普通帧解析会得到一段压缩过的乱码。宁可报错，不要交出垃圾数据。
func TestReadFrameRejectsReservedBits(t *testing.T) {
	for name, first := range map[string]byte{
		"RSV1": 0xC1,
		"RSV2": 0xA1,
		"RSV3": 0x91,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ReadFrame(bytes.NewReader([]byte{first, 0x00}), maxTestPayload)
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("应当以 ErrProtocol 拒绝，实际: %v", err)
			}
		})
	}
}

// TestReadFrameRejectsOversizedControlFrame：控制帧负载上限 125 且不得分片
// （RFC 6455 §5.5）。放行会让一个畸形 close 帧拖着超大负载占满内存。
func TestReadFrameRejectsOversizedControlFrame(t *testing.T) {
	// opcode=0x9 (ping)，len=126 触发 16 位长度分支——控制帧不允许。
	frame := []byte{0x89, 126, 0x00, 0x7E}
	frame = append(frame, make([]byte, 126)...)

	if _, err := ReadFrame(bytes.NewReader(frame), maxTestPayload); !errors.Is(err, ErrProtocol) {
		t.Fatalf("超长控制帧应当被拒，实际: %v", err)
	}
}

// TestReadFrameRejectsFragmentedControlFrame：控制帧 FIN 必须为 1。
func TestReadFrameRejectsFragmentedControlFrame(t *testing.T) {
	// FIN=0 且 opcode=0x8 (close)
	if _, err := ReadFrame(bytes.NewReader([]byte{0x08, 0x00}), maxTestPayload); !errors.Is(err, ErrProtocol) {
		t.Fatalf("分片的控制帧应当被拒，实际: %v", err)
	}
}

// TestReadFrameEnforcesPayloadLimit 防的是内存耗尽。
//
// 长度字段是对端说了算的 64 位数。不设上限，一个声称负载 8 EiB 的帧
// 就能让网关当场 OOM——而它只需要发 10 个字节。
func TestReadFrameEnforcesPayloadLimit(t *testing.T) {
	var hdr []byte
	hdr = append(hdr, 0x82, 127) // FIN + binary，64 位长度分支
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], 1<<40) // 声称 1 TiB
	hdr = append(hdr, size[:]...)

	_, err := ReadFrame(bytes.NewReader(hdr), 1<<20)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("超限负载应当以 ErrTooLarge 拒绝，实际: %v", err)
	}
}

// TestReadFrameRejectsUnknownOpcode：未定义 opcode 一律拒绝。
func TestReadFrameRejectsUnknownOpcode(t *testing.T) {
	// 0x3 是保留的非控制 opcode。
	if _, err := ReadFrame(bytes.NewReader([]byte{0x83, 0x00}), maxTestPayload); !errors.Is(err, ErrProtocol) {
		t.Fatalf("未知 opcode 应当被拒，实际: %v", err)
	}
}

// TestReadFrameOnTruncatedInputReportsUnexpectedEOF。
//
// 半个帧要报「读到一半断了」，不能当成一个空帧交出去——后者会让上层
// 以为对端发了个合法的零长度帧。
func TestReadFrameOnTruncatedInputReportsUnexpectedEOF(t *testing.T) {
	// 声称 8 字节负载，实际只给 3 字节。
	truncated := []byte{0x82, 0x08, 1, 2, 3}

	_, err := ReadFrame(bytes.NewReader(truncated), maxTestPayload)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("截断输入应当报 ErrUnexpectedEOF，实际: %v", err)
	}
}

// TestReadFrameAtCleanEOFReportsEOF：连接干净关闭时要能与「帧读一半断了」区分。
func TestReadFrameAtCleanEOFReportsEOF(t *testing.T) {
	if _, err := ReadFrame(bytes.NewReader(nil), maxTestPayload); !errors.Is(err, io.EOF) {
		t.Fatalf("空输入应当报 EOF，实际: %v", err)
	}
}

// TestFragmentedMessageReassembles 覆盖分片续帧。
//
// 大音频负载会被对端拆成多帧发送。首帧带真实 opcode 且 FIN=0，
// 续帧 opcode=0 且最后一帧 FIN=1。把续帧的 opcode 0 当成独立消息，
// 音频就会碎成一堆无法解码的片段。
func TestFragmentedMessageReassembles(t *testing.T) {
	var buf bytes.Buffer
	writeAll(t, &buf,
		Frame{FIN: false, Opcode: OpText, Payload: []byte("abc")},
		Frame{FIN: false, Opcode: OpContinuation, Payload: []byte("def")},
		Frame{FIN: true, Opcode: OpContinuation, Payload: []byte("ghi")},
	)

	op, payload, err := NewReader(&buf, maxTestPayload).ReadMessage()
	if err != nil {
		t.Fatalf("读取分片消息失败: %v", err)
	}
	if op != OpText {
		t.Errorf("opcode = %v，期望首帧的 OpText", op)
	}
	if string(payload) != "abcdefghi" {
		t.Errorf("重组结果 = %q，期望 %q", payload, "abcdefghi")
	}
}

// TestControlFrameInterleavedInFragmentsIsReturnedFirst。
//
// RFC 6455 §5.4 允许控制帧插在分片之间。ping 必须被立刻看见并回 pong，
// 否则对端会认为链路已死而断开——而此时数据分片其实还在正常传输。
func TestControlFrameInterleavedInFragments(t *testing.T) {
	var buf bytes.Buffer
	writeAll(t, &buf,
		Frame{FIN: false, Opcode: OpText, Payload: []byte("ab")},
		Frame{FIN: true, Opcode: OpPing, Payload: []byte("hb")},
		Frame{FIN: true, Opcode: OpContinuation, Payload: []byte("cd")},
	)

	// 分片状态必须跨调用保留，因此复用同一个 Reader。
	rd := NewReader(&buf, maxTestPayload)

	op, payload, err := rd.ReadMessage()
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if op != OpPing || string(payload) != "hb" {
		t.Fatalf("插入的控制帧应当优先返回，实际 opcode=%v payload=%q", op, payload)
	}

	op, payload, err = rd.ReadMessage()
	if err != nil {
		t.Fatalf("续读失败: %v", err)
	}
	if op != OpText || string(payload) != "abcd" {
		t.Fatalf("控制帧不应打断分片重组，实际 opcode=%v payload=%q", op, payload)
	}
}

// TestReadMessageRejectsContinuationWithoutStart：孤立的续帧是协议错误。
func TestReadMessageRejectsContinuationWithoutStart(t *testing.T) {
	var buf bytes.Buffer
	writeAll(t, &buf, Frame{FIN: true, Opcode: OpContinuation, Payload: []byte("x")})

	if _, _, err := NewReader(&buf, maxTestPayload).ReadMessage(); !errors.Is(err, ErrProtocol) {
		t.Fatalf("无起始帧的续帧应当被拒，实际: %v", err)
	}
}

// TestReadMessageEnforcesLimitAcrossFragments 防的是「分片绕过上限」。
//
// 逐帧检查上限时，对端可以发一万个各 1 MiB 的分片，每帧都合法，
// 重组出来却是 10 GiB。上限必须按重组后的总量算。
func TestReadMessageEnforcesLimitAcrossFragments(t *testing.T) {
	var buf bytes.Buffer
	chunk := make([]byte, 600)
	writeAll(t, &buf,
		Frame{FIN: false, Opcode: OpBinary, Payload: chunk},
		Frame{FIN: true, Opcode: OpContinuation, Payload: chunk},
	)

	if _, _, err := NewReader(&buf, 1000).ReadMessage(); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("分片总量超限应当被拒，实际: %v", err)
	}
}

// TestCloseCodeRoundTrip：close 帧前两字节是大端状态码，其余是 UTF-8 原因。
//
// 实测中 DashScope 用 1011 + 文本原因拒绝过载请求，网关要能把它读出来
// 并映射成可重试的上游错误，而不是当成一次无缘无故的断连。
func TestCloseCodeRoundTrip(t *testing.T) {
	payload := EncodeClosePayload(1011, "throttled")

	code, reason, err := DecodeClosePayload(payload)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if code != 1011 || reason != "throttled" {
		t.Errorf("解析结果 = (%d, %q)，期望 (1011, %q)", code, reason, "throttled")
	}
}

// TestDecodeClosePayloadHandlesEmpty：空 close 负载是合法的，按 1005 处理。
func TestDecodeClosePayloadHandlesEmpty(t *testing.T) {
	code, reason, err := DecodeClosePayload(nil)
	if err != nil {
		t.Fatalf("空负载应当合法: %v", err)
	}
	if code != CloseNoStatus {
		t.Errorf("code = %d，期望 %d（无状态码）", code, CloseNoStatus)
	}
	if reason != "" {
		t.Errorf("reason = %q，期望空", reason)
	}
}

// TestEncodeClosePayloadTruncatesOverlongReason：close 帧是控制帧，
// 总负载不得超过 125 字节，否则对端会以协议错误断开。
func TestEncodeClosePayloadTruncatesOverlongReason(t *testing.T) {
	payload := EncodeClosePayload(CloseNormal, strings.Repeat("x", 500))

	if len(payload) > 125 {
		t.Fatalf("close 负载 %d 字节，超过控制帧上限 125", len(payload))
	}
}

const maxTestPayload = 1 << 20

func writeAll(t *testing.T, w io.Writer, frames ...Frame) {
	t.Helper()
	for _, f := range frames {
		if err := WriteFrame(w, f, false); err != nil {
			t.Fatalf("写入帧失败: %v", err)
		}
	}
}
