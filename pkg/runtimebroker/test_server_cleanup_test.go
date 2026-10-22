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
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// TestTestServerCleanup_EndsAsyncLaunchesBeforeTempDirRemoval pins the test
// server cleanup that fixes ptone/scion#5142. A subtest accepts an async
// launch whose Start is released only by a deferred close, and whose hub
// answers the terminal slowly, so the launch is still running when the
// subtest body returns. Once t.Run returns (every cleanup, including the
// temp-dir removal, has run), the launch must have ended and the test's
// HOME must be gone. Without the cleanup, the launch outlives the subtest
// and keeps writing its journal under a HOME that is being removed.
func TestTestServerCleanup_EndsAsyncLaunchesBeforeTempDirRemoval(t *testing.T) {
	var srv *Server
	var home string
	t.Run("launch outlives the body", func(t *testing.T) {
		mgr := newAsyncManager()
		mgr.startBlock = make(chan struct{})
		defer close(mgr.startBlock)
		var rtb *mockRuntimeBrokerService
		srv, rtb = newAsyncTestServer(t, mgr)
		home = os.Getenv("HOME")
		rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
			if req.State == hubclient.AgentLaunchReportStateSucceeded || req.State == hubclient.AgentLaunchReportStateFailed {
				time.Sleep(300 * time.Millisecond) // a slow hub: the terminal outlives the body
			}
			return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
		}
		w := postCreate(t, srv, map[string]any{
			"name": "agent-cleanup", "asyncLaunch": true, "launchId": "L-cleanup",
			"launchTimeoutSeconds": 300, "config": map[string]any{"template": "claude"},
		})
		if w.Code != http.StatusCreated {
			t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
		}
		if !waitUntil(t, 2*time.Second, func() bool { return mgr.StartCallCount() == 1 }) {
			t.Fatal("Start never called")
		}
	})
	if !registryEmpty(srv) {
		t.Fatal("an async launch was still running after its test's cleanups (ptone/scion#5142)")
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("the test's HOME %s was not removed: %v", home, err)
	}
}
