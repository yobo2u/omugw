package realtimejson

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestValueFieldsAndBoundedScalars(t *testing.T) {
	raw := []byte(` {"ignored":[{"x":"\\\"{}"},true,null,1e2],"i\u0064":"r\/\ud83d\ude00","count":9223372036854775807} `)
	v, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	id, err := v.Field("id")
	if err != nil {
		t.Fatal(err)
	}
	s, err := id.Text(6)
	if err != nil || s != "r/😀" {
		t.Fatalf("text=%q err=%v", s, err)
	}
	n, err := v.Field("count")
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := n.Count(); !ok || got != 9223372036854775807 {
		t.Fatalf("count=%d valid=%v", got, ok)
	}
	missing, err := v.Field("missing")
	if missing != nil || err != nil {
		t.Fatalf("missing=%q err=%v", missing, err)
	}
	// 标量归调用方所有，扫描切片仍借用帧，不能偷偷复制整个 JSON。
	raw[bytes.Index(raw, []byte("922337"))] = '8'
	if got, ok := n.Count(); !ok || got != 8223372036854775807 {
		t.Fatal("扫描复制了源切片")
	}
	clear(raw)
	if s != "r/😀" {
		t.Fatal("解码字符串持有帧缓冲")
	}
}

func TestValueRejectsAmbiguity(t *testing.T) {
	for _, raw := range []string{``, `{`, `{"a":[1,]}`, `{} {}`} {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Fatalf("接受非法 JSON %q", raw)
		}
	}
	v, _ := Parse([]byte(`{"id":"a","i\u0064":"b"}`))
	if _, err := v.Field("id"); err == nil {
		t.Fatal("接受重复关联键")
	}
	for _, raw := range []string{`null`, `[]`, `0`, `"x"`} {
		v, _ := Parse([]byte(raw))
		if _, err := v.Field("id"); err == nil {
			t.Fatal("接受非对象字段")
		}
	}
	for _, raw := range []string{`"\ud800"`, `"\udc00"`, `"\ud800\u0061"`, `"abcdefg"`, `null`, `"` + string([]byte{0xff}) + `"`} {
		v, _ := Parse([]byte(raw))
		if _, err := v.Text(6); err == nil {
			t.Fatalf("接受有损或超界字符串 %q", raw)
		}
	}
	for _, raw := range []string{`-1`, `1.0`, `1e2`, `null`, `"1"`, `9223372036854775808`} {
		v, _ := Parse([]byte(raw))
		if _, ok := v.Count(); ok {
			t.Fatalf("接受非法计数 %q", raw)
		}
	}
	v, _ = Parse([]byte(`0`))
	if n, ok := v.Count(); !ok || n != 0 {
		t.Fatal("零值丢失")
	}
}

func TestValueLargeUnknownDoesNotAllocatePayload(t *testing.T) {
	raw := []byte(`{"unknown":"` + strings.Repeat("A", 4<<20) + `","id":"small"}`)
	r := testing.Benchmark(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			v, err := Parse(raw)
			if err != nil {
				b.Fatal(err)
			}
			id, err := v.Field("id")
			if err != nil {
				b.Fatal(err)
			}
			if s, err := id.Text(512); err != nil || s != "small" {
				b.Fatal(s, err)
			}
		}
	})
	if r.AllocedBytesPerOp() > 32<<10 {
		t.Fatalf("复制了大字段: %d B/op", r.AllocedBytesPerOp())
	}
	t.Logf("4 MiB unknown: %d bytes/op, %d allocs/op", r.AllocedBytesPerOp(), r.AllocsPerOp())
}

func FuzzValueReadOnly(f *testing.F) {
	for _, s := range []string{`{"id":"x","count":0}`, `{"i\u0064":"\ud800","id":"b"}`, `{"x":[{},true,null],"id":"\ud83d\ude00"}`, `[]`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 64<<10 {
			t.Skip()
		}
		before := bytes.Clone(raw)
		v, err := Parse(raw)
		if (err == nil) != json.Valid(raw) {
			t.Fatal("JSON 合法性不符")
		}
		if err == nil {
			for _, name := range []string{"id", "count", "type"} {
				field, err := v.Field(name)
				if err == nil {
					if s, err := field.Text(512); err == nil && len(s) > 512 {
						t.Fatal("字符串超界")
					}
					if n, ok := field.Count(); ok && n < 0 {
						t.Fatal("负计数")
					}
				}
			}
		}
		if !bytes.Equal(before, raw) {
			t.Fatal("改写了输入")
		}
	})
}
