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
	"math"
	"strconv"
	"strings"
	"time"
)

type dialect int

const (
	dialectSQLite dialect = iota
	dialectPostgres
)

// initLockKey is the Postgres advisory lock that serializes Init across
// hubs sharing one database, so concurrent CREATE TABLE IF NOT EXISTS
// statements cannot race on the catalog. The value is arbitrary but fixed
// ("artifact" in ASCII).
const initLockKey int64 = 0x6172746966616374

// sqliteTimeLayout is the fixed-width UTC layout used for SQLite TEXT
// timestamps, so that they sort lexically in time order.
const sqliteTimeLayout = "2006-01-02T15:04:05.000000000Z"

// migration is one named schema step. Steps run in order, each at most once
// per database, and are recorded in artifact_migrations.
type migration struct {
	name     string
	sqlite   string
	postgres string
}

// migrations is the ordered schema history. Append new steps; never edit or
// reorder applied ones.
var migrations = []migration{
	{name: migrationInitial, sqlite: sqliteSchema, postgres: postgresSchema},
	{name: migrationRemoteFiles, sqlite: sqliteRemoteFiles, postgres: postgresRemoteFiles},
	{name: migrationVersionUploads, sqlite: sqliteVersionUploads, postgres: postgresVersionUploads},
	{name: migrationFinalizeClaims, sqlite: sqliteFinalizeClaims, postgres: postgresFinalizeClaims},
	{name: migrationLinkTokens, sqlite: sqliteLinkTokens, postgres: postgresLinkTokens},
	{name: migrationBlobGC, sqlite: sqliteBlobGC, postgres: postgresBlobGC},
}

const ledgerSQLite = `CREATE TABLE IF NOT EXISTS artifact_migrations (
    name       TEXT PRIMARY KEY,
    applied_at TEXT NOT NULL
)`

const ledgerPostgres = `CREATE TABLE IF NOT EXISTS artifact_migrations (
    name       TEXT PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL
)`

// sqlStore implements Store for both dialects. Queries are written with ?
// placeholders and rebound to $N for Postgres; only the DDL and timestamp
// encoding differ between dialects.
type sqlStore struct {
	db      *sql.DB
	dialect dialect
}

// rebind rewrites ? placeholders to $1..$N for Postgres. Queries in this
// file never contain a literal question mark.
func (s *sqlStore) rebind(q string) string {
	if s.dialect != dialectPostgres {
		return q
	}
	var b strings.Builder
	n := 0
	for _, r := range q {
		if r == '?' {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// timeArg encodes a timestamp for the dialect. Both dialects store
// microsecond precision (Postgres TIMESTAMPTZ's resolution), so a value reads
// back the same whichever database holds it.
func (s *sqlStore) timeArg(t time.Time) any {
	t = t.UTC().Truncate(time.Microsecond)
	if s.dialect == dialectSQLite {
		return t.Format(sqliteTimeLayout)
	}
	return t
}

func (s *sqlStore) nullTimeArg(t *time.Time) any {
	if t == nil {
		return nil
	}
	return s.timeArg(*t)
}

// dbTime scans a timestamp stored by either dialect.
type dbTime struct {
	Time  time.Time
	Valid bool
}

func (d *dbTime) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		d.Time, d.Valid = time.Time{}, false
		return nil
	case time.Time:
		d.Time, d.Valid = v.UTC(), true
		return nil
	case string:
		return d.parse(v)
	case []byte:
		return d.parse(string(v))
	default:
		return fmt.Errorf("artifacts: cannot scan %T as a timestamp", src)
	}
}

func (d *dbTime) parse(v string) error {
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return fmt.Errorf("artifacts: bad timestamp %q: %w", v, err)
	}
	d.Time, d.Valid = t.UTC(), true
	return nil
}

func (d dbTime) ptr() *time.Time {
	if !d.Valid {
		return nil
	}
	t := d.Time
	return &t
}

func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func nullInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

// Init implements Store.
func (s *sqlStore) Init(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("artifacts: begin init: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	ledger := ledgerSQLite
	if s.dialect == dialectPostgres {
		if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", initLockKey); err != nil {
			return fmt.Errorf("artifacts: init lock: %w", err)
		}
		ledger = ledgerPostgres
	}
	if _, err := tx.ExecContext(ctx, ledger); err != nil {
		return fmt.Errorf("artifacts: create migrations ledger: %w", err)
	}
	for _, m := range migrations {
		var n int
		if err := tx.QueryRowContext(ctx, s.rebind("SELECT COUNT(*) FROM artifact_migrations WHERE name = ?"), m.name).Scan(&n); err != nil {
			return fmt.Errorf("artifacts: read migrations ledger: %w", err)
		}
		if n > 0 {
			continue
		}
		ddl := m.sqlite
		if s.dialect == dialectPostgres {
			ddl = m.postgres
		}
		if _, err := tx.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("artifacts: migration %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx, s.rebind("INSERT INTO artifact_migrations (name, applied_at) VALUES (?, ?)"),
			m.name, s.timeArg(time.Now())); err != nil {
			return fmt.Errorf("artifacts: record migration %s: %w", m.name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("artifacts: commit init: %w", err)
	}
	return nil
}

// CreatePublished implements Store.
func (s *sqlStore) CreatePublished(ctx context.Context, a *Artifact, v *Version, files []File, grants []Grant) error {
	if v != nil && v.State != VersionStateReady {
		return errors.New("artifacts: CreatePublished needs a ready version")
	}
	return s.createArtifact(ctx, a, v, files, grants)
}

// CreatePending implements Store.
func (s *sqlStore) CreatePending(ctx context.Context, a *Artifact, v *Version, files []File, grants []Grant) error {
	if v != nil && v.State != VersionStatePending {
		return errors.New("artifacts: CreatePending needs a pending version")
	}
	err := s.createArtifact(ctx, a, v, files, grants)
	if err != nil && a.Key != "" {
		// A concurrent publish may have taken the key first; the unique
		// index refused this one. Report it as a conflict so the caller
		// can append to that artifact instead.
		if _, lookErr := s.GetArtifactByKey(ctx, a.ScopeKind, a.ScopeRef, a.OwnerKind, a.OwnerRef, a.Key); lookErr == nil {
			return ErrConflict
		}
	}
	return err
}

// createArtifact writes an artifact, its first version, the version's
// files and the grants in one transaction. The artifact's current version
// is the version when it is ready, and none otherwise.
func (s *sqlStore) createArtifact(ctx context.Context, a *Artifact, v *Version, files []File, grants []Grant) error {
	if a == nil || v == nil {
		return errors.New("artifacts: creating an artifact needs an artifact and a version")
	}
	if v.ArtifactID != a.ID {
		return errors.New("artifacts: version does not belong to the artifact")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("artifacts: begin publish: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	current := 0
	if v.State == VersionStateReady {
		current = v.Seq
	}
	if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO artifact
		(id, scope_kind, scope_ref, owner_kind, owner_ref, "key", title, current_seq, expires_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		a.ID, a.ScopeKind, a.ScopeRef, a.OwnerKind, a.OwnerRef, nullString(a.Key), a.Title, nullInt(current),
		s.nullTimeArg(a.ExpiresAt), s.timeArg(a.CreatedAt), s.timeArg(a.UpdatedAt)); err != nil {
		return fmt.Errorf("artifacts: insert artifact: %w", err)
	}
	if err := s.insertVersion(ctx, tx, v, files); err != nil {
		return err
	}
	for _, g := range grants {
		if g.ArtifactID != a.ID {
			return errors.New("artifacts: grant does not belong to the artifact")
		}
		if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO artifact_grant
			(id, artifact_id, subject_kind, subject_ref, permission, expires_at, created_by_ref, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
			g.ID, g.ArtifactID, g.SubjectKind, g.SubjectRef, g.Permission, s.nullTimeArg(g.ExpiresAt),
			nullString(g.CreatedByRef), s.timeArg(g.CreatedAt)); err != nil {
			return fmt.Errorf("artifacts: insert grant: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("artifacts: commit publish: %w", err)
	}
	a.CurrentSeq = current
	return nil
}

// insertVersion writes a version row and its manifest inside tx.
func (s *sqlStore) insertVersion(ctx context.Context, tx *sql.Tx, v *Version, files []File) error {
	if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO artifact_version
		(id, artifact_id, seq, kind, entry_path, note, total_bytes, file_count, created_by_kind, created_by_ref, created_at, state)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		v.ID, v.ArtifactID, v.Seq, v.Kind, v.EntryPath, nullString(v.Note), v.TotalBytes, v.FileCount,
		nullString(v.CreatedByKind), nullString(v.CreatedByRef), s.timeArg(v.CreatedAt), v.State); err != nil {
		return fmt.Errorf("artifacts: insert version: %w", err)
	}
	return s.insertFiles(ctx, tx, v.ID, files)
}

func (s *sqlStore) insertFiles(ctx context.Context, tx *sql.Tx, versionID string, files []File) error {
	for _, f := range files {
		if f.VersionID != versionID {
			return errors.New("artifacts: file does not belong to the version")
		}
		if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO artifact_file
			(version_id, path, size, sha256, media_type, origin, source_url, fetch_status, fetch_error, received)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			f.VersionID, f.Path, f.Size, nullString(f.SHA256), f.MediaType, fileOrigin(f.Origin),
			nullString(f.SourceURL), nullString(f.FetchStatus), nullString(f.FetchError), !f.Pending); err != nil {
			return fmt.Errorf("artifacts: insert file: %w", err)
		}
	}
	return nil
}

// CreateVersion implements Store.
func (s *sqlStore) CreateVersion(ctx context.Context, v *Version, files []File, maxPending int) error {
	if v == nil || v.State != VersionStatePending {
		return errors.New("artifacts: CreateVersion needs a pending version")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("artifacts: begin version: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Writing the artifact row first takes its row lock (Postgres) or the
	// write lock (SQLite), so concurrent appends assign seqs one at a time.
	res, err := tx.ExecContext(ctx, s.rebind(`UPDATE artifact SET updated_at = ? WHERE id = ? AND deleted_at IS NULL`),
		s.timeArg(v.CreatedAt), v.ArtifactID)
	if err != nil {
		return fmt.Errorf("artifacts: lock artifact: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return ErrNotFound
	}
	var maxSeq, pending int
	if err := tx.QueryRowContext(ctx, s.rebind(`SELECT COALESCE(MAX(seq), 0),
		COALESCE(SUM(CASE WHEN state IN (?, ?) THEN 1 ELSE 0 END), 0)
		FROM artifact_version WHERE artifact_id = ?`), VersionStatePending, VersionStateFinalizing, v.ArtifactID).Scan(&maxSeq, &pending); err != nil {
		return fmt.Errorf("artifacts: next seq: %w", err)
	}
	if pending >= maxPending {
		return ErrTooManyPending
	}
	v.Seq = maxSeq + 1
	if err := s.insertVersion(ctx, tx, v, files); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("artifacts: commit version: %w", err)
	}
	return nil
}

// GetArtifactByKey implements Store.
func (s *sqlStore) GetArtifactByKey(ctx context.Context, scopeKind, scopeRef, ownerKind, ownerRef, key string) (*Artifact, error) {
	if key == "" {
		return nil, ErrNotFound
	}
	return s.getArtifact(ctx, `scope_kind = ? AND scope_ref = ? AND owner_kind = ? AND owner_ref = ? AND "key" = ?`,
		scopeKind, scopeRef, ownerKind, ownerRef, key)
}

// MarkReceived implements Store.
func (s *sqlStore) MarkReceived(ctx context.Context, versionID, path, mediaType string) error {
	res, err := s.db.ExecContext(ctx, s.rebind(`UPDATE artifact_file SET received = ?, media_type = ?
		WHERE version_id = ? AND path = ? AND origin = ?
		AND EXISTS (SELECT 1 FROM artifact_version WHERE id = ? AND state = ?)`),
		true, mediaType, versionID, path, FileOriginUpload, versionID, VersionStatePending)
	if err != nil {
		return fmt.Errorf("artifacts: mark received: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("artifacts: mark received: %w", err)
	} else if n == 1 {
		return nil
	}
	if _, err := s.GetFile(ctx, versionID, path); err != nil {
		return err
	}
	return ErrConflict
}

// FinalizeVersion implements Store.
func (s *sqlStore) FinalizeVersion(ctx context.Context, artifactID string, seq int, claim time.Time, extra []File) (*Artifact, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("artifacts: begin finalize: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now()
	// Lock the artifact row first, as CreateVersion does.
	res, err := tx.ExecContext(ctx, s.rebind(`UPDATE artifact SET updated_at = ? WHERE id = ? AND deleted_at IS NULL`),
		s.timeArg(now), artifactID)
	if err != nil {
		return nil, fmt.Errorf("artifacts: lock artifact: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return nil, ErrNotFound
	}
	var versionID, state string
	var claimedAt dbTime
	err = tx.QueryRowContext(ctx, s.rebind(`SELECT id, state, claimed_at FROM artifact_version WHERE artifact_id = ? AND seq = ?`),
		artifactID, seq).Scan(&versionID, &state, &claimedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("artifacts: finalize version: %w", err)
	}
	if state != VersionStateFinalizing || !claimedAt.Valid || !claimedAt.Time.Equal(claimToken(claim)) {
		return nil, ErrConflict
	}
	if err := s.insertFiles(ctx, tx, versionID, extra); err != nil {
		return nil, err
	}
	var addBytes int64
	for _, f := range extra {
		addBytes += f.Size
	}
	if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE artifact_version
		SET state = ?, total_bytes = total_bytes + ?, file_count = file_count + ? WHERE id = ?`),
		VersionStateReady, addBytes, len(extra), versionID); err != nil {
		return nil, fmt.Errorf("artifacts: finalize version: %w", err)
	}
	if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE artifact SET current_seq = ?
		WHERE id = ? AND (current_seq IS NULL OR current_seq < ?)`), seq, artifactID, seq); err != nil {
		return nil, fmt.Errorf("artifacts: advance current version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("artifacts: commit finalize: %w", err)
	}
	return s.GetArtifact(ctx, artifactID)
}

// ClaimFinalize implements Store. One conditional update claims the
// version, so of two concurrent finalize requests exactly one succeeds.
func (s *sqlStore) ClaimFinalize(ctx context.Context, artifactID string, seq int, staleBefore time.Time) (time.Time, error) {
	claim := claimToken(time.Now())
	res, err := s.db.ExecContext(ctx, s.rebind(`UPDATE artifact_version SET state = ?, claimed_at = ?
		WHERE artifact_id = ? AND seq = ?
		AND (state = ? OR (state = ? AND claimed_at IS NOT NULL AND claimed_at < ?))
		AND NOT EXISTS (SELECT 1 FROM artifact_file WHERE version_id = artifact_version.id AND received = ?)`),
		VersionStateFinalizing, s.timeArg(claim), artifactID, seq,
		VersionStatePending, VersionStateFinalizing, s.timeArg(staleBefore), false)
	if err != nil {
		return time.Time{}, fmt.Errorf("artifacts: claim finalize: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return time.Time{}, fmt.Errorf("artifacts: claim finalize: %w", err)
	} else if n == 1 {
		return claim, nil
	}
	if _, err := s.GetVersion(ctx, artifactID, seq); err != nil {
		return time.Time{}, err
	}
	return time.Time{}, ErrConflict
}

// claimToken is a claim time in the form both dialects store (UTC,
// microseconds), so the value read back compares equal.
func claimToken(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

// ReleaseFinalize implements Store. It changes the version only while it
// still holds this claim, so it never undoes a later claim.
func (s *sqlStore) ReleaseFinalize(ctx context.Context, artifactID string, seq int, claim time.Time) error {
	if _, err := s.db.ExecContext(ctx, s.rebind(`UPDATE artifact_version SET state = ?, claimed_at = NULL
		WHERE artifact_id = ? AND seq = ? AND state = ? AND claimed_at = ?`),
		VersionStatePending, artifactID, seq, VersionStateFinalizing, s.timeArg(claimToken(claim))); err != nil {
		return fmt.Errorf("artifacts: release finalize: %w", err)
	}
	return nil
}

// ListVersions implements Store.
func (s *sqlStore) ListVersions(ctx context.Context, artifactID string, before, limit int) ([]Version, error) {
	if limit <= 0 {
		return nil, nil
	}
	if before <= 0 {
		before = math.MaxInt32
	}
	rows, err := s.db.QueryContext(ctx, s.rebind(`SELECT `+versionColumns+`
		FROM artifact_version WHERE artifact_id = ? AND state = ? AND seq < ? ORDER BY seq DESC LIMIT `+strconv.Itoa(limit)),
		artifactID, VersionStateReady, before)
	if err != nil {
		return nil, fmt.Errorf("artifacts: list versions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Version
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, fmt.Errorf("artifacts: scan version: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("artifacts: list versions: %w", err)
	}
	return out, nil
}

// ReapPending implements Store.
func (s *sqlStore) ReapPending(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	rows, err := s.db.QueryContext(ctx, s.rebind(`SELECT id, artifact_id FROM artifact_version
		WHERE state IN (?, ?) AND created_at < ? ORDER BY created_at LIMIT `+strconv.Itoa(limit)),
		VersionStatePending, VersionStateFinalizing, s.timeArg(cutoff))
	if err != nil {
		return 0, fmt.Errorf("artifacts: find pending versions: %w", err)
	}
	type stale struct{ id, artifactID string }
	var found []stale
	for rows.Next() {
		var v stale
		if err := rows.Scan(&v.id, &v.artifactID); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("artifacts: scan pending version: %w", err)
		}
		found = append(found, v)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("artifacts: find pending versions: %w", err)
	}
	reaped := 0
	for _, v := range found {
		ok, err := s.reapVersion(ctx, v.id, v.artifactID)
		if err != nil {
			return reaped, err
		}
		if ok {
			reaped++
		}
	}
	return reaped, nil
}

// reapVersion fails one pending version and drops its manifest in a
// transaction, soft-deleting its artifact if nothing else is left. It
// reports false when the version was finalized in the meantime.
func (s *sqlStore) reapVersion(ctx context.Context, versionID, artifactID string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("artifacts: begin reap: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := s.timeArg(time.Now())
	if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE artifact SET updated_at = ? WHERE id = ?`), now, artifactID); err != nil {
		return false, fmt.Errorf("artifacts: lock artifact: %w", err)
	}
	res, err := tx.ExecContext(ctx, s.rebind(`UPDATE artifact_version SET state = ? WHERE id = ? AND state IN (?, ?)`),
		VersionStateFailed, versionID, VersionStatePending, VersionStateFinalizing)
	if err != nil {
		return false, fmt.Errorf("artifacts: fail version: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM artifact_file WHERE version_id = ?`), versionID); err != nil {
		return false, fmt.Errorf("artifacts: drop manifest: %w", err)
	}
	if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE artifact SET deleted_at = ?
		WHERE id = ? AND deleted_at IS NULL AND current_seq IS NULL
		AND NOT EXISTS (SELECT 1 FROM artifact_version WHERE artifact_id = ? AND state IN (?, ?))`),
		now, artifactID, artifactID, VersionStatePending, VersionStateFinalizing); err != nil {
		return false, fmt.Errorf("artifacts: retire empty artifact: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("artifacts: commit reap: %w", err)
	}
	return true, nil
}

// GetArtifact implements Store.
func (s *sqlStore) GetArtifact(ctx context.Context, id string) (*Artifact, error) {
	return s.getArtifact(ctx, "id = ?", id)
}

// getArtifact returns the live artifact matching where.
func (s *sqlStore) getArtifact(ctx context.Context, where string, args ...any) (*Artifact, error) {
	var (
		a                    Artifact
		key                  sql.NullString
		seq                  sql.NullInt64
		expires, created     dbTime
		updated, deletedTime dbTime
	)
	err := s.db.QueryRowContext(ctx, s.rebind(`SELECT
		id, scope_kind, scope_ref, owner_kind, owner_ref, "key", title, current_seq, expires_at, created_at, updated_at, deleted_at
		FROM artifact WHERE `+where+` AND deleted_at IS NULL`), args...).Scan(
		&a.ID, &a.ScopeKind, &a.ScopeRef, &a.OwnerKind, &a.OwnerRef, &key, &a.Title, &seq,
		&expires, &created, &updated, &deletedTime)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("artifacts: get artifact: %w", err)
	}
	a.Key = key.String
	a.CurrentSeq = int(seq.Int64)
	a.ExpiresAt = expires.ptr()
	a.CreatedAt = created.Time
	a.UpdatedAt = updated.Time
	a.DeletedAt = deletedTime.ptr()
	return &a, nil
}

// versionColumns is the artifact_version column list scanVersion reads.
const versionColumns = "id, artifact_id, seq, kind, entry_path, note, total_bytes, file_count, created_by_kind, created_by_ref, created_at, state"

func scanVersion(r rowScanner) (Version, error) {
	var (
		v                   Version
		note, byKind, byRef sql.NullString
		created             dbTime
	)
	err := r.Scan(&v.ID, &v.ArtifactID, &v.Seq, &v.Kind, &v.EntryPath, &note, &v.TotalBytes, &v.FileCount,
		&byKind, &byRef, &created, &v.State)
	v.Note = note.String
	v.CreatedByKind = byKind.String
	v.CreatedByRef = byRef.String
	v.CreatedAt = created.Time
	return v, err
}

// GetVersion implements Store.
func (s *sqlStore) GetVersion(ctx context.Context, artifactID string, seq int) (*Version, error) {
	v, err := scanVersion(s.db.QueryRowContext(ctx, s.rebind(`SELECT `+versionColumns+`
		FROM artifact_version WHERE artifact_id = ? AND seq = ?`), artifactID, seq))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("artifacts: get version: %w", err)
	}
	return &v, nil
}

// ListFiles implements Store.
func (s *sqlStore) ListFiles(ctx context.Context, versionID string) ([]File, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(`SELECT `+fileColumns+`
		FROM artifact_file WHERE version_id = ? ORDER BY path`), versionID)
	if err != nil {
		return nil, fmt.Errorf("artifacts: list files: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []File
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, fmt.Errorf("artifacts: scan file: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("artifacts: list files: %w", err)
	}
	return out, nil
}

// GetFile implements Store.
func (s *sqlStore) GetFile(ctx context.Context, versionID, path string) (*File, error) {
	f, err := scanFile(s.db.QueryRowContext(ctx, s.rebind(`SELECT `+fileColumns+`
		FROM artifact_file WHERE version_id = ? AND path = ?`), versionID, path))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("artifacts: get file: %w", err)
	}
	return &f, nil
}

// fileColumns is the artifact_file column list scanFile reads.
const fileColumns = "version_id, path, size, sha256, media_type, origin, source_url, fetch_status, fetch_error, received"

type rowScanner interface{ Scan(dest ...any) error }

func scanFile(r rowScanner) (File, error) {
	var (
		f                                File
		digest, src, status, fetchErrMsg sql.NullString
	)
	var received bool
	err := r.Scan(&f.VersionID, &f.Path, &f.Size, &digest, &f.MediaType, &f.Origin, &src, &status, &fetchErrMsg, &received)
	f.SHA256, f.SourceURL, f.FetchStatus, f.FetchError = digest.String, src.String, status.String, fetchErrMsg.String
	f.Pending = !received
	return f, err
}

// fileOrigin defaults an empty origin to an upload.
func fileOrigin(o string) string {
	if o == "" {
		return FileOriginUpload
	}
	return o
}

// MaxGrantsForIDs bounds the ids one ListGrantsFor call binds, below every
// driver's placeholder limit.
const MaxGrantsForIDs = 1000

// grantColumns is the artifact_grant column list scanGrant reads.
const grantColumns = "id, artifact_id, subject_kind, subject_ref, permission, expires_at, created_by_ref, created_at"

// ListGrants implements Store.
func (s *sqlStore) ListGrants(ctx context.Context, artifactID string) ([]Grant, error) {
	byID, err := s.queryGrants(ctx, "artifact_id = ?", artifactID)
	return byID[artifactID], err
}

// ListGrantsFor implements Store.
func (s *sqlStore) ListGrantsFor(ctx context.Context, artifactIDs []string) (map[string][]Grant, error) {
	if len(artifactIDs) == 0 {
		return map[string][]Grant{}, nil
	}
	if len(artifactIDs) > MaxGrantsForIDs {
		return nil, fmt.Errorf("artifacts: ListGrantsFor accepts at most %d ids", MaxGrantsForIDs)
	}
	args := make([]any, len(artifactIDs))
	for i, id := range artifactIDs {
		args[i] = id
	}
	return s.queryGrants(ctx, "artifact_id IN ("+strings.TrimSuffix(strings.Repeat("?, ", len(args)), ", ")+")", args...)
}

func (s *sqlStore) queryGrants(ctx context.Context, where string, args ...any) (map[string][]Grant, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind("SELECT "+grantColumns+" FROM artifact_grant WHERE "+where+
		" ORDER BY artifact_id, created_at, id"), args...)
	if err != nil {
		return nil, fmt.Errorf("artifacts: list grants: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string][]Grant{}
	for rows.Next() {
		var (
			g                Grant
			expires, created dbTime
			by               sql.NullString
		)
		if err := rows.Scan(&g.ID, &g.ArtifactID, &g.SubjectKind, &g.SubjectRef, &g.Permission,
			&expires, &by, &created); err != nil {
			return nil, fmt.Errorf("artifacts: scan grant: %w", err)
		}
		g.ExpiresAt = expires.ptr()
		g.CreatedByRef = by.String
		g.CreatedAt = created.Time
		out[g.ArtifactID] = append(out[g.ArtifactID], g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("artifacts: list grants: %w", err)
	}
	return out, nil
}

// maxCandidateScopes bounds the scope refs one ListCandidates query binds,
// well below every driver's placeholder limit.
const maxCandidateScopes = 500

// ListCandidates implements Store.
func (s *sqlStore) ListCandidates(ctx context.Context, q CandidateQuery) ([]Candidate, error) {
	if q.PrincipalKind == "" || q.PrincipalRef == "" {
		return nil, errors.New("artifacts: ListCandidates needs a principal")
	}
	if q.Limit <= 0 {
		return nil, errors.New("artifacts: ListCandidates needs a positive limit")
	}
	if len(q.ScopeRefs) > maxCandidateScopes {
		return nil, fmt.Errorf("artifacts: ListCandidates accepts at most %d scopes", maxCandidateScopes)
	}
	query, args := s.candidateQuery(q)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("artifacts: list candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Candidate
	for rows.Next() {
		var (
			c                         Candidate
			key, kind                 sql.NullString
			seq                       sql.NullInt64
			expires, created, updated dbTime
		)
		if err := rows.Scan(&c.ID, &c.ScopeKind, &c.ScopeRef, &c.OwnerKind, &c.OwnerRef, &key, &c.Title,
			&seq, &expires, &created, &updated, &kind); err != nil {
			return nil, fmt.Errorf("artifacts: scan candidate: %w", err)
		}
		c.Key = key.String
		c.CurrentSeq = int(seq.Int64)
		c.ExpiresAt = expires.ptr()
		c.CreatedAt = created.Time
		c.UpdatedAt = updated.Time
		c.CurrentKind = kind.String
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("artifacts: list candidates: %w", err)
	}
	return out, nil
}

// candidateQuery builds ListCandidates' SQL, rebound for the dialect, and
// its arguments.
func (s *sqlStore) candidateQuery(q CandidateQuery) (string, []any) {
	// The candidates are the union of up to three arms, each served by an
	// index and cut to the page in its own order: artifacts the principal
	// owns (idx_artifact_owner), artifacts with a principal grant to it, and
	// artifacts with a scope grant to one of the scopes (both through
	// idx_artifact_grant_subject). A single OR over the whole table would
	// read every artifact on every page; this way the work follows the
	// caller's own rows. Each arm returns at most Limit distinct rows, so
	// fewer than Limit rows overall means every arm is exhausted.
	var (
		b     strings.Builder
		args  []any
		nArms int
	)
	arm := func(from, where string, whereArgs ...any) {
		nArms++
		if nArms > 1 {
			b.WriteString("\nUNION\n")
		}
		b.WriteString("SELECT * FROM (SELECT DISTINCT " + candidateColumns + " FROM " + from +
			"\n\t\tLEFT JOIN artifact_version v ON v.artifact_id = a.id AND v.seq = a.current_seq\n\t\tWHERE " + where)
		args = append(args, whereArgs...)
		s.writeCandidateFilters(&b, &args, q)
		b.WriteString(" ORDER BY a.updated_at DESC, a.id DESC LIMIT ?) arm" + strconv.Itoa(nArms))
		args = append(args, q.Limit)
	}
	arm("artifact a", "a.owner_kind = ? AND a.owner_ref = ?", q.PrincipalKind, q.PrincipalRef)
	if !q.OwnedOnly {
		now := s.timeArg(q.Now)
		grantWhere := " AND (g.expires_at IS NULL OR g.expires_at > ?) AND g.permission IN (?, ?, ?)"
		grantArgs := []any{now, GrantRead, GrantWrite, GrantAdmin}
		arm("artifact_grant g JOIN artifact a ON a.id = g.artifact_id",
			"g.subject_kind = ? AND g.subject_ref = ?"+grantWhere,
			append([]any{SubjectPrincipal, PrincipalRef(q.PrincipalKind, q.PrincipalRef)}, grantArgs...)...)
		if len(q.ScopeRefs) > 0 {
			in := strings.TrimSuffix(strings.Repeat("?, ", len(q.ScopeRefs)), ", ")
			scopeArgs := []any{SubjectScope}
			for _, ref := range q.ScopeRefs {
				scopeArgs = append(scopeArgs, ref)
			}
			arm("artifact_grant g JOIN artifact a ON a.id = g.artifact_id",
				"g.subject_kind = ? AND g.subject_ref IN ("+in+")"+grantWhere, append(scopeArgs, grantArgs...)...)
		}
	}
	b.WriteString("\nORDER BY updated_at DESC, id DESC LIMIT ?")
	args = append(args, q.Limit)

	return s.rebind(b.String()), args
}

// candidateColumns is the column list every arm of the candidate query
// selects; the union and its outer ORDER BY rely on the names.
const candidateColumns = `a.id, a.scope_kind, a.scope_ref, a.owner_kind, a.owner_ref, a."key", a.title,
		a.current_seq, a.expires_at, a.created_at, a.updated_at, v.kind AS current_kind`

// writeCandidateFilters appends the conditions every candidate arm shares:
// live, not expired, the search, the review filter, the home scope and the
// keyset position.
//
// Search folds case the way the database folds it, so a pattern and a
// column are always compared under the same rules: SQLite's LIKE ignores
// case for ASCII letters only and compares other characters exactly;
// Postgres lowercases both sides with LOWER, under the database's own
// collation rules.
func (s *sqlStore) writeCandidateFilters(b *strings.Builder, args *[]any, q CandidateQuery) {
	now := s.timeArg(q.Now)
	b.WriteString(" AND a.deleted_at IS NULL AND (a.expires_at IS NULL OR a.expires_at > ?)")
	*args = append(*args, now)
	if q.Search != "" {
		pattern := "%" + escapeLike(q.Search) + "%"
		if s.dialect == dialectPostgres {
			b.WriteString(` AND (LOWER(a.title) LIKE LOWER(?) ESCAPE '\' OR LOWER(COALESCE(a."key", '')) LIKE LOWER(?) ESCAPE '\')`)
		} else {
			b.WriteString(` AND (a.title LIKE ? ESCAPE '\' OR COALESCE(a."key", '') LIKE ? ESCAPE '\')`)
		}
		*args = append(*args, pattern, pattern)
	}
	if q.ReviewPending {
		b.WriteString(" AND v.kind = ?")
		*args = append(*args, VersionKindReview)
	}
	if q.HomeScope != "" {
		b.WriteString(" AND a.scope_kind = ? AND a.scope_ref = ?")
		*args = append(*args, ScopeKindProject, q.HomeScope)
	}
	if q.After != nil {
		at := s.timeArg(q.After.UpdatedAt)
		b.WriteString(" AND (a.updated_at < ? OR (a.updated_at = ? AND a.id < ?))")
		*args = append(*args, at, at, q.After.ID)
	}
}

// escapeLike escapes the LIKE wildcards and the escape character itself,
// so a search matches literally under ESCAPE '\'.
func escapeLike(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(v)
}

// AddMessageRefs implements Store.
func (s *sqlStore) AddMessageRefs(ctx context.Context, messageID string, refs []MessageRef) error {
	if messageID == "" || len(refs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("artifacts: begin message refs: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, r := range refs {
		if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO artifact_message_ref
			(message_id, artifact_id, seq) VALUES (?, ?, ?)
			ON CONFLICT (message_id, artifact_id) DO NOTHING`),
			messageID, r.ArtifactID, nullInt(r.Seq)); err != nil {
			return fmt.Errorf("artifacts: insert message ref: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("artifacts: commit message refs: %w", err)
	}
	return nil
}

// maxMessageRefQueryIDs bounds the message ids in one ListMessageRefs
// query, well under SQLite's bound-parameter limit.
const maxMessageRefQueryIDs = 500

// ListMessageRefs implements Store.
func (s *sqlStore) ListMessageRefs(ctx context.Context, messageIDs []string) (map[string][]MessageRef, error) {
	out := make(map[string][]MessageRef)
	for start := 0; start < len(messageIDs); start += maxMessageRefQueryIDs {
		end := min(start+maxMessageRefQueryIDs, len(messageIDs))
		if err := s.listMessageRefsChunk(ctx, messageIDs[start:end], out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// listMessageRefsChunk adds the references of one chunk of message ids to
// out, in one query.
func (s *sqlStore) listMessageRefsChunk(ctx context.Context, chunk []string, out map[string][]MessageRef) error {
	args := make([]any, len(chunk))
	for i, id := range chunk {
		args[i] = id
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(chunk)), ", ")
	rows, err := s.db.QueryContext(ctx, s.rebind(`SELECT message_id, artifact_id, seq
		FROM artifact_message_ref WHERE message_id IN (`+placeholders+`)
		ORDER BY message_id, artifact_id`), args...)
	if err != nil {
		return fmt.Errorf("artifacts: list message refs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			msgID string
			r     MessageRef
			seq   sql.NullInt64
		)
		if err := rows.Scan(&msgID, &r.ArtifactID, &seq); err != nil {
			return fmt.Errorf("artifacts: scan message ref: %w", err)
		}
		r.Seq = int(seq.Int64)
		out[msgID] = append(out[msgID], r)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("artifacts: list message refs: %w", err)
	}
	return nil
}
