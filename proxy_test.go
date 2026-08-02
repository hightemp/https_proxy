package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const testIOTimeout = 3 * time.Second

func newDirectTestProxy(t *testing.T) *proxyServer {
	t.Helper()
	config := defaultConfig()
	proxy, err := newProxyServer(config)
	if err != nil {
		t.Fatalf("newProxyServer() error = %v", err)
	}
	direct := func(*http.Request) (*url.URL, error) { return nil, nil }
	proxy.proxyFunc = direct
	proxy.httpClient.Transport.(*http.Transport).Proxy = direct
	t.Cleanup(func() {
		proxy.tunnels.closeAll()
		proxy.httpClient.Transport.(*http.Transport).CloseIdleConnections()
	})
	return proxy
}

func listenLocal(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}

func dialTestServer(t *testing.T, serverURL string) *net.TCPConn {
	t.Helper()
	address := strings.TrimPrefix(serverURL, "http://")
	conn, err := net.DialTimeout("tcp4", address, testIOTimeout)
	if err != nil {
		t.Fatalf("dial proxy %s: %v", address, err)
	}
	tcpConn, ok := conn.(*net.TCPConn)
	if !ok {
		_ = conn.Close()
		t.Fatalf("proxy connection type = %T, want *net.TCPConn", conn)
	}
	if err := tcpConn.SetDeadline(time.Now().Add(testIOTimeout)); err != nil {
		_ = tcpConn.Close()
		t.Fatalf("set proxy connection deadline: %v", err)
	}
	t.Cleanup(func() { _ = tcpConn.Close() })
	return tcpConn
}

func TestConnectPreservesBufferedClientData(t *testing.T) {
	target := listenLocal(t)
	targetResult := make(chan error, 1)
	go func() {
		conn, err := target.Accept()
		if err != nil {
			targetResult <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(testIOTimeout))

		payload := make([]byte, len("EARLY-CLIENT-DATA"))
		if _, err := io.ReadFull(conn, payload); err != nil {
			targetResult <- fmt.Errorf("read early payload: %w", err)
			return
		}
		if string(payload) != "EARLY-CLIENT-DATA" {
			targetResult <- fmt.Errorf("payload = %q", payload)
			return
		}
		_, err = io.WriteString(conn, "TARGET-ACK")
		targetResult <- err
	}()

	proxy := newDirectTestProxy(t)
	proxyHTTP := httptest.NewServer(proxy)
	t.Cleanup(proxyHTTP.Close)
	client := dialTestServer(t, proxyHTTP.URL)

	requestAndPayload := fmt.Sprintf(
		"CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\nEARLY-CLIENT-DATA",
		target.Addr(), target.Addr(),
	)
	if err := writeAll(client, []byte(requestAndPayload)); err != nil {
		t.Fatalf("write pipelined CONNECT: %v", err)
	}

	reader := bufio.NewReader(client)
	status, err := readConnectResponse(reader)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want %d", status, http.StatusOK)
	}
	ack := make([]byte, len("TARGET-ACK"))
	if _, err := io.ReadFull(reader, ack); err != nil {
		t.Fatalf("read target acknowledgement: %v", err)
	}
	if string(ack) != "TARGET-ACK" {
		t.Fatalf("acknowledgement = %q", ack)
	}
	if err := <-targetResult; err != nil {
		t.Fatalf("target: %v", err)
	}
}

func TestConnectPreservesResponseAfterClientHalfClose(t *testing.T) {
	target := listenLocal(t)
	targetResult := make(chan error, 1)
	go func() {
		conn, err := target.Accept()
		if err != nil {
			targetResult <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(testIOTimeout))

		payload, err := io.ReadAll(conn)
		if err != nil {
			targetResult <- fmt.Errorf("read request through half-close: %w", err)
			return
		}
		if string(payload) != "complete request" {
			targetResult <- fmt.Errorf("request = %q", payload)
			return
		}
		_, err = io.WriteString(conn, "response after EOF")
		targetResult <- err
	}()

	proxy := newDirectTestProxy(t)
	proxyHTTP := httptest.NewServer(proxy)
	t.Cleanup(proxyHTTP.Close)
	client := dialTestServer(t, proxyHTTP.URL)
	reader := bufio.NewReader(client)

	connectRequest := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target.Addr(), target.Addr())
	if err := writeAll(client, []byte(connectRequest)); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	status, err := readConnectResponse(reader)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want %d", status, http.StatusOK)
	}
	if _, err := io.WriteString(client, "complete request"); err != nil {
		t.Fatalf("write tunneled request: %v", err)
	}
	if err := client.CloseWrite(); err != nil {
		t.Fatalf("half-close client: %v", err)
	}

	response, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read response after half-close: %v", err)
	}
	if string(response) != "response after EOF" {
		t.Fatalf("response = %q", response)
	}
	if err := <-targetResult; err != nil {
		t.Fatalf("target: %v", err)
	}
}

func TestDialUpstreamUsesDecodedCredentialsAndPreservesBufferedData(t *testing.T) {
	upstream := listenLocal(t)
	upstreamResult := make(chan error, 1)
	go func() {
		conn, err := upstream.Accept()
		if err != nil {
			upstreamResult <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(testIOTimeout))

		request, err := http.ReadRequest(bufio.NewReader(conn))
		if err != nil {
			upstreamResult <- fmt.Errorf("read CONNECT request: %w", err)
			return
		}
		if request.Method != http.MethodConnect || request.Host != "example.test:443" {
			upstreamResult <- fmt.Errorf("request = %s %s", request.Method, request.Host)
			return
		}
		expectedAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("user@name:p:ss"))
		if auth := request.Header.Get("Proxy-Authorization"); auth != expectedAuth {
			upstreamResult <- fmt.Errorf("Proxy-Authorization = %q, want %q", auth, expectedAuth)
			return
		}
		_, err = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\nX-Test: yes\r\n\r\nEARLY-UPSTREAM-DATA")
		upstreamResult <- err
	}()

	config := defaultConfig()
	userinfo := url.UserPassword("user@name", "p:ss").String()
	config.UpstreamProxy = fmt.Sprintf("http://%s@%s", userinfo, upstream.Addr())
	proxy, err := newProxyServer(config)
	if err != nil {
		t.Fatalf("newProxyServer() error = %v", err)
	}

	conn, err := proxy.dialUpstream(context.Background(), "example.test:443")
	if err != nil {
		t.Fatalf("dialUpstream() error = %v", err)
	}
	defer conn.Close()
	payload := make([]byte, len("EARLY-UPSTREAM-DATA"))
	if _, err := io.ReadFull(conn, payload); err != nil {
		t.Fatalf("read buffered upstream data: %v", err)
	}
	if string(payload) != "EARLY-UPSTREAM-DATA" {
		t.Fatalf("buffered upstream data = %q", payload)
	}
	if err := <-upstreamResult; err != nil {
		t.Fatalf("upstream: %v", err)
	}
}

func TestDialUpstreamClearsSetupDeadline(t *testing.T) {
	upstream := listenLocal(t)
	upstreamResult := make(chan error, 1)
	go func() {
		conn, err := upstream.Accept()
		if err != nil {
			upstreamResult <- err
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		if _, err := http.ReadRequest(reader); err != nil {
			upstreamResult <- err
			return
		}
		if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			upstreamResult <- err
			return
		}
		time.Sleep(150 * time.Millisecond)
		_, err = io.WriteString(conn, "late data")
		upstreamResult <- err
	}()

	config := defaultConfig()
	config.ResponseHeaderTimeout = Duration(75 * time.Millisecond)
	config.UpstreamProxy = "http://" + upstream.Addr().String()
	proxy, err := newProxyServer(config)
	if err != nil {
		t.Fatalf("newProxyServer() error = %v", err)
	}
	conn, err := proxy.dialUpstream(context.Background(), "example.test:443")
	if err != nil {
		t.Fatalf("dialUpstream() error = %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(testIOTimeout))
	payload := make([]byte, len("late data"))
	if _, err := io.ReadFull(conn, payload); err != nil {
		t.Fatalf("read after setup deadline: %v", err)
	}
	if string(payload) != "late data" {
		t.Fatalf("payload = %q", payload)
	}
	if err := <-upstreamResult; err != nil {
		t.Fatalf("upstream: %v", err)
	}
}

func TestDialUpstreamResponseTimeout(t *testing.T) {
	upstream := listenLocal(t)
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := upstream.Accept()
		if err == nil {
			accepted <- conn
		}
	}()

	config := defaultConfig()
	config.ResponseHeaderTimeout = Duration(75 * time.Millisecond)
	config.UpstreamProxy = "http://" + upstream.Addr().String()
	proxy, err := newProxyServer(config)
	if err != nil {
		t.Fatalf("newProxyServer() error = %v", err)
	}

	started := time.Now()
	_, err = proxy.dialUpstream(context.Background(), "example.test:443")
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("dialUpstream() error = nil, want timeout")
	}
	if elapsed > time.Second {
		t.Fatalf("dialUpstream() took %s, want a bounded timeout", elapsed)
	}
	select {
	case conn := <-accepted:
		_ = conn.Close()
	case <-time.After(testIOTimeout):
		t.Fatal("upstream did not accept connection")
	}
}

func TestDialHTTPSUpstreamHandshakeTimeout(t *testing.T) {
	upstream := listenLocal(t)
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := upstream.Accept()
		if err == nil {
			accepted <- conn
		}
	}()

	config := defaultConfig()
	config.TLSHandshakeTimeout = Duration(75 * time.Millisecond)
	config.ResponseHeaderTimeout = Duration(time.Second)
	config.UpstreamProxy = "https://" + upstream.Addr().String()
	proxy, err := newProxyServer(config)
	if err != nil {
		t.Fatalf("newProxyServer() error = %v", err)
	}

	started := time.Now()
	_, err = proxy.dialUpstream(context.Background(), "example.test:443")
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("dialUpstream() error = nil, want TLS handshake timeout")
	}
	if elapsed > time.Second {
		t.Fatalf("TLS handshake took %s, want a bounded timeout", elapsed)
	}
	select {
	case conn := <-accepted:
		_ = conn.Close()
	case <-time.After(testIOTimeout):
		t.Fatal("upstream did not accept connection")
	}
}

func TestReadConnectResponseRejectsOversizedHeaders(t *testing.T) {
	response := "HTTP/1.1 200 OK\r\nX-Large: " + strings.Repeat("a", maxConnectResponseHeaderBytes) + "\r\n\r\n"
	_, err := readConnectResponse(bufio.NewReader(strings.NewReader(response)))
	if err == nil || !strings.Contains(err.Error(), "exceed") {
		t.Fatalf("readConnectResponse() error = %v, want header limit error", err)
	}
}

func TestTunnelRegistryClosesTrackedConnectionsAndRejectsNew(t *testing.T) {
	registry := newTunnelRegistry()
	client, clientPeer := net.Pipe()
	dest, destPeer := net.Pipe()
	t.Cleanup(func() {
		_ = clientPeer.Close()
		_ = destPeer.Close()
	})
	tracked := &tunnel{client: client, dest: dest}
	if !registry.track(tracked) {
		t.Fatal("track() = false before shutdown")
	}

	registry.closeAll()
	registry.untrack(tracked)
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout)
	defer cancel()
	if err := registry.wait(ctx); err != nil {
		t.Fatalf("wait() error = %v", err)
	}
	if registry.track(&tunnel{}) {
		t.Fatal("track() = true after shutdown")
	}

	buffer := make([]byte, 1)
	if _, err := clientPeer.Read(buffer); err == nil {
		t.Fatal("client peer remains open after registry shutdown")
	}
	if _, err := destPeer.Read(buffer); err == nil {
		t.Fatal("destination peer remains open after registry shutdown")
	}
}

func TestTunnelRegistryShutdownClosesActiveConnect(t *testing.T) {
	target := listenLocal(t)
	targetAccepted := make(chan struct{})
	targetResult := make(chan error, 1)
	go func() {
		conn, err := target.Accept()
		if err != nil {
			targetResult <- err
			return
		}
		defer conn.Close()
		close(targetAccepted)
		_, err = io.Copy(io.Discard, conn)
		targetResult <- err
	}()

	proxy := newDirectTestProxy(t)
	proxyHTTP := httptest.NewServer(proxy)
	t.Cleanup(proxyHTTP.Close)
	client := dialTestServer(t, proxyHTTP.URL)
	reader := bufio.NewReader(client)
	connectRequest := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target.Addr(), target.Addr())
	if err := writeAll(client, []byte(connectRequest)); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	status, err := readConnectResponse(reader)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want %d", status, http.StatusOK)
	}

	select {
	case <-targetAccepted:
	case <-time.After(testIOTimeout):
		t.Fatal("target did not accept tunnel")
	}
	proxy.tunnels.closeAll()
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout)
	defer cancel()
	if err := proxy.tunnels.wait(ctx); err != nil {
		t.Fatalf("wait for active tunnel shutdown: %v", err)
	}
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("client tunnel remains open after registry shutdown")
	}
	if err := <-targetResult; err != nil {
		t.Fatalf("target: %v", err)
	}
}

func TestHTTPForwardingRemovesDynamicHopHeaders(t *testing.T) {
	originResult := make(chan error, 1)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if value := r.Header.Get("X-Remove-Me"); value != "" {
			originResult <- fmt.Errorf("dynamic request hop header = %q", value)
			return
		}
		w.Header().Set("Connection", "X-Response-Hop")
		w.Header().Set("X-Response-Hop", "remove me")
		w.Header().Set("X-End-To-End", "keep me")
		w.WriteHeader(http.StatusNoContent)
		originResult <- nil
	}))
	t.Cleanup(origin.Close)

	proxy := newDirectTestProxy(t)
	proxyHTTP := httptest.NewServer(proxy)
	t.Cleanup(proxyHTTP.Close)
	proxyURL, err := url.Parse(proxyHTTP.URL)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: testIOTimeout}

	request, err := http.NewRequest(http.MethodGet, origin.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	request.Header.Set("Connection", "X-Remove-Me")
	request.Header.Set("X-Remove-Me", "remove me")
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("client.Do() error = %v", err)
	}
	defer response.Body.Close()
	if response.Header.Get("X-Response-Hop") != "" {
		t.Fatalf("dynamic response hop header = %q", response.Header.Get("X-Response-Hop"))
	}
	if value := response.Header.Get("X-End-To-End"); value != "keep me" {
		t.Fatalf("end-to-end response header = %q", value)
	}
	if err := <-originResult; err != nil {
		t.Fatalf("origin: %v", err)
	}
}
