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

package hub

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/maintenance"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Settled-decision retention maintenance operations (design-8984 §3.3):
// decision-retention-preview walks what the sweep would delete without deleting
// anything, and decision-retention-run runs the sweep once by hand. Both are
// routine maintenance operations (hub admins only via hub.maintenance.execute),
// answer 404 while hub.decision_retention is off, hold LockDecisionRetention so
// they never race the background sweep, and write decision_retention_runs rows
// with policy, actor, and per-project counts (never IDs or content).

const (
	// decisionRetentionOpMaxDuration bounds a preview or manual run.
	decisionRetentionOpMaxDuration = 24 * time.Hour
	// decisionRetentionCodeDisabled refuses a manual run while retention is
	// disabled (legal hold) or has no active window (HTTP 409).
	decisionRetentionCodeDisabled = "decision_retention_disabled"
)

// decisionRetentionLockPoll is how often a preview or manual run retries the
// in-process guard and LockDecisionRetention while another run or tick holds
// them (a var so tests can shorten it).
var decisionRetentionLockPoll = 2 * time.Second

type decisionRetentionPreviewResult struct {
	ByProject       map[string]store.DecisionRetentionCounts
	Pages           int
	EligibleMsgs    int
	SkippedMsgs     int
	ReachedCutoff   bool
	BudgetExhausted bool
}

// Preview walks the exact same settled candidates and whole-message eligibility
// rules as the sweep under p, starting from the beginning and deleting nothing.
// It deduplicates message IDs across pages so a multi-decision prompt straddling
// a page boundary is counted at most once, matching a manual run on an unchanged
// database.
func (sw *decisionRetentionSweeper) Preview(
	ctx context.Context,
	p decisionRetentionPolicy,
	budgetEnd time.Time,
) (decisionRetentionPreviewResult, error) {
	res := decisionRetentionPreviewResult{
		ByProject: map[string]store.DecisionRetentionCounts{},
	}
	if !p.Active || p.GlobalCutoff.IsZero() {
		res.ReachedCutoff = true
		return res, nil
	}
	cfg := p.Config
	pageSize := cfg.PageSize
	if pageSize < decisionRetentionMinPage {
		pageSize = opsettings.DecisionRetentionDefaultPageSize
	}
	globalCutoff := p.GlobalCutoff.UTC()
	var pos store.DecisionRetentionPosition
	seenMsg := make(map[string]bool)

	for {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if !sw.now().Before(budgetEnd) {
			res.BudgetExhausted = true
			return res, nil
		}

		pctx, cancel := context.WithTimeout(ctx, decisionRetentionPageTimeout)
		cands, err := sw.store.SelectDecisionRetentionCandidates(pctx, globalCutoff, pos, pageSize)
		if err != nil {
			cancel()
			return res, err
		}
		if len(cands) == 0 {
			cancel()
			res.ReachedCutoff = true
			return res, nil
		}

		msgIDs := make([]string, 0, len(cands))
		cutoffByProject := make(map[string]time.Time)
		for _, c := range cands {
			cutoff, ok := p.CutoffForProject(c.ProjectID)
			if !ok || !c.CreatedAt.Before(cutoff) {
				continue
			}
			if !seenMsg[c.MessageID] {
				seenMsg[c.MessageID] = true
				msgIDs = append(msgIDs, c.MessageID)
			}
			cutoffByProject[c.ProjectID] = cutoff
		}

		var pr store.DecisionRetentionPurgeResult
		var tx time.Duration
		if len(msgIDs) > 0 {
			t0 := sw.now()
			pr, err = sw.store.PreviewDecisionMessages(pctx, msgIDs, cutoffByProject)
			tx = sw.now().Sub(t0)
		}
		cancel()
		if err != nil {
			return res, err
		}

		last := cands[len(cands)-1]
		pos = store.DecisionRetentionPosition{CreatedAt: last.CreatedAt, ID: last.ID}
		res.Pages++
		res.EligibleMsgs += pr.PurgedMessages
		res.SkippedMsgs += pr.SkippedMessages
		for pid, c := range pr.ByProject {
			acc := res.ByProject[pid]
			acc.Add(c)
			res.ByProject[pid] = acc
		}

		if len(cands) < pageSize {
			res.ReachedCutoff = true
			return res, nil
		}

		pageSize = nextDecisionRetentionPage(pageSize, cfg.PageSize, tx)
		if sw.isSQLite {
			if !sw.now().Add(decisionRetentionSQLiteYield).Before(budgetEnd) {
				res.BudgetExhausted = true
				return res, nil
			}
			if err := sw.sleep(ctx, decisionRetentionSQLiteYield); err != nil {
				return res, err
			}
		}
	}
}

// RunManual walks from the start of the candidate scan under p without reading
// or modifying the sweep cursor, purging eligible messages and updating run.
func (sw *decisionRetentionSweeper) RunManual(
	ctx context.Context,
	p decisionRetentionPolicy,
	run *store.DecisionRetentionRun,
	budgetEnd time.Time,
) (decisionRetentionTickResult, error) {
	var res decisionRetentionTickResult
	if !p.Active || p.GlobalCutoff.IsZero() {
		return res, errors.New("decision retention run: policy is not active")
	}
	res.Ran = true
	if run.Counts == nil {
		run.Counts = map[string]store.DecisionRetentionCounts{}
	}
	var cursor store.DecisionRetentionCursor
	var cursorDirty bool
	walkErr := sw.walk(ctx, &p, &cursor, run, &res, budgetEnd, &cursorDirty)

	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), decisionRetentionFinalWriteTimeout)
	defer cancel()

	now := sw.now().UTC()
	run.Policy = decisionRetentionRunPolicy(p)
	run.FinishedAt = &now
	switch {
	case walkErr != nil:
		run.Status = store.DecisionRetentionStatusFailed
		run.Error = walkErr.Error()
	case res.ReachedCutoff:
		run.Status = store.DecisionRetentionStatusOK
		run.Error = ""
	default:
		run.Status = store.DecisionRetentionStatusPartial
		run.Error = ""
	}
	if err := sw.store.SaveDecisionRetentionRun(wctx, run); err != nil && walkErr == nil {
		walkErr = err
	}
	if _, err := sw.store.PruneDecisionRetentionRuns(wctx, now.Add(-decisionRetentionRunKeep)); err != nil && walkErr == nil {
		walkErr = err
	}
	return res, walkErr
}

// acquireDecisionRetention waits until this process's tick guard and
// LockDecisionRetention are both free, acquires both, and returns a release
// function. waiting is called at most once if it has to wait.
func (s *Server) acquireDecisionRetention(ctx context.Context, waiting func()) (func(), error) {
	notified := false
	for {
		if s.decisionRetention.running.CompareAndSwap(false, true) {
			release, acquired, err := s.tryDecisionRetentionLock(ctx)
			if err != nil {
				s.decisionRetention.running.Store(false)
				return nil, err
			}
			if acquired {
				return func() {
					release()
					s.decisionRetention.running.Store(false)
				}, nil
			}
			s.decisionRetention.running.Store(false)
		}
		if !notified && waiting != nil {
			waiting()
			notified = true
		}
		if err := sleepCtx(ctx, decisionRetentionLockPoll); err != nil {
			return nil, err
		}
	}
}

func decisionRetentionOpBudgetEnd(start time.Time, remaining time.Duration, hasDeadline bool) time.Time {
	end := start.Add(decisionRetentionOpMaxDuration)
	if hasDeadline {
		if byDeadline := start.Add(remaining - decisionRetentionPageTimeout - decisionRetentionDeadlineMargin); byDeadline.Before(end) {
			end = byDeadline
		}
	}
	return end
}

func writeDecisionRetentionPolicy(logger io.Writer, verb string, p decisionRetentionPolicy) {
	window := "no hub default window"
	if p.Config.HasWindow() {
		window = fmt.Sprintf("hub default %d days", p.Config.RetentionDays)
	}
	_, _ = fmt.Fprintf(logger, "%s: %s; project overrides: %d", verb, window, len(p.Config.ProjectOverrides))
	if !p.GlobalCutoff.IsZero() {
		_, _ = fmt.Fprintf(logger, "; global cutoff %s", p.GlobalCutoff.UTC().Format(time.RFC3339))
	}
	_, _ = fmt.Fprintln(logger)
}

func writeDecisionRetentionCounts(logger io.Writer, verb string, byProject map[string]store.DecisionRetentionCounts) {
	projects := make([]string, 0, len(byProject))
	for pid := range byProject {
		projects = append(projects, pid)
	}
	sort.Strings(projects)
	var total store.DecisionRetentionCounts
	countedProjects := 0
	for _, pid := range projects {
		c := byProject[pid]
		total.Add(c)
		if c.Decisions == 0 && c.Answers == 0 {
			continue
		}
		countedProjects++
		_, _ = fmt.Fprintf(logger, "Project %s: %s %d decisions, %d answers\n", pid, verb, c.Decisions, c.Answers)
	}
	_, _ = fmt.Fprintf(logger, "Total: %s %d decisions, %d answers across %d projects\n",
		verb, total.Decisions, total.Answers, countedProjects)
}

func decisionRetentionOpResultMap(run *store.DecisionRetentionRun) map[string]any {
	if run == nil {
		return nil
	}
	var total store.DecisionRetentionCounts
	projects := 0
	counts := make(map[string]store.DecisionRetentionCounts)
	for pid, c := range run.Counts {
		total.Add(c)
		if c.Decisions > 0 || c.Answers > 0 {
			projects++
			counts[pid] = c
		}
	}
	return map[string]any{
		"runId":    run.ID,
		"trigger":  run.Trigger,
		"dryRun":   run.DryRun,
		"status":   run.Status,
		"projects": projects,
		"totals":   total,
		"counts":   counts,
	}
}

func (s *Server) requireDecisionRetentionExperiment() error {
	if !s.experimentEnabled(experiments.DecisionRetention) {
		return errors.New("decision retention is not available: experiment hub.decision_retention is off")
	}
	return nil
}

// runDecisionRetentionPreview runs a dry-run preview under the saved settings
// (treating enabled as true so admins can preview impact before enabling),
// holds LockDecisionRetention so it does not race a sweep, writes a preview
// run row with dry_run=true and actor, and deletes nothing.
func (s *Server) runDecisionRetentionPreview(ctx context.Context, logger io.Writer, actor string) (*store.DecisionRetentionRun, error) {
	st := s.decisionRetentionStore()
	if st == nil {
		return nil, errors.New("decision retention preview: decision retention store is not available")
	}
	release, err := s.acquireDecisionRetention(ctx, func() {
		_, _ = fmt.Fprintln(logger, "Waiting for the running decision retention sweep to finish")
	})
	if err != nil {
		return nil, fmt.Errorf("decision retention preview: %w", err)
	}
	defer release()

	cfg, err := s.decisionRetentionConfigForJob(ctx)
	if err != nil {
		return nil, fmt.Errorf("decision retention preview: %w", err)
	}
	sw := newDecisionRetentionSweeper(st, s.isSQLiteStore())
	sw.policy = s.decisionRetentionPolicyAt
	start := sw.now()
	// Preview evaluates the saved hub default and per-project overrides as if
	// enabled=true so admins can inspect counts before enabling the sweep.
	previewCfg := cfg.Clone()
	previewCfg.Enabled = true
	p := newDecisionRetentionPolicy(true, previewCfg, start)

	run := &store.DecisionRetentionRun{
		Trigger:   store.DecisionRetentionTriggerPreview,
		DryRun:    true,
		StartedAt: start.UTC(),
		Status:    store.DecisionRetentionStatusRunning,
		Actor:     actor,
		Policy:    decisionRetentionRunPolicy(p),
		Counts:    map[string]store.DecisionRetentionCounts{},
	}
	if err := st.SaveDecisionRetentionRun(ctx, run); err != nil {
		return nil, fmt.Errorf("decision retention preview: %w", err)
	}
	writeDecisionRetentionPolicy(logger, "Previewing", p)

	remaining, hasDeadline := chatRetentionTimeLeft(ctx)
	budgetEnd := decisionRetentionOpBudgetEnd(start, remaining, hasDeadline)
	res, err := sw.Preview(ctx, p, budgetEnd)

	now := sw.now().UTC()
	run.FinishedAt = &now
	run.Counts = res.ByProject
	switch {
	case err != nil:
		run.Status = store.DecisionRetentionStatusFailed
		run.Error = err.Error()
	case res.ReachedCutoff:
		run.Status = store.DecisionRetentionStatusOK
		run.Error = ""
	default:
		run.Status = store.DecisionRetentionStatusPartial
		run.Error = ""
	}
	fctx, fcancel := context.WithTimeout(context.WithoutCancel(ctx), decisionRetentionFinalWriteTimeout)
	defer fcancel()
	if serr := st.SaveDecisionRetentionRun(fctx, run); serr != nil && err == nil {
		err = serr
	}
	writeDecisionRetentionCounts(logger, "would delete", res.ByProject)
	_, _ = fmt.Fprintf(logger, "Pages: %d, status: %s\n", res.Pages, run.Status)
	if err != nil {
		return run, fmt.Errorf("decision retention preview: %w", err)
	}
	return run, nil
}

// runDecisionRetentionManual runs the retention sweep once from the beginning
// under LockDecisionRetention, writing a manual run row with actor. Refuses
// and deletes nothing while enabled=false (legal hold) or no effective window
// exists.
func (s *Server) runDecisionRetentionManual(ctx context.Context, logger io.Writer, actor string) (*store.DecisionRetentionRun, decisionRetentionTickResult, error) {
	var res decisionRetentionTickResult
	st := s.decisionRetentionStore()
	if st == nil {
		return nil, res, errors.New("decision retention run: decision retention store is not available")
	}
	release, err := s.acquireDecisionRetention(ctx, func() {
		_, _ = fmt.Fprintln(logger, "Waiting for the running decision retention sweep to finish")
	})
	if err != nil {
		return nil, res, fmt.Errorf("decision retention run: %w", err)
	}
	defer release()

	sw := newDecisionRetentionSweeper(st, s.isSQLiteStore())
	sw.policy = s.decisionRetentionPolicyAt
	start := sw.now()
	p, err := s.decisionRetentionPolicyAt(ctx, start)
	if err != nil {
		return nil, res, fmt.Errorf("decision retention run: %w", err)
	}
	if !p.Active {
		return nil, res, errors.New("decision retention run: decision retention is not enabled or has no active retention window")
	}

	run := &store.DecisionRetentionRun{
		Trigger:   store.DecisionRetentionTriggerManual,
		DryRun:    false,
		StartedAt: start.UTC(),
		Status:    store.DecisionRetentionStatusRunning,
		Actor:     actor,
		Policy:    decisionRetentionRunPolicy(p),
		Counts:    map[string]store.DecisionRetentionCounts{},
	}
	if err := st.SaveDecisionRetentionRun(ctx, run); err != nil {
		return nil, res, fmt.Errorf("decision retention run: %w", err)
	}
	writeDecisionRetentionPolicy(logger, "Running", p)

	remaining, hasDeadline := chatRetentionTimeLeft(ctx)
	budgetEnd := decisionRetentionOpBudgetEnd(start, remaining, hasDeadline)
	res, err = sw.RunManual(ctx, p, run, budgetEnd)

	writeDecisionRetentionCounts(logger, "deleted", run.Counts)
	_, _ = fmt.Fprintf(logger, "Pages: %d, status: %s\n", res.Pages, run.Status)
	if err != nil {
		return run, res, fmt.Errorf("decision retention run: %w", err)
	}
	return run, res, nil
}

// DecisionRetentionPreviewExecutor runs decision-retention-preview.
type DecisionRetentionPreviewExecutor struct {
	srv   *Server
	actor string

	mu     sync.Mutex
	result map[string]any
}

func (e *DecisionRetentionPreviewExecutor) setMaintenanceActor(userID string) { e.actor = userID }

// RunResult implements maintenance.ResultReporter.
func (e *DecisionRetentionPreviewExecutor) RunResult() map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.result
}

// Run implements MaintenanceExecutor.
func (e *DecisionRetentionPreviewExecutor) Run(ctx context.Context, logger io.Writer, _ map[string]string) error {
	if err := e.srv.requireDecisionRetentionExperiment(); err != nil {
		return err
	}
	run, err := e.srv.runDecisionRetentionPreview(ctx, logger, e.actor)
	if run != nil {
		e.mu.Lock()
		e.result = decisionRetentionOpResultMap(run)
		e.mu.Unlock()
	}
	return err
}

// DecisionRetentionRunExecutor runs decision-retention-run. It takes no
// parameters and is refused (409 decision_retention_disabled) while retention
// is disabled (legal hold) or no effective window exists.
type DecisionRetentionRunExecutor struct {
	srv   *Server
	actor string

	mu     sync.Mutex
	result map[string]any
}

func (e *DecisionRetentionRunExecutor) setMaintenanceActor(userID string) { e.actor = userID }

// RunResult implements maintenance.ResultReporter.
func (e *DecisionRetentionRunExecutor) RunResult() map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.result
}

// ValidateParams implements maintenance.ParamValidator.
func (e *DecisionRetentionRunExecutor) ValidateParams(map[string]string) error {
	p, err := e.srv.decisionRetentionPolicyAt(context.Background(), time.Now())
	if err != nil {
		return err
	}
	if !p.Active {
		return &maintenance.RequestError{
			Code:    decisionRetentionCodeDisabled,
			Message: "Decision retention is not enabled or has no active retention window: enable it and configure a window in Admin > Server Config first",
		}
	}
	return nil
}

// Run implements MaintenanceExecutor.
func (e *DecisionRetentionRunExecutor) Run(ctx context.Context, logger io.Writer, _ map[string]string) error {
	if err := e.srv.requireDecisionRetentionExperiment(); err != nil {
		return err
	}
	run, _, err := e.srv.runDecisionRetentionManual(ctx, logger, e.actor)
	if run != nil {
		e.mu.Lock()
		e.result = decisionRetentionOpResultMap(run)
		e.mu.Unlock()
	}
	return err
}
