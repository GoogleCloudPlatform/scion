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

//go:build unix

package hub

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs/migrate"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Run implements MaintenanceExecutor.
func (e *ProjectStorageMigrateExecutor) Run(ctx context.Context, w io.Writer, params map[string]string) error {
	dryRun := params["dryRun"] == "true"
	if !dryRun {
		// Whatever the outcome, the notice's cached plan is stale now.
		defer e.srv.storageMigrationNotice.invalidate()
	}
	in, err := e.srv.projectStorageMigrateInput(ctx, true)
	if err != nil {
		return err
	}
	rep, err := migrate.Run(ctx, in, migrate.Options{DryRun: dryRun, AdoptUnverified: adoptUnverified(params["adoptUnverified"])})
	rep.WriteLog(w)
	if err != nil {
		return err
	}
	if rep.Failed() {
		return errors.New("one or more projects did not migrate completely; see the log, fix the cause and run again")
	}
	return nil
}

// adoptUnverified parses the adoptUnverified parameter: "true" adopts the
// unverified legacy trees of every project, a comma-separated list of
// project slugs adopts those projects' only, and anything else none
// (ptone/scion#5819). Trees whose identity marker names another project
// are never adopted.
func adoptUnverified(param string) func(slug string) bool {
	param = strings.TrimSpace(param)
	if param == "" || param == "false" {
		return nil
	}
	if param == "true" {
		return func(string) bool { return true }
	}
	slugs := map[string]bool{}
	for _, s := range strings.Split(param, ",") {
		if s = strings.TrimSpace(s); s != "" {
			slugs[s] = true
		}
	}
	return func(slug string) bool { return slugs[slug] }
}

// projectStorageMigrateInput gathers one hub-wide migrate.HubInput: the
// workspace and shared-dir Layouts the hub serves files from, its global
// dir, every project, and (withObjects) its object storage for the object-storage cleanup.
func (s *Server) projectStorageMigrateInput(ctx context.Context, withObjects bool) (migrate.HubInput, error) {
	wsLayout, err := s.workspaceLayout()
	if err != nil {
		return migrate.HubInput{}, fmt.Errorf("workspace storage: %w", err)
	}
	sdLayout, _, err := s.sharedDirLayout()
	if err != nil {
		return migrate.HubInput{}, fmt.Errorf("shared dir storage: %w", err)
	}
	in := migrate.HubInput{Workspace: wsLayout, SharedDirs: sdLayout}
	if !wsLayout.Backend.Shared() && !sdLayout.Backend.Shared() {
		return in, nil // migrate.Run reports the no-op
	}
	if in.GlobalDir, err = config.GetGlobalDir(); err != nil {
		return migrate.HubInput{}, fmt.Errorf("global dir: %w", err)
	}
	if in.Projects, err = s.storageMigrateProjects(ctx); err != nil {
		return migrate.HubInput{}, err
	}
	in.ProvisionLock = func(ws migrate.Tree, section func(lockCtx context.Context) error) error {
		return withWorkspaceProvisionLocks(ctx, ws, section)
	}
	// Step 5 re-reads an agent's phase right before moving its home.
	in.AgentStopped = func(lockCtx context.Context, projectID, agentID string) (bool, error) {
		a, err := s.store.GetAgent(lockCtx, agentID)
		if err != nil {
			return false, err
		}
		return a.ProjectID == projectID && storageMigrateAgentStopped(a.Phase), nil
	}
	if stor := s.GetStorage(); withObjects && stor != nil {
		hubID := s.HubID()
		in.Objects = storageObjects{stor}
		in.ObjectPrefix = func(projectID string) string {
			return migrate.LegacyObjectPrefix(hubID, projectID)
		}
	}
	return in, nil
}

// storageMigrateProjects lists every project as the migration sees it.
func (s *Server) storageMigrateProjects(ctx context.Context) ([]migrate.ProjectRef, error) {
	var refs []migrate.ProjectRef
	cursor := ""
	for {
		page, err := s.store.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: 500, Cursor: cursor, SkipTotalCount: true})
		if err != nil {
			return nil, fmt.Errorf("list projects: %w", err)
		}
		for i := range page.Items {
			p := &page.Items[i]
			ref := storageMigrateProjectRef(p)
			ref.LocalBrokerTree = s.localTreeServedByEmbeddedBroker(ctx, p)
			agents, err := s.storageMigrateAgents(ctx, p.ID)
			if err != nil {
				return nil, err
			}
			ref.Agents = agents
			refs = append(refs, ref)
		}
		if page.NextCursor == "" || page.NextCursor == cursor {
			return refs, nil
		}
		cursor = page.NextCursor
	}
}

// storageMigrateAgents lists a project's agents for migration step 5 (Cloud
// Run homes, ptone/scion#5825): each agent's ID and slug, and whether it is
// stopped (stopped or halted: no instance holds its home), the only state
// in which its home is moved.
func (s *Server) storageMigrateAgents(ctx context.Context, projectID string) ([]migrate.AgentRef, error) {
	var refs []migrate.AgentRef
	cursor := ""
	for {
		page, err := s.store.ListAgents(ctx, store.AgentFilter{ProjectID: projectID}, store.ListOptions{Limit: 500, Cursor: cursor, SkipTotalCount: true})
		if err != nil {
			return nil, fmt.Errorf("list agents of project %s: %w", projectID, err)
		}
		for i := range page.Items {
			a := &page.Items[i]
			refs = append(refs, migrate.AgentRef{ID: a.ID, Slug: a.Slug, Stopped: storageMigrateAgentStopped(a.Phase)})
		}
		if page.NextCursor == "" || page.NextCursor == cursor {
			return refs, nil
		}
		cursor = page.NextCursor
	}
}

// storageMigrateAgentStopped reports whether an agent in phase holds no
// running instance, so its home can be moved. An agent in phase error may
// still have a live instance, so it is not.
func storageMigrateAgentStopped(phase string) bool {
	switch state.Phase(phase) {
	case state.PhaseStopped, state.PhaseHalted:
		return true
	}
	return false
}

// storageMigrateProjectRef maps a project to the migration's view: it has a
// project-scoped hub workspace exactly when projectWorkspaceResolved
// resolves one (not empty-per-agent, and not a linked project in a
// per-agent git mode).
func storageMigrateProjectRef(p *store.Project) migrate.ProjectRef {
	return migrate.ProjectRef{
		ID:          p.ID,
		Slug:        p.Slug,
		Git:         p.GitRemote != "",
		NoWorkspace: p.IsEmptyPerAgent() || (p.GitRemote != "" && !p.IsSharedWorkspace()),
	}
}

// localTreeServedByEmbeddedBroker reports whether this process's embedded
// broker serves project from ~/.scion/projects/<slug>, the hub-local tree
// migration steps 2 and 4 read. Such a tree is only copied
// from. A provider lookup that fails counts as served: a needless copy-only
// leaves a tree behind, a wrong rename breaks a broker.
func (s *Server) localTreeServedByEmbeddedBroker(ctx context.Context, project *store.Project) bool {
	if !s.hasEmbeddedBroker() {
		return false
	}
	providers, err := s.store.GetProjectProviders(ctx, project.ID)
	if err != nil {
		return true
	}
	return s.embeddedBrokerUsesSlugProjectDir(project, providers)
}

// withWorkspaceProvisionLocks runs section holding every provisioning lock
// a provisioner of the workspace ws may take (provision.
// WithWorkspaceProvisionLocks: the project root for the hub-side Cloud Run
// provisioner, the project state directory for the Kubernetes init
// container; derived by pkg/provision itself). The state directory is
// first created, if absent, with the broker's leaf contract
// (shareddirs.EnsureLeaf: mode 2775 and the default ACL), never by the
// lock code.
func withWorkspaceProvisionLocks(ctx context.Context, ws migrate.Tree, section func(lockCtx context.Context) error) error {
	base, err := filepath.EvalSymlinks(ws.Base)
	if err != nil {
		return fmt.Errorf("resolve host base: %w", err)
	}
	wsPath := filepath.Join(base, ws.Rel)
	stateRel, err := filepath.Rel(base, provision.ProjectStateDir(wsPath))
	if err != nil || !filepath.IsLocal(stateRel) {
		return fmt.Errorf("provisioning state dir is not below the host base")
	}
	fd, _, err := shareddirs.EnsureLeaf(base, stateRel)
	if err != nil {
		return fmt.Errorf("create provisioning state dir: %w", err)
	}
	if err := shareddirs.CloseFd(fd); err != nil {
		return err
	}
	return provision.WithWorkspaceProvisionLocks(ctx, wsPath, section)
}

// storageObjects adapts storage.Storage to migrate.ObjectStore.
type storageObjects struct{ s storage.Storage }

func (o storageObjects) HasObjects(ctx context.Context, prefix string) (bool, error) {
	res, err := o.s.List(ctx, storage.ListOptions{Prefix: prefix, MaxResults: 1})
	if err != nil {
		return false, err
	}
	return len(res.Objects) > 0, nil
}

func (o storageObjects) DeletePrefix(ctx context.Context, prefix string) error {
	return o.s.DeletePrefix(ctx, prefix)
}

// planStorageMigration returns the projects the migration would move
// something for, or that have a failed step, with the reason.
func (s *Server) planStorageMigration(ctx context.Context) (map[string]string, error) {
	in, err := s.projectStorageMigrateInput(ctx, false)
	if err != nil {
		return nil, err
	}
	rep, err := migrate.Run(ctx, in, migrate.Options{DryRun: true})
	if err != nil {
		return nil, err
	}
	pending := map[string]string{}
	for _, id := range rep.PendingProjects() {
		pending[id] = ""
	}
	for _, id := range rep.IdentityBlockedProjects() {
		pending[id] = noticeReasonIdentity
	}
	return pending, nil
}

// deleteLegacyProjectTrees removes the legacy slug-keyed trees whose
// identity markers name projectID (migrate.DeleteLegacyProjectTrees), for
// the project storage cleanup of a project delete.
func deleteLegacyProjectTrees(layout projectfs.Layout, globalDir, slug, projectID string) ([]projectfs.Deletion, error) {
	return migrate.DeleteLegacyProjectTrees(layout, globalDir, slug, projectID)
}
