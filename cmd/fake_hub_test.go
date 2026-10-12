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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHub is a minimal hub for cmd tests now that every command goes
// through a hub: health, the dev user, project lookup and register, an
// empty project list, no brokers or providers and an empty agent list.
// Anything else is 404. It records the paths it served.
type fakeHub struct {
	*httptest.Server
	mu    sync.Mutex
	paths []string
}

// Requests returns the "METHOD path" of every request served so far.
func (h *fakeHub) Requests() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.paths...)
}

// withFakeHub starts a fakeHub and points the CLI at it, in a fresh temp
// HOME with the leaked SCION_* variables cleared (hermeticCLIEnv):
// SCION_HUB_ENDPOINT is the fake hub's URL and SCION_DEV_TOKEN a dev token.
// With SCION_HUB_ENDPOINT set the CLI resolves the hub as a hub-connected
// context, so requireHub returns a context after the health check, with no
// registration step.
func withFakeHub(t *testing.T) *fakeHub {
	t.Helper()
	hermeticCLIEnv(t)
	h := &fakeHub{}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.paths = append(h.paths, r.Method+" "+r.URL.Path)
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		rest := strings.TrimPrefix(r.URL.Path, "/api/v1/projects/")
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case r.URL.Path == "/api/v1/auth/me":
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "dev-user", "email": "dev@localhost", "displayName": "Dev", "role": "admin"})
		case r.URL.Path == "/api/v1/runtime-brokers":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"brokers": []interface{}{}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/register":
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			id, _ := body["id"].(string)
			name, _ := body["name"].(string)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"project": map[string]string{"id": id, "name": name},
				"created": true,
			})
		case r.URL.Path == "/api/v1/projects":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"projects": []interface{}{}})
		case strings.HasSuffix(rest, "/providers"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"providers": []interface{}{}})
		case strings.HasSuffix(rest, "/agents") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"agents":     []interface{}{},
				"serverTime": time.Now().UTC().Format(time.RFC3339Nano),
			})
		case rest != r.URL.Path && rest != "" && !strings.Contains(rest, "/"):
			_ = json.NewEncoder(w).Encode(map[string]string{"id": rest, "name": "project"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(h.Close)
	t.Setenv("SCION_HUB_ENDPOINT", h.URL)
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_fakehub")
	return h
}
