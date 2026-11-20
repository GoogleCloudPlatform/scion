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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
)

type spyTriggerMetrics struct {
	mu      sync.Mutex
	ingress map[string]int
	fires   map[string]int
}

func (s *spyTriggerMetrics) RecordIngressEvent(sourceKind, result string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ingress == nil {
		s.ingress = make(map[string]int)
	}
	s.ingress[sourceKind+":"+result]++
}

func (s *spyTriggerMetrics) RecordTriggerFire(actionType, status, skipReason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fires == nil {
		s.fires = make(map[string]int)
	}
	s.fires[actionType+":"+status+":"+skipReason]++
}

func TestDispatcher_MetricsRecordsFireOutcomesAndSkipReasons(t *testing.T) {
	t0 := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	ms := newMemTriggerStore()
	projID := uuid.NewString()
	epID := uuid.NewString()

	mkTrigger := func(name string, rules []store.TriggerMatchRule) string {
		id := uuid.NewString()
		require.NoError(t, ms.CreateTrigger(context.Background(), &store.Trigger{
			ID:            id,
			ProjectID:     projID,
			Name:          name,
			SourceKind:    store.TriggerSourceKindWebhook,
			SourceID:      epID,
			MatchTypes:    []string{"webhook.received"},
			MatchRules:    rules,
			ActionType:    store.TriggerActionMessage,
			ActionPayload: `{"agentId":"a1","message":"hi"}`,
			SubjectMode:   store.TriggerSubjectModeNew,
			Status:        store.TriggerStatusActive,
		}))
		return id
	}

	idFired := mkTrigger("t-fired", nil)
	idFailed := mkTrigger("t-failed", nil)
	idRefused := mkTrigger("t-refused", nil)
	_ = mkTrigger("t-skipped", []store.TriggerMatchRule{{Dialect: store.TriggerDialectCEL, Expr: "true"}})

	core := &fakeCore{
		fireFn: func(_ context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
			switch req.Authority.DefinitionID {
			case idFired:
				return hubfire.FireOutcome{Status: hubfire.FireStatusFired}
			case idFailed:
				return hubfire.FireOutcome{Status: hubfire.FireStatusFailed, Error: "delivery failed"}
			case idRefused:
				return hubfire.FireOutcome{Status: hubfire.FireStatusRefused, Error: "creator suspended"}
			default:
				return hubfire.FireOutcome{Status: hubfire.FireStatusFired}
			}
		},
	}
	core.enabled.Store(true)

	evID := uuid.NewString()
	require.NoError(t, ms.CreateTriggerEvent(context.Background(), &store.TriggerEvent{
		ID:         evID,
		ProjectID:  projID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epID,
		Type:       "webhook.received",
		Subject:    "webhook:" + epID,
		DedupKey:   "d-metrics",
		OccurredAt: t0,
		ReceivedAt: t0,
		Payload:    `{"ok":true}`,
		Status:     store.TriggerEventStatusPending,
	}))

	spy := &spyTriggerMetrics{}
	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Core:    core,
		Store:   ms,
		Metrics: spy,
		Now:     func() time.Time { return t0 },
	})
	require.NoError(t, err)

	raw, err := json.Marshal(trigger.EventQueuePayload{Version: 1, EventID: evID})
	require.NoError(t, err)
	res := disp.HandleItem(context.Background(), durablequeue.Item{
		Seq:       1,
		Kind:      trigger.EventQueueKind,
		DedupKey:  evID,
		ProjectID: projID,
		Payload:   raw,
	}, nil)
	require.True(t, res.Done)

	assert.Equal(t, 1, spy.fires["message:fired:"])
	assert.Equal(t, 1, spy.fires["message:failed:"])
	assert.Equal(t, 1, spy.fires["message:refused:"])
	assert.Equal(t, 1, spy.fires["message:skipped:"+trigger.FireSkipReasonRuleEvaluationFailed])
}

func TestIngress_MetricsRecordsClosedSetOutcomes(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	epID := uuid.NewString()
	const secret = "bWV0cmljcy10ZXN0LXNlY3JldC0zMmJ5dGVzISEhIQ"

	spy := &spyTriggerMetrics{}
	outer := &outerStoreWithQueueFallbackTrap{
		ep: &store.WebhookEndpoint{
			ID:            epID,
			ProjectID:     uuid.NewString(),
			Name:          "metrics-ep",
			SecretVersion: 1,
			SignatureMode: store.WebhookSignatureModeScionV1,
			Status:        store.WebhookEndpointStatusActive,
		},
	}
	var secretErr error
	ing := trigger.NewIngress(trigger.IngressConfig{
		Store:      outer,
		QueueStore: outer,
		Queue:      &fakeEnqueueQueue{},
		LoadSecret: func(_ context.Context, _, _ string, _ int) (string, error) {
			if secretErr != nil {
				return "", secretErr
			}
			return secret, nil
		},
		Enabled: func() bool { return true },
		Metrics: spy,
		Now:     func() time.Time { return now },
	})

	// 1. Unknown endpoint (non-UUID path).
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/not-a-uuid", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	ing.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// 2. Oversize payload (> 64 KiB).
	bigBody := `{"x":"` + strings.Repeat("a", trigger.MaxPayloadBytes+10) + `"}`
	req = httptest.NewRequest(http.MethodPost, "/api/v1/hooks/"+epID, strings.NewReader(bigBody))
	req.RemoteAddr = "198.51.100.11:4000"
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	ing.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)

	// 3. Invalid signature.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/hooks/"+epID, strings.NewReader(`{"ok":true}`))
	req.RemoteAddr = "198.51.100.11:4000"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(trigger.HeaderSignature, "t=1,v1=deadbeef")
	rec = httptest.NewRecorder()
	ing.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	// 4. Secret unavailable (503).
	ing.InvalidateEndpoint(epID)
	secretErr = assert.AnError
	req = httptest.NewRequest(http.MethodPost, "/api/v1/hooks/"+epID, strings.NewReader(`{"ok":true}`))
	req.RemoteAddr = "198.51.100.11:4000"
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	ing.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)

	assert.Equal(t, 1, spy.ingress["webhook:"+trigger.IngressResultRejectedUnknownEndpoint])
	assert.Equal(t, 1, spy.ingress["webhook:"+trigger.IngressResultRejectedSize])
	assert.Equal(t, 1, spy.ingress["webhook:"+trigger.IngressResultRejectedSignature])
	assert.Equal(t, 1, spy.ingress["webhook:"+trigger.IngressResultRejectedSecretUnavailable])
}
