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
	"log/slog"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// This file holds the authority of scheduled work. A schedule (or one-shot
// scheduled event) carries an authorization revision: the initiator
// attribution of the last request that changed what a future dispatch does,
// the revision counter, and the frozen effect ceiling of that request's
// credential (AuthorityCeiling). The three are written together.

// Mutation types of the schedule lifecycle writes that change no authority.
const (
	mutationTypeSchedulePause        = "schedule_pause"
	mutationTypeScheduleDelete       = "schedule_delete"
	mutationTypeScheduledEventCancel = "scheduled_event_cancel"
)

// revisionAuthorityCeiling returns the frozen effect ceiling to record on a
// schedule revision authored by the request's identity: the ceiling
// sourceEffectCeiling computes for that credential. It applies to every
// event type (dispatch_agent and message alike). action is the authoring
// action (create, or update for an edit or resume), recorded on a denial.
// On an error it writes the response and returns ok=false, before anything
// is written:
//   - a lookup fault → 500;
//   - a credential that cannot be recorded as an authority source → 403 with
//     details.denied_by="delegation_ceiling".
func (s *Server) revisionAuthorityCeiling(w http.ResponseWriter, r *http.Request, projectID string, action Action) (store.EffectCeiling, bool) {
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
		logAuthzDenial(r, identity, Resource{Type: "schedule", ParentType: "project", ParentID: projectID}, action,
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
	return newSchedulingAudit(ctx, mutationType, "schedule", scheduleID)
}

// newScheduledEventAudit returns the audit record of a one-shot scheduled
// event lifecycle write (a cancel), with the request's actor applied. It is
// written in the same transaction as the write it records.
func newScheduledEventAudit(ctx context.Context, mutationType, eventID string) *store.MutationAuditRecord {
	return newSchedulingAudit(ctx, mutationType, "scheduled_event", eventID)
}

func newSchedulingAudit(ctx context.Context, mutationType, targetType, targetID string) *store.MutationAuditRecord {
	record := &store.MutationAuditRecord{
		MutationType: mutationType,
		TargetType:   targetType,
		TargetID:     targetID,
		Timestamp:    time.Now(),
	}
	auditActorFromContext(ctx).ApplyActor(record)
	applyHubActorFallback(record)
	return record
}

// Fire-time denials of scheduled authority. Each one fails the fire; the
// event records failed with the error text, which names no credential
// material.
var (
	// errScheduledAuthorityUnrecorded: the event carries no recorded
	// authorization revision (legacy attribution), or a recorded revision
	// without a recorded ceiling. The remedy it names writes a new
	// revision: resuming a paused schedule re-authorizes the resumer and
	// records its ceiling, and creating an event records its author's. A
	// metadata-only edit writes no revision, so it is not named.
	errScheduledAuthorityUnrecorded = errors.New("schedule authority not recorded; pause and resume the schedule, or recreate the event")
	// errScheduledAuthorityDenied: the revision's principal or credential no
	// longer carries authority, or the revision is inconsistent.
	errScheduledAuthorityDenied = errors.New("schedule authority denied")
)

// scheduledEventPermissions maps an event type to the permission its fire
// exercises in the event's project. resolveScheduledAuthority checks the
// revision principal's current project admission for it. An event type with
// no entry is denied.
//
// For exec (ptone/scion#7883) the project admission this checks is only a
// project-boundary and liveness gate: ProjectAdmissionForClass admits on
// project membership, not on a binding of agent.attach. The real decision
// is authorizeScheduledExecFire's target-scoped agent.attach check, which
// must never be skipped.
var scheduledEventPermissions = map[string]string{
	"dispatch_agent":       "agent.create",
	"message":              scheduledMessagePermission,
	scheduledEventTypeExec: scheduledExecPermission,
}

// ScheduledAuthority aliases FireAuthority until all scheduled actions migrate
// to FireAction.
// Removed in PR 2 (ptone/scion#8029).
type ScheduledAuthority = FireAuthority

// resolveScheduledAuthority forwards to resolveRecordedAuthority.
// Removed in PR 2 (ptone/scion#8029).
func (s *Server) resolveScheduledAuthority(ctx context.Context, evt store.ScheduledEvent) (ScheduledAuthority, Identity, error) {
	return s.resolveRecordedAuthority(ctx, scheduledRecordedAuthority(evt), evt.ProjectID, scheduledEventPermissions[evt.EventType])
}

// revisionCeilingConsistent forwards to fireRevisionCeilingConsistent.
// Removed in PR 2 (ptone/scion#8029).
func revisionCeilingConsistent(credentialKind string, c store.EffectCeiling) error {
	return fireRevisionCeilingConsistent(credentialKind, c)
}

// scheduledUserAccount applies the account-status rule (checkUserAccount)
// to a scheduled revision's user principal and returns its active users
// row. A lookup fault is returned as a fault (not errScheduledAuthorityDenied).
// Every other refusal, including a lookup that returns neither a row nor an
// error, is errScheduledAuthorityDenied with reasonPrincipalInactive.
func scheduledUserAccount(ctx context.Context, users userRowReader, principalID string) (*store.User, error) {
	user, err := checkUserAccount(ctx, users, PrincipalContext{Kind: PrincipalKindUser, ID: principalID})
	switch {
	case err == nil:
		return user, nil
	case errors.Is(err, errUserLookupNoRow):
		// Must precede the fault case: checkUserAccount reports a lookup
		// with neither a row nor an error as a lookup fault, which the
		// scheduled path refuses as an inactive principal.
		return nil, fmt.Errorf("%w: %s", errScheduledAuthorityDenied, reasonPrincipalInactive)
	case isProjectAccessLookupFault(err):
		return nil, fmt.Errorf("scheduled authority: %w", err)
	default:
		return nil, fmt.Errorf("%w: %s", errScheduledAuthorityDenied, reasonPrincipalInactive)
	}
}

// resolveScheduledUser forwards to resolveFireUser.
// Removed in PR 2 (ptone/scion#8029).
func (s *Server) resolveScheduledUser(ctx context.Context, auth *ScheduledAuthority, projectID, permissionID string) (Identity, error) {
	return s.resolveFireUser(ctx, auth, projectID, permissionID)
}

// resolveScheduledAgent forwards to resolveFireAgent.
// Removed in PR 2 (ptone/scion#8029).
func (s *Server) resolveScheduledAgent(ctx context.Context, auth *ScheduledAuthority, projectID string) (Identity, error) {
	return s.resolveFireAgent(ctx, auth, projectID)
}

// scheduledEffectCeiling returns the ceiling and provenance to freeze for a
// write made at fire time under auth, the authority resolveScheduledAuthority
// returned for evt.
//
// Ceiling: the revision's frozen ceiling. For an agent principal it is
// intersected with the ceiling a request-authorized write by that agent carries at
// fire time (agentRowEffectCeiling, including the deliver eligibility of the
// agent's current chain), so a permission survives only if both allow it.
// It is never a live UAT or agent ceiling on its own.
//
// Provenance: SourceCredentialKind=scheduler, the revision principal, the
// event, schedule and revision references, and the event's four initiator
// fields copied verbatim.
func (s *Server) scheduledEffectCeiling(ctx context.Context, auth ScheduledAuthority, evt store.ScheduledEvent) (store.EffectCeiling, store.AuthorityProvenance, error) {
	ceiling := auth.Ceiling
	if auth.PrincipalKind == store.DelegationPrincipalAgent {
		if auth.agent == nil {
			return store.EffectCeiling{}, store.AuthorityProvenance{}, fmt.Errorf("%w: principal agent not resolved", errScheduledAuthorityDenied)
		}
		row, err := s.authzService.agentRowEffectCeiling(ctx, auth.agent)
		if err != nil {
			if cause, structural := ceilingDenyCauseForError(err); structural {
				return store.EffectCeiling{}, store.AuthorityProvenance{}, fmt.Errorf("%w: %s: %v", errScheduledAuthorityDenied, cause, err)
			}
			return store.EffectCeiling{}, store.AuthorityProvenance{}, fmt.Errorf("scheduled authority: agent ceiling: %w", err)
		}
		ceiling, err = intersectEffectCeilings(ceiling, row)
		if err != nil {
			return store.EffectCeiling{}, store.AuthorityProvenance{}, fmt.Errorf("%w: %v", errScheduledAuthorityDenied, err)
		}
	}
	return ceiling, store.AuthorityProvenance{
		ProvenanceVersion:           store.ProvenanceVersionV1,
		SourcePrincipalKind:         auth.PrincipalKind,
		SourcePrincipalID:           auth.PrincipalID,
		SourceCredentialKind:        store.SourceCredentialScheduler,
		SourceEventID:               evt.ID,
		SourceScheduleID:            evt.ScheduleID,
		SourceAuthorizationRevision: evt.AuthorizationRevision,
		InitiatorPrincipalKind:      evt.InitiatorPrincipalKind,
		InitiatorPrincipalID:        evt.InitiatorPrincipalID,
		InitiatorCredentialKind:     evt.InitiatorCredentialKind,
		InitiatorCredentialID:       evt.InitiatorCredentialID,
	}, nil
}

// intersectEffectCeilings returns the ceiling that allows exactly what both
// a and b allow. Principal is the identity element. Two bounded ceilings
// intersect over the registry (each under its own version) into bounded V1,
// keeping b's boundary. Any non-principal ceiling that Frozen cannot read
// (unrecorded or unknown kind) is an error, so the intersection never
// widens past a ceiling it cannot read.
func intersectEffectCeilings(a, b store.EffectCeiling) (store.EffectCeiling, error) {
	frozen := make([]permissions.FrozenPermissionCeiling, 0, 2)
	for _, c := range []store.EffectCeiling{a, b} {
		if c.Kind == store.EffectCeilingPrincipal {
			continue
		}
		f, ok := c.Frozen()
		if !ok {
			return store.EffectCeiling{}, fmt.Errorf("cannot intersect a %q ceiling", c.Kind)
		}
		frozen = append(frozen, f)
	}
	if a.Kind == store.EffectCeilingPrincipal {
		return b, nil
	}
	if b.Kind == store.EffectCeilingPrincipal {
		return a, nil
	}
	out := foldCeilings(frozen, 0)
	out.BoundaryKind = b.BoundaryKind
	out.BoundaryProjectID = b.BoundaryProjectID
	return out, nil
}

// errScheduledChildDeletedDuringCreate fails a fire whose scheduled child was
// deleted, or is held by a delete, by the time its synchronous dispatch
// returned.
var errScheduledChildDeletedDuringCreate = errors.New("agent was deleted while it was being created")

// scheduledChildLive reports whether the scheduled child is live after its
// synchronous dispatch, by the rule the created publish uses: a missing row
// or one deletedOrDeleteHeld is not live; a failed re-read counts as live.
func (s *Server) scheduledChildLive(ctx context.Context, agent *store.Agent) bool {
	fresh, err := s.store.GetAgent(ctx, agent.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return false
	case err != nil:
		return true
	}
	return !deletedOrDeleteHeld(fresh)
}
