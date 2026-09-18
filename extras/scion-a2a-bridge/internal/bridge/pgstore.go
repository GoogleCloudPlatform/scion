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

package bridge

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// PostgresTaskStore implements the SDK taskstore.Store interface backed by
// PostgreSQL, providing durable task state across standalone bridge replicas.
//
// Each task record stores the full a2a.Task JSON payload alongside ownership
// metadata (owner_key derived from project+agent+caller), a monotonic version
// counter for CAS semantics, and timestamps for pagination.
//
// Owner-key derivation uses the same buildOwnerKey logic as ScopedTaskStore,
// ensuring consistent ownership semantics. In standalone mode, this replaces
// both the in-memory taskstore.InMemory and the ScopedTaskStore wrapper: all
// ownership enforcement and list filtering happen at the SQL level.
type PostgresTaskStore struct {
	db       *sql.DB
	ownsPool bool // true if this store opened the pool and should close it
}

// Compile-time check.
var _ taskstore.Store = (*PostgresTaskStore)(nil)

// NewPostgresTaskStore connects to the Postgres database at databaseURL,
// runs schema migrations for the a2a_sdk_tasks table, and returns a
// ready-to-use task store.
func NewPostgresTaskStore(databaseURL string) (*PostgresTaskStore, error) {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open postgres for SDK task store: %w", err)
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping postgres for SDK task store: %w", err)
	}

	s := &PostgresTaskStore{db: db, ownsPool: true}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate SDK task store: %w", err)
	}

	return s, nil
}

// NewPostgresTaskStoreWithDB creates a PostgresTaskStore using an existing
// database connection pool. This avoids opening a second pool when the
// bridge state store already has one (REQ-4: shared pool). The caller
// retains ownership of the pool and must close it after the store.
func NewPostgresTaskStoreWithDB(db *sql.DB) (*PostgresTaskStore, error) {
	s := &PostgresTaskStore{db: db, ownsPool: false}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("migrate SDK task store: %w", err)
	}
	return s, nil
}

// bridgeEventIDKey is the metadata key used to carry the exact bridge
// TaskEvent autoincrement ID through the SDK event pipeline. Used by
// Update to advance last_event_cursor per-event (Constraint 4).
const bridgeEventIDKey = "_bridgeEventID"

// sdkTerminalStates are the canonical SDK task states stored in JSONB
// payload. All SQL predicates that distinguish active from terminal tasks
// MUST use these exact uppercase strings — they match the SDK's
// TaskState.String() output persisted by json.Marshal.
//
// After migration normalization, legacy lowercase values are mapped to
// their canonical counterparts. Runtime predicates use only this list.
var sdkTerminalStates = []string{
	"TASK_STATE_COMPLETED",
	"TASK_STATE_FAILED",
	"TASK_STATE_CANCELED",
	"TASK_STATE_REJECTED",
}

// terminalStatesSQL is the pre-computed SQL predicate fragment for
// terminal state IN/NOT IN clauses. Computed once at init time.
var terminalStatesSQL string

func init() {
	quoted := make([]string, len(sdkTerminalStates))
	for i, s := range sdkTerminalStates {
		quoted[i] = "'" + s + "'"
	}
	terminalStatesSQL = strings.Join(quoted, ",")
}

// legacyToCanonical maps pre-migration lowercase terminal states to
// their canonical uppercase equivalents. Used only during migration
// normalization — runtime predicates use sdkTerminalStates exclusively.
var legacyToCanonical = map[string]string{
	"completed": "TASK_STATE_COMPLETED",
	"canceled":  "TASK_STATE_CANCELED",
	"failed":    "TASK_STATE_FAILED",
	"rejected":  "TASK_STATE_REJECTED",
}

// Close closes the underlying connection pool only if this store owns it.
// When the pool is shared (created via NewPostgresTaskStoreWithDB), Close
// is a no-op — the pool owner is responsible for closing it.
func (s *PostgresTaskStore) Close() error {
	if s.ownsPool {
		return s.db.Close()
	}
	return nil
}

// GetByIDAndAgent retrieves a task by ID with project+agent validation.
// Used by correlateToTask for durable cross-replica correlation (Constraint 1).
// Rejects empty projectID/agentSlug at the function level — rows with empty
// defaults (pre-migration) never match.
func (s *PostgresTaskStore) GetByIDAndAgent(ctx context.Context, taskID, projectID, agentSlug string) (*taskstore.StoredTask, string, error) {
	if projectID == "" || agentSlug == "" {
		return nil, "", fmt.Errorf("empty correlation key: %w", a2a.ErrTaskNotFound)
	}
	var payload []byte
	var version int64
	var callerUserID string
	err := s.db.QueryRowContext(ctx,
		`SELECT payload, version, caller_user_id FROM a2a_sdk_tasks
		 WHERE id = $1 AND project_id = $2 AND agent_slug = $3`,
		taskID, projectID, agentSlug,
	).Scan(&payload, &version, &callerUserID)
	if err == sql.ErrNoRows {
		return nil, "", a2a.ErrTaskNotFound
	}
	if err != nil {
		return nil, "", fmt.Errorf("get SDK task by agent: %w", err)
	}
	var task a2a.Task
	if err := json.Unmarshal(payload, &task); err != nil {
		return nil, "", fmt.Errorf("unmarshal SDK task: %w", err)
	}
	return &taskstore.StoredTask{
		Task:    &task,
		Version: taskstore.TaskVersion(version),
	}, callerUserID, nil
}

// GetOwnedTaskSnapshotAndCursor retrieves the task snapshot and its
// last_event_cursor in a single query, enforcing owner_key ownership.
// Used by DurableRequestHandler.SubscribeToTask (Constraint 3).
// Returns ErrTaskNotFound on mismatch — no metadata leak.
func (s *PostgresTaskStore) GetOwnedTaskSnapshotAndCursor(
	ctx context.Context, taskID string, ownerKey string,
) (*taskstore.StoredTask, int64, error) {
	if ownerKey == "" {
		return nil, 0, fmt.Errorf("empty owner_key: %w", a2a.ErrUnauthenticated)
	}
	var payload []byte
	var version int64
	var cursor int64
	err := s.db.QueryRowContext(ctx,
		`SELECT payload, version, last_event_cursor
		 FROM a2a_sdk_tasks
		 WHERE id = $1 AND owner_key = $2`,
		taskID, ownerKey,
	).Scan(&payload, &version, &cursor)
	if err == sql.ErrNoRows {
		return nil, 0, a2a.ErrTaskNotFound
	}
	if err != nil {
		return nil, 0, fmt.Errorf("get owned task snapshot: %w", err)
	}
	var task a2a.Task
	if err := json.Unmarshal(payload, &task); err != nil {
		return nil, 0, fmt.Errorf("unmarshal SDK task: %w", err)
	}
	return &taskstore.StoredTask{
		Task:    &task,
		Version: taskstore.TaskVersion(version),
	}, cursor, nil
}

// FindActiveSDKTaskForAgent finds the single most-recently-created non-terminal
// SDK task for the given project+agent. Used by correlateToTask when there is
// no a2aTaskId metadata — the no-metadata branch. Returns ErrTaskNotFound if
// no match or multiple active tasks exist (fail closed on ambiguity).
// Also returns the stored caller_user_id for topic validation.
func (s *PostgresTaskStore) FindActiveSDKTaskForAgent(ctx context.Context, projectID, agentSlug string) (string, string, error) {
	if projectID == "" || agentSlug == "" {
		return "", "", fmt.Errorf("empty correlation key: %w", a2a.ErrTaskNotFound)
	}
	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT id, caller_user_id FROM a2a_sdk_tasks
		 WHERE project_id = $1 AND agent_slug = $2
		   AND payload->'status'->>'state' NOT IN (%s)
		 ORDER BY created_at DESC
		 LIMIT 2`, terminalStatesSQL),
		projectID, agentSlug,
	)
	if err != nil {
		return "", "", fmt.Errorf("find active SDK task: %w", err)
	}
	defer rows.Close()

	var taskID, callerUserID string
	count := 0
	for rows.Next() {
		count++
		if count == 1 {
			if err := rows.Scan(&taskID, &callerUserID); err != nil {
				return "", "", fmt.Errorf("scan active SDK task: %w", err)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return "", "", fmt.Errorf("iterate active SDK tasks: %w", err)
	}
	if count == 0 {
		return "", "", a2a.ErrTaskNotFound
	}
	if count > 1 {
		// Ambiguous — multiple active tasks. Fail closed.
		return "", "", fmt.Errorf("ambiguous: %d active SDK tasks for %s/%s", count, projectID, agentSlug)
	}
	return taskID, callerUserID, nil
}

// sdkTaskStoreMigrationLockID is a Postgres advisory lock ID used to
// serialize schema migrations across replicas (REQ-4).
const sdkTaskStoreMigrationLockID = 827419618 // arbitrary stable int

func (s *PostgresTaskStore) migrate() error {
	// Pin a single connection for the entire lock/migration/unlock lifecycle.
	// pg_advisory_lock is session-scoped — using pool-level Exec could
	// acquire and unlock on different connections, leaking the lock.
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		return fmt.Errorf("acquire migration conn: %w", err)
	}
	defer conn.Close()

	// Acquire advisory lock on the pinned connection.
	if _, err := conn.ExecContext(context.Background(), `SELECT pg_advisory_lock($1)`, sdkTaskStoreMigrationLockID); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, sdkTaskStoreMigrationLockID)

	// Build normalization statements for each legacy→canonical mapping.
	// Each lowercase terminal state is mapped to its correct canonical
	// counterpart — completed→TASK_STATE_COMPLETED, etc. — not all to failed.
	var legacyNormStatements []string
	for legacy, canonical := range legacyToCanonical {
		legacyNormStatements = append(legacyNormStatements,
			fmt.Sprintf(`UPDATE a2a_sdk_tasks
			 SET payload = jsonb_set(payload, '{status,state}', '"%s"'::jsonb),
			     version = version + 1
			 WHERE payload->'status'->>'state' = '%s'`, canonical, legacy),
		)
	}

	// All terminal states for the terminalization WHERE clause: both canonical
	// and legacy values must be excluded (legacy rows should not be re-terminalized
	// if they're already terminal, even in lowercase form).
	allTerminals := make([]string, 0, len(sdkTerminalStates)+len(legacyToCanonical))
	for _, s := range sdkTerminalStates {
		allTerminals = append(allTerminals, "'"+s+"'")
	}
	for legacy := range legacyToCanonical {
		allTerminals = append(allTerminals, "'"+legacy+"'")
	}
	allTerminalsSQL := strings.Join(allTerminals, ",")

	migrations := []string{
		`CREATE TABLE IF NOT EXISTS a2a_sdk_tasks (
			id TEXT PRIMARY KEY,
			context_id TEXT NOT NULL DEFAULT '',
			owner_key TEXT NOT NULL,
			version BIGINT NOT NULL DEFAULT 1,
			payload JSONB NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			exec_owner TEXT,
			exec_heartbeat TIMESTAMPTZ,
			project_id TEXT NOT NULL DEFAULT '',
			agent_slug TEXT NOT NULL DEFAULT '',
			caller_user_id TEXT NOT NULL DEFAULT '',
			last_event_cursor BIGINT NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_a2a_sdk_tasks_owner ON a2a_sdk_tasks(owner_key)`,
		`CREATE INDEX IF NOT EXISTS idx_a2a_sdk_tasks_context_owner ON a2a_sdk_tasks(context_id, owner_key)`,
		`CREATE INDEX IF NOT EXISTS idx_a2a_sdk_tasks_updated ON a2a_sdk_tasks(owner_key, updated_at DESC, id DESC)`,
		// REQ-5: Partial index for janitor reap queries.
		`CREATE INDEX IF NOT EXISTS idx_a2a_sdk_tasks_exec ON a2a_sdk_tasks(exec_heartbeat) WHERE exec_owner IS NOT NULL`,
		// Migration for existing tables: add exec_owner and exec_heartbeat columns.
		`DO $$ BEGIN
			ALTER TABLE a2a_sdk_tasks ADD COLUMN IF NOT EXISTS exec_owner TEXT;
			ALTER TABLE a2a_sdk_tasks ADD COLUMN IF NOT EXISTS exec_heartbeat TIMESTAMPTZ;
		EXCEPTION WHEN duplicate_column THEN NULL;
		END $$`,
		// Constraint 1: Add durable correlation columns for cross-replica ownership.
		`ALTER TABLE a2a_sdk_tasks ADD COLUMN IF NOT EXISTS project_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE a2a_sdk_tasks ADD COLUMN IF NOT EXISTS agent_slug TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE a2a_sdk_tasks ADD COLUMN IF NOT EXISTS caller_user_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE a2a_sdk_tasks ADD COLUMN IF NOT EXISTS last_event_cursor BIGINT NOT NULL DEFAULT 0`,
		// Constraint 7: Terminalize pre-migration rows with empty correlation columns.
		// Fail closed — rows with empty project_id/agent_slug can never be authorized.
		// Empty-ownership active rows are deliberately set to TASK_STATE_FAILED.
		fmt.Sprintf(`UPDATE a2a_sdk_tasks
		 SET payload = jsonb_set(
		         jsonb_set(payload, '{status,state}', '"TASK_STATE_FAILED"'::jsonb),
		         '{status,message}',
		         '{"role":"agent","parts":[{"text":"Terminalized during schema migration"}]}'::jsonb
		     ),
		     exec_owner = NULL,
		     exec_heartbeat = NULL,
		     version = version + 1
		 WHERE project_id = '' AND agent_slug = ''
		   AND payload->'status'->>'state' NOT IN (%s)`, allTerminalsSQL),
	}

	// Append per-state legacy normalization (completed→COMPLETED, etc.).
	migrations = append(migrations, legacyNormStatements...)

	// Drop the old partial index (which may use wrong lowercase predicates)
	// and recreate with canonical uppercase values.
	migrations = append(migrations,
		`DROP INDEX IF EXISTS idx_a2a_sdk_tasks_agent`,
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_a2a_sdk_tasks_agent
		     ON a2a_sdk_tasks(project_id, agent_slug)
		     WHERE payload->'status'->>'state' NOT IN (%s)`, terminalStatesSQL),
	)

	for _, m := range migrations {
		if _, err := conn.ExecContext(context.Background(), m); err != nil {
			return fmt.Errorf("exec migration: %w", err)
		}
	}
	return nil
}

// ownerKeyFromContext extracts the ownership key from the request context
// using the same buildOwnerKey logic as ScopedTaskStore.
func ownerKeyFromContext(ctx context.Context) (string, error) {
	key, ok, err := buildOwnerKey(ctx)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("missing route info: %w", a2a.ErrUnauthenticated)
	}
	if key == "" {
		return "", fmt.Errorf("empty owner key: %w", a2a.ErrUnauthenticated)
	}
	return key, nil
}

// Create creates a new task in the Postgres store.
func (s *PostgresTaskStore) Create(ctx context.Context, task *a2a.Task) (taskstore.TaskVersion, error) {
	if task == nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("task is nil: %w", a2a.ErrInvalidRequest)
	}

	owner, err := ownerKeyFromContext(ctx)
	if err != nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("task creation rejected: %w", err)
	}

	payload, err := json.Marshal(task)
	if err != nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("marshal task: %w", err)
	}

	// Derive durable correlation fields from context (Constraint 1, C4-wrapper).
	// context.WithoutCancel preserves values, so RouteInfo and CallerIdentity
	// are available in the consumer goroutine's detached context.
	var projectID, agentSlug, callerUserID string
	if route, ok := RouteInfoFrom(ctx); ok {
		projectID = route.ProjectSlug
		agentSlug = route.AgentSlug
	}
	if caller := callerIdentityFromContext(ctx); caller != nil {
		callerUserID = caller.UserID
	}

	const version = taskstore.TaskVersion(1)
	now := time.Now().UTC()

	_, execErr := s.db.ExecContext(ctx,
		`INSERT INTO a2a_sdk_tasks (id, context_id, owner_key, version, payload, created_at, updated_at,
		     project_id, agent_slug, caller_user_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		string(task.ID), task.ContextID, owner, int64(version), payload, now, now,
		projectID, agentSlug, callerUserID,
	)
	if execErr != nil {
		if isUniqueViolation(execErr) {
			return taskstore.TaskVersionMissing, taskstore.ErrTaskAlreadyExists
		}
		return taskstore.TaskVersionMissing, fmt.Errorf("create SDK task: %w", execErr)
	}

	return version, nil
}

// Update updates a task using CAS semantics on the version counter.
func (s *PostgresTaskStore) Update(ctx context.Context, req *taskstore.UpdateRequest) (taskstore.TaskVersion, error) {
	if req == nil || req.Task == nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("update request or task is nil: %w", a2a.ErrInvalidRequest)
	}

	owner, err := ownerKeyFromContext(ctx)
	if err != nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("task update rejected: %w", err)
	}

	payload, err := json.Marshal(req.Task)
	if err != nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("marshal task: %w", err)
	}

	now := time.Now().UTC()

	// Constraint 4: Extract the bridge event ID from the SDK event's metadata.
	// This is the exact autoincrement ID from a2a_task_events, carried through
	// the SDK pipeline via _bridgeEventID. We advance last_event_cursor to
	// this specific event (not MAX(id)) to prevent skipping concurrent events.
	var eventCursor int64
	if req.Event != nil {
		if mc, ok := req.Event.(interface{ Meta() map[string]any }); ok {
			if m := mc.Meta(); m != nil {
				switch v := m[bridgeEventIDKey].(type) {
				case int64:
					eventCursor = v
				case float64:
					eventCursor = int64(v)
				}
			}
		}
	}

	if req.Task.Status.State == a2a.TaskStateCanceled {
		return s.updateCanceledTask(ctx, req, owner, payload, now)
	}

	// Use CAS: only update if the version matches (when PrevVersion is tracked).
	if req.PrevVersion != taskstore.TaskVersionMissing {
		var newVersion int64
		err := s.db.QueryRowContext(ctx,
			`UPDATE a2a_sdk_tasks
			 SET payload = $1, version = version + 1, updated_at = $2, context_id = $3,
			     last_event_cursor = GREATEST(last_event_cursor, $7)
			 WHERE id = $4 AND owner_key = $5 AND version = $6
			 RETURNING version`,
			payload, now, req.Task.ContextID, string(req.Task.ID), owner, int64(req.PrevVersion), eventCursor,
		).Scan(&newVersion)
		if err == sql.ErrNoRows {
			// Distinguish between "not found" and "version mismatch".
			exists, existsErr := s.taskExistsForOwner(ctx, string(req.Task.ID), owner)
			if existsErr != nil {
				return taskstore.TaskVersionMissing, fmt.Errorf("check task existence: %w", existsErr)
			}
			if exists {
				return taskstore.TaskVersionMissing, taskstore.ErrConcurrentModification
			}
			return taskstore.TaskVersionMissing, a2a.ErrTaskNotFound
		}
		if err != nil {
			return taskstore.TaskVersionMissing, fmt.Errorf("update SDK task: %w", err)
		}
		return taskstore.TaskVersion(newVersion), nil
	}

	// Version not tracked — update unconditionally (still enforce ownership).
	var newVersion int64
	err = s.db.QueryRowContext(ctx,
		`UPDATE a2a_sdk_tasks
		 SET payload = $1, version = version + 1, updated_at = $2, context_id = $3,
		     last_event_cursor = GREATEST(last_event_cursor, $6)
		 WHERE id = $4 AND owner_key = $5
		 RETURNING version`,
		payload, now, req.Task.ContextID, string(req.Task.ID), owner, eventCursor,
	).Scan(&newVersion)
	if err == sql.ErrNoRows {
		return taskstore.TaskVersionMissing, a2a.ErrTaskNotFound
	}
	if err != nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("update SDK task: %w", err)
	}
	return taskstore.TaskVersion(newVersion), nil
}

// updateCanceledTask atomically commits the authoritative canceled snapshot,
// releases the execution lease, inserts one task-scoped final bridge event,
// and advances the snapshot cursor to that event. This is the convergence
// boundary consumed by a blocking executor on another replica.
func (s *PostgresTaskStore) updateCanceledTask(
	ctx context.Context,
	req *taskstore.UpdateRequest,
	owner string,
	payload []byte,
	now time.Time,
) (taskstore.TaskVersion, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("begin canceled task update: %w", err)
	}
	defer tx.Rollback()

	var newVersion int64
	if req.PrevVersion != taskstore.TaskVersionMissing {
		err = tx.QueryRowContext(ctx,
			`UPDATE a2a_sdk_tasks
			 SET payload=$1, version=version+1, updated_at=$2, context_id=$3,
			     exec_owner=NULL, exec_heartbeat=NULL
			 WHERE id=$4 AND owner_key=$5 AND version=$6
			 RETURNING version`,
			payload, now, req.Task.ContextID, string(req.Task.ID), owner, int64(req.PrevVersion),
		).Scan(&newVersion)
	} else {
		err = tx.QueryRowContext(ctx,
			`UPDATE a2a_sdk_tasks
			 SET payload=$1, version=version+1, updated_at=$2, context_id=$3,
			     exec_owner=NULL, exec_heartbeat=NULL
			 WHERE id=$4 AND owner_key=$5
			 RETURNING version`,
			payload, now, req.Task.ContextID, string(req.Task.ID), owner,
		).Scan(&newVersion)
	}
	if err == sql.ErrNoRows {
		var exists bool
		if existsErr := tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM a2a_sdk_tasks WHERE id=$1 AND owner_key=$2)`,
			string(req.Task.ID), owner,
		).Scan(&exists); existsErr != nil {
			return taskstore.TaskVersionMissing, fmt.Errorf("check task existence: %w", existsErr)
		}
		if exists && req.PrevVersion != taskstore.TaskVersionMissing {
			return taskstore.TaskVersionMissing, taskstore.ErrConcurrentModification
		}
		return taskstore.TaskVersionMissing, a2a.ErrTaskNotFound
	}
	if err != nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("update canceled SDK task: %w", err)
	}

	taskID := string(req.Task.ID)
	dedupKey := "sdk-cancel:" + taskID
	cancelPayload, err := json.Marshal(TaskStatusUpdate{
		TaskID: taskID,
		Status: TaskStatus{State: TaskStateCanceled},
	})
	if err != nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("marshal cancel boundary: %w", err)
	}
	var eventID int64
	err = tx.QueryRowContext(ctx,
		`INSERT INTO a2a_task_events (task_id, kind, payload, final, dedup_key, created_at)
		 VALUES ($1, 'status', $2, true, $3, NOW())
		 ON CONFLICT (task_id, dedup_key) WHERE dedup_key IS NOT NULL DO NOTHING
		 RETURNING id`,
		taskID, json.RawMessage(cancelPayload), dedupKey,
	).Scan(&eventID)
	if err == sql.ErrNoRows {
		err = tx.QueryRowContext(ctx,
			`SELECT id FROM a2a_task_events WHERE task_id=$1 AND dedup_key=$2`,
			taskID, dedupKey,
		).Scan(&eventID)
	}
	if err != nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("insert cancel boundary: %w", err)
	}
	if _, err = tx.ExecContext(ctx,
		`UPDATE a2a_sdk_tasks SET last_event_cursor=GREATEST(last_event_cursor, $1)
		 WHERE id=$2 AND owner_key=$3`, eventID, taskID, owner); err != nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("advance cancel cursor: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("commit canceled task update: %w", err)
	}

	// NOTIFY is only an accelerator; committed polling remains authoritative.
	_, _ = s.db.ExecContext(ctx, "SELECT pg_notify('a2a_task_event', $1)", taskID)
	return taskstore.TaskVersion(newVersion), nil
}

// Get retrieves a task by ID, enforcing ownership.
func (s *PostgresTaskStore) Get(ctx context.Context, taskID a2a.TaskID) (*taskstore.StoredTask, error) {
	owner, err := ownerKeyFromContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("task get rejected: %w", err)
	}

	var payload []byte
	var version int64
	err = s.db.QueryRowContext(ctx,
		`SELECT payload, version FROM a2a_sdk_tasks WHERE id = $1 AND owner_key = $2`,
		string(taskID), owner,
	).Scan(&payload, &version)
	if err == sql.ErrNoRows {
		return nil, a2a.ErrTaskNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get SDK task: %w", err)
	}

	var task a2a.Task
	if err := json.Unmarshal(payload, &task); err != nil {
		return nil, fmt.Errorf("unmarshal SDK task: %w", err)
	}

	return &taskstore.StoredTask{
		Task:    &task,
		Version: taskstore.TaskVersion(version),
	}, nil
}

// List returns tasks matching the request filters, scoped to the caller's
// ownership key. Supports pagination with base64-encoded cursor tokens.
func (s *PostgresTaskStore) List(ctx context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	owner, err := ownerKeyFromContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("task list rejected: %w", err)
	}
	if owner == "" {
		return nil, a2a.ErrUnauthenticated
	}

	const defaultPageSize = 50
	pageSize := req.PageSize
	if pageSize == 0 {
		pageSize = defaultPageSize
	}
	if pageSize < 1 || pageSize > 100 {
		return nil, fmt.Errorf("page size must be between 1 and 100 inclusive, got %d: %w", pageSize, a2a.ErrInvalidRequest)
	}

	// Build query with filters.
	query := `SELECT payload, version, updated_at FROM a2a_sdk_tasks WHERE owner_key = $1`
	args := []interface{}{owner}
	argIdx := 2

	if req.ContextID != "" {
		query += fmt.Sprintf(" AND context_id = $%d", argIdx)
		args = append(args, req.ContextID)
		argIdx++
	}

	// Status and timestamp filters are applied on the JSON payload.
	if req.Status != a2a.TaskStateUnspecified {
		query += fmt.Sprintf(" AND payload->>'status' IS NOT NULL AND payload->'status'->>'state' = $%d", argIdx)
		args = append(args, string(req.Status))
		argIdx++
	}

	if req.StatusTimestampAfter != nil {
		query += fmt.Sprintf(" AND (payload->'status'->>'timestamp')::timestamptz >= $%d", argIdx)
		args = append(args, *req.StatusTimestampAfter)
		argIdx++
	}

	// Count total matching tasks (before pagination).
	countQuery := "SELECT COUNT(*) FROM (" + query + ") AS filtered"
	var totalSize int
	if err := s.db.QueryRowContext(ctx, countQuery, args...).Scan(&totalSize); err != nil {
		return nil, fmt.Errorf("count SDK tasks: %w", err)
	}

	// Apply cursor-based pagination.
	if req.PageToken != "" {
		cursorTime, cursorID, decErr := decodePgPageToken(req.PageToken)
		if decErr != nil {
			return nil, decErr
		}
		query += fmt.Sprintf(
			" AND (updated_at < $%d OR (updated_at = $%d AND id < $%d))",
			argIdx, argIdx+1, argIdx+2,
		)
		args = append(args, cursorTime, cursorTime, string(cursorID))
		argIdx += 3
	}

	// Order by updated_at DESC, then id DESC (consistent with in-memory store).
	query += " ORDER BY updated_at DESC, id DESC"
	query += fmt.Sprintf(" LIMIT $%d", argIdx)
	args = append(args, pageSize+1) // fetch one extra to detect next page
	argIdx++

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list SDK tasks: %w", err)
	}
	defer rows.Close()

	type rowData struct {
		payload   []byte
		version   int64
		updatedAt time.Time
	}
	var rowDatas []rowData
	for rows.Next() {
		var rd rowData
		if err := rows.Scan(&rd.payload, &rd.version, &rd.updatedAt); err != nil {
			return nil, fmt.Errorf("scan SDK task: %w", err)
		}
		rowDatas = append(rowDatas, rd)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate SDK tasks: %w", err)
	}

	// Determine next page token.
	var nextPageToken string
	if len(rowDatas) > pageSize {
		rowDatas = rowDatas[:pageSize]
		last := rowDatas[pageSize-1]
		var lastTask a2a.Task
		if err := json.Unmarshal(last.payload, &lastTask); err != nil {
			return nil, fmt.Errorf("unmarshal last task for pagination: %w", err)
		}
		nextPageToken = encodePgPageToken(last.updatedAt, lastTask.ID)
	}

	// Build response tasks.
	const defaultMaxHistoryLength = 100
	tasks := make([]*a2a.Task, 0, len(rowDatas))
	for _, rd := range rowDatas {
		var task a2a.Task
		if err := json.Unmarshal(rd.payload, &task); err != nil {
			return nil, fmt.Errorf("unmarshal SDK task: %w", err)
		}

		// Apply history length limit.
		historyLength := defaultMaxHistoryLength
		if req.HistoryLength != nil {
			historyLength = *req.HistoryLength
		}
		if historyLength == 0 {
			task.History = []*a2a.Message{}
		} else if historyLength > 0 && len(task.History) > historyLength {
			task.History = task.History[len(task.History)-historyLength:]
		}

		// Conditionally exclude artifacts.
		if !req.IncludeArtifacts {
			task.Artifacts = nil
		}

		tasks = append(tasks, &task)
	}

	return &a2a.ListTasksResponse{
		Tasks:         tasks,
		TotalSize:     totalSize,
		PageSize:      pageSize,
		NextPageToken: nextPageToken,
	}, nil
}

// taskExistsForOwner checks whether a task exists with the given owner.
func (s *PostgresTaskStore) taskExistsForOwner(ctx context.Context, taskID, owner string) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM a2a_sdk_tasks WHERE id = $1 AND owner_key = $2)`,
		taskID, owner,
	).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists, nil
}

// isUniqueViolation checks if the error is a Postgres unique constraint violation.
func isUniqueViolation(err error) bool {
	var sqlStateErr interface{ SQLState() string }
	return errors.As(err, &sqlStateErr) && sqlStateErr.SQLState() == "23505"
}

// ClaimExecution atomically claims execution ownership of a task. The ownerID
// identifies this replica (typically hostname:pid). Only succeeds if no other
// replica currently holds the claim, or if the previous holder's heartbeat
// has expired (older than leaseTimeout). This prevents duplicate execution
// sends to the Hub.
func (s *PostgresTaskStore) ClaimExecution(ctx context.Context, taskID, ownerID string, leaseTimeout time.Duration) (bool, error) {
	result, err := s.db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE a2a_sdk_tasks
		 SET exec_owner = $1, exec_heartbeat = NOW()
		 WHERE id = $2
		   AND (exec_owner IS NULL OR exec_heartbeat < NOW() - $3::interval)
		   AND payload->'status'->>'state' NOT IN (%s)`, terminalStatesSQL),
		ownerID, taskID, fmt.Sprintf("%d seconds", int(leaseTimeout.Seconds())),
	)
	if err != nil {
		return false, fmt.Errorf("claim execution: %w", err)
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}

// HeartbeatExecution refreshes the execution heartbeat for a task, keeping
// the lease alive during long-running operations. Only succeeds if this
// replica is the current owner.
func (s *PostgresTaskStore) HeartbeatExecution(ctx context.Context, taskID, ownerID string) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE a2a_sdk_tasks SET exec_heartbeat = NOW()
		 WHERE id = $1 AND exec_owner = $2`,
		taskID, ownerID,
	)
	if err != nil {
		return fmt.Errorf("heartbeat execution: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return fmt.Errorf("execution lease not held by %s for task %s", ownerID, taskID)
	}
	return nil
}

// ReleaseExecution clears the execution claim after normal completion.
// Only succeeds if this replica is the current owner.
func (s *PostgresTaskStore) ReleaseExecution(ctx context.Context, taskID, ownerID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE a2a_sdk_tasks SET exec_owner = NULL, exec_heartbeat = NULL
		 WHERE id = $1 AND exec_owner = $2`,
		taskID, ownerID,
	)
	if err != nil {
		return fmt.Errorf("release execution: %w", err)
	}
	return nil
}

// ReapStaleTasks transitions tasks with expired execution leases to a
// deterministic "failed" state AND inserts a durable Final=true failure
// event into a2a_task_events — both within a single transaction per task.
//
// Only tasks that have an active exec_owner whose exec_heartbeat has
// expired are eligible — long-running tasks without an execution claim
// are not affected. This handles crash recovery: if a replica dies
// mid-execution, the lease expires and another replica's reaper
// transitions the task to failed.
//
// Each transition is atomic via CAS on the version column within a
// single transaction that also inserts the terminal event with a
// dedup_key of "reap:<taskID>". This eliminates the crash window
// between state update and event insertion. Concurrent reapers on
// multiple replicas produce exactly one winner per task due to CAS.
// Retry after crash is safe: the CAS guard prevents double-transition,
// and the dedup_key prevents duplicate events.
//
// Returns the IDs of reaped tasks. Both startup recovery and periodic
// janitor MUST use this single method.
func (s *PostgresTaskStore) ReapStaleTasks(ctx context.Context, leaseTimeout time.Duration) ([]string, error) {
	// Find tasks with expired execution leases.
	rows, err := s.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT id, owner_key, version, payload, exec_owner FROM a2a_sdk_tasks
		 WHERE exec_owner IS NOT NULL
		   AND exec_heartbeat < NOW() - $1::interval
		   AND payload->'status'->>'state' NOT IN (%s)
		 LIMIT 100`, terminalStatesSQL),
		fmt.Sprintf("%d seconds", int(leaseTimeout.Seconds())),
	)
	if err != nil {
		return nil, fmt.Errorf("list stale SDK tasks: %w", err)
	}
	defer rows.Close()

	type staleTask struct {
		id        string
		owner     string
		version   int64
		payload   []byte
		execOwner string
	}
	var staleTasks []staleTask
	for rows.Next() {
		var st staleTask
		if err := rows.Scan(&st.id, &st.owner, &st.version, &st.payload, &st.execOwner); err != nil {
			return nil, fmt.Errorf("scan stale task: %w", err)
		}
		staleTasks = append(staleTasks, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate stale tasks: %w", err)
	}

	var reapedIDs []string
	var reapErrs []error
	for _, st := range staleTasks {
		reaped, reapErr := s.reapOneTask(ctx, st.id, st.version, st.payload, st.execOwner)
		if reapErr != nil {
			// CAS loss returns (false, nil) — this is a genuine error.
			reapErrs = append(reapErrs, fmt.Errorf("task %s: %w", st.id, reapErr))
			continue
		}
		if reaped {
			reapedIDs = append(reapedIDs, st.id)
		}
	}
	// Return partial successes alongside aggregated errors so the caller
	// can log failures and retry on the next tick.
	return reapedIDs, errors.Join(reapErrs...)
}

// reapOneTask atomically transitions a single stale task to failed and
// inserts the terminal failure event, all within one transaction.
func (s *PostgresTaskStore) reapOneTask(ctx context.Context, taskID string, version int64, payload []byte, execOwner string) (bool, error) {
	// Unmarshal, transition to failed, re-marshal.
	var task a2a.Task
	if err := json.Unmarshal(payload, &task); err != nil {
		return false, fmt.Errorf("unmarshal stale task: %w", err)
	}
	task.Status.State = a2a.TaskStateFailed
	newPayload, err := json.Marshal(&task)
	if err != nil {
		return false, fmt.Errorf("marshal failed task: %w", err)
	}

	// Build the terminal failure event payload.
	failPayload, err := json.Marshal(TaskStatusUpdate{
		TaskID: taskID,
		Status: TaskStatus{State: TaskStateFailed, Message: &Message{
			Role:  "agent",
			Parts: []Part{{Text: "Execution lease expired; replica presumed crashed"}},
		}},
	})
	if err != nil {
		return false, fmt.Errorf("marshal reap event: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin reap tx: %w", err)
	}
	defer tx.Rollback()

	// CAS update: only succeed if version and exec_owner still match
	// and the task is still non-terminal.
	result, err := tx.ExecContext(ctx,
		fmt.Sprintf(`UPDATE a2a_sdk_tasks
		 SET payload = $1, version = version + 1, updated_at = NOW(),
		     exec_owner = NULL, exec_heartbeat = NULL
		 WHERE id = $2 AND version = $3 AND exec_owner = $4
		   AND payload->'status'->>'state' NOT IN (%s)`, terminalStatesSQL),
		newPayload, taskID, version, execOwner,
	)
	if err != nil {
		return false, fmt.Errorf("reap CAS update: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return false, nil // CAS lost — another replica won
	}

	// Insert terminal failure event with Final=true in the same transaction.
	// Dedup key prevents duplicate events on retry.
	_, err = tx.ExecContext(ctx,
		`INSERT INTO a2a_task_events (task_id, kind, payload, final, dedup_key, created_at)
		 VALUES ($1, $2, $3, $4, $5, NOW())
		 ON CONFLICT (task_id, dedup_key) WHERE dedup_key IS NOT NULL DO NOTHING`,
		taskID, "status", json.RawMessage(failPayload), true, fmt.Sprintf("reap:%s", taskID),
	)
	if err != nil {
		return false, fmt.Errorf("insert reap event: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit reap tx: %w", err)
	}

	// Best-effort notification outside transaction.
	_, _ = s.db.ExecContext(ctx, "SELECT pg_notify('a2a_task_event', $1)", taskID)

	return true, nil
}

// PurgeTasksAndEvents deletes terminal SDK tasks and their correlated bridge
// events older than the given cutoff in a single transaction, maintaining
// referential consistency between a2a_sdk_tasks and a2a_task_events.
func (s *PostgresTaskStore) PurgeTasksAndEvents(ctx context.Context, olderThan time.Time) (tasksPurged, eventsPurged int64, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("begin purge tx: %w", err)
	}
	defer tx.Rollback()

	// Delete correlated events for terminal tasks being purged.
	// Uses terminalStatesSQL (canonical states only — legacy normalized during migration).
	evResult, err := tx.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM a2a_task_events
		 WHERE task_id IN (
		     SELECT id FROM a2a_sdk_tasks
		     WHERE updated_at < $1
		       AND payload->'status'->>'state' IN (%s)
		 )`, terminalStatesSQL),
		olderThan,
	)
	if err != nil {
		return 0, 0, fmt.Errorf("purge correlated events: %w", err)
	}
	eventsPurged, _ = evResult.RowsAffected()

	// Delete terminal SDK tasks.
	taskResult, err := tx.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM a2a_sdk_tasks
		 WHERE updated_at < $1
		   AND payload->'status'->>'state' IN (%s)`, terminalStatesSQL),
		olderThan,
	)
	if err != nil {
		return 0, 0, fmt.Errorf("purge SDK tasks: %w", err)
	}
	tasksPurged, _ = taskResult.RowsAffected()

	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("commit purge tx: %w", err)
	}
	return tasksPurged, eventsPurged, nil
}

// OwnerID returns a stable identifier for this replica process, suitable
// for use as exec_owner. Format: hostname:pid.
func OwnerID() string {
	hostname, _ := os.Hostname()
	return fmt.Sprintf("%s:%d", hostname, os.Getpid())
}

// encodePgPageToken encodes a cursor as base64(timestamp_taskID).
func encodePgPageToken(updatedTime time.Time, taskID a2a.TaskID) string {
	timeStr := updatedTime.Format(time.RFC3339Nano)
	return base64.URLEncoding.EncodeToString([]byte(fmt.Sprintf("%s_%s", timeStr, taskID)))
}

// decodePgPageToken decodes a cursor from base64.
func decodePgPageToken(token string) (time.Time, a2a.TaskID, error) {
	decoded, err := base64.URLEncoding.DecodeString(token)
	if err != nil {
		return time.Time{}, "", a2a.ErrParseError
	}
	parts := strings.SplitN(string(decoded), "_", 2)
	if len(parts) != 2 {
		return time.Time{}, "", a2a.ErrParseError
	}
	t, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, "", a2a.ErrParseError
	}
	return t, a2a.TaskID(parts[1]), nil
}
