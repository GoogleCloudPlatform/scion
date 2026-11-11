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
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// migrationActor is the actor of the grant_changed events a migration
// records.
var migrationActor = Actor{Kind: ActorKindSystem, Ref: AuthorityMigration, Authority: AuthorityMigration}

// applyReviewLadder is the 0014_review_ladder step (ptone/scion#5145). The
// removed review-only path (design D24) once gave review access as a user
// write grant on an artifact owned by an agent. Under the grant ladder,
// review is its own level and reads, so those grants become review grants.
//
// It selects user principal write grants on live artifacts owned by an
// agent that no grant_created or grant_changed event names, and changes
// each to review with one grant_changed event (authority migration). "No
// grant event" identifies the review-path rows exactly:
//   - grants were first written with no grant event, and only an owning
//     user or an admin grantee could write one, which an artifact owned by
//     an agent has neither of (its creation writes only the home scope
//     grant), so the review path was the only writer there;
//   - the review path wrote write only until the review level existed, and
//     review afterwards;
//   - the first other way to grant on such an artifact (stewards) came with
//     the grant_created and grant_changed events, which every grant write
//     records since (TestGrantWritesAllowlist);
//   - an artifact's owner is never changed after creation.
//
// Every other grant is left alone, including review grants, which start to
// read through the ladder itself. The step only lowers access (write to
// review) and logs counts only.
//
// When there are candidates and any grant_created or grant_changed event's
// detail is empty, unreadable or naming no grant, the step cannot tell which
// grant that event names. It then converts nothing, logs a warning with the
// counts and defers (errMigrationDeferred): no ledger row is recorded, the
// hub starts, and the step runs again at every start until the event is
// repaired. With no candidates there is nothing to convert, so the events are
// not read and the step is recorded.
func applyReviewLadder(ctx context.Context, tx *sql.Tx, s *sqlStore) error {
	r, err := s.convertReviewLadder(ctx, tx, time.Now())
	if err != nil {
		return err
	}
	if r.undecodable > 0 {
		slog.WarnContext(ctx, "artifacts: migration "+migrationReviewLadder+" deferred: grant events with an unreadable detail",
			"undecodable", r.undecodable, "candidates", r.candidates)
		return errMigrationDeferred
	}
	slog.InfoContext(ctx, "artifacts: migration "+migrationReviewLadder, "candidates", r.candidates,
		"converted", r.converted, "review_grants_total", r.reviewGrants)
	return nil
}

// reviewLadderResult is what convertReviewLadder did.
type reviewLadderResult struct {
	candidates   int // user write grants on live agent-owned artifacts
	converted    int // of those, changed to review
	undecodable  int // grant events whose detail could not be decoded; when > 0, nothing is converted
	reviewGrants int // user principal review grants afterwards
}

// reviewLadderCandidate is one grant applyReviewLadder may convert.
type reviewLadderCandidate struct {
	id, artifactID, subjectRef string
}

// convertReviewLadder does applyReviewLadder's work inside tx. With no
// candidates it reads no grant events, since events only ever exclude a
// candidate. When any grant event is undecodable it returns before any
// write, with nothing converted.
func (s *sqlStore) convertReviewLadder(ctx context.Context, tx *sql.Tx, now time.Time) (reviewLadderResult, error) {
	var r reviewLadderResult
	candidates, err := s.reviewLadderCandidates(ctx, tx)
	if err != nil {
		return r, err
	}
	r.candidates = len(candidates)
	if r.candidates == 0 {
		r.reviewGrants, err = s.countUserReviewGrants(ctx, tx)
		return r, err
	}
	named, undecodable, err := s.grantIDsWithGrantEvents(ctx, tx)
	if err != nil {
		return r, err
	}
	r.undecodable = undecodable
	if undecodable > 0 {
		return r, nil
	}
	for _, c := range candidates {
		if named[c.id] {
			continue
		}
		res, err := tx.ExecContext(ctx, s.rebind(`UPDATE artifact_grant SET permission = ? WHERE id = ? AND permission = ?`),
			GrantReview, c.id, GrantWrite)
		if err != nil {
			return r, fmt.Errorf("artifacts: convert grant: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return r, fmt.Errorf("artifacts: convert grant: %w", err)
		}
		if n != 1 {
			continue
		}
		if err := s.insertEvent(ctx, tx, c.artifactID, EventGrantChanged, &migrationActor, now, map[string]any{
			"grantId": c.id, "subjectKind": SubjectPrincipal, "subjectRef": c.subjectRef,
			"oldPermission": GrantWrite, "newPermission": GrantReview,
		}); err != nil {
			return r, err
		}
		r.converted++
	}
	r.reviewGrants, err = s.countUserReviewGrants(ctx, tx)
	return r, err
}

// countUserReviewGrants counts the user principal review grants, with the
// same prefix check as the candidates (SQLite's LIKE ignores case).
func (s *sqlStore) countUserReviewGrants(ctx context.Context, tx *sql.Tx) (int, error) {
	rows, err := tx.QueryContext(ctx, s.rebind(`SELECT subject_ref FROM artifact_grant WHERE subject_kind = ? AND permission = ?`),
		SubjectPrincipal, GrantReview)
	if err != nil {
		return 0, fmt.Errorf("artifacts: count review grants: %w", err)
	}
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return 0, fmt.Errorf("artifacts: count review grants: %w", err)
		}
		if strings.HasPrefix(ref, PrincipalKindUser+":") {
			n++
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("artifacts: count review grants: %w", err)
	}
	return n, nil
}

// reviewLadderCandidates lists the user principal write grants on live
// artifacts owned by an agent. The rows are read in full before any write.
func (s *sqlStore) reviewLadderCandidates(ctx context.Context, tx *sql.Tx) ([]reviewLadderCandidate, error) {
	rows, err := tx.QueryContext(ctx, s.rebind(`SELECT g.id, g.artifact_id, g.subject_ref
		FROM artifact_grant g JOIN artifact a ON a.id = g.artifact_id
		WHERE g.subject_kind = ? AND g.permission = ? AND g.subject_ref LIKE ?
		AND a.owner_kind = ? AND a.deleted_at IS NULL
		ORDER BY g.id`),
		SubjectPrincipal, GrantWrite, PrincipalKindUser+":%", PrincipalKindAgent)
	if err != nil {
		return nil, fmt.Errorf("artifacts: list review ladder candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []reviewLadderCandidate
	for rows.Next() {
		var c reviewLadderCandidate
		if err := rows.Scan(&c.id, &c.artifactID, &c.subjectRef); err != nil {
			return nil, fmt.Errorf("artifacts: read review ladder candidate: %w", err)
		}
		if strings.HasPrefix(c.subjectRef, PrincipalKindUser+":") {
			out = append(out, c)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("artifacts: list review ladder candidates: %w", err)
	}
	return out, nil
}

// grantIDsWithGrantEvents returns the ids of every grant that a
// grant_created or grant_changed event names, and the number of such
// events whose detail is NULL, empty, unreadable or naming no grant (JSON
// null, {} or no grantId; the caller then converts nothing, since it cannot
// tell which grant they name). The detail JSON is decoded here, so the SQL
// is the same in both dialects.
func (s *sqlStore) grantIDsWithGrantEvents(ctx context.Context, tx *sql.Tx) (map[string]bool, int, error) {
	rows, err := tx.QueryContext(ctx, s.rebind(`SELECT detail FROM artifact_event WHERE action IN (?, ?)`),
		EventGrantCreated, EventGrantChanged)
	if err != nil {
		return nil, 0, fmt.Errorf("artifacts: list grant events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	named := map[string]bool{}
	undecodable := 0
	for rows.Next() {
		var detail sql.NullString
		if err := rows.Scan(&detail); err != nil {
			return nil, 0, fmt.Errorf("artifacts: read grant event: %w", err)
		}
		if !detail.Valid || detail.String == "" {
			undecodable++
			continue
		}
		var d struct {
			GrantID string `json:"grantId"`
		}
		if err := json.Unmarshal([]byte(detail.String), &d); err != nil {
			undecodable++
			continue
		}
		if d.GrantID == "" {
			undecodable++
			continue
		}
		named[d.GrantID] = true
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("artifacts: list grant events: %w", err)
	}
	return named, undecodable, nil
}
