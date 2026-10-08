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
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agentidentitykey"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agentrecovery"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/delegationedge"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	entgroup "github.com/GoogleCloudPlatform/scion/pkg/ent/group"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/notification"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/notificationsubscription"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const emptyAgentRoleBackfillMarkerSection = "migration_empty_agent_roles_backfilled"
const delegationEdgeBackfillMarkerSection = "migration_delegation_edge_backfill_v1"
const projectMembersGroupMarkerBackfillSection = "backfill_project_group_markers_done"

// LegacyProjectMembersGroupMarkerMigrationSection is the hub setting that
// records completion of MigrateLegacyProjectMembersGroupMarkers. Exported so
// pkg/hub tests can clear it without repeating the literal.
const LegacyProjectMembersGroupMarkerMigrationSection = "migration_legacy_project_members_group_marker_v1"
const projectAgentsGroupMarkerBackfillSection = "migration_project_agents_group_markers_backfilled"
const adoptionReviewRequiredAnnotation = "scion.io/adoption-review-required"
const githubTokenInjectionModeMarkerSection = "migration_github_token_injection_mode_always"
const agentIdentityKeyBackfillMarkerSection = "migration_agent_identity_keys_backfilled"

// harnessConfigReconcilePageSize is a package variable, not a const, purely
// so tests can lower it (save/restore) to construct a cheap multi-page
// scenario for ReconcileHarnessConfigColumn without seeding hundreds of rows
// (ptone/scion#2146). Production code never changes it.
var harnessConfigReconcilePageSize = 500

// harnessConfigReconcileTestStuckIDs, when non-nil, is a set of agent IDs
// that ReconcileHarnessConfigColumn will skip updating this call, leaving
// them pending (still harness_config IS NULL) — a test-only seam with no
// production use (always nil outside a test). Its purpose: under the
// sentinel design, every row a real run visits leaves the pending set on
// that same visit, which means the WHERE clause alone (harness_config IS
// NULL) shrinks the result set correctly even if the lastID/IDGT keyset
// cursor is broken — the two mechanisms are behaviorally redundant for any
// scenario where nothing legitimately stays pending across a call. That
// makes the cursor otherwise untestable by black-box means (confirmed by
// directly mutating lastID's advancement into a no-op and observing every
// other test in this file still pass). This seam constructs the one
// scenario where the two mechanisms diverge — some rows deliberately stay
// pending within a call, so a correct cursor must still reach and reconcile
// the OTHER rows that sort after them, while a broken cursor re-fetches the
// same stuck rows forever and never makes progress on the rest
// (ptone/scion#2146).
var harnessConfigReconcileTestStuckIDs map[uuid.UUID]bool

// CompositeStore is a fully Ent-backed implementation of store.Store. Every
// domain is served by a dedicated Ent sub-store; CompositeStore embeds them so
// their methods are promoted to satisfy the store.Store interface, while the
// store-level Close/Ping/Migrate operations act on the shared Ent client.
//
// There is no longer a separate raw-SQL store: all Hub state lives in a single
// Ent database.
type CompositeStore struct {
	*AgentStore
	*ProjectStore
	*UserStore
	*SecretStore
	*TemplateStore
	*NotificationStore
	*ScheduleStore
	*MaintenanceStore
	*MessageStore
	*ExternalStore
	*BrokerSecretStore
	*AllowListStore
	*GroupStore
	*BrokerDispatchStore
	*LifecycleHookStore
	*SkillStore
	*SkillRegistryStore
	*HubSettingStore
	*BrokerSettingStore
	*SkillInjectionStore
	*ProjectPreStartHookStore
	*AgentSessionMetricsStore
	*ConversationStore
	*RoleStore
	*DelegationEdgeStore
	*DelegationAdoptionStore
	*AgentHoldStore
	*MembershipLossCheckStore
	*AgentCredentialStore
	*AgentIdentityKeyStore
	*DecisionAuditStore
	*MutationAuditStore
	*QuotaStore
	*AccessConstraintStore
	*ExternalIdentityStore
	*AgentReincarnationStore
	*UserTerminalWorkspaceStore

	client *ent.Client
	inTx   bool // true when this CompositeStore wraps a transaction

	// uatCeilingBackfillPageSize overrides BackfillUATCeilings's page size
	// when non-zero; see defaultUATCeilingBackfillPageSize. Tests set this
	// per instance to exercise pagination without creating hundreds of
	// rows, and without a package-level variable that every store instance
	// (and every test running concurrently) would otherwise share.
	uatCeilingBackfillPageSize int

	// uatBoundaryValidatePageSize overrides ValidateUserAccessTokenBoundaries's
	// page size when non-zero; see defaultUATBoundaryValidatePageSize. It is
	// per instance for the same reason as uatCeilingBackfillPageSize.
	uatBoundaryValidatePageSize int

	// uatBoundaryLogger receives ValidateUserAccessTokenBoundaries's report
	// of invalid rows. Nil means slog.Default(). Tests set it per instance
	// to capture the report without replacing the process-wide logger.
	uatBoundaryLogger *slog.Logger

	// adoptionLogger receives AdoptLegacyDelegationProvenance's summary.
	// Nil means slog.Default().
	adoptionLogger *slog.Logger

	// adoptionHopHook, when set, runs before each pending hop of
	// AdoptLegacyDelegationProvenance; a non-nil error stops the loop as a
	// write failure would. Tests use it to inject failures and concurrent
	// changes.
	adoptionHopHook func(i int, rec *store.DelegationAdoption) error

	// adoptionTxHook, when set, runs inside each hop's transaction after the
	// adoption writes and before the record update; a non-nil error rolls
	// the hop back. Tests use it to inject a write failure mid-hop.
	adoptionTxHook func(tx store.Store, rec *store.DelegationAdoption) error
}

// Compile-time assertion that CompositeStore satisfies the full store.Store
// interface purely through its embedded Ent-backed sub-stores.
var _ store.Store = (*CompositeStore)(nil)

// WithTx executes fn inside a database transaction. All store operations
// performed via the Store passed to fn participate in the same transaction.
// If fn returns nil the transaction is committed; otherwise it is rolled back
// and the error is returned. Nested calls are pass-through.
func (c *CompositeStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	if c.inTx {
		// Already inside a transaction — pass through to avoid double-nesting.
		return fn(c)
	}
	tx, err := c.client.Tx(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	txStore := newTxCompositeStore(tx)

	defer func() {
		// Safety net: if Commit was not called (i.e. fn panicked or returned
		// an error), ensure the transaction is rolled back. Rollback after
		// Commit is a no-op in ent.
		_ = tx.Rollback()
	}()

	if err := fn(txStore); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// newTxCompositeStore returns a CompositeStore whose operations all run in
// tx (WithTx's transactional store).
func newTxCompositeStore(tx *ent.Tx) *CompositeStore {
	txStore := NewCompositeStore(tx.Client())
	txStore.inTx = true
	txStore.AgentStore.inTx = true
	txStore.AccessConstraintStore.inTx = true
	txStore.MembershipLossCheckStore.inTx = true
	txStore.AgentHoldStore.inTx = true
	return txStore
}

// NewCompositeStore creates a store.Store backed entirely by the given Ent
// client. Each domain sub-store shares the same client and therefore the same
// underlying database, so cross-domain foreign keys (e.g. group -> project,
// agent -> project) resolve natively without any shadow synchronization.
func NewCompositeStore(client *ent.Client) *CompositeStore {
	return &CompositeStore{
		AgentStore:                 NewAgentStore(client),
		ProjectStore:               NewProjectStore(client),
		UserStore:                  NewUserStore(client),
		SecretStore:                NewSecretStore(client),
		TemplateStore:              NewTemplateStore(client),
		NotificationStore:          NewNotificationStore(client),
		ScheduleStore:              NewScheduleStore(client),
		MaintenanceStore:           NewMaintenanceStore(client),
		MessageStore:               NewMessageStore(client),
		ExternalStore:              NewExternalStore(client),
		BrokerSecretStore:          NewBrokerSecretStore(client),
		AllowListStore:             NewAllowListStore(client),
		GroupStore:                 NewGroupStore(client),
		BrokerDispatchStore:        NewBrokerDispatchStore(client),
		LifecycleHookStore:         NewLifecycleHookStore(client),
		SkillStore:                 NewSkillStore(client),
		SkillRegistryStore:         NewSkillRegistryStore(client),
		HubSettingStore:            NewHubSettingStore(client),
		BrokerSettingStore:         NewBrokerSettingStore(client),
		SkillInjectionStore:        NewSkillInjectionStore(client),
		ProjectPreStartHookStore:   NewProjectPreStartHookStore(client),
		AgentSessionMetricsStore:   NewAgentSessionMetricsStore(client),
		ConversationStore:          NewConversationStore(client),
		RoleStore:                  NewRoleStore(client),
		DelegationEdgeStore:        NewDelegationEdgeStore(client),
		DelegationAdoptionStore:    NewDelegationAdoptionStore(client),
		AgentHoldStore:             NewAgentHoldStore(client),
		MembershipLossCheckStore:   NewMembershipLossCheckStore(client),
		AgentCredentialStore:       NewAgentCredentialStore(client),
		AgentIdentityKeyStore:      NewAgentIdentityKeyStore(client),
		DecisionAuditStore:         NewDecisionAuditStore(client),
		MutationAuditStore:         NewMutationAuditStore(client),
		QuotaStore:                 NewQuotaStore(client),
		AccessConstraintStore:      NewAccessConstraintStore(client),
		ExternalIdentityStore:      NewExternalIdentityStore(client),
		AgentReincarnationStore:    NewAgentReincarnationStore(client),
		UserTerminalWorkspaceStore: NewUserTerminalWorkspaceStore(client),
		client:                     client,
	}
}

// DeleteAgent hard-deletes an agent and cascade-deletes its notification
// subscriptions and notifications. The former raw-SQL store enforced this via
// ON DELETE CASCADE foreign keys (notification_subscriptions.agent_id ->
// agents(id), notifications.subscription_id -> notification_subscriptions(id)).
// In the Ent schema agent_id is a plain field with no edge, so the cascade is
// performed explicitly here to preserve store parity. Soft delete goes through
// UpdateAgent and is unaffected, so subscriptions are retained for soft-deleted
// agents.
//
// The agent's group memberships are removed first: group_memberships.agent_id
// is ON DELETE SET NULL, so once the agent row is gone its membership rows no
// longer carry the agent ID and could only be found as orphans
// (ptone/scion#2769).
//
// Everything runs in one transaction, so the membership delete and the agent
// row delete commit or roll back together: a failed agent delete leaves the
// agent with its memberships.
//
// On PostgreSQL the agent row is locked FOR UPDATE before its memberships are
// deleted, so this path takes locks agent -> membership -> agent delete, the
// same order as PurgeDeletedAgents and finalize-hard. Deleting the
// memberships first would invert that order and could deadlock (40P01)
// against a concurrent purge or finalize of the same agent.
func (c *CompositeStore) DeleteAgent(ctx context.Context, id string) error {
	if !c.inTx {
		return c.WithTx(ctx, func(tx store.Store) error { return tx.DeleteAgent(ctx, id) })
	}
	if c.client.Driver().Dialect() == dialect.Postgres {
		uid, err := parseUUID(id)
		if err != nil {
			return err
		}
		if _, err := c.client.Agent.Query().Where(agent.IDEQ(uid)).ForUpdate().IDs(ctx); err != nil {
			return err
		}
	}
	if _, err := c.DeleteGroupMembershipsForAgents(ctx, []string{id}); err != nil {
		return err
	}
	if err := c.AgentStore.DeleteAgent(ctx, id); err != nil {
		return err
	}
	return c.deleteAgentDependents(ctx, id)
}

// deleteAgentDependents removes the records DeleteAgent cascades to.
func (c *CompositeStore) deleteAgentDependents(ctx context.Context, id string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	if _, err := c.client.Notification.Delete().
		Where(notification.AgentIDEQ(uid)).Exec(ctx); err != nil {
		return err
	}
	if _, err := c.client.NotificationSubscription.Delete().
		Where(notificationsubscription.AgentIDEQ(uid)).Exec(ctx); err != nil {
		return err
	}
	// agent_reincarnations.agent_id is likewise a plain field with no DB-level
	// FK (same reasoning as AgentCredential — the requester side is
	// polymorphic, so a real FK edge doesn't fit); cascade explicitly.
	if err := c.DeleteAgentReincarnationsForAgent(ctx, id); err != nil {
		return err
	}
	// agent_identity_keys.agent_id is likewise a plain field with no DB-level
	// FK (see agent_identity_key.go); cascade explicitly, or the deleted
	// agent's keys stay reserved forever — including its own slug, which
	// then blocks renaming any agent later created with that same slug.
	if err := c.DeleteAgentIdentityKeys(ctx, id); err != nil {
		return err
	}
	// agent_recovery is keyed by the agent ID with no FK; cascade explicitly.
	if _, err := c.client.AgentRecovery.Delete().
		Where(agentrecovery.IDEQ(uid.String())).Exec(ctx); err != nil {
		return err
	}
	return nil
}

// DeleteProject deletes a project and cascade-deletes its agents (and each
// agent's notification subscriptions/notifications). The former raw-SQL store
// enforced this via agents.grove_id -> groves(id) ON DELETE CASCADE; the Ent
// project->agents edge has no DB-level cascade, so deleting a project while
// agents still reference it would fail with a foreign-key violation. The bulk
// agent delete is a hard delete, so it also removes soft-deleted agents.
//
// Everything runs in one transaction: the agent-ID query, the cascades
// (including the group-membership delete) and the row deletes commit or roll
// back together.
//
// On PostgreSQL the agent-ID query (lockProjectAgentIDs, shared with
// LockProjectAgents) locks the project's agent rows FOR UPDATE, in ascending
// ID order, before their memberships are deleted. Every agent hard-delete
// path (and the project delete) takes locks agent -> membership -> agent
// delete, with the agent locks in ascending ID order: DeleteAgent and
// finalize-hard (one agent), PurgeDeletedAgents (ascending across all
// batches), this method, and ProjectDeletionService, which calls
// LockProjectAgents before its project-group cascade deletes any membership.
// So none of them can deadlock (40P01) against another on overlapping agents.
//
// The user-delete path (deleteUser and the admin allow-list delete in
// pkg/hub) also overlaps ProjectDeletionService's group cascade, which
// deletes each project group's memberships and then its row. Its
// DeleteGroupMembershipsForUser call locks the groups the user owns (FOR NO
// KEY UPDATE, ascending ID) before deleting the user's memberships, so it
// takes owned group rows before membership rows, the same order as the
// cascade, and the user-row delete's owner_id SET NULL finds those rows
// already held. FOR NO KEY UPDATE is the lock that SET NULL takes; unlike FOR
// UPDATE it does not conflict with the FK check's FOR KEY SHARE when a
// membership, child-group edge or binding referencing an owned group is
// inserted.
func (c *CompositeStore) DeleteProject(ctx context.Context, id string) error {
	if !c.inTx {
		return c.WithTx(ctx, func(tx store.Store) error { return tx.DeleteProject(ctx, id) })
	}
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	agentIDs, err := lockProjectAgentIDs(ctx, c.client, uid)
	if err != nil {
		return err
	}
	if len(agentIDs) > 0 {
		if _, err := c.client.Notification.Delete().
			Where(notification.AgentIDIn(agentIDs...)).Exec(ctx); err != nil {
			return err
		}
		if _, err := c.client.NotificationSubscription.Delete().
			Where(notificationsubscription.AgentIDIn(agentIDs...)).Exec(ctx); err != nil {
			return err
		}
		ids := make([]string, 0, len(agentIDs))
		for _, id := range agentIDs {
			ids = append(ids, id.String())
		}
		if _, err := c.client.AgentRecovery.Delete().
			Where(agentrecovery.IDIn(ids...)).Exec(ctx); err != nil {
			return err
		}
		// Before the agent rows go (agent_id is ON DELETE SET NULL; see
		// DeleteAgent).
		if _, err := c.DeleteGroupMembershipsForAgents(ctx, ids); err != nil {
			return err
		}
		if _, err := c.client.Agent.Delete().
			Where(agent.ProjectIDEQ(uid)).Exec(ctx); err != nil {
			return err
		}
	}
	// agent_identity_keys is keyed by project_id directly, so this is a
	// single bulk delete rather than one per agent ID. Same reasoning as the
	// per-agent cascade in DeleteAgent: no DB-level FK, so it must be
	// explicit or a deleted project's keys stay reserved forever.
	if _, err := c.client.AgentIdentityKey.Delete().
		Where(agentidentitykey.ProjectIDEQ(uid)).Exec(ctx); err != nil {
		return err
	}
	return c.ProjectStore.DeleteProject(ctx, id)
}

// purgeDeletedAgentsBatchSize caps how many agent IDs a single purge
// statement's IN(...) clause covers, so a large purge-eligible set is
// processed in bounded chunks rather than one unbounded IN(...) list. A var,
// not a const, so a test can force multi-batch behavior with a small
// purge-eligible set instead of needing hundreds of rows to exercise it.
var purgeDeletedAgentsBatchSize = 500

// purgeDeletedAgentsTestHook, when non-nil, is invoked by PurgeDeletedAgents
// once per batch, after the candidate IDs for that batch are resolved but
// before they are deleted, and is passed the same in-flight transaction the
// purge itself is using. It exists so a test can deterministically write a
// restore into the exact window the eligibility-predicate re-check on the
// delete itself (below) must close. That window is a Postgres-specific
// concern: under READ COMMITTED, a separate connection's restore can commit
// after this purge's candidate query but before its delete, so the delete
// must re-check the predicate itself rather than trust the candidate list.
// SQLite has no such window in this codebase's production configuration --
// applyDatabasePoolDefaults forces MaxOpenConns to 1 for a sqlite driver
// (pkg/config/hub_config.go; load-bearing there for the same reason it is
// here), so no second, independent connection -- let alone transaction --
// can exist at the same time as this one to interleave with it. This hook
// writes through the purge's own transaction instead of a second, genuinely
// concurrent one for exactly that reason: on this package's
// single-connection SQLite test setup, a second connection attempting to
// write while this transaction is still open would deadlock against it, not
// race it. A write made through the given tx is visible to
// every statement the purge issues afterward on that same tx, which is what
// a Postgres restore's write would also be, once committed, to a subsequent
// statement in this transaction under READ COMMITTED. Always nil in
// production.
var purgeDeletedAgentsTestHook func(tx *ent.Tx, batchCandidateIDs []uuid.UUID)

// PurgeDeletedAgents permanently removes soft-deleted agents older than
// cutoff, and their identity-key rows, in one transaction. It exists only
// here: the embedded AgentStore has no purge of its own, because a bare bulk
// delete of agent rows would skip this cascade. Splitting the original
// single-predicate DELETE into a separate select and delete reopened a
// window where an agent restored in between the two would be hard-deleted
// anyway, taking its keys with it. That is closed here two ways: the whole
// purge runs in one transaction, and the eligibility predicate
// (deleted_at IS NOT NULL AND deleted_at < cutoff) is re-applied directly
// on the agent delete itself, not just the initial candidate query
// -- a candidate restored in between no longer matches it at delete time and
// is excluded, regardless of how stale the candidate list has become.
// Because a bulk delete reports only a count, not which rows it removed,
// identity keys are freed only for the subset of candidates that no longer
// exist afterward (a diff against which of them survived), never for one the
// predicate excluded.
//
// agent_identity_keys has no DB-level FK to agents (see
// agent_identity_key.go), so this cascade must be explicit, the same as
// DeleteAgent and DeleteProject, or a purged agent's keys -- including its
// own slug -- stay reserved forever, blocking any later agent from taking
// them.
func (c *CompositeStore) PurgeDeletedAgents(ctx context.Context, cutoff time.Time) (int, error) {
	useLock := c.AgentStore.usesRowLocks(ctx)
	tx, err := c.client.Tx(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	// Ascending ID order, so the batches (and the per-batch locks below)
	// lock rows in ID order across the whole purge, not just within one
	// batch. Otherwise a later batch could lock a lower ID than an earlier
	// one and deadlock against DeleteProject, which locks a project's agents
	// in one ascending pass.
	candidateIDs, err := tx.Agent.Query().
		Where(agent.DeletedAtNotNil(), agent.DeletedAtLT(cutoff)).
		Order(ent.Asc(agent.FieldID)).
		IDs(ctx)
	if err != nil {
		return 0, err
	}

	txStore := newTxCompositeStore(tx)
	var totalDeleted int
	for _, batch := range chunkUUIDs(candidateIDs, purgeDeletedAgentsBatchSize) {
		if purgeDeletedAgentsTestHook != nil {
			purgeDeletedAgentsTestHook(tx, batch)
		}

		// Remove the group memberships of the agents this batch will delete,
		// before the delete (agent_id is ON DELETE SET NULL; see DeleteAgent).
		// The set is re-read under the eligibility predicate, and locked where
		// the database supports row locks, so a candidate restored in between
		// keeps its memberships just as it keeps its row. Rows are locked in
		// ID order, and the batches themselves are in ID order (see
		// candidateIDs), so the whole purge locks in ascending ID order.
		eligibleQuery := tx.Agent.Query().
			Where(agent.IDIn(batch...), agent.DeletedAtNotNil(), agent.DeletedAtLT(cutoff)).
			Order(ent.Asc(agent.FieldID))
		if useLock {
			eligibleQuery = eligibleQuery.ForUpdate()
		}
		eligibleIDs, err := eligibleQuery.IDs(ctx)
		if err != nil {
			return 0, err
		}
		if len(eligibleIDs) > 0 {
			eligible := make([]string, 0, len(eligibleIDs))
			for _, id := range eligibleIDs {
				eligible = append(eligible, id.String())
			}
			if _, err := txStore.DeleteGroupMembershipsForAgents(ctx, eligible); err != nil {
				return 0, err
			}
		}

		deleted, err := tx.Agent.Delete().
			Where(agent.IDIn(batch...), agent.DeletedAtNotNil(), agent.DeletedAtLT(cutoff)).
			Exec(ctx)
		if err != nil {
			return 0, err
		}
		totalDeleted += deleted

		survivorIDs, err := tx.Agent.Query().Where(agent.IDIn(batch...)).IDs(ctx)
		if err != nil {
			return 0, err
		}
		survived := make(map[uuid.UUID]bool, len(survivorIDs))
		for _, id := range survivorIDs {
			survived[id] = true
		}
		removedIDs := make([]uuid.UUID, 0, len(batch)-len(survivorIDs))
		for _, id := range batch {
			if !survived[id] {
				removedIDs = append(removedIDs, id)
			}
		}
		if len(removedIDs) > 0 {
			if _, err := tx.AgentIdentityKey.Delete().
				Where(agentidentitykey.AgentIDIn(removedIDs...)).Exec(ctx); err != nil {
				return 0, err
			}
			removed := make([]string, 0, len(removedIDs))
			for _, id := range removedIDs {
				removed = append(removed, id.String())
			}
			if _, err := tx.AgentRecovery.Delete().
				Where(agentrecovery.IDIn(removed...)).Exec(ctx); err != nil {
				return 0, err
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return totalDeleted, nil
}

// chunkUUIDs splits ids into slices of at most size, preserving order. A nil
// or empty ids yields no chunks, so a range over the result is a no-op. A
// non-positive size yields a single chunk containing all ids.
func chunkUUIDs(ids []uuid.UUID, size int) [][]uuid.UUID {
	if len(ids) == 0 {
		return nil
	}
	if size <= 0 {
		// A non-positive batch size would divide by zero in the capacity hint
		// below and never advance the loop; treat it as "no batching" and
		// return all ids as a single chunk.
		return [][]uuid.UUID{ids}
	}
	chunks := make([][]uuid.UUID, 0, (len(ids)+size-1)/size)
	for i := 0; i < len(ids); i += size {
		end := i + size
		if end > len(ids) {
			end = len(ids)
		}
		chunks = append(chunks, ids[i:end])
	}
	return chunks
}

// Close closes the underlying Ent client.
func (c *CompositeStore) Close() error {
	return c.client.Close()
}

// Ping verifies connectivity to the underlying database.
func (c *CompositeStore) Ping(ctx context.Context) error {
	drv, ok := c.client.Driver().(*entsql.Driver)
	if !ok {
		return fmt.Errorf("ent client driver does not expose a *sql.DB for ping")
	}
	return drv.DB().PingContext(ctx)
}

// Migrate runs Ent's automatic schema migration against the shared client and
// seeds the built-in maintenance operations, matching the behavior of the
// former raw-SQL store (which seeded these as part of its migrations).
func (c *CompositeStore) Migrate(ctx context.Context) error {
	// Backfill null scope_id to empty string before dedup and schema migration.
	// Must run BEFORE dedup: in SQL NULL != NULL, so dedup won't detect
	// duplicate rows where scope_id IS NULL. Converting to '' first lets
	// dedup catch all real duplicates. Also prevents SQLSTATE 23502 when
	// the schema migration applies the NOT NULL constraint.
	if db := c.DB(); db != nil {
		exists, err := c.accessPoliciesTableExists(ctx, db)
		if err != nil {
			return fmt.Errorf("pre-migration null scope_id check: %w", err)
		}
		if exists {
			result, err := db.ExecContext(ctx,
				"UPDATE access_policies SET scope_id = '' WHERE scope_id IS NULL")
			if err != nil {
				return fmt.Errorf("pre-migration null scope_id backfill: %w", err)
			}
			if n, _ := result.RowsAffected(); n > 0 {
				slog.Info("backfilled null scope_id before migration", "rows_updated", n)
			}
		}
	}

	// Deduplicate access_policies before migration adds a unique index.
	// Existing databases may have duplicate (name, scope_type, scope_id) rows
	// (including former NULL scope_id rows now normalized to '') which would
	// cause the UNIQUE constraint migration to fail.
	if err := c.deduplicateAccessPolicies(ctx); err != nil {
		return fmt.Errorf("pre-migration dedup: %w", err)
	}

	// Deduplicate delegation_edges before migration adds a partial unique index.
	// Existing databases that ran the initial backfill and were interrupted may
	// have duplicate active edges that would violate the new constraint.
	if err := c.deduplicateDelegationEdges(ctx); err != nil {
		return fmt.Errorf("pre-migration delegation edge dedup: %w", err)
	}

	if err := entc.AutoMigrate(ctx, c.client); err != nil {
		return err
	}

	// Must run before pkg/hub/storage_migration.go's namespacing migration
	// (called later, outside CompositeStore.Migrate, once the hub boots)
	// walks stored templates, harness configs and skills: that migration and
	// pkg/storage.ResourceStoragePath both resolve a stored scope into a
	// path, and neither has an arm for "grove". See NormalizeLegacyGroveScopes
	// for the full rationale.
	if err := c.NormalizeLegacyGroveScopes(ctx); err != nil {
		return fmt.Errorf("normalize legacy grove scopes: %w", err)
	}

	if err := c.BackfillEmptyAgentRoles(ctx); err != nil {
		return fmt.Errorf("empty agent role backfill: %w", err)
	}
	if err := c.BackfillDelegationEdges(ctx); err != nil {
		return fmt.Errorf("delegation edge backfill: %w", err)
	}
	// After BackfillDelegationEdges, so edges that backfill writes on a very
	// old database are planned too. Its own marker gates it; the backfill
	// marker does not. A failure is not fatal: unadopted hops keep their
	// current denial and the next boot retries.
	//
	// Deferred snapshot: when planning or the snapshot write fails on the
	// first boot, the hub serves requests with no snapshot, and the next
	// boot's snapshot includes rows written in between. Every path rule
	// applies to those rows, and the admin status view reports
	// snapshotTaken=false until a snapshot exists.
	if err := c.AdoptLegacyDelegationProvenance(ctx); err != nil {
		c.adoptionLog().Error("delegation provenance adoption failed (non-fatal); retried on next boot", "error", err)
	}
	if err := c.BackfillProjectMembersGroupMarkers(ctx); err != nil {
		return fmt.Errorf("project members group marker backfill: %w", err)
	}
	// Runs after BackfillProjectMembersGroupMarkers, which now writes the
	// canonical key, so on any database this leaves no legacy marker behind.
	// Non-fatal like the allowlist migration below: a failure only leaves
	// legacy-marked groups unadoptable by project registration, as before,
	// and the migration retries on the next start.
	if err := c.MigrateLegacyProjectMembersGroupMarkers(ctx); err != nil {
		slog.Error("legacy project members group marker migration failed (non-fatal)", "error", err)
	}
	if err := c.BackfillProjectAgentsGroupMarkers(ctx); err != nil {
		return fmt.Errorf("project agents group marker backfill: %w", err)
	}
	if err := c.BackfillAgentIdentityKeys(ctx); err != nil {
		return fmt.Errorf("agent identity key backfill: %w", err)
	}
	if err := c.ReconcileHarnessConfigColumn(ctx); err != nil {
		return fmt.Errorf("harness_config column reconcile: %w", err)
	}
	if err := c.BackfillUATCeilings(ctx); err != nil {
		return fmt.Errorf("user access token ceiling backfill: %w", err)
	}
	if err := c.ValidateUserAccessTokenBoundaries(ctx); err != nil {
		return fmt.Errorf("user access token boundary validation: %w", err)
	}

	// Migrate AllowListEntry records to User(status=invited) records.
	// Runs after schema migration (which adds the "invited" status enum value)
	// so the new status is available. Idempotent — safe to run on every startup.
	if err := c.MigrateAllowListToInvitedUsers(ctx); err != nil {
		slog.Error("allowlist→invited migration failed (non-fatal)", "error", err)
	}

	// Data backfills belong here rather than at the call site: on Postgres this
	// runs under the schema-migration advisory lock, so replicas booting
	// together do not race, and the columns being read are guaranteed to exist.
	if err := c.BackfillGCPVerificationStatus(ctx); err != nil {
		return err
	}
	if err := c.MigrateGitHubTokenInjectionMode(ctx); err != nil {
		return fmt.Errorf("github token injection mode migration: %w", err)
	}
	return c.SeedMaintenanceOperations(ctx)
}

// BackfillEmptyAgentRoles preserves access for pre-role agents before missing
// roles start resolving to the least-privileged role at runtime.
func (c *CompositeStore) BackfillEmptyAgentRoles(ctx context.Context) error {
	if _, err := c.GetHubSetting(ctx, emptyAgentRoleBackfillMarkerSection); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	agents, err := c.client.Agent.Query().All(ctx)
	if err != nil {
		return err
	}

	var updated int
	for _, a := range agents {
		cfg := &store.AgentAppliedConfig{}
		if a.AppliedConfig != "" {
			parsed, err := parseAppliedConfig(a.AppliedConfig)
			if err != nil {
				return fmt.Errorf("parse applied_config for agent %s: %w", a.ID, err)
			}
			if parsed != nil {
				cfg = parsed
			}
		}
		if cfg.AgentRole != "" {
			continue
		}
		cfg.AgentRole = "full"
		cfg.AgentRoleGrandfathered = true
		if err := c.client.Agent.UpdateOneID(a.ID).
			SetAppliedConfig(marshalAppliedConfig(cfg)).
			Exec(ctx); err != nil {
			return err
		}
		updated++
	}
	if updated > 0 {
		slog.Info("backfilled empty agent roles before role default change", "rows_updated", updated)
	}
	_, err = c.UpsertHubSetting(ctx, emptyAgentRoleBackfillMarkerSection,
		json.RawMessage(`{"schema_version":1,"completed":true}`), "migration", 0, "seeded")
	if errors.Is(err, store.ErrRevisionConflict) {
		return nil
	}
	return err
}

// BackfillDelegationEdges creates delegation edges for all existing agents,
// preserving their current authority as the baseline. This must run AFTER
// BackfillEmptyAgentRoles so that role values are populated.
//
// Sponsor decision: OPTION A — grandfather all existing agents with current
// authority. The live delegation ceiling applies going forward. No permanent
// exemption. If an ancestor loses authority after migration, grandfathered
// agents lose it too.
//
// Edge construction logic:
//   - Delegator is determined from agent provenance (CreatedBy → user, Ancestry → parent agent)
//   - For ambiguous provenance, a "system/migration" delegator is used
//   - All edges are marked grandfathered=true
//   - scope_type=project, scope_id=agent.ProjectID, role=current effective role
func (c *CompositeStore) BackfillDelegationEdges(ctx context.Context) error {
	if _, err := c.GetHubSetting(ctx, delegationEdgeBackfillMarkerSection); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	const pageSize = 500
	var offset int
	var totalCreated int

	for {
		agents, err := c.client.Agent.Query().
			Order(ent.Asc(agent.FieldID)).
			Limit(pageSize).
			Offset(offset).
			All(ctx)
		if err != nil {
			return fmt.Errorf("query agents for delegation edge backfill: %w", err)
		}

		for _, a := range agents {
			// Parse the agent's applied config to get the effective role.
			// Default to "none" (floor), NOT "full" — this is a minting
			// operation that creates durable authority records. Fail closed.
			role := "none"
			if a.AppliedConfig != "" {
				parsed, err := parseAppliedConfig(a.AppliedConfig)
				if err != nil {
					// Parse failure: skip this agent entirely (fail closed).
					// Do not create a delegation edge from unparseable data.
					slog.Error("delegation edge backfill: failed to parse applied_config, skipping agent (fail closed)",
						"agent_id", a.ID, "error", err)
					continue
				} else if parsed != nil && parsed.AgentRole != "" {
					role = parsed.AgentRole
				} else {
					slog.Info("delegation edge backfill: empty/no AgentRole in config, using role=none",
						"agent_id", a.ID)
				}
			} else {
				slog.Info("delegation edge backfill: empty AppliedConfig, using role=none",
					"agent_id", a.ID)
			}

			// Determine the delegator from agent provenance.
			delegatorType, delegatorID := determineDelegator(a)

			// Idempotency: check for an existing active edge before
			// creating. If the migration was interrupted before writing
			// the completion marker, a restart re-processes agents from
			// offset 0. Without this check, duplicates would be created.
			existing, qErr := c.client.DelegationEdge.Query().
				Where(
					delegationedge.DelegateTypeEQ(delegationedge.DelegateTypeAgent),
					delegationedge.DelegateIDEQ(a.ID.String()),
					delegationedge.ScopeTypeEQ(delegationedge.ScopeTypeProject),
					delegationedge.ScopeIDEQ(a.ProjectID.String()),
					delegationedge.ActiveEQ(true),
				).
				First(ctx)
			if qErr == nil && existing != nil {
				// Active edge already exists — skip.
				slog.Debug("delegation edge backfill: active edge already exists, skipping",
					"agent_id", a.ID, "edge_id", existing.ID)
				continue
			}

			// Create the delegation edge. Fail the entire migration on
			// write errors — a partial backfill leaves agents without
			// edges, which is a security gap.
			_, err := c.client.DelegationEdge.Create().
				SetDelegatorType(delegationedge.DelegatorType(delegatorType)).
				SetDelegatorID(delegatorID).
				SetDelegateType(delegationedge.DelegateTypeAgent).
				SetDelegateID(a.ID.String()).
				SetScopeType(delegationedge.ScopeTypeProject).
				SetScopeID(a.ProjectID.String()).
				SetRole(role).
				SetActive(true).
				SetGrandfathered(true).
				Save(ctx)
			if err != nil {
				return fmt.Errorf("delegation edge backfill: failed to create edge for agent %s: %w", a.ID, err)
			}
			totalCreated++
		}

		if len(agents) < pageSize {
			break
		}
		offset += pageSize
	}

	if totalCreated > 0 {
		slog.Info("backfilled delegation edges for existing agents", "edges_created", totalCreated)
	}

	_, err := c.UpsertHubSetting(ctx, delegationEdgeBackfillMarkerSection,
		json.RawMessage(`{"schema_version":1,"completed":true}`), "migration", 0, "seeded")
	if errors.Is(err, store.ErrRevisionConflict) {
		return nil
	}
	return err
}

// ReconcileHarnessConfigColumn populates the harness_config shadow column
// (pkg/ent/schema/agent.go) for agents whose column hasn't caught up with
// their applied_config yet, by extracting it from each row's applied_config
// JSON document. New rows never need this: CreateAgent/UpdateAgent keep the
// column in sync going forward (agent_store.go's harnessConfigOf).
//
// Runs on every boot (ptone/scion#2146), not once behind a one-shot marker:
// a marker-gated version would never reconcile a row an old-binary replica
// writes after a new-binary replica has already run and marked the
// migration done, during a mixed-version rollout.
//
// Residual: a row an upgraded binary already wrote (non-NULL
// harness_config, real value or the "" sentinel) whose applied_config
// harness a pre-upgrade binary later changes keeps its stale column value
// until an upgraded binary next writes the row through UpdateAgent
// (status-only writes do not re-sync it) — this reconcile only selects
// NULL rows, so a restart alone does not fix it.
//
// NULL means exactly "never reconciled or synced by any binary that knows
// this column exists." Every write path other than a raw, pre-column
// legacy row writes a real, non-NULL value: CreateAgent/UpdateAgent call
// SetHarnessConfig(harnessConfigOf(cfg)) unconditionally (agent_store.go)
// — including "" when there's no harness — and this function does the same
// for every row it visits, including one whose applied_config has no
// harness, doesn't parse as JSON at all, or uses the legacy pre-0be8382
// "harness" key instead of "harnessConfig". "" never matches a --harness
// filter (agentFilterPredicates only emits agent.HarnessConfigEQ for a
// non-empty requested value), so writing "" instead of leaving NULL is
// invisible to callers, and it's what makes this query converge to empty
// on a caught-up Hub instead of re-selecting the same
// no-harness/invalid/legacy rows on every boot forever.
//
// A row whose applied_config fails to parse as JSON at all does not fail
// the whole migration — matching entAgentToStore's own tolerance for
// corrupt applied_config (log and continue) — but still gets "" written
// per the sentinel rule above.
//
// A row that parses but needed sanitizing (parseAppliedConfig returns a
// non-nil cfg AND a non-nil error, e.g. an invalid GCP metadata mode) is
// NOT treated as invalid: its HarnessConfig is used exactly like a clean
// row, matching entAgentToStore's identical tolerance for the
// response-facing store.Agent.HarnessConfig.
//
// The per-row update is conditioned on
// Where(HarnessConfigIsNil(), AppliedConfigEQ(a.AppliedConfig)) and preserves
// the row's own Updated timestamp via SetUpdated, so this migration never
// overwrites a value another writer set concurrently and never bumps
// updated as a side effect of a purely internal column sync. The
// AppliedConfigEQ half also covers a writer that predates this column
// entirely: a pre-upgrade replica's UpdateAgent rewrites applied_config
// without touching harness_config, so HarnessConfigIsNil() alone would still
// match and let this migration write a harness parsed from the
// now-superseded applied_config it read at SELECT time — permanently, since
// the row would no longer be NULL for a later boot to pick up. Comparing
// applied_config too means any change to it since the page read, from a
// column-aware or a pre-upgrade writer alike, makes the UPDATE match zero
// rows instead. An ent.IsNotFound from that guard (another writer already
// reconciled the row, or changed its applied_config since the page read) or
// a genuine concurrent delete is logged and skipped, not treated as a boot
// failure — every replica runs this on every boot, so all of these races
// are reachable in normal multi-replica operation. Any other update error
// still aborts startup.
//
// Residual: a concurrent write that does not change applied_config at all
// — e.g. UpdateAgentStatus, UpdateAgentExposedPorts, or
// MarkStaleAgentsOffline, none of which sync harness_config — can still
// land between the page read and this row's UPDATE. Such a write bumps
// updated but leaves harness_config NULL and applied_config unchanged, so
// both guard predicates keep matching and SetUpdated(a.Updated) reverts
// that updated bump to the page-read value. A portable guard against this
// does not exist: comparing on Updated instead of/in addition to
// AppliedConfig fails on SQLite, where the timestamp does not round-trip
// for equality. The window is bounded by the time to process one page
// (seconds), and only during boot-time reconcile of NULL rows.
func (c *CompositeStore) ReconcileHarnessConfigColumn(ctx context.Context) error {
	pageSize := harnessConfigReconcilePageSize
	var (
		lastID                         uuid.UUID
		totalUpdated, totalInvalidJSON int
	)

	for {
		query := c.client.Agent.Query().
			Where(agent.HarnessConfigIsNil(), agent.AppliedConfigNotNil()).
			Order(ent.Asc(agent.FieldID)).
			Limit(pageSize)
		if lastID != uuid.Nil {
			query = query.Where(agent.IDGT(lastID))
		}
		agents, err := query.Select(agent.FieldID, agent.FieldAppliedConfig, agent.FieldUpdated).All(ctx)
		if err != nil {
			return fmt.Errorf("query agents for harness_config reconcile: %w", err)
		}
		if len(agents) == 0 {
			break
		}

		for _, a := range agents {
			lastID = a.ID

			// harnessValue defaults to "" — the sentinel for "reconciled,
			// nothing usable found". It's overwritten below only when
			// applied_config both parses and has a non-empty HarnessConfig.
			harnessValue := ""
			parsed, perr := parseAppliedConfig(a.AppliedConfig)
			switch {
			case parsed == nil:
				// Not valid JSON at all (this also covers an empty
				// applied_config string, which fails the same way) —
				// nothing usable to extract, but still gets "" rather than
				// being left NULL forever.
				slog.Warn("harness_config reconcile: applied_config is not valid JSON; recording no harness",
					"agent_id", a.ID, "error", perr)
				totalInvalidJSON++
			case perr != nil:
				// Parsed, but needed sanitizing (e.g. an invalid GCP
				// metadata mode). The rest of the document, including
				// HarnessConfig, is still used — see the doc comment above.
				slog.Warn("harness_config reconcile: applied_config needed sanitizing; harness_config is still used",
					"agent_id", a.ID, "error", perr)
				harnessValue = parsed.HarnessConfig
			default:
				harnessValue = parsed.HarnessConfig
			}

			if harnessConfigReconcileTestStuckIDs[a.ID] {
				// Test-only seam: simulate a row that legitimately
				// stays pending across this call. lastID has already
				// advanced past it above, so pagination still proceeds to
				// later rows within this call; the row itself is revisited
				// on the next call, same as any real row this run couldn't
				// fix.
				continue
			}

			if err := c.client.Agent.UpdateOneID(a.ID).
				Where(agent.HarnessConfigIsNil(), agent.AppliedConfigEQ(a.AppliedConfig)).
				SetHarnessConfig(harnessValue).
				SetUpdated(a.Updated).
				Exec(ctx); err != nil {
				if ent.IsNotFound(err) {
					// The row was concurrently deleted, another writer
					// already synced its harness_config (an upgraded
					// replica's UpdateAgent, or a concurrent reconcile run),
					// or its applied_config changed since the page read (a
					// pre-upgrade replica's UpdateAgent, which doesn't touch
					// harness_config) — any of these means the guard no
					// longer matches, and none is a boot failure. Not
					// overwriting a concurrent writer's fresher value, nor a
					// pre-upgrade writer's change to applied_config, is
					// exactly the point of the guard.
					slog.Debug("harness_config reconcile: agent no longer exists, was already reconciled, or its applied_config changed since the page read, skipping",
						"agent_id", a.ID)
					continue
				}
				return fmt.Errorf("harness_config reconcile: failed to update agent %s: %w", a.ID, err)
			}
			totalUpdated++
		}

		if len(agents) < pageSize {
			break
		}
	}

	if totalUpdated > 0 || totalInvalidJSON > 0 {
		slog.Info("reconciled harness_config column for existing agents",
			"rows_updated", totalUpdated, "rows_invalid_json", totalInvalidJSON)
	}

	return nil
}

// determineDelegator determines the delegator type and ID for a delegation edge
// based on agent provenance:
//  1. If CreatedBy is set → delegator_type="user", delegator_id=CreatedBy
//  2. If Ancestry has a parent agent (len >= 2, second-to-last entry) and
//     CreatedBy differs from Ancestry[0] → delegator_type="agent", delegator_id=parent
//  3. For ambiguous provenance → delegator_type="user", delegator_id="system/migration"
func determineDelegator(a *ent.Agent) (string, string) {
	const systemMigrationPrincipal = "system/migration"

	// If CreatedBy is set, that's the primary provenance signal.
	if a.CreatedBy != nil {
		createdByStr := a.CreatedBy.String()

		// Check if this was created by another agent (ancestry shows it).
		// Ancestry is [root_user, ..., parent_agent] — the parent is the
		// second-to-last element (the last element is the agent itself in
		// some conventions, but typically the parent is the last in the ancestry).
		if len(a.Ancestry) >= 2 {
			// If CreatedBy matches an element that's not the root user,
			// it was likely created by an agent.
			parentCandidate := a.Ancestry[len(a.Ancestry)-1]
			if parentCandidate == createdByStr && parentCandidate != a.Ancestry[0] {
				return store.DelegationPrincipalAgent, parentCandidate
			}
		}

		// CreatedBy is a user
		return store.DelegationPrincipalUser, createdByStr
	}

	// No CreatedBy — check if we can infer from Ancestry.
	if len(a.Ancestry) > 0 {
		// Use root user from ancestry as the delegator.
		return store.DelegationPrincipalUser, a.Ancestry[0]
	}

	// Ambiguous provenance — use system/migration principal.
	// This ensures the agent participates in the delegation model and is
	// NOT silently unbounded (absent edge = no authority for new lookups).
	return store.DelegationPrincipalUser, systemMigrationPrincipal
}

// BackfillProjectMembersGroupMarkers marks legitimate pre-upgrade project
// members groups so project registration can safely reuse them.
func (c *CompositeStore) BackfillProjectMembersGroupMarkers(ctx context.Context) error {
	if _, err := c.GetHubSetting(ctx, projectMembersGroupMarkerBackfillSection); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	groups, err := c.client.Group.Query().
		Where(
			entgroup.SlugHasPrefix("project:"),
			entgroup.SlugHasSuffix(":members"),
		).
		All(ctx)
	if err != nil {
		return err
	}

	var updated, skipped int
	for _, g := range groups {
		if g.ProjectID == nil {
			skipped++
			slog.Warn("skipping unowned project members group marker backfill",
				"group", g.ID, "slug", g.Slug)
			continue
		}
		project, err := c.GetProject(ctx, g.ProjectID.String())
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				skipped++
				slog.Warn("skipping project members group marker backfill for missing project",
					"group", g.ID, "slug", g.Slug, "project_id", g.ProjectID.String())
				continue
			}
			return err
		}
		if expectedSlug := "project:" + project.Slug + ":members"; g.Slug != expectedSlug {
			skipped++
			slog.Warn("skipping mismatched project members group marker backfill",
				"group", g.ID, "slug", g.Slug, "project_id", project.ID, "expected_slug", expectedSlug)
			continue
		}
		if g.Annotations[store.AnnotationProjectMembersGroup] == "true" {
			continue
		}
		annotations := make(map[string]string, len(g.Annotations)+1)
		for k, v := range g.Annotations {
			annotations[k] = v
		}
		annotations[store.AnnotationProjectMembersGroup] = "true"
		if err := c.client.Group.UpdateOneID(g.ID).
			SetAnnotations(annotations).
			Exec(ctx); err != nil {
			return err
		}
		updated++
	}
	slog.Info("backfilled project members group system markers",
		"rows_updated", updated, "rows_skipped", skipped)

	_, err = c.UpsertHubSetting(ctx, projectMembersGroupMarkerBackfillSection,
		json.RawMessage(`{"schema_version":1,"completed":true}`), "migration", 0, "seeded")
	if errors.Is(err, store.ErrRevisionConflict) {
		return nil
	}
	return err
}

// legacyProjectMembersGroupMarkerPageSize is a package variable, not a
// const, so tests can lower it to exercise pagination cheaply. Production
// code never changes it.
var legacyProjectMembersGroupMarkerPageSize = 500

// MigrateLegacyProjectMembersGroupMarkers rewrites the legacy project members
// group marker key (store.LegacyAnnotationProjectMembersGroup), which
// BackfillProjectMembersGroupMarkers wrote before ptone/scion#2556, to the
// canonical key the hub checks (store.AnnotationProjectMembersGroup). Until
// then the hub refused to adopt a legacy-marked members group on project
// re-ensure. The rewrite only changes the annotation: it never sets
// Group.OwnerID or touches memberships or role bindings.
//
// Only groups whose legacy key is "true" are touched. A group that also
// carries the canonical key with a value other than "true" has conflicting
// markers; it is logged and left as is rather than guessed at.
//
// The migration walks all groups in ID-keyset pages and is idempotent: a
// rewritten group no longer carries the legacy key, so a rerun after an
// interruption skips it. Per-group update errors are logged and skipped; the
// completion marker (a hub_settings row) is written only when every legacy
// group was rewritten, so a failed group is retried on the next start.
// Groups with no ProjectID are skipped, matching hasProjectMembersGroupMarker
// in pkg/hub: a marker on such a group is inert either way.
//
// Rolling upgrade: the only window in which a legacy marker can appear after
// the completion marker is a database that has never run
// BackfillProjectMembersGroupMarkers, booted by an old and a new binary at the
// same time. The consequence is the pre-fix behaviour (the group is not
// adopted and project re-ensure logs "refusing to adopt"), with no security
// impact. To re-run the migration, an operator deletes the hub setting
// migration_legacy_project_members_group_marker_v1
// (LegacyProjectMembersGroupMarkerMigrationSection) and restarts the hub.
func (c *CompositeStore) MigrateLegacyProjectMembersGroupMarkers(ctx context.Context) error {
	if _, err := c.GetHubSetting(ctx, LegacyProjectMembersGroupMarkerMigrationSection); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	var rewritten, conflicting, failed int
	var lastID uuid.UUID
	for {
		q := c.client.Group.Query().
			Order(ent.Asc(entgroup.FieldID)).
			Limit(legacyProjectMembersGroupMarkerPageSize)
		if lastID != uuid.Nil {
			q = q.Where(entgroup.IDGT(lastID))
		}
		groups, err := q.All(ctx)
		if err != nil {
			return fmt.Errorf("query groups for legacy members group marker migration: %w", err)
		}
		for _, g := range groups {
			if g.ProjectID == nil {
				continue
			}
			if g.Annotations[store.LegacyAnnotationProjectMembersGroup] != "true" {
				continue
			}
			if v, ok := g.Annotations[store.AnnotationProjectMembersGroup]; ok && v != "true" {
				conflicting++
				slog.Warn("legacy members group marker migration: conflicting marker keys, leaving group unchanged",
					"group", g.ID, "slug", g.Slug, "canonical_value", v)
				continue
			}
			annotations := make(map[string]string, len(g.Annotations))
			for k, v := range g.Annotations {
				annotations[k] = v
			}
			delete(annotations, store.LegacyAnnotationProjectMembersGroup)
			annotations[store.AnnotationProjectMembersGroup] = "true"
			// No revision check: this runs at startup only, using the same
			// pattern as BackfillProjectMembersGroupMarkers, and a concurrent
			// PATCH could lose non-marker annotation edits.
			if err := c.client.Group.UpdateOneID(g.ID).
				SetAnnotations(annotations).
				Exec(ctx); err != nil {
				failed++
				slog.Error("legacy members group marker migration: failed to rewrite group marker",
					"group", g.ID, "slug", g.Slug, "error", err)
				continue
			}
			rewritten++
		}
		if len(groups) < legacyProjectMembersGroupMarkerPageSize {
			break
		}
		lastID = groups[len(groups)-1].ID
	}

	slog.Info("migrated legacy project members group markers",
		"rows_rewritten", rewritten, "rows_conflicting", conflicting, "rows_failed", failed)
	if failed > 0 {
		return fmt.Errorf("legacy members group marker migration: %d group(s) failed, will retry on next start", failed)
	}

	_, err := c.UpsertHubSetting(ctx, LegacyProjectMembersGroupMarkerMigrationSection,
		json.RawMessage(`{"schema_version":1,"completed":true}`), "migration", 0, "seeded")
	if errors.Is(err, store.ErrRevisionConflict) {
		return nil
	}
	return err
}

// BackfillProjectAgentsGroupMarkers marks legitimate pre-upgrade project
// agents groups with the system annotation so the hardened adoption logic
// accepts them. Uses its own marker section, independent of the members
// group backfill.
func (c *CompositeStore) BackfillProjectAgentsGroupMarkers(ctx context.Context) error {
	if _, err := c.GetHubSetting(ctx, projectAgentsGroupMarkerBackfillSection); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	groups, err := c.client.Group.Query().
		Where(
			entgroup.SlugHasPrefix("project:"),
			entgroup.SlugHasSuffix(":agents"),
		).
		All(ctx)
	if err != nil {
		return err
	}

	var updated, skipped int
	for _, g := range groups {
		if g.ProjectID == nil {
			skipped++
			slog.Warn("skipping unowned project agents group marker backfill",
				"group_id", g.ID, "slug", g.Slug)
			continue
		}
		project, err := c.GetProject(ctx, g.ProjectID.String())
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				skipped++
				slog.Warn("skipping project agents group marker backfill for missing project",
					"group_id", g.ID, "slug", g.Slug, "project_id", g.ProjectID.String())
				continue
			}
			return err
		}
		if expectedSlug := "project:" + project.Slug + ":agents"; g.Slug != expectedSlug {
			skipped++
			slog.Warn("skipping mismatched project agents group marker backfill",
				"group_id", g.ID, "slug", g.Slug, "project_id", project.ID, "expected_slug", expectedSlug)
			continue
		}

		// Check for owner mismatch: the signature of a previously-adopted squat.
		groupOwner := ""
		if g.OwnerID != nil {
			groupOwner = g.OwnerID.String()
		}
		ownerMismatch := groupOwner != "" && project.OwnerID != "" && groupOwner != project.OwnerID
		if ownerMismatch {
			slog.Warn("project agents group owner differs from project owner - manual review recommended",
				"group_id", g.ID, "slug", g.Slug, "project_id", project.ID,
				"group_owner", groupOwner, "project_owner", project.OwnerID)
		}

		if g.Annotations[store.AnnotationProjectAgentsGroup] == "true" {
			// D8-fix: even if the system annotation already exists, ensure the
			// adoption-review annotation is set on mismatch (handles interrupted
			// backfills or later ownership changes).
			if ownerMismatch && g.Annotations[adoptionReviewRequiredAnnotation] != "true" {
				annotations := make(map[string]string, len(g.Annotations)+1)
				for k, v := range g.Annotations {
					annotations[k] = v
				}
				annotations[adoptionReviewRequiredAnnotation] = "true"
				if err := c.client.Group.UpdateOneID(g.ID).
					SetAnnotations(annotations).
					Exec(ctx); err != nil {
					return err
				}
				slog.Warn("added adoption-review-required annotation to already-marked group",
					"group_id", g.ID, "slug", g.Slug, "project_id", project.ID,
					"group_owner", groupOwner, "project_owner", project.OwnerID)
				updated++
			} else {
				slog.Info("project agents group already marked",
					"group_id", g.ID, "slug", g.Slug, "project_id", project.ID, "owner_id", groupOwner)
			}
			continue
		}
		annotations := make(map[string]string, len(g.Annotations)+2)
		for k, v := range g.Annotations {
			annotations[k] = v
		}
		annotations[store.AnnotationProjectAgentsGroup] = "true"
		// D8-fix: when an owner mismatch is detected, add a durable annotation
		// so operators can query for suspect groups after the fact instead of
		// grepping startup logs. The group is still marked (not refused) because
		// project ownership transfers legitimately change the project owner
		// without changing the group owner.
		if ownerMismatch {
			annotations[adoptionReviewRequiredAnnotation] = "true"
		}
		if err := c.client.Group.UpdateOneID(g.ID).
			SetAnnotations(annotations).
			Exec(ctx); err != nil {
			return err
		}
		slog.Info("backfilled project agents group system marker",
			"group_id", g.ID, "slug", g.Slug, "project_id", project.ID, "owner_id", groupOwner)
		updated++
	}
	slog.Info("backfilled project agents group system markers",
		"rows_updated", updated, "rows_skipped", skipped)

	_, err = c.UpsertHubSetting(ctx, projectAgentsGroupMarkerBackfillSection,
		json.RawMessage(`{"schema_version":1,"completed":true}`), "migration", 0, "seeded")
	if errors.Is(err, store.ErrRevisionConflict) {
		return nil
	}
	return err
}

// BackfillAgentIdentityKeys populates agent_identity_keys for every
// pre-existing agent, including soft-deleted ones, so the identity-key
// uniqueness invariant covers agents created before this feature existed,
// not just ones created through the create/rename/restore paths that already
// write their own keys.
//
// Agents are processed in a deterministic order (created ASC, then id ASC):
// slug keys take precedence, and created order decides among display-name
// keys, no matter how many times this runs. A collision does not fail the
// migration -- pre-existing data can hold inconsistencies that predate any
// uniqueness invariant, and a clean backfill cannot be guaranteed
// retroactively. The losing agent's key is logged, not silently dropped.
//
// This runs in two passes over that same order, not one pass writing each
// agent's full key set together: pass one inserts every agent's own slug
// key, pass two inserts display-name keys. A Slug is already unique per
// (project_id, slug) at the Agent table level, so no two agents can ever
// collide on their own slug -- pass one alone can never lose a collision.
// Interleaving slug and display-name inserts per-agent, in created order,
// would let an EARLIER agent's display-name key claim a LATER agent's own
// slug before that later agent ever gets to insert it, leaving a live agent
// with no key for its own immutable identifier: it could not be renamed or
// restored (both re-assert the slug key and would find it already
// reserved). Guaranteeing every slug is claimed before any display-name key
// is attempted keeps each live agent's own slug key intact.
//
// Uses api.IdentityKeysFor, the same function restore and rename use, so a
// legacy Name that slugifies to "" is skipped identically everywhere
// (agent_identity_keys.key is NotEmpty; an empty insert would be a
// permanent failure for that agent's every future write). IdentityKeysFor
// is documented to return the slug first and, when present, the
// display-name key second; this function's two passes rely on that order.
//
// The per-agent, per-key insert is idempotent independent of the completion
// marker below: a (project_id, key) row that already belongs to the agent
// currently being processed is left alone (a re-run after a partial
// failure, or the marker manually cleared, inserts nothing new); one that
// belongs to a DIFFERENT agent is treated as a collision, the same as a
// same-run collision.
func (c *CompositeStore) BackfillAgentIdentityKeys(ctx context.Context) error {
	if _, err := c.GetHubSetting(ctx, agentIdentityKeyBackfillMarkerSection); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	agents, err := c.client.Agent.Query().
		Order(ent.Asc(agent.FieldCreated), ent.Asc(agent.FieldID)).
		All(ctx)
	if err != nil {
		return err
	}

	var inserted, collisions int
	insertKey := func(a *ent.Agent, key string) error {
		_, err := c.client.AgentIdentityKey.Create().
			SetProjectID(a.ProjectID).
			SetKey(key).
			SetAgentID(a.ID).
			Save(ctx)
		if err == nil {
			inserted++
			return nil
		}
		if !ent.IsConstraintError(err) {
			return fmt.Errorf("backfill identity key %q for agent %s: %w", key, a.ID, err)
		}
		existing, qErr := c.client.AgentIdentityKey.Query().
			Where(agentidentitykey.ProjectIDEQ(a.ProjectID), agentidentitykey.KeyEQ(key)).
			Only(ctx)
		if qErr != nil {
			return fmt.Errorf("resolve identity key conflict %q for agent %s: %w", key, a.ID, qErr)
		}
		if existing.AgentID == a.ID {
			// Already backfilled this exact (agent, key) pair: a re-run
			// after a partial failure, or the marker was cleared.
			return nil
		}
		collisions++
		slog.Warn("agent identity key backfill: key already held by another agent, skipped",
			"key", key, "project_id", a.ProjectID,
			"kept_agent_id", existing.AgentID, "dropped_agent_id", a.ID)
		return nil
	}

	// Pass 1: every agent's own slug key first, so a later-created agent's
	// slug is always claimed before any earlier-created agent's
	// display-name key could otherwise pre-empt it.
	for _, a := range agents {
		if err := insertKey(a, a.Slug); err != nil {
			return err
		}
	}
	// Pass 2: display-name keys, keep-first by the same created order.
	for _, a := range agents {
		keys := api.IdentityKeysFor(a.Slug, a.Name)
		if len(keys) < 2 {
			continue // no display-name key distinct from the slug
		}
		if err := insertKey(a, keys[1]); err != nil {
			return err
		}
	}
	if inserted > 0 || collisions > 0 {
		slog.Info("backfilled agent identity keys", "rows_inserted", inserted, "collisions_logged", collisions)
	}

	_, err = c.UpsertHubSetting(ctx, agentIdentityKeyBackfillMarkerSection,
		json.RawMessage(`{"schema_version":1,"completed":true}`), "migration", 0, "seeded")
	if errors.Is(err, store.ErrRevisionConflict) {
		return nil
	}
	return err
}

// MigrateGitHubTokenInjectionMode updates existing GITHUB_TOKEN secrets from
// injection_mode "as_needed" to "always". The token must be injected in the
// first env-resolution pass so it is available when sciontool init clones the
// repository for clone-per-agent and worktree-per-agent workspace modes.
// See https://github.com/ptone/scion/issues/1437.
func (c *CompositeStore) MigrateGitHubTokenInjectionMode(ctx context.Context) error {
	if _, err := c.GetHubSetting(ctx, githubTokenInjectionModeMarkerSection); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	db := c.DB()
	if db == nil {
		return nil
	}

	result, err := db.ExecContext(ctx,
		"UPDATE secrets SET injection_mode = 'always' WHERE key = 'GITHUB_TOKEN' AND injection_mode = 'as_needed'")
	if err != nil {
		return fmt.Errorf("update GITHUB_TOKEN injection_mode: %w", err)
	}
	if n, _ := result.RowsAffected(); n > 0 {
		slog.Info("migrated GITHUB_TOKEN secrets to injection_mode=always", "rows_updated", n)
	}

	_, err = c.UpsertHubSetting(ctx, githubTokenInjectionModeMarkerSection,
		json.RawMessage(`{"schema_version":1,"completed":true}`), "migration", 0, "seeded")
	if errors.Is(err, store.ErrRevisionConflict) {
		return nil
	}
	return err
}

// DB returns the underlying *sql.DB, or nil if the client is not backed by a
// database/sql driver. It is an escape hatch for diagnostics and tests that
// need raw SQL access; production code should use the typed store methods.
func (c *CompositeStore) DB() *sql.DB {
	if drv, ok := c.client.Driver().(*entsql.Driver); ok {
		return drv.DB()
	}
	return nil
}

// Dialect returns the ent dialect of the underlying driver (for example
// dialect.SQLite or dialect.Postgres).
func (c *CompositeStore) Dialect() string {
	return c.client.Driver().Dialect()
}
