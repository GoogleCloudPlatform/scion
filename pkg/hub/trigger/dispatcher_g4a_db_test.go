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
	"sync/atomic"
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

func TestDispatcher_SubjectModeReuse_FreshUnboundClaimDefersAndDelivers(t *testing.T) {
	ctx := context.Background()
	entClient := enttest.NewClient(t)
	t.Logf("dialect=%s", entClient.Driver().Dialect())
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

	mkEv := func(subject, dedup string) string {
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

	// Subcase A: fresh unbound claim -> defers non-terminally with RetryAt,
	// continues deferring while bound agent is in flight, then delivers reuse
	// message once the bound agent is running (never skipped).
	const subjA = "github:octo/repo#55"
	claimA, existing, err := claims.Claim(ctx, tr.ID, 0, subjA, uuid.NewString(), uuid.NewString())
	require.NoError(t, err)
	require.Nil(t, existing)

	ev1 := mkEv(subjA, "del-inprog-a1")
	firstDeferAt := now
	res1 := disp.HandleItem(ctx, makeEventItem(ev1, projID), nil)
	assert.False(t, res1.Done, "fresh unbound claim must defer reuse fire instead of skipping")
	assert.Equal(t, now.Add(trigger.ReuseDeferBackoff), res1.RetryAt)
	assert.Empty(t, core.firedRequests())

	fires1, err := cs.ListTriggerFires(ctx, store.TriggerFireFilter{TriggerID: tr.ID, EventID: ev1}, store.ListOptions{Limit: 5})
	require.NoError(t, err)
	require.Len(t, fires1.Items, 1)
	assert.Equal(t, store.TriggerFireStatusDeferred, fires1.Items[0].Status)
	assert.Equal(t, trigger.ErrFireSubjectDispatchInProgress, fires1.Items[0].Error)
	require.NotNil(t, fires1.Items[0].DeferredAt)
	assert.Equal(t, firstDeferAt, fires1.Items[0].DeferredAt.UTC())
	origFireID := fires1.Items[0].ID

	// Trigger counters must NOT increment while deferred.
	trMid, err := cs.GetTrigger(ctx, tr.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, trMid.FireCount)
	assert.Equal(t, 0, trMid.ErrorCount)

	// Claim binds to an in-flight (provisioning) agent -> continues deferring
	// with ErrFireTargetAgentStillStarting and preserves original DeferredAt.
	agA := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "bound-ag-a",
		Name:      "bound-ag-a",
		ProjectID: projID,
		Phase:     "provisioning",
	}
	require.NoError(t, cs.CreateAgent(ctx, agA))
	require.NoError(t, claims.Bind(ctx, claimA, agA.ID))

	now = res1.RetryAt
	res1InFlight := disp.HandleItem(ctx, makeEventItem(ev1, projID), nil)
	assert.False(t, res1InFlight.Done)
	assert.Equal(t, now.Add(trigger.ReuseDeferBackoff), res1InFlight.RetryAt)
	assert.Empty(t, core.firedRequests())

	fires1Mid, err := cs.ListTriggerFires(ctx, store.TriggerFireFilter{TriggerID: tr.ID, EventID: ev1}, store.ListOptions{Limit: 5})
	require.NoError(t, err)
	require.Len(t, fires1Mid.Items, 1)
	assert.Equal(t, origFireID, fires1Mid.Items[0].ID)
	assert.Equal(t, store.TriggerFireStatusDeferred, fires1Mid.Items[0].Status)
	assert.Equal(t, trigger.ErrFireTargetAgentStillStarting, fires1Mid.Items[0].Error)
	require.NotNil(t, fires1Mid.Items[0].DeferredAt)
	assert.Equal(t, firstDeferAt, fires1Mid.Items[0].DeferredAt.UTC(), "DeferredAt must stay anchored at first deferral")

	// Agent reaches running -> re-handle promotes SAME fire row and delivers reuse message.
	agA.Phase = "running"
	require.NoError(t, cs.UpdateAgent(ctx, agA))
	now = res1InFlight.RetryAt
	res1Delivered := disp.HandleItem(ctx, makeEventItem(ev1, projID), nil)
	assert.True(t, res1Delivered.Done)
	assert.Zero(t, res1Delivered.RetryAt)

	fired := core.firedRequests()
	require.Len(t, fired, 1)
	assert.Equal(t, store.TriggerActionMessage, fired[0].ActionType)
	assert.Contains(t, fired[0].Payload, "reuse-body-"+ev1)
	assert.Contains(t, fired[0].Payload, agA.ID)

	fires1Done, err := cs.ListTriggerFires(ctx, store.TriggerFireFilter{TriggerID: tr.ID, EventID: ev1}, store.ListOptions{Limit: 5})
	require.NoError(t, err)
	require.Len(t, fires1Done.Items, 1)
	assert.Equal(t, origFireID, fires1Done.Items[0].ID)
	assert.Equal(t, store.TriggerFireStatusFired, fires1Done.Items[0].Status)
	assert.Empty(t, fires1Done.Items[0].Error)
	assert.Equal(t, agA.ID, fires1Done.Items[0].TargetAgentID)

	// Subcase B: unbound claim released mid-wait -> re-handle dispatches anew.
	const subjB = "github:octo/repo#56"
	claimB, existingB, err := claims.Claim(ctx, tr.ID, 0, subjB, uuid.NewString(), uuid.NewString())
	require.NoError(t, err)
	require.Nil(t, existingB)

	ev2 := mkEv(subjB, "del-inprog-b1")
	res2Defer := disp.HandleItem(ctx, makeEventItem(ev2, projID), nil)
	assert.False(t, res2Defer.Done)
	assert.Equal(t, now.Add(trigger.ReuseDeferBackoff), res2Defer.RetryAt)

	require.NoError(t, claims.Release(ctx, claimB))
	now = res2Defer.RetryAt
	res2Dispatch := disp.HandleItem(ctx, makeEventItem(ev2, projID), nil)
	assert.True(t, res2Dispatch.Done)

	fired = core.firedRequests()
	require.Len(t, fired, 2)
	assert.Equal(t, store.TriggerActionDispatchAgent, fired[1].ActionType)
	assert.Equal(t, ev2, fired[1].Event.ID)

	// Subcase C: bounded expiry (10 min from first deferral) -> finishes failed with closed reason.
	const subjC = "github:octo/repo#57"
	claimC1, _, err := claims.Claim(ctx, tr.ID, 0, subjC, uuid.NewString(), uuid.NewString())
	require.NoError(t, err)

	ev3 := mkEv(subjC, "del-inprog-c1")
	res3Defer := disp.HandleItem(ctx, makeEventItem(ev3, projID), nil)
	assert.False(t, res3Defer.Done)

	// Refresh the unbound claim partway through so it stays fresh (< 5m) when 10m expires.
	now = now.Add(6 * time.Minute)
	require.NoError(t, claims.Release(ctx, claimC1))
	_, _, err = claims.Claim(ctx, tr.ID, 0, subjC, uuid.NewString(), uuid.NewString())
	require.NoError(t, err)

	now = now.Add(4 * time.Minute) // total 10m since first deferral
	res3Expire := disp.HandleItem(ctx, makeEventItem(ev3, projID), nil)
	assert.True(t, res3Expire.Done)
	assert.Zero(t, res3Expire.RetryAt)
	require.Len(t, core.firedRequests(), 2, "expired deferral must not invoke core.Fire")

	fires3, err := cs.ListTriggerFires(ctx, store.TriggerFireFilter{TriggerID: tr.ID, EventID: ev3}, store.ListOptions{Limit: 5})
	require.NoError(t, err)
	require.Len(t, fires3.Items, 1)
	assert.Equal(t, store.TriggerFireStatusFailed, fires3.Items[0].Status)
	assert.Equal(t, trigger.ErrFireSubjectDispatchInProgress, fires3.Items[0].Error)

	// Subcase D: abandoned unbound claim (> 5 min old) -> falls through to dispatch_agent immediately.
	const subjD = "github:octo/repo#58"
	_, _, err = claims.Claim(ctx, tr.ID, 0, subjD, uuid.NewString(), uuid.NewString())
	require.NoError(t, err)

	now = now.Add(trigger.AbandonedClaimTTL + time.Second)
	ev4 := mkEv(subjD, "del-inprog-d1")
	res4 := disp.HandleItem(ctx, makeEventItem(ev4, projID), nil)
	assert.True(t, res4.Done)
	require.Len(t, core.firedRequests(), 3)
	assert.Equal(t, store.TriggerActionDispatchAgent, core.firedRequests()[2].ActionType)
	assert.Equal(t, ev4, core.firedRequests()[2].Event.ID)

	// Trigger counters: 4 terminal events (ev1 fired, ev2 fired, ev3 failed, ev4 fired) -> FireCount=4, ErrorCount=1.
	trEnd, err := cs.GetTrigger(ctx, tr.ID)
	require.NoError(t, err)
	assert.Equal(t, 4, trEnd.FireCount)
	assert.Equal(t, 1, trEnd.ErrorCount)
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

		live, err := cs.HasLiveTriggerFireForSubjectExcluding(ctx, tid, 0, "github:octo/repo#82", "", true, now)
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
	c, _, err := claims.Claim(ctx, tr.ID, 0, subject, uuid.NewString(), uuid.NewString())
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
	assert.Equal(t, trigger.ErrFireTargetAgentStillStarting, fires1.Items[0].Error)
	origFireID := fires1.Items[0].ID

	// Deferred fire counts as live for subjectLive / skip.
	live, err := cs.HasLiveTriggerFireForSubjectExcluding(ctx, tr.ID, 0, subject, "", false, now)
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
	assert.Empty(t, fires2.Items[0].Error)
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
	c, _, err := claims.Claim(ctx, tr.ID, 0, subject, uuid.NewString(), uuid.NewString())
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
	c, _, err := claims.Claim(ctx, tr.ID, 0, subject, uuid.NewString(), uuid.NewString())
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
	c, _, err := claims.Claim(ctx, tr.ID, 0, subject, uuid.NewString(), uuid.NewString())
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
	c, _, err := claims.Claim(ctx, tr.ID, 0, subject, uuid.NewString(), uuid.NewString())
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
	c, _, err := claims.Claim(ctx, tr.ID, 0, subject, uuid.NewString(), uuid.NewString())
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
	c, _, err := claims.Claim(ctx, tr.ID, 0, subject, uuid.NewString(), uuid.NewString())
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
	c, _, err := claims.Claim(ctx, tr.ID, 0, subject, uuid.NewString(), uuid.NewString())
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

// TestDispatcher_SubjectModeReuse_LostClaimRaceDefersAndDelivers verifies Case 2
// (ptone/scion#9135): two concurrent drainers/replicas handling two events on
// the same subject both pass subjectLive with no claim and race Claim inside
// dispatch_agent. Exactly one agent is created, the loser defers non-terminally
// with RetryAt instead of skipping "subject busy", and on re-handle delivers
// its event as a reuse message to the winner's agent.
func TestDispatcher_SubjectModeReuse_LostClaimRaceDefersAndDelivers(t *testing.T) {
	ctx := context.Background()
	entClient := sharedRaceEntClient(t)
	t.Logf("dialect=%s", entClient.Driver().Dialect())
	cs := entadapter.NewCompositeStore(entClient)
	projID := seedProjectForDispatch(t, cs)

	var clockMu sync.RWMutex
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time {
		clockMu.RLock()
		defer clockMu.RUnlock()
		return now
	}
	advance := func(d time.Duration) {
		clockMu.Lock()
		now = now.Add(d)
		clockMu.Unlock()
	}

	claimsA := trigger.NewSubjectClaims(cs, cs, clock)
	claimsB := trigger.NewSubjectClaims(cs, cs, clock)

	tr := makeActiveTrigger(projID, "", "reuse-claim-race", []string{"github.issue_comment.created"})
	tr.SourceKind = store.TriggerSourceKindGitHub
	tr.ActionType = store.TriggerActionDispatchAgent
	tr.SubjectMode = store.TriggerSubjectModeReuse
	require.NoError(t, cs.CreateTrigger(ctx, tr))

	const subject = "github:octo/repo#9135"
	ev1 := uuid.NewString()
	ev2 := uuid.NewString()
	for _, evID := range []string{ev1, ev2} {
		require.NoError(t, cs.CreateTriggerEvent(ctx, &store.TriggerEvent{
			ID:         evID,
			ProjectID:  projID,
			SourceKind: store.TriggerSourceKindGitHub,
			SourceID:   "1001",
			Type:       "github.issue_comment.created",
			Subject:    subject,
			DedupKey:   "del-race-" + evID,
			OccurredAt: clock(),
			ReceivedAt: clock(),
			Status:     store.TriggerEventStatusPending,
		}))
	}

	// Barrier synchronizes both replicas inside dispatch_agent right before
	// claims.Claim so both observe claim==nil in subjectLive and genuinely race.
	var arrived atomic.Int32
	bothArrived := make(chan struct{})
	var createdAgents atomic.Int32
	var reuseDeliveries atomic.Int32
	var mu sync.Mutex
	var createdAgentID string
	var reuseDeliveredEvents []string

	makeCore := func(cl trigger.SubjectClaims) *g4aTestCore {
		return &g4aTestCore{
			renderFn: func(_ context.Context, req hubfire.FireRequest) (string, *hubfire.FireOutcome, error) {
				return "race-reuse-" + req.Event.ID, nil, nil
			},
			fireFn: func(fctx context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
				if req.ActionType == store.TriggerActionMessage {
					reuseDeliveries.Add(1)
					mu.Lock()
					reuseDeliveredEvents = append(reuseDeliveredEvents, req.Event.ID)
					mu.Unlock()
					return hubfire.FireOutcome{
						Status:    hubfire.FireStatusFired,
						ResultRef: "msg-" + req.Event.ID,
					}
				}
				// Synchronize both dispatch_agent callers before atomic Claim.
				if arrived.Add(1) == 2 {
					close(bothArrived)
				}
				select {
				case <-bothArrived:
				case <-time.After(5 * time.Second):
					return hubfire.FireOutcome{Status: hubfire.FireStatusFailed, Error: "barrier timeout"}
				}

				claim, existing, cErr := cl.Claim(fctx, tr.ID, req.Event.Trigger.ActionIndex, req.Event.Trigger.FireSubject, req.Event.ID, req.Authority.FireID)
				if cErr != nil {
					return hubfire.FireOutcome{Status: hubfire.FireStatusFailed, Error: cErr.Error()}
				}
				if existing != nil {
					// Exact contract of pkg/hub/action_dispatch_agent.go on lost Claim race.
					return hubfire.FireOutcome{
						Status: hubfire.FireStatusSkipped,
						Error:  trigger.ErrFireSubjectBusy,
					}
				}
				ag := &store.Agent{
					ID:        uuid.NewString(),
					Slug:      "race-ag-" + req.Event.ID[:8],
					Name:      "race-ag-" + req.Event.ID[:8],
					ProjectID: projID,
					Phase:     "running",
				}
				if err := cs.CreateAgent(fctx, ag); err != nil {
					_ = cl.Release(fctx, claim)
					return hubfire.FireOutcome{Status: hubfire.FireStatusFailed, Error: err.Error()}
				}
				if err := cl.Bind(fctx, claim, ag.ID); err != nil {
					return hubfire.FireOutcome{Status: hubfire.FireStatusFailed, Error: err.Error()}
				}
				createdAgents.Add(1)
				mu.Lock()
				createdAgentID = ag.ID
				mu.Unlock()
				return hubfire.FireOutcome{
					Status:        hubfire.FireStatusFired,
					ResultRef:     ag.ID,
					TargetAgentID: ag.ID,
				}
			},
		}
	}

	dispA, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:          makeCore(claimsA),
		Store:         cs,
		SubjectClaims: claimsA,
		Now:           clock,
	})
	require.NoError(t, err)
	dispB, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:          makeCore(claimsB),
		Store:         cs,
		SubjectClaims: claimsB,
		Now:           clock,
	})
	require.NoError(t, err)

	var (
		resA, resB durablequeue.Result
		wg         sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		resA = dispA.HandleItem(ctx, makeEventItem(ev1, projID), nil)
	}()
	go func() {
		defer wg.Done()
		resB = dispB.HandleItem(ctx, makeEventItem(ev2, projID), nil)
	}()
	wg.Wait()

	// Exactly one replica won the Claim race (Done==true) and created one agent;
	// the other lost and deferred non-terminally with RetryAt.
	require.Equal(t, int32(1), createdAgents.Load(), "exactly one agent must be created across racing replicas")
	require.NotEqual(t, resA.Done, resB.Done, "one replica must finish fired and the other must defer")

	var (
		loserEvID string
		loserDisp *trigger.Dispatcher
		loserRes  durablequeue.Result
	)
	if !resA.Done {
		loserEvID, loserDisp, loserRes = ev1, dispA, resA
	} else {
		loserEvID, loserDisp, loserRes = ev2, dispB, resB
	}
	assert.Equal(t, clock().Add(trigger.ReuseDeferBackoff), loserRes.RetryAt)

	loserFires, err := cs.ListTriggerFires(ctx, store.TriggerFireFilter{TriggerID: tr.ID, EventID: loserEvID}, store.ListOptions{Limit: 5})
	require.NoError(t, err)
	require.Len(t, loserFires.Items, 1)
	assert.Equal(t, store.TriggerFireStatusDeferred, loserFires.Items[0].Status)
	assert.Equal(t, trigger.ErrFireSubjectDispatchInProgress, loserFires.Items[0].Error)
	loserFireID := loserFires.Items[0].ID

	// Advance clock to RetryAt and re-handle loser -> delivers as reuse message to winner's agent.
	advance(trigger.ReuseDeferBackoff)
	resRetry := loserDisp.HandleItem(ctx, makeEventItem(loserEvID, projID), nil)
	assert.True(t, resRetry.Done)
	assert.Zero(t, resRetry.RetryAt)

	assert.Equal(t, int32(1), createdAgents.Load(), "re-handle must never create a second agent")
	assert.Equal(t, int32(1), reuseDeliveries.Load(), "loser event must be delivered as a reuse message")
	mu.Lock()
	assert.Equal(t, []string{loserEvID}, reuseDeliveredEvents)
	wantAgentID := createdAgentID
	mu.Unlock()

	allFires, err := cs.ListTriggerFires(ctx, store.TriggerFireFilter{TriggerID: tr.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, allFires.Items, 2)
	for _, f := range allFires.Items {
		assert.Equal(t, store.TriggerFireStatusFired, f.Status)
		assert.Empty(t, f.Error)
		assert.Equal(t, wantAgentID, f.TargetAgentID)
		if f.EventID == loserEvID {
			assert.Equal(t, loserFireID, f.ID, "deferred fire row must be promoted in place")
		}
	}

	updatedTr, err := cs.GetTrigger(ctx, tr.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, updatedTr.FireCount)
	assert.Equal(t, 0, updatedTr.ErrorCount)
}

// TestDispatcher_SubjectModeReuse_CrashMidDeferralResumesExactlyOnce verifies
// durable idempotency across crash/restart for both Case 1 (pre-reap concurrent
// lost-lease re-handle) and Case 2 (post-reap revival after hub outage).
func TestDispatcher_SubjectModeReuse_CrashMidDeferralResumesExactlyOnce(t *testing.T) {
	ctx := context.Background()
	entClient := sharedRaceEntClient(t)
	t.Logf("dialect=%s", entClient.Driver().Dialect())
	cs := entadapter.NewCompositeStore(entClient)
	projID := seedProjectForDispatch(t, cs)

	var clockMu sync.RWMutex
	now := time.Date(2026, 11, 28, 13, 0, 0, 0, time.UTC)
	clock := func() time.Time {
		clockMu.RLock()
		defer clockMu.RUnlock()
		return now
	}
	advance := func(d time.Duration) {
		clockMu.Lock()
		now = now.Add(d)
		clockMu.Unlock()
	}
	claims := trigger.NewSubjectClaims(cs, cs, clock)

	tr := makeActiveTrigger(projID, "", "reuse-crash-idempotent", []string{"github.issue_comment.created"})
	tr.SourceKind = store.TriggerSourceKindGitHub
	tr.ActionType = store.TriggerActionDispatchAgent
	tr.SubjectMode = store.TriggerSubjectModeReuse
	require.NoError(t, cs.CreateTrigger(ctx, tr))

	mkEv := func(subject, dedup string) string {
		id := uuid.NewString()
		require.NoError(t, cs.CreateTriggerEvent(ctx, &store.TriggerEvent{
			ID:         id,
			ProjectID:  projID,
			SourceKind: store.TriggerSourceKindGitHub,
			SourceID:   "1001",
			Type:       "github.issue_comment.created",
			Subject:    subject,
			DedupKey:   dedup + "-" + id,
			OccurredAt: clock(),
			ReceivedAt: clock(),
			Status:     store.TriggerEventStatusPending,
		}))
		return id
	}

	// Subcase A: Case 1 deferred -> hub restarts -> two replicas concurrently
	// re-handle the same event at RetryAt -> reuse message delivered exactly once.
	const subjA = "github:octo/repo#9135-crash-a"
	claimA, _, err := claims.Claim(ctx, tr.ID, 0, subjA, uuid.NewString(), uuid.NewString())
	require.NoError(t, err)

	evA := mkEv(subjA, "del-crash-a")
	dispInitial, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:          &g4aTestCore{},
		Store:         cs,
		SubjectClaims: claims,
		Now:           clock,
	})
	require.NoError(t, err)
	resDeferA := dispInitial.HandleItem(ctx, makeEventItem(evA, projID), nil)
	assert.False(t, resDeferA.Done)

	// Bind claim to running agent and advance to RetryAt.
	agA := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "crash-ag-a",
		Name:      "crash-ag-a",
		ProjectID: projID,
		Phase:     "running",
	}
	require.NoError(t, cs.CreateAgent(ctx, agA))
	require.NoError(t, claims.Bind(ctx, claimA, agA.ID))
	advance(trigger.ReuseDeferBackoff)

	var deliveriesA atomic.Int32
	newRestartDisp := func() *trigger.Dispatcher {
		core := &g4aTestCore{
			renderFn: func(_ context.Context, req hubfire.FireRequest) (string, *hubfire.FireOutcome, error) {
				return "crash-resume-" + req.Event.ID, nil, nil
			},
			fireFn: func(_ context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
				if req.ActionType == store.TriggerActionMessage {
					deliveriesA.Add(1)
				}
				return hubfire.FireOutcome{Status: hubfire.FireStatusFired, ResultRef: "msg-" + req.Authority.FireID}
			},
		}
		d, dErr := trigger.NewDispatcher(trigger.DispatcherConfig{
			Core:          core,
			Store:         cs,
			SubjectClaims: claims,
			Now:           clock,
		})
		require.NoError(t, dErr)
		return d
	}

	dispR1 := newRestartDisp()
	dispR2 := newRestartDisp()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = dispR1.HandleItem(ctx, makeEventItem(evA, projID), nil)
	}()
	go func() {
		defer wg.Done()
		_ = dispR2.HandleItem(ctx, makeEventItem(evA, projID), nil)
	}()
	wg.Wait()

	assert.Equal(t, int32(1), deliveriesA.Load(), "concurrent crash-recovery re-handles must deliver reuse message exactly once")
	firesA, err := cs.ListTriggerFires(ctx, store.TriggerFireFilter{TriggerID: tr.ID, EventID: evA}, store.ListOptions{Limit: 5})
	require.NoError(t, err)
	require.Len(t, firesA.Items, 1)
	assert.Equal(t, store.TriggerFireStatusFired, firesA.Items[0].Status)
	assert.Empty(t, firesA.Items[0].Error)

	// Subcase B: Case 2 deferred -> hub down past lease -> maintenance reaps
	// ("hub restarted during defer") -> claim binds -> fresh dispatcher revives
	// SAME fire row and delivers reuse message exactly once.
	const subjB = "github:octo/repo#9135-crash-b"
	evB := mkEv(subjB, "del-crash-b")
	lostRaceCore := &g4aTestCore{
		fireFn: func(_ context.Context, _ hubfire.FireRequest) hubfire.FireOutcome {
			return hubfire.FireOutcome{Status: hubfire.FireStatusSkipped, Error: trigger.ErrFireSubjectBusy}
		},
	}
	dispLost, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:          lostRaceCore,
		Store:         cs,
		SubjectClaims: claims,
		Now:           clock,
	})
	require.NoError(t, err)
	resDeferB := dispLost.HandleItem(ctx, makeEventItem(evB, projID), nil)
	assert.False(t, resDeferB.Done)

	firesBBefore, err := cs.ListTriggerFires(ctx, store.TriggerFireFilter{TriggerID: tr.ID, EventID: evB}, store.ListOptions{Limit: 5})
	require.NoError(t, err)
	require.Len(t, firesBBefore.Items, 1)
	origFireIDB := firesBBefore.Items[0].ID

	// Reap expired deferred fire during hub outage, verifying this iteration's fire row.
	advance(trigger.ReuseDeferBackoff + trigger.FireLeaseHorizon + time.Minute)
	reaped, err := cs.ReapExpiredTriggerFires(ctx, clock(), 100)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, reaped, 1)
	reapedB, err := cs.GetTriggerFire(ctx, origFireIDB)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerFireStatusFailed, reapedB.Status)
	assert.Equal(t, trigger.ErrFireReapedDuringDefer, reapedB.Error)

	// Winner's claim is now bound to a running agent.
	agB := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "crash-ag-b",
		Name:      "crash-ag-b",
		ProjectID: projID,
		Phase:     "running",
	}
	require.NoError(t, cs.CreateAgent(ctx, agB))
	claimB, _, err := claims.Claim(ctx, tr.ID, 0, subjB, uuid.NewString(), uuid.NewString())
	require.NoError(t, err)
	require.NoError(t, claims.Bind(ctx, claimB, agB.ID))

	var deliveriesB atomic.Int32
	reviveCore := &g4aTestCore{
		renderFn: func(_ context.Context, req hubfire.FireRequest) (string, *hubfire.FireOutcome, error) {
			return "revived-case2-" + req.Event.ID, nil, nil
		},
		fireFn: func(_ context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
			if req.ActionType == store.TriggerActionMessage {
				deliveriesB.Add(1)
			}
			return hubfire.FireOutcome{Status: hubfire.FireStatusFired, ResultRef: "msg-" + req.Authority.FireID}
		},
	}
	dispRevive, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:          reviveCore,
		Store:         cs,
		SubjectClaims: claims,
		Now:           clock,
	})
	require.NoError(t, err)

	resRevive := dispRevive.HandleItem(ctx, makeEventItem(evB, projID), nil)
	assert.True(t, resRevive.Done)
	assert.Equal(t, int32(1), deliveriesB.Load())

	// Second re-handle after completion is a no-op (event already processed).
	resAgain := dispRevive.HandleItem(ctx, makeEventItem(evB, projID), nil)
	assert.True(t, resAgain.Done)
	assert.Equal(t, int32(1), deliveriesB.Load())

	firesBAfter, err := cs.ListTriggerFires(ctx, store.TriggerFireFilter{TriggerID: tr.ID, EventID: evB}, store.ListOptions{Limit: 5})
	require.NoError(t, err)
	require.Len(t, firesBAfter.Items, 1)
	assert.Equal(t, origFireIDB, firesBAfter.Items[0].ID)
	assert.Equal(t, store.TriggerFireStatusFired, firesBAfter.Items[0].Status)
	assert.Empty(t, firesBAfter.Items[0].Error)
}

// TestDispatcher_SubjectModeReuse_MultiActionP9DeferralDoesNotBlockSibling
// verifies the P9 multi-action rule: subject claims and reuse deferrals are
// scoped per (trigger, action_index, subject), so a deferred dispatch_agent at
// action_index=0 never blocks a message sibling (action_index=1) or an
// independent dispatch_agent sibling (action_index=2), and re-handling delivers
// only the deferred row without double-firing siblings or double-counting.
func TestDispatcher_SubjectModeReuse_MultiActionP9DeferralDoesNotBlockSibling(t *testing.T) {
	ctx := context.Background()
	entClient := enttest.NewClient(t)
	t.Logf("dialect=%s", entClient.Driver().Dialect())
	cs := entadapter.NewCompositeStore(entClient)
	projID := seedProjectForDispatch(t, cs)

	now := time.Date(2026, 11, 28, 14, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	claims := trigger.NewSubjectClaims(cs, cs, clock)

	tr := makeActiveTrigger(projID, "", "reuse-p9-multi", []string{"github.issue_comment.created"})
	tr.SourceKind = store.TriggerSourceKindGitHub
	tr.SubjectMode = store.TriggerSubjectModeReuse
	tr.ActionType = store.TriggerActionDispatchAgent
	tr.ActionPayload = `{"template":"dev","task":"work-0","reuseMessage":"reused-0"}`
	tr.Actions = []store.TriggerAction{
		{Type: store.TriggerActionDispatchAgent, Payload: `{"template":"dev","task":"work-0","reuseMessage":"reused-0"}`},
		{Type: store.TriggerActionMessage, Payload: `{"agentId":"observer-agent","message":"sibling notification"}`},
		{Type: store.TriggerActionDispatchAgent, Payload: `{"template":"dev","task":"work-2","reuseMessage":"reused-2"}`},
	}
	require.NoError(t, cs.CreateTrigger(ctx, tr))

	// Seed a fresh unbound claim ONLY on action_index=0.
	const subject = "github:octo/repo#9135-p9"
	claim0, existing, err := claims.Claim(ctx, tr.ID, 0, subject, uuid.NewString(), uuid.NewString())
	require.NoError(t, err)
	require.Nil(t, existing)

	core := &g4aTestCore{
		renderFn: func(_ context.Context, req hubfire.FireRequest) (string, *hubfire.FireOutcome, error) {
			return fmt.Sprintf("reused-idx%d-%s", req.Event.Trigger.ActionIndex, req.Event.ID), nil, nil
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
		DedupKey:   "del-p9-1",
		OccurredAt: now,
		ReceivedAt: now,
		Status:     store.TriggerEventStatusPending,
	}))

	// Pass 1: action 0 defers; action 1 (message) and action 2 (dispatch_agent at idx 2) run immediately!
	res1 := disp.HandleItem(ctx, makeEventItem(evID, projID), nil)
	assert.False(t, res1.Done)
	assert.Equal(t, now.Add(trigger.ReuseDeferBackoff), res1.RetryAt)

	fired1 := core.firedRequests()
	require.Len(t, fired1, 2, "siblings at action_index 1 and 2 must run immediately without waiting for action_index 0")
	assert.Equal(t, 1, fired1[0].Event.Trigger.ActionIndex)
	assert.Equal(t, store.TriggerActionMessage, fired1[0].ActionType)
	assert.Equal(t, 2, fired1[1].Event.Trigger.ActionIndex)
	assert.Equal(t, store.TriggerActionDispatchAgent, fired1[1].ActionType)

	fires1, err := cs.ListTriggerFires(ctx, store.TriggerFireFilter{TriggerID: tr.ID, EventID: evID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, fires1.Items, 3)
	byIdx1 := map[int]store.TriggerFire{}
	for _, f := range fires1.Items {
		byIdx1[f.ActionIndex] = f
	}
	assert.Equal(t, store.TriggerFireStatusDeferred, byIdx1[0].Status)
	assert.Equal(t, trigger.ErrFireSubjectDispatchInProgress, byIdx1[0].Error)
	assert.Equal(t, store.TriggerFireStatusFired, byIdx1[1].Status)
	assert.Equal(t, store.TriggerFireStatusFired, byIdx1[2].Status)

	// Bind action 0's claim to a running agent and advance to RetryAt.
	ag0 := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "p9-ag-0",
		Name:      "p9-ag-0",
		ProjectID: projID,
		Phase:     "running",
	}
	require.NoError(t, cs.CreateAgent(ctx, ag0))
	require.NoError(t, claims.Bind(ctx, claim0, ag0.ID))
	now = res1.RetryAt

	// Pass 2: action 0 promotes and delivers reuse message; siblings 1 and 2 are NOT re-fired.
	res2 := disp.HandleItem(ctx, makeEventItem(evID, projID), nil)
	assert.True(t, res2.Done)
	assert.Zero(t, res2.RetryAt)

	fired2 := core.firedRequests()
	require.Len(t, fired2, 3, "only deferred action 0 should fire on pass 2")
	assert.Equal(t, 0, fired2[2].Event.Trigger.ActionIndex)
	assert.Equal(t, store.TriggerActionMessage, fired2[2].ActionType)
	assert.Contains(t, fired2[2].Payload, "reused-idx0-"+evID)
	assert.Contains(t, fired2[2].Payload, ag0.ID)

	fires2, err := cs.ListTriggerFires(ctx, store.TriggerFireFilter{TriggerID: tr.ID, EventID: evID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, fires2.Items, 3)
	for _, f := range fires2.Items {
		assert.Equal(t, store.TriggerFireStatusFired, f.Status)
		assert.Empty(t, f.Error)
	}

	// Trigger FireCount must be 1 for the single matched event (not double-counted across passes).
	updatedTr, err := cs.GetTrigger(ctx, tr.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, updatedTr.FireCount)
	assert.Equal(t, 0, updatedTr.ErrorCount)
}

// TestDispatcher_SubjectModeReuse_PromoteDeferredClearsError pins that
// promoting a deferred fire row back to firing clears its persisted deferral
// reason while in flight (killing the ClearError mutant in
// ClaimTriggerFireCoalescing).
func TestDispatcher_SubjectModeReuse_PromoteDeferredClearsError(t *testing.T) {
	ctx := context.Background()
	entClient := enttest.NewClient(t)
	t.Logf("dialect=%s", entClient.Driver().Dialect())
	cs := entadapter.NewCompositeStore(entClient)
	projID := seedProjectForDispatch(t, cs)

	now := time.Date(2026, 11, 28, 17, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	claims := trigger.NewSubjectClaims(cs, cs, clock)

	tr := makeActiveTrigger(projID, "", "reuse-promote-clears-err", []string{"github.issue_comment.created"})
	tr.SourceKind = store.TriggerSourceKindGitHub
	tr.ActionType = store.TriggerActionDispatchAgent
	tr.SubjectMode = store.TriggerSubjectModeReuse
	require.NoError(t, cs.CreateTrigger(ctx, tr))

	const subject = "github:octo/repo#9135-clear-err"
	claim, existing, err := claims.Claim(ctx, tr.ID, 0, subject, uuid.NewString(), uuid.NewString())
	require.NoError(t, err)
	require.Nil(t, existing)

	evID := uuid.NewString()
	require.NoError(t, cs.CreateTriggerEvent(ctx, &store.TriggerEvent{
		ID:         evID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindGitHub,
		SourceID:   "1001",
		Type:       "github.issue_comment.created",
		Subject:    subject,
		DedupKey:   "del-clear-err-" + evID,
		OccurredAt: now,
		ReceivedAt: now,
		Status:     store.TriggerEventStatusPending,
	}))

	var (
		observedMidFireStatus string
		observedMidFireErr    string
		origFireID            string
	)
	core := &g4aTestCore{
		fireFn: func(fctx context.Context, _ hubfire.FireRequest) hubfire.FireOutcome {
			// Inspect persisted row while status == firing (before FinishTriggerFire).
			mid, gErr := cs.GetTriggerFire(fctx, origFireID)
			if gErr == nil && mid != nil {
				observedMidFireStatus = mid.Status
				observedMidFireErr = mid.Error
			}
			return hubfire.FireOutcome{Status: hubfire.FireStatusFired, ResultRef: "msg-ok"}
		},
	}
	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:          core,
		Store:         cs,
		SubjectClaims: claims,
		Now:           clock,
	})
	require.NoError(t, err)

	// 1. Defer with ErrFireSubjectDispatchInProgress.
	res1 := disp.HandleItem(ctx, makeEventItem(evID, projID), nil)
	require.False(t, res1.Done)
	fires1, err := cs.ListTriggerFires(ctx, store.TriggerFireFilter{TriggerID: tr.ID, EventID: evID}, store.ListOptions{Limit: 5})
	require.NoError(t, err)
	require.Len(t, fires1.Items, 1)
	origFireID = fires1.Items[0].ID
	require.Equal(t, store.TriggerFireStatusDeferred, fires1.Items[0].Status)
	require.Equal(t, trigger.ErrFireSubjectDispatchInProgress, fires1.Items[0].Error)

	// 2. Bind claim to running agent and promote at RetryAt.
	ag := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "clear-err-ag",
		Name:      "clear-err-ag",
		ProjectID: projID,
		Phase:     "running",
	}
	require.NoError(t, cs.CreateAgent(ctx, ag))
	require.NoError(t, claims.Bind(ctx, claim, ag.ID))
	now = res1.RetryAt

	res2 := disp.HandleItem(ctx, makeEventItem(evID, projID), nil)
	require.True(t, res2.Done)
	assert.Equal(t, store.TriggerFireStatusFiring, observedMidFireStatus, "row must be in firing status during Fire")
	assert.Empty(t, observedMidFireErr, "deferred->firing promotion must clear persisted deferral reason")
}
