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
	"github.com/go-jose/go-jose/v4/jwt"
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
// ceiling sourceEffectCeiling computes for that credential. action is the
// authoring action (create, or update for an edit or resume), recorded on a
// denial. On an error it
// writes the response and returns ok=false, before anything is written:
//   - a lookup fault → 500;
//   - a credential that cannot be recorded as an authority source → 403 with
//     details.denied_by="delegation_ceiling", for a dispatch_agent revision.
//
// A message revision whose credential cannot be recorded as an authority
// source records the unrecorded ceiling instead: scheduled-message authority
// is decided by the scheduled-message rule, which does not read this
// ceiling, and an unrecorded ceiling never reads as principal.
func (s *Server) revisionAuthorityCeiling(w http.ResponseWriter, r *http.Request, projectID, eventType string, action Action) (store.EffectCeiling, bool) {
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
var scheduledEventPermissions = map[string]string{
	"dispatch_agent": "agent.create",
}

// ScheduledAuthority is the authority a fired scheduled event runs under:
// the principal and credential of its authorization revision, and the
// revision's frozen ceiling.
type ScheduledAuthority struct {
	PrincipalKind  string // store.DelegationPrincipalUser or store.DelegationPrincipalAgent
	PrincipalID    string
	CredentialKind string // session | dev_local | uat | agent (legacy_unknown never reaches here)
	CredentialID   string
	Ceiling        store.EffectCeiling
	ScheduleID     string
	EventID        string
	Revision       int

	// user is the principal's user row (user principals), loaded at fire
	// time; agent is the principal's agent row (agent principals).
	user  *store.User
	agent *store.Agent
}

// resolveScheduledAuthority returns the authority a fired event runs under,
// and an Identity that carries it, or an error that denies the fire. All
// rules fail closed:
//
//  1. Legacy attribution (AttributionVersion 0 or credential kind
//     legacy_unknown) → errScheduledAuthorityUnrecorded. There is no
//     CreatedBy fallback and no inference from descriptive fields.
//  2. A recorded revision whose ceiling is unrecorded, of an unknown kind, of
//     a kind its credential cannot produce, or of a version Allows does not
//     interpret → errScheduledAuthorityUnrecorded.
//  3. A user principal must exist, be active and hold current admission to
//     the event's project for the event type's permission. Then, by
//     credential: session → a user identity; dev_local → local development
//     authority enabled on this server and the principal is DevUserID (both
//     checked before the user lookup), then a user identity for DevUserID;
//     uat → the token exists, is unrevoked and unexpired and belongs to the
//     principal, then a scoped identity carrying the token's live ceiling.
//  4. An agent principal must exist, not be deleted and be in the event's
//     project. Its identity carries the scopes its token would carry
//     (ceilingFilteredAgentScopes over mintCandidateScopes) and its stored
//     ancestry.
//  5. Any lookup error denies.
//
// The returned identity is for Decide, CanDelegate and the template and
// service-account gates only. It is never passed to sourceEffectCeiling:
// fire-time writes take their ceiling and provenance from
// scheduledEffectCeiling.
func (s *Server) resolveScheduledAuthority(ctx context.Context, evt store.ScheduledEvent) (ScheduledAuthority, Identity, error) {
	initiator := s.scheduledInitiator(evt.InitiatorAttribution)
	if initiator.LegacyUnknown {
		return ScheduledAuthority{}, nil, errScheduledAuthorityUnrecorded
	}
	if s.authzService == nil {
		return ScheduledAuthority{}, nil, fmt.Errorf("scheduled authority: no authorization service")
	}
	auth := ScheduledAuthority{
		PrincipalKind:  initiator.PrincipalKind,
		PrincipalID:    initiator.PrincipalID,
		CredentialKind: initiator.CredentialKind,
		CredentialID:   initiator.CredentialID,
		Ceiling:        evt.AuthorityCeiling,
		ScheduleID:     evt.ScheduleID,
		EventID:        evt.ID,
		Revision:       evt.AuthorizationRevision,
	}
	if auth.PrincipalID == "" {
		return ScheduledAuthority{}, nil, fmt.Errorf("%w: revision has no principal", errScheduledAuthorityDenied)
	}
	// A dev_local revision is recorded with the local development user's
	// identity kind ("dev"); its principal is that user. Any other principal
	// kind on a dev_local revision is inconsistent and denies.
	if auth.CredentialKind == store.InitiatorCredentialKindDevLocal {
		if auth.PrincipalKind != string(PrincipalKindDev) {
			return ScheduledAuthority{}, nil, fmt.Errorf("%w: dev_local revision with principal kind %q", errScheduledAuthorityDenied, auth.PrincipalKind)
		}
		auth.PrincipalKind = store.DelegationPrincipalUser
	}
	if err := revisionCeilingConsistent(auth.CredentialKind, auth.Ceiling); err != nil {
		return ScheduledAuthority{}, nil, err
	}
	permissionID, ok := scheduledEventPermissions[evt.EventType]
	if !ok {
		return ScheduledAuthority{}, nil, fmt.Errorf("%w: event type %q has no scheduled authority rule", errScheduledAuthorityDenied, evt.EventType)
	}

	switch auth.PrincipalKind {
	case store.DelegationPrincipalUser:
		identity, err := s.resolveScheduledUser(ctx, &auth, evt.ProjectID, permissionID)
		if err != nil {
			return ScheduledAuthority{}, nil, err
		}
		return auth, identity, nil
	case store.DelegationPrincipalAgent:
		identity, err := s.resolveScheduledAgent(ctx, &auth, evt.ProjectID)
		if err != nil {
			return ScheduledAuthority{}, nil, err
		}
		return auth, identity, nil
	default:
		return ScheduledAuthority{}, nil, fmt.Errorf("%w: unsupported principal kind %q", errScheduledAuthorityDenied, auth.PrincipalKind)
	}
}

// revisionCeilingConsistent checks that a recorded revision's ceiling is
// recorded and is the kind its credential produces: principal for session
// and dev_local, bounded (with a version Allows interprets) for uat and
// agent.
func revisionCeilingConsistent(credentialKind string, c store.EffectCeiling) error {
	switch c.Kind {
	case store.EffectCeilingUnrecorded:
		return errScheduledAuthorityUnrecorded
	case store.EffectCeilingPrincipal, store.EffectCeilingBounded:
	default:
		return fmt.Errorf("%w: ceiling kind %q", errScheduledAuthorityUnrecorded, c.Kind)
	}
	want := store.EffectCeilingBounded
	switch credentialKind {
	case store.InitiatorCredentialKindSession, store.InitiatorCredentialKindDevLocal:
		want = store.EffectCeilingPrincipal
	case store.InitiatorCredentialKindUAT, store.InitiatorCredentialKindAgent:
	default:
		return fmt.Errorf("%w: unsupported credential kind %q", errScheduledAuthorityDenied, credentialKind)
	}
	if c.Kind != want {
		return fmt.Errorf("%w: %s credential with a %s ceiling", errScheduledAuthorityUnrecorded, credentialKind, c.Kind)
	}
	if c.Kind == store.EffectCeilingBounded && !knownCeilingVersion(c.Version) {
		return fmt.Errorf("%w: ceiling version not interpreted", errScheduledAuthorityUnrecorded)
	}
	return nil
}

// resolveScheduledUser applies rule 3 of resolveScheduledAuthority.
func (s *Server) resolveScheduledUser(ctx context.Context, auth *ScheduledAuthority, projectID, permissionID string) (Identity, error) {
	if auth.CredentialKind == store.InitiatorCredentialKindDevLocal {
		if !s.authzService.devLocalAuthorityEnabled() {
			return nil, fmt.Errorf("%w: %s: %s", errScheduledAuthorityDenied, errSourceNotAllowed, reasonDevLocalDisabled)
		}
		if auth.PrincipalID != DevUserID {
			return nil, fmt.Errorf("%w: %s: %s", errScheduledAuthorityDenied, errSourceNotAllowed, reasonPrincipalInactive)
		}
	}
	if auth.CredentialKind == store.InitiatorCredentialKindAgent {
		return nil, fmt.Errorf("%w: agent credential on a user principal", errScheduledAuthorityDenied)
	}

	user, err := s.store.GetUser(ctx, auth.PrincipalID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", errScheduledAuthorityDenied, reasonPrincipalInactive)
		}
		return nil, fmt.Errorf("scheduled authority: user lookup: %w", err)
	}
	if user == nil || user.Status != store.UserStatusActive {
		return nil, fmt.Errorf("%w: %s", errScheduledAuthorityDenied, reasonPrincipalInactive)
	}
	auth.user = user
	userIdentity := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "scheduler")

	admission, err := s.authzService.ProjectAdmissionForClass(ctx, PrincipalContext{
		Kind:     PrincipalKindUser,
		ID:       user.ID,
		Identity: userIdentity,
	}, projectID, permissionID, executionProjectClass(permissionID), nil)
	if err != nil {
		if errors.Is(err, ErrProjectAccessDenied) {
			return nil, fmt.Errorf("%w: principal %s lacks admission to the project", errScheduledAuthorityDenied, user.ID)
		}
		return nil, fmt.Errorf("scheduled authority: project admission: %w", err)
	}
	if !admission.Admitted {
		return nil, fmt.Errorf("%w: principal %s lacks admission to the project", errScheduledAuthorityDenied, user.ID)
	}

	if auth.CredentialKind != store.InitiatorCredentialKindUAT {
		return userIdentity, nil
	}
	if auth.CredentialID == "" {
		return nil, fmt.Errorf("%w: access token not recorded", errScheduledAuthorityDenied)
	}
	tok, err := s.store.GetUserAccessToken(ctx, auth.CredentialID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: access token not found", errScheduledAuthorityDenied)
		}
		return nil, fmt.Errorf("scheduled authority: access token lookup: %w", err)
	}
	switch {
	case tok == nil:
		return nil, fmt.Errorf("%w: access token not found", errScheduledAuthorityDenied)
	case tok.Revoked:
		return nil, fmt.Errorf("%w: access token revoked", errScheduledAuthorityDenied)
	case tok.ExpiresAt == nil || !time.Now().Before(*tok.ExpiresAt):
		return nil, fmt.Errorf("%w: access token expired", errScheduledAuthorityDenied)
	case tok.UserID != user.ID:
		return nil, fmt.Errorf("%w: access token owner does not match the principal", errScheduledAuthorityDenied)
	}
	boundary := TokenBoundary{Kind: BoundaryKind(tok.BoundaryKind), ProjectID: tok.ProjectID}
	return NewScopedUserIdentityWithBoundaryAndDecoration(userIdentity, boundary, tok.Scopes, tok.ID, tok.NormalizedCeiling(), nil), nil
}

// resolveScheduledAgent applies rule 4 of resolveScheduledAuthority.
func (s *Server) resolveScheduledAgent(ctx context.Context, auth *ScheduledAuthority, projectID string) (Identity, error) {
	if auth.CredentialKind != store.InitiatorCredentialKindAgent {
		return nil, fmt.Errorf("%w: %s credential on an agent principal", errScheduledAuthorityDenied, auth.CredentialKind)
	}
	agent, err := s.store.GetAgent(ctx, auth.PrincipalID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: principal agent not found", errScheduledAuthorityDenied)
		}
		return nil, fmt.Errorf("scheduled authority: agent lookup: %w", err)
	}
	if agent == nil || !agent.DeletedAt.IsZero() {
		return nil, fmt.Errorf("%w: principal agent deleted", errScheduledAuthorityDenied)
	}
	if agent.ProjectID == "" || agent.ProjectID != projectID {
		return nil, fmt.Errorf("%w: principal agent is not in the event's project", errScheduledAuthorityDenied)
	}
	scopes, err := s.authzService.ceilingFilteredAgentScopes(ctx, agent, s.authzService.mintCandidateScopes(agent))
	if err != nil {
		if cause, structural := ceilingDenyCauseForError(err); structural {
			return nil, fmt.Errorf("%w: %s: %v", errScheduledAuthorityDenied, cause, err)
		}
		return nil, fmt.Errorf("scheduled authority: agent ceiling: %w", err)
	}
	auth.agent = agent
	ancestry := make([]string, len(agent.Ancestry))
	copy(ancestry, agent.Ancestry)
	return &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agent.ID},
		ProjectID: agent.ProjectID,
		Scopes:    scopes,
		Ancestry:  ancestry,
	}}, nil
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
		ceiling = intersectEffectCeilings(ceiling, row)
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
// keeping b's boundary. Callers pass recorded ceilings only.
func intersectEffectCeilings(a, b store.EffectCeiling) store.EffectCeiling {
	if a.Kind == store.EffectCeilingPrincipal {
		return b
	}
	if b.Kind == store.EffectCeilingPrincipal {
		return a
	}
	fa, _ := a.Frozen()
	fb, _ := b.Frozen()
	out := foldCeilings([]permissions.FrozenPermissionCeiling{fa, fb}, 0)
	out.BoundaryKind = b.BoundaryKind
	out.BoundaryProjectID = b.BoundaryProjectID
	return out
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
