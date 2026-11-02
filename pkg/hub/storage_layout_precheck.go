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
	"context"
	"fmt"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// storageLayoutMismatchDetail is the refusal's details key holding the
// differing fields (projectfs.LayoutIssueDetails, public view: field names
// only, never their values).
const storageLayoutMismatchDetail = "storageLayoutDiff"

// SetStorageLayout gives the dispatcher the hub's projectfs Layout source
// (Server.projectStorageLayout), for storageLayoutPrecheck. Nil disables
// the check.
func (d *HTTPAgentDispatcher) SetStorageLayout(layout func() (projectfs.Layout, error)) {
	d.storageLayout = layout
}

// storageLayoutPrecheck refuses a shared-mode dispatch to a broker whose
// reported storage layout disagrees with the hub's (design §P2, M1;
// ptone/scion#5823): on a shared Layout, a shared-plain or
// worktree-per-agent hub-native project's workspace and shared dirs are
// the project tree the hub serves, so a broker that would place them
// elsewhere (another backend, share, subpath root or homes) must not run
// the agent. The refusal is a 409 carrying the diff. Projects the broker
// provides from its own path (linked projects), per-agent workspace modes
// and local hubs without a shared-dir split are not checked; a broker the
// store cannot return is left to the dispatch itself.
func (d *HTTPAgentDispatcher) storageLayoutPrecheck(ctx context.Context, agent *store.Agent) error {
	if d.storageLayout == nil || agent == nil || agent.RuntimeBrokerID == "" {
		return nil
	}
	// The Layout is in-memory configuration: a local hub without a
	// shared-dir split has no identity to compare, and no store read is
	// made for it.
	layout, layoutErr := d.storageLayout()
	if layoutErr == nil && !layout.Backend.Shared() && layout.SharedDirs == nil {
		return nil
	}
	info, err := d.resolveDispatchProjectInfo(ctx, agent)
	if err != nil {
		return err
	}
	if info.projectPath != "" {
		return nil
	}
	switch store.ResolveWorkspaceSharingMode(info.workspaceMode) {
	case store.SharingModeSharedPlain, store.SharingModeWorktreePerAgent:
	default:
		return nil
	}
	if layoutErr != nil {
		// Unreachable for a hub that started: hub startup validates the
		// same Layout. Only shared-mode dispatches are refused.
		return &RuntimeTargetRefusal{
			Code:    ErrCodeConflict,
			Status:  http.StatusConflict,
			Message: "the hub's project storage layout is unusable; shared workspaces cannot be dispatched (see hub logs)",
		}
	}
	broker, err := d.store.GetRuntimeBroker(ctx, agent.RuntimeBrokerID)
	if err != nil || broker == nil {
		return nil
	}
	issues := layout.DescriptorIssues(broker.WorkspaceStorage, "")
	if len(issues) == 0 {
		return nil
	}
	d.log.Warn("refusing dispatch: broker storage layout disagrees with the hub's",
		"agent_id", agent.ID, "broker", broker.ID, "diff", projectfs.LayoutIssueSummary(issues, projectfs.ViewLog))
	// The requester may not be a hub admin: the refusal names the
	// differing fields only (projectfs.ViewPublic).
	return &RuntimeTargetRefusal{
		Code:   ErrCodeConflict,
		Status: http.StatusConflict,
		Message: fmt.Sprintf("broker %s places project storage differently from this hub (%s); "+
			"configure the same server.project_storage on the broker, or use a per-agent workspace mode",
			brokerDisplayName(broker), projectfs.LayoutIssueSummary(issues, projectfs.ViewPublic)),
		Details: map[string]interface{}{storageLayoutMismatchDetail: projectfs.LayoutIssueDetails(issues, projectfs.ViewPublic), "broker": broker.ID},
	}
}
