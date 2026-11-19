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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadAndReconcileConfig_RejectsServerDecodeErrorWithoutFallback verifies
// that server startup fails with an error naming the mistyped settings.yaml
// server field rather than silently falling back to legacy server.yaml or
// workstation defaults (ptone/scion#6187).
func TestLoadAndReconcileConfig_RejectsServerDecodeErrorWithoutFallback(t *testing.T) {
	t.Cleanup(resetServerFlags)
	resetServerFlags()

	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(`schema_version: "1"
server:
  mode: hosted
  hub:
    admin_emails: "admin@example.com"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "server.yaml"), []byte(`hub:
  port: 7777
`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := loadAndReconcileConfig(serverStartCmd)
	if err == nil {
		t.Fatal("loadAndReconcileConfig succeeded; expected failure on mistyped server.hub.admin_emails")
	}
	if !strings.Contains(err.Error(), "server.hub.admin_emails") {
		t.Fatalf("loadAndReconcileConfig err = %v, want error naming server.hub.admin_emails", err)
	}
	if strings.Contains(err.Error(), "admin@example.com") {
		t.Fatalf("loadAndReconcileConfig err = %v, must not echo raw scalar value", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(`schema_version: "1"
server:
  mode: hosted
  database:
    driver: postgres
    url: !!int "postgres://scion:super-secret-db-password@db.example.internal/scion"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = loadAndReconcileConfig(serverStartCmd)
	if err == nil {
		t.Fatal("loadAndReconcileConfig succeeded; expected failure on tagged server.database.url")
	}
	if !strings.Contains(err.Error(), "server.database.url") {
		t.Fatalf("loadAndReconcileConfig err = %v, want error naming server.database.url", err)
	}
	if strings.Contains(err.Error(), "super-secret-db-password") {
		t.Fatalf("loadAndReconcileConfig err = %v, must not echo raw secret value", err)
	}
}
