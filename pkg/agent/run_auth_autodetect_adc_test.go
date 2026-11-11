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
	"io/fs"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"gopkg.in/yaml.v3"

	harnessesEmbed "github.com/GoogleCloudPlatform/scion/harnesses"
)

// embeddedHarnessAuthMeta loads the auth block of a bundled harness
// config.yaml, so these pins track the shipped autodetect tables.
func embeddedHarnessAuthMeta(t *testing.T, name string) *config.HarnessAuthMetadata {
	t.Helper()
	data, err := fs.ReadFile(harnessesEmbed.FS, name+"/config.yaml")
	if err != nil {
		t.Fatalf("read %s/config.yaml: %v", name, err)
	}
	var entry config.HarnessConfigEntry
	if err := yaml.Unmarshal(data, &entry); err != nil {
		t.Fatalf("parse %s/config.yaml: %v", name, err)
	}
	if entry.Auth == nil {
		t.Fatalf("%s/config.yaml has no auth block", name)
	}
	return entry.Auth
}

// TestAutoDetectAuthSelectedType_APIKeyBeatsVertexFileCredential pins
// ptone/scion#4559 on the agent-runtime path: with both an ANTHROPIC_API_KEY
// and a staged gcloud-adc file credential, claude selects api-key (matching
// its provisioner), whether the key arrives as env or as an env-type secret.
// The runtimebroker preflight twin is
// TestEnvGather_AutoDetect_APIKeyBeatsVertexFileCredential.
func TestAutoDetectAuthSelectedType_APIKeyBeatsVertexFileCredential(t *testing.T) {
	adc := api.ResolvedSecret{Name: "gcloud-adc", Type: "file", Target: "/home/scion/.config/gcloud/application_default_credentials.json"}
	cases := []struct {
		name string
		opts *api.StartOptions
		want string
	}{
		{
			name: "key in env",
			opts: &api.StartOptions{
				BrokerMode:      true,
				Env:             map[string]string{"ANTHROPIC_API_KEY": "sk-test"},
				ResolvedSecrets: []api.ResolvedSecret{adc},
			},
			want: "api-key",
		},
		{
			name: "key as env secret with identity reachable",
			opts: &api.StartOptions{
				BrokerMode: true,
				Env:        map[string]string{"SCION_METADATA_MODE": "passthrough"},
				ResolvedSecrets: []api.ResolvedSecret{
					adc,
					{Name: "ANTHROPIC_API_KEY", Type: "environment"},
				},
			},
			want: "api-key",
		},
		{
			name: "file credential only",
			opts: &api.StartOptions{
				BrokerMode:      true,
				Env:             map[string]string{},
				ResolvedSecrets: []api.ResolvedSecret{adc},
			},
			want: "vertex-ai",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			auth := &api.AuthConfig{}
			autoDetectAuthSelectedType(auth, embeddedHarnessAuthMeta(t, "claude"), tc.opts)
			if auth.SelectedType != tc.want {
				t.Errorf("SelectedType = %q, want %q", auth.SelectedType, tc.want)
			}
		})
	}
}

// TestAutoDetectAuthSelectedType_AntigravityKeepsVertexFileCredential is the
// counterpart: antigravity (default_type oauth-token, provisioner ranks
// vertex-ai first) keeps the gcloud-adc file credential over GEMINI_API_KEY.
func TestAutoDetectAuthSelectedType_AntigravityKeepsVertexFileCredential(t *testing.T) {
	auth := &api.AuthConfig{}
	opts := &api.StartOptions{
		BrokerMode: true,
		Env:        map[string]string{"GEMINI_API_KEY": "some-key"},
		ResolvedSecrets: []api.ResolvedSecret{
			{Name: "gcloud-adc", Type: "file", Target: "/home/scion/.config/gcloud/application_default_credentials.json"},
		},
	}
	autoDetectAuthSelectedType(auth, embeddedHarnessAuthMeta(t, "antigravity"), opts)
	if auth.SelectedType != "vertex-ai" {
		t.Errorf("SelectedType = %q, want %q", auth.SelectedType, "vertex-ai")
	}
}

// TestAutoDetectAuthSelectedType_OtherFileEnvPairsUnchanged pins, on the
// agent-runtime path, that ptone/scion#4559 leaves every other claude
// file-vs-env pair file-first.
func TestAutoDetectAuthSelectedType_OtherFileEnvPairsUnchanged(t *testing.T) {
	cases := []struct {
		name   string
		secret api.ResolvedSecret
		env    string
		want   string
	}{
		{"auth-file beats API key", api.ResolvedSecret{Name: "CLAUDE_AUTH", Type: "file", Target: "/home/scion/.claude/.credentials.json"}, "ANTHROPIC_API_KEY", "auth-file"},
		{"auth-file beats oauth token", api.ResolvedSecret{Name: "CLAUDE_AUTH", Type: "file", Target: "/home/scion/.claude/.credentials.json"}, "CLAUDE_CODE_OAUTH_TOKEN", "auth-file"},
		{"gcloud-adc beats oauth token", api.ResolvedSecret{Name: "gcloud-adc", Type: "file", Target: "/home/scion/.config/gcloud/application_default_credentials.json"}, "CLAUDE_CODE_OAUTH_TOKEN", "vertex-ai"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			auth := &api.AuthConfig{}
			opts := &api.StartOptions{
				BrokerMode:      true,
				Env:             map[string]string{tc.env: "some-value"},
				ResolvedSecrets: []api.ResolvedSecret{tc.secret},
			}
			autoDetectAuthSelectedType(auth, embeddedHarnessAuthMeta(t, "claude"), opts)
			if auth.SelectedType != tc.want {
				t.Errorf("SelectedType = %q, want %q", auth.SelectedType, tc.want)
			}
		})
	}
}
