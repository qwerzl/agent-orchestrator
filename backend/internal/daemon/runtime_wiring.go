package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/runtime/flysprite"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/runtime/zellij"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/workspace/gitworktree"
	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/terminal"
)

// sessionRuntime is the full runtime surface the daemon threads through the
// terminal mux (PTYSource: AttachCommand+IsAlive), the reaper (ports.Runtime),
// the agent messenger and the reviewer launcher (SendMessage). Both the local
// zellij runtime and the Fly Sprite runtime implement it, so the daemon can
// select one at startup without the rest of the wiring caring which.
type sessionRuntime interface {
	ports.Runtime
	SendMessage(ctx context.Context, handle ports.RuntimeHandle, message string) error
	AttachCommand(handle ports.RuntimeHandle) ([]string, []string, error)
}

// runtimeStack bundles the runtime-specific adapters the daemon wires together:
// the runtime itself, its matching workspace (local worktree vs in-sprite
// clone), and the terminal manager configured with the right PTY spawner.
type runtimeStack struct {
	Runtime   sessionRuntime
	Workspace ports.Workspace
	Terminal  *terminal.Manager
}

// buildRuntimeStack selects the session runtime from cfg.Runtime and assembles
// the matching workspace + terminal manager. "zellij" runs sessions as local
// panes; "flysprite" runs each session as claude-in-zellij inside a Fly Sprite.
func buildRuntimeStack(cfg config.Config, store *sqlite.Store, events terminal.EventSource, log *slog.Logger) (*runtimeStack, error) {
	switch cfg.Runtime {
	case "flysprite":
		if cfg.SpritesToken == "" {
			return nil, fmt.Errorf("AO_RUNTIME=flysprite requires AO_SPRITES_TOKEN (the Fly Sprites API token)")
		}
		host := flysprite.NewHost(cfg.SpritesToken)
		rt := flysprite.New(flysprite.Options{Host: host, DaemonURL: cfg.PublicURL, AuthToken: cfg.AuthToken})
		secrets, err := flyspriteSecrets(cfg)
		if err != nil {
			return nil, err
		}
		ws := flysprite.NewWorkspace(host, projectOriginResolver{store: store}, secrets)
		// The sprite PTY is opened over the Sprites exec API, not a local process,
		// so the terminal manager uses the runtime's own spawner.
		term := terminal.NewManager(rt, events, log, terminal.WithSpawn(rt.Spawn))
		log.Info("session runtime: flysprite (Fly Sprites)")
		return &runtimeStack{Runtime: rt, Workspace: ws, Terminal: term}, nil

	case "zellij", "":
		// zellij's default socket dir is too long on macOS for long session ids
		// (see zellij.DefaultSocketDir); use a short, stable one and ensure it exists.
		socketDir := zellij.DefaultSocketDir()
		if socketDir != "" {
			if err := os.MkdirAll(socketDir, 0o700); err != nil {
				log.Warn("could not create zellij socket dir; spawns may fail", "dir", socketDir, "error", err)
			}
		}
		rt := zellij.New(zellij.Options{SocketDir: socketDir})
		ws, err := gitworktree.New(gitworktree.Options{
			ManagedRoot:  filepath.Join(cfg.DataDir, "worktrees"),
			RepoResolver: projectRepoResolver{store: store},
		})
		if err != nil {
			return nil, fmt.Errorf("session workspace: %w", err)
		}
		term := terminal.NewManager(rt, events, log)
		return &runtimeStack{Runtime: rt, Workspace: ws, Terminal: term}, nil

	default:
		return nil, fmt.Errorf("unknown AO_RUNTIME %q", cfg.Runtime)
	}
}

// flyspriteSecrets resolves the credentials the daemon injects into each sprite:
// the claude.ai OAuth credential (raw from AO_CLAUDE_CREDENTIALS, else read from
// AO_CLAUDE_CREDENTIALS_FILE) and a GitHub token. Credentials are never read
// from the OS keychain; the operator supplies them as a secret/env/file.
func flyspriteSecrets(cfg config.Config) (flysprite.Secrets, error) {
	creds := cfg.ClaudeCredentials
	if creds == "" && cfg.ClaudeCredentialsFile != "" {
		b, err := os.ReadFile(cfg.ClaudeCredentialsFile)
		if err != nil {
			return flysprite.Secrets{}, fmt.Errorf("read AO_CLAUDE_CREDENTIALS_FILE %q: %w", cfg.ClaudeCredentialsFile, err)
		}
		creds = string(b)
	}
	return flysprite.Secrets{ClaudeCredentials: creds, GitHubToken: cfg.GitHubToken}, nil
}

// projectOriginResolver resolves a project's clonable git remote URL from the
// projects table, for the flysprite workspace (which clones inside the sprite).
// A project with no recorded origin URL cannot be run in a sprite.
type projectOriginResolver struct{ store *sqlite.Store }

var _ flysprite.RepoResolver = projectOriginResolver{}

func (r projectOriginResolver) RepoOriginURL(ctx context.Context, projectID domain.ProjectID) (string, error) {
	rec, ok, err := r.store.GetProject(ctx, string(projectID))
	if err != nil {
		return "", fmt.Errorf("look up project %q: %w", projectID, err)
	}
	if !ok {
		return "", fmt.Errorf("no project registered with id %q — add one with `ao project add`: %w", projectID, sessionmanager.ErrProjectNotResolvable)
	}
	if !rec.ArchivedAt.IsZero() {
		return "", fmt.Errorf("project %q is archived: %w", projectID, sessionmanager.ErrProjectNotResolvable)
	}
	return rec.RepoOriginURL, nil
}
