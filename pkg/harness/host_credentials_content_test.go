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
	"strings"
	"testing"
)

// copilotConfigFixtures is the shared fixture set for what counts as sign-in
// in copilot's config.json. All values are synthetic placeholders.
var copilotConfigFixtures = []struct {
	name   string
	data   string
	signIn bool
}{
	{"settings only", `{"trusted_folders": ["/src"], "banner": "never", "theme": "dark", "model": "m"}`, false},
	{"keychain user", `{"trusted_folders": ["/src"], "logged_in_users": [{"host": "https://github.com", "login": "octo"}], "last_logged_in_user": {"host": "https://github.com", "login": "octo"}, "banner": "never"}`, false},
	{"scion-written trustedFolders only", `{"trustedFolders": ["/workspace"]}`, false},
	{"empty object", `{}`, false},
	{"tokens", `{"logged_in_users": [{"host": "https://github.com", "login": "octo"}], "copilot_tokens": {"https://github.com:octo": "placeholder-token"}}`, true},
	{"copilot hmac key", `{"copilot_hmac_key": "placeholder-hmac"}`, true},
	{"capi hmac key", `{"capi_hmac_key": "placeholder-hmac", "theme": "dark"}`, true},
	{"empty tokens map", `{"trusted_folders": ["/src"], "copilot_tokens": {}}`, false},
	{"whitespace-only token value", `{"copilot_tokens": {"https://github.com:octo": "  "}}`, false},
	{"empty token value and empty hmac", `{"copilot_tokens": {"https://github.com:octo": ""}, "copilot_hmac_key": ""}`, false},
	{"unknown key", `{"trusted_folders": ["/src"], "some_future_key": "x"}`, true},
	{"uppercase keys", `{"COPILOT_TOKENS": {"https://github.com:octo": "placeholder-token"}}`, true},
	{"uppercase settings keys", `{"Trusted_Folders": ["/src"], "BANNER": "never"}`, false},
	{"comment lines", "// User settings belong in settings.json.\n// This file is managed automatically.\n{\n  \"copilot_tokens\": {\"https://github.com:octo\": \"placeholder-token\"}\n}\n", true},
	{"comment lines settings only", "// managed automatically\n{\"trusted_folders\": [\"/src\"]}\n", false},
	{"unparseable", `{"copilot_tokens": `, false},
	{"not an object", `["copilot_tokens"]`, false},
	{"null", `null`, false},
	{"empty file", ``, false},
}

func TestCopilotConfigHoldsSignIn(t *testing.T) {
	for _, tc := range copilotConfigFixtures {
		t.Run(tc.name, func(t *testing.T) {
			if got := copilotConfigHoldsSignIn([]byte(tc.data)); got != tc.signIn {
				t.Fatalf("copilotConfigHoldsSignIn(%s) = %v, want %v", tc.data, got, tc.signIn)
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
