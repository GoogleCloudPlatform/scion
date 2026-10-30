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
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
	markerOnly := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(markerOnly, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(markerOnly, ".scion"), []byte("project-id: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
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
		// configReadOnly: the project configuration directory is mounted
		// read-only over the NFS-shared workspace.
		configReadOnly bool
	}{
		{"local, agents dir exists", shared(local, withAgents), true, false, false},
		{"local, .scion only", shared(local, scionOnly), true, false, false},
		{"local without .scion", shared(local, noScion), false, false, false},
		{"local, caller-set annotation dropped", callerSet, false, false, false},
		{"local, repo root does not resolve", RunConfig{Name: "local", Image: "test-image", UnixUsername: "scion", RepoRoot: filepath.Join(noScion, "missing"), Workspace: withAgents}, false, false, false},
		{"NFS shared, agents dir exists", shared(nfsBaseConfig("nfs"), withAgents), true, true, true},
		{"NFS shared, no agents dir", shared(nfsBaseConfig("nfs"), scionOnly), true, false, true},
		{"NFS shared without .scion", shared(nfsBaseConfig("nfs"), noScion), false, false, false},
		{"NFS shared, .scion marker file", shared(nfsBaseConfig("nfs"), markerOnly), false, false, true},
		// The pod mounts the export subPath (the broker's config.Workspace)
		// at /workspace; a workspace below the repo root (a subdirectory
		// project) gets its own .scion mounted at /workspace/.scion from
		// <subPath>/.scion.
		{"NFS shared, workspace below the repo root", func() RunConfig {
			c := nfsBaseConfig("nfs")
			c.RepoRoot, c.Workspace = filepath.Dir(scionOnly), scionOnly
			return c
		}(), false, false, true},
		{"NFS shared, agent's own workspace", func() RunConfig {
			c := shared(nfsBaseConfig("nfs"), scionOnly)
			c.AgentOwnWorkspace = true
			return c
		}(), true, false, false},
		{"NFS shared, repo root reported elsewhere", func() RunConfig {
			c := shared(nfsBaseConfig("nfs"), scionOnly)
			c.RepoRoot = noScion
			return c
		}(), false, false, true},
		{"local, .scion marker file", shared(local, markerOnly), false, false, false},
		{"NFS worktree", shared(nfsWorktreeConfig("wt"), withAgents), false, false, false},
		{"NFS agent dir", shared(nfsAgentDirConfig("cpa"), withAgents), false, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pod, err := newNFSTestK8sRuntime().buildPod("default", tt.cfg)
			if err != nil {
				t.Fatalf("buildPod: %v", err)
			}
			checkK8sProjectConfigMount(t, pod, tt.cfg, tt.configReadOnly)
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
			pod.Annotations = withScionAnnotation(pod.Annotations, k8sWorkspaceAgentsAnnotation, "true", excluded)
			var calls []string
			rt.syncTransfer = func(_ context.Context, _, podName, src, dest string, excludes []string) error {
				calls = append(calls, podName+" "+src+" -> "+dest)
				want := []string(nil)
				if excluded {
					want = []string{".scion/agents"}
				}
				if !slices.Equal(excludes, want) {
					t.Errorf("excludes = %q, want %q", excludes, want)
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

// Sync to the pod leaves the in-project agents root out of the workspace
// copy when the pod's annotation says so, and only then. It reads the pod
// returned by List and makes no other pod request.
func TestSync_WorkspaceAgentsAnnotationExcludes(t *testing.T) {
	for _, excluded := range []bool{true, false} {
		t.Run(map[bool]string{true: "annotated", false: "plain"}[excluded], func(t *testing.T) {
			rt, cs, _ := newTestK8sRuntime()
			pod := newFakeAgentPodWithHomeDirAnnotation("test-agent", "/some/agent/home")
			pod.Annotations = withScionAnnotation(pod.Annotations, k8sWorkspaceAgentsAnnotation, "true", excluded)
			if _, err := cs.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			cs.ClearActions()
			got := map[string][]string{}
			rt.syncTransfer = func(_ context.Context, _, _, src, dest string, excludes []string) error {
				got[src+" -> "+dest] = excludes
				return nil
			}
			captureStdout(t, func() {
				if err := rt.Sync(context.Background(), "test-agent", SyncTo); err != nil {
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

// The tar arguments a sync to the pod uses leave the agents root out of the
// archive.
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
}

// checkK8sProjectConfigMount checks the agent container's mounts: with
// want, exactly one read-only mount of the workspace volume's .scion
// subpath at /workspace/.scion, after the workspace mount and before any
// agents mask; without it, none. In every case the workspace, home and
// agents mask mounts stay writable.
func checkK8sProjectConfigMount(t *testing.T, pod *corev1.Pod, cfg RunConfig, want bool) {
	t.Helper()
	mounts := pod.Spec.Containers[0].VolumeMounts
	configAt, maskAt, workspaceAt := -1, -1, -1
	for i, m := range mounts {
		switch {
		case m.MountPath == "/workspace/.scion":
			if configAt >= 0 {
				t.Errorf("more than one mount at /workspace/.scion: %+v", mounts)
			}
			configAt = i
			wantSub := filepath.Join(cfg.NFSSubPath, ".scion")
			if !m.ReadOnly || m.Name != "workspace" || m.SubPath != wantSub {
				t.Errorf("project config mount = %+v, want read-only workspace subPath %q", m, wantSub)
			}
		case m.Name == k8sAgentsMaskVolume:
			maskAt = i
		case m.MountPath == "/workspace":
			workspaceAt = i
		}
		if m.ReadOnly && (m.MountPath == "/workspace" || m.MountPath == "/workspace/.scion/agents" || strings.HasPrefix(m.MountPath, "/home/")) {
			t.Errorf("agent mount %+v is read-only; the agent's workspace, home and agents mask stay writable", m)
		}
	}
	if !want {
		if configAt >= 0 {
			t.Errorf("unexpected project config mount %+v", mounts[configAt])
		}
		return
	}
	if configAt < 0 {
		t.Fatalf("no read-only project config mount in %+v", mounts)
	}
	if workspaceAt < 0 || configAt < workspaceAt {
		t.Errorf("project config mount at %d must follow the workspace mount at %d", configAt, workspaceAt)
	}
	if maskAt >= 0 && maskAt < configAt {
		t.Errorf("agents mask at %d must follow the project config mount at %d", maskAt, configAt)
	}
}

// An NFS-shared workspace whose .scion is a symbolic link refuses the pod:
// the link itself would stay writable.
func TestBuildPod_ProjectConfigSymlinkRefused(t *testing.T) {
	root := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(root, "cfg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("cfg", filepath.Join(root, ".scion")); err != nil {
		t.Fatal(err)
	}
	cfg := nfsBaseConfig("nfs")
	cfg.RepoRoot, cfg.Workspace = root, root
	if _, err := newNFSTestK8sRuntime().buildPod("default", cfg); !errors.Is(err, errProjectConfigSymlink) {
		t.Fatalf("buildPod error = %v, want errProjectConfigSymlink", err)
	}
}

// An NFS-shared workspace the broker cannot see refuses the pod instead of
// leaving out the read-only .scion mount.
func TestBuildPod_ProjectConfigUncheckableWorkspaceRefused(t *testing.T) {
	cfg := nfsBaseConfig("nfs")
	missing := filepath.Join(t.TempDir(), "not-mounted")
	cfg.RepoRoot, cfg.Workspace = missing, missing
	_, err := newNFSTestK8sRuntime().buildPod("default", cfg)
	if err == nil || !strings.Contains(err.Error(), "cannot check the shared workspace") {
		t.Fatalf("buildPod error = %v, want a refusal", err)
	}
}

// A shared workspace that is a symlink to an unreachable target is refused
// too: the broker cannot see the share through it.
func TestBuildPod_ProjectConfigDanglingWorkspaceLinkRefused(t *testing.T) {
	link := filepath.Join(t.TempDir(), "ws")
	if err := os.Symlink(filepath.Join(t.TempDir(), "gone"), link); err != nil {
		t.Fatal(err)
	}
	cfg := nfsBaseConfig("nfs")
	cfg.RepoRoot, cfg.Workspace = link, link
	_, err := newNFSTestK8sRuntime().buildPod("default", cfg)
	if err == nil || !strings.Contains(err.Error(), "cannot check the shared workspace") {
		t.Fatalf("buildPod error = %v, want a refusal", err)
	}
}

// withScionAnnotation sets or removes one key and leaves the rest as they
// are. It never modifies its input, and returns the input itself when the
// key is to be removed and is absent.
func TestWithScionAnnotation(t *testing.T) {
	for _, tt := range []struct {
		name string
		in   map[string]string
		set  bool
		want map[string]string
	}{
		{"set on nil", nil, true, map[string]string{"k": "v"}},
		{"set beside others", map[string]string{"other": "x"}, true, map[string]string{"k": "v", "other": "x"}},
		{"set replaces a caller value", map[string]string{"k": "caller", "other": "x"}, true, map[string]string{"k": "v", "other": "x"}},
		{"unset removes a caller value", map[string]string{"k": "caller", "other": "x"}, false, map[string]string{"other": "x"}},
		{"unset when absent", map[string]string{"other": "x"}, false, map[string]string{"other": "x"}},
		{"unset on nil", nil, false, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := maps.Clone(tt.in)
			got := withScionAnnotation(tt.in, "k", "v", tt.set)
			if !maps.Equal(got, tt.want) || (got == nil) != (tt.want == nil) {
				t.Errorf("withScionAnnotation = %v, want %v", got, tt.want)
			}
			if !maps.Equal(tt.in, before) {
				t.Errorf("input changed to %v, was %v", tt.in, before)
			}
			_, had := tt.in["k"]
			if same := reflect.ValueOf(got).UnsafePointer() == reflect.ValueOf(tt.in).UnsafePointer(); same != (!tt.set && !had) {
				t.Errorf("returned the input map = %v, want %v", same, !tt.set && !had)
			}
		})
	}
}

// buildPod stamps both annotations it owns from its own decisions: the NFS
// home and the in-project agents exclusion together, and neither on a
// plain pod, whatever the caller passed for them.
func TestBuildPod_HomeAndAgentsAnnotations(t *testing.T) {
	shared := sharedRepoWithScionDir(t)
	caller := map[string]string{k8sHomeStorageAnnotation: "caller", k8sWorkspaceAgentsAnnotation: "caller", "other": "x"}
	for _, tt := range []struct {
		name string
		cfg  RunConfig
		want map[string]string
	}{
		{"NFS home, shared workspace", nfsHomeTestConfig(true), map[string]string{
			k8sHomeStorageAnnotation: HomeStorageNFS, k8sWorkspaceAgentsAnnotation: "true", "other": "x",
		}},
		{"plain pod", nfsHomeTestConfig(false), map[string]string{"other": "x"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rt, _, _ := newTestK8sRuntime()
			cfg := tt.cfg
			cfg.Annotations = maps.Clone(caller)
			if tt.want[k8sWorkspaceAgentsAnnotation] == "true" {
				cfg.RepoRoot, cfg.Workspace = shared, shared
			}
			pod, err := rt.buildPod("default", cfg)
			if err != nil {
				t.Fatalf("buildPod: %v", err)
			}
			if !maps.Equal(pod.Annotations, tt.want) {
				t.Errorf("pod annotations = %v, want %v", pod.Annotations, tt.want)
			}
			if !maps.Equal(cfg.Annotations, caller) {
				t.Errorf("buildPod changed the caller's annotations to %v", cfg.Annotations)
			}
		})
	}
}
