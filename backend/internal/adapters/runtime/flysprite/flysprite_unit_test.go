package flysprite

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/terminal"
)

// fakeBox records the commands/files an adapter issues and returns scripted
// combined output keyed by a substring of the bash -c script.
type fakeBox struct {
	runs    []string
	written map[string][]byte
	scripts map[string]string // substring of script → combined output
	ttyArgv []string
}

func newFakeBox() *fakeBox {
	return &fakeBox{written: map[string][]byte{}, scripts: map[string]string{}}
}

func (b *fakeBox) Run(_ context.Context, _ map[string]string, _ string, argv ...string) ([]byte, error) {
	script := strings.Join(argv, " ")
	b.runs = append(b.runs, script)
	for sub, out := range b.scripts {
		if strings.Contains(script, sub) {
			return []byte(out), nil
		}
	}
	return nil, nil
}

func (b *fakeBox) OpenTTY(_ context.Context, _, _ uint16, argv ...string) (terminal.PTYProcess, error) {
	b.ttyArgv = argv
	return fakePTY{}, nil
}

func (b *fakeBox) WriteFile(_ context.Context, p string, data []byte, _ fs.FileMode) error {
	b.written[p] = data
	return nil
}

type fakePTY struct{}

func (fakePTY) Read([]byte) (int, error)    { return 0, errors.New("eof") }
func (fakePTY) Write(p []byte) (int, error) { return len(p), nil }
func (fakePTY) Resize(uint16, uint16) error { return nil }
func (fakePTY) Close() error                { return nil }

type fakeHost struct {
	box       *fakeBox
	status    string
	found     bool
	statusErr error
	created   []string
	destroyed []string
}

func (h *fakeHost) Create(_ context.Context, name string) (Box, error) {
	h.created = append(h.created, name)
	return h.box, nil
}
func (h *fakeHost) Open(context.Context, string) (Box, error) { return h.box, nil }
func (h *fakeHost) Status(context.Context, string) (string, bool, error) {
	return h.status, h.found, h.statusErr
}
func (h *fakeHost) Destroy(_ context.Context, name string) error {
	h.destroyed = append(h.destroyed, name)
	return nil
}

func newAliveHost() *fakeHost {
	box := newFakeBox()
	box.scripts["list-sessions"] = "x [Created 1s ago]\n"
	return &fakeHost{box: box, status: "running", found: true}
}

func TestRuntimeCreateWritesLayoutAndVerifies(t *testing.T) {
	host := newAliveHost()
	rt := New(Options{Host: host})

	handle, err := rt.Create(context.Background(), ports.RuntimeConfig{
		SessionID:     "x",
		WorkspacePath: "/home/sprite/workspace/repo",
		Argv:          []string{"/opt/homebrew/bin/claude", "--", "fix it"},
		Env:           map[string]string{"AO_SESSION_ID": "x"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	h, err := parseFlyHandle(handle.ID)
	if err != nil || h.Sprite != "ao-x" || h.Session != "x" {
		t.Fatalf("handle = %+v, %v", h, err)
	}
	// A layout was written, and it normalized the absolute claude path to bare.
	var layout string
	for p, data := range host.box.written {
		if strings.Contains(p, "layout-x.kdl") {
			layout = string(data)
		}
	}
	if layout == "" {
		t.Fatal("no layout file written")
	}
	if strings.Contains(layout, "/opt/homebrew/bin/claude") {
		t.Errorf("layout did not normalize argv[0]:\n%s", layout)
	}
	// A create-background command ran.
	if !ranContaining(host.box.runs, "--create-background") {
		t.Errorf("no create-background command ran: %v", host.box.runs)
	}
}

func TestIsAlive(t *testing.T) {
	handle := ports.RuntimeHandle{ID: flyHandle{Sprite: "ao-x", Session: "x"}.encode()}

	t.Run("listed is alive", func(t *testing.T) {
		rt := New(Options{Host: newAliveHost()})
		alive, err := rt.IsAlive(context.Background(), handle)
		if err != nil || !alive {
			t.Fatalf("IsAlive = (%v, %v), want (true, nil)", alive, err)
		}
	})
	t.Run("404 sprite is definitively not alive", func(t *testing.T) {
		host := newAliveHost()
		host.found = false
		rt := New(Options{Host: host})
		alive, err := rt.IsAlive(context.Background(), handle)
		if err != nil || alive {
			t.Fatalf("IsAlive = (%v, %v), want (false, nil)", alive, err)
		}
	})
	t.Run("status probe error propagates (not death)", func(t *testing.T) {
		host := newAliveHost()
		host.statusErr = errors.New("network blip")
		rt := New(Options{Host: host})
		alive, err := rt.IsAlive(context.Background(), handle)
		if err == nil || alive {
			t.Fatalf("IsAlive = (%v, %v), want (false, err)", alive, err)
		}
	})
	t.Run("no active sessions is not alive", func(t *testing.T) {
		host := newAliveHost()
		host.box.scripts["list-sessions"] = "No active zellij sessions found."
		rt := New(Options{Host: host})
		alive, err := rt.IsAlive(context.Background(), handle)
		if err != nil || alive {
			t.Fatalf("IsAlive = (%v, %v), want (false, nil)", alive, err)
		}
	})
}

func TestAttachCommandCarriesSprite(t *testing.T) {
	rt := New(Options{Host: newAliveHost()})
	handle := ports.RuntimeHandle{ID: flyHandle{Sprite: "ao-x", Session: "x"}.encode()}
	argv, env, err := rt.AttachCommand(handle)
	if err != nil {
		t.Fatalf("AttachCommand: %v", err)
	}
	if argv[0] != "bash" || !strings.Contains(strings.Join(argv, " "), "zellij") {
		t.Fatalf("attach argv unexpected: %v", argv)
	}
	if got := envValue(env, spriteEnvKey); got != "ao-x" {
		t.Fatalf("attach env sprite = %q, want ao-x", got)
	}
	// Spawn uses that env to reach the right sprite.
	if _, err := rt.Spawn(context.Background(), argv, env, 40, 120); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if !strings.Contains(strings.Join(rt.host.(*fakeHost).box.ttyArgv, " "), "zellij") {
		t.Fatalf("OpenTTY did not receive the attach argv: %v", rt.host.(*fakeHost).box.ttyArgv)
	}
}

func TestSendMessageWritesCharsThenEnter(t *testing.T) {
	host := newAliveHost()
	rt := New(Options{Host: host})
	handle := ports.RuntimeHandle{ID: flyHandle{Sprite: "ao-x", Session: "x"}.encode()}
	if err := rt.SendMessage(context.Background(), handle, "hello"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if !ranContaining(host.box.runs, "write-chars") {
		t.Errorf("no write-chars command: %v", host.box.runs)
	}
	if !ranContaining(host.box.runs, "'write' '13'") {
		t.Errorf("no Enter (write 13) command: %v", host.box.runs)
	}
}

func TestWorkspaceCreateRemote(t *testing.T) {
	box := newFakeBox()
	// Key on substrings unique to each provisioning script: the install script
	// runs mkdir; the clone script runs `git clone`.
	box.scripts["mkdir"] = "ZELLIJ_OK\n"
	box.scripts["git clone"] = "CLONE_OK\n"
	host := &fakeHost{box: box, status: "running", found: true}
	ws := NewWorkspace(host, staticReposUnit{url: "https://github.com/photon-hq/advanced-imessage-go.git"})

	info, err := ws.Create(context.Background(), ports.WorkspaceConfig{
		ProjectID: "photon", SessionID: "x", Kind: domain.KindWorker, Branch: "ao-x",
	})
	if err != nil {
		t.Fatalf("Workspace.Create: %v", err)
	}
	if !info.Remote {
		t.Error("WorkspaceInfo.Remote = false, want true")
	}
	if !strings.HasSuffix(info.Path, "/advanced-imessage-go") {
		t.Errorf("workspace path = %q", info.Path)
	}
	if len(host.created) != 1 || host.created[0] != "ao-x" {
		t.Errorf("sprite created = %v, want [ao-x]", host.created)
	}
}

func TestWorkspaceCreateRejectsEmptyOrigin(t *testing.T) {
	host := &fakeHost{box: newFakeBox(), found: false}
	ws := NewWorkspace(host, staticReposUnit{url: ""})
	_, err := ws.Create(context.Background(), ports.WorkspaceConfig{ProjectID: "p", SessionID: "x", Branch: "b"})
	if err == nil || !strings.Contains(err.Error(), "no clonable git remote") {
		t.Fatalf("want empty-origin error, got %v", err)
	}
}

type staticReposUnit struct{ url string }

func (s staticReposUnit) RepoOriginURL(context.Context, domain.ProjectID) (string, error) {
	return s.url, nil
}

func ranContaining(runs []string, sub string) bool {
	for _, r := range runs {
		if strings.Contains(r, sub) {
			return true
		}
	}
	return false
}
