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

// ProfileHarnessOverrideImage returns the image that
// profiles.<profileName>.harness_overrides.<harnessConfigName>.image sets
// EXPLICITLY, or "" when that key is unset. An empty profileName means the
// active profile, matching ResolveHarnessConfig.
//
// ResolveHarnessConfig folds this value into its result, so a caller cannot
// tell from that result alone whether the image came from the profile
// override or from the plain harness_configs.<hc>.image default. Image
// precedence needs that distinction: an explicit profile override outranks a
// template's image, while the plain settings default does not
// (ptone/scion#1799; see settings-precedence.md, "Container image and
// Kubernetes image pull policy").
func (vs *VersionedSettings) ProfileHarnessOverrideImage(profileName, harnessConfigName string) string {
	if vs == nil || harnessConfigName == "" {
		return ""
	}
	if profileName == "" {
		profileName = vs.ActiveProfile
	}
	profile, ok := vs.Profiles[profileName]
	if !ok || profile.HarnessOverrides == nil {
		return ""
	}
	return profile.HarnessOverrides[harnessConfigName].Image
}

// ProfileHarnessOverrideImagePullPolicy is ProfileHarnessOverrideImage for
// profiles.<profileName>.harness_overrides.<harnessConfigName>.image_pull_policy.
// An explicitly set profile pull policy takes the same tier as an explicit
// profile image (ptone/scion#1799).
func (vs *VersionedSettings) ProfileHarnessOverrideImagePullPolicy(profileName, harnessConfigName string) string {
	if vs == nil || harnessConfigName == "" {
		return ""
	}
	if profileName == "" {
		profileName = vs.ActiveProfile
	}
	profile, ok := vs.Profiles[profileName]
	if !ok || profile.HarnessOverrides == nil {
		return ""
	}
	return profile.HarnessOverrides[harnessConfigName].ImagePullPolicy
}
