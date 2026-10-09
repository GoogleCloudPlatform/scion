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

//go:build !no_sqlite

package hub

import (
	"context"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Requests finishing during HTTP drain still produce in-memory decision records.
func TestServer_Shutdown_RecordsDecisionAuditFromDrainingRequests(t *testing.T) {
	srv := newShutdownTestServer(t)
	w := &recordingDecisionAuditEmitter{}
	srv.authzService.SetDecisionAuditEmitter(w)

	entered := make(chan struct{})
	release := make(chan struct{})
	httpSrv := &http.Server{Handler: http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.EmitDecisionAudit(r.Context(), &store.DecisionAuditRecord{Result: "deny", Reason: "in-flight"})
		rw.WriteHeader(http.StatusNoContent)
	})}
	drainStarted := make(chan struct{})
	// http.Server runs OnShutdown hooks on every Shutdown call, and
	// newTestHubServer's cleanup calls srv.Shutdown a second time.
	var drainOnce sync.Once
	httpSrv.RegisterOnShutdown(func() { drainOnce.Do(func() { close(drainStarted) }) })
	srv.mu.Lock()
	srv.httpServer = httpSrv
	srv.mu.Unlock()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = httpSrv.Serve(ln) }()

	respDone := make(chan error, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err == nil {
			_ = resp.Body.Close()
		}
		respDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request never reached the handler")
	}

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- srv.Shutdown(context.Background()) }()
	select {
	case <-drainStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP drain never started")
	}
	// CleanupResources has run; the request finishes during the drain.
	close(release)
	require.NoError(t, <-respDone)
	select {
	case err := <-shutdownDone:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown did not return")
	}

	require.Len(t, w.records, 1)
	assert.Equal(t, "in-flight", w.records[0].Reason)
	assert.IsType(t, noopDecisionAuditEmitter{}, srv.decisionAuditRouter.legacy)
}

// Cleanup preserves the in-memory emission seam while listeners drain.
func TestServer_CleanupResources_LeavesDecisionAuditOpen(t *testing.T) {
	srv := newShutdownTestServer(t)
	w := &recordingDecisionAuditEmitter{}
	srv.authzService.SetDecisionAuditEmitter(w)

	require.NoError(t, srv.CleanupResources(context.Background()))
	w.EmitDecisionAudit(context.Background(), &store.DecisionAuditRecord{Result: "deny", Reason: "during-drain"})
	srv.CloseDecisionAudit(context.Background())

	require.Len(t, w.records, 1)
	assert.Equal(t, "during-drain", w.records[0].Reason)
}

// Deferred close leaves the router open until the caller drains every listener.
func TestServer_DeferDecisionAuditClose(t *testing.T) {
	srv := newShutdownTestServer(t)
	w := &recordingDecisionAuditEmitter{}
	srv.authzService.SetDecisionAuditEmitter(w)
	srv.DeferDecisionAuditClose()

	srv.mu.Lock()
	srv.httpServer = &http.Server{}
	srv.mu.Unlock()
	require.NoError(t, srv.Shutdown(context.Background()))

	// Another listener (the WebServer) is still draining.
	w.EmitDecisionAudit(context.Background(), &store.DecisionAuditRecord{Result: "deny", Reason: "web-drain"})
	srv.CloseDecisionAudit(context.Background())

	require.Len(t, w.records, 1)
	assert.Equal(t, "web-drain", w.records[0].Reason)
}
