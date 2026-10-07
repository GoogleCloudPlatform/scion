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
	// nfsWorkspaceBackend is set when server.workspace_storage is nfs: on
	// Kubernetes, local shared dirs are then served from the workspace
	// export instead of a per-dir claim.
	nfsWorkspaceBackend bool
	// claims, when the runtime implements it, looks up a dir's local
	// storage claim (Kubernetes). claimLabels are the labels it needs.
	claims      runtime.SharedDirClaimChecker
	claimLabels map[string]string
}

// checkChangedSharedDirs runs the start check for each shared dir whose
// backend an explicit change moved (rec.Previous). It returns the dirs that
// passed, whose previous entries the caller drops, or an error for the
// first dir that is refused. A dir whose backend for this start does not
// match its change is skipped and keeps its entry.
//
// A dir moved to nfs is refused when its nfs directory is empty while its
// previous local directory is not. On Kubernetes the previous local
// storage is a volume the broker cannot read, so an empty nfs directory is
// refused.
//
// A dir moved back to local is refused when its previous nfs directory is
// not empty while its local directory is empty, and when the nfs directory
// cannot be checked. On Kubernetes the local storage is a claim the broker
// cannot read: a missing claim (or one that cannot be looked up) is
// refused, and an existing claim passes with a warning that its content was
// not checked. A runtime that cannot look up claims, or one whose local
// dirs are served from an nfs workspace export, skips the content check
// with a warning.
func checkChangedSharedDirs(ctx context.Context, in sharedDirCheckInput) ([]string, error) {
	if in.rec == nil || len(in.rec.Previous) == 0 {
		return nil, nil
	}
	var passed []string
	for _, d := range in.dirs {
		previous, ok := in.rec.Previous[d.Name]
		if !ok {
			continue
		}
		served := in.realization.Serves(d.Name)
		var err error
		switch {
		case previous == "local" && served:
			err = checkChangedToNFS(in, d.Name)
		case previous == "nfs" && !served && in.rec.backendFor(d.Name) == "local":
			err = checkChangedToLocal(ctx, in, d.Name)
		default:
			continue
		}
		if err != nil {
			return nil, err
		}
		passed = append(passed, d.Name)
	}
	return passed, nil
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

// checkChangedToLocal checks the dir name after a change from nfs back to
// local.
func checkChangedToLocal(ctx context.Context, in sharedDirCheckInput, name string) error {
	nfsLeaf, err := previousNFSLeaf(in.gs, in.projectID, name)
	if err == nil {
		var nfsEmpty bool
		nfsEmpty, err = localLeafIsEmpty(nfsLeaf)
		if err == nil && nfsEmpty {
			return nil
		}
	}
	if err != nil {
		return fmt.Errorf("shared dir %q now uses the local backend, but its previous nfs directory cannot be checked, so the start is refused: %v. Start again once it can be checked, or reincarnate with %s %s=local %s to skip the check",
			name, err, sharedDirBackendFlag, name, allowEmptySharedDirFlag)
	}
	next := sharedDirCheckNext(name, "local")
	switch {
	case isLocalContainerRuntime(in.runtimeName):
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
				name, localLeaf, nfsLeaf, next)
		}
		return nil
	case isKubernetesRuntime(in.runtimeName) && in.nfsWorkspaceBackend:
		slog.Warn("Start: not checking a shared dir changed back to the local backend; its local storage is served from the nfs workspace export",
			"shared_dir", name, "previous_nfs_dir", nfsLeaf)
		return nil
	case in.claims == nil:
		slog.Warn("Start: not checking a shared dir changed back to the local backend; this runtime cannot look up its local storage",
			"shared_dir", name, "runtime", in.runtimeName, "previous_nfs_dir", nfsLeaf)
		return nil
	}
	exists, err := in.claims.SharedDirClaimExists(ctx, in.claimLabels, name)
	if err != nil {
		return fmt.Errorf("shared dir %q now uses the local backend and its previous nfs directory %s is not empty, but its local storage cannot be looked up, so the start is refused: %v. Start again once it can be looked up, or reincarnate with %s %s=local %s to skip the check",
			name, nfsLeaf, err, sharedDirBackendFlag, name, allowEmptySharedDirFlag)
	}
	if !exists {
		return fmt.Errorf("shared dir %q now uses the local backend and its previous nfs directory %s is not empty, but its local storage, a Kubernetes volume claim, does not exist, so it would start empty. %s",
			name, nfsLeaf, next)
	}
	slog.Warn("Start: a shared dir changed back to the local backend uses an existing Kubernetes volume claim whose content this broker cannot read; it was not checked",
		"shared_dir", name, "previous_nfs_dir", nfsLeaf)
	return nil
}

// previousNFSLeaf returns the nfs directory of the shared dir name for the
// project projectID, resolved from gs's nfs block exactly as an nfs start
// would, without creating anything. The nfs export must be mounted at its
// host base.
func previousNFSLeaf(gs *config.VersionedSettings, projectID, name string) (string, error) {
	nfs := nfsSharedDirStorage(gs)
	if err := nfs.Validate(); err != nil {
		return "", fmt.Errorf("server.shared_dir_storage.nfs is not complete on this broker: %w", err)
	}
	if !shareddirs.ValidProjectID(projectID) {
		return "", fmt.Errorf("no valid hub project ID (%q) to locate it", projectID)
	}
	if err := api.ValidateSharedDirs([]api.SharedDir{{Name: name}}); err != nil {
		return "", err
	}
	res, err := runtime.NewNFSBackend(nfs.NFS).Resolve(runtime.ResolveInput{ProjectID: projectID, SharedDirNames: []string{name}})
	if err != nil {
		return "", fmt.Errorf("resolving it: %w", err)
	}
	sd, ok := res.SharedDirs[name]
	if !ok {
		return "", fmt.Errorf("it is not in the nfs resolution")
	}
	if err := shareddirs.ConfineLeaf(sd.HostPath, res.HostBase, config.SubPathRootOrDefault(nfs.NFS.SubPathRoot), projectID, name); err != nil {
		return "", err
	}
	if _, err := os.Stat(res.HostBase); err != nil {
		return "", fmt.Errorf("the nfs export is not available at %s: %w", res.HostBase, err)
	}
	return sd.HostPath, nil
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
