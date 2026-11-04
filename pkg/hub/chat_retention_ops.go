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
	"log/slog"
	"sort"
	"strconv"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/maintenance"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The chat retention maintenance operations (design chat message retention
// §6.5): chat-retention-preview walks what the sweep would delete without
// deleting anything, and chat-retention-run runs the sweep once by hand.
// Both are routine maintenance operations, so only hub administrators run
// them (hub.maintenance.execute), and both answer 404 while
// hub.chat_retention is off. Each records a chat_retention_runs row with
// the policy, per-project counts and the administrator who started it.

const (
	// chatRetentionOpMaxDuration bounds a preview or a manual run (the
	// maintenance run timeout); the walk stops early enough to record
	// what it did.
	chatRetentionOpMaxDuration = 24 * time.Hour
	// chatRetentionParamDays is the preview's optional parameter: the
	// window to preview instead of the saved one.
	chatRetentionParamDays = "messages_retention_days"
	// chatRetentionCodeDisabled refuses a manual run while the sweep is
	// not enabled (409).
	chatRetentionCodeDisabled = "chat_retention_disabled"
)

// chatRetentionLockPoll is how often a manual run retries the tick guard
// and the advisory lock while a sweep tick holds them (a var so tests can
// shorten it).
var chatRetentionLockPoll = 2 * time.Second

// maintenanceActorSetter is implemented by executors that record who
// started them; executeOperation passes the caller's user ID.
type maintenanceActorSetter interface {
	setMaintenanceActor(userID string)
}

// chatRetentionPreviewResult is what a preview found.
type chatRetentionPreviewResult struct {
	// ByProject counts per project the messages the main window (Main)
	// and the deleted-conversation phase (DeletedConv) would delete.
	ByProject map[string]store.ChatRetentionCounts
	// Selects is the number of selection statements run.
	Selects int
	// MainDone and DeletedDone are true when the phase's walk finished.
	MainDone    bool
	DeletedDone bool
}

// chatRetentionPreview is the state of one preview walk.
type chatRetentionPreview struct {
	sw    *chatRetentionSweeper
	p     chatRetentionPolicy
	res   *chatRetentionPreviewResult
	chunk int
	// owed is true after a selection, until the pause has been slept.
	owed bool
	// convs holds the conversations, and threads the legacy topics, whose
	// messages the deleted-conversation phase counted. The main walk
	// skips them: the sweep deletes them in that phase first.
	convs   map[string]bool
	threads map[string]bool
}

// Preview walks the selections a sweep under p would delete, in the same
// order, chunk sizes and pause, and deletes nothing. The
// deleted-conversation phase runs first, as in a sweep, and its messages
// are not counted again by the main window. The main walk starts at the
// beginning, as a manual run's does, so on an unchanged database the counts
// equal those of a manual run.
func (sw *chatRetentionSweeper) Preview(ctx context.Context, p chatRetentionPolicy) (chatRetentionPreviewResult, error) {
	res := chatRetentionPreviewResult{ByProject: map[string]store.ChatRetentionCounts{}}
	if !p.purges() {
		return res, nil
	}
	pv := &chatRetentionPreview{sw: sw, p: p, res: &res, chunk: p.Config.ChunkSize,
		convs: map[string]bool{}, threads: map[string]bool{}}
	if err := pv.deleted(ctx); err != nil {
		return res, err
	}
	res.DeletedDone = true
	if !p.MainCutoff.IsZero() {
		if err := pv.main(ctx); err != nil {
			return res, err
		}
	}
	res.MainDone = true
	return res, nil
}

// selectPage sleeps the pause owed by the last selection, runs fetch under
// the chunk timeout with the current chunk size, and adapts the chunk size
// to the selection time. It returns the rows and the limit used.
func (pv *chatRetentionPreview) selectPage(ctx context.Context, fetch func(cctx context.Context, limit int) ([]store.ChatRetentionCandidate, error)) ([]store.ChatRetentionCandidate, int, error) {
	if pv.owed {
		if err := pv.sw.sleep(ctx, pv.p.Config.Pause()); err != nil {
			return nil, 0, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	limit := pv.chunk
	var rows []store.ChatRetentionCandidate
	t0 := pv.sw.now()
	err := chatRetentionWithTimeout(ctx, func(cctx context.Context) (err error) {
		rows, err = fetch(cctx, limit)
		return err
	})
	if err != nil {
		return nil, limit, err
	}
	pv.owed = true
	pv.res.Selects++
	pv.chunk = nextChatRetentionChunk(pv.chunk, pv.p.Config.ChunkSize, pv.sw.now().Sub(t0))
	return rows, limit, nil
}

// count adds one message to its project's counter.
func (pv *chatRetentionPreview) count(project string, deletedConv bool) {
	c := pv.res.ByProject[project]
	if deletedConv {
		c.DeletedConv++
	} else {
		c.Main++
	}
	pv.res.ByProject[project] = c
}

// deleted counts the deleted-conversation phase: the candidates of
// walkDeleted, each paged in (created, id) order.
func (pv *chatRetentionPreview) deleted(ctx context.Context) error {
	st := pv.sw.store
	after := ""
	for {
		var ids []string
		err := chatRetentionWithTimeout(ctx, func(cctx context.Context) (err error) {
			ids, err = st.ListChatRetentionDeletedConversations(cctx, pv.p.GraceCutoff, after, chatRetentionCandidatePage)
			return err
		})
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := pv.conversation(ctx, id); err != nil {
				return err
			}
		}
		if len(ids) < chatRetentionCandidatePage {
			break
		}
		after = ids[len(ids)-1]
	}

	after = ""
	for {
		var topics []store.ChatRetentionDeletedTopic
		err := chatRetentionWithTimeout(ctx, func(cctx context.Context) (err error) {
			topics, err = st.ListChatRetentionDeletedTopics(cctx, pv.p.GraceCutoff, after, chatRetentionCandidatePage)
			return err
		})
		if err != nil {
			return err
		}
		for _, t := range topics {
			if t.ConversationID != "" {
				if err := pv.conversation(ctx, t.ConversationID); err != nil {
					return err
				}
			}
			if err := pv.legacyTopic(ctx, t.ID); err != nil {
				return err
			}
		}
		if len(topics) < chatRetentionCandidatePage {
			break
		}
		after = topics[len(topics)-1].ID
	}
	return nil
}

// conversation counts the messages of one deleted conversation, once.
func (pv *chatRetentionPreview) conversation(ctx context.Context, id string) error {
	if pv.convs[id] {
		return nil
	}
	pv.convs[id] = true
	return pv.walkConversation(ctx, id, "")
}

// legacyTopic counts a deleted topic's messages that have no
// conversation, once.
func (pv *chatRetentionPreview) legacyTopic(ctx context.Context, id string) error {
	if pv.threads[id] {
		return nil
	}
	pv.threads[id] = true
	return pv.walkConversation(ctx, "", id)
}

func (pv *chatRetentionPreview) walkConversation(ctx context.Context, conversationID, threadID string) error {
	var pos store.ChatRetentionPosition
	for {
		rows, limit, err := pv.selectPage(ctx, func(cctx context.Context, limit int) ([]store.ChatRetentionCandidate, error) {
			return pv.sw.store.SelectChatRetentionConversationPage(cctx, conversationID, threadID, pos, limit)
		})
		if err != nil {
			return err
		}
		for _, c := range rows {
			pv.count(c.ProjectID, true)
		}
		if len(rows) < limit {
			return nil
		}
		last := rows[len(rows)-1]
		pos = store.ChatRetentionPosition{Created: last.Created, ID: last.ID}
	}
}

// main counts the main window: the created-index walk from the start,
// without the messages the deleted-conversation phase counted.
func (pv *chatRetentionPreview) main(ctx context.Context) error {
	var pos store.ChatRetentionPosition
	for {
		rows, limit, err := pv.selectPage(ctx, func(cctx context.Context, limit int) ([]store.ChatRetentionCandidate, error) {
			return pv.sw.store.SelectChatRetentionCandidates(cctx, pv.p.MainCutoff, pos, limit)
		})
		if err != nil {
			return err
		}
		for _, c := range rows {
			if c.ConversationID != "" && pv.convs[c.ConversationID] {
				continue
			}
			if c.ConversationID == "" && c.ThreadID != "" && pv.threads[c.ThreadID] {
				continue
			}
			pv.count(c.ProjectID, false)
		}
		if len(rows) < limit {
			return nil
		}
		last := rows[len(rows)-1]
		pos = store.ChatRetentionPosition{Created: last.Created, ID: last.ID}
	}
}

// chatRetentionPreviewPolicy is the policy a preview walks: the saved
// section as if enabled, with the window replaced by days when days > 0.
func chatRetentionPreviewPolicy(cfg opsettings.ChatRetentionConfig, days int, now time.Time) chatRetentionPolicy {
	cfg.Enabled = true
	if days > 0 {
		cfg.MessagesRetentionDays = days
	}
	return newChatRetentionPolicy(true, cfg, now)
}

// runChatRetentionPreview runs a preview under the saved settings (with
// the window replaced by days when days > 0) and records it as a dry-run
// preview row with actor. The row is written when the preview starts, so
// the run history shows it while it runs, and again when it ends.
func (s *Server) runChatRetentionPreview(ctx context.Context, logger io.Writer, days int, actor string) (*store.ChatRetentionRun, error) {
	st := s.chatRetentionStore()
	if st == nil {
		return nil, errors.New("chat retention preview: the chat retention store is not available (native chat is disabled or its store is unavailable)")
	}
	sw := newChatRetentionSweeper(st)
	start := sw.now()
	p := chatRetentionPreviewPolicy(s.chatRetentionConfig(), days, start)
	run := &store.ChatRetentionRun{
		Trigger:   store.ChatRetentionTriggerPreview,
		DryRun:    true,
		StartedAt: start.UTC(),
		Status:    store.ChatRetentionStatusRunning,
		Actor:     actor,
		Policy:    chatRetentionRunPolicy(p),
		Counts:    map[string]store.ChatRetentionCounts{},
	}
	if err := st.SaveChatRetentionRun(ctx, run); err != nil {
		return nil, fmt.Errorf("chat retention preview: %w", err)
	}
	writeChatRetentionPolicy(logger, "Previewing", p)

	remaining, hasDeadline := chatRetentionTimeLeft(ctx)
	wctx, cancel := chatRetentionWalkContext(ctx, start, remaining, hasDeadline)
	res, err := sw.Preview(wctx, p)
	// Read before cancel(): whether the walk's own deadline passed. A
	// selection that times out on its chunk timeout (a child of wctx) also
	// wraps context.DeadlineExceeded but leaves wctx live; it is a failure.
	walkExpired := errors.Is(wctx.Err(), context.DeadlineExceeded)
	cancel()

	now := sw.now().UTC()
	run.FinishedAt = &now
	run.Counts = res.ByProject
	switch {
	case err != nil && walkExpired && ctx.Err() == nil:
		// The walk reached its own deadline (chatRetentionWalkContext), not
		// the caller's: the counts so far are kept, as a sweep or manual
		// run that stops at its budget is partial. A chunk timeout is not
		// this case and is recorded as failed below.
		run.Status = store.ChatRetentionStatusPartial
		err = nil
		_, _ = fmt.Fprintln(logger, "The preview stopped at its time limit; the counts are partial")
	case err != nil:
		run.Status = store.ChatRetentionStatusFailed
		run.Error = err.Error()
	default:
		run.Status = store.ChatRetentionStatusOK
	}
	fctx, fcancel := context.WithTimeout(context.WithoutCancel(ctx), chatRetentionFinalWriteTimeout)
	defer fcancel()
	if serr := st.SaveChatRetentionRun(fctx, run); serr != nil && err == nil {
		err = serr
	}
	writeChatRetentionCounts(logger, "would delete", res.ByProject)
	_, _ = fmt.Fprintf(logger, "Selections: %d\n", res.Selects)
	if err != nil {
		return run, fmt.Errorf("chat retention preview: %w", err)
	}
	return run, nil
}

// chatRetentionWalkContext bounds a preview or manual walk so that the
// final writes still fit before ctx's deadline.
func chatRetentionWalkContext(ctx context.Context, start time.Time, remaining time.Duration, hasDeadline bool) (context.Context, context.CancelFunc) {
	end := chatRetentionWalkEnd(start, chatRetentionOpMaxDuration, remaining, hasDeadline)
	return context.WithDeadline(ctx, end)
}

// acquireChatRetention waits until this process's tick guard and the chat
// retention advisory lock are both free, takes them and returns their
// release. waiting is called once when it has to wait.
func (s *Server) acquireChatRetention(ctx context.Context, waiting func()) (func(), error) {
	notified := false
	for {
		if s.chatRetention.running.CompareAndSwap(false, true) {
			release, acquired, err := s.tryChatRetentionLock(ctx)
			if err != nil {
				s.chatRetention.running.Store(false)
				return nil, err
			}
			if acquired {
				return func() {
					release()
					s.chatRetention.running.Store(false)
				}, nil
			}
			s.chatRetention.running.Store(false)
		}
		if !notified && waiting != nil {
			waiting()
			notified = true
		}
		if err := sleepCtx(ctx, chatRetentionLockPoll); err != nil {
			return nil, err
		}
	}
}

// tryChatRetentionLock takes the chat retention advisory lock without
// waiting. A store without advisory locks (one process) always succeeds.
func (s *Server) tryChatRetentionLock(ctx context.Context) (func(), bool, error) {
	locker, ok := s.store.(store.AdvisoryLocker)
	if !ok {
		return func() {}, true, nil
	}
	acquired, release, err := locker.TryAdvisoryLock(ctx, store.LockChatRetention)
	if err != nil {
		return nil, false, err
	}
	if !acquired {
		_ = release()
		return nil, false, nil
	}
	return func() { _ = release() }, true, nil
}

// runChatRetentionManual runs the sweep once, as a tick would but with no
// tick budget (the pause between chunks stays) and with the main walk from
// the beginning (chatRetentionSweepManual), under the lock the sweep ticks
// hold, and records it as a manual run with actor. It refuses while the
// sweep is not active (hub.chat_retention and enabled).
func (s *Server) runChatRetentionManual(ctx context.Context, logger io.Writer, actor string) (*store.ChatRetentionRun, chatRetentionTickResult, error) {
	var res chatRetentionTickResult
	st := s.chatRetentionStore()
	if st == nil {
		return nil, res, errors.New("chat retention run: the chat retention store is not available (native chat is disabled or its store is unavailable)")
	}
	release, err := s.acquireChatRetention(ctx, func() {
		_, _ = fmt.Fprintln(logger, "Waiting for the running chat retention sweep to finish")
	})
	if err != nil {
		return nil, res, fmt.Errorf("chat retention run: %w", err)
	}
	defer release()

	sw := newChatRetentionSweeper(st)
	s.mu.RLock()
	if as := s.attachmentStore; as != nil {
		sw.attachments = as
	}
	s.mu.RUnlock()

	start := sw.now()
	// The policy is taken once the lock is held: the settings may have
	// changed while the run waited.
	p := s.chatRetentionPolicyAt(start)
	if !p.purges() {
		return nil, res, errors.New("chat retention run: chat retention is not enabled")
	}
	run := &store.ChatRetentionRun{
		Trigger:   store.ChatRetentionTriggerManual,
		StartedAt: start.UTC(),
		Status:    store.ChatRetentionStatusRunning,
		Actor:     actor,
		Policy:    chatRetentionRunPolicy(p),
		Counts:    map[string]store.ChatRetentionCounts{},
	}
	if err := st.SaveChatRetentionRun(ctx, run); err != nil {
		return nil, res, fmt.Errorf("chat retention run: %w", err)
	}
	writeChatRetentionPolicy(logger, "Running", p)

	remaining, hasDeadline := chatRetentionTimeLeft(ctx)
	budgetEnd := chatRetentionWalkEnd(start, chatRetentionOpMaxDuration, remaining, hasDeadline)
	res.Ran = true
	err = sw.sweepAndRecord(ctx, p, run, &res, start, budgetEnd, chatRetentionSweepManual)
	if res.LastAppliedCutoff != nil {
		s.noteChatRetentionCutoff(*res.LastAppliedCutoff, time.Now())
	}
	// Failed-message purge counts kept while the run held the guard go
	// into the open sweep run now, while this run still holds the lock.
	fctx, fcancel := context.WithTimeout(context.WithoutCancel(ctx), chatRetentionFinalWriteTimeout)
	if _, ferr := s.flushPendingFailedCounts(fctx, st, time.Now()); ferr != nil {
		_, _ = fmt.Fprintf(logger, "Could not record failed-message purge counts: %v\n", ferr)
	}
	fcancel()

	writeChatRetentionCounts(logger, "deleted", run.Counts)
	_, _ = fmt.Fprintf(logger, "Chunks: %d, status: %s\n", res.Chunks, run.Status)
	if res.AttachmentDeleteFailures > 0 {
		_, _ = fmt.Fprintf(logger, "Attachments left for the attachment GC (bytes not deleted): %d\n", res.AttachmentDeleteFailures)
	}
	if err != nil {
		return run, res, fmt.Errorf("chat retention run: %w", err)
	}
	return run, res, nil
}

// chatRetentionInterruptedError is the error recorded on a preview or
// manual run that a hub restart interrupted.
const chatRetentionInterruptedError = "interrupted: the hub stopped before the run finished"

// chatRetentionReconcileTimeout bounds the start-up reconciliation.
const chatRetentionReconcileTimeout = 10 * time.Second

// chatRetentionRunMaxAge is past the longest a preview or manual run can
// run (its maintenance run timeout), so a row still "running" that started
// longer ago belongs to no live run on any hub replica.
const chatRetentionRunMaxAge = chatRetentionOpMaxDuration + time.Hour

// ReconcileInterruptedChatRetentionRuns marks the preview and manual runs
// still "running" that a hub restart interrupted as failed (interrupted), as
// AbortRunningMaintenanceOps does for their maintenance runs: each run's row
// is written only by the process that started it. Sweep runs are left
// alone; the next tick continues the open one. Call it once at start, after
// SetChatRetentionStore. Failures are logged.
//
// What counts as interrupted:
//   - a manual run started before this process, when this process can take
//     LockChatRetention: a live manual run holds that lock on whichever
//     replica runs it. While another replica holds the lock (a live manual
//     run, or a sweep tick), only manual rows older than
//     chatRetentionRunMaxAge are failed; the rest wait for the next start.
//   - a preview started before this process. A preview takes no lock, so
//     with several replicas a preview still running on another one is
//     marked failed too; that replica's final save overwrites the status
//     and clears the error when its preview ends, so the panel shows it
//     failed only until then. On a single hub (SQLite) every such row is
//     interrupted.
func (s *Server) ReconcileInterruptedChatRetentionRuns(ctx context.Context) int {
	st := s.chatRetentionStore()
	if st == nil {
		return 0
	}
	s.mu.RLock()
	started := s.startTime
	s.mu.RUnlock()
	now := time.Now()
	if started.IsZero() {
		started = now
	}
	ctx, cancel := context.WithTimeout(ctx, chatRetentionReconcileTimeout)
	defer cancel()

	manualBefore := now.Add(-chatRetentionRunMaxAge)
	if release, acquired, err := s.tryChatRetentionLock(ctx); err != nil {
		slog.Warn("chat retention: reconcile: advisory lock", "error", err)
	} else if acquired {
		manualBefore = started
		defer release()
	}
	if manualBefore.After(started) {
		manualBefore = started
	}

	total := 0
	for _, step := range []struct {
		trigger string
		before  time.Time
	}{
		{store.ChatRetentionTriggerPreview, started},
		{store.ChatRetentionTriggerManual, manualBefore},
	} {
		n, err := st.FailRunningChatRetentionRuns(ctx, []string{step.trigger}, step.before, chatRetentionInterruptedError, now)
		if err != nil {
			slog.Warn("chat retention: reconcile interrupted runs", "trigger", step.trigger, "error", err)
			continue
		}
		total += n
	}
	if total > 0 {
		slog.Info("chat retention: marked interrupted preview and manual runs failed", "count", total)
	}
	return total
}

// writeChatRetentionPolicy writes the policy line of a run log.
func writeChatRetentionPolicy(logger io.Writer, verb string, p chatRetentionPolicy) {
	window := "no retention window (only deleted conversations)"
	if !p.MainCutoff.IsZero() {
		window = fmt.Sprintf("messages older than %d days (before %s)", p.Config.MessagesRetentionDays, p.MainCutoff.UTC().Format(time.RFC3339))
	}
	_, _ = fmt.Fprintf(logger, "%s: %s; messages of conversations and topics deleted more than %d days ago (before %s)\n",
		verb, window, p.GraceDays, p.GraceCutoff.UTC().Format(time.RFC3339))
}

// writeChatRetentionCounts writes the per-project message counts of a run
// log, sorted by project, and their total. It never writes a message ID or
// content.
func writeChatRetentionCounts(logger io.Writer, verb string, byProject map[string]store.ChatRetentionCounts) {
	projects := make([]string, 0, len(byProject))
	for project := range byProject {
		projects = append(projects, project)
	}
	sort.Strings(projects)
	var total store.ChatRetentionCounts
	for _, project := range projects {
		c := byProject[project]
		total.Add(c)
		if c.Main == 0 && c.DeletedConv == 0 {
			continue
		}
		name := project
		if name == "" {
			name = "(no project)"
		}
		_, _ = fmt.Fprintf(logger, "Project %s: %s %d by the window, %d of deleted conversations\n", name, verb, c.Main, c.DeletedConv)
	}
	_, _ = fmt.Fprintf(logger, "Total: %s %d messages (%d by the window, %d of deleted conversations) in %d projects\n",
		verb, total.Main+total.DeletedConv, total.Main, total.DeletedConv, chatRetentionProjectsWithMessages(byProject))
}

// chatRetentionProjectsWithMessages counts the projects with at least one
// message counted.
func chatRetentionProjectsWithMessages(byProject map[string]store.ChatRetentionCounts) int {
	n := 0
	for _, c := range byProject {
		if c.Main > 0 || c.DeletedConv > 0 {
			n++
		}
	}
	return n
}

// requireChatRetentionExperiment refuses an operation while
// hub.chat_retention is off (the routes answer 404 before this; this
// covers a flag turned off after the request was accepted).
func (s *Server) requireChatRetentionExperiment() error {
	if !s.experimentEnabled(experiments.ChatRetention) {
		return errors.New("chat retention is not available: experiment hub.chat_retention is off")
	}
	return nil
}

// ChatRetentionPreviewExecutor runs chat-retention-preview. Its optional
// parameter messages_retention_days (a string of whole days, 7 to 36500)
// previews that window instead of the saved one.
type ChatRetentionPreviewExecutor struct {
	srv   *Server
	actor string
}

func (e *ChatRetentionPreviewExecutor) setMaintenanceActor(userID string) { e.actor = userID }

// ValidateParams implements maintenance.ParamValidator.
func (e *ChatRetentionPreviewExecutor) ValidateParams(params map[string]string) error {
	_, err := chatRetentionPreviewDays(params)
	return err
}

// chatRetentionPreviewDays parses the preview's window parameter (0 when
// absent).
func chatRetentionPreviewDays(params map[string]string) (int, error) {
	v, ok := params[chatRetentionParamDays]
	if !ok || v == "" {
		return 0, nil
	}
	days, err := strconv.Atoi(v)
	if err != nil || days < opsettings.ChatRetentionMinDays || days > opsettings.ChatRetentionMaxDays {
		return 0, &maintenance.RequestError{
			Code: maintenance.CodeInvalidParam,
			Message: fmt.Sprintf("%s must be a whole number of days from %d to %d",
				chatRetentionParamDays, opsettings.ChatRetentionMinDays, opsettings.ChatRetentionMaxDays),
		}
	}
	return days, nil
}

// Run implements MaintenanceExecutor.
func (e *ChatRetentionPreviewExecutor) Run(ctx context.Context, logger io.Writer, params map[string]string) error {
	if err := e.srv.requireChatRetentionExperiment(); err != nil {
		return err
	}
	days, err := chatRetentionPreviewDays(params)
	if err != nil {
		return err
	}
	_, err = e.srv.runChatRetentionPreview(ctx, logger, days, e.actor)
	return err
}

// ChatRetentionRunExecutor runs chat-retention-run. It takes no
// parameters and is refused (409 chat_retention_disabled) while the sweep
// is not enabled.
type ChatRetentionRunExecutor struct {
	srv   *Server
	actor string
}

func (e *ChatRetentionRunExecutor) setMaintenanceActor(userID string) { e.actor = userID }

// ValidateParams implements maintenance.ParamValidator: a manual run is
// refused before it is created while the sweep would purge nothing.
func (e *ChatRetentionRunExecutor) ValidateParams(map[string]string) error {
	if !e.srv.chatRetentionPolicyAt(time.Now()).purges() {
		return &maintenance.RequestError{
			Code:    chatRetentionCodeDisabled,
			Message: "Chat retention is not enabled: enable the retention sweep in Admin > Server Config first",
		}
	}
	return nil
}

// Run implements MaintenanceExecutor.
func (e *ChatRetentionRunExecutor) Run(ctx context.Context, logger io.Writer, _ map[string]string) error {
	if err := e.srv.requireChatRetentionExperiment(); err != nil {
		return err
	}
	_, _, err := e.srv.runChatRetentionManual(ctx, logger, e.actor)
	return err
}
