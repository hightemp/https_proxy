package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hightemp/https_proxy/internal/config"
)

type closeWriteTrackingConn struct {
	net.Conn
	called bool
	err    error
}

func (c *closeWriteTrackingConn) CloseWrite() error {
	c.called = true
	return c.err
}

func TestNewHTTPServerAppliesResourceLimits(t *testing.T) {
	cfg := config.Default()
	cfg.MaxTunnels = 400
	cfg.MaxTunnelsPerIP = 180
	cfg.ReadHeaderTimeout = config.Duration(7 * time.Second)
	cfg.IdleTimeout = config.Duration(8 * time.Second)
	cfg.HTTP2SendPingTimeout = config.Duration(9 * time.Second)
	cfg.HTTP2PingTimeout = config.Duration(10 * time.Second)
	cfg.HTTP2WriteByteTimeout = config.Duration(11 * time.Second)
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
	if server.HTTP2 == nil {
		t.Fatal("HTTP2 config is nil")
	}
	if server.HTTP2.MaxConcurrentStreams != 180 {
		t.Fatalf("HTTP/2 streams = %d, want automatic tunnel limit 180", server.HTTP2.MaxConcurrentStreams)
	}
	if server.HTTP2.SendPingTimeout != 9*time.Second ||
		server.HTTP2.PingTimeout != 10*time.Second ||
		server.HTTP2.WriteByteTimeout != 11*time.Second {
		t.Fatalf(
			"HTTP/2 timeouts = %s/%s/%s, want 9s/10s/11s",
			server.HTTP2.SendPingTimeout,
			server.HTTP2.PingTimeout,
			server.HTTP2.WriteByteTimeout,
		)
	}
}

func TestEffectiveHTTP2MaxConcurrentStreamsUsesExplicitOverride(t *testing.T) {
	cfg := config.Default()
	cfg.HTTP2MaxConcurrentStreams = 64

	if got := effectiveHTTP2MaxConcurrentStreams(cfg); got != 64 {
		t.Fatalf("effectiveHTTP2MaxConcurrentStreams() = %d, want 64", got)
	}
}

func TestRunRejectsInvalidConfiguration(t *testing.T) {
	cfg := config.Default()
	cfg.ProxyAddr = "missing-port"

	err := Run(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "invalid proxy_addr") {
		t.Fatalf("Run() error = %v, want invalid proxy_addr", err)
	}
}

func TestRunFailsWhenListenAddressIsInUse(t *testing.T) {
	occupied := listenLocal(t)
	cfg := config.Default()
	cfg.ProxyAddr = occupied.Addr().String()
	cfg.UpstreamProxy = config.DirectUpstream

	err := Run(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "create listener") {
		t.Fatalf("Run() error = %v, want listener error", err)
	}
}

func TestRunRejectsMissingTLSCertificate(t *testing.T) {
	cfg := config.Default()
	cfg.ProxyAddr = unusedTCPAddress(t)
	cfg.Proto = "https"
	cfg.CertPath = filepath.Join(t.TempDir(), "missing-certificate.pem")
	cfg.KeyPath = filepath.Join(t.TempDir(), "missing-private-key.pem")
	cfg.UpstreamProxy = config.DirectUpstream

	err := Run(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "load certificate") {
		t.Fatalf("Run() error = %v, want certificate load error", err)
	}
}

func TestRunClosesActiveTunnelDuringShutdown(t *testing.T) {
	target := listenLocal(t)
	targetConnection := make(chan net.Conn, 1)
	targetError := make(chan error, 1)
	go func() {
		connection, err := target.Accept()
		if err != nil {
			targetError <- err
			return
		}
		targetConnection <- connection
	}()

	cfg := config.Default()
	cfg.ProxyAddr = unusedTCPAddress(t)
	cfg.UpstreamProxy = config.DirectUpstream
	cfg.AllowPrivateDestinations = true
	cfg.BlockedDestinationPorts = nil
	cfg.LogSensitiveData = true
	cfg.ShutdownTimeout = config.Duration(2 * time.Second)
	runContext, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	runResult := make(chan error, 1)
	go func() {
		runResult <- Run(runContext, cfg)
	}()
	waitForProxyListener(t, cfg.ProxyAddr, runResult)

	client, err := net.DialTimeout("tcp", cfg.ProxyAddr, testIOTimeout)
	if err != nil {
		t.Fatalf("dial running proxy: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := client.SetDeadline(time.Now().Add(testIOTimeout)); err != nil {
		t.Fatalf("set client deadline: %v", err)
	}
	connectRequest := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target.Addr(), target.Addr())
	if _, err := io.WriteString(client, connectRequest); err != nil {
		t.Fatalf("write CONNECT request: %v", err)
	}
	reader := bufio.NewReader(client)
	status, err := readConnectResponse(reader)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want %d", status, http.StatusOK)
	}

	var destination net.Conn
	select {
	case destination = <-targetConnection:
		t.Cleanup(func() { _ = destination.Close() })
	case err := <-targetError:
		t.Fatalf("target accept: %v", err)
	case <-time.After(testIOTimeout):
		t.Fatal("target did not accept tunnel")
	}

	cancelRun()
	select {
	case err := <-runResult:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(testIOTimeout):
		t.Fatal("Run() did not finish after context cancellation")
	}

	if err := destination.SetReadDeadline(time.Now().Add(testIOTimeout)); err != nil {
		t.Fatalf("set destination read deadline: %v", err)
	}
	assertConnectionClosed(t, reader, "proxy client")
	assertConnectionClosed(t, bufio.NewReader(destination), "tunnel destination")
}

func unusedTCPAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve TCP address: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release TCP address: %v", err)
	}
	return address
}

func waitForProxyListener(t *testing.T, address string, runResult <-chan error) {
	t.Helper()
	deadline := time.Now().Add(testIOTimeout)
	for time.Now().Before(deadline) {
		connection, err := net.DialTimeout("tcp", address, 25*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			return
		}
		select {
		case runErr := <-runResult:
			t.Fatalf("Run() stopped before accepting connections: %v", runErr)
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatalf("proxy did not listen on %s", address)
}

func assertConnectionClosed(t *testing.T, reader io.Reader, name string) {
	t.Helper()
	if _, err := reader.Read(make([]byte, 1)); err == nil {
		t.Fatalf("%s remains open after shutdown", name)
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

func TestMakeTLSListenerUsesCertificateReloader(t *testing.T) {
	directory := t.TempDir()
	certificatePath := filepath.Join(directory, "certificate.pem")
	keyPath := filepath.Join(directory, "private-key.pem")
	writeTestCertificatePair(t, certificatePath, keyPath, 1)
	cfg := config.Default()
	cfg.ProxyAddr = "127.0.0.1:0"
	cfg.Proto = "https"
	cfg.CertPath = certificatePath
	cfg.KeyPath = keyPath
	server := newHTTPServer(cfg, http.NotFoundHandler())

	listener, err := makeListener(cfg, server)
	if err != nil {
		t.Fatalf("makeListener() error = %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if server.TLSConfig == nil || server.TLSConfig.GetCertificate == nil {
		t.Fatal("TLS certificate reload callback is not configured")
	}
	if len(server.TLSConfig.NextProtos) != 2 ||
		server.TLSConfig.NextProtos[0] != "h2" ||
		server.TLSConfig.NextProtos[1] != "http/1.1" {
		t.Fatalf("TLS ALPN protocols = %v, want [h2 http/1.1]", server.TLSConfig.NextProtos)
	}
	if len(server.TLSConfig.Certificates) != 0 {
		t.Fatal("static TLS certificate configured alongside reload callback")
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

func TestLimitedConnForwardsOptionalCloseWrite(t *testing.T) {
	closeWriteError := errors.New("close write failed")
	base, peer := net.Pipe()
	t.Cleanup(func() {
		_ = base.Close()
		_ = peer.Close()
	})
	forwarding := &closeWriteTrackingConn{Conn: base, err: closeWriteError}
	connection := &limitedConn{Conn: forwarding, release: func() {}}
	if err := connection.CloseWrite(); !errors.Is(err, closeWriteError) {
		t.Fatalf("CloseWrite() error = %v, want %v", err, closeWriteError)
	}
	if !forwarding.called {
		t.Fatal("CloseWrite() was not forwarded")
	}

	unsupported, unsupportedPeer := net.Pipe()
	t.Cleanup(func() {
		_ = unsupported.Close()
		_ = unsupportedPeer.Close()
	})
	connection = &limitedConn{Conn: unsupported, release: func() {}}
	if err := connection.CloseWrite(); err != nil {
		t.Fatalf("optional CloseWrite() error = %v, want nil", err)
	}
}
