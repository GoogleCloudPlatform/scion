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
	"net/http"
	"sync"
	"testing"
)

// newShutdownTestServer builds a Server backed by a fresh in-memory store,
// mirroring the New()-only (never Start()ed) construction path exercised by
// callers such as a fast startup-abort, or combined-mode embeddings that
// mount the Hub API without running its own listener (see ptone/scion#2433).
func newShutdownTestServer(t *testing.T) *Server {
	t.Helper()

	st, err := newTestStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}

	srv, err := New(DefaultServerConfig(), st)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	return srv
}

// TestServer_Shutdown_WithoutStart_ClosesPreviewService proves that
// Shutdown() tears down background services even when the Server was never
// Start()ed (so s.httpServer is nil). Before the fix, Shutdown() returned
// nil immediately in this case without running any cleanup, leaking the
// PreviewService's cleanupNonces goroutine (ptone/scion#2433, gap 1 and
// gap 2).
func TestServer_Shutdown_WithoutStart_ClosesPreviewService(t *testing.T) {
	srv := newShutdownTestServer(t)

	if srv.previewService == nil {
		t.Fatal("expected New() to initialize previewService")
	}

	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() returned error: %v", err)
	}

	// PreviewService.Close() closes stopCleanup exactly once; a receive on
	// it succeeds immediately once closed. If Shutdown() still no-ops for a
	// Server that never started an HTTP listener, this will still be open
	// and the receive will fall through to default.
	select {
	case <-srv.previewService.stopCleanup:
	default:
		t.Fatal("Shutdown() did not close previewService (cleanupNonces goroutine still running)")
	}
}

// TestServer_ShutdownTwice_NoPanic proves that calling Shutdown() twice on
// the same Server does not panic. Before the fix, a Server that had
// Start()ed (so the first Shutdown() call ran its full body, including
// PresenceManager.Stop(), which closes a channel with no guard against a
// second close) would panic on the second Shutdown() call, because nothing
// prevented the background-service teardown from running more than once.
func TestServer_ShutdownTwice_NoPanic(t *testing.T) {
	srv := newShutdownTestServer(t)
	srv.InitPresenceManager()

	// Simulate a Server that completed Start() far enough to have a non-nil
	// httpServer, without binding a real listener: Shutdown()'s nil check on
	// s.httpServer is what historically gated whether background-service
	// teardown ran at all.
	srv.mu.Lock()
	srv.httpServer = &http.Server{}
	srv.mu.Unlock()

	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("first Shutdown() returned error: %v", err)
	}

	// Confirm the first Shutdown() actually drove teardown through
	// CleanupResources() (closing previewService among other things), rather
	// than just exercising the started-path's pre-existing no-panic
	// behavior. Before the fix, a started server's Shutdown() never closed
	// previewService.
	select {
	case <-srv.previewService.stopCleanup:
	default:
		t.Fatal("Shutdown() did not close previewService (cleanupNonces goroutine still running)")
	}

	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown() returned error: %v", err)
	}
}

// TestServer_ShutdownThenCleanupResources_NoPanic proves that calling
// Shutdown() followed by CleanupResources() on the same Server does not
// panic. Before the fix, Shutdown() and CleanupResources() each ran their
// own independent background-service teardown (including
// PresenceManager.Stop(), unguarded against a second close), so calling
// both on a started Server panicked on the second teardown.
func TestServer_ShutdownThenCleanupResources_NoPanic(t *testing.T) {
	srv := newShutdownTestServer(t)
	srv.InitPresenceManager()

	srv.mu.Lock()
	srv.httpServer = &http.Server{}
	srv.mu.Unlock()

	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() returned error: %v", err)
	}

	// Confirm Shutdown() drove teardown through CleanupResources() (closing
	// previewService among other things). Before the fix, a started
	// server's Shutdown() never closed previewService.
	select {
	case <-srv.previewService.stopCleanup:
	default:
		t.Fatal("Shutdown() did not close previewService (cleanupNonces goroutine still running)")
	}

	if err := srv.CleanupResources(context.Background()); err != nil {
		t.Fatalf("CleanupResources() returned error: %v", err)
	}
}

// TestServer_StartBackgroundServices_CleanupResources_ConcurrentRace exercises
// the combined-mode lifecycle ordering from cmd/server_foreground.go: a
// goroutine calls CleanupResources() (there, triggered by <-ctx.Done()) while
// another goroutine is still inside StartBackgroundServices(). Nothing
// enforces that StartBackgroundServices() completes before that
// CleanupResources goroutine observes ctx.Done() and runs, so the two can
// execute concurrently in production.
//
// This proves s.stopPoolSampler is safe under that interleaving: it is
// written under s.mu in StartBackgroundServices and read into a local under
// s.mu in CleanupResources (ptone/scion#2433 / GoogleCloudPlatform/scion#2196
// review comment). Before that fix, go test -race flagged a DATA RACE on
// this field under this exact test. Run with -race to confirm:
//
//	GOCACHE=/tmp/gocache-i2433-dev-5 env -u SCION_AUTO_EXPOSE_PORTS -u SCION_HUB_ENDPOINT \
//	  go test -race -p 2 -run TestServer_StartBackgroundServices_CleanupResources_ConcurrentRace ./pkg/hub/
//
// Note: s.scheduler, s.notificationDispatcher, s.lifecycleHookEvaluator,
// s.messageBrokerProxy, s.events, s.commandBus and s.presenceManager are read
// unsynchronized in the same CleanupResources call and may still be flagged
// by -race under this same interleaving; that is pre-existing behavior on
// main, unchanged by this fix, and out of this PR's scope (tracked as a
// follow-up).
func TestServer_StartBackgroundServices_CleanupResources_ConcurrentRace(t *testing.T) {
	srv := newShutdownTestServer(t)

	// Wire an enabled recorder so StartBackgroundServices actually assigns
	// s.stopPoolSampler (it is a no-op when dbMetrics is nil or disabled, see
	// dbmetrics.StartPoolSampler). The test store satisfies the `DB() *sql.DB`
	// assertion StartBackgroundServices uses to find the underlying *sql.DB.
	srv.SetDBMetrics(&countingRecorder{enabledReturns: true})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		srv.StartBackgroundServices(ctx)
	}()
	go func() {
		defer wg.Done()
		_ = srv.CleanupResources(context.Background())
	}()
	wg.Wait()
}
