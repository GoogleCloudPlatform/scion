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
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

const asyncUnsupportedWarn = "async launch requested for a runtime that does not support async launch; falling back to synchronous create"

// runtimeAsyncManager is an asyncManager that reports the runtime it runs
// agents on, the way agent.AgentManager does through its Runtime field.
type runtimeAsyncManager struct {
	*asyncManager
	rt runtime.Runtime
}

func (m *runtimeAsyncManager) managerRuntime() runtime.Runtime { return m.rt }

func newTestSubstrateRuntime() *runtime.SubstrateRuntime {
	return runtime.NewSubstrateRuntimeForTest(nil, nil, nil, config.V1SubstrateConfig{})
}

func asyncCreateBody(name, launchID string) map[string]any {
	return map[string]any{
		"name": name, "asyncLaunch": true, "launchId": launchID,
		"launchTimeoutSeconds": 300, "config": map[string]any{"template": "claude"},
	}
}

// TestAsyncCreate_RuntimeWithoutAsyncSupportStaysSynchronous: an otherwise
// eligible async create whose resolved runtime does not call the async
// launch hooks (substrate, the cloudrun runtime) takes the synchronous path
// and logs why.
func TestAsyncCreate_RuntimeWithoutAsyncSupportStaysSynchronous(t *testing.T) {
	for _, tc := range []struct {
		name string
		rt   runtime.Runtime
	}{
		{"substrate", newTestSubstrateRuntime()},
		{"cloudrun", &runtime.CloudRunRuntime{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr := &runtimeAsyncManager{asyncManager: newAsyncManager(), rt: tc.rt}
			srv, rtb := newAsyncTestServer(t, mgr)
			var logBuf bytes.Buffer
			srv.agentLifecycleLog = slog.New(slog.NewTextHandler(&logBuf, nil))

			w := postCreate(t, srv, asyncCreateBody("agent-"+tc.name, "L-"+tc.name))
			if w.Code != http.StatusCreated {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}
			resp := decodeCreateResponse(t, w)
			if resp.LaunchPending {
				t.Fatalf("LaunchPending must be false for a runtime without async launch support: %+v", resp)
			}
			if resp.Agent == nil || !resp.Created {
				t.Fatalf("expected a synchronous created agent: %+v", resp)
			}
			if n := mgr.PreflightCallCount(); n != 0 {
				t.Fatalf("Preflight must not be called on the synchronous path, got %d calls", n)
			}
			if n := mgr.StartCallCount(); n != 1 {
				t.Fatalf("expected exactly 1 synchronous Start call, got %d", n)
			}
			if len(rtb.getLaunchReports()) != 0 {
				t.Fatalf("the synchronous path must send no launch reports")
			}
			if !strings.Contains(logBuf.String(), asyncUnsupportedWarn) {
				t.Fatalf("expected the fallback warning in the log, got:\n%s", logBuf.String())
			}
		})
	}
}

// TestAsyncCreate_RuntimeWithoutCapabilityMethodStillAsync is the positive
// control: a runtime that does not implement AsyncLaunchUnsupportedRuntime
// keeps the async path.
func TestAsyncCreate_RuntimeWithoutCapabilityMethodStillAsync(t *testing.T) {
	inner := newAsyncManager()
	inner.startBlock = make(chan struct{})
	t.Cleanup(func() { close(inner.startBlock) })
	mgr := &runtimeAsyncManager{asyncManager: inner, rt: &runtime.MockRuntime{NameFunc: func() string { return "docker" }}}
	srv, _ := newAsyncTestServer(t, mgr)
	var logBuf bytes.Buffer
	srv.agentLifecycleLog = slog.New(slog.NewTextHandler(&logBuf, nil))

	w := postCreate(t, srv, asyncCreateBody("agent-dock", "L-dock"))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	resp := decodeCreateResponse(t, w)
	if !resp.LaunchPending || resp.LaunchID != "L-dock" {
		t.Fatalf("expected an accepted async launch, got %+v", resp)
	}
	if !waitUntil(t, time.Second, func() bool { return mgr.StartCallCount() == 1 }) {
		t.Fatalf("expected the async launch to reach Start")
	}
	if strings.Contains(logBuf.String(), asyncUnsupportedWarn) {
		t.Fatalf("unexpected fallback warning for a runtime without the capability method:\n%s", logBuf.String())
	}
}

// TestAsyncCreate_CloudRunSandboxReturnsLaunchPending: an eligible async
// create on the cloudrun-sandbox runtime, which calls the async launch
// hooks, is accepted as a pending launch with no synchronous fallback, and
// the launch runs to a succeeded report. Start does not block, so a
// synchronous fallback completes and fails the LaunchPending assertion
// instead of hanging.
func TestAsyncCreate_CloudRunSandboxReturnsLaunchPending(t *testing.T) {
	mgr := &runtimeAsyncManager{asyncManager: newAsyncManager(), rt: &runtime.CloudRunSandboxRuntime{}}
	srv, rtb := newAsyncTestServer(t, mgr)
	var logBuf bytes.Buffer
	srv.agentLifecycleLog = slog.New(slog.NewTextHandler(&logBuf, nil))

	w := postCreate(t, srv, asyncCreateBody("agent-crsb", "L-crsb"))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	resp := decodeCreateResponse(t, w)
	if !resp.LaunchPending || resp.LaunchID != "L-crsb" {
		t.Fatalf("expected an accepted async launch on cloudrun-sandbox, got %+v", resp)
	}
	if !waitUntil(t, 2*time.Second, func() bool {
		for _, r := range rtb.getLaunchReports() {
			if r.Report.State == hubclient.AgentLaunchReportStateSucceeded {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("expected the async launch to send a succeeded report")
	}
	if strings.Contains(logBuf.String(), asyncUnsupportedWarn) {
		t.Fatalf("unexpected fallback warning for cloudrun-sandbox:\n%s", logBuf.String())
	}
}

// TestManagerSupportsAsyncLaunch covers the lookup on the real
// agent.AgentManager the broker builds for each runtime.
func TestManagerSupportsAsyncLaunch(t *testing.T) {
	subMgr := agent.NewManager(newTestSubstrateRuntime())
	t.Cleanup(subMgr.Close)
	if managerSupportsAsyncLaunch(subMgr) {
		t.Error("substrate-backed manager: got async support, want none")
	}
	sandboxMgr := agent.NewManager(&runtime.CloudRunSandboxRuntime{})
	t.Cleanup(sandboxMgr.Close)
	if !managerSupportsAsyncLaunch(sandboxMgr) {
		t.Error("cloudrun-sandbox-backed manager: got no async support, want support")
	}
	mockMgr := agent.NewManager(&runtime.MockRuntime{NameFunc: func() string { return "docker" }})
	t.Cleanup(mockMgr.Close)
	if !managerSupportsAsyncLaunch(mockMgr) {
		t.Error("manager on a runtime without the capability method: got no async support, want support")
	}
	if !managerSupportsAsyncLaunch(newAsyncManager()) {
		t.Error("manager with no identifiable runtime: got no async support, want support")
	}
}
