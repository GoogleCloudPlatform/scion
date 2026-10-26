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

package hub

import (
	"context"
	"errors"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs/migrate"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Project and agent storage deletion by the hub (ptone/scion#5835).
//
// The hub is the one deleter of what lives on the shared project storage
// and in its object storage: a project's tree <subpath root>/<project ID>
// (from its own mount of the share, whatever provider rows exist; brokers
// leave it alone), the legacy slug-keyed trees whose identity markers name
// the project, the project's object-storage prefix, an agent's
// agents/<slug> directory and its object-storage prefix. Every deletion is
// audited (projectfs.Audit, deleter "hub"); none ever fails or rolls back
// the delete that committed before it.

// projectStorageCleanupRetryDelay is how long a removal that failed part-way
// waits before its one retry (agent pods may still be terminating and
// writing), and projectStorageCleanupTimeout bounds one background cleanup.
// Variables so tests can shorten them.
var (
	projectStorageCleanupRetryDelay = 30 * time.Second
	projectStorageCleanupTimeout    = 15 * time.Minute
)

// projectStorageCleanupAttemptHook, when set, is called with the attempt
// number (1 or 2) just before each attempt's still-deleted check of a
// project's storage cleanup. Tests only.
var projectStorageCleanupAttemptHook func(attempt int)

// startProjectStorageCleanup starts, in the background, the removal of the
// deleted project's storage:
//
//   - its project root on every shared Layout of the hub
//     (projectfs.Layout.DeleteProject; nothing on the local backend, whose
//     slug-keyed trees keep their own deleters);
//   - on a shared backend, the legacy slug-keyed trees (the share's
//     hub-projects/<slug> and the hub's ~/.scion/projects/<slug>, with
//     their .migrated-<ts> copies) whose identity markers name the project
//     (migrate.DeleteLegacyProjectTrees), never a tree matched by name
//     alone;
//   - its object-storage prefix [hubs/<hub>/]workspaces/<project ID>/, when
//     the hub has object storage.
//
// Project IDs can be reused (registering from the same checkout brings the
// same ID back), so immediately before each attempt the project must still
// be absent from the store; any other answer keeps everything. A removal
// of the project root that failed part-way is retried once after
// projectStorageCleanupRetryDelay; a refusal is not. It runs with its own
// context and timeout, never the request's, and logs every outcome as an
// audit line.
func (s *Server) startProjectStorageCleanup(project *store.Project) {
	if project == nil || project.ID == "" {
		return
	}
	projectID, slug := project.ID, project.Slug
	log := s.projectsLogger()
	layout, layoutErr := s.projectStorageLayout()
	stor := s.GetStorage()
	hubID := s.HubID()
	globalDir, _ := config.GetGlobalDir()
	retryDelay, timeout, hook := projectStorageCleanupRetryDelay, projectStorageCleanupTimeout, projectStorageCleanupAttemptHook

	shared := layoutErr == nil && (layout.Backend.Shared() || layout.SharedDirLayout().Backend.Shared())
	if layoutErr == nil && !shared && stor == nil {
		return // local backend, no object storage: nothing of the hub's to remove
	}
	// stillDeleted checks the store; attempt 0 is the object-storage step.
	stillDeleted := func(ctx context.Context, attempt int) bool {
		if hook != nil && attempt > 0 {
			hook(attempt)
		}
		_, err := s.store.GetProject(ctx, projectID)
		if errors.Is(err, store.ErrNotFound) {
			return true
		}
		if err != nil {
			log.Info("project storage cleanup: could not confirm the project is still deleted; keeping its storage",
				"project_id", projectID, "attempt", attempt, "error", err)
		} else {
			log.Info("project storage kept: the project exists again", "project_id", projectID, "attempt", attempt)
		}
		return false
	}

	s.storageCleanupWG.Add(1)
	go func() {
		defer s.storageCleanupWG.Done()
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		audit := func(ds []projectfs.Deletion) {
			projectfs.AuditAll(ctx, log, projectfs.DeleterHub, projectID, "", ds)
		}

		switch {
		case layoutErr != nil:
			audit([]projectfs.Deletion{{Tree: projectfs.TreeProjectRoot, Outcome: projectfs.OutcomeFailed,
				Err: layoutErr, Reason: "the hub's project storage layout is unavailable"}})
		case shared:
			for attempt := 1; attempt <= 2; attempt++ {
				if !stillDeleted(ctx, attempt) {
					return
				}
				ds, err := layout.DeleteProject(projectID)
				if err != nil {
					audit([]projectfs.Deletion{{Tree: projectfs.TreeProjectRoot, Outcome: projectfs.OutcomeFailed, Err: err}})
					break
				}
				retry := false
				for _, d := range ds {
					retry = retry || projectfs.Retryable(d)
				}
				if !retry || attempt == 2 {
					audit(ds)
					break
				}
				log.Warn("project storage cleanup: removing the project root failed; retrying once",
					"project_id", projectID, "retry_in", retryDelay)
				t := time.NewTimer(retryDelay)
				select {
				case <-ctx.Done():
					t.Stop()
					audit(ds)
					return
				case <-t.C:
				}
			}
			if layout.Backend.Shared() && slug != "" && ctx.Err() == nil {
				ds, err := migrate.DeleteLegacyProjectTrees(layout, globalDir, slug, projectID)
				if err != nil {
					ds = append(ds, projectfs.Deletion{Tree: projectfs.TreeLegacyHubProjects, Outcome: projectfs.OutcomeFailed, Err: err})
				}
				audit(ds)
			}
		}

		if stor != nil && ctx.Err() == nil && stillDeleted(ctx, 0) {
			prefix, err := projectfs.ProjectObjectPrefix(hubID, projectID)
			if err != nil {
				audit([]projectfs.Deletion{{Tree: projectfs.TreeObjectPrefix, Outcome: projectfs.OutcomeFailed, Err: err}})
				return
			}
			audit([]projectfs.Deletion{projectfs.DeleteObjectPrefix(ctx, stor, prefix)})
		}
	}()
}

// agentStorageCleanupTimeout bounds one background agent storage cleanup.
var agentStorageCleanupTimeout = 15 * time.Minute

// agentStorageOnDelete reports whether a finished agent delete removes the
// agent's storage on the share and in object storage: a hard delete that
// deletes the agent's files. A soft delete, or a delete that keeps the
// files, keeps it (the retained-files purge removes it later).
func agentStorageOnDelete(req store.DeletionRequestInfo) bool {
	return !req.Soft && req.DeleteFiles
}

// startAgentStorageCleanup starts, in the background, the removal of the
// deleted agent's storage the hub owns:
//
//   - its directory agents/<slug> on every shared Layout of the hub
//     (projectfs.Layout.DeleteAgent): its own workspace in the per-agent
//     modes, its homes and its secrets. The directory is keyed by slug, so
//     while another agent record of the project (deleted or not) has the
//     same slug only this agent's ID-keyed leaves home-<agent ID> and
//     secrets-<agent ID> are removed (DeleteAgentLeaves);
//   - its object-storage prefix [hubs/<hub>/]workspaces/<project ID>/<agent
//     ID>/, when the hub has object storage.
//
// Callers start it only after the agent's teardown and record change
// committed: on a hard delete that deletes the agent's files
// (agentStorageOnDelete), or when the retained-files purge removed the
// record. Every outcome is an audit line; nothing is returned.
func (s *Server) startAgentStorageCleanup(agent *store.Agent) {
	if agent == nil || agent.ID == "" || agent.ProjectID == "" || agent.Slug == "" {
		return
	}
	a := *agent
	log := s.agentLifecycleLog
	layout, layoutErr := s.projectStorageLayout()
	stor := s.GetStorage()
	hubID := s.HubID()

	s.storageCleanupWG.Add(1)
	go func() {
		defer s.storageCleanupWG.Done()
		ctx, cancel := context.WithTimeout(context.Background(), agentStorageCleanupTimeout)
		defer cancel()
		audit := func(ds []projectfs.Deletion) {
			projectfs.AuditAll(ctx, log, projectfs.DeleterHub, a.ProjectID, a.Slug, ds)
		}

		switch {
		case layoutErr != nil:
			audit([]projectfs.Deletion{{Tree: projectfs.TreeAgentRoot, Outcome: projectfs.OutcomeFailed,
				Err: layoutErr, Reason: "the hub's project storage layout is unavailable"}})
		case layout.Backend.Shared() || layout.SharedDirLayout().Backend.Shared():
			shared, err := s.agentSlugInUse(ctx, &a)
			var ds []projectfs.Deletion
			switch {
			case err != nil:
				ds = []projectfs.Deletion{{Tree: projectfs.TreeAgentRoot, Outcome: projectfs.OutcomeKept,
					Reason: "could not check whether another agent of the project has the same name: " + err.Error()}}
			case shared:
				ds, err = layout.DeleteAgentLeaves(a.ProjectID, a.Slug, a.ID)
			default:
				ds, err = layout.DeleteAgent(a.ProjectID, a.Slug)
			}
			if err != nil {
				ds = append(ds, projectfs.Deletion{Tree: projectfs.TreeAgentRoot, Outcome: projectfs.OutcomeFailed, Err: err})
			}
			audit(ds)
		}

		if stor != nil && ctx.Err() == nil {
			prefix, err := projectfs.AgentObjectPrefix(hubID, a.ProjectID, a.ID)
			if err != nil {
				audit([]projectfs.Deletion{{Tree: projectfs.TreeObjectPrefix, Outcome: projectfs.OutcomeFailed, Err: err}})
				return
			}
			audit([]projectfs.Deletion{projectfs.DeleteObjectPrefix(ctx, stor, prefix)})
		}
	}()
}

// agentSlugInUse reports whether another agent record of a's project, live
// or soft-deleted, has a's slug, so that agents/<slug> may still hold its
// files. A listing that cannot be read to the end counts as in use.
func (s *Server) agentSlugInUse(ctx context.Context, a *store.Agent) (bool, error) {
	cursor := ""
	for page := 0; page < 100; page++ {
		res, err := s.store.ListAgents(ctx, store.AgentFilter{ProjectID: a.ProjectID, IncludeDeleted: true},
			store.ListOptions{Limit: 500, Cursor: cursor, SkipTotalCount: true})
		if err != nil {
			return true, err
		}
		for _, other := range res.Items {
			if other.ID != a.ID && other.Slug == a.Slug {
				return true, nil
			}
		}
		if res.NextCursor == "" {
			return false, nil
		}
		cursor = res.NextCursor
	}
	return true, nil
}
