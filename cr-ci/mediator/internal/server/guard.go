package server

import (
	"context"

	"github.com/GoogleCloudPlatform/scion/cr-ci/mediator/internal/githubapp"
	"github.com/GoogleCloudPlatform/scion/cr-ci/mediator/internal/logging"
)

// checkRunnerJob is the hijack guard. It runs when GitHub reports job jobID
// in progress (or completed) on the cr-ci runner runnerName, from a webhook
// or from reconcile. GitHub gives a queued job to any idle runner whose
// labels match, so the runner may run another job than the one it was
// launched to serve. If so, that job is checked with the fork guard (a repo
// outside TARGET_REPOS, or a failed run lookup, also fails: fail closed):
//
//   - fails (hijack): the runner's execution is cancelled and its
//     registration deleted, then the job it was launched for is served again;
//   - passes (swap): harmless; the job that took the runner gets a runner
//     named for it if it has none, and that runner serves the displaced job.
//
// run may be nil (it is fetched). The result is a short status for the
// HTTP response.
func (s *Server) checkRunnerJob(ctx context.Context, repoFull string, jobID, runID int64, runnerName string, run *githubapp.Run, source string) (string, error) {
	nameID, ok := parseRunnerName(runnerName)
	if !ok {
		return "ok", nil
	}
	s.dispatchMu.Lock()
	defer s.dispatchMu.Unlock()
	rec := s.runners[runnerName]
	served := nameID // after a restart, assume the runner serves its name's job
	if rec != nil {
		served = rec.serve
	}
	if jobID == served {
		return "ok", nil // that job passed the fork guard at dispatch
	}
	attrs := []any{"source", source, "repo", repoFull, "job_id", jobID, "run_id", runID, "runner", runnerName, "served_job_id", served}

	repo, targeted := s.targetRepo(repoFull)
	reason := ""
	switch {
	case !targeted:
		reason = "repo_not_allowed"
	case run == nil:
		r, err := s.o.GitHub.GetRun(ctx, repo, runID)
		if err != nil {
			s.log().Error("fetching workflow run failed", append(attrs, "event", logging.EventError, "error", err.Error())...)
			reason = "run_lookup_failed"
		} else {
			run = r
			reason = forkReason(run, repo)
		}
	default:
		reason = forkReason(run, repo)
	}

	serveRepo := s.runnerRepo(repoFull)
	if rec != nil {
		serveRepo = rec.repo
	}
	if reason == "" {
		if rec == nil {
			rec = &runnerRecord{repo: repo, serve: served, at: s.o.Now()}
			s.runners[runnerName] = rec
		}
		if !rec.reassigned {
			rec.reassigned = true
			s.log().Info("runner picked up another legitimate job", append(attrs, "event", logging.EventRunnerSwap)...)
		}
		// The displaced job needs a runner; the job that took this runner
		// owns a free name for it. If the taker already has a runner, that
		// one is now idle and serves the displaced job: rebind its record,
		// so that cancelling the displaced job while queued stops it.
		out, err := s.dispatchLocked(ctx, newView(), serveRepo, served, rec.runID, jobID, "swap")
		if err != nil {
			return "", err
		}
		if out == outHasName {
			taker := RunnerName(jobID)
			switch tr := s.runners[taker]; {
			case tr == nil:
				s.runners[taker] = &runnerRecord{repo: serveRepo, serve: served, runID: rec.runID, at: s.o.Now()}
			case !tr.reassigned:
				tr.serve, tr.runID = served, rec.runID
			}
		}
		return "swap", nil
	}

	kill := append(attrs, "event", logging.EventKillHijacked, "reason", reason)
	if run != nil {
		kill = append(kill, "run_event", run.Event, "head_repository", headRepo(run))
	}
	servedRunID := int64(0)
	if rec != nil {
		servedRunID = rec.runID
	}
	res, err := s.stopRunner(serveRepo, runnerName, false)
	if err != nil {
		s.log().Error("runner hijacked by an untrusted job; stopping it failed", append(kill, "error", err.Error())...)
		return "", err
	}
	s.log().Error("runner hijacked by an untrusted job; execution cancelled",
		append(kill, "execution", res.ref, "registration_deleted", res.registrationDeleted)...)

	// Serve the displaced job again, with a runner named after the
	// hijacking job (which never gets a runner of its own). Reusing the
	// displaced runner's name would collide with its registration if GitHub
	// refused to delete it (busy), and the hijacking job's later events,
	// which carry that name, would then hit the replacement.
	if serveRepo == (githubapp.Repo{}) {
		return "killed", nil // reconcile backfills it
	}
	if _, err := s.dispatchLocked(ctx, newView(), serveRepo, served, servedRunID, jobID, "hijack_recovery"); err != nil {
		s.log().Error("re-serving the displaced job failed; reconcile will retry", append(attrs, "event", logging.EventError, "error", err.Error())...)
	}
	return "killed", nil
}
