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

// TestResolveProvidePath_Remote covers ptone/scion#3157: for a remote broker
// an explicit --path names a directory on the broker's host, so it is sent
// as given without consulting this host's filesystem, and a non-absolute
// path is refused.
func TestResolveProvidePath_Remote(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	t.Run("absolute path missing locally is sent unchanged", func(t *testing.T) {
		const p = "/srv/remote-only/checkout"
		if _, err := os.Stat(p); err == nil {
			t.Skipf("%s exists on this host", p)
		}
		got, err := resolveProvidePath(p, "proj", "proj", true)
		if err != nil {
			t.Fatalf("resolveProvidePath: %v", err)
		}
		if got != p {
			t.Errorf("got %q, want %q unchanged", got, p)
		}
	})

	t.Run("local project root is not rewritten to its .scion dir", func(t *testing.T) {
		root := filepath.Join(home, "proj")
		if err := os.MkdirAll(filepath.Join(root, ".scion"), 0o755); err != nil {
			t.Fatal(err)
		}
		got, err := resolveProvidePath(root, "proj", "proj", true)
		if err != nil {
			t.Fatalf("resolveProvidePath: %v", err)
		}
		if got != root {
			t.Errorf("got %q, want %q unchanged", got, root)
		}
	})

	for _, p := range []string{"relative/checkout", "./checkout", "global", "home"} {
		t.Run("rejects non-absolute "+p, func(t *testing.T) {
			_, err := resolveProvidePath(p, "proj", "proj", true)
			if err == nil || !strings.Contains(err.Error(), "absolute path") {
				t.Fatalf("got err %v, want an absolute-path error", err)
			}
		})
	}
}

// TestResolveProvidePath_LocalUnchanged guards the local-broker behaviour:
// the path is still resolved on this host (a project root becomes its .scion
// dir) and the global directory is still refused for other projects.
func TestResolveProvidePath_LocalUnchanged(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, "proj")
	if err := os.MkdirAll(filepath.Join(root, ".scion"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := resolveProvidePath(root, "proj", "proj", false)
	if err != nil {
		t.Fatalf("resolveProvidePath: %v", err)
	}
	want, _ := filepath.EvalSymlinks(filepath.Join(root, ".scion"))
	if got != want {
		t.Errorf("got %q, want the resolved %q", got, want)
	}

	if _, err := resolveProvidePath("global", "proj", "proj", false); err == nil {
		t.Error("local --path global for a non-global project must be refused")
	}
}

// TestRunBrokerProvide_RemoteExplicitPathSentUnchanged runs provide end to
// end against a fake hub: with --broker naming another host's broker, an
// explicit --path that does not exist on this host is registered as given,
// while --broker naming this host's own broker still resolves the path
// locally.
func TestRunBrokerProvide_RemoteExplicitPathSentUnchanged(t *testing.T) {
	const (
		target      = "11111111-aaaa-aaaa-aaaa-111111111111"
		localBroker = "aaaaaaaa-0000-0000-0000-000000000001"
		otherBroker = "bbbbbbbb-0000-0000-0000-000000000002"
	)
	brokerNames := map[string]string{localBroker: "local-host", otherBroker: "remote-host"}

	for _, tc := range []struct {
		name     string
		broker   string
		local    bool // path is a local project root (else a remote-only path)
		wantPath func(home string) string
	}{
		{"remote broker by ID, path missing locally", otherBroker, false,
			func(string) string { return "/srv/remote-only/checkout" }},
		{"remote broker by name, path missing locally", "remote-host", false,
			func(string) string { return "/srv/remote-only/checkout" }},
		{"remote broker, local project root sent as given", otherBroker, true,
			func(home string) string { return filepath.Join(home, "linked") }},
		{"this host's broker resolves the path locally", localBroker, true,
			func(home string) string {
				p, _ := filepath.EvalSymlinks(filepath.Join(home, "linked", ".scion"))
				return p
			}},
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
			const hubConn = "test-hub"
			if err := brokercredentials.NewMultiStore("").Save(&brokercredentials.BrokerCredentials{
				Name: hubConn, BrokerID: localBroker, HubEndpoint: hub.URL, SecretKey: "dGVzdA==",
				AuthMode: brokercredentials.AuthModeDevAuth,
			}); err != nil {
				t.Fatal(err)
			}
			linked := filepath.Join(home, "linked", ".scion")
			if err := os.MkdirAll(linked, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Chdir(home)

			saved := []any{brokerProjectID, brokerBrokerID, brokerHubFlag, autoConfirm, brokerMakeDefault, projectPath, hubEndpoint, brokerProvidePath}
			t.Cleanup(func() {
				brokerProjectID, brokerBrokerID, brokerHubFlag = saved[0].(string), saved[1].(string), saved[2].(string)
				autoConfirm, brokerMakeDefault, projectPath = saved[3].(bool), saved[4].(bool), saved[5].(string)
				hubEndpoint, brokerProvidePath = saved[6].(string), saved[7].(string)
			})
			brokerProjectID, brokerBrokerID, brokerHubFlag = target, tc.broker, hubConn
			autoConfirm, brokerMakeDefault, projectPath, hubEndpoint = true, false, "", ""
			if tc.local {
				brokerProvidePath = filepath.Dir(linked)
			} else {
				brokerProvidePath = "/srv/remote-only/checkout"
				if _, err := os.Stat(brokerProvidePath); err == nil {
					t.Skipf("%s exists on this host", brokerProvidePath)
				}
			}

			if err := runBrokerProvide(brokerProvideCmd, nil); err != nil {
				t.Fatalf("runBrokerProvide: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if gotPath == nil {
				t.Fatal("the hub received no register request")
			}
			if want := tc.wantPath(home); *gotPath != want {
				t.Errorf("registered path %q, want %q", *gotPath, want)
			}
		})
	}
}

// TestProvidePathSummary checks that provide reports the path it sent as
// requested, and labels a remote broker's path with that broker rather than
// calling it local.
func TestProvidePathSummary(t *testing.T) {
	if got, want := providePathSummary("/srv/p", "remote-host", true), "Requested project path on remote-host: /srv/p"; got != want {
		t.Errorf("remote summary = %q, want %q", got, want)
	}
	if got, want := providePathSummary("/home/u/p/.scion", "local-host", false), "Requested local project path: /home/u/p/.scion"; got != want {
		t.Errorf("local summary = %q, want %q", got, want)
	}
}

// TestResolveProvidePath_RemoteCleansAndRefusesDotScion checks that a remote
// path is cleaned, and that a path naming a .scion directory is refused with
// a hint to pass the project root that contains it.
func TestResolveProvidePath_RemoteCleansAndRefusesDotScion(t *testing.T) {
	got, err := resolveProvidePath("/srv/a/../proj/", "proj", "proj", true)
	if err != nil {
		t.Fatalf("resolveProvidePath: %v", err)
	}
	if got != "/srv/proj" {
		t.Errorf("got %q, want the cleaned /srv/proj", got)
	}

	for _, p := range []string{"/srv/proj/.scion", "/srv/proj/.scion/", "/srv/proj/./.scion"} {
		_, err := resolveProvidePath(p, "proj", "proj", true)
		if err == nil {
			t.Errorf("%q: expected a .scion refusal", p)
			continue
		}
		if !strings.Contains(err.Error(), "project root") || !strings.Contains(err.Error(), "/srv/proj") {
			t.Errorf("%q: error %q should point at the project root /srv/proj", p, err)
		}
	}
}
