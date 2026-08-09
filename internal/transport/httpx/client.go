// Package httpx 是网关的上游 HTTP 客户端。
//
// 它存在的唯一理由是**四层超时必须独立生效**（原则 2.7）。标准库的
// http.Client.Timeout 是一把大锤：它覆盖从建连到读完 body 的全过程，于是一个
// 思考三分钟的推理请求和一个挂死的连接长得一模一样——要么把前者误杀，
// 要么让后者一直占着连接池。
package httpx

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/yobo2u/omugw/internal/canonical"
	"github.com/yobo2u/omugw/internal/config"
)

// Client 是带四层超时的上游客户端。
type Client struct {
	hc       *http.Client
	timeouts config.Timeouts
	now      func() time.Time
}

// New 按超时配置构造客户端。now 可注入以便测试，传 nil 用 time.Now。
func New(t config.Timeouts, now func() time.Time) *Client {
	if now == nil {
		now = time.Now
	}

	tr := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   t.Connect,
			KeepAlive: 30 * time.Second,
		}).DialContext,

		// ResponseHeaderTimeout 就是**上游首字节**这一层：从请求发完到响应头
		// 到达的上限。它与「整体」分开，才能让长推理请求活下来。
		ResponseHeaderTimeout: t.FirstByte,

		TLSHandshakeTimeout: t.Connect,
		ForceAttemptHTTP2:   true,

		// 连接池按上游主机分配。默认的 2 对网关来说太小——所有请求会挤在
		// 两条连接上排队，而排队时间会被记进「首字节耗时」，看起来像上游变慢了。
		MaxIdleConns:        256,
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     90 * time.Second,
	}

	return &Client{
		// 刻意不设 http.Client.Timeout：它会连流式响应的读取一起算进去，
		// 把一个正常的长流掐断。整体上限由调用方的 context 承担。
		hc:       &http.Client{Transport: tr},
		timeouts: t,
		now:      now,
	}
}

// Response 是一次上游响应。
type Response struct {
	*http.Response

	// UpstreamFirstByte 是上游响应头到达的时刻。
	//
	// 它是**换凭据重试**的依据，不是 failover 的禁区。两者常被混为一谈：
	// 网关完全可以收到上游响应头、发现是 429、于是换一条路重试，而客户端
	// 什么都还没看到。真正不可越过的是**下游首字节**——网关向客户端发出
	// 第一个字节之后就不能重试了，否则客户端会收到重复内容（原则 2.4）。
	// 下游首字节由调用方跟踪，这里管不着。
	UpstreamFirstByte time.Time

	// Latency 是从发起请求到上游响应头到达的耗时。
	Latency time.Duration
}

// Do 发起一次上游请求。
//
// 返回的 Body 已经套上空闲超时：两次成功读取之间超过 Idle 就报错。
// 这才是「上游挂死」的真正判据——整体超时到期只说明响应很长。
func (c *Client) Do(ctx context.Context, req *http.Request) (*Response, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeouts.Total)

	start := c.now()
	resp, err := c.hc.Do(req.WithContext(ctx))
	if err != nil {
		cancel()
		return nil, classify(err, ctx, c.timeouts)
	}

	first := c.now()
	// 整体超时的 cancel 挂在 Body 上：只有流真正读完（或出错）才释放，
	// 提前 cancel 会把还在传输的流掐断。
	resp.Body = newIdleBody(resp.Body, c.timeouts.Idle, cancel)

	return &Response{
		Response:          resp,
		UpstreamFirstByte: first,
		Latency:           first.Sub(start),
	}, nil
}

// classify 把传输层错误归到统一错误分类。
//
// 区分三种超时是有意义的：连接超时和首字节超时说明这个上游此刻不可用，
// 换一个凭据或 Provider 有意义；整体超时往往是请求本身太大，换谁都一样。
func classify(err error, ctx context.Context, t config.Timeouts) error {
	var ne net.Error
	if ok := asNetError(err, &ne); ok && ne.Timeout() {
		if ctx.Err() == context.DeadlineExceeded {
			return canonical.Wrapf(err, canonical.ClassUpstreamUnavailable,
				"上游请求超过整体上限 %v", t.Total)
		}
		return canonical.Wrapf(err, canonical.ClassUpstreamUnavailable,
			"上游在 %v 内未返回响应头", t.FirstByte)
	}
	if ctx.Err() == context.Canceled {
		// 客户端主动断开。不是上游的错，也不该重试。
		return canonical.Wrapf(err, canonical.ClassBadRequest, "请求已被取消")
	}
	return canonical.Wrapf(err, canonical.ClassUpstreamUnavailable, "上游请求失败")
}

// idleBody 用一个定时器保证空闲超时能**打断阻塞中的 Read**。
//
// 早先的实现是在每次 Read 之前比较距上次读取的时长——那个写法对它唯一要防的
// 场景是失效的：真实的流转发阻塞在 Read 里等下一块，一旦底层 Read 挂住，
// 那个检查就永远轮不到执行。当时的测试之所以通过，是因为它在两次 Read 之间
// 手动 sleep，恰好绕开了阻塞这件事，给了假信心。
//
// 现在改为 time.AfterFunc 到期即取消整个请求上下文，从而让阻塞中的 Read
// 立刻返回。
type idleBody struct {
	rc      io.ReadCloser
	timeout time.Duration
	timer   *time.Timer
	release func()

	fired  atomic.Bool
	closed atomic.Bool
}

func newIdleBody(rc io.ReadCloser, timeout time.Duration, release func()) *idleBody {
	b := &idleBody{rc: rc, timeout: timeout, release: release}
	b.timer = time.AfterFunc(timeout, func() {
		b.fired.Store(true)
		// 取消上下文，阻塞中的 Read 会因此立刻返回。
		release()
	})
	return b
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.timer.Reset(b.timeout)
	}
	if err != nil && b.fired.Load() {
		// 底层报错的真正原因是空闲超时把上下文取消了。原样把 context canceled
		// 抛给上层，会让人以为是客户端断开——那是两件完全不同的事。
		return n, canonical.Newf(canonical.ClassUpstreamUnavailable,
			"上游流空闲超过 %v 上限", b.timeout)
	}
	return n, err
}

func (b *idleBody) Close() error {
	if b.closed.Swap(true) {
		return nil
	}
	b.timer.Stop()

	err := b.rc.Close()
	if b.release != nil {
		b.release()
	}
	return err
}

// asNetError 是 errors.As 的一层薄封装，独立出来只为让 classify 读起来顺。
func asNetError(err error, target *net.Error) bool {
	for err != nil {
		if ne, ok := err.(net.Error); ok {
			*target = ne
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// Describe 返回超时配置的可读摘要，用于启动日志。
func (c *Client) Describe() string {
	return fmt.Sprintf("connect=%v first_byte=%v total=%v idle=%v",
		c.timeouts.Connect, c.timeouts.FirstByte, c.timeouts.Total, c.timeouts.Idle)
}
