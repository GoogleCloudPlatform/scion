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
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The exact neutral end-state message. Hard-coded here (not referencing
// the constant) so a change to the wording is caught by this test.
const wantK8sRefusal = "Kubernetes does not support agents that share one git repository on NFS: this agent is " +
	"worktree-per-agent, or uses the project's shared workspace and the runtime broker cannot confirm " +
	"that the shared workspace is not a git repository. Re-create the agent as clone-per-agent, " +
	"or run it on a local or Docker broker. Work not pushed from the existing agent remains in " +
	"the project's shared checkout on the workspace storage."

func TestK8sSharedRepoShapeRefusal(t *testing.T) {
	// The #3998 sticky shape: a pre-existing labelless agent that nfsKeepSharedCheckout
	// (pkg/agent) keeps on the project's shared checkout. Its resulting RunConfig is
	// a git-backed NFS shared workspace with no worktree/agent-dir — i.e. nfsBaseConfig.
	sticky := nfsBaseConfig("sticky")

	cases := []struct {
		name    string
		cfg     RunConfig
		refused bool
	}{
		{"worktree-per-agent", nfsWorktreeConfig("wt"), true},
		{"shared-workspace git", nfsBaseConfig("shared"), true},
		{"#3998 sticky shared checkout", sticky, true},
		{"clone-per-agent", nfsAgentDirConfig("cpa"), false},
		{"empty-per-agent", nfsEmptyAgentDirConfig("epa"), false},
		{"local emptyDir", minimalRunConfig(), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, refuse := k8sSharedRepoShapeRefusal(tc.cfg)
			if refuse != tc.refused {
				t.Fatalf("refuse = %v, want %v", refuse, tc.refused)
			}
			if refuse && msg != wantK8sRefusal {
				t.Errorf("refusal message mismatch:\n got: %q\nwant: %q", msg, wantK8sRefusal)
			}
		})
	}
}

// Run refuses the shared-repo shape on every path (start, restart, wake,
// reincarnate all go through Run), before any pod is created, with the exact
// message. The Resume flag does not change the decision.
func TestRun_RefusesSharedRepoShape(t *testing.T) {
	for _, resume := range []bool{false, true} {
		for _, cfg := range []RunConfig{nfsWorktreeConfig("wt"), nfsBaseConfig("shared")} {
			cfg.Resume = resume
			r := newNFSTestK8sRuntime()
			// The refusal is at the top of Run, before any pod wait. Bound the
			// call with a short deadline so that if the refusal were ever skipped,
			// the test fails fast instead of hanging on the fake pod until the
			// package-level timeout.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, err := r.Run(ctx, cfg)
			cancel()
			if err == nil {
				t.Fatalf("Run(resume=%v, %s) succeeded; want refusal", resume, cfg.Name)
			}
			if err.Error() != wantK8sRefusal {
				t.Errorf("Run error mismatch:\n got: %q\nwant: %q", err.Error(), wantK8sRefusal)
			}
			pods, lerr := r.Client.Clientset.CoreV1().Pods("default").List(context.Background(), metav1.ListOptions{})
			if lerr != nil {
				t.Fatal(lerr)
			}
			if len(pods.Items) != 0 {
				t.Errorf("a refused start must create no pod, got %d", len(pods.Items))
			}
		}
	}
}

// buildPod itself stays a pure constructor: it does NOT refuse the shared-repo
// shape (the refusal is at Run), so the read-only .git narrowing it builds for
// that shape stays testable and ready for the follow-up that lifts the refusal.
func TestBuildPod_DoesNotRefuseSharedRepoShape(t *testing.T) {
	if _, err := newNFSTestK8sRuntime().buildPod("default", nfsWorktreeConfig("wt")); err != nil {
		t.Fatalf("buildPod should construct the worktree pod (refusal is at Run), got: %v", err)
	}
}

// The read-only admin subPath mounts for the agent container.
func TestK8sGitAdminReadOnlyMounts(t *testing.T) {
	got := k8sGitAdminReadOnlyMounts("projects/p/workspace/.git", "agent-1")
	want := []corev1.VolumeMount{
		{Name: "workspace", MountPath: "/repo-root/.git/hooks", SubPath: "projects/p/workspace/.git/hooks", ReadOnly: true},
		{Name: "workspace", MountPath: "/repo-root/.git/info", SubPath: "projects/p/workspace/.git/info", ReadOnly: true},
		{Name: "workspace", MountPath: "/repo-root/.git/objects/info", SubPath: "projects/p/workspace/.git/objects/info", ReadOnly: true},
		{Name: "workspace", MountPath: "/repo-root/.git/worktrees", SubPath: "projects/p/workspace/.git/worktrees", ReadOnly: true},
		{Name: "workspace", MountPath: "/repo-root/.git/worktrees/agent-1", SubPath: "projects/p/workspace/.git/worktrees/agent-1"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d mounts, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("mount %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	// The own-worktree overlay must be read-write (so the agent can write its own
	// HEAD/index/logs), while worktrees/ above it is read-only.
	own := got[len(got)-1]
	if own.ReadOnly {
		t.Error("the agent's own worktree admin dir must be read-write")
	}
}

// The shape is derived from the re-sent per-agent workspace fields
// (NFSWorktreeName / NFSAgentDirName / NFSAgentDirEmpty, and the
// SCION_WORKSPACE_GIT env hint); the ephemeral GitCloneForInit can only add a
// refusal, never lift one, so the refusal fires on a bare restart
// (GitCloneForInit nil) of a shared-repo agent,
// while clone-per-agent and empty-per-agent are not refused. An NFS shared shape
// with no positive non-git signal is refused fail-closed.
func TestK8sSharedRepoShapeRefusal_Restart(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*RunConfig)
		refused bool
	}{
		{"shared-workspace bare restart", func(c *RunConfig) { c.GitCloneForInit = nil }, true},
		{"worktree-per-agent bare restart", func(c *RunConfig) {
			c.GitCloneForInit = nil
			c.NFSWorktreeName = "agent-1"
		}, true},
		{"clone-per-agent restart", func(c *RunConfig) {
			c.GitCloneForInit = nil
			c.NFSAgentDirName = "clone"
		}, false},
		{"empty-per-agent restart", func(c *RunConfig) {
			c.GitCloneForInit = nil
			c.NFSAgentDirName = "empty"
			c.NFSAgentDirEmpty = true
		}, false},
		{"shared-workspace restart, git via env (no GitClone)", func(c *RunConfig) {
			c.GitCloneForInit = nil
			c.Env = append(c.Env, "SCION_WORKSPACE_GIT=true")
		}, true},
		{"shared-workspace restart, positively non-git via env", func(c *RunConfig) {
			c.GitCloneForInit = nil
			c.Env = append(c.Env, "SCION_WORKSPACE_GIT=false")
		}, false},
		{"git clone input wins over a non-git env signal", func(c *RunConfig) {
			// GitCloneForInit present is a positive git signal and is checked first,
			// so a conflicting SCION_WORKSPACE_GIT=false does not allow the start.
			c.Env = append(c.Env, "SCION_WORKSPACE_GIT=false")
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := nfsBaseConfig("restart")
			tc.mutate(&cfg)
			_, refuse := k8sSharedRepoShapeRefusal(cfg)
			if refuse != tc.refused {
				t.Errorf("refuse = %v, want %v", refuse, tc.refused)
			}
		})
	}
}
