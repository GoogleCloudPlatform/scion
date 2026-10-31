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
	"log/slog"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Re-authorization of held config edits at fold time (ptone/scion#5864).
//
// A held edit is applied by a later start (or by the fold of a PATCH to an
// agent with no container), often without the saver on the request. Each
// pending key carries the principal that saved its current value
// (store.AgentPendingConfig.KeySavers, written only by holdAgentConfigPatch
// after the save's own agent.update authorization). Before a key is folded,
// its saver is re-checked against the saver's CURRENT grants and account:
// a key whose saver can no longer edit the agent is dropped, never applied.
//
// Every drop is audited to the fold's own sink (agent_pending_config_discarded
// with one of the saver_* triggers) and named in a warning. The fold never
// writes to the live per-arm security sinks (the test-admin request sink,
// the JWT row block's rejection logs, the UAT and credential rejection
// logs): those record live requests, and a fold is not one.

// Triggers of an agent_pending_config_discarded record for pending keys a
// fold dropped because their saver's re-check did not allow them.
const (
	// pendingDiscardTriggerSaverUnauthorized: the re-check of a supported
	// saver kind (user, dev) denied agent.update, or the saver's account
	// no longer exists.
	pendingDiscardTriggerSaverUnauthorized = "saver_unauthorized"
	// pendingDiscardTriggerSaverUnresolvable: a saver kind not rebuilt at
	// fold time (agent, federated, visitor, broker, an unknown kind), or a
	// user row no live user arm admits (federated, transport, reserved).
	pendingDiscardTriggerSaverUnresolvable = "saver_unresolvable"
	// pendingDiscardTriggerSaverAccountInactive: the saver's account exists
	// but is not active (suspended, invited, pending, or a test identity
	// that is off or expired).
	pendingDiscardTriggerSaverAccountInactive = "saver_account_inactive"
	// pendingDiscardTriggerSaverDelegated: the edit was saved with an agent
	// delegated credential, which is never re-checked.
	pendingDiscardTriggerSaverDelegated = "saver_delegated"
)

// Warning texts for dropped and deferred keys, each after the key list.
const (
	pendingSaverUnauthorizedNote    = "not applied; its saver can no longer edit this agent, so the edit was discarded"
	pendingSaverUnresolvableNote    = "not applied; edits saved by this kind of principal are not applied at start yet, so the edit was discarded"
	pendingSaverAccountInactiveNote = "not applied; its saver's account is not active, so the edit was discarded"
	pendingSaverDelegatedNote       = "not applied; edits saved with a delegated credential are not applied at start, so the edit was discarded"
	pendingDeferredNote             = "held edits by non-admin users wait until maintenance ends"
)

// pendingDropTriggers lists the saver drop triggers in warning order.
var pendingDropTriggers = []string{
	pendingDiscardTriggerSaverUnauthorized,
	pendingDiscardTriggerSaverAccountInactive,
	pendingDiscardTriggerSaverUnresolvable,
	pendingDiscardTriggerSaverDelegated,
}

// pendingDropNotes maps each saver drop trigger to its warning text.
var pendingDropNotes = map[string]string{
	pendingDiscardTriggerSaverUnauthorized:    pendingSaverUnauthorizedNote,
	pendingDiscardTriggerSaverUnresolvable:    pendingSaverUnresolvableNote,
	pendingDiscardTriggerSaverAccountInactive: pendingSaverAccountInactiveNote,
	pendingDiscardTriggerSaverDelegated:       pendingSaverDelegatedNote,
}

// pendingFoldWarnings returns the warnings for keys a fold dropped (by
// trigger) and deferred (maintenance mode): one per trigger with keys, in a
// fixed order, then the deferral.
func pendingFoldWarnings(drops map[string][]string, deferred []string) []string {
	var out []string
	for _, trigger := range pendingDropTriggers {
		if keys := drops[trigger]; len(keys) > 0 {
			out = append(out, strings.Join(keys, ", ")+": "+pendingDropNotes[trigger])
		}
	}
	if len(deferred) > 0 {
		out = append(out, strings.Join(deferred, ", ")+": "+pendingDeferredNote)
	}
	return out
}

// errPendingReauthIndeterminate marks a fold that could not re-check a
// saver: a lookup or authorization fault, never a decision. Nothing is
// dropped or written; the start (or the PATCH) is refused with a 503 and
// can be retried.
var errPendingReauthIndeterminate = errors.New("the agent's held edits could not be re-checked")

// pendingReauthUnavailableMessage is the 503 answer of a start or PATCH
// whose fold hit errPendingReauthIndeterminate.
const pendingReauthUnavailableMessage = "the agent's held edits could not be re-checked; retry"

// saverOutcome is the fold's decision for one pending key's saver.
type saverOutcome int

const (
	// saverApply: the saver may still edit the agent; the key folds.
	saverApply saverOutcome = iota
	// saverUnauthorized, saverUnresolvable, saverAccountInactive and
	// saverDelegated drop the key, with the matching trigger.
	saverUnauthorized
	saverUnresolvable
	saverAccountInactive
	saverDelegated
	// saverDeferred: maintenance mode is on and the saver is not an
	// unscoped local platform admin; the key stays pending, unaudited.
	saverDeferred
)

// trigger is the discard trigger of a dropping outcome; "" otherwise.
func (o saverOutcome) trigger() string {
	switch o {
	case saverUnauthorized:
		return pendingDiscardTriggerSaverUnauthorized
	case saverUnresolvable:
		return pendingDiscardTriggerSaverUnresolvable
	case saverAccountInactive:
		return pendingDiscardTriggerSaverAccountInactive
	case saverDelegated:
		return pendingDiscardTriggerSaverDelegated
	}
	return ""
}

// pendingSaverIdentity is a saver resolved at fold time: the outcome so
// far, and, for a saver that is re-checked, the identity rebuilt from its
// stored row.
type pendingSaverIdentity struct {
	out      saverOutcome
	identity Identity
	// reason is the specific cause of a drop, for the fold's log line only.
	reason string
}

// pendingSaverClientType is the client type of a user identity rebuilt for
// a fold's re-check.
const pendingSaverClientType = "pending_fold"

// pendingSaverRef is the KeySavers reference of the saving caller: its
// precise kind and id, "<Type()>:<ID()>". The workstation dev user is
// "dev:<id>" only when the identity is the trusted local dev user
// (isTrustedLocalDevUser: Type() == "dev" alone proves nothing); any other
// identity claiming that type is "dev_untrusted:<id>", which a fold drops.
// A user access token's identity (ScopedUserIdentity) is "user:<id>".
func pendingSaverRef(ctx context.Context) string {
	id := GetIdentityFromContext(ctx)
	if id == nil {
		return ""
	}
	if id.Type() == string(PrincipalKindDev) {
		if isTrustedLocalDevUser(id) {
			return string(PrincipalKindDev) + ":" + id.ID()
		}
		return "dev_untrusted:" + id.ID()
	}
	return id.Type() + ":" + id.ID()
}

// pendingKeySaverRef returns the saver reference of pending key k (its
// KeySavers entry) and whether it is a legacy reference: a row with no
// entry for k falls back to SavedBy, in the format pendingSaver writes.
func pendingKeySaverRef(pc *store.AgentPendingConfig, k string) (ref string, legacy bool) {
	if pc == nil {
		return "", false
	}
	if ref, ok := pc.KeySavers[k]; ok {
		return ref, false
	}
	return pc.SavedBy, true
}

// legacySaverKind maps a legacy SavedBy reference to the saver kind it was
// written for, before the kind switch (ruling B): "user:<DevUserID>" is the
// dev user; "user:<issuer>:<subject>" is a federated user (decided before
// any row read); any other "user:<id>" is a user, re-checked only when its
// row is positively native (legacyUserRowNative); "agent:<issuer>:<id>" is
// a federated agent and "agent:<id>" an agent. Anything else keeps its
// kind.
func legacySaverKind(ref string) (kind, id string) {
	kind, id, _ = strings.Cut(ref, ":")
	switch kind {
	case "user":
		switch {
		case id == DevUserID:
			return string(PrincipalKindDev), id
		case strings.Contains(id, ":"):
			return "federated_user", id
		}
	case "agent":
		if strings.Contains(id, ":") {
			return "federated_agent", id
		}
	}
	return kind, id
}

// legacyUserRowNative is ruling B's positive allow: a legacy "user:<id>"
// reference is re-checked as a user only when its row reports a native
// account (store.User.IsHumanKind: Kind "" or "human"). A visitor's row is
// always Kind federated, and kind is immutable, so a visitor id never
// passes; test identities and unknown kinds do not pass either.
func legacyUserRowNative(u *store.User) bool {
	return u != nil && u.IsHumanKind()
}

// pendingSaverCache holds a fold attempt's saver decisions, one lookup per
// distinct saver reference.
type pendingSaverCache struct {
	s        *Server
	agent    *store.Agent
	resolved map[string]pendingSaverIdentity
	allowed  map[string]pendingSaverIdentity
}

func newPendingSaverCache(s *Server, agent *store.Agent) *pendingSaverCache {
	return &pendingSaverCache{
		s:        s,
		agent:    agent,
		resolved: map[string]pendingSaverIdentity{},
		allowed:  map[string]pendingSaverIdentity{},
	}
}

// cacheKey distinguishes a legacy reference from a precise one: the same
// text is resolved under different rules.
func pendingSaverCacheKey(ref string, legacy bool) string {
	if legacy {
		return "legacy\x00" + ref
	}
	return ref
}

// resolve is resolvePendingSaver, memoized.
func (c *pendingSaverCache) resolve(ctx context.Context, ref string, legacy bool) (pendingSaverIdentity, error) {
	key := pendingSaverCacheKey(ref, legacy)
	if d, ok := c.resolved[key]; ok {
		return d, nil
	}
	d, err := c.s.resolvePendingSaver(ctx, ref, legacy)
	if err != nil {
		return d, err
	}
	c.resolved[key] = d
	return d, nil
}

// mayApply is pendingSaverMayApply, memoized.
func (c *pendingSaverCache) mayApply(ctx context.Context, ref string, legacy bool) (pendingSaverIdentity, error) {
	key := pendingSaverCacheKey(ref, legacy)
	if d, ok := c.allowed[key]; ok {
		return d, nil
	}
	d, err := c.resolve(ctx, ref, legacy)
	if err != nil {
		return d, err
	}
	if d.out == saverApply {
		d, err = c.s.pendingSaverMayApply(ctx, c.agent, d)
		if err != nil {
			return d, err
		}
	}
	c.allowed[key] = d
	return d, nil
}

// pendingSaverMayApply re-checks a resolved saver (resolvePendingSaver)
// against its CURRENT grants: agent.update on the agent, the check the
// live PATCH makes (authorize, handlers_agents_core.go). A saver that
// resolution already dropped or deferred is returned as it is.
//
// Invariant (ruling A): for the principal, re-authorization at fold uses
// the same identity resolution, grants, account status, membership and
// tier as the live PATCH. It may be stricter, never weaker. Any principal
// gate later added to the live PATCH path is added to the fold, with a row
// in TestPendingReauth_MirrorsLivePatchGates. The one sanctioned
// difference is credential-level: token validity and expiry, a user access
// token's boundary, scopes and ceiling, session generation, agent token
// scopes, delegation scope and the JWT role claim are not replayed; the
// principal's current grants are re-checked instead (disclosed in the API
// reference).
//
// Design note: authorization here is at the action and key level, never
// the value level. Every KeySavers entry was save-authorized by
// construction (agent.update before holdAgentConfigPatch, its only
// writer). If per-value scopes are introduced later (a grant allowing only
// some values of a key), this re-check must be revisited, because a
// current-grant check at key level would not enforce them. The decision is
// for the whole key, never a sub-field.
//
// Decision mapping (§4.0(b)): allowed applies. An indeterminate deny
// (Decision.IsIndeterminate, or a cause pendingFoldDenyCauses marks
// indeterminate) returns errPendingReauthIndeterminate: nothing is dropped
// or written. Any other deny is confirmed by one fresh evaluation with no
// request-local authorization memo (some lookup faults still deny with no
// cause): a second determinate deny drops the key (saver_unauthorized), a
// second allow applies it, and a second indeterminate result refuses the
// fold.
func (s *Server) pendingSaverMayApply(ctx context.Context, agent *store.Agent, d pendingSaverIdentity) (pendingSaverIdentity, error) {
	if d.out != saverApply {
		return d, nil
	}
	if s.authzService == nil || d.identity == nil {
		return d, errPendingReauthIndeterminate
	}
	res := agentResource(agent)
	decision := s.authzService.CheckAccess(ctx, d.identity, res, ActionUpdate)
	if decision.Allowed {
		return d, nil
	}
	if pendingFoldDenyIndeterminate(decision) {
		return d, errPendingReauthIndeterminate
	}
	confirm := s.authzService.CheckAccess(maskAllAuthzMemo(ctx), d.identity, res, ActionUpdate)
	switch {
	case confirm.Allowed:
		return d, nil
	case pendingFoldDenyIndeterminate(confirm):
		return d, errPendingReauthIndeterminate
	}
	d.out, d.reason = saverUnauthorized, "agent.update denied: "+confirm.Reason
	return d, nil
}

// pendingFoldDenyResult is how a fold treats a deny's cause.
type pendingFoldDenyResult int

const (
	// pendingFoldDenyDeterminate: a policy fact; confirmed, then dropped.
	pendingFoldDenyDeterminate pendingFoldDenyResult = iota
	// pendingFoldDenyFault: the check could not be decided; the fold is
	// refused with nothing dropped.
	pendingFoldDenyFault
)

// pendingFoldDenyCauses maps every DenyCause to the fold's treatment. The
// delegation-ceiling and credential causes are not expected for a rebuilt
// user or dev identity and are mapped for completeness.
// TestPendingReauth_DenyCauseMapExhaustive finds every DenyCause constant
// in the package and fails on one missing here.
var pendingFoldDenyCauses = map[DenyCause]pendingFoldDenyResult{
	"":                                       pendingFoldDenyDeterminate,
	DenyCauseResolutionError:                 pendingFoldDenyFault,
	DenyCauseCeilingError:                    pendingFoldDenyFault,
	DenyCauseCeilingOrphaned:                 pendingFoldDenyDeterminate,
	DenyCauseCeilingDelegatorLacksPermission: pendingFoldDenyDeterminate,
	DenyCauseCeilingUnrecorded:               pendingFoldDenyDeterminate,
	DenyCauseCeilingEffectExceeded:           pendingFoldDenyDeterminate,
	DenyCauseCeilingResourceMissing:          pendingFoldDenyDeterminate,
	DenyCauseCeilingProvenanceMissing:        pendingFoldDenyDeterminate,
	DenyCauseCeilingProvenanceAmbiguous:      pendingFoldDenyDeterminate,
	DenyCauseCeilingProvenanceStale:          pendingFoldDenyDeterminate,
	DenyCauseCeilingSourceNotAllowed:         pendingFoldDenyDeterminate,
}

// pendingFoldDenyIndeterminate reports whether a deny leaves the re-check
// undecided: Decision.IsIndeterminate, or a cause mapped as a fault. A
// cause added later and not yet mapped counts as determinate only when
// IsIndeterminate says so.
func pendingFoldDenyIndeterminate(d Decision) bool {
	return d.IsIndeterminate() || pendingFoldDenyCauses[d.DenyCause] == pendingFoldDenyFault
}

// resolvePendingSaver rebuilds the saver of a pending key from its
// reference, by an explicit switch on the saver's kind (§4.1). Only user
// and dev savers are rebuilt, from their stored rows, through every
// principal gate the live PATCH applies to such a row; every other kind is
// dropped and fails closed. A lookup or kind-read fault returns
// errPendingReauthIndeterminate.
func (s *Server) resolvePendingSaver(ctx context.Context, ref string, legacy bool) (pendingSaverIdentity, error) {
	var kind, id string
	if legacy {
		kind, id = legacySaverKind(ref)
	} else {
		kind, id, _ = strings.Cut(ref, ":")
	}
	switch kind {
	case string(PrincipalKindUser):
		// A4-row, in order: fault and missing; the row-kind gates; status;
		// then maintenance mode. CheckAccess follows (pendingSaverMayApply).
		return s.resolvePendingUserSaver(ctx, id, legacy)
	case string(PrincipalKindDev):
		// The workstation dev user, only while this server admits dev
		// authority at all; its account must be active; it is re-checked
		// through the same CheckAccess (no always-allow).
		return s.resolvePendingDevSaver(ctx)
	case string(PrincipalKindAgentDelegated):
		// Never re-checked (OQ-4): dropped, audited and warned.
		return pendingSaverIdentity{out: saverDelegated, reason: "agent delegated saver"}, nil
	case "agent", "federated_user", "federated_agent", "visitor", "broker", "federated_service", "dev_untrusted":
		// Not rebuilt in v1: dropped, failing closed.
		return pendingSaverIdentity{out: saverUnresolvable, reason: "saver kind " + kind + " is not re-checked"}, nil
	default:
		// An unknown kind, or no saver at all.
		return pendingSaverIdentity{out: saverUnresolvable, reason: "unknown saver kind"}, nil
	}
}

// resolvePendingUserSaver rebuilds a user saver from its row, applying the
// hub user JWT arm's complete user-row block (the most complete row-gate
// set of any live user arm, so the fold never admits a row no live arm
// admits), in this order:
//  1. checkUserAccount: a lookup fault refuses the fold; a missing row
//     drops (saver_unauthorized).
//  2. Row-kind gates, before status: a legacy reference whose row is not
//     native, a federated row, a transport identity's email, a reserved
//     test-identity row (saver_unresolvable); a test fixture that is off
//     or expired (saver_account_inactive); an admin-kind test row
//     (testAdminRowRejection, read through s.authConfig.KindForPrincipal
//     at call time): a kind-read error refuses the fold, the tier off or
//     the identity expired drops (saver_account_inactive), a domain or
//     role mismatch drops (saver_unresolvable). The row's own role stands
//     in for the token's role claim, a credential-level check.
//  3. Status: suspended or any other non-active status drops
//     (saver_account_inactive).
//  4. Maintenance mode (adminModeMiddleware): a saver that is not an
//     unscoped local platform admin is deferred, not dropped.
func (s *Server) resolvePendingUserSaver(ctx context.Context, id string, legacy bool) (pendingSaverIdentity, error) {
	u, uErr := checkUserAccount(ctx, s.store, PrincipalContext{Kind: PrincipalKindUser, ID: id})
	refusal := classifyUserAccount(u, uErr)
	switch refusal {
	case accountLookupFault:
		slog.WarnContext(ctx, "pending config: saver lookup failed", "error", uErr)
		return pendingSaverIdentity{}, errPendingReauthIndeterminate
	case accountMissing:
		return pendingSaverIdentity{out: saverUnauthorized, reason: "saver account not found"}, nil
	}
	if u == nil {
		return pendingSaverIdentity{}, errPendingReauthIndeterminate
	}
	if legacy && !legacyUserRowNative(u) {
		return pendingSaverIdentity{out: saverUnresolvable, reason: "legacy saver row is not a native account"}, nil
	}
	if u.IsFederated() {
		return pendingSaverIdentity{out: saverUnresolvable, reason: "federated saver row"}, nil
	}
	if s.isTransportIdentity(u.Email) {
		return pendingSaverIdentity{out: saverUnresolvable, reason: "transport identity"}, nil
	}
	now := s.testIdentities.clock()
	switch reason := testFixtureRejection(u, s.testIdentities.enabled, now); reason {
	case "":
	case "reserved_test_identity_domain":
		return pendingSaverIdentity{out: saverUnresolvable, reason: reason}, nil
	default:
		return pendingSaverIdentity{out: saverAccountInactive, reason: reason}, nil
	}
	_, reason, kErr := testAdminRowRejection(ctx, s.authConfig.KindForPrincipal, s.authConfig.TestAdminTierEnabled, u, u.Role, now)
	if kErr != nil {
		// Not a drop: the kind could not be read, so the fold is refused
		// and retried; nothing is audited as discarded.
		slog.WarnContext(ctx, "pending config: saver kind lookup failed", "error", kErr)
		return pendingSaverIdentity{}, errPendingReauthIndeterminate
	}
	switch reason {
	case "":
	case "test_admin_tier_disabled", "test_admin_identity_expired":
		return pendingSaverIdentity{out: saverAccountInactive, reason: reason}, nil
	default:
		return pendingSaverIdentity{out: saverUnresolvable, reason: reason}, nil
	}
	if refusal != accountAllowed {
		return pendingSaverIdentity{out: saverAccountInactive, reason: accountRefusalReason(refusal)}, nil
	}
	identity := NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, pendingSaverClientType)
	return s.deferDuringMaintenance(pendingSaverIdentity{out: saverApply, identity: identity}, identity), nil
}

// resolvePendingDevSaver rebuilds the workstation dev user saver.
func (s *Server) resolvePendingDevSaver(ctx context.Context) (pendingSaverIdentity, error) {
	if !s.authzService.devLocalAuthorityEnabled() {
		return pendingSaverIdentity{out: saverUnresolvable, reason: "dev authority is off"}, nil
	}
	u, uErr := checkUserAccount(ctx, s.store, devUserPrincipal())
	switch refusal := classifyUserAccount(u, uErr); refusal {
	case accountAllowed:
	case accountLookupFault:
		slog.WarnContext(ctx, "pending config: dev saver lookup failed", "error", uErr)
		return pendingSaverIdentity{}, errPendingReauthIndeterminate
	case accountMissing:
		return pendingSaverIdentity{out: saverUnauthorized, reason: "dev account not found"}, nil
	default:
		return pendingSaverIdentity{out: saverAccountInactive, reason: accountRefusalReason(refusal)}, nil
	}
	identity := NewDevUser(s.authConfig.DevUserCfg)
	return s.deferDuringMaintenance(pendingSaverIdentity{out: saverApply, identity: identity}, identity), nil
}

// deferDuringMaintenance mirrors adminModeMiddleware for a rebuilt saver:
// while maintenance mode is on, a saver that is not an unscoped local
// platform admin is deferred. Maintenance is a transient hub state, not a
// property of the principal, so its keys stay pending (dropping them would
// lose data, applying them would be weaker than the live gate) and are
// re-checked when maintenance ends. The dev user's role is admin, so a dev
// saver is never deferred.
func (s *Server) deferDuringMaintenance(d pendingSaverIdentity, user UserIdentity) pendingSaverIdentity {
	if s.maintenance == nil || !s.maintenance.IsEnabled() || IsUnscopedLocalPlatformAdmin(user) {
		return d
	}
	d.out, d.reason = saverDeferred, "maintenance mode"
	return d
}
