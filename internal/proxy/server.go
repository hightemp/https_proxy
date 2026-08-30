// Package proxy implements HTTP forwarding and CONNECT tunnelling.
package proxy

import (
	"context"
	"errors"
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
	config         config.Config
	authenticator  *auth.Basic
	httpClient     *http.Client
	proxyFunc      func(*http.Request) (*url.URL, error)
	dialer         contextDialer
	network        string
	tunnels        *tunnel.Registry
	tunnelLimiter  *concurrentLimiter
	destinationACL *destinationACL
}

type approvedDestinationContextKey struct{}

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
		config: cfg,
		authenticator: auth.NewBasic(cfg.Username, cfg.Password, auth.BasicOptions{
			LogSensitiveData: cfg.LogSensitiveData,
			MaxFailures:      cfg.AuthMaxFailures,
			FailureWindow:    time.Duration(cfg.AuthFailureWindow),
			BlockDuration:    time.Duration(cfg.AuthBlockDuration),
		}),
		proxyFunc: proxyFunc,
		dialer:    dialer,
		network:   network,
		tunnels:   tunnel.NewRegistry(),
		tunnelLimiter: newConcurrentLimiter(
			cfg.MaxTunnels,
			cfg.MaxTunnelsPerIP,
		),
		destinationACL: newDestinationACL(cfg),
	}
	transport := &http.Transport{
		Proxy:                 proxyFunc,
		DisableCompression:    true,
		MaxIdleConns:          cfg.MaxIdleConns,
		MaxIdleConnsPerHost:   cfg.MaxIdleConnsPerHost,
		IdleConnTimeout:       time.Duration(cfg.IdleConnTimeout),
		TLSHandshakeTimeout:   time.Duration(cfg.TLSHandshakeTimeout),
		ResponseHeaderTimeout: time.Duration(cfg.ResponseHeaderTimeout),
		ExpectContinueTimeout: time.Second,
		DialContext: func(ctx context.Context, _ string, address string) (net.Conn, error) {
			if approved, ok := ctx.Value(approvedDestinationContextKey{}).(approvedDestination); ok && approved.matches(address) {
				return server.dialApprovedDestination(ctx, approved)
			}
			return server.dialDestination(ctx, address)
		},
	}
	server.httpClient = &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return server, nil
}

// ServeHTTP authenticates and dispatches proxy requests.
func (p *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	slog.Info("Received request", "method", r.Method, "url", p.requestURLForLog(r))
	if !p.authenticator.Authenticate(w, r) {
		return
	}

	if r.Method == http.MethodConnect {
		p.handleTunneling(w, r)
	} else {
		p.handleHTTP(w, r)
	}
}

func (p *Server) requestURLForLog(r *http.Request) string {
	if p.config.LogSensitiveData {
		return r.URL.String()
	}

	redacted := *r.URL
	redacted.User = nil
	redacted.RawQuery = ""
	redacted.ForceQuery = false
	redacted.Fragment = ""
	redacted.RawFragment = ""
	redacted.Opaque = ""
	return redacted.String()
}

func (p *Server) errorForLog(err error) any {
	if p.config.LogSensitiveData {
		return err
	}
	return http.StatusText(upstreamErrorStatus(err))
}

func upstreamErrorStatus(err error) int {
	var networkError net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &networkError) && networkError.Timeout() {
		return http.StatusGatewayTimeout
	}
	return http.StatusBadGateway
}

func writeUpstreamError(w http.ResponseWriter, err error) {
	statusCode := upstreamErrorStatus(err)
	http.Error(w, http.StatusText(statusCode), statusCode)
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
	outboundURL, err := forwardedRequestURL(r)
	if err != nil {
		slog.Warn("Invalid HTTP destination", "url", p.requestURLForLog(r))
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	destination, err := requestDestination(outboundURL)
	if err != nil {
		slog.Warn("Invalid HTTP destination", "url", p.requestURLForLog(r))
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	approved, err := p.approveDestination(r.Context(), destination)
	if err != nil {
		p.writeDestinationError(w, r, err)
		return
	}

	outRequest := r.Clone(r.Context())
	outRequest.URL = outboundURL
	outRequest.RequestURI = ""
	outRequest.Host = outRequest.URL.Host
	outRequest = outRequest.WithContext(context.WithValue(
		outRequest.Context(),
		approvedDestinationContextKey{},
		approved,
	))
	// The incoming body populates r.Trailer when it reaches EOF. Keep the same
	// map so the outgoing transport sees those values at that point as well.
	outRequest.Trailer = r.Trailer
	acceptsTrailers := headerValuesContainToken(r.Header.Values("Te"), "trailers")
	removeHopByHopHeaders(outRequest.Header)
	if p.config.PrivacyMode {
		removePrivacyHeaders(outRequest.Header)
		removePrivacyHeaders(outRequest.Trailer)
		if outRequest.Body != nil && outRequest.Body != http.NoBody {
			outRequest.Body = &privacyTrailerBody{
				ReadCloser: outRequest.Body,
				trailer:    outRequest.Trailer,
			}
		}
	}
	if acceptsTrailers {
		outRequest.Header.Set("Te", "trailers")
	}
	if !p.config.PrivacyMode {
		appendVia(outRequest.Header, r.ProtoMajor, r.ProtoMinor)
	}

	response, err := p.httpClient.Do(outRequest)
	if err != nil {
		if errors.Is(err, errDestinationDenied) {
			p.writeDestinationError(w, r, err)
			return
		}
		slog.Error("Error forwarding request", "error", p.errorForLog(err), "url", p.requestURLForLog(r))
		writeUpstreamError(w, err)
		return
	}
	removeHopByHopHeaders(response.Header)
	if p.config.PrivacyMode {
		removePrivacyHeaders(response.Header)
		removePrivacyHeaders(response.Trailer)
	} else {
		appendVia(response.Header, response.ProtoMajor, response.ProtoMinor)
	}
	copyHeaders(w.Header(), response.Header)
	announcedTrailers := announceTrailers(w.Header(), response.Trailer)
	w.WriteHeader(response.StatusCode)

	buffer := bufferPool.Get().(*[]byte)
	defer bufferPool.Put(buffer)
	written, copyErr := io.CopyBuffer(w, response.Body, *buffer)
	closeErr := response.Body.Close()
	if copyErr != nil {
		slog.Error("Error copying response body", "written", written, "error", copyErr)
		return
	}
	if closeErr != nil {
		slog.Debug("Could not close upstream response body", "error", closeErr)
	}
	if p.config.PrivacyMode {
		removePrivacyHeaders(response.Trailer)
	}

	if len(response.Trailer) > 0 {
		// Force chunked framing when an unannounced trailer appeared after a
		// short body that net/http could otherwise send with Content-Length.
		_ = http.NewResponseController(w).Flush()
		copyTrailers(w.Header(), response.Trailer, announcedTrailers)
	}
	slog.Debug("Response copied", "bytes", written, "url", p.requestURLForLog(r))
}

func forwardedRequestURL(request *http.Request) (*url.URL, error) {
	forwarded := *request.URL
	if forwarded.Hostname() != "" {
		return &forwarded, nil
	}
	if request.ProtoMajor != 2 || request.Host == "" {
		return nil, errors.New("request destination host is empty")
	}
	forwarded.Scheme = "http"
	forwarded.Host = request.Host
	return &forwarded, nil
}

func (p *Server) handleTunneling(w http.ResponseWriter, r *http.Request) {
	if err := validateTargetAddress(r.Host); err != nil {
		slog.Warn("Invalid CONNECT target", "host", r.Host)
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	releaseTunnel, ok := p.tunnelLimiter.tryAcquire(addressHost(r.RemoteAddr))
	if !ok {
		slog.Warn("Tunnel limit exceeded", "remote", r.RemoteAddr)
		w.Header().Set("Retry-After", "1")
		http.Error(w, http.StatusText(http.StatusTooManyRequests), http.StatusTooManyRequests)
		return
	}
	defer releaseTunnel()
	approved, err := p.approveDestination(r.Context(), r.Host)
	if err != nil {
		p.writeDestinationError(w, r, err)
		return
	}

	var hijacker http.Hijacker
	switch r.ProtoMajor {
	case 1:
		var ok bool
		hijacker, ok = w.(http.Hijacker)
		if !ok {
			slog.Error("Hijacking not supported")
			http.Error(w, "hijacking not supported", http.StatusInternalServerError)
			return
		}
	case 2:
		// HTTP/2 carries a CONNECT tunnel inside one bidirectional stream.
	default:
		http.Error(w, http.StatusText(http.StatusHTTPVersionNotSupported), http.StatusHTTPVersionNotSupported)
		return
	}

	destination, err := p.dialApprovedUpstream(
		r.Context(),
		approved,
		p.viaForRequest(r.Header, r.ProtoMajor, r.ProtoMinor),
	)
	if err != nil {
		if errors.Is(err, errDestinationDenied) {
			p.writeDestinationError(w, r, err)
			return
		}
		slog.Error("Can't connect to host", "host", r.Host, "error", p.errorForLog(err))
		writeUpstreamError(w, err)
		return
	}
	if r.ProtoMajor == 2 {
		p.relayHTTP2Tunnel(w, r, destination)
		return
	}
	p.relayHTTP1Tunnel(hijacker, destination)
}

func (p *Server) viaForRequest(header http.Header, protoMajor, protoMinor int) string {
	if p.config.PrivacyMode {
		return ""
	}
	return forwardedVia(header, protoMajor, protoMinor)
}

func (p *Server) relayHTTP1Tunnel(hijacker http.Hijacker, destination net.Conn) {
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
	defer func() {
		if closeErr := destination.Close(); closeErr != nil {
			slog.Debug("Could not close destination connection", "error", closeErr)
		}
	}()
	defer func() {
		if closeErr := client.Close(); closeErr != nil {
			slog.Debug("Could not close client connection", "error", closeErr)
		}
	}()

	if _, err := readWriter.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		slog.Debug("Could not write CONNECT response", "error", err)
		return
	}
	if err := readWriter.Flush(); err != nil {
		slog.Debug("Could not flush CONNECT response", "error", err)
		return
	}

	clientSource := tunnel.NewBufferedConn(client, readWriter.Reader)
	tunnel.Relay(client, clientSource, destination, time.Duration(p.config.TunnelIdleTimeout))
}

func (p *Server) relayHTTP2Tunnel(w http.ResponseWriter, r *http.Request, destination net.Conn) {
	release, tracked := p.tunnels.Track(r.Body, destination)
	if !tracked {
		_ = destination.Close()
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	defer release()
	defer func() {
		if closeErr := destination.Close(); closeErr != nil {
			slog.Debug("Could not close destination connection", "error", closeErr)
		}
	}()
	defer func() {
		if closeErr := r.Body.Close(); closeErr != nil {
			slog.Debug("Could not close HTTP/2 CONNECT request body", "error", closeErr)
		}
	}()

	w.WriteHeader(http.StatusOK)
	controller := http.NewResponseController(w)
	if err := controller.Flush(); err != nil {
		slog.Debug("Could not flush HTTP/2 CONNECT response", "error", err)
		return
	}
	tunnel.RelayStream(
		w,
		r.Body,
		destination,
		time.Duration(p.config.TunnelIdleTimeout),
		controller.Flush,
	)
}

func (p *Server) approveDestination(ctx context.Context, address string) (approvedDestination, error) {
	checkContext, cancel := context.WithTimeout(ctx, time.Duration(p.config.DialTimeout))
	defer cancel()
	return p.destinationACL.approve(checkContext, p.network, address)
}

func (p *Server) dialDestination(ctx context.Context, address string) (net.Conn, error) {
	dialContext, cancel := context.WithTimeout(ctx, time.Duration(p.config.DialTimeout))
	defer cancel()
	return p.destinationACL.dial(dialContext, p.dialer, p.network, address)
}

func (p *Server) dialApprovedDestination(ctx context.Context, destination approvedDestination) (net.Conn, error) {
	dialContext, cancel := context.WithTimeout(ctx, time.Duration(p.config.DialTimeout))
	defer cancel()
	return dialApproved(dialContext, p.dialer, p.network, destination)
}

func (p *Server) writeDestinationError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errDestinationDenied) {
		logError := any(http.StatusText(http.StatusForbidden))
		if p.config.LogSensitiveData {
			logError = err
		}
		slog.Warn("Destination blocked", "url", p.requestURLForLog(r), "error", logError)
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return
	}
	slog.Error("Could not check destination", "url", p.requestURLForLog(r), "error", p.errorForLog(err))
	writeUpstreamError(w, err)
}

var _ http.Handler = (*Server)(nil)
