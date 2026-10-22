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
