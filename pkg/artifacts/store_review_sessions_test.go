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

package artifacts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

// forEachSessionBackend is forEachBackend for the review-session store
// tests, whose Postgres path carries the serialized cap: without
// SCION_TEST_POSTGRES_DSN its postgres subtest SKIPS visibly instead of
// being left out, so a run without Postgres can never read as covering it.
// CI runs it with the DSN set in the launch-store-postgres-others job
// (make test-artifacts-postgres, which fails on any skip).
func forEachSessionBackend(t *testing.T, fn func(t *testing.T, db *sql.DB, st Store, reopen func() *sql.DB)) {
	t.Helper()
	forEachBackend(t, fn)
	if os.Getenv("SCION_TEST_POSTGRES_DSN") == "" {
		t.Run("postgres", func(t *testing.T) {
			t.Skip("SCION_TEST_POSTGRES_DSN is not set: the Postgres path of this test did not run")
		})
	}
}

// session is a review session of artifact a by user ref with sid.
func session(a *Artifact, ref, sid string, base int, dirty bool) ReviewSession {
	return ReviewSession{ArtifactID: a.ID, PrincipalKind: PrincipalKindUser, PrincipalRef: ref, SessionID: sid, BaseSeq: base, Dirty: dirty}
}

// sid returns a well-formed session id numbered n.
func sid(n int) string { return fmt.Sprintf("session-%016d", n) }

func listAll(t *testing.T, st Store, artifactID string) []ReviewSession {
	t.Helper()
	got, err := st.ListOpenReviewSessions(context.Background(), artifactID, time.Time{})
	if err != nil {
		t.Fatalf("ListOpenReviewSessions: %v", err)
	}
	return got
}

func TestReviewSessionStoreUpsert(t *testing.T) {
	forEachSessionBackend(t, func(t *testing.T, _ *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		a, _, _, _ := seedArtifact(t, st, "")
		t0 := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
		if err := st.TouchReviewSession(ctx, session(a, "u1", sid(1), 1, false), false, t0, 8); err != nil {
			t.Fatalf("first beat: %v", err)
		}
		got := listAll(t, st, a.ID)
		if len(got) != 1 || !got[0].StartedAt.Equal(t0) || !got[0].LastSeenAt.Equal(t0) || got[0].LastEditAt != nil || got[0].Dirty {
			t.Fatalf("after first beat: %+v", got)
		}
		t1 := t0.Add(time.Minute)
		if err := st.TouchReviewSession(ctx, session(a, "u1", sid(1), 1, true), true, t1, 8); err != nil {
			t.Fatalf("edit beat: %v", err)
		}
		t2 := t1.Add(time.Minute)
		// An unedited beat keeps last_edit_at, moves last_seen_at and takes
		// the new base and dirty.
		if err := st.TouchReviewSession(ctx, session(a, "u1", sid(1), 2, false), false, t2, 8); err != nil {
			t.Fatalf("plain beat: %v", err)
		}
		got = listAll(t, st, a.ID)
		if len(got) != 1 {
			t.Fatalf("one tab, %d rows", len(got))
		}
		g := got[0]
		if !g.StartedAt.Equal(t0) || !g.LastSeenAt.Equal(t2) || g.LastEditAt == nil || !g.LastEditAt.Equal(t1) || g.Dirty || g.BaseSeq != 2 {
			t.Errorf("after three beats: %+v (edit %v)", g, g.LastEditAt)
		}
		// seenAfter filters by last_seen_at.
		if got, _ := st.ListOpenReviewSessions(ctx, a.ID, t2.Add(time.Second)); len(got) != 0 {
			t.Errorf("seenAfter past the last beat listed %d", len(got))
		}
	})
}

func TestReviewSessionStorePrincipalScopedKey(t *testing.T) {
	forEachSessionBackend(t, func(t *testing.T, _ *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		a, _, _, _ := seedArtifact(t, st, "")
		now := time.Now().UTC()
		// Two principals choosing the same session id get two rows.
		for _, ref := range []string{"u1", "u2"} {
			if err := st.TouchReviewSession(ctx, session(a, ref, sid(1), 1, ref == "u1"), false, now, 8); err != nil {
				t.Fatal(err)
			}
		}
		agent := session(a, "u1", sid(1), 1, false)
		agent.PrincipalKind = PrincipalKindAgent
		if err := st.TouchReviewSession(ctx, agent, false, now, 8); err != nil {
			t.Fatal(err)
		}
		if got := listAll(t, st, a.ID); len(got) != 3 {
			t.Fatalf("%d rows, want 3", len(got))
		}
		// Ending u2's session leaves u1's and the agent's.
		if ended, err := st.EndReviewSession(ctx, a.ID, PrincipalKindUser, "u2", sid(1)); err != nil || !ended {
			t.Fatalf("end = %v, %v; want a row deleted", ended, err)
		}
		// Idempotent: a second end deletes nothing and is not an error.
		if ended, err := st.EndReviewSession(ctx, a.ID, PrincipalKindUser, "u2", sid(1)); err != nil || ended {
			t.Fatalf("second end = %v, %v; want nothing deleted", ended, err)
		}
		got := listAll(t, st, a.ID)
		if len(got) != 2 {
			t.Fatalf("%d rows after ending u2's, want 2", len(got))
		}
		for _, g := range got {
			if g.PrincipalRef != "u1" {
				t.Errorf("row of %s:%s survived", g.PrincipalKind, g.PrincipalRef)
			}
		}
	})
}

func TestReviewSessionStoreCap(t *testing.T) {
	forEachSessionBackend(t, func(t *testing.T, _ *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		a, _, _, _ := seedArtifact(t, st, "")
		now := time.Now().UTC()
		// A session last seen outside the live window does not count.
		if err := st.TouchReviewSession(ctx, session(a, "u1", sid(99), 1, false), false, now.Add(-ReviewLiveWindow-time.Second), 3); err != nil {
			t.Fatal(err)
		}
		for i := 1; i <= 3; i++ {
			if err := st.TouchReviewSession(ctx, session(a, "u1", sid(i), 1, false), false, now, 3); err != nil {
				t.Fatalf("session %d: %v", i, err)
			}
		}
		if err := st.TouchReviewSession(ctx, session(a, "u1", sid(4), 1, false), false, now, 3); !errors.Is(err, ErrTooManyReviewSessions) {
			t.Fatalf("fourth live session: %v, want ErrTooManyReviewSessions", err)
		}
		// The refused beat recorded nothing.
		if got := listAll(t, st, a.ID); len(got) != 4 {
			t.Errorf("%d rows, want 4 (3 live, 1 old)", len(got))
		}
		// An existing session beats on.
		if err := st.TouchReviewSession(ctx, session(a, "u1", sid(2), 1, true), true, now.Add(time.Second), 3); err != nil {
			t.Errorf("existing session refused: %v", err)
		}
		// The old session coming back is a beat of an existing row: allowed.
		if err := st.TouchReviewSession(ctx, session(a, "u1", sid(99), 1, false), false, now.Add(time.Second), 3); err != nil {
			t.Errorf("returning session refused: %v", err)
		}
		// The cap is per principal.
		if err := st.TouchReviewSession(ctx, session(a, "u2", sid(1), 1, false), false, now, 3); err != nil {
			t.Errorf("another principal refused: %v", err)
		}
	})
}

// TestReviewSessionStoreCapConcurrent: with the principal one below the
// cap, two beats opening new sessions at once are serialized, so exactly
// one is recorded. The first is held inside its transaction, after its
// upsert and before its count, while the second starts.
func TestReviewSessionStoreCapConcurrent(t *testing.T) {
	type heldKey struct{}
	forEachSessionBackend(t, func(t *testing.T, _ *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		a, _, _, _ := seedArtifact(t, st, "")
		const limit = 4
		now := time.Now().UTC()
		for i := 1; i < limit; i++ {
			if err := st.TouchReviewSession(ctx, session(a, "u1", sid(i), 1, false), false, now, limit); err != nil {
				t.Fatal(err)
			}
		}
		reached := make(chan struct{})
		release := make(chan struct{})
		reviewSessionTouchHook = func(ctx context.Context) {
			if ctx.Value(heldKey{}) != nil {
				close(reached)
				<-release
			}
		}
		t.Cleanup(func() { reviewSessionTouchHook = nil })
		errs := make([]error, 2)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			errs[0] = st.TouchReviewSession(context.WithValue(ctx, heldKey{}, true), session(a, "u1", sid(100), 1, false), false, now.Add(time.Millisecond), limit)
		}()
		<-reached
		go func() {
			defer wg.Done()
			errs[1] = st.TouchReviewSession(ctx, session(a, "u1", sid(101), 1, false), false, now.Add(2*time.Millisecond), limit)
		}()
		// Give the second beat time to reach the lock it must wait on.
		time.Sleep(200 * time.Millisecond)
		close(release)
		wg.Wait()
		ok, refused := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrTooManyReviewSessions):
				refused++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}
		if ok != 1 || refused != 1 {
			t.Errorf("concurrent beats at cap-1: %d recorded, %d refused; want 1 and 1 (%v)", ok, refused, errs)
		}
		if got := listAll(t, st, a.ID); len(got) != limit {
			t.Errorf("%d rows, want %d", len(got), limit)
		}
	})
}

func TestReviewSessionStoreReap(t *testing.T) {
	forEachSessionBackend(t, func(t *testing.T, _ *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		a, _, _, _ := seedArtifact(t, st, "")
		now := time.Now().UTC()
		for i, seen := range []time.Duration{2 * time.Hour, 90 * time.Minute, 61 * time.Minute, 10 * time.Minute} {
			if err := st.TouchReviewSession(ctx, session(a, "u1", sid(i), 1, false), false, now.Add(-seen), 8); err != nil {
				t.Fatal(err)
			}
		}
		cutoff := now.Add(-ReviewSessionRetention)
		if n, err := st.ReapReviewSessions(ctx, cutoff, 2); err != nil || n != 2 {
			t.Fatalf("first pass = %d, %v; want 2 (the limit)", n, err)
		}
		if n, err := st.ReapReviewSessions(ctx, cutoff, 10); err != nil || n != 1 {
			t.Fatalf("second pass = %d, %v; want 1", n, err)
		}
		if n, err := st.ReapReviewSessions(ctx, cutoff, 0); err != nil || n != 0 {
			t.Fatalf("zero limit = %d, %v", n, err)
		}
		got := listAll(t, st, a.ID)
		if len(got) != 1 || got[0].SessionID != sid(3) {
			t.Errorf("left %+v, want only the recent session", got)
		}
	})
}

func TestReviewSessionStoreDeletedWithArtifact(t *testing.T) {
	forEachSessionBackend(t, func(t *testing.T, _ *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		a, _, _, _ := seedArtifact(t, st, "")
		other, _, _, _ := seedArtifact(t, st, "")
		now := time.Now().UTC()
		for _, art := range []*Artifact{a, other} {
			if err := st.TouchReviewSession(ctx, session(art, "u1", sid(1), 1, true), true, now, 8); err != nil {
				t.Fatal(err)
			}
		}
		actor := &Actor{Kind: PrincipalKindUser, Ref: "u0", Authority: logAuthorityOwner}
		if _, err := st.DeleteArtifact(ctx, a.ID, DeleteOptions{Actor: actor, Now: now}); err != nil {
			t.Fatalf("DeleteArtifact: %v", err)
		}
		if got := listAll(t, st, a.ID); len(got) != 0 {
			t.Errorf("%d sessions survived the artifact's delete", len(got))
		}
		if got := listAll(t, st, other.ID); len(got) != 1 {
			t.Errorf("another artifact's session was removed")
		}
	})
}

// TestMigration0013_ReviewSessions: a database migrated through 0011 (0012
// is the sponsor backfill's ledger name, not a DDL step) migrates to the
// review-session table, beats are recorded, and a second Init is a no-op.
func TestMigration0013_ReviewSessions(t *testing.T) {
	if i := migrationIndex(migrationReviewSessions); i < 1 || migrations[i-1].name != migrationDeliveryRedemptions {
		t.Fatalf("migration %q is not right after %q", migrationReviewSessions, migrationDeliveryRedemptions)
	}
	if got := migrationReviewSessions; got != "0013_review_sessions" {
		t.Fatalf("migrationReviewSessions = %q", got)
	}
	for _, b := range testBackends() {
		t.Run(b.name, func(t *testing.T) {
			db, _ := b.open(t)
			st := NewStore(db, b.driver).(*sqlStore)
			migrateTo(t, db, st, migrationReviewSessions)
			ctx := context.Background()
			if err := st.Init(ctx); err != nil {
				t.Fatalf("Init over 0011: %v", err)
			}
			a, _, _, _ := seedArtifact(t, st, "")
			if err := st.TouchReviewSession(ctx, session(a, "u1", sid(1), 1, true), true, time.Now(), 8); err != nil {
				t.Fatalf("beat after migration: %v", err)
			}
			if got := listAll(t, st, a.ID); len(got) != 1 || !got[0].Dirty {
				t.Errorf("after migration: %+v", got)
			}
			if err := st.Init(ctx); err != nil {
				t.Errorf("Init again: %v", err)
			}
		})
	}
	if os.Getenv("SCION_TEST_POSTGRES_DSN") == "" {
		t.Run("postgres", func(t *testing.T) {
			t.Skip("SCION_TEST_POSTGRES_DSN is not set: the Postgres migration did not run")
		})
	}
}
