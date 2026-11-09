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

package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// cloudRunAllowListSettingsYAML sets cloudrun.assignable_service_accounts
// on the "cr" runtime entry.
const cloudRunAllowListSettingsYAML = `schema_version: "1"
runtimes:
    cr:
        type: cloudrun
        cloudrun:
            assignable_service_accounts:
                - assigned-sa@example.com
`

func writeCloudRunTestSettings(t *testing.T, dir, yaml string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
}

// The list is read into CloudRunConfig.AssignableServiceAccounts and
// passes the settings schema.
func TestCloudRunAssignableServiceAccounts_Loads(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCloudRunTestSettings(t, filepath.Join(home, ".scion"), cloudRunAllowListSettingsYAML)

	vs, _, err := LoadGlobalSettingsWithOverlay()
	if err != nil {
		t.Fatalf("LoadGlobalSettingsWithOverlay: %v", err)
	}
	if vs == nil || vs.Runtimes["cr"].CloudRun == nil {
		t.Fatalf("runtime entry cr has no cloudrun block: %+v", vs)
	}
	if got := vs.Runtimes["cr"].CloudRun.AssignableServiceAccounts; !slices.Equal(got, []string{"assigned-sa@example.com"}) {
		t.Errorf("AssignableServiceAccounts = %v", got)
	}
}

func TestProjectSettingsHasCloudRunAssignableServiceAccounts(t *testing.T) {
	setup := func(t *testing.T, projectYAML string) string {
		t.Helper()
		home := t.TempDir()
		t.Setenv("HOME", home)
		// The global settings set the list too: it must not be mistaken
		// for one the project set itself.
		writeCloudRunTestSettings(t, filepath.Join(home, ".scion"), cloudRunAllowListSettingsYAML)
		projectDir := t.TempDir()
		writeCloudRunTestSettings(t, filepath.Join(projectDir, ".scion"), projectYAML)
		return projectDir
	}

	t.Run("project runtime entry sets the list", func(t *testing.T) {
		projectDir := setup(t, cloudRunAllowListSettingsYAML)
		if !ProjectSettingsHasCloudRunAssignableServiceAccounts(projectDir, "cr") {
			t.Error("expected true: the project's own settings.yaml sets the list")
		}
	})
	t.Run("list inherited from global only", func(t *testing.T) {
		projectDir := setup(t, `schema_version: "1"
runtimes:
    cr:
        type: cloudrun
`)
		if ProjectSettingsHasCloudRunAssignableServiceAccounts(projectDir, "cr") {
			t.Error("expected false: only the global settings set the list")
		}
	})
	t.Run("another runtime entry", func(t *testing.T) {
		projectDir := setup(t, cloudRunAllowListSettingsYAML)
		if ProjectSettingsHasCloudRunAssignableServiceAccounts(projectDir, "other") {
			t.Error("expected false: the list is on another entry")
		}
	})
	t.Run("no runtime entry", func(t *testing.T) {
		projectDir := setup(t, cloudRunAllowListSettingsYAML)
		if ProjectSettingsHasCloudRunAssignableServiceAccounts(projectDir, "") {
			t.Error("expected false for an empty runtime entry name")
		}
	})
	t.Run("missing project settings", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		if ProjectSettingsHasCloudRunAssignableServiceAccounts(t.TempDir(), "cr") {
			t.Error("expected false: no project settings file")
		}
	})
}

// Entries that are not service account emails are reported by position
// only, never by value; well-formed entries in any case or with
// surrounding spaces are not.
func TestCloudRunAssignableServiceAccountWarnings(t *testing.T) {
	vs := &VersionedSettings{Runtimes: map[string]V1RuntimeConfig{
		"cr": {CloudRun: &CloudRunConfig{AssignableServiceAccounts: []string{
			"assigned-sa@example-project.iam.gserviceaccount.com",
			"not-an-account",
			" Listed-SA@Example-Project.IAM.gserviceaccount.com ",
			"person@example.com",
			"@example-project.iam.gserviceaccount.com",
			"a@b@example-project.iam.gserviceaccount.com",
			"a b@example-project.iam.gserviceaccount.com",
			"a@example-project..gserviceaccount.com",
		}}},
		"plain": {},
	}}
	got := CloudRunAssignableServiceAccountWarnings(vs, "cr")
	wantIdx := []string{"[1]", "[3]", "[4]", "[5]", "[6]", "[7]"}
	if len(got) != len(wantIdx) {
		t.Fatalf("warnings = %q, want %d", got, len(wantIdx))
	}
	for i, w := range got {
		if !strings.Contains(w, "runtimes.cr.cloudrun.assignable_service_accounts"+wantIdx[i]) {
			t.Errorf("warning %d = %q, want position %s", i, w, wantIdx[i])
		}
		for _, value := range []string{"not-an-account", "person", "example", "a b@"} {
			if strings.Contains(w, value) {
				t.Errorf("warning %q names the value %q", w, value)
			}
		}
	}
	for _, entry := range []string{"plain", "absent"} {
		if got := CloudRunAssignableServiceAccountWarnings(vs, entry); got != nil {
			t.Errorf("entry %s: warnings = %q, want none", entry, got)
		}
	}
	if got := CloudRunAssignableServiceAccountWarnings(nil, "cr"); got != nil {
		t.Errorf("nil settings: warnings = %q, want none", got)
	}
}

// block_service_account is read into CloudRunConfig.BlockServiceAccount
// and passes the settings schema.
func TestCloudRunBlockServiceAccount_Loads(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCloudRunTestSettings(t, filepath.Join(home, ".scion"), `schema_version: "1"
runtimes:
    cr:
        type: cloudrun
        cloudrun:
            block_service_account: block-sa@example-project.iam.gserviceaccount.com
`)
	vs, _, err := LoadGlobalSettingsWithOverlay()
	if err != nil {
		t.Fatalf("LoadGlobalSettingsWithOverlay: %v", err)
	}
	if vs == nil || vs.Runtimes["cr"].CloudRun == nil {
		t.Fatalf("runtime entry cr has no cloudrun block: %+v", vs)
	}
	if got := vs.Runtimes["cr"].CloudRun.BlockServiceAccount; got != "block-sa@example-project.iam.gserviceaccount.com" {
		t.Errorf("BlockServiceAccount = %q", got)
	}
}

func TestProjectSettingsHasCloudRunBlockServiceAccount(t *testing.T) {
	const withBlock = `schema_version: "1"
runtimes:
    cr:
        type: cloudrun
        cloudrun:
            block_service_account: block-sa@example-project.iam.gserviceaccount.com
`
	setup := func(t *testing.T, projectYAML string) string {
		t.Helper()
		home := t.TempDir()
		t.Setenv("HOME", home)
		// The global settings set it too: it must not be mistaken for one
		// the project set itself.
		writeCloudRunTestSettings(t, filepath.Join(home, ".scion"), withBlock)
		projectDir := t.TempDir()
		writeCloudRunTestSettings(t, filepath.Join(projectDir, ".scion"), projectYAML)
		return projectDir
	}
	if !ProjectSettingsHasCloudRunBlockServiceAccount(setup(t, withBlock), "cr") {
		t.Error("expected true: the project's own settings.yaml sets the block account")
	}
	if ProjectSettingsHasCloudRunBlockServiceAccount(setup(t, cloudRunAllowListSettingsYAML), "cr") {
		t.Error("expected false: the project sets only the allow-list")
	}
	if ProjectSettingsHasCloudRunBlockServiceAccount(setup(t, `schema_version: "1"
runtimes:
    cr:
        type: cloudrun
        cloudrun:
            block_service_account: "  "
`), "cr") {
		t.Error("expected false: a blank value sets nothing")
	}
	if ProjectSettingsHasCloudRunBlockServiceAccount(setup(t, withBlock), "other") {
		t.Error("expected false: another runtime entry")
	}
	if ProjectSettingsHasCloudRunAssignableServiceAccounts(setup(t, withBlock), "cr") {
		t.Error("expected false: the project sets only the block account")
	}
}

// A block account that is not a service account email is reported by
// setting name only; a well-formed one in any case, an empty one or a
// missing entry gives no warning.
func TestCloudRunBlockServiceAccountWarnings(t *testing.T) {
	vs := &VersionedSettings{Runtimes: map[string]V1RuntimeConfig{
		"bad":   {CloudRun: &CloudRunConfig{BlockServiceAccount: "person@example.com"}},
		"good":  {CloudRun: &CloudRunConfig{BlockServiceAccount: " Block-SA@Example-Project.iam.gserviceaccount.com "}},
		"empty": {CloudRun: &CloudRunConfig{}},
		"plain": {},
	}}
	got := CloudRunBlockServiceAccountWarnings(vs, "bad")
	if len(got) != 1 || !strings.Contains(got[0], "runtimes.bad.cloudrun.block_service_account") {
		t.Fatalf("warnings = %q, want one naming the setting", got)
	}
	if strings.Contains(got[0], "person") || strings.Contains(got[0], "example") {
		t.Errorf("warning %q names the value", got[0])
	}
	for _, entry := range []string{"good", "empty", "plain", "absent"} {
		if w := CloudRunBlockServiceAccountWarnings(vs, entry); w != nil {
			t.Errorf("entry %s: warnings = %q, want none", entry, w)
		}
	}
	if w := CloudRunBlockServiceAccountWarnings(nil, "bad"); w != nil {
		t.Errorf("nil settings: warnings = %q, want none", w)
	}
}
