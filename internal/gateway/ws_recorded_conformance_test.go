package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/obs"
	"github.com/yobo2u/omugw/internal/testkit"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

const wsRecordedDir = "../../testdata/fixtures/dashscope/realtime"

// 数字来自已审阅的原始录制，不借 Inspect 或用量账本生成自己的预期。
var wsRecordedCases = []struct {
	name, model, fixtureSHA, recordingSHA string
	nodes, messages, messageBytes         int
	pcmBytes                              int
	pcmSHA                                string
	terminals                             []string
	input, output, audio, chars, records  float64
}{
	{"tts-commit-v3", "qwen3-tts-flash-realtime",
		"41eab869cd2456ce2244e887867306bf336c07ae403378842d5a291748f3e429",
		"43edba97b4dce9cd37bc37bed556c8438e36e1987a03e1c4e91d5f09b8b15eeb",
		48, 23, 109252, 78294, "b063ca7ae3af8ab46a3c06dc83e404eb2cd11dbdfc16afe9a4d33e346aea46a7",
		[]string{"resp_LzPxhGd3LK7NJ3YEKi3DT"}, 8, 31, 31, 25, 1},
	{"tts-server-commit-v2", "qwen3-tts-flash-realtime",
		"a6140efd3485acda1d0b3dbbc0d6b9c6036f17bcddd92cc27c593efd68f80f52",
		"7b8fb6cd5defab14e76a5149d8bec4d796c87b26aa72e366e97879a27c579c71",
		46, 22, 126279, 91016, "6d08efc191f77a5a51d365de428a0f1ef1f59e8b96f5f27c15e755e4fef5e936",
		[]string{"resp_I3ZwI4wPLYZN1CAjRitXb"}, 8, 36, 36, 25, 1},
	{"text-tools-v3", "qwen3.5-omni-flash-realtime",
		"2155931e0220da70a2693dbb45bb0fcf63a92fbd9eef6d516dfc7b41c7f277a2",
		"971d3b8752fbb04313b3b2b4f918daf11eb1b4ff73765fc63d0f791f2fa55c6f",
		102, 50, 14976, 0, "",
		[]string{"resp_BPdS3pZw9rS72JXXIsA8P", "resp_W6oAec8azwWhEe5PzRNmN", "resp_Pd74ULjlgW8kwUIxApXGH"}, 1165, 49, 0, 0, 3},
}

func wsRecordedBytes(t *testing.T, path, digest string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%x", sha256.Sum256(raw)) != digest {
		t.Fatalf("归档字节摘要改变: %s", path)
	}
	return raw
}

func wsReadRecorded(t *testing.T, name string) testkit.Fixture {
	t.Helper()
	f, err := testkit.ReadWSFixture(filepath.Join(wsRecordedDir, name+".json"), testkit.DefaultWSLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err := testkit.ValidateWSSession(f, testkit.DefaultWSLimits()); err != nil {
		t.Fatal(err)
	}
	return f
}

// 删除首事件后若仍允许 session/response 动态重命名，就丢掉了握手身份与终态交接。
func TestWSRecordedTail(t *testing.T) {
	for _, tc := range wsRecordedCases {
		t.Run(tc.name, func(t *testing.T) {
			f := wsReadRecorded(t, tc.name)
			before, _ := json.Marshal(f)
			tail, prelude, err := wsRecordedTail(f)
			if err != nil {
				t.Fatal(err)
			}
			if prelude[0].Point != testkit.WSUpstreamSend || prelude[1].ForwardedFrom != prelude[0].ID || len(tail.Response.WS.Nodes) != tc.nodes-2 {
				t.Fatal("prelude 唯一发送/接收关系或 tail 数量错误")
			}
			for _, n := range tail.Response.WS.Nodes {
				if len(n.Fields) != 0 || n.Message != nil && n.Match != "bytes" {
					t.Fatal("tail 仍容许动态 ID 或非字节匹配")
				}
				if n.Message == nil {
					continue
				}
				matcher, err := testkit.NewWSMatcher(testkit.DefaultWSLimits())
				if err != nil || matcher.Match(n, *n.Message) != nil {
					t.Fatal("字面原消息不能匹配")
				}
				// 两种身份都必须锁为录制值，即使转发两点同时换 ID 也不能放行。
				for _, prefix := range []string{"sess_", "resp_"} {
					if bytes.Contains(n.Message.Payload, []byte(prefix)) {
						changed := testkit.WSMessage{Opcode: n.Message.Opcode, Payload: bytes.ReplaceAll(n.Message.Payload, []byte(prefix), []byte("other_"))}
						if matcher.Match(n, changed) == nil {
							t.Fatal("tail 接受了脱离 prelude/终态的实体")
						}
					}
				}
			}
			var ids []string
			for _, terminal := range tail.Response.WS.Outcome.Terminal {
				ids = append(ids, terminal.Symbol)
			}
			if !reflect.DeepEqual(ids, tc.terminals) {
				t.Fatal("终态没有交接到原始字面 ID")
			}
			after, _ := json.Marshal(f)
			if !bytes.Equal(before, after) {
				t.Fatal("tail 修改了原 fixture 内存")
			}
		})
	}
}

// 正式 handler/provider 若丢事件、改字节、漏计混合单位或未 join，实录回放必须失败。
func TestWSRecordedConformance(t *testing.T) {
	for _, tc := range wsRecordedCases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(wsRecordedDir, tc.name+".json")
			before := wsRecordedBytes(t, path, tc.fixtureSHA)
			recordPath := filepath.Join(wsRecordedDir, "recordings", tc.name+".json")
			recordRaw := wsRecordedBytes(t, recordPath, tc.recordingSHA)
			f := wsReadRecorded(t, tc.name)
			var recording struct {
				Records []struct {
					Direction   string    `json:"direction"`
					Kind        string    `json:"kind"`
					Opcode      ws.Opcode `json:"opcode"`
					Payload     []byte    `json:"payload"`
					CloseCode   uint16    `json:"close_code"`
					CloseReason string    `json:"close_reason"`
				} `json:"records"`
			}
			if err := json.Unmarshal(recordRaw, &recording); err != nil {
				t.Fatal(err)
			}
			if len(recording.Records) != tc.messages+2 || len(f.Response.WS.Nodes) != tc.nodes {
				t.Fatal("原始记录或双点节点缺失")
			}
			var pcm []byte
			var messageBytes int
			for i, record := range recording.Records[:tc.messages] {
				points := []testkit.WSPoint{testkit.WSClientSend, testkit.WSUpstreamReceive}
				if record.Direction == "receive" {
					points = []testkit.WSPoint{testkit.WSUpstreamSend, testkit.WSClientReceive}
				} else if record.Direction != "send" {
					t.Fatal("未知原始方向")
				}
				for j, point := range points {
					n := f.Response.WS.Nodes[2*i+j]
					if record.Kind != "message" || n.Point != point || n.Message == nil || n.Message.Opcode != record.Opcode || !bytes.Equal(n.Message.Payload, record.Payload) {
						t.Fatalf("record %d 双点 opcode/payload 不等于原始字节", i)
					}
				}
				messageBytes += len(record.Payload)
				var event struct{ Type, Delta string }
				if err := json.Unmarshal(record.Payload, &event); err != nil {
					t.Fatal(err)
				}
				if event.Type == "response.audio.delta" {
					part, err := base64.StdEncoding.Strict().DecodeString(event.Delta)
					if err != nil || len(part) == 0 {
						t.Fatal("真实音频分片非法或为空")
					}
					pcm = append(pcm, part...)
				}
			}
			for i, direction := range []string{"send", "receive"} {
				close := recording.Records[tc.messages+i]
				if close.Kind != "close" || close.Direction != direction || close.CloseCode != 1000 || close.CloseReason != "" {
					t.Fatal("原录制没有真实双向正常关闭")
				}
			}
			if messageBytes != tc.messageBytes || len(pcm) != tc.pcmBytes {
				t.Fatal("原始消息/音频字节数改变")
			}
			if tc.pcmBytes > 0 {
				sample := f.Response.WS.Samples["output.pcm_s16le.16000.mono"]
				if len(f.Response.WS.Samples) != 1 || !bytes.Equal(pcm, sample.Data) || sample.SHA256 != tc.pcmSHA || fmt.Sprintf("%x", sha256.Sum256(pcm)) != tc.pcmSHA {
					t.Fatal("原分片与内嵌音频样本/SHA 不同")
				}
			} else if len(f.Response.WS.Samples) != 0 {
				t.Fatal("文本实录出现非预期样本")
			}
			result, reg, err := wsReplayRecorded(t, f, tc.model)
			if err != nil {
				t.Fatal(err)
			}
			if result.Outcome.Kind != "completed" || len(result.Outcome.Terminal) != len(tc.terminals) || len(result.Nodes) != tc.nodes-2 {
				t.Fatal("不是全部 tail 节点/业务终态完成")
			}
			// 三份录制均为链式双点图：每一发送必须等前一接收证据；不推广成任意图的网络确定性。
			var order []string
			for i := 1; i <= tc.messages; i++ {
				order = append(order, fmt.Sprintf("n%04d_0", i))
			}
			if !reflect.DeepEqual(result.SendOrder, order) {
				t.Fatal("发令顺序未保留原始接收因果")
			}
			for i, terminal := range result.Outcome.Terminal {
				if terminal.Symbol != tc.terminals[i] || terminal.State != "completed" {
					t.Fatal("终态实体/状态丢失")
				}
			}
			for _, metric := range []string{"omugw_tokens_total", "omugw_ws_tokens_total"} {
				for kind, want := range map[string]float64{"input": tc.input, "output": tc.output, "audio_output": tc.audio, "audio_input": 0} {
					labels := map[string]string{"kind": kind, "fidelity": "authoritative", "outbound": "dashscope.ws.realtime"}
					if metric == "omugw_ws_tokens_total" {
						labels = map[string]string{"kind": kind, "fidelity": "authoritative", "protocol": "dashscope.realtime", "source": "response"}
					}
					if got := wsUsageMetricSum(t, reg, metric, labels); got != want {
						t.Fatalf("%s/%s = %v, want %v", metric, kind, got, want)
					}
				}
			}
			charRecords := float64(0)
			if tc.chars > 0 {
				charRecords = 1
			}
			for unit, want := range map[string]float64{"tokens": tc.records, "characters": charRecords} {
				if got := wsUsageMetricSum(t, reg, "omugw_ws_usage_records_total", map[string]string{"protocol": "dashscope.realtime", "source": "response", "unit": unit, "fidelity": "authoritative"}); got != want {
					t.Fatalf("response %s records = %v, want %v", unit, got, want)
				}
			}
			if got := wsUsageMetricSum(t, reg, "omugw_ws_characters_total", map[string]string{"protocol": "dashscope.realtime", "source": "response", "fidelity": "authoritative"}); got != tc.chars {
				t.Fatalf("characters = %v, want %v", got, tc.chars)
			}
			if wsUsageMetricSum(t, reg, "omugw_ws_usage_records_total", map[string]string{"source": "response", "fidelity": "unavailable"}) != 0 || wsUsageMetricSum(t, reg, "omugw_requests_total", map[string]string{"inbound": "dashscope.realtime", "outbound": "dashscope.ws.realtime", "outcome": "ok"}) != 1 {
				t.Fatal("响应权威用量丢失或会话计数不符")
			}
			if !bytes.Equal(before, wsRecordedBytes(t, path, tc.fixtureSHA)) || !bytes.Equal(recordRaw, wsRecordedBytes(t, recordPath, tc.recordingSHA)) {
				t.Fatal("离线回放改写了归档文件")
			}
			t.Logf("tail=%d terminals=%d messages=%d raw_bytes=%d PCM=%d; tokens=%g/%g audio=%g chars=%g records=%g; SHA unchanged", len(result.Nodes), len(result.Outcome.Terminal), tc.messages, messageBytes, len(pcm), tc.input, tc.output, tc.audio, tc.chars, tc.records)
		})
	}
}

func wsRecordedTail(f testkit.Fixture) (testkit.Fixture, [2]testkit.WSNode, error) {
	var prelude [2]testkit.WSNode
	limits := testkit.DefaultWSLimits()
	if err := testkit.ValidateWSSession(f, limits); err != nil {
		return testkit.Fixture{}, prelude, err
	}
	raw, err := json.Marshal(f)
	if err != nil {
		return testkit.Fixture{}, prelude, err
	}
	var tail testkit.Fixture
	if err := json.Unmarshal(raw, &tail); err != nil {
		return tail, prelude, err
	}
	s := tail.Response.WS
	if len(s.Nodes) < 2 {
		return tail, prelude, fmt.Errorf("实录缺少 prelude")
	}
	copy(prelude[:], s.Nodes[:2])
	a, b := prelude[0], prelude[1]
	if a.Point != testkit.WSUpstreamSend || b.Point != testkit.WSClientReceive || a.Message == nil || b.Message == nil || a.Message.Opcode != ws.OpText || b.Message.Opcode != ws.OpText || !bytes.Equal(a.Message.Payload, b.Message.Payload) || b.ForwardedFrom != a.ID || len(a.After) != 0 || !reflect.DeepEqual(b.After, []string{a.ID}) {
		return tail, prelude, fmt.Errorf("prelude 不是唯一原字节发送/接收对")
	}
	var created struct {
		Type    string
		Session struct{ ID string }
	}
	if err := json.Unmarshal(a.Message.Payload, &created); err != nil || created.Type != "session.created" || created.Session.ID == "" {
		return tail, prelude, fmt.Errorf("prelude 缺少 session.created 身份")
	}
	// 原 fixture 已验证动态引用图；这里固定唯一 session 绑定与接收引用，不能用 synthetic ID 代替。
	var binding, reference testkit.WSFieldRule
	for i, n := range prelude {
		for _, field := range n.Fields {
			if field.Pointer == "/session/id" {
				if i == 0 {
					binding = field
				} else {
					reference = field
				}
			}
		}
	}
	if binding.Mode != "bind" || binding.Namespace != "session" || binding.Symbol == "" || reference.Mode != "reference" || reference.Namespace != binding.Namespace || reference.Symbol != binding.Symbol {
		return tail, prelude, fmt.Errorf("prelude 缺少唯一 session 绑定/引用")
	}
	removed := map[string]bool{a.ID: true, b.ID: true}
	s.Nodes = s.Nodes[2:]
	for i := range s.Nodes {
		n := &s.Nodes[i]
		if removed[n.ForwardedFrom] {
			return tail, prelude, fmt.Errorf("tail 仍引用 prelude 转发点")
		}
		if n.Message != nil {
			var event struct{ Type string }
			if err := json.Unmarshal(n.Message.Payload, &event); err != nil || event.Type == "session.created" {
				return tail, prelude, fmt.Errorf("prelude 不是唯一 session.created 对")
			}
			// 不忽略任何字段：连 event_id/空白都精确匹配，强于原动态规则。
			n.Fields = nil
			n.Match = "bytes"
		}
		after := []string(nil)
		for _, id := range n.After {
			if !removed[id] {
				after = append(after, id)
			}
		}
		n.After = after
	}
	for i := range s.Outcome.Terminal {
		terminal := &s.Outcome.Terminal[i]
		if terminal.IDPointer != "/response/id" || terminal.Namespace != "response" {
			return tail, prelude, fmt.Errorf("实录尾视图仅支持已审阅的 response 终态")
		}
		for _, n := range s.Nodes {
			if n.ID == terminal.Node {
				var event struct{ Response struct{ ID string } }
				if err := json.Unmarshal(n.Message.Payload, &event); err != nil || event.Response.ID == "" {
					return tail, prelude, fmt.Errorf("终态缺少原始 ID")
				}
				terminal.Symbol = event.Response.ID
			}
		}
	}
	coverage := []testkit.WSCoverage(nil)
	for _, c := range s.Coverage {
		ids := []string(nil)
		for _, id := range c.Nodes {
			if !removed[id] {
				ids = append(ids, id)
			}
		}
		if len(ids) != 0 {
			c.Nodes = ids
			coverage = append(coverage, c)
		}
	}
	s.Coverage = coverage
	s.Provenance.SourceSHA256, err = testkit.WSContractDigest(*s)
	if err == nil {
		err = testkit.ValidateWSSession(tail, limits)
	}
	return tail, prelude, err
}

func wsReplayRecorded(t *testing.T, f testkit.Fixture, model string) (testkit.WSReplayResult, *prometheus.Registry, error) {
	t.Helper()
	var result testkit.WSReplayResult
	reg := prometheus.NewRegistry()
	tail, prelude, err := wsRecordedTail(f)
	if err != nil {
		return result, reg, err
	}
	limits := testkit.DefaultWSLimits()
	u, err := testkit.NewWSReplayUpstream(f, limits)
	if err != nil {
		return result, reg, err
	}
	us := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api-ws/v1/realtime" || r.URL.RawQuery != "model="+model || r.Header.Get("Authorization") != "Bearer sec1" || r.Header.Get("Cookie") != "" || r.Header.Get("Origin") != "" || r.Header.Get("Api-Key") != "" || r.Header.Get("Sec-WebSocket-Protocol") != "" {
			t.Error("上游 path/model/鉴权替换或秘密头清洗不符")
		}
		u.ServeHTTP(w, r)
	}))
	defer us.Close()
	defer u.Close()
	cfg := buildTestConfig(us.URL)
	cfg.Providers[0].Kind = string(degrade.ProviderDashScopeWSRealtime)
	cfg.Models[0].Targets[0].UpstreamModel = model
	cfg.WebSocket = config.WebSocket{MaxMessageBytes: limits.MessageBytes, MaxBufferedBytes: 8 << 20, MaxSessions: 4}
	b, err := buildWithWS(cfg, wsHandlerMatrix(t, true), obs.NewMetrics(reg), slog.New(slog.NewTextHandler(io.Discard, nil)), true)
	if err != nil {
		return result, reg, err
	}
	done := make(chan struct{}, 1)
	gs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { done <- struct{}{} }()
		b.Mux.ServeHTTP(w, r)
	}))
	defer gs.Close()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := b.ShutdownWebSockets(ctx); err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	type readyResult struct {
		conn *ws.Conn
		err  error
	}
	ready := make(chan readyResult, 1)
	preludeDone := make(chan struct{})
	// handler 预读在下游 101 之前；这条 goroutine 只提交原始首事件并将连接交接给独立 driver。
	go func() {
		defer close(preludeDone)
		up, err := u.Connection(ctx)
		if err == nil {
			err = up.WriteMessage(prelude[0].Message.Opcode, prelude[0].Message.Payload)
		}
		ready <- readyResult{up, err}
	}()
	c, resp, dialErr := ws.Dial(ctx, "ws"+strings.TrimPrefix(gs.URL, "http")+"/api-ws/v1/realtime?model="+model, ws.DialOptions{
		Header: http.Header{"Authorization": {"Bearer sk-test-1234567890"}, "Origin": {"https://client.example"}, "Cookie": {"private=synthetic"}}, MaxPayload: limits.MessageBytes, Idle: 2 * time.Second})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if c != nil {
		defer c.Close(1000, "")
	}
	if dialErr != nil {
		cancel()
	}
	up := <-ready
	<-preludeDone
	if dialErr != nil || up.err != nil {
		return result, reg, errors.Join(dialErr, up.err)
	}
	op, payload, err := c.ReadMessage()
	if err != nil || op != prelude[1].Message.Opcode || !bytes.Equal(payload, prelude[1].Message.Payload) {
		return result, reg, fmt.Errorf("下游 prelude 未逐字节保全: %w", err)
	}
	result, replayErr := testkit.ReplayWS(ctx, tail, testkit.WSReplayEndpoints{Client: c, Upstream: up.conn}, 1, limits)
	// 先等 handler 自然返回：不能用主动 Shutdown 给本来未正常结束的轨迹补关闭。
	select {
	case <-done:
	case <-ctx.Done():
		return result, reg, errors.Join(replayErr, ctx.Err())
	}
	shutdownErr := b.ShutdownWebSockets(ctx)
	closeErr := u.Close()
	if err := errors.Join(replayErr, shutdownErr, closeErr, u.Err()); err != nil {
		return result, reg, err
	}
	if b.wsBudget.Used() != 0 {
		return result, reg, fmt.Errorf("全部 driver/handler/shutdown join 后预算未归零")
	}
	families, err := reg.Gather()
	if err != nil {
		return result, reg, err
	}
	for _, name := range []string{"omugw_first_byte_seconds", "omugw_request_duration_seconds"} {
		var count uint64
		for _, family := range families {
			if family.GetName() == name {
				for _, metric := range family.Metric {
					count += metric.GetHistogram().GetSampleCount()
				}
			}
		}
		if count != 1 {
			return result, reg, fmt.Errorf("%s count=%d, want 1", name, count)
		}
	}
	return result, reg, nil
}
