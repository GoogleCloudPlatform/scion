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
	"io/fs"
	"reflect"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"gopkg.in/yaml.v3"

	harnessesEmbed "github.com/GoogleCloudPlatform/scion/harnesses"
)

func loadAuthMetaFromHarness(t *testing.T, harnessName string) *config.HarnessAuthMetadata {
	t.Helper()

	data, err := fs.ReadFile(harnessesEmbed.FS, harnessName+"/config.yaml")
	if err != nil {
		t.Fatalf("read config.yaml from harnesses/ embed: %v", err)
	}
	var entry config.HarnessConfigEntry
	if err := yaml.Unmarshal(data, &entry); err != nil {
		t.Fatalf("parse config.yaml: %v", err)
	}
	if entry.Auth == nil {
		t.Fatalf("harness %q config has no auth metadata", harnessName)
	}
	return entry.Auth
}

func TestAuthMetadataAvailable(t *testing.T) {
	cases := []struct {
		name  string
		entry *config.HarnessConfigEntry
		want  bool
	}{
		{"nil entry", nil, false},
		{"nil auth", &config.HarnessConfigEntry{}, false},
		{"empty auth block", &config.HarnessConfigEntry{Auth: &config.HarnessAuthMetadata{}}, false},
		{"types only", &config.HarnessConfigEntry{Auth: &config.HarnessAuthMetadata{
			Types: map[string]config.HarnessAuthTypeMetadata{"api-key": {}},
		}}, true},
		{"autodetect env only", &config.HarnessConfigEntry{Auth: &config.HarnessAuthMetadata{
			Autodetect: config.HarnessAuthAutodetect{Env: map[string]string{"FOO": "bar"}},
		}}, true},
		{"autodetect files only", &config.HarnessConfigEntry{Auth: &config.HarnessAuthMetadata{
			Autodetect: config.HarnessAuthAutodetect{Files: map[string]string{"FOO": "bar"}},
		}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AuthMetadataAvailable(tc.entry); got != tc.want {
				t.Errorf("AuthMetadataAvailable() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRequiredAuthEnvKeysFromConfig_ParityWithCompiled verifies that the
// config-driven preflight returns identical results to the compiled tables
// for all built-in harnesses (claude, codex, gemini-cli).
func TestRequiredAuthEnvKeysFromConfig_ParityWithCompiled(t *testing.T) {
	cases := []struct {
		harness     string
		compiledKey string
		types       []string
	}{
		{"claude", "claude", []string{"", "api-key", "oauth-token", "auth-file", "vertex-ai", "unknown"}},
		{"gemini-cli", "gemini-cli", []string{"", "api-key", "auth-file", "vertex-ai", "unknown"}},
		{"codex", "codex", []string{"", "api-key", "auth-file", "unknown"}},
	}
	for _, tc := range cases {
		authMeta := loadAuthMetaFromHarness(t, tc.harness)
		for _, at := range tc.types {
			t.Run(tc.harness+"/"+at, func(t *testing.T) {
				wantGroups := RequiredAuthEnvKeys(tc.compiledKey, at)
				gotGroups := RequiredAuthEnvKeysFromConfig(authMeta, at)
				if !equalGroups(gotGroups, wantGroups) {
					t.Errorf("config-driven=%v compiled=%v", gotGroups, wantGroups)
				}
			})
		}
	}
}

// TestRequiredAuthSecretsFromConfig_ParityWithCompiled verifies parity for
// all built-in harnesses (claude, codex, gemini-cli).
func TestRequiredAuthSecretsFromConfig_ParityWithCompiled(t *testing.T) {
	cases := []struct {
		harness     string
		compiledKey string
		types       []string
	}{
		{"claude", "claude", []string{"", "api-key", "auth-file", "vertex-ai"}},
		{"gemini-cli", "gemini-cli", []string{"", "api-key", "auth-file", "vertex-ai"}},
		// Codex does not support vertex-ai (capabilities: vertex_ai: no), so
		// the config-driven path correctly returns nil for vertex-ai. The
		// compiled RequiredAuthSecrets was overly broad, returning gcloud-adc
		// for codex+vertex-ai even though codex can't use it.
		{"codex", "codex", []string{"", "api-key", "auth-file"}},
	}
	for _, tc := range cases {
		authMeta := loadAuthMetaFromHarness(t, tc.harness)
		for _, at := range tc.types {
			for _, sa := range []bool{false, true} {
				name := tc.harness + "/" + at
				if sa {
					name += "/sa-assigned"
				}
				t.Run(name, func(t *testing.T) {
					want := RequiredAuthSecrets(tc.compiledKey, at, sa)
					got := RequiredAuthSecretsFromConfig(authMeta, at, sa)
					if !equalRequiredSecrets(got, want) {
						t.Errorf("config-driven=%+v compiled=%+v", got, want)
					}
				})
			}
		}
	}
}

func TestDetectAuthTypeFromEnvVarsFromConfig_Claude(t *testing.T) {
	authMeta := loadAuthMetaFromHarness(t, "claude")
	cases := []struct {
		name string
		keys []string
		want string
	}{
		{"empty", nil, ""},
		{"only api key", []string{"ANTHROPIC_API_KEY"}, ""},
		{"only oauth", []string{"CLAUDE_CODE_OAUTH_TOKEN"}, "oauth-token"},
		{"only GAC", []string{"GOOGLE_APPLICATION_CREDENTIALS"}, "vertex-ai"},
		{"only GCP_PROJECT", []string{"GOOGLE_CLOUD_PROJECT"}, "vertex-ai"},
		{"oauth wins over GAC", []string{"CLAUDE_CODE_OAUTH_TOKEN", "GOOGLE_APPLICATION_CREDENTIALS"}, "oauth-token"},
		{"oauth wins over GCP", []string{"CLAUDE_CODE_OAUTH_TOKEN", "GOOGLE_CLOUD_PROJECT"}, "oauth-token"},
		{"api key wins over GAC", []string{"ANTHROPIC_API_KEY", "GOOGLE_APPLICATION_CREDENTIALS"}, ""},
		{"api key wins over oauth", []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"}, ""},
		{"unrelated key", []string{"PATH"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DetectAuthTypeFromEnvVarsFromConfig(authMeta, keySet(tc.keys))
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestDetectAuthTypeFromEnvVarsFromConfig_Gemini(t *testing.T) {
	authMeta := loadAuthMetaFromHarness(t, "gemini-cli")
	cases := []struct {
		name string
		keys []string
		want string
	}{
		{"empty", nil, ""},
		{"GEMINI key", []string{"GEMINI_API_KEY"}, ""},
		{"GOOGLE key", []string{"GOOGLE_API_KEY"}, ""},
		{"GAC", []string{"GOOGLE_APPLICATION_CREDENTIALS"}, "vertex-ai"},
		{"GCP project", []string{"GOOGLE_CLOUD_PROJECT"}, "vertex-ai"},
		{"GEMINI key wins over GAC", []string{"GEMINI_API_KEY", "GOOGLE_APPLICATION_CREDENTIALS"}, ""},
		{"GOOGLE key wins over GCP project", []string{"GOOGLE_API_KEY", "GOOGLE_CLOUD_PROJECT"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DetectAuthTypeFromEnvVarsFromConfig(authMeta, keySet(tc.keys))
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestDetectAuthTypeFromFileSecretsFromConfig(t *testing.T) {
	cases := []struct {
		harness string
		name    string
		files   []string
		want    string
	}{
		{"claude", "CLAUDE_AUTH only", []string{"CLAUDE_AUTH"}, "auth-file"},
		{"claude", "gcloud-adc only", []string{"gcloud-adc"}, "vertex-ai"},
		{"claude", "auth-file wins over vertex-ai", []string{"CLAUDE_AUTH", "gcloud-adc"}, "auth-file"},
		{"gemini-cli", "OAUTH wins", []string{"GEMINI_OAUTH_CREDS", "gcloud-adc"}, "auth-file"},
		{"gemini-cli", "gcloud-adc only", []string{"gcloud-adc"}, "vertex-ai"},
	}
	for _, tc := range cases {
		t.Run(tc.harness+"/"+tc.name, func(t *testing.T) {
			authMeta := loadAuthMetaFromHarness(t, tc.harness)
			got := DetectAuthTypeFromFileSecretsFromConfig(authMeta, keySet(tc.files))
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestDetectAuthTypeFromGCPIdentityFromConfig(t *testing.T) {
	cases := []struct {
		harness  string
		assigned bool
		want     string
	}{
		{"claude", true, "vertex-ai"},
		{"claude", false, ""},
		{"gemini-cli", true, "vertex-ai"},
		{"gemini-cli", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.harness, func(t *testing.T) {
			authMeta := loadAuthMetaFromHarness(t, tc.harness)
			got := DetectAuthTypeFromGCPIdentityFromConfig(authMeta, tc.assigned)
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

// TestDetectAuthType_NilOrEmptyMeta verifies the *FromConfig functions are
// safe to call with nil or empty metadata — they must return zero values
// rather than panic so the broker can pass them unconditionally.
func TestDetectAuthType_NilOrEmptyMeta(t *testing.T) {
	keys := keySet([]string{"FOO", "ANTHROPIC_API_KEY"})

	if got := DetectAuthTypeFromEnvVarsFromConfig(nil, keys); got != "" {
		t.Errorf("env detection on nil meta: got %q", got)
	}
	if got := DetectAuthTypeFromFileSecretsFromConfig(nil, keys); got != "" {
		t.Errorf("file detection on nil meta: got %q", got)
	}
	if got := DetectAuthTypeFromGCPIdentityFromConfig(nil, true); got != "" {
		t.Errorf("gcp detection on nil meta: got %q", got)
	}
	if got := RequiredAuthEnvKeysFromConfig(nil, "api-key"); got != nil {
		t.Errorf("required env on nil meta: got %v", got)
	}
	if got := RequiredAuthSecretsFromConfig(nil, "vertex-ai", false); got != nil {
		t.Errorf("required secrets on nil meta: got %v", got)
	}

	empty := &config.HarnessAuthMetadata{}
	if got := DetectAuthTypeFromEnvVarsFromConfig(empty, keys); got != "" {
		t.Errorf("env detection on empty meta: got %q", got)
	}
}

// TestRequiredAuthSecretsFromConfig_GCPSAAssignedSkips verifies the
// SkippedWhenGCPServiceAccountAssigned flag is honored — vertex-ai with a
// GCP service account should not require the gcloud-adc file.
func TestRequiredAuthSecretsFromConfig_GCPSAAssignedSkips(t *testing.T) {
	authMeta := loadAuthMetaFromHarness(t, "claude")
	got := RequiredAuthSecretsFromConfig(authMeta, "vertex-ai", true)
	if got != nil {
		t.Errorf("expected nil with GCP SA assigned, got %v", got)
	}
	got = RequiredAuthSecretsFromConfig(authMeta, "vertex-ai", false)
	if len(got) != 1 || got[0].Key != "gcloud-adc" {
		t.Errorf("expected [gcloud-adc] without GCP SA, got %v", got)
	}
}

// TestDetectAuthTypeFromEnvVars_ParityWithCompiled verifies that for every
// built-in harness the config-driven env-var detection produces the same
// result as the compiled switch-case, across all env key combinations tested
// in TestDetectAuthTypeFromEnvVars.
func TestDetectAuthTypeFromEnvVars_ParityWithCompiled(t *testing.T) {
	cases := []struct {
		harness     string
		compiledKey string
		envKeySets  [][]string
	}{
		{
			"claude", "claude",
			[][]string{
				{},
				{"ANTHROPIC_API_KEY"},
				{"CLAUDE_CODE_OAUTH_TOKEN"},
				{"GOOGLE_APPLICATION_CREDENTIALS"},
				{"GOOGLE_CLOUD_PROJECT"},
				{"CLAUDE_CODE_OAUTH_TOKEN", "GOOGLE_APPLICATION_CREDENTIALS"},
				{"CLAUDE_CODE_OAUTH_TOKEN", "GOOGLE_CLOUD_PROJECT"},
				{"ANTHROPIC_API_KEY", "GOOGLE_APPLICATION_CREDENTIALS"},
				{"ANTHROPIC_API_KEY", "GOOGLE_CLOUD_PROJECT"},
				{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"},
				{"SOME_OTHER_VAR"},
			},
		},
		{
			"gemini-cli", "gemini-cli",
			[][]string{
				{},
				{"GEMINI_API_KEY"},
				{"GOOGLE_API_KEY"},
				{"GOOGLE_APPLICATION_CREDENTIALS"},
				{"GOOGLE_CLOUD_PROJECT"},
				{"GEMINI_API_KEY", "GOOGLE_APPLICATION_CREDENTIALS"},
				{"GEMINI_API_KEY", "GOOGLE_CLOUD_PROJECT"},
				{"GOOGLE_API_KEY", "GOOGLE_APPLICATION_CREDENTIALS"},
				{"GOOGLE_API_KEY", "GOOGLE_CLOUD_PROJECT"},
				{"CLAUDE_CODE_OAUTH_TOKEN"}, // should not affect gemini
			},
		},
		{
			"codex", "codex",
			[][]string{
				{},
				{"CODEX_API_KEY"},
				{"OPENAI_API_KEY"},
				{"GOOGLE_APPLICATION_CREDENTIALS"},
			},
		},
	}
	for _, tc := range cases {
		authMeta := loadAuthMetaFromHarness(t, tc.harness)
		for _, keys := range tc.envKeySets {
			name := tc.harness + "/"
			if len(keys) == 0 {
				name += "empty"
			} else {
				for i, k := range keys {
					if i > 0 {
						name += "+"
					}
					name += k
				}
			}
			t.Run(name, func(t *testing.T) {
				ks := keySet(keys)
				want := DetectAuthTypeFromEnvVars(tc.compiledKey, ks)
				got := DetectAuthTypeFromEnvVarsFromConfig(authMeta, ks)
				if got != want {
					t.Errorf("config-driven=%q compiled=%q", got, want)
				}
			})
		}
	}
}

// TestDetectAuthTypeFromFileSecrets_ParityWithCompiled verifies that for every
// built-in harness the config-driven file-secret detection produces the same
// result as the compiled switch-case.
func TestDetectAuthTypeFromFileSecrets_ParityWithCompiled(t *testing.T) {
	cases := []struct {
		harness     string
		compiledKey string
		fileSets    [][]string
	}{
		{
			"claude", "claude",
			[][]string{
				{},
				{"CLAUDE_AUTH"},
				{"gcloud-adc"},
				{"CLAUDE_AUTH", "gcloud-adc"},
			},
		},
		{
			"gemini-cli", "gemini-cli",
			[][]string{
				{},
				{"GEMINI_OAUTH_CREDS"},
				{"gcloud-adc"},
				{"GEMINI_OAUTH_CREDS", "gcloud-adc"},
			},
		},
		{
			"codex", "codex",
			[][]string{
				{},
				{"CODEX_AUTH"},
				{"gcloud-adc"}, // codex compiled doesn't handle gcloud-adc
			},
		},
	}
	for _, tc := range cases {
		authMeta := loadAuthMetaFromHarness(t, tc.harness)
		for _, files := range tc.fileSets {
			name := tc.harness + "/"
			if len(files) == 0 {
				name += "empty"
			} else {
				for i, f := range files {
					if i > 0 {
						name += "+"
					}
					name += f
				}
			}
			t.Run(name, func(t *testing.T) {
				ks := keySet(files)
				want := DetectAuthTypeFromFileSecrets(tc.compiledKey, ks)
				got := DetectAuthTypeFromFileSecretsFromConfig(authMeta, ks)
				if got != want {
					t.Errorf("config-driven=%q compiled=%q", got, want)
				}
			})
		}
	}
}

// TestDetectAuthTypeFromGCPIdentity_ParityWithCompiled verifies that for every
// built-in harness the config-driven GCP identity detection produces the same
// result as the compiled switch-case.
func TestDetectAuthTypeFromGCPIdentity_ParityWithCompiled(t *testing.T) {
	cases := []struct {
		harness     string
		compiledKey string
	}{
		{"claude", "claude"},
		{"gemini-cli", "gemini-cli"},
		{"codex", "codex"},
	}
	for _, tc := range cases {
		authMeta := loadAuthMetaFromHarness(t, tc.harness)
		for _, assigned := range []bool{false, true} {
			name := tc.harness
			if assigned {
				name += "/sa-assigned"
			}
			t.Run(name, func(t *testing.T) {
				want := DetectAuthTypeFromGCPIdentity(tc.compiledKey, assigned)
				got := DetectAuthTypeFromGCPIdentityFromConfig(authMeta, assigned)
				if got != want {
					t.Errorf("config-driven=%q compiled=%q", got, want)
				}
			})
		}
	}
}

// TestGatherConfigEnvVars_ParityWithCompiledLookup verifies that the
// config-driven env var gathering from harness config metadata produces
// the same set of keys as the compiled GatherAuthWithEnv hardcoded lookups
// for built-in harnesses (when all env vars are set).
func TestGatherConfigEnvVars_ParityWithCompiledLookup(t *testing.T) {
	cases := []struct {
		harness string
		envVars map[string]string
		// compiledFields are the hardcoded AuthConfig field values expected from
		// the compiled path. The config-driven path stores the same values in
		// AuthConfig.EnvVars. We check that every key present in the compiled
		// fields is also in EnvVars with the same value.
		compiledFields func(auth api.AuthConfig) map[string]string
	}{
		{
			"claude",
			map[string]string{
				"ANTHROPIC_API_KEY":      "test-key",
				"CLAUDE_CODE_OAUTH_TOKEN": "test-token",
				"GOOGLE_CLOUD_PROJECT":   "test-project",
				"GOOGLE_CLOUD_REGION":    "us-central1",
				"CLOUD_ML_REGION":        "ml-region",
				"GOOGLE_CLOUD_LOCATION":  "location",
			},
			func(auth api.AuthConfig) map[string]string {
				m := map[string]string{}
				if auth.AnthropicAPIKey != "" {
					m["ANTHROPIC_API_KEY"] = auth.AnthropicAPIKey
				}
				if auth.ClaudeOAuthToken != "" {
					m["CLAUDE_CODE_OAUTH_TOKEN"] = auth.ClaudeOAuthToken
				}
				return m
			},
		},
		{
			"codex",
			map[string]string{
				"CODEX_API_KEY":  "test-codex-key",
				"OPENAI_API_KEY": "test-openai-key",
			},
			func(auth api.AuthConfig) map[string]string {
				m := map[string]string{}
				if auth.CodexAPIKey != "" {
					m["CODEX_API_KEY"] = auth.CodexAPIKey
				}
				if auth.OpenAIAPIKey != "" {
					m["OPENAI_API_KEY"] = auth.OpenAIAPIKey
				}
				return m
			},
		},
		{
			"gemini-cli",
			map[string]string{
				"GEMINI_API_KEY":        "test-gemini-key",
				"GOOGLE_API_KEY":        "test-google-key",
				"GOOGLE_CLOUD_PROJECT":  "test-project",
				"GOOGLE_CLOUD_REGION":   "us-central1",
				"CLOUD_ML_REGION":       "ml-region",
				"GOOGLE_CLOUD_LOCATION": "location",
			},
			func(auth api.AuthConfig) map[string]string {
				m := map[string]string{}
				if auth.GeminiAPIKey != "" {
					m["GEMINI_API_KEY"] = auth.GeminiAPIKey
				}
				if auth.GoogleAPIKey != "" {
					m["GOOGLE_API_KEY"] = auth.GoogleAPIKey
				}
				return m
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.harness, func(t *testing.T) {
			authMeta := loadAuthMetaFromHarness(t, tc.harness)
			auth := GatherAuthWithEnv(tc.envVars, false, authMeta)

			// Check that config-driven EnvVars has every harness-specific env key
			compiledFields := tc.compiledFields(auth)
			for k, v := range compiledFields {
				if got, ok := auth.EnvVars[k]; !ok {
					t.Errorf("EnvVars missing key %q (compiled had value %q)", k, v)
				} else if got != v {
					t.Errorf("EnvVars[%q] = %q, compiled = %q", k, got, v)
				}
			}
		})
	}
}

// equalGroups compares two [][]string for value equality, treating nil
// and length-0 as equivalent.
func equalGroups(a, b [][]string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !reflect.DeepEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

// equalRequiredSecrets compares two []api.RequiredSecret slices, treating
// nil and length-0 as equivalent. AlternativeEnvKeys is normalized so a nil
// slice matches an empty one.
func equalRequiredSecrets(a, b []api.RequiredSecret) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Key != b[i].Key || a[i].Type != b[i].Type || a[i].Description != b[i].Description {
			return false
		}
		if !reflect.DeepEqual(append([]string{}, a[i].AlternativeEnvKeys...), append([]string{}, b[i].AlternativeEnvKeys...)) {
			return false
		}
	}
	return true
}

// keySet builds a set from a slice of keys.
func keySet(keys []string) map[string]struct{} {
	out := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		out[k] = struct{}{}
	}
	return out
}
