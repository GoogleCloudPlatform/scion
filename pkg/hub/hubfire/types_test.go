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

package hubfire_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/internal/principal"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

type stubTracker struct{}

func (stubTracker) Detach(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithCancel(ctx)
}

func (stubTracker) Admit(ctx context.Context) (context.Context, func(), bool) {
	return ctx, func() {}, true
}

func (stubTracker) Record(write func()) bool {
	write()
	return true
}

func TestFireTypesAndExecutorContextAlias(t *testing.T) {
	now := time.Date(2026, 11, 15, 2, 0, 0, 0, time.UTC)

	auth := hubfire.RecordedAuthority{
		Attribution: store.InitiatorAttribution{
			InitiatorPrincipalKind:  "user",
			InitiatorPrincipalID:    "u-1",
			InitiatorCredentialKind: store.InitiatorCredentialKindSession,
			AttributionVersion:      1,
			AuthorizationRevision:   2,
		},
		Ceiling:        store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
		DefinitionKind: hubfire.DefinitionKindTrigger,
		DefinitionID:   "trig-1",
		FireID:         "fire-1",
	}

	ev := &hubfire.FireEvent{
		ID:         "ev-1",
		ProjectID:  "proj-1",
		SourceKind: "webhook",
		SourceID:   "ep-1",
		Type:       "webhook.received",
		Subject:    "webhook:ep-1",
		OccurredAt: now,
		ReceivedAt: now,
		Attributes: map[string]string{"endpoint_name": "ci"},
		Payload:    []byte(`{"ok":true}`),
		Trigger:    &hubfire.FireTriggerRef{ID: "trig-1", Name: "deploy"},
	}

	// ExecutorContext must alias principal.ExecutorContext so pkg/hub and
	// pkg/hub/trigger share it without conversion.
	var execCtx principal.ExecutorContext = hubfire.ExecutorContext{
		Kind: "trigger_drainer",
		ID:   "trigger_event:ev-1",
	}

	req := hubfire.FireRequest{
		ProjectID:  "proj-1",
		Authority:  auth,
		ActionType: "message",
		Payload:    `{"agentId":"a-1","message":"hi"}`,
		Event:      ev,
		Executor:   execCtx,
		FireAt:     now,
		Tracked:    stubTracker{},
	}

	assert.Equal(t, "proj-1", req.ProjectID)
	assert.Equal(t, hubfire.DefinitionKindTrigger, req.Authority.DefinitionKind)
	assert.Equal(t, "schedule", hubfire.DefinitionKindSchedule)
	assert.Equal(t, "scheduled_event", hubfire.DefinitionKindScheduledEvent)
	assert.False(t, req.Executor.IsZero())
	require.NotNil(t, req.Event)
	require.NotNil(t, req.Event.Trigger)
	assert.Equal(t, "deploy", req.Event.Trigger.Name)

	ctx := principal.ContextWithExecutor(context.Background(), req.Executor)
	gotExec, ok := principal.ExecutorContextFromContext(ctx)
	require.True(t, ok)
	assert.Equal(t, req.Executor, gotExec)
}

func TestFireOutcomeAndStatuses(t *testing.T) {
	assert.Equal(t, "fired", hubfire.FireStatusFired.String())
	assert.Equal(t, "failed", hubfire.FireStatusFailed.String())
	assert.Equal(t, "refused", hubfire.FireStatusRefused.String())
	assert.Equal(t, "skipped", hubfire.FireStatusSkipped.String())

	called := false
	out := hubfire.FireOutcome{
		Status:    hubfire.FireStatusFired,
		Error:     "",
		ResultRef: "msg-1",
		AfterRecord: func(context.Context) {
			called = true
		},
	}
	assert.Equal(t, hubfire.FireStatusFired, out.Status)
	assert.Equal(t, "msg-1", out.ResultRef)
	require.NotNil(t, out.AfterRecord)
	out.AfterRecord(context.Background())
	assert.True(t, called)
}
