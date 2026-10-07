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
// live, not expired, the search, the review filter and the keyset position.
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
