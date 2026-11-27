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

package entadapter_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/uploadsession"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

func newTestUploadSession(id, ownerID string, declaredSize int64, now time.Time) *store.UploadSession {
	h := sha256.New()
	hs, _ := h.(encoding.BinaryMarshaler).MarshalBinary()
	return &store.UploadSession{
		ID:             id,
		HubID:          "hub-1",
		OwnerKind:      "user",
		OwnerID:        ownerID,
		TargetKind:     store.UploadTargetWorkspace,
		ScopeProjectID: "proj-1",
		DestPath:       "dir/file.bin",
		Overwrite:      true,
		FileName:       "file.bin",
		MediaType:      "application/octet-stream",
		DeclaredSize:   declaredSize,
		HashState:      hs,
		StagingRef:     ".scion-upload-" + id + ".partial",
		IdleExpiresAt:  now.Add(time.Hour),
		HardExpiresAt:  now.Add(24 * time.Hour),
	}
}

func TestUploadSessionStore_CreateAndGet(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	st := entadapter.NewCompositeStore(client)

	now := time.Now().UTC().Truncate(time.Microsecond)
	s := newTestUploadSession("upl_create_1", "user-1", 1024, now)
	s.ArtifactID = "art-123"
	q := store.UploadQuota{
		MaxSessionsPerUser:     4,
		MaxPendingBytesPerUser: 10 << 20,
		MaxPendingBytesHub:     100 << 20,
	}

	if err := st.CreateUploadSession(ctx, s, q); err != nil {
		t.Fatalf("CreateUploadSession: %v", err)
	}
	got, err := st.GetUploadSession(ctx, s.ID)
	if err != nil {
		t.Fatalf("GetUploadSession: %v", err)
	}
	if got.ID != s.ID || got.OwnerID != "user-1" || got.TargetKind != store.UploadTargetWorkspace {
		t.Errorf("unexpected identity fields: %+v", got)
	}
	if got.ScopeProjectID != "proj-1" || got.DestPath != "dir/file.bin" || !got.Overwrite {
		t.Errorf("unexpected scope/dest fields: %+v", got)
	}
	if got.DeclaredSize != 1024 || got.Received != 0 || got.State != store.UploadStateUploading || got.Takeovers != 0 {
		t.Errorf("unexpected state fields: %+v", got)
	}
	if got.ArtifactID != "art-123" {
		t.Errorf("ArtifactID = %q, want art-123", got.ArtifactID)
	}
	if !bytes.Equal(got.HashState, s.HashState) {
		t.Errorf("HashState mismatch")
	}
	if !got.IdleExpiresAt.Equal(s.IdleExpiresAt) || !got.HardExpiresAt.Equal(s.HardExpiresAt) {
		t.Errorf("expiry mismatch: got idle=%v hard=%v, want idle=%v hard=%v",
			got.IdleExpiresAt, got.HardExpiresAt, s.IdleExpiresAt, s.HardExpiresAt)
	}

	if _, err := st.GetUploadSession(ctx, "upl_missing"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetUploadSession(missing) = %v, want ErrNotFound", err)
	}
}

func TestUploadSessionStore_Quotas(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	st := entadapter.NewCompositeStore(client)
	now := time.Now().UTC().Truncate(time.Microsecond)

	t.Run("max_sessions_per_user", func(t *testing.T) {
		q := store.UploadQuota{MaxSessionsPerUser: 2, MaxPendingBytesPerUser: 1 << 30, MaxPendingBytesHub: 2 << 30}
		if err := st.CreateUploadSession(ctx, newTestUploadSession("upl_q_cnt_1", "u-cnt", 100, now), q); err != nil {
			t.Fatalf("create 1: %v", err)
		}
		if err := st.CreateUploadSession(ctx, newTestUploadSession("upl_q_cnt_2", "u-cnt", 100, now), q); err != nil {
			t.Fatalf("create 2: %v", err)
		}
		if err := st.CreateUploadSession(ctx, newTestUploadSession("upl_q_cnt_3", "u-cnt", 100, now), q); !errors.Is(err, store.ErrQuotaExceeded) {
			t.Fatalf("create 3 = %v, want ErrQuotaExceeded", err)
		}
		// Another user is unaffected by u-cnt's per-user count.
		if err := st.CreateUploadSession(ctx, newTestUploadSession("upl_q_cnt_other", "u-cnt-other", 100, now), q); err != nil {
			t.Fatalf("create other user: %v", err)
		}
		// Aborting one session frees a slot for u-cnt.
		if err := st.TransitionUploadSession(ctx, "upl_q_cnt_1", []store.UploadSessionState{store.UploadStateUploading}, store.UploadStateAborted, "", nil); err != nil {
			t.Fatalf("abort 1: %v", err)
		}
		if err := st.CreateUploadSession(ctx, newTestUploadSession("upl_q_cnt_4", "u-cnt", 100, now), q); err != nil {
			t.Fatalf("create 4 after abort: %v", err)
		}
	})

	t.Run("max_pending_bytes_per_user", func(t *testing.T) {
		q := store.UploadQuota{MaxSessionsPerUser: 10, MaxPendingBytesPerUser: 1000, MaxPendingBytesHub: 1 << 30}
		if err := st.CreateUploadSession(ctx, newTestUploadSession("upl_q_ub_1", "u-bytes", 600, now), q); err != nil {
			t.Fatalf("create 1: %v", err)
		}
		if err := st.CreateUploadSession(ctx, newTestUploadSession("upl_q_ub_2", "u-bytes", 500, now), q); !errors.Is(err, store.ErrQuotaExceeded) {
			t.Fatalf("create 2 (600+500>1000) = %v, want ErrQuotaExceeded", err)
		}
		if err := st.CreateUploadSession(ctx, newTestUploadSession("upl_q_ub_3", "u-bytes", 400, now), q); err != nil {
			t.Fatalf("create 3 (600+400==1000): %v", err)
		}
	})

	t.Run("max_pending_bytes_hub", func(t *testing.T) {
		// Use a fresh client so earlier subtests' rows don't affect hub totals.
		c2 := enttest.NewClient(t)
		st2 := entadapter.NewCompositeStore(c2)
		q := store.UploadQuota{MaxSessionsPerUser: 10, MaxPendingBytesPerUser: 1000, MaxPendingBytesHub: 1500}

		if err := st2.CreateUploadSession(ctx, newTestUploadSession("upl_q_hub_1", "u-h1", 800, now), q); err != nil {
			t.Fatalf("create 1: %v", err)
		}
		if err := st2.CreateUploadSession(ctx, newTestUploadSession("upl_q_hub_2", "u-h2", 800, now), q); !errors.Is(err, store.ErrQuotaExceeded) {
			t.Fatalf("create 2 (800+800>1500) = %v, want ErrQuotaExceeded", err)
		}
		// Aborted session still holds staging until deleted, so it still counts toward hub bytes.
		if err := st2.TransitionUploadSession(ctx, "upl_q_hub_1", []store.UploadSessionState{store.UploadStateUploading}, store.UploadStateAborted, "", nil); err != nil {
			t.Fatalf("abort 1: %v", err)
		}
		if err := st2.CreateUploadSession(ctx, newTestUploadSession("upl_q_hub_2b", "u-h2", 800, now), q); !errors.Is(err, store.ErrQuotaExceeded) {
			t.Fatalf("create 2b while aborted row undeleted = %v, want ErrQuotaExceeded", err)
		}
		// Deleting the aborted session frees hub pending bytes.
		if err := st2.DeleteUploadSession(ctx, "upl_q_hub_1"); err != nil {
			t.Fatalf("delete 1: %v", err)
		}
		if err := st2.CreateUploadSession(ctx, newTestUploadSession("upl_q_hub_3", "u-h2", 800, now), q); err != nil {
			t.Fatalf("create 3 after delete: %v", err)
		}
		// Completed session does NOT count toward hub pending bytes.
		if _, err := st2.ClaimUploadLease(ctx, "upl_q_hub_3", store.UploadStateUploading, 0, "tok-h3", time.Minute); err != nil {
			t.Fatalf("claim h3: %v", err)
		}
		if err := st2.TransitionUploadSession(ctx, "upl_q_hub_3", []store.UploadSessionState{store.UploadStateUploading}, store.UploadStateFinalizing, "tok-h3", nil); err != nil {
			t.Fatalf("finalizing h3: %v", err)
		}
		if err := st2.TransitionUploadSession(ctx, "upl_q_hub_3", []store.UploadSessionState{store.UploadStateFinalizing}, store.UploadStateCompleted, "tok-h3", json.RawMessage(`{"ok":true}`)); err != nil {
			t.Fatalf("completed h3: %v", err)
		}
		if err := st2.CreateUploadSession(ctx, newTestUploadSession("upl_q_hub_4", "u-h3", 1000, now), q); err != nil {
			t.Fatalf("create 4 after completed: %v", err)
		}
	})
}

func TestUploadSessionStore_QuotaRace(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	st := entadapter.NewCompositeStore(client)
	now := time.Now().UTC().Truncate(time.Microsecond)

	const (
		goroutines = 16
		maxSess    = 4
	)
	q := store.UploadQuota{
		MaxSessionsPerUser:     maxSess,
		MaxPendingBytesPerUser: 10 << 20,
		MaxPendingBytesHub:     100 << 20,
	}

	var succeeded atomic.Int32
	var quotaErrs atomic.Int32
	var otherErrs atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := range goroutines {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			s := newTestUploadSession(fmt.Sprintf("upl_race_%02d", idx), "u-race", 1024, now)
			err := st.CreateUploadSession(ctx, s, q)
			switch {
			case err == nil:
				succeeded.Add(1)
			case errors.Is(err, store.ErrQuotaExceeded):
				quotaErrs.Add(1)
			default:
				otherErrs.Add(1)
				t.Errorf("goroutine %d unexpected error: %v", idx, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if otherErrs.Load() != 0 {
		t.Fatalf("unexpected errors: %d", otherErrs.Load())
	}
	if succeeded.Load() != maxSess {
		t.Errorf("succeeded = %d, want exactly %d", succeeded.Load(), maxSess)
	}
	if quotaErrs.Load() != goroutines-maxSess {
		t.Errorf("quotaErrs = %d, want %d", quotaErrs.Load(), goroutines-maxSess)
	}
}

func TestUploadSessionStore_LeaseCommitTakeoverAndRelease(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	st := entadapter.NewCompositeStore(client)
	now := time.Now().UTC().Truncate(time.Microsecond)

	s := newTestUploadSession("upl_lease_1", "u-lease", 2048, now)
	if err := st.CreateUploadSession(ctx, s, store.UploadQuota{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	origIdle := s.IdleExpiresAt

	// Offset mismatch returns ErrOffsetMismatch with actual received offset.
	if _, err := st.ClaimUploadLease(ctx, s.ID, store.UploadStateUploading, 512, "tok-wrong-off", time.Minute); err == nil {
		t.Fatal("expected offset mismatch error")
	} else {
		if !errors.Is(err, store.ErrOffsetMismatch) {
			t.Fatalf("got %v, want ErrOffsetMismatch", err)
		}
		var om *store.OffsetMismatchError
		if !errors.As(err, &om) || om.Received != 0 || om.Expected != 512 {
			t.Fatalf("OffsetMismatchError = %+v, want Expected=512 Received=0", om)
		}
	}

	// Claim fresh lease.
	claimed, err := st.ClaimUploadLease(ctx, s.ID, store.UploadStateUploading, 0, "tok-1", time.Minute)
	if err != nil {
		t.Fatalf("ClaimUploadLease(tok-1): %v", err)
	}
	if claimed.Takeovers != 0 || claimed.LeaseToken != "tok-1" || claimed.LeaseUntil == nil {
		t.Fatalf("unexpected claimed state: %+v", claimed)
	}
	if !claimed.IdleExpiresAt.Equal(origIdle) {
		t.Errorf("ClaimUploadLease mutated idle_expires_at: got %v, want %v", claimed.IdleExpiresAt, origIdle)
	}

	// Concurrent claim while lease is active returns ErrBusy.
	if _, err := st.ClaimUploadLease(ctx, s.ID, store.UploadStateUploading, 0, "tok-2", time.Minute); !errors.Is(err, store.ErrBusy) {
		t.Fatalf("ClaimUploadLease while busy = %v, want ErrBusy", err)
	}

	// Release with wrong token fails with ErrLeaseLost and leaves lease intact.
	if err := st.ReleaseUploadLease(ctx, s.ID, "tok-wrong"); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("ReleaseUploadLease(wrong) = %v, want ErrLeaseLost", err)
	}

	// Release with matching token clears lease and does NOT touch idle_expires_at.
	if err := st.ReleaseUploadLease(ctx, s.ID, "tok-1"); err != nil {
		t.Fatalf("ReleaseUploadLease(tok-1): %v", err)
	}
	afterRelease, err := st.GetUploadSession(ctx, s.ID)
	if err != nil {
		t.Fatalf("get after release: %v", err)
	}
	if afterRelease.LeaseToken != "" || afterRelease.LeaseUntil != nil {
		t.Errorf("expected cleared lease after release, got token=%q until=%v", afterRelease.LeaseToken, afterRelease.LeaseUntil)
	}
	if !afterRelease.IdleExpiresAt.Equal(origIdle) {
		t.Errorf("ReleaseUploadLease mutated idle_expires_at: got %v, want %v", afterRelease.IdleExpiresAt, origIdle)
	}

	// Claim again, then force lease_until into the past to test expired takeover.
	if _, err := st.ClaimUploadLease(ctx, s.ID, store.UploadStateUploading, 0, "tok-stale", time.Minute); err != nil {
		t.Fatalf("ClaimUploadLease(tok-stale): %v", err)
	}
	past := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	if err := client.UploadSession.UpdateOneID(s.ID).SetLeaseUntil(past).Exec(ctx); err != nil {
		t.Fatalf("force expired lease: %v", err)
	}

	// Takeover increments takeovers from 0 to 1.
	takenOver, err := st.ClaimUploadLease(ctx, s.ID, store.UploadStateUploading, 0, "tok-winner", time.Minute)
	if err != nil {
		t.Fatalf("ClaimUploadLease(tok-winner): %v", err)
	}
	if takenOver.Takeovers != 1 || takenOver.LeaseToken != "tok-winner" {
		t.Fatalf("takeover state = takeovers:%d token:%q, want 1 / tok-winner", takenOver.Takeovers, takenOver.LeaseToken)
	}

	// Stale writer's CommitUploadChunk is fenced and returns ErrLeaseLost.
	newIdle := now.Add(2 * time.Hour)
	if err := st.CommitUploadChunk(ctx, s.ID, "tok-stale", store.UploadChunkCommit{
		Received:      1024,
		HashState:     []byte("stale"),
		Head:          []byte("stale-head"),
		IdleExpiresAt: newIdle,
	}); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("stale CommitUploadChunk = %v, want ErrLeaseLost", err)
	}

	// Winner commits chunk 1: sets head, parts, received, idle_expires_at, clears lease.
	if err := st.CommitUploadChunk(ctx, s.ID, "tok-winner", store.UploadChunkCommit{
		Received:      1024,
		HashState:     []byte("hash-1"),
		Head:          []byte("first-512-bytes"),
		Parts:         []string{"p00000-tok-winner"},
		IdleExpiresAt: newIdle,
	}); err != nil {
		t.Fatalf("winner CommitUploadChunk 1: %v", err)
	}
	afterC1, err := st.GetUploadSession(ctx, s.ID)
	if err != nil {
		t.Fatalf("get after commit 1: %v", err)
	}
	if afterC1.Received != 1024 || afterC1.LeaseToken != "" || afterC1.LeaseUntil != nil {
		t.Fatalf("after commit 1: %+v", afterC1)
	}
	if string(afterC1.Head) != "first-512-bytes" {
		t.Errorf("Head = %q, want first-512-bytes", afterC1.Head)
	}
	if len(afterC1.Parts) != 1 || afterC1.Parts[0] != "p00000-tok-winner" {
		t.Errorf("Parts = %v, want [p00000-tok-winner]", afterC1.Parts)
	}
	if !afterC1.IdleExpiresAt.Equal(newIdle) {
		t.Errorf("IdleExpiresAt = %v, want %v", afterC1.IdleExpiresAt, newIdle)
	}

	// Commit chunk 2 with a non-empty Head must NOT overwrite existing Head (COALESCE).
	if _, err := st.ClaimUploadLease(ctx, s.ID, store.UploadStateUploading, 1024, "tok-c2", time.Minute); err != nil {
		t.Fatalf("claim chunk 2: %v", err)
	}
	newIdle2 := now.Add(3 * time.Hour)
	if err := st.CommitUploadChunk(ctx, s.ID, "tok-c2", store.UploadChunkCommit{
		Received:      2048,
		HashState:     []byte("hash-2"),
		Head:          []byte("must-not-overwrite"),
		Parts:         []string{"p00000-tok-winner", "p00001-tok-c2"},
		IdleExpiresAt: newIdle2,
	}); err != nil {
		t.Fatalf("commit chunk 2: %v", err)
	}
	afterC2, err := st.GetUploadSession(ctx, s.ID)
	if err != nil {
		t.Fatalf("get after commit 2: %v", err)
	}
	if string(afterC2.Head) != "first-512-bytes" {
		t.Errorf("Head overwritten on chunk 2: got %q, want first-512-bytes", afterC2.Head)
	}
	if afterC2.Received != 2048 || len(afterC2.Parts) != 2 {
		t.Errorf("after commit 2: received=%d parts=%v", afterC2.Received, afterC2.Parts)
	}
}

func TestUploadSessionStore_TransitionsAndStickyTerminalStates(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	st := entadapter.NewCompositeStore(client)
	now := time.Now().UTC().Truncate(time.Microsecond)

	// 1. Happy path: uploading -> finalizing (keeps lease) -> completed (clears lease, stores result).
	s := newTestUploadSession("upl_tr_complete", "u-tr", 512, now)
	if err := st.CreateUploadSession(ctx, s, store.UploadQuota{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	origIdle := s.IdleExpiresAt
	if _, err := st.ClaimUploadLease(ctx, s.ID, store.UploadStateUploading, 0, "tok-fin", time.Minute); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := st.TransitionUploadSession(ctx, s.ID, []store.UploadSessionState{store.UploadStateUploading}, store.UploadStateFinalizing, "tok-fin", nil); err != nil {
		t.Fatalf("uploading->finalizing: %v", err)
	}
	mid, err := st.GetUploadSession(ctx, s.ID)
	if err != nil {
		t.Fatalf("get mid: %v", err)
	}
	if mid.State != store.UploadStateFinalizing || mid.LeaseToken != "tok-fin" || mid.LeaseUntil == nil {
		t.Fatalf("expected finalizing with preserved lease, got %+v", mid)
	}
	// Wrong token cannot complete.
	if err := st.TransitionUploadSession(ctx, s.ID, []store.UploadSessionState{store.UploadStateFinalizing}, store.UploadStateCompleted, "tok-wrong", json.RawMessage(`{}`)); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("wrong token complete = %v, want ErrLeaseLost", err)
	}
	// Matching token completes and stores result.
	wantResult := json.RawMessage(`{"path":"dir/file.bin","size":512}`)
	if err := st.TransitionUploadSession(ctx, s.ID, []store.UploadSessionState{store.UploadStateFinalizing}, store.UploadStateCompleted, "tok-fin", wantResult); err != nil {
		t.Fatalf("finalizing->completed: %v", err)
	}
	done, err := st.GetUploadSession(ctx, s.ID)
	if err != nil {
		t.Fatalf("get done: %v", err)
	}
	if done.State != store.UploadStateCompleted || done.LeaseToken != "" || done.LeaseUntil != nil {
		t.Fatalf("expected completed with cleared lease, got %+v", done)
	}
	if !bytes.Equal(done.Result, wantResult) {
		t.Errorf("Result = %s, want %s", done.Result, wantResult)
	}
	if !done.IdleExpiresAt.Equal(origIdle) {
		t.Errorf("TransitionUploadSession mutated idle_expires_at")
	}

	// 2. Sticky terminal states: cannot transition out of completed, failed, aborted, or expired.
	if err := st.TransitionUploadSession(ctx, s.ID, []store.UploadSessionState{store.UploadStateCompleted}, store.UploadStateFailed, "", nil); !errors.Is(err, store.ErrStateChanged) {
		t.Errorf("completed->failed = %v, want ErrStateChanged", err)
	}
	if err := st.TransitionUploadSession(ctx, s.ID, []store.UploadSessionState{store.UploadStateUploading}, store.UploadStateAborted, "", nil); !errors.Is(err, store.ErrStateChanged) {
		t.Errorf("uploading->aborted on completed = %v, want ErrStateChanged", err)
	}
	if _, err := st.ClaimUploadLease(ctx, s.ID, store.UploadStateUploading, 0, "tok-x", time.Minute); !errors.Is(err, store.ErrStateChanged) {
		t.Errorf("ClaimUploadLease on completed = %v, want ErrStateChanged", err)
	}

	// 3. Abort with empty token fails with ErrBusy while lease is held, succeeds once released.
	s2 := newTestUploadSession("upl_tr_abort", "u-tr", 512, now)
	if err := st.CreateUploadSession(ctx, s2, store.UploadQuota{}); err != nil {
		t.Fatalf("create s2: %v", err)
	}
	if _, err := st.ClaimUploadLease(ctx, s2.ID, store.UploadStateUploading, 0, "tok-a", time.Minute); err != nil {
		t.Fatalf("claim s2: %v", err)
	}
	if err := st.TransitionUploadSession(ctx, s2.ID, []store.UploadSessionState{store.UploadStateUploading}, store.UploadStateAborted, "", nil); !errors.Is(err, store.ErrBusy) {
		t.Fatalf("abort while leased = %v, want ErrBusy", err)
	}
	if err := st.ReleaseUploadLease(ctx, s2.ID, "tok-a"); err != nil {
		t.Fatalf("release s2: %v", err)
	}
	if err := st.TransitionUploadSession(ctx, s2.ID, []store.UploadSessionState{store.UploadStateUploading}, store.UploadStateAborted, "", nil); err != nil {
		t.Fatalf("abort after release: %v", err)
	}
	// Aborted is sticky.
	if err := st.TransitionUploadSession(ctx, s2.ID, []store.UploadSessionState{store.UploadStateAborted}, store.UploadStateExpired, "", nil); !errors.Is(err, store.ErrStateChanged) {
		t.Errorf("aborted->expired = %v, want ErrStateChanged", err)
	}
}

func TestUploadSessionStore_ListExpiredAndDelete(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	st := entadapter.NewCompositeStore(client)
	now := time.Now().UTC().Truncate(time.Microsecond)

	// 1. uploading: idle-expired, free lease -> INCLUDED
	sIdle := newTestUploadSession("upl_exp_idle", "u-e", 100, now)
	sIdle.IdleExpiresAt = now.Add(-time.Minute)
	if err := st.CreateUploadSession(ctx, sIdle, store.UploadQuota{}); err != nil {
		t.Fatalf("create sIdle: %v", err)
	}

	// 2. uploading: hard-expired, free lease -> INCLUDED
	sHard := newTestUploadSession("upl_exp_hard", "u-e", 100, now)
	sHard.HardExpiresAt = now.Add(-time.Minute)
	if err := st.CreateUploadSession(ctx, sHard, store.UploadQuota{}); err != nil {
		t.Fatalf("create sHard: %v", err)
	}

	// 3. uploading: idle-expired, ACTIVE lease -> EXCLUDED
	sLeased := newTestUploadSession("upl_exp_leased", "u-e", 100, now)
	sLeased.IdleExpiresAt = now.Add(-time.Minute)
	if err := st.CreateUploadSession(ctx, sLeased, store.UploadQuota{}); err != nil {
		t.Fatalf("create sLeased: %v", err)
	}
	if _, err := st.ClaimUploadLease(ctx, sLeased.ID, store.UploadStateUploading, 0, "tok-l", time.Hour); err != nil {
		t.Fatalf("claim sLeased: %v", err)
	}

	// 4. uploading: fresh, free lease -> EXCLUDED
	sFresh := newTestUploadSession("upl_exp_fresh", "u-e", 100, now)
	if err := st.CreateUploadSession(ctx, sFresh, store.UploadQuota{}); err != nil {
		t.Fatalf("create sFresh: %v", err)
	}

	// 5. finalizing: stuck > 1h past lease_until -> INCLUDED
	sStuckFin := newTestUploadSession("upl_exp_stuck_fin", "u-e", 100, now)
	if err := st.CreateUploadSession(ctx, sStuckFin, store.UploadQuota{}); err != nil {
		t.Fatalf("create sStuckFin: %v", err)
	}
	if err := client.UploadSession.UpdateOneID(sStuckFin.ID).
		SetState(uploadsession.StateFinalizing).
		SetLeaseToken("tok-stuck").
		SetLeaseUntil(now.Add(-2 * time.Hour)).
		Exec(ctx); err != nil {
		t.Fatalf("update sStuckFin: %v", err)
	}

	// 6. finalizing: lease expired 10m ago (< 1h) -> EXCLUDED
	sRecentFin := newTestUploadSession("upl_exp_recent_fin", "u-e", 100, now)
	if err := st.CreateUploadSession(ctx, sRecentFin, store.UploadQuota{}); err != nil {
		t.Fatalf("create sRecentFin: %v", err)
	}
	if err := client.UploadSession.UpdateOneID(sRecentFin.ID).
		SetState(uploadsession.StateFinalizing).
		SetLeaseToken("tok-rec").
		SetLeaseUntil(now.Add(-10 * time.Minute)).
		Exec(ctx); err != nil {
		t.Fatalf("update sRecentFin: %v", err)
	}

	// 7. terminal (completed): updated_at > 15m ago -> INCLUDED
	sOldTerm := newTestUploadSession("upl_exp_old_term", "u-e", 100, now)
	if err := st.CreateUploadSession(ctx, sOldTerm, store.UploadQuota{}); err != nil {
		t.Fatalf("create sOldTerm: %v", err)
	}
	if err := client.UploadSession.UpdateOneID(sOldTerm.ID).
		SetState(uploadsession.StateCompleted).
		SetUpdatedAt(now.Add(-20 * time.Minute)).
		Exec(ctx); err != nil {
		t.Fatalf("update sOldTerm: %v", err)
	}

	// 8. terminal (completed): updated_at 5m ago (< 15m) -> EXCLUDED
	sNewTerm := newTestUploadSession("upl_exp_new_term", "u-e", 100, now)
	if err := st.CreateUploadSession(ctx, sNewTerm, store.UploadQuota{}); err != nil {
		t.Fatalf("create sNewTerm: %v", err)
	}
	if err := client.UploadSession.UpdateOneID(sNewTerm.ID).
		SetState(uploadsession.StateCompleted).
		SetUpdatedAt(now.Add(-5 * time.Minute)).
		Exec(ctx); err != nil {
		t.Fatalf("update sNewTerm: %v", err)
	}

	got, err := st.ListExpiredUploadSessions(ctx, now, 50)
	if err != nil {
		t.Fatalf("ListExpiredUploadSessions: %v", err)
	}
	gotIDs := make(map[string]bool, len(got))
	for _, s := range got {
		gotIDs[s.ID] = true
	}
	wantIDs := []string{sIdle.ID, sHard.ID, sStuckFin.ID, sOldTerm.ID}
	if len(gotIDs) != len(wantIDs) {
		t.Fatalf("ListExpiredUploadSessions returned %d rows (%v), want %d (%v)", len(gotIDs), gotIDs, len(wantIDs), wantIDs)
	}
	for _, id := range wantIDs {
		if !gotIDs[id] {
			t.Errorf("missing expected expired session %q", id)
		}
	}

	// Limit is respected.
	limited, err := st.ListExpiredUploadSessions(ctx, now, 2)
	if err != nil {
		t.Fatalf("ListExpiredUploadSessions(limit=2): %v", err)
	}
	if len(limited) != 2 {
		t.Errorf("len(limited) = %d, want 2", len(limited))
	}

	// DeleteUploadSession removes row and returns ErrNotFound on repeat.
	if err := st.DeleteUploadSession(ctx, sOldTerm.ID); err != nil {
		t.Fatalf("DeleteUploadSession: %v", err)
	}
	if err := st.DeleteUploadSession(ctx, sOldTerm.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("repeat DeleteUploadSession = %v, want ErrNotFound", err)
	}
}
