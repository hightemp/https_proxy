package proxy

import (
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/hightemp/https_proxy/internal/config"
)

func TestNewHTTPServerAppliesResourceLimits(t *testing.T) {
	cfg := config.Default()
	cfg.ReadHeaderTimeout = config.Duration(7 * time.Second)
	cfg.IdleTimeout = config.Duration(8 * time.Second)
	cfg.MaxHeaderBytes = 32 << 10

	server := newHTTPServer(cfg, http.NotFoundHandler())

	if server.ReadHeaderTimeout != 7*time.Second {
		t.Fatalf("ReadHeaderTimeout = %s, want %s", server.ReadHeaderTimeout, 7*time.Second)
	}
	if server.IdleTimeout != 8*time.Second {
		t.Fatalf("IdleTimeout = %s, want %s", server.IdleTimeout, 8*time.Second)
	}
	if server.MaxHeaderBytes != 32<<10 {
		t.Fatalf("MaxHeaderBytes = %d, want %d", server.MaxHeaderBytes, 32<<10)
	}
}

func TestMakeListenerWrapsConnectionLimiter(t *testing.T) {
	cfg := config.Default()
	cfg.ProxyAddr = "127.0.0.1:0"
	server := newHTTPServer(cfg, http.NotFoundHandler())

	listener, err := makeListener(cfg, server)
	if err != nil {
		t.Fatalf("makeListener() error = %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if _, ok := listener.(*limitedListener); !ok {
		t.Fatalf("listener type = %T, want *limitedListener", listener)
	}
}

func TestLimitedListenerRejectsAndReleasesConnections(t *testing.T) {
	baseListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	listener := newLimitedListener(baseListener, 1, 1)
	t.Cleanup(func() { _ = listener.Close() })

	firstClient, err := net.Dial("tcp4", baseListener.Addr().String())
	if err != nil {
		t.Fatalf("dial first client: %v", err)
	}
	t.Cleanup(func() { _ = firstClient.Close() })
	firstServer, err := listener.Accept()
	if err != nil {
		t.Fatalf("accept first client: %v", err)
	}

	secondClient, err := net.Dial("tcp4", baseListener.Addr().String())
	if err != nil {
		t.Fatalf("dial second client: %v", err)
	}
	t.Cleanup(func() { _ = secondClient.Close() })
	type acceptResult struct {
		connection net.Conn
		err        error
	}
	accepted := make(chan acceptResult, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		accepted <- acceptResult{connection: connection, err: acceptErr}
	}()
	if err := secondClient.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set second client deadline: %v", err)
	}
	if _, err := secondClient.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection above limit was not closed")
	}

	if err := firstServer.Close(); err != nil {
		t.Fatalf("close first server connection: %v", err)
	}
	thirdClient, err := net.Dial("tcp4", baseListener.Addr().String())
	if err != nil {
		t.Fatalf("dial third client: %v", err)
	}
	t.Cleanup(func() { _ = thirdClient.Close() })

	select {
	case result := <-accepted:
		if result.err != nil {
			t.Fatalf("accept after release: %v", result.err)
		}
		if err := result.connection.Close(); err != nil {
			t.Fatalf("close accepted connection: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("connection slot was not released")
	}
}
