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

package store

import "testing"

func TestResolveWorkspaceSharingMode(t *testing.T) {
	tests := []struct {
		label string
		want  WorkspaceSharingMode
	}{
		// Canonical label values
		{label: "shared", want: SharingModeSharedPlain},
		{label: "per-agent", want: SharingModeClonePerAgent},
		{label: "worktree-per-agent", want: SharingModeWorktreePerAgent},

		// Canonical enum values (accepted as aliases)
		{label: "shared-plain", want: SharingModeSharedPlain},
		{label: "clone-per-agent", want: SharingModeClonePerAgent},
		{label: "empty-per-agent", want: SharingModeEmptyPerAgent}, // round-trips through SCION_WORKSPACE_MODE

		// Empty → default (shared-plain)
		{label: "", want: SharingModeSharedPlain},

		// Unknown → default (shared-plain)
		{label: "unknown-mode", want: SharingModeSharedPlain},
		{label: "SHARED", want: SharingModeSharedPlain}, // case-sensitive: unrecognized → default
	}

	for _, tt := range tests {
		t.Run("label="+tt.label, func(t *testing.T) {
			got := ResolveWorkspaceSharingMode(tt.label)
			if got != tt.want {
				t.Errorf("ResolveWorkspaceSharingMode(%q) = %q, want %q", tt.label, got, tt.want)
			}
		})
	}
}

func TestWorkspaceSharingMode_Constants(t *testing.T) {
	// Verify the existing label constants are unchanged (lossless migration).
	if WorkspaceModeShared != "shared" {
		t.Errorf("WorkspaceModeShared = %q, want %q", WorkspaceModeShared, "shared")
	}
	if WorkspaceModePerAgent != "per-agent" {
		t.Errorf("WorkspaceModePerAgent = %q, want %q", WorkspaceModePerAgent, "per-agent")
	}
	if LabelWorkspaceMode != "scion.dev/workspace-mode" {
		t.Errorf("LabelWorkspaceMode = %q, want %q", LabelWorkspaceMode, "scion.dev/workspace-mode")
	}

	// Verify the new typed constants have the expected string values.
	if SharingModeSharedPlain != "shared-plain" {
		t.Errorf("SharingModeSharedPlain = %q, want %q", SharingModeSharedPlain, "shared-plain")
	}
	if SharingModeClonePerAgent != "clone-per-agent" {
		t.Errorf("SharingModeClonePerAgent = %q, want %q", SharingModeClonePerAgent, "clone-per-agent")
	}
	if SharingModeWorktreePerAgent != "worktree-per-agent" {
		t.Errorf("SharingModeWorktreePerAgent = %q, want %q", SharingModeWorktreePerAgent, "worktree-per-agent")
	}
	if SharingModeEmptyPerAgent != "empty-per-agent" {
		t.Errorf("SharingModeEmptyPerAgent = %q, want %q", SharingModeEmptyPerAgent, "empty-per-agent")
	}
	if WorkspaceModeWorktreePerAgent != "worktree-per-agent" {
		t.Errorf("WorkspaceModeWorktreePerAgent = %q, want %q", WorkspaceModeWorktreePerAgent, "worktree-per-agent")
	}
}

func TestProject_IsWorktreePerAgent(t *testing.T) {
	tests := []struct {
		name    string
		project Project
		want    bool
	}{
		{
			name: "worktree-per-agent git project",
			project: Project{
				GitRemote: "github.com/test/repo",
				Labels:    map[string]string{LabelWorkspaceMode: WorkspaceModeWorktreePerAgent},
			},
			want: true,
		},
		{
			name: "shared git project",
			project: Project{
				GitRemote: "github.com/test/repo",
				Labels:    map[string]string{LabelWorkspaceMode: WorkspaceModeShared},
			},
			want: false,
		},
		{
			name: "per-agent git project",
			project: Project{
				GitRemote: "github.com/test/repo",
				Labels:    map[string]string{LabelWorkspaceMode: WorkspaceModePerAgent},
			},
			want: false,
		},
		{
			name: "worktree label but no git remote",
			project: Project{
				Labels: map[string]string{LabelWorkspaceMode: WorkspaceModeWorktreePerAgent},
			},
			want: false,
		},
		{
			name:    "no labels",
			project: Project{GitRemote: "github.com/test/repo"},
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.project.IsWorktreePerAgent()
			if got != tt.want {
				t.Errorf("IsWorktreePerAgent() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestGCPServiceAccount_ReachableFromProject pins the predicate that decides
// whether a service account may be used from within a project.
//
// It is unit-tested here, on the type, because it is now called from more than
// one package and its callers cannot all be reached from any one handler test.
// The cases that matter are the two the previous implementation got wrong: it
// compared ScopeID against the project ID without ever reading Scope, so a
// hub-scoped account was rejected everywhere (it was comparing a hub instance
// ID against a project ID), and a user-scoped account whose ScopeID happened to
// equal a project ID would have been accepted.
func TestGCPServiceAccount_ReachableFromProject(t *testing.T) {
	const projectID = "project-alpha"

	tests := []struct {
		name string
		sa   *GCPServiceAccount
		from string
		want bool
		why  string
	}{
		{
			name: "project scope, own project",
			sa:   &GCPServiceAccount{Scope: ScopeProject, ScopeID: projectID},
			from: projectID,
			want: true,
		},
		{
			name: "project scope, different project",
			sa:   &GCPServiceAccount{Scope: ScopeProject, ScopeID: "project-beta"},
			from: projectID,
			want: false,
			why:  "confinement to the owning project is the whole point of project scope",
		},
		{
			name: "hub scope, reachable from any project",
			sa:   &GCPServiceAccount{Scope: ScopeHub, ScopeID: "hub-instance-1"},
			from: projectID,
			want: true,
			why:  "hub scope means every project; this is the case the old ScopeID comparison could not express",
		},
		{
			name: "hub scope, hub id irrelevant",
			sa:   &GCPServiceAccount{Scope: ScopeHub, ScopeID: "some-completely-different-hub"},
			from: projectID,
			want: true,
			why: "ScopeID on a hub-scoped account is provenance, never a predicate. " +
				"The hub ID derives from config or a hostname hash, so consulting it " +
				"here would orphan every hub-scoped account across a redeploy",
		},
		{
			name: "hub scope, empty scope id",
			sa:   &GCPServiceAccount{Scope: ScopeHub},
			from: projectID,
			want: true,
			why:  "follows from ScopeID being ignored for hub scope",
		},
		{
			name: "user scope is never reachable from a project",
			sa:   &GCPServiceAccount{Scope: ScopeUser, ScopeID: "user-123"},
			from: projectID,
			want: false,
		},
		{
			name: "user scope whose scope id collides with the project id",
			sa:   &GCPServiceAccount{Scope: ScopeUser, ScopeID: projectID},
			from: projectID,
			want: false,
			why: "matching on Scope first is what makes this false. An implementation " +
				"that compared ScopeID alone would accept a user's account as the project's",
		},
		{
			name: "unknown scope fails closed",
			sa:   &GCPServiceAccount{Scope: "something-new", ScopeID: projectID},
			from: projectID,
			want: false,
			why:  "a scope added later must not become reachable by default",
		},
		{
			name: "project scope with empty scope id",
			sa:   &GCPServiceAccount{Scope: ScopeProject},
			from: projectID,
			want: false,
		},
		{
			name: "nil receiver",
			sa:   nil,
			from: projectID,
			want: false,
			why:  "callers reach this straight after a store lookup; a nil must not read as reachable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.sa.ReachableFromProject(tt.from); got != tt.want {
				t.Errorf("ReachableFromProject(%q) = %v, want %v. %s", tt.from, got, tt.want, tt.why)
			}
		})
	}
}

// TestMessageExpiredStuckPendingReason_Value pins the exact string value of
// MessageExpiredStuckPendingReason (nc-promote-busy round 4 Consider).
// Production messages.dispatch_failure_reason rows already carry this exact
// literal, written by pkg/hub/sweep.go before this constant existed. Do not
// "fix" a failure here by updating this expected value: changing the
// constant silently strands every existing row outside
// BackfillNonAgentDispatchState's exact-match predicate. A real change to
// this string must ship together with a data migration for existing rows,
// and this test's expected literal must be updated only as part of that
// migration.
func TestMessageExpiredStuckPendingReason_Value(t *testing.T) {
	const shipped = "expired: stuck in pending state beyond TTL"
	if MessageExpiredStuckPendingReason != shipped {
		t.Fatalf("MessageExpiredStuckPendingReason changed value: got %q, want %q (see this test's doc comment before changing either)",
			MessageExpiredStuckPendingReason, shipped)
	}
}

func TestResolveProjectSharingMode(t *testing.T) {
	tests := []struct {
		label string
		isGit bool
		want  WorkspaceSharingMode
	}{
		// Git projects: identical to label-only resolution.
		{"", true, SharingModeSharedPlain},
		{"shared", true, SharingModeSharedPlain},
		{"per-agent", true, SharingModeClonePerAgent},
		{"worktree-per-agent", true, SharingModeWorktreePerAgent},
		{"bogus", true, SharingModeSharedPlain},
		// empty-per-agent is non-git only: a legacy raw label on a git
		// project must not resolve to a mode without git env.
		{"empty-per-agent", true, SharingModeSharedPlain},

		// Non-git projects: per-agent means an empty private dir.
		{"", false, SharingModeSharedPlain},
		{"shared", false, SharingModeSharedPlain},
		{"per-agent", false, SharingModeEmptyPerAgent},
		{"empty-per-agent", false, SharingModeEmptyPerAgent},
		{"worktree-per-agent", false, SharingModeSharedPlain},
		{"clone-per-agent", false, SharingModeSharedPlain},
		{"bogus", false, SharingModeSharedPlain},
	}
	for _, tt := range tests {
		got := ResolveProjectSharingMode(tt.label, tt.isGit)
		if got != tt.want {
			t.Errorf("ResolveProjectSharingMode(%q, git=%v) = %q, want %q", tt.label, tt.isGit, got, tt.want)
		}
		// The canonical value must survive a round trip through the
		// label-only resolver used by brokers/sciontool (env value).
		if rt := ResolveWorkspaceSharingMode(string(got)); rt != got {
			t.Errorf("round trip of %q via ResolveWorkspaceSharingMode = %q", got, rt)
		}
	}
}

func TestProjectSharingModeHelpers(t *testing.T) {
	tests := []struct {
		name          string
		project       Project
		want          WorkspaceSharingMode
		wantEmptyPerA bool
	}{
		{"non-git no label", Project{}, SharingModeSharedPlain, false},
		{"non-git shared", Project{Labels: map[string]string{LabelWorkspaceMode: "shared"}}, SharingModeSharedPlain, false},
		{"non-git per-agent", Project{Labels: map[string]string{LabelWorkspaceMode: "per-agent"}}, SharingModeEmptyPerAgent, true},
		{"git per-agent", Project{GitRemote: "github.com/a/b", Labels: map[string]string{LabelWorkspaceMode: "per-agent"}}, SharingModeClonePerAgent, false},
		{"git worktree", Project{GitRemote: "github.com/a/b", Labels: map[string]string{LabelWorkspaceMode: "worktree-per-agent"}}, SharingModeWorktreePerAgent, false},
		// Legacy raw canonical label (accepted verbatim before #2703): the
		// helpers must agree with each other.
		{"non-git legacy empty-per-agent label", Project{Labels: map[string]string{LabelWorkspaceMode: "empty-per-agent"}}, SharingModeEmptyPerAgent, true},
		{"git legacy empty-per-agent label", Project{GitRemote: "github.com/a/b", Labels: map[string]string{LabelWorkspaceMode: "empty-per-agent"}}, SharingModeSharedPlain, false},
	}
	var nilProject *Project
	if nilProject.IsEmptyPerAgent() {
		t.Error("nil project must not be empty-per-agent")
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.project.SharingMode(); got != tt.want {
				t.Errorf("SharingMode() = %q, want %q", got, tt.want)
			}
			if got := tt.project.IsEmptyPerAgent(); got != tt.wantEmptyPerA {
				t.Errorf("IsEmptyPerAgent() = %v, want %v", got, tt.wantEmptyPerA)
			}
		})
	}
}

func TestValidateWorkspaceMode(t *testing.T) {
	tests := []struct {
		mode    string
		isGit   bool
		wantErr bool
	}{
		{"", true, false},
		{"", false, false},
		{"shared", true, false},
		{"shared", false, false},
		{"per-agent", true, false},
		{"per-agent", false, false},
		{"worktree-per-agent", true, false},
		{"worktree-per-agent", false, true},
		{"bogus", true, true},
		{"bogus", false, true},
		// Canonical sharing-mode names are not API/label values.
		{"empty-per-agent", false, true},
		{"clone-per-agent", true, true},
	}
	for _, tt := range tests {
		err := ValidateWorkspaceMode(tt.mode, tt.isGit)
		if (err != nil) != tt.wantErr {
			t.Errorf("ValidateWorkspaceMode(%q, git=%v) err = %v, wantErr %v", tt.mode, tt.isGit, err, tt.wantErr)
		}
	}
}
