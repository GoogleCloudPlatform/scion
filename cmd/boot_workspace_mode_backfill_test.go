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
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createTestProject creates a project with the given git remote and labels.
// Returns the created project.
func createTestProject(t *testing.T, ctx context.Context, s store.Store, name, gitRemote string, labels map[string]string) store.Project {
	t.Helper()
	p := &store.Project{
		ID:        uuid.NewString(),
		Name:      name,
		Slug:      uuid.NewString(), // unique slug
		GitRemote: gitRemote,
		Labels:    labels,
	}
	err := s.CreateProject(ctx, p)
	require.NoError(t, err, "failed to create project %s", name)
	return *p
}

// ---------------------------------------------------------------------------
// Scenario 1: git project with no label gets stamped "per-agent"
// ---------------------------------------------------------------------------

func TestWorkspaceModeBackfill_GitProjectNoLabel(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Create a git project with no workspace-mode label (pre-fix scenario).
	project := createTestProject(t, ctx, s, "pre-fix-project", "https://github.com/example/repo.git", nil)

	buf, restore := captureSlog(t)
	defer restore()

	runWorkspaceModeBackfill(ctx, s)

	logOutput := buf.String()

	// Verify the project was stamped.
	got, err := s.GetProject(ctx, project.ID)
	require.NoError(t, err)
	assert.Equal(t, store.WorkspaceModePerAgent, got.Labels[store.LabelWorkspaceMode],
		"git project without workspace-mode label should be stamped per-agent")

	// Verify logs.
	assert.Contains(t, logOutput, "count=1")
	assert.Contains(t, logOutput, "stamped=1")
	assert.Contains(t, logOutput, "pass completed")

	// Verify marker was written.
	done, err := IsMigrationComplete(ctx, s, MigrationWorkspaceModeLabel)
	require.NoError(t, err)
	assert.True(t, done, "migration marker should be written after successful pass")
}

// ---------------------------------------------------------------------------
// Scenario 2: project that already has a label is left untouched
// ---------------------------------------------------------------------------

func TestWorkspaceModeBackfill_ExistingLabelUntouched(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Create a git project that already has the workspace-mode label (post-fix).
	project := createTestProject(t, ctx, s, "post-fix-project", "https://github.com/example/repo.git",
		map[string]string{store.LabelWorkspaceMode: store.WorkspaceModeShared})

	buf, restore := captureSlog(t)
	defer restore()

	runWorkspaceModeBackfill(ctx, s)

	logOutput := buf.String()

	// Verify the project was NOT changed — still "shared".
	got, err := s.GetProject(ctx, project.ID)
	require.NoError(t, err)
	assert.Equal(t, store.WorkspaceModeShared, got.Labels[store.LabelWorkspaceMode],
		"project with existing label should not be modified")

	// No candidates found.
	assert.Contains(t, logOutput, "count=0")

	// Marker should still be written (empty pass is a completed pass).
	done, err := IsMigrationComplete(ctx, s, MigrationWorkspaceModeLabel)
	require.NoError(t, err)
	assert.True(t, done, "marker should be written for empty pass")
}

// ---------------------------------------------------------------------------
// Scenario 3: non-git project (no GitRemote) is left untouched
// ---------------------------------------------------------------------------

func TestWorkspaceModeBackfill_NonGitProjectUntouched(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Create a non-git project (no GitRemote).
	project := createTestProject(t, ctx, s, "plain-project", "", nil)

	buf, restore := captureSlog(t)
	defer restore()

	runWorkspaceModeBackfill(ctx, s)

	logOutput := buf.String()

	// Verify the project was NOT changed — still no label.
	got, err := s.GetProject(ctx, project.ID)
	require.NoError(t, err)
	assert.Empty(t, got.Labels[store.LabelWorkspaceMode],
		"non-git project should not receive workspace-mode label")

	// No candidates found.
	assert.Contains(t, logOutput, "count=0")

	// Marker should still be written.
	done, err := IsMigrationComplete(ctx, s, MigrationWorkspaceModeLabel)
	require.NoError(t, err)
	assert.True(t, done, "marker should be written for empty pass")
}

// ---------------------------------------------------------------------------
// Mixed scenario: all three cases in one pass
// ---------------------------------------------------------------------------

func TestWorkspaceModeBackfill_MixedProjects(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// (a) Git project with no label — should be stamped.
	preFix := createTestProject(t, ctx, s, "pre-fix", "https://github.com/example/pre-fix.git", nil)

	// (b) Git project with existing label — should be left alone.
	postFix := createTestProject(t, ctx, s, "post-fix", "https://github.com/example/post-fix.git",
		map[string]string{store.LabelWorkspaceMode: store.WorkspaceModeWorktreePerAgent})

	// (c) Non-git project — should be left alone.
	plain := createTestProject(t, ctx, s, "plain", "", nil)

	buf, restore := captureSlog(t)
	defer restore()

	runWorkspaceModeBackfill(ctx, s)

	logOutput := buf.String()

	// (a) Pre-fix project was stamped.
	gotPreFix, err := s.GetProject(ctx, preFix.ID)
	require.NoError(t, err)
	assert.Equal(t, store.WorkspaceModePerAgent, gotPreFix.Labels[store.LabelWorkspaceMode],
		"pre-fix git project should be stamped per-agent")

	// (b) Post-fix project untouched.
	gotPostFix, err := s.GetProject(ctx, postFix.ID)
	require.NoError(t, err)
	assert.Equal(t, store.WorkspaceModeWorktreePerAgent, gotPostFix.Labels[store.LabelWorkspaceMode],
		"post-fix project should keep its existing label")

	// (c) Plain project untouched.
	gotPlain, err := s.GetProject(ctx, plain.ID)
	require.NoError(t, err)
	assert.Empty(t, gotPlain.Labels[store.LabelWorkspaceMode],
		"non-git project should not receive workspace-mode label")

	// Only 1 candidate, 1 stamped.
	assert.Contains(t, logOutput, "count=1")
	assert.Contains(t, logOutput, "stamped=1")

	// Marker written.
	done, err := IsMigrationComplete(ctx, s, MigrationWorkspaceModeLabel)
	require.NoError(t, err)
	assert.True(t, done)
}

// ---------------------------------------------------------------------------
// Idempotence: second boot is a no-op
// ---------------------------------------------------------------------------

func TestWorkspaceModeBackfill_Idempotent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Create a pre-fix project.
	createTestProject(t, ctx, s, "pre-fix", "https://github.com/example/repo.git", nil)

	// First boot: stamps the label.
	runWorkspaceModeBackfill(ctx, s)

	// Second boot: should skip entirely.
	buf, restore := captureSlog(t)
	defer restore()

	runWorkspaceModeBackfill(ctx, s)

	logOutput := buf.String()
	assert.Contains(t, logOutput, "already complete, skipping")
}

// ---------------------------------------------------------------------------
// No projects at all: marker still written
// ---------------------------------------------------------------------------

func TestWorkspaceModeBackfill_NoProjects(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	buf, restore := captureSlog(t)
	defer restore()

	runWorkspaceModeBackfill(ctx, s)

	logOutput := buf.String()
	assert.Contains(t, logOutput, "count=0")

	done, err := IsMigrationComplete(ctx, s, MigrationWorkspaceModeLabel)
	require.NoError(t, err)
	assert.True(t, done, "marker should be written for empty pass")
}
