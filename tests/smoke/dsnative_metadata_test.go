//go:build smoke

package smoke_test

import (
	"encoding/json"
	"slices"
	"testing"
)

// TestRecordCaseMetadataIsComplete 校验 12 个用例的元数据自洽：名字唯一、
// 模型角色解析得出非空模型、门有对应上游路径、请求体构造器存在且产出的
// JSON 合法且与 stream 标志一致。
//
// 这些都是录制器接线前就该成立的事实。一个解析成空串的模型角色、或一份
// 语法就不合法的请求体，要等到真实调用时才失败——而那时账已经付过了。
func TestRecordCaseMetadataIsComplete(t *testing.T) {
	if len(recordCases) != 12 {
		t.Fatalf("用例数 = %d，期望恰好 12——audio_input 退出本期举证（无真实 fixture），"+
			"其余格子一项都不许少", len(recordCases))
	}
	for _, c := range recordCases {
		if c.name == "audio_input" {
			t.Fatal("audio_input 不得出现在本期 recordCases——" +
				"qwen-audio-turbo 额度耗尽，无证据不兑现（ADR-0001）")
		}
	}

	seen := make(map[string]bool, len(recordCases))
	for i, c := range recordCases {
		if c.name == "" {
			t.Fatalf("recordCases[%d] 没有名字", i)
		}
		if seen[c.name] {
			t.Errorf("用例名 %q 重复——名字是用例的唯一标识，重名会让证据互相顶掉", c.name)
		}
		seen[c.name] = true
	}

	for _, c := range recordCases {
		t.Run(c.name, func(t *testing.T) {
			if got := modelForRole(c.modelRole); got == "" {
				t.Fatalf("模型角色 %q 解析为空字符串", string(c.modelRole))
			}
			if c.door.Path() == "" {
				t.Fatalf("门 %q 没有对应的上游路径", string(c.door))
			}
			if c.body == nil {
				t.Fatal("缺少请求体构造器")
			}

			raw := c.body(preflightClientModel)
			if !json.Valid(raw) {
				t.Fatalf("请求体不是合法 JSON: %s", raw)
			}

			var shape struct {
				Model  string `json:"model"`
				Stream *bool  `json:"stream"`
			}
			if err := json.Unmarshal(raw, &shape); err != nil {
				t.Fatalf("请求体解不出顶层形态: %v", err)
			}
			if shape.Model != preflightClientModel {
				t.Errorf("请求体 model = %q，期望构造器把入参原样写进去（%q）",
					shape.Model, preflightClientModel)
			}
			// stream 与元数据必须一致：录制器按元数据选流式断言，而真正决定
			// 上游走不走 SSE 的是请求体里的这个字段。两边漂移时录制会拿一份
			// 非流式响应去当流式证据存盘。
			gotStream := shape.Stream != nil && *shape.Stream
			if gotStream != c.stream {
				t.Errorf("请求体 stream = %v，期望与元数据一致（%v）", gotStream, c.stream)
			}
		})
	}
}

// TestExpectedDegradedCapsCoversEveryDegradedCase 把降级能力名单与用例元数据
// 双向对账。
//
// 名单漏一格，assertDegradationHeader 就会在那个用例上以「声明了降级却没登记
// 能力」失败——但那要等到花钱录制时才发生。多一格同样有害：它给一个本不降级的
// 用例定下了一份永远不会被检查的期望。
func TestExpectedDegradedCapsCoversEveryDegradedCase(t *testing.T) {
	declared := make([]string, 0, len(expectedDegradedCaps))
	for name := range expectedDegradedCaps {
		declared = append(declared, name)
	}

	expected := make([]string, 0, len(expectedDegradedCaps))
	for _, c := range recordCases {
		if c.expectDegraded {
			expected = append(expected, c.name)
		}
	}

	slices.Sort(declared)
	slices.Sort(expected)
	if !slices.Equal(declared, expected) {
		t.Fatalf("登记了降级能力的用例 = %v，期望与 expectDegraded 的用例 %v 一致",
			declared, expected)
	}
	for name, caps := range expectedDegradedCaps {
		if len(caps) == 0 {
			t.Errorf("用例 %q 登记了空的降级能力集，等于什么都不检查", name)
		}
	}
}
