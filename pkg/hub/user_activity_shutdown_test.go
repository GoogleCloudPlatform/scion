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
	"database/sql"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type blockingServerLastSeenStore struct {
	store.Store
	enteredOnce sync.Once
	entered     chan struct{}
	release     chan struct{}
	calls       atomic.Int32
}

func newBlockingServerLastSeenStore(inner store.Store) *blockingServerLastSeenStore {
	return &blockingServerLastSeenStore{
		Store:   inner,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (b *blockingServerLastSeenStore) DB() *sql.DB {
	if p, ok := b.Store.(interface{ DB() *sql.DB }); ok {
		return p.DB()
	}
	return nil
}

func (b *blockingServerLastSeenStore) Dialect() string {
	if p, ok := b.Store.(interface{ Dialect() string }); ok {
		return p.Dialect()
	}
	return ""
}

func (b *blockingServerLastSeenStore) UpdateUserLastSeen(ctx context.Context, id string, lastSeen time.Time) error {
	b.calls.Add(1)
	b.enteredOnce.Do(func() { close(b.entered) })
	select {
	case <-b.release:
		return b.Store.UpdateUserLastSeen(ctx, id, lastSeen)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TestUserActivityShutdown_DrainsBeforeStoreCloseNoClosedDBWarning verifies
// that every hub shutdown path (Shutdown, CleanupResources, CloseAuditWriter)
// drains in-flight user last-seen updates before the store closes and rejects
// post-shutdown touches so no "sql: database is closed" warning is logged
// (ptone/scion#8634).
func TestUserActivityShutdown_DrainsBeforeStoreCloseNoClosedDBWarning(t *testing.T) {
	paths := []struct {
		name  string
		close func(*Server, context.Context) error
	}{
		{"Shutdown", (*Server).Shutdown},
		{"CleanupResources", (*Server).CleanupResources},
		{"CloseAuditWriter", (*Server).CloseAuditWriter},
	}

	for _, p := range paths {
		t.Run(p.name, func(t *testing.T) {
			logs := &syncBuffer{}
			origLogger := slog.Default()
			t.Cleanup(func() { slog.SetDefault(origLogger) })
			slog.SetDefault(slog.New(slog.NewJSONHandler(logs, nil)))

			rawStore, err := newTestStore(t, ":memory:")
			require.NoError(t, err)
			t.Cleanup(func() { _ = rawStore.Close() })

			wrapped := newBlockingServerLastSeenStore(rawStore)
			t.Cleanup(func() {
				select {
				case <-wrapped.release:
				default:
					close(wrapped.release)
				}
			})

			srv, err := newTestHubServer(t, testServerConfig(), wrapped)
			require.NoError(t, err)
			waitUserScopedDataSweep(t, srv)

			// Trigger userActivityMiddleware via an authenticated HTTP request.
			req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
			req.Header.Set("Authorization", "Bearer "+testDevToken)
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			require.Equal(t, http.StatusOK, rec.Code)

			select {
			case <-wrapped.entered:
			case <-time.After(2 * time.Second):
				t.Fatal("user last-seen goroutine did not enter UpdateUserLastSeen")
			}

			done := make(chan error, 1)
			go func() {
				done <- p.close(srv, context.Background())
			}()

			select {
			case err := <-done:
				close(wrapped.release)
				t.Fatalf("%s returned before in-flight user last-seen update finished (err=%v)", p.name, err)
			case <-time.After(30 * time.Millisecond):
			}

			close(wrapped.release)
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(2 * time.Second):
				t.Fatalf("%s did not return after in-flight user last-seen update was released", p.name)
			}

			// Close the store and attempt a late touch; neither the drained
			// write nor the post-close touch may hit a closed database.
			callsBefore := wrapped.calls.Load()
			require.NoError(t, rawStore.Close())
			srv.userActivity.Touch(tid("user-after-store-close-" + p.name))
			time.Sleep(20 * time.Millisecond)

			slog.SetDefault(origLogger)

			assert.Equal(t, callsBefore, wrapped.calls.Load(), "post-shutdown Touch must not write to the store")
			assert.NotContains(t, logs.String(), "database is closed", "shutdown must not log closed-DB warnings")
			assert.NotContains(t, logs.String(), "Failed to update user last-seen", "shutdown must not log last-seen failures")
		})
	}
}
