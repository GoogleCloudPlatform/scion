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
	"fmt"
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

func TestDispatcher_CoalesceWindow_MidWindowEditToZeroPromotes(t *testing.T) {
	ctx := context.Background()
	entClient := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(entClient)
	projID := seedProjectForDispatch(t, cs)

	now := time.Date(2026, 11, 22, 17, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	tr := makeActiveTrigger(projID, "", "edit-to-zero", []string{"github.pull_request.synchronize"})
	tr.SourceKind = store.TriggerSourceKindGitHub
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
		Subject:    "github:octo/repo#81",
		DedupKey:   "del-edit0-1",
		OccurredAt: now,
		ReceivedAt: now,
		Status:     store.TriggerEventStatusPending,
	}))

	res1 := disp.HandleItem(ctx, makeEventItem(evID, projID), nil)
	assert.False(t, res1.Done)
	assert.Equal(t, now.Add(30*time.Second), res1.RetryAt)

	fires1, err := cs.ListTriggerFires(ctx, store.TriggerFireFilter{TriggerID: tr.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, fires1.Items, 1)
	origFireID := fires1.Items[0].ID
	assert.Equal(t, store.TriggerFireStatusCoalescing, fires1.Items[0].Status)

	// Mid-window PATCH: coalesce_window changed to 0 while trigger stays active and matching.
	tr.CoalesceWindowSeconds = 0
	require.NoError(t, cs.UpdateTrigger(ctx, tr, store.TriggerFieldMask{CoalesceWindowSeconds: true}, 1, true, nil))

	// At retry time, the existing coalescing row is promoted in place and fired (never stranded).
	now = now.Add(30 * time.Second)
	res2 := disp.HandleItem(ctx, makeEventItem(evID, projID), nil)
	assert.True(t, res2.Done)
	assert.Zero(t, res2.RetryAt)
	require.Len(t, core.firedRequests(), 1)

	fires2, err := cs.ListTriggerFires(ctx, store.TriggerFireFilter{TriggerID: tr.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, fires2.Items, 1)
	assert.Equal(t, origFireID, fires2.Items[0].ID)
	assert.Equal(t, store.TriggerFireStatusFired, fires2.Items[0].Status)
}

func TestDispatcher_CoalesceWindow_MidWindowPauseOrRuleMismatchCancels(t *testing.T) {
	ctx := context.Background()
	entClient := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(entClient)
	projID := seedProjectForDispatch(t, cs)

	now := time.Date(2026, 11, 22, 18, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	trPaused := makeActiveTrigger(projID, "", "mid-pause", []string{"github.pull_request.synchronize"})
	trPaused.SourceKind = store.TriggerSourceKindGitHub
	trPaused.CoalesceWindowSeconds = 30
	require.NoError(t, cs.CreateTrigger(ctx, trPaused))

	trMismatch := makeActiveTrigger(projID, "", "mid-mismatch", []string{"github.pull_request.synchronize"})
	trMismatch.SourceKind = store.TriggerSourceKindGitHub
	trMismatch.CoalesceWindowSeconds = 30
	require.NoError(t, cs.CreateTrigger(ctx, trMismatch))

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
		Subject:    "github:octo/repo#82",
		DedupKey:   "del-cancel-1",
		OccurredAt: now,
		ReceivedAt: now,
		Status:     store.TriggerEventStatusPending,
	}))

	res1 := disp.HandleItem(ctx, makeEventItem(evID, projID), nil)
	assert.False(t, res1.Done)
	assert.Equal(t, now.Add(30*time.Second), res1.RetryAt)

	// Pause trPaused and edit trMismatch so it no longer matches the event type.
	trPaused.Status = store.TriggerStatusPaused
	require.NoError(t, cs.UpdateTrigger(ctx, trPaused, store.TriggerFieldMask{Status: true}, 1, true, nil))

	trMismatch.MatchTypes = []string{"github.issues.opened"}
	require.NoError(t, cs.UpdateTrigger(ctx, trMismatch, store.TriggerFieldMask{MatchTypes: true}, 1, true, nil))

	// On retry, both stranded coalescing rows must be finished as skipped "coalesce cancelled"
	// and no longer count as live for the subject.
	now = now.Add(30 * time.Second)
	res2 := disp.HandleItem(ctx, makeEventItem(evID, projID), nil)
	assert.True(t, res2.Done)
	assert.Zero(t, res2.RetryAt)
	assert.Empty(t, core.firedRequests())

	for _, tid := range []string{trPaused.ID, trMismatch.ID} {
		fires, err := cs.ListTriggerFires(ctx, store.TriggerFireFilter{TriggerID: tid, EventID: evID}, store.ListOptions{Limit: 5})
		require.NoError(t, err)
		require.Len(t, fires.Items, 1)
		assert.Equal(t, store.TriggerFireStatusSkipped, fires.Items[0].Status)
		assert.Equal(t, trigger.ErrFireCoalesceCancelled, fires.Items[0].Error)

		live, err := cs.HasLiveTriggerFireForSubjectExcluding(ctx, tid, "github:octo/repo#82", "", true, now)
		require.NoError(t, err)
		assert.False(t, live, "cancelled coalescing row must not stay live for subject")
	}
}

type blockingG4aCore struct {
	g4aTestCore
	entered chan struct{}
	release chan struct{}
}

func (b *blockingG4aCore) Fire(ctx context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-b.release
	return b.g4aTestCore.Fire(ctx, req)
}

func TestDispatcher_CoalesceWindow_LostLeaseRehandlePromotedFiringNoDuplicate(t *testing.T) {
	ctx := context.Background()
	entClient := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(entClient)
	projID := seedProjectForDispatch(t, cs)

	now := time.Date(2026, 11, 22, 19, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	tr := makeActiveTrigger(projID, "", "lost-lease-coalesce", []string{"github.pull_request.synchronize"})
	tr.SourceKind = store.TriggerSourceKindGitHub
	tr.CoalesceWindowSeconds = 30
	require.NoError(t, cs.CreateTrigger(ctx, tr))

	core := &blockingG4aCore{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
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
		Subject:    "github:octo/repo#83",
		DedupKey:   "del-lostlease-1",
		OccurredAt: now,
		ReceivedAt: now,
		Status:     store.TriggerEventStatusPending,
	}))

	// 1. Initial handle creates coalescing row and returns RetryAt.
	res1 := disp.HandleItem(ctx, makeEventItem(evID, projID), nil)
	assert.False(t, res1.Done)
	assert.Equal(t, now.Add(30*time.Second), res1.RetryAt)

	// 2. Advance past window; worker A promotes row to firing and parks inside Fire.
	now = now.Add(30 * time.Second)
	aDone := make(chan durablequeue.Result, 1)
	go func() {
		aDone <- disp.HandleItem(ctx, makeEventItem(evID, projID), nil)
	}()

	select {
	case <-core.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for worker A to enter Fire after promote")
	}

	// Verify row is in status "firing" while worker A is mid-Fire.
	firesMid, err := cs.ListTriggerFires(ctx, store.TriggerFireFilter{TriggerID: tr.ID, EventID: evID}, store.ListOptions{Limit: 5})
	require.NoError(t, err)
	require.Len(t, firesMid.Items, 1)
	assert.Equal(t, store.TriggerFireStatusFiring, firesMid.Items[0].Status)
	origFireID := firesMid.Items[0].ID

	// 3. Simulate lost-lease redelivery to worker B while A is still firing:
	// must NOT duplicate the fire or invoke core.Fire a second time.
	resB := disp.HandleItem(ctx, makeEventItem(evID, projID), nil)
	assert.True(t, resB.Done)
	assert.Zero(t, resB.RetryAt)

	// Release worker A and wait for completion.
	close(core.release)
	resA := <-aDone
	assert.True(t, resA.Done)

	// Exactly one Fire call and one fire row in status "fired".
	require.Len(t, core.firedRequests(), 1, "lost-lease re-handle must not duplicate Fire")
	firesEnd, err := cs.ListTriggerFires(ctx, store.TriggerFireFilter{TriggerID: tr.ID, EventID: evID}, store.ListOptions{Limit: 5})
	require.NoError(t, err)
	require.Len(t, firesEnd.Items, 1)
	assert.Equal(t, origFireID, firesEnd.Items[0].ID)
	assert.Equal(t, store.TriggerFireStatusFired, firesEnd.Items[0].Status)
}

func TestDispatcher_Reuse_DeferInFlightThenDeliver(t *testing.T) {
	ctx := context.Background()
	entClient := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(entClient)
	projID := seedProjectForDispatch(t, cs)

	now := time.Date(2026, 11, 22, 20, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	claims := trigger.NewSubjectClaims(cs, cs, clock)

	tr := makeActiveTrigger(projID, "", "reuse-defer-deliver", []string{"github.issue_comment.created"})
	tr.SourceKind = store.TriggerSourceKindGitHub
	tr.ActionType = store.TriggerActionDispatchAgent
	tr.SubjectMode = store.TriggerSubjectModeReuse
	require.NoError(t, cs.CreateTrigger(ctx, tr))

	ag := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "launching-ag",
		Name:      "launching-ag",
		ProjectID: projID,
		Phase:     "provisioning",
	}
	require.NoError(t, cs.CreateAgent(ctx, ag))

	const subject = "github:octo/repo#91"
	c, _, err := claims.Claim(ctx, tr.ID, subject, uuid.NewString(), uuid.NewString())
	require.NoError(t, err)
	require.NoError(t, claims.Bind(ctx, c, ag.ID))

	core := &g4aTestCore{
		renderFn: func(_ context.Context, req hubfire.FireRequest) (string, *hubfire.FireOutcome, error) {
			return "reuse-body-" + req.Event.ID, nil, nil
		},
	}
	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:          core,
		Store:         cs,
		SubjectClaims: claims,
		Now:           clock,
	})
	require.NoError(t, err)

	evID := uuid.NewString()
	require.NoError(t, cs.CreateTriggerEvent(ctx, &store.TriggerEvent{
		ID:         evID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindGitHub,
		SourceID:   "1001",
		Type:       "github.issue_comment.created",
		Subject:    subject,
		DedupKey:   "del-defer-1",
		OccurredAt: now,
		ReceivedAt: now,
		Status:     store.TriggerEventStatusPending,
	}))

	// 1. Target agent is provisioning -> fire is deferred (non-terminal) with RetryAt.
	res1 := disp.HandleItem(ctx, makeEventItem(evID, projID), nil)
	assert.False(t, res1.Done)
	assert.Equal(t, now.Add(trigger.ReuseDeferBackoff), res1.RetryAt)
	assert.Empty(t, core.firedRequests())

	fires1, err := cs.ListTriggerFires(ctx, store.TriggerFireFilter{TriggerID: tr.ID, EventID: evID}, store.ListOptions{Limit: 5})
	require.NoError(t, err)
	require.Len(t, fires1.Items, 1)
	assert.Equal(t, store.TriggerFireStatusDeferred, fires1.Items[0].Status)
	origFireID := fires1.Items[0].ID

	// Deferred fire counts as live for subjectLive / skip.
	live, err := cs.HasLiveTriggerFireForSubjectExcluding(ctx, tr.ID, subject, "", false, now)
	require.NoError(t, err)
	assert.True(t, live, "deferred fire row must be treated as live for subject")

	// 2. Early re-handle before not_before stays deferred and returns the same RetryAt.
	now = now.Add(3 * time.Second)
	resEarly := disp.HandleItem(ctx, makeEventItem(evID, projID), nil)
	assert.False(t, resEarly.Done)
	assert.Equal(t, res1.RetryAt, resEarly.RetryAt)
	assert.Empty(t, core.firedRequests())

	// 3. Agent reaches running -> re-handle at RetryAt promotes SAME fire and delivers reuse message.
	ag.Phase = "running"
	require.NoError(t, cs.UpdateAgent(ctx, ag))
	now = res1.RetryAt
	res2 := disp.HandleItem(ctx, makeEventItem(evID, projID), nil)
	assert.True(t, res2.Done)
	assert.Zero(t, res2.RetryAt)

	fired := core.firedRequests()
	require.Len(t, fired, 1)
	assert.Equal(t, store.TriggerActionMessage, fired[0].ActionType)
	assert.Contains(t, fired[0].Payload, "reuse-body-"+evID)

	fires2, err := cs.ListTriggerFires(ctx, store.TriggerFireFilter{TriggerID: tr.ID, EventID: evID}, store.ListOptions{Limit: 5})
	require.NoError(t, err)
	require.Len(t, fires2.Items, 1)
	assert.Equal(t, origFireID, fires2.Items[0].ID)
	assert.Equal(t, store.TriggerFireStatusFired, fires2.Items[0].Status)
}

func TestDispatcher_Reuse_DeferInFlightThenClaimReleasedDispatchesNew(t *testing.T) {
	ctx := context.Background()
	entClient := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(entClient)
	projID := seedProjectForDispatch(t, cs)

	now := time.Date(2026, 11, 22, 20, 30, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	claims := trigger.NewSubjectClaims(cs, cs, clock)

	tr := makeActiveTrigger(projID, "", "reuse-defer-release", []string{"github.issue_comment.created"})
	tr.SourceKind = store.TriggerSourceKindGitHub
	tr.ActionType = store.TriggerActionDispatchAgent
	tr.SubjectMode = store.TriggerSubjectModeReuse
	require.NoError(t, cs.CreateTrigger(ctx, tr))

	ag := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "starting-ag",
		Name:      "starting-ag",
		ProjectID: projID,
		Phase:     "starting",
	}
	require.NoError(t, cs.CreateAgent(ctx, ag))

	const subject = "github:octo/repo#92"
	c, _, err := claims.Claim(ctx, tr.ID, subject, uuid.NewString(), uuid.NewString())
	require.NoError(t, err)
	require.NoError(t, claims.Bind(ctx, c, ag.ID))

	core := &g4aTestCore{}
	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:          core,
		Store:         cs,
		SubjectClaims: claims,
		Now:           clock,
	})
	require.NoError(t, err)

	evID := uuid.NewString()
	require.NoError(t, cs.CreateTriggerEvent(ctx, &store.TriggerEvent{
		ID:         evID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindGitHub,
		SourceID:   "1001",
		Type:       "github.issue_comment.created",
		Subject:    subject,
		DedupKey:   "del-defer-rel-1",
		OccurredAt: now,
		ReceivedAt: now,
		Status:     store.TriggerEventStatusPending,
	}))

	// 1. Agent is starting -> deferred.
	res1 := disp.HandleItem(ctx, makeEventItem(evID, projID), nil)
	assert.False(t, res1.Done)
	assert.Equal(t, now.Add(trigger.ReuseDeferBackoff), res1.RetryAt)

	// 2. Launch fails -> agent enters terminal phase "error"; on re-handle,
	// subjectLive lazily releases the claim and dispatches a new agent.
	ag.Phase = "error"
	require.NoError(t, cs.UpdateAgent(ctx, ag))
	now = res1.RetryAt

	res2 := disp.HandleItem(ctx, makeEventItem(evID, projID), nil)
	assert.True(t, res2.Done)
	fired := core.firedRequests()
	require.Len(t, fired, 1)
	assert.Equal(t, store.TriggerActionDispatchAgent, fired[0].ActionType)
}

func TestDispatcher_Reuse_DeferInFlightBoundedTimeout(t *testing.T) {
	ctx := context.Background()
	entClient := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(entClient)
	projID := seedProjectForDispatch(t, cs)

	now := time.Date(2026, 11, 22, 21, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	claims := trigger.NewSubjectClaims(cs, cs, clock)

	tr := makeActiveTrigger(projID, "", "reuse-defer-timeout", []string{"github.issue_comment.created"})
	tr.SourceKind = store.TriggerSourceKindGitHub
	tr.ActionType = store.TriggerActionDispatchAgent
	tr.SubjectMode = store.TriggerSubjectModeReuse
	require.NoError(t, cs.CreateTrigger(ctx, tr))

	ag := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "stuck-ag",
		Name:      "stuck-ag",
		ProjectID: projID,
		Phase:     "cloning",
	}
	require.NoError(t, cs.CreateAgent(ctx, ag))

	const subject = "github:octo/repo#93"
	c, _, err := claims.Claim(ctx, tr.ID, subject, uuid.NewString(), uuid.NewString())
	require.NoError(t, err)
	require.NoError(t, claims.Bind(ctx, c, ag.ID))

	core := &g4aTestCore{}
	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:          core,
		Store:         cs,
		SubjectClaims: claims,
		Now:           clock,
	})
	require.NoError(t, err)

	evID := uuid.NewString()
	require.NoError(t, cs.CreateTriggerEvent(ctx, &store.TriggerEvent{
		ID:         evID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindGitHub,
		SourceID:   "1001",
		Type:       "github.issue_comment.created",
		Subject:    subject,
		DedupKey:   "del-defer-to-1",
		OccurredAt: now,
		ReceivedAt: now,
		Status:     store.TriggerEventStatusPending,
	}))

	// 1. First handling defers.
	res1 := disp.HandleItem(ctx, makeEventItem(evID, projID), nil)
	assert.False(t, res1.Done)
	assert.Equal(t, now.Add(trigger.ReuseDeferBackoff), res1.RetryAt)

	// 2. Advance past the 10-minute total deferral bound while agent is still cloning.
	now = now.Add(trigger.ReuseDeferTimeout)
	res2 := disp.HandleItem(ctx, makeEventItem(evID, projID), nil)
	assert.True(t, res2.Done)
	assert.Zero(t, res2.RetryAt)
	assert.Empty(t, core.firedRequests())

	fires, err := cs.ListTriggerFires(ctx, store.TriggerFireFilter{TriggerID: tr.ID, EventID: evID}, store.ListOptions{Limit: 5})
	require.NoError(t, err)
	require.Len(t, fires.Items, 1)
	assert.Equal(t, store.TriggerFireStatusFailed, fires.Items[0].Status)
	assert.Equal(t, trigger.ErrFireTargetAgentStillStarting, fires.Items[0].Error)
}

func TestDispatcher_Reuse_DeferCombinedWithCoalesce(t *testing.T) {
	ctx := context.Background()
	entClient := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(entClient)
	projID := seedProjectForDispatch(t, cs)

	now := time.Date(2026, 11, 22, 21, 30, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	claims := trigger.NewSubjectClaims(cs, cs, clock)

	tr := makeActiveTrigger(projID, "", "reuse-defer-coalesce", []string{"github.pull_request.synchronize"})
	tr.SourceKind = store.TriggerSourceKindGitHub
	tr.ActionType = store.TriggerActionDispatchAgent
	tr.SubjectMode = store.TriggerSubjectModeReuse
	tr.CoalesceWindowSeconds = 30
	require.NoError(t, cs.CreateTrigger(ctx, tr))

	ag := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "coalesce-launching-ag",
		Name:      "coalesce-launching-ag",
		ProjectID: projID,
		Phase:     "provisioning",
	}
	require.NoError(t, cs.CreateAgent(ctx, ag))

	const subject = "github:octo/repo#94"
	c, _, err := claims.Claim(ctx, tr.ID, subject, uuid.NewString(), uuid.NewString())
	require.NoError(t, err)
	require.NoError(t, claims.Bind(ctx, c, ag.ID))

	core := &g4aTestCore{
		renderFn: func(_ context.Context, req hubfire.FireRequest) (string, *hubfire.FireOutcome, error) {
			return "sync-" + req.Event.ID, nil, nil
		},
	}
	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:          core,
		Store:         cs,
		SubjectClaims: claims,
		Now:           clock,
	})
	require.NoError(t, err)

	mkEv := func(dedup string) string {
		id := uuid.NewString()
		require.NoError(t, cs.CreateTriggerEvent(ctx, &store.TriggerEvent{
			ID:         id,
			ProjectID:  projID,
			SourceKind: store.TriggerSourceKindGitHub,
			SourceID:   "1001",
			Type:       "github.pull_request.synchronize",
			Subject:    subject,
			DedupKey:   dedup,
			OccurredAt: now,
			ReceivedAt: now,
			Status:     store.TriggerEventStatusPending,
		}))
		return id
	}

	// 1. E1 enters coalescing (30s).
	ev1 := mkEv("del-dc-1")
	res1 := disp.HandleItem(ctx, makeEventItem(ev1, projID), nil)
	assert.False(t, res1.Done)
	assert.Equal(t, now.Add(30*time.Second), res1.RetryAt)

	// 2. Coalesce window elapses -> E1 promotes, sees agent still provisioning -> defers 10s.
	now = now.Add(30 * time.Second)
	res1Defer := disp.HandleItem(ctx, makeEventItem(ev1, projID), nil)
	assert.False(t, res1Defer.Done)
	assert.Equal(t, now.Add(trigger.ReuseDeferBackoff), res1Defer.RetryAt)

	// 3. While E1 is deferred, newer E2 arrives -> supersedes E1 ("coalesced") and coalesces 30s.
	now = now.Add(2 * time.Second)
	ev2 := mkEv("del-dc-2")
	res2 := disp.HandleItem(ctx, makeEventItem(ev2, projID), nil)
	assert.False(t, res2.Done)
	assert.Equal(t, now.Add(30*time.Second), res2.RetryAt)

	fires1, err := cs.ListTriggerFires(ctx, store.TriggerFireFilter{TriggerID: tr.ID, EventID: ev1}, store.ListOptions{Limit: 5})
	require.NoError(t, err)
	require.Len(t, fires1.Items, 1)
	assert.Equal(t, store.TriggerFireStatusSkipped, fires1.Items[0].Status)
	assert.Equal(t, trigger.ErrFireCoalesced, fires1.Items[0].Error)

	// 4. Agent finishes launching; E2 promotes at window end and delivers latest reuse message.
	ag.Phase = "running"
	require.NoError(t, cs.UpdateAgent(ctx, ag))
	now = now.Add(30 * time.Second)
	res2Due := disp.HandleItem(ctx, makeEventItem(ev2, projID), nil)
	assert.True(t, res2Due.Done)

	fired := core.firedRequests()
	require.Len(t, fired, 1)
	assert.Equal(t, store.TriggerActionMessage, fired[0].ActionType)
	assert.Contains(t, fired[0].Payload, "sync-"+ev2)
}

func TestDispatcher_CoalesceWindow_TransientSkipPreservesRealReason(t *testing.T) {
	ctx := context.Background()
	entClient := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(entClient)
	projID := seedProjectForDispatch(t, cs)

	now := time.Date(2026, 11, 22, 22, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	tr := makeActiveTrigger(projID, "", "transient-skip-reason", []string{"github.pull_request.synchronize"})
	tr.SourceKind = store.TriggerSourceKindGitHub
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
		Subject:    "github:octo/repo#95",
		DedupKey:   "del-trans-1",
		OccurredAt: now,
		ReceivedAt: now,
		Status:     store.TriggerEventStatusPending,
	}))

	// 1. Initial handle creates coalescing row.
	res1 := disp.HandleItem(ctx, makeEventItem(evID, projID), nil)
	assert.False(t, res1.Done)

	// 2. Add a match rule while dispatcher has no rule engine -> hits eval_failed
	// skip on re-handle and must preserve the real reason ("rule evaluation failed"),
	// NOT overwrite it as "coalesce cancelled".
	tr.MatchRules = []store.TriggerMatchRule{{Dialect: "cel", Expr: "true"}}
	require.NoError(t, cs.UpdateTrigger(ctx, tr, store.TriggerFieldMask{MatchRules: true}, 1, true, nil))

	now = now.Add(30 * time.Second)
	res2 := disp.HandleItem(ctx, makeEventItem(evID, projID), nil)
	assert.True(t, res2.Done)
	assert.Empty(t, core.firedRequests())

	fires, err := cs.ListTriggerFires(ctx, store.TriggerFireFilter{TriggerID: tr.ID, EventID: evID}, store.ListOptions{Limit: 5})
	require.NoError(t, err)
	require.Len(t, fires.Items, 1)
	assert.Equal(t, store.TriggerFireStatusSkipped, fires.Items[0].Status)
	assert.Equal(t, trigger.ErrFireRuleEvaluationFailed, fires.Items[0].Error)
}

func TestDispatcher_Reuse_DeferAfterMaxCoalesceWindowAndBoundFromFirstDeferral(t *testing.T) {
	ctx := context.Background()
	entClient := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(entClient)
	projID := seedProjectForDispatch(t, cs)

	now := time.Date(2026, 11, 22, 22, 10, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	claims := trigger.NewSubjectClaims(cs, cs, clock)

	// Maximum 10-minute coalesce window combined with reuse.
	tr := makeActiveTrigger(projID, "", "max-coalesce-defer", []string{"github.pull_request.synchronize"})
	tr.SourceKind = store.TriggerSourceKindGitHub
	tr.ActionType = store.TriggerActionDispatchAgent
	tr.SubjectMode = store.TriggerSubjectModeReuse
	tr.CoalesceWindowSeconds = store.MaxTriggerCoalesceWindowSeconds // 600s = 10m
	require.NoError(t, cs.CreateTrigger(ctx, tr))

	ag := &store.Agent{
		ID:          uuid.NewString(),
		Slug:        "max-coalesce-ag",
		Name:        "max-coalesce-ag",
		ProjectID:   projID,
		Phase:       "provisioning",
		LaunchState: store.LaunchStateActive,
		LaunchKind:  "create",
	}
	require.NoError(t, cs.CreateAgent(ctx, ag))

	const subject = "github:octo/repo#96"
	c, _, err := claims.Claim(ctx, tr.ID, subject, uuid.NewString(), uuid.NewString())
	require.NoError(t, err)
	require.NoError(t, claims.Bind(ctx, c, ag.ID))

	core := &g4aTestCore{
		renderFn: func(_ context.Context, req hubfire.FireRequest) (string, *hubfire.FireOutcome, error) {
			return "after-max-coalesce-" + req.Event.ID, nil, nil
		},
	}
	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:          core,
		Store:         cs,
		SubjectClaims: claims,
		Now:           clock,
	})
	require.NoError(t, err)

	// Subcase A: 10-minute coalesce window + in-flight agent -> must NOT fail on
	// first in-flight check; defers and delivers once the launch settles.
	ev1 := uuid.NewString()
	require.NoError(t, cs.CreateTriggerEvent(ctx, &store.TriggerEvent{
		ID:         ev1,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindGitHub,
		SourceID:   "1001",
		Type:       "github.pull_request.synchronize",
		Subject:    subject,
		DedupKey:   "del-maxcoal-1",
		OccurredAt: now,
		ReceivedAt: now,
		Status:     store.TriggerEventStatusPending,
	}))

	resCoal := disp.HandleItem(ctx, makeEventItem(ev1, projID), nil)
	assert.False(t, resCoal.Done)
	assert.Equal(t, now.Add(10*time.Minute), resCoal.RetryAt)

	// 10-minute coalesce window elapses -> first in-flight check defers (sets DeferredAt = t0+10m).
	now = now.Add(10 * time.Minute)
	firstDeferTime := now
	resDefer := disp.HandleItem(ctx, makeEventItem(ev1, projID), nil)
	assert.False(t, resDefer.Done, "max coalesce window must not exhaust deferral bound")
	assert.Equal(t, firstDeferTime.Add(trigger.ReuseDeferBackoff), resDefer.RetryAt)

	fires1, err := cs.ListTriggerFires(ctx, store.TriggerFireFilter{TriggerID: tr.ID, EventID: ev1}, store.ListOptions{Limit: 5})
	require.NoError(t, err)
	require.Len(t, fires1.Items, 1)
	assert.Equal(t, store.TriggerFireStatusDeferred, fires1.Items[0].Status)
	require.NotNil(t, fires1.Items[0].DeferredAt)
	assert.Equal(t, firstDeferTime, fires1.Items[0].DeferredAt.UTC())

	// 9 minutes into deferral (19 minutes since StartedAt): still defers!
	now = firstDeferTime.Add(9 * time.Minute)
	resStillDefer := disp.HandleItem(ctx, makeEventItem(ev1, projID), nil)
	assert.False(t, resStillDefer.Done, "9 min after first deferral must still defer")
	assert.Equal(t, now.Add(trigger.ReuseDeferBackoff), resStillDefer.RetryAt)

	// Agent finishes launching -> delivers reuse message!
	ag.Phase = "running"
	ag.LaunchState = ""
	require.NoError(t, cs.UpdateAgent(ctx, ag))
	now = resStillDefer.RetryAt
	resDelivered := disp.HandleItem(ctx, makeEventItem(ev1, projID), nil)
	assert.True(t, resDelivered.Done)
	require.Len(t, core.firedRequests(), 1)
	assert.Contains(t, core.firedRequests()[0].Payload, "after-max-coalesce-"+ev1)

	// Subcase B: bound is counted strictly from first deferral (10 min after DeferredAt,
	// i.e. 20 min after StartedAt when coalesce_window=10m).
	ag.Phase = "provisioning"
	ag.LaunchState = store.LaunchStateActive
	require.NoError(t, cs.UpdateAgent(ctx, ag))

	ev2 := uuid.NewString()
	require.NoError(t, cs.CreateTriggerEvent(ctx, &store.TriggerEvent{
		ID:         ev2,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindGitHub,
		SourceID:   "1001",
		Type:       "github.pull_request.synchronize",
		Subject:    subject,
		DedupKey:   "del-maxcoal-2",
		OccurredAt: now,
		ReceivedAt: now,
		Status:     store.TriggerEventStatusPending,
	}))
	_ = disp.HandleItem(ctx, makeEventItem(ev2, projID), nil) // coalescing (10m)
	now = now.Add(10 * time.Minute)
	res2Defer := disp.HandleItem(ctx, makeEventItem(ev2, projID), nil) // first deferral
	assert.False(t, res2Defer.Done)

	now = now.Add(trigger.ReuseDeferTimeout) // 10m after first deferral (20m after StartedAt)
	res2Timeout := disp.HandleItem(ctx, makeEventItem(ev2, projID), nil)
	assert.True(t, res2Timeout.Done)
	fires2, err := cs.ListTriggerFires(ctx, store.TriggerFireFilter{TriggerID: tr.ID, EventID: ev2}, store.ListOptions{Limit: 5})
	require.NoError(t, err)
	require.Len(t, fires2.Items, 1)
	assert.Equal(t, store.TriggerFireStatusFailed, fires2.Items[0].Status)
	assert.Equal(t, trigger.ErrFireTargetAgentStillStarting, fires2.Items[0].Error)
}

func TestDispatcher_Reuse_DeferReapAndRetryNeverDuplicated(t *testing.T) {
	ctx := context.Background()
	entClient := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(entClient)
	projID := seedProjectForDispatch(t, cs)

	now := time.Date(2026, 11, 22, 22, 30, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	claims := trigger.NewSubjectClaims(cs, cs, clock)

	tr := makeActiveTrigger(projID, "", "defer-reap-retry", []string{"github.issue_comment.created"})
	tr.SourceKind = store.TriggerSourceKindGitHub
	tr.ActionType = store.TriggerActionDispatchAgent
	tr.SubjectMode = store.TriggerSubjectModeReuse
	require.NoError(t, cs.CreateTrigger(ctx, tr))

	ag := &store.Agent{
		ID:          uuid.NewString(),
		Slug:        "reap-defer-ag",
		Name:        "reap-defer-ag",
		ProjectID:   projID,
		Phase:       "starting",
		LaunchState: store.LaunchStateActive,
		LaunchKind:  "create",
	}
	require.NoError(t, cs.CreateAgent(ctx, ag))

	const subject = "github:octo/repo#97"
	c, _, err := claims.Claim(ctx, tr.ID, subject, uuid.NewString(), uuid.NewString())
	require.NoError(t, err)
	require.NoError(t, claims.Bind(ctx, c, ag.ID))

	core := &g4aTestCore{
		renderFn: func(_ context.Context, req hubfire.FireRequest) (string, *hubfire.FireOutcome, error) {
			return "revived-" + req.Event.ID, nil, nil
		},
	}
	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:          core,
		Store:         cs,
		SubjectClaims: claims,
		Now:           clock,
	})
	require.NoError(t, err)

	evID := uuid.NewString()
	require.NoError(t, cs.CreateTriggerEvent(ctx, &store.TriggerEvent{
		ID:         evID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindGitHub,
		SourceID:   "1001",
		Type:       "github.issue_comment.created",
		Subject:    subject,
		DedupKey:   "del-defer-reap-1",
		OccurredAt: now,
		ReceivedAt: now,
		Status:     store.TriggerEventStatusPending,
	}))

	// 1. Defer the fire.
	res1 := disp.HandleItem(ctx, makeEventItem(evID, projID), nil)
	assert.False(t, res1.Done)
	fires1, err := cs.ListTriggerFires(ctx, store.TriggerFireFilter{TriggerID: tr.ID, EventID: evID}, store.ListOptions{Limit: 5})
	require.NoError(t, err)
	require.Len(t, fires1.Items, 1)
	origFireID := fires1.Items[0].ID
	assert.Equal(t, store.TriggerFireStatusDeferred, fires1.Items[0].Status)

	// 2. Hub restarts past lease -> reaper marks deferred row failed "hub restarted during defer".
	reapAt := now.Add(trigger.ReuseDeferBackoff + trigger.FireLeaseHorizon + time.Minute)
	reaped, err := cs.ReapExpiredTriggerFires(ctx, reapAt, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, reaped)

	reapedFire, err := cs.GetTriggerFire(ctx, origFireID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerFireStatusFailed, reapedFire.Status)
	assert.Equal(t, trigger.ErrFireReapedDuringDefer, reapedFire.Error)

	// 3. Agent is now running -> queue retry revives the SAME fire row in place and delivers.
	ag.Phase = "running"
	ag.LaunchState = ""
	require.NoError(t, cs.UpdateAgent(ctx, ag))
	now = reapAt.Add(time.Second)

	res2 := disp.HandleItem(ctx, makeEventItem(evID, projID), nil)
	assert.True(t, res2.Done)
	require.Len(t, core.firedRequests(), 1)
	assert.Contains(t, core.firedRequests()[0].Payload, "revived-"+evID)

	firesAfter, err := cs.ListTriggerFires(ctx, store.TriggerFireFilter{TriggerID: tr.ID, EventID: evID}, store.ListOptions{Limit: 5})
	require.NoError(t, err)
	require.Len(t, firesAfter.Items, 1, "reaped deferred row must be revived in place, never duplicated")
	assert.Equal(t, origFireID, firesAfter.Items[0].ID)
	assert.Equal(t, store.TriggerFireStatusFired, firesAfter.Items[0].Status)
}

func TestDispatcher_Reuse_DeferPausedUsesTruthfulReasonAndIncompleteCreateDefers(t *testing.T) {
	ctx := context.Background()
	entClient := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(entClient)
	projID := seedProjectForDispatch(t, cs)

	now := time.Date(2026, 11, 22, 22, 45, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	claims := trigger.NewSubjectClaims(cs, cs, clock)

	tr := makeActiveTrigger(projID, "", "defer-pause-truthful", []string{"github.issue_comment.created"})
	tr.SourceKind = store.TriggerSourceKindGitHub
	tr.ActionType = store.TriggerActionDispatchAgent
	tr.SubjectMode = store.TriggerSubjectModeReuse
	require.NoError(t, cs.CreateTrigger(ctx, tr))

	// N1: Agent in suspended phase with active create launch (IsIncompleteCreate() == true):
	// launchStartRefusal refuses wake, so reuse must defer rather than failing wake.
	ag := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "incomplete-create-ag",
		Name:      "incomplete-create-ag",
		ProjectID: projID,
		Phase:     "suspended",
	}
	require.NoError(t, cs.CreateAgent(ctx, ag))
	_, err := entClient.Agent.UpdateOneID(uuid.MustParse(ag.ID)).
		SetLaunchKind("create").
		SetLaunchState(store.LaunchStateActive).
		Save(ctx)
	require.NoError(t, err)
	gotAg, err := cs.GetAgent(ctx, ag.ID)
	require.NoError(t, err)
	require.True(t, gotAg.IsIncompleteCreate())

	const subject = "github:octo/repo#98"
	c, _, err := claims.Claim(ctx, tr.ID, subject, uuid.NewString(), uuid.NewString())
	require.NoError(t, err)
	require.NoError(t, claims.Bind(ctx, c, ag.ID))

	core := &g4aTestCore{}
	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:          core,
		Store:         cs,
		SubjectClaims: claims,
		Now:           clock,
	})
	require.NoError(t, err)

	evID := uuid.NewString()
	require.NoError(t, cs.CreateTriggerEvent(ctx, &store.TriggerEvent{
		ID:         evID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindGitHub,
		SourceID:   "1001",
		Type:       "github.issue_comment.created",
		Subject:    subject,
		DedupKey:   "del-defer-pause-1",
		OccurredAt: now,
		ReceivedAt: now,
		Status:     store.TriggerEventStatusPending,
	}))

	// 1. Defers because IsIncompleteCreate() matches launchStartRefusal.
	res1 := disp.HandleItem(ctx, makeEventItem(evID, projID), nil)
	assert.False(t, res1.Done)
	assert.Equal(t, now.Add(trigger.ReuseDeferBackoff), res1.RetryAt)

	// 2. N3: Pause the trigger while the fire is deferred -> retry finishes with
	// truthful reason "defer cancelled", NOT "coalesce cancelled".
	tr.Status = store.TriggerStatusPaused
	require.NoError(t, cs.UpdateTrigger(ctx, tr, store.TriggerFieldMask{Status: true}, 1, true, nil))

	now = now.Add(trigger.ReuseDeferBackoff)
	res2 := disp.HandleItem(ctx, makeEventItem(evID, projID), nil)
	assert.True(t, res2.Done)
	assert.Empty(t, core.firedRequests())

	fires, err := cs.ListTriggerFires(ctx, store.TriggerFireFilter{TriggerID: tr.ID, EventID: evID}, store.ListOptions{Limit: 5})
	require.NoError(t, err)
	require.Len(t, fires.Items, 1)
	assert.Equal(t, store.TriggerFireStatusSkipped, fires.Items[0].Status)
	assert.Equal(t, trigger.ErrFireDeferCancelled, fires.Items[0].Error)
}

func TestDispatcher_Reuse_NonDispatchActionRunsOwnActionDespiteLiveClaim(t *testing.T) {
	ctx := context.Background()
	entClient := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(entClient)
	projID := seedProjectForDispatch(t, cs)

	now := time.Date(2026, 11, 23, 17, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	claims := trigger.NewSubjectClaims(cs, cs, clock)

	// Start as dispatch_agent + reuse and bind a live subject claim to oldAg.
	tr := makeActiveTrigger(projID, "", "reuse-patched-action", []string{"github.issue_comment.created"})
	tr.SourceKind = store.TriggerSourceKindGitHub
	tr.ActionType = store.TriggerActionDispatchAgent
	tr.SubjectMode = store.TriggerSubjectModeReuse
	require.NoError(t, cs.CreateTrigger(ctx, tr))

	oldAg := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "old-claimed-ag",
		Name:      "old-claimed-ag",
		ProjectID: projID,
		Phase:     "suspended",
	}
	require.NoError(t, cs.CreateAgent(ctx, oldAg))

	targetAg := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "message-target-ag",
		Name:      "message-target-ag",
		ProjectID: projID,
		Phase:     "running",
	}
	require.NoError(t, cs.CreateAgent(ctx, targetAg))

	const subject = "github:octo/repo#99"
	c, _, err := claims.Claim(ctx, tr.ID, subject, uuid.NewString(), uuid.NewString())
	require.NoError(t, err)
	require.NoError(t, claims.Bind(ctx, c, oldAg.ID))

	core := &g4aTestCore{}
	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:          core,
		Store:         cs,
		SubjectClaims: claims,
		Now:           clock,
	})
	require.NoError(t, err)

	mkEv := func(dedup string) string {
		id := uuid.NewString()
		require.NoError(t, cs.CreateTriggerEvent(ctx, &store.TriggerEvent{
			ID:         id,
			ProjectID:  projID,
			SourceKind: store.TriggerSourceKindGitHub,
			SourceID:   "1001",
			Type:       "github.issue_comment.created",
			Subject:    subject,
			DedupKey:   dedup,
			OccurredAt: now,
			ReceivedAt: now,
			Status:     store.TriggerEventStatusPending,
		}))
		return id
	}

	// 1. Update trigger from dispatch_agent+reuse to message+reuse while oldAg claim is live.
	msgPayload := fmt.Sprintf(`{"agentId":%q,"message":"hello target"}`, targetAg.ID)
	tr.ActionType = store.TriggerActionMessage
	tr.ActionPayload = msgPayload
	require.NoError(t, cs.UpdateTrigger(ctx, tr, store.TriggerFieldMask{ActionType: true, ActionPayload: true}, 1, true, nil))

	ev1 := mkEv("del-non-dispatch-msg")
	res1 := disp.HandleItem(ctx, makeEventItem(ev1, projID), nil)
	assert.True(t, res1.Done)
	assert.Zero(t, res1.RetryAt)

	fired := core.firedRequests()
	require.Len(t, fired, 1)
	assert.Equal(t, store.TriggerActionMessage, fired[0].ActionType)
	assert.Equal(t, msgPayload, fired[0].Payload)
	assert.NotContains(t, fired[0].Payload, oldAg.ID, "old claimed agent must not be targeted or woken")

	// 2. Update trigger to github.comment+reuse while oldAg claim is still live.
	commentPayload := `{"body":"automated comment"}`
	tr.ActionType = store.TriggerActionGitHubComment
	tr.ActionPayload = commentPayload
	require.NoError(t, cs.UpdateTrigger(ctx, tr, store.TriggerFieldMask{ActionType: true, ActionPayload: true}, 1, true, nil))
	ev2 := mkEv("del-non-dispatch-ghcomment")
	res2 := disp.HandleItem(ctx, makeEventItem(ev2, projID), nil)
	assert.True(t, res2.Done)
	assert.Zero(t, res2.RetryAt)

	fired = core.firedRequests()
	require.Len(t, fired, 2)
	assert.Equal(t, store.TriggerActionGitHubComment, fired[1].ActionType)
	assert.Equal(t, commentPayload, fired[1].Payload)
	assert.NotContains(t, fired[1].Payload, oldAg.ID, "old claimed agent must not be targeted or woken")
}
