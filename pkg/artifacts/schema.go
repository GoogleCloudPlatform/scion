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

// Schema DDL, one string per dialect. Both create the same tables, columns
// and indexes; they differ only in column types (SQLite stores timestamps
// as RFC 3339 UTC TEXT, Postgres as TIMESTAMPTZ).
//
// Ids are TEXT in both dialects. The service generates UUIDs, but ids also
// arrive from request paths, and a TEXT column lets a malformed id simply
// match nothing instead of failing the query with a type error.
//
// The key column is quoted because KEY is a keyword in SQLite.
//
// Every statement is idempotent (IF NOT EXISTS), so Init can run on every
// hub start. Changes after the initial schema must be appended as named
// migrations (see migrations in store_sql.go), never by editing this DDL in
// a way existing databases would not pick up.

// migrationInitial is the ledger name recorded for the initial schema.
const migrationInitial = "0001_initial_schema"

const sqliteSchema = `
CREATE TABLE IF NOT EXISTS artifact (
    id          TEXT PRIMARY KEY,
    scope_kind  TEXT NOT NULL,
    scope_ref   TEXT NOT NULL,
    owner_kind  TEXT NOT NULL,
    owner_ref   TEXT NOT NULL,
    "key"       TEXT,
    title       TEXT NOT NULL,
    current_seq INTEGER,
    expires_at  TEXT,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    deleted_at  TEXT
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_artifact_owner_key
    ON artifact (scope_kind, scope_ref, owner_kind, owner_ref, "key")
    WHERE "key" IS NOT NULL AND deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_artifact_scope
    ON artifact (scope_kind, scope_ref, deleted_at);

CREATE INDEX IF NOT EXISTS idx_artifact_owner
    ON artifact (owner_kind, owner_ref, deleted_at);

CREATE TABLE IF NOT EXISTS artifact_version (
    id              TEXT PRIMARY KEY,
    artifact_id     TEXT NOT NULL REFERENCES artifact (id),
    seq             INTEGER NOT NULL,
    kind            TEXT NOT NULL,
    entry_path      TEXT NOT NULL,
    note            TEXT,
    total_bytes     INTEGER NOT NULL,
    file_count      INTEGER NOT NULL,
    created_by_kind TEXT,
    created_by_ref  TEXT,
    created_at      TEXT NOT NULL,
    state           TEXT NOT NULL,
    UNIQUE (artifact_id, seq)
);

CREATE TABLE IF NOT EXISTS artifact_file (
    version_id TEXT NOT NULL REFERENCES artifact_version (id),
    path       TEXT NOT NULL,
    size       INTEGER NOT NULL,
    sha256     TEXT NOT NULL,
    media_type TEXT NOT NULL,
    PRIMARY KEY (version_id, path)
);

CREATE INDEX IF NOT EXISTS idx_artifact_file_sha256
    ON artifact_file (sha256);

CREATE TABLE IF NOT EXISTS artifact_grant (
    id             TEXT PRIMARY KEY,
    artifact_id    TEXT NOT NULL REFERENCES artifact (id),
    subject_kind   TEXT NOT NULL,
    subject_ref    TEXT NOT NULL,
    permission     TEXT NOT NULL,
    expires_at     TEXT,
    created_by_ref TEXT,
    created_at     TEXT NOT NULL,
    UNIQUE (artifact_id, subject_kind, subject_ref)
);

CREATE INDEX IF NOT EXISTS idx_artifact_grant_subject
    ON artifact_grant (subject_kind, subject_ref);

CREATE TABLE IF NOT EXISTS artifact_message_ref (
    message_id  TEXT NOT NULL,
    artifact_id TEXT NOT NULL REFERENCES artifact (id),
    seq         INTEGER,
    PRIMARY KEY (message_id, artifact_id)
);

CREATE TABLE IF NOT EXISTS artifact_migrations (
    name       TEXT PRIMARY KEY,
    applied_at TEXT NOT NULL
);
`

const postgresSchema = `
CREATE TABLE IF NOT EXISTS artifact (
    id          TEXT PRIMARY KEY,
    scope_kind  TEXT NOT NULL,
    scope_ref   TEXT NOT NULL,
    owner_kind  TEXT NOT NULL,
    owner_ref   TEXT NOT NULL,
    "key"       TEXT,
    title       TEXT NOT NULL,
    current_seq INTEGER,
    expires_at  TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL,
    deleted_at  TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_artifact_owner_key
    ON artifact (scope_kind, scope_ref, owner_kind, owner_ref, "key")
    WHERE "key" IS NOT NULL AND deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_artifact_scope
    ON artifact (scope_kind, scope_ref, deleted_at);

CREATE INDEX IF NOT EXISTS idx_artifact_owner
    ON artifact (owner_kind, owner_ref, deleted_at);

CREATE TABLE IF NOT EXISTS artifact_version (
    id              TEXT PRIMARY KEY,
    artifact_id     TEXT NOT NULL REFERENCES artifact (id),
    seq             INTEGER NOT NULL,
    kind            TEXT NOT NULL,
    entry_path      TEXT NOT NULL,
    note            TEXT,
    total_bytes     BIGINT NOT NULL,
    file_count      INTEGER NOT NULL,
    created_by_kind TEXT,
    created_by_ref  TEXT,
    created_at      TIMESTAMPTZ NOT NULL,
    state           TEXT NOT NULL,
    UNIQUE (artifact_id, seq)
);

CREATE TABLE IF NOT EXISTS artifact_file (
    version_id TEXT NOT NULL REFERENCES artifact_version (id),
    path       TEXT NOT NULL,
    size       BIGINT NOT NULL,
    sha256     TEXT NOT NULL,
    media_type TEXT NOT NULL,
    PRIMARY KEY (version_id, path)
);

CREATE INDEX IF NOT EXISTS idx_artifact_file_sha256
    ON artifact_file (sha256);

CREATE TABLE IF NOT EXISTS artifact_grant (
    id             TEXT PRIMARY KEY,
    artifact_id    TEXT NOT NULL REFERENCES artifact (id),
    subject_kind   TEXT NOT NULL,
    subject_ref    TEXT NOT NULL,
    permission     TEXT NOT NULL,
    expires_at     TIMESTAMPTZ,
    created_by_ref TEXT,
    created_at     TIMESTAMPTZ NOT NULL,
    UNIQUE (artifact_id, subject_kind, subject_ref)
);

CREATE INDEX IF NOT EXISTS idx_artifact_grant_subject
    ON artifact_grant (subject_kind, subject_ref);

CREATE TABLE IF NOT EXISTS artifact_message_ref (
    message_id  TEXT NOT NULL,
    artifact_id TEXT NOT NULL REFERENCES artifact (id),
    seq         INTEGER,
    PRIMARY KEY (message_id, artifact_id)
);

CREATE TABLE IF NOT EXISTS artifact_migrations (
    name       TEXT PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL
);
`

// migrationRemoteFiles adds the columns that describe where a manifest file
// came from: an upload, or a remote resource the hub fetched at publish
// time (origin, source_url, fetch_status, fetch_error). sha256 becomes
// nullable, because a remote fetch that failed has no content.
const migrationRemoteFiles = "0002_artifact_file_origin"

// SQLite cannot drop a NOT NULL constraint in place, so the table is
// rebuilt with the new shape and its rows copied.
const sqliteRemoteFiles = `
CREATE TABLE artifact_file_new (
    version_id   TEXT NOT NULL REFERENCES artifact_version (id),
    path         TEXT NOT NULL,
    size         INTEGER NOT NULL,
    sha256       TEXT,
    media_type   TEXT NOT NULL,
    origin       TEXT NOT NULL DEFAULT 'upload',
    source_url   TEXT,
    fetch_status TEXT,
    fetch_error  TEXT,
    PRIMARY KEY (version_id, path)
);
INSERT INTO artifact_file_new (version_id, path, size, sha256, media_type)
    SELECT version_id, path, size, sha256, media_type FROM artifact_file;
DROP TABLE artifact_file;
ALTER TABLE artifact_file_new RENAME TO artifact_file;
CREATE INDEX IF NOT EXISTS idx_artifact_file_sha256
    ON artifact_file (sha256);
`

const postgresRemoteFiles = `
ALTER TABLE artifact_file ADD COLUMN IF NOT EXISTS origin TEXT NOT NULL DEFAULT 'upload';
ALTER TABLE artifact_file ADD COLUMN IF NOT EXISTS source_url TEXT;
ALTER TABLE artifact_file ADD COLUMN IF NOT EXISTS fetch_status TEXT;
ALTER TABLE artifact_file ADD COLUMN IF NOT EXISTS fetch_error TEXT;
ALTER TABLE artifact_file ALTER COLUMN sha256 DROP NOT NULL;
`

// migrationVersionUploads supports the two-step publish: a file row of a
// pending version records whether its bytes have arrived, and pending
// versions can be found by age so abandoned ones are reaped.
const migrationVersionUploads = "0003_version_uploads"

const sqliteVersionUploads = `
ALTER TABLE artifact_file ADD COLUMN received INTEGER NOT NULL DEFAULT 1;
CREATE INDEX IF NOT EXISTS idx_artifact_version_state
    ON artifact_version (state, created_at);
`

const postgresVersionUploads = `
ALTER TABLE artifact_file ADD COLUMN IF NOT EXISTS received BOOLEAN NOT NULL DEFAULT TRUE;
CREATE INDEX IF NOT EXISTS idx_artifact_version_state
    ON artifact_version (state, created_at);
`

// migrationFinalizeClaims records when a finalize request claimed a
// version, so a claim left behind by a stopped hub can be taken over.
const migrationFinalizeClaims = "0004_finalize_claims"

const sqliteFinalizeClaims = `
ALTER TABLE artifact_version ADD COLUMN claimed_at TEXT;
`

const postgresFinalizeClaims = `
ALTER TABLE artifact_version ADD COLUMN IF NOT EXISTS claimed_at TIMESTAMPTZ;
`

// migrationLinkTokens makes a share link token hash name at most one link
// grant across the hub, so resolving a token finds one row or none.
const migrationLinkTokens = "0005_link_tokens"

const sqliteLinkTokens = `
CREATE UNIQUE INDEX IF NOT EXISTS idx_artifact_grant_link
    ON artifact_grant (subject_ref) WHERE subject_kind = 'link';
`

const postgresLinkTokens = `
CREATE UNIQUE INDEX IF NOT EXISTS idx_artifact_grant_link
    ON artifact_grant (subject_ref) WHERE subject_kind = 'link';
`

// migrationBlobGC adds the table the blob sweep keeps its state in: for
// each blob digest, when a publish last touched it (writers record this
// before they check whether the blob exists) and since when no live
// artifact references it; and the index the expiry sweep finds expired
// artifacts by.
const migrationBlobGC = "0006_blob_gc"

const sqliteBlobGC = `
CREATE TABLE IF NOT EXISTS artifact_blob (
    sha256             TEXT PRIMARY KEY,
    touched_at         TEXT,
    unreferenced_since TEXT
);
CREATE INDEX IF NOT EXISTS idx_artifact_blob_unreferenced
    ON artifact_blob (unreferenced_since);
CREATE INDEX IF NOT EXISTS idx_artifact_expires
    ON artifact (expires_at) WHERE deleted_at IS NULL AND expires_at IS NOT NULL;
`

const postgresBlobGC = `
CREATE TABLE IF NOT EXISTS artifact_blob (
    sha256             TEXT PRIMARY KEY,
    touched_at         TIMESTAMPTZ,
    unreferenced_since TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_artifact_blob_unreferenced
    ON artifact_blob (unreferenced_since);
CREATE INDEX IF NOT EXISTS idx_artifact_expires
    ON artifact (expires_at) WHERE deleted_at IS NULL AND expires_at IS NOT NULL;
`
