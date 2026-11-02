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
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// sharedCloneFakeGit puts a fake git first on PATH. Every invocation's argv
// is appended to the returned argv log, one argument per line, followed by
// an "--END--" line; the clone environment's GIT_ALLOW_PROTOCOL is appended
// to the env log. A clone becomes a local init of its last argument (or,
// with failFirstBranchClone, a first clone that carries --branch fails as a
// missing remote branch); anything else runs the real git.
func sharedCloneFakeGit(t *testing.T, failFirstBranchClone bool) (argvLog, envLog string) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("git is required for this test: %v", err)
	}
	root := t.TempDir()
	argvLog = filepath.Join(root, "argv.log")
	envLog = filepath.Join(root, "env.log")
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	failBranch := "0"
	if failFirstBranchClone {
		failBranch = "1"
	}
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do printf '%s\\n' \"$a\" >> '" + argvLog + "'; done\n" +
		"printf '%s\\n' '--END--' >> '" + argvLog + "'\n" +
		"last=''; isclone=0; hasbranch=0\n" +
		"for a in \"$@\"; do last=\"$a\"; [ \"$a\" = clone ] && isclone=1; [ \"$a\" = --branch ] && hasbranch=1; done\n" +
		"if [ \"$isclone\" = 1 ]; then\n" +
		"  printf 'GIT_ALLOW_PROTOCOL=%s\\n' \"${GIT_ALLOW_PROTOCOL-<unset>}\" >> '" + envLog + "'\n" +
		"  if [ " + failBranch + " = 1 ] && [ \"$hasbranch\" = 1 ]; then echo \"warning: Could not find remote branch x to clone.\" >&2; echo \"fatal: Remote branch x not found in upstream origin\" >&2; exit 128; fi\n" +
		"  exec '" + realGit + "' init -q \"$last\"\n" +
		"fi\n" +
		"exec '" + realGit + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argvLog, envLog
}

// cloneInvocations returns the argv of every logged invocation that runs
// the clone subcommand.
func cloneInvocations(t *testing.T, argvLog string) [][]string {
	t.Helper()
	data, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	var out [][]string
	var cur []string
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "--END--" {
			if slices.Contains(cur, "clone") {
				out = append(out, cur)
			}
			cur = nil
			continue
		}
		cur = append(cur, line)
	}
	return out
}

// lastIndexOf returns the index of the last argv element equal to v, or -1.
func lastIndexOf(argv []string, v string) int {
	for i := len(argv) - 1; i >= 0; i-- {
		if argv[i] == v {
			return i
		}
	}
	return -1
}

// The clone URL follows "--", so a URL starting with "-" is never read as an
// option, on the first clone and on the default-branch retry.
func TestCloneSharedWorkspace_URLFollowsEndOfOptions(t *testing.T) {
	argvLog, _ := sharedCloneFakeGit(t, true)
	dest := filepath.Join(t.TempDir(), "workspace")
	const url = "-uoption-shaped"
	if err := CloneSharedWorkspace(dest, url, "x", ""); err != nil {
		t.Fatalf("CloneSharedWorkspace: %v", err)
	}
	clones := cloneInvocations(t, argvLog)
	if len(clones) != 2 {
		t.Fatalf("want the branch clone and the default-branch retry, got %d clone invocations: %q", len(clones), clones)
	}
	for i, argv := range clones {
		n := len(argv)
		if n < 3 || argv[n-3] != "--" || argv[n-2] != url || argv[n-1] != dest {
			t.Errorf("clone %d: argv must end with -- <url> <dest>, got %q", i, argv)
		}
	}
}

// CloneSharedWorkspaceHardened puts the caller's -c settings ahead of every
// clone subcommand (after the base host pins, so they override them) and
// adds the caller's environment, on the first clone and on the retry.
func TestCloneSharedWorkspaceHardened_ArgsAndEnvOnEveryClone(t *testing.T) {
	argvLog, envLog := sharedCloneFakeGit(t, true)
	dest := filepath.Join(t.TempDir(), "workspace")
	const url = "https://git.example.invalid/org/repo.git"
	err := CloneSharedWorkspaceHardened(dest, url, "x", "", HostGitHardening{
		Config: []string{"protocol.file.allow=never"}, Env: []string{"GIT_ALLOW_PROTOCOL=https:ssh:git"}})
	if err != nil {
		t.Fatalf("CloneSharedWorkspaceHardened: %v", err)
	}
	clones := cloneInvocations(t, argvLog)
	if len(clones) != 2 {
		t.Fatalf("want the branch clone and the default-branch retry, got %d clone invocations: %q", len(clones), clones)
	}
	for i, argv := range clones {
		never := lastIndexOf(argv, "protocol.file.allow=never")
		user := lastIndexOf(argv, "protocol.file.allow=user")
		clone := lastIndexOf(argv, "clone")
		if never < 0 || never > clone || user > never {
			t.Errorf("clone %d: protocol.file.allow=never must be the last protocol.file.allow setting ahead of clone, got %q", i, argv)
		}
		n := len(argv)
		if argv[n-3] != "--" || argv[n-2] != url {
			t.Errorf("clone %d: argv must end with -- <url> <dest>, got %q", i, argv)
		}
	}
	env, err := os.ReadFile(envLog)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(env), "GIT_ALLOW_PROTOCOL=https:ssh:git\n"); got != 2 {
		t.Errorf("every clone must carry the caller's GIT_ALLOW_PROTOCOL, got:\n%s", env)
	}
}

// With protocol.file.allow=never and a GIT_ALLOW_PROTOCOL that leaves out
// file, a real git refuses a local-path source, and nothing is left at the
// destination.
func TestCloneSharedWorkspaceHardened_RealGitRefusesLocalSource(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("git is required for this test: %v", err)
	}
	source := setupGitRepo(t)
	dest := filepath.Join(t.TempDir(), "workspace")
	err := CloneSharedWorkspaceHardened(dest, source, "", "", HostGitHardening{
		Config: []string{"protocol.file.allow=never"}, Env: []string{"GIT_ALLOW_PROTOCOL=https:ssh:git"}})
	if err == nil {
		t.Fatal("a hardened clone of a local path must fail")
	}
	if IsGitRepoDir(dest) {
		t.Error("a refused clone must not leave a repository at the destination")
	}
	// The same source clones without the hardening, so the refusal above
	// comes from the hardening.
	plain := filepath.Join(t.TempDir(), "workspace")
	if err := CloneSharedWorkspace(plain, source, "", ""); err != nil {
		t.Fatalf("CloneSharedWorkspace of the same source: %v", err)
	}
}
