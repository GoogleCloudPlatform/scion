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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// Common errors returned by store implementations.
var (
	ErrNotFound         = errors.New("not found")
	ErrNotSingular      = errors.New("not singular") // query matched more than one row
	ErrAlreadyExists    = errors.New("already exists")
	ErrVersionConflict  = errors.New("version conflict")
	ErrInvalidInput     = errors.New("invalid input")
	ErrRevisionConflict = errors.New("revision conflict")
	ErrQuotaExceeded    = errors.New("quota exceeded")
	// ErrDeleteInProgress is returned by SetAgentRunID when a delete holds
	// the agent's row (see AgentStore.SetAgentRunID).
	ErrDeleteInProgress = errors.New("agent delete in progress")

	// ErrPhaseMismatch is returned by UpdateAgentStatus when
	// AgentStatusUpdate.IfPhase is set and the stored phase differs. It wraps
	// ErrVersionConflict, so callers and the HTTP error mapping treat it as a
	// conflict.
	ErrPhaseMismatch = fmt.Errorf("agent phase changed: %w", ErrVersionConflict)

	// ErrRunChanged is returned by UpdateAgentStatus when
	// AgentStatusUpdate.IfRunID is set and the stored run_id differs
	// (ptone/scion#2550): the row has moved to a newer run since the caller
	// dispatched its operation. It wraps ErrVersionConflict.
	ErrRunChanged = fmt.Errorf("agent run changed: %w", ErrVersionConflict)

	// ErrSuperAdminBindingRestricted is returned when a non-reconciler caller
	// attempts to create a role binding for the super-admin role definition.
	// Super-admin authority is conferred exclusively via User.Role (out-of-band)
	// and the system reconciler; the F1.5 role-binding machinery must not be a
	// second grant path.
	ErrSuperAdminBindingRestricted = errors.New("super-admin role bindings can only be created by the system reconciler")

	// ErrDirectUserOnly is returned when a role binding for a direct-user-only
	// role (super-admin, project-owner) is attempted with a non-user principal.
	ErrDirectUserOnly = errors.New("this role requires a direct user principal")

	// ErrProjectMembersGroupPrincipal is returned when a role binding or a
	// child-group edge would name a project members group (see
	// IsProjectMembersGroup). Project members groups are system-managed and
	// cannot be granted roles or nested in another group. It wraps
	// ErrInvalidInput so generic mappers report it as a bad request.
	ErrProjectMembersGroupPrincipal = fmt.Errorf("%w: project members groups cannot be role-binding principals or child groups", ErrInvalidInput)

	// ErrScopeMismatch is returned when a role binding's scope type does not
	// match the role definition's scope type.
	ErrScopeMismatch = errors.New("binding scope type does not match role definition scope type")

	// ErrBuiltInMembershipConflict is returned when a principal already has
	// a built-in membership role in the same project. The caller must delete
	// the existing membership binding before assigning a different built-in
	// membership role (change-role = delete + create).
	ErrBuiltInMembershipConflict = errors.New("principal already has a built-in membership role in this project")

	// ErrIdentityKeyConflict is returned when writing an agent's identity
	// keys (its slug and/or slugify(displayName)) would collide with a key
	// already reserved by a different agent in the same project, including a
	// soft-deleted one. Kept distinct from ErrAlreadyExists so callers can
	// map it to a specific message instead of a generic conflict.
	ErrIdentityKeyConflict = errors.New("identity key already reserved by another agent in this project")

	// ErrInvalidPhase is returned by BeginLaunch when the agent's current
	// phase is not eligible for the requested launch kind (design
	// t1-async-create-v11.md §3.3): in P1a, a create launch requires phase
	// in {created, provisioning}. Callers must never fall back to a
	// synchronous send on this error (design §3.4, NB-1); it means a
	// concurrent write (e.g. a stop) landed first.
	ErrInvalidPhase = errors.New("agent phase is not eligible for this launch")
)

// SystemReconcileCreatedBy is the CreatedBy sentinel that identifies the
// system reconciler as the caller of CreateRoleBinding. Only bindings
// with this CreatedBy value (or SystemBackfillCreatedBy) may target the
// super-admin role definition.
const SystemReconcileCreatedBy = "system-reconcile"

// SystemBackfillCreatedBy is the CreatedBy sentinel for the one-time
// startup backfill that migrates existing User.Role values into role bindings.
const SystemBackfillCreatedBy = "system-backfill"

// AdminAPICreatedBy is the CreatedBy sentinel that identifies super-admin
// role bindings created through the admin API (UI-granted promotions).
// These bindings are NOT touched by ReconcileSuperAdminBindings, making
// UI-promoted admins immune to AdminEmails config changes.
const AdminAPICreatedBy = "admin-api"

// IsSuperAdminBindingAllowed reports whether the given CreatedBy value is
// permitted to create a super-admin role binding. Only the system reconciler,
// the startup backfill, and the admin API are allowed.
func IsSuperAdminBindingAllowed(createdBy string) bool {
	return createdBy == SystemReconcileCreatedBy || createdBy == SystemBackfillCreatedBy || createdBy == AdminAPICreatedBy
}

// IsSystemCreatedBinding reports whether the given CreatedBy value identifies
// a binding created automatically by the system (backfill, reconcile, or
// admin API). Used by cleanup routines to distinguish automatic bindings from
// administrator-created ones.
func IsSystemCreatedBinding(createdBy string) bool {
	return createdBy == SystemReconcileCreatedBy || createdBy == SystemBackfillCreatedBy || createdBy == AdminAPICreatedBy
}

// Store defines the interface for Hub data persistence.
// Implementations may use SQLite, PostgreSQL, Firestore, or other backends.
type Store interface {
	// Close releases any resources held by the store.
	Close() error

	// Ping checks connectivity to the underlying database.
	Ping(ctx context.Context) error

	// Migrate applies any pending database migrations.
	Migrate(ctx context.Context) error

	// WithTx executes fn inside a database transaction. All store operations
	// performed via the Store passed to fn participate in the same transaction.
	// If fn returns nil the transaction is committed; otherwise it is rolled
	// back and the error is returned to the caller. Nested calls are
	// pass-through (the inner callback receives the same transactional store).
	WithTx(ctx context.Context, fn func(tx Store) error) error

	// FinalizeAgentDeletion is the delete engine's terminal write (design
	// ptone/scion#2483 §2.3), as ONE transaction: it reads the agent
	// (row-locked where supported), evaluates pred, and when it holds
	// either applies set (DeletionFinalizeSoft, with UpdateAgentDeletion's
	// semantics, Derive included) or removes the agent row and its cascade
	// (DeletionFinalizeHard, as DeleteAgent). Then, still inside the
	// transaction and just before commit, it calls hook (when non-nil) with
	// a transaction-scoped Store, the agent (the post-write row for soft,
	// the pre-delete row for hard) and the mode; a non-nil hook error rolls
	// the whole finalize back and is returned. Returns affected=0 with a nil
	// error when the predicate did not hold or the agent does not exist.
	// Must not be called from inside WithTx.
	FinalizeAgentDeletion(ctx context.Context, id string, pred DeletionPredicate, mode DeletionFinalizeMode, set DeletionFields, hook DeletionFinalizeHook) (affected int, err error)

	// Agent operations
	AgentStore

	// Project operations
	ProjectStore

	// RuntimeBroker operations
	RuntimeBrokerStore

	// BrokerDispatch operations (multi-node command dispatch)
	BrokerDispatchStore

	// Template operations
	TemplateStore

	// HarnessConfig operations
	HarnessConfigStore

	// User operations
	UserStore

	// UserTerminalWorkspace operations
	UserTerminalWorkspaceStore

	// ProjectProvider operations
	ProjectProviderStore

	// EnvVar operations
	EnvVarStore

	// Secret operations
	SecretStore

	// Group operations (Hub Permissions System)
	GroupStore

	// User Access Token operations
	UserAccessTokenStore

	// Broker Secret operations (Runtime Broker authentication)
	BrokerSecretStore

	// Notification operations (Agent Status Notification System)
	NotificationStore

	// ScheduledEvent operations (One-Shot Timers)
	ScheduledEventStore

	// Schedule operations (Recurring Schedules)
	ScheduleStore

	// GCP Service Account operations (GCP Identity for Agents)
	GCPServiceAccountStore

	// GitHub App Installation operations
	GitHubInstallationStore

	// Message operations (Bidirectional Human-Agent Messaging)
	MessageStore

	// Maintenance operations (Admin Maintenance Panel)
	MaintenanceStore

	// Project Sync State operations (Workspace Sync Metadata)
	ProjectSyncStateStore

	// Allow List operations (User Access Control)
	AllowListStore

	// Invite Code operations (User Invitation System)
	InviteCodeStore

	// LifecycleHook operations (Configurable Agent Lifecycle Hooks)
	LifecycleHookStore

	// Skill operations (Skill Bank)
	SkillStore

	// Skill Registry operations (Hub-to-Hub Federation)
	SkillRegistryStore

	// HubSetting operations (Two-Tier Settings Architecture)
	HubSettingStore

	// BrokerSetting operations (ptone/scion#2061 P2, ptone/scion#2177)
	BrokerSettingStore

	// SkillInjection operations (Injected-Skills List)
	SkillInjectionStore

	// ProjectPreStartHook operations (Project-Level Pre-Start Customization Scripts)
	ProjectPreStartHookStore

	// AgentSessionMetrics operations (Hub Metrics Reporting)
	AgentSessionMetricsStore

	// Conversation operations (Multi-Party Messaging)
	ConversationStore

	// Role operations (Permissions Foundation Phase 1E)
	RoleStore

	// Delegation Edge operations (Permissions Foundation Phase 1G)
	DelegationEdgeStore

	// Agent Credential operations (Permissions Foundation Phase 1H)
	AgentCredentialStore

	// Agent Identity Key operations (per-project display-name / slug uniqueness)
	AgentIdentityKeyStore

	// Decision Audit operations (Authorization Decision Audit Phase 1I)
	DecisionAuditStore

	// Mutation Audit operations (Authorization Mutation Audit Phase 1I)
	MutationAuditStore

	// Quota operations (Permissions Phase 2B — Limits/Quotas)
	QuotaStore

	// Access Constraint operations (AC1 — Operator Access Constraint Backend)
	AccessConstraintStore

	// External Identity operations (GE Google Credential Exchange)
	ExternalIdentityStore

	// Agent Reincarnation operations (agent-reincarnate, ptone/scion#1821)
	AgentReincarnationStore
}

// AgentStore defines agent-related persistence operations.
type AgentStore interface {
	// CreateAgent creates a new agent record.
	// Returns ErrAlreadyExists if an agent with the same ID exists.
	CreateAgent(ctx context.Context, agent *Agent) error

	// GetAgent retrieves an agent by ID.
	// Returns ErrNotFound if the agent doesn't exist.
	GetAgent(ctx context.Context, id string) (*Agent, error)

	// GetAgentBySlug retrieves an agent by its slug within a project.
	// Returns ErrNotFound if the agent doesn't exist.
	GetAgentBySlug(ctx context.Context, projectID, slug string) (*Agent, error)

	// GetAgentsByIDs retrieves agents by a list of IDs.
	// Returns only agents that exist; missing IDs are silently skipped.
	// The returned map is keyed by agent ID.
	GetAgentsByIDs(ctx context.Context, ids []string) (map[string]*Agent, error)

	// GetAgentsByIDsIncludingDeleted is GetAgentsByIDs without the
	// soft-delete filter: like GetAgent, it also returns agents whose
	// DeletedAt is set. Missing or malformed IDs are silently skipped.
	GetAgentsByIDsIncludingDeleted(ctx context.Context, ids []string) (map[string]*Agent, error)

	// UpdateAgent updates an existing agent.
	// Uses optimistic locking via StateVersion.
	// Returns ErrNotFound if agent doesn't exist.
	// Returns ErrVersionConflict if the version doesn't match.
	// It never writes soft_delete_op_id (see SetAgentSoftDeleteOpID).
	UpdateAgent(ctx context.Context, agent *Agent) error

	// SetAgentSoftDeleteOpID sets the agent's soft_delete_op_id to opID, or
	// clears it to NULL when opID is empty. It is the only writer of that
	// column: UpdateAgent and CreateAgent leave it untouched. It does not
	// check or bump state_version; the lifecycle transactions call it in the
	// same transaction as the guarded row write it belongs to.
	// Returns ErrNotFound if the agent doesn't exist.
	SetAgentSoftDeleteOpID(ctx context.Context, agentID, opID string) error

	// DeleteAgent removes an agent by ID.
	// Returns ErrNotFound if the agent doesn't exist.
	DeleteAgent(ctx context.Context, id string) error

	// ListAgents returns agents matching the filter criteria. With
	// opts.SortBy empty this is the legacy path (ORDER BY created DESC, id
	// DESC, opts.Cursor/CursorBinding decoded via the legacy cursor codec).
	// With opts.SortBy set to "created" or "updated", this is the real-SQL
	// sorted-mode total order: a keyset WHERE predicate bound to
	// opts.SortCursor (page 0 when nil), a limit+1 probe for NextCursor
	// exactly like the legacy path, and NextCursor minted as a v2 cursor via
	// EncodeAgentCursor using opts.CursorBinding. An unrecognized
	// opts.SortBy or opts.SortDir, or a non-empty legacy opts.Cursor, fails
	// closed with ErrInvalidInput. Used directly by the global endpoint's
	// sorted mode, which has no per-row read filter and so needs no narrow
	// AgentMember projection — unlike the project endpoint's
	// candidate-bounded ListAgentMembers above.
	ListAgents(ctx context.Context, filter AgentFilter, opts ListOptions) (*ListResult[Agent], error)

	// CountAgents returns the number of agents matching filter, applying the
	// exact predicate ListAgents does for its total count, but without
	// loading any rows. Used by the project sorted-mode candidate ceiling
	// as a cheap pre-check before any member row is read.
	CountAgents(ctx context.Context, filter AgentFilter) (int, error)

	// ListAgentMembers returns up to max agents matching filter, in the
	// sorted-mode total order for (sort, dir): sort="updated" orders by
	// COALESCE(last_activity_event, updated) dir, then created DESC, id
	// DESC; sort="created" orders by created dir, then id DESC. Each row
	// carries exactly the fields agentResource (pkg/hub/capabilities.go)
	// reads, plus Phase/Created/Updated/LastActivityEvent for positioning
	// and stats — see AgentMember.
	//
	// max bounds the read so a candidate pool that grew past the caller's
	// ceiling check is still detected by the member read: when the true
	// candidate count exceeds max, exactly max rows are returned (the exact
	// order among untaken rows is unspecified in that case, since the
	// caller's only use of an over-max result is to refuse the request).
	ListAgentMembers(ctx context.Context, filter AgentFilter, sort, dir string, max int) ([]AgentMember, error)

	// CountAgentsByPhaseIDs returns the id and phase of every agent matching
	// filter, with no per-row authorization decision: it backs the global
	// endpoint's sorted-mode "stats" population. Callers pass filter with
	// Phase cleared, matching CountAgents/ListAgentMembers' "stats"
	// convention.
	CountAgentsByPhaseIDs(ctx context.Context, filter AgentFilter) ([]IDPhase, error)

	// ListAgentsWithStaleNonTerminalReincarnationState returns every agent
	// whose reincarnation_state is non-terminal and whose row has not been
	// updated since before olderThan. Backstop for the replica-safe
	// reincarnation sweep (design §3.7): catches an agent left claimed with
	// no matching non-terminal AgentReincarnation record for the main sweep
	// to find (e.g. the record was deleted).
	ListAgentsWithStaleNonTerminalReincarnationState(ctx context.Context, olderThan time.Time) ([]*Agent, error)

	// SetAgentRunID records runID as the agent's current run identity
	// (ptone/scion#2550) and returns the run_id the row held immediately
	// before the write, so a dispatch can later revert to exactly that
	// value. It is a narrow single-column write: it does not check or bump
	// state_version, so it neither conflicts with nor invalidates a
	// concurrent UpdateAgent, and UpdateAgent never writes run_id back.
	// Returns ErrNotFound if the agent doesn't exist.
	//
	// It refuses with ErrDeleteInProgress, writing nothing, when the row is
	// soft-deleted or a delete holds it (finalizing, or deleting under a live
	// lease; the start gate's rule). The check is part of the write, so a
	// delete claim and a run-ID write are ordered by the database: a claim
	// that lands first refuses the write, and a claim that lands after it
	// snapshots the new run ID.
	//
	// The same write appends the replaced run to the row's PreviousRunIDs
	// (AppendPreviousRunID, ptone/scion#3097), so a delete still names it
	// until the new run settles.
	SetAgentRunID(ctx context.Context, agentID, runID string) (previous string, err error)

	// CompareAndSwapAgentRunID sets the agent's run_id to newRunID only if
	// it currently equals expectedRunID, and reports whether it did. A
	// dispatch uses it to correct (or revert) the run ID it minted without
	// overwriting a newer run ID a later dispatch has since recorded. Like
	// SetAgentRunID it neither checks nor bumps state_version. A missing
	// agent reports false with no error. A swap also clears the row's
	// PreviousRunIDs: callers use it only to settle the run (the broker
	// reported the run its runtime holds, or a start landed and so replaced
	// every entry of the name), so no other run's entry is left to delete.
	CompareAndSwapAgentRunID(ctx context.Context, agentID, expectedRunID, newRunID string) (bool, error)

	// RevertAgentRunID is CompareAndSwapAgentRunID (mintedRunID to
	// previousRunID) for a dispatch the broker never acted on, and leaves
	// PreviousRunIDs untouched (ptone/scion#3097): the restored run may
	// itself be unsettled, so the runs listed before the dispatch may still
	// have entries. The list then holds the restored run too, which is
	// harmless: a delete skips the current run, and AppendPreviousRunID
	// drops it.
	RevertAgentRunID(ctx context.Context, agentID, mintedRunID, previousRunID string) (bool, error)

	// UpdateAgentStatus updates only status-related fields.
	// This is a partial update that doesn't require version checking.
	UpdateAgentStatus(ctx context.Context, id string, status AgentStatusUpdate) error

	// SetAgentWorkspacePlacement records where the agent's broker placed its
	// workspace on the latest start (see Agent.WorkspacePlacement). It is a
	// narrow single-column write that neither checks nor bumps
	// state_version. Returns ErrNotFound if the agent doesn't exist or is
	// soft-deleted (a soft-deleted row is never written).
	SetAgentWorkspacePlacement(ctx context.Context, agentID, placement string) error

	// UpdateAgentExposedPorts updates only exposed port registrations.
	UpdateAgentExposedPorts(ctx context.Context, id string, ports []ExposedPort) error

	// PurgeDeletedAgents permanently removes soft-deleted agents older than cutoff.
	// Returns the number of agents purged.
	PurgeDeletedAgents(ctx context.Context, cutoff time.Time) (int, error)

	// MarkStaleAgentsOffline marks agents whose last heartbeat is older
	// than threshold as offline. Only affects agents with phase=running whose
	// activity is not a terminal sticky state (completed, limits_exceeded).
	// Returns the updated agent records for event publishing.
	MarkStaleAgentsOffline(ctx context.Context, threshold time.Time) ([]Agent, error)

	// MarkStalledAgents marks running agents as stalled when their last activity
	// event is older than activityThreshold but they have a recent heartbeat
	// (last_seen >= heartbeatRecency). Only affects agents with phase=running
	// whose activity is not a terminal sticky state or already stalled/offline.
	// Returns the updated agent records for event publishing.
	MarkStalledAgents(ctx context.Context, activityThreshold, heartbeatRecency time.Time) ([]Agent, error)

	// MarkAgentContainerMissing moves a running agent whose container its
	// runtime broker no longer reports to phase=error with exit reason
	// container_missing. An exit reason that is more specific about why the
	// container went away (preempted, evicted) is kept, with its message and
	// exit code. The write is conditional (every check is in the
	// UPDATE's WHERE clause): the agent must still exist and not be
	// soft-deleted, still be assigned to brokerID, still be in phase running,
	// have no reincarnation in flight, and not have been seen (last_seen) at
	// or after cutoff. When any check fails it changes nothing and returns
	// (nil, nil), so a concurrent start,
	// restart, stop or heartbeat always wins. On success it returns the
	// updated record for event publishing.
	MarkAgentContainerMissing(ctx context.Context, id, brokerID string, cutoff time.Time, message string) (*Agent, error)

	// ClearAgentRuntimeTarget removes the runtime target and any runtime
	// target candidate recorded in an agent's applied config
	// (AgentAppliedConfig.RuntimeTarget, RuntimeTargetCandidate) and changes
	// no other applied-config key. When it removes something it bumps
	// state_version by exactly one, so a writer holding a read from before
	// the clear gets ErrVersionConflict from UpdateAgent instead of writing
	// the old target back; it returns cleared=true and the new state_version.
	// The write is conditional on the stored applied config and state_version
	// being unchanged since they were read (and the row is locked for the
	// read where supported); when a concurrent write wins, the row is re-read
	// and the clear retried a bounded number of times, after which an error
	// is returned. It is a no-op (cleared=false) when neither key is recorded
	// or the agent does not exist.
	ClearAgentRuntimeTarget(ctx context.Context, id string) (cleared bool, newVersion int64, err error)

	// SetAgentRuntimeTarget sets the runtime target and runtime target
	// candidate in an agent's applied config (AgentAppliedConfig.RuntimeTarget,
	// RuntimeTargetCandidate; an empty value removes the key) and changes
	// nothing else: no other applied-config key, no other column, and not
	// state_version. The write is conditional on state_version still being
	// expectedVersion (the version the caller read), so a report based on a
	// read from before ClearAgentRuntimeTarget, which bumps state_version,
	// is rejected. It returns written=false, with no error, when the version
	// no longer matches, the applied config changed concurrently, or the
	// agent does not exist.
	SetAgentRuntimeTarget(ctx context.Context, id string, expectedVersion int64, target, candidate string) (written bool, err error)

	// FindOrphanedAgents returns agents whose RuntimeBrokerID references a broker
	// that is offline or does not exist, and who are not in terminal states
	// (stopped, error). Agents assigned to the given currentBrokerID are excluded.
	// This guards against reassigning agents that belong to active remote brokers
	// in multi-broker setups.
	FindOrphanedAgents(ctx context.Context, currentBrokerID string) ([]*Agent, error)

	// ReassignAgentsToBroker bulk-updates the RuntimeBrokerID of the given agents
	// to the specified broker ID. Returns the number of agents updated.
	ReassignAgentsToBroker(ctx context.Context, agents []*Agent, brokerID string) (int, error)

	// ReassignProjectBroker updates projects whose DefaultRuntimeBrokerID
	// matches oldBrokerID to point to newBrokerID. Only projects whose current
	// default broker is offline or missing are updated — projects pointing to
	// active remote brokers are left untouched. Returns the number of projects
	// updated.
	ReassignProjectBroker(ctx context.Context, oldBrokerID, newBrokerID string) (int, error)

	// SetAgentPinnedRuntimeTarget compare-and-sets an agent's placement:
	// runtime_broker_id must equal expected.RuntimeBrokerID ("" also matches
	// NULL) and the pin columns must match the rest of expected (NULL when
	// expected.RuntimeTargetID is ""). It writes runtime_broker_id and the
	// pin from next and bumps state_version in the same conditional update,
	// so a stale UpdateAgent conflicts instead of reverting the move. next
	// must be complete (it never unpins): ErrInvalidPinnedPlacement. A miss
	// returns ErrPinnedPlacementChanged; a missing agent ErrNotFound.
	SetAgentPinnedRuntimeTarget(ctx context.Context, agentID string, expected, next PinnedPlacement) (*Agent, error)

	// AggregateAgentHealth returns lightweight health-oriented counts and lists
	// for the health-summary endpoint without fetching full agent records.
	// This avoids the O(N) deserialization cost of ListAgents on large installations.
	AggregateAgentHealth(ctx context.Context) (*AgentHealthAggregate, error)

	// --- T1 async agent create (design t1-async-create-v11.md §3.3) ---
	// These are the only writers of the launch_* columns. Each runs as a
	// single row-locked transaction. See launch.go for the supporting types.
	//
	// Each of these methods opens its own transaction and must not be called
	// from inside WithTx: the entadapter implementation cannot nest a second
	// transaction inside the ambient one WithTx provides, and returns an
	// error (or, for RunLaunchReaperTick, ReaperTickUnavailable) instead.

	// BeginLaunch starts a new launch for agentID. A create start claim on the
	// row is linked to the new launch (see SettleEndedLaunchClaim). The caller must start its
	// monotonic remaining-budget timer BEFORE calling this (§3.4). Any
	// previous active launch on the row becomes implicitly superseded (its ID
	// no longer matches launch_id). kind must be LaunchKindCreate in P1a;
	// any other value returns an error wrapping ErrInvalidInput (start and
	// restart are P6, design §3.13). Returns ErrInvalidPhase when the
	// agent's current phase is not eligible for kind (P1a: phase must be
	// created or provisioning). Returns ErrNotFound if the agent doesn't
	// exist.
	BeginLaunch(ctx context.Context, agentID, kind string, timeout time.Duration) (launchID string, err error)

	// MarkLaunchAccepted records that a broker (owner) has claimed launchID:
	// if owner is currently empty on the row, sets it, and if phase is
	// "created", moves it to "provisioning". Returns ErrNotFound if the
	// agent doesn't exist. Otherwise never returns an error for a stale
	// caller: if the row's launch_id no longer matches launchID (superseded)
	// or the launch has already ended, this is a no-op that returns the
	// current row unchanged, with a nil error — callers detect that case by
	// comparing the returned Agent's LaunchID/LaunchState against the
	// launchID they hold. Does not bump state_version.
	MarkLaunchAccepted(ctx context.Context, agentID, launchID, owner string) (Agent, error)

	// EndLaunch ends launchID with the given reason if it is still the
	// agent's current, active launch. No-op if launch_id no longer matches or
	// the launch has already ended. Does not bump state_version. Used by the
	// dispatcher's not_launched cases (design §3.4).
	EndLaunch(ctx context.Context, agentID, launchID, reason string) error

	// ApplyLaunchReport evaluates a broker's launch report against the
	// current row and applies the resulting state transition, per the
	// ordered rule list in design §3.7. Bumps state_version only on a
	// terminal apply (succeeded/failed) or a reaper-refining apply.
	ApplyLaunchReport(ctx context.Context, agentID, brokerID string, r LaunchReport) (LaunchReportAnswer, Agent, error)

	// RunLaunchReaperTick runs one tick of the launch deadline/staleness
	// reaper (design §3.7): a single hand-built transaction holding the
	// transaction-scoped advisory lock store.LockAgentLaunchDeadline, which
	// arms/disarms cluster-wide staleness detection and reaps overdue
	// launches. Synchronous, bounded by a 10s internal timeout; safe to call
	// from a 15s ticker on every replica.
	RunLaunchReaperTick(ctx context.Context, p ReaperParams) (ReaperTickResult, error)

	// --- Backend-driven agent delete (design ptone/scion#2483 §2.1) ---

	// UpdateAgentDeletion is the only writer of the deletion_* columns. In
	// one transaction (row-locked where the dialect supports it) it reads
	// the agent, evaluates pred against that row (see
	// DeletionPredicate.Matches), and, when it holds, applies set and bumps
	// state_version, so a stale whole-row UpdateAgent gets ErrVersionConflict
	// instead of clobbering phase or deletion fields. Returns affected=1 when
	// it wrote, and affected=0 with a nil error when the predicate did not
	// hold or the agent does not exist. Must not be called from inside
	// WithTx.
	UpdateAgentDeletion(ctx context.Context, id string, pred DeletionPredicate, set DeletionFields) (affected int, err error)

	// --- Run intent (see run_intent.go) ---
	// These, plus BackfillRunIntent, are the only writers of run_intent and
	// run_intent_at. None of them bumps state_version, and UpdateAgent and
	// CreateAgent never write either column. Like the launch methods, each
	// opens its own transaction and must not be called from inside WithTx.

	// SetRunIntent records the desired run state of agentID. The time is read
	// from the store clock inside the write transaction, under the row lock,
	// and is max(now, stored run_intent_at + 1µs), so successive writes to a
	// row are strictly ordered by run_intent_at even if the clock steps
	// backwards. Returns the stored time. Returns ErrNotFound if the agent
	// doesn't exist and ErrInvalidInput for an unknown intent.
	SetRunIntent(ctx context.Context, agentID string, intent RunIntent) (time.Time, error)

	// SwapRunIntent is SetRunIntent that also returns the intent the row
	// held before the write ("" for none), read under the same row lock.
	//
	// SetRunIntent and SwapRunIntent return ErrDeleteInProgress, and write
	// nothing, for RunIntentRunning on a row a delete holds
	// (DeletionHoldsRow) or a soft-deleted row (ptone/scion#2550).
	SwapRunIntent(ctx context.Context, agentID string, intent RunIntent) (prior RunIntent, at time.Time, err error)

	// RevertRunIntent sets run_intent to `to` only if the row still holds
	// `from` written at exactly fromAt (the value SetRunIntent returned);
	// run_intent_at is left unchanged. It reports whether the row changed.
	// Used by system-initiated stops whose dispatch failed. A revert to
	// RunIntentRunning on a row a delete holds (DeletionHoldsRow) or a
	// soft-deleted row writes nothing and reports false (ptone/scion#2550).
	RevertRunIntent(ctx context.Context, agentID string, from RunIntent, fromAt time.Time, to RunIntent) (bool, error)

	// BackfillRunIntent sets run_intent for every agent whose run_intent is
	// NULL: running for phase running or starting, stopped otherwise. It
	// returns the number of rows written. Idempotent.
	BackfillRunIntent(ctx context.Context) (int, error)

	// --- Start claim (see start_claim.go) ---
	// These, plus a create launch's terminal write, are the only writers of
	// the start_claim_* columns. None of them bumps state_version, and
	// UpdateAgent and CreateAgent never write the columns. Every time is read
	// from the store clock inside the method's transaction, under the row
	// lock; callers pass durations only. Each opens its own transaction and
	// must not be called from inside WithTx.

	// ClaimAgentStart takes a live claim of a start kind (any kind except
	// StartClaimStop) for agentID, owned by owner, with a lease of ttl, and
	// records run intent running in the same write (run_intent_at strictly
	// increasing, as SetRunIntent). It requires that no claim is held, the
	// agent is not deleted or being deleted, and no reincarnation is in
	// flight. Returns ErrDeleteInProgress when a delete holds the row or the
	// agent is soft-deleted (as SwapRunIntent does for a running intent),
	// *ClaimHeldError when a claim is held, ErrClaimPredicate when the agent
	// is otherwise not eligible, and ErrNotFound when it does not exist.
	ClaimAgentStart(ctx context.Context, agentID, owner string, kind StartClaimKind, target string, ttl time.Duration) (StartClaim, error)

	// ClaimAgentStop takes a live claim of kind StartClaimStop, used while a
	// queued stop recorded at intentAt is applied. It requires that no claim
	// is held and that run intent is still stopped at exactly intentAt; it
	// does not change run intent. Errors as ClaimAgentStart.
	ClaimAgentStop(ctx context.Context, agentID, owner string, intentAt time.Time, ttl time.Duration) (StartClaim, error)

	// RenewAgentStart extends a live claim's lease to now+ttl. It applies
	// only when the row still holds claimID, owned by owner, live, with an
	// unexpired lease. held=false (no error) means the claim is lost. An
	// error means the outcome is unknown.
	RenewAgentStart(ctx context.Context, agentID, claimID, owner string, ttl time.Duration) (held bool, err error)

	// MarkStartUnconfirmed moves a live claim to unconfirmed with a hold of
	// hold, under the same predicate as RenewAgentStart.
	MarkStartUnconfirmed(ctx context.Context, agentID, claimID, owner string, hold time.Duration) (held bool, err error)

	// ReleaseAgentStart clears a live claim, under the same predicate as
	// RenewAgentStart.
	ReleaseAgentStart(ctx context.Context, agentID, claimID, owner string) (held bool, err error)

	// ReleaseSupersededStart clears claimID, in any state and of any kind,
	// when run intent is stopped at exactly stopIntentAt: a stop accepted at
	// stopIntentAt superseded it. A claim taken after that stop wrote a newer
	// run_intent_at, so an older stop never releases it.
	ReleaseSupersededStart(ctx context.Context, agentID, claimID string, stopIntentAt time.Time) (released bool, err error)

	// DemoteExpiredStartClaim moves claimID from live to unconfirmed, with
	// the kind's hold from holds, when its lease has expired on the store
	// clock and the agent has no active launch. A renew that commits first
	// wins.
	DemoteExpiredStartClaim(ctx context.Context, agentID, claimID string, holds StartClaimHolds) (demoted bool, err error)

	// DemoteOwnerStartClaims moves every live claim whose owner starts with
	// ownerPrefix to unconfirmed, whatever its lease, except on agents with
	// an active launch. Used when a hub replica restarts under the same pod
	// name: claims owned by its previous process can no longer be renewed.
	// excludeOwner (this process's own owner value) is never demoted.
	DemoteOwnerStartClaims(ctx context.Context, ownerPrefix, excludeOwner string, holds StartClaimHolds) (int, error)

	// ConvertUnconfirmedToStop replaces the unconfirmed claim claimID with a
	// live stop-kind claim owned by owner, with a lease of ttl, when the agent
	// has no active launch. It is taken before the hub stops a container an
	// unconfirmed start left running, so no start can claim the agent while
	// that stop is applied. ErrClaimPredicate when the claim changed.
	ConvertUnconfirmedToStop(ctx context.Context, agentID, claimID, owner string, ttl time.Duration) (StartClaim, error)

	// ReleaseUnconfirmedStart clears claimID when it is unconfirmed and the
	// agent has no active launch.
	ReleaseUnconfirmedStart(ctx context.Context, agentID, claimID string) (released bool, err error)

	// SettleEndedLaunchClaim applies LaunchEndClaimSettlement to a create
	// claim (claimID) linked to the agent's launch (the launch the claim was
	// linked to at BeginLaunch), when that launch has ended without settling
	// it. No-op when the launch is active, the claim changed, the claim is
	// not linked to the agent's current launch, or the end reason settles
	// nothing. A claim taken after a launch ended is never linked to it.
	SettleEndedLaunchClaim(ctx context.Context, agentID, claimID string) (changed bool, err error)

	// ListAgentsWithStartClaim returns every non-deleted agent that holds a
	// start claim.
	ListAgentsWithStartClaim(ctx context.Context) ([]*Agent, error)

	// StoreClock reads the store clock: Postgres now(), or the single SQLite
	// process's clock. The columns written on this clock are run_intent_at,
	// every start_claim_* time, agent_recovery observed_at and
	// first_absent_at, broker_target_inventory last_complete_inventory_at
	// and the launch_* times; comparisons against them (hold expiry,
	// observation freshness) use it. Broker last_heartbeat and connected_at
	// are written on the hub process clock instead, so on Postgres they are
	// not comparable with store-clock times.
	StoreClock(ctx context.Context) (time.Time, error)

	// ClaimAgentReincarnation records a reincarnation as pending (state
	// pending, reincarnation_updated_at=at) and bumps state_version, when the
	// row is still at expectedVersion, holds no start claim, and has no
	// reincarnation in flight. Returns the new state_version.
	// ErrVersionConflict when the version moved, *ClaimHeldError when a start
	// claim is held, ErrClaimPredicate when a reincarnation is in flight.
	ClaimAgentReincarnation(ctx context.Context, agentID string, expectedVersion int64, at time.Time) (int64, error)

	// --- Runtime observations (see start_claim.go) ---

	// RecordRecoveryObservations writes one heartbeat's observations in one
	// transaction: it reads the store clock, upserts each observation whose
	// target is in completeTargets (observations of other targets are
	// ignored), keeping first_absent_at across consecutive absent
	// observations, and upserts broker_target_inventory for each complete
	// target. Returns the store-clock time written.
	RecordRecoveryObservations(ctx context.Context, brokerID string, completeTargets []string, obs []RecoveryObservation) (time.Time, error)

	// GetRecoveryObservations returns the stored observations of the given
	// agents, keyed by agent ID. Agents with none are absent from the map.
	GetRecoveryObservations(ctx context.Context, agentIDs []string) (map[string]RecoveryObservationRecord, error)

	// ListBrokerTargetInventory returns a broker's per-target complete
	// inventory times.
	ListBrokerTargetInventory(ctx context.Context, brokerID string) ([]BrokerTargetInventory, error)
}

// AgentFilter defines criteria for filtering agents.
type AgentFilter struct {
	ProjectID       string
	RuntimeBrokerID string
	Phase           string
	OwnerID         string
	IncludeDeleted  bool // If true, include soft-deleted agents in results

	// OrRunIntent, when non-empty, widens Phase: an agent matches when its
	// phase equals Phase OR its run_intent equals OrRunIntent. When Phase is
	// empty it filters on run_intent alone. omitempty keeps list cursor
	// bindings (a hash of the encoded filter) unchanged when it is unset.
	OrRunIntent string `json:",omitempty"`

	// MemberOrOwnerProjectIDs, when non-empty, restricts results to agents
	// whose project_id is in this set OR whose owner_id matches OwnerID.
	// OwnerID and this field are combined with OR (not AND) when both are set.
	MemberOrOwnerProjectIDs []string

	// MemberProjectIDs, when non-empty, restricts results to agents whose
	// project_id is in this set. Unlike MemberOrOwnerProjectIDs, this is NOT
	// combined with OwnerID — it filters strictly by project ID membership.
	MemberProjectIDs []string

	// ExcludeOwnerID, when non-empty, excludes agents whose owner_id matches
	// this value. Used with MemberProjectIDs to return "shared" agents (in a
	// member project but not personally created).
	ExcludeOwnerID string

	// AncestorID, when non-empty, restricts results to agents whose ancestry
	// chain contains the given principal ID (transitive access via creation lineage).
	AncestorID string

	// Labels, when non-empty, restricts results to agents whose labels
	// contain all specified key-value pairs (AND semantics).
	Labels map[string]string

	// AuthorizedProjectIDs, when non-nil, restricts results to agents whose
	// project_id is in this set. This is used by scope-aware list authorization
	// to push the resolved ScopeSet into the SQL query so pagination and totals
	// reflect only the authorized set. A nil value means no authorization
	// filtering. An empty non-nil slice means no agents are authorized.
	AuthorizedProjectIDs []string

	// ExcludedProjectIDs, when non-nil, excludes agents whose project_id is in
	// this set from the result. RS2: used when a system-wide admin has project-
	// scoped constraints that block the list permission for specific projects.
	// An empty or nil slice means no exclusions.
	ExcludedProjectIDs []string

	// RequestedOwnerID, when non-empty, restricts results to agents whose
	// owner_id matches this value. Unlike OwnerID (which participates in the
	// OR-based Mine/Shared classification via MemberOrOwnerProjectIDs), this
	// field is always combined with every other filter using AND. It exists
	// so an explicit caller-supplied owner filter (e.g. CLI `--owner`, hub
	// query param `ownerId`) can never be folded into the classification OR
	// clause and widen results beyond "agents owned by exactly this
	// principal" (ptone/scion#2146).
	RequestedOwnerID string

	// HarnessConfig, when non-empty, restricts results to agents whose
	// resolved AppliedConfig.HarnessConfig equals this value. The Ent
	// adapter backs this with a dedicated, plain-equality column
	// (harness_config, pkg/ent/schema/agent.go) kept in sync with
	// AppliedConfig.HarnessConfig on every write, rather than parsing or
	// pattern-matching AppliedConfig's JSON at query time (ptone/scion#2146).
	HarnessConfig string

	// IDs, when non-nil, restricts results to agents whose ID is in this set.
	// Always combined with every other filter (including AuthorizedProjectIDs)
	// using AND — it narrows, it never substitutes for authorization. A nil
	// value means no restriction. An empty non-nil slice means no agents
	// match (fail closed, mirroring AuthorizedProjectIDs).
	//
	// This backs relationship queries such as CLI `--ancestors`, where the
	// caller supplies a set of IDs found in another agent's Ancestry chain.
	// Some of those IDs may name users rather than agents (Ancestry mixes
	// both); those simply match no row here, which is how "skip entries that
	// are users" falls out without extra bookkeeping. It is deliberately NOT
	// implemented by fetching each ID individually — doing so would bypass
	// whatever authorization predicate (AuthorizedProjectIDs, etc.) the
	// caller composed this filter with, and future relationship-based
	// visibility (ptone/scion#2128) needs a single choke point to widen.
	IDs []string

	// LineageRootID, when non-empty, restricts results to the agent whose ID
	// equals this value OR whose Ancestry chain contains it — i.e. the root
	// principal plus every agent descended from it, at any depth. It is an
	// internal OR of two sub-conditions, but that OR is itself ANDed with
	// every other filter (including AuthorizedProjectIDs), the same
	// composition pattern already used for MemberOrOwnerProjectIDs above:
	// the OR only decides which rows count as "in the root's lineage", it
	// never widens past the authorization predicate. A root the caller is
	// not authorized to see simply yields no matches, not an error or a
	// disclosure.
	//
	// Backs CLI `--lineage`, which is a CREATION-TREE query, not a
	// messaging-permission query — it says nothing about who the reference
	// agent may message under any message mode (scion set-message-mode),
	// which can be a different, smaller set. The root ID this field is set
	// to is computed client-side (see resolveLineageRootID in cmd/list.go):
	// the reference's direct parent, or the reference itself when it has no
	// parent at all (an empty Ancestry), when its parent is a user rather
	// than an agent, or when its only recorded parent is an agent the
	// caller cannot list. The latter two both root at self because the
	// caller cannot tell them apart; a length-1 parent the caller *can* list
	// roots at that parent.
	//
	// "Parent is a user" cannot be decided from len(Ancestry) alone: a
	// child can inherit a length-1, agent-only Ancestry from a creator
	// whose own Ancestry was itself empty (see resolveLineageRootID's doc
	// for exactly when this happens). Determining "is the parent a user"
	// therefore requires resolving a length-1 Ancestry entry through the
	// caller's authorized list (never a bare per-ID fetch) rather than a
	// purely local length check; an Ancestry of two or more entries never
	// needs this, since its last entry is always an agent ID by
	// construction. The root ID handed to this field may therefore be a
	// USER principal ID, not only an agent ID — the OR predicate treats
	// either the same way, since IDEQ simply never matches a user ID and
	// ancestryContains still finds that user's descendants.
	LineageRootID string
}

// AgentMember is the narrow projection ListAgentMembers reads for
// sorted-mode candidate evaluation. It carries exactly the fields pkg/hub's
// agentResource(*Agent) reads — ID, OwnerID, ProjectID, Labels, Ancestry —
// plus Phase, Created, Updated and LastActivityEvent for positioning
// (pkg/store/agentsort) and stats.
//
// The projection is defined as "exactly the fields agentResource reads", not
// as an independently maintained field list: ToAgent is the single
// construction path a caller must use to build a Resource from a member, so
// that a future agentResource input agentResource gains but AgentMember
// lacks is caught by the equality gate described on ToAgent, rather than
// silently widening what the hub's race re-check can miss (the hub-side
// non-waivable member/full equality test is the gate that exercises this).
type AgentMember struct {
	ID        string
	OwnerID   string
	ProjectID string
	Labels    map[string]string
	Ancestry  []string

	Phase             string
	Created           time.Time
	Updated           time.Time
	LastActivityEvent time.Time
}

// ToAgent copies m's fields into a new Agent, leaving every other field
// zero. It is the one construction path for building an authorization
// Resource from a member row: callers must derive it as
// agentResource(m.ToAgent()), never by hand-listing AgentMember's fields, so
// that comparing that Resource against agentResource(fullRow) in the hub's
// race re-check actually proves the two rows agree on every input the kernel
// reads, not just the ones some earlier author remembered to copy here.
func (m AgentMember) ToAgent() *Agent {
	return &Agent{
		ID:                m.ID,
		OwnerID:           m.OwnerID,
		ProjectID:         m.ProjectID,
		Labels:            m.Labels,
		Ancestry:          m.Ancestry,
		Phase:             m.Phase,
		Created:           m.Created,
		Updated:           m.Updated,
		LastActivityEvent: m.LastActivityEvent,
	}
}

// IDPhase is one row of CountAgentsByPhaseIDs' narrow projection: an agent id
// and its phase, with no other column read.
type IDPhase struct {
	ID    string
	Phase string
}

// AgentHealthAggregate holds pre-computed counts and short lists used by the
// health-summary endpoint. It avoids loading full agent records.
type AgentHealthAggregate struct {
	Total   int            // Total number of non-deleted agents
	ByPhase map[string]int // Count per lifecycle phase

	// Per-broker counts: map[brokerID] → {count, healthy}
	ByBroker map[string]AgentBrokerCounts

	// Names of agents in unhealthy states (capped at 100 per list).
	StalledNames []string
	CrashedNames []string
	ErroredNames []string
}

// AgentBrokerCounts holds per-broker agent tallies.
type AgentBrokerCounts struct {
	Count   int
	Healthy int
}

// AgentStatusUpdate contains fields for status-only updates.
type AgentStatusUpdate struct {
	Phase           string            `json:"phase,omitempty"`
	Activity        string            `json:"activity,omitempty"`
	ToolName        string            `json:"toolName,omitempty"`
	Message         string            `json:"message,omitempty"`
	ConnectionState string            `json:"connectionState,omitempty"`
	ContainerStatus string            `json:"containerStatus,omitempty"`
	RuntimeState    string            `json:"runtimeState,omitempty"`
	TaskSummary     string            `json:"taskSummary,omitempty"`
	Heartbeat       bool              `json:"heartbeat,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`

	// Limits tracking (reported by sciontool)
	CurrentTurns      *int   `json:"currentTurns,omitempty"`
	CurrentModelCalls *int   `json:"currentModelCalls,omitempty"`
	StartedAt         string `json:"startedAt,omitempty"`

	// Exit tracking
	ExitCode   *int   `json:"exitCode,omitempty"`
	ExitReason string `json:"exitReason,omitempty"`
	// ClearExit, when true, clears ExitCode and ExitReason on this update —
	// independent of any phase-transition-gated clear, since a lifecycle
	// start/restart dispatch is a fresh generation of the agent regardless
	// of its current phase: an exit reason recorded against the agent's
	// prior generation (for example state.ExitReasonPreempted/ExitReasonEvicted,
	// which can be set on a still-running agent ahead of its pod actually
	// stopping) describes the old pod, not the one the start/restart is
	// bringing up. Internal to the lifecycle dispatch path — json:"-" so a
	// status report from an external caller (an agent, or a user with
	// update access) cannot set it through the wire API.
	ClearExit bool `json:"-"`
	// ClearMessageIf, when non-empty and Message is empty, clears the
	// agent's message only if it still equals this value. A lifecycle path
	// that set a transient notice uses it to retire that notice without
	// overwriting a newer message. Internal to the hub — json:"-".
	ClearMessageIf string `json:"-"`
	// ClearTerminalRemnants, when true, applies the clear a stopped/error ->
	// running write gets whatever the stored phase is: the message (unless
	// Message is set on this update), the stalled marker, and the exit code
	// and reason. A lifecycle start, restart or wake brings up a new
	// generation of the agent, and its write after a successful dispatch
	// uses this (the HTTP start/restart final write; the wake's
	// post-dispatch starting write) because the row no longer reads
	// stopped/error by then: beginStartDispatch writes starting first, and a
	// heartbeat guarded during the dispatch may have stored the old
	// container's exit message. Internal to the hub — json:"-".
	ClearTerminalRemnants bool `json:"-"`
	// IfPhase, when non-empty, makes the update conditional: it applies only
	// if the stored phase equals IfPhase, checked on the row read inside the
	// update's transaction; otherwise UpdateAgentStatus returns
	// ErrPhaseMismatch and writes nothing. Internal to the hub — json:"-".
	IfPhase string `json:"-"`
	// IfRunID, when non-empty, makes the update conditional on the stored
	// run_id equalling IfRunID, checked on the row read inside the update's
	// transaction; otherwise UpdateAgentStatus returns ErrRunChanged and
	// writes nothing (ptone/scion#2550). A hub that records the result of a
	// stop dispatched for run X sets it to X, so the stop never marks a
	// newer run stopped. Internal to the hub — json:"-".
	IfRunID string `json:"-"`
}

// ProjectStore defines project-related persistence operations.
type ProjectStore interface {
	// CreateProject creates a new project record.
	// Returns ErrAlreadyExists if a project with the same slug exists.
	CreateProject(ctx context.Context, project *Project) error

	// GetProject retrieves a project by ID.
	// Returns ErrNotFound if the project doesn't exist.
	GetProject(ctx context.Context, id string) (*Project, error)

	// GetProjectBySlug retrieves a project by its slug.
	// Returns ErrNotFound if the project doesn't exist.
	GetProjectBySlug(ctx context.Context, slug string) (*Project, error)

	// GetProjectBySlugCaseInsensitive retrieves a project by its slug, ignoring case.
	// This is useful for matching projects without git remotes (like global projects).
	// Returns ErrNotFound if the project doesn't exist.
	GetProjectBySlugCaseInsensitive(ctx context.Context, slug string) (*Project, error)

	// GetProjectsByGitRemote returns all projects matching the normalized git remote URL.
	// Returns an empty slice (not error) if none found.
	GetProjectsByGitRemote(ctx context.Context, gitRemote string) ([]*Project, error)

	// NextAvailableSlug returns the next available slug given a base slug.
	// If baseSlug is available, it is returned as-is. Otherwise, serial
	// suffixes are tried: baseSlug-1, baseSlug-2, etc.
	NextAvailableSlug(ctx context.Context, baseSlug string) (string, error)

	// UpdateProject updates an existing project.
	//
	// UpdateProject never writes OwnerID: callers read, mutate and write the
	// whole row, so writing OwnerID would let a stale read undo a concurrent
	// ownership transfer (ptone/scion#2597). project.OwnerID is ignored on
	// input and, on success, refreshed from the stored row. Set OwnerID at
	// creation through CreateProject, or change it with SetProjectOwnerID.
	// Returns ErrNotFound if the project doesn't exist.
	UpdateProject(ctx context.Context, project *Project) error

	// SetProjectOwnerID updates only the project's OwnerID column, leaving
	// every other field untouched. It is the only store method that changes
	// OwnerID after creation; UpdateProject does not write it. Used by
	// ownership transfer so it cannot clobber fields written by a concurrent
	// full-row UpdateProject, nor be clobbered by one.
	// Returns ErrNotFound if the project doesn't exist.
	SetProjectOwnerID(ctx context.Context, projectID, ownerID string) error

	// DeleteProject removes a project by ID.
	// Returns ErrNotFound if the project doesn't exist.
	DeleteProject(ctx context.Context, id string) error

	// ListProjects returns projects matching the filter criteria.
	ListProjects(ctx context.Context, filter ProjectFilter, opts ListOptions) (*ListResult[Project], error)

	// ListProjectSummaries returns the same projects, in the same order and
	// with the same pagination, as ListProjects for the same filter and
	// options, but only with persisted columns: the computed fields
	// (AgentCount, ActiveBrokerCount, ProjectType) are left zero. It costs
	// no per-project queries, so callers that need only identity, naming,
	// annotations and authorization inputs should prefer it.
	ListProjectSummaries(ctx context.Context, filter ProjectFilter, opts ListOptions) (*ListResult[Project], error)

	// LockProjectForMembership acquires a project-scoped serialization lock
	// for membership mutations. On PostgreSQL this executes SELECT ... FOR
	// UPDATE on the project row, serializing concurrent membership
	// transactions for the same project. On SQLite this is a plain read
	// (SQLite already serializes writes at the database level).
	//
	// Must be called inside a transaction (WithTx) before any membership
	// reads or writes. Returns ErrNotFound if the project does not exist.
	LockProjectForMembership(ctx context.Context, projectID string) error

	// LockProjectAgents locks every agent row of the project (soft-deleted
	// ones included) in ascending agent-ID order. On PostgreSQL this is
	// SELECT ... FOR UPDATE ordered by ID; on SQLite it is a plain read.
	//
	// Must be called inside a transaction (WithTx). Call it before deleting
	// any group membership that may belong to one of the project's agents
	// (for example a project-scoped group), so the transaction locks agent
	// rows before membership rows, in ID order, like every other path that
	// deletes agent memberships. Not finding any agents is not an error.
	LockProjectAgents(ctx context.Context, projectID string) error

	// UpdateProjectMessagingPolicy updates only the cross-project inbound
	// policy and its revision using optimistic concurrency. The
	// expectedRevision must match the current revision; returns
	// ErrRevisionConflict on mismatch. This is the only path that may change
	// the policy; generic UpdateProject must NOT update these fields.
	UpdateProjectMessagingPolicy(ctx context.Context, projectID string, inbound string, expectedRevision int64) (*Project, error)
}

// ProjectFilter defines criteria for filtering projects.
type ProjectFilter struct {
	OwnerID         string
	GitRemotePrefix string
	GitRemote       string // Filter by exact git remote (case-sensitive)
	BrokerID        string // Filter by contributing broker
	Name            string // Filter by exact name (case-insensitive)
	Slug            string // Filter by exact slug (case-insensitive)
	Search          string // Substring match on name and slug (case-insensitive)

	// MemberOrOwnerIDs, when non-empty, restricts results to projects whose ID
	// is in this set OR whose owner_id matches OwnerID. OwnerID and this
	// field are combined with OR (not AND) when both are set.
	MemberOrOwnerIDs []string

	// MemberProjectIDs, when non-empty, restricts results to projects whose ID
	// is in this set. Unlike MemberOrOwnerIDs, this is NOT combined with
	// OwnerID — it filters strictly by project ID membership.
	MemberProjectIDs []string

	// ExcludeOwnerID, when non-empty, excludes projects whose owner_id matches
	// this value. Used with MemberProjectIDs to return "shared" projects (member
	// but not owner).
	ExcludeOwnerID string

	// IsTemplate, when non-nil, restricts results to template projects
	// (true) or non-template projects (false). Template projects carry the
	// scion.io/template: "true" label.
	IsTemplate *bool

	// AuthorizedProjectIDs, when non-nil, restricts results to projects whose
	// ID is in this set. This is used by scope-aware list authorization to push
	// the resolved ScopeSet into the SQL query so pagination and totals reflect
	// only the authorized set. A nil value means no authorization filtering
	// (caller has admin view or filtering is handled elsewhere). An empty non-nil
	// slice means no projects are authorized — the query returns zero results.
	AuthorizedProjectIDs []string

	// ExcludedProjectIDs, when non-nil, excludes projects whose ID is in this
	// set from the result. RS2: used when a system-wide admin has project-scoped
	// constraints that block the list permission for specific projects. The
	// exclusion composes by intersection with all other filters (including
	// AuthorizedProjectIDs). An empty or nil slice means no exclusions.
	ExcludedProjectIDs []string
}

// RuntimeBrokerStore defines runtime broker persistence operations.
type RuntimeBrokerStore interface {
	// CreateRuntimeBroker creates a new runtime broker record.
	CreateRuntimeBroker(ctx context.Context, broker *RuntimeBroker) error

	// GetRuntimeBroker retrieves a runtime broker by ID.
	// Returns ErrNotFound if the broker doesn't exist.
	GetRuntimeBroker(ctx context.Context, id string) (*RuntimeBroker, error)

	// GetRuntimeBrokerByName retrieves a runtime broker by its name (case-insensitive).
	// This is used to prevent duplicate brokers with the same name.
	// Returns ErrNotFound if the broker doesn't exist.
	GetRuntimeBrokerByName(ctx context.Context, name string) (*RuntimeBroker, error)

	// GetLegacyRuntimeBrokerByName is GetRuntimeBrokerByName restricted to
	// legacy rows (no stored runtime target). When several legacy rows
	// match it returns the oldest by creation time. ErrNotFound when none.
	GetLegacyRuntimeBrokerByName(ctx context.Context, name string) (*RuntimeBroker, error)

	// SetRuntimeBrokerTarget updates a flat Runtime Broker's target display
	// name. It never converts a legacy row (ErrRuntimeBrokerNotFlat) and
	// never changes a stored target ID or type (ErrRuntimeTargetChanged).
	SetRuntimeBrokerTarget(ctx context.Context, brokerID string, desc api.RuntimeTargetDescriptor) (*RuntimeBroker, error)

	// UpdateRuntimeBroker updates an existing runtime broker.
	// Returns ErrNotFound if the broker doesn't exist.
	UpdateRuntimeBroker(ctx context.Context, broker *RuntimeBroker) error

	// SetRuntimeBrokerCreatedByIfEmpty atomically sets created_by on a runtime
	// broker, but only if the row's created_by is currently empty. Returns
	// applied=false (not an error) when the row already has a non-empty
	// created_by, or when the row does not exist — both are safe no-ops for
	// an idempotent, never-overwrite backfill of legacy ownerless records.
	//
	// This is intentionally separate from UpdateRuntimeBroker, which never
	// touches created_by at all: ownership must not become settable through
	// the same path a broker uses to update its own heartbeat, status, or
	// capabilities.
	SetRuntimeBrokerCreatedByIfEmpty(ctx context.Context, id, createdBy string) (applied bool, err error)

	// DeleteRuntimeBroker removes a runtime broker by ID.
	// Returns ErrNotFound if the broker doesn't exist.
	DeleteRuntimeBroker(ctx context.Context, id string) error

	// ListRuntimeBrokers returns runtime brokers matching the filter criteria.
	ListRuntimeBrokers(ctx context.Context, filter RuntimeBrokerFilter, opts ListOptions) (*ListResult[RuntimeBroker], error)

	// FindEmbeddedBroker returns the single embedded broker if exactly one
	// exists, or nil if zero or multiple embedded brokers are found (ambiguous).
	// "Embedded" means the broker's labels contain {"scion.io/broker-role": "embedded"}.
	// Used to recover the legacy broker ID from DB when settings are lost;
	// rows with a stored runtime target (flat Runtime Brokers) are never
	// candidates.
	FindEmbeddedBroker(ctx context.Context) (*RuntimeBroker, error)

	// UpdateRuntimeBrokerHeartbeat updates the last heartbeat and status.
	UpdateRuntimeBrokerHeartbeat(ctx context.Context, id string, status string) error

	// ClaimRuntimeBrokerConnection records this hub instance as the owner of the
	// broker's live control-channel socket. The newest connection wins
	// (unconditional claim): it sets connected_hub_id/connected_session_id/
	// connected_at and, in the same write, bumps status to online and refreshes
	// last_heartbeat.
	ClaimRuntimeBrokerConnection(ctx context.Context, brokerID, hubInstanceID, sessionID string) error

	// ReleaseRuntimeBrokerConnection clears the broker's affinity ONLY IF it still
	// names (hubInstanceID, sessionID) — a compare-and-clear. It returns
	// cleared=true when this caller owned the affinity and it was cleared; it
	// returns cleared=false (a no-op) when affinity has already moved to another
	// hub/session, in which case the caller MUST NOT stamp the broker offline.
	// It does not change status (the caller decides offline based on cleared).
	ReleaseRuntimeBrokerConnection(ctx context.Context, brokerID, hubInstanceID, sessionID string) (cleared bool, err error)

	// ReleaseAndMarkBrokerOffline atomically clears broker affinity AND stamps
	// status=offline, ONLY IF affinity still names (hubInstanceID, sessionID).
	// This prevents a stale disconnect callback from clobbering a concurrent
	// reconnect's online status — the session check and the offline stamp happen
	// in the same CAS write with no TOCTOU window.
	// Returns cleared=true when affinity matched and the broker was stamped offline.
	// Returns cleared=false (no-op) when affinity has already moved.
	ReleaseAndMarkBrokerOffline(ctx context.Context, brokerID, hubInstanceID, sessionID string) (cleared bool, err error)

	// ReapStaleBrokerAffinity clears connected_hub_id/connected_session_id/
	// connected_at for brokers whose last_heartbeat is older than staleBefore
	// and whose connected_hub_id is not NULL (i.e. they still claim affinity).
	// Returns the number of rows cleared. Does not change broker status.
	ReapStaleBrokerAffinity(ctx context.Context, staleBefore time.Time) (cleared int, err error)

	// MarkBrokerOffline sets a broker's status to offline. This is used during
	// startup repair to ensure stale broker records that were never properly
	// shut down are explicitly marked offline. Returns ErrNotFound if the
	// broker doesn't exist. No-op if the broker is already offline.
	MarkBrokerOffline(ctx context.Context, brokerID string) error

	// MarkStaleBrokersOffline sets status=offline for brokers whose
	// last_heartbeat is older than threshold and whose status is not already
	// offline. Returns the IDs of affected brokers for event publishing.
	MarkStaleBrokersOffline(ctx context.Context, threshold time.Time) ([]string, error)
}

// RuntimeBrokerFilter defines criteria for filtering runtime brokers.
type RuntimeBrokerFilter struct {
	Status      string
	ProjectID   string
	Name        string // Exact match on broker name (case-insensitive)
	AutoProvide *bool  // Filter by auto-provide flag (nil = no filter)
}

// BrokerDispatchStore defines persistence for the durable broker-dispatch intent
// table and the message dispatch-state CAS helpers (multi-node command dispatch).
type BrokerDispatchStore interface {
	// InsertBrokerDispatch persists a new dispatch intent (state defaults pending).
	InsertBrokerDispatch(ctx context.Context, d *BrokerDispatch) error

	// ClaimBrokerDispatch CAS-transitions a dispatch pending->in_progress for the
	// given hub instance. Returns claimed=false if it was not pending (so exactly
	// one node executes a given intent).
	ClaimBrokerDispatch(ctx context.Context, id, hubInstanceID string) (claimed bool, err error)

	// CompleteBrokerDispatch marks a dispatch done with an optional result JSON.
	CompleteBrokerDispatch(ctx context.Context, id, result string) error

	// FailBrokerDispatch marks a dispatch failed, records the error, bumps
	// attempts, and records result (when non-empty) in the same update. On a
	// failed row, result carries the typed failure envelope for the
	// originating node: the broker's HTTP error answer (brokerError), the
	// env requirements a finalize still lacks (envStillMissing), and/or the
	// hub sentinel errors it failed with (hubErrors).
	FailBrokerDispatch(ctx context.Context, id, errMsg, result string) error

	// GetBrokerDispatch returns a single dispatch row by ID (used by the
	// originator to read the result after the owner completes it).
	GetBrokerDispatch(ctx context.Context, id string) (*BrokerDispatch, error)

	// ListPendingDispatch returns pending intents for a broker (drain query).
	ListPendingDispatch(ctx context.Context, brokerID string) ([]BrokerDispatch, error)

	// HasOutstandingBrokerDispatch reports whether agentID has a dispatch
	// intent for op that is still pending or in_progress (design
	// ptone/scion#2483 §2.1 deleteBlocksStart). Served by the
	// (agent_id, op, state) index.
	HasOutstandingBrokerDispatch(ctx context.Context, agentID, op string) (bool, error)

	// HasCompletedBrokerDispatchSince reports whether agentID has a dispatch
	// intent for op that reached done at or after since (by updated_at).
	// Used only by the delete engine's classification step.
	HasCompletedBrokerDispatchSince(ctx context.Context, agentID, op string, since time.Time) (bool, error)

	// MarkMessageDispatched CAS-flips a message pending->dispatched (dedupes drains).
	MarkMessageDispatched(ctx context.Context, id string) (dispatched bool, err error)

	// MarkMessageFailed sets a message's dispatch_state to "failed" with a reason.
	MarkMessageFailed(ctx context.Context, id string, reason string) error

	// ListPendingMessages returns pending messages whose target agent is on the broker.
	ListPendingMessages(ctx context.Context, brokerID string) ([]Message, error)

	// ReapStuckDispatch re-drives or fails in_progress dispatches that have gone
	// stale (updated_at < stuckBefore). Dispatches with attempts < maxAttempts
	// are reset to pending (re-driven); those at or above the limit are failed.
	// Returns counts of re-driven and failed rows.
	ReapStuckDispatch(ctx context.Context, stuckBefore time.Time, maxAttempts int) (requeued, failed int, err error)

	// CountStuckPendingMessages returns the number of messages still in
	// dispatch_state='pending' whose created timestamp is before the given
	// cutoff. Used by the stuck-message sweep (B5-2) to surface messages that
	// have not been dispatched within the expected window.
	CountStuckPendingMessages(ctx context.Context, before time.Time) (int, error)

	// ExpireStuckPendingMessages transitions messages stuck in pending state
	// past the given cutoff to failed, recording the reason. Returns the
	// number of messages expired.
	ExpireStuckPendingMessages(ctx context.Context, before time.Time, reason string) (int, error)

	// FailPendingMessagesWithMissingRecipient transitions pending messages to
	// failed early, without waiting for ExpireStuckPendingMessages' TTL, when
	// their recipient agent has been deleted (soft- or hard-deleted) and can
	// therefore never accept delivery. Returns the number of messages failed.
	FailPendingMessagesWithMissingRecipient(ctx context.Context, reason string) (int, error)

	// BackfillNonAgentDispatchState repairs non-agent-recipient rows left
	// "pending", or "failed" with reason == expiredReason, by the pre-fix
	// nc-promote-busy bug (a storeMsg persisted without stamping
	// DispatchState). The exact complement of ExpireStuckPendingMessages'
	// and PurgeFailedMessages' agent-only scoping: only an agent recipient
	// is ever legitimately in either state. Returns the number repaired.
	BackfillNonAgentDispatchState(ctx context.Context, expiredReason string) (int, error)
}

// TemplateStore defines template persistence operations.
type TemplateStore interface {
	// CreateTemplate creates a new template record.
	CreateTemplate(ctx context.Context, template *Template) error

	// GetTemplate retrieves a template by ID.
	// Returns ErrNotFound if the template doesn't exist.
	GetTemplate(ctx context.Context, id string) (*Template, error)

	// GetTemplateBySlug retrieves a template by its slug and scope.
	// Returns ErrNotFound if the template doesn't exist.
	GetTemplateBySlug(ctx context.Context, slug, scope, projectID string) (*Template, error)

	// UpdateTemplate updates an existing template.
	// Returns ErrNotFound if the template doesn't exist.
	UpdateTemplate(ctx context.Context, template *Template) error

	// DeleteTemplate removes a template by ID.
	// Returns ErrNotFound if the template doesn't exist.
	DeleteTemplate(ctx context.Context, id string) error

	// DeleteTemplatesByScope removes all templates for a given scope.
	// Returns the number of deleted records. No error if zero rows affected.
	DeleteTemplatesByScope(ctx context.Context, scope, scopeID string) (int, error)

	// ListTemplates returns templates matching the filter criteria.
	ListTemplates(ctx context.Context, filter TemplateFilter, opts ListOptions) (*ListResult[Template], error)
}

// TemplateFilter defines criteria for filtering templates.
type TemplateFilter struct {
	Name      string // Exact match on template name
	Scope     string
	ScopeID   string
	ProjectID string // When set without Scope, returns global + project-scoped + user-scoped templates for this project
	UserID    string // When set with ProjectID (no Scope), also includes user-scoped templates for this user
	Harness   string
	OwnerID   string
	Status    string
	Search    string // Full-text search on name/description
}

// HarnessConfigStore defines harness config persistence operations.
type HarnessConfigStore interface {
	// CreateHarnessConfig creates a new harness config record.
	CreateHarnessConfig(ctx context.Context, hc *HarnessConfig) error

	// GetHarnessConfig retrieves a harness config by ID.
	// Returns ErrNotFound if the harness config doesn't exist.
	GetHarnessConfig(ctx context.Context, id string) (*HarnessConfig, error)

	// GetHarnessConfigBySlug retrieves a harness config by its slug and scope.
	// Returns ErrNotFound if the harness config doesn't exist.
	GetHarnessConfigBySlug(ctx context.Context, slug, scope, scopeID string) (*HarnessConfig, error)

	// UpdateHarnessConfig updates an existing harness config.
	// Returns ErrNotFound if the harness config doesn't exist.
	UpdateHarnessConfig(ctx context.Context, hc *HarnessConfig) error

	// DeleteHarnessConfig removes a harness config by ID.
	// Returns ErrNotFound if the harness config doesn't exist.
	DeleteHarnessConfig(ctx context.Context, id string) error

	// DeleteHarnessConfigsByScope removes all harness configs for a given scope.
	// Returns the number of deleted records. No error if zero rows affected.
	DeleteHarnessConfigsByScope(ctx context.Context, scope, scopeID string) (int, error)

	// ListHarnessConfigs returns harness configs matching the filter criteria.
	ListHarnessConfigs(ctx context.Context, filter HarnessConfigFilter, opts ListOptions) (*ListResult[HarnessConfig], error)

	// UpdateHarnessConfigImageStatus updates only the image_status and
	// image_status_checked_at columns for a harness config.
	UpdateHarnessConfigImageStatus(ctx context.Context, id, status string, checkedAt time.Time) error
}

// HarnessConfigFilter defines criteria for filtering harness configs.
type HarnessConfigFilter struct {
	Name        string // Exact match on name
	Scope       string
	ScopeID     string
	ProjectID   string // When set without Scope, returns global + project-scoped configs for this project
	Harness     string
	OwnerID     string
	Status      string
	ImageStatus string
	Search      string // Full-text search on name/description
}

// UserStore defines user persistence operations.
type UserStore interface {
	// CreateUser creates a new user record.
	CreateUser(ctx context.Context, user *User) error

	// GetUser retrieves a user by ID.
	// Returns ErrNotFound if the user doesn't exist.
	GetUser(ctx context.Context, id string) (*User, error)

	// GetUsersByIDs retrieves users by a list of IDs in one lookup.
	// Returns only users that exist; missing or malformed IDs are silently
	// skipped. The returned map is keyed by user ID.
	GetUsersByIDs(ctx context.Context, ids []string) (map[string]*User, error)

	// GetUserByEmail retrieves a user by email.
	// Returns ErrNotFound if the user doesn't exist.
	GetUserByEmail(ctx context.Context, email string) (*User, error)

	// UpdateUser updates an existing user.
	// Returns ErrNotFound if the user doesn't exist.
	UpdateUser(ctx context.Context, user *User) error

	// UpdateUserLastSeen sets only the last_seen timestamp for a user.
	UpdateUserLastSeen(ctx context.Context, id string, t time.Time) error

	// DeleteUser removes a user by ID.
	// Returns ErrNotFound if the user doesn't exist.
	DeleteUser(ctx context.Context, id string) error

	// ListUsers returns users matching the filter criteria.
	ListUsers(ctx context.Context, filter UserFilter, opts ListOptions) (*ListResult[User], error)

	// IsUserInvitedOrActive returns true if a User record exists with the
	// given email and status in ("invited", "active"). Used by the
	// invite_only authorization gate as a replacement for IsEmailAllowListed.
	IsUserInvitedOrActive(ctx context.Context, email string) (bool, error)

	// IncrementSessionGeneration atomically increments the user's
	// session_generation counter, invalidating all existing sessions.
	IncrementSessionGeneration(ctx context.Context, userID string) error
}

// UserFilter defines criteria for filtering users.
type UserFilter struct {
	Role   string
	Status string
	Search string // fuzzy match on email and display_name
}

// AllowListStore defines operations for the email allow list used in invite_only mode.
type AllowListStore interface {
	AddAllowListEntry(ctx context.Context, entry *AllowListEntry) error
	RemoveAllowListEntry(ctx context.Context, email string) error
	GetAllowListEntry(ctx context.Context, email string) (*AllowListEntry, error)
	ListAllowListEntries(ctx context.Context, opts ListOptions) (*ListResult[AllowListEntry], error)
	IsEmailAllowListed(ctx context.Context, email string) (bool, error)
	BulkAddAllowListEntries(ctx context.Context, entries []*AllowListEntry) (added int, skipped int, err error)
	ListEmailDomains(ctx context.Context) ([]string, error)
	UpdateAllowListEntryInviteID(ctx context.Context, email string, inviteID string) error
	ListAllowListEntriesWithInvites(ctx context.Context, opts ListOptions) (*ListResult[AllowListEntryWithInvite], error)
}

// InviteCodeStore defines operations for invite code management.
type InviteCodeStore interface {
	CreateInviteCode(ctx context.Context, invite *InviteCode) error
	GetInviteCodeByHash(ctx context.Context, codeHash string) (*InviteCode, error)
	GetInviteCode(ctx context.Context, id string) (*InviteCode, error)
	ListInviteCodes(ctx context.Context, opts ListOptions) (*ListResult[InviteCode], error)
	IncrementInviteUseCount(ctx context.Context, id string) error
	RevokeInviteCode(ctx context.Context, id string) error
	DeleteInviteCode(ctx context.Context, id string) error
	GetInviteStats(ctx context.Context) (*InviteStats, error)
}

// ProjectProviderStore defines project-broker relationship operations.
type ProjectProviderStore interface {
	// AddProjectProvider adds a broker as a provider to a project.
	AddProjectProvider(ctx context.Context, provider *ProjectProvider) error

	// RemoveProjectProvider removes a broker from a project's providers.
	RemoveProjectProvider(ctx context.Context, projectID, brokerID string) error

	// GetProjectProvider returns a specific provider by project and broker ID.
	// Returns ErrNotFound if the provider relationship doesn't exist.
	GetProjectProvider(ctx context.Context, projectID, brokerID string) (*ProjectProvider, error)

	// GetProjectProviders returns all providers to a project.
	GetProjectProviders(ctx context.Context, projectID string) ([]ProjectProvider, error)

	// GetBrokerProjects returns all projects a broker provides for.
	GetBrokerProjects(ctx context.Context, brokerID string) ([]ProjectProvider, error)

	// UpdateProviderStatus updates a provider's status and last seen time.
	UpdateProviderStatus(ctx context.Context, projectID, brokerID, status string) error

	// DeleteProjectProvidersByProject removes all provider relationships for a project.
	// Returns the number of providers deleted.
	DeleteProjectProvidersByProject(ctx context.Context, projectID string) (int, error)
}

// EnvVarStore defines environment variable persistence operations.
type EnvVarStore interface {
	// CreateEnvVar creates a new environment variable.
	// Returns ErrAlreadyExists if an env var with the same key+scope+scopeId exists.
	CreateEnvVar(ctx context.Context, envVar *EnvVar) error

	// GetEnvVar retrieves an environment variable by key, scope, and scopeId.
	// Returns ErrNotFound if the env var doesn't exist.
	GetEnvVar(ctx context.Context, key, scope, scopeID string) (*EnvVar, error)

	// UpdateEnvVar updates an existing environment variable.
	// Returns ErrNotFound if the env var doesn't exist.
	UpdateEnvVar(ctx context.Context, envVar *EnvVar) error

	// UpsertEnvVar creates or updates an environment variable.
	// Uses key+scope+scopeId as the unique identifier.
	UpsertEnvVar(ctx context.Context, envVar *EnvVar) (created bool, err error)

	// DeleteEnvVar removes an environment variable.
	// Returns ErrNotFound if the env var doesn't exist.
	DeleteEnvVar(ctx context.Context, key, scope, scopeID string) error

	// DeleteEnvVarsByScope removes all environment variables for a given scope.
	// Returns the number of deleted records. No error if zero rows affected.
	DeleteEnvVarsByScope(ctx context.Context, scope, scopeID string) (int, error)

	// ListEnvVars returns environment variables matching the filter criteria.
	ListEnvVars(ctx context.Context, filter EnvVarFilter) ([]EnvVar, error)

	// ListProgenyEnvVars returns user-scoped env vars with allowProgeny=true and
	// injectionMode="always" whose createdBy is in the given set of ancestor IDs.
	ListProgenyEnvVars(ctx context.Context, ancestorIDs []string) ([]EnvVar, error)
}

// EnvVarFilter defines criteria for filtering environment variables.
type EnvVarFilter struct {
	Scope   string // Required: user, project, runtime_broker
	ScopeID string // Required: ID of the scoped entity
	Key     string // Optional: filter by specific key
}

// SecretStore defines secret persistence operations.
type SecretStore interface {
	// CreateSecret creates a new secret.
	// Returns ErrAlreadyExists if a secret with the same key+scope+scopeId exists.
	CreateSecret(ctx context.Context, secret *Secret) error

	// GetSecret retrieves secret metadata by key, scope, and scopeId.
	// Returns ErrNotFound if the secret doesn't exist.
	// Note: The EncryptedValue is populated but should not be exposed via API.
	GetSecret(ctx context.Context, key, scope, scopeID string) (*Secret, error)

	// UpdateSecret updates an existing secret.
	// Increments the version automatically.
	// Returns ErrNotFound if the secret doesn't exist.
	UpdateSecret(ctx context.Context, secret *Secret) error

	// UpdateSecretRefIfMatches conditionally updates only the SecretRef column,
	// applying the change and incrementing Version only if the row's current
	// SecretRef equals expectedRef AND its current Version equals
	// expectedVersion. Returns applied=false (no error) if the row doesn't
	// exist or either check fails — e.g. a concurrent Set() raced ahead and
	// updated the value and the ref, or an old binary rewrote the value
	// through a ref string that happens to read back unchanged (Version
	// still increments on every write, so the version check catches that
	// same-ref case the ref check alone would miss — ptone/scion#2152
	// round-4 review finding 1). Every other column is left untouched, so
	// callers that only need to repoint the ref (such as GCP SM
	// name-migration tooling) never clobber a concurrent metadata edit the
	// way a GetSecret-then-UpdateSecret read-modify-write would
	// (ptone/scion#2152 round-2 review finding 11).
	UpdateSecretRefIfMatches(ctx context.Context, key, scope, scopeID, expectedRef string, expectedVersion int, newRef string) (applied bool, err error)

	// UpdateSecretValueIfVersion conditionally replaces only the
	// EncryptedValue column, applying the change and incrementing Version
	// only if the row's current Version equals expectedVersion. Returns
	// applied=false (no error) if the row doesn't exist or its Version has
	// moved on, e.g. a concurrent writer updated it first. Callers use it
	// for compare-and-swap read-modify-write on a value they own: read the
	// row with GetSecret, compute the new value, then retry on applied=false.
	UpdateSecretValueIfVersion(ctx context.Context, key, scope, scopeID string, expectedVersion int, newEncryptedValue string) (applied bool, err error)

	// UpsertSecret creates or updates a secret.
	// Uses key+scope+scopeId as the unique identifier.
	UpsertSecret(ctx context.Context, secret *Secret) (created bool, err error)

	// DeleteSecret removes a secret.
	// Returns ErrNotFound if the secret doesn't exist.
	DeleteSecret(ctx context.Context, key, scope, scopeID string) error

	// DeleteSecretsByScope removes all secrets for a given scope.
	// Returns the number of deleted records. No error if zero rows affected.
	DeleteSecretsByScope(ctx context.Context, scope, scopeID string) (int, error)

	// ListSecrets returns secret metadata matching the filter criteria.
	// Note: EncryptedValue is NOT populated in the returned secrets.
	ListSecrets(ctx context.Context, filter SecretFilter) ([]Secret, error)

	// GetSecretValue retrieves the encrypted value of a secret.
	// This is used internally for environment resolution.
	// Returns ErrNotFound if the secret doesn't exist.
	GetSecretValue(ctx context.Context, key, scope, scopeID string) (encryptedValue string, err error)

	// UpdateSecretMeta updates metadata fields of an existing secret without
	// modifying the encrypted value. Increments the version automatically.
	// Returns the updated secret. Returns ErrNotFound if the secret doesn't exist.
	UpdateSecretMeta(ctx context.Context, key, scope, scopeID string, meta *SecretMetaUpdate) (*Secret, error)

	// ListProgenySecrets returns user-scoped secrets with allowProgeny=true
	// whose createdBy is in the given set of ancestor IDs.
	// Note: EncryptedValue is NOT populated in the returned secrets.
	ListProgenySecrets(ctx context.Context, ancestorIDs []string) ([]Secret, error)
}

// SecretFilter defines criteria for filtering secrets.
type SecretFilter struct {
	Scope   string // Required: user, project, runtime_broker
	ScopeID string // Required: ID of the scoped entity
	Key     string // Optional: filter by specific key
	Type    string // Optional: filter by secret type (environment, variable, file)
}

// =============================================================================
// Groups and Policies (Hub Permissions System)
// =============================================================================

// GroupStore defines group-related persistence operations.
type GroupStore interface {
	// CreateGroup creates a new group record.
	// Returns ErrAlreadyExists if a group with the same slug exists.
	CreateGroup(ctx context.Context, group *Group) error

	// GetGroup retrieves a group by ID.
	// Returns ErrNotFound if the group doesn't exist.
	GetGroup(ctx context.Context, id string) (*Group, error)

	// GetGroupBySlug retrieves a group by its slug.
	// Returns ErrNotFound if the group doesn't exist.
	GetGroupBySlug(ctx context.Context, slug string) (*Group, error)

	// UpdateGroup updates an existing group.
	// Returns ErrNotFound if the group doesn't exist.
	UpdateGroup(ctx context.Context, group *Group) error

	// DeleteGroup removes a group by ID.
	// Also removes all group memberships (both as parent and as member).
	// Returns ErrNotFound if the group doesn't exist.
	DeleteGroup(ctx context.Context, id string) error

	// ListGroups returns groups matching the filter criteria.
	ListGroups(ctx context.Context, filter GroupFilter, opts ListOptions) (*ListResult[Group], error)

	// AddGroupMember adds a user or group as a member of a group.
	// Returns ErrAlreadyExists if the membership already exists.
	AddGroupMember(ctx context.Context, member *GroupMember) error

	// RemoveGroupMember removes a member from a group.
	// Returns ErrNotFound if the membership doesn't exist.
	RemoveGroupMember(ctx context.Context, groupID, memberType, memberID string) error

	// UpdateGroupMemberRole updates the role of an existing group member.
	// Returns ErrNotFound if the membership doesn't exist.
	UpdateGroupMemberRole(ctx context.Context, groupID, memberType, memberID, newRole string) error

	// GetGroupMembers returns all members of a group.
	GetGroupMembers(ctx context.Context, groupID string) ([]GroupMember, error)

	// GetUserGroups returns all groups a user is a direct member of.
	GetUserGroups(ctx context.Context, userID string) ([]GroupMember, error)

	// GetGroupMembership returns a specific membership record.
	// Returns ErrNotFound if the membership doesn't exist.
	GetGroupMembership(ctx context.Context, groupID, memberType, memberID string) (*GroupMember, error)

	// WouldCreateCycle checks if adding memberGroupID to groupID would create a cycle.
	// Returns true if a cycle would be created.
	WouldCreateCycle(ctx context.Context, groupID, memberGroupID string) (bool, error)

	// GetGroupByProjectID retrieves the project_agents group associated with a project.
	// Returns ErrNotFound if no project group exists for this project.
	GetGroupByProjectID(ctx context.Context, projectID string) (*Group, error)

	// GetEffectiveGroups returns all groups a user belongs to, including
	// transitive memberships through nested groups.
	GetEffectiveGroups(ctx context.Context, userID string) ([]string, error)

	// GetEffectiveGroupsForAgent returns all groups an agent belongs to,
	// including the implicit project_agents group and transitive parent groups.
	GetEffectiveGroupsForAgent(ctx context.Context, agentID string) ([]string, error)

	// GetParentGroups returns all ancestor groups of the given group —
	// i.e., groups that transitively contain this group as a child.
	// Used by delegation checks to compute the full authority inherited
	// through group membership.
	GetParentGroups(ctx context.Context, groupID string) ([]string, error)

	// CountGroupMembersByRole counts how many members of a group have the given role.
	// Only memberships that still reference a user or an agent are counted;
	// orphaned rows (user and agent both cleared by ON DELETE SET NULL) are not.
	CountGroupMembersByRole(ctx context.Context, groupID, role string) (int, error)

	// DeleteGroupMembershipsForUser removes every group membership of the
	// given user and returns the number of rows removed. Call it before the
	// user row is deleted: the FK is ON DELETE SET NULL, so afterwards the
	// rows no longer carry the user ID (ptone/scion#2769).
	//
	// Must be called inside the same transaction as the user delete. On
	// PostgreSQL it first locks the groups the user owns (SELECT ... FOR NO
	// KEY UPDATE ordered by ID), so the transaction locks owned group rows
	// before membership rows, matching ProjectDeletionService's
	// group-row-then-memberships order and avoiding a deadlock with a
	// concurrent project delete. The strength is FOR NO KEY UPDATE, the lock
	// the user-row delete's owner_id SET NULL takes, not FOR UPDATE: it must
	// not conflict with the FK check's FOR KEY SHARE taken when a membership,
	// child-group edge or policy binding referencing the group is inserted.
	DeleteGroupMembershipsForUser(ctx context.Context, userID string) (int, error)

	// DeleteGroupMembershipsForAgents removes every group membership of the
	// given agents and returns the number of rows removed. Call it before the
	// agent rows are deleted, for the same reason as
	// DeleteGroupMembershipsForUser. An empty ids slice is a no-op.
	DeleteGroupMembershipsForAgents(ctx context.Context, ids []string) (int, error)

	// DeleteOrphanedGroupMemberships removes group memberships whose user and
	// agent are both NULL and returns the number of rows removed. Such rows
	// are always orphans: group-in-group membership uses the child-group
	// edge, never a membership row. Idempotent.
	DeleteOrphanedGroupMemberships(ctx context.Context) (int, error)

	// GetGroupsByIDs retrieves groups by a list of IDs.
	// Returns only groups that exist; missing IDs are silently skipped.
	GetGroupsByIDs(ctx context.Context, ids []string) ([]Group, error)
}

// GroupFilter defines criteria for filtering groups.
type GroupFilter struct {
	OwnerID   string // Filter by owner
	ParentID  string // Filter by parent group
	GroupType string // Filter by group type ("explicit" or "project_agents")
	ProjectID string // Filter by project ID (for project_agents groups)
	Search    string // Case-insensitive match on name or slug
}

// PrincipalRef identifies a principal by type and ID.
// Used for bulk policy lookups across multiple principals.
type PrincipalRef struct {
	Type string // "user", "group", or "agent"
	ID   string // Principal UUID
}

// R-5: PolicyStore interface and PolicyFilter removed — CO1 cutover
// deleted the legacy evaluator and all production Policy reads/writes.
// Ent schema files (policy.go, policybinding.go) retained for now per
// brief constraint (schema migration out of scope).

// =============================================================================
// User Access Tokens (UATs)
// =============================================================================

// UserAccessTokenStore defines user access token persistence operations.
type UserAccessTokenStore interface {
	// CreateUserAccessToken creates a new user access token record.
	CreateUserAccessToken(ctx context.Context, token *UserAccessToken) error

	// GetUserAccessToken retrieves a user access token by ID.
	// Returns ErrNotFound if the token doesn't exist.
	GetUserAccessToken(ctx context.Context, id string) (*UserAccessToken, error)

	// GetUserAccessTokenByHash retrieves a user access token by its key hash.
	// Returns ErrNotFound if the token doesn't exist.
	GetUserAccessTokenByHash(ctx context.Context, hash string) (*UserAccessToken, error)

	// UpdateUserAccessTokenLastUsed updates the last used timestamp.
	UpdateUserAccessTokenLastUsed(ctx context.Context, id string) error

	// RevokeUserAccessToken marks a token as revoked.
	// Returns ErrNotFound if the token doesn't exist.
	RevokeUserAccessToken(ctx context.Context, id string) error

	// DeleteUserAccessToken permanently removes a token by ID.
	// Returns ErrNotFound if the token doesn't exist.
	DeleteUserAccessToken(ctx context.Context, id string) error

	// ListUserAccessTokens returns all non-deleted tokens for a user.
	ListUserAccessTokens(ctx context.Context, userID string) ([]UserAccessToken, error)

	// CountUserAccessTokens returns the number of active (non-revoked) tokens for a user.
	CountUserAccessTokens(ctx context.Context, userID string) (int, error)

	// DeleteUserAccessTokensByProject permanently removes all "project"-boundary
	// tokens scoped to projectID. A hub-boundary token belonging to one of the
	// project's members is never removed by this call, even if that member
	// loses access to the project as a result of its deletion.
	// Returns the number of tokens deleted.
	DeleteUserAccessTokensByProject(ctx context.Context, projectID string) (int, error)

	// LockUserForTokens acquires a per-user serialization lock for token
	// mutations. On PostgreSQL this executes SELECT ... FOR UPDATE on the
	// user row, serializing concurrent token transactions for the same user.
	// On SQLite this is a plain read (SQLite already serializes writes at
	// the database level).
	//
	// Must be called inside a transaction (WithTx) before any token count
	// or mutation. Returns ErrNotFound if the user does not exist.
	LockUserForTokens(ctx context.Context, userID string) error
}

// =============================================================================
// Broker Secrets (Runtime Broker Authentication)
// =============================================================================

// BrokerSecretStore defines broker secret persistence operations.
type BrokerSecretStore interface {
	// CreateBrokerSecret creates a new broker secret record.
	// Returns ErrAlreadyExists if a secret for this broker already exists.
	CreateBrokerSecret(ctx context.Context, secret *BrokerSecret) error

	// GetBrokerSecret retrieves a broker secret by broker ID.
	// Returns ErrNotFound if the secret doesn't exist.
	GetBrokerSecret(ctx context.Context, brokerID string) (*BrokerSecret, error)

	// GetActiveSecrets retrieves all active and deprecated (within grace period) secrets for a broker.
	// This is used during secret rotation to support dual-secret validation.
	// Returns an empty slice if no secrets exist.
	GetActiveSecrets(ctx context.Context, brokerID string) ([]*BrokerSecret, error)

	// UpdateBrokerSecret updates an existing broker secret.
	// Returns ErrNotFound if the secret doesn't exist.
	UpdateBrokerSecret(ctx context.Context, secret *BrokerSecret) error

	// DeleteBrokerSecret removes a broker secret.
	// Returns ErrNotFound if the secret doesn't exist.
	DeleteBrokerSecret(ctx context.Context, brokerID string) error

	// CreateJoinToken creates a new join token for broker registration.
	// Returns ErrAlreadyExists if a token for this broker already exists.
	CreateJoinToken(ctx context.Context, token *BrokerJoinToken) error

	// GetJoinToken retrieves a join token by token hash.
	// Returns ErrNotFound if the token doesn't exist.
	GetJoinToken(ctx context.Context, tokenHash string) (*BrokerJoinToken, error)

	// GetJoinTokenByBrokerID retrieves a join token by broker ID.
	// Returns ErrNotFound if the token doesn't exist.
	GetJoinTokenByBrokerID(ctx context.Context, brokerID string) (*BrokerJoinToken, error)

	// DeleteJoinToken removes a join token by broker ID.
	// Returns ErrNotFound if the token doesn't exist.
	DeleteJoinToken(ctx context.Context, brokerID string) error

	// CleanExpiredJoinTokens removes all expired join tokens.
	CleanExpiredJoinTokens(ctx context.Context) error
}

// =============================================================================
// Notifications (Agent Status Notification System)
// =============================================================================

// NotificationStore manages notification subscriptions and notification records.
type NotificationStore interface {
	// CreateNotificationSubscription creates a new notification subscription.
	CreateNotificationSubscription(ctx context.Context, sub *NotificationSubscription) error

	// GetNotificationSubscription returns a single subscription by ID.
	// Returns ErrNotFound if the subscription doesn't exist.
	GetNotificationSubscription(ctx context.Context, id string) (*NotificationSubscription, error)

	// GetNotificationSubscriptions returns all agent-scoped subscriptions for a watched agent.
	GetNotificationSubscriptions(ctx context.Context, agentID string) ([]NotificationSubscription, error)

	// GetNotificationSubscriptionsByProject returns all subscriptions within a project (any scope).
	GetNotificationSubscriptionsByProject(ctx context.Context, projectID string) ([]NotificationSubscription, error)

	// GetNotificationSubscriptionsByProjectScope returns project-scoped subscriptions
	// (scope='project') for a given project.
	GetNotificationSubscriptionsByProjectScope(ctx context.Context, projectID string) ([]NotificationSubscription, error)

	// GetSubscriptionsForSubscriber returns all subscriptions owned by a subscriber.
	GetSubscriptionsForSubscriber(ctx context.Context, subscriberType, subscriberID string) ([]NotificationSubscription, error)

	// UpdateNotificationSubscriptionTriggers updates the trigger activities of a subscription.
	// Returns ErrNotFound if the subscription doesn't exist.
	UpdateNotificationSubscriptionTriggers(ctx context.Context, id string, triggerActivities []string) error

	// DeleteNotificationSubscription deletes a subscription by ID.
	// Returns ErrNotFound if the subscription doesn't exist.
	DeleteNotificationSubscription(ctx context.Context, id string) error

	// DeleteNotificationSubscriptionsForAgent deletes all subscriptions for a watched agent.
	// No error on zero rows affected.
	DeleteNotificationSubscriptionsForAgent(ctx context.Context, agentID string) error

	// CreateNotification creates a new notification record.
	CreateNotification(ctx context.Context, notif *Notification) error

	// GetNotifications returns notifications for a subscriber.
	// If onlyUnacknowledged is true, only unacknowledged notifications are returned.
	// Results are ordered by created_at DESC.
	GetNotifications(ctx context.Context, subscriberType, subscriberID string, onlyUnacknowledged bool) ([]Notification, error)

	// GetNotificationsByAgent returns notifications for a subscriber filtered by agent ID.
	// If onlyUnacknowledged is true, only unacknowledged notifications are returned.
	// Results are ordered by created_at DESC.
	GetNotificationsByAgent(ctx context.Context, agentID, subscriberType, subscriberID string, onlyUnacknowledged bool) ([]Notification, error)

	// GetNotification returns a single notification by ID.
	// Returns ErrNotFound if the notification doesn't exist.
	GetNotification(ctx context.Context, id string) (*Notification, error)

	// AcknowledgeNotification marks a notification as acknowledged.
	// Returns ErrNotFound if the notification doesn't exist.
	AcknowledgeNotification(ctx context.Context, id string) error

	// AcknowledgeAllNotifications marks all notifications for a subscriber as acknowledged.
	// No error on zero rows affected.
	AcknowledgeAllNotifications(ctx context.Context, subscriberType, subscriberID string) error

	// MarkNotificationDispatched marks a notification as dispatched.
	// Call this only after delivery was at least attempted (success or failure).
	// Do NOT call it when delivery was skipped entirely (e.g. no broker, no
	// dispatcher) — that would silently lose the notification.
	// Returns ErrNotFound if the notification doesn't exist.
	MarkNotificationDispatched(ctx context.Context, id string) error

	// GetLastNotificationStatus returns the status of the most recent notification
	// for a given subscription. Returns ("", nil) if no notifications exist.
	GetLastNotificationStatus(ctx context.Context, subscriptionID string) (string, error)

	// ClaimNotificationForDispatch atomically transitions a notification from
	// dispatched=false to dispatched=true, returning claimed=true if this caller
	// won the CAS race. This is the retry-path concurrency primitive: exactly
	// one caller (sweep or broker-connect hook) claims each notification.
	// Returns (false, nil) if the notification is already dispatched or does
	// not exist.
	ClaimNotificationForDispatch(ctx context.Context, id string) (claimed bool, err error)

	// UnmarkNotificationDispatched reverts a claimed notification back to
	// dispatched=false so a future sweep can retry. Used when delivery cannot
	// proceed (no dispatcher, no broker) after an optimistic claim.
	UnmarkNotificationDispatched(ctx context.Context, id string) error

	// GetUndispatchedAgentNotifications returns agent-targeted notifications
	// with dispatched=false.
	//
	// If brokerID is empty (sweep mode), a 60s grace period is applied so
	// the sweep does not race with an in-flight primary dispatch. All
	// undispatched agent notifications older than 60s are returned.
	//
	// If brokerID is non-empty (broker-connect fast-path), no grace period
	// is applied and only notifications whose subscriber agent currently
	// has RuntimeBrokerID == brokerID are returned.
	//
	// Results are ordered by created_at ASC (oldest first), limited to 100.
	GetUndispatchedAgentNotifications(ctx context.Context, brokerID string) ([]Notification, error)

	// CreateSubscriptionTemplate creates a new subscription template.
	CreateSubscriptionTemplate(ctx context.Context, tmpl *SubscriptionTemplate) error

	// GetSubscriptionTemplate returns a template by ID.
	// Returns ErrNotFound if the template doesn't exist.
	GetSubscriptionTemplate(ctx context.Context, id string) (*SubscriptionTemplate, error)

	// ListSubscriptionTemplates returns all templates, optionally filtered by project.
	// Pass empty projectID to include global templates only, or a specific projectID
	// to include both global and project-specific templates.
	ListSubscriptionTemplates(ctx context.Context, projectID string) ([]SubscriptionTemplate, error)

	// DeleteSubscriptionTemplate deletes a template by ID.
	// Returns ErrNotFound if the template doesn't exist.
	DeleteSubscriptionTemplate(ctx context.Context, id string) error
}

// =============================================================================
// Scheduled Events (One-Shot Timers)
// =============================================================================

// ScheduledEventStore manages one-shot scheduled events.
type ScheduledEventStore interface {
	// CreateScheduledEvent creates a new scheduled event.
	CreateScheduledEvent(ctx context.Context, event *ScheduledEvent) error

	// GetScheduledEvent retrieves a scheduled event by ID.
	// Returns ErrNotFound if the event doesn't exist.
	GetScheduledEvent(ctx context.Context, id string) (*ScheduledEvent, error)

	// ListPendingScheduledEvents returns all events with status "pending".
	// Used on startup to load timers into memory.
	ListPendingScheduledEvents(ctx context.Context) ([]ScheduledEvent, error)

	// UpdateScheduledEventStatus updates the status and optional error for an event.
	UpdateScheduledEventStatus(ctx context.Context, id string, status string, firedAt *time.Time, errMsg string) error

	// CancelScheduledEvent marks an event as cancelled.
	// Returns ErrNotFound if the event doesn't exist or is not pending.
	CancelScheduledEvent(ctx context.Context, id string) error

	// ListScheduledEvents returns events matching the filter criteria.
	ListScheduledEvents(ctx context.Context, filter ScheduledEventFilter, opts ListOptions) (*ListResult[ScheduledEvent], error)

	// PurgeOldScheduledEvents removes non-pending events older than cutoff.
	PurgeOldScheduledEvents(ctx context.Context, cutoff time.Time) (int, error)
}

// =============================================================================
// Recurring Schedules (Cron-Based)
// =============================================================================

// ScheduleStore manages user-defined recurring schedules.
type ScheduleStore interface {
	// CreateSchedule creates a new recurring schedule.
	// Returns ErrAlreadyExists if a schedule with the same project_id+name exists.
	CreateSchedule(ctx context.Context, schedule *Schedule) error

	// GetSchedule retrieves a schedule by ID.
	// Returns ErrNotFound if the schedule doesn't exist.
	GetSchedule(ctx context.Context, id string) (*Schedule, error)

	// ListSchedules returns schedules matching the filter criteria.
	ListSchedules(ctx context.Context, filter ScheduleFilter, opts ListOptions) (*ListResult[Schedule], error)

	// ListActiveZonePrefixedSchedules returns up to limit active schedules,
	// across all projects, whose cron expression starts with a zone prefix
	// (CRON_TZ= or TZ=), ordered by ID and skipping the IDs in excludeIDs.
	// It does not page on created, so it is unaffected by how timestamps are
	// stored. The prefix match may be case-insensitive on some backends;
	// callers must re-check the prefix exactly.
	ListActiveZonePrefixedSchedules(ctx context.Context, limit int, excludeIDs []string) ([]Schedule, error)

	// UpdateSchedule writes the schedule's mutable fields named by `fields`
	// (name, cron_expr, event_type, payload, status, next_run_at) from
	// `schedule`; a field not named in `fields` is left completely untouched
	// in the row, so a struct built from a stale read can never revert a
	// column this call didn't intend to change.
	//
	// The write is ALWAYS conditioned on the schedule's current
	// authorization_revision matching prevRevision (or being NULL, when
	// prevRevisionKnown is false) — the revision the caller read just before
	// building this write — regardless of whether attribution is also being
	// replaced. This closes the same race for every field: a write built
	// from a stale read can never land once a newer, revision-bumping write
	// has landed first.
	//
	// When attribution is non-nil, the same conditional write also replaces
	// schedule.InitiatorAttribution.
	//
	// Returns ErrRevisionConflict when the schedule exists but its revision
	// no longer matches, and ErrNotFound if the schedule itself does not
	// exist.
	UpdateSchedule(ctx context.Context, schedule *Schedule, fields ScheduleFieldMask, prevRevision int, prevRevisionKnown bool, attribution *InitiatorAttribution) error

	// UpdateScheduleStatus updates only the status of a schedule.
	// Returns ErrNotFound if the schedule doesn't exist.
	UpdateScheduleStatus(ctx context.Context, id string, status string) error

	// UpdateScheduleAfterRun updates a schedule after a run completes.
	// Sets last_run_at, next_run_at, run counters, and error status.
	UpdateScheduleAfterRun(ctx context.Context, id string, ranAt time.Time, nextRunAt time.Time, errMsg string) error

	// DeleteSchedule removes a schedule by ID.
	// Returns ErrNotFound if the schedule doesn't exist.
	DeleteSchedule(ctx context.Context, id string) error

	// DeleteSchedulesByProject removes all schedules and their events for a project.
	// Returns the number of schedules deleted.
	DeleteSchedulesByProject(ctx context.Context, projectID string) (int, error)

	// ListDueSchedules returns active schedules whose next_run_at has passed.
	ListDueSchedules(ctx context.Context, now time.Time) ([]Schedule, error)
}

// =============================================================================
// GCP Service Accounts (GCP Identity for Agents)
// =============================================================================

// GCPServiceAccountStore defines GCP service account persistence operations.
type GCPServiceAccountStore interface {
	// CreateGCPServiceAccount registers a new GCP service account.
	// Returns ErrAlreadyExists if a SA with the same email+scope+scopeID exists.
	CreateGCPServiceAccount(ctx context.Context, sa *GCPServiceAccount) error

	// GetGCPServiceAccount retrieves a GCP service account by ID.
	// Returns ErrNotFound if the SA doesn't exist.
	GetGCPServiceAccount(ctx context.Context, id string) (*GCPServiceAccount, error)

	// UpdateGCPServiceAccount updates a GCP service account record.
	// Returns ErrNotFound if the SA doesn't exist.
	UpdateGCPServiceAccount(ctx context.Context, sa *GCPServiceAccount) error

	// DeleteGCPServiceAccount removes a GCP service account by ID.
	// Returns ErrNotFound if the SA doesn't exist.
	DeleteGCPServiceAccount(ctx context.Context, id string) error

	// ListGCPServiceAccounts returns GCP service accounts matching the filter.
	ListGCPServiceAccounts(ctx context.Context, filter GCPServiceAccountFilter) ([]GCPServiceAccount, error)

	// CountGCPServiceAccounts returns the number of GCP service accounts matching the filter.
	CountGCPServiceAccounts(ctx context.Context, filter GCPServiceAccountFilter) (int, error)
}

// GCPServiceAccountFilter defines criteria for filtering GCP service accounts.
type GCPServiceAccountFilter struct {
	Scope   string // Filter by scope (hub, project, user)
	ScopeID string // Filter by scope ID
	Email   string // Filter by SA email
	Managed *bool  // Filter by managed status (nil = no filter)

	// IncludeHubScoped widens the Scope/ScopeID terms from AND to OR against
	// hub scope: the result is everything the scope terms select, plus every
	// hub-scoped account. Other terms (Email, Managed) still apply to the whole
	// result. It is a no-op when no scope terms are set, since an unscoped list
	// already returns hub-scoped accounts.
	//
	// This exists so that "this project's accounts, plus the hub-wide ones it
	// may use" is a single query. The obvious alternative — list the project,
	// list the hub, concatenate — is wrong in ways that only show up later: the
	// two halves are read at different times, and any pagination or ordering
	// applied afterwards is applied to a set the database never saw as one.
	// Expressing it as one predicate makes the invariant structural rather than
	// a rule each caller has to remember.
	//
	// The hub arm matches on Scope alone and never on ScopeID. That is a
	// project-wide invariant, not a local shortcut: on a hub-scoped record
	// ScopeID is provenance -- which hub instance registered it -- and is never
	// a predicate. The hub ID is derived from config or a hostname hash, so
	// filtering on it would orphan every hub-scoped record the first time a
	// hostname changed, silently, as an empty result.
	IncludeHubScoped bool
}

// =============================================================================
// GitHub App Installations
// =============================================================================

// GitHubInstallationStore defines GitHub App installation persistence operations.
type GitHubInstallationStore interface {
	// CreateGitHubInstallation creates a new GitHub App installation record.
	// Uses installation_id as the natural key — creating an existing one is idempotent (no-op).
	CreateGitHubInstallation(ctx context.Context, installation *GitHubInstallation) error

	// GetGitHubInstallation retrieves a GitHub App installation by installation ID.
	// Returns ErrNotFound if the installation doesn't exist.
	GetGitHubInstallation(ctx context.Context, installationID int64) (*GitHubInstallation, error)

	// UpdateGitHubInstallation updates an existing GitHub App installation.
	// Returns ErrNotFound if the installation doesn't exist.
	UpdateGitHubInstallation(ctx context.Context, installation *GitHubInstallation) error

	// DeleteGitHubInstallation removes a GitHub App installation by installation ID.
	// Returns ErrNotFound if the installation doesn't exist.
	DeleteGitHubInstallation(ctx context.Context, installationID int64) error

	// ListGitHubInstallations returns all GitHub App installations matching the filter.
	ListGitHubInstallations(ctx context.Context, filter GitHubInstallationFilter) ([]GitHubInstallation, error)

	// GetInstallationForRepository returns an active GitHub App installation
	// that covers the given repository (owner/repo format).
	// Returns ErrNotFound if no matching active installation exists.
	GetInstallationForRepository(ctx context.Context, repoFullName string) (*GitHubInstallation, error)
}

// GitHubInstallationFilter defines criteria for filtering GitHub App installations.
type GitHubInstallationFilter struct {
	AccountLogin string // Filter by GitHub account login
	Status       string // Filter by status (active, suspended, deleted)
	AppID        int64  // Filter by app ID
}

// =============================================================================
// Messages (Bidirectional Human-Agent Messaging)
// =============================================================================

// MessageStore manages persisted structured messages.
type MessageStore interface {
	// CreateMessage persists a new message.
	CreateMessage(ctx context.Context, msg *Message) error

	// GetMessage returns a single message by ID.
	// Returns ErrNotFound if the message doesn't exist.
	GetMessage(ctx context.Context, id string) (*Message, error)

	// GetMessagesByIDs retrieves messages by a list of IDs.
	// Returns only messages that exist; missing IDs are silently skipped.
	// The returned map is keyed by message ID.
	GetMessagesByIDs(ctx context.Context, ids []string) (map[string]*Message, error)

	// ListMessages returns messages matching the given filter.
	// Results are ordered by created_at DESC.
	ListMessages(ctx context.Context, filter MessageFilter, opts ListOptions) (*ListResult[Message], error)

	// LatestMessagesByThreadIDs returns, for each thread ID, the newest
	// message with that thread_id matching opts, using the ListMessages
	// order (created DESC, then id DESC). It answers the same question as
	// ListMessages with a ThreadID filter and Limit 1, for many threads in
	// one lookup. Threads with no matching message are absent from the map.
	LatestMessagesByThreadIDs(ctx context.Context, threadIDs []string, opts LatestMessageOptions) (map[string]*Message, error)

	// LatestMessagesByConversationIDs is LatestMessagesByThreadIDs keyed
	// by conversation_id instead of thread_id. Malformed conversation IDs
	// are rejected with an error, as ListMessages rejects them.
	LatestMessagesByConversationIDs(ctx context.Context, conversationIDs []string, opts LatestMessageOptions) (map[string]*Message, error)

	// MarkMessageRead marks a message as read.
	// Returns ErrNotFound if the message doesn't exist.
	MarkMessageRead(ctx context.Context, id string) error

	// MarkAllMessagesRead marks all messages for a recipient as read.
	MarkAllMessagesRead(ctx context.Context, recipientID string) error

	// PurgeOldMessages removes read messages older than readCutoff and
	// unread messages older than unreadCutoff. Returns count removed.
	PurgeOldMessages(ctx context.Context, readCutoff time.Time, unreadCutoff time.Time) (int, error)

	// PurgeFailedMessages removes messages with dispatch_state="failed" whose
	// created timestamp is before cutoff. Returns the number of messages
	// removed. Unlike PurgeOldMessages (read/unread semantics, which would
	// also delete successfully delivered history), this filters strictly on
	// dispatch_state so delivered messages are never touched.
	PurgeFailedMessages(ctx context.Context, cutoff time.Time) (int, error)

	// SetMessageConversationID updates the conversation_id on an existing
	// message. Used by the Phase 4 backfill to link legacy messages to
	// Conversation records. Returns ErrNotFound if the message doesn't exist.
	SetMessageConversationID(ctx context.Context, messageID, conversationID string) error

	// CountUnbackfilledMessages returns the number of messages with an empty
	// conversation_id for the given project. When projectID is empty, counts
	// across all projects.
	CountUnbackfilledMessages(ctx context.Context, projectID string) (int, error)

	// CountUnreachableUnbackfilledMessages returns the number of messages with
	// a NULL conversation_id whose project_id does not reference an existing
	// project row. These messages are permanently unattributable by the
	// per-project backfill because ListProjects will never return their
	// project (DEF-111: the projects table hard-deletes).
	//
	// This count is used to split the residual attribution report into
	// reachable (actionable) and unreachable (expected, stable) buckets,
	// so that a permanent non-zero count of orphaned messages does not
	// create alarm fatigue by firing a WARN on every boot forever.
	//
	// DEPENDENCY: correct only because ListProjects (empty ProjectFilter)
	// applies no unconditional filter, so it returns every row in projects
	// and NOT EXISTS is its exact complement. An unconditional filter added
	// to ListProjects would silently break this invariant.
	//
	// GATE (M7, DEF-112): TestReachableCountConsistency_DEF112 enforces
	// this invariant by asserting the counter's reachable count equals
	// the sum of per-project counts over ListProjects. A divergence
	// (e.g. an unconditional filter added to ListProjects) turns that
	// test red.
	CountUnreachableUnbackfilledMessages(ctx context.Context) (int, error)
}

// =============================================================================
// Maintenance Operations (Admin Maintenance Panel)
// =============================================================================

// MaintenanceStore defines storage operations for maintenance tasks.
type MaintenanceStore interface {
	// ListMaintenanceOperations returns all registered operations and migrations.
	ListMaintenanceOperations(ctx context.Context) ([]MaintenanceOperation, error)

	// GetMaintenanceOperation returns a single operation by key.
	GetMaintenanceOperation(ctx context.Context, key string) (*MaintenanceOperation, error)

	// UpdateMaintenanceOperation updates an operation's status and result fields.
	UpdateMaintenanceOperation(ctx context.Context, op *MaintenanceOperation) error

	// CreateMaintenanceRun inserts a new run record.
	CreateMaintenanceRun(ctx context.Context, run *MaintenanceOperationRun) error

	// UpdateMaintenanceRun updates a run's status, result, and log.
	UpdateMaintenanceRun(ctx context.Context, run *MaintenanceOperationRun) error

	// GetMaintenanceRun returns a single run by ID.
	GetMaintenanceRun(ctx context.Context, id string) (*MaintenanceOperationRun, error)

	// ListMaintenanceRuns returns runs for a given operation key, ordered by started_at DESC.
	ListMaintenanceRuns(ctx context.Context, operationKey string, limit int) ([]MaintenanceOperationRun, error)

	// AbortRunningMaintenanceOps transitions any "running" operation runs and
	// migrations to "failed". Called at server startup to clean up operations
	// that were interrupted by a restart.
	AbortRunningMaintenanceOps(ctx context.Context) (runs int64, migrations int64, err error)
}

// =============================================================================
// Project Sync State (Workspace Sync Metadata)
// =============================================================================

// ProjectSyncStateStore manages sync metadata for project workspace synchronization.
type ProjectSyncStateStore interface {
	// UpsertProjectSyncState creates or updates sync state for a project (optionally per broker).
	UpsertProjectSyncState(ctx context.Context, state *ProjectSyncState) error

	// GetProjectSyncState retrieves sync state for a project and optional broker.
	// Pass empty brokerID for hub-managed project state.
	// Returns ErrNotFound if no sync state exists.
	GetProjectSyncState(ctx context.Context, projectID, brokerID string) (*ProjectSyncState, error)

	// ListProjectSyncStates returns all sync states for a project (across all brokers).
	ListProjectSyncStates(ctx context.Context, projectID string) ([]ProjectSyncState, error)

	// DeleteProjectSyncState removes sync state for a project and optional broker.
	// Returns ErrNotFound if the state doesn't exist.
	DeleteProjectSyncState(ctx context.Context, projectID, brokerID string) error

	// DeleteProjectSyncStatesByProject removes all sync states for a project.
	// Returns the number of states deleted.
	DeleteProjectSyncStatesByProject(ctx context.Context, projectID string) (int, error)
}

// =============================================================================
// Lifecycle Hooks (Configurable Agent Lifecycle Hooks)
// =============================================================================

// LifecycleHookStore defines lifecycle-hook persistence operations.
type LifecycleHookStore interface {
	// CreateLifecycleHook creates a new lifecycle hook record.
	// Returns ErrAlreadyExists if a hook with the same ID exists.
	CreateLifecycleHook(ctx context.Context, hook *LifecycleHook) error

	// GetLifecycleHook retrieves a lifecycle hook by ID.
	// Returns ErrNotFound if the hook doesn't exist.
	GetLifecycleHook(ctx context.Context, id string) (*LifecycleHook, error)

	// UpdateLifecycleHook updates an existing lifecycle hook.
	// Uses optimistic locking via StateVersion.
	// Returns ErrNotFound if the hook doesn't exist.
	// Returns ErrVersionConflict if the version doesn't match.
	UpdateLifecycleHook(ctx context.Context, hook *LifecycleHook) error

	// DeleteLifecycleHook removes a lifecycle hook by ID.
	// Returns ErrNotFound if the hook doesn't exist.
	DeleteLifecycleHook(ctx context.Context, id string) error

	// ListLifecycleHooks returns lifecycle hooks matching the filter criteria.
	ListLifecycleHooks(ctx context.Context, filter LifecycleHookFilter, opts ListOptions) (*ListResult[LifecycleHook], error)

	// CompareAndSetHookPhase atomically records newPhase as the last-processed
	// phase for an agent's lifecycle-hook evaluation. It returns changed=true
	// ONLY when the stored phase actually differed from newPhase (or no row
	// existed yet). This is used for HA transition de-duplication: across
	// multiple hub instances the single instance whose CAS succeeds "wins" and
	// fires hooks; all others see changed=false and skip.
	CompareAndSetHookPhase(ctx context.Context, agentID, newPhase string) (changed bool, err error)

	// DeleteHookPhase removes the stored last-processed phase for an agent.
	// Called on terminal phases (stopped/error) and agent deletion to prevent
	// unbounded growth. No error is returned if the row does not exist.
	DeleteHookPhase(ctx context.Context, agentID string) error

	// DeleteLifecycleHooksByScope removes all lifecycle hooks for a scope.
	// Returns the number of hooks deleted.
	DeleteLifecycleHooksByScope(ctx context.Context, scopeType string, scopeID string) (int, error)
}

// LifecycleHookFilter defines criteria for filtering lifecycle hooks.
type LifecycleHookFilter struct {
	ScopeType string // Filter by scope type (hub, project)
	ScopeID   string // Filter by scope ID
	Trigger   string // Filter by trigger (running, suspended, stopped, error)
	Enabled   *bool  // Filter by enabled status (nil = no filter)
}

// =============================================================================
// Skills (Skill Bank)
// =============================================================================

// SkillStore defines skill-related persistence operations.
type SkillStore interface {
	CreateSkill(ctx context.Context, skill *Skill) error
	GetSkill(ctx context.Context, id string) (*Skill, error)
	GetSkillBySlug(ctx context.Context, slug, scope, scopeID string) (*Skill, error)
	UpdateSkill(ctx context.Context, skill *Skill) error
	DeleteSkill(ctx context.Context, id string) error
	ListSkills(ctx context.Context, filter SkillFilter, opts ListOptions) (*ListResult[Skill], error)

	CreateSkillVersion(ctx context.Context, version *SkillVersion) error
	GetSkillVersion(ctx context.Context, id string) (*SkillVersion, error)
	GetSkillVersionByNumber(ctx context.Context, skillID, version string) (*SkillVersion, error)
	ListSkillVersions(ctx context.Context, skillID string, opts ListOptions) (*ListResult[SkillVersion], error)
	UpdateSkillVersion(ctx context.Context, version *SkillVersion) error
	DeleteSkillVersion(ctx context.Context, id string) error

	ResolveSkillVersion(ctx context.Context, skillID, constraint string) (*SkillVersion, error)

	IncrementSkillVersionDownloadCount(ctx context.Context, versionID string) error
}

// SkillFilter defines criteria for filtering skills.
type SkillFilter struct {
	Name    string
	Scope   string
	ScopeID string
	OwnerID string
	Status  string
	Search  string
	Tags    []string

	// AccessScope, when non-nil, restricts results to the read boundary
	// established by ptone/scion#1901. It is applied in the store query
	// before LIMIT/cursor pagination (and before TotalCount) so that
	// out-of-scope rows can never crowd a caller's own rows out of a page,
	// and so TotalCount reflects only rows the caller may actually see. A
	// nil AccessScope applies no restriction — used for the hub-admin/
	// super-admin bypass, which sees every skill unfiltered.
	AccessScope *SkillAccessScope
}

// SkillAccessScope narrows a skill query to the rows a specific caller may
// read, mirroring the ptone/scion#1901 and ptone/scion#1903 rulings —
// creation scope is the only read boundary; visibility does not widen it:
//   - IncludeHubScope: hub-scoped (global/core) skills are visible to any
//     authenticated caller.
//   - CallerID: a user-scoped skill is visible only when its ScopeID equals
//     CallerID (the owning user; for an agent caller, its creator).
//   - ProjectIDs: a project-scoped skill is visible only when its ScopeID is
//     one of these (the caller's project memberships).
type SkillAccessScope struct {
	IncludeHubScope bool
	CallerID        string
	ProjectIDs      []string
}

// =============================================================================
// Skill Registries (Hub-to-Hub Federation)
// =============================================================================

// SkillRegistryStore defines skill registry persistence operations.
type SkillRegistryStore interface {
	CreateSkillRegistry(ctx context.Context, registry *SkillRegistry) error
	GetSkillRegistry(ctx context.Context, id string) (*SkillRegistry, error)
	GetSkillRegistryByName(ctx context.Context, name string) (*SkillRegistry, error)
	UpdateSkillRegistry(ctx context.Context, registry *SkillRegistry) error
	DeleteSkillRegistry(ctx context.Context, id string) error
	ListSkillRegistries(ctx context.Context, opts ListOptions) (*ListResult[SkillRegistry], error)
	PinSkillHash(ctx context.Context, registryID string, uri string, hash string) error
	UnpinSkillHash(ctx context.Context, registryID string, uri string) error
	GetPinnedHash(ctx context.Context, registryID string, uri string) (string, error)
	ListPinnedHashes(ctx context.Context, registryID string) (map[string]string, error)
}

// =============================================================================
// Hub Settings (Two-Tier Settings Architecture)
// =============================================================================

// =============================================================================
// Skill Injections (Injected-Skills List)
// =============================================================================

// SkillInjectionStore defines persistence operations for the injected-skills
// list scoped to a project or user.
type SkillInjectionStore interface {
	// ListSkillInjections returns all skill injections for the given scope+scopeID,
	// ordered by sort_order ascending.
	ListSkillInjections(ctx context.Context, scope, scopeID string) ([]SkillInjection, error)

	// AddSkillInjection creates a new skill injection entry.
	// If si.ID is empty, an ID is auto-generated and written back to si.ID on
	// success. Callers that need a stable ID before the call can pre-populate
	// si.ID with any valid UUID string.
	// Returns ErrAlreadyExists if an entry with the same (scope, scope_id, skill_uri) already exists.
	AddSkillInjection(ctx context.Context, si *SkillInjection) error

	// UpdateSkillInjection updates the mutable fields of a skill injection entry.
	// Returns ErrNotFound if the entry with the given ID does not exist.
	UpdateSkillInjection(ctx context.Context, si *SkillInjection) error

	// RemoveSkillInjection deletes a skill injection entry by ID.
	// Returns ErrNotFound if the entry doesn't exist.
	RemoveSkillInjection(ctx context.Context, id string) error

	// SetSkillInjections atomically replaces the full list for a scope+scopeID.
	// All existing entries for (scope, scopeID) are deleted and the new entries
	// are inserted in a single transaction. SortOrder is taken from each entry;
	// callers should set it before calling (handlers default to request position).
	// Phase 3 note: provisioner integration will call this with []SkillInjection
	// constructed from the merged hub+project+user lists.
	SetSkillInjections(ctx context.Context, scope, scopeID string, entries []SkillInjection, createdBy string) error

	// DeleteSkillInjectionsByScope removes all skill injection entries for the
	// given scope+scopeID. Used during project or user deletion to cascade-clean
	// rows that have no FK cascade. Returns the number of rows deleted.
	DeleteSkillInjectionsByScope(ctx context.Context, scope, scopeID string) (int, error)

	// ListProgenySkillInjections returns user-scoped skill injections with
	// allowProgeny=true whose createdBy is in the given set of ancestor IDs.
	ListProgenySkillInjections(ctx context.Context, ancestorIDs []string) ([]SkillInjection, error)
}

// HubSettingStore defines persistence operations for operational hub settings.
// Each setting is a section (e.g. "access", "telemetry") with a JSON value.
type HubSettingStore interface {
	// GetHubSetting retrieves a hub setting by section name.
	// Returns ErrNotFound if the section doesn't exist.
	GetHubSetting(ctx context.Context, section string) (*HubSetting, error)

	// ListHubSettings returns all hub settings.
	ListHubSettings(ctx context.Context) ([]HubSetting, error)

	// UpsertHubSetting creates or updates a hub setting with CAS semantics.
	//   expectedRevision == 0:  create-only; returns ErrRevisionConflict if the section already exists.
	//   expectedRevision == -1: unconditional upsert (used for seeding); always succeeds.
	//   expectedRevision > 0:   CAS update; returns ErrRevisionConflict if current revision != expectedRevision.
	// origin must be "seeded" or "managed" — seed-writes pass "seeded", admin-PUT writes pass "managed".
	UpsertHubSetting(ctx context.Context, section string, value json.RawMessage,
		updatedBy string, expectedRevision int64, origin string) (*HubSetting, error)

	// DeleteHubSetting removes a hub setting by section name.
	// Returns ErrNotFound if the section doesn't exist.
	DeleteHubSetting(ctx context.Context, section string) error

	// BackfillOrigin sets the origin column for existing rows that predate
	// the origin field. Rows with updated_by="seed" get origin="seeded";
	// all other non-_meta rows get origin="managed". Idempotent.
	BackfillOrigin(ctx context.Context) error
}

// BrokerSettingStore defines persistence operations for general per-broker
// settings (ptone/scion#2061 P2, ptone/scion#2177). One row per broker holds
// a BrokerSettings document; see pkg/hub/brokersettings for the key
// registry that validates and authorizes writes to individual keys.
type BrokerSettingStore interface {
	// GetBrokerSettings retrieves brokerID's settings document.
	// Returns ErrNotFound if the broker has no settings row.
	GetBrokerSettings(ctx context.Context, brokerID string) (*BrokerSettingsRecord, error)

	// PutBrokerSettings replaces brokerID's settings document with CAS
	// semantics.
	//   expectedRevision == 0: create-only; returns ErrRevisionConflict if a row already exists.
	//   expectedRevision > 0:  CAS update; returns ErrRevisionConflict if the current revision differs.
	PutBrokerSettings(ctx context.Context, brokerID string, settings BrokerSettings,
		expectedRevision int64, updatedBy string) (*BrokerSettingsRecord, error)

	// DeleteBrokerSettings removes brokerID's settings row, if any. It is a
	// no-op (not an error) when no row exists, so it is safe to call
	// unconditionally from DeleteRuntimeBroker.
	DeleteBrokerSettings(ctx context.Context, brokerID string) error
}

// =============================================================================
// Agent Session Metrics (Hub Metrics Reporting)
// =============================================================================

// AgentSessionMetricsStore defines operations for agent session metrics.
type AgentSessionMetricsStore interface {
	// CreateAgentSessionMetrics creates a new session metrics record.
	CreateAgentSessionMetrics(ctx context.Context, m *AgentSessionMetrics) error

	// GetAgentSessionMetrics retrieves a session metrics record by ID.
	// Returns ErrNotFound if the record doesn't exist.
	GetAgentSessionMetrics(ctx context.Context, id string) (*AgentSessionMetrics, error)

	// ListAgentSessionMetricsByAgent returns all session metrics for an agent,
	// ordered by started_at DESC.
	ListAgentSessionMetricsByAgent(ctx context.Context, agentID string) ([]*AgentSessionMetrics, error)

	// ListAgentSessionMetricsByProject returns all session metrics for a project,
	// ordered by started_at DESC.
	ListAgentSessionMetricsByProject(ctx context.Context, projectID string) ([]*AgentSessionMetrics, error)

	// AggregateByAgent returns SQL-level aggregate totals for an agent's sessions.
	AggregateByAgent(ctx context.Context, agentID string) (*AgentSessionMetricsAggregates, error)

	// AggregateByProject returns SQL-level aggregate totals for a project's sessions.
	AggregateByProject(ctx context.Context, projectID string) (*AgentSessionMetricsAggregates, error)

	// CountDistinctAgentsByProject returns the number of distinct agents with
	// session metrics in a project.
	CountDistinctAgentsByProject(ctx context.Context, projectID string) (int, error)
}

// =============================================================================
// Conversations (Multi-Party Messaging)
// =============================================================================

// ConversationStore manages conversation, participant, and addressee persistence.
type ConversationStore interface {
	// CreateConversation creates a new conversation.
	// Returns ErrAlreadyExists if a conversation with the same ID exists.
	CreateConversation(ctx context.Context, conv *Conversation) error

	// GetConversation retrieves a conversation by ID.
	// Returns ErrNotFound if the conversation doesn't exist.
	GetConversation(ctx context.Context, id string) (*Conversation, error)

	// UpdateConversation updates an existing conversation.
	// Returns ErrNotFound if the conversation doesn't exist.
	UpdateConversation(ctx context.Context, conv *Conversation) error

	// DeleteConversation soft-deletes a conversation by setting DeletedAt.
	// Returns ErrNotFound if the conversation doesn't exist.
	DeleteConversation(ctx context.Context, id string) error

	// ListConversations returns conversations matching the filter.
	ListConversations(ctx context.Context, filter ConversationFilter, opts ListOptions) (*ListResult[Conversation], error)

	// GetConversationByExternalRef looks up a conversation by (surface, external_ref).
	// Returns ErrNotFound if no matching active (non-deleted) conversation exists.
	// This is the read-only counterpart of UpsertConversationByExternalRef.
	GetConversationByExternalRef(ctx context.Context, surface, externalRef string) (*Conversation, error)

	// GetConversationsByExternalRefs is GetConversationByExternalRef for
	// many external refs on one surface in one lookup. Only active
	// (non-deleted) conversations are returned; refs with no match are
	// absent. The returned map is keyed by external ref.
	GetConversationsByExternalRefs(ctx context.Context, surface string, externalRefs []string) (map[string]*Conversation, error)

	// UpsertConversationByExternalRef creates or updates a conversation keyed on (surface, external_ref).
	// This is the idempotent broker-edge operation. Returns the conversation (created or existing).
	// CRITICAL: this must be safe under concurrent calls — the UNIQUE partial index is the guard.
	UpsertConversationByExternalRef(ctx context.Context, conv *Conversation) (*Conversation, error)

	// AddParticipant adds a principal to a conversation.
	// Returns ErrAlreadyExists if the participant already exists.
	AddParticipant(ctx context.Context, p *ConversationParticipant) error

	// EnsureParticipant ensures a participant row exists. If the participant
	// already exists (active or soft-removed), the row is left untouched.
	// Unlike AddParticipant, this does not clear left_at on existing rows.
	EnsureParticipant(ctx context.Context, p *ConversationParticipant) error

	// RemoveParticipant soft-removes a participant by setting LeftAt.
	// Returns ErrNotFound if the participant doesn't exist.
	RemoveParticipant(ctx context.Context, conversationID, principalKind, principalID string) error

	// ListParticipants returns active participants of a conversation (where left_at IS NULL).
	ListParticipants(ctx context.Context, conversationID string) ([]ConversationParticipant, error)

	// GetConversationsForPrincipal returns conversations a principal participates in (active, left_at IS NULL).
	GetConversationsForPrincipal(ctx context.Context, principalKind, principalID string) ([]Conversation, error)

	// AddAddressee adds an addressee record to a message.
	// Returns ErrAlreadyExists if the addressee already exists.
	AddAddressee(ctx context.Context, a *MessageAddressee) error

	// ListAddressees returns all addressees for a message.
	ListAddressees(ctx context.Context, messageID string) ([]MessageAddressee, error)

	// UpdateDeliveryState updates the delivery state of an addressee.
	// Returns ErrNotFound if the addressee doesn't exist.
	UpdateDeliveryState(ctx context.Context, id string, state string, failureReason *string) error
}

// =============================================================================
// Role Definitions and Bindings (Permissions Foundation Phase 1E)
// =============================================================================

// RoleStore defines role definition and role binding persistence operations.
type RoleStore interface {
	// GetRoleDefinition retrieves a role definition by ID.
	// Returns ErrNotFound if the role definition doesn't exist.
	GetRoleDefinition(ctx context.Context, id string) (*RoleDefinition, error)

	// GetRoleDefinitionsByIDs retrieves role definitions by a list of IDs.
	// Missing IDs are silently omitted from the result map.
	GetRoleDefinitionsByIDs(ctx context.Context, ids []string) (map[string]*RoleDefinition, error)

	// GetRoleDefinitionByName retrieves a role definition by name and scope type.
	// Returns ErrNotFound if the role definition doesn't exist.
	GetRoleDefinitionByName(ctx context.Context, name string, scopeType string) (*RoleDefinition, error)

	// ListRoleDefinitions returns all role definitions.
	ListRoleDefinitions(ctx context.Context) ([]*RoleDefinition, error)

	// CreateRoleDefinition creates a new role definition.
	// Returns ErrAlreadyExists if a role definition with the same name+scopeType exists.
	CreateRoleDefinition(ctx context.Context, rd *RoleDefinition) (*RoleDefinition, error)

	// GetRoleBinding retrieves a role binding by ID.
	// Returns ErrNotFound if the role binding doesn't exist.
	GetRoleBinding(ctx context.Context, id string) (*RoleBinding, error)

	// ListRoleBindingsForPrincipal returns all role bindings for a given principal.
	ListRoleBindingsForPrincipal(ctx context.Context, principalType, principalID string) ([]*RoleBinding, error)

	// ListRoleBindingsForPrincipals returns all role bindings matching any of the
	// given principals, optionally filtered by scope types and scope IDs.
	// This is the authorization hot-path query: it resolves bindings for the
	// entire principal closure (direct user + all transitive groups) and applicable
	// scopes (system + project) in ONE query.
	//
	// If scopeTypes is non-empty, only bindings with a matching scope_type are returned.
	// If scopeIDs is non-empty, only bindings with a matching scope_id are returned.
	// When scopeIDs is provided, system-scoped bindings (scope_type="system") are
	// always included regardless of the scope_id values — callers do not need to
	// pass "" to match system bindings.
	// Both filters are optional; passing nil/empty returns all scopes.
	ListRoleBindingsForPrincipals(ctx context.Context, principals []PrincipalRef, scopeTypes []string, scopeIDs []string) ([]*RoleBinding, error)

	// ListRoleBindingsForScope returns all role bindings for a given scope.
	ListRoleBindingsForScope(ctx context.Context, scopeType, scopeID string) ([]*RoleBinding, error)

	// CreateRoleBinding creates a new role binding.
	// Returns ErrAlreadyExists if the exact binding already exists.
	//
	// Validation enforced:
	//   - RoleDefinitionID must reference a valid role definition
	//   - Scope type must match the role definition's scope type
	//   - PrincipalType must be one of user/agent/group
	//   - super-admin and project-owner bindings are direct-user-only
	//   - Duplicate (role, principal, scope) is rejected
	CreateRoleBinding(ctx context.Context, rb *RoleBinding) (*RoleBinding, error)

	// DeleteRoleBinding deletes a role binding by ID.
	// Returns ErrNotFound if the role binding doesn't exist.
	DeleteRoleBinding(ctx context.Context, id string) error

	// DeleteRoleBindingsForPrincipal deletes all role bindings where the given
	// principal (type + ID) is the bound principal. Used for cascade delete
	// when a group is deleted — no orphan bindings may remain.
	DeleteRoleBindingsForPrincipal(ctx context.Context, principalType, principalID string) (int, error)

	// DeleteRoleBindingsForScope deletes all role bindings scoped to the given
	// scope type and ID. Used for cascade delete when a project is deleted.
	DeleteRoleBindingsForScope(ctx context.Context, scopeType, scopeID string) (int, error)

	// UpdateRoleDefinition updates an existing role definition.
	// Returns ErrNotFound if the role definition doesn't exist.
	UpdateRoleDefinition(ctx context.Context, rd *RoleDefinition) (*RoleDefinition, error)

	// UpdateSystemRoleDefinitionPermissions updates only the permissions list of
	// a system role definition. Unlike UpdateRoleDefinition, this method allows
	// modifying system roles and is intended for startup backfill operations.
	// Returns ErrNotFound if the role definition doesn't exist.
	UpdateSystemRoleDefinitionPermissions(ctx context.Context, id string, permissions []string) error

	// DeleteRoleDefinition deletes a role definition by ID.
	// Returns ErrNotFound if the role definition doesn't exist.
	DeleteRoleDefinition(ctx context.Context, id string) error

	// LockRoleDefinitionForAdminGuard acquires a serialization lock on the
	// given role definition row. On PostgreSQL this executes SELECT ... FOR
	// UPDATE, serializing concurrent transactions that both try to check the
	// last-admin count before mutating super-admin bindings. On SQLite this
	// is a plain existence check (SQLite already serializes all writes at the
	// database level).
	//
	// Must be called inside a transaction (WithTx) before checkLastSuperAdminTx
	// to prevent the READ COMMITTED race where two concurrent demotions/deletes
	// both observe the other admin and both commit, leaving zero super-admins.
	// Returns ErrNotFound if the role definition doesn't exist.
	LockRoleDefinitionForAdminGuard(ctx context.Context, roleDefinitionID string) error

	// ListAllRoleBindings returns all role bindings (admin view).
	// opts controls pagination and sort order. See RoleBindingListOptions for defaults.
	ListAllRoleBindings(ctx context.Context, opts RoleBindingListOptions) ([]*RoleBinding, error)

	// CountAllRoleBindings returns the total number of role bindings.
	CountAllRoleBindings(ctx context.Context) (int, error)

	// GetProjectMembership returns the project membership for a user in a project.
	// Returns ErrNotFound if the user is not a member of the project.
	GetProjectMembership(ctx context.Context, projectID, userID string) (*ProjectMembership, error)

	// ListProjectMembers returns all members of a project via role bindings.
	ListProjectMembers(ctx context.Context, projectID string) ([]*ProjectMembership, error)

	// IsProjectMember returns true if the user has any project-scoped role binding
	// for the given project.
	IsProjectMember(ctx context.Context, projectID, userID string) (bool, error)
}

// =============================================================================
// Delegation Edge Store (Permissions Foundation Phase 1G)
// =============================================================================

// DelegationEdgeStore defines delegation edge persistence operations.
// Delegation edges record every authority-delegating relationship and are
// used by the live delegation ceiling to verify ancestor authority at
// decision time.
type DelegationEdgeStore interface {
	// CreateDelegationEdge records a new delegation edge.
	CreateDelegationEdge(ctx context.Context, edge *DelegationEdge) error

	// GetDelegationEdgesForDelegate returns active delegation edges where
	// the given principal is the delegate (receiving authority).
	GetDelegationEdgesForDelegate(ctx context.Context, delegateType, delegateID string) ([]*DelegationEdge, error)

	// GetDelegationEdgesForDelegator returns active delegation edges where
	// the given principal is the delegator (granting authority).
	GetDelegationEdgesForDelegator(ctx context.Context, delegatorType, delegatorID string) ([]*DelegationEdge, error)

	// DeactivateDelegationEdgesForDelegate deactivates every active edge
	// whose delegate is (delegateType, delegateID) and records d on each.
	// d.Cause and d.OpID are required; a zero d.At is set to the current
	// time. Returns the number of edges deactivated.
	DeactivateDelegationEdgesForDelegate(ctx context.Context, delegateType, delegateID string, d Deactivation) (int, error)

	// DeactivateDelegationEdgesForDelegator deactivates every active edge
	// whose delegator is (delegatorType, delegatorID) and records d on each,
	// with the same rules as DeactivateDelegationEdgesForDelegate.
	DeactivateDelegationEdgesForDelegator(ctx context.Context, delegatorType, delegatorID string, d Deactivation) (int, error)

	// ReactivateDelegationEdgesForDelegate reactivates exactly the inactive
	// edges of the delegate whose recorded deactivation cause equals cause
	// and whose operation ID equals opID, and clears their deactivation
	// record. A reactivation that would give the delegate a second active
	// edge in the same scope returns ErrAlreadyExists. Returns the number of
	// edges reactivated.
	ReactivateDelegationEdgesForDelegate(ctx context.Context, delegateType, delegateID string, cause EdgeDeactivationCause, opID string) (int, error)

	// GetDeactivatedDelegationEdgesForDelegate returns exactly the inactive
	// edges of the delegate whose recorded deactivation cause equals cause
	// and whose operation ID equals opID: the edges
	// ReactivateDelegationEdgesForDelegate with the same arguments would
	// reactivate. It writes nothing.
	GetDeactivatedDelegationEdgesForDelegate(ctx context.Context, delegateType, delegateID string, cause EdgeDeactivationCause, opID string) ([]*DelegationEdge, error)
}

// =============================================================================
// Agent Credential Store (Permissions Foundation Phase 1H)
// =============================================================================

// AgentCredentialStore defines agent credential persistence operations.
type AgentCredentialStore interface {
	// CreateAgentCredential records a newly issued agent token.
	CreateAgentCredential(ctx context.Context, cred *AgentCredential) error

	// GetAgentCredentialByJTIHash looks up a credential by its JTI hash.
	// Returns ErrNotFound if no credential exists with that hash.
	GetAgentCredentialByJTIHash(ctx context.Context, jtiHash string) (*AgentCredential, error)

	// RevokeAgentCredential marks a credential as revoked.
	// Returns ErrNotFound if the credential doesn't exist.
	RevokeAgentCredential(ctx context.Context, id string, revokedBy string, reason string) error

	// RevokeAgentCredentialsByAgent revokes all active credentials for an agent.
	// Returns the number of credentials revoked.
	RevokeAgentCredentialsByAgent(ctx context.Context, agentID string, revokedBy string, reason string) (int, error)

	// UpdateAgentCredentialLastSeen updates the last_seen_at timestamp.
	UpdateAgentCredentialLastSeen(ctx context.Context, id string, lastSeen time.Time) error

	// PurgeExpiredAgentCredentials removes expired credentials older than cutoff.
	// Returns the number of credentials purged.
	PurgeExpiredAgentCredentials(ctx context.Context, cutoff time.Time) (int, error)

	// DeleteAgentCredentialsByProject permanently removes all agent credentials for a project.
	// Returns the number of credentials deleted.
	DeleteAgentCredentialsByProject(ctx context.Context, projectID string) (int, error)
}

// =============================================================================
// Agent Identity Key Store (per-project display-name / slug uniqueness)
// =============================================================================

// AgentIdentityKeyStore defines agent identity-key persistence operations.
// An agent's identity keys are the distinct set {slug, slugify(displayName)};
// the store enforces uniqueness of each key within a project as a database
// invariant (UNIQUE(project_id, key)), including against soft-deleted agents.
type AgentIdentityKeyStore interface {
	// ReplaceAgentIdentityKeys atomically replaces agentID's identity-key
	// rows in projectID with keys: rows for keys no longer present are
	// deleted, rows for new keys are inserted, and rows for keys already
	// present are left alone. Returns ErrIdentityKeyConflict if any key in
	// keys is already reserved by a different agent in the project.
	ReplaceAgentIdentityKeys(ctx context.Context, agentID, projectID string, keys []string) error

	// DeleteAgentIdentityKeys removes all of agentID's identity-key rows,
	// freeing its keys for reuse. Used on hard delete/purge.
	DeleteAgentIdentityKeys(ctx context.Context, agentID string) error

	// ListAgentIdentityKeys returns every identity-key row in projectID,
	// across all agents.
	ListAgentIdentityKeys(ctx context.Context, projectID string) ([]*AgentIdentityKey, error)
}

// =============================================================================
// Decision Audit Store (Authorization Decision Audit Phase 1I)
// =============================================================================

// DecisionAuditStore defines persistence operations for authorization decision audit records.
type DecisionAuditStore interface {
	// CreateDecisionAudit stores a new decision audit record.
	CreateDecisionAudit(ctx context.Context, record *DecisionAuditRecord) error

	// ListDecisionAudits returns decision audit records matching the filter.
	// Returns (records, total count, error).
	ListDecisionAudits(ctx context.Context, filter DecisionAuditFilter) ([]*DecisionAuditRecord, int, error)

	// DeleteDecisionAuditsBefore removes decision audit records older than the given time.
	// Returns the number of records deleted.
	DeleteDecisionAuditsBefore(ctx context.Context, before time.Time) (int, error)
}

// =============================================================================
// Mutation Audit Store (Authorization Mutation Audit Phase 1I)
// =============================================================================

// MutationAuditStore defines persistence operations for authorization mutation audit records.
type MutationAuditStore interface {
	// CreateMutationAudit stores a new mutation audit record.
	CreateMutationAudit(ctx context.Context, record *MutationAuditRecord) error

	// ListMutationAudits returns mutation audit records matching the filter.
	// Returns (records, total count, error).
	ListMutationAudits(ctx context.Context, filter MutationAuditFilter) ([]*MutationAuditRecord, int, error)

	// DeleteMutationAuditsBefore removes mutation audit records older than the given time.
	// Returns the number of records deleted.
	DeleteMutationAuditsBefore(ctx context.Context, before time.Time) (int, error)
}

// =============================================================================
// Quota Store (Permissions Phase 2B — Limits/Quotas)
// =============================================================================

// QuotaStore defines limit and quota persistence operations (Permissions Phase 2B).
type QuotaStore interface {
	// Limit definitions
	// CreateLimitDefinition creates a new limit definition.
	// Returns ErrAlreadyExists if a definition with the same name exists.
	CreateLimitDefinition(ctx context.Context, def *LimitDefinition) (*LimitDefinition, error)

	// GetLimitDefinition retrieves a limit definition by ID.
	// Returns ErrNotFound if the definition doesn't exist.
	GetLimitDefinition(ctx context.Context, id string) (*LimitDefinition, error)

	// GetLimitDefinitionByName retrieves a limit definition by name.
	// Returns ErrNotFound if the definition doesn't exist.
	GetLimitDefinitionByName(ctx context.Context, name string) (*LimitDefinition, error)

	// ListLimitDefinitions returns all limit definitions.
	ListLimitDefinitions(ctx context.Context) ([]*LimitDefinition, error)

	// UpdateLimitDefinition updates an existing limit definition.
	// Returns ErrNotFound if the definition doesn't exist.
	UpdateLimitDefinition(ctx context.Context, def *LimitDefinition) (*LimitDefinition, error)

	// DeleteLimitDefinition deletes a limit definition by ID.
	// Returns ErrNotFound if the definition doesn't exist.
	DeleteLimitDefinition(ctx context.Context, id string) error

	// Entitlement bindings
	// CreateEntitlementBinding creates a new entitlement binding.
	// Returns ErrAlreadyExists if the exact binding already exists.
	CreateEntitlementBinding(ctx context.Context, binding *EntitlementBinding) (*EntitlementBinding, error)

	// GetEntitlementBinding retrieves an entitlement binding by ID.
	// Returns ErrNotFound if the binding doesn't exist.
	GetEntitlementBinding(ctx context.Context, id string) (*EntitlementBinding, error)

	// ListEntitlementBindings returns all entitlement bindings for a limit definition.
	ListEntitlementBindings(ctx context.Context, limitDefinitionID string) ([]*EntitlementBinding, error)

	// ListEntitlementBindingsForSubject returns all entitlement bindings for a given subject.
	ListEntitlementBindingsForSubject(ctx context.Context, subjectType, subjectID string) ([]*EntitlementBinding, error)

	// UpdateEntitlementBinding updates an existing entitlement binding.
	// Returns ErrNotFound if the binding doesn't exist.
	UpdateEntitlementBinding(ctx context.Context, binding *EntitlementBinding) (*EntitlementBinding, error)

	// DeleteEntitlementBinding deletes an entitlement binding by ID.
	// Returns ErrNotFound if the binding doesn't exist.
	DeleteEntitlementBinding(ctx context.Context, id string) error

	// Usage reservations
	// CreateUsageReservation creates a new usage reservation.
	CreateUsageReservation(ctx context.Context, reservation *UsageReservation) (*UsageReservation, error)

	// CountActiveReservations counts non-released reservations for a specific
	// limit, subject, and scope. Only reservations with released_at IS NULL are counted.
	CountActiveReservations(ctx context.Context, limitDefinitionID, subjectID, scopeType, scopeID string) (int64, error)

	// ReleaseReservation releases a reservation by setting released_at.
	// The record is retained for auditing. Matches on limit_definition_id and resource_id.
	ReleaseReservation(ctx context.Context, limitDefinitionID, resourceID string) error

	// ReleaseReservationsByResource releases all reservations for a given resource ID.
	ReleaseReservationsByResource(ctx context.Context, resourceID string) error

	// ListActiveReservations returns active (non-released) reservations for a limit and scope.
	ListActiveReservations(ctx context.Context, limitDefinitionID, scopeType, scopeID string) ([]*UsageReservation, error)

	// HasActiveReservation reports whether resourceID already holds a
	// non-released reservation for the given limit, regardless of scope.
	// Used to make re-reservation idempotent (ptone/scion#1963): a caller
	// re-reserving on agent start/resume must not create a second active
	// reservation for an agent that already holds one.
	HasActiveReservation(ctx context.Context, limitDefinitionID, resourceID string) (bool, error)
}

// =============================================================================
// Access Constraint Store (AC1 — Operator Access Constraint Backend)
// =============================================================================

// AccessConstraintStore defines persistence operations for access constraints.
type AccessConstraintStore interface {
	// CreateAccessConstraint creates a new access constraint.
	// Returns ErrAlreadyExists if a constraint with the same name and scope exists.
	// Sets revision=1 on insert.
	CreateAccessConstraint(ctx context.Context, c *AccessConstraint) (*AccessConstraint, error)

	// GetAccessConstraint retrieves an access constraint by ID.
	// Returns ErrNotFound if the constraint doesn't exist.
	GetAccessConstraint(ctx context.Context, id string) (*AccessConstraint, error)

	// UpdateAccessConstraint updates an existing access constraint.
	// Returns ErrNotFound if the constraint doesn't exist.
	// Increments revision atomically. If expectedRevision > 0, returns
	// ErrRevisionConflict if the stored revision differs (optimistic concurrency).
	UpdateAccessConstraint(ctx context.Context, c *AccessConstraint, expectedRevision int64) (*AccessConstraint, error)

	// DeleteAccessConstraint deletes an access constraint by ID.
	// Returns ErrNotFound if the constraint doesn't exist.
	DeleteAccessConstraint(ctx context.Context, id string) error

	// ListAccessConstraints returns all access constraints with pagination.
	// limit of 0 defaults to 100. Maximum allowed limit is 1000.
	// Kept for callers that page through all constraints (loadAllAccessConstraints).
	ListAccessConstraints(ctx context.Context, limit, offset int) ([]*AccessConstraint, error)

	// ListAccessConstraintsFiltered returns access constraints with SQL-level
	// filtering, sorting, and cursor-based pagination.
	// Returns: items, nextPageToken, totalCount, error.
	ListAccessConstraintsFiltered(ctx context.Context, opts AccessConstraintListOptions) ([]*AccessConstraint, string, int, error)

	// CountAccessConstraints returns the total number of access constraints.
	CountAccessConstraints(ctx context.Context) (int, error)

	// ResolveApplicableConstraints returns all constraints that may apply to
	// the given set of principals and scopes. The caller should further filter
	// by subject matching and time window evaluation.
	//
	// This is a batched lookup: it returns constraints where:
	// - subject_kind is "all_principals", OR
	// - subject_kind is "principal" and the principal matches one of the given principals, OR
	// - subject_kind is "group_closure" and the group matches one of the given principals.
	//
	// AND the scope matches one of the given scopes (system matches everything,
	// project matches the specific project).
	ResolveApplicableConstraints(ctx context.Context, principals []PrincipalRef, scopeTypes []string, scopeIDs []string) ([]*AccessConstraint, error)

	// ListConstraintsForScope returns all constraints scoped to the given scope.
	ListConstraintsForScope(ctx context.Context, scopeType, scopeID string) ([]*AccessConstraint, error)

	// DisableAccessConstraint disables a constraint (for offline recovery).
	// Returns ErrNotFound if the constraint doesn't exist.
	DisableAccessConstraint(ctx context.Context, id string) error

	// AppendConstraintHistoryTx appends one purpose-specific live-boundary
	// history row and atomically enforces the newest-1,000-row retention cap.
	// Callers must use the Store passed by WithTx so the row, retention, and
	// boundary mutation share a transaction.
	AppendConstraintHistoryTx(ctx context.Context, entry *AccessConstraintHistory) error

	// ListConstraintHistory returns retained rows newest-first. The milestone-1
	// writer tests use this method; API pagination is owned by #2405.
	ListConstraintHistory(ctx context.Context, constraintID string) ([]*AccessConstraintHistory, error)
}

// ExternalIdentityBinding represents a persistent mapping from an external
// identity provider's (provider, issuer, subject) triple to a local Hub user.
// Used by the GE Google credential exchange to persist stable cross-login
// identity linkage.
type ExternalIdentityBinding struct {
	ID        string    `json:"id"`
	Provider  string    `json:"provider"` // e.g. "google"
	Issuer    string    `json:"issuer"`   // canonical issuer URL
	Subject   string    `json:"subject"`  // stable provider subject
	UserID    string    `json:"userId"`   // FK to User.ID
	Email     string    `json:"email"`    // email at binding time (informational)
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// ExternalIdentityStore provides durable persistence for external identity
// bindings with transactional uniqueness on (provider, issuer, subject).
type ExternalIdentityStore interface {
	// GetExternalIdentity looks up a binding by (provider, issuer, subject).
	// Returns ErrNotFound if no binding exists.
	GetExternalIdentity(ctx context.Context, provider, issuer, subject string) (*ExternalIdentityBinding, error)

	// CreateExternalIdentity atomically creates a new binding.
	// Returns an error if a binding for this (provider, issuer, subject)
	// already exists (unique constraint violation).
	CreateExternalIdentity(ctx context.Context, binding *ExternalIdentityBinding) error

	// UpdateExternalIdentityEmail updates the email field of an existing binding.
	UpdateExternalIdentityEmail(ctx context.Context, id, email string) error

	// GetExternalIdentitiesByUserID returns all bindings for a given user.
	GetExternalIdentitiesByUserID(ctx context.Context, userID string) ([]*ExternalIdentityBinding, error)
}

// UserTerminalWorkspace is the persisted, per-user state of the terminal
// viewer's (/terminals) open-terminal rail: the ordered list of open agent
// IDs, plus which one was frontmost when it was last saved.
type UserTerminalWorkspace struct {
	UserID           string    `json:"userId"`
	AgentIDs         []string  `json:"agentIds"`         // ordered, lowercase canonical UUIDs
	FrontmostAgentID string    `json:"frontmostAgentId"` // "" when none
	Revision         int64     `json:"revision"`
	Updated          time.Time `json:"updatedAt"`
}

// UserTerminalWorkspaceStore provides durable, per-user persistence for the
// terminal viewer's open-terminal list. There is exactly one row per user.
// The store trusts its caller to have already validated agentIDs and
// frontmostAgentID (size bounds, UUID format, membership); see
// pkg/hub/handlers_user_terminal_workspace.go.
type UserTerminalWorkspaceStore interface {
	// GetUserTerminalWorkspace returns the saved workspace for userID.
	// Returns ErrNotFound when the user has never saved one.
	GetUserTerminalWorkspace(ctx context.Context, userID string) (*UserTerminalWorkspace, error)

	// PutUserTerminalWorkspace unconditionally upserts the workspace for
	// userID, keyed on user_id, incrementing revision atomically
	// (last-writer-wins: there is no expected-revision / compare-and-swap
	// parameter). Returns the stored row.
	PutUserTerminalWorkspace(ctx context.Context, userID string, agentIDs []string, frontmostAgentID string) (*UserTerminalWorkspace, error)
}
