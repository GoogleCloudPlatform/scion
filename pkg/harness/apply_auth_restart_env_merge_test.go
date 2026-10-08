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
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// claudeLikeAuth mirrors the auth block of harnesses/claude/config.yaml.
func claudeLikeAuth() *config.HarnessAuthMetadata {
	return &config.HarnessAuthMetadata{
		DefaultType: "api-key",
		Types: map[string]config.HarnessAuthTypeMetadata{
			"api-key":     {RequiredEnv: []config.HarnessAuthEnvRequirement{{AnyOf: []string{"ANTHROPIC_API_KEY"}}}},
			"oauth-token": {RequiredEnv: []config.HarnessAuthEnvRequirement{{AnyOf: []string{"CLAUDE_CODE_OAUTH_TOKEN"}}}},
			"auth-file": {RequiredFiles: []config.HarnessAuthFileRequirement{
				{Name: "CLAUDE_AUTH", Type: "file", TargetSuffix: "/.claude/.credentials.json"},
			}},
			"vertex-ai": {
				RequiredEnv: []config.HarnessAuthEnvRequirement{
					{AnyOf: []string{"GOOGLE_CLOUD_PROJECT"}},
					{AnyOf: []string{"GOOGLE_CLOUD_REGION", "CLOUD_ML_REGION", "GOOGLE_CLOUD_LOCATION"}},
				},
				RequiredFiles: []config.HarnessAuthFileRequirement{
					{Name: "gcloud-adc", Type: "file", AlternativeEnvKeys: []string{"GOOGLE_APPLICATION_CREDENTIALS"}, SkippedWhenGCPServiceAccountAssigned: true, Required: true},
				},
			},
		},
	}
}

type restartFixture struct {
	t         *testing.T
	h         *ContainerScriptHarness
	agentHome string
}

func newRestartFixture(t *testing.T) *restartFixture {
	t.Helper()
	h, _ := newTestContainerScriptHarness(t)
	h.entry.Auth = claudeLikeAuth()
	return &restartFixture{t: t, h: h, agentHome: t.TempDir()}
}

func (f *restartFixture) secretsDir() string {
	return filepath.Join(f.agentHome, ".scion", "harness", "secrets")
}

// firstStart applies env and returns the recorded secret names and contents,
// as the control plane's recordSecrets would capture them.
func (f *restartFixture) firstStart(env map[string]string) map[string][]byte {
	f.t.Helper()
	if err := f.h.ApplyAuthSettings(f.agentHome, &api.ResolvedAuth{Method: "container-script", EnvVars: env}); err != nil {
		f.t.Fatalf("first ApplyAuthSettings: %v", err)
	}
	rec := map[string][]byte{}
	for _, name := range f.h.StagedSecretNames() {
		data, err := os.ReadFile(filepath.Join(f.secretsDir(), name))
		if err != nil {
			f.t.Fatal(err)
		}
		rec[name] = data
	}
	return rec
}

// restart mimics run.go: the bundle is cleared, the recorded secrets are
// restored, then ApplyAuthSettings runs with this start's resolution.
func (f *restartFixture) restart(rec map[string][]byte, env map[string]string) (envSecrets map[string]string) {
	f.t.Helper()
	return f.restartResolved(rec, &api.ResolvedAuth{Method: "container-script", EnvVars: env})
}

// restartResolved is restart with a full resolution (e.g. from ResolveAuth).
func (f *restartFixture) restartResolved(rec map[string][]byte, resolved *api.ResolvedAuth) (envSecrets map[string]string) {
	f.t.Helper()
	if err := os.RemoveAll(f.secretsDir()); err != nil {
		f.t.Fatal(err)
	}
	if err := os.MkdirAll(f.secretsDir(), 0o700); err != nil {
		f.t.Fatal(err)
	}
	var names []string
	for name, data := range rec {
		if err := os.WriteFile(filepath.Join(f.secretsDir(), name), data, 0o600); err != nil {
			f.t.Fatal(err)
		}
		names = append(names, name)
	}
	f.h.SetRecordedSecrets(names)
	if err := f.h.ApplyAuthSettings(f.agentHome, resolved); err != nil {
		f.t.Fatalf("restart ApplyAuthSettings: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(f.agentHome, ".scion", "harness", "inputs", "auth-candidates.json"))
	if err != nil {
		f.t.Fatal(err)
	}
	var payload struct {
		EnvSecretFiles map[string]string `json:"env_secret_files"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		f.t.Fatal(err)
	}
	return payload.EnvSecretFiles
}

func (f *restartFixture) secretValue(name string) string {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.secretsDir(), name))
	if err != nil {
		f.t.Fatalf("read secret %s: %v", name, err)
	}
	return string(data)
}

// A restart whose resolution carries only ambient env (a
// region var) but not the credential must still reference the recorded
// credential, and must keep it in StagedSecretNames so the record keeps it.
func TestApplyAuthSettings_RestartWithAmbientEnvCarriesRecordedCredential(t *testing.T) {
	f := newRestartFixture(t)
	rec := f.firstStart(map[string]string{
		"ANTHROPIC_API_KEY":     "sk-ant-first-0123456789",
		"GOOGLE_CLOUD_LOCATION": "us-east5",
	})
	got := f.restart(rec, map[string]string{"GOOGLE_CLOUD_LOCATION": "europe-west1", "CLOUD_ML_REGION": "europe-west1"})
	if got["ANTHROPIC_API_KEY"] != "$HOME/.scion/harness/secrets/ANTHROPIC_API_KEY" {
		t.Fatalf("recorded ANTHROPIC_API_KEY not carried on restart: %v", got)
	}
	if f.secretValue("ANTHROPIC_API_KEY") != "sk-ant-first-0123456789" {
		t.Errorf("recorded credential content changed")
	}
	// The new resolution's own value wins for a key it supplies.
	if f.secretValue("GOOGLE_CLOUD_LOCATION") != "europe-west1" {
		t.Errorf("GOOGLE_CLOUD_LOCATION = %q, want the new resolution's value", f.secretValue("GOOGLE_CLOUD_LOCATION"))
	}
	if _, ok := got["CLOUD_ML_REGION"]; !ok {
		t.Errorf("new ambient key missing: %v", got)
	}
	staged := map[string]bool{}
	for _, n := range f.h.StagedSecretNames() {
		staged[n] = true
	}
	if !staged["ANTHROPIC_API_KEY"] {
		t.Errorf("carried credential missing from StagedSecretNames %v; the record would lose it", f.h.StagedSecretNames())
	}
}

// Ambient vertex env without ADC does not satisfy vertex-ai, so it does not
// stop the recorded credential from being carried.
func TestApplyAuthSettings_RestartWithPartialVertexEnvCarriesRecordedCredential(t *testing.T) {
	f := newRestartFixture(t)
	rec := f.firstStart(map[string]string{"ANTHROPIC_API_KEY": "sk-ant-first-0123456789"})
	got := f.restart(rec, map[string]string{"GOOGLE_CLOUD_PROJECT": "p", "GOOGLE_CLOUD_REGION": "us-central1"})
	if _, ok := got["ANTHROPIC_API_KEY"]; !ok {
		t.Fatalf("recorded ANTHROPIC_API_KEY not carried: %v", got)
	}
}

// An explicitly selected type that the new resolution does not satisfy
// carries the recorded credential even when another type is satisfied.
func TestApplyAuthSettings_RestartExplicitTypeUnsatisfiedCarriesRecordedCredential(t *testing.T) {
	f := newRestartFixture(t)
	rec := f.firstStart(map[string]string{"ANTHROPIC_API_KEY": "sk-ant-first-0123456789"})
	// vertex-ai is fully resolved (project, region and the ADC file mapping
	// ResolveAuth produces), but the explicitly selected type is api-key.
	got := f.restartResolved(rec, &api.ResolvedAuth{
		Method: "container-script",
		EnvVars: map[string]string{
			"SCION_HARNESS_SELECTED_AUTH": "api-key",
			"GOOGLE_CLOUD_PROJECT":        "p",
			"GOOGLE_CLOUD_REGION":         "us-central1",
		},
		Files: []api.FileMapping{{ContainerPath: adcContainerPath}},
	})
	if _, ok := got["ANTHROPIC_API_KEY"]; !ok {
		t.Fatalf("recorded ANTHROPIC_API_KEY not carried for explicit api-key: %v", got)
	}
}

// Rotation: a credential newly resolved for a different auth type must not
// be shadowed by the stale recorded one (api-key outranks oauth-token in the
// provisioner's selection order).
func TestApplyAuthSettings_RestartRotatedCredentialNotShadowed(t *testing.T) {
	f := newRestartFixture(t)
	rec := f.firstStart(map[string]string{"ANTHROPIC_API_KEY": "sk-ant-stale-0123456789"})
	got := f.restart(rec, map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "oat-new"})
	if _, ok := got["ANTHROPIC_API_KEY"]; ok {
		t.Fatalf("stale recorded ANTHROPIC_API_KEY shadows the newly resolved oauth token: %v", got)
	}
	if _, ok := got["CLAUDE_CODE_OAUTH_TOKEN"]; !ok {
		t.Fatalf("new credential missing: %v", got)
	}
	for _, n := range f.h.StagedSecretNames() {
		if n == "ANTHROPIC_API_KEY" {
			t.Errorf("stale credential would be re-recorded: %v", f.h.StagedSecretNames())
		}
	}
}

// Rotation of the same key: the new value is staged and wins.
func TestApplyAuthSettings_RestartSameKeyRotatedUsesNewValue(t *testing.T) {
	f := newRestartFixture(t)
	rec := f.firstStart(map[string]string{"ANTHROPIC_API_KEY": "sk-ant-old-0123456789"})
	got := f.restart(rec, map[string]string{"ANTHROPIC_API_KEY": "sk-ant-new-0123456789"})
	if _, ok := got["ANTHROPIC_API_KEY"]; !ok {
		t.Fatalf("ANTHROPIC_API_KEY missing: %v", got)
	}
	if v := f.secretValue("ANTHROPIC_API_KEY"); v != "sk-ant-new-0123456789" {
		t.Errorf("ANTHROPIC_API_KEY = %q, want the newly resolved value", v)
	}
}

// For every bundled harness whose vertex-ai type requires the gcloud-adc
// file, the decision is driven through the real ResolveAuth: a restart
// that resolves only project/region (no ADC) still carries the recorded
// API key, while a restart that switches to vertex-ai with an ADC file
// (which ResolveAuth turns into a file mapping, not an env var) does not
// carry it, so the provisioner selects vertex-ai rather than the stale key.
func TestApplyAuthSettings_RestartVertexSwitchThroughResolveAuth(t *testing.T) {
	adc := filepath.Join(t.TempDir(), "adc.json")
	if err := os.WriteFile(adc, []byte(`{"type":"authorized_user"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ harness, keyEnv string }{
		{"claude", "ANTHROPIC_API_KEY"},
		{"gemini-cli", "GEMINI_API_KEY"},
		{"antigravity", "GEMINI_API_KEY"},
		{"hermes", "OPENAI_API_KEY"},
		{"grok-build", "XAI_API_KEY"},
	} {
		t.Run(tc.harness, func(t *testing.T) {
			newFixture := func() *restartFixture {
				dir, err := filepath.Abs(filepath.Join("..", "..", "harnesses", tc.harness))
				if err != nil {
					t.Fatal(err)
				}
				hc, err := config.LoadHarnessConfigDir(dir)
				if err != nil {
					t.Fatalf("load %s: %v", dir, err)
				}
				h, err := NewContainerScriptHarness(dir, hc.Config)
				if err != nil {
					t.Fatal(err)
				}
				if h.entry.Auth == nil || h.entry.Auth.Types["vertex-ai"].RequiredFiles == nil {
					t.Fatalf("fixture: %s has no vertex-ai required_files", tc.harness)
				}
				return &restartFixture{t: t, h: h, agentHome: t.TempDir()}
			}
			resolve := func(f *restartFixture, auth api.AuthConfig) *api.ResolvedAuth {
				r, err := f.h.ResolveAuth(auth)
				if err != nil {
					t.Fatalf("ResolveAuth: %v", err)
				}
				return r
			}
			first := api.AuthConfig{EnvVars: map[string]string{tc.keyEnv: "key-0123456789abcdef"}}

			// Ambient project/region only: the recorded key is carried.
			f := newFixture()
			if err := f.h.ApplyAuthSettings(f.agentHome, resolve(f, first)); err != nil {
				t.Fatal(err)
			}
			rec := map[string][]byte{tc.keyEnv: []byte("key-0123456789abcdef")}
			got := f.restartResolved(rec, resolve(f, api.AuthConfig{GoogleCloudProject: "p", GoogleCloudRegion: "us-east5"}))
			if _, ok := got[tc.keyEnv]; !ok {
				t.Errorf("ambient-only restart: recorded %s not carried: %v", tc.keyEnv, got)
			}

			// Switch to vertex-ai with an ADC file: the stale key is not carried.
			f = newFixture()
			if err := f.h.ApplyAuthSettings(f.agentHome, resolve(f, first)); err != nil {
				t.Fatal(err)
			}
			switched := resolve(f, api.AuthConfig{GoogleCloudProject: "p", GoogleCloudRegion: "us-east5", GoogleAppCredentials: adc})
			if len(switched.Files) == 0 {
				t.Fatalf("fixture: ResolveAuth produced no ADC file mapping: %+v", switched)
			}
			if _, ok := switched.EnvVars["GOOGLE_APPLICATION_CREDENTIALS"]; ok {
				t.Fatalf("fixture: ResolveAuth unexpectedly produced GOOGLE_APPLICATION_CREDENTIALS as env")
			}
			got = f.restartResolved(rec, switched)
			if _, ok := got[tc.keyEnv]; ok {
				t.Errorf("vertex-ai switch: stale recorded %s shadows the resolved ADC: %v", tc.keyEnv, got)
			}
		})
	}
}
