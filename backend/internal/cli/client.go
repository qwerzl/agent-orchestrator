package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/runfile"
)

// commandTimeout bounds a mutating daemon call. Spawns do real work (git
// worktree add, zellij launch, hook install), so it is generous compared to the
// status probe timeout.
const commandTimeout = 2 * time.Minute

// apiError is the subset of the daemon's JSON error envelope the CLI surfaces.
// RequestID is surfaced so a failed command can be correlated with daemon logs.
type apiError struct {
	Message   string `json:"message"`
	Code      string `json:"code"`
	RequestID string `json:"requestId"`
}

// String renders the envelope for the user: "<message> (<code>) [request <id>]",
// omitting whichever parts the daemon left empty.
func (e apiError) String() string {
	msg := e.Message
	if e.Code != "" {
		msg = fmt.Sprintf("%s (%s)", msg, e.Code)
	}
	if e.RequestID != "" {
		msg = fmt.Sprintf("%s [request %s]", msg, e.RequestID)
	}
	return msg
}

// getJSON sends GET /api/v1/<path> to the running daemon and decodes a 2xx
// response into out. A missing daemon or non-2xx API envelope is rendered the
// same way as mutating calls.
func (c *commandContext) getJSON(ctx context.Context, path string, out any) error {
	return c.doJSON(ctx, http.MethodGet, path, nil, out)
}

// postJSON sends body as JSON to POST /api/v1/<path> on the running daemon and
// decodes a 2xx response into out (out may be nil). A non-2xx response becomes
// an error built from the API error envelope. A missing run-file or a stale one
// (dead PID) yields a clear "not running" message rather than a
// connection-refused dump.
func (c *commandContext) postJSON(ctx context.Context, path string, body, out any) error {
	return c.doJSON(ctx, http.MethodPost, path, body, out)
}

// patchJSON sends body as JSON to PATCH /api/v1/<path> on the running daemon
// and decodes a 2xx response into out.
func (c *commandContext) patchJSON(ctx context.Context, path string, body, out any) error {
	return c.doJSON(ctx, http.MethodPatch, path, body, out)
}

// putJSON sends body as JSON to PUT /api/v1/<path> on the running daemon and
// decodes a 2xx response into out.
func (c *commandContext) putJSON(ctx context.Context, path string, body, out any) error {
	return c.doJSON(ctx, http.MethodPut, path, body, out)
}

// deleteJSON sends DELETE /api/v1/<path> to the running daemon and decodes a
// 2xx response into out.
func (c *commandContext) deleteJSON(ctx context.Context, path string, out any) error {
	return c.doJSON(ctx, http.MethodDelete, path, nil, out)
}

func (c *commandContext) doJSON(ctx context.Context, method, path string, body, out any) error {
	return c.doJSONPath(ctx, method, "/api/v1/"+path, body, out)
}

// postLoopbackJSON posts to a loopback-only daemon control/telemetry endpoint.
// It never uses AO_DAEMON_URL: these routes are localControlRequest-gated and
// only meaningful against a local daemon. In remote mode (no run-file) it fails,
// which callers ignore.
func (c *commandContext) postLoopbackJSON(ctx context.Context, path string, body any) error {
	target, err := c.resolveLoopbackTarget()
	if err != nil {
		return err
	}
	return c.doRequest(ctx, target, http.MethodPost, path, body, nil)
}

func (c *commandContext) doJSONPath(ctx context.Context, method, path string, body, out any) error {
	target, err := c.resolveDaemonTarget()
	if err != nil {
		return err
	}
	return c.doRequest(ctx, target, method, path, body, out)
}

// daemonTarget is the resolved base URL (and optional bearer token) for talking
// to the daemon: a remote daemon from AO_DAEMON_URL, or the local loopback
// daemon discovered via the run-file.
type daemonTarget struct {
	baseURL string
	token   string
}

// wsURL converts the target's HTTP base URL to its ws(s) form for the mux.
func (t daemonTarget) wsURL(path string) string {
	base := t.baseURL
	switch {
	case strings.HasPrefix(base, "https://"):
		base = "wss://" + strings.TrimPrefix(base, "https://")
	case strings.HasPrefix(base, "http://"):
		base = "ws://" + strings.TrimPrefix(base, "http://")
	}
	return base + path
}

// resolveDaemonTarget picks the daemon to talk to. AO_DAEMON_URL (plus the
// AO_AUTH_TOKEN bearer) targets a remote daemon; otherwise it falls back to the
// loopback daemon discovered via the run-file (the historical behavior).
func (c *commandContext) resolveDaemonTarget() (daemonTarget, error) {
	cfg, err := config.Load()
	if err != nil {
		return daemonTarget{}, err
	}
	if raw := strings.TrimSpace(os.Getenv("AO_DAEMON_URL")); raw != "" {
		return daemonTarget{baseURL: strings.TrimRight(raw, "/"), token: cfg.AuthToken}, nil
	}
	return loopbackTarget(c.deps.ProcessAlive, cfg)
}

// resolveLoopbackTarget always resolves the local loopback daemon, ignoring
// AO_DAEMON_URL — for control/telemetry routes that only exist locally.
func (c *commandContext) resolveLoopbackTarget() (daemonTarget, error) {
	cfg, err := config.Load()
	if err != nil {
		return daemonTarget{}, err
	}
	return loopbackTarget(c.deps.ProcessAlive, cfg)
}

func loopbackTarget(processAlive func(int) bool, cfg config.Config) (daemonTarget, error) {
	info, err := runfile.Read(cfg.RunFilePath)
	if err != nil {
		return daemonTarget{}, err
	}
	if info == nil {
		return daemonTarget{}, fmt.Errorf("AO daemon is not running — start it with `ao start`")
	}
	if !processAlive(info.PID) {
		return daemonTarget{}, fmt.Errorf("AO daemon is not running (stale run-file at %s) — start it with `ao start`", cfg.RunFilePath)
	}
	return daemonTarget{baseURL: fmt.Sprintf("http://%s:%d", config.LoopbackHost, info.Port)}, nil
}

func (c *commandContext) doRequest(ctx context.Context, target daemonTarget, method, path string, body, out any) error {
	var reader io.Reader = http.NoBody
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, target.baseURL+path, reader) // #nosec G704 -- target is the discovered loopback daemon or an explicit AO_DAEMON_URL.
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if target.token != "" {
		req.Header.Set("Authorization", "Bearer "+target.token)
	}

	// Reuse the injected client's transport (keeps it stubbable in tests) but
	// give daemon API calls far more headroom than the 2s status-probe timeout.
	client := *c.deps.HTTPClient
	client.Timeout = commandTimeout
	resp, err := client.Do(req) // #nosec G704 -- request target is the resolved daemon URL above.
	if err != nil {
		return fmt.Errorf("call daemon: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e apiError
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Message == "" {
			return fmt.Errorf("daemon returned HTTP %d", resp.StatusCode)
		}
		return fmt.Errorf("%s", e.String())
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}
