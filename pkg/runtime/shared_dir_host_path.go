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
	"io/fs"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
)

// ErrSharedDirStorageUnavailable is wrapped by every error
// ResolveSharedDirHostPath returns once the shared dir is known to use the
// nfs backend (or its settings cannot be read), so callers can tell an
// unusable nfs configuration apart from a bad request. The caller must not
// fall back to the local layout: no agent mounts it for an nfs-backed dir.
var ErrSharedDirStorageUnavailable = errors.New("nfs shared-dir storage is unavailable")

// SharedDirHostPath is the host-side directory backing one shared dir, as
// resolved by ResolveSharedDirHostPath.
type SharedDirHostPath struct {
	// Path is the host directory agents on this machine mount for the
	// shared dir.
	Path string
	// Backend is "local" or "nfs".
	Backend string
}

// ResolveSharedDirHostPath resolves the host directory that agents on this
// machine mount for the project's shared dir name, for host processes
// (chat plugins) that translate /scion-volumes/<name> paths without an
// agent or project directory at hand.
//
// The backend is chosen from gs, which must be global settings only, with
// VersionedSettings.ResolveSharedDirStorageBackend for the active profile.
// For the local backend the result is config.SharedDirHostPath. For the
// nfs backend the path comes from the same NFS backend Resolve and
// confinement check agent start uses (pkg/agent resolveSharedDirs), and
// the leaf is created, when missing, with the same shareddirs.EnsureLeaf
// walk and hardening. A missing or unreadable nfs host base, an
// incomplete nfs block, or a leaf reached through a symlink returns an
// error wrapping ErrSharedDirStorageUnavailable.
func ResolveSharedDirHostPath(gs *config.VersionedSettings, home, slug, projectID, name string) (SharedDirHostPath, error) {
	if err := api.ValidateSharedDirs([]api.SharedDir{{Name: name}}); err != nil {
		return SharedDirHostPath{}, err
	}

	backend, source, _ := gs.ResolveSharedDirStorageBackend("", name)
	switch backend {
	case "", "local":
		return SharedDirHostPath{
			Path:    config.SharedDirHostPath(home, slug, projectID, name),
			Backend: "local",
		}, nil
	case "nfs":
	default:
		return SharedDirHostPath{}, fmt.Errorf("%w: %s: unknown shared-dir storage backend %q",
			ErrSharedDirStorageUnavailable, source, backend)
	}

	sdCfg := &config.V1SharedDirStorageConfig{Backend: "nfs"}
	if gs.Server != nil && gs.Server.SharedDirStorage != nil {
		sdCfg.NFS = gs.Server.SharedDirStorage.NFS
	}
	if err := sdCfg.Validate(); err != nil {
		return SharedDirHostPath{}, fmt.Errorf("%w: %s: %v", ErrSharedDirStorageUnavailable, source, err)
	}
	if !shareddirs.ValidProjectID(projectID) {
		return SharedDirHostPath{}, fmt.Errorf("%w: invalid hub project ID %q", ErrSharedDirStorageUnavailable, projectID)
	}

	res, err := NewNFSBackend(sdCfg.NFS).Resolve(ResolveInput{
		ProjectID:      projectID,
		SharedDirNames: []string{name},
	})
	if err != nil {
		return SharedDirHostPath{}, fmt.Errorf("%w: resolve: %v", ErrSharedDirStorageUnavailable, err)
	}
	sd, ok := res.SharedDirs[name]
	if !ok {
		return SharedDirHostPath{}, fmt.Errorf("%w: shared dir %q not found in NFS resolution", ErrSharedDirStorageUnavailable, name)
	}
	subPathRoot := config.SubPathRootOrDefault(sdCfg.NFS.SubPathRoot)
	if err := shareddirs.ConfineLeaf(sd.HostPath, res.HostBase, subPathRoot, projectID, name); err != nil {
		return SharedDirHostPath{}, fmt.Errorf("%w: %v", ErrSharedDirStorageUnavailable, err)
	}

	// Never create the host base itself: a missing base means the export
	// is not mounted here, and writing below it would land on local disk.
	info, statErr := os.Stat(res.HostBase)
	switch {
	case errors.Is(statErr, fs.ErrNotExist):
		return SharedDirHostPath{}, fmt.Errorf("%w: the nfs export is not mounted at %q",
			ErrSharedDirStorageUnavailable, res.HostBase)
	case statErr != nil:
		return SharedDirHostPath{}, fmt.Errorf("%w: check host base %q: %v",
			ErrSharedDirStorageUnavailable, res.HostBase, statErr)
	case !info.IsDir():
		return SharedDirHostPath{}, fmt.Errorf("%w: host base %q is not a directory",
			ErrSharedDirStorageUnavailable, res.HostBase)
	}

	resolvedHostBase, err := filepath.EvalSymlinks(res.HostBase)
	if err != nil {
		return SharedDirHostPath{}, fmt.Errorf("%w: resolve host base symlinks: %v", ErrSharedDirStorageUnavailable, err)
	}
	leafFd, _, err := shareddirs.EnsureLeaf(resolvedHostBase, sd.ServerRelativePath)
	if err != nil {
		return SharedDirHostPath{}, fmt.Errorf("%w: shared dir %q: %v", ErrSharedDirStorageUnavailable, name, err)
	}
	_ = shareddirs.CloseFd(leafFd)

	want := filepath.Join(resolvedHostBase, sd.ServerRelativePath)
	got, err := filepath.EvalSymlinks(sd.HostPath)
	if err != nil {
		return SharedDirHostPath{}, fmt.Errorf("%w: resolve shared dir %q: %v", ErrSharedDirStorageUnavailable, name, err)
	}
	if got != want {
		return SharedDirHostPath{}, fmt.Errorf("%w: shared dir %q resolves through a symlink to %q, want %q",
			ErrSharedDirStorageUnavailable, name, got, want)
	}
	return SharedDirHostPath{Path: got, Backend: "nfs"}, nil
}

// LoadSharedDirStorageSettings loads the global settings
// ResolveSharedDirHostPath reads, failing closed the way agent start does
// (pkg/agent Start): a global settings file that cannot be loaded, or a
// legacy-format file whose server block was dropped, is an error wrapping
// ErrSharedDirStorageUnavailable only when it mentions shared_dir_storage.
// Otherwise unreadable settings mean the local layout (nil settings).
func LoadSharedDirStorageSettings() (*config.VersionedSettings, error) {
	gs, _, err := config.LoadGlobalSettingsWithOverlay()
	if err != nil {
		if config.GlobalSettingsMentions("shared_dir_storage") {
			return nil, fmt.Errorf("%w: loading global settings for server.shared_dir_storage: %v",
				ErrSharedDirStorageUnavailable, err)
		}
		return nil, nil
	}
	if gs != nil && (gs.Server == nil || gs.Server.SharedDirStorage == nil) &&
		config.GlobalSettingsIsLegacyFormat() && config.GlobalSettingsMentions("shared_dir_storage") {
		return nil, fmt.Errorf(
			"%w: global settings mention server.shared_dir_storage but it was not loaded (missing schema_version: \"1\"?)",
			ErrSharedDirStorageUnavailable)
	}
	return gs, nil
}

// ResolveSharedDirHostPathFromGlobal is LoadSharedDirStorageSettings
// followed by ResolveSharedDirHostPath.
func ResolveSharedDirHostPathFromGlobal(home, slug, projectID, name string) (SharedDirHostPath, error) {
	gs, err := LoadSharedDirStorageSettings()
	if err != nil {
		return SharedDirHostPath{}, err
	}
	return ResolveSharedDirHostPath(gs, home, slug, projectID, name)
}
