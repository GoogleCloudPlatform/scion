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
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/dialects"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/handlers"
	sciontoolhub "github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/testutil"
)

// opencodeStepFinishPayload returns the first step-finish record of the
// real OpenCode bridge capture (hook-payloads-1.18.33.jsonl): the one
// model-end a text-only OpenCode turn emits.
func opencodeStepFinishPayload(t *testing.T) map[string]interface{} {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "sciontool", "hooks", "dialects", "testdata", "opencode", "hook-payloads-1.18.33.jsonl"))
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var rec map[string]interface{}
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &rec))
		if rec["hook_event_name"] == "message.part.updated.step-finish" {
			return rec
		}
	}
	require.NoError(t, scanner.Err())
	t.Fatal("no step-finish record in the OpenCode fixture")
	return nil
}

// TestHarnessError_OpencodeEndToEnd is the in-process end-to-end check for
// ptone/scion#7936. An OpenCode turn that a model API rejected with 400 ends
// with the bridge's gated session.idle carrying "APIError (status 400)"
// (scion-bridge.js sessionErrorText). That payload goes through the real
// opencode dialect.yaml, the sciontool StatusHandler and HubHandler, the
// Hub's status endpoint and store. Then:
//   - the agent is running/harness_error with the reason, as a hubclient
//     Get (what scion list reads) sees it;
//   - a default-trigger subscriber gets a HARNESS_ERROR notification with
//     the reason;
//   - the stall sweep, run as if well past the stall threshold, stalls an
//     idle control agent but not the harness_error agent;
//   - a follow-up text-only turn (its step-finish, the real fixture record)
//     clears harness_error on the Hub.
//
// Not exercised here: a real opencode process and scion-bridge.js (the
// payload is the bridge's documented output), the container heartbeat, and
// the broker.
func TestHarnessError_OpencodeEndToEnd(t *testing.T) {
	ctx := context.Background()
	// Isolation: no live Hub, no real config or credentials, no files
	// outside t.TempDir(). Hub env is cleared before anything reads it.
	testutil.ScrubHubEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	// The sciontool client reads its token file from the scion user's home
	// when that user exists (an agent container), not from $HOME: point it
	// at the temp home so no real credential is read.
	t.Cleanup(sciontoolhub.SetTokenHome(home))
	srv, s := testServer(t)
	pub := NewChannelEventPublisher()
	t.Cleanup(pub.Close)
	srv.SetEventPublisher(pub)

	project := &store.Project{ID: api.NewUUID(), Name: "Harness Error E2E", Slug: "harness-error-e2e"}
	require.NoError(t, s.CreateProject(ctx, project))
	// Notifications to an agent subscriber are dispatched through its
	// Runtime Broker (here the recording dispatcher).
	broker := &store.RuntimeBroker{ID: api.NewUUID(), Name: "E2E Broker", Slug: "e2e-broker", Status: store.BrokerStatusOnline}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	newAgent := func(slug string, activity state.Activity, lastActivity time.Time) *store.Agent {
		a := &store.Agent{
			ID: api.NewUUID(), Slug: slug, Name: slug, ProjectID: project.ID,
			Phase: string(state.PhaseRunning), Activity: string(activity),
			Message: "Harness started", LastActivityEvent: lastActivity, LastSeen: time.Now(),
			RuntimeBrokerID: broker.ID,
		}
		require.NoError(t, s.CreateAgent(ctx, a))
		return a
	}
	longAgo := time.Now().Add(-time.Hour)
	watched := newAgent("oc-agent", state.ActivityWorking, longAgo)
	control := newAgent("idle-agent", state.ActivityWorking, longAgo)
	subscriber := newAgent("parent-agent", state.ActivityWaitingForInput, time.Now())

	sub := newNotifySubscription(project.ID, store.SubscriberTypeAgent, subscriber.Slug, "test")
	sub.AgentID = watched.ID
	sub.CreatedAt = time.Now().Add(-time.Minute)
	require.NoError(t, s.CreateNotificationSubscription(ctx, sub))
	recorder := &recordingDispatcher{}
	nd := NewNotificationDispatcher(s, pub, func() AgentDispatcher { return recorder }, slog.Default())
	nd.Start()
	t.Cleanup(nd.Stop)

	// The sciontool side, pointed at this Hub as the watched agent.
	hubHTTP := httptest.NewServer(srv.Handler())
	t.Cleanup(hubHTTP.Close)
	t.Setenv("SCION_HUB_ENDPOINT", hubHTTP.URL)
	agentToken, err := srv.GenerateAgentToken(watched.ID, project.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)
	t.Setenv("SCION_AUTH_TOKEN", agentToken)
	t.Setenv("SCION_AGENT_ID", watched.ID)
	require.NoError(t, os.WriteFile(filepath.Join(home, "agent-info.json"),
		[]byte(`{"phase":"running","activity":"working","detail":{"message":"Harness started"}}`), 0o644))
	statusHandler := handlers.NewStatusHandler()
	hubHandler := handlers.NewHubHandler()
	require.NotNil(t, hubHandler)
	dialect, err := dialects.LoadMappingDialect(testutil.HarnessDialectPath(t, "opencode"))
	require.NoError(t, err)
	hook := func(payload map[string]interface{}) {
		t.Helper()
		event, err := dialect.Parse(payload)
		require.NoError(t, err)
		require.NoError(t, statusHandler.Handle(event))
		require.NoError(t, hubHandler.Handle(event))
	}

	client, err := hubclient.New(hubHTTP.URL, hubclient.WithDevToken(testDevToken))
	require.NoError(t, err)
	getWatched := func() *hubclient.Agent {
		t.Helper()
		a, err := client.Agents().Get(ctx, watched.ID)
		require.NoError(t, err)
		return a
	}

	// 1. The 400 turn ends.
	hook(map[string]interface{}{
		"hook_event_name": "session.idle",
		"session_id":      "ses_mock400",
		"error":           "APIError (status 400)",
	})
	const reason = "model API: APIError (status 400)"
	got := getWatched()
	assert.Equal(t, string(state.PhaseRunning), got.Phase)
	assert.Equal(t, string(state.ActivityHarnessError), got.Activity)
	assert.Equal(t, reason, got.Message)

	// 2. The HARNESS_ERROR notification, with the reason.
	require.Eventually(t, func() bool { return len(recorder.getCalls()) == 1 }, 5*time.Second, 20*time.Millisecond)
	assert.Equal(t, "oc-agent has a HARNESS_ERROR (harness alive; a new message retries): "+reason,
		recorder.getCalls()[0].Message)
	assert.Equal(t, subscriber.Slug, recorder.getCalls()[0].Agent.Slug)
	notifs, err := s.GetNotifications(ctx, store.SubscriberTypeAgent, subscriber.Slug, false)
	require.NoError(t, err)
	require.Len(t, notifs, 1)
	assert.Equal(t, NotificationStatusHarnessError, notifs[0].Status)

	// 3. Well past the stall threshold (10 minutes on): the idle control
	// agent stalls, the harness_error agent does not.
	stalled, err := s.MarkStalledAgents(ctx, time.Now().Add(10*time.Minute), time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.Len(t, stalled, 1)
	assert.Equal(t, control.ID, stalled[0].ID)
	got = getWatched()
	assert.Equal(t, string(state.ActivityHarnessError), got.Activity)
	assert.Equal(t, reason, got.Message)
	assert.Empty(t, got.StalledFromActivity)

	// 4. A follow-up text-only turn clears it.
	hook(opencodeStepFinishPayload(t))
	got = getWatched()
	assert.Equal(t, string(state.ActivityWorking), got.Activity)
	assert.NotEqual(t, reason, got.Message)
}
