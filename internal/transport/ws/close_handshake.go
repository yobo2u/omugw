package ws

import (
	"context"
	"fmt"
	"time"
	"unicode/utf8"
)

// ArmCloseDeadline 只收紧连接的绝对收尾期限，不发帧、不封普通写，也不取消读上下文。
// 每连接只拥有一个守卫；所有成功返回的 finish（即使守卫已到期）都须调用并 join。
// finish 中止剩余 I/O 且可重复并发调用；零期限不接管连接，返回 nil finish。
func (c *Conn) ArmCloseDeadline(deadline time.Time) (finish func(), err error) {
	if deadline.IsZero() {
		return nil, fmt.Errorf("%w: 关闭守卫必须有绝对期限", ErrProtocol)
	}
	// 装入最早期限与唤醒守卫在同一锁内，避免首次创建和并发收紧漏掉通知。
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	tightened := false
	for {
		previous := c.closeDeadline.Load()
		if previous != nil && !deadline.Before(*previous) {
			break
		}
		if c.closeDeadline.CompareAndSwap(previous, &deadline) {
			tightened = true
			break
		}
	}
	if c.closeGuard == nil {
		c.closeGuard = c.watchTransportClose()
	} else if tightened {
		select {
		case c.closeGuard.changed <- struct{}{}:
		default:
		}
	}
	return c.closeGuard.finish, nil
}

// BeginClose 发起关闭但保留读侧，让调用方独立接收对端 close；sent 只证明本次完整写出。
// deadline 是整个收尾共用的绝对 I/O 期限（含等写锁、帧写、回应、TLS 释放），后续调用不能续期。
// 返回的 finish 无论 err 是否为空都必须调用：它中止剩余 I/O 并 join 期限守卫，可重复调用。
// 调用方仍负责 join 自己的读写 worker、释放 Message/CloseError；finish 不消费接收证据。
// 参数非法时不接管连接，finish 为空操作；需要等待回应的调用方须用本接口而非原始 OpClose 写。
func (c *Conn) BeginClose(code uint16, reason string, deadline time.Time) (sent bool, finish func(), err error) {
	finish = func() {}
	if deadline.IsZero() {
		return false, finish, fmt.Errorf("%w: 关闭握手必须有绝对期限", ErrProtocol)
	}
	if code != CloseNoStatus && !validCloseCode(code) || code == CloseNoStatus && reason != "" {
		return false, finish, fmt.Errorf("%w: 主动关闭状态码或原因非法", ErrProtocol)
	}
	if !utf8.ValidString(reason) {
		return false, finish, fmt.Errorf("%w: close 原因不是合法 UTF-8", ErrInvalidUTF8)
	}
	finish, err = c.ArmCloseDeadline(deadline)
	if err != nil {
		return false, finish, err
	}
	c.closing.Store(true)
	deadline = c.limitCloseDeadline(deadline)
	if !time.Now().Before(deadline) {
		return false, finish, context.DeadlineExceeded
	}
	// 已取得物理关闭权的一方可能还在 TLS 清理；不排队争它的写锁，守卫仍覆盖其释放。
	// 这里只能返回 sent=false，不能把状态位冒充实际帧发送证据。
	if c.closed.Load() {
		return false, finish, ErrClosed
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	deadline = c.limitCloseDeadline(deadline)
	if !time.Now().Before(deadline) {
		return false, finish, context.DeadlineExceeded
	}
	if c.closed.Load() || !c.closeClaimed.CompareAndSwap(false, true) {
		return false, finish, ErrClosed
	}
	writeDeadline := deadline
	if c.writeTimeout > 0 {
		writeDeadline = minDeadline(writeDeadline, time.Now().Add(c.writeTimeout))
	}
	if err := c.conn.SetWriteDeadline(writeDeadline); err != nil {
		return false, finish, err
	}
	defer c.conn.SetWriteDeadline(time.Time{})
	size := closePayloadSize(code, reason)
	if err := c.budget.acquire(int64(size)); err != nil {
		return false, finish, err
	}
	payload := EncodeClosePayload(code, reason)
	defer func() { payload = nil; c.budget.release(int64(size)) }()
	err = writeFrameBudget(c.conn, Frame{FIN: true, Opcode: OpClose, Payload: payload}, c.role.masks(), c.budget)
	return err == nil, finish, err
}

func (c *Conn) limitCloseDeadline(deadline time.Time) time.Time {
	if limit := c.closeDeadline.Load(); limit != nil {
		return minDeadline(deadline, *limit)
	}
	return deadline
}

func minDeadline(a, b time.Time) time.Time {
	if a.IsZero() || !b.IsZero() && b.Before(a) {
		return b
	}
	return a
}
