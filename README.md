# Secure HTTP Proxy Server

This is a simple implementation of a secure HTTP proxy server(can HTTPS and HTTP) in Go.

## Features

- Secure TLS connection
- Basic authentication
- Configurable via a YAML file

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
    proxy_addr: 0.0.0.0:8080
    username: "your_username"
    password: "your_password"
    proto: https
    cert_path: "path/to/your/cert.pem"
    key_path: "path/to/your/key.pem"
    ```

4. Create certificates.

    ```bash
    sudo certbot certonly --standalone -d example.com   
    ```

    Add certs to config:

    ```yaml
    cert_path: "/etc/letsencrypt/live/example.com/fullchain.pem"
    key_path: "/etc/letsencrypt/live/example.com/privkey.pem"
    ```


## Usage

Start the proxy server with the path to your configuration file:

```sh
./https_proxy -config config.yaml
```

## License

This project is licensed under the MIT License.

[![](https://asdertasd.site/counter/https_proxy?a=1)](https://asdertasd.site/counter/https_proxy)