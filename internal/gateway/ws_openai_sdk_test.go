//go:build sdk

package gateway

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yobo2u/omugw/internal/testkit"
	"github.com/yobo2u/omugw/internal/transport/ws"
)

type sdkStep struct {
	Direction string `json:"direction"`
	Raw       string `json:"raw"`
}

type sdkInput struct {
	BaseURL string    `json:"baseURL"`
	CA      string    `json:"ca"`
	Mode    string    `json:"mode"`
	Steps   []sdkStep `json:"steps"`
}

type sdkResult struct {
	Mode        string  `json:"mode"`
	Opened      bool    `json:"opened"`
	RejectedTLS bool    `json:"rejectedTLS"`
	TLSCode     string  `json:"tlsCode"`
	Received    int     `json:"received"`
	Sent        int     `json:"sent"`
	Pings       int     `json:"pings"`
	CloseCode   int     `json:"closeCode"`
	CloseReason string  `json:"closeReason"`
	OpenMillis  float64 `json:"openMillis"`
}

// 首帧重编码、SDK 未走 WSS、错误后断链或握手头串租户，都应使这个边界测试失败。
func TestOpenAIRealtimeNodeSDKWSS(t *testing.T) {
	runOpenAIRealtimeSDK(t, "roundtrip")
}

// TLS 失败必须来自不可信 CA，不能将模块缺失、连接拒绝或任意异常冒充负例通过。
func TestOpenAIRealtimeNodeSDKRejectsUntrustedTLS(t *testing.T) {
	runOpenAIRealtimeSDK(t, "untrusted")
}

func TestOpenAIRealtimeNodeSDKHeartbeat(t *testing.T) {
	for _, mode := range []string{"heartbeat", "no-pong"} {
		t.Run(mode, func(t *testing.T) { runOpenAIRealtimeSDK(t, mode) })
	}
}

func sdkRoundtrip(t *testing.T) []sdkStep {
	t.Helper()
	f, err := testkit.ReadWSFixture("../../testdata/testkit/ws/openai-ga/roundtrip.json", testkit.DefaultWSLimits())
	if err != nil {
		t.Fatal(err)
	}
	var steps []sdkStep
	for _, n := range f.Response.WS.Nodes {
		if n.Kind != "message" || n.Point != "client.send" && n.Point != "upstream.send" {
			continue
		}
		raw := n.Message.Payload
		direction := "receive"
		if n.Point == "client.send" {
			direction = "send"
			// SDK 的 JSON.stringify 去掉客户端空白；独立 fixture 的键序与值保留。
			// 上游原文绝不规整，首事件和未知字段的空白仍须逐字节送达 SDK socket。
			var compact bytes.Buffer
			if err := json.Compact(&compact, raw); err != nil {
				t.Fatal(err)
			}
			raw = compact.Bytes()
		}
		steps = append(steps, sdkStep{direction, string(raw)})
	}
	if len(steps) != 29 || steps[0].Direction != "receive" || !strings.Contains(steps[1].Raw, `"type":"session.update"`) {
		t.Fatal("独立 SDK 轨迹前提已改变，须重新审阅")
	}
	return steps
}

func runOpenAIRealtimeSDK(t *testing.T, mode string) {
	t.Helper()
	steps := sdkRoundtrip(t)
	if mode == "heartbeat" || mode == "no-pong" {
		steps = []sdkStep{steps[0], {"receive", `{"type":"response.created","response":{"id":"pending-silent","status":"in_progress"}}`}}
	}
	upDone := make(chan error, 16)
	var calls atomic.Int32
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		upDone <- sdkUpstream(r, w, steps, mode)
	}))
	t.Cleanup(u.Close)
	cfg := openAITestConfig(u.URL)
	if mode == "heartbeat" || mode == "no-pong" {
		cfg.Timeouts.Connect, cfg.Timeouts.FirstByte = 500*time.Millisecond, time.Second
		cfg.Timeouts.Idle, cfg.Timeouts.Total = 800*time.Millisecond, 2*time.Second
	}
	b, reg := openAITestBuild(t, cfg, false)
	gs, gatewayDone := openAITestServer(t, b, nil)
	cert, ca := sdkCertificate(t)
	target, err := url.Parse(gs.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	// 出站为纯本地 HTTP；不用系统代理，防环境变量把离线轨迹送出机器。
	transport := &http.Transport{}
	proxy.Transport = transport
	proxyDone := make(chan struct{}, 16)
	var proxyCalls atomic.Int32
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { proxyDone <- struct{}{} }()
		proxyCalls.Add(1)
		if r.TLS == nil || r.TLS.ServerName != "localhost" || r.ProtoMajor != 1 || r.Header.Get("Upgrade") != "websocket" || r.Header.Get("Authorization") != "Bearer sk-test-1234567890" {
			t.Error("SDK 未通过真实 TLS/HTTP1 Upgrade/头鉴权入口")
		}
		for key, want := range map[string]string{"OpenAI-Safety-Identifier": "synthetic-safety", "OpenAI-Organization": "synthetic-org", "OpenAI-Project": "synthetic-project"} {
			if r.Header.Get(key) != want {
				t.Errorf("SDK options.headers 未携带 %s", key)
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	ts.Config.ErrorLog = log.New(io.Discard, "", 0)
	ts.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
	ts.StartTLS()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := b.ShutdownWebSockets(ctx); err != nil {
			t.Error(err)
		}
		ts.Close()
		transport.CloseIdleConnections()
	})
	if mode == "untrusted" {
		_, ca = sdkCertificate(t)
	}
	base := "https://localhost:" + strings.Split(ts.Listener.Addr().String(), ":")[1] + "/v1"
	result := sdkRunNode(t, sdkInput{BaseURL: base, CA: ca, Mode: mode, Steps: steps})
	if result.Mode != mode {
		t.Fatalf("SDK 结果未标识测试模式: %+v", result)
	}
	if mode == "untrusted" {
		if !result.RejectedTLS || result.Opened || result.TLSCode == "" || proxyCalls.Load() != 0 || calls.Load() != 0 || b.wsBudget.Used() != 0 {
			t.Fatalf("不可信 CA 未在 HTTP/上游之前拒绝: %+v proxy=%d upstream=%d", result, proxyCalls.Load(), calls.Load())
		}
		return
	}
	wantReceived, wantSent, wantClose, wantReason := 19, 10, 1000, ""
	if mode == "heartbeat" || mode == "no-pong" {
		wantReceived, wantSent = 2, 0
		if result.Pings < 1 {
			t.Fatal("SDK 未收到真实 transport 心跳")
		}
	}
	if mode == "no-pong" {
		// HTTP reader 取消、relay 空闲错误与心跳写锁竞争都须有限退出；
		// CloseWithResult 遇写锁占用会中断 TCP。沉默端不保证收到礼貌 close。
		if !sdkIdleClose(result.CloseCode, result.CloseReason) || result.OpenMillis < 700 || result.OpenMillis > 5000 {
			t.Fatalf("不回 pong 的客户端未在空闲期限后有限关闭: %+v", result)
		}
		wantClose, wantReason = result.CloseCode, result.CloseReason
	}
	if mode == "heartbeat" && (result.Pings < 6 || result.OpenMillis <= float64(cfg.Timeouts.Total.Milliseconds())) {
		t.Fatal("业务沉默且正常回 pong 的会话未跨过 HTTP total")
	}
	// transport 的被动 close ACK 使用空原因；原 sdk-close 必须抵达另一段上游（下方单独核验）。
	if !result.Opened || result.RejectedTLS || result.Received != wantReceived || result.Sent != wantSent || result.CloseCode != wantClose || result.CloseReason != wantReason {
		t.Fatalf("SDK 原字节/关闭结果错误: %+v", result)
	}
	if err := receiveWSTest(t, upDone); err != nil {
		t.Fatal(err)
	}
	awaitWSTest(t, gatewayDone)
	awaitWSTest(t, proxyDone)
	if calls.Load() != 1 || proxyCalls.Load() != 1 || b.wsBudget.Used() != 0 {
		t.Fatal("SDK 结束后重拨或预算未归零")
	}
	b.wsRegistry.mu.Lock()
	remaining := len(b.wsRegistry.sessions)
	b.wsRegistry.mu.Unlock()
	if remaining != 0 {
		t.Fatal("SDK 退出后会话尚未 join")
	}
	if mode == "heartbeat" || mode == "no-pong" {
		assertOpenAIMetric(t, reg, "omugw_ws_diagnostics_total", map[string]string{"reason": "usage_unfinished"}, 1)
		assertOpenAIMetric(t, reg, "omugw_ws_usage_records_total", map[string]string{"source": "response", "unit": "tokens", "fidelity": "unavailable"}, 1)
		assertOpenAIMetric(t, reg, "omugw_ws_usage_records_total", map[string]string{"fidelity": "authoritative"}, 0)
		return
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
		{"omugw_ws_usage_records_total", map[string]string{"unit": "seconds", "fidelity": "authoritative"}, 2},
		{"omugw_ws_usage_records_total", map[string]string{"unit": "seconds", "fidelity": "unavailable"}, 1},
		{"omugw_ws_audio_input_seconds_total", map[string]string{"source": "transcription"}, 1.25},
		{"omugw_upstream_errors_total", map[string]string{"class": "bad_request"}, 1},
		{"omugw_ws_diagnostics_total", map[string]string{"reason": "usage_unfinished"}, 0},
	} {
		assertOpenAIMetric(t, reg, tc.name, tc.labels, tc.want)
	}
}

func sdkUpstream(r *http.Request, w http.ResponseWriter, steps []sdkStep, mode string) error {
	if r.Method != "GET" || r.URL.Path != "/v1/realtime" || r.URL.RawQuery != "model=real-model" || r.Header.Get("Authorization") != "Bearer sec1" || r.Header.Get("User-Agent") != "omugw" || r.Header.Get("OpenAI-Safety-Identifier") != "synthetic-safety" {
		w.WriteHeader(400)
		return errors.New("SDK 正式 Provider 路径/model/假凭据替换/白名单错误")
	}
	for _, h := range []string{"OpenAI-Organization", "OpenAI-Project", "OpenAI-Beta", "Sec-WebSocket-Protocol", "Sec-WebSocket-Extensions", "Origin", "Cookie", "X-Private"} {
		if r.Header.Get(h) != "" {
			w.WriteHeader(400)
			return fmt.Errorf("SDK 握手越界转发 %s", h)
		}
	}
	c, err := ws.Accept(w, r, ws.AcceptOptions{MaxPayload: 1 << 20, Idle: 5 * time.Second, WriteTimeout: time.Second})
	if err != nil {
		return err
	}
	defer c.Close(1000, "")
	for i, step := range steps {
		if step.Direction == "receive" {
			if err := c.WriteMessage(ws.OpText, []byte(step.Raw)); err != nil {
				return err
			}
		} else {
			op, raw, err := c.ReadMessage()
			if err != nil {
				releaseWSRelayError(err)
				return fmt.Errorf("上游等待 SDK 第 %d 条: %w", i, err)
			}
			if op != ws.OpText || string(raw) != step.Raw {
				return fmt.Errorf("SDK 第 %d 条序列化原字节或顺序改变", i)
			}
		}
	}
	_, _, err = c.ReadMessage()
	var closed *ws.CloseError
	if !errors.As(err, &closed) {
		if mode == "no-pong" && errors.Is(err, io.EOF) {
			return nil
		}
		return fmt.Errorf("SDK 没有完成原生 close: %v", err)
	}
	defer closed.Release()
	wantCode, wantReason := uint16(1000), "sdk-close"
	if mode == "no-pong" {
		if !sdkIdleClose(int(closed.Code), closed.Reason) {
			return fmt.Errorf("沉默关闭未抵达上游: %d %q", closed.Code, closed.Reason)
		}
		return nil
	}
	if closed.Code != wantCode || closed.Reason != wantReason {
		return fmt.Errorf("SDK close 被改写: %d %q", closed.Code, closed.Reason)
	}
	return nil
}

func sdkIdleClose(code int, reason string) bool {
	return (code == 1001 || code == 1006) && reason == "" || code == 1011 && reason == "downstream connection failed"
}

func sdkRunNode(t *testing.T, input sdkInput) sdkResult {
	t.Helper()
	dir, err := filepath.Abs("../../tests/sdk/openai-realtime")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"node_modules/openai/package.json", "node_modules/ws/package.json", "client.mjs"} {
		if _, err := os.Stat(filepath.Join(dir, path)); err != nil {
			t.Fatalf("SDK 依赖/客户端缺失；先在 tests/sdk/openai-realtime 运行 npm ci --ignore-scripts: %v", err)
		}
	}
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", "client.mjs")
	cmd.Dir = dir
	// 子进程只收到本地假凭据与测试 CA，不继承真实 OpenAI 凭据、代理或 TLS 绕过选项。
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "NODE_TLS_REJECT_UNAUTHORIZED=1"}
	cmd.Stdin = bytes.NewReader(data)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		t.Fatalf("SDK 子进程失败（已 Wait）: %v\n%s\n%s", err, stdout.String(), stderr.String())
	}
	if cmd.ProcessState == nil || !cmd.ProcessState.Exited() || ctx.Err() != nil {
		t.Fatal("SDK 子进程未正常 join")
	}
	var result sdkResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("SDK 输出不是有效验收结果: %v\n%s\n%s", err, stdout.String(), stderr.String())
	}
	t.Logf("SDK 子进程已 join: %s", bytes.TrimSpace(stdout.Bytes()))
	return result
}

// 每次生成独立 CA 和 localhost 叶证书，防默认证书/公共根让 TLS 负例虚假通过。
func sdkCertificate(t *testing.T) (tls.Certificate, string) {
	t.Helper()
	newKey := func() *ecdsa.PrivateKey {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return key
	}
	rootKey, leafKey := newKey(), newKey()
	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "omugw SDK 测试 CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	rootDER, err := x509.CreateCertificate(rand.Reader, root, root, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{"localhost"}, NotBefore: root.NotBefore, NotAfter: root.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, root, &leafKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER}))
}
