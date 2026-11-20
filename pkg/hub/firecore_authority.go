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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
)

type fireHistoryCreatorKey struct{}

var errNoRecordedAuthorityRule = errors.New("action has no recorded authority rule")

// withFireHistoryCreator attaches the definition's historical CreatedBy value
// to ctx so fireStanding can check both the revision principal and historical
// creator without coupling the fire core to store.ScheduledEvent.
func withFireHistoryCreator(ctx context.Context, createdBy string) context.Context {
	if createdBy == "" {
		return ctx
	}
	return context.WithValue(ctx, fireHistoryCreatorKey{}, createdBy)
}

func fireHistoryCreatorFromContext(ctx context.Context) string {
	v, _ := ctx.Value(fireHistoryCreatorKey{}).(string)
	return v
}

// FireAuthority is hubfire.RecordedAuthority resolved at fire time: the
// principal and credential of the recorded authorization revision and its
// frozen effect ceiling.
type FireAuthority struct {
	PrincipalKind  string // store.DelegationPrincipalUser or store.DelegationPrincipalAgent
	PrincipalID    string
	CredentialKind string // session | dev_local | uat | agent (legacy_unknown never reaches here)
	CredentialID   string
	Ceiling        store.EffectCeiling
	Revision       int
	ProjectID      string
	Recorded       hubfire.RecordedAuthority

	// user is the principal's user row (user principals), loaded at fire
	// time; agent is the principal's agent row (agent principals).
	user  *store.User
	agent *store.Agent

	// historyCreator is the historical creator ID carried by the adapter (if
	// any), checked alongside the revision principal in fireStanding.
	historyCreator string
}

// resolveRecordedAuthority returns the authority a deferred fire runs under
// and an Identity that carries it, or an error that denies the fire. All rules
// fail closed:
//
//  1. Legacy attribution (AttributionVersion 0 or credential kind
//     legacy_unknown) -> errScheduledAuthorityUnrecorded.
//  2. A recorded revision whose ceiling is unrecorded, of an unknown kind, of
//     a kind its credential cannot produce, or of a version Allows does not
//     interpret -> errScheduledAuthorityUnrecorded.
//  3. A user principal must exist, be active and hold current admission to
//     projectID for permissionID. Then, by credential: session -> user
//     identity; dev_local -> local development authority enabled on this
//     server and principal is DevUserID, then user identity for DevUserID;
//     uat -> token exists, is unrevoked and unexpired and belongs to the
//     principal, then scoped identity carrying the token's live ceiling.
//  4. An agent principal must exist, not be deleted and be in projectID. Its
//     identity carries ceilingFilteredAgentScopes over mintCandidateScopes and
//     its stored ancestry.
//  5. Any lookup error denies.
//
// The returned identity is never passed to sourceEffectCeiling.
func (s *Server) resolveRecordedAuthority(ctx context.Context, rec hubfire.RecordedAuthority, projectID, permissionID string) (FireAuthority, Identity, error) {
	initiator := s.scheduledInitiator(rec.Attribution)
	if initiator.LegacyUnknown {
		return FireAuthority{}, nil, errScheduledAuthorityUnrecorded
	}
	if s.authzService == nil {
		return FireAuthority{}, nil, fmt.Errorf("scheduled authority: no authorization service")
	}
	auth := FireAuthority{
		PrincipalKind:  initiator.PrincipalKind,
		PrincipalID:    initiator.PrincipalID,
		CredentialKind: initiator.CredentialKind,
		CredentialID:   initiator.CredentialID,
		Ceiling:        rec.Ceiling,
		Revision:       rec.Attribution.AuthorizationRevision,
		ProjectID:      projectID,
		Recorded:       rec,
		historyCreator: fireHistoryCreatorFromContext(ctx),
	}
	if auth.PrincipalID == "" {
		return FireAuthority{}, nil, fmt.Errorf("%w: revision has no principal", errScheduledAuthorityDenied)
	}
	// A dev_local revision is recorded with the local development user's
	// identity kind ("dev"); its principal is that user. Any other principal
	// kind on a dev_local revision is inconsistent and denies.
	if auth.CredentialKind == store.InitiatorCredentialKindDevLocal {
		if auth.PrincipalKind != string(PrincipalKindDev) {
			return FireAuthority{}, nil, fmt.Errorf("%w: dev_local revision with principal kind %q", errScheduledAuthorityDenied, auth.PrincipalKind)
		}
		auth.PrincipalKind = store.DelegationPrincipalUser
	}
	if err := fireRevisionCeilingConsistent(auth.CredentialKind, auth.Ceiling); err != nil {
		return FireAuthority{}, nil, err
	}
	// resolveRecordedAuthority takes permissionID rather than an event/action
	// type name so both schedule and trigger definitions share one signature;
	// resolveScheduledAuthority maps errNoRecordedAuthorityRule back to the
	// base "event type %q has no scheduled authority rule" text.
	if permissionID == "" {
		return FireAuthority{}, nil, fmt.Errorf("%w: %w", errScheduledAuthorityDenied, errNoRecordedAuthorityRule)
	}

	switch auth.PrincipalKind {
	case store.DelegationPrincipalUser:
		identity, err := s.resolveFireUser(ctx, &auth, projectID, permissionID)
		if err != nil {
			return FireAuthority{}, nil, err
		}
		return auth, identity, nil
	case store.DelegationPrincipalAgent:
		identity, err := s.resolveFireAgent(ctx, &auth, projectID)
		if err != nil {
			return FireAuthority{}, nil, err
		}
		return auth, identity, nil
	default:
		return FireAuthority{}, nil, fmt.Errorf("%w: unsupported principal kind %q", errScheduledAuthorityDenied, auth.PrincipalKind)
	}
}

// fireRevisionCeilingConsistent checks that a recorded revision's ceiling is
// recorded and is the kind its credential produces: principal for session and
// dev_local, bounded (with a version Allows interprets) for uat and agent.
func fireRevisionCeilingConsistent(credentialKind string, c store.EffectCeiling) error {
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

// resolveFireUser applies rule 3 of resolveRecordedAuthority.
func (s *Server) resolveFireUser(ctx context.Context, auth *FireAuthority, projectID, permissionID string) (Identity, error) {
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

	user, err := scheduledUserAccount(ctx, s.store, auth.PrincipalID)
	if err != nil {
		return nil, err
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
	case tok.ExpiresAt == nil || time.Now().After(*tok.ExpiresAt):
		return nil, fmt.Errorf("%w: access token expired", errScheduledAuthorityDenied)
	case tok.UserID != user.ID:
		return nil, fmt.Errorf("%w: access token owner does not match the principal", errScheduledAuthorityDenied)
	}
	boundary := TokenBoundary{Kind: BoundaryKind(tok.BoundaryKind), ProjectID: tok.ProjectID}
	return NewScopedUserIdentityWithBoundary(userIdentity, boundary, tok.Scopes, tok.ID, tok.NormalizedCeiling()), nil
}

// resolveFireAgent applies rule 4 of resolveRecordedAuthority.
func (s *Server) resolveFireAgent(ctx context.Context, auth *FireAuthority, projectID string) (Identity, error) {
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

// fireStanding refuses a deferred fire whose target agent is held or whose
// authoring agent is not in good standing (ptone/scion#3433). target is nil
// for targetless actions (dispatch_agent).
func (s *Server) fireStanding(ctx context.Context, auth FireAuthority, target *store.Agent) error {
	refuse := func(cause string, err error) error {
		slog.Warn("Scheduler: scheduled event refused at fire time",
			"eventID", auth.Recorded.FireID, "definitionKind", auth.Recorded.DefinitionKind,
			"definitionID", auth.Recorded.DefinitionID, "projectID", auth.ProjectID,
			"creator", auth.historyCreator, "cause", cause, "error", err)
		if target != nil {
			return errScheduledMessageRefused
		}
		return fmt.Errorf("scheduled dispatch refused: %s", cause)
	}
	if target != nil {
		held, err := s.agentHeld(ctx, target.ID)
		if err != nil {
			return refuse("target hold lookup failed", err)
		}
		if held {
			return refuse("target agent is suspended", nil)
		}
	}
	var authors []string
	if auth.PrincipalKind == store.DelegationPrincipalAgent && auth.PrincipalID != "" {
		authors = append(authors, auth.PrincipalID)
	} else if init := s.scheduledInitiator(auth.Recorded.Attribution); !init.LegacyUnknown &&
		init.PrincipalKind == store.DelegationPrincipalAgent && init.PrincipalID != "" {
		authors = append(authors, init.PrincipalID)
	}
	if auth.historyCreator != "" {
		authors = append(authors, auth.historyCreator)
	}
	for _, id := range authors {
		creator, err := s.store.GetAgent(ctx, id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return refuse("author lookup failed", err)
		}
		if creator == nil {
			continue
		}
		if err := s.agentStanding(ctx, creator.ID); err != nil {
			return refuse("authoring agent is not in good standing", err)
		}
	}
	return nil
}

// fireCreatorName returns the display creator name recorded in
// AppliedConfig.CreatorName for an authority-resolved fire: an agent principal
// is named by its agent Name, a user principal by their email.
func fireCreatorName(auth FireAuthority) string {
	switch {
	case auth.agent != nil:
		return auth.agent.Name
	case auth.user != nil:
		return auth.user.Email
	default:
		return ""
	}
}

// fireEffectCeiling returns the ceiling and provenance to freeze for an
// authority-producing write made at fire time under auth.
//
// Ceiling: the revision's frozen ceiling. For an agent principal it is
// intersected with the ceiling a request-authorized write by that agent
// carries at fire time (agentRowEffectCeiling, including the deliver
// eligibility of the agent's current chain), so a permission survives only if
// both allow it. It is never a live UAT or agent ceiling on its own.
//
// Provenance: SourceCredentialKind=scheduler, the revision principal, the
// fire, schedule (when DefinitionKind == schedule), and revision references,
// and the recorded attribution's four initiator fields copied verbatim.
func (s *Server) fireEffectCeiling(ctx context.Context, auth FireAuthority) (store.EffectCeiling, store.AuthorityProvenance, error) {
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
	var scheduleID string
	if auth.Recorded.DefinitionKind == hubfire.DefinitionKindSchedule {
		scheduleID = auth.Recorded.DefinitionID
	}
	attr := auth.Recorded.Attribution
	return ceiling, store.AuthorityProvenance{
		ProvenanceVersion:           store.ProvenanceVersionV1,
		SourcePrincipalKind:         auth.PrincipalKind,
		SourcePrincipalID:           auth.PrincipalID,
		SourceCredentialKind:        store.SourceCredentialScheduler,
		SourceEventID:               auth.Recorded.FireID,
		SourceScheduleID:            scheduleID,
		SourceAuthorizationRevision: attr.AuthorizationRevision,
		InitiatorPrincipalKind:      attr.InitiatorPrincipalKind,
		InitiatorPrincipalID:        attr.InitiatorPrincipalID,
		InitiatorCredentialKind:     attr.InitiatorCredentialKind,
		InitiatorCredentialID:       attr.InitiatorCredentialID,
	}, nil
}
