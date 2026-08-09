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
| `PROXY_DIAL_TIMEOUT` | `dial_timeout` |
| `PROXY_TLS_HANDSHAKE_TIMEOUT` | `tls_handshake_timeout` |
| `PROXY_RESPONSE_HEADER_TIMEOUT` | `response_header_timeout` |
| `PROXY_READ_HEADER_TIMEOUT` | `read_header_timeout` |
| `PROXY_IDLE_TIMEOUT` | `idle_timeout` |
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

Certbot renews certificates automatically every 12 hours. Restart the HTTPS proxy after a renewal if needed:

```sh
docker compose restart https-proxy
```

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
dial_timeout: 10s
tls_handshake_timeout: 10s
response_header_timeout: 30s
read_header_timeout: 15s
idle_timeout: 2m
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
| `dial_timeout` | TCP connection timeout |
| `tls_handshake_timeout` | Outbound TLS handshake timeout |
| `response_header_timeout` | Upstream CONNECT/HTTP response-header timeout |
| `read_header_timeout` | Incoming request-header timeout |
| `idle_timeout` | Incoming keep-alive idle timeout |
| `shutdown_timeout` | Graceful shutdown deadline |

Timeout values use Go duration syntax, for example `500ms`, `10s`, or `2m`. Unknown YAML keys and invalid values stop the proxy at startup instead of being silently ignored.

By default, request URL userinfo and query parameters are removed from logs, upstream errors are reduced to their HTTP category, and rejected Basic Auth credentials are not logged. For temporary diagnostics, set `log_sensitive_data: true` or `PROXY_LOG_SENSITIVE_DATA=true`. This can expose passwords and tokens in plaintext logs; disable it immediately after debugging.

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

## Release

1. Update the version in the `VERSION` file.
2. Run:

    ```sh
    make release
    ```

   This will commit, create a git tag `vX.Y.Z`, and push it. GitHub Actions will automatically build binaries and create a release.

## License

This project is licensed under the MIT License.
