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
	"log/slog"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// saAssignmentSourceCeiling returns the frozen effect ceiling and provenance
// of the request's principal, for a write that records a service-account
// assignment on an existing agent (PATCH, reincarnate). It applies the same
// rules and responses as the agent-create ceiling (applyCreateEffectCeiling):
// a source that cannot record authority is refused with 403 and the ceiling
// denial message, and a lookup fault answers 503. ok is false when the
// response has been written; nothing has been written to the store then.
func (s *Server) saAssignmentSourceCeiling(w http.ResponseWriter, r *http.Request, resource Resource, action Action) (store.EffectCeiling, store.AuthorityProvenance, bool) {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if s.authzService == nil {
		slog.ErrorContext(ctx, "service-account assignment: no authorization service to record the source")
		InternalError(w)
		return store.EffectCeiling{}, store.AuthorityProvenance{}, false
	}
	ceiling, prov, err := s.authzService.sourceEffectCeiling(ctx, identity)
	if err != nil {
		cause, structural := ceilingDenyCauseForError(err)
		if !structural {
			slog.ErrorContext(ctx, "service-account assignment: effect ceiling lookup failed",
				"resource_id", resource.ID, "error", err)
			writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable,
				"Unable to evaluate the credential's delegation ceiling; retry later", nil)
			return store.EffectCeiling{}, store.AuthorityProvenance{}, false
		}
		logAuthzDenial(r, identity, resource, action,
			"effect ceiling denied: "+string(cause)+": "+err.Error())
		writeForbiddenDenial(w, ceilingSourceDenialMessage(cause), DeniedByDelegationCeiling)
		return store.EffectCeiling{}, store.AuthorityProvenance{}, false
	}
	return ceiling, prov, true
}

// saAssignmentWrite is the service-account assignment change a write to an
// existing agent makes in its own transaction: replace the active row with
// Assignment, or, when Assignment is nil, deactivate any active row with
// cause sa_cleared.
type saAssignmentWrite struct {
	Assignment *store.AgentServiceAccountAssignment
}

// apply runs the change in tx for agentID.
func (w saAssignmentWrite) apply(r *http.Request, tx store.Store, agentID string) error {
	ctx := r.Context()
	if w.Assignment != nil {
		w.Assignment.AgentID = agentID
		return tx.ReplaceAgentServiceAccountAssignment(ctx, w.Assignment)
	}
	now := time.Now()
	_, err := tx.DeactivateAgentServiceAccountAssignments(ctx, agentID, store.Deactivation{
		Cause: store.EdgeDeactivationSACleared,
		At:    &now,
		OpID:  api.NewUUID(),
	})
	return err
}
