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
	"reflect"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"gopkg.in/yaml.v3"
)

// loadAuthMetaFromHarness reads auth metadata from a built-in harness's
// embedded config.yaml using its api.Harness embed accessor. Tests use this
// so the config-driven preflight is validated against the same files that
// ship in the binary.
func loadAuthMetaFromHarness(t *testing.T, harnessName string) *config.HarnessAuthMetadata {
	t.Helper()

	h := newBuiltin(harnessName)
	if h == nil {
		t.Fatalf("no built-in harness for %q", harnessName)
	}
	embedsFS, basePath := h.GetHarnessEmbedsFS()
	if basePath == "" {
		t.Fatalf("harness %q has no embedded config", harnessName)
	}
	data, err := embedsFS.ReadFile(basePath + "/config.yaml")
	if err != nil {
		t.Fatalf("read embedded config.yaml: %v", err)
	}
	var entry config.HarnessConfigEntry
	if err := yaml.Unmarshal(data, &entry); err != nil {
		t.Fatalf("parse embedded config.yaml: %v", err)
	}
	if entry.Auth == nil {
		t.Fatalf("harness %q embedded config has no auth metadata", harnessName)
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
// for every built-in harness's embedded auth metadata. This is the key
// guardrail that lets Phase 3 ship without behavior changes for built-ins.
func TestRequiredAuthEnvKeysFromConfig_ParityWithCompiled(t *testing.T) {
	cases := []struct {
		harness string
		types   []string
	}{
		{"claude", []string{"", "api-key", "oauth-token", "auth-file", "vertex-ai", "unknown"}},
		{"gemini", []string{"", "api-key", "auth-file", "vertex-ai", "unknown"}},
		{"opencode", []string{"", "api-key", "auth-file", "unknown"}},
		{"codex", []string{"", "api-key", "auth-file", "unknown"}},
	}
	for _, tc := range cases {
		authMeta := loadAuthMetaFromHarness(t, tc.harness)
		for _, at := range tc.types {
			t.Run(tc.harness+"/"+at, func(t *testing.T) {
				wantGroups := RequiredAuthEnvKeys(tc.harness, at)
				gotGroups := RequiredAuthEnvKeysFromConfig(authMeta, at)
				if !equalGroups(gotGroups, wantGroups) {
					t.Errorf("config-driven=%v compiled=%v", gotGroups, wantGroups)
				}
			})
		}
	}
}

// TestRequiredAuthSecretsFromConfig_ParityWithCompiled verifies parity for
// the harnesses that actually declare a vertex-ai type. Codex and OpenCode
// intentionally diverge: the compiled table groups them with claude/gemini
// for vertex-ai (incorrectly suggesting a gcloud-adc secret), but neither
// harness declares a vertex-ai auth type so the config path returns nil —
// this is a deliberate behavior tightening documented in the design doc.
func TestRequiredAuthSecretsFromConfig_ParityWithCompiled(t *testing.T) {
	cases := []struct {
		harness string
		types   []string
	}{
		{"claude", []string{"", "api-key", "auth-file", "vertex-ai"}},
		{"gemini", []string{"", "api-key", "auth-file", "vertex-ai"}},
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
					want := RequiredAuthSecrets(tc.harness, at, sa)
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
	authMeta := loadAuthMetaFromHarness(t, "gemini")
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
		{"gemini", "OAUTH wins", []string{"GEMINI_OAUTH_CREDS", "gcloud-adc"}, "auth-file"},
		{"gemini", "gcloud-adc only", []string{"gcloud-adc"}, "vertex-ai"},
		{"codex", "CODEX_AUTH", []string{"CODEX_AUTH"}, "auth-file"},
		{"opencode", "OPENCODE_AUTH", []string{"OPENCODE_AUTH"}, "auth-file"},
		{"opencode", "no files", nil, ""},
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
		{"gemini", true, "vertex-ai"},
		{"gemini", false, ""},
		{"codex", true, ""},     // no vertex-ai type declared
		{"opencode", true, ""},  // no vertex-ai type declared
		{"opencode", false, ""}, // no vertex-ai type declared
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
