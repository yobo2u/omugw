package testkit

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/yobo2u/omugw/internal/transport/ws"
)

// WSPoint 区分网关两侧的发送与接收，防止只录回复却漏掉对请求转换的断言。
type WSPoint string

const (
	WSClientSend      WSPoint = "client.send"
	WSUpstreamReceive WSPoint = "upstream.receive"
	WSUpstreamSend    WSPoint = "upstream.send"
	WSClientReceive   WSPoint = "client.receive"
)

// WSMessage 保留完整消息的 opcode 与原始字节，避免二进制负载被当成文本重编码。
type WSMessage struct {
	Opcode  ws.Opcode `json:"opcode"`
	Payload []byte    `json:"payload"`
}

// WSNode 显式保留来源与因果边，避免把一次录制的调度顺序误当协议约束。
type WSNode struct {
	ID            string        `json:"id"`
	Point         WSPoint       `json:"point"`
	Source        string        `json:"source"`
	After         []string      `json:"after,omitempty"`
	Kind          string        `json:"kind"`
	Message       *WSMessage    `json:"message,omitempty"`
	CloseCode     *uint16       `json:"close_code,omitempty"`
	CloseReason   string        `json:"close_reason,omitempty"`
	Match         string        `json:"match,omitempty"`
	Fields        []WSFieldRule `json:"fields,omitempty"`
	ForwardedFrom string        `json:"forwarded_from,omitempty"`
}

// WSFieldRule 将动态字段规则与字面值分开，防止规范化吞掉错误的实体引用。
type WSFieldRule struct {
	Pointer   string          `json:"pointer"`
	Mode      string          `json:"mode"`
	Namespace string          `json:"namespace,omitempty"`
	Symbol    string          `json:"symbol,omitempty"`
	Value     json.RawMessage `json:"value,omitempty"`
}

// WSProvenance 保留可追溯信息，防止合成机制测试冒充真实上游能力证据。
// 标签与摘要不构成密码学来源认证，也不代替真实录制与人工审核。
type WSProvenance struct {
	Kind             string            `json:"kind"`
	RecordedAt       string            `json:"recorded_at,omitempty"`
	UpstreamProtocol string            `json:"upstream_protocol,omitempty"`
	UpstreamVersion  string            `json:"upstream_version,omitempty"`
	Model            string            `json:"model,omitempty"`
	SourceSHA256     string            `json:"source_sha256,omitempty"`
	SampleSHA256     map[string]string `json:"sample_sha256,omitempty"`
}

// WSSample 同时保留字节与摘要，避免样本内容改动后仍沿用旧的证据声明。
type WSSample struct {
	Data   []byte `json:"data"`
	SHA256 string `json:"sha256"`
}

// WSTerminal 将业务终态绑到具体消息与实体，避免把传输关闭误当业务完成。
type WSTerminal struct {
	Node         string `json:"node"`
	Namespace    string `json:"namespace"`
	Symbol       string `json:"symbol"`
	IDPointer    string `json:"id_pointer"`
	StatePointer string `json:"state_pointer"`
	State        string `json:"state"`
}

// WSOutcome 单列业务结局，避免仅凭所有发送完成就将中断轨迹判为成功。
type WSOutcome struct {
	Kind     string       `json:"kind"`
	Terminal []WSTerminal `json:"terminal,omitempty"`
}

// WSCoverage 记录要验证的能力及观测点，不自行兑现能力或证明上游实际支持。
type WSCoverage struct {
	Capability string   `json:"capability"`
	Nodes      []string `json:"nodes"`
	Note       string   `json:"note"`
}

// WSSession 将上下游握手与消息轨迹独立保存，防止套用 HTTP upstream.body 的必填契约。
// 该 schema 不代表生产 WS 接线已投放；完整轨迹校验由后续任务接入。
type WSSession struct {
	Version                int                 `json:"version"`
	ClientProtocol         string              `json:"client_protocol"`
	ClientVersion          string              `json:"client_version"`
	Upstream               Request             `json:"upstream"`
	UpstreamExpectedStatus int                 `json:"upstream_expected_status"`
	UpstreamError          json.RawMessage     `json:"upstream_error,omitempty"`
	Provenance             WSProvenance        `json:"provenance"`
	Samples                map[string]WSSample `json:"samples,omitempty"`
	Coverage               []WSCoverage        `json:"coverage,omitempty"`
	Nodes                  []WSNode            `json:"nodes,omitempty"`
	Outcome                WSOutcome           `json:"outcome"`
}

// 先拦传输信封错配，避免空请求体的 WS 握手借用或放宽旧 HTTP 校验。
// 此处仅守基本握手边界，来源、预算、轨迹与关闭结局的完整校验由后续任务接入。
func (f Fixture) validateWSEnvelope() error {
	if f.Response.Body != nil || f.Response.SSE != nil {
		return fmt.Errorf("fixture %q 的 ws 与 body、sse 互斥", f.Name)
	}
	if f.Upstream != nil {
		return fmt.Errorf("fixture %q 的 ws 与 HTTP upstream 预期互斥", f.Name)
	}
	session := f.Response.WS
	for _, handshake := range []struct {
		name    string
		request Request
	}{
		{"request", f.Request},
		{"ws.upstream", session.Upstream},
	} {
		if handshake.request.Method != http.MethodGet {
			return fmt.Errorf("fixture %q 的 %s.method 必须为 GET", f.Name, handshake.name)
		}
		if handshake.request.Path == "" || strings.Contains(handshake.request.Path, "?") {
			// 不回显错误路径，防止误放进 path 的 query 凭据流入失败日志。
			return fmt.Errorf("fixture %q 的 %s.path 必须非空且不带 query", f.Name, handshake.name)
		}
	}
	if session.UpstreamExpectedStatus == 0 {
		return fmt.Errorf("fixture %q 缺少 ws.upstream_expected_status", f.Name)
	}
	if session.Outcome.Kind == "handshake_failed" {
		if f.Response.Status == http.StatusSwitchingProtocols || session.UpstreamExpectedStatus == http.StatusSwitchingProtocols {
			return fmt.Errorf("fixture %q 的 handshake_failed 上下游状态均不能为 101", f.Name)
		}
	} else if f.Response.Status != http.StatusSwitchingProtocols || session.UpstreamExpectedStatus != http.StatusSwitchingProtocols {
		return fmt.Errorf("fixture %q 的 WS 会话上下游状态必须为 101", f.Name)
	}
	return nil
}
