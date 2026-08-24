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
	for _, want := range []string{"Kind", "New", "InboundProtocol", "ValidBody", "RateLimitEnvelope"} {
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
		InboundProtocol:   "test.protocol",
		DefaultPath:       "/v1/x",
		ValidBody:         `{"model":"m"}`,
		RateLimitEnvelope: `{"error":{}}`,
	}
	if err := validate(s); err != nil {
		t.Errorf("完整 Subject 不应报错: %v", err)
	}
}

// TestRunFailsOnIncompleteSubject 保证必填项缺失时是硬失败，不是静默跳过。
func TestRunFailsOnIncompleteSubject(t *testing.T) {
	fake := &testing.T{}
	// 用一个子测试承接 Fatal，避免打断本测试。
	done := make(chan bool)
	go func() {
		defer func() { done <- true }()
		defer func() { _ = recover() }()
		Run(fake, Subject{Name: "incomplete"})
	}()
	<-done

	if !fake.Failed() {
		t.Error("零值 Subject 应当让 Run 失败")
	}
}
