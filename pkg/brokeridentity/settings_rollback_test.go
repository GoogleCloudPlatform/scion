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

package brokeridentity

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

const rollbackInstanceYAML = `schema_version: "1"
server:
  broker:
    enabled: true
    instances:
      - key: local-docker
        name: example-docker
        runtime_target:
          type: docker
`

func loadInstanceIdentity(t *testing.T, global string) *Identity {
	t.Helper()
	inst, err := config.LoadRuntimeBrokerInstances("")
	if err != nil {
		t.Fatal(err)
	}
	if len(inst) != 1 {
		t.Fatalf("expected one instance, got %+v", inst)
	}
	id, err := LoadOrCreate(InstanceDir(global, inst[0].Key), inst[0].Key, inst[0].RuntimeTarget.Type, dockerScope("D1", ""), nil)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestIdentity_ReaddedEntryRestoresSameIDs: settings rollback (an older binary
// rewrites settings.yaml without server.broker.instances), then the entry is
// re-added with the same key. The untouched identity directory restores the
// same IDs.
func TestIdentity_ReaddedEntryRestoresSameIDs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	global := filepath.Join(home, ".scion")
	settings := filepath.Join(global, "settings.yaml")
	if err := os.MkdirAll(global, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(settings, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(rollbackInstanceYAML)
	first := loadInstanceIdentity(t, global)

	// Entry removed by a settings rewrite: no instance is configured.
	write("schema_version: \"1\"\nserver:\n  broker:\n    enabled: true\n")
	if inst, err := config.LoadRuntimeBrokerInstances(""); err != nil || len(inst) != 0 {
		t.Fatalf("entry should be gone: %+v %v", inst, err)
	}

	write(rollbackInstanceYAML)
	again := loadInstanceIdentity(t, global)
	if again.RuntimeBrokerID != first.RuntimeBrokerID || again.RuntimeTarget.ID != first.RuntimeTarget.ID {
		t.Fatal("re-added entry must restore the same identity")
	}
}

func TestInstanceKeyPattern_MatchesConfig(t *testing.T) {
	if InstanceKeyPattern != config.RuntimeBrokerInstanceKeyPattern {
		t.Fatalf("key patterns diverged: %q vs %q", InstanceKeyPattern, config.RuntimeBrokerInstanceKeyPattern)
	}
}
