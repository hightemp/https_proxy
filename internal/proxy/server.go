// Package proxy implements HTTP forwarding and CONNECT tunnelling.
package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/hightemp/https_proxy/internal/auth"
	"github.com/hightemp/https_proxy/internal/config"
	"github.com/hightemp/https_proxy/internal/tunnel"
)

var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Proxy-Connection",
	"TE",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

var bufferPool = sync.Pool{
	New: func() any {
		buffer := make([]byte, 32*1024)
		return &buffer
	},
}

// Server handles authenticated HTTP proxy and CONNECT requests.
type Server struct {
	config        config.Config
	authenticator *auth.Basic
	httpClient    *http.Client
	proxyFunc     func(*http.Request) (*url.URL, error)
	dialer        *net.Dialer
	tunnels       *tunnel.Registry
}

// NewServer validates config and creates a proxy handler.
func NewServer(cfg config.Config) (*Server, error) {
	if err := config.Validate(&cfg); err != nil {
		return nil, err
	}

	proxyFunc, err := buildProxyFunc(cfg.UpstreamProxy)
	if err != nil {
		return nil, err
	}
	network, err := config.DialNetwork(cfg.Network)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: time.Duration(cfg.DialTimeout), KeepAlive: 30 * time.Second}

	server := &Server{
		config:        cfg,
		authenticator: auth.NewBasic(cfg.Username, cfg.Password),
		proxyFunc:     proxyFunc,
		dialer:        dialer,
		tunnels:       tunnel.NewRegistry(),
	}
	transport := &http.Transport{
		Proxy:                 proxyFunc,
		DisableCompression:    true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   time.Duration(cfg.TLSHandshakeTimeout),
		ResponseHeaderTimeout: time.Duration(cfg.ResponseHeaderTimeout),
		ExpectContinueTimeout: time.Second,
		DialContext: func(ctx context.Context, _ string, address string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, address)
		},
	}
	server.httpClient = &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects (>%d) while following %s", len(via), via[len(via)-1].URL.String())
			}
			return nil
		},
	}
	return server, nil
}

// ServeHTTP authenticates and dispatches proxy requests.
func (p *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	slog.Info("Received request", "method", r.Method, "url", r.URL.String())
	if !p.authenticator.Authenticate(w, r) {
		return
	}

	if r.Method == http.MethodConnect {
		p.handleTunneling(w, r)
	} else {
		p.handleHTTP(w, r)
	}
}

func removeHopByHopHeaders(header http.Header) {
	for _, value := range header.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			if name = strings.TrimSpace(name); name != "" {
				header.Del(name)
			}
		}
	}
	for _, name := range hopByHopHeaders {
		header.Del(name)
	}
}

func (p *Server) handleHTTP(w http.ResponseWriter, r *http.Request) {
	outRequest := r.Clone(r.Context())
	outRequest.RequestURI = ""
	outRequest.Host = outRequest.URL.Host
	removeHopByHopHeaders(outRequest.Header)

	response, err := p.httpClient.Do(outRequest)
	if err != nil {
		slog.Error("Error forwarding request", "error", err, "url", r.URL.String())
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer response.Body.Close()

	removeHopByHopHeaders(response.Header)
	for key, values := range response.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(response.StatusCode)

	buffer := bufferPool.Get().(*[]byte)
	defer bufferPool.Put(buffer)
	written, err := io.CopyBuffer(w, response.Body, *buffer)
	if err != nil {
		slog.Error("Error copying response body", "written", written, "error", err)
		return
	}
	slog.Debug("Response copied", "bytes", written, "url", r.URL.String())
}

func (p *Server) handleTunneling(w http.ResponseWriter, r *http.Request) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		slog.Error("Hijacking not supported")
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}

	destination, err := p.dialUpstream(r.Context(), r.Host)
	if err != nil {
		slog.Error("Can't connect to host", "host", r.Host, "error", err)
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	client, readWriter, err := hijacker.Hijack()
	if err != nil {
		slog.Error("Client connection hijack error", "error", err)
		_ = destination.Close()
		return
	}
	if err := client.SetDeadline(time.Time{}); err != nil {
		slog.Debug("Could not clear client connection deadline", "error", err)
	}

	release, tracked := p.tunnels.Track(client, destination)
	if !tracked {
		_ = destination.Close()
		_ = client.Close()
		return
	}
	defer release()
	defer destination.Close()
	defer client.Close()

	if _, err := readWriter.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		slog.Debug("Could not write CONNECT response", "error", err)
		return
	}
	if err := readWriter.Flush(); err != nil {
		slog.Debug("Could not flush CONNECT response", "error", err)
		return
	}

	clientSource := tunnel.NewBufferedConn(client, readWriter.Reader)
	tunnel.Relay(client, clientSource, destination)
}

var _ http.Handler = (*Server)(nil)
