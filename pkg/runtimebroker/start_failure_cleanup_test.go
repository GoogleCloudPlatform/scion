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

// Unit tests of the ptone/scion#5368 additions: the StartFailed clean-up
// classifier, the failed report's runId and the journal's terminal flags.
package runtimebroker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

func TestStartFailureCleanup_Classifier(t *testing.T) {
	ok := func(result string) *hubclient.AgentLaunchReportResult {
		return &hubclient.AgentLaunchReportResult{Result: result}
	}
	status := func(code int) *hubclient.AgentLaunchReportResult {
		return &hubclient.AgentLaunchReportResult{HTTPStatus: code}
	}
	conflict := func(reason string) *hubclient.AgentLaunchReportResult {
		return &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Code: hubclient.AgentLaunchReportCodeStaleLaunch, Reason: reason}
	}
	unknownLaunch := &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusNotFound, Code: hubclient.AgentLaunchReportCodeUnknownLaunch}
	noAnswer := errors.New("simulated unreachable")

	cases := []struct {
		name          string
		result        *hubclient.AgentLaunchReportResult
		err           error
		postCompleted bool
		want          launchCleanup
	}{
		// Post-completed: the total table.
		{"post-completed applied", ok(hubclient.AgentLaunchReportResultApplied), nil, true, launchCleanupFull},
		{"post-completed duplicate", ok(hubclient.AgentLaunchReportResultDuplicate), nil, true, launchCleanupFull},
		{"post-completed 409 failed", conflict(hubclient.AgentLaunchReportReasonFailed), nil, true, launchCleanupFull},
		{"post-completed 409 superseded", conflict(hubclient.AgentLaunchReportReasonSuperseded), nil, true, launchCleanupNone},
		{"post-completed 409 other_owner", conflict(hubclient.AgentLaunchReportReasonOtherOwner), nil, true, launchCleanupNone},
		{"post-completed no answer", nil, noAnswer, true, launchCleanupResources},
		{"post-completed completed", ok(hubclient.AgentLaunchReportResultCompleted), nil, true, launchCleanupResources},
		{"post-completed 400", status(http.StatusBadRequest), nil, true, launchCleanupResources},
		{"post-completed 401", status(http.StatusUnauthorized), nil, true, launchCleanupResources},
		{"post-completed 403", status(http.StatusForbidden), nil, true, launchCleanupResources},
		{"post-completed 404 agent_launch_unknown", unknownLaunch, nil, true, launchCleanupResources},
		{"post-completed 409 deleted", conflict(hubclient.AgentLaunchReportReasonDeleted), nil, true, launchCleanupResources},
		{"post-completed 409 stopped", conflict(hubclient.AgentLaunchReportReasonStopped), nil, true, launchCleanupResources},
		{"post-completed 409 timed_out", conflict(hubclient.AgentLaunchReportReasonTimedOut), nil, true, launchCleanupResources},
		{"post-completed 409 lost", conflict(hubclient.AgentLaunchReportReasonLost), nil, true, launchCleanupResources},
		{"post-completed 409 not_launched", conflict(hubclient.AgentLaunchReportReasonNotLaunched), nil, true, launchCleanupResources},
		{"post-completed 409 unrecognised reason", conflict("some_future_reason"), nil, true, launchCleanupResources},

		// StartFailed only: shouldCleanupAfterFailureReport, except
		// 200 completed gives resources-only.
		{"start-failed applied", ok(hubclient.AgentLaunchReportResultApplied), nil, false, launchCleanupFull},
		{"start-failed duplicate", ok(hubclient.AgentLaunchReportResultDuplicate), nil, false, launchCleanupFull},
		{"start-failed completed", ok(hubclient.AgentLaunchReportResultCompleted), nil, false, launchCleanupResources},
		{"start-failed no answer", nil, noAnswer, false, launchCleanupNone},
		{"start-failed 409 superseded", conflict(hubclient.AgentLaunchReportReasonSuperseded), nil, false, launchCleanupNone},
		{"start-failed 409 other_owner", conflict(hubclient.AgentLaunchReportReasonOtherOwner), nil, false, launchCleanupNone},
		{"start-failed 400", status(http.StatusBadRequest), nil, false, launchCleanupNone},
		{"start-failed 401", status(http.StatusUnauthorized), nil, false, launchCleanupNone},
		{"start-failed 403", status(http.StatusForbidden), nil, false, launchCleanupFull},
		{"start-failed 404 agent_launch_unknown", unknownLaunch, nil, false, launchCleanupFull},
		{"start-failed 409 deleted", conflict(hubclient.AgentLaunchReportReasonDeleted), nil, false, launchCleanupFull},
		{"start-failed 409 failed", conflict(hubclient.AgentLaunchReportReasonFailed), nil, false, launchCleanupFull},
		{"start-failed 409 timed_out", conflict(hubclient.AgentLaunchReportReasonTimedOut), nil, false, launchCleanupFull},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := startFailureCleanup(c.result, c.err, c.postCompleted); got != c.want {
				t.Fatalf("startFailureCleanup(%+v, %v, %v) = %d, want %d", c.result, c.err, c.postCompleted, got, c.want)
			}
		})
	}
}

func TestSendTerminal_FailedCarriesRunID(t *testing.T) {
	rtb := &mockRuntimeBrokerService{}
	s := newTestLaunchSender(t, rtb, 0)
	s.rec.RunID = "run-terminal"

	if _, err := s.SendTerminal(context.Background(), false, "workspace_sync", "launch_timeout", "timed out", "", nil); err != nil {
		t.Fatal(err)
	}
	// The succeeded echo's own runId (the started entry's label) is not the
	// report's top-level runId.
	if _, err := s.SendTerminal(context.Background(), true, "", "", "", "", &hubclient.AgentLaunchReportInfo{ID: "c1", RunID: "run-entry"}); err != nil {
		t.Fatal(err)
	}
	reports := rtb.getLaunchReports()
	if len(reports) != 2 {
		t.Fatalf("expected two reports, got %d", len(reports))
	}
	if got := reportJSON(t, reports[0].Report)["runId"]; got != "run-terminal" {
		t.Fatalf("the failed report's top-level runId = %v, want the launch's run run-terminal", got)
	}
	succeeded := reportJSON(t, reports[1].Report)
	if got, ok := succeeded["runId"]; ok {
		t.Fatalf("the succeeded report must stay unchanged, with no top-level runId, got %v", got)
	}
	if agentInfo, _ := succeeded["agent"].(map[string]any); agentInfo == nil || agentInfo["runId"] != "run-entry" {
		t.Fatalf("the succeeded report's echo lost its own runId: %v", succeeded["agent"])
	}
}

func TestJournalTerminal_FlagsOmittedWhenFalse(t *testing.T) {
	raw, err := json.Marshal(journalTerminal{ErrorCode: "runtime_error"})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"startFailed", "postCompleted"} {
		if strings.Contains(string(raw), `"`+key+`"`) {
			t.Fatalf("%s must be omitted when false: %s", key, raw)
		}
	}
	raw, err = json.Marshal(journalTerminal{ErrorCode: "runtime_error", StartFailed: true, PostCompleted: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"startFailed":true`) || !strings.Contains(string(raw), `"postCompleted":true`) {
		t.Fatalf("set flags must be encoded: %s", raw)
	}

	// A record from an older broker, with neither key, is read (journal
	// version 1 is unchanged) with both flags false.
	path := filepath.Join(t.TempDir(), "L1.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"launchId":"L1","agentId":"a","kind":"create","instanceId":"i","deadline":"2026-01-01T00:00:00Z","seq":1,"slug":"s","agentName":"s","state":"terminal_pending","terminal":{"succeeded":false,"errorCode":"runtime_error"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	old, err := readJournalRecord(path)
	if err != nil {
		t.Fatalf("an older version-1 record must still be read: %v", err)
	}
	if old.Terminal == nil || old.Terminal.StartFailed || old.Terminal.PostCompleted {
		t.Fatalf("an older record must read both flags as false: %+v", old.Terminal)
	}
}
