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
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// agentsMaskMount returns the agent container's mount of the agents mask
// volume, if any.
func agentsMaskMount(pod *corev1.Pod) (corev1.VolumeMount, bool) {
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.Name == k8sAgentsMaskVolume {
			return m, true
		}
	}
	return corev1.VolumeMount{}, false
}

// Local-backend pods keep the in-project agents root out by exclusion
// only: no volume, but the annotation that drives every sync's excludes.
// NFS-shared pods also get the mask volume, but only when the agents dir
// already exists on the broker's view of the export.
func TestBuildPod_SharedWorkspaceAgentsMask(t *testing.T) {
	withAgents := sharedRepoWithScionDir(t)
	scionOnly := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(scionOnly, ".scion"), 0o755); err != nil {
		t.Fatal(err)
	}
	noScion := t.TempDir()
	shared := func(cfg RunConfig, dir string) RunConfig {
		cfg.RepoRoot, cfg.Workspace = dir, dir
		return cfg
	}
	local := RunConfig{Name: "local", Image: "test-image", UnixUsername: "scion"}
	callerSet := shared(local, noScion)
	callerSet.Annotations = map[string]string{k8sWorkspaceAgentsAnnotation: "true", "other": "x"}
	for _, tt := range []struct {
		name             string
		cfg              RunConfig
		excluded, masked bool
	}{
		{"local, agents dir exists", shared(local, withAgents), true, false},
		{"local, .scion only", shared(local, scionOnly), true, false},
		{"local without .scion", shared(local, noScion), false, false},
		{"local, caller-set annotation dropped", callerSet, false, false},
		{"local, repo root does not resolve", RunConfig{Name: "local", Image: "test-image", UnixUsername: "scion", RepoRoot: filepath.Join(noScion, "missing"), Workspace: withAgents}, false, false},
		{"NFS shared, agents dir exists", shared(nfsBaseConfig("nfs"), withAgents), true, true},
		{"NFS shared, no agents dir", shared(nfsBaseConfig("nfs"), scionOnly), true, false},
		{"NFS shared without .scion", shared(nfsBaseConfig("nfs"), noScion), false, false},
		{"NFS worktree", shared(nfsWorktreeConfig("wt"), withAgents), false, false},
		{"NFS agent dir", shared(nfsAgentDirConfig("cpa"), withAgents), false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pod, err := newNFSTestK8sRuntime().buildPod("default", tt.cfg)
			if err != nil {
				t.Fatalf("buildPod: %v", err)
			}
			var maskVolumes []corev1.Volume
			names := map[string]int{}
			for _, v := range pod.Spec.Volumes {
				names[v.Name]++
				if v.Name == k8sAgentsMaskVolume {
					maskVolumes = append(maskVolumes, v)
				}
			}
			for name, n := range names {
				if n != 1 {
					t.Errorf("pod volume name %q used %d times", name, n)
				}
			}
			for _, c := range pod.Spec.InitContainers {
				for _, im := range c.VolumeMounts {
					if im.Name == k8sAgentsMaskVolume {
						t.Errorf("init container %s mounts the agents mask", c.Name)
					}
				}
			}

			wantExcludes := []string(nil)
			if tt.excluded {
				wantExcludes = []string{".scion/agents"}
			}
			for k, v := range tt.cfg.Annotations {
				if k != k8sWorkspaceAgentsAnnotation && pod.Annotations[k] != v {
					t.Errorf("caller annotation %s = %q on the pod, want %q", k, pod.Annotations[k], v)
				}
			}
			if got := k8sWorkspaceSyncExcludes(pod.Annotations); !slices.Equal(got, wantExcludes) {
				t.Errorf("sync excludes = %q, want %q (annotations %v)", got, wantExcludes, pod.Annotations)
			}

			m, mounted := agentsMaskMount(pod)
			if !tt.masked {
				if len(maskVolumes) != 0 || mounted {
					t.Errorf("unexpected agents mask: volumes %+v, mount %+v", maskVolumes, m)
				}
				return
			}
			if len(maskVolumes) != 1 {
				t.Fatalf("want one %q volume, got %+v", k8sAgentsMaskVolume, maskVolumes)
			}
			ed := maskVolumes[0].EmptyDir
			if ed == nil || ed.Medium != corev1.StorageMediumMemory {
				t.Errorf("mask volume = %+v, want a memory emptyDir", maskVolumes[0].VolumeSource)
			}
			if ed != nil && (ed.SizeLimit == nil || ed.SizeLimit.String() != k8sAgentsMaskSizeLimit) {
				t.Errorf("mask size limit = %v, want %s", ed.SizeLimit, k8sAgentsMaskSizeLimit)
			}
			if !mounted || m.MountPath != "/workspace/.scion/agents" || m.SubPath != "" {
				t.Errorf("agent container mask mount = %+v (found %v), want /workspace/.scion/agents", m, mounted)
			}
			if ws := pod.Spec.Containers[0].VolumeMounts[0]; ws.Name != "workspace" || ws.MountPath != "/workspace" {
				t.Errorf("first agent mount = %+v, want the workspace at /workspace", ws)
			}
		})
	}
}

// The start's workspace sync passes the excludes the new pod's annotation
// records, and none for a pod without it.
func TestSyncWorkspaceToPod_Excludes(t *testing.T) {
	for _, excluded := range []bool{true, false} {
		t.Run(map[bool]string{true: "annotated", false: "plain"}[excluded], func(t *testing.T) {
			rt, _, _ := newTestK8sRuntime()
			pod := newFakeAgentPodWithWorkspaceAnnotation("test-agent", "/some/workspace")
			pod.Annotations = withWorkspaceAgentsAnnotation(pod.Annotations, excluded)
			var calls []string
			rt.syncTransfer = func(_ context.Context, _, podName, src, dest string, toPod bool, excludes []string) error {
				calls = append(calls, podName+" "+src+" -> "+dest)
				want := []string(nil)
				if excluded {
					want = []string{".scion/agents"}
				}
				if !toPod || !slices.Equal(excludes, want) {
					t.Errorf("toPod=%v excludes=%q, want toPod and %q", toPod, excludes, want)
				}
				return nil
			}
			if err := rt.syncWorkspaceToPod(context.Background(), "default", pod, "/broker/ws"); err != nil {
				t.Fatalf("syncWorkspaceToPod: %v", err)
			}
			if !slices.Equal(calls, []string{"test-agent /broker/ws -> /workspace"}) {
				t.Errorf("transfers = %q", calls)
			}
		})
	}
}

// Sync in both directions leaves the in-project agents root out of the
// workspace copy when the pod's annotation says so, and only then. It
// reads the pod returned by List and makes no other pod request.
func TestSync_WorkspaceAgentsAnnotationExcludes(t *testing.T) {
	for _, dir := range []SyncDirection{SyncTo, SyncFrom} {
		for _, excluded := range []bool{true, false} {
			t.Run(string(dir)+map[bool]string{true: "/annotated", false: "/plain"}[excluded], func(t *testing.T) {
				rt, cs, _ := newTestK8sRuntime()
				pod := newFakeAgentPodWithHomeDirAnnotation("test-agent", "/some/agent/home")
				pod.Annotations = withWorkspaceAgentsAnnotation(pod.Annotations, excluded)
				if _, err := cs.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
				cs.ClearActions()
				got := map[string][]string{}
				rt.syncTransfer = func(_ context.Context, _, _, src, dest string, _ bool, excludes []string) error {
					got[src+" -> "+dest] = excludes
					return nil
				}
				captureStdout(t, func() {
					if err := rt.Sync(context.Background(), "test-agent", dir); err != nil {
						t.Fatalf("Sync: %v", err)
					}
				})
				if len(got) != 2 {
					t.Fatalf("transfers = %v, want workspace and home", got)
				}
				for k, ex := range got {
					isWorkspace := strings.Contains(k, "/workspace")
					if isWorkspace && excluded && !slices.Equal(ex, []string{".scion/agents"}) {
						t.Errorf("%s excludes = %q, want [.scion/agents]", k, ex)
					}
					if (!isWorkspace || !excluded) && ex != nil {
						t.Errorf("%s excludes = %q, want none", k, ex)
					}
				}
				for _, a := range cs.Actions() {
					if a.GetResource().Resource == "pods" && a.GetVerb() != "list" {
						t.Errorf("Sync made a %s pods request; it should use the listed pod", a.GetVerb())
					}
				}
			})
		}
	}
}

// The tar arguments both sync directions use leave the agents root out of
// the archive (to the pod) and out of the extraction (from the pod), and
// extraction never removes the broker's own copy.
func TestSyncArchiveArgs_ExcludeAgentsDir(t *testing.T) {
	src := t.TempDir()
	for _, f := range []string{"README.md", ".scion/agents/other/prompt.md", ".scion/settings.yaml"} {
		p := filepath.Join(src, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// To the pod: the archive has no agents root entries.
	createArgs := syncArchiveCreateArgs(src, sharedWorkspaceAgentsDir)
	archive := filepath.Join(t.TempDir(), "ws.tgz")
	if out, err := exec.Command("tar", append([]string{"-f", archive}, createArgs...)...).CombinedOutput(); err != nil {
		t.Fatalf("tar create: %v: %s", err, out)
	}
	list, err := exec.Command("tar", "-tzf", archive).CombinedOutput()
	if err != nil {
		t.Fatalf("tar list: %v: %s", err, list)
	}
	if strings.Contains(string(list), ".scion/agents") || !strings.Contains(string(list), "./.scion/settings.yaml") {
		t.Errorf("archive members:\n%s", list)
	}

	// From the pod: a full archive extracts without touching the agents root.
	full := filepath.Join(t.TempDir(), "full.tgz")
	if out, err := exec.Command("tar", "-czf", full, "-C", src, ".").CombinedOutput(); err != nil {
		t.Fatalf("tar create: %v: %s", err, out)
	}
	dest := t.TempDir()
	kept := filepath.Join(dest, ".scion", "agents", "mine", "prompt.md")
	if err := os.MkdirAll(filepath.Dir(kept), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kept, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	extract := append([]string{"-f", full}, syncArchiveExtractArgs(dest, sharedWorkspaceAgentsDir)...)
	if out, err := exec.Command("tar", extract...).CombinedOutput(); err != nil {
		t.Fatalf("tar extract: %v: %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dest, ".scion", "agents", "other")); !os.IsNotExist(err) {
		t.Errorf("extraction wrote into the agents root (stat err %v)", err)
	}
	if b, err := os.ReadFile(kept); err != nil || string(b) != "mine" {
		t.Errorf("broker agents root changed: %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dest, "README.md")); err != nil {
		t.Errorf("workspace file not extracted: %v", err)
	}
}
