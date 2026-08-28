package docs_test

import (
	"bytes"
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"
)

type provenance struct {
	Version          int        `yaml:"version"`
	Modules          []module   `yaml:"modules"`
	PlannedUpstreams []upstream `yaml:"planned_upstreams"`
	Excluded         []excluded `yaml:"excluded_from_source_reading"`
}

type module struct {
	Path               string      `yaml:"path"`
	ImplementationType string      `yaml:"implementation_type"`
	Reference          []reference `yaml:"reference"`
	Note               string      `yaml:"note"`
}

type reference struct {
	Name string `yaml:"name"`
	Kind string `yaml:"kind"`
	URL  string `yaml:"url"`
}

type upstream struct {
	Name         string   `yaml:"name"`
	Repository   string   `yaml:"repository"`
	License      string   `yaml:"license"`
	PlannedUsage string   `yaml:"planned_usage"`
	Modules      []string `yaml:"modules"`
	Note         string   `yaml:"note"`
	Warning      string   `yaml:"warning"`
}

type excluded struct {
	Name       string `yaml:"name"`
	Repository string `yaml:"repository"`
	License    string `yaml:"license"`
	Reason     string `yaml:"reason"`
}

// decodeProvenance 严格解码：KnownFields(true) 把拼错的键变成硬失败，
// 否则一个 `implementation_typo:` 会被静默丢弃，登记表看着完整而实际有洞。
func decodeProvenance(raw []byte) (provenance, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)

	var p provenance
	if err := dec.Decode(&p); err != nil {
		return provenance{}, err
	}
	return p, nil
}

// 哨兵错误让负例断言 errors.Is 而不是比对文案，改措辞不该变红。
var (
	errVersion         = errors.New("version 必须为 1")
	errNoModules       = errors.New("modules 不可为空")
	errModulePath      = errors.New("module path 不可为空")
	errDuplicateModule = errors.New("module path 重复")
	errImplType        = errors.New("implementation_type 必须为 original/derived/clean-room")
	errReferenceField  = errors.New("reference 的 name/kind/url 必须齐全")

	errUpstreamField     = errors.New("planned upstream 的 name/repository/license/planned_usage 必须齐全")
	errDuplicateUpstream = errors.New("planned upstream name 重复")
	errUpstreamModules   = errors.New("planned upstream modules 不可为空且不可含空串")

	errExcludedField     = errors.New("exclusion 的 name/repository/license/reason 必须齐全")
	errDuplicateExcluded = errors.New("exclusion name 重复")
)

var implTypes = map[string]bool{"original": true, "derived": true, "clean-room": true}

func validate(p provenance) error {
	if p.Version != 1 {
		return fmt.Errorf("%w: 实际 %d", errVersion, p.Version)
	}
	if len(p.Modules) == 0 {
		return errNoModules
	}
	if err := validateModules(p.Modules); err != nil {
		return err
	}
	if err := validateUpstreams(p.PlannedUpstreams); err != nil {
		return err
	}
	return validateExcluded(p.Excluded)
}

func validateExcluded(exs []excluded) error {
	seen := make(map[string]bool, len(exs))
	for i, e := range exs {
		if e.Name == "" || e.Repository == "" || e.License == "" || e.Reason == "" {
			return fmt.Errorf("%w: 第 %d 条", errExcludedField, i+1)
		}
		if seen[e.Name] {
			return fmt.Errorf("%w: %s", errDuplicateExcluded, e.Name)
		}
		seen[e.Name] = true
	}
	return nil
}

func validateUpstreams(ups []upstream) error {
	seen := make(map[string]bool, len(ups))
	for i, u := range ups {
		if u.Name == "" || u.Repository == "" || u.License == "" || u.PlannedUsage == "" {
			return fmt.Errorf("%w: 第 %d 条", errUpstreamField, i+1)
		}
		if seen[u.Name] {
			return fmt.Errorf("%w: %s", errDuplicateUpstream, u.Name)
		}
		seen[u.Name] = true

		if len(u.Modules) == 0 {
			return fmt.Errorf("%w: %s", errUpstreamModules, u.Name)
		}
		for _, m := range u.Modules {
			if m == "" {
				return fmt.Errorf("%w: %s", errUpstreamModules, u.Name)
			}
		}
	}
	return nil
}

func validateModules(mods []module) error {
	seen := make(map[string]bool, len(mods))
	for i, m := range mods {
		if m.Path == "" {
			return fmt.Errorf("%w: 第 %d 条", errModulePath, i+1)
		}
		if seen[m.Path] {
			return fmt.Errorf("%w: %s", errDuplicateModule, m.Path)
		}
		seen[m.Path] = true

		if !implTypes[m.ImplementationType] {
			return fmt.Errorf("%w: %s 为 %q", errImplType, m.Path, m.ImplementationType)
		}
		// 只校验已经写出来的 reference。不要求模块必须有 reference：
		// original 模块本就没有可参考的上游。
		for j, r := range m.Reference {
			if r.Name == "" || r.Kind == "" || r.URL == "" {
				return fmt.Errorf("%w: %s 第 %d 条", errReferenceField, m.Path, j+1)
			}
		}
	}
	return nil
}
