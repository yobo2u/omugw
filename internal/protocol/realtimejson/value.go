// Package realtimejson 只共享借用负载的窄扫描，不共享任何协议或计量语义。
package realtimejson

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"unicode/utf8"
)

var errValue = errors.New("realtime json: invalid observation value")

// Value 只借用已通过 Parse 的负载切片，绝不留在 Event 中。
// 未知音频/文本即使占满一帧，也不能经 RawMessage 或 Decoder.Token 复制一遍。
// 调用方不得自行构造或在扫描完成前改写底层字节。
type Value []byte

func Parse(raw []byte) (Value, error) {
	if !json.Valid(raw) {
		return nil, errValue
	}
	return Value(bytes.TrimSpace(raw)), nil
}

func (v Value) Field(name string) (Value, error) {
	if len(v) == 0 || v[0] != '{' {
		return nil, errValue
	}
	var found Value
	for i := skipSpace(v, 1); v[i] != '}'; {
		keyEnd := stringEnd(v, i)
		key := v[i:keyEnd]
		i = skipSpace(v, skipSpace(v, keyEnd)+1)
		end := valueEnd(v, i)
		match := bytes.Equal(key[1:len(key)-1], []byte(name))
		if !match && len(key) <= 6*len(name)+2 && bytes.IndexByte(key, '\\') >= 0 {
			decoded, err := key.Text(len(name))
			match = err == nil && decoded == name
		}
		if match {
			// last-wins 会把两个不同的计费 ID/数值悄悄折成一份可信记录。
			if found != nil {
				return nil, errValue
			}
			found = v[i:end]
		}
		i = skipSpace(v, end)
		if v[i] == ',' {
			i = skipSpace(v, i+1)
		}
	}
	return found, nil
}

func (v Value) Text(limit int) (string, error) {
	// 一个 JSON \uXXXX 最多六个源字节；先限源长度，避免解码后才发现超限。
	if len(v) < 2 || v[0] != '"' || len(v) > 6*limit+2 || !utf8.Valid(v) {
		return "", errValue
	}
	// encoding/json 会把孤立代理项替换成 U+FFFD；ID 不可在这种有损解码后去重。
	for i := 1; i < len(v)-1; i++ {
		if v[i] != '\\' {
			continue
		}
		i++
		if v[i] != 'u' {
			continue
		}
		n, _ := strconv.ParseUint(string(v[i+1:i+5]), 16, 16)
		i += 4
		if n >= 0xdc00 && n <= 0xdfff {
			return "", errValue
		}
		if n >= 0xd800 && n <= 0xdbff {
			if i+6 >= len(v) || v[i+1] != '\\' || v[i+2] != 'u' {
				return "", errValue
			}
			low, _ := strconv.ParseUint(string(v[i+3:i+7]), 16, 16)
			if low < 0xdc00 || low > 0xdfff {
				return "", errValue
			}
			i += 6
		}
	}
	var s string
	if json.Unmarshal(v, &s) != nil || len(s) > limit {
		return "", errValue
	}
	return s, nil
}

func (v Value) Count() (int64, bool) {
	if len(v) == 0 || len(v) > 19 {
		return 0, false
	}
	for _, c := range v {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(string(v), 10, 64)
	return n, err == nil
}

func skipSpace(v []byte, i int) int {
	for i < len(v) && (v[i] == ' ' || v[i] == '\n' || v[i] == '\r' || v[i] == '\t') {
		i++
	}
	return i
}

func stringEnd(v []byte, i int) int {
	for i++; ; i++ {
		switch v[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		}
	}
}

func valueEnd(v []byte, i int) int {
	if v[i] == '"' {
		return stringEnd(v, i)
	}
	if v[i] == '{' || v[i] == '[' {
		// 不递归、不建树；嵌套深度已由 json.Valid 守住。
		depth := 1
		for i++; ; i++ {
			switch v[i] {
			case '"':
				i = stringEnd(v, i) - 1
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1
				}
			}
		}
	}
	for i < len(v) {
		switch v[i] {
		case ',', '}', ']', ' ', '\n', '\r', '\t':
			return i
		}
		i++
	}
	return i
}
