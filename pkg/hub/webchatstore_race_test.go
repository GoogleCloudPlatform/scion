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
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// noopWebChatStore is a minimal WebChatStore that returns zero values for every
// method. It exists solely to exercise the read path under the race detector.
type noopWebChatStore struct{ WebChatStore }

func (noopWebChatStore) GetThreads(context.Context, string, string, int) ([]WebChatThread, error) {
	return nil, nil
}
func (noopWebChatStore) MarkThreadRead(context.Context, string, string, string) error { return nil }
func (noopWebChatStore) GetLastChannel(context.Context, string, string, string) (string, error) {
	return "", nil
}
func (noopWebChatStore) RecordChannel(context.Context, string, string, string, string, time.Time) error {
	return nil
}
func (noopWebChatStore) TouchThread(context.Context, string, string, string, string, time.Time) error {
	return nil
}
func (noopWebChatStore) EnsureGeneralTopic(context.Context, string, string) (string, bool, error) {
	return "", false, nil
}
func (noopWebChatStore) GetTopic(context.Context, string) (*WebChatTopic, error) { return nil, nil }

// TestWebChatStoreRace verifies that concurrent SetWebChatStore calls do not
// race with handler reads of s.webChatStore. Running under `go test -race`
// would fail before the fix (DEF-42) because handlers read the field without
// holding s.mu.
//
// WARNING: This test is a no-op without the race detector. A green run
// under `go test ./pkg/hub/` (no -race) proves nothing — the test
// exercises a concurrency window that only the race detector can observe.
// To get a meaningful result, run:
//
//	go test ./pkg/hub/ -run TestWebChatStoreRace -race
//
// CI does not currently pass -race, so this test does not gate merges.
// It exists for local verification and will become load-bearing if/when
// a -race CI job is added.
func TestWebChatStoreRace(t *testing.T) {
	srv, _ := testServer(t)

	const goroutines = 8
	const iterations = 200

	// Build a context with an authenticated user so handlers reach the
	// webChatStore access instead of bailing at the identity check.
	userIdent := NewAuthenticatedUser("u-1", "race@test", "Race Tester", "admin", "test")
	userCtx := contextWithIdentity(context.Background(), userIdent)

	var wg sync.WaitGroup

	// Writer goroutines: toggle webChatStore between a live store and nil.
	for i := 0; i < goroutines/2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			noop := noopWebChatStore{}
			for j := 0; j < iterations; j++ {
				if j%2 == 0 {
					srv.SetWebChatStore(noop)
				} else {
					srv.SetWebChatStore(nil)
				}
			}
		}()
	}

	// Reader goroutines: call handleChatThreads concurrently.
	// The handler reads s.webChatStore twice — once for the nil guard and
	// once for GetThreads — so a concurrent write must trigger the race
	// detector when the snapshot-under-lock fix is absent.
	for i := 0; i < goroutines/2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				req, _ := http.NewRequestWithContext(userCtx, http.MethodGet,
					"/api/v1/chat/threads?projectId=nonexistent", nil)
				w := httptest.NewRecorder()
				srv.handleChatThreads(w, req)
			}
		}()
	}

	wg.Wait()
}
