package testkit

import (
	"context"
	"encoding/json"
	"testing"
)

// v1 不消费 close 来源声明，必须静态拒绝；相同码/reason 也不能让空标签获得批准。
func TestWSCloseForwardedFromRejectedAtBothEntrances(t *testing.T) {
	for _, side := range []WSPoint{WSClientSend, WSUpstreamSend} {
		for _, sameReason := range []bool{true, false} {
			name := string(side) + "/same"
			if !sameReason {
				name = string(side) + "/different"
			}
			t.Run(name, func(t *testing.T) {
				f := wsReplayFixture(side, "completed", 1000)
				f.Response.WS.Nodes[5].ForwardedFrom = "close-send"
				if !sameReason {
					f.Response.WS.Nodes[5].CloseReason = "synthetic-private"
				}
				raw, err := json.Marshal(f)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := ReadWSFixture(writeWSInput(t, t.TempDir(), "fixture.json", raw), DefaultWSLimits()); err == nil {
					t.Error("loader 静默接纳 close 来源")
				}
				if err := f.Validate(); err == nil {
					t.Error("Validate 静默接纳 close 来源")
				}
			})
		}
	}
}

// 未声明来源的关闭码/reason 各自独立断言；v1 不要求桥自动保全所有关闭原因。
func TestWSCloseIndependentReasonsRemainExecutable(t *testing.T) {
	for _, side := range []WSPoint{WSClientSend, WSUpstreamSend} {
		t.Run(string(side), func(t *testing.T) {
			f := wsReplayFixture(side, "completed", 1000)
			f.Response.WS.Nodes[4].CloseReason = "authored-reason"
			f.Response.WS.Nodes[5].CloseReason = "synthetic-private"
			raw, err := json.Marshal(f)
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := ReadWSFixture(writeWSInput(t, t.TempDir(), "fixture.json", raw), DefaultWSLimits())
			if err != nil {
				t.Fatal(err)
			}
			if err := loaded.Validate(); err != nil {
				t.Fatal(err)
			}
			peers, owner := wsReplayBridgeMode(t, loaded, DefaultWSLimits(), nil, "wrong_reason")
			result, err := ReplayWS(context.Background(), loaded, peers, 1, DefaultWSLimits())
			result, err = finishWSIntegration(result, err, owner)
			if err != nil || result.Outcome.Kind != "completed" {
				t.Fatalf("独立关闭原因被误拒: %v", err)
			}
		})
	}
}
