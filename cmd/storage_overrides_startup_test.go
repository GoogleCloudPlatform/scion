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

package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

const startupOverrideSettings = `schema_version: "1"
runtimes:
  cluster:
    type: kubernetes
profiles:
  nfs-profile:
    runtime: cluster
    shared_dir_storage_backend: nfs
server:
  shared_dir_storage:
    backend: local
    nfs:
      mount_root: /mnt/share
      subpath_root: tree/projects
      shares:
        - id: share-a
          server: 192.0.2.10
          export: /export/a
          pv_name: share-a-pv
`

func startupOverrideHome(t *testing.T, content string) {
	t.Helper()
	old := config.GetGlobalSettingsOverlay()
	t.Cleanup(func() { config.SetGlobalSettingsOverlay(old) })
	config.SetGlobalSettingsOverlay(nil)
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Process start reports removed storage overrides as an ERROR naming the
// key, where it was found and the migration, and does not fail
// (ptone/scion#6371).
func TestStartupReportsRemovedStorageOverrides(t *testing.T) {
	startupOverrideHome(t, startupOverrideSettings)
	var logged []string
	logf := func(format string, args ...interface{}) { logged = append(logged, fmt.Sprintf(format, args...)) }

	public := hubRemovedStorageOverridesFileError(logf)
	if len(logged) != 1 || !strings.HasPrefix(logged[0], "ERROR: the global settings file sets a removed storage override") {
		t.Fatalf("want one ERROR line, got %q", logged)
	}
	for _, want := range []string{"profiles.nfs-profile.shared_dir_storage_backend", `pv_name "share-a-pv"`, "server.shared_dir_storage.backend"} {
		if !strings.Contains(logged[0], want) {
			t.Errorf("ERROR %q does not name %q", logged[0], want)
		}
	}
	if !strings.Contains(public, "profiles.nfs-profile.shared_dir_storage_backend") || strings.Contains(public, "share-a-pv") {
		t.Errorf("public message %q: want the key and no claim", public)
	}
	if err := brokerStorageOverridesCheck(); err == nil {
		t.Error("the broker health check must report the override")
	}

	// The hub's stored profiles are reported with their own source.
	logged = nil
	err := reportRemovedStorageOverrides(logf, config.HubStoredSettingsSource, nil,
		map[string]config.V1ProfileConfig{"stored": {HomeStorageBackend: "nfs"}}, nil)
	if err == nil || len(logged) != 1 || !strings.Contains(logged[0], "the hub's stored settings") ||
		!strings.Contains(logged[0], "profiles.stored.home_storage_backend") {
		t.Errorf("stored overrides: err=%v logged=%q", err, logged)
	}
}

func TestStartupReportsNothingWithoutRemovedStorageOverrides(t *testing.T) {
	startupOverrideHome(t, "schema_version: \"1\"\nprofiles:\n  p:\n    runtime: cluster\nruntimes:\n  cluster:\n    type: kubernetes\n")
	var logged []string
	logf := func(format string, args ...interface{}) { logged = append(logged, fmt.Sprintf(format, args...)) }
	if public := hubRemovedStorageOverridesFileError(logf); public != "" || len(logged) != 0 {
		t.Errorf("want nothing reported, got %q %q", public, logged)
	}
	if err := brokerStorageOverridesCheck(); err != nil {
		t.Errorf("broker check: %v", err)
	}
}
