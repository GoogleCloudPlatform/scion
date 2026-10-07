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
	if a == nil || v == nil {
		return errors.New("artifacts: CreatePublished needs an artifact and a version")
	}
	if v.ArtifactID != a.ID {
		return errors.New("artifacts: version does not belong to the artifact")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("artifacts: begin publish: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO artifact
		(id, scope_kind, scope_ref, owner_kind, owner_ref, "key", title, current_seq, expires_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		a.ID, a.ScopeKind, a.ScopeRef, a.OwnerKind, a.OwnerRef, nullString(a.Key), a.Title, nullInt(v.Seq),
		s.nullTimeArg(a.ExpiresAt), s.timeArg(a.CreatedAt), s.timeArg(a.UpdatedAt)); err != nil {
		return fmt.Errorf("artifacts: insert artifact: %w", err)
	}
	if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO artifact_version
		(id, artifact_id, seq, kind, entry_path, note, total_bytes, file_count, created_by_kind, created_by_ref, created_at, state)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		v.ID, v.ArtifactID, v.Seq, v.Kind, v.EntryPath, nullString(v.Note), v.TotalBytes, v.FileCount,
		nullString(v.CreatedByKind), nullString(v.CreatedByRef), s.timeArg(v.CreatedAt), v.State); err != nil {
		return fmt.Errorf("artifacts: insert version: %w", err)
	}
	for _, f := range files {
		if f.VersionID != v.ID {
			return errors.New("artifacts: file does not belong to the version")
		}
		if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO artifact_file
			(version_id, path, size, sha256, media_type, origin, source_url, fetch_status, fetch_error)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			f.VersionID, f.Path, f.Size, nullString(f.SHA256), f.MediaType, fileOrigin(f.Origin),
			nullString(f.SourceURL), nullString(f.FetchStatus), nullString(f.FetchError)); err != nil {
			return fmt.Errorf("artifacts: insert file: %w", err)
		}
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
	return nil
}

// GetArtifact implements Store.
func (s *sqlStore) GetArtifact(ctx context.Context, id string) (*Artifact, error) {
	var (
		a                    Artifact
		key                  sql.NullString
		seq                  sql.NullInt64
		expires, created     dbTime
		updated, deletedTime dbTime
	)
	err := s.db.QueryRowContext(ctx, s.rebind(`SELECT
		id, scope_kind, scope_ref, owner_kind, owner_ref, "key", title, current_seq, expires_at, created_at, updated_at, deleted_at
		FROM artifact WHERE id = ? AND deleted_at IS NULL`), id).Scan(
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

// GetVersion implements Store.
func (s *sqlStore) GetVersion(ctx context.Context, artifactID string, seq int) (*Version, error) {
	var (
		v                   Version
		note, byKind, byRef sql.NullString
		created             dbTime
	)
	err := s.db.QueryRowContext(ctx, s.rebind(`SELECT
		id, artifact_id, seq, kind, entry_path, note, total_bytes, file_count, created_by_kind, created_by_ref, created_at, state
		FROM artifact_version WHERE artifact_id = ? AND seq = ?`), artifactID, seq).Scan(
		&v.ID, &v.ArtifactID, &v.Seq, &v.Kind, &v.EntryPath, &note, &v.TotalBytes, &v.FileCount,
		&byKind, &byRef, &created, &v.State)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("artifacts: get version: %w", err)
	}
	v.Note = note.String
	v.CreatedByKind = byKind.String
	v.CreatedByRef = byRef.String
	v.CreatedAt = created.Time
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
const fileColumns = "version_id, path, size, sha256, media_type, origin, source_url, fetch_status, fetch_error"

type rowScanner interface{ Scan(dest ...any) error }

func scanFile(r rowScanner) (File, error) {
	var (
		f                                File
		digest, src, status, fetchErrMsg sql.NullString
	)
	err := r.Scan(&f.VersionID, &f.Path, &f.Size, &digest, &f.MediaType, &f.Origin, &src, &status, &fetchErrMsg)
	f.SHA256, f.SourceURL, f.FetchStatus, f.FetchError = digest.String, src.String, status.String, fetchErrMsg.String
	return f, err
}

// fileOrigin defaults an empty origin to an upload.
func fileOrigin(o string) string {
	if o == "" {
		return FileOriginUpload
	}
	return o
}

// ListGrants implements Store.
func (s *sqlStore) ListGrants(ctx context.Context, artifactID string) ([]Grant, error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(`SELECT
		id, artifact_id, subject_kind, subject_ref, permission, expires_at, created_by_ref, created_at
		FROM artifact_grant WHERE artifact_id = ? ORDER BY created_at, id`), artifactID)
	if err != nil {
		return nil, fmt.Errorf("artifacts: list grants: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Grant
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
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("artifacts: list grants: %w", err)
	}
	return out, nil
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
		chunk := messageIDs[start:end]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(chunk)), ", ")
		rows, err := s.db.QueryContext(ctx, s.rebind(`SELECT message_id, artifact_id, seq
			FROM artifact_message_ref WHERE message_id IN (`+placeholders+`)
			ORDER BY message_id, artifact_id`), args...)
		if err != nil {
			return nil, fmt.Errorf("artifacts: list message refs: %w", err)
		}
		for rows.Next() {
			var (
				msgID string
				r     MessageRef
				seq   sql.NullInt64
			)
			if err := rows.Scan(&msgID, &r.ArtifactID, &seq); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("artifacts: scan message ref: %w", err)
			}
			r.Seq = int(seq.Int64)
			out[msgID] = append(out[msgID], r)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, fmt.Errorf("artifacts: list message refs: %w", err)
		}
	}
	return out, nil
}
