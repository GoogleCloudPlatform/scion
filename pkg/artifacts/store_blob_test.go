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
	"io"
	"math"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
)

// TestBlobDeadlineScaling checks that blobDeadline keeps the minimum floor
// for small blobs, scales linearly with size, caps at blobWriteMaxDeadline
// without overflowing, and stays below MinGCGrace.
func TestBlobDeadlineScaling(t *testing.T) {
	if blobWriteMaxDeadline >= MinGCGrace {
		t.Fatalf("blobWriteMaxDeadline (%s) must stay below MinGCGrace (%s)", blobWriteMaxDeadline, MinGCGrace)
	}

	cases := []struct {
		name string
		size int64
		want time.Duration
	}{
		{"negative", -1, 60 * time.Second},
		{"zero", 0, 60 * time.Second},
		{"small_1MiB", 1 << 20, 61 * time.Second},
		{"threshold_32MiB", 32 << 20, 92 * time.Second},
		{"large_256MiB", 256 << 20, 316 * time.Second},
		{"large_1GiB", 1 << 30, 1084 * time.Second},
		{"capped_10GiB", 10 << 30, blobWriteMaxDeadline},
		{"overflow_MaxInt64", math.MaxInt64, blobWriteMaxDeadline},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := blobDeadline(tc.size); got != tc.want {
				t.Errorf("blobDeadline(%d) = %s, want %s", tc.size, got, tc.want)
			}
		})
	}

	// Sub-second test overrides still respect the floor and cap cleanly.
	oldFloor, oldCap := blobWriteDeadline, blobWriteMaxDeadline
	blobWriteDeadline, blobWriteMaxDeadline = 50*time.Millisecond, 500*time.Millisecond
	t.Cleanup(func() { blobWriteDeadline, blobWriteMaxDeadline = oldFloor, oldCap })
	if got := blobDeadline(8); got < 50*time.Millisecond || got > 51*time.Millisecond {
		t.Errorf("sub-second small blobDeadline(8) = %s, want ~50ms", got)
	}
	if got := blobDeadline(1 << 20); got != 500*time.Millisecond {
		t.Errorf("sub-second capped blobDeadline(1MiB) = %s, want 500ms", got)
	}
}

// recordingBlobStorage wraps storage.Storage to capture Upload options and
// the context deadline passed by storeBlob.
type recordingBlobStorage struct {
	storage.Storage
	lastOpts     storage.UploadOptions
	lastDeadline time.Time
	hasDeadline  bool
}

func (r *recordingBlobStorage) Upload(ctx context.Context, p string, body io.Reader, opts storage.UploadOptions) (*storage.Object, error) {
	r.lastOpts = opts
	r.lastDeadline, r.hasDeadline = ctx.Deadline()
	return r.Storage.Upload(ctx, p, body, opts)
}

// TestStoreBlobThresholdAndContextDeadline verifies that storeBlob selects
// single-PUT (Idempotent: true) up to blobResumableThreshold, switches to the
// resumable writer (Idempotent: false) above it, never sets IfGenerationMatch,
// and propagates the size-scaled deadline on the upload context.
func TestStoreBlobThresholdAndContextDeadline(t *testing.T) {
	f := newFixture(t, false)
	rec := &recordingBlobStorage{Storage: f.local}
	f.svc.SetBlobStorage(rec, "hub-1")
	b, ok := f.svc.backend()
	if !ok {
		t.Fatal("backend unavailable")
	}

	// Boundary checks around blobResumableThreshold via the real publish path.
	oldThresh := blobResumableThreshold
	blobResumableThreshold = 64
	t.Cleanup(func() { blobResumableThreshold = oldThresh })

	for _, tc := range []struct {
		name           string
		size           int
		wantIdempotent bool
	}{
		{"below_threshold", 63, true},
		{"at_threshold", 64, true},
		{"above_threshold", 65, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := bytes.Repeat([]byte("a"), tc.size)
			f.publish(userU, tc.name+".bin", body, "scope=project-1")
			if rec.lastOpts.Idempotent != tc.wantIdempotent {
				t.Errorf("size=%d: Idempotent = %v, want %v", tc.size, rec.lastOpts.Idempotent, tc.wantIdempotent)
			}
			if rec.lastOpts.IfGenerationMatch != nil {
				t.Errorf("size=%d: IfGenerationMatch = %v, want nil (must overwrite)", tc.size, *rec.lastOpts.IfGenerationMatch)
			}
		})
	}

	// Verify context deadline scaling directly with realistic large sizes
	// without allocating hundreds of MiB.
	blobResumableThreshold = oldThresh
	ctx := context.Background()
	sample := []byte("stub")
	digest := sha(sample)
	open := func() (io.Reader, error) { return bytes.NewReader(sample), nil }

	beforeSmall := time.Now()
	if err := storeBlob(ctx, b, digest, "application/octet-stream", 1024, open); err != nil {
		t.Fatalf("small storeBlob: %v", err)
	}
	if !rec.hasDeadline {
		t.Fatal("small storeBlob did not set a context deadline")
	}
	smallRemaining := rec.lastDeadline.Sub(beforeSmall)
	if smallRemaining < blobWriteDeadline-5*time.Second || smallRemaining > blobWriteDeadline+5*time.Second {
		t.Errorf("small storeBlob deadline = %s, want ~%s", smallRemaining, blobWriteDeadline)
	}
	if !rec.lastOpts.Idempotent {
		t.Errorf("small storeBlob: Idempotent = false, want true")
	}

	const largeSize int64 = 256 << 20 // 256 MiB
	wantLarge := blobDeadline(largeSize)
	beforeLarge := time.Now()
	if err := storeBlob(ctx, b, digest, "application/octet-stream", largeSize, open); err != nil {
		t.Fatalf("large storeBlob: %v", err)
	}
	if !rec.hasDeadline {
		t.Fatal("large storeBlob did not set a context deadline")
	}
	largeRemaining := rec.lastDeadline.Sub(beforeLarge)
	if largeRemaining < wantLarge-5*time.Second || largeRemaining > wantLarge+5*time.Second {
		t.Errorf("large storeBlob deadline = %s, want ~%s", largeRemaining, wantLarge)
	}
	if rec.lastOpts.Idempotent {
		t.Errorf("large storeBlob: Idempotent = true, want false (resumable writer)")
	}
	if rec.lastOpts.IfGenerationMatch != nil {
		t.Errorf("large storeBlob: IfGenerationMatch = %v, want nil", *rec.lastOpts.IfGenerationMatch)
	}
}

// TestStoreBlobLargePreservesTouchOverwriteProtocol verifies that a blob
// uploaded on the resumable path (above blobResumableThreshold) still touches
// the blob before upload and unconditionally overwrites the object to a new
// generation, so a late generation-pinned sweep delete cannot remove it.
func TestStoreBlobLargePreservesTouchOverwriteProtocol(t *testing.T) {
	oldThresh := blobResumableThreshold
	blobResumableThreshold = 32
	t.Cleanup(func() { blobResumableThreshold = oldThresh })

	f := newFixture(t, false)
	vs := newVersionedStorage(f.local)
	f.svc.SetBlobStorage(vs, "hub-1")

	body := bytes.Repeat([]byte("L"), 64)
	d := sha(body)
	g := &BlobSweeper{}

	// Publish once, soft-delete, and let the sweep mark it as a stale orphan.
	staleOrphan(t, f, vs, g, body)
	if vs.lastOpts.Idempotent {
		t.Fatalf("initial large publish used Idempotent=true, want false")
	}
	genBefore := vs.gen[BlobPath("hub-1", d)]

	// Queue a late generation-pinned delete from the sweep.
	vs.late = true
	if _, _, err := g.Sweep(context.Background(), f.store, vs, "hub-1", DefaultGCGrace, time.Now()); err == nil {
		t.Fatal("the late delete did not fail the pass")
	}
	vs.late = false

	// Re-publish the same large blob: storeBlob must touch it and overwrite
	// unconditionally to a new generation, using the resumable path.
	f.publish(userU, "large-repub.bin", body, "scope=project-1")
	if vs.lastOpts.Idempotent {
		t.Errorf("re-publish used Idempotent=true, want false")
	}
	if vs.lastOpts.IfGenerationMatch != nil {
		t.Errorf("re-publish set IfGenerationMatch=%v, want nil", *vs.lastOpts.IfGenerationMatch)
	}
	if genAfter := vs.gen[BlobPath("hub-1", d)]; genAfter <= genBefore {
		t.Errorf("re-publish did not advance generation: before=%d after=%d", genBefore, genAfter)
	}

	// The late delete from the sweep arrives after the overwrite and must not
	// remove the newly written generation.
	vs.applyLate(context.Background())
	if !blobReadable(t, vs, d) {
		t.Fatal("large blob was removed by a late sweep delete after overwrite")
	}
}
