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

package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Tests for the server-owned scion.dev/workspace-mode label (design #2703
// §2.4 validation matrix) and the empty-per-agent capability gate helpers.

const testGitRemote = "github.com/test/ws-mode"

// TestCreateProject_WorkspaceModeMatrix covers every createProject cell of the
// §2.4 matrix (git and non-git columns), including the raw-labels row.
func TestCreateProject_WorkspaceModeMatrix(t *testing.T) {
	tests := []struct {
		name      string
		gitRemote string
		mode      string
		rawLabel  *string // raw labels[workspace-mode], nil = absent
		wantCode  int
		wantLabel string // "" = label absent
		wantMode  store.WorkspaceSharingMode
	}{
		{name: "git/none", gitRemote: testGitRemote, wantCode: http.StatusCreated, wantMode: store.SharingModeSharedPlain},
		// git/shared is covered by TestCreateProject_SharedWorkspace* (it clones).
		{name: "git/shared raw conflict", gitRemote: testGitRemote, mode: "shared", rawLabel: strPtr("per-agent"), wantCode: http.StatusBadRequest},
		{name: "git/per-agent", gitRemote: testGitRemote, mode: "per-agent", wantCode: http.StatusCreated, wantLabel: "per-agent", wantMode: store.SharingModeClonePerAgent},
		{name: "git/worktree", gitRemote: testGitRemote, mode: "worktree-per-agent", wantCode: http.StatusCreated, wantLabel: "worktree-per-agent", wantMode: store.SharingModeWorktreePerAgent},
		{name: "git/unknown", gitRemote: testGitRemote, mode: "bogus", wantCode: http.StatusBadRequest},

		{name: "nongit/none", wantCode: http.StatusCreated, wantMode: store.SharingModeSharedPlain},
		{name: "nongit/shared", mode: "shared", wantCode: http.StatusCreated, wantLabel: "shared", wantMode: store.SharingModeSharedPlain},
		{name: "nongit/per-agent", mode: "per-agent", wantCode: http.StatusCreated, wantLabel: "per-agent", wantMode: store.SharingModeEmptyPerAgent},
		{name: "nongit/worktree", mode: "worktree-per-agent", wantCode: http.StatusBadRequest},
		{name: "nongit/unknown", mode: "bogus", wantCode: http.StatusBadRequest},
		{name: "nongit/canonical-name-not-accepted", mode: "empty-per-agent", wantCode: http.StatusBadRequest},

		// Raw labels row.
		{name: "raw matches mode", mode: "per-agent", rawLabel: strPtr("per-agent"), wantCode: http.StatusCreated, wantLabel: "per-agent", wantMode: store.SharingModeEmptyPerAgent},
		{name: "raw conflicts with mode", mode: "shared", rawLabel: strPtr("per-agent"), wantCode: http.StatusBadRequest},
		{name: "raw without mode is stripped (non-git)", rawLabel: strPtr("per-agent"), wantCode: http.StatusCreated, wantMode: store.SharingModeSharedPlain},
		{name: "raw without mode is stripped (git)", gitRemote: testGitRemote, rawLabel: strPtr("worktree-per-agent"), wantCode: http.StatusCreated, wantMode: store.SharingModeSharedPlain},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, s := testServer(t)
			body := CreateProjectRequest{
				Name:          "WS Mode " + tt.name,
				GitRemote:     tt.gitRemote,
				WorkspaceMode: tt.mode,
				Labels:        map[string]string{"team": "a"},
			}
			if tt.rawLabel != nil {
				body.Labels[store.LabelWorkspaceMode] = *tt.rawLabel
			}
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects", body)
			require.Equal(t, tt.wantCode, rec.Code, "body: %s", rec.Body.String())
			if tt.wantCode != http.StatusCreated {
				return
			}
			var project store.Project
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&project))
			got, err := s.GetProject(context.Background(), project.ID)
			require.NoError(t, err)
			label, has := got.Labels[store.LabelWorkspaceMode]
			if tt.wantLabel == "" {
				assert.False(t, has, "workspace-mode label should be absent, got %q", label)
			} else {
				assert.Equal(t, tt.wantLabel, label)
			}
			assert.Equal(t, "a", got.Labels["team"], "other labels are kept")
			assert.Equal(t, tt.wantMode, got.SharingMode())
		})
	}
}

// TestRegisterProject_WorkspaceModeLabel covers the register column: linked
// non-git projects reject any mode; git projects keep known values; unknown
// values are rejected for both.
func TestRegisterProject_WorkspaceModeLabel(t *testing.T) {
	tests := []struct {
		name      string
		gitRemote string
		label     string
		wantCode  int
	}{
		{"nongit/shared", "", "shared", http.StatusBadRequest},
		{"nongit/per-agent", "", "per-agent", http.StatusBadRequest},
		{"nongit/worktree", "", "worktree-per-agent", http.StatusBadRequest},
		{"nongit/unknown", "", "bogus", http.StatusBadRequest},
		{"git/shared", testGitRemote, "shared", http.StatusOK},
		{"git/per-agent", testGitRemote, "per-agent", http.StatusOK},
		{"git/worktree", testGitRemote, "worktree-per-agent", http.StatusOK},
		{"git/unknown", testGitRemote, "bogus", http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, s := testServer(t)
			body := RegisterProjectRequest{
				Name:      "Reg " + tt.name,
				GitRemote: tt.gitRemote,
				Labels:    map[string]string{store.LabelWorkspaceMode: tt.label},
			}
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/register", body)
			require.Equal(t, tt.wantCode, rec.Code, "body: %s", rec.Body.String())
			if tt.wantCode != http.StatusOK {
				return
			}
			var resp RegisterProjectResponse
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
			got, err := s.GetProject(context.Background(), resp.Project.ID)
			require.NoError(t, err)
			assert.Equal(t, tt.label, got.Labels[store.LabelWorkspaceMode])
		})
	}

	t.Run("nongit/no label", func(t *testing.T) {
		srv, _ := testServer(t)
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/register",
			RegisterProjectRequest{Name: "Reg plain", Labels: map[string]string{"team": "a"}})
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	})
}

// TestUpdateProject_WorkspaceModeLabelProtected covers the PATCH column: the
// label is preserved when the replacement map omits it, accepted when equal,
// and rejected when it differs.
func TestUpdateProject_WorkspaceModeLabelProtected(t *testing.T) {
	newProject := func(t *testing.T, s store.Store, labels map[string]string) *store.Project {
		t.Helper()
		p := &store.Project{ID: api.NewUUID(), Name: "Patch WS", Slug: "patch-ws-" + api.NewShortID(), Labels: labels}
		require.NoError(t, s.CreateProject(context.Background(), p))
		return p
	}
	patch := func(t *testing.T, srv *Server, id string, labels map[string]string) int {
		t.Helper()
		rec := doRequest(t, srv, http.MethodPatch, "/api/v1/projects/"+id, map[string]any{"labels": labels})
		return rec.Code
	}

	t.Run("omitted key keeps stored value", func(t *testing.T) {
		srv, s := testServer(t)
		p := newProject(t, s, map[string]string{store.LabelWorkspaceMode: "per-agent", "team": "a"})
		require.Equal(t, http.StatusOK, patch(t, srv, p.ID, map[string]string{"team": "b"}))
		got, err := s.GetProject(context.Background(), p.ID)
		require.NoError(t, err)
		assert.Equal(t, "per-agent", got.Labels[store.LabelWorkspaceMode])
		assert.Equal(t, "b", got.Labels["team"])
		assert.True(t, got.IsEmptyPerAgent())
	})
	t.Run("equal value accepted", func(t *testing.T) {
		srv, s := testServer(t)
		p := newProject(t, s, map[string]string{store.LabelWorkspaceMode: "per-agent"})
		require.Equal(t, http.StatusOK, patch(t, srv, p.ID, map[string]string{store.LabelWorkspaceMode: "per-agent", "x": "y"}))
	})
	t.Run("different value rejected", func(t *testing.T) {
		srv, s := testServer(t)
		p := newProject(t, s, map[string]string{store.LabelWorkspaceMode: "per-agent"})
		require.Equal(t, http.StatusBadRequest, patch(t, srv, p.ID, map[string]string{store.LabelWorkspaceMode: "shared"}))
		got, err := s.GetProject(context.Background(), p.ID)
		require.NoError(t, err)
		assert.Equal(t, "per-agent", got.Labels[store.LabelWorkspaceMode], "rejected PATCH leaves the label alone")
	})
	t.Run("clearing value rejected", func(t *testing.T) {
		srv, s := testServer(t)
		p := newProject(t, s, map[string]string{store.LabelWorkspaceMode: "shared"})
		require.Equal(t, http.StatusBadRequest, patch(t, srv, p.ID, map[string]string{store.LabelWorkspaceMode: ""}))
	})
	t.Run("adding to unlabeled project rejected", func(t *testing.T) {
		srv, s := testServer(t)
		p := newProject(t, s, map[string]string{"team": "a"})
		require.Equal(t, http.StatusBadRequest, patch(t, srv, p.ID, map[string]string{store.LabelWorkspaceMode: "per-agent"}))
	})
	t.Run("unlabeled project, labels without key", func(t *testing.T) {
		srv, s := testServer(t)
		p := newProject(t, s, nil)
		require.Equal(t, http.StatusOK, patch(t, srv, p.ID, map[string]string{"team": "a"}))
		got, err := s.GetProject(context.Background(), p.ID)
		require.NoError(t, err)
		_, has := got.Labels[store.LabelWorkspaceMode]
		assert.False(t, has)
	})
}

// TestProjectClone_WorkspaceModeRederived covers the clone column: the mode
// follows the CLONE's git-ness (after a gitRemote override).
func TestProjectClone_WorkspaceModeRederived(t *testing.T) {
	tests := []struct {
		name         string
		srcRemote    string
		srcLabel     string
		overrideGit  string
		wantLabel    string // "" = absent
		wantSharing  store.WorkspaceSharingMode
		wantCloneGit bool
	}{
		{name: "non-git per-agent stays empty-per-agent", srcLabel: "per-agent", wantLabel: "per-agent", wantSharing: store.SharingModeEmptyPerAgent},
		{name: "non-git per-agent + git override becomes clone-per-agent", srcLabel: "per-agent", overrideGit: "https://github.com/o/r.git", wantLabel: "per-agent", wantSharing: store.SharingModeClonePerAgent, wantCloneGit: true},
		{name: "non-git shared carried", srcLabel: "shared", wantLabel: "shared", wantSharing: store.SharingModeSharedPlain},
		{name: "git worktree carried", srcRemote: "github.com/test/src", srcLabel: "worktree-per-agent", wantLabel: "worktree-per-agent", wantSharing: store.SharingModeWorktreePerAgent, wantCloneGit: true},
		{name: "unknown label dropped", srcLabel: "bogus", wantSharing: store.SharingModeSharedPlain},
		{name: "invalid non-git worktree label dropped", srcLabel: "worktree-per-agent", wantSharing: store.SharingModeSharedPlain},
		{name: "no label stays absent", wantSharing: store.SharingModeSharedPlain},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, s := testServer(t)
			ctx := context.Background()
			labels := map[string]string{"team": "a"}
			if tt.srcLabel != "" {
				labels[store.LabelWorkspaceMode] = tt.srcLabel
			}
			src := &store.Project{
				ID: api.NewUUID(), Name: "Clone Src", Slug: "clone-src-" + api.NewShortID(),
				GitRemote: tt.srcRemote, OwnerID: DevUserID, CreatedBy: DevUserID, Labels: labels,
			}
			require.NoError(t, s.CreateProject(ctx, src))

			body := map[string]any{"name": "Clone Dst " + tt.name}
			if tt.overrideGit != "" {
				body["gitRemote"] = tt.overrideGit
			}
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+src.ID+"/clone", body)
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
			var clone store.Project
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&clone))

			got, err := s.GetProject(ctx, clone.ID)
			require.NoError(t, err)
			label, has := got.Labels[store.LabelWorkspaceMode]
			if tt.wantLabel == "" {
				assert.False(t, has, "label should be absent, got %q", label)
			} else {
				assert.Equal(t, tt.wantLabel, label)
			}
			assert.Equal(t, tt.wantCloneGit, got.GitRemote != "")
			assert.Equal(t, tt.wantSharing, got.SharingMode())
		})
	}
}

func TestMergePatchWorkspaceModeLabel_DoesNotMutateInput(t *testing.T) {
	updates := map[string]string{"team": "b"}
	out, err := mergePatchWorkspaceModeLabel(map[string]string{store.LabelWorkspaceMode: "per-agent"}, updates)
	require.NoError(t, err)
	assert.Equal(t, "per-agent", out[store.LabelWorkspaceMode])
	_, has := updates[store.LabelWorkspaceMode]
	assert.False(t, has, "caller's map must not be mutated")
}

// TestMergePatchWorkspaceModeLabel_NilUpdates pins that a nil updates map
// keeps the stored label rather than panicking on a nil-map write.
func TestMergePatchWorkspaceModeLabel_NilUpdates(t *testing.T) {
	out, err := mergePatchWorkspaceModeLabel(map[string]string{store.LabelWorkspaceMode: "per-agent"}, nil)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{store.LabelWorkspaceMode: "per-agent"}, out)
}

// TestDispatchWorkspaceMode_NilProject pins that a nil project sends no mode
// rather than panicking.
func TestDispatchWorkspaceMode_NilProject(t *testing.T) {
	assert.Equal(t, "", dispatchWorkspaceMode(nil))
}

func TestCheckEmptyPerAgentBrokerCapability(t *testing.T) {
	empty := &store.Project{Labels: map[string]string{store.LabelWorkspaceMode: "per-agent"}}
	gitPerAgent := &store.Project{GitRemote: "github.com/a/b", Labels: map[string]string{store.LabelWorkspaceMode: "per-agent"}}
	oldBroker := &store.RuntimeBroker{ID: "b1", Name: "old-broker"}
	noCap := &store.RuntimeBroker{ID: "b2", Name: "nocap", Capabilities: &store.BrokerCapabilities{Reprovision: true}}
	newBroker := &store.RuntimeBroker{ID: "b3", Name: "new", Capabilities: &store.BrokerCapabilities{EmptyPerAgentWorkspace: true}}

	assert.NoError(t, checkEmptyPerAgentBrokerCapability(gitPerAgent, oldBroker), "other modes are never gated")
	assert.NoError(t, checkEmptyPerAgentBrokerCapability(&store.Project{}, oldBroker))
	assert.NoError(t, checkEmptyPerAgentBrokerCapability(empty, newBroker))

	err := checkEmptyPerAgentBrokerCapability(empty, oldBroker)
	require.ErrorIs(t, err, errBrokerLacksEmptyPerAgent)
	assert.Contains(t, err.Error(), "old-broker")
	require.ErrorIs(t, checkEmptyPerAgentBrokerCapability(empty, noCap), errBrokerLacksEmptyPerAgent)
}

// TestDeriveCloneWorkspaceMode covers the clone re-derivation rule without
// the network clone a git shared-workspace clone triggers (review #2717 N6).
func TestDeriveCloneWorkspaceMode(t *testing.T) {
	cases := []struct {
		name       string
		srcLabel   string
		srcIsGit   bool
		cloneIsGit bool
		want       string
		wantMode   store.WorkspaceSharingMode
	}{
		{"no label", "", false, false, "", store.SharingModeSharedPlain},
		{"no label + git override", "", false, true, "", store.SharingModeSharedPlain},
		{"non-git shared", "shared", false, false, "shared", store.SharingModeSharedPlain},
		{"non-git shared + git override", "shared", false, true, "shared", store.SharingModeSharedPlain},
		{"non-git per-agent", "per-agent", false, false, "per-agent", store.SharingModeEmptyPerAgent},
		{"non-git per-agent + git override", "per-agent", false, true, "per-agent", store.SharingModeClonePerAgent},
		{"git worktree", "worktree-per-agent", true, true, "worktree-per-agent", store.SharingModeWorktreePerAgent},
		{"worktree on non-git clone dropped", "worktree-per-agent", true, false, "", store.SharingModeSharedPlain},
		{"unknown dropped", "bogus", false, true, "", store.SharingModeSharedPlain},
		{"legacy canonical value normalised", "empty-per-agent", false, false, "per-agent", store.SharingModeEmptyPerAgent},
		{"legacy canonical value + git override", "empty-per-agent", false, true, "per-agent", store.SharingModeClonePerAgent},
		// On a git source the legacy label resolved to shared-plain, not
		// empty-per-agent, so it must not become per-agent (clone-per-agent).
		{"legacy canonical value on git source dropped", "empty-per-agent", true, true, "", store.SharingModeSharedPlain},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := deriveCloneWorkspaceMode(tc.srcLabel, tc.srcIsGit, tc.cloneIsGit)
			if got != tc.want {
				t.Fatalf("deriveCloneWorkspaceMode(%q, srcGit=%v, cloneGit=%v) = %q, want %q", tc.srcLabel, tc.srcIsGit, tc.cloneIsGit, got, tc.want)
			}
			if mode := store.ResolveProjectSharingMode(got, tc.cloneIsGit); mode != tc.wantMode {
				t.Errorf("resolved mode = %q, want %q", mode, tc.wantMode)
			}
		})
	}
}
