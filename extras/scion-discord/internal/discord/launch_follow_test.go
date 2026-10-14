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

package discord

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	asyncAgentID   = "agent-id-1"
	asyncAgentSlug = "fix-the-build"
)

// activeCreateLaunch is the agent of an accepted asynchronous create.
func activeCreateLaunch(step string) *hubclient.Agent {
	remaining := 60
	return &hubclient.Agent{
		ID:    asyncAgentID,
		Phase: "provisioning",
		Launch: &hubclient.AgentLaunch{
			ID: "launch-1", State: "active", Active: true, Kind: "create",
			Step: step, RemainingSeconds: &remaining,
		},
	}
}

// kickoffRecorder records kickoff deliveries and, for each, how many
// GET agent reads the fake hub had served by then.
type kickoffRecorder struct {
	mu          sync.Mutex
	hub         *linkedUserHub
	readsAtSend []int
}

func (k *kickoffRecorder) deliver(topic string, msg *messages.StructuredMessage) *hubError {
	k.hub.mu.Lock()
	reads := k.hub.agentReadN
	k.hub.mu.Unlock()
	k.mu.Lock()
	k.readsAtSend = append(k.readsAtSend, reads)
	k.mu.Unlock()
	return nil
}

func (k *kickoffRecorder) sends() []int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]int(nil), k.readsAtSend...)
}

// newAsyncThreadEnv returns a linked-user env with a fast launch poll and
// a recorded kickoff.
func newAsyncThreadEnv(t *testing.T) (*linkedUserEnv, *kickoffRecorder) {
	t.Helper()
	e := newLinkedUserEnv(t)
	e.linkChannel(t)
	e.commands.launchPollInterval = 10 * time.Millisecond
	k := &kickoffRecorder{hub: e.hub}
	e.commands.deliverInbound = k.deliver
	return e, k
}

func runThreadCommand(e *linkedUserEnv) {
	e.commands.HandleThread(e.session, luCommand("thread", luStringOpt("title", "Fix the build")))
}

func (h *linkedUserHub) createBodiesSnapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.createBodies...)
}

// agentReadCalls returns the recorded GET agent requests.
func (h *linkedUserHub) agentReadCalls() []recordedHubCall {
	var out []recordedHubCall
	for _, c := range h.snapshot() {
		if c.Method == http.MethodGet && strings.HasPrefix(c.Path, "/api/v1/projects/"+luProject+"/agents/") {
			out = append(out, c)
		}
	}
	return out
}

func threadBinding(t *testing.T, e *linkedUserEnv) string {
	t.Helper()
	slug, err := e.store.GetThreadDefault(context.Background(), luChannel, "thread-1")
	require.NoError(t, err)
	return slug
}

func TestHTTPHubClient_CreateAgent_DecodesLaunch(t *testing.T) {
	var gotBody CreateAgentRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"agent":{"id":"agent-id-1","slug":"a","name":"a","phase":"provisioning","status":"",` +
			`"launch":{"id":"launch-1","state":"active","active":true,"kind":"create","step":"pod_create","remainingSeconds":60}}}`))
	}))
	t.Cleanup(srv.Close)

	client := NewHTTPHubClient(srv.URL, "", "", nil)
	resp, err := client.CreateAgent(context.Background(), luProject, CreateAgentRequest{Name: "a", AcceptAsyncLaunch: true}, luPrincipal)
	require.NoError(t, err)

	assert.True(t, gotBody.AcceptAsyncLaunch, "the create must carry the opt-in")
	assert.Equal(t, asyncAgentID, resp.ID)
	assert.Equal(t, "a", resp.Slug)
	require.NotNil(t, resp.Agent, "the create answer's agent must be decoded")
	assert.Equal(t, "provisioning", resp.Agent.Phase)
	assert.True(t, hubclient.LaunchActive(resp.Agent))
	assert.Equal(t, "pod_create", resp.Agent.Launch.Step)
	require.NotNil(t, resp.Agent.Launch.RemainingSeconds)
	assert.Equal(t, 60, *resp.Agent.Launch.RemainingSeconds)
}

func TestHTTPHubClient_GetAgent_SignedAndDecoded(t *testing.T) {
	e := newLinkedUserEnv(t)
	e.hub.agentReads = []hubclient.Agent{*activeCreateLaunch("image_pull")}

	a, err := e.commands.hubClient.GetAgent(context.Background(), luProject, asyncAgentID, luPrincipal)
	require.NoError(t, err)
	assert.Equal(t, asyncAgentID, a.ID)
	assert.True(t, hubclient.LaunchActive(a))
	assert.Equal(t, "image_pull", a.Launch.Step)
	assertLinkedUserCalls(t, e.hub, http.MethodGet, "/api/v1/projects/"+luProject+"/agents/"+asyncAgentID)

	// The reader WaitForLaunch polls maps a hub 404 to an API not-found.
	e.hub.mu.Lock()
	e.hub.agentReads = nil
	e.hub.mu.Unlock()
	reader := launchAgentReader{hub: e.commands.hubClient, projectID: luProject, onBehalfOf: luPrincipal}
	_, err = reader.Get(context.Background(), asyncAgentID)
	require.Error(t, err)
	assert.True(t, apiclient.IsNotFoundError(err), "got %v", err)
}

func TestHandleThread_AsyncCreate_WaitsThenReady(t *testing.T) {
	e, k := newAsyncThreadEnv(t)
	e.hub.createAgent = activeCreateLaunch("pod_create")
	running := hubclient.Agent{ID: asyncAgentID, Slug: asyncAgentSlug, Phase: "running",
		Launch: &hubclient.AgentLaunch{ID: "launch-1", State: "ended", Kind: "create", EndReason: "running"}}
	e.hub.agentReads = []hubclient.Agent{*activeCreateLaunch("image_pull"), running}

	runThreadCommand(e)

	bodies := e.hub.createBodiesSnapshot()
	require.Len(t, bodies, 1)
	assert.Contains(t, bodies[0], `"acceptAsyncLaunch":true`)

	all := e.discord.allBodies()
	stepEdit := strings.Index(all, "Starting agent `"+asyncAgentSlug+"`… (`image_pull`)")
	ready := strings.Index(all, "✅ Ready — agent `"+asyncAgentSlug+"`")
	require.GreaterOrEqual(t, stepEdit, 0, "the status must show the launch step; got %s", all)
	require.GreaterOrEqual(t, ready, 0, "the status must end ready; got %s", all)
	assert.Less(t, stepEdit, ready, "the step edit must come before ready")

	sends := k.sends()
	require.Len(t, sends, 1, "the kickoff is sent once")
	assert.GreaterOrEqual(t, sends[0], 2, "the kickoff must wait for the read that shows the agent running")
	assert.Equal(t, asyncAgentSlug, threadBinding(t, e))
	assertLinkedUserCalls(t, e.hub, http.MethodGet, "/api/v1/projects/"+luProject+"/agents/"+asyncAgentID)
}

func TestHandleThread_AsyncCreate_FailureEdit(t *testing.T) {
	e, k := newAsyncThreadEnv(t)
	e.hub.createAgent = activeCreateLaunch("pod_create")
	e.hub.agentReads = []hubclient.Agent{{ID: asyncAgentID, Slug: asyncAgentSlug, Phase: "error",
		Message: "image registry.example/x:1 not found",
		Launch: &hubclient.AgentLaunch{ID: "launch-1", State: "ended", Kind: "create",
			Error: "image_pull_failed", EndReason: "failed", Step: "image_pull"}}}

	runThreadCommand(e)

	all := e.discord.allBodies()
	assert.Contains(t, all, "❌ Agent `"+asyncAgentSlug+"` failed to start (`image_pull_failed`): image registry.example/x:1 not found")
	assert.Contains(t, all, "Agent **"+asyncAgentSlug+"** was created but failed to start")
	assert.NotContains(t, all, "✅ Ready")
	assert.Empty(t, k.sends(), "no kickoff to an agent that failed to start")
	assert.Empty(t, threadBinding(t, e), "the thread is not bound to an agent that failed to start")
}

func TestHandleThread_AsyncCreate_WaitErrorNoKickoff(t *testing.T) {
	e, k := newAsyncThreadEnv(t)
	e.hub.createAgent = activeCreateLaunch("pod_create")
	e.hub.failRequest(http.MethodGet, "/api/v1/projects/"+luProject+"/agents/"+asyncAgentID,
		http.StatusBadRequest, `{"error":{"code":"validation_error","message":"bad agent id"}}`)

	runThreadCommand(e)

	all := e.discord.allBodies()
	assert.Contains(t, all, "⚠️ Stopped waiting for agent `"+asyncAgentSlug+"` to start")
	assert.Contains(t, all, "/scion status")
	assert.NotContains(t, all, "✅ Ready")
	assert.Empty(t, k.sends(), "no kickoff when the launch outcome is unknown")
	assert.Empty(t, threadBinding(t, e))
}

func TestHandleThread_SyncCreate_NoAgentRead(t *testing.T) {
	e, k := newAsyncThreadEnv(t)
	// The default fake create answer has no launch: a synchronous answer.

	runThreadCommand(e)

	assert.Empty(t, e.hub.agentReadCalls(), "a synchronous answer is not followed")
	assert.Contains(t, e.discord.allBodies(), "✅ Ready — agent `"+asyncAgentSlug+"`")
	assert.Len(t, k.sends(), 1)
	assert.Equal(t, asyncAgentSlug, threadBinding(t, e))
}

func TestHandleThread_AsyncCreateDisabled_NoOptIn(t *testing.T) {
	e, _ := newAsyncThreadEnv(t)
	e.commands.SetAsyncCreate(false)

	runThreadCommand(e)

	bodies := e.hub.createBodiesSnapshot()
	require.Len(t, bodies, 1)
	assert.NotContains(t, bodies[0], "acceptAsyncLaunch")
}

func TestConfigure_AsyncCreate(t *testing.T) {
	cases := map[string]struct {
		value   string
		want    bool
		wantErr bool
	}{
		"default": {"", true, false},
		"false":   {"false", false, false},
		"true":    {"true", true, false},
		"invalid": {"bogus", false, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			b := NewBroker(discardLogger())
			t.Cleanup(func() { _ = b.Close() })
			cfg := map[string]string{
				"bot_token": "Bot fake-token",
				"db_path":   filepath.Join(t.TempDir(), "async_create_test.db"),
			}
			if tc.value != "" {
				cfg["async_create"] = tc.value
			}
			err := b.Configure(cfg)
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "async_create")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, b.asyncCreate)
		})
	}
}

func TestLaunchFailureText(t *testing.T) {
	withCause := hubclient.LaunchOutcome{
		Agent:   &hubclient.Agent{Launch: &hubclient.AgentLaunch{Error: "runtime_error", Cause: "runtime_forbidden"}},
		Code:    "runtime_error",
		Message: "pods is forbidden",
	}
	assert.Equal(t, " (`runtime_error`, cause: `runtime_forbidden`): pods is forbidden", launchFailureText(withCause))

	noCode := hubclient.LaunchOutcome{Phase: "stopped"}
	assert.Equal(t, " (`phase: stopped`)", launchFailureText(noCode))
}
