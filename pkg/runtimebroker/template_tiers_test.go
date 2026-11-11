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

package runtimebroker

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Tests for ptone/scion#4218 (TS-2a): the broker takes hub telemetry
// defaults and the project's telemetry policy as separate tiers.

func tiersBoolPtr(b bool) *bool { return &b }

func tiersFmt(b *bool) string {
	if b == nil {
		return "<nil>"
	}
	if *b {
		return "true"
	}
	return "false"
}

// TestResolveTelemetryOverride_PolicyBeatsEnv pins decision E6: the
// project's telemetry policy beats the requester's SCION_TELEMETRY_ENABLED
// (acceptance 4). With no policy, the env var still overrides as today.
func TestResolveTelemetryOverride_PolicyBeatsEnv(t *testing.T) {
	cases := []struct {
		name     string
		policy   *bool
		env      map[string]string
		want     *bool
		wantEnvV string // "" = key absent
	}{
		{"policy false beats env true", tiersBoolPtr(false), map[string]string{"SCION_TELEMETRY_ENABLED": "true"}, tiersBoolPtr(false), "false"},
		{"policy true beats env false", tiersBoolPtr(true), map[string]string{"SCION_TELEMETRY_ENABLED": "false"}, tiersBoolPtr(true), "true"},
		{"policy alone", tiersBoolPtr(false), map[string]string{}, tiersBoolPtr(false), ""},
		{"no policy: env true", nil, map[string]string{"SCION_TELEMETRY_ENABLED": "true"}, tiersBoolPtr(true), "true"},
		{"no policy: env 1", nil, map[string]string{"SCION_TELEMETRY_ENABLED": "1"}, tiersBoolPtr(true), "1"},
		{"no policy: env false", nil, map[string]string{"SCION_TELEMETRY_ENABLED": "false"}, tiersBoolPtr(false), "false"},
		{"neither", nil, map[string]string{}, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveTelemetryOverride(tc.policy, tc.env)
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("override = %v, want nil", *got)
			case tc.want != nil && (got == nil || *got != *tc.want):
				t.Fatalf("override = %s, want %v", tiersFmt(got), *tc.want)
			}
			v, ok := tc.env["SCION_TELEMETRY_ENABLED"]
			if tc.wantEnvV == "" && ok {
				t.Errorf("SCION_TELEMETRY_ENABLED unexpectedly set to %q", v)
			}
			if tc.wantEnvV != "" && v != tc.wantEnvV {
				// The container env must agree with the policy, since
				// Start only fills telemetry env keys that are absent.
				t.Errorf("SCION_TELEMETRY_ENABLED = %q, want %q", v, tc.wantEnvV)
			}
		})
	}
	if got := resolveTelemetryOverride(tiersBoolPtr(true), map[string]string{}); got == nil || !*got {
		t.Fatal("policy true must yield override true")
	}
}

// TestBuildStartContext_TelemetryPolicyBeatsEnv drives the real start
// context: a create Config.TelemetryPolicy and a start/restart
// TelemetryPolicy input both beat SCION_TELEMETRY_ENABLED in resolvedEnv.
func TestBuildStartContext_TelemetryPolicyBeatsEnv(t *testing.T) {
	cases := []struct {
		name   string
		in     startContextInputs
		policy *bool // the policy carried by in, for the env assertion
		envVal string
		want   bool
	}{
		{
			name:   "create config policy false beats env true",
			in:     startContextInputs{Config: &CreateAgentConfig{TelemetryPolicy: tiersBoolPtr(false)}, Operation: opCreate},
			policy: tiersBoolPtr(false),
			envVal: "true",
			want:   false,
		},
		{
			name:   "start policy true beats env false",
			in:     startContextInputs{TelemetryPolicy: tiersBoolPtr(true), Operation: opHTTPStart},
			policy: tiersBoolPtr(true),
			envVal: "false",
			want:   true,
		},
		{
			name:   "restart policy false beats env true",
			in:     startContextInputs{TelemetryPolicy: tiersBoolPtr(false), Operation: opHTTPRestart},
			policy: tiersBoolPtr(false),
			envVal: "true",
			want:   false,
		},
		{
			name:   "no policy: env still overrides",
			in:     startContextInputs{Operation: opCreate},
			envVal: "true",
			want:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultServerConfig()
			cfg.StateDir = t.TempDir()
			srv := newTestServerForStartContext(t, cfg)

			in := tc.in
			in.Name = "agent-policy"
			in.ResolvedEnv = map[string]string{"SCION_TELEMETRY_ENABLED": tc.envVal}
			in.HTTPRequest = httptest.NewRequest("POST", "/api/v1/agents", nil)
			sc, err := srv.buildStartContext(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			if sc.Opts.TelemetryOverride == nil || *sc.Opts.TelemetryOverride != tc.want {
				t.Fatalf("TelemetryOverride = %s, want %v", tiersFmt(sc.Opts.TelemetryOverride), tc.want)
			}
			// The container env must carry the winning value too: Start
			// only fills telemetry env keys that are absent, so a stale
			// requester flag would otherwise reach sciontool.
			wantEnv := tc.envVal
			if tc.policy != nil {
				wantEnv = tiersFmt(tc.policy)
			}
			if got := sc.Opts.Env["SCION_TELEMETRY_ENABLED"]; got != wantEnv {
				t.Errorf("container env SCION_TELEMETRY_ENABLED = %q, want %q", got, wantEnv)
			}
		})
	}
}

// TestStartAndRestart_DecodeTelemetryPolicy pins the join between the
// start/restart request body's telemetryPolicy and opts.TelemetryOverride.
func TestStartAndRestart_DecodeTelemetryPolicy(t *testing.T) {
	for _, path := range []string{"/api/v1/agents/test-agent-1/start", "/api/v1/agents/test-agent-1/restart"} {
		t.Run(path, func(t *testing.T) {
			srv := newTestServer(t)
			mgr := srv.manager.(*mockManager)

			body := `{"resolvedEnv": {"SCION_TELEMETRY_ENABLED": "true"}, "telemetryPolicy": false}`
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)

			if w.Code != http.StatusAccepted {
				t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
			}
			opts := mgr.LastStartOpts()
			if opts.TelemetryOverride == nil || *opts.TelemetryOverride {
				t.Fatalf("TelemetryOverride = %s, want false (policy beats env)", tiersFmt(opts.TelemetryOverride))
			}
		})
	}
}

// TestCreateAgent_DecodesTelemetryPolicy covers the create path end to end.
func TestCreateAgent_DecodesTelemetryPolicy(t *testing.T) {
	srv, mgr := newHubDefaultsWiringServer()

	postCreateAgent(t, srv, `{
		"name": "policy-agent",
		"id": "agent-uuid-policy",
		"slug": "policy-agent",
		"resolvedEnv": {"SCION_TELEMETRY_ENABLED": "true"},
		"config": {"template": "claude", "telemetryPolicy": false}
	}`)

	if mgr.startCalls != 1 {
		t.Fatalf("expected Start to be called once, got %d", mgr.startCalls)
	}
	opts := mgr.LastStartOpts()
	if opts.TelemetryOverride == nil || *opts.TelemetryOverride {
		t.Fatalf("TelemetryOverride = %s, want false (policy beats env)", tiersFmt(opts.TelemetryOverride))
	}
}

// TestCreateAgentConfig_DecodesHubTelemetryDefault checks the wire field for
// the hub default telemetry tier.
func TestCreateAgentConfig_DecodesHubTelemetryDefault(t *testing.T) {
	var cfg CreateAgentConfig
	if err := json.Unmarshal([]byte(`{"hubAgentDefaults": {"telemetry": {"cloud": {"endpoint": "hub:4317"}}}, "telemetryPolicy": true}`), &cfg); err != nil {
		t.Fatal(err)
	}
	hd := cfg.HubAgentDefaults
	if hd == nil || hd.Telemetry == nil || hd.Telemetry.Cloud == nil || hd.Telemetry.Cloud.Endpoint != "hub:4317" {
		t.Fatalf("hub default telemetry not decoded: %+v", hd)
	}
	if hd.IsEmpty() {
		t.Error("hub defaults carrying only telemetry must not be empty")
	}
	if cfg.TelemetryPolicy == nil || !*cfg.TelemetryPolicy {
		t.Fatalf("telemetryPolicy not decoded: %v", cfg.TelemetryPolicy)
	}
}

// TestExtractRequiredEnvKeys_TemplateSecretsFromHydratedTemplate pins P6
// (acceptance 5): for a hub template, env-gather lists the secrets of the
// hydrated template, both when the local project has no template of that
// slug and when it has a different one.
func TestExtractRequiredEnvKeys_TemplateSecretsFromHydratedTemplate(t *testing.T) {
	writeTpl := func(t *testing.T, dir, secretKey string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		yaml := "harness_config: claude\nsecrets:\n  - key: " + secretKey + "\n    description: from " + filepath.Base(filepath.Dir(dir)) + "\n"
		if err := os.WriteFile(filepath.Join(dir, "scion-agent.yaml"), []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	newSrv := func(t *testing.T) (*Server, string) {
		srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
			"harness: claude\nimage: test-image\nuser: scion\nauth_selected_type: api-key\n"+claudeAuthBlock,
			"schema_version: \"1\"\nprofiles:\n  default:\n    runtime: mock\n")
		return srv, projectDir
	}
	hubReq := func(projectDir string) CreateAgentRequest {
		return CreateAgentRequest{
			Name:        "tpl-secrets",
			ProjectPath: projectDir,
			ResolvedEnv: map[string]string{"ANTHROPIC_API_KEY": "x"},
			Config:      &CreateAgentConfig{Template: "mytpl", TemplateID: "tpl-uuid", TemplateHash: "h1", HarnessConfig: "claude", Profile: "default"},
		}
	}
	has := func(keys []string, k string) bool {
		for _, x := range keys {
			if x == k {
				return true
			}
		}
		return false
	}

	t.Run("no local template of that slug", func(t *testing.T) {
		srv, projectDir := newSrv(t)
		hydrated := filepath.Join(t.TempDir(), "hydrated", "mytpl")
		writeTpl(t, hydrated, "HUB_TPL_SECRET")

		required, secretInfo, _, _ := srv.extractRequiredEnvKeys(hubReq(projectDir), hydrated)
		if !has(required, "HUB_TPL_SECRET") {
			t.Fatalf("hydrated template secret not listed: required=%v", required)
		}
		if secretInfo["HUB_TPL_SECRET"].Source != "template" {
			t.Errorf("source = %q, want template", secretInfo["HUB_TPL_SECRET"].Source)
		}
	})

	t.Run("different local template of the same slug", func(t *testing.T) {
		srv, projectDir := newSrv(t)
		writeTpl(t, filepath.Join(projectDir, "templates", "mytpl"), "STALE_LOCAL_SECRET")
		hydrated := filepath.Join(t.TempDir(), "hydrated", "mytpl")
		writeTpl(t, hydrated, "HUB_TPL_SECRET")

		required, _, _, _ := srv.extractRequiredEnvKeys(hubReq(projectDir), hydrated)
		if !has(required, "HUB_TPL_SECRET") {
			t.Errorf("hydrated template secret not listed: required=%v", required)
		}
		if has(required, "STALE_LOCAL_SECRET") {
			t.Errorf("the local template of the same slug must not be read for a hub template: required=%v", required)
		}
	})

	t.Run("local mode unchanged", func(t *testing.T) {
		srv, projectDir := newSrv(t)
		writeTpl(t, filepath.Join(projectDir, "templates", "mytpl"), "LOCAL_SECRET")
		req := hubReq(projectDir)
		req.Config.TemplateID, req.Config.TemplateHash = "", ""

		required, _, _, _ := srv.extractRequiredEnvKeys(req, "")
		if !has(required, "LOCAL_SECRET") {
			t.Errorf("local template secret not listed: required=%v", required)
		}
	})
}

// TestExtractRequiredEnvKeys_TemplateAuthTypeFromHydratedTemplate pins the
// same rule for the template-level auth_selectedType cascade: for a hub
// template, env-gather reads it from the hydrated template, never from a
// local template of the same slug.
func TestExtractRequiredEnvKeys_TemplateAuthTypeFromHydratedTemplate(t *testing.T) {
	writeTpl := func(t *testing.T, dir, authType string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		yaml := "harness_config: claude\n"
		if authType != "" {
			yaml += "auth_selectedType: " + authType + "\n"
		}
		if err := os.WriteFile(filepath.Join(dir, "scion-agent.yaml"), []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	newSrv := func(t *testing.T) (*Server, string) {
		// The harness-config selects api-key; ANTHROPIC_API_KEY is
		// supplied, so only a template-selected vertex-ai makes
		// GOOGLE_CLOUD_PROJECT required.
		srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
			"harness: claude\nimage: test-image\nuser: scion\nauth_selected_type: api-key\n"+claudeAuthBlock,
			"schema_version: \"1\"\nprofiles:\n  default:\n    runtime: mock\n")
		return srv, projectDir
	}
	hubReq := func(projectDir string) CreateAgentRequest {
		return CreateAgentRequest{
			Name:        "tpl-auth",
			ProjectPath: projectDir,
			ResolvedEnv: map[string]string{"ANTHROPIC_API_KEY": "x"},
			Config:      &CreateAgentConfig{Template: "mytpl", TemplateID: "tpl-uuid", TemplateHash: "h1", HarnessConfig: "claude", Profile: "default"},
		}
	}
	has := func(keys []string, k string) bool {
		for _, x := range keys {
			if x == k {
				return true
			}
		}
		return false
	}

	t.Run("hydrated vertex-ai, no local template", func(t *testing.T) {
		srv, projectDir := newSrv(t)
		hydrated := filepath.Join(t.TempDir(), "hydrated", "mytpl")
		writeTpl(t, hydrated, "vertex-ai")

		required, _, _, _ := srv.extractRequiredEnvKeys(hubReq(projectDir), hydrated)
		if !has(required, "GOOGLE_CLOUD_PROJECT") {
			t.Errorf("hydrated template auth_selectedType vertex-ai ignored: required=%v", required)
		}
	})

	t.Run("hydrated vertex-ai, local slug says api-key", func(t *testing.T) {
		srv, projectDir := newSrv(t)
		writeTpl(t, filepath.Join(projectDir, "templates", "mytpl"), "api-key")
		hydrated := filepath.Join(t.TempDir(), "hydrated", "mytpl")
		writeTpl(t, hydrated, "vertex-ai")

		required, _, _, _ := srv.extractRequiredEnvKeys(hubReq(projectDir), hydrated)
		if !has(required, "GOOGLE_CLOUD_PROJECT") {
			t.Errorf("local template of the same slug was read instead of the hydrated one: required=%v", required)
		}
	})

	t.Run("hydrated has none, local slug says vertex-ai", func(t *testing.T) {
		srv, projectDir := newSrv(t)
		writeTpl(t, filepath.Join(projectDir, "templates", "mytpl"), "vertex-ai")
		hydrated := filepath.Join(t.TempDir(), "hydrated", "mytpl")
		writeTpl(t, hydrated, "")

		required, _, _, _ := srv.extractRequiredEnvKeys(hubReq(projectDir), hydrated)
		if has(required, "GOOGLE_CLOUD_PROJECT") {
			t.Errorf("local template auth_selectedType leaked into a hub-template preflight: required=%v", required)
		}
	})
}

// TestTemplateTiersCapability pins acceptance 6: heartbeat and info both
// advertise templateTiers.
func TestTemplateTiersCapability(t *testing.T) {
	t.Run("heartbeat", func(t *testing.T) {
		client := &mockRuntimeBrokerService{}
		svc := NewHeartbeatService(client, "b", time.Hour, &heartbeatMockManager{}, nil, slog.Default())
		hb := lastHeartbeat(t, svc, client)
		if hb.Capabilities == nil || !hb.Capabilities.TemplateTiers {
			t.Fatalf("heartbeat capabilities = %+v, want templateTiers", hb.Capabilities)
		}
		blob, err := json.Marshal(hb.Capabilities)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(blob), `"templateTiers":true`) {
			t.Errorf("heartbeat wire capabilities missing templateTiers: %s", blob)
		}
	})
	t.Run("info", func(t *testing.T) {
		srv := newTestServer(t)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/info", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("status %d", w.Code)
		}
		if !strings.Contains(w.Body.String(), `"templateTiers":true`) {
			t.Fatalf("info capabilities missing templateTiers: %s", w.Body.String())
		}
	})
}
