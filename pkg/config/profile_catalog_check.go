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

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/rawbytes"
	"github.com/knadh/koanf/v2"
)

// The profile catalog check (ptone/scion#5057, shared with ptone/scion#5015):
// which profile references stop resolving when the effective profile set
// changes, for example when a non-empty hub-stored profile set replaces the
// settings files' profiles and the built-in defaults.

// ProfileCatalogFindingKind names what a ProfileCatalogFinding is about.
type ProfileCatalogFindingKind string

const (
	// ProfileFindingActiveProfile: the hub's active_profile does not
	// resolve. Agents without a saved profile dispatch on it.
	ProfileFindingActiveProfile ProfileCatalogFindingKind = "active_profile"
	// ProfileFindingProjectActiveProfile: a project's activeProfile does
	// not resolve. It is copied onto new agents as an explicit profile.
	ProfileFindingProjectActiveProfile ProfileCatalogFindingKind = "project_active_profile"
	// ProfileFindingAgentSavedProfile: agents' saved profile does not
	// resolve.
	ProfileFindingAgentSavedProfile ProfileCatalogFindingKind = "agent_saved_profile"
	// ProfileFindingDroppedUnused: a profile in the before set is not in
	// the after set, and nothing references it.
	ProfileFindingDroppedUnused ProfileCatalogFindingKind = "dropped_unused_profile"
	// ProfileFindingActiveProfileFallback: exactly one profile is stored
	// and active_profile is not it, so the overlay resolves active_profile
	// to the single stored profile.
	ProfileFindingActiveProfileFallback ProfileCatalogFindingKind = "active_profile_fallback"
)

// ProfileSource says where a profile name resolved before the change.
type ProfileSource string

const (
	// ProfileSourceStored: one of the hub's stored profiles.
	ProfileSourceStored ProfileSource = "stored"
	// ProfileSourceFile: defined by a settings file (global or project)
	// or the SCION_ environment.
	ProfileSourceFile ProfileSource = "file"
	// ProfileSourceBuiltin: defined only by the embedded default settings.
	ProfileSourceBuiltin ProfileSource = "builtin"
	// ProfileSourceSettings: a legacy settings hierarchy, where a file
	// profile and a built-in default are not told apart.
	ProfileSourceSettings ProfileSource = "settings"
	// ProfileSourceNone: not defined before either.
	ProfileSourceNone ProfileSource = "none"
)

// ProfileCatalogFinding is one profile reference the after set leaves
// unresolved, one unused profile it drops, or the single-profile fallback.
// It carries names, a count and labels only, never settings values.
type ProfileCatalogFinding struct {
	Kind ProfileCatalogFindingKind
	// Profile is the profile name ("" when active_profile is unset).
	Profile string
	// Project is the project ID, for project_active_profile only.
	Project string
	// Agents is, for agent_saved_profile, the agents whose saved profile is
	// Profile; for active_profile and active_profile_fallback, the agents
	// with no saved profile, which dispatch on active_profile; else 0.
	Agents int
	// Source is where Profile resolved before the change.
	Source ProfileSource
	// PreExisting is set when the reference resolves in neither the before
	// nor the after set: the change does not cause it, so a caller reports
	// it as a warning rather than a blocking problem. It relies on before
	// being what the running system resolves today.
	PreExisting bool
}

// CheckProfileCatalog reports the profile references that do not resolve in
// after, the profiles that before has and after drops unused, and the
// single-stored-profile fallback of active_profile.
//
// after and before are effective settings as the system resolves them:
// LoadEffectiveSettings, then the hub-stored profile overlay applied
// (SettingsOverlay.Apply). before is what the running system resolves today;
// after is what it will resolve, for example with a candidate or re-synced
// stored set. A caller evaluating a candidate stored set applies it through a
// throwaway overlay (NewSettingsOverlay, Update, Apply) and never installs
// that overlay as the global one; Apply may log the single-profile fallback.
// A nil before is allowed: Source then comes from origins only (never
// "stored"), PreExisting is false, and nothing is reported as dropped.
//
// origins labels the pre-overlay profiles (ProfileOrigins). projectActive
// maps a project ID to its activeProfile ("" values are ignored). agents maps
// a saved profile name to its number of agents; the "" key counts agents with
// no saved profile. The caller decides which agents count (for example, only
// those on the broker the settings apply to).
//
// Every reference is checked against after, whether or not either side has
// a stored set. A reference that resolved in before is a change the caller
// should block on; one that resolves in neither is PreExisting. Explicit
// names (saved profiles, project activeProfiles) resolve by membership only;
// only active_profile gets the overlay's single-stored-profile fallback,
// read from the overlay's own record. The result is sorted by Kind, Profile
// and Project.
func CheckProfileCatalog(after, before *VersionedSettings, origins map[string]ProfileSource,
	projectActive map[string]string, agents map[string]int) []ProfileCatalogFinding {
	if after == nil {
		return nil
	}
	source := func(name string) ProfileSource {
		if name == "" {
			return ProfileSourceNone
		}
		if before != nil && before.storedProfileNames != nil && hasProfile(before, name) {
			return ProfileSourceStored
		}
		if s, ok := origins[name]; ok {
			return s
		}
		return ProfileSourceNone
	}
	preExisting := func(name string) bool {
		return before != nil && !hasProfile(before, name)
	}

	var out []ProfileCatalogFinding
	referenced := map[string]bool{}

	// The hub's active_profile.
	configured := configuredActiveProfile(after)
	referenced[configured] = true
	if before != nil {
		referenced[configuredActiveProfile(before)] = true
		referenced[before.ActiveProfile] = true
	}
	switch {
	case after.activeProfileFallbackFrom != nil:
		out = append(out, ProfileCatalogFinding{Kind: ProfileFindingActiveProfileFallback,
			Profile: configured, Agents: agents[""], Source: source(configured)})
	case !activeProfileResolves(after):
		out = append(out, ProfileCatalogFinding{Kind: ProfileFindingActiveProfile,
			Profile: configured, Agents: agents[""], Source: source(configured),
			PreExisting: before != nil && !activeProfileResolves(before)})
	}

	// Projects' activeProfiles.
	for project, name := range projectActive {
		if name == "" {
			continue
		}
		referenced[name] = true
		if !hasProfile(after, name) {
			out = append(out, ProfileCatalogFinding{Kind: ProfileFindingProjectActiveProfile,
				Profile: name, Project: project, Source: source(name), PreExisting: preExisting(name)})
		}
	}

	// Agents' saved profiles.
	for name, n := range agents {
		if name == "" || n <= 0 {
			continue
		}
		referenced[name] = true
		if !hasProfile(after, name) {
			out = append(out, ProfileCatalogFinding{Kind: ProfileFindingAgentSavedProfile,
				Profile: name, Agents: n, Source: source(name), PreExisting: preExisting(name)})
		}
	}

	// Profiles before has and after drops, with no reference.
	if before != nil {
		for name := range before.Profiles {
			if !hasProfile(after, name) && !referenced[name] {
				out = append(out, ProfileCatalogFinding{Kind: ProfileFindingDroppedUnused,
					Profile: name, Source: source(name)})
			}
		}
	}

	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Profile != b.Profile {
			return a.Profile < b.Profile
		}
		return a.Project < b.Project
	})
	return out
}

// hasProfile reports whether vs defines profile name.
func hasProfile(vs *VersionedSettings, name string) bool {
	_, ok := vs.Profiles[name]
	return ok
}

// configuredActiveProfile is the active_profile vs was configured with: the
// value before the overlay's single-stored-profile fallback rewrote it.
func configuredActiveProfile(vs *VersionedSettings) string {
	if vs.activeProfileFallbackFrom != nil {
		return *vs.activeProfileFallbackFrom
	}
	return vs.ActiveProfile
}

// activeProfileResolves reports whether vs's active_profile names one of its
// profiles: the overlay recorded no ActiveProfileError, and the (possibly
// fallback) ActiveProfile is in the profile set. Membership decides, not
// error text.
func activeProfileResolves(vs *VersionedSettings) bool {
	return vs.activeProfileErr == nil && hasProfile(vs, vs.ActiveProfile)
}

// ProfileOrigins labels the profile names of projectPath's settings
// hierarchy before any hub-stored overlay: ProfileSourceFile for a name a
// settings file layer (global or project) or the SCION_ environment
// defines, ProfileSourceBuiltin for a name only the embedded defaults
// define. It takes the same branch as LoadEffectiveSettings. On a versioned
// hierarchy it uses LoadVersionedSettings' own layers
// (loadVersionedSettingsKoanf), once with and once without the embedded
// defaults. On a legacy hierarchy every profile is ProfileSourceSettings: the
// legacy adapter has no separable defaults layer. Profiles that reach the hub
// only through its stored set (server config, SCION_SEED_*) are not
// labelled here; CheckProfileCatalog labels them "stored" from before.
func ProfileOrigins(projectPath string) (map[string]ProfileSource, error) {
	out := map[string]ProfileSource{}
	if hasVersioned, _ := detectHierarchyFormat(projectPath); !hasVersioned {
		legacy, err := LoadSettingsKoanf(projectPath)
		if err != nil {
			return nil, fmt.Errorf("loading legacy settings: %w", err)
		}
		vs, _ := AdaptLegacySettings(legacy)
		for name := range vs.Profiles {
			out[name] = ProfileSourceSettings
		}
		return out, nil
	}
	if data, err := GetDefaultSettingsDataYAML(); err == nil {
		defaults := koanf.New(".")
		if err := defaults.Load(rawbytes.Provider(data), yaml.Parser()); err != nil {
			return nil, fmt.Errorf("loading the default settings: %w", err)
		}
		for _, name := range defaults.MapKeys("profiles") {
			out[name] = ProfileSourceBuiltin
		}
	}
	files, err := loadVersionedSettingsKoanf(projectPath, false)
	if err != nil {
		return nil, fmt.Errorf("loading versioned settings: %w", err)
	}
	for _, name := range files.MapKeys("profiles") {
		out[name] = ProfileSourceFile
	}
	return out, nil
}
