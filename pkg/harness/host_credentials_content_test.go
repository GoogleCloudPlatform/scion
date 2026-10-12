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
)

// copilotConfigFixturesFile is the shared fixture set for what counts as
// sign-in in copilot's config.json. The copilot provisioner's tests
// (harnesses/copilot/provision_test.py) run the in-container check over the
// same file, so the host and the container stay on one rule. All values are
// synthetic placeholders.
const copilotConfigFixturesFile = "testdata/copilot_config_signin.json"

// minCopilotConfigFixtures is a floor on the fixture count, so a truncated or
// emptied fixture file cannot pass silently.
const minCopilotConfigFixtures = 32

type copilotConfigFixture struct {
	Name   string `json:"name"`
	Data   string `json:"data"`
	SignIn bool   `json:"signIn"`
}

func loadCopilotConfigFixtures(t *testing.T) []copilotConfigFixture {
	t.Helper()
	raw, err := os.ReadFile(copilotConfigFixturesFile)
	if err != nil {
		t.Fatalf("read %s: %v", copilotConfigFixturesFile, err)
	}
	var fixtures []copilotConfigFixture
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatalf("parse %s: %v", copilotConfigFixturesFile, err)
	}
	if len(fixtures) < minCopilotConfigFixtures {
		t.Fatalf("%s has %d cases, want at least %d", copilotConfigFixturesFile, len(fixtures), minCopilotConfigFixtures)
	}
	return fixtures
}

func TestCopilotConfigHoldsSignIn(t *testing.T) {
	for _, tc := range loadCopilotConfigFixtures(t) {
		t.Run(tc.Name, func(t *testing.T) {
			if got := copilotConfigHoldsSignIn([]byte(tc.Data)); got != tc.SignIn {
				t.Fatalf("copilotConfigHoldsSignIn(%q) = %v, want %v", tc.Data, got, tc.SignIn)
			}
		})
	}
}

// TestHostCredentialHoldsSignIn_OnlyCopilotConfigChecked: the content check
// applies only to copilot's shipped config.json entry. Other files, even
// with the same content, are accepted as before.
func TestHostCredentialHoldsSignIn_OnlyCopilotConfigChecked(t *testing.T) {
	settingsOnly := []byte(`{"trusted_folders": ["/src"]}`)
	copilot := HostCredentialFile{Name: "COPILOT_CONFIG", Field: "CopilotConfigFile", TargetSuffix: "/.copilot/config.json"}
	if hostCredentialHoldsSignIn(copilot, settingsOnly) {
		t.Fatal("settings-only copilot config.json accepted as sign-in")
	}
	for _, f := range []HostCredentialFile{
		{Name: "CLAUDE_AUTH", Field: "ClaudeAuthFile", TargetSuffix: "/.claude/.credentials.json"},
		{Name: "OTHER", Field: "CopilotConfigFile", TargetSuffix: "/.copilot/config.json"},
	} {
		if !hostCredentialHoldsSignIn(f, settingsOnly) {
			t.Fatalf("%+v rejected; only the shipped copilot entry has a content check", f)
		}
	}
}

// TestReadInjectableHostCredentialFile_CopilotConfig covers the co-located
// read path with the shipped copilot harness metadata.
func TestReadInjectableHostCredentialFile_CopilotConfig(t *testing.T) {
	meta := loadAuthMetaFromHarness(t, "copilot")
	for _, tc := range []struct {
		name    string
		data    string
		wantErr bool
	}{
		{"keychain user settings-only file is rejected", `{"trusted_folders": ["/src"], "logged_in_users": [{"host": "https://github.com", "login": "octo"}]}`, true},
		{"file with tokens is read", `{"copilot_tokens": {"https://github.com:octo": "placeholder-token"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			p := filepath.Join(home, ".copilot", "config.json")
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(tc.data), 0o600); err != nil {
				t.Fatal(err)
			}
			files := InjectableHostCredentialFiles(meta, home)
			if len(files) != 1 || files[0].Name != "COPILOT_CONFIG" {
				t.Fatalf("InjectableHostCredentialFiles = %+v, want COPILOT_CONFIG", files)
			}
			data, err := ReadInjectableHostCredentialFile(files[0], home)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "no sign-in data") {
					t.Fatalf("read = %q, %v; want a no-sign-in error", data, err)
				}
				return
			}
			if err != nil || string(data) != tc.data {
				t.Fatalf("read = %q, %v", data, err)
			}
		})
	}
}
