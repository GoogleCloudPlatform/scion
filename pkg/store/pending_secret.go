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
	"errors"
	"fmt"
	"strings"
	"time"
)

// PendingSecretTTL is how long a pending secret waits for the owner's
// decision. It runs from the first write; a replacement by the same agent
// does not extend it.
const PendingSecretTTL = 7 * 24 * time.Hour

// ErrPendingSecretHeldByOtherAgent is returned by CreateOrReplacePendingSecret
// when a live pending secret with the same key and owner was written by a
// different agent. Nothing is changed.
var ErrPendingSecretHeldByOtherAgent = errors.New("a pending secret with this name was written by another agent")

// ErrPendingSecretExistingChanged is returned by
// ClaimPendingSecretForApproval when the approved secret of the same key
// and owner is not the one the pending secret recorded at write time
// (ReplacesSecretID and ReplacesVersion): one exists although none was
// recorded, the recorded one is gone, or it has another ID or version.
// Nothing is changed.
var ErrPendingSecretExistingChanged = errors.New("the existing secret with this name changed after the pending secret was written")

// Pending secret caps. A writing agent holds at most PendingSecretAgentCap
// live pending secrets, and an owner at most PendingSecretOwnerCap, across
// all writers. A replacement of the same agent's pending secret of the same
// key does not count again.
const (
	PendingSecretAgentCap = 10
	PendingSecretOwnerCap = 50
)

// Values of PendingSecretCapError.Cap.
const (
	PendingSecretCapAgent = "agent"
	PendingSecretCapOwner = "owner"
)

// ErrPendingSecretCap is matched (errors.Is) by every PendingSecretCapError.
var ErrPendingSecretCap = errors.New("pending secret cap reached")

// PendingSecretCapError is returned by CreateOrReplacePendingSecret when the
// write would exceed a cap. Nothing is changed.
type PendingSecretCapError struct {
	Cap   string // PendingSecretCapAgent or PendingSecretCapOwner
	Limit int
}

func (e *PendingSecretCapError) Error() string {
	return fmt.Sprintf("pending secret cap reached: at most %d pending secrets per %s", e.Limit, e.Cap)
}

// Is reports whether target is ErrPendingSecretCap.
func (e *PendingSecretCapError) Is(target error) bool { return target == ErrPendingSecretCap }

// PendingSecret is a user-scope secret written by an agent that waits for
// the owning user's approval. It lives in its own table and is never read by
// a delivery path; approval copies it into the secrets table.
type PendingSecret struct {
	ID            string `json:"id"`
	Key           string `json:"key"`
	SecretType    string `json:"type"`
	Target        string `json:"target,omitempty"`
	Scope         string `json:"scope"`
	ScopeID       string `json:"scopeId"`
	AllowProgeny  bool   `json:"allowProgeny"`
	InjectionMode string `json:"injectionMode,omitempty"`
	Description   string `json:"description,omitempty"`

	// ValueRef is the secret backend's handle to the held value. It is
	// never serialized.
	ValueRef string `json:"-"`
	ValueLen int    `json:"valueLen"`

	// Provenance of the write: taken from the verified agent token and the
	// server clock, never from a request body.
	AgentID     string    `json:"agentId"`
	AgentSlug   string    `json:"agentSlug"`
	ProjectID   string    `json:"projectId"`
	ProjectName string    `json:"projectName"`
	Created     time.Time `json:"created"`
	// Updated is the time of the last replacement by the same agent; it
	// equals Created otherwise.
	Updated time.Time `json:"updated"`

	// ReplacesSecretID and ReplacesVersion record the approved secret of the
	// same key and owner seen at write time, if any.
	ReplacesSecretID string `json:"replacesSecretId,omitempty"`
	ReplacesVersion  int    `json:"replacesVersion,omitempty"`

	ExpiresAt time.Time `json:"expiresAt"`
}

// Expired reports whether the pending secret is past its expiry at now.
func (p *PendingSecret) Expired(now time.Time) bool {
	return !now.Before(p.ExpiresAt)
}

// PendingValuePayload splits a pending secret's value ref into the payload
// fields of a stored secret: a Secret Manager reference ("gcpsm:...") is a
// reference with no local value, and any other ref is the local value
// itself (the ValueCipher envelope). The encryption policy fence and the
// payload scan treat a pending row like a secret row with these fields.
func PendingValuePayload(valueRef string) (encryptedValue, secretRef string) {
	if strings.HasPrefix(valueRef, "gcpsm:") {
		return "", valueRef
	}
	return valueRef, ""
}

// PendingSecretWriteResult is the outcome of CreateOrReplacePendingSecret.
type PendingSecretWriteResult struct {
	// Secret is the row as stored.
	Secret *PendingSecret
	// Replaced is true when the write replaced the same agent's live
	// pending row.
	Replaced bool
	// DroppedValueRefs are the value refs of rows the write deleted (the
	// replaced row, or an expired row of the same key and owner). The caller
	// releases them with the secret backend after the write has committed.
	DroppedValueRefs []string
}

// PendingSecretStore defines persistence for pending secrets.
type PendingSecretStore interface {
	// CreateOrReplacePendingSecret stores p in one transaction, keyed by
	// (Key, Scope, ScopeID). The value ref passes the encryption policy
	// fence (SecretEncryptionStore) like a secret value write, split by
	// PendingValuePayload; a refused write returns
	// ErrEncryptionPolicyMismatch and changes nothing.
	//   - no row: p is inserted with Created = Updated = now and
	//     ExpiresAt = now + PendingSecretTTL;
	//   - an expired row (any writer): it is deleted and p is inserted as a
	//     new write;
	//   - a live row by another agent: ErrPendingSecretHeldByOtherAgent, and
	//     nothing changes;
	//   - a live row by the same agent (p.AgentID): it is replaced by p under
	//     a new ID, keeping its Created and ExpiresAt, with Updated = now.
	// A write that is not a replacement must leave the writing agent with
	// at most PendingSecretAgentCap and the owner with at most
	// PendingSecretOwnerCap live pending secrets; otherwise it returns a
	// *PendingSecretCapError and nothing changes. The caps are checked in
	// the same transaction as the write, serialized per owner.
	// A new ID is assigned when p.ID is empty. The input is not modified.
	// A write that keeps losing races on the unique index (outside an
	// ambient transaction) returns ErrTransient, never ErrAlreadyExists.
	CreateOrReplacePendingSecret(ctx context.Context, p *PendingSecret, now time.Time) (*PendingSecretWriteResult, error)

	// GetPendingSecret returns the pending secret with the given ID, with
	// its ValueRef. Returns ErrNotFound when there is none. Expiry is not
	// applied; callers check Expired.
	GetPendingSecret(ctx context.Context, id string) (*PendingSecret, error)

	// ListPendingSecretsByOwner returns the pending secrets of (scope,
	// scopeID), ordered by key. ValueRef is left empty. Expiry is not
	// applied; callers check Expired.
	ListPendingSecretsByOwner(ctx context.Context, scope, scopeID string) ([]PendingSecret, error)

	// DeletePendingSecret deletes the pending secret with the given ID.
	// Returns ErrNotFound when there is none.
	DeletePendingSecret(ctx context.Context, id string) error

	// ClaimPendingSecret deletes the live pending secret id of (scope,
	// scopeID) and returns it with its ValueRef, in one transaction. Of
	// concurrent claims of one row exactly one succeeds. Returns
	// ErrNotFound when there is no such row, it belongs to another owner,
	// or it has expired at now; nothing is changed then.
	ClaimPendingSecret(ctx context.Context, id, scope, scopeID string, now time.Time) (*PendingSecret, error)

	// ClaimPendingSecretForApproval is ClaimPendingSecret that, in the same
	// transaction, compares the approved secret of the row's key and owner
	// with the row's ReplacesSecretID and ReplacesVersion. On a mismatch it
	// returns ErrPendingSecretExistingChanged and the row is kept.
	ClaimPendingSecretForApproval(ctx context.Context, id, scope, scopeID string, now time.Time) (*PendingSecret, error)

	// RestorePendingSecret inserts p exactly as given (ID, ValueRef and
	// times included): it puts back a row a claim removed when the
	// decision could not be completed. Returns ErrAlreadyExists when a row
	// with the same ID, or the same key and owner, exists. The value ref
	// passes the encryption policy fence as in CreateOrReplacePendingSecret.
	RestorePendingSecret(ctx context.Context, p *PendingSecret) error

	// DeletePendingSecretsByScope deletes every pending secret of (scope,
	// scopeID) and returns their value refs, for the caller to release
	// with the secret backend after the deletion has committed.
	DeletePendingSecretsByScope(ctx context.Context, scope, scopeID string) ([]string, error)

	// DeletePendingSecretsByProject deletes every pending secret written
	// from projectID and returns their value refs, for the caller to
	// release after the deletion has committed.
	DeletePendingSecretsByProject(ctx context.Context, projectID string) ([]string, error)

	// ScanPendingSecretPayloads is ScanSecretPayloads over the pending
	// secrets: up to limit rows (1..MaxSecretPayloadScan, otherwise
	// ErrInvalidInput) with ID > afterID in ascending ID order, each with
	// its value ref split by PendingValuePayload. Version is 0. Rows carry
	// EncryptedValue: callers must never log or return it.
	ScanPendingSecretPayloads(ctx context.Context, afterID string, limit int) ([]SecretPayload, error)

	// ListPendingSecretScopeIDs returns the distinct scope IDs that have
	// pending secrets in scope.
	ListPendingSecretScopeIDs(ctx context.Context, scope string) ([]string, error)

	// DeleteExpiredPendingSecrets deletes up to limit pending secrets whose
	// ExpiresAt is at or before now and returns them, with their ValueRef,
	// so the caller can release the held values after the delete has
	// committed. A row deleted concurrently is not returned.
	DeleteExpiredPendingSecrets(ctx context.Context, now time.Time, limit int) ([]PendingSecret, error)
}
