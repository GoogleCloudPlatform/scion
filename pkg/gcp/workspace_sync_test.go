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

package gcp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/rclone/rclone/fs/filter"
)

// The tests below run uploadWorkspace and downloadWorkspace, the bodies of
// SyncWorkspaceToGCS, SyncWorkspaceFromGCS and SyncWorkspaceContentFromGCS,
// with a local directory in place of the bucket.

// workspaceSyncCorpus is a set of workspace-relative file paths covering
// every class and every rule of the workspace sync list.
var workspaceSyncCorpus = []string{
	"README.md", "src/main.go", ".gitignore", ".gitmodules", ".github/workflows/ci.yml",
	"docs/.gitkeep", "a.git/x", "x.scion", "scion/x", "git/HEAD", "refs/heads/main",
	".git", ".git/HEAD", ".git/packed-refs", ".git/index", ".git/config", ".git/config.worktree",
	".git/description", ".git/FETCH_HEAD", ".git/ORIG_HEAD", ".git/COMMIT_EDITMSG",
	".git/refs/heads/main", ".git/refs/heads/feature/x", ".git/refs/tags/v1", ".git/refs/remotes/origin/main",
	".git/objects/ab/cdef0123", ".git/objects/pack/pack-1.pack", ".git/objects/pack/pack-1.idx",
	".git/objects/info/alternates", ".git/objects/info/http-alternates", ".git/objects/info/packs",
	".git/hooks/pre-commit", ".git/hooks/post-checkout", ".git/info/exclude", ".git/info/attributes",
	".git/modules/sub/config", ".git/modules/sub/hooks/pre-commit", ".git/modules/sub/HEAD",
	".git/worktrees/wt/gitdir", ".git/logs/HEAD", ".git/logs/refs/heads/main", ".git/lfs/objects/x",
	".git/refs/heads/.git", ".git/objects/.git/config", ".git/refs/.scion/x", ".git/objects/.DS_Store",
	"sub/.git", "sub/.git/config", "sub/.git/HEAD", "sub/.git/refs/heads/main", "vendor/lib/.git/hooks/pre-commit",
	".scion", ".scion/settings.yaml", ".scion/project-id", "sub/.scion", "sub/.scion/settings.yaml",
	"a/b/.scion/templates/t/scion-agent.yaml", ".DS_Store", "src/.DS_Store",
	// Case variants: a case-insensitive receiver treats them as the real
	// entry.
	".GIT", ".Git/config", ".GIT/HEAD", ".GIT/refs/heads/main", "sub/.GIT/hooks/pre-commit", "sub/.Git",
	".SCION/settings.yaml", ".Scion", "proj/.sCiOn/settings.yaml", ".ds_store", "x/.DS_STORE",
	".git/objects/INFO/alternates", ".git/objects/Info/http-alternates", ".git/OBJECTS/ab/cd",
	".git/Refs/heads/main", ".git/head", ".git/Index", ".git/HOOKS/pre-commit", ".git/Config",
	".git/refs/heads/.GIT", ".git/refs/heads/.Scion/x", ".git/objects/.Ds_Store", "Git/HEAD", "SCION/x",
	// NTFS and HFS+ variants: trailing dots and spaces, streams, 8.3 short
	// names, ignorable characters.
	".git.", ".git./config", ".git /hooks/pre-commit", ".git::$INDEX_ALLOCATION/config", "git~1/config", "GIT~2",
	"sub/git~1/hooks/x", "\ufeff.git/config", ".g\u200cit/config", ".scion.", "proj/.scion /x", "SCION~1/settings.yaml",
	"proj/sc1a2b~1/x", "proj/.s\u206acion/x", ".\u017fcion/x", ".DS_Store.", ".git/objects/info./alternates",
	".git/objects/INFO~1", ".git/objects/i\u200cnfo/alternates", ".git/objects/info:x", ".git/refs/heads/git~1",
	".git/HEAD.", ".git/shallow", ".git/refs/heads/main.lock", ".git/refs/heads/x.LOCK", ".git/packed-refs.lock",
	".git/HEAD.lock", ".git/index.lock", ".git/objects/pack/p.lock", ".git/refs/heads/lockstep", "src/file.lock", ".gitignore", ".gitattributes", "git~1.txt", ".git.bak/x", ".scionrc", "information/x",
}

// TestWorkspaceSyncRcloneDirectoryDecisions checks the directory side of
// the rclone rules: the git data directories are entered, and nothing is
// passed below a directory the classifier never syncs.
func TestWorkspaceSyncRcloneDirectoryDecisions(t *testing.T) {
	enter := map[syncFilter][]string{
		filterWorkspaceContent:   {"src", "a/b"},
		filterWorkspaceGitState:  {".git", ".git/refs", ".git/refs/heads"},
		filterWorkspaceGitObject: {".git", ".git/objects", ".git/objects/pack"},
	}
	for f, dirs := range enter {
		ctx, err := filteredContext(context.Background(), f)
		if err != nil {
			t.Fatal(err)
		}
		includeDir := filter.GetConfig(ctx).IncludeDirectory(ctx, nil)
		for _, d := range dirs {
			if ok, err := includeDir(d); err != nil || !ok {
				t.Errorf("filter %d: directory %q not entered (err %v)", f, d, err)
			}
		}
	}
	never := []string{".GIT", ".git.", "git~1", "sub/.git", ".scion", "proj/.Scion", "SCION~1", ".git/objects/info", ".git/objects/INFO", ".git/hooks", ".git/modules/m"}
	for _, f := range []syncFilter{filterWorkspaceContent, filterWorkspaceGitState, filterWorkspaceGitObject} {
		ctx, err := filteredContext(context.Background(), f)
		if err != nil {
			t.Fatal(err)
		}
		fi := filter.GetConfig(ctx)
		for _, d := range never {
			for _, below := range []string{"/config", "/hooks/pre-commit", "/x/y"} {
				if fi.IncludeRemote(d + below) {
					t.Errorf("filter %d passes %q below never-synced %q", f, d+below, d)
				}
			}
		}
	}
}

// TestWorkspaceSyncRcloneRulesParity checks that the rclone rules the
// workspace transfers run with pass exactly what
// transfer.ClassifyWorkspaceSyncPath assigns to each class.
func TestWorkspaceSyncRcloneRulesParity(t *testing.T) {
	for _, tc := range []struct {
		f    syncFilter
		want func(rel string) bool
	}{
		{filterWorkspaceContent, func(rel string) bool { return transfer.IsWorkspaceSyncContent(rel, false) }},
		{filterWorkspaceGitState, func(rel string) bool { return transfer.IsWorkspaceGitState(rel, false) }},
		{filterWorkspaceGitObject, func(rel string) bool { return transfer.IsWorkspaceGitObject(rel, false) }},
	} {
		ctx, err := filteredContext(context.Background(), tc.f)
		if err != nil {
			t.Fatal(err)
		}
		fi := filter.GetConfig(ctx)
		for _, rel := range workspaceSyncCorpus {
			rcloneIncludes := fi.IncludeRemote(rel)
			classIncludes := tc.want(rel)
			if rcloneIncludes != classIncludes {
				t.Errorf("filter %d: rclone includes %q = %v, classifier says %v", tc.f, rel, rcloneIncludes, classIncludes)
			}
		}
	}
	// Every git data file is in exactly one of the two parts.
	for _, rel := range workspaceSyncCorpus {
		if transfer.ClassifyWorkspaceSyncPath(rel, false) == transfer.WorkspaceSyncGitData &&
			transfer.IsWorkspaceGitState(rel, false) == transfer.IsWorkspaceGitObject(rel, false) {
			t.Errorf("%q: git data in both or neither part", rel)
		}
	}
}

// gitWorkspace is a workspace holding a repository, a nested repository
// and project configuration at the root and in a subdirectory.
var gitWorkspace = map[string]string{
	"README.md":                         "readme",
	"src/main.go":                       "package main\n",
	".gitignore":                        "bin/\n",
	".git/HEAD":                         "ref: refs/heads/main\n",
	".git/packed-refs":                  "abc refs/tags/v1\n",
	".git/index":                        "DIRC",
	".git/refs/heads/main":              "agent-commit\n",
	".git/objects/ab/cdef":              "object",
	".git/config":                       "[core]\n\thooksPath = /tmp/evil\n",
	".git/hooks/pre-commit":             "#!/bin/sh\nevil\n",
	".git/info/attributes":              "* filter=evil\n",
	".git/modules/sub/config":           "[core]\n\tsshCommand = evil\n",
	".git/objects/info/alternates":      "/host/other/objects\n",
	".git/objects/info/http-alternates": "https://evil.example/objects\n",
	".git/logs/HEAD":                    "log\n",
	"sub/.git":                          "gitdir: ../.git/modules/sub\n",
	"vendor/lib/.git/config":            "[core]\n\thooksPath = /tmp/evil\n",
	".scion/project-id":                 "agent-id\n",
	"proj/.scion/settings.yaml":         "evil: true\n",
	"proj/code.go":                      "package proj\n",
}

func wantClass(rel string) transfer.WorkspaceSyncClass {
	return transfer.ClassifyWorkspaceSyncPath(rel, false)
}

func TestUploadWorkspace_CarriesContentAndGitDataOnly(t *testing.T) {
	workspace, bucket := t.TempDir(), t.TempDir()
	writeTree(t, workspace, gitWorkspace)
	if err := uploadWorkspace(context.Background(), workspace, bucket); err != nil {
		t.Fatalf("upload: %v", err)
	}
	for rel, content := range gitWorkspace {
		p := filepath.Join(bucket, rel)
		if wantClass(rel) == transfer.WorkspaceSyncNever {
			assertAbsent(t, p)
			continue
		}
		if got := readFile(t, p); got != content {
			t.Errorf("%s = %q, want %q", rel, got, content)
		}
	}
}

// TestUploadWorkspace_GitDataSemantics checks how an upload treats git data
// already in the bucket: objects are only ever added (a content-only change
// leaves them), while the ref state follows the source as one unit (a ref
// the source no longer has is removed).
func TestUploadWorkspace_GitDataSemantics(t *testing.T) {
	workspace, bucket := t.TempDir(), t.TempDir()
	writeTree(t, workspace, map[string]string{"a.txt": "a", ".git/HEAD": "ref: refs/heads/main\n", ".git/packed-refs": "abc refs/heads/main\n"})
	writeTree(t, bucket, map[string]string{
		".git/refs/heads/main": "stale loose ref\n",
		".git/objects/ab/cd":   "object",
		"stale.txt":            "gone",
	})
	if err := uploadWorkspace(context.Background(), workspace, bucket); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if got := readFile(t, filepath.Join(bucket, ".git/objects/ab/cd")); got != "object" {
		t.Errorf("bucket object = %q, want kept", got)
	}
	assertAbsent(t, filepath.Join(bucket, ".git/refs/heads/main"))
	assertAbsent(t, filepath.Join(bucket, "stale.txt"))
	if got := readFile(t, filepath.Join(bucket, ".git/packed-refs")); got != "abc refs/heads/main\n" {
		t.Errorf("bucket packed-refs = %q", got)
	}
}

// TestUploadWorkspace_SkipsUnsupportedRepository checks that a repository
// that cannot travel as git data uploads its content only, and that the ref
// state an earlier supported upload left at the same prefix is removed, so
// a receiver applies none of it.
func TestUploadWorkspace_SkipsUnsupportedRepository(t *testing.T) {
	for name, marker := range map[string]string{
		"reftable":      ".git/reftable/tables.list",
		"partial clone": ".git/objects/pack/p.promisor",
		"split index":   ".git/sharedindex.0123",
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			workspace, bucket, receiver := t.TempDir(), t.TempDir(), t.TempDir()
			writeTree(t, workspace, map[string]string{"a.txt": "v1", ".git/HEAD": "ref: refs/heads/main\n", ".git/refs/heads/main": "old\n", ".git/objects/ab/cd": "o"})
			if err := uploadWorkspace(ctx, workspace, bucket); err != nil {
				t.Fatalf("first upload: %v", err)
			}
			writeTree(t, workspace, map[string]string{"a.txt": "v2", ".git/refs/heads/main": "new\n", marker: "x"})
			if err := uploadWorkspace(ctx, workspace, bucket); err != nil {
				t.Fatalf("second upload: %v", err)
			}
			if got := readFile(t, filepath.Join(bucket, "a.txt")); got != "v2" {
				t.Errorf("a.txt = %q", got)
			}
			for _, rel := range []string{".git/HEAD", ".git/refs/heads/main"} {
				assertAbsent(t, filepath.Join(bucket, rel))
			}
			// Clearing the ref state leaves the objects.
			if got := readFile(t, filepath.Join(bucket, ".git/objects/ab/cd")); got != "o" {
				t.Errorf("bucket object = %q, want kept", got)
			}
			assertAbsent(t, filepath.Join(bucket, marker))

			writeTree(t, receiver, map[string]string{".git/HEAD": "ref: refs/heads/main\n", ".git/refs/heads/main": "receiver\n"})
			if err := downloadWorkspace(ctx, bucket, receiver, true); err != nil {
				t.Fatalf("download: %v", err)
			}
			if got := readFile(t, filepath.Join(receiver, ".git/refs/heads/main")); got != "receiver\n" {
				t.Errorf("receiver's ref = %q, want kept (no stale ref state applied)", got)
			}
		})
	}
}

// TestDownloadWorkspace_RefusesPreExistingBucketObjects puts every entry
// that is never synced straight into the bucket, as an upload from before
// the shared exclude list (or a tampered bucket) would have, and checks
// that neither download writes any of it.
func TestDownloadWorkspace_RefusesPreExistingBucketObjects(t *testing.T) {
	for _, gitData := range []bool{true, false} {
		bucket, workspace := t.TempDir(), t.TempDir()
		writeTree(t, bucket, gitWorkspace)
		writeTree(t, workspace, map[string]string{
			".git/config":       "[core]\n\tbare = false\n",
			".scion/project-id": "local-id\n",
		})
		if err := downloadWorkspace(context.Background(), bucket, workspace, gitData); err != nil {
			t.Fatalf("gitData=%v: download: %v", gitData, err)
		}
		for rel, content := range gitWorkspace {
			p := filepath.Join(workspace, rel)
			switch {
			case rel == ".git/config":
				if got := readFile(t, p); got != "[core]\n\tbare = false\n" {
					t.Errorf("gitData=%v: receiver's .git/config replaced: %q", gitData, got)
				}
			case rel == ".scion/project-id":
				if got := readFile(t, p); got != "local-id\n" {
					t.Errorf("gitData=%v: receiver's project identity replaced: %q", gitData, got)
				}
			case wantClass(rel) == transfer.WorkspaceSyncNever:
				assertAbsent(t, p)
			case wantClass(rel) == transfer.WorkspaceSyncGitData && !gitData:
				assertAbsent(t, p)
			default:
				if got := readFile(t, p); got != content {
					t.Errorf("gitData=%v: %s = %q, want %q", gitData, rel, got, content)
				}
			}
		}
	}
}

// TestDownloadWorkspace_ContentKeepsReceiverGit is the sync-to case: the
// bucket holds no .git (the CLI never uploads it), so a content download
// must leave the workspace's repository as it is, where a plain sync
// deleted it.
func TestDownloadWorkspace_ContentKeepsReceiverGit(t *testing.T) {
	bucket, workspace := t.TempDir(), t.TempDir()
	writeTree(t, bucket, map[string]string{"a.txt": "new"})
	writeTree(t, workspace, map[string]string{
		"a.txt":                "old",
		".git/HEAD":            "ref: refs/heads/main\n",
		".git/refs/heads/main": "unpushed-commit\n",
		".git/objects/ab/cd":   "object",
		".git/config":          "[core]\n",
	})
	if err := downloadWorkspace(context.Background(), bucket, workspace, false); err != nil {
		t.Fatalf("download: %v", err)
	}
	for rel, want := range map[string]string{
		"a.txt":                "new",
		".git/HEAD":            "ref: refs/heads/main\n",
		".git/refs/heads/main": "unpushed-commit\n",
		".git/objects/ab/cd":   "object",
		".git/config":          "[core]\n",
	} {
		if got := readFile(t, filepath.Join(workspace, rel)); got != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}
}

// TestDownloadWorkspace_DoesNotWriteThroughDestinationLinks reproduces the
// rclone local backend writing through a symlinked directory that exists
// on the destination, and checks the staged apply does not.
func TestDownloadWorkspace_DoesNotWriteThroughDestinationLinks(t *testing.T) {
	bucket, workspace, outside := t.TempDir(), t.TempDir(), t.TempDir()
	writeTree(t, bucket, map[string]string{"dir/new.txt": "agent"})
	if err := os.Symlink(outside, filepath.Join(workspace, "dir")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if err := downloadWorkspace(context.Background(), bucket, workspace, true); err != nil {
		t.Fatalf("download: %v", err)
	}
	assertAbsent(t, filepath.Join(outside, "new.txt"))
	if got := readFile(t, filepath.Join(workspace, "dir/new.txt")); got != "agent" {
		t.Errorf("dir/new.txt = %q", got)
	}
}

// TestDownloadWorkspace_LeavesNoStaging checks the staging directory is
// removed after a download.
func TestDownloadWorkspace_LeavesNoStaging(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	bucket, workspace := t.TempDir(), t.TempDir()
	writeTree(t, bucket, map[string]string{"a.txt": "a"})
	if err := downloadWorkspace(context.Background(), bucket, workspace, true); err != nil {
		t.Fatalf("download: %v", err)
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("staging left behind: %v", entries)
	}
}

// git runs git in dir with no system or global configuration.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"HOME="+t.TempDir(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@example.com",
		"GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestWorkspaceRoundTrip_UnpushedCommitsSurviveSyncBack runs the legs a
// shared-workspace git project takes through the bucket with a real
// repository: the hub's clone is uploaded and downloaded onto a broker
// (create time), an agent commits there without pushing and plants a hook
// and repository configuration, and the broker workspace is uploaded and
// downloaded back into the hub's clone (stop-time sync-back). The commit
// must arrive in the hub's clone; the hook and the configuration must not.
func TestWorkspaceRoundTrip_UnpushedCommitsSurviveSyncBack(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not available: %v", err)
	}
	ctx := context.Background()
	hub, broker := t.TempDir(), t.TempDir()
	createBucket, syncBackBucket := t.TempDir(), t.TempDir()

	git(t, hub, "init", "-q", "-b", "main")
	git(t, hub, "config", "remote.origin.url", "https://example.com/org/repo.git")
	writeTree(t, hub, map[string]string{"README.md": "v1\n"})
	git(t, hub, "add", "README.md")
	git(t, hub, "commit", "-q", "-m", "initial")
	hubConfig := readFile(t, filepath.Join(hub, ".git/config"))

	// Create time: hub -> bucket -> broker.
	if err := uploadWorkspace(ctx, hub, createBucket); err != nil {
		t.Fatalf("create-time upload: %v", err)
	}
	if err := downloadWorkspace(ctx, createBucket, broker, true); err != nil {
		t.Fatalf("create-time download: %v", err)
	}
	assertAbsent(t, filepath.Join(broker, ".git/config"))
	if got := git(t, broker, "log", "--format=%s", "-1"); got != "initial" {
		t.Fatalf("broker checkout log = %q, want initial", got)
	}

	// The agent commits without pushing, and plants a hook and config.
	writeTree(t, broker, map[string]string{"README.md": "v2\n", "new.go": "package x\n"})
	git(t, broker, "add", "README.md", "new.go")
	git(t, broker, "commit", "-q", "-m", "agent work")
	agentHead := git(t, broker, "rev-parse", "HEAD")
	writeTree(t, broker, map[string]string{
		".git/hooks/post-checkout":     "#!/bin/sh\necho pwned\n",
		".git/config":                  "[core]\n\thooksPath = /tmp/evil\n\tfsmonitor = /tmp/evil\n",
		".git/info/attributes":         "* filter=evil\n",
		".git/objects/info/alternates": "/somewhere/else/objects\n",
	})

	// Stop-time sync-back: broker -> bucket -> hub.
	if err := uploadWorkspace(ctx, broker, syncBackBucket); err != nil {
		t.Fatalf("sync-back upload: %v", err)
	}
	if err := downloadWorkspace(ctx, syncBackBucket, hub, true); err != nil {
		t.Fatalf("sync-back download: %v", err)
	}

	if got := git(t, hub, "rev-parse", "HEAD"); got != agentHead {
		t.Errorf("hub HEAD = %s, want the agent's unpushed commit %s", got, agentHead)
	}
	if got := git(t, hub, "log", "--format=%s", "-2"); got != "agent work\ninitial" {
		t.Errorf("hub log = %q", got)
	}
	git(t, hub, "fsck", "--no-progress")
	if got := git(t, hub, "status", "--porcelain"); got != "" {
		t.Errorf("hub checkout not clean after sync-back: %q", got)
	}
	if got := readFile(t, filepath.Join(hub, ".git/config")); got != hubConfig {
		t.Errorf("hub .git/config changed:\n%s", got)
	}
	for _, rel := range []string{".git/hooks/post-checkout", ".git/info/attributes", ".git/objects/info/alternates"} {
		assertAbsent(t, filepath.Join(hub, rel))
	}
}

// TestDownloadWorkspace_CreatesMissingDestination keeps rclone's behaviour
// of creating a destination that does not exist yet.
func TestDownloadWorkspace_CreatesMissingDestination(t *testing.T) {
	bucket := t.TempDir()
	workspace := filepath.Join(t.TempDir(), "projects", "p")
	writeTree(t, bucket, map[string]string{"a.txt": "a", ".git/HEAD": "ref: refs/heads/main\n"})
	if err := downloadWorkspace(context.Background(), bucket, workspace, true); err != nil {
		t.Fatalf("download: %v", err)
	}
	if got := readFile(t, filepath.Join(workspace, "a.txt")); got != "a" {
		t.Errorf("a.txt = %q", got)
	}
	if got := readFile(t, filepath.Join(workspace, ".git/HEAD")); got != "ref: refs/heads/main\n" {
		t.Errorf(".git/HEAD = %q", got)
	}
}

// gitOK reports whether git succeeds in dir (same environment as git).
func gitOK(t *testing.T, dir string, args ...string) bool {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	return cmd.Run() == nil
}

// TestWorkspaceRoundTrip_PackedAndDeletedRefs covers the ref state as one
// unit: the agent commits, deletes a branch and packs its refs (as git gc
// does), and the sync-back must leave the hub's clone at the agent's commit
// without the deleted branch. A second sync-back through the same bucket,
// after another commit and a gc, must again land the agent's state.
func TestWorkspaceRoundTrip_PackedAndDeletedRefs(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not available: %v", err)
	}
	ctx := context.Background()
	hub, broker := t.TempDir(), t.TempDir()
	createBucket, syncBackBucket := t.TempDir(), t.TempDir()

	git(t, hub, "init", "-q", "-b", "main")
	writeTree(t, hub, map[string]string{"README.md": "v1\n"})
	git(t, hub, "add", "README.md")
	git(t, hub, "commit", "-q", "-m", "initial")
	git(t, hub, "branch", "doomed")

	if err := uploadWorkspace(ctx, hub, createBucket); err != nil {
		t.Fatalf("create-time upload: %v", err)
	}
	if err := downloadWorkspace(ctx, createBucket, broker, true); err != nil {
		t.Fatalf("create-time download: %v", err)
	}

	syncBack := func() {
		t.Helper()
		if err := uploadWorkspace(ctx, broker, syncBackBucket); err != nil {
			t.Fatalf("sync-back upload: %v", err)
		}
		if err := downloadWorkspace(ctx, syncBackBucket, hub, true); err != nil {
			t.Fatalf("sync-back download: %v", err)
		}
	}

	// Round 1: commit, delete a branch, pack every ref.
	writeTree(t, broker, map[string]string{"README.md": "v2\n"})
	git(t, broker, "commit", "-q", "-am", "agent work")
	git(t, broker, "branch", "-D", "doomed")
	git(t, broker, "pack-refs", "--all", "--prune")
	agentHead := git(t, broker, "rev-parse", "HEAD")
	syncBack()
	if got := git(t, hub, "rev-parse", "HEAD"); got != agentHead {
		t.Errorf("round 1: hub HEAD = %s, want the agent's commit %s", got, agentHead)
	}
	if gitOK(t, hub, "rev-parse", "--verify", "--quiet", "refs/heads/doomed") {
		t.Error("round 1: the branch the agent deleted is back in the hub's clone")
	}
	if got := git(t, hub, "status", "--porcelain"); got != "" {
		t.Errorf("round 1: hub checkout not clean: %q", got)
	}

	// Round 2, same bucket: another commit, a new loose branch, then gc.
	writeTree(t, broker, map[string]string{"new.go": "package x\n"})
	git(t, broker, "add", "new.go")
	git(t, broker, "commit", "-q", "-m", "more agent work")
	git(t, broker, "branch", "feature")
	git(t, broker, "gc", "-q")
	agentHead = git(t, broker, "rev-parse", "HEAD")
	syncBack()
	if got := git(t, hub, "rev-parse", "HEAD"); got != agentHead {
		t.Errorf("round 2: hub HEAD = %s, want %s", got, agentHead)
	}
	if got := git(t, hub, "rev-parse", "refs/heads/feature"); got != agentHead {
		t.Errorf("round 2: hub feature = %s, want %s", got, agentHead)
	}
	git(t, hub, "fsck", "--no-progress")
}
