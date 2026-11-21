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
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

func TestRestorableSecretName(t *testing.T) {
	authMeta := &config.HarnessAuthMetadata{
		Types: map[string]config.HarnessAuthTypeMetadata{
			"api-key": {RequiredEnv: []config.HarnessAuthEnvRequirement{{AnyOf: []string{"PRIMARY_API_KEY", "SECONDARY_API_KEY"}}}},
			"auth-file": {RequiredFiles: []config.HarnessAuthFileRequirement{
				{Name: "HOST_CRED_FILE", TargetSuffix: ".tool/creds.json", AlternativeEnvKeys: []string{"ALT_FILE_ENV_KEY"}},
				{Name: "gcloud-adc", TargetSuffix: ".config/gcloud/application_default_credentials.json"},
			}},
		},
		Autodetect: config.HarnessAuthAutodetect{Env: map[string]string{"AUTODETECT_ONLY_KEY": "api-key"}},
	}

	h, _ := newTestContainerScriptHarness(t)
	h.entry.Harness, h.entry.Auth = "gemini-cli", authMeta
	baseSynth := []string{"SCION_HARNESS_SELECTED_AUTH", "GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_REGION", "GOOGLE_CLOUD_LOCATION"}
	claudeSynth := []string{"ANTHROPIC_VERTEX_PROJECT_ID", "CLOUD_ML_REGION", "CLAUDE_CODE_USE_VERTEX"}
	for _, name := range append([]string{"PRIMARY_API_KEY", "SECONDARY_API_KEY", "HOST_CRED_FILE", "ALT_FILE_ENV_KEY", "AUTODETECT_ONLY_KEY"}, baseSynth...) {
		if !h.RestorableSecretName(name) {
			t.Errorf("RestorableSecretName(%q) = false, want true", name)
		}
	}
	for _, name := range append([]string{"REMOVED_TOKEN", "gcloud-adc", "", ".identity.json", "../SECRET", "1LEADING_DIGIT", "FOO/BAR"}, claudeSynth...) {
		if h.RestorableSecretName(name) {
			t.Errorf("RestorableSecretName(%q) = true, want false", name)
		}
	}

	h.entry.Harness = "claude"
	for _, name := range claudeSynth {
		if !h.RestorableSecretName(name) {
			t.Errorf("claude RestorableSecretName(%q) = false, want true", name)
		}
	}
	h.entry.Auth = nil
	for _, name := range append(baseSynth, claudeSynth...) {
		if !h.RestorableSecretName(name) {
			t.Errorf("Auth==nil claude RestorableSecretName(%q) = false, want true", name)
		}
	}
	if h.RestorableSecretName("ANTHROPIC_API_KEY") {
		t.Error("Auth==nil RestorableSecretName(ANTHROPIC_API_KEY) = true, want false")
	}
	h.entry.Harness = "generic"
	if h.RestorableSecretName("CLAUDE_CODE_USE_VERTEX") {
		t.Error("Auth==nil non-claude RestorableSecretName(CLAUDE_CODE_USE_VERTEX) = true, want false")
	}
}

func TestResolveAuthSynthesisedKeysAreRestorable(t *testing.T) {
	names := AllHarnessNames()
	if len(names) == 0 {
		t.Fatal("AllHarnessNames() returned no harnesses")
	}
	baseCfg := api.AuthConfig{
		GoogleCloudProject: "my-project", GoogleCloudRegion: "us-central1",
		GoogleAppCredentials: "/tmp/fake-adc.json", GCPMetadataMode: "assign",
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			dir, err := filepath.Abs(filepath.Join("..", "..", "harnesses", name))
			if err != nil {
				t.Fatal(err)
			}
			hc, err := config.LoadHarnessConfigDir(dir)
			if err != nil {
				t.Fatalf("LoadHarnessConfigDir(%s): %v", dir, err)
			}
			h, err := NewContainerScriptHarness(dir, hc.Config)
			if err != nil {
				t.Fatalf("NewContainerScriptHarness(%s): %v", name, err)
			}
			types := []string{"", AuthTypeNone}
			allEnv, allFiles := map[string]string{}, map[string]string{}
			if hc.Config.Auth != nil {
				var typeNames []string
				for tn, tm := range hc.Config.Auth.Types {
					typeNames = append(typeNames, tn)
					for _, req := range tm.RequiredEnv {
						for _, k := range req.AnyOf {
							allEnv[k] = "test-val"
						}
					}
					for _, rf := range tm.RequiredFiles {
						if rf.Field != "" {
							allFiles[rf.Field] = "/tmp/fake-" + rf.Field
						}
						for _, k := range rf.AlternativeEnvKeys {
							allEnv[k] = "test-val"
						}
					}
				}
				for k := range hc.Config.Auth.Autodetect.Env {
					allEnv[k] = "test-val"
				}
				sort.Strings(typeNames)
				types = append(types, typeNames...)
			}
			noAuthMeta := *h
			noAuthMeta.entry.Auth = nil
			for _, selected := range types {
				for _, tc := range []struct {
					target *ContainerScriptHarness
					env    map[string]string
				}{{h, allEnv}, {h, nil}, {&noAuthMeta, nil}} {
					cfg := baseCfg
					cfg.SelectedType, cfg.EnvVars, cfg.Files = selected, tc.env, allFiles
					resolved, err := tc.target.ResolveAuth(cfg)
					if err != nil {
						t.Fatalf("ResolveAuth(selected=%q): %v", selected, err)
					}
					for k := range resolved.EnvVars {
						if !tc.target.RestorableSecretName(k) {
							t.Errorf("harness %s (selected=%q): ResolveAuth produced %q, not restorable", name, selected, k)
						}
					}
				}
			}
		})
	}
}

func TestApplyAuthSettings_FreshResolutionSatisfiedAuth(t *testing.T) {
	f := newRestartFixture(t)
	if err := f.h.ApplyAuthSettings(f.agentHome, &api.ResolvedAuth{
		Method:  "container-script",
		EnvVars: map[string]string{"SCION_HARNESS_SELECTED_AUTH": "api-key", "ANTHROPIC_API_KEY": "sk-ant-test"},
	}); err != nil || !f.h.FreshResolutionSatisfiedAuth() {
		t.Fatalf("complete api-key ApplyAuthSettings: err=%v fresh=%v, want nil/true", err, f.h.FreshResolutionSatisfiedAuth())
	}

	f2 := newRestartFixture(t)
	if err := os.MkdirAll(f2.secretsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"GOOGLE_CLOUD_PROJECT": "recorded-proj", "ANTHROPIC_VERTEX_PROJECT_ID": "recorded-proj", "ANTHROPIC_API_KEY": "recorded-key",
	} {
		if err := os.WriteFile(filepath.Join(f2.secretsDir(), k), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f2.h.SetRecordedSecrets([]string{"GOOGLE_CLOUD_PROJECT", "ANTHROPIC_VERTEX_PROJECT_ID", "ANTHROPIC_API_KEY"})
	if err := f2.h.ApplyAuthSettings(f2.agentHome, &api.ResolvedAuth{
		Method: "container-script",
		EnvVars: map[string]string{
			"SCION_HARNESS_SELECTED_AUTH": "vertex-ai", "GOOGLE_CLOUD_REGION": "us-east5",
			"GOOGLE_CLOUD_LOCATION": "us-east5", "CLOUD_ML_REGION": "us-east5", "CLAUDE_CODE_USE_VERTEX": "1",
		},
	}); err != nil || f2.h.FreshResolutionSatisfiedAuth() {
		t.Fatalf("partial vertex-ai ApplyAuthSettings: err=%v fresh=%v, want nil/false", err, f2.h.FreshResolutionSatisfiedAuth())
	}

	if err := os.WriteFile(filepath.Join(f2.secretsDir(), "ANTHROPIC_API_KEY"), []byte("recorded-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	f2.h.SetRecordedSecrets([]string{"ANTHROPIC_API_KEY"})
	if err := f2.h.ApplyAuthSettings(f2.agentHome, &api.ResolvedAuth{
		Method:  "container-script",
		EnvVars: map[string]string{"SCION_HARNESS_SELECTED_AUTH": "api-key"},
	}); err != nil || f2.h.FreshResolutionSatisfiedAuth() {
		t.Fatalf("recorded-only api-key ApplyAuthSettings: err=%v fresh=%v, want nil/false", err, f2.h.FreshResolutionSatisfiedAuth())
	}
}
