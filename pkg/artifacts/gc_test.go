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
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
)

// blobExists reports whether the fixture's storage holds digest.
func (f *fixture) blobExists(digest string) bool {
	f.t.Helper()
	ok, err := f.blobs.Exists(context.Background(), BlobPath("hub-1", digest))
	if err != nil {
		f.t.Fatal(err)
	}
	return ok
}

// sweepAt runs blob sweep passes at now until the listing wraps.
func (f *fixture) sweepAt(g *BlobSweeper, now time.Time, grace time.Duration) int {
	f.t.Helper()
	total := 0
	for range 100 {
		_, n, err := g.Sweep(context.Background(), f.store, f.blobs, "hub-1", grace, now)
		if err != nil {
			f.t.Fatalf("sweep: %v", err)
		}
		total += n
		if g.cursor == "" {
			return total
		}
	}
	f.t.Fatal("sweep never wrapped")
	return total
}

// TestBlobSweepRules: a blob of a live artifact is never deleted; a blob
// left only by a deleted artifact is deleted once unreferenced for the
// grace period, not before; a touch restarts the wait; a shared blob
// survives while any live artifact uses it.
func TestBlobSweepRules(t *testing.T) {
	f := newFixture(t, false)
	keep := []byte("kept bytes")
	gone := []byte("deleted bytes")
	shared := []byte("shared bytes")
	live := f.publish(userU, "keep.md", keep, "scope=project-1").Artifact.ID
	dead := f.publish(userU, "gone.md", gone, "scope=project-1").Artifact.ID
	f.publish(userU, "s1.md", shared, "scope=project-1")
	s2 := f.publish(userU, "s2.md", shared, "scope=project-1").Artifact.ID
	_ = live
	f.exec(t, `UPDATE artifact SET deleted_at = ? WHERE id IN (?, ?)`, linkNow(), dead, s2)

	g := &BlobSweeper{}
	t0 := time.Now().Add(DefaultGCGrace) // well past every publish's touch
	if n := f.sweepAt(g, t0, DefaultGCGrace); n != 0 {
		t.Fatalf("first pass deleted %d blobs; unreferenced blobs must wait the grace period", n)
	}
	if n := f.sweepAt(g, t0.Add(DefaultGCGrace-time.Second), DefaultGCGrace); n != 0 {
		t.Fatalf("deleted %d blobs before the grace period ended", n)
	}
	if n := f.sweepAt(g, t0.Add(DefaultGCGrace), DefaultGCGrace); n != 1 {
		t.Fatalf("deleted %d blobs at the end of the grace period, want 1", n)
	}
	if f.blobExists(sha(gone)) || !f.blobExists(sha(keep)) || !f.blobExists(sha(shared)) {
		t.Errorf("after the sweep: gone=%v keep=%v shared=%v", f.blobExists(sha(gone)), f.blobExists(sha(keep)), f.blobExists(sha(shared)))
	}
	// Publishing the deleted bytes again re-uploads them.
	f.publish(userU, "again.md", gone, "scope=project-1")
	if !f.blobExists(sha(gone)) {
		t.Errorf("re-published blob missing")
	}
}

// TestBlobSweepGraceFloor: a grace below MinGCGrace is raised to it.
func TestBlobSweepGraceFloor(t *testing.T) {
	f := newFixture(t, false)
	gone := []byte("bytes")
	id := f.publish(userU, "g.md", gone, "scope=project-1").Artifact.ID
	f.exec(t, `UPDATE artifact SET deleted_at = ? WHERE id = ?`, linkNow(), id)
	g := &BlobSweeper{}
	t0 := time.Now().Add(MinGCGrace)
	f.sweepAt(g, t0, time.Minute)
	if n := f.sweepAt(g, t0.Add(MinGCGrace-time.Second), time.Minute); n != 0 {
		t.Errorf("grace floor ignored: deleted %d", n)
	}
	if n := f.sweepAt(g, t0.Add(MinGCGrace), time.Minute); n != 1 {
		t.Errorf("at the floor: deleted %d", n)
	}
}

// TestBlobSweepPendingIsReference: a pending version's files are
// references, so an upload in flight keeps its blobs.
func TestBlobSweepPendingIsReference(t *testing.T) {
	f := newFixture(t, false)
	files := bundle{"a.txt": []byte("pending bytes")}
	req := files.manifest("a.txt")
	req.Scope = "project-1"
	pend := f.createPending(userU, "/api/v1/artifacts", req)
	if rec := f.put(userU, pend.Artifact.ID, 1, "a.txt", files["a.txt"]); rec.Code != http.StatusNoContent {
		t.Fatalf("PUT: %d", rec.Code)
	}
	g := &BlobSweeper{}
	far := time.Now().Add(10 * DefaultGCGrace)
	f.sweepAt(g, far, DefaultGCGrace)
	f.sweepAt(g, far.Add(2*DefaultGCGrace), DefaultGCGrace)
	if !f.blobExists(sha(files["a.txt"])) {
		t.Fatalf("a pending version's blob was deleted")
	}
	if rec := f.finalize(userU, pend.Artifact.ID, 1); rec.Code != http.StatusOK {
		t.Errorf("finalize after the sweep: %d %s", rec.Code, rec.Body.String())
	}
}

// TestBlobSweepTouchProtects: a blob touched by a publish within the grace
// period is spared, even if no reference to it is recorded yet.
func TestBlobSweepTouchProtects(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		d := sha([]byte("orphan"))
		t0 := time.Now()
		if err := st.MarkBlobs(ctx, marks(d), t0); err != nil {
			t.Fatal(err)
		}
		if err := st.TouchBlob(ctx, d, t0.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		var deleted []string
		del := func(x string, _ int64) error { deleted = append(deleted, x); return nil }
		if n, err := st.ReclaimBlobs(ctx, t0.Add(time.Minute), 10, del); err != nil || n != 0 {
			t.Fatalf("touched blob reclaimed: %d %v", n, err)
		}
		// The touch keeps the mark; the blob is spared until the touch is
		// older than the cutoff, then reclaimable.
		if n, _ := st.ReclaimBlobs(ctx, t0.Add(59*time.Minute), 10, del); n != 0 {
			t.Fatalf("reclaimed within the touch's grace")
		}
		if n, _ := st.ReclaimBlobs(ctx, t0.Add(time.Hour), 10, del); n != 1 || len(deleted) != 1 || deleted[0] != d {
			t.Fatalf("not reclaimed once the touch aged: %d %v", n, deleted)
		}
		// A failing delete keeps the state row and ends the pass.
		if err := st.MarkBlobs(ctx, marks(d), t0); err != nil {
			t.Fatal(err)
		}
		boom := errors.New("boom")
		if _, err := st.ReclaimBlobs(ctx, t0.Add(time.Hour), 10, func(string, int64) error { return boom }); !errors.Is(err, boom) {
			t.Fatalf("delete error: %v", err)
		}
		if n, _ := st.ReclaimBlobs(ctx, t0.Add(time.Hour), 10, del); n != 1 {
			t.Errorf("row lost after a failed delete")
		}
		if err := st.MarkBlobs(ctx, make([]BlobMark, MaxBlobBatch+1), t0); err == nil {
			t.Errorf("MarkBlobs over the batch accepted")
		}
	})
}

// TestBlobSweepRechecksReferences: a reference that appears after the
// mark (and without a touch) still stops the delete.
func TestBlobSweepRechecksReferences(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		_, _, f, _ := seedArtifact(t, st, "")
		s := st.(*sqlStore)
		t0 := time.Now()
		// Mark it unreferenced as if the reference did not exist yet.
		if _, err := db.Exec(s.rebind(`INSERT INTO artifact_blob (sha256, unreferenced_since) VALUES (?, ?)`), f.SHA256, s.timeArg(t0)); err != nil {
			t.Fatal(err)
		}
		called := false
		n, err := st.ReclaimBlobs(ctx, t0.Add(time.Hour), 10, func(string, int64) error { called = true; return nil })
		if err != nil || n != 0 || called {
			t.Fatalf("referenced blob reclaimed: %d %v called=%v", n, err, called)
		}
		var rows int
		_ = db.QueryRow(`SELECT COUNT(*) FROM artifact_blob`).Scan(&rows)
		if rows != 0 {
			t.Errorf("state row kept for a referenced blob")
		}
	})
}

// TestBlobSweepTouchWaitsForDelete: a writer touching a blob while the
// sweep deletes it waits until the delete has committed, so its existence
// check afterwards sees the blob gone and it uploads again.
func TestBlobSweepTouchWaitsForDelete(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, reopen func() *sql.DB) {
		ctx := context.Background()
		d := sha([]byte("racing"))
		t0 := time.Now()
		if err := st.MarkBlobs(ctx, marks(d), t0); err != nil {
			t.Fatal(err)
		}
		writer := NewStore(reopen(), driverOf(st))
		var touched sync.WaitGroup
		touchedAt := make(chan time.Time, 1)
		var deletedAt time.Time
		n, err := st.ReclaimBlobs(ctx, t0.Add(time.Hour), 10, func(string, int64) error {
			touched.Add(1)
			go func() {
				defer touched.Done()
				if err := writer.TouchBlob(ctx, d, time.Now()); err != nil {
					t.Errorf("touch: %v", err)
				}
				touchedAt <- time.Now()
			}()
			time.Sleep(200 * time.Millisecond)
			deletedAt = time.Now()
			return nil
		})
		touched.Wait()
		if err != nil || n != 1 {
			t.Fatalf("reclaim: %d %v", n, err)
		}
		if at := <-touchedAt; at.Before(deletedAt) {
			t.Errorf("the touch completed while the delete was in progress")
		}
		// The touch left a fresh row that protects the re-upload.
		if n, _ := st.ReclaimBlobs(ctx, t0.Add(time.Hour), 10, func(string, int64) error { return nil }); n != 0 {
			t.Errorf("the re-touched blob was reclaimed")
		}
	})
}

// TestBlobDigest: only exact blob paths of this hub are swept.
func TestBlobDigest(t *testing.T) {
	d := sha([]byte("x"))
	for name, want := range map[string]bool{
		BlobPath("hub-1", d):                     true,
		"/" + BlobPath("hub-1", d):               true,
		BlobPath("hub-2", d):                     false,
		BlobPath("hub-1", d) + ".tmp":            false,
		strings.ToUpper(BlobPath("hub-1", d)):    false,
		"hubs/hub-1/artifacts/blobs/sha256/" + d: false,
		"x":                                      false,
	} {
		if _, ok := blobDigest("hub-1", name); ok != want {
			t.Errorf("blobDigest(%q) = %v, want %v", name, ok, want)
		}
	}
}

// TestBlobSweepPagesThroughEverything: with more blobs than one pass
// lists, passes resume where the last stopped and reach every blob.
func TestBlobSweepPagesThroughEverything(t *testing.T) {
	f := newFixture(t, false)
	ctx := context.Background()
	const n = gcListBatch + 7
	var digests []string
	for i := range n {
		b := []byte("orphan " + strconv.Itoa(i))
		d := sha(b)
		if _, err := f.blobs.Upload(ctx, BlobPath("hub-1", d), bytes.NewReader(b), storage.UploadOptions{}); err != nil {
			t.Fatal(err)
		}
		digests = append(digests, d)
	}
	g := &BlobSweeper{}
	t0 := time.Now()
	listed, _, err := g.Sweep(ctx, f.store, f.blobs, "hub-1", DefaultGCGrace, t0)
	if err != nil || listed != gcListBatch || g.cursor == "" {
		t.Fatalf("first pass: listed %d, cursor %q, %v", listed, g.cursor, err)
	}
	listed, _, err = g.Sweep(ctx, f.store, f.blobs, "hub-1", DefaultGCGrace, t0)
	if err != nil || listed != 7 || g.cursor != "" {
		t.Fatalf("second pass: listed %d, cursor %q, %v", listed, g.cursor, err)
	}
	deleted := f.sweepAt(g, t0.Add(DefaultGCGrace), DefaultGCGrace)
	deleted += f.sweepAt(g, t0.Add(DefaultGCGrace), DefaultGCGrace)
	deleted += f.sweepAt(g, t0.Add(DefaultGCGrace), DefaultGCGrace)
	deleted += f.sweepAt(g, t0.Add(DefaultGCGrace), DefaultGCGrace)
	deleted += f.sweepAt(g, t0.Add(DefaultGCGrace), DefaultGCGrace)
	deleted += f.sweepAt(g, t0.Add(DefaultGCGrace), DefaultGCGrace)
	if deleted != n {
		t.Errorf("deleted %d of %d orphan blobs", deleted, n)
	}
	for _, d := range digests[:3] {
		if f.blobExists(d) {
			t.Errorf("orphan %s kept", d)
		}
	}
}

// TestBlobSweepRandomized drives random publishes, deletions, expiries
// and sweeps with jumps of the clock, and checks after every sweep that
// every file of every live artifact can still be read.
func TestBlobSweepRandomized(t *testing.T) {
	seed := time.Now().UnixNano()
	rng := rand.New(rand.NewSource(seed))
	t.Logf("seed %d", seed)
	f := newFixture(t, false)
	ctx := context.Background()
	g := &BlobSweeper{}
	clock := time.Now()
	payload := func() []byte { return []byte("content " + strconv.Itoa(rng.Intn(12))) } // few distinct, so blobs are shared
	var ids []string
	for step := range 150 {
		switch op := rng.Intn(10); {
		case op < 4:
			ids = append(ids, f.publish(userU, "f"+strconv.Itoa(step)+".md", payload(), "scope=project-1").Artifact.ID)
		case op == 4 && len(ids) > 0:
			id := ids[rng.Intn(len(ids))]
			f.exec(t, `UPDATE artifact SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`, clock.UTC().Format(sqliteTimeLayout), id)
		case op == 5 && len(ids) > 0:
			id := ids[rng.Intn(len(ids))]
			f.exec(t, `UPDATE artifact SET expires_at = ? WHERE id = ?`, clock.Add(time.Hour).UTC().Format(sqliteTimeLayout), id)
		case op == 6:
			files := bundle{"p.md": payload()}
			req := files.manifest("p.md")
			req.Scope = "project-1"
			pend := f.createPending(userU, "/api/v1/artifacts", req)
			for _, p := range pend.Upload.Required {
				f.put(userU, pend.Artifact.ID, 1, p, files[p])
			}
			if rng.Intn(2) == 0 {
				f.finalize(userU, pend.Artifact.ID, 1)
			}
			ids = append(ids, pend.Artifact.ID)
		default:
			clock = clock.Add(time.Duration(rng.Intn(3*int(DefaultGCGrace/time.Hour))) * time.Hour)
			if _, err := f.store.SweepExpired(ctx, clock, 100); err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.ReapPending(ctx, clock.Add(-PendingVersionTTL), 100); err != nil {
				t.Fatal(err)
			}
			f.sweepAt(g, clock, DefaultGCGrace)
			checkLiveBlobs(t, f, step)
		}
	}
	clock = clock.Add(3 * DefaultGCGrace)
	f.sweepAt(g, clock, DefaultGCGrace)
	f.sweepAt(g, clock.Add(DefaultGCGrace), DefaultGCGrace)
	checkLiveBlobs(t, f, -1)
}

// checkLiveBlobs fails when a file of a ready or pending version of a
// live artifact has no blob.
func checkLiveBlobs(t *testing.T, f *fixture, step int) {
	t.Helper()
	rows, err := f.db.Query(`SELECT DISTINCT fl.sha256 FROM artifact_file fl
		JOIN artifact_version v ON v.id = fl.version_id JOIN artifact a ON a.id = v.artifact_id
		WHERE a.deleted_at IS NULL AND v.state IN ('ready', 'finalizing') AND fl.sha256 IS NOT NULL AND fl.received = 1`)
	if err != nil {
		t.Fatal(err)
	}
	var digests []string
	for rows.Next() {
		var d string
		_ = rows.Scan(&d)
		digests = append(digests, d)
	}
	_ = rows.Close()
	for _, d := range digests {
		if !f.blobExists(d) {
			t.Fatalf("step %d: blob %s of a live artifact was deleted", step, d)
		}
	}
}

// TestBlobSweepSparesBlobBeingPublished: a publish that finds its blob
// already stored relies on it before its reference is recorded; a sweep
// running in that window spares the blob, so the published file can be
// read.
func TestBlobSweepSparesBlobBeingPublished(t *testing.T) {
	f := newFixture(t, false)
	ctx := context.Background()
	body := []byte("orphan bytes, published again")
	d := sha(body)
	if _, err := f.blobs.Upload(ctx, BlobPath("hub-1", d), bytes.NewReader(body), storage.UploadOptions{}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := f.store.MarkBlobs(ctx, marks(d), now.Add(-3*DefaultGCGrace)); err != nil {
		t.Fatal(err)
	}
	f.svc.SetStore(&sweepOnCreateStore{Store: f.store, sweep: func() {
		if _, err := f.store.ReclaimBlobs(ctx, now.Add(-DefaultGCGrace), 10, func(x string, _ int64) error {
			return f.blobs.Delete(ctx, BlobPath("hub-1", x))
		}); err != nil {
			t.Errorf("reclaim: %v", err)
		}
	}})
	id := f.publish(userU, "again.md", body, "scope=project-1").Artifact.ID
	if !f.blobExists(d) {
		t.Fatalf("the sweep deleted a blob a publish was relying on")
	}
	if rec := f.do(&userU, http.MethodGet, "/api/v1/artifacts/"+id+"/files/again.md", nil, nil); rec.Code != http.StatusOK {
		t.Errorf("read the published file: %d", rec.Code)
	}
}

// sweepOnCreateStore runs sweep just before the artifact is recorded.
type sweepOnCreateStore struct {
	Store
	sweep func()
}

func (s *sweepOnCreateStore) CreatePublished(ctx context.Context, a *Artifact, v *Version, files []File, grants []Grant) error {
	s.sweep()
	return s.Store.CreatePublished(ctx, a, v, files, grants)
}

// TestBlobSweepRechecksUnderLock: a touch that lands after a blob was
// found reclaimable but before it is locked still spares it.
func TestBlobSweepRechecksUnderLock(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		d := sha([]byte("late touch"))
		t0 := time.Now()
		if err := st.MarkBlobs(ctx, marks(d), t0.Add(-2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		reclaimCandidateHook = func(x string) {
			// A publish touches it, and a mark pass runs again.
			if err := st.TouchBlob(ctx, x, t0); err != nil {
				t.Errorf("touch: %v", err)
			}
			if err := st.MarkBlobs(ctx, marks(x), t0); err != nil {
				t.Errorf("mark: %v", err)
			}
		}
		t.Cleanup(func() { reclaimCandidateHook = nil })
		called := false
		if n, err := st.ReclaimBlobs(ctx, t0.Add(-time.Hour), 10, func(string, int64) error { called = true; return nil }); err != nil || n != 0 || called {
			t.Errorf("a blob touched before its lock was reclaimed: %d %v %v", n, err, called)
		}
	})
}

// TestBlobMarkClearsReferenced: marking a referenced blob leaves no state
// for it.
func TestBlobMarkClearsReferenced(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		_, _, f, _ := seedArtifact(t, st, "")
		if err := st.TouchBlob(ctx, f.SHA256, time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := st.MarkBlobs(ctx, marks(f.SHA256), time.Now()); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM artifact_blob`).Scan(&n); err != nil || n != 0 {
			t.Errorf("state rows for a referenced blob: %d %v", n, err)
		}
	})
}

// hangingDeleteStorage is local storage whose Delete waits for its
// context to end.
type hangingDeleteStorage struct{ *storage.LocalStorage }

func (hangingDeleteStorage) Delete(ctx context.Context, _ string) error {
	<-ctx.Done()
	return ctx.Err()
}

// TestBlobSweepDeleteHasDeadline: a delete that hangs is cut off, the
// pass reports it, and the blob and its state stay for the next pass.
func TestBlobSweepDeleteHasDeadline(t *testing.T) {
	old := gcDeleteTimeout
	gcDeleteTimeout = 50 * time.Millisecond
	t.Cleanup(func() { gcDeleteTimeout = old })
	f := newFixture(t, false)
	ctx := context.Background()
	body := []byte("stuck")
	d := sha(body)
	if _, err := f.local.Upload(ctx, BlobPath("hub-1", d), bytes.NewReader(body), storage.UploadOptions{}); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	if err := f.store.MarkBlobs(ctx, marks(d), t0.Add(-2*DefaultGCGrace)); err != nil {
		t.Fatal(err)
	}
	g := &BlobSweeper{}
	done := make(chan error, 1)
	go func() {
		_, _, err := g.Sweep(ctx, f.store, hangingDeleteStorage{f.local}, "hub-1", DefaultGCGrace, t0)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("sweep with a hanging delete: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the sweep hung on a delete")
	}
	if !f.blobExists(d) {
		t.Errorf("blob gone after a failed delete")
	}
	if n, err := f.store.ReclaimBlobs(ctx, t0.Add(-DefaultGCGrace), 10, func(string, int64) error { return nil }); err != nil || n != 1 {
		t.Errorf("state lost after a failed delete: %d %v", n, err)
	}
}

// marks wraps digests as BlobMarks with no generation.
func marks(digests ...string) []BlobMark {
	out := make([]BlobMark, len(digests))
	for i, d := range digests {
		out[i] = BlobMark{Digest: d}
	}
	return out
}

// versionedStorage is local storage that versions objects like GCS: each
// upload gets a new generation, listings report it, and
// DeleteIfGeneration honours it. With late set, DeleteIfGeneration does
// not delete: it fails as if it timed out and queues the delete, which
// applyLate later sends to the "object store", as a cancelled request
// that still arrives would.
type versionedStorage struct {
	*storage.LocalStorage
	mu   sync.Mutex
	gen  map[string]int64
	next int64
	late bool
	// never makes DeleteIfGeneration fail without ever deleting.
	never bool
	// failUploads fails that many next uploads.
	failUploads int
	// beforeUpload, when set, runs at the start of every upload.
	beforeUpload func()
	// queued are late deletes: path and generation.
	queued []struct {
		path string
		gen  int64
	}
}

func newVersionedStorage(local *storage.LocalStorage) *versionedStorage {
	return &versionedStorage{LocalStorage: local, gen: map[string]int64{}}
}

func (v *versionedStorage) Upload(ctx context.Context, p string, r io.Reader, o storage.UploadOptions) (*storage.Object, error) {
	v.mu.Lock()
	hook := v.beforeUpload
	v.mu.Unlock()
	if hook != nil {
		hook()
	}
	v.mu.Lock()
	if v.failUploads > 0 {
		v.failUploads--
		v.mu.Unlock()
		return nil, errors.New("upload failed")
	}
	v.mu.Unlock()
	obj, err := v.LocalStorage.Upload(ctx, p, r, o)
	if err == nil {
		v.mu.Lock()
		v.next++
		v.gen[p] = v.next
		v.mu.Unlock()
	}
	return obj, err
}

func (v *versionedStorage) List(ctx context.Context, o storage.ListOptions) (*storage.ListResult, error) {
	res, err := v.LocalStorage.List(ctx, o)
	if err == nil {
		v.mu.Lock()
		for i := range res.Objects {
			res.Objects[i].Generation = v.gen[res.Objects[i].Name]
		}
		v.mu.Unlock()
	}
	return res, err
}

func (v *versionedStorage) DeleteIfGeneration(ctx context.Context, p string, gen int64) error {
	v.mu.Lock()
	if v.never {
		v.mu.Unlock()
		return context.DeadlineExceeded
	}
	if v.late {
		v.queued = append(v.queued, struct {
			path string
			gen  int64
		}{p, gen})
		v.mu.Unlock()
		return context.DeadlineExceeded
	}
	v.mu.Unlock()
	return v.deleteIf(ctx, p, gen)
}

func (v *versionedStorage) deleteIf(ctx context.Context, p string, gen int64) error {
	v.mu.Lock()
	cur, ok := v.gen[p]
	v.mu.Unlock()
	if !ok {
		return storage.ErrNotFound
	}
	if cur != gen {
		return storage.ErrPreconditionFailed
	}
	v.mu.Lock()
	delete(v.gen, p)
	v.mu.Unlock()
	return v.LocalStorage.Delete(ctx, p)
}

// applyLate delivers the queued deletes.
func (v *versionedStorage) applyLate(ctx context.Context) {
	v.mu.Lock()
	q := v.queued
	v.queued = nil
	v.mu.Unlock()
	for _, d := range q {
		_ = v.deleteIf(ctx, d.path, d.gen)
	}
}

// TestBlobSweepLateDeleteSparesRepublishedBlob: a sweep's delete that
// fails (times out) but reaches the object store later does not remove
// bytes a publish relied on in between: the publish finds the blob still
// marked and stores it again, and the late delete names the old
// generation.
func TestBlobSweepLateDeleteSparesRepublishedBlob(t *testing.T) {
	f := newFixture(t, false)
	ctx := context.Background()
	vs := newVersionedStorage(f.local)
	f.svc.SetBlobStorage(vs, "hub-1")
	body := []byte("orphan bytes, swept late")
	d := sha(body)
	if _, err := vs.Upload(ctx, BlobPath("hub-1", d), bytes.NewReader(body), storage.UploadOptions{}); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	g := &BlobSweeper{}
	if _, _, err := g.Sweep(ctx, f.store, vs, "hub-1", DefaultGCGrace, t0.Add(-2*DefaultGCGrace)); err != nil {
		t.Fatal(err)
	}
	vs.late = true
	if _, _, err := g.Sweep(ctx, f.store, vs, "hub-1", DefaultGCGrace, t0); err == nil {
		t.Fatal("the late delete did not fail the pass")
	}
	vs.late = false
	id := f.publish(userU, "again.md", body, "scope=project-1").Artifact.ID
	vs.applyLate(ctx)
	if ok, _ := vs.Exists(ctx, BlobPath("hub-1", d)); !ok {
		t.Fatalf("a late delete removed bytes a publish relies on")
	}
	if rec := f.do(&userU, http.MethodGet, "/api/v1/artifacts/"+id+"/files/again.md", nil, nil); rec.Code != http.StatusOK || rec.Body.String() != string(body) {
		t.Errorf("read the published file: %d", rec.Code)
	}
}

// TestBlobMarkRecordsGeneration: the generation recorded at marking
// reaches the delete, and the latest listing's generation wins.
func TestBlobMarkRecordsGeneration(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		d := sha([]byte("mark"))
		old := time.Now().Add(-time.Hour)
		if err := st.MarkBlobs(ctx, []BlobMark{{Digest: d, Generation: 7}}, old); err != nil {
			t.Fatal(err)
		}
		if err := st.MarkBlobs(ctx, []BlobMark{{Digest: d, Generation: 9}}, time.Now()); err != nil {
			t.Fatal(err)
		}
		var gotGen int64
		if n, err := st.ReclaimBlobs(ctx, time.Now().Add(-time.Minute), 10, func(_ string, g int64) error { gotGen = g; return nil }); err != nil || n != 1 || gotGen != 9 {
			t.Errorf("reclaim: %d %v generation %d", n, err, gotGen)
		}
	})
}

// TestDeleteBlobOutcomes: a missing object or a changed generation counts
// as done; a provider without generations gets a plain delete.
func TestDeleteBlobOutcomes(t *testing.T) {
	ctx := context.Background()
	local, err := storage.NewLocal(storage.Config{Provider: storage.ProviderLocal, Bucket: "b", LocalPath: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	vs := newVersionedStorage(local)
	if _, err := vs.Upload(ctx, "a", strings.NewReader("x"), storage.UploadOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := deleteBlob(ctx, vs, "a", 0); err == nil {
		t.Errorf("a versioned provider without a generation was deleted from")
	}
	if ok, _ := vs.Exists(ctx, "a"); !ok {
		t.Errorf("an unknown generation deleted the object")
	}
	if err := deleteBlob(ctx, vs, "a", 99); err != nil {
		t.Errorf("changed generation: %v", err)
	}
	if ok, _ := vs.Exists(ctx, "a"); !ok {
		t.Errorf("a generation mismatch deleted the object")
	}
	if err := deleteBlob(ctx, vs, "a", vs.gen["a"]); err != nil {
		t.Errorf("matching generation: %v", err)
	}
	if err := deleteBlob(ctx, vs, "a", 1); err != nil {
		t.Errorf("missing object: %v", err)
	}
	if _, err := local.Upload(ctx, "b", strings.NewReader("y"), storage.UploadOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := deleteBlob(ctx, local, "b", 5); err != nil {
		t.Errorf("plain provider: %v", err)
	}
	if ok, _ := local.Exists(ctx, "b"); ok {
		t.Errorf("plain provider did not delete")
	}
}

// TestGCDeleteTimeoutDefault pins the delete deadline below SQLite's busy
// timeout (5s in the hub's DSN), so a slow delete in the sweep's
// transaction cannot make other writers time out.
func TestGCDeleteTimeoutDefault(t *testing.T) {
	if gcDeleteTimeout != 4*time.Second || gcDeleteTimeout >= 5*time.Second {
		t.Errorf("gcDeleteTimeout = %v, want 4s (below the 5s busy timeout)", gcDeleteTimeout)
	}
}

// blobReadable reports whether vs holds digest's blob.
func blobReadable(t *testing.T, vs *versionedStorage, d string) bool {
	t.Helper()
	ok, err := vs.Exists(context.Background(), BlobPath("hub-1", d))
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// liveDigests returns the digests referenced by ready, pending or
// finalizing versions of live artifacts.
func liveDigests(t *testing.T, f *fixture) []string {
	t.Helper()
	rows, err := f.db.Query(`SELECT DISTINCT fl.sha256 FROM artifact_file fl
		JOIN artifact_version v ON v.id = fl.version_id JOIN artifact a ON a.id = v.artifact_id
		WHERE a.deleted_at IS NULL AND v.state IN ('ready', 'pending', 'finalizing') AND fl.sha256 IS NOT NULL AND fl.received = 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

// staleOrphan stores body as an orphan blob in vs and runs a mark pass
// long ago, so the next sweep finds it reclaimable.
func staleOrphan(t *testing.T, f *fixture, vs *versionedStorage, g *BlobSweeper, body []byte) {
	t.Helper()
	ctx := context.Background()
	if _, err := vs.Upload(ctx, BlobPath("hub-1", sha(body)), bytes.NewReader(body), storage.UploadOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := g.Sweep(ctx, f.store, vs, "hub-1", DefaultGCGrace, time.Now().Add(-2*DefaultGCGrace)); err != nil {
		t.Fatal(err)
	}
}

// TestBlobLateDeleteAfterFailedRestore: a publish that finds the blob
// marked but fails to store it again leaves the mark, so the next publish
// stores it again too, and the late delete of the old generation does not
// remove the bytes it relies on.
func TestBlobLateDeleteAfterFailedRestore(t *testing.T) {
	f := newFixture(t, false)
	ctx := context.Background()
	vs := newVersionedStorage(f.local)
	f.svc.SetBlobStorage(vs, "hub-1")
	g := &BlobSweeper{}
	body := []byte("bytes swept late, re-stored after a failure")
	staleOrphan(t, f, vs, g, body)
	vs.late = true
	if _, _, err := g.Sweep(ctx, f.store, vs, "hub-1", DefaultGCGrace, time.Now()); err == nil {
		t.Fatal("the late delete did not fail the pass")
	}
	vs.late = false
	vs.failUploads = 1
	rec := f.do(&userU, http.MethodPost, "/api/v1/artifacts?name=a.md&scope=project-1", body, map[string]string{"Content-Type": "application/octet-stream"})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("publish with a failing upload: %d", rec.Code)
	}
	id := f.publish(userU, "b.md", body, "scope=project-1").Artifact.ID
	vs.applyLate(ctx)
	if !blobReadable(t, vs, sha(body)) {
		t.Fatal("a late delete removed bytes a publish relies on")
	}
	if rec := f.do(&userU, http.MethodGet, "/api/v1/artifacts/"+id+"/files/b.md", nil, nil); rec.Code != http.StatusOK {
		t.Errorf("read: %d", rec.Code)
	}
}

// TestBlobLateDeleteTwoPublishers: while one publisher that found the blob
// marked is storing it again (and fails), another publishes the same
// bytes; the second also stores them again, so the late delete of the old
// generation leaves them.
func TestBlobLateDeleteTwoPublishers(t *testing.T) {
	f := newFixture(t, false)
	ctx := context.Background()
	vs := newVersionedStorage(f.local)
	f.svc.SetBlobStorage(vs, "hub-1")
	g := &BlobSweeper{}
	body := []byte("bytes two publishers share")
	staleOrphan(t, f, vs, g, body)
	vs.late = true
	if _, _, err := g.Sweep(ctx, f.store, vs, "hub-1", DefaultGCGrace, time.Now()); err == nil {
		t.Fatal("the late delete did not fail the pass")
	}
	vs.late = false
	var secondID string
	ran := false
	vs.beforeUpload = func() {
		if ran {
			return
		}
		ran = true
		// The first publisher is about to upload: the second runs now,
		// and the first's upload then fails.
		vs.mu.Lock()
		vs.beforeUpload = nil
		vs.mu.Unlock()
		secondID = f.publish(userU, "second.md", body, "scope=project-1").Artifact.ID
		vs.mu.Lock()
		vs.failUploads = 1
		vs.mu.Unlock()
	}
	rec := f.do(&userU, http.MethodPost, "/api/v1/artifacts?name=first.md&scope=project-1", body, map[string]string{"Content-Type": "application/octet-stream"})
	if rec.Code != http.StatusInternalServerError || secondID == "" {
		t.Fatalf("first publish: %d, second %q", rec.Code, secondID)
	}
	vs.applyLate(ctx)
	if !blobReadable(t, vs, sha(body)) {
		t.Fatal("a late delete removed bytes the second publish relies on")
	}
}

// TestBlobGCInterleavings drives random sequences of publishes (with
// uploads that succeed or fail, and with a sweep or another publish
// running in the middle), deletions, mark passes, sweeps whose deletes
// apply at once, late or never, and deliveries of late deletes, over a
// versioned provider, and checks the invariant stated in gc.go after every
// step: every digest referenced by a ready, pending or finalizing version
// of a live artifact is readable.
func TestBlobGCInterleavings(t *testing.T) {
	seed := time.Now().UnixNano()
	rng := rand.New(rand.NewSource(seed))
	t.Logf("seed %d", seed)
	// One simulated clock for touches and sweeps, so that touches age
	// past the grace period between steps.
	clock := time.Now()
	old := blobClock
	blobClock = func() time.Time { return clock }
	t.Cleanup(func() { blobClock = old })
	reclaimed := 0
	for round := range 10 {
		f := newFixture(t, false)
		ctx := context.Background()
		vs := newVersionedStorage(f.local)
		f.svc.SetBlobStorage(vs, "hub-1")
		g := &BlobSweeper{}
		content := func() []byte { return []byte("content " + strconv.Itoa(rng.Intn(6))) }
		var ids []string
		pass := func(mode int) {
			vs.mu.Lock()
			vs.late, vs.never = mode == 1, mode == 2
			vs.mu.Unlock()
			for {
				_, n, _ := g.Sweep(ctx, f.store, vs, "hub-1", DefaultGCGrace, clock)
				reclaimed += n
				if g.cursor == "" {
					break
				}
			}
			vs.mu.Lock()
			vs.late, vs.never = false, false
			vs.mu.Unlock()
		}
		publishBody := func(name string, body []byte) {
			rec := f.do(&userU, http.MethodPost, "/api/v1/artifacts?name="+name+"&scope=project-1", body,
				map[string]string{"Content-Type": "application/octet-stream"})
			if rec.Code == http.StatusCreated {
				ids = append(ids, decodeInto[ArtifactResponse](t, rec).Artifact.ID)
			}
		}
		publish := func(name string) { publishBody(name, content()) }
		for step := range 150 {
			name := "f" + strconv.Itoa(step) + ".md"
			switch op := rng.Intn(15); op {
			case 0: // publish, maybe with a failing upload
				if rng.Intn(2) == 0 {
					vs.mu.Lock()
					vs.failUploads = 1
					vs.mu.Unlock()
				}
				publish(name)
				vs.mu.Lock()
				vs.failUploads = 0
				vs.mu.Unlock()
			case 1: // a publish whose upload fails, retried with the same bytes
				body := content()
				vs.mu.Lock()
				vs.failUploads = 1
				vs.mu.Unlock()
				publishBody(name, body)
				vs.mu.Lock()
				vs.failUploads = 0
				vs.mu.Unlock()
				publishBody(name+".retry", body)
			case 2: // publish with a sweep in the middle of its upload
				mode := rng.Intn(3)
				vs.beforeUpload = func() {
					vs.mu.Lock()
					vs.beforeUpload = nil
					vs.mu.Unlock()
					pass(mode)
				}
				publish(name)
				vs.beforeUpload = nil
			case 3: // publish with another publish in the middle, the first failing
				vs.beforeUpload = func() {
					vs.mu.Lock()
					vs.beforeUpload = nil
					vs.mu.Unlock()
					publish(name + ".2")
					vs.mu.Lock()
					vs.failUploads = 1
					vs.mu.Unlock()
				}
				publish(name)
				vs.mu.Lock()
				vs.beforeUpload, vs.failUploads = nil, 0
				vs.mu.Unlock()
			case 4, 10, 11: // delete an artifact, or all of them
				if len(ids) > 0 {
					f.exec(t, `UPDATE artifact SET deleted_at = ? WHERE id = ?`, clock.UTC().Format(sqliteTimeLayout), ids[rng.Intn(len(ids))])
				}
				if op == 11 {
					f.exec(t, `UPDATE artifact SET deleted_at = ? WHERE deleted_at IS NULL`, clock.UTC().Format(sqliteTimeLayout))
				}
			case 5, 6: // a sweep pass whose deletes apply at once, late (most often) or never
				pass([]int{0, 1, 1, 2}[rng.Intn(4)])
			case 7: // late deletes arrive
				vs.applyLate(ctx)
			case 8: // time passes
				clock = clock.Add(time.Duration(rng.Intn(3*int(DefaultGCGrace/time.Hour))) * time.Hour)
			case 9: // a touch without a publish (a writer that went away)
				_ = f.store.TouchBlob(ctx, sha(content()), clock)
			case 12, 13: // a two-step publish: create, maybe a sweep, PUT (ok or failing), finalize
				body := content()
				files := bundle{"m.md": body}
				req := files.manifest("m.md")
				req.Scope = "project-1"
				rec := f.postJSON(&userU, "/api/v1/artifacts", req)
				if rec.Code != http.StatusCreated {
					break
				}
				pend := decodeInto[PendingVersionResponse](t, rec)
				if rng.Intn(2) == 0 {
					pass([]int{0, 1, 1, 2}[rng.Intn(4)])
				}
				if rng.Intn(3) == 0 {
					vs.mu.Lock()
					vs.failUploads = 1
					vs.mu.Unlock()
				}
				for _, p := range pend.Upload.Required {
					f.put(userU, pend.Artifact.ID, 1, p, files[p])
				}
				vs.mu.Lock()
				vs.failUploads = 0
				vs.mu.Unlock()
				if rng.Intn(2) == 0 {
					pass([]int{0, 1, 1, 2}[rng.Intn(4)])
				}
				if f.finalize(userU, pend.Artifact.ID, 1).Code == http.StatusOK {
					ids = append(ids, pend.Artifact.ID)
				}
			case 14: // append a version to an artifact, carrying its file forward
				if len(ids) == 0 {
					break
				}
				id := ids[rng.Intn(len(ids))]
				got := f.do(&userU, http.MethodGet, "/api/v1/artifacts/"+id, nil, nil)
				if got.Code != http.StatusOK {
					break
				}
				cur := decodeInto[ArtifactResponse](t, got)
				if cur.Version == nil || len(cur.Version.Files) == 0 {
					break
				}
				cf := cur.Version.Files[0]
				req := CreateVersionRequest{Entry: cf.Path, Files: []ManifestFile{{Path: cf.Path, Size: cf.Size, SHA256: cf.SHA256}}}
				rec := f.postJSON(&userU, "/api/v1/artifacts/"+id+"/versions", req)
				if rec.Code != http.StatusCreated {
					break
				}
				pend := decodeInto[PendingVersionResponse](t, rec)
				if rng.Intn(2) == 0 {
					pass([]int{0, 1, 1, 2}[rng.Intn(4)])
				}
				f.finalize(userU, id, pend.Version.Seq)
			}
			for _, d := range liveDigests(t, f) {
				if !blobReadable(t, vs, d) {
					t.Fatalf("round %d step %d: live digest %s is not readable", round, step, d)
				}
			}
		}
		vs.applyLate(ctx)
		for _, d := range liveDigests(t, f) {
			if !blobReadable(t, vs, d) {
				t.Fatalf("round %d end: live digest %s is not readable", round, d)
			}
		}
	}
	if reclaimed == 0 {
		t.Fatalf("no blob was ever reclaimed; the sequences do not exercise the sweep")
	}
}

// TestBlobLateDeleteManifestReference: a two-step publish whose manifest
// names a digest the sweep is deleting (the delete times out and reaches
// the object store later): the manifest row is a reference from the start,
// the PUT stores the bytes again, and the late delete of the old generation
// leaves them.
func TestBlobLateDeleteManifestReference(t *testing.T) {
	f := newFixture(t, false)
	ctx := context.Background()
	vs := newVersionedStorage(f.local)
	f.svc.SetBlobStorage(vs, "hub-1")
	g := &BlobSweeper{}
	body := []byte("bytes named by a manifest while a delete is in flight")
	staleOrphan(t, f, vs, g, body)
	vs.late = true
	if _, _, err := g.Sweep(ctx, f.store, vs, "hub-1", DefaultGCGrace, time.Now()); err == nil {
		t.Fatal("the late delete did not fail the pass")
	}
	vs.late = false
	files := bundle{"m.md": body}
	req := files.manifest("m.md")
	req.Scope = "project-1"
	pend := f.createPending(userU, "/api/v1/artifacts", req)
	// A mark pass now sees the manifest row as a reference.
	if _, _, err := g.Sweep(ctx, f.store, vs, "hub-1", DefaultGCGrace, time.Now()); err != nil {
		t.Fatal(err)
	}
	if rec := f.put(userU, pend.Artifact.ID, 1, "m.md", body); rec.Code != http.StatusNoContent {
		t.Fatalf("PUT: %d", rec.Code)
	}
	if rec := f.finalize(userU, pend.Artifact.ID, 1); rec.Code != http.StatusOK {
		t.Fatalf("finalize: %d %s", rec.Code, rec.Body.String())
	}
	vs.applyLate(ctx)
	if !blobReadable(t, vs, sha(body)) {
		t.Fatal("a late delete removed bytes a finalized version references")
	}
	if rec := f.do(&userU, http.MethodGet, "/api/v1/artifacts/"+pend.Artifact.ID+"/files/m.md", nil, nil); rec.Code != http.StatusOK {
		t.Errorf("read: %d", rec.Code)
	}
}

// TestBlobCarryForwardSharesObjectAndSurvivesSweeps: a new version whose
// file is unchanged needs no upload (one stored object per digest), and
// the carried file stays readable through sweeps, because the current
// version keeps referencing it.
func TestBlobCarryForwardSharesObjectAndSurvivesSweeps(t *testing.T) {
	f := newFixture(t, false)
	ctx := context.Background()
	vs := newVersionedStorage(f.local)
	f.svc.SetBlobStorage(vs, "hub-1")
	g := &BlobSweeper{}
	same, other := []byte("unchanged file"), []byte("changed file v1")
	files := bundle{"a.md": same, "b.md": other}
	req := files.manifest("a.md")
	req.Scope = "project-1"
	id := f.publishBundle(userU, "/api/v1/artifacts", req, files).Artifact.ID
	uploads := func() int64 {
		vs.mu.Lock()
		defer vs.mu.Unlock()
		return vs.next
	}
	before := uploads()
	v2 := bundle{"a.md": same, "b.md": []byte("changed file v2")}
	pend := f.createPending(userU, "/api/v1/artifacts/"+id+"/versions", v2.manifest("a.md"))
	if len(pend.Upload.Required) != 1 || pend.Upload.Required[0] != "b.md" {
		t.Fatalf("required = %v, want only the changed file", pend.Upload.Required)
	}
	// Sweeps (marks long ago, then reclaim now) between create and finalize.
	for _, at := range []time.Time{time.Now().Add(-2 * DefaultGCGrace), time.Now()} {
		if _, _, err := g.Sweep(ctx, f.store, vs, "hub-1", DefaultGCGrace, at); err != nil {
			t.Fatal(err)
		}
	}
	if rec := f.put(userU, id, 2, "b.md", v2["b.md"]); rec.Code != http.StatusNoContent {
		t.Fatalf("PUT: %d", rec.Code)
	}
	if rec := f.finalize(userU, id, 2); rec.Code != http.StatusOK {
		t.Fatalf("finalize: %d", rec.Code)
	}
	if got := uploads() - before; got != 1 {
		t.Errorf("%d uploads for one changed file", got)
	}
	for _, d := range []string{sha(same), sha(v2["b.md"])} {
		if !blobReadable(t, vs, d) {
			t.Errorf("blob %s of the new version is not readable", d)
		}
	}
}
