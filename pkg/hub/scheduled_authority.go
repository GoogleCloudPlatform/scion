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
	"log/slog"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// This file holds the authority of scheduled work. A schedule (or one-shot
// scheduled event) carries an authorization revision: the initiator
// attribution of the last request that changed what a future dispatch does,
// the revision counter, and the frozen effect ceiling of that request's
// credential (AuthorityCeiling). The three are written together.

// Mutation types of the schedule lifecycle writes that change no authority.
const (
	mutationTypeSchedulePause  = "schedule_pause"
	mutationTypeScheduleDelete = "schedule_delete"
)

// revisionAuthorityCeiling returns the frozen effect ceiling to record on a
// schedule revision of eventType authored by the request's identity: the
// ceiling sourceEffectCeiling computes for that credential. On an error it
// writes the response and returns ok=false, before anything is written:
//   - a lookup fault → 500;
//   - a credential that cannot be recorded as an authority source → 403 with
//     details.denied_by="delegation_ceiling", for a dispatch_agent revision.
//
// A message revision whose credential cannot be recorded as an authority
// source records the unrecorded ceiling instead: scheduled-message authority
// is decided by the scheduled-message rule, which does not read this
// ceiling, and an unrecorded ceiling never reads as principal.
func (s *Server) revisionAuthorityCeiling(w http.ResponseWriter, r *http.Request, projectID, eventType string) (store.EffectCeiling, bool) {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if s.authzService == nil {
		slog.ErrorContext(ctx, "schedule authoring: no authorization service", "project_id", projectID)
		InternalError(w)
		return store.EffectCeiling{}, false
	}
	ceiling, _, err := s.authzService.sourceEffectCeiling(ctx, identity)
	if err != nil {
		cause, structural := ceilingDenyCauseForError(err)
		if !structural {
			slog.ErrorContext(ctx, "schedule authoring: effect ceiling lookup failed",
				"project_id", projectID, "error", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"Unable to evaluate the credential's delegation ceiling; retry later", nil)
			return store.EffectCeiling{}, false
		}
		if eventType == "message" {
			return store.EffectCeiling{}, true
		}
		logAuthzDenial(r, identity, Resource{Type: "schedule", ParentType: "project", ParentID: projectID}, ActionUpdate,
			"effect ceiling denied: "+string(cause)+": "+err.Error())
		writeForbiddenDenial(w, scheduleCeilingDenialMessage(cause), DeniedByDelegationCeiling)
		return store.EffectCeiling{}, false
	}
	return ceiling, true
}

// scheduleCeilingDenialMessage is the neutral response message for a
// schedule authoring credential whose ceiling cannot be recorded.
func scheduleCeilingDenialMessage(cause DenyCause) string {
	if cause == DenyCauseCeilingSourceNotAllowed {
		return "This credential kind cannot authorize scheduled work"
	}
	return ceilingSourceDenialMessage(cause)
}

// newScheduleAudit returns the audit record of a schedule lifecycle write,
// with the request's actor applied. It is written in the same transaction as
// the write it records.
func newScheduleAudit(ctx context.Context, mutationType, scheduleID string) *store.MutationAuditRecord {
	record := &store.MutationAuditRecord{
		MutationType: mutationType,
		TargetType:   "schedule",
		TargetID:     scheduleID,
		Timestamp:    time.Now(),
	}
	auditActorFromContext(ctx).ApplyActor(record)
	applyHubActorFallback(record)
	return record
}
