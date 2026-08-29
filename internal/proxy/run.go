package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/hightemp/https_proxy/internal/config"
)

// Run serves requests until ctx is cancelled and then gracefully shuts down.
func Run(ctx context.Context, cfg config.Config) error {
	if err := config.Validate(&cfg); err != nil {
		return err
	}
	proxyServer, err := NewServer(cfg)
	if err != nil {
		return err
	}
	if cfg.LogSensitiveData {
		slog.Warn("Sensitive logging enabled; full request URLs, upstream errors, and rejected credentials may be written to logs")
	}

	server := newHTTPServer(cfg, proxyServer)
	listener, err := makeListener(cfg, server)
	if err != nil {
		return err
	}

	probeRequest := &http.Request{URL: &url.URL{Scheme: "https", Host: "example.com:443"}}
	if upstream, proxyErr := proxyServer.proxyFunc(probeRequest); proxyErr != nil {
		_ = listener.Close()
		return fmt.Errorf("resolve upstream proxy: %w", proxyErr)
	} else if upstream != nil {
		slog.Info("Upstream proxy configured", "upstream", upstream.Redacted())
	}

	slog.Info("Starting proxy server", "addr", cfg.ProxyAddr, "proto", cfg.Proto)
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
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.ShutdownTimeout))
	defer cancel()

	proxyServer.tunnels.CloseAll()
	shutdownErr := server.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		_ = server.Close()
	}
	tunnelErr := proxyServer.tunnels.Wait(shutdownCtx)
	serveErr := <-serveResult
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	return errors.Join(shutdownErr, tunnelErr, serveErr)
}

func newHTTPServer(cfg config.Config, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              cfg.ProxyAddr,
		ReadHeaderTimeout: time.Duration(cfg.ReadHeaderTimeout),
		IdleTimeout:       time.Duration(cfg.IdleTimeout),
		MaxHeaderBytes:    cfg.MaxHeaderBytes,
		Handler:           handler,
	}
}

func makeListener(cfg config.Config, server *http.Server) (net.Listener, error) {
	var tlsConfig *tls.Config
	if cfg.Proto == "https" {
		certificateReloader, err := newCertificateReloader(
			cfg.CertPath,
			cfg.KeyPath,
			time.Duration(cfg.TLSReloadInterval),
		)
		if err != nil {
			return nil, fmt.Errorf("load certificate: %w", err)
		}
		tlsConfig = &tls.Config{
			GetCertificate: certificateReloader.getCertificate,
			MinVersion:     tls.VersionTLS12,
			NextProtos:     []string{"h2", "http/1.1"},
		}
		server.TLSConfig = tlsConfig
	}

	listener, err := net.Listen("tcp", cfg.ProxyAddr)
	if err != nil {
		return nil, fmt.Errorf("create listener: %w", err)
	}
	listener = newLimitedListener(listener, cfg.MaxConnections, cfg.MaxConnectionsPerIP)
	if tlsConfig != nil {
		return tls.NewListener(listener, tlsConfig), nil
	}
	return listener, nil
}
