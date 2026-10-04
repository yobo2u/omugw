package ws

import (
	"errors"
	"sync"
	"testing"

	dsi "github.com/yobo2u/omugw/internal/protocol/dashscopeinference"
)

func TestAllocateMessageReservesBeforeFill(t *testing.T) {
	if m, err := AllocateMessage(nil, OpText, -1, func([]byte) error {
		t.Fatal("未配置预算时负大小仍进入 fill")
		return nil
	}); m != nil || !errors.Is(err, ErrBufferLimit) {
		t.Fatal("负大小绕过参数校验", err)
	}
	b := productionBudget(t, 8)
	if m, err := AllocateMessage(b, OpText, 8, nil); m != nil || !errors.Is(err, ErrProtocol) || b.Used() != 0 {
		t.Fatal("空 fill 未安全失败", err)
	}
	fillErr := errors.New("填充失败")
	for _, tc := range []struct {
		name          string
		size          int
		op            Opcode
		payload       string
		fillErr, want error
		called        bool
	}{
		{"over-budget", 9, OpText, "", nil, ErrBufferLimit, false},
		{"negative", -1, OpText, "", nil, ErrBufferLimit, false},
		{"fill-error", 8, OpText, "", fillErr, fillErr, true},
		{"invalid-text", 1, OpText, "\xff", nil, ErrInvalidUTF8, true},
		{"binary", 1, OpBinary, "\xff", nil, nil, true},
		{"empty", 0, OpText, "", nil, nil, true},
		{"exact", 8, OpText, "12345678", nil, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := productionBudget(t, 8)
			called := false
			m, err := AllocateMessage(b, tc.op, tc.size, func(dst []byte) error {
				called = true
				if b.Used() != int64(tc.size) || len(dst) != tc.size || cap(dst) != tc.size {
					t.Error("fill 前未精确计额或暴露了额外容量")
				}
				copy(dst, tc.payload)
				return tc.fillErr
			})
			if !errors.Is(err, tc.want) || called != tc.called {
				t.Fatalf("err=%v called=%v", err, called)
			}
			if err != nil {
				if m != nil || b.Used() != 0 {
					t.Fatal("失败转交了消息或泄漏额度")
				}
				return
			}
			if m.Opcode != tc.op || string(m.Payload) != tc.payload || b.Used() != int64(tc.size) {
				t.Fatal("消息字节或持有额度改变")
			}
			copyOfMessage := *m
			var releases sync.WaitGroup
			for range 8 {
				releases.Go(func() { m.Release(); copyOfMessage.Release() })
			}
			releases.Wait()
			if b.Used() != 0 {
				t.Fatal("浅拷贝重复归还或泄漏额度")
			}
		})
	}

	t.Run("direct-task-failure", func(t *testing.T) {
		id := "task-\"\\\n\x01中文"
		const want = `{"header":{"event":"task-failed","task_id":"task-\"\\\n\u0001中文","error_code":"Gateway.TaskStartTimeout","error_message":"task did not start within gateway budget"},"payload":{}}`
		size, err := dsi.TaskFailureSize(id, dsi.LocalStartTimeout)
		if err != nil || size != len(want) {
			t.Fatal("转义 ID 的精确大小错误", size, err)
		}
		b := productionBudget(t, int64(size))
		var filled *byte
		m, err := AllocateMessage(b, OpText, size, func(dst []byte) error {
			if b.Used() != int64(size) {
				t.Fatal("本地错误在预算外编码")
			}
			filled = &dst[0]
			return dsi.PutTaskFailure(dst, id, dsi.LocalStartTimeout)
		})
		if err != nil {
			t.Fatal(err)
		}
		defer m.Release()
		if string(m.Payload) != want || filled != &m.Payload[0] || b.Used() != int64(size) {
			t.Fatal("未直接交付已计额编码区或 ID 转义改变")
		}
		other, err := AllocateMessage(b, OpText, 1, func([]byte) error {
			t.Fatal("已有消息占满预算仍调用 fill")
			return nil
		})
		if other != nil || !errors.Is(err, ErrBufferLimit) {
			t.Fatal("持有中的消息未参与共享容量裁决")
		}
		m.Release()
		if b.Used() != 0 {
			t.Fatal("本地错误未归还额度")
		}
	})
}
