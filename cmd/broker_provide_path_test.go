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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// runBrokerProvide end to end against a fake hub that captures the
// registered path (ptone/scion#2839): the current project's path is never
// registered for another host's broker, while --broker naming this host's
// own broker keeps it and an explicit --path is still sent. The remote
// decision uses the resolved broker ID, so --broker given by name behaves
// like --broker given by ID; with no local broker ID at all, any --broker
// is remote. --project without --path sends no path.
func TestRunBrokerProvide_RegisteredPath(t *testing.T) {
	const (
		target      = "11111111-aaaa-aaaa-aaaa-111111111111"
		localBroker = "aaaaaaaa-0000-0000-0000-000000000001"
		otherBroker = "bbbbbbbb-0000-0000-0000-000000000002"
	)
	brokerNames := map[string]string{localBroker: "local-host", otherBroker: "remote-host"}
	for _, tc := range []struct {
		name            string
		inLinkedProject bool
		project, broker string
		noLocalBroker   bool // no broker credentials on this host
		explicitPath    bool // pass --path <linked project>
		wantLinkedPath  bool
	}{
		{"--project from HOME", false, target, "", false, false, false},
		{"--project from inside the named project", true, target, "", false, false, false},
		{"no flags from inside the linked project", true, "", "", false, false, true},
		{"remote --broker from inside a linked project", true, "", otherBroker, false, false, false},
		{"remote --broker with --project", true, target, otherBroker, false, false, false},
		{"remote --broker with an explicit --path", true, target, otherBroker, false, true, true},
		{"--broker naming this host's own broker", true, "", localBroker, false, false, true},
		{"--broker by name: this host's own broker", true, "", "local-host", false, false, true},
		{"--broker by name: a remote broker", true, "", "remote-host", false, false, false},
		{"--broker with no local broker ID on this host", true, "", localBroker, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var gotPath *string
			hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/"+target:
					_, _ = w.Write([]byte(`{"id":"` + target + `","name":"proj","slug":"proj"}`))
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/runtime-brokers":
					name := r.URL.Query().Get("name")
					var found []string
					for id, n := range brokerNames {
						if n == name {
							found = append(found, `{"id":"`+id+`","name":"`+n+`"}`)
						}
					}
					_, _ = w.Write([]byte(`{"brokers":[` + strings.Join(found, ",") + `]}`))
				case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/runtime-brokers/"):
					id := strings.TrimPrefix(r.URL.Path, "/api/v1/runtime-brokers/")
					n, ok := brokerNames[id]
					if !ok {
						w.WriteHeader(http.StatusNotFound)
						_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"broker not found"}}`))
						return
					}
					_, _ = w.Write([]byte(`{"id":"` + id + `","name":"` + n + `"}`))
				case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/register":
					var req hubclient.RegisterProjectRequest
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						http.Error(w, err.Error(), http.StatusBadRequest)
						return
					}
					mu.Lock()
					p := req.Path
					gotPath = &p
					mu.Unlock()
					_, _ = w.Write([]byte(`{"project":{"id":"` + target + `","name":"proj","slug":"proj"}}`))
				default:
					http.NotFound(w, r)
				}
			}))
			defer hub.Close()

			home := t.TempDir()
			t.Setenv("HOME", home)
			hubConn := ""
			if !tc.noLocalBroker {
				hubConn = "test-hub"
				if err := brokercredentials.NewMultiStore("").Save(&brokercredentials.BrokerCredentials{
					Name: hubConn, BrokerID: localBroker, HubEndpoint: hub.URL, SecretKey: "dGVzdA==",
					AuthMode: brokercredentials.AuthModeDevAuth,
				}); err != nil {
					t.Fatal(err)
				}
			}
			linked := filepath.Join(home, "linked", ".scion")
			if err := os.MkdirAll(linked, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(linked, "settings.yaml"),
				[]byte("schema_version: \"1\"\nhub:\n  project_id: "+target+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.inLinkedProject {
				t.Chdir(filepath.Dir(linked))
			} else {
				t.Chdir(home)
			}

			saved := []any{brokerProjectID, brokerBrokerID, brokerHubFlag, autoConfirm, brokerMakeDefault, projectPath, hubEndpoint, brokerProvidePath}
			t.Cleanup(func() {
				brokerProjectID, brokerBrokerID, brokerHubFlag = saved[0].(string), saved[1].(string), saved[2].(string)
				autoConfirm, brokerMakeDefault, projectPath = saved[3].(bool), saved[4].(bool), saved[5].(string)
				hubEndpoint, brokerProvidePath = saved[6].(string), saved[7].(string)
			})
			brokerProjectID, brokerBrokerID, brokerHubFlag = tc.project, tc.broker, hubConn
			autoConfirm, brokerMakeDefault, projectPath = true, false, ""
			hubEndpoint = ""
			brokerProvidePath = ""
			if tc.explicitPath {
				brokerProvidePath = filepath.Dir(linked)
			}
			if tc.noLocalBroker {
				// No hub connection credentials: reach the hub through the
				// endpoint override instead.
				hubEndpoint = hub.URL
			}

			if err := runBrokerProvide(brokerProvideCmd, nil); err != nil {
				t.Fatalf("runBrokerProvide: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if gotPath == nil {
				t.Fatal("the hub received no register request")
			}
			if !tc.wantLinkedPath {
				if *gotPath != "" {
					t.Errorf("registered path %q, want none (the broker resolves the project by slug)", *gotPath)
				}
				return
			}
			want, _ := filepath.EvalSymlinks(linked)
			if got, _ := filepath.EvalSymlinks(*gotPath); got != want {
				t.Errorf("registered path %q, want the linked project's %q", *gotPath, want)
			}
		})
	}
}
