package flysprite

import (
	"context"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

var ansiCSI = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]")
var ansiOSC = regexp.MustCompile("\x1b\\][^\x07\x1b]*(\x07|\x1b\\\\)")

// stripANSI removes CSI + OSC escape sequences so a captured TUI frame is
// readable in test logs.
func stripANSI(s string) string {
	s = ansiOSC.ReplaceAllString(s, "")
	s = ansiCSI.ReplaceAllString(s, "")
	return strings.ReplaceAll(s, "\r", "")
}

// TestLiveClaudeRemoteControl provisions a real sprite, injects the user's
// claude.ai credential, launches `claude --remote-control`, and reports what the
// pane shows — distinguishing a working full-scope login (Remote Control engages,
// claude runs) from an inference-only token (Remote Control refused).
//
// Skipped unless AO_SPRITE_LIVE=1 and ~/.ao-dev/claude_creds exists.
//
//	AO_SPRITE_LIVE=1 go test ./internal/adapters/runtime/flysprite/ -run TestLiveClaudeRemoteControl -v -timeout 480s
func TestLiveClaudeRemoteControl(t *testing.T) {
	if os.Getenv("AO_SPRITE_LIVE") != "1" {
		t.Skip("set AO_SPRITE_LIVE=1 to run the live claude Remote Control test")
	}
	tokenBytes, err := os.ReadFile(os.Getenv("HOME") + "/.ao-dev/sprites_token")
	if err != nil {
		t.Skipf("no sprites token: %v", err)
	}
	credBytes, err := os.ReadFile(os.Getenv("HOME") + "/.ao-dev/claude_creds")
	if err != nil {
		t.Skipf("no claude creds at ~/.ao-dev/claude_creds: %v", err)
	}

	const (
		sessionID = domain.SessionID("rc-1")
		projectID = domain.ProjectID("photon")
		branch    = "ao-rc-1"
		repoURL   = "https://github.com/photon-hq/advanced-imessage-go"
	)

	host := NewHost(strings.TrimSpace(string(tokenBytes)))
	ws := NewWorkspace(host, staticRepos{url: repoURL}, Secrets{ClaudeCredentials: string(credBytes)})
	rt := New(Options{Host: host})

	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
	defer cancel()
	t.Cleanup(func() {
		sprite, _ := spriteName(sessionID)
		_ = host.Destroy(context.Background(), sprite)
		t.Logf("cleanup: destroyed %s", sprite)
	})

	t.Log("provisioning workspace (sprite + zellij + clone + creds)…")
	wsInfo, err := ws.Create(ctx, ports.WorkspaceConfig{ProjectID: projectID, SessionID: sessionID, Kind: domain.KindWorker, Branch: branch})
	if err != nil {
		t.Fatalf("Workspace.Create: %v", err)
	}

	// Mirror what the claudecode adapter produces for a sprite session.
	agentArgv := []string{
		"claude", "--remote-control",
		"--permission-mode", "default",
		"--", "Reply with exactly the word PONG and then wait.",
	}
	t.Log("launching claude --remote-control…")
	handle, err := rt.Create(ctx, ports.RuntimeConfig{
		SessionID:     sessionID,
		WorkspacePath: wsInfo.Path,
		Argv:          agentArgv,
		Env:           map[string]string{"AO_SESSION_ID": string(sessionID)},
	})
	if err != nil {
		t.Fatalf("Runtime.Create (claude did not start / exited): %v", err)
	}
	if alive, err := rt.IsAlive(ctx, handle); err != nil || !alive {
		t.Fatalf("IsAlive = (%v, %v) — claude exited", alive, err)
	}

	// Attach and watch the pane for ~30s.
	argv, env, err := rt.AttachCommand(handle)
	if err != nil {
		t.Fatalf("AttachCommand: %v", err)
	}
	pty, err := rt.Spawn(ctx, argv, env, 50, 160)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	var mu sync.Mutex
	var buf []byte
	go func() {
		b := make([]byte, 8192)
		for {
			n, e := pty.Read(b)
			if n > 0 {
				mu.Lock()
				buf = append(buf, b[:n]...)
				mu.Unlock()
			}
			if e != nil {
				return
			}
		}
	}()
	time.Sleep(30 * time.Second)
	_ = pty.Close()
	mu.Lock()
	got := string(buf)
	mu.Unlock()

	low := strings.ToLower(got)
	t.Logf("captured %d bytes\n--- HEAD ---\n%s\n--- TAIL ---\n%s", len(got), stripANSI(got)[:min(1500, len(stripANSI(got)))], tail(got, 1000))

	switch {
	case strings.Contains(low, "requires a claude.ai subscription") ||
		strings.Contains(low, "remote control requires"):
		t.Fatalf("Remote Control was REFUSED — the injected credential is inference-only, not a full-scope `claude auth login` token")
	case strings.Contains(low, "pong"):
		t.Logf("SUCCESS: claude authenticated with the injected creds and responded (PONG) — Remote Control session live in the sprite")
	default:
		t.Logf("claude is running but no PONG confirmed in the window — inspect the frame above")
	}
}
