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

// projectDeleteBlockingAgentsLimit caps the agents listed in the refusal. It
// is a variable so tests can lower it.
var projectDeleteBlockingAgentsLimit = 50

// projectAgentRef identifies an agent that blocks a project delete.
type projectAgentRef struct {
	ID            string `json:"id"`
	Slug          string `json:"slug"`
	DeletionState string `json:"deletionState,omitempty"`
}

// projectHasAgentsDeleteError is returned inside the project delete
// transaction when agents block the delete; the transaction rolls back.
type projectHasAgentsDeleteError struct {
	agents    []projectAgentRef
	truncated bool
}

const projectHasAgentsDeleteMessage = "cannot delete a project while it has agents that are not deleted: delete the agents first (the CLI and API offer a force option that removes their records anyway)"

func (e *projectHasAgentsDeleteError) Error() string {
	return projectHasAgentsDeleteMessage
}

// decision is the 409 refusal; details.agents lists the blocking agents (at
// most projectDeleteBlockingAgentsLimit) and details.truncated reports more.
func (e *projectHasAgentsDeleteError) decision() *ProjectDeleteDecision {
	return &ProjectDeleteDecision{
		Allowed:    false,
		DenialCode: ErrCodeConflict,
		Reason:     projectHasAgentsDeleteMessage,
		HTTPStatus: http.StatusConflict,
		Details: map[string]interface{}{
			"agents":    e.agents,
			"truncated": e.truncated,
		},
	}
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
	for _, a := range page.Items {
		e.agents = append(e.agents, projectAgentRef{ID: a.ID, Slug: a.Slug, DeletionState: a.DeletionState})
	}
	return e
}
