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

// ErrConflict is returned when a write does not apply to the current state:
// a key already taken, a version that is no longer pending, or a version
// whose files have not all arrived.
var ErrConflict = errors.New("artifacts: conflict")

// ErrTooManyLinks is returned by CreateLink when an artifact already has
// the maximum number of unexpired share links.
var ErrTooManyLinks = errors.New("artifacts: too many share links")

// ErrTooManyGrants is returned by PutGrant when an artifact already has
// the maximum number of principal and scope grants.
var ErrTooManyGrants = errors.New("artifacts: too many grants")

// ErrHomeGrantAdmin is returned when a write would make the scope grant of
// an artifact's home scope an admin grant (PutGrant), or move an artifact
// to a scope that holds an admin grant on it (UpdateArtifact). The home
// project's grant is read or write only.
var ErrHomeGrantAdmin = errors.New("artifacts: the home scope's grant may only be read or write")

// ErrCrossScopeDisabled is returned by PutGrant for a scope grant to a
// scope other than the artifact's home while sharing across scopes is off.
var ErrCrossScopeDisabled = errors.New("artifacts: sharing with other scopes is off")

// ErrTooManyPending is returned by CreateVersion when an artifact already
// has the maximum number of pending versions.
var ErrTooManyPending = errors.New("artifacts: too many pending versions")

// Version kinds.
const (
	VersionKindPublish = "publish"
	VersionKindReview  = "review"
)

// Version states. A version is pending while its files upload (two-step
// publish), finalizing while one finalize request completes it, and ready
// once every file is stored. The single-file fast path writes ready
// directly.
const (
	VersionStatePending    = "pending"
	VersionStateFinalizing = "finalizing"
	VersionStateReady      = "ready"
	VersionStateFailed     = "failed"
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
	// Pending is true while the bytes of an uploaded file of a pending
	// version have not arrived. Files of ready versions are never pending.
	Pending bool
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

	// CreatePending writes a new artifact together with its first version,
	// which must be pending, that version's manifest and the given grants,
	// in one transaction. The artifact has no current version until the
	// version is finalized. When another live artifact already holds a's
	// key for the same owner and scope, it returns ErrConflict.
	CreatePending(ctx context.Context, a *Artifact, v *Version, files []File, grants []Grant) error

	// CreateVersion appends the pending version v and its manifest to an
	// existing artifact, assigning v.Seq (one above the artifact's highest
	// seq). It returns ErrTooManyPending when the artifact already has
	// maxPending pending versions, and ErrNotFound when the artifact is
	// absent or deleted.
	CreateVersion(ctx context.Context, v *Version, files []File, maxPending int) error

	// GetArtifactByKey returns the live artifact with the given key, owner
	// and scope, or ErrNotFound.
	GetArtifactByKey(ctx context.Context, scopeKind, scopeRef, ownerKind, ownerRef, key string) (*Artifact, error)

	// MarkReceived records that the bytes of file path of the pending
	// version versionID have arrived, with their media type. It returns
	// ErrNotFound when the file is not in the manifest and ErrConflict when
	// the version is no longer pending.
	MarkReceived(ctx context.Context, versionID, path, mediaType string) error

	// ClaimFinalize moves the pending version seq of an artifact to
	// finalizing, so that exactly one finalize request completes it. A
	// version already finalizing is taken over when its claim is older
	// than staleBefore (left behind by a request that never completed). It
	// returns ErrConflict when the version cannot be claimed or a file of
	// its manifest is still pending, and ErrNotFound when it does not
	// exist.
	// On success it returns the claim, which FinalizeVersion and
	// ReleaseFinalize take to act only while that claim still holds.
	ClaimFinalize(ctx context.Context, artifactID string, seq int, staleBefore time.Time) (time.Time, error)

	// ReleaseFinalize returns a finalizing version to pending, for a
	// finalize request that could not complete it, if the version still
	// holds the given claim.
	ReleaseFinalize(ctx context.Context, artifactID string, seq int, claim time.Time) error

	// FinalizeVersion flips the claimed (finalizing) version seq of an
	// artifact to ready, adds the extra manifest rows (files the hub produced, such as
	// fetched remote images) and advances the artifact's current version to
	// seq unless a later one is already current. It returns ErrConflict when
	// the version is not finalizing under the given claim, and the updated
	// artifact otherwise.
	FinalizeVersion(ctx context.Context, artifactID string, seq int, claim time.Time, extra []File) (*Artifact, error)

	// ListVersions returns up to limit ready versions of an artifact,
	// newest first, with a seq below before (0 = from the newest).
	ListVersions(ctx context.Context, artifactID string, before, limit int) ([]Version, error)

	// ReapPending marks every version still pending or finalizing that was
	// created before cutoff as failed and drops its manifest, so its blobs are no
	// longer referenced. An artifact left with neither a ready nor a
	// pending version is soft-deleted, which frees its key. It handles at
	// most limit versions per call and returns how many it reaped.
	ReapPending(ctx context.Context, cutoff time.Time, limit int) (int, error)

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

	// CreateLink adds the share link g (SubjectLink, GrantRead, a non-nil
	// ExpiresAt, SubjectRef the token hash) to its artifact. In the same
	// transaction it drops the artifact's link grants that expired at or
	// before now, and it refuses with ErrTooManyLinks when maxLinks
	// unexpired links remain. It returns ErrNotFound when the artifact is
	// absent or deleted.
	CreateLink(ctx context.Context, g *Grant, maxLinks int, now time.Time) error

	// ResolveLink returns the link grant whose subject ref is tokenHash
	// together with its artifact, in one query, provided that at now the
	// link is unexpired and the artifact is live (not deleted, not
	// expired) with a current version. Every other case, a hash no link
	// has included, returns ErrNotFound.
	ResolveLink(ctx context.Context, tokenHash string, now time.Time) (*Artifact, *Grant, error)

	// LinkActive reports whether link grant linkID of artifact artifactID
	// exists and is unexpired at now.
	LinkActive(ctx context.Context, artifactID, linkID string, now time.Time) (bool, error)

	// RevokeLink deletes link grant linkID of artifact artifactID. It
	// returns ErrNotFound when no such link grant exists.
	RevokeLink(ctx context.Context, artifactID, linkID string) error

	// PutGrant adds the principal or scope grant g to its artifact, or,
	// when the artifact already has a grant for the same subject, sets
	// that grant's permission to g's (g.ID and g.CreatedAt then take the
	// stored grant's). created reports which. Under the artifact's lock it
	// reads the artifact's home scope and refuses an admin grant to it
	// (ErrHomeGrantAdmin) and, unless crossScope, a scope grant to any
	// other scope (ErrCrossScopeDisabled). It refuses with
	// ErrTooManyGrants when adding would exceed maxGrants principal and
	// scope grants, and returns ErrNotFound when the artifact is absent or
	// deleted.
	PutGrant(ctx context.Context, g *Grant, maxGrants int, crossScope bool) (created bool, err error)

	// DeleteGrant deletes principal or scope grant grantID of artifact
	// artifactID. It returns ErrNotFound when there is no such grant, and
	// ErrConflict, deleting nothing, when the grant is the scope grant of
	// the artifact's current home scope (checked under the artifact's
	// lock, so a concurrent move cannot slip between check and delete).
	DeleteGrant(ctx context.Context, artifactID, grantID string) error

	// SetExpiry sets (or, with nil, clears) a live artifact's expiry and
	// returns the updated artifact, or ErrNotFound.
	SetExpiry(ctx context.Context, artifactID string, expiresAt *time.Time) (*Artifact, error)

	// UpdateArtifact applies u to a live artifact in one transaction under
	// the artifact's lock and returns the updated artifact. It returns
	// ErrNotFound when the artifact is absent or deleted, ErrConflict when
	// a move would give the owner two live artifacts with the same key in
	// the new scope, ErrTooManyGrants when a move would exceed u.MaxGrants
	// principal and scope grants, and ErrHomeGrantAdmin when the new scope
	// holds an admin grant on the artifact (lower it first).
	UpdateArtifact(ctx context.Context, artifactID string, u ArtifactUpdate) (*Artifact, error)

	// SweepExpired soft-deletes up to limit live artifacts whose expiry is
	// at or before now, deleting their grants and share links in the same
	// transaction, and returns how many it deleted.
	SweepExpired(ctx context.Context, now time.Time, limit int) (int, error)

	// TouchBlob records that a write is about to store blob digest at now,
	// so the blob sweep leaves the blob alone for the grace period. It
	// waits for a sweep that holds the blob's state to finish.
	TouchBlob(ctx context.Context, digest string, now time.Time) error

	// MarkBlobs records, for each blob (at most MaxBlobBatch), whether a
	// live artifact references it: a referenced blob's state is dropped;
	// an unreferenced one is marked unreferenced since now unless it
	// already is, and its listed generation is recorded.
	MarkBlobs(ctx context.Context, blobs []BlobMark, now time.Time) error

	// ReclaimBlobs deletes up to limit blobs marked unreferenced since at
	// or before cutoff and not touched since cutoff. For each, in one
	// transaction holding the blob's state row, it checks the mark, the
	// touch and the references again, calls del
	// with the digest and the generation recorded at marking (0 when
	// unknown), which removes the bytes, and drops the row; a writer
	// touching the blob meanwhile waits for that transaction. It returns
	// how many blobs it deleted; an error from del ends the pass.
	ReclaimBlobs(ctx context.Context, cutoff time.Time, limit int, del func(digest string, generation int64) error) (int, error)

	// AddMessageRefs records that message messageID references refs, in the
	// artifact_message_ref link table. A reference already recorded for the
	// message and artifact is left as it is. The caller has already checked
	// that every referenced artifact exists.
	AddMessageRefs(ctx context.Context, messageID string, refs []MessageRef) error

	// ListMessageRefs returns the references recorded for each of
	// messageIDs, keyed by message id and ordered by artifact id. Messages
	// without references are absent from the map.
	ListMessageRefs(ctx context.Context, messageIDs []string) (map[string][]MessageRef, error)
}

// BlobMark is one listed blob for MarkBlobs.
type BlobMark struct {
	Digest string
	// Generation is the object generation the listing returned, or 0.
	Generation int64
}

// ArtifactUpdate is a change UpdateArtifact applies.
type ArtifactUpdate struct {
	// HomeGrant, when set, moves the artifact to the scope it names
	// (HomeGrant.SubjectRef). The old home scope's grant is removed; the
	// new scope gets HomeGrant (a read grant) unless it already has a scope
	// grant, which is kept as it is.
	HomeGrant *Grant
	// MaxGrants bounds the artifact's principal and scope grants after a
	// move.
	MaxGrants int
	// SetExpiry applies ExpiresAt, which nil clears.
	SetExpiry bool
	ExpiresAt *time.Time
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
	// HomeScope, when set, keeps only rows homed in this project or
	// carrying an unexpired read, write or admin scope grant to it. It
	// narrows the other conditions and never adds rows.
	HomeScope string
	// SharedOnly, with HomeScope, keeps only the rows homed elsewhere.
	SharedOnly bool
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
