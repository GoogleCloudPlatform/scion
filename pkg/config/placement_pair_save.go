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

// Fixed reasons of the cross-entry placement save checks (never a
// submitted value).
const (
	placementReasonNotKubernetes  = "placement fields are not valid for this runtime type"
	placementReasonRuntimeMissing = "the profile sets placement, but the runtime entry it names does not exist"
	placementReasonPoolConflict   = "pool selector conflicts with the placement of the profile's runtime entry"
)

// PlacementPairSaveChecks checks the Kubernetes placement rules that span
// runtime entries and profiles, in the form of RuntimeSaveChecks: placement
// on a runtime entry that is not Kubernetes, or on a profile whose runtime
// entry is not; a profile with placement whose runtime entry exists
// neither in the runtimes nor among the built-in default runtimes; and a
// profile whose pool node selector differs from its runtime entry's.
// before holds the stored values and after the values the save would
// store. A finding the save introduces (absent before, present after) is
// an error, so the save is refused; one already stored is a warning, so a
// pre-existing problem never blocks a save. Both lists are sorted by path.
func PlacementPairSaveChecks(before, after map[string]V1RuntimeConfig, beforeProfiles, afterProfiles map[string]V1ProfileConfig) (errs, warnings []ValidationError) {
	old := map[ValidationError]bool{}
	for _, e := range placementPairFindings(before, beforeProfiles) {
		old[e] = true
	}
	for _, e := range placementPairFindings(after, afterProfiles) {
		if old[e] {
			warnings = append(warnings, e)
		} else {
			errs = append(errs, e)
		}
	}
	sortValidationErrors(errs)
	sortValidationErrors(warnings)
	return errs, warnings
}

// placementPairFindings lists the cross-entry placement findings of one
// runtimes and profiles pair (see PlacementPairSaveChecks).
func placementPairFindings(runtimes map[string]V1RuntimeConfig, profiles map[string]V1ProfileConfig) []ValidationError {
	var out []ValidationError
	for name, rt := range runtimes {
		if RuntimeEntryHasPlacement(rt) && !IsKubernetesRuntimeEntry(name, rt) {
			out = append(out, ValidationError{Path: "runtimes." + name, Message: placementReasonNotKubernetes})
		}
	}
	builtins := defaultBuiltinCatalog().Runtimes
	for name, p := range profiles {
		if p.Runtime == "" {
			continue
		}
		path := "profiles." + name
		rt, ok := runtimes[p.Runtime]
		if !ok {
			rt, ok = builtins[p.Runtime]
		}
		if !ok {
			if ProfileHasPlacement(p) {
				out = append(out, ValidationError{Path: path + ".runtime", Message: placementReasonRuntimeMissing})
			}
			continue
		}
		if ProfileHasPlacement(p) && !IsKubernetesRuntimeEntry(p.Runtime, rt) {
			out = append(out, ValidationError{Path: path, Message: placementReasonNotKubernetes})
		}
		if ProfilePoolConflict(rt, p) {
			out = append(out, ValidationError{Path: path + ".node_selector", Message: placementReasonPoolConflict})
		}
	}
	return out
}
