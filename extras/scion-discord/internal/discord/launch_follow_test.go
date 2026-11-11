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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/bwmarrin/discordgo"
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
	assert.Contains(t, all, "⚠️ Couldn't confirm that agent `"+asyncAgentSlug+"` started: the hub refused the status request.")
	assert.Contains(t, all, "/scion status")
	assert.NotContains(t, all, asyncAgentID, "the agent ID stays in the log")
	assert.NotContains(t, all, "400", "the raw hub error stays in the log")
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

// snowflakeAt returns a Discord snowflake ID created at t.
func snowflakeAt(t time.Time) string {
	return strconv.FormatInt((t.UnixMilli()-1420070400000)<<22, 10)
}

func TestHandleThread_AsyncCreate_StopsAtInteractionDeadline(t *testing.T) {
	e, k := newAsyncThreadEnv(t)
	start := activeCreateLaunch("pod_create")
	zero := 0
	start.Launch.RemainingSeconds = &zero
	e.hub.createAgent = start
	e.hub.agentReads = []hubclient.Agent{*activeCreateLaunch("image_pull")}

	// The interaction arrived 13 min 10 s ago: the follow's deadline
	// (receipt plus the cap) has already passed.
	i := luCommand("thread", luStringOpt("title", "Fix the build"))
	i.ID = snowflakeAt(time.Now().Add(-(createLaunchWaitCap + 10*time.Second)))

	began := time.Now()
	e.commands.HandleThread(e.session, i)

	assert.Less(t, time.Since(began), 10*time.Second, "the follow must end at the interaction's deadline, not a cap from the follow start")
	all := e.discord.allBodies()
	assert.Contains(t, all, "⚠️ Couldn't confirm that agent `"+asyncAgentSlug+"` started: gave up after 13 min; the agent may still start.")
	assert.Empty(t, k.sends())
	assert.Empty(t, threadBinding(t, e))
}

func TestInteractionReceivedAt(t *testing.T) {
	at := time.Now().Add(-5 * time.Minute).Truncate(time.Millisecond)
	i := luCommand("thread")
	i.ID = snowflakeAt(at)
	assert.True(t, interactionReceivedAt(i).Equal(at), "got %v, want %v", interactionReceivedAt(i), at)

	i.ID = "not-a-snowflake"
	assert.WithinDuration(t, time.Now(), interactionReceivedAt(i), 5*time.Second)
}

func TestHandleThread_ThreadFailed_AsyncCreateSaysStarting(t *testing.T) {
	e, k := newAsyncThreadEnv(t)
	e.hub.createAgent = activeCreateLaunch("pod_create")
	e.discord.failPath = "/api/v" + discordgo.APIVersion + "/channels/" + luChannel + "/threads"

	runThreadCommand(e)

	all := e.discord.allBodies()
	assert.Contains(t, all, "Agent **"+asyncAgentSlug+"** was created but the Discord thread could not be created.")
	assert.Contains(t, all, "The agent is starting.")
	assert.Empty(t, e.hub.agentReadCalls(), "without a thread the launch is not followed")
	assert.Empty(t, k.sends())
}

func TestLaunchWaitErrorText(t *testing.T) {
	cases := map[string]struct {
		err  error
		want string
	}{
		"budget":   {fmt.Errorf("agent %q: %w", asyncAgentID, hubclient.ErrLaunchWaitTimeout), "its launch is still running on the hub"},
		"deleted":  {fmt.Errorf("agent %q: %w", asyncAgentID, hubclient.ErrAgentDeletedDuringLaunch), "the agent was deleted while it was starting"},
		"deadline": {fmt.Errorf("stopped waiting: %w", context.DeadlineExceeded), "gave up after 13 min; the agent may still start"},
		"rejected": {&hubclient.LaunchStatusRejectedError{AgentID: asyncAgentID, Err: errors.New("status 400")}, "the hub refused the status request"},
		"other":    {errors.New("boom"), "its status could not be read"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, launchWaitErrorText(tc.err, 13*time.Minute+10*time.Second))
		})
	}
}
