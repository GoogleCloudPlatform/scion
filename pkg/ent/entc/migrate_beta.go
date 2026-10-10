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

package entc

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
)

// migrationEntities lists every Ent node by its field name on the generated
// *ent.Client, ordered so that an entity always appears after the entities its
// foreign keys reference. Ent models its M2O/O2M relationships as plain FK
// columns on the child node, so copying nodes in this order satisfies every
// constraint at insert time.
//
// The first seven entities have FK edges:
//   - Group        -> User (owner)
//   - Agent        -> Project (required), User (creator, owner)
//   - GroupMembership -> Group (required), User, Agent
//   - PolicyBinding   -> AccessPolicy, User, Group, Agent
//
// The remainder declare no Ent edges (no DB-level FK constraints), so their
// relative order is irrelevant; they are listed alphabetically for readability.
//
// An entity whose source rows can collide on a destination unique index has an
// entry in migrationRowFilters, which drops the colliding rows before insert.
var migrationEntities = []string{
	// FK-ordered core.
	"User",
	"Project",
	"AccessPolicy",
	"Group",
	"Agent",
	"GroupMembership",
	"PolicyBinding",
	// Independent entities (no Ent edges).
	"AgentSessionMetrics",
	"AllowListEntry",
	"ApiKey",
	"BrokerJoinToken",
	"BrokerSecret",
	"EnvVar",
	"GCPServiceAccount",
	"GithubInstallation",
	"HarnessConfig",
	"InviteCode",
	"MaintenanceOperation",
	"MaintenanceOperationRun",
	"Message",
	"Notification",
	"NotificationSubscription",
	"ProjectContributor",
	"ProjectPreStartHook",
	"ProjectSyncState",
	"RuntimeBroker",
	"Schedule",
	"ScheduledEvent",
	"Secret",
	"SubscriptionTemplate",
	"Template",
	"UserAccessToken",
}

// migrationRowFilters maps an entity name to a function that removes source
// rows which would collide on a unique index the destination already has
// (AutoMigrate creates every index before MigrateData runs). It returns the
// rows to copy and the number of rows dropped. Entities without an entry are
// copied unfiltered.
var migrationRowFilters = map[string]func(rows []reflect.Value) ([]reflect.Value, int){
	"AgentSessionMetrics": dedupAgentSessionMetricsRows,
}

// sessionMetricsKey is the unique (agent_id, session_id, started_at) key of
// agent_session_metrics. started_at is held as UnixNano so that equal instants
// read back in different locations compare equal.
type sessionMetricsKey struct {
	agentID   string
	sessionID string
	startedAt int64
}

// dedupAgentSessionMetricsRows keeps one row per (agent_id, session_id,
// started_at). A source written by a Hub from before the unique index existed
// can hold several rows for one key: a retried or resent report of the same
// session segment was stored again. For each key the earliest stored row is
// kept, ordered by created_at and then id, the same rule
// CompositeStore.deduplicateAgentSessionMetrics applies before it adds the
// index, and the same outcome the store gives a repeated report from then on:
// the first one stored wins. Kept rows stay in source order.
func dedupAgentSessionMetricsRows(rows []reflect.Value) ([]reflect.Value, int) {
	keep := make(map[sessionMetricsKey]*ent.AgentSessionMetrics, len(rows))
	for _, rv := range rows {
		m := rv.Interface().(*ent.AgentSessionMetrics)
		k := sessionMetricsKey{agentID: m.AgentID, sessionID: m.SessionID, startedAt: m.StartedAt.UnixNano()}
		if cur, ok := keep[k]; !ok || storedBefore(m.CreatedAt, m.ID, cur.CreatedAt, cur.ID) {
			keep[k] = m
		}
	}
	out := make([]reflect.Value, 0, len(keep))
	for _, rv := range rows {
		m := rv.Interface().(*ent.AgentSessionMetrics)
		k := sessionMetricsKey{agentID: m.AgentID, sessionID: m.SessionID, startedAt: m.StartedAt.UnixNano()}
		if keep[k] == m {
			out = append(out, rv)
		}
	}
	return out, len(rows) - len(out)
}

// storedBefore orders rows by created_at, then id, like ORDER BY created_at
// ASC, id ASC. UUIDs compare by their bytes, which matches both the Postgres
// uuid ordering and the SQLite ordering of their canonical text form.
func storedBefore(aCreated time.Time, aID [16]byte, bCreated time.Time, bID [16]byte) bool {
	if !aCreated.Equal(bCreated) {
		return aCreated.Before(bCreated)
	}
	return bytes.Compare(aID[:], bID[:]) < 0
}

// defaultBatchSize bounds how many rows a single CreateBulk statement inserts.
// Postgres caps a statement at 65535 bind parameters; the widest entity (Agent)
// has ~36 columns, so 500 rows stays comfortably under the limit while keeping
// the number of round-trips low.
const defaultBatchSize = 500

// MigrateOptions tunes a MigrateData run.
type MigrateOptions struct {
	// BatchSize is the maximum number of rows per CreateBulk statement.
	// Defaults to defaultBatchSize when <= 0.
	BatchSize int
	// Logf, if non-nil, receives one human-readable progress line per entity.
	Logf func(format string, args ...any)
}

// EntityResult records the outcome of migrating a single entity.
type EntityResult struct {
	Entity   string
	Source   int // rows present in the source
	Inserted int // rows newly written to the destination this run
	Skipped  int // rows already present in the destination (idempotent skips)
	// Duplicates counts source rows not copied because they collide with an
	// earlier source row on a destination unique index (see
	// migrationRowFilters).
	Duplicates int
	Dest       int // rows in the destination after migration
}

// MigrateReport is the aggregate outcome of a migration run.
type MigrateReport struct {
	Entities       []EntityResult
	ChildGroupEdgs int // group->child_group M2M edges copied
}

// MigrateData copies all data from src into dst, entity by entity, in foreign
// key dependency order. The destination schema must already exist (call
// AutoMigrate first).
//
// Properties:
//   - Idempotent: rows whose primary key already exists in dst are skipped, so
//     a partially completed run can be safely restarted.
//   - Atomic per entity: each entity's inserts run inside a single transaction.
//   - Duplicate-tolerant: for entities in migrationRowFilters, source rows that
//     collide on a destination unique index are skipped, keeping the earliest.
//   - Verified: after each entity the destination row count is compared with
//     the source count less skipped duplicates, and a mismatch aborts the
//     migration.
//
// MigrateData never writes to src.
func MigrateData(ctx context.Context, src, dst *ent.Client, opts MigrateOptions) (*MigrateReport, error) {
	batchSize := opts.BatchSize
	if batchSize <= 0 {
		batchSize = defaultBatchSize
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	report := &MigrateReport{}
	srcStruct := reflect.ValueOf(src).Elem()
	dstStruct := reflect.ValueOf(dst).Elem()

	for _, name := range migrationEntities {
		srcClient := srcStruct.FieldByName(name)
		dstClient := dstStruct.FieldByName(name)
		if !srcClient.IsValid() || !dstClient.IsValid() {
			return report, fmt.Errorf("entity %q not found on ent.Client (generated code drift?)", name)
		}

		res, err := migrateEntity(ctx, name, srcClient, dst, dstClient, batchSize)
		if err != nil {
			return report, fmt.Errorf("migrating %s: %w", name, err)
		}
		report.Entities = append(report.Entities, res)
		if res.Duplicates > 0 {
			logf("migrated %-26s source=%d inserted=%d skipped=%d duplicates=%d dest=%d",
				res.Entity, res.Source, res.Inserted, res.Skipped, res.Duplicates, res.Dest)
		} else {
			logf("migrated %-26s source=%d inserted=%d skipped=%d dest=%d",
				res.Entity, res.Source, res.Inserted, res.Skipped, res.Dest)
		}
	}

	// Copy the one many-to-many edge (Group.child_groups) that lives in a join
	// table rather than as an FK column on a node.
	edges, err := copyGroupChildEdges(ctx, src, dst)
	if err != nil {
		return report, fmt.Errorf("copying group child edges: %w", err)
	}
	report.ChildGroupEdgs = edges
	logf("migrated %-26s edges=%d", "group_child_groups", edges)

	return report, nil
}

// migrateEntity copies a single entity. Reads happen against srcClient (a
// *XClient on the source); writes happen inside a transaction on dst so the
// entity is migrated atomically even when split across multiple CreateBulk
// batches.
func migrateEntity(ctx context.Context, name string, srcClient reflect.Value, dst *ent.Client, dstClient reflect.Value, batchSize int) (EntityResult, error) {
	res := EntityResult{Entity: name}

	rows, err := queryAll(ctx, srcClient)
	if err != nil {
		return res, fmt.Errorf("querying source: %w", err)
	}
	res.Source = len(rows)
	if filter, ok := migrationRowFilters[name]; ok {
		rows, res.Duplicates = filter(rows)
	}

	existing, err := queryIDSet(ctx, dstClient)
	if err != nil {
		return res, fmt.Errorf("reading destination ids: %w", err)
	}

	tx, err := dst.Tx(ctx)
	if err != nil {
		return res, fmt.Errorf("starting transaction: %w", err)
	}
	txClient := reflect.ValueOf(tx).Elem().FieldByName(name)

	batch := make([]reflect.Value, 0, batchSize)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		n, err := createBulk(ctx, txClient, batch)
		if err != nil {
			return err
		}
		res.Inserted += n
		batch = batch[:0]
		return nil
	}

	for _, row := range rows {
		id := row.Elem().FieldByName("ID").Interface()
		if _, ok := existing[id]; ok {
			res.Skipped++
			continue
		}
		builder := txClient.MethodByName("Create").Call(nil)[0]
		if err := applyFields(builder, row.Elem()); err != nil {
			_ = tx.Rollback()
			return res, fmt.Errorf("mapping fields for id %v: %w", id, err)
		}
		batch = append(batch, builder)
		if len(batch) >= batchSize {
			if err := flush(); err != nil {
				_ = tx.Rollback()
				return res, fmt.Errorf("bulk insert: %w", err)
			}
		}
	}
	if err := flush(); err != nil {
		_ = tx.Rollback()
		return res, fmt.Errorf("bulk insert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("committing: %w", err)
	}

	// Verify: the destination must now hold exactly as many rows as the source,
	// less the duplicates the filter dropped.
	dstCount, err := queryCount(ctx, dstClient)
	if err != nil {
		return res, fmt.Errorf("counting destination: %w", err)
	}
	res.Dest = dstCount
	if want := res.Source - res.Duplicates; dstCount != want {
		return res, fmt.Errorf("row count mismatch: source=%d duplicates=%d dest=%d", res.Source, res.Duplicates, dstCount)
	}
	return res, nil
}

// applyFields copies every persisted field from a source entity struct (rowElem,
// the dereferenced *X) onto a create builder by calling the matching generated
// setter via reflection. Pointer fields use SetNillable<Field> when available
// (scalar optionals) and fall back to Set<Field> (JSON pointer fields whose
// setter already takes a pointer). The relationship FK columns (e.g. ProjectID,
// OwnerID) are ordinary fields and are copied the same way, preserving edges.
func applyFields(builder, rowElem reflect.Value) error {
	t := rowElem.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		switch f.Name {
		case "config", "Edges", "selectValues":
			continue
		}
		if f.PkgPath != "" { // unexported field
			continue
		}
		fv := rowElem.Field(i)

		var candidates []string
		if f.Type.Kind() == reflect.Pointer {
			candidates = []string{"SetNillable" + f.Name, "Set" + f.Name}
		} else {
			candidates = []string{"Set" + f.Name}
		}

		applied := false
		for _, mName := range candidates {
			m := builder.MethodByName(mName)
			if !m.IsValid() {
				continue
			}
			mt := m.Type()
			if mt.NumIn() != 1 || mt.IsVariadic() {
				continue
			}
			if !fv.Type().AssignableTo(mt.In(0)) {
				continue
			}
			m.Call([]reflect.Value{fv})
			applied = true
			break
		}
		if !applied && f.Name == "ID" {
			// Every Ent entity in this schema uses a settable UUID primary key.
			// A missing SetID would silently re-generate IDs and break FKs, so
			// fail loud instead.
			return fmt.Errorf("no SetID setter found for %s", t.Name())
		}
	}
	return nil
}

// queryAll returns every row of an entity as a slice of *X reflect.Values via
// client.Query().All(ctx).
func queryAll(ctx context.Context, client reflect.Value) ([]reflect.Value, error) {
	query := client.MethodByName("Query").Call(nil)[0]
	out := query.MethodByName("All").Call([]reflect.Value{reflect.ValueOf(ctx)})
	if err := asError(out[1]); err != nil {
		return nil, err
	}
	slice := out[0]
	rows := make([]reflect.Value, slice.Len())
	for i := 0; i < slice.Len(); i++ {
		rows[i] = slice.Index(i)
	}
	return rows, nil
}

// queryIDSet returns the set of primary keys present for an entity via
// client.Query().IDs(ctx). Ent ID types (uuid.UUID, string, int) are all
// comparable and usable as map keys.
func queryIDSet(ctx context.Context, client reflect.Value) (map[any]struct{}, error) {
	query := client.MethodByName("Query").Call(nil)[0]
	out := query.MethodByName("IDs").Call([]reflect.Value{reflect.ValueOf(ctx)})
	if err := asError(out[1]); err != nil {
		return nil, err
	}
	ids := out[0]
	set := make(map[any]struct{}, ids.Len())
	for i := 0; i < ids.Len(); i++ {
		set[ids.Index(i).Interface()] = struct{}{}
	}
	return set, nil
}

// queryCount returns the row count for an entity via client.Query().Count(ctx).
func queryCount(ctx context.Context, client reflect.Value) (int, error) {
	query := client.MethodByName("Query").Call(nil)[0]
	out := query.MethodByName("Count").Call([]reflect.Value{reflect.ValueOf(ctx)})
	if err := asError(out[1]); err != nil {
		return 0, err
	}
	return int(out[0].Int()), nil
}

// createBulk runs client.CreateBulk(builders...).Save(ctx) and returns the
// number of rows written.
func createBulk(ctx context.Context, client reflect.Value, builders []reflect.Value) (int, error) {
	if len(builders) == 0 {
		return 0, nil
	}
	builderType := builders[0].Type() // *XCreate
	slice := reflect.MakeSlice(reflect.SliceOf(builderType), len(builders), len(builders))
	for i, b := range builders {
		slice.Index(i).Set(b)
	}
	bulk := client.MethodByName("CreateBulk").CallSlice([]reflect.Value{slice})[0]
	out := bulk.MethodByName("Save").Call([]reflect.Value{reflect.ValueOf(ctx)})
	if err := asError(out[1]); err != nil {
		return 0, err
	}
	return out[0].Len(), nil
}

// copyGroupChildEdges copies the Group.child_groups many-to-many relationship,
// the only edge in the schema backed by a join table rather than an FK column.
// It is idempotent: only edges missing on the destination are added.
func copyGroupChildEdges(ctx context.Context, src, dst *ent.Client) (int, error) {
	groups, err := src.Group.Query().All(ctx)
	if err != nil {
		return 0, err
	}
	added := 0
	for _, g := range groups {
		srcChildIDs, err := g.QueryChildGroups().IDs(ctx)
		if err != nil {
			return added, err
		}
		if len(srcChildIDs) == 0 {
			continue
		}
		dstGroup, err := dst.Group.Get(ctx, g.ID)
		if err != nil {
			return added, err
		}
		dstChildIDs, err := dstGroup.QueryChildGroups().IDs(ctx)
		if err != nil {
			return added, err
		}
		have := make(map[any]struct{}, len(dstChildIDs))
		for _, id := range dstChildIDs {
			have[id] = struct{}{}
		}
		missing := srcChildIDs[:0:0]
		for _, id := range srcChildIDs {
			if _, ok := have[id]; !ok {
				missing = append(missing, id)
			}
		}
		if len(missing) == 0 {
			continue
		}
		if err := dst.Group.UpdateOneID(g.ID).AddChildGroupIDs(missing...).Exec(ctx); err != nil {
			return added, err
		}
		added += len(missing)
	}
	return added, nil
}

// asError converts a reflect.Value holding an error interface to a Go error.
func asError(v reflect.Value) error {
	if v.IsNil() {
		return nil
	}
	return v.Interface().(error)
}
