package main

import (
	"crypto/tls"
	"encoding/base64"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	ProxyAddr string `yaml:"proxy_addr"`
	Username  string `yaml:"username"`
	Password  string `yaml:"password"`
	CertPath  string `yaml:"cert_path"`
	KeyPath   string `yaml:"key_path"`
}

var config Config

func main() {
	configPath := flag.String("config", "config.yaml", "Path to the config file")
	flag.Parse()

	content, err := os.ReadFile(*configPath)
	if err != nil {
		log.Fatalf("Error reading config file: %v", err)
	}

	err = yaml.Unmarshal(content, &config)
	if err != nil {
		log.Fatalf("Error parsing config file: %v", err)
	}

	server := &http.Server{
		Addr: config.ProxyAddr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			log.Println("Received request:", r.Method, r.URL)
			if !basicAuth(w, r) {
				return
			}
			handleTunneling(w, r)
		}),
		TLSConfig: &tls.Config{
			// Certificates: []tls.Certificate{cert},
		},
	}

	log.Printf("Starting proxy server on %s\n", config.ProxyAddr)
	log.Fatal(server.ListenAndServeTLS(config.CertPath, config.KeyPath))
}

func basicAuth(w http.ResponseWriter, r *http.Request) bool {
	auth := r.Header.Get("Proxy-Authorization")
	if auth == "" {
		log.Println("No Proxy-Authorization header")
		w.Header().Set("Proxy-Authenticate", `Basic realm="Proxy Authorization Required"`)
		w.WriteHeader(http.StatusProxyAuthRequired)
		return false
	}

	payload, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, "Basic "))
	if err != nil {
		log.Println("Error decoding auth:", err)
		w.WriteHeader(http.StatusBadRequest)
		return false
	}

	pair := strings.SplitN(string(payload), ":", 2)
	if len(pair) != 2 {
		log.Println("Invalid auth format")
		w.Header().Set("Proxy-Authenticate", `Basic realm="Proxy Authorization Required"`)
		w.WriteHeader(http.StatusProxyAuthRequired)
		return false
	}

	if pair[0] != config.Username || pair[1] != config.Password {
		log.Printf("Invalid credentials: %s:%s\n", pair[0], pair[1])
		w.Header().Set("Proxy-Authenticate", `Basic realm="Proxy Authorization Required"`)
		w.WriteHeader(http.StatusProxyAuthRequired)
		return false
	}

	return true
}

func handleTunneling(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	destConn, err := net.DialTimeout("tcp", r.Host, 10*time.Second)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	go transfer(destConn, clientConn)
	go transfer(clientConn, destConn)
}

func transfer(destination io.WriteCloser, source io.ReadCloser) {
	defer destination.Close()
	defer source.Close()
	bytes, err := io.Copy(destination, source)
	if err != nil {
		log.Printf("Transfer error: %v\n", err)
	} else {
		log.Printf("Transferred %d bytes\n", bytes)
	}
}
