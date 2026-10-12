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
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// TestSwapRuntime_ConcurrentDispatchReadsUnderLock swaps the default runtime
// while requests and lookups that read it are in flight (ptone/scion#3111).
// Every read of the default manager and runtime goes through defaultPair
// (under s.mu), so `go test -race` reports no data race. Without -race it
// still exercises the paths for panics.
func TestSwapRuntime_ConcurrentDispatchReadsUnderLock(t *testing.T) {
	srv := newTestServer(t)
	handler := srv.Handler()
	// Two hub connections put the server in multi-hub mode, so the
	// per-hub project filter (which lists agents) is built.
	srv.hubMu.Lock()
	srv.hubConnections["hub-a"] = &HubConnection{Name: "hub-a"}
	srv.hubConnections["hub-b"] = &HubConnection{Name: "hub-b"}
	srv.hubMu.Unlock()
	projectFilter := srv.buildProjectFilterForHub("http://hub-a")
	if projectFilter == nil {
		t.Fatal("expected a project filter in multi-hub mode")
	}

	newRT := func() *runtime.MockRuntime {
		return &runtime.MockRuntime{
			NameFunc: func() string { return "mock" },
			ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
				return []api.AgentInfo{{ID: "c1", Name: "test-agent-1", Phase: "running"}}, nil
			},
			ImageExistsFunc: func(context.Context, string) (bool, error) { return true, nil },
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			srv.SwapRuntime(newRT())
		}
	}()

	readers := []func(){
		func() {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil))
		},
		func() {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/images/status?short=example:latest", nil))
		},
		func() { _, _ = srv.LookupAgent(context.Background(), "test-agent-1", "") },
		func() { _, _ = srv.resolveAgentRuntimeTarget(context.Background(), "test-agent-1", "") },
		func() { _ = srv.allManagers(context.Background()) },
		func() { _ = srv.resolveRuntimeNameForOpts(api.StartOptions{Name: "test-agent-1"}) },
		func() { _ = projectFilter("proj-1") },
		func() { _ = srv.hasRecordlessProber() },
		func() { _, _ = srv.forcedRuntime() },
	}
	for _, read := range readers {
		wg.Add(1)
		go func(read func()) {
			defer wg.Done()
			for ctx.Err() == nil {
				read()
			}
		}(read)
	}
	wg.Wait()
}
