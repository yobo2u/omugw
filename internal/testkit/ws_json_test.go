package testkit

import (
	"encoding/json"
	"strings"
	"testing"
)

// 深度边界必须在容器开头拦截，不能依赖解码后才计算深度。
func TestWSJSONDepthBoundary(t *testing.T) {
	for _, tt := range []struct {
		raw   string
		limit int
		valid bool
	}{
		{`0`, 1, true}, {`false`, 1, true}, {`null`, 1, true},
		{strings.Repeat(`[`, 64) + `0` + strings.Repeat(`]`, 64), 64, true},
		{strings.Repeat(`[`, 65) + `0` + strings.Repeat(`]`, 65), 64, false},
		{`{"a":{"b":0}}`, 1, false}, {`{"a":{"b":0}}`, 2, true},
		{`{"a":0,"\u0061":false}`, 64, false},
		{`{"a":0} {}`, 64, false}, {`[`, 64, false},
	} {
		if _, err := strictWSJSON([]byte(tt.raw), tt.limit); (err == nil) != tt.valid {
			t.Fatalf("depth=%d valid=%v error=%v", tt.limit, tt.valid, err)
		}
	}
}

// pointer 只能精确定位，false/0/null 与不存在必须可区分。
func TestWSJSONPointer(t *testing.T) {
	value, err := strictWSJSON([]byte(`{"a/b":{"~key":[false,0,null]}}`), 64)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		pointer string
		want    any
		exists  bool
	}{
		{"/a~1b/~0key/0", false, true}, {"/a~1b/~0key/1", json.Number("0"), true}, {"/a~1b/~0key/2", nil, true},
		{"/a~1b/~0key/3", nil, false}, {"/a~1b/~0key/-1", nil, false}, {"/a~1b/~0key/01", nil, false},
		{"/a~1b/~0key/+1", nil, false}, {"/a~2b", nil, false}, {"/a~", nil, false}, {"a", nil, false},
	} {
		got, exists := wsJSONPointer(value, tt.pointer)
		if exists != tt.exists || got != tt.want {
			t.Fatalf("pointer %s 存在性或值错误", tt.pointer)
		}
	}
}
