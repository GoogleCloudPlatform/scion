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
// directory's server path) strictly under the project's tree; the inline
// pod has no claim volume at all.
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
			assert.Empty(t, im.SubPath, "%s mount %s", cc.Name, cm.MountPath)
			assert.Equal(t, path.Join(inlineTestExport, cm.SubPath), iv.NFS.Path, "%s mount %s server path", cc.Name, cm.MountPath)
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

	// The workspace, the provisioning state dir, the project-config entry
	// and both shared dirs, by server path.
	for name, want := range map[string]struct {
		path     string
		readOnly bool
	}{
		"workspace":           {inlineTestExport + "/projects/proj-123/workspace", false},
		"workspace-provision": {inlineTestExport + "/projects/proj-123/provision", false},
		"workspace-config":    {inlineTestExport + "/projects/proj-123/workspace/.scion", true},
		"shared-dir-0":        {inlineTestExport + "/projects/proj-123/shared-dirs/notes", false},
		"shared-dir-1":        {inlineTestExport + "/projects/proj-123/shared-dirs/cache", true},
	} {
		v := findVolume(inlinePod, name)
		require.NotNil(t, v, name)
		require.NotNil(t, v.NFS, name)
		assert.Equal(t, want.path, v.NFS.Path, name)
		assert.Equal(t, want.readOnly, v.NFS.ReadOnly, name)
	}
	assert.True(t, k8sPodWorkspaceRecoverable(inlinePod), "the inline workspace volume is persistent")
}

func TestBuildPod_InlineWorktree(t *testing.T) {
	claimPod, inlinePod := buildClaimAndInline(t, nfsWorktreeConfig("wt"))
	assertInlineMatchesClaim(t, claimPod, inlinePod, "proj-123")
	for name, p := range map[string]string{
		"workspace-git":              "/projects/proj-123/workspace/.git",
		"workspace-worktree":         "/projects/proj-123/workspace/worktrees/agent-1",
		"workspace-git-hooks":        "/projects/proj-123/workspace/.git/hooks",
		"workspace-git-objects-info": "/projects/proj-123/workspace/.git/objects/info",
		"workspace-git-own-worktree": "/projects/proj-123/workspace/.git/worktrees/agent-1",
	} {
		v := findVolume(inlinePod, name)
		require.NotNil(t, v, name)
		assert.Equal(t, inlineTestExport+p, v.NFS.Path, name)
	}
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

// Inline, a .scion marker file cannot be mounted read-only by an NFS
// volume, so the pod is refused with the fix; a claim still mounts it.
func TestBuildPod_InlineProjectConfigMarkerFileRefused(t *testing.T) {
	cfg, ws := nfsClaimGoldenConfig(t)
	require.NoError(t, os.Remove(filepath.Join(ws, ".scion")))
	require.NoError(t, os.WriteFile(filepath.Join(ws, ".scion"), []byte("marker"), 0o644))

	_, err := newNFSTestK8sRuntime().buildPod("default", claimTestConfig(cfg))
	require.NoError(t, err)

	_, err = newNFSTestK8sRuntime().buildPod("default", inlineTestConfig(cfg))
	require.Error(t, err)
	assert.Contains(t, err.Error(), filepath.Join(ws, ".scion"))
	assert.Contains(t, err.Error(), "pv_name")
	assert.Contains(t, err.Error(), "make .scion a directory")
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

// An NFS home is never shared with an inline workspace volume: it keeps
// its own claim volume.
func TestBuildPod_InlineWorkspaceHomeOwnClaim(t *testing.T) {
	cfg, _ := nfsClaimGoldenConfig(t)
	cfg = withTestHomeStorage(inlineTestConfig(cfg))
	cfg.HomeStorage.PVClaimName = "scion-workspaces"
	pod, err := newTestK8sRuntimeBuild(t, cfg)
	require.NoError(t, err)
	home := findVolume(pod, k8sHomeVolume)
	require.NotNil(t, home)
	require.NotNil(t, home.PersistentVolumeClaim)
	assert.Equal(t, "scion-workspaces", home.PersistentVolumeClaim.ClaimName)
}

func newTestK8sRuntimeBuild(t *testing.T, cfg RunConfig) (*corev1.Pod, error) {
	t.Helper()
	rt, _, _ := newTestK8sRuntime()
	return rt.buildPod("default", cfg)
}
