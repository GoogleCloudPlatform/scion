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

package agydecode

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	_ "modernc.org/sqlite" // Pure-Go SQLite driver ("sqlite")
)

const (
	defaultBusyTimeoutMs = 2000
	genMetadataPageRows  = 1
	stepsPageRows        = 64
)

type scannedRow struct {
	rowID int64
	idx   int64
	meta  *decodedRowMetadata
}

type usageCandidate struct {
	source           SourceTable
	rowID            int64
	idx              int64
	usageIndex       int
	isRetry          bool
	model            string
	modelID          int64
	modelQuality     int // 4=concrete inline, 3=learned from DB, 2=static table, 1=non-concrete inline, 0=none
	timestamp        *time.Time
	timestampQuality int // 2=row timestamp, 1=trajectory timestamp, 0=none
	tokens           TokenUsage
	keys             []string
	fallbackIdentity string
}

// DecodeFile opens an Antigravity conversation SQLite database read-only,
// paginates gen_metadata (1 row per query) and optionally steps, decodes
// protobuf usage records, deduplicates via union-find over IDs [11, 12, 7],
// and returns canonical usage records with verification status.
func DecodeFile(ctx context.Context, dbPath string, opts Options) (*Result, error) {
	fp, err := Fingerprint(dbPath)
	if err != nil {
		return nil, err
	}

	agyVer := NormalizeVersion(opts.AgyVersion)
	verVerified := IsVerifiedVersion(agyVer)

	sessionID := strings.TrimSuffix(filepath.Base(dbPath), filepath.Ext(dbPath))
	res := &Result{
		AgyVersion:      agyVer,
		VersionVerified: verVerified,
		DBPath:          dbPath,
		SessionID:       sessionID,
		Fingerprint:     fp,
		Cursor:          opts.Cursor.Clone(),
		Records:         []Record{},
	}

	busyMs := opts.BusyTimeoutMs
	if busyMs <= 0 {
		busyMs = defaultBusyTimeoutMs
	}

	dsn, err := buildReadOnlyDSN(dbPath, busyMs)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open agy db %s: %w", dbPath, err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)

	// Run all reads inside a single read-only transaction so gen_metadata,
	// trajectory_metadata_blob, and steps see one consistent WAL snapshot
	// while still streaming one gen_metadata row per query.
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin read-only tx %s: %w", dbPath, err)
	}
	defer func() { _ = tx.Rollback() }()

	tables, err := listTables(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("inspect schema %s: %w", dbPath, err)
	}

	// Not a conversation DB (e.g. conversation_summaries.db).
	if !tables["gen_metadata"] && !tables["steps"] {
		res.Status = aggregateStatus(verVerified, len(res.Errors))
		return res, nil
	}

	var genRows []scannedRow
	if tables["gen_metadata"] {
		var maxRowID *int64
		var pending []int64
		var rowErrs []DecodeError
		genRows, maxRowID, pending, rowErrs, err = readMetadataTable(
			ctx, tx, SourceGenMetadata,
			"SELECT rowid, idx, data FROM gen_metadata WHERE rowid = ?",
			"SELECT rowid, idx, data FROM gen_metadata WHERE rowid > ? ORDER BY rowid LIMIT ?",
			genMetadataPageRows, opts.Cursor.GenMetadataRowID, opts.Cursor.PendingGenMetadataRowIDs, false,
		)
		if err != nil {
			return nil, fmt.Errorf("read gen_metadata %s: %w", dbPath, err)
		}
		if maxRowID != nil {
			res.Cursor.GenMetadataRowID = maxRowID
		}
		res.Cursor.PendingGenMetadataRowIDs = pending
		res.Errors = append(res.Errors, rowErrs...)
	}

	var trajTS *time.Time
	if tables["trajectory_metadata_blob"] {
		var tsWarnings []string
		trajTS, tsWarnings, err = readTrajectoryTimestamp(ctx, tx)
		if err != nil {
			return nil, fmt.Errorf("read trajectory_metadata_blob %s: %w", dbPath, err)
		}
		res.Warnings = append(res.Warnings, tsWarnings...)
	}

	var stepRows []scannedRow
	if tables["steps"] && !opts.SkipSteps {
		var maxRowID *int64
		var pending []int64
		var rowErrs []DecodeError
		stepRows, maxRowID, pending, rowErrs, err = readMetadataTable(
			ctx, tx, SourceSteps,
			"SELECT rowid, idx, metadata FROM steps WHERE rowid = ?",
			"SELECT rowid, idx, metadata FROM steps WHERE rowid > ? ORDER BY rowid LIMIT ?",
			stepsPageRows, opts.Cursor.StepsRowID, opts.Cursor.PendingStepsRowIDs, true,
		)
		if err != nil {
			return nil, fmt.Errorf("read steps %s: %w", dbPath, err)
		}
		if maxRowID != nil {
			res.Cursor.StepsRowID = maxRowID
		}
		res.Cursor.PendingStepsRowIDs = pending
		res.Errors = append(res.Errors, rowErrs...)
	}

	// Learn concrete model_id -> model_name mappings across all decoded rows
	// (seeded from DedupeState when verified so later incremental polls keep
	// consistent model attribution). Ambiguous mappings are discarded.
	learnedModels := make(map[int64]string)
	ambiguousModels := make(map[int64]bool)
	if opts.Dedupe != nil && verVerified {
		for id, name := range opts.Dedupe.LearnedModels {
			learnedModels[id] = name
		}
		for id, amb := range opts.Dedupe.AmbiguousModels {
			if amb {
				ambiguousModels[id] = true
			}
		}
	}
	recordLearned := func(id int64, model string) {
		if id <= 0 || !IsConcreteModel(model) || ambiguousModels[id] {
			return
		}
		if prev, ok := learnedModels[id]; ok && prev != model {
			delete(learnedModels, id)
			ambiguousModels[id] = true
			return
		}
		learnedModels[id] = model
	}
	for _, r := range genRows {
		recordLearned(r.meta.modelID, r.meta.model)
	}
	for _, r := range stepRows {
		recordLearned(r.meta.modelID, r.meta.model)
	}
	if opts.Dedupe != nil && verVerified && (len(learnedModels) > 0 || len(ambiguousModels) > 0) {
		opts.Dedupe.ensureInit()
		for id, name := range learnedModels {
			opts.Dedupe.LearnedModels[id] = name
		}
		for id, amb := range ambiguousModels {
			if amb {
				delete(opts.Dedupe.LearnedModels, id)
				opts.Dedupe.AmbiguousModels[id] = true
			}
		}
	}

	// Build raw usage candidates.
	candidates := buildCandidates(sessionID, genRows, stepRows, trajTS, learnedModels, ambiguousModels)

	// Deduplicate within this decode pass using union-find over [11, 12, 7].
	merged := mergeCandidates(candidates)

	// Apply persistent DedupeState (if provided) and construct final Records.
	recordStatus := StatusUnverified
	if verVerified {
		recordStatus = StatusVerified
	}

	for _, c := range merged {
		allKeys := c.keys
		sourceIdentity := telemetrycontract.SourceIdentityRequestID
		if len(allKeys) == 0 {
			allKeys = []string{c.fallbackIdentity}
			sourceIdentity = telemetrycontract.SourceIdentityComposite
		}

		var canonicalID, emissionKey string
		tok := c.tokens
		var isDelta bool
		if opts.Dedupe != nil && verVerified {
			var emit bool
			canonicalID, emissionKey, tok, isDelta, emit = opts.Dedupe.RecordDelta(c.tokens, allKeys...)
			if !emit {
				// Already emitted at or above these per-channel counts in an earlier poll.
				continue
			}
		} else {
			// Deterministic canonical key from sorted keys (unverified decodes never mutate Dedupe).
			canonicalID = allKeys[0]
			emissionKey = computeEmissionKey(canonicalID, c.tokens)
		}

		model := c.model
		modelSource := telemetrycontract.ModelSourceEvent
		if model == "" {
			if c.modelID > 0 {
				model = fmt.Sprintf("antigravity-model-%d", c.modelID)
			} else {
				model = telemetrycontract.UnknownModel
				modelSource = telemetrycontract.ModelSourceUnknown
			}
		}
		model = telemetrycontract.ResolveModelLabel(model, "")

		rec := Record{
			Status:         recordStatus,
			SessionID:      sessionID,
			Source:         c.source,
			RowID:          c.rowID,
			Idx:            c.idx,
			UsageIndex:     c.usageIndex,
			IsRetry:        c.isRetry,
			IsDelta:        isDelta,
			Identity:       canonicalID,
			EmissionKey:    emissionKey,
			SourceIdentity: sourceIdentity,
			IdentityKeys:   c.keys,
			Timestamp:      c.timestamp,
			Model:          model,
			ModelID:        c.modelID,
			ModelSource:    modelSource,
			Tokens:         &tok,
		}
		res.Records = append(res.Records, rec)
	}

	// Emit malformed sentinel records for any row decode errors so callers
	// inspecting Records directly also see malformed entries (with nil Tokens).
	for _, de := range res.Errors {
		res.Records = append(res.Records, Record{
			Status:         StatusMalformed,
			SessionID:      sessionID,
			Source:         de.Source,
			RowID:          de.RowID,
			Idx:            de.Idx,
			Identity:       fmt.Sprintf("antigravity:%s:malformed:%s:%d", sessionID, de.Source, de.RowID),
			SourceIdentity: telemetrycontract.SourceIdentityComposite,
			Model:          telemetrycontract.UnknownModel,
			ModelSource:    telemetrycontract.ModelSourceUnknown,
			Tokens:         nil,
			Error:          de.Error,
		})
	}

	res.Status = aggregateStatus(verVerified, len(res.Errors))
	return res, nil
}

func aggregateStatus(verVerified bool, errCount int) DecodeStatus {
	if errCount > 0 {
		return StatusMalformed
	}
	if !verVerified {
		return StatusUnverified
	}
	return StatusVerified
}

func buildReadOnlyDSN(dbPath string, busyTimeoutMs int) (string, error) {
	absPath, err := filepath.Abs(dbPath)
	if err != nil {
		return "", fmt.Errorf("resolve db path %s: %w", dbPath, err)
	}
	u := url.URL{
		Scheme: "file",
		Path:   filepath.ToSlash(absPath),
	}
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeoutMs))
	q.Add("_pragma", "query_only(1)")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

type sqlQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func listTables(ctx context.Context, q sqlQuerier) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type = 'table'")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	tables := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		tables[name] = true
	}
	return tables, rows.Err()
}

func readMetadataTable(
	ctx context.Context,
	q sqlQuerier,
	source SourceTable,
	byRowIDQuery string,
	pageQuery string,
	pageRows int,
	afterRowID *int64,
	pendingRowIDs []int64,
	step bool,
) ([]scannedRow, *int64, []int64, []DecodeError, error) {
	var results []scannedRow
	var decodeErrs []DecodeError
	var nextPending []int64
	var maxRowID *int64
	if afterRowID != nil {
		v := *afterRowID
		maxRowID = &v
	}

	processRow := func(rowID int64, idx sql.NullInt64, blob []byte) {
		// Track NULL/empty-blob rows in the bounded pending set rather than
		// freezing maxRowID: permanently-NULL rows (e.g. non-LLM user steps)
		// must not pin the cursor, while in-place NULL -> populated UPDATEs
		// are picked up on the next poll.
		if (step && len(blob) == 0) || (!step && blob == nil) {
			nextPending = append(nextPending, rowID)
			return
		}
		if !idx.Valid {
			decodeErrs = append(decodeErrs, DecodeError{
				Source: source,
				RowID:  rowID,
				Error:  "null idx column",
			})
			return
		}
		meta, err := decodeRowMetadata(blob, step)
		if err != nil {
			decodeErrs = append(decodeErrs, DecodeError{
				Source: source,
				RowID:  rowID,
				Idx:    idx.Int64,
				Error:  err.Error(),
			})
			return
		}
		results = append(results, scannedRow{
			rowID: rowID,
			idx:   idx.Int64,
			meta:  meta,
		})
	}

	// 1. Re-check explicitly tracked pending rowids (one row per query to keep memory bounded).
	seenPending := make(map[int64]bool, len(pendingRowIDs))
	for _, pid := range pendingRowIDs {
		if afterRowID != nil && pid > *afterRowID {
			// Will be covered by the range scan below.
			continue
		}
		if seenPending[pid] {
			continue
		}
		seenPending[pid] = true
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, nil, err
		}
		rows, err := q.QueryContext(ctx, byRowIDQuery, pid)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		for rows.Next() {
			var rowID int64
			var idx sql.NullInt64
			var blob []byte
			if err := rows.Scan(&rowID, &idx, &blob); err != nil {
				_ = rows.Close()
				return nil, nil, nil, nil, err
			}
			processRow(rowID, idx, blob)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, nil, nil, nil, err
		}
		_ = rows.Close()
	}

	// 2. Paginate newly appended rows strictly after afterRowID.
	// When afterRowID is nil, start from math.MinInt64 so rowid=0 is included.
	var scanCursor int64 = math.MinInt64
	if afterRowID != nil {
		scanCursor = *afterRowID
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, nil, err
		}
		rows, err := q.QueryContext(ctx, pageQuery, scanCursor, pageRows)
		if err != nil {
			return nil, nil, nil, nil, err
		}

		count := 0
		var lastRowID int64
		for rows.Next() {
			count++
			var rowID int64
			var idx sql.NullInt64
			var blob []byte
			if err := rows.Scan(&rowID, &idx, &blob); err != nil {
				_ = rows.Close()
				return nil, nil, nil, nil, err
			}
			lastRowID = rowID
			v := rowID
			maxRowID = &v
			processRow(rowID, idx, blob)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, nil, nil, nil, err
		}
		_ = rows.Close()

		if count < pageRows {
			break
		}
		scanCursor = lastRowID
	}

	nextPending = boundPendingRowIDs(nextPending, MaxPendingRowIDs)
	return results, maxRowID, nextPending, decodeErrs, nil
}

// boundPendingRowIDs deduplicates, sorts ascending, and trims rowids to at most
// limit entries, keeping the most recent (highest) rowids.
func boundPendingRowIDs(ids []int64, limit int) []int64 {
	if len(ids) == 0 {
		return nil
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := ids[:0]
	for _, id := range ids {
		if len(out) == 0 || out[len(out)-1] != id {
			out = append(out, id)
		}
	}
	if limit > 0 && len(out) > limit {
		out = append([]int64(nil), out[len(out)-limit:]...)
	}
	return out
}

func readTrajectoryTimestamp(ctx context.Context, q sqlQuerier) (*time.Time, []string, error) {
	rows, err := q.QueryContext(ctx, "SELECT data FROM trajectory_metadata_blob")
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	var trajTS *time.Time
	var warnings []string
	for rows.Next() {
		var blob []byte
		if err := rows.Scan(&blob); err != nil {
			return nil, nil, err
		}
		if len(blob) == 0 {
			continue
		}
		ts, err := decodeTrajectoryTimestamp(blob)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("trajectory_metadata_blob: %v", err))
			continue
		}
		if trajTS == nil && ts != nil {
			trajTS = ts
		}
	}
	return trajTS, warnings, rows.Err()
}

func buildCandidates(
	sessionID string,
	genRows []scannedRow,
	stepRows []scannedRow,
	trajTS *time.Time,
	learnedModels map[int64]string,
	ambiguousModels map[int64]bool,
) []usageCandidate {
	genByIdx := make(map[int64]*decodedRowMetadata, len(genRows))
	for _, r := range genRows {
		genByIdx[r.idx] = r.meta
	}

	var candidates []usageCandidate
	emit := func(source SourceTable, rows []scannedRow) {
		for _, r := range rows {
			for uIdx, u := range r.meta.usages {
				if !u.hasAny {
					continue
				}
				modelID := u.modelID
				if modelID == 0 {
					modelID = r.meta.modelID
				}
				// A retry entry may use a different model than its enclosing row.
				matchesRow := func(m *decodedRowMetadata) bool {
					return m != nil && (modelID == 0 || m.modelID == modelID)
				}
				var inlineModel string
				if matchesRow(r.meta) && r.meta.model != "" {
					inlineModel = r.meta.model
				} else if source == SourceSteps {
					if g := genByIdx[r.idx]; matchesRow(g) && g.model != "" {
						inlineModel = g.model
					}
				}

				model, modelQuality := resolveCandidateModel(inlineModel, modelID, learnedModels, ambiguousModels)

				ts := r.meta.timestamp
				tsQuality := 2
				if ts == nil {
					ts = trajTS
					if ts != nil {
						tsQuality = 1
					} else {
						tsQuality = 0
					}
				}

				candidates = append(candidates, usageCandidate{
					source:           source,
					rowID:            r.rowID,
					idx:              r.idx,
					usageIndex:       uIdx,
					isRetry:          u.isRetry,
					model:            model,
					modelID:          modelID,
					modelQuality:     modelQuality,
					timestamp:        ts,
					timestampQuality: tsQuality,
					tokens:           u.tokens,
					keys:             u.keys,
					fallbackIdentity: fmt.Sprintf("antigravity:%s:%s:%d:%d", sessionID, source, r.idx, uIdx),
				})
			}
		}
	}

	// Process steps first, then gen_metadata (matching t3code precedence when
	// merging timestamps and metadata across both tables).
	emit(SourceSteps, stepRows)
	emit(SourceGenMetadata, genRows)
	return candidates
}

func resolveCandidateModel(
	inlineModel string,
	modelID int64,
	learnedModels map[int64]string,
	ambiguousModels map[int64]bool,
) (string, int) {
	if IsConcreteModel(inlineModel) {
		return inlineModel, 4
	}
	if modelID > 0 && !ambiguousModels[modelID] {
		if learned, ok := learnedModels[modelID]; ok && learned != "" {
			return learned, 3
		}
		if fallback, ok := LookupStaticModelID(modelID); ok {
			return fallback, 2
		}
	}
	if inlineModel != "" {
		return inlineModel, 1
	}
	return "", 0
}

// mergeCandidates deduplicates candidates within a single decode pass using
// union-find over protobuf identity keys [11, 12, 7].
func mergeCandidates(candidates []usageCandidate) []usageCandidate {
	if len(candidates) == 0 {
		return nil
	}

	type group struct {
		cand   usageCandidate
		parent int
		size   int
		owner  int
	}

	groups := make([]group, len(candidates))
	for i, c := range candidates {
		groups[i] = group{
			cand:   c,
			parent: i,
			size:   1,
			owner:  i,
		}
	}

	find := func(i int) int {
		root := i
		for groups[root].parent != root {
			root = groups[root].parent
		}
		for i != root {
			next := groups[i].parent
			groups[i].parent = root
			i = next
		}
		return root
	}

	merge := func(left, right int) int {
		a, b := find(left), find(right)
		if a == b {
			return a
		}
		if groups[a].size < groups[b].size {
			a, b = b, a
		}
		target := &groups[a]
		src := &groups[b]

		first := target
		if src.owner < target.owner {
			first = src
		}

		bestModel := first
		if src.cand.modelQuality > target.cand.modelQuality {
			bestModel = src
		} else if target.cand.modelQuality > src.cand.modelQuality {
			bestModel = target
		}

		bestTime := target
		if src.cand.timestampQuality > target.cand.timestampQuality {
			bestTime = src
		} else if src.cand.timestampQuality == target.cand.timestampQuality &&
			src.cand.timestamp != nil && target.cand.timestamp != nil &&
			src.cand.timestamp.Before(*target.cand.timestamp) {
			bestTime = src
		}

		merged := first.cand
		merged.model = bestModel.cand.model
		merged.modelID = bestModel.cand.modelID
		merged.modelQuality = bestModel.cand.modelQuality
		merged.timestamp = bestTime.cand.timestamp
		merged.timestampQuality = bestTime.cand.timestampQuality
		merged.tokens = mergeTokenUsage(target.cand.tokens, src.cand.tokens)
		merged.keys = dedupeNonEmpty(append(append([]string(nil), target.cand.keys...), src.cand.keys...))
		// If any merged copy was a primary (non-retry) usage, mark IsRetry false.
		merged.isRetry = target.cand.isRetry && src.cand.isRetry

		target.cand = merged
		target.owner = first.owner
		target.size += src.size
		src.parent = a
		return a
	}

	keyToGroup := make(map[string]int, len(candidates)*3)
	for i, c := range candidates {
		for _, k := range c.keys {
			if existing, ok := keyToGroup[k]; ok {
				merge(i, existing)
			}
			keyToGroup[k] = i
		}
	}

	// Collect roots sorted by lowest owner index for deterministic output order.
	var rootIndices []int
	for i := range groups {
		if groups[i].parent == i {
			rootIndices = append(rootIndices, i)
		}
	}
	sort.Slice(rootIndices, func(i, j int) bool {
		return groups[rootIndices[i]].owner < groups[rootIndices[j]].owner
	})
	out := make([]usageCandidate, 0, len(rootIndices))
	for _, i := range rootIndices {
		out = append(out, groups[i].cand)
	}
	return out
}

func mergeTokenUsage(a, b TokenUsage) TokenUsage {
	out := TokenUsage{
		Input:      maxInt64Ptr(a.Input, b.Input),
		Output:     maxInt64Ptr(a.Output, b.Output),
		CacheRead:  maxInt64Ptr(a.CacheRead, b.CacheRead),
		CacheWrite: maxInt64Ptr(a.CacheWrite, b.CacheWrite),
		Reasoning:  maxInt64Ptr(a.Reasoning, b.Reasoning),
	}
	if out.Input != nil {
		out.InputDerivation = telemetrycontract.InputDerivationReported
	}
	if out.Reasoning != nil && out.Output != nil {
		if *out.Reasoning > *out.Output {
			v := *out.Output
			out.Reasoning = &v
		}
		overlap := true
		out.ReasoningOverlapsOutput = &overlap
	}
	return out
}

func maxInt64Ptr(a, b *int64) *int64 {
	if a == nil && b == nil {
		return nil
	}
	var v int64
	switch {
	case a == nil:
		v = *b
	case b == nil:
		v = *a
	case *a >= *b:
		v = *a
	default:
		v = *b
	}
	return &v
}
