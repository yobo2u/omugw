package config

import (
	"strings"
	"testing"
	"time"
)

// TestDiscoveryDefaultsToDisabled 固化默认关闭。
//
// 发现会拿真实凭据打上游。默认开启等于替部署者做了一个会花钱、会产生外部
// 流量的决定——那必须由他显式承担。
func TestDiscoveryDefaultsToDisabled(t *testing.T) {
	d := Default().Discovery
	if d.Enabled {
		t.Fatal("discovery 必须默认关闭")
	}
	if d.RefreshInterval <= 0 || d.Timeout <= 0 {
		t.Fatalf("关闭状态下也应给出可用默认值，实际 refresh=%v timeout=%v",
			d.RefreshInterval, d.Timeout)
	}
	if d.Timeout >= d.RefreshInterval {
		t.Fatalf("默认值自身必须满足 timeout < refresh_interval，实际 %v >= %v",
			d.Timeout, d.RefreshInterval)
	}
}

// TestDisabledDiscoverySkipsValidation：关闭时一概不校验。
//
// 一份没打算用发现的配置，不该因为里面某个发现字段写得不合理而启动失败。
func TestDisabledDiscoverySkipsValidation(t *testing.T) {
	c := Default()
	c.Discovery = Discovery{Enabled: false, RefreshInterval: -1, Timeout: -1}

	if err := c.Validate(); err != nil {
		t.Fatalf("关闭的 discovery 不应参与校验: %v", err)
	}
}

// TestEnabledDiscoveryIsValidated 覆盖启用后的三条硬约束。
func TestEnabledDiscoveryIsValidated(t *testing.T) {
	for name, tc := range map[string]struct {
		d    Discovery
		want string
	}{
		"refresh_interval 非正数": {
			d:    Discovery{Enabled: true, RefreshInterval: 0, Timeout: time.Second},
			want: "refresh_interval",
		},
		"timeout 非正数": {
			d:    Discovery{Enabled: true, RefreshInterval: time.Minute, Timeout: 0},
			want: "timeout",
		},
		"timeout 等于 refresh_interval": {
			d:    Discovery{Enabled: true, RefreshInterval: time.Minute, Timeout: time.Minute},
			want: "追尾",
		},
		"timeout 大于 refresh_interval": {
			d:    Discovery{Enabled: true, RefreshInterval: time.Second, Timeout: time.Minute},
			want: "追尾",
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := Default()
			c.Discovery = tc.d

			err := c.Validate()
			if err == nil {
				t.Fatal("启用的 discovery 配置不合理时应当拒绝启动")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误应点名 %q，实际: %v", tc.want, err)
			}
		})
	}
}

// TestEnabledDiscoveryAcceptsSaneValues：合理配置要能过。
func TestEnabledDiscoveryAcceptsSaneValues(t *testing.T) {
	c := fullGateway()
	c.Discovery = Discovery{
		Enabled:         true,
		RefreshInterval: 30 * time.Minute,
		Timeout:         10 * time.Second,
	}

	if err := c.Validate(); err != nil {
		t.Fatalf("合理的 discovery 配置应当合法: %v", err)
	}
}

// TestDiscoveryDoesNotJoinTheAllOrNothingRule 固化边界：discovery 是可选增强，
// 不参与 auth/credentials/providers/models 那条「要么全配要么全不配」的判定。
//
// 一个只提供健康检查的网关没有上游可问，但它的配置文件里留着一段启用的
// discovery 并不构成「配了一半」。
func TestDiscoveryDoesNotJoinTheAllOrNothingRule(t *testing.T) {
	c := Default()
	c.Discovery = Discovery{
		Enabled:         true,
		RefreshInterval: time.Minute,
		Timeout:         time.Second,
	}

	if err := c.Validate(); err != nil {
		t.Fatalf("仅健康检查形态 + 启用 discovery 应当合法: %v", err)
	}
}
