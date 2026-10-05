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
	"errors"
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// agentMoveDispatcher is the broker dispatch a cross-broker move needs
// beyond AgentDispatcher. HTTPAgentDispatcher implements it; the reincarnate
// worker refuses a move with a dispatcher that does not.
type agentMoveDispatcher interface {
	// DispatchAgentProvisionForMove provisions the agent, without starting
	// it, on agent.RuntimeBrokerID, asking the broker to first confirm the
	// agent's existing workspace on its mount of the NFS export
	// (expectNFSWorkspace: agent.MoveWorkspaceAgentDir or
	// agent.MoveWorkspaceProject). A missing workspace is a 409 from the
	// broker and an error here.
	DispatchAgentProvisionForMove(ctx context.Context, agent *store.Agent, expectNFSWorkspace string) error
	// DispatchAgentDeleteLocalOnly removes the agent's state on
	// agent.RuntimeBrokerID only (container, broker-local agent directory
	// and home), never its files on the NFS export or its branch. It is
	// sent only to a broker advertising AgentMove: an older broker would
	// ignore localOnly and delete the shared workspace.
	DispatchAgentDeleteLocalOnly(ctx context.Context, agent *store.Agent) error
}

var _ agentMoveDispatcher = (*HTTPAgentDispatcher)(nil)

// errBrokerLacksAgentMove is returned, before anything is sent, when a
// localOnly delete targets a broker that does not advertise AgentMove.
var errBrokerLacksAgentMove = errors.New("runtime broker does not advertise the agent move capability; refusing to send a localOnly delete")

// DispatchAgentProvisionForMove implements agentMoveDispatcher.
func (d *HTTPAgentDispatcher) DispatchAgentProvisionForMove(ctx context.Context, agent *store.Agent, expectNFSWorkspace string) error {
	if expectNFSWorkspace == "" {
		return errors.New("DispatchAgentProvisionForMove: the expected NFS workspace is required")
	}
	return d.dispatchProvision(ctx, agent, "DispatchAgentProvisionForMove", false, expectNFSWorkspace)
}

// DispatchAgentDeleteLocalOnly implements agentMoveDispatcher.
func (d *HTTPAgentDispatcher) DispatchAgentDeleteLocalOnly(ctx context.Context, agent *store.Agent) error {
	if err := requireRuntimeBrokerAssigned(agent); err != nil {
		return err
	}
	broker, err := d.store.GetRuntimeBroker(ctx, agent.RuntimeBrokerID)
	if err != nil {
		return fmt.Errorf("load runtime broker %s: %w", agent.RuntimeBrokerID, err)
	}
	if broker.Capabilities == nil || !broker.Capabilities.AgentMove {
		return errBrokerLacksAgentMove
	}
	ctx = withRecordedRuntime(ctx, agent.Runtime)
	endpoint, err := d.getBrokerEndpoint(ctx, agent.RuntimeBrokerID)
	if err != nil {
		return err
	}
	// As DispatchAgentDelete: a linked project's path lets the broker find
	// a file-only agent (container gone).
	if info, _ := d.resolveDispatchProjectInfo(ctx, agent); info.projectPath != "" {
		ctx = withDeleteProjectPath(ctx, info.projectPath)
	}
	err = d.client.DeleteAgent(ctx, agent.RuntimeBrokerID, endpoint, agent.Slug, agent.ProjectID, DeleteAgentOptions{
		DeleteFiles: true,
		LocalOnly:   true,
		RunID:       agent.RunID,
	})
	if errors.Is(err, ErrLifecycleDeferred) {
		return fmt.Errorf("localOnly delete not supported for a cross-node broker: %w", err)
	}
	return err
}
