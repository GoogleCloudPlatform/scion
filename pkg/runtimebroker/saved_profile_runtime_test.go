// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package runtimebroker

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// These tests pin that a start or restart of an existing agent whose saved
// profile cannot be resolved fails with a retryable 503 instead of running
// the agent on the broker's default runtime, while a start that names no
// profile keeps the default (ptone/scion#2709).

// stubRuntimeSettings replaces the settings resolveManagerForOptsStrict
// reads for the duration of the test.
func stubRuntimeSettings(t *testing.T, fn func(string) (*config.VersionedSettings, []string, error)) {
	t.Helper()
	orig := loadRuntimeSettings
	loadRuntimeSettings = fn
	t.Cleanup(func() { loadRuntimeSettings = orig })
}

func TestStartAgent_UnresolvableSavedProfileReturns503(t *testing.T) {
	cases := []struct {
		name     string
		settings func(string) (*config.VersionedSettings, []string, error)
		wantMsg  string
	}{
		{name: "profile missing", wantMsg: "not found"},
		{
			name: "settings load fails",
			settings: func(string) (*config.VersionedSettings, []string, error) {
				return nil, nil, errors.New("malformed settings")
			},
			wantMsg: "malformed settings",
		},
		{
			name: "no settings",
			settings: func(string) (*config.VersionedSettings, []string, error) {
				return nil, nil, nil
			},
			wantMsg: "no project settings found",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLifecycleFixture(t)
			const name = "saved-profile-agent"
			writeSavedAgentProfile(t, f.projectPath, name, "vanished")
			if tc.settings != nil {
				stubRuntimeSettings(t, tc.settings)
			}

			w := lifecyclePost(t, f.srv, "/api/v1/agents/"+name+"/start", map[string]any{
				"projectPath": f.projectPath,
			})
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusServiceUnavailable, w.Body.String())
			}
			if got := w.Header().Get("Retry-After"); got != "30" {
				t.Errorf("Retry-After = %q, want 30", got)
			}
			body := w.Body.String()
			if !strings.Contains(body, "vanished") || !strings.Contains(body, tc.wantMsg) {
				t.Errorf("body does not name the profile and cause %q: %s", tc.wantMsg, body)
			}
			if f.defaultMgr.StartCalls() != 0 {
				t.Errorf("default runtime Start calls = %d, want 0", f.defaultMgr.StartCalls())
			}
			if runs, _ := f.k8sRun(); runs != 0 {
				t.Errorf("kubernetes runtime runs = %d, want 0", runs)
			}
		})
	}
}

// TestRestartAgent_UnresolvableSavedProfileReturns503: the restart fails
// before its stop, so the running agent is left untouched.
func TestRestartAgent_UnresolvableSavedProfileReturns503(t *testing.T) {
	f := newLifecycleFixture(t)
	const name = "restart-saved-profile"
	writeSavedAgentProfile(t, f.projectPath, name, "vanished")
	f.defaultMgr.agents = append(f.defaultMgr.agents, lifecycleAgent(name, f.projectPath, ""))

	w := lifecyclePost(t, f.srv, "/api/v1/agents/"+name+"/restart", map[string]any{})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusServiceUnavailable, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "vanished") {
		t.Errorf("body does not name the profile: %s", w.Body.String())
	}
	if f.defaultMgr.stopCalls != 0 || f.defaultMgr.StartCalls() != 0 {
		t.Errorf("default runtime used: stop=%d start=%d", f.defaultMgr.stopCalls, f.defaultMgr.StartCalls())
	}
}

// TestStartAgent_NoSavedProfileKeepsDefault: a start that names no profile
// still falls back to the default runtime, including when settings cannot
// be loaded.
func TestStartAgent_NoSavedProfileKeepsDefault(t *testing.T) {
	cases := []struct {
		name     string
		settings func(string) (*config.VersionedSettings, []string, error)
	}{
		{name: "settings resolve"},
		{
			name: "settings load fails",
			settings: func(string) (*config.VersionedSettings, []string, error) {
				return nil, nil, errors.New("malformed settings")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLifecycleFixture(t)
			if tc.settings != nil {
				stubRuntimeSettings(t, tc.settings)
			}
			w := lifecyclePost(t, f.srv, "/api/v1/agents/fresh-agent/start", map[string]any{
				"projectPath": f.projectPath,
			})
			if w.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusAccepted, w.Body.String())
			}
			if f.defaultMgr.StartCalls() != 1 {
				t.Errorf("default runtime Start calls = %d, want 1", f.defaultMgr.StartCalls())
			}
		})
	}
}

// TestStartAgent_ResolvableSavedProfileUsesItsRuntime: a saved profile the
// settings define still starts on that profile's runtime.
func TestStartAgent_ResolvableSavedProfileUsesItsRuntime(t *testing.T) {
	f := newLifecycleFixture(t)
	const name = "k8s-saved-agent"
	writeSavedAgentProfile(t, f.projectPath, name, lifecycleK8sProfile)

	w := lifecyclePost(t, f.srv, "/api/v1/agents/"+name+"/start", map[string]any{
		"projectPath": f.projectPath,
		"hubEndpoint": lifecycleHubEndpoint,
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusAccepted, w.Body.String())
	}
	if runs, _ := f.k8sRun(); runs != 1 {
		t.Errorf("kubernetes runtime runs = %d, want 1", runs)
	}
	if f.defaultMgr.StartCalls() != 0 {
		t.Errorf("default runtime Start calls = %d, want 0", f.defaultMgr.StartCalls())
	}
}
