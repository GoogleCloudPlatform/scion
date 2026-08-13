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
	"os"
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
	imageRegistry  string
	active         bool // true when any field has been set
}

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

// Update sets the overlay's fields. Nil maps are treated as "no change" —
// pass an empty (non-nil) map to clear a section. imageRegistry is always
// applied (empty string clears it).
func (o *SettingsOverlay) Update(
	runtimes map[string]V1RuntimeConfig,
	profiles map[string]V1ProfileConfig,
	harnessConfigs map[string]HarnessConfigEntry,
	imageRegistry string,
) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if runtimes != nil {
		o.runtimes = cloneRuntimes(runtimes)
	}
	if profiles != nil {
		o.profiles = cloneProfiles(profiles)
	}
	if harnessConfigs != nil {
		o.harnessConfigs = cloneHarnessConfigs(harnessConfigs)
	}
	o.imageRegistry = imageRegistry
	o.active = true
}

// Apply merges overlay values into a VersionedSettings. DB values
// replace file values for each field that has been set in the overlay.
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

	if o.runtimes != nil {
		vs.Runtimes = cloneRuntimes(o.runtimes)
	}
	if o.profiles != nil {
		vs.Profiles = cloneProfiles(o.profiles)
	}
	if o.harnessConfigs != nil {
		vs.HarnessConfigs = cloneHarnessConfigs(o.harnessConfigs)
	}
	if o.imageRegistry != "" && os.Getenv("SCION_IMAGE_REGISTRY") == "" {
		vs.ImageRegistry = o.imageRegistry
	}
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
