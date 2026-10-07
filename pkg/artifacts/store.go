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
	"time"
)

// ErrNotFound is returned by Store lookups when no live row matches.
var ErrNotFound = errors.New("artifacts: not found")

// Version kinds.
const (
	VersionKindPublish = "publish"
	VersionKindReview  = "review"
)

// Version states. A version is pending while its files upload (two-step
// publish) and ready once every file is stored. The single-file fast path
// writes ready directly.
const (
	VersionStatePending = "pending"
	VersionStateReady   = "ready"
	VersionStateFailed  = "failed"
)

// Grant subject kinds.
const (
	// SubjectScope grants every principal the host authorizes in the scope
	// named by the subject ref. The home-scope read grant is one of these.
	SubjectScope = "scope"
	// SubjectPrincipal grants one principal. Its subject ref is
	// PrincipalRef(kind, ref).
	SubjectPrincipal = "principal"
	// SubjectLink is a share link. Its subject ref is the hash of the link
	// token, never the token itself.
	SubjectLink = "link"
)

// Grant permissions, weakest first.
const (
	GrantRead  = "read"
	GrantWrite = "write"
	GrantAdmin = "admin"
)

// PrincipalRef is the subject ref of a principal grant.
func PrincipalRef(kind, ref string) string { return kind + ":" + ref }

// Artifact is a row of the artifact table.
type Artifact struct {
	ID        string
	ScopeKind string
	ScopeRef  string
	OwnerKind string
	OwnerRef  string
	// Key is the publisher-chosen stable key, or "" when none was given.
	Key   string
	Title string
	// CurrentSeq is the seq of the current version, or 0 when the artifact
	// has no ready version yet.
	CurrentSeq int
	ExpiresAt  *time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
	DeletedAt  *time.Time
}

// Version is a row of the artifact_version table.
type Version struct {
	ID            string
	ArtifactID    string
	Seq           int
	Kind          string
	EntryPath     string
	Note          string
	TotalBytes    int64
	FileCount     int
	CreatedByKind string
	CreatedByRef  string
	CreatedAt     time.Time
	State         string
}

// File origins.
const (
	// FileOriginUpload is a file whose bytes the publisher sent.
	FileOriginUpload = "upload"
	// FileOriginRemote is a remote resource the hub fetched at publish time.
	FileOriginRemote = "remote"
)

// Fetch statuses of a remote file.
const (
	FetchStatusOK     = "ok"
	FetchStatusFailed = "failed"
)

// File is a row of the artifact_file table: one file in a version's
// manifest. SHA256 is the lowercase hex digest that addresses the blob, or
// "" for a remote file whose fetch failed. Origin is FileOriginUpload (the
// default when empty) or FileOriginRemote; SourceURL, FetchStatus and
// FetchError describe a remote file and are "" for uploads.
type File struct {
	VersionID   string
	Path        string
	Size        int64
	SHA256      string
	MediaType   string
	Origin      string
	SourceURL   string
	FetchStatus string
	FetchError  string
}

// Grant is a row of the artifact_grant table.
type Grant struct {
	ID           string
	ArtifactID   string
	SubjectKind  string
	SubjectRef   string
	Permission   string
	ExpiresAt    *time.Time
	CreatedByRef string
	CreatedAt    time.Time
}

// Store is the only access point to the artifact_* tables.
//
// The tables are created by Init with CREATE TABLE IF NOT EXISTS, outside
// the hub's Ent migration graph, so the service owns its schema (design D3)
// and can move to a standalone deployment with its data. Init creates every
// table of the data model; later schema changes are recorded in the
// artifact_migrations ledger.
type Store interface {
	// Init creates the artifact_* tables and indexes if they do not exist
	// and records applied migrations. It is idempotent and safe to run on
	// every hub start, including from several hubs at once on Postgres.
	Init(ctx context.Context) error

	// CreatePublished writes a new artifact together with its first,
	// ready version, that version's files and the given grants, in one
	// transaction. The artifact's CurrentSeq is set to v.Seq.
	CreatePublished(ctx context.Context, a *Artifact, v *Version, files []File, grants []Grant) error

	// GetArtifact returns the artifact with id unless it is absent or
	// soft-deleted, in which case it returns ErrNotFound.
	GetArtifact(ctx context.Context, id string) (*Artifact, error)

	// GetVersion returns version seq of the artifact, or ErrNotFound.
	GetVersion(ctx context.Context, artifactID string, seq int) (*Version, error)

	// ListFiles returns the manifest of a version ordered by path.
	ListFiles(ctx context.Context, versionID string) ([]File, error)

	// GetFile returns one file of a version's manifest, or ErrNotFound.
	GetFile(ctx context.Context, versionID, path string) (*File, error)

	// ListGrants returns every grant on an artifact, expired ones included,
	// ordered by creation.
	ListGrants(ctx context.Context, artifactID string) ([]Grant, error)

	// ListGrantsFor returns every grant on each of the artifacts, expired
	// ones included, keyed by artifact id and ordered by creation. At most
	// MaxGrantsForIDs ids may be given.
	ListGrantsFor(ctx context.Context, artifactIDs []string) (map[string][]Grant, error)

	// ListCandidates returns live (not deleted, not expired) artifacts the
	// query's principal owns, or that carry an unexpired read, write or
	// admin grant naming the principal or one of the query's scopes,
	// ordered by UpdatedAt then ID, both descending. It is a candidate
	// query only: it decides nothing about access, and every row must
	// still pass the service's read check before it is shown.
	ListCandidates(ctx context.Context, q CandidateQuery) ([]Candidate, error)
}

// CandidateQuery selects rows for ListCandidates.
type CandidateQuery struct {
	// PrincipalKind and PrincipalRef identify the owner to match and the
	// principal grant subject (PrincipalRef(kind, ref)). Both are required.
	PrincipalKind string
	PrincipalRef  string
	// ScopeRefs are the scope grant subjects to match. Empty matches no
	// scope grant.
	ScopeRefs []string
	// OwnedOnly keeps only rows the principal owns; grants are ignored.
	OwnedOnly bool
	// Search, when set, keeps rows whose title or key contains it,
	// case-insensitively. It is matched literally (no wildcards).
	Search string
	// ReviewPending keeps only rows whose current version is a review.
	ReviewPending bool
	// After, when set, keeps rows strictly after this position in the
	// result order.
	After *Position
	// Limit caps the rows returned; it must be positive.
	Limit int
	// Now is the instant expiry is judged at.
	Now time.Time
}

// Position is a place in ListCandidates' order.
type Position struct {
	UpdatedAt time.Time
	ID        string
}

// Candidate is one ListCandidates row: the artifact and the kind of its
// current version ("" when it has none).
type Candidate struct {
	Artifact
	CurrentKind string
}

// NewStore returns the Store for db. driverName selects the SQL dialect:
// "postgres" or "pgx" for Postgres, anything else (including "" and
// "sqlite") for SQLite, matching the hub's database driver names.
func NewStore(db *sql.DB, driverName string) Store {
	switch driverName {
	case "postgres", "pgx":
		return &sqlStore{db: db, dialect: dialectPostgres}
	default:
		return &sqlStore{db: db, dialect: dialectSQLite}
	}
}
