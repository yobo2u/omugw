package ws

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
)

// 大首片之后的小续片不得逐片复制整条消息；扩容仍必须计算新旧数组并存峰值。
func TestProductionLargeFirstFragmentGrowth(t *testing.T) {
	const limit = 2 << 20
	const first = (1 << 20) + 1
	for _, peak := range []int64{first + limit - 1, first + limit} {
		budget := productionBudget(t, peak)
		b := payloadBuffer{budget: budget}
		if err := b.grow(first, limit); err != nil {
			t.Fatal(err)
		}
		copy(b.bytes, bytes.Repeat([]byte{'a'}, first))
		err := b.grow(first+1, limit)
		if peak < first+limit {
			if !errors.Is(err, ErrBufferLimit) || budget.Used() != first || len(b.bytes) != first {
				t.Error("未在复制前拒绝并存峰值，或失败改动了原缓冲")
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			b.bytes[first] = 'b'
			start := &b.bytes[0]
			for i := 1; i < 32; i++ {
				if err := b.grow(first+i+1, limit); err != nil {
					t.Fatal(err)
				}
				b.bytes[first+i] = 'b'
				if &b.bytes[0] != start {
					t.Errorf("单字节续片 %d 再次复制了整个大消息", i+1)
					break
				}
			}
			if budget.Used() != int64(cap(b.bytes)) || cap(b.bytes) > limit {
				t.Error("容量未完整计额或越过单条上限")
			}
			if !bytes.Equal(b.bytes[:first], bytes.Repeat([]byte{'a'}, first)) ||
				!bytes.Equal(b.bytes[first:], bytes.Repeat([]byte{'b'}, len(b.bytes)-first)) {
				t.Error("扩容改变了已有字节")
			}
		}
		b.release()
		if budget.Used() != 0 {
			t.Fatal("归还后仍占用容量")
		}
	}
}

func TestProductionLargeFirstFragmentReassembly(t *testing.T) {
	const limit = 2 << 20
	first := bytes.Repeat([]byte{'a'}, (1<<20)+1)
	var wire bytes.Buffer
	if err := WriteFrame(&wire, Frame{Opcode: OpBinary, Payload: first}, false); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 32; i++ {
		if err := WriteFrame(&wire, Frame{FIN: i == 31, Opcode: OpContinuation, Payload: []byte{'b'}}, false); err != nil {
			t.Fatal(err)
		}
	}
	a, _ := tcpPair(t)
	c := newConnBuffered(a, &wire, RoleClient, limit, 0)
	c.budget = productionBudget(t, int64(len(first)+limit))
	m, err := c.ReadOwnedMessage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := append(first, bytes.Repeat([]byte{'b'}, 32)...)
	if m.Opcode != OpBinary || !bytes.Equal(m.Payload, want) || c.budget.Used() != int64(cap(m.Payload)) || cap(m.Payload) > limit {
		t.Error("重组没有保全消息字节与容量所有权")
	}
	m.Release()
	if c.budget.Used() != 0 {
		t.Fatal("Message.Release 未归还扩容后的容量")
	}
}

// 两个消息入口必须给 relay 同一分类，不能靠错误文本猜测 1007。
func TestProductionUTF8ErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		name string
		wire []byte
		want error
	}{
		{"complete-text", []byte{0x81, 1, 0xff}, ErrInvalidUTF8},
		{"fragmented-text", []byte{0x01, 1, 0xe4, 0x80, 1, 'a'}, ErrInvalidUTF8},
		{"truncated-rune", []byte{0x01, 1, 0xe4, 0x80, 1, 0xb8}, ErrInvalidUTF8},
		{"close-reason", []byte{0x88, 3, 3, 232, 0xff}, ErrInvalidUTF8},
		{"rfc-opcode", []byte{0x83, 0}, ErrProtocol},
		{"close-code", []byte{0x88, 2, 3, 238}, ErrProtocol},
		{"close-short", []byte{0x88, 1, 3}, ErrProtocol},
		{"valid-split-rune", []byte{0x01, 1, 0xe4, 0x80, 2, 0xb8, 0xad}, nil},
		{"binary-is-not-text", []byte{0x82, 1, 0xff}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check := func(err error) {
				t.Helper()
				if tc.want == nil {
					if err != nil {
						t.Errorf("合法消息被拒绝: %v", err)
					}
					return
				}
				if !errors.Is(err, ErrProtocol) || errors.Is(err, ErrInvalidUTF8) != (tc.want == ErrInvalidUTF8) {
					t.Errorf("协议/UTF-8 分类不符: %v", err)
				}
			}
			_, _, err := NewReader(bytes.NewReader(tc.wire), 1024).ReadMessage()
			check(err)
			a, _ := tcpPair(t)
			c := newConnBuffered(a, bytes.NewReader(tc.wire), RoleClient, 1024, 0)
			c.budget = productionBudget(t, 64)
			m, err := c.ReadOwnedMessage(context.Background())
			check(err)
			m.Release()
			releaseCloseError(err)
			if c.budget.Used() != 0 {
				t.Error("分类错误路径遗留容量")
			}
		})
	}
}

func TestProductionOutgoingUTF8Classification(t *testing.T) {
	check := func(err error) {
		t.Helper()
		if !errors.Is(err, ErrInvalidUTF8) || !errors.Is(err, ErrProtocol) {
			t.Errorf("出站 UTF-8 丢失独立分类: %v", err)
		}
	}
	_, _, err := DecodeClosePayload([]byte{3, 232, 0xff})
	check(err)
	a, peer := tcpPair(t)
	c := NewConn(a, RoleServer, 1024, 0)
	check(c.WriteMessage(OpText, []byte{0xff}))
	check(c.WriteMessage(OpClose, []byte{3, 232, 0xff}))
	sent, err := c.CloseWithResult(1000, string([]byte{0xff}))
	check(err)
	if sent {
		t.Error("非法原因被报告为已发送")
	}
	if _, err := ReadFrame(peer, 125); !errors.Is(err, io.EOF) {
		t.Error("出站校验失败留下了非法帧或未关闭 socket")
	}
}

func TestProductionNonMinimalFrameLengths(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header []byte
	}{
		{"16-bit-one-byte", []byte{0x82, 126, 0, 1}},
		{"16-bit-125", []byte{0x82, 126, 0, 125}},
		{"64-bit-one-byte", []byte{0x82, 127, 0, 0, 0, 0, 0, 0, 0, 1}},
		{"64-bit-65535", []byte{0x82, 127, 0, 0, 0, 0, 0, 0, 255, 255}},
		{"64-bit-high-bit", []byte{0x82, 127, 128, 0, 0, 0, 0, 0, 0, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := append(tc.header, 'x')
			r := bytes.NewReader(wire)
			if _, err := ReadFrame(r, 1<<20); !errors.Is(err, ErrProtocol) || r.Len() != 1 {
				t.Errorf("非法头未在读取 payload 前拒绝: err=%v 剩余字节=%d", err, r.Len())
			}
			a, _ := tcpPair(t)
			r = bytes.NewReader(wire)
			c := newConnBuffered(a, r, RoleClient, 1<<20, 0)
			c.budget = productionBudget(t, 1)
			m, err := c.ReadOwnedMessage(context.Background())
			m.Release()
			if !errors.Is(err, ErrProtocol) || r.Len() != 1 || c.budget.Used() != 0 {
				t.Errorf("生产入口在头验证前读取/分配 payload 或分类不符: %v", err)
			}
		})
	}
}

func TestProductionMinimalFrameLengthBoundaries(t *testing.T) {
	for _, tc := range []struct {
		size   int
		header []byte
	}{
		{125, []byte{0x82, 125}},
		{126, []byte{0x82, 126, 0, 126}},
		{65535, []byte{0x82, 126, 255, 255}},
		{65536, []byte{0x82, 127, 0, 0, 0, 0, 0, 1, 0, 0}},
	} {
		t.Run(fmt.Sprint(tc.size), func(t *testing.T) {
			payload := bytes.Repeat([]byte{'x'}, tc.size)
			wire := append(tc.header, payload...)
			f, err := ReadFrame(bytes.NewReader(wire), int64(tc.size))
			if err != nil || f.Opcode != OpBinary || !bytes.Equal(f.Payload, payload) {
				t.Fatalf("合法最小编码边界被拒绝或负载改变: %v", err)
			}
			a, _ := tcpPair(t)
			c := newConnBuffered(a, bytes.NewReader(wire), RoleClient, int64(tc.size), 0)
			c.budget = productionBudget(t, int64(tc.size))
			m, err := c.ReadOwnedMessage(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if m.Opcode != OpBinary || !bytes.Equal(m.Payload, payload) || c.budget.Used() != int64(tc.size) {
				t.Error("生产入口的边界字节或预算不符")
			}
			m.Release()
			if c.budget.Used() != 0 {
				t.Error("边界消息未归还预算")
			}
		})
	}
}
