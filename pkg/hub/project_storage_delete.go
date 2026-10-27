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
	"log/slog"
	"path/filepath"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Project and agent storage deletion by the hub (ptone/scion#5835).
//
// The hub is the one deleter of what lives on the shared project storage
// and in its object storage: a project's tree <subpath root>/<project ID>
// (from its own mount of the share, whatever provider rows exist; brokers
// leave it alone), the legacy slug-keyed trees whose identity markers name
// the project, the project's object-storage prefix, and, of an agent, its
// home and secrets below agents/<slug>, that directory once it is empty,
// and its object-storage prefix. Every deletion is audited
// (projectfs.Audit, deleter "hub"); none ever fails or rolls back the
// delete that committed before it.

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

// forcedDeleteMinGrace is the shortest grace a forced project delete with
// agent records gives the project's tree before the maintenance pass
// removes it.
const forcedDeleteMinGrace = 24 * time.Hour

// forcedDeleteGrace is the grace of a forced project delete: the
// soft-delete retention when it is longer than forcedDeleteMinGrace,
// otherwise forcedDeleteMinGrace.
func (s *Server) forcedDeleteGrace() time.Duration {
	if retention, _ := s.softDeleteSettings(); retention > forcedDeleteMinGrace {
		return retention
	}
	return forcedDeleteMinGrace
}

// deferredDeletionReason is the reason on the audit line of a tree the
// maintenance pass removed after its tombstone's time.
const deferredDeletionReason = "deleted after the deferred-deletion time"

// startProjectStorageCleanup starts, in the background, the removal of the
// deleted project's storage:
//
//   - its project root on every shared Layout of the hub
//     (projectfs.Layout.RemoveProjectTree; nothing on the local backend, whose
//     slug-keyed trees keep their own deleters);
//   - on a shared backend, the legacy slug-keyed trees (the share's
//     hub-projects/<slug> and the hub's ~/.scion/projects/<slug>, with
//     their .migrated-<ts> copies) whose identity markers name the project
//     (migrate.DeleteLegacyProjectTrees), never a tree matched by name
//     alone;
//   - its object-storage prefix [hubs/<hub>/]workspaces/<project ID>/, when
//     the hub has object storage.
//
// A non-zero deferUntil (a forced delete that found agent records, whose
// agents may still be running) removes nothing on a shared backend: it
// writes the deletion tombstone with that time into every project root
// (projectfs.Layout.MarkDeleteAfter), and the maintenance pass
// (sweepDeferredProjectDeletions) removes the tree, the legacy trees and
// the object prefix once the time has passed.
//
// Project IDs can be reused (registering from the same checkout brings the
// same ID back), so immediately before each attempt the project must still
// be absent from the store; any other answer keeps everything. A removal
// of the project root that failed part-way is retried once after
// projectStorageCleanupRetryDelay, and when it fails again the tombstone is
// written with the current time so that the maintenance pass retries it; a
// refusal (for example the share not mounted) is not retried. It runs with
// its own context and timeout, never the request's, and logs every outcome
// as an audit line.
func (s *Server) startProjectStorageCleanup(project *store.Project, deferUntil time.Time) {
	if project == nil || project.ID == "" {
		return
	}
	projectID, slug := project.ID, project.Slug
	layout, layoutErr := s.projectStorageLayout()
	shared := layoutErr == nil && (layout.Backend.Shared() || layout.SharedDirLayout().Backend.Shared())
	if layoutErr == nil && !shared && s.GetStorage() == nil {
		return // local backend, no object storage: nothing of the hub's to remove
	}
	log := s.projectsLogger()
	timeout := projectStorageCleanupTimeout

	s.storageCleanupWG.Add(1)
	go func() {
		defer s.storageCleanupWG.Done()
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if layoutErr != nil {
			s.auditStorage(ctx, log, projectID, "", []projectfs.Deletion{{
				Tree: projectfs.TreeProjectRoot, Outcome: projectfs.OutcomeFailed,
				Err: layoutErr, Reason: "the hub's project storage layout is unavailable"}})
			return
		}
		if shared && !deferUntil.IsZero() {
			if !s.projectStillDeleted(ctx, projectID, 1) {
				return
			}
			ds, err := layout.MarkDeleteAfter(projectID, deferUntil)
			if err != nil {
				ds = append(ds, projectfs.Deletion{Tree: projectfs.TreeProjectRoot, Outcome: projectfs.OutcomeFailed, Err: err})
			}
			s.auditStorage(ctx, log, projectID, "", ds)
			return
		}
		reason := ""
		if !deferUntil.IsZero() {
			// A forced delete with agent records on a non-shared backend:
			// there is no project tree on a share to defer, and the object
			// prefix holds only transfer staging (an in-flight transfer
			// fails cleanly), so it is deleted now; no broker workspace is
			// touched (forced-delete ruling, AC-F1b).
			reason = forcedNonSharedPrefixReason
		}
		s.deleteProjectStorage(ctx, layout, projectID, slug, reason, true)
	}()
}

// forcedNonSharedPrefixReason is the reason on the audit line of the object
// prefix a forced delete with agent records removes at once on a non-shared
// backend.
const forcedNonSharedPrefixReason = "forced delete on a non-shared backend: transfer staging removed now; no broker workspace touched"

// projectStillDeleted reports whether projectID is absent from the store
// (a project that exists again keeps its storage). attempt numbers the
// check for projectStorageCleanupAttemptHook; 0 does not call it.
func (s *Server) projectStillDeleted(ctx context.Context, projectID string, attempt int) bool {
	if hook := projectStorageCleanupAttemptHook; hook != nil && attempt > 0 {
		hook(attempt)
	}
	log := s.projectsLogger()
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

// deleteProjectStorage removes projectID's project root on every shared
// Layout of layout (retried once, as startProjectStorageCleanup says), then
// the legacy slug trees (when slug is known) and the object-storage
// prefix, auditing each with reason. retryLater writes the tombstone with
// the current time when the root removal still fails part-way after its
// retry, so the maintenance pass tries again.
func (s *Server) deleteProjectStorage(ctx context.Context, layout projectfs.Layout, projectID, slug, reason string, retryLater bool) {
	log := s.projectsLogger()
	audit := func(ds []projectfs.Deletion) {
		for i := range ds {
			if reason != "" && ds[i].Outcome == projectfs.OutcomeDeleted && ds[i].Reason == "" {
				ds[i].Reason = reason
			}
		}
		s.auditStorage(ctx, log, projectID, "", ds)
	}
	shared := layout.Backend.Shared() || layout.SharedDirLayout().Backend.Shared()
	if shared {
		for attempt := 1; attempt <= 2; attempt++ {
			if !s.projectStillDeleted(ctx, projectID, attempt) {
				return
			}
			ds, err := layout.RemoveProjectTree(projectID)
			if err != nil {
				audit([]projectfs.Deletion{{Tree: projectfs.TreeProjectRoot, Outcome: projectfs.OutcomeFailed, Err: err}})
				break
			}
			retry := false
			for _, d := range ds {
				retry = retry || projectfs.Retryable(d)
			}
			if !retry {
				audit(ds)
				break
			}
			if attempt == 2 {
				audit(ds)
				if retryLater {
					// The tombstone (written anew: a part-way removal may
					// have taken it) hands the tree to the maintenance
					// pass, which retries it.
					marks, err := layout.MarkDeleteAfter(projectID, time.Now())
					if err != nil {
						marks = append(marks, projectfs.Deletion{Tree: projectfs.TreeProjectRoot, Outcome: projectfs.OutcomeFailed,
							Err: err, Reason: "the retry by the maintenance pass could not be recorded"})
					}
					for i := range marks {
						if marks[i].Outcome == projectfs.OutcomeKept {
							marks[i].Reason = "removal failed; the maintenance pass retries it"
						}
					}
					audit(marks)
				}
				break
			}
			log.Warn("project storage cleanup: removing the project root failed; retrying once",
				"project_id", projectID, "retry_in", projectStorageCleanupRetryDelay)
			t := time.NewTimer(projectStorageCleanupRetryDelay)
			select {
			case <-ctx.Done():
				t.Stop()
				audit(ds)
				return
			case <-t.C:
			}
		}
		if layout.Backend.Shared() && slug != "" && ctx.Err() == nil {
			globalDir, _ := config.GetGlobalDir()
			ds, err := deleteLegacyProjectTrees(layout, globalDir, slug, projectID)
			if err != nil {
				ds = append(ds, projectfs.Deletion{Tree: projectfs.TreeLegacyHubProjects, Outcome: projectfs.OutcomeFailed, Err: err})
			}
			audit(ds)
		}
	}

	if stor := s.GetStorage(); stor != nil && ctx.Err() == nil && s.projectStillDeleted(ctx, projectID, 0) {
		prefix, err := projectfs.ProjectObjectPrefix(s.HubID(), projectID)
		if err != nil {
			audit([]projectfs.Deletion{{Tree: projectfs.TreeObjectPrefix, Outcome: projectfs.OutcomeFailed, Err: err}})
			return
		}
		audit([]projectfs.Deletion{projectfs.DeleteObjectPrefix(ctx, stor, prefix)})
	}
}

// sweepDeferredProjectDeletions is the maintenance pass of deferred
// project deletions: it lists the project roots that carry a deletion
// tombstone (projectfs.Layout.DeferredDeletions) and, for each:
//
//   - when its project exists again, clears the tombstone and keeps the
//     tree;
//   - when the tombstone's time has passed, removes the project's storage
//     as a project delete does (deleteProjectStorage, the slug read from
//     the tree's project configuration), audited as deleted after the
//     deferred-deletion time;
//   - otherwise leaves it for a later pass.
//
// A listing that fails (the share not mounted) is one ERROR audit line per
// pass and nothing else. It runs on the purge tick; every hub replica may
// run it, and it is idempotent.
func (s *Server) sweepDeferredProjectDeletions(ctx context.Context) {
	layout, err := s.projectStorageLayout()
	if err != nil || (!layout.Backend.Shared() && !layout.SharedDirLayout().Backend.Shared()) {
		return
	}
	log := s.projectsLogger()
	due, err := layout.DeferredDeletions()
	// A missing tree on a hub with no project yet is expected, not a
	// missing mount: nothing to report then.
	expectedEmpty := err != nil && errors.Is(err, projectfs.ErrNoProjectTree) && !s.hubHasProjects(ctx)
	if err != nil && !expectedEmpty {
		s.auditStorage(ctx, log, "", "", []projectfs.Deletion{{
			Tree: projectfs.TreeProjectRoot, Path: layout.HostBase, Outcome: projectfs.OutcomeFailed,
			Err: err, Reason: "deferred project deletions could not be listed"}})
	}
	now := time.Now()
	seen := map[string]bool{}
	for _, d := range due {
		if seen[d.ProjectID] || ctx.Err() != nil {
			continue
		}
		seen[d.ProjectID] = true
		_, gerr := s.store.GetProject(ctx, d.ProjectID)
		switch {
		case gerr == nil:
			if cerr := layout.ClearDeleteAfter(d.ProjectID); cerr != nil {
				// Audited (redacted: the hub's logs carry no host path); the
				// next pass tries again.
				s.auditStorage(ctx, log, d.ProjectID, "", []projectfs.Deletion{{
					Tree: projectfs.TreeProjectRoot, Path: d.Path, Outcome: projectfs.OutcomeFailed, Err: cerr,
					Reason: "the project exists again, but its deferred-deletion tombstone could not be cleared"}})
				continue
			}
			s.auditStorage(ctx, log, d.ProjectID, "", []projectfs.Deletion{{
				Tree: projectfs.TreeProjectRoot, Path: d.Path, Outcome: projectfs.OutcomeKept,
				Reason: "the project exists again: deferred deletion cancelled"}})
			continue
		case !errors.Is(gerr, store.ErrNotFound):
			log.Info("deferred project deletion: could not confirm the project is still deleted; keeping its tree",
				"project_id", d.ProjectID, "error", gerr)
			continue
		}
		if !d.After.IsZero() && now.Before(d.After) {
			continue
		}
		// The slug only selects legacy slug trees, each of which is removed
		// only when its identity markers name the project, so a slug read
		// through a symlink cannot widen what is removed; .scion is in no
		// agent Mount.
		slug, _ := config.ReadProjectSlug(filepath.Join(d.Path, config.DotScion))
		// retryLater: a removal that fails part-way again keeps (re-marks)
		// the tombstone, so a later pass retries it.
		s.deleteProjectStorage(ctx, layout, d.ProjectID, slug, deferredDeletionReason, true)
	}
}

// agentStorageCleanupTimeout bounds one background agent storage cleanup.
var agentStorageCleanupTimeout = 15 * time.Minute

// agentStorageCleanupObserver, when set, is called at the start of every
// startAgentStorageCleanup. Tests only.
var agentStorageCleanupObserver func(a *store.Agent, teardownConfirmed bool)

// agentStorageOnDelete reports whether a finished agent delete removes the
// agent's storage on the share and in object storage: a hard delete that
// deletes the agent's files. A soft delete, or a delete that keeps the
// files, keeps it (the retained-files purge removes it later).
func agentStorageOnDelete(req store.DeletionRequestInfo) bool {
	return !req.Soft && req.DeleteFiles
}

// agentRootOtherFilesReason is the reason of an agent directory kept after
// a confirmed broker teardown: it still has entries the hub does not own.
const agentRootOtherFilesReason = "the agent directory still has entries the hub does not own (other agent files remain)"

// startAgentStorageCleanup starts, in the background, the removal of the
// deleted agent's storage the hub owns:
//
//   - on every shared Layout of the hub, its home-<agent ID> and
//     secrets-<agent ID> below agents/<slug>, and then agents/<slug>
//     itself only when it is empty (projectfs.Layout.RemoveAgentStorage). The
//     agent's own workspace or worktree is never removed here: the broker
//     removes it under the provisioning lock with its git bookkeeping.
//     A directory that is not empty is kept, audited as workspace pending
//     broker removal when the broker teardown was not confirmed
//     (teardownConfirmed false: a forced delete after a broker failure, an
//     unreachable broker, no broker), and as other agent files remaining
//     otherwise;
//   - its object-storage prefix [hubs/<hub>/]workspaces/<project ID>/<agent
//     ID>/, when the hub has object storage.
//
// Callers start it only after the agent's teardown and record change
// committed: on a hard delete that deletes the agent's files
// (agentStorageOnDelete), or when the retained-files purge removed the
// record. Every outcome is an audit line; nothing is returned.
func (s *Server) startAgentStorageCleanup(agent *store.Agent, teardownConfirmed bool) {
	if agent == nil || agent.ID == "" || agent.ProjectID == "" || agent.Slug == "" {
		return
	}
	if obs := agentStorageCleanupObserver; obs != nil {
		obs(agent, teardownConfirmed)
	}
	a := *agent
	log := s.agentLifecycleLog
	layout, layoutErr := s.projectStorageLayout()
	stor := s.GetStorage()
	hubID := s.HubID()
	shared := layoutErr == nil && (layout.Backend.Shared() || layout.SharedDirLayout().Backend.Shared())
	if layoutErr == nil && !shared && stor == nil {
		return
	}

	s.storageCleanupWG.Add(1)
	go func() {
		defer s.storageCleanupWG.Done()
		ctx, cancel := context.WithTimeout(context.Background(), agentStorageCleanupTimeout)
		defer cancel()
		audit := func(ds []projectfs.Deletion) {
			s.auditStorage(ctx, log, a.ProjectID, a.Slug, ds)
		}

		switch {
		case layoutErr != nil:
			audit([]projectfs.Deletion{{Tree: projectfs.TreeAgentRoot, Outcome: projectfs.OutcomeFailed,
				Err: layoutErr, Reason: "the hub's project storage layout is unavailable"}})
		case shared:
			ds, err := layout.RemoveAgentStorage(a.ProjectID, a.Slug, a.ID)
			if err != nil {
				ds = append(ds, projectfs.Deletion{Tree: projectfs.TreeAgentRoot, Outcome: projectfs.OutcomeFailed, Err: err})
			}
			for i := range ds {
				if ds[i].Tree == projectfs.TreeAgentRoot && ds[i].Outcome == projectfs.OutcomeKept && teardownConfirmed {
					ds[i].Reason = agentRootOtherFilesReason
				}
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

// hubHasProjects reports whether the store has at least one project. A
// shared backend's project tree appears with the first project, so a mount
// point without it is expected (not a missing mount) on a hub with no
// projects. A store error counts as having projects (the check is not
// silenced).
func (s *Server) hubHasProjects(ctx context.Context) bool {
	res, err := s.store.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: 1})
	return err != nil || len(res.Items) > 0
}

// auditStorage writes the hub's audit lines of storage deletions, redacted
// for the hub's logs, which carry no host filesystem path
// (projectfs.Redact): paths relative to the storage roots, errors by class.
func (s *Server) auditStorage(ctx context.Context, log *slog.Logger, projectID, agent string, ds []projectfs.Deletion) {
	var bases []string
	if layout, err := s.projectStorageLayout(); err == nil {
		bases = append(bases, layout.HostBase, layout.SharedDirLayout().HostBase)
	}
	if globalDir, err := config.GetGlobalDir(); err == nil {
		bases = append(bases, globalDir)
	}
	for _, d := range ds {
		projectfs.Audit(ctx, log, projectfs.DeleterHub, projectID, agent, projectfs.Redact(d, bases...))
	}
}
