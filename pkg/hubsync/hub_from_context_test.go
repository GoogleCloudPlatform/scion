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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// contextGuardHub is a fake hub on which the project is not registered. It
// counts every request that would create or change something (any method
// other than GET), and with nameMatch it lists a different project with the
// global project's name, which leads to the matching-projects choice.
func contextGuardHub(t *testing.T, nameMatch bool) (*httptest.Server, *atomic.Int32, *[]string) {
	t.Helper()
	var writes atomic.Int32
	var mu sync.Mutex
	var writePaths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			writes.Add(1)
			mu.Lock()
			writePaths = append(writePaths, r.Method+" "+r.URL.Path)
			mu.Unlock()
		}
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/register":
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"project": map[string]interface{}{"id": body["id"], "name": "global"},
				"created": true,
			})
		case r.URL.Path == "/api/v1/projects" && nameMatch:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"projects": []map[string]string{
				{"id": "other-hub-id", "name": "global", "slug": "global"},
			}})
		case r.URL.Path == "/api/v1/projects":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"projects": []interface{}{}})
		case strings.HasSuffix(r.URL.Path, "/providers"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"providers": []interface{}{}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server, &writes, &writePaths
}

// setupLinkedGlobalProject writes hub-enabled global settings for a project
// linked to a hub project (hub.projectId), moves into HOME so the global
// project is resolved, and returns the settings file path.
func setupLinkedGlobalProject(t *testing.T, home string, linked bool) string {
	t.Helper()
	globalDir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	settings := "project_id: local-project-id\nhub:\n  enabled: true\n"
	if linked {
		settings += "  projectId: linked-hub-id\n"
	}
	path := filepath.Join(globalDir, "settings.yaml")
	if err := os.WriteFile(path, []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(home)
	return path
}

const wantMustBeLinked = "project must be linked to Hub to perform this operation\n\n" +
	"Link this project: scion hub link\n" +
	"Or use local-only mode: scion --no-hub <command>"

// TestEnsureHubReady_HubFromContextDoesNotRelink: a project already linked
// to a hub (hub.projectId) that the hub selected with 'scion use' does not
// know is not linked, re-linked or registered there. EnsureHubReady returns
// the existing not-linked error before any ID or name match, auto-link,
// prompt or registration: the hub gets no write request and the settings
// file is unchanged.
func TestEnsureHubReady_HubFromContextDoesNotRelink(t *testing.T) {
	tests := []struct {
		name      string
		local     bool // dev token present: the loopback hub is the local workstation hub
		nameMatch bool
	}{
		{name: "local workstation hub: no silent auto-link", local: true},
		{name: "remote hub: no link prompt", local: false},
		{name: "name match: no matching-projects choice", local: true, nameMatch: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := clearHubCredentialEnv(t)
			if tc.local {
				t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
			}
			server, writes, paths := contextGuardHub(t, tc.nameMatch)
			settingsPath := setupLinkedGlobalProject(t, home, true)
			before, err := os.ReadFile(settingsPath)
			if err != nil {
				t.Fatal(err)
			}
			// No terminal and no -y: a prompt that ran would decline.
			out := withPromptIO(t, strings.NewReader(""), false)

			_, err = EnsureHubReady("", EnsureHubReadyOptions{
				EndpointOverride: server.URL,
				SkipSync:         true,
				AutoLinkLocal:    true,
				HubFromContext:   true,
			})
			if err == nil || err.Error() != wantMustBeLinked {
				t.Fatalf("EnsureHubReady error = %v, want %q", err, wantMustBeLinked)
			}
			if got := writes.Load(); got != 0 {
				t.Errorf("hub write requests = %d (%v), want 0", got, *paths)
			}
			if out.Len() != 0 {
				t.Errorf("prompt output = %q, want none", out.String())
			}
			after, err := os.ReadFile(settingsPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Errorf("settings changed:\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

// TestEnsureHubReady_HubFromContextControls shows what the guard changes:
// without HubFromContext the linked project is auto-linked on the local
// workstation hub, and with HubFromContext a project without hub.projectId
// is still auto-linked as before.
func TestEnsureHubReady_HubFromContextControls(t *testing.T) {
	tests := []struct {
		name           string
		hubFromContext bool
		linked         bool
	}{
		{name: "linked project, hub not from the context: auto-linked", linked: true},
		{name: "unlinked project, hub from the context: auto-linked", hubFromContext: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := clearHubCredentialEnv(t)
			t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
			server, writes, _ := contextGuardHub(t, false)
			setupLinkedGlobalProject(t, home, tc.linked)
			_ = withPromptIO(t, strings.NewReader(""), false)

			if _, err := EnsureHubReady("", EnsureHubReadyOptions{
				EndpointOverride: server.URL,
				SkipSync:         true,
				AutoLinkLocal:    true,
				HubFromContext:   tc.hubFromContext,
			}); err != nil {
				t.Fatalf("EnsureHubReady: %v", err)
			}
			if writes.Load() == 0 {
				t.Error("expected the project to be registered on the hub")
			}
		})
	}
}

// TestEnsureHubReady_RecordLinkAfterRegistration: when EnsureHubReady
// registers or links a project, RecordLink gets the hub endpoint, the ID
// the hub knows the project by and the project path. A refused
// registration records nothing.
func TestEnsureHubReady_RecordLinkAfterRegistration(t *testing.T) {
	type call struct {
		endpoint, id, path string
		isGlobal           bool
	}
	t.Run("registered: recorded once", func(t *testing.T) {
		home := clearHubCredentialEnv(t)
		t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
		server, _, _ := contextGuardHub(t, false)
		setupLinkedGlobalProject(t, home, false)
		_ = withPromptIO(t, strings.NewReader(""), false)

		var calls []call
		if _, err := EnsureHubReady("", EnsureHubReadyOptions{
			EndpointOverride: server.URL,
			SkipSync:         true,
			AutoLinkLocal:    true,
			HubFromContext:   true,
			RecordLink: func(endpoint, id, path string, isGlobal bool) {
				calls = append(calls, call{endpoint, id, path, isGlobal})
			},
		}); err != nil {
			t.Fatalf("EnsureHubReady: %v", err)
		}
		if len(calls) != 1 {
			t.Fatalf("RecordLink calls = %+v, want 1", calls)
		}
		c := calls[0]
		if c.endpoint != server.URL || c.id != "local-project-id" || !c.isGlobal {
			t.Errorf("RecordLink call = %+v", c)
		}
		want, _ := filepath.EvalSymlinks(filepath.Join(home, ".scion"))
		if got, _ := filepath.EvalSymlinks(c.path); got != want {
			t.Errorf("RecordLink path = %q, want %q", c.path, want)
		}
	})
	t.Run("guard refuses: nothing recorded", func(t *testing.T) {
		home := clearHubCredentialEnv(t)
		t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
		server, _, _ := contextGuardHub(t, false)
		setupLinkedGlobalProject(t, home, true)
		_ = withPromptIO(t, strings.NewReader(""), false)

		called := false
		_, err := EnsureHubReady("", EnsureHubReadyOptions{
			EndpointOverride: server.URL,
			SkipSync:         true,
			AutoLinkLocal:    true,
			HubFromContext:   true,
			RecordLink:       func(string, string, string, bool) { called = true },
		})
		if err == nil || err.Error() != wantMustBeLinked {
			t.Fatalf("EnsureHubReady error = %v, want %q", err, wantMustBeLinked)
		}
		if called {
			t.Error("RecordLink was called for a refused registration")
		}
	})
}
