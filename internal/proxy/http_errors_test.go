package proxy

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hightemp/https_proxy/internal/config"
)

type controlledResponseBody struct {
	reader   io.Reader
	readErr  error
	closeErr error
	closed   bool
}

func (b *controlledResponseBody) Read(buffer []byte) (int, error) {
	if b.readErr != nil {
		return 0, b.readErr
	}
	return b.reader.Read(buffer)
}

func (b *controlledResponseBody) Close() error {
	b.closed = true
	return b.closeErr
}

func TestProxyRequiresConfiguredAuthentication(t *testing.T) {
	cfg := config.Default()
	cfg.Username = "proxy-user"
	cfg.Password = "proxy-password"
	server, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://example.test/resource", nil)
	response := httptest.NewRecorder()

	server.ServeHTTP(response, request)

	if response.Code != http.StatusProxyAuthRequired {
		t.Fatalf("response status = %d, want %d", response.Code, http.StatusProxyAuthRequired)
	}
	if response.Header().Get("Proxy-Authenticate") == "" {
		t.Fatal("Proxy-Authenticate header is missing")
	}
}

func TestHTTPForwardingRejectsOriginFormDestination(t *testing.T) {
	server := newDirectTestProxy(t)
	request := httptest.NewRequest(http.MethodGet, "/origin-form", nil)
	response := httptest.NewRecorder()

	server.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("response status = %d, want %d", response.Code, http.StatusBadRequest)
	}
}

func TestHTTPForwardingClosesBodyOnReadAndCloseFailures(t *testing.T) {
	readError := errors.New("upstream body read failed")
	closeError := errors.New("upstream body close failed")
	tests := []struct {
		name       string
		body       *controlledResponseBody
		wantBody   string
		wantStatus int
	}{
		{
			name:       "read error",
			body:       &controlledResponseBody{reader: strings.NewReader(""), readErr: readError},
			wantStatus: http.StatusOK,
		},
		{
			name:       "close error",
			body:       &controlledResponseBody{reader: strings.NewReader("response body"), closeErr: closeError},
			wantBody:   "response body",
			wantStatus: http.StatusOK,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := newDirectTestProxy(t)
			server.httpClient.Transport = roundTripperFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: test.wantStatus,
					ProtoMajor: 1,
					ProtoMinor: 1,
					Header:     make(http.Header),
					Body:       test.body,
				}, nil
			})
			request := httptest.NewRequest(http.MethodGet, "http://example.test/resource", nil)
			response := httptest.NewRecorder()

			server.ServeHTTP(response, request)

			if response.Code != test.wantStatus {
				t.Fatalf("response status = %d, want %d", response.Code, test.wantStatus)
			}
			if response.Body.String() != test.wantBody {
				t.Fatalf("response body = %q, want %q", response.Body.String(), test.wantBody)
			}
			if !test.body.closed {
				t.Fatal("upstream response body was not closed")
			}
		})
	}
}
