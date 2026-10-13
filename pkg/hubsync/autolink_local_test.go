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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
)

// clearHubCredentialEnv isolates a test from hub-related variables the
// container may export, and points HOME at a fresh directory.
func clearHubCredentialEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, name := range []string{
		"SCION_HUB_ENDPOINT", "SCION_HUB_URL", projectkeys.EnvProjectID,
		"SCION_AGENT_ID", "SCION_HUB_TOKEN", "SCION_AUTH_TOKEN",
		"SCION_DEV_TOKEN", "SCION_DEV_TOKEN_FILE",
	} {
		t.Setenv(name, "")
	}
	return home
}

func TestIsLocalWorkstationEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		devToken string
		hubToken string
		want     bool
	}{
		{name: "loopback IPv4 with dev token", endpoint: "http://127.0.0.1:8080", devToken: "scion_dev_abc", want: true},
		{name: "localhost with dev token", endpoint: "http://localhost:8080", devToken: "scion_dev_abc", want: true},
		{name: "loopback IPv6 with dev token", endpoint: "http://[::1]:8080", devToken: "scion_dev_abc", want: true},
		{name: "remote with dev token", endpoint: "https://hub.example.com", devToken: "scion_dev_abc", want: false},
		{name: "loopback with non-dev token", endpoint: "http://127.0.0.1:8080", devToken: "scion_dev_abc", hubToken: "scion_pat_xyz", want: false},
		{name: "loopback without any dev token", endpoint: "http://127.0.0.1:8080", want: false},
		{name: "empty endpoint", endpoint: "", devToken: "scion_dev_abc", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearHubCredentialEnv(t)
			t.Setenv("SCION_DEV_TOKEN", tc.devToken)
			t.Setenv("SCION_HUB_TOKEN", tc.hubToken)
			if got := IsLocalWorkstationEndpoint(tc.endpoint); got != tc.want {
				t.Errorf("IsLocalWorkstationEndpoint(%q) = %v, want %v", tc.endpoint, got, tc.want)
			}
		})
	}
}

func TestIsLocalWorkstationEndpoint_DevTokenFile(t *testing.T) {
	home := clearHubCredentialEnv(t)
	if err := os.MkdirAll(filepath.Join(home, ".scion"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".scion", "dev-token"), []byte("scion_dev_file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !IsLocalWorkstationEndpoint("http://127.0.0.1:8080") {
		t.Error("IsLocalWorkstationEndpoint = false with ~/.scion/dev-token present, want true")
	}
}

// unregisteredHub is a fake hub on loopback where the project is not
// registered and no project matches by name. It counts register calls.
func unregisteredHub(t *testing.T, projectID string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var registers atomic.Int32
	var registered atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/register":
			registers.Add(1)
			registered.Store(true)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"project": map[string]string{"id": projectID, "name": "repo"},
				"created": true,
			})
		case r.URL.Path == "/api/v1/projects/"+projectID && registered.Load():
			_ = json.NewEncoder(w).Encode(map[string]string{"id": projectID, "name": "repo"})
		case r.URL.Path == "/api/v1/projects":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"projects": []interface{}{}})
		case strings.HasSuffix(r.URL.Path, "/providers"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"providers": []interface{}{}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server, &registers
}

// setupUnregisteredGlobalProject writes hub-enabled global settings for
// projectID and moves into HOME so the global project is resolved.
func setupUnregisteredGlobalProject(t *testing.T, home, projectID string) {
	t.Helper()
	globalDir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	settings := fmt.Sprintf("project_id: %s\nhub:\n  enabled: true\n", projectID)
	if err := os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(home)
}

func TestEnsureHubReady_AutoLinkLocal(t *testing.T) {
	const projectID = "autolink-project-id"
	tests := []struct {
		name          string
		autoLinkLocal bool
		hubToken      string
		wantRegisters int32
		wantPrompt    bool
	}{
		{name: "local workstation hub links without prompting", autoLinkLocal: true, wantRegisters: 1},
		{name: "option off keeps the prompt", autoLinkLocal: false, wantPrompt: true},
		{name: "non-dev credential on loopback keeps the prompt", autoLinkLocal: true, hubToken: "bearer-xyz", wantPrompt: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := clearHubCredentialEnv(t)
			t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
			t.Setenv("SCION_HUB_TOKEN", tc.hubToken)
			server, registers := unregisteredHub(t, projectID)
			setupUnregisteredGlobalProject(t, home, projectID)
			// No terminal and no -y: if the link prompt runs, it declines.
			out := withPromptIO(t, strings.NewReader(""), false)

			hubCtx, err := EnsureHubReady("", EnsureHubReadyOptions{
				EndpointOverride: server.URL,
				SkipSync:         true,
				AutoLinkLocal:    tc.autoLinkLocal,
			})

			prompted := strings.Contains(out.String(), "Link project with Hub?")
			if prompted != tc.wantPrompt {
				t.Errorf("link prompt shown = %v, want %v; output:\n%s", prompted, tc.wantPrompt, out.String())
			}
			if got := registers.Load(); got != tc.wantRegisters {
				t.Errorf("register calls = %d, want %d", got, tc.wantRegisters)
			}
			if tc.wantPrompt {
				if err == nil || !strings.Contains(err.Error(), "must be linked") {
					t.Errorf("EnsureHubReady error = %v, want the not-linked error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("EnsureHubReady: %v", err)
			}
			if hubCtx == nil || hubCtx.ProjectID != projectID {
				t.Fatalf("hubCtx = %+v, want project %s", hubCtx, projectID)
			}
			if want := "Linking project 'global' to the local hub at " + server.URL; !strings.Contains(out.String(), want) {
				t.Errorf("output missing %q; got:\n%s", want, out.String())
			}
		})
	}
}
