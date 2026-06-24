package httpd

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
)

// TestAuthMiddlewareLoopbackMode confirms that with no token configured the
// daemon stays in its historical no-auth loopback mode: protected routes are
// reachable without any credential.
func TestAuthMiddlewareLoopbackMode(t *testing.T) {
	router := newTestRouter(config.Config{AllowedOrigins: []string{"app://renderer"}}, discardLogger(), nil)
	srv := httptest.NewServer(router)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/sessions")
	if err != nil {
		t.Fatalf("GET /api/v1/sessions: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatalf("loopback mode returned 401; want the request to reach the handler")
	}
}

// TestAuthMiddlewareEnforced exercises the token boundary when AO_AUTH_TOKEN is
// configured: protected routes demand the token via header, cookie, or query;
// health probes stay open; the wrong token is rejected.
func TestAuthMiddlewareEnforced(t *testing.T) {
	const token = "s3cret-token"
	cfg := config.Config{AuthToken: token, AllowedOrigins: []string{"app://renderer"}}
	router := newTestRouter(cfg, discardLogger(), nil)
	srv := httptest.NewServer(router)
	defer srv.Close()

	tests := []struct {
		name        string
		path        string
		setAuth     func(*http.Request)
		wantStatus  int  // exact status when set
		wantNot401  bool // otherwise: assert the request passed auth
	}{
		{
			name:       "protected route without token is 401",
			path:       "/api/v1/sessions",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "protected route with wrong bearer is 401",
			path:       "/api/v1/sessions",
			setAuth:    func(r *http.Request) { r.Header.Set("Authorization", "Bearer nope") },
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "protected route with valid bearer passes auth",
			path:       "/api/v1/sessions",
			setAuth:    func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) },
			wantNot401: true,
		},
		{
			name:       "protected route with valid cookie passes auth",
			path:       "/api/v1/sessions",
			setAuth:    func(r *http.Request) { r.AddCookie(&http.Cookie{Name: authCookieName, Value: token}) },
			wantNot401: true,
		},
		{
			name:       "protected route with valid query token passes auth",
			path:       "/api/v1/sessions?token=" + token,
			wantNot401: true,
		},
		{
			name:       "mux without token is 401",
			path:       "/mux",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "healthz stays open without a token",
			path:       "/healthz",
			wantStatus: http.StatusOK,
		},
		{
			name:       "readyz stays open without a token",
			path:       "/readyz",
			wantStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, srv.URL+tt.path, nil)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			if tt.setAuth != nil {
				tt.setAuth(req)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("GET %s: %v", tt.path, err)
			}
			defer resp.Body.Close()

			if tt.wantNot401 {
				if resp.StatusCode == http.StatusUnauthorized {
					t.Fatalf("status = 401, want the request to pass auth")
				}
				return
			}
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
		})
	}
}
