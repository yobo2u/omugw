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
// 返回 CloseError 时同样转交非空 Reason 的所有权，须经 errors.As 取出并 Release；
// 被取消覆盖的关闭错误不交付，由本层归还容量。
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
			_ = abortTransport(c.conn)
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
		releaseCloseError(err)
		return nil, ctx.Err()
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			releaseCloseError(err)
			return nil, ctxErr
		}
	}
	return m, err
}

// 错误未交付时上层拿不到释放入口，取消不能把已复制的关闭原因遗留在预算里。
func releaseCloseError(err error) {
	var closed *CloseError
	if errors.As(err, &closed) {
		closed.Release()
	}
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
	// 先比较半上限再翻倍，避免 int 溢出；接近上限只扩一次，不能逐片整条复制。
	capacity := limit
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
					// 已发 close 后不再写控制帧，但仍须读到真正的 close 回应。
					if !c.closing.Load() {
						err = c.writeFrame(OpPong, control.bytes)
						if errors.Is(err, ErrClosed) && c.closing.Load() {
							err = nil
						}
					}
				case OpClose:
					code, reason, decodeErr := parseClosePayload(control.bytes)
					if decodeErr != nil {
						err = decodeErr
					} else if err = c.budget.acquire(int64(len(reason))); err == nil {
						// 字符串确实拥有独立副本，帧缓冲释放前必须同时计入这两份容量。
						closed := &CloseError{Code: code, Reason: string(reason), IncompleteMessage: fragments}
						if c.budget != nil && len(reason) != 0 {
							closed.owned = &messageOwnership{budget: c.budget, size: int64(len(reason))}
						}
						// 先解除内部 payload 持有，再礼貌回应，不能用残留分片卡住 close。
						control.release()
						partial.release()
						c.respondToPeerClose(code)
						err = closed
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
			return nil, fmt.Errorf("%w: 文本不是合法 UTF-8", ErrInvalidUTF8)
		}
		m := &Message{Opcode: opcode, Payload: partial.bytes, owned: &messageOwnership{budget: c.budget, size: int64(cap(partial.bytes))}}
		partial.bytes = nil
		return m, nil
	}
}
