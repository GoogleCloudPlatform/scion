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

package commands

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
)

// legacyAgentGitconfig is an agent .gitconfig as earlier versions left it:
// an unscoped helper, a helper of ours scoped to a host the hub no longer
// offers the token to, and a helper the agent configured itself.
var legacyAgentGitconfig = [][2]string{
	{"credential.helper", provision.GitTokenCredentialHelper},
	{"credential.https://attacker.example.helper", "!sciontool credential-helper"},
	{"credential.https://own.example.helper", "!agent-own-helper"},
}

// writeGitconfig writes entries (key, value) to path through git config, so
// values with quotes or semicolons are stored exactly.
func writeGitconfig(t *testing.T, path string, entries [][2]string) {
	t.Helper()
	for _, e := range entries {
		if out, err := exec.Command("git", "config", "--file", path, "--add", e[0], e[1]).CombinedOutput(); err != nil {
			t.Fatalf("git config --add %s: %v: %s", e[0], err, out)
		}
	}
	if got := gitConfigGet(t, path, entries[0][0]); got != entries[0][1] {
		t.Fatalf("fixture check: %s = %q, want %q", entries[0][0], got, entries[0][1])
	}
}

// restartPopulatedWorkspace runs gitCloneWorkspace the way a restart does:
// the workspace already holds a checkout, so no clone runs. It returns the
// agent .gitconfig path and the error.
func restartPopulatedWorkspace(t *testing.T, gitconfig [][2]string, cloneURL, tokenRemote string) (string, error) {
	t.Helper()
	agentHome := t.TempDir()
	gitconfigPath := filepath.Join(agentHome, ".gitconfig")
	writeGitconfig(t, gitconfigPath, gitconfig)
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(filepath.Join(workspace, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", agentHome)
	t.Setenv("SCION_GIT_CLONE_URL", cloneURL)
	t.Setenv("SCION_WORKSPACE_PATH", workspace)
	t.Setenv("SCION_GITHUB_APP_ENABLED", "")
	t.Setenv("GITHUB_TOKEN", tokenRemoteTestToken)
	t.Setenv(api.EnvGitTokenRemote, tokenRemote)
	err := gitCloneWorkspace(os.Getuid(), os.Getgid(), agentHome, false)
	if _, statErr := os.Stat(filepath.Join(workspace, "README.md")); statErr != nil {
		t.Fatalf("fixture check: the populated workspace was touched: %v", statErr)
	}
	return gitconfigPath, err
}

// On a restart of a populated clone-per-agent workspace, the agent's
// .gitconfig is brought to the current binding: the legacy unscoped helper
// and our helper scoped to another host are removed, the agent's own helper
// is kept, and the helper for the recorded remote's host is written only
// when the clone URL names that remote.
func TestGitCloneWorkspace_RestartReconcilesLegacyHelper(t *testing.T) {
	for _, tc := range []struct {
		name, cloneURL, tokenRemote string
		wantScoped                  bool
	}{
		{"recorded remote", "https://github.com/org/repo.git", "github.com/org/repo", true},
		{"token remote withheld by the hub", "https://attacker.example/org/repo.git", "", false},
		{"clone URL names another host", "https://attacker.example/org/repo.git", "github.com/org/repo", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gitconfig, err := restartPopulatedWorkspace(t, legacyAgentGitconfig, tc.cloneURL, tc.tokenRemote)
			if err != nil {
				t.Fatalf("gitCloneWorkspace on a populated workspace: %v", err)
			}
			if got := gitConfigGet(t, gitconfig, "credential.helper"); got != "" {
				t.Errorf("the legacy unscoped credential.helper survived the restart: %q", got)
			}
			if got := gitConfigGet(t, gitconfig, "credential.https://attacker.example.helper"); got != "" {
				t.Errorf("our helper scoped to another host survived the restart: %q", got)
			}
			if got := gitConfigGet(t, gitconfig, "credential.https://own.example.helper"); got != "!agent-own-helper" {
				t.Errorf("the agent's own helper = %q, want it kept", got)
			}
			got := gitConfigGet(t, gitconfig, "credential.https://github.com.helper")
			if tc.wantScoped && got != provision.GitTokenCredentialHelper {
				t.Errorf("helper for the recorded remote's host = %q, want the token helper", got)
			}
			if !tc.wantScoped && got != "" {
				t.Errorf("a helper was written although the token is not offered: %q", got)
			}
		})
	}
}

// End to end: after the restart, git run with only the agent .gitconfig
// offers the token to no host when the hub withheld the token remote.
func TestGitCloneWorkspace_RestartLegacyHelperOffersNoToken(t *testing.T) {
	root := t.TempDir()
	bare := tokenRemoteBareRepo(t, root)
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("git is required: %v", err)
	}
	other := newTokenGitServer(t, bare, true)
	gitconfig, err := restartPopulatedWorkspace(t, [][2]string{
		{"credential.helper", provision.GitTokenCredentialHelper},
		{"credential.https://" + other.host + ".helper", provision.GitTokenCredentialHelper},
	}, other.url, "")
	if err != nil {
		t.Fatalf("gitCloneWorkspace on a populated workspace: %v", err)
	}
	ws := filepath.Join(t.TempDir(), "ws")
	run := func(args ...string) error {
		c := exec.Command(gitPath, args...)
		c.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+gitconfig, "GIT_TERMINAL_PROMPT=0", "GITHUB_TOKEN="+tokenRemoteTestToken)
		return c.Run()
	}
	if err := run("init", "-q", ws); err != nil {
		t.Fatal(err)
	}
	if err := run("-C", ws, "remote", "add", "origin", other.url); err != nil {
		t.Fatal(err)
	}
	_ = run("-C", ws, "fetch", "-q", "origin", "main")
	if other.requests.Load() == 0 {
		t.Fatal("fixture check: git never reached the server")
	}
	if other.sawToken.Load() {
		t.Error("a helper left from before the restart offered the token")
	}
}

// The restart reconcile fails closed: when the agent .gitconfig cannot be
// changed, gitCloneWorkspace returns an error instead of starting with the
// legacy helper in place.
func TestGitCloneWorkspace_RestartReconcileFailsClosed(t *testing.T) {
	agentHome := t.TempDir()
	lock := filepath.Join(agentHome, ".gitconfig.lock")
	// An existing lock file makes every git config write fail.
	gitconfigPath := filepath.Join(agentHome, ".gitconfig")
	writeGitconfig(t, gitconfigPath, legacyAgentGitconfig)
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(api.EnvGitTokenRemote, "github.com/org/repo")
	err := reconcileCloneCredentialHelper(gitconfigPath, func(*exec.Cmd) {}, true)
	if err == nil {
		t.Fatal("reconcileCloneCredentialHelper succeeded although the .gitconfig is locked")
	}
	if !strings.Contains(err.Error(), "unscoped") {
		t.Errorf("error = %v, want the unscoped-helper removal failure", err)
	}
}

// The shared-workspace helper setup fails closed when the unscoped helper
// cannot be removed: it returns an error and writes no scoped helper, so the
// legacy helper is never left next to a new one.
func TestConfigureSharedWorkspaceGit_UnsetFailureFailsClosed(t *testing.T) {
	agentHome := t.TempDir()
	gitconfigPath := filepath.Join(agentHome, ".gitconfig")
	if err := os.WriteFile(gitconfigPath, []byte("[credential]\n\thelper = !old-helper\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gitconfigPath+".lock", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(api.EnvGitTokenRemote, "github.com/org/shared")
	err := configureSharedWorkspaceGit(agentHome, 0, 0, false)
	if err == nil {
		t.Fatal("configureSharedWorkspaceGit succeeded although the unscoped helper could not be removed")
	}
	if !strings.Contains(err.Error(), "unscoped") {
		t.Errorf("error = %v, want the unscoped-helper removal failure", err)
	}
	if err := os.Remove(gitconfigPath + ".lock"); err != nil {
		t.Fatal(err)
	}
	if got := gitConfigGet(t, gitconfigPath, "credential.https://github.com.helper"); got != "" {
		t.Errorf("a scoped helper was written after the failed removal: %q", got)
	}
}
