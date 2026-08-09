package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDecodeConfig(t *testing.T) {
	tests := []struct {
		name  string
		yaml  string
		check func(*testing.T, Config)
	}{
		{
			name: "defaults",
			yaml: "{}\n",
			check: func(t *testing.T, got Config) {
				t.Helper()
				want := defaultConfig()
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("decodeConfig() = %+v, want %+v", got, want)
				}
			},
		},
		{
			name: "configuration overrides",
			yaml: strings.Join([]string{
				"proxy_addr: 0.0.0.0:3128",
				"network: tcp4",
				"log_sensitive_data: true",
				"max_connections: 200",
				"max_connections_per_ip: 20",
				"max_tunnels: 50",
				"max_tunnels_per_ip: 5",
				"max_header_bytes: 32768",
				"auth_max_failures: 7",
				"allow_private_destinations: true",
				"blocked_destination_ports: [22, 25]",
				"dial_timeout: 250ms",
				"tls_handshake_timeout: 3s",
				"tls_reload_interval: 350ms",
				"response_header_timeout: 4s",
				"read_header_timeout: 5s",
				"idle_timeout: 6m",
				"tunnel_idle_timeout: 8m",
				"auth_failure_window: 9m",
				"auth_block_duration: 10m",
				"shutdown_timeout: 7s",
			}, "\n") + "\n",
			check: func(t *testing.T, got Config) {
				t.Helper()
				if got.ProxyAddr != "0.0.0.0:3128" {
					t.Errorf("ProxyAddr = %q, want %q", got.ProxyAddr, "0.0.0.0:3128")
				}
				if got.Network != "tcp4" {
					t.Errorf("Network = %q, want %q", got.Network, "tcp4")
				}
				if !got.LogSensitiveData {
					t.Error("LogSensitiveData = false, want true")
				}
				if !got.AllowPrivateDestinations {
					t.Error("AllowPrivateDestinations = false, want true")
				}
				wantLimits := map[string]int{
					"MaxConnections":      200,
					"MaxConnectionsPerIP": 20,
					"MaxTunnels":          50,
					"MaxTunnelsPerIP":     5,
					"MaxHeaderBytes":      32768,
					"AuthMaxFailures":     7,
				}
				gotLimits := map[string]int{
					"MaxConnections":      got.MaxConnections,
					"MaxConnectionsPerIP": got.MaxConnectionsPerIP,
					"MaxTunnels":          got.MaxTunnels,
					"MaxTunnelsPerIP":     got.MaxTunnelsPerIP,
					"MaxHeaderBytes":      got.MaxHeaderBytes,
					"AuthMaxFailures":     got.AuthMaxFailures,
				}
				for name, want := range wantLimits {
					if value := gotLimits[name]; value != want {
						t.Errorf("%s = %d, want %d", name, value, want)
					}
				}
				if want := []int{22, 25}; !reflect.DeepEqual(got.BlockedDestinationPorts, want) {
					t.Errorf("BlockedDestinationPorts = %v, want %v", got.BlockedDestinationPorts, want)
				}

				wantDurations := map[string]time.Duration{
					"DialTimeout":           250 * time.Millisecond,
					"TLSHandshakeTimeout":   3 * time.Second,
					"TLSReloadInterval":     350 * time.Millisecond,
					"ResponseHeaderTimeout": 4 * time.Second,
					"ReadHeaderTimeout":     5 * time.Second,
					"IdleTimeout":           6 * time.Minute,
					"TunnelIdleTimeout":     8 * time.Minute,
					"AuthFailureWindow":     9 * time.Minute,
					"AuthBlockDuration":     10 * time.Minute,
					"ShutdownTimeout":       7 * time.Second,
				}
				gotDurations := map[string]time.Duration{
					"DialTimeout":           time.Duration(got.DialTimeout),
					"TLSHandshakeTimeout":   time.Duration(got.TLSHandshakeTimeout),
					"TLSReloadInterval":     time.Duration(got.TLSReloadInterval),
					"ResponseHeaderTimeout": time.Duration(got.ResponseHeaderTimeout),
					"ReadHeaderTimeout":     time.Duration(got.ReadHeaderTimeout),
					"IdleTimeout":           time.Duration(got.IdleTimeout),
					"TunnelIdleTimeout":     time.Duration(got.TunnelIdleTimeout),
					"AuthFailureWindow":     time.Duration(got.AuthFailureWindow),
					"AuthBlockDuration":     time.Duration(got.AuthBlockDuration),
					"ShutdownTimeout":       time.Duration(got.ShutdownTimeout),
				}
				for name, want := range wantDurations {
					if value := gotDurations[name]; value != want {
						t.Errorf("%s = %s, want %s", name, value, want)
					}
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeConfig(strings.NewReader(tt.yaml))
			if err != nil {
				t.Fatalf("decodeConfig() error = %v", err)
			}
			tt.check(t, got)
		})
	}
}

func TestLoadAppliesFileAndEnvironment(t *testing.T) {
	clearProxyEnvironment(t)
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := []byte("proxy_addr: 127.0.0.1:8081\nnetwork: tcp4\n")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("PROXY_ADDR", "127.0.0.1:8082")
	t.Setenv("PROXY_MAX_CONNECTIONS", "200")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.ProxyAddr != "127.0.0.1:8082" {
		t.Fatalf("ProxyAddr = %q, want environment override", cfg.ProxyAddr)
	}
	if cfg.Network != "tcp4" {
		t.Fatalf("Network = %q, want file value", cfg.Network)
	}
	if cfg.MaxConnections != 200 {
		t.Fatalf("MaxConnections = %d, want 200", cfg.MaxConnections)
	}
}

func TestLoadRejectsInvalidInputs(t *testing.T) {
	tests := []struct {
		name        string
		contents    string
		environment map[string]string
		missing     bool
		wantMessage string
	}{
		{name: "missing file", missing: true, wantMessage: "read config"},
		{name: "invalid YAML", contents: "unexpected: [", wantMessage: "parse config"},
		{name: "invalid environment", contents: "{}\n", environment: map[string]string{"PROXY_MAX_CONNECTIONS": "many"}, wantMessage: "PROXY_MAX_CONNECTIONS"},
		{name: "invalid merged config", contents: "{}\n", environment: map[string]string{"PROXY_MAX_CONNECTIONS": "1", "PROXY_MAX_CONNECTIONS_PER_IP": "2"}, wantMessage: "cannot exceed"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearProxyEnvironment(t)
			path := filepath.Join(t.TempDir(), "config.yaml")
			if !test.missing {
				if err := os.WriteFile(path, []byte(test.contents), 0o600); err != nil {
					t.Fatalf("write config: %v", err)
				}
			}
			for name, value := range test.environment {
				t.Setenv(name, value)
			}

			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), test.wantMessage) {
				t.Fatalf("Load() error = %v, want message %q", err, test.wantMessage)
			}
		})
	}
}

func TestExampleConfigsAreValid(t *testing.T) {
	for _, name := range []string{"config.example.yaml", "config.docker.yaml"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join("..", "..", name)
			file, err := os.Open(path)
			if err != nil {
				t.Fatalf("open %s: %v", path, err)
			}
			defer file.Close()

			cfg, err := decodeConfig(file)
			if err != nil {
				t.Fatalf("decode %s: %v", path, err)
			}
			if err := validateConfig(&cfg); err != nil {
				t.Fatalf("validate %s: %v", path, err)
			}
		})
	}
}

func TestDecodeConfigRejectsInvalidDocuments(t *testing.T) {
	tests := []struct {
		name        string
		yaml        string
		wantMessage string
	}{
		{
			name:        "unknown field",
			yaml:        "proxy_addr: 127.0.0.1:8080\nunexpected_option: true\n",
			wantMessage: "field unexpected_option not found",
		},
		{
			name:        "multiple documents",
			yaml:        "proxy_addr: 127.0.0.1:8080\n---\nproto: http\n",
			wantMessage: "multiple YAML documents are not supported",
		},
		{
			name:        "numeric duration",
			yaml:        "dial_timeout: 10\n",
			wantMessage: "duration must be a string",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeConfig(strings.NewReader(tt.yaml))
			if err == nil {
				t.Fatal("decodeConfig() error = nil, want an error")
			}
			if !strings.Contains(err.Error(), tt.wantMessage) {
				t.Fatalf("decodeConfig() error = %q, want it to contain %q", err, tt.wantMessage)
			}
		})
	}
}

func TestValidateConfig(t *testing.T) {
	tests := []struct {
		name         string
		mutate       func(*Config)
		wantMessage  string
		wantProto    string
		wantNetwork  string
		wantUpstream string
	}{
		{
			name: "normalizes protocol and network",
			mutate: func(config *Config) {
				config.Proto = " HTTPS "
				config.Network = " TCP4 "
				config.CertPath = "cert.pem"
				config.KeyPath = "key.pem"
			},
			wantProto:   "https",
			wantNetwork: "tcp4",
		},
		{
			name: "normalizes direct upstream mode",
			mutate: func(config *Config) {
				config.UpstreamProxy = " DiReCt "
			},
			wantProto:    "http",
			wantNetwork:  "auto",
			wantUpstream: DirectUpstream,
		},
		{
			name: "invalid protocol",
			mutate: func(config *Config) {
				config.Proto = "socks5"
			},
			wantMessage: "proto must be http or https",
		},
		{
			name: "invalid network",
			mutate: func(config *Config) {
				config.Network = "udp"
			},
			wantMessage: "network must be auto, tcp4, or tcp6",
		},
		{
			name: "missing listen port",
			mutate: func(config *Config) {
				config.ProxyAddr = "127.0.0.1"
			},
			wantMessage: "invalid proxy_addr",
		},
		{
			name: "zero listen port",
			mutate: func(config *Config) {
				config.ProxyAddr = "127.0.0.1:0"
			},
			wantMessage: "port must be between 1 and 65535",
		},
		{
			name: "listen port above range",
			mutate: func(config *Config) {
				config.ProxyAddr = "127.0.0.1:65536"
			},
			wantMessage: "port must be between 1 and 65535",
		},
		{
			name: "nonnumeric listen port",
			mutate: func(config *Config) {
				config.ProxyAddr = "127.0.0.1:proxy"
			},
			wantMessage: "port must be between 1 and 65535",
		},
		{
			name: "https without certificate",
			mutate: func(config *Config) {
				config.Proto = "https"
			},
			wantMessage: "cert_path and key_path are required",
		},
		{
			name: "nonpositive dial timeout",
			mutate: func(config *Config) {
				config.DialTimeout = 0
			},
			wantMessage: "dial_timeout must be greater than zero",
		},
		{
			name: "nonpositive TLS handshake timeout",
			mutate: func(config *Config) {
				config.TLSHandshakeTimeout = Duration(-time.Second)
			},
			wantMessage: "tls_handshake_timeout must be greater than zero",
		},
		{
			name: "nonpositive TLS reload interval",
			mutate: func(config *Config) {
				config.TLSReloadInterval = 0
			},
			wantMessage: "tls_reload_interval must be greater than zero",
		},
		{
			name: "nonpositive response header timeout",
			mutate: func(config *Config) {
				config.ResponseHeaderTimeout = 0
			},
			wantMessage: "response_header_timeout must be greater than zero",
		},
		{
			name: "nonpositive read header timeout",
			mutate: func(config *Config) {
				config.ReadHeaderTimeout = 0
			},
			wantMessage: "read_header_timeout must be greater than zero",
		},
		{
			name: "nonpositive idle timeout",
			mutate: func(config *Config) {
				config.IdleTimeout = 0
			},
			wantMessage: "idle_timeout must be greater than zero",
		},
		{
			name: "nonpositive shutdown timeout",
			mutate: func(config *Config) {
				config.ShutdownTimeout = 0
			},
			wantMessage: "shutdown_timeout must be greater than zero",
		},
		{
			name: "nonpositive tunnel idle timeout",
			mutate: func(config *Config) {
				config.TunnelIdleTimeout = 0
			},
			wantMessage: "tunnel_idle_timeout must be greater than zero",
		},
		{
			name: "nonpositive auth failure window",
			mutate: func(config *Config) {
				config.AuthFailureWindow = 0
			},
			wantMessage: "auth_failure_window must be greater than zero",
		},
		{
			name: "nonpositive auth block duration",
			mutate: func(config *Config) {
				config.AuthBlockDuration = 0
			},
			wantMessage: "auth_block_duration must be greater than zero",
		},
		{
			name: "nonpositive connection limit",
			mutate: func(config *Config) {
				config.MaxConnections = 0
			},
			wantMessage: "max_connections must be greater than zero",
		},
		{
			name: "per-IP connections exceed global limit",
			mutate: func(config *Config) {
				config.MaxConnectionsPerIP = config.MaxConnections + 1
			},
			wantMessage: "max_connections_per_ip cannot exceed max_connections",
		},
		{
			name: "nonpositive tunnel limit",
			mutate: func(config *Config) {
				config.MaxTunnels = 0
			},
			wantMessage: "max_tunnels must be greater than zero",
		},
		{
			name: "per-IP tunnels exceed global limit",
			mutate: func(config *Config) {
				config.MaxTunnelsPerIP = config.MaxTunnels + 1
			},
			wantMessage: "max_tunnels_per_ip cannot exceed max_tunnels",
		},
		{
			name: "nonpositive header limit",
			mutate: func(config *Config) {
				config.MaxHeaderBytes = 0
			},
			wantMessage: "max_header_bytes must be greater than zero",
		},
		{
			name: "nonpositive auth failure limit",
			mutate: func(config *Config) {
				config.AuthMaxFailures = 0
			},
			wantMessage: "auth_max_failures must be greater than zero",
		},
		{
			name: "invalid blocked port",
			mutate: func(config *Config) {
				config.BlockedDestinationPorts = []int{0}
			},
			wantMessage: "blocked_destination_ports contains invalid port",
		},
		{
			name: "duplicate blocked port",
			mutate: func(config *Config) {
				config.BlockedDestinationPorts = []int{22, 22}
			},
			wantMessage: "blocked_destination_ports contains duplicate port",
		},
		{
			name: "unsupported upstream scheme",
			mutate: func(config *Config) {
				config.UpstreamProxy = "socks5://proxy.example:1080"
			},
			wantMessage: "invalid upstream_proxy",
		},
		{
			name: "upstream without host",
			mutate: func(config *Config) {
				config.UpstreamProxy = "http://"
			},
			wantMessage: "invalid upstream_proxy",
		},
		{
			name: "upstream with invalid port",
			mutate: func(config *Config) {
				config.UpstreamProxy = "https://proxy.example:0"
			},
			wantMessage: "invalid upstream_proxy",
		},
		{
			name: "upstream with path",
			mutate: func(config *Config) {
				config.UpstreamProxy = "http://proxy.example:8080/path"
			},
			wantMessage: "invalid upstream_proxy",
		},
		{
			name: "upstream with query",
			mutate: func(config *Config) {
				config.UpstreamProxy = "http://proxy.example:8080?mode=test"
			},
			wantMessage: "invalid upstream_proxy",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := defaultConfig()
			tt.mutate(&config)

			err := validateConfig(&config)
			if tt.wantMessage == "" {
				if err != nil {
					t.Fatalf("validateConfig() error = %v", err)
				}
				if config.Proto != tt.wantProto {
					t.Errorf("Proto = %q, want %q", config.Proto, tt.wantProto)
				}
				if config.Network != tt.wantNetwork {
					t.Errorf("Network = %q, want %q", config.Network, tt.wantNetwork)
				}
				if tt.wantUpstream != "" && config.UpstreamProxy != tt.wantUpstream {
					t.Errorf("UpstreamProxy = %q, want %q", config.UpstreamProxy, tt.wantUpstream)
				}
				return
			}

			if err == nil {
				t.Fatal("validateConfig() error = nil, want an error")
			}
			if !strings.Contains(err.Error(), tt.wantMessage) {
				t.Fatalf("validateConfig() error = %q, want it to contain %q", err, tt.wantMessage)
			}
		})
	}
}

func TestApplyEnvOverridesSetsDirectUpstream(t *testing.T) {
	clearProxyEnvironment(t)
	t.Setenv("PROXY_UPSTREAM_PROXY", DirectUpstream)
	config := defaultConfig()

	if err := applyEnvOverrides(&config); err != nil {
		t.Fatalf("applyEnvOverrides() error = %v", err)
	}
	if err := validateConfig(&config); err != nil {
		t.Fatalf("validateConfig() error = %v", err)
	}
	if config.UpstreamProxy != DirectUpstream {
		t.Fatalf("UpstreamProxy = %q, want %q", config.UpstreamProxy, DirectUpstream)
	}
}

func TestApplyEnvOverridesParsesLogSensitiveData(t *testing.T) {
	tests := []struct {
		name      string
		value     string
		want      bool
		wantError bool
	}{
		{name: "enabled", value: "true", want: true},
		{name: "disabled", value: "false", want: false},
		{name: "invalid", value: "sometimes", wantError: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearProxyEnvironment(t)
			t.Setenv("PROXY_LOG_SENSITIVE_DATA", test.value)
			config := defaultConfig()
			config.LogSensitiveData = !test.want

			err := applyEnvOverrides(&config)
			if test.wantError {
				if err == nil || !strings.Contains(err.Error(), "PROXY_LOG_SENSITIVE_DATA") {
					t.Fatalf("applyEnvOverrides() error = %v, want boolean parse error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("applyEnvOverrides() error = %v", err)
			}
			if config.LogSensitiveData != test.want {
				t.Fatalf("LogSensitiveData = %t, want %t", config.LogSensitiveData, test.want)
			}
		})
	}
}

func TestApplyEnvOverridesParsesResourceLimits(t *testing.T) {
	clearProxyEnvironment(t)
	for name, value := range map[string]string{
		"PROXY_MAX_CONNECTIONS":            "200",
		"PROXY_MAX_CONNECTIONS_PER_IP":     "20",
		"PROXY_MAX_TUNNELS":                "50",
		"PROXY_MAX_TUNNELS_PER_IP":         "5",
		"PROXY_MAX_HEADER_BYTES":           "32768",
		"PROXY_AUTH_MAX_FAILURES":          "7",
		"PROXY_ALLOW_PRIVATE_DESTINATIONS": "true",
		"PROXY_BLOCKED_DESTINATION_PORTS":  "22, 25",
	} {
		t.Setenv(name, value)
	}
	config := defaultConfig()

	if err := applyEnvOverrides(&config); err != nil {
		t.Fatalf("applyEnvOverrides() error = %v", err)
	}
	if config.MaxConnections != 200 || config.MaxConnectionsPerIP != 20 {
		t.Fatalf("connection limits = %d/%d, want 200/20", config.MaxConnections, config.MaxConnectionsPerIP)
	}
	if config.MaxTunnels != 50 || config.MaxTunnelsPerIP != 5 {
		t.Fatalf("tunnel limits = %d/%d, want 50/5", config.MaxTunnels, config.MaxTunnelsPerIP)
	}
	if config.MaxHeaderBytes != 32768 || config.AuthMaxFailures != 7 {
		t.Fatalf("header/auth limits = %d/%d, want 32768/7", config.MaxHeaderBytes, config.AuthMaxFailures)
	}
	if !config.AllowPrivateDestinations {
		t.Fatal("AllowPrivateDestinations = false, want true")
	}
	if want := []int{22, 25}; !reflect.DeepEqual(config.BlockedDestinationPorts, want) {
		t.Fatalf("BlockedDestinationPorts = %v, want %v", config.BlockedDestinationPorts, want)
	}
}

func TestParsePortList(t *testing.T) {
	tests := []struct {
		name      string
		value     string
		want      []int
		wantError bool
	}{
		{name: "ports", value: "22, 25,443", want: []int{22, 25, 443}},
		{name: "none", value: "none"},
		{name: "invalid", value: "22,ssh", wantError: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parsePortList(test.value)
			if test.wantError {
				if err == nil {
					t.Fatal("parsePortList() error = nil, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePortList() error = %v", err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("parsePortList() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestProxyAddress(t *testing.T) {
	tests := []struct {
		name   string
		rawURL string
		want   string
	}{
		{name: "HTTP default port", rawURL: "http://proxy.example", want: "proxy.example:80"},
		{name: "HTTPS default port", rawURL: "https://proxy.example", want: "proxy.example:443"},
		{name: "explicit port", rawURL: "http://proxy.example:3128", want: "proxy.example:3128"},
		{name: "IPv6 default port", rawURL: "https://[2001:db8::1]", want: "[2001:db8::1]:443"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proxyURL, err := ParseProxyURL(tt.rawURL)
			if err != nil {
				t.Fatalf("ParseProxyURL(%q) error = %v", tt.rawURL, err)
			}
			got, err := ProxyAddress(proxyURL)
			if err != nil {
				t.Fatalf("ProxyAddress() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("ProxyAddress() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestApplyEnvOverridesParsesDurations(t *testing.T) {
	tests := []struct {
		name    string
		envName string
		value   string
		get     func(Config) time.Duration
		want    time.Duration
	}{
		{name: "dial", envName: "PROXY_DIAL_TIMEOUT", value: "1500ms", get: func(c Config) time.Duration { return time.Duration(c.DialTimeout) }, want: 1500 * time.Millisecond},
		{name: "TLS handshake", envName: "PROXY_TLS_HANDSHAKE_TIMEOUT", value: "11s", get: func(c Config) time.Duration { return time.Duration(c.TLSHandshakeTimeout) }, want: 11 * time.Second},
		{name: "TLS reload", envName: "PROXY_TLS_RELOAD_INTERVAL", value: "45s", get: func(c Config) time.Duration { return time.Duration(c.TLSReloadInterval) }, want: 45 * time.Second},
		{name: "response header", envName: "PROXY_RESPONSE_HEADER_TIMEOUT", value: "12s", get: func(c Config) time.Duration { return time.Duration(c.ResponseHeaderTimeout) }, want: 12 * time.Second},
		{name: "read header", envName: "PROXY_READ_HEADER_TIMEOUT", value: "13s", get: func(c Config) time.Duration { return time.Duration(c.ReadHeaderTimeout) }, want: 13 * time.Second},
		{name: "idle", envName: "PROXY_IDLE_TIMEOUT", value: "3m", get: func(c Config) time.Duration { return time.Duration(c.IdleTimeout) }, want: 3 * time.Minute},
		{name: "tunnel idle", envName: "PROXY_TUNNEL_IDLE_TIMEOUT", value: "4m", get: func(c Config) time.Duration { return time.Duration(c.TunnelIdleTimeout) }, want: 4 * time.Minute},
		{name: "auth failure window", envName: "PROXY_AUTH_FAILURE_WINDOW", value: "5m", get: func(c Config) time.Duration { return time.Duration(c.AuthFailureWindow) }, want: 5 * time.Minute},
		{name: "auth block", envName: "PROXY_AUTH_BLOCK_DURATION", value: "6m", get: func(c Config) time.Duration { return time.Duration(c.AuthBlockDuration) }, want: 6 * time.Minute},
		{name: "shutdown", envName: "PROXY_SHUTDOWN_TIMEOUT", value: "14s", get: func(c Config) time.Duration { return time.Duration(c.ShutdownTimeout) }, want: 14 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearProxyEnvironment(t)
			t.Setenv(tt.envName, tt.value)
			config := defaultConfig()

			if err := applyEnvOverrides(&config); err != nil {
				t.Fatalf("applyEnvOverrides() error = %v", err)
			}
			if got := tt.get(config); got != tt.want {
				t.Fatalf("%s = %s, want %s", tt.envName, got, tt.want)
			}
		})
	}
}

func TestApplyEnvOverridesRejectsInvalidDurations(t *testing.T) {
	tests := []struct {
		name    string
		envName string
	}{
		{name: "dial", envName: "PROXY_DIAL_TIMEOUT"},
		{name: "TLS handshake", envName: "PROXY_TLS_HANDSHAKE_TIMEOUT"},
		{name: "TLS reload", envName: "PROXY_TLS_RELOAD_INTERVAL"},
		{name: "response header", envName: "PROXY_RESPONSE_HEADER_TIMEOUT"},
		{name: "read header", envName: "PROXY_READ_HEADER_TIMEOUT"},
		{name: "idle", envName: "PROXY_IDLE_TIMEOUT"},
		{name: "tunnel idle", envName: "PROXY_TUNNEL_IDLE_TIMEOUT"},
		{name: "auth failure window", envName: "PROXY_AUTH_FAILURE_WINDOW"},
		{name: "auth block", envName: "PROXY_AUTH_BLOCK_DURATION"},
		{name: "shutdown", envName: "PROXY_SHUTDOWN_TIMEOUT"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearProxyEnvironment(t)
			t.Setenv(tt.envName, "not-a-duration")
			config := defaultConfig()

			err := applyEnvOverrides(&config)
			if err == nil {
				t.Fatal("applyEnvOverrides() error = nil, want an error")
			}
			if !strings.Contains(err.Error(), tt.envName) {
				t.Fatalf("applyEnvOverrides() error = %q, want it to contain %q", err, tt.envName)
			}
		})
	}
}

func clearProxyEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"PROXY_ADDR",
		"PROXY_USERNAME",
		"PROXY_PASSWORD",
		"PROXY_PROTO",
		"PROXY_CERT_PATH",
		"PROXY_KEY_PATH",
		"PROXY_UPSTREAM_PROXY",
		"PROXY_NETWORK",
		"PROXY_LOG_SENSITIVE_DATA",
		"PROXY_MAX_CONNECTIONS",
		"PROXY_MAX_CONNECTIONS_PER_IP",
		"PROXY_MAX_TUNNELS",
		"PROXY_MAX_TUNNELS_PER_IP",
		"PROXY_MAX_HEADER_BYTES",
		"PROXY_AUTH_MAX_FAILURES",
		"PROXY_ALLOW_PRIVATE_DESTINATIONS",
		"PROXY_BLOCKED_DESTINATION_PORTS",
		"PROXY_DIAL_TIMEOUT",
		"PROXY_TLS_HANDSHAKE_TIMEOUT",
		"PROXY_TLS_RELOAD_INTERVAL",
		"PROXY_RESPONSE_HEADER_TIMEOUT",
		"PROXY_READ_HEADER_TIMEOUT",
		"PROXY_IDLE_TIMEOUT",
		"PROXY_TUNNEL_IDLE_TIMEOUT",
		"PROXY_AUTH_FAILURE_WINDOW",
		"PROXY_AUTH_BLOCK_DURATION",
		"PROXY_SHUTDOWN_TIMEOUT",
	} {
		t.Setenv(name, "")
	}
}
