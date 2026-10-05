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
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// File-mode PUT accepts shared_dir_storage_backends entries with a complete
// nfs block, including one for a dir no project has yet, and persists them.
func TestHandlePutServerConfig_SharedDirStorageBackends_ValidPersisted(t *testing.T) {
	srv := &Server{}
	rr, settingsPath := fileModePutServerConfig(t, srv, `{
		"server": {"shared_dir_storage": {"backend": "local", "nfs": {"mount_root": "/srv/nfs", "shares": [{"id": "share-1", "pv_name": "pv-1"}]}}},
		"runtimes": {"k8s": {"type": "kubernetes", "shared_dir_storage_backends": {"gocache": "local"}}},
		"profiles": {"gke": {"runtime": "k8s", "shared_dir_storage_backends": {"notes": "nfs", "future-dir": "nfs"}}}
	}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"shared_dir_storage_backends:", "notes: nfs", "future-dir: nfs", "gocache: local"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("settings.yaml should contain %q, got: %s", want, data)
		}
	}
}

// File-mode PUT rejects a bad per-dir entry, naming the key and writing
// nothing.
func TestHandlePutServerConfig_SharedDirStorageBackends_InvalidRejected(t *testing.T) {
	for body, key := range map[string]string{
		`{"profiles":{"gke":{"runtime":"k8s","shared_dir_storage_backends":{"notes":"ceph"}}}}`:     "profiles.gke.shared_dir_storage_backends.notes",
		`{"runtimes":{"k8s":{"type":"kubernetes","shared_dir_storage_backends":{"notes":"nfs"}}}}`:  "runtimes.k8s.shared_dir_storage_backends.notes",
		`{"profiles":{"gke":{"runtime":"k8s","shared_dir_storage_backends":{"Bad_Name":"local"}}}}`: "profiles.gke.shared_dir_storage_backends.Bad_Name",
	} {
		t.Run(key, func(t *testing.T) {
			srv := &Server{}
			rr, settingsPath := fileModePutServerConfig(t, srv, body)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), key) {
				t.Errorf("400 body should name %s, got: %s", key, rr.Body.String())
			}
			if _, err := os.Stat(settingsPath); !os.IsNotExist(err) {
				data, _ := os.ReadFile(settingsPath)
				t.Errorf("nothing should be persisted for an invalid value, got settings.yaml: %s", data)
			}
		})
	}
}

// DB-mode PUT stores shared_dir_storage_backends, and the overlay the
// co-located broker reads resolves the dir from it.
func TestPutServerConfigDB_SharedDirStorageBackends_RoundTrip(t *testing.T) {
	sdsWriteGlobalNFSBlock(t)
	old := config.GetGlobalSettingsOverlay()
	t.Cleanup(func() { config.SetGlobalSettingsOverlay(old) })
	config.SetGlobalSettingsOverlay(config.NewSettingsOverlay())

	srv, _, ops := newTestDBServer(t)
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
		`{"runtimes": {"k8s": {"type": "kubernetes"}}, "profiles": {"gke": {"runtime": "k8s", "shared_dir_storage_backends": {"notes": "nfs"}}}}`), ops)
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	profiles := sdsGetProfilesDB(t, srv, ops)
	if got := profiles["gke"].SharedDirStorageBackends["notes"]; got != "nfs" {
		t.Fatalf("GET after PUT: shared_dir_storage_backends.notes = %q, want nfs", got)
	}
	ApplySnapshot(srv, ops.Snapshot())
	gs, _, err := config.LoadGlobalSettingsWithOverlay()
	if err != nil {
		t.Fatal(err)
	}
	if backend, source, perDir := gs.ResolveSharedDirStorageBackend("gke", "notes"); backend != "nfs" || !perDir {
		t.Fatalf("overlay resolution for gke/notes = %q (%s, perDir=%v), want nfs", backend, source, perDir)
	}
}

// DB-mode PUT rejects a per-dir nfs entry without an nfs block.
func TestPutServerConfigDB_SharedDirStorageBackends_NFSWithoutBlock(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, _, ops := newTestDBServer(t)
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
		`{"profiles": {"gke": {"runtime": "k8s", "shared_dir_storage_backends": {"notes": "nfs"}}}}`), ops)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "profiles.gke.shared_dir_storage_backends.notes") {
		t.Errorf("422 body should name the key, got: %s", rr.Body.String())
	}
}
