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

package trigger_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

type g4aTestCore struct {
	mu       sync.Mutex
	fired    []hubfire.FireRequest
	fireFn   func(ctx context.Context, req hubfire.FireRequest) hubfire.FireOutcome
	renderFn func(ctx context.Context, req hubfire.FireRequest) (string, *hubfire.FireOutcome, error)
}

func (c *g4aTestCore) Fire(ctx context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
	c.mu.Lock()
	c.fired = append(c.fired, req)
	fn := c.fireFn
	c.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}
	return hubfire.FireOutcome{Status: hubfire.FireStatusFired, ResultRef: "res-" + req.Authority.FireID}
}

func (c *g4aTestCore) Experiment(name string) bool {
	return name == experiments.Triggers || name == experiments.GitHubTriggers
}

func (c *g4aTestCore) AuthorizeEventSource(_ context.Context, _ *store.TriggerEvent) (bool, error) {
	return true, nil
}

func (c *g4aTestCore) RenderReuseMessage(ctx context.Context, req hubfire.FireRequest) (string, *hubfire.FireOutcome, error) {
	if c.renderFn != nil {
		return c.renderFn(ctx, req)
	}
	return "", nil, nil
}

func (c *g4aTestCore) firedRequests() []hubfire.FireRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]hubfire.FireRequest, len(c.fired))
	copy(out, c.fired)
	return out
}

func makeEventItem(evID, projectID string) durablequeue.Item {
	payload, _ := json.Marshal(trigger.EventQueuePayload{
		Version: trigger.EventQueuePayloadVersion,
		EventID: evID,
	})
	return durablequeue.Item{
		Kind:      trigger.EventQueueKind,
		DedupKey:  evID,
		ProjectID: projectID,
		Payload:   payload,
		Attempts:  1,
	}
}

func TestDispatcher_CoalesceWindow_LatestWinsAndNonCoalescingImmediate(t *testing.T) {
	ctx := context.Background()
	entClient := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(entClient)
	projID := seedProjectForDispatch(t, cs)

	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	// Coalescing rule (30s window) + non-coalescing rule on the same synchronize event.
	trCoalesce := makeActiveTrigger(projID, "", "sync-coalesce", []string{"github.pull_request.synchronize"})
	trCoalesce.SourceKind = store.TriggerSourceKindGitHub
	trCoalesce.ActionType = store.TriggerActionDispatchAgent
	trCoalesce.ActionPayload = `{"template":"default","task":"review ${event.attributes.head_sha}"}`
	trCoalesce.CoalesceWindowSeconds = 30
	require.NoError(t, cs.CreateTrigger(ctx, trCoalesce))

	trImmediate := makeActiveTrigger(projID, "", "sync-immediate", []string{"github.pull_request.synchronize"})
	trImmediate.SourceKind = store.TriggerSourceKindGitHub
	trImmediate.ActionType = store.TriggerActionMessage
	trImmediate.ActionPayload = `{"agentId":"a-1","message":"immediate"}`
	require.NoError(t, cs.CreateTrigger(ctx, trImmediate))

	core := &g4aTestCore{}
	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:          core,
		Store:         cs,
		SubjectClaims: trigger.NewSubjectClaims(cs, cs, clock),
		Now:           clock,
	})
	require.NoError(t, err)

	// Deliver 3 synchronize events within the 30s window with different head_sha values.
	shas := []string{"sha-111", "sha-222", "sha-333"}
	evIDs := make([]string, len(shas))
	for i, sha := range shas {
		evID := uuid.NewString()
		evIDs[i] = evID
		require.NoError(t, cs.CreateTriggerEvent(ctx, &store.TriggerEvent{
			ID:         evID,
			ProjectID:  projID,
			SourceKind: store.TriggerSourceKindGitHub,
			SourceID:   "1001",
			Type:       "github.pull_request.synchronize",
			Subject:    "github:octo/repo#42",
			DedupKey:   "del-" + sha,
			OccurredAt: now,
			ReceivedAt: now,
			Attributes: map[string]string{"head_sha": sha},
			Status:     store.TriggerEventStatusPending,
		}))

		res := disp.HandleItem(ctx, makeEventItem(evID, projID), nil)
		assert.False(t, res.Done, "event %d must retry at not_before for coalescing rule", i)
		assert.Equal(t, now.Add(30*time.Second), res.RetryAt.UTC())

		// Non-coalescing rule must have fired immediately on every event.
		fired := core.firedRequests()
		require.Len(t, fired, i+1, "non-coalescing rule must fire immediately on event %d", i)
		assert.Equal(t, trImmediate.ID, fired[i].Authority.DefinitionID)

		now = now.Add(5 * time.Second)
	}

	// Advance past the latest event's coalesce window (ev3's not_before = t0+10s+30s = t0+40s).
	now = now.Add(30 * time.Second)

	// Re-handle all 3 events: E1 and E2 were superseded -> Done without firing; E3 fires with sha-333.
	for i, evID := range evIDs {
		res := disp.HandleItem(ctx, makeEventItem(evID, projID), nil)
		assert.True(t, res.Done, "event %d must finish Done after window", i)
	}

	// Total fires sent to Core: 3 immediate + 1 coalesced (latest SHA = sha-333).
	fired := core.firedRequests()
	require.Len(t, fired, 4)
	lastFire := fired[3]
	assert.Equal(t, trCoalesce.ID, lastFire.Authority.DefinitionID)
	require.NotNil(t, lastFire.Event)
	assert.Equal(t, "sha-333", lastFire.Event.Attributes["head_sha"])

	// Verify trigger_fires for trCoalesce: 2 skipped "coalesced" + 1 fired.
	firesRes, err := cs.ListTriggerFires(ctx, store.TriggerFireFilter{TriggerID: trCoalesce.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, firesRes.Items, 3)
	var coalescedSkips, firedCount int
	for _, f := range firesRes.Items {
		switch f.Status {
		case store.TriggerFireStatusSkipped:
			assert.Equal(t, trigger.ErrFireCoalesced, f.Error)
			coalescedSkips++
		case store.TriggerFireStatusFired:
			assert.Equal(t, evIDs[2], f.EventID)
			firedCount++
		default:
			t.Fatalf("unexpected fire status %q", f.Status)
		}
	}
	assert.Equal(t, 2, coalescedSkips)
	assert.Equal(t, 1, firedCount)
}

func TestDispatcher_CoalesceWindow_ReapAndRetryNeverDuplicated(t *testing.T) {
	ctx := context.Background()
	entClient := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(entClient)
	projID := seedProjectForDispatch(t, cs)

	now := time.Date(2026, 11, 22, 13, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	tr := makeActiveTrigger(projID, "", "sync-reap", []string{"github.pull_request.synchronize"})
	tr.SourceKind = store.TriggerSourceKindGitHub
	tr.ActionType = store.TriggerActionDispatchAgent
	tr.CoalesceWindowSeconds = 30
	require.NoError(t, cs.CreateTrigger(ctx, tr))

	core := &g4aTestCore{}
	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:  core,
		Store: cs,
		Now:   clock,
	})
	require.NoError(t, err)

	evID := uuid.NewString()
	require.NoError(t, cs.CreateTriggerEvent(ctx, &store.TriggerEvent{
		ID:         evID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindGitHub,
		SourceID:   "1001",
		Type:       "github.pull_request.synchronize",
		Subject:    "github:octo/repo#99",
		DedupKey:   "del-reap-1",
		OccurredAt: now,
		ReceivedAt: now,
		Status:     store.TriggerEventStatusPending,
	}))

	// 1. First handle puts fire in coalescing.
	res := disp.HandleItem(ctx, makeEventItem(evID, projID), nil)
	assert.False(t, res.Done)
	assert.Empty(t, core.firedRequests())

	// 2. Hub restarts and reaper runs >1 lease past not_before.
	now = now.Add(30*time.Second + trigger.FireLeaseHorizon + time.Minute)
	mres, err := trigger.RunMaintenance(ctx, cs, now)
	require.NoError(t, err)
	assert.Equal(t, 1, mres.FiresReaped)

	firesBefore, err := cs.ListTriggerFires(ctx, store.TriggerFireFilter{TriggerID: tr.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, firesBefore.Items, 1)
	assert.Equal(t, store.TriggerFireStatusFailed, firesBefore.Items[0].Status)
	assert.Equal(t, trigger.ErrFireReapedDuringCoalesce, firesBefore.Items[0].Error)
	origFireID := firesBefore.Items[0].ID

	// 3. Queue item retries -> re-fires the SAME fire row, never duplicated.
	now = now.Add(time.Minute)
	res2 := disp.HandleItem(ctx, makeEventItem(evID, projID), nil)
	assert.True(t, res2.Done)
	require.Len(t, core.firedRequests(), 1)

	firesAfter, err := cs.ListTriggerFires(ctx, store.TriggerFireFilter{TriggerID: tr.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, firesAfter.Items, 1, "reaped coalescing row must be reused, never duplicated")
	assert.Equal(t, origFireID, firesAfter.Items[0].ID)
	assert.Equal(t, store.TriggerFireStatusFired, firesAfter.Items[0].Status)
}

func TestDispatcher_StaleEventRefusal(t *testing.T) {
	ctx := context.Background()
	entClient := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(entClient)
	projID := seedProjectForDispatch(t, cs)

	now := time.Date(2026, 11, 22, 14, 0, 0, 0, time.UTC)

	trDispatch := makeActiveTrigger(projID, "", "stale-dispatch", []string{"github.issue_comment.created"})
	trDispatch.SourceKind = store.TriggerSourceKindGitHub
	trDispatch.ActionType = store.TriggerActionDispatchAgent
	trDispatch.SubjectMode = store.TriggerSubjectModeReuse
	require.NoError(t, cs.CreateTrigger(ctx, trDispatch))

	trComment := makeActiveTrigger(projID, "", "stale-comment", []string{"github.issue_comment.created"})
	trComment.SourceKind = store.TriggerSourceKindGitHub
	trComment.ActionType = store.TriggerActionGitHubComment
	require.NoError(t, cs.CreateTrigger(ctx, trComment))

	trMessage := makeActiveTrigger(projID, "", "stale-message", []string{"github.issue_comment.created"})
	trMessage.SourceKind = store.TriggerSourceKindGitHub
	trMessage.ActionType = store.TriggerActionMessage
	require.NoError(t, cs.CreateTrigger(ctx, trMessage))

	core := &g4aTestCore{}
	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:  core,
		Store: cs,
		Now:   func() time.Time { return now },
	})
	require.NoError(t, err)

	evID := uuid.NewString()
	require.NoError(t, cs.CreateTriggerEvent(ctx, &store.TriggerEvent{
		ID:         evID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindGitHub,
		SourceID:   "1001",
		Type:       "github.issue_comment.created",
		Subject:    "github:octo/repo#7",
		DedupKey:   "del-stale-1",
		OccurredAt: now.Add(-48 * time.Hour),
		ReceivedAt: now,
		Attributes: map[string]string{"stale": "true"},
		Status:     store.TriggerEventStatusPending,
	}))

	res := disp.HandleItem(ctx, makeEventItem(evID, projID), nil)
	assert.True(t, res.Done)

	// Only message rule fired; dispatch_agent and github.comment were refused "stale event".
	fired := core.firedRequests()
	require.Len(t, fired, 1)
	assert.Equal(t, trMessage.ID, fired[0].Authority.DefinitionID)

	for _, tid := range []string{trDispatch.ID, trComment.ID} {
		fRes, err := cs.ListTriggerFires(ctx, store.TriggerFireFilter{TriggerID: tid}, store.ListOptions{Limit: 5})
		require.NoError(t, err)
		require.Len(t, fRes.Items, 1)
		assert.Equal(t, store.TriggerFireStatusRefused, fRes.Items[0].Status)
		assert.Equal(t, trigger.ErrFireStaleEvent, fRes.Items[0].Error)
	}
}

func TestDispatcher_SubjectModeReuse_FreshUnboundClaimSkipsInProgress(t *testing.T) {
	ctx := context.Background()
	entClient := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(entClient)
	projID := seedProjectForDispatch(t, cs)

	now := time.Date(2026, 11, 22, 15, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	claims := trigger.NewSubjectClaims(cs, cs, clock)

	tr := makeActiveTrigger(projID, "", "reuse-in-progress", []string{"github.issue_comment.created"})
	tr.SourceKind = store.TriggerSourceKindGitHub
	tr.ActionType = store.TriggerActionDispatchAgent
	tr.SubjectMode = store.TriggerSubjectModeReuse
	require.NoError(t, cs.CreateTrigger(ctx, tr))

	// Seed a fresh unbound active claim (another replica mid-create, < 5 min old).
	const subject = "github:octo/repo#55"
	_, existing, err := claims.Claim(ctx, tr.ID, subject, uuid.NewString(), uuid.NewString())
	require.NoError(t, err)
	require.Nil(t, existing)

	core := &g4aTestCore{}
	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:          core,
		Store:         cs,
		SubjectClaims: claims,
		Now:           clock,
	})
	require.NoError(t, err)

	// 1. While unbound claim is fresh (< 5 min), reuse skips with "subject dispatch in progress".
	ev1 := uuid.NewString()
	require.NoError(t, cs.CreateTriggerEvent(ctx, &store.TriggerEvent{
		ID:         ev1,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindGitHub,
		SourceID:   "1001",
		Type:       "github.issue_comment.created",
		Subject:    subject,
		DedupKey:   "del-inprog-1",
		OccurredAt: now,
		ReceivedAt: now,
		Status:     store.TriggerEventStatusPending,
	}))
	res1 := disp.HandleItem(ctx, makeEventItem(ev1, projID), nil)
	assert.True(t, res1.Done)
	assert.Empty(t, core.firedRequests())

	fires1, err := cs.ListTriggerFires(ctx, store.TriggerFireFilter{TriggerID: tr.ID, EventID: ev1}, store.ListOptions{Limit: 5})
	require.NoError(t, err)
	require.Len(t, fires1.Items, 1)
	assert.Equal(t, store.TriggerFireStatusSkipped, fires1.Items[0].Status)
	assert.Equal(t, trigger.ErrFireSubjectDispatchInProgress, fires1.Items[0].Error)

	// 2. Once unbound claim is abandoned (> 5 min old), reuse falls through to dispatch_agent Fire.
	now = now.Add(trigger.AbandonedClaimTTL + time.Second)
	ev2 := uuid.NewString()
	require.NoError(t, cs.CreateTriggerEvent(ctx, &store.TriggerEvent{
		ID:         ev2,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindGitHub,
		SourceID:   "1001",
		Type:       "github.issue_comment.created",
		Subject:    subject,
		DedupKey:   "del-inprog-2",
		OccurredAt: now,
		ReceivedAt: now,
		Status:     store.TriggerEventStatusPending,
	}))
	res2 := disp.HandleItem(ctx, makeEventItem(ev2, projID), nil)
	assert.True(t, res2.Done)
	require.Len(t, core.firedRequests(), 1)
	assert.Equal(t, store.TriggerActionDispatchAgent, core.firedRequests()[0].ActionType)
}
