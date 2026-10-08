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
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// MaxBlobBatch bounds the digests one MarkBlobs call binds, below every
// driver's placeholder limit.
const MaxBlobBatch = 500

// blobReferenced is the condition that a live artifact references digest
// sha: a file of a ready, pending or finalizing version of a non-deleted
// artifact. Soft-deleted artifacts are not references (their grace period
// is the undo window); failed versions have no files (ReapPending drops
// them).
const blobReferencedFrom = `artifact_file f
	JOIN artifact_version v ON v.id = f.version_id
	JOIN artifact a ON a.id = v.artifact_id
	WHERE a.deleted_at IS NULL AND v.state IN (?, ?, ?)`

var blobReferencedStates = []any{VersionStateReady, VersionStatePending, VersionStateFinalizing}

// TouchBlob implements Store.
func (s *sqlStore) TouchBlob(ctx context.Context, digest string, now time.Time) error {
	// The upsert waits for a sweep holding the row (Postgres row lock,
	// SQLite write lock).
	if _, err := s.db.ExecContext(ctx, s.rebind(`INSERT INTO artifact_blob (sha256, touched_at) VALUES (?, ?)
		ON CONFLICT (sha256) DO UPDATE SET touched_at = excluded.touched_at`), digest, s.timeArg(now)); err != nil {
		return fmt.Errorf("artifacts: touch blob: %w", err)
	}
	return nil
}

// MarkBlobs implements Store.
func (s *sqlStore) MarkBlobs(ctx context.Context, blobs []BlobMark, now time.Time) error {
	if len(blobs) == 0 {
		return nil
	}
	if len(blobs) > MaxBlobBatch {
		return fmt.Errorf("artifacts: MarkBlobs accepts at most %d digests", MaxBlobBatch)
	}
	args := append([]any{}, blobReferencedStates...)
	for _, b := range blobs {
		args = append(args, b.Digest)
	}
	in := strings.TrimSuffix(strings.Repeat("?, ", len(blobs)), ", ")
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("artifacts: begin mark: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, s.rebind(`SELECT DISTINCT f.sha256 FROM `+blobReferencedFrom+
		` AND f.sha256 IN (`+in+`)`), args...)
	if err != nil {
		return fmt.Errorf("artifacts: find referenced blobs: %w", err)
	}
	referenced := map[string]bool{}
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			_ = rows.Close()
			return fmt.Errorf("artifacts: scan referenced blob: %w", err)
		}
		referenced[d] = true
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("artifacts: find referenced blobs: %w", err)
	}
	at := s.timeArg(now)
	for _, b := range blobs {
		if referenced[b.Digest] {
			if _, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM artifact_blob WHERE sha256 = ?`), b.Digest); err != nil {
				return fmt.Errorf("artifacts: clear blob state: %w", err)
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO artifact_blob (sha256, unreferenced_since, generation) VALUES (?, ?, ?)
			ON CONFLICT (sha256) DO UPDATE SET unreferenced_since = COALESCE(artifact_blob.unreferenced_since, excluded.unreferenced_since),
			generation = excluded.generation`),
			b.Digest, at, b.Generation); err != nil {
			return fmt.Errorf("artifacts: mark blob: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("artifacts: commit mark: %w", err)
	}
	return nil
}

// reclaimCandidateHook, when set (by tests only), runs between finding a
// reclaimable blob and locking it, where a concurrent publish could touch
// or reference it.
var reclaimCandidateHook func(digest string)

// ReclaimBlobs implements Store.
func (s *sqlStore) ReclaimBlobs(ctx context.Context, cutoff time.Time, limit int, del func(string, int64) error) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	at := s.timeArg(cutoff)
	rows, err := s.db.QueryContext(ctx, s.rebind(`SELECT sha256 FROM artifact_blob
		WHERE unreferenced_since IS NOT NULL AND unreferenced_since <= ?
		ORDER BY unreferenced_since LIMIT `+strconv.Itoa(limit)), at)
	if err != nil {
		return 0, fmt.Errorf("artifacts: find reclaimable blobs: %w", err)
	}
	var found []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("artifacts: scan reclaimable blob: %w", err)
		}
		found = append(found, d)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("artifacts: find reclaimable blobs: %w", err)
	}
	n := 0
	for _, d := range found {
		if reclaimCandidateHook != nil {
			reclaimCandidateHook(d)
		}
		ok, err := s.reclaimOne(ctx, d, at, del)
		if err != nil {
			return n, err
		}
		if ok {
			n++
		}
	}
	return n, nil
}

// reclaimOne deletes one blob if, under its state row's lock, it is still
// marked unreferenced since at or before cutoff, untouched since cutoff (a
// writer touches before it uploads) and unreferenced.
func (s *sqlStore) reclaimOne(ctx context.Context, digest string, cutoff any, del func(string, int64) error) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("artifacts: begin reclaim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// The write takes the row's lock (Postgres) or the write lock
	// (SQLite) and re-checks the state in one statement.
	res, err := tx.ExecContext(ctx, s.rebind(`UPDATE artifact_blob SET sha256 = sha256
		WHERE sha256 = ? AND unreferenced_since IS NOT NULL AND unreferenced_since <= ?
		AND (touched_at IS NULL OR touched_at <= ?)`), digest, cutoff, cutoff)
	if err != nil {
		return false, fmt.Errorf("artifacts: lock blob: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return false, err
	}
	var refs int
	args := append(append([]any{}, blobReferencedStates...), digest)
	if err := tx.QueryRowContext(ctx, s.rebind(`SELECT COUNT(*) FROM `+blobReferencedFrom+` AND f.sha256 = ?`), args...).Scan(&refs); err != nil {
		return false, fmt.Errorf("artifacts: check blob references: %w", err)
	}
	if refs > 0 {
		if _, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM artifact_blob WHERE sha256 = ?`), digest); err != nil {
			return false, fmt.Errorf("artifacts: clear blob state: %w", err)
		}
		return false, commit(tx)
	}
	var gen sql.NullInt64
	if err := tx.QueryRowContext(ctx, s.rebind(`SELECT generation FROM artifact_blob WHERE sha256 = ?`), digest).Scan(&gen); err != nil {
		return false, fmt.Errorf("artifacts: read blob generation: %w", err)
	}
	if err := del(digest, gen.Int64); err != nil {
		return false, fmt.Errorf("artifacts: delete blob: %w", err)
	}
	if _, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM artifact_blob WHERE sha256 = ?`), digest); err != nil {
		return false, fmt.Errorf("artifacts: clear blob state: %w", err)
	}
	return true, commit(tx)
}

func commit(tx *sql.Tx) error {
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("artifacts: commit: %w", err)
	}
	return nil
}
