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

package trigger_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/triggerrule"
)

type enrichTestCore struct {
	mu           sync.Mutex
	order        []string
	authorized   bool
	authErr      error
	authCalls    int
	needsCalls   int
	enrichCalls  int
	enrichFn     func(ctx context.Context, ev *hubfire.FireEvent) (map[string]string, map[string][]string, error)
	firedEvents  []*hubfire.FireEvent
	experiments  map[string]bool
	storeCheckFn func()
}

func (c *enrichTestCore) Fire(_ context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
	c.mu.Lock()
	c.order = append(c.order, "fire")
	if req.Event != nil {
		cp := *req.Event
		cp.Attributes = maps.Clone(req.Event.Attributes)
		c.firedEvents = append(c.firedEvents, &cp)
	}
	c.mu.Unlock()
	if c.storeCheckFn != nil {
		c.storeCheckFn()
	}
	return hubfire.FireOutcome{Status: store.TriggerFireStatusFired}
}

func (c *enrichTestCore) Experiment(name string) bool {
	if c.experiments == nil {
		return true
	}
	return c.experiments[name]
}

func (c *enrichTestCore) AuthorizeEventSource(_ context.Context, _ *store.TriggerEvent) (bool, error) {
	c.mu.Lock()
	c.authCalls++
	c.order = append(c.order, "authorize")
	auth, err := c.authorized, c.authErr
	c.mu.Unlock()
	return auth, err
}

func (c *enrichTestCore) NeedsEnrichment(ev *hubfire.FireEvent) bool {
	c.mu.Lock()
	c.needsCalls++
	c.mu.Unlock()
	if ev == nil || ev.SourceKind != store.TriggerSourceKindGitHub {
		return false
	}
	if ev.Attributes["event"] != "issue_comment" || ev.Attributes["is_pull_request"] != "true" {
		return false
	}
	_, already := ev.Attributes["enriched"]
	return !already
}

func (c *enrichTestCore) Enrich(ctx context.Context, ev *hubfire.FireEvent) (map[string]string, map[string][]string, error) {
	c.mu.Lock()
	c.enrichCalls++
	c.order = append(c.order, "enrich")
	fn := c.enrichFn
	c.mu.Unlock()
	if fn != nil {
		return fn(ctx, ev)
	}
	return nil, nil, errors.New("not configured")
}

func makeEventQueueItem(t *testing.T, eventID string) durablequeue.Item {
	t.Helper()
	raw, err := json.Marshal(trigger.EventQueuePayload{
		Version: trigger.EventQueuePayloadVersion,
		EventID: eventID,
	})
	require.NoError(t, err)
	return durablequeue.Item{
		Kind:     trigger.EventQueueKind,
		DedupKey: eventID,
		Payload:  raw,
	}
}

func newPRCommentEvent(projectID string) *store.TriggerEvent {
	now := time.Now().UTC()
	return &store.TriggerEvent{
		ID:         uuid.NewString(),
		ProjectID:  projectID,
		SourceKind: store.TriggerSourceKindGitHub,
		SourceID:   "987654",
		Type:       "github.issue_comment.created",
		Subject:    "github:octo-org/octo-repo#42",
		DedupKey:   uuid.NewString() + "/" + projectID,
		OccurredAt: now,
		ReceivedAt: now,
		Attributes: map[string]string{
			"provider":           "github",
			"repo":               "octo-org/octo-repo",
			"event":              "issue_comment",
			"action":             "created",
			"is_pull_request":    "true",
			"number":             "42",
			"command":            "review",
			"author_association": "OWNER",
			"sender_type":        "User",
		},
		Lists: map[string][]string{
			"labels": {"old"},
		},
		Status: store.TriggerEventStatusPending,
	}
}

func TestDispatcherEnrichment_HappyPathOrderingPersistenceAndListsCEL(t *testing.T) {
	ctx := context.Background()
	st := newMemTriggerStore()
	rules, err := triggerrule.NewDefault()
	require.NoError(t, err)

	projectID := uuid.NewString()
	ev := newPRCommentEvent(projectID)
	require.NoError(t, st.CreateTriggerEvent(ctx, ev))

	// Preset-style fork guard + event.lists.labels.exists rule.
	tr := &store.Trigger{
		ID:         uuid.NewString(),
		ProjectID:  projectID,
		Name:       "pr-review-cmd",
		SourceKind: store.TriggerSourceKindGitHub,
		SourceID:   "",
		MatchTypes: []string{"github.issue_comment.created"},
		MatchRules: []store.TriggerMatchRule{
			{Dialect: "cel", Expr: "event.attributes.command == 'review' && event.attributes.is_fork == 'false'"},
			{Dialect: "cel", Expr: "event.lists.labels.exists(l, l in ['needs-review', 'urgent'])"},
		},
		ActionType:    store.TriggerActionMessage,
		ActionPayload: `{"agent_id":"a1","message":"review"}`,
		Status:        store.TriggerStatusActive,
	}
	require.NoError(t, st.CreateTrigger(ctx, tr))

	core := &enrichTestCore{
		authorized: true,
		enrichFn: func(_ context.Context, ev *hubfire.FireEvent) (map[string]string, map[string][]string, error) {
			attrs := maps.Clone(ev.Attributes)
			attrs["base_ref"] = "main"
			attrs["base_sha"] = "base111"
			attrs["head_ref"] = "feat/x"
			attrs["head_sha"] = "head222"
			attrs["head_repo"] = "octo-org/octo-repo"
			attrs["is_fork"] = "false"
			attrs["draft"] = "false"
			attrs["merged"] = "false"
			attrs["state"] = "open"
			attrs["enriched"] = "true"
			lists := map[string][]string{
				"labels":              {"needs-review"},
				"requested_reviewers": {"alice"},
				"enriched":            {"base_ref", "base_sha", "head_ref", "head_sha", "head_repo", "is_fork", "draft", "merged", "state"},
			}
			return attrs, lists, nil
		},
	}
	// Verify DB row was updated BEFORE Fire runs.
	var storedBeforeFire *store.TriggerEvent
	core.storeCheckFn = func() {
		storedBeforeFire, _ = st.GetTriggerEvent(ctx, ev.ID)
	}

	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:  core,
		Store: st,
		Rules: rules,
	})
	require.NoError(t, err)

	res := disp.HandleItem(ctx, makeEventQueueItem(t, ev.ID), nil)
	require.True(t, res.Done, "err=%v", res.Err)

	assert.Equal(t, []string{"authorize", "enrich", "fire"}, core.order)
	assert.Equal(t, 1, core.authCalls)
	assert.Equal(t, 1, core.enrichCalls)

	require.NotNil(t, storedBeforeFire)
	assert.Equal(t, "true", storedBeforeFire.Attributes["enriched"])
	assert.Equal(t, "false", storedBeforeFire.Attributes["is_fork"])
	assert.Equal(t, "feat/x", storedBeforeFire.Attributes["head_ref"])
	assert.Equal(t, []string{"needs-review"}, storedBeforeFire.Lists["labels"])
	assert.Contains(t, storedBeforeFire.Lists["enriched"], "is_fork")

	require.Len(t, core.firedEvents, 1)
	assert.Equal(t, "true", core.firedEvents[0].Attributes["enriched"])
	assert.Equal(t, "feat/x", core.firedEvents[0].Attributes["head_ref"])
	assert.Equal(t, []string{"needs-review"}, core.firedEvents[0].Lists["labels"])
}

func TestDispatcherEnrichment_FailureLogsOnceAndRulesStillEvaluateFailClosed(t *testing.T) {
	ctx := context.Background()
	st := newMemTriggerStore()
	rules, err := triggerrule.NewDefault()
	require.NoError(t, err)

	projectID := uuid.NewString()
	ev := newPRCommentEvent(projectID)
	require.NoError(t, st.CreateTriggerEvent(ctx, ev))

	// Trigger 1: preset-style fork guard (must fail closed -> skipped when is_fork absent).
	trGuard := &store.Trigger{
		ID:            uuid.NewString(),
		ProjectID:     projectID,
		Name:          "fork-guarded",
		SourceKind:    store.TriggerSourceKindGitHub,
		SourceID:      "",
		MatchTypes:    []string{"github.issue_comment.created"},
		MatchRules:    []store.TriggerMatchRule{{Dialect: "cel", Expr: "event.attributes.is_fork == 'false'"}},
		ActionType:    store.TriggerActionMessage,
		ActionPayload: `{"agent_id":"a1","message":"guarded"}`,
		Status:        store.TriggerStatusActive,
	}
	// Trigger 2: Types-only (must still fire despite enrichment failure).
	trTypesOnly := &store.Trigger{
		ID:            uuid.NewString(),
		ProjectID:     projectID,
		Name:          "types-only",
		SourceKind:    store.TriggerSourceKindGitHub,
		SourceID:      "",
		MatchTypes:    []string{"github.issue_comment.created"},
		ActionType:    store.TriggerActionMessage,
		ActionPayload: `{"agent_id":"a2","message":"fallback"}`,
		Status:        store.TriggerStatusActive,
	}
	require.NoError(t, st.CreateTrigger(ctx, trGuard))
	require.NoError(t, st.CreateTrigger(ctx, trTypesOnly))

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	core := &enrichTestCore{
		authorized: true,
		enrichFn: func(_ context.Context, ev *hubfire.FireEvent) (map[string]string, map[string][]string, error) {
			attrs := maps.Clone(ev.Attributes)
			attrs["enriched"] = "failed"
			return attrs, ev.Lists, errors.New("github enrichment: pull_request_request_failed")
		},
	}

	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:   core,
		Store:  st,
		Rules:  rules,
		Logger: logger,
	})
	require.NoError(t, err)

	res := disp.HandleItem(ctx, makeEventQueueItem(t, ev.ID), nil)
	require.True(t, res.Done, "err=%v", res.Err)

	assert.Equal(t, 1, core.enrichCalls)
	assert.Contains(t, logBuf.String(), "source enrichment failed")

	stored, err := st.GetTriggerEvent(ctx, ev.ID)
	require.NoError(t, err)
	assert.Equal(t, "failed", stored.Attributes["enriched"])
	assert.Empty(t, stored.Attributes["is_fork"])

	// Types-only fired; fork-guarded skipped.
	require.Len(t, core.firedEvents, 1)
	assert.Equal(t, "failed", core.firedEvents[0].Attributes["enriched"])

	fires, err := st.ListTriggerFires(ctx, store.TriggerFireFilter{EventID: ev.ID}, store.ListOptions{})
	require.NoError(t, err)
	statusByTrigger := map[string]string{}
	for _, f := range fires.Items {
		statusByTrigger[f.TriggerID] = f.Status
	}
	assert.Equal(t, store.TriggerFireStatusSkipped, statusByTrigger[trGuard.ID])
	assert.Equal(t, store.TriggerFireStatusFired, statusByTrigger[trTypesOnly.ID])
}

func TestDispatcherEnrichment_NoCandidateOrUnauthorizedMakesZeroEnrichCalls(t *testing.T) {
	ctx := context.Background()
	rules, err := triggerrule.NewDefault()
	require.NoError(t, err)

	t.Run("no candidate matching match_types", func(t *testing.T) {
		st := newMemTriggerStore()
		projectID := uuid.NewString()
		ev := newPRCommentEvent(projectID)
		require.NoError(t, st.CreateTriggerEvent(ctx, ev))

		// Active trigger only watches pull_request.opened, not issue_comment.created.
		require.NoError(t, st.CreateTrigger(ctx, &store.Trigger{
			ID:            uuid.NewString(),
			ProjectID:     projectID,
			Name:          "pr-opened-only",
			SourceKind:    store.TriggerSourceKindGitHub,
			SourceID:      "",
			MatchTypes:    []string{"github.pull_request.opened"},
			ActionType:    store.TriggerActionMessage,
			ActionPayload: `{"agent_id":"a1","message":"opened"}`,
			Status:        store.TriggerStatusActive,
		}))

		core := &enrichTestCore{authorized: true}
		disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{Core: core, Store: st, Rules: rules})
		require.NoError(t, err)

		res := disp.HandleItem(ctx, makeEventQueueItem(t, ev.ID), nil)
		require.True(t, res.Done)
		assert.Zero(t, core.authCalls)
		assert.Zero(t, core.needsCalls)
		assert.Zero(t, core.enrichCalls)
	})

	t.Run("unauthorized association refuses without enriching", func(t *testing.T) {
		st := newMemTriggerStore()
		projectID := uuid.NewString()
		ev := newPRCommentEvent(projectID)
		require.NoError(t, st.CreateTriggerEvent(ctx, ev))

		require.NoError(t, st.CreateTrigger(ctx, &store.Trigger{
			ID:            uuid.NewString(),
			ProjectID:     projectID,
			Name:          "pr-comment",
			SourceKind:    store.TriggerSourceKindGitHub,
			SourceID:      "",
			MatchTypes:    []string{"github.issue_comment.created"},
			ActionType:    store.TriggerActionMessage,
			ActionPayload: `{"agent_id":"a1","message":"hi"}`,
			Status:        store.TriggerStatusActive,
		}))

		core := &enrichTestCore{authorized: false}
		disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{Core: core, Store: st, Rules: rules})
		require.NoError(t, err)

		res := disp.HandleItem(ctx, makeEventQueueItem(t, ev.ID), nil)
		require.True(t, res.Done)
		assert.Equal(t, 1, core.authCalls)
		assert.Zero(t, core.needsCalls)
		assert.Zero(t, core.enrichCalls)
	})
}

func TestDispatcherEnrichment_IdempotentLostLeaseRehandle(t *testing.T) {
	ctx := context.Background()
	st := newMemTriggerStore()
	rules, err := triggerrule.NewDefault()
	require.NoError(t, err)

	projectID := uuid.NewString()
	ev := newPRCommentEvent(projectID)
	require.NoError(t, st.CreateTriggerEvent(ctx, ev))

	require.NoError(t, st.CreateTrigger(ctx, &store.Trigger{
		ID:            uuid.NewString(),
		ProjectID:     projectID,
		Name:          "pr-comment",
		SourceKind:    store.TriggerSourceKindGitHub,
		SourceID:      "",
		MatchTypes:    []string{"github.issue_comment.created"},
		MatchRules:    []store.TriggerMatchRule{{Dialect: "cel", Expr: "event.attributes.is_fork == 'false'"}},
		ActionType:    store.TriggerActionMessage,
		ActionPayload: `{"agent_id":"a1","message":"hi"}`,
		Status:        store.TriggerStatusActive,
	}))

	core := &enrichTestCore{
		authorized: true,
		enrichFn: func(_ context.Context, ev *hubfire.FireEvent) (map[string]string, map[string][]string, error) {
			attrs := maps.Clone(ev.Attributes)
			attrs["is_fork"] = "false"
			attrs["enriched"] = "true"
			return attrs, ev.Lists, nil
		},
	}

	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{Core: core, Store: st, Rules: rules})
	require.NoError(t, err)

	// Simulate first attempt losing its lease right after enrichment persisted (event stays pending).
	res1 := disp.HandleItem(ctx, makeEventQueueItem(t, ev.ID), nil)
	require.True(t, res1.Done)
	assert.Equal(t, 1, core.enrichCalls)

	// Reset event back to pending (simulating lost lease before MarkTriggerEventProcessed)
	// while keeping the persisted enriched attributes.
	st.mu.Lock()
	st.events[ev.ID].Status = store.TriggerEventStatusPending
	st.mu.Unlock()

	res2 := disp.HandleItem(ctx, makeEventQueueItem(t, ev.ID), nil)
	require.True(t, res2.Done)
	assert.Equal(t, 1, core.enrichCalls, "lost-lease re-handle must short-circuit via enriched attribute")

	// Also verify the nil-attrs error fallback sets enriched="failed" and
	// short-circuits a subsequent lost-lease re-handle.
	evFail := newPRCommentEvent(projectID)
	require.NoError(t, st.CreateTriggerEvent(ctx, evFail))
	coreNilErr := &enrichTestCore{
		authorized: true,
		enrichFn: func(_ context.Context, _ *hubfire.FireEvent) (map[string]string, map[string][]string, error) {
			return nil, nil, errors.New("generic enricher failure")
		},
	}
	dispNilErr, err := trigger.NewDispatcher(trigger.DispatcherConfig{Core: coreNilErr, Store: st, Rules: rules})
	require.NoError(t, err)

	resFail1 := dispNilErr.HandleItem(ctx, makeEventQueueItem(t, evFail.ID), nil)
	require.True(t, resFail1.Done)
	assert.Equal(t, 1, coreNilErr.enrichCalls)
	storedFail, err := st.GetTriggerEvent(ctx, evFail.ID)
	require.NoError(t, err)
	assert.Equal(t, "failed", storedFail.Attributes["enriched"])

	st.mu.Lock()
	st.events[evFail.ID].Status = store.TriggerEventStatusPending
	st.mu.Unlock()

	resFail2 := dispNilErr.HandleItem(ctx, makeEventQueueItem(t, evFail.ID), nil)
	require.True(t, resFail2.Done)
	assert.Equal(t, 1, coreNilErr.enrichCalls, "lost-lease re-handle after enriched=failed must not call Enrich again")
}
