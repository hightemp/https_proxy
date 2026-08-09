package config

import (
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
				if got != want {
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
				"dial_timeout: 250ms",
				"tls_handshake_timeout: 3s",
				"response_header_timeout: 4s",
				"read_header_timeout: 5s",
				"idle_timeout: 6m",
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

				wantDurations := map[string]time.Duration{
					"DialTimeout":           250 * time.Millisecond,
					"TLSHandshakeTimeout":   3 * time.Second,
					"ResponseHeaderTimeout": 4 * time.Second,
					"ReadHeaderTimeout":     5 * time.Second,
					"IdleTimeout":           6 * time.Minute,
					"ShutdownTimeout":       7 * time.Second,
				}
				gotDurations := map[string]time.Duration{
					"DialTimeout":           time.Duration(got.DialTimeout),
					"TLSHandshakeTimeout":   time.Duration(got.TLSHandshakeTimeout),
					"ResponseHeaderTimeout": time.Duration(got.ResponseHeaderTimeout),
					"ReadHeaderTimeout":     time.Duration(got.ReadHeaderTimeout),
					"IdleTimeout":           time.Duration(got.IdleTimeout),
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
		{name: "response header", envName: "PROXY_RESPONSE_HEADER_TIMEOUT", value: "12s", get: func(c Config) time.Duration { return time.Duration(c.ResponseHeaderTimeout) }, want: 12 * time.Second},
		{name: "read header", envName: "PROXY_READ_HEADER_TIMEOUT", value: "13s", get: func(c Config) time.Duration { return time.Duration(c.ReadHeaderTimeout) }, want: 13 * time.Second},
		{name: "idle", envName: "PROXY_IDLE_TIMEOUT", value: "3m", get: func(c Config) time.Duration { return time.Duration(c.IdleTimeout) }, want: 3 * time.Minute},
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
		{name: "response header", envName: "PROXY_RESPONSE_HEADER_TIMEOUT"},
		{name: "read header", envName: "PROXY_READ_HEADER_TIMEOUT"},
		{name: "idle", envName: "PROXY_IDLE_TIMEOUT"},
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
		"PROXY_DIAL_TIMEOUT",
		"PROXY_TLS_HANDSHAKE_TIMEOUT",
		"PROXY_RESPONSE_HEADER_TIMEOUT",
		"PROXY_READ_HEADER_TIMEOUT",
		"PROXY_IDLE_TIMEOUT",
		"PROXY_SHUTDOWN_TIMEOUT",
	} {
		t.Setenv(name, "")
	}
}
