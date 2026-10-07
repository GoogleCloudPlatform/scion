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
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin the resolution precedence for harness_config:
//
//	explicit agent-create request > project annotation > template > hub default
//
// where "template" means only the template's declared harness_config /
// default_harness_config — never its bare harness type (ptone/scion#601
// item 2).
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

func (d *capturingDispatcher) DispatchAgentCreate(ctx context.Context, agent *store.Agent) (*CreateDispatchResult, error) {
	d.dispatched = true
	if agent.AppliedConfig != nil {
		d.dispatchedHarnessConfig = agent.AppliedConfig.HarnessConfig
	}
	return d.createAgentDispatcher.DispatchAgentCreate(ctx, agent)
}

func (d *capturingDispatcher) DispatchAgentCreateWithGather(ctx context.Context, agent *store.Agent) (*CreateDispatchResult, error) {
	if _, err := d.DispatchAgentCreate(ctx, agent); err != nil {
		return nil, err
	}
	return envReqsResult(d.envReqs), nil
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
// DefaultHarnessConfig, only a bare Harness type, which since
// ptone/scion#601 item 2 contributes nothing to the harness-config slot.
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

// TestCreateAgent_ProjectHarnessConfigBeatsTemplateHarnessOnlyFallback: a
// template with no declared harness-config, only a bare Harness type. The
// project annotation supplies the value (since ptone/scion#601 item 2 the
// harness type is never a candidate at all, so this is now simply the
// project rung filling an otherwise-empty slot).
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
		"project annotation fills the slot; the template's harness type is not a candidate")
}

// TestCreateAgent_TemplateHarnessTypeNotUsedAsHarnessConfig flips the old
// pin (ptone/scion#601 item 2, decision (a)): a template that declares no
// harness_config/default_harness_config contributes nothing to the
// harness-config slot — its bare Harness type ("claude") is a harness type,
// not a harness-config slug. With no project or hub default either, the slot
// stays empty and broker-side resolution decides.
func TestCreateAgent_TemplateHarnessTypeNotUsedAsHarnessConfig(t *testing.T) {
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
	assert.Empty(t, agent.AppliedConfig.HarnessConfig,
		"the template's harness type must not be used as a harness-config name")
	assert.Empty(t, agent.AppliedConfig.HarnessConfigID)
}

// TestCreateAgent_HubDefaultHarnessConfigBeatsNameInferredTemplateHarness is
// the concrete harm #601 item 2 removes: Template.Harness is inferred from the
// template NAME for any template whose name contains claude/gemini/opencode/
// codex. That inferred type used to fill the harness-config slot before
// applyHubAgentDefaults, so the hub operator's
// agent_defaults.default_harness_config silently lost. Now the hub default
// wins.
func TestCreateAgent_HubDefaultHarnessConfigBeatsNameInferredTemplateHarness(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	const tmplName = "team-claude-reviewer"
	inferred := inferHarnessFromName(tmplName)
	require.NotEmpty(t, inferred, "fixture: the template name must trigger harness inference")
	createHarnessOnlyTemplate(t, s, tmplName, inferred)
	setHubAgentDefaults(srv, opsettings.AgentDefaultsSettings{DefaultHarnessConfig: "hub-hc"})

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "prec-hub-beats-inferred",
		ProjectID: project.ID,
		Template:  tmplName,
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	agent, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, agent.AppliedConfig)
	assert.Equal(t, "hub-hc", agent.AppliedConfig.HarnessConfig,
		"the hub default must beat a name-inferred template harness type")
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
// Not-found logging: level tracks provenance
// ---------------------------------------------------------------------------

// levelCapturingHandler records the level and message of every log record that
// passes the level filter, so a test can assert on how loudly something was
// logged rather than only whether it happened.
type levelCapturingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *levelCapturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *levelCapturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *levelCapturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *levelCapturingHandler) WithGroup(string) slog.Handler      { return h }

// harnessNotFoundRecords returns the captured records for the
// harness-config-not-found message.
func (h *levelCapturingHandler) harnessNotFoundRecords() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []slog.Record
	for _, r := range h.records {
		if strings.Contains(r.Message, "harness config not found") {
			out = append(out, r)
		}
	}
	return out
}

// captureHarnessLogs swaps the server's agent-lifecycle logger for a capturing
// one and returns the handler.
func captureHarnessLogs(srv *Server) *levelCapturingHandler {
	h := &levelCapturingHandler{}
	srv.agentLifecycleLog = slog.New(h)
	return h
}

// TestCreateAgent_UnresolvableProjectHarnessConfigWarns pins the WARN half of
// the provenance rule. A project annotation naming a harness config that does
// not exist is an operator-fixable misconfiguration that displaced a known-good
// template value, so it must be loud.
func TestCreateAgent_UnresolvableProjectHarnessConfigWarns(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	logs := captureHarnessLogs(srv)

	createHarnessTemplate(t, s, "tmpl-harness", "template-harness")
	setProjectHarnessConfigAnnotation(t, s, project, "does-not-exist")

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "log-project-unresolvable",
		ProjectID: project.ID,
		Template:  "tmpl-harness",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	found := logs.harnessNotFoundRecords()
	require.Len(t, found, 1, "expected exactly one not-found log record")
	assert.Equal(t, slog.LevelWarn, found[0].Level,
		"an unresolvable project annotation must warn — it displaced the template")
}

// TestCreateAgent_TemplateHarnessTypeProducesNoNotFoundLog replaces the old
// "DEBUG, not WARN" pin. Before ptone/scion#601 item 2 the template's bare
// Harness type was looked up as a harness-config slug and, finding none,
// logged not-found at DEBUG on nearly every create. It is no longer a
// candidate name, so there is nothing to look up and nothing to log.
func TestCreateAgent_TemplateHarnessTypeProducesNoNotFoundLog(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	logs := captureHarnessLogs(srv)

	createHarnessOnlyTemplate(t, s, "tmpl-bare", "claude")

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "log-template-harness-type",
		ProjectID: project.ID,
		Template:  "tmpl-bare",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	assert.Empty(t, logs.harnessNotFoundRecords(),
		"a harness type is not a harness-config name, so no lookup and no not-found log")
}

// TestCreateAgent_UnresolvableTemplateDefaultHarnessConfigWarns covers
// ptone/scion#620's hub half: a template's explicit default_harness_config is
// a deliberate slug choice (unlike its bare Harness type), so when the hub has
// no record of it the not-found log must be at WARN, with template provenance.
// Dispatch still succeeds — observability only.
func TestCreateAgent_UnresolvableTemplateDefaultHarnessConfigWarns(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	logs := captureHarnessLogs(srv)

	createHarnessTemplate(t, s, "tmpl-missing-hc", "no-such-template-hc")

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "log-template-default-unresolvable",
		ProjectID: project.ID,
		Template:  "tmpl-missing-hc",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	found := logs.harnessNotFoundRecords()
	require.Len(t, found, 1, "expected exactly one not-found log record")
	assert.Equal(t, slog.LevelWarn, found[0].Level,
		"an unresolvable template default_harness_config must warn")
	fromTemplate, ok := recordAttr(found[0], "from_template_default")
	require.True(t, ok, "the log must carry the template provenance attribute")
	assert.True(t, fromTemplate.Bool())
	fromProject, ok := recordAttr(found[0], "from_project_annotation")
	require.True(t, ok)
	assert.False(t, fromProject.Bool())
}

// TestCreateAgent_RequestNamingTemplateDefaultIsNotTemplateProvenance guards
// that template provenance is carried, not inferred by name equality: a
// request that explicitly names the template's default slug is request
// provenance, and keeps the pre-existing (DEBUG) level.
func TestCreateAgent_RequestNamingTemplateDefaultIsNotTemplateProvenance(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	logs := captureHarnessLogs(srv)

	createHarnessTemplate(t, s, "tmpl-same-name", "same-name-hc")

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:          "log-request-same-as-template",
		ProjectID:     project.ID,
		Template:      "tmpl-same-name",
		HarnessConfig: "same-name-hc",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	found := logs.harnessNotFoundRecords()
	require.Len(t, found, 1)
	fromTemplate, ok := recordAttr(found[0], "from_template_default")
	require.True(t, ok)
	assert.False(t, fromTemplate.Bool(),
		"the request named it; template provenance must not be inferred from the name")
	assert.Equal(t, slog.LevelDebug, found[0].Level)
}

// ---------------------------------------------------------------------------
// active-profile: the same precedence chain, end to end
// ---------------------------------------------------------------------------

// TestCreateAgent_ProjectActiveProfileApplied is the behaviour change:
// scion.io/active-profile was parsed and persisted but never applied, so
// setting it in Project Settings had no effect on any agent.
func TestCreateAgent_ProjectActiveProfileApplied(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	setProjectAnnotations(t, s, project, map[string]string{
		projectSettingActiveProfile: "project-profile",
	})

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "prec-profile-project",
		ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	agent, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, agent.AppliedConfig)
	assert.Equal(t, "project-profile", agent.AppliedConfig.Profile,
		"the project's active-profile annotation must reach the dispatched agent")
}

// TestCreateAgent_RequestProfileBeatsProjectActiveProfile is the guard test.
// The request tier already worked before this change
// (TestCreateAgent_ProfileStoredInAppliedConfig); an unguarded write in
// applyProjectDefaults would silently clobber it.
func TestCreateAgent_RequestProfileBeatsProjectActiveProfile(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	setProjectAnnotations(t, s, project, map[string]string{
		projectSettingActiveProfile: "project-profile",
	})

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "prec-profile-request",
		ProjectID: project.ID,
		Profile:   "request-profile",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	agent, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, agent.AppliedConfig)
	assert.Equal(t, "request-profile", agent.AppliedConfig.Profile,
		"an explicit request profile must outrank the project's active-profile annotation")
}

// ---------------------------------------------------------------------------
// Scheduler dispatch path (server.go, dispatchAgentEventHandler)
// ---------------------------------------------------------------------------

// runDispatchAgentEvent fires a dispatch_agent scheduled event for the project
// and returns the created agent.
func runDispatchAgentEvent(t *testing.T, srv *Server, s store.Store, projectID, agentName, template string) *store.Agent {
	t.Helper()
	ctx := context.Background()
	if _, err := s.GetUser(ctx, DevUserID); err != nil {
		require.ErrorIs(t, err, store.ErrNotFound)
		require.NoError(t, s.CreateUser(ctx, &store.User{
			ID:          DevUserID,
			Email:       "dev@localhost",
			DisplayName: "Dev User",
			Role:        store.UserRoleAdmin,
		}))
	}

	payload, err := json.Marshal(DispatchAgentEventPayload{
		AgentName: agentName,
		Template:  template,
		Task:      "do the thing",
	})
	require.NoError(t, err)

	handler := srv.dispatchAgentEventHandler()
	require.NoError(t, handler(ctx, withSessionRevision(store.ScheduledEvent{
		ID:        tid("sched-" + agentName + "-" + t.Name()),
		ProjectID: projectID,
		EventType: "dispatch_agent",
		Payload:   string(payload),
		CreatedBy: DevUserID,
	}, DevUserID)))

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
// the scheduler-side counterpart for a harness-type-only template.
func TestSchedulerDispatch_ProjectHarnessConfigBeatsTemplateHarnessOnlyFallback(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)

	createHarnessOnlyTemplate(t, s, "tmpl-bare", "claude")
	setProjectHarnessConfigAnnotation(t, s, project, "project-harness")

	agent := runDispatchAgentEvent(t, srv, s, project.ID, "sched-harness-only-tmpl", "tmpl-bare")
	assert.Equal(t, "project-harness", agent.AppliedConfig.HarnessConfig,
		"project annotation fills the slot; the template's harness type is not a candidate")
}

// TestSchedulerDispatch_TemplateHarnessTypeNotUsedAsHarnessConfig is the
// scheduler twin of TestCreateAgent_TemplateHarnessTypeNotUsedAsHarnessConfig
// (ptone/scion#601 item 2): a harness-type-only template contributes nothing
// to the harness-config slot on the scheduled-dispatch path either.
func TestSchedulerDispatch_TemplateHarnessTypeNotUsedAsHarnessConfig(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)

	createHarnessOnlyTemplate(t, s, "tmpl-bare", "claude")

	agent := runDispatchAgentEvent(t, srv, s, project.ID, "sched-harness-type-only", "tmpl-bare")
	assert.Empty(t, agent.AppliedConfig.HarnessConfig,
		"the template's harness type must not be used as a harness-config name")
	assert.Empty(t, agent.AppliedConfig.HarnessConfigID)
}

// TestSchedulerDispatch_HubDefaultHarnessConfigBeatsNameInferredTemplateHarness
// is the scheduler twin of
// TestCreateAgent_HubDefaultHarnessConfigBeatsNameInferredTemplateHarness.
func TestSchedulerDispatch_HubDefaultHarnessConfigBeatsNameInferredTemplateHarness(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)

	const tmplName = "sched-claude-reviewer"
	inferred := inferHarnessFromName(tmplName)
	require.NotEmpty(t, inferred, "fixture: the template name must trigger harness inference")
	createHarnessOnlyTemplate(t, s, tmplName, inferred)
	setHubAgentDefaults(srv, opsettings.AgentDefaultsSettings{DefaultHarnessConfig: "hub-hc"})

	agent := runDispatchAgentEvent(t, srv, s, project.ID, "sched-hub-beats-inferred", tmplName)
	assert.Equal(t, "hub-hc", agent.AppliedConfig.HarnessConfig,
		"the hub default must beat a name-inferred template harness type")
}

// TestSchedulerDispatch_UnresolvableTemplateDefaultHarnessConfigWarns is the
// scheduler twin of TestCreateAgent_UnresolvableTemplateDefaultHarnessConfigWarns
// (ptone/scion#620). The template provenance travels on ctx through
// deriveAgentConfig, so this pins it on the scheduled-dispatch path too.
func TestSchedulerDispatch_UnresolvableTemplateDefaultHarnessConfigWarns(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	logs := captureHarnessLogs(srv)

	createHarnessTemplate(t, s, "tmpl-missing-hc", "no-such-template-hc")

	agent := runDispatchAgentEvent(t, srv, s, project.ID, "sched-template-default-unresolvable", "tmpl-missing-hc")
	assert.Equal(t, "no-such-template-hc", agent.AppliedConfig.HarnessConfig)

	found := logs.harnessNotFoundRecords()
	require.Len(t, found, 1, "expected exactly one not-found log record")
	assert.Equal(t, slog.LevelWarn, found[0].Level,
		"an unresolvable template default_harness_config must warn on the scheduler path too")
	fromTemplate, ok := recordAttr(found[0], "from_template_default")
	require.True(t, ok)
	assert.True(t, fromTemplate.Bool())
}

// Note: the scheduler's dispatch_agent payload has no harness-config field, so
// the "explicit request value wins" case has no scheduler-side equivalent.
