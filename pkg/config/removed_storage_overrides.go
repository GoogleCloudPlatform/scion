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
	"fmt"
	"sort"
	"strings"
)

// ProjectStorageMigrationText is the one operator-facing description of
// how existing project data reaches the project storage layout in this
// build: the hub maintenance migration for the hub's trees, and each
// broker's start hook (runtimebroker migrateLocalSharedDirs) for its
// node-local shared dirs. Every warning, refusal and doc line that points at the migration
// uses it, so none names a command that does not exist (the migration is a
// hub maintenance migration, not a CLI command) or a step this build does
// not ship. It is a clause that starts in lower case.
const ProjectStorageMigrationText = "the hub's project-storage-migrate maintenance migration " +
	"(Admin → Maintenance, or POST /api/v1/admin/maintenance/migrations/project-storage-migrate/run; run it with dryRun first) " +
	"moves the hub's own trees; each Runtime Broker moves its own node-local shared dirs into <ProjectRoot>/shared-dirs/<name> " +
	"on the share on its first start after upgrade with its shared-dir layout on the share " +
	"(copy-only, never overwriting; the source is kept as <name>.migrated-<ts>)"

// removedStorageOverride is one removed per-runtime or per-profile storage
// override that a settings source still sets: its settings path and the
// value as written (a per-dir map is rendered as name=value pairs).
type removedStorageOverride struct {
	path, value string
	sharedDir   bool
}

// collectRemovedStorageOverrides lists every removed storage override set
// on runtimes or profiles (the keys RemovedStorageOverrideErrors reports),
// sorted by path.
func collectRemovedStorageOverrides(runtimes map[string]V1RuntimeConfig, profiles map[string]V1ProfileConfig) []removedStorageOverride {
	var out []removedStorageOverride
	check := func(prefix, sdBackend string, sdBackends map[string]string, homeBackend, homeLeaf string) {
		if sdBackend != "" {
			out = append(out, removedStorageOverride{path: prefix + ".shared_dir_storage_backend", value: sdBackend, sharedDir: true})
		}
		if len(sdBackends) > 0 {
			pairs := make([]string, 0, len(sdBackends))
			for name, v := range sdBackends {
				pairs = append(pairs, name+"="+v)
			}
			sort.Strings(pairs)
			out = append(out, removedStorageOverride{path: prefix + ".shared_dir_storage_backends", value: strings.Join(pairs, ","), sharedDir: true})
		}
		if homeBackend != "" {
			out = append(out, removedStorageOverride{path: prefix + ".home_storage_backend", value: homeBackend})
		}
		if homeLeaf != "" {
			out = append(out, removedStorageOverride{path: prefix + ".home_storage_leaf", value: homeLeaf})
		}
	}
	for name, rt := range runtimes {
		check("runtimes."+name, rt.SharedDirStorageBackend, rt.SharedDirStorageBackends, rt.HomeStorageBackend, rt.HomeStorageLeaf)
	}
	for name, p := range profiles {
		check("profiles."+name, p.SharedDirStorageBackend, p.SharedDirStorageBackends, p.HomeStorageBackend, p.HomeStorageLeaf)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

// StorageMigrationRequiredError is the refusal of an agent start whose
// storage would be placed differently than the operator configured it
// before server.project_storage (ptone/scion#6371): its profile or runtime
// entry carries a removed storage override, or its per-agent shared-dir
// backend record pins nfs while the shared-dir layout is not shared. The
// runtime broker answers it with 422 storage_migration_required.
//
// Error is the full text for logs (it may name the claim and subpath root
// of the share). PublicMessage is the text a hub user sees: the keys and
// the migration steps, without the share's names.
type StorageMigrationRequiredError struct {
	detail string
	public string
}

func (e *StorageMigrationRequiredError) Error() string { return e.detail }

// PublicMessage returns the refusal without share-specific names.
func (e *StorageMigrationRequiredError) PublicMessage() string { return e.public }

// NewStorageMigrationRequiredError returns a StorageMigrationRequiredError
// with the given full (log) and public texts.
func NewStorageMigrationRequiredError(detail, public string) *StorageMigrationRequiredError {
	return &StorageMigrationRequiredError{detail: detail, public: public}
}

// GlobalSettingsSource and HubStoredSettingsSource name where runtimes and
// profiles came from, for RemovedStorageOverridesError: the global settings
// file, and the hub's stored runtimes and profiles (the settings overlay a
// co-located broker reads).
const (
	GlobalSettingsSource    = "the global settings file"
	HubStoredSettingsSource = "the hub's stored settings"
)

// RemovedStorageOverridesError returns a *StorageMigrationRequiredError
// naming every removed per-runtime or per-profile storage override that
// runtimes or profiles set (shared_dir_storage_backend,
// shared_dir_storage_backends, home_storage_backend, home_storage_leaf),
// with where it was found (source) and the migration text, or nil when
// none is set. sd is server.shared_dir_storage, used only to name the claim
// and subpath root in the migration text.
//
// These keys are no longer applied (design: no per-runtime storage
// overrides; server.project_storage places every agent's storage), so a
// process that runs agents with them would silently place storage
// differently than before the upgrade (ptone/scion#6371). Process start
// reports this error (log, health, admin settings) and keeps running;
// agent start refuses with it (RemovedStorageOverridesForProfile).
func RemovedStorageOverridesError(source string, runtimes map[string]V1RuntimeConfig, profiles map[string]V1ProfileConfig, sd *V1SharedDirStorageConfig) error {
	return removedStorageOverridesError(source, collectRemovedStorageOverrides(runtimes, profiles), sd)
}

func removedStorageOverridesError(source string, overrides []removedStorageOverride, sd *V1SharedDirStorageConfig) error {
	if len(overrides) == 0 {
		return nil
	}
	set := make([]string, len(overrides))
	sharedDirs, homes := false, false
	for i, o := range overrides {
		set[i] = fmt.Sprintf("%s (%q)", o.path, o.value)
		if o.sharedDir {
			sharedDirs = true
		} else {
			homes = true
		}
	}
	head := fmt.Sprintf("%s sets %s: %s. These storage overrides were removed with %s and are no longer applied, "+
		"so agents using them are refused rather than started with their storage placed elsewhere. ",
		source, plural(len(set), "a removed storage override", "removed storage overrides"), strings.Join(set, ", "), ProjectStorageKey)
	tail := "Then remove the keys listed and check the settings file with scion config validate " +
		"(stored runtimes and profiles are edited in the hub's admin settings, where a save drops them). " +
		"Storage is no longer chosen per runtime or profile; for a node-local directory use a volumes: entry"
	homeText := ""
	if homes {
		homeText = fmt.Sprintf("To keep agent homes on nfs, set %s.backend: \"nfs\" (or homes: \"nfs\" in %s). ", homeStorageKey, ProjectStorageKey)
	}
	detail, public := head, head
	if sharedDirs {
		detail += SharedDirMigrationText(sd)
		public += SharedDirMigrationText(nil)
	}
	detail += homeText + tail
	public += homeText + tail
	return NewStorageMigrationRequiredError(detail, public)
}

// SharedDirMigrationText is the shared-dir remedy of a
// StorageMigrationRequiredError. With sd's nfs block it names the claim
// (shares[0].pv_name) and subpath root shared dirs were mounted from;
// pass nil for the public text, which names neither.
func SharedDirMigrationText(sd *V1SharedDirStorageConfig) string {
	var b strings.Builder
	fmt.Fprintf(&b, "To keep shared dirs on the nfs export, set %s.backend: \"nfs\" "+
		"(transitional: shared dirs of every runtime then use that export, and workspaces stay where they are), "+
		"or write %s with backend \"nfs\" (this also moves workspaces onto the export). ", sharedDirStorageKey, ProjectStorageKey)
	b.WriteString("Check shares[0].pv_name and subpath_root against the live export: Kubernetes agents mount that claim at <subpath_root>/<project-id>/" +
		SharedDirsSubdir + "/<name>, so they decide where the existing shared-dir data is")
	if sd != nil && sd.NFS != nil && len(sd.NFS.Shares) > 0 {
		fmt.Fprintf(&b, " (now: pv_name %q, subpath_root %q)", sd.NFS.Shares[0].PVName, SubPathRootOrDefault(sd.NFS.SubPathRoot))
	}
	b.WriteString("; keep shares[0].server and export accurate too. ")
	return b.String()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// RemovedStorageOverridesError is RemovedStorageOverridesError over vs's
// runtimes and profiles (after any hub settings overlay) and its
// server.shared_dir_storage. Nil for a nil vs.
func (vs *VersionedSettings) RemovedStorageOverridesError(source string) error {
	if vs == nil {
		return nil
	}
	return RemovedStorageOverridesError(source, vs.Runtimes, vs.Profiles, vs.serverSharedDirStorage())
}

// RemovedStorageOverridesForProfile is the agent-start check: a
// *StorageMigrationRequiredError when the profile an agent starts with
// (profileName, else vs.ActiveProfile) or that profile's runtime entry
// sets a removed storage override, else nil. Agents of other profiles are
// unaffected. A flat instance view consults no profile (its runtime_target
// overrides are refused when the instance loads), and an unknown profile
// carries no override. vs is the global settings with the hub overlay,
// as the start reads them.
func (vs *VersionedSettings) RemovedStorageOverridesForProfile(profileName string) error {
	if vs == nil || vs.flat != nil {
		return nil
	}
	if profileName == "" {
		profileName = vs.ActiveProfile
	}
	profile, ok := vs.Profiles[profileName]
	if !ok {
		return nil
	}
	profiles := map[string]V1ProfileConfig{profileName: profile}
	var runtimes map[string]V1RuntimeConfig
	if rt, ok := vs.Runtimes[profile.Runtime]; ok {
		runtimes = map[string]V1RuntimeConfig{profile.Runtime: rt}
	}
	return removedStorageOverridesError(GlobalSettingsSource+" (with any hub-stored runtimes and profiles)",
		collectRemovedStorageOverrides(runtimes, profiles), vs.serverSharedDirStorage())
}

func (vs *VersionedSettings) serverSharedDirStorage() *V1SharedDirStorageConfig {
	if vs == nil || vs.Server == nil {
		return nil
	}
	return vs.Server.SharedDirStorage
}
