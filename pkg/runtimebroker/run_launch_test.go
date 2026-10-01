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
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
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
		{"400", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusBadRequest}, gateStopNoCleanup},
		{"401", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusUnauthorized}, gateStopNoCleanup},
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
		{"400", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusBadRequest}, false},
		{"401", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusUnauthorized}, false},
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

// TestTerminalContext covers a deadline several minutes in the past still
// leaving a live context, because the TTL is deadline + 10 minutes, not the
// deadline itself.
func TestTerminalContext(t *testing.T) {
	ctx, cancel := terminalContext(time.Now().Add(-5 * time.Minute))
	defer cancel()
	if ctx.Err() != nil {
		t.Fatalf("expected a live context 5 minutes past the deadline (TTL = deadline+10min), got %v", ctx.Err())
	}
}

func TestTerminalContext_ExpiredPastTheTTL(t *testing.T) {
	ctx, cancel := terminalContext(time.Now().Add(-15 * time.Minute))
	defer cancel()
	if ctx.Err() == nil {
		t.Fatal("expected an expired context 15 minutes past the deadline (beyond the 10 min TTL)")
	}
}

func TestShouldCleanupAfterSucceededReport(t *testing.T) {
	cases := []struct {
		name   string
		result *hubclient.AgentLaunchReportResult
		want   bool
	}{
		{"applied", &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, false},
		{"duplicate", &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultDuplicate}, false},
		{"completed", &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultCompleted}, false},
		{"409 superseded", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Reason: hubclient.AgentLaunchReportReasonSuperseded}, false},
		{"409 other_owner", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Reason: hubclient.AgentLaunchReportReasonOtherOwner}, false},
		{"409 timed_out", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Reason: hubclient.AgentLaunchReportReasonTimedOut}, true},
		{"409 lost", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Reason: hubclient.AgentLaunchReportReasonLost}, true},
		{"403", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusForbidden}, true},
		{"404 agent_launch_unknown", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusNotFound, Code: hubclient.AgentLaunchReportCodeUnknownLaunch}, true},
		{"400", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusBadRequest}, false},
		{"401", &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusUnauthorized}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldCleanupAfterSucceededReport(c.result); got != c.want {
				t.Errorf("shouldCleanupAfterSucceededReport(%+v) = %v, want %v", c.result, got, c.want)
			}
		})
	}
}

// TestCleanupAbortedLaunch_DeletesFilesWhenMarkerMatches and
// TestCleanupAbortedLaunch_KeepsFilesWhenMarkerMismatched cover
// cleanupAbortedLaunch actually removing the agent's files when its own
// marker write is still the current one, and leaving them alone when a
// newer launch's marker write has superseded it (design §3.8.4; this is
// also the two-brokers-sharing-an-agents-root case, since the marker
// mechanism is broker-agnostic file state, not in-memory state).
func TestCleanupAbortedLaunch_DeletesFilesWhenMarkerMatches(t *testing.T) {
	projectDir := t.TempDir()
	agentDir := filepath.Join(projectDir, "agents", "agent-x")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "prompt.md"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeLaunchMarker(projectDir, false, "agent-x", "L1"); err != nil {
		t.Fatal(err)
	}

	mgr := newAsyncManager()
	srv, _ := newAsyncTestServer(t, mgr)
	rec := newLaunchRecord("L1", "agent-id", store.LaunchKindCreate, "", time.Now().Add(time.Minute), func() {})
	lc := launchCtx{
		opts: api.StartOptions{Name: "agent-x", ProjectPath: projectDir},
		mgr:  mgr,
		key:  launchKey{Slug: "agent-x"},
	}

	srv.cleanupAbortedLaunch(mgr, rec, lc)

	if _, err := os.Stat(agentDir); !os.IsNotExist(err) {
		t.Fatalf("expected the agent directory to be removed (marker matched), stat err = %v", err)
	}
	if n := mgr.CleanupCallCount(); n != 1 {
		t.Fatalf("expected CleanupLaunch to be called once, got %d", n)
	}
}

func TestCleanupAbortedLaunch_KeepsFilesWhenMarkerMismatched(t *testing.T) {
	projectDir := t.TempDir()
	agentDir := filepath.Join(projectDir, "agents", "agent-y")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "prompt.md"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	// A newer launch's marker write has superseded this one.
	if err := writeLaunchMarker(projectDir, false, "agent-y", "L-newer"); err != nil {
		t.Fatal(err)
	}

	mgr := newAsyncManager()
	srv, _ := newAsyncTestServer(t, mgr)
	rec := newLaunchRecord("L-old", "agent-id", store.LaunchKindCreate, "", time.Now().Add(time.Minute), func() {})
	lc := launchCtx{
		opts: api.StartOptions{Name: "agent-y", ProjectPath: projectDir},
		mgr:  mgr,
		key:  launchKey{Slug: "agent-y"},
	}

	srv.cleanupAbortedLaunch(mgr, rec, lc)

	if _, err := os.Stat(agentDir); err != nil {
		t.Fatalf("expected the agent directory to survive a marker mismatch, stat err = %v", err)
	}
}

// TestLaunchSenderAndLaunchCtx_NoHTTPRequestField is design §6 B-6's
// type-level assertion: the types the launch goroutine is built from must
// never carry an *http.Request or http.Request field, so there is no code
// path that could read one.
func TestLaunchSenderAndLaunchCtx_NoHTTPRequestField(t *testing.T) {
	assertNoHTTPRequestField(t, reflect.TypeOf(launchSender{}))
	assertNoHTTPRequestField(t, reflect.TypeOf(launchCtx{}))
	assertNoHTTPRequestField(t, reflect.TypeOf(launchRecord{}))
}

func assertNoHTTPRequestField(t *testing.T, typ reflect.Type) {
	t.Helper()
	reqType := reflect.TypeOf(http.Request{})
	reqPtrType := reflect.TypeOf(&http.Request{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Type == reqType || f.Type == reqPtrType {
			t.Fatalf("%s.%s has type %s; the launch goroutine must never retain *http.Request", typ.Name(), f.Name, f.Type)
		}
	}
}
