package gateway

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

func TestWSRegistryPendingAndDrain(t *testing.T) {
	r := newWSRegistry(1)
	s, err := r.Register(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Register(context.Background()); !errors.Is(err, errWSRegistryFull) || errWSRegistryFull.HTTPStatus() != 429 {
		t.Fatalf("pending 未占限: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("未等待 Done: %v", err)
	}
	awaitWSTest(t, s.Context().Done())
	if !errors.Is(context.Cause(s.Context()), errWSShutdown) {
		t.Fatal("关停原因丢失")
	}
	if _, err := r.Register(context.Background()); !errors.Is(err, errWSRegistrySealed) || errWSRegistrySealed.HTTPStatus() != 503 {
		t.Fatalf("封口后还可登记: %v", err)
	}
	s.Done()
	s.Done()
	for i := 0; i < 2; i++ {
		if err := r.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWSRegistryDoneReleasesSlotAndParentCancellation(t *testing.T) {
	r := newWSRegistry(1)
	ctx, cancel := context.WithCancel(context.Background())
	s, err := r.Register(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	awaitWSTest(t, s.Context().Done())
	if _, err := r.Register(context.Background()); !errors.Is(err, errWSRegistryFull) {
		t.Fatal("取消不代表 worker 已退出")
	}
	s.Done()
	next, err := r.Register(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s.Done()
	if _, err := r.Register(context.Background()); !errors.Is(err, errWSRegistryFull) {
		t.Fatal("重复 Done 错还名额")
	}
	next.Done()
	if err := r.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestWSRegistryCloseOutsideLockAndWaitForDone(t *testing.T) {
	r := newWSRegistry(2)
	s, _ := r.Register(context.Background())
	pending, _ := r.Register(context.Background())
	b := wsTestBudget(t, 1<<20)
	var gate *wsTestGate
	c, peer := wsTestLink(t, false, b, 1<<20, time.Second, func(c net.Conn) net.Conn {
		gate = newWSTestGate(c, false)
		return gate
	})
	if !s.Attach(c) {
		t.Fatal("未封口却拒绝 Attach")
	}
	gate.armed.Store(true)
	shutdown := make(chan error, 1)
	go func() { shutdown <- r.Shutdown(context.Background()) }()
	awaitWSTest(t, gate.entered)
	registered := make(chan error, 1)
	go func() { _, err := r.Register(context.Background()); registered <- err }()
	if err := receiveWSTest(t, registered); !errors.Is(err, errWSRegistrySealed) {
		t.Fatal(err)
	}
	// 一个慢 close 不能阻止尚无连接的 pending 取消，也不能占住 registry 锁。
	awaitWSTest(t, pending.Context().Done())
	pending.Done()
	gate.unblock()
	assertWSTestClose(t, peer, 1001, "")
	awaitWSTest(t, s.Context().Done())
	select {
	case <-shutdown:
		t.Fatal("worker 未 Done 就声称排空")
	default:
	}
	s.Done()
	if err := receiveWSTest(t, shutdown); err != nil {
		t.Fatal(err)
	}
	if b.Used() != 0 {
		t.Fatal("close 预算泄漏")
	}
}

func TestWSRegistryRegisterAttachShutdownRace(t *testing.T) {
	for i := 0; i < 12; i++ {
		r := newWSRegistry(1)
		start := make(chan struct{})
		registered := make(chan *wsSession, 1)
		registerErr := make(chan error, 1)
		shutdown := make(chan error, 1)
		go func() { <-start; s, err := r.Register(context.Background()); registered <- s; registerErr <- err }()
		go func() { <-start; shutdown <- r.Shutdown(context.Background()) }()
		close(start)
		s := receiveWSTest(t, registered)
		err := receiveWSTest(t, registerErr)
		if s != nil {
			awaitWSTest(t, s.Context().Done())
			s.Done()
		} else if !errors.Is(err, errWSRegistrySealed) {
			t.Fatal(err)
		}
		if err := receiveWSTest(t, shutdown); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 12; i++ {
		r := newWSRegistry(1)
		s, _ := r.Register(context.Background())
		b := wsTestBudget(t, 1024)
		c, peer := wsTestLink(t, false, b, 1024, time.Second, nil)
		start := make(chan struct{})
		attached := make(chan bool, 1)
		shutdown := make(chan error, 1)
		go func() { <-start; attached <- s.Attach(c) }()
		go func() { <-start; shutdown <- r.Shutdown(context.Background()) }()
		close(start)
		_ = receiveWSTest(t, attached)
		assertWSTestClose(t, peer, 1001, "")
		awaitWSTest(t, s.Context().Done())
		s.Done()
		if err := receiveWSTest(t, shutdown); err != nil {
			t.Fatal(err)
		}
		if b.Used() != 0 {
			t.Fatal("竞争 Attach 泄漏预算")
		}
	}
}

func TestWSRegistryRepeatedShutdownAndLateAttach(t *testing.T) {
	r := newWSRegistry(1)
	s, _ := r.Register(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = r.Shutdown(ctx)
	awaitWSTest(t, s.Context().Done())
	b := wsTestBudget(t, 1024)
	c, peer := wsTestLink(t, true, b, 1024, time.Second, nil)
	if s.Attach(c) {
		t.Fatal("封口后 Attach 成功")
	}
	assertWSTestClose(t, peer, 1001, "")
	s.Done()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.Shutdown(context.Background()); err != nil {
				t.Error(err)
			}
			s.Done()
		}()
	}
	wg.Wait()
	if b.Used() != 0 {
		t.Fatal("迟到连接未释放")
	}
}
