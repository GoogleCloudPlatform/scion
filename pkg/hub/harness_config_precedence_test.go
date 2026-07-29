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
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin the resolution precedence for harness_config:
//
//	explicit agent-create request > project annotation > template
//
// Historically the template beat the project annotation on both the
// agent-create path and the scheduler dispatch path, which made
// harness_config the odd one out among project settings (max_turns,
// max_model_calls, max_duration and resources have always been
// project-over-template). The project annotation now wins.

// capturingDispatcher records the AppliedConfig exactly as it is handed to the
// dispatcher. In production httpdispatcher overwrites
// AppliedConfig.HarnessConfig with the broker's resolved answer after dispatch,
// so an assertion on the value the hub decided must be made at dispatch time.
type capturingDispatcher struct {
	createAgentDispatcher
	dispatchedHarnessConfig string
	dispatched              bool
}

func (d *capturingDispatcher) DispatchAgentCreate(ctx context.Context, agent *store.Agent) error {
	d.dispatched = true
	if agent.AppliedConfig != nil {
		d.dispatchedHarnessConfig = agent.AppliedConfig.HarnessConfig
	}
	return d.createAgentDispatcher.DispatchAgentCreate(ctx, agent)
}

func (d *capturingDispatcher) DispatchAgentCreateWithGather(ctx context.Context, agent *store.Agent) (*RemoteEnvRequirementsResponse, error) {
	if err := d.DispatchAgentCreate(ctx, agent); err != nil {
		return nil, err
	}
	return d.envReqs, nil
}

// setProjectHarnessConfigAnnotation stamps the project-level
// default-harness-config annotation, exactly as PUT /settings would.
func setProjectHarnessConfigAnnotation(t *testing.T, s store.Store, project *store.Project, value string) {
	t.Helper()
	if project.Annotations == nil {
		project.Annotations = map[string]string{}
	}
	project.Annotations[projectSettingDefaultHarnessConfig] = value
	require.NoError(t, s.UpdateProject(context.Background(), project))
}

// setProjectAnnotations merges the given annotations into the project, exactly
// as PUT /settings would. Used where a test needs more than one setting.
func setProjectAnnotations(t *testing.T, s store.Store, project *store.Project, annotations map[string]string) {
	t.Helper()
	if project.Annotations == nil {
		project.Annotations = map[string]string{}
	}
	for k, v := range annotations {
		project.Annotations[k] = v
	}
	require.NoError(t, s.UpdateProject(context.Background(), project))
}

// createHarnessTemplate creates a global template whose DefaultHarnessConfig is
// set. ContentHash is populated so the agent-create handler's "template has no
// files" guard does not reject it.
func createHarnessTemplate(t *testing.T, s store.Store, slug, defaultHarnessConfig string) *store.Template {
	t.Helper()
	tmpl := &store.Template{
		ID:                   tid("template-" + slug + "-" + t.Name()),
		Name:                 slug,
		Slug:                 slug,
		Harness:              "claude",
		DefaultHarnessConfig: defaultHarnessConfig,
		ContentHash:          "d00dfeed",
		Scope:                store.TemplateScopeGlobal,
		Status:               "active",
	}
	require.NoError(t, s.CreateTemplate(context.Background(), tmpl))
	return tmpl
}

// createHarnessOnlyTemplate creates a global template with no
// DefaultHarnessConfig, so that getHarnessConfigFromTemplate's fallback to the
// bare Harness field is what supplies the template-tier value.
func createHarnessOnlyTemplate(t *testing.T, s store.Store, slug, harness string) *store.Template {
	t.Helper()
	tmpl := &store.Template{
		ID:          tid("template-" + slug + "-" + t.Name()),
		Name:        slug,
		Slug:        slug,
		Harness:     harness,
		ContentHash: "d00dfeed",
		Scope:       store.TemplateScopeGlobal,
		Status:      "active",
	}
	require.NoError(t, s.CreateTemplate(context.Background(), tmpl))
	return tmpl
}

// ---------------------------------------------------------------------------
// Agent-create path (handlers_agents_core.go)
// ---------------------------------------------------------------------------

// TestCreateAgent_ProjectHarnessConfigBeatsTemplate is the headline
// behaviour-change test: with both a project annotation and a template naming a
// different harness config, the project's value now wins (previously the
// template's did).
func TestCreateAgent_ProjectHarnessConfigBeatsTemplate(t *testing.T) {
	disp := &capturingDispatcher{createAgentDispatcher: createAgentDispatcher{createPhase: string(state.PhaseRunning)}}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	createHarnessTemplate(t, s, "tmpl-harness", "template-harness")
	setProjectHarnessConfigAnnotation(t, s, project, "project-harness")

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "prec-both-set",
		ProjectID: project.ID,
		Template:  "tmpl-harness",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	// Asserted at dispatch time: this is the value the hub resolved, before
	// any broker-side resolution could overwrite it.
	assert.True(t, disp.dispatched, "agent should have been dispatched")
	assert.Equal(t, "project-harness", disp.dispatchedHarnessConfig,
		"AppliedConfig handed to the dispatcher should carry the project's harness config")

	agent, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, agent.AppliedConfig)
	assert.Equal(t, "project-harness", agent.AppliedConfig.HarnessConfig,
		"project annotation must outrank the template's default harness config")
}

// TestCreateAgent_ProjectHarnessConfigNoTemplate: annotation set, no template.
func TestCreateAgent_ProjectHarnessConfigNoTemplate(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	setProjectHarnessConfigAnnotation(t, s, project, "project-harness")

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "prec-project-only",
		ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	agent, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, agent.AppliedConfig)
	assert.Equal(t, "project-harness", agent.AppliedConfig.HarnessConfig)
}

// TestCreateAgent_TemplateHarnessConfigWhenNoProjectAnnotation is the
// regression guard for the unchanged case: no annotation, template wins.
func TestCreateAgent_TemplateHarnessConfigWhenNoProjectAnnotation(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	createHarnessTemplate(t, s, "tmpl-harness", "template-harness")

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "prec-template-only",
		ProjectID: project.ID,
		Template:  "tmpl-harness",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	agent, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, agent.AppliedConfig)
	assert.Equal(t, "template-harness", agent.AppliedConfig.HarnessConfig,
		"with no project annotation the template's harness config still applies")
}

// TestCreateAgent_RequestHarnessConfigBeatsProjectAndTemplate: unchanged —
// an explicit request value outranks everything.
func TestCreateAgent_RequestHarnessConfigBeatsProjectAndTemplate(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	createHarnessTemplate(t, s, "tmpl-harness", "template-harness")
	setProjectHarnessConfigAnnotation(t, s, project, "project-harness")

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:          "prec-request-wins",
		ProjectID:     project.ID,
		Template:      "tmpl-harness",
		HarnessConfig: "request-harness",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	agent, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, agent.AppliedConfig)
	assert.Equal(t, "request-harness", agent.AppliedConfig.HarnessConfig,
		"an explicit request value outranks both the project annotation and the template")
}

// TestCreateAgent_ProjectHarnessConfigBeatsProjectDefaultTemplate covers the
// configuration a user actually produces by filling in both fields of the
// Project Settings form: the template arrives from the project's own
// scion.io/default-template annotation rather than from the request, and the
// harness config from scion.io/default-harness-config. This is the most likely
// real-world shape of the population whose behaviour changed.
func TestCreateAgent_ProjectHarnessConfigBeatsProjectDefaultTemplate(t *testing.T) {
	disp := &capturingDispatcher{createAgentDispatcher: createAgentDispatcher{createPhase: string(state.PhaseRunning)}}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	createHarnessTemplate(t, s, "tmpl-harness", "template-harness")
	setProjectAnnotations(t, s, project, map[string]string{
		projectSettingDefaultTemplate:      "tmpl-harness",
		projectSettingDefaultHarnessConfig: "project-harness",
	})

	// No Template in the request — it comes from the project annotation.
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "prec-both-from-project",
		ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	assert.True(t, disp.dispatched, "agent should have been dispatched")
	assert.Equal(t, "project-harness", disp.dispatchedHarnessConfig,
		"annotation-supplied template must not displace the project's harness config")

	agent, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, agent.AppliedConfig)
	assert.Equal(t, "project-harness", agent.AppliedConfig.HarnessConfig)
	assert.Equal(t, "tmpl-harness", agent.Template,
		"the template itself still applies — only harness_config is overridden")
}

// TestCreateAgent_ProjectHarnessConfigBeatsTemplateHarnessOnlyFallback pins the
// precedence against the *other* branch of getHarnessConfigFromTemplate: a
// template with no DefaultHarnessConfig, whose bare Harness field supplies the
// template-tier value. The project annotation must still win.
func TestCreateAgent_ProjectHarnessConfigBeatsTemplateHarnessOnlyFallback(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	createHarnessOnlyTemplate(t, s, "tmpl-bare", "claude")
	setProjectHarnessConfigAnnotation(t, s, project, "project-harness")

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "prec-harness-only-tmpl",
		ProjectID: project.ID,
		Template:  "tmpl-bare",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	agent, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, agent.AppliedConfig)
	assert.Equal(t, "project-harness", agent.AppliedConfig.HarnessConfig,
		"project annotation outranks the template's Harness-field fallback too")
}

// TestCreateAgent_TemplateHarnessOnlyFallbackWhenNoProjectAnnotation is the
// matching regression guard: with no annotation, the Harness-field fallback
// still supplies the value.
func TestCreateAgent_TemplateHarnessOnlyFallbackWhenNoProjectAnnotation(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	createHarnessOnlyTemplate(t, s, "tmpl-bare", "claude")

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "prec-harness-only-tmpl-noann",
		ProjectID: project.ID,
		Template:  "tmpl-bare",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	agent, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, agent.AppliedConfig)
	assert.Equal(t, "claude", agent.AppliedConfig.HarnessConfig)
}

// TestCreateAgent_ProjectHarnessConfigStampsIDWhenResolvable and its
// counterpart below pin the ID/hash consequence of making the project
// annotation load-bearing. When the annotation names a harness config that
// exists in Hub storage, populateAgentConfig stamps its ID and content hash so
// a remote broker can hydrate the bundle.
func TestCreateAgent_ProjectHarnessConfigStampsIDWhenResolvable(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	hc := &store.HarnessConfig{
		ID:          tid("hc-project-harness-" + t.Name()),
		Name:        "project-harness",
		Slug:        "project-harness",
		Harness:     "claude",
		ContentHash: "beefcafe",
		Scope:       store.HarnessConfigScopeGlobal,
	}
	require.NoError(t, s.CreateHarnessConfig(ctx, hc))

	createHarnessTemplate(t, s, "tmpl-harness", "template-harness")
	setProjectHarnessConfigAnnotation(t, s, project, "project-harness")

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "prec-hc-resolvable",
		ProjectID: project.ID,
		Template:  "tmpl-harness",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	agent, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, agent.AppliedConfig)
	assert.Equal(t, "project-harness", agent.AppliedConfig.HarnessConfig)
	assert.Equal(t, hc.ID, agent.AppliedConfig.HarnessConfigID,
		"a resolvable project harness config must be stamped for broker hydration")
	assert.Equal(t, "beefcafe", agent.AppliedConfig.HarnessConfigHash)
}

// TestCreateAgent_ProjectHarnessConfigUnresolvableLeavesIDEmpty documents the
// downside of D-2 flagged in review: an annotation naming a harness config that
// does not exist in Hub storage still displaces the template's known-good
// value, and the agent goes out with no ID or hash. Agent creation deliberately
// does NOT fail — the broker may still resolve the name from its own search
// path — but the hub logs a warning, and this test pins the shape so a future
// change to make it an error is a visible, deliberate decision.
func TestCreateAgent_ProjectHarnessConfigUnresolvableLeavesIDEmpty(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	createHarnessTemplate(t, s, "tmpl-harness", "template-harness")
	setProjectHarnessConfigAnnotation(t, s, project, "does-not-exist")

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "prec-hc-unresolvable",
		ProjectID: project.ID,
		Template:  "tmpl-harness",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	agent, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, agent.AppliedConfig)
	assert.Equal(t, "does-not-exist", agent.AppliedConfig.HarnessConfig,
		"the annotation displaces the template even when it names nothing that exists")
	assert.Empty(t, agent.AppliedConfig.HarnessConfigID,
		"no ID can be stamped, so a remote broker cannot hydrate from Hub storage")
	assert.Empty(t, agent.AppliedConfig.HarnessConfigHash)
}

// ---------------------------------------------------------------------------
// Scheduler dispatch path (server.go, dispatchAgentEventHandler)
// ---------------------------------------------------------------------------

// runDispatchAgentEvent fires a dispatch_agent scheduled event for the project
// and returns the created agent.
func runDispatchAgentEvent(t *testing.T, srv *Server, s store.Store, projectID, agentName, template string) *store.Agent {
	t.Helper()
	ctx := context.Background()

	payload, err := json.Marshal(DispatchAgentEventPayload{
		AgentName: agentName,
		Template:  template,
		Task:      "do the thing",
	})
	require.NoError(t, err)

	handler := srv.dispatchAgentEventHandler()
	require.NoError(t, handler(ctx, store.ScheduledEvent{
		ID:        tid("sched-" + agentName + "-" + t.Name()),
		ProjectID: projectID,
		EventType: "dispatch_agent",
		Payload:   string(payload),
	}))

	agent, err := s.GetAgentBySlug(ctx, projectID, agentName)
	require.NoError(t, err)
	require.NotNil(t, agent)
	require.NotNil(t, agent.AppliedConfig)
	return agent
}

// TestSchedulerDispatch_ProjectHarnessConfigBeatsTemplate mirrors the headline
// create-path case. Site 2 is where the only-if-unset guard inside
// applyProjectDefaults was previously dead code, because the template's value
// was stamped unconditionally before it ran.
func TestSchedulerDispatch_ProjectHarnessConfigBeatsTemplate(t *testing.T) {
	disp := &capturingDispatcher{createAgentDispatcher: createAgentDispatcher{createPhase: string(state.PhaseRunning)}}
	srv, s, project := setupCreateAgentServer(t, disp)

	createHarnessTemplate(t, s, "tmpl-harness", "template-harness")
	setProjectHarnessConfigAnnotation(t, s, project, "project-harness")

	agent := runDispatchAgentEvent(t, srv, s, project.ID, "sched-both-set", "tmpl-harness")
	assert.Equal(t, "project-harness", agent.AppliedConfig.HarnessConfig,
		"scheduler dispatch must honour the project annotation over the template")
}

// TestSchedulerDispatch_ProjectHarnessConfigNoTemplate: annotation set, no template.
func TestSchedulerDispatch_ProjectHarnessConfigNoTemplate(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)

	setProjectHarnessConfigAnnotation(t, s, project, "project-harness")

	agent := runDispatchAgentEvent(t, srv, s, project.ID, "sched-project-only", "")
	assert.Equal(t, "project-harness", agent.AppliedConfig.HarnessConfig)
}

// TestSchedulerDispatch_TemplateHarnessConfigWhenNoProjectAnnotation is the
// scheduler-side regression guard.
func TestSchedulerDispatch_TemplateHarnessConfigWhenNoProjectAnnotation(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)

	createHarnessTemplate(t, s, "tmpl-harness", "template-harness")

	agent := runDispatchAgentEvent(t, srv, s, project.ID, "sched-template-only", "tmpl-harness")
	assert.Equal(t, "template-harness", agent.AppliedConfig.HarnessConfig,
		"with no project annotation the template's harness config still applies")
}

// TestSchedulerDispatch_AppliedConfigHandedToDispatcher asserts the value the
// hub resolved at the moment it reaches the dispatcher, rather than whatever it
// may be rewritten to afterwards.
func TestSchedulerDispatch_AppliedConfigHandedToDispatcher(t *testing.T) {
	disp := &capturingDispatcher{createAgentDispatcher: createAgentDispatcher{createPhase: string(state.PhaseRunning)}}
	srv, s, project := setupCreateAgentServer(t, disp)

	createHarnessTemplate(t, s, "tmpl-harness", "template-harness")
	setProjectHarnessConfigAnnotation(t, s, project, "project-harness")

	runDispatchAgentEvent(t, srv, s, project.ID, "sched-dispatch-capture", "tmpl-harness")

	assert.True(t, disp.dispatched, "agent should have been dispatched")
	assert.Equal(t, "project-harness", disp.dispatchedHarnessConfig,
		"AppliedConfig handed to the dispatcher should carry the project's harness config")
}

// TestSchedulerDispatch_ProjectHarnessConfigBeatsProjectDefaultTemplate mirrors
// the create-path NB-2 case: the template arrives from the project's
// scion.io/default-template annotation rather than from the event payload. This
// exercises the default-template block immediately above the changed hunk in
// dispatchAgentEventHandler, which is the scheduler's counterpart to
// handlers_agents_core.go's.
func TestSchedulerDispatch_ProjectHarnessConfigBeatsProjectDefaultTemplate(t *testing.T) {
	disp := &capturingDispatcher{createAgentDispatcher: createAgentDispatcher{createPhase: string(state.PhaseRunning)}}
	srv, s, project := setupCreateAgentServer(t, disp)

	createHarnessTemplate(t, s, "tmpl-harness", "template-harness")
	setProjectAnnotations(t, s, project, map[string]string{
		projectSettingDefaultTemplate:      "tmpl-harness",
		projectSettingDefaultHarnessConfig: "project-harness",
	})

	// Empty template in the payload — it comes from the project annotation.
	agent := runDispatchAgentEvent(t, srv, s, project.ID, "sched-both-from-project", "")
	assert.Equal(t, "project-harness", agent.AppliedConfig.HarnessConfig,
		"annotation-supplied template must not displace the project's harness config")
	assert.Equal(t, "tmpl-harness", agent.Template,
		"the template itself still applies — only harness_config is overridden")
	assert.Equal(t, "project-harness", disp.dispatchedHarnessConfig,
		"value handed to the dispatcher must match the persisted one")
}

// TestSchedulerDispatch_ProjectHarnessConfigBeatsTemplateHarnessOnlyFallback is
// the scheduler-side counterpart to the Harness-field fallback case.
func TestSchedulerDispatch_ProjectHarnessConfigBeatsTemplateHarnessOnlyFallback(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)

	createHarnessOnlyTemplate(t, s, "tmpl-bare", "claude")
	setProjectHarnessConfigAnnotation(t, s, project, "project-harness")

	agent := runDispatchAgentEvent(t, srv, s, project.ID, "sched-harness-only-tmpl", "tmpl-bare")
	assert.Equal(t, "project-harness", agent.AppliedConfig.HarnessConfig,
		"project annotation outranks the template's Harness-field fallback too")
}

// Note: the scheduler's dispatch_agent payload has no harness-config field, so
// the "explicit request value wins" case has no scheduler-side equivalent.
