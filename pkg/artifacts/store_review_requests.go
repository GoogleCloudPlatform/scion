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
	"time"

	"github.com/google/uuid"
)

// Review request states, access modes, and per-reviewer create outcomes
// (ptone/scion#9296).
const (
	ReviewRequestStateOpen      = "open"
	ReviewRequestStateFulfilled = "fulfilled"
	ReviewRequestStateDismissed = "dismissed"
	ReviewRequestStateWithdrawn = "withdrawn"
	ReviewRequestStateExpired   = "expired"

	ReviewRequestAccessExisting = "existing"
	ReviewRequestAccessRaised   = "raised"
	ReviewRequestAccessBounded  = "bounded"

	ReviewRequestOutcomeCreated   = "created"
	ReviewRequestOutcomeReminded  = "reminded"
	ReviewRequestOutcomeUnchanged = "unchanged"
	ReviewRequestOutcomeRefused   = "refused"
)

// ErrRequestClosed is returned by DismissReviewRequest when a compare-and-set
// on state='open' loses to a concurrent transition.
var ErrRequestClosed = errors.New("artifacts: review request is no longer open")

// ErrNothingToReview is returned by CreateOrRemindReviewRequest when the
// artifact has no published version yet (current_seq == 0).
var ErrNothingToReview = errors.New("artifacts: artifact has no published version")

// ReviewRequest is one row of artifact_review_request.
type ReviewRequest struct {
	ID             string
	ArtifactID     string
	ReviewerKind   string
	ReviewerRef    string
	RequesterKind  string
	RequesterRef   string
	RequestedSeq   int
	Note           string
	Access         string
	GrantID        string
	State          string
	ResponseNote   string
	ResolvedSeq    *int
	ResolvedByKind string
	ResolvedByRef  string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	RemindedAt     *time.Time
	ResolvedAt     *time.Time
}

// migrationReviewRequests adds the artifact_review_request table and its three
// indexes (ptone/scion#9296, ptone/scion#9306). The migration is purely
// additive, so pkg/version.DowngradeFloor does not change.
const migrationReviewRequests = "0016_review_requests"

const sqliteReviewRequests = `
CREATE TABLE IF NOT EXISTS artifact_review_request (
    id               TEXT PRIMARY KEY,
    artifact_id      TEXT NOT NULL REFERENCES artifact (id),
    reviewer_kind    TEXT NOT NULL,
    reviewer_ref     TEXT NOT NULL,
    requester_kind   TEXT NOT NULL,
    requester_ref    TEXT NOT NULL,
    requested_seq    INTEGER NOT NULL,
    note             TEXT NOT NULL DEFAULT '',
    access           TEXT NOT NULL,
    grant_id         TEXT,
    state            TEXT NOT NULL,
    response_note    TEXT NOT NULL DEFAULT '',
    resolved_seq     INTEGER,
    resolved_by_kind TEXT,
    resolved_by_ref  TEXT,
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL,
    reminded_at      TEXT,
    resolved_at      TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_artifact_review_request_open
    ON artifact_review_request (artifact_id, reviewer_kind, reviewer_ref) WHERE state = 'open';
CREATE INDEX IF NOT EXISTS idx_artifact_review_request_reviewer
    ON artifact_review_request (reviewer_kind, reviewer_ref, state, updated_at);
CREATE INDEX IF NOT EXISTS idx_artifact_review_request_artifact
    ON artifact_review_request (artifact_id, state);
`

const postgresReviewRequests = `
CREATE TABLE IF NOT EXISTS artifact_review_request (
    id               TEXT PRIMARY KEY,
    artifact_id      TEXT NOT NULL REFERENCES artifact (id),
    reviewer_kind    TEXT NOT NULL,
    reviewer_ref     TEXT NOT NULL,
    requester_kind   TEXT NOT NULL,
    requester_ref    TEXT NOT NULL,
    requested_seq    INTEGER NOT NULL,
    note             TEXT NOT NULL DEFAULT '',
    access           TEXT NOT NULL,
    grant_id         TEXT,
    state            TEXT NOT NULL,
    response_note    TEXT NOT NULL DEFAULT '',
    resolved_seq     INTEGER,
    resolved_by_kind TEXT,
    resolved_by_ref  TEXT,
    created_at       TIMESTAMPTZ NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL,
    reminded_at      TIMESTAMPTZ,
    resolved_at      TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_artifact_review_request_open
    ON artifact_review_request (artifact_id, reviewer_kind, reviewer_ref) WHERE state = 'open';
CREATE INDEX IF NOT EXISTS idx_artifact_review_request_reviewer
    ON artifact_review_request (reviewer_kind, reviewer_ref, state, updated_at);
CREATE INDEX IF NOT EXISTS idx_artifact_review_request_artifact
    ON artifact_review_request (artifact_id, state);
`

const reviewRequestColumns = `id, artifact_id, reviewer_kind, reviewer_ref, requester_kind, requester_ref,
	requested_seq, note, access, grant_id, state, response_note, resolved_seq,
	resolved_by_kind, resolved_by_ref, created_at, updated_at, reminded_at, resolved_at`

func scanReviewRequest(r rowScanner) (ReviewRequest, error) {
	var (
		rr                           ReviewRequest
		grantID, resByKind, resByRef sql.NullString
		resSeq                       sql.NullInt64
		created, updated             dbTime
		reminded, resolved           dbTime
	)
	err := r.Scan(
		&rr.ID, &rr.ArtifactID, &rr.ReviewerKind, &rr.ReviewerRef, &rr.RequesterKind, &rr.RequesterRef,
		&rr.RequestedSeq, &rr.Note, &rr.Access, &grantID, &rr.State, &rr.ResponseNote, &resSeq,
		&resByKind, &resByRef, &created, &updated, &reminded, &resolved,
	)
	if err != nil {
		return ReviewRequest{}, err
	}
	rr.GrantID = grantID.String
	if resSeq.Valid {
		v := int(resSeq.Int64)
		rr.ResolvedSeq = &v
	}
	rr.ResolvedByKind = resByKind.String
	rr.ResolvedByRef = resByRef.String
	rr.CreatedAt = created.Time
	rr.UpdatedAt = updated.Time
	rr.RemindedAt = reminded.ptr()
	rr.ResolvedAt = resolved.ptr()
	return rr, nil
}

// reviewRequestDismissHook, when set by a test in this package, runs inside
// DismissReviewRequest after reading the open row and before the CAS UPDATE,
// so a test can race another transition and verify ErrRequestClosed.
var reviewRequestDismissHook func(ctx context.Context)

// CreateOrRemindReviewRequest implements Store.
func (s *sqlStore) CreateOrRemindReviewRequest(ctx context.Context, req ReviewRequest, remindCooldown time.Duration, now time.Time, actor *Actor) (ReviewRequest, string, error) {
	if req.ArtifactID == "" || req.ReviewerKind == "" || req.ReviewerRef == "" ||
		req.RequesterKind == "" || req.RequesterRef == "" || req.Access == "" {
		return ReviewRequest{}, "", errors.New("artifacts: CreateOrRemindReviewRequest missing required fields")
	}
	if !actor.valid() {
		return ReviewRequest{}, "", errNoActor
	}
	at := now.UTC().Truncate(time.Microsecond)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReviewRequest{}, "", fmt.Errorf("artifacts: begin review request: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := s.lockLiveArtifact(ctx, tx, req.ArtifactID); err != nil {
		return ReviewRequest{}, "", err
	}
	if err := s.refuseAttachmentTx(ctx, tx, req.ArtifactID); err != nil {
		return ReviewRequest{}, "", err
	}

	var currentSeq int
	if err := tx.QueryRowContext(ctx, s.rebind(`SELECT COALESCE(current_seq, 0) FROM artifact WHERE id = ?`), req.ArtifactID).Scan(&currentSeq); err != nil {
		return ReviewRequest{}, "", fmt.Errorf("artifacts: read artifact current_seq: %w", err)
	}
	if currentSeq == 0 {
		return ReviewRequest{}, "", ErrNothingToReview
	}

	remindExisting := func(existing ReviewRequest) (ReviewRequest, string, error) {
		if existing.RemindedAt != nil && at.Sub(*existing.RemindedAt) < remindCooldown {
			if err := tx.Commit(); err != nil {
				return ReviewRequest{}, "", fmt.Errorf("artifacts: commit review request unchanged: %w", err)
			}
			return existing, ReviewRequestOutcomeUnchanged, nil
		}
		res, err := tx.ExecContext(ctx, s.rebind(`UPDATE artifact_review_request
			SET reminded_at = ?, updated_at = ?
			WHERE id = ? AND state = ?`),
			s.timeArg(at), s.timeArg(at), existing.ID, ReviewRequestStateOpen)
		if err != nil {
			return ReviewRequest{}, "", fmt.Errorf("artifacts: remind review request: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return ReviewRequest{}, "", fmt.Errorf("artifacts: remind review request: %w", err)
		}
		if n == 0 {
			return ReviewRequest{}, "", ErrRequestClosed
		}
		detail := map[string]any{
			"requestId":    existing.ID,
			"reviewerKind": existing.ReviewerKind,
			"reviewerRef":  existing.ReviewerRef,
			"requestedSeq": existing.RequestedSeq,
		}
		if err := s.insertEvent(ctx, tx, req.ArtifactID, EventReviewRequestReminded, actor, at, detail); err != nil {
			return ReviewRequest{}, "", err
		}
		if err := tx.Commit(); err != nil {
			return ReviewRequest{}, "", fmt.Errorf("artifacts: commit review request remind: %w", err)
		}
		existing.RemindedAt = &at
		existing.UpdatedAt = at
		return existing, ReviewRequestOutcomeReminded, nil
	}

	existing, err := scanReviewRequest(tx.QueryRowContext(ctx, s.rebind(`SELECT `+reviewRequestColumns+`
		FROM artifact_review_request
		WHERE artifact_id = ? AND reviewer_kind = ? AND reviewer_ref = ? AND state = ?`),
		req.ArtifactID, req.ReviewerKind, req.ReviewerRef, ReviewRequestStateOpen))
	switch {
	case err == nil:
		return remindExisting(existing)
	case !errors.Is(err, sql.ErrNoRows):
		return ReviewRequest{}, "", fmt.Errorf("artifacts: query open review request: %w", err)
	}

	out := req
	if out.ID == "" {
		out.ID = uuid.NewString()
	}
	out.RequestedSeq = currentSeq
	out.State = ReviewRequestStateOpen
	out.ResponseNote = ""
	out.ResolvedSeq = nil
	out.ResolvedByKind = ""
	out.ResolvedByRef = ""
	out.CreatedAt = at
	out.UpdatedAt = at
	out.RemindedAt = nil
	out.ResolvedAt = nil

	res, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO artifact_review_request
		(id, artifact_id, reviewer_kind, reviewer_ref, requester_kind, requester_ref,
		 requested_seq, note, access, grant_id, state, response_note,
		 created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?)
		ON CONFLICT (artifact_id, reviewer_kind, reviewer_ref) WHERE state = 'open' DO NOTHING`),
		out.ID, out.ArtifactID, out.ReviewerKind, out.ReviewerRef, out.RequesterKind, out.RequesterRef,
		out.RequestedSeq, out.Note, out.Access, nullString(out.GrantID), out.State,
		s.timeArg(at), s.timeArg(at))
	if err != nil {
		return ReviewRequest{}, "", fmt.Errorf("artifacts: insert review request: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return ReviewRequest{}, "", fmt.Errorf("artifacts: insert review request: %w", err)
	}
	if n == 0 {
		// Lost a concurrent create race: re-read the open row and remind it.
		existing, err := scanReviewRequest(tx.QueryRowContext(ctx, s.rebind(`SELECT `+reviewRequestColumns+`
			FROM artifact_review_request
			WHERE artifact_id = ? AND reviewer_kind = ? AND reviewer_ref = ? AND state = ?`),
			req.ArtifactID, req.ReviewerKind, req.ReviewerRef, ReviewRequestStateOpen))
		if err != nil {
			return ReviewRequest{}, "", fmt.Errorf("artifacts: re-read open review request: %w", err)
		}
		return remindExisting(existing)
	}

	detail := map[string]any{
		"requestId":    out.ID,
		"reviewerKind": out.ReviewerKind,
		"reviewerRef":  out.ReviewerRef,
		"access":       out.Access,
		"requestedSeq": out.RequestedSeq,
	}
	if err := s.insertEvent(ctx, tx, req.ArtifactID, EventReviewRequestCreated, actor, at, detail); err != nil {
		return ReviewRequest{}, "", err
	}
	if err := tx.Commit(); err != nil {
		return ReviewRequest{}, "", fmt.Errorf("artifacts: commit review request: %w", err)
	}
	return out, ReviewRequestOutcomeCreated, nil
}

// DismissReviewRequest implements Store.
func (s *sqlStore) DismissReviewRequest(ctx context.Context, artifactID, reviewerKind, reviewerRef, responseNote string, now time.Time, actor *Actor) (ReviewRequest, error) {
	if artifactID == "" || reviewerKind == "" || reviewerRef == "" {
		return ReviewRequest{}, errors.New("artifacts: DismissReviewRequest missing required fields")
	}
	if !actor.valid() {
		return ReviewRequest{}, errNoActor
	}
	at := now.UTC().Truncate(time.Microsecond)

	existing, err := s.GetOpenReviewRequest(ctx, artifactID, reviewerKind, reviewerRef)
	if err != nil {
		return ReviewRequest{}, err
	}
	if hook := reviewRequestDismissHook; hook != nil {
		hook(ctx)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReviewRequest{}, fmt.Errorf("artifacts: begin dismiss review request: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, s.rebind(`UPDATE artifact_review_request
		SET state = ?, response_note = ?, resolved_by_kind = ?, resolved_by_ref = ?,
		    resolved_at = ?, updated_at = ?
		WHERE id = ? AND state = ?`),
		ReviewRequestStateDismissed, responseNote, actor.Kind, actor.Ref,
		s.timeArg(at), s.timeArg(at), existing.ID, ReviewRequestStateOpen)
	if err != nil {
		return ReviewRequest{}, fmt.Errorf("artifacts: dismiss review request: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return ReviewRequest{}, fmt.Errorf("artifacts: dismiss review request: %w", err)
	}
	if n == 0 {
		return ReviewRequest{}, ErrRequestClosed
	}

	detail := map[string]any{
		"requestId":    existing.ID,
		"reviewerKind": existing.ReviewerKind,
		"reviewerRef":  existing.ReviewerRef,
	}
	if err := s.insertEvent(ctx, tx, artifactID, EventReviewRequestDismissed, actor, at, detail); err != nil {
		return ReviewRequest{}, err
	}
	if err := tx.Commit(); err != nil {
		return ReviewRequest{}, fmt.Errorf("artifacts: commit dismiss review request: %w", err)
	}

	out := *existing
	out.State = ReviewRequestStateDismissed
	out.ResponseNote = responseNote
	out.ResolvedByKind = actor.Kind
	out.ResolvedByRef = actor.Ref
	out.ResolvedAt = &at
	out.UpdatedAt = at
	return out, nil
}

// GetOpenReviewRequest implements Store.
func (s *sqlStore) GetOpenReviewRequest(ctx context.Context, artifactID, reviewerKind, reviewerRef string) (*ReviewRequest, error) {
	rr, err := scanReviewRequest(s.db.QueryRowContext(ctx, s.rebind(`SELECT `+reviewRequestColumns+`
		FROM artifact_review_request
		WHERE artifact_id = ? AND reviewer_kind = ? AND reviewer_ref = ? AND state = ?`),
		artifactID, reviewerKind, reviewerRef, ReviewRequestStateOpen))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("artifacts: get open review request: %w", err)
	}
	return &rr, nil
}
