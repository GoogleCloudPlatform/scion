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

// Blob garbage collection (design §6). Blobs are shared by content across
// versions and artifacts, so a blob is deleted only when no live artifact
// references it (Store.MarkBlobs: a file of a ready, pending or finalizing
// version of a non-deleted artifact), it has been unreferenced and
// untouched by a publish for the grace period, and a final check under the
// blob's state row lock still finds it so (Store.ReclaimBlobs). A
// soft-deleted artifact is not a reference; the grace period is its undo
// window. Abandoned pending versions should be reaped (ReapPending) before
// a sweep, so their files stop counting.

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
	res, err := blobs.List(ctx, storage.ListOptions{Prefix: prefix, MaxResults: gcListBatch, StartOffset: g.cursor})
	if err != nil {
		return 0, 0, err
	}
	var digests []string
	for _, o := range res.Objects {
		if d, ok := blobDigest(hubID, o.Name); ok {
			digests = append(digests, d)
		}
	}
	for start := 0; start < len(digests); start += MaxBlobBatch {
		if err := st.MarkBlobs(ctx, digests[start:min(start+MaxBlobBatch, len(digests))], now); err != nil {
			return len(digests), 0, err
		}
	}
	g.cursor = res.NextOffset
	deleted, err = st.ReclaimBlobs(ctx, now.Add(-grace), gcReclaimBatch, func(d string) error {
		err := blobs.Delete(ctx, BlobPath(hubID, d))
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		return err
	})
	return len(digests), deleted, err
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
