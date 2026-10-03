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
// Returns nil when effectiveVS is nil, the effective profile name is
// empty, or the resolved runtime type is not substrate at all (nothing for
// this function to enforce). Callers MUST treat a non-nil return as fatal
// to runtime construction — never log-and-continue with effectiveVS's own
// config.
func ValidateOperatorOnlySubstrateProfile(effectiveVS *config.VersionedSettings, profileName string) error {
	if effectiveVS == nil {
		return nil
	}
	effectiveProfile := profileName
	if effectiveProfile == "" {
		effectiveProfile = effectiveVS.ActiveProfile
	}
	if effectiveProfile == "" {
		return nil
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

	globalRtConfig, globalRuntimeType, gerr := globalVS.ResolveRuntime(effectiveProfile)
	if gerr != nil || globalRuntimeType != "substrate" || !reflect.DeepEqual(rtConfig, globalRtConfig) {
		return fmt.Errorf(
			"substrate runtime %q (profile %q) is not defined identically in operator (global) settings: "+
				"project or repo settings may only select an operator-defined substrate profile by name, "+
				"never define or override its runtime block (api/router endpoints, CA/trust bundle, "+
				"token_audience, egress_allow) — move this definition to the operator's own global settings",
			runtimeKey, effectiveProfile)
	}
	return nil
}
