//go:build !no_sqlite

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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/maintenance"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

// scriptedStarScanner is a MessageStarStore whose upkeep scan walks rows
// in memory, positions being "user/message" pairs of one user. purgePerSweep
// stars are reported purged in every sweep (stars that keep going stale);
// failAt fails the batch that starts after that many examined rows.
type scriptedStarScanner struct {
	MessageStarStore
	rows          int
	purgePerSweep int64
	failAt        int
	calls         []StarCleanupPosition
	limits        []int
	deadlines     []bool
}

var errScannerFailed = errors.New("scanner failed")

func (s *scriptedStarScanner) PurgeMessageStarCandidates(ctx context.Context, after StarCleanupPosition, limit int) (StarCleanupResult, error) {
	s.calls = append(s.calls, after)
	s.limits = append(s.limits, limit)
	_, hasDeadline := ctx.Deadline()
	s.deadlines = append(s.deadlines, hasDeadline)
	start := 0
	if after.MessageID != "" {
		for i := 0; i < s.rows; i++ {
			if starScanID(i) == after.MessageID {
				start = i + 1
			}
		}
	}
	if s.failAt > 0 && start >= s.failAt {
		return StarCleanupResult{}, errScannerFailed
	}
	n := min(limit, s.rows-start)
	res := StarCleanupResult{Examined: n}
	if start == 0 {
		res.Purged = s.purgePerSweep
	}
	if n == limit {
		res.Next = StarCleanupPosition{UserID: "u", MessageID: starScanID(start + n - 1)}
	}
	return res, nil
}

func starScanID(i int) string { return string(rune('a'+i/26)) + string(rune('a'+i%26)) }

func TestReconcileMessageStars_SweepsInBatchesUntilNothingChanges(t *testing.T) {
	scan := &scriptedStarScanner{rows: 5}
	lim := messageStarReconcileLimits{Batch: 2, BatchTimeout: time.Minute, MaxSweeps: 3}
	var seen []int
	report, err := reconcileMessageStars(context.Background(), scan, lim, func(n int, _ messageStarSweep) { seen = append(seen, n) })
	require.NoError(t, err)
	assert.Equal(t, messageStarReconcileReport{Sweeps: 1, Examined: 5, Exhausted: true}, report)
	assert.Equal(t, []int{1}, seen)
	// Batches of 2, 2, 1: the short batch is the end of the table.
	assert.Equal(t, []StarCleanupPosition{{}, {UserID: "u", MessageID: "ab"}, {UserID: "u", MessageID: "ad"}}, scan.calls)
	assert.Equal(t, []int{2, 2, 2}, scan.limits)
	assert.Equal(t, []bool{true, true, true}, scan.deadlines, "every batch is bounded")
}

func TestReconcileMessageStars_ExactBatchMultipleEndsOnEmptyBatch(t *testing.T) {
	scan := &scriptedStarScanner{rows: 4}
	lim := messageStarReconcileLimits{Batch: 2, BatchTimeout: time.Minute, MaxSweeps: 3}
	report, err := reconcileMessageStars(context.Background(), scan, lim, nil)
	require.NoError(t, err)
	assert.Equal(t, 4, report.Examined)
	assert.Len(t, scan.calls, 3, "two full batches, then an empty one that wraps")
}

func TestReconcileMessageStars_StopsAtSweepBoundWhileStarsKeepChanging(t *testing.T) {
	scan := &scriptedStarScanner{rows: 3, purgePerSweep: 1}
	lim := messageStarReconcileLimits{Batch: 10, BatchTimeout: time.Minute, MaxSweeps: 3}
	report, err := reconcileMessageStars(context.Background(), scan, lim, nil)
	require.ErrorIs(t, err, errMessageStarReconcileNotExhausted)
	assert.Equal(t, messageStarReconcileReport{Sweeps: 3, Examined: 9, Purged: 3}, report)
}

func TestReconcileMessageStars_StoreErrorKeepsCountsSoFar(t *testing.T) {
	scan := &scriptedStarScanner{rows: 6, failAt: 4}
	lim := messageStarReconcileLimits{Batch: 2, BatchTimeout: time.Minute, MaxSweeps: 3}
	report, err := reconcileMessageStars(context.Background(), scan, lim, func(int, messageStarSweep) {
		t.Fatal("no sweep completed")
	})
	require.ErrorIs(t, err, errScannerFailed)
	assert.Equal(t, messageStarReconcileReport{Examined: 4}, report)
}

// expiringCtx is a run context that reaches its deadline only when expire
// is called: Done closes then and Err reports DeadlineExceeded. It makes
// the time-limit path deterministic (no timer).
type expiringCtx struct {
	context.Context
	done    chan struct{}
	mu      sync.Mutex
	expired bool
}

func newExpiringCtx() *expiringCtx {
	return &expiringCtx{Context: context.Background(), done: make(chan struct{})}
}

func (c *expiringCtx) Done() <-chan struct{} { return c.done }

func (c *expiringCtx) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.expired {
		return context.DeadlineExceeded
	}
	return nil
}

func (c *expiringCtx) expire() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.expired {
		c.expired = true
		close(c.done)
	}
}

// deadlineScanner is a scripted scanner whose second batch expires the run
// context and returns once the batch context sees it.
type deadlineScanner struct {
	scriptedStarScanner
	run *expiringCtx
}

func (s *deadlineScanner) PurgeMessageStarCandidates(ctx context.Context, after StarCleanupPosition, limit int) (StarCleanupResult, error) {
	if len(s.calls) == 1 {
		s.calls = append(s.calls, after)
		s.run.expire()
		<-ctx.Done()
		return StarCleanupResult{}, ctx.Err()
	}
	return s.scriptedStarScanner.PurgeMessageStarCandidates(ctx, after, limit)
}

func TestReconcileMessageStars_TimeLimitSaysRunAgain(t *testing.T) {
	ctx := newExpiringCtx()
	scan := &deadlineScanner{scriptedStarScanner: scriptedStarScanner{rows: 6}, run: ctx}
	lim := messageStarReconcileLimits{Batch: 2, BatchTimeout: time.Minute, MaxSweeps: 3}
	report, err := reconcileMessageStars(ctx, scan, lim, nil)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, messageStarReconcileReport{Examined: 2}, report)
	assert.Equal(t, "the run hit its time limit after examining 2 stars (repaired 0, purged 0); run the operation again: context deadline exceeded", err.Error())

	// Stopped another way (the hub shutting down): no time-limit claim.
	stopped, stop := context.WithCancel(context.Background())
	stop()
	_, err = reconcileMessageStars(stopped, &scriptedStarScanner{rows: 1}, lim, nil)
	require.ErrorIs(t, err, context.Canceled)
	assert.Contains(t, err.Error(), "the run was stopped after examining 0 stars")
	assert.Contains(t, err.Error(), "run the operation again")
}

func TestReconcileMessageStars_CancelledAndInvalid(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	scan := &scriptedStarScanner{rows: 3}
	_, err := reconcileMessageStars(ctx, scan, defaultMessageStarReconcileLimits, nil)
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, scan.calls)

	for _, lim := range []messageStarReconcileLimits{
		{Batch: 0, BatchTimeout: time.Second, MaxSweeps: 1},
		{Batch: 1, BatchTimeout: 0, MaxSweeps: 1},
		{Batch: 1, BatchTimeout: time.Second, MaxSweeps: 0},
	} {
		_, err := reconcileMessageStars(context.Background(), scan, lim, nil)
		assert.Error(t, err, "%+v", lim)
	}
	assert.Empty(t, scan.calls)
}

// TestChatStarReconcileOperation runs the maintenance operation through the
// operations endpoint, with the experiment off and on: the run completes,
// logs its counts, repairs a moved star (save time kept), purges a deleted
// owner's star and keeps an inactive owner's star.
func TestChatStarReconcileOperation(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "experiment off"
		if enabled {
			name = "experiment on"
		}
		t.Run(name, func(t *testing.T) {
			f := newStarFixture(t, true)
			setStarsExperiment(t, f.srv, enabled, true)
			ctx := context.Background()

			kept := f.threadMsg(t, f.topicA, f.projA, "kept", time.Now())
			moved := f.threadMsg(t, f.topicA, f.projA, "moved", time.Now())
			at := time.Now().UTC().Add(-time.Hour).Truncate(starTimePrecision)
			insert := func(user, msg, key, project string) {
				t.Helper()
				_, err := f.db.Exec(`INSERT INTO webchat_message_star (user_id, message_id, conversation_key, project_id, starred_at) VALUES (?, ?, ?, ?, ?)`,
					user, msg, key, project, sqliteStarTime(at))
				require.NoError(t, err)
			}
			insert(f.member, kept, f.topicA, f.projA)
			insert(f.member, moved, "dm:agent:"+uuid.NewString()+":user:"+f.member, "")
			insert(uuid.NewString(), kept, f.topicA, f.projA) // owner deleted
			member, err := f.s.GetUser(ctx, f.member)
			require.NoError(t, err)
			member.Status = store.UserStatusSuspended
			require.NoError(t, f.s.UpdateUser(ctx, member))

			rr := postOperationRun(t, f.srv, entadapter.ChatStarReconcileKey, "")
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
			var resp struct {
				RunID string `json:"runId"`
			}
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
			require.NotEmpty(t, resp.RunID)
			run := waitForRunStatus(t, f.s, resp.RunID, func(r *store.MaintenanceOperationRun) bool {
				return r.Status != store.MaintenanceStatusRunning
			})
			require.Equal(t, store.MaintenanceStatusCompleted, run.Status, "log: %s result: %s", run.Log, run.Result)
			assert.Contains(t, run.Log, "Sweep 1: examined 3, repaired 1, purged 1")
			assert.Contains(t, run.Log, "Sweep 2: examined 2, repaired 0, purged 0")
			assert.Contains(t, run.Log, "Total: sweeps 2, examined 5, repaired 1, purged 1")
			assert.Contains(t, run.Log, "Complete: no star needs repair or purge")

			got, err := f.stars.GetMessageStar(ctx, f.member, moved)
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, f.topicA, got.ConversationKey)
			assert.Equal(t, f.projA, got.ProjectID)
			assert.True(t, got.StarredAt.Equal(at), "save time kept")
			got, err = f.stars.GetMessageStar(ctx, f.member, kept)
			require.NoError(t, err)
			assert.NotNil(t, got, "an inactive owner's star is kept")
			n := 0
			require.NoError(t, f.db.QueryRow(`SELECT COUNT(*) FROM webchat_message_star`).Scan(&n))
			assert.Equal(t, 2, n, "the deleted owner's star is purged")
		})
	}
}

// TestChatStarReconcileOperation_Registration: the operation is seeded as a
// routine operation, refuses parameters before a run is created, and its
// runs are bounded.
func TestChatStarReconcileOperation_Registration(t *testing.T) {
	srv, s := newTestServerWithStore(t)
	op, err := s.GetMaintenanceOperation(context.Background(), entadapter.ChatStarReconcileKey)
	require.NoError(t, err)
	assert.Equal(t, store.MaintenanceCategoryOperation, op.Category)

	rr := postOperationRun(t, srv, entadapter.ChatStarReconcileKey, `{"params":{"dryRun":"true"}}`)
	require.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), maintenance.CodeUnknownParam)
	runs, err := s.ListMaintenanceRuns(context.Background(), entadapter.ChatStarReconcileKey, 10)
	require.NoError(t, err)
	assert.Empty(t, runs, "no run for a refused request")

	runCtx, cancel := maintenanceRunContext(entadapter.ChatStarReconcileKey)
	defer cancel()
	_, hasDeadline := runCtx.Deadline()
	assert.True(t, hasDeadline, "a run is bounded")

	exec, err := srv.resolveMaintenanceExecutor(entadapter.ChatStarReconcileKey)
	require.NoError(t, err)
	require.IsType(t, &ChatStarReconcileExecutor{}, exec)
}

// TestChatStarReconcileExecutor_NoStarStore: without a chat store with
// stars the run fails visibly instead of reporting an empty table.
func TestChatStarReconcileExecutor_NoStarStore(t *testing.T) {
	srv, _ := newTestServerWithStore(t)
	require.Nil(t, srv.messageStarStore())
	var log bytes.Buffer
	err := (&ChatStarReconcileExecutor{srv: srv}).Run(context.Background(), &log, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no starred messages support")
}

// TestChatStarReconcileExecutor_NotExhaustedFails: a run that stops at its
// sweep bound fails and says to run it again, with the counts logged.
func TestChatStarReconcileExecutor_NotExhaustedFails(t *testing.T) {
	f := newStarFixture(t, true)
	msg := f.threadMsg(t, f.topicA, f.projA, "kept", time.Now())
	_, err := f.db.Exec(`INSERT INTO webchat_message_star (user_id, message_id, conversation_key, project_id, starred_at) VALUES (?, ?, ?, ?, ?)`,
		uuid.NewString(), msg, f.topicA, f.projA, sqliteStarTime(time.Now()))
	require.NoError(t, err)
	var log bytes.Buffer
	exec := &ChatStarReconcileExecutor{srv: f.srv, limits: &messageStarReconcileLimits{Batch: 1, BatchTimeout: 10 * time.Second, MaxSweeps: 1}}
	err = exec.Run(context.Background(), &log, nil)
	require.ErrorIs(t, err, errMessageStarReconcileNotExhausted)
	assert.True(t, strings.Contains(log.String(), "Total: sweeps 1, examined 1, repaired 0, purged 1"), log.String())
	assert.NotContains(t, log.String(), "Complete")
}
