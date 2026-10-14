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

// resolvedConfigManager is asyncManager whose Start reports the config it
// resolved for the agent.
type resolvedConfigManager struct {
	*asyncManager
}

func (m *resolvedConfigManager) Start(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
	info, err := m.asyncManager.Start(ctx, opts)
	if info != nil {
		info.Image = "img:harness-default"
		info.HarnessConfig = "claude"
		info.HarnessAuth = "api-key"
		info.HarnessConfigSource = "template"
		info.Template = "tmpl"
		info.Profile = "prof"
	}
	return info, err
}

// The succeeded terminal report carries the config the start resolved, so
// the hub can record it as it does for a synchronous create
// (ptone/scion#4818).
func TestRunLaunch_SucceededReportCarriesResolvedConfig(t *testing.T) {
	mgr := &resolvedConfigManager{asyncManager: newAsyncManager()}
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

	rec := newLaunchRecord("L-cfg", "agent-cfg", store.LaunchKindCreate, "", time.Now().Add(5*time.Minute), func() {})
	lc := launchCtx{
		opts: api.StartOptions{Name: "agent-cfg"},
		mgr:  mgr,
		key:  launchKey{Slug: "agent-cfg"},
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
	got := *terminal.Agent
	want := map[string][2]string{
		"image":               {got.Image, "img:harness-default"},
		"harnessConfig":       {got.HarnessConfig, "claude"},
		"harnessAuth":         {got.HarnessAuth, "api-key"},
		"harnessConfigSource": {got.HarnessConfigSource, "template"},
		"template":            {got.Template, "tmpl"},
		"profile":             {got.Profile, "prof"},
	}
	for field, gw := range want {
		if gw[0] != gw[1] {
			t.Errorf("succeeded report agent.%s = %q, want %q", field, gw[0], gw[1])
		}
	}
}
