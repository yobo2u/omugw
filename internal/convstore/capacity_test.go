package convstore

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestGlobalTurnLimitRejectsNewRoots 防的是「单链上限拦不住的那种撑爆」。
//
// MaxChainDepth 与 MaxMessages 都是**沿链**统计的，而每个不带
// previous_response_id 的请求都是一条全新的根链：深度 1、消息 2。于是一个
// 普通调用方只要持续发一次性请求，就能在 TTL 内无限堆积根链，两条单链上限
// 一次都不会触发。必须有一条与链无关的全局闸门。
func TestGlobalTurnLimitRejectsNewRoots(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxTurns = 3
	s := NewMemoryStore(limits, nil)
	ctx := context.Background()

	for i := 0; i < limits.MaxTurns; i++ {
		if _, err := s.Append(ctx, "", turn("q", "a"), "m"); err != nil {
			t.Fatalf("第 %d 条根链不该失败: %v", i+1, err)
		}
	}

	_, err := s.Append(ctx, "", turn("q", "a"), "m")
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("超过全局轮数上限应返回 ErrTooLarge，实际 %v", err)
	}
	if got := s.Len(); got != limits.MaxTurns {
		t.Fatalf("拒绝之后仍在增长: Len=%d，上限 %d", got, limits.MaxTurns)
	}
}

// TestGlobalByteLimitRejectsOversizedTotal 覆盖另一维：轮数没超，但累计负载
// 已经吃掉了整个进程的内存预算。
//
// 纯文本的 InlineBytes 是 0，所以字节闸门不能只看内联媒体——否则 20 个 4 MiB
// 的纯文本请求照样一路放行。
func TestGlobalByteLimitRejectsOversizedTotal(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxTurns = 1000
	limits.MaxTotalBytes = 4096
	s := NewMemoryStore(limits, nil)
	ctx := context.Background()

	big := strings.Repeat("字", 1024) // 远超 4096 字节预算的两轮
	var err error
	for i := 0; i < 10; i++ {
		_, err = s.Append(ctx, "", turn(big, big), "m")
		if err != nil {
			break
		}
	}
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("超过全局字节上限应返回 ErrTooLarge，实际 %v", err)
	}
}

// TestGlobalLimitFreesCapacityOnDelete 固化「闸门是容量而不是单调计数器」。
//
// 若删除与过期不回收额度，网关跑够久之后会在远低于真实占用的水位上开始拒绝
// 一切写入——那是另一种形式的不可用，而且看起来像限流配错了。
func TestGlobalLimitFreesCapacityOnDelete(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxTurns = 2
	s := NewMemoryStore(limits, nil)
	ctx := context.Background()

	first, err := s.Append(ctx, "", turn("q1", "a1"), "m")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(ctx, "", turn("q2", "a2"), "m"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(ctx, "", turn("q3", "a3"), "m"); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("已满时应拒绝，实际 %v", err)
	}

	if err := s.Delete(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(ctx, "", turn("q3", "a3"), "m"); err != nil {
		t.Fatalf("删除腾出额度后仍被拒: %v", err)
	}
}

// TestGlobalLimitFreesCapacityOnExpiry 与上一条同理，但走 TTL 过期回收。
func TestGlobalLimitFreesCapacityOnExpiry(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxTurns = 1
	clk := &fakeClock{t: time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)}
	s := NewMemoryStore(limits, clk.Now)
	ctx := context.Background()

	if _, err := s.Append(ctx, "", turn("q1", "a1"), "m"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(ctx, "", turn("q2", "a2"), "m"); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("已满时应拒绝，实际 %v", err)
	}

	clk.Advance(limits.TTL + time.Minute)
	if n := s.GC(); n != 1 {
		t.Fatalf("GC 清理数 = %d，期望 1", n)
	}
	if _, err := s.Append(ctx, "", turn("q2", "a2"), "m"); err != nil {
		t.Fatalf("过期回收后仍被拒: %v", err)
	}
}

// TestDefaultLimitsBoundTotalRetention 固化默认值本身也是有界的。
// 一个「字段存在但默认零值 = 无限」的上限，等于没有上限。
func TestDefaultLimitsBoundTotalRetention(t *testing.T) {
	d := DefaultLimits()
	if d.MaxTurns <= 0 {
		t.Errorf("MaxTurns 默认值 = %d，必须为正", d.MaxTurns)
	}
	if d.MaxTotalBytes <= 0 {
		t.Errorf("MaxTotalBytes 默认值 = %d，必须为正", d.MaxTotalBytes)
	}
}
