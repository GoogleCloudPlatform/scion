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
func (s *sqlStore) PutGrant(ctx context.Context, g *Grant, maxGrants int) (bool, error) {
	if g == nil || (g.SubjectKind != SubjectPrincipal && g.SubjectKind != SubjectScope) || g.SubjectRef == "" ||
		(g.Permission != GrantRead && g.Permission != GrantWrite && g.Permission != GrantAdmin) {
		return false, errors.New("artifacts: PutGrant needs a principal or scope grant with a permission")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("artifacts: begin grant: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.lockLiveArtifact(ctx, tx, g.ArtifactID); err != nil {
		return false, err
	}
	var (
		id      string
		created dbTime
	)
	err = tx.QueryRowContext(ctx, s.rebind(`SELECT id, created_at FROM artifact_grant
		WHERE artifact_id = ? AND subject_kind = ? AND subject_ref = ?`), g.ArtifactID, g.SubjectKind, g.SubjectRef).Scan(&id, &created)
	switch {
	case err == nil:
		if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE artifact_grant SET permission = ? WHERE id = ?`), g.Permission, id); err != nil {
			return false, fmt.Errorf("artifacts: update grant: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf("artifacts: commit grant: %w", err)
		}
		g.ID, g.CreatedAt = id, created.Time
		return false, nil
	case !errors.Is(err, sql.ErrNoRows):
		return false, fmt.Errorf("artifacts: find grant: %w", err)
	}
	var n int
	if err := tx.QueryRowContext(ctx, s.rebind(`SELECT COUNT(*) FROM artifact_grant
		WHERE artifact_id = ? AND subject_kind IN (?, ?)`), g.ArtifactID, SubjectPrincipal, SubjectScope).Scan(&n); err != nil {
		return false, fmt.Errorf("artifacts: count grants: %w", err)
	}
	if n >= maxGrants {
		return false, ErrTooManyGrants
	}
	if err := s.insertGrant(ctx, tx, g); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("artifacts: commit grant: %w", err)
	}
	return true, nil
}

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
func (s *sqlStore) DeleteGrant(ctx context.Context, artifactID, grantID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("artifacts: begin delete grant: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.lockLiveArtifact(ctx, tx, artifactID); err != nil {
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
	return commit(tx)
}

// SetExpiry implements Store.
func (s *sqlStore) SetExpiry(ctx context.Context, artifactID string, expiresAt *time.Time) (*Artifact, error) {
	return s.UpdateArtifact(ctx, artifactID, ArtifactUpdate{SetExpiry: true, ExpiresAt: expiresAt})
}

// UpdateArtifact implements Store.
func (s *sqlStore) UpdateArtifact(ctx context.Context, artifactID string, u ArtifactUpdate) (*Artifact, error) {
	if g := u.HomeGrant; g != nil && (g.SubjectKind != SubjectScope || g.SubjectRef == "" ||
		g.Permission != GrantRead || g.ArtifactID != artifactID || u.MaxGrants <= 0) {
		return nil, errors.New("artifacts: a move needs the new home scope's read grant and a grant cap")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("artifacts: begin update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.lockLiveArtifact(ctx, tx, artifactID); err != nil {
		return nil, err
	}
	if u.HomeGrant != nil {
		if err := s.moveArtifact(ctx, tx, artifactID, u.HomeGrant, u.MaxGrants); err != nil {
			return nil, err
		}
	}
	if u.SetExpiry {
		if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE artifact SET expires_at = ? WHERE id = ?`),
			s.nullTimeArg(u.ExpiresAt), artifactID); err != nil {
			return nil, fmt.Errorf("artifacts: set expiry: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("artifacts: commit update: %w", err)
	}
	return s.GetArtifact(ctx, artifactID)
}

// moveArtifact moves a locked artifact to homeGrant's scope inside tx.
func (s *sqlStore) moveArtifact(ctx context.Context, tx *sql.Tx, artifactID string, homeGrant *Grant, maxGrants int) error {
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
	if _, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM artifact_grant
		WHERE artifact_id = ? AND subject_kind = ? AND subject_ref = ?`), artifactID, SubjectScope, old); err != nil {
		return fmt.Errorf("artifacts: drop old home grant: %w", err)
	}
	var have, total int
	if err := tx.QueryRowContext(ctx, s.rebind(`SELECT
		COALESCE(SUM(CASE WHEN subject_kind = ? AND subject_ref = ? THEN 1 ELSE 0 END), 0), COUNT(*)
		FROM artifact_grant WHERE artifact_id = ? AND subject_kind IN (?, ?)`),
		SubjectScope, scope, artifactID, SubjectPrincipal, SubjectScope).Scan(&have, &total); err != nil {
		return fmt.Errorf("artifacts: count grants: %w", err)
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
func (s *sqlStore) SweepExpired(ctx context.Context, now time.Time, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	at := s.timeArg(now)
	rows, err := s.db.QueryContext(ctx, s.rebind(`SELECT id FROM artifact
		WHERE deleted_at IS NULL AND expires_at IS NOT NULL AND expires_at <= ?
		ORDER BY expires_at LIMIT `+strconv.Itoa(limit)), at)
	if err != nil {
		return 0, fmt.Errorf("artifacts: find expired: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("artifacts: scan expired: %w", err)
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("artifacts: find expired: %w", err)
	}
	swept := 0
	for _, id := range ids {
		ok, err := s.sweepOne(ctx, id, at)
		if err != nil {
			return swept, err
		}
		if ok {
			swept++
		}
	}
	return swept, nil
}

// sweepOne soft-deletes one expired artifact and deletes its grants and
// links, if it is still live and expired (its expiry may have been moved
// since it was found).
func (s *sqlStore) sweepOne(ctx context.Context, id string, at any) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("artifacts: begin sweep: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, s.rebind(`UPDATE artifact SET deleted_at = ?
		WHERE id = ? AND deleted_at IS NULL AND expires_at IS NOT NULL AND expires_at <= ?`), at, id, at)
	if err != nil {
		return false, fmt.Errorf("artifacts: expire artifact: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM artifact_grant WHERE artifact_id = ?`), id); err != nil {
		return false, fmt.Errorf("artifacts: drop grants: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("artifacts: commit sweep: %w", err)
	}
	return true, nil
}
