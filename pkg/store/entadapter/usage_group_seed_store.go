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
	"sort"
	"strings"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entitlementbinding"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/limitdefinition"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/usagegroupbudgetseed"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const usageGroupEpisodeTTLCoverDelay = 2 * time.Minute

type usageGroupSeedRow struct {
	dayUTC         time.Time
	projectScopeID string
	tokensSummable int64
	input          int64
	output         int64
	cacheRead      int64
	cacheWrite     int64
	calls          int64
	events         int64
}

func groupEpisodeSeedOrigin(episodeID string) string {
	return store.UsageGroupRollupOriginSeed + ":" + episodeID
}

func utcMonthStart(ts time.Time) time.Time {
	t := ts.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func sqlBigIntSumExpr(dialectName, innerExpr string) string {
	if dialectName == dialect.Postgres {
		return fmt.Sprintf("CAST(COALESCE(SUM(%s), 0) AS BIGINT)", innerExpr)
	}
	return fmt.Sprintf("COALESCE(SUM(%s), 0)", innerExpr)
}

// acquireGroupEpisodeAdvisoryLock takes a two-key per-group transaction-scoped
// PostgreSQL advisory lock (store.LockUsageGroupEpisode, StableProjectHash(groupID))
// so every episode lifecycle transition (open, close, seed pass) for groupID
// serializes cleanly across Hub replicas without sharing the single-key int8
// singleton lock space (D6a, O4, O5). On SQLite, transactions are already
// serialized by the single writer connection.
func acquireGroupEpisodeAdvisoryLock(ctx context.Context, drv dialect.Driver, groupID string) error {
	if drv.Dialect() != dialect.Postgres {
		return nil
	}
	return drv.Exec(
		ctx,
		"SELECT pg_advisory_xact_lock($1, $2)",
		[]any{int32(store.LockUsageGroupEpisode), store.StableProjectHash(groupID)},
		nil,
	)
}

func entUsageGroupSeedToStore(r *ent.UsageGroupBudgetSeed) *store.UsageGroupSeedEpisode {
	if r == nil {
		return nil
	}
	var members []string
	if len(r.FrozenMemberIds) > 0 {
		members = make([]string, len(r.FrozenMemberIds))
		copy(members, r.FrozenMemberIds)
	}
	var endedCopy *time.Time
	if r.EndedAt != nil {
		t := r.EndedAt.UTC()
		endedCopy = &t
	}
	return &store.UsageGroupSeedEpisode{
		ID:                r.ID.String(),
		GroupID:           r.GroupID,
		ActivatedAt:       r.ActivatedAt.UTC(),
		SeedFromUTC:       r.SeedFromUtc.UTC(),
		CutoffAt:          r.CutoffAt.UTC(),
		EndedAt:           endedCopy,
		FrozenMemberIDs:   members,
		MemberSnapshotSet: r.MemberSnapshotSet,
		Finalized:         r.Finalized,
		StateVersion:      r.StateVersion,
		CreatedAt:         r.CreatedAt.UTC(),
		UpdatedAt:         r.UpdatedAt.UTC(),
	}
}

// computeUnchargedGroupEpisodeRollupSeedWithDriver aggregates uncharged
// usage_events in [fromTS, toTS) for memberUserIDs using SQL GROUP BY
// (day_utc, project_id), returning both system (project_scope_id="") and
// project-scoped (project_scope_id=<event project>) rollup rows plus the sorted
// list of touched project scopes (D6a, R5).
func computeUnchargedGroupEpisodeRollupSeedWithDriver(
	ctx context.Context,
	drv dialect.Driver,
	groupID string,
	memberUserIDs []string,
	fromTS, toTS time.Time,
) ([]usageGroupSeedRow, []string, error) {
	if groupID == "" {
		return nil, nil, fmt.Errorf("%w: group_id is required", store.ErrInvalidInput)
	}
	if len(memberUserIDs) == 0 || !toTS.After(fromTS) {
		return nil, nil, nil
	}

	dialectName := drv.Dialect()
	fromUTC := fromTS.UTC().Truncate(time.Microsecond)
	toUTC := toTS.UTC().Truncate(time.Microsecond)

	dayExpr := "substr(event_ts, 1, 10)"
	var groupParam any = groupID
	notChargedClause := "(charged_group_ids IS NULL OR NOT EXISTS (SELECT 1 FROM json_each(usage_events.charged_group_ids) WHERE json_each.value = ?))"
	if dialectName == dialect.Postgres {
		dayExpr = "to_char(event_ts AT TIME ZONE 'UTC', 'YYYY-MM-DD')"
		b, err := json.Marshal([]string{groupID})
		if err != nil {
			return nil, nil, fmt.Errorf("marshal group id json: %w", err)
		}
		groupParam = string(b)
		notChargedClause = "(charged_group_ids IS NULL OR NOT (charged_group_ids @> CAST(? AS jsonb)))"
	}

	sumTok := sqlBigIntSumExpr(dialectName, "CASE WHEN anomalous THEN 0 ELSE COALESCE(input, 0) + COALESCE(output, 0) + COALESCE(cache_read, 0) + COALESCE(cache_write, 0) END")
	sumIn := sqlBigIntSumExpr(dialectName, "CASE WHEN anomalous THEN 0 ELSE COALESCE(input, 0) END")
	sumOut := sqlBigIntSumExpr(dialectName, "CASE WHEN anomalous THEN 0 ELSE COALESCE(output, 0) END")
	sumCR := sqlBigIntSumExpr(dialectName, "CASE WHEN anomalous THEN 0 ELSE COALESCE(cache_read, 0) END")
	sumCW := sqlBigIntSumExpr(dialectName, "CASE WHEN anomalous THEN 0 ELSE COALESCE(cache_write, 0) END")
	sumCalls := sqlBigIntSumExpr(dialectName, "COALESCE(calls, 0)")
	cntEvents := "COUNT(*)"

	membersJSON, err := json.Marshal(memberUserIDs)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal member user ids: %w", err)
	}
	memberClause := "billing_user_id IN (SELECT value FROM json_each(?))"
	if dialectName == dialect.Postgres {
		memberClause = "billing_user_id IN (SELECT jsonb_array_elements_text(CAST(? AS jsonb)))"
	}

	args := []any{fromUTC, toUTC, string(membersJSON), groupParam}

	query := fmt.Sprintf(`
SELECT
	%s AS day_str,
	COALESCE(project_id, '') AS proj_id,
	%s AS tok_sum,
	%s AS in_sum,
	%s AS out_sum,
	%s AS cr_sum,
	%s AS cw_sum,
	%s AS calls_sum,
	%s AS events_cnt
FROM usage_events
WHERE event_ts >= ? AND event_ts < ?
  AND %s
  AND %s
GROUP BY %s, COALESCE(project_id, '')
ORDER BY day_str ASC, proj_id ASC`,
		dayExpr, sumTok, sumIn, sumOut, sumCR, sumCW, sumCalls, cntEvents,
		memberClause, notChargedClause, dayExpr,
	)
	query = usageRebindSQL(dialectName, query)

	rows := &entsql.Rows{}
	if err := drv.Query(ctx, query, args, rows); err != nil {
		return nil, nil, fmt.Errorf("query uncharged group episode seed: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type rowKey struct {
		dayUTC time.Time
		projID string
	}
	byKey := make(map[rowKey]*usageGroupSeedRow)
	projectSet := make(map[string]struct{})

	addAgg := func(dayUTC time.Time, projID string, tok, in, out, cr, cw, calls, evCnt int64) {
		k := rowKey{dayUTC: dayUTC, projID: projID}
		r, ok := byKey[k]
		if !ok {
			r = &usageGroupSeedRow{dayUTC: dayUTC, projectScopeID: projID}
			byKey[k] = r
		}
		r.tokensSummable = saturatingAddInt64(r.tokensSummable, tok)
		r.input = saturatingAddInt64(r.input, in)
		r.output = saturatingAddInt64(r.output, out)
		r.cacheRead = saturatingAddInt64(r.cacheRead, cr)
		r.cacheWrite = saturatingAddInt64(r.cacheWrite, cw)
		r.calls = saturatingAddInt64(r.calls, calls)
		r.events = saturatingAddInt64(r.events, evCnt)
	}

	for rows.Next() {
		var dayStr, projID string
		var tok, in, out, cr, cw, calls, evCnt int64
		if err := rows.Scan(&dayStr, &projID, &tok, &in, &out, &cr, &cw, &calls, &evCnt); err != nil {
			return nil, nil, fmt.Errorf("scan uncharged group episode seed: %w", err)
		}
		dayUTC, err := time.Parse("2006-01-02", dayStr)
		if err != nil {
			return nil, nil, fmt.Errorf("parse seed day %q: %w", dayStr, err)
		}
		dayUTC = dayUTC.UTC()
		// Mirror ingest: write BOTH system (project_scope_id="") and project
		// (project_scope_id=<event project>) rows for every uncharged event.
		addAgg(dayUTC, "", tok, in, out, cr, cw, calls, evCnt)
		if projID != "" {
			addAgg(dayUTC, projID, tok, in, out, cr, cw, calls, evCnt)
			projectSet[projID] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate uncharged group episode seed: %w", err)
	}

	outRows := make([]usageGroupSeedRow, 0, len(byKey))
	for _, r := range byKey {
		outRows = append(outRows, *r)
	}
	sort.Slice(outRows, func(i, j int) bool {
		if !outRows[i].dayUTC.Equal(outRows[j].dayUTC) {
			return outRows[i].dayUTC.Before(outRows[j].dayUTC)
		}
		return outRows[i].projectScopeID < outRows[j].projectScopeID
	})

	touchedProjects := make([]string, 0, len(projectSet))
	for p := range projectSet {
		touchedProjects = append(touchedProjects, p)
	}
	sort.Strings(touchedProjects)
	return outRows, touchedProjects, nil
}

// replaceUsageGroupEpisodeRollupSeedWithDriver replaces ONLY rows tagged with
// origin="seed:<episodeID>" for groupID inside an existing transaction. It
// never touches origin="ingest" or any other episode's seed rows (D6a).
func replaceUsageGroupEpisodeRollupSeedWithDriver(
	ctx context.Context,
	drv dialect.Driver,
	groupID, episodeID string,
	rows []usageGroupSeedRow,
) error {
	origin := groupEpisodeSeedOrigin(episodeID)
	dialectName := drv.Dialect()
	delSQL := usageRebindSQL(dialectName, `
DELETE FROM usage_group_rollups_daily
WHERE group_id = ?
  AND origin = ?`)
	if err := drv.Exec(ctx, delSQL, []any{groupID, origin}, nil); err != nil {
		return fmt.Errorf("delete existing episode seed rollups: %w", err)
	}

	if len(rows) == 0 {
		return nil
	}

	insSQL := usageRebindSQL(dialectName, `
INSERT INTO usage_group_rollups_daily
	(id, day_utc, group_id, project_scope_id, origin, tokens_summable, input, output, cache_read, cache_write, calls, events)
VALUES
	(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	for _, r := range rows {
		if err := drv.Exec(ctx, insSQL, []any{
			uuid.New().String(),
			utcDayStart(r.dayUTC),
			groupID,
			r.projectScopeID,
			origin,
			r.tokensSummable,
			r.input,
			r.output,
			r.cacheRead,
			r.cacheWrite,
			r.calls,
			r.events,
		}, nil); err != nil {
			return fmt.Errorf("insert episode seed rollup %s (%s): %w", r.dayUTC.UTC().Format("2006-01-02"), r.projectScopeID, err)
		}
	}
	return nil
}

// OpenUsageGroupChargingEpisode implements store.UsageBudgetStore.OpenUsageGroupChargingEpisode.
//
// Under the per-group advisory lock:
//   - If an open episode (ended_at IS NULL) already exists for groupID, returns
//     (existing, opened=false, nil) — bindings activating inside an open
//     episode trigger NO seed (D6a).
//   - Otherwise creates a new episode with seed_from_utc = max(utcMonthStart(activatedAt),
//     previousEpisode.ended_at) and cutoff_at = activatedAt + 2m.
func (s *UsageLedgerStore) OpenUsageGroupChargingEpisode(
	ctx context.Context,
	groupID string,
	activatedAt time.Time,
) (*store.UsageGroupSeedEpisode, bool, error) {
	if groupID == "" {
		return nil, false, fmt.Errorf("%w: group_id is required", store.ErrInvalidInput)
	}
	if activatedAt.IsZero() {
		activatedAt = time.Now().UTC()
	}
	activatedAt = activatedAt.UTC().Truncate(time.Microsecond)

	tx, err := s.client.Tx(ctx)
	if err != nil {
		return nil, false, mapError(err)
	}
	defer func() { _ = tx.Rollback() }()

	txClient := tx.Client()
	drv := txClient.Driver()
	if err := acquireGroupEpisodeAdvisoryLock(ctx, drv, groupID); err != nil {
		return nil, false, fmt.Errorf("acquire group episode lock: %w", err)
	}

	existingOpen, err := txClient.UsageGroupBudgetSeed.Query().
		Where(
			usagegroupbudgetseed.GroupIDEQ(groupID),
			usagegroupbudgetseed.EndedAtIsNil(),
		).
		Order(ent.Desc(usagegroupbudgetseed.FieldActivatedAt)).
		First(ctx)
	if err != nil && !ent.IsNotFound(err) {
		return nil, false, mapError(err)
	}
	if existingOpen != nil {
		if err := tx.Commit(); err != nil {
			return nil, false, mapError(err)
		}
		return entUsageGroupSeedToStore(existingOpen), false, nil
	}

	// Determine seed_from_utc = max(current UTC month start, previous episode's
	// ended_at, previous episode's cutoff_at) so a rapid close-before-cutoff and
	// reopen never overlaps the previous episode's [seed_from, cutoff_at) seed
	// window (D6a, R1).
	seedFrom := utcMonthStart(activatedAt)
	prevClosed, err := txClient.UsageGroupBudgetSeed.Query().
		Where(
			usagegroupbudgetseed.GroupIDEQ(groupID),
			usagegroupbudgetseed.EndedAtNotNil(),
		).
		Order(ent.Desc(usagegroupbudgetseed.FieldEndedAt)).
		First(ctx)
	if err != nil && !ent.IsNotFound(err) {
		return nil, false, mapError(err)
	}
	if prevClosed != nil {
		if prevClosed.EndedAt != nil {
			prevEnd := prevClosed.EndedAt.UTC().Truncate(time.Microsecond)
			if prevEnd.After(seedFrom) {
				seedFrom = prevEnd
			}
		}
		prevCutoff := prevClosed.CutoffAt.UTC().Truncate(time.Microsecond)
		if prevCutoff.After(seedFrom) {
			seedFrom = prevCutoff
		}
		// Guarantee strict uniqueness on (group_id, activated_at) if a rapid
		// close+reopen occurs within the same microsecond.
		if !activatedAt.After(prevClosed.ActivatedAt.UTC()) {
			activatedAt = prevClosed.ActivatedAt.UTC().Add(time.Microsecond)
		}
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	cutoffAt := activatedAt.Add(usageGroupEpisodeTTLCoverDelay)
	created, err := txClient.UsageGroupBudgetSeed.Create().
		SetID(uuid.New()).
		SetGroupID(groupID).
		SetActivatedAt(activatedAt).
		SetSeedFromUtc(seedFrom).
		SetCutoffAt(cutoffAt).
		SetMemberSnapshotSet(false).
		SetFinalized(false).
		SetStateVersion(1).
		SetCreatedAt(now).
		SetUpdatedAt(now).
		Save(ctx)
	if err != nil {
		return nil, false, mapError(err)
	}
	if err := tx.Commit(); err != nil {
		return nil, false, mapError(err)
	}
	return entUsageGroupSeedToStore(created), true, nil
}

// CloseUsageGroupChargingEpisodeIfInactive implements
// store.UsageBudgetStore.CloseUsageGroupChargingEpisodeIfInactive.
func (s *UsageLedgerStore) CloseUsageGroupChargingEpisodeIfInactive(
	ctx context.Context,
	groupID string,
	endedAt time.Time,
) (*store.UsageGroupSeedEpisode, bool, error) {
	if groupID == "" {
		return nil, false, fmt.Errorf("%w: group_id is required", store.ErrInvalidInput)
	}
	if endedAt.IsZero() {
		endedAt = time.Now().UTC()
	}
	endedAt = endedAt.UTC().Truncate(time.Microsecond)

	tx, err := s.client.Tx(ctx)
	if err != nil {
		return nil, false, mapError(err)
	}
	defer func() { _ = tx.Rollback() }()

	txClient := tx.Client()
	drv := txClient.Driver()
	if err := acquireGroupEpisodeAdvisoryLock(ctx, drv, groupID); err != nil {
		return nil, false, fmt.Errorf("acquire group episode lock: %w", err)
	}

	// Check whether groupID still has ANY active pooled binding (day or month, any scope).
	defs, err := txClient.LimitDefinition.Query().
		Where(limitdefinition.NameIn(
			store.LimitLLMTokensPerGroupDay,
			store.LimitLLMTokensPerGroupMonth,
		)).
		All(ctx)
	if err != nil {
		return nil, false, mapError(err)
	}
	if len(defs) > 0 {
		defIDs := make([]uuid.UUID, 0, len(defs))
		for _, d := range defs {
			defIDs = append(defIDs, d.ID)
		}
		hasActive, err := txClient.EntitlementBinding.Query().
			Where(
				entitlementbinding.LimitDefinitionIDIn(defIDs...),
				entitlementbinding.SubjectTypeEQ(store.EntitlementSubjectGroup),
				entitlementbinding.SubjectIDEQ(groupID),
				entitlementbinding.ValueGT(0),
			).
			Exist(ctx)
		if err != nil {
			return nil, false, mapError(err)
		}
		if hasActive {
			if err := tx.Commit(); err != nil {
				return nil, false, mapError(err)
			}
			return nil, false, nil
		}
	}

	openEp, err := txClient.UsageGroupBudgetSeed.Query().
		Where(
			usagegroupbudgetseed.GroupIDEQ(groupID),
			usagegroupbudgetseed.EndedAtIsNil(),
		).
		Order(ent.Desc(usagegroupbudgetseed.FieldActivatedAt)).
		First(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			if commitErr := tx.Commit(); commitErr != nil {
				return nil, false, mapError(commitErr)
			}
			return nil, false, nil
		}
		return nil, false, mapError(err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	n, err := txClient.UsageGroupBudgetSeed.Update().
		Where(
			usagegroupbudgetseed.IDEQ(openEp.ID),
			usagegroupbudgetseed.StateVersionEQ(openEp.StateVersion),
		).
		SetEndedAt(endedAt).
		SetStateVersion(openEp.StateVersion + 1).
		SetUpdatedAt(now).
		Save(ctx)
	if err != nil {
		return nil, false, mapError(err)
	}
	if n == 0 {
		return nil, false, store.ErrBudgetStateConflict
	}

	updated, err := txClient.UsageGroupBudgetSeed.Get(ctx, openEp.ID)
	if err != nil {
		return nil, false, mapError(err)
	}
	if err := tx.Commit(); err != nil {
		return nil, false, mapError(err)
	}
	return entUsageGroupSeedToStore(updated), true, nil
}

// ExecuteUsageGroupEpisodeSeedPass implements
// store.UsageBudgetStore.ExecuteUsageGroupEpisodeSeedPass.
func (s *UsageLedgerStore) ExecuteUsageGroupEpisodeSeedPass(
	ctx context.Context,
	in store.UsageGroupEpisodeSeedPassInput,
) (*store.UsageGroupEpisodeSeedPassResult, error) {
	if in.EpisodeID == "" || in.GroupID == "" {
		return nil, fmt.Errorf("%w: episode_id and group_id are required", store.ErrInvalidInput)
	}
	epUUID, err := uuid.Parse(in.EpisodeID)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid episode_id: %v", store.ErrInvalidInput, err)
	}
	now := in.Now.UTC().Truncate(time.Microsecond)
	if now.IsZero() {
		now = time.Now().UTC().Truncate(time.Microsecond)
	}

	var outRes *store.UsageGroupEpisodeSeedPassResult
	runOnce := func() error {
		tx, err := s.client.Tx(ctx)
		if err != nil {
			return mapError(err)
		}
		defer func() { _ = tx.Rollback() }()

		txClient := tx.Client()
		drv := txClient.Driver()
		if err := acquireGroupEpisodeAdvisoryLock(ctx, drv, in.GroupID); err != nil {
			return fmt.Errorf("acquire group episode lock: %w", err)
		}

		ep, err := txClient.UsageGroupBudgetSeed.Query().
			Where(
				usagegroupbudgetseed.IDEQ(epUUID),
				usagegroupbudgetseed.GroupIDEQ(in.GroupID),
			).
			Only(ctx)
		if err != nil {
			return mapError(err)
		}

		// Finalized episodes are immutable forever (D6a).
		if ep.Finalized {
			if err := tx.Commit(); err != nil {
				return mapError(err)
			}
			outRes = &store.UsageGroupEpisodeSeedPassResult{
				Episode:   entUsageGroupSeedToStore(ep),
				DidMutate: false,
			}
			return nil
		}

		frozenMembers := ep.FrozenMemberIds
		memberSnapshotSet := ep.MemberSnapshotSet
		if !memberSnapshotSet {
			members, err := NewGroupStore(txClient).GetEffectiveGroupMemberUserIDs(ctx, in.GroupID)
			if err != nil {
				return fmt.Errorf("snapshot effective group members: %w", err)
			}
			frozenMembers = members
			memberSnapshotSet = true
		}

		effectiveFrom := ep.SeedFromUtc.UTC()
		if !in.LedgerRetentionCutoff.IsZero() && effectiveFrom.Before(in.LedgerRetentionCutoff.UTC()) {
			effectiveFrom = in.LedgerRetentionCutoff.UTC()
		}
		effectiveTo := ep.CutoffAt.UTC()

		var seedRows []usageGroupSeedRow
		var touchedProjects []string
		if len(frozenMembers) > 0 && effectiveTo.After(effectiveFrom) {
			seedRows, touchedProjects, err = computeUnchargedGroupEpisodeRollupSeedWithDriver(
				ctx, drv, in.GroupID, frozenMembers, effectiveFrom, effectiveTo,
			)
			if err != nil {
				return fmt.Errorf("compute uncharged group episode seed: %w", err)
			}
		}

		if err := replaceUsageGroupEpisodeRollupSeedWithDriver(ctx, drv, in.GroupID, ep.ID.String(), seedRows); err != nil {
			return fmt.Errorf("replace group episode rollup seed: %w", err)
		}

		finalized := in.IsFinalPass || !now.Before(ep.CutoffAt.UTC())

		upd := txClient.UsageGroupBudgetSeed.Update().
			Where(
				usagegroupbudgetseed.IDEQ(ep.ID),
				usagegroupbudgetseed.StateVersionEQ(ep.StateVersion),
			).
			SetMemberSnapshotSet(memberSnapshotSet).
			SetFinalized(finalized).
			SetStateVersion(ep.StateVersion + 1).
			SetUpdatedAt(now)
		if len(frozenMembers) > 0 {
			upd = upd.SetFrozenMemberIds(frozenMembers)
		} else {
			upd = upd.ClearFrozenMemberIds()
		}
		n, err := upd.Save(ctx)
		if err != nil {
			return mapError(err)
		}
		if n == 0 {
			return store.ErrBudgetStateConflict
		}

		saved, err := txClient.UsageGroupBudgetSeed.Get(ctx, ep.ID)
		if err != nil {
			return mapError(err)
		}
		if err := tx.Commit(); err != nil {
			return mapError(err)
		}
		outRes = &store.UsageGroupEpisodeSeedPassResult{
			Episode:             entUsageGroupSeedToStore(saved),
			DidMutate:           true,
			TouchedProjectScope: touchedProjects,
		}
		return nil
	}

	for attempt := 0; attempt < 4; attempt++ {
		err = runOnce()
		if err == nil {
			return outRes, nil
		}
		if !errors.Is(err, store.ErrBudgetStateConflict) {
			return nil, err
		}
	}
	return nil, err
}

// GetUsageGroupSeedEpisode implements store.UsageBudgetStore.GetUsageGroupSeedEpisode.
func (s *UsageLedgerStore) GetUsageGroupSeedEpisode(ctx context.Context, episodeID string) (*store.UsageGroupSeedEpisode, error) {
	epUUID, err := uuid.Parse(episodeID)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid episode_id: %v", store.ErrInvalidInput, err)
	}
	row, err := s.client.UsageGroupBudgetSeed.Get(ctx, epUUID)
	if err != nil {
		return nil, mapError(err)
	}
	return entUsageGroupSeedToStore(row), nil
}

// GetOpenUsageGroupChargingEpisode implements store.UsageBudgetStore.GetOpenUsageGroupChargingEpisode.
func (s *UsageLedgerStore) GetOpenUsageGroupChargingEpisode(ctx context.Context, groupID string) (*store.UsageGroupSeedEpisode, error) {
	if groupID == "" {
		return nil, fmt.Errorf("%w: group_id is required", store.ErrInvalidInput)
	}
	row, err := s.client.UsageGroupBudgetSeed.Query().
		Where(
			usagegroupbudgetseed.GroupIDEQ(groupID),
			usagegroupbudgetseed.EndedAtIsNil(),
		).
		Order(ent.Desc(usagegroupbudgetseed.FieldActivatedAt)).
		First(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entUsageGroupSeedToStore(row), nil
}

// ListIncompleteUsageGroupSeedEpisodes implements store.UsageBudgetStore.ListIncompleteUsageGroupSeedEpisodes.
func (s *UsageLedgerStore) ListIncompleteUsageGroupSeedEpisodes(ctx context.Context) ([]*store.UsageGroupSeedEpisode, error) {
	rows, err := s.client.UsageGroupBudgetSeed.Query().
		Where(usagegroupbudgetseed.FinalizedEQ(false)).
		Order(ent.Asc(usagegroupbudgetseed.FieldActivatedAt)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]*store.UsageGroupSeedEpisode, 0, len(rows))
	for _, r := range rows {
		out = append(out, entUsageGroupSeedToStore(r))
	}
	return out, nil
}

// ListUsageGroupSeedEpisodesOverlapping implements store.UsageBudgetStore.ListUsageGroupSeedEpisodesOverlapping.
func (s *UsageLedgerStore) ListUsageGroupSeedEpisodesOverlapping(
	ctx context.Context,
	groupID string,
	windowStartUTC, windowEndUTC time.Time,
) ([]*store.UsageGroupSeedEpisode, error) {
	if groupID == "" || !windowEndUTC.After(windowStartUTC) {
		return nil, nil
	}
	rows, err := s.client.UsageGroupBudgetSeed.Query().
		Where(
			usagegroupbudgetseed.GroupIDEQ(groupID),
			usagegroupbudgetseed.MemberSnapshotSetEQ(true),
			usagegroupbudgetseed.SeedFromUtcLT(windowEndUTC.UTC()),
			usagegroupbudgetseed.CutoffAtGT(windowStartUTC.UTC()),
		).
		Order(ent.Asc(usagegroupbudgetseed.FieldActivatedAt)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]*store.UsageGroupSeedEpisode, 0, len(rows))
	for _, r := range rows {
		out = append(out, entUsageGroupSeedToStore(r))
	}
	return out, nil
}

// QueryGroupMemberUsageBreakdown implements store.UsageBudgetStore.QueryGroupMemberUsageBreakdown.
// Aggregates per-member consumption directly in SQL (GROUP BY billing_user_id),
// combining ingest-charged events (groupID in charged_group_ids) and uncharged
// pre-activation seeded events from all overlapping charging episodes (each
// usage_events row matched at most once, D6a, R5).
func (s *UsageLedgerStore) QueryGroupMemberUsageBreakdown(
	ctx context.Context,
	groupID, projectScopeID string,
	episodes []*store.UsageGroupSeedEpisode,
	windowStartUTC, windowEndUTC time.Time,
) ([]store.UsageGroupMemberBreakdown, error) {
	if groupID == "" || !windowEndUTC.After(windowStartUTC) {
		return nil, nil
	}

	drv := s.client.Driver()
	dialectName := drv.Dialect()
	winStart := windowStartUTC.UTC().Truncate(time.Microsecond)
	winEnd := windowEndUTC.UTC().Truncate(time.Microsecond)

	var groupParam any = groupID
	chargedClause := "(charged_group_ids IS NOT NULL AND EXISTS (SELECT 1 FROM json_each(usage_events.charged_group_ids) WHERE json_each.value = ?))"
	notChargedClause := "(charged_group_ids IS NULL OR NOT EXISTS (SELECT 1 FROM json_each(usage_events.charged_group_ids) WHERE json_each.value = ?))"
	if dialectName == dialect.Postgres {
		b, err := json.Marshal([]string{groupID})
		if err != nil {
			return nil, fmt.Errorf("marshal group id json: %w", err)
		}
		groupParam = string(b)
		chargedClause = "(charged_group_ids IS NOT NULL AND charged_group_ids @> CAST(? AS jsonb))"
		notChargedClause = "(charged_group_ids IS NULL OR NOT (charged_group_ids @> CAST(? AS jsonb)))"
	}

	sumTok := sqlBigIntSumExpr(dialectName, "CASE WHEN anomalous THEN 0 ELSE COALESCE(input, 0) + COALESCE(output, 0) + COALESCE(cache_read, 0) + COALESCE(cache_write, 0) END")
	sumCalls := sqlBigIntSumExpr(dialectName, "COALESCE(calls, 0)")

	var whereParts []string
	args := make([]any, 0, 16)

	whereParts = append(whereParts, "event_ts >= ? AND event_ts < ?")
	args = append(args, winStart, winEnd)

	if projectScopeID != "" {
		whereParts = append(whereParts, "project_id = ?")
		args = append(args, projectScopeID)
	}

	// Build OR clause across all overlapping episodes with non-empty member
	// snapshots, using a single JSON array parameter per episode (O2) and
	// clamping finalized episodes by received_at <= ep.CutoffAt so late
	// uncharged uploads after finalization do not inflate the breakdown (O3).
	memberClause := "billing_user_id IN (SELECT value FROM json_each(?))"
	if dialectName == dialect.Postgres {
		memberClause = "billing_user_id IN (SELECT jsonb_array_elements_text(CAST(? AS jsonb)))"
	}
	var epClauses []string
	var epArgs []any
	for _, ep := range episodes {
		if ep == nil || !ep.MemberSnapshotSet || len(ep.FrozenMemberIDs) == 0 {
			continue
		}
		effFrom := ep.SeedFromUTC.UTC().Truncate(time.Microsecond)
		if effFrom.Before(winStart) {
			effFrom = winStart
		}
		effTo := ep.CutoffAt.UTC().Truncate(time.Microsecond)
		if effTo.After(winEnd) {
			effTo = winEnd
		}
		if !effTo.After(effFrom) {
			continue
		}
		membersJSON, err := json.Marshal(ep.FrozenMemberIDs)
		if err != nil {
			return nil, fmt.Errorf("marshal episode member user ids: %w", err)
		}
		if ep.Finalized {
			epClauses = append(epClauses, fmt.Sprintf("(event_ts >= ? AND event_ts < ? AND received_at <= ? AND %s)", memberClause))
			epArgs = append(epArgs, effFrom, effTo, ep.CutoffAt.UTC().Truncate(time.Microsecond), string(membersJSON))
		} else {
			epClauses = append(epClauses, fmt.Sprintf("(event_ts >= ? AND event_ts < ? AND %s)", memberClause))
			epArgs = append(epArgs, effFrom, effTo, string(membersJSON))
		}
	}

	if len(epClauses) > 0 {
		whereParts = append(whereParts, fmt.Sprintf(
			"(%s OR (%s AND (%s)))",
			chargedClause, notChargedClause, strings.Join(epClauses, " OR "),
		))
		args = append(args, groupParam, groupParam)
		args = append(args, epArgs...)
	} else {
		whereParts = append(whereParts, chargedClause)
		args = append(args, groupParam)
	}

	query := fmt.Sprintf(`
SELECT
	COALESCE(billing_user_id, '') AS uid,
	%s AS tok_sum,
	%s AS calls_sum,
	COUNT(*) AS events_cnt
FROM usage_events
WHERE %s
GROUP BY COALESCE(billing_user_id, '')
ORDER BY tok_sum DESC, uid ASC`,
		sumTok, sumCalls, strings.Join(whereParts, " AND "),
	)
	query = usageRebindSQL(dialectName, query)

	rows := &entsql.Rows{}
	if err := drv.Query(ctx, query, args, rows); err != nil {
		return nil, fmt.Errorf("query group member usage breakdown: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]store.UsageGroupMemberBreakdown, 0)
	for rows.Next() {
		var uid string
		var tok, calls, evCnt int64
		if err := rows.Scan(&uid, &tok, &calls, &evCnt); err != nil {
			return nil, fmt.Errorf("scan group member usage breakdown: %w", err)
		}
		if uid == "" {
			continue
		}
		out = append(out, store.UsageGroupMemberBreakdown{
			UserID:         uid,
			TokensSummable: tok,
			Calls:          calls,
			Events:         evCnt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate group member usage breakdown: %w", err)
	}
	return out, nil
}
