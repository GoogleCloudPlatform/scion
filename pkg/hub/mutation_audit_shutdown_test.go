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
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type blockingMutationAuditStore struct {
	store.Store
	fault       *storeFaultSwitch
	enteredOnce sync.Once
	entered     chan struct{}
	release     chan struct{}
	cancelled   chan struct{}
	err         error
	calls       atomic.Int32
}

func newBlockingMutationAuditStore(inner store.Store, fault *storeFaultSwitch) *blockingMutationAuditStore {
	return &blockingMutationAuditStore{
		Store:     inner,
		fault:     fault,
		entered:   make(chan struct{}),
		release:   make(chan struct{}),
		cancelled: make(chan struct{}),
	}
}

func (b *blockingMutationAuditStore) CreateMutationAudit(ctx context.Context, record *store.MutationAuditRecord) error {
	if !b.fault.Active() {
		return b.Store.CreateMutationAudit(ctx, record)
	}
	b.calls.Add(1)
	b.enteredOnce.Do(func() { close(b.entered) })
	select {
	case <-b.release:
		if b.err != nil {
			return b.err
		}
		return b.Store.CreateMutationAudit(ctx, record)
	case <-ctx.Done():
		select {
		case <-b.cancelled:
		default:
			close(b.cancelled)
		}
		return ctx.Err()
	}
}

// TestMutationAuditShutdown_JoinsInFlightWriteBeforeStoreClose verifies that
// every hub shutdown path (Shutdown, CleanupResources, and CloseAuditWriter,
// which cmd/server_foreground.go calls before closing the store) waits for
// in-flight emitMutationAudit writes to complete and refuses post-close writes
// so they never hit a closed database (ptone/scion#8111).
func TestMutationAuditShutdown_JoinsInFlightWriteBeforeStoreClose(t *testing.T) {
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
			srv, s, wrapped, fault := testServerWithStoreFault(t, newBlockingMutationAuditStore)
			fault.Arm()

			targetID := tid("mut-audit-join-" + p.name)
			srv.emitMutationAudit(context.Background(), &store.MutationAuditRecord{
				MutationType:       "agent_credential_revoke",
				TargetType:         "agent_credential",
				TargetID:           targetID,
				ActorPrincipalKind: mintAuditSystemActorKind,
				ActorPrincipalID:   mintAuditSystemActorID,
			})

			select {
			case <-wrapped.entered:
			case <-time.After(2 * time.Second):
				t.Fatal("emitMutationAudit goroutine did not enter CreateMutationAudit")
			}

			done := make(chan error, 1)
			go func() {
				done <- p.close(srv, context.Background())
			}()

			select {
			case err := <-done:
				close(wrapped.release)
				t.Fatalf("%s returned before in-flight emitMutationAudit finished (err=%v)", p.name, err)
			case <-time.After(30 * time.Millisecond):
			}

			close(wrapped.release)
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(2 * time.Second):
				t.Fatalf("%s did not return after in-flight emitMutationAudit was released", p.name)
			}

			recs, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{
				MutationType: "agent_credential_revoke",
				TargetID:     targetID,
			})
			require.NoError(t, err)
			require.Len(t, recs, 1, "in-flight mutation audit must be persisted before %s returns", p.name)

			// Any call arriving after shutdown closed admission must be skipped
			// rather than racing s.Close().
			callsBefore := wrapped.calls.Load()
			_ = s.Close()
			srv.emitMutationAudit(context.Background(), &store.MutationAuditRecord{
				MutationType:       "agent_credential_revoke",
				TargetType:         "agent_credential",
				TargetID:           targetID + "-late",
				ActorPrincipalKind: mintAuditSystemActorKind,
				ActorPrincipalID:   mintAuditSystemActorID,
			})
			assert.Equal(t, callsBefore, wrapped.calls.Load(), "post-shutdown emitMutationAudit must not write to the store")
		})
	}
}

// TestMutationAuditShutdown_BoundedWaitCancelsStuckWrite verifies that a
// blocked store never hangs shutdown: when the caller's context expires,
// CloseAuditWriter cancels the stuck write's context and returns promptly.
func TestMutationAuditShutdown_BoundedWaitCancelsStuckWrite(t *testing.T) {
	srv, _, wrapped, fault := testServerWithStoreFault(t, newBlockingMutationAuditStore)
	fault.Arm()
	t.Cleanup(func() {
		select {
		case <-wrapped.release:
		default:
			close(wrapped.release)
		}
	})

	srv.emitMutationAudit(context.Background(), &store.MutationAuditRecord{
		MutationType:       "agent_credential_revoke",
		TargetType:         "agent_credential",
		TargetID:           tid("mut-audit-stuck"),
		ActorPrincipalKind: mintAuditSystemActorKind,
		ActorPrincipalID:   mintAuditSystemActorID,
	})

	select {
	case <-wrapped.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("emitMutationAudit goroutine did not enter CreateMutationAudit")
	}

	closeCtx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()

	start := time.Now()
	require.NoError(t, srv.CloseAuditWriter(closeCtx))
	elapsed := time.Since(start)
	assert.GreaterOrEqual(t, elapsed, 30*time.Millisecond, "CloseAuditWriter should wait until ctx deadline for in-flight write")
	assert.Less(t, elapsed, 500*time.Millisecond, "CloseAuditWriter must honor ctx deadline without hanging")

	// Must be cancelled promptly by CloseAuditWriter on ctx expiry, well before
	// the 1s auditWriteTimeout would expire on its own.
	select {
	case <-wrapped.cancelled:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("stuck CreateMutationAudit context was not cancelled when shutdown wait expired")
	}
}

// TestEmitMutationAudit_CapturesCallTimeLoggerAndLogsTargetID verifies that
// emitMutationAudit captures slog.Default() before spawning its goroutine so
// an async error never bleeds into a later slog.SetDefault swap, and includes
// target_id on the warning record (ptone/scion#8111).
func TestEmitMutationAudit_CapturesCallTimeLoggerAndLogsTargetID(t *testing.T) {
	callLogs := &syncBuffer{}
	laterLogs := &syncBuffer{}
	orig := slog.Default()
	t.Cleanup(func() { slog.SetDefault(orig) })

	srv, _, wrapped, fault := testServerWithStoreFault(t, newBlockingMutationAuditStore)
	wrapped.err = errors.New("injected audit write failure")
	fault.Arm()

	slog.SetDefault(slog.New(slog.NewJSONHandler(callLogs, nil)))
	targetID := tid("mut-audit-logger")
	srv.emitMutationAudit(context.Background(), &store.MutationAuditRecord{
		MutationType:       "agent_credential_revoke",
		TargetType:         "agent_credential",
		TargetID:           targetID,
		ActorPrincipalKind: mintAuditSystemActorKind,
		ActorPrincipalID:   mintAuditSystemActorID,
	})

	select {
	case <-wrapped.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("emitMutationAudit goroutine did not enter CreateMutationAudit")
	}

	// Simulate the next test swapping slog.Default() while the goroutine is
	// still running.
	slog.SetDefault(slog.New(slog.NewJSONHandler(laterLogs, nil)))
	close(wrapped.release)
	require.NoError(t, srv.CloseAuditWriter(context.Background()))

	assert.Empty(t, strings.TrimSpace(laterLogs.String()),
		"async audit warning must not bleed into a later slog.SetDefault logger")

	var matched bool
	for _, line := range strings.Split(callLogs.String(), "\n") {
		var rec struct {
			Msg          string `json:"msg"`
			MutationType string `json:"mutation_type"`
			TargetID     string `json:"target_id"`
			Error        string `json:"error"`
		}
		if json.Unmarshal([]byte(line), &rec) == nil && rec.Msg == "failed to emit mutation audit record" {
			matched = true
			assert.Equal(t, "agent_credential_revoke", rec.MutationType)
			assert.Equal(t, targetID, rec.TargetID)
			assert.Contains(t, rec.Error, "injected audit write failure")
		}
	}
	assert.True(t, matched, "expected warning in call-time logger; got:\n%s", callLogs.String())
}
