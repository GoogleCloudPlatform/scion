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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScheduledMessage_WithoutWake_PhaseErrors(t *testing.T) {
	cases := []struct {
		name      string
		phase     state.Phase
		setup     func(t *testing.T, f *schedWake)
		wantParts func(slug string) []string
	}{
		{
			name:  "suspended suggests --wake",
			phase: state.PhaseSuspended,
			wantParts: func(slug string) []string {
				return []string{fmt.Sprintf("agent %q is suspended", slug), "use --wake to resume and deliver"}
			},
		},
		{
			name:  "suspended resume memory suggests --wake",
			phase: state.PhaseSuspended,
			setup: func(t *testing.T, f *schedWake) {
				ctx := context.Background()
				require.NoError(t, f.store.UpdateAgentStatus(ctx, f.target.ID, store.AgentStatusUpdate{Phase: string(state.PhaseStarting)}))
				launchID, err := f.store.BeginStartLaunch(ctx, f.target.ID, store.LaunchKindStart, 5*time.Minute, store.BeginLaunchOptions{PriorPhase: string(state.PhaseSuspended)})
				require.NoError(t, err)
				ans, _, err := f.store.ApplyLaunchReport(ctx, f.target.ID, f.target.RuntimeBrokerID, store.LaunchReport{
					LaunchID: launchID, InstanceID: "inst-1", State: store.LaunchReportStateFailed, ErrorCode: "resume_failed",
				})
				require.NoError(t, err)
				require.Equal(t, store.LaunchReportResultApplied, ans.Result)
			},
			wantParts: func(slug string) []string {
				return []string{fmt.Sprintf("agent %q is suspended", slug), "use --wake to resume and deliver"}
			},
		},
		{
			name:  "stopped suggests scion resume",
			phase: state.PhaseStopped,
			wantParts: func(slug string) []string {
				return []string{fmt.Sprintf("agent %q is stopped", slug), "scion resume"}
			},
		},
		{
			name:  "error suggests scion resume",
			phase: state.PhaseError,
			wantParts: func(slug string) []string {
				return []string{fmt.Sprintf("agent %q is in error state", slug), "scion resume"}
			},
		},
		{
			name:  "halted suggests scion start",
			phase: state.PhaseHalted,
			wantParts: func(slug string) []string {
				return []string{fmt.Sprintf("agent %q is halted", slug), "scion start"}
			},
		},
		{
			name:  "provisioning reports not yet running",
			phase: state.PhaseProvisioning,
			wantParts: func(slug string) []string {
				return []string{fmt.Sprintf("agent %q is not yet running (phase: provisioning)", slug)}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSchedWake(t, tc.phase)
			if tc.setup != nil {
				tc.setup(t, f)
			}
			u := f.user(t, "sw-phase-"+tc.name, "agent.message", "agent.read")

			err := f.fire(withSessionRevision(f.event(false), u.ID))
			require.Error(t, err)
			for _, part := range tc.wantParts(f.target.Slug) {
				assert.Contains(t, err.Error(), part)
			}
			assert.NotContains(t, err.Error(), "agent_not_found")
			assert.Empty(t, f.disp.getStartCalls())
			assert.Empty(t, f.disp.getMessageCalls(), "non-running target without wake must not dispatch to broker")
		})
	}
}

func TestScheduledMessage_WithoutWake_AuthzAndStandingPrecedePhaseCheck(t *testing.T) {
	t.Run("unauthorized caller does not learn target is suspended", func(t *testing.T) {
		f := newSchedWake(t, state.PhaseSuspended)
		outsider := &store.User{
			ID: api.NewUUID(), Email: "outsider-7825@example.com",
			DisplayName: "Outsider", Role: "member", Status: store.UserStatusActive,
		}
		require.NoError(t, f.store.CreateUser(context.Background(), outsider))

		err := f.fire(withSessionRevision(f.event(false), outsider.ID))
		require.Error(t, err)
		assert.Equal(t, errScheduledMessageRefused.Error(), err.Error())
		assert.NotContains(t, err.Error(), "suspended")
		assert.Empty(t, f.disp.getMessageCalls())
	})

	t.Run("held suspended target returns generic refusal", func(t *testing.T) {
		f := newSchedWake(t, state.PhaseSuspended)
		u := f.user(t, "sw-held-susp", "agent.message", "agent.read")
		_, err := f.store.CreateAgentHolds(context.Background(), []*store.AgentHold{{
			AgentID:           f.target.ID,
			ProjectID:         f.target.ProjectID,
			Cause:             store.AgentHoldCauseOwnerAccessEnded,
			RootPrincipalType: store.AgentHoldRootUser,
			RootPrincipalID:   u.ID,
			Trigger:           store.MembershipLossTriggerMemberRemove,
		}})
		require.NoError(t, err)

		fireErr := f.fire(withSessionRevision(f.event(false), u.ID))
		require.Error(t, fireErr)
		assert.Equal(t, errScheduledMessageRefused.Error(), fireErr.Error())
		assert.NotContains(t, fireErr.Error(), "suspended")
		assert.Empty(t, f.disp.getMessageCalls())
	})
}

func TestScheduledMessage_WithoutWake_RecordsInEventAndScheduleHistory(t *testing.T) {
	t.Run("one-shot event by agentName (scion message --in/--at shape)", func(t *testing.T) {
		f := newSchedWake(t, state.PhaseSuspended)
		u := f.user(t, "sw-oneshot-hist", "agent.message", "agent.read")
		ctx := context.Background()
		wantErr := fmt.Sprintf("agent %q is suspended; use --wake to resume and deliver", f.target.Slug)

		sched := NewScheduler(f.store, slog.Default())
		sched.RegisterEventHandler("message", f.srv.messageEventHandler())

		payload, err := json.Marshal(MessageEventPayload{
			AgentName: f.target.Slug,
			Message:   "delayed via --in",
		})
		require.NoError(t, err)

		evt := withSessionRevision(store.ScheduledEvent{
			ID:        api.NewUUID(),
			ProjectID: f.target.ProjectID,
			EventType: "message",
			Payload:   string(payload),
			FireAt:    time.Now(),
			Status:    store.ScheduledEventPending,
			CreatedBy: u.ID,
		}, u.ID)
		require.NoError(t, f.store.CreateScheduledEvent(ctx, &evt))

		sched.fireEvent(ctx, evt, false)

		got, err := f.store.GetScheduledEvent(ctx, evt.ID)
		require.NoError(t, err)
		assert.Equal(t, store.ScheduledEventFailed, got.Status)
		assert.Equal(t, wantErr, got.Error)
		assert.Empty(t, f.disp.getMessageCalls())
	})

	t.Run("recurring schedule records error on schedule and history event", func(t *testing.T) {
		f := newSchedWake(t, state.PhaseSuspended)
		u := f.user(t, "sw-recur-hist", "agent.message", "agent.read")
		ctx := context.Background()
		wantErr := fmt.Sprintf("agent %q is suspended; use --wake to resume and deliver", f.target.Slug)

		f.srv.scheduler = NewScheduler(f.store, slog.Default())
		f.srv.scheduler.RegisterEventHandler("message", f.srv.messageEventHandler())

		payload, err := json.Marshal(MessageEventPayload{
			AgentName: f.target.Slug,
			Message:   "recurring ping",
		})
		require.NoError(t, err)

		seeded := withSessionRevision(store.ScheduledEvent{}, u.ID)
		row := &store.Schedule{
			ID:                   api.NewUUID(),
			ProjectID:            f.target.ProjectID,
			Name:                 "ping-suspended",
			CronExpr:             "0 * * * *",
			EventType:            "message",
			Payload:              string(payload),
			Status:               store.ScheduleStatusActive,
			CreatedBy:            u.ID,
			InitiatorAttribution: seeded.InitiatorAttribution,
			AuthorityCeiling:     seeded.AuthorityCeiling,
		}
		require.NoError(t, f.store.CreateSchedule(ctx, row))

		f.srv.executeSchedule(ctx, *row, time.Now().UTC())

		gotSched, err := f.store.GetSchedule(ctx, row.ID)
		require.NoError(t, err)
		assert.Equal(t, "error", gotSched.LastRunStatus)
		assert.Equal(t, wantErr, gotSched.LastRunError)
		assert.Equal(t, 1, gotSched.RunCount)
		assert.Equal(t, 1, gotSched.ErrorCount)

		events, err := f.store.ListScheduledEvents(ctx, store.ScheduledEventFilter{ScheduleID: row.ID}, store.ListOptions{})
		require.NoError(t, err)
		require.Len(t, events.Items, 1)
		assert.Equal(t, store.ScheduledEventFailed, events.Items[0].Status)
		assert.Equal(t, wantErr, events.Items[0].Error)
		assert.Empty(t, f.disp.getMessageCalls())
	})
}

type raceBroker404Dispatcher struct {
	wakeTrackingDispatcher
	onMessage func(ctx context.Context, agent *store.Agent) error
}

func (d *raceBroker404Dispatcher) DispatchAgentMessage(ctx context.Context, agent *store.Agent, message string, interrupt bool, structuredMsg *messages.StructuredMessage) error {
	if d.onMessage != nil {
		return d.onMessage(ctx, agent)
	}
	return d.wakeTrackingDispatcher.DispatchAgentMessage(ctx, agent, message, interrupt, structuredMsg)
}

func TestScheduledMessage_PostDispatchBroker404Race(t *testing.T) {
	brokerNotFound := &brokerStatusError{
		StatusCode: http.StatusNotFound,
		Body:       `{"error":{"code":"agent_not_found","message":"Agent not found"}}`,
	}

	t.Run("target suspends between preflight read and broker dispatch", func(t *testing.T) {
		f := newSchedWake(t, state.PhaseRunning)
		u := f.user(t, "sw-race-susp", "agent.message", "agent.read")
		disp := &raceBroker404Dispatcher{
			onMessage: func(ctx context.Context, agent *store.Agent) error {
				_ = f.store.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{Phase: string(state.PhaseSuspended)})
				return brokerNotFound
			},
		}
		f.srv.SetDispatcher(disp)

		err := f.fire(withSessionRevision(f.event(false), u.ID))
		require.Error(t, err)
		assert.Equal(t, fmt.Sprintf("agent %q is suspended; use --wake to resume and deliver", f.target.Slug), err.Error())
		assert.NotContains(t, err.Error(), "agent_not_found")
		assert.NotContains(t, err.Error(), "404")
	})

	t.Run("broker container gone while store still shows running", func(t *testing.T) {
		f := newSchedWake(t, state.PhaseRunning)
		u := f.user(t, "sw-race-nocontainer", "agent.message", "agent.read")
		disp := &raceBroker404Dispatcher{
			onMessage: func(_ context.Context, _ *store.Agent) error {
				return brokerNotFound
			},
		}
		f.srv.SetDispatcher(disp)

		err := f.fire(withSessionRevision(f.event(false), u.ID))
		require.Error(t, err)
		assert.Equal(t, fmt.Sprintf("agent %q has no running container; message was not delivered", f.target.Slug), err.Error())
		assert.NotContains(t, err.Error(), "agent_not_found")
		assert.NotContains(t, err.Error(), "404")
	})
}

func TestHandleAgentMessage_WithoutWake_ResumeMemorySuspended(t *testing.T) {
	f := newSchedWake(t, state.PhaseSuspended)
	ctx := context.Background()
	u := f.user(t, "dm-resmem-susp", "agent.message", "agent.read")

	require.NoError(t, f.store.UpdateAgentStatus(ctx, f.target.ID, store.AgentStatusUpdate{Phase: string(state.PhaseStarting)}))
	launchID, err := f.store.BeginStartLaunch(ctx, f.target.ID, store.LaunchKindStart, 5*time.Minute, store.BeginLaunchOptions{PriorPhase: string(state.PhaseSuspended)})
	require.NoError(t, err)
	ans, _, err := f.store.ApplyLaunchReport(ctx, f.target.ID, f.target.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "inst-dm", State: store.LaunchReportStateFailed, ErrorCode: "resume_failed",
	})
	require.NoError(t, err)
	require.Equal(t, store.LaunchReportResultApplied, ans.Result)

	body, err := json.Marshal(MessageRequest{Message: "direct hello", Wake: false})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+f.target.ID+"/message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, "cli")))

	rec := httptest.NewRecorder()
	f.srv.handleAgentMessage(rec, req, f.target.ID)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), ErrCodeAgentNotRunning)
	assert.Contains(t, rec.Body.String(), "is suspended. Use --wake to resume and deliver.")
}
