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
	"sync"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// defaultAgentListLimit and maxAgentListLimit mirror the pagination bounds of
// the legacy SQLite agent store so listing behavior is identical across
// backends.
const (
	defaultAgentListLimit = 50
	maxAgentListLimit     = 200
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
		RuntimeState:        a.RuntimeState,
		StalledFromActivity: a.StalledFromActivity,
		CurrentTurns:        a.CurrentTurns,
		CurrentModelCalls:   a.CurrentModelCalls,
		Image:               a.Image,
		Detached:            a.Detached,
		Runtime:             a.Runtime,
		RuntimeBrokerID:     a.RuntimeBrokerID,
		WebPTYEnabled:       a.WebPtyEnabled,
		TaskSummary:         a.TaskSummary,
		Message:             a.Message,
		Created:             a.Created,
		Updated:             a.Updated,
		Visibility:          a.Visibility,
		Ancestry:            a.Ancestry,
		StateVersion:        a.StateVersion,
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
		var cfg store.AgentAppliedConfig
		if err := json.Unmarshal([]byte(a.AppliedConfig), &cfg); err == nil {
			sa.AppliedConfig = &cfg
		}
	}
	return sa
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
		SetTaskSummary(a.TaskSummary).
		SetMessage(a.Message).
		SetCreated(now).
		SetUpdated(now).
		SetStateVersion(a.StateVersion)

	if a.Visibility != "" {
		create.SetVisibility(a.Visibility)
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

	update := s.client.Agent.Update().
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
		SetVisibility(a.Visibility).
		SetUpdated(now).
		SetStateVersion(newVersion)

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
			return err
		}
		update.SetOwnerID(ownerUID)
	}

	affected, err := update.Save(ctx)
	if err != nil {
		return mapError(err)
	}
	if affected == 0 {
		// No row matched the (id, state_version) pair. Distinguish a missing
		// agent from a stale write so callers can retry conflicts.
		exists, existErr := s.client.Agent.Query().Where(agent.IDEQ(uid)).Exist(ctx)
		if existErr != nil {
			return existErr
		}
		if !exists {
			return store.ErrNotFound
		}
		return store.ErrVersionConflict
	}

	a.Updated = now
	a.StateVersion = newVersion
	return nil
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

// ListAgents returns agents matching the filter criteria.
func (s *AgentStore) ListAgents(ctx context.Context, filter store.AgentFilter, opts store.ListOptions) (*store.ListResult[store.Agent], error) {
	preds, err := agentFilterPredicates(filter)
	if err != nil {
		return nil, err
	}

	query := s.client.Agent.Query()
	if len(preds) > 0 {
		query.Where(preds...)
	}

	totalCount, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, err
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = defaultAgentListLimit
	}
	if limit > maxAgentListLimit {
		limit = maxAgentListLimit
	}

	// Fetch one extra row to detect whether a further page exists.
	rows, err := query.
		Order(agent.ByCreated(entsql.OrderDesc())).
		Limit(limit + 1).
		All(ctx)
	if err != nil {
		return nil, err
	}

	items := make([]store.Agent, 0, len(rows))
	for _, a := range rows {
		items = append(items, *entAgentToStore(a))
	}

	result := &store.ListResult[store.Agent]{
		Items:      items,
		TotalCount: totalCount,
	}
	if len(items) > limit {
		result.Items = items[:limit]
		result.NextCursor = items[limit-1].ID
	}
	return result, nil
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
	if filter.Phase != "" {
		preds = append(preds, agent.PhaseEQ(filter.Phase))
	}
	if filter.AncestorID != "" {
		preds = append(preds, ancestryContains(filter.AncestorID))
	}
	for k, v := range filter.Labels {
		preds = append(preds, labelContains(k, v))
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
	}

	if su.Message != "" {
		upd.SetMessage(su.Message)
	}
	if su.ConnectionState != "" {
		upd.SetConnectionState(su.ConnectionState)
	}
	if su.ContainerStatus != "" {
		upd.SetContainerStatus(su.ContainerStatus)
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
func marshalAppliedConfig(cfg *store.AgentAppliedConfig) string {
	if cfg == nil {
		return ""
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return ""
	}
	return string(data)
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
