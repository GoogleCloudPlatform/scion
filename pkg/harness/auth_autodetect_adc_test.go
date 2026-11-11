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
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"

	harnessesEmbed "github.com/GoogleCloudPlatform/scion/harnesses"
)

type autodetectCase struct {
	harness       string
	fileKeys      []string
	envKeys       []string
	gcpSAAssigned bool
	want          string
}

func runAutodetectCases(t *testing.T, cases []autodetectCase) {
	t.Helper()
	for _, tc := range cases {
		name := tc.harness
		for _, k := range tc.fileKeys {
			name += "/file=" + k
		}
		for _, k := range tc.envKeys {
			name += "/env=" + k
		}
		if tc.gcpSAAssigned {
			name += "/sa-assigned"
		}
		t.Run(name, func(t *testing.T) {
			authMeta := loadAuthMetaFromHarness(t, tc.harness)
			got := AutoDetectAuthType(authMeta, keySet(tc.fileKeys), keySet(tc.envKeys), tc.gcpSAAssigned)
			if got != tc.want {
				t.Errorf("AutoDetectAuthType() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAutoDetectAuthType_APIKeyBeatsVertexFileCredential pins
// ptone/scion#4559: for the api-key default harnesses with a gcloud-adc
// file credential, an API key env credential wins over that file
// credential, matching their provisioners.
func TestAutoDetectAuthType_APIKeyBeatsVertexFileCredential(t *testing.T) {
	runAutodetectCases(t, []autodetectCase{
		// claude: the four presence combinations of API key env and
		// gcloud-adc file.
		{"claude", []string{"gcloud-adc"}, []string{"ANTHROPIC_API_KEY"}, false, "api-key"},
		{"claude", []string{"gcloud-adc"}, []string{"ANTHROPIC_API_KEY"}, true, "api-key"},
		{"claude", []string{"gcloud-adc"}, nil, false, "vertex-ai"},
		{"claude", nil, []string{"ANTHROPIC_API_KEY"}, false, "api-key"},
		{"claude", nil, nil, false, ""},
		{"claude", nil, nil, true, "vertex-ai"},
		// The key still wins when a Vertex env var is also present.
		{"claude", []string{"gcloud-adc"}, []string{"ANTHROPIC_API_KEY", "GOOGLE_CLOUD_PROJECT"}, false, "api-key"},
		{"gemini-cli", []string{"gcloud-adc"}, []string{"GEMINI_API_KEY"}, false, "api-key"},
		{"gemini-cli", []string{"gcloud-adc"}, nil, false, "vertex-ai"},
		{"grok-build", []string{"gcloud-adc"}, []string{"XAI_API_KEY"}, false, "api-key"},
		{"grok-build", []string{"gcloud-adc"}, nil, false, "vertex-ai"},
		{"hermes", []string{"gcloud-adc"}, []string{"OPENAI_API_KEY"}, false, "api-key"},
		{"hermes", []string{"gcloud-adc"}, nil, false, "vertex-ai"},
	})
}

// TestAutoDetectAuthType_OtherFileEnvPairsUnchanged pins that ptone/scion#4559
// changes nothing beyond the API key over gcloud-adc pair: every other file
// credential still wins over an env credential.
func TestAutoDetectAuthType_OtherFileEnvPairsUnchanged(t *testing.T) {
	runAutodetectCases(t, []autodetectCase{
		// claude auth-file and oauth-token pairs.
		{"claude", []string{"CLAUDE_AUTH"}, []string{"ANTHROPIC_API_KEY"}, false, "auth-file"},
		{"claude", []string{"CLAUDE_AUTH"}, []string{"CLAUDE_CODE_OAUTH_TOKEN"}, false, "auth-file"},
		{"claude", []string{"CLAUDE_AUTH"}, []string{"GOOGLE_CLOUD_PROJECT"}, false, "auth-file"},
		{"claude", []string{"gcloud-adc"}, []string{"CLAUDE_CODE_OAUTH_TOKEN"}, false, "vertex-ai"},
		{"claude", []string{"gcloud-adc"}, []string{"GOOGLE_CLOUD_PROJECT"}, false, "vertex-ai"},
		// gemini-cli and grok-build auth files.
		{"gemini-cli", []string{"GEMINI_OAUTH_CREDS"}, []string{"GEMINI_API_KEY"}, false, "auth-file"},
		{"grok-build", []string{"GROK_AUTH"}, []string{"XAI_API_KEY"}, false, "auth-file"},
		// codex, copilot and opencode auth files.
		{"codex", []string{"CODEX_AUTH"}, []string{"OPENAI_API_KEY"}, false, "auth-file"},
		{"copilot", []string{"COPILOT_CONFIG"}, []string{"GH_TOKEN"}, false, "auth-file"},
		{"opencode", []string{"OPENCODE_AUTH"}, []string{"ANTHROPIC_API_KEY"}, false, "auth-file"},
		{"opencode", []string{"OPENCODE_AUTH"}, []string{"GOOGLE_CLOUD_PROJECT"}, false, "auth-file"},
		// antigravity (default_type oauth-token): gcloud-adc keeps winning.
		{"antigravity", []string{"gcloud-adc"}, []string{"GEMINI_API_KEY"}, false, "vertex-ai"},
		{"antigravity", []string{"gcloud-adc"}, []string{"AGY_TOKEN"}, false, "vertex-ai"},
	})
}

// TestAutoDetectAuthType_APIKeyOverVertexFileNeedsAPIKeyDefault covers the
// rule's default_type gate on synthetic metadata.
func TestAutoDetectAuthType_APIKeyOverVertexFileNeedsAPIKeyDefault(t *testing.T) {
	meta := func(defaultType string) *config.HarnessAuthMetadata {
		return &config.HarnessAuthMetadata{
			DefaultType: defaultType,
			Types: map[string]config.HarnessAuthTypeMetadata{
				"api-key": {}, "oauth-token": {}, "vertex-ai": {},
			},
			Autodetect: config.HarnessAuthAutodetect{
				Env:   map[string]string{"KEY": "api-key"},
				Files: map[string]string{"adc": "vertex-ai"},
			},
		}
	}
	for defaultType, want := range map[string]string{"api-key": "api-key", "oauth-token": "vertex-ai", "": "vertex-ai"} {
		got := AutoDetectAuthType(meta(defaultType), keySet([]string{"adc"}), keySet([]string{"KEY"}), false)
		if got != want {
			t.Errorf("default_type %q: AutoDetectAuthType() = %q, want %q", defaultType, got, want)
		}
	}
}

// TestAutoDetectAuthType_APIKeyOverVertexFileFiresOnlyForKnownHarnesses
// loads every embedded harness config and pins which harnesses the
// ptone/scion#4559 exception applies to. For each pair of one autodetect
// file key and one autodetect env key, the exception fires when the file key
// alone resolves to a type and adding the env key changes the result. Firing
// is observed through AutoDetectAuthType, not re-derived from the rule.
//
// A new harness with default_type api-key, a vertex-ai file mapping and an
// api-key env mapping makes this test fail: check that its provision.py
// ranks api-key above vertex-ai before adding it to the expected set.
func TestAutoDetectAuthType_APIKeyOverVertexFileFiresOnlyForKnownHarnesses(t *testing.T) {
	expected := map[string]bool{
		"claude":     true,
		"gemini-cli": true,
		"grok-build": true,
		"hermes":     true,
	}

	entries, err := fs.ReadDir(harnessesEmbed.FS, ".")
	if err != nil {
		t.Fatalf("read harnesses FS root: %v", err)
	}

	seen := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if _, err := fs.Stat(harnessesEmbed.FS, name+"/config.yaml"); err != nil {
			continue
		}
		seen++
		authMeta := loadAuthMetaFromHarness(t, name)

		var firing []string
		for fileKey := range authMeta.Autodetect.Files {
			files := keySet([]string{fileKey})
			fileOnly := AutoDetectAuthType(authMeta, files, nil, false)
			if fileOnly == "" {
				continue
			}
			for envKey := range authMeta.Autodetect.Env {
				got := AutoDetectAuthType(authMeta, files, keySet([]string{envKey}), false)
				if got != fileOnly {
					firing = append(firing, fmt.Sprintf("file %s (%s) + env %s -> %s", fileKey, fileOnly, envKey, got))
				}
			}
		}
		sort.Strings(firing)

		switch {
		case expected[name] && len(firing) == 0:
			t.Errorf("harness %q: expected an API key env credential to beat its gcloud-adc file credential, but no file/env pair changed the file result", name)
		case !expected[name] && len(firing) > 0:
			t.Errorf("harness %q: the API key over gcloud-adc exception now applies (%s); confirm its provision.py ranks api-key above vertex-ai, then add it to the expected set here and to the docs in AutoDetectAuthType and agent-credentials.md",
				name, strings.Join(firing, "; "))
		}
	}

	for name := range expected {
		if _, err := fs.Stat(harnessesEmbed.FS, name+"/config.yaml"); err != nil {
			t.Errorf("expected harness %q has no embedded config.yaml", name)
		}
	}
	if seen == 0 {
		t.Fatal("no embedded harness configs found")
	}
}
