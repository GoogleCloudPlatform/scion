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
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// scheduleCreatorMayStop reports whether the request's user may pause or
// delete schedule scheduleID of projectID because they created it. It is
// consulted only after the project-level scheduled_event.update/delete
// decision refused them (authorizeScheduledEventRowOr), so the normal path
// (and its audit) is unchanged and decided once. Project members can
// create recurring schedules but hold neither permission, so without this
// they could not stop their own.
//
// Pause and delete only stop future runs, so this grants nothing new. It
// applies only to a user's interactive session (a scoped access token keeps
// its own scope ceiling), only while the user can still read the project's
// schedules, and only to a schedule in projectID whose stored creator
// (CreatedBy, written by the hub from the authenticated identity at create)
// is that user and whose current authority (its initiator attribution) is
// still that user: after an owner edits or resumes it, it runs under the
// owner and the creator path no longer applies.
//
// When it allows the request it also returns the schedule's
// AuthorizationRevision it checked. The pause or delete handler reloads the
// schedule and refuses (creatorStopCurrent) if the revision changed in
// between, so an owner's re-authorization landing after this check is not
// overridden.
func (s *Server) scheduleCreatorMayStop(r *http.Request, projectID, scheduleID string) (int, bool) {
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if isNilIdentity(identity) || identity.ID() == "" {
		return 0, false
	}
	if _, ok := identity.(UserIdentity); !ok || identity.Type() == "agent" {
		return 0, false
	}
	if !sessionCredentialAllowed(ctx) {
		return 0, false
	}
	if !s.scheduledEventAccessDecision(ctx, projectID, ActionRead, nil).allowed {
		return 0, false
	}
	sc, err := s.store.GetSchedule(ctx, scheduleID)
	if err != nil || sc == nil || sc.ProjectID != projectID {
		return 0, false
	}
	if sc.CreatedBy == "" || sc.CreatedBy != identity.ID() {
		return 0, false
	}
	// The schedule must also still run under the creator's own authority:
	// once someone else re-authorized it (an edit or resume records them as
	// the initiator), stopping it is theirs to decide, not the creator's.
	initiator := s.scheduledInitiator(sc.InitiatorAttribution)
	if initiator.LegacyUnknown || initiator.PrincipalKind != identity.Type() ||
		initiator.PrincipalID != identity.ID() {
		return 0, false
	}
	return sc.AuthorizationRevision, true
}

// creatorStopCurrent reports whether a pause or delete may go ahead on
// schedule, the row the handler reloaded. creatorRevision is nil unless the
// creator path (scheduleCreatorMayStop) allowed the request, and then holds
// the authorization revision that check saw: when the reloaded row has
// another one, someone re-authorized the schedule in between, so it answers
// 409 and returns false. The write that follows is conditioned on the same
// revision (creatorStopGuard), for a re-authorization landing after the
// reload.
func creatorStopCurrent(w http.ResponseWriter, schedule *store.Schedule, creatorRevision *int) bool {
	if creatorRevision == nil || schedule.AuthorizationRevision == *creatorRevision {
		return true
	}
	writeCreatorStopConflict(w)
	return false
}

// creatorStopGuard, inside the transaction of a creator-path delete, writes
// schedule's unchanged status conditioned on creatorRevision, so the delete
// commits only while the schedule still has the revision the creator check
// saw: the conditional update locks the row (Postgres) or runs in the
// single writer (SQLite), and fails with store.ErrRevisionConflict once a
// re-authorization has landed.
func creatorStopGuard(ctx context.Context, tx store.Store, schedule *store.Schedule, creatorRevision int) error {
	guard := *schedule
	return tx.UpdateSchedule(ctx, &guard, store.ScheduleFieldMask{Status: true}, creatorRevision, true, nil)
}

// writeCreatorStopConflict answers 409 for a creator-path pause or delete
// of a schedule re-authorized after the creator check.
func writeCreatorStopConflict(w http.ResponseWriter) {
	writeError(w, http.StatusConflict, ErrCodeRevisionConflict,
		"schedule was re-authorized while this request was checked; refresh and retry", nil)
}
