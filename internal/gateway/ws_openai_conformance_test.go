package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/config"
	"github.com/yobo2u/omugw/internal/testkit"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

func TestOpenAIRealtimeConformanceReplay(t *testing.T) {
	limits := testkit.DefaultWSLimits()
	f, err := testkit.ReadWSFixture("../../testdata/testkit/ws/openai-ga/roundtrip.json", limits)
	if err != nil {
		t.Fatal(err)
	}
	original, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	tail := wsConformanceTail(t, f)
	for _, n := range tail.Response.WS.Nodes {
		if len(n.Fields) != 0 || n.Message != nil && n.Match != "bytes" {
			t.Fatal("tail 放宽了字面 ID 或原字节")
		}
	}
	u, err := testkit.NewWSReplayUpstream(f, limits)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	us := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v1/realtime" || r.URL.RawQuery != "model=real-model" || r.Header.Get("Authorization") != "Bearer sec1" || r.Header.Get("User-Agent") != "omugw" || r.Header.Get("OpenAI-Safety-Identifier") != "synthetic-safety" {
			t.Error("GA 正式装配的路径/model/凭据/白名单不符")
		}
		for _, h := range []string{"Api-Key", "Cookie", "Origin", "OpenAI-Organization", "OpenAI-Project", "OpenAI-Beta", "Sec-WebSocket-Protocol", "Sec-WebSocket-Extensions", "X-DashScope-WorkSpace", "X-Private"} {
			if r.Header.Get(h) != "" {
				t.Errorf("额外头转发: %s", h)
			}
		}
		u.ServeHTTP(w, r)
	}))
	defer us.Close()
	b, reg := openAITestBuild(t, openAITestConfig(us.URL), false)
	gs, done := openAITestServer(t, b, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type readyResult struct {
		c   *ws.Conn
		err error
	}
	ready := make(chan readyResult, 1)
	preludeDone := make(chan struct{})
	go func() {
		defer close(preludeDone)
		c, err := u.Connection(ctx)
		if err == nil {
			err = c.WriteMessage(f.Response.WS.Nodes[0].Message.Opcode, f.Response.WS.Nodes[0].Message.Payload)
		}
		ready <- readyResult{c, err}
	}()
	c, resp, dialErr := ws.Dial(ctx, "ws"+strings.TrimPrefix(gs.URL, "http")+"/v1/realtime?model=real-model", ws.DialOptions{
		MaxPayload: limits.MessageBytes, WriteTimeout: time.Second,
		Header: http.Header{"Api-Key": {"sk-test-1234567890"}, "OpenAI-Safety-Identifier": {"synthetic-safety"}, "Origin": {"https://client.example"}, "Cookie": {"private=synthetic"}, "OpenAI-Organization": {"private-org"}, "OpenAI-Project": {"private-project"}, "X-DashScope-WorkSpace": {"private-workspace"}, "X-Private": {"private-value"}, "Sec-WebSocket-Extensions": {"permessage-deflate"}},
	})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if c != nil {
		defer c.Close(1000, "")
	}
	if dialErr != nil {
		cancel()
	}
	up := receiveWSTest(t, ready)
	awaitWSTest(t, preludeDone)
	if err := errors.Join(dialErr, up.err); err != nil {
		t.Fatal(err)
	}
	if resp.Header.Get("Sec-WebSocket-Extensions") != "" || resp.Header.Get("Sec-WebSocket-Protocol") != "" {
		t.Fatal("下游意外选择扩展/子协议")
	}
	wsReadLiteral(t, c, string(f.Response.WS.Nodes[1].Message.Payload))
	result, replayErr := testkit.ReplayWS(ctx, tail, testkit.WSReplayEndpoints{Client: c, Upstream: up.c}, 1, limits)
	// 自然终结在 Shutdown 之前，避免以强制关停补造原生 close 证据。
	awaitWSTest(t, done)
	if err := errors.Join(replayErr, b.ShutdownWebSockets(ctx), u.Close(), u.Err()); err != nil {
		t.Fatal(err)
	}
	wantOrder := []string{"update.c", "updated.u", "image.c", "image-ack.u", "create-text.c", "text-start.u", "text.u", "text-done.u", "audio.c", "commit.c", "committed.u", "asr-delta.u", "asr.u", "seconds.u", "seconds-zero.u", "seconds-missing.u", "tools.c", "call.u", "tool-output.c", "error.u", "create-audio.c", "audio-start.u", "audio-out.u", "cancel.c", "cancelled.u", "truncate.c", "truncated.u", "future.u", "close.c"}
	if result.Outcome.Kind != "completed" || len(result.Nodes) != 58 || !reflect.DeepEqual(result.SendOrder, wantOrder) {
		t.Fatalf("四点轨迹结果不符: %+v", result)
	}
	for _, tc := range []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"omugw_tokens_total", map[string]string{"kind": "input", "fidelity": "authoritative"}, 145},
		{"omugw_tokens_total", map[string]string{"kind": "output", "fidelity": "authoritative"}, 132},
		{"omugw_ws_usage_records_total", map[string]string{"source": "response", "unit": "tokens", "fidelity": "authoritative"}, 2},
		{"omugw_ws_usage_records_total", map[string]string{"source": "transcription", "unit": "tokens", "fidelity": "authoritative"}, 1},
		{"omugw_ws_usage_records_total", map[string]string{"unit": "tokens", "fidelity": "unavailable"}, 0},
		{"omugw_ws_usage_records_total", map[string]string{"unit": "seconds", "fidelity": "authoritative"}, 2},
		{"omugw_ws_usage_records_total", map[string]string{"unit": "seconds", "fidelity": "unavailable"}, 1},
		{"omugw_ws_audio_input_seconds_total", map[string]string{"source": "transcription"}, 1.25},
		{"omugw_upstream_errors_total", map[string]string{"class": "bad_request"}, 1},
		{"omugw_requests_total", map[string]string{"inbound": "openai.realtime", "outbound": "openai.realtime", "outcome": "ok"}, 1},
		{"omugw_ws_diagnostics_total", map[string]string{"reason": "usage_unfinished"}, 0},
	} {
		assertOpenAIMetric(t, reg, tc.name, tc.labels, tc.want)
	}
	for kind, want := range map[string]float64{"input": 132, "output": 123, "text_input": 119, "audio_input": 13, "image_input": 0, "cache_read": 64, "cached_text_input": 64, "cached_audio_input": 0, "cached_image_input": 0, "text_output": 30, "audio_output": 91} {
		assertOpenAIMetric(t, reg, "omugw_ws_tokens_total", map[string]string{"source": "response", "kind": kind}, want)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
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
			t.Fatalf("%s count=%d want=1", name, count)
		}
	}
	after, err := json.Marshal(f)
	if err != nil || !bytes.Equal(original, after) || b.wsBudget.Used() != 0 {
		t.Fatal("tail 污染原 fixture 或会话遗留预算")
	}
}

func TestOpenAIRealtimeBinaryLargeAndTotal(t *testing.T) {
	upDone := make(chan error, 1)
	const limit = 1 << 20
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := ws.Accept(w, r, ws.AcceptOptions{MaxPayload: limit, WriteTimeout: time.Second})
		if err != nil {
			upDone <- err
			return
		}
		defer c.Close(1000, "")
		if err = c.WriteMessage(ws.OpText, []byte(openAIReady)); err != nil {
			upDone <- err
			return
		}
		for {
			op, payload, err := c.ReadMessage()
			if err != nil {
				releaseWSRelayError(err)
				upDone <- nil
				return
			}
			if err = c.WriteMessage(op, payload); err != nil {
				upDone <- err
				return
			}
		}
	}))
	defer u.Close()
	cfg := openAITestConfig(u.URL)
	cfg.Timeouts.Connect, cfg.Timeouts.FirstByte, cfg.Timeouts.Total, cfg.Timeouts.Idle = 100*time.Millisecond, 200*time.Millisecond, 300*time.Millisecond, 200*time.Millisecond
	b, _ := openAITestBuild(t, cfg, false)
	s, done := openAITestServer(t, b, nil)
	c, _, err := openAITestDial(t, s.URL, "/v1/realtime")
	if err != nil {
		t.Fatal(err)
	}
	wsReadLiteral(t, c, openAIReady)
	for _, tc := range []struct {
		op  ws.Opcode
		raw []byte
	}{
		{ws.OpBinary, []byte{0, 255, 1, 0, 128}},
		{ws.OpText, []byte(` {"type":"future.event","audio":"` + strings.Repeat("A", limit-36) + `"} `)},
		{ws.OpBinary, bytes.Repeat([]byte{0xa5}, limit)},
	} {
		read := make(chan error, 1)
		go func() {
			op, raw, err := c.ReadMessage()
			if err == nil && (op != tc.op || !bytes.Equal(raw, tc.raw)) {
				err = errors.New("大消息类型/字节被改写")
			}
			read <- err
		}()
		// 首轮跨过 HTTP total，读者仍响应心跳；后续大消息不重复等待。
		if len(tc.raw) == 5 {
			time.Sleep(400 * time.Millisecond)
		}
		if err := c.WriteMessage(tc.op, tc.raw); err != nil {
			t.Error(err)
		}
		if err := receiveWSTest(t, read); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.WriteMessage(ws.OpBinary, bytes.Repeat([]byte{0xa5}, limit+1)); err != nil {
		t.Log("超限发送时对端已关闭:", err)
	}
	assertOpenAITestClose(t, c, 1009)
	awaitWSTest(t, done)
	if err := receiveWSTest(t, upDone); err != nil {
		t.Fatal(err)
	}
}

func assertOpenAITestClose(t *testing.T, c *ws.Conn, code uint16) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	m, err := c.ReadOwnedMessage(ctx)
	if m != nil {
		m.Release()
	}
	var closed *ws.CloseError
	if !errors.As(err, &closed) {
		t.Fatalf("预期 close %d: %v", code, err)
	}
	defer closed.Release()
	if closed.Code != code {
		t.Fatalf("close %d want %d", closed.Code, code)
	}
}

// 两门各一段正在重组的大消息必须占同一预算；独立 Provider 私建预算将使第三条误通过。
func TestOpenAIRealtimeSharedResources(t *testing.T) {
	for _, mode := range []string{"sessions-and-shutdown", "aggregate-payload"} {
		t.Run(mode, func(t *testing.T) {
			const message = 65536
			type peer struct {
				c   *ws.Conn
				raw net.Conn
			}
			peers := make(chan peer, 4)
			var calls atomic.Int32
			u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				hj := &wsTestHijacker{ResponseWriter: w}
				c, err := ws.Accept(hj, r, ws.AcceptOptions{MaxPayload: message, WriteTimeout: time.Second})
				if err != nil {
					t.Error(err)
					return
				}
				// 即使准入回归而多出一次 Dial，未被测试消费的 socket 也有兜底 owner。
				t.Cleanup(func() { _ = hj.raw.Close(); _ = c.Close(1001, "") })
				initial := openAIReady
				if r.URL.Path == "/api-ws/v1/realtime" {
					initial = wsTestInitial
				}
				if err := c.WriteMessage(ws.OpText, []byte(initial)); err != nil {
					t.Error(err)
				}
				peers <- peer{c, hj.raw}
			}))
			defer u.Close()
			cfg := openAITestConfig(u.URL)
			cfg.Providers = append(cfg.Providers, config.ProviderSpec{Endpoint: "ds", Kind: "dashscope.ws.realtime", BaseURL: u.URL, CredentialPool: "pool1"})
			cfg.Models[0].Targets = append(cfg.Models[0].Targets, config.TargetSpec{Endpoint: "ds", UpstreamModel: "real-model"})
			cfg.WebSocket = config.WebSocket{MaxMessageBytes: message, MaxBufferedBytes: 2*message + 32768, MaxSessions: 2}
			cfg.Timeouts.Idle, cfg.Timeouts.Total = 5*time.Second, 6*time.Second
			b, reg := openAITestBuild(t, cfg, true)
			s, done := openAITestServer(t, b, nil)
			var clients []*ws.Conn
			var upstreams []peer
			for i, path := range []string{"/api-ws/v1/realtime", "/v1/realtime"} {
				c, _, err := openAITestDial(t, s.URL, path)
				if err != nil {
					t.Fatal(err)
				}
				p := receiveWSTest(t, peers)
				initial := wsTestInitial
				if i == 1 {
					initial = openAIReady
				}
				wsReadLiteral(t, c, initial)
				clients, upstreams = append(clients, c), append(upstreams, p)
			}
			if mode == "sessions-and-shutdown" {
				c, resp, err := openAITestDial(t, s.URL, "/v1/realtime")
				if c != nil || err == nil || resp == nil || resp.StatusCode != 429 || calls.Load() != 2 {
					t.Fatal("两门会话数未聚合或满额后仍拨上游")
				}
				awaitWSTest(t, done)
				assertOpenAIMetric(t, reg, "omugw_requests_total", map[string]string{"inbound": "openai.realtime", "outcome": "capacity"}, 1)
			} else {
				// 只提交固定长度头，实际内核 TCP 让两个上游 reader 各持有 M，尚无完整消息可释放。
				for i, p := range upstreams {
					if _, err := p.raw.Write([]byte{0x82, 127, 0, 0, 0, 0, 0, 1, 0, 0}); err != nil {
						t.Fatal(err)
					}
					deadline := time.Now().Add(2 * time.Second)
					for b.wsBudget.Used() != int64((i+1)*message) && time.Now().Before(deadline) {
						time.Sleep(time.Millisecond)
					}
					if got := b.wsBudget.Used(); got != int64((i+1)*message) {
						t.Fatalf("出站重组预算=%d want=%d", got, (i+1)*message)
					}
				}
				_ = clients[1].WriteMessage(ws.OpBinary, bytes.Repeat([]byte{1}, message))
				assertOpenAITestClose(t, clients[1], 1013)
				assertOpenAITestClose(t, upstreams[1].c, 1013)
				awaitWSTest(t, done)
				if b.wsBudget.Used() != message {
					t.Fatal("容量拒绝误释放另一扇门或遗留本门预算")
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			stopped := make(chan error, 2)
			// 并发关闭同一个 Built，两个已劫持门与内部 close/relay worker 都须 join。
			go func() { stopped <- b.ShutdownWebSockets(ctx) }()
			go func() { stopped <- b.ShutdownWebSockets(ctx) }()
			for i := 0; i < 2; i++ {
				if err := receiveWSTest(t, stopped); err != nil {
					t.Fatal(err)
				}
			}
			active := 2
			if mode == "aggregate-payload" {
				active = 1
			}
			for i := 0; i < active; i++ {
				assertOpenAITestClose(t, clients[i], 1001)
				assertOpenAITestClose(t, upstreams[i].c, 1001)
				awaitWSTest(t, done)
			}
			if b.wsBudget.Used() != 0 {
				t.Fatal("双门退出预算未归零")
			}
			b.wsRegistry.mu.Lock()
			remaining, sealed := len(b.wsRegistry.sessions), b.wsRegistry.sealed
			b.wsRegistry.mu.Unlock()
			if remaining != 0 || !sealed || calls.Load() != 2 {
				t.Fatal("双门 shutdown 未封口/排空或意外重拨")
			}
			for _, path := range []string{"/v1/realtime", "/api-ws/v1/realtime"} {
				c, resp, err := openAITestDial(t, s.URL, path)
				if c != nil || err == nil || resp == nil || resp.StatusCode != 503 || calls.Load() != 2 {
					t.Fatal("shutdown 后仍准入或触达上游")
				}
				awaitWSTest(t, done)
			}
			assertOpenAIMetric(t, reg, "omugw_requests_total", map[string]string{"inbound": "dashscope.realtime", "outbound": "dashscope.ws.realtime", "outcome": "cancelled"}, 1)
		})
	}
}
