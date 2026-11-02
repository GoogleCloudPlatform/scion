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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// invalidProjectStorageBodies are server-config PUT bodies whose
// server.project_storage breaks a field rule, with the field reported.
var invalidProjectStorageBodies = map[string]string{
	`{"server":{"project_storage":{"backend":"nfs","nfs":{"mount_root":"/mnt/x","shares":[{"id":"../x","server":"10.0.0.2","export":"/e"}]}}}}`:                       "server.project_storage.nfs.shares[0].id",
	`{"server":{"project_storage":{"backend":"nfs","nfs":{"mount_root":"/mnt/x","shares":[{"id":"a","server":"-oexec","export":"/e"}]}}}}`:                            "server.project_storage.nfs.shares[0].server",
	`{"server":{"project_storage":{"backend":"nfs","nfs":{"mount_root":"/mnt/x","shares":[{"id":"a","server":"10.0.0.2","export":"-oexec"}]}}}}`:                      "server.project_storage.nfs.shares[0].export",
	`{"server":{"project_storage":{"backend":"nfs","nfs":{"mount_root":"/mnt/x","mount_options":"-oexec","shares":[{"id":"a","server":"10.0.0.2","export":"/e"}]}}}}`: "server.project_storage.nfs.mount_options",
}

// assertProjectStorageFieldRefusal checks a 422 validation_error whose
// details.errors name want.
func assertProjectStorageFieldRefusal(t *testing.T, rr *httptest.ResponseRecorder, want string) {
	t.Helper()
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, rr.Body.String())
	}
	found := false
	for _, e := range settingsErrorEntries(t, rr) {
		// This route is admin-only, so the entry's message carries the value.
		if e.Field == want && strings.Contains(e.Message, "(got ") {
			found = true
		}
	}
	if !found {
		t.Errorf("details.errors has no entry for %s with its value (body %s)", want, rr.Body.String())
	}
	if !strings.Contains(resp.Error.Message, want) {
		t.Errorf("message %q does not name %s", resp.Error.Message, want)
	}
	if strings.Contains(rr.Body.String(), "requires_restart") {
		t.Errorf("a refused write reports requires_restart: %s", rr.Body.String())
	}
}

// TestHandlePutServerConfig_ProjectStorageFieldsValidated: a file-mode PUT
// of an invalid server.project_storage is refused with 422 and the field
// errors, and nothing is written.
func TestHandlePutServerConfig_ProjectStorageFieldsValidated(t *testing.T) {
	for body, want := range invalidProjectStorageBodies {
		t.Run(want, func(t *testing.T) {
			rr, settingsPath := fileModePutServerConfig(t, &Server{}, body)
			assertProjectStorageFieldRefusal(t, rr, want)
			if _, err := os.Stat(settingsPath); !os.IsNotExist(err) {
				data, _ := os.ReadFile(settingsPath)
				t.Errorf("nothing should be persisted for an invalid block, got settings.yaml: %s", data)
			}
		})
	}
}
