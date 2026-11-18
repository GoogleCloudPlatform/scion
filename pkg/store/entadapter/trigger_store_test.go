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

package entadapter

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestTriggerStore_WebhookEndpointCRUDAndTransactionalDelete(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	projID := uuid.NewString()
	otherProjID := uuid.NewString()
	userID := uuid.NewString()
	hubID := uuid.NewString()

	ep1 := &store.WebhookEndpoint{
		ID:                  uuid.NewString(),
		ProjectID:           projID,
		Name:                "github-builds",
		Description:         "CI webhook",
		OwnerPrincipalKind:  store.DelegationPrincipalUser,
		OwnerPrincipalID:    userID,
		OwnerCredentialKind: store.InitiatorCredentialKindSession,
		CreatedBy:           userID,
	}
	require.NoError(t, cs.CreateWebhookEndpoint(ctx, ep1))
	assert.Equal(t, 1, ep1.SecretVersion)
	assert.Equal(t, store.WebhookSignatureModeScionV1, ep1.SignatureMode)
	assert.Equal(t, store.WebhookEndpointStatusActive, ep1.Status)

	// Duplicate name in same project fails with ErrAlreadyExists; same name in other project succeeds.
	dup := *ep1
	dup.ID = uuid.NewString()
	require.ErrorIs(t, cs.CreateWebhookEndpoint(ctx, &dup), store.ErrAlreadyExists)

	otherEP := *ep1
	otherEP.ID = uuid.NewString()
	otherEP.ProjectID = otherProjID
	require.NoError(t, cs.CreateWebhookEndpoint(ctx, &otherEP))

	gotByName, err := cs.GetWebhookEndpointByName(ctx, projID, "github-builds")
	require.NoError(t, err)
	assert.Equal(t, ep1.ID, gotByName.ID)

	count, err := cs.CountWebhookEndpoints(ctx, projID)
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	// Flush counters and last_received_at.
	recvAt := time.Date(2026, 11, 15, 22, 0, 0, 0, time.UTC)
	require.NoError(t, cs.FlushWebhookEndpointCounters(ctx, ep1.ID, 3, 2, &recvAt))
	gotAfterFlush, err := cs.GetWebhookEndpoint(ctx, ep1.ID)
	require.NoError(t, err)
	assert.Equal(t, 3, gotAfterFlush.ReceiveCount)
	assert.Equal(t, 2, gotAfterFlush.RejectCount)
	require.NotNil(t, gotAfterFlush.LastReceivedAt)
	assert.True(t, gotAfterFlush.LastReceivedAt.Equal(recvAt))

	// Store internal secrets for ep1 and otherEP.
	secKey1 := store.WebhookEndpointSecretKey(projID, ep1.ID, 1)
	otherSecKey := store.WebhookEndpointSecretKey(otherProjID, otherEP.ID, 1)
	require.NoError(t, cs.CreateSecret(ctx, &store.Secret{
		ID:             uuid.NewString(),
		Key:            secKey1,
		EncryptedValue: "sealed-v1",
		SecretType:     store.SecretTypeInternal,
		Scope:          store.ScopeHub,
		ScopeID:        hubID,
	}))
	require.NoError(t, cs.CreateSecret(ctx, &store.Secret{
		ID:             uuid.NewString(),
		Key:            otherSecKey,
		EncryptedValue: "sealed-other",
		SecretType:     store.SecretTypeInternal,
		Scope:          store.ScopeHub,
		ScopeID:        hubID,
	}))

	// Attach a trigger to ep1 and another trigger to a different endpoint.
	ep2 := &store.WebhookEndpoint{
		ID:                  uuid.NewString(),
		ProjectID:           projID,
		Name:                "second-ep",
		OwnerPrincipalKind:  store.DelegationPrincipalUser,
		OwnerPrincipalID:    userID,
		OwnerCredentialKind: store.InitiatorCredentialKindSession,
	}
	require.NoError(t, cs.CreateWebhookEndpoint(ctx, ep2))

	trDep := &store.Trigger{
		ID:            uuid.NewString(),
		ProjectID:     projID,
		Name:          "dep-trigger",
		SourceKind:    store.TriggerSourceKindWebhook,
		SourceID:      ep1.ID,
		MatchTypes:    []string{"webhook.received"},
		ActionType:    store.TriggerActionMessage,
		ActionPayload: `{"agentId":"a1","message":"hi"}`,
	}
	require.NoError(t, cs.CreateTrigger(ctx, trDep))

	trUnrelated := &store.Trigger{
		ID:            uuid.NewString(),
		ProjectID:     projID,
		Name:          "other-trigger",
		SourceKind:    store.TriggerSourceKindWebhook,
		SourceID:      ep2.ID,
		MatchTypes:    []string{"webhook.received"},
		ActionType:    store.TriggerActionMessage,
		ActionPayload: `{"agentId":"a1","message":"hi"}`,
	}
	require.NoError(t, cs.CreateTrigger(ctx, trUnrelated))

	// Wrong project ID on DeleteWebhookEndpoint returns ErrNotFound without mutating anything.
	_, _, err = cs.DeleteWebhookEndpoint(ctx, otherProjID, ep1.ID)
	require.ErrorIs(t, err, store.ErrNotFound)

	// Deleting ep1 pauses trDep (source_deleted), removes secKey1, and leaves trUnrelated and otherSecKey untouched.
	pausedIDs, deletedKeys, err := cs.DeleteWebhookEndpoint(ctx, projID, ep1.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{trDep.ID}, pausedIDs)
	assert.Equal(t, []string{secKey1}, deletedKeys)

	_, err = cs.GetWebhookEndpoint(ctx, ep1.ID)
	require.ErrorIs(t, err, store.ErrNotFound)

	_, err = cs.GetSecret(ctx, secKey1, store.ScopeHub, hubID)
	require.ErrorIs(t, err, store.ErrNotFound)

	_, err = cs.GetSecret(ctx, otherSecKey, store.ScopeHub, hubID)
	require.NoError(t, err)

	gotDep, err := cs.GetTrigger(ctx, trDep.ID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerStatusPaused, gotDep.Status)
	assert.Equal(t, store.TriggerStatusReasonSourceDeleted, gotDep.StatusReason)

	gotUnrelated, err := cs.GetTrigger(ctx, trUnrelated.ID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerStatusActive, gotUnrelated.Status)
}

func TestTriggerStore_TriggerAttributionCASAndCounters(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	projID := uuid.NewString()
	agentID := uuid.NewString()
	stamp := store.FixtureProvenance{RootID: uuid.NewString(), IssuerID: uuid.NewString()}

	tr := &store.Trigger{
		ID:            uuid.NewString(),
		ProjectID:     projID,
		Name:          "on-push",
		Description:   "initial",
		SourceKind:    store.TriggerSourceKindWebhook,
		SourceID:      uuid.NewString(),
		MatchTypes:    []string{"webhook.push", "webhook.tag.*"},
		ActionType:    store.TriggerActionMessage,
		ActionPayload: `{"agentId":"target-1","message":"pushed"}`,
		CreatedBy:     agentID,
		InitiatorAttribution: store.InitiatorAttribution{
			InitiatorPrincipalKind:  store.DelegationPrincipalAgent,
			InitiatorPrincipalID:    agentID,
			InitiatorCredentialKind: store.InitiatorCredentialKindAgent,
			AttributionVersion:      1,
			AuthorizationRevision:   1,
		},
		AuthorityCeiling: store.EffectCeiling{
			Kind:              store.EffectCeilingBounded,
			Version:           permissions.CeilingVersionV1,
			PermissionIDs:     []string{"agent.message"},
			BoundaryKind:      string(permissions.BoundaryKindProject),
			BoundaryProjectID: projID,
		},
		Fixture: stamp,
	}
	require.NoError(t, cs.CreateTrigger(ctx, tr))

	got, err := cs.GetTrigger(ctx, tr.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"webhook.push", "webhook.tag.*"}, got.MatchTypes)
	assert.Equal(t, 1, got.AuthorizationRevision)
	assert.Equal(t, stamp, got.Fixture)
	assert.Equal(t, []string{"agent.message"}, got.AuthorityCeiling.PermissionIDs)

	// Agent-scoped list filter matches recorded authority.
	listed, err := cs.ListTriggers(ctx, store.TriggerFilter{ProjectID: projID, InitiatorAgentID: agentID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, 1, listed.TotalCount)

	// Stale revision fails with ErrRevisionConflict.
	tr.Description = "updated"
	err = cs.UpdateTrigger(ctx, tr, store.TriggerFieldMask{Description: true}, 99, true, nil)
	require.ErrorIs(t, err, store.ErrRevisionConflict)

	// Matching revision succeeds; re-attribution updates revision atomically.
	userID := uuid.NewString()
	newAttr := store.InitiatorAttribution{
		InitiatorPrincipalKind:  store.DelegationPrincipalUser,
		InitiatorPrincipalID:    userID,
		InitiatorCredentialKind: store.InitiatorCredentialKindSession,
		AttributionVersion:      1,
		AuthorizationRevision:   2,
	}
	tr.AuthorityCeiling = store.EffectCeiling{Kind: store.EffectCeilingPrincipal}
	tr.MatchTypes = []string{"webhook.received"}
	require.NoError(t, cs.UpdateTrigger(ctx, tr, store.TriggerFieldMask{Description: true, MatchTypes: true}, 1, true, &newAttr))

	got2, err := cs.GetTrigger(ctx, tr.ID)
	require.NoError(t, err)
	assert.Equal(t, "updated", got2.Description)
	assert.Equal(t, []string{"webhook.received"}, got2.MatchTypes)
	assert.Equal(t, 2, got2.AuthorizationRevision)
	assert.Equal(t, userID, got2.InitiatorPrincipalID)

	// RecordTriggerFire increments error_count on failed and refused, not on fired or skipped.
	t0 := time.Date(2026, 11, 15, 22, 10, 0, 0, time.UTC)
	require.NoError(t, cs.RecordTriggerFire(ctx, tr.ID, t0, store.TriggerFireStatusFired, ""))
	require.NoError(t, cs.RecordTriggerFire(ctx, tr.ID, t0.Add(time.Second), store.TriggerFireStatusSkipped, "target not running"))
	require.NoError(t, cs.RecordTriggerFire(ctx, tr.ID, t0.Add(2*time.Second), store.TriggerFireStatusRefused, "message refused"))
	require.NoError(t, cs.RecordTriggerFire(ctx, tr.ID, t0.Add(3*time.Second), store.TriggerFireStatusFailed, "dispatch failed"))

	got3, err := cs.GetTrigger(ctx, tr.ID)
	require.NoError(t, err)
	assert.Equal(t, 4, got3.FireCount)
	assert.Equal(t, 2, got3.ErrorCount)
	assert.Equal(t, store.TriggerFireStatusFailed, got3.LastFireStatus)
	assert.Equal(t, "dispatch failed", got3.LastFireError)
}

func TestTriggerStore_EventsFiresVisibilityAndReaper(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	projID := uuid.NewString()
	agentA := uuid.NewString()
	agentB := uuid.NewString()
	userID := uuid.NewString()

	// Endpoint owned by user; trigger owned by agentA.
	userEP := &store.WebhookEndpoint{
		ID:                  uuid.NewString(),
		ProjectID:           projID,
		Name:                "user-ep",
		OwnerPrincipalKind:  store.DelegationPrincipalUser,
		OwnerPrincipalID:    userID,
		OwnerCredentialKind: store.InitiatorCredentialKindSession,
	}
	require.NoError(t, cs.CreateWebhookEndpoint(ctx, userEP))

	agentAEP := &store.WebhookEndpoint{
		ID:                  uuid.NewString(),
		ProjectID:           projID,
		Name:                "agent-a-ep",
		OwnerPrincipalKind:  store.DelegationPrincipalAgent,
		OwnerPrincipalID:    agentA,
		OwnerCredentialKind: store.InitiatorCredentialKindAgent,
	}
	require.NoError(t, cs.CreateWebhookEndpoint(ctx, agentAEP))

	trAgentA := &store.Trigger{
		ID:            uuid.NewString(),
		ProjectID:     projID,
		Name:          "agent-a-tr",
		SourceKind:    store.TriggerSourceKindWebhook,
		SourceID:      userEP.ID,
		MatchTypes:    []string{"webhook.received"},
		ActionType:    store.TriggerActionMessage,
		ActionPayload: `{"agentId":"x","message":"m"}`,
		InitiatorAttribution: store.InitiatorAttribution{
			InitiatorPrincipalKind:  store.DelegationPrincipalAgent,
			InitiatorPrincipalID:    agentA,
			InitiatorCredentialKind: store.InitiatorCredentialKindAgent,
			AttributionVersion:      1,
			AuthorizationRevision:   1,
		},
	}
	require.NoError(t, cs.CreateTrigger(ctx, trAgentA))

	now := time.Date(2026, 11, 15, 22, 30, 0, 0, time.UTC)
	evtUserFired := &store.TriggerEvent{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   userEP.ID,
		Type:       "webhook.received",
		Subject:    "webhook:" + userEP.ID,
		DedupKey:   "deliv-1",
		ReceivedAt: now,
		Payload:    `{"n":1}`,
	}
	require.NoError(t, cs.CreateTriggerEvent(ctx, evtUserFired))

	// Duplicate dedup key on same source is rejected.
	dupEvt := *evtUserFired
	dupEvt.ID = uuid.NewString()
	require.ErrorIs(t, cs.CreateTriggerEvent(ctx, &dupEvt), store.ErrAlreadyExists)

	evtUserUnfired := &store.TriggerEvent{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   userEP.ID,
		Type:       "webhook.received",
		Subject:    "webhook:" + userEP.ID,
		DedupKey:   "deliv-2",
		ReceivedAt: now.Add(time.Second),
		Payload:    `{"n":2}`,
	}
	require.NoError(t, cs.CreateTriggerEvent(ctx, evtUserUnfired))

	evtAgentAEP := &store.TriggerEvent{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   agentAEP.ID,
		Type:       "webhook.received",
		Subject:    "webhook:" + agentAEP.ID,
		DedupKey:   "deliv-3",
		ReceivedAt: now.Add(2 * time.Second),
		Payload:    `{"n":3}`,
	}
	require.NoError(t, cs.CreateTriggerEvent(ctx, evtAgentAEP))

	// Claim fire for (evtUserFired, trAgentA); second claim returns claimed=false + existing.
	fire := &store.TriggerFire{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		TriggerID:  trAgentA.ID,
		EventID:    evtUserFired.ID,
		Subject:    evtUserFired.Subject,
		ActionType: store.TriggerActionMessage,
		LeaseUntil: now.Add(10 * time.Minute),
		StartedAt:  now,
	}
	claimed, existing, err := cs.ClaimTriggerFire(ctx, fire)
	require.NoError(t, err)
	require.True(t, claimed)
	assert.Nil(t, existing)

	fireDup := *fire
	fireDup.ID = uuid.NewString()
	claimed2, existing2, err := cs.ClaimTriggerFire(ctx, &fireDup)
	require.NoError(t, err)
	assert.False(t, claimed2)
	require.NotNil(t, existing2)
	assert.Equal(t, fire.ID, existing2.ID)

	live, err := cs.HasLiveTriggerFireForSubject(ctx, trAgentA.ID, evtUserFired.Subject, now.Add(time.Minute))
	require.NoError(t, err)
	assert.True(t, live)

	// Agent A sees evtAgentAEP (owned endpoint) and evtUserFired (fired by trAgentA), NOT evtUserUnfired.
	visA, err := cs.ListTriggerEvents(ctx, store.TriggerEventFilter{ProjectID: projID, VisibleToAgentID: agentA}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, 2, visA.TotalCount)

	visB, err := cs.ListTriggerEvents(ctx, store.TriggerEventFilter{ProjectID: projID, VisibleToAgentID: agentB}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, 0, visB.TotalCount)

	hasFiredA, err := cs.AgentHasFiredTriggerForEvent(ctx, evtUserFired.ID, agentA)
	require.NoError(t, err)
	assert.True(t, hasFiredA)
	hasFiredB, err := cs.AgentHasFiredTriggerForEvent(ctx, evtUserFired.ID, agentB)
	require.NoError(t, err)
	assert.False(t, hasFiredB)

	// Reap expired fire after lease_until passes; subsequent FinishTriggerFire CAS returns false.
	reaped, err := cs.ReapExpiredTriggerFires(ctx, now.Add(11*time.Minute), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, reaped)

	finished, err := cs.FinishTriggerFire(ctx, fire.ID, store.TriggerFireStatusFired, "", "", now.Add(12*time.Minute))
	require.NoError(t, err)
	assert.False(t, finished)
}

func TestLockTriggerQuota_RequiresWithTx(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	projID := uuid.NewString()

	require.ErrorIs(t, cs.LockTriggerQuota(ctx, projID), store.ErrInvalidInput)

	require.NoError(t, cs.WithTx(ctx, func(tx store.Store) error {
		require.ErrorIs(t, tx.LockTriggerQuota(ctx, "not-a-uuid"), store.ErrInvalidInput)
		return tx.LockTriggerQuota(ctx, projID)
	}))
}

// TestLockTriggerQuota_ConcurrentLastSlot verifies that concurrent transactions
// racing for the last webhook-endpoint or trigger quota slot insert/activate at
// most one row and refuse the rest. On PostgreSQL (run in CI via
// test-store-postgres-entadapter with -tags integration and
// SCION_TEST_POSTGRES_URL) pg_advisory_xact_lock(LockTriggerQuota,
// StableProjectHash(projectID)) is what prevents write skew under READ
// COMMITTED; SQLite serializes writers natively.
func TestLockTriggerQuota_ConcurrentLastSlot(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	userID := uuid.NewString()
	errFull := errors.New("quota full")

	t.Run("webhook_endpoints", func(t *testing.T) {
		projID := uuid.NewString()
		const limit = 4
		for i := 0; i < limit-1; i++ {
			require.NoError(t, cs.CreateWebhookEndpoint(ctx, &store.WebhookEndpoint{
				ID:                  uuid.NewString(),
				ProjectID:           projID,
				Name:                fmt.Sprintf("seed-ep-%d", i),
				OwnerPrincipalKind:  store.DelegationPrincipalUser,
				OwnerPrincipalID:    userID,
				OwnerCredentialKind: store.InitiatorCredentialKindSession,
			}))
		}

		start := make(chan struct{})
		var wg sync.WaitGroup
		var inserted, refused atomic.Int32
		for i := 0; i < 8; i++ {
			i := i
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for {
					ep := &store.WebhookEndpoint{
						ID:                  uuid.NewString(),
						ProjectID:           projID,
						Name:                fmt.Sprintf("race-ep-%d", i),
						OwnerPrincipalKind:  store.DelegationPrincipalUser,
						OwnerPrincipalID:    userID,
						OwnerCredentialKind: store.InitiatorCredentialKindSession,
					}
					err := cs.WithTx(ctx, func(tx store.Store) error {
						if lErr := tx.LockTriggerQuota(ctx, projID); lErr != nil {
							return lErr
						}
						cnt, cErr := tx.CountWebhookEndpoints(ctx, projID)
						if cErr != nil {
							return cErr
						}
						if cnt >= limit {
							return errFull
						}
						return tx.CreateWebhookEndpoint(ctx, ep)
					})
					switch {
					case err == nil:
						inserted.Add(1)
						return
					case errors.Is(err, errFull):
						refused.Add(1)
						return
					case errors.Is(err, store.ErrTransient):
						time.Sleep(time.Millisecond)
					default:
						assert.NoError(t, err)
						return
					}
				}
			}()
		}
		close(start)
		wg.Wait()
		assert.Equal(t, int32(1), inserted.Load())
		assert.Equal(t, int32(7), refused.Load())
		cnt, err := cs.CountWebhookEndpoints(ctx, projID)
		require.NoError(t, err)
		assert.Equal(t, limit, cnt)
	})

	t.Run("active_triggers_per_source", func(t *testing.T) {
		projID := uuid.NewString()
		sourceID := uuid.NewString()
		const limit = 4
		for i := 0; i < limit-1; i++ {
			require.NoError(t, cs.CreateTrigger(ctx, &store.Trigger{
				ID:            uuid.NewString(),
				ProjectID:     projID,
				Name:          fmt.Sprintf("seed-tr-%d", i),
				SourceKind:    store.TriggerSourceKindWebhook,
				SourceID:      sourceID,
				MatchTypes:    []string{"webhook.received"},
				ActionType:    store.TriggerActionMessage,
				ActionPayload: `{"agentId":"a1","message":"hi"}`,
				Status:        store.TriggerStatusActive,
			}))
		}

		start := make(chan struct{})
		var wg sync.WaitGroup
		var inserted, refused atomic.Int32
		for i := 0; i < 8; i++ {
			i := i
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for {
					tr := &store.Trigger{
						ID:            uuid.NewString(),
						ProjectID:     projID,
						Name:          fmt.Sprintf("race-tr-%d", i),
						SourceKind:    store.TriggerSourceKindWebhook,
						SourceID:      sourceID,
						MatchTypes:    []string{"webhook.received"},
						ActionType:    store.TriggerActionMessage,
						ActionPayload: `{"agentId":"a1","message":"hi"}`,
						Status:        store.TriggerStatusActive,
					}
					err := cs.WithTx(ctx, func(tx store.Store) error {
						if lErr := tx.LockTriggerQuota(ctx, projID); lErr != nil {
							return lErr
						}
						cnt, cErr := tx.CountTriggers(ctx, store.TriggerFilter{
							ProjectID:  projID,
							SourceKind: store.TriggerSourceKindWebhook,
							SourceID:   sourceID,
							Status:     store.TriggerStatusActive,
						})
						if cErr != nil {
							return cErr
						}
						if cnt >= limit {
							return errFull
						}
						return tx.CreateTrigger(ctx, tr)
					})
					switch {
					case err == nil:
						inserted.Add(1)
						return
					case errors.Is(err, errFull):
						refused.Add(1)
						return
					case errors.Is(err, store.ErrTransient):
						time.Sleep(time.Millisecond)
					default:
						assert.NoError(t, err)
						return
					}
				}
			}()
		}
		close(start)
		wg.Wait()
		assert.Equal(t, int32(1), inserted.Load())
		assert.Equal(t, int32(7), refused.Load())
		cnt, err := cs.CountTriggers(ctx, store.TriggerFilter{
			ProjectID:  projID,
			SourceKind: store.TriggerSourceKindWebhook,
			SourceID:   sourceID,
			Status:     store.TriggerStatusActive,
		})
		require.NoError(t, err)
		assert.Equal(t, limit, cnt)
	})
}
