package flysprite

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"path"

	sprites "github.com/superfly/sprites-go"

	"github.com/aoagents/agent-orchestrator/backend/internal/terminal"
)

// Box is one Fly Sprite's exec + filesystem surface, used by the runtime and
// workspace adapters. It is an interface so tests can fake a sprite without the
// network, and so the SDK stays confined to this file.
type Box interface {
	// Run executes argv inside the sprite without a TTY and returns combined
	// stdout+stderr. env may be nil (inherit the sprite's environment); dir may
	// be "" (the sprite's default directory).
	Run(ctx context.Context, env map[string]string, dir string, argv ...string) ([]byte, error)
	// OpenTTY starts argv on a PTY inside the sprite, sized rows×cols, and
	// returns it as a terminal.PTYProcess for the mux attach loop.
	OpenTTY(ctx context.Context, rows, cols uint16, argv ...string) (terminal.PTYProcess, error)
	// WriteFile writes data into the sprite filesystem at filePath, creating
	// parent directories.
	WriteFile(ctx context.Context, filePath string, data []byte, mode fs.FileMode) error
}

// Host manages Fly Sprite lifecycle and hands out Boxes.
type Host interface {
	Create(ctx context.Context, name string) (Box, error)
	Open(ctx context.Context, name string) (Box, error)
	// Status reports the sprite's lifecycle status. found is false ONLY when the
	// sprite definitively does not exist (HTTP 404); any other failure is a probe
	// error returned in err — never a false "gone" (see the death-semantics rule).
	Status(ctx context.Context, name string) (status string, found bool, err error)
	Destroy(ctx context.Context, name string) error
}

// NewHost builds a Host backed by the real Sprites API at api.sprites.dev.
func NewHost(token string) Host { return &sdkHost{client: sprites.New(token)} }

type sdkHost struct{ client *sprites.Client }

func (h *sdkHost) Create(ctx context.Context, name string) (Box, error) {
	sp, err := h.client.CreateSprite(ctx, name, nil)
	if err != nil {
		return nil, fmt.Errorf("flysprite: create sprite %s: %w", name, err)
	}
	return &sdkBox{sp: sp}, nil
}

func (h *sdkHost) Open(ctx context.Context, name string) (Box, error) {
	sp, err := h.client.GetSprite(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("flysprite: open sprite %s: %w", name, err)
	}
	return &sdkBox{sp: sp}, nil
}

func (h *sdkHost) Status(ctx context.Context, name string) (string, bool, error) {
	sp, err := h.client.GetSprite(ctx, name)
	if err != nil {
		if isNotFound(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("flysprite: status %s: %w", name, err)
	}
	return sp.Status, true, nil
}

func (h *sdkHost) Destroy(ctx context.Context, name string) error {
	if err := h.client.DestroySprite(ctx, name); err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("flysprite: destroy sprite %s: %w", name, err)
	}
	return nil
}

// isNotFound reports whether err is a definitive 404 from the Sprites API.
func isNotFound(err error) bool {
	if ae := sprites.IsAPIError(err); ae != nil {
		return ae.StatusCode == http.StatusNotFound
	}
	return false
}

type sdkBox struct{ sp *sprites.Sprite }

func (b *sdkBox) Run(ctx context.Context, env map[string]string, dir string, argv ...string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("flysprite: Run requires a command")
	}
	cmd := b.sp.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = envSlice(env)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}

func (b *sdkBox) OpenTTY(ctx context.Context, rows, cols uint16, argv ...string) (terminal.PTYProcess, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("flysprite: OpenTTY requires a command")
	}
	// A cancelable child context lets Close detach this client (kill the attach
	// exec) without disturbing the in-sprite zellij session other clients share.
	ttyCtx, cancel := context.WithCancel(ctx)
	cmd := b.sp.CommandContext(ttyCtx, argv[0], argv[1:]...)
	cmd.SetTTY(true)
	if rows > 0 && cols > 0 {
		_ = cmd.SetTTYSize(rows, cols)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("flysprite: tty stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("flysprite: tty stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("flysprite: start tty exec: %w", err)
	}
	return &spritePTY{cmd: cmd, stdin: stdin, stdout: stdout, cancel: cancel}, nil
}

func (b *sdkBox) WriteFile(ctx context.Context, filePath string, data []byte, mode fs.FileMode) error {
	fsys := b.sp.Filesystem()
	if dir := path.Dir(filePath); dir != "" && dir != "." && dir != "/" {
		if err := fsys.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("flysprite: mkdir %s: %w", dir, err)
		}
	}
	if err := fsys.WriteFileContext(ctx, filePath, data, mode); err != nil {
		return fmt.Errorf("flysprite: write %s: %w", filePath, err)
	}
	return nil
}

// envSlice renders an env map as a sorted KEY=VALUE slice (sorted for
// determinism; sprite Cmd.Env nil means inherit).
func envSlice(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for _, k := range sortedKeys(env) {
		out = append(out, k+"="+env[k])
	}
	return out
}
