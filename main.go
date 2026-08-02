package main

import (
	"bufio"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

const maxConnectResponseHeaderBytes = 64 << 10

type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return errors.New("duration must be a string such as 500ms, 10s, or 2m")
	}
	value, err := time.ParseDuration(node.Value)
	if err != nil {
		return err
	}
	*d = Duration(value)
	return nil
}

func (d Duration) String() string {
	return time.Duration(d).String()
}

type Config struct {
	ProxyAddr             string   `yaml:"proxy_addr"`
	Username              string   `yaml:"username"`
	Password              string   `yaml:"password"`
	Proto                 string   `yaml:"proto"`
	CertPath              string   `yaml:"cert_path"`
	KeyPath               string   `yaml:"key_path"`
	UpstreamProxy         string   `yaml:"upstream_proxy"`
	Network               string   `yaml:"network"`
	DialTimeout           Duration `yaml:"dial_timeout"`
	TLSHandshakeTimeout   Duration `yaml:"tls_handshake_timeout"`
	ResponseHeaderTimeout Duration `yaml:"response_header_timeout"`
	ReadHeaderTimeout     Duration `yaml:"read_header_timeout"`
	IdleTimeout           Duration `yaml:"idle_timeout"`
	ShutdownTimeout       Duration `yaml:"shutdown_timeout"`
}

// Hop-by-hop headers that should not be forwarded by proxies (RFC 7230 §6.1).
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

// Buffer pool to reduce GC pressure during data transfer.
var bufPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 32*1024)
		return &buf
	},
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

func (c *bufferedConn) CloseWrite() error {
	if conn, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return conn.CloseWrite()
	}
	return nil
}

type proxyServer struct {
	config     Config
	httpClient *http.Client
	proxyFunc  func(*http.Request) (*url.URL, error)
	dialer     *net.Dialer
	tunnels    *tunnelRegistry
}

type tunnel struct {
	client net.Conn
	dest   net.Conn
}

type tunnelRegistry struct {
	mu      sync.Mutex
	closing bool
	active  map[*tunnel]struct{}
	wg      sync.WaitGroup
}

func newTunnelRegistry() *tunnelRegistry {
	return &tunnelRegistry{active: make(map[*tunnel]struct{})}
}

func (r *tunnelRegistry) track(t *tunnel) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closing {
		return false
	}
	r.active[t] = struct{}{}
	r.wg.Add(1)
	return true
}

func (r *tunnelRegistry) untrack(t *tunnel) {
	r.mu.Lock()
	if _, ok := r.active[t]; ok {
		delete(r.active, t)
		r.wg.Done()
	}
	r.mu.Unlock()
}

func (r *tunnelRegistry) closeAll() {
	r.mu.Lock()
	r.closing = true
	active := make([]*tunnel, 0, len(r.active))
	for t := range r.active {
		active = append(active, t)
	}
	r.mu.Unlock()

	for _, t := range active {
		_ = t.client.Close()
		_ = t.dest.Close()
	}
}

func (r *tunnelRegistry) wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func main() {
	configPath := flag.String("config", "config.yaml", "Path to the config file")
	flag.Parse()

	config, err := loadConfig(*configPath)
	if err != nil {
		slog.Error("Error loading config file", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, config); err != nil {
		slog.Error("Proxy server stopped with error", "error", err)
		os.Exit(1)
	}
	slog.Info("Server stopped")
}

func defaultConfig() Config {
	return Config{
		ProxyAddr:             "127.0.0.1:8080",
		Proto:                 "http",
		Network:               "auto",
		DialTimeout:           Duration(10 * time.Second),
		TLSHandshakeTimeout:   Duration(10 * time.Second),
		ResponseHeaderTimeout: Duration(30 * time.Second),
		ReadHeaderTimeout:     Duration(15 * time.Second),
		IdleTimeout:           Duration(2 * time.Minute),
		ShutdownTimeout:       Duration(15 * time.Second),
	}
}

func loadConfig(path string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	defer file.Close()

	config, err := decodeConfig(file)
	if err != nil {
		return Config{}, err
	}
	if err := applyEnvOverrides(&config); err != nil {
		return Config{}, err
	}
	if err := validateConfig(&config); err != nil {
		return Config{}, err
	}
	return config, nil
}

func decodeConfig(r io.Reader) (Config, error) {
	config := defaultConfig()
	decoder := yaml.NewDecoder(r)
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}

	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return Config{}, errors.New("parse config: multiple YAML documents are not supported")
		}
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	return config, nil
}

// applyEnvOverrides overrides config fields from environment variables when non-empty.
// Supported variables:
//
//	PROXY_ADDR, PROXY_USERNAME, PROXY_PASSWORD, PROXY_PROTO,
//	PROXY_CERT_PATH, PROXY_KEY_PATH, PROXY_UPSTREAM_PROXY, PROXY_NETWORK,
//	PROXY_DIAL_TIMEOUT, PROXY_TLS_HANDSHAKE_TIMEOUT,
//	PROXY_RESPONSE_HEADER_TIMEOUT, PROXY_READ_HEADER_TIMEOUT,
//	PROXY_IDLE_TIMEOUT, PROXY_SHUTDOWN_TIMEOUT.
func applyEnvOverrides(c *Config) error {
	if v := os.Getenv("PROXY_ADDR"); v != "" {
		c.ProxyAddr = v
	}
	if v := os.Getenv("PROXY_USERNAME"); v != "" {
		c.Username = v
	}
	if v := os.Getenv("PROXY_PASSWORD"); v != "" {
		c.Password = v
	}
	if v := os.Getenv("PROXY_PROTO"); v != "" {
		c.Proto = v
	}
	if v := os.Getenv("PROXY_CERT_PATH"); v != "" {
		c.CertPath = v
	}
	if v := os.Getenv("PROXY_KEY_PATH"); v != "" {
		c.KeyPath = v
	}
	if v := os.Getenv("PROXY_UPSTREAM_PROXY"); v != "" {
		c.UpstreamProxy = v
	}
	if v := os.Getenv("PROXY_NETWORK"); v != "" {
		c.Network = v
	}

	durationOverrides := []struct {
		name string
		dest *Duration
	}{
		{"PROXY_DIAL_TIMEOUT", &c.DialTimeout},
		{"PROXY_TLS_HANDSHAKE_TIMEOUT", &c.TLSHandshakeTimeout},
		{"PROXY_RESPONSE_HEADER_TIMEOUT", &c.ResponseHeaderTimeout},
		{"PROXY_READ_HEADER_TIMEOUT", &c.ReadHeaderTimeout},
		{"PROXY_IDLE_TIMEOUT", &c.IdleTimeout},
		{"PROXY_SHUTDOWN_TIMEOUT", &c.ShutdownTimeout},
	}
	for _, override := range durationOverrides {
		value := os.Getenv(override.name)
		if value == "" {
			continue
		}
		duration, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("parse %s: %w", override.name, err)
		}
		*override.dest = Duration(duration)
	}
	return nil
}

func validateConfig(config *Config) error {
	config.Proto = strings.ToLower(strings.TrimSpace(config.Proto))
	if config.Proto != "http" && config.Proto != "https" {
		return fmt.Errorf("proto must be http or https, got %q", config.Proto)
	}

	config.Network = strings.ToLower(strings.TrimSpace(config.Network))
	if _, err := dialNetwork(config.Network); err != nil {
		return err
	}

	if err := validateListenAddress(config.ProxyAddr); err != nil {
		return fmt.Errorf("invalid proxy_addr: %w", err)
	}
	if config.Proto == "https" && (config.CertPath == "" || config.KeyPath == "") {
		return errors.New("cert_path and key_path are required when proto is https")
	}

	durations := []struct {
		name  string
		value Duration
	}{
		{"dial_timeout", config.DialTimeout},
		{"tls_handshake_timeout", config.TLSHandshakeTimeout},
		{"response_header_timeout", config.ResponseHeaderTimeout},
		{"read_header_timeout", config.ReadHeaderTimeout},
		{"idle_timeout", config.IdleTimeout},
		{"shutdown_timeout", config.ShutdownTimeout},
	}
	for _, duration := range durations {
		if duration.value <= 0 {
			return fmt.Errorf("%s must be greater than zero", duration.name)
		}
	}

	if config.UpstreamProxy != "" {
		if _, err := parseProxyURL(config.UpstreamProxy); err != nil {
			return fmt.Errorf("invalid upstream_proxy: %w", err)
		}
	}
	return nil
}

func validateListenAddress(address string) error {
	_, portText, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	return nil
}

func dialNetwork(network string) (string, error) {
	switch network {
	case "auto":
		return "tcp", nil
	case "tcp4", "tcp6":
		return network, nil
	default:
		return "", fmt.Errorf("network must be auto, tcp4, or tcp6, got %q", network)
	}
}

func parseProxyURL(rawURL string) (*url.URL, error) {
	proxyURL, err := url.Parse(rawURL)
	if err != nil {
		return nil, errors.New("URL cannot be parsed")
	}
	proxyURL.Scheme = strings.ToLower(proxyURL.Scheme)
	if proxyURL.Scheme != "http" && proxyURL.Scheme != "https" {
		return nil, errors.New("scheme must be http or https")
	}
	if proxyURL.Hostname() == "" {
		return nil, errors.New("host is required")
	}
	if proxyURL.Path != "" && proxyURL.Path != "/" {
		return nil, errors.New("path is not allowed")
	}
	if proxyURL.RawQuery != "" || proxyURL.Fragment != "" {
		return nil, errors.New("query and fragment are not allowed")
	}
	if _, err := proxyAddress(proxyURL); err != nil {
		return nil, err
	}
	return proxyURL, nil
}

func proxyAddress(proxyURL *url.URL) (string, error) {
	host := proxyURL.Hostname()
	if host == "" {
		return "", errors.New("host is required")
	}
	port := proxyURL.Port()
	if port == "" {
		switch proxyURL.Scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		default:
			return "", errors.New("scheme must be http or https")
		}
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return "", errors.New("port must be between 1 and 65535")
	}
	return net.JoinHostPort(host, port), nil
}

func newProxyServer(config Config) (*proxyServer, error) {
	if err := validateConfig(&config); err != nil {
		return nil, err
	}

	proxyFunc, err := buildProxyFunc(config.UpstreamProxy)
	if err != nil {
		return nil, err
	}
	network, err := dialNetwork(config.Network)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: time.Duration(config.DialTimeout), KeepAlive: 30 * time.Second}

	proxy := &proxyServer{
		config:    config,
		proxyFunc: proxyFunc,
		dialer:    dialer,
		tunnels:   newTunnelRegistry(),
	}
	transport := &http.Transport{
		Proxy:                 proxyFunc,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   time.Duration(config.TLSHandshakeTimeout),
		ResponseHeaderTimeout: time.Duration(config.ResponseHeaderTimeout),
		ExpectContinueTimeout: time.Second,
		DialContext: func(ctx context.Context, _ string, address string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, address)
		},
	}
	proxy.httpClient = &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects (>%d) while following %s", len(via), via[len(via)-1].URL.String())
			}
			return nil
		},
	}
	return proxy, nil
}

func buildProxyFunc(rawURL string) (func(*http.Request) (*url.URL, error), error) {
	if rawURL != "" {
		proxyURL, err := parseProxyURL(rawURL)
		if err != nil {
			return nil, fmt.Errorf("invalid upstream_proxy: %w", err)
		}
		return http.ProxyURL(proxyURL), nil
	}

	return func(request *http.Request) (*url.URL, error) {
		proxyURL, err := http.ProxyFromEnvironment(request)
		if err != nil || proxyURL == nil {
			return proxyURL, err
		}
		parsed, err := parseProxyURL(proxyURL.String())
		if err != nil {
			return nil, fmt.Errorf("invalid proxy from environment: %w", err)
		}
		return parsed, nil
	}, nil
}

func (p *proxyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	slog.Info("Received request", "method", r.Method, "url", r.URL.String())
	if !p.basicAuth(w, r) {
		return
	}

	if r.Method == http.MethodConnect {
		p.handleTunneling(w, r)
	} else {
		p.handleHTTP(w, r)
	}
}

func (p *proxyServer) basicAuth(w http.ResponseWriter, r *http.Request) bool {
	// Authentication disabled when no credentials are configured.
	if p.config.Username == "" && p.config.Password == "" {
		return true
	}

	auth := r.Header.Get("Proxy-Authorization")
	if auth == "" {
		slog.Debug("No Proxy-Authorization header", "remote", r.RemoteAddr)
		w.Header().Set("Proxy-Authenticate", `Basic realm="Proxy Authorization Required"`)
		w.WriteHeader(http.StatusProxyAuthRequired)
		return false
	}

	scheme, encoded, ok := strings.Cut(auth, " ")
	if !ok || !strings.EqualFold(scheme, "Basic") {
		slog.Warn("Invalid auth scheme", "remote", r.RemoteAddr)
		w.Header().Set("Proxy-Authenticate", `Basic realm="Proxy Authorization Required"`)
		w.WriteHeader(http.StatusProxyAuthRequired)
		return false
	}
	payload, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		slog.Warn("Error decoding auth", "error", err, "remote", r.RemoteAddr)
		w.WriteHeader(http.StatusBadRequest)
		return false
	}

	pair := strings.SplitN(string(payload), ":", 2)
	if len(pair) != 2 {
		slog.Warn("Invalid auth format", "remote", r.RemoteAddr)
		w.Header().Set("Proxy-Authenticate", `Basic realm="Proxy Authorization Required"`)
		w.WriteHeader(http.StatusProxyAuthRequired)
		return false
	}

	// Constant-time comparison to prevent timing attacks.
	usernameMatch := subtle.ConstantTimeCompare([]byte(pair[0]), []byte(p.config.Username))
	passwordMatch := subtle.ConstantTimeCompare([]byte(pair[1]), []byte(p.config.Password))
	if usernameMatch&passwordMatch != 1 {
		slog.Warn("Invalid credentials", "user", pair[0], "remote", r.RemoteAddr)
		w.Header().Set("Proxy-Authenticate", `Basic realm="Proxy Authorization Required"`)
		w.WriteHeader(http.StatusProxyAuthRequired)
		return false
	}

	return true
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

func (p *proxyServer) handleHTTP(w http.ResponseWriter, r *http.Request) {
	outRequest := r.Clone(r.Context())
	outRequest.RequestURI = ""
	outRequest.Host = outRequest.URL.Host
	removeHopByHopHeaders(outRequest.Header)

	resp, err := p.httpClient.Do(outRequest)
	if err != nil {
		slog.Error("Error forwarding request", "error", err, "url", r.URL.String())
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer resp.Body.Close()

	removeHopByHopHeaders(resp.Header)
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)

	bufPtr := bufPool.Get().(*[]byte)
	defer bufPool.Put(bufPtr)
	written, err := io.CopyBuffer(w, resp.Body, *bufPtr)
	if err != nil {
		// Headers already sent — cannot call http.Error, just log.
		slog.Error("Error copying response body", "written", written, "error", err)
		return
	}
	slog.Debug("Response copied", "bytes", written, "url", r.URL.String())
}

func readConnectResponse(br *bufio.Reader) (int, error) {
	remaining := maxConnectResponseHeaderBytes
	statusLine, err := readLimitedLine(br, &remaining)
	if err != nil {
		return 0, fmt.Errorf("read status line from upstream: %w", err)
	}

	parts := strings.Fields(statusLine)
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "HTTP/") {
		return 0, fmt.Errorf("invalid CONNECT response from upstream: %q", statusLine)
	}
	statusCode, err := strconv.Atoi(parts[1])
	if err != nil || statusCode < 100 || statusCode > 999 {
		return 0, fmt.Errorf("invalid upstream status code %q", parts[1])
	}

	for {
		line, err := readLimitedLine(br, &remaining)
		if err != nil {
			return 0, fmt.Errorf("read header from upstream: %w", err)
		}
		if line == "" {
			break
		}
	}
	return statusCode, nil
}

func readLimitedLine(br *bufio.Reader, remaining *int) (string, error) {
	var line []byte
	for {
		part, prefix, err := br.ReadLine()
		if err != nil {
			return "", err
		}
		consumed := len(part)
		if !prefix {
			// Account conservatively for the line terminator stripped by ReadLine.
			consumed += 2
		}
		*remaining -= consumed
		if *remaining < 0 {
			return "", fmt.Errorf("CONNECT response headers exceed %d bytes", maxConnectResponseHeaderBytes)
		}
		line = append(line, part...)
		if !prefix {
			return string(line), nil
		}
	}
}

func (p *proxyServer) dialUpstream(ctx context.Context, targetHost string) (net.Conn, error) {
	if err := validateTargetAddress(targetHost); err != nil {
		return nil, err
	}

	proxyRequest := &http.Request{URL: &url.URL{Scheme: "https", Host: targetHost}}
	upstreamURL, err := p.proxyFunc(proxyRequest)
	if err != nil {
		return nil, fmt.Errorf("select upstream proxy: %w", err)
	}
	network, err := dialNetwork(p.config.Network)
	if err != nil {
		return nil, err
	}
	if upstreamURL == nil {
		return p.dialer.DialContext(ctx, network, targetHost)
	}

	upstreamURL, err = parseProxyURL(upstreamURL.String())
	if err != nil {
		return nil, fmt.Errorf("invalid selected upstream proxy: %w", err)
	}
	upstreamAddress, err := proxyAddress(upstreamURL)
	if err != nil {
		return nil, fmt.Errorf("invalid selected upstream proxy: %w", err)
	}
	slog.Debug("Connecting via upstream proxy", "upstream", upstreamURL.Redacted(), "target", targetHost)

	rawConn, err := p.dialer.DialContext(ctx, network, upstreamAddress)
	if err != nil {
		return nil, fmt.Errorf("dial upstream proxy %s: %w", upstreamURL.Redacted(), err)
	}

	setupDeadline := time.Now().Add(time.Duration(p.config.ResponseHeaderTimeout))
	if err := rawConn.SetDeadline(setupDeadline); err != nil {
		_ = rawConn.Close()
		return nil, fmt.Errorf("set upstream setup deadline: %w", err)
	}
	setupCtx, cancel := context.WithTimeout(ctx, time.Duration(p.config.ResponseHeaderTimeout))
	defer cancel()

	var conn net.Conn = rawConn
	if upstreamURL.Scheme == "https" {
		tlsConn := tls.Client(rawConn, &tls.Config{
			ServerName: upstreamURL.Hostname(),
			MinVersion: tls.VersionTLS12,
		})
		handshakeCtx, handshakeCancel := context.WithTimeout(setupCtx, time.Duration(p.config.TLSHandshakeTimeout))
		err := tlsConn.HandshakeContext(handshakeCtx)
		handshakeCancel()
		if err != nil {
			_ = rawConn.Close()
			return nil, fmt.Errorf("TLS handshake with upstream proxy: %w", err)
		}
		conn = tlsConn
	}

	connectRequest := buildConnectRequest(targetHost, upstreamURL)
	if err := writeAll(conn, []byte(connectRequest)); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("send CONNECT to upstream: %w", err)
	}

	br := bufio.NewReader(conn)
	statusCode, err := readConnectResponse(br)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if statusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("upstream proxy CONNECT returned %d", statusCode)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("clear upstream setup deadline: %w", err)
	}
	return &bufferedConn{Conn: conn, reader: br}, nil
}

func validateTargetAddress(targetHost string) error {
	host, portText, err := net.SplitHostPort(targetHost)
	if err != nil || host == "" {
		return fmt.Errorf("invalid CONNECT target %q", targetHost)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid CONNECT target port in %q", targetHost)
	}
	return nil
}

func buildConnectRequest(targetHost string, upstreamURL *url.URL) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", targetHost, targetHost)
	if upstreamURL.User != nil {
		username := upstreamURL.User.Username()
		password, _ := upstreamURL.User.Password()
		credentials := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
		fmt.Fprintf(&builder, "Proxy-Authorization: Basic %s\r\n", credentials)
	}
	builder.WriteString("\r\n")
	return builder.String()
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

func (p *proxyServer) handleTunneling(w http.ResponseWriter, r *http.Request) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		slog.Error("Hijacking not supported")
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}

	destConn, err := p.dialUpstream(r.Context(), r.Host)
	if err != nil {
		slog.Error("Can't connect to host", "host", r.Host, "error", err)
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	clientConn, readWriter, err := hijacker.Hijack()
	if err != nil {
		slog.Error("Client connection hijack error", "error", err)
		_ = destConn.Close()
		return
	}
	if err := clientConn.SetDeadline(time.Time{}); err != nil {
		slog.Debug("Could not clear client connection deadline", "error", err)
	}

	activeTunnel := &tunnel{client: clientConn, dest: destConn}
	if !p.tunnels.track(activeTunnel) {
		_ = destConn.Close()
		_ = clientConn.Close()
		return
	}
	defer p.tunnels.untrack(activeTunnel)
	defer destConn.Close()
	defer clientConn.Close()

	if _, err := readWriter.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		slog.Debug("Could not write CONNECT response", "error", err)
		return
	}
	if err := readWriter.Flush(); err != nil {
		slog.Debug("Could not flush CONNECT response", "error", err)
		return
	}

	clientSource := &bufferedConn{Conn: clientConn, reader: readWriter.Reader}
	relayTunnel(clientConn, clientSource, destConn)
}

type transferResult struct {
	err error
}

func relayTunnel(clientWriter net.Conn, clientSource io.Reader, destination net.Conn) {
	results := make(chan transferResult, 2)
	go copyAndHalfClose(destination, clientSource, results)
	go copyAndHalfClose(clientWriter, destination, results)

	first := <-results
	if first.err != nil {
		deadline := time.Now()
		_ = clientWriter.SetDeadline(deadline)
		_ = destination.SetDeadline(deadline)
	}
	<-results
}

func copyAndHalfClose(destination io.Writer, source io.Reader, results chan<- transferResult) {
	written, err := transfer(destination, source)
	if err == nil {
		if conn, ok := destination.(interface{ CloseWrite() error }); ok {
			if closeErr := conn.CloseWrite(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
				err = closeErr
			}
		}
	}
	if err != nil {
		slog.Debug("Transfer finished with error", "bytes", written, "error", err)
	} else {
		slog.Debug("Transfer complete", "bytes", written)
	}
	results <- transferResult{err: err}
}

func transfer(destination io.Writer, source io.Reader) (int64, error) {
	bufPtr := bufPool.Get().(*[]byte)
	defer bufPool.Put(bufPtr)
	return io.CopyBuffer(destination, source, *bufPtr)
}

func run(ctx context.Context, config Config) error {
	if err := validateConfig(&config); err != nil {
		return err
	}
	proxy, err := newProxyServer(config)
	if err != nil {
		return err
	}

	server := &http.Server{
		Addr:              config.ProxyAddr,
		ReadHeaderTimeout: time.Duration(config.ReadHeaderTimeout),
		IdleTimeout:       time.Duration(config.IdleTimeout),
		Handler:           proxy,
	}
	listener, err := makeListener(config, server)
	if err != nil {
		return err
	}

	probeRequest := &http.Request{URL: &url.URL{Scheme: "https", Host: "example.com:443"}}
	if upstream, proxyErr := proxy.proxyFunc(probeRequest); proxyErr != nil {
		_ = listener.Close()
		return fmt.Errorf("resolve upstream proxy: %w", proxyErr)
	} else if upstream != nil {
		slog.Info("Upstream proxy configured", "upstream", upstream.Redacted())
	}

	slog.Info("Starting proxy server", "addr", config.ProxyAddr, "proto", config.Proto)
	serveResult := make(chan error, 1)
	go func() {
		serveResult <- server.Serve(listener)
	}()

	select {
	case err := <-serveResult:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve proxy: %w", err)
	case <-ctx.Done():
	}

	slog.Info("Shutting down proxy server...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Duration(config.ShutdownTimeout))
	defer cancel()

	proxy.tunnels.closeAll()
	shutdownErr := server.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		_ = server.Close()
	}
	tunnelErr := proxy.tunnels.wait(shutdownCtx)
	serveErr := <-serveResult
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	return errors.Join(shutdownErr, tunnelErr, serveErr)
}

func makeListener(config Config, server *http.Server) (net.Listener, error) {
	var tlsConfig *tls.Config
	if config.Proto == "https" {
		cert, err := tls.LoadX509KeyPair(config.CertPath, config.KeyPath)
		if err != nil {
			return nil, fmt.Errorf("load certificate: %w", err)
		}
		tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
		server.TLSConfig = tlsConfig
	}

	listener, err := net.Listen("tcp", config.ProxyAddr)
	if err != nil {
		return nil, fmt.Errorf("create listener: %w", err)
	}
	if tlsConfig != nil {
		return tls.NewListener(listener, tlsConfig), nil
	}
	return listener, nil
}
