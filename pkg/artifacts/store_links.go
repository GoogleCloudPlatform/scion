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
)

// CreateLink implements Store.
func (s *sqlStore) CreateLink(ctx context.Context, g *Grant, maxLinks int, now time.Time) error {
	if g == nil || g.SubjectKind != SubjectLink || g.Permission != GrantRead || g.ExpiresAt == nil || g.SubjectRef == "" {
		return errors.New("artifacts: CreateLink needs a read link grant with an expiry and a token hash")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("artifacts: begin link: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// A no-op write to the artifact row takes its row lock (Postgres) or
	// the write lock (SQLite), so concurrent mints count links one at a
	// time, without moving updated_at (the list order).
	res, err := tx.ExecContext(ctx, s.rebind(`UPDATE artifact SET id = id WHERE id = ? AND deleted_at IS NULL`), g.ArtifactID)
	if err != nil {
		return fmt.Errorf("artifacts: lock artifact: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return fmt.Errorf("artifacts: lock artifact: %w", err)
		}
		return ErrNotFound
	}
	at := s.timeArg(now)
	if _, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM artifact_grant
		WHERE artifact_id = ? AND subject_kind = ? AND expires_at <= ?`), g.ArtifactID, SubjectLink, at); err != nil {
		return fmt.Errorf("artifacts: drop expired links: %w", err)
	}
	var n int
	if err := tx.QueryRowContext(ctx, s.rebind(`SELECT COUNT(*) FROM artifact_grant
		WHERE artifact_id = ? AND subject_kind = ?`), g.ArtifactID, SubjectLink).Scan(&n); err != nil {
		return fmt.Errorf("artifacts: count links: %w", err)
	}
	if n >= maxLinks {
		return ErrTooManyLinks
	}
	if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO artifact_grant
		(id, artifact_id, subject_kind, subject_ref, permission, expires_at, created_by_ref, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
		g.ID, g.ArtifactID, g.SubjectKind, g.SubjectRef, g.Permission, s.nullTimeArg(g.ExpiresAt),
		nullString(g.CreatedByRef), s.timeArg(g.CreatedAt)); err != nil {
		return fmt.Errorf("artifacts: insert link: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("artifacts: commit link: %w", err)
	}
	return nil
}

// ResolveLink implements Store. Every refusal (no such hash, an expired
// link, a deleted, expired or unpublished artifact) is the same empty
// result of the same single indexed query. A link row without an expiry
// never matches (NULL > now is not true); CreateLink never writes one.
func (s *sqlStore) ResolveLink(ctx context.Context, tokenHash string, now time.Time) (*Artifact, *Grant, error) {
	var (
		a                  Artifact
		g                  Grant
		key, by            sql.NullString
		seq                sql.NullInt64
		expires, created   dbTime
		updated            dbTime
		gExpires, gCreated dbTime
	)
	at := s.timeArg(now)
	err := s.db.QueryRowContext(ctx, s.rebind(`SELECT
		a.id, a.scope_kind, a.scope_ref, a.owner_kind, a.owner_ref, a."key", a.title, a.current_seq,
		a.expires_at, a.created_at, a.updated_at,
		g.id, g.subject_ref, g.permission, g.expires_at, g.created_by_ref, g.created_at
		FROM artifact_grant g JOIN artifact a ON a.id = g.artifact_id
		WHERE g.subject_kind = ? AND g.subject_ref = ? AND g.permission = ?
		AND g.expires_at > ?
		AND a.deleted_at IS NULL AND (a.expires_at IS NULL OR a.expires_at > ?)
		AND a.current_seq IS NOT NULL`),
		SubjectLink, tokenHash, GrantRead, at, at).Scan(
		&a.ID, &a.ScopeKind, &a.ScopeRef, &a.OwnerKind, &a.OwnerRef, &key, &a.Title, &seq,
		&expires, &created, &updated,
		&g.ID, &g.SubjectRef, &g.Permission, &gExpires, &by, &gCreated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("artifacts: resolve link: %w", err)
	}
	a.Key = key.String
	a.CurrentSeq = int(seq.Int64)
	a.ExpiresAt = expires.ptr()
	a.CreatedAt = created.Time
	a.UpdatedAt = updated.Time
	g.ArtifactID = a.ID
	g.SubjectKind = SubjectLink
	g.ExpiresAt = gExpires.ptr()
	g.CreatedByRef = by.String
	g.CreatedAt = gCreated.Time
	return &a, &g, nil
}

// LinkActive implements Store.
func (s *sqlStore) LinkActive(ctx context.Context, artifactID, linkID string, now time.Time) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, s.rebind(`SELECT COUNT(*) FROM artifact_grant
		WHERE id = ? AND artifact_id = ? AND subject_kind = ? AND permission = ?
		AND expires_at > ?`),
		linkID, artifactID, SubjectLink, GrantRead, s.timeArg(now)).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("artifacts: check link: %w", err)
	}
	return n == 1, nil
}

// RevokeLink implements Store.
func (s *sqlStore) RevokeLink(ctx context.Context, artifactID, linkID string) error {
	res, err := s.db.ExecContext(ctx, s.rebind(`DELETE FROM artifact_grant
		WHERE id = ? AND artifact_id = ? AND subject_kind = ?`), linkID, artifactID, SubjectLink)
	if err != nil {
		return fmt.Errorf("artifacts: revoke link: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("artifacts: revoke link: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
