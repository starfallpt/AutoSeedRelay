package server

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/autoseedrelay/relay/internal/config"
)

func TestRunGracefulShutdown(t *testing.T) {
	// Grab a free port, then release it so Run can bind it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	cfg := &config.Config{ListenAddr: addr, LogLevel: "info", DBPath: "data/test.db"}
	srv, err := New(cfg, nil, Deps{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- srv.Run(ctx) }()

	waitListening(t, addr)

	// Cancel the context → Run shuts down gracefully and returns nil.
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run(ctx) returned error = %v, want nil", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run(ctx) did not return after ctx cancellation")
	}

	// The port must be released: a fresh listener should bind immediately.
	ln2, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("port %s not released after shutdown: %v", addr, err)
	}
	ln2.Close()
}

func waitListening(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server did not start listening on %s within 5s", addr)
}
