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

package entadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agentreincarnation"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/project"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/agentsort"
)

// defaultAgentListLimit and maxAgentListLimit mirror the pagination bounds of
// the legacy SQLite agent store so listing behavior is identical across
// backends.
const (
	defaultAgentListLimit = 500
	maxAgentListLimit     = 500
)

// AgentStore implements the store.AgentStore sub-interface using the Ent ORM.
//
// It supersedes the former raw-SQL store implementation and is designed for
// multi-replica Postgres deployments:
//   - UpdateAgent guards writes with a state_version compare-and-swap so
//     concurrent updates surface store.ErrVersionConflict rather than silently
//     clobbering each other.
//   - The read-modify-write hot paths (UpdateAgentStatus, MarkStaleAgentsOffline,
//     MarkStalledAgents) run inside a transaction and take row locks via
//     SELECT ... FOR UPDATE (a no-op on SQLite, enforced on Postgres).
//   - Soft-deleted agents (deleted_at IS NOT NULL) are excluded from default
//     listings via an Ent predicate.
type AgentStore struct {
	client *ent.Client

	// dialect is detected lazily on first use of a lock-taking path and
	// memoized. SELECT ... FOR UPDATE is only emitted on Postgres; the SQLite
	// driver rejects the clause outright, so it must be elided there.
	dialectOnce sync.Once
	dialectName string
}

// NewAgentStore creates a new Ent-backed AgentStore.
func NewAgentStore(client *ent.Client) *AgentStore {
	return &AgentStore{client: client}
}

// usesRowLocks reports whether the backend supports SELECT ... FOR UPDATE.
// The dialect is captured from a no-op selector the first time it is needed.
func (s *AgentStore) usesRowLocks(ctx context.Context) bool {
	s.dialectOnce.Do(func() {
		_, _ = s.client.Agent.Query().
			Where(func(sel *entsql.Selector) { s.dialectName = sel.Dialect() }).
			Exist(ctx)
	})
	return s.dialectName == dialect.Postgres
}

// Compile-time assertion that AgentStore satisfies the store.AgentStore
// sub-interface.
var _ store.AgentStore = (*AgentStore)(nil)

// entAgentToStore converts an Ent Agent entity into a store.Agent model.
func entAgentToStore(a *ent.Agent) *store.Agent {
	sa := &store.Agent{
		ID:                  a.ID.String(),
		Slug:                a.Slug,
		Name:                a.Name,
		Template:            a.Template,
		ProjectID:           a.ProjectID.String(),
		Labels:              a.Labels,
		Annotations:         a.Annotations,
		Phase:               a.Phase,
		Activity:            a.Activity,
		ToolName:            a.ToolName,
		ConnectionState:     a.ConnectionState,
		ContainerStatus:     a.ContainerStatus,
		ExitCode:            a.ExitCode,
		ExitReason:          a.ExitReason,
		RuntimeState:        a.RuntimeState,
		StalledFromActivity: a.StalledFromActivity,
		CurrentTurns:        a.CurrentTurns,
		CurrentModelCalls:   a.CurrentModelCalls,
		Image:               a.Image,
		Detached:            a.Detached,
		Runtime:             a.Runtime,
		RuntimeBrokerID:     a.RuntimeBrokerID,
		WebPTYEnabled:       a.WebPtyEnabled,
		ExposedPorts:        a.ExposedPorts,
		TaskSummary:         a.TaskSummary,
		Message:             a.Message,
		Created:             a.Created,
		Updated:             a.Updated,
		MessageMode:         string(a.MessageMode),
		Ancestry:            a.Ancestry,
		StateVersion:        a.StateVersion,
		Generation:          a.Generation,
		ReincarnationState:  a.ReincarnationState,
		LaunchAsyncOptIn:    a.LaunchAsyncOptIn,
		LaunchID:            a.LaunchID,
		LaunchState:         a.LaunchState,
		LaunchEndReason:     a.LaunchEndReason,
		LaunchKind:          a.LaunchKind,
		LaunchOwner:         a.LaunchOwner,
		LaunchSeq:           a.LaunchSeq,
		LaunchStep:          a.LaunchStep,
		LaunchError:         a.LaunchError,
		DeletionState:       a.DeletionState,
		DeletionClaim:       a.DeletionClaim,
		DeletionCode:        a.DeletionCode,
		DeletionError:       a.DeletionError,
		DeletionPrior:       a.DeletionPrior,
		DeletionRequest:     a.DeletionRequest,
	}
	sa.DeletionLeaseAt = copyTimePtr(a.DeletionLeaseAt)
	sa.DeletionStartedAt = copyTimePtr(a.DeletionStartedAt)
	sa.DeletionFailedAt = copyTimePtr(a.DeletionFailedAt)
	if a.LaunchDeadline != nil {
		sa.LaunchDeadline = *a.LaunchDeadline
	}
	if a.LaunchLastReportAt != nil {
		sa.LaunchLastReportAt = *a.LaunchLastReportAt
	}
	if a.RunIntent != nil {
		sa.RunIntent = store.RunIntent(*a.RunIntent)
	}
	if a.RunIntentAt != nil {
		t := *a.RunIntentAt
		sa.RunIntentAt = &t
	}
	if a.ReincarnationUpdatedAt != nil {
		t := *a.ReincarnationUpdatedAt
		sa.ReincarnationUpdatedAt = &t
	}
	if a.CreatedBy != nil {
		sa.CreatedBy = a.CreatedBy.String()
	}
	if a.OwnerID != nil {
		sa.OwnerID = a.OwnerID.String()
	}
	if a.LastSeen != nil {
		sa.LastSeen = *a.LastSeen
	}
	if a.LastActivityEvent != nil {
		sa.LastActivityEvent = *a.LastActivityEvent
	}
	if a.StartedAt != nil {
		sa.StartedAt = *a.StartedAt
	}
	if a.DeletedAt != nil {
		sa.DeletedAt = *a.DeletedAt
	}
	if a.AppliedConfig != "" {
		cfg, err := parseAppliedConfig(a.AppliedConfig)
		if err != nil {
			// Logged rather than returned: entAgentToStore has no error path and
			// is called from list queries, so failing here would make one corrupt
			// row break the listing for every agent beside it. The sanitised
			// config is still used; the error says what was dropped.
			slog.Error("agent store: applied_config could not be used as stored",
				"agent_id", sa.ID, "error", err)
		}
		sa.AppliedConfig = cfg
	}
	return sa
}

// validGCPMetadataMode reports whether mode is one the layers below the Hub
// know how to act on.
func validGCPMetadataMode(mode string) bool {
	switch mode {
	case store.GCPMetadataModeAssign, store.GCPMetadataModeBlock, store.GCPMetadataModePassthrough:
		return true
	default:
		return false
	}
}

// parseAppliedConfig decodes a stored applied_config and strips anything the
// rest of the system cannot safely act on. It returns the config to use — which
// may be nil, or non-nil with a field removed — together with an error
// describing what was discarded, so the caller can log it.
//
// Both failure modes used to be silent. The unmarshal error was dropped by an
// `err == nil` guard, so a corrupt row became a nil AppliedConfig that is
// indistinguishable from an agent that never had one. And a config that parsed
// but carried an unusable GCPIdentity.MetadataMode was passed through verbatim.
//
// The mode check exists because an empty MetadataMode on a non-nil GCPIdentity
// is worse than no GCPIdentity at all: it asserts that a GCP identity decision
// was made while naming no decision, and there is no safe default to read from
// it. Dropping just that field lets the agent fall back to the secure "block"
// default the broker applies when no mode is supplied, while keeping the rest of
// the applied config — image, template, harness — which is unrelated and
// probably fine. Discarding the whole config over one bad field would turn a
// metadata-mode problem into an agent-wide one.
func parseAppliedConfig(raw string) (*store.AgentAppliedConfig, error) {
	var cfg store.AgentAppliedConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, fmt.Errorf("applied_config is not valid JSON, ignoring it entirely: %w", err)
	}
	if cfg.GCPIdentity != nil && !validGCPMetadataMode(cfg.GCPIdentity.MetadataMode) {
		bad := cfg.GCPIdentity.MetadataMode
		cfg.GCPIdentity = nil
		return &cfg, fmt.Errorf(
			"applied_config has GCP metadata mode %q, which is not one of %q/%q/%q; "+
				"dropping the GCP identity so the agent falls back to the secure default",
			bad, store.GCPMetadataModeAssign, store.GCPMetadataModeBlock, store.GCPMetadataModePassthrough)
	}
	return &cfg, nil
}

// CreateAgent creates a new agent record.
func (s *AgentStore) CreateAgent(ctx context.Context, a *store.Agent) error {
	uid, err := parseUUID(a.ID)
	if err != nil {
		return err
	}
	projectUID, err := parseUUID(a.ProjectID)
	if err != nil {
		return err
	}

	now := time.Now()
	a.Created = now
	a.Updated = now
	a.StateVersion = 1
	// A freshly created agent is always generation 1, regardless of what the
	// caller's struct happened to carry (e.g. a zero value).
	a.Generation = 1

	create := s.client.Agent.Create().
		SetID(uid).
		SetSlug(a.Slug).
		SetName(a.Name).
		SetTemplate(a.Template).
		SetProjectID(projectUID).
		SetPhase(a.Phase).
		SetActivity(a.Activity).
		SetToolName(a.ToolName).
		SetConnectionState(a.ConnectionState).
		SetContainerStatus(a.ContainerStatus).
		SetRuntimeState(a.RuntimeState).
		SetStalledFromActivity(a.StalledFromActivity).
		SetCurrentTurns(a.CurrentTurns).
		SetCurrentModelCalls(a.CurrentModelCalls).
		SetImage(a.Image).
		SetDetached(a.Detached).
		SetRuntime(a.Runtime).
		SetRuntimeBrokerID(a.RuntimeBrokerID).
		SetWebPtyEnabled(a.WebPTYEnabled).
		SetExposedPorts(utcExposedPorts(a.ExposedPorts)).
		SetTaskSummary(a.TaskSummary).
		SetMessage(a.Message).
		SetCreated(now).
		SetUpdated(now).
		SetStateVersion(a.StateVersion).
		SetGeneration(a.Generation).
		// The async-launch client opt-in (design §3.2) is the one
		// launch_* field CreateAgent sets — the rest start at their column
		// defaults ("", false, 0) because a freshly created agent has no
		// launch yet. Every other launch_* column is written only by
		// BeginLaunch/MarkLaunchAccepted/EndLaunch/ApplyLaunchReport/
		// RunLaunchReaperTick (§3.3).
		SetLaunchAsyncOptIn(a.LaunchAsyncOptIn)

	if a.MessageMode != "" {
		create.SetMessageMode(agent.MessageMode(a.MessageMode))
	}
	if a.Labels != nil {
		create.SetLabels(a.Labels)
	}
	if a.Annotations != nil {
		create.SetAnnotations(a.Annotations)
	}
	if len(a.Ancestry) > 0 {
		create.SetAncestry(a.Ancestry)
	}
	if cfg := marshalAppliedConfig(a.AppliedConfig); cfg != "" {
		create.SetAppliedConfig(cfg)
	}
	// Always set, never leave NULL, even when harnessConfigOf returns "" (no
	// harness configured). NULL is reserved to mean "never written by a
	// binary that knows this column exists" — see
	// CompositeStore.ReconcileHarnessConfigColumn's doc for why that
	// distinction is what makes the reconcile query converge
	// (ptone/scion#2146).
	create.SetHarnessConfig(harnessConfigOf(a.AppliedConfig))
	if !a.LastSeen.IsZero() {
		create.SetLastSeen(a.LastSeen)
	}
	if !a.LastActivityEvent.IsZero() {
		create.SetLastActivityEvent(a.LastActivityEvent)
	}
	if !a.StartedAt.IsZero() {
		create.SetStartedAt(a.StartedAt)
	}
	if !a.DeletedAt.IsZero() {
		create.SetDeletedAt(a.DeletedAt)
	}
	if a.CreatedBy != "" {
		createdByUID, err := parseUUID(a.CreatedBy)
		if err != nil {
			return err
		}
		create.SetCreatedBy(createdByUID)
	}
	if a.OwnerID != "" {
		ownerUID, err := parseUUID(a.OwnerID)
		if err != nil {
			return err
		}
		create.SetOwnerID(ownerUID)
	}

	created, err := create.Save(ctx)
	if err != nil {
		return mapError(err)
	}

	a.Created = created.Created
	a.Updated = created.Updated
	return nil
}

// GetAgent retrieves an agent by ID.
func (s *AgentStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	uid, err := parseGetID(id)
	if err != nil {
		return nil, err
	}
	a, err := s.client.Agent.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entAgentToStore(a), nil
}

// GetAgentBySlug retrieves an agent by its slug within a project.
func (s *AgentStore) GetAgentBySlug(ctx context.Context, projectID, slug string) (*store.Agent, error) {
	projectUID, err := parseUUID(projectID)
	if err != nil {
		return nil, err
	}
	a, err := s.client.Agent.Query().
		Where(agent.ProjectIDEQ(projectUID), agent.SlugEQ(slug), agent.DeletedAtIsNil()).
		Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entAgentToStore(a), nil
}

// GetAgentsByIDs retrieves agents by a list of IDs.
// Returns only agents that exist; missing IDs are silently skipped.
func (s *AgentStore) GetAgentsByIDs(ctx context.Context, ids []string) (map[string]*store.Agent, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	uuids := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		uid, err := parseUUID(id)
		if err != nil {
			continue // skip invalid UUIDs
		}
		uuids = append(uuids, uid)
	}

	if len(uuids) == 0 {
		return nil, nil
	}

	agents, err := s.client.Agent.Query().
		Where(agent.IDIn(uuids...), agent.DeletedAtIsNil()).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}

	result := make(map[string]*store.Agent, len(agents))
	for _, a := range agents {
		sa := entAgentToStore(a)
		result[sa.ID] = sa
	}

	return result, nil
}

// UpdateAgent updates an existing agent using optimistic locking on
// state_version. The mutable field set mirrors the legacy SQLite store:
// identity-adjacent operational fields are updated, while immutable lineage
// fields (created_at, created_by, project_id, ancestry) and the sciontool-owned
// counters (current_turns, current_model_calls, started_at) are left untouched.
func (s *AgentStore) UpdateAgent(ctx context.Context, a *store.Agent) error {
	uid, err := parseUUID(a.ID)
	if err != nil {
		return err
	}

	now := time.Now()
	expectedVersion := a.StateVersion
	newVersion := expectedVersion + 1

	if a.Phase != "running" {
		// Fast path, unchanged from before T1: one statement, no
		// transaction. Rows whose write never touches phase=running (the
		// overwhelming majority of UpdateAgent calls) pay nothing extra and
		// gain no new error path (design t1-async-create-v11.md §3.3: "the
		// same transaction runs" applies only to a running write).
		update, err := buildAgentUpdate(s.client.Agent, uid, a, expectedVersion, newVersion, now)
		if err != nil {
			return err
		}
		affected, err := update.Save(ctx)
		if err != nil {
			return mapError(err)
		}
		if affected == 0 {
			return agentUpdateConflictError(ctx, s.client.Agent, uid)
		}
		a.Updated = now
		a.StateVersion = newVersion
		return nil
	}

	// a.Phase == "running": try the single-statement CAS first, restricted to
	// rows that do NOT have an active launch. This is a single UPDATE, so it
	// is already atomic on its own — no transaction needed — and it is the
	// only path taken by a row that has never had a launch (the overwhelming
	// majority of UpdateAgent calls while the feature is off or unused).
	affected, err := s.tryRunningFastPath(ctx, uid, a, expectedVersion, newVersion, now)
	if err != nil {
		return err
	}
	if affected {
		a.Updated = now
		a.StateVersion = newVersion
		return nil
	}

	// affected == 0: probe once with a single read, in place of a bare
	// Exist, to disambiguate a stale version / missing row from a row that
	// genuinely has an active launch — so a row with no launch gets exactly
	// main's statement sequence and error classes (one UPDATE, then one
	// read; ErrNotFound or ErrVersionConflict), never the transactional path
	// or its failure modes. Like main's Exist, this selects only the columns
	// it needs (state_version, launch_state) rather than the full row, and
	// wraps a read error the same way main's Exist-based check does.
	injectAgentRunningRace("before_probe")
	probeVersion, probeLaunchState, err := s.probeAgentVersionAndLaunchState(ctx, uid)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.ErrNotFound
		}
		return err
	}
	if probeVersion != expectedVersion {
		return store.ErrVersionConflict
	}
	if probeLaunchState != store.LaunchStateActive {
		// The version still matches, but launch_state is no longer active —
		// a narrow race (e.g. a concurrent EndLaunch, which never bumps
		// state_version) cleared the exclusion between the fast path's
		// failed UPDATE and this probe. Retry the fast path once; it should
		// now match.
		injectAgentRunningRace("before_retry")
		affected, err := s.tryRunningFastPath(ctx, uid, a, expectedVersion, newVersion, now)
		if err != nil {
			return err
		}
		if affected {
			a.Updated = now
			a.StateVersion = newVersion
			return nil
		}
		// Yet another race landed in between (e.g. a BeginLaunch
		// re-activated the launch without bumping state_version, in the gap
		// between the probe and this retry). Do NOT classify this as a
		// conflict here — that would wrongly return ErrVersionConflict for a
		// write whose version is still current. Fall through to the
		// transactional path below instead: its own CAS carries no
		// launch_state exclusion, so it correctly reports ErrNotFound/
		// ErrVersionConflict if that's what changed, or atomically ends the
		// newly-active launch together with the phase write if not. This is
		// terminal — at most one retry, then the tx settles it either way.
	}

	// The transactional path: either the probe found a genuinely active
	// launch, or a second race after the retry above means only the tx's own
	// CAS can tell what the row's current state actually is. The CAS update
	// and the conditional "end active launch" update must run in the SAME
	// transaction — never as two separate statements/transactions — so a
	// launch can never be observed active on a row that has already
	// committed phase=running,
	// and a failure of the second statement rolls back the phase write
	// instead of leaving it committed with a now-stale caller copy
	// (a.StateVersion/a.Updated would otherwise disagree with the row, and a
	// retry would get a spurious ErrVersionConflict).
	// updateAgentRunningTx re-runs the CAS without the launch_state
	// exclusion above, so it also correctly reports a version conflict or
	// not-found if the row changed yet again between here and the
	// transaction actually starting.
	injectAgentRunningTxEnter()
	tx, err := s.client.Tx(ctx)
	if err != nil {
		if !errors.Is(err, ent.ErrTxStarted) {
			return err
		}
		// s.client already wraps a transaction (constructed inside
		// store.WithTx) — run directly on it so this participates in the
		// caller's transaction instead of starting (and failing to start) a
		// nested one. The caller owns commit/rollback.
		if err := s.updateAgentRunningTx(ctx, s.client, uid, a, expectedVersion, newVersion, now); err != nil {
			return err
		}
		a.Updated = now
		a.StateVersion = newVersion
		return nil
	}
	if err := s.updateAgentRunningTx(ctx, tx.Client(), uid, a, expectedVersion, newVersion, now); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("agent store: commit UpdateAgent (running): %w", err)
	}
	a.Updated = now
	a.StateVersion = newVersion
	return nil
}

// tryRunningFastPath attempts the single-statement CAS for a phase=running
// write, excluding rows with an active launch (agent_store.go's UpdateAgent
// running branch). It reports whether the statement matched a row; it never
// opens a transaction.
func (s *AgentStore) tryRunningFastPath(ctx context.Context, uid uuid.UUID, a *store.Agent, expectedVersion, newVersion int64, now time.Time) (bool, error) {
	update, err := buildAgentUpdate(s.client.Agent, uid, a, expectedVersion, newVersion, now)
	if err != nil {
		return false, err
	}
	update.Where(agent.Or(agent.LaunchStateIsNil(), agent.LaunchStateNEQ(store.LaunchStateActive)))
	affected, err := update.Save(ctx)
	if err != nil {
		return false, mapError(err)
	}
	return affected == 1, nil
}

// agentVersionAndLaunchState is the projection probeAgentVersionAndLaunchState
// reads; struct tags name the columns for ent's generated Select+Scan.
type agentVersionAndLaunchState struct {
	StateVersion int64  `sql:"state_version"`
	LaunchState  string `sql:"launch_state"`
}

// probeAgentVersionAndLaunchState reads just state_version and launch_state
// for uid — the two columns UpdateAgent's running-write fast-path miss needs
// to disambiguate a conflict from a genuinely active launch — rather than
// the full row. Returns store.ErrNotFound if the row doesn't exist; any
// other read error is wrapped the same way agentUpdateConflictError's
// Exist-based check wraps main's.
func (s *AgentStore) probeAgentVersionAndLaunchState(ctx context.Context, uid uuid.UUID) (int64, string, error) {
	var rows []agentVersionAndLaunchState
	err := s.client.Agent.Query().
		Where(agent.IDEQ(uid)).
		Select(agent.FieldStateVersion, agent.FieldLaunchState).
		Scan(ctx, &rows)
	if err != nil {
		return 0, "", fmt.Errorf("ent: check existence: %w", err)
	}
	if len(rows) == 0 {
		return 0, "", store.ErrNotFound
	}
	return rows[0].StateVersion, rows[0].LaunchState, nil
}

// agentRunningTxFailureHook, when non-nil, is called inside
// updateAgentRunningTx between the CAS update and the conditional
// end-active-launch update. Tests use it to prove the two statements commit
// or roll back together: an error here must leave the row's phase,
// state_version and launch_state exactly as they were before the call.
var agentRunningTxFailureHook func() error

func injectAgentRunningTxFailure() error {
	if agentRunningTxFailureHook == nil {
		return nil
	}
	return agentRunningTxFailureHook()
}

// agentRunningTxEnterHook, when non-nil, is called immediately before
// UpdateAgent's running branch opens (or reuses an ambient) transaction —
// before updateAgentRunningTx's CAS runs. Tests use it as a tripwire: it
// must never fire for a plain stale-version or not-found running write on a
// row with no launch (those return from the fast-path probe above, without
// reaching this point), only for a row that genuinely has, or might still
// have after a race, an active launch.
var agentRunningTxEnterHook func()

func injectAgentRunningTxEnter() {
	if agentRunningTxEnterHook != nil {
		agentRunningTxEnterHook()
	}
}

// agentRunningRaceHook, when non-nil, is called at named points inside
// UpdateAgent's running branch, between its separate autocommitted
// statements (mirroring launch_reaper.go's injectFailure). Tests use it to
// land a concurrent write in the exact window a real race would: "before_probe"
// is between the first fast-path miss and the probe read; "before_retry" is
// between the probe (having found a matching version but a non-active
// launch_state) and the retried fast path.
var agentRunningRaceHook func(point string)

func injectAgentRunningRace(point string) {
	if agentRunningRaceHook != nil {
		agentRunningRaceHook(point)
	}
}

// updateAgentRunningTx runs the CAS update and, only if it matched, the
// conditional "end active launch as running_observed" update, both against
// client — either a freshly opened transaction's client or the ambient
// tx-scoped client when s is already inside one. It returns
// store.ErrNotFound / store.ErrVersionConflict, without touching
// launch_state, when the CAS did not match a row.
func (s *AgentStore) updateAgentRunningTx(ctx context.Context, client *ent.Client, uid uuid.UUID, a *store.Agent, expectedVersion, newVersion int64, now time.Time) error {
	update, err := buildAgentUpdate(client.Agent, uid, a, expectedVersion, newVersion, now)
	if err != nil {
		return err
	}
	affected, err := update.Save(ctx)
	if err != nil {
		return mapError(err)
	}
	if affected == 0 {
		return agentUpdateConflictError(ctx, client.Agent, uid)
	}

	if err := injectAgentRunningTxFailure(); err != nil {
		return err
	}

	// T1 async agent create (design §3.3): end any still-active launch as
	// running_observed. Conditioned on the ROW's launch_state (not the
	// caller's struct, which may be stale), so a caller whose
	// copy still says launch_state="" (never having read the launch that
	// BeginLaunch just started) or "active" (stale, when the row has
	// actually already ended, e.g. not_launched) cannot corrupt it: the
	// WHERE clause only ever matches a row that is genuinely still active,
	// and a mismatch is a silent no-op (zero rows = success), never an
	// error.
	_, err = client.Agent.Update().
		Where(agent.IDEQ(uid), agent.LaunchStateEQ(store.LaunchStateActive)).
		SetLaunchState(store.LaunchStateEnded).
		SetLaunchEndReason(store.LaunchEndReasonRunningObserved).
		Save(ctx)
	return err
}

// buildAgentUpdate constructs the CAS update statement shared by both the
// fast path and the running-write transactional path. ac is either
// s.client.Agent or a tx-scoped client's Agent — both are *ent.AgentClient.
func buildAgentUpdate(ac *ent.AgentClient, uid uuid.UUID, a *store.Agent, expectedVersion, newVersion int64, now time.Time) (*ent.AgentUpdate, error) {
	update := ac.Update().
		Where(agent.IDEQ(uid), agent.StateVersionEQ(expectedVersion)).
		SetSlug(a.Slug).
		SetName(a.Name).
		SetTemplate(a.Template).
		SetPhase(a.Phase).
		SetActivity(a.Activity).
		SetToolName(a.ToolName).
		SetConnectionState(a.ConnectionState).
		SetContainerStatus(a.ContainerStatus).
		SetRuntimeState(a.RuntimeState).
		SetStalledFromActivity(a.StalledFromActivity).
		SetImage(a.Image).
		SetDetached(a.Detached).
		SetRuntime(a.Runtime).
		SetRuntimeBrokerID(a.RuntimeBrokerID).
		SetWebPtyEnabled(a.WebPTYEnabled).
		SetTaskSummary(a.TaskSummary).
		SetMessage(a.Message).
		SetUpdated(now).
		SetStateVersion(newVersion).
		SetGeneration(a.Generation).
		SetReincarnationState(a.ReincarnationState)
	if a.ReincarnationUpdatedAt != nil {
		update.SetReincarnationUpdatedAt(*a.ReincarnationUpdatedAt)
	}

	if a.MessageMode != "" {
		update.SetMessageMode(agent.MessageMode(a.MessageMode))
	}

	if a.ExitCode != nil {
		update.SetExitCode(*a.ExitCode)
	} else {
		update.ClearExitCode()
	}
	update.SetExitReason(a.ExitReason)

	if a.Labels != nil {
		update.SetLabels(a.Labels)
	} else {
		update.ClearLabels()
	}
	if a.Annotations != nil {
		update.SetAnnotations(a.Annotations)
	} else {
		update.ClearAnnotations()
	}
	if cfg := marshalAppliedConfig(a.AppliedConfig); cfg != "" {
		update.SetAppliedConfig(cfg)
	} else {
		update.ClearAppliedConfig()
	}
	// Always set, never clear to NULL — see CreateAgent's identical comment
	// and ReconcileHarnessConfigColumn's doc (ptone/scion#2146).
	// AppliedConfig=nil (cleared above) still yields "" here, which is
	// exactly the sentinel this row should carry, not NULL.
	update.SetHarnessConfig(harnessConfigOf(a.AppliedConfig))
	if a.LastSeen.IsZero() {
		update.ClearLastSeen()
	} else {
		update.SetLastSeen(a.LastSeen)
	}
	if a.LastActivityEvent.IsZero() {
		update.ClearLastActivityEvent()
	} else {
		update.SetLastActivityEvent(a.LastActivityEvent)
	}
	if a.DeletedAt.IsZero() {
		update.ClearDeletedAt()
	} else {
		update.SetDeletedAt(a.DeletedAt)
	}
	if a.OwnerID == "" {
		update.ClearOwnerID()
	} else {
		ownerUID, err := parseUUID(a.OwnerID)
		if err != nil {
			return nil, err
		}
		update.SetOwnerID(ownerUID)
	}

	// T1 async agent create (design §3.3, T3): a write of phase=running
	// always clears launch_error, regardless of what the caller's struct
	// carries for the launch_* columns — UpdateAgent otherwise never sets any
	// launch_* column from the caller's struct; this is the one exception,
	// and it is unconditional (not gated on the row's launch_state).
	if a.Phase == "running" {
		update.SetLaunchError("")
	}

	return update, nil
}

// agentUpdateConflictError distinguishes a missing agent from a stale write
// so callers can retry conflicts, for a CAS update that matched zero rows.
func agentUpdateConflictError(ctx context.Context, ac *ent.AgentClient, uid uuid.UUID) error {
	exists, err := ac.Query().Where(agent.IDEQ(uid)).Exist(ctx)
	if err != nil {
		return err
	}
	if !exists {
		return store.ErrNotFound
	}
	return store.ErrVersionConflict
}

// DeleteAgent permanently removes an agent by ID (hard delete).
func (s *AgentStore) DeleteAgent(ctx context.Context, id string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	err = s.client.Agent.DeleteOneID(uid).Exec(ctx)
	if err != nil {
		return mapError(err)
	}
	return nil
}

// ListAgents returns agents matching the filter criteria. See the
// store.AgentStore interface doc for the legacy/sorted-mode split:
// opts.SortBy empty is the
// legacy path, unchanged; opts.SortBy set to "created" or "updated" is the
// real-SQL sorted-mode path the global endpoint's sorted mode uses directly.
func (s *AgentStore) ListAgents(ctx context.Context, filter store.AgentFilter, opts store.ListOptions) (*store.ListResult[store.Agent], error) {
	preds, err := agentFilterPredicates(filter)
	if err != nil {
		return nil, err
	}

	query := s.client.Agent.Query()
	if len(preds) > 0 {
		query.Where(preds...)
	}

	totalCount := 0
	if !opts.SkipTotalCount {
		var err error
		totalCount, err = query.Clone().Count(ctx)
		if err != nil {
			return nil, err
		}
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = defaultAgentListLimit
	}
	if limit > maxAgentListLimit {
		limit = maxAgentListLimit
	}

	if opts.SortBy == "" {
		// Legacy path: a request without sort is byte-identical to before
		// sorted mode existed, untouched by it.
		if opts.Cursor != "" {
			cursorCreated, cursorID, err := decodeListCursor(opts.Cursor, opts.CursorBinding)
			if err != nil {
				return nil, fmt.Errorf("invalid cursor: %w", err)
			}
			query.Where(agentBeforeCursor(cursorCreated, cursorID))
		}

		// Fetch one extra row to detect whether a further page exists.
		rows, err := query.
			Order(agent.ByCreated(entsql.OrderDesc()), agent.ByID(entsql.OrderDesc())).
			Limit(limit + 1).
			All(ctx)
		if err != nil {
			return nil, err
		}

		items := make([]store.Agent, 0, len(rows))
		for _, a := range rows {
			items = append(items, *entAgentToStore(a))
		}

		result := &store.ListResult[store.Agent]{TotalCount: totalCount}
		if len(items) > limit {
			result.Items = items[:limit]
			last := result.Items[len(result.Items)-1]
			result.NextCursor = encodeListCursor(last.Created, last.ID, opts.CursorBinding)
		} else {
			result.Items = items
		}
		return result, nil
	}

	// Sorted mode. Fail closed on an unrecognized
	// sort or dir, matching ListAgentMembers' own contract, rather than
	// letting agentSortOrder/agentAfterCursor silently fall back to a
	// default. The hub validates these before calling in; this is the store
	// API's own contract, independent of any one caller.
	if opts.SortBy != agentsort.Created && opts.SortBy != agentsort.Updated {
		return nil, fmt.Errorf("ListAgents: invalid sort %q: %w", opts.SortBy, store.ErrInvalidInput)
	}
	if opts.SortDir != agentsort.Asc && opts.SortDir != agentsort.Desc {
		return nil, fmt.Errorf("ListAgents: invalid dir %q: %w", opts.SortDir, store.ErrInvalidInput)
	}
	// A legacy opaque cursor has no meaning in sorted mode. Reject it rather
	// than ignore it, so a caller that passes one (alone or alongside
	// SortCursor) gets an error instead of a silently different page.
	if opts.Cursor != "" {
		return nil, fmt.Errorf("ListAgents: legacy cursor is not valid in sorted mode: %w", store.ErrInvalidInput)
	}
	if opts.SortCursor != nil {
		query.Where(agentAfterCursor(opts.SortBy, opts.SortDir, *opts.SortCursor))
	}

	// Fetch one extra row to detect whether a further page exists, exactly
	// like the legacy path above: the store's existing limit+1
	// probe says whether more rows exist.
	rows, err := query.
		Order(agentSortOrder(opts.SortBy, opts.SortDir)).
		Limit(limit + 1).
		All(ctx)
	if err != nil {
		return nil, err
	}

	items := make([]store.Agent, 0, len(rows))
	for _, a := range rows {
		items = append(items, *entAgentToStore(a))
	}

	result := &store.ListResult[store.Agent]{TotalCount: totalCount}
	if len(items) > limit {
		result.Items = items[:limit]
		last := result.Items[len(result.Items)-1]
		lastRow := agentsort.KeyFor(opts.SortBy, last.ID, last.Created, last.Updated, last.LastActivityEvent)
		result.NextCursor = store.EncodeAgentCursor(opts.SortBy, opts.SortDir, lastRow.K, lastRow.Created, last.ID, opts.CursorBinding)
	} else {
		result.Items = items
	}
	return result, nil
}

// CountAgentsByPhaseIDs returns the id and phase of every agent matching
// filter, via a narrow SELECT id, phase with no decision made: it backs
// the global endpoint's sorted-mode "stats"
// population. Unlike ListAgentMembers it has no candidate ceiling: the
// global endpoint's stats population is the same SQL scope predicate that
// already authorizes the request, with no per-row read filter.
func (s *AgentStore) CountAgentsByPhaseIDs(ctx context.Context, filter store.AgentFilter) ([]store.IDPhase, error) {
	preds, err := agentFilterPredicates(filter)
	if err != nil {
		return nil, err
	}
	query := s.client.Agent.Query()
	if len(preds) > 0 {
		query.Where(preds...)
	}
	rows, err := query.Select(agent.FieldID, agent.FieldPhase).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]store.IDPhase, len(rows))
	for i, a := range rows {
		out[i] = store.IDPhase{ID: a.ID.String(), Phase: a.Phase}
	}
	return out, nil
}

// CountAgents returns the number of agents matching filter, using the exact
// predicate ListAgents applies for its own total count (agentFilterPredicates),
// with no row loaded. It backs the sorted-mode candidate ceiling pre-check:
// a cheap COUNT before any member row is read, so a candidate pool above the
// ceiling costs no more than one query.
func (s *AgentStore) CountAgents(ctx context.Context, filter store.AgentFilter) (int, error) {
	preds, err := agentFilterPredicates(filter)
	if err != nil {
		return 0, err
	}
	query := s.client.Agent.Query()
	if len(preds) > 0 {
		query.Where(preds...)
	}
	return query.Count(ctx)
}

// agentMemberSelectFields is the exact SQL column list ListAgentMembers
// selects: precisely the fields agentResource (pkg/hub/capabilities.go)
// reads (ID, OwnerID, ProjectID, Labels, Ancestry), plus Phase, Created,
// Updated and LastActivityEvent for positioning (pkg/store/agentsort) and
// stats. This list, not a separately maintained one, is the projection's
// definition: widening agentResource's inputs without adding the new field
// here is exactly what the non-waivable equality gate is meant to catch.
// That gate -- a reflection-filled store.Agent written and read back through
// the real ListAgentMembers and GetAgentsByIDs, compared via
// reflect.DeepEqual(memberResource(m), agentResource(full)) -- lives in
// pkg/hub (TestListProjectAgentsSorted_MemberProjectionEquality,
// agent_sorted_project_list_reflection_test.go), not in this package:
// agentResource and memberResource are only visible from package hub.
// TestListAgentMembers_ProjectionEqualsFullRow in this package is a
// narrower, store-only check that the narrow SELECT's columns agree with a
// full-row read; it is not itself reflection-filled and cannot substitute
// for the hub-level gate.
var agentMemberSelectFields = []string{
	agent.FieldID,
	agent.FieldOwnerID,
	agent.FieldProjectID,
	agent.FieldLabels,
	agent.FieldAncestry,
	agent.FieldPhase,
	agent.FieldCreated,
	agent.FieldUpdated,
	agent.FieldLastActivityEvent,
}

// entAgentToMember converts a partial *ent.Agent — one hydrated only from
// agentMemberSelectFields — into a store.AgentMember. Unlike entAgentToStore,
// it never reads a field outside that list, so a row fetched via the narrow
// SELECT below can never appear to carry a value (e.g. an AppliedConfig)
// that was simply never selected.
func entAgentToMember(a *ent.Agent) store.AgentMember {
	m := store.AgentMember{
		ID:        a.ID.String(),
		ProjectID: a.ProjectID.String(),
		Labels:    a.Labels,
		Ancestry:  a.Ancestry,
		Phase:     a.Phase,
		Created:   a.Created,
		Updated:   a.Updated,
	}
	if a.OwnerID != nil {
		m.OwnerID = a.OwnerID.String()
	}
	if a.LastActivityEvent != nil {
		m.LastActivityEvent = *a.LastActivityEvent
	}
	return m
}

// ListAgentMembers returns up to max agents matching filter, projected down
// to the narrow AgentMember shape and ordered per the sorted-mode total
// order for (sort, dir).
//
// The SQL SELECT list is exactly agentMemberSelectFields — no wide column
// (AppliedConfig in particular) is ever read off the wire for a candidate
// row — which is what keeps a 2,000-row candidate scan cheap enough for the
// server's request WriteTimeout, not just what the design's equality gate
// requires.
//
// The candidate set is bounded by the caller's ceiling check to at most a
// couple thousand rows, so this fetches every matching row up to max (with
// no ORDER BY at the SQL level — order does not matter for a candidate pool
// this small, and comparing every row afterward with agentsort.SortRows
// keeps exactly one implementation of the tie-break rules instead of asking
// each dialect to reproduce it) and sorts them in Go.
func (s *AgentStore) ListAgentMembers(ctx context.Context, filter store.AgentFilter, sortKey, dir string, max int) ([]store.AgentMember, error) {
	// Fail closed on an unrecognized sort or dir (unknown values return
	// ErrInvalidInput), rather than letting
	// agentsort.KeyFor/Less silently fall back to a default ordering. The
	// hub handler already validates these before calling in; this is the
	// store API's own contract, independent of any one caller.
	if sortKey != agentsort.Created && sortKey != agentsort.Updated {
		return nil, fmt.Errorf("ListAgentMembers: invalid sort %q: %w", sortKey, store.ErrInvalidInput)
	}
	if dir != agentsort.Asc && dir != agentsort.Desc {
		return nil, fmt.Errorf("ListAgentMembers: invalid dir %q: %w", dir, store.ErrInvalidInput)
	}

	preds, err := agentFilterPredicates(filter)
	if err != nil {
		return nil, err
	}
	query := s.client.Agent.Query()
	if len(preds) > 0 {
		query.Where(preds...)
	}
	if max <= 0 {
		max = defaultAgentListLimit
	}
	rows, err := query.Select(agentMemberSelectFields...).Limit(max).All(ctx)
	if err != nil {
		return nil, err
	}

	members := make([]store.AgentMember, 0, len(rows))
	rowsForSort := make([]agentsort.Row, 0, len(rows))
	for _, a := range rows {
		m := entAgentToMember(a)
		members = append(members, m)
		rowsForSort = append(rowsForSort, agentsort.KeyFor(sortKey, m.ID, m.Created, m.Updated, m.LastActivityEvent))
	}

	// Sort members using the same permutation computed over rowsForSort, by
	// sorting a slice of indices rather than re-deriving keys mid-sort.
	idx := make([]int, len(members))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(i, j int) bool {
		return agentsort.Less(dir, rowsForSort[idx[i]], rowsForSort[idx[j]])
	})
	ordered := make([]store.AgentMember, len(members))
	for i, j := range idx {
		ordered[i] = members[j]
	}
	return ordered, nil
}

// ListAgentsWithStaleNonTerminalReincarnationState is the agent-state
// backstop for the replica-safe reincarnation sweep (design §3.4 Amendment
// A6.6): an agent left claimed with no matching non-terminal
// AgentReincarnation record (for ListStaleNonTerminalAgentReincarnations to
// find — e.g. the claim landed but CreateAgentReincarnation never did, or the
// record was deleted) still gets reset once it looks stale.
//
// Staleness is judged on reincarnation_updated_at, NOT `updated`: every
// broker heartbeat's UpdateAgentStatus bumps `updated` for any agent whose
// container the broker still reports (including a stopped one — heartbeat
// lists via `docker ps -a`), which would otherwise keep an orphaned claim
// looking fresh forever. reincarnation_updated_at is bumped only by
// reincarnation-owned writes (the claim, each worker step, every terminal
// write), so it is a clock only this feature moves. A nil
// reincarnation_updated_at (a row from before this column existed, or one
// set non-terminal by anything else) is treated as immediately eligible —
// there is no better signal, and leaving it un-resettable would be worse.
//
// The "no non-terminal record" half of the backstop's contract is checked
// here explicitly, per agent, rather than relied upon as an ordering
// property of the caller's two loops: an agent that still has one is left
// for the record-side sweep (ListStaleNonTerminalAgentReincarnations,
// followed by sweepFailStaleRecord's agent-state-aware restore decision) to
// own instead.
func (s *AgentStore) ListAgentsWithStaleNonTerminalReincarnationState(ctx context.Context, olderThan time.Time) ([]*store.Agent, error) {
	nonTerminal := []string{
		store.ReincarnationStateStopping,
		store.ReincarnationStateProvisioning,
		store.ReincarnationStateStarting,
		store.ReincarnationStatePending,
	}
	rows, err := s.client.Agent.Query().
		Where(
			agent.ReincarnationStateIn(nonTerminal...),
			agent.Or(
				agent.ReincarnationUpdatedAtIsNil(),
				agent.ReincarnationUpdatedAtLT(olderThan),
			),
		).
		All(ctx)
	if err != nil {
		return nil, err
	}

	recNonTerminal := make([]agentreincarnation.State, 0, len(store.AgentReincarnationNonTerminalStates))
	for _, st := range store.AgentReincarnationNonTerminalStates {
		recNonTerminal = append(recNonTerminal, agentreincarnation.State(st))
	}

	out := make([]*store.Agent, 0, len(rows))
	for _, a := range rows {
		hasNonTerminalRecord, err := s.client.AgentReincarnation.Query().
			Where(
				agentreincarnation.AgentIDEQ(a.ID.String()),
				agentreincarnation.StateIn(recNonTerminal...),
			).
			Exist(ctx)
		if err != nil {
			return nil, err
		}
		if hasNonTerminalRecord {
			continue
		}
		out = append(out, entAgentToStore(a))
	}
	return out, nil
}

// agentBeforeCursor returns a predicate for keyset pagination after the given cursor.
func agentBeforeCursor(cursorCreated time.Time, cursorID uuid.UUID) predicate.Agent {
	return keysetBeforeCursor(agent.FieldCreated, agent.FieldID, cursorCreated, cursorID)
}

// agentFilterPredicates translates a store.AgentFilter into Ent predicates,
// preserving the exact OR/AND composition of the legacy SQLite query.
func agentFilterPredicates(filter store.AgentFilter) ([]predicate.Agent, error) {
	var preds []predicate.Agent

	switch {
	case len(filter.MemberOrOwnerProjectIDs) > 0:
		// (project_id IN (...) OR owner_id = OwnerID)
		projectUIDs := parseUUIDList(filter.MemberOrOwnerProjectIDs)
		var orParts []predicate.Agent
		if len(projectUIDs) > 0 {
			orParts = append(orParts, agent.ProjectIDIn(projectUIDs...))
		}
		if filter.OwnerID != "" {
			ownerUID, err := parseUUID(filter.OwnerID)
			if err != nil {
				return nil, err
			}
			orParts = append(orParts, agent.OwnerIDEQ(ownerUID))
		}
		if len(orParts) > 0 {
			preds = append(preds, agent.Or(orParts...))
		}
	case len(filter.MemberProjectIDs) > 0:
		projectUIDs := parseUUIDList(filter.MemberProjectIDs)
		preds = append(preds, agent.ProjectIDIn(projectUIDs...))
	case filter.OwnerID != "":
		ownerUID, err := parseUUID(filter.OwnerID)
		if err != nil {
			return nil, err
		}
		preds = append(preds, agent.OwnerIDEQ(ownerUID))
	}

	if filter.ExcludeOwnerID != "" {
		excludeUID, err := parseUUID(filter.ExcludeOwnerID)
		if err != nil {
			return nil, err
		}
		preds = append(preds, agent.OwnerIDNEQ(excludeUID))
	}
	if filter.ProjectID != "" {
		projectUID, err := parseUUID(filter.ProjectID)
		if err != nil {
			return nil, err
		}
		preds = append(preds, agent.ProjectIDEQ(projectUID))
	}
	if filter.RuntimeBrokerID != "" {
		preds = append(preds, agent.RuntimeBrokerIDEQ(filter.RuntimeBrokerID))
	}
	switch {
	case filter.Phase != "" && filter.OrRunIntent != "":
		preds = append(preds, agent.Or(agent.PhaseEQ(filter.Phase), agent.RunIntentEQ(filter.OrRunIntent)))
	case filter.Phase != "":
		preds = append(preds, agent.PhaseEQ(filter.Phase))
	case filter.OrRunIntent != "":
		preds = append(preds, agent.RunIntentEQ(filter.OrRunIntent))
	}
	if filter.AncestorID != "" {
		preds = append(preds, ancestryContains(filter.AncestorID))
	}
	for k, v := range filter.Labels {
		preds = append(preds, labelContains(k, v))
	}

	// RequestedOwnerID is always ANDed, independent of the OwnerID/
	// MemberOrOwnerProjectIDs OR-based Mine/Shared classification above
	// (ptone/scion#2146 — see the field doc in pkg/store/store.go).
	if filter.RequestedOwnerID != "" {
		requestedOwnerUID, err := parseUUID(filter.RequestedOwnerID)
		if err != nil {
			return nil, err
		}
		preds = append(preds, agent.OwnerIDEQ(requestedOwnerUID))
	}

	if filter.HarnessConfig != "" {
		preds = append(preds, agent.HarnessConfigEQ(filter.HarnessConfig))
	}

	// IDs: narrowing-only restriction to a specific agent ID set (e.g. a CLI
	// --ancestors relationship query). Fail-closed like AuthorizedProjectIDs:
	// nil means no restriction, empty non-nil means no agents match.
	if filter.IDs != nil {
		if len(filter.IDs) == 0 {
			preds = append(preds, agent.IDEQ(uuid.Nil))
		} else {
			idUUIDs := parseUUIDList(filter.IDs)
			if len(idUUIDs) > 0 {
				preds = append(preds, agent.IDIn(idUUIDs...))
			} else {
				preds = append(preds, agent.IDEQ(uuid.Nil))
			}
		}
	}

	// LineageRootID: the root agent plus all its descendants, as one OR
	// sub-predicate that is itself ANDed with everything else in preds
	// (ptone/scion#2146 — see the field doc in pkg/store/store.go).
	if filter.LineageRootID != "" {
		rootUID, err := parseUUID(filter.LineageRootID)
		if err != nil {
			return nil, err
		}
		preds = append(preds, agent.Or(agent.IDEQ(rootUID), ancestryContains(filter.LineageRootID)))
	}

	// AuthorizedProjectIDs: scope-aware authorization filter applied at the SQL
	// level so pagination and totals reflect only the authorized set.
	// Fail-closed: if all IDs fail UUID parsing, match nothing rather than
	// omitting the predicate (which would return all agents).
	if filter.AuthorizedProjectIDs != nil {
		if len(filter.AuthorizedProjectIDs) == 0 {
			// Empty authorized set: no agents visible.
			preds = append(preds, agent.ProjectIDEQ(uuid.Nil))
		} else {
			projectUIDs := parseUUIDList(filter.AuthorizedProjectIDs)
			if len(projectUIDs) > 0 {
				preds = append(preds, agent.ProjectIDIn(projectUIDs...))
			} else {
				// All IDs failed UUID parsing: fail closed — no agents visible.
				preds = append(preds, agent.ProjectIDEQ(uuid.Nil))
			}
		}
	}

	// RS2: ExcludedProjectIDs — exclude agents from specific projects when
	// project-scoped constraints block the list permission. Fail-closed:
	// malformed exclusion IDs are an authorization predicate error (they
	// represent constraint scope data that cannot be applied, which would
	// silently widen access if skipped).
	if len(filter.ExcludedProjectIDs) > 0 {
		excludeIDs, err := parseUUIDsStrict(filter.ExcludedProjectIDs)
		if err != nil {
			return nil, fmt.Errorf("invalid authorization predicate: ExcludedProjectIDs: %w", err)
		}
		preds = append(preds, agent.ProjectIDNotIn(excludeIDs...))
	}

	// Exclude soft-deleted agents unless explicitly requested.
	if !filter.IncludeDeleted {
		preds = append(preds, agent.DeletedAtIsNil())
	}

	return preds, nil
}

// UpdateAgentStatus applies a partial, status-only update. It is the hottest
// agent write path, so it runs as a locked read-modify-write: the row is loaded
// with SELECT ... FOR UPDATE, the legacy sticky/transition rules are applied in
// Go, and the result is written back inside the same transaction. Unlike
// UpdateAgent it does not touch state_version (status churn is not a
// conflict-worthy mutation).
func (s *AgentStore) UpdateAgentStatus(ctx context.Context, id string, su store.AgentStatusUpdate) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}

	// Prime dialect detection before opening the transaction: the detection
	// probe runs on s.client, which would contend with the open transaction on
	// single-connection SQLite.
	useLock := s.usesRowLocks(ctx)

	tx, err := s.client.Tx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	q := tx.Agent.Query().Where(agent.IDEQ(uid))
	if useLock {
		q = q.ForUpdate()
	}
	current, err := q.Only(ctx)
	if err != nil {
		return mapError(err)
	}

	now := time.Now()

	// Guard 0c, enforced inside the transaction (design ptone/scion#2483
	// §2.1 Guards): while a delete holds a live lease, or once the row is
	// soft-deleted, a status report must not move phase, activity or the
	// exit fields — a report read before the delete claim cannot land after
	// it. The handler applies the same guard (guardAgentPhaseTransition);
	// this repeats it on the locked row.
	if current.DeletedAt != nil || entAgentDeletionActive(current, now) {
		su.Phase = ""
		su.Activity = ""
		su.ExitCode = nil
		su.ExitReason = ""
		su.Message = ""
		su.ClearExit = false
		su.ClearMessageIf = ""
	}

	upd := tx.Agent.UpdateOneID(uid).
		SetUpdated(now).
		SetLastSeen(now)

	if su.Phase != "" {
		upd.SetPhase(su.Phase)
	}

	activityProvided := su.Activity != ""
	if activityProvided {
		// Preserve a terminal/sticky activity: once an agent is stopped with a
		// crashed/limits_exceeded activity, a non-terminal status report must
		// not overwrite it.
		sticky := current.Phase == "stopped" &&
			isTerminalActivity(current.Activity) &&
			!isTerminalActivity(su.Activity)
		if !sticky {
			upd.SetActivity(su.Activity)
		}
		// A fresh activity report clears any stalled marker and refreshes the
		// activity timestamp; tool_name tracks the new activity verbatim.
		upd.SetStalledFromActivity("")
		upd.SetLastActivityEvent(now)
		upd.SetToolName(su.ToolName)
	} else if su.Phase == "stopped" || su.Phase == "error" {
		// Transitioning to a terminal phase without an explicit activity: clear
		// any leftover live activity (e.g. a lingering "stalled" set by the
		// platform) so a stopped/crashed agent never displays a stale activity.
		// A terminal activity (crashed/limits_exceeded) carries information about
		// HOW the agent stopped and is preserved.
		if current.Activity != "" && !isTerminalActivity(current.Activity) {
			upd.SetActivity("")
			upd.SetStalledFromActivity("")
			upd.SetToolName("")
		}
	}

	// A (re)start — a transition from a terminal phase (stopped/error) to running
	// — clears terminal remnants from the prior stop/crash: the stale crash/stop
	// message and any leftover stalled marker. This is gated on the CURRENT phase
	// being terminal so routine running→running heartbeats (which carry their own
	// sticky-stalled rules in the broker handler) are left untouched. An explicit
	// message in the same update (su.Message != "") wins and is set below.
	if su.Phase == "running" && (current.Phase == "stopped" || current.Phase == "error") {
		if su.Message == "" {
			upd.SetMessage("")
		}
		upd.SetStalledFromActivity("")
		upd.ClearExitCode()
		upd.SetExitReason("")
	}

	// ClearExit covers the case the phase-transition clear above does not:
	// a lifecycle start/restart dispatched while the CURRENT phase is
	// already running (or anything else). A disruption reason can be
	// recorded on a still-running agent ahead of its pod actually stopping
	// (state.ExitReasonPreempted/ExitReasonEvicted), and that reason
	// describes the old pod, not the new generation a start/restart brings
	// up, so it must be cleared regardless of the current phase.
	if su.ClearExit {
		upd.ClearExitCode()
		upd.SetExitReason("")
	}

	// T1 async agent create (design t1-async-create-v11.md §3.3): a write of
	// phase=running always clears launch_error (T3) — once an agent has run,
	// it must never again match the incomplete-create predicate, whatever
	// happens to it later. If a launch is still active on this row, the same
	// write also ends it as running_observed, evaluated on the row this
	// method has already locked (not on the caller's struct, which may be
	// stale). No CASE SQL: both are plain Go conditionals on current.
	if su.Phase == "running" {
		upd.SetLaunchError("")
		if current.LaunchState == store.LaunchStateActive {
			upd.SetLaunchState(store.LaunchStateEnded)
			upd.SetLaunchEndReason(store.LaunchEndReasonRunningObserved)
		}
	}

	if su.Message != "" {
		upd.SetMessage(su.Message)
	} else if su.ClearMessageIf != "" && current.Message == su.ClearMessageIf {
		upd.SetMessage("")
	}
	if su.ConnectionState != "" {
		upd.SetConnectionState(su.ConnectionState)
	}
	if su.ContainerStatus != "" {
		upd.SetContainerStatus(su.ContainerStatus)
	}
	if su.ExitCode != nil {
		upd.SetExitCode(*su.ExitCode)
	}
	if su.ExitReason != "" {
		upd.SetExitReason(su.ExitReason)
	}
	if su.RuntimeState != "" {
		upd.SetRuntimeState(su.RuntimeState)
	}
	if su.TaskSummary != "" {
		upd.SetTaskSummary(su.TaskSummary)
	}
	if su.CurrentTurns != nil {
		upd.SetCurrentTurns(*su.CurrentTurns)
	}
	if su.CurrentModelCalls != nil {
		upd.SetCurrentModelCalls(*su.CurrentModelCalls)
	}
	if su.StartedAt != "" {
		if t, ok := parseTimeString(su.StartedAt); ok {
			upd.SetStartedAt(t)
		}
	}

	if err := upd.Exec(ctx); err != nil {
		return mapError(err)
	}
	return tx.Commit()
}

// UpdateAgentExposedPorts applies a partial exposed-port update without using
// the whole-record optimistic-lock path. Port registration must not race with
// high-frequency status writes.
func (s *AgentStore) UpdateAgentExposedPorts(ctx context.Context, id string, ports []store.ExposedPort) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}

	affected, err := s.client.Agent.Update().
		Where(agent.IDEQ(uid)).
		SetExposedPorts(utcExposedPorts(ports)).
		SetUpdated(time.Now()).
		Save(ctx)
	if err != nil {
		return mapError(err)
	}
	if affected == 0 {
		return store.ErrNotFound
	}
	return nil
}

// utcExposedPorts returns a copy of ports with every ExposedAt converted to
// UTC. agents.exposed_ports is a JSON column, so the field-level UTC mutation
// hook cannot reach the embedded times; normalising here covers every writer.
// The caller's slice is not modified. A nil slice stays nil.
func utcExposedPorts(ports []store.ExposedPort) []store.ExposedPort {
	if ports == nil {
		return nil
	}
	out := make([]store.ExposedPort, len(ports))
	for i, p := range ports {
		p.ExposedAt = p.ExposedAt.UTC()
		out[i] = p
	}
	return out
}

// PurgeDeletedAgents permanently removes soft-deleted agents older than cutoff.
func (s *AgentStore) PurgeDeletedAgents(ctx context.Context, cutoff time.Time) (int, error) {
	deleted, err := s.client.Agent.Delete().
		Where(agent.DeletedAtNotNil(), agent.DeletedAtLT(cutoff)).
		Exec(ctx)
	if err != nil {
		return 0, err
	}
	return deleted, nil
}

// staleOfflineExcluded lists the terminal/sticky activities that must not be
// overwritten when sweeping stale agents to "offline".
var staleOfflineExcluded = []string{"completed", "limits_exceeded", "blocked", "offline"}

// MarkStaleAgentsOffline marks running agents whose last heartbeat predates
// threshold as offline, returning the updated records for event publishing.
func (s *AgentStore) MarkStaleAgentsOffline(ctx context.Context, threshold time.Time) ([]store.Agent, error) {
	useLock := s.usesRowLocks(ctx)

	tx, err := s.client.Tx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now()

	q := tx.Agent.Query().Where(
		agent.LastSeenNotNil(),
		agent.LastSeenLT(threshold),
		agent.PhaseEQ("running"),
		agent.ActivityNotIn(staleOfflineExcluded...),
	)
	if useLock {
		q = q.ForUpdate()
	}
	candidates, err := q.All(ctx)
	if err != nil {
		return nil, err
	}

	updated := make([]store.Agent, 0, len(candidates))
	for _, a := range candidates {
		if err := tx.Agent.UpdateOneID(a.ID).
			SetActivity("offline").
			SetUpdated(now).
			Exec(ctx); err != nil {
			return nil, err
		}
		a.Activity = "offline"
		a.Updated = now
		updated = append(updated, *entAgentToStore(a))
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return updated, nil
}

// containerMissingStatus is the container_status recorded by
// MarkAgentContainerMissing.
const containerMissingStatus = "missing"

// containerMissingKeptExitReasons are the exit reasons, more specific than
// container_missing, that MarkAgentContainerMissing keeps (together with the
// stored message and exit code) when the agent already has one: the runtime
// broker recorded why the container went away before it disappeared.
// These match the ExitReason values added by ptone/scion#2542; switch to
// those constants once both changes have landed.
var containerMissingKeptExitReasons = []string{"preempted", "evicted"}

// MarkAgentContainerMissing implements store.AgentStore. See the interface
// documentation for the guard conditions.
//
// Every guard is part of the UPDATE's WHERE clause (a compare-and-set), not
// a separate read: the row changes only if it still matches at write time,
// so a concurrent delete, start, stop, reassignment or heartbeat always wins
// without relying on a row lock. Two such UPDATEs run in one transaction:
// the first matches only a row whose exit reason is one to keep and leaves
// the reason, message and exit code alone; the second, run only when the
// first changed nothing, matches every other row and records
// container_missing.
func (s *AgentStore) MarkAgentContainerMissing(ctx context.Context, id, brokerID string, cutoff time.Time, message string) (*store.Agent, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return nil, err
	}

	tx, err := s.client.Tx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	guards := func(reason predicate.Agent) *ent.AgentUpdate {
		return tx.Agent.Update().
			Where(
				agent.IDEQ(uid),
				agent.DeletedAtIsNil(),
				agent.RuntimeBrokerIDEQ(brokerID),
				agent.PhaseEQ("running"),
				agent.Or(
					agent.ReincarnationStateIsNil(),
					agent.ReincarnationStateIn(store.ReincarnationStateNone, store.ReincarnationStateFailed),
				),
				agent.Or(agent.LastSeenIsNil(), agent.LastSeenLT(cutoff)),
				// A delete in progress owns the phase (design
				// ptone/scion#2483 §2.1 Guards); expired leases included,
				// since teardown may already have run. A failed
				// (rolled-back) delete does not block this write.
				agent.Or(
					agent.DeletionStateIsNil(),
					agent.DeletionStateNotIn(store.DeletionStateDeleting, store.DeletionStateFinalizing),
				),
				reason,
			).
			SetPhase("error").
			SetActivity("").
			SetStalledFromActivity("").
			SetToolName("").
			SetContainerStatus(containerMissingStatus)
	}

	n, err := guards(agent.ExitReasonIn(containerMissingKeptExitReasons...)).Save(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	if n == 0 {
		n, err = guards(agent.Or(
			agent.ExitReasonIsNil(),
			agent.ExitReasonNotIn(containerMissingKeptExitReasons...),
		)).
			ClearExitCode().
			SetExitReason(string(state.ExitReasonContainerMissing)).
			SetMessage(message).
			Save(ctx)
		if err != nil {
			return nil, mapError(err)
		}
	}
	if n == 0 {
		return nil, nil
	}
	updated, err := tx.Agent.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return entAgentToStore(updated), nil
}

// clearRuntimeTargetHook, when set by a test, runs inside each
// ClearAgentRuntimeTarget attempt between the read and the conditional write,
// with the attempt's transaction, so the test can change the row and make the
// write miss.
var clearRuntimeTargetHook func(ctx context.Context, tx *ent.Tx, id uuid.UUID)

// setRuntimeTargetHook, when set by a test, runs inside the
// SetAgentRuntimeTarget transaction after the read and its checks, just
// before the conditional write, so a test can change the row in between.
var setRuntimeTargetHook func(ctx context.Context, tx *ent.Tx, id uuid.UUID)

// clearRuntimeTargetAttempts bounds ClearAgentRuntimeTarget's re-read and
// retry loop.
const clearRuntimeTargetAttempts = 5

// ClearAgentRuntimeTarget implements store.AgentStore. See the interface for
// the contract.
//
// Each attempt reads and conditionally writes inside one transaction, with
// the row locked (SELECT ... FOR UPDATE) where the dialect supports it, so a
// concurrent writer cannot slip in between. The write is additionally
// conditional on the stored applied config and state_version being
// unchanged; if it still misses, the row is re-read and the clear retried, up
// to clearRuntimeTargetAttempts times, after which an error is returned.
func (s *AgentStore) ClearAgentRuntimeTarget(ctx context.Context, id string) (bool, int64, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return false, 0, err
	}
	// Prime dialect detection before opening a transaction (see
	// UpdateAgentStatus).
	useLock := s.usesRowLocks(ctx)
	for i := 0; i < clearRuntimeTargetAttempts; i++ {
		res, err := s.clearRuntimeTargetOnce(ctx, uid, id, useLock)
		if err != nil {
			return false, 0, err
		}
		if res.done {
			return res.cleared, res.newVersion, nil
		}
	}
	return false, 0, fmt.Errorf("clear runtime target for agent %s: agent changed concurrently %d times", id, clearRuntimeTargetAttempts)
}

// runtimeTargetKeys are the applied-config JSON keys ClearAgentRuntimeTarget
// removes.
var runtimeTargetKeys = []string{"runtimeTarget", "runtimeTargetCandidate"}

// clearRuntimeTargetResult is the outcome of one ClearAgentRuntimeTarget
// attempt. done is false only when the conditional write matched no row
// because the agent changed after it was read.
type clearRuntimeTargetResult struct {
	done       bool
	cleared    bool
	newVersion int64
}

// clearRuntimeTargetOnce is one ClearAgentRuntimeTarget attempt.
func (s *AgentStore) clearRuntimeTargetOnce(ctx context.Context, uid uuid.UUID, id string, useLock bool) (clearRuntimeTargetResult, error) {
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return clearRuntimeTargetResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	q := tx.Agent.Query().Where(agent.IDEQ(uid), agent.DeletedAtIsNil())
	if useLock {
		q = q.ForUpdate()
	}
	row, err := q.Select(agent.FieldAppliedConfig, agent.FieldStateVersion).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return clearRuntimeTargetResult{done: true}, nil
		}
		return clearRuntimeTargetResult{}, mapError(err)
	}
	if row.AppliedConfig == "" {
		return clearRuntimeTargetResult{done: true}, nil
	}
	// Edit the raw JSON object rather than round-tripping it through
	// store.AgentAppliedConfig, so every other stored key is kept exactly
	// as written.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(row.AppliedConfig), &raw); err != nil {
		return clearRuntimeTargetResult{}, fmt.Errorf("clear runtime target for agent %s: %w", id, err)
	}
	found := false
	for _, k := range runtimeTargetKeys {
		if _, ok := raw[k]; ok {
			delete(raw, k)
			found = true
		}
	}
	if !found {
		return clearRuntimeTargetResult{done: true}, nil
	}
	cleared, err := json.Marshal(raw)
	if err != nil {
		return clearRuntimeTargetResult{}, err
	}
	if clearRuntimeTargetHook != nil {
		clearRuntimeTargetHook(ctx, tx, uid)
	}
	newVersion := row.StateVersion + 1
	n, err := tx.Agent.Update().
		Where(
			agent.IDEQ(uid),
			agent.DeletedAtIsNil(),
			agent.AppliedConfigEQ(row.AppliedConfig),
			agent.StateVersionEQ(row.StateVersion),
		).
		SetAppliedConfig(string(cleared)).
		SetStateVersion(newVersion).
		Save(ctx)
	if err != nil {
		return clearRuntimeTargetResult{}, mapError(err)
	}
	if n == 0 {
		return clearRuntimeTargetResult{}, nil
	}
	if err := tx.Commit(); err != nil {
		return clearRuntimeTargetResult{}, err
	}
	return clearRuntimeTargetResult{done: true, cleared: true, newVersion: newVersion}, nil
}

// appliedConfigUnchanged matches a row whose applied_config is still the
// value read; an empty read also matches a NULL column.
func appliedConfigUnchanged(read string) predicate.Agent {
	if read == "" {
		return agent.Or(agent.AppliedConfigIsNil(), agent.AppliedConfigEQ(""))
	}
	return agent.AppliedConfigEQ(read)
}

// SetAgentRuntimeTarget implements store.AgentStore. See the interface for
// the contract.
//
// The read and the conditional write run in one transaction, with the row
// locked where the dialect supports it. The write is conditional on
// state_version and the stored applied config being unchanged, and edits
// only the two target keys in the raw JSON, so concurrent status writes
// (which do not touch applied_config) are never undone.
func (s *AgentStore) SetAgentRuntimeTarget(ctx context.Context, id string, expectedVersion int64, target, candidate string) (bool, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return false, err
	}
	// Prime dialect detection before opening a transaction (see
	// UpdateAgentStatus).
	useLock := s.usesRowLocks(ctx)
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	q := tx.Agent.Query().Where(agent.IDEQ(uid), agent.DeletedAtIsNil())
	if useLock {
		q = q.ForUpdate()
	}
	row, err := q.Select(agent.FieldAppliedConfig, agent.FieldStateVersion).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return false, nil
		}
		return false, mapError(err)
	}
	if row.StateVersion != expectedVersion {
		return false, nil
	}
	raw := map[string]json.RawMessage{}
	if row.AppliedConfig != "" {
		if err := json.Unmarshal([]byte(row.AppliedConfig), &raw); err != nil {
			return false, fmt.Errorf("set runtime target for agent %s: %w", id, err)
		}
		if raw == nil { // the stored value was the JSON literal null
			raw = map[string]json.RawMessage{}
		}
	}
	for key, value := range map[string]string{"runtimeTarget": target, "runtimeTargetCandidate": candidate} {
		if value == "" {
			delete(raw, key)
			continue
		}
		enc, err := json.Marshal(value)
		if err != nil {
			return false, err
		}
		raw[key] = enc
	}
	updated, err := json.Marshal(raw)
	if err != nil {
		return false, err
	}
	if setRuntimeTargetHook != nil {
		setRuntimeTargetHook(ctx, tx, uid)
	}
	n, err := tx.Agent.Update().
		Where(
			agent.IDEQ(uid),
			agent.DeletedAtIsNil(),
			agent.StateVersionEQ(expectedVersion),
			appliedConfigUnchanged(row.AppliedConfig),
		).
		SetAppliedConfig(string(updated)).
		Save(ctx)
	if err != nil {
		return false, mapError(err)
	}
	if n == 0 {
		return false, nil
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// stalledExcluded lists the activities that disqualify a running agent from
// being marked "stalled" (terminal, already-stalled, or intentionally waiting).
var stalledExcluded = []string{"completed", "limits_exceeded", "blocked", "stalled", "offline", "waiting_for_input"}

// MarkStalledAgents marks running agents whose last activity event predates
// activityThreshold but whose heartbeat is still recent (>= heartbeatRecency)
// as stalled, preserving the prior activity in stalled_from_activity.
func (s *AgentStore) MarkStalledAgents(ctx context.Context, activityThreshold, heartbeatRecency time.Time) ([]store.Agent, error) {
	useLock := s.usesRowLocks(ctx)

	tx, err := s.client.Tx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now()

	q := tx.Agent.Query().Where(
		agent.LastActivityEventNotNil(),
		agent.LastActivityEventLT(activityThreshold),
		agent.LastSeenNotNil(),
		agent.LastSeenGTE(heartbeatRecency),
		agent.PhaseEQ("running"),
		agent.ActivityNotIn(stalledExcluded...),
	)
	if useLock {
		q = q.ForUpdate()
	}
	candidates, err := q.All(ctx)
	if err != nil {
		return nil, err
	}

	updated := make([]store.Agent, 0, len(candidates))
	for _, a := range candidates {
		prevActivity := a.Activity
		if err := tx.Agent.UpdateOneID(a.ID).
			SetStalledFromActivity(prevActivity).
			SetActivity("stalled").
			SetUpdated(now).
			Exec(ctx); err != nil {
			return nil, err
		}
		a.StalledFromActivity = prevActivity
		a.Activity = "stalled"
		a.Updated = now
		updated = append(updated, *entAgentToStore(a))
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return updated, nil
}

// --- helpers ---

// isTerminalActivity reports whether the activity is a terminal/sticky state
// that a non-terminal status report must not overwrite on a stopped agent.
func isTerminalActivity(activity string) bool {
	return activity == "crashed" || activity == "limits_exceeded"
}

// marshalAppliedConfig serializes the applied-config document to JSON text,
// returning "" for a nil config so the column is left empty.
//
// This is the DB-persistence path, not a response surface: it deliberately
// bypasses AgentAppliedConfig's own MarshalJSON (which hides Env unless
// ResponseView has been called -- see pkg/store/models.go) via a local alias
// type, so every field including Env round-trips to the column exactly as
// held in memory. The response-side gate only ever applies to what an HTTP
// handler serializes back to a caller, never to what gets written to the DB.
func marshalAppliedConfig(cfg *store.AgentAppliedConfig) string {
	if cfg == nil {
		return ""
	}
	type rawAppliedConfig store.AgentAppliedConfig
	data, err := json.Marshal((*rawAppliedConfig)(cfg))
	if err != nil {
		return ""
	}
	return string(data)
}

// harnessConfigOf extracts the top-level harness-config name from cfg, or ""
// if cfg is nil or has none set. This is the single value CreateAgent and
// UpdateAgent write into the harness_config shadow column (see its doc in
// pkg/ent/schema/agent.go) — kept as its own function so both call sites
// derive it identically and cannot drift (ptone/scion#2146). Both call
// sites write this value UNCONDITIONALLY, including "", rather than
// skipping the write or clearing the column to NULL when it's empty — NULL
// is reserved to mean "never written by a binary that knows this column
// exists".
func harnessConfigOf(cfg *store.AgentAppliedConfig) string {
	if cfg == nil {
		return ""
	}
	return cfg.HarnessConfig
}

// parseTimeString parses a status update's started_at string, accepting the
// RFC3339 forms the legacy store persisted. It reports false when the value is
// unparseable so the caller leaves the field unchanged.
func parseTimeString(s string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// parseUUIDList parses a list of string UUIDs, silently skipping any that are
// malformed (mirroring the lenient handling of the legacy IN (...) filters).
func parseUUIDList(ids []string) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if uid, err := uuid.Parse(id); err == nil {
			out = append(out, uid)
		}
	}
	return out
}

// FindOrphanedAgents returns non-terminal agents whose RuntimeBrokerID
// references a broker that is offline or does not exist. Agents assigned to
// currentBrokerID are excluded so that a running multi-broker setup does not
// have its agents reassigned.
func (s *AgentStore) FindOrphanedAgents(ctx context.Context, currentBrokerID string) ([]*store.Agent, error) {
	parsedID, parseErr := uuid.Parse(currentBrokerID)
	if parseErr != nil {
		return nil, fmt.Errorf("invalid currentBrokerID %q: %w", currentBrokerID, parseErr)
	}

	// Collect IDs of all online brokers (excluding the current one, which may
	// not be stamped online yet during startup).
	onlineBrokers, err := s.client.RuntimeBroker.Query().
		Where(
			runtimebroker.StatusEQ(store.BrokerStatusOnline),
			runtimebroker.IDNEQ(parsedID),
		).
		Select(runtimebroker.FieldID).
		All(ctx)
	if err != nil {
		return nil, err
	}

	// Build the set of broker IDs whose agents must NOT be touched:
	// the current broker + every online remote broker.
	excludeIDs := make([]string, 0, len(onlineBrokers)+1)
	excludeIDs = append(excludeIDs, currentBrokerID)
	for _, b := range onlineBrokers {
		excludeIDs = append(excludeIDs, b.ID.String())
	}

	// Terminal phases: agents in these states should not be reassigned.
	terminalPhases := []string{"stopped", "error"}

	rows, err := s.client.Agent.Query().
		Where(
			agent.RuntimeBrokerIDNotIn(excludeIDs...),
			agent.RuntimeBrokerIDNEQ(""),        // Must have a broker assignment
			agent.PhaseNotIn(terminalPhases...), // Not in a terminal state
			agent.DeletedAtIsNil(),              // Not soft-deleted
		).
		All(ctx)
	if err != nil {
		return nil, err
	}

	agents := make([]*store.Agent, 0, len(rows))
	for _, a := range rows {
		sa := entAgentToStore(a)
		agents = append(agents, sa)
	}
	return agents, nil
}

// ReassignAgentsToBroker bulk-updates the RuntimeBrokerID of the given agents
// to the specified broker ID. Returns the number of agents actually updated.
func (s *AgentStore) ReassignAgentsToBroker(ctx context.Context, agents []*store.Agent, brokerID string) (int, error) {
	if len(agents) == 0 {
		return 0, nil
	}

	ids := make([]uuid.UUID, 0, len(agents))
	for _, a := range agents {
		uid, err := uuid.Parse(a.ID)
		if err != nil {
			continue
		}
		ids = append(ids, uid)
	}
	if len(ids) == 0 {
		return 0, nil
	}

	affected, err := s.client.Agent.Update().
		Where(agent.IDIn(ids...)).
		SetRuntimeBrokerID(brokerID).
		Save(ctx)
	if err != nil {
		return 0, err
	}
	return affected, nil
}

// ReassignProjectBroker updates projects whose DefaultRuntimeBrokerID matches
// oldBrokerID to point to newBrokerID. It applies the same multi-broker safety
// guard as FindOrphanedAgents: the old broker must be offline or missing — if
// it is still online, no projects are updated (it's a legitimate remote
// broker). Returns the number of projects updated.
func (s *AgentStore) ReassignProjectBroker(ctx context.Context, oldBrokerID, newBrokerID string) (int, error) {
	if oldBrokerID == newBrokerID {
		return 0, nil
	}

	// Safety guard: check if the old broker is still online. If so, it is a
	// legitimate remote broker and its projects must not be touched.
	oldUID, err := uuid.Parse(oldBrokerID)
	if err != nil {
		return 0, fmt.Errorf("invalid oldBrokerID %q: %w", oldBrokerID, err)
	}

	oldBroker, err := s.client.RuntimeBroker.Query().
		Where(runtimebroker.IDEQ(oldUID)).
		Select(runtimebroker.FieldStatus).
		Only(ctx)
	if err != nil && !ent.IsNotFound(err) {
		return 0, err
	}
	if err == nil && oldBroker.Status == store.BrokerStatusOnline {
		// Old broker is still online — do not reassign its projects.
		return 0, nil
	}

	affected, err := s.client.Project.Update().
		Where(project.DefaultRuntimeBrokerIDEQ(oldBrokerID)).
		SetDefaultRuntimeBrokerID(newBrokerID).
		Save(ctx)
	if err != nil {
		return 0, err
	}
	return affected, nil
}

// AggregateAgentHealth computes health-oriented counts via GROUP BY queries
// instead of loading full agent records. The approach uses three lightweight
// queries:
//  1. COUNT(*) GROUP BY phase            → ByPhase + Total
//  2. COUNT(*) GROUP BY runtime_broker_id, phase, activity → ByBroker
//  3. SELECT name WHERE phase/activity ∈ unhealthy (limit 100 each)
func (s *AgentStore) AggregateAgentHealth(ctx context.Context) (*store.AgentHealthAggregate, error) {
	result := &store.AgentHealthAggregate{
		ByPhase:  make(map[string]int),
		ByBroker: make(map[string]store.AgentBrokerCounts),
	}

	// 1. Count agents by phase (non-deleted only).
	var phaseCounts []struct {
		Phase string `json:"phase"`
		Count int    `json:"count"`
	}
	err := s.client.Agent.Query().
		Where(agent.DeletedAtIsNil()).
		GroupBy(agent.FieldPhase).
		Aggregate(ent.Count()).
		Scan(ctx, &phaseCounts)
	if err != nil {
		return nil, fmt.Errorf("aggregate phase counts: %w", err)
	}
	for _, pc := range phaseCounts {
		result.ByPhase[pc.Phase] += pc.Count
		result.Total += pc.Count
	}

	// 2. Count agents by broker, phase, and activity for per-broker health tallies.
	var brokerCounts []struct {
		BrokerID string `json:"runtime_broker_id"`
		Phase    string `json:"phase"`
		Activity string `json:"activity"`
		Count    int    `json:"count"`
	}
	err = s.client.Agent.Query().
		Where(agent.DeletedAtIsNil(), agent.RuntimeBrokerIDNEQ("")).
		GroupBy(agent.FieldRuntimeBrokerID, agent.FieldPhase, agent.FieldActivity).
		Aggregate(ent.Count()).
		Scan(ctx, &brokerCounts)
	if err != nil {
		return nil, fmt.Errorf("aggregate broker counts: %w", err)
	}
	for _, bc := range brokerCounts {
		bid := bc.BrokerID
		if bid == "" {
			continue
		}
		entry := result.ByBroker[bid]
		entry.Count += bc.Count
		if bc.Phase != "error" && bc.Activity != "stalled" && bc.Activity != "crashed" {
			entry.Healthy += bc.Count
		}
		result.ByBroker[bid] = entry
	}

	// 3. Fetch names of unhealthy agents (capped lists).
	const unhealthyCap = 100

	stalledAgents, err := s.client.Agent.Query().
		Where(agent.DeletedAtIsNil(), agent.ActivityEQ("stalled")).
		Select(agent.FieldName).
		Limit(unhealthyCap).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("query stalled agents: %w", err)
	}
	for _, a := range stalledAgents {
		result.StalledNames = append(result.StalledNames, a.Name)
	}

	crashedAgents, err := s.client.Agent.Query().
		Where(agent.DeletedAtIsNil(), agent.ActivityEQ("crashed")).
		Select(agent.FieldName).
		Limit(unhealthyCap).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("query crashed agents: %w", err)
	}
	for _, a := range crashedAgents {
		result.CrashedNames = append(result.CrashedNames, a.Name)
	}

	erroredAgents, err := s.client.Agent.Query().
		Where(agent.DeletedAtIsNil(), agent.PhaseEQ("error")).
		Select(agent.FieldName).
		Limit(unhealthyCap).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("query errored agents: %w", err)
	}
	for _, a := range erroredAgents {
		result.ErroredNames = append(result.ErroredNames, a.Name)
	}

	return result, nil
}
