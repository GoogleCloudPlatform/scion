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

// LoadHubSharedDirStorage reads server.shared_dir_storage the way the
// brokers' agent-start path does: from the global settings file only,
// through LoadGlobalSettings (no SCION_ environment provider, no DB overlay,
// never a project's settings.yaml), so the hub and its brokers resolve shared
// dirs from the same source. The hub calls it once at startup.
//
// It returns (nil, nil) when the block is genuinely unset. It fails closed
// with an error when the file plausibly configures the block but cannot be
// used: it fails to load and mentions shared_dir_storage, or it loaded as the
// legacy format and mentions the key without a block being loaded.
func LoadHubSharedDirStorage() (*V1SharedDirStorageConfig, error) {
	gs, _, err := LoadGlobalSettings()
	if err != nil {
		if GlobalSettingsMentions("shared_dir_storage") {
			return nil, fmt.Errorf("server.shared_dir_storage: global settings unreadable: %w", err)
		}
		return nil, nil
	}
	if gs == nil || gs.Server == nil || gs.Server.SharedDirStorage == nil {
		if GlobalSettingsIsLegacyFormat() && GlobalSettingsMentions("shared_dir_storage") {
			return nil, errors.New(
				"server.shared_dir_storage: global settings mention it but it was not loaded (missing schema_version \"1\"?)")
		}
		return nil, nil
	}
	return gs.Server.SharedDirStorage, nil
}
