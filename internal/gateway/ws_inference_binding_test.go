package gateway

import (
	"testing"

	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/router"
)

func TestInferenceBinding(t *testing.T) {
	for _, mutate := range []func(*router.Target){func(x *router.Target) { x.Kind = degrade.ProviderOpenAICompat }, func(x *router.Target) { x.Endpoint = "" }, func(x *router.Target) { x.BaseURL = "" }, func(x *router.Target) { x.CredentialPool = "" }, func(x *router.Target) { x.UpstreamModel = "alias" }, func(x *router.Target) { x.NativeEndpoint = "text-generation" }} {
		x := inferenceTarget()
		mutate(&x)
		if _, err := newWSInferenceBinding(x); err == nil {
			t.Fatal("非法握手绑定准入")
		}
	}
	b, err := newWSInferenceBinding(inferenceTarget())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, match, model string
		mutate             func(*router.Target)
		full               bool
		want               bool
	}{
		{"相同真实model", inferenceASR, inferenceASR, nil, true, true},
		{"endpoint", inferenceASR, inferenceASR, func(x *router.Target) { x.Endpoint = "other" }, true, false},
		{"URL", inferenceASR, inferenceASR, func(x *router.Target) { x.BaseURL += "/other" }, true, false},
		{"pool", inferenceASR, inferenceASR, func(x *router.Target) { x.CredentialPool = "other" }, true, false},
		{"kind", inferenceASR, inferenceASR, func(x *router.Target) { x.Kind = degrade.ProviderOpenAICompat }, true, false},
		{"native", inferenceASR, inferenceASR, func(x *router.Target) { x.NativeEndpoint = "text-generation" }, true, false},
		{"wildcard别名", "*", "client-alias", nil, true, false},
		{"同名通配可匹配", "*", inferenceASR, nil, true, true},
		{"未整门兑现", inferenceASR, inferenceASR, nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := inferenceTarget()
			x.UpstreamModel = inferenceASR
			if tc.mutate != nil {
				tc.mutate(&x)
			}
			rt, err := router.New([]router.Rule{{Match: tc.match, Targets: []router.Target{x}}})
			if err != nil {
				t.Fatal(err)
			}
			err = validateWSInferenceModel(rt, inferenceMatrix(t, tc.full), b, tc.model)
			if (err == nil) != tc.want {
				t.Fatalf("err=%v want=%v", err, tc.want)
			}
		})
	}
}
