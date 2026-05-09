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
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// ResolveRuntime determines the runtime to use for an agent.
// If profileFlag is non-empty, it is used as the profile name.
// Otherwise, GetRuntime resolves the active profile from merged settings
// (grove settings override global settings).
func ResolveRuntime(projectPath, agentName, profileFlag string) runtime.Runtime {
	return runtime.GetRuntime(projectPath, profileFlag)
}
