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

package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateWorkspaceSource_Empty(t *testing.T) {
	resolved, err := ValidateWorkspaceSource("", "/some/root")
	if err != nil {
		t.Errorf("expected nil error for empty source, got %v", err)
	}
	if resolved != "" {
		t.Errorf("expected empty resolved path for empty source, got %q", resolved)
	}
}

func TestValidateWorkspaceSource_Valid(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	root := filepath.Join(tmpHome, "projects", "proj1")
	tests := []struct {
		name   string
		source string
		root   string
	}{
		{name: "source equals root", source: root, root: root},
		{name: "source under root", source: filepath.Join(root, "workspace"), root: root},
		{name: "no root to check against", source: "/tmp/some/path", root: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ValidateWorkspaceSource(tt.source, tt.root); err != nil {
				t.Errorf("expected valid, got error: %v", err)
			}
		})
	}
}

func TestValidateWorkspaceSource_RejectsFilesystemRoot(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	for _, source := range []string{"/", "//", "/."} {
		t.Run(source, func(t *testing.T) {
			resolved, err := ValidateWorkspaceSource(source, "")
			if err == nil {
				t.Fatalf("expected error for source %q, got nil", source)
			}
			if resolved != "" {
				t.Errorf("expected empty resolved path on rejection, got %q", resolved)
			}
		})
	}
}

func TestValidateWorkspaceSource_RejectsHome(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	resolved, err := ValidateWorkspaceSource(tmpHome, "")
	if err == nil {
		t.Fatal("expected error when source equals $HOME, got nil")
	}
	if resolved != "" {
		t.Errorf("expected empty resolved path on rejection, got %q", resolved)
	}
}

func TestValidateWorkspaceSource_RejectsScionHomeDir(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	scionDir := filepath.Join(tmpHome, ".scion")
	tests := []string{
		scionDir,
		filepath.Join(scionDir, "project-configs", "proj1"),
		filepath.Join(scionDir, "settings.yaml"),
		filepath.Join(scionDir, "harness-configs", "claude"),
		filepath.Join(scionDir, "templates", "default"),
	}

	for _, source := range tests {
		t.Run(source, func(t *testing.T) {
			if _, err := ValidateWorkspaceSource(source, ""); err == nil {
				t.Fatalf("expected error for source %q under ~/.scion, got nil", source)
			}
		})
	}
}

// TestValidateWorkspaceSource_RootlessAcceptsScionWorkspaceSubtree and
// TestValidateWorkspaceSource_RootlessAcceptsScionProjectsSubtree cover the
// two named, workspace-bearing subtrees of ~/.scion that a rootless call
// site must still admit: the global project's own workspace
// (~/.scion/workspace, pkg/agent/provision.go's Case 3 "global" branch) and
// every hub-managed project's externalized directory
// (~/.scion/projects/<slug>/..., set as opts.Workspace in
// pkg/runtimebroker/handlers.go and start_context.go for hub-dispatched
// agents on a local-runtime broker). Without these, buildCommonRunArgs, the
// k8s runtime, and the broker's primary workspace lookup would refuse every
// legitimate workspace under ~/.scion, not just unsafe ones.
func TestValidateWorkspaceSource_RootlessAcceptsScionWorkspaceSubtree(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	globalWorkspace := filepath.Join(tmpHome, ".scion", "workspace")
	if err := os.MkdirAll(globalWorkspace, 0755); err != nil {
		t.Fatal(err)
	}

	if _, err := ValidateWorkspaceSource(globalWorkspace, ""); err != nil {
		t.Errorf("expected ~/.scion/workspace to be accepted at a rootless call site, got error: %v", err)
	}
}

func TestValidateWorkspaceSource_RootlessAcceptsScionProjectsSubtree(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	tests := []string{
		filepath.Join(tmpHome, ".scion", "projects", "my-project"),
		filepath.Join(tmpHome, ".scion", "projects", "my-project", "workspace"),
		filepath.Join(tmpHome, ".scion", "projects", "my-project", ".scion", "agents", "agent-1", "workspace"),
	}

	for _, source := range tests {
		t.Run(source, func(t *testing.T) {
			if err := os.MkdirAll(source, 0755); err != nil {
				t.Fatal(err)
			}
			if _, err := ValidateWorkspaceSource(source, ""); err != nil {
				t.Errorf("expected %q to be accepted at a rootless call site, got error: %v", source, err)
			}
		})
	}
}

// TestValidateWorkspaceSource_RootlessScionProjectsSubtreeRefusesNonWorkspacePaths
// covers the sensitive shapes a single hub-managed project's own directory
// can otherwise contain: an agent's home (credential-bearing, never a
// legitimate workspace source) and another, unrelated file. Both share a
// project directory with the allowed shapes
// (TestValidateWorkspaceSource_RootlessAcceptsScionProjectsSubtree), so
// admitting the whole projects/<slug> subtree by name alone -- rather than
// the specific shapes isAllowedProjectSubtree names -- would accept
// them at any rootless call site.
func TestValidateWorkspaceSource_RootlessScionProjectsSubtreeRefusesNonWorkspacePaths(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	agentHome := filepath.Join(tmpHome, ".scion", "projects", "my-project", ".scion", "agents", "agent-1", "home")
	if err := os.MkdirAll(agentHome, 0755); err != nil {
		t.Fatal(err)
	}
	settingsFile := filepath.Join(tmpHome, ".scion", "projects", "my-project", "settings.yaml")
	if err := os.MkdirAll(filepath.Dir(settingsFile), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settingsFile, nil, 0644); err != nil {
		t.Fatal(err)
	}

	for _, source := range []string{agentHome, settingsFile} {
		t.Run(source, func(t *testing.T) {
			if _, err := ValidateWorkspaceSource(source, ""); err == nil {
				t.Errorf("expected %q to be refused at a rootless call site, got nil", source)
			}
		})
	}
}

// TestValidateWorkspaceSource_RootlessAcceptsScionProjectsWorktreesSubtree
// covers the hub-native worktree-per-agent shared-base layout: the
// project's own ~/.scion/projects/<slug> directory is the shared git
// checkout, and each agent's worktree is a direct child of its "worktrees"
// subdirectory (isAllowedProjectSubtree's worktrees/<name> rule).
func TestValidateWorkspaceSource_RootlessAcceptsScionProjectsWorktreesSubtree(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	tests := []string{
		filepath.Join(tmpHome, ".scion", "projects", "my-project", "worktrees", "agent-1"),
		filepath.Join(tmpHome, ".scion", "projects", "my-project", "worktrees", "agent-1", "sub"),
	}

	for _, source := range tests {
		t.Run(source, func(t *testing.T) {
			if err := os.MkdirAll(source, 0755); err != nil {
				t.Fatal(err)
			}
			if _, err := ValidateWorkspaceSource(source, ""); err != nil {
				t.Errorf("expected %q to be accepted at a rootless call site, got error: %v", source, err)
			}
		})
	}
}

// TestValidateWorkspaceSource_RootlessScionProjectsWorktreesSubtreeRejectsBareWorktreesDir
// proves the worktrees/<name> rule requires a non-empty name segment: the
// "worktrees" directory itself (no name following it) must never be
// admitted as a workspace source, the same way the agents/<id>/workspace
// rule it is modeled on never admits "agents" alone.
func TestValidateWorkspaceSource_RootlessScionProjectsWorktreesSubtreeRejectsBareWorktreesDir(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	worktreesDir := filepath.Join(tmpHome, ".scion", "projects", "my-project", "worktrees")
	if err := os.MkdirAll(worktreesDir, 0755); err != nil {
		t.Fatal(err)
	}

	if _, err := ValidateWorkspaceSource(worktreesDir, ""); err == nil {
		t.Errorf("expected the bare worktrees directory %q to be refused, got nil", worktreesDir)
	}
}

// TestIsAllowedProjectSubtree_WorktreesRuleRejectsEmptyName is a direct unit
// test on isAllowedProjectSubtree itself (unexported, same package): proves
// the empty-name case is rejected at the classification function directly,
// not only indirectly through ValidateWorkspaceSource's other floors (which
// Clean away a trailing or doubled separator before this function ever sees
// the path, making that collapsed form otherwise unreachable through the
// public API).
func TestIsAllowedProjectSubtree_WorktreesRuleRejectsEmptyName(t *testing.T) {
	tests := []string{
		filepath.Join("p", "worktrees") + string(filepath.Separator),
		filepath.Join("p", "worktrees") + string(filepath.Separator) + string(filepath.Separator) + "x",
	}
	for _, rel := range tests {
		t.Run(rel, func(t *testing.T) {
			if isAllowedProjectSubtree(rel) {
				t.Errorf("isAllowedProjectSubtree(%q) = true, want false (empty worktrees name)", rel)
			}
		})
	}
}

// TestValidateWorkspaceSource_RootlessScionProjectsWorktreesSubtreeRejectsLookalikes
// is a table test covering paths shaped closely enough to worktrees/<name>
// to be worth pinning explicitly. Only the first two rows (worktreesX,
// worktrees-old) are refusals the worktrees/<name> rule itself must produce,
// by not matching a prefix or suffix of the "worktrees" literal; the
// remaining rows are refused by other rules (the .git/ and .scion/ nested
// cases) or only after filepath.Clean collapses the embedded ".." segments
// to a path that still doesn't match any allowed shape (the two traversal
// cases, built by string concatenation here, not filepath.Join, so the
// ".." segments reach the guard uncleaned, the way a caller-controlled
// string would). root is "" throughout, so none of these rejections are
// merely root containment catching what the allow-list would otherwise
// admit.
func TestValidateWorkspaceSource_RootlessScionProjectsWorktreesSubtreeRejectsLookalikes(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	sep := string(filepath.Separator)
	projectDir := filepath.Join(tmpHome, ".scion", "projects", "my-project")

	tests := []struct {
		name   string
		source string
	}{
		{
			name:   "worktreesX lookalike (suffix, not a separator)",
			source: filepath.Join(projectDir, "worktreesX", "a"),
		},
		{
			name:   "worktrees-old lookalike",
			source: filepath.Join(projectDir, "worktrees-old", "a"),
		},
		{
			name:   "worktrees nested under .git",
			source: filepath.Join(projectDir, ".git", "worktrees", "a"),
		},
		{
			name:   "worktrees nested under .scion",
			source: filepath.Join(projectDir, ".scion", "worktrees", "a"),
		},
		{
			name:   "dot-dot reaching outside worktrees/<name>, built uncleaned",
			source: projectDir + sep + "worktrees" + sep + "a" + sep + ".." + sep + ".." + sep + ".scion" + sep + "agents" + sep + "x" + sep + "home",
		},
		{
			name:   "dot-dot collapsing worktrees itself, built uncleaned",
			source: projectDir + sep + "worktrees" + sep + ".." + sep + "..",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.MkdirAll(filepath.Dir(tt.source), 0755); err != nil {
				t.Fatal(err)
			}
			if _, err := ValidateWorkspaceSource(tt.source, ""); err == nil {
				t.Errorf("expected %q to be refused at a rootless call site, got nil", tt.source)
			}
		})
	}
}

// TestValidateWorkspaceSource_RejectsSymlinkedWorktreesResolvingOutsideProjectRoot
// proves a worktrees/<name> entry that is lexically in the right place but
// resolves (via a symlink) outside the project's own root is still refused:
// the worktrees/<name> rule is purely lexical classification, not proof of
// physical location, so the existing root-containment and symlink
// resolution machinery (resolveForValidation) must still catch this.
func TestValidateWorkspaceSource_RejectsSymlinkedWorktreesResolvingOutsideProjectRoot(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	outside := filepath.Join(tmpHome, "outside-project")
	if err := os.MkdirAll(outside, 0755); err != nil {
		t.Fatal(err)
	}

	projectDir := filepath.Join(tmpHome, ".scion", "projects", "my-project")
	worktreesDir := filepath.Join(projectDir, "worktrees")
	if err := os.MkdirAll(worktreesDir, 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(worktreesDir, "agent-1")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	if _, err := ValidateWorkspaceSource(link, projectDir); err == nil {
		t.Errorf("expected a worktrees/<name> entry resolving outside the project root to be refused, got nil")
	}
}

// TestValidateWorkspaceSource_RootlessSymlinkedWorktreesResolvingToDisallowedScionHomePath
// is the rootless counterpart to
// TestValidateWorkspaceSource_RejectsSymlinkedWorktreesResolvingOutsideProjectRoot:
// with no root supplied, a symlinked worktrees/<name> entry cannot be caught
// by root containment at all (there is no root to contain it against), so
// this exercises the ~/.scion floor as the sole remaining guard. The link
// target is itself inside ~/.scion (a different project's agent home, never
// a legitimate workspace source), not just any outside-the-tree path, so
// this specifically proves the worktrees/<name> rule's lexical match does
// not shortcut the floor that governs everything under ~/.scion.
func TestValidateWorkspaceSource_RootlessSymlinkedWorktreesResolvingToDisallowedScionHomePath(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	disallowed := filepath.Join(tmpHome, ".scion", "projects", "other", ".scion", "agents", "a", "home")
	if err := os.MkdirAll(disallowed, 0755); err != nil {
		t.Fatal(err)
	}

	worktreesDir := filepath.Join(tmpHome, ".scion", "projects", "my-project", "worktrees")
	if err := os.MkdirAll(worktreesDir, 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(worktreesDir, "agent-1")
	if err := os.Symlink(disallowed, link); err != nil {
		t.Fatal(err)
	}

	if _, err := ValidateWorkspaceSource(link, ""); err == nil {
		t.Errorf("expected a rootless worktrees/<name> entry resolving to a disallowed ~/.scion path to be refused, got nil")
	}
}

// TestValidateWorkspaceSource_RootlessAcceptsScionProjectConfigsSubtree
// covers the one shape a hub-dispatched project's marker-resolved
// externalized directory legitimately produces
// (isAllowedProjectConfigsSubtree): <dir>/.scion/agents/<agent-id>/workspace,
// and anything under it.
func TestValidateWorkspaceSource_RootlessAcceptsScionProjectConfigsSubtree(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	tests := []string{
		filepath.Join(tmpHome, ".scion", "project-configs", "hub-slug__11111111", ".scion", "agents", "agent-1", "workspace"),
		filepath.Join(tmpHome, ".scion", "project-configs", "hub-slug__11111111", ".scion", "agents", "agent-1", "workspace", "sub"),
	}

	for _, source := range tests {
		t.Run(source, func(t *testing.T) {
			if err := os.MkdirAll(source, 0755); err != nil {
				t.Fatal(err)
			}
			if _, err := ValidateWorkspaceSource(source, ""); err != nil {
				t.Errorf("expected %q to be accepted at a rootless call site, got error: %v", source, err)
			}
		})
	}
}

// TestValidateWorkspaceSource_RootlessScionProjectConfigsSubtreeRefusesNonWorkspacePaths
// is the project-configs counterpart to
// TestValidateWorkspaceSource_RootlessScionProjectsSubtreeRefusesNonWorkspacePaths:
// unlike ~/.scion/projects/<slug>, the bare project-configs directory is
// never itself a workspace (it holds the project's externalized
// configuration, see isAllowedProjectConfigsSubtree), so every one of these
// must be refused, not just the sibling home directory.
func TestValidateWorkspaceSource_RootlessScionProjectConfigsSubtreeRefusesNonWorkspacePaths(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	dir := filepath.Join(tmpHome, ".scion", "project-configs", "hub-slug__11111111")
	tests := []string{
		filepath.Join(dir, ".scion", "agents", "agent-1", "home"),
		filepath.Join(dir, ".scion", "agents", "agent-1"),
		filepath.Join(dir, ".scion"),
		filepath.Join(dir, ".scion", "settings.yaml"),
		filepath.Join(dir, ".scion", "templates"),
		filepath.Join(dir, "workspace"),
		filepath.Join(dir, "agents", "agent-1", "workspace"),
		filepath.Join(dir, ".scion", "agents", "agent-1", "workspacex"),
		filepath.Join(dir, "a", "b", "workspace"),
	}

	for _, source := range tests {
		t.Run(source, func(t *testing.T) {
			if err := os.MkdirAll(source, 0755); err != nil {
				t.Fatal(err)
			}
			if _, err := ValidateWorkspaceSource(source, ""); err == nil {
				t.Errorf("expected %q to be refused at a rootless call site, got nil", source)
			}
		})
	}
}

// TestValidateAgentHomeSource_AcceptsRealAgentHomeShapes covers
// ValidateAgentHomeSource's own allow list (isScionHomeAllowedHomeSubtree):
// the three real shapes config.GetAgentHomePath produces for an agent's home
// directory under ~/.scion, one for each project layout that keeps it there.
// This is the positive-acceptance-set proof k8s_runtime.go's Sync() call
// site depends on (see TestSync_RejectsPersistedHomeDirPath's own comment
// for why Sync() itself can't exercise the positive case end to end).
func TestValidateAgentHomeSource_AcceptsRealAgentHomeShapes(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	tests := []string{
		filepath.Join(tmpHome, ".scion", "agents", "agent-1", "home"),
		filepath.Join(tmpHome, ".scion", "agents", "agent-1", "home", "sub"),
		filepath.Join(tmpHome, ".scion", "projects", "my-project", ".scion", "agents", "agent-1", "home"),
		filepath.Join(tmpHome, ".scion", "project-configs", "hub-slug__11111111", ".scion", "agents", "agent-1", "home"),
	}

	for _, source := range tests {
		t.Run(source, func(t *testing.T) {
			if err := os.MkdirAll(source, 0755); err != nil {
				t.Fatal(err)
			}
			if _, err := ValidateAgentHomeSource(source, ""); err != nil {
				t.Errorf("expected %q to be accepted at a rootless call site, got error: %v", source, err)
			}
		})
	}
}

// TestValidateAgentHomeSource_RefusesWorkspaceShapes confirms the two allow
// lists are disjoint in practice, not just by the doc comment's claim: every
// shape ValidateWorkspaceSource admits is refused by ValidateAgentHomeSource,
// and (the mirror case, covered by the existing
// TestValidateWorkspaceSource_RootlessScionProjectsSubtreeRefusesNonWorkspacePaths
// and its project-configs counterpart) every real agent home is refused by
// ValidateWorkspaceSource.
func TestValidateAgentHomeSource_RefusesWorkspaceShapes(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	tests := []string{
		filepath.Join(tmpHome, ".scion", "workspace"),
		filepath.Join(tmpHome, ".scion", "projects", "my-project"),
		filepath.Join(tmpHome, ".scion", "projects", "my-project", "workspace"),
		filepath.Join(tmpHome, ".scion", "projects", "my-project", ".scion", "agents", "agent-1", "workspace"),
		filepath.Join(tmpHome, ".scion", "project-configs", "hub-slug__11111111", ".scion", "agents", "agent-1", "workspace"),
	}

	for _, source := range tests {
		t.Run(source, func(t *testing.T) {
			if err := os.MkdirAll(source, 0755); err != nil {
				t.Fatal(err)
			}
			if _, err := ValidateAgentHomeSource(source, ""); err == nil {
				t.Errorf("expected %q (a workspace shape) to be refused by ValidateAgentHomeSource, got nil", source)
			}
		})
	}
}

// TestValidateAgentHomeSource_RefusesNearMissHomeShapes covers paths that
// share a prefix with a real agent-home shape but stop short of it, or
// extend past it without the required path separator: the bare
// ~/.scion/agents directory and an agent's own state directory under it
// (not its "home" leaf), a "home"-prefixed but distinct leaf name
// ("homework"), the bare externalized project-configs directory and its
// own ".scion" subdirectory and agents directory (none of which are any
// agent's home), and the same "home"-prefixed-but-distinct leaf name under
// a hub-managed project's own agents directory.
func TestValidateAgentHomeSource_RefusesNearMissHomeShapes(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	tests := []string{
		filepath.Join(tmpHome, ".scion", "agents"),
		filepath.Join(tmpHome, ".scion", "agents", "a"),
		filepath.Join(tmpHome, ".scion", "agents", "a", "workspace"),
		filepath.Join(tmpHome, ".scion", "agents", "a", "homework"),
		filepath.Join(tmpHome, ".scion", "project-configs", "hub-slug__11111111"),
		filepath.Join(tmpHome, ".scion", "project-configs", "hub-slug__11111111", ".scion"),
		filepath.Join(tmpHome, ".scion", "project-configs", "hub-slug__11111111", ".scion", "agents", "a"),
		filepath.Join(tmpHome, ".scion", "projects", "my-project", ".scion", "agents", "a", "home2"),
	}

	for _, source := range tests {
		t.Run(source, func(t *testing.T) {
			if err := os.MkdirAll(source, 0755); err != nil {
				t.Fatal(err)
			}
			if _, err := ValidateAgentHomeSource(source, ""); err == nil {
				t.Errorf("expected %q to be refused by ValidateAgentHomeSource, got nil", source)
			}
		})
	}
}

// TestValidateWorkspaceSource_RootlessScionProjectsItselfNotAllowed confirms
// ~/.scion/projects on its own is refused: it spans every hub-managed
// project on the machine, so admitting it would defeat the whole point of
// requiring a specific project directory below it. ~/.scion/workspace, by
// contrast, stays allowed as a leaf -- there is only one global project, so
// there is nothing further to require.
func TestValidateWorkspaceSource_RootlessScionProjectsItselfNotAllowed(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	projectsDir := filepath.Join(tmpHome, ".scion", "projects")
	if err := os.MkdirAll(projectsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateWorkspaceSource(projectsDir, ""); err == nil {
		t.Fatal("expected ~/.scion/projects on its own to be refused at a rootless call site, got nil")
	}
}

// TestValidateWorkspaceSource_RootlessScionHomeItselfNeverAllowed confirms
// ~/.scion itself is never on the allow list, even though it is the literal
// parent of both named allowed subtrees -- only named descendants of it are
// admitted.
func TestValidateWorkspaceSource_RootlessScionHomeItselfNeverAllowed(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	scionDir := filepath.Join(tmpHome, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateWorkspaceSource(scionDir, ""); err == nil {
		t.Fatal("expected ~/.scion itself to be refused at a rootless call site, got nil")
	}
}

func TestValidateWorkspaceSource_RejectsOutsideRoot(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	root := filepath.Join(tmpHome, "projects", "proj1")
	tests := []struct {
		name   string
		source string
	}{
		{name: "sibling directory", source: filepath.Join(tmpHome, "projects", "proj1-other")},
		{name: "parent of root", source: filepath.Join(tmpHome, "projects")},
		{name: "unrelated absolute path", source: "/var/lib/other"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ValidateWorkspaceSource(tt.source, root); err == nil {
				t.Fatalf("expected error for source %q outside root %q, got nil", tt.source, root)
			}
		})
	}
}

// TestValidateWorkspaceSource_RootUnderScionHomeDirIsAccepted covers the
// global project: its own workspace directory lives at
// ~/.scion/workspace, i.e. under the generally-forbidden ~/.scion tree.
// When that exact directory is passed as both source and root, it must
// still be accepted — ~/.scion/workspace is on the named allow list
// (isScionHomeAllowedSubtree), and containment against its own root agrees;
// the two are expected to agree here, not in tension.
func TestValidateWorkspaceSource_RootUnderScionHomeDirIsAccepted(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	globalProjectDir := filepath.Join(tmpHome, ".scion")
	globalWorkspace := filepath.Join(globalProjectDir, "workspace")

	// The root workspaceSourceRoots actually passes for the global project
	// is <projectDir>/workspace itself, never ~/.scion bare -- ~/.scion is
	// refused as a root unconditionally (see
	// TestValidateWorkspaceSource_ScionHomeFloorAppliesWithRootsSupplied).
	if _, err := ValidateWorkspaceSource(globalWorkspace, globalWorkspace); err != nil {
		t.Errorf("expected global project workspace to be accepted under its own root, got error: %v", err)
	}

	// A path under ~/.scion but NOT under the given root is still rejected.
	other := filepath.Join(globalProjectDir, "project-configs", "other-project")
	if _, err := ValidateWorkspaceSource(other, globalWorkspace); err == nil {
		t.Error("expected error for a ~/.scion path outside the given root, got nil")
	}
}

// TestValidateWorkspaceSource_ScionHomeFloorAppliesWithRootsSupplied covers
// the ~/.scion floor when roots are supplied: it must hold regardless,
// exactly like the '/' and $HOME floors above it. Without this, a root that
// itself resolves to ~/.scion or a non-allow-listed subtree of it —
// reachable, for example, when the project directory's own repo root is
// unusually ~/.scion itself (a dotfiles pattern), or when ~/.scion is
// admitted as a bonus root because it is the main worktree of a project
// living in a linked worktree, see isMainWorktreeOf — would let plain
// prefix/equality containment against that root decide instead, letting an
// unsafe source slip past the allow list (isScionHomeAllowedSubtree)
// entirely.
func TestValidateWorkspaceSource_ScionHomeFloorAppliesWithRootsSupplied(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	scionDir := filepath.Join(tmpHome, ".scion")
	legitimateRoot := filepath.Join(tmpHome, "some-project")
	if err := os.MkdirAll(legitimateRoot, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatal(err)
	}

	// source equal to ~/.scion, with an otherwise-legitimate root supplied
	// alongside it: still refused.
	if _, err := ValidateWorkspaceSource(scionDir, legitimateRoot); err == nil {
		t.Error("expected error when source equals ~/.scion even with a root supplied, got nil")
	}

	// root equal to ~/.scion: refused outright, the same as '/' or $HOME as
	// a root (TestValidateWorkspaceSource_UnusableSuppliedRootIsRejected
	// covers the '/' and $HOME cases; this is the ~/.scion counterpart). The
	// shape this guards is isMainWorktreeOf admitting ~/.scion as a bonus
	// root because it is the main worktree of a project living in a linked
	// worktree.
	if _, err := ValidateWorkspaceSource(legitimateRoot, scionDir); err == nil {
		t.Error("expected error when the supplied root is ~/.scion, got nil")
	}

	// A root that resolves to a non-allow-listed SUBDIRECTORY of ~/.scion,
	// not ~/.scion itself (here, ~/.scion/harness-configs): a source under
	// it must be in the named allow list regardless of which root admitted
	// it. A root check that only compares for exact equality against
	// ~/.scion, without also considering a subdirectory one level down, is
	// not enough on its own -- this proves the allow list is consulted even
	// when a root like this one would otherwise let containment decide.
	harnessConfigsDir := filepath.Join(scionDir, "harness-configs")
	if err := os.MkdirAll(harnessConfigsDir, 0755); err != nil {
		t.Fatal(err)
	}
	sourceUnderHarnessConfigs := filepath.Join(harnessConfigsDir, "claude")
	if err := os.MkdirAll(sourceUnderHarnessConfigs, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateWorkspaceSource(sourceUnderHarnessConfigs, harnessConfigsDir); err == nil {
		t.Error("expected error for a source under a non-allow-listed ~/.scion subdirectory used as its own root, got nil")
	}

	// An allowed subtree of ~/.scion (the global workspace) must still be
	// accepted when it is itself one of the supplied roots, even alongside
	// an unrelated bonus root -- the same multi-root shape as the templates
	// case above, but for a source the allow list actually names. The floor
	// added above must refuse unsafe ~/.scion paths without also starting to
	// refuse the ones the allow list names.
	globalWorkspace := filepath.Join(scionDir, "workspace")
	if err := os.MkdirAll(globalWorkspace, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateWorkspaceSource(globalWorkspace, legitimateRoot, globalWorkspace); err != nil {
		t.Errorf("expected ~/.scion/workspace to still be accepted alongside an unrelated bonus root, got error: %v", err)
	}

	// A hub-managed project's own agent-home directory, with the bare
	// project slug directory as root -- the real shape
	// pkg/runtimebroker/handlers.go's workspaceDir produces for a non-git,
	// GCS-bootstrapped project (filepath.Join(globalDir, "projects", slug),
	// no further subpath). Plain containment against that root would accept
	// this source; the ~/.scion floor, which runs before containment, must
	// still refuse it since an agent's home is not one of
	// isAllowedProjectSubtree's named shapes.
	projectSlugDir := filepath.Join(scionDir, "projects", "hub-project")
	agentHome := filepath.Join(projectSlugDir, ".scion", "agents", "agent-1", "home")
	if err := os.MkdirAll(agentHome, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateWorkspaceSource(agentHome, projectSlugDir); err == nil {
		t.Error("expected error for a hub-managed project's agent-home directory even with the bare project slug directory as root, got nil")
	}
}

// TestValidateWorkspaceSource_AcceptsExternalizedProjectRoot documents the
// two other legitimate non-git workspace sources so the shared
// validator keeps working for them once wired into the run.go choke point:
// an externalized project's settings.WorkspacePath (source == root exactly),
// and a non-git, in-repo project's fallback (project root == parent of the
// project's .scion directory).
func TestValidateWorkspaceSource_AcceptsExternalizedProjectRoot(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// settings.WorkspacePath: the externalized project's workspace *is* the
	// resolved root.
	externalRoot := filepath.Join(tmpHome, "external-workspaces", "proj1")
	if _, err := ValidateWorkspaceSource(externalRoot, externalRoot); err != nil {
		t.Errorf("expected externalized settings.WorkspacePath source to be accepted, got error: %v", err)
	}

	// filepath.Dir(projectDir) fallback: project root is the parent of the
	// project's .scion directory, and the workspace source equals it.
	projectRoot := filepath.Join(tmpHome, "my-project")
	if _, err := ValidateWorkspaceSource(projectRoot, projectRoot); err != nil {
		t.Errorf("expected in-repo non-git project root source to be accepted, got error: %v", err)
	}
}

// TestIsFilesystemRoot covers the filepath.Dir(p) == p check directly: a
// plain literal comparison against the single-separator string "/" would
// never match a Windows volume root such as `C:\`, which filepath.Dir
// returns unchanged, the same way it does for "/" on every other OS.
func TestIsFilesystemRoot(t *testing.T) {
	if !isFilesystemRoot(string(filepath.Separator)) {
		t.Errorf("isFilesystemRoot(%q) = false, want true", string(filepath.Separator))
	}
	if isFilesystemRoot(filepath.Join(string(filepath.Separator), "some-dir")) {
		t.Error("isFilesystemRoot of a non-root path = true, want false")
	}
}

// TestValidateWorkspaceSource_RejectsSymlinkToFilesystemRoot covers a source
// that is nominally inside an allowed root, but is actually a symlink
// resolving to '/'. The containment check must judge it by where it leads,
// not by its nominal location.
func TestValidateWorkspaceSource_RejectsSymlinkToFilesystemRoot(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	root := filepath.Join(tmpHome, "project")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(root, "workspace")
	if err := os.Symlink(string(filepath.Separator), link); err != nil {
		t.Fatal(err)
	}

	if _, err := ValidateWorkspaceSource(link, root); err == nil {
		t.Error("expected error for a symlink inside root resolving to '/', got nil")
	}
}

// TestValidateWorkspaceSource_RejectsSymlinkToHome is the $HOME sibling of
// TestValidateWorkspaceSource_RejectsSymlinkToFilesystemRoot.
func TestValidateWorkspaceSource_RejectsSymlinkToHome(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	root := filepath.Join(tmpHome, "project")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(root, "workspace")
	if err := os.Symlink(tmpHome, link); err != nil {
		t.Fatal(err)
	}

	if _, err := ValidateWorkspaceSource(link, root); err == nil {
		t.Error("expected error for a symlink inside root resolving to $HOME, got nil")
	}
}

// TestValidateWorkspaceSource_AcceptsSymlinkedRootThatStaysInBounds covers
// the opposite direction: the root itself is reached through a symlink, but
// both the symlink and the source resolve to the same real, in-bounds
// location. Resolving symlinks must not turn a legitimate, in-bounds source
// into a false rejection, and the caller must be handed back the resolved,
// symlink-free path — not the nominal, symlinked one — so that a bind mount
// or sync set up from the return value targets the real location directly.
func TestValidateWorkspaceSource_AcceptsSymlinkedRootThatStaysInBounds(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	realRoot := filepath.Join(tmpHome, "real-project")
	realWorkspace := filepath.Join(realRoot, "workspace")
	if err := os.MkdirAll(realWorkspace, 0755); err != nil {
		t.Fatal(err)
	}

	rootLink := filepath.Join(tmpHome, "project-link")
	if err := os.Symlink(realRoot, rootLink); err != nil {
		t.Fatal(err)
	}

	source := filepath.Join(rootLink, "workspace")
	resolved, err := ValidateWorkspaceSource(source, rootLink)
	if err != nil {
		t.Fatalf("expected a symlinked root that stays in bounds to be accepted, got error: %v", err)
	}

	wantResolved, evalErr := filepath.EvalSymlinks(realWorkspace)
	if evalErr != nil {
		t.Fatalf("EvalSymlinks(%q): %v", realWorkspace, evalErr)
	}
	if resolved != wantResolved {
		t.Errorf("resolved path = %q, want the real, symlink-free path %q (not the nominal symlinked source %q)", resolved, wantResolved, source)
	}
}

// TestValidateWorkspaceSource_ResolvesNotYetCreatedPathAgainstNearestAncestor
// documents the fallback for a source that hasn't been created yet: symlink
// resolution walks up to the nearest existing ancestor, resolves that, and
// rejoins the not-yet-created remainder, rather than failing just because
// the leaf doesn't exist.
func TestValidateWorkspaceSource_ResolvesNotYetCreatedPathAgainstNearestAncestor(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	realRoot := filepath.Join(tmpHome, "real-project")
	if err := os.MkdirAll(realRoot, 0755); err != nil {
		t.Fatal(err)
	}

	rootLink := filepath.Join(tmpHome, "project-link")
	if err := os.Symlink(realRoot, rootLink); err != nil {
		t.Fatal(err)
	}

	// "workspace" does not exist under either realRoot or rootLink yet.
	source := filepath.Join(rootLink, "workspace")
	resolved, err := ValidateWorkspaceSource(source, rootLink)
	if err != nil {
		t.Errorf("expected a not-yet-created in-bounds path to be accepted, got error: %v", err)
	}
	wantResolved := filepath.Join(realRoot, "workspace")
	if resolved != wantResolved {
		t.Errorf("resolved path = %q, want %q (real root + not-yet-created suffix)", resolved, wantResolved)
	}

	// The same not-yet-created leaf, but the symlinked ancestor actually
	// leads outside the given root, must still be rejected.
	outsideLink := filepath.Join(tmpHome, "outside")
	if err := os.MkdirAll(outsideLink, 0755); err != nil {
		t.Fatal(err)
	}
	outsideSource := filepath.Join(outsideLink, "not-yet-created")
	if _, err := ValidateWorkspaceSource(outsideSource, rootLink); err == nil {
		t.Error("expected a not-yet-created path outside root to be rejected, got nil")
	}
}

// TestValidateWorkspaceSource_RejectsRelativeSource covers relative sources:
// a relative source resolves relative to the current
// directory (filepath.Clean and filepath.EvalSymlinks both preserve
// relativity), so it is never equal to '/', $HOME, or a ~/.scion prefix --
// the whole floor was skipped by construction, regardless of what the
// current directory actually was. A relative source must be refused
// outright rather than silently admitted.
func TestValidateWorkspaceSource_RejectsRelativeSource(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(oldWd) }()
	if err := os.Chdir(tmpHome); err != nil {
		t.Fatal(err)
	}

	for _, source := range []string{".", "..", "templates", "projects/x"} {
		t.Run(source, func(t *testing.T) {
			if _, err := ValidateWorkspaceSource(source, ""); err == nil {
				t.Errorf("expected relative source %q to be refused, got nil", source)
			}
		})
	}
}

// TestValidateWorkspaceSource_RejectsRelativeRoot is the root-side sibling of
// TestValidateWorkspaceSource_RejectsRelativeSource: a relative root can
// never be meaningfully compared for containment either, and a caller
// should never be supplying one.
func TestValidateWorkspaceSource_RejectsRelativeRoot(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	source := filepath.Join(tmpHome, "project", "workspace")
	if err := os.MkdirAll(source, 0755); err != nil {
		t.Fatal(err)
	}

	if _, err := ValidateWorkspaceSource(source, "relative-root"); err == nil {
		t.Error("expected a relative root to be refused, got nil")
	}
}

// TestValidateWorkspaceSource_NoRootStillRejectsAbsoluteDenySet documents the
// degraded-but-safe behavior for call sites that cannot readily determine a
// per-project root: the '/', $HOME, and ~/.scion checks still apply even when
// root is empty.
func TestValidateWorkspaceSource_NoRootStillRejectsAbsoluteDenySet(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	if _, err := ValidateWorkspaceSource("/", ""); err == nil {
		t.Error("expected error for '/' even with empty root")
	}
	if _, err := ValidateWorkspaceSource(tmpHome, ""); err == nil {
		t.Error("expected error for $HOME even with empty root")
	}
	if _, err := ValidateWorkspaceSource(filepath.Join(tmpHome, ".scion"), ""); err == nil {
		t.Error("expected error for ~/.scion even with empty root")
	}
	// An unrelated path with no root configured is allowed through — the
	// containment check is opt-in per call site.
	if _, err := ValidateWorkspaceSource(filepath.Join(os.TempDir(), "unrelated"), ""); err != nil {
		t.Errorf("expected no error for unrelated path with empty root, got %v", err)
	}
}

// TestValidateWorkspaceSource_RejectsAncestorsOfHomeAndScionHome covers a
// source that is a strict ancestor of $HOME or ~/.scion, not merely equal to
// either: accepting such a source would still admit the whole subtree the
// equality checks exist to protect, since $HOME or ~/.scion sits underneath
// it. Covers the immediate parent, the grandparent, and the root itself.
func TestValidateWorkspaceSource_RejectsAncestorsOfHomeAndScionHome(t *testing.T) {
	tmpDir := t.TempDir()
	tmpHome := filepath.Join(tmpDir, "users", "me")
	if err := os.MkdirAll(tmpHome, 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmpHome)
	if err := os.MkdirAll(filepath.Join(tmpHome, ".scion"), 0755); err != nil {
		t.Fatal(err)
	}

	ancestors := []string{
		filepath.Join(tmpDir, "users"), // parent of $HOME
		tmpDir,                         // grandparent of $HOME
	}
	for _, source := range ancestors {
		t.Run(source, func(t *testing.T) {
			if _, err := ValidateWorkspaceSource(source, ""); err == nil {
				t.Errorf("expected %q (an ancestor of $HOME) to be refused, got nil", source)
			}
		})
	}

	// An unrelated sibling directory, not an ancestor of $HOME, is still
	// accepted at a rootless call site -- this floor must not over-refuse.
	sibling := filepath.Join(tmpDir, "users", "someone-else")
	if err := os.MkdirAll(sibling, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateWorkspaceSource(sibling, ""); err != nil {
		t.Errorf("expected unrelated sibling %q to be accepted, got error: %v", sibling, err)
	}
}

// TestValidateWorkspaceSource_RejectsAncestorsWithSymlinkedScionHome covers
// the same two floors as TestValidateWorkspaceSource_RejectsAncestorsOfHomeAndScionHome,
// but with ~/.scion symlinked to a location outside $HOME entirely (a real,
// supported layout — resolveForValidation follows it before either floor
// runs). With a real ~/.scion directly under $HOME, the $HOME-ancestor and
// ~/.scion-ancestor floors overlap completely: a source refused by one is
// always refused by the other too, so neither floor can be shown to matter
// on its own. Symlinking ~/.scion elsewhere separates them: a source that is
// only an ancestor of $HOME, not of the symlink target, exercises the
// $HOME-ancestor floor alone, and a source that is only an ancestor of the
// symlink target, not of $HOME, exercises the ~/.scion-ancestor floor alone.
func TestValidateWorkspaceSource_RejectsAncestorsWithSymlinkedScionHome(t *testing.T) {
	tmpDir := t.TempDir()
	tmpHome := filepath.Join(tmpDir, "users", "me")
	if err := os.MkdirAll(tmpHome, 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmpHome)

	scionData := filepath.Join(tmpDir, "data", "scion-data")
	if err := os.MkdirAll(scionData, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(scionData, filepath.Join(tmpHome, ".scion")); err != nil {
		t.Fatal(err)
	}

	homeAncestorOnly := filepath.Join(tmpDir, "users") // ancestor of $HOME, not of scionData
	scionAncestorOnly := filepath.Join(tmpDir, "data") // ancestor of scionData, not of $HOME

	if _, err := ValidateWorkspaceSource(homeAncestorOnly, ""); err == nil {
		t.Errorf("expected %q (an ancestor of $HOME only) to be refused, got nil", homeAncestorOnly)
	}
	if _, err := ValidateWorkspaceSource(scionAncestorOnly, ""); err == nil {
		t.Errorf("expected %q (an ancestor of the symlinked ~/.scion only) to be refused, got nil", scionAncestorOnly)
	}
}

// TestValidateWorkspaceSource_RejectsCriticalSystemPaths covers the
// fsutil.IsCriticalSystemPath floor: accepting one of these as a workspace
// source would mount or sync a whole system directory instead of one
// project's own files, regardless of whether a root was supplied.
func TestValidateWorkspaceSource_RejectsCriticalSystemPaths(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	for _, source := range []string{"/etc", "/usr", "/var"} {
		t.Run(source, func(t *testing.T) {
			if _, err := ValidateWorkspaceSource(source, ""); err == nil {
				t.Errorf("expected critical system path %q to be refused, got nil", source)
			}
		})
	}
}

// TestValidateWorkspaceSource_RejectsRootEqualToScionHomeWithAllowedSource
// covers the pathEqualFailSafe(resolvedRoot, scionHomeDir) check in the
// root-refusal loop: a root of exactly ~/.scion, paired with a source that
// the ~/.scion allow list itself admits, is the discriminating case for it
// -- plain containment alone would accept it, so only the explicit
// root-equality refusal catches it.
func TestValidateWorkspaceSource_RejectsRootEqualToScionHomeWithAllowedSource(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	scionHome := filepath.Join(tmpHome, ".scion")
	source := filepath.Join(scionHome, "projects", "p", "workspace")
	if err := os.MkdirAll(source, 0755); err != nil {
		t.Fatal(err)
	}

	if _, err := ValidateWorkspaceSource(source, scionHome); err == nil {
		t.Error("expected a root equal to ~/.scion to be refused even though the source is otherwise allow-listed, got nil")
	}
}

// TestValidateWorkspaceSource_UnusableSuppliedRootIsRejected covers a caller
// that supplies a root, but the root itself is one of the paths this
// function refuses as a source ('/' or $HOME). That must be refused outright
// as a misconfiguration, not silently treated as if no root had been
// supplied at all -- which would otherwise admit anything not on the fixed
// deny list.
func TestValidateWorkspaceSource_UnusableSuppliedRootIsRejected(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	source := filepath.Join(tmpHome, "some-dir")
	if err := os.MkdirAll(source, 0755); err != nil {
		t.Fatal(err)
	}

	// The '/' assertion below does not, on its own, prove the root loop's
	// isFilesystemRoot(resolvedRoot) check runs: the containment check
	// further down builds resolvedRoot+string(filepath.Separator), which for
	// root "/" is "//" -- a prefix no real, Clean-d path ever has -- so the
	// '/' case is also refused by containment alone, on Linux, independent
	// of isFilesystemRoot. This assertion alone does not distinguish the
	// two checks; TestIsFilesystemRoot covers isFilesystemRoot directly.
	// The $HOME assertion below exercises the root loop's separate
	// pathEqualFailSafe(resolvedRoot, cleanHome) check meaningfully, since
	// $HOME is not a filesystem root and so is not also caught by
	// containment's own "//"-shaped non-match.
	if _, err := ValidateWorkspaceSource(source, "/"); err == nil {
		t.Error("expected error when the supplied root is '/', got nil")
	}
	if _, err := ValidateWorkspaceSource(source, tmpHome); err == nil {
		t.Error("expected error when the supplied root is $HOME, got nil")
	}
}

// TestValidateWorkspaceSource_UnresolvableHomeFailsClosed covers the case
// where the home directory cannot be determined at all (e.g. HOME unset in a
// broker service unit): the $HOME and ~/.scion checks must not be silently
// skipped just because there is nothing to compare against.
func TestValidateWorkspaceSource_UnresolvableHomeFailsClosed(t *testing.T) {
	t.Setenv("HOME", "")

	if _, err := ValidateWorkspaceSource("/var/lib/some-workspace", ""); err == nil {
		t.Fatal("expected an unresolvable home directory to fail closed, got nil")
	}
}

// TestValidateWorkspaceSource_UnresolvableScionHomeFailsClosed is the
// ~/.scion counterpart to TestValidateWorkspaceSource_UnresolvableHomeFailsClosed:
// $HOME itself resolves fine, but ~/.scion is a broken (dangling) symlink, so
// resolveForValidation returns an error for it specifically -- not the
// "doesn't exist yet" case that same function already walks up through
// cleanly. A source elsewhere under $HOME, with a root that would otherwise
// admit it via plain containment, must still be refused: silently falling
// back to the unresolved, lexical ~/.scion join here would skip the floor
// instead of failing closed, exactly the gap a resolved-vs-lexical mismatch
// (e.g. a legitimately symlinked ~/.scion resolving successfully elsewhere)
// could otherwise open up.
func TestValidateWorkspaceSource_UnresolvableScionHomeFailsClosed(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	scionDir := filepath.Join(tmpHome, ".scion")
	if err := os.Symlink(filepath.Join(tmpHome, "nonexistent-target"), scionDir); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(tmpHome, "project")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "workspace")
	if err := os.MkdirAll(source, 0755); err != nil {
		t.Fatal(err)
	}

	if _, err := ValidateWorkspaceSource(source, root); err == nil {
		t.Fatal("expected a broken ~/.scion symlink to fail closed even for a source under an unrelated root, got nil")
	}
}

// TestValidateWorkspaceSource_MultipleRootsAcceptsEither covers a caller that
// has more than one legitimate root for a single source (for example a git
// repo root and a separately-verified worktree location): the source is
// accepted if it falls under any one of them.
func TestValidateWorkspaceSource_MultipleRootsAcceptsEither(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	rootA := filepath.Join(tmpHome, "root-a")
	rootB := filepath.Join(tmpHome, "root-b")
	sourceUnderB := filepath.Join(rootB, "workspace")
	if err := os.MkdirAll(sourceUnderB, 0755); err != nil {
		t.Fatal(err)
	}

	// sourceUnderB is not under rootA, but it is under rootB -- accepted.
	if _, err := ValidateWorkspaceSource(sourceUnderB, rootA, rootB); err != nil {
		t.Errorf("expected source under the second of two roots to be accepted, got error: %v", err)
	}

	// A source under neither root is still refused.
	elsewhere := filepath.Join(tmpHome, "elsewhere")
	if err := os.MkdirAll(elsewhere, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateWorkspaceSource(elsewhere, rootA, rootB); err == nil {
		t.Error("expected source under neither of two roots to be rejected, got nil")
	}
}

// TestResolveForValidation_RejectsDanglingSymlink covers a symlink that
// exists as a directory entry but whose target does not: EvalSymlinks fails
// with the same IsNotExist error it would return for a path that simply
// isn't there yet, so without an explicit check the ancestor walk would
// silently treat the dangling link's name as a "not yet created" path
// component and return a resolved path that still contains the link.
func TestResolveForValidation_RejectsDanglingSymlink(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	root := filepath.Join(tmpHome, "root")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}

	outside := filepath.Join(tmpHome, "outside")
	danglingTarget := filepath.Join(outside, "notyet")
	// outside/notyet is never created -- the symlink below is dangling.

	link := filepath.Join(root, "ws")
	if err := os.Symlink(danglingTarget, link); err != nil {
		t.Fatal(err)
	}

	if _, err := ValidateWorkspaceSource(link, root); err == nil {
		t.Fatal("expected a dangling symlink to be rejected, got nil")
	}
}

// TestPathEqualFailSafeCaseFold and TestPathIsOrUnderCaseFoldForOS cover the
// case-insensitive-filesystem floor gap directly, with the platform check
// factored out to a parameter so the comparison logic itself is provable on
// Linux CI, where the real, filesystem-backed gap (a default
// case-insensitive volume on macOS or Windows) cannot be reproduced, but
// the fold-or-not decision these two functions make is pure string logic,
// identical on every platform.
func TestPathEqualFailSafeCaseFold(t *testing.T) {
	home := "/Users/example"
	variant := "/Users/EXAMPLE"

	if !pathEqualFailSafeCaseFold(home, home, false) {
		t.Error("expected an exact match to count regardless of case-insensitivity")
	}
	if pathEqualFailSafeCaseFold(home, variant, false) {
		t.Error("expected a case-variant match to be refused when the filesystem is case-sensitive")
	}
	if !pathEqualFailSafeCaseFold(home, variant, true) {
		t.Error("expected a case-variant match to count when the filesystem is case-insensitive")
	}
	if pathEqualFailSafeCaseFold(home, "/Users/other", true) {
		t.Error("expected an unrelated path to never match, case-insensitive or not")
	}
}

func TestPathIsOrUnderCaseFoldForOS(t *testing.T) {
	scionHomeDir := "/Users/example/.scion"
	variantAncestor := "/Users/example/.SCION"
	variantDescendant := "/Users/example/.SCION/templates"

	if pathIsOrUnderCaseFoldForOS(variantAncestor, scionHomeDir, false) {
		t.Error("expected a case-variant ancestor match to be refused when the filesystem is case-sensitive")
	}
	if !pathIsOrUnderCaseFoldForOS(variantAncestor, scionHomeDir, true) {
		t.Error("expected a case-variant exact match to count when the filesystem is case-insensitive")
	}
	if !pathIsOrUnderCaseFoldForOS(variantDescendant, scionHomeDir, true) {
		t.Error("expected a case-variant descendant to count when the filesystem is case-insensitive")
	}
	if pathIsOrUnderCaseFoldForOS("/Users/example/other", scionHomeDir, true) {
		t.Error("expected an unrelated path to never match, case-insensitive or not")
	}
	// A prefix collision that is not actually an ancestor relationship (no
	// separator boundary) must not match: "/Users/example/.scion-backup" is
	// not under "/Users/example/.scion".
	if pathIsOrUnderCaseFoldForOS("/Users/example/.SCION-backup", scionHomeDir, true) {
		t.Error("expected a same-prefix sibling directory to not match as an ancestor relationship")
	}
}

// TestValidateWorkspaceSource_CaseFoldWiring is the validator-level
// counterpart to TestPathEqualFailSafeCaseFold and
// TestPathIsOrUnderCaseFoldForOS, which only prove the comparison logic
// itself: this proves ValidateWorkspaceSource actually calls it.
// caseInsensitiveFilesystem is a package variable precisely so a test can
// force the case-insensitive path on any host, including this package's
// own Linux CI, where the real platform default never exercises it.
func TestValidateWorkspaceSource_CaseFoldWiring(t *testing.T) {
	// t.TempDir()'s own generated leaf component is numeric (e.g. "001"),
	// which has no letter case to vary, so $HOME is placed one level below
	// a controlled, all-letters name instead.
	tmpDir := t.TempDir()
	tmpHome := filepath.Join(tmpDir, "home")
	if err := os.MkdirAll(tmpHome, 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmpHome)

	scionDir := filepath.Join(tmpHome, ".scion")
	variantSource := filepath.Join(tmpHome, strings.ToUpper(filepath.Base(scionDir)), "templates")
	if err := os.MkdirAll(variantSource, 0755); err != nil {
		t.Fatal(err)
	}

	// There are four fold call sites in ValidateWorkspaceSource:
	//   (a) pathIsOrUnderCaseFold(resolvedSource, scionHomeDir) -- the
	//       source-side ~/.scion floor, exercised via variantSource above;
	//   (b) pathEqualFailSafe(resolvedSource, cleanHome) -- the source-side
	//       $HOME floor, exercised below via variantHome as a source;
	//   (c) pathEqualFailSafe(resolvedRoot, cleanHome) -- the unusable-root
	//       check against $HOME, exercised below via sourceUnderVariantHome
	//       with variantHome as its root;
	//   (d) pathEqualFailSafe(resolvedRoot, scionHomeDir) -- the same
	//       unusable-root check against ~/.scion, targeted below via
	//       sourceUnderVariantScionDir with variantScionDir as its root.
	// A source *under* variantHome, not variantHome itself, isolates (c)
	// from (b): this source is never equal to cleanHome by any spelling,
	// only contained within the case-variant root under test.
	variantHome := filepath.Join(tmpDir, strings.ToUpper(filepath.Base(tmpHome)))
	if err := os.MkdirAll(variantHome, 0755); err != nil {
		t.Fatal(err)
	}
	sourceUnderVariantHome := filepath.Join(variantHome, "some-project")
	if err := os.MkdirAll(sourceUnderVariantHome, 0755); err != nil {
		t.Fatal(err)
	}

	// (d) cannot be isolated from (a) the same way: any source contained in
	// a case-variant ~/.scion root is itself fold-under ~/.scion, so it is
	// already refused by the source-side fold (a) before the root loop
	// runs. The assertions below using variantScionDir as a root therefore
	// check the outcome only -- they do not exercise (d) on its own.
	variantScionDir := filepath.Join(tmpHome, strings.ToUpper(filepath.Base(scionDir)))
	sourceUnderVariantScionDir := filepath.Join(variantScionDir, "some-project")
	if err := os.MkdirAll(sourceUnderVariantScionDir, 0755); err != nil {
		t.Fatal(err)
	}

	original := caseInsensitiveFilesystem
	t.Cleanup(func() { caseInsensitiveFilesystem = original })

	caseInsensitiveFilesystem = false
	if _, err := ValidateWorkspaceSource(variantSource, ""); err != nil {
		t.Errorf("expected a case-variant ~/.scion path to be treated as unrelated to ~/.scion when case-insensitive matching is off, got error: %v", err)
	}
	if _, err := ValidateWorkspaceSource(variantHome, ""); err != nil {
		t.Errorf("expected a case-variant $HOME source to be treated as unrelated to $HOME when case-insensitive matching is off, got error: %v", err)
	}
	if _, err := ValidateWorkspaceSource(sourceUnderVariantHome, variantHome); err != nil {
		t.Errorf("expected a case-variant $HOME root to be usable when case-insensitive matching is off, got error: %v", err)
	}
	if _, err := ValidateWorkspaceSource(sourceUnderVariantScionDir, variantScionDir); err != nil {
		t.Errorf("expected a case-variant ~/.scion root to be usable when case-insensitive matching is off, got error: %v", err)
	}

	caseInsensitiveFilesystem = true
	if _, err := ValidateWorkspaceSource(variantSource, ""); err == nil {
		t.Error("expected a case-variant ~/.scion path to be refused once case-insensitive matching is on, got nil")
	}
	if _, err := ValidateWorkspaceSource(variantHome, ""); err == nil {
		t.Error("expected a case-variant $HOME source to be refused once case-insensitive matching is on, got nil")
	}
	if _, err := ValidateWorkspaceSource(sourceUnderVariantHome, variantHome); err == nil {
		t.Error("expected a case-variant $HOME root to be refused as a misconfigured root once case-insensitive matching is on, got nil")
	}
	if _, err := ValidateWorkspaceSource(sourceUnderVariantScionDir, variantScionDir); err == nil {
		t.Error("expected a case-variant ~/.scion root to be refused as a misconfigured root once case-insensitive matching is on, got nil")
	}
}
