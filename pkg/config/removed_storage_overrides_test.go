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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func overrideTestSD() *V1SharedDirStorageConfig {
	return &V1SharedDirStorageConfig{Backend: "local", NFS: &V1NFSConfig{
		MountRoot: "/mnt/share", SubPathRoot: "tree/projects",
		Shares: []V1NFSShare{{ID: "share-a", Server: "192.0.2.10", Export: "/export/a", PVName: "share-a-pv"}},
	}}
}

func asMigrationRequired(t *testing.T, err error) *StorageMigrationRequiredError {
	t.Helper()
	var se *StorageMigrationRequiredError
	require.True(t, errors.As(err, &se), "want *StorageMigrationRequiredError, got %v", err)
	return se
}

func TestRemovedStorageOverridesError_None(t *testing.T) {
	assert.NoError(t, RemovedStorageOverridesError(GlobalSettingsSource, nil, nil, nil))
	assert.NoError(t, RemovedStorageOverridesError(GlobalSettingsSource,
		map[string]V1RuntimeConfig{"r": {Type: "kubernetes", SharedDirStorageClass: "fast"}},
		map[string]V1ProfileConfig{"p": {Runtime: "r"}}, overrideTestSD()))
	var vs *VersionedSettings
	assert.NoError(t, vs.RemovedStorageOverridesError(GlobalSettingsSource))
}

func TestRemovedStorageOverridesError_EveryKeyAndPlace(t *testing.T) {
	runtimes := map[string]V1RuntimeConfig{
		"r1": {SharedDirStorageBackend: "nfs", HomeStorageLeaf: "pod"},
		"r2": {SharedDirStorageBackends: map[string]string{"b": "nfs", "a": "local"}},
	}
	profiles := map[string]V1ProfileConfig{
		"p1": {HomeStorageBackend: "nfs"},
		"p2": {SharedDirStorageBackend: "nfs"},
	}
	se := asMigrationRequired(t, RemovedStorageOverridesError(HubStoredSettingsSource, runtimes, profiles, overrideTestSD()))
	for _, want := range []string{
		HubStoredSettingsSource + " sets removed storage overrides",
		`profiles.p1.home_storage_backend ("nfs")`,
		`profiles.p2.shared_dir_storage_backend ("nfs")`,
		`runtimes.r1.home_storage_leaf ("pod")`,
		`runtimes.r1.shared_dir_storage_backend ("nfs")`,
		`runtimes.r2.shared_dir_storage_backends ("a=local,b=nfs")`,
		`server.shared_dir_storage.backend: "nfs"`,
		`server.home_storage.backend: "nfs"`,
		"volumes:",
	} {
		assert.Contains(t, se.Error(), want)
		assert.Contains(t, se.PublicMessage(), want)
	}
	// Sorted by path.
	assert.Less(t, strings.Index(se.Error(), "profiles.p1"), strings.Index(se.Error(), "runtimes.r2"))
	// The log text names the claim and subpath root that decide where the
	// data is; the public text names neither.
	assert.Contains(t, se.Error(), `pv_name "share-a-pv", subpath_root "tree/projects"`)
	assert.NotContains(t, se.PublicMessage(), "share-a-pv")
	assert.NotContains(t, se.PublicMessage(), "tree/projects")
	// server and export are "keep accurate", never data-path critical.
	assert.Contains(t, se.Error(), "keep shares[0].server and export accurate")
}

func TestRemovedStorageOverridesError_RemedyFollowsKeys(t *testing.T) {
	homesOnly := asMigrationRequired(t, RemovedStorageOverridesError(GlobalSettingsSource, nil,
		map[string]V1ProfileConfig{"p": {HomeStorageBackend: "nfs"}}, overrideTestSD()))
	assert.Contains(t, homesOnly.Error(), "a removed storage override:")
	assert.Contains(t, homesOnly.Error(), "server.home_storage.backend")
	assert.NotContains(t, homesOnly.Error(), "server.shared_dir_storage.backend")

	sharedOnly := asMigrationRequired(t, RemovedStorageOverridesError(GlobalSettingsSource, nil,
		map[string]V1ProfileConfig{"p": {SharedDirStorageBackend: "nfs"}}, nil))
	assert.Contains(t, sharedOnly.Error(), "server.shared_dir_storage.backend")
	assert.NotContains(t, sharedOnly.Error(), "server.home_storage.backend")
	assert.Equal(t, sharedOnly.PublicMessage(), sharedOnly.Error(), "without an nfs block both texts are the same")
}

func TestRemovedStorageOverridesForProfile(t *testing.T) {
	vs := &VersionedSettings{
		ActiveProfile: "active-nfs",
		Runtimes: map[string]V1RuntimeConfig{
			"plain-rt": {Type: "kubernetes"},
			"homes-rt": {Type: "kubernetes", HomeStorageBackend: "nfs"},
		},
		Profiles: map[string]V1ProfileConfig{
			"active-nfs": {Runtime: "plain-rt", SharedDirStorageBackend: "nfs"},
			"via-rt":     {Runtime: "homes-rt"},
			"plain":      {Runtime: "plain-rt"},
		},
		Server: &V1ServerConfig{SharedDirStorage: overrideTestSD()},
	}
	se := asMigrationRequired(t, vs.RemovedStorageOverridesForProfile("active-nfs"))
	assert.Contains(t, se.Error(), "profiles.active-nfs.shared_dir_storage_backend")
	assert.NotContains(t, se.Error(), "runtimes.homes-rt", "another profile's runtime entry is not this start's chain")

	se = asMigrationRequired(t, vs.RemovedStorageOverridesForProfile(""))
	assert.Contains(t, se.Error(), "profiles.active-nfs", "an unnamed profile is the active profile")

	se = asMigrationRequired(t, vs.RemovedStorageOverridesForProfile("via-rt"))
	assert.Contains(t, se.Error(), "runtimes.homes-rt.home_storage_backend")

	assert.NoError(t, vs.RemovedStorageOverridesForProfile("plain"))
	assert.NoError(t, vs.RemovedStorageOverridesForProfile("missing"))

	// A flat instance consults no profile.
	flat := vs.ForFlatInstance(&FlatInstancePolicy{})
	assert.NoError(t, flat.RemovedStorageOverridesForProfile("active-nfs"))

	var nilVS *VersionedSettings
	assert.NoError(t, nilVS.RemovedStorageOverridesForProfile("active-nfs"))
}

// TestProjectStorageMigrationText_PinnedAndRealSteps pins the one text that
// tells operators how existing data reaches the project storage layout. It
// names the hub maintenance migration and its API route, which exist in
// this build, and no CLI command: neither `scion config migrate` nor
// `scion server migrate-storage` moves project data.
func TestProjectStorageMigrationText_PinnedAndRealSteps(t *testing.T) {
	assert.Equal(t, "the hub's project-storage-migrate maintenance migration "+
		"(Admin → Maintenance, or POST /api/v1/admin/maintenance/migrations/project-storage-migrate/run; run it with dryRun first) "+
		"moves the hub's own trees; each Runtime Broker moves its own node-local shared dirs into <ProjectRoot>/shared-dirs/<name> "+
		"on the share on its first start after upgrade with its shared-dir layout on the share "+
		"(copy-only, never overwriting; the source is kept as <name>.migrated-<ts>)", ProjectStorageMigrationText)
	assertNoNonexistentMigrationStep(t, ProjectStorageMigrationText)
}

// nonexistentMigrationSteps are texts an operator-facing storage message
// must not contain: commands that do not move project data, the vague
// pointer this text replaced, and promises of future behaviour.
var nonexistentMigrationSteps = []string{"scion config migrate", "migrate-storage", "run the project-storage migration", "coming"}

func assertNoNonexistentMigrationStep(t *testing.T, text string) {
	t.Helper()
	for _, bad := range nonexistentMigrationSteps {
		assert.NotContains(t, text, bad)
	}
}

// TestStorageTexts_NameOnlyRealSteps checks every startup warning that
// points at moving data, and the removed-override refusal, for steps that
// do not exist; the warnings that point at the migration use the pinned
// text.
func TestStorageTexts_NameOnlyRealSteps(t *testing.T) {
	nfs := &V1NFSConfig{MountRoot: "/mnt/share", SubPathRoot: "tree/projects",
		Shares: []V1NFSShare{{ID: "share-a", Server: "192.0.2.10", Export: "/export/a", PVName: "share-a-pv"}}}
	cases := []struct {
		name string
		ws   *V1WorkspaceStorageConfig
		sd   *V1SharedDirStorageConfig
	}{
		{name: "split", ws: &V1WorkspaceStorageConfig{Backend: "local"}, sd: &V1SharedDirStorageConfig{Backend: "nfs", NFS: nfs}},
		{name: "workspace nfs, no shared_dir_storage", ws: &V1WorkspaceStorageConfig{Backend: "nfs", NFS: nfs}},
		{name: "workspace nfs, shared_dir_storage local", ws: &V1WorkspaceStorageConfig{Backend: "nfs", NFS: nfs}, sd: &V1SharedDirStorageConfig{Backend: "local"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, warnings, err := SynthesizeProjectStorage(nil, tc.ws, tc.sd, nil)
			require.NoError(t, err)
			require.NotEmpty(t, warnings)
			assert.Contains(t, warnings[0], "check the file with scion config validate")
			for _, w := range warnings {
				assertNoNonexistentMigrationStep(t, w)
				assert.Contains(t, w, ProjectStorageMigrationText)
			}
		})
	}
	refusal := asMigrationRequired(t, RemovedStorageOverridesError(GlobalSettingsSource, nil,
		map[string]V1ProfileConfig{"p": {SharedDirStorageBackend: "nfs", HomeStorageBackend: "nfs"}}, overrideTestSD()))
	assertNoNonexistentMigrationStep(t, refusal.Error())
	assertNoNonexistentMigrationStep(t, refusal.PublicMessage())
}
