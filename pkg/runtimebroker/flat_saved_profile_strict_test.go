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
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// trackedRunID returns the run ID recorded on the start tracked by ctx (the
// context the handler passed to Manager.Start), or "" when there is none.
func trackedRunID(srv *Server, ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	e, _ := ctx.Value(trackedStartCtxKey{}).(*trackedStart)
	if e == nil {
		return ""
	}
	srv.startsInFlight.mu.Lock()
	defer srv.startsInFlight.mu.Unlock()
	return e.runID
}

// TestFlatInstance_UnresolvableSavedProfileNeverReturns503: strict
// saved-profile resolution (ptone/scion#2709) refuses with a retryable 503
// when an existing agent's saved profile cannot be resolved. A flat instance
// never consults saved profiles, so an existing agent carrying an
// unresolvable saved profile (and settings that fail to load) still starts
// and restarts on the flat instance's single manager, and the accepted start
// is run-tracked with the hub run ID (ptone/scion#2550). A legacy Runtime
// Broker in the same situation still gets the 503.
func TestFlatInstance_UnresolvableSavedProfileNeverReturns503(t *testing.T) {
	for _, path := range []string{"/api/v1/agents/test-agent-1/start", flatRestartPath} {
		t.Run("flat "+path, func(t *testing.T) {
			f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
			projectDir, err := config.GetResolvedProjectDir("")
			if err != nil {
				t.Fatal(err)
			}
			writeFlatFixtureFile(t, filepath.Join(config.GetAgentHomePath(projectDir, "test-agent-1"), "agent-info.json"),
				`{"name":"test-agent-1","profile":"vanished"}`)
			f.srv.loadSettings = func(string) (*config.VersionedSettings, []string, error) {
				return nil, nil, errors.New("settings unavailable")
			}
			w := serveFlat(f.srv, http.MethodPost, path,
				`{"runId":"run-flat-strict","expectedRuntimeTargetId":"`+f.identity.RuntimeTarget.ID+`"}`)
			if w.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202 (never 503): %s", w.Code, w.Body.String())
			}
			f.mgr.mu.Lock()
			starts, profile, runID, startCtx := f.mgr.startCalls, f.mgr.lastStartOpts.Profile, f.mgr.lastStartOpts.RunID, f.mgr.lastStartCtx
			f.mgr.mu.Unlock()
			if starts != 1 || profile != "" || runID != "run-flat-strict" {
				t.Fatalf("starts=%d profile=%q runID=%q, want one start on the flat manager with no profile and the hub run ID",
					starts, profile, runID)
			}
			if got := trackedRunID(f.srv, startCtx); got != "run-flat-strict" {
				t.Fatalf("tracked start run ID = %q, want run-flat-strict (an accepted start is run-tracked)", got)
			}
		})
	}

	// A refused start never reaches the runtime and leaves no tracked run
	// after the request returns. That the refusal precedes setRunID is
	// guaranteed by the handler's code order; the tracked entry closes when
	// the handler returns, so this check cannot observe the order itself.
	t.Run("flat refused start is not started", func(t *testing.T) {
		f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
		w := serveFlat(f.srv, http.MethodPost, "/api/v1/agents/test-agent-1/start?runId=run-refused",
			`{"runId":"run-refused","expectedRuntimeTargetId":"another-target"}`)
		expectFlatRefusal(t, w, http.StatusConflict, ErrCodeRuntimeTargetMismatch)
		if mgrStartCalls(f) != 0 {
			t.Fatal("a refused start must not reach the runtime")
		}
		if f.srv.startsInFlight.hasRun(launchKey{Slug: "test-agent-1"}, "run-refused") {
			t.Fatal("a refused start must not leave a tracked run")
		}
	})

	t.Run("legacy start still refuses with 503", func(t *testing.T) {
		f := newLifecycleFixture(t)
		const name = "legacy-strict-agent"
		writeSavedAgentProfile(t, f.projectPath, name, "vanished")
		w := lifecyclePost(t, f.srv, "/api/v1/agents/"+name+"/start", map[string]any{"projectPath": f.projectPath})
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503: %s", w.Code, w.Body.String())
		}
		if e := decodeFlatError(t, w); e.Code != ErrCodeRuntimeUnavailable {
			t.Fatalf("code = %q, want %q", e.Code, ErrCodeRuntimeUnavailable)
		}
		if f.defaultMgr.StartCalls() != 0 {
			t.Fatalf("legacy default runtime started %d times", f.defaultMgr.StartCalls())
		}
	})
}
