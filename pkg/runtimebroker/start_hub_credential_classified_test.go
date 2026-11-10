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

package runtimebroker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// TestStartResumeRestartHubCredential pins what the broker hands the agent
// manager for the start, resume and restart bodies the hub sends for an
// existing agent: the credential in the resolved env reaches
// StartOptions.HubIssuedToken when the body's env classifications mark it
// secret-bootstrap, and is dropped when the body carries no
// classifications. The resume flag is passed through either way.
func TestStartResumeRestartHubCredential(t *testing.T) {
	// Made-up value; never a real credential.
	const credential = "fake.classified.hub.credential"
	for _, tc := range []struct {
		name, path string
		resume     bool
	}{
		{"start", "/api/v1/agents/test-agent-2/start", false},
		{"resume", "/api/v1/agents/test-agent-2/start", true},
		{"restart", "/api/v1/agents/test-agent-1/restart", false},
	} {
		for _, classified := range []bool{true, false} {
			label := tc.name + "/classified"
			if !classified {
				label = tc.name + "/no classifications"
			}
			t.Run(label, func(t *testing.T) {
				srv := newTestServer(t)
				mgr := srv.manager.(*mockManager)
				body := map[string]any{
					"resolvedEnv": map[string]string{"SCION_AUTH_TOKEN": credential, "SCION_AGENT_ID": "hub-agent-id"},
				}
				if classified {
					body["envClassifications"] = map[string]api.EnvKind{
						"SCION_AUTH_TOKEN": api.EnvKindSecretBootstrap,
						"SCION_AGENT_ID":   api.EnvKindPlain,
					}
				}
				if tc.resume {
					body["resume"] = true
				}
				data, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(string(data)))
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				srv.Handler().ServeHTTP(w, req)
				if strings.Contains(w.Body.String(), credential) {
					t.Errorf("response body carries the credential value")
				}
				if mgr.StartCalls() != 1 {
					t.Fatalf("Manager.Start calls = %d, want 1 (status %d: %s)", mgr.StartCalls(), w.Code, w.Body.String())
				}
				opts := mgr.LastStartOpts()
				if classified {
					if opts.HubIssuedToken != credential {
						t.Errorf("HubIssuedToken not the classified credential (len %d)", len(opts.HubIssuedToken))
					}
				} else if opts.HubIssuedToken != "" {
					t.Errorf("HubIssuedToken set (len %d) without the hub's classification", len(opts.HubIssuedToken))
				}
				if _, ok := opts.Env["SCION_AUTH_TOKEN"]; ok {
					t.Errorf("the credential reached the agent env as a plain value")
				}
				if opts.Resume != tc.resume {
					t.Errorf("Resume = %v, want %v", opts.Resume, tc.resume)
				}
			})
		}
	}
}
