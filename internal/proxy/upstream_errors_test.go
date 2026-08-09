package proxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/hightemp/https_proxy/internal/config"
)

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(buffer []byte) (int, error) {
	return f(buffer)
}

func TestBuildProxyFuncConfiguredURL(t *testing.T) {
	tests := []struct {
		name        string
		rawURL      string
		wantURL     string
		wantMessage string
	}{
		{name: "HTTP proxy", rawURL: "http://proxy.example:3128", wantURL: "http://proxy.example:3128"},
		{name: "HTTPS proxy", rawURL: "https://user:password@proxy.example", wantURL: "https://user:password@proxy.example"},
		{name: "invalid proxy", rawURL: "socks5://proxy.example:1080", wantMessage: "invalid upstream_proxy"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			proxyFunc, err := buildProxyFunc(test.rawURL)
			if test.wantMessage != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantMessage) {
					t.Fatalf("buildProxyFunc() error = %v, want %q", err, test.wantMessage)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildProxyFunc() error = %v", err)
			}
			got, err := proxyFunc(&http.Request{URL: &url.URL{Scheme: "https", Host: "origin.example:443"}})
			if err != nil {
				t.Fatalf("proxyFunc() error = %v", err)
			}
			if got.String() != test.wantURL {
				t.Fatalf("proxyFunc() = %q, want %q", got, test.wantURL)
			}
		})
	}
}

func TestBuildProxyFuncEnvironmentSelection(t *testing.T) {
	selectorError := errors.New("environment proxy failed")
	tests := []struct {
		name        string
		selector    func(*http.Request) (*url.URL, error)
		wantURL     string
		wantMessage string
	}{
		{
			name:     "no environment proxy",
			selector: func(*http.Request) (*url.URL, error) { return nil, nil },
		},
		{
			name: "valid environment proxy",
			selector: func(*http.Request) (*url.URL, error) {
				return &url.URL{Scheme: "http", Host: "environment-proxy.example:8080"}, nil
			},
			wantURL: "http://environment-proxy.example:8080",
		},
		{
			name: "selector error",
			selector: func(*http.Request) (*url.URL, error) {
				return nil, selectorError
			},
			wantMessage: selectorError.Error(),
		},
		{
			name: "invalid environment proxy",
			selector: func(*http.Request) (*url.URL, error) {
				return &url.URL{Scheme: "socks5", Host: "environment-proxy.example:1080"}, nil
			},
			wantMessage: "invalid proxy from environment",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			proxyFunc, err := buildProxyFuncWithEnvironment("", test.selector)
			if err != nil {
				t.Fatalf("buildProxyFuncWithEnvironment() error = %v", err)
			}
			got, err := proxyFunc(&http.Request{URL: &url.URL{Scheme: "https", Host: "origin.example:443"}})
			if test.wantMessage != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantMessage) {
					t.Fatalf("proxyFunc() error = %v, want %q", err, test.wantMessage)
				}
				return
			}
			if err != nil {
				t.Fatalf("proxyFunc() error = %v", err)
			}
			if got == nil {
				if test.wantURL != "" {
					t.Fatalf("proxyFunc() = nil, want %q", test.wantURL)
				}
				return
			}
			if got.String() != test.wantURL {
				t.Fatalf("proxyFunc() = %q, want %q", got, test.wantURL)
			}
		})
	}
}

func TestDialUpstreamRejectsInvalidSetup(t *testing.T) {
	selectorError := errors.New("proxy selector failed")
	tests := []struct {
		name        string
		target      string
		mutate      func(*Server)
		wantMessage string
	}{
		{name: "invalid target", target: "missing-port", wantMessage: "invalid CONNECT target"},
		{
			name:   "proxy selector error",
			target: "example.test:443",
			mutate: func(server *Server) {
				server.proxyFunc = func(*http.Request) (*url.URL, error) { return nil, selectorError }
			},
			wantMessage: "select upstream proxy",
		},
		{
			name:   "invalid selected proxy",
			target: "example.test:443",
			mutate: func(server *Server) {
				server.proxyFunc = func(*http.Request) (*url.URL, error) {
					return &url.URL{Scheme: "socks5", Host: "proxy.example:1080"}, nil
				}
			},
			wantMessage: "invalid selected upstream proxy",
		},
		{
			name:   "invalid network",
			target: "example.test:443",
			mutate: func(server *Server) {
				server.config.Network = "udp"
			},
			wantMessage: "network must be",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := newDirectTestProxy(t)
			if test.mutate != nil {
				test.mutate(server)
			}
			_, err := server.dialUpstream(context.Background(), test.target, testViaHeader)
			if err == nil || !strings.Contains(err.Error(), test.wantMessage) {
				t.Fatalf("dialUpstream() error = %v, want %q", err, test.wantMessage)
			}
		})
	}
}

func TestDialUpstreamReportsConnectionFailures(t *testing.T) {
	tests := []struct {
		name        string
		configure   func(*config.Config, string)
		wantMessage string
	}{
		{
			name: "direct destination",
			configure: func(cfg *config.Config, _ string) {
				cfg.UpstreamProxy = config.DirectUpstream
			},
			wantMessage: "connect",
		},
		{
			name: "upstream proxy",
			configure: func(cfg *config.Config, unavailableAddress string) {
				cfg.UpstreamProxy = "http://" + unavailableAddress
			},
			wantMessage: "dial upstream proxy",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			unavailableAddress := unusedTCPAddress(t)
			cfg := config.Default()
			test.configure(&cfg, unavailableAddress)
			server, err := NewServer(cfg)
			if err != nil {
				t.Fatalf("NewServer() error = %v", err)
			}
			target := unavailableAddress
			if cfg.UpstreamProxy != config.DirectUpstream {
				target = "example.test:443"
			}

			_, err = server.dialUpstream(context.Background(), target, testViaHeader)
			if err == nil || !strings.Contains(err.Error(), test.wantMessage) {
				t.Fatalf("dialUpstream() error = %v, want %q", err, test.wantMessage)
			}
		})
	}
}

func TestDialUpstreamRejectsProxyResponses(t *testing.T) {
	tests := []struct {
		name        string
		response    string
		wantMessage string
	}{
		{name: "authentication required", response: "HTTP/1.1 407 Proxy Authentication Required\r\n\r\n", wantMessage: "returned 407"},
		{name: "malformed status", response: "NOT-HTTP 200 OK\r\n\r\n", wantMessage: "invalid CONNECT response"},
		{name: "invalid status code", response: "HTTP/1.1 invalid\r\n\r\n", wantMessage: "invalid upstream status code"},
		{name: "truncated headers", response: "HTTP/1.1 200 OK\r\nX-Incomplete: yes", wantMessage: "read header from upstream"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstream := listenLocal(t)
			upstreamResult := make(chan error, 1)
			go func() {
				connection, err := upstream.Accept()
				if err != nil {
					upstreamResult <- err
					return
				}
				defer func() { _ = connection.Close() }()
				if _, err := http.ReadRequest(bufio.NewReader(connection)); err != nil {
					upstreamResult <- err
					return
				}
				_, err = io.WriteString(connection, test.response)
				upstreamResult <- err
			}()

			cfg := config.Default()
			cfg.UpstreamProxy = "http://" + upstream.Addr().String()
			server, err := NewServer(cfg)
			if err != nil {
				t.Fatalf("NewServer() error = %v", err)
			}
			_, err = server.dialUpstream(context.Background(), "example.test:443", testViaHeader)
			if err == nil || !strings.Contains(err.Error(), test.wantMessage) {
				t.Fatalf("dialUpstream() error = %v, want %q", err, test.wantMessage)
			}
			if err := <-upstreamResult; err != nil {
				t.Fatalf("upstream: %v", err)
			}
		})
	}
}

func TestReadConnectResponseRejectsMalformedResponses(t *testing.T) {
	tests := []struct {
		name        string
		response    string
		wantMessage string
	}{
		{name: "empty", wantMessage: "read status line"},
		{name: "missing HTTP version", response: "200 OK\r\n\r\n", wantMessage: "invalid CONNECT response"},
		{name: "status below range", response: "HTTP/1.1 99 Invalid\r\n\r\n", wantMessage: "invalid upstream status code"},
		{name: "status above range", response: "HTTP/1.1 1000 Invalid\r\n\r\n", wantMessage: "invalid upstream status code"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := readConnectResponse(bufio.NewReader(strings.NewReader(test.response)))
			if err == nil || !strings.Contains(err.Error(), test.wantMessage) {
				t.Fatalf("readConnectResponse() error = %v, want %q", err, test.wantMessage)
			}
		})
	}
}

func TestWriteAllReportsWriterFailures(t *testing.T) {
	writeError := errors.New("write failed")
	tests := []struct {
		name  string
		write writerFunc
		want  error
	}{
		{name: "zero write", write: func([]byte) (int, error) { return 0, nil }, want: io.ErrShortWrite},
		{name: "writer error", write: func([]byte) (int, error) { return 0, writeError }, want: writeError},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := writeAll(test.write, []byte("payload"))
			if !errors.Is(err, test.want) {
				t.Fatalf("writeAll() error = %v, want %v", err, test.want)
			}
		})
	}
}
