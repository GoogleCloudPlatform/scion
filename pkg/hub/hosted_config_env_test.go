//go:build !no_sqlite

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

package hub

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hostedguard"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hostedEnvSecret marks a value that must never appear in a refusal.
const hostedEnvSecret = "p3-member-secret"

// hostedRefusedAgentConfigs are member agent configs a hosted hub refuses
// for the env or hub rule, with the message prefix each must produce.
func hostedRefusedAgentConfigs() map[string]struct {
	cfg        *api.ScionConfig
	wantPrefix string
} {
	return map[string]struct {
		cfg        *api.ScionConfig
		wantPrefix string
	}{
		"env reference":      {&api.ScionConfig{Env: map[string]string{"AWS_KEY": "${" + hostedEnvSecret + "}"}}, `env "AWS_KEY": host environment references`},
		"env bare reference": {&api.ScionConfig{Env: map[string]string{"K": "x$" + hostedEnvSecret}}, `env "K": host environment references`},
		"env empty":          {&api.ScionConfig{Env: map[string]string{"GITHUB_TOKEN": ""}}, `env "GITHUB_TOKEN": host environment references`},
		"env scion name":     {&api.ScionConfig{Env: map[string]string{"SCION_HUB_ENDPOINT": "http://" + hostedEnvSecret}}, `env "SCION_HUB_ENDPOINT": names reserved`},
		"env gce metadata":   {&api.ScionConfig{Env: map[string]string{"GCE_METADATA_HOST": hostedEnvSecret}}, `env "GCE_METADATA_HOST": names reserved`},
		"hub endpoint":       {&api.ScionConfig{Hub: &api.AgentHubConfig{Endpoint: "http://127.0.0.1:9810/" + hostedEnvSecret}}, `hub "endpoint": a hub endpoint in agent config`},
	}
}

func assertNoHostedValue(t *testing.T, s string) {
	t.Helper()
	assert.NotContains(t, s, hostedEnvSecret, "the refusal never echoes a value")
	assert.NotContains(t, s, "127.0.0.1", "the refusal never echoes the endpoint")
}

// TestCreateAgent_HostedRefusesMemberEnvAndHub: on a hosted hub a create
// whose config holds a host env reference, an empty passthrough value, a
// reserved env name or a hub endpoint is refused (400 hosted_config_refused
// naming the field) and stores and dispatches nothing.
func TestCreateAgent_HostedRefusesMemberEnvAndHub(t *testing.T) {
	for name, tc := range hostedRefusedAgentConfigs() {
		t.Run(name, func(t *testing.T) {
			disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
			srv, s, project := setupCreateAgentServer(t, disp)
			require.False(t, srv.workstation, "test servers run hosted")
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
				Name: "hosted-env-agent", ProjectID: project.ID, Config: tc.cfg,
			})
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			code, msg := decodeHostedRefusal(t, rec)
			assert.Equal(t, hostedguard.ErrorCode, code)
			assert.True(t, strings.HasPrefix(msg, tc.wantPrefix), msg)
			assertNoHostedValue(t, rec.Body.String())
			assert.Nil(t, disp.capturedAgent, "nothing dispatched")
			_, err := s.GetAgentBySlug(context.Background(), project.ID, "hosted-env-agent")
			assert.ErrorIs(t, err, store.ErrNotFound, "a refused create stores nothing")
		})
	}
}

// TestCreateAgent_HostedAcceptsMemberSettableEnv: literal env, the
// auto-expose keys and the log level and telemetry keys the CLI writes are
// accepted on a hosted hub.
func TestCreateAgent_HostedAcceptsMemberSettableEnv(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	env := map[string]string{
		"PLAIN":                   "value",
		"PRICE":                   "5$",
		"SCION_AUTO_EXPOSE_PORTS": "true",
		"SCION_LOG_LEVEL":         "debug",
		"SCION_TELEMETRY_ENABLED": "false",
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "hosted-literal-env", ProjectID: project.ID, Config: &api.ScionConfig{Env: env},
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	got, err := s.GetAgentBySlug(context.Background(), project.ID, "hosted-literal-env")
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig.InlineConfig)
	assert.Equal(t, env, got.AppliedConfig.InlineConfig.Env)
	require.NotNil(t, disp.capturedAgent, "dispatched")
}

// TestCreateAgent_WorkstationKeepsMemberEnvAndHub: a workstation hub stores
// and dispatches member env and hub config unchanged.
func TestCreateAgent_WorkstationKeepsMemberEnvAndHub(t *testing.T) {
	for name, tc := range hostedRefusedAgentConfigs() {
		t.Run(name, func(t *testing.T) {
			disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
			srv, s, project := setupCreateAgentServer(t, disp)
			srv.workstation = true
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
				Name: "workstation-env-agent", ProjectID: project.ID, Config: tc.cfg,
			})
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
			_, err := s.GetAgentBySlug(context.Background(), project.ID, "workstation-env-agent")
			require.NoError(t, err)
			require.NotNil(t, disp.capturedAgent, "dispatched")
		})
	}
}

// TestDispatch_HostedConfigPrecheckEnvAndHub: on a hosted hub every
// dispatch path refuses an agent whose stored inline config breaks the env
// or hub rule (config stored before the check existed), with no broker
// call; a workstation hub dispatches it.
func TestDispatch_HostedConfigPrecheckEnvAndHub(t *testing.T) {
	ctx := context.Background()
	for name, tc := range hostedRefusedAgentConfigs() {
		for _, op := range precheckOps(ctx) {
			t.Run(name+"/"+op.name, func(t *testing.T) {
				d, m, a := hostedPrecheckDispatcher(t, true, nil)
				a.AppliedConfig.InlineConfig.Env = tc.cfg.Env
				a.AppliedConfig.InlineConfig.Hub = tc.cfg.Hub
				err := op.run(d, a)
				var refusal *RuntimeTargetRefusal
				require.True(t, errors.As(err, &refusal), "refused with a typed refusal: %v", err)
				assert.Equal(t, http.StatusBadRequest, refusal.Status)
				assert.Equal(t, hostedguard.ErrorCode, refusal.Code)
				assert.True(t, strings.HasPrefix(refusal.Message, tc.wantPrefix), refusal.Message)
				assertNoHostedValue(t, refusal.Message)
				assert.False(t, op.called(m), "no broker call")
			})
		}
		t.Run(name+"/workstation dispatches", func(t *testing.T) {
			d, m, a := hostedPrecheckDispatcher(t, false, nil)
			a.AppliedConfig.InlineConfig.Env = tc.cfg.Env
			a.AppliedConfig.InlineConfig.Hub = tc.cfg.Hub
			err := d.DispatchAgentStart(ctx, a, "task", false)
			var refusal *RuntimeTargetRefusal
			assert.False(t, errors.As(err, &refusal) && refusal.Code == hostedguard.ErrorCode, "no hosted refusal: %v", err)
			assert.True(t, m.startCalled, "broker called")
		})
	}
}

// TestValidateAgentConfigPatch_HostedRefusesMemberEnvAndHub: on a hosted
// hub a config PATCH (and a held edit, validated by the same call) with a
// host env reference, an empty passthrough value or a hub endpoint is
// refused when written; a workstation hub accepts it. A reserved name is
// refused earlier by the PATCH's own reserved-key check.
func TestValidateAgentConfigPatch_HostedRefusesMemberEnvAndHub(t *testing.T) {
	ctx := context.Background()
	for name, tc := range hostedRefusedAgentConfigs() {
		if strings.Contains(tc.wantPrefix, "names reserved") {
			continue
		}
		t.Run(name, func(t *testing.T) {
			srv, _ := testServer(t)
			agent := &store.Agent{ID: tid("agent-patch-env"), ProjectID: tid("project-patch-env"), AppliedConfig: &store.AgentAppliedConfig{}}
			rec := httptest.NewRecorder()
			assert.False(t, srv.validateAgentConfigPatch(ctx, rec, agent, tc.cfg, nil))
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			code, msg := decodeHostedRefusal(t, rec)
			assert.Equal(t, hostedguard.ErrorCode, code)
			assert.True(t, strings.HasPrefix(msg, tc.wantPrefix), msg)
			assertNoHostedValue(t, rec.Body.String())

			srv.workstation = true
			rec = httptest.NewRecorder()
			assert.True(t, srv.validateAgentConfigPatch(ctx, rec, agent, tc.cfg, nil), rec.Body.String())
		})
	}
}

const (
	// No double quotes: putHarnessConfigYAML sends the YAML unescaped.
	hostedHCEnvRefYAML   = "harness: claude\nenv:\n  HC_KEY: ${" + hostedEnvSecret + "}\n"
	hostedHCEnvEmptyYAML = "harness: claude\nenv:\n  HC_KEY: ''\n"
	hostedHCEnvScionYAML = "harness: claude\nenv:\n  SCION_HUB_ENDPOINT: http://" + hostedEnvSecret + "\n"
	// hostedHCCodexYAML carries the reserved name a shipped harness config
	// sets (harnesses/codex/config.yaml), which member config may set.
	hostedHCCodexYAML = "harness: codex\nenv:\n  SCION_LIVENESS_HARNESS_ROOT_DEPTH: \"2\"\n  LITERAL: plain\n"
)

func assertHostedHCEnvRefusal(t *testing.T, rec *httptest.ResponseRecorder, entry, name string) {
	t.Helper()
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	code, msg := decodeHostedRefusal(t, rec)
	assert.Equal(t, hostedguard.ErrorCode, code)
	assert.True(t, strings.HasPrefix(msg, `env "`+entry+`": `), msg)
	assert.Contains(t, msg, `harness config "`+name+`"`)
	assertNoHostedValue(t, rec.Body.String())
}

// TestHarnessConfigFileWrite_HostedProjectScopeEnv: on a hosted hub,
// writing a project-scope harness config whose env references the host
// environment, passes a host variable through or sets a reserved name is
// refused; a global harness config and a workstation hub accept it.
func TestHarnessConfigFileWrite_HostedProjectScopeEnv(t *testing.T) {
	for name, tc := range map[string]struct{ yaml, entry string }{
		"reference":     {hostedHCEnvRefYAML, "HC_KEY"},
		"empty":         {hostedHCEnvEmptyYAML, "HC_KEY"},
		"reserved name": {hostedHCEnvScionYAML, "SCION_HUB_ENDPOINT"},
	} {
		t.Run(name+" refused", func(t *testing.T) {
			srv, s, stor := testHarnessConfigFileServer(t)
			hc := newHostedTestHC(t, srv, s, "hc-member", "member-hc", store.HarnessConfigScopeProject, tid("project-1"), hostedHCOKYAML)
			objects := len(stor.content)
			rec := putHarnessConfigYAML(t, srv, hc.ID, tc.yaml)
			assertHostedHCEnvRefusal(t, rec, tc.entry, "member-hc")
			assert.Len(t, stor.content, objects, "no object written for refused content")
		})
		for accepted, scope := range map[string]string{"global": store.HarnessConfigScopeGlobal, "workstation": store.HarnessConfigScopeProject} {
			t.Run(name+" accepted on "+accepted, func(t *testing.T) {
				srv, s, _ := testHarnessConfigFileServer(t)
				srv.workstation = accepted == "workstation"
				scopeID := ""
				if scope == store.HarnessConfigScopeProject {
					scopeID = tid("project-1")
				}
				hc := newHostedTestHC(t, srv, s, "hc-accepted", "accepted-hc", scope, scopeID, "")
				rec := putHarnessConfigYAML(t, srv, hc.ID, tc.yaml)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			})
		}
	}
}

// TestHarnessConfigClone_HostedEnvIntoProjectScope: cloning a global
// harness config into project scope on a hosted hub is refused when its env
// references the host environment, and accepted for a copy of a shipped
// config that sets SCION_LIVENESS_HARNESS_ROOT_DEPTH.
func TestHarnessConfigClone_HostedEnvIntoProjectScope(t *testing.T) {
	ctx := context.Background()
	t.Run("host reference refused", func(t *testing.T) {
		srv, s, _ := testHarnessConfigFileServer(t)
		require.NoError(t, s.CreateProject(ctx, &store.Project{ID: tid("project-clone"), Name: "clone", Slug: "clone"}))
		src := newHostedTestHC(t, srv, s, "hc-global-src", "global-src", store.HarnessConfigScopeGlobal, "", hostedHCEnvRefYAML)
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+src.ID+"/clone", map[string]interface{}{
			"name": "cloned-hc", "scope": store.HarnessConfigScopeProject, "projectId": tid("project-clone"),
		})
		assertHostedHCEnvRefusal(t, rec, "HC_KEY", "cloned-hc")
		_, err := s.GetHarnessConfigBySlug(ctx, "cloned-hc", store.HarnessConfigScopeProject, tid("project-clone"))
		assert.ErrorIs(t, err, store.ErrNotFound, "no clone stored")
	})
	t.Run("codex liveness depth accepted", func(t *testing.T) {
		srv, s, _ := testHarnessConfigFileServer(t)
		require.NoError(t, s.CreateProject(ctx, &store.Project{ID: tid("project-clone"), Name: "clone", Slug: "clone"}))
		src := newHostedTestHC(t, srv, s, "hc-global-codex", "codex", store.HarnessConfigScopeGlobal, "", hostedHCCodexYAML)
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+src.ID+"/clone", map[string]interface{}{
			"name": "my-codex", "scope": store.HarnessConfigScopeProject, "projectId": tid("project-clone"),
		})
		require.Less(t, rec.Code, 300, rec.Body.String())
		_, err := s.GetHarnessConfigBySlug(ctx, "my-codex", store.HarnessConfigScopeProject, tid("project-clone"))
		assert.NoError(t, err, "clone stored")
	})
}

// TestDispatch_HostedHarnessConfigEnvPrecheck: on a hosted hub a dispatch
// with a stored project-scope harness config whose env breaks the env rule
// is refused with no broker call; a workstation hub dispatches it.
func TestDispatch_HostedHarnessConfigEnvPrecheck(t *testing.T) {
	ctx := context.Background()
	for _, hosted := range []bool{true, false} {
		t.Run(map[bool]string{true: "hosted", false: "workstation"}[hosted], func(t *testing.T) {
			srv, s, _ := testHarnessConfigFileServer(t)
			hc := newHostedTestHC(t, srv, s, "hc-stored", "stored-hc", store.HarnessConfigScopeProject, tid("project-1"), hostedHCEnvEmptyYAML)
			d, m, a := hostedHCDispatcher(t, srv, s, hc, hosted)
			err := d.DispatchAgentStart(ctx, a, "task", false)
			var refusal *RuntimeTargetRefusal
			refused := errors.As(err, &refusal) && refusal.Code == hostedguard.ErrorCode
			if !hosted {
				assert.False(t, refused, "no hosted refusal: %v", err)
				assert.True(t, m.startCalled, "broker called")
				return
			}
			require.True(t, refused, "refused: %v", err)
			assert.Equal(t, http.StatusBadRequest, refusal.Status)
			assert.True(t, strings.HasPrefix(refusal.Message, `env "HC_KEY": `), refusal.Message)
			assert.Contains(t, refusal.Message, `harness config "stored-hc"`)
			assert.False(t, m.startCalled, "no broker call")
		})
	}
}

// TestMemberSettableEnv_Parity pins the single member-safe list
// (api.MemberSettableEnv) against the Hub's sets and the reserved prefixes:
// hostedguard cannot import pkg/secret, so the prefix sets are compared
// here, and the PATCH and auto-expose sets must equal the list's edit scope.
func TestMemberSettableEnv_Parity(t *testing.T) {
	for _, p := range append(api.ReservedEnvPrefixes(), "OTHER_", "SCION", "scion_") {
		name := p + "X"
		assert.Equal(t, secret.IsReservedEnvTarget(name), hostedguard.IsReservedEnvName(name), "reserved prefix parity for %q", name)
	}
	edit := api.MemberSettableEnvNames(api.MemberEnvEdit)
	assert.Equal(t, edit, patchableReservedEnvKeys, "PATCH exemptions are the list's edit scope")
	assert.Equal(t, edit, autoExposeEnvKeys, "the configure page's auto-expose keys are the list's edit scope")
	for _, e := range api.MemberSettableEnv() {
		assert.True(t, secret.IsReservedEnvTarget(e.Name), "%s is a reserved name", e.Name)
		assert.Equal(t, e.Scope&api.MemberEnvEdit != 0, !patchReservedEnvKey(e.Name), "PATCH scope of %s", e.Name)
	}
}

// TestValidateAgentConfigPatch_HostedMemberEnvScope: on a hosted hub a
// config edit may add or change only the edit-scope names (auto-expose);
// SCION_LOG_LEVEL and SCION_TELEMETRY_ENABLED are create-only and refused
// when an edit adds or changes them.
func TestValidateAgentConfigPatch_HostedMemberEnvScope(t *testing.T) {
	ctx := context.Background()
	srv, _ := testServer(t)
	require.False(t, srv.workstation)
	agent := &store.Agent{ID: tid("agent-patch-scope"), ProjectID: tid("project-patch-scope"),
		AppliedConfig: &store.AgentAppliedConfig{Env: map[string]string{"SCION_LOG_LEVEL": "info"}}}
	for name, tc := range map[string]struct {
		env     map[string]string
		allowed bool
	}{
		"auto-expose added":               {map[string]string{"SCION_LOG_LEVEL": "info", "SCION_AUTO_EXPOSE_PORTS": "true"}, true},
		"stored create-only value echoed": {map[string]string{"SCION_LOG_LEVEL": "info"}, true},
		"create-only value changed":       {map[string]string{"SCION_LOG_LEVEL": "debug"}, false},
		"create-only name added":          {map[string]string{"SCION_LOG_LEVEL": "info", "SCION_TELEMETRY_ENABLED": "false"}, false},
		"harness-config-only name added":  {map[string]string{"SCION_LOG_LEVEL": "info", "SCION_LIVENESS_HARNESS_ROOT_DEPTH": "2"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			ok := srv.validateAgentConfigPatch(ctx, rec, agent, &api.ScionConfig{Env: tc.env}, nil)
			assert.Equal(t, tc.allowed, ok, rec.Body.String())
		})
	}
}

// TestCreateAgent_HostedMemberEnvScope: at create, agent config may set the
// create-scope names but not a harness-config-only one.
func TestCreateAgent_HostedMemberEnvScope(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, _, project := setupCreateAgentServer(t, disp)
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "hosted-liveness", ProjectID: project.ID,
		Config: &api.ScionConfig{Env: map[string]string{"SCION_LIVENESS_HARNESS_ROOT_DEPTH": "2"}},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	code, msg := decodeHostedRefusal(t, rec)
	assert.Equal(t, hostedguard.ErrorCode, code)
	assert.True(t, strings.HasPrefix(msg, `env "SCION_LIVENESS_HARNESS_ROOT_DEPTH": names reserved`), msg)
}

// TestHarnessConfigFileWrite_HostedProjectScopeEnvTemplate: on a hosted hub a
// project-scope harness config may not set a reserved name in env_template
// unless the member-safe list allows it for harness configs
// (SCION_AGENT_NAME); a workstation hub accepts it.
func TestHarnessConfigFileWrite_HostedProjectScopeEnvTemplate(t *testing.T) {
	for _, tc := range []struct {
		name, key string
		refused   bool
	}{
		{"reserved name", "SCION_HUB_ENDPOINT", true},
		{"agent name", "SCION_AGENT_NAME", false},
		{"non-reserved", "NO_COLOR", false},
	} {
		for _, workstation := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "/hosted", true: "/workstation"}[workstation], func(t *testing.T) {
				srv, s, _ := testHarnessConfigFileServer(t)
				srv.workstation = workstation
				hc := newHostedTestHC(t, srv, s, "hc-member", "member-hc", store.HarnessConfigScopeProject, tid("project-1"), hostedHCOKYAML)
				rec := putHarnessConfigYAML(t, srv, hc.ID, "harness: claude\nenv_template:\n  "+tc.key+": '{{ .AgentName }}-"+hostedEnvSecret+"'\n")
				if !tc.refused || workstation {
					require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
					return
				}
				require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
				code, msg := decodeHostedRefusal(t, rec)
				assert.Equal(t, hostedguard.ErrorCode, code)
				assert.True(t, strings.HasPrefix(msg, `env_template "`+tc.key+`": names reserved`), msg)
				assert.Contains(t, msg, `harness config "member-hc"`)
				assertNoHostedValue(t, rec.Body.String())
			})
		}
	}
}

// TestHarnessConfigClone_HostedShippedConfigsIntoProjectScope: every shipped
// harness config (harnesses/*/config.yaml) can still be cloned into project
// scope on a hosted hub.
func TestHarnessConfigClone_HostedShippedConfigsIntoProjectScope(t *testing.T) {
	ctx := context.Background()
	paths, err := filepath.Glob(filepath.Join("..", "..", "harnesses", "*", "config.yaml"))
	require.NoError(t, err)
	require.NotEmpty(t, paths)
	for _, p := range paths {
		name := filepath.Base(filepath.Dir(p))
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(p)
			require.NoError(t, err)
			srv, s, _ := testHarnessConfigFileServer(t)
			require.False(t, srv.workstation)
			require.NoError(t, s.CreateProject(ctx, &store.Project{ID: tid("project-clone"), Name: "clone", Slug: "clone"}))
			src := newHostedTestHC(t, srv, s, "hc-global-shipped", name, store.HarnessConfigScopeGlobal, "", string(data))
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+src.ID+"/clone", map[string]interface{}{
				"name": "my-" + name, "scope": store.HarnessConfigScopeProject, "projectId": tid("project-clone"),
			})
			require.Less(t, rec.Code, 300, rec.Body.String())
		})
	}
}
