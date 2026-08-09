package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hightemp/https_proxy/internal/config"
)

func TestCertificateReloaderReloadsChangedPair(t *testing.T) {
	certificatePath := filepath.Join(t.TempDir(), "certificate.pem")
	keyPath := filepath.Join(filepath.Dir(certificatePath), "private-key.pem")
	writeTestCertificatePair(t, certificatePath, keyPath, 1)

	reloader, err := newCertificateReloader(certificatePath, keyPath, time.Minute)
	if err != nil {
		t.Fatalf("newCertificateReloader() error = %v", err)
	}
	initial, err := reloader.getCertificate(nil)
	if err != nil {
		t.Fatalf("get initial certificate: %v", err)
	}

	writeTestCertificatePair(t, certificatePath, keyPath, 2)
	beforeScheduledCheck, err := reloader.getCertificate(nil)
	if err != nil {
		t.Fatalf("get certificate before scheduled check: %v", err)
	}
	if beforeScheduledCheck != initial {
		t.Fatal("certificate was checked before the reload interval elapsed")
	}
	reloader.nextCheck = time.Time{}
	reloaded, err := reloader.getCertificate(nil)
	if err != nil {
		t.Fatalf("get reloaded certificate: %v", err)
	}
	if initial == reloaded {
		t.Fatal("certificate pointer was not replaced")
	}
	if serial := certificateSerial(t, reloaded); serial != 2 {
		t.Fatalf("reloaded certificate serial = %d, want 2", serial)
	}
}

func TestCertificateReloaderKeepsLastValidPair(t *testing.T) {
	certificatePath := filepath.Join(t.TempDir(), "certificate.pem")
	keyPath := filepath.Join(filepath.Dir(certificatePath), "private-key.pem")
	writeTestCertificatePair(t, certificatePath, keyPath, 1)

	reloader, err := newCertificateReloader(certificatePath, keyPath, 0)
	if err != nil {
		t.Fatalf("newCertificateReloader() error = %v", err)
	}
	initial, err := reloader.getCertificate(nil)
	if err != nil {
		t.Fatalf("get initial certificate: %v", err)
	}
	if err := os.WriteFile(keyPath, []byte("incomplete renewal"), 0o600); err != nil {
		t.Fatalf("write invalid private key: %v", err)
	}

	retained, err := reloader.getCertificate(nil)
	if err != nil {
		t.Fatalf("get certificate during invalid renewal: %v", err)
	}
	if retained != initial {
		t.Fatal("invalid replacement discarded the last valid certificate")
	}

	writeTestCertificatePair(t, certificatePath, keyPath, 2)
	reloaded, err := reloader.getCertificate(nil)
	if err != nil {
		t.Fatalf("get certificate after valid renewal: %v", err)
	}
	if serial := certificateSerial(t, reloaded); serial != 2 {
		t.Fatalf("reloaded certificate serial = %d, want 2", serial)
	}
}

func TestCertificateReloaderIsSafeForConcurrentHandshakes(t *testing.T) {
	certificatePath := filepath.Join(t.TempDir(), "certificate.pem")
	keyPath := filepath.Join(filepath.Dir(certificatePath), "private-key.pem")
	writeTestCertificatePair(t, certificatePath, keyPath, 1)

	reloader, err := newCertificateReloader(certificatePath, keyPath, 0)
	if err != nil {
		t.Fatalf("newCertificateReloader() error = %v", err)
	}
	writeTestCertificatePair(t, certificatePath, keyPath, 2)

	const handshakes = 32
	results := make(chan *tls.Certificate, handshakes)
	var waitGroup sync.WaitGroup
	for range handshakes {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			certificate, getErr := reloader.getCertificate(nil)
			if getErr != nil {
				t.Errorf("getCertificate() error = %v", getErr)
				return
			}
			results <- certificate
		}()
	}
	waitGroup.Wait()
	close(results)

	for certificate := range results {
		if serial := certificateSerial(t, certificate); serial != 2 {
			t.Fatalf("certificate serial = %d, want 2", serial)
		}
	}
}

func TestTLSListenerServesRenewedCertificate(t *testing.T) {
	directory := t.TempDir()
	certificatePath := filepath.Join(directory, "certificate.pem")
	keyPath := filepath.Join(directory, "private-key.pem")
	writeTestCertificatePair(t, certificatePath, keyPath, 1)
	cfg := config.Default()
	cfg.ProxyAddr = "127.0.0.1:0"
	cfg.Proto = "https"
	cfg.CertPath = certificatePath
	cfg.KeyPath = keyPath
	cfg.TLSReloadInterval = config.Duration(time.Nanosecond)
	server := newHTTPServer(cfg, http.NotFoundHandler())
	listener, err := makeListener(cfg, server)
	if err != nil {
		t.Fatalf("makeListener() error = %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	assertServedSerial(t, listener, certificatePath, 1)
	writeTestCertificatePair(t, certificatePath, keyPath, 2)
	assertServedSerial(t, listener, certificatePath, 2)
}

func assertServedSerial(t *testing.T, listener net.Listener, trustedCertificatePath string, wantSerial int64) {
	t.Helper()
	serverResult := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			serverResult <- fmt.Errorf("accept TLS connection: %w", err)
			return
		}
		defer func() { _ = connection.Close() }()
		tlsConnection, ok := connection.(*tls.Conn)
		if !ok {
			serverResult <- fmt.Errorf("accepted connection type %T is not *tls.Conn", connection)
			return
		}
		serverResult <- tlsConnection.Handshake()
	}()

	trustedCertificate, err := os.ReadFile(trustedCertificatePath)
	if err != nil {
		t.Fatalf("read trusted certificate: %v", err)
	}
	rootCertificates := x509.NewCertPool()
	if !rootCertificates.AppendCertsFromPEM(trustedCertificate) {
		t.Fatal("could not add trusted test certificate")
	}
	connection, err := tls.Dial("tcp", listener.Addr().String(), &tls.Config{
		RootCAs:    rootCertificates,
		ServerName: "localhost",
		MinVersion: tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("dial TLS listener: %v", err)
	}
	if serial := connection.ConnectionState().PeerCertificates[0].SerialNumber.Int64(); serial != wantSerial {
		t.Fatalf("served certificate serial = %d, want %d", serial, wantSerial)
	}
	if err := connection.Close(); err != nil {
		t.Fatalf("close TLS client: %v", err)
	}
	if err := <-serverResult; err != nil {
		t.Fatalf("server TLS handshake: %v", err)
	}
}

func writeTestCertificatePair(t *testing.T, certificatePath, keyPath string, serial int64) {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate private key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	privateKeyDER, err := x509.MarshalECPrivateKey(privateKey)
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER})
	privateKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privateKeyDER})
	if err := os.WriteFile(certificatePath, certificatePEM, 0o600); err != nil {
		t.Fatalf("write certificate: %v", err)
	}
	if err := os.WriteFile(keyPath, privateKeyPEM, 0o600); err != nil {
		t.Fatalf("write private key: %v", err)
	}
}

func certificateSerial(t *testing.T, certificate *tls.Certificate) int64 {
	t.Helper()
	parsed, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return parsed.SerialNumber.Int64()
}
