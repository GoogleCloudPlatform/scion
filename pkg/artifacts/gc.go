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

package artifacts

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
)

// INVARIANT: every digest referenced by a ready, pending or finalizing
// version of a live artifact is readable from blob storage, at every point
// of any interleaving of writes, marks and sweeps, including deletes that
// time out and still reach the object store later.
//
// Why it holds. The rules, each enforced in one place:
//
//  R1. References are the files of every pending, finalizing or ready
//      version of a non-deleted artifact, whether or not their bytes have
//      arrived (blobReferencedFrom). A blob is deleted only when, under its
//      state row's lock (reclaimOne), it has been marked unreferenced since
//      before the grace period, has not been touched since before it, and
//      still has no reference.
//  R2. Every write of bytes goes through storeBlob: it touches the blob
//      (waiting for a sweep holding the row) and then always uploads,
//      overwriting any existing object. No write relies on an object that
//      is already there.
//  R3. A delete on a provider that versions objects names the generation
//      the sweep listed (DeleteIfGeneration); a provider without versions
//      deletes synchronously. A versioned provider without a known
//      generation gets no delete.
//
// The paths that create a reference:
//
//   - Single-file publish, PUT of a pending file, fetched remote images at
//     finalize: storeBlob (R2) before the reference is recorded. The touch
//     keeps the sweep off the blob for the grace period (R1), longer than
//     the request; the upload's new generation outlives any delete issued
//     before it (R3).
//   - Two-step create or append: its manifest rows are references from the
//     moment they are written (R1), before any upload; the upload then goes
//     through storeBlob.
//   - Carry-forward (a file identical to the current version's needs no
//     upload): its digest is already referenced by the current version,
//     continuously since that version's own write (R1, R2), so no delete
//     can have been decided for it since; a delete issued before that write
//     names an older generation (R3).
//
// Unchanged files still share one stored object: a digest has one object
// path, which writes overwrite. TestBlobGCInterleavings checks the
// invariant over random sequences; the deterministic tests pin each path.
//
// Blob garbage collection (design §6). A soft-deleted artifact is not a
// reference; the grace period is its undo window. Abandoned pending
// versions should be reaped (ReapPending) before a sweep, so their files
// stop counting.

const (
	// DefaultGCGrace is the default grace period of the blob sweep.
	DefaultGCGrace = 168 * time.Hour
	// MinGCGrace is the shortest grace period the sweep uses, whatever it
	// is given.
	MinGCGrace = 24 * time.Hour

	// gcListBatch bounds the blobs one sweep pass lists and marks.
	gcListBatch = 1000
	// gcReclaimBatch bounds the blobs one sweep pass deletes.
	gcReclaimBatch = 200
)

// staleUploadTempAge is how old a temporary upload file must be before the
// sweep removes it; an upload in progress is far younger.
const staleUploadTempAge = time.Hour

// gcDeleteTimeout bounds one blob delete. The delete runs inside the store
// transaction holding the blob's state, so it stays below SQLite's busy
// timeout (5s in the hub's DSN) and a slow object store cannot make other
// writers time out. A variable so tests can lower it.
var gcDeleteTimeout = 4 * time.Second

var errUnknownGeneration = errors.New("artifacts: blob generation unknown; not deleted")

// deleteBlob deletes the blob at p within gcDeleteTimeout. A timeout rolls
// the sweep's transaction back and the blob's mark stays. A delete the
// object store applies after that is harmless: on a versioned provider it
// names the generation the sweep saw, and every write since stored a new
// one (storeBlob always uploads). A missing object or a changed generation
// counts as done: the bytes the sweep meant to delete are gone or were
// replaced. A provider that versions objects but gives no generation for
// this blob gets no delete at all: the blob and its mark stay.
func deleteBlob(ctx context.Context, blobs storage.Storage, p string, generation int64) error {
	dctx, cancel := context.WithTimeout(ctx, gcDeleteTimeout)
	defer cancel()
	var err error
	if gd, ok := blobs.(storage.GenerationDeleter); ok {
		if generation == 0 {
			// Without the generation the delete could not be made safe
			// against arriving late; keep the blob and its mark.
			return errUnknownGeneration
		}
		err = gd.DeleteIfGeneration(dctx, p, generation)
	} else {
		err = blobs.Delete(dctx, p)
	}
	if errors.Is(err, storage.ErrNotFound) || errors.Is(err, storage.ErrPreconditionFailed) {
		return nil
	}
	return err
}

// BlobSweeper sweeps the blobs of one hub, a page of the blob listing per
// pass. It is not safe for concurrent use; run one per hub.
type BlobSweeper struct {
	cursor string
}

// Sweep runs one pass: it lists up to gcListBatch blobs from where the
// last pass stopped (wrapping at the end), marks each as referenced or not
// at now, then deletes up to gcReclaimBatch blobs unreferenced and
// untouched for grace (at least MinGCGrace). It returns how many blobs it
// listed and deleted.
func (g *BlobSweeper) Sweep(ctx context.Context, st Store, blobs storage.Storage, hubID string, grace time.Duration, now time.Time) (listed, deleted int, err error) {
	if st == nil || blobs == nil || hubID == "" {
		return 0, 0, errors.New("artifacts: blob sweep needs a store, blob storage and a hub id")
	}
	grace = max(grace, MinGCGrace)
	prefix := "hubs/" + hubID + "/artifacts/blobs/sha256/"
	if g.cursor == "" {
		// At the start of each walk, remove temporary upload files a
		// crash left behind (local storage writes through one).
		if tc, ok := blobs.(storage.TempCleaner); ok {
			if _, err := tc.RemoveStaleTemps(ctx, prefix, staleUploadTempAge); err != nil {
				return 0, 0, err
			}
		}
	}
	res, err := blobs.List(ctx, storage.ListOptions{Prefix: prefix, MaxResults: gcListBatch, StartOffset: g.cursor})
	if err != nil {
		return 0, 0, err
	}
	var marks []BlobMark
	for _, o := range res.Objects {
		if d, ok := blobDigest(hubID, o.Name); ok {
			marks = append(marks, BlobMark{Digest: d, Generation: o.Generation})
		}
	}
	for start := 0; start < len(marks); start += MaxBlobBatch {
		if err := st.MarkBlobs(ctx, marks[start:min(start+MaxBlobBatch, len(marks))], now); err != nil {
			return len(marks), 0, err
		}
	}
	g.cursor = res.NextOffset
	deleted, err = st.ReclaimBlobs(ctx, now.Add(-grace), gcReclaimBatch, func(d string, generation int64) error {
		return deleteBlob(ctx, blobs, BlobPath(hubID, d), generation)
	})
	return len(marks), deleted, err
}

// blobDigest returns the digest of the blob stored at name, which must be
// exactly BlobPath(hubID, digest) for a lowercase hex SHA-256 digest.
func blobDigest(hubID, name string) (string, bool) {
	name = strings.TrimPrefix(name, "/")
	i := strings.LastIndexByte(name, '/')
	if i < 0 {
		return "", false
	}
	d := name[i+1:]
	if !isHexDigest(d) || BlobPath(hubID, d) != name {
		return "", false
	}
	return d, true
}
