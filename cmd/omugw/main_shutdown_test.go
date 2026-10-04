package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type shutdownWSProbe struct {
	sealed      chan struct{}
	httpStopped chan struct{}
	once        sync.Once
}

func (p *shutdownWSProbe) ShutdownWebSockets(ctx context.Context) error {
	p.once.Do(func() { close(p.sealed) })
	if ctx.Err() != nil {
		return ctx.Err()
	}
	select {
	case <-p.httpStopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestShutdownServersSealsThenJoinsConcurrently(t *testing.T) {
	p := &shutdownWSProbe{sealed: make(chan struct{}), httpStopped: make(chan struct{})}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	srv.Config.RegisterOnShutdown(func() {
		select {
		case <-p.sealed:
		default:
			t.Error("HTTP 停机先于 WS 封口")
		}
		close(p.httpStopped)
	})
	metrics := httptest.NewServer(http.NotFoundHandler())
	defer metrics.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := shutdownServers(ctx, p, srv.Config, metrics.Config); err != nil {
		t.Fatal("WS join 与 HTTP Shutdown 没有共用预算并行完成", err)
	}
}
