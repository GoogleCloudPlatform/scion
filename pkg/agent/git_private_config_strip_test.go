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

//go:build unix

package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// stripTestRepo makes a repository with one commit and a linked worktree.
func stripTestRepo(t *testing.T) (repo, ws string, git func(dir string, args ...string) error) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	repo = filepath.Join(root, "repo")
	git = func(dir string, args ...string) error {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "SCION_HOST_UID=",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return errors.New(string(out))
		}
		return nil
	}
	must := func(dir string, args ...string) {
		t.Helper()
		if err := git(dir, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	must(root, "init", "-q", "-b", "main", repo)
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	must(repo, "add", "f")
	must(repo, "commit", "-q", "-m", "init")
	ws = filepath.Join(root, "ws")
	must(repo, "worktree", "add", "-q", "-b", "agent-x", ws)
	return repo, ws, git
}

// With stripping, never-carry keys whose values would each run a command
// (touching a sentinel) are planted in .git/config. The seed (layer check,
// git cross-check, submodule check and the copy) runs no planted command, the
// agent's copy lacks every planted key, and the start notice names them.
// The controls afterwards show the plants are live under plain git.
func TestSeedCheckedPrivateGitConfigStrip_RunsNoPlantedCommand(t *testing.T) {
	repo, ws, git := stripTestRepo(t)
	sent := t.TempDir()
	s := func(n string) string { return filepath.Join(sent, n) }
	hooks := t.TempDir()
	for _, h := range []string{"pre-commit", "post-checkout", "post-index-change", "reference-transaction", "pre-auto-gc", "post-merge"} {
		if err := os.WriteFile(filepath.Join(hooks, h), []byte("#!/bin/sh\n: > '"+s("hook-"+h)+"'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	plant := "[core]\n" +
		"\tfsmonitor = \": > '" + s("fsmonitor") + "'\"\n" +
		"\thooksPath = " + hooks + "\n" +
		"\tpager = \": > '" + s("pager") + "'; cat\"\n" +
		"[pager]\n" +
		"\tconfig = \": > '" + s("pager-config") + "'; cat\"\n" +
		"\tls-files = \": > '" + s("pager-ls-files") + "'; cat\"\n" +
		"[alias]\n" +
		"\tsnt = \"!: > '" + s("alias") + "'\"\n"
	f, err := os.OpenFile(filepath.Join(repo, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(plant); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	agentDir := t.TempDir()
	dest, never, err := seedCheckedPrivateGitConfigStrip(context.Background(), repo, ws, agentDir, "agent-x", true)
	if err != nil {
		t.Fatalf("seed with stripping refused: %v", err)
	}
	entries, err := os.ReadDir(sent)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("the seed ran a planted command: sentinel %s exists", e.Name())
	}
	copyBytes, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	copyText := strings.ToLower(string(copyBytes))
	for _, k := range []string{"fsmonitor", "hookspath", "pager", "[alias", "snt", sent} {
		if strings.Contains(copyText, strings.ToLower(k)) {
			t.Errorf("the agent's copy still carries %q:\n%s", k, copyBytes)
		}
	}
	want := []string{"core.fsmonitor", "core.hookspath", "core.pager", "pager.config", "pager.ls-files", "alias.snt"}
	if strings.Join(never, ",") != strings.Join(want, ",") {
		t.Errorf("left out %v, want %v", never, want)
	}
	notice := util.NeverCarriedNotice(never)
	for _, k := range want {
		if !strings.Contains(notice, k) {
			t.Errorf("notice does not name %s: %q", k, notice)
		}
	}
	if strings.Contains(notice, sent) || strings.Contains(notice, hooks) {
		t.Errorf("notice carries a value: %q", notice)
	}

	// Without stripping (a runtime that does not bind the copy) the keys are
	// refused, and still nothing runs.
	_, _, err = seedCheckedPrivateGitConfigStrip(context.Background(), repo, ws, t.TempDir(), "agent-x", false)
	var refused *util.RepoConfigRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("without stripping: %v, want a refusal", err)
	}
	if entries, _ := os.ReadDir(sent); len(entries) != 0 {
		t.Errorf("the refusing seed ran a planted command (%d sentinels)", len(entries))
	}

	// Controls: the plants are live under plain git.
	_ = git(repo, "status")
	_ = git(repo, "snt")
	_ = git(repo, "commit", "-q", "--allow-empty", "-m", "c")
	for _, n := range []string{"fsmonitor", "alias", "hook-pre-commit"} {
		if _, err := os.Stat(s(n)); err != nil {
			t.Errorf("control: plain git did not create sentinel %s, so its plant proves nothing", n)
		}
	}
}

// Keys are left out of the copy only on the runtimes that bind it.
func TestBindsPrivateGitConfig(t *testing.T) {
	for name, want := range map[string]bool{
		"docker": true, "podman": true,
		"container": false, "apple-container": false, "kubernetes": false, "cloudrun": false, "": false,
	} {
		if got := bindsPrivateGitConfig(name); got != want {
			t.Errorf("bindsPrivateGitConfig(%q) = %v, want %v", name, got, want)
		}
	}
}

// As above for the filter, diff, ssh and proxy commands: the seed's host git runs on the REAL .git/config while
// these never-carry keys are still in it. With .gitattributes routing every path through the planted filter and
// diff driver, and a tracked file left stat-dirty, the seed runs none of the planted commands, the copy lacks every
// planted key, and the notice names them. The control shows plain git status and git add do run the filter.
func TestSeedCheckedPrivateGitConfigStrip_RunsNoFilterOrDiffCommand(t *testing.T) {
	repo, ws, git := stripTestRepo(t)
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("* filter=snt diff=snt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := git(repo, "add", ".gitattributes"); err != nil {
		t.Fatal(err)
	}
	if err := git(repo, "commit", "-q", "-m", "attributes"); err != nil {
		t.Fatal(err)
	}
	sent := t.TempDir()
	s := func(n string) string { return filepath.Join(sent, n) }
	plant := "[filter \"snt\"]\n" +
		"\tclean = \": > '" + s("clean") + "'; cat\"\n" +
		"\tsmudge = \": > '" + s("smudge") + "'; cat\"\n" +
		"\tprocess = \": > '" + s("process") + "'\"\n" +
		"[diff \"snt\"]\n" +
		"\ttextconv = \": > '" + s("textconv") + "'; cat\"\n" +
		"[diff]\n" +
		"\texternal = \": > '" + s("external") + "'\"\n" +
		"[core]\n" +
		"\tsshCommand = \": > '" + s("ssh") + "'\"\n" +
		"\tgitProxy = \": > '" + s("gitproxy") + "'\"\n"
	f, err := os.OpenFile(filepath.Join(repo, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(plant); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	// A tracked file whose stat no longer matches the index but whose size does (same length, new content and a
	// new mtime), so a status or add must read it through the clean filter to compare it.
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(repo, "f"), later, later); err != nil {
		t.Fatal(err)
	}

	dest, never, err := seedCheckedPrivateGitConfigStrip(context.Background(), repo, ws, t.TempDir(), "agent-x", true)
	// Sentinels first: a seed whose git call ran a planted command must fail here, even if it then errors.
	if entries, _ := os.ReadDir(sent); len(entries) != 0 {
		for _, e := range entries {
			t.Errorf("the seed ran a planted command: sentinel %s exists", e.Name())
		}
	}
	if err != nil {
		t.Fatalf("seed with stripping refused: %v", err)
	}
	copyBytes, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	copyText := strings.ToLower(string(copyBytes))
	for _, k := range []string{"snt", "external", "sshcommand", "gitproxy", sent} {
		if strings.Contains(copyText, strings.ToLower(k)) {
			t.Errorf("the agent's copy still carries %q:\n%s", k, copyBytes)
		}
	}
	want := []string{"filter.snt.clean", "filter.snt.smudge", "filter.snt.process", "diff.snt.textconv", "diff.external", "core.sshcommand", "core.gitproxy"}
	if strings.Join(never, ",") != strings.Join(want, ",") {
		t.Errorf("left out %v, want %v", never, want)
	}
	notice := util.NeverCarriedNotice(never)
	for _, k := range want {
		if !strings.Contains(notice, k) {
			t.Errorf("notice does not name %s: %q", k, notice)
		}
	}

	// Control: the filter plant is live under plain git in the work tree.
	_ = git(repo, "status")
	_ = git(repo, "add", "f")
	_, cerr := os.Stat(s("clean"))
	_, perr := os.Stat(s("process"))
	if cerr != nil && perr != nil {
		t.Error("control: plain git status/add ran neither the clean nor the process filter, so the plant proves nothing")
	}
}
