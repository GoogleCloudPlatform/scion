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
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
)

// NFSPodShare identifies the NFS share a Kubernetes pod mounts: by claim
// (the share's pv_name), or, when the share has no claim, inline by its
// server and export. It is the one test of "the pod mounts this share"
// that replaces checks on the claim name alone.
type NFSPodShare struct {
	ClaimName string
	Server    string
	Export    string
}

// Mountable reports whether a pod can mount the share: it has a claim, or
// a server and export for inline volumes.
func (s NFSPodShare) Mountable() bool {
	return s.ClaimName != "" || s.Inline()
}

// Inline reports whether a pod mounts the share inline: no claim, and a
// server and export.
func (s NFSPodShare) Inline() bool {
	return s.ClaimName == "" && s.Server != "" && s.Export != ""
}

// Same reports whether s and o are the same mountable share: the same
// claim, or both inline on the same server and export.
func (s NFSPodShare) Same(o NFSPodShare) bool {
	if !s.Mountable() || !o.Mountable() {
		return false
	}
	if s.ClaimName != "" || o.ClaimName != "" {
		return s.ClaimName == o.ClaimName
	}
	return s.Server == o.Server && s.Export == o.Export
}

// workspaceNFSShare is the share of an NFS workspace start.
func workspaceNFSShare(config RunConfig) NFSPodShare {
	return NFSPodShare{ClaimName: config.NFSPVClaimName, Server: config.NFSShareServer, Export: config.NFSShareExport}
}

// nfsWorkspaceOnShare reports whether the pod's workspace is on the NFS
// share (by claim or inline), rather than an emptyDir.
func nfsWorkspaceOnShare(config RunConfig) bool {
	return config.WorkspaceBackendName == "nfs" && workspaceNFSShare(config).Mountable()
}

// sharedDirNFSShare is the share of a start's shared dirs, or the zero
// share when they are not on a shared backend.
func sharedDirNFSShare(r *SharedDirRealization) NFSPodShare {
	if !r.Shared() {
		return NFSPodShare{}
	}
	return NFSPodShare{ClaimName: r.PVClaimName, Server: r.Server, Export: r.Export}
}

// podVolumeSource turns a directory realized for a pod
// (projectfs.Layout.Realize with RuntimePod) into the pod volume's source:
// the claim, or the inline NFS volume of the directory's server path. It is
// the only place a Kubernetes NFS share volume source is built.
func podVolumeSource(r projectfs.Realized) (corev1.VolumeSource, error) {
	switch {
	case r.ClaimName != "" && r.NFS == nil:
		return corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: r.ClaimName, ReadOnly: r.ReadOnly}}, nil
	case r.ClaimName == "" && r.NFS != nil:
		if r.NFS.Server == "" || r.NFS.Path == "" || r.SubPath != "" {
			return corev1.VolumeSource{}, fmt.Errorf("inline NFS volume for %q needs a server and a path and no subPath", r.Target)
		}
		return corev1.VolumeSource{NFS: &corev1.NFSVolumeSource{Server: r.NFS.Server, Path: r.NFS.Path, ReadOnly: r.ReadOnly}}, nil
	default:
		return corev1.VolumeSource{}, fmt.Errorf("pod mount of %q needs exactly one of a claim and an inline NFS volume", r.Target)
	}
}

// nfsShareLayout is the projectfs Layout of a pod's NFS share, built from
// the start's configuration.
func nfsShareLayout(backend projectfs.Backend, subPathRoot string, share NFSPodShare) projectfs.Layout {
	return projectfs.Layout{
		Backend:     backend,
		SubPathRoot: subPathRoot,
		Share:       projectfs.Share{Server: share.Server, Export: share.Export, PVName: share.ClaimName},
	}
}

// realizeNFSLeaf realizes the directory at subPath of a pod's share for a
// mount at target. Inline, the directory is resolved through
// projectfs.Layout.ProjectLeaf, so it must sit strictly under the
// project's tree (each inline volume names its own server path). With a
// claim, the subPath is mounted from the claim as before.
func realizeNFSLeaf(layout projectfs.Layout, projectID, subPath, target string, readOnly bool) (projectfs.Realized, error) {
	var r projectfs.Resolved
	if layout.Share.PVName == "" {
		var err error
		if r, err = layout.ProjectLeaf(projectID, subPath, target, readOnly); err != nil {
			return projectfs.Realized{}, fmt.Errorf("inline NFS volume for %s: %w", target, err)
		}
	} else {
		r = projectfs.Resolved{Backend: layout.Backend, SubPath: subPath, Target: target, ReadOnly: readOnly}
	}
	return layout.Realize(r, projectfs.RuntimePod)
}

// nfsPodVolumes places the directories of the workspace share in a pod.
// With a claim, every directory is a subPath mount of the one "workspace"
// claim volume. Inline, each directory is its own NFS volume of its server
// path, mounted with no subPath, so no container ever gets the export or
// the project directory as a whole.
type nfsPodVolumes struct {
	layout    projectfs.Layout
	projectID string
	// inline are the inline volumes, in the order first used.
	inline []corev1.Volume
}

// newWorkspaceNFSPodVolumes returns the placer for an NFS workspace start.
func newWorkspaceNFSPodVolumes(config RunConfig) *nfsPodVolumes {
	return &nfsPodVolumes{
		layout:    nfsShareLayout(projectfs.BackendNFS, config.NFSSubPathRoot, workspaceNFSShare(config)),
		projectID: config.ProjectID,
	}
}

// claimVolume returns the "workspace" claim volume (claim mode only).
func (b *nfsPodVolumes) claimVolume() (corev1.Volume, error) {
	if b.layout.Share.PVName == "" {
		return corev1.Volume{}, errors.New("the NFS workspace share has no claim")
	}
	// A whole-claim volume is the deliberate exception to
	// projectfs.Layout.Realize, which models subPath mounts only; its
	// mounts are realized per directory in mount.
	src, err := podVolumeSource(projectfs.Realized{ClaimName: b.layout.Share.PVName, Target: "/workspace"})
	if err != nil {
		return corev1.Volume{}, err
	}
	return corev1.Volume{Name: "workspace", VolumeSource: src}, nil
}

// mount returns the mount of the directory at subPath at mountPath. Inline,
// it adds the volume inlineName for it (the same name must always name the
// same directory).
func (b *nfsPodVolumes) mount(inlineName, subPath, mountPath string, readOnly bool) (corev1.VolumeMount, error) {
	if b.layout.Share.PVName != "" && subPath == "" {
		// An empty workspace subPath mounts the claim's root, as before.
		return corev1.VolumeMount{Name: "workspace", MountPath: mountPath, ReadOnly: readOnly}, nil
	}
	realized, err := realizeNFSLeaf(b.layout, b.projectID, subPath, mountPath, readOnly)
	if err != nil {
		return corev1.VolumeMount{}, err
	}
	if realized.ClaimName != "" {
		return corev1.VolumeMount{Name: "workspace", MountPath: mountPath, SubPath: realized.SubPath, ReadOnly: readOnly}, nil
	}
	src, err := podVolumeSource(realized)
	if err != nil {
		return corev1.VolumeMount{}, err
	}
	if err := b.addInline(corev1.Volume{Name: inlineName, VolumeSource: src}); err != nil {
		return corev1.VolumeMount{}, err
	}
	return corev1.VolumeMount{Name: inlineName, MountPath: mountPath, ReadOnly: readOnly}, nil
}

// mountWithin returns the mount of the directory or file at subPath at
// mountPath, an entry inside the directory of the inline volume parentName
// (already added by mount). Inline, the entry is mounted from that volume
// by its subPath relative to the volume's directory, not as its own
// volume: entries inside the workspace are mounted from the workspace
// volume so the node resolves them within it. The entry is still realized
// through the layout first, so it must sit strictly under the project's
// tree. With a claim, it is mount's subPath mount of the claim.
func (b *nfsPodVolumes) mountWithin(parentName, subPath, mountPath string, readOnly bool) (corev1.VolumeMount, error) {
	if b.layout.Share.PVName != "" {
		return b.mount(parentName, subPath, mountPath, readOnly)
	}
	realized, err := realizeNFSLeaf(b.layout, b.projectID, subPath, mountPath, readOnly)
	if err != nil {
		return corev1.VolumeMount{}, err
	}
	if realized.NFS == nil {
		return corev1.VolumeMount{}, fmt.Errorf("inline NFS mount of %q has no server path", mountPath)
	}
	for _, parent := range b.inline {
		if parent.Name != parentName {
			continue
		}
		rel, ok := strings.CutPrefix(realized.NFS.Path, strings.TrimSuffix(parent.NFS.Path, "/")+"/")
		if parent.NFS.Server != realized.NFS.Server || !ok || rel == "" {
			return corev1.VolumeMount{}, fmt.Errorf("inline NFS mount of %q (%s) is not inside volume %q (%s)",
				mountPath, realized.NFS.Path, parentName, parent.NFS.Path)
		}
		return corev1.VolumeMount{Name: parentName, MountPath: mountPath, SubPath: rel, ReadOnly: readOnly}, nil
	}
	return corev1.VolumeMount{}, fmt.Errorf("inline NFS mount of %q needs the volume %q of its parent directory", mountPath, parentName)
}

// addInline adds v unless a volume of that name is already there, which
// must then be the same.
func (b *nfsPodVolumes) addInline(v corev1.Volume) error {
	for _, have := range b.inline {
		if have.Name != v.Name {
			continue
		}
		if have.NFS.Server != v.NFS.Server || have.NFS.Path != v.NFS.Path {
			return fmt.Errorf("inline NFS volume %q names two directories (%s:%s and %s:%s)",
				v.Name, have.NFS.Server, have.NFS.Path, v.NFS.Server, v.NFS.Path)
		}
		if have.NFS.ReadOnly != v.NFS.ReadOnly {
			return fmt.Errorf("inline NFS volume %q of %s is mounted both read-only and read-write", v.Name, v.NFS.Path)
		}
		return nil
	}
	b.inline = append(b.inline, v)
	return nil
}

// Inline volume names of the workspace share's directories.
const (
	nfsVolWorkspace      = "workspace"
	nfsVolAgentDir       = "workspace-agent-dir"
	nfsVolGit            = "workspace-git"
	nfsVolWorktree       = "workspace-worktree"
	nfsVolProvisionState = "workspace-provision"
)

// sharedDirPodVolume returns the volume of shared dir sd (index i) on a
// shared backend and the subPath its mounts use: the claim volume by the
// dir's subPath, or the dir's own inline NFS volume (empty subPath).
// sharedDirClaimSubPath checks the dir's subPath first, as before.
func sharedDirPodVolume(r *SharedDirRealization, i int, name string, readOnly bool, target string) (corev1.Volume, string, error) {
	_, subPath, err := sharedDirClaimSubPath(r, name)
	if err != nil {
		return corev1.Volume{}, "", err
	}
	layout := nfsShareLayout(projectfs.Backend(r.Backend), r.SubPathRoot, sharedDirNFSShare(r))
	realized, err := realizeNFSLeaf(layout, r.ProjectID, subPath, target, readOnly)
	if err != nil {
		return corev1.Volume{}, "", fmt.Errorf("shared dir %q: %w", name, err)
	}
	src, err := podVolumeSource(realized)
	if err != nil {
		return corev1.Volume{}, "", fmt.Errorf("shared dir %q: %w", name, err)
	}
	return corev1.Volume{Name: fmt.Sprintf("shared-dir-%d", i), VolumeSource: src}, realized.SubPath, nil
}
