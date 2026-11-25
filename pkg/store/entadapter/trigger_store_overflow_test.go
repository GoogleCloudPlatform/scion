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

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

func TestTriggerEventOverflowStore_SumExistingAndPurge(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	t.Logf("dialect=%s", client.Driver().Dialect())
	cs := NewCompositeStore(client)

	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	projA := uuid.NewString()
	projB := uuid.NewString()
	epA := uuid.NewString()
	epB := uuid.NewString()

	// Empty project returns 0 without error (NULL aggregate handled on SQLite and Postgres).
	sum0, err := cs.SumTriggerEventOverflowBytes(ctx, projA, now.Add(-7*24*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, int64(0), sum0)

	// Create:
	// 1. Inline event in projA (payload_ref == "") -> must NOT be counted in overflow sum.
	evInline := &store.TriggerEvent{
		ID:           uuid.NewString(),
		ProjectID:    projA,
		SourceKind:   store.TriggerSourceKindWebhook,
		SourceID:     epA,
		Type:         "webhook.received",
		Subject:      "webhook:" + epA,
		DedupKey:     "inline-1",
		OccurredAt:   now,
		ReceivedAt:   now,
		Payload:      `{"small":true}`,
		PayloadBytes: 14,
		Status:       store.TriggerEventStatusProcessed,
		CreatedAt:    now,
	}
	require.NoError(t, cs.CreateTriggerEvent(ctx, evInline))

	// 2. Live overflow event in projA (900 KiB, within 7-day window).
	refLiveA := "trigger-events/" + projA + "/" + uuid.NewString() + ".json"
	evLiveA := &store.TriggerEvent{
		ID:           uuid.NewString(),
		ProjectID:    projA,
		SourceKind:   store.TriggerSourceKindWebhook,
		SourceID:     epA,
		Type:         "webhook.received",
		Subject:      "webhook:" + epA,
		DedupKey:     "overflow-live-a",
		OccurredAt:   now.Add(-2 * time.Hour),
		ReceivedAt:   now.Add(-2 * time.Hour),
		Payload:      "",
		PayloadRef:   refLiveA,
		PayloadBytes: 900 * 1024,
		Status:       store.TriggerEventStatusProcessed,
		CreatedAt:    now.Add(-2 * time.Hour),
	}
	require.NoError(t, cs.CreateTriggerEvent(ctx, evLiveA))

	// Round-trip PayloadRef via GetTriggerEvent.
	gotLiveA, err := cs.GetTriggerEvent(ctx, evLiveA.ID)
	require.NoError(t, err)
	assert.Equal(t, refLiveA, gotLiveA.PayloadRef)
	assert.Equal(t, "", gotLiveA.Payload)
	assert.Equal(t, 900*1024, gotLiveA.PayloadBytes)

	// 3. Expired overflow event in projA (8 days old, processed) -> outside 7d window and purgeable.
	refExpiredA := "trigger-events/" + projA + "/" + uuid.NewString() + ".json"
	evExpiredA := &store.TriggerEvent{
		ID:           uuid.NewString(),
		ProjectID:    projA,
		SourceKind:   store.TriggerSourceKindWebhook,
		SourceID:     epA,
		Type:         "webhook.received",
		Subject:      "webhook:" + epA,
		DedupKey:     "overflow-expired-a",
		OccurredAt:   now.Add(-8 * 24 * time.Hour),
		ReceivedAt:   now.Add(-8 * 24 * time.Hour),
		Payload:      "",
		PayloadRef:   refExpiredA,
		PayloadBytes: 500 * 1024,
		Status:       store.TriggerEventStatusProcessed,
		CreatedAt:    now.Add(-8 * 24 * time.Hour),
	}
	require.NoError(t, cs.CreateTriggerEvent(ctx, evExpiredA))

	// 4. Live overflow event in projB (must be isolated from projA).
	refLiveB := "trigger-events/" + projB + "/" + uuid.NewString() + ".json"
	evLiveB := &store.TriggerEvent{
		ID:           uuid.NewString(),
		ProjectID:    projB,
		SourceKind:   store.TriggerSourceKindWebhook,
		SourceID:     epB,
		Type:         "webhook.received",
		Subject:      "webhook:" + epB,
		DedupKey:     "overflow-live-b",
		OccurredAt:   now.Add(-time.Hour),
		ReceivedAt:   now.Add(-time.Hour),
		Payload:      "",
		PayloadRef:   refLiveB,
		PayloadBytes: 700 * 1024,
		Status:       store.TriggerEventStatusPending,
		CreatedAt:    now.Add(-time.Hour),
	}
	require.NoError(t, cs.CreateTriggerEvent(ctx, evLiveB))

	// Sum within 7-day retention window: projA counts only evLiveA (900 KiB), projB counts evLiveB (700 KiB).
	sumA, err := cs.SumTriggerEventOverflowBytes(ctx, projA, now.Add(-7*24*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, int64(900*1024), sumA)

	sumB, err := cs.SumTriggerEventOverflowBytes(ctx, projB, now.Add(-7*24*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, int64(700*1024), sumB)

	// ExistingTriggerEventPayloadRefs reports present refs scoped to projectID (uses (project_id, payload_ref) index, O3).
	refOrphan := "trigger-events/" + projA + "/" + uuid.NewString() + ".json"
	existingA, err := cs.ExistingTriggerEventPayloadRefs(ctx, projA, []string{refLiveA, refExpiredA, refLiveB, refOrphan})
	require.NoError(t, err)
	assert.True(t, existingA[refLiveA])
	assert.True(t, existingA[refExpiredA])
	assert.False(t, existingA[refLiveB], "projB ref must not match when querying projA")
	assert.False(t, existingA[refOrphan])

	existingB, err := cs.ExistingTriggerEventPayloadRefs(ctx, projB, []string{refLiveB, refLiveA})
	require.NoError(t, err)
	assert.True(t, existingB[refLiveB])
	assert.False(t, existingB[refLiveA], "projA ref must not match when querying projB")

	// PurgeOldTriggerHistoryWithPayloadRefs deletes evExpiredA and returns its payload_ref.
	evDel, firesDel, purgedRefs, err := cs.PurgeOldTriggerHistoryWithPayloadRefs(ctx, now.Add(-7*24*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 1, evDel)
	assert.Equal(t, 0, firesDel)
	assert.Equal(t, []string{refExpiredA}, purgedRefs)

	// After purge, refExpiredA is no longer in ExistingTriggerEventPayloadRefs; live refs remain.
	existingAfterA, err := cs.ExistingTriggerEventPayloadRefs(ctx, projA, []string{refLiveA, refExpiredA})
	require.NoError(t, err)
	assert.True(t, existingAfterA[refLiveA])
	assert.False(t, existingAfterA[refExpiredA])
	existingAfterB, err := cs.ExistingTriggerEventPayloadRefs(ctx, projB, []string{refLiveB})
	require.NoError(t, err)
	assert.True(t, existingAfterB[refLiveB])
}
