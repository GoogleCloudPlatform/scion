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
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// TestEnsureHubReady_SkipProjectLinkGlobal: with SkipProjectLink, an
// unlinked global project whose name matches the hub's Global project is
// neither linked nor registered (ptone/scion#5217). No prompt is shown, the
// hub gets no write request (no register, no provider update), no
// project_id or hub.projectId is written, and the context has no project.
func TestEnsureHubReady_SkipProjectLinkGlobal(t *testing.T) {
	tests := []struct {
		name string
		opts EnsureHubReadyOptions
	}{
		{name: "no terminal, no --yes", opts: EnsureHubReadyOptions{}},
		{name: "--non-interactive", opts: EnsureHubReadyOptions{AutoConfirm: true, NonInteractive: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := clearHubCredentialEnv(t)
			t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
			srv, writes, paths := contextGuardHub(t, true)
			globalDir := writeGlobalSettings(t, home, "hub:\n  enabled: true\n")
			before, err := os.ReadFile(filepath.Join(globalDir, "settings.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			out := withPromptIO(t, strings.NewReader(""), false)

			opts := tc.opts
			opts.EndpointOverride = srv.URL
			opts.ExplicitProject = true
			opts.SkipProjectLink = true
			hubCtx, err := EnsureHubReady("global", opts)
			if err != nil {
				t.Fatalf("EnsureHubReady: %v", err)
			}
			if hubCtx.ProjectID != "" {
				t.Errorf("ProjectID = %q, want empty", hubCtx.ProjectID)
			}
			if !hubCtx.IsGlobal || hubCtx.ProjectPath != globalDir {
				t.Errorf("ProjectPath = %q (global %v), want %q (global)", hubCtx.ProjectPath, hubCtx.IsGlobal, globalDir)
			}
			if n := writes.Load(); n != 0 {
				t.Errorf("hub got %d write requests (%v), want none", n, *paths)
			}
			if out.Len() != 0 {
				t.Errorf("unexpected prompt output:\n%s", out.String())
			}
			after, err := os.ReadFile(filepath.Join(globalDir, "settings.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Errorf("settings changed:\n%s\nwant unchanged:\n%s", after, before)
			}
		})
	}
}

// TestEnsureHubReady_WithoutSkipProjectLinkGlobalLinks pins the behaviour
// SkipProjectLink avoids, so TestEnsureHubReady_SkipProjectLinkGlobal
// cannot pass vacuously: without it the same setup prompts (no terminal:
// ErrNoTerminal) or links and registers (--non-interactive).
func TestEnsureHubReady_WithoutSkipProjectLinkGlobalLinks(t *testing.T) {
	home := clearHubCredentialEnv(t)
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
	srv, _, _ := contextGuardHub(t, true)
	writeGlobalSettings(t, home, "hub:\n  enabled: true\n")
	withPromptIO(t, strings.NewReader(""), false)
	_, err := EnsureHubReady("global", EnsureHubReadyOptions{EndpointOverride: srv.URL, ExplicitProject: true})
	if !errors.Is(err, ErrNoTerminal) {
		t.Errorf("error = %v, want ErrNoTerminal", err)
	}

	home = clearHubCredentialEnv(t)
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
	srv, writes, _ := contextGuardHub(t, true)
	writeGlobalSettings(t, home, "hub:\n  enabled: true\n")
	withPromptIO(t, strings.NewReader(""), false)
	if _, err := EnsureHubReady("global", EnsureHubReadyOptions{EndpointOverride: srv.URL, ExplicitProject: true, AutoConfirm: true, NonInteractive: true}); err != nil {
		t.Fatalf("EnsureHubReady: %v", err)
	}
	if writes.Load() == 0 {
		t.Error("hub got no write request, want the link to register")
	}
}

// TestEnsureHubReady_SkipProjectLinkKeepsLinkedElsewhereRefusal: on the hub
// selected with 'scion use', a project linked to another hub is refused
// with SkipProjectLink too, so the refusal never becomes an upload. This
// holds when the project carries a hub project ID and when only hub.linked
// records the link (no ID at all).
func TestEnsureHubReady_SkipProjectLinkKeepsLinkedElsewhereRefusal(t *testing.T) {
	tests := []struct {
		name     string
		settings string
	}{
		{name: "hub project ID", settings: "project_id: local-project-id\nhub:\n  enabled: true\n  projectId: linked-hub-id\n"},
		{name: "hub.linked without any ID", settings: "hub:\n  enabled: true\n  linked: true\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := clearHubCredentialEnv(t)
			t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
			srv, writes, paths := contextGuardHub(t, true)
			writeGlobalSettings(t, home, tc.settings)
			withPromptIO(t, strings.NewReader(""), false)

			_, err := EnsureHubReady("global", EnsureHubReadyOptions{
				EndpointOverride: srv.URL,
				ExplicitProject:  true,
				HubFromContext:   true,
				SkipProjectLink:  true,
			})
			if err == nil || err.Error() != wantMustBeLinked {
				t.Fatalf("error = %v, want:\n%s", err, wantMustBeLinked)
			}
			if n := writes.Load(); n != 0 {
				t.Errorf("hub got %d write requests (%v), want none", n, *paths)
			}
		})
	}
}

// TestEnsureHubReady_ExplicitMissingProjectDir: a project directory named
// with --project that does not exist is refused before anything is written
// or sent to the hub; the directory is not created (ptone/scion#5217).
func TestEnsureHubReady_ExplicitMissingProjectDir(t *testing.T) {
	home := clearHubCredentialEnv(t)
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
	srv, writes, paths := contextGuardHub(t, false)
	writeGlobalSettings(t, home, "hub:\n  enabled: true\n")
	missing := filepath.Join(t.TempDir(), "aplive")

	_, err := EnsureHubReady(missing, EnsureHubReadyOptions{EndpointOverride: srv.URL, ExplicitProject: true, AutoConfirm: true})
	if err == nil || !strings.Contains(err.Error(), "does not exist") || !strings.Contains(err.Error(), missing) {
		t.Fatalf("error = %v, want the missing-directory error naming %s", err, missing)
	}
	if _, statErr := os.Stat(missing); !os.IsNotExist(statErr) {
		t.Errorf("%s was created (stat err %v)", missing, statErr)
	}
	if n := writes.Load(); n != 0 {
		t.Errorf("hub got %d write requests (%v), want none", n, *paths)
	}

	// A split-storage marker whose external directory (under
	// ~/.scion/project-configs) is missing resolves to that missing
	// directory and is refused the same way.
	projectRoot := t.TempDir()
	marker := "project-id: 11111111-2222-3333-4444-555555555555\nproject-name: gone\nproject-slug: gone\n"
	if err := os.WriteFile(filepath.Join(projectRoot, ".scion"), []byte(marker), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = EnsureHubReady(projectRoot, EnsureHubReadyOptions{EndpointOverride: srv.URL, ExplicitProject: true, AutoConfirm: true})
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("marker with missing external dir: error = %v, want the missing-directory error", err)
	}
	if n := writes.Load(); n != 0 {
		t.Errorf("hub got %d write requests (%v), want none", n, *paths)
	}
}

// TestCheckExplicitProjectDir covers the guard's exemptions: an existing
// directory, a target not named with a flag, the global project, and a
// hub-connected context (where nothing is written or registered).
func TestCheckExplicitProjectDir(t *testing.T) {
	clearHubCredentialEnv(t)
	existing := t.TempDir()
	missing := filepath.Join(existing, "missing")

	if err := checkExplicitProjectDir("x", existing, false, true); err != nil {
		t.Errorf("existing dir: %v", err)
	}
	if err := checkExplicitProjectDir("", missing, false, false); err != nil {
		t.Errorf("not explicit: %v", err)
	}
	if err := checkExplicitProjectDir("global", missing, true, true); err != nil {
		t.Errorf("global: %v", err)
	}
	if err := checkExplicitProjectDir("missing", missing, false, true); err == nil {
		t.Error("explicit missing dir: no error")
	} else if want := `project directory "` + missing + `" (from --project "missing") does not exist`; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err, want)
	}

	t.Setenv("SCION_HUB_ENDPOINT", "http://127.0.0.1:1")
	if err := checkExplicitProjectDir("missing", missing, false, true); err != nil {
		t.Errorf("hub context: %v, want no error", err)
	}
}

// TestEnsureHubReady_SkipProjectLinkLinkedWithoutID: a project whose
// settings record hub.linked but no project ID is refused on the hub
// selected with 'scion use' without asking the hub about an empty ID. A
// lookup of "" would request the project list (GET /api/v1/projects/),
// which a hub answers with 200, so the project would count as registered
// and the refusal would turn into an upload.
func TestEnsureHubReady_SkipProjectLinkLinkedWithoutID(t *testing.T) {
	home := clearHubCredentialEnv(t)
	t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
	var projectGets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/projects"):
			projectGets.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"projects": []interface{}{}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	writeGlobalSettings(t, home, "hub:\n  enabled: true\n  linked: true\n")
	withPromptIO(t, strings.NewReader(""), false)

	_, err := EnsureHubReady("global", EnsureHubReadyOptions{
		EndpointOverride: srv.URL,
		ExplicitProject:  true,
		HubFromContext:   true,
		SkipProjectLink:  true,
	})
	if err == nil || err.Error() != wantMustBeLinked {
		t.Fatalf("error = %v, want:\n%s", err, wantMustBeLinked)
	}
	if n := projectGets.Load(); n != 0 {
		t.Errorf("hub got %d project lookups, want none for an empty ID", n)
	}
}
