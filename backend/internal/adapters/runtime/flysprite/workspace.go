package flysprite

import (
	"context"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const (
	// workspaceRoot is where session repos are cloned inside the sprite.
	workspaceRoot = "/home/sprite/workspace"
	// zellijVersion pins the zellij the sprite installs — the version the local
	// adapter (and its version check) supports.
	zellijVersion = "v0.44.3"
)

// RepoResolver maps a project to a clonable git remote URL. The flysprite
// workspace clones inside the sprite, so it needs a remote URL (a local path is
// useless on the remote box).
type RepoResolver interface {
	RepoOriginURL(ctx context.Context, projectID domain.ProjectID) (string, error)
}

// Workspace provisions a session's working copy INSIDE a Fly Sprite: it creates
// the sprite, ensures zellij is installed, and clones the project repo onto the
// session branch. It returns a remote WorkspaceInfo so the session manager skips
// daemon-host provisioning.
type Workspace struct {
	host  Host
	repos RepoResolver
}

// NewWorkspace builds a Workspace. Both dependencies are required.
func NewWorkspace(host Host, repos RepoResolver) *Workspace {
	if host == nil || repos == nil {
		panic("flysprite: Workspace requires Host and RepoResolver")
	}
	return &Workspace{host: host, repos: repos}
}

// Create destroys any leftover sprite for the session, creates a fresh one,
// installs zellij, and clones the repo onto the branch.
func (w *Workspace) Create(ctx context.Context, cfg ports.WorkspaceConfig) (ports.WorkspaceInfo, error) {
	sprite, err := spriteName(cfg.SessionID)
	if err != nil {
		return ports.WorkspaceInfo{}, err
	}
	originURL, err := w.repos.RepoOriginURL(ctx, cfg.ProjectID)
	if err != nil {
		return ports.WorkspaceInfo{}, fmt.Errorf("flysprite workspace: resolve repo for project %s: %w", cfg.ProjectID, err)
	}
	originURL = strings.TrimSpace(originURL)
	if originURL == "" {
		return ports.WorkspaceInfo{}, fmt.Errorf("flysprite workspace: project %s has no clonable git remote (RepoOriginURL is empty); sprites clone over the network and cannot use a local-only repo", cfg.ProjectID)
	}
	branch := strings.TrimSpace(cfg.Branch)
	if branch == "" {
		return ports.WorkspaceInfo{}, fmt.Errorf("flysprite workspace: branch is required")
	}

	// A fresh sprite per spawn. Destroy tolerates a missing sprite (404).
	if err := w.host.Destroy(ctx, sprite); err != nil {
		return ports.WorkspaceInfo{}, err
	}
	box, err := w.host.Create(ctx, sprite)
	if err != nil {
		return ports.WorkspaceInfo{}, err
	}

	repoDir, err := w.provision(ctx, box, originURL, branch)
	if err != nil {
		// Roll back the sprite so a failed clone does not leak compute.
		_ = w.host.Destroy(context.Background(), sprite)
		return ports.WorkspaceInfo{}, err
	}
	return ports.WorkspaceInfo{
		Path:      repoDir,
		Branch:    branch,
		SessionID: cfg.SessionID,
		ProjectID: cfg.ProjectID,
		Remote:    true,
	}, nil
}

// Restore reconnects to an existing sprite (its filesystem persists across
// suspend/restart). If the sprite is gone it is re-provisioned from scratch.
func (w *Workspace) Restore(ctx context.Context, cfg ports.WorkspaceConfig) (ports.WorkspaceInfo, error) {
	sprite, err := spriteName(cfg.SessionID)
	if err != nil {
		return ports.WorkspaceInfo{}, err
	}
	_, found, err := w.host.Status(ctx, sprite)
	if err != nil {
		return ports.WorkspaceInfo{}, err
	}
	if !found {
		return w.Create(ctx, cfg)
	}
	branch := strings.TrimSpace(cfg.Branch)
	return ports.WorkspaceInfo{
		Path:      repoDirFor(w.mustOriginURL(ctx, cfg.ProjectID)),
		Branch:    branch,
		SessionID: cfg.SessionID,
		ProjectID: cfg.ProjectID,
		Remote:    true,
	}, nil
}

// Destroy tears down the session's sprite (and with it the cloned workspace).
func (w *Workspace) Destroy(ctx context.Context, info ports.WorkspaceInfo) error {
	sprite, err := spriteName(info.SessionID)
	if err != nil {
		return err
	}
	return w.host.Destroy(ctx, sprite)
}

// provision installs zellij and clones the repo onto the branch, returning the
// in-sprite repo directory.
func (w *Workspace) provision(ctx context.Context, box Box, originURL, branch string) (string, error) {
	if out, err := box.Run(ctx, nil, "", "bash", "-c", installZellijScript()); err != nil || !strings.Contains(string(out), "ZELLIJ_OK") {
		return "", fmt.Errorf("flysprite workspace: install zellij: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	repoDir := repoDirFor(originURL)
	if out, err := box.Run(ctx, nil, "", "bash", "-c", cloneScript(originURL, repoDir, branch)); err != nil || !strings.Contains(string(out), "CLONE_OK") {
		return "", fmt.Errorf("flysprite workspace: clone %s: %w (%s)", originURL, err, strings.TrimSpace(string(out)))
	}
	return repoDir, nil
}

func (w *Workspace) mustOriginURL(ctx context.Context, projectID domain.ProjectID) string {
	url, _ := w.repos.RepoOriginURL(ctx, projectID)
	return strings.TrimSpace(url)
}

// repoDirFor derives the in-sprite clone directory from a remote URL.
func repoDirFor(originURL string) string {
	return workspaceRoot + "/" + repoBaseName(originURL)
}

// repoBaseName extracts the repository directory name from a git URL, stripping
// any trailing ".git" and path/scheme. Falls back to "repo".
func repoBaseName(originURL string) string {
	s := strings.TrimSpace(originURL)
	s = strings.TrimSuffix(s, ".git")
	s = strings.TrimRight(s, "/")
	if i := strings.LastIndexAny(s, "/:"); i >= 0 {
		s = s[i+1:]
	}
	if s == "" {
		return "repo"
	}
	return s
}

func installZellijScript() string {
	return `set -e
mkdir -p "$HOME/.local/bin"
export PATH="$HOME/.local/bin:$PATH"
if ! command -v zellij >/dev/null 2>&1; then
  curl -fsSL https://github.com/zellij-org/zellij/releases/download/` + zellijVersion + `/zellij-x86_64-unknown-linux-musl.tar.gz -o /tmp/z.tgz
  tar xzf /tmp/z.tgz -C "$HOME/.local/bin"
fi
zellij --version >/dev/null
echo ZELLIJ_OK`
}

// cloneScript clones originURL into repoDir and checks out branch, creating it
// from the default HEAD when it does not exist on the remote.
func cloneScript(originURL, repoDir, branch string) string {
	q := func(s string) string { return shellQuote(s) }
	return `set -e
rm -rf ` + q(repoDir) + `
git clone ` + q(originURL) + ` ` + q(repoDir) + `
cd ` + q(repoDir) + `
if git ls-remote --exit-code --heads origin ` + q(branch) + ` >/dev/null 2>&1; then
  git checkout ` + q(branch) + `
else
  git checkout -b ` + q(branch) + `
fi
echo CLONE_OK`
}
