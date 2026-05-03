# Secure HTTP Proxy Server

[![Go Version](https://img.shields.io/github/go-mod/go-version/hightemp/https_proxy)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![GitHub release](https://img.shields.io/github/v/release/hightemp/https_proxy)](https://github.com/hightemp/https_proxy/releases)
[![GitHub Downloads](https://img.shields.io/github/downloads/hightemp/https_proxy/total)](https://github.com/hightemp/https_proxy/releases)
[![GitHub Workflow Status](https://img.shields.io/github/actions/workflow/status/hightemp/https_proxy/release.yml)](https://github.com/hightemp/https_proxy/actions)
[![Go Report Card](https://goreportcard.com/badge/github.com/hightemp/https_proxy)](https://goreportcard.com/report/github.com/hightemp/https_proxy)

A secure HTTP/HTTPS proxy server in Go with Basic authentication, TLS support, and upstream proxy chaining.

## Features

- HTTP and HTTPS proxy modes
- Basic authentication
- TLS with configurable certificates
- Upstream proxy chaining (proxy chain support)
  - Supports HTTP and HTTPS upstream proxies
  - Configurable via `config.yaml` or environment variables (`HTTPS_PROXY`, `HTTP_PROXY`)
  - Basic authentication to upstream proxy
- Configurable via YAML file
- Systemd service support
- Graceful shutdown

## Installation

### From release

Download the latest binary from the [Releases](https://github.com/hightemp/https_proxy/releases) page.

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

## Configuration

Create a `config.yaml` file (see `config.example.yaml`):

```yaml
proxy_addr: 0.0.0.0:8080
username: "your_username"
password: "your_password"
proto: https
cert_path: "path/to/your/cert.pem"
key_path: "path/to/your/key.pem"
# upstream_proxy: http://user:pass@upstream-proxy:8080
```

| Parameter | Description |
|---|---|
| `proxy_addr` | Listen address and port |
| `username` | Basic auth username |
| `password` | Basic auth password |
| `proto` | `http` or `https` |
| `cert_path` | Path to TLS certificate (for `https` mode) |
| `key_path` | Path to TLS private key (for `https` mode) |
| `upstream_proxy` | Upstream proxy URL for chaining (optional) |

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

## Release

1. Update the version in the `VERSION` file.
2. Run:

    ```sh
    make release
    ```

   This will commit, create a git tag `vX.Y.Z`, and push it. GitHub Actions will automatically build binaries and create a release.

## License

This project is licensed under the MIT License.

[![](https://asdertasd.site/counter/https_proxy?a=1)](https://asdertasd.site/counter/https_proxy)