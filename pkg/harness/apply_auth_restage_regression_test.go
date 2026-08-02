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

package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// TestApplyAuthSettings_RestagePreservesExistingSecretFiles is a regression test for #723.
//
// On agent restart, ApplyAuthSettings is called again. If the resolved auth has
// empty EnvVars (because the auth gathering on the restart path didn't find
// credentials), the newly written auth-candidates.json has empty
// env_secret_files: {}. But the secret files from the original creation still
// exist on disk.
//
// Expected behavior: the auth-candidates.json should reference any existing
// secret files on disk if the new resolution produces empty env vars.
//
// This test demonstrates the bug: after a successful first provision that wrote
// secret files and auth-candidates.json with proper references, a second call
// to ApplyAuthSettings with empty env vars produces auth-candidates.json with
// empty env_secret_files, even though the secret files still exist on disk.
func TestApplyAuthSettings_RestagePreservesExistingSecretFiles(t *testing.T) {
	h, _ := newTestContainerScriptHarness(t)
	agentHome := t.TempDir()

	// ---- First call: simulate initial creation with valid Vertex AI auth ----
	firstResolved := &api.ResolvedAuth{
		Method: "vertex-ai",
		EnvVars: map[string]string{
			"SCION_HARNESS_SELECTED_AUTH": "vertex-ai",
			"GOOGLE_CLOUD_PROJECT":        "my-project",
			"GOOGLE_CLOUD_REGION":         "us-central1",
			"GOOGLE_CLOUD_LOCATION":       "us-central1",
		},
	}
	if err := h.ApplyAuthSettings(agentHome, firstResolved); err != nil {
		t.Fatalf("first ApplyAuthSettings: %v", err)
	}

	// Verify first call produced correct auth-candidates.json
	firstData, err := os.ReadFile(filepath.Join(agentHome, ".scion", "harness", "inputs", "auth-candidates.json"))
	if err != nil {
		t.Fatalf("read first auth-candidates.json: %v", err)
	}
	var firstPayload map[string]interface{}
	if err := json.Unmarshal(firstData, &firstPayload); err != nil {
		t.Fatalf("unmarshal first auth-candidates.json: %v", err)
	}

	// Verify secret files were staged
	secretDir := filepath.Join(agentHome, ".scion", "harness", "secrets")
	for _, name := range []string{"GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_REGION", "GOOGLE_CLOUD_LOCATION"} {
		data, err := os.ReadFile(filepath.Join(secretDir, name))
		if err != nil {
			t.Fatalf("secret file %s not staged: %v", name, err)
		}
		if len(data) == 0 {
			t.Fatalf("secret file %s is empty", name)
		}
	}

	// Verify env_secret_files references the staged files
	firstEnvSecrets, ok := firstPayload["env_secret_files"].(map[string]interface{})
	if !ok {
		t.Fatal("first auth-candidates.json missing env_secret_files map")
	}
	if len(firstEnvSecrets) == 0 {
		t.Fatal("first auth-candidates.json has empty env_secret_files; expected references to staged secrets")
	}
	if _, ok := firstEnvSecrets["GOOGLE_CLOUD_PROJECT"]; !ok {
		t.Error("first auth-candidates.json missing GOOGLE_CLOUD_PROJECT in env_secret_files")
	}

	// ---- Second call: simulate restart with empty auth (the bug) ----
	// On restart, GatherAuthWithEnv returns empty credentials because
	// GOOGLE_CLOUD_PROJECT doesn't reach the auth overlay.
	restartResolved := &api.ResolvedAuth{
		Method:  "container-script",
		EnvVars: map[string]string{},
	}
	if err := h.ApplyAuthSettings(agentHome, restartResolved); err != nil {
		t.Fatalf("restart ApplyAuthSettings: %v", err)
	}

	// Read the re-staged auth-candidates.json
	restartData, err := os.ReadFile(filepath.Join(agentHome, ".scion", "harness", "inputs", "auth-candidates.json"))
	if err != nil {
		t.Fatalf("read restart auth-candidates.json: %v", err)
	}
	var restartPayload map[string]interface{}
	if err := json.Unmarshal(restartData, &restartPayload); err != nil {
		t.Fatalf("unmarshal restart auth-candidates.json: %v", err)
	}

	// Verify secret files still exist on disk (they do — they're never deleted)
	for _, name := range []string{"GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_REGION", "GOOGLE_CLOUD_LOCATION"} {
		if _, err := os.Stat(filepath.Join(secretDir, name)); err != nil {
			t.Fatalf("secret file %s was removed during restage: %v", name, err)
		}
	}

	// BUG: The re-staged auth-candidates.json has empty env_secret_files
	// because the restart resolved auth had empty EnvVars, so
	// stageEnvSecretFiles returns an empty map. The existing secret files
	// are ignored.
	//
	// The test asserts the DESIRED behavior: auth-candidates.json should
	// still reference the existing secret files on disk.
	restartEnvSecrets, ok := restartPayload["env_secret_files"].(map[string]interface{})
	if !ok {
		t.Fatal("restart auth-candidates.json missing env_secret_files map")
	}
	if len(restartEnvSecrets) == 0 {
		t.Error("restart auth-candidates.json has empty env_secret_files — " +
			"existing secret files from creation are not referenced. " +
			"Bug #723: ApplyAuthSettings should preserve references to " +
			"existing secret files when new resolution has empty env vars")
	}
}

// TestApplyAuthSettings_RestageOverwritesExistingCandidates verifies the basic
// overwrite semantics: a second call to ApplyAuthSettings with valid (non-empty)
// credentials should produce a correct auth-candidates.json that replaces the
// first one.
func TestApplyAuthSettings_RestageOverwritesExistingCandidates(t *testing.T) {
	h, _ := newTestContainerScriptHarness(t)
	agentHome := t.TempDir()

	// First call: API key auth
	firstResolved := &api.ResolvedAuth{
		Method: "api-key",
		EnvVars: map[string]string{
			"SCION_HARNESS_SELECTED_AUTH": "api-key",
			"ANTHROPIC_API_KEY":           "sk-ant-first",
		},
	}
	if err := h.ApplyAuthSettings(agentHome, firstResolved); err != nil {
		t.Fatalf("first ApplyAuthSettings: %v", err)
	}

	// Second call: Vertex AI auth (credential rotation)
	secondResolved := &api.ResolvedAuth{
		Method: "vertex-ai",
		EnvVars: map[string]string{
			"SCION_HARNESS_SELECTED_AUTH": "vertex-ai",
			"GOOGLE_CLOUD_PROJECT":        "my-project",
			"GOOGLE_CLOUD_REGION":         "us-central1",
		},
	}
	if err := h.ApplyAuthSettings(agentHome, secondResolved); err != nil {
		t.Fatalf("second ApplyAuthSettings: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(agentHome, ".scion", "harness", "inputs", "auth-candidates.json"))
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	// Should reflect the second call's auth type
	if payload["explicit_type"] != "vertex-ai" {
		t.Errorf("explicit_type=%v, want vertex-ai", payload["explicit_type"])
	}
	if payload["resolved_method"] != "vertex-ai" {
		t.Errorf("resolved_method=%v, want vertex-ai", payload["resolved_method"])
	}

	// Should have GOOGLE_CLOUD_PROJECT in env_secret_files, not ANTHROPIC_API_KEY
	envSecrets, ok := payload["env_secret_files"].(map[string]interface{})
	if !ok {
		t.Fatal("missing env_secret_files")
	}
	if _, ok := envSecrets["GOOGLE_CLOUD_PROJECT"]; !ok {
		t.Error("expected GOOGLE_CLOUD_PROJECT in env_secret_files")
	}
	if _, ok := envSecrets["ANTHROPIC_API_KEY"]; ok {
		t.Error("ANTHROPIC_API_KEY should not be in env_secret_files after re-staging with vertex-ai")
	}
}
