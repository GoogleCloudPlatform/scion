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
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
)

const (
	inlineTestServer = "192.0.2.10"
	inlineTestExport = "/scion-export"
)

func TestPodVolumeSource(t *testing.T) {
	src, err := podVolumeSource(projectfs.Realized{ClaimName: "pv", SubPath: "projects/p/workspace", Target: "/workspace", ReadOnly: true})
	require.NoError(t, err)
	assert.Equal(t, corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "pv", ReadOnly: true}}, src)

	src, err = podVolumeSource(projectfs.Realized{NFS: &projectfs.NFSSource{Server: "s", Path: "/e/projects/p/workspace"}, Target: "/workspace"})
	require.NoError(t, err)
	assert.Equal(t, corev1.VolumeSource{NFS: &corev1.NFSVolumeSource{Server: "s", Path: "/e/projects/p/workspace"}}, src)

	for name, r := range map[string]projectfs.Realized{
		"neither":         {Target: "/t"},
		"both":            {ClaimName: "pv", NFS: &projectfs.NFSSource{Server: "s", Path: "/p"}, Target: "/t"},
		"inline no path":  {NFS: &projectfs.NFSSource{Server: "s"}, Target: "/t"},
		"inline subPath":  {NFS: &projectfs.NFSSource{Server: "s", Path: "/p"}, SubPath: "x", Target: "/t"},
		"inline noserver": {NFS: &projectfs.NFSSource{Path: "/p"}, Target: "/t"},
	} {
		if _, err := podVolumeSource(r); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestNFSPodShare(t *testing.T) {
	claim := NFSPodShare{ClaimName: "pv", Server: "s", Export: "/e"}
	inline := NFSPodShare{Server: "s", Export: "/e"}
	assert.True(t, claim.Mountable())
	assert.False(t, claim.Inline())
	assert.True(t, inline.Mountable())
	assert.True(t, inline.Inline())
	assert.False(t, NFSPodShare{Server: "s"}.Mountable())
	assert.False(t, NFSPodShare{}.Mountable())

	assert.True(t, claim.Same(NFSPodShare{ClaimName: "pv"}))
	assert.False(t, claim.Same(NFSPodShare{ClaimName: "other"}))
	assert.False(t, claim.Same(inline), "a claim and an inline share are different volumes")
	assert.True(t, inline.Same(NFSPodShare{Server: "s", Export: "/e"}))
	assert.False(t, inline.Same(NFSPodShare{Server: "s", Export: "/other"}))
	assert.False(t, NFSPodShare{}.Same(NFSPodShare{}), "unmountable shares are never the same volume")
}

// inlineTestConfig is cfg with its workspace (and shared-dir) share
// switched from the claim to inline: no claim, the share's server and
// export.
func inlineTestConfig(cfg RunConfig) RunConfig {
	cfg.NFSPVClaimName = ""
	cfg.NFSShareServer, cfg.NFSShareExport = inlineTestServer, inlineTestExport
	if cfg.SharedDirStorage != nil {
		sd := *cfg.SharedDirStorage
		sd.PVClaimName = ""
		sd.Server, sd.Export, sd.SubPathRoot = inlineTestServer, inlineTestExport, "projects"
		cfg.SharedDirStorage = &sd
	}
	return cfg
}

// claimTestConfig is cfg with the share's server and export set as well,
// as pkg/agent sets them; with a claim they change nothing.
func claimTestConfig(cfg RunConfig) RunConfig {
	cfg.NFSShareServer, cfg.NFSShareExport = inlineTestServer, inlineTestExport
	return cfg
}

// assertInlineMatchesClaim checks the inline pod against the claim pod of
// the same start: every container keeps the same mount targets in the same
// order; each claim subPath mount of the share is, inline, a mount with no
// subPath of its own NFS volume whose path is <export>/<subPath> (the
// directory's server path) strictly under the project's tree, except the
// workspace's .scion entry, which is a subPath mount of the inline
// "workspace" volume at the same server path; the inline pod has no claim
// volume at all.
func assertInlineMatchesClaim(t *testing.T, claimPod, inlinePod *corev1.Pod, projectID string) {
	t.Helper()
	projectPrefix := path.Join(inlineTestExport, "projects", projectID) + "/"
	for _, v := range inlinePod.Spec.Volumes {
		assert.Nil(t, v.PersistentVolumeClaim, "inline pod volume %q must not be a claim", v.Name)
		if v.NFS != nil {
			assert.Equal(t, inlineTestServer, v.NFS.Server, v.Name)
			assert.True(t, strings.HasPrefix(v.NFS.Path, projectPrefix) && len(v.NFS.Path) > len(projectPrefix),
				"inline volume %q path %q must be strictly under %q", v.Name, v.NFS.Path, projectPrefix)
			assert.NotEqual(t, inlineTestExport, v.NFS.Path, "no container gets the whole export")
		}
	}
	claimContainers := append(append([]corev1.Container{}, claimPod.Spec.InitContainers...), claimPod.Spec.Containers...)
	inlineContainers := append(append([]corev1.Container{}, inlinePod.Spec.InitContainers...), inlinePod.Spec.Containers...)
	require.Equal(t, len(claimContainers), len(inlineContainers))
	nfsMounts := 0
	for i := range claimContainers {
		cc, ic := claimContainers[i], inlineContainers[i]
		require.Equal(t, cc.Name, ic.Name)
		require.Equal(t, len(cc.VolumeMounts), len(ic.VolumeMounts), cc.Name)
		for j, cm := range cc.VolumeMounts {
			im := ic.VolumeMounts[j]
			assert.Equal(t, cm.MountPath, im.MountPath, "%s mount %d target", cc.Name, j)
			assert.Equal(t, cm.ReadOnly, im.ReadOnly, "%s mount %s", cc.Name, cm.MountPath)
			cv := findVolume(claimPod, cm.Name)
			require.NotNil(t, cv, cm.Name)
			if cv.PersistentVolumeClaim == nil {
				continue
			}
			nfsMounts++
			iv := findVolume(inlinePod, im.Name)
			require.NotNil(t, iv, im.Name)
			require.NotNil(t, iv.NFS, "%s mount %s must be an inline NFS volume", cc.Name, cm.MountPath)
			assert.Equal(t, path.Join(inlineTestExport, cm.SubPath), path.Join(iv.NFS.Path, im.SubPath), "%s mount %s server path", cc.Name, cm.MountPath)
			if path.Base(cm.MountPath) == projectfs.ProjectConfigEntryName {
				assert.Equal(t, nfsVolWorkspace, im.Name, "%s mount %s is from the workspace volume", cc.Name, cm.MountPath)
				assert.Equal(t, projectfs.ProjectConfigEntryName, im.SubPath, "%s mount %s", cc.Name, cm.MountPath)
				continue
			}
			assert.Empty(t, im.SubPath, "%s mount %s", cc.Name, cm.MountPath)
			assert.Equal(t, cm.ReadOnly || cv.PersistentVolumeClaim.ReadOnly, iv.NFS.ReadOnly, "%s mount %s volume read-only", cc.Name, cm.MountPath)
		}
	}
	assert.NotZero(t, nfsMounts)
}

// buildClaimAndInline builds the pod of cfg (a claim config) with the
// claim and inline.
func buildClaimAndInline(t *testing.T, cfg RunConfig) (claimPod, inlinePod *corev1.Pod) {
	t.Helper()
	var err error
	claimPod, err = newNFSTestK8sRuntime().buildPod("default", claimTestConfig(cfg))
	require.NoError(t, err)
	inlinePod, err = newNFSTestK8sRuntime().buildPod("default", inlineTestConfig(cfg))
	require.NoError(t, err)
	return claimPod, inlinePod
}

func TestBuildPod_InlineSharedPlainSharedDirsProjectConfig(t *testing.T) {
	cfg, _ := nfsClaimGoldenConfig(t)
	claimPod, inlinePod := buildClaimAndInline(t, cfg)
	assertInlineMatchesClaim(t, claimPod, inlinePod, "proj-123")

	// The workspace, the provisioning state dir and both shared dirs, by
	// server path.
	for name, want := range map[string]struct {
		path     string
		readOnly bool
	}{
		"workspace":           {inlineTestExport + "/projects/proj-123/workspace", false},
		"workspace-provision": {inlineTestExport + "/projects/proj-123/provision", false},
		"shared-dir-0":        {inlineTestExport + "/projects/proj-123/shared-dirs/notes", false},
		"shared-dir-1":        {inlineTestExport + "/projects/proj-123/shared-dirs/cache", true},
	} {
		v := findVolume(inlinePod, name)
		require.NotNil(t, v, name)
		require.NotNil(t, v.NFS, name)
		assert.Equal(t, want.path, v.NFS.Path, name)
		assert.Equal(t, want.readOnly, v.NFS.ReadOnly, name)
	}
	assertInlineProjectConfigFromWorkspace(t, inlinePod)
	assert.True(t, k8sPodWorkspaceRecoverable(inlinePod), "the inline workspace volume is persistent")
}

// assertInlineProjectConfigFromWorkspace checks that the agent container
// mounts the workspace's .scion entry read-only from the inline
// "workspace" volume by the subPath ".scion", and that no inline volume of
// its own names the entry or anything under it.
func assertInlineProjectConfigFromWorkspace(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	ws := findVolume(pod, nfsVolWorkspace)
	require.NotNil(t, ws)
	require.NotNil(t, ws.NFS)
	entryPath := path.Join(ws.NFS.Path, projectfs.ProjectConfigEntryName)
	for _, v := range pod.Spec.Volumes {
		if v.NFS != nil {
			assert.False(t, v.NFS.Path == entryPath || strings.HasPrefix(v.NFS.Path, entryPath+"/"),
				"volume %q mounts %s on its own; the entry comes from the workspace volume", v.Name, v.NFS.Path)
		}
	}
	var found bool
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.MountPath != "/workspace/"+projectfs.ProjectConfigEntryName {
			continue
		}
		found = true
		assert.Equal(t, nfsVolWorkspace, m.Name)
		assert.Equal(t, projectfs.ProjectConfigEntryName, m.SubPath)
		assert.True(t, m.ReadOnly)
	}
	assert.True(t, found, "the agent container mounts the workspace's .scion")
}

// Inline, with the in-project agents root masked, the .scion entry is
// still mounted from the workspace volume and the mask over it stays an
// empty in-memory volume: neither is an inline NFS volume of its own.
func TestBuildPod_InlineProjectConfigAndAgentsMask(t *testing.T) {
	cfg, ws := nfsClaimGoldenConfig(t)
	require.NoError(t, os.MkdirAll(filepath.Join(ws, ".scion", "agents"), 0o755))
	cfg.RepoRoot = ws
	claimPod, inlinePod := buildClaimAndInline(t, cfg)
	assertInlineMatchesClaim(t, claimPod, inlinePod, "proj-123")
	assertInlineProjectConfigFromWorkspace(t, inlinePod)

	mask := findVolume(inlinePod, k8sAgentsMaskVolume)
	require.NotNil(t, mask, "the agents root is masked")
	assert.NotNil(t, mask.EmptyDir)
	assert.Nil(t, mask.NFS)
	var maskMount *corev1.VolumeMount
	for i, m := range inlinePod.Spec.Containers[0].VolumeMounts {
		if m.Name == k8sAgentsMaskVolume {
			maskMount = &inlinePod.Spec.Containers[0].VolumeMounts[i]
		}
	}
	require.NotNil(t, maskMount)
	assert.Equal(t, "/workspace/.scion/agents", maskMount.MountPath)
	assert.Empty(t, maskMount.SubPath)
}

// mountWithin mounts an entry from its parent's inline volume by the
// entry's path relative to it, and refuses an entry outside that
// directory or a parent volume that is not there; with a claim it is the
// claim subPath mount.
func TestNFSPodVolumes_MountWithin(t *testing.T) {
	cfg := inlineTestConfig(nfsBaseConfig("mw"))
	b := newWorkspaceNFSPodVolumes(cfg)
	_, err := b.mountWithin(nfsVolWorkspace, "projects/proj-123/workspace/.scion", "/workspace/.scion", true)
	require.Error(t, err, "the parent volume is not there yet")
	assert.Contains(t, err.Error(), "needs the volume")

	_, err = b.mount(nfsVolWorkspace, "projects/proj-123/workspace", "/workspace", false)
	require.NoError(t, err)
	m, err := b.mountWithin(nfsVolWorkspace, "projects/proj-123/workspace/.scion", "/workspace/.scion", true)
	require.NoError(t, err)
	assert.Equal(t, corev1.VolumeMount{Name: nfsVolWorkspace, MountPath: "/workspace/.scion", SubPath: ".scion", ReadOnly: true}, m)
	assert.Len(t, b.inline, 1, "no volume of its own")

	for _, sub := range []string{"projects/proj-123/workspace", "projects/proj-123/workspace-other/.scion", "projects/proj-123/provision"} {
		_, err := b.mountWithin(nfsVolWorkspace, sub, "/x", true)
		assert.Error(t, err, "subPath %q is not inside the workspace volume", sub)
	}
	_, err = b.mountWithin(nfsVolWorkspace, "projects/other/workspace/.scion", "/x", true)
	assert.Error(t, err, "outside the project tree")

	// A parent volume of the same path on another server does not
	// contain the entry.
	otherServer := newWorkspaceNFSPodVolumes(cfg)
	otherServer.inline = []corev1.Volume{{Name: nfsVolWorkspace, VolumeSource: corev1.VolumeSource{
		NFS: &corev1.NFSVolumeSource{Server: "192.0.2.99", Path: inlineTestExport + "/projects/proj-123/workspace"}}}}
	_, err = otherServer.mountWithin(nfsVolWorkspace, "projects/proj-123/workspace/.scion", "/workspace/.scion", true)
	require.Error(t, err, "the parent volume is on another server")
	assert.Contains(t, err.Error(), "is not inside volume")

	claim := newWorkspaceNFSPodVolumes(claimTestConfig(nfsBaseConfig("mw")))
	m, err = claim.mountWithin(nfsVolWorkspace, "projects/proj-123/workspace/.scion", "/workspace/.scion", true)
	require.NoError(t, err)
	assert.Equal(t, corev1.VolumeMount{Name: "workspace", MountPath: "/workspace/.scion", SubPath: "projects/proj-123/workspace/.scion", ReadOnly: true}, m)
}

// Inline, buildPod refuses the worktree-per-agent shape (Run refuses it
// at start today); the claim pod of the same start still builds.
func TestBuildPod_InlineWorktreeRefused(t *testing.T) {
	cfg := nfsWorktreeConfig("wt")
	_, err := newNFSTestK8sRuntime().buildPod("default", claimTestConfig(cfg))
	require.NoError(t, err)
	_, err = newNFSTestK8sRuntime().buildPod("default", inlineTestConfig(cfg))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "inline NFS volumes do not support worktree-per-agent yet")
}

// Inline, the .git admin entries are subPath mounts of the .git volume
// and add no volumes of their own; they need that volume to be there.
// With a claim they are k8sGitAdminReadOnlyMounts' claim subPath mounts.
func TestK8sNFSGitAdminMounts(t *testing.T) {
	const gitSubPath = "projects/proj-123/workspace/.git"
	cfg := nfsWorktreeConfig("wt")

	b := newWorkspaceNFSPodVolumes(inlineTestConfig(cfg))
	_, err := k8sNFSGitAdminMounts(b, gitSubPath, "agent-1")
	require.Error(t, err, "the .git volume is not there yet")
	assert.Contains(t, err.Error(), "needs the volume")

	_, err = b.mount(nfsVolGit, gitSubPath, "/repo-root/.git", false)
	require.NoError(t, err)
	got, err := k8sNFSGitAdminMounts(b, gitSubPath, "agent-1")
	require.NoError(t, err)
	assert.Equal(t, []corev1.VolumeMount{
		{Name: nfsVolGit, MountPath: "/repo-root/.git/hooks", SubPath: "hooks", ReadOnly: true},
		{Name: nfsVolGit, MountPath: "/repo-root/.git/info", SubPath: "info", ReadOnly: true},
		{Name: nfsVolGit, MountPath: "/repo-root/.git/objects/info", SubPath: "objects/info", ReadOnly: true},
		{Name: nfsVolGit, MountPath: "/repo-root/.git/worktrees", SubPath: "worktrees", ReadOnly: true},
		{Name: nfsVolGit, MountPath: "/repo-root/.git/worktrees/agent-1", SubPath: "worktrees/agent-1"},
	}, got)
	require.Len(t, b.inline, 1, "only the .git volume")
	assert.Equal(t, nfsVolGit, b.inline[0].Name)
	assert.Equal(t, inlineTestExport+"/"+gitSubPath, b.inline[0].NFS.Path)

	claim := newWorkspaceNFSPodVolumes(claimTestConfig(cfg))
	got, err = k8sNFSGitAdminMounts(claim, gitSubPath, "agent-1")
	require.NoError(t, err)
	assert.Equal(t, k8sGitAdminReadOnlyMounts(gitSubPath, "agent-1"), got)
	assert.Empty(t, claim.inline)
}

func TestBuildPod_InlineAgentDirModes(t *testing.T) {
	for name, mutate := range map[string]func(*RunConfig){
		"clone-per-agent": func(c *RunConfig) { c.NFSAgentDirName, c.NFSAgentBranch = "agent-1", "agent-1" },
		"empty-per-agent": func(c *RunConfig) {
			c.NFSAgentDirName, c.NFSAgentDirEmpty, c.GitCloneForInit = "agent-1", true, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := nfsBaseConfig("ad")
			mutate(&cfg)
			claimPod, inlinePod := buildClaimAndInline(t, cfg)
			assertInlineMatchesClaim(t, claimPod, inlinePod, "proj-123")
			ws := findVolume(inlinePod, "workspace")
			require.NotNil(t, ws)
			assert.Equal(t, inlineTestExport+"/projects/proj-123/agents/agent-1/workspace", ws.NFS.Path)
			dir := findVolume(inlinePod, "workspace-agent-dir")
			require.NotNil(t, dir)
			assert.Equal(t, inlineTestExport+"/projects/proj-123/agents/agent-1", dir.NFS.Path)
			for _, v := range inlinePod.Spec.Volumes {
				if v.NFS != nil {
					assert.NotEqual(t, inlineTestExport+"/projects/proj-123/workspace", v.NFS.Path, "the shared workspace is not mounted")
				}
			}
		})
	}
}

// Inline, a workspace subPath outside the project tree (here the claim
// root, an empty subPath) is refused: no container gets the export.
func TestBuildPod_InlineOutsideProjectTreeRefused(t *testing.T) {
	for _, sub := range []string{"", "projects", "projects/other/workspace", "projects/proj-123"} {
		cfg := inlineTestConfig(nfsBaseConfig("x"))
		cfg.NFSSubPath = sub
		_, err := newNFSTestK8sRuntime().buildPod("default", cfg)
		assert.Error(t, err, "subPath %q", sub)
	}
}

// Inline, a .scion marker file is mounted read-only from the workspace
// volume by the subPath ".scion", as with a claim (only the volume source
// differs), and adds no volume of its own.
func TestBuildPod_InlineProjectConfigMarkerFileMounted(t *testing.T) {
	cfg, ws := nfsClaimGoldenConfig(t)
	_, dirPod := buildClaimAndInline(t, cfg)

	require.NoError(t, os.Remove(filepath.Join(ws, ".scion")))
	require.NoError(t, os.WriteFile(filepath.Join(ws, ".scion"), []byte("marker"), 0o644))
	claimPod, inlinePod := buildClaimAndInline(t, cfg)
	assertInlineMatchesClaim(t, claimPod, inlinePod, "proj-123")
	assertInlineProjectConfigFromWorkspace(t, inlinePod)

	entryMount := func(pod *corev1.Pod) corev1.VolumeMount {
		t.Helper()
		for _, m := range pod.Spec.Containers[0].VolumeMounts {
			if m.MountPath == "/workspace/.scion" {
				return m
			}
		}
		t.Fatal("no /workspace/.scion mount")
		return corev1.VolumeMount{}
	}
	claimEntry, inlineEntry := entryMount(claimPod), entryMount(inlinePod)
	assert.Equal(t, corev1.VolumeMount{Name: "workspace", MountPath: "/workspace/.scion", SubPath: "projects/proj-123/workspace/.scion", ReadOnly: true}, claimEntry)
	assert.Equal(t, corev1.VolumeMount{Name: nfsVolWorkspace, MountPath: "/workspace/.scion", SubPath: ".scion", ReadOnly: true}, inlineEntry)
	assert.Equal(t, path.Join(inlineTestExport, claimEntry.SubPath), path.Join(findVolume(inlinePod, inlineEntry.Name).NFS.Path, inlineEntry.SubPath),
		"same directory on the export")

	assert.Equal(t, dirPod.Spec.Volumes, inlinePod.Spec.Volumes, "a marker file adds no volume")
}

// Shared dirs with neither a claim nor the share's server and export (the
// transitional split without its claim) still fail closed.
func TestBuildPod_InlineSharedDirsWithoutShareFailClosed(t *testing.T) {
	cfg, _ := nfsClaimGoldenConfig(t)
	cfg = inlineTestConfig(cfg)
	cfg.SharedDirStorage.Server, cfg.SharedDirStorage.Export = "", ""
	_, err := newNFSTestK8sRuntime().buildPod("default", cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no claim")
}

// Inline shared dirs need no claim: nothing gets or creates a
// PersistentVolumeClaim.
func TestCreateSharedDirPVCs_InlineNoClaimCalls(t *testing.T) {
	cfg, _ := nfsClaimGoldenConfig(t)
	cfg = inlineTestConfig(cfg)
	rt, clientset, _ := newTestK8sRuntime()
	require.NoError(t, rt.createSharedDirPVCs(t.Context(), "default", cfg))
	for _, a := range clientset.Actions() {
		assert.NotEqual(t, "persistentvolumeclaims", a.GetResource().Resource, "unexpected %s of a claim", a.GetVerb())
	}
}

// The inline gates: the workspace is on the share (provisioning init
// container, shared-repo shape check) exactly when it has a claim or an
// inline server and export.
func TestNFSWorkspaceOnShare_Inline(t *testing.T) {
	cfg := inlineTestConfig(nfsBaseConfig("g"))
	assert.True(t, nfsInitContainerInjected(cfg))
	cfg.NFSShareExport = ""
	assert.False(t, nfsInitContainerInjected(cfg))
	cfg = inlineTestConfig(nfsBaseConfig("g"))
	cfg.WorkspaceBackendName = ""
	assert.False(t, nfsInitContainerInjected(cfg))
}

// An NFS home with an inline workspace share is refused by the runtime
// too, as at agent start, even when the home has a claim of its own.
func TestBuildPod_InlineWorkspaceHomeOwnClaim(t *testing.T) {
	cfg, _ := nfsClaimGoldenConfig(t)
	cfg = withTestHomeStorage(inlineTestConfig(cfg))
	cfg.HomeStorage.PVClaimName = "scion-workspaces"
	_, err := newTestK8sRuntimeBuild(t, cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shares[0].pv_name")
	assert.Contains(t, err.Error(), "NFS homes do not support yet")
}

func newTestK8sRuntimeBuild(t *testing.T, cfg RunConfig) (*corev1.Pod, error) {
	t.Helper()
	rt, _, _ := newTestK8sRuntime()
	return rt.buildPod("default", cfg)
}

// addInline accepts the same directory under the same name once, and
// reports a name reused for another directory apart from a read-only
// mismatch on the same directory.
func TestNFSPodVolumes_AddInlineConflicts(t *testing.T) {
	vol := func(path string, readOnly bool) corev1.Volume {
		return corev1.Volume{Name: "workspace-git", VolumeSource: corev1.VolumeSource{
			NFS: &corev1.NFSVolumeSource{Server: inlineTestServer, Path: path, ReadOnly: readOnly}}}
	}
	b := &nfsPodVolumes{}
	require.NoError(t, b.addInline(vol("/e/projects/p/a", false)))
	require.NoError(t, b.addInline(vol("/e/projects/p/a", false)))
	assert.Len(t, b.inline, 1)

	err := b.addInline(vol("/e/projects/p/b", false))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "names two directories")
	assert.Contains(t, err.Error(), "/e/projects/p/a")
	assert.Contains(t, err.Error(), "/e/projects/p/b")

	err = b.addInline(vol("/e/projects/p/a", true))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "both read-only and read-write")
	assert.NotContains(t, err.Error(), "two directories")

	other := vol("/e/projects/p/a", false)
	other.NFS.Server = "192.0.2.99"
	err = b.addInline(other)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "names two directories")
	assert.Len(t, b.inline, 1)
}
