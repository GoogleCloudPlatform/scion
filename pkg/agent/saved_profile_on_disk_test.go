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
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// savedProfileFixture returns a project .scion directory and the path of
// agent name's agent-info.json in it (where provision writes it).
func savedProfileFixture(t *testing.T, name string) (projectScionDir, agentInfo string) {
	t.Helper()
	projectScionDir = filepath.Join(t.TempDir(), config.DotScion)
	if err := os.MkdirAll(projectScionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	projectDir, err := config.GetResolvedProjectDir(projectScionDir)
	if err != nil {
		t.Fatal(err)
	}
	home := config.GetAgentHomePath(projectDir, name)
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	return projectScionDir, filepath.Join(home, "agent-info.json")
}

func TestSavedProfileOnDisk(t *testing.T) {
	t.Run("found matches the agent-info.json profile", func(t *testing.T) {
		dir, info := savedProfileFixture(t, "a1")
		if err := os.WriteFile(info, []byte(`{"profile":"local"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		got, state := SavedProfileOnDisk(dir, "a1", false, "")
		if got != "local" || state != SavedProfileFound {
			t.Fatalf("SavedProfileOnDisk = (%q, %v), want (local, Found)", got, state)
		}
		if want := savedInfoProfile("a1", dir); got != want {
			t.Fatalf("SavedProfileOnDisk %q differs from the agent-info.json profile %q", got, want)
		}
	})
	t.Run("found with an empty profile", func(t *testing.T) {
		dir, info := savedProfileFixture(t, "a2")
		if err := os.WriteFile(info, []byte(`{"name":"a2"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if got, state := SavedProfileOnDisk(dir, "a2", false, ""); got != "" || state != SavedProfileFound {
			t.Fatalf("SavedProfileOnDisk = (%q, %v), want (\"\", Found)", got, state)
		}
	})
	t.Run("missing", func(t *testing.T) {
		dir, _ := savedProfileFixture(t, "a3")
		if got, state := SavedProfileOnDisk(dir, "a3", false, ""); got != "" || state != SavedProfileMissing {
			t.Fatalf("SavedProfileOnDisk = (%q, %v), want (\"\", Missing)", got, state)
		}
	})
	t.Run("unreadable without provenance", func(t *testing.T) {
		dir, info := savedProfileFixture(t, "a4")
		if err := os.WriteFile(info, []byte(`{not json`), 0o644); err != nil {
			t.Fatal(err)
		}
		if got, state := SavedProfileOnDisk(dir, "a4", false, ""); got != "" || state != SavedProfileUnreadable {
			t.Fatalf("SavedProfileOnDisk = (%q, %v), want (\"\", Unreadable)", got, state)
		}
	})
	t.Run("unreadable uses the provisioned profile", func(t *testing.T) {
		dir, info := savedProfileFixture(t, "a5")
		if err := os.WriteFile(info, []byte(`{not json`), 0o644); err != nil {
			t.Fatal(err)
		}
		projectDir, err := config.GetResolvedProjectDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		agentDir, _, err := AgentStateDir(projectDir, "a5", false, "", true)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(agentDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := writeImageProvenance(agentDir, imageProvenance{Version: imageProvenanceVersion, Profile: "local"}); err != nil {
			t.Fatal(err)
		}
		if got, state := SavedProfileOnDisk(dir, "a5", false, ""); got != "local" || state != SavedProfileUnreadable {
			t.Fatalf("SavedProfileOnDisk = (%q, %v), want (local, Unreadable)", got, state)
		}
	})
}
