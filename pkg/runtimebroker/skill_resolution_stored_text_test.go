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
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
)

// Store a fixed reason phrase for skill resolver errors; full detail goes
// to server-side logs (ptone/scion#5061).

const (
	resolverTestSkill  = "gh://example-org/example-skills/my-skill@main"
	resolverTestBody   = "ARBITRARY-UPSTREAM-BODY <html>see https://internal.example.test/doc?token=s3cr3t-value</html>"
	resolverTestSecret = "s3cr3t-value"
	resolverLogMsg     = "Required skill could not be resolved"
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

// assertResolverLogRecord checks the server-side record carries the full
// resolver text under correlation id ref, with URL query values redacted.
func assertResolverLogRecord(t *testing.T, logs, ref string) {
	t.Helper()
	rec := logRecord(t, logs, resolverLogMsg)
	if rec["correlation_id"] != ref {
		t.Errorf("log correlation_id = %v, want %q", rec["correlation_id"], ref)
	}
	got, _ := rec["error"].(string)
	if !strings.Contains(got, "ARBITRARY-UPSTREAM-BODY") {
		t.Errorf("log error = %q, want the full resolver text", got)
	}
	if strings.Contains(logs, resolverTestSecret) {
		t.Errorf("logs carry the URL query value %q: %s", resolverTestSecret, logs)
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
	assertResolverLogRecord(t, logs, startErrTextRunID)
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
	assertResolverLogRecord(t, logs.String(), startErrTextRunID)
}

// TestStartRestart_SkillResolverError_FixedReasonPhrase covers start and
// restart: the correlation id in the response is the one in the log
// record.
func TestStartRestart_SkillResolverError_FixedReasonPhrase(t *testing.T) {
	refPattern := regexp.MustCompile(`^Failed to provision agent: required skill "` + regexp.QuoteMeta(resolverTestSkill) +
		`" could not be resolved: resolver error \(ref ([^()]+)\)$`)
	for _, op := range []string{"start", "restart"} {
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
			assertResolverLogRecord(t, logs.String(), m[1])
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
		URI:     "https://user:pa55word@skills.example.test/packs/my-skill.tar.gz?sig=abc123&token=xyz789",
		Code:    agent.SkillErrCodeNotFound,
		Message: "not found: " + resolverTestBody,
	}
	report, logs := asyncCreateFailure(t, "skill-resolver-http-ref", skillErr)
	want := `required skill "https://skills.example.test/packs/my-skill.tar.gz" could not be resolved: skill not found (ref ` + startErrTextRunID + `)`
	if report.Message != want {
		t.Errorf("stored message = %q, want %q", report.Message, want)
	}
	for _, secret := range []string{"pa55word", "abc123", "xyz789", resolverTestSecret} {
		if strings.Contains(logs, secret) {
			t.Errorf("logs carry %q: %s", secret, logs)
		}
	}
}
