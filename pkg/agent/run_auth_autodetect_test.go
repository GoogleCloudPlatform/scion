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

package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// --- autoDetectAuthSelectedType: ptone/scion#1873 ---------------------------
//
// These pin the fix for the antigravity vertex-ai-via-passthrough gap: Start()
// used to leave auth.SelectedType empty whenever nothing explicit chose one,
// deferring entirely to the container-side provisioner's own (harness-local,
// default_type-blind) guess. autoDetectAuthSelectedType makes Go the single
// source of truth, reusing the same file -> env -> GCP-identity precedence
// the hub preflight (extractRequiredEnvKeys) already uses.

// antigravityLikeAuthMeta returns auth metadata shaped like
// harnesses/antigravity/config.yaml's auth block, minus the ambient
// GOOGLE_CLOUD_PROJECT->vertex-ai env autodetect entry — so tests that use it
// isolate the GCP-identity leg instead of the (separate, pre-existing,
// already-tested) env-presence leg.
func antigravityLikeAuthMeta() *config.HarnessAuthMetadata {
	return &config.HarnessAuthMetadata{
		DefaultType: "oauth-token",
		Types: map[string]config.HarnessAuthTypeMetadata{
			"oauth-token": {
				RequiredEnv: []config.HarnessAuthEnvRequirement{
					{AnyOf: []string{"AGY_TOKEN"}},
				},
			},
			"api-key": {
				RequiredEnv: []config.HarnessAuthEnvRequirement{
					{AnyOf: []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}},
				},
			},
			"vertex-ai": {
				RequiredEnv: []config.HarnessAuthEnvRequirement{
					{AnyOf: []string{"GOOGLE_CLOUD_PROJECT"}},
					{AnyOf: []string{"GOOGLE_CLOUD_LOCATION", "GOOGLE_CLOUD_REGION"}},
				},
				RequiredFiles: []config.HarnessAuthFileRequirement{
					{
						Name: "gcloud-adc", Type: "file", Field: "GoogleAppCredentials",
						AlternativeEnvKeys:                   []string{"GOOGLE_APPLICATION_CREDENTIALS"},
						SkippedWhenGCPServiceAccountAssigned: true,
						Required:                             true,
					},
				},
			},
		},
		Autodetect: config.HarnessAuthAutodetect{
			Env: map[string]string{
				"AGY_TOKEN":      "oauth-token",
				"GEMINI_API_KEY": "api-key",
				"GOOGLE_API_KEY": "api-key",
			},
			Files: map[string]string{"gcloud-adc": "vertex-ai"},
		},
	}
}

// TestAutoDetectAuthSelectedType_GCPIdentityPassthrough proves the core
// target scenario: with a GCP SA reachable via passthrough (signalled by
// SCION_METADATA_MODE, the only channel this crosses into pkg/agent) and no
// other credential present at all, vertex-ai is selected from identity alone
// — with no ADC file, no AGY_TOKEN, no API key. BrokerMode: true because the
// identity signal only exists on the broker path (R1).
func TestAutoDetectAuthSelectedType_GCPIdentityPassthrough(t *testing.T) {
	auth := &api.AuthConfig{}
	opts := &api.StartOptions{
		BrokerMode: true,
		Env: map[string]string{
			"SCION_METADATA_MODE": "passthrough",
		},
	}

	autoDetectAuthSelectedType(auth, antigravityLikeAuthMeta(), opts)

	if auth.SelectedType != "vertex-ai" {
		t.Errorf("SelectedType = %q, want %q (GCP SA reachable via passthrough)", auth.SelectedType, "vertex-ai")
	}
}

// TestAutoDetectAuthSelectedType_GCPIdentityAssign is the "assign" twin of
// the passthrough test above.
func TestAutoDetectAuthSelectedType_GCPIdentityAssign(t *testing.T) {
	auth := &api.AuthConfig{}
	opts := &api.StartOptions{
		BrokerMode: true,
		Env: map[string]string{
			"SCION_METADATA_MODE": "assign",
		},
	}

	autoDetectAuthSelectedType(auth, antigravityLikeAuthMeta(), opts)

	if auth.SelectedType != "vertex-ai" {
		t.Errorf("SelectedType = %q, want %q (GCP SA assigned)", auth.SelectedType, "vertex-ai")
	}
}

// TestAutoDetectAuthSelectedType_BlockModeNoAutoVertexAI is the regression
// guard for "no SA means no vertex-ai auto-selection from identity": block
// mode (or no GCP identity signal at all) must not auto-select vertex-ai.
func TestAutoDetectAuthSelectedType_BlockModeNoAutoVertexAI(t *testing.T) {
	for _, metadataMode := range []string{"block", ""} {
		t.Run("mode="+metadataMode, func(t *testing.T) {
			auth := &api.AuthConfig{}
			opts := &api.StartOptions{BrokerMode: true, Env: map[string]string{}}
			if metadataMode != "" {
				opts.Env["SCION_METADATA_MODE"] = metadataMode
			}

			autoDetectAuthSelectedType(auth, antigravityLikeAuthMeta(), opts)

			if auth.SelectedType != "" {
				t.Errorf("SelectedType = %q, want %q (no GCP SA available, must not auto-select vertex-ai)", auth.SelectedType, "")
			}
		})
	}
}

// TestAutoDetectAuthSelectedType_FileSecretBeatsIdentity proves the file leg
// runs first: an actually-staged ADC file secret should win even without any
// identity signal at all (e.g. plain local ADC login, no hub-managed GCP
// identity).
func TestAutoDetectAuthSelectedType_FileSecretBeatsIdentity(t *testing.T) {
	auth := &api.AuthConfig{}
	opts := &api.StartOptions{
		BrokerMode: true,
		Env:        map[string]string{}, // no SCION_METADATA_MODE at all
		ResolvedSecrets: []api.ResolvedSecret{
			{Name: "gcloud-adc", Type: "file", Target: "/home/scion/.config/gcloud/application_default_credentials.json"},
		},
	}

	autoDetectAuthSelectedType(auth, antigravityLikeAuthMeta(), opts)

	if auth.SelectedType != "vertex-ai" {
		t.Errorf("SelectedType = %q, want %q (staged ADC file present)", auth.SelectedType, "vertex-ai")
	}
}

// TestAutoDetectAuthSelectedType_EnvCredentialBeatsIdentity proves the env
// leg outranks the GCP-identity leg: an actually-present, non-default-type
// API key alongside a reachable GCP SA should still pick api-key, not
// vertex-ai — identity is the fallback, not a takeover.
func TestAutoDetectAuthSelectedType_EnvCredentialBeatsIdentity(t *testing.T) {
	auth := &api.AuthConfig{}
	opts := &api.StartOptions{
		BrokerMode: true,
		Env: map[string]string{
			"SCION_METADATA_MODE": "passthrough",
			"GEMINI_API_KEY":      "some-key",
		},
	}

	autoDetectAuthSelectedType(auth, antigravityLikeAuthMeta(), opts)

	if auth.SelectedType != "api-key" {
		t.Errorf("SelectedType = %q, want %q (GEMINI_API_KEY present should win over bare identity)", auth.SelectedType, "api-key")
	}
}

// TestAutoDetectAuthSelectedType_DefaultTypeCredentialBeatsIdentity is the
// pkg/agent regression pin for ptone/scion#1882 — the exact scenario the
// review found: antigravity's own default_type is "oauth-token", and AGY_TOKEN
// (which maps to it) must still win over a reachable GCP SA. Before the fix,
// AutoDetectAuthType's env leg returning "" (pickAutodetectCandidate's
// "already on default" signal, indistinguishable from "nothing matched" to a
// naively-chaining caller) let this function fall through to the identity leg
// and wrongly select vertex-ai, silently evicting AGY_TOKEN from the
// container. See TestAutoDetectAuthType_DefaultTypeCredentialBeatsGCPIdentity
// in pkg/harness for the same pin against the real harness configs
// (including claude's ANTHROPIC_API_KEY case).
func TestAutoDetectAuthSelectedType_DefaultTypeCredentialBeatsIdentity(t *testing.T) {
	auth := &api.AuthConfig{}
	opts := &api.StartOptions{
		BrokerMode: true,
		Env: map[string]string{
			"SCION_METADATA_MODE": "passthrough",
			"AGY_TOKEN":           "some-token",
		},
	}

	autoDetectAuthSelectedType(auth, antigravityLikeAuthMeta(), opts)

	if auth.SelectedType != "oauth-token" {
		t.Errorf("SelectedType = %q, want %q (AGY_TOKEN — antigravity's default_type credential — must win over a reachable GCP SA)", auth.SelectedType, "oauth-token")
	}
}

// TestAutoDetectAuthSelectedType_ExplicitSelectionWins proves the function is
// a no-op once something explicit (CLI --harness-auth, template, profile,
// scion-agent.json) has already set SelectedType, even in a passthrough
// environment that would otherwise auto-select vertex-ai.
func TestAutoDetectAuthSelectedType_ExplicitSelectionWins(t *testing.T) {
	auth := &api.AuthConfig{SelectedType: "oauth-token"}
	opts := &api.StartOptions{
		BrokerMode: true,
		Env: map[string]string{
			"SCION_METADATA_MODE": "passthrough",
		},
	}

	autoDetectAuthSelectedType(auth, antigravityLikeAuthMeta(), opts)

	if auth.SelectedType != "oauth-token" {
		t.Errorf("SelectedType = %q, want %q (explicit selection must not be overridden)", auth.SelectedType, "oauth-token")
	}
}

// TestAutoDetectAuthSelectedType_NilAuthMetaNoOp guards the nil-authMeta path
// (harness configs without a declarative auth: block).
func TestAutoDetectAuthSelectedType_NilAuthMetaNoOp(t *testing.T) {
	auth := &api.AuthConfig{}
	opts := &api.StartOptions{BrokerMode: true, Env: map[string]string{"SCION_METADATA_MODE": "passthrough"}}

	autoDetectAuthSelectedType(auth, nil, opts)

	if auth.SelectedType != "" {
		t.Errorf("SelectedType = %q, want %q (nil authMeta must not auto-select anything)", auth.SelectedType, "")
	}
}

// TestAutoDetectAuthSelectedType_LocalModeNoOp is the R1 regression guard:
// in local/workstation mode (BrokerMode: false), this function must not make
// a binding decision from opts.Env/opts.ResolvedSecrets alone. Those only
// reflect what the broker pre-resolved; local mode's GatherAuthWithEnv
// separately discovers host env vars (os.Getenv) and host credential files
// (e.g. ~/.claude/.credentials.json, the local ADC file) that never pass
// through opts at all, and ResolveAuth / provision.py's own ordering already
// handles that case correctly. The identity signal is also broker-only by
// construction, so it has nothing to contribute locally.
func TestAutoDetectAuthSelectedType_LocalModeNoOp(t *testing.T) {
	auth := &api.AuthConfig{}
	opts := &api.StartOptions{
		BrokerMode: false,
		Env: map[string]string{
			// Would resolve to vertex-ai via identity in broker mode.
			"SCION_METADATA_MODE": "passthrough",
		},
		ResolvedSecrets: []api.ResolvedSecret{
			// Would resolve to vertex-ai via file secret in broker mode.
			{Name: "gcloud-adc", Type: "file", Target: "/home/scion/.config/gcloud/application_default_credentials.json"},
		},
	}

	autoDetectAuthSelectedType(auth, antigravityLikeAuthMeta(), opts)

	if auth.SelectedType != "" {
		t.Errorf("SelectedType = %q, want %q (local mode must defer to GatherAuthWithEnv's own host discovery, not opts alone)", auth.SelectedType, "")
	}
}

// TestAutoDetectAuthSelectedType_Seam_ResolveAuthForwardsSelectedType is the
// R2 Start()-wiring test: it exercises the actual seam Start() relies on —
// autoDetectAuthSelectedType's result flowing into
// ContainerScriptHarness.ResolveAuth as SCION_HARNESS_SELECTED_AUTH, which
// container_script_harness.go stages as auth-candidates.json's
// "explicit_type" and the container-side provisioner (e.g. antigravity's
// provision.py) trusts instead of re-guessing.
func TestAutoDetectAuthSelectedType_Seam_ResolveAuthForwardsSelectedType(t *testing.T) {
	h, err := harness.NewContainerScriptHarness("/fake/harness-config-dir", config.HarnessConfigEntry{
		Harness:     "antigravity",
		Provisioner: &config.HarnessProvisionerConfig{Type: "container-script"},
		Auth:        antigravityLikeAuthMeta(),
	})
	if err != nil {
		t.Fatalf("NewContainerScriptHarness: %v", err)
	}

	auth := api.AuthConfig{}
	opts := &api.StartOptions{
		BrokerMode: true,
		Env:        map[string]string{"SCION_METADATA_MODE": "passthrough"},
	}
	autoDetectAuthSelectedType(&auth, antigravityLikeAuthMeta(), opts)
	if auth.SelectedType != "vertex-ai" {
		t.Fatalf("precondition failed: SelectedType = %q, want %q", auth.SelectedType, "vertex-ai")
	}

	resolved, err := h.ResolveAuth(auth)
	if err != nil {
		t.Fatalf("ResolveAuth: %v", err)
	}
	if got := resolved.EnvVars["SCION_HARNESS_SELECTED_AUTH"]; got != "vertex-ai" {
		t.Errorf("resolved.EnvVars[SCION_HARNESS_SELECTED_AUTH] = %q, want %q — this is what the container-side provisioner trusts as explicit_type",
			got, "vertex-ai")
	}
}

// TestAutoDetectAuthSelectedType_Seam_DefaultTypeCredentialSurvivesResolveAuth
// is the Start()-wiring counterpart to
// TestAutoDetectAuthSelectedType_DefaultTypeCredentialBeatsIdentity: it
// confirms the seam forwards "oauth-token", not "vertex-ai", so
// provision.py's explicit_type never evicts AGY_TOKEN even when a GCP SA is
// also reachable (C1, end to end through the actual ResolveAuth call).
func TestAutoDetectAuthSelectedType_Seam_DefaultTypeCredentialSurvivesResolveAuth(t *testing.T) {
	h, err := harness.NewContainerScriptHarness("/fake/harness-config-dir", config.HarnessConfigEntry{
		Harness:     "antigravity",
		Provisioner: &config.HarnessProvisionerConfig{Type: "container-script"},
		Auth:        antigravityLikeAuthMeta(),
	})
	if err != nil {
		t.Fatalf("NewContainerScriptHarness: %v", err)
	}

	auth := api.AuthConfig{}
	opts := &api.StartOptions{
		BrokerMode: true,
		Env: map[string]string{
			"SCION_METADATA_MODE": "passthrough",
			"AGY_TOKEN":           "some-token",
		},
	}
	autoDetectAuthSelectedType(&auth, antigravityLikeAuthMeta(), opts)
	if auth.SelectedType != "oauth-token" {
		t.Fatalf("precondition failed: SelectedType = %q, want %q", auth.SelectedType, "oauth-token")
	}

	resolved, err := h.ResolveAuth(auth)
	if err != nil {
		t.Fatalf("ResolveAuth: %v", err)
	}
	if got := resolved.EnvVars["SCION_HARNESS_SELECTED_AUTH"]; got != "oauth-token" {
		t.Errorf("resolved.EnvVars[SCION_HARNESS_SELECTED_AUTH] = %q, want %q (AGY_TOKEN's type must survive to explicit_type, not be evicted by identity)",
			got, "oauth-token")
	}
}

// antigravityLikeAuthMetaYAML is antigravityLikeAuthMeta's config.yaml auth
// block, staged on disk for TestStartBrokerMode_AutoDetectsAuthSelectedType
// below — that test needs a real container-script harness-config directory
// (harness.Resolve requires one on disk), not the in-memory
// config.HarnessConfigEntry the seam tests above construct directly.
const antigravityLikeAuthMetaYAML = `
auth:
  default_type: oauth-token
  types:
    oauth-token:
      required_env:
        - any_of: ["AGY_TOKEN"]
    api-key:
      required_env:
        - any_of: ["GEMINI_API_KEY", "GOOGLE_API_KEY"]
    vertex-ai:
      required_env:
        - any_of: ["GOOGLE_CLOUD_PROJECT"]
        - any_of: ["GOOGLE_CLOUD_LOCATION", "GOOGLE_CLOUD_REGION"]
      required_files:
        - name: gcloud-adc
          type: file
          field: GoogleAppCredentials
          alternative_env_keys: ["GOOGLE_APPLICATION_CREDENTIALS"]
          skipped_when_gcp_service_account_assigned: true
          required: true
  autodetect:
    env:
      AGY_TOKEN: oauth-token
      GEMINI_API_KEY: api-key
      GOOGLE_API_KEY: api-key
    files:
      gcloud-adc: vertex-ai
`

// TestStartBrokerMode_AutoDetectsAuthSelectedType is a full, broker-mode
// Start() test (ptone/scion#1882) proving the wiring at the
// autoDetectAuthSelectedType(&auth, authMeta, &opts) call in Start() actually
// reaches the container, using a real on-disk container-script harness-config
// (not the in-memory entry the seam tests above use) and a real
// MockRuntime.Run capture — the same TestStartBrokerMode_EmptyEnvNotFatal
// pattern.
//
// Asserts on capturedConfig.ResolvedAuth.EnvVars, not capturedConfig.Env:
// SCION_HARNESS_SELECTED_AUTH is forwarded via ContainerScriptHarness.ResolveAuth's
// returned api.ResolvedAuth (run.go's runCfg.ResolvedAuth = resolvedAuth), not
// merged into the flat container env slice (that slice comes from
// buildAgentEnv(finalScionCfg, opts.Env) — a separate path auth EnvVars never
// join at the Go level; runtime implementations read ResolvedAuth.EnvVars
// directly when actually launching a container, see e.g. pkg/runtime/common.go).
func TestStartBrokerMode_AutoDetectsAuthSelectedType(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "GCPIdentityPassthrough_SelectsVertexAI",
			env: map[string]string{
				"SCION_METADATA_MODE":   "passthrough",
				"GOOGLE_CLOUD_PROJECT":  "my-gcp-project",
				"GOOGLE_CLOUD_LOCATION": "global",
			},
			want: "vertex-ai",
		},
		{
			name: "DefaultTypeCredentialBeatsIdentity_SelectsOAuthToken",
			env: map[string]string{
				"SCION_METADATA_MODE": "passthrough",
				"AGY_TOKEN":           "some-token",
			},
			want: "oauth-token",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()

			t.Chdir(tmpDir)
			t.Setenv("HOME", tmpDir)

			globalScionDir := filepath.Join(tmpDir, ".scion")

			// A real on-disk container-script harness-config: harness.Resolve
			// requires hcDir.Path to be non-empty for the provisioner branch
			// (pkg/harness/resolve.go), which the in-memory entries the seam
			// tests use above cannot satisfy.
			hcDir := filepath.Join(globalScionDir, "harness-configs", "antigravity-test")
			if err := os.MkdirAll(hcDir, 0755); err != nil {
				t.Fatalf("mkdir harness-config dir: %v", err)
			}
			hcYAML := "harness: antigravity-test\nuser: scion\nimage: test-image:latest\n" +
				"provisioner:\n  type: container-script\n  command: [\"python3\", \"provision.py\"]\n" +
				antigravityLikeAuthMetaYAML
			if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte(hcYAML), 0644); err != nil {
				t.Fatalf("write harness-config config.yaml: %v", err)
			}

			tplDir := filepath.Join(globalScionDir, "templates", "default")
			if err := os.MkdirAll(tplDir, 0755); err != nil {
				t.Fatalf("mkdir template dir: %v", err)
			}
			if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "antigravity-test"}`), 0644); err != nil {
				t.Fatalf("write template scion-agent.json: %v", err)
			}

			if err := os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644); err != nil {
				t.Fatalf("write global settings.yaml: %v", err)
			}

			projectDir := filepath.Join(tmpDir, "project")
			projectScionDir := filepath.Join(projectDir, ".scion")
			if err := os.MkdirAll(projectScionDir, 0755); err != nil {
				t.Fatalf("mkdir project .scion dir: %v", err)
			}

			var capturedConfig runtime.RunConfig
			mockRT := &runtime.MockRuntime{
				ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
					return []api.AgentInfo{}, nil
				},
				RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
					capturedConfig = cfg
					return "mock-id", nil
				},
			}

			mgr := NewManager(mockRT)

			_, err := mgr.Start(context.Background(), api.StartOptions{
				Name:        "test-agent",
				ProjectPath: projectScionDir,
				BrokerMode:  true,
				Env:         tc.env,
			})
			if err != nil {
				t.Fatalf("Start failed: %v", err)
			}

			if capturedConfig.ResolvedAuth == nil {
				t.Fatal("capturedConfig.ResolvedAuth is nil")
			}
			if got := capturedConfig.ResolvedAuth.EnvVars["SCION_HARNESS_SELECTED_AUTH"]; got != tc.want {
				t.Errorf("ResolvedAuth.EnvVars[SCION_HARNESS_SELECTED_AUTH] = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestStartNoneAuthTypeTreatedAsNoAuth covers agents created in no-auth
// mode (ptone/scion#2561). Create persists auth_selectedType "none" to
// scion-agent.json; a later start, restart, resume or wake reaches Start
// without NoAuth set. "none" must select the no-auth path rather than be
// forwarded to the container-side provisioner as an auth type.
func TestStartNoneAuthTypeTreatedAsNoAuth(t *testing.T) {
	cases := []struct {
		name          string
		persistedAuth string
		harnessAuth   string
		env           map[string]string
		wantNoAuth    bool
		wantSelected  string
	}{
		{
			name:          "PersistedNone",
			persistedAuth: "none",
			wantNoAuth:    true,
		},
		{
			name:        "ExplicitNone",
			harnessAuth: "none",
			wantNoAuth:  true,
		},
		{
			name:          "ExplicitTypeOverridesPersistedNone",
			persistedAuth: "none",
			harnessAuth:   "api-key",
			env:           map[string]string{"GEMINI_API_KEY": "test-key"},
			wantSelected:  "api-key",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			t.Chdir(tmpDir)
			t.Setenv("HOME", tmpDir)

			globalScionDir := filepath.Join(tmpDir, ".scion")
			hcDir := filepath.Join(globalScionDir, "harness-configs", "noauth-test")
			if err := os.MkdirAll(hcDir, 0755); err != nil {
				t.Fatalf("mkdir harness-config dir: %v", err)
			}
			hcYAML := "harness: noauth-test\nuser: scion\nimage: test-image:latest\n" +
				"provisioner:\n  type: container-script\n  command: [\"python3\", \"provision.py\"]\n" +
				"no_auth:\n  behavior: drop-to-shell\n" +
				antigravityLikeAuthMetaYAML
			if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte(hcYAML), 0644); err != nil {
				t.Fatalf("write harness-config config.yaml: %v", err)
			}

			tplDir := filepath.Join(globalScionDir, "templates", "default")
			if err := os.MkdirAll(tplDir, 0755); err != nil {
				t.Fatalf("mkdir template dir: %v", err)
			}
			if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "noauth-test"}`), 0644); err != nil {
				t.Fatalf("write template scion-agent.json: %v", err)
			}
			if err := os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644); err != nil {
				t.Fatalf("write global settings.yaml: %v", err)
			}

			projectScionDir := filepath.Join(tmpDir, "project", ".scion")
			agentName := "noauth-agent"
			agentDir := filepath.Join(projectScionDir, "agents", agentName)
			agentHome := filepath.Join(agentDir, "home")
			if err := os.MkdirAll(agentHome, 0755); err != nil {
				t.Fatalf("mkdir agent home: %v", err)
			}
			agentCfg := `{"harness_config": "noauth-test"`
			if tc.persistedAuth != "" {
				agentCfg += `, "auth_selectedType": "` + tc.persistedAuth + `"`
			}
			agentCfg += `}`
			if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(agentCfg), 0644); err != nil {
				t.Fatalf("write agent scion-agent.json: %v", err)
			}

			candidatesPath := filepath.Join(agentHome, ".scion", "harness", "inputs", "auth-candidates.json")
			if tc.wantNoAuth {
				// Seed a stale auth-candidates.json, as create leaves
				// behind, so the test proves Start removes it on the
				// no-auth path.
				if err := os.MkdirAll(filepath.Dir(candidatesPath), 0755); err != nil {
					t.Fatalf("mkdir harness inputs dir: %v", err)
				}
				if err := os.WriteFile(candidatesPath, []byte(`{"explicit_type":"none"}`), 0644); err != nil {
					t.Fatalf("write stale auth-candidates.json: %v", err)
				}
			}

			var capturedConfig runtime.RunConfig
			mockRT := &runtime.MockRuntime{
				ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
					return []api.AgentInfo{}, nil
				},
				RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
					capturedConfig = cfg
					return "mock-id", nil
				},
			}

			mgr := NewManager(mockRT)
			_, err := mgr.Start(context.Background(), api.StartOptions{
				Name:        agentName,
				ProjectPath: projectScionDir,
				BrokerMode:  true,
				HarnessAuth: tc.harnessAuth,
				Env:         tc.env,
			})
			if err != nil {
				t.Fatalf("Start failed: %v", err)
			}

			if capturedConfig.NoAuth != tc.wantNoAuth {
				t.Errorf("RunConfig.NoAuth = %t, want %t", capturedConfig.NoAuth, tc.wantNoAuth)
			}
			var selected string
			if capturedConfig.ResolvedAuth != nil {
				selected = capturedConfig.ResolvedAuth.EnvVars["SCION_HARNESS_SELECTED_AUTH"]
			}
			if selected != tc.wantSelected {
				t.Errorf("SCION_HARNESS_SELECTED_AUTH = %q, want %q", selected, tc.wantSelected)
			}

			data, err := os.ReadFile(candidatesPath)
			if tc.wantNoAuth {
				if !os.IsNotExist(err) {
					t.Errorf("auth-candidates.json present after no-auth Start (err=%v): %s", err, data)
				}
				return
			}
			if err != nil {
				t.Fatalf("read auth-candidates.json: %v", err)
			}
			var payload map[string]interface{}
			if err := json.Unmarshal(data, &payload); err != nil {
				t.Fatalf("parse auth-candidates.json: %v", err)
			}
			if got := payload["explicit_type"]; got != tc.wantSelected {
				t.Errorf("explicit_type = %v, want %q", got, tc.wantSelected)
			}
		})
	}
}
