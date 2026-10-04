package dashscopeinference

import (
	"errors"
	"unicode/utf8"

	"github.com/yobo2u/omugw/internal/canonical"
)

type LocalFailureKind uint8

const (
	LocalPolicy LocalFailureKind = iota
	LocalUnsupportedTask
	LocalStartTimeout
	LocalDrainTimeout
)

var errFailure = errors.New("dashscope inference: invalid local failure encoding")

const (
	failurePrefix  = `{"header":{"event":"task-failed","task_id":`
	failureCode    = `,"error_code":"`
	failureMessage = `","error_message":"`
	failureSuffix  = `"},"payload":{}}`
)

func localFailure(kind LocalFailureKind) (code, message string, ok bool) {
	switch kind {
	case LocalPolicy:
		return "Gateway.InvalidTask", "invalid task envelope or binding", true
	case LocalUnsupportedTask:
		return "Gateway.UnsupportedTask", "task contract not implemented", true
	case LocalStartTimeout:
		return "Gateway.TaskStartTimeout", "task did not start within gateway budget", true
	case LocalDrainTimeout:
		return "Gateway.TaskDrainTimeout", "task did not finish within gateway budget", true
	default:
		return "", "", false
	}
}

func validTaskID(id string) bool { return id != "" && len(id) <= maxIDBytes && utf8.ValidString(id) }

// TaskFailureSize 先计算转义后的精确额度，不能为计额先生成一份完整编码负载。
func TaskFailureSize(id string, kind LocalFailureKind) (int, error) {
	code, message, ok := localFailure(kind)
	if !ok || !validTaskID(id) {
		return 0, errFailure
	}
	n := len(failurePrefix) + 2 + len(id) + len(failureCode) + len(code) + len(failureMessage) + len(message) + len(failureSuffix)
	for i := 0; i < len(id); i++ {
		switch id[i] {
		case '"', '\\', '\b', '\f', '\n', '\r', '\t':
			n++
		default:
			if id[i] < 0x20 {
				n += 5
			}
		}
	}
	if n > 4096 {
		return 0, errFailure
	}
	return n, nil
}

// PutTaskFailure 只写调用方已计额的精确切片；任何参数错误都先于首字节写入。
func PutTaskFailure(dst []byte, id string, kind LocalFailureKind) error {
	n, err := TaskFailureSize(id, kind)
	if err != nil || len(dst) != n {
		return errFailure
	}
	code, message, _ := localFailure(kind)
	i := copy(dst, failurePrefix)
	dst[i] = '"'
	i++
	const hex = "0123456789abcdef"
	for j := 0; j < len(id); j++ {
		c := id[j]
		var escaped byte
		switch c {
		case '"', '\\':
			escaped = c
		case '\b':
			escaped = 'b'
		case '\f':
			escaped = 'f'
		case '\n':
			escaped = 'n'
		case '\r':
			escaped = 'r'
		case '\t':
			escaped = 't'
		}
		switch {
		case escaped != 0:
			dst[i], dst[i+1] = '\\', escaped
			i += 2
		case c < 0x20:
			i += copy(dst[i:], `\u00`)
			dst[i], dst[i+1] = hex[c>>4], hex[c&0xf]
			i += 2
		default:
			dst[i] = c
			i++
		}
	}
	dst[i] = '"'
	i++
	i += copy(dst[i:], failureCode)
	i += copy(dst[i:], code)
	i += copy(dst[i:], failureMessage)
	i += copy(dst[i:], message)
	copy(dst[i:], failureSuffix)
	return nil
}

// ClassifyClose 不借 Realtime 的 reason 推断鉴权或额度，也不把未知异常关闭记成功。
func ClassifyClose(code uint16, _ string) *canonical.Error {
	if code == 1000 || code == 1001 {
		return nil
	}
	return &canonical.Error{Class: canonical.ClassInternal, Message: "unclassified DashScope Inference connection close"}
}
