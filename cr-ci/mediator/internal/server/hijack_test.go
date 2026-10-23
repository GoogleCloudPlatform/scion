package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/cr-ci/mediator/internal/fakes"
	"github.com/GoogleCloudPlatform/scion/cr-ci/mediator/internal/logging"
)

func inProgress(jobID, runID int64, repo, runnerName string) []byte {
	return jobEvent("in_progress", jobID, runID, repo, "", runnerName)
}

func jobEvent(action string, jobID, runID int64, repo, conclusion, runnerName string) []byte {
	var m map[string]any
	_ = json.Unmarshal(jobPayload(action, jobID, runID, repo, []string{testLabel}, conclusion), &m)
	m["workflow_job"].(map[string]any)["runner_name"] = runnerName
	b, _ := json.Marshal(m)
	return b
}

func execFor(h *harness, runnerName string) []fakes.Execution {
	var out []fakes.Execution
	for _, e := range h.executions() {
		if e.Env["RUNNER_NAME"] == runnerName {
			out = append(out, e)
		}
	}
	return out
}

func (h *harness) activeFor(runnerName string) int {
	n := 0
	for _, e := range execFor(h, runnerName) {
		if !e.Completed {
			n++
		}
	}
	return n
}

// ---- C1: cancellation keyed on runner_name and busy state --------------------

func TestCancelledWhileQueuedLeavesBusyRunnerAlone(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.addSameRepoRun(10)
	h.addSameRepoRun(11)
	h.queued(100, 10)
	// GitHub gave cr-job-100 to job 101; job 100 is then superseded while
	// still queued.
	h.gh.SetRunnerState("cr-job-100", "online", true)
	code, _ := h.webhook(jobEvent("completed", 100, 10, testRepo, "cancelled", ""))
	if code != http.StatusOK {
		t.Fatalf("completed: %d, want 200", code)
	}
	if len(h.cr.CancelCalls) != 0 || len(h.gh.Deleted) != 0 {
		t.Errorf("busy runner touched: cancels=%v deleted=%v", h.cr.CancelCalls, h.gh.Deleted)
	}
}

func TestCancelledOnRunnerTakesNoAction(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.addSameRepoRun(10)
	h.queued(100, 10)
	h.gh.SetRunnerState("cr-job-100", "online", true)
	code, _ := h.webhook(jobEvent("completed", 100, 10, testRepo, "cancelled", "cr-job-100"))
	if code != http.StatusOK {
		t.Fatalf("completed: %d", code)
	}
	// GitHub stops the job itself and the ephemeral runner exits.
	if len(h.cr.CancelCalls) != 0 || len(h.gh.Deleted) != 0 {
		t.Errorf("cancel on runner stopped something: cancels=%v deleted=%v", h.cr.CancelCalls, h.gh.Deleted)
	}
	if h.claims.Pending(100) {
		t.Error("claim of the finished runner kept")
	}
}

// TestCancelledWhileQueuedRaceToBusy: the idle runner picks up a job between
// the runner list and the delete; GitHub answers 422 and it is left alone.
func TestCancelledWhileQueuedSkippedWhenDeleteSaysBusy(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.addSameRepoRun(10)
	h.queued(100, 10)
	h.gh.SetRunnerState("cr-job-100", "offline", false)
	h.gh.BusyOnDelete = true
	if code, _ := h.webhook(jobEvent("completed", 100, 10, testRepo, "cancelled", "")); code != http.StatusOK {
		t.Fatalf("completed: %d", code)
	}
	if len(h.cr.CancelCalls) != 0 {
		t.Errorf("execution cancelled although the runner turned busy: %v", h.cr.CancelCalls)
	}
}

// ---- R1: webhook work survives the delivery being dropped --------------------

func (h *harness) serveDirect(ctx context.Context, body []byte) int {
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "workflow_job")
	req.Header.Set("X-Hub-Signature-256", sign(fakeHookSecret, body))
	rec := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rec, req)
	return rec.Code
}

func TestDispatchSurvivesDroppedDelivery(t *testing.T) {
	t.Run("launch completes", func(t *testing.T) {
		h := newHarness(t, harnessOpts{})
		h.addSameRepoRun(10, fakes.GitHubJob{ID: 100, Status: "queued", Labels: []string{testLabel}})
		ctx, cancel := context.WithCancel(context.Background())
		h.cr.OnRun = cancel // GitHub gives up right after the mint
		h.serveDirect(ctx, jobPayload("queued", 100, 10, testRepo, []string{testLabel}, ""))
		_, registered := h.gh.RunnerByName("cr-job-100")
		if h.activeFor("cr-job-100") != 1 || !registered || !h.claims.Pending(100) {
			t.Errorf("launch abandoned with the delivery: registered=%v claim=%v executions=%+v", registered, h.claims.Pending(100), h.executions())
		}
	})
	t.Run("cleanup completes", func(t *testing.T) {
		h := newHarness(t, harnessOpts{})
		h.addSameRepoRun(10, fakes.GitHubJob{ID: 100, Status: "queued", Labels: []string{testLabel}})
		ctx, cancel := context.WithCancel(context.Background())
		h.cr.OnRun = cancel
		h.cr.FailRun = true
		h.serveDirect(ctx, jobPayload("queued", 100, 10, testRepo, []string{testLabel}, ""))
		if len(h.gh.Runners()) != 0 {
			t.Errorf("registration left behind after a failed launch on a dropped delivery: %v", h.gh.Runners())
		}
	})
}

// ---- R2: replayed queued event ----------------------------------------------

func TestReplayedQueuedEventDoesNotLaunch(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.addSameRepoRun(10)
	body := jobPayload("queued", 100, 10, testRepo, []string{testLabel}, "")
	h.queued(100, 10)
	// The job runs and finishes; GitHub removes the ephemeral runner.
	h.gh.SetJob(10, fakes.GitHubJob{ID: 100, Status: "completed", Labels: []string{testLabel}, RunnerName: "cr-job-100"})
	h.webhook(jobEvent("completed", 100, 10, testRepo, "success", "cr-job-100"))
	h.gh.RemoveRunner("cr-job-100")
	h.cr.Complete("100")

	code, out := h.webhook(body) // redelivered or replayed
	if code != http.StatusOK || !strings.Contains(out, "not queued") {
		t.Errorf("replay: %d %q", code, out)
	}
	if h.jitCount() != 1 || len(h.executions()) != 1 {
		t.Errorf("replay launched an orphan: jit=%d executions=%d", h.jitCount(), len(h.executions()))
	}
}

// ---- R3: labels a cr-ci runner cannot satisfy --------------------------------

func TestUnservableLabels(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	labels := []string{"self-hosted", testLabel}
	h.addSameRepoRun(10, fakes.GitHubJob{ID: 100, Status: "queued", Labels: labels, CreatedAt: h.clock.Now()})
	code, _ := h.webhook(jobPayload("queued", 100, 10, testRepo, labels, ""))
	if code != http.StatusOK || h.jitCount() != 0 {
		t.Errorf("unservable job minted: %d jit=%d", code, h.jitCount())
	}
	if !strings.Contains(h.logs.String(), "labels_unservable") {
		t.Error("no labels_unservable log")
	}
	if res := h.reconcile(); res.Backfilled != 0 || h.jitCount() != 0 {
		t.Errorf("backfill minted for an unservable job: %+v", res)
	}
}

// ---- O4: capacity counts launches the list does not show yet -----------------

func TestCapacityCountsUnlistedLaunches(t *testing.T) {
	h := newHarness(t, harnessOpts{max: 1})
	h.cr.ListLag = time.Minute
	h.addSameRepoRun(10)
	h.addSameRepoRun(11)
	h.queued(100, 10)
	if code, out := h.queued(101, 11); code != http.StatusOK || !strings.Contains(out, "capacity") {
		t.Errorf("second launch within the list lag: %d %q, want capacity full", code, out)
	}
	if len(h.executions()) != 1 {
		t.Errorf("executions = %d, want 1", len(h.executions()))
	}
}

// ---- R4: hijack guard ---------------------------------------------------------

func TestInProgressNormalMatch(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.addSameRepoRun(10)
	h.queued(100, 10)
	gets := h.gh.RunGets
	code, out := h.webhook(inProgress(100, 10, testRepo, "cr-job-100"))
	if code != http.StatusOK || strings.TrimSpace(out) != "ok" {
		t.Fatalf("in_progress: %d %q", code, out)
	}
	if h.gh.RunGets != gets || len(h.cr.CancelCalls) != 0 || len(h.gh.Deleted) != 0 {
		t.Error("a runner running its own job was re-checked or stopped")
	}
	if code, _ := h.webhook(inProgress(300, 30, testRepo, "some-other-runner")); code != http.StatusOK || len(h.cr.CancelCalls) != 0 {
		t.Error("foreign runner handled")
	}
}

func TestHijackKill(t *testing.T) {
	tests := []struct {
		name      string
		run       *fakes.GitHubRun // the hijacking job's run; nil = not found
		repo      string           // repository of the hijacking job
		busy      bool             // registration still busy: GitHub refuses the delete
		reNamedTo string           // name of the replacement runner for job 100; "" = none
	}{
		{"fork PR", &fakes.GitHubRun{ID: 12, Repo: testRepo, HeadRepo: "contributor/example-repo", Event: "pull_request", Status: "in_progress"}, testRepo, false, "cr-job-120"},
		{"fork PR, busy registration", &fakes.GitHubRun{ID: 12, Repo: testRepo, HeadRepo: "contributor/example-repo", Event: "pull_request", Status: "in_progress"}, testRepo, true, "cr-job-120"},
		{"pull_request_target", &fakes.GitHubRun{ID: 12, Repo: testRepo, HeadRepo: testRepo, Event: "pull_request_target", Status: "in_progress"}, testRepo, false, "cr-job-120"},
		{"workflow_run", &fakes.GitHubRun{ID: 12, Repo: testRepo, HeadRepo: testRepo, Event: "workflow_run", Status: "in_progress"}, testRepo, false, "cr-job-120"},
		{"run lookup fails (fail closed)", nil, testRepo, false, "cr-job-120"},
		{"repo not served", nil, "other-owner/other-repo", false, "cr-job-120"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, harnessOpts{})
			h.addSameRepoRun(10)
			if tt.run != nil {
				h.gh.AddRun(*tt.run)
			}
			h.queued(100, 10)
			first := execFor(h, "cr-job-100")[0]
			if tt.busy {
				h.gh.SetRunnerState("cr-job-100", "online", true)
			}

			code, out := h.webhook(inProgress(120, 12, tt.repo, "cr-job-100"))
			if code != http.StatusOK || !strings.Contains(out, "killed") {
				t.Fatalf("in_progress: %d %q", code, out)
			}
			if len(h.cr.CancelCalls) != 1 || h.cr.CancelCalls[0] != first.Name {
				t.Errorf("cancel calls = %v, want [%s] (looked up by runner name)", h.cr.CancelCalls, first.Name)
			}
			h.wantLog(logging.EventKillHijacked)
			// Job 100 is served again at once, under a free name.
			var replacement []fakes.Execution
			for _, e := range h.executions() {
				if e.Env["GH_JOB_ID"] == "100" && !e.Completed {
					replacement = append(replacement, e)
				}
			}
			if len(replacement) != 1 || replacement[0].Env["RUNNER_NAME"] != tt.reNamedTo {
				t.Fatalf("replacement for job 100 = %+v, want one named %s", replacement, tt.reNamedTo)
			}
			if _, ok := h.gh.RunnerByName(tt.reNamedTo); !ok {
				t.Errorf("no %s registration", tt.reNamedTo)
			}
		})
	}
}

// TestHijackLaterEventsSpareReplacement: the hijacking job's completed
// event, which still names the hijacked runner, must not touch the runner
// that replaced it.
func TestHijackLaterEventsSpareReplacement(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.addSameRepoRun(10)
	h.gh.AddRun(fakes.GitHubRun{ID: 12, Repo: testRepo, HeadRepo: "contributor/example-repo", Event: "pull_request", Status: "in_progress"})
	h.queued(100, 10)
	h.webhook(inProgress(120, 12, testRepo, "cr-job-100"))
	repl := execFor(h, "cr-job-120")
	if len(repl) != 1 {
		t.Fatalf("no replacement: %+v", h.executions())
	}
	h.webhook(jobEvent("completed", 120, 12, testRepo, "failure", "cr-job-100"))
	if h.activeFor("cr-job-120") != 1 || !h.claims.Pending(120) {
		t.Errorf("replacement runner stopped or its claim dropped by the hijacker's completed event")
	}
	if code, _, _ := h.claim(repl[0].Env["RUNNER_CLAIM_TOKEN"]); code != http.StatusOK {
		t.Errorf("replacement claim: %d", code)
	}
}

func TestHijackOnCompletedAndInReconcile(t *testing.T) {
	fork := fakes.GitHubRun{ID: 12, Repo: testRepo, HeadRepo: "contributor/example-repo", Event: "pull_request", Status: "in_progress"}
	t.Run("completed event", func(t *testing.T) {
		h := newHarness(t, harnessOpts{})
		h.addSameRepoRun(10)
		h.gh.AddRun(fork)
		h.queued(100, 10)
		if code, out := h.webhook(jobEvent("completed", 120, 12, testRepo, "success", "cr-job-100")); code != http.StatusOK || !strings.Contains(out, "killed") {
			t.Fatalf("completed: %d %q", code, out)
		}
		h.wantLog(logging.EventKillHijacked)
	})
	t.Run("missed webhook, found by reconcile", func(t *testing.T) {
		h := newHarness(t, harnessOpts{})
		h.addSameRepoRun(10)
		h.queued(100, 10)
		fork.Jobs = []fakes.GitHubJob{{ID: 120, Status: "in_progress", Labels: []string{testLabel}, RunnerName: "cr-job-100"}}
		h.gh.AddRun(fork)
		res := h.reconcile()
		if res.HijacksKilled != 1 || h.activeFor("cr-job-100") != 0 {
			t.Fatalf("result %+v; cr-job-100 active = %d, want the hijacked one killed", res, h.activeFor("cr-job-100"))
		}
		if len(h.cr.CancelCalls) != 1 {
			t.Errorf("cancel calls = %v", h.cr.CancelCalls)
		}
		if h.activeFor("cr-job-120") != 1 {
			t.Errorf("no replacement named cr-job-120: %+v", h.executions())
		}
		// The next pass does not kill again.
		h.gh.SetJob(12, fakes.GitHubJob{ID: 120, Status: "completed", Labels: []string{testLabel}, RunnerName: "cr-job-100"})
		if res := h.reconcile(); res.HijacksKilled != 0 {
			t.Errorf("second pass killed %d", res.HijacksKilled)
		}
	})
}

// TestLegitimateSwap: the runner launched for job 100 starts job 101, which
// is just as legitimate. Nothing is killed, and job 100 is still served.
func TestLegitimateSwap(t *testing.T) {
	t.Run("taker had no runner", func(t *testing.T) {
		h := newHarness(t, harnessOpts{})
		h.addSameRepoRun(10)
		h.addSameRepoRun(11, fakes.GitHubJob{ID: 101, Status: "in_progress", Labels: []string{testLabel}, RunnerName: "cr-job-100"})
		h.queued(100, 10) // job 101's webhook was missed
		code, out := h.webhook(inProgress(101, 11, testRepo, "cr-job-100"))
		if code != http.StatusOK || !strings.Contains(out, "swap") {
			t.Fatalf("in_progress: %d %q", code, out)
		}
		if len(h.cr.CancelCalls) != 0 || len(h.gh.Deleted) != 0 {
			t.Error("legitimate swap killed a runner")
		}
		ex := execFor(h, "cr-job-101")
		if len(ex) != 1 || ex[0].Env["GH_JOB_ID"] != "100" {
			t.Fatalf("no runner serving job 100 under the free name cr-job-101: %+v", h.executions())
		}
		h.wantLog(logging.EventRunnerSwap)
		// Reconcile sees job 100 queued: served, so nothing more; and the
		// swap seen again in reconcile changes nothing.
		h.gh.SetRunnerState("cr-job-100", "online", true)
		if res := h.reconcile(); res.Backfilled != 0 || res.HijacksKilled != 0 || len(h.executions()) != 2 {
			t.Errorf("reconcile after swap: %+v, executions %d", res, len(h.executions()))
		}
	})
	t.Run("taker had its own runner", func(t *testing.T) {
		h := newHarness(t, harnessOpts{})
		h.addSameRepoRun(10)
		h.addSameRepoRun(11)
		h.queued(100, 10)
		h.queued(101, 11)
		if code, out := h.webhook(inProgress(101, 11, testRepo, "cr-job-100")); code != http.StatusOK || !strings.Contains(out, "swap") {
			t.Fatalf("in_progress: %d %q", code, out)
		}
		if len(h.executions()) != 2 || len(h.cr.CancelCalls) != 0 {
			t.Errorf("executions = %d, cancels = %v; want 2 and none (cr-job-101 serves job 100)", len(h.executions()), h.cr.CancelCalls)
		}
		// Job 100 then starts on cr-job-101: that runner was launched for
		// 101, which is running elsewhere, so it is another swap with
		// nothing left to serve.
		h.gh.SetJob(10, fakes.GitHubJob{ID: 100, Status: "in_progress", Labels: []string{testLabel}, RunnerName: "cr-job-101"})
		if code, _ := h.webhook(inProgress(100, 10, testRepo, "cr-job-101")); code != http.StatusOK || len(h.executions()) != 2 {
			t.Errorf("second swap launched more runners: %d", len(h.executions()))
		}
	})
	t.Run("cancel after swap stops nothing", func(t *testing.T) {
		h := newHarness(t, harnessOpts{})
		h.addSameRepoRun(10)
		h.addSameRepoRun(11)
		h.queued(100, 10)
		h.queued(101, 11)
		h.webhook(inProgress(101, 11, testRepo, "cr-job-100"))
		h.gh.SetRunnerState("cr-job-100", "online", true)
		if code, _ := h.webhook(jobEvent("completed", 101, 11, testRepo, "cancelled", "cr-job-100")); code != http.StatusOK {
			t.Fatalf("completed: %d", code)
		}
		if len(h.cr.CancelCalls) != 0 || len(h.gh.Deleted) != 0 {
			t.Errorf("cancel of a job on a runner touched runners: cancels=%v deleted=%v", h.cr.CancelCalls, h.gh.Deleted)
		}
	})
}

// ---- review round 2 ----------------------------------------------------------

// TestClaimKeyFetchFailureIs503: when Google's keys cannot be fetched, the
// runner gets 503 (it retries) and the claim is not consumed.
func TestClaimKeyFetchFailureIs503(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.addSameRepoRun(10)
	h.queued(100, 10)
	tok := execFor(h, "cr-job-100")[0].Env["RUNNER_CLAIM_TOKEN"]
	h.google.Fail = true
	if code, _, _ := h.claim(tok); code != http.StatusServiceUnavailable {
		t.Fatalf("claim during a JWKS outage: %d, want 503", code)
	}
	// Within the refetch backoff there is still no key set: still 503.
	if code, _, _ := h.claim(tok); code != http.StatusServiceUnavailable {
		t.Fatalf("claim within the JWKS backoff: %d, want 503", code)
	}
	if code, _ := h.reconcileWith(h.google.IDToken(testMediatorURL, schedulerEmail, h.clock.Now())); code != http.StatusServiceUnavailable {
		t.Errorf("reconcile during a JWKS outage: %d, want 503", code)
	}
	h.google.Fail = false
	h.clock.Add(61 * time.Second)
	if code, _, _ := h.claim(tok); code != http.StatusOK {
		t.Errorf("claim after the outage: %d, want 200 (not consumed by the 503)", code)
	}
}

// TestSwapRebindsTakersRunner: job 101 takes cr-job-100 while its own
// cr-job-101 is idle. cr-job-101 now serves job 100, so cancelling job 100
// while queued stops cr-job-101 (and leaves the busy cr-job-100 alone).
func TestSwapRebindsTakersRunner(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.addSameRepoRun(10)
	h.addSameRepoRun(11)
	h.queued(100, 10)
	h.queued(101, 11)
	idle := execFor(h, "cr-job-101")[0]
	h.webhook(inProgress(101, 11, testRepo, "cr-job-100"))
	h.gh.SetRunnerState("cr-job-100", "online", true)

	if code, _ := h.webhook(jobEvent("completed", 100, 10, testRepo, "cancelled", "")); code != http.StatusOK {
		t.Fatalf("completed: %d", code)
	}
	if len(h.cr.CancelCalls) != 1 || h.cr.CancelCalls[0] != idle.Name {
		t.Errorf("cancel calls = %v, want only the idle %s", h.cr.CancelCalls, idle.Name)
	}
	if _, ok := h.gh.RunnerByName("cr-job-101"); ok {
		t.Error("idle cr-job-101 not deleted")
	}
	if _, ok := h.gh.RunnerByName("cr-job-100"); !ok {
		t.Error("busy cr-job-100 deleted")
	}
}

// TestSecretRefetchDoesNotBlockValidDeliveries: a delivery with a bad
// signature (anyone can send one) triggers a Secret Manager refetch at most
// once a minute; while it is in flight, correctly signed deliveries are
// verified against the cached secret without waiting.
func TestSecretRefetchDoesNotBlockValidDeliveries(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	ping := []byte(`{"zen":"hi"}`)
	if code, _ := h.webhookWith(fakeHookSecret, "ping", ping); code != http.StatusOK {
		t.Fatalf("first delivery: %d", code)
	}
	h.clock.Add(61 * time.Second)
	started, release := make(chan struct{}), make(chan struct{})
	h.gapis.SetSecretGate(started, release)
	bad := make(chan int, 1)
	go func() {
		code, _ := h.webhookWith("FAKE-forged-secret", "ping", ping)
		bad <- code
	}()
	<-started // the refetch is in flight and blocked

	fast := make(chan int, 1)
	go func() {
		code, _ := h.webhookWith(fakeHookSecret, "ping", ping)
		fast <- code
	}()
	select {
	case code := <-fast:
		if code != http.StatusOK {
			t.Errorf("valid delivery during a refetch: %d", code)
		}
	case <-time.After(fakes.GateBackstop): // the refetch stays blocked until release
		t.Error("a correctly signed delivery waited for the in-flight secret refetch")
	}
	close(release)
	if code := <-bad; code != http.StatusUnauthorized {
		t.Errorf("forged delivery: %d, want 401", code)
	}
}
