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

//go:build !no_sqlite

package testinfra

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/mutationaudit"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/testidentitydelegation"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Offline member-tier delegations (ptone/scion#4240, phase DL).

const (
	dlHubID = "dl-hub-1"
	dlSub   = "100000000000000000091"
)

var dlIssuers = []config.TestIdentityWorkloadIssuer{
	{Sub: dlSub, MaxTier: config.WorkloadIssuerTierMember},
	{Sub: "100000000000000000092", MaxTier: config.WorkloadIssuerTierAdmin},
}

func dlMarked(t *testing.T) *ent.Client {
	t.Helper()
	client := newClient(t)
	_, err := Mark(context.Background(), client, MarkInput{HubID: dlHubID, Operator: "op", Host: "host-1", Now: time.Now()})
	require.NoError(t, err)
	return client
}

func dlInput() DelegateInput {
	return DelegateInput{HubID: dlHubID, WorkloadSub: dlSub, WorkloadIssuers: dlIssuers, Operator: "dl-op", Host: "dl-host"}
}

func dlCount(t *testing.T, client *ent.Client, mutation string) (rows, audits int) {
	t.Helper()
	ctx := context.Background()
	rows, err := client.TestIdentityDelegation.Query().Count(ctx)
	require.NoError(t, err)
	audits, err = client.MutationAudit.Query().Where(mutationaudit.MutationType(mutation)).Count(ctx)
	require.NoError(t, err)
	return rows, audits
}

func TestDelegate_WritesBoundMemberDelegationAndAudit(t *testing.T) {
	client := dlMarked(t)
	ctx := context.Background()
	marker, err := client.TestInfraMarker.Get(ctx, MarkerID)
	require.NoError(t, err)

	in := dlInput()
	in.TTL = 2 * time.Hour
	in.MemberCap = 3
	in.Label = "bootstrap"
	d, err := Delegate(ctx, client, in)
	require.NoError(t, err)

	assert.Equal(t, store.DelegationGrantorTestInfraOffline, d.GrantorKind)
	assert.Equal(t, dlHubID, d.HubID)
	assert.Equal(t, store.TestInfraOfflineGrantorID(dlHubID), d.GrantorID)
	assert.Equal(t, store.TestInfraOfflineDelegationScope, d.Scope)
	assert.Equal(t, 3, d.MemberCap)
	assert.Equal(t, dlSub, d.WorkloadSub)
	require.NotNil(t, d.MarkerCreatedAt)
	assert.True(t, d.MarkerCreatedAt.Equal(marker.CreatedAt), "bound to the current marker instance")
	assert.Equal(t, 2*time.Hour, d.ExpiresAt.Sub(d.Created))

	row, err := client.TestIdentityDelegation.Get(ctx, uuid.MustParse(d.ID))
	require.NoError(t, err)
	assert.Equal(t, testidentitydelegation.GrantorKindTestInfraOffline, row.GrantorKind)

	audits, err := client.MutationAudit.Query().Where(mutationaudit.MutationType(DelegateMutationType)).All(ctx)
	require.NoError(t, err)
	require.Len(t, audits, 1)
	a := audits[0]
	assert.Equal(t, "operator", a.ActorPrincipalKind)
	assert.Equal(t, "dl-op", a.ActorPrincipalID)
	assert.Equal(t, d.ID, a.TargetID)
	for _, want := range []string{"grantor_kind=test_infra_offline", "hub_id=" + dlHubID, "marker_id=" + MarkerID, "workload_sub=" + dlSub, "host=dl-host", "operator=dl-op"} {
		assert.Contains(t, a.AfterSummary, want)
	}
}

// The reserved grantor id is a version 5 UUID of the hub id: never a
// server-generated (version 4) user id, and distinct per hub.
func TestTestInfraOfflineGrantorID(t *testing.T) {
	id := store.TestInfraOfflineGrantorID(dlHubID)
	u, err := uuid.Parse(id)
	require.NoError(t, err)
	assert.Equal(t, uuid.Version(5), u.Version())
	assert.Equal(t, id, store.TestInfraOfflineGrantorID(dlHubID), "deterministic")
	assert.NotEqual(t, id, store.TestInfraOfflineGrantorID("dl-hub-2"))
	assert.Empty(t, store.TestInfraOfflineGrantorID(""))
}

func TestDelegate_Refusals(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		mutate func(*DelegateInput)
		want   error
	}{
		{"empty hub id", func(in *DelegateInput) { in.HubID = "" }, ErrDelegateHubIDRequired},
		{"padded hub id", func(in *DelegateInput) { in.HubID = " " + dlHubID }, ErrDelegateHubIDRequired},
		{"marker names another hub", func(in *DelegateInput) { in.HubID = "dl-hub-other" }, ErrDelegateMarkerHubMismatch},
		{"admin scope", func(in *DelegateInput) { in.Scope = "test_identity:issue_admin" }, ErrDelegateScope},
		{"super-admin scope", func(in *DelegateInput) { in.Scope = "test_identity:issue_super_admin" }, ErrDelegateScope},
		{"off the allowlist", func(in *DelegateInput) { in.WorkloadSub = "100000000000000000099" }, ErrDelegateNotAllowlisted},
		{"empty allowlist", func(in *DelegateInput) { in.WorkloadIssuers = nil }, ErrDelegateNotAllowlisted},
		{"unknown max tier", func(in *DelegateInput) {
			in.WorkloadIssuers = []config.TestIdentityWorkloadIssuer{{Sub: dlSub, MaxTier: "owner"}}
		}, ErrDelegateNotAllowlisted},
		{"ttl too long", func(in *DelegateInput) { in.TTL = store.TestInfraOfflineDelegationMaxTTL + time.Second }, ErrDelegateTTL},
		{"ttl too short", func(in *DelegateInput) { in.TTL = 30 * time.Second }, ErrDelegateTTL},
		{"cap too high", func(in *DelegateInput) { in.MemberCap = store.TestInfraOfflineDelegationMaxMemberCap + 1 }, ErrDelegateMemberCap},
		{"negative cap", func(in *DelegateInput) { in.MemberCap = -1 }, ErrDelegateMemberCap},
		{"label too long", func(in *DelegateInput) { in.Label = strings.Repeat("l", delegationLabelMaxRunes+1) }, ErrDelegateLabel},
		{"label control char", func(in *DelegateInput) { in.Label = "a\x07b" }, ErrDelegateLabel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := dlMarked(t)
			in := dlInput()
			tc.mutate(&in)
			_, err := Delegate(ctx, client, in)
			require.ErrorIs(t, err, tc.want)
			rows, audits := dlCount(t, client, DelegateMutationType)
			assert.Zero(t, rows, "a refused delegate writes no delegation")
			assert.Zero(t, audits, "a refused delegate writes no audit row")
		})
	}

	t.Run("no marker", func(t *testing.T) {
		client := newClient(t)
		_, err := Delegate(ctx, client, dlInput())
		require.ErrorIs(t, err, ErrMarkerAbsent)
		rows, audits := dlCount(t, client, DelegateMutationType)
		assert.Zero(t, rows)
		assert.Zero(t, audits)
	})

	t.Run("an admin max_tier entry still yields only the member scope", func(t *testing.T) {
		client := dlMarked(t)
		in := dlInput()
		in.WorkloadSub = "100000000000000000092"
		d, err := Delegate(ctx, client, in)
		require.NoError(t, err)
		assert.Equal(t, store.TestInfraOfflineDelegationScope, d.Scope)
	})
}

func TestDelegate_LiveCapPerHub(t *testing.T) {
	client := dlMarked(t)
	ctx := context.Background()
	var ids []string
	for i := 0; i < store.TestInfraOfflineMaxLiveDelegations; i++ {
		d, err := Delegate(ctx, client, dlInput())
		require.NoError(t, err)
		ids = append(ids, d.ID)
	}
	_, err := Delegate(ctx, client, dlInput())
	require.ErrorIs(t, err, ErrDelegateTooManyLive)
	rows, _ := dlCount(t, client, DelegateMutationType)
	assert.Equal(t, store.TestInfraOfflineMaxLiveDelegations, rows)

	// A revoked delegation frees a slot.
	_, changed, err := RevokeDelegation(ctx, client, RevokeDelegationInput{HubID: dlHubID, ID: ids[0], Operator: "op"})
	require.NoError(t, err)
	require.True(t, changed)
	_, err = Delegate(ctx, client, dlInput())
	require.NoError(t, err)
}

func TestRevokeDelegation(t *testing.T) {
	client := dlMarked(t)
	ctx := context.Background()
	d, err := Delegate(ctx, client, dlInput())
	require.NoError(t, err)

	_, _, err = RevokeDelegation(ctx, client, RevokeDelegationInput{HubID: "dl-hub-other", ID: d.ID, Operator: "op"})
	require.ErrorIs(t, err, ErrDelegationOtherHub)
	_, _, err = RevokeDelegation(ctx, client, RevokeDelegationInput{HubID: dlHubID, ID: uuid.NewString(), Operator: "op"})
	require.ErrorIs(t, err, ErrDelegationNotFound)
	_, _, err = RevokeDelegation(ctx, client, RevokeDelegationInput{HubID: dlHubID, ID: "not-a-uuid", Operator: "op"})
	require.ErrorIs(t, err, ErrDelegationNotFound)

	got, changed, err := RevokeDelegation(ctx, client, RevokeDelegationInput{HubID: dlHubID, ID: d.ID, Operator: "op", Host: "h"})
	require.NoError(t, err)
	assert.True(t, changed)
	require.NotNil(t, got.RevokedAt)
	require.NotNil(t, got.RevokedBy)
	assert.Equal(t, "operator:op", *got.RevokedBy)

	again, changed, err := RevokeDelegation(ctx, client, RevokeDelegationInput{HubID: dlHubID, ID: d.ID, Operator: "op2"})
	require.NoError(t, err)
	assert.False(t, changed, "revoking a revoked delegation changes nothing")
	require.NotNil(t, again.RevokedBy)
	assert.Equal(t, "operator:op", *again.RevokedBy, "the first revocation is kept")

	_, audits := dlCount(t, client, RevokeDelegationMutationType)
	assert.Equal(t, 1, audits, "one revoke audit row")

	// Revocation needs no marker (it only removes authority).
	d2, err := Delegate(ctx, client, dlInput())
	require.NoError(t, err)
	_, err = Unmark(ctx, client, UnmarkInput{Operator: "op"})
	require.NoError(t, err)
	_, changed, err = RevokeDelegation(ctx, client, RevokeDelegationInput{HubID: dlHubID, ID: d2.ID, Operator: "op"})
	require.NoError(t, err)
	assert.True(t, changed)
}

// A human-grantor delegation is revoked through the hub API, never offline.
func TestRevokeDelegation_RefusesHumanGrantor(t *testing.T) {
	client := dlMarked(t)
	ctx := context.Background()
	human, err := client.TestIdentityDelegation.Create().
		SetID(uuid.New()).
		SetGrantorID(uuid.NewString()).
		SetWorkloadSub(dlSub).
		SetScope(store.TestInfraOfflineDelegationScope).
		SetMemberCap(1).
		SetCreatedAt(time.Now()).
		SetExpiresAt(time.Now().Add(time.Hour)).
		Save(ctx)
	require.NoError(t, err)
	assert.Equal(t, testidentitydelegation.GrantorKindHuman, human.GrantorKind, "the column default is human")
	_, _, err = RevokeDelegation(ctx, client, RevokeDelegationInput{HubID: dlHubID, ID: human.ID.String(), Operator: "op"})
	require.ErrorIs(t, err, ErrDelegationNotOffline)
	row, err := client.TestIdentityDelegation.Get(ctx, human.ID)
	require.NoError(t, err)
	assert.Nil(t, row.RevokedAt)
}
