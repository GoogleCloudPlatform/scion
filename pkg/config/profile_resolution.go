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

import "context"

// ProfileResolutionMode is how a request resolves Runtime Broker Profiles
// from settings (.design/flat-runtime-brokers-contract.md section 10). It is
// derived from the request's context: a request that carries a
// FlatInstancePolicy is a flat instance request (ProfileResolutionFrom).
type ProfileResolutionMode int

const (
	// ProfileResolutionLegacy is today's resolution: an explicit profile,
	// else the settings active_profile, supplies the profile tier
	// (profile resources, harness overrides, runtime). The zero value.
	ProfileResolutionLegacy ProfileResolutionMode = iota
	// ProfileResolutionFlatInstance is resolution for a flat Runtime Broker
	// instance: the profile tier is skipped entirely, including the
	// active_profile fallback, and the instance's own policy takes the
	// profile tier's rung (ForFlatInstance). Non-profile defaults (global
	// settings, harness configs, templates, the request) still apply.
	ProfileResolutionFlatInstance
)

// ProfileResolutionFrom returns the mode ctx carries: flat instance exactly
// when ctx carries a FlatInstancePolicy (WithFlatInstancePolicy), legacy
// otherwise.
func ProfileResolutionFrom(ctx context.Context) ProfileResolutionMode {
	if FlatInstancePolicyFrom(ctx) != nil {
		return ProfileResolutionFlatInstance
	}
	return ProfileResolutionLegacy
}

// ForProfileResolution returns the policy-less settings view of mode, for a
// caller that knows a request is flat but cannot see the instance's local
// policy (the Hub's reincarnation planning). Legacy mode returns vs itself.
// Flat instance mode returns a shallow copy with no active profile and no
// profiles, so every profile-tier lookup (including the active_profile
// fallback of the VersionedSettings resolvers) finds nothing, while every
// other setting is shared unchanged. vs and its maps are never modified.
// A Runtime Broker serving a flat instance uses ForFlatInstance instead.
func (vs *VersionedSettings) ForProfileResolution(mode ProfileResolutionMode) *VersionedSettings {
	if vs == nil || mode != ProfileResolutionFlatInstance {
		return vs
	}
	view := *vs
	view.ActiveProfile = ""
	view.Profiles = nil
	return &view
}

// LoadEffectiveSettingsFor is LoadEffectiveSettings returning the settings
// view of the request: the flat instance view when ctx carries a
// FlatInstancePolicy (see ForFlatInstance), the loaded settings otherwise.
func LoadEffectiveSettingsFor(ctx context.Context, projectPath string) (*VersionedSettings, []string, error) {
	vs, warnings, err := LoadEffectiveSettings(projectPath)
	return vs.ForFlatInstance(FlatInstancePolicyFrom(ctx)), warnings, err
}

// LoadGlobalSettingsWithOverlayFor is LoadGlobalSettingsWithOverlay returning
// the settings view of the request (see LoadEffectiveSettingsFor).
func LoadGlobalSettingsWithOverlayFor(ctx context.Context) (*VersionedSettings, []string, error) {
	vs, warnings, err := LoadGlobalSettingsWithOverlay()
	return vs.ForFlatInstance(FlatInstancePolicyFrom(ctx)), warnings, err
}
