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
	"fmt"
	"strconv"
	"time"
)

// ReviewSession is the row of one review tab, as its heartbeat last reported it
// (ptone/scion#6849). The principal is part of the key, so a session id one
// principal chooses can never address another principal's row.
type ReviewSession struct {
	ArtifactID    string
	PrincipalKind string
	PrincipalRef  string
	SessionID     string
	// BaseSeq is the version under review.
	BaseSeq int
	// StartedAt is when the session's first beat arrived, and LastSeenAt
	// when its latest did (server clock).
	StartedAt  time.Time
	LastSeenAt time.Time
	// LastEditAt is when a beat last reported an edit, or nil.
	LastEditAt *time.Time
	// Dirty is true when the review text differs from the text the
	// review started from.
	Dirty bool
}

// ErrTooManyReviewSessions is returned by TouchReviewSession when the beat
// would create a new session row while the principal already has the
// maximum number of live sessions on the artifact.
var ErrTooManyReviewSessions = errors.New("artifacts: too many live review sessions")

// migrationReviewSessions adds the table of review tab sessions
// (ptone/scion#6849): one row per tab, keyed by artifact, principal and a
// client-chosen session id, touched by each heartbeat and removed when the
// review closes, with the artifact, or by the reaper, at least an hour after
// the last beat. The indexes serve the open-review check (one artifact's
// recent rows) and the reaper (old rows); the primary key serves the
// per-principal count.
const migrationReviewSessions = "0013_review_sessions"

const sqliteReviewSessions = `
CREATE TABLE IF NOT EXISTS artifact_review_session (
    artifact_id    TEXT NOT NULL REFERENCES artifact (id),
    principal_kind TEXT NOT NULL,
    principal_ref  TEXT NOT NULL,
    session_id     TEXT NOT NULL,
    base_seq       INTEGER NOT NULL,
    started_at     TEXT NOT NULL,
    last_seen_at   TEXT NOT NULL,
    last_edit_at   TEXT,
    dirty          INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (artifact_id, principal_kind, principal_ref, session_id)
);
CREATE INDEX IF NOT EXISTS idx_artifact_review_session_seen
    ON artifact_review_session (artifact_id, last_seen_at);
CREATE INDEX IF NOT EXISTS idx_artifact_review_session_reap
    ON artifact_review_session (last_seen_at);
`

const postgresReviewSessions = `
CREATE TABLE IF NOT EXISTS artifact_review_session (
    artifact_id    TEXT NOT NULL REFERENCES artifact (id),
    principal_kind TEXT NOT NULL,
    principal_ref  TEXT NOT NULL,
    session_id     TEXT NOT NULL,
    base_seq       INTEGER NOT NULL,
    started_at     TIMESTAMPTZ NOT NULL,
    last_seen_at   TIMESTAMPTZ NOT NULL,
    last_edit_at   TIMESTAMPTZ,
    dirty          BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (artifact_id, principal_kind, principal_ref, session_id)
);
CREATE INDEX IF NOT EXISTS idx_artifact_review_session_seen
    ON artifact_review_session (artifact_id, last_seen_at);
CREATE INDEX IF NOT EXISTS idx_artifact_review_session_reap
    ON artifact_review_session (last_seen_at);
`

// reviewSessionLockClass is the first key of the Postgres advisory lock
// that serializes TouchReviewSession per (artifact, principal); the second
// key is a hash of the two. Two-key advisory locks never collide with the
// one-key Init lock (initLockKey). A hash collision between two pairs only
// makes their beats wait for each other: the count is still per pair.
const reviewSessionLockClass int32 = 0x61727273 // "arrs"

// reviewSessionTouchHook, when a test in this package sets it, runs inside
// TouchReviewSession's transaction after the upsert and before the count,
// so a test can hold one beat there while another runs. It is nil in
// production: nothing outside this package's tests sets it.
var reviewSessionTouchHook func(ctx context.Context)

// TouchReviewSession implements Store.
//
// The per-principal cap is serialized on both dialects (the check-then-act
// would otherwise let concurrent beats overshoot it): on Postgres the
// transaction first takes an advisory lock for the (artifact, principal)
// pair; on SQLite the transaction writes first (the upsert), which takes
// the database's write lock before anything is counted. A beat that would
// open a session past the cap is rolled back. A beat of a session that
// already exists is never refused, so a row that beats again after leaving
// the live window is not counted again.
func (s *sqlStore) TouchReviewSession(ctx context.Context, rs ReviewSession, edited bool, now time.Time, maxPerPrincipal int) error {
	if rs.ArtifactID == "" || rs.PrincipalKind == "" || rs.PrincipalRef == "" || rs.SessionID == "" {
		return errors.New("artifacts: TouchReviewSession needs an artifact, a principal and a session id")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("artifacts: begin review session: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if s.dialect == dialectPostgres {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1, hashtext($2))`,
			reviewSessionLockClass, rs.ArtifactID+"\x1f"+PrincipalRef(rs.PrincipalKind, rs.PrincipalRef)); err != nil {
			return fmt.Errorf("artifacts: lock review sessions: %w", err)
		}
	}
	at := s.timeArg(now)
	var editAt any
	if edited {
		editAt = at
	}
	// started_at is set on insert only; last_edit_at moves only on an edit.
	if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO artifact_review_session
		(artifact_id, principal_kind, principal_ref, session_id, base_seq, started_at, last_seen_at, last_edit_at, dirty)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (artifact_id, principal_kind, principal_ref, session_id) DO UPDATE SET
			base_seq = excluded.base_seq,
			last_seen_at = excluded.last_seen_at,
			last_edit_at = COALESCE(excluded.last_edit_at, artifact_review_session.last_edit_at),
			dirty = excluded.dirty`),
		rs.ArtifactID, rs.PrincipalKind, rs.PrincipalRef, rs.SessionID, rs.BaseSeq, at, at, editAt, rs.Dirty); err != nil {
		return fmt.Errorf("artifacts: touch review session: %w", err)
	}
	if hook := reviewSessionTouchHook; hook != nil {
		hook(ctx)
	}
	// A row whose started_at is this beat's time was inserted by it.
	var started dbTime
	if err := tx.QueryRowContext(ctx, s.rebind(`SELECT started_at FROM artifact_review_session
		WHERE artifact_id = ? AND principal_kind = ? AND principal_ref = ? AND session_id = ?`),
		rs.ArtifactID, rs.PrincipalKind, rs.PrincipalRef, rs.SessionID).Scan(&started); err != nil {
		return fmt.Errorf("artifacts: read review session: %w", err)
	}
	if started.Time.Equal(now.UTC().Truncate(time.Microsecond)) {
		var live int
		if err := tx.QueryRowContext(ctx, s.rebind(`SELECT COUNT(*) FROM artifact_review_session
			WHERE artifact_id = ? AND principal_kind = ? AND principal_ref = ? AND last_seen_at >= ?`),
			rs.ArtifactID, rs.PrincipalKind, rs.PrincipalRef, s.timeArg(now.Add(-ReviewLiveWindow))).Scan(&live); err != nil {
			return fmt.Errorf("artifacts: count review sessions: %w", err)
		}
		if live > maxPerPrincipal {
			return ErrTooManyReviewSessions
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("artifacts: commit review session: %w", err)
	}
	return nil
}

// EndReviewSession implements Store.
func (s *sqlStore) EndReviewSession(ctx context.Context, artifactID, kind, ref, sessionID string) (bool, error) {
	res, err := s.db.ExecContext(ctx, s.rebind(`DELETE FROM artifact_review_session
		WHERE artifact_id = ? AND principal_kind = ? AND principal_ref = ? AND session_id = ?`),
		artifactID, kind, ref, sessionID)
	if err != nil {
		return false, fmt.Errorf("artifacts: end review session: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("artifacts: end review session: %w", err)
	}
	return n > 0, nil
}

// ListOpenReviewSessions implements Store.
func (s *sqlStore) ListOpenReviewSessions(ctx context.Context, artifactID string, seenAfter time.Time) ([]ReviewSession, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(`SELECT artifact_id, principal_kind, principal_ref, session_id,
		base_seq, started_at, last_seen_at, last_edit_at, dirty
		FROM artifact_review_session WHERE artifact_id = ? AND last_seen_at >= ?
		ORDER BY last_seen_at DESC LIMIT `+strconv.Itoa(maxListedReviewSessions)),
		artifactID, s.timeArg(seenAfter))
	if err != nil {
		return nil, fmt.Errorf("artifacts: list review sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ReviewSession
	for rows.Next() {
		var rs ReviewSession
		var started, seen, edit dbTime
		if err := rows.Scan(&rs.ArtifactID, &rs.PrincipalKind, &rs.PrincipalRef, &rs.SessionID,
			&rs.BaseSeq, &started, &seen, &edit, &rs.Dirty); err != nil {
			return nil, fmt.Errorf("artifacts: scan review session: %w", err)
		}
		rs.StartedAt, rs.LastSeenAt, rs.LastEditAt = started.Time, seen.Time, edit.ptr()
		out = append(out, rs)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("artifacts: list review sessions: %w", err)
	}
	return out, nil
}

// ReapReviewSessions implements Store.
func (s *sqlStore) ReapReviewSessions(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	// The subquery bounds one pass; both dialects accept a row-value IN.
	res, err := s.db.ExecContext(ctx, s.rebind(`DELETE FROM artifact_review_session
		WHERE (artifact_id, principal_kind, principal_ref, session_id) IN (
			SELECT artifact_id, principal_kind, principal_ref, session_id FROM artifact_review_session
			WHERE last_seen_at < ? ORDER BY last_seen_at LIMIT `+strconv.Itoa(limit)+`)`),
		s.timeArg(cutoff))
	if err != nil {
		return 0, fmt.Errorf("artifacts: reap review sessions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("artifacts: reap review sessions: %w", err)
	}
	return int(n), nil
}
