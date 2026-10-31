package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/cr-ci/mediator/internal/fakes"
	"github.com/GoogleCloudPlatform/scion/cr-ci/mediator/internal/logging"
)

// Tests for runner records when runner names are reused.

// onRunner sets workflow_job.runner_id in a webhook body.
func onRunner(body []byte, runnerID int64) []byte {
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	m["workflow_job"].(map[string]any)["runner_id"] = runnerID
	b, _ := json.Marshal(m)
	return b
}

// registeredID returns the registration ID of runner name in the fake GitHub.
func (h *harness) registeredID(name string) int64 {
	h.t.Helper()
	r, ok := h.gh.RunnerByName(name)
	if !ok {
		h.t.Fatalf("runner %s not registered", name)
	}
	return r.ID
}

// record returns a copy of the server's record for runner name.
func (h *harness) record(name string) (runnerRecord, bool) {
	h.srv.dispatchMu.Lock()
	defer h.srv.dispatchMu.Unlock()
	rec, ok := h.srv.runners[name]
	if !ok {
		return runnerRecord{}, false
	}
	return *rec, true
}

func (h *harness) forgottenHas(name string) bool {
	h.srv.dispatchMu.Lock()
	defer h.srv.dispatchMu.Unlock()
	_, ok := h.srv.forgotten[name]
	return ok
}

// sameNameRelaunch: job 101 takes cr-job-100, so cr-job-101 (#1) is launched for
// job 100. #1 runs job 102, whose events are not delivered yet, and finishes. A
// re-check of job 101 launches a second cr-job-101 (#2) for job 100. It returns
// the registration IDs of #1 and #2.
func sameNameRelaunch(t *testing.T, o harnessOpts) (h *harness, id1, id2 int64) {
	t.Helper()
	h = newHarness(t, o)
	h.addSameRepoRun(10)
	h.addSameRepoRun(11, fakes.GitHubJob{ID: 101, Status: "in_progress", Labels: []string{testLabel}, RunnerName: "cr-job-100"})
	h.addSameRepoRun(12, fakes.GitHubJob{ID: 102, Status: "completed", Labels: []string{testLabel}, RunnerName: "cr-job-101"})
	h.queued(100, 10)
	if code, out := h.webhook(inProgress(101, 11, testRepo, "cr-job-100")); code != http.StatusOK || !strings.Contains(out, "swap") {
		t.Fatalf("in_progress 101: %d %q", code, out)
	}
	first := execFor(h, "cr-job-101")
	if len(first) != 1 {
		t.Fatalf("cr-job-101 executions = %d, want 1", len(first))
	}
	if code, _, _ := h.claim(first[0].Env["RUNNER_CLAIM_TOKEN"]); code != http.StatusOK {
		t.Fatalf("claim of #1: %d", code)
	}
	id1 = h.registeredID("cr-job-101")
	h.gh.RemoveRunner("cr-job-101")
	h.cr.CompleteRunner("cr-job-101")

	if code, _ := h.webhook(inProgress(101, 11, testRepo, "cr-job-100")); code != http.StatusOK {
		t.Fatalf("in_progress 101 redelivered: %d", code)
	}
	if n := len(execFor(h, "cr-job-101")); n != 2 {
		t.Fatalf("cr-job-101 executions = %d, want 2 (relaunch)", n)
	}
	id2 = h.registeredID("cr-job-101")
	if id1 == id2 {
		t.Fatalf("same registration ID %d for both runners", id1)
	}
	return h, id1, id2
}

// TestLateCompletedAfterSameNameRelaunchKeepsRecord (test 1): the completed event of
// #1 (runner_id #1) arrives after #2 was launched. #2's record and claim survive, it
// is not marked reassigned, nothing is launched, and the name is not forgotten.
// Binding mutant: revert the runner_id check in forgetRunner; this test goes red.
func TestLateCompletedAfterSameNameRelaunchKeepsRecord(t *testing.T) {
	h, id1, id2 := sameNameRelaunch(t, harnessOpts{})
	execs := len(h.executions())

	if code, _ := h.webhook(onRunner(jobEvent("completed", 102, 12, testRepo, "success", "cr-job-101"), id1)); code != http.StatusOK {
		t.Fatalf("late completed: %d", code)
	}
	rec, ok := h.record("cr-job-101")
	if !ok {
		t.Fatal("record of #2 deleted by #1's late completed event")
	}
	if rec.runnerID != id2 || rec.serve != 100 {
		t.Errorf("record = runner_id %d serve %d, want %d and 100", rec.runnerID, rec.serve, id2)
	}
	if rec.reassigned {
		t.Error("record of #2 marked reassigned by #1's late completed event")
	}
	if !h.claims.Pending(101) {
		t.Error("claim of #2 dropped by #1's late completed event")
	}
	if got := len(h.executions()); got != execs {
		t.Errorf("executions %d -> %d after the late completed event", execs, got)
	}
	if h.forgottenHas("cr-job-101") {
		t.Error("cr-job-101 forgotten while #2 is live")
	}
	if got := strings.Count(h.logs.String(), `"reason":"runner_id_mismatch"`); got != 2 {
		t.Errorf("reason=runner_id_mismatch lines = %d, want 2 (guard and forget)", got)
	}
}

// TestStaleRunnerIDUntrustedJobStillKilled: an event whose runner_id is #1's for a
// job that fails the fork guard still kills the runner of that name.
// Binding mutant: apply the runner_id skip before or regardless of forkReason;
// this test goes red.
func TestStaleRunnerIDUntrustedJobStillKilled(t *testing.T) {
	h, id1, _ := sameNameRelaunch(t, harnessOpts{})
	h.gh.AddRun(fakes.GitHubRun{ID: 13, Repo: testRepo, HeadRepo: "contributor/example-repo", Event: "pull_request", Status: "in_progress",
		Jobs: []fakes.GitHubJob{{ID: 130, Status: "in_progress", Labels: []string{testLabel}, RunnerName: "cr-job-101"}}})

	code, out := h.webhook(onRunner(inProgress(130, 13, testRepo, "cr-job-101"), id1))
	if code != http.StatusOK || !strings.Contains(out, "killed") {
		t.Fatalf("untrusted job with a stale runner_id: %d %q, want killed", code, out)
	}
	h.wantLog(logging.EventKillHijacked)
	if n := h.activeFor("cr-job-101"); n != 0 {
		t.Errorf("cr-job-101 active = %d, want 0", n)
	}
}

// TestReboundRunnerNotDuplicatedByBackfill (test 2): job 101 takes cr-job-100 and
// the idle cr-job-101 is rebound to job 100. Once cr-job-100 is gone, reconcile
// backfill sees job 100 queued and must not launch another runner for it.
func TestReboundRunnerNotDuplicatedByBackfill(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.addSameRepoRun(10)
	h.addSameRepoRun(11)
	h.queued(100, 10)
	h.queued(101, 11)
	if code, out := h.webhook(inProgress(101, 11, testRepo, "cr-job-100")); code != http.StatusOK || !strings.Contains(out, "swap") {
		t.Fatalf("in_progress 101: %d %q", code, out)
	}
	if rec, ok := h.record("cr-job-101"); !ok || rec.serve != 100 || rec.reassigned {
		t.Fatalf("cr-job-101 not rebound to job 100: %+v %v", rec, ok)
	}
	// Job 101 finishes on cr-job-100; that runner is gone.
	id := h.registeredID("cr-job-100")
	h.gh.SetJob(11, fakes.GitHubJob{ID: 101, Status: "completed", Labels: []string{testLabel}, RunnerName: "cr-job-100"})
	h.gh.RemoveRunner("cr-job-100")
	h.cr.CompleteRunner("cr-job-100")
	h.webhook(onRunner(jobEvent("completed", 101, 11, testRepo, "success", "cr-job-100"), id))
	execs := len(h.executions())

	if res := h.reconcile(); res.Backfilled != 0 {
		t.Errorf("backfilled %d; job 100 is served by the rebound cr-job-101", res.Backfilled)
	}
	if got := len(h.executions()); got != execs {
		t.Errorf("executions %d -> %d: duplicate runner for job 100", execs, got)
	}
}

// TestSwapTakerFinishesFirstRelaunchesOnce (test 3): job 101 takes cr-job-100, and
// cr-job-101 (#1) is launched for job 100. #1 runs another job and finishes before
// job 101 does. Job 101's completed event relaunches cr-job-101 for job 100 exactly
// once: a redelivery and a reconcile pass launch nothing more.
func TestSwapTakerFinishesFirstRelaunchesOnce(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.addSameRepoRun(10)
	h.addSameRepoRun(11, fakes.GitHubJob{ID: 101, Status: "in_progress", Labels: []string{testLabel}, RunnerName: "cr-job-100"})
	h.queued(100, 10)
	if code, out := h.webhook(inProgress(101, 11, testRepo, "cr-job-100")); code != http.StatusOK || !strings.Contains(out, "swap") {
		t.Fatalf("in_progress 101: %d %q", code, out)
	}
	h.gh.RemoveRunner("cr-job-101")
	h.cr.CompleteRunner("cr-job-101")

	id := h.registeredID("cr-job-100")
	h.gh.SetJob(11, fakes.GitHubJob{ID: 101, Status: "completed", Labels: []string{testLabel}, RunnerName: "cr-job-100"})
	h.gh.RemoveRunner("cr-job-100")
	h.cr.CompleteRunner("cr-job-100")
	completed := onRunner(jobEvent("completed", 101, 11, testRepo, "success", "cr-job-100"), id)
	h.webhook(completed)
	if n := len(execFor(h, "cr-job-101")); n != 2 || h.activeFor("cr-job-101") != 1 {
		t.Fatalf("cr-job-101 executions = %d (active %d), want 2 (1 active): one relaunch", n, h.activeFor("cr-job-101"))
	}

	h.webhook(completed) // redelivered
	if res := h.reconcile(); res.Backfilled != 0 {
		t.Errorf("reconcile backfilled %d", res.Backfilled)
	}
	if n := len(h.executions()); n != 3 {
		t.Errorf("executions = %d, want 3: exactly one relaunch", n)
	}
}

// TestReconcileGuardsLiveReusedName (test 4): reconcile must not skip the guard for a
// reused runner name that is live again.
func TestReconcileGuardsLiveReusedName(t *testing.T) {
	untrustedOn := func(h *harness, runner string) {
		h.gh.AddRun(fakes.GitHubRun{ID: 13, Repo: testRepo, HeadRepo: "contributor/example-repo", Event: "pull_request", Status: "in_progress",
			Jobs: []fakes.GitHubJob{{ID: 130, Status: "in_progress", Labels: []string{testLabel}, RunnerName: runner}}})
	}
	t.Run("late completed of the earlier runner does not forget the name", func(t *testing.T) {
		h, id1, _ := sameNameRelaunch(t, harnessOpts{})
		h.webhook(onRunner(jobEvent("completed", 102, 12, testRepo, "success", "cr-job-101"), id1))
		if h.forgottenHas("cr-job-101") {
			t.Fatal("cr-job-101 forgotten while #2 is live")
		}
		untrustedOn(h, "cr-job-101") // its webhooks are missed
		if res := h.reconcile(); res.HijacksKilled != 1 || h.activeFor("cr-job-101") != 0 {
			t.Errorf("result %+v, cr-job-101 active %d; want the reused name checked and killed", res, h.activeFor("cr-job-101"))
		}
	})
	t.Run("relaunch clears the forgotten name", func(t *testing.T) {
		h := newHarness(t, harnessOpts{max: 2})
		h.addSameRepoRun(10)
		h.addSameRepoRun(11, fakes.GitHubJob{ID: 101, Status: "in_progress", Labels: []string{testLabel}, RunnerName: "cr-job-100"})
		h.addSameRepoRun(12, fakes.GitHubJob{ID: 102, Status: "completed", Labels: []string{testLabel}, RunnerName: "cr-job-101"})
		h.queued(100, 10)
		if code, out := h.webhook(inProgress(101, 11, testRepo, "cr-job-100")); code != http.StatusOK || !strings.Contains(out, "swap") {
			t.Fatalf("in_progress 101: %d %q", code, out)
		}
		// #1 runs job 102; its completed event arrives while #1 is still registered
		// (capacity is full, so no runner is launched for it) and forgets the name.
		h.webhook(onRunner(jobEvent("completed", 102, 12, testRepo, "success", "cr-job-101"), h.registeredID("cr-job-101")))
		if !h.forgottenHas("cr-job-101") {
			t.Fatal("precondition: cr-job-101 not forgotten after its completed event")
		}
		h.gh.RemoveRunner("cr-job-101")
		h.cr.CompleteRunner("cr-job-101")

		// Reconcile re-checks job 101 on cr-job-100 and relaunches cr-job-101.
		h.reconcile()
		if h.activeFor("cr-job-101") != 1 {
			t.Fatalf("cr-job-101 not relaunched: %+v", h.executions())
		}
		if h.forgottenHas("cr-job-101") {
			t.Error("relaunch of cr-job-101 left the name forgotten")
		}
		untrustedOn(h, "cr-job-101")
		if res := h.reconcile(); res.HijacksKilled != 1 || h.activeFor("cr-job-101") != 0 {
			t.Errorf("result %+v, cr-job-101 active %d; want the reused name checked and killed", res, h.activeFor("cr-job-101"))
		}
	})
}

// TestReboundRecordDoneAllowsRelaunch: after a restart, job 101 takes cr-job-100
// while cr-job-101 is idle, so cr-job-101 gets a rebind-created record (no
// execution ref) serving job 100. Once cr-job-101 is gone and its execution done,
// that record must not block a runner for the still-queued job 100: a re-check
// launches exactly one, and reconcile launches nothing more.
func TestReboundRecordDoneAllowsRelaunch(t *testing.T) {
	first := newHarness(t, harnessOpts{})
	first.addSameRepoRun(10)
	first.addSameRepoRun(11)
	first.queued(100, 10)
	first.queued(101, 11)
	h := newHarness(t, harnessOpts{reuseFrom: first}) // fresh memory, same world
	h.gh.SetJob(11, fakes.GitHubJob{ID: 101, Status: "in_progress", Labels: []string{testLabel}, RunnerName: "cr-job-100"})
	if code, out := h.webhook(inProgress(101, 11, testRepo, "cr-job-100")); code != http.StatusOK || !strings.Contains(out, "swap") {
		t.Fatalf("in_progress 101: %d %q", code, out)
	}
	rec, ok := h.record("cr-job-101")
	if !ok || rec.serve != 100 || rec.reassigned || rec.ref != "" {
		t.Fatalf("cr-job-101 record = %+v %v, want a rebind-created record (no ref) serving job 100", rec, ok)
	}
	h.gh.RemoveRunner("cr-job-101")
	h.cr.CompleteRunner("cr-job-101")
	execs := len(h.executions())

	if code, _ := h.webhook(inProgress(101, 11, testRepo, "cr-job-100")); code != http.StatusOK {
		t.Fatalf("in_progress 101 redelivered: %d", code)
	}
	if got := len(h.executions()); got != execs+1 || h.activeFor("cr-job-101") != 1 {
		t.Fatalf("executions %d -> %d, cr-job-101 active %d; want exactly one relaunch for job 100", execs, got, h.activeFor("cr-job-101"))
	}
	if res := h.reconcile(); res.Backfilled != 0 {
		t.Errorf("reconcile backfilled %d", res.Backfilled)
	}
	if got := len(h.executions()); got != execs+1 {
		t.Errorf("executions = %d, want %d: exactly one relaunch", got, execs+1)
	}
}

// TestCompletedWithoutRunnerIDKeepsRecord: a completed event without a runner_id
// leaves the record, its claim and the name alone, even for a record whose
// registration ID is unknown (0).
func TestCompletedWithoutRunnerIDKeepsRecord(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.addSameRepoRun(10)
	h.queued(100, 10)
	h.srv.dispatchMu.Lock()
	h.srv.runners["cr-job-100"].runnerID = 0
	h.srv.dispatchMu.Unlock()

	if code, _ := h.webhook(jobEvent("completed", 100, 10, testRepo, "success", "cr-job-100")); code != http.StatusOK {
		t.Fatalf("completed: %d", code)
	}
	if _, ok := h.record("cr-job-100"); !ok {
		t.Error("record deleted by a completed event without a runner_id")
	}
	if !h.claims.Pending(100) {
		t.Error("claim dropped by a completed event without a runner_id")
	}
	if h.forgottenHas("cr-job-100") {
		t.Error("cr-job-100 forgotten by a completed event without a runner_id")
	}
	if !strings.Contains(h.logs.String(), `"reason":"runner_id_missing"`) {
		t.Errorf("no reason=runner_id_missing line:\n%s", h.logs.String())
	}
}

// TestCompletedWithoutRecordLogsNoRecord: after a restart there is no record for
// the runner, so a completed event with its runner_id changes nothing and says so.
func TestCompletedWithoutRecordLogsNoRecord(t *testing.T) {
	first := newHarness(t, harnessOpts{})
	first.addSameRepoRun(10)
	first.queued(100, 10)
	h := newHarness(t, harnessOpts{reuseFrom: first}) // fresh memory, same world

	if code, _ := h.webhook(onRunner(jobEvent("completed", 100, 10, testRepo, "success", "cr-job-100"), h.registeredID("cr-job-100"))); code != http.StatusOK {
		t.Fatalf("completed: %d", code)
	}
	if !strings.Contains(h.logs.String(), `"reason":"no_record"`) {
		t.Errorf("no reason=no_record line:\n%s", h.logs.String())
	}
	if h.forgottenHas("cr-job-100") {
		t.Error("cr-job-100 forgotten without a record")
	}
}
