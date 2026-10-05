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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// publishAgentCreatedIfLive publishes agent.created for agent from a re-read
// of its row, unless a delete got there first (ptone/scion#2972).
// createAgent publishes created only after its dispatch returns, and with
// asynchronous launch (ptone/scion#2153) that can be well after a DELETE
// claimed, or even finished with, the row. Publishing then would re-add a
// deleted agent in web clients after its deleted event.
//
// The publish is skipped when:
//   - the row is gone (hard-deleted);
//   - DeletedAt is set (soft-deleted);
//   - a delete claim holds the row: deleteStopNoop, i.e. a live deleting or
//     finalizing lease, or a finalizing row whose lease expired. The
//     expired-finalizing case skips on purpose: teardown and revocation have
//     run, only a retry or force lifts it, and created carries no deletion
//     view, so it would show the agent without its banner.
//
// A failed delete (state failed, or a deleting row whose lease expired) does
// not suppress the publish: the delete gave up and the agent is live.
//
// The fresh row is published rather than agent, as the final createAgent
// publish always did: a concurrent status write (or a guarded store write
// that a racing delete dropped) may have moved it on. If the re-read fails
// for a reason other than not-found, agent is published as before.
//
// Residual window: the re-read and the publish are not atomic with the delete
// engine, so a delete that claims and finishes between them can still emit
// this (unmarked) created after its deleted. Web clients drop an unmarked
// created for an ID they tombstoned on deleted (ptone/scion#2886), which
// covers it there. MessageBrokerProxy.handleLifecycleEvent re-checks the row
// by the same rule before subscribing (createdAgentLive, ptone/scion#3056).
func (s *Server) publishAgentCreatedIfLive(ctx context.Context, agent *store.Agent) {
	fresh, err := s.store.GetAgent(ctx, agent.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.agentLifecycleLog.Debug("skipping agent.created publish: agent deleted",
			"agent_id", agent.ID)
		return
	case err != nil:
		s.agentLifecycleLog.Warn("failed to re-read agent before created publish",
			"agent_id", agent.ID, "error", err)
		s.events.PublishAgentCreated(ctx, agent)
		return
	}
	if deletedOrDeleteHeld(fresh) {
		s.agentLifecycleLog.Debug("skipping agent.created publish: agent soft-deleted or delete in progress",
			"agent_id", agent.ID, "soft_deleted", !fresh.DeletedAt.IsZero(),
			"deletion_state", fresh.DeletionState, "deletion_claim", fresh.DeletionClaim)
		return
	}
	s.events.PublishAgentCreated(ctx, fresh)
}
