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
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hub link follow-ups (template sync and the broker list) target the hub
// project ID, not the local project_id the link started from, when the
// hub picks a different ID.
func TestRunHubLink_FollowUpsUseHubProjectID(t *testing.T) {
	const localID = "local-id"
	tests := []struct {
		name         string
		registeredID string // ID the hub returns on register
		nameMatchID  string // hub project with the same name, if any
		wantHubID    string
	}{
		{
			name:         "links to an existing hub project by name",
			registeredID: "hub-id",
			nameMatchID:  "hub-id",
			wantHubID:    "hub-id",
		},
		{
			name:         "hub returns a different ID on register",
			registeredID: "p-linked",
			wantHubID:    "p-linked",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, globalDir := setupProvideTest(t)
			origGlobal := globalMode
			t.Cleanup(func() { globalMode = origGlobal })
			globalMode = true
			t.Setenv("SCION_AUTH_TOKEN", "test-token")

			var (
				mu           sync.Mutex
				brokerQuery  []string
				syncTargetID []string
			)
			origOffer := offerTemplateSyncOnLinkFn
			t.Cleanup(func() { offerTemplateSyncOnLinkFn = origOffer })
			offerTemplateSyncOnLinkFn = func(_, _, projectID string) {
				syncTargetID = append(syncTargetID, projectID)
			}

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/healthz":
					_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
				case r.URL.Path == "/api/v1/projects/register" && r.Method == http.MethodPost:
					var body map[string]interface{}
					_ = json.NewDecoder(r.Body).Decode(&body)
					_ = json.NewEncoder(w).Encode(map[string]interface{}{
						"project": map[string]interface{}{"id": tt.registeredID, "name": body["name"], "slug": body["name"]},
						"created": true,
					})
				case r.URL.Path == "/api/v1/projects" && r.Method == http.MethodGet:
					projects := []interface{}{}
					if tt.nameMatchID != "" {
						projects = append(projects, map[string]interface{}{
							"id": tt.nameMatchID, "name": "global", "slug": "global",
						})
					}
					_ = json.NewEncoder(w).Encode(map[string]interface{}{"projects": projects})
				case r.URL.Path == "/api/v1/runtime-brokers" && r.Method == http.MethodGet:
					mu.Lock()
					brokerQuery = append(brokerQuery, r.URL.Query().Get("projectId"))
					mu.Unlock()
					_ = json.NewEncoder(w).Encode(map[string]interface{}{"brokers": []interface{}{}})
				default:
					w.WriteHeader(http.StatusNotFound)
					_ = json.NewEncoder(w).Encode(map[string]interface{}{
						"error": map[string]interface{}{"code": "not_found", "message": "not found"},
					})
				}
			}))
			t.Cleanup(server.Close)
			t.Setenv("SCION_HUB_ENDPOINT", server.URL)

			require.NoError(t, config.UpdateSetting(globalDir, "project_id", localID, true))

			require.NoError(t, runHubLink(hubLinkCmd, nil))

			assert.Equal(t, []string{tt.wantHubID}, syncTargetID, "template sync target")
			mu.Lock()
			defer mu.Unlock()
			assert.Equal(t, []string{tt.wantHubID}, brokerQuery, "broker list project filter")
		})
	}
}
