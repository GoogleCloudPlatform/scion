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

package runtimebroker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestIsStrictWorktreeChild guards the only shape of path
// tryProvisionWorktree is ever allowed to pass to `git worktree remove` or
// os.RemoveAll: a real descendant of <base>/worktrees, never that directory
// itself and never something outside it. An empty AgentID makes
// provision.WorktreePath resolve to the shared "worktrees" parent directory
// of every agent (GoogleCloudPlatform/scion#1931) — the shape this check
// exists to reject.
func TestIsStrictWorktreeChild(t *testing.T) {
	base := "/proj/workspace"
	tests := []struct {
		name string
		path string
		want bool
	}{
		{"real descendant", "/proj/workspace/worktrees/agent-1", true},
		{"nested descendant", "/proj/workspace/worktrees/agent-1/sub", true},
		{"the worktrees dir itself", "/proj/workspace/worktrees", false},
		{"the worktrees dir with trailing slash", "/proj/workspace/worktrees/", false},
		{"a sibling directory", "/proj/workspace/other", false},
		{"outside the project entirely", "/etc/passwd", false},
		{"a relative segment resolving back out", "/proj/workspace/worktrees/../other", false},
		{"empty path", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isStrictWorktreeChild(base, tt.path); got != tt.want {
				t.Errorf("isStrictWorktreeChild(%q, %q) = %v, want %v", base, tt.path, got, tt.want)
			}
		})
	}
	if isStrictWorktreeChild("", "/proj/workspace/worktrees/agent-1") {
		t.Error("expected false when base is empty")
	}
}

// TestShouldCleanupPartialWorktree_NeverTheSharedWorktreesDir is the direct
// unit-level guard for GoogleCloudPlatform/scion#1931: provision.WorktreePath
// resolves to the shared "worktrees" parent directory itself when AgentID is
// empty, so a resolver bug that ever produces that value as "the worktree
// path" must never be allowed to authorize its removal — nor to authorize
// removing anything that pre-existed.
func TestShouldCleanupPartialWorktree_NeverTheSharedWorktreesDir(t *testing.T) {
	projectRoot := "/proj/workspace"
	sharedWorktreesDir := filepath.Join(projectRoot, "worktrees")
	ownWorktree := filepath.Join(sharedWorktreesDir, "agent-1")

	tests := []struct {
		name         string
		projectRoot  string
		worktreePath string
		preExisted   bool
		want         bool
	}{
		{"normal partial cleanup, not preExisted", projectRoot, ownWorktree, false, true},
		{"never when preExisted, even for a valid own path", projectRoot, ownWorktree, true, false},
		{"never the shared worktrees dir itself, even if not preExisted", projectRoot, sharedWorktreesDir, false, false},
		{"never outside the project", projectRoot, "/etc/passwd", false, false},
		{"empty worktreePath", projectRoot, "", false, false},
		{"empty projectRoot", "", ownWorktree, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldCleanupPartialWorktree(tt.projectRoot, tt.worktreePath, tt.preExisted); got != tt.want {
				t.Errorf("shouldCleanupPartialWorktree(%q, %q, %v) = %v, want %v",
					tt.projectRoot, tt.worktreePath, tt.preExisted, got, tt.want)
			}
		})
	}
}

// TestTryProvisionWorktree_InvalidAgentIDLeavesSharedBaseIntact is an
// end-to-end guard (through the real resolveWorktreeProvision ->
// tryProvisionWorktree path, not a hand-built worktreeProvisionResult) for
// an AgentID of "..". Since provision.WorktreePath(base, agentID) is
// filepath.Join(base, "worktrees", agentID), an AgentID of ".." would
// otherwise resolve to base itself (filepath.Join cleans "worktrees/.."
// away) — the shared clone root holding the common .git and every other
// agent's worktrees. isSingleCleanPathElement rejects this AgentID outright,
// before anything is resolved or created on disk; validateMountedWorktree
// is a second, independent check on whatever path is finally about to be
// mounted.
func TestTryProvisionWorktree_InvalidAgentIDLeavesSharedBaseIntact(t *testing.T) {
	requireWorktreeGit(t)

	for _, agentID := range []string{"..", "a/b"} {
		t.Run("agentID="+agentID, func(t *testing.T) {
			cfg := DefaultServerConfig()
			cfg.StateDir = t.TempDir()
			srv := newTestServerForStartContext(t, cfg)

			projectPath := filepath.Join(t.TempDir(), "proj")
			if err := os.MkdirAll(projectPath, 0o755); err != nil {
				t.Fatal(err)
			}
			invalidGitClone := &api.GitCloneConfig{URL: filepath.Join(t.TempDir(), "does-not-exist.git")}

			opts := &api.StartOptions{}
			ok, _, err := srv.tryProvisionWorktree(context.Background(), startContextInputs{
				Name: "agent-x", AgentID: agentID,
				ProjectID: "p1", ProjectSlug: "proj", ProjectPath: projectPath,
				WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
				Config:        &CreateAgentConfig{GitClone: invalidGitClone},
			}, opts, map[string]string{}, srv.runtime.Name())

			if ok {
				t.Error("expected ok=false for an invalid AgentID")
			}
			if err == nil {
				t.Error("expected an error rejecting the invalid AgentID")
			}

			// The shared base must never be created, let alone removed, as a
			// side effect of this rejected attempt: any stat error other than
			// "does not exist" would indicate something unexpected happened
			// to it.
			base := filepath.Join(projectPath, "workspace")
			if _, statErr := os.Stat(base); statErr != nil && !os.IsNotExist(statErr) {
				t.Fatalf("unexpected error checking the shared base for AgentID=%q: %v", agentID, statErr)
			}
		})
	}
}

// setUpAgent1SharedBase provisions a first agent's real worktree via
// tryProvisionWorktree, establishing the shared base clone that later
// scenarios in this file set up pre-existing state under.
func setUpAgent1SharedBase(t *testing.T, srv *Server, projectPath, bare string) {
	t.Helper()
	opts := &api.StartOptions{}
	ok, _, err := srv.tryProvisionWorktree(context.Background(), startContextInputs{
		Name: "agent-1", AgentID: "agent-1",
		ProjectID: "p1", ProjectSlug: "proj", ProjectPath: projectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: &api.GitCloneConfig{URL: bare, Branch: "main"}},
	}, opts, map[string]string{}, srv.runtime.Name())
	if err != nil || !ok {
		t.Fatalf("setup: tryProvisionWorktree for agent-1: ok=%v err=%v", ok, err)
	}
}

// TestTryProvisionWorktree_SymlinkedOwnWorktreeRejected proves a symlink
// planted at an agent's own worktree target — pointing at a directory
// outside the shared base — is rejected rather than mounted, and that the
// symlink and its target are left untouched.
func TestTryProvisionWorktree_SymlinkedOwnWorktreeRejected(t *testing.T) {
	requireWorktreeGit(t)
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	bare := initBareRepoWithCommit(t)
	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	setUpAgent1SharedBase(t, srv, projectPath, bare)

	base := filepath.Join(projectPath, "workspace")
	outsideDir := t.TempDir()
	sentinelFile := filepath.Join(outsideDir, "sentinel.txt")
	const sentinelContent = "must not be mounted"
	if err := os.WriteFile(sentinelFile, []byte(sentinelContent), 0o644); err != nil {
		t.Fatal(err)
	}

	agent2Path := filepath.Join(base, "worktrees", "agent-2")
	if err := os.Symlink(outsideDir, agent2Path); err != nil {
		t.Fatal(err)
	}

	opts := &api.StartOptions{}
	ok, _, err := srv.tryProvisionWorktree(context.Background(), startContextInputs{
		Name: "agent-2", AgentID: "agent-2",
		ProjectID: "p1", ProjectSlug: "proj", ProjectPath: projectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: &api.GitCloneConfig{URL: bare, Branch: "main"}},
	}, opts, map[string]string{}, srv.runtime.Name())

	if ok {
		t.Error("expected ok=false for a symlinked worktree target")
	}
	if err == nil {
		t.Fatal("expected an error rejecting the symlinked worktree target")
	}
	if opts.Workspace != "" {
		t.Errorf("expected no workspace to be mounted, got %q", opts.Workspace)
	}
	if target, readErr := os.Readlink(agent2Path); readErr != nil || target != outsideDir {
		t.Errorf("expected the symlink to survive unchanged, got target=%q err=%v", target, readErr)
	}
	if got, readErr := os.ReadFile(sentinelFile); readErr != nil || string(got) != sentinelContent {
		t.Errorf("expected the outside file to survive untouched, got %q err=%v", got, readErr)
	}
}

// TestTryProvisionWorktree_SymlinkedWorktreesDirRejected proves that if the
// shared base's own "worktrees" directory is itself a symlink to somewhere
// outside the base, provisioning is rejected before it creates anything
// through that symlink.
func TestTryProvisionWorktree_SymlinkedWorktreesDirRejected(t *testing.T) {
	requireWorktreeGit(t)
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	bare := initBareRepoWithCommit(t)
	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	setUpAgent1SharedBase(t, srv, projectPath, bare)

	base := filepath.Join(projectPath, "workspace")
	worktreesDir := filepath.Join(base, "worktrees")
	outsideDir := t.TempDir()

	if err := os.RemoveAll(worktreesDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, worktreesDir); err != nil {
		t.Fatal(err)
	}

	opts := &api.StartOptions{}
	ok, _, err := srv.tryProvisionWorktree(context.Background(), startContextInputs{
		Name: "agent-2", AgentID: "agent-2",
		ProjectID: "p1", ProjectSlug: "proj", ProjectPath: projectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: &api.GitCloneConfig{URL: bare, Branch: "main"}},
	}, opts, map[string]string{}, srv.runtime.Name())

	if ok {
		t.Error("expected ok=false when the worktrees directory is a symlink")
	}
	if err == nil {
		t.Fatal("expected an error rejecting the symlinked worktrees directory")
	}
	entries, _ := os.ReadDir(outsideDir)
	if len(entries) != 0 {
		t.Errorf("expected nothing created in the outside directory, found: %v", entries)
	}
}

// TestTryProvisionWorktree_SharerRegistryOutsidePathRejected proves that a
// sharer-registry marker naming a worktree path outside the shared base is
// never joined or mounted.
func TestTryProvisionWorktree_SharerRegistryOutsidePathRejected(t *testing.T) {
	requireWorktreeGit(t)
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	bare := initBareRepoWithCommit(t)
	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	setUpAgent1SharedBase(t, srv, projectPath, bare)

	base := filepath.Join(projectPath, "workspace")
	outsideDir := t.TempDir()
	sentinelFile := filepath.Join(outsideDir, "sentinel.txt")
	const sentinelContent = "must not be mounted"
	if err := os.WriteFile(sentinelFile, []byte(sentinelContent), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := provision.RegisterSharer(base, "", "agent-3", outsideDir, "some-other-agent"); err != nil {
		t.Fatalf("plant sharer marker: %v", err)
	}

	opts := &api.StartOptions{}
	ok, _, err := srv.tryProvisionWorktree(context.Background(), startContextInputs{
		Name: "agent-3", AgentID: "agent-3",
		ProjectID: "p1", ProjectSlug: "proj", ProjectPath: projectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: &api.GitCloneConfig{URL: bare, Branch: "main"}},
	}, opts, map[string]string{}, srv.runtime.Name())

	if ok {
		t.Error("expected ok=false when the sharer registry names a path outside the base")
	}
	if err == nil {
		t.Fatal("expected an error rejecting the sharer-registry entry")
	}
	if opts.Workspace != "" {
		t.Errorf("expected no workspace to be mounted, got %q", opts.Workspace)
	}
	if got, readErr := os.ReadFile(sentinelFile); readErr != nil || string(got) != sentinelContent {
		t.Errorf("expected the outside file to survive untouched, got %q err=%v", got, readErr)
	}
}

// TestTryProvisionWorktree_SharerRegistryFakeGitfileStillRejected proves the
// final mount-time gate catches a sharer-registry entry that would pass
// ensureWorktree's own worktree-shape check — a .git gitfile whose target
// textually resolves under the base's own admin directory — but whose
// physical location is still outside the base's "worktrees" directory once
// symlinks are resolved.
func TestTryProvisionWorktree_SharerRegistryFakeGitfileStillRejected(t *testing.T) {
	requireWorktreeGit(t)
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	bare := initBareRepoWithCommit(t)
	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	setUpAgent1SharedBase(t, srv, projectPath, bare)

	base := filepath.Join(projectPath, "workspace")
	outsideDir := t.TempDir()
	sentinelFile := filepath.Join(outsideDir, "sentinel.txt")
	const sentinelContent = "must not be mounted"
	if err := os.WriteFile(sentinelFile, []byte(sentinelContent), 0o644); err != nil {
		t.Fatal(err)
	}
	adminEntry := filepath.Join(base, ".git", "worktrees", "fake-entry")
	if err := os.WriteFile(filepath.Join(outsideDir, ".git"), []byte("gitdir: "+adminEntry+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := provision.RegisterSharer(base, "", "agent-3", outsideDir, "some-other-agent"); err != nil {
		t.Fatalf("plant sharer marker: %v", err)
	}

	opts := &api.StartOptions{}
	ok, _, err := srv.tryProvisionWorktree(context.Background(), startContextInputs{
		Name: "agent-3", AgentID: "agent-3",
		ProjectID: "p1", ProjectSlug: "proj", ProjectPath: projectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: &api.GitCloneConfig{URL: bare, Branch: "main"}},
	}, opts, map[string]string{}, srv.runtime.Name())

	if ok {
		t.Error("expected ok=false when the sharer registry names a path outside the base, even with a matching gitfile")
	}
	if err == nil {
		t.Fatal("expected an error rejecting the sharer-registry entry")
	}
	if opts.Workspace != "" {
		t.Errorf("expected no workspace to be mounted, got %q", opts.Workspace)
	}
	if got, readErr := os.ReadFile(sentinelFile); readErr != nil || string(got) != sentinelContent {
		t.Errorf("expected the outside file to survive untouched, got %q err=%v", got, readErr)
	}
}

// TestValidateMountedWorktree is a direct, function-level unit test for the
// final mount-time gate. It calls validateMountedWorktree directly against a
// real shared base, independent of ensureWorktree's own equivalent checks —
// which, in every scenario reachable through tryProvisionWorktree, already
// validate a registry or git-list join target before validateMountedWorktree
// ever sees it. Calling the function directly here pins its own behavior
// even when those earlier checks would otherwise already have refused the
// same input.
func TestValidateMountedWorktree(t *testing.T) {
	requireWorktreeGit(t)
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	bare := initBareRepoWithCommit(t)
	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	setUpAgent1SharedBase(t, srv, projectPath, bare)
	base := filepath.Join(projectPath, "workspace")
	agent1Worktree := filepath.Join(base, "worktrees", "agent-1")

	t.Run("a direct child real worktree is accepted", func(t *testing.T) {
		if err := validateMountedWorktree(agent1Worktree, base); err != nil {
			t.Errorf("expected the real direct-child worktree to be accepted, got: %v", err)
		}
	})

	t.Run("a nested subdirectory of a real worktree is rejected", func(t *testing.T) {
		nested := filepath.Join(agent1Worktree, "sub")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatal(err)
		}
		adminEntry := filepath.Join(base, ".git", "worktrees", "fake-entry")
		if err := os.WriteFile(filepath.Join(nested, ".git"), []byte("gitdir: "+adminEntry+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := validateMountedWorktree(nested, base); err == nil {
			t.Error("expected a nested subdirectory of a real worktree to be rejected")
		}
	})

	t.Run("an intermediate symlink component is rejected", func(t *testing.T) {
		outsideDir := t.TempDir()
		sub := filepath.Join(outsideDir, "sub")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		adminEntry := filepath.Join(base, ".git", "worktrees", "fake-entry-2")
		if err := os.WriteFile(filepath.Join(sub, ".git"), []byte("gitdir: "+adminEntry+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		lnk := filepath.Join(agent1Worktree, "lnk2")
		if err := os.Symlink(outsideDir, lnk); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(lnk, "sub")
		if err := validateMountedWorktree(marker, base); err == nil {
			t.Error("expected a path reached through an intermediate symlink to be rejected")
		}
	})

	t.Run("a path with a symlink-and-dot-dot component is rejected", func(t *testing.T) {
		adminEntry := filepath.Join(base, ".git", "worktrees", "agent-1")
		inner := filepath.Join(agent1Worktree, "agent-1")
		if err := os.MkdirAll(inner, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(inner, ".git"), []byte("gitdir: "+adminEntry+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(agent1Worktree, filepath.Join(agent1Worktree, "up")); err != nil {
			t.Fatal(err)
		}
		sep := string(filepath.Separator)
		p := filepath.Join(agent1Worktree, "up") + sep + ".." + sep + "agent-1"
		if err := validateMountedWorktree(p, base); err == nil {
			t.Errorf("expected %s to be rejected", p)
		}
	})

	t.Run("a path through a symlink back to the worktrees directory is rejected", func(t *testing.T) {
		adminEntry := filepath.Join(base, ".git", "worktrees", "agent-1")
		// the same gitdir in absolute form, so the shape check alone accepts the path below
		if err := os.WriteFile(filepath.Join(agent1Worktree, ".git"), []byte("gitdir: "+adminEntry+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(base, "worktrees"), filepath.Join(agent1Worktree, "wt")); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(agent1Worktree, "wt", "agent-1")
		if err := validateMountedWorktree(p, base); err == nil {
			t.Errorf("expected %s to be rejected", p)
		}
	})
}

// TestTryProvisionWorktree_SharerRegistryNestedMarkerRejected proves that a
// sharer-registry marker naming a subdirectory of an existing real worktree —
// not a direct child of the shared base's own "worktrees" directory — is
// refused, even with a gitfile that resolves into the base's own admin
// directory. The resolved workspace must be a direct child of the base's own
// "worktrees" directory; entries directly inside "worktrees" are created
// only by the broker.
func TestTryProvisionWorktree_SharerRegistryNestedMarkerRejected(t *testing.T) {
	requireWorktreeGit(t)
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	bare := initBareRepoWithCommit(t)
	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	setUpAgent1SharedBase(t, srv, projectPath, bare)

	base := filepath.Join(projectPath, "workspace")
	agent1Worktree := filepath.Join(base, "worktrees", "agent-1")
	nested := filepath.Join(agent1Worktree, "sub")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	adminEntry := filepath.Join(base, ".git", "worktrees", "fake-entry")
	if err := os.WriteFile(filepath.Join(nested, ".git"), []byte("gitdir: "+adminEntry+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := provision.RegisterSharer(base, "", "agent-4", nested, "agent-1"); err != nil {
		t.Fatalf("plant sharer marker: %v", err)
	}

	opts := &api.StartOptions{}
	ok, _, err := srv.tryProvisionWorktree(context.Background(), startContextInputs{
		Name: "agent-4", AgentID: "agent-4",
		ProjectID: "p1", ProjectSlug: "proj", ProjectPath: projectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: &api.GitCloneConfig{URL: bare, Branch: "main"}},
	}, opts, map[string]string{}, srv.runtime.Name())

	if ok {
		t.Error("expected ok=false when the sharer registry names a nested subdirectory of another worktree")
	}
	if err == nil {
		t.Fatal("expected an error rejecting the nested sharer-registry entry")
	}
	if opts.Workspace != "" {
		t.Errorf("expected no workspace to be mounted, got %q", opts.Workspace)
	}
	if _, statErr := os.Stat(nested); statErr != nil {
		t.Errorf("expected the nested directory to survive untouched, stat error: %v", statErr)
	}
}

// TestTryProvisionWorktree_SharerRegistryIntermediateSymlinkRejected proves
// that a sharer-registry marker reached through an intermediate symlink
// component is refused once its physical location is resolved, even though
// the final path component itself is a real, non-symlink directory with a
// gitfile that looks valid on its own.
func TestTryProvisionWorktree_SharerRegistryIntermediateSymlinkRejected(t *testing.T) {
	requireWorktreeGit(t)
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	bare := initBareRepoWithCommit(t)
	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	setUpAgent1SharedBase(t, srv, projectPath, bare)

	base := filepath.Join(projectPath, "workspace")
	agent1Worktree := filepath.Join(base, "worktrees", "agent-1")

	outsideDir := t.TempDir()
	sub := filepath.Join(outsideDir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	adminEntry := filepath.Join(base, ".git", "worktrees", "fake-entry")
	if err := os.WriteFile(filepath.Join(sub, ".git"), []byte("gitdir: "+adminEntry+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	lnk := filepath.Join(agent1Worktree, "lnk")
	if err := os.Symlink(outsideDir, lnk); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(lnk, "sub")

	if err := provision.RegisterSharer(base, "", "agent-5", marker, "agent-1"); err != nil {
		t.Fatalf("plant sharer marker: %v", err)
	}

	opts := &api.StartOptions{}
	ok, _, err := srv.tryProvisionWorktree(context.Background(), startContextInputs{
		Name: "agent-5", AgentID: "agent-5",
		ProjectID: "p1", ProjectSlug: "proj", ProjectPath: projectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: &api.GitCloneConfig{URL: bare, Branch: "main"}},
	}, opts, map[string]string{}, srv.runtime.Name())

	if ok {
		t.Error("expected ok=false when the sharer registry names a path reached through an intermediate symlink")
	}
	if err == nil {
		t.Fatal("expected an error rejecting the marker reached through the symlink")
	}
	if opts.Workspace != "" {
		t.Errorf("expected no workspace to be mounted, got %q", opts.Workspace)
	}
	if got, readErr := os.Readlink(lnk); readErr != nil || got != outsideDir {
		t.Errorf("expected the symlink to survive unchanged, got target=%q err=%v", got, readErr)
	}
}

// TestTryProvisionWorktree_SharerRegistryNonCanonicalPathRejected proves a
// sharer-registry marker is refused when its own text is not already the
// literal, canonical "worktrees/<name>" form, even when every resolved-path
// check on it would otherwise pass. A marker built as
// "worktrees/agent-1/lnk/../agent-1", where lnk is a symlink back to
// worktrees/agent-1, resolves physically (following the real filesystem,
// symlinks included) to the legitimate worktrees/agent-1 — so both
// EvalSymlinks-based checks accept it. But this exact string is what is
// stored and later consumed downstream by a purely lexical Clean (no
// symlink resolution), which resolves the same text to a different
// location, worktrees/agent-1/agent-1.
func TestTryProvisionWorktree_SharerRegistryNonCanonicalPathRejected(t *testing.T) {
	requireWorktreeGit(t)
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	bare := initBareRepoWithCommit(t)
	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	setUpAgent1SharedBase(t, srv, projectPath, bare)

	base := filepath.Join(projectPath, "workspace")
	agent1Worktree := filepath.Join(base, "worktrees", "agent-1")

	// A second worktree-shaped directory nested inside agent-1's own
	// worktree, reachable only through the lexical (not physical) reading
	// of the marker below.
	inner := filepath.Join(agent1Worktree, "agent-1")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	adminEntry := filepath.Join(base, ".git", "worktrees", "agent-1")
	if err := os.WriteFile(filepath.Join(inner, ".git"), []byte("gitdir: "+adminEntry+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// lnk resolves back to agent-1's own real worktree, so the marker below
	// physically resolves to a direct child (worktrees/agent-1) even though
	// its own text is not that direct-child form.
	lnk := filepath.Join(agent1Worktree, "lnk")
	if err := os.Symlink(agent1Worktree, lnk); err != nil {
		t.Fatal(err)
	}
	// filepath.Join would lexically clean "lnk/../agent-1" away before this
	// even reaches RegisterSharer, defeating the point of this test — the
	// exact literal text with "lnk/.." still in it is what gets stored, and
	// it is not resolved by anything until it is used downstream.
	marker := lnk + string(filepath.Separator) + ".." + string(filepath.Separator) + "agent-1"

	if err := provision.RegisterSharer(base, "", "agent-9", marker, "agent-1"); err != nil {
		t.Fatalf("plant sharer marker: %v", err)
	}

	opts := &api.StartOptions{}
	ok, _, err := srv.tryProvisionWorktree(context.Background(), startContextInputs{
		Name: "agent-9", AgentID: "agent-9",
		ProjectID: "p1", ProjectSlug: "proj", ProjectPath: projectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: &api.GitCloneConfig{URL: bare, Branch: "main"}},
	}, opts, map[string]string{}, srv.runtime.Name())

	if ok {
		t.Error("expected ok=false for a marker that is not already in canonical direct-child form")
	}
	if err == nil {
		t.Fatal("expected an error rejecting the non-canonical marker")
	}
	if opts.Workspace != "" {
		t.Errorf("expected no workspace to be mounted, got %q", opts.Workspace)
	}
}

// TestTryProvisionWorktree_SymlinkedProjectParentAccepted proves a project
// path reached through a symlinked ancestor directory — a supported,
// legitimate configuration — is still accepted and mounted: the final
// containment check resolves both the "worktrees" directory and the
// worktree path through that same ancestor symlink before comparing them,
// rather than comparing their un-resolved text.
func TestTryProvisionWorktree_SymlinkedProjectParentAccepted(t *testing.T) {
	requireWorktreeGit(t)
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	bare := initBareRepoWithCommit(t)
	realProjectDir := filepath.Join(t.TempDir(), "real-proj")
	if err := os.MkdirAll(realProjectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	projectPath := filepath.Join(t.TempDir(), "proj-link")
	if err := os.Symlink(realProjectDir, projectPath); err != nil {
		t.Fatal(err)
	}

	opts := &api.StartOptions{}
	ok, _, err := srv.tryProvisionWorktree(context.Background(), startContextInputs{
		Name: "agent-1", AgentID: "agent-1",
		ProjectID: "p1", ProjectSlug: "proj", ProjectPath: projectPath,
		WorkspaceMode: store.WorkspaceModeWorktreePerAgent,
		Config:        &CreateAgentConfig{GitClone: &api.GitCloneConfig{URL: bare, Branch: "main"}},
	}, opts, map[string]string{}, srv.runtime.Name())

	if err != nil {
		t.Fatalf("expected provisioning through a symlinked project parent to succeed, got err=%v", err)
	}
	if !ok {
		t.Fatal("expected ok=true for a symlinked project parent")
	}
	if opts.Workspace == "" {
		t.Fatal("expected a workspace to be mounted")
	}
}
