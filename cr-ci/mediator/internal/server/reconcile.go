package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/cr-ci/mediator/internal/githubapp"
	"github.com/GoogleCloudPlatform/scion/cr-ci/mediator/internal/logging"
	"github.com/GoogleCloudPlatform/scion/cr-ci/mediator/internal/oidc"
)

// ReconcileResult is the /reconcile response body.
type ReconcileResult struct {
	ClaimsExpired    int      `json:"claims_expired"`
	RunnersReaped    int      `json:"runners_reaped"`
	ExecutionsReaped int      `json:"executions_reaped"`
	HijacksKilled    int      `json:"hijacks_killed"`
	Backfilled       int      `json:"backfilled"`
	LeftQueued       int      `json:"left_queued"`
	ForkRunsRejected int      `json:"fork_runs_rejected"`
	Errors           []string `json:"errors,omitempty"`
	// Skipped: steps for one repo, run or job that GitHub answered with a 5xx.
	// They are retried by the next pass and do not fail this one (HTTP 200).
	Skipped            []string `json:"skipped,omitempty"`
	CapacityFullAtStop bool     `json:"capacity_full,omitempty"`
}

func (s *Server) handleReconcile(w http.ResponseWriter, r *http.Request) {
	if s.o.MediatorURL == "" {
		s.log().Error("MEDIATOR_URL is not set; cannot verify the reconciler", "event", logging.EventError, "reason", "not_configured")
		plain(w, http.StatusServiceUnavailable, "not configured")
		return
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		s.log().Warn("reconcile denied", "event", logging.EventReconcileDenied, "reason", "no bearer token")
		plain(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if err := s.o.ReconcileOIDC.Verify(r.Context(), token); err != nil {
		if errors.Is(err, oidc.ErrKeysUnavailable) {
			s.log().Error("reconcile deferred: cannot verify the caller", "event", logging.EventError, "stage", "reconcile_identity", "error", err.Error())
			plain(w, http.StatusServiceUnavailable, "unavailable")
			return
		}
		s.log().Warn("reconcile denied", "event", logging.EventReconcileDenied, "reason", err.Error())
		plain(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	ctx, cancel := detached(r.Context(), s.o.ReconcileTimeout)
	defer cancel()
	res := s.Reconcile(ctx)
	status := http.StatusOK
	if len(res.Errors) > 0 {
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(res)
}

type candidate struct {
	repo githubapp.Repo
	job  githubapp.Job
}

type runnerJob struct {
	repo githubapp.Repo
	run  githubapp.Run
	job  githubapp.Job
}

// Reconcile runs one pass:
//  1. discovery, without the lock: queued and in_progress runs and their jobs;
//  2. under the lock from here on: the hijack guard for jobs running on
//     cr-ci runners (covers missed webhooks), sharing one view;
//  3. expire claims, reap stale registrations and executions, and backfill
//     queued servable jobs, oldest first, within capacity (a fresh view).
func (s *Server) Reconcile(ctx context.Context) ReconcileResult {
	var res ReconcileResult
	// Every GitHub call in a pass concerns one repo, run or job. A GitHub 5xx
	// there (a 502 on one run's jobs list, say) skips that item only: the rest of
	// the pass goes on, and the next pass retries it. Anything else is an error
	// and makes /reconcile answer 500.
	addErr := func(stage string, err error) {
		if githubServerError(err) {
			res.Skipped = append(res.Skipped, stage+": "+err.Error())
			s.log().Warn("reconcile step skipped after a GitHub server error; retried next pass", "event", logging.EventReconcileSkip, "stage", stage, "error", err.Error())
			return
		}
		res.Errors = append(res.Errors, stage+": "+err.Error())
		s.log().Error("reconcile step failed", "event", logging.EventError, "stage", stage, "error", err.Error())
	}

	cands, onRunners, notices, listed := s.discover(ctx, &res, addErr)
	// Skipping one repo's or run's 5xx keeps a pass green, but a pass that could
	// list no repo at all (a sustained GitHub outage) must turn /reconcile red.
	// (Other errors already make it red.)
	if listed == 0 && len(s.o.TargetRepos) > 0 && len(res.Errors) == 0 {
		msg := "GitHub server errors on every repo; nothing was discovered"
		res.Errors = append(res.Errors, "all_skipped: "+msg)
		s.log().Error("reconcile skipped every repo after GitHub server errors", "event", logging.EventReconcileAllSkipped,
			"repos", len(s.o.TargetRepos), "skipped", len(res.Skipped))
	}

	// One lock hold from the hijack guard to the end of the pass, so the guard
	// loop can share one view (each swap would otherwise list runners and
	// executions again) and nothing launches between its checks.
	s.dispatchMu.Lock()
	defer s.dispatchMu.Unlock()
	gv := newView()
	for _, rj := range onRunners {
		out, err := s.checkRunnerJobLocked(ctx, gv, rj.repo.String(), rj.job.ID, rj.run.ID, rj.job.RunnerName, &rj.run, "reconcile")
		if err != nil {
			addErr("runner_check "+rj.job.RunnerName, err)
		}
		if out == "killed" {
			res.HijacksKilled++
			gv = newView() // the kill changed registrations and executions
		}
	}

	now := s.o.Now()
	for k := range s.noticed {
		if !notices[k] {
			delete(s.noticed, k)
		}
	}
	res.ClaimsExpired = s.o.Claims.Sweep()
	for name, rec := range s.runners {
		if now.Sub(rec.at) > s.o.TaskTimeout+s.o.ReapAfter {
			delete(s.runners, name)
		}
	}
	for ref, at := range s.cancelled {
		if now.Sub(at) > cancelledTTL {
			delete(s.cancelled, ref)
		}
	}
	// Normally cleared when a guard pass sees the registration deleted (the hijacker's
	// completed delivery); this bounds the map if that delivery is lost. A later kill
	// of the same runner then logs ERROR again, which is louder and safe.
	for name, at := range s.hijacked {
		if now.Sub(at) > cancelledTTL {
			delete(s.hijacked, name)
		}
	}

	s.reapRunners(ctx, now, &res, addErr)

	n, err := s.o.Launcher.ReapOrphans(ctx, s.o.TaskTimeout+s.o.ReapAfter)
	res.ExecutionsReaped = n
	if n > 0 {
		s.log().Warn("cancelled executions past the task timeout", "event", logging.EventExecutionReaped, "count", n)
	}
	if err != nil {
		addErr("reap_executions", err)
	}

	v := newView()
	for i, c := range cands {
		out, err := s.dispatchLocked(ctx, v, c.repo, c.job.ID, c.job.RunID, c.job.ID, "reconcile")
		switch {
		case err != nil:
			addErr("dispatch", err)
		case out == outLaunched:
			res.Backfilled++
		case out == outQueued:
			res.LeftQueued = len(cands) - i
			res.CapacityFullAtStop = true
		}
		if res.CapacityFullAtStop {
			break
		}
	}

	s.log().Info("reconcile finished", "event", logging.EventReconcile,
		"claims_expired", res.ClaimsExpired, "runners_reaped", res.RunnersReaped, "executions_reaped", res.ExecutionsReaped,
		"hijacks_killed", res.HijacksKilled, "backfilled", res.Backfilled, "left_queued", res.LeftQueued,
		"fork_runs_rejected", res.ForkRunsRejected, "errors", len(res.Errors), "skipped", len(res.Skipped))
	return res
}

// githubServerError reports whether err is a GitHub 5xx response.
func githubServerError(err error) bool {
	var ae *githubapp.APIError
	return errors.As(err, &ae) && ae.Status >= 500 && ae.Status <= 599
}

// discover lists queued and in_progress runs of every served repo. It
// returns queued servable jobs of runs that pass the fork guard (oldest
// first, deduplicated) and the jobs currently running on cr-ci runners.
// listed counts the repos for which at least one runs list succeeded.
func (s *Server) discover(ctx context.Context, res *ReconcileResult, addErr func(string, error)) (cands []candidate, onRunners []runnerJob, notices map[string]bool, listed int) {
	notices = map[string]bool{}
	notice := func(key string) bool { // true the first time a notice is seen
		notices[key] = true
		s.dispatchMu.Lock()
		defer s.dispatchMu.Unlock()
		first := !s.noticed[key]
		s.noticed[key] = true
		return first
	}
	seenJob := map[int64]bool{}
	for _, repo := range s.o.TargetRepos {
		repoListed := false
		for _, status := range []string{"queued", "in_progress"} {
			runs, err := s.o.GitHub.ListRuns(ctx, repo, status)
			if err != nil {
				addErr("list_runs "+repo.String(), err)
				continue
			}
			if !repoListed {
				repoListed = true
				listed++
			}
			for i := range runs {
				run := runs[i]
				jobs, err := s.o.GitHub.ListJobs(ctx, repo, run.ID)
				if err != nil {
					addErr("list_jobs "+repo.String(), err)
					continue
				}
				reason := forkReason(&run, repo)
				for _, j := range jobs {
					if seenJob[j.ID] {
						continue
					}
					seenJob[j.ID] = true
					if _, ours := parseRunnerName(j.RunnerName); ours && j.Status == "in_progress" {
						onRunners = append(onRunners, runnerJob{repo: repo, run: run, job: j})
						continue
					}
					if j.Status != "queued" || !hasLabel(j.Labels, s.o.RunnerLabel) {
						continue
					}
					if !s.servable(j.Labels) {
						if notice("labels:" + strconv.FormatInt(j.ID, 10)) {
							s.log().Warn("job labels cannot be served by a cr-ci runner (it carries RUNNER_LABEL only)", "event", logging.EventWebhookIgnored,
								"source", "reconcile", "repo", repo.String(), "job_id", j.ID, "reason", "labels_unservable", "labels", strings.Join(j.Labels, ","))
						}
						continue
					}
					if reason != "" {
						res.ForkRunsRejected++
						if notice("fork:" + strconv.FormatInt(run.ID, 10)) {
							s.log().Warn("refusing to backfill a fork or privileged-trigger run", "event", logging.EventRejectFork,
								"source", "reconcile", "repo", repo.String(), "run_id", run.ID, "job_id", j.ID, "reason", reason,
								"run_event", run.Event, "head_repository", headRepo(&run))
						}
						continue
					}
					cands = append(cands, candidate{repo: repo, job: j})
				}
			}
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		a, b := cands[i].job, cands[j].job
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.ID < b.ID
	})
	return cands, onRunners, notices, listed
}

// runnerScopes returns one repo per distinct runner list: every repo with
// repo scope, one repo per owner with org scope.
func (s *Server) runnerScopes() []githubapp.Repo {
	seen := map[string]bool{}
	var out []githubapp.Repo
	for _, r := range s.o.TargetRepos {
		if k := s.scopeKey(r); !seen[k] {
			seen[k] = true
			out = append(out, r)
		}
	}
	return out
}

// reapRunners deletes offline, idle cr-job-* registrations of this stack
// (Options.StackLabel) older than ReapAfter, and those of other stacks once
// they have been offline for TaskTimeout + ReapAfter. Age is measured from the launch when this process launched the
// runner, otherwise from when reconcile first saw it offline. Runners whose
// claim is still pending (cold start) are kept.
func (s *Server) reapRunners(ctx context.Context, now time.Time, res *ReconcileResult, addErr func(string, error)) {
	stillOffline := map[string]bool{}
	for _, repo := range s.runnerScopes() {
		runners, err := s.o.GitHub.ListRunners(ctx, repo)
		if err != nil {
			addErr("list_runners "+repo.String(), err)
			for name := range s.offlineSeen {
				stillOffline[name] = true // keep first-seen times we could not re-check
			}
			continue
		}
		for _, rn := range runners {
			nameID, ok := parseRunnerName(rn.Name)
			if !ok || rn.Status != "offline" || rn.Busy {
				continue
			}
			// Another stack's registration may be one of its runners cold-starting,
			// so it gets that stack's whole task budget first. Offline for longer
			// than TASK_TIMEOUT + ReapAfter, it cannot belong to a live execution: a
			// leftover of a stack that was torn down (teardown.sh --regional-only),
			// which would otherwise block the backfill of its job's runner name.
			reapAfter := s.o.ReapAfter
			other := s.otherStack(rn)
			if other {
				reapAfter = s.o.TaskTimeout + s.o.ReapAfter
			}
			stillOffline[rn.Name] = true
			since, seen := s.offlineSeen[rn.Name]
			if !seen {
				since = now
				s.offlineSeen[rn.Name] = now
			}
			if rec := s.runners[rn.Name]; rec != nil && !other && rec.at.Before(since) {
				since = rec.at
			}
			if now.Sub(since) < reapAfter || (!other && s.o.Claims.Pending(nameID)) {
				continue
			}
			if err := s.o.GitHub.DeleteRunner(ctx, repo, rn.ID); err != nil {
				addErr("delete_runner "+rn.Name, err)
				continue
			}
			delete(s.offlineSeen, rn.Name)
			delete(s.runners, rn.Name)
			res.RunnersReaped++
			s.log().Warn("deleted stale offline runner registration", "event", logging.EventRunnerReaped,
				"repo", repo.String(), "runner", rn.Name, "runner_id", rn.ID, "offline_for", now.Sub(since).String(), "other_stack", other)
		}
	}
	for name := range s.offlineSeen {
		if !stillOffline[name] {
			delete(s.offlineSeen, name)
		}
	}
}
