package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/hightemp/https_proxy/internal/config"
)

type resolverFunc func(context.Context, string, string) ([]netip.Addr, error)

func (f resolverFunc) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return f(ctx, network, host)
}

type dialerFunc func(context.Context, string, string) (net.Conn, error)

func (f dialerFunc) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return f(ctx, network, address)
}

func TestDestinationACL(t *testing.T) {
	tests := []struct {
		name         string
		address      string
		allowPrivate bool
		blockedPorts []int
		resolved     []netip.Addr
		wantDenied   bool
	}{
		{name: "public literal", address: "8.8.8.8:443"},
		{name: "loopback", address: "127.0.0.1:443", wantDenied: true},
		{name: "metadata", address: "169.254.169.254:80", wantDenied: true},
		{name: "private", address: "10.0.0.1:443", wantDenied: true},
		{name: "carrier grade NAT", address: "100.64.0.1:443", wantDenied: true},
		{name: "dangerous port", address: "8.8.8.8:22", blockedPorts: []int{22}, wantDenied: true},
		{name: "dangerous port remains blocked for private mode", address: "127.0.0.1:22", allowPrivate: true, blockedPorts: []int{22}, wantDenied: true},
		{name: "private explicitly allowed", address: "127.0.0.1:443", allowPrivate: true},
		{name: "hostname resolving public", address: "public.example:443", resolved: []netip.Addr{netip.MustParseAddr("8.8.8.8")}},
		{name: "hostname resolving private", address: "private.example:443", resolved: []netip.Addr{netip.MustParseAddr("10.0.0.1")}, wantDenied: true},
		{name: "hostname with mixed addresses", address: "mixed.example:443", resolved: []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("10.0.0.1")}, wantDenied: true},
		{name: "localhost hostname", address: "api.localhost:443", wantDenied: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			acl := newDestinationACL(config.Config{
				AllowPrivateDestinations: test.allowPrivate,
				BlockedDestinationPorts:  test.blockedPorts,
			})
			acl.resolver = resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
				return test.resolved, nil
			})

			err := acl.check(context.Background(), test.address)
			if gotDenied := errors.Is(err, errDestinationDenied); gotDenied != test.wantDenied {
				t.Fatalf("check() error = %v, denied = %t, want %t", err, gotDenied, test.wantDenied)
			}
			if !test.wantDenied && err != nil {
				t.Fatalf("check() error = %v", err)
			}
		})
	}
}

func TestProxyRejectsPrivateDestinationByDefault(t *testing.T) {
	proxy, err := NewServer(config.Default())
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:443/private", nil)
	response := httptest.NewRecorder()

	proxy.ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("response status = %d, want %d", response.Code, http.StatusForbidden)
	}
}

func TestDestinationCheckUsesDialTimeout(t *testing.T) {
	cfg := config.Default()
	cfg.DialTimeout = config.Duration(20 * time.Millisecond)
	proxy, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	proxy.destinationACL.resolver = resolverFunc(func(ctx context.Context, _, _ string) ([]netip.Addr, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	request := httptest.NewRequest(http.MethodGet, "http://slow.example/resource", nil)
	response := httptest.NewRecorder()

	proxy.ServeHTTP(response, request)

	if response.Code != http.StatusGatewayTimeout {
		t.Fatalf("response status = %d, want %d", response.Code, http.StatusGatewayTimeout)
	}
}

func TestHTTPDialUsesOnlyTheAddressApprovedByACL(t *testing.T) {
	cfg := config.Default()
	cfg.UpstreamProxy = config.DirectUpstream
	proxy, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	transport := proxy.httpClient.Transport.(*http.Transport)
	t.Cleanup(transport.CloseIdleConnections)

	resolverCalls := 0
	proxy.destinationACL.resolver = resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		resolverCalls++
		if resolverCalls == 1 {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	})

	upstreamResult := make(chan error, 1)
	var dialedAddress string
	proxy.dialer = dialerFunc(func(_ context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" {
			return nil, fmt.Errorf("network = %q, want tcp", network)
		}
		dialedAddress = address
		client, upstream := net.Pipe()
		go func() {
			defer func() { _ = upstream.Close() }()
			request, err := http.ReadRequest(bufio.NewReader(upstream))
			if err != nil {
				upstreamResult <- err
				return
			}
			if request.Host != "rebinding.example" {
				upstreamResult <- fmt.Errorf("Host = %q, want rebinding.example", request.Host)
				return
			}
			_, err = io.WriteString(upstream, "HTTP/1.1 204 No Content\r\nContent-Length: 0\r\n\r\n")
			upstreamResult <- err
		}()
		return client, nil
	})

	request := httptest.NewRequest(http.MethodGet, "http://rebinding.example/resource", nil)
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("response status = %d, want %d", response.Code, http.StatusNoContent)
	}
	if dialedAddress != "8.8.8.8:80" {
		t.Fatalf("dialed address = %q, want approved IP", dialedAddress)
	}
	if resolverCalls != 1 {
		t.Fatalf("resolver calls = %d, want 1", resolverCalls)
	}
	if err := <-upstreamResult; err != nil {
		t.Fatalf("upstream: %v", err)
	}
}
