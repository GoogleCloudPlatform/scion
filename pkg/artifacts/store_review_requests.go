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

// ReviewRequestKey identifies one review request row for keyset pagination in
// the maintenance reaper.
type ReviewRequestKey struct {
	ArtifactID string
	ID         string
}

// ReviewRequestGrantOption asks CreateOrRemindReviewRequest to write or raise
// a principal review grant inside the same lockLiveArtifact transaction.
type ReviewRequestGrantOption struct {
	Grant     *Grant
	MaxGrants int
	Actor     *Actor
}

// CreateOrRemindReviewRequest implements Store.
func (s *sqlStore) CreateOrRemindReviewRequest(ctx context.Context, req ReviewRequest, remindCooldown time.Duration, now time.Time, actor *Actor, grantOpt *ReviewRequestGrantOption) (ReviewRequest, string, error) {
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

	// If the caller is raising the reviewer's access to review (design §4.3
	// step 5b), apply the grant in the same lockLiveArtifact transaction
	// through putGrantTx and under maxGrants.
	var raisedGrantID string
	if grantOpt != nil && grantOpt.Grant != nil {
		g := grantOpt.Grant
		g.ArtifactID = req.ArtifactID
		if g.ID == "" {
			g.ID = uuid.NewString()
		}
		if g.CreatedAt.IsZero() {
			g.CreatedAt = at
		}
		if _, _, err := s.putGrantTx(ctx, tx, g, grantOpt.MaxGrants, false, grantOpt.Actor, false, at); err != nil {
			return ReviewRequest{}, "", err
		}
		raisedGrantID = g.ID
	}

	remindExisting := func(existing ReviewRequest) (ReviewRequest, string, error) {
		nextAccess := existing.Access
		nextGrantID := existing.GrantID
		if raisedGrantID != "" {
			nextAccess = ReviewRequestAccessRaised
			nextGrantID = raisedGrantID
		}
		if existing.RemindedAt != nil && at.Sub(*existing.RemindedAt) < remindCooldown {
			if raisedGrantID != "" {
				res, err := tx.ExecContext(ctx, s.rebind(`UPDATE artifact_review_request
					SET access = ?, grant_id = ?, updated_at = ?
					WHERE id = ? AND state = ?`),
					nextAccess, nullString(nextGrantID), s.timeArg(at), existing.ID, ReviewRequestStateOpen)
				if err != nil {
					return ReviewRequest{}, "", fmt.Errorf("artifacts: update raised grant on review request: %w", err)
				}
				n, err := res.RowsAffected()
				if err != nil {
					return ReviewRequest{}, "", fmt.Errorf("artifacts: update raised grant on review request: %w", err)
				}
				if n == 0 {
					return ReviewRequest{}, "", ErrRequestClosed
				}
				existing.Access = nextAccess
				existing.GrantID = nextGrantID
				existing.UpdatedAt = at
			}
			if err := tx.Commit(); err != nil {
				return ReviewRequest{}, "", fmt.Errorf("artifacts: commit review request unchanged: %w", err)
			}
			return existing, ReviewRequestOutcomeUnchanged, nil
		}
		nextNote := existing.Note
		if req.Note != "" && req.RequesterKind == existing.RequesterKind && req.RequesterRef == existing.RequesterRef {
			nextNote = req.Note
		}
		res, err := tx.ExecContext(ctx, s.rebind(`UPDATE artifact_review_request
			SET note = ?, access = ?, grant_id = ?, reminded_at = ?, updated_at = ?
			WHERE id = ? AND state = ?`),
			nextNote, nextAccess, nullString(nextGrantID), s.timeArg(at), s.timeArg(at), existing.ID, ReviewRequestStateOpen)
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
		existing.Note = nextNote
		existing.Access = nextAccess
		existing.GrantID = nextGrantID
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
	if raisedGrantID != "" {
		out.GrantID = raisedGrantID
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

// GetReviewRequest implements Store.
func (s *sqlStore) GetReviewRequest(ctx context.Context, artifactID, requestID string) (*ReviewRequest, error) {
	rr, err := scanReviewRequest(s.db.QueryRowContext(ctx, s.rebind(`SELECT `+reviewRequestColumns+`
		FROM artifact_review_request
		WHERE artifact_id = ? AND id = ?`),
		artifactID, requestID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("artifacts: get review request: %w", err)
	}
	return &rr, nil
}

// ListReviewRequests implements Store: returns all open requests for
// artifactID (newest first) followed by up to closedLimit most recent closed
// requests.
func (s *sqlStore) ListReviewRequests(ctx context.Context, artifactID string, closedLimit int) ([]ReviewRequest, error) {
	return s.listReviewRequests(ctx, artifactID, "", "", closedLimit)
}

// ListReviewRequestsForReviewer implements Store: returns all open requests
// for (artifactID, reviewerKind, reviewerRef) followed by up to closedLimit
// most recent closed requests.
func (s *sqlStore) ListReviewRequestsForReviewer(ctx context.Context, artifactID, reviewerKind, reviewerRef string, closedLimit int) ([]ReviewRequest, error) {
	return s.listReviewRequests(ctx, artifactID, reviewerKind, reviewerRef, closedLimit)
}

func (s *sqlStore) listReviewRequests(ctx context.Context, artifactID, reviewerKind, reviewerRef string, closedLimit int) ([]ReviewRequest, error) {
	openQ := `SELECT ` + reviewRequestColumns + ` FROM artifact_review_request WHERE artifact_id = ? AND state = ?`
	openArgs := []any{artifactID, ReviewRequestStateOpen}
	if reviewerKind != "" {
		openQ += ` AND reviewer_kind = ? AND reviewer_ref = ?`
		openArgs = append(openArgs, reviewerKind, reviewerRef)
	}
	openQ += ` ORDER BY created_at DESC, id DESC`

	rows, err := s.db.QueryContext(ctx, s.rebind(openQ), openArgs...)
	if err != nil {
		return nil, fmt.Errorf("artifacts: list open review requests: %w", err)
	}
	var out []ReviewRequest
	for rows.Next() {
		rr, err := scanReviewRequest(rows)
		if err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("artifacts: scan open review request: %w", err)
		}
		out = append(out, rr)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("artifacts: list open review requests: %w", err)
	}
	_ = rows.Close()

	if closedLimit <= 0 {
		return out, nil
	}

	closedQ := `SELECT ` + reviewRequestColumns + ` FROM artifact_review_request WHERE artifact_id = ? AND state != ?`
	closedArgs := []any{artifactID, ReviewRequestStateOpen}
	if reviewerKind != "" {
		closedQ += ` AND reviewer_kind = ? AND reviewer_ref = ?`
		closedArgs = append(closedArgs, reviewerKind, reviewerRef)
	}
	closedQ += ` ORDER BY resolved_at DESC, id DESC LIMIT ` + strconv.Itoa(closedLimit)

	crows, err := s.db.QueryContext(ctx, s.rebind(closedQ), closedArgs...)
	if err != nil {
		return nil, fmt.Errorf("artifacts: list closed review requests: %w", err)
	}
	defer func() { _ = crows.Close() }()
	for crows.Next() {
		rr, err := scanReviewRequest(crows)
		if err != nil {
			return nil, fmt.Errorf("artifacts: scan closed review request: %w", err)
		}
		out = append(out, rr)
	}
	if err := crows.Err(); err != nil {
		return nil, fmt.Errorf("artifacts: list closed review requests: %w", err)
	}
	return out, nil
}

// WithdrawReviewRequest implements Store: transitions an open review request
// to withdrawn under lockLiveArtifact + CAS on state='open' (design §4.6
// writer 3). A raised grant is left in place; bounded grant cleanup is hooked
// for Phase 4.
func (s *sqlStore) WithdrawReviewRequest(ctx context.Context, artifactID, requestID string, now time.Time, actor *Actor) (ReviewRequest, error) {
	if artifactID == "" || requestID == "" {
		return ReviewRequest{}, errors.New("artifacts: WithdrawReviewRequest missing required fields")
	}
	if !actor.valid() {
		return ReviewRequest{}, errNoActor
	}
	at := now.UTC().Truncate(time.Microsecond)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReviewRequest{}, fmt.Errorf("artifacts: begin withdraw review request: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := s.lockLiveArtifact(ctx, tx, artifactID); err != nil {
		return ReviewRequest{}, err
	}
	if err := s.checkExpect(ctx, tx, artifactID, actor); err != nil {
		return ReviewRequest{}, err
	}

	existing, err := scanReviewRequest(tx.QueryRowContext(ctx, s.rebind(`SELECT `+reviewRequestColumns+`
		FROM artifact_review_request
		WHERE artifact_id = ? AND id = ?`), artifactID, requestID))
	if errors.Is(err, sql.ErrNoRows) {
		return ReviewRequest{}, ErrNotFound
	}
	if err != nil {
		return ReviewRequest{}, fmt.Errorf("artifacts: query review request for withdraw: %w", err)
	}
	if existing.State != ReviewRequestStateOpen {
		return ReviewRequest{}, ErrRequestClosed
	}

	res, err := tx.ExecContext(ctx, s.rebind(`UPDATE artifact_review_request
		SET state = ?, resolved_by_kind = ?, resolved_by_ref = ?,
		    resolved_at = ?, updated_at = ?
		WHERE id = ? AND artifact_id = ? AND state = ?`),
		ReviewRequestStateWithdrawn, actor.Kind, actor.Ref,
		s.timeArg(at), s.timeArg(at), existing.ID, artifactID, ReviewRequestStateOpen)
	if err != nil {
		return ReviewRequest{}, fmt.Errorf("artifacts: withdraw review request: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return ReviewRequest{}, fmt.Errorf("artifacts: withdraw review request: %w", err)
	}
	if n == 0 {
		return ReviewRequest{}, ErrRequestClosed
	}

	// Phase 4 hook: bounded grants (ReviewRequestAccessBounded) do not exist
	// until Phase 4; when they do, delete existing.GrantID here if its
	// created_by_ref is still BoundedReviewCreator. Raised grants are
	// intentionally left in place (design §4.4).
	if err := s.cleanupWithdrawnRequestGrantTx(ctx, tx, existing, actor, at); err != nil {
		return ReviewRequest{}, err
	}

	detail := map[string]any{
		"requestId":    existing.ID,
		"reviewerKind": existing.ReviewerKind,
		"reviewerRef":  existing.ReviewerRef,
	}
	if err := s.insertEvent(ctx, tx, artifactID, EventReviewRequestWithdrawn, actor, at, detail); err != nil {
		return ReviewRequest{}, err
	}
	if err := tx.Commit(); err != nil {
		return ReviewRequest{}, fmt.Errorf("artifacts: commit withdraw review request: %w", err)
	}

	out := existing
	out.State = ReviewRequestStateWithdrawn
	out.ResolvedByKind = actor.Kind
	out.ResolvedByRef = actor.Ref
	out.ResolvedAt = &at
	out.UpdatedAt = at
	return out, nil
}

// cleanupWithdrawnRequestGrantTx is the Phase 4 hook for deleting a
// still-role-gated bounded grant when its review request is withdrawn.
// Raised grants are intentionally left in place.
func (s *sqlStore) cleanupWithdrawnRequestGrantTx(_ context.Context, _ *sql.Tx, _ ReviewRequest, _ *Actor, _ time.Time) error {
	return nil
}

// ListOpenReviewRequests implements Store: walks open review requests in
// (artifact_id, id) order for the maintenance reaper.
func (s *sqlStore) ListOpenReviewRequests(ctx context.Context, after *ReviewRequestKey, limit int) ([]ReviewRequest, error) {
	if limit <= 0 {
		return nil, errors.New("artifacts: ListOpenReviewRequests needs a positive limit")
	}
	var (
		query string
		args  []any
	)
	if after == nil {
		query = `SELECT ` + reviewRequestColumns + `
			FROM artifact_review_request
			WHERE state = ?
			ORDER BY artifact_id, id
			LIMIT ` + strconv.Itoa(limit)
		args = []any{ReviewRequestStateOpen}
	} else {
		query = `SELECT ` + reviewRequestColumns + `
			FROM artifact_review_request
			WHERE state = ? AND (artifact_id, id) > (?, ?)
			ORDER BY artifact_id, id
			LIMIT ` + strconv.Itoa(limit)
		args = []any{ReviewRequestStateOpen, after.ArtifactID, after.ID}
	}
	rows, err := s.db.QueryContext(ctx, s.rebind(query), args...)
	if err != nil {
		return nil, fmt.Errorf("artifacts: list open review requests: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ReviewRequest
	for rows.Next() {
		rr, err := scanReviewRequest(rows)
		if err != nil {
			return nil, fmt.Errorf("artifacts: scan open review request: %w", err)
		}
		out = append(out, rr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("artifacts: list open review requests: %w", err)
	}
	return out, nil
}

// ExpireReviewRequest implements Store: CAS-transitions an open review request
// to expired only if its state is still 'open' and updated_at matches
// expectUpdatedAt (design §4.6 writer 7).
func (s *sqlStore) ExpireReviewRequest(ctx context.Context, artifactID, requestID string, expectUpdatedAt time.Time, reason string, now time.Time, actor *Actor) (bool, error) {
	if artifactID == "" || requestID == "" {
		return false, errors.New("artifacts: ExpireReviewRequest missing required fields")
	}
	if !actor.valid() {
		return false, errNoActor
	}
	at := now.UTC().Truncate(time.Microsecond)
	expAt := expectUpdatedAt.UTC().Truncate(time.Microsecond)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("artifacts: begin expire review request: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var reviewerKind, reviewerRef string
	err = tx.QueryRowContext(ctx, s.rebind(`SELECT reviewer_kind, reviewer_ref
		FROM artifact_review_request
		WHERE id = ? AND artifact_id = ? AND state = ? AND updated_at = ?`),
		requestID, artifactID, ReviewRequestStateOpen, s.timeArg(expAt)).Scan(&reviewerKind, &reviewerRef)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("artifacts: query review request for expire: %w", err)
	}

	res, err := tx.ExecContext(ctx, s.rebind(`UPDATE artifact_review_request
		SET state = ?, resolved_by_kind = ?, resolved_by_ref = ?,
		    resolved_at = ?, updated_at = ?
		WHERE id = ? AND artifact_id = ? AND state = ? AND updated_at = ?`),
		ReviewRequestStateExpired, actor.Kind, actor.Ref,
		s.timeArg(at), s.timeArg(at), requestID, artifactID, ReviewRequestStateOpen, s.timeArg(expAt))
	if err != nil {
		return false, fmt.Errorf("artifacts: expire review request: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("artifacts: expire review request: %w", err)
	}
	if n == 0 {
		return false, nil
	}

	detail := map[string]any{
		"requestId":    requestID,
		"reviewerKind": reviewerKind,
		"reviewerRef":  reviewerRef,
	}
	if reason != "" {
		detail["reason"] = reason
	}
	if err := s.insertEvent(ctx, tx, artifactID, EventReviewRequestExpired, actor, at, detail); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("artifacts: commit expire review request: %w", err)
	}
	return true, nil
}

// PurgeClosedReviewRequests implements Store: deletes up to limit closed
// review request rows whose resolved_at is at or before resolvedBefore.
func (s *sqlStore) PurgeClosedReviewRequests(ctx context.Context, resolvedBefore time.Time, limit int) (int, error) {
	if limit <= 0 {
		return 0, errors.New("artifacts: PurgeClosedReviewRequests needs a positive limit")
	}
	cutoff := resolvedBefore.UTC().Truncate(time.Microsecond)
	res, err := s.db.ExecContext(ctx, s.rebind(`DELETE FROM artifact_review_request
		WHERE id IN (
			SELECT id FROM artifact_review_request
			WHERE state != ? AND resolved_at IS NOT NULL AND resolved_at <= ?
			ORDER BY resolved_at ASC LIMIT `+strconv.Itoa(limit)+`)`),
		ReviewRequestStateOpen, s.timeArg(cutoff))
	if err != nil {
		return 0, fmt.Errorf("artifacts: purge closed review requests: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("artifacts: purge closed review requests: %w", err)
	}
	return int(n), nil
}
