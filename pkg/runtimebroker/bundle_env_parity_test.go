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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
)

// TestBundleEnvLayer_PreflightLaunchParity verifies that the runtime broker
// preflight (extractRequiredEnvKeys) and launch (agent.BundleEnvLayer) agree
// on every Decision 4 literal-bundle case:
//  1. ${VAR} syntax in a bundle value is kept literally (non-empty) and satisfies preflight.
//  2. Empty bundle value with the key set in broker host env is NOT satisfied (never reads host).
//  3. Empty bundle value filled by hub ResolvedEnv IS satisfied.
func TestBundleEnvLayer_PreflightLaunchParity(t *testing.T) {
	unsetHostGCPEnv(t)
	t.Setenv("PARITY_HOST_ONLY", "from-broker-host")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "from-broker-host")

	t.Run("literal var syntax satisfies both preflight and launch", func(t *testing.T) {
		_ = os.Unsetenv("UNSET_PARITY_VAR")
		rawBundle := map[string]string{
			"GOOGLE_CLOUD_PROJECT":  "${UNSET_PARITY_VAR}/proj",
			"GOOGLE_CLOUD_LOCATION": "us-east5",
		}
		entries, warnings := agent.BundleEnvLayer(rawBundle)
		if entries["GOOGLE_CLOUD_PROJECT"] != "${UNSET_PARITY_VAR}/proj" {
			t.Fatalf("BundleEnvLayer GOOGLE_CLOUD_PROJECT = %q, want literal", entries["GOOGLE_CLOUD_PROJECT"])
		}
		if len(warnings) == 0 || !strings.Contains(strings.Join(warnings, "\n"), "harness-config env values are not expanded") {
			t.Errorf("expected not-expanded warning from BundleEnvLayer, got %v", warnings)
		}

		srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
			"harness: claude\nimage: test-image\nuser: scion\nauth_selected_type: vertex-ai\n"+
				"env:\n  GOOGLE_CLOUD_PROJECT: \"${UNSET_PARITY_VAR}/proj\"\n  GOOGLE_CLOUD_LOCATION: us-east5\n"+claudeAuthBlock,
			`
schema_version: "1"
harness_configs:
  claude:
    harness: claude
profiles:
  default:
    runtime: mock
`)
		// Project-tier harness-config env variable syntax is refused by
		// hostedguard on a hosted broker before reaching BundleEnvLayer.
		srv.config.WorkstationMode = true
		body := `{
			"name": "parity-literal",
			"id": "agent-parity-literal",
			"gatherEnv": true,
			"projectPath": "` + projectDir + `",
			"config": {"template": "claude", "profile": "default", "gcpIdentity": {"metadata_mode": "passthrough"}}
		}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("preflight status = %d, want 201 (parity with BundleEnvLayer); body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("empty bundle value with host env set is missing in both preflight and launch", func(t *testing.T) {
		rawBundle := map[string]string{
			"GOOGLE_CLOUD_PROJECT":  "",
			"GOOGLE_CLOUD_LOCATION": "us-east5",
		}
		entries, _ := agent.BundleEnvLayer(rawBundle)
		if v, ok := entries["GOOGLE_CLOUD_PROJECT"]; !ok || v != "" {
			t.Fatalf("BundleEnvLayer GOOGLE_CLOUD_PROJECT = (%q, %v), want (\"\", true)", v, ok)
		}

		srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
			"harness: claude\nimage: test-image\nuser: scion\nauth_selected_type: vertex-ai\n"+
				"env:\n  GOOGLE_CLOUD_PROJECT: \"\"\n  GOOGLE_CLOUD_LOCATION: us-east5\n"+claudeAuthBlock,
			`
schema_version: "1"
harness_configs:
  claude:
    harness: claude
profiles:
  default:
    runtime: mock
`)
		// Project-tier empty bundle env values are refused by hostedguard on a
		// hosted broker before reaching BundleEnvLayer.
		srv.config.WorkstationMode = true
		body := `{
			"name": "parity-empty-host",
			"id": "agent-parity-empty-host",
			"gatherEnv": true,
			"projectPath": "` + projectDir + `",
			"config": {"template": "claude", "profile": "default", "gcpIdentity": {"metadata_mode": "passthrough"}}
		}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusAccepted {
			t.Fatalf("preflight status = %d, want 202 (empty bundle value must not read host env); body=%s", w.Code, w.Body.String())
		}
		var envReqs EnvRequirementsResponse
		if err := json.Unmarshal(w.Body.Bytes(), &envReqs); err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(envReqs.Needs, "GOOGLE_CLOUD_PROJECT") {
			t.Errorf("preflight Needs = %v, want GOOGLE_CLOUD_PROJECT", envReqs.Needs)
		}
	})

	t.Run("empty bundle value filled by hub ResolvedEnv satisfies both", func(t *testing.T) {
		srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
			"harness: claude\nimage: test-image\nuser: scion\nauth_selected_type: vertex-ai\n"+
				"env:\n  GOOGLE_CLOUD_PROJECT: \"\"\n  GOOGLE_CLOUD_LOCATION: us-east5\n"+claudeAuthBlock,
			`
schema_version: "1"
harness_configs:
  claude:
    harness: claude
profiles:
  default:
    runtime: mock
`)
		// Project-tier empty bundle env values are refused by hostedguard on a
		// hosted broker before reaching BundleEnvLayer.
		srv.config.WorkstationMode = true
		body := `{
			"name": "parity-hub-filled",
			"id": "agent-parity-hub-filled",
			"gatherEnv": true,
			"projectPath": "` + projectDir + `",
			"resolvedEnv": {"GOOGLE_CLOUD_PROJECT": "hub-proj"},
			"config": {"template": "claude", "profile": "default", "gcpIdentity": {"metadata_mode": "passthrough"}}
		}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusCreated {
			t.Fatalf("preflight status = %d, want 201 (hub ResolvedEnv fills empty bundle key); body=%s", w.Code, w.Body.String())
		}
	})
}
