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

package hub

import (
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// workspaceSpecFor builds the workspace-recreation inputs for agent from its
// stored AppliedConfig, plus the caller's resolved workspaceMode (not stored
// on AppliedConfig — it comes from the project's sharing-mode resolution, the
// same source buildCreateRequest already uses). buildCreateRequest and
// DispatchAgentStart both call this single builder.
func workspaceSpecFor(agent *store.Agent, workspaceMode string) WorkspaceDispatchSpec {
	spec := WorkspaceDispatchSpec{WorkspaceMode: workspaceMode}
	if agent != nil && agent.AppliedConfig != nil {
		spec.GitClone = agent.AppliedConfig.GitClone
		spec.Branch = agent.AppliedConfig.Branch
	}
	return spec
}
