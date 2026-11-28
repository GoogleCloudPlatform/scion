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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/msgaction"
)

// Command request lifecycle status constants.
const (
	CommandRequestStatusPending   = string(msgaction.ClassPending)
	CommandRequestStatusApproved  = msgaction.CommandRequestResolvedStatus
	CommandRequestStatusReplaced  = string(msgaction.ClassReplaced)
	CommandRequestStatusWithdrawn = string(msgaction.ClassWithdrawn)
	CommandRequestStatusDismissed = string(msgaction.ClassDismissed)
	CommandRequestStatusExpired   = string(msgaction.ClassExpired)
)

// Command request withdrawn_by values (design §4.12).
const (
	CommandRequestWithdrawnByAgent  = "agent"
	CommandRequestWithdrawnBySystem = "system"
)

// Command run lifecycle status constants.
const (
	CommandRunStatusQueued      = "queued"
	CommandRunStatusWaking      = "waking"
	CommandRunStatusDispatching = "dispatching"
	CommandRunStatusRunning     = "running"
	CommandRunStatusSucceeded   = "succeeded"
	CommandRunStatusFailed      = "failed"
	CommandRunStatusUnknown     = "unknown"
	CommandRunStatusNotStarted  = "not_started"
)

// Command run wake values.
const (
	CommandRunWakeNone   = "none"
	CommandRunWakeResume = "resume"
)

// Command request list pagination bounds.
const (
	DefaultCommandRequestListLimit = 50
	MaxCommandRequestListLimit     = 100
)

// IsTerminalCommandRunStatus reports whether status is a terminal command_runs status.
func IsTerminalCommandRunStatus(status string) bool {
	switch status {
	case CommandRunStatusSucceeded, CommandRunStatusFailed, CommandRunStatusUnknown, CommandRunStatusNotStarted:
		return true
	default:
		return false
	}
}

// CommandRequest is the durable record of one version of an agent's proposal
// to run a command in a named human's execution container.
type CommandRequest struct {
	ID                   string
	ProjectID            string
	AgentID              string
	AgentName            string
	Key                  string
	KeyNorm              string
	Version              int
	Description          string
	Script               string
	Workdir              string
	TimeoutS             int
	ShareOutputRequested bool
	CommandSHA256        string
	ApproverUserID       string
	MessageID            string
	IntroLength          int
	ConversationID       string
	ChatKey              string
	Status               string
	ReplacedByID         string
	ReplacedAt           *time.Time
	WithdrawnReason      string
	WithdrawnBy          string
	DismissReason        string
	ResolvedByUserID     string
	ResolvedByName       string
	ResolvedByEmail      string
	ResolvedAt           *time.Time
	Surface              string
	IdempotencyKey       string
	CreatedAt            time.Time
	ExpiresAt            time.Time
}

// CommandRun is the single execution attempt of an approved CommandRequest.
// ID is the Conduit run_id minted at approval.
type CommandRun struct {
	ID                 string
	RequestID          string
	CommandSHA256      string
	ScriptLen          int
	ApproverUserID     string
	ExecAgentID        string
	ExecProjectID      string
	LaunchID           string
	Status             string
	Reason             string
	Wake               string
	ShareOutput        bool
	TimeoutS           int
	CaptureMode        string
	Workdir            string
	ExitCode           *int
	DurationMS         *int64
	OutputBytes        *int64
	OutputStatus       string
	NotAfter           *time.Time
	LeaseUntil         time.Time
	LastPolledAt       *time.Time
	PollClaimedAt      *time.Time
	CreatedAt          time.Time
	DispatchedAt       *time.Time
	FinishedAt         *time.Time
	OutcomeDeliveredAt *time.Time
	OutcomeMessageID   string
}

// CommandRequestPromptInput is the input to CreateCommandRequestPrompt.
type CommandRequestPromptInput struct {
	Message      *Message
	Intro        string
	ProjectName  string
	ApproverHint string
	AgentID      string
	AgentName    string
	ApproverID   string
	Request      *messages.CommandRequest
	Update       bool
	Now          time.Time
	TTL          time.Duration
}

// CommandRequestPromptResult is returned by CreateCommandRequestPrompt.
type CommandRequestPromptResult struct {
	Message  *Message
	Request  *CommandRequest
	Replaced *CommandRequest
}

// CommandRequestKeyPendingError is returned when a non-update send names a key
// that already has a pending version (409 command_request_key_pending).
type CommandRequestKeyPendingError struct {
	Key       string
	Version   int
	MessageID string
	RowID     string
	Status    string
}

func (e *CommandRequestKeyPendingError) Error() string {
	return fmt.Sprintf("command request already pending for %s v%d (row %s); pass --update to replace it",
		e.Key, e.Version, e.RowID)
}

// CommandRequestLimitError is returned when an agent already has the maximum
// number of pending command requests (429 command_request_limit, design §4.5).
type CommandRequestLimitError struct {
	AgentID string
	Limit   int
	Current int
}

func (e *CommandRequestLimitError) Error() string {
	return fmt.Sprintf("agent has %d pending command requests (limit %d); withdraw or wait for existing requests before sending another",
		e.Current, e.Limit)
}

// DismissCommandRequestInput is the input to DismissCommandRequest.
type DismissCommandRequestInput struct {
	RequestID      string
	UserID         string
	UserName       string
	UserEmail      string
	Reason         string
	Surface        string
	IdempotencyKey string
	Now            time.Time
}

// DismissCommandRequestResult is returned by DismissCommandRequest.
type DismissCommandRequestResult struct {
	Request          *CommandRequest
	AlreadyDismissed bool
	Replayed         bool
	Conflict         *CommandRequest
	ConflictCode     string
}

// WithdrawCommandRequestInput is the input to WithdrawCommandRequest.
type WithdrawCommandRequestInput struct {
	AgentID string
	Key     string
	Reason  string
	Now     time.Time
}

// WithdrawCommandRequestResult is returned by WithdrawCommandRequest.
type WithdrawCommandRequestResult struct {
	Request          *CommandRequest
	AlreadyWithdrawn bool
	Conflict         *CommandRequest
	ConflictCode     string
}

type withdrawnCommandRequestsCollectorKey struct{}

// WithWithdrawnCommandRequestsCollector attaches a slice pointer to ctx so
// WithdrawAgentCommandRequestsOnDelete records every row it transitions inside
// a hard-delete or retention-purge transaction. Callers inspect the slice
// strictly after commit to publish live updates and emit audit events without
// a pre-transaction listing race.
func WithWithdrawnCommandRequestsCollector(ctx context.Context, out *[]*CommandRequest) context.Context {
	return context.WithValue(ctx, withdrawnCommandRequestsCollectorKey{}, out)
}

// RecordWithdrawnCommandRequests appends rows to any collector attached to ctx.
func RecordWithdrawnCommandRequests(ctx context.Context, rows []*CommandRequest) {
	if len(rows) == 0 {
		return
	}
	if out, ok := ctx.Value(withdrawnCommandRequestsCollectorKey{}).(*[]*CommandRequest); ok && out != nil {
		*out = append(*out, rows...)
	}
}

// ApproveCommandRequestInput is the input to ApproveCommandRequest.
type ApproveCommandRequestInput struct {
	RequestID      string
	CommandSHA256  string
	UserID         string
	UserName       string
	UserEmail      string
	Surface        string
	IdempotencyKey string
	ExecAgentID    string
	ExecProjectID  string
	ShareOutput    bool
	LeaseUntil     time.Time
	Now            time.Time
}

// ApproveCommandRequestResult is returned by ApproveCommandRequest and
// CreateAndApproveMarkdownCommandRequest.
type ApproveCommandRequestResult struct {
	Request  *CommandRequest
	Run      *CommandRun
	Replayed bool
	// Conflict is non-nil when the CAS failed due to status, expiry, or hash mismatch,
	// or when an insert-only Markdown Execute hits an existing v1 row.
	Conflict     *CommandRequest
	ConflictRun  *CommandRun
	ConflictCode string
}

// CreateMarkdownCommandRequestInput is the input to
// CreateAndApproveMarkdownCommandRequest (§4.14 step 5).
type CreateMarkdownCommandRequestInput struct {
	ProjectID      string
	AgentID        string
	AgentName      string
	MessageID      string
	ConversationID string
	ChatKey        string
	Script         string
	CommandSHA256  string
	UserID         string
	UserName       string
	UserEmail      string
	IdempotencyKey string
	ExecAgentID    string
	ExecProjectID  string
	ShareOutput    bool
	LeaseUntil     time.Time
	Now            time.Time
	TTL            time.Duration
}

// CommandRunDispatchInput is the input to TransitionCommandRunToDispatching.
type CommandRunDispatchInput struct {
	RunID        string
	LaunchID     string
	NotAfter     time.Time
	LeaseUntil   time.Time
	DispatchedAt time.Time
}

// FinishCommandRunInput is the input to FinishCommandRun.
type FinishCommandRunInput struct {
	RunID  string
	Status string // must satisfy IsTerminalCommandRunStatus
	// ExpectedStatuses, when non-empty, adds status IN (...) to the finish CAS
	// predicate so a stale caller (e.g. sweeper acting on a queued snapshot)
	// cannot overwrite a run that has already transitioned to dispatching.
	ExpectedStatuses []string
	Reason           string
	ExitCode         *int
	DurationMS       *int64
	OutputBytes      *int64
	OutputStatus     string
	// Output, when non-empty, is inserted into exec_run_outputs with
	// owner_kind='action_run' inside the finish-CAS transaction on a win.
	Output     []byte
	FinishedAt time.Time
}

// ListAgentCommandRequestsOptions configures ListAgentCommandRequests.
type ListAgentCommandRequestsOptions struct {
	AgentID string
	Key     string // optional; compared case-insensitively on key_norm
	Cursor  string
	Limit   int
}

// ListAgentCommandRequestsResult is returned by ListAgentCommandRequests.
type ListAgentCommandRequestsResult struct {
	Items      []*CommandRequest
	Runs       map[string]*CommandRun // keyed by request ID
	NextCursor string
}

// CommandRequestHistoryQuery configures ListResolvedCommandRequests.
type CommandRequestHistoryQuery struct {
	ViewerID        string
	FilterProjectID string
	FilterAgentID   string
	CursorTime      *time.Time
	CursorID        string
	Limit           int
}

// CommandRequestStore defines persistence operations for human-approved command
// requests and their single-execution command_runs rows (ptone/scion#9278).
type CommandRequestStore interface {
	// CreateCommandRequestPrompt inserts the command-request message (rendering
	// the Markdown fallback and setting intro_length) and the new command_requests
	// row in one transaction, applying next-version and --update rules.
	CreateCommandRequestPrompt(ctx context.Context, in *CommandRequestPromptInput) (*CommandRequestPromptResult, error)

	// GetCommandRequest returns one command_requests row by ID.
	GetCommandRequest(ctx context.Context, id string) (*CommandRequest, error)

	// GetCommandRequestByMessageID returns the command_requests row anchored to
	// messageID, or ErrNotFound if none exists.
	GetCommandRequestByMessageID(ctx context.Context, messageID string) (*CommandRequest, error)

	// ListAgentCommandRequests lists an agent's own command requests (optionally
	// filtered by key), ordered newest-first, together with any associated run
	// rows keyed by request ID.
	ListAgentCommandRequests(ctx context.Context, opts *ListAgentCommandRequestsOptions) (*ListAgentCommandRequestsResult, error)

	// ListOpenCommandRequests returns pending, unexpired, non-markdown command
	// requests with a non-nil message_id whose approver_user_id equals viewerID,
	// ordered (created_at DESC, id DESC), up to limit rows.
	ListOpenCommandRequests(ctx context.Context, viewerID string, now time.Time, limit int) ([]*CommandRequest, error)

	// ListResolvedCommandRequests returns resolved (approved, dismissed,
	// withdrawn, expired) non-markdown command requests with non-nil
	// resolved_at and message_id whose approver_user_id equals q.ViewerID,
	// ordered (resolved_at DESC, id DESC), strictly after the keyset cursor.
	ListResolvedCommandRequests(ctx context.Context, q CommandRequestHistoryQuery) ([]*CommandRequest, error)

	// GetCommandRequestsByIDs batch-loads command_requests rows by ID.
	// Missing or malformed IDs are omitted from the returned map.
	GetCommandRequestsByIDs(ctx context.Context, ids []string) (map[string]*CommandRequest, error)

	// ApproveCommandRequest atomically transitions a pending, non-expired
	// command_requests row matching CommandSHA256 from pending -> approved and
	// inserts the single queued command_runs row in the same transaction.
	// When the same user replays with the same IdempotencyKey on an already-
	// approved row, it returns Replayed=true with the existing request and run.
	// On a CAS miss it returns Conflict and ConflictCode without mutating rows.
	ApproveCommandRequest(ctx context.Context, in *ApproveCommandRequestInput) (*ApproveCommandRequestResult, error)

	// CreateAndApproveMarkdownCommandRequest atomically inserts a version-1
	// command_requests row directly in status=approved (surface=markdown_block,
	// key="md-<message_id>-<sha256[:16]>") and its single queued command_runs
	// row in one transaction (§4.14 step 5). Insert-only: a second call for the
	// same (agent_id, message_id, script) returns ConflictCode=already_resolved
	// with the existing row and run (or Replayed=true if same user + same
	// IdempotencyKey).
	CreateAndApproveMarkdownCommandRequest(ctx context.Context, in *CreateMarkdownCommandRequestInput) (*ApproveCommandRequestResult, error)

	// DismissCommandRequest atomically transitions a pending, non-expired
	// command_requests row from pending -> dismissed. Repeating dismiss on an
	// already-dismissed row returns AlreadyDismissed=true. On a CAS miss against
	// another non-pending state (e.g. approved, replaced, withdrawn, expired),
	// it returns Conflict and ConflictCode without mutating rows.
	DismissCommandRequest(ctx context.Context, in *DismissCommandRequestInput) (*DismissCommandRequestResult, error)

	// WithdrawCommandRequest atomically transitions the calling agent's latest
	// version of Key from pending -> withdrawn (withdrawn_by="agent"). Repeating
	// withdraw on an already-withdrawn head returns AlreadyWithdrawn=true. On a
	// non-pending state (approved, dismissed, expired), it returns Conflict and
	// ConflictCode without mutating rows.
	WithdrawCommandRequest(ctx context.Context, in *WithdrawCommandRequestInput) (*WithdrawCommandRequestResult, error)

	// WithdrawAgentCommandRequestsOnDelete transitions all pending
	// command_requests for agentID to withdrawn (withdrawn_by="system",
	// withdrawn_reason="agent deleted") inside the caller's hard-delete
	// transaction. Approved rows and command_runs are left untouched (§4.12).
	WithdrawAgentCommandRequestsOnDelete(ctx context.Context, agentID string, now time.Time) ([]*CommandRequest, error)

	// ExpireCommandRequests transitions pending rows with expires_at <= now to
	// expired and returns the transitioned rows.
	ExpireCommandRequests(ctx context.Context, now time.Time, limit int) ([]*CommandRequest, error)

	// GetCommandRun returns one command_runs row by run ID.
	GetCommandRun(ctx context.Context, runID string) (*CommandRun, error)

	// GetCommandRunByRequestID returns the command_runs row for requestID, or
	// ErrNotFound if the request has no run.
	GetCommandRunByRequestID(ctx context.Context, requestID string) (*CommandRun, error)

	// GetCommandRunByOutcomeMessageID returns the command_runs row whose
	// outcome_message_id matches messageID, or ErrNotFound.
	GetCommandRunByOutcomeMessageID(ctx context.Context, messageID string) (*CommandRun, error)

	// GetCommandRunsByRequestIDs returns existing command_runs keyed by request ID.
	GetCommandRunsByRequestIDs(ctx context.Context, requestIDs []string) (map[string]*CommandRun, error)

	// TransitionCommandRunToWaking atomically transitions a run from
	// queued -> waking (guarded by finished_at IS NULL) and records wake.
	TransitionCommandRunToWaking(ctx context.Context, runID string, wake string) (bool, error)

	// TransitionCommandRunToDispatching atomically transitions a run from
	// queued|waking -> dispatching (guarded by finished_at IS NULL and
	// lease_until > dispatchedAt), pinning launch_id, not_after, lease_until,
	// and dispatched_at. Must commit before sending exec-start.
	TransitionCommandRunToDispatching(ctx context.Context, in *CommandRunDispatchInput) (bool, error)

	// TransitionCommandRunToRunning atomically transitions a run from
	// dispatching -> running (guarded by finished_at IS NULL).
	TransitionCommandRunToRunning(ctx context.Context, runID string) (bool, error)

	// FinishCommandRun atomically records a run's terminal outcome (guarded by
	// finished_at IS NULL, first writer wins). Only when the CAS wins and
	// in.Output is non-empty, it inserts the exec_run_outputs row with
	// owner_kind='action_run' in the same transaction.
	FinishCommandRun(ctx context.Context, in *FinishCommandRunInput) (bool, error)

	// ClaimCommandRunOutcomeDelivery atomically sets outcome_delivered_at on a
	// finished run where outcome_delivered_at IS NULL (or where a prior claim
	// without an outcome_message_id has aged past staleClaimBefore, when
	// non-zero) so outbound delivery of the command-response message is claimed
	// at-least-once across crashes.
	ClaimCommandRunOutcomeDelivery(ctx context.Context, runID string, deliveredAt, staleClaimBefore time.Time) (bool, error)

	// ClearCommandRunOutcomeDelivery clears outcome_delivered_at when the exact
	// claimed timestamp matches, allowing a retry after a transient send or
	// dispatch failure while preserving any recorded outcome_message_id.
	ClearCommandRunOutcomeDelivery(ctx context.Context, runID string, claimedAt time.Time) (bool, error)

	// RecordCommandRunOutcomeMessage records outcome_message_id on a run after
	// the command-response message is sent.
	RecordCommandRunOutcomeMessage(ctx context.Context, runID, messageID string) error

	// ListExpiredCommandRuns returns up to limit unfinished runs
	// (finished_at IS NULL) whose lease_until < now, oldest lease first.
	ListExpiredCommandRuns(ctx context.Context, now time.Time, limit int) ([]*CommandRun, error)

	// ClaimCommandRunsForPoll atomically claims up to limit live unfinished
	// command runs in status dispatching or running (finished_at IS NULL,
	// lease_until >= now), excluding excludeRunIDs, whose poll_claimed_at is
	// nil or <= now-claimTTL. It sets poll_claimed_at = now on the claimed
	// rows so multiple replicas divide recovery polling cleanly (design §4.12).
	ClaimCommandRunsForPoll(ctx context.Context, now time.Time, claimTTL time.Duration, limit int, excludeRunIDs []string) ([]*CommandRun, error)

	// ListCommandRunsPendingOutcomeDelivery returns up to limit finished runs
	// whose finished_at >= finishedSince and either outcome_delivered_at IS NULL
	// or (when staleClaimBefore is non-zero) outcome_delivered_at <= staleClaimBefore
	// with outcome_message_id IS NULL, oldest finished_at first (§4.9).
	ListCommandRunsPendingOutcomeDelivery(ctx context.Context, finishedSince, staleClaimBefore time.Time, limit int) ([]*CommandRun, error)

	// UpdateCommandRunLastPolled updates last_polled_at on an unfinished run.
	UpdateCommandRunLastPolled(ctx context.Context, runID string, polledAt time.Time) error

	// GetActionRunOutput returns the captured output bytes for a command run
	// from exec_run_outputs, hard-filtered to owner_kind='action_run', or
	// ErrNotFound when no output row exists. Callers in pkg/hub are constrained
	// by the TestExecOutputReaders allow-list.
	GetActionRunOutput(ctx context.Context, runID string) ([]byte, error)

	// HasActionRunOutput reports whether an exec_run_outputs row with
	// owner_kind='action_run' exists for runID without reading its content.
	HasActionRunOutput(ctx context.Context, runID string) (bool, error)

	// HasActionRunOutputs reports which runIDs have an exec_run_outputs row
	// with owner_kind='action_run' without reading output content.
	HasActionRunOutputs(ctx context.Context, runIDs []string) (map[string]bool, error)

	// PurgeOldActionRunOutputs deletes exec_run_outputs rows with
	// owner_kind='action_run' whose created_at is older than olderThan.
	PurgeOldActionRunOutputs(ctx context.Context, olderThan time.Time) (int, error)
}
