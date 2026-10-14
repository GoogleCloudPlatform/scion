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
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// Store a fixed reason phrase for skill resolver errors; full detail goes
// to server-side logs (ptone/scion#5061).

const (
	resolverTestSkill      = "gh://example-org/example-skills/my-skill@main"
	resolverTestBody       = "ARBITRARY-UPSTREAM-BODY <html>see https://docs.example.test/doc?token=qv-1</html>"
	resolverTestQueryValue = "qv-1"
	resolverLogMsg         = skillFailureLogMsg
)

func resolverTestError() *agent.SkillResolutionError {
	return &agent.SkillResolutionError{
		URI:     resolverTestSkill,
		Code:    agent.SkillErrCodeResolveFailed,
		Message: "GitHub API error (422) while resolving commit: " + resolverTestBody,
	}
}

// logRecord returns the first JSON log record in logs whose msg is msg.
func logRecord(t *testing.T, logs, msg string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		var r map[string]any
		if json.Unmarshal([]byte(line), &r) == nil && r["msg"] == msg {
			return r
		}
	}
	t.Fatalf("no %q log record; logs: %s", msg, logs)
	return nil
}

// assertResolverLogRecord checks that exactly one server-side record, the
// skill failure record, carries the full resolver text, under correlation
// id ref and with URL query values redacted. When generalMsg is set, the
// general failure record must carry the same correlation id.
func assertResolverLogRecord(t *testing.T, logs, generalMsg, ref string) {
	t.Helper()
	rec := logRecord(t, logs, resolverLogMsg)
	if rec["correlation_id"] != ref {
		t.Errorf("log correlation_id = %v, want %q", rec["correlation_id"], ref)
	}
	got, _ := rec["error"].(string)
	if !strings.Contains(got, "ARBITRARY-UPSTREAM-BODY") {
		t.Errorf("log error = %q, want the full resolver text", got)
	}
	if n := strings.Count(logs, "ARBITRARY-UPSTREAM-BODY"); n != 1 {
		t.Errorf("the full resolver text is in %d log records, want 1: %s", n, logs)
	}
	if strings.Contains(logs, resolverTestQueryValue) {
		t.Errorf("logs carry the URL query value %q: %s", resolverTestQueryValue, logs)
	}
	if generalMsg != "" {
		general := logRecord(t, logs, generalMsg)
		if general["correlation_id"] != ref {
			t.Errorf("%q record correlation_id = %v, want %q", generalMsg, general["correlation_id"], ref)
		}
		if cause, ok := general["cause"]; ok {
			t.Errorf("%q record carries a runtime cause %v; the full text belongs only in the skill failure record", generalMsg, cause)
		}
	}
}

func TestAsyncLaunch_SkillResolverError_StoresFixedReasonPhrase(t *testing.T) {
	report, logs := asyncCreateFailure(t, "skill-resolver-async", fmt.Errorf("provision: %w", resolverTestError()))

	if report.ErrorCode != ErrCodeSkillResolution {
		t.Fatalf("error code = %q, want %q", report.ErrorCode, ErrCodeSkillResolution)
	}
	// report.Message is what the hub stores as the phase message and
	// launch.error.
	want := `required skill "` + resolverTestSkill + `" could not be resolved: resolver error (ref ` + startErrTextRunID + `)`
	if report.Message != want {
		t.Errorf("stored message = %q, want %q", report.Message, want)
	}
	assertResolverLogRecord(t, logs, "runLaunch: agent start failed", startErrTextRunID)
}

func TestSyncCreate_SkillResolverError_FixedReasonPhrase(t *testing.T) {
	mgr := newAsyncManager()
	mgr.setStartErr(resolverTestError())
	srv, _ := newAsyncTestServer(t, mgr)
	logs := &syncBuffer{}
	srv.agentLifecycleLog = slog.New(slog.NewJSONHandler(logs, nil))

	w := postCreate(t, srv, map[string]any{
		"name": "skill-resolver-sync", "projectId": startErrTextProjectID, "runId": startErrTextRunID,
		"config": map[string]any{"template": "claude"},
	})
	var resp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode error body: %v (status %d, body %s)", err, w.Code, w.Body.String())
	}
	want := `Failed to provision agent: required skill "` + resolverTestSkill + `" could not be resolved: resolver error (ref ` + startErrTextRunID + `)`
	if resp.Error.Message != want {
		t.Errorf("message = %q, want %q", resp.Error.Message, want)
	}
	if strings.Contains(w.Body.String(), "ARBITRARY-UPSTREAM-BODY") {
		t.Errorf("response body carries resolver text: %s", w.Body.String())
	}
	assertResolverLogRecord(t, logs.String(), "Agent create failed", startErrTextRunID)
}

// TestStartRestart_SkillResolverError_FixedReasonPhrase covers start and
// restart: the correlation id in the response is the one in the log
// record.
func TestStartRestart_SkillResolverError_FixedReasonPhrase(t *testing.T) {
	refPattern := regexp.MustCompile(`^Failed to provision agent: required skill "` + regexp.QuoteMeta(resolverTestSkill) +
		`" could not be resolved: resolver error \(ref ([^()]+)\)$`)
	for _, tc := range []struct{ op, generalMsg string }{
		{"start", "Agent start failed"},
		{"restart", "Agent restart failed"},
	} {
		op := tc.op
		t.Run(op, func(t *testing.T) {
			srv := newTestServer(t)
			logs := &syncBuffer{}
			srv.agentLifecycleLog = slog.New(slog.NewJSONHandler(logs, nil))
			mgr := srv.manager.(*mockManager)
			mgr.startErr = fmt.Errorf("provision: %w", resolverTestError())

			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/"+op, nil)
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)

			var resp ErrorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode error body: %v (status %d, body %s)", err, w.Code, w.Body.String())
			}
			m := refPattern.FindStringSubmatch(resp.Error.Message)
			if m == nil {
				t.Fatalf("message = %q, want it to match %s", resp.Error.Message, refPattern)
			}
			if strings.Contains(w.Body.String(), "ARBITRARY-UPSTREAM-BODY") {
				t.Errorf("response body carries resolver text: %s", w.Body.String())
			}
			assertResolverLogRecord(t, logs.String(), tc.generalMsg, m[1])
		})
	}
}

// TestAsyncLaunch_SkillInstallError_TextUnchanged pins the install failure
// text: its reason is already public, so it is stored as before.
func TestAsyncLaunch_SkillInstallError_TextUnchanged(t *testing.T) {
	installErr := &agent.SkillResolutionError{
		URI: resolverTestSkill, Code: agent.SkillErrCodeInstallFailed,
		Message: "download failed with status 404",
	}
	report, _ := asyncCreateFailure(t, "skill-install-async", installErr)
	want := `required skill "` + resolverTestSkill + `" could not be installed: download failed with status 404`
	if report.Message != want {
		t.Errorf("stored message = %q, want %q", report.Message, want)
	}
}

// TestAsyncLaunch_SkillResolverError_HTTPRefRedacted checks an http(s)
// skill reference is stored without its userinfo or query string.
func TestAsyncLaunch_SkillResolverError_HTTPRefRedacted(t *testing.T) {
	skillErr := &agent.SkillResolutionError{
		URI:     "https://user:pw-1@skills.example.test/packs/my-skill.tar.gz?sig=qv-2&token=qv-3",
		Code:    agent.SkillErrCodeNotFound,
		Message: "not found: " + resolverTestBody,
	}
	report, logs := asyncCreateFailure(t, "skill-resolver-http-ref", skillErr)
	want := `required skill "https://skills.example.test/packs/my-skill.tar.gz" could not be resolved: skill not found (ref ` + startErrTextRunID + `)`
	if report.Message != want {
		t.Errorf("stored message = %q, want %q", report.Message, want)
	}
	for _, v := range []string{"pw-1", "qv-2", "qv-3", resolverTestQueryValue} {
		if strings.Contains(logs, v) {
			t.Errorf("logs carry %q: %s", v, logs)
		}
	}
	assertResolverLogRecord(t, logs, "runLaunch: agent start failed", startErrTextRunID)
}

// TestProvisionOnly_SkillResolverError_FixedReasonPhrase covers the
// provision-only and reprovision branch of create.
func TestProvisionOnly_SkillResolverError_FixedReasonPhrase(t *testing.T) {
	const runID = "run-provision-5061"
	for _, reprovision := range []bool{false, true} {
		t.Run(fmt.Sprintf("reprovision=%v", reprovision), func(t *testing.T) {
			var srv *Server
			if reprovision {
				var mgr *provisionCapturingManager
				srv, mgr = newTestServerWithProvisionCapture()
				mgr.reprovisionErr = resolverTestError()
			} else {
				srv = newTestServer(t)
				srv.manager.(*mockManager).provisionErr = resolverTestError()
			}
			logs := &syncBuffer{}
			srv.agentLifecycleLog = slog.New(slog.NewJSONHandler(logs, nil))

			body := fmt.Sprintf(`{"name": "skill-provision-agent", "id": "agent-skill-provision", "runId": %q,
				"provisionOnly": true, "reprovision": %v, "config": {"template": "claude"}}`, runID, reprovision)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)

			var resp ErrorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode error body: %v (status %d, body %s)", err, w.Code, w.Body.String())
			}
			want := `Failed to provision agent: required skill "` + resolverTestSkill + `" could not be resolved: resolver error (ref ` + runID + `)`
			if resp.Error.Message != want {
				t.Errorf("message = %q, want %q", resp.Error.Message, want)
			}
			if resp.Error.Details["skill"] != resolverTestSkill {
				t.Errorf("details.skill = %v, want %q", resp.Error.Details["skill"], resolverTestSkill)
			}
			if strings.Contains(w.Body.String(), "ARBITRARY-UPSTREAM-BODY") {
				t.Errorf("response body carries resolver text: %s", w.Body.String())
			}
			assertResolverLogRecord(t, logs.String(), "", runID)
		})
	}
}

// resolverTimeoutError is a resolver call cut off by the launch deadline.
func resolverTimeoutError() error {
	return fmt.Errorf("provision: %w: %w", &agent.SkillResolutionError{
		URI:     resolverTestSkill,
		Code:    agent.SkillErrCodeTimeout,
		Message: "timed out reaching GitHub: " + resolverTestBody,
	}, context.DeadlineExceeded)
}

// TestAsyncLaunch_SkillResolverDeadline_LogsOnceRedacted covers a resolver
// cut off by the launch deadline: the launch reports launch_timeout with
// its own text, and the skill failure is still logged once, redacted, with
// the general record's correlation id.
func TestAsyncLaunch_SkillResolverDeadline_LogsOnceRedacted(t *testing.T) {
	mgr := newAsyncManager()
	mgr.startBlock = make(chan struct{}) // never closed: Start blocks until ctx' expires
	defer close(mgr.startBlock)
	mgr.startCancelErr = resolverTimeoutError()
	srv, rtb := newAsyncTestServer(t, mgr)
	logs := &syncBuffer{}
	srv.agentLifecycleLog = slog.New(slog.NewJSONHandler(logs, nil))
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
	}

	// LaunchTimeoutSeconds=21, the smallest accepted value, leaves ctx' a
	// budget of about 1s.
	w := postCreate(t, srv, map[string]any{
		"id": "skill-deadline-async", "name": "skill-deadline-async", "asyncLaunch": true, "launchId": "L-skill-deadline",
		"projectId": startErrTextProjectID, "runId": startErrTextRunID,
		"launchTimeoutSeconds": 21, "config": map[string]any{"template": "claude"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("async create: status = %d, body = %s", w.Code, w.Body.String())
	}
	var failed *hubclient.AgentLaunchReport
	if !waitUntil(t, 8*time.Second, func() bool {
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
	if failed.ErrorCode != api.LaunchCodeLaunchTimeout {
		t.Fatalf("error code = %q, want %q", failed.ErrorCode, api.LaunchCodeLaunchTimeout)
	}
	if want := "launch timed out before the agent started"; failed.Message != want {
		t.Errorf("stored message = %q, want %q", failed.Message, want)
	}
	assertResolverLogRecord(t, logs.String(), "runLaunch: agent start failed", startErrTextRunID)
}

// TestSyncCreate_SkillResolverDeadline_LogsOnceRedacted is the synchronous
// create counterpart.
func TestSyncCreate_SkillResolverDeadline_LogsOnceRedacted(t *testing.T) {
	mgr := newAsyncManager()
	mgr.setStartErr(resolverTimeoutError())
	srv, _ := newAsyncTestServer(t, mgr)
	logs := &syncBuffer{}
	srv.agentLifecycleLog = slog.New(slog.NewJSONHandler(logs, nil))

	w := postCreate(t, srv, map[string]any{
		"name": "skill-deadline-sync", "projectId": startErrTextProjectID, "runId": startErrTextRunID,
		"config": map[string]any{"template": "claude"},
	})
	var resp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode error body: %v (status %d, body %s)", err, w.Code, w.Body.String())
	}
	want := `Failed to provision agent: required skill "` + resolverTestSkill + `" could not be resolved: timed out reaching the skill source (ref ` + startErrTextRunID + `)`
	if resp.Error.Message != want {
		t.Errorf("message = %q, want %q", resp.Error.Message, want)
	}
	if w.Code != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want %d", w.Code, http.StatusGatewayTimeout)
	}
	assertResolverLogRecord(t, logs.String(), "Agent create failed", startErrTextRunID)
}

// TestSyncCreate_SkillFailureWithEarlierCase_LogsOnce covers an error that
// holds a skill failure together with a case the create response checks
// first (template not found): the response is that case's, and the skill
// failure is still logged once, with the general record's correlation id.
func TestSyncCreate_SkillFailureWithEarlierCase_LogsOnce(t *testing.T) {
	mgr := newAsyncManager()
	mgr.setStartErr(fmt.Errorf("%w: %w", config.ErrTemplateNotFound, resolverTestError()))
	srv, _ := newAsyncTestServer(t, mgr)
	logs := &syncBuffer{}
	srv.agentLifecycleLog = slog.New(slog.NewJSONHandler(logs, nil))

	w := postCreate(t, srv, map[string]any{
		"name": "skill-earlier-case", "projectId": startErrTextProjectID, "runId": startErrTextRunID,
		"config": map[string]any{"template": "claude"},
	})
	var resp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode error body: %v (status %d, body %s)", err, w.Code, w.Body.String())
	}
	if w.Code != http.StatusNotFound || resp.Error.Code != ErrCodeNotFound {
		t.Errorf("got status %d code %q, want %d %q", w.Code, resp.Error.Code, http.StatusNotFound, ErrCodeNotFound)
	}
	if strings.Contains(w.Body.String(), "ARBITRARY-UPSTREAM-BODY") {
		t.Errorf("response body carries resolver text: %s", w.Body.String())
	}
	assertResolverLogRecord(t, logs.String(), "Agent create failed", startErrTextRunID)
}

// TestProvisionOnly_SkillFailureWithEarlierCase_LogsOnce is the
// provision-only counterpart of TestSyncCreate_SkillFailureWithEarlierCase_LogsOnce.
func TestProvisionOnly_SkillFailureWithEarlierCase_LogsOnce(t *testing.T) {
	const runID = "run-provision-earlier-5061"
	srv := newTestServer(t)
	srv.manager.(*mockManager).provisionErr = fmt.Errorf("%w: %w", config.ErrTemplateNotFound, resolverTestError())
	logs := &syncBuffer{}
	srv.agentLifecycleLog = slog.New(slog.NewJSONHandler(logs, nil))

	body := fmt.Sprintf(`{"name": "skill-provision-earlier", "id": "agent-skill-provision-earlier", "runId": %q,
		"provisionOnly": true, "config": {"template": "claude"}}`, runID)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	var resp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode error body: %v (status %d, body %s)", err, w.Code, w.Body.String())
	}
	if w.Code != http.StatusNotFound || resp.Error.Code != ErrCodeNotFound {
		t.Errorf("got status %d code %q, want %d %q", w.Code, resp.Error.Code, http.StatusNotFound, ErrCodeNotFound)
	}
	if strings.Contains(w.Body.String(), "ARBITRARY-UPSTREAM-BODY") {
		t.Errorf("response body carries resolver text: %s", w.Body.String())
	}
	assertResolverLogRecord(t, logs.String(), "Agent provision failed: template or harness-config not found", runID)
}
