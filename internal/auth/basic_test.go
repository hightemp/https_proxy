package auth

import (
	"bytes"
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBasicAuthenticate(t *testing.T) {
	basicHeader := func(username, password string) string {
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))
	}
	tests := []struct {
		name          string
		username      string
		password      string
		header        string
		wantOK        bool
		wantStatus    int
		wantChallenge bool
	}{
		{name: "disabled", wantOK: true},
		{name: "valid credentials", username: "alice", password: "secret", header: basicHeader("alice", "secret"), wantOK: true},
		{name: "valid empty username", password: "secret", header: basicHeader("", "secret"), wantOK: true},
		{name: "valid empty password", username: "alice", header: basicHeader("alice", ""), wantOK: true},
		{name: "missing header", username: "alice", password: "secret", wantStatus: http.StatusProxyAuthRequired, wantChallenge: true},
		{name: "wrong username", username: "alice", password: "secret", header: basicHeader("bob", "secret"), wantStatus: http.StatusProxyAuthRequired, wantChallenge: true},
		{name: "wrong password", username: "alice", password: "secret", header: basicHeader("alice", "wrong"), wantStatus: http.StatusProxyAuthRequired, wantChallenge: true},
		{name: "unsupported scheme", username: "alice", password: "secret", header: "Bearer token", wantStatus: http.StatusProxyAuthRequired, wantChallenge: true},
		{name: "invalid base64", username: "alice", password: "secret", header: "Basic !!!", wantStatus: http.StatusProxyAuthRequired, wantChallenge: true},
		{name: "invalid payload", username: "alice", password: "secret", header: "Basic " + base64.StdEncoding.EncodeToString([]byte("alice")), wantStatus: http.StatusProxyAuthRequired, wantChallenge: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authenticator := NewBasic(test.username, test.password, BasicOptions{})
			request := httptest.NewRequest(http.MethodGet, "http://example.test", nil)
			if test.header != "" {
				request.Header.Set("Proxy-Authorization", test.header)
			}
			response := httptest.NewRecorder()

			if got := authenticator.Authenticate(response, request); got != test.wantOK {
				t.Fatalf("Authenticate() = %t, want %t", got, test.wantOK)
			}
			if test.wantOK {
				return
			}
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			challenge := response.Header().Get("Proxy-Authenticate")
			if test.wantChallenge && challenge == "" {
				t.Fatal("Proxy-Authenticate header is empty")
			}
			if !test.wantChallenge && challenge != "" {
				t.Fatalf("Proxy-Authenticate header = %q, want empty", challenge)
			}
		})
	}
}

func TestBasicAuthenticateSensitiveLogging(t *testing.T) {
	tests := []struct {
		name      string
		sensitive bool
		wantData  bool
	}{
		{name: "redacted by default"},
		{name: "explicitly enabled", sensitive: true, wantData: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			previousLogger := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			defer slog.SetDefault(previousLogger)

			authenticator := NewBasic("expected-user", "correct-password", BasicOptions{LogSensitiveData: test.sensitive})
			request := httptest.NewRequest(http.MethodGet, "http://example.test", nil)
			credentials := base64.StdEncoding.EncodeToString([]byte("attempted-user:wrong-password"))
			request.Header.Set("Proxy-Authorization", "Basic "+credentials)

			if authenticator.Authenticate(httptest.NewRecorder(), request) {
				t.Fatal("Authenticate() = true, want false")
			}
			for _, sensitiveValue := range []string{"attempted-user", "wrong-password"} {
				containsData := strings.Contains(logs.String(), sensitiveValue)
				if containsData != test.wantData {
					t.Fatalf("log contains %q = %t, want %t; log: %s", sensitiveValue, containsData, test.wantData, logs.String())
				}
			}
		})
	}
}

func TestBasicAuthenticateRateLimitsFailuresPerIP(t *testing.T) {
	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	authenticator := NewBasic("alice", "correct", BasicOptions{
		MaxFailures:   2,
		FailureWindow: time.Minute,
		BlockDuration: 5 * time.Minute,
	})
	authenticator.failures.now = func() time.Time { return now }

	authenticate := func(remoteAddress, password string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "http://example.test", nil)
		request.RemoteAddr = remoteAddress
		credentials := base64.StdEncoding.EncodeToString([]byte("alice:" + password))
		request.Header.Set("Proxy-Authorization", "Basic "+credentials)
		response := httptest.NewRecorder()
		authenticator.Authenticate(response, request)
		return response
	}

	for attempt := 1; attempt <= 2; attempt++ {
		if response := authenticate("192.0.2.10:1234", "wrong"); response.Code != http.StatusProxyAuthRequired {
			t.Fatalf("failed attempt %d status = %d, want %d", attempt, response.Code, http.StatusProxyAuthRequired)
		}
	}
	if response := authenticate("192.0.2.10:5678", "correct"); response.Code != http.StatusTooManyRequests {
		t.Fatalf("blocked attempt status = %d, want %d", response.Code, http.StatusTooManyRequests)
	} else if response.Header().Get("Retry-After") != "300" {
		t.Fatalf("Retry-After = %q, want %q", response.Header().Get("Retry-After"), "300")
	}
	if response := authenticate("192.0.2.11:1234", "correct"); response.Code != http.StatusOK {
		t.Fatalf("other IP status = %d, want %d", response.Code, http.StatusOK)
	}

	now = now.Add(5 * time.Minute)
	if response := authenticate("192.0.2.10:1234", "correct"); response.Code != http.StatusOK {
		t.Fatalf("attempt after block status = %d, want %d", response.Code, http.StatusOK)
	}
}
