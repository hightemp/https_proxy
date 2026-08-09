package proxy

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hightemp/https_proxy/internal/config"
)

type hijackRecorder struct {
	*httptest.ResponseRecorder
	connection net.Conn
	readWriter *bufio.ReadWriter
	err        error
}

func (r *hijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return r.connection, r.readWriter, r.err
}

func TestConnectRejectsPrivateDestinationByDefault(t *testing.T) {
	server, err := NewServer(config.Default())
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodConnect, "http://proxy.test", nil)
	request.Host = "127.0.0.1:443"
	response := httptest.NewRecorder()

	server.ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("CONNECT status = %d, want %d", response.Code, http.StatusForbidden)
	}
}

func TestConnectRequiresHijacker(t *testing.T) {
	server := newDirectTestProxy(t)
	request := httptest.NewRequest(http.MethodConnect, "http://proxy.test", nil)
	request.Host = "example.test:443"
	response := httptest.NewRecorder()

	server.ServeHTTP(response, request)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("CONNECT status = %d, want %d", response.Code, http.StatusInternalServerError)
	}
}

func TestConnectReturnsBadGatewayWhenTargetIsUnavailable(t *testing.T) {
	server := newDirectTestProxy(t)
	proxyHTTP := httptest.NewServer(server)
	t.Cleanup(proxyHTTP.Close)
	client := dialTestServer(t, proxyHTTP.URL)
	unavailableAddress := unusedTCPAddress(t)
	request := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", unavailableAddress, unavailableAddress)
	if err := writeAll(client, []byte(request)); err != nil {
		t.Fatalf("write CONNECT request: %v", err)
	}

	status, err := readConnectResponse(bufio.NewReader(client))
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if status != http.StatusBadGateway {
		t.Fatalf("CONNECT status = %d, want %d", status, http.StatusBadGateway)
	}
}

func TestConnectClosesDestinationWhenHijackFails(t *testing.T) {
	target := listenLocal(t)
	targetResult := acceptAndWaitForClose(target)
	server := newDirectTestProxy(t)
	request := httptest.NewRequest(http.MethodConnect, "http://proxy.test", nil)
	request.Host = target.Addr().String()
	hijackError := errors.New("hijack failed")
	response := &hijackRecorder{ResponseRecorder: httptest.NewRecorder(), err: hijackError}

	server.ServeHTTP(response, request)

	if err := waitForConnectionResult(targetResult); err != nil {
		t.Fatalf("destination connection: %v", err)
	}
}

func TestConnectRejectsTunnelAfterRegistryShutdown(t *testing.T) {
	target := listenLocal(t)
	targetResult := acceptAndWaitForClose(target)
	server := newDirectTestProxy(t)
	server.tunnels.CloseAll()
	client, peer := net.Pipe()
	t.Cleanup(func() { _ = peer.Close() })
	response := &hijackRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		connection:       client,
		readWriter:       bufio.NewReadWriter(bufio.NewReader(client), bufio.NewWriter(client)),
	}
	request := httptest.NewRequest(http.MethodConnect, "http://proxy.test", nil)
	request.Host = target.Addr().String()

	server.ServeHTTP(response, request)

	if _, err := peer.Read(make([]byte, 1)); err == nil {
		t.Fatal("hijacked client remains open after tunnel registry rejected it")
	}
	if err := waitForConnectionResult(targetResult); err != nil {
		t.Fatalf("destination connection: %v", err)
	}
}

func TestConnectHandlesResponseWriteFailures(t *testing.T) {
	tests := []struct {
		name       string
		bufferSize int
	}{
		{name: "write", bufferSize: 1},
		{name: "flush", bufferSize: 4096},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target := listenLocal(t)
			targetResult := acceptAndWaitForClose(target)
			server := newDirectTestProxy(t)
			client, peer := net.Pipe()
			t.Cleanup(func() { _ = peer.Close() })
			writeError := errors.New("client write failed")
			failedWriter := writerFunc(func([]byte) (int, error) { return 0, writeError })
			response := &hijackRecorder{
				ResponseRecorder: httptest.NewRecorder(),
				connection:       client,
				readWriter: bufio.NewReadWriter(
					bufio.NewReader(client),
					bufio.NewWriterSize(failedWriter, test.bufferSize),
				),
			}
			request := httptest.NewRequest(http.MethodConnect, "http://proxy.test", nil)
			request.Host = target.Addr().String()

			server.ServeHTTP(response, request)

			if _, err := peer.Read(make([]byte, 1)); err == nil {
				t.Fatal("client remains open after CONNECT response failure")
			}
			if err := waitForConnectionResult(targetResult); err != nil {
				t.Fatalf("destination connection: %v", err)
			}
		})
	}
}

func acceptAndWaitForClose(listener net.Listener) <-chan error {
	result := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			result <- err
			return
		}
		defer func() { _ = connection.Close() }()
		if err := connection.SetReadDeadline(time.Now().Add(testIOTimeout)); err != nil {
			result <- err
			return
		}
		_, err = connection.Read(make([]byte, 1))
		if err == nil {
			result <- errors.New("connection remains open")
			return
		}
		if !errors.Is(err, io.EOF) {
			result <- fmt.Errorf("read closed connection: %w", err)
			return
		}
		result <- nil
	}()
	return result
}

func waitForConnectionResult(result <-chan error) error {
	select {
	case err := <-result:
		return err
	case <-time.After(testIOTimeout):
		return errors.New("timed out waiting for connection to close")
	}
}
