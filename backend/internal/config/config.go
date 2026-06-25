// Package config loads the daemon's runtime configuration. By default the HTTP
// daemon is a loopback-only sidecar: it binds 127.0.0.1, takes no public
// traffic, and reads everything it needs from the environment with sane
// defaults so it can boot with zero configuration in development.
//
// A daemon can opt into an authenticated remote mode (e.g. hosted on Fly.io)
// by setting AO_AUTH_TOKEN and pointing AO_BIND_HOST beyond loopback. Binding a
// non-loopback host without an auth token is refused: the daemon must never
// become a public no-auth service. See Load.
package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// LoopbackHost is the host the daemon binds by default. Historically it was
	// the ONLY host the daemon ever bound — it has no TLS and, without an auth
	// token, no access control, so a stray bind to 0.0.0.0 would turn it into a
	// public no-auth service. The bind host is now configurable via AO_BIND_HOST
	// (validated by validateBindHost), but Load refuses any non-loopback host
	// unless AO_AUTH_TOKEN is also set, preserving that invariant.
	LoopbackHost = "127.0.0.1"
	// DefaultRuntime is the session runtime adapter used when AO_RUNTIME is unset.
	// "zellij" runs sessions as local panes; "flysprite" runs them in Fly Sprites.
	DefaultRuntime = "zellij"
	// DefaultPort is the single port for REST, terminal mux, health, and control.
	DefaultPort = 3001
	// DefaultRequestTimeout bounds a single REST request. Long-lived terminal mux
	// connections are mounted outside this timeout.
	DefaultRequestTimeout = 60 * time.Second
	// DefaultShutdownTimeout is the hard cap on graceful shutdown. After this
	// the process exits even if connections are still draining.
	DefaultShutdownTimeout = 10 * time.Second
	// DefaultAgent is the compatibility value used when AO_AGENT is unset. The
	// daemon validates it at startup, but worker/orchestrator spawns resolve from
	// explicit requests or project role config instead of falling back to it.
	DefaultAgent = "claude-code"
	// DefaultTelemetryPostHogHost is the default PostHog ingestion host when
	// remote telemetry is enabled and AO_TELEMETRY_POSTHOG_HOST is unset.
	DefaultTelemetryPostHogHost = "https://us.i.posthog.com"
)

// TelemetryRemote selects the remote telemetry exporter.
type TelemetryRemote string

const (
	// TelemetryRemoteOff disables remote telemetry export.
	TelemetryRemoteOff TelemetryRemote = "off"
	// TelemetryRemotePostHog exports allowlisted events to PostHog.
	TelemetryRemotePostHog TelemetryRemote = "posthog"
)

// TelemetryConfig controls local and remote telemetry behavior.
type TelemetryConfig struct {
	Events      bool
	Metrics     bool
	Remote      TelemetryRemote
	PostHogKey  string
	PostHogHost string
}

// DefaultAllowedOrigins are the browser origins the daemon's CORS boundary
// trusts, beyond loopback-served content (which the middleware always trusts —
// local pages can reach the no-auth daemon directly anyway). The daemon has no
// auth, so every entry must be an origin web content cannot present:
// app://renderer is the packaged Electron renderer, served from a custom
// scheme only the desktop app registers — no website can bear it. The opaque
// "null" origin (file:// pages, sandboxed iframes on any website) must never
// be added.
var DefaultAllowedOrigins = []string{
	"app://renderer",
}

// Config is the fully-resolved daemon configuration. It is immutable once
// built by Load.
type Config struct {
	// Host is the bind address. Defaults to loopback (see LoopbackHost);
	// AO_BIND_HOST may widen it, but only when AuthToken is set.
	Host string
	// Port is the TCP port to bind. The daemon fails fast if it is taken.
	Port int
	// RequestTimeout bounds REST request handling.
	RequestTimeout time.Duration
	// ShutdownTimeout is the hard graceful-shutdown deadline.
	ShutdownTimeout time.Duration
	// RunFilePath is where the PID + port handshake file (running.json) is
	// written so the Electron supervisor can discover and reap the daemon.
	RunFilePath string
	// DataDir is the directory holding durable SQLite state: DB and WAL files.
	// It is created on first use by the storage layer.
	DataDir string
	// Agent is the compatibility agent adapter id selected by AO_AGENT;
	// startSession fails fast if no adapter with this id is registered.
	Agent string
	// AllowedOrigins are the browser origins granted CORS read access (see
	// DefaultAllowedOrigins). Overridden by AO_ALLOWED_ORIGINS.
	AllowedOrigins []string
	// AuthToken, when non-empty, is the shared bearer token the daemon requires
	// on every /api/v1/* request and the /mux WebSocket. Empty selects the
	// historical loopback no-auth mode. Set via AO_AUTH_TOKEN.
	AuthToken string
	// Runtime selects the session runtime adapter: "zellij" (local panes) or
	// "flysprite" (Fly Sprites). Set via AO_RUNTIME; defaults to DefaultRuntime.
	Runtime string
	// SpritesToken is the Fly Sprites API bearer token used by the flysprite
	// runtime to create/exec/destroy sprites. Required when Runtime=="flysprite".
	// Set via AO_SPRITES_TOKEN.
	SpritesToken string
	// ClaudeCredentials is the raw contents of the user's claude.ai OAuth
	// credential (~/.claude/.credentials.json), injected into each sprite so
	// Remote Control works. Set via AO_CLAUDE_CREDENTIALS, or read from
	// ClaudeCredentialsFile when that is empty.
	ClaudeCredentials string
	// ClaudeCredentialsFile is a path the daemon reads the claude.ai credential
	// from when ClaudeCredentials is empty. Set via AO_CLAUDE_CREDENTIALS_FILE.
	ClaudeCredentialsFile string
	// GitHubToken authenticates git clone/push and gh inside sprites. Set via
	// AO_GITHUB_TOKEN, falling back to GITHUB_TOKEN then GH_TOKEN.
	GitHubToken string
	// Telemetry controls local/remote telemetry sinks.
	Telemetry TelemetryConfig
}

// Addr returns the host:port the HTTP server binds. It uses net.JoinHostPort so
// the result is correct for IPv6 literals as well as IPv4 / hostnames.
func (c Config) Addr() string {
	return net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}

// Load resolves configuration from the environment, applying defaults. It
// returns an error only for values that are present but malformed (e.g. a
// non-numeric AO_PORT); missing values fall back to defaults.
//
// Recognised variables:
//
//	AO_PORT              bind port           (default 3001)
//	AO_REQUEST_TIMEOUT   per-request timeout (Go duration > 0, default 60s)
//	AO_SHUTDOWN_TIMEOUT  shutdown deadline   (Go duration > 0, default 10s)
//	AO_RUN_FILE          running.json path   (default ~/.ao/running.json)
//	AO_DATA_DIR          durable state dir   (default ~/.ao/data)
//	AO_AGENT             compatibility agent id (default claude-code)
//	AO_ALLOWED_ORIGINS   CORS origins, comma-separated (default DefaultAllowedOrigins)
//	AO_AUTH_TOKEN        shared bearer token; empty = loopback no-auth mode
//	AO_RUNTIME           session runtime zellij|flysprite (default zellij)
//	AO_SPRITES_TOKEN     Fly Sprites API token (required when AO_RUNTIME=flysprite)
//	AO_CLAUDE_CREDENTIALS       claude.ai OAuth credential content, injected into sprites
//	AO_CLAUDE_CREDENTIALS_FILE  path read for the above when AO_CLAUDE_CREDENTIALS is unset
//	AO_GITHUB_TOKEN      GitHub token for sprites (falls back to GITHUB_TOKEN, GH_TOKEN)
//	AO_BIND_HOST         bind host; non-loopback requires AO_AUTH_TOKEN (default 127.0.0.1)
//	AO_TELEMETRY_EVENTS  local event capture off|on (default off)
//	AO_TELEMETRY_METRICS local metric capture off|on (default off)
//	AO_TELEMETRY_REMOTE  remote exporter off|posthog (default off)
//	AO_TELEMETRY_POSTHOG_KEY   PostHog project key
//	AO_TELEMETRY_POSTHOG_HOST  PostHog host (default DefaultTelemetryPostHogHost)
//
// The bind host defaults to loopback. AO_BIND_HOST may widen it for an
// authenticated remote deployment, but Load refuses a non-loopback host unless
// AO_AUTH_TOKEN is set.
func Load() (Config, error) {
	cfg := Config{
		Host:            LoopbackHost,
		Port:            DefaultPort,
		RequestTimeout:  DefaultRequestTimeout,
		ShutdownTimeout: DefaultShutdownTimeout,
		Agent:           DefaultAgent,
		Runtime:         DefaultRuntime,
		AllowedOrigins:  DefaultAllowedOrigins,
		Telemetry: TelemetryConfig{
			Remote:      TelemetryRemoteOff,
			PostHogHost: DefaultTelemetryPostHogHost,
		},
	}

	if raw := os.Getenv("AO_PORT"); raw != "" {
		port, err := strconv.Atoi(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid AO_PORT %q: %w", raw, err)
		}
		if port < 1 || port > 65535 {
			return Config{}, fmt.Errorf("invalid AO_PORT %d: out of range 1-65535", port)
		}
		cfg.Port = port
	}

	if raw := os.Getenv("AO_REQUEST_TIMEOUT"); raw != "" {
		d, err := parsePositiveDuration("AO_REQUEST_TIMEOUT", raw)
		if err != nil {
			return Config{}, err
		}
		cfg.RequestTimeout = d
	}

	if raw := os.Getenv("AO_SHUTDOWN_TIMEOUT"); raw != "" {
		d, err := parsePositiveDuration("AO_SHUTDOWN_TIMEOUT", raw)
		if err != nil {
			return Config{}, err
		}
		cfg.ShutdownTimeout = d
	}

	if raw := os.Getenv("AO_AGENT"); raw != "" {
		cfg.Agent = raw
	}

	if raw := os.Getenv("AO_AUTH_TOKEN"); raw != "" {
		cfg.AuthToken = raw
	}

	if raw := os.Getenv("AO_RUNTIME"); raw != "" {
		switch raw {
		case "zellij", "flysprite":
			cfg.Runtime = raw
		default:
			return Config{}, fmt.Errorf("invalid AO_RUNTIME %q: must be zellij|flysprite", raw)
		}
	}

	if raw := os.Getenv("AO_SPRITES_TOKEN"); raw != "" {
		cfg.SpritesToken = raw
	}
	if raw := os.Getenv("AO_CLAUDE_CREDENTIALS"); raw != "" {
		cfg.ClaudeCredentials = raw
	}
	if raw := os.Getenv("AO_CLAUDE_CREDENTIALS_FILE"); raw != "" {
		cfg.ClaudeCredentialsFile = raw
	}
	cfg.GitHubToken = firstNonEmptyEnv("AO_GITHUB_TOKEN", "GITHUB_TOKEN", "GH_TOKEN")

	// AO_BIND_HOST widens the bind beyond loopback. It is honoured only when an
	// auth token is set: the daemon has no TLS or access control otherwise, and
	// exposing it without a token would create a public no-auth service. TLS is
	// expected to be terminated by an upstream proxy (e.g. the Fly edge).
	if raw := os.Getenv("AO_BIND_HOST"); raw != "" {
		host, err := validateBindHost(raw)
		if err != nil {
			return Config{}, err
		}
		if !isLoopbackHost(host) && cfg.AuthToken == "" {
			return Config{}, fmt.Errorf("AO_BIND_HOST=%q binds beyond loopback but AO_AUTH_TOKEN is not set: refusing to expose a no-auth daemon", raw)
		}
		cfg.Host = host
	}

	if raw, ok := os.LookupEnv("AO_ALLOWED_ORIGINS"); ok && raw != "" {
		// Explicit override replaces the defaults entirely so a deployment can
		// also narrow the list. The "null" origin is rejected, never silently
		// dropped: an operator allowing it would open the no-auth daemon to
		// every sandboxed iframe on the web.
		origins := make([]string, 0, 4)
		for _, origin := range strings.Split(raw, ",") {
			origin = strings.TrimSpace(origin)
			if origin == "" {
				continue
			}
			if origin == "null" || origin == "*" {
				return Config{}, fmt.Errorf("invalid AO_ALLOWED_ORIGINS entry %q: wildcard and null origins are not allowed", origin)
			}
			origins = append(origins, origin)
		}
		cfg.AllowedOrigins = origins
	}

	if raw := os.Getenv("AO_TELEMETRY_EVENTS"); raw != "" {
		v, err := parseToggleEnv("AO_TELEMETRY_EVENTS", raw)
		if err != nil {
			return Config{}, err
		}
		cfg.Telemetry.Events = v
	}
	if raw := os.Getenv("AO_TELEMETRY_METRICS"); raw != "" {
		v, err := parseToggleEnv("AO_TELEMETRY_METRICS", raw)
		if err != nil {
			return Config{}, err
		}
		cfg.Telemetry.Metrics = v
	}
	if raw := os.Getenv("AO_TELEMETRY_REMOTE"); raw != "" {
		remote, err := parseTelemetryRemote(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid AO_TELEMETRY_REMOTE %q: %w", raw, err)
		}
		cfg.Telemetry.Remote = remote
	}
	if raw := os.Getenv("AO_TELEMETRY_POSTHOG_KEY"); raw != "" {
		cfg.Telemetry.PostHogKey = raw
	}
	if raw := os.Getenv("AO_TELEMETRY_POSTHOG_HOST"); raw != "" {
		cfg.Telemetry.PostHogHost = raw
	}

	runFile, err := resolveRunFilePath()
	if err != nil {
		return Config{}, err
	}
	cfg.RunFilePath = runFile

	dataDir, err := resolveDataDir()
	if err != nil {
		return Config{}, err
	}
	cfg.DataDir = dataDir

	return cfg, nil
}

// firstNonEmptyEnv returns the value of the first set, non-empty environment
// variable among keys, or "" if none are set.
func firstNonEmptyEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

func parseToggleEnv(name, raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "on", "true", "1", "yes":
		return true, nil
	case "off", "false", "0", "no":
		return false, nil
	default:
		return false, fmt.Errorf("%s must be off|on", name)
	}
}

func parseTelemetryRemote(raw string) (TelemetryRemote, error) {
	switch TelemetryRemote(strings.ToLower(strings.TrimSpace(raw))) {
	case TelemetryRemoteOff:
		return TelemetryRemoteOff, nil
	case TelemetryRemotePostHog:
		return TelemetryRemotePostHog, nil
	default:
		return "", fmt.Errorf("must be off|posthog")
	}
}

// validateBindHost accepts an IP literal (e.g. 0.0.0.0, ::, 127.0.0.1) or the
// hostname "localhost" — the values that make sense as a TCP bind target. It
// rejects arbitrary hostnames so a typo cannot silently bind an unexpected
// interface.
func validateBindHost(raw string) (string, error) {
	host := strings.TrimSpace(raw)
	if host == "" {
		return "", fmt.Errorf("AO_BIND_HOST must not be empty")
	}
	if host == "localhost" {
		return host, nil
	}
	if ip := net.ParseIP(host); ip != nil {
		return host, nil
	}
	return "", fmt.Errorf("invalid AO_BIND_HOST %q: must be an IP literal or \"localhost\"", raw)
}

// isLoopbackHost reports whether a validated bind host stays on loopback.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// parsePositiveDuration rejects zero and negative durations: a zero
// RequestTimeout would expire every request instantly, and a non-positive
// ShutdownTimeout would defeat graceful shutdown.
func parsePositiveDuration(name, raw string) (time.Duration, error) {
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", name, raw, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("invalid %s %q: must be > 0", name, raw)
	}
	return d, nil
}

// resolveRunFilePath picks where running.json lives. An explicit AO_RUN_FILE
// wins; otherwise it sits under the canonical AO home directory so the CLI and
// Electron supervisor share one handshake location.
func resolveRunFilePath() (string, error) {
	if p, ok := os.LookupEnv("AO_RUN_FILE"); ok && p != "" {
		return p, nil
	}
	stateDir, err := defaultStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(stateDir, "running.json"), nil
}

// resolveDataDir picks where durable state (the SQLite DB) lives. An explicit
// AO_DATA_DIR wins; otherwise it defaults under the same canonical AO home
// directory as the run-file.
func resolveDataDir() (string, error) {
	if p, ok := os.LookupEnv("AO_DATA_DIR"); ok && p != "" {
		return p, nil
	}
	stateDir, err := defaultStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(stateDir, "data"), nil
}

func defaultStateDir() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve state dir: %w", err)
	}
	return filepath.Join(homeDir, ".ao"), nil
}
