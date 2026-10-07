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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Hub-level tests for the ExplicitTimezone writers: create capture (HTTP
// and scheduled spawn), the agent PATCH, and reincarnate carry-forward.

// --- writer unit tests -----------------------------------------------------

func TestAdoptLegacyTZ(t *testing.T) {
	assert.False(t, adoptLegacyTZ(nil))

	ac := &store.AgentAppliedConfig{Env: map[string]string{"TZ": "Europe/Paris", "FOO": "bar"}, InlineConfig: &api.ScionConfig{Env: map[string]string{"TZ": "Asia/Tokyo"}}}
	assert.True(t, adoptLegacyTZ(ac))
	assert.Equal(t, "Europe/Paris", ac.ExplicitTimezone, "Env wins over a diverged InlineConfig copy")
	assert.True(t, ac.ExplicitTimezoneLegacy)
	assert.NotContains(t, ac.Env, "TZ")
	assert.NotContains(t, ac.InlineConfig.Env, "TZ")
	assert.Equal(t, "bar", ac.Env["FOO"])
	assert.False(t, adoptLegacyTZ(ac), "adoption is idempotent")

	pinned := &store.AgentAppliedConfig{ExplicitTimezone: "UTC", Env: map[string]string{"TZ": "Europe/Paris"}}
	adoptLegacyTZ(pinned)
	assert.Equal(t, "UTC", pinned.ExplicitTimezone, "an existing pin is never overwritten")
	assert.False(t, pinned.ExplicitTimezoneLegacy)
	assert.NotContains(t, pinned.Env, "TZ")

	unpinned := &store.AgentAppliedConfig{ExplicitTimezoneUnpinned: true, Env: map[string]string{"TZ": "Europe/Paris"}}
	adoptLegacyTZ(unpinned)
	assert.Empty(t, unpinned.ExplicitTimezone, "an unpin tombstone blocks re-pinning")
	assert.NotContains(t, unpinned.Env, "TZ")

	empty := &store.AgentAppliedConfig{Env: map[string]string{"TZ": ""}}
	adoptLegacyTZ(empty)
	assert.Empty(t, empty.ExplicitTimezone)
	assert.NotContains(t, empty.Env, "TZ", "an empty TZ record is stripped too")
}

func TestCaptureCreateTZ(t *testing.T) {
	ac := &store.AgentAppliedConfig{Env: map[string]string{"TZ": "Europe/Paris"}}
	captureCreateTZ(ac, "Asia/Tokyo")
	assert.Equal(t, "Europe/Paris", ac.ExplicitTimezone, "the agent's env beats the harness config")
	assert.False(t, ac.ExplicitTimezoneLegacy, "a create capture is an explicit pin")
	assert.NotContains(t, ac.Env, "TZ")

	hcOnly := &store.AgentAppliedConfig{}
	captureCreateTZ(hcOnly, "Asia/Tokyo")
	assert.Equal(t, "Asia/Tokyo", hcOnly.ExplicitTimezone)

	none := &store.AgentAppliedConfig{}
	captureCreateTZ(none, "")
	assert.Empty(t, none.ExplicitTimezone)

	kept := &store.AgentAppliedConfig{ExplicitTimezone: "UTC", Env: map[string]string{"TZ": "Europe/Paris"}}
	captureCreateTZ(kept, "Asia/Tokyo")
	assert.Equal(t, "UTC", kept.ExplicitTimezone)
	assert.NotContains(t, kept.Env, "TZ")

	unpinned := &store.AgentAppliedConfig{ExplicitTimezoneUnpinned: true, InlineConfig: &api.ScionConfig{Env: map[string]string{"TZ": "Europe/Paris"}}}
	captureCreateTZ(unpinned, "Asia/Tokyo")
	assert.Empty(t, unpinned.ExplicitTimezone)
	assert.NotContains(t, unpinned.InlineConfig.Env, "TZ")
}

func TestApplyExplicitTimezoneEdit(t *testing.T) {
	ac := &store.AgentAppliedConfig{ExplicitTimezone: "Europe/Paris", ExplicitTimezoneLegacy: true}
	changed, err := applyExplicitTimezoneEdit(ac, "Asia/Kathmandu")
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, "Asia/Kathmandu", ac.ExplicitTimezone)
	assert.False(t, ac.ExplicitTimezoneLegacy, "a PATCH pin is explicit")
	assert.False(t, ac.ExplicitTimezoneUnpinned)

	changed, err = applyExplicitTimezoneEdit(ac, "")
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Empty(t, ac.ExplicitTimezone)
	assert.True(t, ac.ExplicitTimezoneUnpinned)

	for _, bad := range []string{"Local", "Mars/Olympus", "not a zone"} {
		before := *ac
		_, err := applyExplicitTimezoneEdit(ac, bad)
		assert.Error(t, err, bad)
		assert.Equal(t, before, *ac, "a rejected value changes nothing")
	}
}

// --- create capture: HTTP and scheduled spawn ------------------------------

func createTZTemplate(t *testing.T, s store.Store, slug, hcName string, env map[string]string) {
	t.Helper()
	require.NoError(t, s.CreateTemplate(context.Background(), &store.Template{
		ID:                   tid("template-" + slug + "-" + t.Name()),
		Name:                 slug,
		Slug:                 slug,
		Harness:              "claude",
		DefaultHarnessConfig: hcName,
		ContentHash:          "d00dfeed",
		Scope:                store.TemplateScopeGlobal,
		Status:               "active",
		Config:               &store.TemplateConfig{Harness: "claude", Env: env},
	}))
}

func createTZHarnessConfig(t *testing.T, s store.Store, name, tz string) {
	t.Helper()
	require.NoError(t, s.CreateHarnessConfig(context.Background(), &store.HarnessConfig{
		ID:          tid("hc-" + name + "-" + t.Name()),
		Name:        name,
		Slug:        name,
		Harness:     "claude",
		ContentHash: "hchash-" + name,
		Scope:       store.HarnessConfigScopeGlobal,
		Config:      &store.HarnessConfigData{Harness: "claude", Env: map[string]string{"TZ": tz}},
	}))
}

// TestCreateAgent_CapturesTimezone covers writer (a) on the HTTP create
// path: the first non-empty of request env, template env and harness-config
// env becomes an explicit pin and leaves the env records (AC8, I2).
func TestCreateAgent_CapturesTimezone(t *testing.T) {
	tests := []struct {
		name      string
		reqEnv    map[string]string
		tmplEnv   map[string]string
		hcTZ      string
		wantPin   string
		noTmplReq bool
	}{
		{name: "request config env", reqEnv: map[string]string{"TZ": "Europe/Paris"}, tmplEnv: map[string]string{"TZ": "Asia/Tokyo"}, hcTZ: "America/Denver", wantPin: "Europe/Paris"},
		{name: "template env beats harness config", tmplEnv: map[string]string{"TZ": "Asia/Tokyo"}, hcTZ: "America/Denver", wantPin: "Asia/Tokyo"},
		{name: "harness config", hcTZ: "America/Denver", wantPin: "America/Denver"},
		{name: "no TZ anywhere", wantPin: ""},
		{name: "no template, request env", reqEnv: map[string]string{"TZ": "Asia/Kathmandu"}, noTmplReq: true, wantPin: "Asia/Kathmandu"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
			srv, s, project := setupCreateAgentServer(t, disp)
			req := CreateAgentRequest{Name: "tz-create", ProjectID: project.ID, Task: "do something"}
			if !tt.noTmplReq {
				hcName := ""
				if tt.hcTZ != "" {
					hcName = "tz-hc"
					createTZHarnessConfig(t, s, hcName, tt.hcTZ)
				}
				createTZTemplate(t, s, "tz-tmpl", hcName, tt.tmplEnv)
				req.Template = "tz-tmpl"
			}
			if tt.reqEnv != nil {
				req.Config = &api.ScionConfig{Env: tt.reqEnv}
			}
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", req)
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

			var resp CreateAgentResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			agent, err := s.GetAgent(context.Background(), resp.Agent.ID)
			require.NoError(t, err)
			ac := agent.AppliedConfig
			require.NotNil(t, ac)
			assert.Equal(t, tt.wantPin, ac.ExplicitTimezone)
			assert.False(t, ac.ExplicitTimezoneLegacy)
			assert.Empty(t, legacyEnvTZ(ac), "TZ must not persist in the env records")
			if tt.reqEnv != nil {
				// CreateInputs is copied before the capture and keeps the
				// requested TZ for replay; the carried pin wins at reincarnate.
				require.NotNil(t, ac.CreateInputs)
				require.NotNil(t, ac.CreateInputs.InlineConfig)
				assert.Equal(t, tt.reqEnv["TZ"], ac.CreateInputs.InlineConfig.Env["TZ"])
			}
			if disp.capturedAgent != nil {
				assert.Equal(t, tt.wantPin, disp.capturedAgent.AppliedConfig.ExplicitTimezone, "the pin is in place before dispatch")
			}
		})
	}
}

// tzTemplateStore resolves any template slug to a global template whose
// config env carries TZ, for the scheduled-spawn create path.
type tzTemplateStore struct {
	*mockScheduledEventStore
}

func (r *tzTemplateStore) GetTemplateBySlug(_ context.Context, slug, _, _ string) (*store.Template, error) {
	return &store.Template{
		ID:          "tmpl-tz",
		Slug:        slug,
		Name:        slug,
		Harness:     "claude",
		ContentHash: "d00dfeed",
		Status:      "active",
		Scope:       store.TemplateScopeGlobal,
		Config:      &store.TemplateConfig{Harness: "claude", Env: map[string]string{"TZ": "Asia/Kathmandu"}},
	}, nil
}

// TestDispatchAgentEventHandler_CapturesTimezone covers writer (a) on the
// scheduled-spawn create path (AC8).
func TestDispatchAgentEventHandler_CapturesTimezone(t *testing.T) {
	ms := newMockStore()
	ms.projects["project-1"] = &store.Project{ID: "project-1", Name: "test-project"}
	creatorID := seedFullRoleDispatchCreator(ms, "project-1")
	srv := newEventHandlerTestServer(&tzTemplateStore{ms})

	err := srv.dispatchAgentEventHandler()(context.Background(), withMockAgentRevision(store.ScheduledEvent{
		ID:        "dispatch-tz-1",
		ProjectID: "project-1",
		EventType: "dispatch_agent",
		Payload:   `{"agentName":"sched-tz","template":"tz-tmpl","task":"Do the thing"}`,
		CreatedBy: creatorID,
	}, creatorID))
	require.NoError(t, err)

	created := findMockAgent(ms, "sched-tz")
	require.NotNil(t, created)
	require.NotNil(t, created.AppliedConfig)
	assert.Equal(t, "Asia/Kathmandu", created.AppliedConfig.ExplicitTimezone)
	assert.Empty(t, legacyEnvTZ(created.AppliedConfig))
}

// tzWarningDispatcher reports a hub-only env warning from the broker on
// create, as HTTPAgentDispatcher.applyBrokerResponse does.
type tzWarningDispatcher struct {
	createAgentDispatcher
}

func (d *tzWarningDispatcher) DispatchAgentCreateWithGather(ctx context.Context, agent *store.Agent) (*CreateDispatchResult, error) {
	addDispatchWarnings(ctx, "Warning: TZ dropped by broker")
	return d.createAgentDispatcher.DispatchAgentCreateWithGather(ctx, agent)
}

// TestCreateAgent_RelaysDispatchWarnings checks broker warnings collected
// during dispatch reach the create response.
func TestCreateAgent_RelaysDispatchWarnings(t *testing.T) {
	disp := &tzWarningDispatcher{createAgentDispatcher{createPhase: string(state.PhaseRunning)}}
	srv, _, project := setupCreateAgentServer(t, disp)
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{Name: "tz-warn", ProjectID: project.ID, Task: "x"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Contains(t, resp.Warnings, "Warning: TZ dropped by broker")
}

// submitEnvWarningDispatcher reports a broker warning from finalize-env
// and returns an accepted launch. It removes the agent row first, so the
// hub's accepted-launch persist fails and adds its own warning.
type submitEnvWarningDispatcher struct {
	createAgentDispatcher
	st store.Store
}

func (d *submitEnvWarningDispatcher) DispatchFinalizeEnv(ctx context.Context, agent *store.Agent, _ map[string]string) (*CreateDispatchResult, error) {
	addDispatchWarnings(ctx, "Warning: TZ dropped by broker")
	if err := d.st.DeleteAgent(ctx, agent.ID); err != nil {
		return nil, err
	}
	return &CreateDispatchResult{Launch: &LaunchAccepted{ID: "launch-submit-warn"}}, nil
}

// TestSubmitAgentEnv_RelaysDispatchWarnings checks the submit-env response
// carries both the broker warnings collected during finalize-env and the
// hub's accepted-launch warnings.
func TestSubmitAgentEnv_RelaysDispatchWarnings(t *testing.T) {
	disp := &submitEnvWarningDispatcher{}
	srv, s, project := setupCreateAgentServer(t, disp)
	disp.st = s
	agent := &store.Agent{
		ID:              tid("agent-submit-warn"),
		Name:            "submit-warn",
		Slug:            "submit-warn",
		ProjectID:       project.ID,
		RuntimeBrokerID: project.DefaultRuntimeBrokerID,
		Phase:           string(state.PhaseProvisioning),
	}
	require.NoError(t, s.CreateAgent(context.Background(), agent))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents/submit-warn/env",
		SubmitEnvRequest{Env: map[string]string{"API_KEY": "v"}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Warnings, 2, "%v", resp.Warnings)
	assert.Equal(t, "Warning: TZ dropped by broker", resp.Warnings[0])
	assert.True(t, strings.HasPrefix(resp.Warnings[1], "Failed to update agent after launch was accepted: "), resp.Warnings[1])
}

// TestCreateAgent_EnvGatherNeverAsksForTZ checks the 202 env-gather
// response never lists TZ as a need and labels a hub-supplied TZ with the
// resolver's source (I4, labels).
func TestCreateAgent_EnvGatherNeverAsksForTZ(t *testing.T) {
	disp := &createAgentDispatcher{
		createPhase: string(state.PhaseProvisioning),
		envReqs: &RemoteEnvRequirementsResponse{
			Required:   []string{"TZ", "API_KEY"},
			HubHas:     []string{"TZ"},
			Needs:      []string{"TZ", "API_KEY"},
			SecretInfo: map[string]SecretKeyInfo{"TZ": {}, "API_KEY": {}},
		},
	}
	srv, _, project := setupCreateAgentServer(t, disp)
	setHubAgentDefaults(srv, opsettings.AgentDefaultsSettings{DefaultTimezone: "Asia/Tokyo"})
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{Name: "tz-gather", ProjectID: project.ID, Task: "x", GatherEnv: true})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.EnvGather)
	assert.Equal(t, []string{"API_KEY"}, resp.EnvGather.Needs)
	assert.NotContains(t, resp.EnvGather.SecretInfo, "TZ")
	require.Len(t, resp.EnvGather.HubHas, 1)
	assert.Equal(t, EnvSource{Key: "TZ", Scope: TZSourceHubDefault}, resp.EnvGather.HubHas[0])
}

// --- PATCH (writer (b)) ----------------------------------------------------

type tzPatchResponse struct {
	ResolvedTimezone *string  `json:"resolvedTimezone"`
	TimezoneSource   string   `json:"timezoneSource"`
	Warnings         []string `json:"warnings"`
	ID               string   `json:"id"`
}

func patchAgentTZ(t *testing.T, srv *Server, agentID string, body map[string]interface{}) (int, tzPatchResponse) {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/agents/"+agentID, body)
	var resp tzPatchResponse
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
		require.NotNil(t, resp.ResolvedTimezone, "resolvedTimezone must be on every PATCH response: %s", rec.Body.String())
		require.NotEmpty(t, resp.TimezoneSource)
		require.NotEmpty(t, resp.ID, "the agent is still the body of the response")
	}
	return rec.Code, resp
}

func tzPatchServer(t *testing.T, phase string, mutate func(a *store.Agent)) (*Server, store.Store, *store.Agent) {
	t.Helper()
	srv, s, project, broker := setupReincarnateTestServer(t, newReincarnateTestDispatcher())
	setHubAgentDefaults(srv, opsettings.AgentDefaultsSettings{DefaultTimezone: "Asia/Tokyo"})
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = phase
		if mutate != nil {
			mutate(a)
		}
	})
	return srv, s, agent
}

// TestPatchAgent_ExplicitTimezone covers AC10: pin, unpin, any phase, the
// next-start warning and the resolved zone on the response.
func TestPatchAgent_ExplicitTimezone(t *testing.T) {
	ctx := context.Background()

	t.Run("pin then unpin", func(t *testing.T) {
		srv, s, agent := tzPatchServer(t, string(state.PhaseCreated), nil)
		code, resp := patchAgentTZ(t, srv, agent.ID, map[string]interface{}{"explicitTimezone": "Asia/Kathmandu"})
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, "Asia/Kathmandu", *resp.ResolvedTimezone)
		assert.Equal(t, TZSourceExplicit, resp.TimezoneSource)
		assert.Empty(t, resp.Warnings)
		got, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		assert.Equal(t, "Asia/Kathmandu", got.AppliedConfig.ExplicitTimezone)
		assert.False(t, got.AppliedConfig.ExplicitTimezoneUnpinned)

		code, resp = patchAgentTZ(t, srv, agent.ID, map[string]interface{}{"explicitTimezone": ""})
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, "Asia/Tokyo", *resp.ResolvedTimezone)
		assert.Equal(t, TZSourceHubDefault, resp.TimezoneSource)
		got, err = s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		assert.Empty(t, got.AppliedConfig.ExplicitTimezone)
		assert.True(t, got.AppliedConfig.ExplicitTimezoneUnpinned)
	})

	t.Run("unpin with no rungs reports none", func(t *testing.T) {
		srv, _, agent := tzPatchServer(t, string(state.PhaseStopped), func(a *store.Agent) {
			a.AppliedConfig.ExplicitTimezone = "Europe/Paris"
		})
		setHubAgentDefaults(srv, opsettings.AgentDefaultsSettings{})
		code, resp := patchAgentTZ(t, srv, agent.ID, map[string]interface{}{"explicitTimezone": ""})
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, "", *resp.ResolvedTimezone)
		assert.Equal(t, TZSourceNone, resp.TimezoneSource)
	})

	t.Run("running agent gets the next-start warning", func(t *testing.T) {
		srv, s, agent := tzPatchServer(t, string(state.PhaseRunning), nil)
		code, resp := patchAgentTZ(t, srv, agent.ID, map[string]interface{}{"explicitTimezone": "Europe/Paris"})
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, []string{explicitTimezoneNextStartWarning}, resp.Warnings)
		got, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		assert.Equal(t, "Europe/Paris", got.AppliedConfig.ExplicitTimezone, "accepted in any phase")

		// Re-sending the same zone changes nothing, so it does not warn.
		code, resp = patchAgentTZ(t, srv, agent.ID, map[string]interface{}{"explicitTimezone": "Europe/Paris"})
		require.Equal(t, http.StatusOK, code)
		assert.Empty(t, resp.Warnings, "re-PATCHing the same zone gives no warning")
	})

	t.Run("next-start warning only when the zone changes under a live container", func(t *testing.T) {
		legacyParis := func(a *store.Agent) {
			a.AppliedConfig.ExplicitTimezone = "Europe/Paris"
			a.AppliedConfig.ExplicitTimezoneLegacy = true
		}
		pinnedTokyo := func(a *store.Agent) { a.AppliedConfig.ExplicitTimezone = "Asia/Tokyo" }
		for _, tc := range []struct {
			name   string
			phase  state.Phase
			mutate func(a *store.Agent)
			value  string
			warn   bool
		}{
			{name: "running", phase: state.PhaseRunning, value: "Europe/Paris", warn: true},
			{name: "starting", phase: state.PhaseStarting, value: "Europe/Paris", warn: true},
			{name: "cloning", phase: state.PhaseCloning, value: "Europe/Paris", warn: true},
			{name: "created", phase: state.PhaseCreated, value: "Europe/Paris"},
			{name: "provisioning", phase: state.PhaseProvisioning, value: "Europe/Paris"},
			{name: "suspended", phase: state.PhaseSuspended, value: "Europe/Paris"},
			{name: "stopping", phase: state.PhaseStopping, value: "Europe/Paris"},
			{name: "stopped", phase: state.PhaseStopped, value: "Europe/Paris"},
			{name: "error", phase: state.PhaseError, value: "Europe/Paris"},
			// Clears the legacy label only; the container zone is unchanged.
			{name: "running, same zone over a legacy pin", phase: state.PhaseRunning, mutate: legacyParis, value: "Europe/Paris"},
			{name: "running, new zone over a legacy pin", phase: state.PhaseRunning, mutate: legacyParis, value: "Asia/Kathmandu", warn: true},
			// The hub default is Asia/Tokyo, so unpinning Tokyo resolves the same zone.
			{name: "running, unpin to the same hub default zone", phase: state.PhaseRunning, mutate: pinnedTokyo, value: ""},
			{name: "running, pin equal to the hub default", phase: state.PhaseRunning, value: "Asia/Tokyo"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				srv, s, agent := tzPatchServer(t, string(tc.phase), tc.mutate)
				code, resp := patchAgentTZ(t, srv, agent.ID, map[string]interface{}{"explicitTimezone": tc.value})
				require.Equal(t, http.StatusOK, code)
				if tc.warn {
					assert.Equal(t, []string{explicitTimezoneNextStartWarning}, resp.Warnings)
				} else {
					assert.Empty(t, resp.Warnings)
				}
				got, err := s.GetAgent(ctx, agent.ID)
				require.NoError(t, err)
				assert.Equal(t, tc.value, got.AppliedConfig.ExplicitTimezone, "the edit is saved either way")
				assert.False(t, got.AppliedConfig.ExplicitTimezoneLegacy)
			})
		}
	})

	t.Run("invalid zone is rejected and changes nothing", func(t *testing.T) {
		for _, bad := range []string{"Local", "Mars/Olympus"} {
			srv, s, agent := tzPatchServer(t, string(state.PhaseCreated), func(a *store.Agent) {
				a.AppliedConfig.ExplicitTimezone = "Europe/Paris"
			})
			code, _ := patchAgentTZ(t, srv, agent.ID, map[string]interface{}{"explicitTimezone": bad, "taskSummary": "changed"})
			assert.Equal(t, http.StatusBadRequest, code, bad)
			got, err := s.GetAgent(ctx, agent.ID)
			require.NoError(t, err)
			assert.Equal(t, "Europe/Paris", got.AppliedConfig.ExplicitTimezone)
			assert.NotEqual(t, "changed", got.TaskSummary)
		}
	})

	t.Run("unrelated PATCH still reports the zone", func(t *testing.T) {
		srv, s, agent := tzPatchServer(t, string(state.PhaseRunning), nil)
		tzTestEnvVar(t, s, store.EnvVar{Scope: store.ScopeUser, ScopeID: agent.OwnerID, Value: "Europe/Lisbon"})
		code, resp := patchAgentTZ(t, srv, agent.ID, map[string]interface{}{"taskSummary": "hello"})
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, "Europe/Lisbon", *resp.ResolvedTimezone)
		assert.Equal(t, TZSourceUser, resp.TimezoneSource)
		assert.Empty(t, resp.Warnings)
	})

	t.Run("pin equal to the hub default stays a pin", func(t *testing.T) {
		srv, s, agent := tzPatchServer(t, string(state.PhaseCreated), nil)
		code, resp := patchAgentTZ(t, srv, agent.ID, map[string]interface{}{"explicitTimezone": "Asia/Tokyo"})
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, TZSourceExplicit, resp.TimezoneSource)
		setHubAgentDefaults(srv, opsettings.AgentDefaultsSettings{DefaultTimezone: "UTC"})
		got, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		assert.Equal(t, agentTZ{TZ: "Asia/Tokyo", Source: TZSourceExplicit}, srv.agentTZ(ctx, got))
	})

	t.Run("unadopted legacy agent re-saved with an unrelated env change keeps its zone", func(t *testing.T) {
		srv, s, agent := tzPatchServer(t, string(state.PhaseStopped), func(a *store.Agent) {
			a.AppliedConfig.Env = map[string]string{"TZ": "Europe/Paris"}
		})
		code, resp := patchAgentTZ(t, srv, agent.ID, map[string]interface{}{
			"config": map[string]interface{}{"env": map[string]interface{}{"FOO": "bar"}},
		})
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, "Europe/Paris", *resp.ResolvedTimezone)
		assert.Equal(t, TZSourceLegacy, resp.TimezoneSource)
		got, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		assert.Equal(t, "Europe/Paris", got.AppliedConfig.ExplicitTimezone)
		assert.Equal(t, "bar", got.AppliedConfig.Env["FOO"])
		assert.NotContains(t, got.AppliedConfig.Env, "TZ")
	})

	t.Run("legacy env TZ is adopted on PATCH", func(t *testing.T) {
		srv, s, agent := tzPatchServer(t, string(state.PhaseRunning), func(a *store.Agent) {
			a.AppliedConfig.InlineConfig = &api.ScionConfig{Env: map[string]string{"TZ": "Europe/London"}}
		})
		code, resp := patchAgentTZ(t, srv, agent.ID, map[string]interface{}{"taskSummary": "hello"})
		require.Equal(t, http.StatusOK, code)
		assert.Equal(t, "Europe/London", *resp.ResolvedTimezone)
		assert.Equal(t, TZSourceLegacy, resp.TimezoneSource)
		got, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		assert.Equal(t, "Europe/London", got.AppliedConfig.ExplicitTimezone)
		assert.True(t, got.AppliedConfig.ExplicitTimezoneLegacy)
		assert.Empty(t, legacyEnvTZ(got.AppliedConfig))
	})
}

// TestPatchAgent_ConfigEnvTZIgnored covers the config.env.TZ strip (I2): a
// non-empty TZ is dropped with a warning, an empty one silently, and neither
// reaches the env records or CreateInputs.
func TestPatchAgent_ConfigEnvTZIgnored(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		value    string
		wantWarn bool
	}{
		{name: "non-empty", value: "Europe/Paris", wantWarn: true},
		{name: "empty", value: "", wantWarn: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, agent := tzPatchServer(t, string(state.PhaseCreated), func(a *store.Agent) {
				a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{}
			})
			code, resp := patchAgentTZ(t, srv, agent.ID, map[string]interface{}{
				"config": map[string]interface{}{"env": map[string]interface{}{"TZ": tc.value, "FOO": "bar"}},
			})
			require.Equal(t, http.StatusOK, code)
			if tc.wantWarn {
				assert.Equal(t, []string{configEnvTZIgnoredWarning}, resp.Warnings)
			} else {
				assert.Empty(t, resp.Warnings)
			}
			assert.Equal(t, "Asia/Tokyo", *resp.ResolvedTimezone, "config.env.TZ never sets the zone")
			assert.Equal(t, TZSourceHubDefault, resp.TimezoneSource)

			got, err := s.GetAgent(ctx, agent.ID)
			require.NoError(t, err)
			ac := got.AppliedConfig
			assert.Empty(t, ac.ExplicitTimezone)
			assert.Empty(t, legacyEnvTZ(ac))
			assert.NotContains(t, ac.Env, "TZ")
			assert.Equal(t, "bar", ac.Env["FOO"], "other env keys still apply")
			require.NotNil(t, ac.CreateInputs.InlineConfig)
			assert.NotContains(t, ac.CreateInputs.InlineConfig.Env, "TZ", "an ignored TZ must not be recorded for reincarnate")

			project, err := s.GetProject(ctx, got.ProjectID)
			require.NoError(t, err)
			fresh, _, err := srv.buildFreshAppliedConfig(ctx, got, project, "")
			require.NoError(t, err)
			assert.Empty(t, fresh.ExplicitTimezone, "reincarnate must not turn the ignored TZ into a pin")
		})
	}
}

// --- reincarnate carry-forward (writer (d)) --------------------------------

// TestReincarnate_CarriesTimezone covers D5: a pin and an unpin survive
// reincarnate, and a legacy env TZ is adopted and keeps its label.
func TestReincarnate_CarriesTimezone(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name         string
		mutate       func(ac *store.AgentAppliedConfig)
		wantPin      string
		wantLegacy   bool
		wantUnpinned bool
	}{
		{
			name:    "pin",
			mutate:  func(ac *store.AgentAppliedConfig) { ac.ExplicitTimezone = "Asia/Kathmandu" },
			wantPin: "Asia/Kathmandu",
		},
		{
			name: "unpin tombstone blocks a create-input TZ",
			mutate: func(ac *store.AgentAppliedConfig) {
				ac.ExplicitTimezoneUnpinned = true
				ac.CreateInputs.InlineConfig = &api.ScionConfig{Env: map[string]string{"TZ": "Europe/Paris"}}
			},
			wantUnpinned: true,
		},
		{
			name: "legacy env TZ is adopted with its label",
			mutate: func(ac *store.AgentAppliedConfig) {
				ac.Env = map[string]string{"TZ": "Europe/London"}
				ac.InlineConfig = &api.ScionConfig{Env: map[string]string{"TZ": "Europe/London"}}
			},
			wantPin:    "Europe/London",
			wantLegacy: true,
		},
		{
			name: "no pin re-derives from CreateInputs",
			mutate: func(ac *store.AgentAppliedConfig) {
				ac.CreateInputs.InlineConfig = &api.ScionConfig{Env: map[string]string{"TZ": "Europe/Paris"}}
			},
			wantPin: "Europe/Paris",
		},
		{
			name: "persisted legacy adoption without CreateInputs keeps its label",
			mutate: func(ac *store.AgentAppliedConfig) {
				ac.CreateInputs = nil
				ac.ExplicitTimezone = "Europe/London"
				ac.ExplicitTimezoneLegacy = true
			},
			wantPin:    "Europe/London",
			wantLegacy: true,
		},
		{
			name: "legacy create-input TZ without CreateInputs",
			mutate: func(ac *store.AgentAppliedConfig) {
				ac.CreateInputs = nil
				ac.InlineConfig = &api.ScionConfig{Env: map[string]string{"TZ": "Europe/London"}}
			},
			wantPin:    "Europe/London",
			wantLegacy: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			disp := newReincarnateTestDispatcher()
			srv, s, project, broker := setupReincarnateTestServer(t, disp)
			agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
				tc.mutate(a.AppliedConfig)
			})
			fresh, _, err := srv.buildFreshAppliedConfig(ctx, agent, project, "")
			require.NoError(t, err)
			assert.Equal(t, tc.wantPin, fresh.ExplicitTimezone)
			assert.Equal(t, tc.wantLegacy, fresh.ExplicitTimezoneLegacy)
			assert.Equal(t, tc.wantUnpinned, fresh.ExplicitTimezoneUnpinned)
			assert.Empty(t, legacyEnvTZ(fresh), "the reincarnated record carries no env TZ")
		})
	}
}

// TestGetAgent_AdoptsLegacyTZInResponse checks the agent GET (the configure
// page's load) shows a legacy env TZ as the pin it becomes, without writing.
func TestGetAgent_AdoptsLegacyTZInResponse(t *testing.T) {
	srv, s, agent := tzPatchServer(t, string(state.PhaseStopped), func(a *store.Agent) {
		a.AppliedConfig.Env = map[string]string{"TZ": "Europe/Paris", "FOO": "bar"}
	})
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		AppliedConfig *store.AgentAppliedConfig `json:"appliedConfig"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.AppliedConfig)
	assert.Equal(t, "Europe/Paris", resp.AppliedConfig.ExplicitTimezone)
	assert.True(t, resp.AppliedConfig.ExplicitTimezoneLegacy)
	assert.NotContains(t, resp.AppliedConfig.Env, "TZ")

	stored, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "Europe/Paris", stored.AppliedConfig.Env["TZ"], "GET does not write")
}

// --- template TZ (AC9) -----------------------------------------------------

// updateTZTemplateEnv rewrites the env of the template createTZTemplate made.
func updateTZTemplateEnv(t *testing.T, s store.Store, slug string, env map[string]string) {
	t.Helper()
	ctx := context.Background()
	tmpl, err := s.GetTemplate(ctx, tid("template-"+slug+"-"+t.Name()))
	require.NoError(t, err)
	tmpl.Config.Env = env
	tmpl.ContentHash = "feedd00d"
	require.NoError(t, s.UpdateTemplate(ctx, tmpl))
}

// TestCreateAgent_TemplateTZUnaffectedByLaterTemplateEdit covers AC9: a
// template TZ is captured as a pin on create, and a later edit of the
// template's TZ does not move the agent, on start or on reincarnate.
func TestCreateAgent_TemplateTZUnaffectedByLaterTemplateEdit(t *testing.T) {
	ctx := context.Background()
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	setHubAgentDefaults(srv, opsettings.AgentDefaultsSettings{DefaultTimezone: "America/Denver"})
	createTZTemplate(t, s, "tz-tmpl", "", map[string]string{"TZ": "Europe/Paris"})

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "tz-tmpl-agent", ProjectID: project.ID, Task: "x", Template: "tz-tmpl",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	agent, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.Equal(t, "Europe/Paris", agent.AppliedConfig.ExplicitTimezone, "the template TZ is captured as a pin")
	assert.Empty(t, legacyEnvTZ(agent.AppliedConfig))

	updateTZTemplateEnv(t, s, "tz-tmpl", map[string]string{"TZ": "Asia/Tokyo"})

	agent, err = s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "Europe/Paris", agent.AppliedConfig.ExplicitTimezone, "the stored pin is unchanged")
	assert.Equal(t, agentTZ{TZ: "Europe/Paris", Source: TZSourceExplicit}, srv.agentTZ(ctx, agent))

	d := NewHTTPAgentDispatcherWithClient(s, &mockRuntimeBrokerClient{}, false, slog.Default())
	d.SetHubAgentDefaultsProvider(srv.hubAgentDefaults)
	startEnv, err := d.buildStartEnv(ctx, agent, "test", mintSiteStart)
	require.NoError(t, err)
	assert.Equal(t, "Europe/Paris", startEnv.env["TZ"], "a start sends the pinned zone")

	fresh, _, err := srv.buildFreshAppliedConfig(ctx, agent, project, "")
	require.NoError(t, err)
	assert.Equal(t, "Europe/Paris", fresh.ExplicitTimezone, "reincarnate carries the pin over the edited template")
	assert.Empty(t, legacyEnvTZ(fresh))
}

// TestReincarnate_TemplateTZ covers the AC9 reincarnate bullets with a
// template that carries TZ: an unpinned agent stays unpinned, and an agent
// with no pin re-derives the current template zone as a pin.
func TestReincarnate_TemplateTZ(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name         string
		unpinned     bool
		wantPin      string
		wantUnpinned bool
	}{
		{name: "unpinned template agent stays unpinned", unpinned: true, wantUnpinned: true},
		{name: "no pin re-derives the current template zone", wantPin: "Asia/Tokyo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, project, broker := setupReincarnateTestServer(t, newReincarnateTestDispatcher())
			// Created from the template while it said Paris; it says Tokyo now.
			createTZTemplate(t, s, "tz-tmpl", "", map[string]string{"TZ": "Asia/Tokyo"})
			agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
				a.Template = "tz-tmpl"
				a.AppliedConfig.ExplicitTimezoneUnpinned = tc.unpinned
			})
			fresh, _, err := srv.buildFreshAppliedConfig(ctx, agent, project, "")
			require.NoError(t, err)
			assert.Equal(t, tc.wantPin, fresh.ExplicitTimezone)
			assert.Equal(t, tc.wantUnpinned, fresh.ExplicitTimezoneUnpinned)
			assert.False(t, fresh.ExplicitTimezoneLegacy)
			assert.Empty(t, fresh.Env[agentTZEnvKey], "no TZ in Env")
			if fresh.InlineConfig != nil {
				assert.Empty(t, fresh.InlineConfig.Env[agentTZEnvKey], "no TZ in InlineConfig.Env")
			}
		})
	}
}
