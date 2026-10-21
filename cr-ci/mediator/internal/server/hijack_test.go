package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

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

func execFor(h *harness, jobID string) []fakes.Execution {
	var out []fakes.Execution
	for _, e := range h.executions() {
		if e.Env["GH_JOB_ID"] == jobID {
			out = append(out, e)
		}
	}
	return out
}

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
	// Runners that are not ours are ignored.
	if code, _ := h.webhook(inProgress(300, 30, testRepo, "some-other-runner")); code != http.StatusOK || len(h.cr.CancelCalls) != 0 {
		t.Error("foreign runner handled")
	}
}

func TestInProgressHijackKill(t *testing.T) {
	tests := []struct {
		name     string
		run      *fakes.GitHubRun // the hijacking job's run; nil = not found
		repo     string           // repository of the hijacking job
		wantGone bool             // registration deleted (repo scope cannot address it through an unserved repo)
	}{
		{"fork PR", &fakes.GitHubRun{ID: 12, Repo: testRepo, HeadRepo: "contributor/example-repo", Event: "pull_request", Status: "in_progress"}, testRepo, true},
		{"pull_request_target", &fakes.GitHubRun{ID: 12, Repo: testRepo, HeadRepo: testRepo, Event: "pull_request_target", Status: "in_progress"}, testRepo, true},
		{"run lookup fails (fail closed)", nil, testRepo, true},
		{"repo not served", nil, "other-owner/other-repo", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, harnessOpts{})
			h.addSameRepoRun(10, fakes.GitHubJob{ID: 100, Status: "queued", Labels: []string{testLabel}})
			if tt.run != nil {
				h.gh.AddRun(*tt.run)
			}
			h.queued(100, 10)
			first := execFor(h, "100")[0]

			code, out := h.webhook(inProgress(120, 12, tt.repo, "cr-job-100"))
			if code != http.StatusOK || !strings.Contains(out, "killed") {
				t.Fatalf("in_progress: %d %q", code, out)
			}
			if len(h.cr.CancelCalls) != 1 || h.cr.CancelCalls[0] != first.Name {
				t.Errorf("cancel calls = %v, want [%s]", h.cr.CancelCalls, first.Name)
			}
			if tt.wantGone && len(h.gh.Deleted) != 1 {
				t.Errorf("hijacked registration not deleted: %v", h.gh.Deleted)
			}
			h.wantLog(logging.EventKillHijacked)

			if tt.wantGone {
				// The legitimate job is re-dispatched at once with a fresh runner.
				ex := execFor(h, "100")
				if len(ex) != 2 || ex[1].Completed {
					t.Fatalf("job 100 not re-dispatched: %+v", ex)
				}
				if _, ok := h.gh.RunnerByName("cr-job-100"); !ok {
					t.Error("no new cr-job-100 registration")
				}
				if ex[1].Env["RUNNER_CLAIM_TOKEN"] == first.Env["RUNNER_CLAIM_TOKEN"] {
					t.Error("claim token reused")
				}
			}
		})
	}
}

// TestInProgressLegitimateSwap: a runner minted for job 100 starts job 101,
// which is just as legitimate. Nothing is killed, and the job that took the
// runner gets a runner named for it, which then serves job 100.
func TestInProgressLegitimateSwap(t *testing.T) {
	t.Run("taker had no runner", func(t *testing.T) {
		h := newHarness(t, harnessOpts{})
		h.addSameRepoRun(10, fakes.GitHubJob{ID: 100, Status: "queued", Labels: []string{testLabel}})
		h.addSameRepoRun(11, fakes.GitHubJob{ID: 101, Status: "in_progress", Labels: []string{testLabel}})
		h.queued(100, 10) // job 101's webhook was missed

		code, out := h.webhook(inProgress(101, 11, testRepo, "cr-job-100"))
		if code != http.StatusOK || !strings.Contains(out, "swap") {
			t.Fatalf("in_progress: %d %q", code, out)
		}
		if len(h.cr.CancelCalls) != 0 || len(h.gh.Deleted) != 0 {
			t.Error("legitimate swap killed a runner")
		}
		if len(execFor(h, "101")) != 1 {
			t.Errorf("no replacement runner for the swap: %+v", h.executions())
		}
		h.wantLog(logging.EventRunnerSwap)
		// Job 100 is still queued; its minted registration is busy with 101
		// and cr-job-101 is idle for it, so reconcile must not launch more.
		if res := h.reconcile(); res.Backfilled != 0 {
			t.Errorf("reconcile launched %d extra runners", res.Backfilled)
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
	})
}

// TestCancelSwappedJob: cancelling a job that runs on a runner minted for
// another job stops that runner, not the runner minted for the cancelled job.
func TestCancelSwappedJob(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.addSameRepoRun(10)
	h.addSameRepoRun(11)
	h.queued(100, 10)
	h.queued(101, 11)
	ran, idle := execFor(h, "100")[0], execFor(h, "101")[0]
	h.webhook(inProgress(101, 11, testRepo, "cr-job-100"))

	code, _ := h.webhook(jobEvent("completed", 101, 11, testRepo, "cancelled", "cr-job-100"))
	if code != http.StatusOK {
		t.Fatalf("completed: %d", code)
	}
	if len(h.cr.CancelCalls) != 1 || h.cr.CancelCalls[0] != ran.Name {
		t.Errorf("cancel calls = %v, want only %s (the runner that ran job 101)", h.cr.CancelCalls, ran.Name)
	}
	for _, e := range h.executions() {
		if e.Name == idle.Name && e.Completed {
			t.Error("cancelled cr-job-101, which is now waiting to serve job 100")
		}
	}
	if _, ok := h.gh.RunnerByName("cr-job-100"); ok {
		t.Error("registration of the runner that ran the cancelled job not deleted")
	}
	if _, ok := h.gh.RunnerByName("cr-job-101"); !ok {
		t.Error("idle cr-job-101 deleted")
	}
}
