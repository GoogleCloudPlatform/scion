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
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// installTestDecisionAuditWriter replaces srv's decision audit writer with
// one backed by fs. It closes the writer hub.New built first, so that
// writer's goroutines do not outlive the test.
func installTestDecisionAuditWriter(t *testing.T, srv *Server, fs *fakeDecisionAuditStore) *StoreDecisionAuditEmitter {
	t.Helper()
	w := newStoreDecisionAuditEmitter(fs, slog.New(slog.NewTextHandler(io.Discard, nil)), testDecisionAuditConfig())
	t.Cleanup(func() { w.Close(context.Background()) })
	if srv.decisionAuditWriter != nil {
		srv.decisionAuditWriter.Close(context.Background())
	}
	srv.decisionAuditWriter = w
	srv.authzService.SetDecisionAuditEmitter(w)
	return w
}

// TestServer_Shutdown_WritesDecisionAuditFromDrainingRequests checks the
// shutdown order: a request still in flight when the HTTP drain starts
// emits its decision audit record after CleanupResources has run, and that
// record must be written, not dropped as a shutdown drop. Before the fix,
// CleanupResources closed the writer ahead of the HTTP drain.
func TestServer_Shutdown_WritesDecisionAuditFromDrainingRequests(t *testing.T) {
	srv := newShutdownTestServer(t)
	fs := &fakeDecisionAuditStore{}
	w := installTestDecisionAuditWriter(t, srv, fs)

	entered := make(chan struct{})
	release := make(chan struct{})
	httpSrv := &http.Server{Handler: http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.EmitDecisionAudit(r.Context(), auditRec("deny", "in-flight"))
		rw.WriteHeader(http.StatusNoContent)
	})}
	drainStarted := make(chan struct{})
	httpSrv.RegisterOnShutdown(func() { close(drainStarted) })
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

	written, _ := fs.snapshot()
	assert.Equal(t, []string{"in-flight"}, written)
	assert.Zero(t, w.droppedCount(DecisionAuditDropShutdown, "deny"))

	// Shutdown closed the writer after the drain.
	w.EmitDecisionAudit(context.Background(), auditRec("deny", "late"))
	assert.Equal(t, int64(1), w.droppedCount(DecisionAuditDropShutdown, "deny"))
}

// TestServer_CleanupResources_LeavesDecisionAuditOpen checks that combined
// mode's teardown hook (CleanupResources, run before the WebServer's HTTP
// drain) does not close the writer.
func TestServer_CleanupResources_LeavesDecisionAuditOpen(t *testing.T) {
	srv := newShutdownTestServer(t)
	fs := &fakeDecisionAuditStore{}
	w := installTestDecisionAuditWriter(t, srv, fs)

	require.NoError(t, srv.CleanupResources(context.Background()))
	w.EmitDecisionAudit(context.Background(), auditRec("deny", "during-drain"))
	srv.CloseDecisionAudit(context.Background())

	written, _ := fs.snapshot()
	assert.Equal(t, []string{"during-drain"}, written)
	assert.Zero(t, w.droppedCount(DecisionAuditDropShutdown, "deny"))
}

// TestServer_DeferDecisionAuditClose checks that after
// DeferDecisionAuditClose, Shutdown leaves the writer open for the caller
// (runServer), which closes it once every listener has drained.
func TestServer_DeferDecisionAuditClose(t *testing.T) {
	srv := newShutdownTestServer(t)
	fs := &fakeDecisionAuditStore{}
	w := installTestDecisionAuditWriter(t, srv, fs)
	srv.DeferDecisionAuditClose()

	srv.mu.Lock()
	srv.httpServer = &http.Server{}
	srv.mu.Unlock()
	require.NoError(t, srv.Shutdown(context.Background()))

	// Another listener (the WebServer) is still draining.
	w.EmitDecisionAudit(context.Background(), auditRec("deny", "web-drain"))
	srv.CloseDecisionAudit(context.Background())

	written, _ := fs.snapshot()
	assert.Equal(t, []string{"web-drain"}, written)
	assert.Zero(t, w.droppedCount(DecisionAuditDropShutdown, "deny"))
}
