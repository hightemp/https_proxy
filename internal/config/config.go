// Package config loads and validates proxy configuration.
package config

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DirectUpstream disables use of explicit and environment proxy servers.
const DirectUpstream = "direct"

// Duration is a time.Duration that accepts Go duration strings in YAML.
type Duration time.Duration

// UnmarshalYAML parses a duration such as 500ms, 10s, or 2m.
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

// String returns the duration in Go duration syntax.
func (d Duration) String() string {
	return time.Duration(d).String()
}

// Config contains all runtime settings for the proxy server.
type Config struct {
	ProxyAddr                string   `yaml:"proxy_addr"`
	Username                 string   `yaml:"username"`
	Password                 string   `yaml:"password"`
	Proto                    string   `yaml:"proto"`
	CertPath                 string   `yaml:"cert_path"`
	KeyPath                  string   `yaml:"key_path"`
	UpstreamProxy            string   `yaml:"upstream_proxy"`
	Network                  string   `yaml:"network"`
	LogSensitiveData         bool     `yaml:"log_sensitive_data"`
	MaxConnections           int      `yaml:"max_connections"`
	MaxConnectionsPerIP      int      `yaml:"max_connections_per_ip"`
	MaxTunnels               int      `yaml:"max_tunnels"`
	MaxTunnelsPerIP          int      `yaml:"max_tunnels_per_ip"`
	MaxHeaderBytes           int      `yaml:"max_header_bytes"`
	AuthMaxFailures          int      `yaml:"auth_max_failures"`
	AllowPrivateDestinations bool     `yaml:"allow_private_destinations"`
	BlockedDestinationPorts  []int    `yaml:"blocked_destination_ports"`
	DialTimeout              Duration `yaml:"dial_timeout"`
	TLSHandshakeTimeout      Duration `yaml:"tls_handshake_timeout"`
	TLSReloadInterval        Duration `yaml:"tls_reload_interval"`
	ResponseHeaderTimeout    Duration `yaml:"response_header_timeout"`
	ReadHeaderTimeout        Duration `yaml:"read_header_timeout"`
	IdleTimeout              Duration `yaml:"idle_timeout"`
	TunnelIdleTimeout        Duration `yaml:"tunnel_idle_timeout"`
	AuthFailureWindow        Duration `yaml:"auth_failure_window"`
	AuthBlockDuration        Duration `yaml:"auth_block_duration"`
	ShutdownTimeout          Duration `yaml:"shutdown_timeout"`
}

// Default returns a configuration with safe local-listener defaults.
func Default() Config {
	return defaultConfig()
}

func defaultConfig() Config {
	return Config{
		ProxyAddr:               "127.0.0.1:8080",
		Proto:                   "http",
		Network:                 "auto",
		MaxConnections:          1024,
		MaxConnectionsPerIP:     64,
		MaxTunnels:              256,
		MaxTunnelsPerIP:         16,
		MaxHeaderBytes:          64 << 10,
		AuthMaxFailures:         10,
		BlockedDestinationPorts: []int{21, 22, 23, 25, 110, 111, 135, 137, 138, 139, 445, 1433, 2049, 2375, 2376, 3306, 3389, 5432, 5900, 6379, 9200, 11211, 27017},
		DialTimeout:             Duration(10 * time.Second),
		TLSHandshakeTimeout:     Duration(10 * time.Second),
		TLSReloadInterval:       Duration(time.Minute),
		ResponseHeaderTimeout:   Duration(30 * time.Second),
		ReadHeaderTimeout:       Duration(15 * time.Second),
		IdleTimeout:             Duration(2 * time.Minute),
		TunnelIdleTimeout:       Duration(10 * time.Minute),
		AuthFailureWindow:       Duration(time.Minute),
		AuthBlockDuration:       Duration(5 * time.Minute),
		ShutdownTimeout:         Duration(15 * time.Second),
	}
}

// Load reads, overrides, and validates configuration from path.
func Load(path string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	defer func() {
		// The file is read-only and fully consumed by decodeConfig below.
		_ = file.Close()
	}()

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
//	PROXY_LOG_SENSITIVE_DATA, PROXY_MAX_CONNECTIONS,
//	PROXY_MAX_CONNECTIONS_PER_IP, PROXY_MAX_TUNNELS,
//	PROXY_MAX_TUNNELS_PER_IP, PROXY_MAX_HEADER_BYTES,
//	PROXY_AUTH_MAX_FAILURES, PROXY_ALLOW_PRIVATE_DESTINATIONS,
//	PROXY_BLOCKED_DESTINATION_PORTS,
//	PROXY_DIAL_TIMEOUT, PROXY_TLS_HANDSHAKE_TIMEOUT,
//	PROXY_TLS_RELOAD_INTERVAL,
//	PROXY_RESPONSE_HEADER_TIMEOUT, PROXY_READ_HEADER_TIMEOUT,
//	PROXY_IDLE_TIMEOUT, PROXY_TUNNEL_IDLE_TIMEOUT,
//	PROXY_AUTH_FAILURE_WINDOW, PROXY_AUTH_BLOCK_DURATION,
//	PROXY_SHUTDOWN_TIMEOUT.
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
	boolOverrides := []struct {
		name string
		dest *bool
	}{
		{"PROXY_LOG_SENSITIVE_DATA", &c.LogSensitiveData},
		{"PROXY_ALLOW_PRIVATE_DESTINATIONS", &c.AllowPrivateDestinations},
	}
	for _, override := range boolOverrides {
		value := os.Getenv(override.name)
		if value == "" {
			continue
		}
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("parse %s: %w", override.name, err)
		}
		*override.dest = parsed
	}

	intOverrides := []struct {
		name string
		dest *int
	}{
		{"PROXY_MAX_CONNECTIONS", &c.MaxConnections},
		{"PROXY_MAX_CONNECTIONS_PER_IP", &c.MaxConnectionsPerIP},
		{"PROXY_MAX_TUNNELS", &c.MaxTunnels},
		{"PROXY_MAX_TUNNELS_PER_IP", &c.MaxTunnelsPerIP},
		{"PROXY_MAX_HEADER_BYTES", &c.MaxHeaderBytes},
		{"PROXY_AUTH_MAX_FAILURES", &c.AuthMaxFailures},
	}
	for _, override := range intOverrides {
		value := os.Getenv(override.name)
		if value == "" {
			continue
		}
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("parse %s: %w", override.name, err)
		}
		*override.dest = parsed
	}

	if value := os.Getenv("PROXY_BLOCKED_DESTINATION_PORTS"); value != "" {
		ports, err := parsePortList(value)
		if err != nil {
			return fmt.Errorf("parse PROXY_BLOCKED_DESTINATION_PORTS: %w", err)
		}
		c.BlockedDestinationPorts = ports
	}

	durationOverrides := []struct {
		name string
		dest *Duration
	}{
		{"PROXY_DIAL_TIMEOUT", &c.DialTimeout},
		{"PROXY_TLS_HANDSHAKE_TIMEOUT", &c.TLSHandshakeTimeout},
		{"PROXY_TLS_RELOAD_INTERVAL", &c.TLSReloadInterval},
		{"PROXY_RESPONSE_HEADER_TIMEOUT", &c.ResponseHeaderTimeout},
		{"PROXY_READ_HEADER_TIMEOUT", &c.ReadHeaderTimeout},
		{"PROXY_IDLE_TIMEOUT", &c.IdleTimeout},
		{"PROXY_TUNNEL_IDLE_TIMEOUT", &c.TunnelIdleTimeout},
		{"PROXY_AUTH_FAILURE_WINDOW", &c.AuthFailureWindow},
		{"PROXY_AUTH_BLOCK_DURATION", &c.AuthBlockDuration},
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

// Validate normalizes and validates config in place.
func Validate(config *Config) error {
	return validateConfig(config)
}

func validateConfig(config *Config) error {
	config.Proto = strings.ToLower(strings.TrimSpace(config.Proto))
	if config.Proto != "http" && config.Proto != "https" {
		return fmt.Errorf("proto must be http or https, got %q", config.Proto)
	}

	config.Network = strings.ToLower(strings.TrimSpace(config.Network))
	if _, err := DialNetwork(config.Network); err != nil {
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
		{"tls_reload_interval", config.TLSReloadInterval},
		{"response_header_timeout", config.ResponseHeaderTimeout},
		{"read_header_timeout", config.ReadHeaderTimeout},
		{"idle_timeout", config.IdleTimeout},
		{"tunnel_idle_timeout", config.TunnelIdleTimeout},
		{"auth_failure_window", config.AuthFailureWindow},
		{"auth_block_duration", config.AuthBlockDuration},
		{"shutdown_timeout", config.ShutdownTimeout},
	}

	positiveLimits := []struct {
		name  string
		value int
	}{
		{"max_connections", config.MaxConnections},
		{"max_connections_per_ip", config.MaxConnectionsPerIP},
		{"max_tunnels", config.MaxTunnels},
		{"max_tunnels_per_ip", config.MaxTunnelsPerIP},
		{"max_header_bytes", config.MaxHeaderBytes},
		{"auth_max_failures", config.AuthMaxFailures},
	}
	for _, limit := range positiveLimits {
		if limit.value <= 0 {
			return fmt.Errorf("%s must be greater than zero", limit.name)
		}
	}
	if config.MaxConnectionsPerIP > config.MaxConnections {
		return errors.New("max_connections_per_ip cannot exceed max_connections")
	}
	if config.MaxTunnelsPerIP > config.MaxTunnels {
		return errors.New("max_tunnels_per_ip cannot exceed max_tunnels")
	}
	seenPorts := make(map[int]struct{}, len(config.BlockedDestinationPorts))
	for _, port := range config.BlockedDestinationPorts {
		if port < 1 || port > 65535 {
			return fmt.Errorf("blocked_destination_ports contains invalid port %d", port)
		}
		if _, exists := seenPorts[port]; exists {
			return fmt.Errorf("blocked_destination_ports contains duplicate port %d", port)
		}
		seenPorts[port] = struct{}{}
	}
	for _, duration := range durations {
		if duration.value <= 0 {
			return fmt.Errorf("%s must be greater than zero", duration.name)
		}
	}

	if strings.EqualFold(strings.TrimSpace(config.UpstreamProxy), DirectUpstream) {
		config.UpstreamProxy = DirectUpstream
	} else if config.UpstreamProxy != "" {
		if _, err := ParseProxyURL(config.UpstreamProxy); err != nil {
			return fmt.Errorf("invalid upstream_proxy: %w", err)
		}
	}
	return nil
}

func parsePortList(value string) ([]int, error) {
	if strings.EqualFold(strings.TrimSpace(value), "none") {
		return nil, nil
	}

	parts := strings.Split(value, ",")
	ports := make([]int, 0, len(parts))
	for _, part := range parts {
		port, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			return nil, fmt.Errorf("invalid port %q", strings.TrimSpace(part))
		}
		ports = append(ports, port)
	}
	return ports, nil
}

func validateListenAddress(address string) error {
	_, portText, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return errors.New("port must be between 1 and 65535")
	}
	return nil
}

// DialNetwork converts a configured network mode into a net.Dial network.
func DialNetwork(network string) (string, error) {
	switch network {
	case "auto":
		return "tcp", nil
	case "tcp4", "tcp6":
		return network, nil
	default:
		return "", fmt.Errorf("network must be auto, tcp4, or tcp6, got %q", network)
	}
}

// ParseProxyURL parses and validates an HTTP or HTTPS upstream proxy URL.
func ParseProxyURL(rawURL string) (*url.URL, error) {
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
	if _, err := ProxyAddress(proxyURL); err != nil {
		return nil, err
	}
	return proxyURL, nil
}

// ProxyAddress returns the validated host:port address for an upstream URL.
func ProxyAddress(proxyURL *url.URL) (string, error) {
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
