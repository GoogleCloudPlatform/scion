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
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// CheckProfileCatalog fixtures (ptone/scion#5057). Every settings view is
// built through the real loader (with the embedded default profiles "local"
// and "remote") and the real overlay. Every fixture sets active_profile
// explicitly and asserts the exact finding set.

// catalogHome isolates HOME and the working directory, clears the global
// overlay, and writes a versioned global settings file whose profiles are
// fileProfiles (each on the "docker" runtime) and whose active_profile is
// active. It returns the global .scion directory.
func catalogHome(t *testing.T, active string, fileProfiles ...string) string {
	t.Helper()
	old := GetGlobalSettingsOverlay()
	t.Cleanup(func() { SetGlobalSettingsOverlay(old) })
	SetGlobalSettingsOverlay(nil)

	home := t.TempDir()
	t.Setenv("HOME", home)
	wd, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(home))
	t.Cleanup(func() { _ = os.Chdir(wd) })

	globalDir := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0o755))
	var b strings.Builder
	b.WriteString("schema_version: \"1\"\nactive_profile: " + active + "\n")
	if len(fileProfiles) > 0 {
		b.WriteString("profiles:\n")
		for _, p := range fileProfiles {
			b.WriteString("  " + p + ":\n    runtime: docker\n")
		}
	}
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(b.String()), 0o644))
	return globalDir
}

// storedProfiles is a stored profile set of the named profiles, each on the
// "docker" runtime; nil for no stored set.
func storedProfiles(names []string) map[string]V1ProfileConfig {
	if names == nil {
		return nil
	}
	profiles := map[string]V1ProfileConfig{}
	for _, p := range names {
		profiles[p] = V1ProfileConfig{Runtime: "docker"}
	}
	return profiles
}

// catalogView is ProfileCatalogView of globalDir with stored as the stored
// profile set (nil: none).
func catalogView(t *testing.T, globalDir string, stored []string) *VersionedSettings {
	t.Helper()
	vs, err := ProfileCatalogView(globalDir, nil, storedProfiles(stored))
	require.NoError(t, err)
	return vs
}

func catalogOrigins(t *testing.T, globalDir string) map[string]ProfileSource {
	t.Helper()
	origins, err := ProfileOrigins(globalDir)
	require.NoError(t, err)
	return origins
}

type catalogCase struct {
	name          string
	active        string
	fileProfiles  []string
	before        []string // nil: unmanaged (no stored set)
	after         []string // nil: unmanaged
	projectActive map[string]string
	agents        map[string]int
	want          []ProfileCatalogFinding
}

func runCatalogCase(t *testing.T, c catalogCase) {
	t.Helper()
	globalDir := catalogHome(t, c.active, c.fileProfiles...)
	before := catalogView(t, globalDir, c.before)
	after := catalogView(t, globalDir, c.after)
	got := CheckProfileCatalog(after, before, catalogOrigins(t, globalDir), c.projectActive, c.agents)
	if !reflect.DeepEqual(got, c.want) {
		t.Fatalf("findings:\n got  %+v\n want %+v", got, c.want)
	}
}

func TestCheckProfileCatalog_Fixtures(t *testing.T) {
	cases := []catalogCase{
		{
			// Fixture 1 (= settings-lead's case): a built-in-only profile in
			// use, replaced by a non-empty stored set.
			name: "builtin profile in use", active: "gke", fileProfiles: []string{"gke", "local-nfs"},
			after: []string{"gke", "local-nfs"}, agents: map[string]int{"local": 3},
			want: []ProfileCatalogFinding{
				{Kind: ProfileFindingAgentSavedProfile, Profile: "local", Agents: 3, Source: ProfileSourceBuiltin},
				{Kind: ProfileFindingDroppedUnused, Profile: "remote", Source: ProfileSourceBuiltin},
			},
		},
		{
			// Fixture 1v: the stored set already replaced the built-ins (the
			// community hub today): pre-existing, still labelled builtin.
			name: "builtin profile already gone", active: "gke", fileProfiles: []string{"gke", "local-nfs"},
			before: []string{"gke", "local-nfs"}, after: []string{"gke", "local-nfs"}, agents: map[string]int{"local": 3},
			want: []ProfileCatalogFinding{
				{Kind: ProfileFindingAgentSavedProfile, Profile: "local", Agents: 3, Source: ProfileSourceBuiltin, PreExisting: true},
			},
		},
		{
			// Fixture 2: built-in profiles nobody uses vanish: dropped, no use kind.
			name: "builtin profiles unused", active: "gke", fileProfiles: []string{"gke", "local-nfs"},
			after: []string{"gke", "local-nfs"},
			want: []ProfileCatalogFinding{
				{Kind: ProfileFindingDroppedUnused, Profile: "local", Source: ProfileSourceBuiltin},
				{Kind: ProfileFindingDroppedUnused, Profile: "remote", Source: ProfileSourceBuiltin},
			},
		},
		{
			// Fixture 3a: active_profile is built-in only, two profiles stored.
			name: "builtin active profile, two stored", active: "local", fileProfiles: []string{"gke", "local-nfs"},
			after: []string{"gke", "local-nfs"}, agents: map[string]int{"": 2},
			want: []ProfileCatalogFinding{
				{Kind: ProfileFindingActiveProfile, Profile: "local", Agents: 2, Source: ProfileSourceBuiltin},
				{Kind: ProfileFindingDroppedUnused, Profile: "remote", Source: ProfileSourceBuiltin},
			},
		},
		{
			// Fixture 3b: the same with exactly one stored profile: the
			// overlay's fallback, reported as information.
			name: "builtin active profile, one stored", active: "local", fileProfiles: []string{"gke", "local-nfs"},
			after: []string{"gke"}, agents: map[string]int{"": 2},
			want: []ProfileCatalogFinding{
				{Kind: ProfileFindingActiveProfileFallback, Profile: "local", Agents: 2, Source: ProfileSourceBuiltin},
				{Kind: ProfileFindingDroppedUnused, Profile: "local-nfs", Source: ProfileSourceFile},
				{Kind: ProfileFindingDroppedUnused, Profile: "remote", Source: ProfileSourceBuiltin},
			},
		},
		{
			// Before resolves active_profile only through the single-stored
			// fallback; after stores two profiles without it: a change.
			name: "active profile only via fallback before", active: "local", fileProfiles: []string{"gke", "local-nfs"},
			before: []string{"gke"}, after: []string{"gke", "local-nfs"}, agents: map[string]int{"": 2},
			want: []ProfileCatalogFinding{
				{Kind: ProfileFindingActiveProfile, Profile: "local", Agents: 2, Source: ProfileSourceBuiltin},
			},
		},
		{
			// Fixture 4: a project's activeProfile is missing.
			name: "project active profile missing", active: "gke", fileProfiles: []string{"gke", "local-nfs"},
			after: []string{"gke", "local-nfs"}, projectActive: map[string]string{"p1": "local", "p2": ""},
			want: []ProfileCatalogFinding{
				{Kind: ProfileFindingDroppedUnused, Profile: "remote", Source: ProfileSourceBuiltin},
				{Kind: ProfileFindingProjectActiveProfile, Profile: "local", Project: "p1", Source: ProfileSourceBuiltin},
			},
		},
		{
			// Fixture 4 with one stored profile: the project gets no
			// fallback; the hub active_profile does.
			name: "project active profile missing, one stored", active: "local-nfs", fileProfiles: []string{"gke", "local-nfs"},
			after: []string{"gke"}, projectActive: map[string]string{"p1": "local"},
			want: []ProfileCatalogFinding{
				{Kind: ProfileFindingActiveProfileFallback, Profile: "local-nfs", Source: ProfileSourceFile},
				{Kind: ProfileFindingDroppedUnused, Profile: "remote", Source: ProfileSourceBuiltin},
				{Kind: ProfileFindingProjectActiveProfile, Profile: "local", Project: "p1", Source: ProfileSourceBuiltin},
			},
		},
		{
			// Fixture 5: a stored set goes away (seeded row emptied): agents on
			// a stored profile fail against the files plus built-ins.
			name: "stored to unmanaged", active: "local",
			before: []string{"gke", "old"}, agents: map[string]int{"gke": 1},
			want: []ProfileCatalogFinding{
				{Kind: ProfileFindingAgentSavedProfile, Profile: "gke", Agents: 1, Source: ProfileSourceStored},
				{Kind: ProfileFindingDroppedUnused, Profile: "old", Source: ProfileSourceStored},
			},
		},
		{
			// Neither side stored: still checked; a broken reference is pre-existing.
			name: "neither stored", active: "gke", fileProfiles: []string{"gke"},
			agents: map[string]int{"typo": 1},
			want: []ProfileCatalogFinding{
				{Kind: ProfileFindingAgentSavedProfile, Profile: "typo", Agents: 1, Source: ProfileSourceNone, PreExisting: true},
			},
		},
		{
			// A file entry wins over the built-in of the same name.
			name: "file source", active: "gke", fileProfiles: []string{"gke", "local", "local-nfs"},
			after: []string{"gke", "local-nfs"}, agents: map[string]int{"local": 1},
			want: []ProfileCatalogFinding{
				{Kind: ProfileFindingAgentSavedProfile, Profile: "local", Agents: 1, Source: ProfileSourceFile},
				{Kind: ProfileFindingDroppedUnused, Profile: "remote", Source: ProfileSourceBuiltin},
			},
		},
		{
			// A profile only the running hub's stored set had.
			name: "stored source", active: "gke", fileProfiles: []string{"gke"},
			before: []string{"gke", "old"}, after: []string{"gke"}, agents: map[string]int{"old": 1},
			want: []ProfileCatalogFinding{
				{Kind: ProfileFindingAgentSavedProfile, Profile: "old", Agents: 1, Source: ProfileSourceStored},
			},
		},
		{
			// The lead's fixture: a reference in neither set is a pre-existing warning.
			name: "none source, pre-existing", active: "gke", fileProfiles: []string{"gke"},
			after: []string{"gke"}, agents: map[string]int{"typo": 1},
			want: []ProfileCatalogFinding{
				{Kind: ProfileFindingAgentSavedProfile, Profile: "typo", Agents: 1, Source: ProfileSourceNone, PreExisting: true},
				{Kind: ProfileFindingDroppedUnused, Profile: "local", Source: ProfileSourceBuiltin},
				{Kind: ProfileFindingDroppedUnused, Profile: "remote", Source: ProfileSourceBuiltin},
			},
		},
		{
			// The hub active_profile in neither set: pre-existing.
			name: "active profile in neither set", active: "typo", fileProfiles: []string{"gke"},
			after: []string{"gke", "x"}, agents: map[string]int{"": 2},
			want: []ProfileCatalogFinding{
				{Kind: ProfileFindingActiveProfile, Profile: "typo", Agents: 2, Source: ProfileSourceNone, PreExisting: true},
				{Kind: ProfileFindingDroppedUnused, Profile: "local", Source: ProfileSourceBuiltin},
				{Kind: ProfileFindingDroppedUnused, Profile: "remote", Source: ProfileSourceBuiltin},
			},
		},
		{
			// Agents and projects whose profiles resolve give nothing.
			name: "everything resolves", active: "gke", fileProfiles: []string{"gke", "local-nfs"},
			after:         []string{"gke", "local-nfs", "local", "remote"},
			projectActive: map[string]string{"p1": "local-nfs"}, agents: map[string]int{"gke": 4, "local": 1, "": 3},
			want: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { runCatalogCase(t, c) })
	}
}

// With the running hub's overlay installed globally, ProfileCatalogView
// ignores it: an empty candidate stored set leaves the files and built-ins,
// and a single-profile fallback keeps the configured active_profile.
func TestProfileCatalogView_IgnoresGlobalOverlay(t *testing.T) {
	globalDir := catalogHome(t, "local", "gke", "local-nfs")
	running := NewSettingsOverlay()
	running.Update(nil, storedProfiles([]string{"gke"}), nil, "")
	SetGlobalSettingsOverlay(running) // catalogHome restores the previous overlay

	before := catalogView(t, globalDir, []string{"gke"})
	after := catalogView(t, globalDir, []string{})
	for _, name := range []string{"gke", "local-nfs", "local", "remote"} {
		if !hasProfile(after, name) {
			t.Fatalf("after with an empty stored set lacks %q: the global overlay was applied", name)
		}
	}
	if before.activeProfileFallbackFrom == nil || *before.activeProfileFallbackFrom != "local" {
		t.Fatalf("before lost the configured active_profile: fallback from %v, want \"local\"", before.activeProfileFallbackFrom)
	}
	// The change from the unmanaged view to the one-profile stored set.
	got := CheckProfileCatalog(before, after, catalogOrigins(t, globalDir), nil, map[string]int{"": 2})
	want := []ProfileCatalogFinding{
		{Kind: ProfileFindingActiveProfileFallback, Profile: "local", Agents: 2, Source: ProfileSourceBuiltin},
		{Kind: ProfileFindingDroppedUnused, Profile: "local-nfs", Source: ProfileSourceFile},
		{Kind: ProfileFindingDroppedUnused, Profile: "remote", Source: ProfileSourceBuiltin},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("findings with a global overlay installed:\n got  %+v\n want %+v", got, want)
	}
}

// A nil before: origins still label the source, nothing is pre-existing or
// dropped.
func TestCheckProfileCatalog_NilBefore(t *testing.T) {
	globalDir := catalogHome(t, "gke", "gke", "local-nfs")
	after := catalogView(t, globalDir, []string{"gke", "local-nfs"})
	got := CheckProfileCatalog(after, nil, catalogOrigins(t, globalDir), nil, map[string]int{"local": 1})
	want := []ProfileCatalogFinding{{Kind: ProfileFindingAgentSavedProfile, Profile: "local", Agents: 1, Source: ProfileSourceBuiltin}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("findings:\n got  %+v\n want %+v", got, want)
	}
}

// A versioned hierarchy labels file and built-in profiles; a file entry
// wins over the built-in of the same name.
func TestProfileOrigins_Versioned(t *testing.T) {
	globalDir := catalogHome(t, "gke", "gke", "local")
	got := catalogOrigins(t, globalDir)
	want := map[string]ProfileSource{"gke": ProfileSourceFile, "local": ProfileSourceFile, "remote": ProfileSourceBuiltin}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("origins = %v, want %v", got, want)
	}
}

// A legacy hierarchy labels every profile "settings".
func TestProfileOrigins_Legacy(t *testing.T) {
	old := GetGlobalSettingsOverlay()
	t.Cleanup(func() { SetGlobalSettingsOverlay(old) })
	SetGlobalSettingsOverlay(nil)
	home := t.TempDir()
	t.Setenv("HOME", home)
	projectDir := filepath.Join(home, "my-project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(`
active_profile: legacy-profile
harnesses:
  gemini:
    image: example.com/gemini:latest
    user: scion
profiles:
  legacy-profile:
    runtime: docker
`), 0o644))

	got, err := ProfileOrigins(projectDir)
	require.NoError(t, err)
	if got["legacy-profile"] != ProfileSourceSettings {
		t.Fatalf("origins = %v, want legacy-profile labelled settings", got)
	}
	for name, src := range got {
		if src != ProfileSourceSettings {
			t.Fatalf("legacy origin %q = %q, want settings", name, src)
		}
	}
}

// The finding carries names, counts and labels only: exactly these fields.
func TestProfileCatalogFinding_Fields(t *testing.T) {
	typ := reflect.TypeOf(ProfileCatalogFinding{})
	var names []string
	for i := 0; i < typ.NumField(); i++ {
		names = append(names, typ.Field(i).Name)
	}
	sort.Strings(names)
	want := []string{"Agents", "Kind", "PreExisting", "Profile", "Project", "Source"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("ProfileCatalogFinding fields = %v, want %v", names, want)
	}
}
