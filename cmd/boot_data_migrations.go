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

package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// maxBootLogErrors is the maximum number of per-row error messages logged
// during a boot-time migration. result.Errors holds one entry per refused
// row and is unbounded; logging it whole turns a bad migration into a
// disk-space incident (design §4.3). We log the first N plus the total.
const maxBootLogErrors = 10

// defaultBackfillBudget is the maximum wall-clock time the message backfill
// is allowed to consume during a single boot. Measured on the gteam snapshot
// (24,700 messages, 39 projects): a full execute run completes in 37 seconds
// (design §7 OQ-1). The budget is set generously at 10 minutes purely as a
// runaway guard rather than a tuning parameter — it is not expected to be
// reached under normal operation. If a single project's backfill exceeds the
// entire budget, it is retried from scratch on every boot; the budget check
// logs at ERROR naming the project (design §4.5).
//
// Exported as a variable (not a constant) so tests can override it.
var defaultBackfillBudget = 10 * time.Minute

// runBootDataMigrations runs the conversation-model data migrations that an
// upgrading hub needs. It is called once during store setup, before the hub
// serves any request, so a completed run is observable to every later reader
// without a gate (design §4.1, F3).
//
// It never returns an error: a failed data migration degrades history or
// leaves a repair pending, neither of which justifies refusing to boot
// (design A2). Failures are logged at ERROR and the completion marker is
// left unwritten so the next boot retries.
//
// After the data migrations, the residual report splits unattributed
// messages into reachable (actionable, WARN) and unreachable (stable,
// INFO) buckets per design §4.6 / M6.
func runBootDataMigrations(ctx context.Context, s store.Store) {
	runWithAdvisoryLock(ctx, s, store.LockDataMigrations, "conversation data migrations", func() {
		// Each migration is wrapped in its own deferred recover so that a
		// panic in one does not prevent the other from running, and neither
		// can kill the process during boot. Design alternative A2 rejected
		// blocking boot on a data-migration failure; an unrecovered panic
		// is a harder version of that same outcome. The marker is NOT
		// written on panic — a panicking pass did not complete, which is
		// a run-level failure under M-1'.
		//
		// runWithAdvisoryLock has an early fn() path (no-op locker) that
		// returns before its deferred release, so it cannot be relied on
		// for containment.
		runMigrationSafe(ctx, s, "DM key migration", runDMKeyMigration)      // §4.4
		runMigrationSafe(ctx, s, "Message backfill", runMessageBackfill)     // §4.5
	})

	// Split the residual report into reachable/unreachable (M6, §4.6).
	reportResidualUnattributed(ctx, s)
}

// runMigrationSafe calls fn inside a deferred recover. A panic is logged at
// ERROR and the function returns normally, so the caller can proceed to the
// next migration and the hub can continue booting. The marker is never
// written: fn is responsible for its own marker write, and a panic aborts
// fn before it can reach that write — which is the correct M-1' outcome
// (a panicking pass is a run-level failure).
func runMigrationSafe(ctx context.Context, s store.Store, label string, fn func(context.Context, store.Store)) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error(fmt.Sprintf("%s: recovered from panic; migration did not complete, will retry next boot", label),
				"panic", r,
			)
		}
	}()
	fn(ctx, s)
}

// runDMKeyMigration runs the DM key migration with M-1' marker semantics.
//
// M-1' (design §4.3): a completion marker records that a full pass
// completed without a run-level failure. Row-level refusals are counted,
// persisted alongside the marker, and reported — they do NOT block the
// marker. A marker must never be written for a pass that did not finish.
//
// The distinction is load-bearing: on production data the migration
// produces thousands of deterministic row-level refusals that no retry
// can change. Blocking the marker on those refusals would create a
// permanent livelock — the migration re-runs every boot, making no
// progress, adding tens of seconds to every startup indefinitely.
func runDMKeyMigration(ctx context.Context, s store.Store) {
	// Fast path: already complete. O(1) with respect to data volume.
	done, err := IsMigrationComplete(ctx, s, MigrationDMKey)
	if err != nil {
		slog.Error("DM key migration: failed to check completion marker; will attempt migration",
			"error", err)
		// Fall through: attempting the migration is safer than skipping it
		// when we cannot read the marker. The migration is idempotent.
	} else if done {
		slog.Debug("DM key migration: already complete, skipping")
		return
	}

	slog.Info("DM key migration: starting")

	svc := messaging.NewDMMigrationService(s)
	result, err := svc.Run(ctx, messaging.DMMigrationConfig{
		DryRun: false,
	})

	// --- M-1' decision point ---

	if err != nil {
		// RUN-LEVEL failure: the pass did not complete. Do NOT write the
		// marker. The next boot will retry.
		slog.Error("DM key migration did not complete; will retry next boot",
			"error", err)
		return
	}

	// The pass completed. Row-level refusals are a terminal, correct,
	// permanent outcome — not an error that blocks the marker.
	//
	// Defensive nil guard: result should never be nil when err is nil
	// (DMMigrationService.Run documents this), but guarding it prevents
	// a nil-deref if the contract is ever violated, and it lets the
	// AC-2b mutation test (remove the `return` above) expose a clean
	// assertion failure rather than a crash.
	if result == nil {
		result = &messaging.DMMigrationResult{}
	}
	residuals := len(result.Errors)

	// Log the result.
	slog.Info("DM key migration: pass completed",
		"scanned", result.TotalScanned,
		"rekeyed", result.OldFormatRekeyed,
		"participants_added", result.ParticipantsAdded,
		"empty_ref_skipped", result.EmptyRefSkipped,
		"unparseable", result.Unparseable,
		"ambiguous", result.Ambiguous,
		"row_errors", residuals,
	)

	// Log a bounded sample of per-row errors.
	if residuals > 0 {
		logBoundedErrors("DM key migration", result.Errors, maxBootLogErrors)
	}

	// Write the completion marker. Row-level refusals do not block this.
	if markErr := MarkMigrationComplete(ctx, s, MigrationDMKey, residuals); markErr != nil {
		slog.Error("DM key migration: failed to write completion marker; will retry next boot",
			"error", markErr)
	}
}

// runMessageBackfill runs the per-project message backfill with M-1' marker
// semantics (design §4.5).
//
// It enumerates projects exactly as the CLI does, skips those already in
// the marker's projects_done, runs the backfill for each remaining project,
// and persists per-project progress. A time budget bounds the total boot
// penalty; if exhausted mid-list, the next boot resumes at the first
// project not yet in projects_done.
//
// M-1' (design §4.3): a run-level failure (could not list, could not write,
// context cancelled) means the pass did not happen — log ERROR, do not
// record that project as done. Row-level refusals are terminal, correct,
// permanent outcomes — they do NOT block progress.
//
// M9 (design §4.8): a completed marker lacking the PermanentResidual key
// (pre-M9 format) is treated as incomplete and triggers a one-time re-run.
// The backfill is idempotent; the re-run writes the new marker format.
func runMessageBackfill(ctx context.Context, s store.Store) {
	// Fast path: already complete. O(1) with respect to data volume.
	marker, err := loadBackfillMarker(ctx, s)
	if err != nil {
		slog.Error("Message backfill: failed to check completion marker; will attempt migration",
			"error", err)
		// Fall through: attempting the migration is safer than skipping it.
	} else if marker.CompletedAt != nil {
		// M9: a completed marker lacking PermanentResidual was written
		// before M9 and does not have the measured permanent count. Treat
		// it as incomplete so the idempotent backfill re-runs once and
		// writes the new format. Reading absent-as-zero would make the
		// entire reachable population look actionable (design §4.8).
		if marker.PermanentResidual == nil {
			slog.Info("Message backfill: pre-M9 marker detected (no permanent_residual); re-running to upgrade marker format")
			// Clear CompletedAt so the pass runs. ProjectsDone was already
			// cleared by markBackfillComplete, so the full project list
			// will be re-enumerated.
			marker.CompletedAt = nil
		} else {
			slog.Debug("Message backfill: already complete, skipping")
			return
		}
	}

	slog.Info("Message backfill: starting")

	// Enumerate projects, exactly as cmd/server_backfill.go does.
	projectIDs, err := listAllProjectIDs(ctx, s)
	if err != nil {
		slog.Error("Message backfill: failed to list projects; will retry next boot",
			"error", err)
		return
	}

	if len(projectIDs) == 0 {
		slog.Info("Message backfill: no projects found; marking complete")
		if markErr := markBackfillComplete(ctx, s, marker); markErr != nil {
			slog.Error("Message backfill: failed to write completion marker",
				"error", markErr)
		}
		return
	}

	// Build the set of already-done projects for O(1) lookup.
	doneSet := make(map[string]bool, len(marker.ProjectsDone))
	for _, pid := range marker.ProjectsDone {
		doneSet[pid] = true
	}

	budget := defaultBackfillBudget
	deadline := time.Now().Add(budget)
	totalResiduals := marker.Residuals // carry forward from prior boots

	// M9: accumulate the measured permanent residual (design §4.8 second
	// correction). Reset to zero at the start of a pass beginning with
	// empty projects_done to prevent double-counting on a repeated pass.
	// Carry forward only within a pass (resumed from prior boot with
	// projects already done).
	permanentResidual := 0
	transientFailures := 0
	if len(marker.ProjectsDone) > 0 && marker.PermanentResidual != nil {
		permanentResidual = *marker.PermanentResidual
		transientFailures = marker.TransientFailures
	}

	for _, pid := range projectIDs {
		if doneSet[pid] {
			continue
		}

		// Check budget BEFORE starting the project, so we don't begin
		// work we can't finish within the budget.
		if time.Now().After(deadline) {
			slog.Error("Message backfill: time budget exhausted; will resume next boot",
				"budget", budget.String(),
				"projects_remaining", countRemaining(projectIDs, doneSet),
			)
			return
		}

		projectStart := time.Now()
		result, runErr := runBackfillForProject(ctx, s, pid)

		// --- M-1' decision point (per-project) ---

		if runErr != nil {
			// RUN-LEVEL failure: this project's pass did not complete.
			// Do NOT record it as done. Log and continue to the next
			// project — one project failing should not block others.
			slog.Error("Message backfill: project did not complete; will retry next boot",
				"project", pid,
				"error", runErr,
			)
			continue
		}

		// The pass completed. Row-level refusals are a terminal outcome.
		residuals := len(result.Errors)
		totalResiduals += residuals

		// M9: measure the permanent residual for this project (design §4.8
		// second correction). After the backfill pass, CountUnbackfilledMessages(pid)
		// gives the number of messages still unbackfilled — a pure measurement
		// with no tally subtraction. Transient failures are accumulated
		// separately and reported as their own WARN line.
		projectPermanent, countErr := measureProjectPermanentResidual(ctx, s, pid)
		if countErr != nil {
			slog.Error("Message backfill: failed to measure permanent residual for project; will retry next boot",
				"project", pid,
				"error", countErr,
			)
			// Cannot persist an accurate permanent count. Stop and retry
			// on the next boot to avoid persisting incorrect data.
			return
		}
		permanentResidual += projectPermanent

		// M9: tally transient failures separately. These are write and
		// resolution failures — retryable, reported as their own WARN line.
		// Never subtracted from the measurement (design §4.8 second correction).
		projectTransient := result.WriteFailures + result.ResolutionFailures
		transientFailures += projectTransient

		// M9 / DEF-114: log two identities so a reader can verify each
		// from the log without touching the database:
		//
		//   processed  = attributed + inferred + skipped + derive_failures
		//   row_errors = derive_failures + write_failures + resolution_failures
		//
		// These are DIFFERENT equations. The boot hook previously logged
		// row_errors (the second) in the field where a reader expects the
		// first (message disposition). That conflation hid the +4/-4
		// cancellation on gteam. Log both explicitly.
		deriveCount := sumDeriveFailures(result.DeriveFailures)
		logArgs := []any{
			"project", pid,
			"processed", result.TotalProcessed,
			"attributed", result.Attributed,
			"inferred", result.Inferred,
			"skipped", result.Skipped,
			"derive_failures", deriveCount,
			"write_failures", result.WriteFailures,
			"resolution_failures", result.ResolutionFailures,
			"row_errors", residuals,
			"permanent_residual", projectPermanent,
			"elapsed", time.Since(projectStart).Round(time.Millisecond).String(),
		}
		// Append per-cause derive failure counts.
		for cause, count := range result.DeriveFailures {
			logArgs = append(logArgs, "derive_"+cause, count)
		}
		slog.Info("Message backfill: project completed", logArgs...)

		if residuals > 0 {
			logBoundedErrors("Message backfill ("+pid+")", result.Errors, maxBootLogErrors)
		}

		// Record this project as done and persist immediately, so
		// progress survives a crash between projects.
		marker.ProjectsDone = append(marker.ProjectsDone, pid)
		marker.Residuals = totalResiduals
		marker.PermanentResidual = &permanentResidual
		marker.TransientFailures = transientFailures
		doneSet[pid] = true

		if saveErr := saveBackfillProgress(ctx, s, marker); saveErr != nil {
			slog.Error("Message backfill: failed to persist per-project progress; will retry next boot",
				"project", pid,
				"error", saveErr,
			)
			// Don't continue — if we can't persist progress, we risk
			// re-processing projects on the next boot. Stop and retry.
			return
		}
	}

	// Check whether every enumerated project is in projects_done.
	// Only set completed_at when that is true (design §4.5).
	remaining := countRemaining(projectIDs, doneSet)
	if remaining > 0 {
		slog.Warn("Message backfill: not all projects completed; will retry incomplete projects next boot",
			"remaining", remaining,
			"done", len(doneSet),
		)
		return
	}

	// Set completed_at, clear projects_done (bounded growth per design §4.5).
	if markErr := markBackfillComplete(ctx, s, marker); markErr != nil {
		slog.Error("Message backfill: failed to write completion marker; will retry next boot",
			"error", markErr)
		return
	}

	slog.Info("Message backfill: all projects complete",
		"projects", len(projectIDs),
		"total_residuals", totalResiduals,
		"permanent_residual", permanentResidual,
		"transient_failures", transientFailures,
	)
}

// measureProjectPermanentResidual measures the number of messages that
// remain unbackfilled for a project after its backfill pass completes.
//
// Design §4.8 second correction: the permanent count is a pure measurement
// — CountUnbackfilledMessages(pid) taken after the pass — with NO tally
// subtraction. Transient failures (write/resolution) are reported as their
// own separate count, never subtracted from the measurement. This prevents
// the tally/measurement mixing that caused the off-by-24 in the first
// correction: rows that are both errored and stamped are inside the
// measurement and never cause a gap.
//
// The measured term is drawn from the same population the global live counter
// measures (CountUnbackfilledMessages("")), so at steady state the two agree
// by construction and actionable reaches zero exactly — not via the clamp.
func measureProjectPermanentResidual(ctx context.Context, s store.Store, pid string) (int, error) {
	stillUnbackfilled, err := s.CountUnbackfilledMessages(ctx, pid)
	if err != nil {
		return 0, fmt.Errorf("counting unbackfilled messages for project %s: %w", pid, err)
	}
	return stillUnbackfilled, nil
}

// sumDeriveFailures totals the per-cause derive failure counts.
func sumDeriveFailures(m map[string]int) int {
	total := 0
	for _, v := range m {
		total += v
	}
	return total
}

// listAllProjectIDs enumerates all projects, paginating as the CLI does.
func listAllProjectIDs(ctx context.Context, s store.Store) ([]string, error) {
	var projectIDs []string
	cursor := ""
	for {
		projects, err := s.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: 500, Cursor: cursor})
		if err != nil {
			return nil, fmt.Errorf("listing projects: %w", err)
		}
		for _, p := range projects.Items {
			projectIDs = append(projectIDs, p.ID)
		}
		if projects.NextCursor == "" {
			break
		}
		cursor = projects.NextCursor
	}
	return projectIDs, nil
}

// runBackfillForProject runs the backfill for a single project in execute
// mode. Extracted for testability.
func runBackfillForProject(ctx context.Context, s store.Store, projectID string) (*messaging.BackfillResult, error) {
	svc := messaging.NewBackfillService(s, s, s)
	return svc.Run(ctx, messaging.BackfillConfig{
		ProjectID: projectID,
		DryRun:    false,
	})
}

// markBackfillComplete sets completed_at and clears projects_done to bound
// the marker's growth (design §4.5). The residual count and the permanent
// residual (M9) are preserved.
func markBackfillComplete(ctx context.Context, s store.Store, m backfillMarker) error {
	now := time.Now().UTC()
	m.CompletedAt = &now
	m.ProjectsDone = nil // clear for bounded growth
	// m.Residuals and m.PermanentResidual are preserved across completion.
	return saveBackfillProgress(ctx, s, m)
}

// countRemaining returns the number of project IDs not in the done set.
func countRemaining(projectIDs []string, doneSet map[string]bool) int {
	n := 0
	for _, pid := range projectIDs {
		if !doneSet[pid] {
			n++
		}
	}
	return n
}

// logBoundedErrors logs a sample of per-row errors, capped at limit entries,
// with the total count. This prevents an unbounded error list from turning a
// bad migration into a disk-space incident (design §4.3).
func logBoundedErrors(prefix string, errors []string, limit int) {
	total := len(errors)
	shown := total
	if shown > limit {
		shown = limit
	}

	for i := 0; i < shown; i++ {
		slog.Warn(fmt.Sprintf("%s: row error", prefix),
			"index", i+1,
			"total", total,
			"error", errors[i],
		)
	}

	if total > limit {
		slog.Warn(fmt.Sprintf("%s: %d more row errors not shown", prefix, total-limit),
			"shown", limit,
			"total", total,
		)
	}
}

// computeResidualBuckets is the single source of truth for the three-bucket
// residual arithmetic (design §4.8). Production logs what this returns;
// tests call this function — there is one formula, not two.
//
// The arithmetic:
//
//	reachable        = total - unreachable
//	actionablePreClamp = reachable - permanent
//	actionable       = max(0, actionablePreClamp)
//
// The clamp on actionable is a drift guard, not the mechanism that produces
// zero. At steady state actionable reaches zero exactly because the measured
// permanent term is drawn from the same population the live counter measures
// (design §4.8 correction).
func computeResidualBuckets(total, unreachable, permanent int) (reachable, actionablePreClamp, actionable int) {
	reachable = total - unreachable
	actionablePreClamp = reachable - permanent
	actionable = actionablePreClamp
	if actionable < 0 {
		actionable = 0
	}
	return reachable, actionablePreClamp, actionable
}

// reportResidualUnattributed splits the residual unattributed-message count
// into three buckets (design §4.6, §4.8):
//
//   - unreachable (INFO): messages whose project_id references a hard-deleted
//     project. Stable and permanent (DEF-111).
//   - permanent (INFO): messages in listed projects that are permanently
//     unbackfillable — derive refusals and intentionally skipped messages.
//     Measured during the backfill pass and persisted in the marker (M9).
//   - actionable (WARN): messages that re-running the backfill could fix.
//     WARN fires only when this count is non-zero.
//
// All arithmetic is delegated to computeResidualBuckets so that production
// and tests share one formula. The report logs what the function returns.
//
// CONSEQUENCE: the DEF-112 drift concern is now live. The counter's
// predicate ("project_id NOT IN projects") and the backfill's skip predicate
// must share one expression. This makes M7 required, not optional.
//
// GATE (M7, DEF-112): TestReachableCountConsistency_DEF112 enforces the
// invariant that these two predicates agree.
func reportResidualUnattributed(ctx context.Context, s store.Store) {
	tCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	totalUnbackfilled, err := s.CountUnbackfilledMessages(tCtx, "")
	if err != nil {
		slog.Warn("Failed to count unbackfilled messages for residual report", "error", err)
		return
	}

	if totalUnbackfilled == 0 {
		// No unattributed messages at all — nothing to report.
		return
	}

	unreachable, err := s.CountUnreachableUnbackfilledMessages(tCtx)
	if err != nil {
		slog.Warn("Failed to count unreachable unbackfilled messages", "error", err)
		// Fall through with unreachable=0 so the total is still reported
		// as reachable. This is the conservative direction: it may
		// over-warn but will never suppress a real problem.
		unreachable = 0
	}

	// M9: load the backfill marker to get the persisted permanent residual.
	marker, markerErr := loadBackfillMarker(tCtx, s)
	permanent := 0
	if markerErr != nil {
		slog.Warn("Failed to load backfill marker for residual report; treating permanent as 0",
			"error", markerErr)
		// Fall through with permanent=0: conservative direction (may over-warn).
	} else if marker.PermanentResidual != nil {
		permanent = *marker.PermanentResidual
	}

	// Single source of truth for the three-bucket arithmetic.
	_, _, actionable := computeResidualBuckets(totalUnbackfilled, unreachable, permanent)

	// INFO always: report the stable unreachable count.
	if unreachable > 0 {
		slog.Info("Message attribution complete",
			"unreachable", unreachable,
			"detail", "unreachable messages reference hard-deleted projects and cannot be attributed by per-project backfill (DEF-111); this count is expected to be stable",
		)
	}

	// INFO for permanent count (stable, no operator action possible).
	if permanent > 0 {
		slog.Info("Permanently unattributable messages in listed projects",
			"permanent", permanent,
			"detail", "derive refusals and intentionally skipped messages; no operator action can attribute these",
		)
	}

	// WARN only when actionable > 0: these are messages that arrived after
	// the backfill pass completed — new drift that a re-run could fix.
	if actionable > 0 {
		slog.Warn("Messages remain unattributed in listed projects",
			"count", actionable,
		)
	}

	// M9: report post-derivation failures (write + resolution) as a
	// separate WARN. These are NOT transient — they include deterministic
	// authorization refusals (e.g. participant validation on direct
	// conversations). They are also NOT unattributed messages — the
	// associated messages were stamped successfully; only a secondary
	// operation (e.g. AddParticipant) was refused.
	//
	// No remedy string: advertising "scion server backfill" for a
	// deterministic refusal is DEF-111's exact shape (a warning whose
	// advertised remedy cannot reduce it).
	postDerive := 0
	if marker.TransientFailures > 0 {
		postDerive = marker.TransientFailures
	}
	if postDerive > 0 {
		slog.Warn("Post-derivation failures during last backfill pass",
			"count", postDerive,
			"detail", "write or resolution failures after key derivation succeeded; not retried automatically; may indicate a data or authorization anomaly; see per-cause breakdown in backfill logs",
		)
	}
}
