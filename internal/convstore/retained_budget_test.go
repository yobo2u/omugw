package convstore

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/canonical"
)

// 许多短块的结构比文本本身更大，不能以空文本为由按零成本保留。
func TestByteBudgetIncludesContentStructures(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxTotalBytes = 8192
	for _, text := range []string{"", "a"} {
		t.Run("text="+text, func(t *testing.T) {
			s := NewMemoryStore(limits, nil)
			parts := make([]canonical.Part, 256)
			for i := range parts {
				parts[i] = canonical.Text(text)
			}
			_, err := s.Append(t.Context(), "", []canonical.Message{{Role: canonical.RoleUser, Parts: parts}}, "m")
			if !errors.Is(err, ErrTooLarge) || s.Len() != 0 {
				t.Fatalf("超预算结构仍被保存: err=%v len=%d", err, s.Len())
			}
		})
	}
}

func TestByteBudgetIncludesIdentifiers(t *testing.T) {
	large := strings.Repeat("x", 2048)
	for _, tc := range []struct {
		name string
		edit func(*Turn)
	}{
		{"id", func(v *Turn) { v.ID = large }},
		{"owner", func(v *Turn) { v.Owner = large }},
		{"message_name", func(v *Turn) { v.Messages[0].Name = large }},
		{"call_id", func(v *Turn) {
			v.Messages[0].Parts = []canonical.Part{{Kind: canonical.PartToolCall, ToolCall: &canonical.ToolCall{ID: large, Name: "f"}}}
		}},
		{"media_detail", func(v *Turn) {
			p := canonical.ImageURL("https://example.invalid/a", "image/png")
			p.Media.Detail = large
			v.Messages[0].Parts = []canonical.Part{p}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limits := DefaultLimits()
			limits.MaxTotalBytes = 1024
			s := NewMemoryStore(limits, nil)
			v := Turn{ID: "r", Messages: []canonical.Message{canonical.UserText("q")}}
			tc.edit(&v)
			if err := s.AppendWithID(t.Context(), v); !errors.Is(err, ErrTooLarge) {
				t.Fatalf("标识/元数据未计入预算: %v", err)
			}
		})
	}
}

func TestByteBudgetRejectsOverflow(t *testing.T) {
	s := NewMemoryStore(DefaultLimits(), nil)
	err := s.AppendWithID(t.Context(), Turn{
		ID: "r", Messages: []canonical.Message{canonical.UserText("q")}, InlineBytes: math.MaxInt64,
	})
	if !errors.Is(err, ErrTooLarge) || s.Len() != 0 {
		t.Fatalf("溢出未被拒绝: err=%v len=%d", err, s.Len())
	}
}

func TestByteCapacityReturnsAfterCleanup(t *testing.T) {
	for _, cleanup := range []string{"delete", "expire"} {
		t.Run(cleanup, func(t *testing.T) {
			limits := DefaultLimits()
			limits.MaxTotalBytes = 4096
			clock := &fakeClock{t: time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)}
			s := NewMemoryStore(limits, clock.Now)
			messages := []canonical.Message{canonical.UserText(strings.Repeat("a", 2500))}
			id, err := s.Append(t.Context(), "", messages, "m")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Append(t.Context(), "", messages, "m"); !errors.Is(err, ErrTooLarge) {
				t.Fatalf("累计超限未拒绝: %v", err)
			}
			if cleanup == "delete" {
				if err := s.Delete(t.Context(), id); err != nil {
					t.Fatal(err)
				}
			} else {
				clock.Advance(limits.TTL + time.Second)
				s.GC()
			}
			if _, err := s.Append(t.Context(), "", messages, "m"); err != nil {
				t.Fatalf("清理后额度未归还: %v", err)
			}
		})
	}
}
