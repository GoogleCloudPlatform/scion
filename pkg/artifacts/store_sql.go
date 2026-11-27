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

// migration is one named step. Steps run in order, each at most once per
// database, and are recorded in artifact_migrations. A step runs its
// dialect's DDL, when there is any, and then apply, when set; both run in
// Init's transaction, so a failure leaves neither the change nor the ledger
// row.
//
// apply may return errMigrationDeferred to put its step off: Init then
// records no ledger row for it, keeps going with the later steps and
// succeeds, and the step runs again at the next start. A deferring apply
// must return before it writes anything, so it leaves nothing behind in
// the shared transaction. Only a data step whose effect no later step
// depends on may defer (no schema change); 0014_review_ladder qualifies,
// since it only rewrites grant permissions.
type migration struct {
	name     string
	sqlite   string
	postgres string
	apply    func(ctx context.Context, tx *sql.Tx, s *sqlStore) error
}

// errMigrationDeferred is returned by a migration's apply to put its step
// off until the next start (see migration).
var errMigrationDeferred = errors.New("artifacts: migration deferred")

// migrations is the ordered schema history. Append new steps; never edit or
// reorder applied ones.
var migrations = []migration{
	{name: migrationInitial, sqlite: sqliteSchema, postgres: postgresSchema},
	{name: migrationRemoteFiles, sqlite: sqliteRemoteFiles, postgres: postgresRemoteFiles},
	{name: migrationVersionUploads, sqlite: sqliteVersionUploads, postgres: postgresVersionUploads},
	{name: migrationFinalizeClaims, sqlite: sqliteFinalizeClaims, postgres: postgresFinalizeClaims},
	{name: migrationLinkTokens, sqlite: sqliteLinkTokens, postgres: postgresLinkTokens},
	{name: migrationBlobGC, sqlite: sqliteBlobGC, postgres: postgresBlobGC},
	{name: migrationOrphans, sqlite: sqliteOrphans, postgres: postgresOrphans},
	{name: migrationVersionFailReason, sqlite: sqliteVersionFailReason, postgres: postgresVersionFailReason},
	{name: migrationAttachments, sqlite: sqliteAttachments, postgres: postgresAttachments},
	{name: migrationAttachmentProject, sqlite: sqliteAttachmentProject, postgres: postgresAttachmentProject},
	{name: migrationDeliveryRedemptions, sqlite: sqliteDeliveryRedemptions, postgres: postgresDeliveryRedemptions},
	{name: migrationReviewSessions, sqlite: sqliteReviewSessions, postgres: postgresReviewSessions},
	{name: migrationReviewLadder, apply: applyReviewLadder},
	{name: migrationReviewDrafts, sqlite: sqliteReviewDrafts, postgres: postgresReviewDrafts},
	{name: migrationReviewRequests, sqlite: sqliteReviewRequests, postgres: postgresReviewRequests},
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
	// beforeOrphanUpdate, when non-nil (tests only), runs in setOwnerOrphaned
	// after selecting candidate artifact IDs and before the per-row UPDATE loop,
	// so tests can simulate a concurrent ownership change in that window.
	beforeOrphanUpdate func(tx *sql.Tx) error
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
		if ddl != "" {
			if _, err := tx.ExecContext(ctx, ddl); err != nil {
				return fmt.Errorf("artifacts: migration %s: %w", m.name, err)
			}
		}
		if m.apply != nil {
			if err := m.apply(ctx, tx, s); errors.Is(err, errMigrationDeferred) {
				continue
			} else if err != nil {
				return fmt.Errorf("artifacts: migration %s: %w", m.name, err)
			}
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
	err := s.createArtifact(ctx, a, v, files, grants)
	if err != nil && a != nil && a.Key != "" {
		if _, lookErr := s.GetArtifactByKey(ctx, a.ScopeKind, a.ScopeRef, a.OwnerKind, a.OwnerRef, a.Key); lookErr == nil {
			return ErrConflict
		}
	}
	return err
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
// is the version when it is ready, and none otherwise. A grant created by
// SponsorReviewCreator gets its grant_created event in the same
// transaction.
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
		(id, scope_kind, scope_ref, owner_kind, owner_ref, "key", title, current_seq, expires_at, created_at, updated_at, sponsor_ref)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		a.ID, a.ScopeKind, a.ScopeRef, a.OwnerKind, a.OwnerRef, nullString(a.Key), a.Title, nullInt(current),
		s.nullTimeArg(a.ExpiresAt), s.timeArg(a.CreatedAt), s.timeArg(a.UpdatedAt), nullString(a.SponsorRef)); err != nil {
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
		// The automatic review grant (ptone/scion#7134) is the hub's own
		// act, recorded with its event in this transaction; the home grant
		// is implicit and records none.
		if g.CreatedByRef == SponsorReviewCreator {
			if !validSponsorReviewGrant(&g) {
				return errors.New("artifacts: an automatic review grant must be a review grant to a user")
			}
			if err := s.insertEvent(ctx, tx, a.ID, EventGrantCreated, &sponsorReviewActor, g.CreatedAt, map[string]any{
				"grantId": g.ID, "subjectKind": g.SubjectKind, "subjectRef": g.SubjectRef, "permission": g.Permission,
			}); err != nil {
				return err
			}
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
		(id, artifact_id, seq, kind, entry_path, note, total_bytes, file_count, created_by_kind, created_by_ref, created_at, state, source_attachment_id, source_message_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		v.ID, v.ArtifactID, v.Seq, v.Kind, v.EntryPath, nullString(v.Note), v.TotalBytes, v.FileCount,
		nullString(v.CreatedByKind), nullString(v.CreatedByRef), s.timeArg(v.CreatedAt), v.State,
		nullString(v.SourceAttachmentID), nullString(v.SourceMessageID)); err != nil {
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
	if err := s.refuseAttachmentTx(ctx, tx, v.ArtifactID); err != nil {
		return err
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

// AppendPublishedVersion implements Store.
func (s *sqlStore) AppendPublishedVersion(ctx context.Context, v *Version, files []File) (*Artifact, error) {
	if v == nil || v.State != VersionStateReady {
		return nil, errors.New("artifacts: AppendPublishedVersion needs a ready version")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("artifacts: begin append published version: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, s.rebind(`UPDATE artifact SET updated_at = ? WHERE id = ? AND deleted_at IS NULL`),
		s.timeArg(v.CreatedAt), v.ArtifactID)
	if err != nil {
		return nil, fmt.Errorf("artifacts: lock artifact: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return nil, ErrNotFound
	}
	if err := s.refuseAttachmentTx(ctx, tx, v.ArtifactID); err != nil {
		return nil, err
	}
	var maxSeq int
	if err := tx.QueryRowContext(ctx, s.rebind(`SELECT COALESCE(MAX(seq), 0)
		FROM artifact_version WHERE artifact_id = ?`), v.ArtifactID).Scan(&maxSeq); err != nil {
		return nil, fmt.Errorf("artifacts: next seq: %w", err)
	}
	v.Seq = maxSeq + 1
	if err := s.insertVersion(ctx, tx, v, files); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE artifact SET current_seq = ?
		WHERE id = ? AND (current_seq IS NULL OR current_seq < ?)`), v.Seq, v.ArtifactID, v.Seq); err != nil {
		return nil, fmt.Errorf("artifacts: advance current version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("artifacts: commit append published version: %w", err)
	}
	return s.GetArtifact(ctx, v.ArtifactID)
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
func (s *sqlStore) MarkReceived(ctx context.Context, versionID, path, mediaType string, siblings map[string]string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("artifacts: begin mark received: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, s.rebind(`UPDATE artifact_file SET received = ?, media_type = ?
		WHERE version_id = ? AND path = ? AND origin = ?
		AND EXISTS (SELECT 1 FROM artifact_version WHERE id = ? AND state = ?)`),
		true, mediaType, versionID, path, FileOriginUpload, versionID, VersionStatePending)
	if err != nil {
		return fmt.Errorf("artifacts: mark received: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("artifacts: mark received: %w", err)
	}
	if n != 1 {
		_ = tx.Rollback()
		if _, err := s.GetFile(ctx, versionID, path); err != nil {
			return err
		}
		return ErrConflict
	}
	// Files of the same version with the same digest share the one stored
	// object, so they arrive together (one upload per digest), each with
	// the media type the caller detected for its own path.
	for p, mt := range siblings {
		if p == path {
			continue
		}
		if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE artifact_file SET received = ?, media_type = ?
			WHERE version_id = ? AND path = ? AND origin = ? AND received = ?
			AND sha256 = (SELECT sha256 FROM artifact_file WHERE version_id = ? AND path = ?)`),
			true, mt, versionID, p, FileOriginUpload, false, versionID, path); err != nil {
			return fmt.Errorf("artifacts: mark same-digest file received: %w", err)
		}
	}
	return commit(tx)
}

// FinalizeVersion implements Store.
func (s *sqlStore) FinalizeVersion(ctx context.Context, artifactID string, seq int, claim time.Time, extra []File, base int) (*Artifact, error) {
	a, _, err := s.FinalizeVersionWithEffects(ctx, artifactID, seq, claim, extra, base)
	return a, err
}

// FinalizeVersionWithEffects implements Store.
func (s *sqlStore) FinalizeVersionWithEffects(ctx context.Context, artifactID string, seq int, claim time.Time, extra []File, base int) (*Artifact, FinalizeEffects, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, FinalizeEffects{}, fmt.Errorf("artifacts: begin finalize: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC().Truncate(time.Microsecond)
	// Lock the artifact row first, as CreateVersion does.
	res, err := tx.ExecContext(ctx, s.rebind(`UPDATE artifact SET updated_at = ? WHERE id = ? AND deleted_at IS NULL`),
		s.timeArg(now), artifactID)
	if err != nil {
		return nil, FinalizeEffects{}, fmt.Errorf("artifacts: lock artifact: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return nil, FinalizeEffects{}, ErrNotFound
	}
	var versionID, state, kind string
	var createdByKind, createdByRef sql.NullString
	var claimedAt dbTime
	err = tx.QueryRowContext(ctx, s.rebind(`SELECT id, state, kind, created_by_kind, created_by_ref, claimed_at FROM artifact_version WHERE artifact_id = ? AND seq = ?`),
		artifactID, seq).Scan(&versionID, &state, &kind, &createdByKind, &createdByRef, &claimedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, FinalizeEffects{}, ErrNotFound
	}
	if err != nil {
		return nil, FinalizeEffects{}, fmt.Errorf("artifacts: finalize version: %w", err)
	}
	if state != VersionStateFinalizing || !claimedAt.Valid || !claimedAt.Time.Equal(claimToken(claim)) {
		return nil, FinalizeEffects{}, ErrConflict
	}
	if base > 0 {
		if base >= seq {
			return nil, FinalizeEffects{}, ErrStaleBase
		}
		var current sql.NullInt64
		if err := tx.QueryRowContext(ctx, s.rebind(`SELECT current_seq FROM artifact WHERE id = ?`), artifactID).Scan(&current); err != nil {
			return nil, FinalizeEffects{}, fmt.Errorf("artifacts: read current version: %w", err)
		}
		if !current.Valid || int(current.Int64) != base {
			return nil, FinalizeEffects{}, ErrStaleBase
		}
		var between int
		if err := tx.QueryRowContext(ctx, s.rebind(`SELECT COUNT(*) FROM artifact_version
			WHERE artifact_id = ? AND seq > ? AND seq < ? AND kind = ? AND state IN (?, ?)`),
			artifactID, base, seq, VersionKindPublish, VersionStatePending, VersionStateFinalizing).Scan(&between); err != nil {
			return nil, FinalizeEffects{}, fmt.Errorf("artifacts: read pending versions: %w", err)
		}
		if between > 0 {
			return nil, FinalizeEffects{}, ErrPendingNewer
		}
	}
	if err := s.insertFiles(ctx, tx, versionID, extra); err != nil {
		return nil, FinalizeEffects{}, err
	}
	var addBytes int64
	for _, f := range extra {
		addBytes += f.Size
	}
	if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE artifact_version
		SET state = ?, total_bytes = total_bytes + ?, file_count = file_count + ? WHERE id = ?`),
		VersionStateReady, addBytes, len(extra), versionID); err != nil {
		return nil, FinalizeEffects{}, fmt.Errorf("artifacts: finalize version: %w", err)
	}
	if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE artifact SET current_seq = ?
		WHERE id = ? AND (current_seq IS NULL OR current_seq < ?)`), seq, artifactID, seq); err != nil {
		return nil, FinalizeEffects{}, fmt.Errorf("artifacts: advance current version: %w", err)
	}

	var effects FinalizeEffects
	// Design §4.6 writer 4 and G4 (ungated): finalizing a kind: review version
	// atomically fulfils the finalizing reviewer's open review request (if any)
	// and deletes that reviewer's saved draft (if any) in the same transaction.
	// Publishing (kind: publish) writes nothing to artifact_review_request
	// (carry-forward, writer 5).
	if kind == VersionKindReview && createdByKind.String != "" && createdByRef.String != "" {
		cbKind, cbRef := createdByKind.String, createdByRef.String
		var openReqID string
		qerr := tx.QueryRowContext(ctx, s.rebind(`SELECT id FROM artifact_review_request
			WHERE artifact_id = ? AND reviewer_kind = ? AND reviewer_ref = ? AND state = ?`),
			artifactID, cbKind, cbRef, ReviewRequestStateOpen).Scan(&openReqID)
		if qerr != nil && !errors.Is(qerr, sql.ErrNoRows) {
			return nil, FinalizeEffects{}, fmt.Errorf("artifacts: query open review request on finalize: %w", qerr)
		}
		if qerr == nil && openReqID != "" {
			fres, ferr := tx.ExecContext(ctx, s.rebind(`UPDATE artifact_review_request
				SET state = ?, resolved_seq = ?, resolved_by_kind = ?, resolved_by_ref = ?,
				    resolved_at = ?, updated_at = ?
				WHERE id = ? AND state = ?`),
				ReviewRequestStateFulfilled, seq, cbKind, cbRef,
				s.timeArg(now), s.timeArg(now), openReqID, ReviewRequestStateOpen)
			if ferr != nil {
				return nil, FinalizeEffects{}, fmt.Errorf("artifacts: fulfil review request on finalize: %w", ferr)
			}
			if fn, ferr := fres.RowsAffected(); ferr != nil {
				return nil, FinalizeEffects{}, fmt.Errorf("artifacts: fulfil review request on finalize: %w", ferr)
			} else if fn == 1 {
				effects.FulfilledRequestID = openReqID
				revActor := &Actor{Kind: cbKind, Ref: cbRef, Authority: AuthorityReviewer}
				detail := map[string]any{
					"requestId":    openReqID,
					"reviewerKind": cbKind,
					"reviewerRef":  cbRef,
					"resolvedSeq":  seq,
				}
				if err := s.insertEvent(ctx, tx, artifactID, EventReviewRequestFulfilled, revActor, now, detail); err != nil {
					return nil, FinalizeEffects{}, err
				}
			}
		}

		dres, derr := tx.ExecContext(ctx, s.rebind(`DELETE FROM artifact_review_draft
			WHERE artifact_id = ? AND principal_kind = ? AND principal_ref = ?`),
			artifactID, cbKind, cbRef)
		if derr != nil {
			return nil, FinalizeEffects{}, fmt.Errorf("artifacts: delete review draft on finalize: %w", derr)
		}
		if dn, derr := dres.RowsAffected(); derr != nil {
			return nil, FinalizeEffects{}, fmt.Errorf("artifacts: delete review draft on finalize: %w", derr)
		} else if dn > 0 {
			effects.DraftDeleted = true
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, FinalizeEffects{}, fmt.Errorf("artifacts: commit finalize: %w", err)
	}
	a, err := s.GetArtifact(ctx, artifactID)
	return a, effects, err
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
	return s.listVersionsIn(ctx, artifactID, before, limit, VersionStateReady)
}

// ListVersionHistory implements Store.
func (s *sqlStore) ListVersionHistory(ctx context.Context, artifactID string, before, limit int) ([]Version, error) {
	return s.listVersionsIn(ctx, artifactID, before, limit, VersionStateReady, VersionStateFailed)
}

// listVersionsIn returns up to limit versions of an artifact in one of
// states, newest first, with a seq below before (0 = from the newest).
func (s *sqlStore) listVersionsIn(ctx context.Context, artifactID string, before, limit int, states ...string) ([]Version, error) {
	if limit <= 0 || len(states) == 0 {
		return nil, nil
	}
	if before <= 0 {
		before = math.MaxInt32
	}
	marks := strings.TrimSuffix(strings.Repeat("?, ", len(states)), ", ")
	args := []any{artifactID}
	for _, st := range states {
		args = append(args, st)
	}
	args = append(args, before)
	rows, err := s.db.QueryContext(ctx, s.rebind(`SELECT `+versionColumns+`
		FROM artifact_version WHERE artifact_id = ? AND state IN (`+marks+`) AND seq < ? ORDER BY seq DESC LIMIT `+strconv.Itoa(limit)),
		args...)
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

// DiscardFinalize implements Store.
func (s *sqlStore) DiscardFinalize(ctx context.Context, artifactID string, seq int, claim time.Time, reason string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("artifacts: begin discard: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// Lock the artifact row first, as FinalizeVersion does.
	if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE artifact SET updated_at = ? WHERE id = ?`), s.timeArg(time.Now()), artifactID); err != nil {
		return fmt.Errorf("artifacts: lock artifact: %w", err)
	}
	var versionID string
	err = tx.QueryRowContext(ctx, s.rebind(`SELECT id FROM artifact_version
		WHERE artifact_id = ? AND seq = ? AND state = ? AND claimed_at = ?`),
		artifactID, seq, VersionStateFinalizing, s.timeArg(claimToken(claim))).Scan(&versionID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrConflict
	}
	if err != nil {
		return fmt.Errorf("artifacts: discard version: %w", err)
	}
	if _, err := tx.ExecContext(ctx, s.rebind(`UPDATE artifact_version SET state = ?, claimed_at = NULL, fail_reason = ? WHERE id = ?`),
		VersionStateFailed, nullString(reason), versionID); err != nil {
		return fmt.Errorf("artifacts: fail version: %w", err)
	}
	if _, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM artifact_file WHERE version_id = ?`), versionID); err != nil {
		return fmt.Errorf("artifacts: drop manifest: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("artifacts: commit discard: %w", err)
	}
	return nil
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
	res, err := tx.ExecContext(ctx, s.rebind(`UPDATE artifact_version SET state = ?, fail_reason = ? WHERE id = ? AND state IN (?, ?)`),
		VersionStateFailed, FailReasonExpired, versionID, VersionStatePending, VersionStateFinalizing)
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

// GetArtifact implements Store. A row of an attachment scope is never
// returned (GetAttachment is its lookup), so every artifact route, message
// reference and recipient check answers for it as for an unknown id.
func (s *sqlStore) GetArtifact(ctx context.Context, id string) (*Artifact, error) {
	return s.getArtifact(ctx, "id = ? AND "+notAttachmentScope("scope_kind"), id)
}

// getArtifact returns the live artifact matching where.
// artifactColumns is the artifact column list scanArtifact reads.
const artifactColumns = `id, scope_kind, scope_ref, owner_kind, owner_ref, "key", title, current_seq, expires_at, created_at, updated_at, deleted_at,
		orphaned_at, message_project_id, sponsor_ref, former_owner_kind, former_owner_ref, transferred_at, former_key
		`

func scanArtifact(r rowScanner) (Artifact, error) {
	var (
		a                              Artifact
		key                            sql.NullString
		seq                            sql.NullInt64
		expires, created               dbTime
		updated, deletedTime           dbTime
		orphaned, transferred          dbTime
		messageProject                 sql.NullString
		sponsor                        sql.NullString
		formerKind, formerRef, prevKey sql.NullString
	)
	err := r.Scan(
		&a.ID, &a.ScopeKind, &a.ScopeRef, &a.OwnerKind, &a.OwnerRef, &key, &a.Title, &seq,
		&expires, &created, &updated, &deletedTime, &orphaned, &messageProject, &sponsor,
		&formerKind, &formerRef, &transferred, &prevKey)
	if err != nil {
		return Artifact{}, err
	}
	a.Key = key.String
	a.CurrentSeq = int(seq.Int64)
	a.ExpiresAt = expires.ptr()
	a.CreatedAt = created.Time
	a.UpdatedAt = updated.Time
	a.DeletedAt = deletedTime.ptr()
	a.OrphanedAt = orphaned.ptr()
	a.MessageProjectID = nullStringPtr(messageProject)
	a.SponsorRef = sponsor.String
	a.FormerOwnerKind = formerKind.String
	a.FormerOwnerRef = formerRef.String
	a.TransferredAt = transferred.ptr()
	a.FormerKey = prevKey.String
	return a, nil
}

func (s *sqlStore) getArtifact(ctx context.Context, where string, args ...any) (*Artifact, error) {
	a, err := scanArtifact(s.db.QueryRowContext(ctx, s.rebind(`SELECT `+artifactColumns+`
		FROM artifact WHERE `+where+` AND deleted_at IS NULL`), args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("artifacts: get artifact: %w", err)
	}
	return &a, nil
}

// GetArtifacts implements Store.
func (s *sqlStore) GetArtifacts(ctx context.Context, ids []string) (map[string]*Artifact, error) {
	if len(ids) == 0 {
		return map[string]*Artifact{}, nil
	}
	if len(ids) > MaxGrantsForIDs {
		return nil, fmt.Errorf("artifacts: GetArtifacts accepts at most %d ids", MaxGrantsForIDs)
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx, s.rebind(`SELECT `+artifactColumns+`
		FROM artifact WHERE id IN (`+strings.TrimSuffix(strings.Repeat("?, ", len(args)), ", ")+`) AND deleted_at IS NULL`), args...)
	if err != nil {
		return nil, fmt.Errorf("artifacts: get artifacts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]*Artifact, len(ids))
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, fmt.Errorf("artifacts: scan artifact: %w", err)
		}
		aCopy := a
		out[aCopy.ID] = &aCopy
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("artifacts: get artifacts: %w", err)
	}
	return out, nil
}

// versionColumns is the artifact_version column list scanVersion reads.
const versionColumns = "id, artifact_id, seq, kind, entry_path, note, total_bytes, file_count, created_by_kind, created_by_ref, created_at, state, fail_reason, source_attachment_id, source_message_id"

func scanVersion(r rowScanner) (Version, error) {
	var (
		v                                           Version
		note, byKind, byRef, reason, srcAtt, srcMsg sql.NullString
		created                                     dbTime
	)
	err := r.Scan(&v.ID, &v.ArtifactID, &v.Seq, &v.Kind, &v.EntryPath, &note, &v.TotalBytes, &v.FileCount,
		&byKind, &byRef, &created, &v.State, &reason, &srcAtt, &srcMsg)
	v.FailReason = reason.String
	v.Note = note.String
	v.CreatedByKind = byKind.String
	v.CreatedByRef = byRef.String
	v.CreatedAt = created.Time
	v.SourceAttachmentID = srcAtt.String
	v.SourceMessageID = srcMsg.String
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
	query, args, err := s.candidateQuery(q)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("artifacts: list candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Candidate
	for rows.Next() {
		var (
			c                              Candidate
			key, kind, sponsor             sql.NullString
			formerKind, formerRef, prevKey sql.NullString
			seq                            sql.NullInt64
			expires, created, updated      dbTime
			orphaned, transferred          dbTime
			requestedByMe, myDraft         bool
		)
		if err := rows.Scan(&c.ID, &c.ScopeKind, &c.ScopeRef, &c.OwnerKind, &c.OwnerRef, &key, &c.Title,
			&seq, &expires, &created, &updated, &orphaned, &sponsor, &formerKind, &formerRef, &transferred, &prevKey, &kind, &requestedByMe, &myDraft); err != nil {
			return nil, fmt.Errorf("artifacts: scan candidate: %w", err)
		}
		c.Key = key.String
		c.CurrentSeq = int(seq.Int64)
		c.ExpiresAt = expires.ptr()
		c.CreatedAt = created.Time
		c.UpdatedAt = updated.Time
		c.OrphanedAt = orphaned.ptr()
		c.SponsorRef = sponsor.String
		c.FormerOwnerKind = formerKind.String
		c.FormerOwnerRef = formerRef.String
		c.TransferredAt = transferred.ptr()
		c.FormerKey = prevKey.String
		c.CurrentKind = kind.String
		c.RequestedByMe = requestedByMe
		c.MyDraft = myDraft
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("artifacts: list candidates: %w", err)
	}
	return out, nil
}

// candidateQuery builds ListCandidates' SQL, rebound for the dialect, and
// its arguments.
func (s *sqlStore) candidateQuery(q CandidateQuery) (string, []any, error) {
	// The candidates are the union of up to three arms, each served by an
	// index and cut to the page in its own order: artifacts the principal
	// owns (idx_artifact_owner), artifacts with a principal grant to it, and
	// artifacts with a scope grant to one of the scopes (both through
	// idx_artifact_grant_subject). A single OR over the whole table would
	// read every artifact on every page; this way the work follows the
	// caller's own rows. Each arm returns at most Limit distinct rows, so
	// fewer than Limit rows overall means every arm is exhausted.
	var (
		b       strings.Builder
		args    []any
		nArms   int
		filtErr error
	)
	selectCols := candidateColumns
	var selectArgs []any
	if q.ReviewRequestsEnabled {
		selectCols += `, EXISTS (SELECT 1 FROM artifact_review_request r WHERE r.artifact_id = a.id AND r.reviewer_kind = ? AND r.reviewer_ref = ? AND r.state = ?) AS requested_by_me`
		selectArgs = append(selectArgs, q.PrincipalKind, q.PrincipalRef, ReviewRequestStateOpen)
	} else {
		selectCols += `, FALSE AS requested_by_me`
	}
	if q.ReviewDraftsEnabled {
		selectCols += `, EXISTS (SELECT 1 FROM artifact_review_draft d WHERE d.artifact_id = a.id AND d.principal_kind = ? AND d.principal_ref = ?) AS my_draft`
		selectArgs = append(selectArgs, q.PrincipalKind, q.PrincipalRef)
	} else {
		selectCols += `, FALSE AS my_draft`
	}
	arm := func(from, where string, whereArgs ...any) {
		if filtErr != nil {
			return
		}
		nArms++
		if nArms > 1 {
			b.WriteString("\nUNION\n")
		}
		b.WriteString("SELECT * FROM (SELECT DISTINCT " + selectCols + " FROM " + from +
			"\n\t\tLEFT JOIN artifact_version v ON v.artifact_id = a.id AND v.seq = a.current_seq\n\t\tWHERE " + where)
		args = append(args, selectArgs...)
		args = append(args, whereArgs...)
		if err := s.writeCandidateFilters(&b, &args, q); err != nil {
			filtErr = err
			return
		}
		b.WriteString(" ORDER BY a.updated_at DESC, a.id DESC LIMIT ?) arm" + strconv.Itoa(nArms))
		args = append(args, q.Limit)
	}
	arm("artifact a", "a.owner_kind = ? AND a.owner_ref = ?", q.PrincipalKind, q.PrincipalRef)
	if !q.OwnedOnly {
		now := s.timeArg(q.Now)
		permSQL, permArgs := readGrantFilter("g")
		grantWhere := " AND (g.expires_at IS NULL OR g.expires_at > ?) AND " + permSQL
		grantArgs := append([]any{now}, permArgs...)
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
	if filtErr != nil {
		return "", nil, filtErr
	}
	b.WriteString("\nORDER BY updated_at DESC, id DESC LIMIT ?")
	args = append(args, q.Limit)

	return s.rebind(b.String()), args, nil
}

// readGrantFilter returns the SQL condition, on the grant table aliased
// alias, that a grant carries a permission in grantsForRead, and its
// arguments. Both list pushdowns (the grant arms of the candidate query and
// the "shared with the project" subquery) use it, so the read set the list
// pushes down to SQL is always the one canReadWith matches in Go.
func readGrantFilter(alias string) (string, []any) {
	args := make([]any, len(grantsForRead))
	for i, p := range grantsForRead {
		args[i] = p
	}
	return alias + ".permission IN (" + strings.TrimSuffix(strings.Repeat("?, ", len(args)), ", ") + ")", args
}

// candidateColumns is the column list every arm of the candidate query
// selects; the union and its outer ORDER BY rely on the names.
const candidateColumns = `a.id, a.scope_kind, a.scope_ref, a.owner_kind, a.owner_ref, a."key", a.title,
		a.current_seq, a.expires_at, a.created_at, a.updated_at, a.orphaned_at, a.sponsor_ref,
		a.former_owner_kind, a.former_owner_ref, a.transferred_at, a.former_key, v.kind AS current_kind`

// writeCandidateFilters appends the conditions every candidate arm shares:
// live, not expired, the search, the review filter, the home scope and the
// keyset position.
//
// Search folds case the way the database folds it, so a pattern and a
// column are always compared under the same rules: SQLite's LIKE ignores
// case for ASCII letters only and compares other characters exactly;
// Postgres lowercases both sides with LOWER, under the database's own
// collation rules.
func (s *sqlStore) writeCandidateFilters(b *strings.Builder, args *[]any, q CandidateQuery) error {
	now := s.timeArg(q.Now)
	// Attachments are never listed: they are read through their message.
	b.WriteString(" AND " + notAttachmentScope("a.scope_kind"))
	b.WriteString(" AND a.deleted_at IS NULL AND (a.expires_at IS NULL OR a.expires_at > ?)")
	*args = append(*args, now)
	if q.ReviewOnLatest {
		b.WriteString(" AND v.kind = ?")
		*args = append(*args, VersionKindReview)
	}
	if q.RequestedOnly {
		if q.ReviewRequestsEnabled {
			b.WriteString(" AND EXISTS (SELECT 1 FROM artifact_review_request r WHERE r.artifact_id = a.id AND r.reviewer_kind = ? AND r.reviewer_ref = ? AND r.state = ?)")
			*args = append(*args, q.PrincipalKind, q.PrincipalRef, ReviewRequestStateOpen)
		} else {
			b.WriteString(" AND 1 = 0")
		}
	}
	if q.DraftsOnly {
		if q.ReviewDraftsEnabled {
			b.WriteString(" AND EXISTS (SELECT 1 FROM artifact_review_draft d WHERE d.artifact_id = a.id AND d.principal_kind = ? AND d.principal_ref = ?)")
			*args = append(*args, q.PrincipalKind, q.PrincipalRef)
		} else {
			b.WriteString(" AND 1 = 0")
		}
	}
	if q.HomeScope != "" {
		// Shared with the project: an unexpired scope grant to it, found
		// through the (artifact_id, subject_kind, subject_ref) unique index.
		shared, sharedArgs := projectSharedExistsSQL(q.HomeScope, now)
		switch {
		case q.ScopeShares && q.SharedOnly:
			b.WriteString(" AND a.scope_kind = ? AND a.scope_ref <> ? AND " + shared)
			*args = append(*args, ScopeKindProject, q.HomeScope)
			*args = append(*args, sharedArgs...)
		case q.ScopeShares:
			b.WriteString(" AND a.scope_kind = ? AND (a.scope_ref = ? OR " + shared + ")")
			*args = append(*args, ScopeKindProject, q.HomeScope)
			*args = append(*args, sharedArgs...)
		default:
			b.WriteString(" AND a.scope_kind = ? AND a.scope_ref = ?")
			*args = append(*args, ScopeKindProject, q.HomeScope)
		}
	}
	if q.Query != nil {
		if err := s.writeQueryTermsFilters(b, args, q, now); err != nil {
			return err
		}
	}
	if q.After != nil {
		at := s.timeArg(q.After.UpdatedAt)
		b.WriteString(" AND (a.updated_at < ? OR (a.updated_at = ? AND a.id < ?))")
		*args = append(*args, at, at, q.After.ID)
	}
	return nil
}

func projectSharedExistsSQL(homeScope string, now any) (string, []any) {
	permSQL, permArgs := readGrantFilter("sg")
	sql := `EXISTS (SELECT 1 FROM artifact_grant sg WHERE sg.artifact_id = a.id AND sg.subject_kind = ?
			AND sg.subject_ref = ? AND (sg.expires_at IS NULL OR sg.expires_at > ?) AND ` + permSQL + `)`
	args := append([]any{SubjectScope, homeScope, now}, permArgs...)
	return sql, args
}

func (s *sqlStore) writeSearchLike(b *strings.Builder, args *[]any, text string, negate bool) {
	pattern := "%" + escapeLike(text) + "%"
	if negate {
		b.WriteString(" AND NOT ")
	} else {
		b.WriteString(" AND ")
	}
	if s.dialect == dialectPostgres {
		b.WriteString(`(LOWER(a.title) LIKE LOWER(?) ESCAPE '\' OR LOWER(COALESCE(a."key", '')) LIKE LOWER(?) ESCAPE '\')`)
	} else {
		b.WriteString(`(a.title LIKE ? ESCAPE '\' OR COALESCE(a."key", '') LIKE ? ESCAPE '\')`)
	}
	*args = append(*args, pattern, pattern)
}

// writeQueryTermsFilters translates validated store.QueryTerms into SQL
// predicates ANDed onto every candidate arm. An unknown field or flag value
// returns an error rather than silently becoming a no-op.
func (s *sqlStore) writeQueryTermsFilters(b *strings.Builder, args *[]any, q CandidateQuery, now any) error {
	qt := q.Query
	if qt.Text != "" {
		s.writeSearchLike(b, args, qt.Text, false)
	}
	for _, ex := range qt.ExcludeText {
		if ex == "" {
			continue
		}
		s.writeSearchLike(b, args, ex, true)
	}
	for _, term := range qt.Terms {
		if len(term.Values) == 0 {
			return fmt.Errorf("artifacts: empty values for filter field %q", term.Field)
		}
		switch term.Field {
		case "project":
			marks := strings.TrimSuffix(strings.Repeat("?, ", len(term.Values)), ", ")
			if term.Negate {
				b.WriteString(" AND (a.scope_kind <> ? OR a.scope_ref NOT IN (" + marks + "))")
			} else {
				b.WriteString(" AND a.scope_kind = ? AND a.scope_ref IN (" + marks + ")")
			}
			*args = append(*args, ScopeKindProject)
			for _, v := range term.Values {
				*args = append(*args, v)
			}
		case "is":
			if err := s.writeIsTermFilter(b, args, q, now, term); err != nil {
				return err
			}
		default:
			return fmt.Errorf("artifacts: unsupported filter field %q", term.Field)
		}
	}
	return nil
}

func (s *sqlStore) writeIsTermFilter(b *strings.Builder, args *[]any, q CandidateQuery, now any, term QueryTerm) error {
	if term.Negate {
		for _, v := range term.Values {
			switch v {
			case "reviewed":
				b.WriteString(" AND (v.kind IS NULL OR v.kind <> ?)")
				*args = append(*args, VersionKindReview)
			case "requested":
				if q.ReviewRequestsEnabled {
					b.WriteString(" AND NOT EXISTS (SELECT 1 FROM artifact_review_request r WHERE r.artifact_id = a.id AND r.reviewer_kind = ? AND r.reviewer_ref = ? AND r.state = ?)")
					*args = append(*args, q.PrincipalKind, q.PrincipalRef, ReviewRequestStateOpen)
				}
			case "draft":
				if q.ReviewDraftsEnabled {
					b.WriteString(" AND NOT EXISTS (SELECT 1 FROM artifact_review_draft d WHERE d.artifact_id = a.id AND d.principal_kind = ? AND d.principal_ref = ?)")
					*args = append(*args, q.PrincipalKind, q.PrincipalRef)
				}
			case "mine":
				b.WriteString(" AND NOT (a.owner_kind = ? AND a.owner_ref = ?)")
				*args = append(*args, q.PrincipalKind, q.PrincipalRef)
			case "shared":
				if q.HomeScope != "" {
					// On project views, NOT shared with this project means homed here.
					b.WriteString(" AND a.scope_ref = ?")
					*args = append(*args, q.HomeScope)
				}
				// On global views, AccessShared depends on host.Authorize per home
				// project; collect() applies the exact -is:shared check after canReadWith.
			default:
				return fmt.Errorf("artifacts: unsupported is filter value %q", v)
			}
		}
		return nil
	}

	clauses := make([]string, 0, len(term.Values))
	var clauseArgs []any
	for _, v := range term.Values {
		switch v {
		case "reviewed":
			clauses = append(clauses, "v.kind = ?")
			clauseArgs = append(clauseArgs, VersionKindReview)
		case "requested":
			if q.ReviewRequestsEnabled {
				clauses = append(clauses, "EXISTS (SELECT 1 FROM artifact_review_request r WHERE r.artifact_id = a.id AND r.reviewer_kind = ? AND r.reviewer_ref = ? AND r.state = ?)")
				clauseArgs = append(clauseArgs, q.PrincipalKind, q.PrincipalRef, ReviewRequestStateOpen)
			} else {
				clauses = append(clauses, "1 = 0")
			}
		case "draft":
			if q.ReviewDraftsEnabled {
				clauses = append(clauses, "EXISTS (SELECT 1 FROM artifact_review_draft d WHERE d.artifact_id = a.id AND d.principal_kind = ? AND d.principal_ref = ?)")
				clauseArgs = append(clauseArgs, q.PrincipalKind, q.PrincipalRef)
			} else {
				clauses = append(clauses, "1 = 0")
			}
		case "mine":
			clauses = append(clauses, "(a.owner_kind = ? AND a.owner_ref = ?)")
			clauseArgs = append(clauseArgs, q.PrincipalKind, q.PrincipalRef)
		case "shared":
			if q.HomeScope != "" {
				if !q.ScopeShares {
					clauses = append(clauses, "1 = 0")
				} else {
					shared, sharedArgs := projectSharedExistsSQL(q.HomeScope, now)
					clauses = append(clauses, "(a.scope_kind = ? AND a.scope_ref <> ? AND "+shared+")")
					clauseArgs = append(clauseArgs, ScopeKindProject, q.HomeScope)
					clauseArgs = append(clauseArgs, sharedArgs...)
				}
			} else {
				// Sound SQL pre-filter: owned rows are never AccessShared.
				// collect() applies the exact listAccess check after canReadWith.
				clauses = append(clauses, "NOT (a.owner_kind = ? AND a.owner_ref = ?)")
				clauseArgs = append(clauseArgs, q.PrincipalKind, q.PrincipalRef)
			}
		default:
			return fmt.Errorf("artifacts: unsupported is filter value %q", v)
		}
	}
	b.WriteString(" AND (" + strings.Join(clauses, " OR ") + ")")
	*args = append(*args, clauseArgs...)
	return nil
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
