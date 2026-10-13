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
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
)

// An explicit shared-dir backend change moves the recorded backend of named
// shared dirs from local to nfs, or from nfs back to local, as part of a
// reincarnation. Only the agent's record changes: the data is copied by the
// operator, and neither directory is ever moved or deleted. The next start
// refuses an empty directory on the new backend while the directory on the
// previous backend is not empty, unless the change was made with
// AllowEmptySharedDir.

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
// target backend must be nfs or local, and a change to nfs needs a complete
// server.shared_dir_storage.nfs in gs (global settings). allowEmpty without
// a change is an error.
func validateSharedDirBackendChanges(changes map[string]string, allowEmpty bool, dirs []api.SharedDir, gs *config.VersionedSettings) error {
	if len(changes) == 0 {
		if allowEmpty {
			return fmt.Errorf("%s needs a shared dir backend change (%s NAME=nfs or NAME=local)", allowEmptySharedDirFlag, sharedDirBackendFlag)
		}
		return nil
	}
	toNFS := false
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
		switch backend := changes[name]; backend {
		case "nfs":
			toNFS = true
		case "local":
		default:
			return fmt.Errorf("shared dir %q: only a change to the nfs or local backend is supported (got %q)", name, backend)
		}
		if !known[name] {
			if len(names) == 0 {
				return fmt.Errorf("shared dir %q is not one of the agent's shared dirs (it has none)", name)
			}
			return fmt.Errorf("shared dir %q is not one of the agent's shared dirs (%s)", name, strings.Join(names, ", "))
		}
	}
	if !toNFS {
		return nil
	}
	if err := nfsSharedDirStorage(gs).Validate(); err != nil {
		return fmt.Errorf("a change to the nfs backend needs a complete server.shared_dir_storage.nfs block on this broker: %w", err)
	}
	return nil
}

// nfsSharedDirStorage returns the nfs shared-dir storage config of gs
// (global settings): backend nfs with gs's server.shared_dir_storage.nfs
// block, which may be missing or incomplete; callers validate it.
func nfsSharedDirStorage(gs *config.VersionedSettings) *config.V1SharedDirStorageConfig {
	nfs := &config.V1SharedDirStorageConfig{Backend: "nfs"}
	if gs != nil && gs.Server != nil && gs.Server.SharedDirStorage != nil {
		nfs.NFS = gs.Server.SharedDirStorage.NFS
	}
	return nfs
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
// recorded on its target backend (nfs or local). A dir that moves gets a
// previous entry naming the backend it leaves, so the next start checks its
// directories, unless allowEmpty is set; allowEmpty also drops any previous
// entry for the named dirs. A dir that moves back to the backend its
// pending previous entry names (a change not yet checked by a start) only
// loses that entry: its data never left that backend. A dir already on its
// target backend is otherwise left as it is.
func changeSharedDirBackends(rec *sharedDirStorageRecord, changes map[string]string, allowEmpty bool) *sharedDirStorageRecord {
	out := &sharedDirStorageRecord{
		Backend:  rec.Backend,
		Dirs:     maps.Clone(rec.Dirs),
		Previous: maps.Clone(rec.Previous),
	}
	for _, name := range slices.Sorted(maps.Keys(changes)) {
		target := changes[name]
		current := out.backendFor(name)
		if current != target {
			if out.Backend == target {
				delete(out.Dirs, name)
			} else {
				if out.Dirs == nil {
					out.Dirs = make(map[string]string)
				}
				out.Dirs[name] = target
			}
			switch {
			case allowEmpty:
			case out.Previous[name] == target:
				delete(out.Previous, name)
			default:
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

// sharedDirCheckInput is what checkChangedSharedDirs needs from a start.
type sharedDirCheckInput struct {
	rec  *sharedDirStorageRecord
	dirs []api.SharedDir
	// realization and volumes come from resolveSharedDirsPerDir; volumes
	// maps each mounted dir's name to its volume.
	realization *runtime.SharedDirRealization
	volumes     map[string]api.VolumeMount
	projectDir  string
	runtimeName string

	// The fields below are used only for dirs changed back to local.
	//
	// gs is the global settings snapshot of the start, for the nfs block
	// that locates a dir's previous nfs directory, and projectID the
	// hub-dispatched project ID that keys it.
	gs        *config.VersionedSettings
	projectID string
	// claims, when the runtime implements it, tells whether a dir's local
	// storage is a claim of its own and looks that claim up (Kubernetes).
	claims runtime.SharedDirClaimChecker
}

// checkChangedSharedDirs runs the start check for each shared dir whose
// backend an explicit change moved (rec.Previous). It returns the dirs that
// passed, whose previous entries the caller drops, and the dirs changed
// back to local whose local storage is a runtime claim, which the caller
// checks with checkSharedDirClaims once the run config is known; or an
// error for the first dir that is refused. A dir whose backend for this
// start does not match its change is skipped and keeps its entry.
//
// A dir moved to nfs is refused when its nfs directory is empty while its
// previous local directory is not. On Kubernetes the previous local
// storage is a volume the broker cannot read, so an empty nfs directory is
// refused.
//
// A dir moved back to local, on a runtime that bind-mounts the local
// directory, is refused when its previous nfs directory cannot be checked,
// or is not empty while the local directory is empty. On any other runtime
// the local storage cannot be read here: a runtime that cannot look up
// claims skips the check with a warning, and the others defer it.
func checkChangedSharedDirs(ctx context.Context, in sharedDirCheckInput) (passed, deferred []string, err error) {
	if in.rec == nil || len(in.rec.Previous) == 0 {
		return nil, nil, nil
	}
	for _, d := range in.dirs {
		previous, ok := in.rec.Previous[d.Name]
		if !ok {
			continue
		}
		served := in.realization.Serves(d.Name)
		switch {
		case previous == "local" && served:
			if err := checkChangedToNFS(in, d.Name); err != nil {
				return nil, nil, err
			}
		case previous == "nfs" && !served && in.rec.backendFor(d.Name) == "local":
			if !isLocalContainerRuntime(in.runtimeName) {
				if in.claims == nil {
					slog.Warn("Start: not checking a shared dir changed back to the local backend; this runtime cannot look up its local storage",
						"shared_dir", d.Name, "runtime", in.runtimeName)
				} else {
					deferred = append(deferred, d.Name)
					continue
				}
			} else if err := checkChangedToLocalDir(in, d.Name); err != nil {
				return nil, nil, err
			}
		default:
			continue
		}
		passed = append(passed, d.Name)
	}
	return passed, deferred, nil
}

// dropCheckedSharedDirs removes the previous entries of the dirs in names
// from rec and saves the result for the agent in agentDir. It returns the
// updated record (rec itself when names is empty). A failed save only logs
// a warning: the check then runs again at the next start.
func dropCheckedSharedDirs(agentDir string, rec *sharedDirStorageRecord, names []string, agentName string) *sharedDirStorageRecord {
	if rec == nil || len(names) == 0 {
		return rec
	}
	updated := *rec
	updated.Previous = maps.Clone(rec.Previous)
	for _, name := range names {
		delete(updated.Previous, name)
	}
	if len(updated.Previous) == 0 {
		updated.Previous = nil
	}
	if err := saveSharedDirStorageRecord(agentDir, &updated); err != nil {
		slog.Warn("Start: could not update the agent's shared-dir storage record after checking changed shared dirs", "agent", agentName, "error", err)
	}
	return &updated
}

// sharedDirCheckNext is the end of a refusal of the start check for the dir
// name, moved to backend.
func sharedDirCheckNext(name, backend string) string {
	return fmt.Sprintf("Copy the data into the %s directory and start again, or reincarnate with %s %s=%s %s to start with the empty directory",
		backend, sharedDirBackendFlag, name, backend, allowEmptySharedDirFlag)
}

// checkChangedToNFS checks the dir name after a change from local to nfs.
func checkChangedToNFS(in sharedDirCheckInput, name string) error {
	nfsLeaf := in.volumes[name].Source
	if nfsLeaf == "" {
		return fmt.Errorf("shared dir %q: cannot find its nfs directory to check it after the backend change", name)
	}
	nfsEmpty, err := dirIsEmpty(nfsLeaf)
	if err != nil {
		return fmt.Errorf("shared dir %q: checking its nfs directory %s: %w", name, nfsLeaf, err)
	}
	if !nfsEmpty {
		return nil
	}
	next := sharedDirCheckNext(name, "nfs")
	if isKubernetesRuntime(in.runtimeName) {
		return fmt.Errorf("shared dir %q now uses the nfs backend and its nfs directory %s is empty; its previous local storage is a Kubernetes volume that this broker cannot read, so the start is refused. %s",
			name, nfsLeaf, next)
	}
	localLeaf, err := config.GetSharedDirPath(in.projectDir, name)
	if err != nil {
		return fmt.Errorf("shared dir %q: resolving its previous local directory: %w", name, err)
	}
	localEmpty, err := localLeafIsEmpty(localLeaf)
	if err != nil {
		return fmt.Errorf("shared dir %q: checking its previous local directory %s: %w", name, localLeaf, err)
	}
	if !localEmpty {
		return fmt.Errorf("shared dir %q now uses the nfs backend, but its nfs directory %s is empty while its previous local directory %s is not. %s",
			name, nfsLeaf, localLeaf, next)
	}
	return nil
}

// checkPreviousNFSDir reports whether the previous nfs directory of the dir
// name holds data, and returns its path. A directory that cannot be checked
// is a refusal of the start, naming the flag that skips the check.
func checkPreviousNFSDir(in sharedDirCheckInput, name string) (nfsLeaf string, hasData bool, err error) {
	nfsLeaf, empty, err := previousNFSLeafIsEmpty(in.gs, in.projectID, name)
	if err != nil {
		return "", false, fmt.Errorf("shared dir %q now uses the local backend, but its previous nfs directory cannot be checked, so the start is refused: %v. Start again once it can be checked, or reincarnate with %s %s=local %s to skip the check",
			name, err, sharedDirBackendFlag, name, allowEmptySharedDirFlag)
	}
	return nfsLeaf, !empty, nil
}

// checkChangedToLocalDir checks the dir name after a change from nfs back
// to local on a runtime that bind-mounts the broker's local directory.
func checkChangedToLocalDir(in sharedDirCheckInput, name string) error {
	nfsLeaf, hasData, err := checkPreviousNFSDir(in, name)
	if err != nil || !hasData {
		return err
	}
	localLeaf, err := config.GetSharedDirPath(in.projectDir, name)
	if err != nil {
		return fmt.Errorf("shared dir %q: resolving its local directory: %w", name, err)
	}
	localEmpty, err := localLeafIsEmpty(localLeaf)
	if err != nil {
		return fmt.Errorf("shared dir %q: checking its local directory %s: %w", name, localLeaf, err)
	}
	if localEmpty {
		return fmt.Errorf("shared dir %q now uses the local backend, but its local directory %s is empty while its previous nfs directory %s is not. %s",
			name, localLeaf, nfsLeaf, sharedDirCheckNext(name, "local"))
	}
	return nil
}

// checkSharedDirClaims checks the dirs in names, deferred by
// checkChangedSharedDirs, against cfg, the config the runtime is about to
// run. A dir to which the runtime gives no claim of its own (on Kubernetes,
// local dirs served from the nfs workspace claim) is skipped with a
// warning. Otherwise a previous nfs directory that cannot be checked
// refuses the start, an empty one passes, and when it holds data a missing
// claim or a failed lookup refuses the start, while an existing claim
// passes with a warning that its content was not checked. It returns the
// dirs that passed.
func checkSharedDirClaims(ctx context.Context, in sharedDirCheckInput, names []string, cfg runtime.RunConfig) ([]string, error) {
	if in.claims == nil {
		return nil, fmt.Errorf("checking shared dirs changed back to local: the runtime cannot look up their storage")
	}
	var passed []string
	for _, name := range names {
		if !in.claims.SharedDirUsesClaim(cfg, name) {
			slog.Warn("Start: not checking a shared dir changed back to the local backend; its local storage has no claim of its own (it is served from the nfs workspace claim)",
				"shared_dir", name)
			passed = append(passed, name)
			continue
		}
		nfsLeaf, hasData, err := checkPreviousNFSDir(in, name)
		if err != nil {
			return nil, err
		}
		if !hasData {
			passed = append(passed, name)
			continue
		}
		exists, err := in.claims.SharedDirClaimExists(ctx, cfg, name)
		if err != nil {
			return nil, fmt.Errorf("shared dir %q now uses the local backend and its previous nfs directory %s is not empty, but its local storage cannot be looked up, so the start is refused: %v. Start again once it can be looked up, or reincarnate with %s %s=local %s to skip the check",
				name, nfsLeaf, err, sharedDirBackendFlag, name, allowEmptySharedDirFlag)
		}
		if !exists {
			return nil, fmt.Errorf("shared dir %q now uses the local backend and its previous nfs directory %s is not empty, but its local storage, a Kubernetes volume claim, does not exist, so it would start empty. %s",
				name, nfsLeaf, sharedDirCheckNext(name, "local"))
		}
		slog.Warn("Start: a shared dir changed back to the local backend uses an existing Kubernetes volume claim whose content this broker cannot read; it was not checked",
			"shared_dir", name, "previous_nfs_dir", nfsLeaf)
		passed = append(passed, name)
	}
	return passed, nil
}

// previousNFSLeafIsEmpty reports whether the nfs directory of the shared
// dir name for the project projectID holds no data, and returns its path.
// The directory is resolved from gs's nfs block as an nfs start resolves
// it: the host base through its symlinks, then a walk of every component
// below it that refuses any symlink (shareddirs.OpenAnchoredRoot). Nothing
// is created. A missing directory is empty; the nfs export must be
// available at its host base.
func previousNFSLeafIsEmpty(gs *config.VersionedSettings, projectID, name string) (string, bool, error) {
	nfs := nfsSharedDirStorage(gs)
	if err := nfs.Validate(); err != nil {
		return "", false, fmt.Errorf("server.shared_dir_storage.nfs is not complete on this broker: %w", err)
	}
	if !shareddirs.ValidProjectID(projectID) {
		return "", false, fmt.Errorf("no valid hub project ID (%q) to locate it", projectID)
	}
	if err := api.ValidateSharedDirs([]api.SharedDir{{Name: name}}); err != nil {
		return "", false, err
	}
	res, err := runtime.NewNFSBackend(nfs.NFS).Resolve(runtime.ResolveInput{ProjectID: projectID, SharedDirNames: []string{name}})
	if err != nil {
		return "", false, fmt.Errorf("resolving it: %w", err)
	}
	sd, ok := res.SharedDirs[name]
	if !ok {
		return "", false, fmt.Errorf("it is not in the nfs resolution")
	}
	if err := shareddirs.ConfineLeaf(sd.HostPath, res.HostBase, config.SubPathRootOrDefault(nfs.NFS.SubPathRoot), projectID, name); err != nil {
		return "", false, err
	}
	resolvedHostBase, err := filepath.EvalSymlinks(res.HostBase)
	if err != nil {
		return "", false, fmt.Errorf("the nfs export is not available at %s: %w", res.HostBase, err)
	}
	root, err := shareddirs.OpenAnchoredRoot(resolvedHostBase, sd.ServerRelativePath)
	if errors.Is(err, fs.ErrNotExist) {
		return sd.HostPath, true, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("opening %s: %w", sd.HostPath, err)
	}
	defer func() { _ = root.Close() }()
	f, err := root.Open(".")
	if err != nil {
		return "", false, fmt.Errorf("opening %s: %w", sd.HostPath, err)
	}
	defer func() { _ = f.Close() }()
	_, err = f.Readdirnames(1)
	if errors.Is(err, io.EOF) {
		return sd.HostPath, true, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading %s: %w", sd.HostPath, err)
	}
	return sd.HostPath, false, nil
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
	return &pendingSharedDirBackendChange{agentDir: agentDir, gs: gs, dirs: dirs, rec: rec, createdProfile: GetSavedProfile(opts.Name, opts.ProjectPath)}, nil
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
