package flysprite

import (
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// The flysprite runtime runs zellij INSIDE a Fly Sprite, with the agent in a
// pane, and issues every zellij command through the sprite exec API. Sprites
// are always Linux, so this mirrors only the Unix subset of the local zellij
// adapter (no PowerShell/cmd branches). The command/layout shapes intentionally
// match backend/internal/adapters/runtime/zellij/commands.go so behaviour is
// identical to a local zellij session.

const (
	agentPaneName = "agent"
	// handleSep separates the sprite name, zellij session, and pane id in a
	// RuntimeHandle.ID. It is a unit-separator control byte so it can never
	// collide with a sprite/session/pane token.
	handleSep = "\x1f"
)

// sessionName derives a zellij session name from an AO session id. AO ids are
// already restricted to a safe charset, but validate defensively so a malformed
// id never reaches the sprite shell.
func sessionName(id domain.SessionID) (string, error) {
	s := strings.TrimSpace(string(id))
	if s == "" {
		return "", fmt.Errorf("flysprite: empty session id")
	}
	if !validToken(s) {
		return "", fmt.Errorf("flysprite: session id %q has characters outside [A-Za-z0-9_-]", s)
	}
	return s, nil
}

// spriteName derives the Fly Sprite name from an AO session id. Sprite names are
// DNS-ish: lowercase alphanumerics and dashes. The "ao-" prefix namespaces our
// sprites within the org.
func spriteName(id domain.SessionID) (string, error) {
	s := strings.ToLower(strings.TrimSpace(string(id)))
	if s == "" {
		return "", fmt.Errorf("flysprite: empty session id")
	}
	if !validToken(s) {
		return "", fmt.Errorf("flysprite: session id %q has characters outside [A-Za-z0-9_-]", s)
	}
	return "ao-" + s, nil
}

// validToken reports whether s is non-empty and contains only characters safe
// for a zellij session name / sprite name token.
func validToken(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}

// ---- RuntimeHandle encoding ----

type flyHandle struct {
	Sprite  string
	Session string
	Pane    string
}

func (h flyHandle) encode() string {
	return strings.Join([]string{h.Sprite, h.Session, h.Pane}, handleSep)
}

func parseFlyHandle(id string) (flyHandle, error) {
	parts := strings.Split(id, handleSep)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
		return flyHandle{}, fmt.Errorf("flysprite: malformed runtime handle %q", id)
	}
	return flyHandle{Sprite: parts[0], Session: parts[1], Pane: parts[2]}, nil
}

// ---- agent argv normalization ----

// normalizeAgentArgv0 rewrites an absolute argv[0] (resolved on the daemon host,
// e.g. /opt/homebrew/bin/claude) to its bare basename, since the binary lives at
// a different path inside the sprite and is resolved via the login-shell PATH.
func normalizeAgentArgv0(argv []string) []string {
	if len(argv) == 0 {
		return argv
	}
	out := append([]string(nil), argv...)
	if strings.HasPrefix(out[0], "/") || strings.Contains(out[0], "\\") {
		out[0] = path.Base(out[0])
	}
	return out
}

// ---- zellij command argv builders (mirror the local adapter) ----

func createSessionArgs(session, layoutPath string) []string {
	opts := embeddedClientOptions()
	args := make([]string, 0, 6+len(opts)+6)
	args = append(args, "attach", "--create-background", session, "options", "--default-layout", layoutPath)
	args = append(args, opts...)
	args = append(args, "--session-serialization", "false", "--show-startup-tips", "false", "--show-release-notes", "false")
	return args
}

func attachArgs(session string) []string {
	opts := embeddedClientOptions()
	args := make([]string, 0, 3+len(opts))
	args = append(args, "attach", session, "options")
	args = append(args, opts...)
	return args
}

func listSessionsArgs() []string { return []string{"list-sessions", "--no-formatting"} }
func deleteSessionArgs(s string) []string {
	return []string{"delete-session", "--force", s}
}

// writeCharsArgs types text into the session's focused pane (the single agent
// pane). A single-pane layout means no --pane-id is needed.
func writeCharsArgs(session, chunk string) []string {
	return []string{"--session", session, "action", "write-chars", chunk}
}

// writeEnterArgs sends a carriage return (byte 13) to submit the typed message.
func writeEnterArgs(session string) []string {
	return []string{"--session", session, "action", "write", "13"}
}

// chunks splits s into pieces of at most size bytes without splitting a UTF-8
// rune, so a large message can be pasted in bounded zellij writes.
func chunks(s string, size int) []string {
	if size <= 0 || len(s) <= size {
		return []string{s}
	}
	var out []string
	for len(s) > size {
		cut := size
		for cut > 0 && !utf8RuneStart(s[cut]) {
			cut--
		}
		if cut == 0 {
			cut = size
		}
		out = append(out, s[:cut])
		s = s[cut:]
	}
	if len(s) > 0 {
		out = append(out, s)
	}
	return out
}

// utf8RuneStart reports whether b is the first byte of a UTF-8 rune (i.e. not a
// 10xxxxxx continuation byte).
func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

func embeddedClientOptions() []string {
	return []string{
		"--pane-frames", "false",
		"--mouse-mode", "true",
		"--advanced-mouse-actions", "false",
		"--mouse-hover-effects", "false",
		"--focus-follows-mouse", "false",
		"--mouse-click-through", "false",
		"--support-kitty-keyboard-protocol", "false",
	}
}

// ---- layout (Linux) ----

// buildLayout renders the KDL layout that launches the agent in a pane. The
// agent runs under `bash -lc` so the sprite's login-shell PATH (which includes
// ~/.local/bin where claude and ao live) resolves bare command names. env is
// exported before the agent command; PATH is deliberately NOT overridden so the
// login shell owns it.
func buildLayout(workspacePath string, argv []string, env map[string]string) string {
	cmd := wrapLaunchCommandUnix(argv, env)
	var b strings.Builder
	b.WriteString("layout {\n")
	b.WriteString("  cwd ")
	b.WriteString(kdlQuote(workspacePath))
	b.WriteString("\n")
	b.WriteString("  pane command=")
	b.WriteString(kdlQuote("bash"))
	b.WriteString(" name=")
	b.WriteString(kdlQuote(agentPaneName))
	b.WriteString(" borderless=true {\n")
	b.WriteString("    args ")
	b.WriteString(kdlQuote("-lc"))
	b.WriteString(" ")
	b.WriteString(kdlQuote(cmd))
	b.WriteString("\n")
	b.WriteString("  }\n")
	b.WriteString("}\n")
	return b.String()
}

// wrapLaunchCommandUnix exports env (except PATH) then runs argv as a quoted
// POSIX command, so values with spaces (e.g. a prompt) survive `bash -lc`.
func wrapLaunchCommandUnix(argv []string, env map[string]string) string {
	var b strings.Builder
	for _, k := range sortedKeys(env) {
		if k == "PATH" {
			continue
		}
		b.WriteString("export ")
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(shellQuote(env[k]))
		b.WriteString("; ")
	}
	b.WriteString(quoteArgvUnix(argv))
	return b.String()
}

// ---- env validation + quoting helpers ----

func validateEnvKeys(env map[string]string) error {
	for k := range env {
		if !validEnvKey(k) {
			return fmt.Errorf("flysprite: invalid env key %q", k)
		}
	}
	return nil
}

func validEnvKey(key string) bool {
	if key == "" {
		return false
	}
	for i, r := range key {
		if r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
			continue
		}
		if i > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return true
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func quoteArgvUnix(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = shellQuote(a)
	}
	return strings.Join(parts, " ")
}

func kdlQuote(s string) string { return strconv.Quote(s) }
