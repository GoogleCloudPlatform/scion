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
	"sort"
	"strconv"
	"strings"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/decision"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/decisionanswer"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/decisionfacetentry"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/decisionretentionrun"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// DecisionRetentionStore implements store.DecisionRetentionStore.
type DecisionRetentionStore struct {
	cs     *CompositeStore
	client *ent.Client
	db     *sql.DB
	pg     bool
}

// NewDecisionRetentionStore creates a DecisionRetentionStore over cs.
func NewDecisionRetentionStore(cs *CompositeStore) *DecisionRetentionStore {
	return &DecisionRetentionStore{
		cs:     cs,
		client: cs.client,
		db:     clientDB(cs.client),
		pg:     cs.isPostgres(),
	}
}

var _ store.DecisionRetentionStore = (*DecisionRetentionStore)(nil)

func (s *DecisionRetentionStore) requireDB() error {
	if s.db == nil {
		return errors.New("decision retention store: no SQL database")
	}
	return nil
}

func (s *DecisionRetentionStore) rebind(query string) string {
	if !s.pg {
		return query
	}
	var b strings.Builder
	n := 0
	for _, r := range query {
		if r == '?' {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// candidateScanQuery builds the candidate scan SQL. The settled = true filter
// is a literal so both SQLite and Postgres match the partial index
// decision_settled_created_at_id (WHERE settled = true). The explicit
// created_at >= ? lower bound gives SQLite a two-sided range on (created_at, id).
func (s *DecisionRetentionStore) candidateScanQuery(globalCutoff time.Time, after store.DecisionRetentionPosition, limit int) (string, []any) {
	query := `SELECT id, message_id, project_id, created_at FROM decisions WHERE settled = true AND created_at < ?`
	args := []any{globalCutoff.UTC()}
	if !after.IsStart() {
		c := after.CreatedAt.UTC()
		query += ` AND created_at >= ? AND (created_at > ? OR id > ?)`
		args = append(args, c, c, after.ID)
	}
	query += ` ORDER BY created_at, id LIMIT ?`
	args = append(args, limit)
	return s.rebind(query), args
}

// SelectDecisionRetentionCandidates implements store.DecisionRetentionStore.
func (s *DecisionRetentionStore) SelectDecisionRetentionCandidates(ctx context.Context, globalCutoff time.Time, after store.DecisionRetentionPosition, limit int) ([]store.DecisionRetentionCandidate, error) {
	if err := s.requireDB(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, nil
	}
	query, args := s.candidateScanQuery(globalCutoff, after, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("decision retention: select candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]store.DecisionRetentionCandidate, 0, limit)
	for rows.Next() {
		var c store.DecisionRetentionCandidate
		if err := rows.Scan(&c.ID, &c.MessageID, &c.ProjectID, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("decision retention: scan candidate: %w", err)
		}
		c.CreatedAt = c.CreatedAt.UTC()
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("decision retention: select candidates: %w", err)
	}
	return out, nil
}

// decisionRowSettled reports whether d is settled by the exact predicate
// (design-8984 §3.2): terminal status (replaced, withdrawn, dismissed) or
// decided with at least one direct answer.
func decisionRowSettled(d *ent.Decision, answers []*ent.DecisionAnswer) bool {
	switch d.Status {
	case decision.StatusReplaced, decision.StatusWithdrawn, decision.StatusDismissed:
		return true
	case decision.StatusDecided:
		for _, a := range answers {
			if a.Provenance == decisionanswer.ProvenanceDirect {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// decisionRowLastActivity computes the latest timestamp of d and its answers
// (design-8984 §3.2): max(created_at, decided_at, replaced_at, withdrawn_at,
// dismissed_at, max(answer.created_at)).
func decisionRowLastActivity(d *ent.Decision, answers []*ent.DecisionAnswer) time.Time {
	latest := d.CreatedAt.UTC()
	for _, ts := range []*time.Time{d.DecidedAt, d.ReplacedAt, d.WithdrawnAt, d.DismissedAt} {
		if ts != nil && ts.UTC().After(latest) {
			latest = ts.UTC()
		}
	}
	for _, a := range answers {
		if a.CreatedAt.UTC().After(latest) {
			latest = a.CreatedAt.UTC()
		}
	}
	return latest
}

// PurgeDecisionMessages implements store.DecisionRetentionStore.
func (s *DecisionRetentionStore) PurgeDecisionMessages(ctx context.Context, messageIDs []string, cutoffByProject map[string]time.Time) (store.DecisionRetentionPurgeResult, error) {
	res := store.DecisionRetentionPurgeResult{ByProject: map[string]store.DecisionRetentionCounts{}}
	if len(messageIDs) == 0 || len(cutoffByProject) == 0 {
		return res, nil
	}
	seenMsg := make(map[uuid.UUID]bool, len(messageIDs))
	msgUUIDs := make([]uuid.UUID, 0, len(messageIDs))
	for _, raw := range messageIDs {
		u, err := uuid.Parse(raw)
		if err != nil {
			continue
		}
		if !seenMsg[u] {
			seenMsg[u] = true
			msgUUIDs = append(msgUUIDs, u)
		}
	}
	if len(msgUUIDs) == 0 {
		return res, nil
	}

	err := s.cs.WithTx(ctx, func(tx store.Store) error {
		txCS := tx.(*CompositeStore)
		var txErr error
		res, txErr = s.purgeDecisionMessagesTx(ctx, txCS, msgUUIDs, cutoffByProject)
		return txErr
	})
	if err != nil {
		return store.DecisionRetentionPurgeResult{ByProject: map[string]store.DecisionRetentionCounts{}}, err
	}
	return res, nil
}

func (s *DecisionRetentionStore) purgeDecisionMessagesTx(
	ctx context.Context,
	txCS *CompositeStore,
	msgUUIDs []uuid.UUID,
	cutoffByProject map[string]time.Time,
) (store.DecisionRetentionPurgeResult, error) {
	res := store.DecisionRetentionPurgeResult{ByProject: map[string]store.DecisionRetentionCounts{}}
	client := txCS.client

	// 1. Lock all decision rows for the candidate messages in deterministic id
	// order (FOR UPDATE on Postgres) so we serialize cleanly with all live
	// writers and the facet indexer (design-8984 §3.4).
	decQ := client.Decision.Query().
		Where(decision.MessageIDIn(msgUUIDs...)).
		Order(ent.Asc(decision.FieldID))
	if txCS.isPostgres() {
		decQ = decQ.ForUpdate()
	}
	lockedDecs, err := decQ.All(ctx)
	if err != nil {
		return res, mapError(err)
	}
	if len(lockedDecs) == 0 {
		return res, nil
	}

	lockedDecIDs := make([]uuid.UUID, len(lockedDecs))
	byMsg := make(map[uuid.UUID][]*ent.Decision)
	for i, d := range lockedDecs {
		lockedDecIDs[i] = d.ID
		byMsg[d.MessageID] = append(byMsg[d.MessageID], d)
	}

	// 2. Load all answers for the locked decision rows.
	lockedAns, err := client.DecisionAnswer.Query().
		Where(decisionanswer.DecisionRowIDIn(lockedDecIDs...)).
		All(ctx)
	if err != nil {
		return res, mapError(err)
	}
	ansByDec := make(map[uuid.UUID][]*ent.DecisionAnswer, len(lockedDecs))
	for _, a := range lockedAns {
		ansByDec[a.DecisionRowID] = append(ansByDec[a.DecisionRowID], a)
	}

	// 3. Re-check whole-message eligibility on the locked rows.
	var eligibleDecs []*ent.Decision
	var eligibleDecIDs []uuid.UUID
	var eligibleAnsIDs []uuid.UUID

	for _, mid := range msgUUIDs {
		decs := byMsg[mid]
		if len(decs) == 0 {
			continue
		}
		eligible := true
		for _, d := range decs {
			cutoff, ok := cutoffByProject[d.ProjectID.String()]
			if !ok || cutoff.IsZero() {
				eligible = false
				break
			}
			ans := ansByDec[d.ID]
			if !decisionRowSettled(d, ans) {
				eligible = false
				break
			}
			if !decisionRowLastActivity(d, ans).Before(cutoff.UTC()) {
				eligible = false
				break
			}
		}
		if !eligible {
			res.SkippedMessages++
			continue
		}
		res.PurgedMessages++
		for _, d := range decs {
			eligibleDecs = append(eligibleDecs, d)
			eligibleDecIDs = append(eligibleDecIDs, d.ID)
			pid := d.ProjectID.String()
			c := res.ByProject[pid]
			c.Decisions++
			for _, a := range ansByDec[d.ID] {
				eligibleAnsIDs = append(eligibleAnsIDs, a.ID)
				c.Answers++
			}
			res.ByProject[pid] = c
		}
	}

	if len(eligibleDecIDs) == 0 {
		return res, nil
	}

	// 4. Compute facet decrements from locked eligible rows/answers with
	// facet_indexed = true (design-8984 §3.4).
	var deltas []facetEntryDelta
	maxPurgedAt := make(map[facetEntryKey]time.Time)
	recordPurgedAt := func(k facetEntryKey, ts time.Time) {
		t := ts.UTC().Truncate(time.Microsecond)
		if cur, ok := maxPurgedAt[k]; !ok || t.After(cur) {
			maxPurgedAt[k] = t
		}
	}

	for _, d := range eligibleDecs {
		hasChatKey := d.ChatKey != nil && *d.ChatKey != ""
		if !hasChatKey {
			continue
		}
		if d.FacetIndexed {
			k := facetEntryKey{
				ProjectID: d.ProjectID,
				ChatKey:   *d.ChatKey,
				Kind:      decisionfacetentry.KindAsker,
				ActorID:   d.AskerAgentID.String(),
			}
			deltas = append(deltas, facetEntryDelta{
				ProjectID: k.ProjectID,
				ChatKey:   k.ChatKey,
				Kind:      k.Kind,
				ActorID:   k.ActorID,
				Delta:     -1,
			})
			recordPurgedAt(k, d.CreatedAt)
		}

		ans := ansByDec[d.ID]
		hasDirect := false
		for _, a := range ans {
			if a.Provenance == decisionanswer.ProvenanceDirect {
				hasDirect = true
				break
			}
		}
		for _, a := range ans {
			if !a.FacetIndexed || a.AnsweredByUserID == nil || *a.AnsweredByUserID == "" {
				continue
			}
			// Superseded interpreted answers contributed 0 when direct exists.
			if a.Provenance == decisionanswer.ProvenanceAgentInterpreted && hasDirect {
				continue
			}
			k := facetEntryKey{
				ProjectID: d.ProjectID,
				ChatKey:   *d.ChatKey,
				Kind:      decisionfacetentry.KindAnswerer,
				ActorID:   *a.AnsweredByUserID,
			}
			deltas = append(deltas, facetEntryDelta{
				ProjectID: k.ProjectID,
				ChatKey:   k.ChatKey,
				Kind:      k.Kind,
				ActorID:   k.ActorID,
				Delta:     -1,
			})
			recordPurgedAt(k, a.CreatedAt)
		}
	}

	// 5. Delete answers first, then decisions.
	if len(eligibleAnsIDs) > 0 {
		if _, err := client.DecisionAnswer.Delete().
			Where(decisionanswer.IDIn(eligibleAnsIDs...)).
			Exec(ctx); err != nil {
			return res, mapError(err)
		}
	}
	if _, err := client.Decision.Delete().
		Where(decision.IDIn(eligibleDecIDs...)).
		Exec(ctx); err != nil {
		return res, mapError(err)
	}

	// 6. Apply negative facet deltas in deterministic sorted key order.
	if len(deltas) == 0 {
		return res, nil
	}
	if err := applyDecisionFacetDeltas(ctx, client, deltas); err != nil {
		return res, err
	}

	// 7. Delete touched summary rows at ref_count <= 0 (both kinds) and
	// recompute name/email/latest_at when the purged row was the newest
	// contributor (design-8984 §3.4).
	touchedKeys := make([]facetEntryKey, 0, len(maxPurgedAt))
	for k := range maxPurgedAt {
		touchedKeys = append(touchedKeys, k)
	}
	sort.Slice(touchedKeys, func(i, j int) bool {
		ki, kj := touchedKeys[i], touchedKeys[j]
		if ki.ProjectID != kj.ProjectID {
			return ki.ProjectID.String() < kj.ProjectID.String()
		}
		if ki.ChatKey != kj.ChatKey {
			return ki.ChatKey < kj.ChatKey
		}
		if ki.Kind != kj.Kind {
			return ki.Kind < kj.Kind
		}
		return ki.ActorID < kj.ActorID
	})

	for _, k := range touchedKeys {
		entry, err := client.DecisionFacetEntry.Query().
			Where(
				decisionfacetentry.ProjectIDEQ(k.ProjectID),
				decisionfacetentry.ChatKeyEQ(k.ChatKey),
				decisionfacetentry.KindEQ(k.Kind),
				decisionfacetentry.ActorIDEQ(k.ActorID),
			).
			Only(ctx)
		if ent.IsNotFound(err) {
			// Concurrent clearDecisionAnswerUser or project delete already removed it.
			continue
		}
		if err != nil {
			return res, mapError(err)
		}
		if entry.RefCount <= 0 {
			if err := client.DecisionFacetEntry.DeleteOneID(entry.ID).Exec(ctx); err != nil {
				return res, mapError(err)
			}
			continue
		}
		// Recompute only when the purged contributor could have been the newest
		// (entry.LatestAt <= maxPurgedAt[k], including ties).
		if !entry.LatestAt.UTC().Truncate(time.Microsecond).After(maxPurgedAt[k]) {
			if err := recomputeFacetEntryMetadataTx(ctx, client, entry, k); err != nil {
				return res, err
			}
		}
	}

	return res, nil
}

// recomputeFacetEntryMetadataTx recomputes (name, email, latest_at) for a
// surviving decision_facet_entries row from the newest remaining indexed
// contributor, or deletes the row if none remains.
func recomputeFacetEntryMetadataTx(ctx context.Context, client *ent.Client, entry *ent.DecisionFacetEntry, k facetEntryKey) error {
	drv := client.Driver()
	isPG := drv.Dialect() == dialect.Postgres
	rebind := func(q string) string {
		if !isPG {
			return q
		}
		var b strings.Builder
		n := 0
		for _, r := range q {
			if r == '?' {
				n++
				b.WriteByte('$')
				b.WriteString(strconv.Itoa(n))
				continue
			}
			b.WriteRune(r)
		}
		return b.String()
	}

	switch k.Kind {
	case decisionfacetentry.KindAsker:
		actorUUID, err := uuid.Parse(k.ActorID)
		if err != nil {
			return nil
		}
		q := rebind(`SELECT asker_agent_name, created_at
FROM decisions
WHERE project_id = ? AND chat_key = ? AND asker_agent_id = ? AND facet_indexed = true
ORDER BY created_at DESC, asker_agent_name DESC
LIMIT 1`)
		rows := &entsql.Rows{}
		if err := drv.Query(ctx, q, []any{k.ProjectID, k.ChatKey, actorUUID}, rows); err != nil {
			return mapError(err)
		}
		found := rows.Next()
		var name string
		var latest time.Time
		var scanErr error
		if found {
			scanErr = rows.Scan(&name, &latest)
		}
		rowErr := rows.Err()
		closeErr := rows.Close()
		if scanErr != nil {
			return mapError(scanErr)
		}
		if rowErr != nil {
			return mapError(rowErr)
		}
		if closeErr != nil {
			return mapError(closeErr)
		}
		if !found {
			return mapError(client.DecisionFacetEntry.DeleteOneID(entry.ID).Exec(ctx))
		}
		return mapError(client.DecisionFacetEntry.UpdateOneID(entry.ID).
			SetName(name).
			SetEmail("").
			SetLatestAt(latest.UTC().Truncate(time.Microsecond)).
			Exec(ctx))

	case decisionfacetentry.KindAnswerer:
		q := rebind(`SELECT a.answered_by_name, a.answered_by_email, a.created_at
FROM decision_answers a
INNER JOIN decisions d ON d.id = a.decision_row_id
WHERE d.project_id = ? AND d.chat_key = ? AND a.answered_by_user_id = ? AND a.facet_indexed = true
  AND (a.provenance = 'direct' OR NOT EXISTS (
    SELECT 1 FROM decision_answers s
    WHERE s.decision_row_id = a.decision_row_id AND s.provenance = 'direct'
  ))
ORDER BY a.created_at DESC, a.answered_by_name DESC, a.answered_by_email DESC
LIMIT 1`)
		rows := &entsql.Rows{}
		if err := drv.Query(ctx, q, []any{k.ProjectID, k.ChatKey, k.ActorID}, rows); err != nil {
			return mapError(err)
		}
		found := rows.Next()
		var name, email string
		var latest time.Time
		var scanErr error
		if found {
			scanErr = rows.Scan(&name, &email, &latest)
		}
		rowErr := rows.Err()
		closeErr := rows.Close()
		if scanErr != nil {
			return mapError(scanErr)
		}
		if rowErr != nil {
			return mapError(rowErr)
		}
		if closeErr != nil {
			return mapError(closeErr)
		}
		if !found {
			return mapError(client.DecisionFacetEntry.DeleteOneID(entry.ID).Exec(ctx))
		}
		return mapError(client.DecisionFacetEntry.UpdateOneID(entry.ID).
			SetName(name).
			SetEmail(email).
			SetLatestAt(latest.UTC().Truncate(time.Microsecond)).
			Exec(ctx))
	}
	return nil
}

// GetDecisionRetentionCursor implements store.DecisionRetentionStore.
func (s *DecisionRetentionStore) GetDecisionRetentionCursor(ctx context.Context) (store.DecisionRetentionCursor, error) {
	row, err := s.cs.GetHubSetting(ctx, store.HubSettingKeyDecisionRetentionCursor)
	if errors.Is(err, store.ErrNotFound) {
		return store.DecisionRetentionCursor{}, nil
	}
	if err != nil {
		return store.DecisionRetentionCursor{}, fmt.Errorf("decision retention: get cursor: %w", err)
	}
	var c store.DecisionRetentionCursor
	if len(row.Value) > 0 {
		if err := json.Unmarshal(row.Value, &c); err != nil {
			return store.DecisionRetentionCursor{}, fmt.Errorf("decision retention: decode cursor: %w", err)
		}
	}
	if !c.Position.CreatedAt.IsZero() {
		c.Position.CreatedAt = c.Position.CreatedAt.UTC()
	}
	if c.ResetAt != nil {
		t := c.ResetAt.UTC()
		c.ResetAt = &t
	}
	if !c.UpdatedAt.IsZero() {
		c.UpdatedAt = c.UpdatedAt.UTC()
	}
	return c, nil
}

// SaveDecisionRetentionCursor implements store.DecisionRetentionStore.
func (s *DecisionRetentionStore) SaveDecisionRetentionCursor(ctx context.Context, cursor store.DecisionRetentionCursor) error {
	if cursor.UpdatedAt.IsZero() {
		cursor.UpdatedAt = time.Now().UTC()
	} else {
		cursor.UpdatedAt = cursor.UpdatedAt.UTC()
	}
	if !cursor.Position.CreatedAt.IsZero() {
		cursor.Position.CreatedAt = cursor.Position.CreatedAt.UTC()
	}
	if cursor.ResetAt != nil {
		t := cursor.ResetAt.UTC()
		cursor.ResetAt = &t
	}
	raw, err := json.Marshal(cursor)
	if err != nil {
		return fmt.Errorf("decision retention: encode cursor: %w", err)
	}
	if _, err := s.cs.UpsertHubSetting(ctx, store.HubSettingKeyDecisionRetentionCursor, raw, "decision-retention", -1, "managed"); err != nil {
		return fmt.Errorf("decision retention: save cursor: %w", err)
	}
	return nil
}

// LatestDecisionRetentionRun implements store.DecisionRetentionStore.
func (s *DecisionRetentionStore) LatestDecisionRetentionRun(ctx context.Context, trigger string, since time.Time) (*store.DecisionRetentionRun, error) {
	row, err := s.client.DecisionRetentionRun.Query().
		Where(
			decisionretentionrun.TriggerEQ(decisionretentionrun.Trigger(trigger)),
			decisionretentionrun.DryRun(false),
			decisionretentionrun.StartedAtGTE(since.UTC()),
		).
		Order(ent.Desc(decisionretentionrun.FieldStartedAt)).
		First(ctx)
	if ent.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("decision retention: latest run: %w", err)
	}
	return decisionRetentionRunFromEnt(row)
}

const decisionRetentionRunListMax = 500

// ListDecisionRetentionRuns implements store.DecisionRetentionStore.
func (s *DecisionRetentionStore) ListDecisionRetentionRuns(ctx context.Context, filter store.DecisionRetentionRunFilter) ([]store.DecisionRetentionRun, error) {
	q := s.client.DecisionRetentionRun.Query()
	if filter.Trigger != "" {
		q = q.Where(decisionretentionrun.TriggerEQ(decisionretentionrun.Trigger(filter.Trigger)))
	}
	if !filter.FinishedAfter.IsZero() {
		q = q.Where(decisionretentionrun.FinishedAtGT(filter.FinishedAfter.UTC()))
	}
	limit := filter.Limit
	if limit <= 0 || limit > decisionRetentionRunListMax {
		limit = decisionRetentionRunListMax
	}
	rows, err := q.Order(ent.Desc(decisionretentionrun.FieldStartedAt)).Limit(limit).All(ctx)
	if err != nil {
		return nil, fmt.Errorf("decision retention: list runs: %w", err)
	}
	out := make([]store.DecisionRetentionRun, 0, len(rows))
	for _, row := range rows {
		r, err := decisionRetentionRunFromEnt(row)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, nil
}

func decisionRetentionRunFromEnt(row *ent.DecisionRetentionRun) (*store.DecisionRetentionRun, error) {
	r := &store.DecisionRetentionRun{
		ID:        row.ID.String(),
		Trigger:   string(row.Trigger),
		DryRun:    row.DryRun,
		StartedAt: row.StartedAt.UTC(),
		Status:    string(row.Status),
	}
	if row.FinishedAt != nil {
		t := row.FinishedAt.UTC()
		r.FinishedAt = &t
	}
	if row.Actor != nil {
		r.Actor = *row.Actor
	}
	if row.Error != nil {
		r.Error = *row.Error
	}
	if len(row.Policy) > 0 {
		if err := json.Unmarshal(row.Policy, &r.Policy); err != nil {
			return nil, fmt.Errorf("decision retention: run %s policy: %w", r.ID, err)
		}
	}
	if len(row.Counts) > 0 {
		if err := json.Unmarshal(row.Counts, &r.Counts); err != nil {
			return nil, fmt.Errorf("decision retention: run %s counts: %w", r.ID, err)
		}
	}
	if r.Counts == nil {
		r.Counts = map[string]store.DecisionRetentionCounts{}
	}
	return r, nil
}

// SaveDecisionRetentionRun implements store.DecisionRetentionStore.
func (s *DecisionRetentionStore) SaveDecisionRetentionRun(ctx context.Context, run *store.DecisionRetentionRun) error {
	if run == nil {
		return errors.New("decision retention: nil run")
	}
	policyJSON, err := json.Marshal(run.Policy)
	if err != nil {
		return fmt.Errorf("decision retention: marshal policy: %w", err)
	}
	counts := run.Counts
	if counts == nil {
		counts = map[string]store.DecisionRetentionCounts{}
	}
	countsJSON, err := json.Marshal(counts)
	if err != nil {
		return fmt.Errorf("decision retention: marshal counts: %w", err)
	}
	if run.ID == "" {
		id := uuid.New()
		create := s.client.DecisionRetentionRun.Create().
			SetID(id).
			SetTrigger(decisionretentionrun.Trigger(run.Trigger)).
			SetDryRun(run.DryRun).
			SetStartedAt(run.StartedAt.UTC()).
			SetStatus(decisionretentionrun.Status(run.Status)).
			SetPolicy(policyJSON).
			SetCounts(countsJSON)
		if run.FinishedAt != nil {
			create.SetFinishedAt(run.FinishedAt.UTC())
		}
		if run.Actor != "" {
			create.SetActor(run.Actor)
		}
		if run.Error != "" {
			create.SetError(run.Error)
		}
		if _, err := create.Save(ctx); err != nil {
			return fmt.Errorf("decision retention: insert run: %w", err)
		}
		run.ID = id.String()
		return nil
	}
	id, err := uuid.Parse(run.ID)
	if err != nil {
		return fmt.Errorf("decision retention: run id %q: %w", run.ID, err)
	}
	upd := s.client.DecisionRetentionRun.UpdateOneID(id).
		SetStatus(decisionretentionrun.Status(run.Status)).
		SetPolicy(policyJSON).
		SetCounts(countsJSON)
	if run.FinishedAt != nil {
		upd.SetFinishedAt(run.FinishedAt.UTC())
	} else {
		upd.ClearFinishedAt()
	}
	if run.Error != "" {
		upd.SetError(run.Error)
	} else {
		upd.ClearError()
	}
	if _, err := upd.Save(ctx); err != nil {
		return fmt.Errorf("decision retention: update run %s: %w", run.ID, err)
	}
	return nil
}

// PruneDecisionRetentionRuns implements store.DecisionRetentionStore.
func (s *DecisionRetentionStore) PruneDecisionRetentionRuns(ctx context.Context, before time.Time) (int, error) {
	n, err := s.client.DecisionRetentionRun.Delete().
		Where(decisionretentionrun.StartedAtLT(before.UTC())).
		Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("decision retention: prune runs: %w", err)
	}
	return n, nil
}
