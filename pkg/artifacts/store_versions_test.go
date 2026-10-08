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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// pendingArtifact creates an artifact whose first version is pending with
// the given files (paths), all of them still to be uploaded.
func pendingArtifact(t *testing.T, st Store, key string, paths ...string) (*Artifact, *Version) {
	t.Helper()
	now := time.Now()
	a := &Artifact{ID: uuid.NewString(), ScopeKind: ScopeKindProject, ScopeRef: "project-1",
		OwnerKind: PrincipalKindAgent, OwnerRef: "agent-1", Key: key, Title: "t", CreatedAt: now, UpdatedAt: now}
	v := &Version{ID: uuid.NewString(), ArtifactID: a.ID, Seq: 1, Kind: VersionKindPublish, EntryPath: paths[0],
		FileCount: len(paths), TotalBytes: int64(len(paths)), CreatedAt: now, State: VersionStatePending}
	var files []File
	for _, p := range paths {
		files = append(files, File{VersionID: v.ID, Path: p, Size: 1, SHA256: sha([]byte(p)), MediaType: "text/plain", Pending: true})
	}
	if err := st.CreatePending(context.Background(), a, v, files, nil); err != nil {
		t.Fatalf("CreatePending: %v", err)
	}
	return a, v
}

func TestStoreTwoStepVersion(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		a, v := pendingArtifact(t, st, "k", "index.html", "img/a.png")

		got, err := st.GetArtifact(ctx, a.ID)
		if err != nil || got.CurrentSeq != 0 {
			t.Fatalf("pending artifact = %+v, %v; want no current version", got, err)
		}
		if vs, err := st.ListVersions(ctx, a.ID, 0, 10); err != nil || len(vs) != 0 {
			t.Errorf("ListVersions lists a pending version: %+v, %v", vs, err)
		}
		files, _ := st.ListFiles(ctx, v.ID)
		for _, f := range files {
			if !f.Pending {
				t.Errorf("file %s not pending before upload", f.Path)
			}
		}

		// Finalize refuses while a file is missing.
		if err := st.MarkReceived(ctx, v.ID, "index.html", "text/html", nil); err != nil {
			t.Fatalf("MarkReceived: %v", err)
		}
		if err := claimFin(ctx, st, a.ID, 1); !errors.Is(err, ErrConflict) {
			t.Fatalf("claim with a missing file: %v, want ErrConflict", err)
		}
		if _, err := st.FinalizeVersion(ctx, a.ID, 1, claimOf(a.ID, 1), nil); !errors.Is(err, ErrConflict) {
			t.Fatalf("finalize without a claim: %v, want ErrConflict", err)
		}
		if err := st.MarkReceived(ctx, v.ID, "nope.txt", "text/plain", nil); !errors.Is(err, ErrNotFound) {
			t.Errorf("MarkReceived(unknown path) = %v, want ErrNotFound", err)
		}
		if err := st.MarkReceived(ctx, v.ID, "img/a.png", "image/png", nil); err != nil {
			t.Fatal(err)
		}
		extra := []File{{VersionID: v.ID, Path: "_remote/" + strings.Repeat("cd", 32), Size: 7, SHA256: strings.Repeat("ef", 32),
			MediaType: "image/png", Origin: FileOriginRemote, SourceURL: "https://example.com/x.png", FetchStatus: FetchStatusOK}}
		if err := claimFin(ctx, st, a.ID, 1); err != nil {
			t.Fatalf("ClaimFinalize: %v", err)
		}
		if err := claimFin(ctx, st, a.ID, 1); !errors.Is(err, ErrConflict) {
			t.Fatalf("second claim: %v, want ErrConflict", err)
		}
		if err := st.MarkReceived(ctx, v.ID, "index.html", "text/html", nil); !errors.Is(err, ErrConflict) {
			t.Errorf("upload to a finalizing version = %v, want ErrConflict", err)
		}
		if err := st.ReleaseFinalize(ctx, a.ID, 1, claimOf(a.ID, 1)); err != nil {
			t.Fatal(err)
		}
		if err := claimFin(ctx, st, a.ID, 1); err != nil {
			t.Fatalf("claim after release: %v", err)
		}
		if err := claimFin(ctx, st, a.ID, 9); !errors.Is(err, ErrNotFound) {
			t.Errorf("claim of a missing version = %v, want ErrNotFound", err)
		}
		got, err = st.FinalizeVersion(ctx, a.ID, 1, claimOf(a.ID, 1), extra)
		if err != nil || got.CurrentSeq != 1 {
			t.Fatalf("FinalizeVersion = %+v, %v", got, err)
		}
		gv, _ := st.GetVersion(ctx, a.ID, 1)
		if gv.State != VersionStateReady || gv.FileCount != 3 || gv.TotalBytes != 9 {
			t.Errorf("finalized version = %+v; want ready, 3 files, 9 bytes", gv)
		}
		f, err := st.GetFile(ctx, v.ID, "img/a.png")
		if err != nil || f.Pending || f.MediaType != "image/png" {
			t.Errorf("received file = %+v, %v", f, err)
		}
		if _, err := st.FinalizeVersion(ctx, a.ID, 1, claimOf(a.ID, 1), nil); !errors.Is(err, ErrConflict) {
			t.Errorf("second finalize = %v, want ErrConflict", err)
		}
		if err := st.MarkReceived(ctx, v.ID, "index.html", "text/html", nil); !errors.Is(err, ErrConflict) {
			t.Errorf("MarkReceived on a ready version = %v, want ErrConflict", err)
		}

		// Append two versions; finalizing the later first keeps it current.
		v2 := &Version{ID: uuid.NewString(), ArtifactID: a.ID, Kind: VersionKindPublish, EntryPath: "index.html",
			CreatedAt: time.Now(), State: VersionStatePending}
		v3 := &Version{ID: uuid.NewString(), ArtifactID: a.ID, Kind: VersionKindPublish, EntryPath: "index.html",
			CreatedAt: time.Now(), State: VersionStatePending}
		for _, nv := range []*Version{v2, v3} {
			if err := st.CreateVersion(ctx, nv, nil, 2); err != nil {
				t.Fatalf("CreateVersion: %v", err)
			}
		}
		if v2.Seq != 2 || v3.Seq != 3 {
			t.Fatalf("seqs = %d, %d; want 2, 3", v2.Seq, v3.Seq)
		}
		v4 := &Version{ID: uuid.NewString(), ArtifactID: a.ID, Kind: VersionKindPublish, EntryPath: "x", CreatedAt: time.Now(), State: VersionStatePending}
		if err := st.CreateVersion(ctx, v4, nil, 2); !errors.Is(err, ErrTooManyPending) {
			t.Errorf("third pending version = %v, want ErrTooManyPending", err)
		}
		for _, seq := range []int{2, 3} {
			if err := claimFin(ctx, st, a.ID, seq); err != nil {
				t.Fatalf("claim v%d: %v", seq, err)
			}
		}
		if got, err := st.FinalizeVersion(ctx, a.ID, 3, claimOf(a.ID, 3), nil); err != nil || got.CurrentSeq != 3 {
			t.Fatalf("finalize v3 = %+v, %v", got, err)
		}
		if got, err := st.FinalizeVersion(ctx, a.ID, 2, claimOf(a.ID, 2), nil); err != nil || got.CurrentSeq != 3 {
			t.Errorf("finalize v2 after v3 = %+v, %v; current must stay 3", got, err)
		}
		if page, err := st.ListVersions(ctx, a.ID, 3, 1); err != nil || len(page) != 1 || page[0].Seq != 2 {
			t.Errorf("ListVersions(before 3, limit 1) = %+v, %v; want [2]", page, err)
		}
		vs, err := st.ListVersions(ctx, a.ID, 0, 10)
		if err != nil || len(vs) != 3 || vs[0].Seq != 3 || vs[2].Seq != 1 {
			t.Errorf("ListVersions = %+v, %v; want 3,2,1", vs, err)
		}

		if got, err := st.GetArtifactByKey(ctx, ScopeKindProject, "project-1", PrincipalKindAgent, "agent-1", "k"); err != nil || got.ID != a.ID {
			t.Errorf("GetArtifactByKey = %+v, %v", got, err)
		}
		if _, err := st.GetArtifactByKey(ctx, ScopeKindProject, "project-1", PrincipalKindAgent, "agent-2", "k"); !errors.Is(err, ErrNotFound) {
			t.Errorf("other owner's key = %v, want ErrNotFound", err)
		}
		missing := &Version{ID: uuid.NewString(), ArtifactID: uuid.NewString(), Kind: VersionKindPublish, EntryPath: "x", CreatedAt: time.Now(), State: VersionStatePending}
		if err := st.CreateVersion(ctx, missing, nil, 2); !errors.Is(err, ErrNotFound) {
			t.Errorf("CreateVersion on a missing artifact = %v, want ErrNotFound", err)
		}
	})
}

func TestStoreCreatePendingKeyConflict(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, _ func() *sql.DB) {
		pendingArtifact(t, st, "same", "a.txt")
		now := time.Now()
		a := &Artifact{ID: uuid.NewString(), ScopeKind: ScopeKindProject, ScopeRef: "project-1",
			OwnerKind: PrincipalKindAgent, OwnerRef: "agent-1", Key: "same", Title: "t", CreatedAt: now, UpdatedAt: now}
		v := &Version{ID: uuid.NewString(), ArtifactID: a.ID, Seq: 1, Kind: VersionKindPublish, EntryPath: "a.txt", CreatedAt: now, State: VersionStatePending}
		if err := st.CreatePending(context.Background(), a, v, nil, nil); !errors.Is(err, ErrConflict) {
			t.Errorf("CreatePending with a taken key = %v, want ErrConflict", err)
		}
	})
}

func TestStoreCreateVersionConcurrent(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, reopen func() *sql.DB) {
		a, _ := pendingArtifact(t, st, "", "a.txt")
		const n = 6
		var wg sync.WaitGroup
		seqs := make([]int, n)
		errs := make([]error, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				other := NewStore(reopen(), driverOf(st))
				v := &Version{ID: uuid.NewString(), ArtifactID: a.ID, Kind: VersionKindPublish, EntryPath: "a.txt",
					CreatedAt: time.Now(), State: VersionStatePending}
				errs[i] = other.CreateVersion(context.Background(), v, nil, n+1)
				seqs[i] = v.Seq
			}(i)
		}
		wg.Wait()
		seen := map[int]bool{}
		for i := range seqs {
			if errs[i] != nil {
				t.Fatalf("CreateVersion %d: %v", i, errs[i])
			}
			if seen[seqs[i]] || seqs[i] < 2 || seqs[i] > n+1 {
				t.Errorf("seq %d duplicated or out of range: %v", seqs[i], seqs)
			}
			seen[seqs[i]] = true
		}
	})
}

func driverOf(st Store) string {
	if st.(*sqlStore).dialect == dialectPostgres {
		return "postgres"
	}
	return "sqlite"
}

func TestStoreReapPending(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		// An artifact with only an abandoned pending version.
		abandoned, av := pendingArtifact(t, st, "gone", "a.txt")
		// An artifact with a ready version and an abandoned second one.
		kept, kv := pendingArtifact(t, st, "", "a.txt")
		if err := st.MarkReceived(ctx, kv.ID, "a.txt", "text/plain", nil); err != nil {
			t.Fatal(err)
		}
		if err := claimFin(ctx, st, kept.ID, 1); err != nil {
			t.Fatal(err)
		}
		if _, err := st.FinalizeVersion(ctx, kept.ID, 1, claimOf(kept.ID, 1), nil); err != nil {
			t.Fatal(err)
		}
		stale := &Version{ID: uuid.NewString(), ArtifactID: kept.ID, Kind: VersionKindPublish, EntryPath: "a.txt",
			CreatedAt: time.Now(), State: VersionStatePending}
		if err := st.CreateVersion(ctx, stale, []File{{VersionID: stale.ID, Path: "a.txt", Size: 1, SHA256: strings.Repeat("ab", 32), MediaType: "text/plain", Pending: true}}, 4); err != nil {
			t.Fatal(err)
		}
		// The abandoned artifact's version was claimed by a finalize that
		// never completed: it is reaped like a pending one.
		if err := st.MarkReceived(ctx, av.ID, "a.txt", "text/plain", nil); err != nil {
			t.Fatal(err)
		}
		if err := claimFin(ctx, st, abandoned.ID, 1); err != nil {
			t.Fatal(err)
		}
		// A fresh pending version is not reaped.
		fresh, _ := pendingArtifact(t, st, "", "a.txt")

		// Age everything but the fresh artifact's version.
		old := st.(*sqlStore).timeArg(time.Now().Add(-48 * time.Hour))
		for _, id := range []string{av.ID, stale.ID} {
			if _, err := db.Exec(st.(*sqlStore).rebind("UPDATE artifact_version SET created_at = ? WHERE id = ?"), old, id); err != nil {
				t.Fatal(err)
			}
		}
		n, err := st.ReapPending(ctx, time.Now().Add(-24*time.Hour), 100)
		if err != nil || n != 2 {
			t.Fatalf("ReapPending = %d, %v; want 2", n, err)
		}
		if _, err := st.GetArtifact(ctx, abandoned.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("artifact with nothing left is still live: %v", err)
		}
		if got, err := st.GetArtifact(ctx, kept.ID); err != nil || got.CurrentSeq != 1 {
			t.Errorf("artifact with a ready version = %+v, %v", got, err)
		}
		if gv, err := st.GetVersion(ctx, kept.ID, 2); err != nil || gv.State != VersionStateFailed {
			t.Errorf("reaped version = %+v, %v; want failed", gv, err)
		}
		if files, err := st.ListFiles(ctx, stale.ID); err != nil || len(files) != 0 {
			t.Errorf("reaped version keeps its manifest: %+v, %v", files, err)
		}
		if _, err := st.FinalizeVersion(ctx, kept.ID, 2, claimOf(kept.ID, 2), nil); !errors.Is(err, ErrConflict) {
			t.Errorf("finalizing a reaped version = %v, want ErrConflict", err)
		}
		if got, err := st.GetArtifact(ctx, fresh.ID); err != nil || got.ID != fresh.ID {
			t.Errorf("fresh pending artifact reaped: %v", err)
		}
		// The freed key can be used again.
		pendingArtifact(t, st, "gone", "a.txt")
		if n, err := st.ReapPending(ctx, time.Now().Add(-24*time.Hour), 100); err != nil || n != 0 {
			t.Errorf("second ReapPending = %d, %v; want 0", n, err)
		}
	})
}

// TestStoreConcurrentFinalizes: two versions finalized at once leave the
// higher one current, whichever commits last.
func TestStoreConcurrentFinalizes(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, reopen func() *sql.DB) {
		ctx := context.Background()
		a, v1 := pendingArtifact(t, st, "", "a.txt")
		if err := st.MarkReceived(ctx, v1.ID, "a.txt", "text/plain", nil); err != nil {
			t.Fatal(err)
		}
		var seqs []int
		for i := 0; i < 2; i++ {
			v := &Version{ID: uuid.NewString(), ArtifactID: a.ID, Kind: VersionKindPublish, EntryPath: "a.txt", CreatedAt: time.Now(), State: VersionStatePending}
			if err := st.CreateVersion(ctx, v, nil, 4); err != nil {
				t.Fatal(err)
			}
			seqs = append(seqs, v.Seq)
		}
		for _, seq := range append([]int{1}, seqs...) {
			if err := claimFin(ctx, st, a.ID, seq); err != nil {
				t.Fatal(err)
			}
		}
		var wg sync.WaitGroup
		errs := make([]error, 3)
		for i, seq := range append([]int{1}, seqs...) {
			wg.Add(1)
			go func(i, seq int) {
				defer wg.Done()
				_, errs[i] = NewStore(reopen(), driverOf(st)).FinalizeVersion(ctx, a.ID, seq, claimOf(a.ID, seq), nil)
			}(i, seq)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("finalize %d: %v", i, err)
			}
		}
		got, err := st.GetArtifact(ctx, a.ID)
		if err != nil || got.CurrentSeq != seqs[1] {
			t.Errorf("current = %+v, %v; want %d", got, err, seqs[1])
		}
	})
}

// TestStoreReapRacesFinalize: a reap and a finalize of the same claimed
// version never both succeed, and the version ends ready or failed.
func TestStoreReapRacesFinalize(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, reopen func() *sql.DB) {
		ctx := context.Background()
		for round := 0; round < 5; round++ {
			a, v := pendingArtifact(t, st, "", "a.txt")
			if err := st.MarkReceived(ctx, v.ID, "a.txt", "text/plain", nil); err != nil {
				t.Fatal(err)
			}
			if err := claimFin(ctx, st, a.ID, 1); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(st.(*sqlStore).rebind("UPDATE artifact_version SET created_at = ? WHERE id = ?"),
				st.(*sqlStore).timeArg(time.Now().Add(-48*time.Hour)), v.ID); err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			var finErr error
			var reaped int
			wg.Add(2)
			go func() {
				defer wg.Done()
				_, finErr = NewStore(reopen(), driverOf(st)).FinalizeVersion(ctx, a.ID, 1, claimOf(a.ID, 1), nil)
			}()
			go func() {
				defer wg.Done()
				reaped, _ = NewStore(reopen(), driverOf(st)).ReapPending(ctx, time.Now().Add(-24*time.Hour), 100)
			}()
			wg.Wait()
			gv, err := st.GetVersion(ctx, a.ID, 1)
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case finErr == nil && reaped == 0 && gv.State == VersionStateReady:
			case (errors.Is(finErr, ErrConflict) || errors.Is(finErr, ErrNotFound)) && reaped == 1 && gv.State == VersionStateFailed:
			default:
				t.Fatalf("round %d: finalize %v, reaped %d, state %s", round, finErr, reaped, gv.State)
			}
		}
	})
}

// staleClaim is the cutoff the service passes: claims older than this may
// be taken over.
func staleClaim() time.Time { return time.Now().Add(-staleFinalizeClaim) }

// TestStoreStaleFinalizeClaimIsTakenOver: a claim left behind by a finalize
// that never completed can be taken over once it is older than the cutoff;
// a recent one cannot.
func TestStoreStaleFinalizeClaimIsTakenOver(t *testing.T) {
	forEachBackend(t, func(t *testing.T, db *sql.DB, st Store, _ func() *sql.DB) {
		ctx := context.Background()
		a, v := pendingArtifact(t, st, "", "a.txt")
		if err := st.MarkReceived(ctx, v.ID, "a.txt", "text/plain", nil); err != nil {
			t.Fatal(err)
		}
		if err := claimFin(ctx, st, a.ID, 1); err != nil {
			t.Fatal(err)
		}
		if err := claimFin(ctx, st, a.ID, 1); !errors.Is(err, ErrConflict) {
			t.Fatalf("recent claim taken over: %v", err)
		}
		old := st.(*sqlStore).timeArg(time.Now().Add(-staleFinalizeClaim - time.Minute))
		if _, err := db.Exec(st.(*sqlStore).rebind("UPDATE artifact_version SET claimed_at = ? WHERE id = ?"), old, v.ID); err != nil {
			t.Fatal(err)
		}
		first := claimOf(a.ID, 1)
		if err := claimFin(ctx, st, a.ID, 1); err != nil {
			t.Fatalf("stale claim not taken over: %v", err)
		}
		// The request that lost its claim can neither release nor finalize
		// the version any more.
		if err := st.ReleaseFinalize(ctx, a.ID, 1, first); err != nil {
			t.Fatal(err)
		}
		if gv, _ := st.GetVersion(ctx, a.ID, 1); gv.State != VersionStateFinalizing {
			t.Fatalf("an old claim's release changed the version to %s", gv.State)
		}
		if _, err := st.FinalizeVersion(ctx, a.ID, 1, first, nil); !errors.Is(err, ErrConflict) {
			t.Fatalf("finalize under an old claim = %v, want ErrConflict", err)
		}
		if got, err := st.FinalizeVersion(ctx, a.ID, 1, claimOf(a.ID, 1), nil); err != nil || got.CurrentSeq != 1 {
			t.Fatalf("finalize after takeover = %+v, %v", got, err)
		}
	})
}

// claims holds the claim each test finalize made, by artifact and seq.
var claims sync.Map

func claimKey(id string, seq int) string { return fmt.Sprintf("%s/%d", id, seq) }

// claimFin claims version seq of artifact id for finalize and remembers
// the claim for claimOf.
func claimFin(ctx context.Context, st Store, id string, seq int) error {
	c, err := st.ClaimFinalize(ctx, id, seq, staleClaim())
	if err == nil {
		claims.Store(claimKey(id, seq), c)
	}
	return err
}

// claimOf is the last claim claimFin made for version seq of artifact id.
func claimOf(id string, seq int) time.Time {
	v, _ := claims.Load(claimKey(id, seq))
	c, _ := v.(time.Time)
	return c
}
