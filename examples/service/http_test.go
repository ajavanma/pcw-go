package main

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A net.Pipe connection exercises the real HTTP server, connection deadlines
// and shutdown without requiring a TCP port for the Go-only tests.
type pipeListener struct {
	connection chan net.Conn
	closed     chan struct{}
	once       sync.Once
	address    net.Addr
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.connection:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return l.address }

func startPipeServer(t *testing.T, ctx context.Context, handler http.Handler) (net.Conn, <-chan error) {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	listener := &pipeListener{connection: make(chan net.Conn, 1), closed: make(chan struct{}), address: serverConn.LocalAddr()}
	listener.connection <- serverConn
	server := &http.Server{Handler: handler, BaseContext: func(net.Listener) context.Context { return ctx }}
	finished := make(chan error, 1)
	go func() { finished <- serve(ctx, server, listener) }()
	if err := clientConn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
		_ = server.Close()
	})
	return clientConn, finished
}

func readHTTPStatus(t *testing.T, conn net.Conn, want int) {
	t.Helper()
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != want {
		t.Fatalf("HTTP status %d, want %d, body %s (%v)", response.StatusCode, want, body, err)
	}
}

func TestServiceDeadlineIncludesSlowRequestBody(t *testing.T) {
	root := privateRoot(t)
	var calls atomic.Int32
	handler := testService(t, func(context.Context, string, workflowInput) (workflowSummary, error) {
		calls.Add(1)
		return successfulSummary(), nil
	}, root, 1, 50*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn, _ := startPipeServer(t, ctx, handler)
	// The client supplies only one of two promised bytes, leaving body reading
	// blocked until the workflow's connection read deadline expires.
	if _, err := io.WriteString(conn, "POST /v1/workflows HTTP/1.1\r\nHost: service\r\nContent-Type: application/json\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{"); err != nil {
		t.Fatal(err)
	}
	readHTTPStatus(t, conn, http.StatusGatewayTimeout)
	if calls.Load() != 0 {
		t.Fatal("incomplete body reached worker")
	}
	assertNoOutputs(t, root)
}

func TestServiceShutdownCancelsActiveWorkflow(t *testing.T) {
	root := privateRoot(t)
	entered := make(chan struct{})
	workerCanceled := make(chan struct{})
	handler := testService(t, func(ctx context.Context, _ string, _ workflowInput) (workflowSummary, error) {
		close(entered)
		<-ctx.Done()
		close(workerCanceled)
		return workflowSummary{}, ctx.Err()
	}, root, 1, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn, finished := startPipeServer(t, ctx, handler)
	request := strings.NewReader("POST /v1/workflows HTTP/1.1\r\nHost: service\r\nContent-Type: application/json\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{}")
	if _, err := io.Copy(conn, request); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("workflow did not start")
	}
	cancel()
	readHTTPStatus(t, conn, http.StatusServiceUnavailable)
	select {
	case <-workerCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not cancel worker")
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("graceful shutdown failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP server did not finish shutdown")
	}
}
