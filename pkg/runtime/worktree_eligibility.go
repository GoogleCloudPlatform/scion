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
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// WorktreeModeEligible reports whether the host environment supports
// worktree-per-agent mode. It returns (true, "") when git >= 2.47 is
// available (required for --relative-paths), or (false, reason) with a
// human-readable explanation when the check fails.
//
// The caller is responsible for logging and for choosing the fallback
// (typically clone-per-agent).
func WorktreeModeEligible() (bool, string) {
	version, _, err := util.GetGitVersion()
	if err != nil {
		return false, fmt.Sprintf("unable to determine git version: %v", err)
	}
	return worktreeEligibleForVersion(version)
}

// worktreeEligibleForVersion is the testable core: it checks whether the
// given version string satisfies the git >= 2.47 requirement.
func worktreeEligibleForVersion(version string) (bool, string) {
	if err := util.CompareGitVersion(version, 2, 47); err != nil {
		return false, fmt.Sprintf(
			"git >= 2.47.0 required for worktree-per-agent mode (--relative-paths), found %s",
			version,
		)
	}
	return true, ""
}
