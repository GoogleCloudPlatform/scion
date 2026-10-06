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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

const (
	startErrTextProjectID = "proj-3113"
	startErrTextRunID     = "run-3113"
)

// asyncCreateFailure posts an async create whose Manager.Start returns
// startErr, and returns the failed terminal report the broker sent the hub
// plus everything the broker logged.
func asyncCreateFailure(t *testing.T, name string, startErr error) (*hubclient.AgentLaunchReport, string) {
	t.Helper()
	mgr := newAsyncManager()
	mgr.setStartErr(startErr)
	srv, rtb := newAsyncTestServer(t, mgr)
	logs := &syncBuffer{}
	srv.agentLifecycleLog = slog.New(slog.NewJSONHandler(logs, nil))
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
	}

	w := postCreate(t, srv, map[string]any{
		"id": name, "name": name, "asyncLaunch": true, "launchId": "L-" + name,
		"projectId": startErrTextProjectID, "runId": startErrTextRunID,
		"launchTimeoutSeconds": 300, "config": map[string]any{"template": "claude"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("async create: status = %d, body = %s", w.Code, w.Body.String())
	}
	var failed *hubclient.AgentLaunchReport
	if !waitUntil(t, 5*time.Second, func() bool {
		for _, r := range rtb.getLaunchReports() {
			if r.Report.State == hubclient.AgentLaunchReportStateFailed {
				failed = r.Report
				return true
			}
		}
		return false
	}) {
		t.Fatalf("no failed terminal report; reports: %+v", rtb.getLaunchReports())
	}
	return failed, logs.String()
}

// syncCreateFailure posts the same create synchronously and returns the
// error body's code and message.
func syncCreateFailure(t *testing.T, name string, startErr error) (code, message string) {
	t.Helper()
	mgr := newAsyncManager()
	mgr.setStartErr(startErr)
	srv, _ := newAsyncTestServer(t, mgr)
	srv.agentLifecycleLog = slog.New(slog.NewJSONHandler(&syncBuffer{}, nil))

	w := postCreate(t, srv, map[string]any{
		"name": name, "projectId": startErrTextProjectID, "runId": startErrTextRunID,
		"config": map[string]any{"template": "claude"},
	})
	var resp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("sync create: decode error body: %v (status %d, body %s)", err, w.Code, w.Body.String())
	}
	return resp.Error.Code, resp.Error.Message
}

// assertDetailOnlyInLog checks the runtime detail stayed out of the client
// message and reached the broker's "agent start failed" log record, which
// names the agent, project and run.
func assertDetailOnlyInLog(t *testing.T, message, logs, agentID, detail string) {
	t.Helper()
	if strings.Contains(message, detail) {
		t.Errorf("client message leaked runtime detail %q: %q", detail, message)
	}
	var rec map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		var r map[string]any
		if json.Unmarshal([]byte(line), &r) == nil && r["msg"] == "runLaunch: agent start failed" {
			rec = r
			break
		}
	}
	if rec == nil {
		t.Fatalf("no \"runLaunch: agent start failed\" log record; logs: %s", logs)
	}
	if got, _ := rec["error"].(string); !strings.Contains(got, detail) {
		t.Errorf("log record error = %q, want it to carry the runtime detail %q", got, detail)
	}
	for key, want := range map[string]string{"agent_id": agentID, "project_id": startErrTextProjectID, "run_id": startErrTextRunID} {
		if rec[key] != want {
			t.Errorf("log record %s = %v, want %q", key, rec[key], want)
		}
	}
}

// TestAsyncStartError_NormalisedText covers ptone/scion#3113: an async
// start failure reports fixed text under its unchanged code, the runtime
// detail reaches only the broker log, and (where the synchronous create
// has a matching branch) the sync and async texts are identical.
func TestAsyncStartError_NormalisedText(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		detail    string
		wantCode  string
		wantMsg   string
		checkSync bool
	}{
		{
			name:      "runtime_error",
			err:       errors.New(identityLeakingRuntimeError),
			detail:    "my-actor-7f3",
			wantCode:  "runtime_error",
			wantMsg:   "Failed to create agent",
			checkSync: true,
		},
		{
			name:      "name_in_use",
			err:       fmt.Errorf("docker run scion-ctr-9e1d on node gke-pool-2: %w", agent.ErrContainerNameInUse),
			detail:    "scion-ctr-9e1d",
			wantCode:  "name_in_use",
			wantMsg:   agent.ErrContainerNameInUse.Error(),
			checkSync: true,
		},
		{
			name:     "template_not_found",
			err:      fmt.Errorf("template path /srv/broker-7/templates/secret not found or not a directory: %w", config.ErrTemplateNotFound),
			detail:   "/srv/broker-7/templates/secret",
			wantCode: "template_not_found",
			wantMsg:  "Failed to create agent: template not found",
		},
		{
			name:     "harness_config_not_found",
			err:      fmt.Errorf("harness-config %q not found (searched: /srv/broker-7/harness-configs/x): %w", "x", config.ErrHarnessConfigNotFound),
			detail:   "/srv/broker-7/harness-configs/x",
			wantCode: "template_not_found",
			wantMsg:  "Failed to create agent: harness-config not found",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			name := "agent-" + strings.ReplaceAll(c.name, "_", "-")
			report, logs := asyncCreateFailure(t, name, c.err)
			if report.ErrorCode != c.wantCode {
				t.Errorf("async error code = %q, want %q", report.ErrorCode, c.wantCode)
			}
			if report.Message != c.wantMsg {
				t.Errorf("async message = %q, want %q", report.Message, c.wantMsg)
			}
			assertDetailOnlyInLog(t, report.Message, logs, name, c.detail)

			if !c.checkSync {
				return
			}
			_, syncMsg := syncCreateFailure(t, name, c.err)
			if syncMsg != report.Message {
				t.Errorf("sync and async text differ for the same failure: sync %q, async %q", syncMsg, report.Message)
			}
		})
	}
}

// TestClassifyStartError_FixedText pins classifyStartError's message for
// each normalised branch, independent of the launch plumbing.
func TestClassifyStartError_FixedText(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{errors.New(identityLeakingRuntimeError), "Failed to create agent"},
		{fmt.Errorf("wrapped scion-ctr-9e1d: %w", agent.ErrContainerNameInUse), agent.ErrContainerNameInUse.Error()},
		{fmt.Errorf("template /srv/x not found: %w", config.ErrTemplateNotFound), "Failed to create agent: template not found"},
		{fmt.Errorf("harness-config /srv/y: %w", config.ErrHarnessConfigNotFound), "Failed to create agent: harness-config not found"},
	}
	for _, c := range cases {
		_, got := classifyStartError(t.Context(), c.err)
		if got != c.want {
			t.Errorf("classifyStartError(%q) message = %q, want %q", c.err, got, c.want)
		}
	}
}

// TestStartAndRestart_NameInUse_FixedText: the synchronous start and
// restart endpoints report the same fixed name-in-use text as create and
// the async launch, never the wrapped error's runtime detail.
func TestStartAndRestart_NameInUse_FixedText(t *testing.T) {
	for _, path := range []string{"/api/v1/agents/test-agent-1/start", "/api/v1/agents/test-agent-1/restart"} {
		t.Run(path, func(t *testing.T) {
			srv := newTestServer(t)
			mgr := srv.manager.(*mockManager)
			mgr.startErr = fmt.Errorf("docker run scion-ctr-9e1d: %w", agent.ErrContainerNameInUse)
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"runId":"run-x"}`))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusConflict {
				t.Fatalf("status %d, want 409: %s", w.Code, w.Body.String())
			}
			var resp ErrorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode error body: %v (%s)", err, w.Body.String())
			}
			if resp.Error.Code != ErrCodeConflict || resp.Error.Message != agent.ErrContainerNameInUse.Error() {
				t.Errorf("error = %+v, want code %q with the fixed name-in-use text", resp.Error, ErrCodeConflict)
			}
		})
	}
}
