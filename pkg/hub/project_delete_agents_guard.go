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
	"log/slog"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Project delete and agents whose runtime deletion is not confirmed
// (ptone/scion#4811).
//
// A project delete removes every agent record of the project in its
// transaction and only afterwards asks the runtimes to delete the agents,
// best-effort. So, unless the request is forced, the delete is refused with
// 409 conflict while the project has an agent whose runtime deletion is not
// confirmed: any agent that is not soft-deleted, whatever its deletion
// state ("" for never deleted, deleting, finalizing or failed). A
// soft-deleted agent counts as confirmed: the agent delete engine
// soft-deletes only after the runtime delete succeeded (a forced agent
// delete is always hard). The caller deletes the agents first, through the
// agent delete, which keeps a failed agent's record, or retries with
// force=true to remove the records anyway.
//
// An agent whose saved placement is inconsistent cannot be deleted that way:
// the agent delete refuses it (ptone/scion#4844). The refusal flags each
// such agent with the placement code the agent delete would answer
// (placementRefusal, ptone/scion#4960), so the operator knows which agents
// need the recovery path instead of a plain delete.

// projectDeleteBlockingAgentsLimit caps the agents listed in the refusal. It
// is a variable so tests can lower it.
var projectDeleteBlockingAgentsLimit = 50

// projectAgentRef identifies an agent that blocks a project delete.
type projectAgentRef struct {
	ID            string `json:"id"`
	Slug          string `json:"slug"`
	DeletionState string `json:"deletionState,omitempty"`
	// PlacementRefusal is the 409 code an agent delete refuses this agent
	// with because its saved placement is inconsistent
	// (agentDeletePlacementCheck), or "" when it would not be refused for
	// that (or the check could not be made).
	PlacementRefusal string `json:"placementRefusal,omitempty"`
}

// projectHasAgentsDeleteError is returned inside the project delete
// transaction when agents block the delete; the transaction rolls back.
type projectHasAgentsDeleteError struct {
	agents    []projectAgentRef
	truncated bool
}

const projectHasAgentsDeleteMessage = "cannot delete a project while it has agents that are not deleted: delete the agents first (the CLI and API offer a force option that removes their records anyway)"

// projectHasPlacementRefusedAgentsMessage is added to the refusal's reason
// when a listed agent is flagged with placementRefusal.
const projectHasPlacementRefusedAgentsMessage = "; agents flagged with placementRefusal have an inconsistent saved placement and cannot be deleted individually, see " + inconsistentPlacementDocsURL

func (e *projectHasAgentsDeleteError) Error() string {
	return projectHasAgentsDeleteMessage
}

// placementRefused reports whether a listed agent is flagged with a
// placement refusal.
func (e *projectHasAgentsDeleteError) placementRefused() bool {
	for _, a := range e.agents {
		if a.PlacementRefusal != "" {
			return true
		}
	}
	return false
}

// decision is the 409 refusal; details.agents lists the blocking agents (at
// most projectDeleteBlockingAgentsLimit) and details.truncated reports more.
// When an agent is flagged with placementRefusal, the reason says so and
// details.placementDocsUrl links the recovery guidance.
func (e *projectHasAgentsDeleteError) decision() *ProjectDeleteDecision {
	d := &ProjectDeleteDecision{
		Allowed:    false,
		DenialCode: ErrCodeConflict,
		Reason:     projectHasAgentsDeleteMessage,
		HTTPStatus: http.StatusConflict,
		Details: map[string]interface{}{
			"agents":    e.agents,
			"truncated": e.truncated,
		},
	}
	if e.placementRefused() {
		d.Reason += projectHasPlacementRefusedAgentsMessage
		d.Details["placementDocsUrl"] = inconsistentPlacementDocsURL
	}
	return d
}

// checkProjectAgentsDeletedTx returns a *projectHasAgentsDeleteError when
// the project has an agent that is not soft-deleted. It runs inside the
// delete transaction, after the project's agent rows are locked.
func checkProjectAgentsDeletedTx(ctx context.Context, tx store.Store, projectID string) error {
	page, err := tx.ListAgents(ctx, store.AgentFilter{ProjectID: projectID},
		store.ListOptions{Limit: projectDeleteBlockingAgentsLimit, SkipTotalCount: true})
	if err != nil {
		return fmt.Errorf("list project agents for deletion: %w", err)
	}
	if page == nil || len(page.Items) == 0 {
		return nil
	}
	// A further page means more agents block than are listed.
	e := &projectHasAgentsDeleteError{truncated: page.NextCursor != ""}
	brokers := &brokerReadCachingStore{Store: tx}
	for i := range page.Items {
		a := &page.Items[i]
		e.agents = append(e.agents, projectAgentRef{ID: a.ID, Slug: a.Slug, DeletionState: a.DeletionState,
			PlacementRefusal: projectAgentPlacementRefusal(ctx, brokers, a)})
	}
	return e
}

// projectAgentPlacementRefusal is the placement code an agent delete would
// refuse a with (agentDeletePlacementCheck), or "". A check that fails for
// another reason (a store error) is logged and leaves the agent unflagged:
// it must not change the refusal itself.
func projectAgentPlacementRefusal(ctx context.Context, st store.Store, a *store.Agent) string {
	err := agentDeletePlacementCheck(ctx, st, a)
	if err == nil {
		return ""
	}
	if refusal, ok := placementRefusalOf(err); ok {
		return refusal.Code
	}
	slog.Warn("project delete refusal: could not check an agent's saved placement",
		"agent_id", a.ID, "project_id", a.ProjectID, "error", err)
	return ""
}

// brokerReadCachingStore reads each Runtime Broker row at most once (the
// row or the error), so flagging the listed agents of a project costs one
// read per distinct Runtime Broker. It is used for one refusal only.
type brokerReadCachingStore struct {
	store.Store
	reads map[string]brokerRead
}

type brokerRead struct {
	broker *store.RuntimeBroker
	err    error
}

func (s *brokerReadCachingStore) GetRuntimeBroker(ctx context.Context, id string) (*store.RuntimeBroker, error) {
	if r, ok := s.reads[id]; ok {
		return r.broker, r.err
	}
	b, err := s.Store.GetRuntimeBroker(ctx, id)
	if s.reads == nil {
		s.reads = map[string]brokerRead{}
	}
	s.reads[id] = brokerRead{broker: b, err: err}
	return b, err
}
