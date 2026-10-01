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
