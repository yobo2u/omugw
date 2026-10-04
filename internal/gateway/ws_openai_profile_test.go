package gateway

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/degrade"
	"github.com/yobo2u/omugw/internal/obs"
	oaws "github.com/yobo2u/omugw/internal/provider/openairealtime"
	"github.com/yobo2u/omugw/internal/router"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

func TestOpenAIRealtimeProfile(t *testing.T) {
	h := NewOpenAIRealtimeHandler(WSDeps{})
	for _, raw := range []string{wsTestInitial, `{"type":"session.updated","session":{"id":"s","type":"realtime","object":"realtime.session"}}`} {
		if err := h.profile.checkReady([]byte(raw)); err == nil {
			t.Fatal("非 GA ready 获准升级")
		}
	}
	if err := h.profile.checkReady([]byte(openAIReady)); err != nil {
		t.Fatal(err)
	}
	err := h.profile.checkReady([]byte(`{"type":"error","error":{"code":"rate_limit_exceeded","message":"private"}}`))
	var failure *canonical.Error
	if !errors.As(err, &failure) || failure.Class != canonical.ClassRateLimit || strings.Contains(err.Error(), "private") {
		t.Fatal("首错误未保留安全分类", err)
	}
	for _, header := range []http.Header{{"OpenAI-Beta": {""}}, {"Sec-WebSocket-Protocol": {""}}} {
		if err := h.profile.validateHeaders(header); err == nil {
			t.Fatal("GA 禁止的握手形式未拒绝")
		}
	}
	if f := h.profile.classifyClose(1011, "To many requests. private"); f == nil || f.Class != canonical.ClassInternal || f.Retryable || strings.Contains(f.Error(), "private") {
		t.Fatal("串用了 DS 关闭文案")
	}
	if f := h.profile.classifyClose(1000, "private"); f != nil {
		t.Fatal(f)
	}
	w := httptest.NewRecorder()
	h.writeWSError(w, canonical.Newf(canonical.ClassAuth, "private"))
	if w.Code != 401 || !bytes.Contains(w.Body.Bytes(), []byte(`"error":`)) || bytes.Contains(w.Body.Bytes(), []byte("private")) {
		t.Fatal("未绑定 OpenAI 安全错误信封")
	}
}

// 手工测试矩阵仅核验固定 profile 的消费者接线；不把合成服务端当作生产开门证据。
func TestOpenAIRealtimeProfileRelayPreservesEvents(t *testing.T) {
	const clientUpdate = ` {"type":"session.update","session":{"audio":{"input":{"transcription":{"model":"unconfirmed"}}}},"future":[null,1]} `
	const committed = `{"type":"input_audio_buffer.committed","item_id":"disabled"}`
	const failure = ` {"type":"error","error":{"code":"invalid_value","message":"private"},"future":true} `
	const unknown = `{"type":"future.event","text":"private","usage":{"input_tokens":999}}`
	const closeReason = "To many requests. private"
	upDone := make(chan error, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var result error
		defer func() { upDone <- result }()
		if r.URL.Path != "/v1/realtime" || r.URL.Query().Get("model") != "real-model" || r.Header.Get("Authorization") != "Bearer synthetic-a" {
			result = errors.New("固定 GA profile 未绑定正确 Provider 坐标")
			return
		}
		c, err := ws.Accept(w, r, ws.AcceptOptions{MaxPayload: 1 << 20, Idle: time.Second, WriteTimeout: time.Second})
		if err != nil {
			result = err
			return
		}
		defer c.Close(1000, "")
		if result = c.WriteMessage(ws.OpText, []byte(openAIReady)); result != nil {
			return
		}
		op, raw, err := c.ReadMessage()
		if err != nil || op != ws.OpText || string(raw) != clientUpdate {
			result = errors.New("下游配置未原样转发")
			return
		}
		for _, s := range []string{committed, failure, unknown, openAIResponse("r", openAIResponseUsage), openAIResponse("r", openAIResponseUsage)} {
			if result = c.WriteMessage(ws.OpText, []byte(s)); result != nil {
				return
			}
		}
		_ = c.Close(1011, closeReason)
	}))
	defer up.Close()
	d := wsHandlerDeps(t, up.URL)
	reg := prometheus.NewRegistry()
	d.Metrics = obs.NewMetrics(reg)
	var err error
	d.Router, err = router.New([]router.Rule{{Match: "real-model", Targets: []router.Target{{Kind: degrade.ProviderOpenAIRealtime, Endpoint: "ep", BaseURL: up.URL, UpstreamModel: "real-model", CredentialPool: "pool"}}}})
	if err != nil {
		t.Fatal(err)
	}
	d.Providers["ep"] = oaws.New(d.Timeouts, d.Limits, d.Budget)
	d.Matrix = degrade.NewMatrix()
	caps := degrade.ExpressibleSet(degrade.ProtoOpenAIRealtime)
	if err := d.Matrix.Add(degrade.NewRoute(degrade.ProtoOpenAIRealtime, degrade.ProviderOpenAIRealtime).Pass(caps...).Redeem(degrade.EndpointOpenAIRealtime, caps...).MarkHomogeneous().Build()); err != nil {
		t.Fatal(err)
	}
	h := NewOpenAIRealtimeHandler(d)
	if err := checkWSDoor(d.Matrix, h); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { defer close(done); h.ServeHTTP(w, r) }))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, resp, err := ws.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/realtime?model=real-model", ws.DialOptions{Header: http.Header{"Authorization": {"Bearer synthetic-key"}}, MaxPayload: 1 << 20, WriteTimeout: time.Second})
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(1000, "")
	wsReadLiteral(t, c, openAIReady)
	if err := c.WriteMessage(ws.OpText, []byte(clientUpdate)); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{committed, failure, unknown, openAIResponse("r", openAIResponseUsage), openAIResponse("r", openAIResponseUsage)} {
		wsReadLiteral(t, c, s)
	}
	_, _, err = c.ReadMessage()
	var closed *ws.CloseError
	if !errors.As(err, &closed) || closed.Code != 1011 || closed.Reason != closeReason {
		t.Fatal("原生关闭被重写", err)
	}
	closed.Release()
	awaitWSTest(t, done)
	if err := receiveWSTest(t, upDone); err != nil {
		t.Fatal(err)
	}
	assertOpenAIMetric(t, reg, "omugw_ws_usage_records_total", map[string]string{"fidelity": "authoritative"}, 1)
	assertOpenAIMetric(t, reg, "omugw_ws_usage_records_total", map[string]string{"fidelity": "unavailable"}, 0)
	assertOpenAIMetric(t, reg, "omugw_ws_tokens_total", map[string]string{"kind": "input"}, 132)
	assertOpenAIMetric(t, reg, "omugw_upstream_errors_total", map[string]string{"class": "bad_request"}, 1)
	if d.Budget.Used() != 0 {
		t.Fatal("profile 接线遗留预算")
	}
	for _, st := range d.Pools["pool"].Stats() {
		if st.ConsecutiveFails != 0 || st.Picks > 1 {
			t.Fatal("误用了 DS close 分类冷却凭据或重拨")
		}
	}
	if err := d.Registry.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}
