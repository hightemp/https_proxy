# Secure HTTP Proxy Server

[![GitHub Repo](https://img.shields.io/badge/github-hightemp%2Fhttps__proxy-blue?logo=github)](https://github.com/hightemp/https_proxy)
[![Go Version](https://img.shields.io/github/go-mod/go-version/hightemp/https_proxy)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![GitHub release](https://img.shields.io/github/v/release/hightemp/https_proxy)](https://github.com/hightemp/https_proxy/releases)
[![GitHub Downloads](https://img.shields.io/github/downloads/hightemp/https_proxy/total)](https://github.com/hightemp/https_proxy/releases)
[![Docker Pulls](https://img.shields.io/docker/pulls/hightemp/https_proxy.svg)](https://hub.docker.com/r/hightemp/https_proxy)
[![GitHub Workflow Status](https://img.shields.io/github/actions/workflow/status/hightemp/https_proxy/release.yml)](https://github.com/hightemp/https_proxy/actions)
[![Go Report Card](https://goreportcard.com/badge/github.com/hightemp/https_proxy)](https://goreportcard.com/report/github.com/hightemp/https_proxy)
[![](https://asdertasd.site/counter/https_proxy?a=1)](https://asdertasd.site/counter/https_proxy)

A secure HTTP/HTTPS proxy server in Go with Basic authentication, TLS support, and upstream proxy chaining.

## Features

- HTTP and HTTPS proxy modes
- Basic authentication
- TLS with configurable certificates
- Upstream proxy chaining (proxy chain support)
  - Supports HTTP and HTTPS upstream proxies
  - Configurable via `config.yaml` or environment variables (`HTTPS_PROXY`, `HTTP_PROXY`)
  - Basic authentication to upstream proxy
- Selectable outbound network (`auto`, IPv4-only, or IPv6-only)
- Bounded dial, TLS handshake, and response-header timeouts
- Configurable via YAML file
- Systemd service support
- Graceful shutdown of HTTP requests and hijacked CONNECT tunnels

## Installation

### From release

Download the latest binary from the [Releases](https://github.com/hightemp/https_proxy/releases) page.

### Docker

https://hub.docker.com/r/hightemp/https_proxy

One-liner (HTTP proxy on port 8080, **no authentication**):

```sh
docker run -d --name https_proxy -p 8080:8080 hightemp/https_proxy:latest
```

Enable Basic auth via env vars:

```sh
docker run -d --name https_proxy -p 8080:8080 \
  -e PROXY_USERNAME=alice -e PROXY_PASSWORD=s3cret \
  hightemp/https_proxy:latest
```

> If both `username` and `password` are empty, authentication is disabled.

With a custom config:

```sh
docker run -d --name https_proxy -p 8080:8080 -v $(pwd)/config.yaml:/etc/https_proxy/config.yaml:ro hightemp/https_proxy:latest
```

#### Environment variables

Any of these override the corresponding YAML field:

| Variable | Overrides |
|---|---|
| `PROXY_ADDR` | `proxy_addr` |
| `PROXY_USERNAME` | `username` |
| `PROXY_PASSWORD` | `password` |
| `PROXY_PROTO` | `proto` (`http` / `https`) |
| `PROXY_CERT_PATH` | `cert_path` |
| `PROXY_KEY_PATH` | `key_path` |
| `PROXY_UPSTREAM_PROXY` | `upstream_proxy` (URL or `direct`) |
| `PROXY_NETWORK` | `network` (`auto` / `tcp4` / `tcp6`) |
| `PROXY_LOG_SENSITIVE_DATA` | `log_sensitive_data` (`true` exposes full request URLs, upstream errors, and rejected credentials in logs) |
| `PROXY_MAX_CONNECTIONS` | `max_connections` |
| `PROXY_MAX_CONNECTIONS_PER_IP` | `max_connections_per_ip` |
| `PROXY_MAX_TUNNELS` | `max_tunnels` |
| `PROXY_MAX_TUNNELS_PER_IP` | `max_tunnels_per_ip` |
| `PROXY_MAX_HEADER_BYTES` | `max_header_bytes` |
| `PROXY_AUTH_MAX_FAILURES` | `auth_max_failures` |
| `PROXY_ALLOW_PRIVATE_DESTINATIONS` | `allow_private_destinations` |
| `PROXY_BLOCKED_DESTINATION_PORTS` | `blocked_destination_ports` (comma-separated ports or `none`) |
| `PROXY_DIAL_TIMEOUT` | `dial_timeout` |
| `PROXY_TLS_HANDSHAKE_TIMEOUT` | `tls_handshake_timeout` |
| `PROXY_TLS_RELOAD_INTERVAL` | `tls_reload_interval` |
| `PROXY_RESPONSE_HEADER_TIMEOUT` | `response_header_timeout` |
| `PROXY_READ_HEADER_TIMEOUT` | `read_header_timeout` |
| `PROXY_IDLE_TIMEOUT` | `idle_timeout` |
| `PROXY_TUNNEL_IDLE_TIMEOUT` | `tunnel_idle_timeout` |
| `PROXY_AUTH_FAILURE_WINDOW` | `auth_failure_window` |
| `PROXY_AUTH_BLOCK_DURATION` | `auth_block_duration` |
| `PROXY_SHUTDOWN_TIMEOUT` | `shutdown_timeout` |

### Docker Compose (HTTP + HTTPS with Let's Encrypt)

The bundled [docker-compose.yml](docker-compose.yml) starts an HTTP proxy, an HTTPS proxy, and a `certbot` sidecar that issues and auto-renews Let's Encrypt certificates into a shared volume. All settings come from a `.env` file — no YAML editing required.

1. Copy the env template and fill it in:

    ```sh
    cp .env.example .env
    # edit DOMAIN, EMAIL, PROXY_USERNAME, PROXY_PASSWORD
    ```

2. Issue the initial Let's Encrypt certificate (port 80 must be reachable on `$DOMAIN`):

    ```sh
    docker compose run --rm --service-ports certbot issue
    ```

3. Start the stack:

    ```sh
    docker compose up -d
    ```

Certbot checks for renewals every 12 hours. On new TLS handshakes, the HTTPS proxy checks the mounted certificate files at most once per `PROXY_TLS_RELOAD_INTERVAL` (one minute by default). A valid replacement is loaded without restarting the container or interrupting existing connections. If Certbot is temporarily updating the certificate/key pair or the new files are invalid, the proxy keeps the last valid certificate and retries later.

### Build from source

1. Clone the repository:

    ```sh
    git clone https://github.com/hightemp/https_proxy
    cd https_proxy
    ```

2. Build the project:

    ```sh
    make build
    ```

### Project structure

```text
cmd/https_proxy/  application entry point
internal/config/  configuration loading and validation
internal/auth/    proxy authentication
internal/proxy/   HTTP forwarding, upstream chaining, and server lifecycle
internal/tunnel/  CONNECT tunnel tracking and bidirectional relay
```

## Configuration

Create a `config.yaml` file (see `config.example.yaml`):

```yaml
proxy_addr: 127.0.0.1:8080
username: "your_username"
password: "your_password"
proto: http
cert_path: ""
key_path: ""
network: auto
log_sensitive_data: false
max_connections: 1024
max_connections_per_ip: 64
max_tunnels: 256
max_tunnels_per_ip: 16
max_header_bytes: 65536
auth_max_failures: 10
allow_private_destinations: false
blocked_destination_ports: [21, 22, 23, 25, 110, 111, 135, 137, 138, 139, 445, 1433, 2049, 2375, 2376, 3306, 3389, 5432, 5900, 6379, 9200, 11211, 27017]
dial_timeout: 10s
tls_handshake_timeout: 10s
tls_reload_interval: 1m
response_header_timeout: 30s
read_header_timeout: 15s
idle_timeout: 2m
tunnel_idle_timeout: 10m
auth_failure_window: 1m
auth_block_duration: 5m
shutdown_timeout: 15s
# upstream_proxy: http://user:pass@upstream-proxy:8080
# upstream_proxy: direct
```

The example listens on localhost. Set `proxy_addr` to `0.0.0.0:8080` only when the proxy must accept remote connections, and configure authentication before exposing it.

| Parameter | Description |
|---|---|
| `proxy_addr` | Listen address and port |
| `username` | Basic auth username |
| `password` | Basic auth password |
| `proto` | `http` or `https` |
| `cert_path` | Path to TLS certificate (for `https` mode) |
| `key_path` | Path to TLS private key (for `https` mode) |
| `upstream_proxy` | Upstream proxy URL, `direct`, or empty to use proxy environment variables |
| `network` | Outbound address family: `auto`, `tcp4`, or `tcp6` |
| `log_sensitive_data` | Log full request URLs, upstream errors, and rejected Basic Auth username/password; disabled by default |
| `max_connections` | Maximum simultaneous client TCP connections |
| `max_connections_per_ip` | Maximum simultaneous client connections per source IP |
| `max_tunnels` | Maximum simultaneous CONNECT tunnels |
| `max_tunnels_per_ip` | Maximum simultaneous CONNECT tunnels per source IP |
| `max_header_bytes` | Maximum size of incoming HTTP request headers |
| `auth_max_failures` | Failed authentication attempts per IP before temporary blocking |
| `allow_private_destinations` | Allow loopback, private, link-local, and other non-public destinations |
| `blocked_destination_ports` | Ports denied by destination policy; use `[]` to clear the YAML list |
| `dial_timeout` | TCP connection timeout |
| `tls_handshake_timeout` | Outbound TLS handshake timeout |
| `tls_reload_interval` | How often new TLS handshakes check certificate files for a valid replacement |
| `response_header_timeout` | Upstream CONNECT/HTTP response-header timeout |
| `read_header_timeout` | Incoming request-header timeout |
| `idle_timeout` | Incoming keep-alive idle timeout |
| `tunnel_idle_timeout` | Close a CONNECT tunnel after no traffic in either direction |
| `auth_failure_window` | Window in which failed authentication attempts are counted |
| `auth_block_duration` | How long an IP is blocked after exceeding `auth_max_failures` |
| `shutdown_timeout` | Graceful shutdown deadline |

Timeout values use Go duration syntax, for example `500ms`, `10s`, or `2m`. Unknown YAML keys and invalid values stop the proxy at startup instead of being silently ignored.

By default, request URL userinfo and query parameters are removed from logs, upstream errors are reduced to their HTTP category, and rejected Basic Auth credentials are not logged. For temporary diagnostics, set `log_sensitive_data: true` or `PROXY_LOG_SENSITIVE_DATA=true`. This can expose passwords and tokens in plaintext logs; disable it immediately after debugging.

### Resource limits and destination policy

Connection and CONNECT limits are enforced globally and per source IP. Excess TCP connections are closed immediately; excess tunnels and rate-limited authentication attempts receive `429 Too Many Requests`. A tunnel is closed when no bytes flow in either direction for `tunnel_idle_timeout`.

The default destination policy rejects loopback, private, link-local, metadata, carrier-grade NAT, and other reserved addresses. It also blocks common administration, mail, database, and cache ports. To intentionally proxy internal services, set `allow_private_destinations: true`. To clear only the blocked port list, use `blocked_destination_ports: []` in YAML or `PROXY_BLOCKED_DESTINATION_PORTS=none`.

### Outbound network

`network: auto` uses Go's normal dual-stack dialing. If the server advertises IPv6 but its IPv6 route is broken, use IPv4-only dialing so affected requests fail over immediately:

```yaml
network: tcp4
```

The setting applies to direct CONNECT targets, ordinary forwarded HTTP requests, and the connection to an upstream proxy. When chaining through an upstream proxy, that upstream still resolves and connects to the final target itself.

### Upstream Proxy (Proxy Chain)

To route all traffic through an upstream proxy, set `upstream_proxy` in `config.yaml`:

```yaml
upstream_proxy: http://user:pass@upstream-proxy:8080
```

HTTPS upstream proxies are also supported:

```yaml
upstream_proxy: https://user:pass@upstream-proxy:8443
```

If `upstream_proxy` is not set in the config, the proxy falls back to standard environment variables (`HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY`).

To force direct connections and ignore those environment variables, use the explicit `direct` mode:

```yaml
upstream_proxy: direct
```

An explicitly configured upstream URL is validated at startup and never silently falls back to a direct connection. Percent-encode reserved characters in credentials, for example `user%40example` for `user@example` and `p%3Ass` for `p:ss`.

### TLS Certificates

Generate self-signed certificates:

```bash
bash generate_certs.sh
```

The generated certificate includes Subject Alternative Names for `localhost`, `127.0.0.1`, and `::1`, which are required by modern TLS clients. Trust `cert.pem` locally before using it; the certificate is self-signed.

Or use Let's Encrypt:

```bash
sudo certbot certonly --standalone -d example.com
```

```yaml
cert_path: "/etc/letsencrypt/live/example.com/fullchain.pem"
key_path: "/etc/letsencrypt/live/example.com/privkey.pem"
```

## Usage

```sh
./https_proxy -config config.yaml
```

### Systemd Service

```sh
sudo make install
```

The installer creates a locked system account named `https_proxy`; the service does not run as root. The configuration remains owned by `root`, is readable by the `https_proxy` group, and is not writable by the service. The unit also isolates devices and home directories, denies Linux capabilities, and prevents namespace creation.

TLS files used by the system service must be readable by the `https_proxy` group. A protected directory is created automatically; install certificates without making the private key world-readable:

```sh
sudo install -o root -g https_proxy -m 0640 cert.pem /etc/https_proxy/certs/cert.pem
sudo install -o root -g https_proxy -m 0640 key.pem /etc/https_proxy/certs/key.pem
```

Then use these paths in `/etc/https_proxy/config.yaml`:

```yaml
cert_path: /etc/https_proxy/certs/cert.pem
key_path: /etc/https_proxy/certs/key.pem
```

If Certbot manages the source certificate, deploy a copy with these ownership and mode settings from a renewal hook. The proxy detects the replacement automatically; no service restart is required. Direct paths under `/home` are intentionally inaccessible to the hardened unit.

Manage the service:

```sh
make start / stop / restart / status
```

## Makefile Commands

| Command | Description |
|---|---|
| `make build` | Build the binary |
| `make build-static` | Build a static binary (linux/amd64) |
| `make run` | Run the proxy |
| `make install` | Install binary, config and systemd service |
| `make uninstall` | Remove binary and service (keep config) |
| `make uninstall-full` | Remove everything including config |
| `make release` | Tag version from `VERSION` file and push |
| `make docker-build` | Build Docker image `hightemp/https_proxy:VERSION` and `:latest` |
| `make docker-push` | Build and push image to Docker Hub |
| `make docker-release` | Alias for `docker-push` |

## Testing

Run the complete test suite with the race detector:

```sh
go test -race ./...
```

Generate a local coverage summary:

```sh
go test -covermode=atomic -coverprofile=coverage.out ./...
go tool cover -func=coverage.out
```

The test workflow runs for pushes and pull requests and enforces at least 85% total statement coverage.

## Release

1. Update the version in the `VERSION` file.
2. Run:

    ```sh
    make release
    ```

   This will commit, create a git tag `vX.Y.Z`, and push it. GitHub Actions will automatically build binaries and create a release.

## License

This project is licensed under the MIT License.
