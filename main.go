package main

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	ProxyAddr string `yaml:"proxy_addr"`
	Username  string `yaml:"username"`
	Password  string `yaml:"password"`
	Proto     string `yaml:"proto"`
	CertPath  string `yaml:"cert_path"`
	KeyPath   string `yaml:"key_path"`
}

// Hop-by-hop headers that should not be forwarded by proxies (RFC 2616 §13.5.1).
var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
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

var (
	config     Config
	httpClient *http.Client
)

func main() {
	configPath := flag.String("config", "config.yaml", "Path to the config file")
	flag.Parse()

	content, err := os.ReadFile(*configPath)
	if err != nil {
		slog.Error("Error reading config file", "error", err)
		os.Exit(1)
	}

	err = yaml.Unmarshal(content, &config)
	if err != nil {
		slog.Error("Error parsing config file", "error", err)
		os.Exit(1)
	}

	// Global HTTP client with connection pooling and timeouts.
	httpClient = &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects (>%d) while following %s", len(via), via[len(via)-1].URL.String())
			}
			return nil
		},
		Timeout: 60 * time.Second,
	}

	server := &http.Server{
		Addr:         config.ProxyAddr,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			slog.Info("Received request", "method", r.Method, "url", r.URL.String())
			if !basicAuth(w, r) {
				return
			}

			if r.Method == http.MethodConnect {
				handleTunneling(w, r)
			} else {
				handleHTTP(w, r)
			}
		}),
	}

	// Graceful shutdown on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		slog.Info("Shutting down proxy server...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			slog.Error("Server shutdown error", "error", err)
		}
	}()

	slog.Info("Starting proxy server", "addr", config.ProxyAddr, "proto", config.Proto)
	if config.Proto == "https" {
		ln, err := net.Listen("tcp", config.ProxyAddr)
		if err != nil {
			slog.Error("Error creating listener", "error", err)
			os.Exit(1)
		}

		cert, err := tls.LoadX509KeyPair(config.CertPath, config.KeyPath)
		if err != nil {
			slog.Error("Error loading certificate", "error", err)
			os.Exit(1)
		}

		server.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}

		tlsListener := tls.NewListener(ln, server.TLSConfig)

		if err := server.Serve(tlsListener); err != http.ErrServerClosed {
			slog.Error("Server error", "error", err)
			os.Exit(1)
		}
	} else {
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			slog.Error("Server error", "error", err)
			os.Exit(1)
		}
	}

	slog.Info("Server stopped")
}

func basicAuth(w http.ResponseWriter, r *http.Request) bool {
	auth := r.Header.Get("Proxy-Authorization")
	if auth == "" {
		slog.Debug("No Proxy-Authorization header", "remote", r.RemoteAddr)
		w.Header().Set("Proxy-Authenticate", `Basic realm="Proxy Authorization Required"`)
		w.WriteHeader(http.StatusProxyAuthRequired)
		return false
	}

	payload, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, "Basic "))
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
	usernameMatch := subtle.ConstantTimeCompare([]byte(pair[0]), []byte(config.Username))
	passwordMatch := subtle.ConstantTimeCompare([]byte(pair[1]), []byte(config.Password))
	if usernameMatch&passwordMatch != 1 {
		slog.Warn("Invalid credentials", "user", pair[0], "remote", r.RemoteAddr)
		w.Header().Set("Proxy-Authenticate", `Basic realm="Proxy Authorization Required"`)
		w.WriteHeader(http.StatusProxyAuthRequired)
		return false
	}

	return true
}

func handleHTTP(w http.ResponseWriter, r *http.Request) {
	r.RequestURI = ""
	r.Host = r.URL.Host

	// Remove hop-by-hop headers.
	for _, h := range hopByHopHeaders {
		r.Header.Del(h)
	}

	resp, err := httpClient.Do(r)
	if err != nil {
		slog.Error("Error forwarding request", "error", err, "url", r.URL.String())
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer resp.Body.Close()

	// Copy response headers, skipping hop-by-hop headers.
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

func handleTunneling(w http.ResponseWriter, r *http.Request) {
	destConn, err := net.DialTimeout("tcp", r.Host, 10*time.Second)
	if err != nil {
		slog.Error("Can't connect to host", "host", r.Host, "error", err)
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	w.WriteHeader(http.StatusOK)

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		slog.Error("Hijacking not supported")
		destConn.Close()
		return
	}
	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		slog.Error("Client connection hijack error", "error", err)
		destConn.Close()
		return
	}

	// Use a WaitGroup to wait for both directions to finish,
	// then close both connections cleanly.
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		transfer(destConn, clientConn)
		// Signal the other direction to stop by setting a read deadline.
		if tc, ok := destConn.(*net.TCPConn); ok {
			tc.SetReadDeadline(time.Now())
		}
	}()

	go func() {
		defer wg.Done()
		transfer(clientConn, destConn)
		if tc, ok := clientConn.(*net.TCPConn); ok {
			tc.SetReadDeadline(time.Now())
		}
	}()

	wg.Wait()
	destConn.Close()
	clientConn.Close()
}

func transfer(destination io.Writer, source io.Reader) {
	bufPtr := bufPool.Get().(*[]byte)
	defer bufPool.Put(bufPtr)
	written, err := io.CopyBuffer(destination, source, *bufPtr)
	if err != nil {
		slog.Debug("Transfer finished with error", "bytes", written, "error", err)
	} else {
		slog.Debug("Transfer complete", "bytes", written)
	}
}
