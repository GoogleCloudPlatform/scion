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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// TestClassifyGateAnswer covers design t1-async-create-v11.md §3.8.2's
// claim/checkpoint answer table (B-2).
func TestClassifyGateAnswer(t *testing.T) {
	cases := []struct {
		name   string
		result *hubclient.AgentLaunchReportResult
		want   gateAction
	}{
		{"applied", &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, gateContinue},
		{"duplicate", &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultDuplicate}, gateContinue},
		{"completed", &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultCompleted}, gateCompleted},
		{"409 superseded", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Reason: hubclient.AgentLaunchReportReasonSuperseded}, gateAbortNoCleanup},
		{"409 other_owner", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Reason: hubclient.AgentLaunchReportReasonOtherOwner}, gateAbortNoCleanup},
		{"409 deleted", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Reason: hubclient.AgentLaunchReportReasonDeleted}, gateAbortCleanup},
		{"409 stopped", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Reason: hubclient.AgentLaunchReportReasonStopped}, gateAbortCleanup},
		{"409 timed_out", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Reason: hubclient.AgentLaunchReportReasonTimedOut}, gateAbortCleanup},
		{"409 lost", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Reason: hubclient.AgentLaunchReportReasonLost}, gateAbortCleanup},
		{"409 failed", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Reason: hubclient.AgentLaunchReportReasonFailed}, gateAbortCleanup},
		{"409 not_launched", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Reason: hubclient.AgentLaunchReportReasonNotLaunched}, gateAbortCleanup},
		{"403", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusForbidden}, gateAbortCleanup},
		{"404 agent_launch_unknown", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusNotFound, Code: hubclient.AgentLaunchReportCodeUnknownLaunch}, gateAbortCleanup},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyGateAnswer(c.result); got != c.want {
				t.Errorf("classifyGateAnswer(%+v) = %v, want %v", c.result, got, c.want)
			}
		})
	}
}

// TestShouldCleanupAfterFailureReport covers design §3.8.2 step 7 (F2(d)):
// every answer to a failed report cleans up except completed and 409
// superseded/other_owner.
func TestShouldCleanupAfterFailureReport(t *testing.T) {
	cases := []struct {
		name   string
		result *hubclient.AgentLaunchReportResult
		want   bool
	}{
		{"applied", &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, true},
		{"completed", &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultCompleted}, false},
		{"409 superseded", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Reason: hubclient.AgentLaunchReportReasonSuperseded}, false},
		{"409 other_owner", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Reason: hubclient.AgentLaunchReportReasonOtherOwner}, false},
		{"409 timed_out (reaper refine)", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Reason: hubclient.AgentLaunchReportReasonTimedOut}, true},
		{"409 lost", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Reason: hubclient.AgentLaunchReportReasonLost}, true},
		{"403", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusForbidden}, true},
		{"404 agent_launch_unknown", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusNotFound, Code: hubclient.AgentLaunchReportCodeUnknownLaunch}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldCleanupAfterFailureReport(c.result); got != c.want {
				t.Errorf("shouldCleanupAfterFailureReport(%+v) = %v, want %v", c.result, got, c.want)
			}
		})
	}
}

func TestClassifyStartError(t *testing.T) {
	t.Run("ctx expired means launch_timeout regardless of the error", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 0)
		defer cancel()
		<-ctx.Done()
		code, _ := classifyStartError(ctx, errors.New("some runtime error"))
		if code != "launch_timeout" {
			t.Fatalf("code = %q, want launch_timeout", code)
		}
	})

	t.Run("template not found", func(t *testing.T) {
		code, _ := classifyStartError(context.Background(), config.ErrTemplateNotFound)
		if code != "template_not_found" {
			t.Fatalf("code = %q, want template_not_found", code)
		}
	})

	t.Run("harness config not found", func(t *testing.T) {
		code, _ := classifyStartError(context.Background(), config.ErrHarnessConfigNotFound)
		if code != "template_not_found" {
			t.Fatalf("code = %q, want template_not_found", code)
		}
	})

	t.Run("other errors are runtime_error", func(t *testing.T) {
		code, _ := classifyStartError(context.Background(), errors.New("boom"))
		if code != "runtime_error" {
			t.Fatalf("code = %q, want runtime_error", code)
		}
	})
}
