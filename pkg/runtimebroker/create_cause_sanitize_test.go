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
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// credentialShapedValues are the values credentialShapedStartError embeds;
// none may appear in any broker log record.
var credentialShapedValues = []string{
	"sk-live-0123456789abcdefABCDEF",
	"hunter2-value",
	"pw-value-123",
	"abcdefghijklmnop0123",
	"MIIBVQIBADANBgkqhkiG9w0BAQEFAASC",
	"eyJhbGciOiJIUzI1NiJ9",
	"envvar-value-4242",
}

// credentialShapedStartError is a Manager.Start failure whose backend text
// repeats credential-shaped parts of a start spec and runs past
// maxLoggedCauseBytes.
func credentialShapedStartError() error {
	backend := "docker: Error response from daemon: failed to create task: " +
		"env FOO_API_KEY=sk-live-0123456789abcdefABCDEF " +
		`spec {"DB_PASSWORD": "hunter2-value"} ` +
		"pull https://deploy:pw-value-123@registry.example.com/img " +
		"Authorization: Bearer abcdefghijklmnop0123 " +
		`env [{"name":"GITHUB_PAT","value":"envvar-value-4242"}] ` +
		"-----BEGIN PRIVATE KEY-----\nMIIBVQIBADANBgkqhkiG9w0BAQEFAASC\n-----END PRIVATE KEY----- " +
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2lnbmF0dXJl " +
		strings.Repeat("container output line\n", 100)
	return fmt.Errorf("failed to launch container: %w", errors.New(backend))
}

// sanitizedCauseLogSites lists the log records pinned to pass a failure's
// cause and error text through sanitizeFailureCause: the records of the
// create and start failures whose client response is the fixed
// "Failed to <op>" text (synchronous create, async launch, create
// preflight, start, restart, and start context preparation), and the start
// context record for a missing local file. Each listed site needs a case in
// TestCreateFailureCause_SanitizedAtEveryLogSite.
var sanitizedCauseLogSites = []string{
	"Agent create failed",
	"runLaunch: agent start failed",
	"Agent create failed: preflight",
	"Agent start failed",
	"Agent restart failed",
	"buildStartContext failed",
	"buildStartContext failed: local file missing",
}

// assertSanitizedCauseRecord checks logs hold exactly one msg record, that
// its agent_id, runtime and profile fields equal want's, that its cause and
// error fields are sanitized and truncated, and that no record holds any
// credential-shaped value.
func assertSanitizedCauseRecord(t *testing.T, logs, msg string, want map[string]string) {
	t.Helper()
	var recs []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		var r map[string]any
		if json.Unmarshal([]byte(line), &r) == nil && r["msg"] == msg {
			recs = append(recs, r)
		}
	}
	if len(recs) != 1 {
		t.Fatalf("%d %q records, want exactly 1; logs: %s", len(recs), msg, logs)
	}
	for _, field := range []string{"agent_id", "runtime", "profile"} {
		if got, ok := recs[0][field].(string); !ok || got != want[field] {
			t.Errorf("%s = %#v, want %q", field, recs[0][field], want[field])
		}
	}
	for _, field := range []string{"cause", "error"} {
		got, ok := recs[0][field].(string)
		if !ok {
			t.Fatalf("%s = %#v, want a string", field, recs[0][field])
		}
		for _, want := range []string{"FOO_API_KEY=[REDACTED]", `"DB_PASSWORD": [REDACTED]`, "https://[REDACTED]@registry.example.com", "Bearer [REDACTED]", `"value":[REDACTED]`, "...[truncated, "} {
			if !strings.Contains(got, want) {
				t.Errorf("%s lacks %q: %q", field, want, got)
			}
		}
		if len(got) > maxLoggedCauseBytes+64 {
			t.Errorf("%s is %d bytes, want it bounded near %d", field, len(got), maxLoggedCauseBytes)
		}
	}
	for _, v := range credentialShapedValues {
		if strings.Contains(logs, v) {
			t.Errorf("logs contain %q", v)
		}
	}
}

// asyncPreflightFailure posts an async create whose Manager.Preflight
// returns preflightErr, and returns the response and the broker's logs.
func asyncPreflightFailure(t *testing.T, name string, preflightErr error) (int, string, string) {
	t.Helper()
	mgr := newAsyncManager()
	mgr.setPreflightErr(preflightErr)
	srv, _ := newAsyncTestServer(t, mgr)
	logs := &syncBuffer{}
	srv.agentLifecycleLog = slog.New(slog.NewJSONHandler(logs, nil))
	w := postCreate(t, srv, map[string]any{
		"id": name, "name": name, "asyncLaunch": true, "launchId": "L-" + name,
		"projectId": startErrTextProjectID, "runId": startErrTextRunID,
		"launchTimeoutSeconds": 300, "config": map[string]any{"template": "claude"},
	})
	return w.Code, w.Body.String(), logs.String()
}

// startOpFailure posts op ("start" or "restart") for an agent whose
// Manager.Start returns startErr, and returns the response and the logs.
func startOpFailure(t *testing.T, op string, startErr error) (int, string, string) {
	t.Helper()
	srv := newTestServer(t)
	srv.manager.(*mockManager).startErr = startErr
	logs := &syncBuffer{}
	srv.agentLifecycleLog = slog.New(slog.NewJSONHandler(logs, nil))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/"+op+"?projectId="+startErrTextProjectID,
		strings.NewReader(`{"runId":"`+startErrTextRunID+`"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w.Code, w.Body.String(), logs.String()
}

// assertStartOpSanitized checks a start or restart failing with
// credential-shaped backend text logs one sanitized msg record and answers
// with the plain failure's body.
func assertStartOpSanitized(t *testing.T, op, msg string) {
	t.Helper()
	status, body, logs := startOpFailure(t, op, credentialShapedStartError())
	_, plainBody, _ := startOpFailure(t, op, errors.New("docker run scion-ctr-1: exit status 125"))
	if status != http.StatusInternalServerError || body != plainBody {
		t.Errorf("response = %d %s, want the plain failure's body %s", status, body, plainBody)
	}
	assertSanitizedCauseRecord(t, logs, msg, map[string]string{"agent_id": "test-agent-1", "runtime": "mock", "profile": ""})
}

// TestCreateFailureCause_SanitizedAtEveryLogSite: a create, start or
// restart that fails with credential-shaped backend text logs one record
// per failure, with the agent id, runtime and profile, and with that text
// sanitized and truncated; the response the client gets is the same fixed
// body a plain failure gets.
func TestCreateFailureCause_SanitizedAtEveryLogSite(t *testing.T) {
	cases := map[string]func(t *testing.T){
		"Agent create failed": func(t *testing.T) {
			const name = "agent-sync-sanitize"
			status, body, _, logs := postSyncCreateFailure(t, name, credentialShapedStartError())
			_, plainBody, _, _ := postSyncCreateFailure(t, name, errors.New("docker run scion-ctr-1: exit status 125"))
			if status != http.StatusInternalServerError || body != plainBody {
				t.Errorf("response = %d %s, want the plain failure's body %s", status, body, plainBody)
			}
			assertSanitizedCauseRecord(t, logs, "Agent create failed", map[string]string{"agent_id": name, "runtime": "mock", "profile": ""})
		},
		"runLaunch: agent start failed": func(t *testing.T) {
			const name = "agent-async-sanitize"
			report, logs := asyncCreateFailure(t, name, credentialShapedStartError())
			if report.ErrorCode != "runtime_error" || report.Message != "Failed to create agent" {
				t.Errorf("report = %s %q, want runtime_error with the fixed text", report.ErrorCode, report.Message)
			}
			raw, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			for _, v := range credentialShapedValues {
				if strings.Contains(string(raw), v) {
					t.Errorf("report contains %q", v)
				}
			}
			assertSanitizedCauseRecord(t, logs, "runLaunch: agent start failed", map[string]string{"agent_id": name, "runtime": "mock", "profile": ""})
		},
		"Agent create failed: preflight": func(t *testing.T) {
			status, body, logs := asyncPreflightFailure(t, "agent-preflight-sanitize", credentialShapedStartError())
			_, plainBody, _ := asyncPreflightFailure(t, "agent-preflight-sanitize", errors.New("runtime not ready"))
			if status != http.StatusInternalServerError || body != plainBody {
				t.Errorf("response = %d %s, want the plain failure's body %s", status, body, plainBody)
			}
			assertSanitizedCauseRecord(t, logs, "Agent create failed: preflight", map[string]string{"agent_id": "agent-preflight-sanitize", "runtime": "mock", "profile": ""})
		},
		"Agent start failed": func(t *testing.T) {
			assertStartOpSanitized(t, "start", "Agent start failed")
		},
		"Agent restart failed": func(t *testing.T) {
			assertStartOpSanitized(t, "restart", "Agent restart failed")
		},
		"buildStartContext failed": func(t *testing.T) {
			for name, err := range map[string]error{
				"plain error":   credentialShapedStartError(),
				"runtime error": &startContextError{Status: http.StatusInternalServerError, Message: "curated", OriginalErr: credentialShapedStartError()},
				"hub error":     &startContextError{IsHubError: true, Message: "curated", OriginalErr: credentialShapedStartError()},
			} {
				t.Run(name, func(t *testing.T) {
					srv := newTestServer(t)
					logs := &syncBuffer{}
					srv.agentLifecycleLog = slog.New(slog.NewJSONHandler(logs, nil))
					w := httptest.NewRecorder()
					srv.writeStartContextError(w, err, "start agent", "agent_id", "agent-ctx-sanitize", "runtime", "docker", "profile", "default")
					if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), `"Failed to start agent"`) {
						t.Errorf("response = %d %s, want 500 with the fixed text", w.Code, w.Body.String())
					}
					assertSanitizedCauseRecord(t, logs.String(), "buildStartContext failed", map[string]string{"agent_id": "agent-ctx-sanitize", "runtime": "docker", "profile": "default"})
				})
			}
		},
		"buildStartContext failed: local file missing": func(t *testing.T) {
			srv := newTestServer(t)
			logs := &syncBuffer{}
			srv.agentLifecycleLog = slog.New(slog.NewJSONHandler(logs, nil))
			w := httptest.NewRecorder()
			err := &startContextError{IsHubError: true, Message: "curated", OriginalErr: fmt.Errorf("%w: %w", credentialShapedStartError(), fs.ErrNotExist)}
			srv.writeStartContextError(w, err, "start agent", "agent_id", "agent-ctx-missing", "runtime", "docker", "profile", "default")
			if w.Code != http.StatusUnprocessableEntity {
				t.Errorf("response = %d %s, want 422", w.Code, w.Body.String())
			}
			assertSanitizedCauseRecord(t, logs.String(), "buildStartContext failed: local file missing", map[string]string{"agent_id": "agent-ctx-missing", "runtime": "docker", "profile": "default"})
		},
	}
	for _, site := range sanitizedCauseLogSites {
		run, ok := cases[site]
		if !ok {
			t.Errorf("log site %q has no case", site)
			continue
		}
		t.Run(site, run)
	}
	if len(cases) != len(sanitizedCauseLogSites) {
		t.Errorf("%d cases for %d log sites", len(cases), len(sanitizedCauseLogSites))
	}
}
