package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hightemp/https_proxy/internal/config"
)

func TestAuthenticatedHTTP2ConnectTunnel(t *testing.T) {
	target := listenLocal(t)
	targetResult := make(chan error, 1)
	go serveSingleTunnelExchange(target, targetResult)

	directory := t.TempDir()
	certificatePath := filepath.Join(directory, "certificate.pem")
	keyPath := filepath.Join(directory, "private-key.pem")
	writeTestCertificatePair(t, certificatePath, keyPath, 1)

	cfg := config.Default()
	cfg.ProxyAddr = unusedTCPAddress(t)
	cfg.Proto = "https"
	cfg.CertPath = certificatePath
	cfg.KeyPath = keyPath
	cfg.Username = "proxy-user"
	cfg.Password = "proxy-password"
	cfg.UpstreamProxy = config.DirectUpstream
	cfg.AllowPrivateDestinations = true
	cfg.BlockedDestinationPorts = nil
	cfg.TunnelIdleTimeout = config.Duration(time.Second)

	runContext, cancelRun := context.WithCancel(context.Background())
	runResult := make(chan error, 1)
	go func() {
		runResult <- Run(runContext, cfg)
	}()
	waitForProxyListener(t, cfg.ProxyAddr, runResult)
	t.Cleanup(func() {
		cancelRun()
		select {
		case err := <-runResult:
			if err != nil {
				t.Errorf("Run() cleanup error = %v", err)
			}
		case <-time.After(testIOTimeout):
			t.Error("Run() did not stop during cleanup")
		}
	})

	client := newHTTP2ProxyTestClient(t, certificatePath)
	proxyURL := fmt.Sprintf("https://localhost:%s", portOf(t, cfg.ProxyAddr))

	unauthenticated, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodConnect,
		proxyURL,
		nil,
	)
	if err != nil {
		t.Fatalf("create unauthenticated CONNECT request: %v", err)
	}
	unauthenticated.Host = target.Addr().String()
	unauthenticatedResponse, err := client.Do(unauthenticated)
	if err != nil {
		t.Fatalf("perform unauthenticated CONNECT: %v", err)
	}
	if unauthenticatedResponse.ProtoMajor != 2 {
		t.Fatalf("unauthenticated protocol = %s, want HTTP/2", unauthenticatedResponse.Proto)
	}
	if unauthenticatedResponse.StatusCode != http.StatusProxyAuthRequired {
		t.Fatalf(
			"unauthenticated CONNECT status = %d, want %d",
			unauthenticatedResponse.StatusCode,
			http.StatusProxyAuthRequired,
		)
	}
	if err := unauthenticatedResponse.Body.Close(); err != nil {
		t.Fatalf("close unauthenticated response: %v", err)
	}

	requestReader, requestWriter := io.Pipe()
	authenticated, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodConnect,
		proxyURL,
		requestReader,
	)
	if err != nil {
		t.Fatalf("create authenticated CONNECT request: %v", err)
	}
	authenticated.Host = target.Addr().String()
	credentials := base64.StdEncoding.EncodeToString([]byte("proxy-user:proxy-password"))
	authenticated.Header.Set("Proxy-Authorization", "Basic "+credentials)

	response, err := client.Do(authenticated)
	if err != nil {
		t.Fatalf("perform authenticated CONNECT: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.ProtoMajor != 2 {
		t.Fatalf("authenticated protocol = %s, want HTTP/2", response.Proto)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("authenticated CONNECT status = %d, want %d", response.StatusCode, http.StatusOK)
	}

	if _, err := requestWriter.Write([]byte("ping")); err != nil {
		t.Fatalf("write CONNECT request data: %v", err)
	}
	if err := requestWriter.Close(); err != nil {
		t.Fatalf("close CONNECT request stream: %v", err)
	}
	responsePayload := make([]byte, len("pong"))
	if _, err := io.ReadFull(response.Body, responsePayload); err != nil {
		t.Fatalf("read CONNECT response data: %v", err)
	}
	if string(responsePayload) != "pong" {
		t.Fatalf("CONNECT response payload = %q, want pong", responsePayload)
	}

	select {
	case err := <-targetResult:
		if err != nil {
			t.Fatalf("target exchange: %v", err)
		}
	case <-time.After(testIOTimeout):
		t.Fatal("target exchange did not finish")
	}
}

func TestHTTP2ConnectMultiplexesMoreThanHTTP1ProxyLimit(t *testing.T) {
	const tunnelCount = 40

	target := listenLocal(t)
	targetResult := serveTunnelEchoes(target, tunnelCount)

	cfg := config.Default()
	cfg.Username = "proxy-user"
	cfg.Password = "proxy-password"
	cfg.UpstreamProxy = config.DirectUpstream
	cfg.AllowPrivateDestinations = true
	cfg.BlockedDestinationPorts = nil
	cfg.TunnelIdleTimeout = config.Duration(2 * time.Second)
	proxyHandler, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	var proxyConnections atomic.Int32
	proxyTLS := newHTTP2TestServer(proxyHandler, &proxyConnections)
	t.Cleanup(proxyTLS.Close)
	client := proxyTLS.Client()
	client.Timeout = 5 * time.Second

	warmup, err := http.NewRequest(http.MethodConnect, proxyTLS.URL, nil)
	if err != nil {
		t.Fatalf("create warmup CONNECT request: %v", err)
	}
	warmup.Host = "warmup.invalid:443"
	warmupResponse, err := client.Do(warmup)
	if err != nil {
		t.Fatalf("perform warmup CONNECT: %v", err)
	}
	if warmupResponse.ProtoMajor != 2 || warmupResponse.StatusCode != http.StatusProxyAuthRequired {
		t.Fatalf(
			"warmup response = %s %d, want HTTP/2 %d",
			warmupResponse.Proto,
			warmupResponse.StatusCode,
			http.StatusProxyAuthRequired,
		)
	}
	if err := warmupResponse.Body.Close(); err != nil {
		t.Fatalf("close warmup response: %v", err)
	}

	credentials := base64.StdEncoding.EncodeToString([]byte("proxy-user:proxy-password"))
	start := make(chan struct{})
	results := make(chan error, tunnelCount)
	var tunnels sync.WaitGroup
	for index := range tunnelCount {
		tunnels.Add(1)
		go func() {
			defer tunnels.Done()
			<-start
			requestReader, requestWriter := io.Pipe()
			request, requestErr := http.NewRequest(http.MethodConnect, proxyTLS.URL, requestReader)
			if requestErr != nil {
				results <- requestErr
				return
			}
			request.Host = target.Addr().String()
			request.Header.Set("Proxy-Authorization", "Basic "+credentials)
			response, requestErr := client.Do(request)
			if requestErr != nil {
				_ = requestWriter.CloseWithError(requestErr)
				results <- requestErr
				return
			}
			defer func() { _ = response.Body.Close() }()
			if response.ProtoMajor != 2 || response.StatusCode != http.StatusOK {
				_ = requestWriter.Close()
				results <- fmt.Errorf("tunnel %d response = %s %d", index, response.Proto, response.StatusCode)
				return
			}
			payload := byte(index)
			if _, requestErr = requestWriter.Write([]byte{payload}); requestErr != nil {
				results <- requestErr
				return
			}
			if requestErr = requestWriter.Close(); requestErr != nil {
				results <- requestErr
				return
			}
			responsePayload := make([]byte, 1)
			if _, requestErr = io.ReadFull(response.Body, responsePayload); requestErr != nil {
				results <- requestErr
				return
			}
			if responsePayload[0] != payload {
				results <- fmt.Errorf("tunnel %d payload = %d, want %d", index, responsePayload[0], payload)
				return
			}
			results <- nil
		}()
	}
	close(start)
	tunnels.Wait()
	close(results)
	for result := range results {
		if result != nil {
			t.Fatalf("multiplexed CONNECT: %v", result)
		}
	}
	if connections := proxyConnections.Load(); connections != 1 {
		t.Fatalf("proxy TLS connections = %d, want 1 multiplexed HTTP/2 connection", connections)
	}
	select {
	case err := <-targetResult:
		if err != nil {
			t.Fatalf("target echo server: %v", err)
		}
	case <-time.After(testIOTimeout):
		t.Fatal("target echo server did not finish")
	}
}

func newHTTP2ProxyTestClient(t *testing.T, certificatePath string) *http.Client {
	t.Helper()
	certificate, err := os.ReadFile(certificatePath)
	if err != nil {
		t.Fatalf("read proxy certificate: %v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certificate) {
		t.Fatal("append proxy certificate to roots")
	}
	transport := &http.Transport{
		ForceAttemptHTTP2: true,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    roots,
			ServerName: "localhost",
		},
	}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: testIOTimeout}
}

func newHTTP2TestServer(handler http.Handler, connections *atomic.Int32) *httptest.Server {
	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = true
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.StartTLS()
	return server
}

func serveTunnelEchoes(listener net.Listener, count int) <-chan error {
	result := make(chan error, 1)
	go func() {
		var handlers sync.WaitGroup
		errors := make(chan error, count)
		for range count {
			connection, err := listener.Accept()
			if err != nil {
				result <- err
				return
			}
			handlers.Add(1)
			go func() {
				defer handlers.Done()
				defer func() { _ = connection.Close() }()
				if err := connection.SetDeadline(time.Now().Add(testIOTimeout)); err != nil {
					errors <- err
					return
				}
				payload := make([]byte, 1)
				if _, err := io.ReadFull(connection, payload); err != nil {
					errors <- err
					return
				}
				if _, err := connection.Write(payload); err != nil {
					errors <- err
					return
				}
				errors <- nil
			}()
		}
		handlers.Wait()
		close(errors)
		for err := range errors {
			if err != nil {
				result <- err
				return
			}
		}
		result <- nil
	}()
	return result
}

func serveSingleTunnelExchange(listener net.Listener, result chan<- error) {
	connection, err := listener.Accept()
	if err != nil {
		result <- err
		return
	}
	defer func() { _ = connection.Close() }()
	if err := connection.SetDeadline(time.Now().Add(testIOTimeout)); err != nil {
		result <- err
		return
	}
	payload := make([]byte, len("ping"))
	if _, err := io.ReadFull(connection, payload); err != nil {
		result <- err
		return
	}
	if string(payload) != "ping" {
		result <- fmt.Errorf("target payload = %q, want ping", payload)
		return
	}
	if _, err := connection.Write([]byte("pong")); err != nil {
		result <- err
		return
	}
	result <- nil
}

func portOf(t *testing.T, address string) string {
	t.Helper()
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatalf("split address %q: %v", address, err)
	}
	return port
}
