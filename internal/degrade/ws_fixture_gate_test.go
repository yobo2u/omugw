package degrade

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/testkit"
)

// 仍消费旧 request.path 门禁；合成 WS 只在临时路径演示机制，不进入真实投放目录。
func TestWSFixtureFitsExistingGate(t *testing.T) {
	f, err := testkit.ReadWSFixture("../../testdata/testkit/ws/valid/hello.json", testkit.DefaultWSLimits())
	if err != nil {
		t.Fatal("合成 WS 样例读取失败")
	}
	build := func(protocol Protocol, lossy bool) (*Route, error) {
		b := NewRoute(protocol, ProviderOpenAICompat)
		for _, cap := range ExpressibleSet(protocol) {
			if lossy && cap == canonical.CapRealtimeSession {
				b.Degrade("仅合成门禁：丢失会话语义", cap)
			} else {
				b.Pass(cap)
			}
		}
		return b.Redeem(EndpointOpenAIRealtime, canonical.CapRealtimeSession).Build()
	}
	r, err := build(ProtoOpenAIRealtime, false)
	if err != nil {
		t.Fatal("合成测试 Route 构造失败")
	}
	for _, tc := range []struct {
		name, path           string
		lossy, named, wantOK bool
	}{
		{"get_path", "/v1/realtime", false, false, true},
		{"query_in_path", "/v1/realtime?model=synthetic-model", false, false, false},
		{"unopened", "/api-ws/v1/realtime", false, false, false},
		{"lossy_without_named_file", "/v1/realtime", true, false, false},
		{"lossy_with_named_file", "/v1/realtime", true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			local := f
			local.Request.Path = tc.path
			name := "hello.json"
			if tc.named {
				name = "realtime_session.json"
			}
			dir := filepath.Join(root, FixtureDir(r.InProtocol(), r.OutProvider()))
			if os.MkdirAll(dir, 0o755) != nil {
				t.Fatal("临时 fixture 目录创建失败")
			}
			raw, err := json.Marshal(local)
			if err != nil || os.WriteFile(filepath.Join(dir, name), raw, 0o644) != nil {
				t.Fatal("临时 fixture 写入失败")
			}
			localRoute := r
			if tc.lossy {
				localRoute, err = build(ProtoOpenAIRealtime, true)
				if err != nil {
					t.Fatal("有损合成 Route 构造失败")
				}
			}
			err = checkRouteFixtures(localRoute, root)
			if (err == nil) != tc.wantOK {
				t.Fatal("WS 样例绕过旧路径或有损能力文件名门禁")
			}
			if tc.lossy && !tc.named && (err == nil || !strings.Contains(err.Error(), "realtime_session")) {
				t.Fatal("缺少同名 JSON 未被能力门禁点名")
			}
		})
	}
	// 用 Chat 自己可表达的能力，隔离门归属错误，不能靠另一项能力违规偶然报红。
	_, err = NewRoute(ProtoOpenAIChat, ProviderOpenAICompat).
		Pass(ExpressibleSet(ProtoOpenAIChat)...).
		Redeem(EndpointOpenAIRealtime, canonical.CapTextGeneration).Build()
	if err == nil || !strings.Contains(err.Error(), "belongs to inbound protocol") {
		t.Fatal("WS 已知门错绑给 Chat 未被门归属校验拒绝")
	}
}
