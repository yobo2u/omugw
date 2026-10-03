package ws

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"unicode/utf8"
)

// ReadOwnedMessage 只允许一个在途读者；可与写和 Close 并发。预算为 nil 时仍可
// 使用此接口，但生产 relay 必须配置共享 Budget，并在 payload 使用完后 Release。
// 取消只拥有本次读取的关闭权；交付消息前必须撤销并 join 取消回调。
func (c *Conn) ReadOwnedMessage(ctx context.Context) (*Message, error) {
	if !c.readMu.TryLock() {
		return nil, errors.New("ws: 不允许并发读取")
	}
	defer c.readMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var claimed atomic.Bool
	done := make(chan struct{})
	// AfterFunc 在成功的每次读上不启动工作者；只有取消才调度关闭回调。
	stop := context.AfterFunc(ctx, func() {
		defer close(done)
		if claimed.CompareAndSwap(false, true) {
			c.closed.Store(true)
			_ = c.conn.Close()
		}
	})
	defer func() {
		if !stop() {
			<-done
		}
	}()

	m, err := c.readOwned(ctx)
	if !claimed.CompareAndSwap(false, true) {
		m.Release()
		return nil, ctx.Err()
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
	}
	return m, err
}

// 自动 pong 同样可能写超时，只有底层读取的超时才是接收空闲。
func readError(err error) error {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return ErrIdleTimeout
	}
	return err
}

// payloadBuffer 不单独复制每个帧；先验证头再直接填入重组区。扩容时先预占整个
// 新数组，复制并放弃旧引用后才归还旧容量，防止峰值躲进 append 的隐式分配。
type payloadBuffer struct {
	bytes  []byte
	budget *BufferBudget
}

func (b *payloadBuffer) grow(size, limit int) error {
	if size <= cap(b.bytes) {
		b.bytes = b.bytes[:size]
		return nil
	}
	capacity := size
	if cap(b.bytes) <= limit/2 {
		capacity = max(size, 2*cap(b.bytes))
	}
	if err := b.budget.acquire(int64(capacity)); err != nil {
		return err
	}
	next := make([]byte, size, capacity)
	copy(next, b.bytes)
	oldCapacity := cap(b.bytes)
	b.bytes = next
	b.budget.release(int64(oldCapacity))
	return nil
}

func (b *payloadBuffer) release() {
	capacity := cap(b.bytes)
	b.bytes = nil
	b.budget.release(int64(capacity))
}

func (c *Conn) readOwned(ctx context.Context) (*Message, error) {
	partial := payloadBuffer{budget: c.budget}
	defer partial.release()
	var opcode Opcode
	fragments := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		h, err := readFrameHeader(c.reader.r)
		if err != nil {
			return nil, readError(err)
		}
		if h.masked != (c.role == RoleServer) {
			return nil, fmt.Errorf("%w: 帧掩码方向非法", ErrProtocol)
		}
		if h.opcode.isControl() {
			control := payloadBuffer{budget: c.budget}
			if err := control.grow(int(h.size), maxControlPayload); err != nil {
				return nil, err
			}
			err := readError(readFramePayload(c.reader.r, h, control.bytes))
			if err == nil {
				switch h.opcode {
				case OpPing:
					err = c.writeFrame(OpPong, control.bytes)
				case OpClose:
					code, reason, decodeErr := DecodeClosePayload(control.bytes)
					if decodeErr != nil {
						err = decodeErr
					} else {
						// 先解除内部 payload 持有，再礼貌回应，不能用残留分片卡住 close。
						control.release()
						partial.release()
						c.respondToPeerClose(code)
						err = &CloseError{Code: code, Reason: reason, IncompleteMessage: fragments}
					}
				}
			}
			control.release()
			if err != nil {
				return nil, err
			}
			continue
		}
		if h.opcode == OpContinuation {
			if !fragments {
				return nil, fmt.Errorf("%w: 收到续帧但没有起始帧", ErrProtocol)
			}
		} else {
			if fragments {
				return nil, fmt.Errorf("%w: 分片未结束时收到新的起始帧", ErrProtocol)
			}
			opcode = h.opcode
			fragments = true
		}
		if h.size > c.reader.limit-int64(len(partial.bytes)) {
			return nil, ErrMessageTooLarge
		}
		start := len(partial.bytes)
		// reader.limit 可能大于本机 int，但单帧头和累加容量仍不能溢出。
		if h.size > int64(int(^uint(0)>>1)-start) {
			return nil, ErrMessageTooLarge
		}
		limit := int(min(c.reader.limit, int64(^uint(0)>>1)))
		if err := partial.grow(start+int(h.size), limit); err != nil {
			return nil, err
		}
		if err := readFramePayload(c.reader.r, h, partial.bytes[start:]); err != nil {
			return nil, readError(err)
		}
		if !h.fin {
			continue
		}
		if opcode == OpText && !utf8.Valid(partial.bytes) {
			return nil, fmt.Errorf("%w: 文本不是合法 UTF-8", ErrProtocol)
		}
		m := &Message{Opcode: opcode, Payload: partial.bytes, owned: &messageOwnership{budget: c.budget, size: int64(cap(partial.bytes))}}
		partial.bytes = nil
		return m, nil
	}
}
