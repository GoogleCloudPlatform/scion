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
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// `scion broker provide --project <p>` registers a local path only for the
// project it names (ptone/scion#2839): registering the CWD's unrelated
// project, often the global ~/.scion, made the broker provision p's agents
// there, where a delete cannot safely remove them.
func TestLocalPathForProvidedProject(t *testing.T) {
	const target, other = "11111111-aaaa-aaaa-aaaa-111111111111", "22222222-bbbb-bbbb-bbbb-222222222222"
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeSettings := func(scionDir, projectID string) {
		t.Helper()
		if err := os.MkdirAll(scionDir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := "schema_version: \"1\"\n"
		if projectID != "" {
			body += "hub:\n  project_id: " + projectID + "\n"
		}
		if err := os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	globalDir := filepath.Join(home, ".scion")
	writeSettings(globalDir, other)
	linked := filepath.Join(home, "linked")
	writeSettings(filepath.Join(linked, ".scion"), target)
	unrelated := filepath.Join(home, "unrelated")
	writeSettings(filepath.Join(unrelated, ".scion"), other)
	unlinked := filepath.Join(home, "unlinked")
	writeSettings(filepath.Join(unlinked, ".scion"), "")

	for _, tc := range []struct {
		name, cwd string
		wantPath  bool
	}{
		{"CWD is the named project", linked, true},
		{"CWD is the global dir (home)", home, false},
		{"CWD is another project", unrelated, false},
		{"CWD project not linked", unlinked, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(tc.cwd)
			got := localPathForProvidedProject("", target)
			if tc.wantPath {
				want, _ := filepath.EvalSymlinks(filepath.Join(linked, ".scion"))
				if gotEval, _ := filepath.EvalSymlinks(got); gotEval != want {
					t.Errorf("got %q, want the named project's %q", got, want)
				}
			} else if got != "" {
				t.Errorf("got %q, want no local path (the broker resolves the project by slug)", got)
			}
		})
	}
	// The global project itself, provided from home, keeps its path.
	t.Chdir(home)
	wantGlobal, _ := filepath.EvalSymlinks(globalDir)
	if got, _ := filepath.EvalSymlinks(localPathForProvidedProject("", other)); got != wantGlobal {
		t.Errorf("providing the global project from home registered %q, want %q", got, wantGlobal)
	}

	// The project ID fallback (no hub.project_id anywhere, so the global
	// settings are unlinked for this case, as the precedence mirrors the
	// rest of runBrokerProvide): a git project's project-id file.
	writeSettings(globalDir, "")
	fallback := filepath.Join(home, "fallback", ".scion")
	writeSettings(fallback, "")
	if err := config.WriteProjectID(fallback, target); err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Dir(fallback))
	wantFallback, _ := filepath.EvalSymlinks(fallback)
	if got, _ := filepath.EvalSymlinks(localPathForProvidedProject("", target)); got != wantFallback {
		t.Errorf("project_id fallback: got %q, want %q", got, wantFallback)
	}

	// A non-git linked project: the CWD's .scion is a marker file, and the
	// settings live in the external config dir it resolves to.
	markerRoot := filepath.Join(home, "nongit")
	if err := os.MkdirAll(markerRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := &config.ProjectMarker{ProjectID: target, ProjectName: "nongit", ProjectSlug: "nongit"}
	if err := config.WriteProjectMarker(filepath.Join(markerRoot, config.DotScion), marker); err != nil {
		t.Fatal(err)
	}
	extDir, err := marker.ExternalProjectPath()
	if err != nil {
		t.Fatal(err)
	}
	writeSettings(extDir, target)
	t.Chdir(markerRoot)
	wantExt, _ := filepath.EvalSymlinks(extDir)
	if got, _ := filepath.EvalSymlinks(localPathForProvidedProject("", target)); got != wantExt {
		t.Errorf("marker-file project: got %q, want the external dir %q", got, wantExt)
	}
	if got := localPathForProvidedProject("", other); got != "" {
		t.Errorf("marker-file project linked elsewhere: got %q, want none", got)
	}
}

// The path registered by `scion broker provide`: never a local path for a
// remote broker (--broker), with or without --project; the named project's
// path only when the CWD is that project; the CWD's project without
// --project, as before.
func TestProviderRegisterPath(t *testing.T) {
	const target = "11111111-aaaa-aaaa-aaaa-111111111111"
	home := t.TempDir()
	t.Setenv("HOME", home)
	linked := filepath.Join(home, "linked", ".scion")
	if err := os.MkdirAll(linked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(linked, "settings.yaml"),
		[]byte("schema_version: \"1\"\nhub:\n  project_id: "+target+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Dir(linked))
	want, _ := filepath.EvalSymlinks(linked)
	for _, tc := range []struct {
		name                       string
		namedProject, remoteBroker bool
		wantPath                   bool
	}{
		{"local broker, CWD project", false, false, true},
		{"local broker, --project names the CWD project", true, false, true},
		{"remote broker, CWD project", false, true, false},
		{"remote broker, --project", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := providerRegisterPath("", target, tc.namedProject, tc.remoteBroker)
			if !tc.wantPath {
				if got != "" {
					t.Errorf("got %q, want no local path", got)
				}
				return
			}
			if gotEval, _ := filepath.EvalSymlinks(got); gotEval != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
}

// runBrokerProvide end to end against a fake hub that captures the
// registered path (review NB3), covering the wiring of providerRegisterPath:
// --project from HOME, a remote --broker, and --broker naming this host's
// own broker (which, being local, still registers the linked path).
func TestRunBrokerProvide_RegisteredPath(t *testing.T) {
	const (
		target      = "11111111-aaaa-aaaa-aaaa-111111111111"
		localBroker = "aaaaaaaa-0000-0000-0000-000000000001"
		otherBroker = "bbbbbbbb-0000-0000-0000-000000000002"
	)
	for _, tc := range []struct {
		name            string
		inLinkedProject bool
		project, broker string
		wantLinkedPath  bool
	}{
		{"--project from HOME", false, target, "", false},
		{"--project from inside the named project", true, target, "", true},
		{"remote --broker from inside a linked project", true, "", otherBroker, false},
		{"remote --broker with --project", true, target, otherBroker, false},
		{"--broker naming this host's own broker", true, "", localBroker, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var gotPath *string
			hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/"+target:
					_, _ = w.Write([]byte(`{"id":"` + target + `","name":"proj","slug":"proj"}`))
				case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/runtime-brokers/"):
					id := strings.TrimPrefix(r.URL.Path, "/api/v1/runtime-brokers/")
					_, _ = w.Write([]byte(`{"id":"` + id + `","name":"broker-` + id[:8] + `"}`))
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
			if err := brokercredentials.NewMultiStore("").Save(&brokercredentials.BrokerCredentials{
				Name: "test-hub", BrokerID: localBroker, HubEndpoint: hub.URL, SecretKey: "dGVzdA==",
				AuthMode: brokercredentials.AuthModeDevAuth,
			}); err != nil {
				t.Fatal(err)
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

			saved := []any{brokerProjectID, brokerBrokerID, brokerHubFlag, autoConfirm, brokerMakeDefault, projectPath}
			t.Cleanup(func() {
				brokerProjectID, brokerBrokerID, brokerHubFlag = saved[0].(string), saved[1].(string), saved[2].(string)
				autoConfirm, brokerMakeDefault, projectPath = saved[3].(bool), saved[4].(bool), saved[5].(string)
			})
			brokerProjectID, brokerBrokerID, brokerHubFlag = tc.project, tc.broker, "test-hub"
			autoConfirm, brokerMakeDefault, projectPath = true, false, ""

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
