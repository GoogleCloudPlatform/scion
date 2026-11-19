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
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type blockingMutationAuditStore struct {
	store.Store
	fault        *storeFaultSwitch
	enteredOnce  sync.Once
	entered      chan struct{}
	release      chan struct{}
	cancelled    chan struct{}
	ignoreCancel bool
	cancelDelay  time.Duration
	exited       atomic.Bool
	err          error
	calls        atomic.Int32
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
	defer b.exited.Store(true)

	if b.ignoreCancel {
		<-b.release
		if b.err != nil {
			return b.err
		}
		return b.Store.CreateMutationAudit(ctx, record)
	}

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
		if b.cancelDelay > 0 {
			select {
			case <-b.release:
			case <-time.After(b.cancelDelay):
			}
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
			// with an explicit warning rather than racing s.Close().
			dropLogs := &syncBuffer{}
			origLogger := slog.Default()
			t.Cleanup(func() { slog.SetDefault(origLogger) })
			slog.SetDefault(slog.New(slog.NewJSONHandler(dropLogs, nil)))

			callsBefore := wrapped.calls.Load()
			_ = s.Close()
			lateTargetID := targetID + "-late"
			srv.emitMutationAudit(context.Background(), &store.MutationAuditRecord{
				MutationType:       "agent_credential_revoke",
				TargetType:         "agent_credential",
				TargetID:           lateTargetID,
				ActorPrincipalKind: mintAuditSystemActorKind,
				ActorPrincipalID:   mintAuditSystemActorID,
			})
			slog.SetDefault(origLogger)

			assert.Equal(t, callsBefore, wrapped.calls.Load(), "post-shutdown emitMutationAudit must not write to the store")

			var dropMatched bool
			for _, line := range strings.Split(dropLogs.String(), "\n") {
				var rec struct {
					Msg          string `json:"msg"`
					MutationType string `json:"mutation_type"`
					TargetID     string `json:"target_id"`
				}
				if json.Unmarshal([]byte(line), &rec) == nil && rec.Msg == "dropped mutation audit record after audit shutdown" {
					dropMatched = true
					assert.Equal(t, "agent_credential_revoke", rec.MutationType)
					assert.Equal(t, lateTargetID, rec.TargetID)
				}
			}
			assert.True(t, dropMatched, "expected dropped-record warning log; got:\n%s", dropLogs.String())
		})
	}
}

// TestMutationAuditShutdown_BoundedWaitCancelsStuckWrite verifies that a
// blocked store never hangs shutdown: when the caller's context expires,
// CloseAuditWriter cancels the stuck write's context, waits a short bounded
// grace for cancelled goroutines to return before CloseAuditWriter itself
// returns, and still returns promptly (logging remaining count) if a
// non-cooperative store ignores cancellation.
func TestMutationAuditShutdown_BoundedWaitCancelsStuckWrite(t *testing.T) {
	t.Run("cooperative write unwinds before CloseAuditWriter returns", func(t *testing.T) {
		srv, _, wrapped, fault := testServerWithStoreFault(t, newBlockingMutationAuditStore)
		wrapped.cancelDelay = 25 * time.Millisecond
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
			TargetID:           tid("mut-audit-stuck-coop"),
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
		assert.True(t, wrapped.exited.Load(), "CloseAuditWriter must wait for cancelled cooperative write to exit before returning")
		assert.GreaterOrEqual(t, elapsed, 50*time.Millisecond, "CloseAuditWriter should wait ctx deadline + cooperative unwind")
		assert.Less(t, elapsed, 500*time.Millisecond, "CloseAuditWriter must honor ctx deadline + unwind grace without hanging")

		select {
		case <-wrapped.cancelled:
		default:
			t.Fatal("stuck CreateMutationAudit context was not cancelled when shutdown wait expired")
		}
	})

	t.Run("non-cooperative write is bounded by cancel grace and logged", func(t *testing.T) {
		warnLogs := &syncBuffer{}
		origLogger := slog.Default()
		t.Cleanup(func() { slog.SetDefault(origLogger) })

		srv, _, wrapped, fault := testServerWithStoreFault(t, newBlockingMutationAuditStore)
		wrapped.ignoreCancel = true
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
			TargetID:           tid("mut-audit-stuck-noncoop"),
			ActorPrincipalKind: mintAuditSystemActorKind,
			ActorPrincipalID:   mintAuditSystemActorID,
		})

		select {
		case <-wrapped.entered:
		case <-time.After(2 * time.Second):
			t.Fatal("emitMutationAudit goroutine did not enter CreateMutationAudit")
		}

		slog.SetDefault(slog.New(slog.NewJSONHandler(warnLogs, nil)))
		closeCtx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		defer cancel()

		start := time.Now()
		require.NoError(t, srv.CloseAuditWriter(closeCtx))
		elapsed := time.Since(start)
		slog.SetDefault(origLogger)

		assert.False(t, wrapped.exited.Load(), "non-cooperative write is still blocked until test cleanup releases it")
		assert.GreaterOrEqual(t, elapsed, 250*time.Millisecond, "CloseAuditWriter should wait the post-cancel grace before giving up")
		assert.Less(t, elapsed, time.Second, "CloseAuditWriter must never hang on a non-cooperative store")

		var loggedRemaining bool
		for _, line := range strings.Split(warnLogs.String(), "\n") {
			var rec struct {
				Msg       string `json:"msg"`
				Cancelled int    `json:"cancelled"`
				Running   int    `json:"running"`
			}
			if json.Unmarshal([]byte(line), &rec) == nil && rec.Running == 1 && rec.Cancelled == 1 {
				loggedRemaining = true
			}
		}
		assert.True(t, loggedRemaining, "expected warning logging cancelled=1 and running=1; got:\n%s", warnLogs.String())

		// Release the stuck goroutine before test server cleanup runs.
		close(wrapped.release)
	})
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

// TestEmitMutationAudit_SyncAndAsyncShareRecordBuildAndWrite verifies that
// emitMutationAudit (async) and emitMutationAuditSync (sync) produce identical
// actor/credential/correlation/executor fields and UTC timestamps from the
// shared builder/writer path, and that emitMutationAuditSync detaches from
// request cancellation while completing synchronously before returning
// (ptone/scion#3834).
func TestEmitMutationAudit_SyncAndAsyncShareRecordBuildAndWrite(t *testing.T) {
	origZone := time.Local
	time.Local = time.FixedZone("UTC-7", -7*60*60)
	t.Cleanup(func() { time.Local = origZone })

	srv, s := testServer(t)

	decoration := &CredentialDecoration{
		Kind:      CredentialKindUAT,
		TokenID:   "tok-shared-3834",
		TokenName: "shared-audit-token",
		Boundary:  decorationBoundary{Kind: "project", ProjectID: "proj-3834"},
		Labels:    map[string]string{"env": "ci"},
	}
	baseUser := NewAuthenticatedUser("user-3834", "user3834@example.com", "User 3834", store.UserRoleAdmin, "api")
	scoped := NewScopedUserIdentityWithDecoration(baseUser, "proj-3834", []string{"project:read"}, "tok-shared-3834", decoration)

	reqCtx, cancelReq := context.WithCancel(context.Background())
	reqCtx = logging.ContextWithRequestMeta(reqCtx, &logging.RequestMeta{RequestID: "req-shared-3834"})
	reqCtx = contextWithIdentity(reqCtx, scoped)
	reqCtx = contextWithCredentialContext(reqCtx, credentialContextForIdentity(scoped))
	reqCtx = ContextWithExecutor(reqCtx, ExecutorContext{Kind: "hub_settings", ID: "server-config"})

	asyncTarget := tid("mut-audit-async-3834")
	asyncRec := &store.MutationAuditRecord{
		MutationType:  gcpIAMNotAppliedMutation,
		TargetType:    gcpIAMAuditTarget,
		TargetID:      asyncTarget,
		BeforeSummary: "before-val",
		AfterSummary:  "after-val",
	}
	srv.emitMutationAudit(reqCtx, asyncRec)

	// Cancel the request context before calling emitMutationAuditSync to prove
	// the synchronous path detaches from caller cancellation while still
	// extracting actor metadata from reqCtx.
	cancelReq()

	syncTarget := tid("mut-audit-sync-3834")
	syncRec := &store.MutationAuditRecord{
		MutationType:  gcpIAMNotAppliedMutation,
		TargetType:    gcpIAMAuditTarget,
		TargetID:      syncTarget,
		BeforeSummary: "before-val",
		AfterSummary:  "after-val",
	}
	srv.emitMutationAuditSync(reqCtx, syncRec)

	// Synchronous record must already be persisted before CloseAuditWriter.
	syncPersisted, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{
		MutationType: gcpIAMNotAppliedMutation,
		TargetID:     syncTarget,
	})
	require.NoError(t, err)
	require.Len(t, syncPersisted, 1, "emitMutationAuditSync must persist before returning even when caller ctx is cancelled")

	require.NoError(t, srv.CloseAuditWriter(context.Background()))

	asyncPersisted, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{
		MutationType: gcpIAMNotAppliedMutation,
		TargetID:     asyncTarget,
	})
	require.NoError(t, err)
	require.Len(t, asyncPersisted, 1)

	assert.Equal(t, time.UTC, asyncRec.Timestamp.Location(), "async record Timestamp must default to UTC")
	assert.Equal(t, time.UTC, syncRec.Timestamp.Location(), "sync record Timestamp must default to UTC")

	gotAsync := asyncPersisted[0]
	gotSync := syncPersisted[0]
	assert.Equal(t, gotAsync.MutationType, gotSync.MutationType)
	assert.Equal(t, gotAsync.TargetType, gotSync.TargetType)
	assert.Equal(t, gotAsync.BeforeSummary, gotSync.BeforeSummary)
	assert.Equal(t, gotAsync.AfterSummary, gotSync.AfterSummary)
	assert.Equal(t, "user-3834", gotSync.ActorPrincipalID)
	assert.Equal(t, gotAsync.ActorPrincipalKind, gotSync.ActorPrincipalKind)
	assert.Equal(t, gotAsync.ActorPrincipalID, gotSync.ActorPrincipalID)
	assert.Equal(t, gotAsync.ActorCredentialID, gotSync.ActorCredentialID)
	assert.Equal(t, gotAsync.ActorCredentialType, gotSync.ActorCredentialType)
	assert.Equal(t, gotAsync.CredentialName, gotSync.CredentialName)
	assert.Equal(t, gotAsync.CredentialBoundaryKind, gotSync.CredentialBoundaryKind)
	assert.Equal(t, gotAsync.CredentialBoundaryProjectID, gotSync.CredentialBoundaryProjectID)
	assert.Equal(t, gotAsync.CredentialLabels, gotSync.CredentialLabels)
	assert.Equal(t, "req-shared-3834", gotSync.CorrelationID)
	assert.Equal(t, gotAsync.CorrelationID, gotSync.CorrelationID)
	assert.Equal(t, "hub_settings", gotSync.ExecutorKind)
	assert.Equal(t, "server-config", gotSync.ExecutorID)
	assert.Equal(t, gotAsync.ExecutorKind, gotSync.ExecutorKind)
	assert.Equal(t, gotAsync.ExecutorID, gotSync.ExecutorID)
}

// TestEmitMutationAuditSync_BestEffortStoreFailureAndNilStore verifies that
// emitMutationAuditSync logs store errors and nil-store conditions with the
// same structured fields as emitMutationAudit without panicking
// (ptone/scion#3834).
func TestEmitMutationAuditSync_BestEffortStoreFailureAndNilStore(t *testing.T) {
	t.Run("store error logs target_id", func(t *testing.T) {
		logs := &syncBuffer{}
		orig := slog.Default()
		t.Cleanup(func() { slog.SetDefault(orig) })

		srv, _, wrapped, fault := testServerWithStoreFault(t, newBlockingMutationAuditStore)
		wrapped.err = errors.New("injected sync audit failure")
		close(wrapped.release)
		fault.Arm()

		slog.SetDefault(slog.New(slog.NewJSONHandler(logs, nil)))
		targetID := tid("mut-audit-sync-err")
		srv.emitMutationAuditSync(context.Background(), &store.MutationAuditRecord{
			MutationType:       gcpIAMNotAppliedMutation,
			TargetType:         gcpIAMAuditTarget,
			TargetID:           targetID,
			ActorPrincipalKind: gcpIAMSystemActorKind,
			ActorPrincipalID:   gcpIAMSystemActorID,
		})
		slog.SetDefault(orig)

		var matched bool
		for _, line := range strings.Split(logs.String(), "\n") {
			var rec struct {
				Msg          string `json:"msg"`
				MutationType string `json:"mutation_type"`
				TargetID     string `json:"target_id"`
				Error        string `json:"error"`
			}
			if json.Unmarshal([]byte(line), &rec) == nil && rec.Msg == "failed to emit mutation audit record" {
				matched = true
				assert.Equal(t, gcpIAMNotAppliedMutation, rec.MutationType)
				assert.Equal(t, targetID, rec.TargetID)
				assert.Contains(t, rec.Error, "injected sync audit failure")
			}
		}
		assert.True(t, matched, "expected warning log with target_id; got:\n%s", logs.String())
	})

	t.Run("nil store logs no store on sync and async paths", func(t *testing.T) {
		logs := &syncBuffer{}
		orig := slog.Default()
		t.Cleanup(func() { slog.SetDefault(orig) })
		slog.SetDefault(slog.New(slog.NewJSONHandler(logs, nil)))

		srv := &Server{}
		srv.emitMutationAuditSync(context.Background(), &store.MutationAuditRecord{
			MutationType: gcpIAMNotAppliedMutation,
			TargetType:   gcpIAMAuditTarget,
			TargetID:     "sync-nil-store",
		})
		srv.emitMutationAudit(context.Background(), &store.MutationAuditRecord{
			MutationType: gcpIAMNotAppliedMutation,
			TargetType:   gcpIAMAuditTarget,
			TargetID:     "async-nil-store",
		})
		require.NoError(t, srv.CloseAuditWriter(context.Background()))
		slog.SetDefault(orig)

		seenTargets := map[string]string{}
		for _, line := range strings.Split(logs.String(), "\n") {
			var rec struct {
				Msg          string `json:"msg"`
				MutationType string `json:"mutation_type"`
				TargetID     string `json:"target_id"`
				Error        string `json:"error"`
			}
			if json.Unmarshal([]byte(line), &rec) == nil && rec.Msg == "failed to emit mutation audit record" {
				seenTargets[rec.TargetID] = rec.Error
			}
		}
		assert.Equal(t, map[string]string{
			"sync-nil-store":  "no store",
			"async-nil-store": "no store",
		}, seenTargets, "both sync and async paths must log no-store warning cleanly; got:\n%s", logs.String())
	})
}
