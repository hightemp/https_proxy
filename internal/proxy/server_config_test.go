package proxy

import (
	"net/http"
	"testing"
	"time"

	"github.com/hightemp/https_proxy/internal/config"
)

func TestNewServerConfiguresHTTPTransportPool(t *testing.T) {
	cfg := config.Default()
	cfg.MaxIdleConns = 240
	cfg.MaxIdleConnsPerHost = 24
	cfg.IdleConnTimeout = config.Duration(3 * time.Minute)

	server, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	transport, ok := server.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("httpClient.Transport = %T, want *http.Transport", server.httpClient.Transport)
	}
	t.Cleanup(transport.CloseIdleConnections)

	limits := []struct {
		name string
		got  int
		want int
	}{
		{name: "global", got: transport.MaxIdleConns, want: cfg.MaxIdleConns},
		{name: "per host", got: transport.MaxIdleConnsPerHost, want: cfg.MaxIdleConnsPerHost},
	}
	for _, tt := range limits {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Fatalf("idle connection limit = %d, want %d", tt.got, tt.want)
			}
		})
	}
	if transport.IdleConnTimeout != time.Duration(cfg.IdleConnTimeout) {
		t.Fatalf("IdleConnTimeout = %s, want %s", transport.IdleConnTimeout, cfg.IdleConnTimeout)
	}
}
