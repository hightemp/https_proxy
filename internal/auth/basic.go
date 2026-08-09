// Package auth implements proxy client authentication.
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"log/slog"
	"net/http"
	"strings"
)

// Basic authenticates proxy requests against one configured credential pair.
type Basic struct {
	enabled      bool
	usernameHash [sha256.Size]byte
	passwordHash [sha256.Size]byte
}

// NewBasic creates a Basic authenticator. Authentication is disabled when both
// username and password are empty.
func NewBasic(username, password string) *Basic {
	return &Basic{
		enabled:      username != "" || password != "",
		usernameHash: sha256.Sum256([]byte(username)),
		passwordHash: sha256.Sum256([]byte(password)),
	}
}

// Authenticate validates Proxy-Authorization and writes an error response when
// authentication fails.
func (a *Basic) Authenticate(w http.ResponseWriter, r *http.Request) bool {
	if !a.enabled {
		return true
	}

	auth := r.Header.Get("Proxy-Authorization")
	if auth == "" {
		slog.Debug("No Proxy-Authorization header", "remote", r.RemoteAddr)
		writeRequired(w)
		return false
	}

	scheme, encoded, ok := strings.Cut(auth, " ")
	if !ok || !strings.EqualFold(scheme, "Basic") {
		slog.Warn("Invalid auth scheme", "remote", r.RemoteAddr)
		writeRequired(w)
		return false
	}
	payload, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		slog.Warn("Error decoding auth", "error", err, "remote", r.RemoteAddr)
		writeRequired(w)
		return false
	}

	pair := strings.SplitN(string(payload), ":", 2)
	if len(pair) != 2 {
		slog.Warn("Invalid auth format", "remote", r.RemoteAddr)
		writeRequired(w)
		return false
	}

	usernameHash := sha256.Sum256([]byte(pair[0]))
	passwordHash := sha256.Sum256([]byte(pair[1]))
	usernameMatch := subtle.ConstantTimeCompare(usernameHash[:], a.usernameHash[:])
	passwordMatch := subtle.ConstantTimeCompare(passwordHash[:], a.passwordHash[:])
	if usernameMatch&passwordMatch != 1 {
		slog.Warn("Invalid credentials", "user", pair[0], "remote", r.RemoteAddr)
		writeRequired(w)
		return false
	}

	return true
}

func writeRequired(w http.ResponseWriter) {
	w.Header().Set("Proxy-Authenticate", `Basic realm="Proxy Authorization Required"`)
	w.WriteHeader(http.StatusProxyAuthRequired)
}
