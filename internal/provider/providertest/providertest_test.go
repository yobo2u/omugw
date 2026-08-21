package providertest

import (
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/provider"
)

// TestValidateReportsMissingFields 固化「必填项缺失即失败」。
//
// 不静默跳过对应的不变量族——跳过等于把「没跑」伪装成「跑过了」，
// 而这个套件存在的全部理由就是消除那种隐形缺口。
func TestValidateReportsMissingFields(t *testing.T) {
	err := validate(Subject{Name: "empty"})
	if err == nil {
		t.Fatal("零值 Subject 应当报错")
	}
	for _, want := range []string{"Kind", "New", "ValidBody", "RateLimitEnvelope"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息未指出缺失项 %q: %v", want, err)
		}
	}
}

// TestValidateAcceptsCompleteSubject 保证填全后不再报错。
func TestValidateAcceptsCompleteSubject(t *testing.T) {
	s := Subject{
		Name:              "complete",
		Kind:              "test.provider",
		New:               func(*testing.T, Deps) provider.Provider { return nil },
		DefaultPath:       "/v1/x",
		ValidBody:         `{"model":"m"}`,
		RateLimitEnvelope: `{"error":{}}`,
	}
	if err := validate(s); err != nil {
		t.Errorf("完整 Subject 不应报错: %v", err)
	}
}
