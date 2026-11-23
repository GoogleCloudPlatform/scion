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
	"fmt"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/msgaction"
)

// Secret request status constants.
const (
	SecretRequestStatusPending    = string(msgaction.ClassPending)
	SecretRequestStatusFulfilling = msgaction.SecretRequestClaimedStatus
	SecretRequestStatusFulfilled  = msgaction.SecretRequestResolvedStatus
	SecretRequestStatusReplaced   = string(msgaction.ClassReplaced)
	SecretRequestStatusWithdrawn  = string(msgaction.ClassWithdrawn)
	SecretRequestStatusDismissed  = string(msgaction.ClassDismissed)
	SecretRequestStatusExpired    = string(msgaction.ClassExpired)
)

// Secret request withdrawn_by values.
const (
	SecretRequestWithdrawnByAgent = "agent"
	SecretRequestWithdrawnByHub   = "hub"
)

// Secret request surface values.
const (
	SecretRequestSurfaceNative         = msgaction.SecretRequestSurfaceNative
	SecretRequestSurfaceStandalonePage = msgaction.SecretRequestSurfaceStandalonePage
)

// SecretRequestListState filters GET /api/v1/agent/secret-requests.
type SecretRequestListState string

const (
	SecretRequestListStatePending   SecretRequestListState = "pending"
	SecretRequestListStateFulfilled SecretRequestListState = "fulfilled"
	SecretRequestListStateWithdrawn SecretRequestListState = "withdrawn"
	SecretRequestListStateDismissed SecretRequestListState = "dismissed"
	SecretRequestListStateExpired   SecretRequestListState = "expired"
	SecretRequestListStateAll       SecretRequestListState = "all"
)

// Secret request list pagination bounds.
const (
	DefaultSecretRequestListLimit = 50
	MaxSecretRequestListLimit     = 100
)

// SecretRequest is the durable record of one requested secret key version.
// It never stores secret values.
type SecretRequest struct {
	ID                string
	ProjectID         string
	AgentID           string
	AgentName         string
	Key               string
	KeyNorm           string
	Version           int
	Description       string
	SecretType        string
	Target            string
	MessageID         string
	Position          int
	IntroLength       int
	ConversationID    string
	ChatKey           string
	Status            string
	ReplacedByID      string
	ReplacedAt        *time.Time
	WithdrawnReason   string
	WithdrawnBy       string
	ResolvedByUserID  string
	ResolvedByName    string
	ResolvedByEmail   string
	DismissReason     string
	Surface           string
	IdempotencyKey    string
	ResponseMessageID string
	SecretVersion     int
	ClaimedAt         *time.Time
	CreatedAt         time.Time
	ExpiresAt         time.Time
	ResolvedAt        *time.Time
}

// SecretRequestPromptInput is the input to CreateSecretRequestPrompt.
type SecretRequestPromptInput struct {
	Message       *Message
	Intro         string
	ProjectName   string
	StandaloneURL string
	AgentID       string
	AgentName     string
	Requests      []messages.SecretRequest
	Update        bool
	Now           time.Time
	TTL           time.Duration
}

// CreatedSecretRequest is one row inserted by CreateSecretRequestPrompt.
type CreatedSecretRequest struct {
	Row      *SecretRequest
	Replaced *SecretRequest // non-nil when an earlier pending/terminal row was linked
}

// SecretRequestPromptResult is the result of CreateSecretRequestPrompt.
type SecretRequestPromptResult struct {
	Message  *Message
	Requests []CreatedSecretRequest
}

// PendingSecretRequestKey describes an existing pending key blocking a non-update send.
type PendingSecretRequestKey struct {
	Key       string
	Version   int
	MessageID string
	RowID     string
	Status    string
}

// SecretRequestKeyPendingError is returned when a non-update send names a key
// that already has a pending version (409 secret_request_key_pending).
type SecretRequestKeyPendingError struct {
	Existing []PendingSecretRequestKey
}

func (e *SecretRequestKeyPendingError) Error() string {
	parts := make([]string, len(e.Existing))
	for i, k := range e.Existing {
		parts[i] = fmt.Sprintf("%s v%d (message %s)", k.Key, k.Version, k.MessageID)
	}
	hintKey := "KEY"
	if len(e.Existing) == 1 {
		hintKey = e.Existing[0].Key
	}
	return fmt.Sprintf("secret request already pending for %s; pass --update to replace it, or scion secret request withdraw %s",
		strings.Join(parts, ", "), hintKey)
}

// SecretRequestSubmittingError is returned when an --update send races an
// in-flight fulfill claim (409 secret_request_submitting).
type SecretRequestSubmittingError struct {
	Keys []PendingSecretRequestKey
}

func (e *SecretRequestSubmittingError) Error() string {
	parts := make([]string, len(e.Keys))
	for i, k := range e.Keys {
		parts[i] = fmt.Sprintf("%s v%d", k.Key, k.Version)
	}
	return fmt.Sprintf("secret request is currently being fulfilled for %s; wait for it to finish", strings.Join(parts, ", "))
}

// ClaimSecretRequestsInput is the input to ClaimSecretRequestsForFulfill.
type ClaimSecretRequestsInput struct {
	MessageID      string
	RowIDs         []string
	UserID         string
	UserName       string
	UserEmail      string
	Surface        string
	IdempotencyKey string
	Now            time.Time
}

// ClaimSecretRequestsResult is returned by ClaimSecretRequestsForFulfill.
type ClaimSecretRequestsResult struct {
	Claimed   []*SecretRequest
	ClaimedAt time.Time
	// Replayed is true when the same user already fulfilled this card with the
	// same idempotencyKey; Rows holds the fulfilled rows.
	Replayed bool
	Rows     []*SecretRequest
	// Conflict is non-nil when a row could not be claimed due to its current status.
	Conflict *SecretRequest
}

// FinishSecretRequestFulfillInput is the input to FinishSecretRequestFulfill.
type FinishSecretRequestFulfillInput struct {
	MessageID       string
	RowIDs          []string
	UserID          string
	ClaimedAt       time.Time
	Surface         string
	IdempotencyKey  string
	SecretVersions  map[string]int // rowID -> backend secret version
	ResponseMessage *Message       // optional response message inserted in the same tx
	Now             time.Time
}

// FinishSecretRequestFulfillResult is returned by FinishSecretRequestFulfill.
type FinishSecretRequestFulfillResult struct {
	Rows            []*SecretRequest
	ResponseMessage *Message
	// Conflict is non-nil if the claim was lost (e.g. agent hard-deleted during backend.Set).
	Conflict *SecretRequest
}

// RollbackSecretRequestClaimInput is the input to RollbackSecretRequestClaim.
type RollbackSecretRequestClaimInput struct {
	MessageID string
	RowIDs    []string
	UserID    string
	ClaimedAt time.Time
}

// DismissSecretRequestsInput is the input to DismissSecretRequests.
type DismissSecretRequestsInput struct {
	MessageID       string
	RowIDs          []string // empty targets all pending rows on the card
	UserID          string
	UserName        string
	UserEmail       string
	Reason          string
	Surface         string
	IdempotencyKey  string
	ResponseMessage *Message
	Now             time.Time
}

// DismissSecretRequestsResult is returned by DismissSecretRequests.
type DismissSecretRequestsResult struct {
	Dismissed        []*SecretRequest
	AlreadyDismissed []*SecretRequest
	ResponseMessage  *Message
	Replayed         bool
	// Submitting is non-nil when a targeted row is currently in "fulfilling".
	Submitting *SecretRequest
}

// WithdrawSecretRequestsInput is the input to WithdrawSecretRequests.
type WithdrawSecretRequestsInput struct {
	AgentID  string
	Keys     []string
	WholeSet bool
	Reason   string
	Now      time.Time
}

// WithdrawSecretRequestsResult is returned by WithdrawSecretRequests.
type WithdrawSecretRequestsResult struct {
	Withdrawn        []*SecretRequest
	AlreadyWithdrawn []*SecretRequest
}

// SecretRequestWithdrawFailure is one per-key failure in WithdrawSecretRequestsError.
type SecretRequestWithdrawFailure struct {
	Key     string
	Reason  string // already_fulfilled | dismissed | expired | submitting | not_found
	Request *SecretRequest
}

// WithdrawSecretRequestsError is returned when an all-or-nothing agent withdraw fails.
type WithdrawSecretRequestsError struct {
	Failures []SecretRequestWithdrawFailure
}

func (e *WithdrawSecretRequestsError) Error() string {
	parts := make([]string, len(e.Failures))
	for i, f := range e.Failures {
		parts[i] = fmt.Sprintf("%s: %s", f.Key, f.Reason)
	}
	return strings.Join(parts, "; ")
}

// ListAgentSecretRequestsOptions configures ListAgentSecretRequests.
type ListAgentSecretRequestsOptions struct {
	AgentID string
	State   SecretRequestListState
	Cursor  string
	Limit   int
}

// ListAgentSecretRequestsResult is returned by ListAgentSecretRequests.
type ListAgentSecretRequestsResult struct {
	Items      []*SecretRequest
	NextCursor string
}

// SecretRequestStore defines persistence operations for agent-to-human secret requests.
type SecretRequestStore interface {
	// CreateSecretRequestPrompt inserts the secret-request message and its
	// request rows in one transaction, applying the S1 version and --update rules.
	CreateSecretRequestPrompt(ctx context.Context, in *SecretRequestPromptInput) (*SecretRequestPromptResult, error)

	// GetSecretRequest returns one secret request row by ID.
	GetSecretRequest(ctx context.Context, id string) (*SecretRequest, error)

	// GetSecretRequestsByMessageID returns all secret request rows for a card
	// ordered by position ascending.
	GetSecretRequestsByMessageID(ctx context.Context, messageID string) ([]*SecretRequest, error)

	// GetSecretRequestsByIDs returns secret request rows by ID.
	GetSecretRequestsByIDs(ctx context.Context, ids []string) (map[string]*SecretRequest, error)

	// ClaimSecretRequestsForFulfill atomically transitions pending, non-expired
	// rows of a card from pending -> fulfilling.
	ClaimSecretRequestsForFulfill(ctx context.Context, in *ClaimSecretRequestsInput) (*ClaimSecretRequestsResult, error)

	// FinishSecretRequestFulfill atomically transitions claimed rows from
	// fulfilling -> fulfilled (matched on claimed_at and resolved_by_user_id)
	// and optionally inserts the secret-response message in the same transaction.
	FinishSecretRequestFulfill(ctx context.Context, in *FinishSecretRequestFulfillInput) (*FinishSecretRequestFulfillResult, error)

	// RollbackSecretRequestClaim returns claimed rows from fulfilling -> pending,
	// clearing resolved_by_* and claimed_at.
	RollbackSecretRequestClaim(ctx context.Context, in *RollbackSecretRequestClaimInput) (int, error)

	// DismissSecretRequests atomically transitions targeted pending rows from
	// pending -> dismissed and optionally inserts the secret-response message.
	DismissSecretRequests(ctx context.Context, in *DismissSecretRequestsInput) (*DismissSecretRequestsResult, error)

	// WithdrawSecretRequests atomically withdraws pending rows for the agent
	// (all-or-nothing), returning withdrawn and alreadyWithdrawn rows.
	WithdrawSecretRequests(ctx context.Context, in *WithdrawSecretRequestsInput) (*WithdrawSecretRequestsResult, error)

	// ListAgentSecretRequests lists an agent's own secret requests filtered by state.
	ListAgentSecretRequests(ctx context.Context, opts *ListAgentSecretRequestsOptions) (*ListAgentSecretRequestsResult, error)

	// ExpireSecretRequests transitions pending rows with expires_at <= now to
	// expired and returns the transitioned rows.
	ExpireSecretRequests(ctx context.Context, now time.Time, limit int) ([]*SecretRequest, error)

	// RecoverStaleSecretRequestClaims transitions fulfilling rows with
	// claimed_at <= cutoff back to pending and returns the recovered rows.
	RecoverStaleSecretRequestClaims(ctx context.Context, cutoff time.Time, limit int) ([]*SecretRequest, error)

	// WithdrawAgentSecretRequestsOnDelete transitions pending and fulfilling rows
	// for agentID to withdrawn (reason "agent deleted", withdrawn_by "hub")
	// inside the caller's transaction, returning the transitioned rows.
	WithdrawAgentSecretRequestsOnDelete(ctx context.Context, agentID string, now time.Time) ([]*SecretRequest, error)

	// DeleteProjectSecretRequests removes all secret_requests rows for projectID
	// (called inside the project-delete transaction).
	DeleteProjectSecretRequests(ctx context.Context, projectID string) (int, error)
}
