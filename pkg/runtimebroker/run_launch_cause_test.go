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
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// runFailedLaunch runs an async create launch whose Manager.Start fails
// with startErr and returns its one terminal report.
func runFailedLaunch(t *testing.T, name string, startErr error) *hubclient.AgentLaunchReport {
	t.Helper()
	mgr := newAsyncManager()
	mgr.setStartErr(startErr)
	srv, rtb := newAsyncTestServer(t, mgr)
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := newLaunchRecord("L-"+name, name, store.LaunchKindCreate, "", time.Now().Add(time.Hour), cancel)
	lc := launchCtx{
		opts: api.StartOptions{Name: name},
		mgr:  mgr,
		key:  launchKey{Slug: name},
	}

	done := make(chan struct{})
	go func() {
		srv.runLaunch(ctx, rec, lc)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runLaunch did not return after a failed Start")
	}

	var terminals []*hubclient.AgentLaunchReport
	for _, r := range rtb.getLaunchReports() {
		if r.Report.State == hubclient.AgentLaunchReportStateSucceeded || r.Report.State == hubclient.AgentLaunchReportStateFailed {
			terminals = append(terminals, r.Report)
		}
	}
	if len(terminals) != 1 {
		t.Fatalf("expected exactly one terminal, got %d: %+v", len(terminals), terminals)
	}
	got := terminals[0]
	if got.State != hubclient.AgentLaunchReportStateFailed || got.ErrorCode != ErrCodeRuntimeError {
		t.Fatalf("terminal = %+v, want failed with %s", got, ErrCodeRuntimeError)
	}
	return got
}

// TestRunLaunch_FailedReportCarriesRuntimeCause: a Start refused by the
// Kubernetes API sends a failed report whose cause is the fixed code a
// synchronous create's details.cause carries (ptone/scion#4666), and the
// report body has none of the refusal's identifying text.
func TestRunLaunch_FailedReportCarriesRuntimeCause(t *testing.T) {
	got := runFailedLaunch(t, "agent-cause-forbidden", k8sStartFailure(forbiddenPodsError()))
	if got.Cause != api.BrokerRuntimeCauseForbidden {
		t.Fatalf("terminal cause = %q, want %q", got.Cause, api.BrokerRuntimeCauseForbidden)
	}
	body, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	assertNoK8sIdentity(t, string(body))
}

// TestRunLaunch_FailedReportWithoutCause: a Start failure with no runtime
// cause sends a failed report with no cause field at all.
func TestRunLaunch_FailedReportWithoutCause(t *testing.T) {
	got := runFailedLaunch(t, "agent-cause-none", fmt.Errorf("failed to launch container: %w", errors.New("exec: docker: permission denied")))
	if got.Cause != "" {
		t.Fatalf("terminal cause = %q, want none", got.Cause)
	}
	body, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `"cause"`) {
		t.Fatalf("report body has a cause field: %s", body)
	}
}

// TestLaunchFailureCause: only a runtime_error carries a cause, and only the
// fixed code.
func TestLaunchFailureCause(t *testing.T) {
	forbidden := k8sStartFailure(forbiddenPodsError())
	cases := []struct {
		name string
		code string
		err  error
		want string
	}{
		{"runtime_error with a forbidden status", ErrCodeRuntimeError, forbidden, api.BrokerRuntimeCauseForbidden},
		{"runtime_error with no status", ErrCodeRuntimeError, errors.New("boom"), ""},
		{"another code keeps no cause", "template_not_found", fmt.Errorf("%w: %w", config.ErrTemplateNotFound, forbidden), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := launchFailureCause(tc.code, tc.err); got != tc.want {
				t.Fatalf("launchFailureCause(%q) = %q, want %q", tc.code, got, tc.want)
			}
		})
	}
}
