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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
)

// SettingsOverlay holds DB-backed settings that override file-loaded values.
// In hosted/HA mode the hub's operational settings system (Layer-1) persists
// runtimes, profiles, harness_configs, and image_registry to the database.
// LoadEffectiveSettings always reads from disk, so the broker (which calls
// LoadEffectiveSettings on every agent dispatch) never sees DB changes.
//
// This overlay bridges that gap for co-located mode (hub + broker in the same
// process, e.g. Cloud Run): ApplySnapshot updates it, LoadEffectiveSettings
// merges it after loading from disk. DB values win over file values.
//
// For standalone brokers (separate process), the overlay is unused; those
// brokers would need an API-based settings fetch (future work).
type SettingsOverlay struct {
	mu             sync.RWMutex
	runtimes       map[string]V1RuntimeConfig
	profiles       map[string]V1ProfileConfig
	harnessConfigs map[string]HarnessConfigEntry
	// imageRegistry holds the hub's image_registry layers
	// (UpdateImageRegistryInputs); nil until fed.
	imageRegistry *ImageRegistryHubInputs
	active        bool // true when any field has been set

	// fallbackLogMu guards fallbackLogged, the (active_profile, chosen
	// profile) pairs logSingleProfileFallback has logged since the last
	// Apply that resolved without the fallback (at most
	// maxFallbackLogKeys). It is separate from mu because Apply holds mu
	// only for reading.
	fallbackLogMu  sync.Mutex
	fallbackLogged map[string]struct{}

	// Built-in retention (ptone/scion#5113; settings_overlay_retain.go).
	// retain, conservative and refs are guarded by mu; retained has its own
	// mutex because Apply holds mu only for reading.
	retain           bool
	conservative     bool
	refs             ProfileReferences
	retained         retainState
	builtinsOverride *builtinCatalog // tests only; nil uses the embedded defaults
}

// maxFallbackLogKeys caps the set of logged single-stored-profile fallback
// pairs; when it is full the set is cleared, so memory stays bounded and a
// pair is at worst logged again.
const maxFallbackLogKeys = 16

// globalOverlay is the process-wide settings overlay. It is nil by default;
// SetGlobalSettingsOverlay installs it. Only the hub server should call
// SetGlobalSettingsOverlay, and only once, during startup in co-located mode.
var globalOverlay *SettingsOverlay

// SetGlobalSettingsOverlay installs a process-wide settings overlay.
// LoadEffectiveSettings will merge values from this overlay after loading
// from disk. Pass nil to remove the overlay.
func SetGlobalSettingsOverlay(o *SettingsOverlay) {
	globalOverlay = o
}

// GetGlobalSettingsOverlay returns the current process-wide overlay, or nil.
func GetGlobalSettingsOverlay() *SettingsOverlay {
	return globalOverlay
}

// NewSettingsOverlay creates an empty overlay. It is inactive until
// at least one Update call sets values.
func NewSettingsOverlay() *SettingsOverlay {
	return &SettingsOverlay{}
}

// Update sets the overlay's fields. Nil maps are treated as "no change".
// An empty (non-nil) map clears the section: a stored catalog with no
// entries means the section is not managed in the hub, the same as unset,
// so Apply keeps the settings file's map for it (ptone/scion#5007).
// image_registry is fed separately (UpdateImageRegistryInputs).
func (o *SettingsOverlay) Update(
	runtimes map[string]V1RuntimeConfig,
	profiles map[string]V1ProfileConfig,
	harnessConfigs map[string]HarnessConfigEntry,
) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if runtimes != nil {
		o.runtimes = nonEmptyOrNil(cloneRuntimes(runtimes))
	}
	if profiles != nil {
		o.profiles = nonEmptyOrNil(cloneProfiles(profiles))
	}
	if harnessConfigs != nil {
		o.harnessConfigs = nonEmptyOrNil(cloneHarnessConfigs(harnessConfigs))
	}
	o.active = true
}

// UpdateImageRegistryInputs sets the hub's image_registry layers, which
// Apply hands to the loaded settings (VersionedSettings.ImageRegistryLayers),
// and activates the overlay. The hub computes them with the accessor it
// resolves dispatches with (ptone/scion#3916), and calls this whether or not
// any catalog section is stored, so a managed endpoints value applies with
// no stored catalog. An empty profile resolves through the broker's own
// live settings (vs.ActiveProfile).
func (o *SettingsOverlay) UpdateImageRegistryInputs(in ImageRegistryHubInputs) {
	o.mu.Lock()
	defer o.mu.Unlock()
	c := in.Clone()
	o.imageRegistry = &c
	o.active = true
}

// nonEmptyOrNil returns m, or nil when m has no entries.
func nonEmptyOrNil[V any](m map[string]V) map[string]V {
	if len(m) == 0 {
		return nil
	}
	return m
}

// Apply merges overlay values into a VersionedSettings.
// When built-in retention is enabled (SetProfileReferences), referenced
// built-in profiles and the built-in runtimes effective profiles need are
// kept alongside the stored ones (see settings_overlay_retain.go). DB values
// replace file values for each field that has been set in the overlay
// with at least one entry; an empty stored map leaves the file's map in
// place (see Update). Runtimes and profiles are handled independently, so
// stored profiles resolve against the file's runtimes when no runtimes are
// stored, and the active_profile resolution against stored profiles (and
// its errors) applies only when stored profiles exist.
// Called by LoadEffectiveSettings after loading from disk.
func (o *SettingsOverlay) Apply(vs *VersionedSettings) {
	if o == nil || vs == nil {
		return
	}
	o.mu.RLock()
	defer o.mu.RUnlock()

	if !o.active {
		return
	}

	if len(o.runtimes) > 0 {
		vs.Runtimes = cloneRuntimes(o.runtimes)
	}
	if len(o.profiles) > 0 {
		vs.Profiles = cloneProfiles(o.profiles)
		stored := sortedNames(o.profiles)
		var retained []string
		if o.retain {
			retained = o.retainProfiles(vs, vs.ActiveProfile)
		}
		if configured, chosen := resolveOverlaidActiveProfile(vs, stored, retained); chosen != "" {
			o.logSingleProfileFallback(configured, chosen)
		} else {
			o.resetSingleProfileFallbackLog()
		}
	}
	if o.retain {
		vs.retainedRuntimeNames = o.retainRuntimes(vs)
		o.logRetention()
	}
	if len(o.harnessConfigs) > 0 {
		vs.HarnessConfigs = cloneHarnessConfigs(o.harnessConfigs)
	}
	if o.imageRegistry != nil {
		c := o.imageRegistry.Clone()
		vs.imageRegistryHub = &c
	}
}

// ActiveProfileError reports that the default profile (active_profile)
// cannot be resolved after the overlay replaced the profiles map: the
// configured name is not one of the stored profiles, and there is not
// exactly one stored profile to use instead (see
// resolveOverlaidActiveProfile). It is returned only
// when a caller needs the default profile (ResolveRuntime with an empty
// profile name), never when settings are loaded, so a broker whose agents
// all carry their own profile still starts and runs.
type ActiveProfileError struct {
	// Configured is the active_profile value from the settings file; empty
	// when none is set.
	Configured string
	// Available is the sorted list of the hub's stored profile names.
	Available []string
	// Retained is the sorted list of built-in profiles retained beside
	// them (ptone/scion#5113); they resolve by name too.
	Retained []string
}

func (e *ActiveProfileError) Error() string {
	available := joinAvailableProfiles(e.Available, e.Retained)
	configured := fmt.Sprintf("%q", e.Configured)
	if e.Configured == "" {
		configured = `"" (unset)`
	}
	return fmt.Sprintf("active_profile %s is not one of the hub's stored profiles (available profiles: %s); set active_profile to one of them, or give the agent an explicit profile",
		configured, available)
}

// StoredProfileNotFoundError reports that a profile named explicitly (an
// agent's requested or saved profile) is not one of the hub's stored
// profiles, after the overlay replaced the profiles map. Its text keeps the
// "profile %q not found" form and adds the available profiles.
type StoredProfileNotFoundError struct {
	// Requested is the profile name that was asked for.
	Requested string
	// Available is the sorted list of the hub's stored profile names.
	Available []string
	// Retained is the sorted list of built-in profiles retained beside
	// them (ptone/scion#5113); they resolve by name too.
	Retained []string
}

func (e *StoredProfileNotFoundError) Error() string {
	return fmt.Sprintf("profile %q not found in the hub's stored profiles (available profiles: %s)",
		e.Requested, joinAvailableProfiles(e.Available, e.Retained))
}

// StoredProfileRuntimeNotFoundError reports that a profile resolved from
// the hub's stored profiles (named, saved, or the default profile) names a
// runtime that the runtimes, after the overlay, do not define
// (ptone/scion#4889). Stored settings are authoritative on a broker that
// applies the overlay, so a dispatch with this profile must fail instead of
// falling back to the broker's default runtime. Its text keeps the
// "runtime %q not found for profile %q" form and adds the available
// runtimes.
type StoredProfileRuntimeNotFoundError struct {
	// Profile is the resolved profile name.
	Profile string
	// Runtime is the runtimes key the profile names.
	Runtime string
	// Available is the sorted list of runtime names that are defined.
	Available []string
}

func (e *StoredProfileRuntimeNotFoundError) Error() string {
	available := "none"
	if len(e.Available) > 0 {
		available = strings.Join(e.Available, ", ")
	}
	return fmt.Sprintf("runtime %q not found for profile %q in the hub's stored settings (available runtimes: %s)",
		e.Runtime, e.Profile, available)
}

// IsStoredProfileError reports whether err is one of the typed errors
// ResolveRuntime returns when the hub's stored profiles do not resolve:
// *ActiveProfileError, *StoredProfileNotFoundError or
// *StoredProfileRuntimeNotFoundError.
func IsStoredProfileError(err error) bool {
	var ape *ActiveProfileError
	var spe *StoredProfileNotFoundError
	var sre *StoredProfileRuntimeNotFoundError
	return errors.As(err, &ape) || errors.As(err, &spe) || errors.As(err, &sre)
}

// joinAvailableProfiles lists the stored profile names, then the retained
// built-in ones, as "a, b; retained built-in: c".
func joinAvailableProfiles(names, retained []string) string {
	out := "none"
	if len(names) > 0 {
		out = strings.Join(names, ", ")
	}
	if len(retained) > 0 {
		out += "; retained built-in: " + strings.Join(retained, ", ")
	}
	return out
}

// resolveOverlaidActiveProfile decides the default profile once the overlay
// has replaced vs.Profiles with the stored profiles (plus any retained
// built-in profiles), and records the stored and retained names so that
// ResolveRuntime can name them when a requested profile is missing. stored
// and retained are sorted and disjoint. The order is
// (ptone/scion#5113):
//
//  1. the file's active_profile, when it is a stored profile;
//  2. otherwise, when exactly one profile is stored, that profile:
//     vs.ActiveProfile is set to it, and its name is returned as chosen
//     (with the file value as configured) so the caller can log the choice.
//     Retained profiles are not counted, so with one stored profile the
//     default stays on it;
//  3. otherwise, the file's active_profile, when it is a retained built-in
//     profile;
//  4. otherwise, vs records an ActiveProfileError, which ResolveRuntime
//     returns when the default profile is needed; vs.ActiveProfile is left
//     unchanged.
//
// active_profile has no hub (DB) setting today. If one is ever added, its
// value would take precedence over the file's value here.
func resolveOverlaidActiveProfile(vs *VersionedSettings, stored, retained []string) (configured, chosen string) {
	vs.storedProfileNames = stored
	vs.retainedProfileNames = retained
	vs.activeProfileErr = nil
	vs.activeProfileFallbackFrom = nil
	if containsString(stored, vs.ActiveProfile) {
		return "", ""
	}
	// EffectiveDefaultProfile is this step, shared with the hub's
	// image_registry resolution for agents with no profile.
	if effective := EffectiveDefaultProfile(vs.ActiveProfile, stored); effective != vs.ActiveProfile {
		configured = vs.ActiveProfile
		vs.activeProfileFallbackFrom = &configured
		vs.ActiveProfile = effective
		return configured, effective
	}
	if containsString(retained, vs.ActiveProfile) {
		return "", ""
	}
	vs.activeProfileErr = &ActiveProfileError{Configured: vs.ActiveProfile, Available: stored, Retained: retained}
	return "", ""
}

// ConfiguredActiveProfile returns active_profile as the settings files
// configure it: ActiveProfile, except when the hub settings overlay's
// single-stored-profile fallback (see resolveOverlaidActiveProfile)
// replaced it, in which case the value it replaced. Callers that act on the
// configured selection rather than on dispatch resolution (the Hub's
// system-runtime endpoint) use this, so the fallback never redirects them.
func (vs *VersionedSettings) ConfiguredActiveProfile() string {
	if vs == nil {
		return ""
	}
	if vs.activeProfileFallbackFrom != nil {
		return *vs.activeProfileFallbackFrom
	}
	return vs.ActiveProfile
}

// logSingleProfileFallback logs that the only stored profile is used as the
// default profile because active_profile does not name a stored profile.
// Apply runs on every settings load, so each (configured, chosen) pair is
// logged once until an Apply resolves without the fallback
// (resetSingleProfileFallbackLog), after which it is logged again.
func (o *SettingsOverlay) logSingleProfileFallback(configured, chosen string) {
	key := configured + "\x00" + chosen
	o.fallbackLogMu.Lock()
	defer o.fallbackLogMu.Unlock()
	if _, ok := o.fallbackLogged[key]; ok {
		return
	}
	if o.fallbackLogged == nil || len(o.fallbackLogged) >= maxFallbackLogKeys {
		o.fallbackLogged = make(map[string]struct{})
	}
	o.fallbackLogged[key] = struct{}{}
	if configured == "" {
		configured = "(unset)"
	}
	slog.Info("Default profile set to the only stored profile: active_profile does not name one of the hub's stored profiles",
		"profile", chosen, "active_profile", configured)
}

// resetSingleProfileFallbackLog forgets the logged fallback pairs once an
// Apply resolves without the fallback, so a later fallback is logged again.
func (o *SettingsOverlay) resetSingleProfileFallbackLog() {
	o.fallbackLogMu.Lock()
	defer o.fallbackLogMu.Unlock()
	if len(o.fallbackLogged) > 0 {
		o.fallbackLogged = nil
	}
}

// Profiles returns a deep copy of the overlay's profiles map. Returns nil if
// the overlay has not been activated or no profiles have been stored (an
// empty stored map counts as none; see Update).
func (o *SettingsOverlay) Profiles() map[string]V1ProfileConfig {
	if o == nil {
		return nil
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	if !o.active || o.profiles == nil {
		return nil
	}
	return cloneProfiles(o.profiles)
}

// deepCloneJSON performs a deep copy of src into dst via JSON round-trip.
// This is intentionally generic so that new fields added to V1RuntimeConfig,
// V1ProfileConfig, or HarnessConfigEntry are automatically deep-copied
// without needing per-field clone logic. The types are JSON-serializable
// by design (they carry json struct tags for API/storage).
func deepCloneJSON[T any](src T) T {
	data, err := json.Marshal(src)
	if err != nil {
		// All settings types are JSON-serializable; a marshal failure
		// here indicates a programming error, not a runtime condition.
		panic("settings_overlay: json.Marshal failed: " + err.Error())
	}
	var dst T
	if err := json.Unmarshal(data, &dst); err != nil {
		panic("settings_overlay: json.Unmarshal failed: " + err.Error())
	}
	return dst
}

// cloneRuntimes returns a deep copy of a runtimes map.
func cloneRuntimes(src map[string]V1RuntimeConfig) map[string]V1RuntimeConfig {
	if src == nil {
		return nil
	}
	return deepCloneJSON(src)
}

// cloneProfiles returns a deep copy of a profiles map.
func cloneProfiles(src map[string]V1ProfileConfig) map[string]V1ProfileConfig {
	if src == nil {
		return nil
	}
	return deepCloneJSON(src)
}

// cloneHarnessConfigs returns a deep copy of a harness configs map.
func cloneHarnessConfigs(src map[string]HarnessConfigEntry) map[string]HarnessConfigEntry {
	if src == nil {
		return nil
	}
	return deepCloneJSON(src)
}
