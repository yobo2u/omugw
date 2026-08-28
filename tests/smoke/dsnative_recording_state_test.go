//go:build smoke

package smoke_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/yobo2u/omugw/internal/testkit"
)

// recordingState 保存录制代理捕获到的上游请求与真实响应。
//
// 捕获发生在代理的 HTTP 处理器 goroutine 里，读取发生在测试 goroutine 里，
// 因此每一格都必须过锁：录制器一旦在半途读到一个正在被写入的切片，
// 落盘的 fixture 就是一份谁也复现不出来的残缺证据。
type recordingState struct {
	mu       sync.Mutex
	requests int
	upstream testkit.UpstreamExpectation
	response testkit.Response
	err      error
}

// recordingSnapshot 是录制状态在某一时刻的只读拷贝。
//
// 处理器里的错误必须随快照一起带出来：服务端 goroutine 里 t.Fatal 会让
// panic 跨 goroutine 逃逸，测试拿到的是一次崩溃而不是一条断言失败。
type recordingSnapshot struct {
	Requests int
	Upstream testkit.UpstreamExpectation
	Response testkit.Response
	Err      error
}

// claim 登记一次新请求，并报告本次是否是被允许捕获的那一次。
//
// 一个代理只服务一次捕获：第二次请求若继续写入状态，先前那次的证据会被
// 悄悄顶掉，而录制器对此一无所知，照样把后一次的响应配着前一次的请求存盘。
func (s *recordingState) claim() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.requests++
	if s.requests > 1 {
		if s.err == nil {
			s.err = fmt.Errorf("录制代理收到第 %d 次请求，一个代理只允许捕获一次", s.requests)
		}
		return false
	}
	return true
}

// storeErr 记下处理器里的首个错误。后来的错误多半是首个错误的连锁反应。
func (s *recordingState) storeErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.err == nil {
		s.err = err
	}
}

// setUpstream 登记发往上游的请求预期。传入值在此深拷贝，与调用方彻底断开。
func (s *recordingState) setUpstream(exp testkit.UpstreamExpectation) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.upstream = cloneUpstream(exp)
}

// setResponseHead 登记真实响应的状态码与安全响应头。
func (s *recordingState) setResponseHead(status int, headers map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.response.Status = status
	s.response.Headers = maps.Clone(headers)
}

// setResponseBody 登记非流式响应体。
func (s *recordingState) setResponseBody(body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.response.Body = json.RawMessage(bytes.Clone(body))
}

// appendEvent 追加一条流式事件，并把它记成独立的一帧。
//
// 帧边界取自代理逐事件转发的实际节奏，而不是底层 Read 返回的字节块：
// 一次 Read 里可能躺着半条事件，也可能躺着三条，拿它当帧会把回放时的
// 分片节奏做成假的，而缓冲类 bug 恰恰只在分片边界上现形。
func (s *recordingState) appendEvent(ev testkit.SSEEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.response.SSE == nil {
		s.response.SSE = &testkit.SSEBody{}
	}
	s.response.SSE.Events = append(s.response.SSE.Events, ev)
	s.response.SSE.Frames = append(s.response.SSE.Frames, 1)
}

// Snapshot 返回捕获状态的深拷贝，供测试 goroutine 安全读取。
func (s *recordingState) Snapshot() recordingSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	return recordingSnapshot{
		Requests: s.requests,
		Upstream: cloneUpstream(s.upstream),
		Response: cloneResponse(s.response),
		Err:      s.err,
	}
}

// cloneUpstream 深拷贝上游请求预期，切断字典与字节切片的共享。
func cloneUpstream(exp testkit.UpstreamExpectation) testkit.UpstreamExpectation {
	exp.Headers = maps.Clone(exp.Headers)
	exp.Body = json.RawMessage(bytes.Clone(exp.Body))
	return exp
}

// cloneResponse 深拷贝上游响应，含 SSE 事件与帧边界。
func cloneResponse(resp testkit.Response) testkit.Response {
	resp.Headers = maps.Clone(resp.Headers)
	resp.Body = json.RawMessage(bytes.Clone(resp.Body))
	if resp.SSE != nil {
		resp.SSE = &testkit.SSEBody{
			Events: slices.Clone(resp.SSE.Events),
			Frames: slices.Clone(resp.SSE.Frames),
		}
	}
	return resp
}
