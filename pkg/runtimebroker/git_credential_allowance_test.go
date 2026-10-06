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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAllowGitCredentials_ReachesStartFromRequestOnly pins that the
// allowance reaches Manager.Start from the create, start and restart request
// bodies, that BrokerMode is always set, and that an absent, false or null
// field means false (fail closed).
func TestAllowGitCredentials_ReachesStartFromRequestOnly(t *testing.T) {
	paths := []struct {
		name, path, base string
		status           int
	}{
		{"create", "/api/v1/agents", `"name": "cred-agent", "config": {"template": "claude"}`, http.StatusCreated},
		{"start", "/api/v1/agents/test-agent-1/start", "", http.StatusAccepted},
		{"restart", "/api/v1/agents/test-agent-1/restart", "", http.StatusAccepted},
	}
	fields := []struct {
		name  string
		field string
		want  bool
	}{
		{"absent", "", false},
		{"false", `"allowGitCredentials": false`, false},
		{"null", `"allowGitCredentials": null`, false},
		{"true", `"allowGitCredentials": true`, true},
	}
	for _, p := range paths {
		for _, f := range fields {
			t.Run(p.name+"/"+f.name, func(t *testing.T) {
				srv := newTestServer(t)
				mgr := srv.manager.(*mockManager)
				parts := []string{}
				for _, s := range []string{p.base, f.field, `"resolvedEnv": {"PLAIN": "p"}`} {
					if s != "" {
						parts = append(parts, s)
					}
				}
				body := "{" + strings.Join(parts, ", ") + "}"
				req := httptest.NewRequest(http.MethodPost, p.path, strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				srv.Handler().ServeHTTP(w, req)
				if w.Code != p.status {
					t.Fatalf("status = %d, want %d: %s", w.Code, p.status, w.Body.String())
				}
				if mgr.startCalls != 1 {
					t.Fatalf("Start calls = %d, want 1", mgr.startCalls)
				}
				if !mgr.lastStartOpts.BrokerMode {
					t.Errorf("BrokerMode = false, want true for every dispatched start")
				}
				if got := mgr.lastStartOpts.AllowGitCredentials; got != f.want {
					t.Errorf("AllowGitCredentials = %v, want %v", got, f.want)
				}
			})
		}
	}
}

// TestAllowGitCredentials_WrongTypeFailsClosed pins that a wrong-typed
// allowance ("true" as a string) never allows GitHub credentials. Create
// refuses the body with 400 and runs no start. Start and restart ignore
// body decode errors (their body is optional), so the start runs with the
// allowance left false.
func TestAllowGitCredentials_WrongTypeFailsClosed(t *testing.T) {
	paths := []struct {
		name, path, base string
		status           int
		starts           int
	}{
		{"create", "/api/v1/agents", `"name": "cred-agent", "config": {"template": "claude"}, `, http.StatusBadRequest, 0},
		{"start", "/api/v1/agents/test-agent-1/start", "", http.StatusAccepted, 1},
		{"restart", "/api/v1/agents/test-agent-1/restart", "", http.StatusAccepted, 1},
	}
	for _, p := range paths {
		t.Run(p.name, func(t *testing.T) {
			srv := newTestServer(t)
			mgr := srv.manager.(*mockManager)
			body := "{" + p.base + `"allowGitCredentials": "true"}`
			req := httptest.NewRequest(http.MethodPost, p.path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code != p.status {
				t.Fatalf("status = %d, want %d: %s", w.Code, p.status, w.Body.String())
			}
			if mgr.startCalls != p.starts {
				t.Fatalf("Start calls = %d, want %d", mgr.startCalls, p.starts)
			}
			if p.starts > 0 && mgr.lastStartOpts.AllowGitCredentials {
				t.Errorf("AllowGitCredentials = true, want false for a wrong-typed value")
			}
		})
	}
}
