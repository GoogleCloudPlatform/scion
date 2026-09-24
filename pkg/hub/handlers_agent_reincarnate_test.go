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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
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
}

func newReincarnateTestDispatcher() *reincarnateTestDispatcher {
	return &reincarnateTestDispatcher{}
}

func (d *reincarnateTestDispatcher) DispatchAgentCreate(context.Context, *store.Agent) error {
	return nil
}
func (d *reincarnateTestDispatcher) DispatchAgentProvision(context.Context, *store.Agent) error {
	return nil
}
func (d *reincarnateTestDispatcher) DispatchAgentReprovision(_ context.Context, agent *store.Agent) error {
	d.mu.Lock()
	d.reprovisionCalls++
	err := d.reprovisionErr
	d.mu.Unlock()
	return err
}
func (d *reincarnateTestDispatcher) DispatchAgentStart(_ context.Context, _ *store.Agent, task string, resume bool) error {
	d.mu.Lock()
	d.startCalls++
	d.lastStartTask = task
	d.lastStartResume = &resume
	err := d.startErr
	d.mu.Unlock()
	return err
}
func (d *reincarnateTestDispatcher) DispatchAgentStop(_ context.Context, _ *store.Agent) error {
	d.mu.Lock()
	d.stopCalls++
	err := d.stopErr
	d.mu.Unlock()
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
func (d *reincarnateTestDispatcher) DispatchAgentCreateWithGather(context.Context, *store.Agent) (*RemoteEnvRequirementsResponse, error) {
	return nil, nil
}
func (d *reincarnateTestDispatcher) DispatchFinalizeEnv(context.Context, *store.Agent, map[string]string) error {
	return nil
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
func waitForReincarnationSettled(t *testing.T, s store.Store, agentID string) *store.AgentReincarnation {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		list, err := s.ListAgentReincarnations(context.Background(), agentID)
		require.NoError(t, err)
		if len(list) > 0 {
			switch list[0].State {
			case store.AgentReincarnationStateCompleted, store.AgentReincarnationStateFailed:
				return list[0]
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
			// supports (p1b-r1 R4 / Amendment A2); a test that wants the
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

	t.Run("same project with the scope is allowed", func(t *testing.T) {
		other := agentIdentityFor(tid("coordinator"), project.ID, ScopeAgentLifecycle)
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
		{Image: "new:v2"},
		{HarnessConfig: "gemini"},
		{HarnessAuth: "oauth"},
		{Model: "opus"},
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

// TestBrokerHeartbeat_RefreshesCapabilities is the p1a-r1 R1(c) regression
// test for the chosen fix (heartbeat, not a live hub->broker /info query —
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
		// the exact false-412 case R1 describes.
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

// TestEmbeddedBrokerCapabilities_PassesReincarnateGate is the p1a-r1 R1(b)
// regression test: an embedded broker's capabilities (as cmd/server_broker.go
// now writes them, on both create and update) must pass the reincarnate 412
// gate, unlike the pre-fix Capabilities{Sync,Attach} literal that omitted
// Reprovision entirely.
func TestEmbeddedBrokerCapabilities_PassesReincarnateGate(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, _ := setupReincarnateTestServer(t, disp)

	// Mirrors cmd/server_broker.go's embedded-broker Capabilities literal
	// after the R1(b) fix, rather than importing cmd (which would pull the
	// whole CLI package into this test binary).
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

// TestReincarnateAgent_NonCloneWorkspace_Returns400 is the Amendment A2 /
// p1b-r1 R4 regression test: Phase 1 targets clone-per-agent workspaces only
// (design §7). An agent with no GitClone (worktree-per-agent or
// shared-workspace) must be rejected before anything is persisted or
// computed — including on a dry run, so --dry-run reports the restriction
// instead of showing a plan a real request could not safely execute.
func TestReincarnateAgent_NonCloneWorkspace_Returns400(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.AppliedConfig.GitClone = nil // worktree-per-agent / shared-workspace
	})
	beforeVersion := agent.StateVersion
	self := agentIdentityFor(agent.ID, project.ID)

	for _, dryRun := range []bool{true, false} {
		req := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h", DryRun: dryRun})
		rec := httptest.NewRecorder()
		srv.handleReincarnateAgent(rec, req, agent.ID)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "dryRun=%v: body: %s", dryRun, rec.Body.String())
	}

	after, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, beforeVersion, after.StateVersion, "agent must be untouched")
	assert.Equal(t, "", after.ReincarnationState)

	list, err := s.ListAgentReincarnations(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Empty(t, list, "no reincarnation record should be created")
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

// TestReincarnateAgent_LegacyAgent_DropsSkillsWithWarning covers the Phase 0
// review round 2 skills addendum: an agent with no CreateInputs (predates the
// field) must have InlineConfig.Skills dropped entirely by the legacy
// fallback, with a plan warning, rather than risk freezing stale injected
// skills in as template scope.
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
// design's A1 addendum 2 (Phase 0 review round 3, R3-2) calls for: a legacy
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
		// p1b-r1 N2: the old value ("v1") must NEVER appear in the plan — it
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

	fresh, _, err := srv.buildFreshAppliedConfig(context.Background(), agent, project)
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
	// 409 gate (p1b-r1 R1) checks agent.ReincarnationState, not the
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

// TestReincarnateAgent_AC8_OrphanCannotWedgeAfterConflict is the p1b-r1 R1
// regression test: a version conflict on the claim write (the guarded
// UpdateAgent that sets reincarnation_state=pending) must leave nothing
// behind — no orphaned agent_reincarnations row, and no stuck claim — so a
// retry succeeds. Before the fix, the record was created FIRST, so any
// failure on the following UpdateAgent left a permanent pending row with no
// worker running for it and no way to clear it.
type failOnceUpdateStore struct {
	store.Store
	mu     sync.Mutex
	failed bool
}

func (f *failOnceUpdateStore) UpdateAgent(ctx context.Context, a *store.Agent) error {
	f.mu.Lock()
	if !f.failed {
		f.failed = true
		f.mu.Unlock()
		return store.ErrVersionConflict
	}
	f.mu.Unlock()
	return f.Store.UpdateAgent(ctx, a)
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

// phaseObservingDispatcher simulates the container reporting a status-only
// phase update (as sciontool/the broker do via UpdateAgentStatus, which does
// not bump state_version) during Stop, and records the persisted phase seen
// at Reprovision time — the p1b-r1 R2 regression test.
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

// TestReincarnateAgent_R2_WorkerDoesNotClobberReportedPhase: the worker's
// per-step writes must merge onto the CURRENT row (re-read each time), not
// overwrite it with a stale in-memory copy. Before the fix, the worker wrote
// back the whole agent object it loaded before Stop, on every subsequent
// step — including a step wholly unrelated to the concurrent status report —
// so a status-only phase update landing during Stop was silently reverted at
// Reprovision time.
func TestReincarnateAgent_R2_WorkerDoesNotClobberReportedPhase(t *testing.T) {
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
	// (p1b-r1 R2's "set phase to stopping, then provisioning, then
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

// AC-6: a start failure leaves state=failed with an error and phase=error,
// and the previous config snapshot remains retrievable.
func TestReincarnateAgent_AC6_StartFailureMarksFailed(t *testing.T) {
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
	assert.Equal(t, store.ReincarnationStateFailed, final.ReincarnationState)
	assert.Equal(t, 1, final.Generation, "generation must not increment on failure")
	assert.Contains(t, final.Message, "no such image")

	list, err := s.ListAgentReincarnations(context.Background(), agent.ID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, store.AgentReincarnationStateFailed, list[0].State)
	assert.Contains(t, list[0].Error, "no such image")
	require.NotNil(t, list[0].PreviousAppliedConfig, "previous config snapshot must remain retrievable")
	assert.Equal(t, "old-image:v1", list[0].PreviousAppliedConfig.Image)
}

// TestReincarnateAgent_AC6_NonCreatorRequesterGetsNotifiedOnFailure is the
// design §3.4 Amendment A3.8 / p1b-r1 N1 regression test: a requester who is
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
	requester := agentIdentityFor(coordinator.ID, project.ID, ScopeAgentLifecycle)

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

	require.Eventually(t, func() bool {
		notifs, err := s.GetNotifications(context.Background(), store.SubscriberTypeAgent, coordinator.Slug, false)
		return err == nil && len(notifs) > 0
	}, 2*time.Second, 50*time.Millisecond,
		"the non-creator requester must actually receive a failure notification")
}

// TestReincarnateAgent_R3_StopFailureIsFatal is the p1b-r1 R3 regression
// test: a DispatchAgentStop error must fail the reincarnation before any
// config write, not be tolerated. Before the fix, a stop failure was logged
// and ignored, so the worker went on to reprovision and start a new session
// while the old generation's container was potentially still running.
func TestReincarnateAgent_R3_StopFailureIsFatal(t *testing.T) {
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

// TestSweepStaleReincarnations_MarksNonTerminalFailed is the design §3.7 F4 /
// p1b-r1 boot-sweep regression test: a reincarnation record and its agent's
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

	n, err := srv.sweepStaleReincarnations(context.Background())
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

// =============================================================================
// AC-2b (Phase 0 review round 4, R4-1): reincarnate must replay the full
// create pipeline (applyProjectDefaults -> applyHubAgentDefaults ->
// populateAgentConfig/resolveDerivedConfig), not resolveDerivedConfig alone,
// or a project/hub default loses to the template instead of outranking it.
// =============================================================================

// TestReincarnateAgent_AC2b_ProjectDefaultModelBeatsTemplate is the test the
// round 4 reviewer's repro calls for directly: a project default_model
// annotation must still outrank the template's model after reincarnate,
// exactly as it does on create.
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
// time. Per the round-5 correction, HarnessConfig is NOT a kept field —
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
// capture bug the round 4 reviewer flagged: it must record what the
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
// round-5-tightened half of AC-2b (p0-r5): a template's harness config name
// must still outrank a hub operational default after reincarnate, exactly as
// TestCreateAgent_HubDefaultHarnessConfig_LosesToTemplate proves for create.
// Before deriveAgentConfig's harness-config resolution rung was moved ahead
// of applyHubAgentDefaults, the hub tier would have won by reaching the
// still-empty slot first.
// TestDispatchAgentEventHandler_SetsCreateInputs is the p1b-r1 R6 regression
// test: without CreateInputs, every scheduled agent looks "legacy" to
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

	err := srv.dispatchAgentEventHandler()(context.Background(), store.ScheduledEvent{
		ID:        "dispatch-createinputs-1",
		ProjectID: "project-1",
		EventType: "dispatch_agent",
		Payload:   `{"agentName":"sched-createinputs","task":"Do the thing","branch":"sched-branch"}`,
		CreatedBy: creatorID,
	})
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

	fresh, _, err := srv.buildFreshAppliedConfig(context.Background(), agent, project)
	require.NoError(t, err)
	assert.Equal(t, "template-hc", fresh.HarnessConfig,
		"the template's harness config must still outrank the hub default after reincarnate")
}

// TestReincarnateAgent_AC2b_Matrix_CreateAndReincarnateAgree is the
// round-5-required matrix test: over a set of scenarios that each source
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
			// p1b-r1 C1: role=none must survive the round trip exactly like
			// an explicit --no-auth would, because AgentRole is itself kept.
			name:     "role=none derives NoAuth",
			explicit: CreateAgentRequest{AgentRole: "none"},
		},
		{
			// p1b-r1 R5: the auto-no-auth fallback (resolveDerivedConfig) must
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

			// --- create: the real create HTTP path. ---
			createReq := tc.explicit
			createReq.Name = "matrix-create-" + tidSlugSafe(tc.name)
			createReq.ProjectID = project.ID
			createReq.Template = templateSlug
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", createReq)
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
			var createResp CreateAgentResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &createResp))
			created, err := s.GetAgent(ctx, createResp.Agent.ID)
			require.NoError(t, err)
			require.NotNil(t, created.AppliedConfig)
			require.NotNil(t, created.AppliedConfig.CreateInputs,
				"create must always persist CreateInputs (p1b-r1 R6); a nil here would silently fall back to the legacy heuristic")

			// GCPIdentity resolution/access-checking is out of scope for
			// AC-2b (it happens in the create handler, not the derivation
			// pipeline); stamp one directly to test the KEPT-field round
			// trip in isolation.
			gcpIdentity := &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModeAssign, ServiceAccountID: "kept-sa"}
			created.AppliedConfig.GCPIdentity = gcpIdentity
			require.NoError(t, s.UpdateAgent(ctx, created))

			// --- snapshot what create actually produced, BEFORE staling the
			// live row (p1b-r1 R5: round-trip the CREATED agent's own
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
			require.NoError(t, s.UpdateAgent(ctx, created))

			fresh, _, err := srv.buildFreshAppliedConfig(ctx, created, project)
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
			var freshSkills []api.SkillReference
			if fresh.InlineConfig != nil {
				freshSkills = fresh.InlineConfig.Skills
			}
			assert.Equal(t, wantSkills, freshSkills, "InlineConfig.Skills must match what create produced")
			assert.Equal(t, gcpIdentity, fresh.GCPIdentity, "GCPIdentity must be kept unchanged")

			if tc.autoNoAuthHC {
				require.True(t, want.NoAuth, "precondition: create must have taken the auto-no-auth fallback")
			}
		})
	}
}
