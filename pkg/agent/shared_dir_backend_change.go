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

package agent

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// An explicit shared-dir backend change moves the recorded backend of named
// shared dirs from local to nfs, as part of a reincarnation. Only the
// agent's record changes: the data is copied by the operator, and the local
// directory is never moved or deleted. The next start refuses an empty nfs
// directory while the previous local directory is not empty, unless the
// change was made with AllowEmptySharedDir.

// allowEmptySharedDirFlag and sharedDirBackendFlag name the reincarnate
// flags in user-facing errors.
const (
	sharedDirBackendFlag    = "--shared-dir-backend"
	allowEmptySharedDirFlag = "--allow-empty-shared-dir"
)

// agentSharedDirs returns the agent's shared dirs: the project
// settings' shared_dirs when set, else the dirs the hub dispatched.
func agentSharedDirs(settings *config.VersionedSettings, opts api.StartOptions) []api.SharedDir {
	if settings != nil && len(settings.SharedDirs) > 0 {
		return settings.SharedDirs
	}
	return opts.SharedDirs
}

// validateSharedDirBackendChanges checks a requested change before anything
// is provisioned: every name must be one of the agent's shared dirs, the
// only supported target backend is nfs, and server.shared_dir_storage.nfs
// must be complete in gs (global settings). allowEmpty without a change is
// an error.
func validateSharedDirBackendChanges(changes map[string]string, allowEmpty bool, dirs []api.SharedDir, gs *config.VersionedSettings) error {
	if len(changes) == 0 {
		if allowEmpty {
			return fmt.Errorf("%s needs a shared dir backend change (%s NAME=nfs)", allowEmptySharedDirFlag, sharedDirBackendFlag)
		}
		return nil
	}
	known := make(map[string]bool, len(dirs))
	names := make([]string, 0, len(dirs))
	for _, d := range dirs {
		known[d.Name] = true
		names = append(names, d.Name)
	}
	for _, name := range slices.Sorted(maps.Keys(changes)) {
		if err := api.ValidateSharedDirs([]api.SharedDir{{Name: name}}); err != nil {
			return fmt.Errorf("invalid shared dir name %q", name)
		}
		if backend := changes[name]; backend != "nfs" {
			return fmt.Errorf("shared dir %q: only a change to the nfs backend is supported (got %q)", name, backend)
		}
		if !known[name] {
			if len(names) == 0 {
				return fmt.Errorf("shared dir %q is not one of the agent's shared dirs (it has none)", name)
			}
			return fmt.Errorf("shared dir %q is not one of the agent's shared dirs (%s)", name, strings.Join(names, ", "))
		}
	}
	nfs := &config.V1SharedDirStorageConfig{Backend: "nfs"}
	if gs != nil && gs.Server != nil && gs.Server.SharedDirStorage != nil {
		nfs.NFS = gs.Server.SharedDirStorage.NFS
	}
	if err := nfs.Validate(); err != nil {
		return fmt.Errorf("a change to the nfs backend needs a complete server.shared_dir_storage.nfs block on this broker: %w", err)
	}
	return nil
}

// initialSharedDirStorageRecord returns the record an agent without one
// would write at its next start: the default backend and per-dir backends
// chosen from gs for profile, as at a first start.
func initialSharedDirStorageRecord(gs *config.VersionedSettings, profile string, dirs []api.SharedDir, agentName string) (*sharedDirStorageRecord, error) {
	def, err := selectSharedDirStorage(gs, profile, "", agentName)
	if err != nil {
		return nil, err
	}
	overrides, err := selectSharedDirBackends(gs, profile, nil, def, dirs, agentName)
	if err != nil {
		return nil, err
	}
	return newSharedDirStorageRecord(def, overrides), nil
}

// changeSharedDirBackends returns a copy of rec with each dir in changes
// recorded on the nfs backend. A dir that moves from local gets a previous
// entry, so the next start checks its directories, unless allowEmpty is
// set; allowEmpty also drops any previous entry for the named dirs. A dir
// already on nfs is otherwise left as it is.
func changeSharedDirBackends(rec *sharedDirStorageRecord, changes map[string]string, allowEmpty bool) *sharedDirStorageRecord {
	out := &sharedDirStorageRecord{
		Backend:  rec.Backend,
		Dirs:     maps.Clone(rec.Dirs),
		Previous: maps.Clone(rec.Previous),
	}
	for _, name := range slices.Sorted(maps.Keys(changes)) {
		current := out.backendFor(name)
		if current != "nfs" {
			if out.Backend == "nfs" {
				delete(out.Dirs, name)
			} else {
				if out.Dirs == nil {
					out.Dirs = make(map[string]string)
				}
				out.Dirs[name] = "nfs"
			}
			if !allowEmpty {
				if out.Previous == nil {
					out.Previous = make(map[string]string)
				}
				out.Previous[name] = current
			}
		}
		if allowEmpty {
			delete(out.Previous, name)
		}
	}
	if len(out.Dirs) == 0 {
		out.Dirs = nil
	}
	if len(out.Previous) == 0 {
		out.Previous = nil
	}
	return out
}

// dirIsEmpty reports whether the directory at path has no entries.
func dirIsEmpty(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	_, err = f.Readdirnames(1)
	if errors.Is(err, io.EOF) {
		return true, nil
	}
	return false, err
}

// localLeafIsEmpty reports whether a shared dir's local directory holds no
// data: it does not exist, or it is an empty directory. Symlinks are not
// followed; a symlink or any other non-directory counts as not empty.
func localLeafIsEmpty(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsDir() {
		return false, nil
	}
	return dirIsEmpty(path)
}

// checkChangedSharedDirs runs the start check for each shared dir whose
// backend an explicit change moved to nfs (rec.Previous). It returns the
// dirs that passed, whose previous entries the caller drops, or an error
// for the first dir whose nfs directory is empty while its previous local
// directory is not. On Kubernetes the previous local storage is a volume
// the broker cannot read, so an empty nfs directory is refused. A dir this
// start does not mount from nfs is skipped and keeps its entry. volumes maps
// each mounted dir's name to its volume (from resolveSharedDirsPerDir).
func checkChangedSharedDirs(rec *sharedDirStorageRecord, dirs []api.SharedDir, realization *runtime.SharedDirRealization, volumes map[string]api.VolumeMount, projectDir, runtimeName string) ([]string, error) {
	if rec == nil || len(rec.Previous) == 0 {
		return nil, nil
	}
	var passed []string
	for _, d := range dirs {
		if _, ok := rec.Previous[d.Name]; !ok || !realization.Serves(d.Name) {
			continue
		}
		nfsLeaf := volumes[d.Name].Source
		if nfsLeaf == "" {
			return nil, fmt.Errorf("shared dir %q: cannot find its nfs directory to check it after the backend change", d.Name)
		}
		nfsEmpty, err := dirIsEmpty(nfsLeaf)
		if err != nil {
			return nil, fmt.Errorf("shared dir %q: checking its nfs directory %s: %w", d.Name, nfsLeaf, err)
		}
		if !nfsEmpty {
			passed = append(passed, d.Name)
			continue
		}
		next := fmt.Sprintf("Copy the data into the nfs directory and start again, or reincarnate with %s %s=nfs %s to start with the empty directory",
			sharedDirBackendFlag, d.Name, allowEmptySharedDirFlag)
		if isKubernetesRuntime(runtimeName) {
			return nil, fmt.Errorf("shared dir %q now uses the nfs backend and its nfs directory %s is empty; its previous local storage is a Kubernetes volume that this broker cannot read, so the start is refused. %s",
				d.Name, nfsLeaf, next)
		}
		localLeaf, err := config.GetSharedDirPath(projectDir, d.Name)
		if err != nil {
			return nil, fmt.Errorf("shared dir %q: resolving its previous local directory: %w", d.Name, err)
		}
		localEmpty, err := localLeafIsEmpty(localLeaf)
		if err != nil {
			return nil, fmt.Errorf("shared dir %q: checking its previous local directory %s: %w", d.Name, localLeaf, err)
		}
		if !localEmpty {
			return nil, fmt.Errorf("shared dir %q now uses the nfs backend, but its nfs directory %s is empty while its previous local directory %s is not. %s",
				d.Name, nfsLeaf, localLeaf, next)
		}
		passed = append(passed, d.Name)
	}
	return passed, nil
}

// pendingSharedDirBackendChange is a validated explicit backend change,
// recorded once Reprovision has provisioned the agent.
type pendingSharedDirBackendChange struct {
	agentDir string
	gs       *config.VersionedSettings
	dirs     []api.SharedDir
	rec      *sharedDirStorageRecord // nil when the agent has no record yet
	// createdProfile is the profile in the agent's agent-info.json before
	// Reprovision re-renders it, used when the request names no profile.
	createdProfile string
}

// savedAgentProfile returns the profile recorded for the agent before this
// reprovision, or "" when none is recorded.
func savedAgentProfile(opts api.StartOptions) string {
	if info := getSavedAgentInfo(opts.Name, opts.ProjectPath); info != nil {
		return info.Profile
	}
	return ""
}

// prepareSharedDirBackendChange loads what an explicit backend change needs
// and validates it. A request that cannot be honoured is refused with
// ErrReprovisionRefused, before anything is provisioned.
func prepareSharedDirBackendChange(projectDir, agentDir string, opts api.StartOptions) (*pendingSharedDirBackendChange, error) {
	settings, _, err := config.LoadEffectiveSettings(projectDir)
	if err != nil {
		return nil, fmt.Errorf("reprovision: load effective settings for the shared dir backend change: %w", err)
	}
	gs, _, err := config.LoadGlobalSettingsWithOverlay()
	if err != nil {
		return nil, fmt.Errorf("reprovision: load global settings for the shared dir backend change: %w", err)
	}
	dirs := agentSharedDirs(settings, opts)
	if err := validateSharedDirBackendChanges(opts.SharedDirBackendChanges, opts.AllowEmptySharedDir, dirs, gs); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrReprovisionRefused, err)
	}
	rec, err := loadSharedDirStorageRecord(agentDir)
	if err != nil {
		return nil, err
	}
	return &pendingSharedDirBackendChange{agentDir: agentDir, gs: gs, dirs: dirs, rec: rec, createdProfile: savedAgentProfile(opts)}, nil
}

// record writes the changed record. An agent without a record first gets
// the record its next start would have written, using the request's
// profile, else the profile the agent had before this reprovision, else
// the re-rendered config's.
func (c *pendingSharedDirBackendChange) record(opts api.StartOptions, cfg *api.ScionConfig) error {
	rec := c.rec
	if rec == nil {
		profile := opts.Profile
		if profile == "" {
			profile = c.createdProfile
		}
		if profile == "" && cfg != nil && cfg.Info != nil {
			profile = cfg.Info.Profile
		}
		initial, err := initialSharedDirStorageRecord(c.gs, profile, c.dirs, opts.Name)
		if err != nil {
			return fmt.Errorf("recording the shared dir backend change: %w", err)
		}
		rec = initial
	}
	if err := saveSharedDirStorageRecord(c.agentDir, changeSharedDirBackends(rec, opts.SharedDirBackendChanges, opts.AllowEmptySharedDir)); err != nil {
		return fmt.Errorf("recording the shared dir backend change: %w", err)
	}
	return nil
}
