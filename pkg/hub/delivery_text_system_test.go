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

// Phase 9f tests: verify that the scheduler message delivery path and the
// notification dispatch path stamp DeliveryText when the envelope switch is ON,
// and leave it empty when the switch is OFF.

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Scheduler message delivery — DeliveryText
// ---------------------------------------------------------------------------

// TestPhase9f_Scheduler_DeliveryText_StampedWhenSwitchOn verifies that the
// messageEventHandler stamps DeliveryText on the dispatched StructuredMessage
// when the envelope switch is ON.
func TestPhase9f_Scheduler_DeliveryText_StampedWhenSwitchOn(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("9f-sched-on-project")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "9f-sched-on-project", Slug: "9f-sched-on-project",
	}))
	brokerID := tid("9f-sched-on-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: "9f-sched-on-broker", Slug: "9f-sched-on-broker",
		Status: store.BrokerStatusOnline,
	}))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: projectID, BrokerID: brokerID, BrokerName: "9f-sched-on-broker",
		Status: store.BrokerStatusOnline,
	}))
	agentID := tid("9f-sched-on-agent")
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: agentID, Name: "9f-sched-on-agent", Slug: "9f-sched-on-agent",
		ProjectID: projectID, RuntimeBrokerID: brokerID,
		Phase: "running", Visibility: store.VisibilityPrivate,
	}))

	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)

	// Enable the envelope switch.
	enableReadSwitch(t, srv)

	handler := srv.messageEventHandler()
	payload, _ := json.Marshal(MessageEventPayload{
		AgentName: "9f-sched-on-agent",
		Message:   "Phase 9f scheduler delivery text test",
	})
	evt := store.ScheduledEvent{
		ID:        api.NewUUID(),
		ProjectID: projectID,
		EventType: "message",
		Payload:   string(payload),
		Status:    store.ScheduledEventPending,
	}

	err := handler(ctx, evt)
	require.NoError(t, err, "messageEventHandler should succeed")

	calls := dispatcher.getCalls()
	require.Equal(t, 1, len(calls), "expected 1 dispatch call")
	require.NotNil(t, calls[0].StructuredMessage)
	if calls[0].StructuredMessage.DeliveryText == "" {
		t.Error("DeliveryText is empty — envelope switch is ON, scheduler messages should use the new envelope")
	}
}

// TestPhase9f_Scheduler_DeliveryText_EmptyWhenSwitchOff verifies that the
// messageEventHandler does NOT stamp DeliveryText when the envelope switch
// is OFF (the default).
func TestPhase9f_Scheduler_DeliveryText_EmptyWhenSwitchOff(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("9f-sched-off-project")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "9f-sched-off-project", Slug: "9f-sched-off-project",
	}))
	brokerID := tid("9f-sched-off-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: "9f-sched-off-broker", Slug: "9f-sched-off-broker",
		Status: store.BrokerStatusOnline,
	}))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: projectID, BrokerID: brokerID, BrokerName: "9f-sched-off-broker",
		Status: store.BrokerStatusOnline,
	}))
	agentID := tid("9f-sched-off-agent")
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: agentID, Name: "9f-sched-off-agent", Slug: "9f-sched-off-agent",
		ProjectID: projectID, RuntimeBrokerID: brokerID,
		Phase: "running", Visibility: store.VisibilityPrivate,
	}))

	dispatcher := &recordingDispatcher{}
	srv.SetDispatcher(dispatcher)

	// Do NOT enable the envelope switch — default is OFF.

	handler := srv.messageEventHandler()
	payload, _ := json.Marshal(MessageEventPayload{
		AgentName: "9f-sched-off-agent",
		Message:   "Phase 9f scheduler legacy test",
	})
	evt := store.ScheduledEvent{
		ID:        api.NewUUID(),
		ProjectID: projectID,
		EventType: "message",
		Payload:   string(payload),
		Status:    store.ScheduledEventPending,
	}

	err := handler(ctx, evt)
	require.NoError(t, err, "messageEventHandler should succeed")

	calls := dispatcher.getCalls()
	require.Equal(t, 1, len(calls), "expected 1 dispatch call")
	require.NotNil(t, calls[0].StructuredMessage)
	if calls[0].StructuredMessage.DeliveryText != "" {
		t.Errorf("DeliveryText should be empty when envelope switch is OFF, got %q",
			calls[0].StructuredMessage.DeliveryText)
	}
}

// ---------------------------------------------------------------------------
// Notification dispatch — DeliveryText
// ---------------------------------------------------------------------------

// TestPhase9f_Notification_DeliveryText_StampedWhenSwitchOn verifies that the
// notification dispatcher stamps DeliveryText on the dispatched
// StructuredMessage when the envelope switch is ON.
func TestPhase9f_Notification_DeliveryText_StampedWhenSwitchOn(t *testing.T) {
	env := setupNotificationTest(t)
	env.nd.writeDenyEnabled = func() bool { return true }
	env.nd.Start()
	defer env.nd.Stop()

	env.publishStatus("completed")

	require.Eventually(t, func() bool {
		return len(env.dispatcher.getCalls()) == 1
	}, 2*time.Second, 50*time.Millisecond)

	calls := env.dispatcher.getCalls()
	require.NotNil(t, calls[0].StructuredMessage)
	if calls[0].StructuredMessage.DeliveryText == "" {
		t.Error("DeliveryText is empty — envelope switch is ON, notifications should use the new envelope")
	}
}

// TestPhase9f_Notification_DeliveryText_EmptyWhenSwitchOff verifies that the
// notification dispatcher does NOT stamp DeliveryText when the envelope
// switch is OFF (writeDenyEnabled is nil, the default).
func TestPhase9f_Notification_DeliveryText_EmptyWhenSwitchOff(t *testing.T) {
	env := setupNotificationTest(t)
	// writeDenyEnabled is nil by default in setupNotificationTest — switch OFF.
	env.nd.Start()
	defer env.nd.Stop()

	env.publishStatus("completed")

	require.Eventually(t, func() bool {
		return len(env.dispatcher.getCalls()) == 1
	}, 2*time.Second, 50*time.Millisecond)

	calls := env.dispatcher.getCalls()
	require.NotNil(t, calls[0].StructuredMessage)
	if calls[0].StructuredMessage.DeliveryText != "" {
		t.Errorf("DeliveryText should be empty when envelope switch is OFF, got %q",
			calls[0].StructuredMessage.DeliveryText)
	}
}

// ---------------------------------------------------------------------------
// helpers (shared with other test files via package scope)
// ---------------------------------------------------------------------------

// enableReadSwitchOnND is a helper that sets the writeDenyEnabled callback
// on a NotificationDispatcher to always return true. This is the nd-level
// equivalent of enableReadSwitch (which operates on *Server).
func enableReadSwitchOnND(nd *NotificationDispatcher) {
	nd.writeDenyEnabled = func() bool { return true }
}

// newSchedulerTestServer creates a test Server with a dispatcher and
// operational settings suitable for exercising the messageEventHandler
// dispatch path. It is NOT a general-purpose replacement for testServer;
// it is purpose-built for the scheduler DeliveryText tests.
func newSchedulerTestServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	srv, s := testServer(t)
	srv.scheduler = NewScheduler(s, slog.Default())
	return srv, s
}
