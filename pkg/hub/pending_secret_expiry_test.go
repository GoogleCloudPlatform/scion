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

package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestPendingSecretExpiry_Sweep: the sweep deletes expired pending
// secrets, releases their held values after the delete, logs one
// secret_pending_expire event per row, keeps live rows and is idempotent.
func TestPendingSecretExpiry_Sweep(t *testing.T) {
	f := newPendingFixture(t, "pend-expiry")
	audit := recordPendingAudit(f.Server)
	ctx := context.Background()

	old := decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "EXP_OLD", userScopeWrite("test-value-old", false)))
	oldRef := f.pendingRow(t, old.PendingID).ValueRef
	sweepAt := old.ExpiresAt.Add(time.Second)

	// A row written later is still live at sweepAt.
	live, err := f.Store.CreateOrReplacePendingSecret(ctx, &store.PendingSecret{
		Key: "EXP_LIVE", SecretType: store.SecretTypeEnvironment, Scope: store.ScopeUser, ScopeID: f.UserID,
		ValueRef: mustHold(t, f.Server, "test-value-live"), ValueLen: 15,
		AgentID: f.AgentB, AgentSlug: "b", ProjectID: f.ProjectB, ProjectName: "b",
	}, sweepAt.Add(-time.Hour))
	require.NoError(t, err)

	n, err := f.Server.expirePendingSecrets(ctx, sweepAt)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	_, err = f.Store.GetPendingSecret(ctx, old.PendingID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	f.pendingRow(t, live.Secret.ID)
	assert.Contains(t, f.drops.dropped, oldRef, "the held value is released")

	expires := audit.ofType(PendingSecretEventExpire)
	require.Len(t, expires, 1)
	assert.Equal(t, old.PendingID, expires[0].PendingID)
	assert.Equal(t, "EXP_OLD", expires[0].Key)
	assert.Equal(t, PendingSecretActorSystem, expires[0].ActorKind)
	assert.Empty(t, expires[0].ActorID)
	assert.Equal(t, f.AgentID, expires[0].AgentID)
	assert.Equal(t, f.UserID, expires[0].ScopeID)

	// Idempotent.
	n, err = f.Server.expirePendingSecrets(ctx, sweepAt)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Len(t, audit.ofType(PendingSecretEventExpire), 1)
}

// TestPendingSecretExpiry_ManyBatches: more expired rows than one batch
// are all removed in one sweep.
func TestPendingSecretExpiry_ManyBatches(t *testing.T) {
	f := newPendingFixture(t, "pend-expiry-batch")
	ctx := context.Background()
	past := time.Now().Add(-store.PendingSecretTTL - time.Hour)
	// Spread the rows over owners and agents so every write stays within
	// the per-owner and per-agent caps.
	total := pendingSecretExpiryBatch + 5
	owners := map[string]bool{}
	for i := 0; i < total; i++ {
		owner := tid(fmt.Sprintf("pend-expiry-batch-owner-%d", i/store.PendingSecretOwnerCap))
		owners[owner] = true
		_, err := f.Store.CreateOrReplacePendingSecret(ctx, &store.PendingSecret{
			Key: fmt.Sprintf("BATCH_%d", i), SecretType: store.SecretTypeEnvironment,
			Scope: store.ScopeUser, ScopeID: owner, ValueRef: "test-ref", ValueLen: 1,
			AgentID: tid(fmt.Sprintf("pend-expiry-batch-writer-%d", i/store.PendingSecretAgentCap)), AgentSlug: "w",
			ProjectID: f.ProjectID, ProjectName: f.ProjectName,
		}, past)
		require.NoError(t, err)
	}
	n, err := f.Server.expirePendingSecrets(ctx, time.Now())
	require.NoError(t, err)
	assert.Equal(t, total, n)
	for owner := range owners {
		rows, err := f.Store.ListPendingSecretsByOwner(ctx, store.ScopeUser, owner)
		require.NoError(t, err)
		assert.Empty(t, rows)
	}
}

// TestPendingSecretExpiry_Registered: the sweep is registered as the
// hourly "pending-secret-expiry" singleton.
func TestPendingSecretExpiry_Registered(t *testing.T) {
	f := newPendingFixture(t, "pend-expiry-reg")
	sched := NewScheduler(f.Store, nil)
	f.Server.registerPendingSecretExpiry(sched)
	var found bool
	for _, h := range sched.recurring {
		if h.Name == "pending-secret-expiry" {
			found = true
			assert.Equal(t, 60, h.Interval)
			assert.True(t, h.Singleton)
		}
	}
	assert.True(t, found)
}

// TestSecretsList_IncludePending: the merged user list returns approved
// and live pending secrets with a state per item, approved first for a
// key with both, pending metadata and provenance, the project secrets the
// key would override in projects the caller can read, and never a value.
func TestSecretsList_IncludePending(t *testing.T) {
	f := newPendingFixture(t, "pend-merged")
	ctx := context.Background()

	// Approved user secret APPROVED_ONLY and REPLACED (with a pending
	// replacement), a pending NEW_KEY that overrides a project secret in
	// a project the owner can read and one in a project it cannot.
	for _, key := range []string{"APPROVED_ONLY", "REPLACED"} {
		rec := ownerRequest(t, f.Server, f.owner(), CredentialKindInteractive, http.MethodPut, "/api/v1/secrets/"+key,
			SetSecretRequest{Value: "dGVzdC12YWx1ZS1hcHByb3ZlZA==", Scope: store.ScopeUser})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "REPLACED", userScopeWrite("test-value-p1", false)))
	newKey := decodePendingResponse(t, agentPut(t, f.Server, f.AgentB, f.TokenB, "NEW_KEY", userScopeWrite("test-value-p2", false)))

	rec := ownerRequest(t, f.Server, f.owner(), CredentialKindInteractive, http.MethodPut,
		"/api/v1/secrets/NEW_KEY?scope=project&scopeId="+f.ProjectID,
		SetSecretRequest{Value: "dGVzdC12YWx1ZS1wcm9q", Scope: store.ScopeProject, ScopeID: f.ProjectID})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	hidden := tid("pend-merged-hidden")
	require.NoError(t, f.Store.CreateProject(ctx, &store.Project{
		ID: hidden, Name: "hidden-project", Slug: "hidden-pend-merged", Created: time.Now(), Updated: time.Now(),
	}))
	_, _, err := f.Server.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name: "NEW_KEY", Value: "test-value-hidden", Scope: store.ScopeProject, ScopeID: hidden,
	})
	require.NoError(t, err)

	// An expired pending row is left out.
	_, err = f.Store.CreateOrReplacePendingSecret(ctx, &store.PendingSecret{
		Key: "EXPIRED_KEY", SecretType: store.SecretTypeEnvironment, Scope: store.ScopeUser, ScopeID: f.UserID,
		ValueRef: mustHold(t, f.Server, "test-value-x"), ValueLen: 12,
		AgentID: f.AgentID, AgentSlug: "a", ProjectID: f.ProjectID, ProjectName: f.ProjectName,
	}, time.Now().Add(-store.PendingSecretTTL-time.Minute))
	require.NoError(t, err)

	rec = ownerRequest(t, f.Server, f.owner(), CredentialKindInteractive, http.MethodGet,
		"/api/v1/secrets?scope=user&includePending=true", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "test-value")
	assert.NotContains(t, rec.Body.String(), "enc:v")
	var resp ListSecretsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	// The fixture stores user secrets of its own (SD_*); they are
	// approved items like any other and are left out of the checks.
	ours := func(key string) bool { return !strings.HasPrefix(key, "SD_") }
	approvedOurs := 0
	for _, sec := range resp.Secrets {
		if ours(sec.Key) {
			approvedOurs++
		}
	}
	assert.Equal(t, 2, approvedOurs, "secrets keeps the approved list")

	type row struct{ key, state string }
	var got []row
	var items []SecretListItem
	for _, it := range resp.Items {
		if !ours(it.Key) {
			continue
		}
		items = append(items, it)
		got = append(got, row{it.Key, it.State})
		switch it.State {
		case SecretStateApproved:
			require.NotNil(t, it.Secret)
			assert.Nil(t, it.Pending)
		case SecretStatePending:
			require.NotNil(t, it.Pending)
			assert.Nil(t, it.Secret)
			assert.NotNil(t, it.Pending.OverridesProjectSecrets, "never null")
		}
	}
	assert.Equal(t, []row{
		{"APPROVED_ONLY", "approved"},
		{"NEW_KEY", "pending"},
		{"REPLACED", "approved"},
		{"REPLACED", "pending"},
	}, got)

	pendingNew := items[1].Pending
	assert.Equal(t, newKey.PendingID, pendingNew.ID)
	assert.Equal(t, f.AgentB, pendingNew.AgentID)
	assert.Equal(t, f.ProjectB, pendingNew.ProjectID)
	assert.True(t, pendingNew.AllowProgeny)
	assert.False(t, pendingNew.ReplacesExisting)
	assert.False(t, pendingNew.ExpiresAt.IsZero())
	assert.Equal(t, []ProjectSecretOverride{{ProjectID: f.ProjectID, ProjectName: f.ProjectName}},
		pendingNew.OverridesProjectSecrets, "only projects the caller can read are named")

	pendingReplaced := items[3].Pending
	assert.True(t, pendingReplaced.ReplacesExisting)
	assert.Equal(t, items[2].Secret.ID, pendingReplaced.ReplacesSecretID)
	assert.Empty(t, pendingReplaced.OverridesProjectSecrets)

	// Without includePending the response is the approved list only.
	rec = ownerRequest(t, f.Server, f.owner(), CredentialKindInteractive, http.MethodGet, "/api/v1/secrets?scope=user", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var plain map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &plain))
	assert.NotContains(t, plain, "items")
}

// TestSecretsList_IncludePendingRefusals: the merged list is served only
// to the owner's session; agents, user access tokens and federated callers
// are refused, another user sees only its own (empty) list, and the flag is
// refused for any other scope.
func TestSecretsList_IncludePendingRefusals(t *testing.T) {
	f := newPendingFixture(t, "pend-merged-refuse")
	decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "MR_KEY", userScopeWrite("test-value-1", false)))
	path := "/api/v1/secrets?scope=user&includePending=true"

	rec := doRequestWithAgentToken(t, f.Server, http.MethodGet, path, nil, f.Token)
	assert.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "MR_KEY")

	rec = ownerRequest(t, f.Server, f.owner(), CredentialKindUAT, http.MethodGet, path, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)

	fed := NewFederatedUserIdentity("https://issuer.example", f.UserID, f.UserID+"@test.com", "Fed", "member", nil)
	rec = ownerRequest(t, f.Server, fed, CredentialKindFederation, http.MethodGet, path, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)

	for name, ident := range map[string]UserIdentity{
		"other user": sessionUser(tid("pend-merged-other")),
		"hub admin":  NewAuthenticatedUser(tid("pend-merged-admin"), "admin@test.com", "Admin", "admin", "web"),
	} {
		rec = ownerRequest(t, f.Server, ident, CredentialKindInteractive, http.MethodGet, path, nil)
		require.Equal(t, http.StatusOK, rec.Code, name)
		assert.NotContains(t, rec.Body.String(), "MR_KEY", name)
	}

	rec = ownerRequest(t, f.Server, f.owner(), CredentialKindInteractive, http.MethodGet,
		"/api/v1/secrets?scope=project&scopeId="+f.ProjectID+"&includePending=true", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	rec = ownerRequest(t, f.Server, f.owner(), CredentialKindInteractive, http.MethodGet,
		"/api/v1/secrets?scope=user&includePending=maybe", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// heldValuesBackend wraps a secret backend with a fixed list of held
// pending values (as a Secret Manager backend would list them) and records
// the refs passed to DropPending.
type heldValuesBackend struct {
	secret.SecretBackend
	held    []secret.HeldPendingValue
	dropped []string
}

func (b *heldValuesBackend) ListHeldPending(context.Context) ([]secret.HeldPendingValue, error) {
	return b.held, nil
}

func (b *heldValuesBackend) DropPending(_ context.Context, ref string) error {
	b.dropped = append(b.dropped, ref)
	return nil
}

// TestPendingSecretExpiry_ReleasesUnreferencedHeldValues: the sweep
// releases a held value no row references once it is past the grace age:
// a value held for a write whose row was never inserted, and a value whose
// row an approval claimed but never finished. A value its row references
// (however old) and an unreferenced value younger than the grace age are
// kept. Each release is audited as secret_pending_expire by the system.
func TestPendingSecretExpiry_ReleasesUnreferencedHeldValues(t *testing.T) {
	f := newPendingFixture(t, "pend-orphan")
	audit := recordPendingAudit(f.Server)
	ctx := context.Background()
	now := time.Now()

	// A live row whose value was stored long ago: referenced, kept.
	kept := decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "ORPHAN_KEPT", userScopeWrite("test-value-kept", false)))
	keptRef := f.pendingRow(t, kept.PendingID).ValueRef

	// Crash window 2: an approval claimed the row (deleting it) and never
	// wrote the approved secret, so the value is left with no row.
	claimedResp := decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "ORPHAN_CLAIMED", userScopeWrite("test-value-claimed", false)))
	claimed, err := f.Store.ClaimPendingSecretForApproval(ctx, claimedResp.PendingID, store.ScopeUser, f.UserID, now)
	require.NoError(t, err)
	_, err = f.Server.secretBackend.GetMeta(ctx, "ORPHAN_CLAIMED", store.ScopeUser, f.UserID)
	require.ErrorIs(t, err, store.ErrNotFound, "the approved secret was never written")

	old := now.Add(-pendingHeldValueGrace - time.Minute)
	young := now.Add(-pendingHeldValueGrace + time.Minute)
	// Crash window 1: a value held for a write whose row was never inserted.
	neverInserted := secret.HeldPendingValue{Ref: "gcpsm:test-never-inserted", PendingID: "pend-never-inserted", Created: old}
	youngOrphan := secret.HeldPendingValue{Ref: "gcpsm:test-young", PendingID: "pend-young", Created: young}
	backend := &heldValuesBackend{SecretBackend: f.Server.secretBackend, held: []secret.HeldPendingValue{
		{Ref: keptRef, PendingID: kept.PendingID, Created: old},
		{Ref: claimed.ValueRef, PendingID: claimed.ID, Created: old},
		neverInserted,
		youngOrphan,
	}}
	f.Server.SetSecretBackend(backend)

	n, err := f.Server.releaseUnreferencedPendingValues(ctx, now)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.ElementsMatch(t, []string{claimed.ValueRef, neverInserted.Ref}, backend.dropped)
	assert.NotContains(t, backend.dropped, keptRef, "a value its row references is kept")
	assert.NotContains(t, backend.dropped, youngOrphan.Ref, "a value younger than the grace age is kept")
	f.pendingRow(t, kept.PendingID)

	expires := audit.ofType(PendingSecretEventExpire)
	require.Len(t, expires, 2)
	var ids []string
	for _, e := range expires {
		ids = append(ids, e.PendingID)
		assert.Equal(t, PendingSecretActorSystem, e.ActorKind)
		assert.Equal(t, pendingSecretOrphanedValue, e.Reason)
	}
	assert.ElementsMatch(t, []string{claimed.ID, neverInserted.PendingID}, ids)

	// Once the young value is past the grace age it is released too.
	backend.held = []secret.HeldPendingValue{youngOrphan}
	backend.dropped = nil
	n, err = f.Server.releaseUnreferencedPendingValues(ctx, now.Add(2*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, []string{youngOrphan.Ref}, backend.dropped)
}

// TestPendingSecretExpiry_LocalBackendHoldsNothingElsewhere: with the
// local backend every value lives on its row, so nothing is released.
func TestPendingSecretExpiry_LocalBackendHoldsNothingElsewhere(t *testing.T) {
	f := newPendingFixture(t, "pend-orphan-local")
	decodePendingResponse(t, agentPut(t, f.Server, f.AgentID, f.Token, "LOCAL_KEY", userScopeWrite("test-value-1", false)))
	dropsBefore := len(f.drops.dropped)
	n, err := f.Server.releaseUnreferencedPendingValues(context.Background(), time.Now().Add(48*time.Hour))
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Len(t, f.drops.dropped, dropsBefore)
}
