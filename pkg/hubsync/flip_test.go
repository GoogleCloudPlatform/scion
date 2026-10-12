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

package hubsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// flipHub is a fake hub for the hub-always contract. Projects start
// unregistered; register registers the requested project ID (or projectID).
// Name matches and provider status are configurable; agent creates are
// recorded.
type flipHub struct {
	projectID string
	matches   []map[string]string // returned for a projects list by name
	online    bool                // a provider is online

	mu         sync.Mutex
	registered map[string]bool
	creates    []hubclient.CreateAgentRequest
}

func (h *flipHub) serve(t *testing.T) *httptest.Server {
	t.Helper()
	h.registered = map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		rest := strings.TrimPrefix(r.URL.Path, "/api/v1/projects/")
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/register":
			var req hubclient.RegisterProjectRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			id := req.ID
			if id == "" {
				id = h.projectID
			}
			h.registered[id] = true
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"project": map[string]string{"id": id, "name": req.Name},
				"created": true,
			})
		case r.URL.Path == "/api/v1/projects":
			matches := h.matches
			if matches == nil {
				matches = []map[string]string{}
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"projects": matches})
		case strings.HasSuffix(rest, "/providers"):
			providers := []interface{}{}
			if h.online {
				providers = append(providers, map[string]string{"brokerId": "b1", "brokerName": "local", "status": "online"})
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"providers": providers})
		case strings.HasSuffix(rest, "/agents") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"agents":     []interface{}{},
				"serverTime": time.Now().UTC().Format(time.RFC3339Nano),
			})
		case strings.HasSuffix(rest, "/agents") && r.Method == http.MethodPost:
			var req hubclient.CreateAgentRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			h.creates = append(h.creates, req)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"agent": map[string]string{"id": "id-" + req.Name, "name": req.Name},
			})
		case rest != r.URL.Path && !strings.Contains(rest, "/") && h.registered[rest]:
			_ = json.NewEncoder(w).Encode(map[string]string{"id": rest, "name": "repo"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// writeGlobalSettings writes content as the global settings and moves into
// HOME so the global project is resolved. It returns the global directory.
func writeGlobalSettings(t *testing.T, home, content string) string {
	t.Helper()
	globalDir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(home)
	return globalDir
}

// addLocalAgent creates an agent directory as local-only mode left it.
func addLocalAgent(t *testing.T, projectDir, name string) {
	t.Helper()
	dir := filepath.Join(projectDir, "agents", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scion-agent.yaml"), []byte("harness: claude\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// closedLoopbackURL returns a loopback URL on which nothing listens.
func closedLoopbackURL(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return "http://" + addr
}

func TestEnsureHubReady_NoEndpoint(t *testing.T) {
	const plain = "No hub configured. Start the local hub with 'scion server start' (first run opens setup), " +
		"or set a remote one with 'scion config set hub.endpoint <url>'."
	tests := []struct {
		name     string
		settings string
		agents   []string
		want     string
	}{
		{name: "fresh project", settings: "project_id: p1\n", want: plain},
		{name: "stale hub.enabled=true does not need an endpoint any less", settings: "project_id: p1\nhub:\n  enabled: true\n", want: plain},
		{name: "local-mode project with agents gets the migration text", settings: "project_id: p1\n", agents: []string{"a1"}, want: MigrationNotice},
		{name: "a linked project with agents gets the plain text", settings: "project_id: p1\nhub:\n  linked: true\n", agents: []string{"a1"}, want: plain},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := clearHubCredentialEnv(t)
			globalDir := writeGlobalSettings(t, home, tc.settings)
			for _, a := range tc.agents {
				addLocalAgent(t, globalDir, a)
			}
			hubCtx, err := EnsureHubReady("", EnsureHubReadyOptions{AutoConfirm: true})
			if hubCtx != nil {
				t.Errorf("hubCtx = %+v, want nil with an error", hubCtx)
			}
			if !errors.Is(err, ErrHubNotConfigured) {
				t.Fatalf("error = %v, want ErrHubNotConfigured", err)
			}
			if err.Error() != tc.want {
				t.Errorf("error text = %q, want %q", err.Error(), tc.want)
			}
		})
	}
}

func TestEnsureHubReady_Unreachable(t *testing.T) {
	t.Run("local workstation endpoint", func(t *testing.T) {
		home := clearHubCredentialEnv(t)
		t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
		writeGlobalSettings(t, home, "project_id: p1\n")
		_, err := EnsureHubReady("", EnsureHubReadyOptions{EndpointOverride: closedLoopbackURL(t)})
		var notRunning *LocalServerNotRunningError
		if !errors.As(err, &notRunning) {
			t.Fatalf("error = %v, want LocalServerNotRunningError", err)
		}
		if want := "Local scion server is not running. Start it with 'scion server start'."; err.Error() != want {
			t.Errorf("error text = %q, want %q", err.Error(), want)
		}
	})
	t.Run("remote endpoint", func(t *testing.T) {
		home := clearHubCredentialEnv(t)
		t.Setenv("SCION_HUB_TOKEN", "scion_pat_test")
		writeGlobalSettings(t, home, "project_id: p1\n")
		ep := closedLoopbackURL(t) // loopback, but not reached with the dev token
		_, err := EnsureHubReady("", EnsureHubReadyOptions{EndpointOverride: ep})
		if err == nil {
			t.Fatal("EnsureHubReady succeeded against a closed endpoint")
		}
		var notRunning *LocalServerNotRunningError
		if errors.As(err, &notRunning) {
			t.Fatalf("remote endpoint got the local-server error: %v", err)
		}
		for _, want := range []string{"hub at " + ep + " is not responding", "scion hub status"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not contain %q", err, want)
			}
		}
	})
}

// TestEnsureHubReady_StaleDisableIgnored checks that a stale hub.enabled=false
// or hub.local_only=true no longer keeps the CLI off a configured hub, and
// that the first link prints the one-time notice saying so.
func TestEnsureHubReady_StaleDisableIgnored(t *testing.T) {
	for _, setting := range []string{"enabled: false", "local_only: true"} {
		t.Run(setting, func(t *testing.T) {
			home := clearHubCredentialEnv(t)
			t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
			hub := &flipHub{projectID: "stale-project"}
			srv := hub.serve(t)
			globalDir := writeGlobalSettings(t, home, fmt.Sprintf("project_id: stale-project\nhub:\n  %s\n", setting))
			out := withPromptIO(t, strings.NewReader(""), false)

			hubCtx, err := EnsureHubReady("", EnsureHubReadyOptions{EndpointOverride: srv.URL})
			if err != nil {
				t.Fatalf("EnsureHubReady: %v", err)
			}
			if hubCtx == nil || hubCtx.Endpoint != srv.URL {
				t.Fatalf("hubCtx = %+v, want the configured hub", hubCtx)
			}
			if !strings.Contains(out.String(), legacyModeIgnoredNotice) {
				t.Errorf("output missing the one-time notice; got:\n%s", out.String())
			}
			if linked := ProjectLinkedSetting(globalDir, true); linked == nil || !*linked {
				t.Fatalf("hub.linked = %v, want true after the first link", linked)
			}

			// Second command: linked is recorded, so the notice is not repeated.
			out.Reset()
			if _, err := EnsureHubReady("", EnsureHubReadyOptions{EndpointOverride: srv.URL}); err != nil {
				t.Fatalf("second EnsureHubReady: %v", err)
			}
			if strings.Contains(out.String(), legacyModeIgnoredNotice) {
				t.Errorf("notice repeated on the second command:\n%s", out.String())
			}
		})
	}
}

// TestEnsureHubReady_LocalHubNameMatchKeepsPrompt pins the matching-projects
// prompt on the local workstation hub: with name matches (and no ID match)
// auto-link does not pick silently. Under --yes the deterministic answer is
// the prompt's existing one: link to the first match.
func TestEnsureHubReady_LocalHubNameMatchKeepsPrompt(t *testing.T) {
	home := clearHubCredentialEnv(t)
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
	hub := &flipHub{projectID: "local-project", matches: []map[string]string{
		{"id": "existing-1", "name": "global", "slug": "global"},
		{"id": "existing-2", "name": "global", "slug": "global-2"},
	}}
	srv := hub.serve(t)
	writeGlobalSettings(t, home, "project_id: local-project\n")
	out := withPromptIO(t, strings.NewReader(""), false)

	hubCtx, err := EnsureHubReady("", EnsureHubReadyOptions{EndpointOverride: srv.URL, AutoConfirm: true})
	if err != nil {
		t.Fatalf("EnsureHubReady: %v", err)
	}
	if !strings.Contains(out.String(), "Found 2 existing project(s) with the name 'global'") {
		t.Errorf("matching-projects prompt not shown on the local hub; output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Auto-linking to: global (ID: existing-1)") {
		t.Errorf("--yes did not link to the first match; output:\n%s", out.String())
	}
	if strings.Contains(out.String(), "Linking project 'global' to the local hub") {
		t.Errorf("auto-link linked silently despite name matches; output:\n%s", out.String())
	}
	if hubCtx == nil {
		t.Fatal("hubCtx is nil")
	}
	// The link registers this directory under the selected hub project's
	// ID; it is not registered as a new project under its own ID.
	if !hub.registered["existing-1"] || hub.registered["local-project"] {
		t.Errorf("registered = %v, want only existing-1 (linked, not registered as new)", hub.registered)
	}

	// Without a terminal and without --yes, nothing is picked.
	home2 := clearHubCredentialEnv(t)
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
	hub2 := &flipHub{projectID: "local-project", matches: hub.matches}
	srv2 := hub2.serve(t)
	writeGlobalSettings(t, home2, "project_id: local-project\n")
	withPromptIO(t, strings.NewReader(""), false)
	if _, err := EnsureHubReady("", EnsureHubReadyOptions{EndpointOverride: srv2.URL}); !errors.Is(err, ErrNoTerminal) {
		t.Errorf("error = %v, want ErrNoTerminal (no silent pick)", err)
	}
}

// TestEnsureHubReady_AutoLinkImportsLocalAgents checks the one-release
// import on the local workstation hub: the first command links the project,
// imports its local-mode agents without a prompt (a stopped agent is
// provisioned, not started), prints the migration lines and records the link.
func TestEnsureHubReady_AutoLinkImportsLocalAgents(t *testing.T) {
	home := clearHubCredentialEnv(t)
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
	hub := &flipHub{projectID: "import-project", online: true}
	srv := hub.serve(t)
	globalDir := writeGlobalSettings(t, home, "project_id: import-project\n")
	addLocalAgent(t, globalDir, "a1")
	withImportRuntime(t, &runtime.MockRuntime{
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
	})
	out := withPromptIO(t, strings.NewReader(""), false)

	if _, err := EnsureHubReady("", EnsureHubReadyOptions{EndpointOverride: srv.URL}); err != nil {
		t.Fatalf("EnsureHubReady: %v", err)
	}
	if len(hub.creates) != 1 || hub.creates[0].Name != "a1" || !hub.creates[0].ProvisionOnly {
		t.Fatalf("creates = %+v, want a1 provisioned without starting", hub.creates)
	}
	for _, want := range []string{
		"Linking project 'global' to the local hub",
		"Local-only mode has been removed: scion now runs agents through a hub.",
		"Importing 1 existing agent(s)",
		"+ a1 (imported as created (not started))",
		LocalModeRemovalDocsURL,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q; got:\n%s", want, out.String())
		}
	}
	if linked := ProjectLinkedSetting(globalDir, true); linked == nil || !*linked {
		t.Fatalf("hub.linked = %v, want true", linked)
	}

	// The next command does not import again.
	out.Reset()
	if _, err := EnsureHubReady("", EnsureHubReadyOptions{EndpointOverride: srv.URL}); err != nil {
		t.Fatalf("second EnsureHubReady: %v", err)
	}
	if len(hub.creates) != 1 || strings.Contains(out.String(), "Importing") {
		t.Errorf("second command imported again: creates=%d output:\n%s", len(hub.creates), out.String())
	}
}

// TestEnsureHubReady_AutoLinkNoBrokerDefersImport checks that with no broker
// online the agents are not imported and the link is not recorded, so the
// next command retries the import.
func TestEnsureHubReady_AutoLinkNoBrokerDefersImport(t *testing.T) {
	home := clearHubCredentialEnv(t)
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
	hub := &flipHub{projectID: "nobroker-project"}
	srv := hub.serve(t)
	globalDir := writeGlobalSettings(t, home, "project_id: nobroker-project\n")
	addLocalAgent(t, globalDir, "a1")
	out := withPromptIO(t, strings.NewReader(""), false)

	if _, err := EnsureHubReady("", EnsureHubReadyOptions{EndpointOverride: srv.URL}); err != nil {
		t.Fatalf("EnsureHubReady: %v", err)
	}
	if len(hub.creates) != 0 {
		t.Errorf("creates = %+v, want none without a broker", hub.creates)
	}
	if !strings.Contains(out.String(), "no runtime broker is online") {
		t.Errorf("output missing the no-broker warning; got:\n%s", out.String())
	}
	if linked := ProjectLinkedSetting(globalDir, true); linked != nil {
		t.Errorf("hub.linked = %v, want unset so the import is retried", *linked)
	}
}
