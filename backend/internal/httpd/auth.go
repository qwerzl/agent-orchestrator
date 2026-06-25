package httpd

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

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
	// The login endpoint must be reachable without the token so a browser can
	// exchange it for the ao_token cookie (EventSource / the mux WebSocket cannot
	// send an Authorization header).
	if path == authLoginPath {
		return false
	}
	if path == "/mux" {
		return true
	}
	return strings.HasPrefix(path, "/api/")
}

// authLoginPath is the unauthenticated token-exchange endpoint.
const authLoginPath = "/api/v1/auth/login"

// mountAuthLogin registers the token-login endpoint when auth is enabled. The
// web app POSTs {"token":"..."}; a valid token is echoed back as an HttpOnly
// ao_token cookie that subsequent SSE/mux requests present. When no token is
// configured (loopback no-auth mode) the route is not mounted.
func mountAuthLogin(r chi.Router, cfg config.Config) {
	if cfg.AuthToken == "" {
		return
	}
	r.Post(authLoginPath, func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			envelope.WriteAPIError(w, req, http.StatusBadRequest, "bad_request", "INVALID_JSON", "request body must be valid JSON", nil)
			return
		}
		if !validToken(strings.TrimSpace(body.Token), cfg.AuthToken) {
			envelope.WriteAPIError(w, req, http.StatusUnauthorized, "unauthorized", "UNAUTHORIZED", "invalid auth token", nil)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     authCookieName,
			Value:    cfg.AuthToken,
			Path:     "/",
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   7 * 24 * 60 * 60,
		})
		envelope.WriteJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})
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
