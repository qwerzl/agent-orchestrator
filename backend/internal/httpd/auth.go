package httpd

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
)

// authCookieName is the cookie the web app presents on requests that cannot
// carry an Authorization header — notably the EventSource stream
// (/api/v1/events) and the /mux WebSocket. A token-login endpoint sets it after
// validating the shared token. The bearer header and a ?token= query parameter
// are also accepted, the latter for clients (EventSource, some WebSocket
// stacks) that can set neither header nor cookie cross-origin.
const authCookieName = "ao_token"

// authMiddleware enforces the shared bearer token on the daemon's remote
// surface. When cfg.AuthToken is empty the daemon is in loopback no-auth mode
// and the middleware is a pass-through, preserving historical behavior.
//
// When a token is configured, requests to authProtectedPath routes
// (/api/v1/* and /mux) must present the matching token via the Authorization
// header, the ao_token cookie, or a ?token= query parameter. Health probes,
// CORS preflight, and the loopback-gated control/telemetry endpoints are left
// alone — the latter defend themselves with localControlRequest.
func authMiddleware(cfg config.Config) func(http.Handler) http.Handler {
	want := cfg.AuthToken
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if want == "" || r.Method == http.MethodOptions || !authProtectedPath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			if !validToken(tokenFromRequest(r), want) {
				envelope.WriteAPIError(w, r, http.StatusUnauthorized, "unauthorized", "UNAUTHORIZED",
					"missing or invalid auth token", nil)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// authProtectedPath reports whether a route requires the auth token when one is
// configured. The REST API and the terminal mux carry session-controlling
// power and must be gated; /healthz, /readyz, /shutdown, and
// /internal/telemetry/* are deliberately excluded (the last two keep their
// loopback-only localControlRequest gate).
func authProtectedPath(path string) bool {
	if path == "/mux" {
		return true
	}
	return strings.HasPrefix(path, "/api/")
}

// tokenFromRequest extracts a presented token from, in order of preference, the
// Authorization bearer header, the ao_token cookie, then a ?token= query
// parameter.
func tokenFromRequest(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if rest, ok := strings.CutPrefix(h, "Bearer "); ok {
			return strings.TrimSpace(rest)
		}
	}
	if c, err := r.Cookie(authCookieName); err == nil && c.Value != "" {
		return c.Value
	}
	return r.URL.Query().Get("token")
}

// validToken compares the presented and configured tokens in constant time. An
// empty presented or configured token is always invalid.
func validToken(got, want string) bool {
	if got == "" || want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
