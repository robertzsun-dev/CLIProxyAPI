package api

import (
	"context"
	"net"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestServerStartWithReady_FailedStartupDoesNotSignalReady(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	for _, useTLS := range []bool{false, true} {
		t.Run(map[bool]string{false: "occupied address", true: "invalid TLS"}[useTLS], func(t *testing.T) {
			cfg := &config.Config{Host: "127.0.0.1", Port: 0}
			cfg.TLS.Enable = useTLS
			server := NewServer(cfg, nil, nil, "")
			if !useTLS {
				server.server.Addr = listener.Addr().String()
			}
			ready := false
			if err := server.StartWithReady(func() { ready = true }); err == nil {
				t.Fatal("expected startup failure")
			}
			if ready {
				t.Fatal("failed startup signaled ready")
			}
		})
	}
}

func TestServerStartWithReady_ListenerIsOpen(t *testing.T) {
	server := NewServer(&config.Config{Host: "127.0.0.1", Port: 0}, nil, nil, "")
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- server.StartWithReady(func() { close(ready) }) }()
	<-ready
	server.listenerMu.Lock()
	addr := server.muxBaseListener.Addr().String()
	server.listenerMu.Unlock()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Errorf("ready listener cannot accept connections: %v", err)
	} else {
		_ = conn.Close()
	}
	if err := server.Stop(context.Background()); err != nil {
		t.Errorf("stop ready server: %v", err)
	}
	if err := <-done; err != nil {
		t.Errorf("server exit: %v", err)
	}
}
