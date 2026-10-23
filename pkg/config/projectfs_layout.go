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

package config

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
)

// NFSLayout translates an nfs block (server.workspace_storage.nfs or
// server.shared_dir_storage.nfs) into the layout authority's Layout: the
// first share, mounted on this host at <mount_root>/<share id>, with the
// validated subpath_root. It is the one place an nfs block becomes a
// projectfs.Layout, so every reader of either block resolves the same tree.
func NFSLayout(nfs *V1NFSConfig) (projectfs.Layout, error) {
	if nfs == nil {
		return projectfs.Layout{}, errors.New("NFS config is nil")
	}
	if len(nfs.Shares) == 0 {
		return projectfs.Layout{}, errors.New("no NFS shares configured")
	}
	subPathRoot, err := ResolveSubPathRoot(nfs.SubPathRoot)
	if err != nil {
		return projectfs.Layout{}, err
	}
	share := nfs.Shares[0]
	return projectfs.Layout{
		Backend:     projectfs.BackendNFS,
		HostBase:    filepath.Join(nfs.MountRoot, share.ID),
		SubPathRoot: subPathRoot,
		Share: projectfs.Share{
			ID:     share.ID,
			Server: share.Server,
			Export: share.Export,
			PVName: share.PVName,
		},
		UID: nfs.UID,
		GID: nfs.GID,
	}, nil
}

// DefaultVolumeMountBase is where the cloudrun-volume and
// gke-shared-volume backends are mounted: <base>/<volume_name>. Neither
// volume block has a mount path field; the volume name identifies the mount
// point.
const DefaultVolumeMountBase = "/mnt"

// ProjectStorageLayout translates server.project_storage into the layout
// authority's Layout: the local Layout for the local backend (and a nil
// block), NFSLayout for nfs, and for the volume backends the volume mounted
// at <volumeBase>/<volume_name> with the validated subpath_root. Homes is
// the block's homes value (local when unset). It is the one place the block
// becomes a projectfs.Layout, for the hub and the brokers alike. Call it on
// a block that passed Validate.
func ProjectStorageLayout(ps *V1ProjectStorageConfig, volumeBase string) (projectfs.Layout, error) {
	homes := projectfs.BackendLocal
	if ps != nil && ps.Homes == HomeStorageBackendNFS {
		homes = projectfs.BackendNFS
	}
	var layout projectfs.Layout
	switch ps.EffectiveBackend() {
	case WorkspaceStorageBackendLocal:
		layout = projectfs.Layout{Backend: projectfs.BackendLocal}
	case WorkspaceStorageBackendNFS:
		l, err := NFSLayout(ps.NFS)
		if err != nil {
			return projectfs.Layout{}, fmt.Errorf("%s.nfs: %w", ProjectStorageKey, err)
		}
		layout = l
	case WorkspaceStorageBackendCloudRunVolume:
		v := ps.CloudRunVolume
		if v == nil || v.VolumeName == "" {
			return projectfs.Layout{}, fmt.Errorf("%s.cloudrun_volume.volume_name is not set", ProjectStorageKey)
		}
		l, err := volumeLayout(projectfs.BackendCloudRunVolume, volumeBase, v.VolumeName, v.SubPathRoot, "")
		if err != nil {
			return projectfs.Layout{}, fmt.Errorf("%s.cloudrun_volume: %w", ProjectStorageKey, err)
		}
		layout = l
	case WorkspaceStorageBackendGKESharedVolume:
		v := ps.GKESharedVolume
		if v == nil || v.VolumeName == "" {
			return projectfs.Layout{}, fmt.Errorf("%s.gke_shared_volume.volume_name is not set", ProjectStorageKey)
		}
		l, err := volumeLayout(projectfs.BackendGKESharedVolume, volumeBase, v.VolumeName, v.SubPathRoot, v.PVClaimName)
		if err != nil {
			return projectfs.Layout{}, fmt.Errorf("%s.gke_shared_volume: %w", ProjectStorageKey, err)
		}
		layout = l
	default:
		return projectfs.Layout{}, fmt.Errorf("%s.backend %q is not supported", ProjectStorageKey, ps.Backend)
	}
	layout.Homes = homes
	if ps != nil && ps.LegacySharedDirsLocal {
		layout.SharedDirs = &projectfs.Layout{Backend: projectfs.BackendLocal, Homes: homes}
	}
	if ps != nil && ps.LegacySharedDirsNFS != nil {
		sdLayout, err := NFSLayout(ps.LegacySharedDirsNFS)
		if err != nil {
			return projectfs.Layout{}, fmt.Errorf("%s.nfs: %w", sharedDirStorageKey, err)
		}
		sdLayout.Homes = homes
		layout.SharedDirs = &sdLayout
	}
	return layout, nil
}

func volumeLayout(backend projectfs.Backend, volumeBase, volumeName, subPathRoot, claim string) (projectfs.Layout, error) {
	if volumeBase == "" {
		volumeBase = DefaultVolumeMountBase
	}
	if filepath.Base(volumeName) != volumeName || volumeName == "." || volumeName == ".." {
		return projectfs.Layout{}, fmt.Errorf("volume_name %q must be a single path component", volumeName)
	}
	root, err := ResolveSubPathRoot(subPathRoot)
	if err != nil {
		return projectfs.Layout{}, err
	}
	return projectfs.Layout{
		Backend:     backend,
		HostBase:    filepath.Join(volumeBase, volumeName),
		SubPathRoot: root,
		Share:       projectfs.Share{ID: volumeName, PVName: claim},
	}, nil
}

// ValidateHubProjectStorageLayout checks that ps builds a Layout a hub can
// serve files from: ProjectStorageLayout succeeds and every shared host
// base (the backend's, and the transitional shared-dir split's) is an
// absolute path on this host.
func ValidateHubProjectStorageLayout(ps *V1ProjectStorageConfig) error {
	layout, err := ProjectStorageLayout(ps, DefaultVolumeMountBase)
	if err != nil {
		return err
	}
	for _, l := range []projectfs.Layout{layout, layout.SharedDirLayout()} {
		if l.Backend.Shared() && !filepath.IsAbs(l.HostBase) {
			return fmt.Errorf("the %s share must be mounted at an absolute path on the hub (mount_root gives host base %q)", l.Backend, l.HostBase)
		}
	}
	return nil
}
