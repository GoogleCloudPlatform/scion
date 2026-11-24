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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/GoogleCloudPlatform/scion/pkg/celengine"
	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/triggerrule"
)

type fakeCore struct {
	enabled           atomic.Bool
	githubDisabled    atomic.Bool
	admitFn           func(ctx, itemCtx context.Context) (context.Context, context.Context, func(), bool)
	fireFn            func(ctx context.Context, req hubfire.FireRequest) hubfire.FireOutcome
	authorizeSourceFn func(ctx context.Context, ev *store.TriggerEvent) (bool, error)
}

func (c *fakeCore) Fire(ctx context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
	if c.fireFn != nil {
		return c.fireFn(ctx, req)
	}
	return hubfire.FireOutcome{Status: hubfire.FireStatusFired}
}

func (c *fakeCore) Experiment(name string) bool {
	switch name {
	case experiments.Triggers:
		return c.enabled.Load()
	case experiments.GitHubTriggers:
		return c.enabled.Load() && !c.githubDisabled.Load()
	default:
		return false
	}
}

func (c *fakeCore) AdmitFire(ctx, itemCtx context.Context) (context.Context, context.Context, func(), bool) {
	if c.admitFn != nil {
		return c.admitFn(ctx, itemCtx)
	}
	return ctx, itemCtx, func() {}, true
}

func (c *fakeCore) AuthorizeEventSource(ctx context.Context, ev *store.TriggerEvent) (bool, error) {
	if c.authorizeSourceFn != nil {
		return c.authorizeSourceFn(ctx, ev)
	}
	return true, nil
}

type nopLease struct{}

func (nopLease) Renew(context.Context) (bool, error) { return true, nil }

// memTriggerStore is a minimal in-memory TriggerStore + DurableQueueStore for
// pure unit tests that also compile and run under -tags no_sqlite.
type memTriggerStore struct {
	mu                    sync.Mutex
	events                map[string]*store.TriggerEvent
	triggers              []*store.Trigger
	fires                 map[string]*store.TriggerFire // key: eventID + "|" + triggerID
	byFireID              map[string]*store.TriggerFire
	claimSubjectSkipCalls atomic.Int32
}

func newMemTriggerStore() *memTriggerStore {
	return &memTriggerStore{
		events:   make(map[string]*store.TriggerEvent),
		fires:    make(map[string]*store.TriggerFire),
		byFireID: make(map[string]*store.TriggerFire),
	}
}

func (m *memTriggerStore) LockTriggerQuota(context.Context, string) error { return nil }
func (m *memTriggerStore) CreateWebhookEndpoint(context.Context, *store.WebhookEndpoint) error {
	return nil
}
func (m *memTriggerStore) GetWebhookEndpoint(context.Context, string) (*store.WebhookEndpoint, error) {
	return nil, store.ErrNotFound
}
func (m *memTriggerStore) GetWebhookEndpointByName(context.Context, string, string) (*store.WebhookEndpoint, error) {
	return nil, store.ErrNotFound
}
func (m *memTriggerStore) ListWebhookEndpoints(context.Context, store.WebhookEndpointFilter, store.ListOptions) (*store.ListResult[store.WebhookEndpoint], error) {
	return &store.ListResult[store.WebhookEndpoint]{}, nil
}
func (m *memTriggerStore) CountWebhookEndpoints(context.Context, string) (int, error) { return 0, nil }
func (m *memTriggerStore) FlushWebhookEndpointCounters(context.Context, string, int, int, *time.Time) error {
	return nil
}
func (m *memTriggerStore) DeleteWebhookEndpoint(context.Context, string, string) ([]string, []string, error) {
	return nil, nil, nil
}
func (m *memTriggerStore) CreateTrigger(_ context.Context, tr *store.Trigger) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *tr
	m.triggers = append(m.triggers, &cp)
	return nil
}
func (m *memTriggerStore) GetTrigger(_ context.Context, id string) (*store.Trigger, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, tr := range m.triggers {
		if tr.ID == id {
			cp := *tr
			return &cp, nil
		}
	}
	return nil, store.ErrNotFound
}
func (m *memTriggerStore) GetTriggerByName(context.Context, string, string) (*store.Trigger, error) {
	return nil, store.ErrNotFound
}
func (m *memTriggerStore) ListTriggers(_ context.Context, filter store.TriggerFilter, _ store.ListOptions) (*store.ListResult[store.Trigger], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var items []store.Trigger
	for _, tr := range m.triggers {
		if filter.ProjectID != "" && tr.ProjectID != filter.ProjectID {
			continue
		}
		if filter.SourceKind != "" && tr.SourceKind != filter.SourceKind {
			continue
		}
		if filter.SourceID != "" && tr.SourceID != filter.SourceID {
			continue
		}
		if len(filter.SourceIDs) > 0 && !slices.Contains(filter.SourceIDs, tr.SourceID) {
			continue
		}
		if filter.Status != "" && tr.Status != filter.Status {
			continue
		}
		items = append(items, *tr)
	}
	return &store.ListResult[store.Trigger]{Items: items, TotalCount: len(items)}, nil
}
func (m *memTriggerStore) CountTriggers(context.Context, store.TriggerFilter) (int, error) {
	return 0, nil
}
func (m *memTriggerStore) ProjectsWithTriggersBySourceKind(_ context.Context, projectIDs []string, sourceKind string) (map[string]bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]bool)
	for _, tr := range m.triggers {
		if tr.SourceKind == sourceKind && slices.Contains(projectIDs, tr.ProjectID) {
			out[tr.ProjectID] = true
		}
	}
	return out, nil
}
func (m *memTriggerStore) UpdateTrigger(context.Context, *store.Trigger, store.TriggerFieldMask, int, bool, *store.InitiatorAttribution) error {
	return nil
}
func (m *memTriggerStore) UpdateTriggerStatus(context.Context, string, string, string) error {
	return nil
}
func (m *memTriggerStore) RecordTriggerFire(_ context.Context, id string, firedAt time.Time, status, errMsg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, tr := range m.triggers {
		if tr.ID == id {
			tr.FireCount++
			if status == store.TriggerFireStatusFailed || status == store.TriggerFireStatusRefused {
				tr.ErrorCount++
			}
			t := firedAt
			tr.LastFireAt = &t
			tr.LastFireStatus = status
			tr.LastFireError = errMsg
			return nil
		}
	}
	return store.ErrNotFound
}
func (m *memTriggerStore) DeleteTrigger(context.Context, string) error { return nil }
func (m *memTriggerStore) DeleteTriggersByProject(context.Context, string) (int, error) {
	return 0, nil
}

func (m *memTriggerStore) CreateTriggerEvent(_ context.Context, evt *store.TriggerEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *evt
	m.events[cp.ID] = &cp
	return nil
}
func (m *memTriggerStore) GetTriggerEvent(_ context.Context, id string) (*store.TriggerEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ev, ok := m.events[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *ev
	return &cp, nil
}
func (m *memTriggerStore) GetTriggerEventByDedupKey(context.Context, string, string, string) (*store.TriggerEvent, error) {
	return nil, store.ErrNotFound
}
func (m *memTriggerStore) GetTriggerEventByBodySHA256(context.Context, string, string, string) (*store.TriggerEvent, error) {
	return nil, store.ErrNotFound
}
func (m *memTriggerStore) ListTriggerEvents(context.Context, store.TriggerEventFilter, store.ListOptions) (*store.ListResult[store.TriggerEvent], error) {
	return &store.ListResult[store.TriggerEvent]{}, nil
}
func (m *memTriggerStore) CountPendingTriggerEvents(context.Context, string) (int, error) {
	return 0, nil
}
func (m *memTriggerStore) UpdateTriggerEventAttributes(_ context.Context, id string, attributes map[string]string, lists map[string][]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ev, ok := m.events[id]
	if !ok {
		return store.ErrNotFound
	}
	if ev.Status != store.TriggerEventStatusPending {
		return nil
	}
	ev.Attributes = maps.Clone(attributes)
	if lists != nil {
		cp := make(map[string][]string, len(lists))
		for k, vals := range lists {
			s := make([]string, len(vals))
			copy(s, vals)
			cp[k] = s
		}
		ev.Lists = cp
	}
	return nil
}
func (m *memTriggerStore) MarkTriggerEventProcessed(_ context.Context, id string, processedAt time.Time, matchCount, fireCount int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ev, ok := m.events[id]
	if !ok {
		return store.ErrNotFound
	}
	if ev.Status != store.TriggerEventStatusPending {
		return nil
	}
	ev.Status = store.TriggerEventStatusProcessed
	t := processedAt
	ev.ProcessedAt = &t
	ev.MatchCount = matchCount
	ev.FireCount = fireCount
	ev.Error = ""
	return nil
}
func (m *memTriggerStore) MarkTriggerEventFailed(_ context.Context, id string, processedAt time.Time, errMsg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ev, ok := m.events[id]
	if !ok {
		return store.ErrNotFound
	}
	if ev.Status != store.TriggerEventStatusPending {
		return nil
	}
	ev.Status = store.TriggerEventStatusFailed
	t := processedAt
	ev.ProcessedAt = &t
	ev.Error = errMsg
	return nil
}
func (m *memTriggerStore) ReconcileOrphanedPendingTriggerEvents(context.Context, string, time.Time, time.Time, string, int) (int, error) {
	return 0, nil
}
func (m *memTriggerStore) ClaimTriggerFire(_ context.Context, fire *store.TriggerFire) (bool, *store.TriggerFire, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := fire.EventID + "|" + fire.TriggerID
	if ex, exists := m.fires[key]; exists {
		cp := *ex
		return false, &cp, nil
	}
	cp := *fire
	if cp.Status == "" {
		cp.Status = store.TriggerFireStatusFiring
	}
	m.fires[key] = &cp
	m.byFireID[cp.ID] = &cp
	*fire = cp
	return true, nil, nil
}
func (m *memTriggerStore) ClaimTriggerFireSubjectSkip(_ context.Context, fire *store.TriggerFire, skipError string, now time.Time) (bool, bool, *store.TriggerFire, error) {
	m.claimSubjectSkipCalls.Add(1)
	m.mu.Lock()
	defer m.mu.Unlock()
	key := fire.EventID + "|" + fire.TriggerID
	if ex, exists := m.fires[key]; exists {
		cp := *ex
		return false, false, &cp, nil
	}
	busy := false
	for _, f := range m.byFireID {
		if f.TriggerID == fire.TriggerID &&
			f.Subject == fire.Subject &&
			f.Status == store.TriggerFireStatusFiring &&
			f.LeaseUntil.After(now.UTC()) {
			busy = true
			break
		}
	}
	cp := *fire
	if busy {
		cp.Status = store.TriggerFireStatusSkipped
		cp.Error = skipError
		fin := now.UTC()
		cp.FinishedAt = &fin
	} else if cp.Status == "" {
		cp.Status = store.TriggerFireStatusFiring
	}
	m.fires[key] = &cp
	m.byFireID[cp.ID] = &cp
	*fire = cp
	return true, busy, nil, nil
}
func (m *memTriggerStore) ClaimTriggerFireCoalescing(_ context.Context, fire *store.TriggerFire, coalesceError string, now, notBefore time.Time) (bool, bool, []store.TriggerFire, *store.TriggerFire, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now = now.UTC()
	notBefore = notBefore.UTC()
	leaseHorizon := fire.LeaseUntil.UTC().Sub(now)
	if leaseHorizon <= 0 {
		leaseHorizon = trigger.FireLeaseHorizon
	}
	key := fire.EventID + "|" + fire.TriggerID
	if ex, exists := m.fires[key]; exists {
		if ex.Status == store.TriggerFireStatusCoalescing || ex.Status == store.TriggerFireStatusDeferred {
			if ex.NotBefore != nil && ex.NotBefore.UTC().After(now) {
				cp := *ex
				return false, false, nil, &cp, nil
			}
			ex.Status = store.TriggerFireStatusFiring
			ex.LeaseUntil = now.Add(leaseHorizon)
			cp := *ex
			*fire = cp
			return true, true, nil, nil, nil
		}
		if ex.Status == store.TriggerFireStatusFailed &&
			(ex.Error == trigger.ErrFireReapedDuringCoalesce || ex.Error == trigger.ErrFireReapedDuringDefer) {
			ex.Status = store.TriggerFireStatusFiring
			ex.Error = ""
			ex.FinishedAt = nil
			ex.LeaseUntil = now.Add(leaseHorizon)
			cp := *ex
			*fire = cp
			return true, true, nil, nil, nil
		}
		cp := *ex
		return false, false, nil, &cp, nil
	}
	var superseded []store.TriggerFire
	for _, f := range m.byFireID {
		isPendingOrReaped := f.Status == store.TriggerFireStatusCoalescing ||
			f.Status == store.TriggerFireStatusDeferred ||
			(f.Status == store.TriggerFireStatusFailed && (f.Error == trigger.ErrFireReapedDuringCoalesce || f.Error == trigger.ErrFireReapedDuringDefer))
		if f.TriggerID == fire.TriggerID && f.Subject == fire.Subject && isPendingOrReaped {
			f.Status = store.TriggerFireStatusSkipped
			f.Error = coalesceError
			fin := now
			f.FinishedAt = &fin
			superseded = append(superseded, *f)
		}
	}
	cp := *fire
	cp.Status = store.TriggerFireStatusCoalescing
	nb := notBefore
	cp.NotBefore = &nb
	cp.LeaseUntil = notBefore.Add(leaseHorizon)
	m.fires[key] = &cp
	m.byFireID[cp.ID] = &cp
	*fire = cp
	return true, false, superseded, nil, nil
}
func (m *memTriggerStore) DeferTriggerFire(_ context.Context, id string, deferredAt, notBefore, leaseUntil time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.byFireID[id]
	if !ok || f.Status != store.TriggerFireStatusFiring {
		return false, nil
	}
	f.Status = store.TriggerFireStatusDeferred
	if f.DeferredAt == nil && !deferredAt.IsZero() {
		da := deferredAt.UTC()
		f.DeferredAt = &da
	}
	nb := notBefore.UTC()
	f.NotBefore = &nb
	f.LeaseUntil = leaseUntil.UTC()
	return true, nil
}
func (m *memTriggerStore) FinishPendingTriggerFire(_ context.Context, id, status, errMsg, matchedThrough, decidedDialect, ruleTrace string, finishedAt time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.byFireID[id]
	if !ok {
		return false, nil
	}
	isPendingOrReaped := f.Status == store.TriggerFireStatusCoalescing ||
		f.Status == store.TriggerFireStatusDeferred ||
		(f.Status == store.TriggerFireStatusFailed && (f.Error == trigger.ErrFireReapedDuringCoalesce || f.Error == trigger.ErrFireReapedDuringDefer))
	if !isPendingOrReaped {
		return false, nil
	}
	f.Status = status
	f.Error = errMsg
	if matchedThrough != "" {
		f.MatchedThrough = matchedThrough
	}
	if decidedDialect != "" {
		f.DecidedDialect = decidedDialect
	}
	if ruleTrace != "" {
		f.RuleTrace = ruleTrace
	}
	t := finishedAt.UTC()
	f.FinishedAt = &t
	return true, nil
}
func (m *memTriggerStore) FinishTriggerFire(_ context.Context, id, status, resultRef, targetAgentID, errMsg string, finishedAt time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.byFireID[id]
	if !ok || f.Status != store.TriggerFireStatusFiring {
		return false, nil
	}
	f.Status = status
	f.ResultRef = resultRef
	if targetAgentID != "" {
		f.TargetAgentID = targetAgentID
	}
	f.Error = errMsg
	t := finishedAt
	f.FinishedAt = &t
	return true, nil
}
func (m *memTriggerStore) GetTriggerFire(_ context.Context, id string) (*store.TriggerFire, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.byFireID[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	cp := *f
	return &cp, nil
}
func (m *memTriggerStore) ListTriggerFires(_ context.Context, filter store.TriggerFireFilter, _ store.ListOptions) (*store.ListResult[store.TriggerFire], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var items []store.TriggerFire
	for _, f := range m.byFireID {
		if filter.EventID != "" && f.EventID != filter.EventID {
			continue
		}
		if filter.TriggerID != "" && f.TriggerID != filter.TriggerID {
			continue
		}
		if filter.Status != "" && f.Status != filter.Status {
			continue
		}
		items = append(items, *f)
	}
	return &store.ListResult[store.TriggerFire]{Items: items, TotalCount: len(items)}, nil
}
func (m *memTriggerStore) CountTriggerFiresSince(context.Context, string, time.Time, ...string) (int, error) {
	return 0, nil
}
func (m *memTriggerStore) HasLiveTriggerFireForSubject(_ context.Context, triggerID, subject string, now time.Time) (bool, error) {
	return m.HasLiveTriggerFireForSubjectExcluding(context.Background(), triggerID, subject, "", true, now)
}
func (m *memTriggerStore) HasLiveTriggerFireForSubjectExcluding(_ context.Context, triggerID, subject, excludeFireID string, includeFiring bool, now time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, f := range m.byFireID {
		if f.ID == excludeFireID {
			continue
		}
		statusMatch := f.Status == store.TriggerFireStatusCoalescing ||
			f.Status == store.TriggerFireStatusDeferred ||
			(includeFiring && f.Status == store.TriggerFireStatusFiring)
		if f.TriggerID == triggerID &&
			f.Subject == subject &&
			statusMatch &&
			f.LeaseUntil.After(now.UTC()) {
			return true, nil
		}
	}
	return false, nil
}
func (m *memTriggerStore) HasPendingTriggerFiresForEvent(_ context.Context, eventID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, f := range m.byFireID {
		if f.EventID != eventID {
			continue
		}
		if f.Status == store.TriggerFireStatusCoalescing ||
			f.Status == store.TriggerFireStatusDeferred ||
			(f.Status == store.TriggerFireStatusFailed && (f.Error == trigger.ErrFireReapedDuringCoalesce || f.Error == trigger.ErrFireReapedDuringDefer)) {
			return true, nil
		}
	}
	return false, nil
}
func (m *memTriggerStore) AgentHasFiredTriggerForEvent(context.Context, string, string) (bool, error) {
	return false, nil
}
func (m *memTriggerStore) EventHasTriggerFireForResultRef(context.Context, string, string) (bool, error) {
	return false, nil
}
func (m *memTriggerStore) ReapExpiredTriggerFires(context.Context, time.Time, int) (int, error) {
	return 0, nil
}
func (m *memTriggerStore) PurgeOldTriggerHistory(context.Context, time.Time) (int, int, error) {
	return 0, 0, nil
}
func (m *memTriggerStore) ClaimTriggerSubject(context.Context, string, string, string, string, time.Time, time.Time) (*store.TriggerSubjectClaim, *store.TriggerSubjectClaim, error) {
	return nil, nil, nil
}
func (m *memTriggerStore) BindTriggerSubjectClaim(context.Context, string, string, string, time.Time) (bool, error) {
	return false, nil
}
func (m *memTriggerStore) ReleaseTriggerSubjectClaim(context.Context, string, string, time.Time) (bool, error) {
	return false, nil
}
func (m *memTriggerStore) GetActiveTriggerSubjectClaim(context.Context, string, string) (*store.TriggerSubjectClaim, error) {
	return nil, store.ErrNotFound
}
func (m *memTriggerStore) ReleaseTriggerSubjectClaimsForAgent(context.Context, string, time.Time) (int, error) {
	return 0, nil
}
func (m *memTriggerStore) ReleaseTriggerSubjectClaimsForProjectBySourceKind(context.Context, string, string, string, time.Time) (int, error) {
	return 0, nil
}
func (m *memTriggerStore) ReleaseTerminalTriggerSubjectClaims(context.Context, time.Time) (int, error) {
	return 0, nil
}
func (m *memTriggerStore) PurgeOldTriggerSubjectClaims(context.Context, time.Time) (int, error) {
	return 0, nil
}

func TestDispatcher_WakeFireDetachAndPerFireBudget(t *testing.T) {
	ms := newMemTriggerStore()
	core := &fakeCore{}
	core.enabled.Store(true)

	projID := uuid.NewString()
	epID := uuid.NewString()
	trID := uuid.NewString()
	evID := uuid.NewString()
	t0 := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

	require.NoError(t, ms.CreateTrigger(context.Background(), &store.Trigger{
		ID:            trID,
		ProjectID:     projID,
		Name:          "wake-tr",
		SourceKind:    store.TriggerSourceKindWebhook,
		SourceID:      epID,
		MatchTypes:    []string{"webhook.*"},
		MatchDialect:  store.TriggerDialectCEL,
		ActionType:    store.TriggerActionMessage,
		ActionPayload: `{"agentId":"a1","message":"hi","wake":true}`,
		SubjectMode:   store.TriggerSubjectModeNew,
		Status:        store.TriggerStatusActive,
	}))
	longSubject := "webhook:" + epID + "/prod-" + strings.Repeat("s", 300)
	require.Greater(t, len(longSubject), trigger.MaxFireSubjectBytes)
	require.NoError(t, ms.CreateTriggerEvent(context.Background(), &store.TriggerEvent{
		ID:         evID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		Type:       "webhook.deploy.finished",
		Subject:    longSubject,
		DedupKey:   "d-1",
		OccurredAt: t0,
		ReceivedAt: t0,
		Attributes: map[string]string{"event": "deploy.finished"},
		Payload:    `{"ok":true}`,
		Status:     store.TriggerEventStatusPending,
	}))

	type ctxKey struct{}
	core.fireFn = func(ctx context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
		// Wait past the short per-fire timeout while detached onto the item context.
		valCtx := context.WithValue(ctx, ctxKey{}, "kept")
		detached, cancel := req.Tracked.Detach(valCtx)
		defer cancel()

		assert.Equal(t, "kept", detached.Value(ctxKey{}))
		// Per-fire ctx expires in 25ms, while detached survives because item ctx has 2s.
		<-ctx.Done()
		assert.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
		assert.NoError(t, detached.Err(), "detached wake context must outlive per-fire timeout inside item budget")

		admittedCtx, doneAdmit, ok := req.Tracked.Admit(detached)
		assert.True(t, ok)
		defer doneAdmit()
		assert.NoError(t, admittedCtx.Err())

		assert.Equal(t, trigger.ExecutorKindTriggerDrainer, req.Executor.Kind)
		assert.Equal(t, trigger.ExecutorIDPrefixTriggerEvent+evID, req.Executor.ID)
		require.NotNil(t, req.Event)
		assert.Equal(t, longSubject, req.Event.Subject, "FireEvent carries full trigger_events.subject")
		require.NotNil(t, req.Event.Trigger)
		assert.Equal(t, trID, req.Event.Trigger.ID)
		assert.Equal(t, "wake-tr", req.Event.Trigger.Name)

		return hubfire.FireOutcome{Status: hubfire.FireStatusFired}
	}

	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:           core,
		Store:          ms,
		PerFireTimeout: 25 * time.Millisecond,
		Now:            func() time.Time { return t0 },
	})
	require.NoError(t, err)

	itemCtx, cancelItem := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelItem()

	raw, err := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: evID})
	require.NoError(t, err)
	res := disp.HandleItem(itemCtx, durablequeue.Item{
		Seq:       1,
		Kind:      trigger.EventQueueKind,
		DedupKey:  evID,
		ProjectID: projID,
		Payload:   raw,
	}, nopLease{})
	assert.True(t, res.Done)
	assert.NoError(t, res.Err)

	ev, err := ms.GetTriggerEvent(context.Background(), evID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerEventStatusProcessed, ev.Status)
	assert.Equal(t, 1, ev.MatchCount)
	assert.Equal(t, 1, ev.FireCount)

	fires, err := ms.ListTriggerFires(context.Background(), store.TriggerFireFilter{}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, fires.Items, 1)
	assert.Equal(t, trigger.MaxFireSubjectBytes, len(fires.Items[0].Subject), "trigger_fires.subject keeps 256-byte truncation")
	assert.Equal(t, longSubject[:trigger.MaxFireSubjectBytes], fires.Items[0].Subject)
}

func TestDispatcher_RuleBudgetExhaustedAndMalformedPayload(t *testing.T) {
	ms := newMemTriggerStore()
	core := &fakeCore{}
	core.enabled.Store(true)

	projID := uuid.NewString()
	epID := uuid.NewString()
	tr1 := uuid.NewString()
	tr2 := uuid.NewString()
	evID := uuid.NewString()

	cur := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	var step int
	clock := func() time.Time {
		step++
		// Advance clock past 1s rule budget right after JSON payload parse.
		if step >= 2 {
			return cur.Add(2 * time.Second)
		}
		return cur
	}

	require.NoError(t, ms.CreateTrigger(context.Background(), &store.Trigger{
		ID:           tr1,
		ProjectID:    projID,
		Name:         "match-tr",
		SourceKind:   store.TriggerSourceKindWebhook,
		SourceID:     epID,
		MatchTypes:   []string{"webhook.received"},
		MatchDialect: store.TriggerDialectCEL,
		ActionType:   store.TriggerActionMessage,
		SubjectMode:  store.TriggerSubjectModeNew,
		Status:       store.TriggerStatusActive,
	}))
	require.NoError(t, ms.CreateTrigger(context.Background(), &store.Trigger{
		ID:           tr2,
		ProjectID:    projID,
		Name:         "nonmatch-tr",
		SourceKind:   store.TriggerSourceKindWebhook,
		SourceID:     epID,
		MatchTypes:   []string{"webhook.other"},
		MatchDialect: store.TriggerDialectCEL,
		ActionType:   store.TriggerActionMessage,
		SubjectMode:  store.TriggerSubjectModeNew,
		Status:       store.TriggerStatusActive,
	}))
	require.NoError(t, ms.CreateTriggerEvent(context.Background(), &store.TriggerEvent{
		ID:         evID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		Type:       "webhook.received",
		Subject:    "webhook:" + epID,
		DedupKey:   "d-budget",
		OccurredAt: cur,
		ReceivedAt: cur,
		Payload:    `{"x":1}`,
		Status:     store.TriggerEventStatusPending,
	}))

	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:       core,
		Store:      ms,
		RuleBudget: time.Second,
		Now:        clock,
	})
	require.NoError(t, err)

	// Malformed payload is Permanent failure.
	badRes := disp.HandleItem(context.Background(), durablequeue.Item{
		Kind:    trigger.EventQueueKind,
		Payload: []byte(`{"v":2,"event_id":""}`),
	}, nopLease{})
	assert.True(t, badRes.Permanent)
	assert.False(t, badRes.Done)

	// Budget-exhausted event records skipped "rule budget exhausted" for matching trigger only,
	// without counting as an error on the trigger definition (§5.5, §6.3).
	raw, _ := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: evID})
	res := disp.HandleItem(context.Background(), durablequeue.Item{
		Kind:     trigger.EventQueueKind,
		DedupKey: evID,
		Payload:  raw,
	}, nopLease{})
	assert.True(t, res.Done)

	fires, err := ms.ListTriggerFires(context.Background(), store.TriggerFireFilter{}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, fires.Items, 1)
	assert.Equal(t, tr1, fires.Items[0].TriggerID)
	assert.Equal(t, store.TriggerFireStatusSkipped, fires.Items[0].Status)
	assert.Equal(t, trigger.ErrFireRuleBudgetExhausted, fires.Items[0].Error)

	gotTr1, err := ms.GetTrigger(context.Background(), tr1)
	require.NoError(t, err)
	assert.Equal(t, 1, gotTr1.FireCount)
	assert.Equal(t, 0, gotTr1.ErrorCount)
}

func TestDispatcher_WakeFireAdmitWaitPastPerFireTimeoutThenDetach(t *testing.T) {
	type abortKey struct{}

	makeAdmitter := func(shutdownCtx context.Context) func(ctx, itemCtx context.Context) (context.Context, context.Context, func(), bool) {
		return func(ctx, itemCtx context.Context) (context.Context, context.Context, func(), bool) {
			abortCtx, cancelAbort := context.WithCancel(context.Background())
			stopShutdown := context.AfterFunc(shutdownCtx, cancelAbort)
			stopItem := context.AfterFunc(itemCtx, cancelAbort)
			fireCtx, cancelFire := context.WithCancel(ctx)
			stopFire := context.AfterFunc(abortCtx, cancelFire)
			withAbort := context.WithValue(fireCtx, abortKey{}, abortCtx)
			return withAbort, abortCtx, func() {
				stopFire()
				stopItem()
				stopShutdown()
				cancelFire()
				cancelAbort()
			}, true
		}
	}

	t.Run("Admit then wait past PerFireTimeout then Detach stays alive and records fired", func(t *testing.T) {
		ms := newMemTriggerStore()
		shutdownCtx, cancelShutdown := context.WithCancel(context.Background())
		defer cancelShutdown()

		core := &fakeCore{admitFn: makeAdmitter(shutdownCtx)}
		core.enabled.Store(true)

		projID := uuid.NewString()
		epID := uuid.NewString()
		trID := uuid.NewString()
		evID := uuid.NewString()

		require.NoError(t, ms.CreateTrigger(context.Background(), &store.Trigger{
			ID:           trID,
			ProjectID:    projID,
			Name:         "real-order-wake",
			SourceKind:   store.TriggerSourceKindWebhook,
			SourceID:     epID,
			MatchTypes:   []string{"webhook.received"},
			MatchDialect: store.TriggerDialectCEL,
			ActionType:   store.TriggerActionMessage,
			SubjectMode:  store.TriggerSubjectModeNew,
			Status:       store.TriggerStatusActive,
		}))
		require.NoError(t, ms.CreateTriggerEvent(context.Background(), &store.TriggerEvent{
			ID:         evID,
			ProjectID:  projID,
			SourceKind: store.TriggerSourceKindWebhook,
			SourceID:   epID,
			Type:       "webhook.received",
			Subject:    "webhook:" + epID,
			Status:     store.TriggerEventStatusPending,
			OccurredAt: time.Now().UTC(),
		}))

		var detachedAlive bool
		core.fireFn = func(ctx context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
			// Real deliverMessageAction order: Admit first, spend PerFireTimeout
			// during wake, then Detach for post-wake persist/dispatch.
			admittedCtx, doneAdmit, ok := req.Tracked.Admit(ctx)
			if !ok {
				return hubfire.FireOutcome{Status: hubfire.FireStatusFailed, Error: "not admitted"}
			}
			defer doneAdmit()

			<-admittedCtx.Done()
			if !assert.ErrorIs(t, admittedCtx.Err(), context.DeadlineExceeded) {
				return hubfire.FireOutcome{Status: hubfire.FireStatusFailed, Error: admittedCtx.Err().Error()}
			}

			detached, cancelDetached := req.Tracked.Detach(admittedCtx)
			defer cancelDetached()

			// context.AfterFunc invokes cancel in its own goroutine when abortCtx is
			// already done; wait across a short scheduling window so an R1 revert
			// (holding the expired admittedCtx as abortCtx) deterministically fails.
			select {
			case <-detached.Done():
				return hubfire.FireOutcome{Status: hubfire.FireStatusFailed, Error: detached.Err().Error()}
			case <-time.After(30 * time.Millisecond):
			}
			if err := detached.Err(); err != nil {
				return hubfire.FireOutcome{Status: hubfire.FireStatusFailed, Error: err.Error()}
			}
			assert.NotNil(t, detached.Value(abortKey{}), "context values from Admit must carry onto Detach")
			detachedAlive = true
			return hubfire.FireOutcome{Status: hubfire.FireStatusFired}
		}

		disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
			Core:           core,
			Store:          ms,
			PerFireTimeout: 20 * time.Millisecond,
		})
		require.NoError(t, err)

		itemCtx, cancelItem := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancelItem()

		raw, err := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: evID})
		require.NoError(t, err)
		res := disp.HandleItem(itemCtx, durablequeue.Item{
			Seq:       1,
			Kind:      trigger.EventQueueKind,
			DedupKey:  evID,
			ProjectID: projID,
			Payload:   raw,
		}, nopLease{})
		require.NoError(t, res.Err)
		assert.True(t, res.Done)
		assert.True(t, detachedAlive, "post-wake Detach must succeed with live context after PerFireTimeout")

		fires, err := ms.ListTriggerFires(context.Background(), store.TriggerFireFilter{TriggerID: trID}, store.ListOptions{})
		require.NoError(t, err)
		require.Len(t, fires.Items, 1)
		assert.Equal(t, store.TriggerFireStatusFired, fires.Items[0].Status)
	})

	t.Run("Admit then wait past PerFireTimeout then Detach cancels on shutdown abort", func(t *testing.T) {
		ms := newMemTriggerStore()
		shutdownCtx, cancelShutdown := context.WithCancel(context.Background())
		defer cancelShutdown()

		core := &fakeCore{admitFn: makeAdmitter(shutdownCtx)}
		core.enabled.Store(true)

		projID := uuid.NewString()
		epID := uuid.NewString()
		trID := uuid.NewString()
		evID := uuid.NewString()

		require.NoError(t, ms.CreateTrigger(context.Background(), &store.Trigger{
			ID:           trID,
			ProjectID:    projID,
			Name:         "shutdown-abort-wake",
			SourceKind:   store.TriggerSourceKindWebhook,
			SourceID:     epID,
			MatchTypes:   []string{"webhook.received"},
			MatchDialect: store.TriggerDialectCEL,
			ActionType:   store.TriggerActionMessage,
			SubjectMode:  store.TriggerSubjectModeNew,
			Status:       store.TriggerStatusActive,
		}))
		require.NoError(t, ms.CreateTriggerEvent(context.Background(), &store.TriggerEvent{
			ID:         evID,
			ProjectID:  projID,
			SourceKind: store.TriggerSourceKindWebhook,
			SourceID:   epID,
			Type:       "webhook.received",
			Subject:    "webhook:" + epID,
			Status:     store.TriggerEventStatusPending,
			OccurredAt: time.Now().UTC(),
		}))

		detachedReady := make(chan struct{})
		detachedErrCh := make(chan error, 1)
		core.fireFn = func(ctx context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
			admittedCtx, doneAdmit, ok := req.Tracked.Admit(ctx)
			if !ok {
				return hubfire.FireOutcome{Status: hubfire.FireStatusFailed, Error: "not admitted"}
			}
			defer doneAdmit()

			<-admittedCtx.Done()
			detached, cancelDetached := req.Tracked.Detach(admittedCtx)
			defer cancelDetached()

			if err := detached.Err(); err != nil {
				detachedErrCh <- err
				close(detachedReady)
				return hubfire.FireOutcome{Status: hubfire.FireStatusFailed, Error: err.Error()}
			}
			close(detachedReady)
			<-detached.Done()
			detachedErrCh <- detached.Err()
			return hubfire.FireOutcome{Status: hubfire.FireStatusFailed, Error: detached.Err().Error()}
		}

		disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
			Core:           core,
			Store:          ms,
			PerFireTimeout: 20 * time.Millisecond,
		})
		require.NoError(t, err)

		itemCtx, cancelItem := context.WithCancel(context.Background())
		defer cancelItem()

		raw, err := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: evID})
		require.NoError(t, err)
		resCh := make(chan durablequeue.Result, 1)
		go func() {
			resCh <- disp.HandleItem(itemCtx, durablequeue.Item{
				Seq:       1,
				Kind:      trigger.EventQueueKind,
				DedupKey:  evID,
				ProjectID: projID,
				Payload:   raw,
			}, nopLease{})
		}()

		select {
		case <-detachedReady:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for detached context after PerFireTimeout")
		}
		select {
		case err := <-detachedErrCh:
			t.Fatalf("detached context exited prematurely before shutdown abort: %v", err)
		default:
		}

		cancelShutdown()

		select {
		case err := <-detachedErrCh:
			require.ErrorIs(t, err, context.Canceled)
		case <-time.After(time.Second):
			t.Fatal("detached context did not cancel on shutdown abort")
		}
		<-resCh
	})
}

type testDispatchDialect struct {
	name        string
	maxEvalTime time.Duration
	mu          sync.Mutex
	evalCalls   []string
	maxBudgets  []time.Duration
	hasTrackers []bool
	fn          func(ctx context.Context, rule string, budget ruleengine.Budget) (ruleengine.Decision, error)
}

func (d *testDispatchDialect) Name() string { return d.name }

func (d *testDispatchDialect) Declares() ruleengine.Capabilities {
	maxTime := d.maxEvalTime
	if maxTime <= 0 {
		maxTime = 100 * time.Millisecond
	}
	return ruleengine.Capabilities{
		PerformsIO:  false,
		MaxEvalTime: maxTime,
		ResultKinds: []ruleengine.Kind{ruleengine.KindFilter},
	}
}

func (d *testDispatchDialect) Compile(_ context.Context, rule string, kind ruleengine.Kind, env ruleengine.EnvironmentRef) (ruleengine.Compiled, error) {
	return ruleengine.NewCompiled(env.Consumer, d.name, kind, rule), nil
}

func (d *testDispatchDialect) Evaluate(ctx context.Context, c ruleengine.Compiled, _ ruleengine.Input, budget ruleengine.Budget) (ruleengine.Decision, error) {
	rule, _ := c.Rep().(string)
	d.mu.Lock()
	d.evalCalls = append(d.evalCalls, rule)
	d.maxBudgets = append(d.maxBudgets, budget.MaxEvalTime)
	d.hasTrackers = append(d.hasTrackers, budget.Tracker != nil)
	d.mu.Unlock()
	if d.fn != nil {
		return d.fn(ctx, rule, budget)
	}
	return ruleengine.Decision{Kind: c.Kind(), Bool: true}, nil
}

func newTestTriggerEngineWithDialects(t *testing.T, extra ...ruleengine.Dialect) (*ruleengine.Engine, *triggerrule.Rules) {
	t.Helper()
	celEng, err := celengine.New(triggerrule.Env(), triggerrule.Stage1Limits(), triggerrule.Stage1CacheConfig())
	require.NoError(t, err)
	celDialect, err := ruleengine.NewCELDialect(celEng)
	require.NoError(t, err)
	re := ruleengine.NewEngine()
	require.NoError(t, re.Register(celDialect))
	grants := []ruleengine.DialectGrant{ruleengine.Grant(ruleengine.DialectCEL)}
	for _, ex := range extra {
		require.NoError(t, re.Register(ex))
		grants = append(grants, ruleengine.Grant(ex.Name()))
	}
	require.NoError(t, re.AllowConsumerDialects(triggerrule.ConsumerName, grants...))
	rules, err := triggerrule.New(re)
	require.NoError(t, err)
	return re, rules
}

type memFireAuditSink struct {
	mu      sync.Mutex
	records []trigger.FireAuditRecord
}

func (s *memFireAuditSink) RecordFire(_ context.Context, rec trigger.FireAuditRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, rec)
}

func (s *memFireAuditSink) snapshot() []trigger.FireAuditRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]trigger.FireAuditRecord, len(s.records))
	copy(out, s.records)
	return out
}

func TestDispatcher_MatchAllOutcomesTraceMetricsAndAudit(t *testing.T) {
	ctx := context.Background()
	ms := newMemTriggerStore()
	core := &fakeCore{}
	core.enabled.Store(true)

	uncertainDialect := &testDispatchDialect{
		name: "uncertain_test",
		fn: func(_ context.Context, rule string, _ ruleengine.Budget) (ruleengine.Decision, error) {
			if rule == "abstain_case" {
				return ruleengine.Decision{Trace: "model=probe,verdict=abstain"}, &ruleengine.UncertainError{Reason: "abstain"}
			}
			return ruleengine.Decision{Kind: ruleengine.KindFilter, Bool: true}, nil
		},
	}
	_, rules := newTestTriggerEngineWithDialects(t, uncertainDialect)

	projID := uuid.NewString()
	epID := uuid.NewString()
	evID := uuid.NewString()
	t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

	trMatched := &store.Trigger{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		Name:       "tr-matched",
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		MatchTypes: []string{"webhook.received"},
		MatchRules: []store.TriggerMatchRule{
			{Dialect: "cel", Expr: `event.attributes.branch == "main"`},
			{Dialect: "cel", Expr: `has(event.payload.pr) && event.payload.pr.number == 42`},
		},
		ActionType: store.TriggerActionMessage,
		Status:     store.TriggerStatusActive,
		InitiatorAttribution: store.InitiatorAttribution{
			AuthorizationRevision: 3,
		},
	}
	trFalseShortCircuit := &store.Trigger{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		Name:       "tr-false",
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		MatchTypes: []string{"webhook.received"},
		MatchRules: []store.TriggerMatchRule{
			{Dialect: "cel", Expr: `event.attributes.branch == "release"`},
			{Dialect: "uncertain_test", Expr: `must_not_run`},
		},
		ActionType: store.TriggerActionMessage,
		Status:     store.TriggerStatusActive,
		InitiatorAttribution: store.InitiatorAttribution{
			AuthorizationRevision: 1,
		},
	}
	trUncertain := &store.Trigger{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		Name:       "tr-uncertain",
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		MatchTypes: []string{"webhook.received"},
		MatchRules: []store.TriggerMatchRule{
			{Dialect: "cel", Expr: `event.attributes.branch == "main"`},
			{Dialect: "uncertain_test", Expr: `abstain_case`},
			{Dialect: "cel", Expr: `true`},
		},
		ActionType: store.TriggerActionMessage,
		Status:     store.TriggerStatusActive,
		InitiatorAttribution: store.InitiatorAttribution{
			AuthorizationRevision: 2,
		},
	}
	trFailedRuntime := &store.Trigger{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		Name:       "tr-failed-runtime",
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		MatchTypes: []string{"webhook.received"},
		MatchRules: []store.TriggerMatchRule{
			{Dialect: "cel", Expr: `true`},
			{Dialect: "cel", Expr: `event.payload.missing_secret_key == "canary-value"`},
			{Dialect: "cel", Expr: `true`},
		},
		ActionType: store.TriggerActionMessage,
		Status:     store.TriggerStatusActive,
		InitiatorAttribution: store.InitiatorAttribution{
			AuthorizationRevision: 1,
		},
	}
	trNoLongerCompiles := &store.Trigger{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		Name:       "tr-compile-broken",
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		MatchTypes: []string{"webhook.received"},
		MatchRules: []store.TriggerMatchRule{
			{Dialect: "cel", Expr: `invalid syntax ===`},
		},
		ActionType: store.TriggerActionMessage,
		Status:     store.TriggerStatusActive,
		InitiatorAttribution: store.InitiatorAttribution{
			AuthorizationRevision: 1,
		},
	}

	for _, tr := range []*store.Trigger{trMatched, trFalseShortCircuit, trUncertain, trFailedRuntime, trNoLongerCompiles} {
		require.NoError(t, ms.CreateTrigger(ctx, tr))
	}

	const secretPayloadCanary = "top-secret-webhook-body-9912"
	require.NoError(t, ms.CreateTriggerEvent(ctx, &store.TriggerEvent{
		ID:         evID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		Type:       "webhook.received",
		Subject:    "webhook:" + epID,
		DedupKey:   "d-outcomes",
		OccurredAt: t0,
		ReceivedAt: t0,
		Attributes: map[string]string{"branch": "main"},
		Payload:    `{"pr":{"number":42},"body":"` + secretPayloadCanary + `"}`,
		Status:     store.TriggerEventStatusPending,
	}))

	var firedIDs []string
	core.fireFn = func(_ context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
		firedIDs = append(firedIDs, req.Authority.DefinitionID)
		return hubfire.FireOutcome{Status: hubfire.FireStatusFired, ResultRef: "msg-ok"}
	}

	var warnBuf, auditLogBuf bytes.Buffer
	warnLogger := slog.New(slog.NewJSONHandler(&warnBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	slogAudit := trigger.NewSlogAuditSink(trigger.SlogAuditConfig{
		Logger: slog.New(slog.NewJSONHandler(&auditLogBuf, &slog.HandlerOptions{Level: slog.LevelInfo})),
		Now:    func() time.Time { return t0 },
	})
	memAudit := &memFireAuditSink{}

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	otelMetrics, err := trigger.NewOTelRuleEvalMetrics(mp)
	require.NoError(t, err)

	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:        core,
		Store:       ms,
		Rules:       rules,
		RuleMetrics: otelMetrics,
		FireAudit: trigger.MultiFireAuditSink(
			slogAudit,
			memAudit,
		),
		Logger: warnLogger,
		Now:    func() time.Time { return t0 },
	})
	require.NoError(t, err)

	raw, err := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: evID})
	require.NoError(t, err)
	res := disp.HandleItem(ctx, durablequeue.Item{
		Seq:       1,
		Kind:      trigger.EventQueueKind,
		DedupKey:  evID,
		ProjectID: projID,
		Payload:   raw,
	}, nopLease{})
	require.NoError(t, res.Err)
	assert.True(t, res.Done)

	// Only trMatched fires; trFalseShortCircuit short-circuited before element 2.
	assert.Equal(t, []string{trMatched.ID}, firedIDs)
	assert.Equal(t, []string{"abstain_case"}, uncertainDialect.evalCalls, "element 2 of trFalse must not run after element 1 false")

	gotEv, err := ms.GetTriggerEvent(ctx, evID)
	require.NoError(t, err)
	assert.Equal(t, 1, gotEv.MatchCount, "match_count counts full matches only")
	assert.Equal(t, 1, gotEv.FireCount)

	firesRes, err := ms.ListTriggerFires(ctx, store.TriggerFireFilter{}, store.ListOptions{})
	require.NoError(t, err)
	byTrig := make(map[string]store.TriggerFire, len(firesRes.Items))
	for _, f := range firesRes.Items {
		byTrig[f.TriggerID] = f
	}
	assert.NotContains(t, byTrig, trFalseShortCircuit.ID, "false match must write no fire row")
	require.Len(t, byTrig, 4)

	// 1. matched -> fired, matched_through "2/2", decided_dialect "cel".
	fMatched := byTrig[trMatched.ID]
	assert.Equal(t, store.TriggerFireStatusFired, fMatched.Status)
	assert.Equal(t, "2/2", fMatched.MatchedThrough)
	assert.Equal(t, "cel", fMatched.DecidedDialect)
	var traceMatched triggerrule.RuleTrace
	require.NoError(t, json.Unmarshal([]byte(fMatched.RuleTrace), &traceMatched))
	require.Len(t, traceMatched.Elements, 2)
	assert.Equal(t, "matched", traceMatched.Elements[0].Class)
	assert.Equal(t, "matched", traceMatched.Elements[1].Class)
	assert.Empty(t, traceMatched.Unexecuted)
	assert.NotNil(t, traceMatched.BudgetRemainingMs)

	// 2. uncertain -> skipped "rule uncertain", matched_through "2/3", decided_dialect "uncertain_test".
	fUncertain := byTrig[trUncertain.ID]
	assert.Equal(t, store.TriggerFireStatusSkipped, fUncertain.Status)
	assert.Equal(t, triggerrule.SkipReasonRuleUncertain, fUncertain.Error)
	assert.Equal(t, "2/3", fUncertain.MatchedThrough)
	assert.Equal(t, "uncertain_test", fUncertain.DecidedDialect)
	var traceUnc triggerrule.RuleTrace
	require.NoError(t, json.Unmarshal([]byte(fUncertain.RuleTrace), &traceUnc))
	require.Len(t, traceUnc.Elements, 2)
	assert.Equal(t, "uncertain", traceUnc.Elements[1].Class)
	assert.Equal(t, "abstain", traceUnc.Elements[1].Reason)
	assert.Equal(t, []int{3}, traceUnc.Unexecuted)

	// 3. failed runtime -> skipped "rule evaluation failed", matched_through "2/3", decided_dialect "cel".
	fFailed := byTrig[trFailedRuntime.ID]
	assert.Equal(t, store.TriggerFireStatusSkipped, fFailed.Status)
	assert.Equal(t, triggerrule.SkipReasonEvalFailed, fFailed.Error)
	assert.Equal(t, "2/3", fFailed.MatchedThrough)
	assert.Equal(t, "cel", fFailed.DecidedDialect)
	assert.NotContains(t, fFailed.RuleTrace, "canary-value")
	assert.NotContains(t, fFailed.RuleTrace, secretPayloadCanary)
	var traceFail triggerrule.RuleTrace
	require.NoError(t, json.Unmarshal([]byte(fFailed.RuleTrace), &traceFail))
	require.Len(t, traceFail.Elements, 2)
	assert.Equal(t, "failed", traceFail.Elements[1].Class)
	assert.Equal(t, celengine.ReasonMissingInput, traceFail.Elements[1].Reason)
	assert.Equal(t, []int{3}, traceFail.Unexecuted)

	// 4. stored compile error -> skipped "rule no longer compiles" + slog.Warn.
	fCompile := byTrig[trNoLongerCompiles.ID]
	assert.Equal(t, store.TriggerFireStatusSkipped, fCompile.Status)
	assert.Equal(t, triggerrule.SkipReasonNoLongerCompiles, fCompile.Error)
	assert.Equal(t, "1/1", fCompile.MatchedThrough)
	assert.Equal(t, "cel", fCompile.DecidedDialect)
	assert.Contains(t, warnBuf.String(), trNoLongerCompiles.ID, "stored compile error must emit slog.Warn")
	assert.NotContains(t, warnBuf.String(), "invalid syntax ===", "warning log must remain content-free")

	// Skipped rules never increment ErrorCount on trigger definitions (§5.5, §6.3).
	for _, skipID := range []string{trUncertain.ID, trFailedRuntime.ID, trNoLongerCompiles.ID} {
		gotTr, gErr := ms.GetTrigger(ctx, skipID)
		require.NoError(t, gErr)
		assert.Equal(t, 1, gotTr.FireCount)
		assert.Equal(t, 0, gotTr.ErrorCount)
	}

	// Audit records carry matched_through and decided_dialect, and NEVER rule text or payload (§5).
	auditRecs := memAudit.snapshot()
	require.Len(t, auditRecs, 4)
	auditOut := auditLogBuf.String()
	assert.Contains(t, auditOut, `"event_type":"trigger.fire.fired"`)
	assert.Contains(t, auditOut, `"event_type":"trigger.fire.skipped"`)
	assert.Contains(t, auditOut, `"matched_through":"2/2"`)
	assert.Contains(t, auditOut, `"matched_through":"2/3"`)
	assert.Contains(t, auditOut, `"decided_dialect":"uncertain_test"`)
	for _, forbidden := range []string{
		secretPayloadCanary,
		"canary-value",
		"missing_secret_key",
		"event.attributes.branch",
		"invalid syntax ===",
	} {
		assert.NotContains(t, auditOut, forbidden, "audit log must never carry rule text or payload")
	}

	// Per-dialect evaluation counters & histogram (trigger_rule_eval_seconds{dialect, class}, §5).
	stats := disp.RuleEvalStats()
	assert.Equal(t, int64(4), stats[trigger.RuleEvalKey{Dialect: "cel", Class: "matched"}].Count)
	assert.Equal(t, int64(1), stats[trigger.RuleEvalKey{Dialect: "cel", Class: "false"}].Count)
	assert.Equal(t, int64(2), stats[trigger.RuleEvalKey{Dialect: "cel", Class: "failed"}].Count)
	assert.Equal(t, int64(1), stats[trigger.RuleEvalKey{Dialect: "uncertain_test", Class: "uncertain"}].Count)

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(ctx, &rm))
	var foundHist bool
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == trigger.MetricTriggerRuleEval {
				foundHist = true
				assert.Equal(t, "s", m.Unit, "histogram unit must be seconds for _seconds Prometheus export")
				hist, ok := m.Data.(metricdata.Histogram[float64])
				require.True(t, ok)
				assert.GreaterOrEqual(t, len(hist.DataPoints), 4)
			}
		}
	}
	assert.True(t, foundHist, "expected %s OTel histogram", trigger.MetricTriggerRuleEval)
}

func TestDispatcher_CorruptStoredMatchRules_SkipsAndNeverFires(t *testing.T) {
	ctx := context.Background()
	ms := newMemTriggerStore()
	core := &fakeCore{}
	core.enabled.Store(true)
	_, rules := newTestTriggerEngineWithDialects(t)

	projID := uuid.NewString()
	epID := uuid.NewString()
	evID := uuid.NewString()
	t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

	trCorruptFlag := &store.Trigger{
		ID:                uuid.NewString(),
		ProjectID:         projID,
		Name:              "corrupt-json",
		SourceKind:        store.TriggerSourceKindWebhook,
		SourceID:          epID,
		MatchTypes:        []string{"webhook.received"},
		MatchRulesCorrupt: true,
		ActionType:        store.TriggerActionMessage,
		Status:            store.TriggerStatusActive,
	}
	trEmptyElem := &store.Trigger{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		Name:       "corrupt-empty-elem",
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		MatchTypes: []string{"webhook.received"},
		MatchRules: []store.TriggerMatchRule{{Dialect: "cel", Expr: "   "}},
		ActionType: store.TriggerActionMessage,
		Status:     store.TriggerStatusActive,
	}
	nineElems := make([]store.TriggerMatchRule, triggerrule.MaxMatchRules+1)
	for i := range nineElems {
		nineElems[i] = store.TriggerMatchRule{Dialect: "cel", Expr: "true"}
	}
	trTooMany := &store.Trigger{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		Name:       "corrupt-9-elems",
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		MatchTypes: []string{"webhook.received"},
		MatchRules: nineElems,
		ActionType: store.TriggerActionMessage,
		Status:     store.TriggerStatusActive,
	}
	trGood := &store.Trigger{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		Name:       "healthy-neighbor",
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		MatchTypes: []string{"webhook.received"},
		MatchRules: []store.TriggerMatchRule{{Dialect: "cel", Expr: "true"}},
		ActionType: store.TriggerActionMessage,
		Status:     store.TriggerStatusActive,
	}

	for _, tr := range []*store.Trigger{trCorruptFlag, trEmptyElem, trTooMany, trGood} {
		require.NoError(t, ms.CreateTrigger(ctx, tr))
	}
	require.NoError(t, ms.CreateTriggerEvent(ctx, &store.TriggerEvent{
		ID:         evID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		Type:       "webhook.received",
		Subject:    "webhook:" + epID,
		DedupKey:   "d-corrupt",
		OccurredAt: t0,
		ReceivedAt: t0,
		Payload:    `{"ok":true}`,
		Status:     store.TriggerEventStatusPending,
	}))

	var firedIDs []string
	core.fireFn = func(_ context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
		firedIDs = append(firedIDs, req.Authority.DefinitionID)
		return hubfire.FireOutcome{Status: hubfire.FireStatusFired}
	}

	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:  core,
		Store: ms,
		Rules: rules,
		Now:   func() time.Time { return t0 },
	})
	require.NoError(t, err)

	raw, _ := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: evID})
	res := disp.HandleItem(ctx, durablequeue.Item{
		Kind:     trigger.EventQueueKind,
		DedupKey: evID,
		Payload:  raw,
	}, nopLease{})
	require.NoError(t, res.Err)
	assert.True(t, res.Done)

	// SEC-3b1rw-01: corrupt triggers NEVER fire; healthy neighbor still fires.
	assert.Equal(t, []string{trGood.ID}, firedIDs)

	firesRes, err := ms.ListTriggerFires(ctx, store.TriggerFireFilter{}, store.ListOptions{})
	require.NoError(t, err)
	byTrig := make(map[string]store.TriggerFire, len(firesRes.Items))
	for _, f := range firesRes.Items {
		byTrig[f.TriggerID] = f
	}
	for _, tc := range []struct {
		id         string
		wantReason string
	}{
		{trCorruptFlag.ID, triggerrule.MatchReasonCorruptRules},
		{trEmptyElem.ID, triggerrule.MatchReasonCorruptRules},
		{trTooMany.ID, celengine.CategorySize},
	} {
		f, ok := byTrig[tc.id]
		require.True(t, ok, "expected skipped fire row for corrupt trigger %s", tc.id)
		assert.Equal(t, store.TriggerFireStatusSkipped, f.Status)
		assert.Equal(t, triggerrule.SkipReasonEvalFailed, f.Error)
		assert.Contains(t, f.RuleTrace, tc.wantReason)
	}
}

func TestDispatcher_SharedEventBudgetAlwaysPassedAndClampsAcrossTriggers(t *testing.T) {
	ctx := context.Background()
	ms := newMemTriggerStore()
	core := &fakeCore{}
	core.enabled.Store(true)

	slowDialect := &testDispatchDialect{
		name:        "slow_probe",
		maxEvalTime: 500 * time.Millisecond,
		fn: func(evalCtx context.Context, rule string, _ ruleengine.Budget) (ruleengine.Decision, error) {
			switch rule {
			case "spend_45ms":
				time.Sleep(45 * time.Millisecond)
				return ruleengine.Decision{Kind: ruleengine.KindFilter, Bool: true}, nil
			case "block_until_deadline":
				<-evalCtx.Done()
				return ruleengine.Decision{}, evalCtx.Err()
			default:
				return ruleengine.Decision{Kind: ruleengine.KindFilter, Bool: true}, nil
			}
		},
	}
	_, rules := newTestTriggerEngineWithDialects(t, slowDialect)

	projID := uuid.NewString()
	epID := uuid.NewString()
	evID := uuid.NewString()
	t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

	tr1 := &store.Trigger{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		Name:       "tr-1-spends-part",
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		MatchTypes: []string{"webhook.received"},
		MatchRules: []store.TriggerMatchRule{{Dialect: "slow_probe", Expr: "spend_45ms"}},
		ActionType: store.TriggerActionMessage,
		Status:     store.TriggerStatusActive,
		CreatedAt:  t0,
	}
	tr2 := &store.Trigger{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		Name:       "tr-2-exhausts-remainder",
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		MatchTypes: []string{"webhook.received"},
		MatchRules: []store.TriggerMatchRule{
			{Dialect: "slow_probe", Expr: "block_until_deadline"},
			{Dialect: "slow_probe", Expr: "never_reached"},
		},
		ActionType: store.TriggerActionMessage,
		Status:     store.TriggerStatusActive,
		CreatedAt:  t0.Add(time.Second),
	}
	tr3 := &store.Trigger{
		ID:         uuid.NewString(),
		ProjectID:  projID,
		Name:       "tr-3-pre-exhausted",
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		MatchTypes: []string{"webhook.received"},
		MatchRules: []store.TriggerMatchRule{{Dialect: "slow_probe", Expr: "never_started"}},
		ActionType: store.TriggerActionMessage,
		Status:     store.TriggerStatusActive,
		CreatedAt:  t0.Add(2 * time.Second),
	}
	for _, tr := range []*store.Trigger{tr1, tr2, tr3} {
		require.NoError(t, ms.CreateTrigger(ctx, tr))
	}
	require.NoError(t, ms.CreateTriggerEvent(ctx, &store.TriggerEvent{
		ID:         evID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		Type:       "webhook.received",
		Subject:    "webhook:" + epID,
		DedupKey:   "d-shared-budget",
		OccurredAt: t0,
		ReceivedAt: t0,
		Payload:    `{"ok":true}`,
		Status:     store.TriggerEventStatusPending,
	}))

	const eventBudget = 80 * time.Millisecond
	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:       core,
		Store:      ms,
		Rules:      rules,
		RuleBudget: eventBudget,
		Now:        func() time.Time { return t0 },
	})
	require.NoError(t, err)

	raw, _ := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: evID})
	res := disp.HandleItem(ctx, durablequeue.Item{
		Kind:     trigger.EventQueueKind,
		DedupKey: evID,
		Payload:  raw,
	}, nopLease{})
	require.NoError(t, res.Err)
	assert.True(t, res.Done)

	// SEC-3b1rw-01 & design §8 item 4: WithBudget is always passed and clamps each
	// element's deadline to the remaining aggregate event budget (< 500ms dialect MaxEvalTime).
	require.Len(t, slowDialect.evalCalls, 2, "tr3 and element 2 of tr2 must not execute once budget is exhausted")
	assert.Equal(t, []bool{true, true}, slowDialect.hasTrackers, "dispatcher must always pass WithBudget tracker")
	assert.LessOrEqual(t, slowDialect.maxBudgets[0], eventBudget)
	assert.Less(t, slowDialect.maxBudgets[1], slowDialect.maxBudgets[0], "second trigger's element deadline must clamp to smaller remainder")

	firesRes, err := ms.ListTriggerFires(ctx, store.TriggerFireFilter{}, store.ListOptions{})
	require.NoError(t, err)
	byTrig := make(map[string]store.TriggerFire, len(firesRes.Items))
	for _, f := range firesRes.Items {
		byTrig[f.TriggerID] = f
	}
	assert.Equal(t, store.TriggerFireStatusFired, byTrig[tr1.ID].Status)
	assert.Equal(t, "1/1", byTrig[tr1.ID].MatchedThrough)

	assert.Equal(t, store.TriggerFireStatusSkipped, byTrig[tr2.ID].Status)
	assert.Equal(t, triggerrule.SkipReasonBudgetExhausted, byTrig[tr2.ID].Error)
	assert.Equal(t, "1/2", byTrig[tr2.ID].MatchedThrough)

	assert.Equal(t, store.TriggerFireStatusSkipped, byTrig[tr3.ID].Status)
	assert.Equal(t, triggerrule.SkipReasonBudgetExhausted, byTrig[tr3.ID].Error)
	assert.Equal(t, "1/1", byTrig[tr3.ID].MatchedThrough)
}

func TestDispatcher_UngrantedStoredDialectAtEval_SkipsFailClosed(t *testing.T) {
	ctx := context.Background()
	ms := newMemTriggerStore()
	core := &fakeCore{}
	core.enabled.Store(true)

	tempDialect := &testDispatchDialect{name: "temp_dialect"}
	re, rules := newTestTriggerEngineWithDialects(t, tempDialect)

	// Narrow consumer "trigger" back to Grant("cel") only before evaluation (§8 item 7, SEC-3b1rw-03).
	require.NoError(t, re.AllowConsumerDialects(triggerrule.ConsumerName, ruleengine.Grant(ruleengine.DialectCEL)))

	projID := uuid.NewString()
	epID := uuid.NewString()
	trID := uuid.NewString()
	evID := uuid.NewString()
	t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

	require.NoError(t, ms.CreateTrigger(ctx, &store.Trigger{
		ID:         trID,
		ProjectID:  projID,
		Name:       "ungranted-tr",
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		MatchTypes: []string{"webhook.received"},
		MatchRules: []store.TriggerMatchRule{
			{Dialect: "cel", Expr: "true"},
			{Dialect: "temp_dialect", Expr: "allow"},
		},
		ActionType: store.TriggerActionMessage,
		Status:     store.TriggerStatusActive,
	}))
	require.NoError(t, ms.CreateTriggerEvent(ctx, &store.TriggerEvent{
		ID:         evID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		Type:       "webhook.received",
		Subject:    "webhook:" + epID,
		DedupKey:   "d-ungranted",
		OccurredAt: t0,
		ReceivedAt: t0,
		Payload:    `{}`,
		Status:     store.TriggerEventStatusPending,
	}))

	var fired bool
	core.fireFn = func(_ context.Context, _ hubfire.FireRequest) hubfire.FireOutcome {
		fired = true
		return hubfire.FireOutcome{Status: hubfire.FireStatusFired}
	}

	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:  core,
		Store: ms,
		Rules: rules,
		Now:   func() time.Time { return t0 },
	})
	require.NoError(t, err)

	raw, _ := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: evID})
	res := disp.HandleItem(ctx, durablequeue.Item{
		Kind:     trigger.EventQueueKind,
		DedupKey: evID,
		Payload:  raw,
	}, nopLease{})
	require.NoError(t, res.Err)
	assert.True(t, res.Done)
	assert.False(t, fired, "un-granted dialect at evaluation must never fire")
	assert.Empty(t, tempDialect.evalCalls)

	firesRes, err := ms.ListTriggerFires(ctx, store.TriggerFireFilter{}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, firesRes.Items, 1)
	assert.Equal(t, store.TriggerFireStatusSkipped, firesRes.Items[0].Status)
	assert.Equal(t, triggerrule.SkipReasonEvalFailed, firesRes.Items[0].Error)
	assert.Equal(t, "2/2", firesRes.Items[0].MatchedThrough)
	assert.Equal(t, "temp_dialect", firesRes.Items[0].DecidedDialect)
}

func TestDispatcher_SubjectRuleDerivationAndFallbacks(t *testing.T) {
	ctx := context.Background()
	ms := newMemTriggerStore()
	core := &fakeCore{}
	core.enabled.Store(true)
	_, rules := newTestTriggerEngineWithDialects(t)

	projID := uuid.NewString()
	epID := uuid.NewString()
	evID := uuid.NewString()
	t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	longSourceSubj := "webhook:" + epID + "/src-" + strings.Repeat("z", 300)

	trValidSubj := &store.Trigger{
		ID:             uuid.NewString(),
		ProjectID:      projID,
		Name:           "tr-valid-subj",
		SourceKind:     store.TriggerSourceKindWebhook,
		SourceID:       epID,
		MatchTypes:     []string{"webhook.received"},
		MatchRules:     []store.TriggerMatchRule{{Dialect: "cel", Expr: `has(event.payload.service)`}},
		SubjectDialect: "cel",
		SubjectRule:    `"svc:" + string(event.payload.service)`,
		SubjectMode:    store.TriggerSubjectModeNew,
		ActionType:     store.TriggerActionMessage,
		Status:         store.TriggerStatusActive,
	}
	trEvalErrSubj := &store.Trigger{
		ID:             uuid.NewString(),
		ProjectID:      projID,
		Name:           "tr-err-subj",
		SourceKind:     store.TriggerSourceKindWebhook,
		SourceID:       epID,
		MatchTypes:     []string{"webhook.received"},
		MatchRules:     []store.TriggerMatchRule{{Dialect: "cel", Expr: "true"}},
		SubjectDialect: "cel",
		SubjectRule:    `string(event.payload.nonexistent_field)`,
		SubjectMode:    store.TriggerSubjectModeNew,
		ActionType:     store.TriggerActionMessage,
		Status:         store.TriggerStatusActive,
	}
	trOversizeSubj := &store.Trigger{
		ID:             uuid.NewString(),
		ProjectID:      projID,
		Name:           "tr-oversize-subj",
		SourceKind:     store.TriggerSourceKindWebhook,
		SourceID:       epID,
		MatchTypes:     []string{"webhook.received"},
		MatchRules:     []store.TriggerMatchRule{{Dialect: "cel", Expr: "true"}},
		SubjectDialect: "cel",
		SubjectRule:    `string(event.payload.big)`,
		SubjectMode:    store.TriggerSubjectModeNew,
		ActionType:     store.TriggerActionMessage,
		Status:         store.TriggerStatusActive,
	}
	trControlSubj := &store.Trigger{
		ID:             uuid.NewString(),
		ProjectID:      projID,
		Name:           "tr-ctrl-subj",
		SourceKind:     store.TriggerSourceKindWebhook,
		SourceID:       epID,
		MatchTypes:     []string{"webhook.received"},
		MatchRules:     []store.TriggerMatchRule{{Dialect: "cel", Expr: "true"}},
		SubjectDialect: "cel",
		SubjectRule:    `"bad\nline"`,
		SubjectMode:    store.TriggerSubjectModeNew,
		ActionType:     store.TriggerActionMessage,
		Status:         store.TriggerStatusActive,
	}

	for _, tr := range []*store.Trigger{trValidSubj, trEvalErrSubj, trOversizeSubj, trControlSubj} {
		require.NoError(t, ms.CreateTrigger(ctx, tr))
	}
	require.NoError(t, ms.CreateTriggerEvent(ctx, &store.TriggerEvent{
		ID:         evID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		Type:       "webhook.received",
		Subject:    longSourceSubj,
		DedupKey:   "d-subj",
		OccurredAt: t0,
		ReceivedAt: t0,
		Payload:    `{"service":"payments","big":"` + strings.Repeat("b", 300) + `"}`,
		Status:     store.TriggerEventStatusPending,
	}))

	deliveredSubj := make(map[string]string)
	core.fireFn = func(_ context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
		deliveredSubj[req.Authority.DefinitionID] = req.Event.Subject
		return hubfire.FireOutcome{Status: hubfire.FireStatusFired}
	}

	var logBuf bytes.Buffer
	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:   core,
		Store:  ms,
		Rules:  rules,
		Logger: slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})),
		Now:    func() time.Time { return t0 },
	})
	require.NoError(t, err)

	raw, _ := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: evID})
	res := disp.HandleItem(ctx, durablequeue.Item{
		Kind:     trigger.EventQueueKind,
		DedupKey: evID,
		Payload:  raw,
	}, nopLease{})
	require.NoError(t, res.Err)
	assert.True(t, res.Done)

	firesRes, err := ms.ListTriggerFires(ctx, store.TriggerFireFilter{}, store.ListOptions{})
	require.NoError(t, err)
	byTrig := make(map[string]store.TriggerFire, len(firesRes.Items))
	for _, f := range firesRes.Items {
		byTrig[f.TriggerID] = f
	}
	require.Len(t, byTrig, 4)

	// Valid subject_rule derives trigger_fires.subject while keeping FireEvent.Subject
	// as the unchanged hub-shaped source event subject (stage1-design rev 8.34, L23).
	assert.Equal(t, "svc:payments", byTrig[trValidSubj.ID].Subject)
	assert.Equal(t, longSourceSubj, deliveredSubj[trValidSubj.ID])

	// Fallbacks preserve 256B truncation on trigger_fires.subject and full source subject on FireEvent.Subject.
	for _, fbID := range []string{trEvalErrSubj.ID, trOversizeSubj.ID, trControlSubj.ID} {
		assert.Equal(t, longSourceSubj[:trigger.MaxFireSubjectBytes], byTrig[fbID].Subject)
		assert.Equal(t, longSourceSubj, deliveredSubj[fbID])
		assert.Contains(t, logBuf.String(), fbID, "subject fallback must log warning for %s", fbID)
	}
}

func TestDispatcher_LegacyScalarMatchRuleAndEmptyRulesBehavior(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

	t.Run("nil_engine_skips_legacy_scalar_and_fires_empty_rules", func(t *testing.T) {
		ms := newMemTriggerStore()
		var firedIDs []string
		core := &fakeCore{
			fireFn: func(_ context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
				firedIDs = append(firedIDs, req.Authority.DefinitionID)
				return hubfire.FireOutcome{Status: hubfire.FireStatusFired}
			},
		}
		core.enabled.Store(true)

		projID := uuid.NewString()
		epID := uuid.NewString()
		evID := uuid.NewString()

		trLegacy := &store.Trigger{
			ID:           uuid.NewString(),
			ProjectID:    projID,
			Name:         "legacy-scalar-no-engine",
			SourceKind:   store.TriggerSourceKindWebhook,
			SourceID:     epID,
			MatchTypes:   []string{"webhook.received"},
			MatchDialect: store.TriggerDialectCEL,
			MatchRule:    `event.type == "webhook.received"`,
			ActionType:   store.TriggerActionMessage,
			Status:       store.TriggerStatusActive,
		}
		trEmpty := &store.Trigger{
			ID:         uuid.NewString(),
			ProjectID:  projID,
			Name:       "empty-rules-no-engine",
			SourceKind: store.TriggerSourceKindWebhook,
			SourceID:   epID,
			MatchTypes: []string{"webhook.received"},
			ActionType: store.TriggerActionMessage,
			Status:     store.TriggerStatusActive,
		}
		require.NoError(t, ms.CreateTrigger(ctx, trLegacy))
		require.NoError(t, ms.CreateTrigger(ctx, trEmpty))
		require.NoError(t, ms.CreateTriggerEvent(ctx, &store.TriggerEvent{
			ID:         evID,
			ProjectID:  projID,
			SourceKind: store.TriggerSourceKindWebhook,
			SourceID:   epID,
			Type:       "webhook.received",
			Subject:    "webhook:" + epID,
			DedupKey:   "d-legacy-nil",
			OccurredAt: t0,
			ReceivedAt: t0,
			Payload:    `{"ok":true}`,
			Status:     store.TriggerEventStatusPending,
		}))

		disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
			Core:  core,
			Store: ms,
			Now:   func() time.Time { return t0 },
		})
		require.NoError(t, err)

		raw, _ := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: evID})
		res := disp.HandleItem(ctx, durablequeue.Item{
			Kind:     trigger.EventQueueKind,
			DedupKey: evID,
			Payload:  raw,
		}, nopLease{})
		require.NoError(t, res.Err)
		assert.True(t, res.Done)
		assert.Equal(t, []string{trEmpty.ID}, firedIDs)

		fires, err := ms.ListTriggerFires(ctx, store.TriggerFireFilter{}, store.ListOptions{})
		require.NoError(t, err)
		byTrig := make(map[string]store.TriggerFire, len(fires.Items))
		for _, f := range fires.Items {
			byTrig[f.TriggerID] = f
		}
		assert.Equal(t, store.TriggerFireStatusSkipped, byTrig[trLegacy.ID].Status)
		assert.Equal(t, trigger.ErrFireRuleEvaluationFailed, byTrig[trLegacy.ID].Error)
		assert.Equal(t, store.TriggerFireStatusFired, byTrig[trEmpty.ID].Status)
	})

	t.Run("engine_skips_whitespace_legacy_scalar_and_evaluates_valid_legacy_scalar", func(t *testing.T) {
		ms := newMemTriggerStore()
		_, rules := newTestTriggerEngineWithDialects(t)
		var firedIDs []string
		core := &fakeCore{
			fireFn: func(_ context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
				firedIDs = append(firedIDs, req.Authority.DefinitionID)
				return hubfire.FireOutcome{Status: hubfire.FireStatusFired}
			},
		}
		core.enabled.Store(true)

		projID := uuid.NewString()
		epID := uuid.NewString()
		evID := uuid.NewString()

		trWhitespaceLegacy := &store.Trigger{
			ID:           uuid.NewString(),
			ProjectID:    projID,
			Name:         "legacy-scalar-whitespace",
			SourceKind:   store.TriggerSourceKindWebhook,
			SourceID:     epID,
			MatchTypes:   []string{"webhook.received"},
			MatchDialect: store.TriggerDialectCEL,
			MatchRule:    "   ",
			ActionType:   store.TriggerActionMessage,
			Status:       store.TriggerStatusActive,
		}
		trValidLegacy := &store.Trigger{
			ID:           uuid.NewString(),
			ProjectID:    projID,
			Name:         "legacy-scalar-valid",
			SourceKind:   store.TriggerSourceKindWebhook,
			SourceID:     epID,
			MatchTypes:   []string{"webhook.received"},
			MatchDialect: store.TriggerDialectCEL,
			MatchRule:    `event.type == "webhook.received"`,
			ActionType:   store.TriggerActionMessage,
			Status:       store.TriggerStatusActive,
		}
		require.NoError(t, ms.CreateTrigger(ctx, trWhitespaceLegacy))
		require.NoError(t, ms.CreateTrigger(ctx, trValidLegacy))
		require.NoError(t, ms.CreateTriggerEvent(ctx, &store.TriggerEvent{
			ID:         evID,
			ProjectID:  projID,
			SourceKind: store.TriggerSourceKindWebhook,
			SourceID:   epID,
			Type:       "webhook.received",
			Subject:    "webhook:" + epID,
			DedupKey:   "d-legacy-eval",
			OccurredAt: t0,
			ReceivedAt: t0,
			Payload:    `{"ok":true}`,
			Status:     store.TriggerEventStatusPending,
		}))

		disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
			Core:  core,
			Store: ms,
			Rules: rules,
			Now:   func() time.Time { return t0 },
		})
		require.NoError(t, err)

		raw, _ := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: evID})
		res := disp.HandleItem(ctx, durablequeue.Item{
			Kind:     trigger.EventQueueKind,
			DedupKey: evID,
			Payload:  raw,
		}, nopLease{})
		require.NoError(t, res.Err)
		assert.True(t, res.Done)
		assert.Equal(t, []string{trValidLegacy.ID}, firedIDs)

		fires, err := ms.ListTriggerFires(ctx, store.TriggerFireFilter{}, store.ListOptions{})
		require.NoError(t, err)
		byTrig := make(map[string]store.TriggerFire, len(fires.Items))
		for _, f := range fires.Items {
			byTrig[f.TriggerID] = f
		}
		assert.Equal(t, store.TriggerFireStatusSkipped, byTrig[trWhitespaceLegacy.ID].Status)
		assert.Equal(t, trigger.ErrFireRuleEvaluationFailed, byTrig[trWhitespaceLegacy.ID].Error)
		assert.Equal(t, store.TriggerFireStatusFired, byTrig[trValidLegacy.ID].Status)
	})
}

func TestDispatcher_GitHubCandidateQueryMatchesEmptyAndExactSourceIDOnlyForGitHub(t *testing.T) {
	ms := newMemTriggerStore()
	now := time.Date(2026, 11, 18, 21, 0, 0, 0, time.UTC)

	var firedTriggerIDs []string
	var mu sync.Mutex
	core := &fakeCore{
		fireFn: func(_ context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
			mu.Lock()
			firedTriggerIDs = append(firedTriggerIDs, req.Authority.DefinitionID)
			mu.Unlock()
			return hubfire.FireOutcome{Status: hubfire.FireStatusFired}
		},
	}
	core.enabled.Store(true)

	projID := uuid.NewString()
	ghWildcard := uuid.NewString()
	ghExact := uuid.NewString()
	ghOtherInst := uuid.NewString()
	whWildcard := uuid.NewString()
	whExact := uuid.NewString()
	epID := uuid.NewString()

	for _, tc := range []struct {
		id, name, kind, sourceID, matchType string
	}{
		{ghWildcard, "gh-wildcard", store.TriggerSourceKindGitHub, "", "github.issue_comment.created"},
		{ghExact, "gh-exact", store.TriggerSourceKindGitHub, "424242", "github.*"},
		{ghOtherInst, "gh-other-inst", store.TriggerSourceKindGitHub, "999999", "github.*"},
		{whWildcard, "wh-empty-id", store.TriggerSourceKindWebhook, "", "webhook.*"},
		{whExact, "wh-exact", store.TriggerSourceKindWebhook, epID, "webhook.*"},
	} {
		require.NoError(t, ms.CreateTrigger(context.Background(), &store.Trigger{
			ID:           tc.id,
			ProjectID:    projID,
			Name:         tc.name,
			SourceKind:   tc.kind,
			SourceID:     tc.sourceID,
			MatchTypes:   []string{tc.matchType},
			MatchDialect: store.TriggerDialectCEL,
			ActionType:   store.TriggerActionMessage,
			SubjectMode:  store.TriggerSubjectModeNew,
			Status:       store.TriggerStatusActive,
		}))
	}

	ghEvID := uuid.NewString()
	require.NoError(t, ms.CreateTriggerEvent(context.Background(), &store.TriggerEvent{
		ID:         ghEvID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindGitHub,
		SourceID:   "424242",
		Type:       "github.issue_comment.created",
		Subject:    "github:acme/widgets#7",
		DedupKey:   "d-gh",
		OccurredAt: now,
		ReceivedAt: now,
		Payload:    `{"action":"created"}`,
		Status:     store.TriggerEventStatusPending,
	}))

	whEvID := uuid.NewString()
	require.NoError(t, ms.CreateTriggerEvent(context.Background(), &store.TriggerEvent{
		ID:         whEvID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		Type:       "webhook.received",
		Subject:    "webhook:" + epID,
		DedupKey:   "d-wh",
		OccurredAt: now,
		ReceivedAt: now,
		Payload:    `{}`,
		Status:     store.TriggerEventStatusPending,
	}))

	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:  core,
		Store: ms,
		Now:   func() time.Time { return now },
	})
	require.NoError(t, err)

	ghRaw, _ := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: ghEvID})
	res := disp.HandleItem(context.Background(), durablequeue.Item{
		Kind:     trigger.EventQueueKind,
		DedupKey: ghEvID,
		Payload:  ghRaw,
	}, nopLease{})
	assert.True(t, res.Done)
	assert.ElementsMatch(t, []string{ghWildcard, ghExact}, firedTriggerIDs)

	firedTriggerIDs = nil
	whRaw, _ := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: whEvID})
	res = disp.HandleItem(context.Background(), durablequeue.Item{
		Kind:     trigger.EventQueueKind,
		DedupKey: whEvID,
		Payload:  whRaw,
	}, nopLease{})
	assert.True(t, res.Done)
	assert.Equal(t, []string{whExact}, firedTriggerIDs, "webhook lookup must remain exact source_id equality")
}

func TestDispatcher_GitHubSourceAuthorizerRefusesAllMatchedCandidatesOnce(t *testing.T) {
	ms := newMemTriggerStore()
	now := time.Date(2026, 11, 18, 21, 5, 0, 0, time.UTC)

	var (
		authCalls atomic.Int32
		fireCalls atomic.Int32
	)
	core := &fakeCore{
		authorizeSourceFn: func(_ context.Context, _ *store.TriggerEvent) (bool, error) {
			authCalls.Add(1)
			return false, nil
		},
		fireFn: func(_ context.Context, _ hubfire.FireRequest) hubfire.FireOutcome {
			fireCalls.Add(1)
			return hubfire.FireOutcome{Status: hubfire.FireStatusFired}
		},
	}
	core.enabled.Store(true)

	projID := uuid.NewString()
	trMatch1 := uuid.NewString()
	trMatch2 := uuid.NewString()
	trNonMatch := uuid.NewString()

	for _, tc := range []struct {
		id, name, matchType string
	}{
		{trMatch1, "gh-match-1", "github.issue_comment.created"},
		{trMatch2, "gh-match-2", "github.issue_comment.*"},
		{trNonMatch, "gh-nonmatch", "github.pull_request.opened"},
	} {
		require.NoError(t, ms.CreateTrigger(context.Background(), &store.Trigger{
			ID:           tc.id,
			ProjectID:    projID,
			Name:         tc.name,
			SourceKind:   store.TriggerSourceKindGitHub,
			SourceID:     "",
			MatchTypes:   []string{tc.matchType},
			MatchDialect: store.TriggerDialectCEL,
			ActionType:   store.TriggerActionMessage,
			SubjectMode:  store.TriggerSubjectModeNew,
			Status:       store.TriggerStatusActive,
		}))
	}

	// Event 1: no rule matches -> AuthorizeEventSource is not called.
	unmatchedEvID := uuid.NewString()
	require.NoError(t, ms.CreateTriggerEvent(context.Background(), &store.TriggerEvent{
		ID:         unmatchedEvID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindGitHub,
		SourceID:   "424242",
		Type:       "github.issues.closed",
		Subject:    "github:acme/widgets#1",
		DedupKey:   "d-unmatched",
		OccurredAt: now,
		ReceivedAt: now,
		Payload:    `{}`,
		Status:     store.TriggerEventStatusPending,
	}))

	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:  core,
		Store: ms,
		Now:   func() time.Time { return now },
	})
	require.NoError(t, err)

	rawUnmatched, _ := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: unmatchedEvID})
	res := disp.HandleItem(context.Background(), durablequeue.Item{
		Kind:     trigger.EventQueueKind,
		DedupKey: unmatchedEvID,
		Payload:  rawUnmatched,
	}, nopLease{})
	require.True(t, res.Done)
	assert.Zero(t, authCalls.Load(), "unmatched event should not check source authorization")
	assert.Zero(t, fireCalls.Load())

	// Event 2: two rules match -> AuthorizeEventSource runs once, refuses both matched rules, marks event processed.
	evID := uuid.NewString()
	require.NoError(t, ms.CreateTriggerEvent(context.Background(), &store.TriggerEvent{
		ID:         evID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindGitHub,
		SourceID:   "424242",
		Type:       "github.issue_comment.created",
		Subject:    "github:acme/widgets#12",
		DedupKey:   "d-refused",
		OccurredAt: now,
		ReceivedAt: now,
		Payload:    `{"action":"created"}`,
		Status:     store.TriggerEventStatusPending,
	}))
	rawEv, _ := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: evID})
	res = disp.HandleItem(context.Background(), durablequeue.Item{
		Kind:     trigger.EventQueueKind,
		DedupKey: evID,
		Payload:  rawEv,
	}, nopLease{})
	require.True(t, res.Done)
	assert.Equal(t, int32(1), authCalls.Load(), "source authorization runs once per event")
	assert.Zero(t, fireCalls.Load(), "Core.Fire must never run when association is unauthorized")

	ev, err := ms.GetTriggerEvent(context.Background(), evID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerEventStatusProcessed, ev.Status)
	assert.Equal(t, 2, ev.MatchCount)
	assert.Equal(t, 0, ev.FireCount)

	fires, err := ms.ListTriggerFires(context.Background(), store.TriggerFireFilter{}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, fires.Items, 2)
	gotTrIDs := make([]string, 0, 2)
	for _, f := range fires.Items {
		gotTrIDs = append(gotTrIDs, f.TriggerID)
		assert.Equal(t, store.TriggerFireStatusRefused, f.Status)
		assert.Equal(t, trigger.ErrFireGitHubAssociationUnauthorized, f.Error)
		assert.NotNil(t, f.FinishedAt)
	}
	assert.ElementsMatch(t, []string{trMatch1, trMatch2}, gotTrIDs)

	for _, id := range []string{trMatch1, trMatch2} {
		tr, gErr := ms.GetTrigger(context.Background(), id)
		require.NoError(t, gErr)
		assert.Equal(t, 1, tr.FireCount)
		assert.Equal(t, 1, tr.ErrorCount)
		assert.Equal(t, store.TriggerFireStatusRefused, tr.LastFireStatus)
		assert.Equal(t, trigger.ErrFireGitHubAssociationUnauthorized, tr.LastFireError)
	}
}

func TestDispatcher_GitHubEventWaitsWhenGitHubTriggersExperimentDisabled(t *testing.T) {
	ms := newMemTriggerStore()
	now := time.Date(2026, 11, 18, 21, 10, 0, 0, time.UTC)

	core := &fakeCore{}
	core.enabled.Store(true)
	core.githubDisabled.Store(true)

	projID := uuid.NewString()
	trID := uuid.NewString()
	evID := uuid.NewString()
	require.NoError(t, ms.CreateTrigger(context.Background(), &store.Trigger{
		ID:           trID,
		ProjectID:    projID,
		Name:         "gh-wait",
		SourceKind:   store.TriggerSourceKindGitHub,
		SourceID:     "",
		MatchTypes:   []string{"github.*"},
		MatchDialect: store.TriggerDialectCEL,
		ActionType:   store.TriggerActionMessage,
		SubjectMode:  store.TriggerSubjectModeNew,
		Status:       store.TriggerStatusActive,
	}))
	require.NoError(t, ms.CreateTriggerEvent(context.Background(), &store.TriggerEvent{
		ID:         evID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindGitHub,
		SourceID:   "424242",
		Type:       "github.issue_comment.created",
		Subject:    "github:acme/widgets#99",
		DedupKey:   "d-wait",
		OccurredAt: now,
		ReceivedAt: now,
		Payload:    `{}`,
		Status:     store.TriggerEventStatusPending,
	}))

	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:  core,
		Store: ms,
		Now:   func() time.Time { return now },
	})
	require.NoError(t, err)

	raw, _ := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: evID})
	res := disp.HandleItem(context.Background(), durablequeue.Item{
		Kind:     trigger.EventQueueKind,
		DedupKey: evID,
		Payload:  raw,
	}, nopLease{})
	assert.False(t, res.Done)
	assert.False(t, res.Permanent, "disabled github_triggers experiment must defer, not poison")
	assert.Error(t, res.Err)

	ev, err := ms.GetTriggerEvent(context.Background(), evID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerEventStatusPending, ev.Status)

	// Re-enabling hub.github_triggers processes the queued event.
	core.githubDisabled.Store(false)
	res = disp.HandleItem(context.Background(), durablequeue.Item{
		Kind:     trigger.EventQueueKind,
		DedupKey: evID,
		Payload:  raw,
	}, nopLease{})
	assert.True(t, res.Done)
	ev, err = ms.GetTriggerEvent(context.Background(), evID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerEventStatusProcessed, ev.Status)
	assert.Equal(t, 1, ev.FireCount)
}

func TestDispatcher_GitHubSourceAuthorizerTransientErrorLeavesPendingAndRetries(t *testing.T) {
	ms := newMemTriggerStore()
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)

	core := &fakeCore{}
	core.enabled.Store(true)
	var transientFail atomic.Bool
	transientFail.Store(true)
	var authCalls atomic.Int32
	core.authorizeSourceFn = func(_ context.Context, _ *store.TriggerEvent) (bool, error) {
		authCalls.Add(1)
		if transientFail.Load() {
			return false, errors.New("github 502 bad gateway")
		}
		return true, nil
	}

	projID := uuid.NewString()
	trID := uuid.NewString()
	evID := uuid.NewString()
	require.NoError(t, ms.CreateTrigger(context.Background(), &store.Trigger{
		ID:           trID,
		ProjectID:    projID,
		Name:         "gh-transient",
		SourceKind:   store.TriggerSourceKindGitHub,
		SourceID:     "",
		MatchTypes:   []string{"github.issue_comment.created"},
		MatchDialect: store.TriggerDialectCEL,
		ActionType:   store.TriggerActionMessage,
		SubjectMode:  store.TriggerSubjectModeNew,
		Status:       store.TriggerStatusActive,
	}))
	require.NoError(t, ms.CreateTriggerEvent(context.Background(), &store.TriggerEvent{
		ID:         evID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindGitHub,
		SourceID:   "777001",
		Type:       "github.issue_comment.created",
		Subject:    "github:acme/widgets#11",
		DedupKey:   "d-transient",
		OccurredAt: now,
		ReceivedAt: now,
		Payload:    `{}`,
		Status:     store.TriggerEventStatusPending,
	}))

	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:  core,
		Store: ms,
		Now:   func() time.Time { return now },
	})
	require.NoError(t, err)

	raw, _ := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: evID})

	// 1. Transient/inconclusive check error: item is NOT Done and NOT Permanent;
	// event stays pending with zero fire rows (no refused fire).
	res := disp.HandleItem(context.Background(), durablequeue.Item{
		Kind:     trigger.EventQueueKind,
		DedupKey: evID,
		Payload:  raw,
	}, nopLease{})
	assert.False(t, res.Done)
	assert.False(t, res.Permanent, "inconclusive association check must retry until MaxAge, not poison")
	assert.Error(t, res.Err)
	assert.Equal(t, int32(1), authCalls.Load())

	ev, err := ms.GetTriggerEvent(context.Background(), evID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerEventStatusPending, ev.Status)

	fires, err := ms.ListTriggerFires(context.Background(), store.TriggerFireFilter{ProjectID: projID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, fires.Items, "transient check failure must not record a refused fire")

	// 2. Retry after transient error clears: event processes and fires normally.
	transientFail.Store(false)
	res = disp.HandleItem(context.Background(), durablequeue.Item{
		Kind:     trigger.EventQueueKind,
		DedupKey: evID,
		Payload:  raw,
	}, nopLease{})
	assert.True(t, res.Done)
	assert.NoError(t, res.Err)
	assert.Equal(t, int32(2), authCalls.Load())

	ev, err = ms.GetTriggerEvent(context.Background(), evID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerEventStatusProcessed, ev.Status)
	assert.Equal(t, 1, ev.FireCount)

	fires, err = ms.ListTriggerFires(context.Background(), store.TriggerFireFilter{ProjectID: projID}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, fires.Items, 1)
	assert.Equal(t, store.TriggerFireStatusFired, fires.Items[0].Status)
}

// coreWithoutSourceAuthorizer implements trigger.Core without implementing
// trigger.SourceAuthorizer, used to verify github events fail closed.
type coreWithoutSourceAuthorizer struct {
	fireFn func(ctx context.Context, req hubfire.FireRequest) hubfire.FireOutcome
}

func (c *coreWithoutSourceAuthorizer) Fire(ctx context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
	if c.fireFn != nil {
		return c.fireFn(ctx, req)
	}
	return hubfire.FireOutcome{Status: hubfire.FireStatusFired}
}

func (c *coreWithoutSourceAuthorizer) Experiment(name string) bool {
	return name == experiments.Triggers || name == experiments.GitHubTriggers
}

func TestDispatcher_GitHubFailsClosedWithoutSourceAuthorizer(t *testing.T) {
	ms := newMemTriggerStore()
	now := time.Date(2026, 11, 20, 6, 15, 0, 0, time.UTC)

	var fireCalls atomic.Int32
	core := &coreWithoutSourceAuthorizer{
		fireFn: func(_ context.Context, _ hubfire.FireRequest) hubfire.FireOutcome {
			fireCalls.Add(1)
			return hubfire.FireOutcome{Status: hubfire.FireStatusFired}
		},
	}
	_, implementsAuthorizer := any(core).(trigger.SourceAuthorizer)
	require.False(t, implementsAuthorizer, "test core must not implement SourceAuthorizer")

	projID := uuid.NewString()
	ghTrID := uuid.NewString()
	ghEvID := uuid.NewString()
	whEpID := uuid.NewString()
	whTrID := uuid.NewString()
	whEvID := uuid.NewString()

	require.NoError(t, ms.CreateTrigger(context.Background(), &store.Trigger{
		ID:           ghTrID,
		ProjectID:    projID,
		Name:         "gh-fail-closed",
		SourceKind:   store.TriggerSourceKindGitHub,
		SourceID:     "",
		MatchTypes:   []string{"github.issue_comment.created"},
		MatchDialect: store.TriggerDialectCEL,
		ActionType:   store.TriggerActionMessage,
		SubjectMode:  store.TriggerSubjectModeNew,
		Status:       store.TriggerStatusActive,
	}))
	require.NoError(t, ms.CreateTriggerEvent(context.Background(), &store.TriggerEvent{
		ID:         ghEvID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindGitHub,
		SourceID:   "999001",
		Type:       "github.issue_comment.created",
		Subject:    "github:acme/widgets#42",
		DedupKey:   "d-no-authorizer",
		OccurredAt: now,
		ReceivedAt: now,
		Payload:    `{}`,
		Status:     store.TriggerEventStatusPending,
	}))

	require.NoError(t, ms.CreateTrigger(context.Background(), &store.Trigger{
		ID:           whTrID,
		ProjectID:    projID,
		Name:         "wh-still-fires",
		SourceKind:   store.TriggerSourceKindWebhook,
		SourceID:     whEpID,
		MatchTypes:   []string{"webhook.received"},
		MatchDialect: store.TriggerDialectCEL,
		ActionType:   store.TriggerActionMessage,
		SubjectMode:  store.TriggerSubjectModeNew,
		Status:       store.TriggerStatusActive,
	}))
	require.NoError(t, ms.CreateTriggerEvent(context.Background(), &store.TriggerEvent{
		ID:         whEvID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   whEpID,
		Type:       "webhook.received",
		Subject:    "webhook:test",
		DedupKey:   "d-wh-ok",
		OccurredAt: now,
		ReceivedAt: now,
		Payload:    `{}`,
		Status:     store.TriggerEventStatusPending,
	}))

	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:  core,
		Store: ms,
		Now:   func() time.Time { return now },
	})
	require.NoError(t, err)

	// GitHub event must fail closed as a retryable internal error without firing.
	ghRaw, _ := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: ghEvID})
	res := disp.HandleItem(context.Background(), durablequeue.Item{
		Kind:     trigger.EventQueueKind,
		DedupKey: ghEvID,
		Payload:  ghRaw,
	}, nopLease{})
	assert.False(t, res.Done)
	assert.False(t, res.Permanent)
	assert.ErrorContains(t, res.Err, "source authorizer unavailable")
	assert.Zero(t, fireCalls.Load(), "github event must never fire when Core lacks SourceAuthorizer")

	ev, err := ms.GetTriggerEvent(context.Background(), ghEvID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerEventStatusPending, ev.Status)

	fires, err := ms.ListTriggerFires(context.Background(), store.TriggerFireFilter{ProjectID: projID, TriggerID: ghTrID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, fires.Items)

	// Webhook events do not require SourceAuthorizer and still fire normally.
	whRaw, _ := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: whEvID})
	res = disp.HandleItem(context.Background(), durablequeue.Item{
		Kind:     trigger.EventQueueKind,
		DedupKey: whEvID,
		Payload:  whRaw,
	}, nopLease{})
	assert.True(t, res.Done)
	assert.NoError(t, res.Err)
	assert.Equal(t, int32(1), fireCalls.Load())
}

func TestDispatcher_GitHubSourceAuthorizationPrecedesRuleEvaluation(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 55, 0, 0, time.UTC)

	setup := func(t *testing.T, authFn func(context.Context, *store.TriggerEvent) (bool, error)) (*memTriggerStore, *trigger.Dispatcher, string, string, string, string, string, *atomic.Int32, *atomic.Int32) {
		t.Helper()
		ms := newMemTriggerStore()
		_, rules := newTestTriggerEngineWithDialects(t)
		var authCalls, fireCalls atomic.Int32
		core := &fakeCore{
			authorizeSourceFn: func(ctx context.Context, ev *store.TriggerEvent) (bool, error) {
				authCalls.Add(1)
				return authFn(ctx, ev)
			},
			fireFn: func(_ context.Context, _ hubfire.FireRequest) hubfire.FireOutcome {
				fireCalls.Add(1)
				return hubfire.FireOutcome{Status: hubfire.FireStatusFired}
			},
		}
		core.enabled.Store(true)

		projID := uuid.NewString()
		trCorruptID := uuid.NewString()
		trMatchCELID := uuid.NewString()
		trNoMatchCELID := uuid.NewString()
		evID := uuid.NewString()

		require.NoError(t, ms.CreateTrigger(context.Background(), &store.Trigger{
			ID:                trCorruptID,
			ProjectID:         projID,
			Name:              "gh-corrupt-rules",
			SourceKind:        store.TriggerSourceKindGitHub,
			SourceID:          "",
			MatchTypes:        []string{"github.issue_comment.created"},
			MatchDialect:      store.TriggerDialectCEL,
			MatchRulesCorrupt: true,
			ActionType:        store.TriggerActionMessage,
			SubjectMode:       store.TriggerSubjectModeNew,
			Status:            store.TriggerStatusActive,
		}))
		require.NoError(t, ms.CreateTrigger(context.Background(), &store.Trigger{
			ID:           trMatchCELID,
			ProjectID:    projID,
			Name:         "gh-cel-match",
			SourceKind:   store.TriggerSourceKindGitHub,
			SourceID:     "",
			MatchTypes:   []string{"github.issue_comment.created"},
			MatchDialect: store.TriggerDialectCEL,
			MatchRules:   []store.TriggerMatchRule{{Dialect: store.TriggerDialectCEL, Expr: `event.attributes.command == "review"`}},
			ActionType:   store.TriggerActionMessage,
			SubjectMode:  store.TriggerSubjectModeNew,
			Status:       store.TriggerStatusActive,
		}))
		require.NoError(t, ms.CreateTrigger(context.Background(), &store.Trigger{
			ID:           trNoMatchCELID,
			ProjectID:    projID,
			Name:         "gh-cel-nomatch",
			SourceKind:   store.TriggerSourceKindGitHub,
			SourceID:     "",
			MatchTypes:   []string{"github.issue_comment.created"},
			MatchDialect: store.TriggerDialectCEL,
			MatchRules:   []store.TriggerMatchRule{{Dialect: store.TriggerDialectCEL, Expr: `event.attributes.command == "deploy"`}},
			ActionType:   store.TriggerActionMessage,
			SubjectMode:  store.TriggerSubjectModeNew,
			Status:       store.TriggerStatusActive,
		}))
		require.NoError(t, ms.CreateTriggerEvent(context.Background(), &store.TriggerEvent{
			ID:         evID,
			ProjectID:  projID,
			SourceKind: store.TriggerSourceKindGitHub,
			SourceID:   "777123",
			Type:       "github.issue_comment.created",
			Subject:    "github:acme/widgets#55",
			DedupKey:   "d-order-" + evID,
			OccurredAt: now,
			ReceivedAt: now,
			Attributes: map[string]string{"command": "review"},
			Payload:    `{}`,
			Status:     store.TriggerEventStatusPending,
		}))

		disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
			Core:  core,
			Store: ms,
			Rules: rules,
			Now:   func() time.Time { return now },
		})
		require.NoError(t, err)
		return ms, disp, projID, trCorruptID, trMatchCELID, trNoMatchCELID, evID, &authCalls, &fireCalls
	}

	t.Run("unauthorized refuses all type-matched triggers without evaluating rules or recording rule skips", func(t *testing.T) {
		ms, disp, projID, trCorruptID, trMatchCELID, trNoMatchCELID, evID, authCalls, fireCalls := setup(t, func(context.Context, *store.TriggerEvent) (bool, error) {
			return false, nil
		})
		raw, _ := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: evID})
		res := disp.HandleItem(context.Background(), durablequeue.Item{Kind: trigger.EventQueueKind, DedupKey: evID, Payload: raw}, nopLease{})
		assert.True(t, res.Done)
		assert.NoError(t, res.Err)
		assert.Equal(t, int32(1), authCalls.Load())
		assert.Zero(t, fireCalls.Load())

		fires, err := ms.ListTriggerFires(context.Background(), store.TriggerFireFilter{ProjectID: projID}, store.ListOptions{})
		require.NoError(t, err)
		require.Len(t, fires.Items, 3)
		byTr := make(map[string]store.TriggerFire, 3)
		for _, f := range fires.Items {
			byTr[f.TriggerID] = f
		}
		for _, id := range []string{trCorruptID, trMatchCELID, trNoMatchCELID} {
			assert.Equal(t, store.TriggerFireStatusRefused, byTr[id].Status)
			assert.Equal(t, trigger.ErrFireGitHubAssociationUnauthorized, byTr[id].Error)
		}
	})

	t.Run("transient auth error retries without evaluating rules or recording rule skips", func(t *testing.T) {
		ms, disp, projID, _, _, _, evID, authCalls, fireCalls := setup(t, func(context.Context, *store.TriggerEvent) (bool, error) {
			return false, errors.New("github 503")
		})
		raw, _ := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: evID})
		res := disp.HandleItem(context.Background(), durablequeue.Item{Kind: trigger.EventQueueKind, DedupKey: evID, Payload: raw}, nopLease{})
		assert.False(t, res.Done)
		assert.False(t, res.Permanent)
		assert.Error(t, res.Err)
		assert.Equal(t, int32(1), authCalls.Load())
		assert.Zero(t, fireCalls.Load())

		ev, err := ms.GetTriggerEvent(context.Background(), evID)
		require.NoError(t, err)
		assert.Equal(t, store.TriggerEventStatusPending, ev.Status)

		fires, err := ms.ListTriggerFires(context.Background(), store.TriggerFireFilter{ProjectID: projID}, store.ListOptions{})
		require.NoError(t, err)
		assert.Empty(t, fires.Items, "retrying event must not record any rule-skip or fire row")
	})

	t.Run("authorized runs 3b-2 rule evaluation unchanged", func(t *testing.T) {
		ms, disp, projID, trCorruptID, trMatchCELID, trNoMatchCELID, evID, authCalls, fireCalls := setup(t, func(context.Context, *store.TriggerEvent) (bool, error) {
			return true, nil
		})
		raw, _ := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: evID})
		res := disp.HandleItem(context.Background(), durablequeue.Item{Kind: trigger.EventQueueKind, DedupKey: evID, Payload: raw}, nopLease{})
		assert.True(t, res.Done)
		assert.NoError(t, res.Err)
		assert.Equal(t, int32(1), authCalls.Load())
		assert.Equal(t, int32(1), fireCalls.Load())

		fires, err := ms.ListTriggerFires(context.Background(), store.TriggerFireFilter{ProjectID: projID}, store.ListOptions{})
		require.NoError(t, err)
		require.Len(t, fires.Items, 2)
		byTr := make(map[string]store.TriggerFire, 2)
		for _, f := range fires.Items {
			byTr[f.TriggerID] = f
		}
		assert.Equal(t, store.TriggerFireStatusSkipped, byTr[trCorruptID].Status)
		assert.Equal(t, trigger.ErrFireRuleEvaluationFailed, byTr[trCorruptID].Error)
		assert.Equal(t, store.TriggerFireStatusFired, byTr[trMatchCELID].Status)
		assert.Equal(t, "1/1", byTr[trMatchCELID].MatchedThrough)
		assert.Equal(t, "cel", byTr[trMatchCELID].DecidedDialect)
		_, nonMatchFired := byTr[trNoMatchCELID]
		assert.False(t, nonMatchFired, "non-matching CEL trigger must not record a fire row")
	})
}

func TestDispatcher_SubjectModeSkip_WhileWakeFireInFlightAndAfter(t *testing.T) {
	ms := newMemTriggerStore()
	core := &fakeCore{}
	core.enabled.Store(true)

	projID := uuid.NewString()
	epID := uuid.NewString()
	trSkipID := uuid.NewString()
	trNewID := uuid.NewString()
	t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

	require.NoError(t, ms.CreateTrigger(context.Background(), &store.Trigger{
		ID:            trSkipID,
		ProjectID:     projID,
		Name:          "skip-tr",
		SourceKind:    store.TriggerSourceKindWebhook,
		SourceID:      epID,
		MatchTypes:    []string{"webhook.received"},
		MatchDialect:  store.TriggerDialectCEL,
		ActionType:    store.TriggerActionMessage,
		ActionPayload: `{"agentId":"a1","message":"wake-skip","wake":true}`,
		SubjectMode:   store.TriggerSubjectModeSkip,
		Status:        store.TriggerStatusActive,
	}))
	require.NoError(t, ms.CreateTrigger(context.Background(), &store.Trigger{
		ID:            trNewID,
		ProjectID:     projID,
		Name:          "new-tr",
		SourceKind:    store.TriggerSourceKindWebhook,
		SourceID:      epID,
		MatchTypes:    []string{"webhook.received"},
		MatchDialect:  store.TriggerDialectCEL,
		ActionType:    store.TriggerActionMessage,
		ActionPayload: `{"agentId":"a1","message":"always-new"}`,
		SubjectMode:   store.TriggerSubjectModeNew,
		Status:        store.TriggerStatusActive,
	}))

	mkEvent := func(id, subject string) string {
		require.NoError(t, ms.CreateTriggerEvent(context.Background(), &store.TriggerEvent{
			ID:         id,
			ProjectID:  projID,
			SourceKind: store.TriggerSourceKindWebhook,
			SourceID:   epID,
			Type:       "webhook.received",
			Subject:    subject,
			DedupKey:   "d-" + id,
			OccurredAt: t0,
			ReceivedAt: t0,
			Payload:    `{"ok":true}`,
			Status:     store.TriggerEventStatusPending,
		}))
		return id
	}

	ev1 := mkEvent(uuid.NewString(), "webhook:"+epID+"/pr/42")
	ev2 := mkEvent(uuid.NewString(), "webhook:"+epID+"/pr/42")
	evOther := mkEvent(uuid.NewString(), "webhook:"+epID+"/pr/99")
	ev3 := mkEvent(uuid.NewString(), "webhook:"+epID+"/pr/42")

	ev1WakeEntered := make(chan struct{})
	releaseEv1Wake := make(chan struct{})
	var (
		mu        sync.Mutex
		skipFires []string
		newFires  []string
	)

	core.fireFn = func(ctx context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
		mu.Lock()
		switch req.Authority.DefinitionID {
		case trSkipID:
			skipFires = append(skipFires, req.Event.ID)
		case trNewID:
			newFires = append(newFires, req.Event.ID)
		}
		mu.Unlock()

		if req.Authority.DefinitionID == trSkipID && req.Event.ID == ev1 {
			detached, cancelDetached := req.Tracked.Detach(ctx)
			defer cancelDetached()
			close(ev1WakeEntered)
			select {
			case <-releaseEv1Wake:
			case <-detached.Done():
				return hubfire.FireOutcome{Status: hubfire.FireStatusFailed, Error: detached.Err().Error()}
			}
		}
		return hubfire.FireOutcome{Status: hubfire.FireStatusFired, ResultRef: "msg-" + req.Event.ID[:8]}
	}

	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:           core,
		Store:          ms,
		Now:            func() time.Time { return t0 },
		PerFireTimeout: 40 * time.Millisecond,
	})
	require.NoError(t, err)

	makeItem := func(seq int64, evID string) durablequeue.Item {
		raw, mErr := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: evID})
		require.NoError(t, mErr)
		return durablequeue.Item{
			Seq:       seq,
			Kind:      trigger.EventQueueKind,
			DedupKey:  evID,
			ProjectID: projID,
			Payload:   raw,
		}
	}

	// Start ev1; wait until trSkip's wake fire is in flight.
	ev1Done := make(chan durablequeue.Result, 1)
	go func() {
		ev1Done <- disp.HandleItem(context.Background(), makeItem(1, ev1), nopLease{})
	}()
	select {
	case <-ev1WakeEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ev1 wake fire to enter")
	}

	// While ev1's wake fire on pr/42 is in flight, handle ev2 on the same subject:
	// trSkip must skip ("subject busy") without calling Fire, whereas trNew MUST fire.
	res2 := disp.HandleItem(context.Background(), makeItem(2, ev2), nopLease{})
	require.NoError(t, res2.Err)
	assert.True(t, res2.Done)

	// Different subject pr/99 while pr/42 is still busy MUST fire for both trSkip and trNew.
	resOther := disp.HandleItem(context.Background(), makeItem(3, evOther), nopLease{})
	require.NoError(t, resOther.Err)
	assert.True(t, resOther.Done)

	// Release ev1 wake fire and wait for ev1 to finish.
	close(releaseEv1Wake)
	res1 := <-ev1Done
	require.NoError(t, res1.Err)
	assert.True(t, res1.Done)

	// After ev1's wake fire ends, ev3 on pr/42 MUST fire again for trSkip.
	res3 := disp.HandleItem(context.Background(), makeItem(4, ev3), nopLease{})
	require.NoError(t, res3.Err)
	assert.True(t, res3.Done)

	mu.Lock()
	assert.Equal(t, []string{ev1, evOther, ev3}, skipFires, "skip trigger must skip ev2 while wake fire is in flight, fire other subjects, and fire ev3 after wake ends")
	assert.ElementsMatch(t, []string{ev1, ev2, evOther, ev3}, newFires, "new mode trigger must always fire")
	mu.Unlock()

	firesRes, err := ms.ListTriggerFires(context.Background(), store.TriggerFireFilter{}, store.ListOptions{})
	require.NoError(t, err)
	var ev2SkipRow *store.TriggerFire
	for i := range firesRes.Items {
		f := &firesRes.Items[i]
		if f.TriggerID == trSkipID && f.EventID == ev2 {
			ev2SkipRow = f
		}
	}
	require.NotNil(t, ev2SkipRow, "expected trigger_fires row for skipped ev2")
	assert.Equal(t, store.TriggerFireStatusSkipped, ev2SkipRow.Status)
	assert.Equal(t, trigger.ErrFireSubjectBusy, ev2SkipRow.Error)

	gotEv2, err := ms.GetTriggerEvent(context.Background(), ev2)
	require.NoError(t, err)
	assert.Equal(t, 2, gotEv2.MatchCount)
	assert.Equal(t, 1, gotEv2.FireCount, "only trNew fired on ev2; trSkip skipped")

	gotSkipTr, err := ms.GetTrigger(context.Background(), trSkipID)
	require.NoError(t, err)
	assert.Equal(t, 4, gotSkipTr.FireCount)
	assert.Equal(t, 0, gotSkipTr.ErrorCount, "skipped subject busy must not increment error_count")
}

func TestDispatcher_SubjectModeSkip_DerivedSubjectRuleCoalescesAndPreservesSourceSubject(t *testing.T) {
	ctx := context.Background()
	ms := newMemTriggerStore()
	core := &fakeCore{}
	core.enabled.Store(true)
	_, rules := newTestTriggerEngineWithDialects(t)

	projID := uuid.NewString()
	epID := uuid.NewString()
	trID := uuid.NewString()
	t0 := time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)

	require.NoError(t, ms.CreateTrigger(ctx, &store.Trigger{
		ID:             trID,
		ProjectID:      projID,
		Name:           "skip-derived-subj-tr",
		SourceKind:     store.TriggerSourceKindWebhook,
		SourceID:       epID,
		MatchTypes:     []string{"webhook.received"},
		MatchRules:     []store.TriggerMatchRule{{Dialect: "cel", Expr: `has(event.payload.service)`}},
		SubjectDialect: "cel",
		SubjectRule:    `"deploy:" + string(event.payload.service)`,
		SubjectMode:    store.TriggerSubjectModeSkip,
		ActionType:     store.TriggerActionMessage,
		ActionPayload:  `{"agentId":"a1","message":"deploy","wake":true}`,
		Status:         store.TriggerStatusActive,
		InitiatorAttribution: store.InitiatorAttribution{
			AuthorizationRevision: 1,
		},
	}))

	mkEvent := func(sourceSubj, service string) string {
		id := uuid.NewString()
		require.NoError(t, ms.CreateTriggerEvent(ctx, &store.TriggerEvent{
			ID:         id,
			ProjectID:  projID,
			SourceKind: store.TriggerSourceKindWebhook,
			SourceID:   epID,
			Type:       "webhook.received",
			Subject:    sourceSubj,
			DedupKey:   "d-" + id,
			OccurredAt: t0,
			ReceivedAt: t0,
			Payload:    `{"service":"` + service + `"}`,
			Status:     store.TriggerEventStatusPending,
		}))
		return id
	}

	srcSubj1 := "webhook:" + epID + "/delivery-aaa"
	srcSubj2 := "webhook:" + epID + "/delivery-bbb"
	srcSubj3 := "webhook:" + epID + "/delivery-ccc"
	ev1 := mkEvent(srcSubj1, "payments")
	ev2 := mkEvent(srcSubj2, "payments")
	ev3 := mkEvent(srcSubj3, "billing")

	ev1Entered := make(chan struct{})
	releaseEv1 := make(chan struct{})
	var (
		mu              sync.Mutex
		firedEvents     []string
		envelopeSubjMap = make(map[string]string)
	)
	core.fireFn = func(fctx context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
		mu.Lock()
		firedEvents = append(firedEvents, req.Event.ID)
		envelopeSubjMap[req.Event.ID] = req.Event.Subject
		mu.Unlock()

		if req.Event.ID == ev1 {
			detached, cancelDetached := req.Tracked.Detach(fctx)
			defer cancelDetached()
			close(ev1Entered)
			select {
			case <-releaseEv1:
			case <-detached.Done():
				return hubfire.FireOutcome{Status: hubfire.FireStatusFailed, Error: detached.Err().Error()}
			}
		}
		return hubfire.FireOutcome{Status: hubfire.FireStatusFired, ResultRef: "msg-" + req.Event.ID[:8]}
	}

	var auditSink memFireAuditSink
	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:           core,
		Store:          ms,
		Rules:          rules,
		FireAudit:      &auditSink,
		Now:            func() time.Time { return t0 },
		PerFireTimeout: 40 * time.Millisecond,
	})
	require.NoError(t, err)

	makeItem := func(seq int64, evID string) durablequeue.Item {
		raw, mErr := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: evID})
		require.NoError(t, mErr)
		return durablequeue.Item{
			Seq:       seq,
			Kind:      trigger.EventQueueKind,
			DedupKey:  evID,
			ProjectID: projID,
			Payload:   raw,
		}
	}

	ev1Done := make(chan durablequeue.Result, 1)
	go func() {
		ev1Done <- disp.HandleItem(ctx, makeItem(1, ev1), nopLease{})
	}()
	select {
	case <-ev1Entered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ev1 firing on deploy:payments")
	}

	// Different source subject (srcSubj2 != srcSubj1) deriving the SAME subject ("deploy:payments")
	// must skip as "subject busy" while ev1 is firing.
	res2 := disp.HandleItem(ctx, makeItem(2, ev2), nopLease{})
	require.NoError(t, res2.Err)
	assert.True(t, res2.Done)

	// Different derived subject ("deploy:billing") must still fire while ev1 is in flight.
	res3 := disp.HandleItem(ctx, makeItem(3, ev3), nopLease{})
	require.NoError(t, res3.Err)
	assert.True(t, res3.Done)

	close(releaseEv1)
	res1 := <-ev1Done
	require.NoError(t, res1.Err)
	assert.True(t, res1.Done)

	mu.Lock()
	assert.Equal(t, []string{ev1, ev3}, firedEvents, "ev2 must skip on derived subject collision while ev3 on different derived subject fires")
	assert.Equal(t, srcSubj1, envelopeSubjMap[ev1], "L23: FireEvent.Subject for envelope must stay source subject")
	assert.Equal(t, srcSubj3, envelopeSubjMap[ev3], "L23: FireEvent.Subject for envelope must stay source subject")
	mu.Unlock()

	firesRes, err := ms.ListTriggerFires(ctx, store.TriggerFireFilter{}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, firesRes.Items, 3)
	byEv := make(map[string]store.TriggerFire, 3)
	for _, f := range firesRes.Items {
		byEv[f.EventID] = f
	}

	assert.Equal(t, store.TriggerFireStatusFired, byEv[ev1].Status)
	assert.Equal(t, "deploy:payments", byEv[ev1].Subject)
	assert.Equal(t, "1/1", byEv[ev1].MatchedThrough)

	assert.Equal(t, store.TriggerFireStatusSkipped, byEv[ev2].Status)
	assert.Equal(t, trigger.ErrFireSubjectBusy, byEv[ev2].Error)
	assert.Equal(t, "deploy:payments", byEv[ev2].Subject)
	assert.Equal(t, "1/1", byEv[ev2].MatchedThrough)
	assert.Equal(t, "cel", byEv[ev2].DecidedDialect)

	assert.Equal(t, store.TriggerFireStatusFired, byEv[ev3].Status)
	assert.Equal(t, "deploy:billing", byEv[ev3].Subject)

	assert.Len(t, auditSink.snapshot(), 3, "every terminal fire including subject-busy skip must emit FireAuditRecord")
}

func TestDispatcher_GitHubSubjectModeSkip_UnauthorizedRefusesWithoutSkipClaimAndAuthorizedCoalesces(t *testing.T) {
	ctx := context.Background()
	ms := newMemTriggerStore()
	core := &fakeCore{}
	core.enabled.Store(true)
	_, rules := newTestTriggerEngineWithDialects(t)

	projID := uuid.NewString()
	trID := uuid.NewString()
	t0 := time.Date(2026, 11, 20, 17, 0, 0, 0, time.UTC)

	var sourceAllowed atomic.Bool
	core.authorizeSourceFn = func(_ context.Context, _ *store.TriggerEvent) (bool, error) {
		return sourceAllowed.Load(), nil
	}

	require.NoError(t, ms.CreateTrigger(ctx, &store.Trigger{
		ID:            trID,
		ProjectID:     projID,
		Name:          "gh-pr-skip-tr",
		SourceKind:    store.TriggerSourceKindGitHub,
		SourceID:      "",
		MatchTypes:    []string{"github.pull_request.*", "github.issue_comment.created"},
		MatchRules:    []store.TriggerMatchRule{{Dialect: "cel", Expr: `event.attributes.repo == "octo-org/octo-repo"`}},
		SubjectMode:   store.TriggerSubjectModeSkip,
		ActionType:    store.TriggerActionMessage,
		ActionPayload: `{"agentId":"a1","message":"review PR","wake":true}`,
		Status:        store.TriggerStatusActive,
		InitiatorAttribution: store.InitiatorAttribution{
			AuthorizationRevision: 1,
		},
	}))

	mkGitHubEvent := func(evType, subject, repo string) string {
		id := uuid.NewString()
		require.NoError(t, ms.CreateTriggerEvent(ctx, &store.TriggerEvent{
			ID:         id,
			ProjectID:  projID,
			SourceKind: store.TriggerSourceKindGitHub,
			SourceID:   "9901",
			Type:       evType,
			Subject:    subject,
			DedupKey:   "gh-" + id,
			OccurredAt: t0,
			ReceivedAt: t0,
			Attributes: map[string]string{"repo": repo, "number": "42"},
			Payload:    `{"number":42}`,
			Status:     store.TriggerEventStatusPending,
		}))
		return id
	}

	evAuth1Entered := make(chan struct{})
	releaseEvAuth1 := make(chan struct{})
	var (
		mu          sync.Mutex
		firedEvents []string
	)
	var evAuth1 string
	core.fireFn = func(fctx context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
		mu.Lock()
		firedEvents = append(firedEvents, req.Event.ID)
		mu.Unlock()
		if req.Event.ID == evAuth1 {
			detached, cancelDetached := req.Tracked.Detach(fctx)
			defer cancelDetached()
			close(evAuth1Entered)
			select {
			case <-releaseEvAuth1:
			case <-detached.Done():
				return hubfire.FireOutcome{Status: hubfire.FireStatusFailed, Error: detached.Err().Error()}
			}
		}
		return hubfire.FireOutcome{Status: hubfire.FireStatusFired, ResultRef: "msg-" + req.Event.ID[:8]}
	}

	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:           core,
		Store:          ms,
		Rules:          rules,
		Now:            func() time.Time { return t0 },
		PerFireTimeout: 40 * time.Millisecond,
	})
	require.NoError(t, err)

	makeItem := func(seq int64, evID string) durablequeue.Item {
		raw, mErr := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: evID})
		require.NoError(t, mErr)
		return durablequeue.Item{
			Seq:       seq,
			Kind:      trigger.EventQueueKind,
			DedupKey:  evID,
			ProjectID: projID,
			Payload:   raw,
		}
	}

	// 1. Unauthorized GitHub association on a skip-mode trigger:
	// records refused ("github association not authorized"), never evaluates rules,
	// and NEVER calls ClaimTriggerFireSubjectSkip.
	sourceAllowed.Store(false)
	evUnauth := mkGitHubEvent("github.pull_request.opened", "github:octo-org/octo-repo#42", "octo-org/octo-repo")
	resUnauth := disp.HandleItem(ctx, makeItem(1, evUnauth), nopLease{})
	require.NoError(t, resUnauth.Err)
	assert.True(t, resUnauth.Done)
	assert.Zero(t, ms.claimSubjectSkipCalls.Load(), "unauthorized GitHub event must never reach ClaimTriggerFireSubjectSkip")
	assert.Empty(t, disp.RuleEvalStats(), "unauthorized GitHub event must be refused before rule evaluation")
	mu.Lock()
	assert.Empty(t, firedEvents)
	mu.Unlock()

	// 2. Authorized GitHub association, but CEL rule evaluates false:
	// still never reaches ClaimTriggerFireSubjectSkip.
	sourceAllowed.Store(true)
	evNonMatch := mkGitHubEvent("github.pull_request.opened", "github:other-org/other-repo#42", "other-org/other-repo")
	resNonMatch := disp.HandleItem(ctx, makeItem(2, evNonMatch), nopLease{})
	require.NoError(t, resNonMatch.Err)
	assert.True(t, resNonMatch.Done)
	assert.Zero(t, ms.claimSubjectSkipCalls.Load(), "non-matching GitHub event must not reach ClaimTriggerFireSubjectSkip")

	// 3. Authorized + matching GitHub events coalesce on github:<owner>/<repo>#<number>:
	// evAuth1 on #42 claims firing (and was NOT poisoned by evUnauth's terminal refused row);
	// evAuth2 (issue_comment on same #42) skips as "subject busy";
	// evAuth3 (PR #43) fires independently while #42 is in flight.
	evAuth1 = mkGitHubEvent("github.pull_request.opened", "github:octo-org/octo-repo#42", "octo-org/octo-repo")
	evAuth2 := mkGitHubEvent("github.issue_comment.created", "github:octo-org/octo-repo#42", "octo-org/octo-repo")
	evAuth3 := mkGitHubEvent("github.pull_request.opened", "github:octo-org/octo-repo#43", "octo-org/octo-repo")

	ev1Done := make(chan durablequeue.Result, 1)
	go func() {
		ev1Done <- disp.HandleItem(ctx, makeItem(3, evAuth1), nopLease{})
	}()
	select {
	case <-evAuth1Entered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for evAuth1 firing on github:octo-org/octo-repo#42")
	}

	resAuth2 := disp.HandleItem(ctx, makeItem(4, evAuth2), nopLease{})
	require.NoError(t, resAuth2.Err)
	assert.True(t, resAuth2.Done)

	resAuth3 := disp.HandleItem(ctx, makeItem(5, evAuth3), nopLease{})
	require.NoError(t, resAuth3.Err)
	assert.True(t, resAuth3.Done)

	close(releaseEvAuth1)
	resAuth1 := <-ev1Done
	require.NoError(t, resAuth1.Err)
	assert.True(t, resAuth1.Done)

	assert.Equal(t, int32(3), ms.claimSubjectSkipCalls.Load(), "only source-authorized, rule-matched GitHub events reach ClaimTriggerFireSubjectSkip")
	mu.Lock()
	assert.Equal(t, []string{evAuth1, evAuth3}, firedEvents)
	mu.Unlock()

	firesRes, err := ms.ListTriggerFires(ctx, store.TriggerFireFilter{}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, firesRes.Items, 4)
	byEv := make(map[string]store.TriggerFire, 4)
	for _, f := range firesRes.Items {
		byEv[f.EventID] = f
	}

	assert.Equal(t, store.TriggerFireStatusRefused, byEv[evUnauth].Status)
	assert.Equal(t, trigger.ErrFireGitHubAssociationUnauthorized, byEv[evUnauth].Error)
	assert.Equal(t, "github:octo-org/octo-repo#42", byEv[evUnauth].Subject)

	assert.Equal(t, store.TriggerFireStatusFired, byEv[evAuth1].Status)
	assert.Equal(t, "github:octo-org/octo-repo#42", byEv[evAuth1].Subject)

	assert.Equal(t, store.TriggerFireStatusSkipped, byEv[evAuth2].Status)
	assert.Equal(t, trigger.ErrFireSubjectBusy, byEv[evAuth2].Error)
	assert.Equal(t, "github:octo-org/octo-repo#42", byEv[evAuth2].Subject)

	assert.Equal(t, store.TriggerFireStatusFired, byEv[evAuth3].Status)
	assert.Equal(t, "github:octo-org/octo-repo#43", byEv[evAuth3].Subject)
}

func TestDispatcher_DispatchAgentSkipUsesSubjectClaimsNotFireSkip(t *testing.T) {
	ctx := context.Background()
	ms := newMemTriggerStore()
	core := &fakeCore{}
	core.enabled.Store(true)

	projID := uuid.NewString()
	epID := uuid.NewString()
	t0 := time.Date(2026, 11, 21, 1, 0, 0, 0, time.UTC)

	dispTrigID := uuid.NewString()
	require.NoError(t, ms.CreateTrigger(ctx, &store.Trigger{
		ID:          dispTrigID,
		ProjectID:   projID,
		Name:        "disp-skip",
		SourceKind:  store.TriggerSourceKindWebhook,
		SourceID:    epID,
		MatchTypes:  []string{"webhook.run"},
		ActionType:  store.TriggerActionDispatchAgent,
		SubjectMode: store.TriggerSubjectModeSkip,
		Status:      store.TriggerStatusActive,
	}))
	msgTrigID := uuid.NewString()
	require.NoError(t, ms.CreateTrigger(ctx, &store.Trigger{
		ID:          msgTrigID,
		ProjectID:   projID,
		Name:        "msg-skip",
		SourceKind:  store.TriggerSourceKindWebhook,
		SourceID:    epID,
		MatchTypes:  []string{"webhook.run"},
		ActionType:  store.TriggerActionMessage,
		SubjectMode: store.TriggerSubjectModeSkip,
		Status:      store.TriggerStatusActive,
	}))

	evID := uuid.NewString()
	require.NoError(t, ms.CreateTriggerEvent(ctx, &store.TriggerEvent{
		ID:         evID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		Type:       "webhook.run",
		Subject:    "webhook:" + epID + "/sub-1",
		Status:     store.TriggerEventStatusPending,
		ReceivedAt: t0,
	}))

	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:  core,
		Store: ms,
		Now:   func() time.Time { return t0 },
	})
	require.NoError(t, err)

	raw, err := json.Marshal(trigger.EventQueuePayload{Version: trigger.EventQueuePayloadVersion, EventID: evID})
	require.NoError(t, err)
	res := disp.HandleItem(ctx, durablequeue.Item{Seq: 1, Kind: trigger.EventQueueKind, DedupKey: evID, Payload: raw}, nopLease{})
	require.NoError(t, res.Err)
	assert.True(t, res.Done)

	// Only the message trigger uses PR 4's ClaimTriggerFireSubjectSkip;
	// dispatch_agent uses ClaimTriggerFire and delegates skip to SubjectClaims.
	assert.Equal(t, int32(1), ms.claimSubjectSkipCalls.Load(), "only message+skip must call ClaimTriggerFireSubjectSkip")
}

func TestDispatcher_EvaluatesCandidatesOldestFirst(t *testing.T) {
	ctx := context.Background()
	ms := newMemTriggerStore()
	rules, err := triggerrule.NewDefault()
	require.NoError(t, err)

	projID := uuid.NewString()
	epID := uuid.NewString()
	evID := uuid.NewString()
	t0 := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

	// Insert newest-first (mirroring Ent ListTriggers' Created DESC, ID DESC order)
	// including a same-timestamp pair to pin ID ASC tie-breaking.
	trNewest := &store.Trigger{
		ID:         "00000000-0000-0000-0000-000000000004",
		ProjectID:  projID,
		Name:       "newest",
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		MatchTypes: []string{"webhook.received"},
		ActionType: store.TriggerActionMessage,
		Status:     store.TriggerStatusActive,
		CreatedAt:  t0.Add(3 * time.Minute),
	}
	trTieB := &store.Trigger{
		ID:         "00000000-0000-0000-0000-000000000003",
		ProjectID:  projID,
		Name:       "tie-b",
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		MatchTypes: []string{"webhook.received"},
		ActionType: store.TriggerActionMessage,
		Status:     store.TriggerStatusActive,
		CreatedAt:  t0.Add(2 * time.Minute),
	}
	trTieA := &store.Trigger{
		ID:         "00000000-0000-0000-0000-000000000002",
		ProjectID:  projID,
		Name:       "tie-a",
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		MatchTypes: []string{"webhook.received"},
		ActionType: store.TriggerActionMessage,
		Status:     store.TriggerStatusActive,
		CreatedAt:  t0.Add(2 * time.Minute),
	}
	trOldest := &store.Trigger{
		ID:         "00000000-0000-0000-0000-000000000001",
		ProjectID:  projID,
		Name:       "oldest",
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		MatchTypes: []string{"webhook.received"},
		ActionType: store.TriggerActionMessage,
		Status:     store.TriggerStatusActive,
		CreatedAt:  t0.Add(1 * time.Minute),
	}
	for _, tr := range []*store.Trigger{trNewest, trTieB, trTieA, trOldest} {
		require.NoError(t, ms.CreateTrigger(ctx, tr))
	}
	require.NoError(t, ms.CreateTriggerEvent(ctx, &store.TriggerEvent{
		ID:         evID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		Type:       "webhook.received",
		Subject:    "webhook:" + epID,
		DedupKey:   "d-oldest-first",
		OccurredAt: t0,
		ReceivedAt: t0,
		Payload:    `{"ok":true}`,
		Status:     store.TriggerEventStatusPending,
	}))

	var firedNames []string
	core := &fakeCore{
		fireFn: func(_ context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
			firedNames = append(firedNames, req.Event.Trigger.Name)
			return hubfire.FireOutcome{Status: hubfire.FireStatusFired}
		},
	}
	core.enabled.Store(true)

	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:  core,
		Store: ms,
		Rules: rules,
		Now:   func() time.Time { return t0 },
	})
	require.NoError(t, err)

	raw, _ := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: evID})
	res := disp.HandleItem(ctx, durablequeue.Item{
		Kind:     trigger.EventQueueKind,
		DedupKey: evID,
		Payload:  raw,
	}, nopLease{})
	require.NoError(t, res.Err)
	assert.True(t, res.Done)
	assert.Equal(t, []string{"oldest", "tie-a", "tie-b", "newest"}, firedNames)
}

func TestDispatcher_CompileBudgetExhaustionSkipsMissesKeepsHitsAndRecordsMetric(t *testing.T) {
	ctx := context.Background()
	ms := newMemTriggerStore()
	rules, err := triggerrule.NewDefault()
	require.NoError(t, err)

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	otelMetrics, err := trigger.NewOTelRuleEvalMetrics(mp)
	require.NoError(t, err)

	projID := uuid.NewString()
	epID := uuid.NewString()
	t0 := time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)

	// Candidate order (oldest-first):
	// 1. trCold1: uncached CEL rule -> compiles (1 miss), exhausts 1ns compile budget, evaluates & fires.
	// 2. trCold2: uncached CEL rule -> gated before compile -> skipped with "rule compile budget exhausted" (R4).
	// 3. trWarm:  pre-warmed CEL rule + pre-warmed subject_rule -> cache hit STILL evaluates & fires (R3).
	// 4. trColdSubj: pre-warmed CEL rule + uncached subject_rule -> matches & fires, subject falls back to ev.Subject (R3).
	// 5. trTypesOnly: no rules -> matches & fires.
	trCold1 := &store.Trigger{
		ID:                   uuid.NewString(),
		ProjectID:            projID,
		Name:                 "cold-1",
		SourceKind:           store.TriggerSourceKindWebhook,
		SourceID:             epID,
		MatchTypes:           []string{"webhook.received"},
		MatchRules:           []store.TriggerMatchRule{{Dialect: ruleengine.DialectCEL, Expr: `event.payload.k == "v1"`}},
		ActionType:           store.TriggerActionMessage,
		Status:               store.TriggerStatusActive,
		InitiatorAttribution: store.InitiatorAttribution{AuthorizationRevision: 1},
		CreatedAt:            t0.Add(1 * time.Second),
	}
	trCold2 := &store.Trigger{
		ID:                   uuid.NewString(),
		ProjectID:            projID,
		Name:                 "cold-2",
		SourceKind:           store.TriggerSourceKindWebhook,
		SourceID:             epID,
		MatchTypes:           []string{"webhook.received"},
		MatchRules:           []store.TriggerMatchRule{{Dialect: ruleengine.DialectCEL, Expr: `event.payload.k2 == "v2"`}},
		ActionType:           store.TriggerActionMessage,
		Status:               store.TriggerStatusActive,
		InitiatorAttribution: store.InitiatorAttribution{AuthorizationRevision: 1},
		CreatedAt:            t0.Add(2 * time.Second),
	}
	trWarm := &store.Trigger{
		ID:                   uuid.NewString(),
		ProjectID:            projID,
		Name:                 "warm-hit",
		SourceKind:           store.TriggerSourceKindWebhook,
		SourceID:             epID,
		MatchTypes:           []string{"webhook.received"},
		MatchRules:           []store.TriggerMatchRule{{Dialect: ruleengine.DialectCEL, Expr: `event.payload.warm == true`}},
		SubjectDialect:       ruleengine.DialectCEL,
		SubjectRule:          `"warm:" + event.payload.k`,
		ActionType:           store.TriggerActionMessage,
		Status:               store.TriggerStatusActive,
		InitiatorAttribution: store.InitiatorAttribution{AuthorizationRevision: 1},
		CreatedAt:            t0.Add(3 * time.Second),
	}
	trColdSubj := &store.Trigger{
		ID:                   uuid.NewString(),
		ProjectID:            projID,
		Name:                 "warm-match-cold-subj",
		SourceKind:           store.TriggerSourceKindWebhook,
		SourceID:             epID,
		MatchTypes:           []string{"webhook.received"},
		MatchRules:           []store.TriggerMatchRule{{Dialect: ruleengine.DialectCEL, Expr: `event.payload.warm == true`}},
		SubjectDialect:       ruleengine.DialectCEL,
		SubjectRule:          `"coldsubj:" + event.payload.k`,
		ActionType:           store.TriggerActionMessage,
		Status:               store.TriggerStatusActive,
		InitiatorAttribution: store.InitiatorAttribution{AuthorizationRevision: 1},
		CreatedAt:            t0.Add(4 * time.Second),
	}
	trTypesOnly := &store.Trigger{
		ID:                   uuid.NewString(),
		ProjectID:            projID,
		Name:                 "types-only",
		SourceKind:           store.TriggerSourceKindWebhook,
		SourceID:             epID,
		MatchTypes:           []string{"webhook.received"},
		ActionType:           store.TriggerActionMessage,
		Status:               store.TriggerStatusActive,
		InitiatorAttribution: store.InitiatorAttribution{AuthorizationRevision: 1},
		CreatedAt:            t0.Add(5 * time.Second),
	}

	// Pre-warm trWarm's match + subject rules and trColdSubj's match rule (leaving trColdSubj's subject rule cold).
	preAct, err := triggerrule.NewEventActivation(hubfire.FireEvent{
		ID:        "prewarm",
		ProjectID: projID,
		Type:      "webhook.received",
		Subject:   "webhook:" + epID,
		Payload:   []byte(`{"k":"v1","k2":"v2","warm":true}`),
	})
	require.NoError(t, err)
	warmScope := celengine.CacheScope{Consumer: triggerrule.ConsumerName, ProjectID: projID, ID: trWarm.ID, Revision: 1}
	_, _, err = rules.MatchAllWithTrace(ctx, warmScope, []triggerrule.Rule{{Dialect: ruleengine.DialectCEL, Expr: trWarm.MatchRules[0].Expr}}, preAct)
	require.NoError(t, err)
	_, err = rules.Subject(ctx, warmScope, triggerrule.Rule{Dialect: ruleengine.DialectCEL, Expr: trWarm.SubjectRule}, preAct)
	require.NoError(t, err)
	coldSubjScope := celengine.CacheScope{Consumer: triggerrule.ConsumerName, ProjectID: projID, ID: trColdSubj.ID, Revision: 1}
	_, _, err = rules.MatchAllWithTrace(ctx, coldSubjScope, []triggerrule.Rule{{Dialect: ruleengine.DialectCEL, Expr: trColdSubj.MatchRules[0].Expr}}, preAct)
	require.NoError(t, err)

	for _, tr := range []*store.Trigger{trCold1, trCold2, trWarm, trColdSubj, trTypesOnly} {
		require.NoError(t, ms.CreateTrigger(ctx, tr))
	}

	evID := uuid.NewString()
	require.NoError(t, ms.CreateTriggerEvent(ctx, &store.TriggerEvent{
		ID:         evID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		Type:       "webhook.received",
		Subject:    "webhook:" + epID,
		DedupKey:   "d-compile-budget",
		OccurredAt: t0,
		ReceivedAt: t0,
		Payload:    `{"k":"v1","k2":"v2","warm":true}`,
		Status:     store.TriggerEventStatusPending,
	}))

	var firedNames []string
	core := &fakeCore{
		fireFn: func(_ context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
			firedNames = append(firedNames, req.Event.Trigger.Name)
			return hubfire.FireOutcome{Status: hubfire.FireStatusFired}
		},
	}
	core.enabled.Store(true)

	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:          core,
		Store:         ms,
		Rules:         rules,
		RuleMetrics:   otelMetrics,
		CompileBudget: time.Nanosecond,
		Now:           func() time.Time { return t0 },
	})
	require.NoError(t, err)

	raw, _ := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: evID})
	res := disp.HandleItem(ctx, durablequeue.Item{
		Kind:     trigger.EventQueueKind,
		DedupKey: evID,
		Payload:  raw,
	}, nopLease{})
	require.NoError(t, res.Err)
	assert.True(t, res.Done, "plain skip completes the item on attempt 0")
	assert.Equal(t, []string{"cold-1", "warm-hit", "warm-match-cold-subj", "types-only"}, firedNames)

	fires, err := ms.ListTriggerFires(ctx, store.TriggerFireFilter{EventID: evID}, store.ListOptions{})
	require.NoError(t, err)
	byTrig := make(map[string]store.TriggerFire, len(fires.Items))
	for _, f := range fires.Items {
		byTrig[f.TriggerID] = f
	}
	// R4: trCold2 skipped with compile-specific error and trace reason (never "rule evaluation failed" or "rule budget exhausted").
	fCold2 := byTrig[trCold2.ID]
	assert.Equal(t, store.TriggerFireStatusSkipped, fCold2.Status)
	assert.Equal(t, trigger.ErrFireRuleCompileBudgetExhausted, fCold2.Error)
	assert.Contains(t, fCold2.RuleTrace, `"reason":"compile_budget_exhausted"`)

	// R3: trWarm derived its cached subject; trColdSubj fell back to ev.Subject.
	assert.Equal(t, "warm:v1", byTrig[trWarm.ID].Subject)
	assert.Equal(t, "webhook:"+epID, byTrig[trColdSubj.ID].Subject)

	// C2: compile miss metric incremented exactly once (for trCold1) with dialect="cel" and no project_id label.
	assert.Equal(t, map[string]int64{"cel": 1}, disp.RuleCompileMissStats())
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(ctx, &rm))
	var missVal int64
	foundMissMetric := false
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != trigger.MetricTriggerRuleCompileMissesTotal {
				continue
			}
			foundMissMetric = true
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok)
			for _, dp := range sum.DataPoints {
				for _, kv := range dp.Attributes.ToSlice() {
					assert.NotEqual(t, "project_id", string(kv.Key), "compile miss metric must not carry project_id label")
				}
				missVal += dp.Value
			}
		}
	}
	assert.True(t, foundMissMetric, "expected %s metric to be exported", trigger.MetricTriggerRuleCompileMissesTotal)
	assert.Equal(t, int64(1), missVal)
}

func TestNormalizeFireSkipReason_CompileBudgetExhaustedAndAgentNameInUse(t *testing.T) {
	assert.Equal(t, trigger.FireSkipReasonRuleCompileBudgetExhausted,
		trigger.NormalizeFireSkipReason(trigger.ErrFireRuleCompileBudgetExhausted))
	assert.Equal(t, trigger.FireSkipReasonRuleCompileBudgetExhausted,
		trigger.NormalizeFireSkipReason("  rule compile budget exhausted  "))
	assert.Equal(t, trigger.FireSkipReasonAgentNameInUse,
		trigger.NormalizeFireSkipReason("agent name in use"))
	assert.Equal(t, trigger.FireSkipReasonAgentNameInUse,
		trigger.NormalizeFireSkipReason("  agent name in use  "))
	assert.Equal(t, trigger.FireSkipReasonCommentRateLimited,
		trigger.NormalizeFireSkipReason("comment rate limited"))
	assert.Equal(t, trigger.FireSkipReasonCommentRateLimited,
		trigger.NormalizeFireSkipReason("  comment rate limited  "))
	assert.Equal(t, trigger.FireSkipReasonOther,
		trigger.NormalizeFireSkipReason("some free-text error"))
}
