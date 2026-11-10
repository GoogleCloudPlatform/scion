//go:build !no_sqlite

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
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// The co-located settings overlay install reports the hub's stored
// runtimes and profiles that carry a removed storage override, as an
// ERROR, and still installs the overlay (ptone/scion#6371).
func TestInstallColocatedSettingsOverlay_ReportsRemovedStorageOverrides(t *testing.T) {
	resetGlobalSettingsOverlay(t)
	var buf bytes.Buffer
	oldOut, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(oldOut); log.SetFlags(oldFlags) })

	cfg := &config.GlobalConfig{}
	cfg.Database.Driver = "sqlite"
	if !installColocatedSettingsOverlay(cfg, newStoredCatalogTestHub(t,
		`{"nfs-p":{"runtime":"stored-docker","shared_dir_storage_backend":"nfs"}}`,
		`{"stored-docker":{"type":"docker"}}`, "managed")) {
		t.Fatal("the overlay must still be installed")
	}
	out := buf.String()
	if !strings.Contains(out, "ERROR: the hub's stored settings sets a removed storage override") ||
		!strings.Contains(out, `profiles.nfs-p.shared_dir_storage_backend ("nfs")`) {
		t.Fatalf("want the ERROR naming the stored key, got:\n%s", out)
	}
}
