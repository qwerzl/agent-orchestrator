package flysprite

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// staticRepos is a RepoResolver that returns one fixed URL for the live test.
type staticRepos struct{ url string }

func (s staticRepos) RepoOriginURL(context.Context, domain.ProjectID) (string, error) {
	return s.url, nil
}

// TestLiveSpriteRoundTrip provisions a real Fly Sprite, clones the test repo,
// launches an agent in zellij, attaches a TTY, and tears everything down. It is
// skipped unless AO_SPRITE_LIVE=1 and ~/.ao-dev/sprites_token exists, so the
// default `go test` run never touches the network.
//
//	AO_SPRITE_LIVE=1 go test ./internal/adapters/runtime/flysprite/ -run TestLiveSpriteRoundTrip -v
func TestLiveSpriteRoundTrip(t *testing.T) {
	if os.Getenv("AO_SPRITE_LIVE") != "1" {
		t.Skip("set AO_SPRITE_LIVE=1 to run the live Fly Sprite integration test")
	}
	tokenBytes, err := os.ReadFile(os.Getenv("HOME") + "/.ao-dev/sprites_token")
	if err != nil {
		t.Skipf("no sprites token at ~/.ao-dev/sprites_token: %v", err)
	}
	token := strings.TrimSpace(string(tokenBytes))

	const (
		sessionID = domain.SessionID("smoke-1")
		projectID = domain.ProjectID("photon")
		branch    = "ao-smoke-1"
		repoURL   = "https://github.com/photon-hq/advanced-imessage-go"
	)

	host := NewHost(token)
	ws := NewWorkspace(host, staticRepos{url: repoURL})
	rt := New(Options{Host: host})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Always tear the sprite down, even on failure.
	t.Cleanup(func() {
		sprite, _ := spriteName(sessionID)
		if err := host.Destroy(context.Background(), sprite); err != nil {
			t.Logf("cleanup destroy %s: %v", sprite, err)
		} else {
			t.Logf("cleanup: destroyed %s", sprite)
		}
	})

	t.Log("provisioning workspace (create sprite + install zellij + clone)…")
	wsInfo, err := ws.Create(ctx, ports.WorkspaceConfig{
		ProjectID: projectID,
		SessionID: sessionID,
		Kind:      domain.KindWorker,
		Branch:    branch,
	})
	if err != nil {
		t.Fatalf("Workspace.Create: %v", err)
	}
	if !wsInfo.Remote {
		t.Fatalf("WorkspaceInfo.Remote = false, want true")
	}
	if !strings.Contains(wsInfo.Path, "advanced-imessage-go") {
		t.Fatalf("workspace path %q does not contain the repo dir", wsInfo.Path)
	}
	t.Logf("workspace ready at %s on branch %s", wsInfo.Path, wsInfo.Branch)

	// A benign long-running "agent" so the test is deterministic (claude itself
	// needs interactive login until Phase 3 injects credentials).
	agentArgv := []string{"bash", "-lc", "echo AGENT_UP; while true; do echo tick $(date +%H:%M:%S); sleep 1; done"}

	t.Log("launching agent in zellij…")
	handle, err := rt.Create(ctx, ports.RuntimeConfig{
		SessionID:     sessionID,
		WorkspacePath: wsInfo.Path,
		Argv:          agentArgv,
		Env:           map[string]string{"AO_SESSION_ID": string(sessionID)},
	})
	if err != nil {
		t.Fatalf("Runtime.Create: %v", err)
	}
	t.Logf("runtime handle: %s", handle.ID)

	alive, err := rt.IsAlive(ctx, handle)
	if err != nil || !alive {
		t.Fatalf("IsAlive = (%v, %v), want (true, nil)", alive, err)
	}

	// Attach a TTY and confirm the pane streams output.
	argv, env, err := rt.AttachCommand(handle)
	if err != nil {
		t.Fatalf("AttachCommand: %v", err)
	}
	pty, err := rt.Spawn(ctx, argv, env, 40, 120)
	if err != nil {
		t.Fatalf("Spawn attach: %v", err)
	}
	var mu sync.Mutex
	var buf []byte
	go func() {
		b := make([]byte, 4096)
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
	time.Sleep(6 * time.Second)
	_ = pty.Close()
	mu.Lock()
	got := string(buf)
	mu.Unlock()
	t.Logf("attach captured %d bytes", len(got))
	if len(got) == 0 {
		t.Fatalf("attach produced no output")
	}
	if !strings.Contains(got, "tick") && !strings.Contains(got, "AGENT_UP") {
		t.Errorf("attach output did not contain expected agent output; sample tail: %q", tail(got, 300))
	}

	// Teardown via the adapters (in addition to the cleanup safety net).
	if err := rt.Destroy(ctx, handle); err != nil {
		t.Errorf("Runtime.Destroy: %v", err)
	}
	if err := ws.Destroy(ctx, wsInfo); err != nil {
		t.Errorf("Workspace.Destroy: %v", err)
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
