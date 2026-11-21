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
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// runIDManager is asyncManager whose Start reports the entry it created as
// labelled with runID.
type runIDManager struct {
	*asyncManager
	runID     string
	projectID string
	labels    map[string]string
}

func (m *runIDManager) Start(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
	info, err := m.asyncManager.Start(ctx, opts)
	if info != nil {
		info.RunID = m.runID
		if m.projectID != "" {
			info.ProjectID = m.projectID
		}
		if m.labels != nil {
			info.Labels = m.labels
		}
	}
	return info, err
}

// The succeeded terminal report carries the run the launched entry is
// labelled with (ptone/scion#3176): the hub settles exactly that run.
func TestRunLaunch_SucceededReportCarriesRunID(t *testing.T) {
	mgr := &runIDManager{asyncManager: newAsyncManager(), runID: "run-x"}
	srv, rtb := newAsyncTestServer(t, mgr.asyncManager)
	var mu sync.Mutex
	var terminal *hubclient.AgentLaunchReport
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		if req.State == hubclient.AgentLaunchReportStateSucceeded {
			mu.Lock()
			terminal = req
			mu.Unlock()
		}
		return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
	}

	rec := newLaunchRecord("L-runid", "agent-runid", store.LaunchKindCreate, "", time.Now().Add(5*time.Minute), func() {})
	lc := launchCtx{
		opts: api.StartOptions{Name: "agent-runid"},
		mgr:  mgr,
		key:  launchKey{Slug: "agent-runid"},
	}
	done := make(chan struct{})
	go func() {
		srv.runLaunch(context.Background(), rec, lc)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runLaunch did not return")
	}

	mu.Lock()
	defer mu.Unlock()
	if terminal == nil {
		t.Fatal("no succeeded report was sent")
	}
	if terminal.Agent == nil {
		t.Fatal("the succeeded report carries no agent info")
	}
	if terminal.Agent.RunID != "run-x" {
		t.Fatalf("succeeded report agent.runId = %q, want %q", terminal.Agent.RunID, "run-x")
	}
}

// The succeeded terminal report forwards ProjectID and Labels from the start
// result so the hub can verify entry identity before adopting a kept run ID
// (ptone/scion#8240).
func TestRunLaunch_SucceededReportCarriesProjectIDAndLabels(t *testing.T) {
	wantLabels := map[string]string{
		"agent_id":         "agent-uuid-1",
		"scion.project_id": "proj-uuid-1",
	}
	mgr := &runIDManager{
		asyncManager: newAsyncManager(),
		runID:        "run-kept",
		projectID:    "proj-uuid-1",
		labels:       wantLabels,
	}
	srv, rtb := newAsyncTestServer(t, mgr.asyncManager)
	var mu sync.Mutex
	var terminal *hubclient.AgentLaunchReport
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		if req.State == hubclient.AgentLaunchReportStateSucceeded {
			mu.Lock()
			terminal = req
			mu.Unlock()
		}
		return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
	}

	rec := newLaunchRecord("L-identity", "agent-identity", store.LaunchKindStart, "", time.Now().Add(5*time.Minute), func() {})
	lc := launchCtx{
		opts: api.StartOptions{Name: "agent-identity"},
		mgr:  mgr,
		key:  launchKey{Slug: "agent-identity"},
	}
	done := make(chan struct{})
	go func() {
		srv.runLaunch(context.Background(), rec, lc)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runLaunch did not return")
	}

	mu.Lock()
	defer mu.Unlock()
	if terminal == nil || terminal.Agent == nil {
		t.Fatal("no succeeded report with agent info was sent")
	}
	if terminal.Agent.ProjectID != "proj-uuid-1" {
		t.Errorf("succeeded report agent.projectId = %q, want %q", terminal.Agent.ProjectID, "proj-uuid-1")
	}
	for k, want := range wantLabels {
		if got := terminal.Agent.Labels[k]; got != want {
			t.Errorf("succeeded report agent.labels[%q] = %q, want %q", k, got, want)
		}
	}
}
