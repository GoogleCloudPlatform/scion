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

//go:build !no_sqlite

package cmd

import (
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReportEmptyPerAgentProjects(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	affected := createTestProject(t, ctx, s, "legacy-nongit-per-agent", "", map[string]string{store.LabelWorkspaceMode: store.WorkspaceModePerAgent})
	createTestProject(t, ctx, s, "nongit-shared", "", map[string]string{store.LabelWorkspaceMode: store.WorkspaceModeShared})
	createTestProject(t, ctx, s, "nongit-unlabelled", "", nil)
	createTestProject(t, ctx, s, "remote-per-agent", "https://github.com/example/repo.git", map[string]string{store.LabelWorkspaceMode: store.WorkspaceModePerAgent})

	buf, restore := captureSlog(t)
	defer restore()

	reportEmptyPerAgentProjects(ctx, s)

	out := buf.String()
	assert.Contains(t, out, affected.ID)
	assert.Contains(t, out, "count=1")
	assert.NotContains(t, out, "nongit-shared")
	assert.NotContains(t, out, "remote-per-agent")

	// Read-only: the label is left unchanged.
	got, err := s.GetProject(ctx, affected.ID)
	require.NoError(t, err)
	assert.Equal(t, store.WorkspaceModePerAgent, got.Labels[store.LabelWorkspaceMode])

	done, err := IsMigrationComplete(ctx, s, MigrationEmptyPerAgentLegacyReport)
	require.NoError(t, err)
	assert.True(t, done, "completion marker written after the pass")
}

func TestReportEmptyPerAgentProjects_SilentWhenNoneAffected(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	createTestProject(t, ctx, s, "nongit-unlabelled", "", nil)

	buf, restore := captureSlog(t)
	defer restore()

	reportEmptyPerAgentProjects(ctx, s)

	assert.NotContains(t, buf.String(), "level=WARN")
	done, err := IsMigrationComplete(ctx, s, MigrationEmptyPerAgentLegacyReport)
	require.NoError(t, err)
	assert.True(t, done, "an empty pass is still a completed pass")
}

// TestReportEmptyPerAgentProjects_OneShot: after the first pass, projects
// created as empty-per-agent on purpose are not reported (review #2717 r2
// finding 1).
func TestReportEmptyPerAgentProjects_OneShot(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	legacy := createTestProject(t, ctx, s, "legacy-nongit-per-agent", "", map[string]string{store.LabelWorkspaceMode: store.WorkspaceModePerAgent})

	buf, restore := captureSlog(t)
	defer restore()

	reportEmptyPerAgentProjects(ctx, s)
	require.Contains(t, buf.String(), legacy.ID, "first boot reports the legacy project")
	done, err := IsMigrationComplete(ctx, s, MigrationEmptyPerAgentLegacyReport)
	require.NoError(t, err)
	require.True(t, done)

	// A project deliberately created as empty-per-agent after the upgrade.
	deliberate := createTestProject(t, ctx, s, "new-empty-per-agent", "", map[string]string{store.LabelWorkspaceMode: store.WorkspaceModePerAgent})
	buf.Reset()

	reportEmptyPerAgentProjects(ctx, s)
	assert.NotContains(t, buf.String(), deliberate.ID)
	assert.NotContains(t, buf.String(), legacy.ID)
	assert.NotContains(t, buf.String(), "level=WARN")
}
