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

// Mount-scope regression tests for NFS project storage (ptone/scion#6943).
//
// On a shared Layout every project lives at <subpath root>/<project ID>/
// on the export, beside the project configuration directory (.scion) the
// hub writes. Agent containers must get only their own directories from
// it: the agent workspace (the project workspace for shared-plain), the
// agent home, the declared shared dirs and, read-only, the agent secrets.
// For each runtime and each workspace mode it places on the share, these
// tests build the agent's mounts on an NFS Layout (subpath root
// "projects") in the shape pkg/agent produces and assert that the share
// mounts are exactly that set, and that none is the export root, a parent
// of the project root, the project root, or the project configuration
// directory or anything under it.

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	corev1 "k8s.io/api/core/v1"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	scopeProjectID = "4f1d2c3b-5a69-4e70-8b91-a2b3c4d5e6f7"
	scopeAgent     = "dev"
	scopeAgentID   = "0b9f6a52-3c1e-4f43-9d8e-2a6f1c7b5e10"
	scopeRoot      = "projects"
	scopeExport    = "/exports/scion"
	scopeServer    = "10.0.0.2"
	scopeClaim     = "scion-workspaces"
)

// scopeSharedDirs are the shared dirs every case declares: one read-write,
// one read-only.
var scopeSharedDirs = []api.SharedDir{{Name: "scratch"}, {Name: "ref", ReadOnly: true}}

// shareMount is one mount of a directory on the share, by its
// export-relative path (slash-separated; "" is the export root).
type shareMount struct {
	rel      string
	target   string
	readOnly bool
}

func (m shareMount) String() string {
	ro := "rw"
	if m.readOnly {
		ro = "ro"
	}
	return fmt.Sprintf("%q -> %s (%s)", m.rel, m.target, ro)
}

// scopeRel joins elems below the test project's root.
func scopeRel(elems ...string) string {
	return path.Join(append([]string{scopeRoot, scopeProjectID}, elems...)...)
}

// mountScopeViolation reports why an export-relative path must never be
// mounted into an agent container, or "" when it may be.
func mountScopeViolation(rel string) string {
	rel = path.Clean("/" + rel)[1:] // "" for the export root
	projectRoot := scopeRel()
	config := scopeRel(projectfs.ProjectConfigDirName)
	switch {
	case rel == "":
		return "the export root"
	case rel == projectRoot:
		return "the project root"
	case strings.HasPrefix(projectRoot, rel+"/"):
		return "a parent of the project root"
	case rel == config || strings.HasPrefix(rel, config+"/"):
		return "the project configuration directory or a path under it"
	case rel == scopeRel("agents"):
		return "the parent of every agent directory"
	case rel == scopeRel("shared-dirs"):
		return "the parent of every shared dir"
	}
	return ""
}

// assertMountScope fails when a mount in got is refused by
// mountScopeViolation, and when got is not exactly want (as a set).
func assertMountScope(t *testing.T, got, want []shareMount) {
	t.Helper()
	for _, m := range got {
		if why := mountScopeViolation(m.rel); why != "" {
			t.Errorf("agent mount %s is %s", m, why)
		}
	}
	key := func(ms []shareMount) []string {
		out := make([]string, len(ms))
		for i, m := range ms {
			out[i] = m.String()
		}
		slices.Sort(out)
		return out
	}
	if g, w := key(got), key(want); !slices.Equal(g, w) {
		t.Errorf("agent share mounts differ from the allowed set\n got: %s\nwant: %s",
			strings.Join(g, "\n      "), strings.Join(w, "\n      "))
	}
}

// TestMountScopeViolation pins the refused paths themselves.
func TestMountScopeViolation(t *testing.T) {
	for _, rel := range []string{
		"", ".", "/", "projects", "projects/",
		scopeRel(), scopeRel() + "/",
		scopeRel(".scion"), scopeRel(".scion", "settings.yaml"), scopeRel(".scion", "templates", "x"),
		scopeRel("agents"), scopeRel("shared-dirs"),
	} {
		if mountScopeViolation(rel) == "" {
			t.Errorf("mountScopeViolation(%q) = \"\", want a violation", rel)
		}
	}
	for _, rel := range []string{
		scopeRel("workspace"), scopeRel("workspace", ".scion"), scopeRel("workspace", ".git"),
		scopeRel("workspace", "worktrees", scopeAgent),
		scopeRel("agents", scopeAgent, "workspace"),
		scopeRel("agents", scopeAgent, "home-"+scopeAgentID),
		scopeRel("agents", scopeAgent, "secrets-"+scopeAgentID),
		scopeRel("shared-dirs", "scratch"),
		scopeRel(".scion-x"), "projects-other",
	} {
		if why := mountScopeViolation(rel); why != "" {
			t.Errorf("mountScopeViolation(%q) = %q, want none", rel, why)
		}
	}
}

// scopeSharedDirMounts is the allowed share mounts of scopeSharedDirs at
// their default container targets.
func scopeSharedDirMounts() []shareMount {
	return []shareMount{
		{rel: scopeRel("shared-dirs", "scratch"), target: "/scion-volumes/scratch"},
		{rel: scopeRel("shared-dirs", "ref"), target: "/scion-volumes/ref", readOnly: true},
	}
}

// --- Docker and Podman (buildCommonRunArgs bind emission) ---

// dockerScopeFixture is an NFS project tree under a temporary host base,
// with a local agent home: the local-container runtimes keep the home on
// the broker host and deliver secrets through the environment, so their
// share mounts are the workspace and the shared dirs.
type dockerScopeFixture struct {
	layout  projectfs.Layout
	project projectfs.Project
	home    string
}

func newDockerScopeFixture(t *testing.T) *dockerScopeFixture {
	t.Helper()
	base := t.TempDir()
	t.Setenv("HOME", filepath.Join(base, "home"))
	f := &dockerScopeFixture{
		layout:  projectfs.Layout{Backend: projectfs.BackendNFS, HostBase: filepath.Join(base, "mnt", "share1"), SubPathRoot: scopeRoot},
		project: projectfs.Project{ID: scopeProjectID},
		home:    filepath.Join(base, "home", ".scion", "project-configs", "p__4f1d2c3b", ".scion", "agents", scopeAgent, "home"),
	}
	cfg, err := f.layout.ProjectConfig(scopeProjectID)
	if err != nil {
		t.Fatal(err)
	}
	// The project configuration directory exists, with content, so that a
	// mount of it or of a parent would be emitted rather than refused for a
	// missing source.
	writeFile(t, filepath.Join(cfg.HostPath, "settings.yaml"), "x: 1\n")
	mkdirs(t, f.home)
	return f
}

// workspace resolves and creates the workspace of mode for agent ("" for
// the project workspace).
func (f *dockerScopeFixture) workspace(t *testing.T, mode projectfs.Mode, agent string) string {
	t.Helper()
	r, err := f.layout.Resolve(f.project, agent, projectfs.WorkspaceMount(mode, ""))
	if err != nil {
		t.Fatal(err)
	}
	mkdirs(t, r.HostPath)
	return r.HostPath
}

// sharedDirVolumes resolves and creates scopeSharedDirs as pkg/agent hands
// them to the local-container runtimes: one volume per resolved leaf.
func (f *dockerScopeFixture) sharedDirVolumes(t *testing.T) []api.VolumeMount {
	t.Helper()
	var vols []api.VolumeMount
	for _, sd := range scopeSharedDirs {
		r, err := f.layout.Resolve(f.project, "", projectfs.SharedDirMount(sd, ""))
		if err != nil {
			t.Fatal(err)
		}
		mkdirs(t, r.HostPath)
		vols = append(vols, api.VolumeMount{Source: r.HostPath, Target: r.Target, ReadOnly: sd.ReadOnly})
	}
	return vols
}

// shareMounts splits the binds of args into share mounts and the sources
// of every other bind.
func (f *dockerScopeFixture) shareMounts(t *testing.T, args []string) (share []shareMount, other []string) {
	t.Helper()
	hostBase := layoutRealPath(t, f.layout.HostBase)
	for _, b := range layoutBinds(t, args) {
		src := b.source
		if r, err := filepath.EvalSymlinks(src); err == nil {
			src = r
		}
		rel, err := filepath.Rel(hostBase, src)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			other = append(other, src)
			continue
		}
		if rel == "." {
			rel = ""
		}
		share = append(share, shareMount{rel: filepath.ToSlash(rel), target: b.target, readOnly: b.readOnly})
	}
	return share, other
}

func TestBuildCommonRunArgs_NFSMountScope(t *testing.T) {
	type modeCase struct {
		name string
		// setup returns the run config of the mode on f and the allowed
		// share mounts besides the shared dirs.
		setup func(t *testing.T, f *dockerScopeFixture, cfg RunConfig) (RunConfig, []shareMount)
	}
	modes := []modeCase{
		{name: "shared-plain", setup: func(t *testing.T, f *dockerScopeFixture, cfg RunConfig) (RunConfig, []shareMount) {
			ws := f.workspace(t, projectfs.ModeSharedPlain, "")
			// The workspace's own .scion is repository content, mounted
			// read-only over the workspace.
			mkdirs(t, filepath.Join(ws, ".scion"))
			cfg.Workspace = ws
			return cfg, []shareMount{
				{rel: scopeRel("workspace"), target: "/workspace"},
				{rel: scopeRel("workspace", ".scion"), target: "/workspace/.scion", readOnly: true},
			}
		}},
		// Worktree-per-agent on these runtimes is exactly the shared-plain
		// shape of a git project: the per-agent worktree on the share is
		// Kubernetes-only (pkg/agent nfsWorktreeSelection), so the agent
		// gets the project workspace, with the repository root recomposed
		// to it (composeSharedRepoRoot).
		{name: "worktree-per-agent", setup: func(t *testing.T, f *dockerScopeFixture, cfg RunConfig) (RunConfig, []shareMount) {
			ws := f.workspace(t, projectfs.ModeWorktreePerAgent, "")
			layoutGit(t, ws, "init", "-q")
			mkdirs(t, filepath.Join(ws, ".scion"))
			cfg.RepoRoot, cfg.Workspace = ws, ws
			// pkg/agent seeds the per-agent private .git/config copy
			// bound over the repository's (off the share).
			cfg.PrivateGitConfigSource = seedTestPrivateGitConfig(t)
			// The workspace's .git admin paths are narrowed read-only,
			// all inside the project workspace.
			git := func(elems ...string) string { return scopeRel(append([]string{"workspace", ".git"}, elems...)...) }
			return cfg, []shareMount{
				{rel: scopeRel("workspace"), target: "/workspace"},
				{rel: scopeRel("workspace", ".scion"), target: "/workspace/.scion", readOnly: true},
				{rel: git("config.worktree"), target: "/workspace/.git/config.worktree", readOnly: true},
				{rel: git("hooks"), target: "/workspace/.git/hooks", readOnly: true},
				{rel: git("info"), target: "/workspace/.git/info", readOnly: true},
				{rel: git("objects", "info"), target: "/workspace/.git/objects/info", readOnly: true},
				{rel: git("scion-sharers"), target: "/workspace/.git/scion-sharers", readOnly: true},
			}
		}},
		// Clone-per-agent is not on the share for these runtimes
		// (TestSelectWorkspaceBackend_NFSClonePerAgentIsLocal), and
		// empty-per-agent on NFS is refused for them in pkg/agent
		// (errEmptyPerAgentNFSRuntime).
	}
	for _, rt := range []string{"docker", "podman"} {
		for _, mc := range modes {
			t.Run(rt+"/"+mc.name, func(t *testing.T) {
				f := newDockerScopeFixture(t)
				cfg, want := mc.setup(t, f, RunConfig{
					Harness: &harness.Generic{}, Name: scopeAgent, UnixUsername: "scion", Image: "scion-agent:latest",
					RuntimeName: rt, WorkspaceBackendName: "nfs", ProjectID: scopeProjectID,
					HomeDir: f.home, SharedDirs: scopeSharedDirs, Volumes: f.sharedDirVolumes(t),
				})
				args, err := buildCommonRunArgs(cfg)
				if err != nil {
					t.Fatalf("buildCommonRunArgs: %v", err)
				}
				share, other := f.shareMounts(t, args)
				assertMountScope(t, share, append(want, scopeSharedDirMounts()...))
				// Off the share: only the local home and the private git
				// config copy.
				allowedOther := []string{layoutRealPath(t, f.home)}
				if cfg.PrivateGitConfigSource != "" {
					allowedOther = append(allowedOther, layoutRealPath(t, cfg.PrivateGitConfigSource))
				}
				for _, src := range other {
					if !slices.Contains(allowedOther, src) {
						t.Errorf("unexpected bind source off the share: %s", src)
					}
				}
			})
		}
	}
}

// On an nfs workspace_storage, clone-per-agent keeps its workspace on the
// node (the local backend): the share mounts of the local-container
// runtimes are then only the shared dirs.
func TestSelectWorkspaceBackend_NFSClonePerAgentIsLocal(t *testing.T) {
	cfg := &config.V1WorkspaceStorageConfig{Backend: "nfs", NFS: &config.V1NFSConfig{}}
	if got := SelectWorkspaceBackend(cfg, store.SharingModeClonePerAgent).Name(); got != "local" {
		t.Errorf("clone-per-agent on nfs selects backend %q, want local", got)
	}
}

// --- Kubernetes (buildPod volumes and subPaths) ---

// k8sScopeShare names how the pod reaches the share and where the home
// is: on the share (nfsHome, which needs a claim) or in the pod.
type k8sScopeShare struct {
	name    string
	inline  bool
	nfsHome bool
}

// k8sScopeConfig is an NFS pod config for the test project with the
// shared dirs on the workspace share and, when share.nfsHome, an NFS home. workspace is the host
// path of the project workspace, which the broker checks for a .scion
// entry.
func k8sScopeConfig(share k8sScopeShare, workspace string) RunConfig {
	cfg := withTestHomeStorage(RunConfig{
		Name: scopeAgent, Image: "test-image", UnixUsername: "scion",
		WorkspaceBackendName: "nfs", ProjectID: scopeProjectID, NFSSubPathRoot: scopeRoot,
		NFSSubPath: scopeRel("workspace"), Workspace: workspace,
		SharedDirs: scopeSharedDirs,
		GitCloneForInit: &api.GitCloneConfig{
			URL: "https://example.com/repo.git", Branch: "main",
		},
		Labels: map[string]string{"scion.project": "p"},
	})
	cfg.HomeStorage.ProjectID, cfg.HomeStorage.AgentSlug, cfg.HomeStorage.AgentID = scopeProjectID, scopeAgent, scopeAgentID
	cfg.HomeStorage.SubPathRoot = scopeRoot
	cfg.HomeStorage.PVClaimName = scopeClaim
	sd := &SharedDirRealization{
		Backend: "nfs", ProjectID: scopeProjectID, SubPathRoot: scopeRoot,
		SubPaths: map[string]string{},
	}
	for _, d := range scopeSharedDirs {
		sd.SubPaths[d.Name] = scopeRel("shared-dirs", d.Name)
	}
	if share.inline {
		cfg.NFSShareServer, cfg.NFSShareExport = scopeServer, scopeExport
		sd.Server, sd.Export = scopeServer, scopeExport
	} else {
		cfg.NFSPVClaimName = scopeClaim
		sd.PVClaimName = scopeClaim
	}
	cfg.SharedDirStorage = sd
	if !share.nfsHome {
		cfg.HomeStorageBackend, cfg.HomeStorage = "", nil
	}
	return cfg
}

// podShareMounts returns the share mounts of c in pod, and fails for a pod
// volume that is a host path or a claim other than the share's.
func podShareMounts(t *testing.T, pod *corev1.Pod, c corev1.Container) []shareMount {
	t.Helper()
	vols := map[string]corev1.Volume{}
	for _, v := range pod.Spec.Volumes {
		vols[v.Name] = v
	}
	var out []shareMount
	for _, vm := range c.VolumeMounts {
		v, ok := vols[vm.Name]
		if !ok {
			t.Fatalf("container %s mounts unknown volume %q", c.Name, vm.Name)
		}
		var volRel string
		switch {
		case v.PersistentVolumeClaim != nil:
			if v.PersistentVolumeClaim.ClaimName != scopeClaim {
				t.Errorf("container %s mounts claim %q, not the share's", c.Name, v.PersistentVolumeClaim.ClaimName)
				continue
			}
			volRel = "" // the claim's root is the export root
		case v.NFS != nil:
			rel, ok := strings.CutPrefix(v.NFS.Path, scopeExport+"/")
			if v.NFS.Path == scopeExport {
				rel, ok = "", true
			}
			if !ok || v.NFS.Server != scopeServer {
				t.Errorf("container %s mounts NFS volume %s:%s outside the share", c.Name, v.NFS.Server, v.NFS.Path)
				continue
			}
			volRel = rel
			if v.NFS.ReadOnly && !vm.ReadOnly {
				vm.ReadOnly = true
			}
		case v.HostPath != nil:
			t.Errorf("container %s mounts host path %s", c.Name, v.HostPath.Path)
			continue
		case v.Secret != nil, v.ConfigMap != nil, v.EmptyDir != nil, v.Projected != nil, v.DownwardAPI != nil:
			continue // not on the share
		default:
			t.Errorf("container %s mounts volume %q of a kind this test does not classify; add it as on or off the share", c.Name, vm.Name)
			continue
		}
		out = append(out, shareMount{rel: path.Join(volRel, vm.SubPath), target: vm.MountPath, readOnly: vm.ReadOnly})
	}
	return out
}

// k8sHomeMount is the allowed share mount of the NFS home.
func k8sHomeMount() shareMount {
	return shareMount{rel: scopeRel("agents", scopeAgent, "home-"+scopeAgentID), target: "/home/scion"}
}

func TestBuildPod_NFSMountScope(t *testing.T) {
	type modeCase struct {
		name string
		// setup sets the mode on cfg and returns the allowed share mounts
		// of the agent container besides the home and the shared dirs.
		setup func(cfg RunConfig) (RunConfig, []shareMount)
		// noInline: buildPod refuses the mode on an inline share.
		noInline bool
	}
	modes := []modeCase{
		{name: "shared-plain", setup: func(cfg RunConfig) (RunConfig, []shareMount) {
			return cfg, []shareMount{
				{rel: scopeRel("workspace"), target: "/workspace"},
				{rel: scopeRel("workspace", ".scion"), target: "/workspace/.scion", readOnly: true},
			}
		}},
		{name: "clone-per-agent", setup: func(cfg RunConfig) (RunConfig, []shareMount) {
			cfg.NFSAgentDirName, cfg.NFSAgentBranch = scopeAgent, scopeAgent
			cfg.AgentOwnWorkspace = true
			return cfg, []shareMount{{rel: scopeRel("agents", scopeAgent, "workspace"), target: "/workspace"}}
		}},
		{name: "empty-per-agent", setup: func(cfg RunConfig) (RunConfig, []shareMount) {
			cfg.NFSAgentDirName, cfg.NFSAgentDirEmpty = scopeAgent, true
			cfg.AgentOwnWorkspace = true
			cfg.GitCloneForInit = nil
			return cfg, []shareMount{{rel: scopeRel("agents", scopeAgent, "workspace"), target: "/workspace"}}
		}},
		{name: "worktree-per-agent", noInline: true, setup: func(cfg RunConfig) (RunConfig, []shareMount) {
			cfg.NFSWorktreeName, cfg.NFSWorktreeBranch = scopeAgent, scopeAgent
			cfg.AgentOwnWorkspace = true
			git := func(elems ...string) string { return scopeRel(append([]string{"workspace", ".git"}, elems...)...) }
			wt := "/repo-root/worktrees/" + scopeAgent
			return cfg, []shareMount{
				{rel: git(), target: "/repo-root/.git"},
				{rel: scopeRel("workspace", "worktrees", scopeAgent), target: wt},
				{rel: git("hooks"), target: "/repo-root/.git/hooks", readOnly: true},
				{rel: git("info"), target: "/repo-root/.git/info", readOnly: true},
				{rel: git("objects", "info"), target: "/repo-root/.git/objects/info", readOnly: true},
				{rel: git("worktrees"), target: "/repo-root/.git/worktrees", readOnly: true},
				{rel: git("worktrees", scopeAgent), target: "/repo-root/.git/worktrees/" + scopeAgent},
			}
		}},
	}
	// The NFS home has its own mount check (no other mount may reach the
	// home), so the claim is covered both with and without it.
	for _, share := range []k8sScopeShare{
		{name: "claim-nfs-home", nfsHome: true},
		{name: "claim-pod-home"},
		{name: "inline-pod-home", inline: true},
	} {
		for _, mc := range modes {
			t.Run(share.name+"/"+mc.name, func(t *testing.T) {
				ws := filepath.Join(t.TempDir(), "workspace")
				mkdirs(t, filepath.Join(ws, ".scion"))
				cfg, want := mc.setup(k8sScopeConfig(share, ws))
				pod, err := newNFSTestK8sRuntime().buildPod("default", cfg)
				if share.inline && mc.noInline {
					if err == nil {
						t.Fatal("buildPod accepted a mode it does not support on an inline share")
					}
					return
				}
				if err != nil {
					t.Fatalf("buildPod: %v", err)
				}
				if len(pod.Spec.Containers) != 1 {
					t.Fatalf("pod has %d containers, want 1", len(pod.Spec.Containers))
				}
				if share.nfsHome {
					want = append(want, k8sHomeMount())
				}
				want = append(want, scopeSharedDirMounts()...)
				assertMountScope(t, podShareMounts(t, pod, pod.Spec.Containers[0]), want)
				// The init containers prepare the agent's directories; they
				// get more of the project tree than the agent, but never the
				// project root or its configuration directory either.
				for _, ic := range pod.Spec.InitContainers {
					for _, m := range podShareMounts(t, pod, ic) {
						if why := mountScopeViolation(m.rel); why != "" {
							t.Errorf("init container %s mount %s is %s", ic.Name, m, why)
						}
					}
				}
			})
		}
	}
}

// --- Cloud Run (instance NFS volumes) ---

// cloudRunShareMounts returns the share mounts of the agent container of
// inst.
func cloudRunShareMounts(t *testing.T, inst *runpb.Instance) []shareMount {
	t.Helper()
	vols := map[string]*runpb.Volume{}
	for _, v := range inst.GetVolumes() {
		vols[v.GetName()] = v
	}
	var out []shareMount
	for _, c := range inst.GetContainers() {
		for _, vm := range c.GetVolumeMounts() {
			v, ok := vols[vm.GetName()]
			if !ok {
				t.Fatalf("container mounts unknown volume %q", vm.GetName())
			}
			nfs := v.GetNfs()
			if nfs == nil {
				t.Errorf("volume %q is not an NFS volume", vm.GetName())
				continue
			}
			rel, ok := strings.CutPrefix(nfs.GetPath(), scopeExport+"/")
			if nfs.GetPath() == scopeExport {
				rel, ok = "", true
			}
			if !ok || nfs.GetServer() != scopeServer {
				t.Errorf("volume %q is %s:%s, outside the share", vm.GetName(), nfs.GetServer(), nfs.GetPath())
				continue
			}
			out = append(out, shareMount{rel: rel, target: vm.GetMountPath(), readOnly: nfs.GetReadOnly()})
		}
	}
	return out
}

// Cloud Run mounts the project workspace, the agent home and, read-only,
// the agent secrets, whatever the workspace mode: buildCloudRunInstance does
// not read it. It has no shared dirs on the share, and Run refuses
// empty-per-agent before any path is built.
func TestCloudRunInstance_NFSMountScope(t *testing.T) {
	paths, err := cloudRunNFSExportPaths(scopeExport, scopeRoot, scopeProjectID, scopeAgent, scopeAgentID)
	if err != nil {
		t.Fatalf("cloudRunNFSExportPaths: %v", err)
	}
	paths.server = scopeServer
	r := &CloudRunRuntime{}
	home := "/home/" + r.ExecUser()
	sharedPlain := []shareMount{
		{rel: scopeRel("workspace"), target: "/workspace"},
		{rel: scopeRel("agents", scopeAgent, "home-"+scopeAgentID), target: home},
		{rel: scopeRel("agents", scopeAgent, "secrets-"+scopeAgentID), target: home + "/.scion/secrets", readOnly: true},
	}
	cfgFor := func(mode string) RunConfig {
		return RunConfig{
			Name: scopeAgent, Image: "test-image", WorkspaceBackendName: "nfs", ProjectID: scopeProjectID,
			NFSSubPathRoot: scopeRoot, Env: []string{"SCION_WORKSPACE_MODE=" + mode},
			Labels: map[string]string{"agent_id": scopeAgentID},
		}
	}
	for _, mode := range []string{"shared-plain", "clone-per-agent", "worktree-per-agent"} {
		t.Run(mode, func(t *testing.T) {
			got := cloudRunShareMounts(t, r.buildCloudRunInstance(cfgFor(mode), 1000, 1000, paths))
			if mode == "shared-plain" {
				assertMountScope(t, got, sharedPlain)
				return
			}
			// Clone-per-agent and worktree-per-agent get the project
			// workspace today; clone-per-agent is moving to a per-agent
			// workspace (#6926), so only the refused paths are asserted
			// for them until then.
			for _, m := range got {
				if why := mountScopeViolation(m.rel); why != "" {
					t.Errorf("agent mount %s is %s", m, why)
				}
			}
		})
	}

	t.Run("empty-per-agent refused by Run", func(t *testing.T) {
		_, err := r.Run(t.Context(), cfgFor("empty-per-agent"))
		if !errors.Is(err, errEmptyPerAgentCloudRun) {
			t.Errorf("Run(empty-per-agent) = %v, want %v: the agent would get the project workspace", err, errEmptyPerAgentCloudRun)
		}
	})
}
