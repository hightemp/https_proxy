// Package auth implements proxy client authentication.
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// BasicOptions controls authentication logging and failed-attempt limiting.
type BasicOptions struct {
	LogSensitiveData bool
	MaxFailures      int
	FailureWindow    time.Duration
	BlockDuration    time.Duration
}

// Basic authenticates proxy requests against one configured credential pair.
type Basic struct {
	enabled          bool
	logSensitiveData bool
	failures         *failureLimiter
	usernameHash     [sha256.Size]byte
	passwordHash     [sha256.Size]byte
}

// NewBasic creates a Basic authenticator. Authentication is disabled when both
// username and password are empty. Sensitive logging includes rejected
// credentials and should only be enabled temporarily for diagnostics.
func NewBasic(username, password string, options BasicOptions) *Basic {
	return &Basic{
		enabled:          username != "" || password != "",
		logSensitiveData: options.LogSensitiveData,
		failures:         newFailureLimiter(options.MaxFailures, options.FailureWindow, options.BlockDuration),
		usernameHash:     sha256.Sum256([]byte(username)),
		passwordHash:     sha256.Sum256([]byte(password)),
	}
}

// Authenticate validates Proxy-Authorization and writes an error response when
// authentication fails.
func (a *Basic) Authenticate(w http.ResponseWriter, r *http.Request) bool {
	if !a.enabled {
		return true
	}
	client := clientKey(r.RemoteAddr)
	if retryAfter := a.failures.retryAfter(client); retryAfter > 0 {
		slog.Warn("Authentication rate limit exceeded", "remote", r.RemoteAddr, "retry_after", retryAfter.Round(time.Second))
		writeRateLimited(w, retryAfter)
		return false
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
		return a.reject(w, client)
	}
	payload, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		slog.Warn("Error decoding auth", "error", err, "remote", r.RemoteAddr)
		return a.reject(w, client)
	}

	pair := strings.SplitN(string(payload), ":", 2)
	if len(pair) != 2 {
		slog.Warn("Invalid auth format", "remote", r.RemoteAddr)
		return a.reject(w, client)
	}

	usernameHash := sha256.Sum256([]byte(pair[0]))
	passwordHash := sha256.Sum256([]byte(pair[1]))
	usernameMatch := subtle.ConstantTimeCompare(usernameHash[:], a.usernameHash[:])
	passwordMatch := subtle.ConstantTimeCompare(passwordHash[:], a.passwordHash[:])
	if usernameMatch&passwordMatch != 1 {
		if a.logSensitiveData {
			slog.Warn("Invalid credentials", "username", pair[0], "password", pair[1], "remote", r.RemoteAddr)
		} else {
			slog.Warn("Invalid credentials", "remote", r.RemoteAddr)
		}
		return a.reject(w, client)
	}

	a.failures.reset(client)
	return true
}

func (a *Basic) reject(w http.ResponseWriter, client string) bool {
	a.failures.recordFailure(client)
	writeRequired(w)
	return false
}

func clientKey(remoteAddress string) string {
	host, _, err := net.SplitHostPort(remoteAddress)
	if err == nil {
		return host
	}
	return remoteAddress
}

func writeRequired(w http.ResponseWriter) {
	w.Header().Set("Proxy-Authenticate", `Basic realm="Proxy Authorization Required"`)
	w.WriteHeader(http.StatusProxyAuthRequired)
}

func writeRateLimited(w http.ResponseWriter, retryAfter time.Duration) {
	seconds := int((retryAfter + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	http.Error(w, http.StatusText(http.StatusTooManyRequests), http.StatusTooManyRequests)
}
