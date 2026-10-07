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
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reincarnateTestDispatcher is a fake AgentDispatcher recording every call the
// reincarnation worker makes, with optional injected failures per step.
type reincarnateTestDispatcher struct {
	mu sync.Mutex

	stopCalls        int
	reprovisionCalls int
	startCalls       int
	lastStartTask    string
	lastStartResume  *bool

	stopErr        error
	reprovisionErr error
	startErr       error

	// reprovisionImage, when non-empty, simulates the broker echoing back a
	// resolved container image on a successful reprovision response — as the
	// real HTTP dispatcher's applyBrokerResponse does by mutating
	// agent.AppliedConfig.Image in place (design §3.4 Amendment A11.1(b)).
	reprovisionImage string

	// startImage, when non-empty, simulates the broker echoing back a
	// resolved container image on a successful start response.
	startImage string

	// imageRegistry is the registry the fake reports through ImageRegistry(),
	// as the HTTP dispatcher reports the registry it rewrites images to.
	imageRegistry string

	// reprovisionEcho and startEcho, when set, run on a successful dispatch
	// against the dispatched agent's AppliedConfig, simulating the broker
	// echo applyBrokerAgentConfig writes in place (HarnessConfig,
	// HarnessAuth, Image, Profile).
	reprovisionEcho func(cfg *store.AgentAppliedConfig)
	startEcho       func(cfg *store.AgentAppliedConfig)

	// rerenderErr is returned by every reprovision call after the first:
	// the worker's best-effort re-render of the previous config
	// (ptone/scion#1935). reprovisionErr applies to the first call only.
	// Move dispatch (agentMoveDispatcher): calls record the broker each
	// went to; errors make that call fail.
	moveProvisionCalls   []string // "<brokerID>|<expected workspace>"
	moveProvisionErr     error
	localOnlyDeleteCalls []string // "<brokerID>|<runID>"
	localOnlyDeleteErr   map[string]error
	startBrokers         []string
	// runStore, when set, makes DispatchAgentStart mint and record a run
	// on the row first, like the real dispatcher's beginRun, and record
	// startPlacement (when set), like a target's start report.
	runStore       store.Store
	startPlacement string
	// stopHook, when set, runs inside DispatchAgentStop.
	stopHook func()

	rerenderErr error
	// rerenderEcho, when set, runs on a successful re-render against the
	// dispatched AppliedConfig, simulating the broker's echo.
	rerenderEcho func(cfg *store.AgentAppliedConfig)
	// reprovisionConfigs snapshots the AppliedConfig of every reprovision
	// dispatch, in call order, as the worker sent it.
	reprovisionConfigs []store.AgentAppliedConfig
}

// reprovisionSnapshot returns the reprovision call count and a copy of the
// dispatched configs.
func (d *reincarnateTestDispatcher) reprovisionSnapshot() (int, []store.AgentAppliedConfig) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.reprovisionCalls, append([]store.AgentAppliedConfig(nil), d.reprovisionConfigs...)
}

func (d *reincarnateTestDispatcher) ImageRegistry() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.imageRegistry
}

func newReincarnateTestDispatcher() *reincarnateTestDispatcher {
	return &reincarnateTestDispatcher{}
}

func (d *reincarnateTestDispatcher) DispatchAgentCreate(context.Context, *store.Agent) (*CreateDispatchResult, error) {
	return nil, nil
}
func (d *reincarnateTestDispatcher) DispatchAgentProvision(context.Context, *store.Agent) error {
	return nil
}
func (d *reincarnateTestDispatcher) DispatchAgentReprovision(_ context.Context, agent *store.Agent) error {
	d.mu.Lock()
	d.reprovisionCalls++
	if agent.AppliedConfig != nil {
		d.reprovisionConfigs = append(d.reprovisionConfigs, *agent.AppliedConfig)
	}
	err := d.reprovisionErr
	image := d.reprovisionImage
	echo := d.reprovisionEcho
	if d.reprovisionCalls > 1 {
		err, image, echo = d.rerenderErr, "", d.rerenderEcho
	}
	d.mu.Unlock()
	if err == nil && image != "" && agent.AppliedConfig != nil {
		agent.AppliedConfig.Image = image
	}
	if err == nil && echo != nil && agent.AppliedConfig != nil {
		echo(agent.AppliedConfig)
	}
	return err
}
func (d *reincarnateTestDispatcher) DispatchAgentProvisionForMove(_ context.Context, agent *store.Agent, expect string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.moveProvisionCalls = append(d.moveProvisionCalls, agent.RuntimeBrokerID+"|"+expect)
	return d.moveProvisionErr
}
func (d *reincarnateTestDispatcher) DispatchAgentDeleteLocalOnly(_ context.Context, agent *store.Agent) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.localOnlyDeleteCalls = append(d.localOnlyDeleteCalls, agent.RuntimeBrokerID+"|"+agent.RunID)
	return d.localOnlyDeleteErr[agent.RuntimeBrokerID]
}
func (d *reincarnateTestDispatcher) moveSnapshot() (provisions, localDeletes, startBrokers []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.moveProvisionCalls...), append([]string(nil), d.localOnlyDeleteCalls...), append([]string(nil), d.startBrokers...)
}
func (d *reincarnateTestDispatcher) DispatchAgentStart(ctx context.Context, agent *store.Agent, task string, resume bool) error {
	d.mu.Lock()
	if d.runStore != nil {
		runID := fmt.Sprintf("run-%s-%d", agent.RuntimeBrokerID, len(d.startBrokers)+1)
		if _, err := d.runStore.SetAgentRunID(ctx, agent.ID, runID); err == nil {
			agent.RunID = runID
		}
		if d.startPlacement != "" {
			_ = d.runStore.SetAgentWorkspacePlacement(ctx, agent.ID, d.startPlacement)
		}
	}
	d.startBrokers = append(d.startBrokers, agent.RuntimeBrokerID)
	d.startCalls++
	d.lastStartTask = task
	d.lastStartResume = &resume
	err := d.startErr
	image := d.startImage
	echo := d.startEcho
	d.mu.Unlock()
	if err == nil && image != "" && agent.AppliedConfig != nil {
		agent.AppliedConfig.Image = image
	}
	if err == nil && echo != nil && agent.AppliedConfig != nil {
		echo(agent.AppliedConfig)
	}
	return err
}
func (d *reincarnateTestDispatcher) DispatchAgentStop(_ context.Context, _ *store.Agent) error {
	d.mu.Lock()
	d.stopCalls++
	err := d.stopErr
	hook := d.stopHook
	d.mu.Unlock()
	if hook != nil {
		hook()
	}
	return err
}
func (d *reincarnateTestDispatcher) DispatchAgentRestart(context.Context, *store.Agent) error {
	return nil
}
func (d *reincarnateTestDispatcher) DispatchAgentResetAuth(context.Context, *store.Agent) error {
	return nil
}
func (d *reincarnateTestDispatcher) DispatchAgentDelete(context.Context, *store.Agent, bool, bool, bool, time.Time) error {
	return nil
}
func (d *reincarnateTestDispatcher) DispatchAgentMessage(context.Context, *store.Agent, string, bool, *messages.StructuredMessage) error {
	return nil
}
func (d *reincarnateTestDispatcher) DispatchAgentLogs(context.Context, *store.Agent, int) (string, error) {
	return "", nil
}
func (d *reincarnateTestDispatcher) DispatchAgentExec(context.Context, *store.Agent, []string, int) (string, int, error) {
	return "", 0, nil
}
func (d *reincarnateTestDispatcher) DispatchCheckAgentPrompt(context.Context, *store.Agent) (bool, error) {
	return false, nil
}
func (d *reincarnateTestDispatcher) DispatchAgentCreateWithGather(context.Context, *store.Agent) (*CreateDispatchResult, error) {
	return nil, nil
}
func (d *reincarnateTestDispatcher) DispatchFinalizeEnv(context.Context, *store.Agent, map[string]string) (*CreateDispatchResult, error) {
	return nil, nil
}

// waitForReincarnationSettled polls the store until the agent's most recent
// AgentReincarnation record reaches a terminal state (completed or failed) —
// the LAST write the worker makes on either path — or fails the test after
// timeout. The worker runs in a detached background goroutine (design §3.1),
// so tests synchronize on persisted store state, the actual observable
// outcome, rather than on dispatcher call timing: the failure path's
// UpdateAgent/UpdateAgentReincarnation calls happen strictly after
// DispatchAgentStart returns its error, so signaling on the dispatcher call
// itself would race the worker's own post-dispatch bookkeeping.
// waitForReincarnationSettled waits until BOTH the record and the agent row
// have reached a terminal, consistent state (design §3.4 Amendment A8.3): the
// record must be completed/failed, the agent's reincarnation_state must be
// back to ""/failed, and — for a completed record specifically — the agent's
// Generation must have reached rec.ToGeneration. Waiting on the record alone
// is not enough: the A6 CAS ordering always advances the record BEFORE the
// matching agent-row write (see tryAdvanceReincarnation's callers), so a
// caller that reads the agent row right after this returned could still
// observe it mid-transition — for example still "starting" a moment after
// the record already reads "completed". This raced several tests
// intermittently before the two waits were tied together.
func waitForReincarnationSettled(t *testing.T, s store.Store, agentID string) *store.AgentReincarnation {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(5 * time.Second)
	for {
		list, err := s.ListAgentReincarnations(ctx, agentID)
		require.NoError(t, err)
		if len(list) > 0 {
			rec := list[0]
			switch rec.State {
			case store.AgentReincarnationStateCompleted, store.AgentReincarnationStateFailed:
				agent, err := s.GetAgent(ctx, agentID)
				require.NoError(t, err)
				agentSettled := agent.ReincarnationState == store.ReincarnationStateNone || agent.ReincarnationState == store.ReincarnationStateFailed
				generationSettled := rec.State != store.AgentReincarnationStateCompleted || agent.Generation >= rec.ToGeneration
				if agentSettled && generationSettled {
					return rec
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for reincarnation to settle for agent %s", agentID)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// setupReincarnateTestServer creates a project, an online broker with the
// reprovision capability, and wires the given dispatcher.
func setupReincarnateTestServer(t *testing.T, disp AgentDispatcher) (*Server, store.Store, *store.Project, *store.RuntimeBroker) {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   tid("reincarnate-project-" + t.Name()),
		Name: "Reincarnate Test Project",
		Slug: "reincarnate-test-project-" + tidSlugSafe(t.Name()),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	broker := &store.RuntimeBroker{
		ID:           tid("reincarnate-broker-" + t.Name()),
		Name:         "Reincarnate Test Broker",
		Slug:         "reincarnate-test-broker-" + tidSlugSafe(t.Name()),
		Status:       store.BrokerStatusOnline,
		Capabilities: &store.BrokerCapabilities{Reprovision: true},
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	provider := &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}
	require.NoError(t, s.AddProjectProvider(ctx, provider))
	project.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, s.UpdateProject(ctx, project))

	srv.SetDispatcher(disp)
	return srv, s, project, broker
}

// tidSlugSafe returns a lowercase, slug-safe fragment derived from a test
// name (which may contain "/" from subtests).
func tidSlugSafe(name string) string {
	out := make([]byte, 0, len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			out = append(out, c)
		case c >= 'A' && c <= 'Z':
			out = append(out, c-'A'+'a')
		default:
			out = append(out, '-')
		}
	}
	return string(out)
}

// newReincarnateTestAgent creates a fully-formed agent (with CreateInputs, as
// the create path would leave it) ready for a reincarnate test.
func newReincarnateTestAgent(t *testing.T, s store.Store, project *store.Project, broker *store.RuntimeBroker, mutate func(a *store.Agent)) *store.Agent {
	t.Helper()
	ctx := context.Background()
	// The agent's creator is a live project member, so the agent is in good
	// standing (ptone/scion#3433).
	ensureStandingRoot(t, s, project.ID, tid("user-creator"))

	a := &store.Agent{
		ID:              tid("reincarnate-agent-" + t.Name()),
		Slug:            "reincarnate-agent-" + tidSlugSafe(t.Name()),
		Name:            "Reincarnate Test Agent",
		Template:        "",
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           "running",
		CreatedBy:       tid("user-creator"),
		OwnerID:         tid("user-creator"),
		Ancestry:        []string{tid("user-creator")},
		MessageMode:     "project",
		Labels:          map[string]string{"team": "platform"},
		AppliedConfig: &store.AgentAppliedConfig{
			Image:       "old-image:v1",
			HarnessAuth: "api-key",
			CreatorName: "user-creator",
			AgentRole:   "baseline",
			Workspace:   "/tmp/reincarnate-workspace",
			// GitClone: clone-per-agent, the only workspace mode Phase 1
			// supports (design §3.4 Amendment A2); a test that wants the
			// non-clone-per-agent rejection path sets this to nil explicitly.
			GitClone: &api.GitCloneConfig{URL: "https://example.com/reincarnate-test-repo.git"},
			CreateInputs: &store.AgentCreateInputs{
				Workspace: "/tmp/reincarnate-workspace",
			},
		},
	}
	if mutate != nil {
		mutate(a)
	}
	require.NoError(t, s.CreateAgent(ctx, a))
	return a
}

func agentIdentityFor(agentID, projectID string, scopes ...AgentTokenScope) AgentIdentity {
	return &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agentID},
		ProjectID: projectID,
		Scopes:    scopes,
	}}
}

// delegatingRequesterFor returns an agent identity for requesterID that may
// reincarnate a baseline agent in projectID: the lifecycle scope plus every
// scope of the baseline role, which re-recording the target's authority
// under the requester requires (CanDelegate).
func delegatingRequesterFor(requesterID, projectID string) AgentIdentity {
	return agentIdentityFor(requesterID, projectID, append(ScopesForRole(AgentRoleBaseline), ScopeAgentLifecycle)...)
}

func reincarnateRequest(t *testing.T, agentID string, identity Identity, body interface{}) *http.Request {
	t.Helper()
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentID+"/reincarnate", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	if identity != nil {
		req = req.WithContext(contextWithIdentity(req.Context(), identity))
	}
	return req
}

// =============================================================================
// Authorization (design §3.8, decision D2)
// =============================================================================

func TestReincarnateAgent_Authz_Unauthenticated(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)

	req := reincarnateRequest(t, agent.ID, nil, ReincarnateAgentRequest{DryRun: true})
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, req, agent.ID)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestReincarnateAgent_Authz_SelfAnyRole(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.AppliedConfig.AgentRole = "readonly" // no lifecycle scope at all
	})

	// Self, with NO scopes whatsoever: still allowed per D2.
	self := agentIdentityFor(agent.ID, project.ID)
	req := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{DryRun: true})
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, req, agent.ID)

	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestReincarnateAgent_Authz_OtherAgentRequiresLifecycleScope(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)

	t.Run("missing scope is denied", func(t *testing.T) {
		other := agentIdentityFor(tid("coordinator"), project.ID)
		req := reincarnateRequest(t, agent.ID, other, ReincarnateAgentRequest{DryRun: true})
		rec := httptest.NewRecorder()
		srv.handleReincarnateAgent(rec, req, agent.ID)
		assert.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("cross-project is denied even with the scope", func(t *testing.T) {
		other := agentIdentityFor(tid("coordinator"), tid("some-other-project"), ScopeAgentLifecycle)
		req := reincarnateRequest(t, agent.ID, other, ReincarnateAgentRequest{DryRun: true})
		rec := httptest.NewRecorder()
		srv.handleReincarnateAgent(rec, req, agent.ID)
		assert.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("same project with the scope and delegation authority is allowed", func(t *testing.T) {
		coordinator := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
			a.ID = tid("coordinator")
			a.Slug = "coordinator-" + tidSlugSafe(t.Name())
			a.Name = "Coordinator"
		})
		other := delegatingRequesterFor(coordinator.ID, project.ID)
		req := reincarnateRequest(t, agent.ID, other, ReincarnateAgentRequest{DryRun: true})
		rec := httptest.NewRecorder()
		srv.handleReincarnateAgent(rec, req, agent.ID)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})
}

func TestReincarnateAgent_Authz_UserViaHTTP(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)

	// The dev-auth identity used by doRequest is granted broad access in
	// tests, exercising the "user"/"dev" branch through the full HTTP path
	// (mux + route dispatch), not just the handler function directly.
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/reincarnate", ReincarnateAgentRequest{DryRun: true})
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// =============================================================================
// Validation
// =============================================================================

func TestReincarnateAgent_RejectsUnsupportedOverrides(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)

	cases := []ReincarnateAgentRequest{
		{HarnessConfig: "gemini"},
		// Patch fields with invalid values (ptone/scion#3302).
		{HarnessAuth: "oauth"},
		{Role: "superuser"},
		{ThinkingLevel: intPtr(101)},
		{ThinkingLevel: intPtr(-1)},
		{Env: map[string]string{"K": "V"}},
		{TemplateHash: "abc123"},
		{ResetOverrides: true},
		{Rollback: true},
	}
	for _, tc := range cases {
		req := reincarnateRequest(t, agent.ID, self, tc)
		rec := httptest.NewRecorder()
		srv.handleReincarnateAgent(rec, req, agent.ID)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "override request %+v should be rejected", tc)
	}
}

// TestBrokerHeartbeat_RefreshesCapabilities is the design §3.4 Amendment A2
// regression test for the chosen fix (heartbeat, not a live hub->broker /info query —
// see hubclient.BrokerHeartbeat.Capabilities' doc comment for why): a
// heartbeat that reports capabilities must overwrite whatever
// CompleteBrokerJoin last stored, so an already-registered broker's
// Reprovision capability is never stuck stale until a manual --force
// re-registration.
func TestBrokerHeartbeat_RefreshesCapabilities(t *testing.T) {
	srv, s := testServer(t)
	grantDevUserRuntimeBrokerAccess(t, s)
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:     tid("broker-cap-refresh"),
		Name:   "Cap Refresh Broker",
		Slug:   "cap-refresh-broker",
		Status: store.BrokerStatusOnline,
		// nil: simulates a broker registered before Reprovision existed —
		// the exact false-412 case this heartbeat-refresh fix addresses.
		Capabilities: nil,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	heartbeat := brokerHeartbeatRequest{
		Status:       "online",
		Capabilities: &store.BrokerCapabilities{Sync: true, Attach: true, Reprovision: true},
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/runtime-brokers/"+broker.ID+"/heartbeat", heartbeat)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	updated, err := s.GetRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err)
	require.NotNil(t, updated.Capabilities)
	assert.True(t, updated.Capabilities.Reprovision,
		"a heartbeat reporting capabilities must refresh the stored ones")
}

// TestBrokerHeartbeat_NoCapabilities_LeavesStoredCapabilitiesUntouched
// covers the other half: an old broker's heartbeat has no Capabilities field
// at all, and that must not be misread as "clear the stored capabilities" —
// the field is a "refresh if present" signal, not a full replace.
func TestBrokerHeartbeat_NoCapabilities_LeavesStoredCapabilitiesUntouched(t *testing.T) {
	srv, s := testServer(t)
	grantDevUserRuntimeBrokerAccess(t, s)
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:           tid("broker-cap-old"),
		Name:         "Old Broker",
		Slug:         "old-broker",
		Status:       store.BrokerStatusOnline,
		Capabilities: &store.BrokerCapabilities{Sync: true, Attach: true, Reprovision: true},
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/runtime-brokers/"+broker.ID+"/heartbeat",
		brokerHeartbeatRequest{Status: "online"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	updated, err := s.GetRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err)
	require.NotNil(t, updated.Capabilities)
	assert.True(t, updated.Capabilities.Reprovision,
		"a heartbeat with no Capabilities field must not clear the stored ones")
}

// TestEmbeddedBrokerCapabilities_PassesReincarnateGate is the design §3.4
// Amendment A2 regression test: an embedded broker's capabilities (as cmd/server_broker.go
// now writes them, on both create and update) must pass the reincarnate 412
// gate, unlike the pre-fix Capabilities{Sync,Attach} literal that omitted
// Reprovision entirely.
func TestEmbeddedBrokerCapabilities_PassesReincarnateGate(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, _ := setupReincarnateTestServer(t, disp)

	// Mirrors cmd/server_broker.go's embedded-broker Capabilities literal
	// (design §3.4 Amendment A2), rather than importing cmd (which would
	// pull the whole CLI package into this test binary).
	embeddedBroker := &store.RuntimeBroker{
		ID:           tid("embedded-broker-" + t.Name()),
		Name:         "embedded",
		Slug:         "embedded-" + tidSlugSafe(t.Name()),
		Status:       store.BrokerStatusOnline,
		Capabilities: &store.BrokerCapabilities{WebPTY: false, Sync: true, Attach: true, Reprovision: true},
	}
	require.NoError(t, s.CreateRuntimeBroker(context.Background(), embeddedBroker))

	agent := newReincarnateTestAgent(t, s, project, embeddedBroker, nil)
	self := agentIdentityFor(agent.ID, project.ID)

	req := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{DryRun: true})
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, req, agent.ID)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// AC-9: migrating an agent on a broker without the capability returns 412,
// and the agent is untouched.
func TestReincarnateAgent_AC9_OldBrokerReturns412AndAgentUntouched(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	// Downgrade the broker to lack the capability.
	broker.Capabilities = &store.BrokerCapabilities{Reprovision: false}
	require.NoError(t, s.UpdateRuntimeBroker(context.Background(), broker))

	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	beforeVersion := agent.StateVersion
	self := agentIdentityFor(agent.ID, project.ID)

	req := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "handoff"})
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, req, agent.ID)

	assert.Equal(t, http.StatusPreconditionFailed, rec.Code)

	after, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, beforeVersion, after.StateVersion, "agent must be completely untouched on a 412")
	assert.Equal(t, 1, after.Generation)
	assert.Equal(t, "", after.ReincarnationState)

	list, err := s.ListAgentReincarnations(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Empty(t, list, "no reincarnation record should be created on a 412")
}

// TestReincarnateAgent_AC9_NilCapabilities_Returns412 covers a broker that
// has never reported capabilities at all (e.g. one registered before the
// reprovision capability existed) — nil, not merely Reprovision: false — and
// must be treated the same as an explicitly-old broker.
func TestReincarnateAgent_AC9_NilCapabilities_Returns412(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	broker.Capabilities = nil
	require.NoError(t, s.UpdateRuntimeBroker(context.Background(), broker))

	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)

	req := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "handoff"})
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, req, agent.ID)

	assert.Equal(t, http.StatusPreconditionFailed, rec.Code)
}

// TestReincarnateAgent_WorktreePerAgentOrNeitherWorkspace_Returns400 is the
// design §3.4 Amendment A2/A4/A23 regression test: worktree-per-agent is
// still rejected, and so is the "neither GitClone nor Workspace" case A23
// explicitly keeps a 400 for. (Shared-workspace and hub-managed agents are
// now ELIGIBLE via the explicit-mount case — see
// TestReincarnateAgent_ExplicitMountWorkspace_Eligible — so they are no
// longer covered here.) Rejection must happen before anything is persisted
// or computed — including on a dry run, so --dry-run reports the restriction
// instead of showing a plan a real request could not safely execute.
//
// A fabricated predecessor of this test set AppliedConfig.GitClone = nil
// directly, which no real worktree-per-agent agent ever has: populateAgentConfig
// sets GitClone for every git-remote
// project that isn't shared-workspace, worktree-per-agent included. So a
// worktree-per-agent agent's GitClone is non-nil and used to slip past a
// gate that only checked for nil. Each case here derives GitClone (or its
// absence) the same way populateAgentConfig actually would, for a project
// carrying the real workspace-mode label.
func TestReincarnateAgent_WorktreePerAgentOrNeitherWorkspace_Returns400(t *testing.T) {
	const genericMsg = "reincarnate requires a clone-per-agent, shared-workspace or hub-managed workspace"
	const worktreeMsg = "reincarnate does not yet support worktree-per-agent workspaces"
	cases := []struct {
		name           string
		workspaceMode  string // "" = clone-per-agent (the default for a git-remote project)
		clearWorkspace bool   // force the "neither GitClone nor Workspace" edge case
		nonGit         bool   // project has no git remote
		wantRejected   bool
		wantBodyText   string // O1 (review p1b-r1): pin the exact 400 message
	}{
		{
			name:          "worktree-per-agent: GitClone is set, but this is not clone-per-agent",
			workspaceMode: store.WorkspaceModeWorktreePerAgent,
			wantRejected:  true,
			wantBodyText:  worktreeMsg,
		},
		{
			name:           "neither GitClone nor Workspace",
			workspaceMode:  store.WorkspaceModeShared,
			clearWorkspace: true,
			wantRejected:   true,
			wantBodyText:   genericMsg,
		},
		{
			name:          "clone-per-agent: eligible",
			workspaceMode: "",
			wantRejected:  false,
		},
		{
			// miller79/scion#167: same-broker empty-per-agent is
			// eligible only on a local-disk runtime. This fixture agent
			// has no recorded runtime and its broker reports no
			// profiles, so resolveAgentRuntime yields "" and the
			// allow-list refuses it (fail closed). The positive cases
			// are TestReincarnateAgent_SameBrokerEmptyPerAgent_Eligible
			// and ..._UnrecordedRuntime.
			name:          "empty-per-agent (non-git per-agent), no recorded or resolvable runtime: refused",
			workspaceMode: store.WorkspaceModePerAgent,
			nonGit:        true,
			wantRejected:  true,
			wantBodyText:  "empty-per-agent reincarnation is supported only on local-disk runtimes (docker, podman, container)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			disp := newReincarnateTestDispatcher()
			srv, s, project, broker := setupReincarnateTestServer(t, disp)
			ctx := context.Background()

			if !tc.nonGit {
				project.GitRemote = "https://example.com/repo.git"
			}
			if tc.workspaceMode != "" {
				project.Labels = map[string]string{store.LabelWorkspaceMode: tc.workspaceMode}
			}
			require.NoError(t, s.UpdateProject(ctx, project))

			// What the create path actually produces for this project —
			// not a hand-set field — so this test breaks if
			// populateAgentConfig's GitClone/Workspace conditions ever
			// change shape again without a matching gate update.
			probe := &store.Agent{AppliedConfig: &store.AgentAppliedConfig{}}
			require.NoError(t, srv.populateAgentConfig(ctx, probe, project, nil))

			agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
				a.AppliedConfig.GitClone = probe.AppliedConfig.GitClone
				a.AppliedConfig.Workspace = probe.AppliedConfig.Workspace
				a.AppliedConfig.CreateInputs.Workspace = probe.AppliedConfig.Workspace
				if tc.clearWorkspace {
					a.AppliedConfig.Workspace = ""
					a.AppliedConfig.CreateInputs.Workspace = ""
				}
			})
			beforeVersion := agent.StateVersion
			self := agentIdentityFor(agent.ID, project.ID)

			for _, dryRun := range []bool{true, false} {
				wantCode := http.StatusOK
				switch {
				case tc.wantRejected:
					wantCode = http.StatusBadRequest
				case !dryRun:
					wantCode = http.StatusAccepted
				}
				req := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h", DryRun: dryRun})
				rec := httptest.NewRecorder()
				srv.handleReincarnateAgent(rec, req, agent.ID)
				assert.Equal(t, wantCode, rec.Code, "dryRun=%v: body: %s", dryRun, rec.Body.String())
				if tc.wantBodyText != "" {
					assert.Contains(t, rec.Body.String(), tc.wantBodyText, "dryRun=%v", dryRun)
				}
			}

			if !tc.wantRejected {
				return
			}

			after, err := s.GetAgent(ctx, agent.ID)
			require.NoError(t, err)
			assert.Equal(t, beforeVersion, after.StateVersion, "agent must be untouched")
			assert.Equal(t, "", after.ReincarnationState)

			list, err := s.ListAgentReincarnations(ctx, agent.ID)
			require.NoError(t, err)
			assert.Empty(t, list, "no reincarnation record should be created")
		})
	}
}

// TestReincarnateAgent_ExplicitMountWorkspace_Eligible is the design §3.4
// Amendment A23 regression test: a shared-workspace or hub-managed agent —
// no GitClone, but populateAgentConfig gives it a non-empty Workspace — is
// now eligible for reincarnate, where Phase 1 (Amendments A2/A4) rejected
// every non-clone-per-agent workspace outright. Both --dry-run and a real
// request must be accepted, and neither writes anything the caller did not
// ask for. Worktree-per-agent is unaffected by this change — see
// TestReincarnateAgent_WorktreePerAgentOrNeitherWorkspace_Returns400.
func TestReincarnateAgent_ExplicitMountWorkspace_Eligible(t *testing.T) {
	cases := []struct {
		name          string
		gitRemote     string
		workspaceMode string
		wantBranch    string // O1 (review p1b-r1): the plan's Branch for a shared agent is the project default, not scion/<slug>
	}{
		{
			name:          "shared-workspace git project",
			gitRemote:     "https://example.com/repo.git",
			workspaceMode: store.WorkspaceModeShared,
			wantBranch:    "develop",
		},
		{
			name:      "hub-managed project (no git remote)",
			gitRemote: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			disp := newReincarnateTestDispatcher()
			srv, s, project, broker := setupReincarnateTestServer(t, disp)
			ctx := context.Background()

			project.GitRemote = tc.gitRemote
			if tc.workspaceMode != "" {
				project.Labels = map[string]string{store.LabelWorkspaceMode: tc.workspaceMode}
			}
			if tc.wantBranch != "" {
				if project.Labels == nil {
					project.Labels = map[string]string{}
				}
				project.Labels["scion.dev/default-branch"] = tc.wantBranch
			}
			require.NoError(t, s.UpdateProject(ctx, project))

			probe := &store.Agent{AppliedConfig: &store.AgentAppliedConfig{}}
			require.NoError(t, srv.populateAgentConfig(ctx, probe, project, nil))
			require.Nil(t, probe.AppliedConfig.GitClone, "fixture check: an explicit-mount agent must have no GitClone")
			require.NotEmpty(t, probe.AppliedConfig.Workspace, "fixture check: populateAgentConfig must set an explicit Workspace")

			agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
				a.AppliedConfig.GitClone = nil
				a.AppliedConfig.Workspace = probe.AppliedConfig.Workspace
				a.AppliedConfig.CreateInputs.Workspace = probe.AppliedConfig.Workspace
				a.AppliedConfig.Branch = probe.AppliedConfig.Branch
			})
			self := agentIdentityFor(agent.ID, project.ID)

			dryReq := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h", DryRun: true})
			dryRec := httptest.NewRecorder()
			srv.handleReincarnateAgent(dryRec, dryReq, agent.ID)
			assert.Equal(t, http.StatusOK, dryRec.Code, "dry run: body: %s", dryRec.Body.String())
			if tc.wantBranch != "" {
				var dryResp ReincarnateAgentResponse
				require.NoError(t, json.Unmarshal(dryRec.Body.Bytes(), &dryResp))
				assert.Equal(t, tc.wantBranch, dryResp.Plan.Branch, "shared agent's plan branch must be the project default, not scion/<slug>")
			}

			realReq := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h", DryRun: false})
			realRec := httptest.NewRecorder()
			srv.handleReincarnateAgent(realRec, realReq, agent.ID)
			assert.Equal(t, http.StatusAccepted, realRec.Code, "real request: body: %s", realRec.Body.String())
		})
	}
}

// TestReincarnateAgent_SameBrokerEmptyPerAgent_Eligible pins
// miller79/scion#167: an empty-per-agent agent on a local-disk runtime,
// whose broker advertises ReprovisionEmptyPerAgent, is eligible for a
// same-broker reincarnation. The dry run reports it eligible (200, no move
// verdict) and a real request is accepted and reprovisions on the agent's
// own broker with no workspace source in the dispatched config (the broker
// reuses the private workspace in place).
func TestReincarnateAgent_SameBrokerEmptyPerAgent_Eligible(t *testing.T) {
	for _, rt := range []string{"docker", "podman", "container"} {
		t.Run(rt, func(t *testing.T) {
			disp := newReincarnateTestDispatcher()
			srv, s, project, broker := setupReincarnateTestServer(t, disp)
			ctx := context.Background()

			broker.Capabilities = &store.BrokerCapabilities{Reprovision: true, EmptyPerAgentWorkspace: true, ReprovisionEmptyPerAgent: true}
			require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))
			project.GitRemote = ""
			project.Labels = map[string]string{store.LabelWorkspaceMode: store.WorkspaceModePerAgent}
			require.NoError(t, s.UpdateProject(ctx, project))
			require.True(t, project.IsEmptyPerAgent(), "fixture check: project must be empty-per-agent")

			probe := &store.Agent{AppliedConfig: &store.AgentAppliedConfig{}}
			require.NoError(t, srv.populateAgentConfig(ctx, probe, project, nil))
			require.Nil(t, probe.AppliedConfig.GitClone, "fixture check: empty-per-agent has no GitClone")
			require.Empty(t, probe.AppliedConfig.Workspace, "fixture check: empty-per-agent has no Workspace")

			agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
				a.Runtime = rt
				a.AppliedConfig.GitClone = nil
				a.AppliedConfig.Workspace = ""
				if a.AppliedConfig.CreateInputs != nil {
					a.AppliedConfig.CreateInputs.Workspace = ""
				}
			})
			self := agentIdentityFor(agent.ID, project.ID)

			for _, req := range []ReincarnateAgentRequest{
				{Handoff: "h", DryRun: true},
				{Handoff: "h", DryRun: true, TargetBroker: broker.ID}, // --broker <current>
			} {
				dryRec := httptest.NewRecorder()
				srv.handleReincarnateAgent(dryRec, reincarnateRequest(t, agent.ID, self, req), agent.ID)
				require.Equal(t, http.StatusOK, dryRec.Code, "dry run (target %q): body: %s", req.TargetBroker, dryRec.Body.String())
				var dryResp ReincarnateAgentResponse
				require.NoError(t, json.Unmarshal(dryRec.Body.Bytes(), &dryResp))
				assert.Equal(t, agent.ID, dryResp.AgentID)
				assert.Equal(t, 2, dryResp.Generation)
				assert.Nil(t, dryResp.MoveVerdict, "a same-broker reincarnation is not a move")
			}
			n, _ := disp.reprovisionSnapshot()
			require.Equal(t, 0, n, "a dry run must not dispatch anything")

			realRec := httptest.NewRecorder()
			srv.handleReincarnateAgent(realRec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
			require.Equal(t, http.StatusAccepted, realRec.Code, "real request: body: %s", realRec.Body.String())

			r := waitForReincarnationSettled(t, s, agent.ID)
			require.Equal(t, store.AgentReincarnationStateCompleted, r.State, r.Error)
			n, cfgs := disp.reprovisionSnapshot()
			require.Equal(t, 1, n, "exactly one reprovision dispatch")
			assert.Nil(t, cfgs[0].GitClone, "empty-per-agent reprovision carries no git clone")
			assert.Empty(t, cfgs[0].Workspace, "empty-per-agent reprovision carries no workspace path")
			after, err := s.GetAgent(ctx, agent.ID)
			require.NoError(t, err)
			assert.Equal(t, broker.ID, after.RuntimeBrokerID, "the agent stays on its broker")
		})
	}
}

// TestReincarnateAgent_SameBrokerEmptyPerAgent_UnrecordedRuntime pins the
// runtime fallback of the miller79/scion#167 gate: an agent with no recorded
// Runtime is judged by resolveAgentRuntime over its broker's profiles. A
// single docker profile resolves to docker and is accepted; mixed profile
// types resolve to "" and are refused with 400 before anything is touched,
// on the dry run and the real request alike.
func TestReincarnateAgent_SameBrokerEmptyPerAgent_UnrecordedRuntime(t *testing.T) {
	cases := []struct {
		name     string
		profiles []store.BrokerProfile
		wantOK   bool
	}{
		{
			name:     "single docker profile: accepted",
			profiles: []store.BrokerProfile{{Name: "default", Type: "docker", Available: true}},
			wantOK:   true,
		},
		{
			name: "mixed profile types: refused",
			profiles: []store.BrokerProfile{
				{Name: "default", Type: "docker", Available: true},
				{Name: "k8s", Type: "kubernetes", Available: true},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			disp := newReincarnateTestDispatcher()
			srv, s, project, broker := setupReincarnateTestServer(t, disp)
			ctx := context.Background()

			broker.Capabilities = &store.BrokerCapabilities{Reprovision: true, EmptyPerAgentWorkspace: true, ReprovisionEmptyPerAgent: true}
			broker.Profiles = tc.profiles
			require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))
			project.GitRemote = ""
			project.Labels = map[string]string{store.LabelWorkspaceMode: store.WorkspaceModePerAgent}
			require.NoError(t, s.UpdateProject(ctx, project))

			agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
				a.Runtime = ""
				a.AppliedConfig.GitClone = nil
				a.AppliedConfig.Workspace = ""
				a.AppliedConfig.Profile = ""
				if a.AppliedConfig.CreateInputs != nil {
					a.AppliedConfig.CreateInputs.Workspace = ""
				}
			})
			require.Equal(t, "", agent.Runtime, "fixture check: no recorded runtime")
			beforeVersion := agent.StateVersion
			self := agentIdentityFor(agent.ID, project.ID)

			for _, dryRun := range []bool{true, false} {
				wantCode := http.StatusBadRequest
				if tc.wantOK {
					wantCode = http.StatusOK
					if !dryRun {
						wantCode = http.StatusAccepted
					}
				}
				rec := httptest.NewRecorder()
				srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h", DryRun: dryRun}), agent.ID)
				require.Equal(t, wantCode, rec.Code, "dryRun=%v: body: %s", dryRun, rec.Body.String())
				if !tc.wantOK {
					assert.Contains(t, rec.Body.String(), "supported only on local-disk runtimes", "dryRun=%v", dryRun)
				}
			}

			if tc.wantOK {
				r := waitForReincarnationSettled(t, s, agent.ID)
				require.Equal(t, store.AgentReincarnationStateCompleted, r.State, r.Error)
				return
			}
			after, err := s.GetAgent(ctx, agent.ID)
			require.NoError(t, err)
			assert.Equal(t, beforeVersion, after.StateVersion, "agent must be untouched on a 400")
			assert.Equal(t, "", after.ReincarnationState)
			list, err := s.ListAgentReincarnations(ctx, agent.ID)
			require.NoError(t, err)
			assert.Empty(t, list, "no reincarnation record on a 400")
			n, _ := disp.reprovisionSnapshot()
			assert.Equal(t, 0, n, "nothing dispatched on a 400")
		})
	}
}

// TestReincarnateAgent_SameBrokerEmptyPerAgent_Refused pins the refusals of
// the miller79/scion#167 gate. A runtime outside the local-disk allow-list
// gets 400; a broker without ReprovisionEmptyPerAgent gets 412 (an older
// broker would refuse the reprovision only after the worker had stopped the
// agent). Each holds on the dry run and the real request, with and without
// `--broker <current>`, and the agent is never stopped: no stop dispatch,
// no reincarnation record, no state change.
func TestReincarnateAgent_SameBrokerEmptyPerAgent_Refused(t *testing.T) {
	const runtimeMsg = "empty-per-agent reincarnation is supported only on local-disk runtimes (docker, podman, container)"
	const capMsg = "runtime broker does not support in-place empty-per-agent reincarnation; upgrade the broker"
	fullCaps := &store.BrokerCapabilities{Reprovision: true, EmptyPerAgentWorkspace: true, ReprovisionEmptyPerAgent: true}
	cases := []struct {
		name     string
		runtime  string
		caps     *store.BrokerCapabilities
		wantCode int
		wantMsg  string
	}{
		{name: "kubernetes runtime", runtime: "kubernetes", caps: fullCaps, wantCode: http.StatusBadRequest, wantMsg: runtimeMsg},
		{name: "cloudrun runtime", runtime: "cloudrun", caps: fullCaps, wantCode: http.StatusBadRequest, wantMsg: runtimeMsg},
		{name: "unknown runtime", runtime: "some-future-runtime", caps: fullCaps, wantCode: http.StatusBadRequest, wantMsg: runtimeMsg},
		{
			name:     "broker without ReprovisionEmptyPerAgent",
			runtime:  "docker",
			caps:     &store.BrokerCapabilities{Reprovision: true, EmptyPerAgentWorkspace: true, AgentMove: true},
			wantCode: http.StatusPreconditionFailed,
			wantMsg:  capMsg,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			disp := newReincarnateTestDispatcher()
			srv, s, project, broker := setupReincarnateTestServer(t, disp)
			ctx := context.Background()

			broker.Capabilities = tc.caps
			require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))
			project.GitRemote = ""
			project.Labels = map[string]string{store.LabelWorkspaceMode: store.WorkspaceModePerAgent}
			require.NoError(t, s.UpdateProject(ctx, project))

			agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
				a.Runtime = tc.runtime
				a.AppliedConfig.GitClone = nil
				a.AppliedConfig.Workspace = ""
				if a.AppliedConfig.CreateInputs != nil {
					a.AppliedConfig.CreateInputs.Workspace = ""
				}
			})
			beforeVersion := agent.StateVersion
			self := agentIdentityFor(agent.ID, project.ID)

			for _, target := range []string{"", broker.ID} {
				for _, dryRun := range []bool{true, false} {
					rec := httptest.NewRecorder()
					req := ReincarnateAgentRequest{Handoff: "h", DryRun: dryRun, TargetBroker: target}
					srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, req), agent.ID)
					assert.Equal(t, tc.wantCode, rec.Code, "target=%q dryRun=%v: body: %s", target, dryRun, rec.Body.String())
					assert.Contains(t, rec.Body.String(), tc.wantMsg, "target=%q dryRun=%v", target, dryRun)
					if tc.wantCode == http.StatusPreconditionFailed {
						assert.Contains(t, rec.Body.String(), ErrCodeUnsupportedCapability, "target=%q dryRun=%v", target, dryRun)
					}
				}
			}

			disp.mu.Lock()
			stops := disp.stopCalls
			disp.mu.Unlock()
			assert.Zero(t, stops, "the agent must never be stopped for a request the gate refuses up front")
			n, _ := disp.reprovisionSnapshot()
			assert.Zero(t, n, "nothing may be dispatched")
			after, err := s.GetAgent(ctx, agent.ID)
			require.NoError(t, err)
			assert.Equal(t, beforeVersion, after.StateVersion, "agent must be untouched")
			assert.Equal(t, 1, after.Generation)
			assert.Equal(t, "", after.ReincarnationState)
			list, err := s.ListAgentReincarnations(ctx, agent.ID)
			require.NoError(t, err)
			assert.Empty(t, list, "no reincarnation record may be created")
		})
	}
}

// TestReincarnateAgent_ModeSwitchedToShared_Returns400 is the design §3.4
// Amendment A23.1 (review p1b-r1) R3 regression test: A23 contract (a) keeps
// "not shared" as a condition for the clone-per-agent case, unchanged from
// Phase 1. A project can be switched from clone-per-agent to shared-workspace
// after an agent already exists (a project label update — see
// handlers_projects_core.go), so an agent whose AppliedConfig.GitClone still
// reflects the old mode must still be rejected: reincarnating it would
// silently abandon its own clone (still on disk, possibly holding unpushed
// work) in favor of the newly-mounted shared checkout.
func TestReincarnateAgent_ModeSwitchedToShared_Returns400(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	ctx := context.Background()

	project.GitRemote = "https://example.com/repo.git"
	require.NoError(t, s.UpdateProject(ctx, project))

	// Build the agent's config the way create actually would, while the
	// project was still clone-per-agent.
	probe := &store.Agent{AppliedConfig: &store.AgentAppliedConfig{}}
	require.NoError(t, srv.populateAgentConfig(ctx, probe, project, nil))
	require.NotNil(t, probe.AppliedConfig.GitClone, "fixture check: a clone-per-agent project must produce a GitClone")

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.AppliedConfig.GitClone = probe.AppliedConfig.GitClone
		a.AppliedConfig.Workspace = ""
		a.AppliedConfig.CreateInputs.Workspace = ""
	})
	beforeVersion := agent.StateVersion
	self := agentIdentityFor(agent.ID, project.ID)

	// The mode switch, after the agent already exists.
	project.Labels = map[string]string{store.LabelWorkspaceMode: store.WorkspaceModeShared}
	require.NoError(t, s.UpdateProject(ctx, project))

	for _, dryRun := range []bool{true, false} {
		req := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h", DryRun: dryRun})
		rec := httptest.NewRecorder()
		srv.handleReincarnateAgent(rec, req, agent.ID)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "dryRun=%v: body: %s", dryRun, rec.Body.String())
	}

	after, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, beforeVersion, after.StateVersion, "agent must be untouched")
	assert.Equal(t, "", after.ReincarnationState)

	list, err := s.ListAgentReincarnations(ctx, agent.ID)
	require.NoError(t, err)
	assert.Empty(t, list, "no reincarnation record should be created")
}

// TestReincarnateAgent_ModeSwitchedToCloneOnly_Returns400 is the design §3.4
// Amendment A23.2 (review p1b-r2) FYI-b regression test, the mirror of R3
// for the reverse switch: a project created as shared-workspace, later
// switched to clone-per-agent (the workspace-mode label removed), leaves an
// existing agent's stored config with GitClone == nil and a non-empty
// Workspace pointing at the old shared checkout. A fresh reincarnation would
// derive a GitClone from the project's CURRENT (now clone-per-agent) mode,
// but this agent has no existing real clone on disk for the broker's
// GitClone branch to find, so — without this check — it would be stopped
// and then refused by the broker's "no existing git clone" 409, the same
// stop-then-refuse shape R1 fixed for the other direction.
//
// The hub-managed case (no git remote) is included as the negative control
// the brief asks for explicitly: switchedToCloneOnly requires GitRemote !=
// "", so a hub-managed project's agents must stay eligible.
func TestReincarnateAgent_ModeSwitchedToCloneOnly_Returns400(t *testing.T) {
	cases := []struct {
		name         string
		gitRemote    string
		startShared  bool // project starts shared (git-remote case) before the switch
		switchOff    bool // remove the shared label, simulating the mode switch
		wantRejected bool
	}{
		{
			name:         "shared switched to clone-per-agent: rejected",
			gitRemote:    "https://example.com/repo.git",
			startShared:  true,
			switchOff:    true,
			wantRejected: true,
		},
		{
			name:         "hub-managed (no git remote): unaffected",
			gitRemote:    "",
			startShared:  false,
			switchOff:    false,
			wantRejected: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			disp := newReincarnateTestDispatcher()
			srv, s, project, broker := setupReincarnateTestServer(t, disp)
			ctx := context.Background()

			project.GitRemote = tc.gitRemote
			if tc.startShared {
				project.Labels = map[string]string{store.LabelWorkspaceMode: store.WorkspaceModeShared}
			}
			require.NoError(t, s.UpdateProject(ctx, project))

			// Build the agent's config the way create actually would, under
			// the ORIGINAL mode.
			probe := &store.Agent{AppliedConfig: &store.AgentAppliedConfig{}}
			require.NoError(t, srv.populateAgentConfig(ctx, probe, project, nil))
			require.Nil(t, probe.AppliedConfig.GitClone, "fixture check: this project must produce no GitClone before any switch")
			require.NotEmpty(t, probe.AppliedConfig.Workspace, "fixture check: populateAgentConfig must set an explicit Workspace")

			agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
				a.AppliedConfig.GitClone = nil
				a.AppliedConfig.Workspace = probe.AppliedConfig.Workspace
				a.AppliedConfig.CreateInputs.Workspace = probe.AppliedConfig.Workspace
			})
			beforeVersion := agent.StateVersion
			self := agentIdentityFor(agent.ID, project.ID)

			if tc.switchOff {
				// The mode switch, after the agent already exists: shared -> clone-per-agent.
				project.Labels = nil
				require.NoError(t, s.UpdateProject(ctx, project))
			}

			for _, dryRun := range []bool{true, false} {
				wantCode := http.StatusOK
				switch {
				case tc.wantRejected:
					wantCode = http.StatusBadRequest
				case !dryRun:
					wantCode = http.StatusAccepted
				}
				req := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h", DryRun: dryRun})
				rec := httptest.NewRecorder()
				srv.handleReincarnateAgent(rec, req, agent.ID)
				assert.Equal(t, wantCode, rec.Code, "dryRun=%v: body: %s", dryRun, rec.Body.String())
			}

			if !tc.wantRejected {
				return
			}

			after, err := s.GetAgent(ctx, agent.ID)
			require.NoError(t, err)
			assert.Equal(t, beforeVersion, after.StateVersion, "agent must be untouched")
			assert.Equal(t, "", after.ReincarnationState)

			list, err := s.ListAgentReincarnations(ctx, agent.ID)
			require.NoError(t, err)
			assert.Empty(t, list, "no reincarnation record should be created")
		})
	}
}

// TestReincarnateAgent_LinkedSharedProject_ClearedWorkspace_Returns400 is the
// design §3.4 Amendment A23.1 (review p1b-r1) R1 regression test: a
// shared-workspace project linked to the agent's broker via a
// store.ProjectProvider{LocalPath} has its AppliedConfig.Workspace cleared by
// buildCreateRequest at dispatch time (the broker derives its own workspace
// location from the linked path instead) — see effectiveDispatchWorkspace.
// Before the fix, the Hub's gate evaluated the raw (non-empty) Workspace,
// judged the agent eligible, stopped it, and only then discovered — via the
// broker's 409 refusal — that the dispatched request had Workspace="" and
// GitClone=nil. The gate must now refuse up front, with nothing stopped and
// no dispatch call made at all.
func TestReincarnateAgent_LinkedSharedProject_ClearedWorkspace_Returns400(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	ctx := context.Background()

	project.GitRemote = "https://example.com/repo.git"
	project.Labels = map[string]string{store.LabelWorkspaceMode: store.WorkspaceModeShared}
	require.NoError(t, s.UpdateProject(ctx, project))

	// Link the agent's broker to a local path for this project — the
	// condition effectiveDispatchWorkspace/buildCreateRequest key off.
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		LocalPath:  "/home/broker/projects/shared-ws",
		Status:     store.BrokerStatusOnline,
	}))

	probe := &store.Agent{AppliedConfig: &store.AgentAppliedConfig{}}
	require.NoError(t, srv.populateAgentConfig(ctx, probe, project, nil))
	require.Nil(t, probe.AppliedConfig.GitClone, "fixture check: a shared-workspace project must produce no GitClone")
	require.True(t, filepath.IsAbs(probe.AppliedConfig.Workspace), "fixture check: populateAgentConfig's Workspace for a shared project must be absolute")

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.AppliedConfig.GitClone = nil
		a.AppliedConfig.Workspace = probe.AppliedConfig.Workspace
		a.AppliedConfig.CreateInputs.Workspace = probe.AppliedConfig.Workspace
	})
	beforeVersion := agent.StateVersion
	self := agentIdentityFor(agent.ID, project.ID)

	for _, dryRun := range []bool{true, false} {
		req := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h", DryRun: dryRun})
		rec := httptest.NewRecorder()
		srv.handleReincarnateAgent(rec, req, agent.ID)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "dryRun=%v: body: %s", dryRun, rec.Body.String())
	}

	after, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, beforeVersion, after.StateVersion, "agent must be untouched")
	assert.Equal(t, "", after.ReincarnationState)

	list, err := s.ListAgentReincarnations(ctx, agent.ID)
	require.NoError(t, err)
	assert.Empty(t, list, "no reincarnation record should be created")

	assert.Zero(t, disp.stopCalls, "the agent must never be stopped for a request the gate refuses up front")
	assert.Zero(t, disp.reprovisionCalls, "the broker must never be dispatched for a request the gate refuses up front")
}

// =============================================================================
// Dry run (AC-2, AC-2a) — no store writes
// =============================================================================

func TestReincarnateAgent_DryRun_ShowsDiffAndWritesNothing(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)

	template := &store.Template{
		ID:          tid("tmpl-" + t.Name()),
		Name:        "t",
		Slug:        "reincarnate-template-" + tidSlugSafe(t.Name()),
		Harness:     "claude",
		Scope:       store.TemplateScopeGlobal,
		Status:      store.TemplateStatusActive,
		ContentHash: "new-template-hash",
		Config:      &store.TemplateConfig{Image: "template-image:v2"},
	}
	require.NoError(t, s.CreateTemplate(context.Background(), template))

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Template = template.Slug
		a.AppliedConfig.TemplateHash = "old-template-hash"
		// No explicit image in CreateInputs -> template image applies fresh.
	})
	beforeVersion := agent.StateVersion
	self := agentIdentityFor(agent.ID, project.ID)

	req := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{DryRun: true})
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, req, agent.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp ReincarnateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "planned", resp.State)
	assert.Equal(t, 2, resp.Generation)
	assert.Equal(t, "old-template-hash", resp.Plan.Template.Old)
	assert.Equal(t, "new-template-hash", resp.Plan.Template.New)
	assert.Equal(t, "old-image:v1", resp.Plan.Image.Old)
	assert.Equal(t, "template-image:v2", resp.Plan.Image.New)

	// Nothing changed: state_version, generation, reincarnation_state, and
	// the reincarnation history are all exactly as before.
	after, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, beforeVersion, after.StateVersion, "dry run must not write to the store")
	assert.Equal(t, 1, after.Generation)
	assert.Equal(t, "", after.ReincarnationState)

	list, err := s.ListAgentReincarnations(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Empty(t, list)

	assert.Zero(t, disp.stopCalls)
	assert.Zero(t, disp.reprovisionCalls)
	assert.Zero(t, disp.startCalls)
}

// AC-2a: an agent created with an EXPLICIT image keeps it across a template
// image bump, while an agent that took the template's image at create picks
// up the new template image fresh — proving CreateInputs (not the live,
// possibly-stale AppliedConfig.Image) drives the replay.
func TestReincarnateAgent_AC2a_ExplicitImageSurvivesTemplateBump(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)

	template := &store.Template{
		ID:          tid("tmpl-explicit-" + t.Name()),
		Name:        "t",
		Slug:        "reincarnate-template-explicit-" + tidSlugSafe(t.Name()),
		Harness:     "claude",
		Scope:       store.TemplateScopeGlobal,
		Status:      store.TemplateStatusActive,
		ContentHash: "new-template-hash",
		Config:      &store.TemplateConfig{Image: "template-image:v2"},
	}
	require.NoError(t, s.CreateTemplate(context.Background(), template))

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Template = template.Slug
		a.AppliedConfig.Image = "explicit-image:v1" // what create left behind
		a.AppliedConfig.CreateInputs.InlineConfig = &api.ScionConfig{Image: "explicit-image:v1"}
	})
	self := agentIdentityFor(agent.ID, project.ID)

	req := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{DryRun: true})
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, req, agent.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp ReincarnateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "explicit-image:v1", resp.Plan.Image.Old)
	assert.Equal(t, "explicit-image:v1", resp.Plan.Image.New, "an explicit image must survive a template image bump")
}

// TestReincarnateAgent_PlanFillsImageFromHarnessConfig is the plan-side
// half of design §3.4 Amendment A11 item 1: an agent with no explicit
// image and no template image must have its plan show the harness config's
// image, not "" -> "" (which would look like nothing changed when the agent
// is actually about to pick up a real image for the first time). This also
// covers the "previously reincarnated agent with an empty stored image"
// case: CreateInputs is already non-nil here (as it would be for an agent
// that has already gone through the CreateInputs-capturing path once before)
// with AppliedConfig.Image left empty, and the plan must still show
// "" -> a real image, not "" -> "" read as no-op.
func TestReincarnateAgent_PlanFillsImageFromHarnessConfig(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)

	hc := &store.HarnessConfig{
		ID:          tid("hc-plan-fill-" + t.Name()),
		Name:        "hc",
		Slug:        "plan-fill-hc-" + tidSlugSafe(t.Name()),
		Harness:     "claude",
		Scope:       store.HarnessConfigScopeGlobal,
		Status:      store.HarnessConfigStatusActive,
		ContentHash: "hc-hash-v1",
		Config:      &store.HarnessConfigData{Image: "harness-config-image:v1"},
	}
	require.NoError(t, s.CreateHarnessConfig(context.Background(), hc))

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.AppliedConfig.Image = "" // never had an image resolved onto it
		a.AppliedConfig.HarnessConfig = hc.Slug
		// HarnessConfig is explicit-only (design §3.3 Amendment A1), driven by
		// CreateInputs.HarnessConfig, not kept from the live AppliedConfig —
		// this is what the requester actually asked for at create time.
		a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{HarnessConfig: hc.Slug}
	})
	self := agentIdentityFor(agent.ID, project.ID)

	req := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{DryRun: true})
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, req, agent.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp ReincarnateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "", resp.Plan.Image.Old)
	assert.Equal(t, "harness-config-image:v1", resp.Plan.Image.New,
		"the plan must fall back to the harness config's image when neither an explicit image nor a template supplied one")
}

// TestReincarnateAgent_TemplateImageBeatsHarnessConfig proves the
// harness-config image fallback (A11.1a) never outranks a template image:
// the broker's own precedence is explicit inline, then template, then
// harness config (pkg/agent/provision.go), so a template image must win here
// even though a harness config with a different image is also in play.
func TestReincarnateAgent_TemplateImageBeatsHarnessConfig(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)

	template := &store.Template{
		ID:          tid("tmpl-precedence-" + t.Name()),
		Name:        "t",
		Slug:        "precedence-template-" + tidSlugSafe(t.Name()),
		Harness:     "claude",
		Scope:       store.TemplateScopeGlobal,
		Status:      store.TemplateStatusActive,
		ContentHash: "template-hash",
		Config:      &store.TemplateConfig{Image: "template-image:v1"},
	}
	require.NoError(t, s.CreateTemplate(context.Background(), template))

	hc := &store.HarnessConfig{
		ID:          tid("hc-precedence-" + t.Name()),
		Name:        "hc",
		Slug:        "precedence-hc-" + tidSlugSafe(t.Name()),
		Harness:     "claude",
		Scope:       store.HarnessConfigScopeGlobal,
		Status:      store.HarnessConfigStatusActive,
		ContentHash: "hc-hash-v1",
		Config:      &store.HarnessConfigData{Image: "harness-config-image:v1"},
	}
	require.NoError(t, s.CreateHarnessConfig(context.Background(), hc))

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Template = template.Slug
		a.AppliedConfig.Image = ""
		a.AppliedConfig.HarnessConfig = hc.Slug
		// HarnessConfig is explicit-only (design §3.3 Amendment A1), driven by
		// CreateInputs.HarnessConfig — record what the requester actually asked
		// for, so it outranks the template for HarnessConfigID resolution while
		// still losing to the template for Image.
		a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{HarnessConfig: hc.Slug}
	})
	self := agentIdentityFor(agent.ID, project.ID)

	req := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{DryRun: true})
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, req, agent.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp ReincarnateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "template-image:v1", resp.Plan.Image.New,
		"a template image must beat the harness-config fallback")
}

// TestReincarnateAgent_PlanUsesSettingsImageOverHarnessConfig pins
// ptone/scion#2156: the reincarnate plan must mirror the broker's own
// precedence (explicit inline, then template, then
// Hub settings harness_configs.<name>, then the harness config's own stored
// image) — not stop at the harness config's stored image the way A11.1(a)
// did before settings could win. A settings image with no template image
// must make plan.Image.New equal the settings image, not the harness
// config's own image.
//
// The overlay is keyed by the harness config's SLUG, and Name is
// deliberately different from Slug: the broker (and so the reincarnate
// lookup, to match it) resolves settings by the dispatched harness-config
// name/slug, never by the harness config's own display Name. Keying by
// Name here would pass even with the wrong lookup key, hiding the bug.
func TestReincarnateAgent_PlanUsesSettingsImageOverHarnessConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate LoadEffectiveSettings from any ambient config

	hcSlug := "settings-image-hc-" + tidSlugSafe(t.Name())

	overlay := config.NewSettingsOverlay()
	overlay.Update(nil, nil, map[string]config.HarnessConfigEntry{
		hcSlug: {Harness: "claude", Image: "settings-image:v1"},
	}, "")
	config.SetGlobalSettingsOverlay(overlay)
	t.Cleanup(func() { config.SetGlobalSettingsOverlay(nil) })

	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)

	hc := &store.HarnessConfig{
		ID:          tid("hc-settings-image-" + t.Name()),
		Name:        "HC Pinned Display Name",
		Slug:        hcSlug,
		Harness:     "claude",
		Scope:       store.HarnessConfigScopeGlobal,
		Status:      store.HarnessConfigStatusActive,
		ContentHash: "hc-hash-v1",
		Config:      &store.HarnessConfigData{Image: "harness-config-image:v1"},
	}
	require.NoError(t, s.CreateHarnessConfig(context.Background(), hc))

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.AppliedConfig.Image = ""
		a.AppliedConfig.HarnessConfig = hc.Slug
		a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{HarnessConfig: hc.Slug}
	})
	self := agentIdentityFor(agent.ID, project.ID)

	req := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{DryRun: true})
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, req, agent.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp ReincarnateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "settings-image:v1", resp.Plan.Image.New,
		"a Hub settings harness_configs.<name>.image must outrank the harness config's own stored image")
}

// TestReincarnateAgent_WorkerPersistsBrokerEchoedImage is the worker-side
// half of design §3.4 Amendment A11 item 1: when the broker echoes back a
// resolved image different from what the hub planned (e.g. the broker's own
// search path resolved something the hub could not predict), the worker must
// persist that image on the agent's live AppliedConfig. Since
// ptone/scion#1907 the persisted image is the START echo (the runtime-
// resolved image); the reprovision echo's image is the rendered,
// not-yet-qualified value and is not taken.
func TestReincarnateAgent_WorkerPersistsBrokerEchoedImage(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	disp.reprovisionImage = "broker-reprovision-echo:v9"
	disp.startImage = "broker-resolved-image:v9"
	srv, s, project, broker := setupReincarnateTestServer(t, disp)

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.AppliedConfig.Image = "old-image:v1"
	})
	self := agentIdentityFor(agent.ID, project.ID)

	req := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{})
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, req, agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	waitForReincarnationSettled(t, s, agent.ID)

	final, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	require.NotNil(t, final.AppliedConfig)
	assert.Equal(t, "broker-resolved-image:v9", final.AppliedConfig.Image,
		"the start-echoed image must be persisted at completion")
}

// createImageHarnessConfig stores a global harness config whose only
// configured field is its container image.
func createImageHarnessConfig(t *testing.T, s store.Store, image string) *store.HarnessConfig {
	t.Helper()
	hc := &store.HarnessConfig{
		ID:          tid("hc-image-" + t.Name()),
		Name:        "hc",
		Slug:        "image-hc-" + tidSlugSafe(t.Name()),
		Harness:     "claude",
		Scope:       store.HarnessConfigScopeGlobal,
		Status:      store.HarnessConfigStatusActive,
		ContentHash: "hc-hash-" + image,
		Config:      &store.HarnessConfigData{Image: image},
	}
	require.NoError(t, s.CreateHarnessConfig(context.Background(), hc))
	return hc
}

// TestReincarnateAgent_PlanShowsHarnessConfigImageChange covers an agent whose
// image came from its harness config: when only the harness config's image
// changes, the plan must show the change as old -> new.
func TestReincarnateAgent_PlanShowsHarnessConfigImageChange(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	hc := createImageHarnessConfig(t, s, "harness-config-image:v2")

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.AppliedConfig.Image = "harness-config-image:v1" // resolved from the harness config at create
		a.AppliedConfig.HarnessConfig = hc.Slug
		a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{HarnessConfig: hc.Slug}
	})
	self := agentIdentityFor(agent.ID, project.ID)

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{DryRun: true}), agent.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp ReincarnateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "harness-config-image:v1", resp.Plan.Image.Old)
	assert.Equal(t, "harness-config-image:v2", resp.Plan.Image.New,
		"a harness-config image change must show up in the plan")
}

// TestReincarnateAgent_InlineImageBeatsHarnessConfig proves an explicit
// inline image outranks the harness config's image, so a harness-config image
// change leaves an explicitly chosen image unchanged.
func TestReincarnateAgent_InlineImageBeatsHarnessConfig(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	hc := createImageHarnessConfig(t, s, "harness-config-image:v2")

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.AppliedConfig.Image = "explicit-image:v1"
		a.AppliedConfig.HarnessConfig = hc.Slug
		a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{
			HarnessConfig: hc.Slug,
			InlineConfig:  &api.ScionConfig{Image: "explicit-image:v1"},
		}
	})
	self := agentIdentityFor(agent.ID, project.ID)

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{DryRun: true}), agent.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp ReincarnateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "explicit-image:v1", resp.Plan.Image.Old)
	assert.Equal(t, "explicit-image:v1", resp.Plan.Image.New,
		"an explicit inline image must survive a harness-config image change")
}

// TestReincarnateAgent_RecordCarriesBrokerEchoedImage proves the reincarnation
// record's NewAppliedConfig carries the image the broker echoed back from
// start (ptone/scion#1907: not the reprovision echo's unqualified image), the
// same image the agent row ends up with.
func TestReincarnateAgent_RecordCarriesBrokerEchoedImage(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	disp.reprovisionImage = "broker-reprovision-echo:v9"
	disp.startImage = "broker-resolved-image:v9"
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	r := waitForReincarnationSettled(t, s, agent.ID)
	require.Equal(t, store.AgentReincarnationStateCompleted, r.State, r.Error)
	require.NotNil(t, r.NewAppliedConfig)
	assert.Equal(t, "broker-resolved-image:v9", r.NewAppliedConfig.Image,
		"the record must carry the broker-echoed image")
}

// TestReincarnateAgent_NoBrokerEchoKeepsHubResolvedImage proves that when the
// broker echoes no image, the image the hub resolved from the harness config
// is what the agent row ends up with.
func TestReincarnateAgent_NoBrokerEchoKeepsHubResolvedImage(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	hc := createImageHarnessConfig(t, s, "harness-config-image:v2")

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.AppliedConfig.Image = ""
		a.AppliedConfig.HarnessConfig = hc.Slug
		a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{HarnessConfig: hc.Slug}
	})
	self := agentIdentityFor(agent.ID, project.ID)

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	r := waitForReincarnationSettled(t, s, agent.ID)
	require.Equal(t, store.AgentReincarnationStateCompleted, r.State, r.Error)

	final, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	require.NotNil(t, final.AppliedConfig)
	assert.Equal(t, "harness-config-image:v2", final.AppliedConfig.Image)
}

// httpReprovisionDispatcher is the reincarnate test dispatcher with
// reprovision sent through the real HTTPAgentDispatcher (to a mock broker
// client), so the worker runs against the real dispatchProvision.
type httpReprovisionDispatcher struct {
	*reincarnateTestDispatcher
	http *HTTPAgentDispatcher
}

func (d *httpReprovisionDispatcher) DispatchAgentReprovision(ctx context.Context, agent *store.Agent) error {
	return d.http.DispatchAgentReprovision(ctx, agent)
}

// TestReincarnateAgent_RowEnvMatchesFreshConfigAfterReprovision proves the
// env resolved at reprovision time (storage env vars and environment-type
// values) is sent to the broker but does not end up in the agent row: after a
// reincarnation the row's Env equals the fresh derivation's, and the next
// plan shows no env change.
func TestReincarnateAgent_RowEnvMatchesFreshConfigAfterReprovision(t *testing.T) {
	ctx := context.Background()
	fake := newReincarnateTestDispatcher()
	disp := &httpReprovisionDispatcher{reincarnateTestDispatcher: fake}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)

	broker.Endpoint = "http://localhost:9800"
	require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))
	require.NoError(t, s.CreateEnvVar(ctx, &store.EnvVar{
		ID:            tid("ev-stored-" + t.Name()),
		Key:           "STORED_VAR",
		Value:         "stored-value",
		Scope:         store.ScopeProject,
		ScopeID:       project.ID,
		InjectionMode: store.InjectionModeAlways,
	}))
	mockClient := &mockRuntimeBrokerClient{}
	disp.http = NewHTTPAgentDispatcherWithClient(s, mockClient, false, slog.Default())
	disp.http.SetSecretBackend(&mockSecretBackend{
		secrets: []secret.SecretWithValue{
			{SecretMeta: secret.SecretMeta{Name: "RESOLVED_VAR", SecretType: "environment", Target: "RESOLVED_VAR"}, Value: "resolved-value"},
		},
	})

	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	r := waitForReincarnationSettled(t, s, agent.ID)
	require.Equal(t, store.AgentReincarnationStateCompleted, r.State, r.Error)

	require.NotNil(t, mockClient.lastCreateReq)
	require.True(t, mockClient.lastCreateReq.Reprovision)
	assert.Equal(t, "stored-value", mockClient.lastCreateReq.ResolvedEnv["STORED_VAR"], "the broker must still receive the storage env var")
	assert.Equal(t, "resolved-value", mockClient.lastCreateReq.ResolvedEnv["RESOLVED_VAR"], "the broker must still receive the environment-type value")

	final, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	require.NotNil(t, final.AppliedConfig)
	fresh, _, err := srv.buildFreshAppliedConfig(ctx, final, project, "")
	require.NoError(t, err)
	assert.Equal(t, fresh.Env, final.AppliedConfig.Env, "the row's env must equal the fresh derivation's env")
	assert.NotContains(t, final.AppliedConfig.Env, "STORED_VAR")
	assert.NotContains(t, final.AppliedConfig.Env, "RESOLVED_VAR")
	assert.Equal(t, KeyDiff{}, computeReincarnationPlan(final.AppliedConfig, fresh, nil, "").EnvKeys, "the next plan must show no env change")
}

// imageRegistryPlan runs a dry-run reincarnate against an agent whose image
// comes from a harness config holding hcImage, with the dispatcher rewriting
// images to registry, and returns the plan's image change.
func imageRegistryPlan(t *testing.T, registry, hcImage, rowImage string) FieldChange {
	t.Helper()
	disp := newReincarnateTestDispatcher()
	disp.imageRegistry = registry
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	hc := createImageHarnessConfig(t, s, hcImage)

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.AppliedConfig.Image = rowImage
		a.AppliedConfig.HarnessConfig = hc.Slug
		a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{HarnessConfig: hc.Slug}
	})
	self := agentIdentityFor(agent.ID, project.ID)

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{DryRun: true}), agent.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ReincarnateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp.Plan.Image
}

// TestReincarnateAgent_PlanUnchangedForQualifiedRowAndBareHarnessConfigImage
// covers a create-born agent: the harness config holds a bare image, the
// dispatcher rewrote it to the registry at create, and the broker echoed the
// qualified name onto the row. With nothing changed, the plan must say so.
func TestReincarnateAgent_PlanUnchangedForQualifiedRowAndBareHarnessConfigImage(t *testing.T) {
	img := imageRegistryPlan(t, "ghcr.io/test-org", "scion-claude:v1", "ghcr.io/test-org/scion-claude:v1")
	assert.Equal(t, "ghcr.io/test-org/scion-claude:v1", img.New, "the fresh image must be in the registry-qualified form the broker runs")
	assert.Equal(t, img.New, img.Old, "an unchanged image must not show as a diff")
}

// TestReincarnateAgent_PlanShowsQualifiedHarnessConfigImageBump covers the
// same agent after its harness config's image was bumped: both sides of the
// diff are registry-qualified.
func TestReincarnateAgent_PlanShowsQualifiedHarnessConfigImageBump(t *testing.T) {
	img := imageRegistryPlan(t, "ghcr.io/test-org", "scion-claude:v2", "ghcr.io/test-org/scion-claude:v1")
	assert.Equal(t, "ghcr.io/test-org/scion-claude:v1", img.Old)
	assert.Equal(t, "ghcr.io/test-org/scion-claude:v2", img.New)
}

// TestReincarnateAgent_PlanUnchangedForLegacyBareRowImage covers a row that
// still holds the bare image (written before the fresh config was stored in
// canonical form): with the harness config unchanged, the plan must not show
// a bare -> qualified diff.
func TestReincarnateAgent_PlanUnchangedForLegacyBareRowImage(t *testing.T) {
	img := imageRegistryPlan(t, "ghcr.io/test-org", "scion-claude:v1", "scion-claude:v1")
	assert.Equal(t, "ghcr.io/test-org/scion-claude:v1", img.New)
	assert.Equal(t, img.New, img.Old, "a bare row image equal to the fresh image in canonical form must not show as a diff")
}

// TestReincarnateAgent_PlanShowsChangeFromEmptyRowImage proves canonical
// comparison never hides a change from an empty stored image.
func TestReincarnateAgent_PlanShowsChangeFromEmptyRowImage(t *testing.T) {
	img := imageRegistryPlan(t, "ghcr.io/test-org", "scion-claude:v1", "")
	assert.Equal(t, "", img.Old)
	assert.Equal(t, "ghcr.io/test-org/scion-claude:v1", img.New)
}

// TestReincarnateAgent_EmptyDispatcherRegistryLeavesImageBare proves that when
// the dispatcher does not rewrite images, neither does the fresh config, even
// if a registry is configured elsewhere: the registry must come from the
// dispatcher, the component that actually rewrites images on the way to the
// broker.
func TestReincarnateAgent_EmptyDispatcherRegistryLeavesImageBare(t *testing.T) {
	t.Setenv("SCION_IMAGE_REGISTRY", "ghcr.io/elsewhere")
	img := imageRegistryPlan(t, "", "scion-claude:v1", "")
	assert.Equal(t, "scion-claude:v1", img.New, "with no dispatcher registry the image must be left as is")
}

// TestReincarnateAgent_ExplicitImageCanonicalised proves the canonical form
// applies to an explicit inline image too, since the dispatcher rewrites
// every image it sends.
func TestReincarnateAgent_ExplicitImageCanonicalised(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	disp.imageRegistry = "ghcr.io/test-org"
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.AppliedConfig.Image = "ghcr.io/test-org/explicit-image:v1"
		a.AppliedConfig.CreateInputs.InlineConfig = &api.ScionConfig{Image: "explicit-image:v1"}
	})
	self := agentIdentityFor(agent.ID, project.ID)

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{DryRun: true}), agent.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ReincarnateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "ghcr.io/test-org/explicit-image:v1", resp.Plan.Image.New)
	assert.Equal(t, resp.Plan.Image.New, resp.Plan.Image.Old)
}

// TestReincarnateAgent_RowImageMatchesRecordAfterStartEcho proves the agent
// row ends a reincarnation with exactly the image in the record, including an
// image the start response echoed back after the starting step's write.
func TestReincarnateAgent_RowImageMatchesRecordAfterStartEcho(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	disp.reprovisionImage = "ghcr.io/test-org/reprovision-echo:v1"
	disp.startImage = "ghcr.io/test-org/start-echo:v2"
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	r := waitForReincarnationSettled(t, s, agent.ID)
	require.Equal(t, store.AgentReincarnationStateCompleted, r.State, r.Error)
	require.NotNil(t, r.NewAppliedConfig)
	final, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	require.NotNil(t, final.AppliedConfig)
	assert.Equal(t, "ghcr.io/test-org/start-echo:v2", r.NewAppliedConfig.Image)
	assert.Equal(t, r.NewAppliedConfig.Image, final.AppliedConfig.Image, "the row must end with the image the record holds")
}

// TestUpdateReincarnationStep_StoresCopyOfAppliedConfig covers
// ptone/scion#1907: the step write stores a copy of the caller's applied
// config, so neither side can change the other through a shared pointer:
// not the caller mutating its struct after the write, and not the
// dispatcher writing the broker echo onto the returned agent.
func TestUpdateReincarnationStep_StoresCopyOfAppliedConfig(t *testing.T) {
	ctx := context.Background()
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)

	cfg := &store.AgentAppliedConfig{Image: "ghcr.io/test-org/fresh:v2", HarnessAuth: "api-key", Profile: "fresh-profile"}
	written, err := srv.updateReincarnationStep(ctx, agent.ID, reincarnationStepUpdate{
		reincarnationState: store.ReincarnationStateProvisioning,
		appliedConfig:      cfg,
	}, reincarnationStepMaxAttempts)
	require.NoError(t, err)
	require.NotNil(t, written.AppliedConfig)
	assert.NotSame(t, cfg, written.AppliedConfig, "the step must not store the caller's pointer")

	cfg.Image = "mutated-by-caller:v1"
	cfg.Profile = "mutated-by-caller"
	assert.Equal(t, "ghcr.io/test-org/fresh:v2", written.AppliedConfig.Image, "the in-memory agent must not see the caller's later mutation")
	assert.Equal(t, "fresh-profile", written.AppliedConfig.Profile)
	stored, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "ghcr.io/test-org/fresh:v2", stored.AppliedConfig.Image, "the stored row must not see the caller's later mutation")

	written.AppliedConfig.HarnessAuth = "echoed-by-broker"
	assert.Equal(t, "api-key", cfg.HarnessAuth, "an echo written onto the returned agent must not leak into the caller's struct")
}

// TestCopyBrokerEcho covers the helper the worker takes broker echoes with:
// non-empty fields win, empty fields leave dst alone, and the image is only
// taken when asked.
func TestCopyBrokerEcho(t *testing.T) {
	dst := &store.AgentAppliedConfig{Image: "qualified:v1", HarnessConfig: "hc", HarnessAuth: "auth", HarnessConfigSource: "src", Profile: "p", Model: "m"}
	copyBrokerEcho(dst, &store.AgentAppliedConfig{Image: "bare:v1", HarnessAuth: "echoed-auth", Model: "ignored"}, false)
	assert.Equal(t, store.AgentAppliedConfig{Image: "qualified:v1", HarnessConfig: "hc", HarnessAuth: "echoed-auth", HarnessConfigSource: "src", Profile: "p", Model: "m"}, *dst,
		"an empty HarnessConfigSource echo (older broker) keeps the recorded value")

	copyBrokerEcho(dst, &store.AgentAppliedConfig{Image: "resolved:v2", HarnessConfig: "echoed-hc", HarnessConfigSource: "echoed-src", Profile: "echoed-p"}, true)
	assert.Equal(t, store.AgentAppliedConfig{Image: "resolved:v2", HarnessConfig: "echoed-hc", HarnessAuth: "echoed-auth", HarnessConfigSource: "echoed-src", Profile: "echoed-p", Model: "m"}, *dst)

	snapshot := *dst
	copyBrokerEcho(nil, dst, true) // must not panic
	copyBrokerEcho(dst, nil, true)
	assert.Equal(t, snapshot, *dst, "a nil src must leave dst unchanged")
}

// TestCopyBrokerEcho_MirrorsApplyBrokerAgentConfig is the drift guard for
// copyBrokerEcho: applyBrokerAgentConfig (the source of truth) is run with a
// RemoteAgentInfo whose every string field is set, onto a copy of a config;
// copyBrokerEcho must then bring dst to exactly that copy. An AppliedConfig
// field applyBrokerAgentConfig starts writing that copyBrokerEcho does not
// copy fails here instead of being silently dropped on reincarnate.
func TestCopyBrokerEcho_MirrorsApplyBrokerAgentConfig(t *testing.T) {
	info := &RemoteAgentInfo{}
	v := reflect.ValueOf(info).Elem()
	for i := 0; i < v.NumField(); i++ {
		if f := v.Field(i); f.Kind() == reflect.String {
			f.SetString("echo-" + v.Type().Field(i).Name)
		}
	}
	base := store.AgentAppliedConfig{
		Image: "ghcr.io/test-org/base:v1", HarnessConfig: "base-hc", HarnessAuth: "base-auth",
		Profile: "base-profile", Model: "base-model", Task: "base-task",
	}
	echoed := base
	applyBrokerAgentConfig(&store.Agent{AppliedConfig: &echoed}, info)
	require.NotEqual(t, base, echoed, "the populated broker answer must change the config")

	dst := base
	copyBrokerEcho(&dst, &echoed, true)
	assert.Equal(t, echoed, dst, "copyBrokerEcho must copy every AppliedConfig field applyBrokerAgentConfig writes")
}

// TestReincarnateAgent_StartWithoutImageEchoKeepsQualifiedImage covers
// ptone/scion#1907's fallback: the reprovision echoes an unqualified image
// and the start succeeds without echoing any image. The row and the record
// keep the registry-qualified image buildFreshAppliedConfig resolved.
func TestReincarnateAgent_StartWithoutImageEchoKeepsQualifiedImage(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	disp.imageRegistry = "ghcr.io/test-org"
	disp.reprovisionImage = "explicit-image:v1"
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.AppliedConfig.Image = "ghcr.io/test-org/explicit-image:v1"
		a.AppliedConfig.CreateInputs.InlineConfig = &api.ScionConfig{Image: "explicit-image:v1"}
	})
	self := agentIdentityFor(agent.ID, project.ID)

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	r := waitForReincarnationSettled(t, s, agent.ID)
	require.Equal(t, store.AgentReincarnationStateCompleted, r.State, r.Error)
	require.NotNil(t, r.NewAppliedConfig)
	final, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	require.NotNil(t, final.AppliedConfig)
	assert.Equal(t, "ghcr.io/test-org/explicit-image:v1", r.NewAppliedConfig.Image, "the record must keep the qualified image")
	assert.Equal(t, "ghcr.io/test-org/explicit-image:v1", final.AppliedConfig.Image, "the row must keep the qualified image")
}

// TestReincarnateAgent_StartFailureKeepsQualifiedImage covers
// ptone/scion#1907: the reprovision echo carries the rendered config's
// unqualified image. When the start then fails, the row keeps the
// registry-qualified image buildFreshAppliedConfig resolved, while the
// reprovision's other echoed fields are still persisted.
func TestReincarnateAgent_StartFailureKeepsQualifiedImage(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	disp.imageRegistry = "ghcr.io/test-org"
	disp.reprovisionImage = "explicit-image:v1"
	disp.reprovisionEcho = func(cfg *store.AgentAppliedConfig) { cfg.HarnessAuth = "reprovision-echoed-auth" }
	disp.startErr = fmt.Errorf("broker refused start")
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.AppliedConfig.Image = "ghcr.io/test-org/explicit-image:v1"
		a.AppliedConfig.CreateInputs.InlineConfig = &api.ScionConfig{Image: "explicit-image:v1"}
	})
	self := agentIdentityFor(agent.ID, project.ID)

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	r := waitForReincarnationSettled(t, s, agent.ID)
	require.Equal(t, store.AgentReincarnationStateFailed, r.State)
	final, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	require.NotNil(t, final.AppliedConfig)
	assert.Equal(t, "ghcr.io/test-org/explicit-image:v1", final.AppliedConfig.Image,
		"a failed start must leave the qualified image, not the reprovision echo's unqualified one")
	assert.Equal(t, "reprovision-echoed-auth", final.AppliedConfig.HarnessAuth,
		"the reprovision echo's other fields are persisted by the starting step")
}

// TestReincarnateAgent_StartEchoPersistedAtCompletion covers
// ptone/scion#1907: every field the start response echoes is persisted on
// the row and the record at completion. The worker takes the echo
// explicitly, so this fails if that copy is removed (there is no pointer
// aliasing left to carry it).
func TestReincarnateAgent_StartEchoPersistedAtCompletion(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	disp.startEcho = func(cfg *store.AgentAppliedConfig) {
		cfg.Image = "ghcr.io/test-org/start-echo:v3"
		cfg.HarnessConfig = "start-echo-hc"
		cfg.HarnessAuth = "start-echo-auth"
		cfg.Profile = "start-echo-profile"
	}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	r := waitForReincarnationSettled(t, s, agent.ID)
	require.Equal(t, store.AgentReincarnationStateCompleted, r.State, r.Error)
	final, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	for name, cfg := range map[string]*store.AgentAppliedConfig{"row": final.AppliedConfig, "record": r.NewAppliedConfig} {
		require.NotNil(t, cfg, name)
		assert.Equal(t, "ghcr.io/test-org/start-echo:v3", cfg.Image, name)
		assert.Equal(t, "start-echo-hc", cfg.HarnessConfig, name)
		assert.Equal(t, "start-echo-auth", cfg.HarnessAuth, name)
		assert.Equal(t, "start-echo-profile", cfg.Profile, name)
	}
}

// TestDispatchImageRegistry_ReadsTheHTTPDispatcher proves the reincarnate
// path reads the registry from the HTTP dispatcher itself, the value it
// rewrites images with at send time.
func TestDispatchImageRegistry_ReadsTheHTTPDispatcher(t *testing.T) {
	d := &HTTPAgentDispatcher{}
	d.SetImageRegistry("ghcr.io/test-org")
	assert.Equal(t, "ghcr.io/test-org", dispatchImageRegistry(d))
	assert.Equal(t, "", dispatchImageRegistry(nil), "a dispatcher that does not rewrite images reports no registry")
}

// TestReincarnateAgent_LegacyAgent_DropsSkillsWithWarning covers the legacy
// skills addendum (design §3.4 Amendment A1): an agent with no CreateInputs
// (predates the field) must have InlineConfig.Skills dropped entirely by the
// legacy fallback, with a plan warning, rather than risk freezing stale
// injected skills in as template scope.
func TestReincarnateAgent_LegacyAgent_DropsSkillsWithWarning(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.AppliedConfig.CreateInputs = nil // legacy: predates CreateInputs
		a.AppliedConfig.InlineConfig = &api.ScionConfig{
			Skills: []api.SkillReference{
				{URI: "scion-platform://some-hub-skill", Scope: "hub"},
			},
		}
	})
	self := agentIdentityFor(agent.ID, project.ID)

	req := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{DryRun: true})
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, req, agent.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp ReincarnateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	foundLegacyWarning := false
	foundDroppedSkillsWarning := false
	for _, w := range resp.Plan.Warnings {
		if w == "explicit inputs reconstructed heuristically (agent predates CreateInputs)" {
			foundLegacyWarning = true
		}
		if containsAll(w, "dropped", "scion-platform://some-hub-skill") {
			foundDroppedSkillsWarning = true
		}
	}
	assert.True(t, foundLegacyWarning, "expected legacy-fallback warning, got %v", resp.Plan.Warnings)
	assert.True(t, foundDroppedSkillsWarning, "expected dropped-skills warning naming the ref, got %v", resp.Plan.Warnings)
}

// TestReincarnateAgent_LegacyAgent_TemplateEnvV1ToV2 is the AC-2a test the
// design's A1 addendum 2 calls for: a legacy
// agent's InlineConfig.Env is indistinguishable-by-inspection from a mix of
// explicit keys and template defaults aliased in at create time
// (buildAppliedConfig aliases AppliedConfig.Env to InlineConfig.Env). The
// legacy fallback must drop every key the CURRENT template defines — so a
// template env default that changed from v1 (at create) to v2 (at
// reincarnate) comes out as v2, not the stale v1 — and keep, with a warning,
// any key the template does not define.
func TestReincarnateAgent_LegacyAgent_TemplateEnvV1ToV2(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)

	template := &store.Template{
		ID:          tid("tmpl-env-v2-" + t.Name()),
		Name:        "t",
		Slug:        "reincarnate-template-env-" + tidSlugSafe(t.Name()),
		Harness:     "claude",
		Scope:       store.TemplateScopeGlobal,
		Status:      store.TemplateStatusActive,
		ContentHash: "template-hash-v2",
		Config: &store.TemplateConfig{
			Env: map[string]string{"TEMPLATE_ENV_KEY": "v2"},
		},
	}
	require.NoError(t, s.CreateTemplate(context.Background(), template))

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Template = template.Slug
		a.AppliedConfig.CreateInputs = nil // legacy: predates CreateInputs
		a.AppliedConfig.InlineConfig = &api.ScionConfig{
			Env: map[string]string{
				// Aliased in at create from the template, which was v1 then.
				"TEMPLATE_ENV_KEY": "v1",
				// A genuinely explicit, agent-specific key the template never defined.
				"AGENT_SPECIFIC_KEY": "keep-me",
			},
		}
		// The live (pre-reincarnate) AppliedConfig.Env mirrors the alias, as
		// buildAppliedConfig would have left it.
		a.AppliedConfig.Env = map[string]string{
			"TEMPLATE_ENV_KEY":   "v1",
			"AGENT_SPECIFIC_KEY": "keep-me",
		}
	})
	self := agentIdentityFor(agent.ID, project.ID)

	req := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{DryRun: true})
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, req, agent.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp ReincarnateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	// The plan's env diff must show TEMPLATE_ENV_KEY changing v1 -> v2 (via
	// the "changed" key name), and AGENT_SPECIFIC_KEY surviving untouched
	// (absent from added/removed/changed).
	assert.Contains(t, resp.Plan.EnvKeys.Changed, "TEMPLATE_ENV_KEY")
	assert.NotContains(t, resp.Plan.EnvKeys.Changed, "AGENT_SPECIFIC_KEY")
	assert.NotContains(t, resp.Plan.EnvKeys.Removed, "AGENT_SPECIFIC_KEY")

	foundDroppedWarning := false
	foundKeptWarning := false
	for _, w := range resp.Plan.Warnings {
		// The old value ("v1") must NEVER appear in the plan — it
		// came from a legacy agent's live env, indistinguishable from a
		// genuine per-agent secret that happens to share this key name. Only
		// the template's own new value is safe to surface.
		if containsAll(w, "dropped", "TEMPLATE_ENV_KEY", `new="v2"`) {
			assert.NotContains(t, w, `old="v1"`, "the old value must be masked, never shown in the plan")
			foundDroppedWarning = true
		}
		if containsAll(w, "kept", "AGENT_SPECIFIC_KEY") {
			foundKeptWarning = true
		}
	}
	assert.True(t, foundDroppedWarning, "expected a warning naming TEMPLATE_ENV_KEY's old/new values, got %v", resp.Plan.Warnings)
	assert.True(t, foundKeptWarning, "expected a warning flagging AGENT_SPECIFIC_KEY as kept-but-uncertain, got %v", resp.Plan.Warnings)
}

// TestReincarnateAgent_FreshConfigDoesNotAliasEnv proves design §3.3 A1
// addendum 2 rule 3: the reincarnate builder's fresh AppliedConfig.Env and
// InlineConfig.Env must be separate maps, so a template env default that
// resolveDerivedConfig merges into AppliedConfig.Env does not silently also
// appear in the new generation's InlineConfig.Env (which is dispatched to
// the broker as part of the persisted config, not just as resolved env).
func TestReincarnateAgent_FreshConfigDoesNotAliasEnv(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)

	template := &store.Template{
		ID:          tid("tmpl-noalias-" + t.Name()),
		Name:        "t",
		Slug:        "reincarnate-template-noalias-" + tidSlugSafe(t.Name()),
		Harness:     "claude",
		Scope:       store.TemplateScopeGlobal,
		Status:      store.TemplateStatusActive,
		ContentHash: "template-hash-noalias",
		Config: &store.TemplateConfig{
			Env: map[string]string{"TEMPLATE_DEFAULT_KEY": "from-template"},
		},
	}
	require.NoError(t, s.CreateTemplate(context.Background(), template))

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Template = template.Slug
		a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{
			InlineConfig: &api.ScionConfig{
				Env: map[string]string{"EXPLICIT_KEY": "explicit-value"},
			},
		}
	})

	fresh, _, err := srv.buildFreshAppliedConfig(context.Background(), agent, project, "")
	require.NoError(t, err)

	require.Contains(t, fresh.Env, "TEMPLATE_DEFAULT_KEY", "template env default must reach AppliedConfig.Env")
	require.NotNil(t, fresh.InlineConfig)
	assert.NotContains(t, fresh.InlineConfig.Env, "TEMPLATE_DEFAULT_KEY",
		"template env default must NOT leak into the new generation's InlineConfig.Env (no aliasing in the fresh config)")
	assert.Equal(t, "explicit-value", fresh.InlineConfig.Env["EXPLICIT_KEY"])

	// Mutating AppliedConfig.Env after the fact must not reach InlineConfig.Env.
	fresh.Env["MUTATED_AFTER"] = "x"
	assert.NotContains(t, fresh.InlineConfig.Env, "MUTATED_AFTER")
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// =============================================================================
// Concurrency (AC-8)
// =============================================================================

func TestReincarnateAgent_AC8_ConflictWhenAlreadyPending(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)

	// Manually seed a pending reincarnation, simulating one already in flight
	// (avoids a timing-dependent race against the real worker goroutine). The
	// The 409 gate (design §3.4 Amendment A3) checks agent.ReincarnationState, not the
	// agent_reincarnations table directly, so both must be set together —
	// exactly the invariant handleReincarnateAgent's claim-then-create order
	// maintains for a real request.
	require.NoError(t, s.CreateAgentReincarnation(context.Background(), &store.AgentReincarnation{
		AgentID:        agent.ID,
		FromGeneration: 1,
		ToGeneration:   2,
		State:          store.AgentReincarnationStatePending,
	}))
	agent.ReincarnationState = store.ReincarnationStatePending
	require.NoError(t, s.UpdateAgent(context.Background(), agent))

	req := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"})
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, req, agent.ID)

	assert.Equal(t, http.StatusConflict, rec.Code)
}

// TestReincarnateAgent_DryRunConflictsWhenAlreadyStarting is the design
// §3.4 Amendment A11 item 3 regression test: the already-pending/already-
// in-flight 409 gate used to run only on the real (non-dry-run) path, so a
// --dry-run request against an agent whose migration was already in the
// "starting" state would compute and return a plan instead of reporting the
// conflict — misleading a caller into thinking a reincarnate was safe to
// start when one was already running. The gate must fire for --dry-run too,
// and it must not touch the agent row at all (no claim to make on a dry
// run) — state_version stays exactly where it was.
func TestReincarnateAgent_DryRunConflictsWhenAlreadyStarting(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)

	require.NoError(t, s.CreateAgentReincarnation(context.Background(), &store.AgentReincarnation{
		AgentID:        agent.ID,
		FromGeneration: 1,
		ToGeneration:   2,
		State:          store.AgentReincarnationStateStarting,
	}))
	agent.ReincarnationState = store.ReincarnationStateStarting
	require.NoError(t, s.UpdateAgent(context.Background(), agent))
	beforeVersion := agent.StateVersion

	req := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{DryRun: true})
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, req, agent.ID)

	assert.Equal(t, http.StatusConflict, rec.Code, "a dry run must 409 against an in-flight reincarnation, not silently compute a plan")

	after, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, beforeVersion, after.StateVersion, "a dry run must never write the agent row, conflict or not")
	assert.Equal(t, store.ReincarnationStateStarting, after.ReincarnationState)
}

// TestReincarnateAgent_AC8_OrphanCannotWedgeAfterConflict is the design §3.4
// Amendment A3 regression test: a version conflict on the claim write (the guarded
// ClaimAgentReincarnation that sets reincarnation_state=pending) must leave nothing
// behind — no orphaned agent_reincarnations row, and no stuck claim — so a
// retry succeeds. Before the fix, the record was created FIRST, so any
// failure on the following UpdateAgent left a permanent pending row with no
// worker running for it and no way to clear it.
type failOnceUpdateStore struct {
	store.Store
	mu     sync.Mutex
	failed bool
}

func (f *failOnceUpdateStore) takeFailure() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failed {
		return false
	}
	f.failed = true
	return true
}

func (f *failOnceUpdateStore) ClaimAgentReincarnation(ctx context.Context, agentID string, expectedVersion int64, at time.Time) (int64, error) {
	if f.takeFailure() {
		return 0, store.ErrVersionConflict
	}
	return f.Store.ClaimAgentReincarnation(ctx, agentID, expectedVersion, at)
}

// WithTx injects the same one-shot conflict into the claim transaction.
func (f *failOnceUpdateStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return f.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&failOnceUpdateTx{Store: tx, parent: f})
	})
}

type failOnceUpdateTx struct {
	store.Store
	parent *failOnceUpdateStore
}

func (t *failOnceUpdateTx) ClaimAgentReincarnation(ctx context.Context, agentID string, expectedVersion int64, at time.Time) (int64, error) {
	if t.parent.takeFailure() {
		return 0, store.ErrVersionConflict
	}
	return t.Store.ClaimAgentReincarnation(ctx, agentID, expectedVersion, at)
}

func TestReincarnateAgent_AC8_OrphanCannotWedgeAfterConflict(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)

	orig := srv.store
	srv.store = &failOnceUpdateStore{Store: orig}
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
	assert.Equal(t, http.StatusConflict, rec.Code, "first request hits the injected version conflict on the claim write")
	srv.store = orig

	list, err := s.ListAgentReincarnations(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Empty(t, list, "the claim write fails before the record is ever created, so nothing is orphaned")

	after, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ReincarnationStateNone, after.ReincarnationState, "the failed claim must not stick")

	rec = httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
	assert.Equal(t, http.StatusAccepted, rec.Code, "retry after a transient conflict must succeed; got %d %s", rec.Code, rec.Body.String())
}

// conflictOnFailedWriteStore rejects the first UpdateAgent that sets
// reincarnation_state=failed with a version conflict, simulating a
// concurrent full-row write landing between failReincarnation's read and
// write.
type conflictOnFailedWriteStore struct {
	store.Store
	mu   sync.Mutex
	done bool
}

func (f *conflictOnFailedWriteStore) UpdateAgent(ctx context.Context, a *store.Agent) error {
	f.mu.Lock()
	if !f.done && a.ReincarnationState == store.ReincarnationStateFailed {
		f.done = true
		f.mu.Unlock()
		return store.ErrVersionConflict
	}
	f.mu.Unlock()
	return f.Store.UpdateAgent(ctx, a)
}

// TestReincarnateAgent_FailReincarnationConflictDoesNotWedgeAgent is the
// design §3.4 Amendment A5.1 regression test: failReincarnation must
// retry on a version conflict, the same as every other worker write. Before
// the fix, a single conflict on the "failed" write left the agent row stuck
// non-terminal (e.g. "starting") while the reincarnation record said
// "failed" — a state the boot/periodic sweep cannot clear, since it keys off
// non-terminal *records*, and this one is already terminal. Every later
// request 409s forever.
func TestReincarnateAgent_FailReincarnationConflictDoesNotWedgeAgent(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	disp.startErr = fmt.Errorf("boom")
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)

	// Installed before the request and never restored: the worker goroutine
	// reads srv.store and can outlive waitForReincarnationSettled, so a
	// restore write would race it (ptone/scion#3184).
	installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *conflictOnFailedWriteStore {
		return &conflictOnFailedWriteStore{Store: inner}
	})
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	waitForReincarnationSettled(t, s, agent.ID)

	after, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ReincarnationStateFailed, after.ReincarnationState,
		"the retry must land the agent's reincarnation_state at failed, not leave it stuck non-terminal")
	assert.Equal(t, "error", after.Phase)

	rec = httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h2"}), agent.ID)
	assert.NotEqual(t, http.StatusConflict, rec.Code,
		"agent must not be wedged after a failed reincarnation whose write hit one conflict; got %d %s", rec.Code, rec.Body.String())
}

// blockingStopDispatcher blocks the first Stop call until released, so a
// test can pause a worker mid-flight and run a sweep concurrently.
type blockingStopDispatcher struct {
	*reincarnateTestDispatcher
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (d *blockingStopDispatcher) DispatchAgentStop(ctx context.Context, a *store.Agent) error {
	first := false
	d.once.Do(func() { first = true })
	if first {
		close(d.entered)
		<-d.release
	}
	return d.reincarnateTestDispatcher.DispatchAgentStop(ctx, a)
}

// TestReincarnateAgent_BootSweepDoesNotAdmitSecondWorkerWhileFirstInFlight is
// the design §3.4 Amendment A5.2 regression test: the replica-safe
// sweep must not touch a genuinely in-flight reincarnation just because a
// sweep happens to run concurrently (simulating another replica booting).
// The staleness bound is what protects it — sweepStaleReincarnations (the
// real 30-minute bound, not a test-only cutoff) must find the record and
// agent state both far too fresh to touch, so a second reincarnate request
// against the same agent still gets 409, and no second worker starts.
func TestReincarnateAgent_BootSweepDoesNotAdmitSecondWorkerWhileFirstInFlight(t *testing.T) {
	base := newReincarnateTestDispatcher()
	disp := &blockingStopDispatcher{reincarnateTestDispatcher: base, entered: make(chan struct{}), release: make(chan struct{})}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h1"}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	<-disp.entered // worker 1 is inside Stop

	// "Replica B" boots and runs its sweep against the shared DB, using the
	// real production bound (not sweepStaleReincarnationsOlderThan).
	n, err := srv.sweepStaleReincarnations(context.Background())
	require.NoError(t, err)
	assert.Zero(t, n, "a worker that started milliseconds ago must not look stale")

	rec = httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h2"}), agent.ID)
	code2 := rec.Code
	close(disp.release)

	assert.Equal(t, http.StatusConflict, code2, "a second reincarnation must not be admitted while the first worker is still running")

	waitForReincarnationSettled(t, s, agent.ID)
	list, err := s.ListAgentReincarnations(context.Background(), agent.ID)
	require.NoError(t, err)
	require.Len(t, list, 1, "exactly one worker must have run against this agent")
	assert.Equal(t, store.AgentReincarnationStateCompleted, list[0].State)
}

// phaseObservingDispatcher simulates the container reporting a status-only
// phase update (as sciontool/the broker do via UpdateAgentStatus, which does
// not bump state_version) during Stop, and records the persisted phase seen
// at Reprovision time — the design §3.4 Amendment A3 regression test.
type phaseObservingDispatcher struct {
	*reincarnateTestDispatcher
	s                  store.Store
	phaseAtReprovision string
}

func (d *phaseObservingDispatcher) DispatchAgentStop(ctx context.Context, a *store.Agent) error {
	_ = d.s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Phase: "stopped"})
	return d.reincarnateTestDispatcher.DispatchAgentStop(ctx, a)
}

func (d *phaseObservingDispatcher) DispatchAgentReprovision(ctx context.Context, a *store.Agent) error {
	cur, err := d.s.GetAgent(ctx, a.ID)
	if err == nil {
		d.phaseAtReprovision = cur.Phase
	}
	return d.reincarnateTestDispatcher.DispatchAgentReprovision(ctx, a)
}

// TestReincarnateAgent_WorkerDoesNotClobberReportedPhase: the worker's
// per-step writes must merge onto the CURRENT row (re-read each time), not
// overwrite it with a stale in-memory copy. Before the fix, the worker wrote
// back the whole agent object it loaded before Stop, on every subsequent
// step — including a step wholly unrelated to the concurrent status report —
// so a status-only phase update landing during Stop was silently reverted at
// Reprovision time.
func TestReincarnateAgent_WorkerDoesNotClobberReportedPhase(t *testing.T) {
	base := newReincarnateTestDispatcher()
	disp := &phaseObservingDispatcher{reincarnateTestDispatcher: base}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	disp.s = s
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	waitForReincarnationSettled(t, s, agent.ID)
	assert.NotEqual(t, "running", disp.phaseAtReprovision,
		"while the container is stopped and being reprovisioned, the row must not claim phase=running")
	// The worker explicitly moves the phase to "provisioning" for this step
	// (design §3.4 Amendment A3's "set phase to stopping, then provisioning, then
	// starting"); a status-only "stopped" report from mid-Stop is expected
	// to be superseded by that transition, not preserved forever. What must
	// NOT happen is the pre-Stop "running" value silently surviving because
	// the worker overwrote the status report with a stale in-memory copy.
	assert.Equal(t, "provisioning", disp.phaseAtReprovision)
}

// =============================================================================
// End-to-end worker run (AC-1, AC-3, AC-5)
// =============================================================================

func TestReincarnateAgent_EndToEnd_IdentityContinuityAndHandoff(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)

	// AC-5: self-migration. The dispatcher's context is context.Background()
	// (see runReincarnationWorker), not r.Context() — cancelling the
	// request's context (as a real self-stop container would) must not abort
	// the worker. Simulate that by cancelling the request context right
	// after the handler returns 202, before waiting for the worker.
	ctx, cancel := context.WithCancel(context.Background())
	self := agentIdentityFor(agent.ID, project.ID)
	req := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "do the thing next"})
	req = req.WithContext(contextWithIdentity(ctx, self))
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, req, agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	cancel() // simulate the calling container being stopped

	waitForReincarnationSettled(t, s, agent.ID)

	final, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)

	// AC-1: identity preserved, generation incremented.
	assert.Equal(t, agent.ID, final.ID)
	assert.Equal(t, agent.Slug, final.Slug)
	assert.Equal(t, tid("user-creator"), final.CreatedBy)
	assert.Equal(t, []string{tid("user-creator")}, final.Ancestry)
	assert.Equal(t, "project", final.MessageMode)
	assert.Equal(t, map[string]string{"team": "platform"}, final.Labels)
	assert.Equal(t, 2, final.Generation)
	assert.Equal(t, "", final.ReincarnationState)

	list, err := s.ListAgentReincarnations(context.Background(), agent.ID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, store.AgentReincarnationStateCompleted, list[0].State)
	assert.NotNil(t, list[0].CompletedAt)
	require.NotNil(t, list[0].PreviousAppliedConfig)
	assert.Equal(t, "old-image:v1", list[0].PreviousAppliedConfig.Image)

	// AC-3: the new generation's first harness input is the preamble plus
	// the handoff, byte for byte, and the harness started without resume.
	assert.Equal(t, 1, disp.startCalls)
	require.NotNil(t, disp.lastStartResume)
	assert.False(t, *disp.lastStartResume, "must start without the harness resume flag (decision D3)")
	assert.Contains(t, disp.lastStartTask, "do the thing next")
	assert.Contains(t, disp.lastStartTask, fmt.Sprintf("generation %d of agent %q", 2, agent.Slug))
	assert.Equal(t, 1, disp.reprovisionCalls)
	assert.GreaterOrEqual(t, disp.stopCalls, 1)
}

// TestBuildReincarnationPreamble_CatchUpWindow is the Amendment A25
// 2a.3/R4 (p2a-r1 review), then O-b (p2a-r2 review), update of the former
// TestBuildReincarnationPreamble_DoesNotPromiseRedelivery: now that 2a.1
// (catch-up works in agent containers) and 2a.2/R3 (the migration gate
// persists-and-defers on every hub delivery path instead of
// rejecting/dropping) are both true, step 2 is allowed to say messages sent
// during the migration can be read with catch-up. R4 corrected the window
// to name only a start (an end would have to be the state-clear instant,
// not known until long after this text is built). O-b corrected the command
// itself: plain `scion conversation list` truncates IDs to 12 runes with no
// `conv:` prefix, which `catch-up` cannot accept — step 2 now says
// `list --json` and spells out the `conv:<id>` prefix.
func TestBuildReincarnationPreamble_CatchUpWindow(t *testing.T) {
	srv, _ := testServer(t)
	agent := &store.Agent{ID: "agent-1", Slug: "arqa-a"}

	start := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	requester := reincarnationRequesterContext{Handle: "user:requester@example.com", Resolved: true}
	preamble := srv.buildReincarnationPreamble(agent, 2, "do the thing next", start, requester, nil)

	assert.Contains(t, preamble,
		"2. Run `scion conversation list --json` and, for each conversation, "+
			"`scion conversation catch-up conv:<id> --since <duration reaching back to 2026-09-28T10:00:00Z>`.",
		"step 2 must be runnable as written: --json for full IDs, and the conv:<id> prefix catch-up requires")
	assert.Contains(t, preamble,
		"Messages sent to you since 2026-09-28T10:00:00Z were saved to your conversations, not dropped.",
		"step 2 must reinstate the catch-up claim now that 2a.1/R3 make it true, naming only a start")
	assert.NotContains(t, preamble, "to 2026-09-28T10:05",
		"an end timestamp would be a lower bound the preamble cannot honestly state (R4)")
	assert.Contains(t, preamble,
		"If that command is unavailable in this environment, rely on the handoff and on incoming messages.",
		"the image-lag fallback (#1910) must remain since it is not fixed by this phase")
	assert.NotContains(t, preamble, "redeliver",
		"catch-up is not automatic redelivery — the wording must not claim that")
	assert.Contains(t, preamble, "do the thing next", "the handoff must still be appended verbatim")
}

// TestBuildReincarnationPreamble_A26StepsAndNoHandoff is the design
// Amendment A26 golden test for Phase 2b: steps 1-2 keep their 2a wording
// (pinned separately by TestBuildReincarnationPreamble_CatchUpWindow above),
// and "everything else in §3.9" — steps 3-4 and the no-handoff fallback text
// — comes in here. It also pins Amendment A23's invariant that the preamble
// carries no workspace-mode-specific text: a clone-per-agent agent and a
// shared/mounted-workspace agent must produce byte-identical step text, both
// with a handoff and without one. This test only exercises the *resolved,
// non-self* case; TestBuildReincarnationPreamble_A262_SelfRequest and
// TestBuildReincarnationPreamble_A262_UnresolvedRequester below cover the
// other two reincarnationRequesterContext shapes.
func TestBuildReincarnationPreamble_A26StepsAndNoHandoff(t *testing.T) {
	srv, _ := testServer(t)
	start := time.Date(2026, 9, 29, 8, 0, 0, 0, time.UTC)
	const requesterHandle = "user:requester@example.com"
	requester := reincarnationRequesterContext{Handle: requesterHandle, Resolved: true}

	cloneAgent := &store.Agent{
		ID:   "agent-clone",
		Slug: "clone-agt",
		AppliedConfig: &store.AgentAppliedConfig{
			GitClone: &api.GitCloneConfig{URL: "https://example.invalid/repo.git"},
		},
	}
	sharedAgent := &store.Agent{
		ID:   "agent-shared",
		Slug: "shared-agt",
		AppliedConfig: &store.AgentAppliedConfig{
			Workspace: "/mnt/shared/project",
		},
	}

	assertA26Steps := func(t *testing.T, preamble string, agent *store.Agent, toGeneration int) {
		t.Helper()
		assert.Contains(t, preamble,
			fmt.Sprintf("You are generation %d of agent %q (id %s), reincarnated on request of %s.", toGeneration, agent.Slug, agent.ID, requesterHandle),
			"the header line (A26.1) must name the resolved requester")
		assert.Contains(t, preamble,
			" 1. Verify your environment: `git status` shows the branch and state your handoff describes, and any files your handoff names as canonical are readable.\n",
			"step 1 must keep A23's neutral wording regardless of workspace mode")
		assert.Contains(t, preamble,
			fmt.Sprintf(" 3. Message %s that generation %d is up, and state your next action.\n", requesterHandle, toGeneration),
			"step 3 (§3.9, A26.1) must name both the resolved requester and the target generation")
		assert.Contains(t, preamble,
			" 4. Continue from the handoff's \"Immediate active work\" section below (its next action). Do not redo anything listed under \"Do not redo\".\n",
			"step 4 (§3.9, come in per A26) must point at the handoff template's real section headings")
	}

	for _, tc := range []struct {
		name  string
		agent *store.Agent
	}{
		{"clone-per-agent", cloneAgent},
		{"shared/non-clone", sharedAgent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("with handoff", func(t *testing.T) {
				preamble := srv.buildReincarnationPreamble(tc.agent, 4, "next: ship it", start, requester, nil)
				assertA26Steps(t, preamble, tc.agent, 4)
				assert.Contains(t, preamble, "next: ship it")
				assert.NotContains(t, preamble, "No handoff was provided")
			})
			t.Run("no handoff", func(t *testing.T) {
				preamble := srv.buildReincarnationPreamble(tc.agent, 4, "", start, requester, nil)
				assertA26Steps(t, preamble, tc.agent, 4)
				assert.Contains(t, preamble,
					"No handoff was provided. Reconstruct context from your branch, your conversations "+
						"(`scion conversation list`), and any project scratchpad before acting.",
					"the no-handoff fallback text (§3.9, come in per A26) must name `scion conversation list`")
			})
		})
	}
}

// TestBuildReincarnationPreamble_A262_SelfRequest is the design Amendment
// A26.2 R1 golden test: a self-migration must never tell the new generation
// to message the agent's own handle. It covers both sub-cases of the fix —
// a resolved creator hint appended to step 3, and no hint at all when the
// creator didn't resolve.
func TestBuildReincarnationPreamble_A262_SelfRequest(t *testing.T) {
	srv, _ := testServer(t)
	start := time.Date(2026, 9, 29, 8, 0, 0, 0, time.UTC)
	agent := &store.Agent{ID: "agent-self", Slug: "self-agt"}

	t.Run("with a resolved creator hint", func(t *testing.T) {
		requester := reincarnationRequesterContext{IsSelf: true, CreatorHandle: "user:owner@example.com", CreatorResolved: true}
		preamble := srv.buildReincarnationPreamble(agent, 3, "h", start, requester, nil)

		assert.Contains(t, preamble,
			`You are generation 3 of agent "self-agt" (id agent-self), reincarnated at its own request.`,
			"the header must say the migration was self-requested, never name a requester handle")
		assert.Contains(t, preamble,
			` 3. Message the owner named in your handoff's "Authority and ownership" section that generation 3 is up, and state your next action. If the handoff names none, message user:owner@example.com.`,
			"step 3 must point at the handoff, then give the resolved creator hint as its own sentence (A26.3 N2)")
		assert.NotContains(t, preamble, "agent:"+agent.Slug,
			"the preamble must never emit the agent's own handle (A26.2 R1)")
	})

	t.Run("with no resolved creator", func(t *testing.T) {
		requester := reincarnationRequesterContext{IsSelf: true}
		preamble := srv.buildReincarnationPreamble(agent, 3, "h", start, requester, nil)

		assert.Contains(t, preamble,
			`You are generation 3 of agent "self-agt" (id agent-self), reincarnated at its own request.`)
		assert.Contains(t, preamble,
			` 3. Message the owner named in your handoff's "Authority and ownership" section that generation 3 is up, and state your next action.`+"\n",
			"with no creator hint, step 3 must end right after the handoff-driven instruction, no parenthetical")
		assert.NotContains(t, preamble, "If the handoff names none",
			"no creator hint means no fallback clause at all")
		assert.NotContains(t, preamble, "agent:"+agent.Slug)
	})
}

// TestBuildReincarnationPreamble_A262_UnresolvedRequester is the design
// Amendment A26.2 O2 golden test: when the requester cannot be resolved (and
// it is not a self-request), the header's "on request of" clause is omitted
// entirely rather than rendering the generic fallback phrase there too, but
// step 3 still keeps the fallback phrase so there is at least an
// instruction.
func TestBuildReincarnationPreamble_A262_UnresolvedRequester(t *testing.T) {
	srv, _ := testServer(t)
	start := time.Date(2026, 9, 29, 8, 0, 0, 0, time.UTC)
	agent := &store.Agent{ID: "agent-u", Slug: "unresolved-agt"}
	requester := reincarnationRequesterContext{} // Resolved: false, IsSelf: false

	preamble := srv.buildReincarnationPreamble(agent, 5, "h", start, requester, nil)

	assert.Contains(t, preamble, `You are generation 5 of agent "unresolved-agt" (id agent-u).`+"\n",
		"an unresolved, non-self requester must omit the 'on request of' clause entirely (Amendment A26.2 O2)")
	assert.NotContains(t, preamble, "on request of",
		"no tautological fallback clause in the header")
	assert.Contains(t, preamble,
		" 3. Message whoever requested this migration that generation 5 is up, and state your next action.\n",
		"step 3 still keeps the generic fallback phrase so there is some instruction")
}

// TestReincarnationChangesLine is the design Amendment A26.3 FYI-1 test for
// the "Changes:" preamble line: computeReincarnationPlan's diff is rendered
// only for the fields that actually changed, an empty side renders as
// "(none)", and the whole line is omitted when nothing changed.
func TestReincarnationChangesLine(t *testing.T) {
	cases := []struct {
		name string
		plan ReincarnationPlan
		want string
	}{
		{
			name: "nothing changed: no line at all",
			plan: ReincarnationPlan{
				Template:   FieldChange{Old: "h1", New: "h1"},
				Image:      FieldChange{Old: "img:v1", New: "img:v1"},
				HarnessCfg: FieldChange{Old: "hc1", New: "hc1"},
			},
			want: "",
		},
		{
			name: "all three changed",
			plan: ReincarnationPlan{
				Template:   FieldChange{Old: "h1", New: "h2"},
				Image:      FieldChange{Old: "img:v1", New: "img:v2"},
				HarnessCfg: FieldChange{Old: "hc1", New: "hc2"},
			},
			want: "Changes: template h1->h2, image img:v1->img:v2, harness-config hc1->hc2\n",
		},
		{
			name: "only image changed",
			plan: ReincarnationPlan{
				Template:   FieldChange{Old: "h1", New: "h1"},
				Image:      FieldChange{Old: "img:v1", New: "img:v2"},
				HarnessCfg: FieldChange{Old: "hc1", New: "hc1"},
			},
			want: "Changes: image img:v1->img:v2\n",
		},
		{
			name: "empty old side renders as (none)",
			plan: ReincarnationPlan{
				Template: FieldChange{Old: "", New: "h1"},
			},
			want: "Changes: template (none)->h1\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, reincarnationChangesLine(tc.plan))
		})
	}
}

// TestBuildReincarnationPreamble_A263_ChangesLine is the design Amendment
// A26.3 FYI-1 golden test for where the "Changes:" line sits in the full
// preamble: right after the header line, before "Before resuming:", and
// omitted entirely when nothing changed.
func TestBuildReincarnationPreamble_A263_ChangesLine(t *testing.T) {
	srv, _ := testServer(t)
	start := time.Date(2026, 9, 29, 8, 0, 0, 0, time.UTC)
	agent := &store.Agent{ID: "agent-c", Slug: "changes-agt"}
	requester := reincarnationRequesterContext{Handle: "user:req@example.com", Resolved: true}

	t.Run("changed: the line appears between the header and Before resuming", func(t *testing.T) {
		plan := &ReincarnationPlan{
			Template: FieldChange{Old: "h1", New: "h2"},
			Image:    FieldChange{Old: "img:v1", New: "img:v2"},
		}
		preamble := srv.buildReincarnationPreamble(agent, 2, "h", start, requester, plan)

		want := `You are generation 2 of agent "changes-agt" (id agent-c), reincarnated on request of user:req@example.com.` + "\n" +
			"Changes: template h1->h2, image img:v1->img:v2\n" +
			"Before resuming:\n"
		assert.Contains(t, preamble, want)
	})

	t.Run("unchanged: no Changes line at all", func(t *testing.T) {
		plan := &ReincarnationPlan{
			Template: FieldChange{Old: "h1", New: "h1"},
		}
		preamble := srv.buildReincarnationPreamble(agent, 2, "h", start, requester, plan)

		assert.NotContains(t, preamble, "Changes:")
		want := `You are generation 2 of agent "changes-agt" (id agent-c), reincarnated on request of user:req@example.com.` + "\n" +
			"Before resuming:\n"
		assert.Contains(t, preamble, want)
	})

	t.Run("nil plan: no Changes line, no panic (Amendment A26.4 O1)", func(t *testing.T) {
		preamble := srv.buildReincarnationPreamble(agent, 2, "h", start, requester, nil)

		assert.NotContains(t, preamble, "Changes:")
		want := `You are generation 2 of agent "changes-agt" (id agent-c), reincarnated on request of user:req@example.com.` + "\n" +
			"Before resuming:\n"
		assert.Contains(t, preamble, want)
	})

	t.Run("content hashes are abbreviated in the preamble line (Amendment A26.4 O2)", func(t *testing.T) {
		oldHash := "sha256:" + strings.Repeat("a", 64)
		newHash := "sha256:" + strings.Repeat("b", 64)
		plan := &ReincarnationPlan{
			Template:   FieldChange{Old: oldHash, New: newHash},
			HarnessCfg: FieldChange{Old: oldHash, New: newHash},
		}
		preamble := srv.buildReincarnationPreamble(agent, 2, "h", start, requester, plan)

		wantAbbrev := "sha256:" + strings.Repeat("a", 12) + "->" + "sha256:" + strings.Repeat("b", 12)
		assert.Contains(t, preamble, "Changes: template "+wantAbbrev+", harness-config "+wantAbbrev+"\n")
		assert.NotContains(t, preamble, oldHash, "the full 64-hex hash must not appear in the preamble")
		assert.NotContains(t, preamble, newHash, "the full 64-hex hash must not appear in the preamble")
	})
}

// TestReincarnateAbbreviateHash is the design Amendment A26.4 O2 unit test
// for the preamble-only hash abbreviation: a `sha256:<64 hex>` value
// (pkg/transfer/hash.go's shape) is shortened to `sha256:` plus 12 hex; an
// empty value renders as "(none)"; any other shape (not a content hash, or
// a `sha256:` prefix with the wrong length) is returned unabbreviated.
func TestReincarnateAbbreviateHash(t *testing.T) {
	fullHash := "sha256:" + strings.Repeat("f", 64)
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty renders as (none)", "", "(none)"},
		{"a full sha256 hash is abbreviated to 12 hex", fullHash, "sha256:" + strings.Repeat("f", 12)},
		{"a non-hash value is unchanged", "template-image:v2", "template-image:v2"},
		{"a sha256: prefix with the wrong length is unchanged", "sha256:short", "sha256:short"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, reincarnateAbbreviateHash(tc.in))
		})
	}
}

// TestResolveReincarnationRequesterName_A26_1 is the design Amendment A26.1
// (Amendment A26.2) test matrix for resolving a bare principal ID to a
// display string: a resolvable user, a resolvable agent, a principal ID
// that resolves to neither (unresolvable), a GetUser store error that still
// falls through to a successful GetAgent lookup (fall-through), and a
// genuine store error that ends in the fallback (fallback) — these are
// distinct outcomes, so this file has one test for each. All cases must be
// safe — the function never panics or errors, only ever returns
// (string, bool) — matching the requirement that a lookup failure must
// never fail the reincarnation itself.
func TestResolveReincarnationRequesterName_A26_1(t *testing.T) {
	t.Run("empty id falls back immediately", func(t *testing.T) {
		srv, _ := testServer(t)
		handle, resolved := srv.resolveReincarnationRequesterName(context.Background(), "")
		assert.Equal(t, reincarnationRequesterFallback, handle)
		assert.False(t, resolved)
	})

	t.Run("resolves a user", func(t *testing.T) {
		srv, s := testServer(t)
		user := &store.User{
			ID:      tid("a261-req-user"),
			Email:   "req-user@example.com",
			Role:    store.UserRoleMember,
			Status:  store.UserStatusActive,
			Created: time.Now(),
		}
		require.NoError(t, s.CreateUser(context.Background(), user))

		handle, resolved := srv.resolveReincarnationRequesterName(context.Background(), user.ID)
		assert.Equal(t, "user:req-user@example.com", handle)
		assert.True(t, resolved)
	})

	t.Run("resolves an agent when the ID is not a user", func(t *testing.T) {
		disp := newReincarnateTestDispatcher()
		srv, s, project, broker := setupReincarnateTestServer(t, disp)
		agent := newReincarnateTestAgent(t, s, project, broker, nil)

		handle, resolved := srv.resolveReincarnationRequesterName(context.Background(), agent.ID)
		assert.Equal(t, "agent:"+agent.Slug, handle)
		assert.True(t, resolved)
	})

	t.Run("unresolvable ID falls back with a WARN, does not panic or error", func(t *testing.T) {
		srv, _ := testServer(t)
		handle, resolved := srv.resolveReincarnationRequesterName(context.Background(), "no-such-principal-id")
		assert.Equal(t, reincarnationRequesterFallback, handle)
		assert.False(t, resolved)
	})

	t.Run("a GetUser store error still falls through to GetAgent", func(t *testing.T) {
		disp := newReincarnateTestDispatcher()
		srv, s, project, broker := setupReincarnateTestServer(t, disp)
		agent := newReincarnateTestAgent(t, s, project, broker, nil)

		// Swap in a store whose GetUser always errors (not ErrNotFound), to
		// prove the fall-through path is taken on a genuine store error and
		// not just on a clean miss. GetAgent still succeeds via the real
		// store, so this proves the function falls through to the agent
		// lookup after a non-NotFound GetUser error, rather than a store
		// error short-circuiting resolution the way a NotFound is expected
		// to. This is fall-through, not fallback — see the next case for an
		// actual fallback via a store error.
		srv.store = &reincarnateGetUserErrorStore{Store: s, err: fmt.Errorf("injected store failure")}

		handle, resolved := srv.resolveReincarnationRequesterName(context.Background(), agent.ID)
		assert.Equal(t, "agent:"+agent.Slug, handle,
			"a GetUser store error must fall through to GetAgent, not abandon resolution")
		assert.True(t, resolved)
	})

	t.Run("a GetUser store error on a real user ID ends in the fallback", func(t *testing.T) {
		srv, s := testServer(t)
		user := &store.User{
			ID:      tid("a261-storeerr-user"),
			Email:   "storeerr@example.com",
			Role:    store.UserRoleMember,
			Status:  store.UserStatusActive,
			Created: time.Now(),
		}
		require.NoError(t, s.CreateUser(context.Background(), user))

		// This ID genuinely resolves via GetUser under an unwrapped store —
		// the previous case proves fall-through works when GetAgent would
		// have hit. Here GetUser errors (masking the real user) and GetAgent
		// then genuinely misses (this ID was never an agent ID), so
		// resolution must land on the fallback, not silently succeed via
		// some other path.
		srv.store = &reincarnateGetUserErrorStore{Store: s, err: fmt.Errorf("injected store failure")}

		handle, resolved := srv.resolveReincarnationRequesterName(context.Background(), user.ID)
		assert.Equal(t, reincarnationRequesterFallback, handle,
			"a GetUser store error masking a real user, with GetAgent also missing, must end in the fallback")
		assert.False(t, resolved)
	})
}

// reincarnateGetUserErrorStore wraps a real store.Store and forces GetUser to
// return a non-ErrNotFound error for every call, so
// TestResolveReincarnationRequesterName_A26_1's store-error cases exercise a
// genuine store failure rather than a clean "not found" miss.
type reincarnateGetUserErrorStore struct {
	store.Store
	err error
}

func (e *reincarnateGetUserErrorStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	return nil, e.err
}

// TestBuildReincarnationRequesterContext_A262_R1 is the design Amendment
// A26.2 R1 unit test for the self-request detection and creator-hint
// resolution that buildReincarnationRequesterContext performs before the
// preamble is built — decoupled from the full worker so the decision logic
// itself (not just the end-to-end wiring, covered separately by
// TestReincarnateAgent_AC6_NonCreatorRequesterGetsNotifiedOnFailure and
// TestReincarnateAgent_SelfRequest_PreambleNeverEmitsOwnHandle) has a direct
// test.
func TestBuildReincarnationRequesterContext_A262_R1(t *testing.T) {
	t.Run("non-self: passes through to the requester resolver", func(t *testing.T) {
		srv, s := testServer(t)
		user := &store.User{
			ID: tid("a262-nonself-user"), Email: "nonself@example.com",
			Role: store.UserRoleMember, Status: store.UserStatusActive, Created: time.Now(),
		}
		require.NoError(t, s.CreateUser(context.Background(), user))
		agent := &store.Agent{ID: "agent-nonself", Slug: "nonself-agt"}

		got := srv.buildReincarnationRequesterContext(context.Background(), agent, user.ID)
		assert.False(t, got.IsSelf)
		assert.Equal(t, "user:nonself@example.com", got.Handle)
		assert.True(t, got.Resolved)
	})

	t.Run("self: never resolves or emits the agent's own handle, no CreatedBy", func(t *testing.T) {
		srv, _ := testServer(t)
		agent := &store.Agent{ID: "agent-self-1", Slug: "self-agt-1"}

		got := srv.buildReincarnationRequesterContext(context.Background(), agent, agent.ID)
		assert.True(t, got.IsSelf)
		assert.Empty(t, got.Handle)
		assert.False(t, got.Resolved)
		assert.False(t, got.CreatorResolved)
	})

	t.Run("self: resolves a distinct creator as a hint", func(t *testing.T) {
		srv, s := testServer(t)
		creator := &store.User{
			ID: tid("a262-creator-user"), Email: "creator@example.com",
			Role: store.UserRoleMember, Status: store.UserStatusActive, Created: time.Now(),
		}
		require.NoError(t, s.CreateUser(context.Background(), creator))
		agent := &store.Agent{ID: "agent-self-2", Slug: "self-agt-2", CreatedBy: creator.ID}

		got := srv.buildReincarnationRequesterContext(context.Background(), agent, agent.ID)
		assert.True(t, got.IsSelf)
		assert.False(t, got.Resolved, "self-request must never populate Handle/Resolved")
		assert.True(t, got.CreatorResolved)
		assert.Equal(t, "user:creator@example.com", got.CreatorHandle)
	})

	t.Run("self: an unresolvable CreatedBy yields no creator hint", func(t *testing.T) {
		srv, _ := testServer(t)
		agent := &store.Agent{ID: "agent-self-3", Slug: "self-agt-3", CreatedBy: "no-such-principal"}

		got := srv.buildReincarnationRequesterContext(context.Background(), agent, agent.ID)
		assert.True(t, got.IsSelf)
		assert.False(t, got.CreatorResolved)
		assert.Empty(t, got.CreatorHandle)
	})

	t.Run("self: the defensive guard drops a creator that resolves back to the agent itself", func(t *testing.T) {
		// A contrived but directly-testable shape for the "never emit the
		// agent's own handle" rule (A26.2 R1): CreatedBy set to the agent's
		// own ID, so GetAgent(CreatedBy) resolves to the agent itself. Uses
		// the full project/broker fixtures since GetAgent needs a real,
		// persisted row to resolve against.
		disp := newReincarnateTestDispatcher()
		srv, s, project, broker := setupReincarnateTestServer(t, disp)
		agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
			a.CreatedBy = a.ID
		})

		got := srv.buildReincarnationRequesterContext(context.Background(), agent, agent.ID)
		assert.True(t, got.IsSelf)
		assert.False(t, got.CreatorResolved, "a creator that resolves to the agent's own handle must never surface as a hint")
		assert.Empty(t, got.CreatorHandle)
	})
}

// blockingUntilDoneStore wraps a real store.Store and makes GetUser/GetAgent
// block until the ctx passed to them is done, then return its error. Used by
// TestBuildReincarnationRequesterContext_A2614_TimeoutBound to simulate a
// stuck backend: the real resolution path is otherwise exercised, only the
// store round trip is replaced.
type blockingUntilDoneStore struct {
	store.Store
}

func (b *blockingUntilDoneStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (b *blockingUntilDoneStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestBuildReincarnationRequesterContext_A2614_TimeoutBound is the design
// Amendment A26.14/A26.18 test: an unbounded store call during requester
// resolution must not stall the worker, on either resolution path — non-self
// (the requester handle) or self (the creator hint). With this Server's
// reincarnationRequesterResolveTimeout field shortened and a store whose
// GetUser/GetAgent block until their ctx is done, each subtest's build must
// still return, with the unresolved fallback, within its own bounded wait
// (a select with a generous time.After), independent of the production
// timeout: if the production timeout were removed, the blocking store's
// calls would hang on context.Background() forever, and the subtest must
// fail promptly rather than hang the suite. The field is set on this test's
// own Server instance only, so no t.Cleanup restore is needed and no other
// test's workers can observe it.
func TestBuildReincarnationRequesterContext_A2614_TimeoutBound(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)

	srv.reincarnationRequesterResolveTimeout = 50 * time.Millisecond

	srv.store = &blockingUntilDoneStore{Store: s}

	t.Run("non-self", func(t *testing.T) {
		done := make(chan reincarnationRequesterContext, 1)
		go func() {
			done <- srv.buildReincarnationRequesterContext(context.Background(), agent, "some-other-requester-id")
		}()

		select {
		case got := <-done:
			assert.False(t, got.IsSelf)
			assert.False(t, got.Resolved)
			assert.Equal(t, reincarnationRequesterFallback, got.Handle)
		case <-time.After(2 * time.Second):
			t.Fatal("buildReincarnationRequesterContext did not return within the test's own bounded wait — the production timeout did not fire (or was removed)")
		}
	})

	t.Run("self", func(t *testing.T) {
		done := make(chan reincarnationRequesterContext, 1)
		go func() {
			done <- srv.buildReincarnationRequesterContext(context.Background(), agent, agent.ID)
		}()

		select {
		case got := <-done:
			assert.True(t, got.IsSelf)
			assert.False(t, got.CreatorResolved)
			assert.Empty(t, got.CreatorHandle)
		case <-time.After(2 * time.Second):
			t.Fatal("buildReincarnationRequesterContext did not return within the test's own bounded wait — the production timeout did not fire (or was removed)")
		}
	})
}

// AC-6: a start failure leaves state=failed with an error and phase=error,
// and the previous config snapshot remains retrievable.
func TestReincarnateAgent_AC6_StartFailureMarksFailed(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	disp.startErr = fmt.Errorf("no such image: nonexistent:latest")
	// This start error is ambiguous (not a confirmed broker refusal), so no
	// re-render of `previous` runs (ptone/scion#1935) and the row keeps the
	// fresh config. TestReincarnateAgent_StartFailure_RerendersPreviousConfig
	// covers a definitive start failure.
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)

	req := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"})
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, req, agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	waitForReincarnationSettled(t, s, agent.ID)

	final, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "error", final.Phase)
	assert.Equal(t, store.ReincarnationStateFailed, final.ReincarnationState)
	assert.Equal(t, 1, final.Generation, "generation must not increment on failure")
	assert.Contains(t, final.Message, "no such image")
	// Design §3.4 Amendment A4.3: a start failure happens after reprovision
	// already succeeded (disk is gen N+1), so AppliedConfig must NOT be
	// restored to `previous` — it must keep the freshly rendered config,
	// identifiable by the reincarnation preamble this worker stamped onto
	// Task right before dispatching reprovision.
	assert.Contains(t, final.AppliedConfig.Task, "[SCION REINCARNATION]",
		"a post-reprovision-success failure (start) must keep the fresh AppliedConfig, not restore `previous`")

	list, err := s.ListAgentReincarnations(context.Background(), agent.ID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, store.AgentReincarnationStateFailed, list[0].State)
	assert.Contains(t, list[0].Error, "no such image")
	require.NotNil(t, list[0].PreviousAppliedConfig, "previous config snapshot must remain retrievable")
	assert.Equal(t, "old-image:v1", list[0].PreviousAppliedConfig.Image)
}

// TestReincarnateAgent_ReprovisionDispatchFailure_RestoresAppliedConfig is the
// design §3.4 Amendment A4.3 regression test for the other half of the
// restore decision: a reprovision *dispatch* failure happens after the
// worker has already written the fresh AppliedConfig to the store (so a
// concurrent reader would see gen N+1's config) but the broker never
// actually re-rendered the disk. failReincarnation must restore `previous`
// here, or the store would claim a generation that was never really
// provisioned. Contrast with TestReincarnateAgent_AC6_StartFailureMarksFailed,
// which fails one step later (after reprovision succeeded) and must NOT
// restore.
func TestReincarnateAgent_ReprovisionDispatchFailure_RestoresAppliedConfig(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	disp.reprovisionErr = fmt.Errorf("broker refused: reprovision refused: container is still running")
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)

	req := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"})
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, req, agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	waitForReincarnationSettled(t, s, agent.ID)

	final, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "error", final.Phase)
	assert.Equal(t, store.ReincarnationStateFailed, final.ReincarnationState)
	assert.Equal(t, 1, final.Generation, "generation must not increment when reprovision dispatch fails")
	assert.Contains(t, final.Message, "reprovision refused")
	assert.Equal(t, "old-image:v1", final.AppliedConfig.Image,
		"AppliedConfig must be restored to `previous` when reprovision dispatch fails")
	assert.NotContains(t, final.AppliedConfig.Task, "[SCION REINCARNATION]",
		"a pre-reprovision-success failure must restore `previous`, not keep the fresh, never-rendered config")

	assert.Equal(t, 2, disp.reprovisionCalls, "the failed reprovision plus the best-effort re-render of `previous` (ptone/scion#1935)")
	assert.Zero(t, disp.startCalls, "start must not run when reprovision dispatch fails")

	list, err := s.ListAgentReincarnations(context.Background(), agent.ID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, store.AgentReincarnationStateFailed, list[0].State)
	assert.Contains(t, list[0].Error, "reprovision refused")
}

// TestReincarnateAgent_ReprovisionFailure_RerendersPreviousConfig covers
// ptone/scion#1935 option (c): a failed reprovision dispatch is followed by
// one best-effort reprovision with the previous applied config, and the row
// is rolled back to previous.
func TestReincarnateAgent_ReprovisionFailure_RerendersPreviousConfig(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	disp.reprovisionErr = fmt.Errorf("request timeout after 2m0s")
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	r := waitForReincarnationSettled(t, s, agent.ID)
	require.Equal(t, store.AgentReincarnationStateFailed, r.State)
	assert.Contains(t, r.Error, "reprovision failed: request timeout")

	calls, cfgs := disp.reprovisionSnapshot()
	require.Equal(t, 2, calls, "one reprovision with the fresh config, one re-render with the previous config")
	require.Len(t, cfgs, 2)
	assert.Contains(t, cfgs[0].Task, "[SCION REINCARNATION]", "the first dispatch carries the fresh config")
	require.NotNil(t, r.PreviousAppliedConfig)
	assert.Equal(t, *r.PreviousAppliedConfig, cfgs[1], "the re-render dispatches exactly the previous config")
	assert.Zero(t, disp.startCalls)

	final, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ReincarnationStateFailed, final.ReincarnationState)
	assert.Equal(t, "old-image:v1", final.AppliedConfig.Image, "the row is rolled back to previous")
	assert.NotContains(t, final.AppliedConfig.Task, "[SCION REINCARNATION]")
}

// TestReincarnateAgent_StartFailure_RerendersPreviousConfig covers
// ptone/scion#1935 option (c) after a successful reprovision: the start
// fails, the previous config is re-rendered, and since the re-render
// succeeded (the disk is back at gen N) the row is restored to previous too.
func TestReincarnateAgent_StartFailure_RerendersPreviousConfig(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	disp.startErr = definitiveStartErr()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	r := waitForReincarnationSettled(t, s, agent.ID)
	require.Equal(t, store.AgentReincarnationStateFailed, r.State)
	assert.Contains(t, r.Error, "start failed: runtime broker returned error 409")

	calls, cfgs := disp.reprovisionSnapshot()
	require.Equal(t, 2, calls)
	require.NotNil(t, r.PreviousAppliedConfig)
	assert.Equal(t, *r.PreviousAppliedConfig, cfgs[1], "the re-render dispatches exactly the previous config")

	final, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, final.Generation)
	assert.Equal(t, "old-image:v1", final.AppliedConfig.Image, "a successful re-render restores previous on the row")
	assert.NotContains(t, final.AppliedConfig.Task, "[SCION REINCARNATION]")
}

// TestReincarnateAgent_RerenderFailure_KeepsOriginalError covers
// ptone/scion#1935 option (c) when the re-render itself fails: no panic, one
// re-render attempt only (no retry loop), the reincarnation still fails with
// the ORIGINAL error, and the row keeps the gen N+1 config the disk holds.
func TestReincarnateAgent_RerenderFailure_KeepsOriginalError(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	disp.startErr = definitiveStartErr()
	disp.rerenderErr = fmt.Errorf("reprovision refused: container is still running")
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	r := waitForReincarnationSettled(t, s, agent.ID)
	require.Equal(t, store.AgentReincarnationStateFailed, r.State)
	assert.Contains(t, r.Error, "start failed: runtime broker returned error 409")
	assert.NotContains(t, r.Error, "container is still running", "the re-render failure must not mask the original error")
	assert.NotContains(t, r.Error, "worker panic")

	calls, _ := disp.reprovisionSnapshot()
	assert.Equal(t, 2, calls, "exactly one re-render attempt")

	final, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "error", final.Phase)
	assert.Equal(t, store.ReincarnationStateFailed, final.ReincarnationState)
	assert.Contains(t, final.Message, "runtime broker returned error 409")
	assert.Contains(t, final.AppliedConfig.Task, "[SCION REINCARNATION]", "a failed re-render leaves the row at gen N+1, matching the disk")
}

// definitiveStartErr is a start failure the broker itself answered with its
// error envelope and no start-attempted marker: the start was refused before
// any container was created (reincarnationStartLeftNoContainer).
func definitiveStartErr() error {
	return &brokerStatusError{StatusCode: http.StatusConflict, Body: `{"error":{"code":"start_refused","message":"start refused"}}`}
}

// TestReincarnationStartLeftNoContainer pins the classification the
// start-failure re-render uses (ptone/scion#1935).
func TestReincarnationStartLeftNoContainer(t *testing.T) {
	assert.True(t, reincarnationStartLeftNoContainer(definitiveStartErr()), "a broker refusal is definitive")
	assert.True(t, reincarnationStartLeftNoContainer(fmt.Errorf("wrap: %w", errStartBrokerNotConnected)), "a request that never reached the broker is definitive")
	assert.False(t, reincarnationStartLeftNoContainer(fmt.Errorf("request timeout after 2m0s")), "a timeout is ambiguous")
	assert.False(t, reincarnationStartLeftNoContainer(&brokerStatusError{StatusCode: http.StatusGatewayTimeout, Body: "gateway timeout"}), "a proxy error is ambiguous")
	assert.False(t, reincarnationStartLeftNoContainer(brokerEnvelope(t, http.StatusInternalServerError, "runtime_error", startAttempted("run-1"))),
		"a failure from inside Manager.Start may have created a container")
	assert.False(t, reincarnationStartLeftNoContainer(nil))
}

// TestReincarnateAgent_AmbiguousStartFailure_NoRerender covers
// ptone/scion#1935: a start failure whose outcome is ambiguous
// (here a transport timeout: a gen N+1 container may be running) makes no
// re-render dispatch, so the credential that container would use is not
// revoked, and the row stays at gen N+1. The re-render would have been
// refused (and revoked) had it run.
func TestReincarnateAgent_AmbiguousStartFailure_NoRerender(t *testing.T) {
	srv, s, agent, mock, gen, calls := setupReincarnateCredentialTest(t, fmt.Errorf("request timeout after 2m0s"))
	self := agentIdentityFor(agent.ID, agent.ProjectID)

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	r := waitForReincarnationSettled(t, s, agent.ID)
	require.Equal(t, store.AgentReincarnationStateFailed, r.State)
	assert.Contains(t, r.Error, "start failed: request timeout")

	assert.Equal(t, int32(1), calls.Load(), "no re-render dispatch after an ambiguous start failure")
	require.True(t, mock.lastCreateReq.Reprovision)
	require.Len(t, gen.jtis, 1, "only the reprovision minted a credential")
	assert.Nil(t, getTestAgentCredential(t, s, gen.jtis[0]).RevokedAt, "a credential a possibly running container uses must not be revoked")

	final, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Contains(t, final.AppliedConfig.Task, "[SCION REINCARNATION]", "the row stays at gen N+1, matching the disk and any container")
}

// TestReincarnateAgent_DefinitiveStartFailure_RerenderRefusedRevokes covers
// ptone/scion#1935: after a definitive start failure (the broker
// refused it; no container exists) the re-render is dispatched. When that
// re-render fails, dispatchProvision's create-failed revoke revokes the
// agent's credentials by agent: every credential, including the one the
// successful reprovision minted. That is the accepted outcome: no container
// is using them, and the agent is failed and must be started again (which
// mints afresh). The row stays at gen N+1.
func TestReincarnateAgent_DefinitiveStartFailure_RerenderRefusedRevokes(t *testing.T) {
	srv, s, agent, _, gen, calls := setupReincarnateCredentialTest(t, definitiveStartErr())
	self := agentIdentityFor(agent.ID, agent.ProjectID)

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	r := waitForReincarnationSettled(t, s, agent.ID)
	require.Equal(t, store.AgentReincarnationStateFailed, r.State)
	assert.Contains(t, r.Error, "start failed: runtime broker returned error 409")
	assert.NotContains(t, r.Error, "reprovision_refused", "the re-render failure must not mask the original error")

	assert.Equal(t, int32(2), calls.Load(), "the re-render is dispatched after a definitive start failure")
	require.Len(t, gen.jtis, 2, "the reprovision and the re-render each minted a credential")
	for _, jti := range gen.jtis {
		cred := getTestAgentCredential(t, s, jti)
		require.NotNil(t, cred.RevokedAt, "a refused re-render revokes the agent's credentials by agent")
		require.NotNil(t, cred.RevokeReason)
		assert.Equal(t, agentCredentialRevokeReasonCreateFailed, *cred.RevokeReason)
	}

	final, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Contains(t, final.AppliedConfig.Task, "[SCION REINCARNATION]", "a failed re-render leaves the row at gen N+1")
}

// setupReincarnateCredentialTest wires reprovision through the real
// HTTPAgentDispatcher with a minting token generator, against a mock broker
// whose first reprovision succeeds and every later one (the re-render) is
// refused. The start is the fake's and fails with startErr. calls counts
// reprovision round trips.
func setupReincarnateCredentialTest(t *testing.T, startErr error) (*Server, store.Store, *store.Agent, *mockRuntimeBrokerClient, *fakeMintingTokenGenerator, *atomic.Int32) {
	t.Helper()
	ctx := context.Background()
	fake := newReincarnateTestDispatcher()
	fake.startErr = startErr
	disp := &httpReprovisionDispatcher{reincarnateTestDispatcher: fake}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	broker.Endpoint = "http://localhost:9800"
	require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))

	calls := &atomic.Int32{}
	mock := &mockRuntimeBrokerClient{}
	mock.createWithGatherFunc = func(_ context.Context, _, _ string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
		if calls.Add(1) > 1 {
			return nil, nil, &brokerStatusError{StatusCode: http.StatusConflict, Body: `{"error":{"code":"reprovision_refused","message":"container is still running"}}`}
		}
		return &RemoteAgentResponse{
			Agent:         &RemoteAgentInfo{ID: req.ID, Slug: req.Slug, Name: req.Name, Phase: "created"},
			Created:       true,
			Reprovisioned: req.Reprovision,
		}, nil, nil
	}
	disp.http = NewHTTPAgentDispatcherWithClient(s, mock, false, slog.Default())
	gen := &fakeMintingTokenGenerator{store: s}
	disp.http.SetTokenGenerator(gen)

	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	return srv, s, agent, mock, gen, calls
}

// TestReincarnateAgent_RerenderEchoDoesNotLeak covers ptone/scion#1935: the
// re-render dispatches a copy of previous, so the broker's echo on that
// dispatch reaches neither the record's PreviousAppliedConfig nor the
// restored row.
func TestReincarnateAgent_RerenderEchoDoesNotLeak(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	disp.startErr = definitiveStartErr()
	disp.rerenderEcho = func(cfg *store.AgentAppliedConfig) {
		cfg.Image = "rerender-echo:v9"
		cfg.HarnessConfig = "rerender-echo-hc"
		cfg.Profile = "rerender-echo-profile"
	}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	r := waitForReincarnationSettled(t, s, agent.ID)
	require.Equal(t, store.AgentReincarnationStateFailed, r.State)

	calls, _ := disp.reprovisionSnapshot()
	require.Equal(t, 2, calls)
	final, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	require.NotNil(t, r.PreviousAppliedConfig)
	for name, cfg := range map[string]*store.AgentAppliedConfig{"record previous": r.PreviousAppliedConfig, "restored row": final.AppliedConfig} {
		assert.Equal(t, "old-image:v1", cfg.Image, name)
		assert.Empty(t, cfg.HarnessConfig, name)
		assert.Empty(t, cfg.Profile, name)
	}
}

// failAdvanceToStartingStore fails every provisioning -> starting record
// CAS with an error.
type failAdvanceToStartingStore struct {
	store.Store
}

func (f *failAdvanceToStartingStore) TryAdvanceAgentReincarnation(ctx context.Context, rec *store.AgentReincarnation, fromState string, cutoff time.Time) (bool, error) {
	if fromState == store.AgentReincarnationStateProvisioning && rec.State == store.AgentReincarnationStateStarting {
		return false, fmt.Errorf("injected advance failure")
	}
	return f.Store.TryAdvanceAgentReincarnation(ctx, rec, fromState, cutoff)
}

// TestReincarnateAgent_AdvanceToStartingFailure_RerendersPreviousConfig
// covers ptone/scion#1935: a record CAS error after a
// successful reprovision (no start dispatched, so no container) re-renders
// previous and, since that succeeded, restores the row.
func TestReincarnateAgent_AdvanceToStartingFailure_RerendersPreviousConfig(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)

	// Installed before the request and never restored: the worker goroutine
	// reads srv.store and can outlive waitForReincarnationSettled, so a
	// restore write would race it (ptone/scion#3184).
	installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *failAdvanceToStartingStore {
		return &failAdvanceToStartingStore{Store: inner}
	})
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	r := waitForReincarnationSettled(t, s, agent.ID)
	require.Equal(t, store.AgentReincarnationStateFailed, r.State)
	assert.Contains(t, r.Error, "failed to advance record to starting")

	calls, cfgs := disp.reprovisionSnapshot()
	require.Equal(t, 2, calls, "the reprovision plus one re-render")
	require.NotNil(t, r.PreviousAppliedConfig)
	assert.Equal(t, *r.PreviousAppliedConfig, cfgs[1])
	assert.Zero(t, disp.startCalls)

	final, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "old-image:v1", final.AppliedConfig.Image, "a successful re-render restores previous on the row")
	assert.NotContains(t, final.AppliedConfig.Task, "[SCION REINCARNATION]")
}

// failStartingWriteStore fails every agent row write that records the
// reincarnation's starting step.
type failStartingWriteStore struct {
	store.Store
}

func (f *failStartingWriteStore) UpdateAgent(ctx context.Context, a *store.Agent) error {
	if a.ReincarnationState == store.ReincarnationStateStarting {
		return fmt.Errorf("injected starting-step write failure")
	}
	return f.Store.UpdateAgent(ctx, a)
}

// TestReincarnateAgent_StartingWriteFailure_RerendersPreviousConfig covers
// ptone/scion#1935: the starting-step row write fails after
// a successful reprovision (no start dispatched, so no container), so
// previous is re-rendered and, since that succeeded, the row is restored.
func TestReincarnateAgent_StartingWriteFailure_RerendersPreviousConfig(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)

	// Installed before the request and never restored: the worker goroutine
	// reads srv.store and can outlive waitForReincarnationSettled, so a
	// restore write would race it (ptone/scion#3184).
	installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *failStartingWriteStore {
		return &failStartingWriteStore{Store: inner}
	})
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	r := waitForReincarnationSettled(t, s, agent.ID)
	require.Equal(t, store.AgentReincarnationStateFailed, r.State)
	assert.Contains(t, r.Error, "failed to record starting state")

	calls, cfgs := disp.reprovisionSnapshot()
	require.Equal(t, 2, calls, "the reprovision plus one re-render")
	require.NotNil(t, r.PreviousAppliedConfig)
	assert.Equal(t, *r.PreviousAppliedConfig, cfgs[1])
	assert.Zero(t, disp.startCalls, "no start is dispatched when the starting step cannot be recorded")

	final, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ReincarnationStateFailed, final.ReincarnationState)
	assert.Equal(t, "old-image:v1", final.AppliedConfig.Image, "a successful re-render restores previous on the row")
	assert.NotContains(t, final.AppliedConfig.Task, "[SCION REINCARNATION]")
}

// TestReincarnateAgent_ReprovisionAndRerenderFailure_StillRestoresRow covers
// ptone/scion#1935: after a failed reprovision the row is
// restored to previous whatever the re-render's outcome, including when the
// re-render fails too.
func TestReincarnateAgent_ReprovisionAndRerenderFailure_StillRestoresRow(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	disp.reprovisionErr = fmt.Errorf("request timeout after 2m0s")
	disp.rerenderErr = fmt.Errorf("broker unreachable")
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	r := waitForReincarnationSettled(t, s, agent.ID)
	require.Equal(t, store.AgentReincarnationStateFailed, r.State)
	assert.Contains(t, r.Error, "reprovision failed: request timeout")
	assert.NotContains(t, r.Error, "broker unreachable")

	calls, _ := disp.reprovisionSnapshot()
	assert.Equal(t, 2, calls)
	final, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "old-image:v1", final.AppliedConfig.Image, "the row is restored to previous even when the re-render fails")
	assert.NotContains(t, final.AppliedConfig.Task, "[SCION REINCARNATION]")
}

// TestReincarnateAgent_FailureBeforeReprovision_NoRerender covers
// ptone/scion#1935 option (c)'s precondition: a failure before any
// reprovision dispatch (here, the stop) makes no re-render dispatch.
func TestReincarnateAgent_FailureBeforeReprovision_NoRerender(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	disp.stopErr = fmt.Errorf("broker unreachable")
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	r := waitForReincarnationSettled(t, s, agent.ID)
	require.Equal(t, store.AgentReincarnationStateFailed, r.State)

	calls, _ := disp.reprovisionSnapshot()
	assert.Zero(t, calls, "no reprovision, and so no re-render, before the reprovision step")
}

// TestReincarnateAgent_AC6_NonCreatorRequesterGetsNotifiedOnFailure is the
// design §3.4 Amendment A3.8 regression test: a requester who is
// not the agent itself and not already subscribed (a coordinator, not the
// creator) gets a notification subscription at request time, and actually
// receives the failure notification through the ordinary
// subscription-dispatch path once the reincarnation fails.
func TestReincarnateAgent_AC6_NonCreatorRequesterGetsNotifiedOnFailure(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	disp.startErr = fmt.Errorf("no such image: nonexistent:latest")
	srv, s, project, broker := setupReincarnateTestServer(t, disp)

	// Wire a real notification dispatcher, same pattern as
	// setupIntegrationTest: replace the event publisher, then build and
	// start a dispatcher against it. setupReincarnateTestServer's dispatcher
	// fake (disp) doubles as the AgentDispatcher the notification dispatcher
	// uses to deliver agent-to-agent messages.
	pub := NewChannelEventPublisher()
	srv.SetEventPublisher(pub)
	t.Cleanup(pub.Close)
	nd := NewNotificationDispatcher(s, pub, func() AgentDispatcher { return disp }, slog.Default())
	nd.Start()
	t.Cleanup(nd.Stop)

	agent := newReincarnateTestAgent(t, s, project, broker, nil)

	// A coordinator agent distinct from both the target and its creator
	// (tid("user-creator")), with no existing subscription.
	coordinator := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.ID = tid("coordinator-" + t.Name())
		a.Slug = "coordinator-" + tidSlugSafe(t.Name())
		a.Name = "Coordinator"
	})
	requester := delegatingRequesterFor(coordinator.ID, project.ID)

	req := reincarnateRequest(t, agent.ID, requester, ReincarnateAgentRequest{Handoff: "h"})
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, req, agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	subs, err := s.GetNotificationSubscriptions(context.Background(), agent.ID)
	require.NoError(t, err)
	found := false
	for _, sub := range subs {
		if sub.SubscriberType == store.SubscriberTypeAgent && sub.SubscriberID == coordinator.Slug {
			found = true
		}
	}
	assert.True(t, found, "non-creator requester must be subscribed to the agent's notifications")

	waitForReincarnationSettled(t, s, agent.ID)

	final, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "error", final.Phase)

	// The worker→preamble wiring needs its own end-to-end assertion: the
	// resolved handle must actually reach the dispatched preamble, not just
	// be provable at the resolver/builder unit level. The failure here
	// happens at the start step, after reprovision already succeeded, so
	// AppliedConfig.Task still holds the fresh preamble (design §3.4
	// Amendment A4.3).
	assert.Contains(t, final.AppliedConfig.Task,
		fmt.Sprintf("Message agent:%s that generation", coordinator.Slug),
		"the resolved requester handle must reach the dispatched preamble text")

	require.Eventually(t, func() bool {
		notifs, err := s.GetNotifications(context.Background(), store.SubscriberTypeAgent, coordinator.Slug, false)
		return err == nil && len(notifs) > 0
	}, 2*time.Second, 50*time.Millisecond,
		"the non-creator requester must actually receive a failure notification")
}

// TestReincarnateAgent_SelfRequest_PreambleNeverEmitsOwnHandle is the design
// Amendment A26.2 R1 end-to-end regression test: self-migration is the
// AC-10/2c dogfood path, and the preamble must never resolve the agent's own
// ID back to its own handle, which would tell the new generation to message
// itself. Drives the full worker (via a start failure, so
// AppliedConfig.Task still holds the fresh preamble per Amendment A4.3) and
// asserts the dispatched preamble never contains the agent's own handle.
func TestReincarnateAgent_SelfRequest_PreambleNeverEmitsOwnHandle(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	disp.startErr = fmt.Errorf("no such image: nonexistent:latest")
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)

	req := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"})
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, req, agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	waitForReincarnationSettled(t, s, agent.ID)

	final, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "error", final.Phase)

	task := final.AppliedConfig.Task
	assert.Contains(t, task, "reincarnated at its own request",
		"a self-migration's header must say so, not name a requester")
	assert.Contains(t, task, `Message the owner named in your handoff's "Authority and ownership" section`,
		"step 3 must point at the handoff, not the agent's own identity")
	assert.NotContains(t, task, "agent:"+agent.Slug,
		"the preamble must never tell the new generation to message its own handle (A26.2 R1)")
}

// TestReincarnateAgent_ChangesLineWiring is the design Amendment A26.4 R1
// worker end-to-end test for the preamble's "Changes:" line: it must reflect
// the actual old->new diff between the outgoing and incoming generation —
// not a self-diff, not the arguments swapped, and not an empty plan — and it
// must be absent entirely when nothing changed. Reuses the
// template/image-bump fixture from TestReincarnateAgent_DryRun_ShowsDiffAndWritesNothing.
func TestReincarnateAgent_ChangesLineWiring(t *testing.T) {
	t.Run("changed: the exact line reaches AppliedConfig.Task", func(t *testing.T) {
		disp := newReincarnateTestDispatcher()
		disp.startErr = fmt.Errorf("no such image: nonexistent:latest")
		srv, s, project, broker := setupReincarnateTestServer(t, disp)

		template := &store.Template{
			ID:          tid("tmpl-" + t.Name()),
			Name:        "t",
			Slug:        "reincarnate-template-" + tidSlugSafe(t.Name()),
			Harness:     "claude",
			Scope:       store.TemplateScopeGlobal,
			Status:      store.TemplateStatusActive,
			ContentHash: "new-template-hash",
			Config:      &store.TemplateConfig{Image: "template-image:v2"},
		}
		require.NoError(t, s.CreateTemplate(context.Background(), template))

		agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
			a.Template = template.Slug
			a.AppliedConfig.TemplateHash = "old-template-hash"
			// No explicit image in CreateInputs -> template image applies fresh.
		})
		self := agentIdentityFor(agent.ID, project.ID)

		req := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"})
		rec := httptest.NewRecorder()
		srv.handleReincarnateAgent(rec, req, agent.ID)
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

		waitForReincarnationSettled(t, s, agent.ID)

		final, err := s.GetAgent(context.Background(), agent.ID)
		require.NoError(t, err)
		assert.Equal(t, "error", final.Phase)

		assert.Contains(t, final.AppliedConfig.Task,
			"Changes: template old-template-hash->new-template-hash, image old-image:v1->template-image:v2\n",
			"the Changes: line must reflect the actual old->new diff, not a self-diff or swapped arguments")

		// Amendment A26.5 O1: pin the by-construction property Amendment
		// A26.4 O1 relies on directly, rather than only via a hard-coded
		// expected string that a future reintroduced worker-side recompute
		// could coincidentally still satisfy — decode the 202 body's own
		// plan and assert the preamble's line is derived from that exact
		// value.
		var resp ReincarnateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		want := reincarnationChangesLine(resp.Plan)
		// Amendment A26.6 R1: reincarnationChangesLine returns "" for a plan
		// with no diffs, and assert.Contains(x, "") is always true — so
		// without this, the assertion below would pass vacuously if the 202
		// body ever lost or zeroed its plan (a response-side regression),
		// even though the by-construction property this test exists to pin
		// would then be false.
		require.NotEmpty(t, want, "the 202 body must carry the non-empty plan the worker rendered")
		assert.Contains(t, final.AppliedConfig.Task, want,
			"the preamble's Changes: line must be exactly reincarnationChangesLine(<the 202 body's plan>)")
	})

	t.Run("unchanged: no Changes: line at all", func(t *testing.T) {
		disp := newReincarnateTestDispatcher()
		disp.startErr = fmt.Errorf("no such image: nonexistent:latest")
		srv, s, project, broker := setupReincarnateTestServer(t, disp)

		template := &store.Template{
			ID:          tid("tmpl-unchanged-" + t.Name()),
			Name:        "t",
			Slug:        "reincarnate-template-unchanged-" + tidSlugSafe(t.Name()),
			Harness:     "claude",
			Scope:       store.TemplateScopeGlobal,
			Status:      store.TemplateStatusActive,
			ContentHash: "same-template-hash",
			Config:      &store.TemplateConfig{Image: "old-image:v1"},
		}
		require.NoError(t, s.CreateTemplate(context.Background(), template))

		agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
			a.Template = template.Slug
			a.AppliedConfig.TemplateHash = "same-template-hash"
			a.AppliedConfig.Image = "old-image:v1"
		})
		self := agentIdentityFor(agent.ID, project.ID)

		req := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"})
		rec := httptest.NewRecorder()
		srv.handleReincarnateAgent(rec, req, agent.ID)
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

		waitForReincarnationSettled(t, s, agent.ID)

		final, err := s.GetAgent(context.Background(), agent.ID)
		require.NoError(t, err)
		assert.Equal(t, "error", final.Phase)

		assert.NotContains(t, final.AppliedConfig.Task, "Changes:")
	})
}

// TestReincarnateAgent_MigratingMessageOwnership is the design Amendment
// A26.8 worker end-to-end test: since a `sciontool status blocked` POST
// from the CLI is unconditionally discarded by Guard 0b
// (handlers_agent_lifecycle.go, reincarnationInFlight) for the whole
// in-flight window, the reincarnation worker itself is the only thing that
// can make "migrating to generation N" observable on the agent row. It must
// be set at the very first step write (stopping) and cleared again once the
// migration completes.
func TestReincarnateAgent_MigratingMessageOwnership(t *testing.T) {
	base := newReincarnateTestDispatcher()
	disp := &blockingStopDispatcher{reincarnateTestDispatcher: base, entered: make(chan struct{}), release: make(chan struct{})}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	<-disp.entered // worker is inside Stop; the stopping step's write has already landed

	midFlight, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "migrating to generation 2", midFlight.Message,
		"the worker, not the CLI, must own the in-flight status message (A26.8)")

	close(disp.release)
	waitForReincarnationSettled(t, s, agent.ID)

	final, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ReincarnationStateNone, final.ReincarnationState)
	assert.Equal(t, "", final.Message, "completion must clear the migrating message")
}

// TestReincarnateAgent_MigratingMessagePreservedIfNewGenAlreadySetOne is the
// design Amendment A26.8 test for the conditional clear: the completion
// write must clear Message only if it still holds the exact migrating text
// it set at the stopping step — never a message something else wrote in the
// meantime.
//
// It also pins the status endpoint's side (ptone/scion#2267): a
// message-only status POST while the migration is in flight is a guarded
// no-op (200 {"applied":false,"reason":"reincarnation_in_flight"}) and
// leaves the worker's migrating message in place. Before #2267 that POST
// bypassed Guard 0b and was the path this test used to set a foreign
// message; with it closed, the foreign writer is simulated by a direct
// store write (UpdateAgentStatus, which does not apply Guard 0b) while the
// worker is paused just before DispatchAgentStart.
func TestReincarnateAgent_MigratingMessagePreservedIfNewGenAlreadySetOne(t *testing.T) {
	disp := newGatedDispatcher("start", nil)
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)
	ctx := context.Background()

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	<-disp.entered // worker is about to call DispatchAgentStart; reincarnation_state is "starting"

	inFlight, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	require.Equal(t, store.ReincarnationStateStarting, inFlight.ReincarnationState,
		"this test is only meaningful while a migration is genuinely in flight")
	require.Equal(t, "migrating to generation 2", inFlight.Message)

	// The status endpoint: a message-only POST is dropped by Guard 0b.
	statusBody, err := json.Marshal(store.AgentStatusUpdate{Message: "gen 2 says hi"})
	require.NoError(t, err)
	statusReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agent.ID+"/status", bytes.NewReader(statusBody))
	statusReq = statusReq.WithContext(contextWithIdentity(statusReq.Context(), agentIdentityFor(agent.ID, project.ID, ScopeAgentStatusUpdate)))
	statusRec := httptest.NewRecorder()
	srv.updateAgentStatus(statusRec, statusReq, agent.ID)
	require.Equal(t, http.StatusOK, statusRec.Code, statusRec.Body.String())
	var statusResp map[string]any
	require.NoError(t, json.Unmarshal(statusRec.Body.Bytes(), &statusResp), statusRec.Body.String())
	assert.Equal(t, false, statusResp["applied"])
	assert.Equal(t, "reincarnation_in_flight", statusResp["reason"])

	afterPost, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	require.Equal(t, "migrating to generation 2", afterPost.Message,
		"a message-only status POST must not replace the worker's migrating message while the migration is in flight")

	// A foreign writer sets Message directly in the store mid-flight.
	require.NoError(t, s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{Message: "gen 2 says hi"}))
	midFlight, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	require.Equal(t, "gen 2 says hi", midFlight.Message)

	close(disp.release)
	waitForReincarnationSettled(t, s, agent.ID)

	final, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ReincarnationStateNone, final.ReincarnationState)
	assert.Equal(t, "gen 2 says hi", final.Message,
		"the completion write must not clobber a message that is no longer the migrating text")
}

// TestReincarnateAgent_FailureNeverLeavesMigratingMessageStale is the design
// Amendment A26.8 test for the failure path: writeFailedAgent's message
// write is unconditional (design §3.7), so a failure can never leave
// "migrating to generation N" stale on the agent row — it is always replaced
// by a failure message.
func TestReincarnateAgent_FailureNeverLeavesMigratingMessageStale(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	disp.startErr = fmt.Errorf("no such image: nonexistent:latest")
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)

	req := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"})
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, req, agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	waitForReincarnationSettled(t, s, agent.ID)

	final, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.NotEqual(t, "migrating to generation 2", final.Message,
		"a failure must never leave the in-flight migrating message stale")
	assert.Contains(t, final.Message, "reincarnation failed: start failed: no such image")
}

// TestReincarnateAgent_AC6_NotifiesWithRealisticActivity is the design §3.4
// Amendment A5.3 regression test. The notification dispatcher matches a
// subscription on the status event's Activity when non-empty, falling back
// to Phase only when Activity is empty. A real running agent almost always
// has a non-empty Activity ("working", "idle", "waiting_for_input", …), and
// the previous AC-6 notification test only passed because its fixture left
// Activity empty. Without clearing Activity on failure, the ERROR
// notification either never fires (Activity isn't a trigger) or a stale,
// misleading one fires instead (Activity happens to be a trigger like
// "waiting_for_input"). failReincarnation and the stopping step both clear
// Activity, mirroring the synchronous stop handler.
func TestReincarnateAgent_AC6_NotifiesWithRealisticActivity(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	disp.startErr = fmt.Errorf("no such image: nonexistent:latest")
	srv, s, project, broker := setupReincarnateTestServer(t, disp)

	pub := NewChannelEventPublisher()
	srv.SetEventPublisher(pub)
	t.Cleanup(pub.Close)
	nd := NewNotificationDispatcher(s, pub, func() AgentDispatcher { return disp }, slog.Default())
	nd.Start()
	t.Cleanup(nd.Stop)

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Activity = "working" // realistic: a running agent almost always has one
	})
	coordinator := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.ID = tid("coord-" + t.Name())
		a.Slug = "coord-" + tidSlugSafe(t.Name())
	})
	requester := delegatingRequesterFor(coordinator.ID, project.ID)

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, requester, ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	waitForReincarnationSettled(t, s, agent.ID)

	final, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "error", final.Phase)
	assert.Equal(t, "", final.Activity, "Activity must be cleared on failure, mirroring the stop handler")

	require.Eventually(t, func() bool {
		notifs, notifErr := s.GetNotifications(context.Background(), store.SubscriberTypeAgent, coordinator.Slug, false)
		return notifErr == nil && len(notifs) > 0
	}, 2*time.Second, 50*time.Millisecond,
		"requester must be notified of the failure even when the agent had a realistic non-empty Activity")
}

// TestReincarnateAgent_StopFailureIsFatal is the design §3.4 Amendment A3
// regression test: a DispatchAgentStop error must fail the reincarnation before any
// config write, not be tolerated. Before the fix, a stop failure was logged
// and ignored, so the worker went on to reprovision and start a new session
// while the old generation's container was potentially still running.
func TestReincarnateAgent_StopFailureIsFatal(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	disp.stopErr = fmt.Errorf("broker unreachable")
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)

	req := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"})
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, req, agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	waitForReincarnationSettled(t, s, agent.ID)

	final, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "error", final.Phase)
	assert.Equal(t, store.ReincarnationStateFailed, final.ReincarnationState)
	assert.Equal(t, 1, final.Generation, "generation must not increment when stop fails")
	assert.Contains(t, final.Message, "broker unreachable")
	assert.Equal(t, "old-image:v1", final.AppliedConfig.Image, "config must not be overwritten when stop fails")

	assert.Zero(t, disp.reprovisionCalls, "reprovision must not run when stop fails")
	assert.Zero(t, disp.startCalls, "start must not run when stop fails")

	list, err := s.ListAgentReincarnations(context.Background(), agent.ID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, store.AgentReincarnationStateFailed, list[0].State)
	assert.Contains(t, list[0].Error, "broker unreachable")
}

// TestSweepStaleReincarnations_MarksNonTerminalFailed is the design §3.7
// boot-sweep regression test: a reincarnation record and its agent's
// reincarnation_state left non-terminal (simulating a hub restart mid-flight,
// since the running worker never gets to finish either way) must both be
// marked failed with reason "hub restarted during reincarnation".
func TestSweepStaleReincarnations_MarksNonTerminalFailed(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	// CreateAgent does not persist ReincarnationState (it is always ""/None
	// for a brand-new agent in practice); set it via UpdateAgent afterward,
	// exactly as the real reincarnate worker would leave it mid-flight.
	agent.ReincarnationState = store.ReincarnationStateProvisioning
	agent.Phase = "provisioning"
	require.NoError(t, s.UpdateAgent(context.Background(), agent))
	require.NoError(t, s.CreateAgentReincarnation(context.Background(), &store.AgentReincarnation{
		AgentID:        agent.ID,
		FromGeneration: 1,
		ToGeneration:   2,
		State:          store.AgentReincarnationStatePending,
	}))

	// A future cutoff treats every existing row as stale, without needing an
	// actual 30-minute-old record (design §3.7's replica-safe staleness
	// bound; see sweepStaleReincarnationsOlderThan).
	n, err := srv.sweepStaleReincarnationsOlderThan(context.Background(), time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	after, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ReincarnationStateFailed, after.ReincarnationState)
	assert.Equal(t, "error", after.Phase)
	assert.Contains(t, after.Message, "hub restarted during reincarnation")

	list, err := s.ListAgentReincarnations(context.Background(), agent.ID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, store.AgentReincarnationStateFailed, list[0].State)
	assert.Equal(t, "hub restarted during reincarnation", list[0].Error)
}

// TestSweepStaleReincarnations_RestoresPreviousEnvIntoAgentRow pins that a
// failed reincarnation's rollback puts the previous env back into the agent
// row. The sweep restores from the PreviousAppliedConfig it reads back from
// the store, so this also covers the snapshot serialization: an env-style map
// the snapshot drops would be missing from the restored row.
func TestSweepStaleReincarnations_RestoresPreviousEnvIntoAgentRow(t *testing.T) {
	ctx := context.Background()
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	withEnv := func(prefix string) *api.ScionConfig {
		return &api.ScionConfig{
			Env: map[string]string{prefix + "_INLINE_VAR": prefix + "-inline"},
			Telemetry: &api.TelemetryConfig{Cloud: &api.TelemetryCloudConfig{
				Endpoint: "https://otel.example.com",
				Headers:  map[string]string{"x-" + prefix: prefix + "-header"},
			}},
		}
	}
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.AppliedConfig.Env = map[string]string{"EXPLICIT_VAR": "explicit-value"}
		a.AppliedConfig.InlineConfig = withEnv("APPLIED")
		a.AppliedConfig.CreateInputs.InlineConfig = withEnv("CREATE")
	})
	previous, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)

	// The record is written the way the handler writes it: the previous
	// snapshot is the agent's applied config as stored before migration.
	require.NoError(t, s.CreateAgentReincarnation(ctx, &store.AgentReincarnation{
		AgentID:               agent.ID,
		FromGeneration:        1,
		ToGeneration:          2,
		State:                 store.AgentReincarnationStateProvisioning,
		PreviousAppliedConfig: previous.AppliedConfig,
	}))

	// Mid-migration the row holds the fresh config, with no env of its own.
	agent.ReincarnationState = store.ReincarnationStateProvisioning
	agent.Phase = "provisioning"
	agent.AppliedConfig = &store.AgentAppliedConfig{Image: "fresh-image:v2"}
	require.NoError(t, s.UpdateAgent(ctx, agent))

	n, err := srv.sweepStaleReincarnationsOlderThan(ctx, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, n)

	after, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ReincarnationStateFailed, after.ReincarnationState)
	require.NotNil(t, after.AppliedConfig)
	assert.Equal(t, previous.AppliedConfig, after.AppliedConfig,
		"rollback must restore the previous applied config exactly, env maps included")
	assert.Equal(t, map[string]string{"EXPLICIT_VAR": "explicit-value"}, after.AppliedConfig.Env)
}

// TestSweepStaleReincarnations_IgnoresTerminalRecords is the companion test:
// a completed or already-failed record must not be touched, and an agent
// with no in-flight reincarnation must not be counted or modified.
func TestSweepStaleReincarnations_IgnoresTerminalRecords(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	beforeVersion := agent.StateVersion
	require.NoError(t, s.CreateAgentReincarnation(context.Background(), &store.AgentReincarnation{
		AgentID:        agent.ID,
		FromGeneration: 1,
		ToGeneration:   2,
		State:          store.AgentReincarnationStateCompleted,
	}))

	n, err := srv.sweepStaleReincarnations(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, n)

	after, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, beforeVersion, after.StateVersion, "an agent with no in-flight reincarnation must be untouched")
}

// TestSweepStaleReincarnations_TerminalAgentRecordOnly_AgentNone is the
// design §3.4 Amendment A6.5/A7 regression test: a stale record whose
// agent has ALREADY completed (reincarnation_state=="") must be failed on
// the record alone. The agent's state, phase, Activity, generation and
// AppliedConfig must be left completely untouched — something else (the
// completion write) already resolved the agent side of this migration, and
// there is nothing left for the sweep to reconcile there.
func TestSweepStaleReincarnations_TerminalAgentRecordOnly_AgentNone(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	// CreateAgent does not persist Generation or ReincarnationState (a
	// brand-new agent is always generation 1 / ""); set them via UpdateAgent
	// afterward, exactly as a real completed reincarnation would leave them.
	agent.ReincarnationState = store.ReincarnationStateNone
	agent.Generation = 2
	agent.Phase = "running"
	agent.Activity = "working"
	agent.AppliedConfig.Image = "gen-2-image"
	require.NoError(t, s.UpdateAgent(context.Background(), agent))
	beforeVersion := agent.StateVersion
	require.NoError(t, s.CreateAgentReincarnation(context.Background(), &store.AgentReincarnation{
		AgentID:               agent.ID,
		FromGeneration:        1,
		ToGeneration:          2,
		State:                 store.AgentReincarnationStateStarting,
		PreviousAppliedConfig: &store.AgentAppliedConfig{Image: "gen-1-image"},
	}))

	n, err := srv.sweepStaleReincarnationsOlderThan(context.Background(), time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	after, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, beforeVersion, after.StateVersion, "the agent row must be completely untouched")
	assert.Equal(t, store.ReincarnationStateNone, after.ReincarnationState)
	assert.Equal(t, "running", after.Phase)
	assert.Equal(t, "working", after.Activity)
	assert.Equal(t, 2, after.Generation)
	assert.Equal(t, "gen-2-image", after.AppliedConfig.Image, "gen N+1 must survive; the record's gen-1 PreviousAppliedConfig must not be restored")

	list, err := s.ListAgentReincarnations(context.Background(), agent.ID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, store.AgentReincarnationStateFailed, list[0].State)
	assert.Equal(t, "hub restarted during reincarnation", list[0].Error)
}

// TestSweepStaleReincarnations_TerminalAgentRecordOnly_AgentFailed is the
// second A6.5/A7 variant: an agent already at reincarnation_state=
// "failed" (an earlier failure or sweep pass already resolved it) must also
// be left untouched, including its own Message — this record's failure
// reason must not overwrite a DIFFERENT, earlier failure's message.
func TestSweepStaleReincarnations_TerminalAgentRecordOnly_AgentFailed(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	agent.ReincarnationState = store.ReincarnationStateFailed
	agent.Phase = "error"
	agent.Message = "reincarnation failed: an earlier, unrelated failure"
	agent.AppliedConfig.Image = "restored-gen-1-image"
	require.NoError(t, s.UpdateAgent(context.Background(), agent))
	beforeVersion := agent.StateVersion
	require.NoError(t, s.CreateAgentReincarnation(context.Background(), &store.AgentReincarnation{
		AgentID:               agent.ID,
		FromGeneration:        1,
		ToGeneration:          2,
		State:                 store.AgentReincarnationStateProvisioning,
		PreviousAppliedConfig: &store.AgentAppliedConfig{Image: "restored-gen-1-image"},
	}))

	n, err := srv.sweepStaleReincarnationsOlderThan(context.Background(), time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	after, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, beforeVersion, after.StateVersion, "the agent row must be completely untouched")
	assert.Equal(t, store.ReincarnationStateFailed, after.ReincarnationState)
	assert.Equal(t, "error", after.Phase)
	assert.Equal(t, "reincarnation failed: an earlier, unrelated failure", after.Message,
		"this record's failure reason must not overwrite an earlier, unrelated failure's Message")

	list, err := s.ListAgentReincarnations(context.Background(), agent.ID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, store.AgentReincarnationStateFailed, list[0].State)
	assert.Equal(t, "hub restarted during reincarnation", list[0].Error)
}

// =============================================================================
// Replica-safe sweep: the record's own state/updated_at must track
// the worker's progress (design §3.4 Amendment A6), and the sweep's failure
// paths must not touch anything a second writer already claimed or resolved.
// =============================================================================

// gatedDispatcher blocks the first call of the chosen step ("reprovision" or
// "start") until released, and optionally returns an error from that first
// call once released.
type gatedDispatcher struct {
	*reincarnateTestDispatcher
	step     string
	once     sync.Once
	entered  chan struct{}
	release  chan struct{}
	firstErr error
}

func newGatedDispatcher(step string, firstErr error) *gatedDispatcher {
	return &gatedDispatcher{
		reincarnateTestDispatcher: newReincarnateTestDispatcher(),
		step:                      step,
		entered:                   make(chan struct{}),
		release:                   make(chan struct{}),
		firstErr:                  firstErr,
	}
}

func (d *gatedDispatcher) gate() error {
	first := false
	d.once.Do(func() { first = true })
	if first {
		close(d.entered)
		<-d.release
		return d.firstErr
	}
	return nil
}

func (d *gatedDispatcher) DispatchAgentReprovision(ctx context.Context, a *store.Agent) error {
	if d.step == "reprovision" {
		if err := d.gate(); err != nil {
			return err
		}
	}
	return d.reincarnateTestDispatcher.DispatchAgentReprovision(ctx, a)
}

func (d *gatedDispatcher) DispatchAgentStart(ctx context.Context, a *store.Agent, task string, resume bool) error {
	if d.step == "start" {
		if err := d.gate(); err != nil {
			return err
		}
	}
	return d.reincarnateTestDispatcher.DispatchAgentStart(ctx, a, task, resume)
}

// TestReincarnateAgent_WorkerStepsBumpRecordUpdatedAt is the design §3.4
// Amendment A6 regression test: A5.2 requires the worker to bump the
// record's own state/updated_at on every step, not just at completion or
// failure — otherwise the sweep's staleness bound measures total worker
// duration from the record's insert time, not time since last progress.
func TestReincarnateAgent_WorkerStepsBumpRecordUpdatedAt(t *testing.T) {
	disp := newGatedDispatcher("reprovision", nil)
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	list, err := s.ListAgentReincarnations(context.Background(), agent.ID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	recID := list[0].ID
	defer close(disp.release)
	select {
	case <-disp.entered: // worker has written the stopping and provisioning steps
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for worker to reach DispatchAgentReprovision; it may have failed before reprovision")
	}

	// Both provisioning writes (the record via tryAdvanceReincarnation, the
	// agent row via updateReincarnationStep) finish before
	// DispatchAgentReprovision and share one timestamp, which stays stable
	// while the worker is held there.
	cur, err := s.GetAgentReincarnation(context.Background(), recID)
	require.NoError(t, err)
	require.Equal(t, store.AgentReincarnationStateProvisioning, cur.State,
		"the record's own state must track the worker's progress, not stay pending")

	a, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	require.NotNil(t, a.ReincarnationUpdatedAt, "the provisioning step must have set ReincarnationUpdatedAt")
	t.Logf("agent reincarnation_state=%q; record state=%q reincarnation_updated_at=%v record_updated_at=%v",
		a.ReincarnationState, cur.State, *a.ReincarnationUpdatedAt, cur.UpdatedAt)
	assert.False(t, cur.UpdatedAt.Before(*a.ReincarnationUpdatedAt),
		"the worker's provisioning step must have bumped the record's updated_at, so it cannot predate that step")
}

// TestReincarnateAgent_RecordUpdatedAtBumpedAtStartingStep exercises the same
// design §3.4 Amendment A6 property from a different angle: it gates the
// *start* call (after stopping/provisioning/starting have all been written) and compares
// the record's updated_at directly against the agent row's own
// ReincarnationUpdatedAt from that same starting-step write — the
// purpose-built clock (design §3.4 Amendment A6.6) that tracks exactly the
// writes this worker makes, unlike the general Updated field, which broker
// heartbeats bump too and so cannot be used to prove causal ordering between
// these two specific writes. tryAdvanceReincarnation pins both clocks to the
// same instant (see reincarnationStepUpdate.now), so they are never
// observably out of order for the same step.
func TestReincarnateAgent_RecordUpdatedAtBumpedAtStartingStep(t *testing.T) {
	disp := newGatedDispatcher("start", nil)
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	<-disp.entered // stopping, provisioning and starting steps all written
	defer close(disp.release)

	list, err := s.ListAgentReincarnations(context.Background(), agent.ID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	mid, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	require.NotNil(t, mid.ReincarnationUpdatedAt, "the starting step must have set ReincarnationUpdatedAt")
	t.Logf("record requested_at=%s updated_at=%s; agent reincarnation_updated_at (starting step)=%s",
		list[0].RequestedAt.Format(time.RFC3339Nano), list[0].UpdatedAt.Format(time.RFC3339Nano), mid.ReincarnationUpdatedAt.Format(time.RFC3339Nano))
	assert.False(t, list[0].UpdatedAt.Before(*mid.ReincarnationUpdatedAt),
		"the worker's starting step must have bumped the record's updated_at, so it cannot predate that step")
}

// notificationRace wires a real NotificationDispatcher onto a reincarnate test
// server, with a coordinator agent that requests the reincarnation and so is
// subscribed to the target agent's notifications.
type notificationRace struct {
	srv         *Server
	s           store.Store
	pub         *ChannelEventPublisher
	project     *store.Project
	broker      *store.RuntimeBroker
	agent       *store.Agent
	coordinator *store.Agent
	sentinels   int
}

func newNotificationRace(t *testing.T, disp AgentDispatcher) *notificationRace {
	t.Helper()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	grantDevUserRuntimeBrokerAccess(t, s)

	pub := NewChannelEventPublisher()
	srv.SetEventPublisher(pub)
	t.Cleanup(pub.Close)
	nd := NewNotificationDispatcher(s, pub, func() AgentDispatcher { return disp }, slog.Default())
	nd.Start()
	t.Cleanup(nd.Stop)

	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	coordinator := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.ID = tid("coord-" + t.Name())
		a.Slug = "coord-" + tidSlugSafe(t.Name())
	})
	return &notificationRace{srv: srv, s: s, pub: pub, project: project, broker: broker, agent: agent, coordinator: coordinator}
}

func (r *notificationRace) reincarnate(t *testing.T) {
	t.Helper()
	requester := delegatingRequesterFor(r.coordinator.ID, r.project.ID)
	rec := httptest.NewRecorder()
	r.srv.handleReincarnateAgent(rec, reincarnateRequest(t, r.agent.ID, requester, ReincarnateAgentRequest{Handoff: "h"}), r.agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
}

// sendCrashHeartbeat reports the agent's container as crashed with exit code
// 137. The heartbeat handler publishes its status event before returning.
func (r *notificationRace) sendCrashHeartbeat(t *testing.T, activity string) {
	t.Helper()
	ec := 137
	code := sendHeartbeat(t, r.srv, r.broker.ID, r.project.ID, brokerAgentHeartbeat{
		Slug:       r.agent.Slug,
		Phase:      "stopped",
		Activity:   activity,
		ExitCode:   &ec,
		ExitReason: "crashed",
	})
	require.Equal(t, http.StatusOK, code)
}

// awaitDispatcherCaughtUp waits until the notification dispatcher has fully
// processed every status event published before the call. The dispatcher
// handles status events one at a time, in publish order, on a single
// goroutine, so once a notification exists for a sentinel event published
// now, every earlier event has been handled.
func (r *notificationRace) awaitDispatcherCaughtUp(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	r.sentinels++
	suffix := fmt.Sprintf("sentinel-%d-%s", r.sentinels, t.Name())
	sentinel := newReincarnateTestAgent(t, r.s, r.project, r.broker, func(a *store.Agent) {
		a.ID = tid(suffix)
		a.Slug = tidSlugSafe(suffix)
		a.Phase = "error"
		a.Message = "sentinel"
	})
	require.NoError(t, r.s.CreateNotificationSubscription(ctx, &store.NotificationSubscription{
		ID:                api.NewUUID(),
		Scope:             store.SubscriptionScopeAgent,
		AgentID:           sentinel.ID,
		SubscriberType:    store.SubscriberTypeAgent,
		SubscriberID:      r.coordinator.Slug,
		ProjectID:         r.project.ID,
		TriggerActivities: []string{"ERROR"},
		// Backdated: the dispatcher skips events older than the subscription,
		// and the sentinel agent row was written just above.
		CreatedAt: time.Now().Add(-time.Minute),
		CreatedBy: r.coordinator.ID,
	}))
	r.pub.PublishAgentStatus(ctx, sentinel)
	require.Eventually(t, func() bool {
		notifs, err := r.s.GetNotifications(ctx, store.SubscriberTypeAgent, r.coordinator.Slug, false)
		if err != nil {
			return false
		}
		for _, n := range notifs {
			if n.AgentID == sentinel.ID {
				return true
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond, "the notification dispatcher never processed the sentinel event")
}

// assertHeartbeatSuppressed checks that a crash heartbeat sent while the
// worker was paused left the worker-owned status fields as the worker last
// wrote them.
func (r *notificationRace) assertHeartbeatSuppressed(t *testing.T, beforeHeartbeat *store.Agent) {
	t.Helper()
	got, err := r.s.GetAgent(context.Background(), r.agent.ID)
	require.NoError(t, err)
	assert.Equal(t, beforeHeartbeat.Phase, got.Phase, "the racing heartbeat must not change the phase mid-migration")
	assert.Equal(t, beforeHeartbeat.Activity, got.Activity, "the racing heartbeat must not change the activity mid-migration")
	assert.Nil(t, got.ExitCode, "the racing heartbeat's exit code must not be recorded mid-migration")
}

// agentNotifications returns the coordinator's notifications about the
// reincarnated agent, ignoring sentinel notifications.
func (r *notificationRace) agentNotifications(t *testing.T) []store.Notification {
	t.Helper()
	notifs, err := r.s.GetNotifications(context.Background(), store.SubscriberTypeAgent, r.coordinator.Slug, false)
	require.NoError(t, err)
	var out []store.Notification
	for _, n := range notifs {
		if n.AgentID == r.agent.ID {
			out = append(out, n)
		}
	}
	return out
}

// TestReincarnateAgent_ExactlyOneErrorNotificationOnStartFailure proves a crash
// heartbeat racing a reincarnation cannot pre-empt the worker's own failure
// notification. Without heartbeat suppression, the racing heartbeat persists
// phase=error with an "Agent crashed" message and publishes its own ERROR
// notification; the notification dispatcher's dedup-by-last-status then
// swallows the worker's real failure notification, so the subscriber sees one
// notification carrying the wrong reason. With suppression, the only
// notification is the worker's, and it names the start error. The heartbeat
// is handled completely before the worker is allowed to fail, so the result
// cannot depend on goroutine timing.
func TestReincarnateAgent_ExactlyOneErrorNotificationOnStartFailure(t *testing.T) {
	for _, activity := range []string{"", "crashed"} {
		t.Run("activity="+activity, func(t *testing.T) {
			disp := newGatedDispatcher("start", fmt.Errorf("no such image: nonexistent:latest"))
			r := newNotificationRace(t, disp)
			r.reincarnate(t)

			<-disp.entered // worker paused inside DispatchAgentStart
			mid, err := r.s.GetAgent(context.Background(), r.agent.ID)
			require.NoError(t, err)
			require.Equal(t, store.ReincarnationStateStarting, mid.ReincarnationState, "sanity: the race must land in the starting state")

			r.sendCrashHeartbeat(t, activity)
			r.awaitDispatcherCaughtUp(t)
			r.assertHeartbeatSuppressed(t, mid)

			close(disp.release) // DispatchAgentStart now returns its error; the worker fails
			waitForReincarnationSettled(t, r.s, r.agent.ID)

			final, err := r.s.GetAgent(context.Background(), r.agent.ID)
			require.NoError(t, err)
			assert.Equal(t, "error", final.Phase, "the worker's own failure must still land")

			r.awaitDispatcherCaughtUp(t)
			notifs := r.agentNotifications(t)
			require.Len(t, notifs, 1, "exactly one notification must fire for the failed reincarnation")
			assert.Equal(t, "ERROR", notifs[0].Status)
			assert.Contains(t, notifs[0].Message, "no such image",
				"the notification must carry the start error, not the racing heartbeat's crash message")
		})
	}
}

// TestReincarnateAgent_NoErrorNotificationOnSuccessWithRacingCrashHeartbeat is
// the success-path counterpart: a crash heartbeat lands while the migration is
// provisioning (between stop and reprovision, where the old container's crash
// was observed live), and the migration then completes. Without suppression,
// the heartbeat persists phase=error and publishes an ERROR notification for
// a migration that succeeded.
func TestReincarnateAgent_NoErrorNotificationOnSuccessWithRacingCrashHeartbeat(t *testing.T) {
	for _, activity := range []string{"", "crashed"} {
		t.Run("activity="+activity, func(t *testing.T) {
			disp := newGatedDispatcher("reprovision", nil)
			r := newNotificationRace(t, disp)
			r.reincarnate(t)

			<-disp.entered // worker paused inside DispatchAgentReprovision
			mid, err := r.s.GetAgent(context.Background(), r.agent.ID)
			require.NoError(t, err)
			require.Equal(t, store.ReincarnationStateProvisioning, mid.ReincarnationState, "sanity: the race must land in the provisioning state")

			r.sendCrashHeartbeat(t, activity)
			r.awaitDispatcherCaughtUp(t)
			r.assertHeartbeatSuppressed(t, mid)

			close(disp.release) // reprovision and start succeed; the migration completes
			waitForReincarnationSettled(t, r.s, r.agent.ID)

			final, err := r.s.GetAgent(context.Background(), r.agent.ID)
			require.NoError(t, err)
			assert.NotEqual(t, "error", final.Phase, "the migration must succeed despite the racing crash heartbeat")
			assert.Nil(t, final.ExitCode, "the racing heartbeat's exit code must not be recorded")
			assert.Equal(t, store.ReincarnationStateNone, final.ReincarnationState)
			assert.Equal(t, 2, final.Generation, "the migration must actually complete, not just avoid phase=error")

			r.awaitDispatcherCaughtUp(t)
			for _, n := range r.agentNotifications(t) {
				assert.NotEqual(t, "ERROR", n.Status,
					"no ERROR notification may fire from the racing crash heartbeat when the migration succeeds (got %q)", n.Message)
			}
		})
	}
}

// TestReincarnateAgent_SweptWorkerFailureDoesNotClobberNewClaim is the design
// §3.4 Amendment A6 regression test: once the sweep has failed a
// record, the worker that used to own it must not be able to write the
// agent row (or the record) again — a version conflict is not the only
// guard needed here, because the worker's write does not race the claim on
// state_version, it races the sweep on the record's own state.
func TestReincarnateAgent_SweptWorkerFailureDoesNotClobberNewClaim(t *testing.T) {
	disp := newGatedDispatcher("reprovision", fmt.Errorf("broker timed out"))
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	list, err := s.ListAgentReincarnations(context.Background(), agent.ID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	rec1 := list[0]
	<-disp.entered

	ctx := context.Background()

	// Sweep (another replica, record considered stale) fails rec1.
	_, err = srv.sweepStaleReincarnationsOlderThan(ctx, time.Now().Add(time.Hour))
	require.NoError(t, err)
	swept, err := s.GetAgentReincarnation(ctx, rec1.ID)
	require.NoError(t, err)
	require.Equal(t, store.AgentReincarnationStateFailed, swept.State)

	// A new reincarnation claims the agent (simulated directly: state
	// pending, a new config marker), the way a fresh `scion reincarnate`
	// request would after the sweep reset reincarnation_state.
	a, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	a.ReincarnationState = store.ReincarnationStatePending
	a.AppliedConfig.Image = "gen-claimed-by-second-request"
	require.NoError(t, s.UpdateAgent(ctx, a))

	// The first worker's dispatch now returns (error).
	close(disp.release)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		r, _ := s.GetAgentReincarnation(ctx, rec1.ID)
		if r != nil && r.Error != swept.Error {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	after, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	r1, err := s.GetAgentReincarnation(ctx, rec1.ID)
	require.NoError(t, err)
	t.Logf("after: agent state=%q image=%q message=%q; rec1.Error=%q",
		after.ReincarnationState, after.AppliedConfig.Image, after.Message, r1.Error)
	assert.Equal(t, store.ReincarnationStatePending, after.ReincarnationState, "swept worker must not write the agent")
	assert.Equal(t, "gen-claimed-by-second-request", after.AppliedConfig.Image,
		"swept worker must not restore its own previous config over a newer claim")
	assert.Equal(t, swept.Error, r1.Error, "swept worker must not rewrite the swept record")
}

// TestReincarnateAgent_SweptWorkerFailureCASErrorDoesNotClobberNewClaim is
// the design §3.4 Amendment A9.1 regression test: the A8.2 disambiguation
// must prove authorship with a stamp match, not infer it from
// State==newState alone. Same scenario as
// SweptWorkerFailureDoesNotClobberNewClaim, plus ONE transient (not landed)
// error on the swept worker's own failure CAS. Without the stamp check,
// State=="failed"==newState holds for BOTH the sweep's own write and this
// worker's (never-landed) attempt — they are indistinguishable by state
// alone — so the retry's disambiguation would wrongly claim the sweep's
// write as its own and clobber the new claim.
func TestReincarnateAgent_SweptWorkerFailureCASErrorDoesNotClobberNewClaim(t *testing.T) {
	disp := newGatedDispatcher("reprovision", fmt.Errorf("broker timed out"))
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)
	// Installed (disarmed) before the worker starts and armed below, instead
	// of swapping srv.store under the running worker and restoring it while
	// the worker may still read it (ptone/scion#3184).
	fs, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *casFaultStore {
		return &casFaultStore{Store: inner, fault: f, failCASTo: store.AgentReincarnationStateFailed}
	})

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	list, err := s.ListAgentReincarnations(context.Background(), agent.ID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	rec1 := list[0]
	<-disp.entered
	ctx := context.Background()

	// Sweep (another replica, record considered stale) fails rec1.
	_, err = srv.sweepStaleReincarnationsOlderThan(ctx, time.Now().Add(time.Hour))
	require.NoError(t, err)
	swept, err := s.GetAgentReincarnation(ctx, rec1.ID)
	require.NoError(t, err)
	require.Equal(t, store.AgentReincarnationStateFailed, swept.State)

	// A new reincarnation claims the agent.
	a, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	a.ReincarnationState = store.ReincarnationStatePending
	a.AppliedConfig.Image = "gen-claimed-by-second-request"
	require.NoError(t, s.UpdateAgent(ctx, a))

	// One transient, NOT-landed error on the worker's own failure CAS.
	fault.Arm()

	// The first worker's dispatch now returns (error), driving it into
	// failReincarnation.
	close(disp.release)
	require.Eventually(t, func() bool { return fs.attempts.Load() >= 2 }, 3*time.Second, 2*time.Millisecond,
		"the worker must retry its failure CAS after the injected error")

	// Nothing past the retry involves further injected faults or external
	// I/O — only an in-process disambiguation re-read, and an agent write
	// ONLY if that disambiguation were (wrongly) to claim ownership. Poll
	// for a bounded, short moment so a clobber (if the fix regressed) is
	// still caught quickly, without sleeping the full bound unconditionally.
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		cur, gerr := s.GetAgent(ctx, agent.ID)
		require.NoError(t, gerr)
		if cur.ReincarnationState != store.ReincarnationStatePending {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	after, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	r1, err := s.GetAgentReincarnation(ctx, rec1.ID)
	require.NoError(t, err)
	t.Logf("after: agent state=%q image=%q message=%q; rec1.Error=%q",
		after.ReincarnationState, after.AppliedConfig.Image, after.Message, r1.Error)
	assert.Equal(t, store.ReincarnationStatePending, after.ReincarnationState, "swept worker must not write the agent")
	assert.Equal(t, "gen-claimed-by-second-request", after.AppliedConfig.Image,
		"swept worker must not restore its own previous config over a newer claim")
	assert.Equal(t, swept.Error, r1.Error, "swept worker must not rewrite the swept record")
}

// sweepDuringFailStore deterministically interleaves a rival sweep CAS (and a
// second reincarnation's claim) into the EXACT instant between
// failReincarnation's first (erroring) CAS attempt and its retry — no
// worker goroutine, gating, or polling needed: the race window is
// synthesized directly inside the fault-injecting TryAdvanceAgentReincarnation
// call itself.
type sweepDuringFailStore struct {
	store.Store
	fired  atomic.Bool
	onFire func(ctx context.Context)
}

func (f *sweepDuringFailStore) TryAdvanceAgentReincarnation(ctx context.Context, r *store.AgentReincarnation, expectState string, olderThan time.Time) (bool, error) {
	if r.State == store.AgentReincarnationStateFailed && r.Error == "boom" && f.fired.CompareAndSwap(false, true) {
		f.onFire(ctx)
		return false, fmt.Errorf("injected transient db error (write did not land)")
	}
	return f.Store.TryAdvanceAgentReincarnation(ctx, r, expectState, olderThan)
}

// TestReincarnateAgent_FailDisambiguationClaimsSweepWriteAndClobbersNewClaim
// is the design §3.4 Amendment A9.1 regression test, in the precise form:
// failReincarnation is called directly (no worker goroutine or dispatcher
// gating), and the fault store injects the rival sweep CAS plus a second
// reincarnation's claim into the exact window between the first (erroring,
// never-landed) CAS attempt and the retry. Without the UpdatedAt stamp
// check, the retry's disambiguation sees State=="failed"==newState — true
// for both this call's own never-landed attempt AND the sweep's unrelated
// write — and wrongly claims authorship, clobbering the second claim.
func TestReincarnateAgent_FailDisambiguationClaimsSweepWriteAndClobbersNewClaim(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	ctx := context.Background()
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.ReincarnationState = store.ReincarnationStateProvisioning
		a.AppliedConfig.Image = "new-image:v2"
	})
	previous := &store.AgentAppliedConfig{Image: "old-image:v1"}
	rec1 := &store.AgentReincarnation{AgentID: agent.ID, FromGeneration: 1, ToGeneration: 2,
		State: store.AgentReincarnationStateProvisioning, PreviousAppliedConfig: previous}
	require.NoError(t, s.CreateAgentReincarnation(ctx, rec1))

	fs := &sweepDuringFailStore{Store: s}
	fs.onFire = func(ctx context.Context) {
		// The sweep fails rec1 (its own CAS, its own error text).
		ok, err := s.TryAdvanceAgentReincarnation(ctx, &store.AgentReincarnation{ID: rec1.ID,
			State: store.AgentReincarnationStateFailed, Error: "hub restarted during reincarnation"},
			store.AgentReincarnationStateProvisioning, time.Time{})
		require.NoError(t, err)
		require.True(t, ok)
		// ...and a second, freshly admitted reincarnation claims the agent.
		a, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		a.ReincarnationState = store.ReincarnationStatePending
		a.AppliedConfig.Image = "gen-claimed-by-second-request"
		require.NoError(t, s.UpdateAgent(ctx, a))
		require.NoError(t, s.CreateAgentReincarnation(ctx, &store.AgentReincarnation{AgentID: agent.ID,
			FromGeneration: 1, ToGeneration: 2, State: store.AgentReincarnationStatePending}))
	}
	srv.store = fs
	srv.failReincarnation(ctx, agent.ID, rec1.ID, store.AgentReincarnationStateProvisioning, "boom", previous)
	srv.store = s

	after, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	got1, err := s.GetAgentReincarnation(ctx, rec1.ID)
	require.NoError(t, err)
	t.Logf("agent state=%q image=%q msg=%q; rec1 state=%q err=%q", after.ReincarnationState, after.AppliedConfig.Image, after.Message, got1.State, got1.Error)
	assert.Equal(t, store.ReincarnationStatePending, after.ReincarnationState, "the worker did not make the failed write; it must not write the agent row")
	assert.Equal(t, "gen-claimed-by-second-request", after.AppliedConfig.Image)
}

// TestReincarnateAgent_SweptWorkerFailureDoesNotClobberSecondWorker covers
// the same defect end-to-end (a real second `scion reincarnate` request and
// worker, rather than a manually simulated claim): once the sweep fails
// worker 1's record, worker 1's later (delayed) dispatch failure must not
// undo worker 2's completed migration.
func TestReincarnateAgent_SweptWorkerFailureDoesNotClobberSecondWorker(t *testing.T) {
	disp := newGatedDispatcher("reprovision", fmt.Errorf("broker timeout"))
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h1"}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code)
	<-disp.entered

	_, err := srv.sweepStaleReincarnationsOlderThan(context.Background(), time.Now().Add(time.Hour))
	require.NoError(t, err)

	rec = httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h2"}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	waitForReincarnationSettled(t, s, agent.ID) // worker 2 completes

	close(disp.release) // worker 1's reprovision finally returns an error
	time.Sleep(200 * time.Millisecond)

	after, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	t.Logf("final: state=%q phase=%q gen=%d taskHasPreamble=%v", after.ReincarnationState, after.Phase, after.Generation,
		containsAll(after.AppliedConfig.Task, "[SCION REINCARNATION]"))
	assert.Equal(t, store.ReincarnationStateNone, after.ReincarnationState, "worker 2 completed; a swept worker 1 must not overwrite it")
	assert.Contains(t, after.AppliedConfig.Task, "[SCION REINCARNATION]", "worker 2's gen N+1 config must survive")
}

// TestReincarnateAgent_SweepKeepsGenNPlusOneAfterSuccessfulReprovision is the
// design §3.4 Amendment A6.5 regression test: the sweep must not
// blindly restore PreviousAppliedConfig for every stale record. Once the
// agent's own reincarnation_state reaches "starting", DispatchAgentReprovision
// has already succeeded and the disk holds gen N+1 — no status report ever
// corrects AppliedConfig, so restoring gen N here would permanently strand
// the store on the wrong generation's config (violates A4.3).
func TestReincarnateAgent_SweepKeepsGenNPlusOneAfterSuccessfulReprovision(t *testing.T) {
	disp := newGatedDispatcher("start", nil)
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	<-disp.entered // reprovision succeeded; worker is inside Start (hub "crashes" here)

	mid, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	require.Equal(t, store.ReincarnationStateStarting, mid.ReincarnationState)
	require.Contains(t, mid.AppliedConfig.Task, "[SCION REINCARNATION]")

	// Another replica's sweep, 30+ min later.
	_, err = srv.sweepStaleReincarnationsOlderThan(context.Background(), time.Now().Add(time.Hour))
	require.NoError(t, err)
	close(disp.release)
	time.Sleep(100 * time.Millisecond)

	after, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	t.Logf("after sweep: state=%q image=%q taskHasPreamble=%v", after.ReincarnationState, after.AppliedConfig.Image,
		containsAll(after.AppliedConfig.Task, "[SCION REINCARNATION]"))
	assert.Contains(t, after.AppliedConfig.Task, "[SCION REINCARNATION]",
		"reprovision had succeeded (state=starting): the sweep must keep the gen N+1 AppliedConfig (A4.3), not restore gen N")
}

// TestReincarnateAgent_SweepCASRespectsStalenessCutoff is the design §3.4
// Amendment A7.1/A9.2 regression test: the sweep's CAS itself enforces the
// same staleness bound the sweep used to select the record, not just the
// state. A record fresher than the cutoff (its own UpdatedAt is AFTER the
// cutoff) must not be failed, even calling sweepFailStaleRecord on it
// directly with that record still held — closing the gap where a live
// worker bumps a record's UpdatedAt WITHOUT (yet) changing its State between
// the sweep's list query and its CAS.
func TestReincarnateAgent_SweepCASRespectsStalenessCutoff(t *testing.T) {
	disp := newGatedDispatcher("reprovision", nil)
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)
	ctx := context.Background()

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	<-disp.entered // worker stalled inside reprovision; record state=provisioning
	defer close(disp.release)

	list, err := s.ListAgentReincarnations(ctx, agent.ID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	r := list[0]

	cutoff := r.UpdatedAt.Add(-time.Minute) // r is NOT stale relative to this cutoff
	require.True(t, r.UpdatedAt.After(cutoff))

	srv.sweepFailStaleRecord(ctx, r, "hub restarted during reincarnation", cutoff)

	got, err := s.GetAgentReincarnation(ctx, r.ID)
	require.NoError(t, err)
	assert.NotEqual(t, store.AgentReincarnationStateFailed, got.State,
		"a record fresher than the sweep's cutoff must not be failed by the sweep's CAS")
}

// interposingStore lets a test run code between the sweep's GetAgent (the
// read sweepFailStaleRecord uses to detect an already-resolved agent) and
// the record CAS that follows it.
type interposingStore struct {
	store.Store
	armed   atomic.Bool
	onFirst func()
}

func (s *interposingStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	a, err := s.Store.GetAgent(ctx, id)
	if s.armed.CompareAndSwap(true, false) && s.onFirst != nil {
		s.onFirst()
	}
	return a, err
}

// twoGateDispatcher blocks in reprovision (until relReprov; then succeeds)
// and again in start (until relStart).
type twoGateDispatcher struct {
	*reincarnateTestDispatcher
	inReprov, relReprov, inStart, relStart chan struct{}
}

func (d *twoGateDispatcher) DispatchAgentReprovision(ctx context.Context, a *store.Agent) error {
	close(d.inReprov)
	<-d.relReprov
	return d.reincarnateTestDispatcher.DispatchAgentReprovision(ctx, a)
}

func (d *twoGateDispatcher) DispatchAgentStart(ctx context.Context, a *store.Agent, task string, resume bool) error {
	close(d.inStart)
	<-d.relStart
	return d.reincarnateTestDispatcher.DispatchAgentStart(ctx, a, task, resume)
}

// TestReincarnateAgent_SweepDoesNotRestoreGenNOverSuccessfulReprovisionRace
// is the design §3.4 Amendment A7 regression test for the sweep's own
// TOCTOU: it lists a record as stale, then reads the agent to decide whether
// the agent side is already resolved, and only THEN CASes the record. A
// worker that advances between the sweep's agent read and its CAS — here:
// its stalled reprovision call returns success and it CASes the record to
// "starting" — must not have gen N restored over its now-successfully
// reprovisioned gen N+1 disk. The sweep's CAS is pinned to the state it
// listed the record at (advanceListedRecord), so once the worker has moved
// the record on, the sweep's CAS loses the race and does nothing.
func TestReincarnateAgent_SweepDoesNotRestoreGenNOverSuccessfulReprovisionRace(t *testing.T) {
	disp := &twoGateDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher(),
		inReprov: make(chan struct{}), relReprov: make(chan struct{}), inStart: make(chan struct{}), relStart: make(chan struct{})}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)
	ctx := context.Background()

	ip := &interposingStore{Store: srv.store}
	srv.store = ip

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	list, err := s.ListAgentReincarnations(ctx, agent.ID)
	require.NoError(t, err)
	recID := list[0].ID
	<-disp.inReprov // worker stalled inside reprovision; agent+record = provisioning

	// Right after the sweep reads the agent (sees "provisioning"), the
	// stalled worker's reprovision returns success and it advances to
	// "starting" — a live update the sweep's earlier read cannot see.
	ip.onFirst = func() {
		close(disp.relReprov) // reprovision returns success
		<-disp.inStart        // worker has CASed record->starting and written agent starting
	}
	ip.armed.Store(true)
	_, err = srv.sweepStaleReincarnationsOlderThan(ctx, time.Now().Add(time.Hour))
	require.NoError(t, err)
	close(disp.relStart)
	waitForReincarnationSettled(t, s, agent.ID)

	after, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	r, err := s.GetAgentReincarnation(ctx, recID)
	require.NoError(t, err)
	t.Logf("record state=%q err=%q; agent state=%q image=%q taskHasPreamble=%v",
		r.State, r.Error, after.ReincarnationState, after.AppliedConfig.Image,
		containsAll(after.AppliedConfig.Task, "[SCION REINCARNATION]"))
	assert.Contains(t, after.AppliedConfig.Task, "[SCION REINCARNATION]",
		"reprovision succeeded and the record reached 'starting' before the sweep's CAS: gen N must not be restored")
}

// TestReincarnateAgent_SweepDecidesFromRecordNotLaggingAgentRow is the design
// §3.4 Amendment A7 regression test for the second half of the same
// finding: the sweep's restore decision must come from rec.State (the
// listed record), not the agent row, because the agent write for a step
// always lands AFTER the matching record CAS (see tryAdvanceReincarnation's
// callers) — a hub crash in that exact window leaves record="starting"
// (reprovision already succeeded) while the agent still reads "provisioning".
// Deciding from the lagging agent row would restore gen N over a gen N+1
// disk; deciding from the record (the thing A6 made truthful) does not.
func TestReincarnateAgent_SweepDecidesFromRecordNotLaggingAgentRow(t *testing.T) {
	disp := newGatedDispatcher("start", nil)
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)
	ctx := context.Background()

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	<-disp.entered // record=starting, agent=starting, reprovision done
	defer close(disp.release)

	// Simulate the crash window: the record's CAS to "starting" landed, but
	// the agent's matching "starting" write did not.
	a, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	a.ReincarnationState = store.ReincarnationStateProvisioning
	require.NoError(t, s.UpdateAgent(ctx, a))

	_, err = srv.sweepStaleReincarnationsOlderThan(ctx, time.Now().Add(time.Hour))
	require.NoError(t, err)
	after, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	t.Logf("agent state=%q image=%q taskHasPreamble=%v", after.ReincarnationState, after.AppliedConfig.Image,
		containsAll(after.AppliedConfig.Task, "[SCION REINCARNATION]"))
	assert.Contains(t, after.AppliedConfig.Task, "[SCION REINCARNATION]",
		"the record said 'starting' (reprovision succeeded); gen N must not be restored")
}

// casFaultStore injects a fault into the reincarnation record's CAS path, to
// exercise tryAdvanceReincarnation's own retry-and-disambiguate logic
// (design §3.4 Amendment A8.2) rather than a real, timing-dependent DB
// failure.
//   - persistent=false (default): the fault fires exactly once, the first
//     time TryAdvanceAgentReincarnation is called with State==failCASTo.
//     landReal=false additionally means that first call never reaches the
//     real store at all (a fault before commit) — the retry's CAS finds the
//     record unchanged and succeeds normally.
//   - landReal=true: the faulted call DOES delegate to the real store
//     first (so the write actually commits), and returns the injected error
//     anyway — simulating a response lost after commit. The retry's CAS
//     then finds the record already moved past fromState, which is exactly
//     what tryAdvanceReincarnation's re-read disambiguation exists to catch.
//   - persistent=true: the fault fires on every call with State==failCASTo,
//     never delegating — a DB that never recovers within the retry budget.
type casFaultStore struct {
	store.Store
	fault      *storeFaultSwitch // nil: always active
	failCASTo  string
	landReal   bool
	persistent bool
	failedOnce atomic.Bool
	// attempts counts calls whose target State==failCASTo, incremented after
	// each such call has returned its result (real or faulted) — a
	// synchronization point tests can poll instead of sleeping a fixed
	// duration to wait for a retry to have happened.
	attempts atomic.Int32
}

func (f *casFaultStore) TryAdvanceAgentReincarnation(ctx context.Context, r *store.AgentReincarnation, expectState string, olderThan time.Time) (bool, error) {
	targeted := f.fault.Active() && f.failCASTo != "" && r.State == f.failCASTo
	if targeted && (f.persistent || f.failedOnce.CompareAndSwap(false, true)) {
		if f.landReal {
			_, _ = f.Store.TryAdvanceAgentReincarnation(ctx, r, expectState, olderThan)
		}
		if targeted {
			f.attempts.Add(1)
		}
		return false, fmt.Errorf("injected transient db error")
	}
	ok, err := f.Store.TryAdvanceAgentReincarnation(ctx, r, expectState, olderThan)
	if targeted {
		f.attempts.Add(1)
	}
	return ok, err
}

// TestReincarnateAgent_CompletionCASTransientErrorDoesNotLoseMigration is the
// design §3.4 Amendment A8.2 regression test: one transient error on the
// completion CAS, with the underlying write never having landed, must not
// silently drop the completion. tryAdvanceReincarnation retries with the
// same fromState/newState, the retry finds the record unchanged and
// succeeds, and generation still reaches ToGeneration (AC-1) — instead of
// the pre-fix behavior, where a single error was treated exactly like
// losing the CAS race, leaving generation at N and the record non-terminal
// until a sweep 30+ minutes later flipped a healthy gen-N+1 agent to
// failed/phase=error.
func TestReincarnateAgent_CompletionCASTransientErrorDoesNotLoseMigration(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	// Installed before the request and never restored: the worker goroutine
	// reads srv.store and can outlive waitForReincarnationSettled, so a
	// restore write would race it (ptone/scion#3184).
	installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *casFaultStore {
		return &casFaultStore{Store: inner, failCASTo: store.AgentReincarnationStateCompleted}
	})
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	settled := waitForReincarnationSettled(t, s, agent.ID)

	assert.Equal(t, store.AgentReincarnationStateCompleted, settled.State)
	after, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, after.Generation, "gen N+1 is running; generation must reach 2 despite one transient CAS error (AC-1)")
	assert.Equal(t, store.ReincarnationStateNone, after.ReincarnationState)
	assert.NotEqual(t, "error", after.Phase, "a successfully started agent must not be flipped to phase=error")

	// A sweep 30+ minutes later must find nothing left to do.
	n, err := srv.sweepStaleReincarnationsOlderThan(context.Background(), time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.Zero(t, n, "a completed record must not be swept")
}

// TestReincarnateAgent_CompletionCASErrorAfterLandedWriteIsTreatedAsOwned is
// the "write landed but errored" variant of A8.2: the FIRST attempt's write
// actually commits (landReal=true) before the injected error is returned —
// a response lost after commit, not a failure to commit. The retry's own
// CAS then finds the record already at "completed", not "starting"
// (fromState), so tryAdvanceReincarnation's disambiguation re-read must
// recognize this as this worker's own earlier success, not as having lost
// the record to someone else.
func TestReincarnateAgent_CompletionCASErrorAfterLandedWriteIsTreatedAsOwned(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	// Installed before the request and never restored: the worker goroutine
	// reads srv.store and can outlive waitForReincarnationSettled, so a
	// restore write would race it (ptone/scion#3184).
	installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *casFaultStore {
		return &casFaultStore{Store: inner, failCASTo: store.AgentReincarnationStateCompleted, landReal: true}
	})
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	settled := waitForReincarnationSettled(t, s, agent.ID)

	assert.Equal(t, store.AgentReincarnationStateCompleted, settled.State)
	after, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, after.Generation, "the first attempt's write actually landed; the retry must detect that and still complete the migration")
	assert.Equal(t, store.ReincarnationStateNone, after.ReincarnationState)
}

// TestReincarnateAgent_StepCASPersistentErrorEndsFailed is the design §3.4
// Amendment A8.2 "step CAS still errors after retries" regression test: a
// persistent (never-recovering) error on a step's record CAS must run the
// normal failure path (failReincarnation) rather than have the worker return
// silently — the pre-fix behavior left the agent stopped, non-terminal, and
// behind a 409 for up to 30+ minutes with nothing recorded and nobody
// notified.
func TestReincarnateAgent_StepCASPersistentErrorEndsFailed(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	// Installed before the request and never restored: the worker goroutine
	// reads srv.store and can outlive waitForReincarnationSettled, so a
	// restore write would race it (ptone/scion#3184).
	installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *casFaultStore {
		return &casFaultStore{Store: inner, failCASTo: store.AgentReincarnationStateProvisioning, persistent: true}
	})
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	settled := waitForReincarnationSettled(t, s, agent.ID)

	assert.Equal(t, store.AgentReincarnationStateFailed, settled.State)
	assert.Contains(t, settled.Error, "failed to advance record to provisioning")
	after, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ReincarnationStateFailed, after.ReincarnationState,
		"a persistent step CAS error must run the normal failure path, not strand the agent silently")
	assert.Equal(t, "error", after.Phase)
	assert.Equal(t, "old-image:v1", after.AppliedConfig.Image, "a pre-reprovision failure must restore `previous`")
}

// TestReincarnateAgent_E2E_BrokerReprovisionRefusal409EndsInFailedRecord is
// an end-to-end regression test for design §3.4 Amendment A4.2: unlike every
// other test in this file, it wires a REAL HTTPAgentDispatcher (not the
// reincarnateTestDispatcher fake) against a real HTTP server standing in for
// the runtime broker, so the whole wire path is exercised — the broker's
// Conflict(...) JSON envelope, brokerHTTPTransport's status>=400 handling
// (brokerHTTPError), and failReincarnation's own error-message recording —
// not just the in-process Go error value a fake dispatcher would hand back
// directly. It proves a 409 from the broker (a refused reprovision, e.g.
// agent.ErrReprovisionRefused) ends with the record failed and its Error
// field carrying the broker's reason, and the agent at reincarnation_state=
// failed / phase=error, exactly like any other reprovision-dispatch failure.
func TestReincarnateAgent_E2E_BrokerReprovisionRefusal409EndsInFailedRecord(t *testing.T) {
	const refusalReason = "reprovision refused: agent e2e-409-agent container is still running, stop it first"
	fakeBroker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/api/v1/agents") {
			// Mirrors runtimebroker/errors.go's Conflict(...) envelope exactly,
			// so brokerHTTPError's "runtime broker returned error 409: <body>"
			// carries the same text a real broker would send.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{
					"code":    "conflict",
					"message": "Failed to provision agent: " + refusalReason,
				},
			})
			return
		}
		// Every other call this worker makes (Stop) just needs to succeed.
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer fakeBroker.Close()

	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   tid("e2e-409-project-" + t.Name()),
		Name: "E2E 409 Test Project",
		Slug: "e2e-409-test-project-" + tidSlugSafe(t.Name()),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	broker := &store.RuntimeBroker{
		ID:           tid("e2e-409-broker-" + t.Name()),
		Name:         "E2E 409 Test Broker",
		Slug:         "e2e-409-test-broker-" + tidSlugSafe(t.Name()),
		Endpoint:     fakeBroker.URL,
		Status:       store.BrokerStatusOnline,
		Capabilities: &store.BrokerCapabilities{Reprovision: true},
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: project.ID, BrokerID: broker.ID, BrokerName: broker.Name, Status: store.BrokerStatusOnline,
	}))
	project.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, s.UpdateProject(ctx, project))

	srv.SetDispatcher(NewHTTPAgentDispatcher(s, false, slog.Default()))

	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	settled := waitForReincarnationSettled(t, s, agent.ID)
	assert.Equal(t, store.AgentReincarnationStateFailed, settled.State)
	assert.Contains(t, settled.Error, "409", "the record's error must surface the broker's HTTP status")
	assert.Contains(t, settled.Error, refusalReason, "the record's error must surface the broker's refusal reason verbatim")

	after, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ReincarnationStateFailed, after.ReincarnationState)
	assert.Equal(t, "error", after.Phase)
	assert.Equal(t, "old-image:v1", after.AppliedConfig.Image, "reprovision never succeeded, so AppliedConfig must be restored to `previous`")
}

// TestReincarnateAgent_BackstopNotDefeatedByHeartbeat is the design §3.4
// Amendment A6.6 regression test: the agent-state backstop must key
// its staleness check on reincarnation_updated_at, not on `updated` — every
// broker heartbeat's UpdateAgentStatus bumps `updated` for any agent whose
// container the broker still reports (including a stopped one), which would
// otherwise keep an orphaned claim (pending, no record) from ever looking
// stale.
func TestReincarnateAgent_BackstopNotDefeatedByHeartbeat(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	agent.ReincarnationState = store.ReincarnationStatePending // failed claim-revert residue: no record
	claimedAt := time.Now()
	agent.ReincarnationUpdatedAt = &claimedAt // set the same way handleReincarnateAgent's claim does
	require.NoError(t, s.UpdateAgent(context.Background(), agent))

	time.Sleep(20 * time.Millisecond)
	cutoff := time.Now() // "30 minutes after the orphaned claim"
	time.Sleep(20 * time.Millisecond)
	// The broker heartbeat that arrived within the last 30s.
	require.NoError(t, s.UpdateAgentStatus(context.Background(), agent.ID, store.AgentStatusUpdate{Phase: "stopped"}))

	_, err := srv.sweepStaleReincarnationsOlderThan(context.Background(), cutoff)
	require.NoError(t, err)

	after, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ReincarnationStateFailed, after.ReincarnationState,
		"an orphaned claim with no record must be reset even though the broker keeps heartbeating the agent")

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, agentIdentityFor(agent.ID, project.ID), ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
	assert.NotEqual(t, http.StatusConflict, rec.Code, "agent must not be wedged behind a permanent 409")
}

// TestReincarnateAgent_BackstopResetsOrphanAgentState is the design §3.4
// Amendment A5.2 backstop regression test: an agent left non-terminal with
// no matching non-terminal AgentReincarnation record (e.g. the record was
// deleted, or the claim landed but CreateAgentReincarnation never did) still
// gets reset once it looks stale — but not before.
func TestReincarnateAgent_BackstopResetsOrphanAgentState(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	agent.ReincarnationState = store.ReincarnationStateStarting
	agent.Message = "migrating to generation 2" // what a real in-flight worker would have set
	claimedAt := time.Now()
	agent.ReincarnationUpdatedAt = &claimedAt // a real claim always sets this
	require.NoError(t, s.UpdateAgent(context.Background(), agent))

	n, err := srv.sweepStaleReincarnations(context.Background())
	require.NoError(t, err)
	assert.Zero(t, n, "fresh orphan must not be swept")

	n, err = srv.sweepStaleReincarnationsOlderThan(context.Background(), time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	after, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ReincarnationStateFailed, after.ReincarnationState)
	assert.Equal(t, "error", after.Phase)
	// Amendment A26.8: the orphan-sweep path (reincarnate_worker.go's
	// sweepStaleReincarnationsOlderThan) sets Message unconditionally too, so
	// a stale "migrating to generation N" can never survive this path either.
	assert.NotEqual(t, "migrating to generation 2", after.Message)
	assert.Contains(t, after.Message, "reincarnation failed: hub restarted during reincarnation")
}

// =============================================================================
// AC-2b: reincarnate must replay the full
// create pipeline (applyProjectDefaults -> applyHubAgentDefaults ->
// populateAgentConfig/resolveDerivedConfig), not resolveDerivedConfig alone,
// or a project/hub default loses to the template instead of outranking it.
// =============================================================================

// TestReincarnateAgent_AC2b_ProjectDefaultModelBeatsTemplate is the test the
// design calls for directly: a project default_model annotation must still
// outrank the template's model after reincarnate, exactly as it does on
// create.
func TestReincarnateAgent_AC2b_ProjectDefaultModelBeatsTemplate(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)

	project.Annotations = map[string]string{"scion.io/default-model": "project-model"}
	require.NoError(t, s.UpdateProject(context.Background(), project))

	template := &store.Template{
		ID:          tid("tmpl-ac2b-model-" + t.Name()),
		Name:        "t",
		Slug:        "reincarnate-template-ac2b-model-" + tidSlugSafe(t.Name()),
		Harness:     "claude",
		Scope:       store.TemplateScopeGlobal,
		Status:      store.TemplateStatusActive,
		ContentHash: "template-hash-ac2b",
		Config:      &store.TemplateConfig{Model: "template-model"},
	}
	require.NoError(t, s.CreateTemplate(context.Background(), template))

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Template = template.Slug
		// Model was never explicit at create: CreateInputs (and therefore the
		// fresh config) must leave it empty so project/hub defaults can fill
		// it, rather than freezing in whatever the outgoing generation had.
		a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{}
		a.AppliedConfig.Model = "project-model" // what create resolved it to
	})
	self := agentIdentityFor(agent.ID, project.ID)

	req := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{DryRun: true})
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, req, agent.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp ReincarnateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "project-model", resp.Plan.Model.New,
		"project default_model must still outrank the template after reincarnate")
}

// TestReincarnateAgent_AC2b_HubDefaultHarnessConfigStillApplies covers an
// agent whose harness config name came from the hub's operational
// agent_defaults (no project annotation, no explicit request) at create
// time. HarnessConfig is NOT a kept field —
// CreateInputs.HarnessConfig is empty here (it was never explicit), so
// buildFreshAppliedConfig leaves the fresh slot empty and deriveAgentConfig's
// own applyHubAgentDefaults rung re-fills it from the CURRENT hub default,
// landing on the same name coincidentally (this test's hub default is
// configured to match). This proves deriveAgentConfig's full replay
// (applyHubAgentDefaults's ctx flag included) re-derives HarnessConfigID/Hash
// for it correctly, matching what create would produce for the same
// hub-defaulted name.
func TestReincarnateAgent_AC2b_HubDefaultHarnessConfigStillApplies(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)

	srv.mu.Lock()
	srv.config.AgentDefaults.DefaultHarnessConfig = "hub-default-hc-" + tidSlugSafe(t.Name())
	srv.mu.Unlock()

	hc := &store.HarnessConfig{
		ID:          tid("hc-ac2b-" + t.Name()),
		Name:        "Hub Default HC",
		Slug:        "hub-default-hc-" + tidSlugSafe(t.Name()),
		Harness:     "claude",
		Scope:       store.HarnessConfigScopeGlobal,
		Status:      store.HarnessConfigStatusActive,
		ContentHash: "hc-hash-v2",
	}
	require.NoError(t, s.CreateHarnessConfig(context.Background(), hc))

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		// What create resolved via the hub default (no project annotation, no
		// explicit request) — a kept field, so CreateInputs.HarnessConfig
		// must be empty (see the next test) but AppliedConfig.HarnessConfig
		// carries the resolved name forward.
		a.AppliedConfig.HarnessConfig = "hub-default-hc-" + tidSlugSafe(t.Name())
		a.AppliedConfig.HarnessConfigHash = "hc-hash-v1" // stale, from the old generation
		a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{}
	})
	self := agentIdentityFor(agent.ID, project.ID)

	req := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{DryRun: true})
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, req, agent.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp ReincarnateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "hc-hash-v1", resp.Plan.HarnessCfg.Old)
	assert.Equal(t, "hc-hash-v2", resp.Plan.HarnessCfg.New,
		"the hub-defaulted harness config name must be kept and its ID/Hash re-derived fresh")
}

// TestReincarnateAgent_AC2b_CreateInputsHarnessConfigEmptyWhenNotExplicit is
// the store-level regression test for the CreateInputs.HarnessConfig
// capture bug: it must record what the
// requester actually asked for (req.HarnessConfig / req.Config.HarnessConfig),
// never the request->project->template-resolved value buildAppliedConfig's
// caller computes.
func TestReincarnateAgent_AC2b_CreateInputsHarnessConfigEmptyWhenNotExplicit(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, _ := setupReincarnateTestServer(t, disp)

	project.Annotations = map[string]string{"scion.io/default-harness-config": "project-default-hc"}
	require.NoError(t, s.UpdateProject(context.Background(), project))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "ac2b-no-explicit-harness-config",
		ProjectID: project.ID,
		// No HarnessConfig field, no Config.HarnessConfig: fully implicit,
		// resolved only via the project annotation.
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	persisted, err := s.GetAgent(context.Background(), resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, persisted.AppliedConfig)
	require.NotNil(t, persisted.AppliedConfig.CreateInputs)

	assert.Equal(t, "project-default-hc", persisted.AppliedConfig.HarnessConfig,
		"the live AppliedConfig should still carry the project-resolved name")
	assert.Empty(t, persisted.AppliedConfig.CreateInputs.HarnessConfig,
		"CreateInputs.HarnessConfig must be empty: the requester never specified one explicitly")
}

// TestReincarnateAgent_AC2b_TemplateHarnessConfigBeatsHubDefault is the
// tightened half of AC-2b: a template's harness config name
// must still outrank a hub operational default after reincarnate, exactly as
// TestCreateAgent_HubDefaultHarnessConfig_LosesToTemplate proves for create.
// Before deriveAgentConfig's harness-config resolution rung was moved ahead
// of applyHubAgentDefaults, the hub tier would have won by reaching the
// still-empty slot first.
// TestDispatchAgentEventHandler_SetsCreateInputs is the design §3.4 Amendment
// A3 regression test: without CreateInputs, every scheduled agent looks "legacy" to
// `scion reincarnate` forever, and the legacy fallback would permanently pin
// whatever HarnessConfig/HarnessAuth/Profile/ThinkingLevel it resolved to
// into CreateInputs on the FIRST reincarnation — so even a second
// reincarnation would never pick up a template change. Branch and
// NoAuth=true are the only explicit inputs a scheduled agent has.
func TestDispatchAgentEventHandler_SetsCreateInputs(t *testing.T) {
	ms := newMockStore()
	ms.projects["project-1"] = &store.Project{ID: "project-1", Name: "test-project"}
	creatorID := seedFullRoleDispatchCreator(ms, "project-1")
	srv := newEventHandlerTestServer(&resolvingTemplateStore{ms})

	err := srv.dispatchAgentEventHandler()(context.Background(), withMockAgentRevision(store.ScheduledEvent{
		ID:        "dispatch-createinputs-1",
		ProjectID: "project-1",
		EventType: "dispatch_agent",
		Payload:   `{"agentName":"sched-createinputs","task":"Do the thing","branch":"sched-branch"}`,
		CreatedBy: creatorID,
	}, creatorID))
	require.NoError(t, err)

	created := findMockAgent(ms, "sched-createinputs")
	require.NotNil(t, created, "agent was not created")
	require.NotNil(t, created.AppliedConfig)
	require.NotNil(t, created.AppliedConfig.CreateInputs,
		"scheduled dispatch must persist CreateInputs, or the agent is stuck looking legacy to reincarnate forever")
	assert.Equal(t, "sched-branch", created.AppliedConfig.CreateInputs.Branch)
	assert.True(t, created.AppliedConfig.CreateInputs.NoAuth, "every scheduled agent is NoAuth by construction")
}

func TestReincarnateAgent_AC2b_TemplateHarnessConfigBeatsHubDefault(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)

	template := createHarnessTemplate(t, s, "reincarnate-tmpl-beats-hub-"+tidSlugSafe(t.Name()), "template-hc")
	setHubAgentDefaults(srv, opsettings.AgentDefaultsSettings{DefaultHarnessConfig: "hub-hc-" + tidSlugSafe(t.Name())})

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Template = template.Slug
		a.AppliedConfig.HarnessConfig = "template-hc"             // what create resolved it to at generation 1
		a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{} // never explicit
	})

	fresh, _, err := srv.buildFreshAppliedConfig(context.Background(), agent, project, "")
	require.NoError(t, err)
	assert.Equal(t, "template-hc", fresh.HarnessConfig,
		"the template's harness config must still outrank the hub default after reincarnate")
}

// TestReincarnateAgent_AC2b_Matrix_CreateAndReincarnateAgree is the
// required matrix test: over a set of scenarios that each source
// HarnessConfig/Model/HarnessAuth/Profile/ThinkingLevel from a different tier
// (explicit request, project annotation, template, hub operational default),
// create and reincarnate must land on identical values for all of those
// fields plus NoAuth, and GCPIdentity must survive as a kept field
// unchanged. This is the regression guard for deriveAgentConfig actually
// being the single shared pipeline both paths claim to use.
func TestReincarnateAgent_AC2b_Matrix_CreateAndReincarnateAgree(t *testing.T) {
	tl := func(v int) *int { return &v }

	cases := []struct {
		name               string
		projectAnnotations map[string]string
		hubDefaults        opsettings.AgentDefaultsSettings
		templateHC         string
		templateModel      string
		explicit           CreateAgentRequest
		autoNoAuthHC       bool // seed a harness config with no_auth.behavior=drop-to-shell and no satisfiable creds
		gcpAdcHC           bool // seed a harness config whose auth type the assigned GCPIdentity satisfies
	}{
		{
			name: "explicit beats everything",
			explicit: CreateAgentRequest{
				HarnessConfig: "explicit-hc",
				HarnessAuth:   "explicit-auth",
				Profile:       "explicit-profile",
				Config: &api.ScionConfig{
					Model:         "explicit-model",
					ThinkingLevel: tl(2),
					Skills:        []api.SkillReference{{URI: "skill://explicit-skill"}},
				},
			},
		},
		{
			name: "project defaults, nothing explicit",
			projectAnnotations: map[string]string{
				"scion.io/default-harness-config": "project-hc",
				"scion.io/default-harness-auth":   "project-auth",
				"scion.io/default-model":          "project-model",
				"scion.io/default-thinking-level": "3",
				"scion.io/active-profile":         "project-profile",
			},
		},
		{
			name:          "template supplies harness config and model",
			templateHC:    "template-hc",
			templateModel: "template-model",
		},
		{
			name: "hub defaults, nothing else",
			hubDefaults: opsettings.AgentDefaultsSettings{
				DefaultHarnessConfig: "hub-hc",
				DefaultHarnessAuth:   "hub-auth",
				DefaultModel:         "hub-model",
				DefaultThinkingLevel: tl(4),
			},
		},
		{
			name:     "explicit none auth derives NoAuth",
			explicit: CreateAgentRequest{HarnessAuth: "none"},
		},
		{
			// role=none must survive the round trip exactly like
			// an explicit --no-auth would, because AgentRole is itself kept.
			name:     "role=none derives NoAuth",
			explicit: CreateAgentRequest{AgentRole: "none"},
		},
		{
			// The auto-no-auth fallback (resolveDerivedConfig) must
			// fire identically on both sides for a harness config that
			// declares no_auth.behavior=drop-to-shell with no satisfiable
			// credentials — this is NOT an explicit NoAuth input, so it is
			// the one case createInputs.NoAuth does NOT cover on its own; the
			// fresh.HarnessAuth=="none" OR-term in buildFreshAppliedConfig
			// (fed by deriveAgentConfig's own auto-fallback re-running) is
			// what must catch it.
			name:         "auto-no-auth from harness config with no credentials",
			explicit:     CreateAgentRequest{HarnessConfig: "auto-noauth-hc"},
			autoNoAuthHC: true,
		},
		{
			// design §3.4 Amendment A5.4: the auto-no-auth check reads
			// GCPIdentity (agentHasGCPIdentityAssigned), so a case where an
			// assigned GCP identity SATISFIES the harness config's auth type
			// must land on the same NoAuth=false on both sides. Every case in
			// this matrix now assigns the same GCPIdentity via the real create
			// request, but only this case's harness config makes that
			// assignment actually matter to the auto-no-auth outcome.
			name:     "auto-no-auth skipped when GCP identity satisfies the harness config's auth type",
			explicit: CreateAgentRequest{HarnessConfig: "gcp-adc-hc"},
			gcpAdcHC: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			disp := newReincarnateTestDispatcher()
			srv, s, project, _ := setupReincarnateTestServer(t, disp)

			if len(tc.projectAnnotations) > 0 {
				project.Annotations = tc.projectAnnotations
				require.NoError(t, s.UpdateProject(ctx, project))
			}
			setHubAgentDefaults(srv, tc.hubDefaults)

			var templateSlug string
			if tc.templateHC != "" || tc.templateModel != "" {
				template := &store.Template{
					ID:                   tid("tmpl-matrix-" + tc.name + "-" + t.Name()),
					Name:                 "t",
					Slug:                 "reincarnate-matrix-" + tidSlugSafe(tc.name) + "-" + tidSlugSafe(t.Name()),
					Harness:              "claude",
					DefaultHarnessConfig: tc.templateHC,
					ContentHash:          "matrix-template-hash",
					Scope:                store.TemplateScopeGlobal,
					Status:               store.TemplateStatusActive,
					Config:               &store.TemplateConfig{Model: tc.templateModel},
				}
				require.NoError(t, s.CreateTemplate(ctx, template))
				templateSlug = template.Slug
			}

			if tc.autoNoAuthHC {
				hc := &store.HarnessConfig{
					ID:          tid("hc-matrix-" + tc.name + "-" + t.Name()),
					Name:        "auto-noauth-hc",
					Slug:        "auto-noauth-hc",
					Harness:     "claude",
					ContentHash: "auto-noauth-hash",
					Scope:       store.HarnessConfigScopeGlobal,
					Status:      store.HarnessConfigStatusActive,
					Config: &store.HarnessConfigData{
						NoAuthBehavior: "drop-to-shell",
						AuthMeta: &api.HarnessAuthMetadata{
							Types: map[string]api.HarnessAuthTypeMetadata{
								"api-key": {
									RequiredEnv: []api.HarnessAuthEnvRequirement{
										{AnyOf: []string{"ANTHROPIC_API_KEY"}},
									},
								},
							},
						},
					},
				}
				require.NoError(t, s.CreateHarnessConfig(ctx, hc))
			}
			if tc.gcpAdcHC {
				hc := &store.HarnessConfig{
					ID:          tid("hc-gcp-matrix-" + tc.name + "-" + t.Name()),
					Name:        "gcp-adc-hc",
					Slug:        "gcp-adc-hc",
					Harness:     "claude",
					ContentHash: "gcp-adc-hash",
					Scope:       store.HarnessConfigScopeGlobal,
					Status:      store.HarnessConfigStatusActive,
					Config: &store.HarnessConfigData{
						NoAuthBehavior: "drop-to-shell",
						AuthMeta: &api.HarnessAuthMetadata{
							Types: map[string]api.HarnessAuthTypeMetadata{
								"vertex-ai": {
									RequiredEnv: []api.HarnessAuthEnvRequirement{
										{AnyOf: []string{"GOOGLE_CLOUD_PROJECT"}},
									},
									// SkippedWhenGCPServiceAccountAssigned is
									// what actually makes isAuthTypeSatisfied
									// skip the RequiredEnv check above when a
									// GCP identity is assigned (it treats the
									// whole auth type as "GCP-backed
									// runtime-provided" — the env check alone
									// is not itself GCP-aware).
									RequiredFiles: []api.HarnessAuthFileRequirement{
										{
											Name:                                 "gcloud-adc",
											Type:                                 "file",
											Field:                                "GoogleAppCredentials",
											AlternativeEnvKeys:                   []string{"GOOGLE_APPLICATION_CREDENTIALS"},
											SkippedWhenGCPServiceAccountAssigned: true,
											Required:                             true,
										},
									},
								},
							},
						},
					},
				}
				require.NoError(t, s.CreateHarnessConfig(ctx, hc))
			}

			// design §3.4 Amendment A5.4: assign the SAME GCPIdentity on
			// both sides by giving the real create request one, rather than
			// stamping it onto the row after the fact — create's auto-no-auth
			// check runs with it in place exactly as reincarnate's does.
			sa := &store.GCPServiceAccount{
				ID:         tid("sa-matrix-" + tc.name + "-" + t.Name()),
				Scope:      store.ScopeProject,
				ScopeID:    project.ID,
				Email:      "matrix-worker@example.iam.gserviceaccount.com",
				ProjectID:  "matrix-gcp-project",
				Verified:   true,
				VerifiedAt: time.Now(),
				CreatedBy:  tid("user-creator"),
				CreatedAt:  time.Now(),
			}
			require.NoError(t, s.CreateGCPServiceAccount(ctx, sa))

			// --- create: the real create HTTP path. ---
			createReq := tc.explicit
			createReq.Name = "matrix-create-" + tidSlugSafe(tc.name)
			createReq.ProjectID = project.ID
			createReq.Template = templateSlug
			createReq.GCPIdentity = &GCPIdentityAssignment{MetadataMode: store.GCPMetadataModeAssign, ServiceAccountID: sa.ID}
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", createReq)
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
			var createResp CreateAgentResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &createResp))
			created, err := s.GetAgent(ctx, createResp.Agent.ID)
			require.NoError(t, err)
			require.NotNil(t, created.AppliedConfig)
			require.NotNil(t, created.AppliedConfig.CreateInputs,
				"create must always persist CreateInputs (design §3.4 Amendment A3.7); a nil here would silently fall back to the legacy heuristic")
			require.NotNil(t, created.AppliedConfig.GCPIdentity, "precondition: create must have resolved the assigned GCPIdentity")
			gcpIdentity := created.AppliedConfig.GCPIdentity

			// --- snapshot what create actually produced, BEFORE staling the
			// live row (round-trip the CREATED agent's own
			// CreateInputs, do not hand-build a second one). ---
			wantVal := *created.AppliedConfig // shallow copy: decouple from the in-place staling below
			want := &wantVal
			wantEnv := maps.Clone(want.Env)
			var wantSkills []api.SkillReference
			if want.InlineConfig != nil {
				wantSkills = want.InlineConfig.Skills
			}
			wantHubAccessScopes := append([]string(nil), want.HubAccessScopes...)

			// --- stale the live row: every derived field gets garbage that
			// does not appear anywhere else in this test, so a passing
			// assertion below can only mean the value was re-derived, not
			// coincidentally inherited. CreateInputs is left untouched —
			// it's the real one create persisted. ---
			staleThinking := 99
			created.AppliedConfig.HarnessConfig = "stale-hc-from-old-generation"
			created.AppliedConfig.HarnessAuth = "stale-auth"
			created.AppliedConfig.NoAuth = false
			created.AppliedConfig.Model = "stale-model"
			created.AppliedConfig.Profile = "stale-profile"
			created.AppliedConfig.ThinkingLevel = &staleThinking
			created.AppliedConfig.Image = "stale-image:v0"
			created.AppliedConfig.Env = map[string]string{"STALE_KEY": "stale-value"}
			created.AppliedConfig.HarnessConfigID = "stale-hcid"
			created.AppliedConfig.HarnessConfigHash = "stale-hash"
			created.AppliedConfig.TemplateHash = "stale-template-hash"
			created.AppliedConfig.HubAccessScopes = []string{"stale-scope"}
			// design §3.4 Amendment A3.5 kept fields: stale them too, so
			// the compare below can only pass if buildFreshAppliedConfig
			// actually copies them from `old`, not merely leaves its own
			// zero-value default sitting there by coincidence.
			// Unlike the fields staled above, these two are KEPT fields
			// (design §3.4 Amendment A3.5): the correct behavior is to carry
			// forward whatever the CURRENT/live row says, not reset to
			// create's original value — so the values set here are what
			// fresh must reproduce, not `want`'s.
			keptWorkspaceStoragePath := "gs://evolved-bucket/evolved-path"
			keptAgentRoleGrandfathered := !want.AgentRoleGrandfathered
			created.AppliedConfig.WorkspaceStoragePath = keptWorkspaceStoragePath
			created.AppliedConfig.WorkspaceStorageBucket = "evolved-bucket"
			created.AppliedConfig.AgentRoleGrandfathered = keptAgentRoleGrandfathered
			require.NoError(t, s.UpdateAgent(ctx, created))

			fresh, _, err := srv.buildFreshAppliedConfig(ctx, created, project, "")
			require.NoError(t, err)

			assert.Equal(t, want.HarnessConfig, fresh.HarnessConfig, "HarnessConfig must match what create produced")
			assert.Equal(t, want.Model, fresh.Model, "Model must match what create produced")
			assert.Equal(t, want.HarnessAuth, fresh.HarnessAuth, "HarnessAuth must match what create produced")
			assert.Equal(t, want.NoAuth, fresh.NoAuth, "NoAuth must match what create produced")
			assert.Equal(t, want.Profile, fresh.Profile, "Profile must match what create produced")
			if assert.Equal(t, want.ThinkingLevel == nil, fresh.ThinkingLevel == nil, "ThinkingLevel nilness must agree") &&
				want.ThinkingLevel != nil {
				assert.Equal(t, *want.ThinkingLevel, *fresh.ThinkingLevel, "ThinkingLevel must match what create produced")
			}
			assert.Equal(t, want.Image, fresh.Image, "Image must match what create produced")
			assert.Equal(t, wantEnv, fresh.Env, "Env must match what create produced")
			assert.Equal(t, want.HarnessConfigID, fresh.HarnessConfigID, "HarnessConfigID must match what create produced")
			assert.Equal(t, want.HarnessConfigHash, fresh.HarnessConfigHash, "HarnessConfigHash must match what create produced")
			assert.Equal(t, want.TemplateHash, fresh.TemplateHash, "TemplateHash must match what create produced")
			assert.Equal(t, wantHubAccessScopes, fresh.HubAccessScopes, "HubAccessScopes must match what create produced")
			assert.Equal(t, keptWorkspaceStoragePath, fresh.WorkspaceStoragePath, "WorkspaceStoragePath must be kept from the live row, not reset")
			assert.Equal(t, "evolved-bucket", fresh.WorkspaceStorageBucket, "WorkspaceStorageBucket must be kept with WorkspaceStoragePath")
			assert.Equal(t, keptAgentRoleGrandfathered, fresh.AgentRoleGrandfathered, "AgentRoleGrandfathered must be kept from the live row, not reset")
			var freshSkills []api.SkillReference
			if fresh.InlineConfig != nil {
				freshSkills = fresh.InlineConfig.Skills
			}
			assert.Equal(t, wantSkills, freshSkills, "InlineConfig.Skills must match what create produced")
			assert.Equal(t, gcpIdentity, fresh.GCPIdentity, "GCPIdentity must be kept unchanged")

			if tc.autoNoAuthHC {
				require.True(t, want.NoAuth, "precondition: create must have taken the auto-no-auth fallback")
			}
			if tc.gcpAdcHC {
				require.False(t, want.NoAuth,
					"precondition: the assigned GCPIdentity must satisfy the harness config's auth type, so create must NOT have taken the auto-no-auth fallback")
			}
		})
	}
}

// TestReincarnateAgent_TemplateHarnessTypeNotUsedAsHarnessConfig pins
// ptone/scion#601 item 2 on the reincarnate path, which re-derives the
// harness config through deriveAgentConfig: a harness-type-only template
// leaves the slot to the hub default (and, with none, empty).
func TestReincarnateAgent_TemplateHarnessTypeNotUsedAsHarnessConfig(t *testing.T) {
	for _, tc := range []struct {
		name       string
		hubDefault string
		want       string
	}{
		{name: "NoHubDefault", hubDefault: "", want: ""},
		{name: "HubDefaultWins", hubDefault: "hub-hc", want: "hub-hc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			disp := newReincarnateTestDispatcher()
			srv, s, project, broker := setupReincarnateTestServer(t, disp)
			if tc.hubDefault != "" {
				setHubAgentDefaults(srv, opsettings.AgentDefaultsSettings{DefaultHarnessConfig: tc.hubDefault})
			}

			template := &store.Template{
				ID:          tid("tmpl-harness-type-" + t.Name()),
				Name:        "team-claude-reviewer",
				Slug:        "reincarnate-harness-type-" + tidSlugSafe(t.Name()),
				Harness:     "claude",
				Scope:       store.TemplateScopeGlobal,
				Status:      store.TemplateStatusActive,
				ContentHash: "tmpl-hash",
			}
			require.NoError(t, s.CreateTemplate(context.Background(), template))

			agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
				a.Template = template.Slug
			})

			fresh, _, err := srv.buildFreshAppliedConfig(context.Background(), agent, project, "")
			require.NoError(t, err)
			require.NotNil(t, fresh)
			assert.Equal(t, tc.want, fresh.HarnessConfig,
				"the template's harness type must not be used as a harness-config name on reincarnate")
		})
	}
}

// TestReincarnateAgent_PlanExplicitProfileOverrideImageBeatsTemplate pins
// ptone/scion#1799 on the reincarnate plan: an EXPLICIT
// profiles.<p>.harness_overrides.<hc>.image outranks the template image (as
// the broker's Start now does), while the user's explicit request image
// still outranks the profile override.
func TestReincarnateAgent_PlanExplicitProfileOverrideImageBeatsTemplate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // isolate LoadEffectiveSettings from any ambient config
	hcSlug := "profile-image-hc-" + tidSlugSafe(t.Name())
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".scion"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".scion", "settings.yaml"), []byte(`schema_version: "1"
active_profile: pinned
profiles:
  pinned:
    runtime: docker
    harness_overrides:
      `+hcSlug+`:
        image: profile-image:v4
harness_configs:
  `+hcSlug+`:
    harness: claude
    image: settings-image:v1
`), 0644))

	for _, tc := range []struct {
		name          string
		explicitImage string
		want          string
	}{
		{name: "profile override beats template", want: "profile-image:v4"},
		{name: "explicit request image beats profile override", explicitImage: "explicit-image:v9", want: "explicit-image:v9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			disp := newReincarnateTestDispatcher()
			srv, s, project, broker := setupReincarnateTestServer(t, disp)

			template := &store.Template{
				ID:          tid("tmpl-profile-" + t.Name()),
				Name:        "t",
				Slug:        "reincarnate-template-profile-" + tidSlugSafe(t.Name()),
				Harness:     "claude",
				Scope:       store.TemplateScopeGlobal,
				Status:      store.TemplateStatusActive,
				ContentHash: "new-template-hash",
				Config:      &store.TemplateConfig{Image: "template-image:v2"},
			}
			require.NoError(t, s.CreateTemplate(context.Background(), template))

			agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
				a.Template = template.Slug
				a.AppliedConfig.HarnessConfig = hcSlug
				a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{HarnessConfig: hcSlug}
				if tc.explicitImage != "" {
					a.AppliedConfig.CreateInputs.InlineConfig = &api.ScionConfig{Image: tc.explicitImage}
				}
			})
			self := agentIdentityFor(agent.ID, project.ID)

			req := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{DryRun: true})
			rec := httptest.NewRecorder()
			srv.handleReincarnateAgent(rec, req, agent.ID)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

			var resp ReincarnateAgentResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.Equal(t, tc.want, resp.Plan.Image.New)
		})
	}
}

// claimRefusedTxStore refuses the reincarnation claim inside the claim
// transaction with err.
type claimRefusedTxStore struct {
	store.Store
	err error
}

func (s *claimRefusedTxStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return s.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&claimRefusedTx{Store: tx, err: s.err})
	})
}

type claimRefusedTx struct {
	store.Store
	err error
}

func (t *claimRefusedTx) ClaimAgentReincarnation(context.Context, string, int64, time.Time) (int64, error) {
	return 0, t.err
}

// A reincarnation claim refused inside the claim transaction answers 409
// with the refusal's message and leaves no reincarnation record.
func TestReincarnateAgent_ClaimRefusedInTransaction(t *testing.T) {
	cases := []struct {
		name string
		err  error
		msg  string
	}{
		{"already pending", store.ErrClaimPredicate, "a reincarnation is already pending for this agent"},
		{"start in progress", &store.ClaimHeldError{ClaimID: "c", Kind: store.StartClaimUser, State: store.StartClaimLive, Since: time.Now()}, "a start is in progress for this agent; retry once it completes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			disp := newReincarnateTestDispatcher()
			srv, s, project, broker := setupReincarnateTestServer(t, disp)
			agent := newReincarnateTestAgent(t, s, project, broker, nil)
			self := agentIdentityFor(agent.ID, project.ID)
			srv.store = &claimRefusedTxStore{Store: srv.store, err: tc.err}

			rec := httptest.NewRecorder()
			srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
			require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), tc.msg)
			list, err := s.ListAgentReincarnations(context.Background(), agent.ID)
			require.NoError(t, err)
			assert.Empty(t, list, "a refused claim leaves no record")
		})
	}
}
