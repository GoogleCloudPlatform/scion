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

// reportEmptyPerAgentProjects logs, once, the legacy non-git projects whose
// stored scion.dev/workspace-mode label now resolves to the empty-per-agent
// workspace mode (design #2703).
//
// Background: before #2703, the hub stored raw labels verbatim on non-git
// project create and PATCH, and the label had no effect for non-git
// projects. It now means "each agent gets an empty, private workspace
// directory", and agents of such projects need a broker that advertises the
// emptyPerAgentWorkspace capability (starts otherwise fail with 412). Hub
// clients never sent the label for non-git projects, so this should be
// rare; the report makes any affected project visible to operators.
//
// Legacy-only by construction: boot data migrations run before the hub
// serves any request, so every project found on the first pass predates
// empty-per-agent support. The completion marker then stops later boots
// from reporting projects created as empty-per-agent on purpose.
//
// The pass is read-only. A listing failure is a run-level failure: the
// marker is left unwritten and the next boot retries.
func reportEmptyPerAgentProjects(ctx context.Context, s store.Store) {
	done, err := IsMigrationComplete(ctx, s, MigrationEmptyPerAgentLegacyReport)
	if err != nil {
		slog.Error("Empty-per-agent legacy report: failed to check completion marker; will attempt report",
			"error", err)
	} else if done {
		slog.Debug("Empty-per-agent legacy report: already complete, skipping")
		return
	}

	affected, err := findEmptyPerAgentProjects(ctx, s)
	if err != nil {
		slog.Error("Empty-per-agent legacy report: failed to list projects; will retry next boot", "error", err)
		return
	}
	for _, p := range affected {
		slog.Warn("Empty-per-agent legacy report: non-git project now resolves to the empty-per-agent workspace mode; its agents need a broker with the emptyPerAgentWorkspace capability",
			"project_id", p.ID,
			"project_name", p.Name,
			"label", p.Labels[store.LabelWorkspaceMode],
		)
	}
	if len(affected) > 0 {
		slog.Warn("Empty-per-agent legacy report: pass completed", "count", len(affected))
	}

	if markErr := MarkMigrationComplete(ctx, s, MigrationEmptyPerAgentLegacyReport, 0); markErr != nil {
		slog.Error("Empty-per-agent legacy report: failed to write completion marker; will retry next boot",
			"error", markErr)
	}
}

// findEmptyPerAgentProjects returns all projects that resolve to the
// empty-per-agent workspace mode.
func findEmptyPerAgentProjects(ctx context.Context, s store.Store) ([]store.Project, error) {
	var out []store.Project
	cursor := ""
	for {
		result, err := s.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: 500, Cursor: cursor})
		if err != nil {
			return nil, fmt.Errorf("listing projects: %w", err)
		}
		if result == nil {
			return nil, fmt.Errorf("listing projects: received nil result")
		}
		for i := range result.Items {
			if result.Items[i].IsEmptyPerAgent() {
				out = append(out, result.Items[i])
			}
		}
		if result.NextCursor == "" {
			break
		}
		cursor = result.NextCursor
	}
	return out, nil
}
