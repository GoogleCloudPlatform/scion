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

// TestRequiredAuthEnvKeysFromConfig_BuiltInHarnesses verifies that the
// config-driven preflight returns correct results for all built-in harnesses.
func TestRequiredAuthEnvKeysFromConfig_BuiltInHarnesses(t *testing.T) {
	cases := []struct {
		harness  string
		authType string
		want     [][]string
	}{
		// Claude
		{"claude", "", [][]string{{"ANTHROPIC_API_KEY"}}},
		{"claude", "api-key", [][]string{{"ANTHROPIC_API_KEY"}}},
		{"claude", "oauth-token", [][]string{{"CLAUDE_CODE_OAUTH_TOKEN"}}},
		{"claude", "auth-file", nil},
		{"claude", "vertex-ai", [][]string{{"GOOGLE_CLOUD_PROJECT"}, {"GOOGLE_CLOUD_REGION", "CLOUD_ML_REGION", "GOOGLE_CLOUD_LOCATION"}}},
		{"claude", "unknown", nil},

		// Gemini-CLI
		{"gemini-cli", "", [][]string{{"GEMINI_API_KEY", "GOOGLE_API_KEY"}}},
		{"gemini-cli", "api-key", [][]string{{"GEMINI_API_KEY", "GOOGLE_API_KEY"}}},
		{"gemini-cli", "auth-file", nil},
		{"gemini-cli", "vertex-ai", [][]string{{"GOOGLE_CLOUD_PROJECT"}, {"GOOGLE_CLOUD_REGION", "CLOUD_ML_REGION", "GOOGLE_CLOUD_LOCATION"}}},
		{"gemini-cli", "unknown", nil},

		// Codex
		{"codex", "", [][]string{{"CODEX_API_KEY", "OPENAI_API_KEY"}}},
		{"codex", "api-key", [][]string{{"CODEX_API_KEY", "OPENAI_API_KEY"}}},
		{"codex", "auth-file", nil},
		{"codex", "unknown", nil},
	}
	for _, tc := range cases {
		t.Run(tc.harness+"/"+tc.authType, func(t *testing.T) {
			authMeta := loadAuthMetaFromHarness(t, tc.harness)
			got := RequiredAuthEnvKeysFromConfig(authMeta, tc.authType)
			if !equalGroups(got, tc.want) {
				t.Errorf("got=%v want=%v", got, tc.want)
			}
		})
	}
}

// TestRequiredAuthSecretsFromConfig_BuiltInHarnesses verifies correct results
// for all built-in harnesses.
func TestRequiredAuthSecretsFromConfig_BuiltInHarnesses(t *testing.T) {
	cases := []struct {
		harness       string
		authType      string
		gcpSAAssigned bool
		wantNil       bool
		wantKey       string
	}{
		// Claude
		{"claude", "", false, true, ""},
		{"claude", "api-key", false, true, ""},
		{"claude", "auth-file", false, true, ""},
		{"claude", "vertex-ai", false, false, "gcloud-adc"},
		{"claude", "vertex-ai", true, true, ""},

		// Gemini-CLI
		{"gemini-cli", "", false, true, ""},
		{"gemini-cli", "api-key", false, true, ""},
		{"gemini-cli", "auth-file", false, true, ""},
		{"gemini-cli", "vertex-ai", false, false, "gcloud-adc"},
		{"gemini-cli", "vertex-ai", true, true, ""},

		// Codex (no vertex-ai support)
		{"codex", "", false, true, ""},
		{"codex", "api-key", false, true, ""},
		{"codex", "auth-file", false, true, ""},
	}
	for _, tc := range cases {
		name := tc.harness + "/" + tc.authType
		if tc.gcpSAAssigned {
			name += "/sa-assigned"
		}
		t.Run(name, func(t *testing.T) {
			authMeta := loadAuthMetaFromHarness(t, tc.harness)
			got := RequiredAuthSecretsFromConfig(authMeta, tc.authType, tc.gcpSAAssigned)
			if tc.wantNil {
				if got != nil {
					t.Errorf("got %+v, want nil", got)
				}
				return
			}
			if len(got) != 1 || got[0].Key != tc.wantKey {
				t.Errorf("got %+v, want key=%q", got, tc.wantKey)
			}
		})
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

// TestDetectAuthTypeFromEnvVarsFromConfig_BuiltInHarnesses verifies env-var
// auto-detection for all built-in harnesses.
func TestDetectAuthTypeFromEnvVarsFromConfig_BuiltInHarnesses(t *testing.T) {
	cases := []struct {
		harness string
		keys    []string
		want    string
	}{
		// Claude
		{"claude", nil, ""},
		{"claude", []string{"ANTHROPIC_API_KEY"}, ""},
		{"claude", []string{"CLAUDE_CODE_OAUTH_TOKEN"}, "oauth-token"},
		{"claude", []string{"GOOGLE_APPLICATION_CREDENTIALS"}, "vertex-ai"},
		{"claude", []string{"GOOGLE_CLOUD_PROJECT"}, "vertex-ai"},
		{"claude", []string{"CLAUDE_CODE_OAUTH_TOKEN", "GOOGLE_APPLICATION_CREDENTIALS"}, "oauth-token"},
		{"claude", []string{"CLAUDE_CODE_OAUTH_TOKEN", "GOOGLE_CLOUD_PROJECT"}, "oauth-token"},
		{"claude", []string{"ANTHROPIC_API_KEY", "GOOGLE_APPLICATION_CREDENTIALS"}, ""},
		{"claude", []string{"ANTHROPIC_API_KEY", "GOOGLE_CLOUD_PROJECT"}, ""},
		{"claude", []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"}, ""},
		{"claude", []string{"SOME_OTHER_VAR"}, ""},

		// Gemini-CLI
		{"gemini-cli", nil, ""},
		{"gemini-cli", []string{"GEMINI_API_KEY"}, ""},
		{"gemini-cli", []string{"GOOGLE_API_KEY"}, ""},
		{"gemini-cli", []string{"GOOGLE_APPLICATION_CREDENTIALS"}, "vertex-ai"},
		{"gemini-cli", []string{"GOOGLE_CLOUD_PROJECT"}, "vertex-ai"},
		{"gemini-cli", []string{"GEMINI_API_KEY", "GOOGLE_APPLICATION_CREDENTIALS"}, ""},
		{"gemini-cli", []string{"GEMINI_API_KEY", "GOOGLE_CLOUD_PROJECT"}, ""},
		{"gemini-cli", []string{"GOOGLE_API_KEY", "GOOGLE_APPLICATION_CREDENTIALS"}, ""},
		{"gemini-cli", []string{"GOOGLE_API_KEY", "GOOGLE_CLOUD_PROJECT"}, ""},
		{"gemini-cli", []string{"CLAUDE_CODE_OAUTH_TOKEN"}, ""},

		// Codex
		{"codex", nil, ""},
		{"codex", []string{"CODEX_API_KEY"}, ""},
		{"codex", []string{"OPENAI_API_KEY"}, ""},
	}
	for _, tc := range cases {
		name := tc.harness + "/"
		if len(tc.keys) == 0 {
			name += "empty"
		} else {
			for i, k := range tc.keys {
				if i > 0 {
					name += "+"
				}
				name += k
			}
		}
		t.Run(name, func(t *testing.T) {
			authMeta := loadAuthMetaFromHarness(t, tc.harness)
			got := DetectAuthTypeFromEnvVarsFromConfig(authMeta, keySet(tc.keys))
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDetectAuthTypeFromFileSecretsFromConfig_BuiltInHarnesses verifies file-secret
// auto-detection for all built-in harnesses.
func TestDetectAuthTypeFromFileSecretsFromConfig_BuiltInHarnesses(t *testing.T) {
	cases := []struct {
		harness string
		files   []string
		want    string
	}{
		// Claude
		{"claude", nil, ""},
		{"claude", []string{"CLAUDE_AUTH"}, "auth-file"},
		{"claude", []string{"gcloud-adc"}, "vertex-ai"},
		{"claude", []string{"CLAUDE_AUTH", "gcloud-adc"}, "auth-file"},

		// Gemini-CLI
		{"gemini-cli", nil, ""},
		{"gemini-cli", []string{"GEMINI_OAUTH_CREDS"}, "auth-file"},
		{"gemini-cli", []string{"gcloud-adc"}, "vertex-ai"},
		{"gemini-cli", []string{"GEMINI_OAUTH_CREDS", "gcloud-adc"}, "auth-file"},

		// Codex
		{"codex", nil, ""},
		{"codex", []string{"CODEX_AUTH"}, "auth-file"},
	}
	for _, tc := range cases {
		name := tc.harness + "/"
		if len(tc.files) == 0 {
			name += "empty"
		} else {
			for i, f := range tc.files {
				if i > 0 {
					name += "+"
				}
				name += f
			}
		}
		t.Run(name, func(t *testing.T) {
			authMeta := loadAuthMetaFromHarness(t, tc.harness)
			got := DetectAuthTypeFromFileSecretsFromConfig(authMeta, keySet(tc.files))
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDetectAuthTypeFromGCPIdentityFromConfig_BuiltInHarnesses verifies GCP
// identity detection for all built-in harnesses.
func TestDetectAuthTypeFromGCPIdentityFromConfig_BuiltInHarnesses(t *testing.T) {
	cases := []struct {
		harness  string
		assigned bool
		want     string
	}{
		{"claude", true, "vertex-ai"},
		{"claude", false, ""},
		{"gemini-cli", true, "vertex-ai"},
		{"gemini-cli", false, ""},
		{"codex", true, ""},
		{"codex", false, ""},
	}
	for _, tc := range cases {
		name := tc.harness
		if tc.assigned {
			name += "/sa-assigned"
		}
		t.Run(name, func(t *testing.T) {
			authMeta := loadAuthMetaFromHarness(t, tc.harness)
			got := DetectAuthTypeFromGCPIdentityFromConfig(authMeta, tc.assigned)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestGatherConfigEnvVars_BuiltInHarnesses verifies that config-driven env
// var gathering produces expected keys for built-in harnesses.
func TestGatherConfigEnvVars_BuiltInHarnesses(t *testing.T) {
	cases := []struct {
		harness string
		envVars map[string]string
		// wantKeys are env var keys that should appear in AuthConfig.EnvVars
		wantKeys map[string]string
	}{
		{
			"claude",
			map[string]string{
				"ANTHROPIC_API_KEY":       "test-key",
				"CLAUDE_CODE_OAUTH_TOKEN": "test-token",
				"GOOGLE_CLOUD_PROJECT":    "test-project",
				"GOOGLE_CLOUD_REGION":     "us-central1",
			},
			map[string]string{
				"ANTHROPIC_API_KEY":       "test-key",
				"CLAUDE_CODE_OAUTH_TOKEN": "test-token",
				"GOOGLE_CLOUD_PROJECT":    "test-project",
				"GOOGLE_CLOUD_REGION":     "us-central1",
			},
		},
		{
			"codex",
			map[string]string{
				"CODEX_API_KEY":  "test-codex-key",
				"OPENAI_API_KEY": "test-openai-key",
			},
			map[string]string{
				"CODEX_API_KEY":  "test-codex-key",
				"OPENAI_API_KEY": "test-openai-key",
			},
		},
		{
			"gemini-cli",
			map[string]string{
				"GEMINI_API_KEY":       "test-gemini-key",
				"GOOGLE_API_KEY":       "test-google-key",
				"GOOGLE_CLOUD_PROJECT": "test-project",
				"GOOGLE_CLOUD_REGION":  "us-central1",
			},
			map[string]string{
				"GEMINI_API_KEY":       "test-gemini-key",
				"GOOGLE_API_KEY":       "test-google-key",
				"GOOGLE_CLOUD_PROJECT": "test-project",
				"GOOGLE_CLOUD_REGION":  "us-central1",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.harness, func(t *testing.T) {
			authMeta := loadAuthMetaFromHarness(t, tc.harness)
			auth := GatherAuthWithEnv(tc.envVars, false, authMeta)

			for k, v := range tc.wantKeys {
				if got, ok := auth.EnvVars[k]; !ok {
					t.Errorf("EnvVars missing key %q (want value %q)", k, v)
				} else if got != v {
					t.Errorf("EnvVars[%q] = %q, want %q", k, got, v)
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

// keySet builds a set from a slice of keys.
func keySet(keys []string) map[string]struct{} {
	out := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		out[k] = struct{}{}
	}
	return out
}
