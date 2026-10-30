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
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func gitOutput(t *testing.T, repo string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %v in %s: %v", args, repo, err)
	}
	return string(out)
}

// TestDeleteAgentFiles_ChangesOnlyTheProjectRepository: deleting an agent
// whose worktree's .git file was changed to name another repository's
// branch deletes the agent's branch in the project's repository and changes
// nothing in the other repository.
func TestDeleteAgentFiles_ChangesOnlyTheProjectRepository(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	t.Setenv("HOME", t.TempDir())

	root := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	setupGitRepo(t, root)
	projectDir := filepath.Join(root, ".scion")
	agentDir := filepath.Join(projectDir, "agents", "worker")
	if err := os.MkdirAll(filepath.Join(agentDir, "home"), 0o755); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(agentDir, "workspace")
	if out, err := exec.Command("git", "-C", root, "worktree", "add", "-b", "worker", workspace).CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}

	other := filepath.Join(t.TempDir(), "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	setupGitRepo(t, other)
	for _, b := range []string{"worker", "unrelated"} {
		if out, err := exec.Command("git", "-C", other, "branch", b).CombinedOutput(); err != nil {
			t.Fatalf("git branch %s: %v\n%s", b, err, out)
		}
	}
	otherBefore := gitOutput(t, other, "for-each-ref") + gitOutput(t, other, "worktree", "list", "--porcelain")

	// The worktree's .git file names a made-up admin directory whose HEAD is
	// the other repository's "unrelated" branch (checked out nowhere) and
	// whose commondir is the other repository: git run in the worktree
	// would report that repository and that branch.
	fake := filepath.Join(t.TempDir(), "admin")
	if err := os.MkdirAll(fake, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"HEAD":      "ref: refs/heads/unrelated\n",
		"commondir": filepath.Join(other, ".git") + "\n",
		"gitdir":    filepath.Join(workspace, ".git") + "\n",
	} {
		if err := os.WriteFile(filepath.Join(fake, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(workspace, ".git"), []byte("gitdir: "+fake+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := DeleteAgentFiles("worker", projectDir, true); err != nil {
		t.Fatalf("DeleteAgentFiles: %v", err)
	}
	if after := gitOutput(t, other, "for-each-ref") + gitOutput(t, other, "worktree", "list", "--porcelain"); after != otherBefore {
		t.Errorf("the other repository changed:\nbefore:\n%s\nafter:\n%s", otherBefore, after)
	}
	if err := exec.Command("git", "-C", root, "rev-parse", "--verify", "--quiet", "refs/heads/worker").Run(); err == nil {
		t.Error("the agent's branch in the project repository remains")
	}
	if _, err := os.Stat(agentDir); !os.IsNotExist(err) {
		t.Errorf("agent directory remains: %v", err)
	}
}
