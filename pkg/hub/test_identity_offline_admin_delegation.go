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

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Offline hub-admin test identity delegations.
//
// With the startup setting server.hub.test_identities.offline_admin_delegations
// set to this hub's id, at --test-mode=admin or above, an offline delegation
// (grantor kind store.DelegationGrantorTestInfraOffline, written only by
// `scion server test-infra delegate --scope hub-admin`) may hold the
// hub-admin scope and mint test hub-admin identities through
// POST /api/v1/test-identities/admin. Nothing else: an offline delegation
// never re-issues an admin-kind token, never mints an admin web-login code,
// and never covers test super-admin issuance.
//
// The live human super-admin grantor of a human admin delegation is
// replaced, for this kind and scope only, by the offline gate
// (offlineGrantorGate: marker instance, three-way hub id binding, the
// reserved grantor naming no user, not revoked or expired, the scope
// available and the subject allow-listed at the admin tier) plus the
// conditions here: the setting on, the live --test-mode level admin or
// above, and the permission decided byte-equal to test_identity.issue_admin.
// Every condition is read again inside the issuance transaction
// (recheckOfflineAdminDelegationGrantor). A hub that starts without the
// setting ends every offline hub-admin delegation and revokes the
// identities they issued (EndOfflineAdminDelegationsWhenOff).

// OfflineAdminDelegationsConfig is ServerConfig.OfflineAdminDelegations:
// the offline hub-admin delegation setting cmd validated at startup.
type OfflineAdminDelegationsConfig struct {
	// HubID is the value of
	// server.hub.test_identities.offline_admin_delegations: the hub id the
	// setting is bound to. Empty means off.
	HubID string
}

func (c OfflineAdminDelegationsConfig) requested() bool { return c.HubID != "" }

// Named refusals of the offline hub-admin delegation setting. Callers wrap
// them with %w and add detail; match with errors.Is.
var (
	ErrOfflineAdminDelegationsBelowAdmin = testAdminGateErr("offline_admin_delegations", "below_admin_level",
		config.TestIdentityOfflineAdminDelegationsKey+" requires --test-mode=admin or --test-mode=super-admin; remove the setting or start the hub at an admin level")
	ErrOfflineAdminDelegationsHubIDMismatch = testAdminGateErr("offline_admin_delegations", "hub_id_mismatch",
		"the value of "+config.TestIdentityOfflineAdminDelegationsKey+" must equal this hub's resolved hub id")
	ErrOfflineAdminDelegationsRequiresAuditSink = testAdminGateErr("offline_admin_delegations", "requires_audit_sink",
		config.TestIdentityOfflineAdminDelegationsKey+" requires the test admin audit sink")
)

// ErrOfflineAdminDelegationsCleanup refuses startup when a hub without the
// offline hub-admin delegation setting could not end the offline hub-admin
// delegations and the identities they issued.
var ErrOfflineAdminDelegationsCleanup = errors.New("offline hub-admin delegations could not be ended at startup")

// testAdminReasonOfflineDelegation is the refusal detail reason of an
// admin issuance through an offline delegation that the offline hub-admin
// conditions refuse.
const testAdminReasonOfflineDelegation = "offline_admin_delegation_refused"

// offlineAdminCleanupActorID names the startup cleanup in audit records
// and in a revoked delegation's revoked_by.
const offlineAdminCleanupActorID = "offline-admin-delegations-off"

// validateOfflineAdminDelegationsConfig re-checks, at construction, the
// setting cmd validated: an admin level whose gate is on, and a hub id
// binding equal to cfg.HubID.
func validateOfflineAdminDelegationsConfig(cfg ServerConfig) error {
	c := cfg.OfflineAdminDelegations
	if !c.requested() {
		return nil
	}
	if !cfg.TestMode.level().IsAdminLevel() || !cfg.TestAdminGate.HubAdmin {
		return fmt.Errorf("%w: --test-mode is %q", ErrOfflineAdminDelegationsBelowAdmin, cfg.TestMode.level())
	}
	if cfg.HubID == "" || c.HubID != cfg.HubID {
		return fmt.Errorf("%w: setting %q, resolved hub id %q", ErrOfflineAdminDelegationsHubIDMismatch, c.HubID, cfg.HubID)
	}
	return nil
}

// offlineAdminPermissionAdmitted reports whether permission may be decided
// for an offline hub-admin delegation: never a super-admin variant (checked
// first), otherwise only the one canonical permission ID, byte for byte.
func offlineAdminPermissionAdmitted(permission string) bool {
	if store.IsSuperAdminIssueVariant(permission) {
		return false
	}
	return permission == permissionTestIdentityIssueAdmin
}

// offlineAdminScopeAdmitted reports whether scope is the offline hub-admin
// scope: never a super-admin variant (checked first), otherwise only the
// one canonical scope, byte for byte.
func offlineAdminScopeAdmitted(scope string) bool {
	if store.IsSuperAdminIssueVariant(scope) {
		return false
	}
	// store.TestInfraOfflineAdminDelegationScope is the same scope;
	// TestOfflineAdminDelegation_Constants pins that.
	return scope == testIdentityDelegationAdminScope
}

// offlineAdminDelegationsOn reports whether the offline hub-admin
// delegation setting is on (immutable after New).
func (s *Server) offlineAdminDelegationsOn() bool {
	return s != nil && s.offlineAdmin.on
}

// offlineAdminState is the immutable offline hub-admin delegation switch
// captured by New. It has no setter.
type offlineAdminState struct {
	on bool
}

// recheckOfflineAdminDelegationGrantor stands in for
// recheckTestAdminDelegationGrantor when an offline delegation mints a test
// hub-admin, inside the issuance transaction and under the issuance lock.
// It re-reads the delegation through tx and admits the mint only when ALL
// hold: the permission is test_identity.issue_admin (a super-admin variant
// is refused first); the request's state is an offline delegation whose
// gate passed for this request with the hub-admin scope; the stored row is
// the same delegation, of the offline kind and the hub-admin scope; the
// setting is on; and the offline gate passes again on tx at the admin
// level. It returns errTestAdminIssuerNotLive on any failure, a store
// error as is.
func (s *Server) recheckOfflineAdminDelegationGrantor(ctx context.Context, tx store.Store, st *workloadDelegationState, permission string) error {
	if !offlineAdminPermissionAdmitted(permission) {
		return fmt.Errorf("%w: permission %q is not delegable offline", errTestAdminIssuerNotLive, permission)
	}
	if st == nil || st.delegation == nil || !st.offlineGatePassed || st.offline == nil || !st.offline.adminScope {
		return fmt.Errorf("%w: no passed offline hub-admin gate for this request", errTestAdminIssuerNotLive)
	}
	if !s.offlineAdminDelegationsOn() {
		return fmt.Errorf("%w: offline hub-admin delegations are off", errTestAdminIssuerNotLive)
	}
	if s.workloadIssuer == nil {
		return fmt.Errorf("%w: no workload issuer", errTestAdminIssuerNotLive)
	}
	d, err := tx.GetTestIdentityDelegation(ctx, st.delegation.ID)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%w: delegation not found", errTestAdminIssuerNotLive)
	}
	if err != nil {
		return err
	}
	switch {
	case d.EffectiveGrantorKind() != store.DelegationGrantorTestInfraOffline:
		return fmt.Errorf("%w: delegation grantor kind is not offline", errTestAdminIssuerNotLive)
	case !offlineAdminScopeAdmitted(d.Scope):
		return fmt.Errorf("%w: delegation scope is not the offline hub-admin scope", errTestAdminIssuerNotLive)
	case d.GrantorID != st.delegation.GrantorID:
		return fmt.Errorf("%w: delegation grantor changed", errTestAdminIssuerNotLive)
	}
	facts, refusal := s.workloadIssuer.offlineGrantorGate(ctx, tx, d)
	if refusal != nil {
		return fmt.Errorf("%w: %s", errTestAdminIssuerNotLive, refusal.reason)
	}
	if facts == nil || !facts.adminScope || !facts.level.IsAdminLevel() {
		return fmt.Errorf("%w: the hub does not run --test-mode at admin or above", errTestAdminIssuerNotLive)
	}
	return nil
}

// isOfflineNonMemberDelegation reports whether d is an offline delegation
// holding anything but the member scope: the hub-admin scope, or any other
// value a row could carry.
func isOfflineNonMemberDelegation(d *store.TestIdentityDelegation) bool {
	return d != nil && d.EffectiveGrantorKind() == store.DelegationGrantorTestInfraOffline &&
		d.Scope != store.TestInfraOfflineDelegationScope
}

// EndOfflineAdminDelegationsWhenOff runs once at startup, before the hub
// serves requests. When the offline hub-admin delegation setting is off, it
// revokes every offline delegation that holds anything but the member scope
// (writing a test_identity_delegation_revoke audit record for each), then
// ends every admin-kind test identity issued through one of them: its web
// sessions end and it is revoked as by admin revoke-all (deleted, or
// suspended with its role bindings removed), with a test_admin_revoke
// audit record (and a sink line when the sink is configured), whatever its
// current status. With the setting on it does nothing.
// Any failure returns ErrOfflineAdminDelegationsCleanup and the caller
// refuses to start.
func (s *Server) EndOfflineAdminDelegationsWhenOff(ctx context.Context) error {
	if s == nil || s.store == nil || s.offlineAdminDelegationsOn() {
		return nil
	}
	now := s.testIdentities.clock()
	delegations, err := s.store.ListTestIdentityDelegations(ctx, "", 0)
	if err != nil {
		return fmt.Errorf("%w: list delegations: %v", ErrOfflineAdminDelegationsCleanup, err)
	}
	actor := AuditActor{PrincipalKind: "system", PrincipalID: offlineAdminCleanupActorID}
	ended := make(map[string]bool)
	for i := range delegations {
		d := &delegations[i]
		if !isOfflineNonMemberDelegation(d) {
			continue
		}
		ended[d.ID] = true
		if d.RevokedAt != nil {
			continue
		}
		err := s.store.WithTx(ctx, func(tx store.Store) error {
			changed, err := tx.RevokeTestIdentityDelegation(ctx, d.ID, "system:"+offlineAdminCleanupActorID, now)
			if err != nil || !changed {
				return err
			}
			record := &store.MutationAuditRecord{
				MutationType:  testIdentityDelegationRevokeMutation,
				TargetType:    testIdentityDelegationAuditTarget,
				TargetID:      d.ID,
				BeforeSummary: testIdentityDelegationSummary(d),
				AfterSummary:  fmt.Sprintf(`{"reason":"%s is off at startup"}`, config.TestIdentityOfflineAdminDelegationsKey),
				Timestamp:     now,
			}
			actor.ApplyActor(record)
			return s.testIdentities.hooks.writeAudit(ctx, tx, record)
		})
		if err != nil {
			return fmt.Errorf("%w: revoke delegation %s: %v", ErrOfflineAdminDelegationsCleanup, d.ID, err)
		}
		slog.Warn("offline hub-admin delegation ended at startup: the offline admin delegation setting is off",
			"delegation_id", d.ID, "setting", config.TestIdentityOfflineAdminDelegationsKey)
	}
	if len(ended) == 0 {
		return nil
	}
	users, err := s.store.ListTestAdminUsers(ctx)
	if err != nil {
		return fmt.Errorf("%w: list test admin identities: %v", ErrOfflineAdminDelegationsCleanup, err)
	}
	for i := range users {
		u := &users[i]
		// Every status, Suspended included: revocation is terminal and
		// strips the role bindings, so a suspended identity cannot later be
		// reactivated with authority from a dead delegation.
		if u.IssuerDelegationID == nil || !ended[*u.IssuerDelegationID] {
			continue
		}
		if err := s.store.IncrementSessionGeneration(ctx, u.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%w: end web sessions of %s: %v", ErrOfflineAdminDelegationsCleanup, u.ID, err)
		}
		outcome, err := s.revokeTestAdminIdentityWith(ctx, u, actor, now, false)
		if err != nil {
			return fmt.Errorf("%w: revoke test admin identity %s: %v", ErrOfflineAdminDelegationsCleanup, u.ID, err)
		}
		if outcome == "deleted" {
			s.afterUserDeleted(ctx, u.ID)
		}
		slog.Warn("test admin identity revoked at startup: its offline hub-admin delegation ended",
			"test_identity_id", u.ID, "delegation_id", *u.IssuerDelegationID, "outcome", outcome)
	}
	return nil
}
