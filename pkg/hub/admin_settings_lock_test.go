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
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/internal/settingslockhook"
	"github.com/GoogleCloudPlatform/scion/pkg/util/filelock"
)

// While another holder keeps the settings lock, the file-mode server-config
// PUT waits the (shortened) lock wait and answers 503 with a neutral "try
// again" message, writing nothing. Once the holder releases the lock, the
// same PUT succeeds, which shows the handler kept no lock of its own.
func TestHandlePutServerConfig_SettingsLockTimeoutIs503(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	globalDir := filepath.Join(tmpHome, ".scion")
	if err := os.MkdirAll(globalDir, 0700); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(globalDir, "settings.yaml")
	const orig = "schema_version: \"1\"\n"
	if err := os.WriteFile(settingsPath, []byte(orig), 0644); err != nil {
		t.Fatal(err)
	}
	// Another process's lock: a separate open file description conflicts
	// with the settings lock's flock even within this process.
	held, err := filelock.Acquire(filepath.Join(globalDir, ".settings.lock"), 0644, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(held.Unlock)
	t.Cleanup(settingslockhook.Set(300 * time.Millisecond))

	srv := &Server{}
	const body = `{"server":{"auth":{"default_user_role":"viewer"}}}`
	put := func() *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		srv.handleAdminServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", body))
		return rr
	}

	start := time.Now()
	rr := put()
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 while the lock is held, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), ErrCodeUnavailable) || !strings.Contains(rr.Body.String(), "Settings are being written; try again") {
		t.Errorf("503 body should carry %s and the try-again message, got: %s", ErrCodeUnavailable, rr.Body.String())
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("waited %s; the shortened lock wait was not used", d)
	}
	if data, _ := os.ReadFile(settingsPath); string(data) != orig {
		t.Errorf("settings.yaml changed despite the lock timeout:\n%s", data)
	}

	held.Unlock()
	if rr := put(); rr.Code != http.StatusOK {
		t.Fatalf("expected 200 after the lock is released, got %d: %s", rr.Code, rr.Body.String())
	}
	if data, _ := os.ReadFile(settingsPath); !strings.Contains(string(data), "default_user_role: viewer") {
		t.Errorf("settings.yaml not written after the lock is released:\n%s", data)
	}
}
