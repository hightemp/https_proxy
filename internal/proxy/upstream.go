package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hightemp/https_proxy/internal/config"
	"github.com/hightemp/https_proxy/internal/tunnel"
)

const maxConnectResponseHeaderBytes = 64 << 10

func buildProxyFunc(rawURL string) (func(*http.Request) (*url.URL, error), error) {
	return buildProxyFuncWithEnvironment(rawURL, http.ProxyFromEnvironment)
}

func buildProxyFuncWithEnvironment(
	rawURL string,
	proxyFromEnvironment func(*http.Request) (*url.URL, error),
) (func(*http.Request) (*url.URL, error), error) {
	if rawURL == config.DirectUpstream {
		return func(*http.Request) (*url.URL, error) {
			return nil, nil
		}, nil
	}
	if rawURL != "" {
		proxyURL, err := config.ParseProxyURL(rawURL)
		if err != nil {
			return nil, fmt.Errorf("invalid upstream_proxy: %w", err)
		}
		return http.ProxyURL(proxyURL), nil
	}

	return func(request *http.Request) (*url.URL, error) {
		proxyURL, err := proxyFromEnvironment(request)
		if err != nil || proxyURL == nil {
			return proxyURL, err
		}
		parsed, err := config.ParseProxyURL(proxyURL.String())
		if err != nil {
			return nil, fmt.Errorf("invalid proxy from environment: %w", err)
		}
		return parsed, nil
	}, nil
}

func readConnectResponse(reader *bufio.Reader) (int, error) {
	remaining := maxConnectResponseHeaderBytes
	statusLine, err := readLimitedLine(reader, &remaining)
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
		line, err := readLimitedLine(reader, &remaining)
		if err != nil {
			return 0, fmt.Errorf("read header from upstream: %w", err)
		}
		if line == "" {
			break
		}
	}
	return statusCode, nil
}

func readLimitedLine(reader *bufio.Reader, remaining *int) (string, error) {
	var line []byte
	for {
		part, prefix, err := reader.ReadLine()
		if err != nil {
			return "", err
		}
		consumed := len(part)
		if !prefix {
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

func (p *Server) dialUpstream(ctx context.Context, targetHost, via string) (net.Conn, error) {
	if err := validateTargetAddress(targetHost); err != nil {
		return nil, err
	}

	proxyRequest := &http.Request{URL: &url.URL{Scheme: "https", Host: targetHost}}
	upstreamURL, err := p.proxyFunc(proxyRequest)
	if err != nil {
		return nil, fmt.Errorf("select upstream proxy: %w", err)
	}
	network, err := config.DialNetwork(p.config.Network)
	if err != nil {
		return nil, err
	}
	if upstreamURL == nil {
		return p.dialer.DialContext(ctx, network, targetHost)
	}

	upstreamURL, err = config.ParseProxyURL(upstreamURL.String())
	if err != nil {
		return nil, fmt.Errorf("invalid selected upstream proxy: %w", err)
	}
	upstreamAddress, err := config.ProxyAddress(upstreamURL)
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
	stopSetupInterrupt := context.AfterFunc(setupCtx, func() {
		_ = rawConn.SetDeadline(time.Now())
	})
	defer stopSetupInterrupt()

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
			if cause := context.Cause(setupCtx); cause != nil {
				err = cause
			}
			return nil, fmt.Errorf("TLS handshake with upstream proxy: %w", err)
		}
		conn = tlsConn
	}

	connectRequest := buildConnectRequest(targetHost, upstreamURL, via)
	if err := writeAll(conn, []byte(connectRequest)); err != nil {
		_ = conn.Close()
		if cause := context.Cause(setupCtx); cause != nil {
			err = cause
		}
		return nil, fmt.Errorf("send CONNECT to upstream: %w", err)
	}

	reader := bufio.NewReader(conn)
	statusCode, err := readConnectResponse(reader)
	if err != nil {
		_ = conn.Close()
		if cause := context.Cause(setupCtx); cause != nil {
			return nil, fmt.Errorf("read CONNECT response from upstream: %w", cause)
		}
		return nil, err
	}
	if !stopSetupInterrupt() {
		_ = conn.Close()
		cause := context.Cause(setupCtx)
		if cause == nil {
			cause = context.Canceled
		}
		return nil, fmt.Errorf("set up upstream CONNECT: %w", cause)
	}
	if statusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("upstream proxy CONNECT returned %d", statusCode)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("clear upstream setup deadline: %w", err)
	}
	return tunnel.NewBufferedConn(conn, reader), nil
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

func buildConnectRequest(targetHost string, upstreamURL *url.URL, via string) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", targetHost, targetHost)
	if via != "" {
		fmt.Fprintf(&builder, "Via: %s\r\n", via)
	}
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
