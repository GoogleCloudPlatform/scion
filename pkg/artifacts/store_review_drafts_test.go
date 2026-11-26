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
	"os"
	"slices"
	"sync"
	"testing"
	"time"
)

// forEachDraftBackend runs fn on SQLite always and on Postgres when
// SCION_TEST_POSTGRES_DSN is set. Without SCION_TEST_POSTGRES_DSN its
// postgres subtest skips visibly instead of being omitted, matching
// forEachSessionBackend (CI's make test-artifacts-postgres sets the DSN and
// fails on any skip).
func forEachDraftBackend(t *testing.T, fn func(t *testing.T, db *sql.DB, st Store, reopen func() *sql.DB)) {
	t.Helper()
	forEachBackend(t, fn)
	if os.Getenv("SCION_TEST_POSTGRES_DSN") == "" {
		t.Run("postgres", func(t *testing.T) {
			t.Skip("SCION_TEST_POSTGRES_DSN is not set: the Postgres path of this test did not run")
		})
	}
}

func testDraft(a *Artifact, kind, ref string, base int, entry, body, note string) ReviewDraft {
	return ReviewDraft{
		ArtifactID:    a.ID,
		PrincipalKind: kind,
		PrincipalRef:  ref,
		BaseSeq:       base,
		EntryPath:     entry,
		Body:          body,
		Note:          note,
	}
}

func TestReviewDraftStorePutGetDelete(t *testing.T) {
	forEachDraftBackend(t, func(t *testing.T, _ *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		a, _, _, _ := seedArtifact(t, st, "")

		// Absent draft returns ErrNotFound.
		if _, err := st.GetReviewDraft(ctx, a.ID, PrincipalKindUser, "u1"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("GetReviewDraft when absent = %v, want ErrNotFound", err)
		}

		// Conditional update when no row exists returns conflict with Rev=0.
		t0 := time.Date(2026, 11, 19, 10, 0, 0, 123456000, time.UTC)
		_, err := st.PutReviewDraft(ctx, testDraft(a, PrincipalKindUser, "u1", 1, "doc.md", "v1", ""), 1, t0, 200)
		var ce *DraftConflictError
		if !errors.Is(err, ErrDraftConflict) || !errors.As(err, &ce) || ce.Rev != 0 || ce.BaseSeq != 0 || !ce.SavedAt.IsZero() {
			t.Fatalf("PutReviewDraft(expectRev=1) on empty = %v (%+v), want ErrDraftConflict with Rev=0", err, ce)
		}

		// Insert at expectRev=0 stores byte-exact UTF-8 (CRLF, emoji, non-NFC
		// combining sequence) and empty note.
		rawBody := "# Heading\r\n\nLine with {++mark++}{>>comment 🎉<<} and e\u0301\n"
		clientTime := time.Date(2026, 11, 19, 9, 59, 58, 500000000, time.UTC)
		in := testDraft(a, PrincipalKindUser, "u1", 1, "doc.md", rawBody, "")
		in.ClientSavedAt = &clientTime
		d1, err := st.PutReviewDraft(ctx, in, 0, t0, 200)
		if err != nil {
			t.Fatalf("first PutReviewDraft: %v", err)
		}
		if d1.Rev != 1 || d1.Body != rawBody || d1.Note != "" || !d1.CreatedAt.Equal(t0) || !d1.UpdatedAt.Equal(t0) {
			t.Fatalf("inserted draft = %+v", d1)
		}
		if d1.ClientSavedAt == nil || !d1.ClientSavedAt.Equal(clientTime) || d1.CarriedBaseSeq != nil || d1.CarriedBody != nil {
			t.Fatalf("inserted draft optional fields = %+v", d1)
		}

		got1, err := st.GetReviewDraft(ctx, a.ID, PrincipalKindUser, "u1")
		if err != nil {
			t.Fatalf("GetReviewDraft: %v", err)
		}
		if got1.Rev != 1 || got1.Body != rawBody || got1.Note != "" || !got1.CreatedAt.Equal(t0) || !got1.UpdatedAt.Equal(t0) {
			t.Fatalf("GetReviewDraft mismatch: %+v", got1)
		}

		// Second insert (expectRev=0) conflicts with Rev=1 and carries no body.
		_, err = st.PutReviewDraft(ctx, testDraft(a, PrincipalKindUser, "u1", 1, "doc.md", "overwrite", "n"), 0, t0.Add(time.Second), 200)
		ce = nil
		if !errors.Is(err, ErrDraftConflict) || !errors.As(err, &ce) || ce.Rev != 1 || !ce.SavedAt.Equal(t0) || ce.BaseSeq != 1 {
			t.Fatalf("duplicate insert = %v (%+v), want conflict Rev=1", err, ce)
		}

		// Update with expectRev=1 advances to Rev=2, preserves CreatedAt, and
		// stores carried text.
		t1 := t0.Add(2 * time.Minute)
		cBase := 1
		cBody := "earlier {>>old<<}"
		upd := testDraft(a, PrincipalKindUser, "u1", 2, "doc.md", "fresh v2 {++add++}", "summary note")
		upd.CarriedBaseSeq = &cBase
		upd.CarriedBody = &cBody
		d2, err := st.PutReviewDraft(ctx, upd, 1, t1, 200)
		if err != nil {
			t.Fatalf("update PutReviewDraft: %v", err)
		}
		if d2.Rev != 2 || !d2.CreatedAt.Equal(t0) || !d2.UpdatedAt.Equal(t1) || d2.Note != "summary note" {
			t.Fatalf("updated draft = %+v", d2)
		}
		if d2.CarriedBaseSeq == nil || *d2.CarriedBaseSeq != 1 || d2.CarriedBody == nil || *d2.CarriedBody != cBody {
			t.Fatalf("carried fields = %v, %v", d2.CarriedBaseSeq, d2.CarriedBody)
		}

		// Stale update (expectRev=1 when stored is 2) conflicts and leaves rev 2 intact.
		_, err = st.PutReviewDraft(ctx, testDraft(a, PrincipalKindUser, "u1", 2, "doc.md", "stale", ""), 1, t1.Add(time.Second), 200)
		ce = nil
		if !errors.Is(err, ErrDraftConflict) || !errors.As(err, &ce) || ce.Rev != 2 || !ce.SavedAt.Equal(t1) || ce.BaseSeq != 2 {
			t.Fatalf("stale update = %v (%+v), want conflict Rev=2", err, ce)
		}

		// Clearing carried text on next update sets both carried columns back to nil.
		t2 := t1.Add(time.Minute)
		d3, err := st.PutReviewDraft(ctx, testDraft(a, PrincipalKindUser, "u1", 2, "doc.md", "dismissed carried", "n2"), 2, t2, 200)
		if err != nil {
			t.Fatalf("clear carried PutReviewDraft: %v", err)
		}
		if d3.Rev != 3 || d3.CarriedBaseSeq != nil || d3.CarriedBody != nil {
			t.Fatalf("cleared carried = %+v", d3)
		}

		// Conditional DELETE with wrong rev conflicts and keeps the row.
		deleted, err := st.DeleteReviewDraft(ctx, a.ID, PrincipalKindUser, "u1", 2)
		ce = nil
		if deleted || !errors.Is(err, ErrDraftConflict) || !errors.As(err, &ce) || ce.Rev != 3 || !ce.SavedAt.Equal(t2) || ce.BaseSeq != 2 {
			t.Fatalf("stale DeleteReviewDraft = %v, %v (%+v); want conflict Rev=3", deleted, err, ce)
		}
		if still, err := st.GetReviewDraft(ctx, a.ID, PrincipalKindUser, "u1"); err != nil || still.Rev != 3 {
			t.Fatalf("row after stale delete = %+v, %v", still, err)
		}

		// Conditional DELETE with matching rev removes the row.
		if deleted, err := st.DeleteReviewDraft(ctx, a.ID, PrincipalKindUser, "u1", 3); err != nil || !deleted {
			t.Fatalf("matching DeleteReviewDraft = %v, %v; want true, nil", deleted, err)
		}
		// Conditional DELETE when row is already absent is idempotent (false, nil).
		if deleted, err := st.DeleteReviewDraft(ctx, a.ID, PrincipalKindUser, "u1", 3); err != nil || deleted {
			t.Fatalf("repeat conditional DeleteReviewDraft = %v, %v; want false, nil", deleted, err)
		}
		// Unconditional DELETE when absent is idempotent too.
		if deleted, err := st.DeleteReviewDraft(ctx, a.ID, PrincipalKindUser, "u1", 0); err != nil || deleted {
			t.Fatalf("unconditional absent DeleteReviewDraft = %v, %v; want false, nil", deleted, err)
		}
	})
}

func TestReviewDraftStorePrincipalScopedKey(t *testing.T) {
	forEachDraftBackend(t, func(t *testing.T, _ *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		a, _, _, _ := seedArtifact(t, st, "")
		now := time.Now().UTC().Truncate(time.Microsecond)

		// user:u1, user:u2 and agent:u1 on the same artifact each get their own row.
		for _, tc := range []struct {
			kind, ref, body string
		}{
			{PrincipalKindUser, "u1", "user-1 draft"},
			{PrincipalKindUser, "u2", "user-2 draft"},
			{PrincipalKindAgent, "u1", "agent-u1 draft"},
		} {
			d, err := st.PutReviewDraft(ctx, testDraft(a, tc.kind, tc.ref, 1, "doc.md", tc.body, ""), 0, now, 200)
			if err != nil || d.Rev != 1 {
				t.Fatalf("put %s:%s = %+v, %v", tc.kind, tc.ref, d, err)
			}
		}

		// Deleting user:u2 leaves user:u1 and agent:u1 untouched.
		if deleted, err := st.DeleteReviewDraft(ctx, a.ID, PrincipalKindUser, "u2", 0); err != nil || !deleted {
			t.Fatalf("delete u2 = %v, %v", deleted, err)
		}
		u1, err := st.GetReviewDraft(ctx, a.ID, PrincipalKindUser, "u1")
		if err != nil || u1.Body != "user-1 draft" {
			t.Fatalf("user:u1 = %+v, %v", u1, err)
		}
		ag1, err := st.GetReviewDraft(ctx, a.ID, PrincipalKindAgent, "u1")
		if err != nil || ag1.Body != "agent-u1 draft" {
			t.Fatalf("agent:u1 = %+v, %v", ag1, err)
		}
		if _, err := st.GetReviewDraft(ctx, a.ID, PrincipalKindUser, "u2"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("user:u2 after delete = %v, want ErrNotFound", err)
		}
	})
}

func TestReviewDraftStoreCap(t *testing.T) {
	forEachDraftBackend(t, func(t *testing.T, _ *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		const limit = 3
		now := time.Now().UTC()
		var arts []*Artifact
		for i := 0; i <= limit; i++ {
			a, _, _, _ := seedArtifact(t, st, "")
			arts = append(arts, a)
		}
		for i := 0; i < limit; i++ {
			if _, err := st.PutReviewDraft(ctx, testDraft(arts[i], PrincipalKindUser, "u1", 1, "doc.md", "b", ""), 0, now, limit); err != nil {
				t.Fatalf("draft %d: %v", i+1, err)
			}
		}
		// One past the cap is refused and leaves nothing behind.
		if _, err := st.PutReviewDraft(ctx, testDraft(arts[limit], PrincipalKindUser, "u1", 1, "doc.md", "over", ""), 0, now, limit); !errors.Is(err, ErrTooManyDrafts) {
			t.Fatalf("past cap = %v, want ErrTooManyDrafts", err)
		}
		if _, err := st.GetReviewDraft(ctx, arts[limit].ID, PrincipalKindUser, "u1"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("refused insert persisted: %v", err)
		}
		// Updating an existing draft at the cap succeeds without changing count.
		if upd, err := st.PutReviewDraft(ctx, testDraft(arts[0], PrincipalKindUser, "u1", 1, "doc.md", "updated", ""), 1, now.Add(time.Second), limit); err != nil || upd.Rev != 2 {
			t.Fatalf("update at cap = %+v, %v", upd, err)
		}
		// Another principal has its own cap.
		if _, err := st.PutReviewDraft(ctx, testDraft(arts[limit], PrincipalKindUser, "u2", 1, "doc.md", "ok", ""), 0, now, limit); err != nil {
			t.Fatalf("other principal at u1's cap: %v", err)
		}
	})
}

// TestReviewDraftStoreCapConcurrent verifies C3 on both SQL dialects: when a
// principal is at limit - 1 drafts and two inserts on distinct artifacts race,
// per-principal serialization ensures exactly one insert commits and the other
// returns ErrTooManyDrafts.
func TestReviewDraftStoreCapConcurrent(t *testing.T) {
	type heldKey struct{}
	forEachDraftBackend(t, func(t *testing.T, db *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		const limit = 4
		now := time.Now().UTC()
		for i := 0; i < limit-1; i++ {
			a, _, _, _ := seedArtifact(t, st, "")
			if _, err := st.PutReviewDraft(ctx, testDraft(a, PrincipalKindUser, "u1", 1, "doc.md", "seed", ""), 0, now, limit); err != nil {
				t.Fatal(err)
			}
		}
		raceA, _, _, _ := seedArtifact(t, st, "")
		raceB, _, _, _ := seedArtifact(t, st, "")

		reached := make(chan struct{})
		release := make(chan struct{})
		reviewDraftPutHook = func(ctx context.Context) {
			if ctx.Value(heldKey{}) != nil {
				close(reached)
				<-release
			}
		}
		t.Cleanup(func() { reviewDraftPutHook = nil })

		errs := make([]error, 2)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, errs[0] = st.PutReviewDraft(
				context.WithValue(ctx, heldKey{}, true),
				testDraft(raceA, PrincipalKindUser, "u1", 1, "doc.md", "race-a", ""),
				0, now.Add(time.Millisecond), limit,
			)
		}()
		<-reached
		go func() {
			defer wg.Done()
			_, errs[1] = st.PutReviewDraft(
				ctx,
				testDraft(raceB, PrincipalKindUser, "u1", 1, "doc.md", "race-b", ""),
				0, now.Add(2*time.Millisecond), limit,
			)
		}()
		// Give the second insert time to block on the advisory / write lock.
		time.Sleep(200 * time.Millisecond)
		close(release)
		wg.Wait()

		ok, refused := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrTooManyDrafts):
				refused++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}
		if ok != 1 || refused != 1 {
			t.Errorf("concurrent inserts at cap-1: %d recorded, %d refused; want 1 and 1 (%v)", ok, refused, errs)
		}
		var total int
		q := st.(*sqlStore).rebind(`SELECT COUNT(*) FROM artifact_review_draft WHERE principal_kind = ? AND principal_ref = ?`)
		if err := db.QueryRowContext(ctx, q, PrincipalKindUser, "u1").Scan(&total); err != nil {
			t.Fatal(err)
		}
		if total != limit {
			t.Errorf("%d rows stored for u1, want %d", total, limit)
		}
	})
}

func TestReviewDraftStoreReap(t *testing.T) {
	forEachDraftBackend(t, func(t *testing.T, _ *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		now := time.Now().UTC()

		// Three untouched >30d drafts, one created >30d ago but saved 5d ago,
		// and one saved 10m ago.
		var arts []*Artifact
		for i := 0; i < 5; i++ {
			a, _, _, _ := seedArtifact(t, st, "")
			arts = append(arts, a)
		}
		for i, age := range []time.Duration{45 * 24 * time.Hour, 35 * 24 * time.Hour, 31 * 24 * time.Hour} {
			if _, err := st.PutReviewDraft(ctx, testDraft(arts[i], PrincipalKindUser, "u1", 1, "doc.md", "old", ""), 0, now.Add(-age), 200); err != nil {
				t.Fatal(err)
			}
		}
		// Created 40 days ago, updated 5 days ago -> must survive because TTL is
		// measured from updated_at.
		if _, err := st.PutReviewDraft(ctx, testDraft(arts[3], PrincipalKindUser, "u1", 1, "doc.md", "created-old", ""), 0, now.Add(-40*24*time.Hour), 200); err != nil {
			t.Fatal(err)
		}
		if _, err := st.PutReviewDraft(ctx, testDraft(arts[3], PrincipalKindUser, "u1", 1, "doc.md", "saved-recently", ""), 1, now.Add(-5*24*time.Hour), 200); err != nil {
			t.Fatal(err)
		}
		if _, err := st.PutReviewDraft(ctx, testDraft(arts[4], PrincipalKindUser, "u1", 1, "doc.md", "fresh", ""), 0, now.Add(-10*time.Minute), 200); err != nil {
			t.Fatal(err)
		}

		cutoff := now.Add(-30 * 24 * time.Hour)
		if n, err := st.ReapReviewDrafts(ctx, cutoff, 2); err != nil || n != 2 {
			t.Fatalf("first reap pass = %d, %v; want 2 (batch limit)", n, err)
		}
		if n, err := st.ReapReviewDrafts(ctx, cutoff, 10); err != nil || n != 1 {
			t.Fatalf("second reap pass = %d, %v; want 1", n, err)
		}
		if n, err := st.ReapReviewDrafts(ctx, cutoff, 0); err != nil || n != 0 {
			t.Fatalf("zero-limit reap = %d, %v; want 0", n, err)
		}
		for i := 0; i < 3; i++ {
			if _, err := st.GetReviewDraft(ctx, arts[i].ID, PrincipalKindUser, "u1"); !errors.Is(err, ErrNotFound) {
				t.Errorf("expired draft %d survived: %v", i, err)
			}
		}
		for _, idx := range []int{3, 4} {
			if _, err := st.GetReviewDraft(ctx, arts[idx].ID, PrincipalKindUser, "u1"); err != nil {
				t.Errorf("active draft %d reaped: %v", idx, err)
			}
		}
	})
}

func TestReviewDraftStoreDeletedWithArtifact(t *testing.T) {
	forEachDraftBackend(t, func(t *testing.T, _ *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		a, _, _, _ := seedArtifact(t, st, "")
		expiring, _, _, _ := seedArtifact(t, st, "")
		other, _, _, _ := seedArtifact(t, st, "")
		now := time.Now().UTC()

		for _, u := range []string{"u1", "u2"} {
			if _, err := st.PutReviewDraft(ctx, testDraft(a, PrincipalKindUser, u, 1, "doc.md", "secret-"+u, "note-"+u), 0, now, 200); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := st.PutReviewDraft(ctx, testDraft(expiring, PrincipalKindUser, "u1", 1, "doc.md", "expiring-draft", ""), 0, now, 200); err != nil {
			t.Fatal(err)
		}
		if _, err := st.PutReviewDraft(ctx, testDraft(other, PrincipalKindUser, "u1", 1, "doc.md", "keep", ""), 0, now, 200); err != nil {
			t.Fatal(err)
		}

		actor := &Actor{Kind: PrincipalKindUser, Ref: "u0", Authority: AuthorityOwner}
		delRes, err := st.DeleteArtifact(ctx, a.ID, DeleteOptions{Actor: actor, Now: now})
		if err != nil {
			t.Fatalf("DeleteArtifact: %v", err)
		}
		if delRes.Drafts != 2 {
			t.Errorf("DeleteResult.Drafts = %d, want 2", delRes.Drafts)
		}
		for _, u := range []string{"u1", "u2"} {
			if _, err := st.GetReviewDraft(ctx, a.ID, PrincipalKindUser, u); !errors.Is(err, ErrNotFound) {
				t.Errorf("draft of %s survived artifact delete: %v", u, err)
			}
		}
		ev := onlyEvent(t, st, a.ID, EventDeleted, actor)
		if ev.Detail["draftsRemoved"] != float64(2) {
			t.Errorf("EventDeleted detail = %+v, want draftsRemoved=2", ev.Detail)
		}

		// Expiry sweep deletes through DeleteArtifact and drops drafts too.
		past := now.Add(-time.Minute)
		if _, err := st.SetExpiry(ctx, expiring.ID, &past, actor); err != nil {
			t.Fatal(err)
		}
		if swept, err := st.SweepExpired(ctx, now, 10); err != nil || len(swept) != 1 {
			t.Fatalf("SweepExpired = %v, %v", swept, err)
		}
		if _, err := st.GetReviewDraft(ctx, expiring.ID, PrincipalKindUser, "u1"); !errors.Is(err, ErrNotFound) {
			t.Errorf("draft survived SweepExpired: %v", err)
		}
		expEv := onlyEvent(t, st, expiring.ID, EventDeleted, &retentionActor)
		if expEv.Detail["draftsRemoved"] != float64(1) {
			t.Errorf("SweepExpired EventDeleted detail = %+v, want draftsRemoved=1", expEv.Detail)
		}

		// Another artifact's draft is untouched.
		if d, err := st.GetReviewDraft(ctx, other.ID, PrincipalKindUser, "u1"); err != nil || d.Body != "keep" {
			t.Errorf("other artifact's draft = %+v, %v", d, err)
		}
	})
}

func TestMigration0015_ReviewDrafts(t *testing.T) {
	if i := migrationIndex(migrationReviewDrafts); i < 1 || migrations[i-1].name != migrationReviewLadder {
		t.Fatalf("migration %q is not right after %q", migrationReviewDrafts, migrationReviewLadder)
	}
	if got := migrationReviewDrafts; got != "0015_review_drafts" {
		t.Fatalf("migrationReviewDrafts = %q", got)
	}
	for _, b := range testBackends() {
		t.Run(b.name, func(t *testing.T) {
			db, _ := b.open(t)
			st := NewStore(db, b.driver).(*sqlStore)
			migrateTo(t, db, st, migrationReviewDrafts)
			ctx := context.Background()
			if err := st.Init(ctx); err != nil {
				t.Fatalf("Init over 0014: %v", err)
			}
			a, _, _, _ := seedArtifact(t, st, "")
			d, err := st.PutReviewDraft(ctx, testDraft(a, PrincipalKindUser, "u1", 1, "doc.md", "migrated", "note"), 0, time.Now().UTC(), 200)
			if err != nil || d.Rev != 1 {
				t.Fatalf("PutReviewDraft after migration = %+v, %v", d, err)
			}
			if got, err := st.GetReviewDraft(ctx, a.ID, PrincipalKindUser, "u1"); err != nil || got.Body != "migrated" {
				t.Errorf("GetReviewDraft after migration = %+v, %v", got, err)
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

func TestReviewDraftStoreListKeys(t *testing.T) {
	forEachDraftBackend(t, func(t *testing.T, _ *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		if _, err := st.ListReviewDraftKeys(ctx, nil, 0); err == nil {
			t.Fatalf("ListReviewDraftKeys(limit=0) = nil, want error")
		}
		empty, err := st.ListReviewDraftKeys(ctx, nil, 10)
		if err != nil || len(empty) != 0 {
			t.Fatalf("ListReviewDraftKeys on empty = %v, %v", empty, err)
		}

		a1, _, _, _ := seedArtifact(t, st, "")
		a2, _, _, _ := seedArtifact(t, st, "")
		now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

		drafts := []ReviewDraft{
			testDraft(a1, PrincipalKindUser, "u2", 1, "doc.md", "b1", ""),
			testDraft(a1, PrincipalKindAgent, "a1", 1, "doc.md", "b2", ""),
			testDraft(a1, PrincipalKindUser, "u1", 1, "doc.md", "b3", ""),
			testDraft(a2, PrincipalKindUser, "u1", 1, "doc.md", "b4", ""),
			testDraft(a2, PrincipalKindAgent, "a2", 1, "doc.md", "b5", ""),
		}
		for _, d := range drafts {
			if _, err := st.PutReviewDraft(ctx, d, 0, now, 200); err != nil {
				t.Fatalf("PutReviewDraft(%s, %s:%s): %v", d.ArtifactID, d.PrincipalKind, d.PrincipalRef, err)
			}
		}

		// Walk all 5 keys in pages of 2; verify strict PK ordering.
		var walked []ReviewDraftKey
		var after *ReviewDraftKey
		for {
			page, err := st.ListReviewDraftKeys(ctx, after, 2)
			if err != nil {
				t.Fatalf("ListReviewDraftKeys: %v", err)
			}
			walked = append(walked, page...)
			if len(page) < 2 {
				break
			}
			last := page[len(page)-1]
			after = &last
		}
		if len(walked) != 5 {
			t.Fatalf("walked %d keys, want 5: %+v", len(walked), walked)
		}
		for i := 1; i < len(walked); i++ {
			prev, cur := walked[i-1], walked[i]
			less := prev.ArtifactID < cur.ArtifactID ||
				(prev.ArtifactID == cur.ArtifactID && (prev.PrincipalKind < cur.PrincipalKind ||
					(prev.PrincipalKind == cur.PrincipalKind && prev.PrincipalRef < cur.PrincipalRef)))
			if !less {
				t.Fatalf("keys not strictly ordered at %d: %+v >= %+v", i, prev, cur)
			}
		}

		// Deleting rows mid-walk does not skip subsequent keys.
		after = nil
		var seenWhileDeleting []ReviewDraftKey
		for {
			page, err := st.ListReviewDraftKeys(ctx, after, 2)
			if err != nil {
				t.Fatalf("ListReviewDraftKeys while deleting: %v", err)
			}
			if len(page) == 0 {
				break
			}
			for _, k := range page {
				seenWhileDeleting = append(seenWhileDeleting, k)
				if deleted, err := st.DeleteReviewDraft(ctx, k.ArtifactID, k.PrincipalKind, k.PrincipalRef, 0); err != nil || !deleted {
					t.Fatalf("DeleteReviewDraft(%+v) = %v, %v", k, deleted, err)
				}
			}
			if len(page) < 2 {
				break
			}
			last := page[len(page)-1]
			after = &last
		}
		if len(seenWhileDeleting) != 5 {
			t.Fatalf("seenWhileDeleting = %d, want 5", len(seenWhileDeleting))
		}
		rem, err := st.ListReviewDraftKeys(ctx, nil, 10)
		if err != nil || len(rem) != 0 {
			t.Fatalf("remaining keys after walk-delete = %v, %v", rem, err)
		}
	})
}

func TestReviewDraftStoreListArtifactIDs(t *testing.T) {
	forEachDraftBackend(t, func(t *testing.T, _ *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

		a1, _, _, _ := seedArtifact(t, st, "")
		a2, _, _, _ := seedArtifact(t, st, "")
		a3, _, _, _ := seedArtifact(t, st, "")

		// Empty before any drafts.
		got, err := st.ListReviewDraftArtifactIDs(ctx, PrincipalKindUser, "u1")
		if err != nil || len(got) != 0 {
			t.Fatalf("empty ListReviewDraftArtifactIDs = %v, %v", got, err)
		}
		// Empty kind/ref returns empty without error.
		if got, err := st.ListReviewDraftArtifactIDs(ctx, "", "u1"); err != nil || len(got) != 0 {
			t.Fatalf("empty kind ListReviewDraftArtifactIDs = %v, %v", got, err)
		}

		// Save drafts for u1 on a1 and a3, u2 on a2 and a3, agent ag1 on a1.
		for _, tc := range []struct {
			art       *Artifact
			kind, ref string
		}{
			{a1, PrincipalKindUser, "u1"},
			{a3, PrincipalKindUser, "u1"},
			{a2, PrincipalKindUser, "u2"},
			{a3, PrincipalKindUser, "u2"},
			{a1, PrincipalKindAgent, "ag1"},
		} {
			if _, err := st.PutReviewDraft(ctx, testDraft(tc.art, tc.kind, tc.ref, 1, "doc.md", "draft", ""), 0, now, MaxDraftsPerPrincipal); err != nil {
				t.Fatalf("PutReviewDraft(%+v): %v", tc, err)
			}
		}

		gotU1, err := st.ListReviewDraftArtifactIDs(ctx, PrincipalKindUser, "u1")
		if err != nil {
			t.Fatalf("ListReviewDraftArtifactIDs(u1): %v", err)
		}
		wantU1 := []string{a1.ID, a3.ID}
		slices.Sort(wantU1)
		if !slices.Equal(gotU1, wantU1) {
			t.Errorf("u1 draft IDs = %v, want %v", gotU1, wantU1)
		}

		gotU2, err := st.ListReviewDraftArtifactIDs(ctx, PrincipalKindUser, "u2")
		if err != nil {
			t.Fatalf("ListReviewDraftArtifactIDs(u2): %v", err)
		}
		wantU2 := []string{a2.ID, a3.ID}
		slices.Sort(wantU2)
		if !slices.Equal(gotU2, wantU2) {
			t.Errorf("u2 draft IDs = %v, want %v", gotU2, wantU2)
		}

		gotAg1, err := st.ListReviewDraftArtifactIDs(ctx, PrincipalKindAgent, "ag1")
		if err != nil {
			t.Fatalf("ListReviewDraftArtifactIDs(ag1): %v", err)
		}
		if !slices.Equal(gotAg1, []string{a1.ID}) {
			t.Errorf("ag1 draft IDs = %v, want [%s]", gotAg1, a1.ID)
		}

		// Deleting u1's draft on a1 removes only a1 for u1.
		if _, err := st.DeleteReviewDraft(ctx, a1.ID, PrincipalKindUser, "u1", 0); err != nil {
			t.Fatalf("DeleteReviewDraft: %v", err)
		}
		gotU1After, err := st.ListReviewDraftArtifactIDs(ctx, PrincipalKindUser, "u1")
		if err != nil {
			t.Fatalf("ListReviewDraftArtifactIDs(u1 after delete): %v", err)
		}
		if !slices.Equal(gotU1After, []string{a3.ID}) {
			t.Errorf("u1 draft IDs after delete = %v, want [%s]", gotU1After, a3.ID)
		}
	})
}
