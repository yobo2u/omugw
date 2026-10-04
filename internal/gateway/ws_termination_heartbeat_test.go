package gateway

import (
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/config"
	dsi "github.com/yobo2u/omugw/internal/protocol/dashscopeinference"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

type wsHeartbeatCloseGate struct {
	*wsTestGate
	closed chan struct{}
	once   sync.Once
}

func (g *wsHeartbeatCloseGate) Close() error {
	g.once.Do(func() { close(g.closed) })
	return g.wsTestGate.Close()
}

// 固定 local failure 已交付、heartbeat 已持写锁的窗口；短暂争锁不能冒充耗尽 B。
// stalled 分支另证同一个期限仍会强拆真正不返回的写，不能靠等锁无限延命。
func TestWSTerminationLocalFailureHeartbeatClose(t *testing.T) {
	for _, stalled := range []bool{false, true} {
		name := "released"
		if stalled {
			name = "stalled"
		}
		t.Run(name, func(t *testing.T) {
			b := wsTestBudget(t, 1<<20)
			var gate *wsHeartbeatCloseGate
			down, client := wsTestLink(t, false, b, 1<<18, 0, func(c net.Conn) net.Conn {
				gate = &wsHeartbeatCloseGate{wsTestGate: newWSTestGate(c, false), closed: make(chan struct{})}
				return gate
			})
			up, server := wsTestLink(t, true, b, 1<<18, 0, nil)
			timeouts := config.Timeouts{Connect: 80 * time.Millisecond, FirstByte: 160 * time.Millisecond, Idle: 160 * time.Millisecond}
			closing, pingDone, done := make(chan struct{}), make(chan error, 1), make(chan error, 1)
			pingExited, closeExited := make(chan struct{}), make(chan struct{})
			var deadline time.Time
			term := newWSTermination(func(code uint16, reason string, until time.Time) {
				// 只安排已选中 ticker 的 Ping；交付、物理 close 与守卫仍走正式实现。
				gate.armed.Store(true)
				go func() { defer close(pingExited); pingDone <- down.Ping(nil) }()
				<-gate.entered
				deadline = until
				close(closing)
				closeWSConnections([]*ws.Conn{down, up}, code, reason, until)
			}, wsCloseBudget(timeouts))
			if err := term.attachRelay(down, up, wsRelayOptions{Timeouts: timeouts, Budget: b, MaxMessageBytes: 1 << 18}); err != nil {
				t.Fatal(err)
			}
			failure := canonical.Newf(canonical.ClassUpstreamUnavailable, "task_drain_timeout")
			at := time.Now()
			won, _ := term.policyEnd(wsPolicyEnd{At: at, Failure: failure, Code: 1011, Reason: "task_drain_timeout", Local: &wsLocalTaskFailure{TaskID: "a", Kind: dsi.LocalDrainTimeout}}, false)
			if !won {
				t.Fatal("本地失败未认领")
			}
			go func() { defer close(closeExited); done <- term.close() }()
			t.Cleanup(func() {
				gate.unblock()
				_ = client.Close()
				_ = server.Close()
				_ = down.Close(1001, "")
				_ = up.Close(1001, "")
				awaitWSTest(t, closeExited)
				awaitWSTest(t, pingExited)
				if b.Used() != 0 {
					t.Error("清理后预算未归零", b.Used())
				}
			})
			inferenceLocalFailure(t, client, "a", "Gateway.TaskDrainTimeout")
			awaitWSTest(t, closing)
			if deadline.After(at.Add(160 * time.Millisecond)) {
				t.Fatal("交付后重开 Dall")
			}
			// 此 timer 只断言尚有 B 时不能提前强拆；竞争顺序由上面的 entered 屏障决定。
			observation := time.NewTimer(20 * time.Millisecond)
			defer observation.Stop()
			select {
			case <-gate.closed:
				t.Errorf("heartbeat 持锁即强拆，关闭期限尚余 %s", time.Until(deadline))
			case <-observation.C:
			}
			if !stalled {
				gate.unblock()
				// 本例只安排一帧空 Ping；被动取证，避免关闭后的测试端 pong 写遮蔽 close。
				if f := client.read(t); f.Opcode != ws.OpPing || len(f.Payload) != 0 {
					t.Fatal("关闭抢在已持锁的 heartbeat 之前", f)
				}
				inferenceWireClose(t, client, 1011, "task_drain_timeout")
			}
			inferenceWireClose(t, server, 1011, "task_drain_timeout")
			if err := receiveWSTest(t, done); !errors.Is(err, failure) {
				t.Fatal("关闭覆盖了业务失败", err)
			}
			if err := receiveWSTest(t, pingDone); !stalled && err != nil {
				t.Fatal("可恢复的 heartbeat 被强拆", err)
			} else if stalled && err == nil {
				t.Fatal("阻塞 heartbeat 未在绝对期限强拆")
			}
			awaitWSTest(t, closeExited)
			awaitWSTest(t, pingExited)
			if time.Now().After(deadline.Add(wsSchedulingSlack())) {
				t.Fatal("争锁/强拆及 join 超出同一绝对期限")
			}
			if b.Used() != 0 {
				t.Fatal("关闭后预算未归零", b.Used())
			}
			if term.deadline.After(deadline) || !errors.Is(term.close(), failure) {
				t.Fatal("重复关闭续期或覆盖业务结果")
			}
		})
	}
}
