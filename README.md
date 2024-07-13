# Secure HTTP Proxy Server

This is a simple implementation of a secure HTTP proxy server in Go. The proxy server uses basic authentication and supports HTTP tunneling via the `CONNECT` method.

## Features

- Secure TLS connection
- Basic authentication
- HTTP tunneling with `CONNECT` method
- Configurable via a YAML file

## Requirements

- Go 1.16 or higher

## Installation

1. Clone the repository:

    ```sh
    git clone https://github.com/hightemp/https_proxy
    cd https_proxy
    ```

2. Build the project:

    ```sh
    go build -o https_proxy main.go
    ```

3. Create a `config.yaml` file with the following content:

    ```yaml
    proxy_addr: ":8080"
    username: "your_username"
    password: "your_password"
    cert_path: "path/to/your/cert.pem"
    key_path: "path/to/your/key.pem"
    ```

## Usage

Start the proxy server with the path to your configuration file:

```sh
./proxy-server -config config.yaml
```

## License

This project is licensed under the MIT License.