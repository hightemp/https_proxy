package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/hightemp/https_proxy/internal/config"
)

const testIOTimeout = 3 * time.Second
const testViaHeader = "1.1 https_proxy"

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func newDirectTestProxy(t *testing.T) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.AllowPrivateDestinations = true
	cfg.BlockedDestinationPorts = nil
	proxy, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	direct := func(*http.Request) (*url.URL, error) { return nil, nil }
	proxy.proxyFunc = direct
	transport := proxy.httpClient.Transport.(*http.Transport)
	transport.Proxy = direct
	t.Cleanup(func() {
		proxy.tunnels.CloseAll()
		transport.CloseIdleConnections()
	})
	return proxy
}

func TestRequestURLForLog(t *testing.T) {
	request := httptest.NewRequest(
		http.MethodGet,
		"http://attempted-user:wrong-password@example.test/private?token=secret",
		nil,
	)
	tests := []struct {
		name      string
		sensitive bool
		want      string
	}{
		{name: "redacted by default", want: "http://example.test/private"},
		{name: "explicitly enabled", sensitive: true, want: request.URL.String()},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := &Server{config: config.Config{LogSensitiveData: test.sensitive}}
			if got := server.requestURLForLog(request); got != test.want {
				t.Fatalf("requestURLForLog() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestErrorForLog(t *testing.T) {
	secretError := errors.New("secret upstream failure")
	tests := []struct {
		name      string
		sensitive bool
		want      string
	}{
		{name: "redacted by default", want: http.StatusText(http.StatusBadGateway)},
		{name: "explicitly enabled", sensitive: true, want: secretError.Error()},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := &Server{config: config.Config{LogSensitiveData: test.sensitive}}
			if got := fmt.Sprint(server.errorForLog(secretError)); got != test.want {
				t.Fatalf("errorForLog() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestUpstreamErrorStatus(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "generic error", err: errors.New("connection failed"), want: http.StatusBadGateway},
		{name: "deadline", err: context.DeadlineExceeded, want: http.StatusGatewayTimeout},
		{name: "wrapped deadline", err: fmt.Errorf("wait for upstream: %w", context.DeadlineExceeded), want: http.StatusGatewayTimeout},
		{name: "network timeout", err: &net.DNSError{IsTimeout: true}, want: http.StatusGatewayTimeout},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := upstreamErrorStatus(test.err); got != test.want {
				t.Fatalf("upstreamErrorStatus() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestHTTPForwardingReturnsNeutralUpstreamErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "bad gateway", err: errors.New("secret upstream failure"), want: http.StatusBadGateway},
		{name: "gateway timeout", err: context.DeadlineExceeded, want: http.StatusGatewayTimeout},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			proxy := newDirectTestProxy(t)
			proxy.httpClient.Transport = roundTripperFunc(func(*http.Request) (*http.Response, error) {
				return nil, test.err
			})
			request := httptest.NewRequest(
				http.MethodGet,
				"http://attempted-user:wrong-password@example.test/private?token=secret",
				nil,
			)
			response := httptest.NewRecorder()

			proxy.ServeHTTP(response, request)

			if response.Code != test.want {
				t.Fatalf("response status = %d, want %d", response.Code, test.want)
			}
			wantBody := http.StatusText(test.want) + "\n"
			if response.Body.String() != wantBody {
				t.Fatalf("response body = %q, want %q", response.Body.String(), wantBody)
			}
			for _, secret := range []string{"secret upstream failure", "wrong-password", "token=secret"} {
				if strings.Contains(response.Body.String(), secret) {
					t.Fatalf("response body contains sensitive value %q", secret)
				}
			}
		})
	}
}

func TestConnectRejectsInvalidTarget(t *testing.T) {
	proxy := newDirectTestProxy(t)
	request := httptest.NewRequest(http.MethodConnect, "http://proxy.test", nil)
	request.Host = "missing-port"
	response := httptest.NewRecorder()

	proxy.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("response status = %d, want %d", response.Code, http.StatusBadRequest)
	}
	if want := http.StatusText(http.StatusBadRequest) + "\n"; response.Body.String() != want {
		t.Fatalf("response body = %q, want %q", response.Body.String(), want)
	}
}

func TestConcurrentLimiterEnforcesGlobalAndPerKeyLimits(t *testing.T) {
	limiter := newConcurrentLimiter(2, 1)
	releaseFirst, ok := limiter.tryAcquire("first")
	if !ok {
		t.Fatal("first acquisition rejected")
	}
	if _, ok := limiter.tryAcquire("first"); ok {
		t.Fatal("second acquisition for the same key accepted")
	}
	releaseSecond, ok := limiter.tryAcquire("second")
	if !ok {
		t.Fatal("second key acquisition rejected")
	}
	if _, ok := limiter.tryAcquire("third"); ok {
		t.Fatal("acquisition above global limit accepted")
	}

	releaseFirst()
	releaseFirst()
	releaseThird, ok := limiter.tryAcquire("third")
	if !ok {
		t.Fatal("acquisition after release rejected")
	}
	releaseSecond()
	releaseThird()
}

func TestConnectRejectsTunnelLimit(t *testing.T) {
	proxy := newDirectTestProxy(t)
	proxy.tunnelLimiter = newConcurrentLimiter(1, 1)
	request := httptest.NewRequest(http.MethodConnect, "http://proxy.test", nil)
	request.Host = "example.test:443"
	release, ok := proxy.tunnelLimiter.tryAcquire(addressHost(request.RemoteAddr))
	if !ok {
		t.Fatal("could not occupy tunnel limit")
	}
	defer release()
	response := httptest.NewRecorder()

	proxy.ServeHTTP(response, request)

	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("response status = %d, want %d", response.Code, http.StatusTooManyRequests)
	}
	if response.Header().Get("Retry-After") != "1" {
		t.Fatalf("Retry-After = %q, want %q", response.Header().Get("Retry-After"), "1")
	}
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

func TestBuildProxyFuncDirectIgnoresEnvironment(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://proxy.invalid:8080")
	t.Setenv("HTTPS_PROXY", "http://proxy.invalid:8443")
	proxyFunc, err := buildProxyFunc(config.DirectUpstream)
	if err != nil {
		t.Fatalf("buildProxyFunc() error = %v", err)
	}

	request := &http.Request{URL: &url.URL{Scheme: "https", Host: "example.test:443"}}
	upstream, err := proxyFunc(request)
	if err != nil {
		t.Fatalf("proxyFunc() error = %v", err)
	}
	if upstream != nil {
		t.Fatalf("proxyFunc() = %s, want direct connection", upstream)
	}
}

func TestForwardedVia(t *testing.T) {
	tests := []struct {
		name       string
		header     http.Header
		protoMajor int
		protoMinor int
		want       string
	}{
		{
			name:       "new chain",
			protoMajor: 1,
			protoMinor: 1,
			want:       testViaHeader,
		},
		{
			name:       "existing chain",
			header:     http.Header{"Via": {"1.0 first-proxy", "1.1 second-proxy"}},
			protoMajor: 2,
			protoMinor: 0,
			want:       "1.0 first-proxy, 1.1 second-proxy, 2.0 https_proxy",
		},
		{
			name: "connection-specific Via removed",
			header: http.Header{
				"Connection": {"Via"},
				"Via":        {"1.0 remove-me"},
			},
			protoMajor: 1,
			protoMinor: 1,
			want:       testViaHeader,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := forwardedVia(test.header, test.protoMajor, test.protoMinor); got != test.want {
				t.Fatalf("forwardedVia() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestBuildConnectRequestDecodesProxyCredentials(t *testing.T) {
	proxyURL, err := config.ParseProxyURL("https://user%40example:p%3Ass%2Fword@proxy.example")
	if err != nil {
		t.Fatalf("ParseProxyURL() error = %v", err)
	}

	request := buildConnectRequest("origin.example:443", proxyURL, testViaHeader)
	wantCredentials := base64.StdEncoding.EncodeToString([]byte("user@example:p:ss/word"))
	wantHeader := "Proxy-Authorization: Basic " + wantCredentials + "\r\n"
	if !strings.Contains(request, wantHeader) {
		t.Fatalf("CONNECT request does not contain decoded credentials header %q: %q", wantHeader, request)
	}
	if strings.Contains(request, "%40") || strings.Contains(request, "%3A") {
		t.Fatalf("CONNECT request contains percent-encoded credentials: %q", request)
	}
	if !strings.Contains(request, "Via: "+testViaHeader+"\r\n") {
		t.Fatalf("CONNECT request does not contain Via header: %q", request)
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
		if via := request.Header.Get("Via"); via != testViaHeader {
			upstreamResult <- fmt.Errorf("Via = %q, want %q", via, testViaHeader)
			return
		}
		_, err = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\nX-Test: yes\r\n\r\nEARLY-UPSTREAM-DATA")
		upstreamResult <- err
	}()

	cfg := config.Default()
	userinfo := url.UserPassword("user@name", "p:ss").String()
	cfg.UpstreamProxy = fmt.Sprintf("http://%s@%s", userinfo, upstream.Addr())
	proxy, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	conn, err := proxy.dialUpstream(context.Background(), "example.test:443", testViaHeader)
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

	cfg := config.Default()
	cfg.ResponseHeaderTimeout = config.Duration(75 * time.Millisecond)
	cfg.UpstreamProxy = "http://" + upstream.Addr().String()
	proxy, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	conn, err := proxy.dialUpstream(context.Background(), "example.test:443", testViaHeader)
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

	cfg := config.Default()
	cfg.ResponseHeaderTimeout = config.Duration(75 * time.Millisecond)
	cfg.UpstreamProxy = "http://" + upstream.Addr().String()
	proxy, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	started := time.Now()
	_, err = proxy.dialUpstream(context.Background(), "example.test:443", testViaHeader)
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

func TestDialUpstreamStopsWaitingWhenContextIsCanceled(t *testing.T) {
	upstream := listenLocal(t)
	upstreamReady := make(chan net.Conn, 1)
	upstreamError := make(chan error, 1)
	go func() {
		conn, err := upstream.Accept()
		if err != nil {
			upstreamError <- err
			return
		}
		if _, err := http.ReadRequest(bufio.NewReader(conn)); err != nil {
			_ = conn.Close()
			upstreamError <- err
			return
		}
		upstreamReady <- conn
	}()

	cfg := config.Default()
	cfg.ResponseHeaderTimeout = config.Duration(5 * time.Second)
	cfg.UpstreamProxy = "http://" + upstream.Addr().String()
	proxy, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dialResult := make(chan error, 1)
	go func() {
		conn, err := proxy.dialUpstream(ctx, "example.test:443", testViaHeader)
		if conn != nil {
			_ = conn.Close()
		}
		dialResult <- err
	}()

	select {
	case conn := <-upstreamReady:
		t.Cleanup(func() { _ = conn.Close() })
	case err := <-upstreamError:
		t.Fatalf("upstream: %v", err)
	case <-time.After(testIOTimeout):
		t.Fatal("upstream did not receive CONNECT request")
	}

	cancel()
	select {
	case err := <-dialResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("dialUpstream() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("dialUpstream() did not stop after context cancellation")
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

	cfg := config.Default()
	cfg.TLSHandshakeTimeout = config.Duration(75 * time.Millisecond)
	cfg.ResponseHeaderTimeout = config.Duration(time.Second)
	cfg.UpstreamProxy = "https://" + upstream.Addr().String()
	proxy, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}

	started := time.Now()
	_, err = proxy.dialUpstream(context.Background(), "example.test:443", testViaHeader)
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
	proxy.tunnels.CloseAll()
	ctx, cancel := context.WithTimeout(context.Background(), testIOTimeout)
	defer cancel()
	if err := proxy.tunnels.Wait(ctx); err != nil {
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

func TestHTTPForwardingReturnsRedirectWithoutFollowing(t *testing.T) {
	tests := []int{
		http.StatusMovedPermanently,
		http.StatusFound,
		http.StatusSeeOther,
		http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect,
	}

	for _, statusCode := range tests {
		t.Run(http.StatusText(statusCode), func(t *testing.T) {
			destinationHit := make(chan struct{}, 1)
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/destination" {
					destinationHit <- struct{}{}
					w.WriteHeader(http.StatusNoContent)
					return
				}
				w.Header().Set("Location", "/destination")
				w.WriteHeader(statusCode)
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
			client := &http.Client{
				Transport: transport,
				Timeout:   testIOTimeout,
				CheckRedirect: func(*http.Request, []*http.Request) error {
					return http.ErrUseLastResponse
				},
			}

			response, err := client.Get(origin.URL + "/redirect")
			if err != nil {
				t.Fatalf("client.Get() error = %v", err)
			}
			if _, err := io.Copy(io.Discard, response.Body); err != nil {
				t.Fatalf("read response body: %v", err)
			}
			if err := response.Body.Close(); err != nil {
				t.Fatalf("close response body: %v", err)
			}

			if response.StatusCode != statusCode {
				t.Fatalf("response status = %d, want %d", response.StatusCode, statusCode)
			}
			if location := response.Header.Get("Location"); location != "/destination" {
				t.Fatalf("response Location = %q, want %q", location, "/destination")
			}
			select {
			case <-destinationHit:
				t.Fatal("proxy followed redirect instead of returning it")
			default:
			}
		})
	}
}

func TestHTTPForwardingAppendsVia(t *testing.T) {
	originResult := make(chan error, 1)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := strings.Join(r.Header.Values("Via"), ", "); got != "1.0 client-proxy, 1.1 https_proxy" {
			originResult <- fmt.Errorf("request Via = %q", got)
			return
		}
		w.Header().Set("Via", "1.0 origin-proxy")
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
	request.Header.Set("Via", "1.0 client-proxy")
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("client.Do() error = %v", err)
	}
	defer response.Body.Close()

	if err := <-originResult; err != nil {
		t.Fatalf("origin: %v", err)
	}
	if got := strings.Join(response.Header.Values("Via"), ", "); got != "1.0 origin-proxy, 1.1 https_proxy" {
		t.Fatalf("response Via = %q", got)
	}
}

func TestHTTPForwardingPreservesTrailers(t *testing.T) {
	originResult := make(chan error, 1)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err == nil && string(body) != "request body" {
			err = fmt.Errorf("request body = %q", body)
		}
		if err == nil && r.Trailer.Get("X-Request-Checksum") != "request-complete" {
			err = fmt.Errorf("request trailer = %q", r.Trailer.Get("X-Request-Checksum"))
		}
		if err == nil && r.Header.Get("Te") != "trailers" {
			err = fmt.Errorf("request TE = %q", r.Header.Get("Te"))
		}

		w.Header().Set("Trailer", "X-Response-Checksum")
		w.WriteHeader(http.StatusOK)
		if _, writeErr := io.WriteString(w, "response body"); err == nil {
			err = writeErr
		}
		w.Header().Set("X-Response-Checksum", "response-complete")
		w.Header().Set(http.TrailerPrefix+"X-Late-Trailer", "late-value")
		originResult <- err
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

	request, err := http.NewRequest(http.MethodPost, origin.URL, strings.NewReader("request body"))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	request.ContentLength = -1
	request.Header.Set("TE", "trailers")
	request.Trailer = http.Header{"X-Request-Checksum": {"request-complete"}}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("client.Do() error = %v", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if string(body) != "response body" {
		t.Fatalf("response body = %q", body)
	}
	if got := response.Trailer.Get("X-Response-Checksum"); got != "response-complete" {
		t.Fatalf("declared response trailer = %q", got)
	}
	if got := response.Trailer.Get("X-Late-Trailer"); got != "late-value" {
		t.Fatalf("late response trailer = %q", got)
	}
	if err := <-originResult; err != nil {
		t.Fatalf("origin: %v", err)
	}
}

func TestHTTPForwardingPreservesCompression(t *testing.T) {
	var compressed bytes.Buffer
	gzipWriter := gzip.NewWriter(&compressed)
	if _, err := gzipWriter.Write([]byte("compressed response")); err != nil {
		t.Fatalf("compress response: %v", err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}
	compressedPayload := append([]byte(nil), compressed.Bytes()...)

	acceptEncoding := make(chan string, 1)
	originResult := make(chan error, 1)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		acceptEncoding <- r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Encoding", "gzip")
		_, err := w.Write(compressedPayload)
		originResult <- err
	}))
	t.Cleanup(origin.Close)

	proxy := newDirectTestProxy(t)
	proxyHTTP := httptest.NewServer(proxy)
	t.Cleanup(proxyHTTP.Close)
	proxyURL, err := url.Parse(proxyHTTP.URL)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	transport := &http.Transport{
		Proxy:              http.ProxyURL(proxyURL),
		DisableCompression: true,
	}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: testIOTimeout}

	response, err := client.Get(origin.URL)
	if err != nil {
		t.Fatalf("client.Get() error = %v", err)
	}
	defer response.Body.Close()

	if got := <-acceptEncoding; got != "" {
		t.Fatalf("origin Accept-Encoding = %q, want empty", got)
	}
	if got := response.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("response Content-Encoding = %q, want gzip", got)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if !bytes.Equal(body, compressedPayload) {
		t.Fatalf("response body was transformed: got %d bytes, want %d", len(body), len(compressedPayload))
	}
	if err := <-originResult; err != nil {
		t.Fatalf("origin: %v", err)
	}
}
