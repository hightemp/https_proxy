package auth

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBasicAuthenticate(t *testing.T) {
	validHeader := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:secret"))
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
		{name: "valid credentials", username: "alice", password: "secret", header: validHeader, wantOK: true},
		{name: "missing header", username: "alice", password: "secret", wantStatus: http.StatusProxyAuthRequired, wantChallenge: true},
		{name: "wrong password", username: "alice", password: "secret", header: "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:wrong")), wantStatus: http.StatusProxyAuthRequired, wantChallenge: true},
		{name: "unsupported scheme", username: "alice", password: "secret", header: "Bearer token", wantStatus: http.StatusProxyAuthRequired, wantChallenge: true},
		{name: "invalid base64", username: "alice", password: "secret", header: "Basic !!!", wantStatus: http.StatusBadRequest},
		{name: "invalid payload", username: "alice", password: "secret", header: "Basic " + base64.StdEncoding.EncodeToString([]byte("alice")), wantStatus: http.StatusProxyAuthRequired, wantChallenge: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authenticator := NewBasic(test.username, test.password)
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
