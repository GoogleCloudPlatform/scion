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
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin the hub operational agent_defaults ladder rungs (Gap 2b,
// design §3.2.2). Two fields are resolved hub-side because the hub has to stamp
// TemplateID/TemplateHash and HarnessConfigID/HarnessConfigHash for a remote
// broker to hydrate the bundles:
//
//	default_template        — joins the template ladder, below request and
//	                          below the project annotation
//	default_harness_config  — applied by applyHubAgentDefaults, strictly between
//	                          applyProjectDefaults and populateAgentConfig
//
// The other four agent_defaults fields are deliberately absent from both: they
// must land BELOW the template, which nothing stamped hub-side can do.

// setHubAgentDefaults sets the hub operational agent_defaults on a test server,
// simulating what ApplySnapshot does in Postgres mode. Writes under s.mu, the
// same lock hubAgentDefaults() reads under.
func setHubAgentDefaults(srv *Server, d opsettings.AgentDefaultsSettings) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	srv.config.AgentDefaults = d
}

// recordsContaining returns captured log records whose message contains sub.
func (h *levelCapturingHandler) recordsContaining(sub string) []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []slog.Record
	for _, r := range h.records {
		if strings.Contains(r.Message, sub) {
			out = append(out, r)
		}
	}
	return out
}

// hubDefaultTemplateWarnings returns the records emitted by
// warnHubDefaultTemplateUnusable.
func (h *levelCapturingHandler) hubDefaultTemplateWarnings() []slog.Record {
	return h.recordsContaining("hub operational default_template is unusable")
}

// recordAttr reads a single attribute off a captured record.
func recordAttr(r slog.Record, key string) (slog.Value, bool) {
	var found slog.Value
	var ok bool
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			found, ok = a.Value, true
			return false
		}
		return true
	})
	return found, ok
}

// ---------------------------------------------------------------------------
// applyHubAgentDefaults — the unit-level rank contract
// ---------------------------------------------------------------------------

// TestApplyHubAgentDefaults_HarnessConfig is the table the design asks for. All
// four cases reduce to the same only-if-empty rule; what differs is which tier
// put the incumbent value in the slot, and the case names record that, because
// the ordering guarantee is "the hub tier is below all three of them".
func TestApplyHubAgentDefaults_HarnessConfig(t *testing.T) {
	const hubDefault = "hub-wide-hc"

	tests := []struct {
		name string
		// incumbent is what the higher tier already placed in the slot by the
		// time applyHubAgentDefaults runs.
		incumbent   string
		want        string
		wantApplied bool
	}{
		{
			// buildAppliedConfig copies req.HarnessConfig (or req.Config.HarnessConfig)
			// into the slot long before applyProjectDefaults.
			name:        "LosesToRequest",
			incumbent:   "requested-hc",
			want:        "requested-hc",
			wantApplied: false,
		},
		{
			// scion.io/default-harness-config, applied inline on the create path
			// and by applyProjectDefaults on the scheduler path.
			name:        "LosesToProjectAnnotation",
			incumbent:   "project-annotation-hc",
			want:        "project-annotation-hc",
			wantApplied: false,
		},
		{
			// getHarnessConfigFromTemplate's value, stamped by the template rung
			// on both paths before applyProjectDefaults runs.
			name:        "LosesToTemplate",
			incumbent:   "template-hc",
			want:        "template-hc",
			wantApplied: false,
		},
		{
			name:        "AppliesWhenAllEmpty",
			incumbent:   "",
			want:        hubDefault,
			wantApplied: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ac := &store.AgentAppliedConfig{HarnessConfig: tt.incumbent}
			applied := applyHubAgentDefaults(ac, opsettings.AgentDefaultsSettings{
				DefaultHarnessConfig: hubDefault,
			})
			assert.Equal(t, tt.want, ac.HarnessConfig)
			assert.Equal(t, tt.wantApplied, applied,
				"the return value is the provenance signal populateAgentConfig logs from")
		})
	}
}

// TestApplyHubAgentDefaults_Model is the table the design asks for.
// Analogous to TestApplyHubAgentDefaults_HarnessConfig: the hub operational
// DefaultModel fills ac.Model only when no higher tier has set it.
func TestApplyHubAgentDefaults_Model(t *testing.T) {
	const hubDefault = "medium"

	tests := []struct {
		name      string
		incumbent string
		want      string
	}{
		{
			name:      "LosesToRequest",
			incumbent: "large",
			want:      "large",
		},
		{
			name:      "LosesToProjectAnnotation",
			incumbent: "small",
			want:      "small",
		},
		{
			name:      "LosesToTemplate",
			incumbent: "opus",
			want:      "opus",
		},
		{
			name:      "AppliesWhenEmpty",
			incumbent: "",
			want:      hubDefault,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ac := &store.AgentAppliedConfig{Model: tt.incumbent}
			applied := applyHubAgentDefaults(ac, opsettings.AgentDefaultsSettings{
				DefaultModel: hubDefault,
			})
			assert.Equal(t, tt.want, ac.Model)
			assert.False(t, applied,
				"the return value tracks harness-config provenance only; model must not affect it")
		})
	}
}

// TestApplyHubAgentDefaults_ModelEmptyDefault verifies that an empty hub
// DefaultModel leaves ac.Model untouched.
func TestApplyHubAgentDefaults_ModelEmptyDefault(t *testing.T) {
	ac := &store.AgentAppliedConfig{}
	applied := applyHubAgentDefaults(ac, opsettings.AgentDefaultsSettings{})
	assert.Empty(t, ac.Model)
	assert.False(t, applied)
}

// TestApplyHubAgentDefaults_ThinkingLevel verifies the only-if-nil guard for
// DefaultThinkingLevel.
func TestApplyHubAgentDefaults_ThinkingLevel(t *testing.T) {
	hubLevel := 3

	t.Run("AppliesWhenNil", func(t *testing.T) {
		ac := &store.AgentAppliedConfig{}
		applyHubAgentDefaults(ac, opsettings.AgentDefaultsSettings{
			DefaultThinkingLevel: &hubLevel,
		})
		require.NotNil(t, ac.ThinkingLevel)
		assert.Equal(t, 3, *ac.ThinkingLevel)
	})

	t.Run("LosesToPreSet", func(t *testing.T) {
		existing := 5
		ac := &store.AgentAppliedConfig{ThinkingLevel: &existing}
		applyHubAgentDefaults(ac, opsettings.AgentDefaultsSettings{
			DefaultThinkingLevel: &hubLevel,
		})
		require.NotNil(t, ac.ThinkingLevel)
		assert.Equal(t, 5, *ac.ThinkingLevel)
	})

	t.Run("NilDefaultDoesNothing", func(t *testing.T) {
		ac := &store.AgentAppliedConfig{}
		applyHubAgentDefaults(ac, opsettings.AgentDefaultsSettings{})
		assert.Nil(t, ac.ThinkingLevel)
	})
}

// TestApplyHubAgentDefaults_ModelDoesNotAffectHCProvenance verifies that
// applying only DefaultModel (no DefaultHarnessConfig) returns false, so
// callers do not set withHubDefaultHarnessConfig context spuriously.
func TestApplyHubAgentDefaults_ModelDoesNotAffectHCProvenance(t *testing.T) {
	ac := &store.AgentAppliedConfig{}
	applied := applyHubAgentDefaults(ac, opsettings.AgentDefaultsSettings{
		DefaultModel: "medium",
	})
	assert.Equal(t, "medium", ac.Model)
	assert.False(t, applied,
		"model-only application must not signal harness-config provenance")
}

// TestApplyHubAgentDefaults_IgnoresTheFourLimitFields is the A5 guard at unit
// level. The four limit/resource fields must not be stamped hub-side: doing so
// would send them to the broker as top-of-chain and let a hub-wide floor
// override a template's explicit value (design §3.2.1). They belong to a
// separate low-rank channel applied broker-side.
func TestApplyHubAgentDefaults_IgnoresTheFourLimitFields(t *testing.T) {
	ac := &store.AgentAppliedConfig{}
	applyHubAgentDefaults(ac, opsettings.AgentDefaultsSettings{
		DefaultMaxTurns:      50,
		DefaultMaxModelCalls: 200,
		DefaultMaxDuration:   "2h",
	})

	// AgentAppliedConfig has no turn/duration slots at all; InlineConfig is the
	// only place the four fields could have been smuggled through, and it is a
	// top-of-chain slot, so it must stay nil.
	assert.Nil(t, ac.InlineConfig,
		"nothing may be written into InlineConfig — it is a top-of-chain slot (alternative A5)")
	assert.Equal(t, store.AgentAppliedConfig{}, *ac,
		"none of the four limit/resource fields may be stamped hub-side")
}

func TestApplyHubAgentDefaults_NilAppliedConfig(t *testing.T) {
	assert.False(t, applyHubAgentDefaults(nil, opsettings.AgentDefaultsSettings{
		DefaultHarnessConfig: "hub-wide-hc",
	}))
}

// ---------------------------------------------------------------------------
// default_template — create path, and the degradation rule
// ---------------------------------------------------------------------------

// TestCreateAgent_HubDefaultTemplate_MissingTemplate_DoesNotFailCreate is the
// important one (design §5.2 risk (a)). resolveTemplate failing normally 404s
// the whole create. If a stale hub-wide default_template names a template that
// has since been deleted, that rule turns an operational setting into "nobody
// in this deployment can create an agent". So a failure whose name came from
// the hub default must warn and continue with no template.
func TestCreateAgent_HubDefaultTemplate_MissingTemplate_DoesNotFailCreate(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	logs := captureHarnessLogs(srv)
	setHubAgentDefaults(srv, opsettings.AgentDefaultsSettings{DefaultTemplate: "deleted-template"})

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "hub-default-ghost",
		ProjectID: project.ID,
		Task:      "do something",
	})
	require.Equal(t, http.StatusCreated, rec.Code,
		"a stale hub default_template must not 404 the create; body: %s", rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)

	agent, err := s.GetAgent(context.Background(), resp.Agent.ID)
	require.NoError(t, err)
	assert.Empty(t, agent.Template,
		"the unresolvable name must be cleared, not carried to the broker")

	warnings := logs.hubDefaultTemplateWarnings()
	require.Len(t, warnings, 1, "the degradation must be logged, not silent")
	assert.Equal(t, slog.LevelWarn, warnings[0].Level)
	name, ok := recordAttr(warnings[0], "template")
	require.True(t, ok, "the log must name the template so an operator can fix it")
	assert.Equal(t, "deleted-template", name.String())
}

// TestCreateAgent_RequestedTemplate_Missing_Still404s is the polarity control
// for the test above, and it is not optional: without it, an implementation
// that swallows *all* template-resolution errors passes the first test. Both,
// or neither counts.
func TestCreateAgent_RequestedTemplate_Missing_Still404s(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, _, project := setupCreateAgentServer(t, disp)
	logs := captureHarnessLogs(srv)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "requested-ghost",
		ProjectID: project.ID,
		Template:  "does-not-exist",
		Task:      "do something",
	})
	require.Equal(t, http.StatusNotFound, rec.Code,
		"a template named in the request must still 404; body: %s", rec.Body.String())

	assert.Empty(t, logs.hubDefaultTemplateWarnings(),
		"the hub-default degradation must not fire for a request-supplied name")
}

// erroringTemplateStore makes the first lookup in resolveTemplate fail with a
// non-ErrNotFound error, simulating a DB blip or network fault.
type erroringTemplateStore struct {
	store.Store
	err error
}

func (s *erroringTemplateStore) GetTemplate(context.Context, string) (*store.Template, error) {
	return nil, s.err
}

// TestCreateAgent_HubDefaultTemplate_StoreError_StillFailsHard is the boundary
// of the degradation rule, and the asymmetry is deliberate.
//
// The two exits that degrade are DETERMINISTIC: the template really is
// unusable, it will be unusable on the next create too, and the operator gets
// the same warning every time until they fix the setting. A store error is
// neither — it is transient, and it is an I-don't-know rather than a
// this-is-broken. A DB blip is no evidence that the hub default is stale.
// Degrading it would mean some creates silently get no template and others get
// one depending on store weather, which is harder to diagnose than the clean
// failure it replaced: the agent comes up looking fine and then behaves
// differently from its siblings for a reason its own record cannot explain.
//
// The same line is drawn by the pre-start hook resolution in
// handlers_agent_create_helpers.go ("The hub fallback is entered only on a
// definitive 'no project hook' (ErrNotFound). Any other project-lookup failure
// [...] is ambiguous"), so this is a house convention rather than a local call.
//
// If this test starts failing because someone "finished the job" and degraded
// all three exits, that is the bug, not this test.
func TestCreateAgent_HubDefaultTemplate_StoreError_StillFailsHard(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, _, project := setupCreateAgentServer(t, disp)
	logs := captureHarnessLogs(srv)
	srv.store = &erroringTemplateStore{Store: srv.store, err: errors.New("connection reset by peer")}
	setHubAgentDefaults(srv, opsettings.AgentDefaultsSettings{DefaultTemplate: "hub-tmpl"})

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "hub-default-store-error",
		ProjectID: project.ID,
		Task:      "do something",
	})
	assert.NotEqual(t, http.StatusCreated, rec.Code,
		"a store error must fail the create even when the name came from the hub default; body: %s",
		rec.Body.String())
	assert.Empty(t, logs.hubDefaultTemplateWarnings(),
		"a store error is not a stale-setting warning — it must not be reported as one")
}

// createPendingTemplate creates a template that resolves but is unusable: no
// files and no content hash, the state a template sits in between creation and
// `scion template sync`. This is the input to the file-less exit.
func createPendingTemplate(t *testing.T, s store.Store, slug string) *store.Template {
	t.Helper()
	tmpl := &store.Template{
		ID:      tid("template-pending-" + slug + "-" + t.Name()),
		Name:    slug,
		Slug:    slug,
		Harness: "claude",
		// No Files, no ContentHash — deliberately.
		Scope:  store.TemplateScopeGlobal,
		Status: "pending",
	}
	require.NoError(t, s.CreateTemplate(context.Background(), tmpl))
	return tmpl
}

// TestCreateAgent_PendingTemplate_Requested_Still400s is the pre-existing half
// of the file-less exit, and it is the one that needed a test most: Phase 7
// refactored that guard from a standalone `if resolvedTemplate != nil && ...`
// after the not-found block into a case of the same switch. The control flow is
// equivalent — the old guard's nil check made it mutually exclusive with the
// not-found branch anyway — but that was a reading standing in for a test, on a
// user-visible 400 this PR restructured. Now it is a test.
func TestCreateAgent_PendingTemplate_Requested_Still400s(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	logs := captureHarnessLogs(srv)
	createPendingTemplate(t, s, "pending-tmpl")

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "requested-pending",
		ProjectID: project.ID,
		Template:  "pending-tmpl",
		Task:      "do something",
	})
	require.Equal(t, http.StatusBadRequest, rec.Code,
		"a request-named pending template must still be rejected; body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "sync template files first",
		"the actionable remediation must survive the switch refactor")
	assert.Empty(t, logs.hubDefaultTemplateWarnings(),
		"the hub-default degradation must not fire for a request-supplied name")
}

// TestCreateAgent_HubDefaultTemplate_Pending_DoesNotFailCreate is the other
// polarity: the same unusable template, reached through the hub default, must
// degrade rather than 400. This is the "degrade #2" exit — a stale hub default
// naming a template nobody ever synced would otherwise block every create in the
// deployment, exactly as a deleted one would.
//
// Both, or neither counts — the same standard the brief set for the 404 pair.
func TestCreateAgent_HubDefaultTemplate_Pending_DoesNotFailCreate(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	logs := captureHarnessLogs(srv)
	createPendingTemplate(t, s, "pending-tmpl")
	setHubAgentDefaults(srv, opsettings.AgentDefaultsSettings{DefaultTemplate: "pending-tmpl"})

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "hub-default-pending",
		ProjectID: project.ID,
		Task:      "do something",
	})
	require.Equal(t, http.StatusCreated, rec.Code,
		"a stale hub default naming an unsynced template must not fail the create; body: %s",
		rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	agent, err := s.GetAgent(context.Background(), resp.Agent.ID)
	require.NoError(t, err)
	assert.Empty(t, agent.Template, "the unusable template must be cleared, not carried to the broker")

	warnings := logs.hubDefaultTemplateWarnings()
	require.Len(t, warnings, 1, "the degradation must be logged, not silent")
	reason, ok := recordAttr(warnings[0], "reason")
	require.True(t, ok)
	assert.Contains(t, reason.String(), "sync template files first",
		"the warning must carry the same actionable remediation the 400 would have")
}

// TestCreateAgent_ProjectAnnotationTemplate_Missing_Still404s extends the
// polarity control one tier down: the project annotation is also not the hub
// default, so it keeps the 404 too. This is what proves the provenance flag is
// set on the hub-default branch only.
func TestCreateAgent_ProjectAnnotationTemplate_Missing_Still404s(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	setProjectAnnotations(t, s, project, map[string]string{
		projectSettingDefaultTemplate: "annotation-ghost",
	})
	setHubAgentDefaults(srv, opsettings.AgentDefaultsSettings{DefaultTemplate: "deleted-template"})

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "annotation-ghost-agent",
		ProjectID: project.ID,
		Task:      "do something",
	})
	require.Equal(t, http.StatusNotFound, rec.Code,
		"the annotation outranks the hub default, so its failure keeps the 404; body: %s", rec.Body.String())
}

// TestCreateAgent_HubDefaultTemplate_AppliesWhenRequestAndAnnotationEmpty is the
// positive half: the rung must actually fire, otherwise every degradation test
// above passes vacuously.
func TestCreateAgent_HubDefaultTemplate_AppliesWhenRequestAndAnnotationEmpty(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	createHarnessTemplate(t, s, "hub-tmpl", "hub-tmpl-hc")
	setHubAgentDefaults(srv, opsettings.AgentDefaultsSettings{DefaultTemplate: "hub-tmpl"})

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "hub-default-applies",
		ProjectID: project.ID,
		Task:      "do something",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	agent, err := s.GetAgent(context.Background(), resp.Agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "hub-tmpl", agent.Template)
	require.NotNil(t, agent.AppliedConfig)
	assert.NotEmpty(t, agent.AppliedConfig.TemplateID,
		"the hub-resolved template must be stamped for broker hydration — that is why it is resolved hub-side")
}

// TestCreateAgent_HubDefaultTemplate_LosesToRequestAndAnnotation pins the rank:
// the hub default is the lowest tier on the template ladder.
func TestCreateAgent_HubDefaultTemplate_LosesToRequestAndAnnotation(t *testing.T) {
	t.Run("LosesToRequest", func(t *testing.T) {
		disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
		srv, s, project := setupCreateAgentServer(t, disp)
		createHarnessTemplate(t, s, "hub-tmpl", "hub-hc")
		createHarnessTemplate(t, s, "requested-tmpl", "requested-hc")
		setHubAgentDefaults(srv, opsettings.AgentDefaultsSettings{DefaultTemplate: "hub-tmpl"})

		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
			Name:      "req-beats-hub",
			ProjectID: project.ID,
			Template:  "requested-tmpl",
		})
		require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

		var resp CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		agent, err := s.GetAgent(context.Background(), resp.Agent.ID)
		require.NoError(t, err)
		assert.Equal(t, "requested-tmpl", agent.Template)
	})

	t.Run("LosesToProjectAnnotation", func(t *testing.T) {
		disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
		srv, s, project := setupCreateAgentServer(t, disp)
		createHarnessTemplate(t, s, "hub-tmpl", "hub-hc")
		createHarnessTemplate(t, s, "annotation-tmpl", "annotation-hc")
		setProjectAnnotations(t, s, project, map[string]string{
			projectSettingDefaultTemplate: "annotation-tmpl",
		})
		setHubAgentDefaults(srv, opsettings.AgentDefaultsSettings{DefaultTemplate: "hub-tmpl"})

		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
			Name:      "annotation-beats-hub",
			ProjectID: project.ID,
		})
		require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

		var resp CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		agent, err := s.GetAgent(context.Background(), resp.Agent.ID)
		require.NoError(t, err)
		assert.Equal(t, "annotation-tmpl", agent.Template)
	})
}

// ---------------------------------------------------------------------------
// default_harness_config — create path
// ---------------------------------------------------------------------------

// TestCreateAgent_HubDefaultHarnessConfig_AppliesAndStampsID is acceptance
// criterion 10's positive half. The "ID is not empty" assertion is what proves
// applyHubAgentDefaults runs BEFORE populateAgentConfig: called after it, the
// name would be set but nothing would ever be stamped, and a remote broker
// would have nothing to hydrate from.
func TestCreateAgent_HubDefaultHarnessConfig_AppliesAndStampsID(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	hc := &store.HarnessConfig{
		ID:          tid("hc-hub-default-" + t.Name()),
		Name:        "hub-wide-hc",
		Slug:        "hub-wide-hc",
		Harness:     "claude",
		ContentHash: "feedface",
		Scope:       store.HarnessConfigScopeGlobal,
	}
	require.NoError(t, s.CreateHarnessConfig(ctx, hc))
	setHubAgentDefaults(srv, opsettings.AgentDefaultsSettings{DefaultHarnessConfig: "hub-wide-hc"})

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "hub-hc-applies",
		ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	agent, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, agent.AppliedConfig)
	assert.Equal(t, "hub-wide-hc", agent.AppliedConfig.HarnessConfig)
	assert.Equal(t, hc.ID, agent.AppliedConfig.HarnessConfigID,
		"stamped ID proves applyHubAgentDefaults ran before populateAgentConfig")
	assert.Equal(t, "feedface", agent.AppliedConfig.HarnessConfigHash)
}

// TestCreateAgent_HubDefaultHarnessConfig_LosesToProjectAnnotation is acceptance
// criterion 10 in full: hub default X, project annotation Y, Y wins, AND Y's ID
// is stamped rather than left empty. The second half is the placement proof —
// called before applyProjectDefaults, the hub value would have won; called after
// populateAgentConfig, no ID would be stamped at all.
func TestCreateAgent_HubDefaultHarnessConfig_LosesToProjectAnnotation(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	hubHC := &store.HarnessConfig{
		ID:          tid("hc-hub-x-" + t.Name()),
		Name:        "hub-hc-x",
		Slug:        "hub-hc-x",
		Harness:     "claude",
		ContentHash: "aaaaaaaa",
		Scope:       store.HarnessConfigScopeGlobal,
	}
	require.NoError(t, s.CreateHarnessConfig(ctx, hubHC))
	projectHC := &store.HarnessConfig{
		ID:          tid("hc-project-y-" + t.Name()),
		Name:        "project-hc-y",
		Slug:        "project-hc-y",
		Harness:     "claude",
		ContentHash: "bbbbbbbb",
		Scope:       store.HarnessConfigScopeGlobal,
	}
	require.NoError(t, s.CreateHarnessConfig(ctx, projectHC))

	setProjectHarnessConfigAnnotation(t, s, project, "project-hc-y")
	setHubAgentDefaults(srv, opsettings.AgentDefaultsSettings{DefaultHarnessConfig: "hub-hc-x"})

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "annotation-beats-hub-hc",
		ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	agent, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, agent.AppliedConfig)
	assert.Equal(t, "project-hc-y", agent.AppliedConfig.HarnessConfig,
		"the project annotation outranks the hub operational default")
	assert.Equal(t, projectHC.ID, agent.AppliedConfig.HarnessConfigID,
		"criterion 10: the winning value's ID must be stamped, not left empty")
	assert.Equal(t, "bbbbbbbb", agent.AppliedConfig.HarnessConfigHash)
}

// TestCreateAgent_HubDefaultHarnessConfig_LosesToTemplate pins the third tier:
// the template's harness value is already in the slot by the time
// applyHubAgentDefaults runs, which is exactly why the call sits where it does.
func TestCreateAgent_HubDefaultHarnessConfig_LosesToTemplate(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)

	createHarnessTemplate(t, s, "tmpl-with-hc", "template-hc")
	setHubAgentDefaults(srv, opsettings.AgentDefaultsSettings{DefaultHarnessConfig: "hub-hc"})

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "template-beats-hub-hc",
		ProjectID: project.ID,
		Template:  "tmpl-with-hc",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	agent, err := s.GetAgent(context.Background(), resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, agent.AppliedConfig)
	assert.Equal(t, "template-hc", agent.AppliedConfig.HarnessConfig)
}

// TestCreateAgent_HubDefaultHarnessConfig_UnresolvableWarnsWithHubProvenance is
// the third provenance value the design asks for at
// handlers_agent_create_helpers.go. Before it, the not-found log could only say
// "from the project annotation" or "everything else", so an operator could not
// tell a bad hub default from a bad project annotation.
func TestCreateAgent_HubDefaultHarnessConfig_UnresolvableWarnsWithHubProvenance(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, _, project := setupCreateAgentServer(t, disp)
	logs := captureHarnessLogs(srv)
	setHubAgentDefaults(srv, opsettings.AgentDefaultsSettings{DefaultHarnessConfig: "no-such-hc"})

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "hub-hc-unresolvable",
		ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	found := logs.harnessNotFoundRecords()
	require.Len(t, found, 1)
	assert.Equal(t, slog.LevelWarn, found[0].Level,
		"a stale hub default costs every agent in the deployment its ID/hash — warn")

	fromHub, ok := recordAttr(found[0], "from_hub_default")
	require.True(t, ok, "the log must carry the new provenance attribute")
	assert.True(t, fromHub.Bool())
	fromProject, ok := recordAttr(found[0], "from_project_annotation")
	require.True(t, ok)
	assert.False(t, fromProject.Bool(),
		"the two provenances must be distinguishable, which is the point of adding a third value")
}

// TestCreateAgent_HubDefaultHarnessConfig_ProvenanceNotInferredFromTheSetting is
// the trap the design names: provenance must be carried, not recomputed. Here a
// user explicitly requests the very name the hub defaults to. Re-reading the
// setting would report from_hub_default=true; the carried flag reports false,
// because applyHubAgentDefaults never fired.
func TestCreateAgent_HubDefaultHarnessConfig_ProvenanceNotInferredFromTheSetting(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, _, project := setupCreateAgentServer(t, disp)
	logs := captureHarnessLogs(srv)
	setHubAgentDefaults(srv, opsettings.AgentDefaultsSettings{DefaultHarnessConfig: "same-name-hc"})

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:          "user-named-the-same-thing",
		ProjectID:     project.ID,
		HarnessConfig: "same-name-hc",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	found := logs.harnessNotFoundRecords()
	require.Len(t, found, 1)
	fromHub, ok := recordAttr(found[0], "from_hub_default")
	require.True(t, ok)
	assert.False(t, fromHub.Bool(),
		"the user named it; inferring provenance by re-reading the setting would misreport this")
}

// ---------------------------------------------------------------------------
// File mode — acceptance criterion 12
// ---------------------------------------------------------------------------

// TestCreateAgent_FileMode_NoHubDefaultRungFires is the file-mode-parity guard
// at the request boundary. In file mode BuildLayer1SnapshotFromFile leaves the
// agent-defaults fields zero, so ServerConfig.AgentDefaults is the zero value,
// so hubAgentDefaults() returns zero and neither rung can fire — even though
// the co-located broker's settings.yaml may well set both. That is what keeps
// the values at the BOTTOM of the broker chain instead of promoting them to the
// hub tier (design §3.2.4, rejected alternative A7).
//
// setupCreateAgentServer leaves AgentDefaults at its zero value, which is
// precisely the file-mode state, so this asserts on the real thing rather than
// a simulation of it.
func TestCreateAgent_FileMode_NoHubDefaultRungFires(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)

	require.Equal(t, opsettings.AgentDefaultsSettings{}, srv.hubAgentDefaults(),
		"file-mode precondition: no hub agent defaults are cached")

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "file-mode-agent",
		ProjectID: project.ID,
		Task:      "do something",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	agent, err := s.GetAgent(context.Background(), resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, agent.AppliedConfig)
	assert.Empty(t, agent.Template, "no template rung may fire in file mode")
	assert.Empty(t, agent.AppliedConfig.HarnessConfig, "no harness-config rung may fire in file mode")
	assert.Empty(t, agent.AppliedConfig.HarnessConfigID)
}

// ---------------------------------------------------------------------------
// Scheduler-dispatch path — the same two rungs (acceptance criterion 13)
// ---------------------------------------------------------------------------

// TestDispatchAgentEventHandler_HubDefaultTemplate_MissingTemplate_DoesNotFail is
// the scheduler mirror of the degradation rule. This path never 404s, so the
// harm it avoids is different: a name nothing can resolve must not be left on
// the agent record for the broker to try to hydrate.
func TestDispatchAgentEventHandler_HubDefaultTemplate_MissingTemplate_DoesNotFail(t *testing.T) {
	ms := newMockStore()
	ms.projects["project-1"] = &store.Project{ID: "project-1", Name: "test-project"}
	creatorID := seedFullRoleDispatchCreator(ms, "project-1")

	srv := newEventHandlerTestServer(ms)
	logs := captureHarnessLogs(srv)
	setHubAgentDefaults(srv, opsettings.AgentDefaultsSettings{DefaultTemplate: "deleted-template"})

	err := srv.dispatchAgentEventHandler()(context.Background(), store.ScheduledEvent{
		ID:        "dispatch-hub-ghost-1",
		ProjectID: "project-1",
		EventType: "dispatch_agent",
		Payload:   `{"agentName":"sched-hub-ghost","task":"Do the thing"}`,
		CreatedBy: creatorID,
	})
	require.NoError(t, err)

	created := findMockAgent(ms, "sched-hub-ghost")
	require.NotNil(t, created, "agent was not created")
	assert.Empty(t, created.Template,
		"the unresolvable hub-default name must be cleared, not dispatched")

	warnings := logs.hubDefaultTemplateWarnings()
	require.Len(t, warnings, 1, "the degradation must be logged on this path too")
	assert.Equal(t, slog.LevelWarn, warnings[0].Level)
}

// TestDispatchAgentEventHandler_PayloadTemplate_Missing_KeepsName is the
// scheduler polarity control. A name that came from the scheduled payload is
// left alone on a resolve failure — exactly as before this change — so a bug
// that clears every unresolvable template name fails here.
func TestDispatchAgentEventHandler_PayloadTemplate_Missing_KeepsName(t *testing.T) {
	ms := newMockStore()
	ms.projects["project-1"] = &store.Project{ID: "project-1", Name: "test-project"}
	creatorID := seedFullRoleDispatchCreator(ms, "project-1")

	srv := newEventHandlerTestServer(ms)
	logs := captureHarnessLogs(srv)
	setHubAgentDefaults(srv, opsettings.AgentDefaultsSettings{DefaultTemplate: "deleted-template"})

	err := srv.dispatchAgentEventHandler()(context.Background(), store.ScheduledEvent{
		ID:        "dispatch-payload-ghost-1",
		ProjectID: "project-1",
		EventType: "dispatch_agent",
		Payload:   `{"agentName":"sched-payload-ghost","template":"payload-tmpl","task":"Do the thing"}`,
		CreatedBy: creatorID,
	})
	require.NoError(t, err)

	created := findMockAgent(ms, "sched-payload-ghost")
	require.NotNil(t, created, "agent was not created")
	assert.Equal(t, "payload-tmpl", created.Template,
		"a payload-supplied name keeps the pre-change behaviour: the broker may resolve it locally")
	assert.Empty(t, logs.hubDefaultTemplateWarnings(),
		"the hub-default degradation must not fire for a payload-supplied name")
}

// erroringSchedulerStore is the scheduler-path counterpart of
// erroringTemplateStore.
type erroringSchedulerStore struct {
	*mockScheduledEventStore
	err error
}

func (s *erroringSchedulerStore) GetTemplate(context.Context, string) (*store.Template, error) {
	return nil, s.err
}

// TestDispatchAgentEventHandler_HubDefaultTemplate_StoreError_KeepsName is the
// scheduler mirror of the boundary. This path cannot "fail hard" — a resolve
// failure never fails a scheduled dispatch — so the analogue of failing hard is
// keeping the pre-existing behaviour: leave the name alone and let the broker
// try to resolve it locally. Clearing it on a transient blip would give exactly
// the intermittent silent divergence the create-path test above describes, with
// scheduled agents losing their template only on the dispatches that happened
// to hit a bad moment.
func TestDispatchAgentEventHandler_HubDefaultTemplate_StoreError_KeepsName(t *testing.T) {
	ms := newMockStore()
	ms.projects["project-1"] = &store.Project{ID: "project-1", Name: "test-project"}
	creatorID := seedFullRoleDispatchCreator(ms, "project-1")

	srv := newEventHandlerTestServer(&erroringSchedulerStore{
		mockScheduledEventStore: ms,
		err:                     errors.New("connection reset by peer"),
	})
	logs := captureHarnessLogs(srv)
	setHubAgentDefaults(srv, opsettings.AgentDefaultsSettings{DefaultTemplate: "hub-tmpl"})

	err := srv.dispatchAgentEventHandler()(context.Background(), store.ScheduledEvent{
		ID:        "dispatch-hub-store-error",
		ProjectID: "project-1",
		EventType: "dispatch_agent",
		Payload:   `{"agentName":"sched-hub-store-error","task":"Do the thing"}`,
		CreatedBy: creatorID,
	})
	require.NoError(t, err)

	created := findMockAgent(ms, "sched-hub-store-error")
	require.NotNil(t, created, "agent was not created")
	assert.Equal(t, "hub-tmpl", created.Template,
		"a store error is not evidence the setting is stale — keep the name, as before")
	assert.Empty(t, logs.hubDefaultTemplateWarnings(),
		"a store error must not be reported as a stale-setting warning")
}

// TestDispatchAgentEventHandler_HubDefaultTemplate_Applies is the scheduler
// mirror of the positive rung. It uses resolvingTemplateStore, which arms the
// panic trap documented above mockScheduledEventStore's stubs — the harness
// getter is stubbed, so the path is survivable; do not remove those stubs.
func TestDispatchAgentEventHandler_HubDefaultTemplate_Applies(t *testing.T) {
	ms := newMockStore()
	ms.projects["project-1"] = &store.Project{ID: "project-1", Name: "test-project"}
	creatorID := seedFullRoleDispatchCreator(ms, "project-1")

	srv := newEventHandlerTestServer(&resolvingTemplateStore{ms})
	setHubAgentDefaults(srv, opsettings.AgentDefaultsSettings{DefaultTemplate: "hub-tmpl"})

	err := srv.dispatchAgentEventHandler()(context.Background(), store.ScheduledEvent{
		ID:        "dispatch-hub-tmpl-1",
		ProjectID: "project-1",
		EventType: "dispatch_agent",
		Payload:   `{"agentName":"sched-hub-tmpl","task":"Do the thing"}`,
		CreatedBy: creatorID,
	})
	require.NoError(t, err)

	created := findMockAgent(ms, "sched-hub-tmpl")
	require.NotNil(t, created, "agent was not created")
	assert.Equal(t, "hub-tmpl", created.Template,
		"both payload.Template and agent.Template must be set by the rung")
	require.NotNil(t, created.AppliedConfig)
	assert.Equal(t, "claude", created.AppliedConfig.HarnessConfig,
		"the resolved template's harness value still wins over any hub default")
}

// TestDispatchAgentEventHandler_HubDefaultTemplate_LosesToPayloadAndAnnotation
// pins the rank on the scheduler path.
func TestDispatchAgentEventHandler_HubDefaultTemplate_LosesToPayloadAndAnnotation(t *testing.T) {
	t.Run("LosesToPayload", func(t *testing.T) {
		ms := newMockStore()
		ms.projects["project-1"] = &store.Project{ID: "project-1", Name: "test-project"}
		creatorID := seedFullRoleDispatchCreator(ms, "project-1")
		srv := newEventHandlerTestServer(&resolvingTemplateStore{ms})
		setHubAgentDefaults(srv, opsettings.AgentDefaultsSettings{DefaultTemplate: "hub-tmpl"})

		err := srv.dispatchAgentEventHandler()(context.Background(), store.ScheduledEvent{
			ID:        "dispatch-rank-payload",
			ProjectID: "project-1",
			EventType: "dispatch_agent",
			Payload:   `{"agentName":"sched-rank-payload","template":"payload-tmpl"}`,
			CreatedBy: creatorID,
		})
		require.NoError(t, err)

		created := findMockAgent(ms, "sched-rank-payload")
		require.NotNil(t, created)
		assert.Equal(t, "payload-tmpl", created.Template)
	})

	t.Run("LosesToProjectAnnotation", func(t *testing.T) {
		ms := newMockStore()
		ms.projects["project-1"] = &store.Project{
			ID:          "project-1",
			Name:        "test-project",
			Annotations: map[string]string{projectSettingDefaultTemplate: "annotation-tmpl"},
		}
		creatorID := seedFullRoleDispatchCreator(ms, "project-1")
		srv := newEventHandlerTestServer(&resolvingTemplateStore{ms})
		setHubAgentDefaults(srv, opsettings.AgentDefaultsSettings{DefaultTemplate: "hub-tmpl"})

		err := srv.dispatchAgentEventHandler()(context.Background(), store.ScheduledEvent{
			ID:        "dispatch-rank-annotation",
			ProjectID: "project-1",
			EventType: "dispatch_agent",
			Payload:   `{"agentName":"sched-rank-annotation"}`,
			CreatedBy: creatorID,
		})
		require.NoError(t, err)

		created := findMockAgent(ms, "sched-rank-annotation")
		require.NotNil(t, created)
		assert.Equal(t, "annotation-tmpl", created.Template)
	})
}

// resolvingHarnessConfigStore mirrors the existing resolvingTemplateStore: the
// one method change that lets populateAgentConfig actually stamp an ID, so a
// test can assert on the stamp rather than only on the name.
//
// The plain mockScheduledEventStore returns ErrNotFound here, which is enough to
// prove the NAME reaches the slot but says nothing about WHEN — the hub rung
// could run after populateAgentConfig and the name would still be on the record.
// Only a stamped ID distinguishes the two.
type resolvingHarnessConfigStore struct {
	*mockScheduledEventStore
}

func (r *resolvingHarnessConfigStore) GetHarnessConfigBySlug(_ context.Context, slug, _, _ string) (*store.HarnessConfig, error) {
	return &store.HarnessConfig{
		ID:          "hc-resolvable",
		Name:        slug,
		Slug:        slug,
		Harness:     "claude",
		ContentHash: "cafebabe",
		Scope:       store.HarnessConfigScopeGlobal,
	}, nil
}

// TestDispatchAgentEventHandler_HubDefaultHarnessConfig_Applies is the scheduler
// mirror of the harness-config rung: same function, same placement, strictly
// between applyProjectDefaults and populateAgentConfig.
//
// The HarnessConfigID assertion is the half that pins "strictly BEFORE
// populateAgentConfig" on this path. Sink the call below populateAgentConfig and
// the name is still set on the record — only the stamp disappears, and with it a
// remote broker's ability to hydrate the config. Without this assertion that
// inversion ships green (review B1, mutation M5).
func TestDispatchAgentEventHandler_HubDefaultHarnessConfig_Applies(t *testing.T) {
	ms := newMockStore()
	ms.projects["project-1"] = &store.Project{ID: "project-1", Name: "test-project"}
	creatorID := seedFullRoleDispatchCreator(ms, "project-1")

	srv := newEventHandlerTestServer(&resolvingHarnessConfigStore{ms})
	setHubAgentDefaults(srv, opsettings.AgentDefaultsSettings{DefaultHarnessConfig: "hub-wide-hc"})

	err := srv.dispatchAgentEventHandler()(context.Background(), store.ScheduledEvent{
		ID:        "dispatch-hub-hc-1",
		ProjectID: "project-1",
		EventType: "dispatch_agent",
		Payload:   `{"agentName":"sched-hub-hc","task":"Do the thing"}`,
		CreatedBy: creatorID,
	})
	require.NoError(t, err)

	created := findMockAgent(ms, "sched-hub-hc")
	require.NotNil(t, created)
	require.NotNil(t, created.AppliedConfig)
	assert.Equal(t, "hub-wide-hc", created.AppliedConfig.HarnessConfig)
	assert.Equal(t, "hc-resolvable", created.AppliedConfig.HarnessConfigID,
		"a stamped ID proves the rung ran BEFORE populateAgentConfig — criterion 10's second conjunct")
	assert.Equal(t, "cafebabe", created.AppliedConfig.HarnessConfigHash)
}

// TestDispatchAgentEventHandler_HubDefaultHarnessConfig_LosesToProjectAnnotation
// is criterion 10 on the scheduler path, and it is the test that pins "strictly
// AFTER applyProjectDefaults" for the whole change.
//
// It matters more here than the equivalent create-path test, because the two
// paths source the annotation differently: the create path reads
// scion.io/default-harness-config inline before the call, whereas THIS path gets
// it from applyProjectDefaults itself. So the scheduler is the only path where
// the ordering relative to applyProjectDefaults is load-bearing for the
// annotation — and it was the unpinned one. Hoist the call above
// applyProjectDefaults and the hub default silently beats every project's
// annotation on every scheduled dispatch (review B1, mutation M4).
//
// Both conjuncts of criterion 10 are asserted: Y wins, and Y's ID is stamped
// rather than left empty.
func TestDispatchAgentEventHandler_HubDefaultHarnessConfig_LosesToProjectAnnotation(t *testing.T) {
	ms := newMockStore()
	ms.projects["project-1"] = &store.Project{
		ID:          "project-1",
		Name:        "test-project",
		Annotations: map[string]string{projectSettingDefaultHarnessConfig: "project-hc-y"},
	}
	creatorID := seedFullRoleDispatchCreator(ms, "project-1")

	srv := newEventHandlerTestServer(&resolvingHarnessConfigStore{ms})
	setHubAgentDefaults(srv, opsettings.AgentDefaultsSettings{DefaultHarnessConfig: "hub-hc-x"})

	err := srv.dispatchAgentEventHandler()(context.Background(), store.ScheduledEvent{
		ID:        "dispatch-hub-hc-annotation",
		ProjectID: "project-1",
		EventType: "dispatch_agent",
		Payload:   `{"agentName":"sched-annotation-beats-hub"}`,
		CreatedBy: creatorID,
	})
	require.NoError(t, err)

	created := findMockAgent(ms, "sched-annotation-beats-hub")
	require.NotNil(t, created)
	require.NotNil(t, created.AppliedConfig)
	assert.Equal(t, "project-hc-y", created.AppliedConfig.HarnessConfig,
		"the project annotation outranks the hub operational default on this path too")
	assert.Equal(t, "hc-resolvable", created.AppliedConfig.HarnessConfigID,
		"criterion 10: the winning value's ID must be stamped, not left empty")
}

// TestDispatchAgentEventHandler_HubDefaultHarnessConfig_LosesToTemplate is the
// scheduler rank check for the harness-config rung.
func TestDispatchAgentEventHandler_HubDefaultHarnessConfig_LosesToTemplate(t *testing.T) {
	ms := newMockStore()
	ms.projects["project-1"] = &store.Project{ID: "project-1", Name: "test-project"}
	creatorID := seedFullRoleDispatchCreator(ms, "project-1")

	srv := newEventHandlerTestServer(&resolvingTemplateStore{ms})
	setHubAgentDefaults(srv, opsettings.AgentDefaultsSettings{DefaultHarnessConfig: "hub-wide-hc"})

	err := srv.dispatchAgentEventHandler()(context.Background(), store.ScheduledEvent{
		ID:        "dispatch-hub-hc-2",
		ProjectID: "project-1",
		EventType: "dispatch_agent",
		Payload:   `{"agentName":"sched-hub-hc-tmpl","template":"payload-tmpl"}`,
		CreatedBy: creatorID,
	})
	require.NoError(t, err)

	created := findMockAgent(ms, "sched-hub-hc-tmpl")
	require.NotNil(t, created)
	require.NotNil(t, created.AppliedConfig)
	assert.Equal(t, "claude", created.AppliedConfig.HarnessConfig,
		"the template's harness value is in the slot before applyHubAgentDefaults runs")
}

// TestDispatchAgentEventHandler_FileMode_NoHubDefaultRungFires is criterion 12
// on the scheduler path.
func TestDispatchAgentEventHandler_FileMode_NoHubDefaultRungFires(t *testing.T) {
	ms := newMockStore()
	ms.projects["project-1"] = &store.Project{ID: "project-1", Name: "test-project"}
	creatorID := seedFullRoleDispatchCreator(ms, "project-1")

	srv := newEventHandlerTestServer(ms)
	require.Equal(t, opsettings.AgentDefaultsSettings{}, srv.hubAgentDefaults(),
		"file-mode precondition: no hub agent defaults are cached")

	err := srv.dispatchAgentEventHandler()(context.Background(), store.ScheduledEvent{
		ID:        "dispatch-file-mode-1",
		ProjectID: "project-1",
		EventType: "dispatch_agent",
		Payload:   `{"agentName":"sched-file-mode","task":"Do the thing"}`,
		CreatedBy: creatorID,
	})
	require.NoError(t, err)

	created := findMockAgent(ms, "sched-file-mode")
	require.NotNil(t, created)
	assert.Empty(t, created.Template)
	require.NotNil(t, created.AppliedConfig)
	assert.Empty(t, created.AppliedConfig.HarnessConfig)
}

// findMockAgent returns the agent with the given slug from the mock store.
func findMockAgent(ms *mockScheduledEventStore, slug string) *store.Agent {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	for _, a := range ms.agents {
		if a.Slug == slug {
			return a
		}
	}
	return nil
}
