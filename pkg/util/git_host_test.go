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

package util

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// hostGitTestEnv isolates git from the host's global and system config and
// clears the agent-container guard, so CreateWorktree runs.
func hostGitTestEnv(t *testing.T) string {
	t.Helper()
	t.Setenv("SCION_HOST_UID", "")
	global := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(global, []byte("[user]\n\temail = you@example.com\n\tname = Your Name\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	return global
}

func hostRunGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// plantRepoCommands places, in the repository's default .git/hooks
// directory, every hook git runs for the host-side commands under test, each
// touching a marker in markers. These are repository content at the default
// path (not a config key); HostGitArgs' core.hooksPath=/dev/null pin keeps
// them from running. Config-based settings (core.hooksPath, core.fsmonitor,
// filter drivers) are covered by the deny-by-default policy tests instead.
func plantRepoCommands(t *testing.T, repo, markers string) {
	t.Helper()
	gitDir := hostRunGit(t, repo, "rev-parse", "--absolute-git-dir")
	hookDir := filepath.Join(gitDir, "hooks")
	if err := os.MkdirAll(hookDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, hook := range []string{"post-checkout", "reference-transaction", "post-merge", "pre-commit", "post-index-change"} {
		script := "#!/bin/sh\ntouch '" + filepath.Join(markers, "hook-"+hook) + "'\n"
		if err := os.WriteFile(filepath.Join(hookDir, hook), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func assertNoMarkers(t *testing.T, markers string) {
	t.Helper()
	entries, err := os.ReadDir(markers)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "hooks" {
			t.Errorf("repository-defined command ran: %s", e.Name())
		}
	}
}

// newHostGitRepo returns a repository with one committed file, and a
// marker directory outside it.
func newHostGitRepo(t *testing.T) (repo, markers string) {
	t.Helper()
	hostGitTestEnv(t)
	repo = filepath.Join(t.TempDir(), "repo")
	hostRunGit(t, filepath.Dir(repo), "init", "-q", "-b", "main", repo)
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hostRunGit(t, repo, "add", "file.txt")
	hostRunGit(t, repo, "commit", "-q", "-m", "initial")
	markers = t.TempDir()
	return repo, markers
}

func TestHostGitArgs(t *testing.T) {
	want := []string{
		"-c", "core.hooksPath=" + os.DevNull,
		"-c", "core.fsmonitor=false",
		"-c", "core.pager=cat",
		"-c", "protocol.ext.allow=never",
		"-c", "protocol.file.allow=user",
		"-c", "submodule.recurse=false",
		"-c", "fetch.recurseSubmodules=false",
		"-c", "merge.verifySignatures=false",
		"-c", "commit.gpgsign=false",
		"-c", "tag.gpgsign=false",
	}
	if got := HostGitArgs(); !reflect.DeepEqual(got, want) {
		t.Errorf("HostGitArgs() = %q, want %q", got, want)
	}
}

func TestParseGitConfigList(t *testing.T) {
	out := []byte("global\x00user.name\nA B\x00local\x00filter.My.Driver.smudge\nx\ny\x00local\x00core.flag\x00")
	got := parseGitConfigList(out)
	want := []gitConfigEntry{
		{scope: "global", key: "user.name", value: "A B", hasValue: true},
		{scope: "local", key: "filter.My.Driver.smudge", value: "x\ny", hasValue: true},
		{scope: "local", key: "core.flag", value: "", hasValue: false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseGitConfigList = %#v, want %#v", got, want)
	}
	sec, sub, v := splitConfigKey("filter.My.Driver.smudge")
	if sec != "filter" || sub != "My.Driver" || v != "smudge" {
		t.Errorf("splitConfigKey = %q %q %q", sec, sub, v)
	}
}

// Filter driver variables the repository sets are replaced by the global
// value of the same variable, or emptied when there is none.
// A standard git-lfs filter in the repository config is allowed, and its
// variables are pinned to the global/system value as defence in depth.
func TestHostGitCommand_FilterPinsStandardLFS(t *testing.T) {
	repo, _ := newHostGitRepo(t)
	global := os.Getenv("GIT_CONFIG_GLOBAL")
	f, err := os.OpenFile(global, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("[filter \"lfs\"]\n\tsmudge = git-lfs smudge -- %f\n\tprocess = git-lfs filter-process\n\trequired = true\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	// The repository sets the standard git-lfs commands too (git lfs
	// install --local): allowed by the policy.
	hostRunGit(t, repo, "config", "filter.lfs.smudge", "git-lfs smudge -- %f")
	hostRunGit(t, repo, "config", "filter.lfs.clean", "git-lfs clean -- %f")
	hostRunGit(t, repo, "config", "filter.lfs.required", "true")

	cmd, err := HostGitCommand(context.Background(), repo, HostGitOptions{Policy: RepoPolicyLinked}, "status")
	if err != nil {
		t.Fatalf("HostGitCommand: %v", err)
	}
	args := strings.Join(cmd.Args, "\x00")
	for _, want := range []string{"filter.lfs.clean=", "filter.lfs.smudge=git-lfs smudge -- %f", "filter.lfs.required=true"} {
		if !strings.Contains(args, "\x00"+want+"\x00") {
			t.Errorf("command args lack -c %q: %q", want, cmd.Args)
		}
	}
	if cmd.Args[len(cmd.Args)-1] != "status" {
		t.Errorf("subcommand not last: %q", cmd.Args)
	}
}

// Deny-by-default: a repository config key outside the allow-list (or an
// include directive) refuses the command, whether or not it is a network
// command, and names the key. This covers the known key families plus an
// arbitrary unknown key.
func TestHostGitCommand_PolicyRefuses(t *testing.T) {
	cases := []struct{ key, value string }{
		// A filter subsection containing '=' and a non-standard filter.
		{"filter.a=b.smudge", "touch /tmp/x; cat"},
		{"filter.custom.smudge", "evil"},
		// Signature-verification knobs that could run gpg.program.
		{"gpg.program", "touch /tmp/x"},
		{"gpg.ssh.program", "touch /tmp/x"},
		{"log.showSignature", "true"},
		// http.* TLS / resolve / cookie / cert keys.
		{"http.sslVerify", "false"},
		{"http.sslCAInfo", "/tmp/ca"},
		{"http.curloptResolve", "example.com:443:127.0.0.1"},
		{"http.cookieFile", "/tmp/cookies"},
		{"http.https://github.com.sslCert", "/tmp/cert"},
		{"http.proxy", "http://proxy.example"},
		// Command / redirect keys.
		{"url.https://elsewhere.example/.insteadOf", "https://github.com/"},
		{"url.https://elsewhere.example/.pushInsteadOf", "https://github.com/"},
		{"credential.helper", "store"},
		{"credential.https://github.com.username", "x"},
		{"core.sshCommand", "ssh -v"},
		{"core.editor", "vi"},
		{"core.worktree", "/tmp/elsewhere"},
		{"remote.origin.uploadpack", "git-upload-pack"},
		{"remote.origin.receivepack", "git-receive-pack"},
		{"remote.origin.proxy", "http://proxy.example"},
		{"remote.origin.vcs", "hg"},
		{"alias.co", "checkout"},
		{"diff.external", "/tmp/d"},
		{"diff.x.textconv", "/tmp/t"},
		{"merge.x.driver", "/tmp/m %O %A %B"},
		{"submodule.x.update", "!touch /tmp/x"},
		{"submodule.x.url", "file:///tmp/s"},
		{"lfs.url", "https://evil.example/lfs"},
		{"lfs.storage", "/tmp/store"},
		{"lfs.standalonetransferagent", "evil"},
		{"lfs.extension.foo.clean", "evil %f"},
		{"gpg.ssh.allowedSignersFile", "/tmp/signers"},
		// An arbitrary unknown key proves the default is deny.
		{"totally.unknown.key", "whatever"},
	}
	for _, mode := range []RepoConfigPolicy{RepoPolicyManaged, RepoPolicyLinked} {
		for _, c := range cases {
			for _, network := range []bool{false, true} {
				name := fmt.Sprintf("policy%d/%s/net%v", mode, c.key, network)
				t.Run(name, func(t *testing.T) {
					repo, _ := newHostGitRepo(t)
					hostRunGit(t, repo, "config", c.key, c.value)
					_, err := HostGitCommand(context.Background(), repo, HostGitOptions{Policy: mode, Network: network}, "status")
					var refused *RepoConfigRefusedError
					if !errors.As(err, &refused) {
						t.Fatalf("error = %v, want *RepoConfigRefusedError", err)
					}
					if !strings.EqualFold(refused.Key, c.key) && !strings.HasPrefix(strings.ToLower(refused.Key), strings.ToLower(strings.SplitN(c.key, ".", 2)[0])) {
						t.Errorf("refused key = %q, want %q", refused.Key, c.key)
					}
				})
			}
		}
	}
}

// include/includeIf directives are refused outright.
func TestHostGitCommand_RefusesIncludes(t *testing.T) {
	for _, key := range []string{"include.path", "includeIf.gitdir:/x/.path"} {
		repo, _ := newHostGitRepo(t)
		// Write the directive as data (git config would try to open it).
		gitDir := hostRunGit(t, repo, "rev-parse", "--absolute-git-dir")
		cfg := filepath.Join(gitDir, "config")
		data, err := os.ReadFile(cfg)
		if err != nil {
			t.Fatal(err)
		}
		section, name, _ := strings.Cut(key, ".")
		if strings.Contains(key, ":") {
			// includeIf "<cond>" .path
			cond := strings.TrimSuffix(strings.SplitN(key, ".path", 2)[0], "")
			cond = strings.TrimPrefix(cond, "includeIf.")
			data = append(data, []byte("[includeIf \""+cond+"\"]\n\tpath = /dev/null\n")...)
		} else {
			data = append(data, []byte("["+section+"]\n\t"+name+" = /dev/null\n")...)
		}
		if err := os.WriteFile(cfg, data, 0o644); err != nil {
			t.Fatal(err)
		}
		_, err = HostGitCommand(context.Background(), repo, HostGitOptions{Policy: RepoPolicyLinked}, "status")
		var refused *RepoConfigRefusedError
		if !errors.As(err, &refused) {
			t.Errorf("%s: error = %v, want *RepoConfigRefusedError", key, err)
		}
	}
}

// The managed policy allows only the keys git and Scion write; the linked
// policy allows a typical user repository too.
func TestHostGitCommand_AllowsExpectedKeys(t *testing.T) {
	t.Run("managed", func(t *testing.T) {
		repo, _ := newHostGitRepo(t)
		for _, kv := range [][2]string{
			{"gc.auto", "0"}, {"branch.autoSetupMerge", "false"},
			{"user.name", "Scion"}, {"user.email", "agent@scion.dev"},
			{"extensions.worktreeConfig", "true"},
		} {
			hostRunGit(t, repo, "config", kv[0], kv[1])
		}
		if _, err := HostGitCommand(context.Background(), repo, HostGitOptions{Policy: RepoPolicyManaged}, "status"); err != nil {
			t.Fatalf("managed policy refused a Scion-written config: %v", err)
		}
	})
	t.Run("linked-typical", func(t *testing.T) {
		repo, _ := newHostGitRepo(t)
		for _, kv := range [][2]string{
			{"core.autocrlf", "input"}, {"core.ignorecase", "true"},
			{"remote.origin.url", "https://github.com/org/repo.git"},
			{"remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"},
			{"remote.origin.pushurl", "git@github.com:org/repo.git"},
			{"branch.main.remote", "origin"}, {"branch.main.merge", "refs/heads/main"},
			{"branch.main.rebase", "true"},
			{"user.name", "Dev"}, {"user.email", "dev@example.com"},
			{"pull.rebase", "true"}, {"push.default", "simple"},
			{"init.defaultBranch", "main"}, {"color.ui", "auto"},
			{"filter.lfs.clean", "git-lfs clean -- %f"},
			{"filter.lfs.smudge", "git-lfs smudge -- %f"},
			{"filter.lfs.process", "git-lfs filter-process"},
			{"filter.lfs.required", "true"},
			{"submodule.sub.update", "checkout"},
		} {
			hostRunGit(t, repo, "config", kv[0], kv[1])
		}
		if _, err := HostGitCommand(context.Background(), repo, HostGitOptions{Policy: RepoPolicyLinked}, "status"); err != nil {
			t.Fatalf("linked policy refused a typical user config: %v", err)
		}
	})
	t.Run("managed-refuses-linked-only-keys", func(t *testing.T) {
		repo, _ := newHostGitRepo(t)
		hostRunGit(t, repo, "config", "pull.rebase", "true")
		_, err := HostGitCommand(context.Background(), repo, HostGitOptions{Policy: RepoPolicyManaged}, "status")
		var refused *RepoConfigRefusedError
		if !errors.As(err, &refused) {
			t.Fatalf("managed: error = %v, want pull.rebase refused", err)
		}
	})
}

// A repository-scope core.worktree is allowed only when it equals the
// caller's AllowedWorktreePath.
func TestHostGitCommand_CoreWorktreeMatch(t *testing.T) {
	repo, _ := newHostGitRepo(t)
	wt := repo // an existing directory git can treat as the work tree
	hostRunGit(t, repo, "config", "core.worktree", wt)
	if _, err := HostGitCommand(context.Background(), repo, HostGitOptions{Policy: RepoPolicyLinked, AllowedWorktreePath: wt}, "status"); err != nil {
		t.Errorf("matching core.worktree refused: %v", err)
	}
	_, err := HostGitCommand(context.Background(), repo, HostGitOptions{Policy: RepoPolicyLinked, AllowedWorktreePath: "/other"}, "status")
	var refused *RepoConfigRefusedError
	if !errors.As(err, &refused) {
		t.Errorf("mismatched core.worktree: error = %v, want refusal", err)
	}
}

// Keys in the host user's global config are not subject to the policy.
func TestHostGitCommand_GlobalKeysNotChecked(t *testing.T) {
	repo, _ := newHostGitRepo(t)
	global := os.Getenv("GIT_CONFIG_GLOBAL")
	hostRunGit(t, repo, "config", "--file", global, "url.https://mirror.example/.insteadOf", "https://github.com/")
	hostRunGit(t, repo, "config", "--file", global, "credential.helper", "store")
	hostRunGit(t, repo, "config", "--file", global, "core.sshCommand", "ssh -v")
	if _, err := HostGitCommand(context.Background(), repo, HostGitOptions{Policy: RepoPolicyManaged, Network: true}, "fetch"); err != nil {
		t.Fatalf("global keys triggered a refusal: %v", err)
	}
}

func TestHostGitCommand_RequiresDir(t *testing.T) {
	if _, err := HostGitCommand(context.Background(), "", HostGitOptions{}, "status"); err == nil {
		t.Error("expected an error for an empty dir")
	}
}

func TestCreateWorktree_RunsNoRepositoryCommands(t *testing.T) {
	repo, markers := newHostGitRepo(t)
	plantRepoCommands(t, repo, markers)

	wt := filepath.Join(repo, "agents", "a", "workspace")
	if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CreateWorktree(wt, "agent-a"); err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(wt, "file.txt"))
	if err != nil || string(got) != "content\n" {
		t.Fatalf("worktree file = %q, %v", got, err)
	}
	// The branch-exists path runs the second worktree add.
	if _, err := RemoveWorktree(filepath.Dir(wt), wt, repo, ""); err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}
	if err := CreateWorktree(wt, "agent-a"); err != nil {
		t.Fatalf("CreateWorktree (existing branch): %v", err)
	}
	assertNoMarkers(t, markers)
}

func TestDeleteBranchIn_RunsNoRepositoryHooks(t *testing.T) {
	repo, markers := newHostGitRepo(t)
	hostRunGit(t, repo, "branch", "doomed")
	plantRepoCommands(t, repo, markers)
	if !DeleteBranchIn(repo, "doomed") {
		t.Fatal("DeleteBranchIn returned false")
	}
	if BranchExistsIn(repo, "doomed") {
		t.Error("branch still exists")
	}
	assertNoMarkers(t, markers)
}

// A branch name that is empty or looks like an option is refused outright, so a
// hostile value can never reach `git branch -D` as a flag (it is also passed
// after "--"). This guards both DeleteBranchIn and, through it, the branch that
// RemoveWorktree deletes.
func TestDeleteBranchIn_RefusesOptionLikeBranch(t *testing.T) {
	repo, markers := newHostGitRepo(t)
	// A real branch whose deletion must NOT be triggered by a hostile value.
	hostRunGit(t, repo, "branch", "keep")
	plantRepoCommands(t, repo, markers)
	for _, hostile := range []string{"", "-D", "--force", "-", "--all"} {
		if DeleteBranchIn(repo, hostile) {
			t.Errorf("DeleteBranchIn accepted the option-like branch %q", hostile)
		}
	}
	if !BranchExistsIn(repo, "keep") {
		t.Error("a real branch was deleted by an option-like value")
	}
	assertNoMarkers(t, markers)
}

// RemoveWorktree never acts on the repository the worktree's (agent-writable)
// .git file points at: it validates the worktree against the caller-named
// repository (worktreeOnBranch) and fails closed on a rewritten .git file, so
// no branch is deleted anywhere — and in particular nothing in the repository
// the .git file names is touched.
func TestRemoveWorktree_RewrittenGitFileFailsClosed(t *testing.T) {
	repo, markers := newHostGitRepo(t)
	agentDir := filepath.Join(repo, "agents", "a")
	wt := filepath.Join(agentDir, "workspace")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CreateWorktree(wt, "agent-a"); err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}

	other, _ := newHostGitRepo(t)
	hostRunGit(t, other, "branch", "agent-a")
	hostRunGit(t, other, "worktree", "add", "-q", filepath.Join(t.TempDir(), "o"), "-b", "other-wt")
	plantRepoCommands(t, other, markers)
	otherAdmin := filepath.Join(other, ".git", "worktrees", "o")
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+otherAdmin+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	deleted, err := RemoveWorktree(agentDir, wt, repo, "agent-a")
	if err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}
	// The .git file was rewritten to name another repository, so the worktree no
	// longer validates against the caller's repository: fail closed, delete nothing.
	if deleted {
		t.Error("a rewritten .git file must not cause a branch deletion")
	}
	if !BranchExistsIn(repo, "agent-a") {
		t.Error("branch agent-a was deleted despite the worktree .git mismatch")
	}
	if !BranchExistsIn(other, "agent-a") || !BranchExistsIn(other, "other-wt") {
		t.Error("a branch was deleted in the repository the .git file names")
	}
	assertNoMarkers(t, markers)
}

// With no repository root, RemoveWorktree removes the worktree directory but
// deletes no branch: a branch is deleted only through the repository the caller
// names, never one discovered from the worktree's own (agent-writable) .git file.
func TestRemoveWorktree_NoRepoRootDeletesNoBranch(t *testing.T) {
	repo, markers := newHostGitRepo(t)
	agentDir := filepath.Join(repo, "agents", "a")
	wt := filepath.Join(agentDir, "workspace")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CreateWorktree(wt, "agent-a"); err != nil {
		t.Fatal(err)
	}
	deleted, err := RemoveWorktree(agentDir, wt, "", "agent-a")
	if err != nil || deleted {
		t.Fatalf("RemoveWorktree = %v, %v; want no branch deleted", deleted, err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Error("worktree directory still exists")
	}
	if !BranchExistsIn(repo, "agent-a") {
		t.Error("branch agent-a was deleted even though no repository root was given")
	}
	assertNoMarkers(t, markers)
}

// sharedWorkspaceFixture returns a source repository and a shared
// workspace cloned from it.
func sharedWorkspaceFixture(t *testing.T) (source, workspace, markers string) {
	t.Helper()
	source, markers = newHostGitRepo(t)
	workspace = filepath.Join(t.TempDir(), "workspace")
	if err := CloneSharedWorkspace(workspace, source, "", ""); err != nil {
		t.Fatalf("CloneSharedWorkspace: %v", err)
	}
	return source, workspace, markers
}

func commitFile(t *testing.T, repo, name, content, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	hostRunGit(t, repo, "add", name)
	hostRunGit(t, repo, "-c", "core.hooksPath="+os.DevNull, "commit", "-q", "-m", msg)
}

func TestPullSharedWorkspace_RunsNoRepositoryCommands(t *testing.T) {
	source, workspace, markers := sharedWorkspaceFixture(t)
	plantRepoCommands(t, workspace, markers)
	commitFile(t, source, "new.txt", "new\n", "add new")

	result, err := PullSharedWorkspace(workspace, source, "")
	if err != nil {
		t.Fatalf("PullSharedWorkspace: %v", err)
	}
	if !result.Updated || len(result.Commits) != 1 || result.Commits[0].Subject != "add new" {
		t.Errorf("result = %+v", result)
	}
	if got, err := os.ReadFile(filepath.Join(workspace, "new.txt")); err != nil || string(got) != "new\n" {
		t.Errorf("new.txt = %q, %v", got, err)
	}
	assertNoMarkers(t, markers)
}

// The pull uses the clone URL the hub passes, not the workspace's remote.
func TestPullSharedWorkspace_UsesCloneURL(t *testing.T) {
	source, workspace, _ := sharedWorkspaceFixture(t)
	other, _ := newHostGitRepo(t)
	commitFile(t, other, "other.txt", "other\n", "from other")
	hostRunGit(t, workspace, "remote", "set-url", "origin", other)
	commitFile(t, source, "new.txt", "new\n", "from source")

	result, err := PullSharedWorkspace(workspace, source, "")
	if err != nil {
		t.Fatalf("PullSharedWorkspace: %v", err)
	}
	if len(result.Commits) != 1 || result.Commits[0].Subject != "from source" {
		t.Errorf("commits = %+v, want the source commit only", result.Commits)
	}
	if _, err := os.Stat(filepath.Join(workspace, "other.txt")); !os.IsNotExist(err) {
		t.Error("pulled from the workspace's configured remote")
	}
	if got := hostRunGit(t, workspace, "rev-parse", "refs/remotes/origin/main"); got != hostRunGit(t, source, "rev-parse", "HEAD") {
		t.Errorf("origin/main = %s, want the source head", got)
	}
}

func TestPullSharedWorkspace_RefusesRepositoryTransportConfig(t *testing.T) {
	source, workspace, _ := sharedWorkspaceFixture(t)
	hostRunGit(t, workspace, "config", "url."+t.TempDir()+".insteadOf", source)
	commitFile(t, source, "new.txt", "new\n", "add new")

	_, err := PullSharedWorkspace(workspace, source, "")
	if err == nil || !strings.Contains(err.Error(), "git pull failed") || !strings.Contains(err.Error(), "insteadof") {
		t.Fatalf("PullSharedWorkspace error = %v, want a refusal naming the key", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "new.txt")); !os.IsNotExist(err) {
		t.Error("pull ran despite the refusal")
	}
}

func TestPullSharedWorkspace_RequiresCloneURL(t *testing.T) {
	_, workspace, _ := sharedWorkspaceFixture(t)
	if _, err := PullSharedWorkspace(workspace, "", ""); err == nil || !strings.Contains(err.Error(), "git pull failed") {
		t.Fatalf("error = %v, want a pull failure", err)
	}
}

// The upstream branch configured for the current branch is the one pulled.
func TestPullSharedWorkspace_ConfiguredUpstreamBranch(t *testing.T) {
	source, workspace, _ := sharedWorkspaceFixture(t)
	hostRunGit(t, source, "branch", "release")
	hostRunGit(t, workspace, "fetch", "-q", "origin")
	hostRunGit(t, workspace, "checkout", "-q", "-b", "work", "--track", "origin/release")
	hostRunGit(t, source, "checkout", "-q", "release")
	commitFile(t, source, "rel.txt", "rel\n", "release change")

	result, err := PullSharedWorkspace(workspace, source, "")
	if err != nil {
		t.Fatalf("PullSharedWorkspace: %v", err)
	}
	if len(result.Commits) != 1 || result.Commits[0].Subject != "release change" {
		t.Errorf("commits = %+v", result.Commits)
	}
}

// Host-side worktree creation leaves ordinary git use in the worktree
// unchanged: commit, push, fetch and rebase work.
func TestCreateWorktree_WorktreeSupportsCommitPushFetchRebase(t *testing.T) {
	repo, _ := newHostGitRepo(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	hostRunGit(t, repo, "init", "-q", "--bare", "-b", "main", remote)
	hostRunGit(t, repo, "remote", "add", "origin", remote)
	hostRunGit(t, repo, "push", "-q", "origin", "main")

	wt := filepath.Join(repo, "agents", "a", "workspace")
	if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CreateWorktree(wt, "agent-a"); err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	commitFile(t, wt, "a.txt", "a\n", "agent change")
	hostRunGit(t, wt, "push", "-q", "-u", "origin", "agent-a")

	// Move main on the remote, then fetch and rebase onto it.
	other := filepath.Join(t.TempDir(), "other")
	hostRunGit(t, repo, "clone", "-q", remote, other)
	commitFile(t, other, "b.txt", "b\n", "main change")
	hostRunGit(t, other, "push", "-q", "origin", "main")

	hostRunGit(t, wt, "fetch", "-q", "origin")
	hostRunGit(t, wt, "rebase", "-q", "origin/main")
	if got := hostRunGit(t, wt, "log", "--format=%s", "-3"); got != "agent change\nmain change\ninitial" {
		t.Errorf("log after rebase = %q", got)
	}
	hostRunGit(t, wt, "push", "-q", "--force-with-lease", "origin", "agent-a")
	if got := hostRunGit(t, wt, "rev-parse", "@{upstream}"); got != hostRunGit(t, wt, "rev-parse", "HEAD") {
		t.Errorf("upstream %s != HEAD", got)
	}
}

// Every command HostGitCommand builds, network or not, starts with
// HostGitArgs, so work-tree writes never recurse into submodules.
func TestHostGitCommand_CarriesFixedArgs(t *testing.T) {
	repo, _ := newHostGitRepo(t)
	for _, network := range []bool{false, true} {
		cmd, err := HostGitCommand(context.Background(), repo, HostGitOptions{Network: network}, "merge", "--ff-only", "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		fixed := HostGitArgs()
		if !reflect.DeepEqual(cmd.Args[1:1+len(fixed)], fixed) {
			t.Errorf("args = %q, want them to start with %q", cmd.Args, fixed)
		}
	}
}

// allowVar unit coverage: the managed and linked allow-lists, and the value
// checks for core.worktree, filter.lfs.*, submodule.*.update and *.program.
func TestAllowVar(t *testing.T) {
	v := func(key, value string) gitConfigVar {
		sec, sub, name := splitConfigKey(key)
		return gitConfigVar{Section: strings.ToLower(sec), Subsection: sub, HasSubsection: sub != "", Name: strings.ToLower(name), Value: value}
	}
	managedOK := []string{"core.repositoryformatversion", "core.filemode", "extensions.worktreeConfig", "remote.origin.url", "remote.origin.fetch", "branch.main.remote", "branch.main.merge", "gc.auto", "user.name"}
	for _, k := range managedOK {
		if ok, _ := allowVar(RepoPolicyManaged, v(k, "x"), repoConfigAllowOptions{}); !ok {
			t.Errorf("managed allowVar(%q) = false, want true", k)
		}
	}
	// Allowed in linked, refused in managed. Includes the keys allowed in
	// linked only because a HostGitArgs pin neutralises them.
	linkedOnly := []string{"core.autocrlf", "core.sparseCheckout", "pull.rebase", "push.default", "remote.origin.pushurl", "color.ui", "init.defaultBranch", "core.hooksPath", "core.fsmonitor", "core.pager", "submodule.recurse", "maintenance.auto", "maintenance.gc.enabled", "gpg.format", "user.signingKey", "advice.detachedHead", "rerere.enabled"}
	for _, k := range linkedOnly {
		if ok, _ := allowVar(RepoPolicyManaged, v(k, "x"), repoConfigAllowOptions{}); ok {
			t.Errorf("managed allowVar(%q) = true, want false", k)
		}
		if ok, reason := allowVar(RepoPolicyLinked, v(k, "x"), repoConfigAllowOptions{}); !ok {
			t.Errorf("linked allowVar(%q) = false (%s), want true", k, reason)
		}
	}
	// user.signingKey is top-level user.*; the v() helper treats it as such.
	deny := []string{"core.sshCommand", "core.editor", "credential.helper", "url.x.insteadOf", "http.proxy", "http.sslVerify", "gpg.program", "gpg.ssh.program", "gpg.ssh.allowedSignersFile", "alias.st", "remote.origin.uploadpack", "diff.external", "diff.x.textconv", "merge.x.driver", "lfs.url", "lfs.storage", "lfs.standalonetransferagent", "lfs.extension.foo.clean", "totally.unknown.key", "diff.orderfile", "remote.origin.promisor", "remote.origin.partialclonefilter"}
	for _, mode := range []RepoConfigPolicy{RepoPolicyManaged, RepoPolicyLinked} {
		for _, k := range deny {
			if ok, _ := allowVar(mode, v(k, "x"), repoConfigAllowOptions{}); ok {
				t.Errorf("policy%d allowVar(%q) = true, want false", mode, k)
			}
		}
	}
	// Submodule URL scheme check.
	if ok, _ := allowVar(RepoPolicyLinked, v("submodule.s.url", "https://github.com/o/r"), repoConfigAllowOptions{}); !ok {
		t.Error("submodule https url: want allowed")
	}
	if ok, _ := allowVar(RepoPolicyLinked, v("submodule.s.url", "git@github.com:o/r"), repoConfigAllowOptions{}); !ok {
		t.Error("submodule scp-ssh url: want allowed")
	}
	for _, bad := range []string{"file:///tmp/s", "ext::sh -c id", "/local/path", "../rel"} {
		if ok, _ := allowVar(RepoPolicyLinked, v("submodule.s.url", bad), repoConfigAllowOptions{}); ok {
			t.Errorf("submodule url %q: want refused", bad)
		}
	}
	// lfs inert key allowed.
	if ok, _ := allowVar(RepoPolicyLinked, v("lfs.concurrenttransfers", "3"), repoConfigAllowOptions{}); !ok {
		t.Error("lfs.concurrenttransfers: want allowed")
	}
	// An empty quoted subsection is not a top-level key.
	emptySub := gitConfigVar{Section: "push", Subsection: "", HasSubsection: true, Name: "default", Value: "simple"}
	if ok, _ := allowVar(RepoPolicyLinked, emptySub, repoConfigAllowOptions{}); ok {
		t.Error("[push \"\"] default: want refused (not a top-level push.default)")
	}
	// core.worktree only with a matching AllowedWorktreePath.
	if ok, _ := allowVar(RepoPolicyLinked, v("core.worktree", "/wt"), repoConfigAllowOptions{worktreePath: "/wt"}); !ok {
		t.Error("core.worktree matching path: want allowed")
	}
	if ok, _ := allowVar(RepoPolicyLinked, v("core.worktree", "/wt"), repoConfigAllowOptions{worktreePath: "/other"}); ok {
		t.Error("core.worktree mismatched path: want refused")
	}
	// filter.lfs.* only when standard.
	if ok, _ := allowVar(RepoPolicyLinked, v("filter.lfs.smudge", "git-lfs smudge -- %f"), repoConfigAllowOptions{}); !ok {
		t.Error("standard filter.lfs.smudge: want allowed")
	}
	if ok, _ := allowVar(RepoPolicyLinked, v("filter.lfs.smudge", "evil"), repoConfigAllowOptions{}); ok {
		t.Error("non-standard filter.lfs.smudge: want refused")
	}
	// submodule.*.update only for none|checkout|rebase|merge.
	if ok, _ := allowVar(RepoPolicyLinked, v("submodule.s.update", "rebase"), repoConfigAllowOptions{}); !ok {
		t.Error("submodule update rebase: want allowed")
	}
	if ok, _ := allowVar(RepoPolicyLinked, v("submodule.s.update", "!cmd"), repoConfigAllowOptions{}); ok {
		t.Error("submodule update !cmd: want refused")
	}
}

func TestHostGitEnv(t *testing.T) {
	in := []string{
		"HOME=/h", "PATH=/bin", "GIT_DIR=/x", "GIT_WORK_TREE=/y", "GIT_COMMON_DIR=/z",
		"GIT_INDEX_FILE=/i", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.pager", "GIT_CONFIG_VALUE_0=x",
		"GIT_CONFIG_PARAMETERS='a.b=c'", "GIT_EXEC_PATH=/e", "GIT_EDITOR=vi", "GIT_PAGER=less",
		"GIT_CONFIG_GLOBAL=/g", "GIT_CONFIG_NOSYSTEM=1", "GIT_SSH_COMMAND=ssh", "GIT_TERMINAL_PROMPT=0",
	}
	want := []string{"HOME=/h", "PATH=/bin", "GIT_CONFIG_GLOBAL=/g", "GIT_CONFIG_NOSYSTEM=1", "GIT_SSH_COMMAND=ssh", "GIT_TERMINAL_PROMPT=0"}
	if got := HostGitEnv(in); !reflect.DeepEqual(got, want) {
		t.Errorf("HostGitEnv = %q, want %q", got, want)
	}
}

// A change to the repository's filter drivers while the command runs is
// reported after it.
func TestHostGitCmd_DetectsFilterConfigChange(t *testing.T) {
	ctx := context.Background()

	t.Run("refused key appears during the command", func(t *testing.T) {
		repo, _ := newHostGitRepo(t)
		cmd, err := HostGitCommand(ctx, repo, HostGitOptions{}, "config", "filter.late.smudge", "x")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cmd.CombinedOutput(); !errors.Is(err, ErrRepoConfigChanged) {
			t.Errorf("refused key added during the command: error = %v, want ErrRepoConfigChanged", err)
		}
	})

	t.Run("allowed non-filter change is not a change", func(t *testing.T) {
		repo, _ := newHostGitRepo(t)
		cmd, err := HostGitCommand(ctx, repo, HostGitOptions{}, "config", "gc.auto", "0")
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Run(); err != nil {
			t.Errorf("non-filter change: error = %v, want nil", err)
		}
	})

	t.Run("sensitive key change is reported", func(t *testing.T) {
		repo, _ := newHostGitRepo(t)
		hostRunGit(t, repo, "remote", "add", "origin", "https://github.com/o/r.git")
		cmd, err := HostGitCommand(ctx, repo, HostGitOptions{}, "config", "remote.origin.url", "https://evil.example/o/r.git")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cmd.Output(); !errors.Is(err, ErrRepoConfigChanged) {
			t.Errorf("remote.origin.url changed during the command: error = %v, want ErrRepoConfigChanged", err)
		}
	})

	t.Run("non-sensitive allowed key change is tolerated", func(t *testing.T) {
		repo, _ := newHostGitRepo(t)
		// user.name is allowed but not sensitive; changing it mid-command is
		// tolerated (git-lfs-style allowed writes must not fail the command).
		cmd, err := HostGitCommand(ctx, repo, HostGitOptions{}, "config", "user.name", "changed")
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Run(); err != nil {
			t.Errorf("user.name change: error = %v, want nil (tolerated)", err)
		}
	})

	t.Run("git-lfs-style lfs.repositoryformatversion write tolerated", func(t *testing.T) {
		repo, _ := newHostGitRepo(t)
		// Simulate git-lfs writing its repo-format key during our command.
		cmd, err := HostGitCommand(ctx, repo, HostGitOptions{Policy: RepoPolicyManaged}, "config", "lfs.repositoryformatversion", "0")
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Run(); err != nil {
			t.Errorf("lfs.repositoryformatversion=0 write: error = %v, want nil (tolerated)", err)
		}
	})

	t.Run("no change, no error", func(t *testing.T) {
		repo, _ := newHostGitRepo(t)
		cmd, err := HostGitCommand(ctx, repo, HostGitOptions{}, "status", "--porcelain")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cmd.Output(); err != nil {
			t.Errorf("unchanged: error = %v", err)
		}
	})
}

// The git-lfs keys git-lfs writes are allowed, value-checked.
func TestAllowVar_LFSWrittenKeys(t *testing.T) {
	v := func(key, value string) gitConfigVar {
		sec, sub, name := splitConfigKey(key)
		return gitConfigVar{Section: strings.ToLower(sec), Subsection: sub, HasSubsection: sub != "", Name: strings.ToLower(name), Value: value}
	}
	for _, mode := range []RepoConfigPolicy{RepoPolicyManaged, RepoPolicyLinked} {
		ok := func(key, val string) {
			if a, _ := allowVar(mode, v(key, val), repoConfigAllowOptions{}); !a {
				t.Errorf("policy%d: %s=%s should be allowed", mode, key, val)
			}
		}
		no := func(key, val string) {
			if a, _ := allowVar(mode, v(key, val), repoConfigAllowOptions{}); a {
				t.Errorf("policy%d: %s=%s should be refused", mode, key, val)
			}
		}
		ok("lfs.repositoryformatversion", "0")
		no("lfs.repositoryformatversion", "1")
		ok("lfs.https://github.com/o/r.git/info/lfs.access", "basic")
		ok("lfs.https://github.com/o/r.git/info/lfs.access", "none")
		no("lfs.https://github.com/o/r.git/info/lfs.access", "negotiate") // non-basic access mode
		no("lfs.https://github.com/o/r.git/info/lfs.access", "ntlm")      // non-basic access mode
		ok("lfs.https://github.com/o/r.git/info/lfs.locksverify", "true")
		no("lfs.https://github.com/o/r.git/info/lfs.locksverify", "maybe")
		// A non-URL subsection is refused.
		no("lfs.not a url.access", "basic")
		no("lfs.ext::sh -c id.access", "basic")
		// Still refused: redirect/host-path keys.
		no("lfs.url", "https://evil.example/lfs")
		no("lfs.storage", "/tmp/x")
	}
}

// With extensions.worktreeConfig on, the main worktree's config.worktree
// (in the common dir) is checked too.
func TestHostGitCommand_ChecksMainWorktreeConfig(t *testing.T) {
	repo, _ := newHostGitRepo(t)
	hostRunGit(t, repo, "config", "extensions.worktreeConfig", "true")
	gitDir := hostRunGit(t, repo, "rev-parse", "--absolute-git-dir")
	// A refused key placed in the main worktree's config.worktree.
	if err := os.WriteFile(filepath.Join(gitDir, "config.worktree"), []byte("[core]\n\tsshCommand = ssh -v\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := HostGitCommand(context.Background(), repo, HostGitOptions{Policy: RepoPolicyLinked}, "status")
	var refused *RepoConfigRefusedError
	if !errors.As(err, &refused) || !strings.EqualFold(refused.Key, "core.sshCommand") {
		t.Fatalf("error = %v, want core.sshCommand refused from config.worktree", err)
	}
}

// A remote operation carries the transport allow-list.
func TestHostGitCommand_NetworkTransportAllowlist(t *testing.T) {
	repo, _ := newHostGitRepo(t)
	cmd, err := HostGitCommand(context.Background(), repo, HostGitOptions{Network: true}, "fetch")
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(cmd.Args, "\x00")
	for _, want := range []string{"protocol.allow=never", "protocol.https.allow=always", "protocol.ssh.allow=always"} {
		if !strings.Contains(args, "\x00"+want+"\x00") {
			t.Errorf("network args lack -c %q: %q", want, cmd.Args)
		}
	}
}

// diff/merge driver keys that name a command are refused; data attributes
// are allowed (explicit keys).
func TestHostGitCommand_DiffMergeExplicitKeys(t *testing.T) {
	allowed := [][2]string{
		{"diff.algorithm", "histogram"},
		{"diff.myd.xfuncname", "^def "},
		{"merge.conflictstyle", "zdiff3"},
		{"merge.mym.name", "my merge"},
	}
	for _, kv := range allowed {
		repo, _ := newHostGitRepo(t)
		hostRunGit(t, repo, "config", kv[0], kv[1])
		if _, err := HostGitCommand(context.Background(), repo, HostGitOptions{Policy: RepoPolicyLinked}, "status"); err != nil {
			t.Errorf("%s=%s refused: %v", kv[0], kv[1], err)
		}
	}
	refused := [][2]string{
		{"diff.external", "/tmp/d"},
		{"diff.myd.textconv", "/tmp/t"},
		{"diff.myd.command", "/tmp/c"},
		{"merge.mym.driver", "/tmp/m %O %A %B"},
		{"merge.tool", "mytool"},
	}
	for _, kv := range refused {
		repo, _ := newHostGitRepo(t)
		hostRunGit(t, repo, "config", kv[0], kv[1])
		_, err := HostGitCommand(context.Background(), repo, HostGitOptions{Policy: RepoPolicyLinked}, "status")
		var r *RepoConfigRefusedError
		if !errors.As(err, &r) {
			t.Errorf("%s=%s: error = %v, want refusal", kv[0], kv[1], err)
		}
	}
}

// Keys allowed in a linked checkout only because a fixed
// pin overrides them (core.hooksPath, core.fsmonitor, core.pager,
// submodule.recurse) must not run their configured value on a host git
// command. CreateWorktree (which checks out) is the trigger.
func TestLinkedPinnedKeysDoNotExecute(t *testing.T) {
	repo, markers := newHostGitRepo(t)
	fail := filepath.Join(markers, "ran")
	hookDir := filepath.Join(markers, "hooks")
	if err := os.MkdirAll(hookDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, hook := range []string{"post-checkout", "reference-transaction", "post-index-change"} {
		if err := os.WriteFile(filepath.Join(hookDir, hook), []byte("#!/bin/sh\ntouch '"+fail+"'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Values that would run a command if the pins did not override them.
	hostRunGit(t, repo, "config", "core.hooksPath", hookDir)
	hostRunGit(t, repo, "config", "core.fsmonitor", "touch '"+fail+"'; false")
	hostRunGit(t, repo, "config", "core.pager", "touch '"+fail+"'; cat")
	hostRunGit(t, repo, "config", "submodule.recurse", "true")

	wt := filepath.Join(repo, "agents", "a", "workspace")
	if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CreateWorktree(wt, "agent-a"); err != nil {
		t.Fatalf("CreateWorktree (should be allowed, keys are pinned): %v", err)
	}
	if _, err := os.Stat(fail); !os.IsNotExist(err) {
		t.Errorf("a pinned key ran its configured value (marker present)")
	}
}

// commit.gpgsign/tag.gpgsign are pinned off, so a commit the broker makes
// does not invoke a signing program.
func TestGpgSignPinnedOff(t *testing.T) {
	repo, _ := newHostGitRepo(t)
	cmd, err := HostGitCommand(context.Background(), repo, HostGitOptions{Policy: RepoPolicyLinked}, "status")
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(cmd.Args, "\x00")
	for _, want := range []string{"commit.gpgsign=false", "tag.gpgsign=false", "protocol.file.allow=user"} {
		if !strings.Contains(args, "\x00"+want+"\x00") {
			t.Errorf("args lack -c %q: %q", want, cmd.Args)
		}
	}
}

// A valueless boolean key in .git/config ([core] symlinks) must pass
// the cross-check (git lists it with no value; the parser stores "true").
func TestHostGitCommand_ValuelessBooleanKey(t *testing.T) {
	repo, _ := newHostGitRepo(t)
	gitDir := hostRunGit(t, repo, "rev-parse", "--absolute-git-dir")
	data, err := os.ReadFile(filepath.Join(gitDir, "config"))
	if err != nil {
		t.Fatal(err)
	}
	// A bare boolean with no value, in a legal hand-written form.
	data = append(data, []byte("[core]\n\tsymlinks\n")...)
	if err := os.WriteFile(filepath.Join(gitDir, "config"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := HostGitCommand(context.Background(), repo, HostGitOptions{Policy: RepoPolicyManaged}, "status"); err != nil {
		t.Fatalf("valueless boolean refused: %v", err)
	}
}

// config.worktree is checked even with extensions.worktreeConfig OFF
// (fail closed), in both the main worktree and a linked worktree, and the
// subset check tolerates an allowed key git ignores.
func TestHostGitCommand_ConfigWorktreeAlwaysChecked(t *testing.T) {
	writeWT := func(gitDir, body string) {
		if err := os.WriteFile(filepath.Join(gitDir, "config.worktree"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("main worktree, extension off, refused key", func(t *testing.T) {
		repo, _ := newHostGitRepo(t)
		gitDir := hostRunGit(t, repo, "rev-parse", "--absolute-git-dir")
		writeWT(gitDir, "[core]\n\tsshCommand = ssh -v\n")
		_, err := HostGitCommand(context.Background(), repo, HostGitOptions{Policy: RepoPolicyLinked}, "status")
		var refused *RepoConfigRefusedError
		if !errors.As(err, &refused) || !strings.EqualFold(refused.Key, "core.sshCommand") {
			t.Fatalf("error = %v, want core.sshCommand refused (extension off)", err)
		}
	})
	t.Run("main worktree, extension off, allowed key tolerated", func(t *testing.T) {
		repo, _ := newHostGitRepo(t)
		gitDir := hostRunGit(t, repo, "rev-parse", "--absolute-git-dir")
		writeWT(gitDir, "[core]\n\tautocrlf = input\n")
		if _, err := HostGitCommand(context.Background(), repo, HostGitOptions{Policy: RepoPolicyLinked}, "status"); err != nil {
			t.Fatalf("allowed key in an ignored config.worktree refused: %v", err)
		}
	})
	t.Run("linked worktree config.worktree, refused key", func(t *testing.T) {
		repo, _ := newHostGitRepo(t)
		lwt := filepath.Join(t.TempDir(), "lwt")
		hostRunGit(t, repo, "worktree", "add", "-q", lwt, "-b", "lw")
		// The linked worktree's own admin dir.
		admin := strings.TrimSpace(hostRunGit(t, lwt, "rev-parse", "--absolute-git-dir"))
		for _, ext := range []bool{false, true} {
			if ext {
				hostRunGit(t, repo, "config", "extensions.worktreeConfig", "true")
			}
			if err := os.WriteFile(filepath.Join(admin, "config.worktree"), []byte("[core]\n\tfsmonitor = /tmp/x\n[core]\n\tsshCommand = ssh -v\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := HostGitCommand(context.Background(), lwt, HostGitOptions{Policy: RepoPolicyLinked}, "status")
			var refused *RepoConfigRefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("ext=%v: error = %v, want a refusal from the linked worktree's config.worktree", ext, err)
			}
		}
	})
}

func TestDeriveLFSEndpoint(t *testing.T) {
	cases := map[string]struct {
		want string
		ok   bool
	}{
		"https://github.com/o/r":       {"https://github.com/o/r.git/info/lfs", true},
		"https://github.com/o/r.git":   {"https://github.com/o/r.git/info/lfs", true},
		"https://github.com/o/r.git/":  {"https://github.com/o/r.git/info/lfs", true},
		"http://host/o/r":              {"http://host/o/r.git/info/lfs", true},
		"git@github.com:o/r.git":       {"", false},
		"ssh://git@github.com/o/r.git": {"", false},
		"":                             {"", false},
	}
	for in, exp := range cases {
		got, ok := DeriveLFSEndpoint(in)
		if ok != exp.ok || got != exp.want {
			t.Errorf("DeriveLFSEndpoint(%q) = %q,%v want %q,%v", in, got, ok, exp.want, exp.ok)
		}
	}
}

func TestScopedCredentialHelperEnv(t *testing.T) {
	env, ok := ScopedCredentialHelperEnv(nil, "https://github.com/o/r.git", "secret-tok")
	if !ok {
		t.Fatal("expected ok for an https URL")
	}
	joined := strings.Join(env, "\n")
	// An empty generic reset, then the host-scoped helper.
	if !strings.Contains(joined, "GIT_CONFIG_KEY_0=credential.helper\nGIT_CONFIG_VALUE_0=\n") {
		t.Errorf("missing the empty credential.helper reset: %q", env)
	}
	if !strings.Contains(joined, "GIT_CONFIG_KEY_1=credential.https://github.com.helper") {
		t.Errorf("credential not scoped to the host: %q", env)
	}
	if !strings.Contains(joined, "GIT_CONFIG_COUNT=2") {
		t.Errorf("wrong count: %q", env)
	}
	// Respects an existing count (starts at 2 -> adds 2 more -> 4).
	env2, _ := ScopedCredentialHelperEnv([]string{"GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=x.y", "GIT_CONFIG_VALUE_0=z", "GIT_CONFIG_KEY_1=a.b", "GIT_CONFIG_VALUE_1=c"}, "https://h/o/r", "t")
	j2 := strings.Join(env2, "\n")
	if !strings.Contains(j2, "GIT_CONFIG_COUNT=4") || !strings.Contains(j2, "GIT_CONFIG_KEY_3=credential.https://h.helper") {
		t.Errorf("did not append after existing count: %q", env2)
	}
	if _, ok := ScopedCredentialHelperEnv(nil, "git@github.com:o/r.git", "t"); ok {
		t.Error("expected not ok for a non-http(s) URL")
	}
}

// The token appears in the environment only as the scoped-credential variable,
// never in a GIT_CONFIG_VALUE (which the helper text reads at run time), and the
// helper text is a constant with no credential value.
func TestScopedCredentialHelperEnv_TokenOnlyInEnvVar(t *testing.T) {
	const token = "sup3r-s3cret-t0ken"
	env, ok := ScopedCredentialHelperEnv(nil, "https://github.com/o/r.git", token)
	if !ok {
		t.Fatal("expected ok for an https URL")
	}
	var carriers []string
	for _, kv := range env {
		if strings.Contains(kv, token) {
			carriers = append(carriers, kv)
		}
	}
	if len(carriers) != 1 || !strings.HasPrefix(carriers[0], scopedGitCredentialEnv+"=") {
		t.Errorf("token must appear only in %s, got carriers: %v", scopedGitCredentialEnv, carriers)
	}
	// No GIT_CONFIG_VALUE carries the token, and the helper text is a credential-free constant.
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_CONFIG_VALUE_") && strings.Contains(kv, token) {
			t.Errorf("a GIT_CONFIG_VALUE carries the token: %q", kv)
		}
	}
	if strings.Contains(scopedCredentialHelperText, token) {
		t.Error("the helper text constant must not contain any credential")
	}
}

// HostGitEnv drops an inherited scoped-credential variable, so it never reaches a
// host git call that did not set it. ScopedCredentialHelperEnv then re-adds it
// (scrub-then-append) for the one command that needs it.
func TestHostGitEnv_ScrubsScopedCredential(t *testing.T) {
	in := []string{"PATH=/usr/bin", scopedGitCredentialEnv + "=leftover-token", "HOME=/home/x"}
	for _, kv := range HostGitEnv(in) {
		if strings.HasPrefix(kv, scopedGitCredentialEnv+"=") {
			t.Errorf("HostGitEnv must drop an inherited %s, got: %q", scopedGitCredentialEnv, kv)
		}
	}
	// Scrub-then-append: scoping the scrubbed env re-adds exactly one carrier.
	scoped, ok := ScopedCredentialHelperEnv(HostGitEnv(in), "https://github.com/o/r.git", "tok")
	if !ok {
		t.Fatal("expected ok")
	}
	n := 0
	for _, kv := range scoped {
		if strings.HasPrefix(kv, scopedGitCredentialEnv+"=") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("expected exactly one scoped-credential var after scrub-then-scope, got %d", n)
	}
}

// End-to-end functional check of the PRODUCTION scoped-credential path: the env
// built by ScopedCredentialHelperEnv (which installs scopedCredentialHelperText)
// delivers the token to git (test d — e2e auth), while GIT_TRACE — which logs the
// credential helper's run_command argv — never shows the token. This fails if the
// token is ever formatted back into the helper text (the helper argv would carry
// it) or into a config value, so it kills that mutant. A shell-metacharacter token
// confirms the quoted ${VAR} expansion.
func TestScopedCredentialHelper_NoTokenOnAnyArgv(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	const token = `argv-must-not-have-me$;&|` // includes shell metacharacters
	dir := t.TempDir()

	// The production scoped env, built on an isolated base (no ambient helper or
	// token). ScopedCredentialHelperEnv installs scopedCredentialHelperText and the
	// SCION_SCOPED_GIT_CREDENTIAL value; HostGitEnv keeps the isolation vars.
	base := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + dir,
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_SYSTEM=" + os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
	}
	env, ok := ScopedCredentialHelperEnv(HostGitEnv(base), "https://example.com/o/r.git", token)
	if !ok {
		t.Fatal("ScopedCredentialHelperEnv returned ok=false for an https URL")
	}

	// GIT_TRACE=2 logs the credential helper's run_command argv to stderr.
	cmd := exec.Command("git", "credential", "fill")
	cmd.Dir = dir
	cmd.Env = append(env, "GIT_TRACE=2")
	cmd.Stdin = strings.NewReader("protocol=https\nhost=example.com\n\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git credential fill failed: %v\nstderr:\n%s", err, stderr.String())
	}
	// Test d: git received the exact token from the production helper.
	if !strings.Contains(stdout.String(), "password="+token) {
		t.Errorf("git did not receive the token from the scoped helper:\n%s", stdout.String())
	}
	// GIT_TRACE logs the helper's run_command argv; it must not contain the token,
	// so formatting the token back into the helper text is caught here.
	if strings.Contains(stderr.String(), token) {
		t.Error("the token appeared in the git trace (a helper or argv carried it)")
	}
}

// A managed-base worktree add carries GIT_LFS_SKIP_SMUDGE and no lfs.url.
func TestSafeGitCommandBuilderSkipsLFSSmudge(t *testing.T) {
	repo, _ := newHostGitRepo(t)
	cmd, err := HostGitCommand(context.Background(), repo, HostGitOptions{Policy: RepoPolicyManaged, SkipLFSSmudge: true}, "status")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cmd.Env, "GIT_LFS_SKIP_SMUDGE=1") {
		t.Errorf("env lacks GIT_LFS_SKIP_SMUDGE=1: %v", cmd.Env)
	}
}

// A linked checkout pins lfs.url from the trusted remote.
func TestHostGitCommandPinsLFSURL(t *testing.T) {
	repo, _ := newHostGitRepo(t)
	cmd, err := HostGitCommand(context.Background(), repo, HostGitOptions{Policy: RepoPolicyLinked, LFSURL: "https://host/o/r.git/info/lfs"}, "status")
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(cmd.Args, "\x00")
	if !strings.Contains(args, "\x00lfs.url=https://host/o/r.git/info/lfs\x00") {
		t.Errorf("args lack the pinned lfs.url: %q", cmd.Args)
	}
	if !strings.Contains(args, "\x00lfs.https://host/o/r.git/info/lfs.access=basic\x00") {
		t.Errorf("args lack the pinned lfs.<endpoint>.access=basic: %q", cmd.Args)
	}
}

// requireGitLFS skips the test when git-lfs is not installed. The live
// behaviour (git-lfs runs the smudge filter) can only be exercised with it
// present; the argv/env wiring is asserted unconditionally in the sibling unit
// tests (ScopedCredentialHelperEnv, SkipLFSSmudge env, the pinned lfs.url,
// DeriveLFSEndpoint).
func requireGitLFS(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git-lfs"); err != nil {
		t.Skip("git-lfs not installed; skipping the live git-lfs assertion (argv/env wiring is covered unconditionally elsewhere)")
	}
}

// lfsPointer is a valid git-lfs pointer (a 64-hex oid), which git-lfs will try
// to smudge (fetch) unless the smudge is skipped.
const lfsPointer = "version https://git-lfs.github.com/spec/v1\noid sha256:1111111111111111111111111111111111111111111111111111111111111111\nsize 3\n"

// configureGlobalLFSFilter writes filter.lfs.* into the test's isolated global
// config, as `git lfs install` does, so git-lfs's filter runs on checkout.
func configureGlobalLFSFilter(t *testing.T) {
	t.Helper()
	global := os.Getenv("GIT_CONFIG_GLOBAL")
	if global == "" {
		t.Fatal("GIT_CONFIG_GLOBAL not set by hostGitTestEnv")
	}
	body := "[filter \"lfs\"]\n\tclean = git-lfs clean -- %f\n\tsmudge = git-lfs smudge -- %f\n\tprocess = git-lfs filter-process\n\trequired = true\n"
	f, err := os.OpenFile(global, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(body); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
}

// recordingLFSServer returns an httptest server that records every request and
// fails the test if any carries an Authorization header. hits is the request
// count.
func recordingLFSServer(t *testing.T, hits *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		if r.Header.Get("Authorization") != "" {
			t.Errorf("the .lfsconfig endpoint received an Authorization header")
		}
		http.Error(w, "no", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// An agent-chosen .lfsconfig endpoint must never be contacted (and so never
// receive a host credential) by a host-side checkout. With git-lfs present and
// its filter configured globally, a valid LFS pointer would otherwise be
// smudged against the .lfsconfig endpoint.
func TestLFSConfigEndpointNeverContacted(t *testing.T) {
	requireGitLFS(t)

	t.Run("linked checkout, ssh origin, skips smudge", func(t *testing.T) {
		hostGitTestEnv(t)
		configureGlobalLFSFilter(t)
		var hits int32
		srv := recordingLFSServer(t, &hits)
		repo := filepath.Join(t.TempDir(), "repo")
		hostRunGit(t, filepath.Dir(repo), "init", "-q", "-b", "main", repo)
		hostRunGit(t, repo, "remote", "add", "origin", "git@example.com:o/r.git")
		writeFile(t, filepath.Join(repo, ".lfsconfig"), "[lfs]\n\turl = "+srv.URL+"/lfs\n")
		writeFile(t, filepath.Join(repo, ".gitattributes"), "*.bin filter=lfs -text\n")
		writeFile(t, filepath.Join(repo, "big.bin"), lfsPointer)
		hostRunGit(t, repo, "add", ".")
		hostRunGit(t, repo, "commit", "-q", "-m", "lfs")
		wt := filepath.Join(repo, "agents", "a", "workspace")
		if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := CreateWorktree(wt, "agent-a"); err != nil {
			t.Fatalf("CreateWorktree: %v", err)
		}
		if got := atomic.LoadInt32(&hits); got != 0 {
			t.Errorf(".lfsconfig endpoint contacted %d time(s); must be 0", got)
		}
	})

	t.Run("hub pull skips smudge", func(t *testing.T) {
		hostGitTestEnv(t)
		configureGlobalLFSFilter(t)
		var hits int32
		srv := recordingLFSServer(t, &hits)
		source, workspace, _ := sharedWorkspaceFixture(t)
		writeFile(t, filepath.Join(source, ".lfsconfig"), "[lfs]\n\turl = "+srv.URL+"/lfs\n")
		writeFile(t, filepath.Join(source, ".gitattributes"), "*.bin filter=lfs -text\n")
		writeFile(t, filepath.Join(source, "big.bin"), lfsPointer)
		hostRunGit(t, source, "add", ".")
		hostRunGit(t, source, "-c", "core.hooksPath="+os.DevNull, "commit", "-q", "-m", "lfs")
		if _, err := PullSharedWorkspace(workspace, source, ""); err != nil {
			t.Fatalf("PullSharedWorkspace: %v", err)
		}
		if got := atomic.LoadInt32(&hits); got != 0 {
			t.Errorf(".lfsconfig endpoint contacted %d time(s); must be 0", got)
		}
	})

	t.Run("linked checkout, http origin with .lfsconfig endpoint, skips (does not pin)", func(t *testing.T) {
		hostGitTestEnv(t)
		configureGlobalLFSFilter(t)
		var hits int32
		srv := recordingLFSServer(t, &hits)
		repo := filepath.Join(t.TempDir(), "repo")
		hostRunGit(t, filepath.Dir(repo), "init", "-q", "-b", "main", repo)
		// http(s) origin AND a tracked .lfsconfig naming a separate endpoint:
		// we must not pin the origin-derived endpoint and must not contact the
		// .lfsconfig one.
		hostRunGit(t, repo, "remote", "add", "origin", "https://git.example.com/o/r.git")
		writeFile(t, filepath.Join(repo, ".lfsconfig"), "[lfs]\n\turl = "+srv.URL+"/lfs\n")
		writeFile(t, filepath.Join(repo, ".gitattributes"), "*.bin filter=lfs -text\n")
		writeFile(t, filepath.Join(repo, "big.bin"), lfsPointer)
		hostRunGit(t, repo, "add", ".")
		hostRunGit(t, repo, "commit", "-q", "-m", "lfs")
		wt := filepath.Join(repo, "agents", "a", "workspace")
		if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := CreateWorktree(wt, "agent-a"); err != nil {
			t.Fatalf("CreateWorktree: %v", err)
		}
		if got := atomic.LoadInt32(&hits); got != 0 {
			t.Errorf(".lfsconfig endpoint contacted %d time(s); must be 0", got)
		}
	})
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The pointer-warning scan must not follow a symlink to a FIFO (which
// would block) or open non-regular files. A linked worktree with an ssh origin
// (skip-smudge path) that contains such a symlink must still complete.
func TestCreateWorktree_PointerScanDoesNotHangOnFIFOSymlink(t *testing.T) {
	hostGitTestEnv(t)
	repo := filepath.Join(t.TempDir(), "repo")
	hostRunGit(t, filepath.Dir(repo), "init", "-q", "-b", "main", repo)
	hostRunGit(t, repo, "remote", "add", "origin", "git@example.com:o/r.git") // ssh → skip-smudge
	writeFile(t, filepath.Join(repo, ".gitattributes"), "*.bin filter=lfs -text\n")
	// A FIFO outside the repo, and a tracked symlink pointing at it.
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := mkfifoForTest(fifo, 0o644); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}
	if err := os.Symlink(fifo, filepath.Join(repo, "link.bin")); err != nil {
		t.Fatal(err)
	}
	hostRunGit(t, repo, "add", ".")
	hostRunGit(t, repo, "commit", "-q", "-m", "fifo link")
	wt := filepath.Join(repo, "agents", "a", "workspace")
	if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- CreateWorktree(wt, "agent-a") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("CreateWorktree: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("CreateWorktree hung (pointer scan followed the FIFO symlink)")
	}
}

// A repository with initialised submodules (https URL, active/branch) is not
// refused by the linked policy; recursion is pinned off regardless.
func TestHostGitCommand_SubmoduleConfigAllowed(t *testing.T) {
	repo, _ := newHostGitRepo(t)
	for _, kv := range [][2]string{
		{"submodule.lib.url", "https://github.com/o/lib.git"},
		{"submodule.lib.active", "true"},
		{"submodule.lib.branch", "main"},
		{"submodule.lib.ignore", "dirty"},
		{"submodule.recurse", "false"},
	} {
		hostRunGit(t, repo, "config", kv[0], kv[1])
	}
	if _, err := HostGitCommand(context.Background(), repo, HostGitOptions{Policy: RepoPolicyLinked}, "status"); err != nil {
		t.Fatalf("a submodule config with an https url was refused: %v", err)
	}
	// An ssh-scp submodule url is also accepted.
	repo2, _ := newHostGitRepo(t)
	hostRunGit(t, repo2, "config", "submodule.lib.url", "git@github.com:o/lib.git")
	if _, err := HostGitCommand(context.Background(), repo2, HostGitOptions{Policy: RepoPolicyLinked}, "status"); err != nil {
		t.Fatalf("a submodule scp-ssh url was refused: %v", err)
	}
}

// A pull with a token and a non-http(s) clone URL ignores the token
// rather than failing (git uses its own transport auth).
func TestPullSharedWorkspace_TokenIgnoredForNonHTTPURL(t *testing.T) {
	source, workspace, _ := sharedWorkspaceFixture(t)
	commitFile(t, source, "new.txt", "new\n", "change")
	// source is a local path (non-http); a token must not make the pull fail.
	result, err := PullSharedWorkspace(workspace, source, "a-token")
	if err != nil {
		t.Fatalf("PullSharedWorkspace with a token on a local-path URL: %v", err)
	}
	if !result.Updated {
		t.Errorf("expected the pull to update")
	}
}

// An LFS repository (where git-lfs has written lfs.repositoryformatversion
// to .git/config) must not be refused. CreateWorktree and PullSharedWorkspace
// must each succeed twice in a row.
func TestLFSRepositoryNotRefusedAfterGitLFSWrites(t *testing.T) {
	requireGitLFS(t)

	t.Run("CreateWorktree twice", func(t *testing.T) {
		hostGitTestEnv(t)
		configureGlobalLFSFilter(t)
		repo := filepath.Join(t.TempDir(), "repo")
		hostRunGit(t, filepath.Dir(repo), "init", "-q", "-b", "main", repo)
		hostRunGit(t, repo, "remote", "add", "origin", "git@example.com:o/r.git")
		writeFile(t, filepath.Join(repo, ".gitattributes"), "*.bin filter=lfs -text\n")
		writeFile(t, filepath.Join(repo, "big.bin"), lfsPointer)
		hostRunGit(t, repo, "add", ".")
		hostRunGit(t, repo, "commit", "-q", "-m", "lfs")
		for i, branch := range []string{"agent-a", "agent-b"} {
			wt := filepath.Join(repo, "agents", branch, "workspace")
			if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := CreateWorktree(wt, branch); err != nil {
				t.Fatalf("CreateWorktree #%d: %v", i+1, err)
			}
		}
	})

	t.Run("PullSharedWorkspace twice", func(t *testing.T) {
		hostGitTestEnv(t)
		configureGlobalLFSFilter(t)
		source, workspace, _ := sharedWorkspaceFixture(t)
		writeFile(t, filepath.Join(source, ".gitattributes"), "*.bin filter=lfs -text\n")
		writeFile(t, filepath.Join(source, "a.bin"), lfsPointer)
		hostRunGit(t, source, "add", ".")
		hostRunGit(t, source, "-c", "core.hooksPath="+os.DevNull, "commit", "-q", "-m", "lfs")
		if _, err := PullSharedWorkspace(workspace, source, ""); err != nil {
			t.Fatalf("PullSharedWorkspace #1: %v", err)
		}
		writeFile(t, filepath.Join(source, "b.bin"), lfsPointer)
		hostRunGit(t, source, "add", ".")
		hostRunGit(t, source, "-c", "core.hooksPath="+os.DevNull, "commit", "-q", "-m", "lfs2")
		if _, err := PullSharedWorkspace(workspace, source, ""); err != nil {
			t.Fatalf("PullSharedWorkspace #2 (after git-lfs wrote repo config): %v", err)
		}
	})
}

// A committed .lfsconfig that sets lfs.<url>.access (even spelled
// differently from the origin-derived endpoint) makes the linked checkout skip
// the smudge rather than pin, so the endpoint is never contacted.
func TestLFSConfigAccessKeyForcesSkip(t *testing.T) {
	requireGitLFS(t)
	// The origin IS the recording server, so the origin-derived endpoint the
	// code would pin points at it. The committed .lfsconfig sets access on a
	// URL-equivalent but textually-different spelling of that same endpoint.
	// With the fix, the access key forces skip-smudge and the server gets no
	// request; if the code pinned instead, git-lfs would smudge against the
	// (origin-derived) endpoint and hit the recording server — so the
	// zero-contact assertion actually fires on a regression.
	for _, spelling := range []string{"trailing-slash", "uppercase-scheme"} {
		t.Run(spelling, func(t *testing.T) {
			hostGitTestEnv(t)
			configureGlobalLFSFilter(t)
			var hits int32
			srv := recordingLFSServer(t, &hits)
			origin := srv.URL + "/o/r.git"
			endpoint, ok := DeriveLFSEndpoint(origin)
			if !ok {
				t.Fatalf("could not derive the LFS endpoint for %q", origin)
			}
			u, err := neturl.Parse(endpoint)
			if err != nil {
				t.Fatal(err)
			}
			switch spelling {
			case "trailing-slash":
				u.Path += "/"
			case "uppercase-scheme":
				u.Scheme = strings.ToUpper(u.Scheme)
			}
			configKey := u.String()
			// Fail if it silently degrades to the exact pinned spelling (then it
			// would not exercise an equivalent-but-different key at all).
			if configKey == endpoint {
				t.Fatalf("spelling %q did not change the endpoint (%q); the case would not test an equivalent spelling", spelling, endpoint)
			}
			// A test-local credential helper (written to the test's own
			// GIT_CONFIG_GLOBAL temp file, never the real global/system config)
			// and no terminal prompt, so that if the guard is removed git-lfs
			// actually AUTHENTICATES and sends a request — the hit then proves
			// the endpoint was contacted, not that credentials were missing.
			// With the guard in place the smudge is skipped, so the endpoint is
			// still not contacted even though a credential is available.
			addTestCredentialHelper(t, srv.URL)
			t.Setenv("GIT_TERMINAL_PROMPT", "0")
			repo := filepath.Join(t.TempDir(), "repo")
			hostRunGit(t, filepath.Dir(repo), "init", "-q", "-b", "main", repo)
			hostRunGit(t, repo, "remote", "add", "origin", origin)
			writeFile(t, filepath.Join(repo, ".lfsconfig"), "[lfs \""+configKey+"\"]\n\taccess = basic\n")
			writeFile(t, filepath.Join(repo, ".gitattributes"), "*.bin filter=lfs -text\n")
			writeFile(t, filepath.Join(repo, "big.bin"), lfsPointer)
			hostRunGit(t, repo, "add", ".")
			hostRunGit(t, repo, "commit", "-q", "-m", "lfs")
			wt := filepath.Join(repo, "agents", "a", "workspace")
			if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
				t.Fatal(err)
			}
			err = CreateWorktree(wt, "agent-a")
			// Check the hit count BEFORE acting on the CreateWorktree error, so
			// the contact (or its absence) is always reported.
			if got := atomic.LoadInt32(&hits); got != 0 {
				t.Errorf("endpoint contacted %d time(s); a .lfsconfig access key must force skip-smudge", got)
			}
			if err != nil {
				t.Fatalf("CreateWorktree: %v", err)
			}
		})
	}
}

// lfsConfigNamesEndpoint (unit): any lfs.url/pushurl, remote.*.lfsurl or
// lfs.<url>.access in the committed .lfsconfig forces skip-smudge.
func TestLfsConfigNamesEndpoint(t *testing.T) {
	hostGitTestEnv(t)
	cases := map[string]bool{
		"":                                   false,
		"[lfs]\n\turl = https://x/lfs\n":     true,
		"[lfs]\n\tpushurl = https://x/lfs\n": true,
		"[remote \"origin\"]\n\tlfsurl = https://x":           true,
		"[lfs \"https://x:443/lfs\"]\n\taccess = negotiate\n": true,
		"[lfs]\n\tconcurrenttransfers = 4\n":                  false, // inert, not an endpoint
		"[lfs\n\tbroken":                                      true,  // present but unparseable -> fail closed
	}
	for body, want := range cases {
		repo := filepath.Join(t.TempDir(), "repo")
		hostRunGit(t, filepath.Dir(repo), "init", "-q", "-b", "main", repo)
		if body != "" {
			writeFile(t, filepath.Join(repo, ".lfsconfig"), body)
			hostRunGit(t, repo, "add", ".")
			hostRunGit(t, repo, "-c", "core.hooksPath="+os.DevNull, "commit", "-q", "-m", "c")
		} else {
			hostRunGit(t, repo, "-c", "core.hooksPath="+os.DevNull, "commit", "-q", "--allow-empty", "-m", "empty")
		}
		if got := lfsConfigNamesEndpoint(repo, "HEAD"); got != want {
			t.Errorf("lfsConfigNamesEndpoint(%q) = %v, want %v", body, got, want)
		}
	}
}

// A tracked .gitattributes that is itself a symlink to a FIFO or
// /dev/zero must not hang or unbounded-read CreateWorktree. repoTracksLFS reads
// it through the regular-file-only safe reader.
func TestCreateWorktree_GitattributesSymlinkDoesNotHang(t *testing.T) {
	for _, target := range []string{"fifo", "devzero"} {
		t.Run(target, func(t *testing.T) {
			hostGitTestEnv(t)
			repo := filepath.Join(t.TempDir(), "repo")
			hostRunGit(t, filepath.Dir(repo), "init", "-q", "-b", "main", repo)
			hostRunGit(t, repo, "remote", "add", "origin", "git@example.com:o/r.git") // ssh → skip path runs repoTracksLFS
			var dst string
			switch target {
			case "fifo":
				dst = filepath.Join(t.TempDir(), "fifo")
				if err := mkfifoForTest(dst, 0o644); err != nil {
					t.Skipf("mkfifo unsupported: %v", err)
				}
			case "devzero":
				if _, err := os.Stat("/dev/zero"); err != nil {
					t.Skip("/dev/zero unavailable")
				}
				dst = "/dev/zero"
			}
			if err := os.Symlink(dst, filepath.Join(repo, ".gitattributes")); err != nil {
				t.Fatal(err)
			}
			hostRunGit(t, repo, "add", ".")
			hostRunGit(t, repo, "-c", "core.hooksPath="+os.DevNull, "commit", "-q", "-m", "attr link")
			wt := filepath.Join(repo, "agents", "a", "workspace")
			if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- CreateWorktree(wt, "agent-a") }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("CreateWorktree: %v", err)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("CreateWorktree hung reading a .gitattributes symlink")
			}
		})
	}
}

// The post-command sensitive-key check catches a reordered
// or inserted multi-valued key (remote.origin.url).
func TestVerifyConfigStable_MultiValuedKeyChange(t *testing.T) {
	hostGitTestEnv(t)
	repo := filepath.Join(t.TempDir(), "repo")
	hostRunGit(t, filepath.Dir(repo), "init", "-q", "-b", "main", repo)
	hostRunGit(t, repo, "remote", "add", "origin", "https://github.com/o/r.git")
	// Add a second remote.origin.url with the SAME value: the last value is
	// unchanged, so a last-wins (collapsed) snapshot would miss it, but the
	// ordered-list snapshot sees [url] -> [url, url] and reports a change. (git
	// allows duplicate multi-values; it uses the first url for fetch.)
	cmd, err := HostGitCommand(context.Background(), repo, HostGitOptions{Policy: RepoPolicyLinked},
		"config", "--add", "remote.origin.url", "https://github.com/o/r.git")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cmd.CombinedOutput(); !errors.Is(err, ErrRepoConfigChanged) {
		t.Errorf("inserting a second remote.origin.url: error = %v, want ErrRepoConfigChanged", err)
	}
}

// addTestCredentialHelper writes a credential helper to the test's own
// GIT_CONFIG_GLOBAL temp file (set by hostGitTestEnv) scoped to serverURL's
// host, returning a fixed dummy username/password. It is test-local only
// (a temp config, never the real global or system config), so git/git-lfs can
// authenticate to the recording server during the test.
func addTestCredentialHelper(t *testing.T, serverURL string) {
	t.Helper()
	global := os.Getenv("GIT_CONFIG_GLOBAL")
	if global == "" {
		t.Fatal("GIT_CONFIG_GLOBAL not set by hostGitTestEnv")
	}
	f, err := os.OpenFile(global, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Scope to the server's scheme://host[:port]; matches any path on it.
	body := "[credential \"" + serverURL + "\"]\n\thelper = \"!f() { echo username=ci; echo password=ci-token; }; f\"\n"
	if _, err := f.WriteString(body); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
}

// lfsOptionForLinkedCheckout reads the .lfsconfig of the ref being checked
// out, not the root's HEAD. A branch that adds an endpoint is skipped even when
// HEAD has none; a branch with none (and an http origin) is pinned.
func TestLfsOptionForLinkedCheckout_ReadsBranchRef(t *testing.T) {
	hostGitTestEnv(t)
	repo := filepath.Join(t.TempDir(), "repo")
	hostRunGit(t, filepath.Dir(repo), "init", "-q", "-b", "main", repo)
	hostRunGit(t, repo, "remote", "add", "origin", "https://host.example/o/r.git")
	// main (HEAD): no .lfsconfig endpoint.
	writeFile(t, filepath.Join(repo, "f.txt"), "x\n")
	hostRunGit(t, repo, "add", ".")
	hostRunGit(t, repo, "-c", "core.hooksPath="+os.DevNull, "commit", "-q", "-m", "main")
	// feat: adds a .lfsconfig naming an endpoint.
	hostRunGit(t, repo, "checkout", "-q", "-b", "feat")
	writeFile(t, filepath.Join(repo, ".lfsconfig"), "[lfs]\n\turl = https://lfs.example/lfs\n")
	hostRunGit(t, repo, "add", ".")
	hostRunGit(t, repo, "-c", "core.hooksPath="+os.DevNull, "commit", "-q", "-m", "feat lfs")
	hostRunGit(t, repo, "checkout", "-q", "main")

	if opt := lfsOptionForLinkedCheckout(repo, "feat", ""); !opt.skipSmudge {
		t.Errorf("branch feat names an endpoint: want skipSmudge, got %+v", opt)
	}
	if opt := lfsOptionForLinkedCheckout(repo, "main", ""); opt.skipSmudge || opt.url == "" {
		t.Errorf("branch main has no endpoint (http origin): want pinned url, got %+v", opt)
	}
	// A new branch (does not exist yet) reads HEAD, which is main → pinned.
	if opt := lfsOptionForLinkedCheckout(repo, "brand-new", ""); opt.skipSmudge || opt.url == "" {
		t.Errorf("new branch reads HEAD (no endpoint): want pinned url, got %+v", opt)
	}
}

// The git-lfs endpoint is pinned from the broker-recorded origin, not the
// live remote.origin.url. When the live origin no longer matches the recorded
// one (an agent repointed it), the smudge is skipped rather than pinned.
func TestLfsOptionForLinkedCheckout_RecordedOrigin(t *testing.T) {
	hostGitTestEnv(t)
	repo := filepath.Join(t.TempDir(), "repo")
	hostRunGit(t, filepath.Dir(repo), "init", "-q", "-b", "main", repo)
	writeFile(t, filepath.Join(repo, "f.txt"), "x\n")
	hostRunGit(t, repo, "add", ".")
	hostRunGit(t, repo, "-c", "core.hooksPath="+os.DevNull, "commit", "-q", "-m", "main")

	const recorded = "https://host.example/o/r.git"

	// Live origin matches the recorded origin (http): pin the recorded endpoint.
	hostRunGit(t, repo, "remote", "add", "origin", recorded)
	opt := lfsOptionForLinkedCheckout(repo, "main", recorded)
	if opt.skipSmudge || opt.url == "" {
		t.Fatalf("live origin matches recorded (http): want pinned url, got %+v", opt)
	}
	if want, _ := DeriveLFSEndpoint(recorded); opt.url != want {
		t.Errorf("pinned url = %q, want endpoint derived from the recorded origin %q", opt.url, want)
	}

	// The live origin is repointed elsewhere: it no longer matches the recorded
	// one, so skip rather than pin (never contact that host).
	hostRunGit(t, repo, "remote", "set-url", "origin", "https://other.example/x.git")
	if opt := lfsOptionForLinkedCheckout(repo, "main", recorded); !opt.skipSmudge {
		t.Errorf("live origin repointed away from recorded: want skipSmudge, got %+v", opt)
	}

	// An ssh recorded origin cannot derive an http(s) endpoint: skip.
	hostRunGit(t, repo, "remote", "set-url", "origin", "git@host.example:o/r.git")
	if opt := lfsOptionForLinkedCheckout(repo, "main", "git@host.example:o/r.git"); !opt.skipSmudge {
		t.Errorf("ssh recorded origin: want skipSmudge, got %+v", opt)
	}
}

// A committed .lfsconfig whose blob is unreadable (present in the tree but the
// object is missing) must be treated as naming an endpoint — skip the smudge,
// not pin — just like an unparseable one. `git show` reports the same error for
// an absent and an unreadable path, so the code distinguishes them with ls-tree.
func TestLfsOptionForLinkedCheckout_UnreadableLfsConfigSkips(t *testing.T) {
	hostGitTestEnv(t)
	repo := filepath.Join(t.TempDir(), "repo")
	hostRunGit(t, filepath.Dir(repo), "init", "-q", "-b", "main", repo)
	hostRunGit(t, repo, "remote", "add", "origin", "https://host.example/o/r.git")
	writeFile(t, filepath.Join(repo, ".lfsconfig"), "[lfs]\n\turl = https://lfs.example/lfs\n")
	hostRunGit(t, repo, "add", ".")
	hostRunGit(t, repo, "-c", "core.hooksPath="+os.DevNull, "commit", "-q", "-m", "lfs")

	// Control: with the blob readable, the endpoint is seen and the smudge skips.
	if opt := lfsOptionForLinkedCheckout(repo, "main", ""); !opt.skipSmudge {
		t.Fatalf("readable .lfsconfig names an endpoint: want skipSmudge, got %+v", opt)
	}

	// Remove the blob object so `git show main:.lfsconfig` fails while ls-tree
	// still reports the entry.
	sha := strings.TrimSpace(string(mustOutput(t, repo, "rev-parse", "main:.lfsconfig")))
	if len(sha) < 3 {
		t.Fatalf("unexpected blob sha %q", sha)
	}
	obj := filepath.Join(repo, ".git", "objects", sha[:2], sha[2:])
	if err := os.Remove(obj); err != nil {
		t.Fatalf("remove blob object: %v", err)
	}
	// Confirm the blob is now unreadable but the tree entry remains.
	if _, err := mustOutputErr(repo, "show", "main:.lfsconfig"); err == nil {
		t.Skip("git still served the removed blob (packed elsewhere); cannot exercise the unreadable path")
	}
	if opt := lfsOptionForLinkedCheckout(repo, "main", ""); !opt.skipSmudge {
		t.Errorf("unreadable .lfsconfig must skip the smudge (fail closed), got %+v", opt)
	}
}

func mustOutput(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	out, err := mustOutputErr(dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return out
}

func mustOutputErr(dir string, args ...string) ([]byte, error) {
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	return c.CombinedOutput()
}

// The host-side guard refuses a root-level .git/commondir or gitdir in a linked
// checkout (a .git directory): such a file would redirect the repository's common
// git dir — where host git reads config, hooks and refs — through an
// agent-writable path. A .git gitfile (a linked worktree) is not the target.
func TestRefuseRootCommonDirRedirect(t *testing.T) {
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A normal .git directory (no root commondir/gitdir) passes.
	if err := refuseRootCommonDirRedirect(dir, false, AudienceLocal); err != nil {
		t.Fatalf("a .git with no root commondir/gitdir must pass, got: %v", err)
	}
	// A root-level commondir or gitdir is refused.
	for _, name := range []string{"commondir", "gitdir"} {
		p := filepath.Join(gitDir, name)
		if err := os.WriteFile(p, []byte("../elsewhere\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		err := refuseRootCommonDirRedirect(dir, false, AudienceLocal)
		var refused *RepoConfigRefusedError
		if !errors.As(err, &refused) {
			t.Errorf("a root .git/%s must be refused with RepoConfigRefusedError, got: %v", name, err)
		} else if refused.Dir != dir || refused.Seeded {
			// The host form keeps the directory and is not a seed refusal.
			t.Errorf("host refusal = dir %q seeded %v; want %q, false", refused.Dir, refused.Seeded, dir)
		}
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	// A .git gitfile (a linked worktree, which legitimately resolves a commondir
	// inside its admin dir) is not the target of this guard.
	if err := os.RemoveAll(gitDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gitDir, []byte("gitdir: /somewhere/worktrees/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := refuseRootCommonDirRedirect(dir, false, AudienceLocal); err != nil {
		t.Errorf("a .git gitfile (linked worktree) must not be refused, got: %v", err)
	}
}

// Host-side git refuses, through HostGitCommand's policy check, to run in a
// repository whose .git directory carries a root-level commondir or gitdir (a
// redirect of the common git dir). The command is refused before any git runs.
func TestHostGitCommand_RefusesRootCommonDirRedirect(t *testing.T) {
	for _, name := range []string{"commondir", "gitdir"} {
		t.Run(name, func(t *testing.T) {
			repo, _ := newHostGitRepo(t)
			if err := os.WriteFile(filepath.Join(repo, ".git", name), []byte("../elsewhere\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := HostGitCommand(context.Background(), repo, HostGitOptions{Policy: RepoPolicyLinked}, "status")
			var refused *RepoConfigRefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("error = %v, want *RepoConfigRefusedError for a root .git/%s", err, name)
			}
			if !strings.Contains(refused.Key, name) {
				t.Errorf("refused key = %q, want it to name .git/%s", refused.Key, name)
			}
		})
	}
}

// fakeGitOnPath puts a git wrapper first on PATH. For every call it appends a
// record to the returned log: "ARGV\t<argv>", one "ENV\t<k=v>" line per GIT_*
// variable it was given, and "END". It acts instead of git for a few marker
// subcommands and runs the real git otherwise:
//   - fake-stderr: writes fakeGitStderr to stderr and exits 3;
//   - fake-mutate: appends a refused key to ./.git/config and exits 0;
//   - fake-mkgit: creates ./.git and exits 0;
//   - fake-block: creates ./.fake-started, then becomes sleep 30;
//   - fake-hold: leaves a background sleep holding stdout and stderr open,
//     writes its PID to ./.fake-hold-pid, and exits 0;
//   - fake-arm: creates ./.fake-armed and exits 0; from then on a config
//     listing in that directory becomes sleep 120 (so the after-run check
//     blocks).
func fakeGitOnPath(t *testing.T) (log string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the git wrapper is a shell script")
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	log = filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\n" +
		"{ printf 'ARGV\\t%s\\n' \"$*\"; env | grep '^GIT_' | sort | sed 's/^/ENV\\t/'; printf 'END\\n'; } >> '" + log + "'\n" +
		"case \" $* \" in\n" +
		"*' fake-stderr '*) printf '" + strings.ReplaceAll(fakeGitStderr, "\n", "\\n") + "' >&2; exit 3;;\n" +
		"*' fake-mutate '*) printf '[core]\\n\\tsshCommand = fake-marker-m1\\n' >> .git/config; exit 0;;\n" +
		"*' fake-mkgit '*) mkdir .git; exit 0;;\n" +
		"*' fake-block '*) touch .fake-started; exec sleep 30;;\n" +
		"*' fake-hold '*) sleep 30 & echo $! > .fake-hold-pid; exit 0;;\n" +
		"*' fake-arm '*) touch .fake-armed; exit 0;;\n" +
		"*' config --show-scope '*) if [ -e .fake-armed ]; then exec sleep 120; fi;;\n" +
		"esac\n" +
		"exec '" + realGit + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

// fakeGitStderr is what the wrapper's fake-stderr writes: leading and
// trailing blanks, a tab and two lines, so any trimming or rewriting shows.
const fakeGitStderr = " raw: fake-marker-e1\n\tsecond line \n"

// fakeGitCall is one recorded call of the wrapper.
type fakeGitCall struct {
	argv string
	env  map[string]string // GIT_* variables only
}

// fakeGitCalls parses the wrapper's log.
func fakeGitCalls(t *testing.T, log string) []fakeGitCall {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var calls []fakeGitCall
	var cur *fakeGitCall
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "ARGV\t"):
			calls = append(calls, fakeGitCall{argv: strings.TrimPrefix(line, "ARGV\t"), env: map[string]string{}})
			cur = &calls[len(calls)-1]
		case strings.HasPrefix(line, "ENV\t") && cur != nil:
			k, v, _ := strings.Cut(strings.TrimPrefix(line, "ENV\t"), "=")
			cur.env[k] = v
		}
	}
	return calls
}

// fakeGitRuns counts the recorded calls whose argv ends with suffix.
func fakeGitRuns(t *testing.T, log, suffix string) int {
	t.Helper()
	n := 0
	for _, c := range fakeGitCalls(t, log) {
		if strings.HasSuffix(c.argv, suffix) {
			n++
		}
	}
	return n
}

// gitEnvMap returns the GIT_* entries of env, the last value winning.
func gitEnvMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		if k, v, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "GIT_") {
			m[k] = v
		}
	}
	return m
}

// diffEnvKeys names the keys whose presence or value differs (no values).
func diffEnvKeys(got, want map[string]string) []string {
	var keys []string
	for k, v := range want {
		if g, ok := got[k]; !ok || g != v {
			keys = append(keys, k)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// plantInheritedGitEnv sets variables that would choose a repository or add
// config if they reached a host-side git call.
func plantInheritedGitEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "planted-git-dir"))
	t.Setenv("GIT_CONFIG_PARAMETERS", "'core.pager=plantmarker'")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.pager")
	t.Setenv("GIT_CONFIG_VALUE_0", "plantmarker")
	t.Setenv("GIT_INDEX_FILE", filepath.Join(t.TempDir(), "planted-index"))
	t.Setenv("GIT_WORK_TREE", filepath.Join(t.TempDir(), "planted-work-tree"))
}

var plantedGitEnvKeys = []string{"GIT_DIR", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0", "GIT_INDEX_FILE", "GIT_WORK_TREE"}

func TestRunHostGit_ExitCode(t *testing.T) {
	repo, _ := newHostGitRepo(t)
	if res := RunHostGit(context.Background(), repo, HostGitOptions{}, "show-ref", "--verify", "--quiet", "refs/heads/main"); res.Err != nil || res.ExitCode != 0 {
		t.Errorf("present ref: exit %d, err %v; want 0, nil", res.ExitCode, res.Err)
	}
	res := RunHostGit(context.Background(), repo, HostGitOptions{}, "show-ref", "--verify", "--quiet", "refs/heads/missing")
	if res.Err != nil || res.ExitCode != 1 {
		t.Errorf("missing ref: exit %d, err %v; want 1, nil (a normal exit is not an error)", res.ExitCode, res.Err)
	}
	if res := RunHostGit(context.Background(), repo, HostGitOptions{}, "rev-parse", "--show-toplevel"); res.ExitCode != 0 || strings.TrimSpace(string(res.Stdout)) == "" {
		t.Errorf("rev-parse: exit %d, stdout %q", res.ExitCode, res.Stdout)
	}
}

// Stderr comes back byte for byte, with the exit code, and git runs exactly
// once (no retry) whether it succeeds or fails.
func TestRunHostGit_RawStderrAndOneExec(t *testing.T) {
	repo, _ := newHostGitRepo(t)
	log := fakeGitOnPath(t)

	res := RunHostGit(context.Background(), repo, HostGitOptions{}, "fake-stderr")
	if res.Err != nil {
		t.Fatalf("Err = %v; a normal exit 3 is not an error", res.Err)
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", res.ExitCode)
	}
	if string(res.Stderr) != fakeGitStderr {
		t.Errorf("Stderr = %q, want exactly %q", res.Stderr, fakeGitStderr)
	}
	if n := fakeGitRuns(t, log, " fake-stderr"); n != 1 {
		t.Errorf("a failing command ran %d times, want exactly 1", n)
	}

	res = RunHostGit(context.Background(), repo, HostGitOptions{}, "rev-parse", "--show-toplevel")
	if res.Err != nil || res.ExitCode != 0 {
		t.Fatalf("rev-parse: exit %d, err %v", res.ExitCode, res.Err)
	}
	if n := fakeGitRuns(t, log, " rev-parse --show-toplevel"); n != 1 {
		t.Errorf("a succeeding command ran %d times, want exactly 1", n)
	}
}

// A process killed through ctx reports ExitCode -1 and an error that is
// ctx's, named once. Cancellation waits for git to signal that it started,
// so no wall-clock deadline is involved.
func TestRunHostGit_KilledByContext(t *testing.T) {
	repo, _ := newHostGitRepo(t)
	fakeGitOnPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	marker := filepath.Join(repo, ".fake-started")
	go func() {
		for {
			if _, err := os.Stat(marker); err == nil {
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()
	res := RunHostGit(ctx, repo, HostGitOptions{}, "fake-block")
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 for a killed process", res.ExitCode)
	}
	if !errors.Is(res.Err, context.Canceled) {
		t.Fatalf("Err = %v, want it to wrap context.Canceled", res.Err)
	}
	if n := strings.Count(res.Err.Error(), context.Canceled.Error()); n != 1 {
		t.Errorf("Err %q names %q %d times, want once", res.Err, context.Canceled.Error(), n)
	}
	// The process error is kept alongside ctx's.
	var exitErr *exec.ExitError
	if !errors.As(res.Err, &exitErr) {
		t.Errorf("Err %v does not keep the *exec.ExitError", res.Err)
	}
}

// A Ceiling value other than CeilingOn and CeilingOff is refused before
// anything runs, with an error that names no path.
func TestRunHostGit_UnknownCeilingRefuses(t *testing.T) {
	repo, _ := newHostGitRepo(t)
	log := fakeGitOnPath(t)
	res := RunHostGit(context.Background(), repo, HostGitOptions{Ceiling: HostGitCeiling(2)}, "rev-parse", "--show-toplevel")
	if !errors.Is(res.Err, errHostGitCeilingInvalid) {
		t.Fatalf("Err = %v, want the unknown-setting refusal", res.Err)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", res.ExitCode)
	}
	if strings.Contains(res.Err.Error(), repo) {
		t.Errorf("refusal names the repository path: %q", res.Err)
	}
	if calls := fakeGitCalls(t, log); len(calls) != 0 {
		t.Errorf("git ran %d times for a refused setting", len(calls))
	}
}

// A descendant that keeps git's output pipes open does not keep RunHostGit
// waiting past hostGitWaitDelay once git has exited.
func TestRunHostGit_WaitDelayBoundsHeldPipes(t *testing.T) {
	repo, _ := newHostGitRepo(t)
	fakeGitOnPath(t)
	// Kill the descendant the fake left behind once the test is done.
	t.Cleanup(func() {
		data, err := os.ReadFile(filepath.Join(repo, ".fake-hold-pid"))
		if err != nil {
			return
		}
		var pid int
		if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid); err != nil || pid <= 0 {
			return
		}
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill() // it may have exited already
		}
	})
	start := time.Now()
	res := RunHostGit(context.Background(), repo, HostGitOptions{}, "fake-hold")
	if elapsed := time.Since(start); elapsed > hostGitWaitDelay+10*time.Second {
		t.Errorf("RunHostGit returned after %v, want within %v plus slack", elapsed, hostGitWaitDelay)
	}
	if !errors.Is(res.Err, exec.ErrWaitDelay) {
		t.Errorf("Err = %v, want exec.ErrWaitDelay", res.Err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want git's own 0", res.ExitCode)
	}
}

// The after-run check runs with its own bounded context: a config listing
// that blocks during the check is killed by hostGitVerifyTimeout and
// reported as a config change, whatever the caller's context.
func TestRunHostGit_VerifyIsBounded(t *testing.T) {
	repo, _ := newHostGitRepo(t)
	log := fakeGitOnPath(t)
	start := time.Now()
	res := RunHostGit(context.Background(), repo, HostGitOptions{}, "fake-arm")
	elapsed := time.Since(start)
	if elapsed > hostGitVerifyTimeout+15*time.Second {
		t.Errorf("RunHostGit returned after %v, want within %v plus slack", elapsed, hostGitVerifyTimeout)
	}
	if !errors.Is(res.Err, ErrRepoConfigChanged) {
		t.Errorf("Err = %v, want ErrRepoConfigChanged from the blocked check", res.Err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want git's own 0", res.ExitCode)
	}
	// The check did start after the command, so the bound is what ended it.
	var after bool
	for _, c := range fakeGitCalls(t, log) {
		if strings.HasSuffix(c.argv, " fake-arm") {
			after = true
			continue
		}
		if after && strings.HasSuffix(c.argv, " config --show-scope -z --list") {
			return
		}
	}
	t.Error("no config listing was recorded after the command")
}

// A refused repository config is Err, before git runs the command. For the
// remote audience the refusal carries no directory in any field or sink.
func TestRunHostGit_PolicyRefusalIsErr(t *testing.T) {
	repo, _ := newHostGitRepo(t)
	hostRunGit(t, repo, "config", "core.sshCommand", "ssh -v")
	log := fakeGitOnPath(t)
	for _, audience := range []Audience{AudienceLocal, AudienceRemote} {
		res := RunHostGit(context.Background(), repo, HostGitOptions{Audience: audience}, "rev-parse", "--show-toplevel")
		var refused *RepoConfigRefusedError
		if !errors.As(res.Err, &refused) {
			t.Fatalf("audience %v: Err = %v, want *RepoConfigRefusedError", audience, res.Err)
		}
		if res.ExitCode != -1 || len(res.Stdout) != 0 {
			t.Errorf("audience %v: refused command reported exit %d, stdout %q", audience, res.ExitCode, res.Stdout)
		}
		if refused.Audience != audience {
			t.Errorf("refusal audience = %v, want %v", refused.Audience, audience)
		}
		if audience == AudienceRemote {
			if refused.Dir != "" {
				t.Errorf("remote refusal carries Dir %q", refused.Dir)
			}
			for sink, got := range refusalSinks(t, res.Err) {
				if strings.Contains(got, repo) {
					t.Errorf("%s of a remote refusal prints the repository path: %q", sink, got)
				}
			}
		} else if !strings.Contains(res.Err.Error(), repo) {
			t.Errorf("local refusal %q does not name the repository", res.Err)
		}
	}
	if n := fakeGitRuns(t, log, " rev-parse --show-toplevel"); n != 0 {
		t.Errorf("the refused command ran %d times", n)
	}
}

// A repository config change during the run is Err (ErrRepoConfigChanged),
// whatever git's own exit.
func TestRunHostGit_ConfigChangedDuringRunIsErr(t *testing.T) {
	repo, _ := newHostGitRepo(t)
	fakeGitOnPath(t)
	res := RunHostGit(context.Background(), repo, HostGitOptions{}, "fake-mutate")
	if !errors.Is(res.Err, ErrRepoConfigChanged) {
		t.Fatalf("Err = %v, want ErrRepoConfigChanged", res.Err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want git's own 0", res.ExitCode)
	}
}

// ceilingFixture returns a repository "parent" with an empty work tree. With
// poison, its config is unparseable, so any git that reads it fails with
// "bad config".
func ceilingFixture(t *testing.T, poison bool) (parent string) {
	t.Helper()
	hostGitTestEnv(t)
	parent = filepath.Join(t.TempDir(), "parent")
	hostRunGit(t, filepath.Dir(parent), "init", "-q", "-b", "main", parent)
	if poison {
		f, err := os.OpenFile(filepath.Join(parent, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString("[core\n"); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return parent
}

// ceilingLeaf makes the directory rel (slash-separated) under parent.
func ceilingLeaf(t *testing.T, parent, rel string) string {
	t.Helper()
	leaf := filepath.Join(parent, filepath.FromSlash(rel))
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		t.Fatal(err)
	}
	return leaf
}

func ceilingEnv() []string {
	return append(HostGitEnv(os.Environ()), "LC_ALL=C")
}

// Ceiling (a): by default git in a non-repository leaf inside a repository
// finds no repository and never reads the enclosing repository's config: a
// direct child of the work-tree root, a deeper leaf, and a leaf reached
// through a symlink from elsewhere.
func TestRunHostGit_CeilingDefaultStopsAtLeaf(t *testing.T) {
	parent := ceilingFixture(t, true)
	deep := ceilingLeaf(t, parent, "sub/leaf")
	child := ceilingLeaf(t, parent, "child")
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(deep, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	for _, dir := range []string{child, deep, link} {
		res := RunHostGit(context.Background(), dir, HostGitOptions{Env: ceilingEnv(), Audience: AudienceRemote}, "rev-parse", "--is-inside-work-tree")
		if res.Err != nil {
			t.Fatalf("%s: Err = %v", dir, res.Err)
		}
		if res.ExitCode != 128 || !strings.Contains(string(res.Stderr), "not a git repository") {
			t.Errorf("%s: exit %d, stderr %q; want 128 and not a git repository", dir, res.ExitCode, res.Stderr)
		}
		if strings.Contains(string(res.Stderr), "bad config") {
			t.Errorf("%s: git read the enclosing repository's config: %q", dir, res.Stderr)
		}
	}
}

// Ceiling (b): with CeilingOff git discovers the enclosing repository.
func TestRunHostGit_CeilingOffDiscoversParent(t *testing.T) {
	leaf := ceilingLeaf(t, ceilingFixture(t, false), "sub/leaf")
	res := RunHostGit(context.Background(), leaf, HostGitOptions{Env: ceilingEnv(), Ceiling: CeilingOff, Audience: AudienceRemote}, "rev-parse", "--is-inside-work-tree")
	if res.Err != nil || res.ExitCode != 0 || string(res.Stdout) != "true\n" {
		t.Errorf("exit %d, err %v, stdout %q, stderr %q; want 0 and true", res.ExitCode, res.Err, res.Stdout, res.Stderr)
	}
}

// A ceiling whose resolved path contains the path-list separator would be
// split by git into unrelated ceilings: refused before git runs, with an
// error that names no path, for both audiences.
func TestRunHostGit_CeilingRefusesListSeparator(t *testing.T) {
	parent := ceilingFixture(t, true)
	leaf := ceilingLeaf(t, parent, "we"+string(filepath.ListSeparator)+"ird/leaf")
	for _, audience := range []Audience{AudienceLocal, AudienceRemote} {
		res := RunHostGit(context.Background(), leaf, HostGitOptions{Env: ceilingEnv(), Audience: audience}, "rev-parse", "--is-inside-work-tree")
		if !errors.Is(res.Err, errHostGitCeilingSeparator) {
			t.Fatalf("audience %v: Err = %v, want the separator refusal", audience, res.Err)
		}
		if res.ExitCode != -1 || len(res.Stderr) != 0 {
			t.Errorf("audience %v: git ran (exit %d, stderr %q)", audience, res.ExitCode, res.Stderr)
		}
		if msg := res.Err.Error(); strings.Contains(msg, parent) || strings.Contains(msg, "ird") {
			t.Errorf("audience %v: refusal names a path: %q", audience, msg)
		}
	}
}

// For the remote audience, the discovery-mode errors name no path and wrap
// no *fs.PathError; for the local audience they keep the cause.
func TestRunHostGit_DiscoveryErrorsRemoteHaveNoPath(t *testing.T) {
	hostGitTestEnv(t)
	base := filepath.Join(t.TempDir(), "pathmarker-r7")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	headLeaf := filepath.Join(base, "headleaf")
	if err := os.MkdirAll(headLeaf, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(headLeaf, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A path under a regular file: checking it for .git fails with an error
	// other than "does not exist" on Unix, and cannot be resolved elsewhere.
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	underFile := filepath.Join(file, "sub")
	for name, dir := range map[string]string{"HEAD leaf": headLeaf, "under a file": underFile} {
		res := RunHostGit(context.Background(), dir, HostGitOptions{Env: ceilingEnv(), Audience: AudienceRemote}, "rev-parse", "--is-inside-work-tree")
		if res.Err == nil {
			t.Fatalf("%s: no error", name)
		}
		if strings.Contains(res.Err.Error(), "pathmarker-r7") {
			t.Errorf("%s: remote error names a path: %q", name, res.Err)
		}
		var pe *fs.PathError
		if errors.As(res.Err, &pe) {
			t.Errorf("%s: remote error wraps a *fs.PathError", name)
		}
	}
	if runtime.GOOS != "windows" {
		res := RunHostGit(context.Background(), underFile, HostGitOptions{Env: ceilingEnv(), Audience: AudienceLocal}, "rev-parse", "--is-inside-work-tree")
		var pe *fs.PathError
		if !errors.As(res.Err, &pe) {
			t.Errorf("local error %v does not keep its cause", res.Err)
		}
	}
}

// A leaf that holds a HEAD entry may be taken by git as a git directory,
// whose config nothing checked: refused. A .git that appears in the leaf
// while git runs is a config change.
func TestRunHostGit_CeilingLeafBecomesRepository(t *testing.T) {
	leaf := ceilingLeaf(t, ceilingFixture(t, false), "sub/leaf")
	if err := os.WriteFile(filepath.Join(leaf, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := RunHostGit(context.Background(), leaf, HostGitOptions{Env: ceilingEnv()}, "rev-parse", "--is-inside-work-tree")
	if res.Err == nil || res.ExitCode != -1 {
		t.Errorf("leaf with HEAD: exit %d, err %v; want a refusal", res.ExitCode, res.Err)
	}
	if err := os.Remove(filepath.Join(leaf, "HEAD")); err != nil {
		t.Fatal(err)
	}
	fakeGitOnPath(t)
	res = RunHostGit(context.Background(), leaf, HostGitOptions{Env: ceilingEnv()}, "fake-mkgit")
	if !errors.Is(res.Err, ErrRepoConfigChanged) {
		t.Errorf("a .git created during the run: Err = %v, want ErrRepoConfigChanged", res.Err)
	}
}

// The door's own listings (readFilterPins, and crossCheckAgainstGit through
// the policy check) run with the door's environment and the HostGitArgs pins
// first, and see no inherited variable that chooses a repository or adds
// config. readFilterPins sees exactly the main command's environment,
// GIT_CEILING_DIRECTORIES included when the door sets it. The global and
// system config values are whatever HostGitEnv passes; an empty-config
// assertion arrives with the change that sets those defaults in HostGitEnv.
func TestHostGitCheckers_DoorListingsUseDoorEnv(t *testing.T) {
	pins := strings.Join(HostGitArgs(), " ")
	const pinMarker = "user.name=pinmarker"
	for _, mode := range []string{"repository", "discovery"} {
		t.Run(mode, func(t *testing.T) {
			var dir string
			if mode == "repository" {
				dir, _ = newHostGitRepo(t)
			} else {
				dir = ceilingLeaf(t, ceilingFixture(t, false), "sub/leaf")
			}
			log := fakeGitOnPath(t)
			plantInheritedGitEnv(t)
			want := gitEnvMap(HostGitEnv(os.Environ()))
			if mode == "discovery" {
				resolved, err := filepath.EvalSymlinks(dir)
				if err != nil {
					t.Fatal(err)
				}
				want["GIT_CEILING_DIRECTORIES"] = filepath.Dir(resolved)
			}
			res := RunHostGit(context.Background(), dir, HostGitOptions{Config: []string{pinMarker}}, "rev-parse", "--is-inside-work-tree")
			if res.Err != nil {
				t.Fatalf("Err = %v", res.Err)
			}
			var main, filter, cross int
			for _, c := range fakeGitCalls(t, log) {
				if !strings.HasPrefix(c.argv, pins+" ") {
					t.Errorf("call %q does not start with the HostGitArgs pins", c.argv)
				}
				for _, k := range plantedGitEnvKeys {
					if _, ok := c.env[k]; ok {
						t.Errorf("call %q received the inherited %s", c.argv, k)
					}
				}
				if d := diffEnvKeys(c.env, want); len(d) > 0 {
					t.Errorf("call %q: environment differs from the door's in %v", c.argv, d)
				}
				switch {
				case strings.HasSuffix(c.argv, " rev-parse --is-inside-work-tree"):
					main++
				case strings.HasSuffix(c.argv, " config --show-scope -z --list") && strings.Contains(c.argv, pinMarker):
					filter++
				case strings.HasSuffix(c.argv, " config --show-scope -z --list"):
					cross++
				}
			}
			if main != 1 || filter != 1 {
				t.Errorf("recorded %d main and %d filter-pin calls, want 1 and 1", main, filter)
			}
			if mode == "repository" && cross < 1 {
				t.Error("no policy cross-check call was recorded")
			}
			if mode == "discovery" && cross != 0 {
				t.Errorf("discovery mode ran %d policy cross-checks, want 0 (nothing to check)", cross)
			}
		})
	}
}
