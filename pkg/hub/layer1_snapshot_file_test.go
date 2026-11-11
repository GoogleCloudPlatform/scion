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

package hub

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// TestBuildLayer1SnapshotFromFile_ServerlessSettings guards
// ptone/scion#2284: the file-mode reload path (LoadGlobalConfig ->
// BuildLayer1SnapshotFromFile) must honour top-level sections of a
// settings.yaml that has no "server" key, and agree with the same file
// carrying a "server" key. Otherwise a file-mode admin PUT resets the live
// quota, agent-secret and timezone values until the next restart.
func TestBuildLayer1SnapshotFromFile_ServerlessSettings(t *testing.T) {
	const topLevel = `quotas:
  enforce_broker_quotas: false
agent_secrets:
  user_scope_only: true
default_timezone: Asia/Tokyo
`
	snapFor := func(t *testing.T, content string) Layer1Snapshot {
		t.Helper()
		home := t.TempDir()
		t.Setenv("HOME", home)
		dir := filepath.Join(home, ".scion")
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		gc, err := config.LoadGlobalConfig(t.TempDir())
		if err != nil {
			t.Fatalf("LoadGlobalConfig: %v", err)
		}
		return BuildLayer1SnapshotFromFile(gc)
	}

	with := snapFor(t, "schema_version: \"1\"\nserver:\n  hub:\n    port: 9810\n"+topLevel)
	without := snapFor(t, "schema_version: \"1\"\n"+topLevel)

	for name, snap := range map[string]Layer1Snapshot{"with server": with, "without server": without} {
		if snap.EnforceBrokerQuotas == nil || *snap.EnforceBrokerQuotas {
			t.Errorf("%s: EnforceBrokerQuotas = %s, want false", name, boolPtrText(snap.EnforceBrokerQuotas))
		}
		if snap.AgentSecretsUserScopeOnly == nil || !*snap.AgentSecretsUserScopeOnly {
			t.Errorf("%s: AgentSecretsUserScopeOnly = %s, want true", name, boolPtrText(snap.AgentSecretsUserScopeOnly))
		}
		if snap.DefaultTimezone != "Asia/Tokyo" {
			t.Errorf("%s: DefaultTimezone = %q, want Asia/Tokyo", name, snap.DefaultTimezone)
		}
	}
}

// boolPtrText renders a *bool for test failure messages.
func boolPtrText(b *bool) string {
	if b == nil {
		return "<nil>"
	}
	return strconv.FormatBool(*b)
}
