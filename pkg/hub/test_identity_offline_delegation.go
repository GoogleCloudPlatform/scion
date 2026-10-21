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
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// Offline member-tier workload issuer delegations (ptone/scion#4240, phase
// DL; dl-design.md).
//
// The offline host command `scion server test-infra delegate` writes a
// delegation whose grantor kind is store.DelegationGrantorTestInfraOffline.
// There is no human grantor whose live bindings could bound it, so DEC-3
// C1's live-human-grantor check is replaced, for this kind only, by ONE
// live, fail-closed gate (offlineGrantorGate), run when the request
// authenticates and again inside every mint transaction
// (recheckWorkloadDelegation):
//
//   - the delegation's grantor kind is offline and its scope is the member
//     scope (the kind never holds an admin scope);
//   - three-way hub id binding: the running hub's pinned hub id equals the
//     delegation's hub id and the marker's hub id, and the delegation's
//     grantor id is the reserved id of that hub;
//   - the running hub's --test-mode is member or above;
//   - the marker is present, of class test, and is the same marker instance
//     the delegation was bound to (an unmark followed by a new mark ends
//     every delegation bound to the old marker);
//   - the delegation is neither revoked nor expired (checkDelegationLive,
//     including the offline maximum age);
//   - the reserved grantor id names no user row.
//
// Every read is live (no cache), and any read error denies. Each request
// the gate admits writes one test_identity_delegation_redeem_offline
// mutation audit row; a write failure denies.
//
// Authority: the reserved grantor has no bindings. Decide Step 5b4
// (offlineDelegationBinding) adds a request-local synthetic grant of
// exactly test_identity.issue at hub scope, only for a request whose state
// says the gate passed for THIS request; the delegation's frozen ceiling
// ({test_identity.issue}, hub boundary) still intersects it.

// testIdentityDelegationRedeemOfflineMutation is the mutation type of the
// per-redeem audit row of an offline delegation.
const testIdentityDelegationRedeemOfflineMutation = "test_identity_delegation_redeem_offline"

// testInfraMarkerAuditID names the marker in the redeem audit (the fixed id
// of the single marker row, the offline package's MarkerID).
const testInfraMarkerAuditID = "test-infra-marker"

// offlineGrantorAuditKind is the actor principal kind of the redeem audit
// row: the grantor kind, never "user".
const offlineGrantorAuditKind = store.DelegationGrantorTestInfraOffline

// offlineGrantorDisplayName is the display name of the context-only
// identity built for an offline delegation's reserved grantor.
const offlineGrantorDisplayName = "Test infrastructure (offline delegation)"

// offlineGateFacts are the live facts the offline gate read for one
// request, for the audit records.
type offlineGateFacts struct {
	hubID           string
	markerCreatedAt time.Time
	level           TestMode
}

// sameMarkerInstant reports whether two marker creation times are the same
// instant at microsecond precision (the hub-maintenance timestamp
// normalization may rewrite the stored text format, never the instant).
func sameMarkerInstant(a, b time.Time) bool {
	return a.UTC().Truncate(time.Microsecond).Equal(b.UTC().Truncate(time.Microsecond))
}

// offlineGrantorGate is the single live redeem gate of an offline
// delegation. st is the store to read through: the server store at
// authentication, the transaction store inside a mint. It returns the facts
// it read, or a 403 refusal on any failed condition or read error.
func (a *workloadIssuerAuthenticator) offlineGrantorGate(ctx context.Context, st store.Store, d *store.TestIdentityDelegation) (*offlineGateFacts, *workloadDelegationRefusal) {
	if a == nil || st == nil || d == nil {
		return nil, refuse403("offline delegation gate unavailable")
	}
	now := a.clock()
	switch {
	case d.EffectiveGrantorKind() != store.DelegationGrantorTestInfraOffline:
		return nil, refuse403("offline gate: delegation grantor kind is not offline")
	case d.Scope != store.TestInfraOfflineDelegationScope:
		return nil, refuse403("offline gate: an offline delegation holds only the member scope")
	case a.hubID == "":
		return nil, refuse403("offline gate: the hub has no resolved hub id")
	case d.HubID != a.hubID:
		return nil, refuse403("offline gate: delegation hub id differs from this hub's id")
	case d.GrantorID != store.TestInfraOfflineGrantorID(a.hubID):
		return nil, refuse403("offline gate: delegation grantor is not this hub's reserved offline grantor")
	case d.MarkerCreatedAt == nil:
		return nil, refuse403("offline gate: delegation is bound to no marker")
	}
	if refusal := a.checkDelegationLive(d, now); refusal != nil {
		return nil, refusal
	}
	if a.testModeLevel == nil {
		return nil, refuse403("offline gate: test-mode level unavailable")
	}
	level := a.testModeLevel()
	if !level.Capabilities().TestIdentities {
		return nil, refuse403("offline gate: the hub does not run --test-mode at member or above")
	}
	marker, err := st.GetTestInfraBinding(ctx)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, refuse403("offline gate: test infrastructure marker absent")
	case err != nil:
		return nil, refuse403("offline gate: test infrastructure marker could not be read")
	case marker == nil:
		return nil, refuse403("offline gate: test infrastructure marker could not be read")
	case !config.IsTestHubClass(marker.Class):
		return nil, refuse403("offline gate: test infrastructure marker class is not test")
	case marker.HubID != a.hubID:
		return nil, refuse403("offline gate: test infrastructure marker names a different hub id")
	case !sameMarkerInstant(marker.CreatedAt, *d.MarkerCreatedAt):
		return nil, refuse403("offline gate: test infrastructure marker is not the one the delegation was bound to")
	}
	// The reserved grantor id must never name a user row; a lookup error
	// other than not-found denies too.
	if _, err := st.GetUser(ctx, d.GrantorID); err == nil {
		return nil, refuse403("offline gate: reserved grantor id names a user")
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, refuse403("offline gate: reserved grantor id could not be checked")
	}
	return &offlineGateFacts{hubID: a.hubID, markerCreatedAt: marker.CreatedAt, level: level}, nil
}

// auditOfflineRedeem writes the per-redeem audit row of a request the
// offline gate admitted. A missing writer or a write failure denies.
func (a *workloadIssuerAuthenticator) auditOfflineRedeem(ctx context.Context, r *http.Request, d *store.TestIdentityDelegation, sub, runLabel string, facts *offlineGateFacts, now time.Time) *workloadDelegationRefusal {
	if a.writeAudit == nil || facts == nil {
		return refuse403("offline redeem audit unavailable")
	}
	record := &store.MutationAuditRecord{
		Timestamp:           now,
		MutationType:        testIdentityDelegationRedeemOfflineMutation,
		ActorPrincipalKind:  offlineGrantorAuditKind,
		ActorPrincipalID:    d.GrantorID,
		ActorCredentialID:   d.ID,
		ActorCredentialType: string(CredentialKindWorkloadDelegation),
		ExecutorKind:        "workload",
		ExecutorID:          sub,
		TargetType:          testIdentityDelegationAuditTarget,
		TargetID:            d.ID,
		AfterSummary: fmt.Sprintf("offline issuer delegation redeemed: grantor_kind=%s hub_id=%s marker_id=%s marker_created_at=%s test_mode=%s delegation_id=%s workload_sub=%s route=%s %s run_label=%q",
			store.DelegationGrantorTestInfraOffline, facts.hubID, testInfraMarkerAuditID,
			facts.markerCreatedAt.UTC().Format(time.RFC3339Nano), string(facts.level), d.ID, sub,
			r.Method, logging.RequestPath(r), runLabel),
	}
	if err := a.writeAudit(ctx, a.store, record); err != nil {
		return refuse403("offline redeem audit could not be written")
	}
	return nil
}

// offlineGrantorUser is the context-only user the workload arm builds the
// identity for when an offline delegation passed the gate. It is never
// stored; its id is the delegation's reserved grantor id.
func offlineGrantorUser(d *store.TestIdentityDelegation) *store.User {
	return &store.User{
		ID:          d.GrantorID,
		DisplayName: offlineGrantorDisplayName,
		Role:        store.UserRoleMember,
		Status:      store.UserStatusActive,
	}
}

// offlineDelegationBinding is Decide Step 5b4: the request-local synthetic
// grant of an offline delegation. It returns a hub-scope (system) candidate
// binding for exactly test_identity.issue, and its role, only when ALL hold:
//
//   - the permission decided is test_identity.issue;
//   - the principal is a user whose identity is the hub-boundary scoped
//     identity the workload arm built (its credential id is the
//     delegation id);
//   - the request credential context is a workload delegation with that id;
//   - the request's workload delegation state names that delegation, its
//     grantor kind is offline, its scope is the member scope, its grantor
//     id is the principal, and offlineGatePassed is true (set only by the
//     arm after offlineGrantorGate passed for this request).
//
// Otherwise it returns nils. It reads only the request context and its
// arguments, and shares no state with the agent synthetic bindings.
func offlineDelegationBinding(ctx context.Context, principal PrincipalContext, credential CredentialContext, permissionID string) (*CandidateBinding, *RolePermissions) {
	if permissionID != permissionTestIdentityIssue || principal.Kind != PrincipalKindUser || principal.ID == "" {
		return nil, nil
	}
	scoped, ok := principal.Identity.(*ScopedUserIdentity)
	if !ok || scoped == nil {
		return nil, nil
	}
	reqCred := GetCredentialContextFromContext(ctx)
	if reqCred.Kind != CredentialKindWorkloadDelegation {
		return nil, nil
	}
	st := workloadDelegationFromContext(ctx)
	if st == nil || !st.offlineGatePassed || st.delegation == nil {
		return nil, nil
	}
	d := st.delegation
	switch {
	case d.ID == "",
		d.EffectiveGrantorKind() != store.DelegationGrantorTestInfraOffline,
		d.Scope != store.TestInfraOfflineDelegationScope,
		d.GrantorID != principal.ID,
		scoped.ID() != principal.ID,
		scoped.CredentialID() != d.ID,
		reqCred.ID != d.ID,
		credential.ID != d.ID:
		return nil, nil
	}
	id := "synthetic:test-infra-offline-delegation:" + d.ID
	role := &RolePermissions{
		RoleID:      id,
		RoleName:    "test-infra-offline-delegation",
		ScopeType:   ScopeTypeSystem,
		Permissions: map[string]struct{}{permissionTestIdentityIssue: {}},
	}
	cb := &CandidateBinding{
		BindingID:        id,
		RoleDefinitionID: id,
		PrincipalType:    "user",
		PrincipalID:      principal.ID,
		ScopeType:        ScopeTypeSystem,
	}
	return cb, role
}
