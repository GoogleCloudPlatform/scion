// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package runtime

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// CloudRunRuntime implements the Runtime interface for Google Cloud Run.
//
// Cloud Run with a host-mounted share (or managed volume) calls Tier-1
// ProvisionShared DIRECTLY broker-side — no init container is needed
// because the broker provisions before deploying the Cloud Run service.
//
// Lifecycle methods (deploy/exec/logs via the Cloud Run Admin API) are
// deferred to a follow-up PR — this implementation focuses on the
// provisioning + mount-realization wiring that PR3 requires. Lifecycle
// methods return a descriptive "not yet implemented" error.
type CloudRunRuntime struct {
	// Project is the GCP project ID for Cloud Run API calls.
	Project string

	// Region is the GCP region for Cloud Run services (e.g. "us-central1").
	Region string

	// WorkspaceStorage holds the workspace storage configuration, used to
	// select the workspace backend and realize mount descriptors.
	WorkspaceStorage *config.V1WorkspaceStorageConfig
}

// NewCloudRunRuntime returns a new CloudRunRuntime.
func NewCloudRunRuntime(cfg *config.V1CloudRunConfig) *CloudRunRuntime {
	rt := &CloudRunRuntime{}
	if cfg != nil {
		rt.Project = cfg.Project
		rt.Region = cfg.Region
	}
	return rt
}

func (r *CloudRunRuntime) Name() string { return "cloudrun" }

func (r *CloudRunRuntime) ExecUser() string { return "scion" }

// Run provisions the workspace broker-side using Tier-1 ProvisionShared,
// then would deploy a Cloud Run service. The deployment step is deferred.
//
// This is the broker-side direct provisioning path: Cloud Run with a
// host-mounted share calls ProvisionShared directly (no init container),
// because the broker has access to the mounted filesystem before the
// container is deployed.
func (r *CloudRunRuntime) Run(ctx context.Context, cfg RunConfig) (string, error) {
	if err := r.provisionWorkspace(ctx, cfg); err != nil {
		return "", fmt.Errorf("cloudrun: workspace provisioning failed: %w", err)
	}

	// TODO(PR-followup): Deploy Cloud Run service via Admin API.
	// The service spec would reference the workspace volume (cloudrun-volume
	// or NFS mount) with the realized MountDescriptor fields.
	return "", fmt.Errorf("cloudrun: Run not yet implemented — workspace provisioned, but Cloud Run service deployment requires the Admin API (follow-up PR)")
}

// provisionWorkspace performs broker-side direct provisioning for the Cloud
// Run runtime. It selects the workspace backend, resolves paths, and calls
// ProvisionShared (Tier 1) directly — no init container. The context is
// propagated so provisioning (git clone, chown) is cancellable.
func (r *CloudRunRuntime) provisionWorkspace(ctx context.Context, cfg RunConfig) error {
	if cfg.ProjectID == "" {
		return fmt.Errorf("ProjectID is required for workspace provisioning")
	}

	// Cloud Run uses a shared, plain workspace for now: the initial runtime
	// scope provisions a single broker-side workspace per project. Per-agent
	// worktrees (SharingModeWorktreePerAgent) are a follow-up once Cloud Run
	// multi-agent lifecycle lands, so the mode is fixed rather than derived.
	mode := store.SharingModeSharedPlain
	backend := SelectWorkspaceBackend(r.WorkspaceStorage, mode)

	slog.Info("cloudrun: provisioning workspace broker-side",
		"project_id", cfg.ProjectID,
		"backend", backend.Name())

	resolved, err := backend.Resolve(ResolveInput{
		ProjectID:  cfg.ProjectID,
		ProjectDir: cfg.Workspace,
		Mode:       mode,
	})
	if err != nil {
		return fmt.Errorf("resolve workspace: %w", err)
	}

	// Only call ProvisionShared for backends with a host path (NFS, local
	// with shared storage). Cloud Run managed volumes are provisioned by the
	// platform, not the broker.
	if resolved.HostPath != "" {
		err = ProvisionShared(ProvisionInput{
			Ctx:       ctx,
			Resolved:  resolved,
			ProjectID: cfg.ProjectID,
			AgentID:   cfg.Labels["agent_id"],
			AgentName: cfg.Name,
			Mode:      mode,
			GitClone:  cfg.GitClone,
			Locker:    cfg.Locker,
			NFSUID:    cfg.NFSUID,
			NFSGID:    cfg.NFSGID,
		})
		if err != nil {
			return fmt.Errorf("ProvisionShared: %w", err)
		}
	}

	return nil
}

func (r *CloudRunRuntime) Stop(ctx context.Context, id string) error {
	return fmt.Errorf("cloudrun: Stop not yet implemented")
}

func (r *CloudRunRuntime) Delete(ctx context.Context, id string) error {
	return fmt.Errorf("cloudrun: Delete not yet implemented")
}

func (r *CloudRunRuntime) List(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
	return nil, fmt.Errorf("cloudrun: List not yet implemented")
}

func (r *CloudRunRuntime) GetLogs(ctx context.Context, id string) (string, error) {
	return "", fmt.Errorf("cloudrun: GetLogs not yet implemented")
}

func (r *CloudRunRuntime) Attach(ctx context.Context, id string) error {
	return fmt.Errorf("cloudrun: Attach not yet implemented")
}

func (r *CloudRunRuntime) ImageExists(ctx context.Context, image string) (bool, error) {
	return false, fmt.Errorf("cloudrun: ImageExists not yet implemented")
}

func (r *CloudRunRuntime) ImageID(ctx context.Context, image string) (string, error) {
	return "", fmt.Errorf("cloudrun: ImageID not yet implemented")
}

func (r *CloudRunRuntime) RemoveImage(ctx context.Context, image string) error {
	return fmt.Errorf("cloudrun: RemoveImage not yet implemented")
}

func (r *CloudRunRuntime) PullImage(ctx context.Context, image string) error {
	return fmt.Errorf("cloudrun: PullImage not yet implemented")
}

func (r *CloudRunRuntime) Sync(ctx context.Context, id string, direction SyncDirection) error {
	return fmt.Errorf("cloudrun: Sync not yet implemented")
}

func (r *CloudRunRuntime) Exec(ctx context.Context, id string, cmd []string) (string, error) {
	return "", fmt.Errorf("cloudrun: Exec not yet implemented")
}

func (r *CloudRunRuntime) GetWorkspacePath(ctx context.Context, id string) (string, error) {
	return "", fmt.Errorf("cloudrun: GetWorkspacePath not yet implemented")
}
