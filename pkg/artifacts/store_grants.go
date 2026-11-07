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
	"strings"
	"time"
)

// lockLiveArtifact takes the artifact row's lock (Postgres) or the write
// lock (SQLite) inside tx with a no-op write, and reports ErrNotFound when
// the artifact is absent or deleted.
func (s *sqlStore) lockLiveArtifact(ctx context.Context, tx *sql.Tx, artifactID string) error {
	res, err := tx.ExecContext(ctx, s.rebind(`UPDATE artifact SET id = id WHERE id = ? AND deleted_at IS NULL`), artifactID)
	if err != nil {
		return fmt.Errorf("artifacts: lock artifact: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("artifacts: lock artifact: %w", err)
	}
	if n != 1 {
		return ErrNotFound
	}
	return nil
}

// PutGrant implements Store.
func (s *sqlStore) PutGrant(ctx context.Context, g *Grant, maxGrants int, crossScope bool, actor *Actor) (GrantPut, error) {
	res, err := s.PutGrantNotify(ctx, g, maxGrants, crossScope, actor, false)
	return res.Put, err
}

// PutGrantNotify implements Store.
func (s *sqlStore) PutGrantNotify(ctx context.Context, g *Grant, maxGrants int, crossScope bool, actor *Actor, notify bool) (GrantPutResult, error) {
	put, notified, err := s.putGrant(ctx, g, maxGrants, crossScope, "", actor, notify)
	return GrantPutResult{Put: put, Notified: notified}, err
}

// PutReviewGrant implements Store.
func (s *sqlStore) PutReviewGrant(ctx context.Context, g *Grant, maxGrants int, home string, actor *Actor) (GrantPut, error) {
	if g == nil || g.SubjectKind != SubjectPrincipal || !strings.HasPrefix(g.SubjectRef, PrincipalKindUser+":") ||
		g.Permission != GrantReview || home == "" {
		return GrantPutUnchanged, errors.New("artifacts: PutReviewGrant needs a user principal grant with review permission and a home scope")
	}
	put, _, err := s.putGrant(ctx, g, maxGrants, false, home, actor, false)
	return put, err
}

// validGrantPermission reports whether p is a permission a principal or
// scope grant may carry.
func validGrantPermission(p string) bool {
	switch p {
	case GrantRead, GrantReview, GrantWrite, GrantAdmin:
		return true
	}
	return false
}

// grantRank orders the permissions a principal grant may carry: read 1 <
// review 2 < write 3 < admin 4, and 0 for anything else. A grant raises
// its subject's level when its rank is higher than the one it replaces.
func grantRank(p string) int {
	switch p {
	case GrantRead:
		return 1
	case GrantReview:
		return 2
	case GrantWrite:
		return 3
	case GrantAdmin:
		return 4
	}
	return 0
}

// reviewSubjectOK reports whether a grant of permission p may name the
// subject (kind, ref): a review grant names a user principal only.
func reviewSubjectOK(p, kind, ref string) bool {
	return p != GrantReview || (kind == SubjectPrincipal && strings.HasPrefix(ref, PrincipalKindUser+":"))
}

// putGrant is PutGrant and, with reviewHome set, PutReviewGrant: then the
// artifact must still be owned by an agent and homed in reviewHome, and an
// existing grant for the subject other than review is never replaced. A
// review grant never replaces a read, write or admin grant on either path:
// the subject's one principal row would lose the read the review counts
// with. notified is true when notify holds and the write raised the
// subject's level (grantRank; a created grant counts as a raise), decided
// under the artifact's lock; the event's detail then records
// "notified": true.
func (s *sqlStore) putGrant(ctx context.Context, g *Grant, maxGrants int, crossScope bool, reviewHome string, actor *Actor, notify bool) (GrantPut, bool, error) {
	if g == nil || (g.SubjectKind != SubjectPrincipal && g.SubjectKind != SubjectScope) || g.SubjectRef == "" ||
		!validGrantPermission(g.Permission) || !reviewSubjectOK(g.Permission, g.SubjectKind, g.SubjectRef) {
		return GrantPutUnchanged, false, errors.New("artifacts: PutGrant needs a principal or scope grant with a permission (review: a user principal only)")
	}
	if !actor.valid() {
		return GrantPutUnchanged, false, errNoActor
	}
	// Only a user is ever told.
	notify = notify && g.SubjectKind == SubjectPrincipal && strings.HasPrefix(g.SubjectRef, PrincipalKindUser+":")
	// The automatic form is reserved to the creation path (ptone/scion#7134).
	if g.CreatedByRef == SponsorReviewCreator || actor.Authority == AuthoritySponsorReview {
		return GrantPutUnchanged, false, errSponsorReviewReserved
	}
	if stewardActor(actor) && g.Permission == GrantAdmin {
		return GrantPutUnchanged, false, ErrStewardActionNotAllowed
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return GrantPutUnchanged, false, fmt.Errorf("artifacts: begin grant: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.lockLiveArtifact(ctx, tx, g.ArtifactID); err != nil {
		return GrantPutUnchanged, false, err
	}
	if err := s.refuseAttachmentTx(ctx, tx, g.ArtifactID); err != nil {
		return GrantPutUnchanged, false, err
	}
	if reviewHome != "" {
		var home, ownerKind string
		if err := tx.QueryRowContext(ctx, s.rebind(`SELECT scope_ref, owner_kind FROM artifact WHERE id = ?`), g.ArtifactID).Scan(&home, &ownerKind); err != nil {
			return GrantPutUnchanged, false, fmt.Errorf("artifacts: read home: %w", err)
		}
		if home != reviewHome || ownerKind != PrincipalKindAgent {
			return GrantPutUnchanged, false, ErrReviewGrantRefused
		}
	}
	// The review path checks its own home and owner first, so its refusal
	// stays ErrReviewGrantRefused.
	if err := s.checkExpect(ctx, tx, g.ArtifactID, actor); err != nil {
		return GrantPutUnchanged, false, err
	}
	if g.SubjectKind == SubjectScope {
		var home string
		if err := tx.QueryRowContext(ctx, s.rebind(`SELECT scope_ref FROM artifact WHERE id = ?`), g.ArtifactID).Scan(&home); err != nil {
			return GrantPutUnchanged, false, fmt.Errorf("artifacts: read home: %w", err)
		}
		switch {
		case g.SubjectRef == home && g.Permission == GrantAdmin:
			return GrantPutUnchanged, false, ErrHomeGrantAdmin
		case g.SubjectRef != home && !crossScope:
			return GrantPutUnchanged, false, ErrCrossScopeDisabled
		}
	}
	var (
		id       string
		perm     string
		by       sql.NullString
		created  dbTime
		notified bool
	)
	err = tx.QueryRowContext(ctx, s.rebind(`SELECT id, permission, created_by_ref, created_at FROM artifact_grant
		WHERE artifact_id = ? AND subject_kind = ? AND subject_ref = ?`), g.ArtifactID, g.SubjectKind, g.SubjectRef).Scan(&id, &perm, &by, &created)
	switch {
	case err == nil:
		// PutReviewGrant's grant is always a review grant, so this refuses
		// it too.
		if g.Permission == GrantReview && perm != GrantReview {
			return GrantPutUnchanged, false, ErrReviewReplacesAccess
		}
		if perm != g.Permission {
			// A level change by a person ends an automatic grant's
			// automatic form (ptone/scion#7134): its creator becomes that
			// person, so it no longer depends on a role in the home
			// project. Every other grant keeps its creator.
			newBy := by
			if by.String == SponsorReviewCreator {
				newBy = sql.NullString{String: PrincipalRef(actor.Kind, actor.Ref), Valid: true}
			}
			if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE artifact_grant SET permission = ?, created_by_ref = ? WHERE id = ?`),
				g.Permission, newBy, id); err != nil {
				return GrantPutUnchanged, false, fmt.Errorf("artifacts: update grant: %w", err)
			}
			detail := map[string]any{
				"grantId": id, "subjectKind": g.SubjectKind, "subjectRef": g.SubjectRef,
				"oldPermission": perm, "newPermission": g.Permission,
			}
			// Only a raise is announced (Ask for review): a lowering or a
			// level change to the same rank is not.
			notified = notify && grantRank(g.Permission) > grantRank(perm)
			if notified {
				detail["notified"] = true
			}
			if err := s.insertEvent(ctx, tx, g.ArtifactID, EventGrantChanged, actor, time.Now(), detail); err != nil {
				return GrantPutUnchanged, false, err
			}
		}
		if err := tx.Commit(); err != nil {
			return GrantPutUnchanged, false, fmt.Errorf("artifacts: commit grant: %w", err)
		}
		g.ID, g.CreatedAt = id, created.Time
		if perm != g.Permission {
			return GrantPutChanged, notified, nil
		}
		return GrantPutUnchanged, false, nil
	case !errors.Is(err, sql.ErrNoRows):
		return GrantPutUnchanged, false, fmt.Errorf("artifacts: find grant: %w", err)
	}
	var n int
	if err := tx.QueryRowContext(ctx, s.rebind(`SELECT COUNT(*) FROM artifact_grant
		WHERE artifact_id = ? AND subject_kind IN (?, ?)`), g.ArtifactID, SubjectPrincipal, SubjectScope).Scan(&n); err != nil {
		return GrantPutUnchanged, false, fmt.Errorf("artifacts: count grants: %w", err)
	}
	if n >= maxGrants {
		return GrantPutUnchanged, false, ErrTooManyGrants
	}
	if err := s.insertGrant(ctx, tx, g); err != nil {
		return GrantPutUnchanged, false, err
	}
	detail := map[string]any{
		"grantId": g.ID, "subjectKind": g.SubjectKind, "subjectRef": g.SubjectRef, "permission": g.Permission,
	}
	// A created grant is a raise.
	if notify {
		detail["notified"] = true
	}
	if err := s.insertEvent(ctx, tx, g.ArtifactID, EventGrantCreated, actor, time.Now(), detail); err != nil {
		return GrantPutUnchanged, false, err
	}
	if err := tx.Commit(); err != nil {
		return GrantPutUnchanged, false, fmt.Errorf("artifacts: commit grant: %w", err)
	}
	return GrantPutCreated, notify, nil
}

// errSponsorReviewReserved refuses a grant write, other than the creation of
// an artifact, that carries the automatic review grant's creator or
// authority.
var errSponsorReviewReserved = errors.New("artifacts: the automatic review grant is written only when an artifact is created")

func (s *sqlStore) insertGrant(ctx context.Context, tx *sql.Tx, g *Grant) error {
	if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO artifact_grant
		(id, artifact_id, subject_kind, subject_ref, permission, expires_at, created_by_ref, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
		g.ID, g.ArtifactID, g.SubjectKind, g.SubjectRef, g.Permission, s.nullTimeArg(g.ExpiresAt),
		nullString(g.CreatedByRef), s.timeArg(g.CreatedAt)); err != nil {
		return fmt.Errorf("artifacts: insert grant: %w", err)
	}
	return nil
}

// DeleteGrant implements Store.
func (s *sqlStore) DeleteGrant(ctx context.Context, artifactID, grantID string, actor *Actor) error {
	if !actor.valid() {
		return errNoActor
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("artifacts: begin delete grant: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.lockLiveArtifact(ctx, tx, artifactID); err != nil {
		return err
	}
	if err := s.checkExpect(ctx, tx, artifactID, actor); err != nil {
		return err
	}
	var home int
	if err := tx.QueryRowContext(ctx, s.rebind(`SELECT COUNT(*) FROM artifact_grant g JOIN artifact a ON a.id = g.artifact_id
		WHERE g.id = ? AND g.artifact_id = ? AND g.subject_kind = ? AND g.subject_ref = a.scope_ref`),
		grantID, artifactID, SubjectScope).Scan(&home); err != nil {
		return fmt.Errorf("artifacts: check home grant: %w", err)
	}
	if home > 0 {
		return ErrConflict
	}
	var subjectKind, subjectRef, perm string
	err = tx.QueryRowContext(ctx, s.rebind(`SELECT subject_kind, subject_ref, permission FROM artifact_grant
		WHERE id = ? AND artifact_id = ? AND subject_kind IN (?, ?)`), grantID, artifactID, SubjectPrincipal, SubjectScope).Scan(&subjectKind, &subjectRef, &perm)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("artifacts: find grant: %w", err)
	}
	res, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM artifact_grant
		WHERE id = ? AND artifact_id = ? AND subject_kind IN (?, ?)`), grantID, artifactID, SubjectPrincipal, SubjectScope)
	if err != nil {
		return fmt.Errorf("artifacts: delete grant: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("artifacts: delete grant: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	if err := s.insertEvent(ctx, tx, artifactID, EventGrantRemoved, actor, time.Now(), map[string]any{
		"grantId": grantID, "subjectKind": subjectKind, "subjectRef": subjectRef, "permission": perm,
	}); err != nil {
		return err
	}
	return commit(tx)
}

// SetExpiry implements Store.
func (s *sqlStore) SetExpiry(ctx context.Context, artifactID string, expiresAt *time.Time, actor *Actor) (*Artifact, error) {
	return s.UpdateArtifact(ctx, artifactID, ArtifactUpdate{SetExpiry: true, ExpiresAt: expiresAt, Actor: actor})
}

// UpdateArtifact implements Store.
func (s *sqlStore) UpdateArtifact(ctx context.Context, artifactID string, u ArtifactUpdate) (*Artifact, error) {
	if g := u.HomeGrant; g != nil && (g.SubjectKind != SubjectScope || g.SubjectRef == "" ||
		g.Permission != GrantRead || g.ArtifactID != artifactID || u.MaxGrants <= 0) {
		return nil, errors.New("artifacts: a move needs the new home scope's read grant and a grant cap")
	}
	if (u.HomeGrant != nil || u.SetExpiry) && !u.Actor.valid() {
		return nil, errNoActor
	}
	if u.HomeGrant != nil && stewardActor(u.Actor) {
		return nil, ErrStewardActionNotAllowed
	}
	now := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("artifacts: begin update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.lockLiveArtifact(ctx, tx, artifactID); err != nil {
		return nil, err
	}
	if err := s.refuseAttachmentTx(ctx, tx, artifactID); err != nil {
		return nil, err
	}
	if err := s.checkExpect(ctx, tx, artifactID, u.Actor); err != nil {
		return nil, err
	}
	if u.HomeGrant != nil {
		if err := s.moveArtifact(ctx, tx, artifactID, u.HomeGrant, u.MaxGrants, u.Actor, now); err != nil {
			return nil, err
		}
	}
	if u.SetExpiry {
		var old dbTime
		if err := tx.QueryRowContext(ctx, s.rebind(`SELECT expires_at FROM artifact WHERE id = ?`), artifactID).Scan(&old); err != nil {
			return nil, fmt.Errorf("artifacts: read expiry: %w", err)
		}
		if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE artifact SET expires_at = ? WHERE id = ?`),
			s.nullTimeArg(u.ExpiresAt), artifactID); err != nil {
			return nil, fmt.Errorf("artifacts: set expiry: %w", err)
		}
		action, detail := EventExpiryCleared, map[string]any{}
		if old.Valid {
			detail["oldExpiresAt"] = old.Time.UTC().Format(time.RFC3339Nano)
		}
		if u.ExpiresAt != nil {
			action = EventExpirySet
			detail["newExpiresAt"] = u.ExpiresAt.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)
		}
		if err := s.insertEvent(ctx, tx, artifactID, action, u.Actor, now, detail); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("artifacts: commit update: %w", err)
	}
	return s.GetArtifact(ctx, artifactID)
}

// moveArtifact moves a locked artifact to homeGrant's scope inside tx.
//
// It records a moved event by actor with the old and new home scope and
// the id of the old home's scope grant it dropped ("" when there was
// none): a move takes access away from the old home's members, and the
// event table audits every change of access.
func (s *sqlStore) moveArtifact(ctx context.Context, tx *sql.Tx, artifactID string, homeGrant *Grant, maxGrants int, actor *Actor, now time.Time) error {
	var old string
	if err := tx.QueryRowContext(ctx, s.rebind(`SELECT scope_ref FROM artifact WHERE id = ?`), artifactID).Scan(&old); err != nil {
		return fmt.Errorf("artifacts: read home: %w", err)
	}
	scope := homeGrant.SubjectRef
	if scope == old {
		return nil
	}
	// The key stays unique per owner and scope among live artifacts.
	var clash int
	if err := tx.QueryRowContext(ctx, s.rebind(`SELECT COUNT(*) FROM artifact a JOIN artifact b
		ON b.owner_kind = a.owner_kind AND b.owner_ref = a.owner_ref AND b."key" = a."key"
		WHERE a.id = ? AND a."key" IS NOT NULL AND b.id <> a.id AND b.deleted_at IS NULL
		AND b.scope_kind = a.scope_kind AND b.scope_ref = ?`), artifactID, scope).Scan(&clash); err != nil {
		return fmt.Errorf("artifacts: check key: %w", err)
	}
	if clash > 0 {
		return ErrConflict
	}
	if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE artifact SET scope_ref = ? WHERE id = ?`), scope, artifactID); err != nil {
		return fmt.Errorf("artifacts: move: %w", err)
	}
	// The old home loses its grant: a move is not a share.
	var dropped string
	err := tx.QueryRowContext(ctx, s.rebind(`SELECT id FROM artifact_grant
		WHERE artifact_id = ? AND subject_kind = ? AND subject_ref = ?`), artifactID, SubjectScope, old).Scan(&dropped)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("artifacts: find old home grant: %w", err)
	}
	if _, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM artifact_grant
		WHERE artifact_id = ? AND subject_kind = ? AND subject_ref = ?`), artifactID, SubjectScope, old); err != nil {
		return fmt.Errorf("artifacts: drop old home grant: %w", err)
	}
	if err := s.insertEvent(ctx, tx, artifactID, EventMoved, actor, now,
		map[string]any{"fromScope": old, "toScope": scope, "droppedGrantId": dropped}); err != nil {
		return err
	}
	var have, haveAdmin, total int
	if err := tx.QueryRowContext(ctx, s.rebind(`SELECT
		COALESCE(SUM(CASE WHEN subject_kind = ? AND subject_ref = ? THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN subject_kind = ? AND subject_ref = ? AND permission = ? THEN 1 ELSE 0 END), 0), COUNT(*)
		FROM artifact_grant WHERE artifact_id = ? AND subject_kind IN (?, ?)`),
		SubjectScope, scope, SubjectScope, scope, GrantAdmin, artifactID, SubjectPrincipal, SubjectScope).Scan(&have, &haveAdmin, &total); err != nil {
		return fmt.Errorf("artifacts: count grants: %w", err)
	}
	if haveAdmin > 0 {
		// The home project's grant is read or write only.
		return ErrHomeGrantAdmin
	}
	if have > 0 {
		return nil
	}
	if total >= maxGrants {
		return ErrTooManyGrants
	}
	return s.insertGrant(ctx, tx, homeGrant)
}

// SweepExpired implements Store.
func (s *sqlStore) SweepExpired(ctx context.Context, now time.Time, limit int) ([]ArtifactRef, error) {
	if limit <= 0 {
		return nil, nil
	}
	at := s.timeArg(now)
	rows, err := s.db.QueryContext(ctx, s.rebind(`SELECT id, scope_ref FROM artifact
		WHERE deleted_at IS NULL AND expires_at IS NOT NULL AND expires_at <= ?
		ORDER BY expires_at LIMIT `+strconv.Itoa(limit)), at)
	if err != nil {
		return nil, fmt.Errorf("artifacts: find expired: %w", err)
	}
	var found []ArtifactRef
	for rows.Next() {
		var ref ArtifactRef
		if err := rows.Scan(&ref.ID, &ref.ScopeRef); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("artifacts: scan expired: %w", err)
		}
		found = append(found, ref)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("artifacts: find expired: %w", err)
	}
	var swept []ArtifactRef
	for _, ref := range found {
		_, err := s.DeleteArtifact(ctx, ref.ID, DeleteOptions{Actor: &retentionActor, ExpiredAt: &now, Now: now})
		switch {
		case errors.Is(err, ErrNotFound):
			// Deleted meanwhile, or its expiry moved since it was found.
		case err != nil:
			return swept, err
		default:
			swept = append(swept, ref)
		}
	}
	return swept, nil
}
