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
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// registeredHub is a fake hub on which projectID is registered. It counts
// every request it receives and every register call.
func registeredHub(t *testing.T, projectID string) (srv *httptest.Server, requests, registers *atomic.Int32) {
	t.Helper()
	requests, registers = &atomic.Int32{}, &atomic.Int32{}
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/register":
			registers.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"project": map[string]string{"id": projectID, "name": "global"},
				"created": false,
			})
		case r.URL.Path == "/api/v1/projects/"+projectID:
			_ = json.NewEncoder(w).Encode(map[string]string{"id": projectID, "name": "global"})
		case r.URL.Path == "/api/v1/projects":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"projects": []interface{}{}})
		case strings.HasSuffix(r.URL.Path, "/providers"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"providers": []interface{}{}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, requests, registers
}

// unlinkGlobalProject writes the settings 'scion hub unlink' leaves behind
// on the global project: hub.linked=false, hub.projectId cleared, and
// hub.enabled untouched (true from the earlier link).
func unlinkGlobalProject(t *testing.T, globalDir string) {
	t.Helper()
	if err := config.UpdateSetting(globalDir, "hub.linked", "false", true); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateSetting(globalDir, "hub.projectId", "", true); err != nil {
		t.Fatal(err)
	}
}

// TestEnsureHubReady_UnlinkedProjectRefuses checks that an unlinked project
// is refused before any hub call, including on the local workstation hub,
// where the auto-link would otherwise link it again without asking.
func TestEnsureHubReady_UnlinkedProjectRefuses(t *testing.T) {
	const projectID = "unlinked-project-id"
	for _, tc := range []struct {
		name     string
		devToken string
	}{
		{name: "local workstation endpoint (auto-link on)", devToken: "scion_dev_test"},
		{name: "remote-style endpoint", devToken: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := clearHubCredentialEnv(t)
			t.Setenv("SCION_DEV_TOKEN", tc.devToken)
			server, requests, registers := registeredHub(t, projectID)
			setupUnregisteredGlobalProject(t, home, projectID)
			unlinkGlobalProject(t, filepath.Join(home, ".scion"))
			out := withPromptIO(t, strings.NewReader(""), false)

			hubCtx, err := EnsureHubReady("", EnsureHubReadyOptions{
				EndpointOverride: server.URL,
				SkipSync:         true,
				AutoLinkLocal:    true,
				AutoConfirm:      true,
			})
			if !errors.Is(err, ErrProjectUnlinked) {
				t.Fatalf("EnsureHubReady error = %v, want ErrProjectUnlinked", err)
			}
			if hubCtx != nil {
				t.Errorf("hubCtx = %+v, want nil", hubCtx)
			}
			if !strings.Contains(err.Error(), "scion hub link") {
				t.Errorf("error %q does not point at 'scion hub link'", err)
			}
			if n := requests.Load(); n != 0 {
				t.Errorf("hub received %d requests, want none", n)
			}
			if n := registers.Load(); n != 0 {
				t.Errorf("register calls = %d, want 0 (no silent re-link)", n)
			}
			if out.Len() != 0 {
				t.Errorf("unexpected prompt output:\n%s", out.String())
			}
		})
	}
}

// TestEnsureHubReady_UnlinkRelinkRoundTrip runs unlink -> refused ->
// link -> works on the same project.
func TestEnsureHubReady_UnlinkRelinkRoundTrip(t *testing.T) {
	const projectID = "roundtrip-project-id"
	home := clearHubCredentialEnv(t)
	server, _, _ := registeredHub(t, projectID)
	setupUnregisteredGlobalProject(t, home, projectID)
	globalDir := filepath.Join(home, ".scion")
	withPromptIO(t, strings.NewReader(""), false)
	opts := EnsureHubReadyOptions{EndpointOverride: server.URL, SkipSync: true, AutoLinkLocal: true}

	// Linked: works.
	if err := config.UpdateSetting(globalDir, "hub.linked", "true", true); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureHubReady("", opts); err != nil {
		t.Fatalf("linked project: %v", err)
	}

	// Unlinked: refused.
	unlinkGlobalProject(t, globalDir)
	if _, err := EnsureHubReady("", opts); !errors.Is(err, ErrProjectUnlinked) {
		t.Fatalf("after unlink: error = %v, want ErrProjectUnlinked", err)
	}

	// Re-linked ('scion hub link' writes hub.linked=true): works again.
	if err := config.UpdateSetting(globalDir, "hub.linked", "true", true); err != nil {
		t.Fatal(err)
	}
	hubCtx, err := EnsureHubReady("", opts)
	if err != nil {
		t.Fatalf("after re-link: %v", err)
	}
	if hubCtx == nil || hubCtx.ProjectID != projectID {
		t.Fatalf("after re-link: hubCtx = %+v, want project %s", hubCtx, projectID)
	}
}

// TestEnsureHubReady_UnlinkedSkippedInHubContainer checks that the check
// does not apply inside a hub-connected agent container.
func TestEnsureHubReady_UnlinkedSkippedInHubContainer(t *testing.T) {
	const projectID = "container-project-id"
	home := clearHubCredentialEnv(t)
	server, _, _ := registeredHub(t, projectID)
	setupUnregisteredGlobalProject(t, home, projectID)
	unlinkGlobalProject(t, filepath.Join(home, ".scion"))
	t.Setenv("SCION_HUB_ENDPOINT", server.URL)

	if _, err := EnsureHubReady("", EnsureHubReadyOptions{SkipSync: true}); errors.Is(err, ErrProjectUnlinked) {
		t.Fatalf("hub-connected container got ErrProjectUnlinked")
	}
}

func TestProjectExplicitlyUnlinked(t *testing.T) {
	write := func(t *testing.T, content string) string {
		t.Helper()
		dir := filepath.Join(t.TempDir(), ".scion")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if content != "" {
			if err := os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	for _, tc := range []struct {
		name    string
		content string
		want    bool
	}{
		{name: "no settings file", content: "", want: false},
		{name: "linked unset", content: "hub:\n  enabled: true\n", want: false},
		{name: "linked true", content: "hub:\n  linked: true\n", want: false},
		{name: "linked false", content: "hub:\n  enabled: true\n  linked: false\n", want: true},
		{name: "versioned file, linked false", content: "schema_version: \"1\"\nhub:\n  linked: false\n", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearHubCredentialEnv(t)
			if got := projectExplicitlyUnlinked(write(t, tc.content), false); got != tc.want {
				t.Errorf("projectExplicitlyUnlinked = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestProjectExplicitlyUnlinked_GlobalDoesNotLeak checks that an unlinked
// global project does not mark a project without its own hub.linked value
// as unlinked, although the merged settings inherit the global value.
func TestProjectExplicitlyUnlinked_GlobalDoesNotLeak(t *testing.T) {
	home := clearHubCredentialEnv(t)
	globalDir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte("hub:\n  enabled: true\n  linked: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	projectDir := filepath.Join(t.TempDir(), ".scion")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte("hub:\n  enabled: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if projectExplicitlyUnlinked(projectDir, false) {
		t.Error("project without its own hub.linked is reported unlinked")
	}
	if !projectExplicitlyUnlinked(globalDir, true) {
		t.Error("the unlinked global project is not reported unlinked")
	}
}

// setupSplitStorageProject creates a git repo whose .scion has a project-id
// file, so config.UpdateSetting writes the project's settings to the
// external project-configs directory (split storage). It moves into the
// repo and returns the in-repo .scion path and the external directory.
func setupSplitStorageProject(t *testing.T, home string) (scionDir, extDir string) {
	t.Helper()
	repo := filepath.Join(home, "work", "splitrepo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	scionDir = filepath.Join(repo, ".scion")
	if err := os.MkdirAll(scionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scionDir, "project-id"), []byte("0b5e6a1c-0000-4000-8000-000000000001\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	extDir = config.GetProjectConfigDir(scionDir)
	if extDir == scionDir || !strings.Contains(extDir, "project-configs") {
		t.Fatalf("expected an external config dir for split storage, got %q", extDir)
	}
	if err := config.UpdateSetting(scionDir, "hub.enabled", "true", false); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	return scionDir, extDir
}

// TestEnsureHubReady_UnlinkedSplitStorageProject checks the refusal for a
// git project with split storage, where 'scion hub unlink' writes
// hub.linked=false to the external directory rather than the in-repo .scion.
func TestEnsureHubReady_UnlinkedSplitStorageProject(t *testing.T) {
	home := clearHubCredentialEnv(t)
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
	scionDir, extDir := setupSplitStorageProject(t, home)
	server, requests, registers := registeredHub(t, "0b5e6a1c-0000-4000-8000-000000000001")
	withPromptIO(t, strings.NewReader(""), false)

	// What 'scion hub unlink' writes.
	if err := config.UpdateSetting(scionDir, "hub.linked", "false", false); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateSetting(scionDir, "hub.projectId", "", false); err != nil {
		t.Fatal(err)
	}
	if own, _ := config.LoadSettingsFromDir(scionDir); own != nil && own.Hub != nil && own.Hub.Linked != nil {
		t.Fatalf("test setup: hub.linked was written in-repo, want only in %s", extDir)
	}

	_, err := EnsureHubReady("", EnsureHubReadyOptions{
		EndpointOverride: server.URL,
		SkipSync:         true,
		AutoLinkLocal:    true,
		AutoConfirm:      true,
	})
	if !errors.Is(err, ErrProjectUnlinked) {
		t.Fatalf("EnsureHubReady error = %v, want ErrProjectUnlinked", err)
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("hub received %d requests, want none", n)
	}
	if n := registers.Load(); n != 0 {
		t.Errorf("register calls = %d, want 0 (no silent re-link)", n)
	}

	// Re-link (hub link writes hub.linked=true to the same place): works.
	if err := config.UpdateSetting(scionDir, "hub.linked", "true", false); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureHubReady("", EnsureHubReadyOptions{EndpointOverride: server.URL, SkipSync: true, AutoLinkLocal: true}); err != nil {
		t.Fatalf("after re-link: %v", err)
	}
}

// TestProjectExplicitlyUnlinked_ExternalOverridesInRepo checks that the
// external split-storage value wins over an in-repo one.
func TestProjectExplicitlyUnlinked_ExternalOverridesInRepo(t *testing.T) {
	home := clearHubCredentialEnv(t)
	scionDir, extDir := setupSplitStorageProject(t, home)

	if err := os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte("hub:\n  linked: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateSetting(scionDir, "hub.linked", "false", false); err != nil {
		t.Fatal(err)
	}
	if !projectExplicitlyUnlinked(scionDir, false) {
		t.Errorf("external hub.linked=false (in %s) must win over in-repo true", extDir)
	}
	if err := config.UpdateSetting(scionDir, "hub.linked", "true", false); err != nil {
		t.Fatal(err)
	}
	if projectExplicitlyUnlinked(scionDir, false) {
		t.Error("external hub.linked=true must clear the refusal")
	}
}

// registerTestHubCtx returns a HubContext for registerProject against the
// fake hub, for a non-global project in projectDir.
func registerTestHubCtx(t *testing.T, projectDir, projectID string) *HubContext {
	t.Helper()
	server, _, _ := registeredHub(t, projectID)
	client, err := hubclient.New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &HubContext{Client: client, Endpoint: server.URL, ProjectID: projectID, ProjectPath: projectDir}
}

// TestRegisterProject_RecordsLink checks that registration records
// hub.linked=true, as 'scion hub link' does, so 'scion hub unlink' works
// for projects linked by the preflight.
func TestRegisterProject_RecordsLink(t *testing.T) {
	clearHubCredentialEnv(t)
	projectDir := filepath.Join(t.TempDir(), "proj", ".scion")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	withPromptIO(t, strings.NewReader(""), false)
	hubCtx := registerTestHubCtx(t, projectDir, "register-link-id")

	if err := registerProject(context.Background(), hubCtx, "proj", false); err != nil {
		t.Fatalf("registerProject: %v", err)
	}
	linked := ProjectLinkedSetting(projectDir, false)
	if linked == nil || !*linked {
		t.Fatalf("hub.linked = %v, want true", linked)
	}
}

// TestRegisterProject_LinkWriteFailureWarns checks that a failure to record
// hub.linked is a warning: registration still succeeds.
func TestRegisterProject_LinkWriteFailureWarns(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	clearHubCredentialEnv(t)
	projectDir := filepath.Join(t.TempDir(), "proj", ".scion")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	out := withPromptIO(t, strings.NewReader(""), false)
	hubCtx := registerTestHubCtx(t, projectDir, "register-fail-id")
	// A read-only project directory makes the settings write fail.
	if err := os.Chmod(projectDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(projectDir, 0o755) })

	if err := registerProject(context.Background(), hubCtx, "proj", false); err != nil {
		t.Fatalf("registerProject must succeed when only the linked write fails: %v", err)
	}
	if !strings.Contains(out.String(), "Warning: failed to save linked state") {
		t.Errorf("missing warning; output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Linked to existing project") {
		t.Errorf("registration output missing; output:\n%s", out.String())
	}
}
