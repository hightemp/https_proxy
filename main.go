package main

import (
	"crypto/tls"
	"encoding/base64"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	proxyAddr = ":9999"
	username  = "user"
	password  = "pass"
)

func main() {
	server := &http.Server{
		Addr: proxyAddr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !basicAuth(w, r) {
				return
			}
			handleTunneling(w, r)
		}),
		TLSConfig: &tls.Config{
			// Certificates: []tls.Certificate{cert},
		},
	}

	log.Printf("Starting proxy server on %s\n", proxyAddr)
	log.Fatal(server.ListenAndServeTLS("./cert.pem", "./key.pem"))
}

func basicAuth(w http.ResponseWriter, r *http.Request) bool {
	auth := r.Header.Get("Proxy-Authorization")
	if auth == "" {
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
	if len(pair) != 2 || pair[0] != username || pair[1] != password {
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
	io.Copy(destination, source)
}
