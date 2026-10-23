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

package agent

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

func TestTrustedProjectRepoRoot(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	setupGitRepo(t, mkdirAllT(t, repo))
	deep := mkdirAllT(t, filepath.Join(repo, "a", "b", ".scion"))
	if got := trustedProjectRepoRoot(deep); got != eval(t, repo) {
		t.Errorf("nested project: got %q, want %q", got, eval(t, repo))
	}

	link := filepath.Join(root, "link")
	if err := os.Symlink(deep, link); err != nil {
		t.Fatal(err)
	}
	if got := trustedProjectRepoRoot(link); got != eval(t, repo) {
		t.Errorf("symlinked project dir: got %q, want %q", got, eval(t, repo))
	}

	// Neither the environment nor the repository configuration is read.
	t.Run("ignores env and core.worktree", func(t *testing.T) {
		t.Setenv("GIT_DIR", filepath.Join(root, "elsewhere"))
		t.Setenv("GIT_WORK_TREE", root)
		gitCfg := filepath.Join(repo, ".git", "config")
		data, _ := os.ReadFile(gitCfg)
		mustWrite(t, gitCfg, string(data)+"[core]\n\tworktree = "+root+"\n")
		if got := trustedProjectRepoRoot(deep); got != eval(t, repo) {
			t.Errorf("with GIT_DIR, GIT_WORK_TREE and core.worktree set: got %q, want %q", got, eval(t, repo))
		}
	})

	gitfile := mkdirAllT(t, filepath.Join(root, "linked", ".scion"))
	mustWrite(t, filepath.Join(root, "linked", ".git"), "gitdir: "+filepath.Join(repo, ".git")+"\n")
	if got := trustedProjectRepoRoot(gitfile); got != "" {
		t.Errorf(".git file: got %q, want \"\"", got)
	}

	withCommondir := filepath.Join(root, "cd")
	setupGitRepo(t, mkdirAllT(t, withCommondir))
	mustWrite(t, filepath.Join(withCommondir, ".git", "commondir"), "../..\n")
	if got := trustedProjectRepoRoot(withCommondir); got != "" {
		t.Errorf(".git with commondir: got %q, want \"\"", got)
	}

	// A directory with no repository at or above it in the temp tree. t.TempDir
	// is not itself a git repo, so the walk reaches the filesystem root and
	// returns "".
	plain := mkdirAllT(t, filepath.Join(t.TempDir(), "plain"))
	if got := trustedProjectRepoRoot(plain); got != "" {
		t.Errorf("no repository: got %q, want \"\"", got)
	}
}

func mkdirAllT(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Provisioning stops when git reports a different repository root than the
// directory walk (here because core.worktree in the project's
// configuration names another directory).
func TestProvision_RefusesWhenRepoRootsDisagree(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	setupGitRepo(t, mkdirAllT(t, repo))
	other := mkdirAllT(t, filepath.Join(root, "other"))
	if out, err := exec.Command("git", "-C", repo, "config", "core.worktree", other).CombinedOutput(); err != nil {
		t.Fatalf("git config: %v\n%s", err, out)
	}
	if _, err := checkProvisionRepoRoot(filepath.Join(repo, ".scion")); err == nil {
		t.Error("checkProvisionRepoRoot: expected an error when git and the walk disagree")
	}
	if got, err := checkProvisionRepoRoot(mkdirAllT(t, filepath.Join(root, "ok", ".scion"))); err == nil {
		t.Errorf("project with no repository: got %q, want an error", got)
	}
}

// An agent whose record is missing falls back to the broker-known-path derivation on its
// next start (a missing record means the derivation governs); no backfill
// record is written.
func TestStart_MissingRecordUsesDerivation(t *testing.T) {
	f := newRepoRootMetadataFixture(t)
	agentDir := config.GetAgentDir(f.projectScionDir, f.agentName, false)
	if err := os.Remove(filepath.Join(agentDir, provisionedWorktreeStateFile)); err != nil {
		t.Fatal(err)
	}
	cfg := f.start(t)
	if got, want := cfg.RepoRoot, eval(t, f.projectDir); got != want {
		t.Errorf("restart: RepoRoot = %q, want the derived project root %q", got, want)
	}
	if rec, ok := readRepoRootRecord(agentDir); ok && rec.Source == repoRootSourceBackfill {
		t.Errorf("a backfill record was written on a missing-record start: %+v", rec)
	}
}

// A record that names a repository other than the project's derived root (here
// in the legacy no-source format) makes the start refuse (fail closed); the
// other repository is never mounted.
func TestStart_RecordNamingAnotherRepoRefused(t *testing.T) {
	f := newRepoRootMetadataFixture(t)
	agentDir := config.GetAgentDir(f.projectScionDir, f.agentName, false)
	mustWrite(t, filepath.Join(agentDir, provisionedWorktreeStateFile), `{"repoRoot":"`+eval(t, f.otherRepo)+`"}`)

	err := f.startErr(t)
	if err == nil {
		t.Fatalf("restart: want a refusal when the record names another repository; got RepoRoot=%q", f.captured.RepoRoot)
	}
	if !strings.Contains(err.Error(), "does not match the repository root") {
		t.Errorf("restart refused for an unexpected reason: %v", err)
	}
}

// TestReconcileRepoRoot covers the fail-closed intersection decision table
// (D = the derived root, R = the record). It is the single place the
// mismatch/unreadable policy lives.
func TestReconcileRepoRoot(t *testing.T) {
	errRefuse := errors.New("derivation refused")
	cases := []struct {
		name      string
		derived   string
		detectErr error
		rec       repoRootRecordState
		wantRoot  string
		wantErr   bool
	}{
		{name: "row1 D refuses, record present -> refuse", derived: "", detectErr: errRefuse, rec: repoRootRecordState{status: recordPresent, root: "/repo"}, wantErr: true},
		{name: "row1 D refuses, record unreadable -> refuse", derived: "", detectErr: errRefuse, rec: repoRootRecordState{status: recordUnreadable}, wantErr: true},
		{name: "row2 D plain, record present -> plain", derived: "", rec: repoRootRecordState{status: recordPresent, root: "/repo"}, wantRoot: ""},
		{name: "row2 D plain, record missing -> plain", derived: "", rec: repoRootRecordState{status: recordMissing}, wantRoot: ""},
		{name: "row2 D plain, record unreadable -> plain", derived: "", rec: repoRootRecordState{status: recordUnreadable}, wantRoot: ""},
		{name: "row3 missing record, D full root -> D", derived: "/repo", rec: repoRootRecordState{status: recordMissing}, wantRoot: "/repo"},
		{name: "row4 record equals D -> D", derived: "/repo", rec: repoRootRecordState{status: recordPresent, root: "/repo"}, wantRoot: "/repo"},
		{name: "row4 record mismatches D -> refuse", derived: "/repo", rec: repoRootRecordState{status: recordPresent, root: "/other"}, wantErr: true},
		{name: "row5 record unreadable, D full root -> refuse", derived: "/repo", rec: repoRootRecordState{status: recordUnreadable}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := reconcileRepoRoot("a", tc.derived, tc.detectErr, tc.rec)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want a refusal, got root %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantRoot {
				t.Errorf("root = %q, want %q", got, tc.wantRoot)
			}
		})
	}

	// Row 4 with aliased paths: a record root that resolves (through a symlink)
	// to the derived root is accepted, pinning the samePath use in reconcile.
	t.Run("row4 record aliases D -> D", func(t *testing.T) {
		root := t.TempDir()
		realRoot := mkdirAllT(t, filepath.Join(root, "repo"))
		alias := filepath.Join(root, "repo-link")
		if err := os.Symlink(realRoot, alias); err != nil {
			t.Fatal(err)
		}
		got, err := reconcileRepoRoot("a", realRoot, nil, repoRootRecordState{status: recordPresent, root: alias})
		if err != nil {
			t.Fatalf("aliased record equal to D: unexpected error: %v", err)
		}
		if got != realRoot {
			t.Errorf("aliased record equal to D: got %q, want %q", got, realRoot)
		}
	})
}

// TestReadRepoRootRecordState covers reading the on-disk record R: a missing
// file, a readable record (including a legacy record with no source), a corrupt
// or empty record (unreadable -> refuse), a non-regular record (a directory or a
// symlink: unreadable when outside the mounts, missing under a mount), and
// records under an agent mount, which are missing whatever their contents so an
// agent-reachable record can never force a root or a refusal. The broker ctx
// signal is not a record and is not read here.
func TestReadRepoRootRecordState(t *testing.T) {
	ws := t.TempDir()
	agentDir := t.TempDir()
	recPath := filepath.Join(agentDir, provisionedWorktreeStateFile)

	if rs := readRepoRootRecordState("a", agentDir, ws, nil); rs.status != recordMissing {
		t.Errorf("no file: got %+v, want missing", rs)
	}

	mustWrite(t, recPath, `{"repoRoot":"/repo","source":"provision"}`)
	if rs := readRepoRootRecordState("a", agentDir, ws, nil); rs.status != recordPresent || rs.root != "/repo" {
		t.Errorf("valid record: got %+v, want present /repo", rs)
	}

	mustWrite(t, recPath, `{"repoRoot":"/repo"}`)
	if rs := readRepoRootRecordState("a", agentDir, ws, nil); rs.status != recordPresent || rs.root != "/repo" {
		t.Errorf("legacy record without a source: got %+v, want present /repo", rs)
	}

	mustWrite(t, recPath, `{not json`)
	if rs := readRepoRootRecordState("a", agentDir, ws, nil); rs.status != recordUnreadable {
		t.Errorf("corrupt record: got %+v, want unreadable", rs)
	}

	mustWrite(t, recPath, `{"source":"provision"}`)
	if rs := readRepoRootRecordState("a", agentDir, ws, nil); rs.status != recordUnreadable {
		t.Errorf("record with an empty root: got %+v, want unreadable", rs)
	}

	// Records an agent could reach: agentDir sits under the workspace mount.
	// Guarded first, before parsing, so even a corrupt or empty-root record is
	// missing (never a refusal).
	root2 := t.TempDir()
	ws2 := mkdirAllT(t, filepath.Join(root2, "ws"))
	agentDir2 := mkdirAllT(t, filepath.Join(ws2, "agents", "a"))
	rec2Path := filepath.Join(agentDir2, provisionedWorktreeStateFile)
	for _, tc := range []struct {
		name    string
		content string
	}{
		{name: "valid under a mount", content: `{"repoRoot":"/repo","source":"provision"}`},
		{name: "corrupt under a mount", content: `{not json`},
		{name: "empty-root under a mount", content: `{"source":"provision"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mustWrite(t, rec2Path, tc.content)
			if rs := readRepoRootRecordState("a", agentDir2, ws2, nil); rs.status != recordMissing {
				t.Errorf("record under the workspace mount: got %+v, want missing (ignored)", rs)
			}
		})
	}

	// The record path is a directory: it exists, so the guard runs, and the
	// no-follow read then fails because a directory is not a regular file.
	t.Run("directory at the record path, plain -> unreadable", func(t *testing.T) {
		ad := t.TempDir()
		if err := os.Mkdir(filepath.Join(ad, provisionedWorktreeStateFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if rs := readRepoRootRecordState("a", ad, t.TempDir(), nil); rs.status != recordUnreadable {
			t.Errorf("directory at the record path, plain: got %+v, want unreadable", rs)
		}
	})
	t.Run("directory at the record path, under a mount -> missing", func(t *testing.T) {
		root := t.TempDir()
		ws3 := mkdirAllT(t, filepath.Join(root, "ws"))
		ad := mkdirAllT(t, filepath.Join(ws3, "agents", "a"))
		if err := os.Mkdir(filepath.Join(ad, provisionedWorktreeStateFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if rs := readRepoRootRecordState("a", ad, ws3, nil); rs.status != recordMissing {
			t.Errorf("directory at the record path, under a mount: got %+v, want missing", rs)
		}
	})

	// A symlink at the record path must not be followed. Outside the mounts
	// the no-follow read fails (unreadable); under a mount the guard makes it
	// missing before any open.
	t.Run("symlink at the record path, plain -> unreadable", func(t *testing.T) {
		ad := t.TempDir()
		target := filepath.Join(t.TempDir(), "elsewhere.json")
		mustWrite(t, target, `{"repoRoot":"/repo","source":"provision"}`)
		if err := os.Symlink(target, filepath.Join(ad, provisionedWorktreeStateFile)); err != nil {
			t.Fatal(err)
		}
		if rs := readRepoRootRecordState("a", ad, t.TempDir(), nil); rs.status != recordUnreadable {
			t.Errorf("symlink at the record path, plain: got %+v, want unreadable (no-follow)", rs)
		}
	})
	t.Run("symlink at the record path, under a mount -> missing", func(t *testing.T) {
		root := t.TempDir()
		ws3 := mkdirAllT(t, filepath.Join(root, "ws"))
		ad := mkdirAllT(t, filepath.Join(ws3, "agents", "a"))
		target := filepath.Join(t.TempDir(), "elsewhere.json")
		mustWrite(t, target, `{"repoRoot":"/repo","source":"provision"}`)
		if err := os.Symlink(target, filepath.Join(ad, provisionedWorktreeStateFile)); err != nil {
			t.Fatal(err)
		}
		if rs := readRepoRootRecordState("a", ad, ws3, nil); rs.status != recordMissing {
			t.Errorf("symlink at the record path, under a mount: got %+v, want missing", rs)
		}
	})

	// agentDir is itself a regular file, so joining the record name onto it
	// gives a non-not-exist Lstat error (ENOTDIR): unreadable outside the
	// mounts, missing under a mount.
	t.Run("agentDir is a file, plain -> unreadable", func(t *testing.T) {
		agentFile := filepath.Join(t.TempDir(), "notadir")
		mustWrite(t, agentFile, "x")
		if rs := readRepoRootRecordState("a", agentFile, t.TempDir(), nil); rs.status != recordUnreadable {
			t.Errorf("agentDir is a file, plain: got %+v, want unreadable", rs)
		}
	})
	t.Run("agentDir is a file, under a mount -> missing", func(t *testing.T) {
		root := t.TempDir()
		ws3 := mkdirAllT(t, filepath.Join(root, "ws"))
		mkdirAllT(t, filepath.Join(ws3, "agents"))
		agentFile := filepath.Join(ws3, "agents", "notadir")
		mustWrite(t, agentFile, "x")
		if rs := readRepoRootRecordState("a", agentFile, ws3, nil); rs.status != recordMissing {
			t.Errorf("agentDir is a file, under a mount: got %+v, want missing", rs)
		}
	})
}

// Behaviour change: a project whose checkout is a linked worktree (its
// top-level .git is a gitfile, not a directory) is refused at provision,
// because the repository root cannot be identified without reading git
// metadata the container could edit. detectRepoRoot used to accept it.
func TestProvision_RefusesLinkedWorktreeProject(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	root := t.TempDir()
	base := filepath.Join(root, "base")
	setupGitRepo(t, mkdirAllT(t, base))
	linked := filepath.Join(root, "linked")
	if out, err := exec.Command("git", "-C", base, "worktree", "add", "-b", "feature", linked).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}
	// <linked>/.git is a gitfile, so trustedProjectRepoRoot stops and the
	// cross-check refuses.
	_, err := checkProvisionRepoRoot(filepath.Join(linked, ".scion"))
	if err == nil {
		t.Fatal("checkProvisionRepoRoot: expected a refusal for a linked-worktree checkout")
	}
	for _, want := range []string{"interim", "main checkout", "submodule"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal message %q does not mention %q", err.Error(), want)
		}
	}
	if got := trustedProjectRepoRoot(filepath.Join(linked, ".scion")); got != "" {
		t.Errorf("trustedProjectRepoRoot of a linked worktree = %q, want \"\"", got)
	}
}

// An explicit workspace that is the project's .scion directory, or inside
// .scion/agents, is refused at provision; the hub-native base's
// .scion/workspace is still allowed.
func TestRefuseWorkspaceInAgentsDir(t *testing.T) {
	root := t.TempDir()
	scion := mkdirAllT(t, filepath.Join(root, "proj", ".scion"))
	agents := mkdirAllT(t, filepath.Join(scion, "agents"))
	agentDir := mkdirAllT(t, filepath.Join(agents, "a"))
	wtWorkspace := mkdirAllT(t, filepath.Join(scion, "workspace", "worktrees", "a"))
	outside := mkdirAllT(t, filepath.Join(root, "proj", "work"))
	// Refused: the .scion directory itself, the agents tree, and below it.
	for _, ws := range []string{scion, agents, agentDir} {
		if err := refuseWorkspaceInAgentsDir(scion, ws); err == nil {
			t.Errorf("refuseWorkspaceInAgentsDir(%q): want a refusal", ws)
		}
	}
	// The parent spelling of projectDir (the project root, which the function
	// turns into <root>/.scion) refuses the resolved .scion the same way.
	if err := refuseWorkspaceInAgentsDir(filepath.Dir(scion), scion); err == nil {
		t.Error("refuseWorkspaceInAgentsDir(projectRoot, .scion): want a refusal")
	}
	// Allowed: the hub-native .scion/workspace worktree, and a workspace
	// outside .scion entirely.
	for _, ws := range []string{wtWorkspace, outside} {
		if err := refuseWorkspaceInAgentsDir(scion, ws); err != nil {
			t.Errorf("refuseWorkspaceInAgentsDir(%q) = %v, want nil", ws, err)
		}
	}
	// The project directory given as the parent (the project root, not
	// .scion) is handled too.
	if err := refuseWorkspaceInAgentsDir(filepath.Dir(scion), agentDir); err == nil {
		t.Error("refuseWorkspaceInAgentsDir(projectRoot, agentDir): want a refusal")
	}
	// A symlink into the agents tree is caught (both sides resolve).
	link := filepath.Join(root, "link-to-agents")
	if err := os.Symlink(agentDir, link); err != nil {
		t.Fatal(err)
	}
	if err := refuseWorkspaceInAgentsDir(scion, link); err == nil {
		t.Error("refuseWorkspaceInAgentsDir(symlink into agents): want a refusal")
	}

	// A project whose .scion directory is itself reached through a symlink
	// (the projectDir passed in is named .scion but is a link): both the
	// ==.scion and the agents-tree checks still fire (both sides resolve).
	realProj := mkdirAllT(t, filepath.Join(root, "real", ".scion"))
	realAgentDir := mkdirAllT(t, filepath.Join(realProj, "agents", "x"))
	linkParent := mkdirAllT(t, filepath.Join(root, "linked"))
	scionLink := filepath.Join(linkParent, ".scion")
	if err := os.Symlink(realProj, scionLink); err != nil {
		t.Fatal(err)
	}
	if err := refuseWorkspaceInAgentsDir(scionLink, scionLink); err == nil {
		t.Error("refuseWorkspaceInAgentsDir(symlinked .scion, itself): want a refusal")
	}
	agentsLink := filepath.Join(root, "link-to-real-agent")
	if err := os.Symlink(realAgentDir, agentsLink); err != nil {
		t.Fatal(err)
	}
	if err := refuseWorkspaceInAgentsDir(scionLink, agentsLink); err == nil {
		t.Error("refuseWorkspaceInAgentsDir(symlinked .scion, symlinked agent dir): want a refusal")
	}
}

// refuseWorkspaceInAgentsDir fails closed: when resolving the project's
// .scion directory, or its agents directory, fails for a reason other than
// not-exist (here EACCES from an unsearchable parent), the workspace is
// refused rather than allowed.
func TestRefuseWorkspaceInAgentsDir_FailClosedOnResolveError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-bit denial does not apply to root")
	}
	// Branch 1: resolving .scion fails. The project directory is made
	// unsearchable, so EvalSymlinks(<project>/.scion) returns EACCES. The
	// workspace itself lives elsewhere and resolves cleanly.
	t.Run("scion", func(t *testing.T) {
		root := t.TempDir()
		proj := mkdirAllT(t, filepath.Join(root, "proj"))
		mkdirAllT(t, filepath.Join(proj, ".scion"))
		ws := mkdirAllT(t, filepath.Join(root, "work"))
		if err := os.Chmod(proj, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(proj, 0o755) })
		err := refuseWorkspaceInAgentsDir(proj, ws)
		if err == nil {
			t.Fatal("refuseWorkspaceInAgentsDir with an unreadable .scion: want a refusal")
		}
		if !strings.Contains(err.Error(), "resolve project .scion") {
			t.Errorf("refuseWorkspaceInAgentsDir refused for an unexpected reason: %v", err)
		}
	})
	// Branch 2: .scion resolves, but resolving .scion/agents fails because
	// .scion is unsearchable.
	t.Run("agents", func(t *testing.T) {
		root := t.TempDir()
		proj := mkdirAllT(t, filepath.Join(root, "proj"))
		scion := mkdirAllT(t, filepath.Join(proj, ".scion"))
		mkdirAllT(t, filepath.Join(scion, "agents"))
		ws := mkdirAllT(t, filepath.Join(root, "work"))
		if err := os.Chmod(scion, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(scion, 0o755) })
		err := refuseWorkspaceInAgentsDir(proj, ws)
		if err == nil {
			t.Fatal("refuseWorkspaceInAgentsDir with an unreadable agents dir: want a refusal")
		}
		if !strings.Contains(err.Error(), "resolve project agents directory") {
			t.Errorf("refuseWorkspaceInAgentsDir refused for an unexpected reason: %v", err)
		}
	})
}

// A provision record naming another repository is not used (its root does
// not match the trusted walk from the project), so the resolved RepoRoot
// is empty.
func TestResolveStartRepoRoot_RejectsRecordNotMatchingProject(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	setupGitRepo(t, mkdirAllT(t, repo))
	other := filepath.Join(root, "other")
	setupGitRepo(t, mkdirAllT(t, other))
	projectDir := mkdirAllT(t, filepath.Join(repo, ".scion"))
	agentDir := mkdirAllT(t, filepath.Join(projectDir, "agents", "a"))
	ws := mkdirAllT(t, filepath.Join(agentDir, "workspace"))

	// A provision record naming another repository. The project's derived root
	// is this repo, so the record does not equal D and the start is refused.
	if err := writeRepoRootRecord(agentDir, eval(t, other), repoRootSourceProvision, nil); err != nil {
		t.Fatal(err)
	}
	in := startRepoRootInput{agentName: "a", agentDir: agentDir, projectDir: projectDir, workspace: ws}
	_, _, _, err := resolveStartRepoRoot(in)
	if err == nil {
		t.Fatal("resolveStartRepoRoot: want a refusal when the record names a repository other than the project's derived root")
	}
	if !strings.Contains(err.Error(), "does not match the repository root") {
		t.Errorf("resolveStartRepoRoot refused for an unexpected reason: %v", err)
	}
}

// The provision refusal is root-only. A project whose root is a normal
// repository still provisions even when it contains a submodule (whose own
// .git is a gitfile), because trustedProjectRepoRoot walks up from the
// project directory and stops at the root's .git directory, above any
// contained submodule.
func TestProvision_ContainingSubmoduleStillProvisions(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	setupGitRepo(t, mkdirAllT(t, repo))
	// A submodule-like nested checkout: a directory whose .git is a gitfile.
	sub := mkdirAllT(t, filepath.Join(repo, "vendor", "sub"))
	mustWrite(t, filepath.Join(sub, ".git"), "gitdir: "+filepath.Join(repo, ".git", "modules", "sub")+"\n")
	projectDir := mkdirAllT(t, filepath.Join(repo, ".scion"))

	got, err := checkProvisionRepoRoot(projectDir)
	if err != nil {
		t.Fatalf("checkProvisionRepoRoot refused a project that merely contains a submodule: %v", err)
	}
	if got != eval(t, repo) {
		t.Errorf("checkProvisionRepoRoot = %q, want the project root %q", got, eval(t, repo))
	}
}

// TestStart_CorruptRecordRefused is row 5 at Start level: a corrupt on-disk
// record outside every agent mount refuses the start.
func TestStart_CorruptRecordRefused(t *testing.T) {
	f := newRepoRootMetadataFixture(t)
	agentDir := config.GetAgentDir(f.projectScionDir, f.agentName, false)
	mustWrite(t, filepath.Join(agentDir, provisionedWorktreeStateFile), `{not json`)

	err := f.startErr(t)
	if err == nil {
		t.Fatalf("restart: want a refusal for a corrupt record; got RepoRoot=%q", f.captured.RepoRoot)
	}
	if !strings.Contains(err.Error(), "could not be read") {
		t.Errorf("restart refused for an unexpected reason: %v", err)
	}
}

// TestResolveStartRepoRoot_CtxIsCandidateNotRecord checks that the broker ctx
// signal is the candidate that feeds the derived root, not a record R. With ctx
// set but not validating as the workspace's base and no on-disk record, the
// derivation falls to detectRepoRoot and governs (as on main); ctx is never
// compared as a record, so the start is not refused.
func TestResolveStartRepoRoot_CtxIsCandidateNotRecord(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	root := t.TempDir()
	base := filepath.Join(root, "base")
	setupGitRepo(t, mkdirAllT(t, base))
	ws := createRealWorktree(t, base, "a")
	projectDir := mkdirAllT(t, filepath.Join(base, ".scion"))
	agentDir := mkdirAllT(t, filepath.Join(projectDir, "agents", "a"))

	in := startRepoRootInput{
		agentName:   "a",
		agentDir:    agentDir,
		projectDir:  projectDir,
		workspace:   ws,
		ctxRepoRoot: filepath.Join(root, "unrelated"),
	}
	got, rec, _, err := resolveStartRepoRoot(in)
	if err != nil {
		t.Fatalf("unexpected refusal: %v (ctx must be a candidate, not a record)", err)
	}
	if got != eval(t, base) {
		t.Errorf("RepoRoot = %q, want the detectRepoRoot fallback %q", got, eval(t, base))
	}
	if rec.status != recordMissing {
		t.Errorf("record = %+v, want missing (ctx is not a record)", rec)
	}
}

// TestResolveStartRepoRoot_CtxWithDifferentRecordRefuses: a fresh, validating ctx and a readable on-disk record (outside every mount) that
// names a different root. ctx feeds the derived root (D = base); the record is R
// and does not equal D, so the start is refused (row 4). ctx does not override a
// disagreeing record.
func TestResolveStartRepoRoot_CtxWithDifferentRecordRefuses(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	root := t.TempDir()
	base := filepath.Join(root, "base")
	setupGitRepo(t, mkdirAllT(t, base))
	ws := createRealWorktree(t, base, "a")
	other := filepath.Join(root, "other")
	setupGitRepo(t, mkdirAllT(t, other))
	projectDir := mkdirAllT(t, filepath.Join(base, ".scion"))
	agentDir := mkdirAllT(t, filepath.Join(root, "state", "a"))

	// A record naming another root, outside every agent mount.
	if err := writeRepoRootRecord(agentDir, eval(t, other), repoRootSourceProvision, nil); err != nil {
		t.Fatal(err)
	}
	in := startRepoRootInput{
		agentName:   "a",
		agentDir:    agentDir,
		projectDir:  projectDir,
		workspace:   ws,
		ctxRepoRoot: eval(t, base),
	}
	_, _, _, err := resolveStartRepoRoot(in)
	if err == nil {
		t.Fatal("want a refusal: ctx validates as the derived root but the record names another root")
	}
	if !strings.Contains(err.Error(), "does not match the repository root") {
		t.Errorf("refused for an unexpected reason: %v", err)
	}
}
