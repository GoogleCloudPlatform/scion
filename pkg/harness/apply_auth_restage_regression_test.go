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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// TestApplyAuthSettings_CarriesOnlyRecordedSecrets covers the restart path:
// the control plane restores exactly the secret files it recorded from the
// previous ApplyAuthSettings (StagedSecretNames) and passes their names via
// SetRecordedSecrets. A safe-named file written into the secrets directory by
// anything else is never referenced, neither as an env secret (when the new
// resolution has none) nor as a file secret (a name matching required_files).
func TestApplyAuthSettings_CarriesOnlyRecordedSecrets(t *testing.T) {
	h, _ := newTestContainerScriptHarness(t)
	agentHome := t.TempDir()
	h.entry.Auth = &config.HarnessAuthMetadata{
		Types: map[string]config.HarnessAuthTypeMetadata{
			"claude": {RequiredFiles: []config.HarnessAuthFileRequirement{{Name: "CLAUDE_AUTH", TargetSuffix: ".claude/.credentials.json"}}},
		},
	}
	credSrc := filepath.Join(t.TempDir(), "creds.json")
	if err := os.WriteFile(credSrc, []byte(`{"token":"t"}`), 0600); err != nil {
		t.Fatal(err)
	}

	// First start: env secrets and a file secret staged from the resolution.
	first := &api.ResolvedAuth{
		Method:  "vertex-ai",
		EnvVars: map[string]string{"GOOGLE_CLOUD_PROJECT": "my-project", "GOOGLE_CLOUD_REGION": "us-central1"},
		Files:   []api.FileMapping{{SourcePath: credSrc, ContainerPath: "~/.claude/.credentials.json"}},
	}
	if err := h.ApplyAuthSettings(agentHome, first); err != nil {
		t.Fatalf("first ApplyAuthSettings: %v", err)
	}
	recorded := h.StagedSecretNames()
	want := []string{"CLAUDE_AUTH", "GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_REGION"}
	if strings.Join(recorded, ",") != strings.Join(want, ",") {
		t.Fatalf("StagedSecretNames = %v, want %v", recorded, want)
	}
	secretDir := filepath.Join(agentHome, ".scion", "harness", "secrets")
	contents := map[string][]byte{}
	for _, name := range recorded {
		data, err := os.ReadFile(filepath.Join(secretDir, name))
		if err != nil {
			t.Fatal(err)
		}
		contents[name] = data
	}

	// Restart: the secrets directory starts empty, the control plane restores
	// the recorded files, and safe-named files that were not recorded exist for
	// both merge paths.
	if err := os.RemoveAll(secretDir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(secretDir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range contents {
		if name == "CLAUDE_AUTH" {
			continue // not recorded in this pass, see below
		}
		if err := os.WriteFile(filepath.Join(secretDir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{"UNRECORDED_TOKEN": "unrecorded", "CLAUDE_AUTH": "unrecorded"} {
		if err := os.WriteFile(filepath.Join(secretDir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	h.SetRecordedSecrets([]string{"GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_REGION"})
	if err := h.ApplyAuthSettings(agentHome, &api.ResolvedAuth{Method: "container-script", EnvVars: map[string]string{}}); err != nil {
		t.Fatalf("restart ApplyAuthSettings: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(agentHome, ".scion", "harness", "inputs", "auth-candidates.json"))
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		EnvSecretFiles  map[string]string `json:"env_secret_files"`
		FileSecretFiles map[string]string `json:"file_secret_files"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_REGION"} {
		if _, ok := payload.EnvSecretFiles[name]; !ok {
			t.Errorf("recorded env secret %s not carried: %v", name, payload.EnvSecretFiles)
		}
	}
	if _, ok := payload.EnvSecretFiles["UNRECORDED_TOKEN"]; ok {
		t.Error("an unrecorded file was referenced as an env secret")
	}
	if _, ok := payload.FileSecretFiles["CLAUDE_AUTH"]; ok {
		t.Error("an unrecorded file matching a required_files name was referenced as a file secret")
	}
	if got := strings.Join(h.StagedSecretNames(), ","); got != "GOOGLE_CLOUD_PROJECT,GOOGLE_CLOUD_REGION" {
		t.Errorf("StagedSecretNames after restart = %q", got)
	}

	// A recorded file-type secret is carried when the resolution does not
	// re-supply it.
	if err := os.WriteFile(filepath.Join(secretDir, "CLAUDE_AUTH"), contents["CLAUDE_AUTH"], 0600); err != nil {
		t.Fatal(err)
	}
	h.SetRecordedSecrets([]string{"CLAUDE_AUTH"})
	if err := h.ApplyAuthSettings(agentHome, &api.ResolvedAuth{Method: "container-script", EnvVars: map[string]string{"GOOGLE_CLOUD_PROJECT": "p"}}); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(agentHome, ".scion", "harness", "inputs", "auth-candidates.json"))
	payload.FileSecretFiles = nil
	_ = json.Unmarshal(data, &payload)
	if payload.FileSecretFiles["CLAUDE_AUTH"] != "$HOME/.scion/harness/secrets/CLAUDE_AUTH" {
		t.Errorf("recorded file secret not carried: %v", payload.FileSecretFiles)
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

// StagedSecretNames lists only secret files staged under
// $HOME/.scion/harness/secrets/. A broker-mode file secret (no host source)
// is referenced at its harness-native container path and is not listed.
func TestApplyAuthSettings_StagedSecretNamesOnlySecretsDir(t *testing.T) {
	h, _ := newTestContainerScriptHarness(t)
	agentHome := t.TempDir()
	h.entry.Auth = &config.HarnessAuthMetadata{
		Types: map[string]config.HarnessAuthTypeMetadata{
			"claude": {RequiredFiles: []config.HarnessAuthFileRequirement{{Name: "CLAUDE_AUTH", TargetSuffix: ".claude/.credentials.json"}}},
		},
	}
	resolved := &api.ResolvedAuth{
		Method:  "container-script",
		EnvVars: map[string]string{"GOOGLE_CLOUD_PROJECT": "p"},
		Files:   []api.FileMapping{{SourcePath: "", ContainerPath: "~/.claude/.credentials.json"}},
	}
	if err := h.ApplyAuthSettings(agentHome, resolved); err != nil {
		t.Fatalf("ApplyAuthSettings: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(agentHome, ".scion", "harness", "inputs", "auth-candidates.json"))
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		FileSecretFiles map[string]string `json:"file_secret_files"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.FileSecretFiles["CLAUDE_AUTH"] != "$HOME/.claude/.credentials.json" {
		t.Fatalf("fixture: expected the native-path file secret reference, got %v", payload.FileSecretFiles)
	}
	if got := strings.Join(h.StagedSecretNames(), ","); got != "GOOGLE_CLOUD_PROJECT" {
		t.Errorf("StagedSecretNames = %q, want only the secrets-dir file GOOGLE_CLOUD_PROJECT", got)
	}
}
