package testkit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"unicode/utf8"
)

// strictWSJSON 在结构解码前保留键存在性，防止重复键与深层对象被规范化吞掉。
func strictWSJSON(raw []byte, depthLimit int) (any, error) {
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("WS JSON 必须为 UTF8")
	}
	if err := checkWSJSONSurrogates(raw); err != nil {
		return nil, err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	value, err := readWSJSONValue(d, 0, depthLimit)
	if err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, fmt.Errorf("WS JSON 含尾随内容")
	}
	return value, nil
}

// Go 解码器会将未配对 surrogate 替换为 U+FFFD；必须先检查原始 escape，防止文本或 ID 身份碰撞。
// 此处只守 Unicode 配对，完整 JSON 语法仍由原解码器裁决，不复制第二套解码逻辑。
func checkWSJSONSurrogates(raw []byte) error {
	inString := false
	for i := 0; i < len(raw); i++ {
		if raw[i] == '"' {
			inString = !inString
			continue
		}
		if !inString || raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) || raw[i] != 'u' {
			// 跳过整个普通 escape，防止字面 \\ud800 或转义引号被误作 Unicode escape。
			continue
		}
		code, ok := wsJSONHex4(raw[i+1:])
		if !ok {
			return fmt.Errorf("WS JSON Unicode escape 不合法")
		}
		i += 4
		if code >= 0xdc00 && code <= 0xdfff {
			return fmt.Errorf("WS JSON 含未配对 surrogate")
		}
		if code < 0xd800 || code > 0xdbff {
			continue
		}
		if len(raw)-i-1 < 6 || raw[i+1] != '\\' || raw[i+2] != 'u' {
			return fmt.Errorf("WS JSON 含未配对 surrogate")
		}
		low, ok := wsJSONHex4(raw[i+3:])
		if !ok || low < 0xdc00 || low > 0xdfff {
			return fmt.Errorf("WS JSON 含未配对 surrogate")
		}
		i += 6
	}
	return nil
}

func wsJSONHex4(raw []byte) (uint16, bool) {
	if len(raw) < 4 {
		return 0, false
	}
	var code uint16
	for _, digit := range raw[:4] {
		code <<= 4
		switch {
		case digit >= '0' && digit <= '9':
			code |= uint16(digit - '0')
		case digit >= 'a' && digit <= 'f':
			code |= uint16(digit-'a') + 10
		case digit >= 'A' && digit <= 'F':
			code |= uint16(digit-'A') + 10
		default:
			return 0, false
		}
	}
	return code, true
}

func readWSJSONValue(d *json.Decoder, depth, limit int) (any, error) {
	token, err := d.Token()
	if err != nil {
		return nil, fmt.Errorf("WS JSON 语法不合法")
	}
	delim, container := token.(json.Delim)
	if !container {
		return token, nil
	}
	if depth >= limit {
		return nil, fmt.Errorf("WS JSON 超过深度预算")
	}
	switch delim {
	case '{':
		object := make(map[string]any)
		for d.More() {
			keyToken, err := d.Token()
			if err != nil {
				return nil, fmt.Errorf("WS JSON 对象键不合法")
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, fmt.Errorf("WS JSON 对象键不合法")
			}
			if _, exists := object[key]; exists {
				return nil, fmt.Errorf("WS JSON 含重复键")
			}
			value, err := readWSJSONValue(d, depth+1, limit)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		if end, err := d.Token(); err != nil || end != json.Delim('}') {
			return nil, fmt.Errorf("WS JSON 对象未闭合")
		}
		return object, nil
	case '[':
		array := make([]any, 0)
		for d.More() {
			value, err := readWSJSONValue(d, depth+1, limit)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		if end, err := d.Token(); err != nil || end != json.Delim(']') {
			return nil, fmt.Errorf("WS JSON 数组未闭合")
		}
		return array, nil
	default:
		return nil, fmt.Errorf("WS JSON 分隔符不合法")
	}
}

// legacyHasWS 只识别传输标签，不加强旧 HTTP 的未知键/重复键规则。
// 逐 token 检查能看见被后续 ws:null 掩盖的 WS 分支，避免从旧 Loader 绕过专用校验。
func legacyHasWS(raw []byte) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var contexts []string
	var expectKey []bool
	pending := ""
	for {
		token, err := d.Token()
		if err != nil {
			return false
		}
		if delim, ok := token.(json.Delim); ok {
			switch delim {
			case '{', '[':
				context := "other"
				if len(contexts) == 0 {
					context = "root"
				} else if contexts[len(contexts)-1] == "root" && strings.EqualFold(pending, "response") {
					context = "response"
				}
				contexts = append(contexts, context)
				expectKey = append(expectKey, delim == '{')
				pending = ""
			case '}', ']':
				contexts = contexts[:len(contexts)-1]
				expectKey = expectKey[:len(expectKey)-1]
				if len(expectKey) > 0 {
					expectKey[len(expectKey)-1] = true
				}
			}
			continue
		}
		if len(contexts) == 0 {
			continue
		}
		i := len(contexts) - 1
		if expectKey[i] {
			if key, ok := token.(string); ok {
				if contexts[i] == "response" && strings.EqualFold(key, "ws") {
					return true
				}
				pending = key
			}
			expectKey[i] = false
		} else {
			expectKey[i] = true
			pending = ""
		}
	}
}

// readFixtureBounded 在任何结构解码前限制字节数，防止旧入口先吞下无界 WS 文件。
func readFixtureBounded(path string, limit int64) ([]byte, error) {
	if limit <= 0 || limit == int64(^uint64(0)>>1) {
		return nil, fmt.Errorf("文件预算不合法")
	}
	file, err := openFixtureRegular(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("fixture 超过文件字节预算")
	}
	return raw, nil
}

// schema 键名只认精确标签，防止 encoding/json 大小写别名绕过重复键检查。
func checkWSSchema(value any, typ reflect.Type) error {
	if typ == reflect.TypeFor[json.RawMessage]() {
		return nil
	}
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	switch typ.Kind() {
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		fields := make(map[string]reflect.Type)
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			key := strings.Split(field.Tag.Get("json"), ",")[0]
			fields[key] = field.Type
		}
		// 空串是 RFC 6901 的根 pointer；必须保留“显式根”与“漏填/null”的区别。
		var pointerFields []string
		switch typ {
		case reflect.TypeFor[WSFieldRule]():
			pointerFields = []string{"pointer"}
		case reflect.TypeFor[WSTerminal]():
			pointerFields = []string{"id_pointer", "state_pointer"}
		}
		for _, key := range pointerFields {
			if _, ok := object[key].(string); !ok {
				return fmt.Errorf("WS pointer 字段必须显式为字符串")
			}
		}
		for key, child := range object {
			field, ok := fields[key]
			if !ok {
				return fmt.Errorf("WS schema 含未知字段")
			}
			if err := checkWSSchema(child, field); err != nil {
				return err
			}
		}
	case reflect.Slice:
		if array, ok := value.([]any); ok {
			for _, child := range array {
				if err := checkWSSchema(child, typ.Elem()); err != nil {
					return err
				}
			}
		}
	case reflect.Map:
		if object, ok := value.(map[string]any); ok {
			for _, child := range object {
				if err := checkWSSchema(child, typ.Elem()); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func decodeWSFixture(raw []byte, limits WSLimits) (Fixture, error) {
	value, err := strictWSJSON(raw, limits.JSONDepth)
	if err != nil {
		return Fixture{}, err
	}
	if err := checkWSSchema(value, reflect.TypeFor[Fixture]()); err != nil {
		return Fixture{}, err
	}
	var f Fixture
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&f); err != nil {
		return Fixture{}, fmt.Errorf("WS 文件结构解码失败")
	}
	if err := ValidateWSSession(f, limits); err != nil {
		return Fixture{}, err
	}
	return f, nil
}

// ReadWSFixture 是 WS 专用入口，调用方必须显式传入预算。
func ReadWSFixture(path string, limits WSLimits) (Fixture, error) {
	if err := limits.validate(); err != nil {
		return Fixture{}, err
	}
	raw, err := readFixtureBounded(path, limits.FileBytes)
	if err != nil {
		return Fixture{}, err
	}
	return decodeWSFixture(raw, limits)
}

// ReadWSFixtureDir 按名称读取，目录总预算包含解码失败前已读取的文件字节。
func ReadWSFixtureDir(path string, limits WSLimits) ([]Fixture, error) {
	if err := limits.validate(); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("WS fixture 目录不能为符号链接")
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	var fixtures []Fixture
	remaining := limits.DirectoryBytes
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("WS fixture 目录含符号链接")
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if remaining == 0 {
			return nil, fmt.Errorf("WS fixture 超过目录预算")
		}
		limit := limits.FileBytes
		if remaining < limit {
			limit = remaining
		}
		raw, err := readFixtureBounded(filepath.Join(path, entry.Name()), limit)
		if err != nil {
			return nil, err
		}
		remaining -= int64(len(raw))
		f, err := decodeWSFixture(raw, limits)
		if err != nil {
			return nil, err
		}
		fixtures = append(fixtures, f)
	}
	if len(fixtures) == 0 {
		return nil, fmt.Errorf("目录中没有 WS fixture")
	}
	return fixtures, nil
}
