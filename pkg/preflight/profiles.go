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

package preflight

import "github.com/GoogleCloudPlatform/scion/pkg/config"

// ProfilesCheckName is the name of the profile catalog check.
const ProfilesCheckName = "settings.profiles"

// ProfileFindingSeverity is the one mapping from a profile catalog finding
// (config.CheckProfileCatalog, which carries no severity so ptone/scion#5015
// can map the same classes) to the preflight severity and code:
//   - a reference the change breaks (active_profile, project_active_profile,
//     agent_saved_profile, profile_runtime_missing) blocks;
//   - the same reference already broken before the change (PreExisting) is a
//     warning, code suffix ".pre_existing";
//   - a profile dropped with no reference is a warning;
//   - the single-stored-profile fallback and a changed default are
//     information.
//
// An unknown kind maps to block, so a kind added without a mapping fails
// closed.
func ProfileFindingSeverity(f config.ProfileCatalogFinding) (Severity, string) {
	code := ProfilesCheckName + "." + string(f.Kind)
	switch f.Kind {
	case config.ProfileFindingActiveProfile, config.ProfileFindingProjectActiveProfile,
		config.ProfileFindingAgentSavedProfile, config.ProfileFindingProfileRuntimeMissing:
		if f.PreExisting {
			return SeverityWarn, code + ".pre_existing"
		}
		return SeverityBlock, code
	case config.ProfileFindingDroppedUnused:
		return SeverityWarn, code
	case config.ProfileFindingActiveProfileFallback, config.ProfileFindingActiveProfileChanged:
		return SeverityInfo, code
	default:
		return SeverityBlock, code
	}
}
