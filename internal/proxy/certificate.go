package proxy

import (
	"crypto/sha256"
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

type certificateFingerprint struct {
	certificate [sha256.Size]byte
	privateKey  [sha256.Size]byte
}

type certificateReloader struct {
	mu              sync.Mutex
	certificatePath string
	keyPath         string
	reloadInterval  time.Duration
	nextCheck       time.Time
	certificate     *tls.Certificate
	fingerprint     certificateFingerprint
}

func newCertificateReloader(certificatePath, keyPath string, reloadInterval time.Duration) (*certificateReloader, error) {
	certificate, fingerprint, err := loadCertificatePair(certificatePath, keyPath)
	if err != nil {
		return nil, err
	}
	return &certificateReloader{
		certificatePath: certificatePath,
		keyPath:         keyPath,
		reloadInterval:  reloadInterval,
		nextCheck:       time.Now().Add(reloadInterval),
		certificate:     certificate,
		fingerprint:     fingerprint,
	}, nil
}

func (r *certificateReloader) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	if now.Before(r.nextCheck) {
		return r.certificate, nil
	}
	r.nextCheck = now.Add(r.reloadInterval)

	certificate, fingerprint, err := loadCertificatePair(r.certificatePath, r.keyPath)
	if err != nil {
		slog.Warn("Could not reload TLS certificate; keeping the previous certificate", "error", err)
		return r.certificate, nil
	}
	if fingerprint == r.fingerprint {
		return r.certificate, nil
	}

	r.certificate = certificate
	r.fingerprint = fingerprint
	slog.Info("TLS certificate reloaded")
	return r.certificate, nil
}

func loadCertificatePair(certificatePath, keyPath string) (*tls.Certificate, certificateFingerprint, error) {
	certificatePEM, err := os.ReadFile(certificatePath)
	if err != nil {
		return nil, certificateFingerprint{}, fmt.Errorf("read TLS certificate: %w", err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, certificateFingerprint{}, fmt.Errorf("read TLS private key: %w", err)
	}
	defer clear(keyPEM)
	certificate, err := tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil {
		return nil, certificateFingerprint{}, fmt.Errorf("parse TLS certificate pair: %w", err)
	}
	fingerprint := certificateFingerprint{
		certificate: sha256.Sum256(certificatePEM),
		privateKey:  sha256.Sum256(keyPEM),
	}
	return &certificate, fingerprint, nil
}
