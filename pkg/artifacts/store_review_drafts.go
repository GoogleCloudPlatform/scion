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
	"errors"
	"fmt"
	"strconv"
	"time"
)

// ReviewDraft is one reviewer's saved draft review of an artifact
// (ptone/scion#7116). One row exists per (artifact, principal): the
// principal is part of the primary key, so no lookup or write can address
// another principal's draft.
type ReviewDraft struct {
	ArtifactID     string
	PrincipalKind  string
	PrincipalRef   string
	Rev            int
	BaseSeq        int
	EntryPath      string
	Body           string
	Note           string
	CarriedBaseSeq *int
	CarriedBody    *string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	ClientSavedAt  *time.Time
}

// ReviewDraftKey identifies one saved review draft row without its body or
// note. It is used by the periodic lost-access reaper to walk drafts in
// primary-key order.
type ReviewDraftKey struct {
	ArtifactID    string
	PrincipalKind string
	PrincipalRef  string
}

// ErrDraftConflict is wrapped by *DraftConflictError when a conditional write
// or delete of a review draft finds a different stored revision.
var ErrDraftConflict = errors.New("artifacts: review draft conflict")

// DraftConflictError reports the stored draft's revision, save time and base
// version when a conditional write or delete conflicts. It never carries the
// body, carried body or note. Rev is 0 when an update expected an existing
// row and none exists.
type DraftConflictError struct {
	Rev     int
	SavedAt time.Time
	BaseSeq int
}

func (e *DraftConflictError) Error() string {
	return fmt.Sprintf("%s: stored rev %d", ErrDraftConflict, e.Rev)
}

func (e *DraftConflictError) Unwrap() error { return ErrDraftConflict }

// ErrTooManyDrafts is returned by PutReviewDraft when inserting a new draft
// would push the principal past maxPerPrincipal saved drafts across the hub.
var ErrTooManyDrafts = errors.New("artifacts: too many saved review drafts")

// migrationReviewDrafts adds the per-reviewer, per-artifact durable review
// draft table (ptone/scion#7116, ptone/scion#8470). The migration is purely
// additive (a new table and its indexes), so pkg/version.DowngradeFloor does
// not change.
const migrationReviewDrafts = "0015_review_drafts"

const sqliteReviewDrafts = `
CREATE TABLE IF NOT EXISTS artifact_review_draft (
    artifact_id      TEXT NOT NULL REFERENCES artifact (id),
    principal_kind   TEXT NOT NULL,
    principal_ref    TEXT NOT NULL,
    rev              INTEGER NOT NULL,
    base_seq         INTEGER NOT NULL,
    entry_path       TEXT NOT NULL,
    body             TEXT NOT NULL,
    note             TEXT NOT NULL,
    carried_base_seq INTEGER,
    carried_body     TEXT,
    own_marks        TEXT,
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL,
    client_saved_at  TEXT,
    PRIMARY KEY (artifact_id, principal_kind, principal_ref)
);
CREATE INDEX IF NOT EXISTS idx_artifact_review_draft_principal
    ON artifact_review_draft (principal_kind, principal_ref, updated_at);
CREATE INDEX IF NOT EXISTS idx_artifact_review_draft_reap
    ON artifact_review_draft (updated_at);
`

const postgresReviewDrafts = `
CREATE TABLE IF NOT EXISTS artifact_review_draft (
    artifact_id      TEXT NOT NULL REFERENCES artifact (id),
    principal_kind   TEXT NOT NULL,
    principal_ref    TEXT NOT NULL,
    rev              INTEGER NOT NULL,
    base_seq         INTEGER NOT NULL,
    entry_path       TEXT NOT NULL,
    body             TEXT NOT NULL,
    note             TEXT NOT NULL,
    carried_base_seq INTEGER,
    carried_body     TEXT,
    own_marks        TEXT,
    created_at       TIMESTAMPTZ NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL,
    client_saved_at  TIMESTAMPTZ,
    PRIMARY KEY (artifact_id, principal_kind, principal_ref)
);
CREATE INDEX IF NOT EXISTS idx_artifact_review_draft_principal
    ON artifact_review_draft (principal_kind, principal_ref, updated_at);
CREATE INDEX IF NOT EXISTS idx_artifact_review_draft_reap
    ON artifact_review_draft (updated_at);
`

// reviewDraftLockClass is the first key of the two-key Postgres advisory
// transaction lock that serializes PutReviewDraft inserts per principal (C3:
// exact per-principal cap via advisory lock on Postgres and write-first
// transaction on SQLite; the bounded-overshoot alternative was not chosen).
// Two-key advisory locks live in a separate lock space from the one-key Init
// lock (initLockKey) and use a distinct class from reviewSessionLockClass. A
// hashtext collision between two principals only makes their inserts wait for
// each other; the count query still filters by exact (principal_kind,
// principal_ref).
const reviewDraftLockClass int32 = 0x61727264 // "arrd"

// reviewDraftPutHook, when set by a test in this package, runs inside
// PutReviewDraft's insert transaction after the INSERT and before the
// per-principal count check, so a test can hold one insert there while
// another races. It is nil in production.
var reviewDraftPutHook func(ctx context.Context)

// PutReviewDraft implements Store.
func (s *sqlStore) PutReviewDraft(ctx context.Context, d ReviewDraft, expectRev int, now time.Time, maxPerPrincipal int) (ReviewDraft, error) {
	if d.ArtifactID == "" || d.PrincipalKind == "" || d.PrincipalRef == "" || d.EntryPath == "" {
		return ReviewDraft{}, errors.New("artifacts: PutReviewDraft needs an artifact, a principal and an entry path")
	}
	if expectRev < 0 {
		return ReviewDraft{}, errors.New("artifacts: PutReviewDraft expectRev must be non-negative")
	}
	if (d.CarriedBaseSeq == nil) != (d.CarriedBody == nil) {
		return ReviewDraft{}, errors.New("artifacts: PutReviewDraft carried base and body must both be set or both nil")
	}

	at := now.UTC().Truncate(time.Microsecond)
	var clientSaved *time.Time
	if d.ClientSavedAt != nil {
		cs := d.ClientSavedAt.UTC().Truncate(time.Microsecond)
		clientSaved = &cs
	}
	var carriedBase, carriedBody any
	if d.CarriedBaseSeq != nil {
		carriedBase = *d.CarriedBaseSeq
		carriedBody = *d.CarriedBody
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReviewDraft{}, fmt.Errorf("artifacts: begin review draft write: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if expectRev == 0 {
		if s.dialect == dialectPostgres {
			if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1, hashtext($2))`,
				reviewDraftLockClass, PrincipalRef(d.PrincipalKind, d.PrincipalRef)); err != nil {
				return ReviewDraft{}, fmt.Errorf("artifacts: lock review drafts: %w", err)
			}
		}
		res, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO artifact_review_draft
			(artifact_id, principal_kind, principal_ref, rev, base_seq, entry_path, body, note,
			 carried_base_seq, carried_body, own_marks, created_at, updated_at, client_saved_at)
			VALUES (?, ?, ?, 1, ?, ?, ?, ?, ?, ?, NULL, ?, ?, ?)
			ON CONFLICT (artifact_id, principal_kind, principal_ref) DO NOTHING`),
			d.ArtifactID, d.PrincipalKind, d.PrincipalRef, d.BaseSeq, d.EntryPath, d.Body, d.Note,
			carriedBase, carriedBody, s.timeArg(at), s.timeArg(at), s.nullTimeArg(clientSaved))
		if err != nil {
			return ReviewDraft{}, fmt.Errorf("artifacts: insert review draft: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return ReviewDraft{}, fmt.Errorf("artifacts: insert review draft: %w", err)
		}
		if n == 0 {
			return ReviewDraft{}, s.readDraftConflict(ctx, tx, d.ArtifactID, d.PrincipalKind, d.PrincipalRef)
		}
		if hook := reviewDraftPutHook; hook != nil {
			hook(ctx)
		}
		var count int
		if err := tx.QueryRowContext(ctx, s.rebind(`SELECT COUNT(*) FROM artifact_review_draft
			WHERE principal_kind = ? AND principal_ref = ?`),
			d.PrincipalKind, d.PrincipalRef).Scan(&count); err != nil {
			return ReviewDraft{}, fmt.Errorf("artifacts: count review drafts: %w", err)
		}
		if count > maxPerPrincipal {
			return ReviewDraft{}, ErrTooManyDrafts
		}
		if err := tx.Commit(); err != nil {
			return ReviewDraft{}, fmt.Errorf("artifacts: commit review draft: %w", err)
		}
		out := d
		out.Rev = 1
		out.CreatedAt = at
		out.UpdatedAt = at
		out.ClientSavedAt = clientSaved
		return out, nil
	}

	nextRev := expectRev + 1
	res, err := tx.ExecContext(ctx, s.rebind(`UPDATE artifact_review_draft SET
		rev = ?,
		base_seq = ?,
		entry_path = ?,
		body = ?,
		note = ?,
		carried_base_seq = ?,
		carried_body = ?,
		updated_at = ?,
		client_saved_at = ?
		WHERE artifact_id = ? AND principal_kind = ? AND principal_ref = ? AND rev = ?`),
		nextRev, d.BaseSeq, d.EntryPath, d.Body, d.Note, carriedBase, carriedBody,
		s.timeArg(at), s.nullTimeArg(clientSaved),
		d.ArtifactID, d.PrincipalKind, d.PrincipalRef, expectRev)
	if err != nil {
		return ReviewDraft{}, fmt.Errorf("artifacts: update review draft: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return ReviewDraft{}, fmt.Errorf("artifacts: update review draft: %w", err)
	}
	if n == 0 {
		return ReviewDraft{}, s.readDraftConflict(ctx, tx, d.ArtifactID, d.PrincipalKind, d.PrincipalRef)
	}
	var created dbTime
	if err := tx.QueryRowContext(ctx, s.rebind(`SELECT created_at FROM artifact_review_draft
		WHERE artifact_id = ? AND principal_kind = ? AND principal_ref = ?`),
		d.ArtifactID, d.PrincipalKind, d.PrincipalRef).Scan(&created); err != nil {
		return ReviewDraft{}, fmt.Errorf("artifacts: read review draft created_at: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ReviewDraft{}, fmt.Errorf("artifacts: commit review draft: %w", err)
	}
	out := d
	out.Rev = nextRev
	out.CreatedAt = created.Time
	out.UpdatedAt = at
	out.ClientSavedAt = clientSaved
	return out, nil
}

func (s *sqlStore) readDraftConflict(ctx context.Context, tx *sql.Tx, artifactID, kind, ref string) error {
	var rev, baseSeq int
	var savedAt dbTime
	err := tx.QueryRowContext(ctx, s.rebind(`SELECT rev, updated_at, base_seq FROM artifact_review_draft
		WHERE artifact_id = ? AND principal_kind = ? AND principal_ref = ?`),
		artifactID, kind, ref).Scan(&rev, &savedAt, &baseSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return &DraftConflictError{}
	}
	if err != nil {
		return fmt.Errorf("artifacts: read conflicting review draft: %w", err)
	}
	return &DraftConflictError{Rev: rev, SavedAt: savedAt.Time, BaseSeq: baseSeq}
}

// GetReviewDraft implements Store.
func (s *sqlStore) GetReviewDraft(ctx context.Context, artifactID, kind, ref string) (ReviewDraft, error) {
	var (
		d                         ReviewDraft
		carriedBase               sql.NullInt64
		carriedBody               sql.NullString
		created, updated, clientT dbTime
	)
	err := s.db.QueryRowContext(ctx, s.rebind(`SELECT
		artifact_id, principal_kind, principal_ref, rev, base_seq, entry_path, body, note,
		carried_base_seq, carried_body, created_at, updated_at, client_saved_at
		FROM artifact_review_draft
		WHERE artifact_id = ? AND principal_kind = ? AND principal_ref = ?`),
		artifactID, kind, ref).Scan(
		&d.ArtifactID, &d.PrincipalKind, &d.PrincipalRef, &d.Rev, &d.BaseSeq, &d.EntryPath, &d.Body, &d.Note,
		&carriedBase, &carriedBody, &created, &updated, &clientT)
	if errors.Is(err, sql.ErrNoRows) {
		return ReviewDraft{}, ErrNotFound
	}
	if err != nil {
		return ReviewDraft{}, fmt.Errorf("artifacts: get review draft: %w", err)
	}
	if carriedBase.Valid {
		cb := int(carriedBase.Int64)
		d.CarriedBaseSeq = &cb
	}
	if carriedBody.Valid {
		cs := carriedBody.String
		d.CarriedBody = &cs
	}
	d.CreatedAt = created.Time
	d.UpdatedAt = updated.Time
	d.ClientSavedAt = clientT.ptr()
	return d, nil
}

// DeleteReviewDraft implements Store.
func (s *sqlStore) DeleteReviewDraft(ctx context.Context, artifactID, kind, ref string, expectRev int) (bool, error) {
	if expectRev < 0 {
		return false, errors.New("artifacts: DeleteReviewDraft expectRev must be non-negative")
	}
	if expectRev == 0 {
		res, err := s.db.ExecContext(ctx, s.rebind(`DELETE FROM artifact_review_draft
			WHERE artifact_id = ? AND principal_kind = ? AND principal_ref = ?`),
			artifactID, kind, ref)
		if err != nil {
			return false, fmt.Errorf("artifacts: delete review draft: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return false, fmt.Errorf("artifacts: delete review draft: %w", err)
		}
		return n > 0, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("artifacts: begin delete review draft: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM artifact_review_draft
		WHERE artifact_id = ? AND principal_kind = ? AND principal_ref = ? AND rev = ?`),
		artifactID, kind, ref, expectRev)
	if err != nil {
		return false, fmt.Errorf("artifacts: delete review draft: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("artifacts: delete review draft: %w", err)
	}
	if n > 0 {
		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf("artifacts: commit delete review draft: %w", err)
		}
		return true, nil
	}

	var rev, baseSeq int
	var savedAt dbTime
	err = tx.QueryRowContext(ctx, s.rebind(`SELECT rev, updated_at, base_seq FROM artifact_review_draft
		WHERE artifact_id = ? AND principal_kind = ? AND principal_ref = ?`),
		artifactID, kind, ref).Scan(&rev, &savedAt, &baseSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("artifacts: read conflicting review draft: %w", err)
	}
	if rev == expectRev {
		// A concurrent insert committed at expectRev after our first DELETE:
		// retry once under the same condition.
		res2, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM artifact_review_draft
			WHERE artifact_id = ? AND principal_kind = ? AND principal_ref = ? AND rev = ?`),
			artifactID, kind, ref, expectRev)
		if err != nil {
			return false, fmt.Errorf("artifacts: delete review draft: %w", err)
		}
		if n2, err := res2.RowsAffected(); err == nil && n2 > 0 {
			if err := tx.Commit(); err != nil {
				return false, fmt.Errorf("artifacts: commit delete review draft: %w", err)
			}
			return true, nil
		}
		return false, s.readDraftConflict(ctx, tx, artifactID, kind, ref)
	}
	return false, &DraftConflictError{Rev: rev, SavedAt: savedAt.Time, BaseSeq: baseSeq}
}

// ReapReviewDrafts implements Store.
func (s *sqlStore) ReapReviewDrafts(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	// The subquery bounds one pass; both dialects accept a row-value IN.
	res, err := s.db.ExecContext(ctx, s.rebind(`DELETE FROM artifact_review_draft
		WHERE (artifact_id, principal_kind, principal_ref) IN (
			SELECT artifact_id, principal_kind, principal_ref FROM artifact_review_draft
			WHERE updated_at < ? ORDER BY updated_at LIMIT `+strconv.Itoa(limit)+`)`),
		s.timeArg(cutoff))
	if err != nil {
		return 0, fmt.Errorf("artifacts: reap review drafts: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("artifacts: reap review drafts: %w", err)
	}
	return int(n), nil
}

// ListReviewDraftKeys implements Store.
func (s *sqlStore) ListReviewDraftKeys(ctx context.Context, after *ReviewDraftKey, limit int) ([]ReviewDraftKey, error) {
	if limit <= 0 {
		return nil, errors.New("artifacts: ListReviewDraftKeys needs a positive limit")
	}
	var (
		query string
		args  []any
	)
	if after == nil {
		query = `SELECT artifact_id, principal_kind, principal_ref
			FROM artifact_review_draft
			ORDER BY artifact_id, principal_kind, principal_ref
			LIMIT ` + strconv.Itoa(limit)
	} else {
		query = `SELECT artifact_id, principal_kind, principal_ref
			FROM artifact_review_draft
			WHERE (artifact_id, principal_kind, principal_ref) > (?, ?, ?)
			ORDER BY artifact_id, principal_kind, principal_ref
			LIMIT ` + strconv.Itoa(limit)
		args = []any{after.ArtifactID, after.PrincipalKind, after.PrincipalRef}
	}
	rows, err := s.db.QueryContext(ctx, s.rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("artifacts: list review draft keys: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ReviewDraftKey
	for rows.Next() {
		var k ReviewDraftKey
		if err := rows.Scan(&k.ArtifactID, &k.PrincipalKind, &k.PrincipalRef); err != nil {
			return nil, fmt.Errorf("artifacts: scan review draft key: %w", err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("artifacts: list review draft keys: %w", err)
	}
	return out, nil
}

// ListReviewDraftArtifactIDs implements Store.
func (s *sqlStore) ListReviewDraftArtifactIDs(ctx context.Context, kind, ref string) ([]string, error) {
	if kind == "" || ref == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, s.rebind(`SELECT artifact_id
		FROM artifact_review_draft
		WHERE principal_kind = ? AND principal_ref = ?
		ORDER BY artifact_id`), kind, ref)
	if err != nil {
		return nil, fmt.Errorf("artifacts: list review draft artifact ids: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("artifacts: scan review draft artifact id: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("artifacts: list review draft artifact ids: %w", err)
	}
	return out, nil
}
