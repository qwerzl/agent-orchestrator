package project

import "github.com/aoagents/agent-orchestrator/backend/internal/domain"

// GetResult is the discriminated result returned by Service.Get.
type GetResult struct {
	Status   string
	Project  *Project
	Degraded *Degraded
}

// AddInput is the body shape for POST /api/v1/projects.
type AddInput struct {
	Path      string  `json:"path"`
	ProjectID *string `json:"projectId,omitempty"`
	Name      *string `json:"name,omitempty"`
	// RepoOriginURL registers the project by its clonable git remote instead of a
	// local path. Use this against a remote daemon (e.g. the flysprite runtime on
	// Fly/Railway), where the repo is cloned inside the sandbox and the daemon
	// never touches a local checkout. When set, Path is ignored.
	RepoOriginURL *string               `json:"repoOriginUrl,omitempty"`
	Config        *domain.ProjectConfig `json:"config,omitempty"`
	AsWorkspace   bool                  `json:"asWorkspace,omitempty"`
}

// SetConfigInput is the body shape for PUT /api/v1/projects/{id}/config. Config
// replaces the project's stored config wholesale; a zero-value config clears it.
type SetConfigInput struct {
	Config domain.ProjectConfig `json:"config"`
}

// RemoveResult reports what DELETE /api/v1/projects/{id} actually did.
type RemoveResult struct {
	ProjectID         domain.ProjectID `json:"projectId"`
	RemovedStorageDir bool             `json:"removedStorageDir"`
}
