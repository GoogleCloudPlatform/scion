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

// publishAgentCreatedIfLive publishes agent.created for a from a re-read of
// its row, unless a delete got there first (ptone/scion#2972). createAgent
// publishes created only after its dispatch returns, and with asynchronous
// launch (ptone/scion#2153) that can be well after a DELETE claimed, or even
// finished with, the row. Publishing then would re-add a deleted agent in web
// clients after its deleted event.
//
// The publish is skipped when:
//   - the row is gone (hard-deleted);
//   - DeletedAt is set (soft-deleted);
//   - a delete claim holds the row: deleteStopNoop, i.e. a live deleting or
//     finalizing lease, or a finalizing row whose lease expired (teardown has
//     run, only a retry or force lifts it).
//
// A failed delete (state failed, or a deleting row whose lease expired) does
// not suppress the publish: the delete gave up and the agent is live.
//
// The fresh row is published rather than a, as the final createAgent publish
// always did: a concurrent status write (or a guarded store write that a
// racing delete dropped) may have moved it on. If the re-read fails for a
// reason other than not-found, a is published as before.
func (s *Server) publishAgentCreatedIfLive(ctx context.Context, a *store.Agent) {
	fresh, err := s.store.GetAgent(ctx, a.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.agentLifecycleLog.Debug("skipping agent.created publish: agent deleted",
			"agent_id", a.ID)
		return
	case err != nil:
		s.agentLifecycleLog.Warn("failed to re-read agent before created publish",
			"agent_id", a.ID, "error", err)
		s.events.PublishAgentCreated(ctx, a)
		return
	}
	if !fresh.DeletedAt.IsZero() {
		s.agentLifecycleLog.Debug("skipping agent.created publish: agent soft-deleted",
			"agent_id", a.ID)
		return
	}
	if deleteStopNoop(fresh) {
		s.agentLifecycleLog.Debug("skipping agent.created publish: delete in progress",
			"agent_id", a.ID, "deletion_state", fresh.DeletionState, "deletion_claim", fresh.DeletionClaim)
		return
	}
	s.events.PublishAgentCreated(ctx, fresh)
}
