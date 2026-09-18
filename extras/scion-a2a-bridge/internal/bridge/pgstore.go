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

// Close closes the underlying connection pool only if this store owns it.
// When the pool is shared (created via NewPostgresTaskStoreWithDB), Close
// is a no-op — the pool owner is responsible for closing it.
func (s *PostgresTaskStore) Close() error {
	if s.ownsPool {
		return s.db.Close()
	}
	return nil
}

// sdkTaskStoreMigrationLockID is a Postgres advisory lock ID used to
// serialize schema migrations across replicas (REQ-4).
const sdkTaskStoreMigrationLockID = 827419618 // arbitrary stable int

func (s *PostgresTaskStore) migrate() error {
	// Acquire advisory lock to prevent DDL races across replicas.
	if _, err := s.db.Exec(`SELECT pg_advisory_lock($1)`, sdkTaskStoreMigrationLockID); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer s.db.Exec(`SELECT pg_advisory_unlock($1)`, sdkTaskStoreMigrationLockID)

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
			exec_heartbeat TIMESTAMPTZ
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
	}
	for _, m := range migrations {
		if _, err := s.db.Exec(m); err != nil {
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

	const version = taskstore.TaskVersion(1)
	now := time.Now().UTC()

	_, execErr := s.db.ExecContext(ctx,
		`INSERT INTO a2a_sdk_tasks (id, context_id, owner_key, version, payload, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		string(task.ID), task.ContextID, owner, int64(version), payload, now, now,
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

	// Use CAS: only update if the version matches (when PrevVersion is tracked).
	if req.PrevVersion != taskstore.TaskVersionMissing {
		var newVersion int64
		err := s.db.QueryRowContext(ctx,
			`UPDATE a2a_sdk_tasks
			 SET payload = $1, version = version + 1, updated_at = $2, context_id = $3
			 WHERE id = $4 AND owner_key = $5 AND version = $6
			 RETURNING version`,
			payload, now, req.Task.ContextID, string(req.Task.ID), owner, int64(req.PrevVersion),
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
		 SET payload = $1, version = version + 1, updated_at = $2, context_id = $3
		 WHERE id = $4 AND owner_key = $5
		 RETURNING version`,
		payload, now, req.Task.ContextID, string(req.Task.ID), owner,
	).Scan(&newVersion)
	if err == sql.ErrNoRows {
		return taskstore.TaskVersionMissing, a2a.ErrTaskNotFound
	}
	if err != nil {
		return taskstore.TaskVersionMissing, fmt.Errorf("update SDK task: %w", err)
	}
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
	return err != nil && strings.Contains(err.Error(), "duplicate key value violates unique constraint")
}

// ClaimExecution atomically claims execution ownership of a task. The ownerID
// identifies this replica (typically hostname:pid). Only succeeds if no other
// replica currently holds the claim, or if the previous holder's heartbeat
// has expired (older than leaseTimeout). This prevents duplicate execution
// sends to the Hub.
func (s *PostgresTaskStore) ClaimExecution(ctx context.Context, taskID, ownerID string, leaseTimeout time.Duration) (bool, error) {
	result, err := s.db.ExecContext(ctx,
		`UPDATE a2a_sdk_tasks
		 SET exec_owner = $1, exec_heartbeat = NOW()
		 WHERE id = $2
		   AND (exec_owner IS NULL OR exec_heartbeat < NOW() - $3::interval)
		   AND payload->'status'->>'state' NOT IN ('TASK_STATE_COMPLETED', 'TASK_STATE_FAILED', 'TASK_STATE_CANCELED', 'TASK_STATE_REJECTED')`,
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
// deterministic "failed" state. Only tasks that have an active exec_owner
// whose exec_heartbeat has expired are eligible — long-running tasks without
// an execution claim are not affected. This handles crash recovery: if a
// replica dies mid-execution, the lease expires and another replica's reaper
// transitions the task to failed, allowing safe user retry.
//
// Each transition is atomic via CAS on the version column, so concurrent
// reapers on multiple replicas produce exactly one winner per task.
// Returns the count and IDs of reaped tasks so the caller can emit terminal
// failure events to the event log (REQ-6).
func (s *PostgresTaskStore) ReapStaleTasks(ctx context.Context, leaseTimeout time.Duration) ([]string, error) {
	// Find tasks with expired execution leases.
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, owner_key, version, payload, exec_owner FROM a2a_sdk_tasks
		 WHERE exec_owner IS NOT NULL
		   AND exec_heartbeat < NOW() - $1::interval
		   AND payload->'status'->>'state' NOT IN ('TASK_STATE_COMPLETED', 'TASK_STATE_FAILED', 'TASK_STATE_CANCELED', 'TASK_STATE_REJECTED')
		 LIMIT 100`,
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
	for _, st := range staleTasks {
		// Unmarshal, transition to failed, re-marshal.
		var task a2a.Task
		if err := json.Unmarshal(st.payload, &task); err != nil {
			continue
		}
		task.Status.State = a2a.TaskStateFailed
		newPayload, err := json.Marshal(&task)
		if err != nil {
			continue
		}

		// Atomic conditional transition: CAS on version AND exec_owner.
		// Only succeed if the same stale exec_owner still holds the lease
		// and the version hasn't changed (no concurrent recovery).
		result, err := s.db.ExecContext(ctx,
			`UPDATE a2a_sdk_tasks
			 SET payload = $1, version = version + 1, updated_at = NOW(),
			     exec_owner = NULL, exec_heartbeat = NULL
			 WHERE id = $2 AND version = $3 AND exec_owner = $4
			   AND payload->'status'->>'state' NOT IN ('TASK_STATE_COMPLETED', 'TASK_STATE_FAILED', 'TASK_STATE_CANCELED', 'TASK_STATE_REJECTED')`,
			newPayload, st.id, st.version, st.execOwner,
		)
		if err != nil {
			continue
		}
		if n, _ := result.RowsAffected(); n > 0 {
			reapedIDs = append(reapedIDs, st.id)
		}
	}
	return reapedIDs, nil
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
	evResult, err := tx.ExecContext(ctx,
		`DELETE FROM a2a_task_events
		 WHERE task_id IN (
		     SELECT id FROM a2a_sdk_tasks
		     WHERE updated_at < $1
		       AND payload->'status'->>'state' IN ('TASK_STATE_COMPLETED', 'TASK_STATE_FAILED', 'TASK_STATE_CANCELED', 'TASK_STATE_REJECTED')
		 )`,
		olderThan,
	)
	if err != nil {
		return 0, 0, fmt.Errorf("purge correlated events: %w", err)
	}
	eventsPurged, _ = evResult.RowsAffected()

	// Delete terminal SDK tasks.
	taskResult, err := tx.ExecContext(ctx,
		`DELETE FROM a2a_sdk_tasks
		 WHERE updated_at < $1
		   AND payload->'status'->>'state' IN ('TASK_STATE_COMPLETED', 'TASK_STATE_FAILED', 'TASK_STATE_CANCELED', 'TASK_STATE_REJECTED')`,
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
