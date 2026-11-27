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

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// UploadTargetKind identifies the destination of a resumable chunked upload
// session (ptone/scion#8654 §3.4).
type UploadTargetKind string

const (
	UploadTargetChatAttachment UploadTargetKind = "chat_attachment"
	UploadTargetWorkspace      UploadTargetKind = "workspace"
	UploadTargetSharedDir      UploadTargetKind = "shared_dir"
)

// Valid reports whether k is a recognized UploadTargetKind.
func (k UploadTargetKind) Valid() bool {
	switch k {
	case UploadTargetChatAttachment, UploadTargetWorkspace, UploadTargetSharedDir:
		return true
	default:
		return false
	}
}

// UploadSessionState is the lifecycle state of a resumable chunked upload
// session (ptone/scion#8654 §3.4, §3.7).
type UploadSessionState string

const (
	UploadStateUploading  UploadSessionState = "uploading"
	UploadStateFinalizing UploadSessionState = "finalizing"
	UploadStateCompleted  UploadSessionState = "completed"
	UploadStateFailed     UploadSessionState = "failed"
	UploadStateAborted    UploadSessionState = "aborted"
	UploadStateExpired    UploadSessionState = "expired"
)

// IsTerminal reports whether st is a sticky terminal state. Once a session
// reaches a terminal state, no writer may transition it to another state.
func (st UploadSessionState) IsTerminal() bool {
	switch st {
	case UploadStateCompleted, UploadStateFailed, UploadStateAborted, UploadStateExpired:
		return true
	default:
		return false
	}
}

// Valid reports whether st is a recognized UploadSessionState.
func (st UploadSessionState) Valid() bool {
	switch st {
	case UploadStateUploading, UploadStateFinalizing, UploadStateCompleted,
		UploadStateFailed, UploadStateAborted, UploadStateExpired:
		return true
	default:
		return false
	}
}

// Sweeper retention windows from ptone/scion#8654 §3.7.
const (
	// UploadTerminalRetention is how long terminal rows (including completed
	// rows carrying replayable result JSON) are retained before the sweeper
	// deletes staging and removes the row.
	UploadTerminalRetention = 15 * time.Minute

	// UploadFinalizingStuckAge is how long past lease_until a finalizing
	// session must sit before the sweeper marks it failed.
	UploadFinalizingStuckAge = 1 * time.Hour
)

// Typed errors returned by UploadSessionStore.
// ErrQuotaExceeded and ErrNotFound are declared in store.go.
var (
	// ErrBusy is returned when another writer holds an active lease on the
	// upload session.
	ErrBusy = errors.New("upload session busy")

	// ErrOffsetMismatch is returned by ClaimUploadLease when the caller's
	// expected offset does not match the session's committed received offset.
	// Callers may unwrap to *OffsetMismatchError to read the actual received
	// offset without a second lookup.
	ErrOffsetMismatch = errors.New("upload offset mismatch")

	// ErrLeaseLost is returned by CommitUploadChunk, ReleaseUploadLease, or
	// TransitionUploadSession when the caller's lease token no longer matches
	// the session row (e.g. after a lease takeover).
	ErrLeaseLost = errors.New("upload lease lost")

	// ErrStateChanged is returned when a conditional lease claim or state
	// transition fails because the session is no longer in an expected state
	// (including when it has already reached a sticky terminal state).
	ErrStateChanged = errors.New("upload session state changed")
)

// OffsetMismatchError carries the committed received offset when
// ClaimUploadLease fails with ErrOffsetMismatch.
type OffsetMismatchError struct {
	Expected int64
	Received int64
}

func (e *OffsetMismatchError) Error() string {
	return fmt.Sprintf("%s: expected offset %d, session received %d", ErrOffsetMismatch, e.Expected, e.Received)
}

func (e *OffsetMismatchError) Unwrap() error {
	return ErrOffsetMismatch
}

// UploadSession is the durable state for one resumable chunked upload
// (ptone/scion#8654 §3.4).
type UploadSession struct {
	ID             string
	HubID          string
	OwnerKind      string
	OwnerID        string
	TargetKind     UploadTargetKind
	ScopeProjectID string
	ScopeSharedDir string
	DestPath       string
	Overwrite      bool
	FileName       string
	MediaType      string
	DeclaredSize   int64
	Received       int64
	HashState      []byte
	Head           []byte
	Parts          []string
	StagingRef     string
	State          UploadSessionState
	LeaseToken     string
	LeaseUntil     *time.Time
	Takeovers      int
	Result         json.RawMessage
	ArtifactID     string
	IdleExpiresAt  time.Time
	HardExpiresAt  time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// UploadQuota holds the per-user and hub-wide limits enforced atomically by
// CreateUploadSession (ptone/scion#8654 §3.8). A non-positive field disables
// that specific limit check.
type UploadQuota struct {
	MaxSessionsPerUser     int
	MaxPendingBytesPerUser int64
	MaxPendingBytesHub     int64
}

// UploadChunkCommit holds the fields updated by CommitUploadChunk.
type UploadChunkCommit struct {
	// Received is the new committed byte offset (old received + chunk length).
	Received int64
	// HashState is the marshalled sha256 state after Received bytes.
	HashState []byte
	// Head, when non-empty, is written via COALESCE(head, ?) so only the first
	// chunk populates it and later chunks never overwrite it.
	Head []byte
	// Parts, when non-nil, replaces the ordered list of committed ObjectSink
	// part object names. Leave nil for FileSink.
	Parts []string
	// IdleExpiresAt is the new idle expiry timestamp (only CreateUploadSession
	// and CommitUploadChunk ever advance idle_expires_at).
	IdleExpiresAt time.Time
}

// UploadSessionStore defines persistence operations for HA-safe resumable
// chunked upload sessions (ptone/scion#8654 §3.4, §3.7, §3.8).
//
// Every row mutation is a conditional UPDATE fenced by state and/or
// lease_token. Terminal states (completed, failed, aborted, expired) are
// sticky. idle_expires_at is modified only on CreateUploadSession and
// CommitUploadChunk.
type UploadSessionStore interface {
	// CreateUploadSession inserts s in state uploading after checking q
	// atomically under a hub-wide advisory lock (Postgres) or write
	// transaction (SQLite). Returns ErrQuotaExceeded if any quota would be
	// exceeded.
	CreateUploadSession(ctx context.Context, s *UploadSession, q UploadQuota) error

	// GetUploadSession fetches a session by ID, or ErrNotFound.
	GetUploadSession(ctx context.Context, id string) (*UploadSession, error)

	// ClaimUploadLease atomically acquires a short lease on id when
	// state == wantState, received == atOffset, and the lease is currently
	// free or expired. Taking over an expired lease increments takeovers.
	// Never touches idle_expires_at.
	// Returns ErrNotFound, ErrStateChanged, *OffsetMismatchError (ErrOffsetMismatch),
	// or ErrBusy.
	ClaimUploadLease(ctx context.Context, id string, wantState UploadSessionState, atOffset int64, token string, ttl time.Duration) (*UploadSession, error)

	// CommitUploadChunk advances received, hash_state, head (if unset), parts
	// (if non-nil), and idle_expires_at, and clears the lease, fenced by
	// lease_token == token AND state == uploading.
	// Returns ErrNotFound or ErrLeaseLost.
	CommitUploadChunk(ctx context.Context, id, token string, upd UploadChunkCommit) error

	// ReleaseUploadLease clears the lease when lease_token == token.
	// Never touches idle_expires_at.
	// Returns ErrNotFound or ErrLeaseLost.
	ReleaseUploadLease(ctx context.Context, id, token string) error

	// TransitionUploadSession conditionally transitions id from one of the
	// non-terminal states in from to to.
	//
	// When token != "", the UPDATE requires lease_token == token.
	// When token == "", the UPDATE requires the lease to be free or expired.
	// Transitioning to a terminal state clears the lease and stores result (if
	// non-empty). Transitioning to a non-terminal state (e.g. finalizing)
	// preserves the lease. Never touches idle_expires_at.
	//
	// Returns ErrNotFound, ErrStateChanged, ErrLeaseLost, or ErrBusy.
	TransitionUploadSession(ctx context.Context, id string, from []UploadSessionState, to UploadSessionState, token string, result json.RawMessage) error

	// ListExpiredUploadSessions returns up to limit sessions eligible for
	// sweeper action as of now (§3.7):
	//   - state == uploading with (idle_expires_at < now OR hard_expires_at < now)
	//     and lease free/expired;
	//   - state == finalizing with lease_until IS NULL OR lease_until < now-1h;
	//   - terminal states (completed, failed, aborted, expired) with
	//     updated_at < now-15m.
	ListExpiredUploadSessions(ctx context.Context, now time.Time, limit int) ([]*UploadSession, error)

	// DeleteUploadSession deletes the session row by ID, or returns ErrNotFound.
	DeleteUploadSession(ctx context.Context, id string) error
}
