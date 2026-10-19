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

package testinfra

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/testidentitydelegation"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/google/uuid"
)

// Offline member-tier workload issuer delegations (ptone/scion#4240, phase
// DL). Delegate is the only writer of a delegation whose grantor kind is
// store.DelegationGrantorTestInfraOffline; it is called only by the offline
// host command `scion server test-infra delegate`. The hub API's store path
// refuses that kind, and a pkg/hub source test pins that no non-test pkg/hub
// file references this package.

// DelegateMutationType is the mutation_audits.mutation_type of the audit row
// Delegate writes in the same transaction as the delegation.
const DelegateMutationType = "test_identity_delegation_create_offline"

// RevokeDelegationMutationType is the mutation_audits.mutation_type of the
// audit row RevokeDelegation writes (the same type the hub API's revoke
// writes).
const RevokeDelegationMutationType = "test_identity_delegation_revoke"

// delegationLabelMaxRunes bounds an offline delegation's label (the API's
// bound).
const delegationLabelMaxRunes = 64

// Named refusals of Delegate and RevokeDelegation. Each one writes nothing.
var (
	ErrDelegateHubIDRequired     = errors.New("test-infra delegate: hub id must be non-empty and carry no surrounding whitespace")
	ErrDelegateMarkerHubMismatch = errors.New("test-infra delegate: the test infrastructure marker names a different hub id")
	ErrDelegateMarkerClass       = errors.New(`test-infra delegate: the test infrastructure marker class is not "test"`)
	ErrDelegateNotAllowlisted    = errors.New("test-infra delegate: the workload subject is not on server.hub.test_identities.workload_issuers with a max_tier covering member")
	ErrDelegateScope             = errors.New("test-infra delegate: an offline delegation holds only the member scope " + store.TestInfraOfflineDelegationScope)
	ErrDelegateTTL               = errors.New("test-infra delegate: the lifetime is out of bounds")
	ErrDelegateMemberCap         = errors.New("test-infra delegate: the member cap is out of bounds")
	ErrDelegateLabel             = errors.New("test-infra delegate: the label is too long or has control characters")
	ErrDelegateTooManyLive       = errors.New("test-infra delegate: this hub already holds the maximum number of live offline delegations")
	ErrDelegationNotFound        = errors.New("test-infra delegate revoke: no such delegation")
	ErrDelegationNotOffline      = errors.New("test-infra delegate revoke: the delegation is not an offline delegation; revoke it through the hub API")
	ErrDelegationOtherHub        = errors.New("test-infra delegate revoke: the delegation is bound to a different hub id")
)

// DelegateInput is the input of Delegate.
type DelegateInput struct {
	// HubID is the pinned hub id the delegation is bound to. The caller has
	// already checked it is pinned and equals the configuration's resolved
	// hub id; Delegate checks it against the marker.
	HubID string
	// WorkloadSub is the pinned Google subject of the delegated workload.
	WorkloadSub string
	// Scope must be store.TestInfraOfflineDelegationScope (empty means
	// that scope). Any other value, an admin scope in particular, is
	// refused.
	Scope string
	// WorkloadIssuers is the hub's startup allowlist
	// (server.hub.test_identities.workload_issuers) from the same
	// configuration; WorkloadSub must be on it with a max_tier covering
	// member.
	WorkloadIssuers []config.TestIdentityWorkloadIssuer
	// TTL is the delegation's lifetime; zero means
	// store.TestInfraOfflineDelegationDefaultTTL.
	TTL time.Duration
	// MemberCap is the per-delegation identity cap; zero means
	// store.TestInfraOfflineDelegationMaxMemberCap.
	MemberCap int
	// Label is an optional short label for the test window.
	Label string
	// Operator identifies who ran the command, for the audit row.
	Operator string
	// Host is the host name the command ran on.
	Host string
	// Now is the creation time (wall clock of the host).
	Now time.Time
}

// subCoversMember reports whether sub is on issuers with a max_tier that
// covers the member tier.
func subCoversMember(issuers []config.TestIdentityWorkloadIssuer, sub string) bool {
	for _, e := range issuers {
		if e.Sub != sub {
			continue
		}
		return e.MaxTier == config.WorkloadIssuerTierMember || e.MaxTier == config.WorkloadIssuerTierAdmin
	}
	return false
}

// Delegate creates an offline member-tier workload issuer delegation bound
// to in.HubID and to the current marker instance, and writes a
// DelegateMutationType audit row, in one transaction. It refuses (writing
// nothing) an empty or padded hub id, any scope but the member scope, a
// subject off the allowlist, a lifetime or cap out of bounds, a missing
// marker (ErrMarkerAbsent), a marker for another hub id or of another
// class, and a hub that already holds
// store.TestInfraOfflineMaxLiveDelegations live offline delegations. It
// returns the created delegation; it holds no secret.
func Delegate(ctx context.Context, client *ent.Client, in DelegateInput) (*store.TestIdentityDelegation, error) {
	if client == nil {
		return nil, errors.New("test-infra delegate: no database client")
	}
	hubID := strings.TrimSpace(in.HubID)
	if hubID == "" || hubID != in.HubID {
		return nil, ErrDelegateHubIDRequired
	}
	scope := in.Scope
	if scope == "" {
		scope = store.TestInfraOfflineDelegationScope
	}
	if scope != store.TestInfraOfflineDelegationScope {
		return nil, fmt.Errorf("%w (requested %q)", ErrDelegateScope, scope)
	}
	sub := in.WorkloadSub
	if sub == "" || strings.TrimSpace(sub) != sub || !subCoversMember(in.WorkloadIssuers, sub) {
		return nil, ErrDelegateNotAllowlisted
	}
	ttl := in.TTL
	if ttl == 0 {
		ttl = store.TestInfraOfflineDelegationDefaultTTL
	}
	if ttl < store.TestInfraOfflineDelegationMinTTL || ttl > store.TestInfraOfflineDelegationMaxTTL {
		return nil, fmt.Errorf("%w: must be between %s and %s", ErrDelegateTTL, store.TestInfraOfflineDelegationMinTTL, store.TestInfraOfflineDelegationMaxTTL)
	}
	memberCap := in.MemberCap
	if memberCap == 0 {
		memberCap = store.TestInfraOfflineDelegationMaxMemberCap
	}
	if memberCap < 1 || memberCap > store.TestInfraOfflineDelegationMaxMemberCap {
		return nil, fmt.Errorf("%w: must be between 1 and %d", ErrDelegateMemberCap, store.TestInfraOfflineDelegationMaxMemberCap)
	}
	label := strings.TrimSpace(in.Label)
	if utf8.RuneCountInString(label) > delegationLabelMaxRunes || !utf8.ValidString(label) || strings.ContainsFunc(label, isControl) {
		return nil, ErrDelegateLabel
	}
	operator := strings.TrimSpace(in.Operator)
	if operator == "" {
		operator = "unknown"
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	expiresAt := now.Add(ttl)

	tx, err := client.Tx(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	rollback := func(cause error) (*store.TestIdentityDelegation, error) {
		_ = tx.Rollback()
		return nil, cause
	}

	marker, err := tx.TestInfraMarker.Get(ctx, MarkerID)
	if err != nil {
		if ent.IsNotFound(err) {
			return rollback(ErrMarkerAbsent)
		}
		return rollback(fmt.Errorf("read test infrastructure marker: %w", err))
	}
	if marker.HubID != hubID {
		return rollback(fmt.Errorf("%w: marker %q, --hub-id %q", ErrDelegateMarkerHubMismatch, marker.HubID, hubID))
	}
	if !config.IsTestHubClass(marker.Class) {
		return rollback(ErrDelegateMarkerClass)
	}

	live, err := tx.TestIdentityDelegation.Query().
		Where(
			testidentitydelegation.GrantorKindEQ(testidentitydelegation.GrantorKindTestInfraOffline),
			testidentitydelegation.HubIDEQ(hubID),
			testidentitydelegation.RevokedAtIsNil(),
			testidentitydelegation.ExpiresAtGT(now),
		).
		Count(ctx)
	if err != nil {
		return rollback(fmt.Errorf("count live offline delegations: %w", err))
	}
	if live >= store.TestInfraOfflineMaxLiveDelegations {
		return rollback(fmt.Errorf("%w (%d)", ErrDelegateTooManyLive, store.TestInfraOfflineMaxLiveDelegations))
	}

	markerAt := marker.CreatedAt
	grantorID := store.TestInfraOfflineGrantorID(hubID)
	row, err := tx.TestIdentityDelegation.Create().
		SetID(uuid.New()).
		SetGrantorID(grantorID).
		SetWorkloadSub(sub).
		SetScope(scope).
		SetMemberCap(memberCap).
		SetLabel(label).
		SetCreatedAt(now).
		SetExpiresAt(expiresAt).
		SetGrantorKind(testidentitydelegation.GrantorKindTestInfraOffline).
		SetHubID(hubID).
		SetMarkerCreatedAt(markerAt).
		Save(ctx)
	if err != nil {
		return rollback(fmt.Errorf("write offline delegation: %w", err))
	}

	audit := entadapter.NewMutationAuditStore(tx.Client())
	if err := audit.CreateMutationAudit(ctx, &store.MutationAuditRecord{
		Timestamp:          now,
		MutationType:       DelegateMutationType,
		ActorPrincipalKind: "operator",
		ActorPrincipalID:   operator,
		TargetType:         "test_identity_delegation",
		TargetID:           row.ID.String(),
		AfterSummary: fmt.Sprintf("offline issuer delegation created: grantor_kind=%s hub_id=%s marker_id=%s marker_created_at=%s workload_sub=%s scope=%s member_cap=%d expires_at=%s host=%s operator=%s",
			store.DelegationGrantorTestInfraOffline, hubID, MarkerID, markerAt.UTC().Format(time.RFC3339Nano), sub, scope, memberCap,
			expiresAt.Format(time.RFC3339), in.Host, operator),
	}); err != nil {
		return rollback(fmt.Errorf("write %s audit row: %w", DelegateMutationType, err))
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit offline delegation: %w", err)
	}
	return &store.TestIdentityDelegation{
		ID:              row.ID.String(),
		GrantorID:       row.GrantorID,
		WorkloadSub:     row.WorkloadSub,
		Scope:           row.Scope,
		MemberCap:       row.MemberCap,
		Label:           row.Label,
		Created:         row.CreatedAt,
		ExpiresAt:       row.ExpiresAt,
		GrantorKind:     string(row.GrantorKind),
		HubID:           row.HubID,
		MarkerCreatedAt: row.MarkerCreatedAt,
	}, nil
}

// isControl reports whether r is a control character.
func isControl(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0)
}

// RevokeDelegationInput is the input of RevokeDelegation.
type RevokeDelegationInput struct {
	// HubID is the pinned hub id; the delegation must be bound to it.
	HubID string
	// ID is the delegation id.
	ID string
	// Operator identifies who ran the command, for the audit row and
	// revoked_by ("operator:<operator>").
	Operator string
	// Host is the host name the command ran on.
	Host string
	// Now is the revocation time (wall clock of the host).
	Now time.Time
}

// RevokeDelegation revokes an offline delegation bound to in.HubID and
// writes a RevokeDelegationMutationType audit row, in one transaction. It
// reports changed=false (and writes nothing) when the delegation is already
// revoked. It refuses an unknown id (ErrDelegationNotFound), a human-grantor
// delegation (ErrDelegationNotOffline: humans revoke through the hub API)
// and a delegation bound to another hub id (ErrDelegationOtherHub). It does
// not require the marker: revocation only ever removes authority.
func RevokeDelegation(ctx context.Context, client *ent.Client, in RevokeDelegationInput) (*store.TestIdentityDelegation, bool, error) {
	if client == nil {
		return nil, false, errors.New("test-infra delegate revoke: no database client")
	}
	hubID := strings.TrimSpace(in.HubID)
	if hubID == "" || hubID != in.HubID {
		return nil, false, ErrDelegateHubIDRequired
	}
	id, err := uuid.Parse(strings.TrimSpace(in.ID))
	if err != nil {
		return nil, false, ErrDelegationNotFound
	}
	operator := strings.TrimSpace(in.Operator)
	if operator == "" {
		operator = "unknown"
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()

	tx, err := client.Tx(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("begin transaction: %w", err)
	}
	rollback := func(cause error) (*store.TestIdentityDelegation, bool, error) {
		_ = tx.Rollback()
		return nil, false, cause
	}
	row, err := tx.TestIdentityDelegation.Get(ctx, id)
	if err != nil {
		if ent.IsNotFound(err) {
			return rollback(ErrDelegationNotFound)
		}
		return rollback(fmt.Errorf("read delegation: %w", err))
	}
	if row.GrantorKind != testidentitydelegation.GrantorKindTestInfraOffline {
		return rollback(ErrDelegationNotOffline)
	}
	if row.HubID != hubID {
		return rollback(fmt.Errorf("%w: delegation %q, --hub-id %q", ErrDelegationOtherHub, row.HubID, hubID))
	}
	view := &store.TestIdentityDelegation{
		ID:              row.ID.String(),
		GrantorID:       row.GrantorID,
		WorkloadSub:     row.WorkloadSub,
		Scope:           row.Scope,
		MemberCap:       row.MemberCap,
		Label:           row.Label,
		Created:         row.CreatedAt,
		ExpiresAt:       row.ExpiresAt,
		RevokedAt:       row.RevokedAt,
		RevokedBy:       row.RevokedBy,
		GrantorKind:     string(row.GrantorKind),
		HubID:           row.HubID,
		MarkerCreatedAt: row.MarkerCreatedAt,
	}
	if row.RevokedAt != nil {
		if err := tx.Rollback(); err != nil {
			return nil, false, fmt.Errorf("end transaction: %w", err)
		}
		return view, false, nil
	}
	revokedBy := "operator:" + operator
	n, err := tx.TestIdentityDelegation.Update().
		Where(testidentitydelegation.IDEQ(id), testidentitydelegation.RevokedAtIsNil()).
		SetRevokedAt(now).
		SetRevokedBy(revokedBy).
		Save(ctx)
	if err != nil {
		return rollback(fmt.Errorf("revoke delegation: %w", err))
	}
	if n == 0 {
		// Revoked concurrently: that revocation wrote its own audit row.
		if err := tx.Rollback(); err != nil {
			return nil, false, fmt.Errorf("end transaction: %w", err)
		}
		return view, false, nil
	}
	audit := entadapter.NewMutationAuditStore(tx.Client())
	if err := audit.CreateMutationAudit(ctx, &store.MutationAuditRecord{
		Timestamp:          now,
		MutationType:       RevokeDelegationMutationType,
		ActorPrincipalKind: "operator",
		ActorPrincipalID:   operator,
		TargetType:         "test_identity_delegation",
		TargetID:           view.ID,
		BeforeSummary: fmt.Sprintf("offline issuer delegation revoked: grantor_kind=%s hub_id=%s workload_sub=%s scope=%s host=%s operator=%s",
			store.DelegationGrantorTestInfraOffline, row.HubID, row.WorkloadSub, row.Scope, in.Host, operator),
	}); err != nil {
		return rollback(fmt.Errorf("write %s audit row: %w", RevokeDelegationMutationType, err))
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("commit delegation revoke: %w", err)
	}
	view.RevokedAt = &now
	view.RevokedBy = &revokedBy
	return view, true, nil
}
