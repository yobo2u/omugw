package ws

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// 反向掩码不能被本地宽松解码器掩盖；合法方向仍须交付同一负载。
func TestProductionRoleMask(t *testing.T) {
	for _, role := range []Role{RoleClient, RoleServer} {
		for _, masked := range []bool{false, true} {
			t.Run(fmt.Sprintf("role-%d/masked-%t", role, masked), func(t *testing.T) {
				a, b := tcpPair(t)
				_ = a.SetDeadline(time.Now().Add(time.Second))
				c := NewConn(a, role, 1024, 0)
				if err := WriteFrame(b, Frame{FIN: true, Opcode: OpText, Payload: []byte("ok")}, masked); err != nil {
					t.Fatal(err)
				}
				_, p, err := c.ReadMessage()
				if masked != (role == RoleServer) {
					if !errors.Is(err, ErrProtocol) {
						t.Fatal("反向掩码未拒绝")
					}
				} else if err != nil || string(p) != "ok" {
					t.Fatal("合法掩码未交付")
				}
			})
		}
	}
}

func productionBudget(t *testing.T, limit int64) *BufferBudget {
	t.Helper()
	b, err := NewBufferBudget(limit)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func productionOwnedConn(t *testing.T, budget *BufferBudget, limit int64) (*Conn, net.Conn) {
	t.Helper()
	a, raw := tcpPair(t)
	c := NewConn(a, RoleClient, limit, 0)
	c.budget = budget
	c.writeTimeout = 100 * time.Millisecond
	return c, raw
}

func TestProductionBoundedWrite(t *testing.T) {
	t.Run("unread-peer", func(t *testing.T) {
		a, raw := tcpPair(t)
		_ = a.(*net.TCPConn).SetWriteBuffer(1024)
		_ = raw.(*net.TCPConn).SetReadBuffer(1024)
		c := NewConn(a, RoleClient, 16<<20, 0)
		c.writeTimeout = 50 * time.Millisecond
		done := make(chan error, 1)
		go func() { done <- c.WriteMessage(OpBinary, make([]byte, 16<<20)) }()
		t.Cleanup(func() { _ = a.Close() })
		err := awaitDialHandoff(t, done, "有限写未退出")
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			t.Fatal("不读对端没有触发写期限")
		}
	})
	t.Run("close-under-lock", func(t *testing.T) {
		a, raw := tcpPair(t)
		gate := &closeResultWriteGate{Conn: a, entered: make(chan struct{}), closed: make(chan struct{})}
		t.Cleanup(func() { _ = gate.Close() })
		c := NewConn(gate, RoleClient, 1024, 0)
		c.writeTimeout = time.Second
		done := make(chan error, 1)
		go func() { done <- c.Ping([]byte("hb")) }()
		awaitDialHandoff(t, gate.entered, "Ping 未持锁写入")
		sent, err := c.CloseWithResult(1000, "")
		if sent || err != nil {
			t.Fatal("持锁 Close 补造发送")
		}
		if awaitDialHandoff(t, done, "Close 未回收 Ping") == nil {
			t.Fatal("关闭未中断写")
		}
		if _, err := ReadFrame(raw, 125); !errors.Is(err, io.EOF) {
			t.Fatal("关闭未释放 socket")
		}
	})
	t.Run("ping-pong-and-deadline-clear", func(t *testing.T) {
		c, raw := productionOwnedConn(t, productionBudget(t, 4096), 1024)
		if err := c.Ping(make([]byte, 126)); !errors.Is(err, ErrProtocol) {
			t.Fatal("超长 Ping 未拒绝")
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		read := make(chan error, 1)
		go func() {
			m, err := c.ReadOwnedMessage(ctx)
			if m != nil {
				if string(m.Payload) != "data" {
					err = errors.New("消息被控制帧打乱")
				}
				m.Release()
			}
			read <- err
		}()
		ping := make(chan error, 1)
		go func() { ping <- c.Ping([]byte("local")) }()
		if err := WriteFrame(raw, Frame{FIN: true, Opcode: OpPing, Payload: []byte("peer")}, false); err != nil {
			t.Fatal(err)
		}
		seen := map[Opcode]string{}
		for i := 0; i < 2; i++ {
			f, err := ReadFrame(raw, 125)
			if err != nil {
				t.Fatal(err)
			}
			seen[f.Opcode] = string(f.Payload)
		}
		if seen[OpPing] != "local" || seen[OpPong] != "peer" {
			t.Fatal("并发 Ping 与自动 Pong 发生串帧")
		}
		if err := awaitDialHandoff(t, ping, "Ping 未归还"); err != nil {
			t.Fatal(err)
		}
		if err := WriteFrame(raw, Frame{FIN: true, Opcode: OpText, Payload: []byte("data")}, false); err != nil {
			t.Fatal(err)
		}
		if err := awaitDialHandoff(t, read, "读未归还"); err != nil {
			t.Fatal(err)
		}
		// 通过包装真实 socket 观察期限清除，不靠 sleep 等旧期限过期。
		a, _ := tcpPair(t)
		observed := &productionDeadlineConn{Conn: a}
		x := NewConn(observed, RoleClient, 1024, 0)
		x.writeTimeout = time.Second
		if err := x.Ping(nil); err != nil {
			t.Fatal(err)
		}
		if len(observed.writes) != 2 || observed.writes[0].IsZero() || !observed.writes[1].IsZero() {
			t.Fatal("写期限没有在写锁内成对设置清除")
		}
	})
}

type productionDeadlineConn struct {
	net.Conn
	writes []time.Time
}

func (c *productionDeadlineConn) SetWriteDeadline(d time.Time) error {
	c.writes = append(c.writes, d)
	return c.Conn.SetWriteDeadline(d)
}

func TestProductionOwnedReadCancellation(t *testing.T) {
	t.Run("continuous-ping", func(t *testing.T) {
		budget := productionBudget(t, 128)
		c, raw := productionOwnedConn(t, budget, 1024)
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		peer := make(chan int, 1)
		go func() {
			n := 0
			defer func() { peer <- n }()
			for {
				if WriteFrame(raw, Frame{FIN: true, Opcode: OpPing, Payload: []byte("hb")}, false) != nil {
					return
				}
				f, err := ReadFrame(raw, 125)
				if err != nil || f.Opcode != OpPong || string(f.Payload) != "hb" {
					return
				}
				n++
			}
		}()
		m, err := c.ReadOwnedMessage(ctx)
		if m != nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("持续 Ping 延长了绝对读期限")
		}
		if n := awaitDialHandoff(t, peer, "取消未释放实际 TCP"); n == 0 {
			t.Fatal("没有实际心跳交换")
		}
		if budget.Used() != 0 {
			t.Fatal("取消遗留控制帧额度")
		}
	})
	t.Run("handoff", func(t *testing.T) {
		budget := productionBudget(t, 64)
		c, raw := productionOwnedConn(t, budget, 1024)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if err := WriteFrame(raw, Frame{FIN: true, Opcode: OpText, Payload: []byte("one")}, false); err != nil {
			t.Fatal(err)
		}
		m, err := c.ReadOwnedMessage(ctx)
		if err != nil || string(m.Payload) != "one" {
			t.Fatal("首笔读失败")
		}
		m.Release()
		cancel()
		if err := WriteFrame(raw, Frame{FIN: true, Opcode: OpText, Payload: []byte("two")}, false); err != nil {
			t.Fatal(err)
		}
		next, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		m, err = c.ReadOwnedMessage(next)
		if err != nil || string(m.Payload) != "two" {
			t.Fatal("旧 ctx 取消关闭了下一笔读")
		}
		m.Release()
		if err := c.Ping([]byte("alive")); err != nil {
			t.Fatal(err)
		}
		if f, err := ReadFrame(raw, 125); err != nil || f.Opcode != OpPing || string(f.Payload) != "alive" {
			t.Fatal("成功交接后业务连接不可用")
		}
	})
	t.Run("partial-cancel-and-idle", func(t *testing.T) {
		for _, idle := range []time.Duration{0, 30 * time.Millisecond} {
			budget := productionBudget(t, 64)
			c, raw := productionOwnedConn(t, budget, 1024)
			c.idle = idle
			// deadlineReader 在构造时取得 idle，单独构造以免绕过真实读期限。
			c = newConnBuffered(c.conn, c.conn, RoleClient, 1024, idle)
			c.budget = budget
			if err := WriteFrame(raw, Frame{Opcode: OpText, Payload: []byte("half")}, false); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			m, err := c.ReadOwnedMessage(ctx)
			cancel()
			want := error(context.DeadlineExceeded)
			if idle > 0 {
				want = ErrIdleTimeout
			}
			if m != nil || !errors.Is(err, want) || budget.Used() != 0 {
				t.Fatal("中断没有返回对应期限错误或泄漏分片")
			}
		}
	})
}

// 在 payload Read 真正发生时取 Used，防止“先分配读完再记账”的假限额。
type productionReadProbe struct {
	io.Reader
	budget   *BufferBudget
	observed chan int64
	once     sync.Once
}

func (r *productionReadProbe) Read(p []byte) (int, error) {
	if len(p) == 32 {
		r.once.Do(func() { r.observed <- r.budget.Used() })
	}
	return r.Reader.Read(p)
}

func TestProductionBufferBudget(t *testing.T) {
	for _, n := range []int64{-1, 0} {
		if _, err := NewBufferBudget(n); err == nil {
			t.Fatal("无效预算可构造")
		}
	}
	t.Run("shared-ownership", func(t *testing.T) {
		b := productionBudget(t, 6)
		c1, raw1 := productionOwnedConn(t, b, 1024)
		c2, raw2 := productionOwnedConn(t, b, 1024)
		if _, _, err := c1.ReadMessage(); err == nil {
			t.Fatal("旧读接口绕过预算所有权")
		}
		_ = WriteFrame(raw1, Frame{FIN: true, Opcode: OpBinary, Payload: []byte("1234")}, false)
		m, err := c1.ReadOwnedMessage(context.Background())
		if err != nil || m.Opcode != OpBinary || string(m.Payload) != "1234" || b.Used() < int64(cap(m.Payload)) || b.Used() > 6 {
			t.Fatal("成功消息容量没有归调用方持有")
		}
		// 仅发头而无 payload；额度不足必须当场拒绝，不能等半条消息。
		_, _ = raw2.Write([]byte{0x82, 4})
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if got, err := c2.ReadOwnedMessage(ctx); got != nil || !errors.Is(err, ErrBufferLimit) {
			t.Fatal("共享预算不足没有立即拒绝")
		}
		if b.Used() != 4 {
			t.Fatal("失败读归还了他人的消息容量")
		}
		copyOfMessage := *m
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); copyOfMessage.Release(); m.Release() }()
		}
		wg.Wait()
		if b.Used() != 0 {
			t.Fatal("重复 Release 没有恰好归还一次")
		}
	})
	t.Run("reserved-before-payload", func(t *testing.T) {
		b := productionBudget(t, 32)
		a, raw := tcpPair(t)
		probe := &productionReadProbe{Reader: a, budget: b, observed: make(chan int64, 1)}
		c := newConnBuffered(a, probe, RoleClient, 1024, 0)
		c.budget = b
		_, _ = raw.Write([]byte{0x82, 32})
		done := make(chan error, 1)
		go func() {
			m, err := c.ReadOwnedMessage(context.Background())
			if m != nil {
				m.Release()
			}
			done <- err
		}()
		if used := awaitDialHandoff(t, probe.observed, "未进入 payload 读取"); used != 32 {
			t.Error("完整 payload 到达前容量未预占")
		}
		_ = c.Close(1000, "")
		if err := awaitDialHandoff(t, done, "关闭未归还半帧读"); err == nil {
			t.Fatal("半帧关闭误交付消息")
		}
		if b.Used() != 0 {
			t.Fatal("关闭遗留半帧容量")
		}
	})
	t.Run("fragment-growth-peak", func(t *testing.T) {
		b := productionBudget(t, 9)
		c, raw := productionOwnedConn(t, b, 1024)
		_ = WriteFrame(raw, Frame{Opcode: OpBinary, Payload: []byte("1234")}, false)
		_, _ = raw.Write([]byte{0x80, 4})
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if m, err := c.ReadOwnedMessage(ctx); m != nil || !errors.Is(err, ErrBufferLimit) {
			t.Fatal("扩容未计算旧4字节与新8字节并存峰值")
		}
		if b.Used() != 0 {
			t.Fatal("扩容失败保留半条消息额度")
		}
	})
	for _, tc := range []struct {
		name string
		wire []byte
		want error
	}{
		{"truncated", []byte{0x82, 4, 1}, io.ErrUnexpectedEOF},
		{"illegal-after-partial", []byte{0x02, 2, 1, 2, 0x83, 0}, ErrProtocol},
		{"bad-text", []byte{0x81, 1, 0xff}, ErrProtocol},
		{"oversize", []byte{0x82, 126, 4, 1}, ErrMessageTooLarge},
		{"fragment-oversize", append([]byte{0x02, 2, 1, 2, 0x80, 126}, []byte{4, 0}...), ErrMessageTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := productionBudget(t, 2048)
			c, raw := productionOwnedConn(t, b, 1024)
			_, _ = raw.Write(tc.wire)
			_ = raw.Close()
			if m, err := c.ReadOwnedMessage(context.Background()); m != nil || !errors.Is(err, tc.want) {
				t.Fatalf("失败分类不符: %v", err)
			}
			if b.Used() != 0 {
				t.Fatal("读失败泄漏容量")
			}
		})
	}
	t.Run("peer-close-partial", func(t *testing.T) {
		b := productionBudget(t, 64)
		c, raw := productionOwnedConn(t, b, 1024)
		_, _ = raw.Write([]byte{0x02, 2, 1, 2, 0x88, 0})
		_, err := c.ReadOwnedMessage(context.Background())
		var ce *CloseError
		if !errors.As(err, &ce) || ce.Code != 1005 || !ce.IncompleteMessage || b.Used() != 0 {
			t.Fatal("关闭丢失分片证据或泄漏额度")
		}
		if f, err := ReadFrame(raw, 125); err != nil || f.Opcode != OpClose || len(f.Payload) != 0 {
			t.Fatal("空 close 回应编码了本地状态")
		}
	})
	t.Run("masked-write-scratch", func(t *testing.T) {
		b := productionBudget(t, 7)
		c, raw := productionOwnedConn(t, b, 1024)
		_ = WriteFrame(raw, Frame{FIN: true, Opcode: OpBinary, Payload: []byte("1234")}, false)
		m, err := c.ReadOwnedMessage(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer m.Release()
		if err := c.WriteMessage(m.Opcode, m.Payload); !errors.Is(err, ErrBufferLimit) {
			t.Fatal("掩码临时副本未计入预算")
		}
		if b.Used() != 4 {
			t.Fatal("掩码失败改动了调用方所有权")
		}
	})
}

func TestProductionOptionsReachConn(t *testing.T) {
	b := productionBudget(t, 64)
	accepted := make(chan *Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _ := Accept(w, r, AcceptOptions{MaxPayload: 1024, WriteTimeout: time.Second, Budget: b})
		accepted <- c
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, _, err := Dial(ctx, wsURL(t, srv), DialOptions{MaxPayload: 1024, WriteTimeout: time.Second, Budget: b})
	if err != nil {
		t.Fatal(err)
	}
	defer c.conn.Close()
	s := awaitDialHandoff(t, accepted, "Accept 未交接")
	if s == nil {
		t.Fatal("Accept 失败")
	}
	defer s.conn.Close()
	for _, x := range []*Conn{c, s} {
		if _, _, err := x.ReadMessage(); err == nil {
			t.Fatal("握手 opts 没有接入所有权保护")
		}
	}
	if err := c.WriteMessage(OpText, []byte("ping")); err != nil {
		t.Fatal(err)
	}
	m, err := s.ReadOwnedMessage(ctx)
	if err != nil || string(m.Payload) != "ping" || b.Used() != 4 {
		t.Fatal("生产选项未生效")
	}
	m.Release()
	if c.writeTimeout != time.Second || s.writeTimeout != time.Second {
		t.Fatal("写期限未接入")
	}
}

func TestProductionUTF8AndClose(t *testing.T) {
	for _, tc := range []struct {
		name   string
		frames []Frame
		valid  bool
	}{
		{"split-rune", []Frame{{Opcode: OpText, Payload: []byte{0xe4}}, {FIN: true, Opcode: OpContinuation, Payload: []byte{0xb8, 0xad}}}, true},
		{"bad-text", []Frame{{FIN: true, Opcode: OpText, Payload: []byte{0xff}}}, false},
		{"truncated-rune", []Frame{{FIN: true, Opcode: OpText, Payload: []byte{0xe4}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, raw := pipeRaw(t, time.Second)
			for _, f := range tc.frames {
				if err := WriteFrame(raw, f, false); err != nil {
					t.Fatal(err)
				}
			}
			_, p, err := c.ReadMessage()
			if tc.valid {
				if err != nil || string(p) != "中" {
					t.Fatal("跨帧 UTF-8 字符丢失")
				}
			} else if !errors.Is(err, ErrProtocol) {
				t.Fatal("坏文本未拒绝")
			}
		})
	}
	for _, code := range []uint16{0, 999, 1004, 1005, 1006, 1015, 2000, 2999, 5000} {
		t.Run(fmt.Sprintf("close-%d", code), func(t *testing.T) {
			c, raw := pipeRaw(t, time.Second)
			wire := []byte{0x88, 2, 0, 0}
			binary.BigEndian.PutUint16(wire[2:], code)
			if _, err := raw.Write(wire); err != nil {
				t.Fatal(err)
			}
			if _, _, err := c.ReadMessage(); !errors.Is(err, ErrProtocol) {
				t.Fatal("非法关闭码未拒绝")
			}
		})
	}
	t.Run("empty-local-close", func(t *testing.T) {
		c, raw := pipeRaw(t, time.Second)
		if sent, err := c.CloseWithResult(CloseNoStatus, ""); err != nil || !sent {
			t.Fatal("空关闭没有实际发送")
		}
		f, err := ReadFrame(raw, 125)
		if err != nil || f.Opcode != OpClose || len(f.Payload) != 0 {
			t.Fatal("1005 被编码上线")
		}
	})
	for _, tc := range []struct {
		code   uint16
		reason string
	}{{1006, ""}, {1000, string([]byte{0xff})}} {
		t.Run(fmt.Sprintf("outgoing-%d", tc.code), func(t *testing.T) {
			c, raw := pipeRaw(t, time.Second)
			sent, err := c.CloseWithResult(tc.code, tc.reason)
			if sent || !errors.Is(err, ErrProtocol) {
				t.Fatal("非法主动关闭被当成已发送")
			}
			_ = raw.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := ReadFrame(raw, 125); !errors.Is(err, io.EOF) {
				t.Fatal("非法关闭未释放或写出帧")
			}
		})
	}
}

// 写入观察器只记录真实 WriteFrame 交给下层的块，短写是 io.Writer 合法失败形态。
type productionChunkWriter struct {
	bytes.Buffer
	largest         int
	largestCapacity int
	shortAt, calls  int
}

func (w *productionChunkWriter) Write(p []byte) (int, error) {
	w.calls++
	if len(p) > w.largest {
		w.largest = len(p)
	}
	if cap(p) > w.largestCapacity {
		w.largestCapacity = cap(p)
	}
	if w.calls == w.shortAt {
		return len(p) - 1, nil
	}
	return w.Buffer.Write(p)
}

func TestProductionChunkedMask(t *testing.T) {
	for _, n := range []int{3, 4, 5, 32767, 32768, 32769, 65539, 65540, 65541} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			p := make([]byte, n)
			for i := range p {
				p[i] = byte(i*37 + 11)
			}
			original := bytes.Clone(p)
			w := &productionChunkWriter{}
			if err := WriteFrame(w, Frame{FIN: true, Opcode: OpBinary, Payload: p}, true); err != nil {
				t.Fatal(err)
			}
			if w.largest > 32768 || w.largestCapacity > 32768 {
				t.Error("掩码仍整条复制写出")
			}
			f, err := ReadFrame(&w.Buffer, 1<<20)
			if err != nil || !bytes.Equal(f.Payload, original) || !bytes.Equal(p, original) {
				t.Fatal("跨块掩码或调用方负载改变")
			}
		})
	}
	for _, masked := range []bool{false, true} {
		for _, shortAt := range []int{1, 2} {
			w := &productionChunkWriter{shortAt: shortAt}
			if err := WriteFrame(w, Frame{FIN: true, Opcode: OpBinary, Payload: []byte("payload")}, masked); !errors.Is(err, io.ErrShortWrite) {
				t.Error("短写误报完整发送")
			}
		}
	}
}

// 门闩只延迟实际 socket 的关闭；回调已取得取消权后，读调用不得抢先归还成功。
type productionCloseGate struct {
	net.Conn
	entered, release chan struct{}
	once, opened     sync.Once
}

func (g *productionCloseGate) Close() error {
	g.once.Do(func() { close(g.entered) })
	<-g.release
	return g.Conn.Close()
}
func (g *productionCloseGate) open() { g.opened.Do(func() { close(g.release) }) }

type productionReadSignal struct {
	io.Reader
	entered, payload chan struct{}
	first, body      sync.Once
}

func (r *productionReadSignal) Read(p []byte) (int, error) {
	r.first.Do(func() { close(r.entered) })
	n, err := r.Reader.Read(p)
	if n == 3 {
		r.body.Do(func() { close(r.payload) })
	}
	return n, err
}

func TestProductionOwnedReadJoinsCancellation(t *testing.T) {
	a, raw := tcpPair(t)
	g := &productionCloseGate{Conn: a, entered: make(chan struct{}), release: make(chan struct{})}
	probe := &productionReadSignal{Reader: a, entered: make(chan struct{}), payload: make(chan struct{})}
	b := productionBudget(t, 64)
	c := newConnBuffered(g, probe, RoleClient, 1024, 0)
	c.budget = b
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	t.Cleanup(func() { g.open(); cancel(); _ = a.Close(); _ = raw.Close() })
	go func() {
		m, err := c.ReadOwnedMessage(ctx)
		if m != nil {
			m.Release()
			err = errors.New("取消取得关闭权却交付了消息")
		}
		done <- err
	}()
	awaitDialHandoff(t, probe.entered, "读取未就绪")
	cancel()
	awaitDialHandoff(t, g.entered, "取消未取得关闭权")
	if err := WriteFrame(raw, Frame{FIN: true, Opcode: OpBinary, Payload: []byte("abc")}, false); err != nil {
		t.Fatal(err)
	}
	awaitDialHandoff(t, probe.payload, "测试未实际读完 payload")
	select {
	case <-done:
		t.Fatal("回调关闭仍在途，ReadOwnedMessage 未 join 就返回")
	case <-time.After(30 * time.Millisecond):
	}
	g.open()
	if err := awaitDialHandoff(t, done, "取消回调退出后读未归还"); !errors.Is(err, context.Canceled) {
		t.Fatal("取消与成功交接没有唯一胜者")
	}
	if b.Used() != 0 {
		t.Fatal("取消抢占成功消息后没有 Release")
	}
	if _, err := a.Write([]byte{1}); err == nil {
		t.Fatal("取消未实际关闭 TCP")
	}
}

func TestProductionOwnedReadCancellationUnderWriteLock(t *testing.T) {
	a, raw := tcpPair(t)
	g := &closeResultWriteGate{Conn: a, entered: make(chan struct{}), closed: make(chan struct{})}
	t.Cleanup(func() { _ = g.Close() })
	probe := &productionReadSignal{Reader: a, entered: make(chan struct{}), payload: make(chan struct{})}
	c := newConnBuffered(g, probe, RoleClient, 1024, 0)
	c.budget = productionBudget(t, 64)
	write := make(chan error, 1)
	go func() { write <- c.Ping([]byte("local")) }()
	awaitDialHandoff(t, g.entered, "业务写没有取得锁")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	read := make(chan error, 1)
	go func() {
		m, err := c.ReadOwnedMessage(ctx)
		if m != nil {
			m.Release()
		}
		read <- err
	}()
	if err := WriteFrame(raw, Frame{FIN: true, Opcode: OpPing, Payload: []byte("abc")}, false); err != nil {
		t.Fatal(err)
	}
	awaitDialHandoff(t, probe.payload, "自动 pong 前未实际读完 ping")
	cancel()
	if err := awaitDialHandoff(t, read, "写锁阻塞了取消回收"); !errors.Is(err, context.Canceled) {
		t.Fatal("取消错误分类改变")
	}
	if err := awaitDialHandoff(t, write, "取消未归还持锁写"); err == nil {
		t.Fatal("持锁写错误被吞掉")
	}
	if c.budget.Used() != 0 {
		t.Fatal("自动 pong 或写 scratch 泄漏")
	}
}

type productionShortConn struct {
	net.Conn
	calls, shortAt int
}

func (c *productionShortConn) Write(p []byte) (int, error) {
	c.calls++
	if c.calls == c.shortAt {
		return c.Conn.Write(p[:len(p)-1])
	}
	return c.Conn.Write(p)
}

func TestProductionShortCloseHasNoSendEvidence(t *testing.T) {
	for _, at := range []int{1, 2} {
		a, raw := tcpPair(t)
		c := NewConn(&productionShortConn{Conn: a, shortAt: at}, RoleClient, 1024, 0)
		c.budget = productionBudget(t, 64)
		sent, err := c.CloseWithResult(1000, "bye")
		if sent || !errors.Is(err, io.ErrShortWrite) {
			t.Fatal("部分 close 写入误报 sent")
		}
		if _, err := ReadFrame(raw, 125); err == nil {
			t.Fatal("测试没有实际产生半帧")
		}
		if c.budget.Used() != 0 {
			t.Fatal("close 写失败泄漏临时容量")
		}
	}
}

// 真实 socket 的写期限在写前到期，防止自动 pong 的写失败被错称成接收空闲。
type productionExpiredWriteConn struct{ net.Conn }

func (c *productionExpiredWriteConn) SetWriteDeadline(d time.Time) error {
	if !d.IsZero() {
		d = time.Now().Add(-time.Second)
	}
	return c.Conn.SetWriteDeadline(d)
}

func TestProductionPongTimeoutIsNotReadIdle(t *testing.T) {
	a, raw := tcpPair(t)
	c := NewConn(&productionExpiredWriteConn{a}, RoleClient, 1024, 0)
	c.writeTimeout = time.Second
	c.budget = productionBudget(t, 64)
	if err := WriteFrame(raw, Frame{FIN: true, Opcode: OpPing, Payload: []byte("hb")}, false); err != nil {
		t.Fatal(err)
	}
	_, err := c.ReadOwnedMessage(context.Background())
	var ne net.Error
	if errors.Is(err, ErrIdleTimeout) || !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatal("自动 pong 写超时被误归接收空闲")
	}
	if c.budget.Used() != 0 {
		t.Fatal("pong 失败保留控制帧额度")
	}
}

func productionCloseReleaser(t *testing.T, err *CloseError) func() {
	t.Helper()
	r, ok := any(err).(interface{ Release() })
	if !ok {
		t.Fatal("受控关闭错误缺少 Release 所有权接口")
	}
	return r.Release
}

// 125 字节帧与 123 字节 reason 必须同时预占；仅按帧大小计额会漏掉整份副本。
func TestProductionCloseReasonBudget(t *testing.T) {
	reason := strings.Repeat("r", 123)
	for _, limit := range []int64{125, 247, 248} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			b := productionBudget(t, limit)
			c, raw := productionOwnedConn(t, b, 1024)
			if err := WriteFrame(raw, Frame{FIN: true, Opcode: OpClose, Payload: EncodeClosePayload(1000, reason)}, false); err != nil {
				t.Fatal(err)
			}
			m, err := c.ReadOwnedMessage(context.Background())
			if m != nil {
				m.Release()
				t.Fatal("关闭帧被交付成业务消息")
			}
			if limit < 248 {
				if !errors.Is(err, ErrBufferLimit) {
					t.Error("125+123 并存峰值没有在复制前拒绝")
				}
				if b.Used() != 0 {
					t.Error("副本预占失败后遗留控制帧额度")
				}
				return
			}
			var ce *CloseError
			if !errors.As(err, &ce) || ce.Code != 1000 || ce.Reason != reason || ce.IncompleteMessage {
				t.Fatal("正好够的预算没有保全关闭证据")
			}
			if b.Used() != 123 {
				t.Error("已交付 CloseError 的 reason 未继续占额")
			}
			copyOfError := *ce
			release, releaseCopy := productionCloseReleaser(t, ce), productionCloseReleaser(t, &copyOfError)
			var wg sync.WaitGroup
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() { defer wg.Done(); releaseCopy(); release() }()
			}
			wg.Wait()
			if b.Used() != 0 {
				t.Fatal("浅拷贝并发重复 Release 没有恰好归还一次")
			}
			f, err := ReadFrame(raw, 125)
			if err != nil || f.Opcode != OpClose || !bytes.Equal(f.Payload, []byte{3, 232}) {
				t.Fatal("关闭原因所有权改变了既有自动回应")
			}
			if sent, err := c.CloseWithResult(1000, ""); sent || err != nil {
				t.Fatal("Release 后补造了关闭发送证据")
			}
		})
	}
	t.Run("incomplete-message", func(t *testing.T) {
		b := productionBudget(t, 251)
		c, raw := productionOwnedConn(t, b, 1024)
		if err := WriteFrame(raw, Frame{Opcode: OpBinary, Payload: []byte("abc")}, false); err != nil {
			t.Fatal(err)
		}
		if err := WriteFrame(raw, Frame{FIN: true, Opcode: OpClose, Payload: EncodeClosePayload(1001, reason)}, false); err != nil {
			t.Fatal(err)
		}
		_, err := c.ReadOwnedMessage(context.Background())
		var ce *CloseError
		if !errors.As(err, &ce) || !ce.IncompleteMessage || ce.Reason != reason {
			t.Fatal("受控关闭丢失半条消息证据")
		}
		if b.Used() != 123 {
			t.Error("部分消息未归还或 reason 交接未计额")
		}
		productionCloseReleaser(t, ce)()
		if b.Used() != 0 {
			t.Fatal("释放关闭原因后额度未清空")
		}
	})
	t.Run("invalid-utf8-before-copy", func(t *testing.T) {
		b := productionBudget(t, 125)
		c, raw := productionOwnedConn(t, b, 1024)
		payload := append([]byte{3, 232}, bytes.Repeat([]byte{0xff}, 123)...)
		if err := WriteFrame(raw, Frame{FIN: true, Opcode: OpClose, Payload: payload}, false); err != nil {
			t.Fatal(err)
		}
		if m, err := c.ReadOwnedMessage(context.Background()); m != nil || !errors.Is(err, ErrProtocol) {
			t.Fatal("非法 UTF-8 没有先校验就占用 reason 副本额度")
		}
		if b.Used() != 0 {
			t.Fatal("非法 UTF-8 留下了关闭原因容量")
		}
	})
	t.Run("legacy-unbudgeted", func(t *testing.T) {
		c, raw := pipeRaw(t, time.Second)
		if err := WriteFrame(raw, Frame{FIN: true, Opcode: OpClose, Payload: EncodeClosePayload(1000, reason)}, false); err != nil {
			t.Fatal(err)
		}
		_, _, err := c.ReadMessage()
		var ce *CloseError
		if !errors.As(err, &ce) || ce.Reason != reason {
			t.Fatal("无 Budget 的旧调用没有保全原因")
		}
		productionCloseReleaser(t, ce)()
		if ce.Reason != reason {
			t.Fatal("无 Budget 的旧调用被新增了释放义务")
		}
	})
}

// 比较同一合法帧头分支的空/最大原因，专门阻止只校验出站 close 也复制整份 reason。
func TestProductionCloseReasonValidationAllocations(t *testing.T) {
	a, _ := tcpPair(t)
	_ = a.SetWriteDeadline(time.Now().Add(3 * time.Second))
	c := NewConn(a, RoleServer, 1024, 0)
	empty := []byte{3, 232}
	full := append([]byte{3, 232}, bytes.Repeat([]byte{'r'}, 123)...)
	emptyAllocs := testing.AllocsPerRun(20, func() {
		if c.WriteMessage(OpClose, empty) != nil {
			t.Fatal("空 close 实际写失败")
		}
	})
	fullAllocs := testing.AllocsPerRun(20, func() {
		if c.WriteMessage(OpClose, full) != nil {
			t.Fatal("最大 close 实际写失败")
		}
	})
	if fullAllocs > emptyAllocs {
		t.Error("出站 close 仅校验时仍额外分配 reason")
	}
	badCode := []byte{3, 238}
	badUTF8 := append([]byte{3, 232}, bytes.Repeat([]byte{0xff}, 123)...)
	codeAllocs := testing.AllocsPerRun(20, func() {
		if _, _, err := DecodeClosePayload(badCode); !errors.Is(err, ErrProtocol) {
			t.Fatal("非法关闭码未拒绝")
		}
	})
	utf8Allocs := testing.AllocsPerRun(20, func() {
		if _, _, err := DecodeClosePayload(badUTF8); !errors.Is(err, ErrProtocol) {
			t.Fatal("非法原因未拒绝")
		}
	})
	if utf8Allocs > codeAllocs {
		t.Error("拒绝非法 UTF-8 前复制了 reason")
	}
}

// 真正读完 payload 才放行取消时序，防止把未读完帧的失败冒充已构造 CloseError 的丢弃。
type productionCloseReadProbe struct {
	io.Reader
	entered, payload chan struct{}
	first, body      sync.Once
}

func (r *productionCloseReadProbe) Read(p []byte) (int, error) {
	r.first.Do(func() { close(r.entered) })
	n, err := io.ReadFull(r.Reader, p)
	if len(p) == 125 && n == 125 {
		r.body.Do(func() { close(r.payload) })
	}
	return n, err
}

func TestProductionCloseReasonCancellation(t *testing.T) {
	t.Run("callback-claims-before-handoff", func(t *testing.T) {
		a, raw := tcpPair(t)
		g := &productionCloseGate{Conn: a, entered: make(chan struct{}), release: make(chan struct{})}
		probe := &productionCloseReadProbe{Reader: a, entered: make(chan struct{}), payload: make(chan struct{})}
		b := productionBudget(t, 248)
		c := newConnBuffered(g, probe, RoleClient, 1024, 0)
		c.budget = b
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		exited := make(chan struct{})
		t.Cleanup(func() {
			cancel()
			g.open()
			_ = a.Close()
			_ = raw.Close()
			awaitDialHandoff(t, exited, "关闭原因取消读工作者未归还")
		})
		go func() { defer close(exited); _, err := c.ReadOwnedMessage(ctx); done <- err }()
		awaitDialHandoff(t, probe.entered, "关闭帧读取未就绪")
		cancel()
		awaitDialHandoff(t, g.entered, "取消未先取得 socket 关闭权")
		if err := WriteFrame(raw, Frame{FIN: true, Opcode: OpClose, Payload: EncodeClosePayload(1000, strings.Repeat("r", 123))}, false); err != nil {
			t.Fatal(err)
		}
		awaitDialHandoff(t, probe.payload, "取消竞争没有实际读完关闭原因")
		g.open()
		err := awaitDialHandoff(t, done, "取消未归还关闭错误")
		if !errors.Is(err, context.Canceled) {
			t.Fatal("取消先取得关闭权却交付了 CloseError")
		}
		if b.Used() != 0 {
			t.Fatal("CAS 丢弃 CloseError 时未归还原因额度")
		}
	})
	t.Run("ctx-error-overrides-close-error", func(t *testing.T) {
		a, raw := tcpPair(t)
		g := &productionCloseGate{Conn: a, entered: make(chan struct{}), release: make(chan struct{})}
		b := productionBudget(t, 248)
		c := NewConn(g, RoleClient, 1024, 0)
		c.budget = b
		base, cancel := context.WithCancel(context.Background())
		ctx := &productionDeferredCancelContext{Context: base, entered: make(chan struct{}), release: make(chan struct{}), exited: make(chan struct{})}
		done := make(chan error, 1)
		exited := make(chan struct{})
		t.Cleanup(func() {
			cancel()
			ctx.open()
			g.open()
			_ = a.Close()
			_ = raw.Close()
			awaitDialHandoff(t, exited, "ctx 覆盖关闭错误的读未归还")
		})
		if err := WriteFrame(raw, Frame{FIN: true, Opcode: OpClose, Payload: EncodeClosePayload(1000, strings.Repeat("r", 123))}, false); err != nil {
			t.Fatal(err)
		}
		go func() { defer close(exited); _, err := c.ReadOwnedMessage(ctx); done <- err }()
		// 被动回应已经走到真正关闭 socket 前：帧缓冲释放，原因仍在等待错误交接。
		awaitDialHandoff(t, g.entered, "被动回应未到达 socket 关闭")
		if b.Used() != 123 {
			t.Error("错误交接前没有持有原因容量")
		}
		cancel()
		awaitDialHandoff(t, ctx.entered, "测试未暂停取消回调调度")
		g.open()
		err := awaitDialHandoff(t, done, "ctx.Err 覆盖路径未归还")
		if !errors.Is(err, context.Canceled) {
			t.Fatal("取消已可见却交付了 CloseError")
		}
		if b.Used() != 0 {
			t.Error("ctx.Err 覆盖 CloseError 时未归还原因额度")
		}
		ctx.open()
		awaitDialHandoff(t, ctx.exited, "迟到取消调度未归还")
	})
}

// 只在真实 pong 完整写出后的期限清除处暂停，防止以伪造写结果冒充心跳交换。
type productionPongReturnGate struct {
	net.Conn
	entered, release chan struct{}
	once, opened     sync.Once
}

func (g *productionPongReturnGate) SetWriteDeadline(d time.Time) error {
	err := g.Conn.SetWriteDeadline(d)
	if d.IsZero() {
		g.once.Do(func() { close(g.entered); <-g.release })
	}
	return err
}

func (g *productionPongReturnGate) open() { g.opened.Do(func() { close(g.release) }) }

// 读循环先观察到取消时也必须释放 TCP；撤销迟到回调不能把错误退出当成成功交接。
func TestProductionOwnedReadCancellationBeforeCallback(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprint("partial=", partial), func(t *testing.T) {
			a, raw := tcpPair(t)
			gate := &productionPongReturnGate{Conn: a, entered: make(chan struct{}), release: make(chan struct{})}
			c := NewConn(gate, RoleClient, 1024, 0)
			c.budget = productionBudget(t, 128)
			c.writeTimeout = time.Second
			base, cancel := context.WithCancel(context.Background())
			ctx := &productionDeferredCancelContext{Context: base, entered: make(chan struct{}), release: make(chan struct{}), exited: make(chan struct{})}
			done, exited := make(chan error, 1), make(chan struct{})
			callbackEntered := false
			t.Cleanup(func() {
				cancel()
				ctx.open()
				gate.open()
				_ = a.Close()
				_ = raw.Close()
				awaitDialHandoff(t, exited, "取消读工作者未退出")
				if callbackEntered {
					awaitDialHandoff(t, ctx.exited, "迟到回调调度未退出")
				}
			})
			go func() {
				defer close(exited)
				m, err := c.ReadOwnedMessage(ctx)
				if m != nil {
					m.Release()
					err = errors.New("取消却交付了消息")
				}
				done <- err
			}()
			_ = raw.SetDeadline(time.Now().Add(3 * time.Second))
			if partial {
				if err := WriteFrame(raw, Frame{Opcode: OpText, Payload: []byte("half")}, false); err != nil {
					t.Fatal(err)
				}
			}
			if err := WriteFrame(raw, Frame{FIN: true, Opcode: OpPing, Payload: []byte("hb")}, false); err != nil {
				t.Fatal(err)
			}
			awaitDialHandoff(t, gate.entered, "自动 pong 未完整写出")
			if f, err := ReadFrame(raw, 125); err != nil || f.Opcode != OpPong || string(f.Payload) != "hb" {
				t.Fatalf("没有实际心跳交换: %+v %v", f, err)
			}
			cancel()
			awaitDialHandoff(t, ctx.entered, "取消回调未被暂停")
			callbackEntered = true
			gate.open()
			if err := awaitDialHandoff(t, done, "读循环未先观察到取消"); !errors.Is(err, context.Canceled) {
				t.Fatalf("取消错误分类改变: %v", err)
			}
			if c.budget.Used() != 0 {
				t.Error("取消遗留控制帧或部分消息额度")
			}
			// 对端已读完唯一 pong，既无待发帧也无未读输入；必须看到真正 EOF，不能接受超时。
			_ = raw.SetReadDeadline(time.Now().Add(time.Second))
			var b [1]byte
			if n, err := raw.Read(b[:]); n != 0 || !errors.Is(err, io.EOF) {
				t.Errorf("取消返回后 TCP 仍未释放: n=%d err=%v", n, err)
			}
			ctx.open()
			awaitDialHandoff(t, ctx.exited, "迟到回调未退出")
			if err := c.Ping(nil); !errors.Is(err, ErrClosed) {
				t.Errorf("取消返回后仍允许业务写: %v", err)
			}
		})
	}
}

// 合法的自定义 context 将回调调度门闩与 Done/Err 分离，确定性覆盖取消回调迟到的交错。
type productionDeferredCancelContext struct {
	context.Context
	entered, release, exited chan struct{}
	once                     sync.Once
}

func (c *productionDeferredCancelContext) Value(any) any { return nil }
func (c *productionDeferredCancelContext) AfterFunc(f func()) func() bool {
	return context.AfterFunc(c.Context, func() { close(c.entered); defer close(c.exited); <-c.release; f() })
}
func (c *productionDeferredCancelContext) open() { c.once.Do(func() { close(c.release) }) }
