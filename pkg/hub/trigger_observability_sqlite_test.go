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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAdminScheduler_DurableQueuesGatedAndPopulated(t *testing.T) {
	srv, s := testServer(t)
	srv.scheduler = NewScheduler(s, slog.Default())
	ctx := context.Background()

	// Seed a queue item created 90 seconds ago and a second leased item.
	dqs, ok := s.(store.DurableQueueStore)
	require.True(t, ok, "SQLite store must implement DurableQueueStore")
	now := time.Now().UTC()
	_, err := dqs.EnqueueDurableItem(ctx, nil, store.DurableQueueRow{
		Kind:          trigger.EventQueueKind,
		DedupKey:      "obs-item-1",
		ProjectID:     api.NewUUID(),
		Payload:       []byte(`{"v":1,"event_id":"ev-1"}`),
		CreatedAt:     now.Add(-90 * time.Second),
		NotBefore:     now.Add(-90 * time.Second),
		NextAttemptAt: now.Add(-90 * time.Second),
	})
	require.NoError(t, err)
	_, err = dqs.EnqueueDurableItem(ctx, nil, store.DurableQueueRow{
		Kind:          trigger.EventQueueKind,
		DedupKey:      "obs-item-2",
		ProjectID:     api.NewUUID(),
		Payload:       []byte(`{"v":1,"event_id":"ev-2"}`),
		CreatedAt:     now.Add(-30 * time.Second),
		NotBefore:     now.Add(-30 * time.Second),
		NextAttemptAt: now.Add(-30 * time.Second),
	})
	require.NoError(t, err)
	claimed, err := dqs.ClaimDurableItems(ctx, trigger.EventQueueKind, "obs-owner", now, now.Add(time.Minute), 1)
	require.NoError(t, err)
	require.Len(t, claimed, 1)

	admin := seedHubAdminForObsTest(t, s, "")

	// With hub.triggers off, durableQueues is omitted.
	recOff := requestAsIdentity(t, srv, admin, http.MethodGet, "/api/v1/admin/scheduler", nil)
	require.Equal(t, http.StatusOK, recOff.Code)
	var bodyOff map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(recOff.Body.Bytes(), &bodyOff))
	_, hasQueuesOff := bodyOff["durableQueues"]
	assert.False(t, hasQueuesOff, "durableQueues must be omitted while hub.triggers is off")

	// Enable hub.triggers -> durableQueues appears with pending=2, leased=1, oldestAgeSeconds >= 85.
	setTriggersExperimentForObsTest(t, srv, true)
	recOn := requestAsIdentity(t, srv, admin, http.MethodGet, "/api/v1/admin/scheduler", nil)
	require.Equal(t, http.StatusOK, recOn.Code)
	var bodyOn struct {
		DurableQueues []AdminDurableQueueStatus `json:"durableQueues"`
	}
	require.NoError(t, json.Unmarshal(recOn.Body.Bytes(), &bodyOn))
	require.Len(t, bodyOn.DurableQueues, 1)
	assert.Equal(t, trigger.EventQueueKind, bodyOn.DurableQueues[0].Kind)
	assert.Equal(t, int64(2), bodyOn.DurableQueues[0].Pending)
	assert.Equal(t, int64(1), bodyOn.DurableQueues[0].Leased)
	assert.GreaterOrEqual(t, bodyOn.DurableQueues[0].OldestAgeSeconds, 85.0)
}

func TestHealthSummary_TriggersSectionGatedAndPopulated(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := api.NewUUID()
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID:   projectID,
		Name: "Obs Project",
		Slug: "obs-proj",
	}))
	now := time.Now().UTC()
	lastFail := now.Add(-5 * time.Minute)
	epID := api.NewUUID()

	// Seed:
	// 1. Active failing trigger (LastFireStatus=failed)
	// 2. Active healthy trigger (LastFireStatus=fired) — excluded
	// 3. Hub-paused trigger (source_deleted)
	// 4. Hub-paused trigger (target_deleted)
	// 5. User-paused trigger (no reason) — excluded
	seedTrigger := func(name, status, statusReason, lastFireStatus, lastFireErr string, lastFireAt *time.Time, agentID string) {
		tr := &store.Trigger{
			ID:             api.NewUUID(),
			ProjectID:      projectID,
			Name:           name,
			SourceKind:     store.TriggerSourceKindWebhook,
			SourceID:       epID,
			MatchTypes:     []string{"webhook.received"},
			ActionType:     store.TriggerActionMessage,
			ActionPayload:  `{"agentName":"worker","message":"hi"}`,
			SubjectMode:    store.TriggerSubjectModeNew,
			Status:         status,
			StatusReason:   statusReason,
			LastFireStatus: lastFireStatus,
			LastFireError:  lastFireErr,
			LastFireAt:     lastFireAt,
			FireCount:      2,
			ErrorCount:     1,
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		if agentID != "" {
			tr.InitiatorAttribution = store.InitiatorAttribution{
				InitiatorPrincipalKind: store.DelegationPrincipalAgent,
				InitiatorPrincipalID:   agentID,
			}
		}
		require.NoError(t, s.CreateTrigger(ctx, tr))
	}

	seedTrigger("failing-tr", store.TriggerStatusActive, "", store.TriggerFireStatusFailed, "delivery failed", &lastFail, "")
	seedTrigger("healthy-tr", store.TriggerStatusActive, "", store.TriggerFireStatusFired, "", &lastFail, "")
	seedTrigger("paused-src", store.TriggerStatusPaused, store.TriggerStatusReasonSourceDeleted, "", "", nil, "")
	seedTrigger("paused-tgt", store.TriggerStatusPaused, store.TriggerStatusReasonTargetDeleted, "", "", nil, "")
	seedTrigger("paused-user", store.TriggerStatusPaused, store.TriggerStatusReasonNone, "", "", nil, "")

	admin := seedHubAdminForObsTest(t, s, projectID)

	// With hub.triggers off: triggers section is omitted.
	recOff := requestAsIdentity(t, srv, admin, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, recOff.Code)
	var rawOff map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(recOff.Body.Bytes(), &rawOff))
	_, hasTriggersOff := rawOff["triggers"]
	assert.False(t, hasTriggersOff, "triggers section must be omitted while hub.triggers is off")

	// Enable hub.triggers: triggers section is populated with failing + paused + paused_by_reason.
	setTriggersExperimentForObsTest(t, srv, true)
	recOn := requestAsIdentity(t, srv, admin, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, recOn.Code)
	var summary HealthSummaryResponse
	require.NoError(t, json.Unmarshal(recOn.Body.Bytes(), &summary))
	require.NotNil(t, summary.Triggers)
	assert.Equal(t, 1, summary.Triggers.Total)
	require.Len(t, summary.Triggers.Failing, 1)
	assert.Equal(t, "failing-tr", summary.Triggers.Failing[0].Name)
	assert.Equal(t, "obs-proj", summary.Triggers.Failing[0].ProjectSlug)
	assert.Equal(t, store.TriggerFireStatusFailed, summary.Triggers.Failing[0].LastFireStatus)

	assert.Equal(t, 2, summary.Triggers.PausedTotal)
	require.Len(t, summary.Triggers.Paused, 2)
	assert.Equal(t, 1, summary.Triggers.PausedByReason[store.TriggerStatusReasonSourceDeleted])
	assert.Equal(t, 1, summary.Triggers.PausedByReason[store.TriggerStatusReasonTargetDeleted])

	// Attention list contains trigger items without degrading overall status.
	var triggerAttentionMsgs []string
	for _, it := range summary.Attention {
		if it.Kind == HealthAttentionTrigger {
			triggerAttentionMsgs = append(triggerAttentionMsgs, it.Message)
		}
	}
	assert.Len(t, triggerAttentionMsgs, 3)

	// Caller-scoped authorization: a user with hub.health.read but no project
	// access to projectID sees an empty triggers section, not projectID's triggers.
	outsider := NewAuthenticatedUser(api.NewUUID(), "outsider@example.com", "Outsider", "member", "session")
	reqOut := httptest.NewRequest(http.MethodGet, "/api/v1/admin/health/summary", nil)
	reqOut = reqOut.WithContext(contextWithIdentity(reqOut.Context(), outsider))
	outSec := srv.healthSummaryTriggerSection(reqOut)
	require.NotNil(t, outSec)
	assert.Empty(t, outSec.Failing)
	assert.Empty(t, outSec.Paused)
	assert.Zero(t, outSec.Total)
	assert.Zero(t, outSec.PausedTotal)
}

func TestMetricsEndpoint_TriggersSectionGatedAndReaperRecorded(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Enqueue one item through Server's durable queue so Enqueued increments.
	q := srv.TriggerEventQueue()
	require.NotNil(t, q)
	_, err := q.Enqueue(ctx, nil, durablequeue.Item{
		Kind:      trigger.EventQueueKind,
		DedupKey:  "metrics-item-1",
		ProjectID: api.NewUUID(),
		Payload:   []byte(`{"v":1,"event_id":"ev-m1"}`),
	})
	require.NoError(t, err)

	// Seed an expired firing row and run trigger maintenance so reaper metrics increment.
	projectID := api.NewUUID()
	now := time.Now().UTC()
	trID := api.NewUUID()
	evID := api.NewUUID()
	require.NoError(t, s.CreateTriggerEvent(ctx, &store.TriggerEvent{
		ID:         evID,
		ProjectID:  projectID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   api.NewUUID(),
		Type:       "webhook.received",
		Subject:    "webhook:test",
		DedupKey:   "reap-dedup-1",
		OccurredAt: now.Add(-20 * time.Minute),
		ReceivedAt: now.Add(-20 * time.Minute),
		Status:     store.TriggerEventStatusPending,
		CreatedAt:  now.Add(-20 * time.Minute),
	}))
	claimed, _, err := s.ClaimTriggerFire(ctx, &store.TriggerFire{
		ID:         api.NewUUID(),
		ProjectID:  projectID,
		TriggerID:  trID,
		EventID:    evID,
		Subject:    "webhook:test",
		ActionType: store.TriggerActionMessage,
		Status:     store.TriggerFireStatusFiring,
		LeaseUntil: now.Add(-15 * time.Minute),
		StartedAt:  now.Add(-25 * time.Minute),
		CreatedAt:  now.Add(-25 * time.Minute),
	})
	require.NoError(t, err)
	require.True(t, claimed)

	srv.runTriggerMaintenance(ctx)

	admin := NewAuthenticatedUser("admin-1", "admin@example.com", "Admin", "admin", "cli")

	// With hub.triggers off: /metrics omits triggers.
	reqOff := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	reqOff = reqOff.WithContext(contextWithIdentity(reqOff.Context(), admin))
	recOff := httptest.NewRecorder()
	srv.handleMetrics(recOff, reqOff)
	require.Equal(t, http.StatusOK, recOff.Code)
	var rawOff map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(recOff.Body.Bytes(), &rawOff))
	_, hasTriggersOff := rawOff["triggers"]
	assert.False(t, hasTriggersOff, "/metrics must omit triggers while hub.triggers is off")

	// With hub.triggers on: /metrics includes triggers with enqueued=1 and reapedTotal=1.
	setTriggersExperimentForObsTest(t, srv, true)
	reqOn := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	reqOn = reqOn.WithContext(contextWithIdentity(reqOn.Context(), admin))
	recOn := httptest.NewRecorder()
	srv.handleMetrics(recOn, reqOn)
	require.Equal(t, http.StatusOK, recOn.Code)
	var bodyOn struct {
		Triggers *TriggerMetricsSnapshot `json:"triggers"`
	}
	require.NoError(t, json.Unmarshal(recOn.Body.Bytes(), &bodyOn))
	require.NotNil(t, bodyOn.Triggers)
	assert.Equal(t, int64(1), bodyOn.Triggers.Queues[trigger.EventQueueKind].ItemsTotal[QueueOutcomeEnqueued])
	assert.Equal(t, int64(1), bodyOn.Triggers.Reaper.ReapedTotal)
	assert.Equal(t, int64(1), bodyOn.Triggers.Reaper.RunsTotal[TriggerReaperOutcomeOK])
	assert.NotNil(t, bodyOn.Triggers.Reaper.LastRunAt)
}

func seedHubAdminForObsTest(t *testing.T, s store.Store, projectID string) UserIdentity {
	t.Helper()
	ctx := context.Background()
	u := &store.User{
		ID:          api.NewUUID(),
		Email:       "obs-admin@example.com",
		DisplayName: "Obs Admin",
		Role:        store.UserRoleAdmin,
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, u))
	rd, err := s.GetRoleDefinitionByName(ctx, "hub-admin", store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               api.NewUUID(),
		RoleDefinitionID: rd.ID,
		PrincipalType:    "user",
		PrincipalID:      u.ID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
	})
	require.NoError(t, err)
	if projectID != "" {
		prd, err := s.GetRoleDefinitionByName(ctx, "project-owner", store.RoleScopeProject)
		require.NoError(t, err)
		_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
			ID:               api.NewUUID(),
			RoleDefinitionID: prd.ID,
			PrincipalType:    "user",
			PrincipalID:      u.ID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          projectID,
		})
		require.NoError(t, err)
	}
	return NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, store.UserRoleAdmin, "session")
}

func setTriggersExperimentForObsTest(t *testing.T, srv *Server, enabled bool) {
	t.Helper()
	var active []experiments.Experiment
	for _, e := range srv.experimentRegistry().All() {
		if e.Name == experiments.Triggers {
			e.Default = enabled
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	srv.experiments = reg
}
