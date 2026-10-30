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
	"log/slog"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
)

// ErrSharedDirStorageUnavailable is wrapped by every error
// ResolveSharedDirHostPath returns once the shared dir is known to use a
// shared backend (or its settings cannot be read), so callers can tell an
// unusable shared-dir storage apart from a bad request. The caller must not
// fall back to the local layout: no agent mounts it for a shared-backend dir.
var ErrSharedDirStorageUnavailable = errors.New("nfs shared-dir storage is unavailable")

// ensureSharedDirLeaf is shareddirs.EnsureLeafOwned, replaceable in tests
// to replace a path component after the walk and reach the backstop check.
var ensureSharedDirLeaf = shareddirs.EnsureLeafOwned

// SharedDirHostPath is the host-side directory backing one shared dir, as
// resolved by ResolveSharedDirHostPath.
type SharedDirHostPath struct {
	// Path is the host directory agents on this machine mount for the
	// shared dir.
	Path string
	// Backend is "local", or the shared backend of the shared-dir Layout
	// ("nfs", "gke-shared-volume" or "cloudrun-volume").
	Backend string
	// Resolved is the layout authority's answer for the same directory
	// (pkg/projectfs). Host processes write below Path only through
	// Resolved.Open, never with path-based os calls: agents mount the dir
	// read-write, so a component below it may be a planted symlink.
	Resolved projectfs.Resolved
}

// ResolveSharedDirHostPath resolves the host directory that agents on this
// machine mount for the project's shared dir name, for host processes
// (chat plugins) that translate /scion-volumes/<name> paths without an
// agent or project directory at hand.
//
// The placement comes from gs, which must be global settings only, through
// the layout authority: config.SharedDirStorageLayout (server.project_storage,
// or the block synthesized from the deprecated storage blocks, then
// projectfs.Layout.SharedDirLayout), exactly as agent start resolves it
// (pkg/agent resolveSharedDirs). There are no per-profile, per-runtime or
// per-dir rungs. For the local backend the result is
// config.SharedDirHostPath. For a shared backend the path comes from
// Layout.Resolve (name and project ID validation, ConfineLeaf), and the
// leaf is created, when missing, with the same shareddirs.EnsureLeaf walk
// and hardening. A settings error, a missing or unreadable host base, or a
// leaf reached through a symlink returns an error wrapping
// ErrSharedDirStorageUnavailable.
//
// Keep this chain in lockstep with pkg/agent resolveSharedDirs: both apply
// the same path checks, in the same order (name and project ID
// validation, Resolve with ConfineLeaf, ValidateNotExportRoot, host-base
// stat refusal, EvalSymlinks of the host base, EnsureLeaf, then the
// resolved-path backstop), except that agent start on Kubernetes may leave
// an unmounted or permission-refused nfs workspace-export leaf to the node
// (see resolveSharedDirs); this chain always refuses. A change to either
// chain must be made to both; TestSharedDirChainsParity in pkg/agent runs
// one table of refusals through both.
//
// Limits, compared with agent start (which this does not replace): plugins
// run out of process, so they never see the hub settings DB overlay; only
// the global settings file is read. The storage blocks are Layer-0 (never
// in the overlay), so both resolve the same tree.
func ResolveSharedDirHostPath(gs *config.VersionedSettings, home, slug, projectID, name string) (SharedDirHostPath, error) {
	if err := api.ValidateSharedDirs([]api.SharedDir{{Name: name}}); err != nil {
		return SharedDirHostPath{}, err
	}

	var srv *config.V1ServerConfig
	if gs != nil {
		srv = gs.Server
	}
	layout, _, err := config.SharedDirStorageLayout(srv)
	if err != nil {
		return SharedDirHostPath{}, fmt.Errorf("%w: %v", ErrSharedDirStorageUnavailable, err)
	}
	if !layout.Backend.Shared() {
		mount := projectfs.SharedDirMount(api.SharedDir{Name: name}, "")
		path := config.SharedDirHostPath(home, slug, projectID, name)
		return SharedDirHostPath{
			Path:    path,
			Backend: "local",
			Resolved: projectfs.Resolved{
				Backend: projectfs.BackendLocal, HostPath: path,
				Target: mount.Target, Kind: mount.Kind, Name: mount.Name,
			},
		}, nil
	}
	backend := string(layout.Backend)

	if !shareddirs.ValidProjectID(projectID) {
		return SharedDirHostPath{}, fmt.Errorf("%w: invalid hub project ID %q", ErrSharedDirStorageUnavailable, projectID)
	}
	res, err := layout.Resolve(projectfs.Project{ID: projectID}, "", projectfs.SharedDirMount(api.SharedDir{Name: name}, ""))
	if err != nil {
		return SharedDirHostPath{}, fmt.Errorf("%w: resolve: %v", ErrSharedDirStorageUnavailable, err)
	}
	if err := ValidateNotExportRoot(res.HostPath, res.HostBase); err != nil {
		return SharedDirHostPath{}, fmt.Errorf("%w: %v", ErrSharedDirStorageUnavailable, err)
	}

	// Never create the host base itself: a missing base means the share
	// is not mounted here, and writing below it would land on local disk.
	info, statErr := os.Stat(res.HostBase)
	switch {
	case errors.Is(statErr, fs.ErrNotExist):
		return SharedDirHostPath{}, fmt.Errorf("%w: the %s share is not mounted at %q",
			ErrSharedDirStorageUnavailable, backend, res.HostBase)
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
	leafFd, _, err := ensureSharedDirLeaf(resolvedHostBase, res.RelPath, res.Owner())
	if err != nil {
		return SharedDirHostPath{}, fmt.Errorf("%w: shared dir %q: %v", ErrSharedDirStorageUnavailable, name, err)
	}
	_ = shareddirs.CloseFd(leafFd)

	// Backstop, as on the agent side: the walk above works on descriptors
	// opened at walk time, so re-resolve the path fresh and require it to
	// be the expected clean path, in case a component was replaced after
	// the walk.
	want := filepath.Join(resolvedHostBase, res.RelPath)
	got, err := filepath.EvalSymlinks(res.HostPath)
	if err != nil {
		return SharedDirHostPath{}, fmt.Errorf("%w: resolve shared dir %q: %v", ErrSharedDirStorageUnavailable, name, err)
	}
	if got != want {
		return SharedDirHostPath{}, fmt.Errorf("%w: shared dir %q resolves through a symlink to %q, want %q",
			ErrSharedDirStorageUnavailable, name, got, want)
	}
	// res is the layout authority's answer itself (the shared-dir Layout
	// resolved it above), so there is no second derivation to cross-check.
	return SharedDirHostPath{Path: got, Backend: backend, Resolved: res}, nil
}

// LoadSharedDirStorageSettings loads the global settings
// ResolveSharedDirHostPath reads, failing closed the way agent start does
// (pkg/agent Start): a global settings file that cannot be loaded, or a
// legacy-format file from which no storage block loaded (its server block
// is kept, ptone/scion#3919, so only a misplaced or commented-out block),
// is an error wrapping ErrSharedDirStorageUnavailable only when it
// mentions a key that places shared dirs (config.SharedDirPlacementKeys).
// Otherwise unreadable settings mean the local layout (nil settings).
func LoadSharedDirStorageSettings() (*config.VersionedSettings, error) {
	gs, _, err := config.LoadGlobalSettingsWithOverlay()
	if err != nil {
		if key := config.MentionedSharedDirPlacementKey(config.GlobalSettingsMentions); key != "" {
			return nil, fmt.Errorf("%w: loading global settings for server.%s: %v",
				ErrSharedDirStorageUnavailable, key, err)
		}
		slog.Warn("Failed to load global settings; no shared-dir storage key was found in the raw file, using the local shared-dir layout",
			"error", err)
		return nil, nil
	}
	if gs != nil && !gs.Server.HasSharedDirPlacementBlock() && config.GlobalSettingsIsLegacyFormat() {
		if key := config.GlobalSettingsStorageKey(config.SharedDirPlacementKeys); key != "" {
			return nil, fmt.Errorf(
				"%w: global settings mention server.%s but it was not loaded (missing schema_version: \"1\"?)",
				ErrSharedDirStorageUnavailable, key)
		}
	}
	return gs, nil
}
