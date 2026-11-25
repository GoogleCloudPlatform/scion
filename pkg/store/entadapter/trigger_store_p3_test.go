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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTriggerStore_BoundEndpointsCascadeAndRotation exercises all Stage 2 P3
// TriggerStore methods on SQLite (default) and real PostgreSQL (when built with
// -tags integration and SCION_TEST_POSTGRES_URL).
func TestTriggerStore_BoundEndpointsCascadeAndRotation(t *testing.T) {
	t.Logf("enttest.Active (Postgres) = %v", enttest.Active())
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)

	projID := uuid.NewString()
	agentA := uuid.NewString()
	agentB := uuid.NewString()
	userID := uuid.NewString()

	mkSecret := func(epID string, ver int, val string) {
		t.Helper()
		require.NoError(t, cs.CreateSecret(ctx, &store.Secret{
			ID:             uuid.NewString(),
			Key:            store.WebhookEndpointSecretKey(projID, epID, ver),
			EncryptedValue: val,
			Scope:          store.ScopeHub,
			ScopeID:        "hub-1",
			SecretType:     store.SecretTypeInternal,
			CreatedBy:      userID,
			UpdatedBy:      userID,
		}))
	}

	// 1. Create agent-lifetime and persistent bound endpoints + unbound endpoint.
	epAgent := &store.WebhookEndpoint{
		ID:                  uuid.NewString(),
		ProjectID:           projID,
		Name:                "ep-agent-life",
		OwnerPrincipalKind:  store.DelegationPrincipalAgent,
		OwnerPrincipalID:    agentA,
		OwnerCredentialKind: store.InitiatorCredentialKindAgent,
		SecretVersion:       1,
		SignatureMode:       store.WebhookSignatureModeScionV1,
		Status:              store.WebhookEndpointStatusActive,
		BoundAgentID:        agentA,
		BoundLifetime:       store.WebhookBoundLifetimeAgent,
		CreatedBy:           agentA,
	}
	require.NoError(t, cs.CreateWebhookEndpoint(ctx, epAgent))
	mkSecret(epAgent.ID, 1, "sealed-a-v1")

	epPersist := &store.WebhookEndpoint{
		ID:                  uuid.NewString(),
		ProjectID:           projID,
		Name:                "ep-persist",
		OwnerPrincipalKind:  store.DelegationPrincipalUser,
		OwnerPrincipalID:    userID,
		OwnerCredentialKind: store.InitiatorCredentialKindSession,
		SecretVersion:       1,
		SignatureMode:       store.WebhookSignatureModeScionV1,
		Status:              store.WebhookEndpointStatusActive,
		BoundAgentID:        agentA,
		BoundLifetime:       store.WebhookBoundLifetimePersistent,
		CreatedBy:           userID,
	}
	require.NoError(t, cs.CreateWebhookEndpoint(ctx, epPersist))
	mkSecret(epPersist.ID, 1, "sealed-p-v1")

	epOther := &store.WebhookEndpoint{
		ID:                  uuid.NewString(),
		ProjectID:           projID,
		Name:                "ep-other",
		OwnerPrincipalKind:  store.DelegationPrincipalUser,
		OwnerPrincipalID:    userID,
		OwnerCredentialKind: store.InitiatorCredentialKindSession,
		SecretVersion:       1,
		SignatureMode:       store.WebhookSignatureModeScionV1,
		Status:              store.WebhookEndpointStatusActive,
		BoundAgentID:        agentB,
		BoundLifetime:       store.WebhookBoundLifetimeAgent,
		CreatedBy:           userID,
	}
	require.NoError(t, cs.CreateWebhookEndpoint(ctx, epOther))
	mkSecret(epOther.ID, 1, "sealed-o-v1")

	// CountBoundWebhookEndpoints & ListWebhookEndpoints filters.
	cntA, err := cs.CountBoundWebhookEndpoints(ctx, projID, agentA)
	require.NoError(t, err)
	assert.Equal(t, 2, cntA)

	cntB, err := cs.CountBoundWebhookEndpoints(ctx, projID, agentB)
	require.NoError(t, err)
	assert.Equal(t, 1, cntB)

	// VisibleToAgentID returns endpoints owned by OR bound to agentA (epAgent + epPersist, NOT epOther).
	visA, err := cs.ListWebhookEndpoints(ctx, store.WebhookEndpointFilter{
		ProjectID:        projID,
		VisibleToAgentID: agentA,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Len(t, visA.Items, 2)

	// BoundAgentID filter returns only endpoints bound to agentB.
	boundB, err := cs.ListWebhookEndpoints(ctx, store.WebhookEndpointFilter{
		ProjectID:    projID,
		BoundAgentID: agentB,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, boundB.Items, 1)
	assert.Equal(t, epOther.ID, boundB.Items[0].ID)

	// ListTriggerEvents with VisibleToAgentID includes events on user-owned endpoint bound to agentA.
	evBound := &store.TriggerEvent{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epPersist.ID,
		Type:       "webhook.received",
		Subject:    "webhook:" + epPersist.ID,
		DedupKey:   uuid.NewString(),
		OccurredAt: time.Now().UTC(),
		ReceivedAt: time.Now().UTC(),
		Payload:    `{"ok":true}`,
		Status:     store.TriggerEventStatusProcessed,
	}
	require.NoError(t, cs.CreateTriggerEvent(ctx, evBound))

	evListA, err := cs.ListTriggerEvents(ctx, store.TriggerEventFilter{
		ProjectID:        projID,
		VisibleToAgentID: agentA,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, evListA.Items, 1)
	assert.Equal(t, evBound.ID, evListA.Items[0].ID)

	evListB, err := cs.ListTriggerEvents(ctx, store.TriggerEventFilter{
		ProjectID:        projID,
		VisibleToAgentID: agentB,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, evListB.Items)

	// 2. UpdateWebhookEndpoint increments config_version and updates masked fields.
	epPersist.Description = "updated desc"
	epPersist.Status = store.WebhookEndpointStatusDisabled
	require.NoError(t, cs.UpdateWebhookEndpoint(ctx, epPersist, store.WebhookEndpointFieldMask{
		Description: true,
		Status:      true,
	}))
	assert.Equal(t, "updated desc", epPersist.Description)
	assert.Equal(t, store.WebhookEndpointStatusDisabled, epPersist.Status)
	assert.Equal(t, 2, epPersist.ConfigVersion)

	// 3. RotateWebhookEndpointSecret + PurgeExpiredRotatedWebhookSecrets.
	t0 := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	mkSecret(epPersist.ID, 2, "sealed-p-v2")
	rotated, err := cs.RotateWebhookEndpointSecret(ctx, projID, epPersist.ID, 2, t0)
	require.NoError(t, err)
	assert.Equal(t, 2, rotated.SecretVersion)
	require.NotNil(t, rotated.SecretRotatedAt)
	assert.True(t, rotated.SecretRotatedAt.Equal(t0))

	// Within 24h grace: v1 is NOT purged.
	purged, err := cs.PurgeExpiredRotatedWebhookSecrets(ctx, t0.Add(23*time.Hour), store.WebhookSecretRotationGraceWindow)
	require.NoError(t, err)
	assert.Equal(t, 0, purged)
	_, err = cs.GetSecret(ctx, store.WebhookEndpointSecretKey(projID, epPersist.ID, 1), store.ScopeHub, "hub-1")
	require.NoError(t, err)

	// After 24h grace: v1 IS purged, v2 stays.
	purged, err = cs.PurgeExpiredRotatedWebhookSecrets(ctx, t0.Add(24*time.Hour+time.Second), store.WebhookSecretRotationGraceWindow)
	require.NoError(t, err)
	assert.Equal(t, 1, purged)
	_, err = cs.GetSecret(ctx, store.WebhookEndpointSecretKey(projID, epPersist.ID, 1), store.ScopeHub, "hub-1")
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = cs.GetSecret(ctx, store.WebhookEndpointSecretKey(projID, epPersist.ID, 2), store.ScopeHub, "hub-1")
	require.NoError(t, err)

	// 4. DeleteWebhookEndpointForBoundAgent: deletes epAgent, its secrets, and
	//    agentA's own trigger; pauses userID's trigger with source_deleted.
	trOwn := &store.Trigger{
		ID:             uuid.NewString(),
		ProjectID:      projID,
		Name:           "tr-own",
		SourceKind:     store.TriggerSourceKindWebhook,
		SourceID:       epAgent.ID,
		MatchTypes:     []string{"webhook.received"},
		ActionType:     store.TriggerActionMessage,
		ActionPayload:  `{"agentId":"` + agentA + `","message":"hi"}`,
		SubjectDialect: store.TriggerDialectCEL,
		SubjectMode:    store.TriggerSubjectModeNew,
		Status:         store.TriggerStatusActive,
		CreatedBy:      agentA,
		InitiatorAttribution: store.InitiatorAttribution{
			AttributionVersion:      1,
			InitiatorPrincipalKind:  store.DelegationPrincipalAgent,
			InitiatorPrincipalID:    agentA,
			InitiatorCredentialKind: store.InitiatorCredentialKindAgent,
			AuthorizationRevision:   1,
		},
	}
	require.NoError(t, cs.CreateTrigger(ctx, trOwn))

	trOtherAuthor := &store.Trigger{
		ID:             uuid.NewString(),
		ProjectID:      projID,
		Name:           "tr-other-author",
		SourceKind:     store.TriggerSourceKindWebhook,
		SourceID:       epAgent.ID,
		MatchTypes:     []string{"webhook.received"},
		ActionType:     store.TriggerActionMessage,
		ActionPayload:  `{"agentId":"` + agentB + `","message":"hi"}`,
		SubjectDialect: store.TriggerDialectCEL,
		SubjectMode:    store.TriggerSubjectModeNew,
		Status:         store.TriggerStatusActive,
		CreatedBy:      userID,
		InitiatorAttribution: store.InitiatorAttribution{
			AttributionVersion:      1,
			InitiatorPrincipalKind:  store.DelegationPrincipalUser,
			InitiatorPrincipalID:    userID,
			InitiatorCredentialKind: store.InitiatorCredentialKindSession,
			AuthorizationRevision:   1,
		},
	}
	require.NoError(t, cs.CreateTrigger(ctx, trOtherAuthor))

	delTrs, pausedTrs, delSecs, err := cs.DeleteWebhookEndpointForBoundAgent(ctx, projID, epAgent.ID, agentA)
	require.NoError(t, err)
	assert.Equal(t, []string{trOwn.ID}, delTrs)
	assert.Equal(t, []string{trOtherAuthor.ID}, pausedTrs)
	assert.Len(t, delSecs, 1)

	_, err = cs.GetWebhookEndpoint(ctx, epAgent.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = cs.GetTrigger(ctx, trOwn.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	gotOther, err := cs.GetTrigger(ctx, trOtherAuthor.ID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerStatusPaused, gotOther.Status)
	assert.Equal(t, store.TriggerStatusReasonSourceDeleted, gotOther.StatusReason)

	// Retry is idempotent (ErrNotFound).
	_, _, _, err = cs.DeleteWebhookEndpointForBoundAgent(ctx, projID, epAgent.ID, agentA)
	assert.ErrorIs(t, err, store.ErrNotFound)

	// 5. UnbindWebhookEndpointForAgent: unbinds persistent endpoint; retry returns false.
	unbound, err := cs.UnbindWebhookEndpointForAgent(ctx, projID, epPersist.ID, agentA)
	require.NoError(t, err)
	assert.True(t, unbound)

	gotPersist, err := cs.GetWebhookEndpoint(ctx, epPersist.ID)
	require.NoError(t, err)
	assert.Empty(t, gotPersist.BoundAgentID)
	assert.Empty(t, gotPersist.BoundLifetime)

	unboundAgain, err := cs.UnbindWebhookEndpointForAgent(ctx, projID, epPersist.ID, agentA)
	require.NoError(t, err)
	assert.False(t, unboundAgain)

	// 6. ListOrphanedAgentLifetimeWebhookEndpoints (O2 backstop):
	//    returns agent-lifetime endpoints bound to missing or soft-deleted agents,
	//    and excludes live agents and persistent endpoints.
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID:   projID,
		Name: "orphan-proj",
		Slug: "orphan-" + projID[:8],
	}))
	liveAg := &store.Agent{
		ID:        agentB,
		ProjectID: projID,
		Name:      "live-agent",
		Slug:      "live-agent",
	}
	require.NoError(t, cs.CreateAgent(ctx, liveAg))

	softDelAgID := uuid.NewString()
	softDelAg := &store.Agent{
		ID:        softDelAgID,
		ProjectID: projID,
		Name:      "softdel-agent",
		Slug:      "softdel-agent",
	}
	require.NoError(t, cs.CreateAgent(ctx, softDelAg))
	softDelTime := time.Now().UTC().Truncate(time.Microsecond)
	_, err = cs.client.Agent.UpdateOneID(uuid.MustParse(softDelAgID)).SetDeletedAt(softDelTime).Save(ctx)
	require.NoError(t, err)

	epOrphanSoft := &store.WebhookEndpoint{
		ID:                  uuid.NewString(),
		ProjectID:           projID,
		Name:                "ep-orphan-softdel",
		OwnerPrincipalKind:  store.DelegationPrincipalAgent,
		OwnerPrincipalID:    softDelAgID,
		OwnerCredentialKind: store.InitiatorCredentialKindAgent,
		SecretVersion:       1,
		SignatureMode:       store.WebhookSignatureModeScionV1,
		Status:              store.WebhookEndpointStatusActive,
		BoundAgentID:        softDelAgID,
		BoundLifetime:       store.WebhookBoundLifetimeAgent,
	}
	require.NoError(t, cs.CreateWebhookEndpoint(ctx, epOrphanSoft))

	missingAgID := uuid.NewString()
	epOrphanMissing := &store.WebhookEndpoint{
		ID:                  uuid.NewString(),
		ProjectID:           projID,
		Name:                "ep-orphan-missing",
		OwnerPrincipalKind:  store.DelegationPrincipalUser,
		OwnerPrincipalID:    userID,
		OwnerCredentialKind: store.InitiatorCredentialKindSession,
		SecretVersion:       1,
		SignatureMode:       store.WebhookSignatureModeScionV1,
		Status:              store.WebhookEndpointStatusActive,
		BoundAgentID:        missingAgID,
		BoundLifetime:       store.WebhookBoundLifetimeAgent,
	}
	require.NoError(t, cs.CreateWebhookEndpoint(ctx, epOrphanMissing))

	epPersistSoft := &store.WebhookEndpoint{
		ID:                  uuid.NewString(),
		ProjectID:           projID,
		Name:                "ep-persist-softdel",
		OwnerPrincipalKind:  store.DelegationPrincipalUser,
		OwnerPrincipalID:    userID,
		OwnerCredentialKind: store.InitiatorCredentialKindSession,
		SecretVersion:       1,
		SignatureMode:       store.WebhookSignatureModeScionV1,
		Status:              store.WebhookEndpointStatusActive,
		BoundAgentID:        softDelAgID,
		BoundLifetime:       store.WebhookBoundLifetimePersistent,
	}
	require.NoError(t, cs.CreateWebhookEndpoint(ctx, epPersistSoft))

	orphans, err := cs.ListOrphanedBoundWebhookEndpoints(ctx, 50)
	require.NoError(t, err)
	orphanIDs := make([]string, 0, len(orphans))
	for _, o := range orphans {
		orphanIDs = append(orphanIDs, o.ID)
	}
	assert.Contains(t, orphanIDs, epOrphanSoft.ID)
	assert.Contains(t, orphanIDs, epOrphanMissing.ID)
	assert.Contains(t, orphanIDs, epPersistSoft.ID, "persistent endpoint bound to soft-deleted agent must be orphaned (O2)")
	assert.NotContains(t, orphanIDs, epOther.ID, "live agent endpoint must not be orphaned")

	// 7. O1 restore race: if the soft-deleted agent is restored after listing
	//    orphans, DeleteWebhookEndpointForBoundAgent must return ErrNotFound
	//    and UnbindWebhookEndpointForAgent must return false, leaving both intact.
	_, err = cs.client.Agent.UpdateOneID(uuid.MustParse(softDelAgID)).ClearDeletedAt().Save(ctx)
	require.NoError(t, err)

	_, _, _, err = cs.DeleteWebhookEndpointForBoundAgent(ctx, projID, epOrphanSoft.ID, softDelAgID)
	assert.ErrorIs(t, err, store.ErrNotFound, "must not delete endpoint of live/restored agent")
	_, err = cs.GetWebhookEndpoint(ctx, epOrphanSoft.ID)
	require.NoError(t, err, "epOrphanSoft must remain after restore")

	unboundRestored, err := cs.UnbindWebhookEndpointForAgent(ctx, projID, epPersistSoft.ID, softDelAgID)
	require.NoError(t, err)
	assert.False(t, unboundRestored, "must not unbind persistent endpoint of live/restored agent")
	gotPersistRestored, err := cs.GetWebhookEndpoint(ctx, epPersistSoft.ID)
	require.NoError(t, err)
	assert.Equal(t, softDelAgID, gotPersistRestored.BoundAgentID)

	// Re-soft-deleting softDelAgID allows both delete and persistent unbind to succeed.
	_, err = cs.client.Agent.UpdateOneID(uuid.MustParse(softDelAgID)).SetDeletedAt(softDelTime).Save(ctx)
	require.NoError(t, err)

	_, _, _, err = cs.DeleteWebhookEndpointForBoundAgent(ctx, projID, epOrphanSoft.ID, softDelAgID)
	require.NoError(t, err)
	_, err = cs.GetWebhookEndpoint(ctx, epOrphanSoft.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	unboundOrphanPersist, err := cs.UnbindWebhookEndpointForAgent(ctx, projID, epPersistSoft.ID, softDelAgID)
	require.NoError(t, err)
	assert.True(t, unboundOrphanPersist)
	gotPersistAfterUnbind, err := cs.GetWebhookEndpoint(ctx, epPersistSoft.ID)
	require.NoError(t, err)
	assert.Empty(t, gotPersistAfterUnbind.BoundAgentID)
	assert.Empty(t, gotPersistAfterUnbind.BoundLifetime)
}
