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

package agent

import (
	"github.com/ptone/scion-agent/pkg/runtime"
)

// ResolveRuntime determines the runtime to use for an agent.
// It prioritizes the explicit profile, then saved profile, then saved runtime.
// Finally it uses the runtime system's default detection.
func ResolveRuntime(grovePath, agentName, profileFlag string) runtime.Runtime {
	effectiveProfile := profileFlag
	if effectiveProfile == "" {
		// If no profile flag, check if we have a saved profile for this agent
		effectiveProfile = GetSavedProfile(agentName, grovePath)
	}

	effectiveRuntime := effectiveProfile
	if effectiveRuntime == "" {
		// If still no profile, we'll let GetRuntime handle auto-detection
		// but we might want to check for saved runtime as fallback
		effectiveRuntime = GetSavedRuntime(agentName, grovePath)
	}

	return runtime.GetRuntime(grovePath, effectiveRuntime)
}
