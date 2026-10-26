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
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// workspaceSpecFor builds the workspace-recreation inputs for agent from its
// stored AppliedConfig, plus the project facts the caller resolved for this
// dispatch (not stored on AppliedConfig): the workspace mode from the
// project's sharing-mode resolution, the same source buildCreateRequest
// already uses, and a shared-plain project's workspace clone settings.
// buildCreateRequest and DispatchAgentStart both call this single builder.
func workspaceSpecFor(agent *store.Agent, info projectDispatchInfo) WorkspaceDispatchSpec {
	spec := WorkspaceDispatchSpec{
		WorkspaceMode:        info.workspaceMode,
		SharedWorkspaceClone: info.sharedWorkspaceClone,
	}
	if agent != nil && agent.AppliedConfig != nil {
		spec.GitClone = withTokenRemote(agent.AppliedConfig.GitClone, info.gitRemote)
		spec.Branch = agent.AppliedConfig.Branch
	}
	return spec
}

// withTokenRemote returns a copy of gc whose TokenRemote is the project's
// recorded git remote, or nil when gc is nil. The value always comes from the
// project record resolved for this dispatch: whatever TokenRemote the stored
// config carries is replaced, so an agent created before the project's
// remote changed, or a config that arrived with its own value, never binds
// the token to anything but the current record. An empty gitRemote clears
// it, and the clone then gets no token.
func withTokenRemote(gc *api.GitCloneConfig, gitRemote string) *api.GitCloneConfig {
	if gc == nil {
		return nil
	}
	out := *gc
	out.TokenRemote = gitRemote
	return &out
}

// checkDispatchCloneURLs returns an error wrapping
// provision.ErrHostCloneURLRefused when spec's per-agent clone URL or shared
// workspace clone URL is one provisioning refuses (provision.CheckHostCloneURL),
// so such an agent is not dispatched at all. The broker refuses the same
// URLs on every path; this fails the dispatch before any broker request.
// The error names no URL.
func checkDispatchCloneURLs(spec WorkspaceDispatchSpec) error {
	if gc := spec.GitClone; gc != nil && gc.URL != "" && provision.CheckHostCloneURL(gc.URL) != nil {
		return fmt.Errorf("clone-per-agent: %w", provision.ErrHostCloneURLRefused)
	}
	if gc := spec.SharedWorkspaceClone; gc != nil && gc.URL != "" && provision.CheckHostCloneURL(gc.URL) != nil {
		return fmt.Errorf("shared-workspace: %w", provision.ErrHostCloneURLRefused)
	}
	return nil
}
