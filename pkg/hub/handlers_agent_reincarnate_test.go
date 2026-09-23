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
		if containsAll(w, "dropped", "TEMPLATE_ENV_KEY", `old="v1"`, `new="v2"`) {
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
	// Block the worker from ever reaching Start, so the pending record stays
	// pending for the duration of the test.
	disp.stopErr = fmt.Errorf("network blip") // tolerated by the worker, doesn't block
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	self := agentIdentityFor(agent.ID, project.ID)

	// Manually seed a pending reincarnation, simulating one already in flight
	// (avoids a timing-dependent race against the real worker goroutine).
	require.NoError(t, s.CreateAgentReincarnation(context.Background(), &store.AgentReincarnation{
		AgentID:        agent.ID,
		FromGeneration: 1,
		ToGeneration:   2,
		State:          store.AgentReincarnationStatePending,
	}))

	req := reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"})
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, req, agent.ID)

	assert.Equal(t, http.StatusConflict, rec.Code)
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
	}{
		{
			name: "explicit beats everything",
			explicit: CreateAgentRequest{
				HarnessConfig: "explicit-hc",
				HarnessAuth:   "explicit-auth",
				Profile:       "explicit-profile",
				Config:        &api.ScionConfig{Model: "explicit-model", ThinkingLevel: tl(2)},
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
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			disp := newReincarnateTestDispatcher()
			srv, s, project, broker := setupReincarnateTestServer(t, disp)

			if len(tc.projectAnnotations) > 0 {
				project.Annotations = tc.projectAnnotations
				require.NoError(t, s.UpdateProject(context.Background(), project))
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
				require.NoError(t, s.CreateTemplate(context.Background(), template))
				templateSlug = template.Slug
			}

			// --- create side: the real create HTTP path. ---
			createReq := tc.explicit
			createReq.Name = "matrix-create-" + tidSlugSafe(tc.name)
			createReq.ProjectID = project.ID
			createReq.Template = templateSlug
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", createReq)
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
			var createResp CreateAgentResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &createResp))
			created, err := s.GetAgent(context.Background(), createResp.Agent.ID)
			require.NoError(t, err)
			require.NotNil(t, created.AppliedConfig)

			// --- reincarnate side: an agent on the same template/project,
			// whose CreateInputs mirror the same explicit request, but whose
			// LIVE AppliedConfig is deliberately stale — proving the fresh
			// values come from re-derivation, not from the old generation. ---
			var explicitThinkingLevel *int
			if tc.explicit.Config != nil {
				explicitThinkingLevel = tc.explicit.Config.ThinkingLevel
			}
			gcpIdentity := &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModeAssign, ServiceAccountID: "kept-sa"}
			agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
				a.Template = templateSlug
				a.AppliedConfig.HarnessConfig = "stale-hc-from-old-generation"
				a.AppliedConfig.HarnessAuth = "stale-auth"
				a.AppliedConfig.Model = "stale-model"
				a.AppliedConfig.Profile = "stale-profile"
				a.AppliedConfig.ThinkingLevel = tl(99)
				a.AppliedConfig.GCPIdentity = gcpIdentity
				a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{
					HarnessConfig: tc.explicit.HarnessConfig,
					HarnessAuth:   tc.explicit.HarnessAuth,
					Profile:       tc.explicit.Profile,
					ThinkingLevel: explicitThinkingLevel,
					InlineConfig:  tc.explicit.Config,
				}
			})

			fresh, _, err := srv.buildFreshAppliedConfig(context.Background(), agent, project)
			require.NoError(t, err)

			assert.Equal(t, created.AppliedConfig.HarnessConfig, fresh.HarnessConfig, "HarnessConfig must agree")
			assert.Equal(t, created.AppliedConfig.Model, fresh.Model, "Model must agree")
			assert.Equal(t, created.AppliedConfig.HarnessAuth, fresh.HarnessAuth, "HarnessAuth must agree")
			assert.Equal(t, created.AppliedConfig.NoAuth, fresh.NoAuth, "NoAuth must agree")
			assert.Equal(t, created.AppliedConfig.Profile, fresh.Profile, "Profile must agree")
			if assert.Equal(t, created.AppliedConfig.ThinkingLevel == nil, fresh.ThinkingLevel == nil, "ThinkingLevel nilness must agree") &&
				created.AppliedConfig.ThinkingLevel != nil {
				assert.Equal(t, *created.AppliedConfig.ThinkingLevel, *fresh.ThinkingLevel, "ThinkingLevel must agree")
			}
			assert.Equal(t, gcpIdentity, fresh.GCPIdentity, "GCPIdentity must be kept unchanged")
		})
	}
}
