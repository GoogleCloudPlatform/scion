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

// TestEmbeddedBrokerCapabilities_PassesReincarnateGate is the design §3.4
// Amendment A2 regression test: an embedded broker's capabilities (as cmd/server_broker.go
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

// TestReincarnateAgent_NonCloneWorkspace_Returns400 is the design §3.4
// Amendment A2/A4 regression test: Phase 1 targets clone-per-agent workspaces
// only (design §7). An agent with no GitClone (worktree-per-agent or
// shared-workspace) must be rejected before anything is persisted or
// computed — including on a dry run, so --dry-run reports the restriction
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
func TestReincarnateAgent_NonCloneWorkspace_Returns400(t *testing.T) {
	cases := []struct {
		name          string
		workspaceMode string // "" = clone-per-agent (the default for a git-remote project)
		wantRejected  bool
	}{
		{
			name:          "worktree-per-agent: GitClone is set, but this is not clone-per-agent",
			workspaceMode: store.WorkspaceModeWorktreePerAgent,
			wantRejected:  true,
		},
		{
			name:          "shared-workspace",
			workspaceMode: store.WorkspaceModeShared,
			wantRejected:  true,
		},
		{
			name:          "clone-per-agent: the one mode Phase 1 supports",
			workspaceMode: "",
			wantRejected:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			disp := newReincarnateTestDispatcher()
			srv, s, project, broker := setupReincarnateTestServer(t, disp)
			ctx := context.Background()

			project.GitRemote = "https://example.com/repo.git"
			if tc.workspaceMode != "" {
				project.Labels = map[string]string{store.LabelWorkspaceMode: tc.workspaceMode}
			}
			require.NoError(t, s.UpdateProject(ctx, project))

			// What the create path actually produces for this project —
			// not a hand-set field — so this test breaks if
			// populateAgentConfig's GitClone condition ever changes shape
			// again without a matching gate update.
			probe := &store.Agent{AppliedConfig: &store.AgentAppliedConfig{}}
			srv.populateAgentConfig(ctx, probe, project, nil)

			agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
				a.AppliedConfig.GitClone = probe.AppliedConfig.GitClone
				a.AppliedConfig.Workspace = ""
				a.AppliedConfig.CreateInputs.Workspace = ""
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

// TestReincarnateAgent_AC8_OrphanCannotWedgeAfterConflict is the design §3.4
// Amendment A3 regression test: a version conflict on the claim write (the guarded
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
// design §3.4 Amendment A5.1 (F1) regression test: failReincarnation must
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

	orig := srv.store
	srv.store = &conflictOnFailedWriteStore{Store: orig}
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	waitForReincarnationSettled(t, s, agent.ID)
	srv.store = orig

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
// the design §3.4 Amendment A5.2 (F2) regression test: the replica-safe
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

	assert.Equal(t, 1, disp.reprovisionCalls)
	assert.Zero(t, disp.startCalls, "start must not run when reprovision dispatch fails")

	list, err := s.ListAgentReincarnations(context.Background(), agent.ID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, store.AgentReincarnationStateFailed, list[0].State)
	assert.Contains(t, list[0].Error, "reprovision refused")
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

// TestReincarnateAgent_AC6_NotifiesWithRealisticActivity is the design §3.4
// Amendment A5.3 (F3) regression test. The notification dispatcher matches a
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
	requester := agentIdentityFor(coordinator.ID, project.ID, ScopeAgentLifecycle)

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

// TestReincarnateAgent_R3_StopFailureIsFatal is the design §3.4 Amendment A3
// regression test: a DispatchAgentStop error must fail the reincarnation before any
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
// Replica-safe sweep, round 3: the record's own state/updated_at must track
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
// Amendment A6 (R1) regression test: A5.2 requires the worker to bump the
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
	initial := list[0]
	<-disp.entered // worker has written the stopping and provisioning steps
	defer close(disp.release)

	time.Sleep(20 * time.Millisecond)
	cur, err := s.GetAgentReincarnation(context.Background(), initial.ID)
	require.NoError(t, err)
	a, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	t.Logf("agent reincarnation_state=%q; record state=%q updated_at initial=%v now=%v",
		a.ReincarnationState, cur.State, initial.UpdatedAt, cur.UpdatedAt)
	assert.True(t, cur.UpdatedAt.After(initial.UpdatedAt),
		"worker has passed the stopping and provisioning steps but the record's updated_at was never bumped")
	assert.Equal(t, store.AgentReincarnationStateProvisioning, cur.State,
		"the record's own state must track the worker's progress, not stay pending")
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

// TestReincarnateAgent_SweptWorkerFailureDoesNotClobberNewClaim is the design
// §3.4 Amendment A6 (R2) regression test: once the sweep has failed a
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
// design §3.4 Amendment A6.5 (R3-1) regression test: the sweep must not
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
// Amendment A6.6 (R3-2) regression test: the agent-state backstop must key
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
			// design §3.4 Amendment A5.4 (F4): the auto-no-auth check reads
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

			// design §3.4 Amendment A5.4 (F4): assign the SAME GCPIdentity on
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
			// design §3.4 Amendment A3.5 (R4) kept fields: stale them too, so
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
			created.AppliedConfig.AgentRoleGrandfathered = keptAgentRoleGrandfathered
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
			assert.Equal(t, keptWorkspaceStoragePath, fresh.WorkspaceStoragePath, "WorkspaceStoragePath must be kept from the live row, not reset")
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
