//go:build smoke

package smoke_test

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	nativewire "github.com/yobo2u/omugw/internal/protocol/dashscopenative"
)

// gzipMagic 是 gzip 成员头的前两个字节（RFC 1952 §2.3.1）。
//
// 拿它当判据是因为压缩字节与 JSON 之间没有任何中间态：录制体一旦以它开头，
// 存下来的就是一段谁也解释不了的二进制，而它头上仍挂着 application/json。
var gzipMagic = []byte{0x1f, 0x8b}

// encodingCapture 是假源站看到的内容协商结果，或它自身失败的原因。
type encodingCapture struct {
	acceptEncoding string
	err            error
}

// gzipJSON 把一段明文 JSON 压成 gzip 成员，模拟上游启用压缩后真正上线的字节。
func gzipJSON(payload string) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := io.WriteString(zw, payload); err != nil {
		return nil, fmt.Errorf("写入 gzip 流失败: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("关闭 gzip 流失败: %w", err)
	}
	return buf.Bytes(), nil
}

// TestRecordingProxyStoresPlaintextJSONWhenUpstreamNegotiatesGzip 钉住一条录制
// 不变量：无论上游是否压缩，落进快照与回给下游的都必须是明文 JSON。
//
// 防的是转发 Accept-Encoding 引出的连锁反应。网关的 Transport 会替调用方补上
// Accept-Encoding: gzip，代理把它照抄进出站请求后，代理自己的 Transport 就认定
// 压缩是调用方谈的，于是不再透明解压；relay 又只留 Content-Type、丢掉
// Content-Encoding。三步叠起来的结果是一份贴着 application/json 标签的 gzip
// 字节：录制器照存不误，回放时没有任何头提示它被压过，而它既不是合法 JSON，
// 也解不出 request_id——症状要到有人比对 fixture 内容时才现形。
func TestRecordingProxyStoresPlaintextJSONWhenUpstreamNegotiatesGzip(t *testing.T) {
	// Given: 一个只在对方主动谈 gzip 时才压缩的源站。
	const (
		plaintextBody = `{"output":{"text":"你好"},"request_id":"req-gzip-001"}`
		wantRequestID = "req-gzip-001"
		gatewayBody   = `{"model":"qwen-plus","input":{"messages":[{"role":"user","content":"你好"}]}}`
	)

	compressed, err := gzipJSON(plaintextBody)
	if err != nil {
		t.Fatalf("构造 gzip 响应体失败: %v", err)
	}

	// 容量 2 且非阻塞发送：源站写响应失败时还要再报一次，写满就丢弃，
	// 绝不能让服务端 goroutine 卡在 channel 上把测试拖成超时。
	originCh := make(chan encodingCapture, 2)
	send := func(c encodingCapture) {
		select {
		case originCh <- c:
		default:
		}
	}

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			send(encodingCapture{err: fmt.Errorf("读取源站请求体失败: %w", err)})
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}
		accept := r.Header.Get("Accept-Encoding")
		send(encodingCapture{acceptEncoding: accept})

		w.Header().Set("Content-Type", "application/json")
		payload := []byte(plaintextBody)
		// 压不压完全由对方谈出来的 Accept-Encoding 决定，源站绝不凭空压：
		// 否则「代理拿到的是 gzip」就成了源站单方面造出来的假象，这条测试
		// 也就退化成自说自话，证明不了转发那个头有任何后果。
		if strings.Contains(strings.ToLower(accept), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			payload = compressed
		}
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(payload); err != nil {
			send(encodingCapture{err: fmt.Errorf("写出源站响应失败: %w", err)})
		}
	}))
	defer origin.Close()

	proxy, state := newRecordingProxy(t, origin.URL)

	// When: 下游显式带着 Accept-Encoding: gzip 打到录制代理。
	//
	// 显式设置有两重作用：让这个头真的抵达代理（复现网关 Transport 自动补头的
	// 效果），同时让本地 Transport 认定压缩是调用方自己谈的，从而不再替我们
	// 解压——否则读到的永远是解压后的明文，代理究竟回了什么就无从分辨。
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		proxy.URL+nativewire.TextGenerationPath, strings.NewReader(gatewayBody))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("请求录制代理失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	downstream, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取代理响应体失败: %v", err)
	}

	// Then: 源站确实谈成了 gzip。这一条先于其余断言，因为压缩路径没被走到时，
	// 后面每一条都会不劳而获地变绿。
	select {
	case captured := <-originCh:
		if captured.err != nil {
			t.Fatalf("假源站失败: %v", captured.err)
		}
		if !strings.Contains(strings.ToLower(captured.acceptEncoding), "gzip") {
			t.Fatalf("源站 Accept-Encoding = %q，期望含 gzip：压缩根本没被协商出来，"+
				"这条测试就没在验它声称要验的东西", captured.acceptEncoding)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("等待假源站收到请求超时")
	}

	snap := state.Snapshot()
	if snap.Err != nil {
		t.Fatalf("代理处理器出错: %v", snap.Err)
	}
	if snap.Response.Status != http.StatusOK {
		t.Fatalf("快照 response.status = %d，期望 200", snap.Response.Status)
	}

	// Then: 落盘的是明文 JSON，不是压缩流。三条判据递进——魔数说明它是 gzip，
	// json.Valid 说明它当不成 fixture，request_id 说明内容真的还在。
	if bytes.HasPrefix(snap.Response.Body, gzipMagic) {
		t.Errorf("快照 response.body 以 gzip 魔数 %x 开头，存下来的是压缩字节而非 JSON", gzipMagic)
	}
	if !json.Valid(snap.Response.Body) {
		t.Errorf("快照 response.body 不是合法 JSON，前 64 字节为 %.64q", snap.Response.Body)
	}
	var decoded struct {
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(snap.Response.Body, &decoded); err != nil {
		t.Errorf("快照 response.body 解码失败: %v", err)
	} else if decoded.RequestID != wantRequestID {
		t.Errorf("快照 response.body 的 request_id = %q，期望 %q", decoded.RequestID, wantRequestID)
	}

	// Then: 响应头白名单不因压缩而松口。存了 Content-Encoding 等于把「这份 body
	// 被压过」写进 fixture，让下游误以为压缩字节是合法的录制形态。
	for k := range snap.Response.Headers {
		if strings.EqualFold(k, "content-encoding") {
			t.Errorf("快照 response.headers 混入了不该留存的 %q", k)
		}
	}

	// Then: 回给下游的同样是明文 JSON。录制对了但回放是压缩流，依赖代理做在线
	// 断言的用例照样会读到一坨读不懂的字节。
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("下游状态码 = %d，期望 200: %.64q", resp.StatusCode, downstream)
	}
	if bytes.HasPrefix(downstream, gzipMagic) {
		t.Errorf("下游响应体以 gzip 魔数 %x 开头，期望明文 JSON", gzipMagic)
	}
	if !json.Valid(downstream) {
		t.Errorf("下游响应体不是合法 JSON，前 64 字节为 %.64q", downstream)
	}
	if string(downstream) != plaintextBody {
		t.Errorf("下游响应体 = %.64q，期望 %q", downstream, plaintextBody)
	}
}
