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

package runtime

import (
	"errors"
	"fmt"
	"reflect"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// ErrSubstrateProfileInvalid marks a substrate runtime that failed
// deterministic config validation: the profile failed
// ValidateOperatorOnlySubstrateProfile, or its operator-defined runtime block
// failed NewSubstrateRuntime's config checks (a missing required endpoint, or
// substrate.Validate). The *ErrorRuntime GetRuntime returns in those cases
// carries an error matching this sentinel under errors.Is, so a caller can
// tell a misconfigured substrate profile apart from any other runtime
// construction failure.
//
// Construct-time dependency failures (building the Kubernetes client,
// substrate.Dial) do NOT match this sentinel: they can be transient, so they
// surface as a plain degraded *ErrorRuntime instead.
var ErrSubstrateProfileInvalid = errors.New("substrate profile invalid")

// substrateProfileError tags a substrate construction failure with
// ErrSubstrateProfileInvalid while keeping the underlying error's own text
// and its errors.Is/As chain intact.
type substrateProfileError struct {
	err error
}

func (e *substrateProfileError) Error() string { return e.err.Error() }

func (e *substrateProfileError) Unwrap() []error {
	return []error{ErrSubstrateProfileInvalid, e.err}
}

// substrateProfileInvalid wraps err so it matches ErrSubstrateProfileInvalid.
func substrateProfileInvalid(err error) error {
	return &substrateProfileError{err: err}
}

// ValidateOperatorOnlySubstrateProfile enforces that a profile resolving to
// a substrate runtime is entirely operator-defined. effectiveVS is whatever
// settings GetRuntime actually resolved against — typically project-merged
// (config.LoadEffectiveSettings), which for a hub-managed or linked project
// can carry content a tenant/repo author controls. profileName is the raw
// profile name GetRuntime was called with (may be empty).
//
// A substrate runtimes.<name> block carries the ateapi/router endpoints,
// CA/trust-bundle, token_audience, and egress_allow — everything the
// bootstrap payload (hub token, resolved secrets) and the actor's egress
// policy depend on. Letting project-merged settings define or override any
// of that would let a repo redirect the bootstrap payload to an attacker
// endpoint or widen egress arbitrarily — exactly the tier
// resolveHubEndpointForCreate (pkg/runtimebroker/hubenv.go) already refuses
// to trust for the hub endpoint, for the same reason.
//
// Project settings MAY select an operator-defined profile by name (e.g. via
// their own active_profile): this function resolves profileName the same
// way ResolveRuntime does (falling back to effectiveVS.ActiveProfile) so
// that selection is honored, but then requires the resulting runtime
// definition to be byte-identical to what the operator's own global-only
// settings (config.LoadGlobalSettings) define for that exact name. A
// mismatch — the name missing from global settings entirely, resolving to
// a non-substrate type there, or differing in any field — fails closed
// with an error naming the offending profile/runtime, rather than silently
// falling back to either config.
//
// An empty effective profile name (profileName and ActiveProfile both
// empty) is checked like any other name: GetRuntime resolves it to the
// profile stored under the empty key, and so may build a substrate runtime
// from it, so the operator-only requirement applies to that key too. Do not
// add an early return for the empty name: it would leave a profile
// GetRuntime builds unchecked. In co-located mode, once the hub has stored
// profiles, the settings overlay replaces the profiles map
// (config.SettingsOverlay.Apply), so a project-stored empty-name profile
// does not survive there; this check is what covers settings loaded without
// that replacement (a standalone broker, or a hub with no stored profiles),
// and is defense in depth otherwise.
//
// Returns nil when effectiveVS is nil or the resolved runtime type is not
// substrate at all (nothing for this function to enforce). Callers MUST
// treat a non-nil return as fatal to runtime construction — never
// log-and-continue with effectiveVS's own config.
func ValidateOperatorOnlySubstrateProfile(effectiveVS *config.VersionedSettings, profileName string) error {
	if effectiveVS == nil {
		return nil
	}
	effectiveProfile := profileName
	if effectiveProfile == "" {
		effectiveProfile = effectiveVS.ActiveProfile
	}

	rtConfig, runtimeType, err := effectiveVS.ResolveRuntime(effectiveProfile)
	if err != nil || runtimeType != "substrate" {
		// Not a substrate profile (or not resolvable at all) under the
		// project-merged view — nothing for this function to enforce. The
		// ordinary "profile/runtime not found" handling elsewhere in
		// GetRuntime covers the resolution failure case.
		return nil
	}

	runtimeKey := effectiveProfile
	if profile, ok := effectiveVS.Profiles[effectiveProfile]; ok {
		runtimeKey = profile.Runtime
	}

	globalVS, _, gerr := config.LoadGlobalSettings()
	if gerr != nil {
		return fmt.Errorf("substrate runtime %q (profile %q): resolving operator settings: %w", runtimeKey, effectiveProfile, gerr)
	}

	// Look up the same literal profile name in the operator's settings.
	// ResolveRuntime would replace an empty name with the operator's own
	// active_profile, comparing against a different profile than the one
	// GetRuntime resolved.
	globalRtConfig, globalRuntimeType, ok := resolveProfileRuntimeExact(globalVS, effectiveProfile)
	if !ok || globalRuntimeType != "substrate" || !reflect.DeepEqual(rtConfig, globalRtConfig) {
		return fmt.Errorf(
			"substrate runtime %q (profile %q) is not defined identically in operator (global) settings: "+
				"project or repo settings may only select an operator-defined substrate profile by name, "+
				"never define or override its runtime block (api/router endpoints, CA/trust bundle, "+
				"token_audience, egress_allow) — move this definition to the operator's own global settings",
			runtimeKey, effectiveProfile)
	}
	return nil
}

// resolveProfileRuntimeExact returns the runtime config and effective
// runtime type of the profile stored under exactly profileName in vs, with
// no active_profile fallback for an empty name. The type is the runtime
// entry's Type, or its runtimes key when Type is unset, as in
// config.VersionedSettings.ResolveRuntime. ok is false when vs is nil, the
// profile is missing, or it names a missing runtime entry.
func resolveProfileRuntimeExact(vs *config.VersionedSettings, profileName string) (rtConfig config.V1RuntimeConfig, runtimeType string, ok bool) {
	if vs == nil {
		return config.V1RuntimeConfig{}, "", false
	}
	profile, ok := vs.Profiles[profileName]
	if !ok {
		return config.V1RuntimeConfig{}, "", false
	}
	rtConfig, ok = vs.Runtimes[profile.Runtime]
	if !ok {
		return config.V1RuntimeConfig{}, "", false
	}
	runtimeType = rtConfig.Type
	if runtimeType == "" {
		runtimeType = profile.Runtime
	}
	return rtConfig, runtimeType, true
}
