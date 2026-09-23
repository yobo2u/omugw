package convstore

import (
	"context"
	"testing"
)

// TestConversationDataOwnership 防的是调用方通过 Append 输入或查询结果的切片别名，
// 绕过 Store 接口改写已经保存的历史。
func TestConversationDataOwnership(t *testing.T) {
	for _, source := range []string{"append_input", "history_output", "turn_output"} {
		t.Run(source, func(t *testing.T) {
			s, _ := newTestStore(t)
			ctx := context.Background()
			msgs := turn("original", "answer")
			id, err := s.Append(ctx, "", msgs, "m")
			if err != nil {
				t.Fatal(err)
			}

			switch source {
			case "append_input":
				msgs[0].Parts[0].Text = "tampered"
			case "history_output":
				history, err := s.History(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				history[0].Parts[0].Text = "tampered"
			case "turn_output":
				saved, err := s.Turn(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				saved.Messages[0].Parts[0].Text = "tampered"
			}

			got, err := s.History(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if got[0].TextContent() != "original" {
				t.Fatal("调用方修改了 Store 内部的会话历史")
			}
		})
	}
}

func TestOpaqueConversationDataOwnership(t *testing.T) {
	for _, source := range []string{"append_input", "turn_output", "turns_output"} {
		t.Run(source, func(t *testing.T) {
			s, _ := newTestStore(t)
			ctx := context.Background()
			opaque := []byte("original")
			if err := s.AppendWithID(ctx, Turn{
				ID: "resp_fixed", Messages: turn("q", "a"), Model: "m", Opaque: opaque,
			}); err != nil {
				t.Fatal(err)
			}
			switch source {
			case "append_input":
				opaque[0] = 'X'
			case "turn_output":
				got, err := s.Turn(ctx, "resp_fixed")
				if err != nil {
					t.Fatal(err)
				}
				got.Opaque[0] = 'X'
			case "turns_output":
				got, err := s.Turns(ctx, "resp_fixed")
				if err != nil {
					t.Fatal(err)
				}
				got[0].Opaque[0] = 'X'
			}
			got, err := s.Turn(ctx, "resp_fixed")
			if err != nil {
				t.Fatal(err)
			}
			if string(got.Opaque) != "original" {
				t.Fatalf("Opaque 被调用方改写为 %q", got.Opaque)
			}
		})
	}
}
