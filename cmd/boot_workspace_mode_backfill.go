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

package cmd

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// runWorkspaceModeBackfill stamps the scion.dev/workspace-mode label on
// pre-fix git projects that are missing it.
//
// Background: commit b5e32b6c (2026-08-31) fixed project creation to persist
// the workspace-mode label, but projects created before that commit have no
// label. When the label is absent, ResolveWorkspaceSharingMode("") returns
// SharingModeSharedPlain regardless of the project's actual intended mode.
// This migration retroactively stamps "per-agent" on all unlabeled git
// projects, which is the correct default intent for git projects that predate
// explicit mode selection (see findings doc and the original fix's test
// naming convention).
//
// Scope: only projects with a non-empty GitRemote and no existing
// scion.dev/workspace-mode label. Non-git projects (no GitRemote) are not
// workspace-mode projects and are left untouched. Projects that already have
// the label (created after the fix, or manually stamped) are left untouched.
//
// M-1' semantics (same as runGroupRefRepair): a completion marker records
// that a full pass completed without a run-level failure. There are no
// expected row-level refusals in this migration (every qualifying project
// gets the same label stamped), but an UpdateProject failure is treated as
// a run-level failure that aborts the pass and leaves the marker unwritten
// for retry on next boot.
func runWorkspaceModeBackfill(ctx context.Context, s store.Store) {
	// Fast path: already complete.
	done, err := IsMigrationComplete(ctx, s, MigrationWorkspaceModeLabel)
	if err != nil {
		slog.Error("Workspace mode backfill: failed to check completion marker; will attempt migration",
			"error", err)
	} else if done {
		slog.Debug("Workspace mode backfill: already complete, skipping")
		return
	}

	slog.Info("Workspace mode backfill: starting")

	// Enumerate all projects and find those needing the label.
	candidates, err := findWorkspaceModeCandidates(ctx, s)
	if err != nil {
		slog.Error("Workspace mode backfill: failed to list projects; will retry next boot",
			"error", err)
		return
	}

	slog.Info("Workspace mode backfill: found candidates",
		"count", len(candidates))

	if len(candidates) == 0 {
		// No candidates — an empty pass is still a completed pass.
		if markErr := MarkMigrationComplete(ctx, s, MigrationWorkspaceModeLabel, 0); markErr != nil {
			slog.Error("Workspace mode backfill: failed to write completion marker; will retry next boot",
				"error", markErr)
		}
		return
	}

	stamped := 0
	for _, project := range candidates {
		if project.Labels == nil {
			project.Labels = make(map[string]string)
		}
		project.Labels[store.LabelWorkspaceMode] = store.WorkspaceModePerAgent

		if updateErr := s.UpdateProject(ctx, &project); updateErr != nil {
			// Run-level failure: abort and retry next boot.
			slog.Error("Workspace mode backfill: failed to update project; will retry next boot",
				"project_id", project.ID,
				"project_name", project.Name,
				"error", updateErr)
			return
		}

		stamped++
		slog.Info("Workspace mode backfill: stamped project",
			"project_id", project.ID,
			"project_name", project.Name,
			"label", store.WorkspaceModePerAgent,
		)
	}

	// Pass completed. Write the marker.
	slog.Info("Workspace mode backfill: pass completed",
		"stamped", stamped,
	)

	if markErr := MarkMigrationComplete(ctx, s, MigrationWorkspaceModeLabel, 0); markErr != nil {
		slog.Error("Workspace mode backfill: failed to write completion marker; will retry next boot",
			"error", markErr)
	}
}

// findWorkspaceModeCandidates returns all projects that have a non-empty
// GitRemote and no scion.dev/workspace-mode label. These are the pre-fix
// git projects that need the label backfilled.
func findWorkspaceModeCandidates(ctx context.Context, s store.Store) ([]store.Project, error) {
	var candidates []store.Project
	cursor := ""
	for {
		result, err := s.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: 500, Cursor: cursor})
		if err != nil {
			return nil, fmt.Errorf("listing projects: %w", err)
		}
		for _, p := range result.Items {
			if p.GitRemote != "" && p.Labels[store.LabelWorkspaceMode] == "" {
				candidates = append(candidates, p)
			}
		}
		if result.NextCursor == "" {
			break
		}
		cursor = result.NextCursor
	}
	return candidates, nil
}
