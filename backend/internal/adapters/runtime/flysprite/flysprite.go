// Package flysprite runs agent sessions as claude-in-zellij inside Fly Sprites.
// It mirrors the local zellij runtime adapter but issues every zellij command
// through the Sprites exec API instead of a local process, so the daemon can
// host sessions remotely. The sprite itself (and the cloned workspace) is
// created by the Workspace adapter in this package; the Runtime resolves the
// sprite by the deterministic name derived from the session id.
package flysprite

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/terminal"
)

const (
	defaultTimeout = 90 * time.Second
	// layoutDir is where per-session zellij layout files are written inside the
	// sprite (the sprite user's home is /home/sprite).
	layoutDir = "/home/sprite/.ao"
	// spriteEnvKey carries the sprite name from AttachCommand to Spawn through the
	// terminal attach env, since SpawnFunc receives env but not the handle.
	spriteEnvKey = "__AO_SPRITE"
	// messageChunkBytes bounds a single write-chars paste.
	messageChunkBytes = 16 * 1024
	// pathPrelude ensures ~/.local/bin (zellij, claude, ao) is on PATH for the
	// control commands without pulling in login-shell profile noise that would
	// corrupt list-sessions parsing.
	pathPrelude = `export PATH="$HOME/.local/bin:$PATH"; `
)

// Runtime is the Fly Sprite runtime adapter. It satisfies the runtime contract
// the daemon needs: Create, Destroy, IsAlive, AttachCommand (terminal.PTYSource)
// and SendMessage.
type Runtime struct {
	host      Host
	timeout   time.Duration
	daemonURL string
	authToken string
}

// Options configures a Runtime.
type Options struct {
	// Host drives sprite lifecycle + exec. Required.
	Host Host
	// Timeout bounds a single zellij control command (and the create readiness
	// poll). Defaults to defaultTimeout.
	Timeout time.Duration
	// DaemonURL is the daemon's public base URL, injected into the agent env as
	// AO_DAEMON_URL so in-sprite `ao hooks` callbacks reach the daemon. Empty
	// disables hook-env injection.
	DaemonURL string
	// AuthToken is injected alongside DaemonURL as AO_AUTH_TOKEN so the in-sprite
	// hook callbacks authenticate to the public daemon.
	AuthToken string
}

// New builds a Runtime. It panics if Host is nil, matching the other adapters'
// fail-fast wiring expectations.
func New(opts Options) *Runtime {
	if opts.Host == nil {
		panic("flysprite: Host is required")
	}
	t := opts.Timeout
	if t <= 0 {
		t = defaultTimeout
	}
	return &Runtime{host: opts.Host, timeout: t, daemonURL: opts.DaemonURL, authToken: opts.AuthToken}
}

// Create writes the agent layout into the sprite and starts a detached zellij
// session running it. zellij `attach --create-background` returns immediately;
// the in-sprite zellij server keeps the agent alive independent of any client.
func (r *Runtime) Create(ctx context.Context, cfg ports.RuntimeConfig) (ports.RuntimeHandle, error) {
	session, err := sessionName(cfg.SessionID)
	if err != nil {
		return ports.RuntimeHandle{}, err
	}
	sprite, err := spriteName(cfg.SessionID)
	if err != nil {
		return ports.RuntimeHandle{}, err
	}
	if cfg.WorkspacePath == "" {
		return ports.RuntimeHandle{}, errors.New("flysprite runtime: workspace path is required")
	}
	if len(cfg.Argv) == 0 {
		return ports.RuntimeHandle{}, errors.New("flysprite runtime: launch command is required")
	}
	if err := validateEnvKeys(cfg.Env); err != nil {
		return ports.RuntimeHandle{}, err
	}

	box, err := r.host.Open(ctx, sprite)
	if err != nil {
		return ports.RuntimeHandle{}, err
	}

	// Clear any prior same-name zellij session left by a partial earlier spawn,
	// so create-background cannot fail with "session already exists".
	_, _ = r.runZellij(ctx, box, deleteSessionArgs(session)...)

	layout := buildLayout(cfg.WorkspacePath, normalizeAgentArgv0(cfg.Argv), r.agentEnv(cfg.Env))
	layoutPath := layoutDir + "/layout-" + session + ".kdl"
	if err := box.WriteFile(ctx, layoutPath, []byte(layout), 0o600); err != nil {
		return ports.RuntimeHandle{}, err
	}

	if out, err := r.runZellij(ctx, box, createSessionArgs(session, layoutPath)...); err != nil {
		return ports.RuntimeHandle{}, fmt.Errorf("flysprite runtime: create session %s: %w (%s)", session, err, strings.TrimSpace(string(out)))
	}

	handle := ports.RuntimeHandle{ID: flyHandle{Sprite: sprite, Session: session, Pane: agentPaneName}.encode()}
	if err := r.waitAlive(ctx, handle); err != nil {
		_ = r.Destroy(context.Background(), handle)
		return ports.RuntimeHandle{}, err
	}
	return handle, nil
}

// agentEnv augments the session env with the public daemon URL + auth token so
// in-sprite `ao hooks` callbacks reach the daemon (sprites are off the private
// network). It copies rather than mutating the caller's map.
func (r *Runtime) agentEnv(base map[string]string) map[string]string {
	if r.daemonURL == "" {
		return base
	}
	merged := make(map[string]string, len(base)+2)
	for k, v := range base {
		merged[k] = v
	}
	merged["AO_DAEMON_URL"] = r.daemonURL
	if r.authToken != "" {
		merged["AO_AUTH_TOKEN"] = r.authToken
	}
	return merged
}

// waitAlive polls IsAlive until the session is listed or the timeout elapses;
// create-background returns before zellij has registered the session.
func (r *Runtime) waitAlive(ctx context.Context, handle ports.RuntimeHandle) error {
	deadline := time.Now().Add(r.timeout)
	var lastErr error
	for {
		alive, err := r.IsAlive(ctx, handle)
		if err == nil && alive {
			return nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = errors.New("session not yet listed")
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("flysprite runtime: session did not become ready: %w", lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// Destroy kills the handle's zellij session and removes its serialized state so
// a later attach cannot resurrect it. The sprite itself is torn down by the
// workspace adapter. A missing sprite or session is treated as success.
func (r *Runtime) Destroy(ctx context.Context, handle ports.RuntimeHandle) error {
	h, err := parseFlyHandle(handle.ID)
	if err != nil {
		return err
	}
	box, err := r.host.Open(ctx, h.Sprite)
	if err != nil {
		if strings.Contains(err.Error(), "open sprite") && isMissingSprite(ctx, r.host, h.Sprite) {
			return nil
		}
		// The sprite may simply be gone; do not block teardown on a probe error.
		return nil
	}
	out, derr := r.runZellij(ctx, box, deleteSessionArgs(h.Session)...)
	if derr != nil && !deleteSessionMissingOutput(string(out)) {
		return fmt.Errorf("flysprite runtime: destroy session %s: %w", h.Session, derr)
	}
	return nil
}

// IsAlive reports whether the sprite exists AND its zellij session is listed. A
// 404 on the sprite, or "no active sessions", is a definitive "not alive"; any
// other failure is a probe error (never treated as proof of death).
func (r *Runtime) IsAlive(ctx context.Context, handle ports.RuntimeHandle) (bool, error) {
	h, err := parseFlyHandle(handle.ID)
	if err != nil {
		return false, err
	}
	_, found, err := r.host.Status(ctx, h.Sprite)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	box, err := r.host.Open(ctx, h.Sprite)
	if err != nil {
		return false, err
	}
	out, err := r.runZellij(ctx, box, listSessionsArgs()...)
	if err != nil {
		if noActiveSessionsOutput(string(out)) {
			return false, nil
		}
		return false, fmt.Errorf("flysprite runtime: probe session %s: %w", h.Session, err)
	}
	return sessionListedAlive(string(out), h.Session), nil
}

// AttachCommand returns the argv that attaches a TTY to the session's zellij
// pane, plus an env entry carrying the sprite name so Spawn knows which sprite
// to exec into.
func (r *Runtime) AttachCommand(handle ports.RuntimeHandle) ([]string, []string, error) {
	h, err := parseFlyHandle(handle.ID)
	if err != nil {
		return nil, nil, err
	}
	script := pathPrelude + "exec zellij " + quoteArgvUnix(attachArgs(h.Session))
	return []string{"bash", "-c", script}, []string{spriteEnvKey + "=" + h.Sprite}, nil
}

// Spawn is the terminal.SpawnFunc the mux uses for sprite-backed sessions. It
// reads the sprite name from the attach env (set by AttachCommand), opens the
// sprite, and starts the attach argv on a TTY exec.
func (r *Runtime) Spawn(ctx context.Context, argv []string, env []string, rows, cols uint16) (terminal.PTYProcess, error) {
	sprite := envValue(env, spriteEnvKey)
	if sprite == "" {
		return nil, errors.New("flysprite spawn: attach env missing sprite name")
	}
	box, err := r.host.Open(ctx, sprite)
	if err != nil {
		return nil, err
	}
	return box.OpenTTY(ctx, rows, cols, argv...)
}

// SendMessage types a message into the agent pane and submits it with Enter.
func (r *Runtime) SendMessage(ctx context.Context, handle ports.RuntimeHandle, message string) error {
	h, err := parseFlyHandle(handle.ID)
	if err != nil {
		return err
	}
	box, err := r.host.Open(ctx, h.Sprite)
	if err != nil {
		return err
	}
	for _, chunk := range chunks(message, messageChunkBytes) {
		if out, err := r.runZellij(ctx, box, writeCharsArgs(h.Session, chunk)...); err != nil {
			return fmt.Errorf("flysprite runtime: write message %s: %w (%s)", h.Session, err, strings.TrimSpace(string(out)))
		}
	}
	if out, err := r.runZellij(ctx, box, writeEnterArgs(h.Session)...); err != nil {
		return fmt.Errorf("flysprite runtime: submit message %s: %w (%s)", h.Session, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// runZellij runs a zellij subcommand inside the sprite via `bash -c`, prefixing
// PATH so ~/.local/bin/zellij resolves. It returns combined output for the
// caller to inspect (e.g. "no active sessions").
func (r *Runtime) runZellij(ctx context.Context, box Box, args ...string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	script := pathPrelude + "zellij " + quoteArgvUnix(args)
	return box.Run(cctx, nil, "", "bash", "-c", script)
}

// ---- list-sessions / delete-session output parsing (mirrors the local adapter) ----

func noActiveSessionsOutput(out string) bool {
	s := strings.ToLower(out)
	return strings.Contains(s, "no active") && strings.Contains(s, "session")
}

func deleteSessionMissingOutput(out string) bool {
	s := strings.ToLower(out)
	if noActiveSessionsOutput(s) {
		return true
	}
	return strings.Contains(s, "session") &&
		(strings.Contains(s, "not found") ||
			strings.Contains(s, "does not exist") ||
			strings.Contains(s, "not exist") ||
			strings.Contains(s, "not a session"))
}

// sessionListedAlive reports whether session appears (and is not EXITED) in
// `zellij list-sessions --no-formatting` output, whose lines begin with the
// session name.
func sessionListedAlive(out, session string) bool {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if fields[0] == session {
			return !strings.Contains(strings.ToUpper(line), "EXITED")
		}
	}
	return false
}

// envValue extracts KEY's value from a KEY=VALUE env slice.
func envValue(env []string, key string) string {
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return e[len(prefix):]
		}
	}
	return ""
}

// isMissingSprite reports whether the sprite is definitively gone (404).
func isMissingSprite(ctx context.Context, host Host, sprite string) bool {
	_, found, err := host.Status(ctx, sprite)
	return err == nil && !found
}
