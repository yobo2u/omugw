package docs_test

import (
	"errors"
	"os"
	"testing"
)

// runCases 把 YAML 片段解码后送进 validate，断言命中的是指定哨兵错误。
// 用 errors.Is 而不是比对错误文案，措辞改动不该让测试变红。
func runCases(t *testing.T, cases caseSet) {
	t.Helper()
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p, err := decodeProvenance([]byte(tc.body))
			if err != nil {
				t.Fatalf("片段应能解码: %v", err)
			}
			if err := validate(p); !errors.Is(err, tc.want) {
				t.Errorf("validate 错误 = %v, 期望 %v", err, tc.want)
			}
		})
	}
}

type caseSet = map[string]struct {
	body string
	want error
}

// TestRealProvenanceIsValid 防的是登记表本身腐烂：字段拼错、缩进写歪、
// 复制粘贴出重复条目——这些在纯注释型 YAML 里没人会发现，直到某天需要
// 靠它回答「这段代码是抄的还是自己写的」。
func TestRealProvenanceIsValid(t *testing.T) {
	raw, err := os.ReadFile("provenance.yaml")
	if err != nil {
		t.Fatal(err)
	}

	p, err := decodeProvenance(raw)
	if err != nil {
		t.Fatalf("真实登记表应能严格解码: %v", err)
	}
	if err := validate(p); err != nil {
		t.Fatalf("真实登记表应通过校验: %v", err)
	}

	// 三张表都非空才算真的校验过：空列表会让下面所有规则空转，
	// 于是整份测试在登记表被清空时依然全绿——那是最坏的一种绿。
	if len(p.Modules) == 0 {
		t.Error("modules 为空，校验将空转")
	}
	if len(p.PlannedUpstreams) == 0 {
		t.Error("planned_upstreams 为空，校验将空转")
	}
	if len(p.Excluded) == 0 {
		t.Error("excluded_from_source_reading 为空，校验将空转")
	}
}

// TestDecodeRejectsUnknownField 防的是字段名拼错后静默变成注释：
// 非严格解码下 `implementation_typo:` 会被丢弃，模块看起来仍然「有登记」，
// 实际上来源信息已经丢了。
func TestDecodeRejectsUnknownField(t *testing.T) {
	const body = "version: 1\n" +
		"modules:\n" +
		"  - path: internal/x\n" +
		"    implementation_typo: original\n"

	if _, err := decodeProvenance([]byte(body)); err == nil {
		t.Fatal("未知字段必须让解码失败")
	}
}

// TestValidateRejectsBadHeader 防的是整份登记表被读成空壳：version 缺失
// （零值）或改成未来格式时，旧校验规则不再适用；modules 为空则说明表被清空，
// 而不是「这个项目没有模块」。
func TestValidateRejectsBadHeader(t *testing.T) {
	const oneModule = "modules:\n  - path: internal/x\n    implementation_type: original\n"

	runCases(t, caseSet{
		"version 缺失":  {body: oneModule, want: errVersion},
		"version 非 1": {body: "version: 2\n" + oneModule, want: errVersion},
		"modules 为空":  {body: "version: 1\n", want: errNoModules},
	})
}

// TestValidateRejectsBadModule 防的是模块条目失去追溯价值：没有 path 的条目
// 指不到任何代码，重复 path 会让两份互相矛盾的声明并存，implementation_type
// 写错则整条来源声明失去法律意义——derived 要留 copyright，original 不用。
func TestValidateRejectsBadModule(t *testing.T) {
	runCases(t, caseSet{
		"path 为空": {
			body: "version: 1\nmodules:\n  - implementation_type: original\n",
			want: errModulePath,
		},
		"path 重复": {
			body: "version: 1\nmodules:\n" +
				"  - path: internal/x\n    implementation_type: original\n" +
				"  - path: internal/x\n    implementation_type: derived\n",
			want: errDuplicateModule,
		},
		"implementation_type 为空": {
			body: "version: 1\nmodules:\n  - path: internal/x\n",
			want: errImplType,
		},
		"implementation_type 非法": {
			body: "version: 1\nmodules:\n  - path: internal/x\n    implementation_type: copied\n",
			want: errImplType,
		},
	})
}

// TestValidateRejectsBadReference 防的是参考来源写了一半：只有 name 没有 url
// 的条目，等于声称「参考过什么」却无法复核。注意本规则不要求模块必须有
// reference——original 模块本就不该有。
func TestValidateRejectsBadReference(t *testing.T) {
	const head = "version: 1\nmodules:\n  - path: internal/x\n" +
		"    implementation_type: clean-room\n    reference:\n"

	runCases(t, caseSet{
		"reference name 为空": {
			body: head + "      - kind: public-documentation\n        url: https://e.example\n",
			want: errReferenceField,
		},
		"reference kind 为空": {
			body: head + "      - name: Spec\n        url: https://e.example\n",
			want: errReferenceField,
		},
		"reference url 为空": {
			body: head + "      - name: Spec\n        kind: public-documentation\n",
			want: errReferenceField,
		},
	})
}

// TestValidateAcceptsAllowedImplTypesWithoutReference 同时钉两件事。
//
// 其一是白名单的完整性：original / derived / clean-room 三种取值都必须被放行。
// 真实登记表当前只有 original 与 clean-room，若不逐个正例覆盖，某天有人从
// 白名单里删掉 derived，整份测试依然全绿，而所有「基于兼容许可证上游修改而来」
// 的模块会在登记时被莫名拒绝。
//
// 其二是上一条规则的边界：缺 reference 与 reference 写残是两回事，前者必须
// 放行，否则会逼人给自研代码编造参考来源。
func TestValidateAcceptsAllowedImplTypesWithoutReference(t *testing.T) {
	for _, implType := range []string{"original", "derived", "clean-room"} {
		t.Run(implType, func(t *testing.T) {
			body := "version: 1\nmodules:\n  - path: internal/x\n    implementation_type: " + implType + "\n"

			p, err := decodeProvenance([]byte(body))
			if err != nil {
				t.Fatal(err)
			}
			if err := validate(p); err != nil {
				t.Errorf("无 reference 的 %s 模块应合法: %v", implType, err)
			}
		})
	}
}

// TestValidateRejectsBadUpstream 防的是计划接入的上游漏掉许可证信息：
// 缺 license 或 planned_usage 的条目，会让「这个上游能不能进 Apache-2.0 Core」
// 这个硬门槛无从判断；modules 为空则说明没人写明打算抄哪一块。
func TestValidateRejectsBadUpstream(t *testing.T) {
	const head = "version: 1\nmodules:\n  - path: internal/x\n" +
		"    implementation_type: original\nplanned_upstreams:\n"
	const full = "  - name: A\n    repository: o/a\n    license: MIT\n" +
		"    planned_usage: reference\n    modules: [x]\n"

	runCases(t, caseSet{
		"name 为空": {
			body: head + "  - repository: o/a\n    license: MIT\n" +
				"    planned_usage: reference\n    modules: [x]\n",
			want: errUpstreamField,
		},
		"repository 为空": {
			body: head + "  - name: A\n    license: MIT\n" +
				"    planned_usage: reference\n    modules: [x]\n",
			want: errUpstreamField,
		},
		"license 为空": {
			body: head + "  - name: A\n    repository: o/a\n" +
				"    planned_usage: reference\n    modules: [x]\n",
			want: errUpstreamField,
		},
		"planned_usage 为空": {
			body: head + "  - name: A\n    repository: o/a\n    license: MIT\n    modules: [x]\n",
			want: errUpstreamField,
		},
		"name 重复": {
			body: head + full + full,
			want: errDuplicateUpstream,
		},
		"modules 为空": {
			body: head + "  - name: A\n    repository: o/a\n    license: MIT\n" +
				"    planned_usage: reference\n",
			want: errUpstreamModules,
		},
		"modules 含空串": {
			body: head + "  - name: A\n    repository: o/a\n    license: MIT\n" +
				"    planned_usage: reference\n    modules: [\"\"]\n",
			want: errUpstreamModules,
		},
	})
}

// TestValidateRejectsBadExclusion 防的是「为什么不读这个仓库的源码」这条
// 记录退化成一个名字：没有 license 与 reason，半年后没人记得是许可证不兼容
// 还是单纯没需要，于是有人去读了本不该读的源码。
func TestValidateRejectsBadExclusion(t *testing.T) {
	const head = "version: 1\nmodules:\n  - path: internal/x\n" +
		"    implementation_type: original\nexcluded_from_source_reading:\n"
	const full = "  - name: A\n    repository: o/a\n    license: AGPL-3.0\n    reason: 不兼容\n"

	runCases(t, caseSet{
		"name 为空": {
			body: head + "  - repository: o/a\n    license: AGPL-3.0\n    reason: 不兼容\n",
			want: errExcludedField,
		},
		"repository 为空": {
			body: head + "  - name: A\n    license: AGPL-3.0\n    reason: 不兼容\n",
			want: errExcludedField,
		},
		"license 为空": {
			body: head + "  - name: A\n    repository: o/a\n    reason: 不兼容\n",
			want: errExcludedField,
		},
		"reason 为空": {
			body: head + "  - name: A\n    repository: o/a\n    license: AGPL-3.0\n",
			want: errExcludedField,
		},
		"name 重复": {
			body: head + full + full,
			want: errDuplicateExcluded,
		},
	})
}
