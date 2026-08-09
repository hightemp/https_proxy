package proxy

import (
	"context"
	"errors"
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
