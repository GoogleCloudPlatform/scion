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
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
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

func TestTriggerEvents_SinceTerminalCASAndOrphanReconcile(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	projID := uuid.NewString()
	epID := uuid.NewString()
	base := time.Now().UTC().Truncate(time.Second).Add(-96 * time.Hour)

	mkEvt := func(name string, receivedAt time.Time) *store.TriggerEvent {
		ev := &store.TriggerEvent{
			ID:         uuid.NewString(),
			ProjectID:  projID,
			SourceKind: store.TriggerSourceKindWebhook,
			SourceID:   epID,
			Type:       "webhook.received",
			Subject:    "webhook:" + epID,
			DedupKey:   name,
			ReceivedAt: receivedAt,
			Payload:    `{"ok":true}`,
		}
		require.NoError(t, cs.CreateTriggerEvent(ctx, ev))
		return ev
	}

	evOrphanOld := mkEvt("orphan-old", base)
	evQueuedOld := mkEvt("queued-old", base.Add(time.Hour))
	evOrphanRecent := mkEvt("orphan-recent", base.Add(80*time.Hour))
	evProcessed := mkEvt("processed-evt", base.Add(82*time.Hour))

	// Queue only evQueuedOld in durable_queue_items.
	require.NoError(t, cs.WithTx(ctx, func(tx store.Store) error {
		_, qErr := cs.EnqueueDurableItem(ctx, tx, store.DurableQueueRow{
			Kind:      "trigger.event",
			DedupKey:  evQueuedOld.ID,
			ProjectID: projID,
			Payload:   []byte(`{"v":1,"event_id":"` + evQueuedOld.ID + `"}`),
			CreatedAt: base.Add(time.Hour),
		})
		return qErr
	}))

	// Since filter returns only events received at or after cutoff.
	sinceCutoff := base.Add(80 * time.Hour)
	sinceRes, err := cs.ListTriggerEvents(ctx, store.TriggerEventFilter{
		ProjectID: projID,
		Since:     &sinceCutoff,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, 2, sinceRes.TotalCount)

	// Mark evProcessed processed; subsequent MarkTriggerEventFailed must not clobber terminal status.
	require.NoError(t, cs.MarkTriggerEventProcessed(ctx, evProcessed.ID, base.Add(83*time.Hour), 2, 1))
	require.NoError(t, cs.MarkTriggerEventFailed(ctx, evProcessed.ID, base.Add(84*time.Hour), "dropped by queue limits"))
	gotProc, err := cs.GetTriggerEvent(ctx, evProcessed.ID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerEventStatusProcessed, gotProc.Status)
	assert.Equal(t, 2, gotProc.MatchCount)
	assert.Equal(t, 1, gotProc.FireCount)
	assert.Empty(t, gotProc.Error)

	// Missing event returns ErrNotFound.
	require.ErrorIs(t, cs.MarkTriggerEventFailed(ctx, uuid.NewString(), base.Add(84*time.Hour), "x"), store.ErrNotFound)

	// Reconcile orphaned pending events older than 72h: only evOrphanOld transitions to failed.
	now := base.Add(96 * time.Hour)
	reconciled, err := cs.ReconcileOrphanedPendingTriggerEvents(
		ctx, "trigger.event", now.Add(-72*time.Hour), now, "dropped by queue limits", 100,
	)
	require.NoError(t, err)
	assert.Equal(t, 1, reconciled)

	gotOrphanOld, err := cs.GetTriggerEvent(ctx, evOrphanOld.ID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerEventStatusFailed, gotOrphanOld.Status)
	assert.Equal(t, "dropped by queue limits", gotOrphanOld.Error)

	gotQueuedOld, err := cs.GetTriggerEvent(ctx, evQueuedOld.ID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerEventStatusPending, gotQueuedOld.Status)

	gotOrphanRecent, err := cs.GetTriggerEvent(ctx, evOrphanRecent.ID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerEventStatusPending, gotOrphanRecent.Status)
}

func TestListTriggerEvents_AgentVisibility_ManyFiresSubquery(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	projID := uuid.NewString()
	pid := uuid.MustParse(projID)
	agentA := uuid.NewString()
	agentB := uuid.NewString()
	userID := uuid.NewString()
	base := time.Date(2026, 11, 17, 1, 0, 0, 0, time.UTC)

	userEP := &store.WebhookEndpoint{
		ID:                  uuid.NewString(),
		ProjectID:           projID,
		Name:                "user-ep",
		OwnerPrincipalKind:  store.DelegationPrincipalUser,
		OwnerPrincipalID:    userID,
		OwnerCredentialKind: store.InitiatorCredentialKindSession,
	}
	agentEP := &store.WebhookEndpoint{
		ID:                  uuid.NewString(),
		ProjectID:           projID,
		Name:                "agent-a-ep",
		OwnerPrincipalKind:  store.DelegationPrincipalAgent,
		OwnerPrincipalID:    agentA,
		OwnerCredentialKind: store.InitiatorCredentialKindAgent,
	}
	require.NoError(t, cs.CreateWebhookEndpoint(ctx, userEP))
	require.NoError(t, cs.CreateWebhookEndpoint(ctx, agentEP))

	mkAgentTrigger := func(name, principalID, credKind string, attrVer int) *store.Trigger {
		tr := &store.Trigger{
			ID:           uuid.NewString(),
			ProjectID:    projID,
			Name:         name,
			SourceKind:   store.TriggerSourceKindWebhook,
			SourceID:     userEP.ID,
			MatchTypes:   []string{"webhook.received"},
			MatchDialect: store.TriggerDialectCEL,
			ActionType:   store.TriggerActionMessage,
			SubjectMode:  store.TriggerSubjectModeNew,
			Status:       store.TriggerStatusActive,
			InitiatorAttribution: store.InitiatorAttribution{
				InitiatorPrincipalKind:  store.DelegationPrincipalAgent,
				InitiatorPrincipalID:    principalID,
				InitiatorCredentialKind: credKind,
				AttributionVersion:      attrVer,
				AuthorizationRevision:   1,
			},
		}
		require.NoError(t, cs.CreateTrigger(ctx, tr))
		return tr
	}

	trA1 := mkAgentTrigger("tr-a1", agentA, store.InitiatorCredentialKindAgent, 1)
	trA2 := mkAgentTrigger("tr-a2", agentA, store.InitiatorCredentialKindAgent, 1)
	trOther := mkAgentTrigger("tr-b", agentB, store.InitiatorCredentialKindAgent, 1)
	trLegacy := mkAgentTrigger("tr-a-legacy", agentA, store.InitiatorCredentialKindLegacyUnknown, 1)

	mkEvent := func(id, epID, dedup string, receivedAt time.Time) *store.TriggerEvent {
		ev := &store.TriggerEvent{
			ID:         id,
			ProjectID:  projID,
			SourceKind: store.TriggerSourceKindWebhook,
			SourceID:   epID,
			Type:       "webhook.received",
			Subject:    "webhook:" + epID,
			DedupKey:   dedup,
			OccurredAt: receivedAt,
			ReceivedAt: receivedAt,
			Payload:    `{"ok":true}`,
			Status:     store.TriggerEventStatusProcessed,
		}
		require.NoError(t, cs.CreateTriggerEvent(ctx, ev))
		return ev
	}

	ev1 := mkEvent(uuid.NewString(), userEP.ID, "d-ev1", base.Add(time.Second))
	// Tie-break pair sharing identical ReceivedAt (ordered DESC by ID).
	ev2Low := mkEvent("00000000-0000-4000-8000-000000000001", userEP.ID, "d-ev2-low", base.Add(2*time.Second))
	ev2High := mkEvent("ffffffff-ffff-4fff-bfff-ffffffffffff", agentEP.ID, "d-ev2-high", base.Add(2*time.Second))
	ev3 := mkEvent(uuid.NewString(), userEP.ID, "d-ev3", base.Add(3*time.Second))
	evHidden := mkEvent(uuid.NewString(), userEP.ID, "d-ev-hidden", base.Add(4*time.Second))

	claimFire := func(tr *store.Trigger, ev *store.TriggerEvent) {
		claimed, _, err := cs.ClaimTriggerFire(ctx, &store.TriggerFire{
			ID:                   uuid.NewString(),
			ProjectID:            projID,
			TriggerID:            tr.ID,
			EventID:              ev.ID,
			Subject:              ev.Subject,
			ActionType:           store.TriggerActionMessage,
			Status:               store.TriggerFireStatusFired,
			LeaseUntil:           base.Add(10 * time.Minute),
			StartedAt:            base,
			InitiatorAttribution: tr.InitiatorAttribution,
		})
		require.NoError(t, err)
		require.True(t, claimed)
	}

	// ev1 is fired by BOTH trA1 and trA2 (must still appear and count once).
	claimFire(trA1, ev1)
	claimFire(trA2, ev1)
	claimFire(trA1, ev2Low)
	claimFire(trA1, ev3)
	// evHidden is fired only by another agent's trigger and a legacy-attributed trigger.
	claimFire(trOther, evHidden)
	claimFire(trLegacy, evHidden)

	// Seed 66,000 additional fire rows for trA1 (exceeds both SQLite's ~32,766
	// variable limit and PostgreSQL's 65,535 bind-parameter limit).
	const bulkFires = 66_000
	const batchSize = 500
	trA1UUID := uuid.MustParse(trA1.ID)
	leaseUntil := base.Add(10 * time.Minute)
	tx, err := cs.client.Tx(ctx)
	require.NoError(t, err)
	for offset := 0; offset < bulkFires; offset += batchSize {
		builders := make([]*ent.TriggerFireCreate, 0, batchSize)
		for i := 0; i < batchSize; i++ {
			builders = append(builders, tx.TriggerFire.Create().
				SetID(uuid.New()).
				SetProjectID(pid).
				SetTriggerID(trA1UUID).
				SetEventID(uuid.New()).
				SetSubject("webhook:bulk").
				SetActionType(store.TriggerActionMessage).
				SetStatus(store.TriggerFireStatusFired).
				SetLeaseUntil(leaseUntil).
				SetStartedAt(base).
				SetCreated(base),
			)
		}
		require.NoError(t, tx.TriggerFire.CreateBulk(builders...).Exec(ctx))
	}
	require.NoError(t, tx.Commit())

	var (
		gotIDs []string
		cursor string
	)
	for {
		page, lErr := cs.ListTriggerEvents(ctx, store.TriggerEventFilter{
			ProjectID:        projID,
			VisibleToAgentID: agentA,
		}, store.ListOptions{
			Limit:  2,
			Cursor: cursor,
		})
		require.NoError(t, lErr)
		assert.Equal(t, 4, page.TotalCount)
		for _, item := range page.Items {
			gotIDs = append(gotIDs, item.ID)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	assert.Equal(t, []string{ev3.ID, ev2High.ID, ev2Low.ID, ev1.ID}, gotIDs)

	hasA, err := cs.AgentHasFiredTriggerForEvent(ctx, ev1.ID, agentA)
	require.NoError(t, err)
	assert.True(t, hasA)
	hasHidden, err := cs.AgentHasFiredTriggerForEvent(ctx, evHidden.ID, agentA)
	require.NoError(t, err)
	assert.False(t, hasHidden)
}

func TestTriggerEvent_ListsPersistenceAndCeiling(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	projID := uuid.NewString()
	now := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)

	evWithLists := &store.TriggerEvent{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindGitHub,
		SourceID:   "424242",
		Type:       "github.pull_request.opened",
		Subject:    "github:acme/widgets#42",
		DedupKey:   "72d3162e-cc78-11e3-81ab-4c9367dc0958/" + projID,
		OccurredAt: now,
		ReceivedAt: now,
		Attributes: map[string]string{"provider": "github", "repo": "acme/widgets"},
		Lists: map[string][]string{
			"labels":              {"bug", "p1"},
			"assignees":           {"octocat"},
			"requested_reviewers": {},
		},
		Payload:      `{"action":"opened"}`,
		PayloadBytes: len(`{"action":"opened"}`),
	}
	require.NoError(t, cs.CreateTriggerEvent(ctx, evWithLists))
	assert.Equal(t, []string{"bug", "p1"}, evWithLists.Lists["labels"])
	assert.Equal(t, []string{"octocat"}, evWithLists.Lists["assignees"])
	assert.Equal(t, []string{}, evWithLists.Lists["requested_reviewers"])

	byID, err := cs.GetTriggerEvent(ctx, evWithLists.ID)
	require.NoError(t, err)
	assert.Equal(t, evWithLists.Lists, byID.Lists)

	byDedup, err := cs.GetTriggerEventByDedupKey(ctx, store.TriggerSourceKindGitHub, "424242", evWithLists.DedupKey)
	require.NoError(t, err)
	assert.Equal(t, evWithLists.Lists, byDedup.Lists)

	listed, err := cs.ListTriggerEvents(ctx, store.TriggerEventFilter{
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindGitHub,
	}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, listed.Items, 1)
	assert.Equal(t, evWithLists.Lists, listed.Items[0].Lists)

	// Omitted Lists defaults to empty map.
	evEmpty := &store.TriggerEvent{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindGitHub,
		SourceID:   "424242",
		Type:       "github.issue_comment.created",
		Subject:    "github:acme/widgets#43",
		DedupKey:   "82d3162e-cc78-11e3-81ab-4c9367dc0958/" + projID,
		OccurredAt: now,
		ReceivedAt: now,
		Payload:    `{"action":"created"}`,
	}
	require.NoError(t, cs.CreateTriggerEvent(ctx, evEmpty))
	gotEmpty, err := cs.GetTriggerEvent(ctx, evEmpty.ID)
	require.NoError(t, err)
	assert.Empty(t, gotEmpty.Lists)

	// Regression (R2): 40 50-char '&'-heavy labels fit comfortably within 8 KiB
	// under SetEscapeHTML(false) (~2.1 KiB) even though standard json.Marshal
	// expands each '&' to '\u0026' (> 11 KiB). The store must measure the unescaped
	// JSON bytes matching githubtrigger.Normalize.
	ampLabels := make([]string, 40)
	for i := range ampLabels {
		prefix := fmt.Sprintf("L%02d", i)
		ampLabels[i] = prefix + strings.Repeat("&", 50-len(prefix))
	}
	ampLists := map[string][]string{"labels": ampLabels}
	escapedRaw, err := json.Marshal(ampLists)
	require.NoError(t, err)
	require.Greater(t, len(escapedRaw), store.MaxTriggerEventListsBytes)

	evAmp := &store.TriggerEvent{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindGitHub,
		SourceID:   "424242",
		Type:       "github.issue_comment.created",
		Subject:    "github:acme/widgets#45",
		DedupKey:   "a2d3162e-cc78-11e3-81ab-4c9367dc0958/" + projID,
		OccurredAt: now,
		ReceivedAt: now,
		Lists:      ampLists,
		Payload:    `{"action":"created"}`,
	}
	require.NoError(t, cs.CreateTriggerEvent(ctx, evAmp))
	gotAmp, err := cs.GetTriggerEvent(ctx, evAmp.ID)
	require.NoError(t, err)
	assert.Equal(t, ampLabels, gotAmp.Lists["labels"])

	// Lists genuinely exceeding 8 KiB under SetEscapeHTML(false) are rejected with ErrInvalidInput.
	oversized := make([]string, 0, 200)
	for i := 0; i < 200; i++ {
		oversized = append(oversized, strings.Repeat("x", 60))
	}
	evOversized := &store.TriggerEvent{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindGitHub,
		SourceID:   "424242",
		Type:       "github.pull_request.opened",
		Subject:    "github:acme/widgets#44",
		DedupKey:   "92d3162e-cc78-11e3-81ab-4c9367dc0958/" + projID,
		OccurredAt: now,
		ReceivedAt: now,
		Lists:      map[string][]string{"labels": oversized},
		Payload:    `{"action":"opened"}`,
	}
	require.ErrorIs(t, cs.CreateTriggerEvent(ctx, evOversized), store.ErrInvalidInput)
}

func TestTriggerEvent_BodySHA256UniquePerProjectAndSourceKind(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	projA := uuid.NewString()
	projB := uuid.NewString()
	now := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	hash1 := strings.Repeat("a", 64)
	hash2 := strings.Repeat("b", 64)

	ev1 := &store.TriggerEvent{
		ID:         uuid.NewString(),
		ProjectID:  projA,
		SourceKind: store.TriggerSourceKindGitHub,
		SourceID:   "424242",
		Type:       "github.issue_comment.created",
		Subject:    "github:acme/widgets#1",
		DedupKey:   "11111111-1111-1111-1111-111111111111/" + projA,
		BodySHA256: hash1,
		OccurredAt: now,
		ReceivedAt: now,
		Payload:    `{"action":"created"}`,
	}
	require.NoError(t, cs.CreateTriggerEvent(ctx, ev1))

	byHash, err := cs.GetTriggerEventByBodySHA256(ctx, projA, store.TriggerSourceKindGitHub, hash1)
	require.NoError(t, err)
	assert.Equal(t, ev1.ID, byHash.ID)
	assert.Equal(t, hash1, byHash.BodySHA256)

	// Replay with a brand-new DedupKey on the same (project_id, source_kind, body_sha256)
	// violates the partial unique index and returns store.ErrAlreadyExists.
	replaySameProject := &store.TriggerEvent{
		ID:         uuid.NewString(),
		ProjectID:  projA,
		SourceKind: store.TriggerSourceKindGitHub,
		SourceID:   "424242",
		Type:       "github.issue_comment.created",
		Subject:    "github:acme/widgets#1",
		DedupKey:   "22222222-2222-2222-2222-222222222222/" + projA,
		BodySHA256: hash1,
		OccurredAt: now,
		ReceivedAt: now,
		Payload:    `{"action":"created"}`,
	}
	require.ErrorIs(t, cs.CreateTriggerEvent(ctx, replaySameProject), store.ErrAlreadyExists)

	// Same body_sha256 in a DIFFERENT project succeeds (multi-project fan-out).
	sameHashOtherProject := &store.TriggerEvent{
		ID:         uuid.NewString(),
		ProjectID:  projB,
		SourceKind: store.TriggerSourceKindGitHub,
		SourceID:   "424242",
		Type:       "github.issue_comment.created",
		Subject:    "github:acme/widgets#1",
		DedupKey:   "11111111-1111-1111-1111-111111111111/" + projB,
		BodySHA256: hash1,
		OccurredAt: now,
		ReceivedAt: now,
		Payload:    `{"action":"created"}`,
	}
	require.NoError(t, cs.CreateTriggerEvent(ctx, sameHashOtherProject))

	// Different body_sha256 in the same project succeeds.
	diffHashSameProject := &store.TriggerEvent{
		ID:         uuid.NewString(),
		ProjectID:  projA,
		SourceKind: store.TriggerSourceKindGitHub,
		SourceID:   "424242",
		Type:       "github.issue_comment.created",
		Subject:    "github:acme/widgets#1",
		DedupKey:   "33333333-3333-3333-3333-333333333333/" + projA,
		BodySHA256: hash2,
		OccurredAt: now,
		ReceivedAt: now,
		Payload:    `{"action":"created","n":2}`,
	}
	require.NoError(t, cs.CreateTriggerEvent(ctx, diffHashSameProject))

	// Multiple generic webhook events with empty body_sha256 in the same project
	// do NOT collide under the partial unique index (WHERE body_sha256 <> '').
	for i := 0; i < 2; i++ {
		wh := &store.TriggerEvent{
			ID:         uuid.NewString(),
			ProjectID:  projA,
			SourceKind: store.TriggerSourceKindWebhook,
			SourceID:   "ep-1",
			Type:       "webhook.received",
			Subject:    "webhook:ep-1",
			DedupKey:   fmt.Sprintf("wh-deliv-%d", i),
			OccurredAt: now,
			ReceivedAt: now,
			Payload:    `{}`,
		}
		require.NoError(t, cs.CreateTriggerEvent(ctx, wh))
	}
}

func TestTriggerStore_OrderedMatchRulesRoundTripAndEmptyScalars(t *testing.T) {
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	projID := uuid.NewString()
	userID := uuid.NewString()

	initialRules := []store.TriggerMatchRule{
		{Dialect: "cel", Expr: "event.attributes.branch == 'main'"},
		{Dialect: "cel", Expr: "has(event.payload.pr)"},
		{Dialect: "system_one", Expr: "classify urgent"},
	}

	tr := &store.Trigger{
		ID:            uuid.NewString(),
		ProjectID:     projID,
		Name:          "ordered-rules",
		SourceKind:    store.TriggerSourceKindWebhook,
		SourceID:      uuid.NewString(),
		MatchTypes:    []string{"webhook.received"},
		MatchRules:    initialRules,
		ActionType:    store.TriggerActionMessage,
		ActionPayload: `{"agentId":"target-1","message":"pushed"}`,
		CreatedBy:     userID,
		InitiatorAttribution: store.InitiatorAttribution{
			InitiatorPrincipalKind:  store.DelegationPrincipalUser,
			InitiatorPrincipalID:    userID,
			InitiatorCredentialKind: store.InitiatorCredentialKindSession,
			AttributionVersion:      1,
			AuthorizationRevision:   1,
		},
	}
	require.NoError(t, cs.CreateTrigger(ctx, tr))

	got, err := cs.GetTrigger(ctx, tr.ID)
	require.NoError(t, err)
	assert.Equal(t, initialRules, got.MatchRules)
	assert.Empty(t, got.MatchDialect, "scalar match_dialect column must stay empty when not explicitly set")
	assert.Empty(t, got.MatchRule, "scalar match_rule column must stay empty when not explicitly set")

	// Reorder and update MatchRules via TriggerFieldMask.
	reordered := []store.TriggerMatchRule{
		{Dialect: "cel", Expr: "has(event.payload.pr)"},
		{Dialect: "cel", Expr: "event.attributes.branch == 'main'"},
	}
	got.MatchRules = reordered
	newAttr := got.InitiatorAttribution
	newAttr.AuthorizationRevision = 2
	require.NoError(t, cs.UpdateTrigger(ctx, got, store.TriggerFieldMask{MatchRules: true}, 1, true, &newAttr))

	got2, err := cs.GetTrigger(ctx, tr.ID)
	require.NoError(t, err)
	assert.Equal(t, reordered, got2.MatchRules)
	assert.Equal(t, 2, got2.AuthorizationRevision)
	assert.Empty(t, got2.MatchDialect)
	assert.Empty(t, got2.MatchRule)

	// Clear MatchRules back to empty (Types-only).
	got2.MatchRules = nil
	newAttr.AuthorizationRevision = 3
	require.NoError(t, cs.UpdateTrigger(ctx, got2, store.TriggerFieldMask{MatchRules: true}, 2, true, &newAttr))

	got3, err := cs.GetTrigger(ctx, tr.ID)
	require.NoError(t, err)
	assert.Empty(t, got3.MatchRules)
	assert.False(t, got3.MatchRulesCorrupt)
	assert.Empty(t, got3.MatchDialect)
	assert.Empty(t, got3.MatchRule)

	// Corrupt stored match_rules payloads must mark MatchRulesCorrupt=true (fail-closed)
	// rather than silently degrading to a Types-only trigger.
	trUUID := uuid.MustParse(tr.ID)
	for _, corruptRaw := range []string{"{not-json", "null", "{}", "[null]", "[123]", `[{"dialect":123}]`} {
		require.NoError(t, cs.client.Trigger.UpdateOneID(trUUID).SetMatchRules(corruptRaw).Exec(ctx))
		corruptGot, gErr := cs.GetTrigger(ctx, tr.ID)
		require.NoError(t, gErr)
		assert.Nil(t, corruptGot.MatchRules, "corrupt payload %q must not yield partial rules", corruptRaw)
		assert.True(t, corruptGot.MatchRulesCorrupt, "corrupt payload %q must set MatchRulesCorrupt", corruptRaw)
	}

	// Replacing MatchRules on a corrupt in-memory Trigger clears MatchRulesCorrupt both in-place and on reload.
	corruptToRepair, err := cs.GetTrigger(ctx, tr.ID)
	require.NoError(t, err)
	require.True(t, corruptToRepair.MatchRulesCorrupt)
	corruptToRepair.MatchRules = []store.TriggerMatchRule{{Dialect: "cel", Expr: "true"}}
	newAttr.AuthorizationRevision = 4
	require.NoError(t, cs.UpdateTrigger(ctx, corruptToRepair, store.TriggerFieldMask{MatchRules: true}, 3, true, &newAttr))
	assert.False(t, corruptToRepair.MatchRulesCorrupt)
	repaired, err := cs.GetTrigger(ctx, tr.ID)
	require.NoError(t, err)
	assert.False(t, repaired.MatchRulesCorrupt)
	assert.Equal(t, []store.TriggerMatchRule{{Dialect: "cel", Expr: "true"}}, repaired.MatchRules)
}
