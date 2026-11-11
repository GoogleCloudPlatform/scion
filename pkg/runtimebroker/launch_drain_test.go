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
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

func registryEmpty(srv *Server) bool {
	srv.launchRegistry.mu.Lock()
	defer srv.launchRegistry.mu.Unlock()
	return len(srv.launchRegistry.records) == 0
}

// TestRunLaunch_ShutdownReportsBrokerRestarted is C-T4 (C-AC3): a shutdown
// drain during Manager.Start is not a local cancel. The launch reports
// failed{broker_restarted} and cleans up on the confirming answer; with
// the hub unreachable, the drain returns within its bound and the terminal
// stays journalled for the next boot.
func TestRunLaunch_ShutdownReportsBrokerRestarted(t *testing.T) {
	t.Run("delivered", func(t *testing.T) {
		mgr := newAsyncManager()
		mgr.startBlock = make(chan struct{})
		srv, rtb := newAsyncTestServer(t, mgr)
		rtb.launchReportFunc = appliedReports
		w := postCreate(t, srv, map[string]any{
			"name": "agent-sd", "asyncLaunch": true, "launchId": "L-sd",
			"launchTimeoutSeconds": 300, "config": map[string]any{"template": "claude"},
		})
		if w.Code != http.StatusCreated {
			t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
		}
		if !waitUntil(t, 2*time.Second, func() bool { return mgr.StartCallCount() == 1 }) {
			t.Fatal("Start never called")
		}
		path := filepath.Join(srv.launchJournal.dir(), "L-sd.json")
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("launch not journalled: %v", err)
		}

		srv.DrainLaunches(context.Background(), 5*time.Second)

		reports := terminalReports(rtb)
		if len(reports) != 1 {
			t.Fatalf("expected one terminal report, got %d", len(reports))
		}
		r := reports[0]
		if r.State != hubclient.AgentLaunchReportStateFailed || r.ErrorCode != api.LaunchCodeBrokerRestarted || r.InstanceID != srv.launchInstanceID {
			t.Fatalf("unexpected shutdown report: %+v", r)
		}
		if mgr.CleanupCallCount() != 1 {
			t.Fatalf("expected cleanup on the confirming answer, got %d", mgr.CleanupCallCount())
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("a delivered launch's record was not removed: %v", err)
		}
	})

	t.Run("hub unreachable", func(t *testing.T) {
		mgr := newAsyncManager()
		mgr.startBlock = make(chan struct{})
		srv, rtb := newAsyncTestServer(t, mgr)
		srv.launchTimingOverride = fastLaunchTimings(fastTestKeepaliveInterval)
		rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
			if req.State == hubclient.AgentLaunchReportStateFailed || req.State == hubclient.AgentLaunchReportStateSucceeded {
				return nil, errors.New("simulated unreachable")
			}
			return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
		}
		w := postCreate(t, srv, map[string]any{
			"name": "agent-su", "asyncLaunch": true, "launchId": "L-su",
			"launchTimeoutSeconds": 300, "config": map[string]any{"template": "claude"},
		})
		if w.Code != http.StatusCreated {
			t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
		}
		if !waitUntil(t, 2*time.Second, func() bool { return mgr.StartCallCount() == 1 }) {
			t.Fatal("Start never called")
		}

		start := time.Now()
		srv.DrainLaunches(context.Background(), 300*time.Millisecond)
		if d := time.Since(start); d > 3*time.Second {
			t.Fatalf("drain took %v, past its bound", d)
		}
		if !waitUntil(t, 5*time.Second, func() bool { return registryEmpty(srv) }) {
			t.Fatal("the launch did not end after the drain")
		}
		jr := readJournalFileT(t, filepath.Join(srv.launchJournal.dir(), "L-su.json"))
		if jr.State != "terminal_pending" || jr.Terminal == nil || jr.Terminal.ErrorCode != api.LaunchCodeBrokerRestarted {
			t.Fatalf("expected a pending broker_restarted terminal left for the next boot, got %+v", jr)
		}
		if mgr.CleanupCallCount() != 0 {
			t.Fatal("cleanup ran without a confirming answer")
		}
	})
}

// TestBrokerDraining_RefusesCreateStartRestart is C-T12 (C-AC3c): once a
// drain has begun, create, start and restart answer 503 broker_draining
// with Retry-After before any tracker entry, registration or launch; stop
// is still served.
func TestBrokerDraining_RefusesCreateStartRestart(t *testing.T) {
	mgr := newAsyncManager()
	srv, _ := newAsyncTestServer(t, mgr)
	srv.DrainLaunches(context.Background(), time.Second)

	assertDraining := func(t *testing.T, w *httptest.ResponseRecorder) {
		t.Helper()
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503; body = %s", w.Code, w.Body.String())
		}
		if got := w.Header().Get("Retry-After"); got != "5" {
			t.Fatalf("Retry-After = %q, want 5", got)
		}
		var resp ErrorResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Error.Code != api.BrokerErrCodeBrokerDraining {
			t.Fatalf("error code = %q, want %q", resp.Error.Code, api.BrokerErrCodeBrokerDraining)
		}
	}
	serve := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		return w
	}

	t.Run("create sync and async", func(t *testing.T) {
		assertDraining(t, postCreate(t, srv, map[string]any{"name": "agent-d1", "config": map[string]any{"template": "claude"}}))
		assertDraining(t, postCreate(t, srv, map[string]any{
			"name": "agent-d2", "requestId": "req-d2", "asyncLaunch": true, "launchId": "L-d2",
			"launchTimeoutSeconds": 300, "config": map[string]any{"template": "claude"},
		}))
		srv.dispatchAttemptsMu.Lock()
		_, hasAttempt := srv.dispatchAttempts["req-d2"]
		srv.dispatchAttemptsMu.Unlock()
		if hasAttempt {
			t.Fatal("a refused create left a dispatch attempt")
		}
	})
	t.Run("start", func(t *testing.T) {
		assertDraining(t, serve(http.MethodPost, "/api/v1/agents/agent-d3/start", `{}`))
	})
	t.Run("restart", func(t *testing.T) {
		assertDraining(t, serve(http.MethodPost, "/api/v1/agents/agent-d4/restart", `{}`))
	})
	if mgr.StartCallCount() != 0 || mgr.PreflightCallCount() != 0 {
		t.Fatal("a refused request reached the manager")
	}
	if len(srv.startsInFlight.keys()) != 0 {
		t.Fatalf("a refused request left a tracker entry: %v", srv.startsInFlight.keys())
	}
	if !registryEmpty(srv) {
		t.Fatal("a refused request left a registry record")
	}
	if _, err := os.Stat(srv.launchJournal.dir()); !os.IsNotExist(err) {
		t.Fatalf("a refused request journalled something: %v", err)
	}
	t.Run("stop still served", func(t *testing.T) {
		w := serve(http.MethodPost, "/api/v1/agents/agent-d3/stop", `{}`)
		if w.Code == http.StatusServiceUnavailable {
			t.Fatalf("stop refused while draining: %s", w.Body.String())
		}
	})
}

// TestBrokerDraining_RefusesAsyncCreateInAdmission covers a drain that
// begins after createAgent's own check, while the create is in admission:
// beginAsyncLaunch refuses with broker_draining before accepting, so the
// launch neither starts nor is journalled.
func TestBrokerDraining_RefusesAsyncCreateInAdmission(t *testing.T) {
	mgr := newAsyncManager()
	srv, rtb := newAsyncTestServer(t, mgr)
	testHookCreateBeforeBuildStartContext = func(CreateAgentRequest) {
		srv.DrainLaunches(context.Background(), time.Second)
	}
	t.Cleanup(func() { testHookCreateBeforeBuildStartContext = nil })

	w := postCreate(t, srv, map[string]any{
		"name": "agent-da", "asyncLaunch": true, "launchId": "L-da",
		"launchTimeoutSeconds": 300, "config": map[string]any{"template": "claude"},
	})
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), api.BrokerErrCodeBrokerDraining) {
		t.Fatalf("status = %d, body = %s; want 503 broker_draining", w.Code, w.Body.String())
	}
	if mgr.StartCallCount() != 0 || len(rtb.getLaunchReports()) != 0 {
		t.Fatal("a refused launch started or reported")
	}
	if _, err := os.Stat(filepath.Join(srv.launchJournal.dir(), "L-da.json")); !os.IsNotExist(err) {
		t.Fatalf("a refused launch was journalled: %v", err)
	}
	if !registryEmpty(srv) {
		t.Fatal("a refused launch left a registry record")
	}
}
