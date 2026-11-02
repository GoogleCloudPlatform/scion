package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/cr-ci/mediator/internal/githubapp"
	"github.com/GoogleCloudPlatform/scion/cr-ci/mediator/internal/launcher"
	"github.com/GoogleCloudPlatform/scion/cr-ci/mediator/internal/logging"
)

type outcome string

const (
	outLaunched outcome = "launched"
	outSkipped  outcome = "already served"
	outHasName  outcome = "already has a runner" // a runner with the requested name is registered
	outNotQueue outcome = "job not queued"
	outQueued   outcome = "capacity full; left queued"
)

// view caches the runner lists and active executions for a sequence of
// dispatches under one lock hold (reconcile), so each is fetched once.
type view struct {
	runners   map[string]map[string]githubapp.Runner // scope key -> name -> runner
	execs     []launcher.Execution
	haveExecs bool
}

func newView() *view { return &view{runners: map[string]map[string]githubapp.Runner{}} }

func (s *Server) runnersIn(ctx context.Context, v *view, repo githubapp.Repo) (map[string]githubapp.Runner, error) {
	key := s.scopeKey(repo)
	if m, ok := v.runners[key]; ok {
		return m, nil
	}
	list, err := s.o.GitHub.ListRunners(ctx, repo)
	if err != nil {
		return nil, err
	}
	m := make(map[string]githubapp.Runner, len(list))
	for _, r := range list {
		m[r.Name] = r
	}
	v.runners[key] = m
	return m, nil
}

func (s *Server) execsIn(ctx context.Context, v *view) ([]launcher.Execution, error) {
	if !v.haveExecs {
		a, err := s.o.Launcher.Executions(ctx)
		if err != nil {
			return nil, err
		}
		v.execs, v.haveExecs = a, true
	}
	return v.execs, nil
}

// live returns the running executions, minus those this process already
// cancelled (they stay running for a moment), and how many launched
// executions the list does not show yet.
func (s *Server) live(execs []launcher.Execution) (live []launcher.Execution, unlisted int) {
	listed := map[string]bool{}
	for _, e := range execs {
		listed[e.Ref] = true
		if _, gone := s.cancelled[e.Ref]; !gone && !e.Done {
			live = append(live, e)
		}
	}
	now := s.o.Now()
	for _, rec := range s.runners {
		if now.Sub(rec.at) < launchListLag && !listed[rec.ref] {
			if _, gone := s.cancelled[rec.ref]; !gone {
				unlisted++
			}
		}
	}
	return live, unlisted
}

// dispatch serves job `serve` with a runner named after it.
func (s *Server) dispatch(ctx context.Context, repo githubapp.Repo, serve, runID int64, source string) (outcome, error) {
	s.dispatchMu.Lock()
	defer s.dispatchMu.Unlock()
	return s.dispatchLocked(ctx, newView(), repo, serve, runID, serve, source)
}

// dispatchLocked launches a runner named cr-job-<nameID> to serve job
// `serve`, unless the job is already served, capacity is full or the job is
// no longer queued. nameID differs from serve only when the job's own name
// is held by a runner that GitHub gave to another job (swap, hijack).
// The order is fixed: idempotency, capacity, still-queued, mint, claim, run.
// The caller holds dispatchMu.
func (s *Server) dispatchLocked(ctx context.Context, v *view, repo githubapp.Repo, serve, runID, nameID int64, source string) (outcome, error) {
	name := RunnerName(nameID)
	attrs := []any{"source", source, "repo", repo.String(), "job_id", serve, "run_id", runID, "runner", name}
	fail := func(stage string, err error) (outcome, error) {
		s.log().Error("dispatch failed", append(attrs, "event", logging.EventError, "stage", stage, "error", err.Error())...)
		return "", err
	}
	skip := func(reason string, out outcome, extra ...any) (outcome, error) {
		s.log().Info("dispatch skipped", append(append(attrs, "event", logging.EventDispatchSkip, "reason", reason), extra...)...)
		return out, nil
	}

	// Idempotency rests on GitHub and Cloud Run state, which survives
	// restarts. Memory only marks runners that were reassigned to another
	// job (they no longer serve the job they were launched for).
	runners, err := s.runnersIn(ctx, v, repo)
	if err != nil {
		return fail("list_runners", err)
	}
	if _, ok := runners[name]; ok {
		return skip("registered", outHasName)
	}
	execs, err := s.execsIn(ctx, v)
	if err != nil {
		return fail("list_executions", err)
	}
	live, unlisted := s.live(execs)
	for _, e := range live {
		if e.RunnerName == name {
			return skip("execution", outSkipped, "execution", e.Ref)
		}
		if e.JobID == serve {
			if rec := s.runners[e.RunnerName]; rec == nil || !rec.reassigned {
				return skip("execution", outSkipped, "execution", e.Ref)
			}
		}
	}
	if used := len(live) + unlisted; used >= s.o.MaxConcurrent {
		s.log().Info("capacity full; job left queued", append(attrs, "event", logging.EventCapacityFull, "active", used, "max", s.o.MaxConcurrent)...)
		return outQueued, nil
	}
	// A redelivered or replayed event for a finished job must not mint an
	// orphan runner.
	job, err := s.o.GitHub.GetJob(ctx, repo, serve)
	if err != nil {
		return fail("get_job", err)
	}
	if job.Status != "queued" {
		return skip("not_queued", outNotQueue, "job_status", job.Status)
	}

	// Labels: RUNNER_LABEL (the only one jobs may ask for, see servable) and
	// the stack label (reap scoping, see Options.StackLabel).
	labels := []string{s.o.RunnerLabel}
	if s.o.StackLabel != "" {
		labels = append(labels, s.o.StackLabel)
	}
	runnerID, jit, err := s.o.GitHub.GenerateJITConfig(ctx, repo, githubapp.JITRequest{
		Name: name, RunnerGroupID: s.o.RunnerGroupID, Labels: labels, WorkFolder: WorkFolder,
	})
	if err != nil {
		if runnerID != 0 {
			s.deleteAfterFailure(repo, runnerID, attrs)
		}
		return fail("generate_jitconfig", err)
	}
	token, err := s.o.Claims.Issue(nameID, jit)
	if err != nil {
		s.deleteAfterFailure(repo, runnerID, attrs)
		return fail("issue_claim", err)
	}
	ref, err := s.o.Launcher.Launch(ctx, launcher.LaunchSpec{
		JobID: serve, RunID: runID, Repo: repo.String(), ServedRepos: s.servedRepos(repo), RunnerName: name, ClaimToken: token,
	})
	if err != nil {
		s.o.Claims.DropJob(nameID)
		s.deleteAfterFailure(repo, runnerID, attrs)
		s.log().Error("launching runner execution failed; registration removed", append(attrs, "event", logging.EventLaunchFailed, "error", err.Error())...)
		return "", err
	}
	now := s.o.Now()
	s.runners[name] = &runnerRecord{repo: repo, serve: serve, runID: runID, ref: ref, runnerID: runnerID, at: now}
	runners[name] = githubapp.Runner{ID: runnerID, Name: name, Status: "offline"}
	v.execs = append(v.execs, launcher.Execution{Ref: ref, JobID: serve, RunnerName: name, Created: now})
	s.log().Info("runner launched", append(attrs, "event", logging.EventDispatch, "execution", ref, "runner_id", runnerID, "active", len(live)+unlisted+1)...)
	return outLaunched, nil
}

// deleteAfterFailure removes a registration on a fresh context, so the
// cleanup runs even when the failed step ran out of time.
func (s *Server) deleteAfterFailure(repo githubapp.Repo, runnerID int64, attrs []any) {
	ctx, cancel := cleanupContext()
	defer cancel()
	if err := s.o.GitHub.DeleteRunner(ctx, repo, runnerID); err != nil {
		// /reconcile reaps it once it has been offline long enough.
		s.log().Error("deleting runner registration after failure failed", append(attrs, "event", logging.EventError, "runner_id", runnerID, "error", err.Error())...)
	}
}

type stopResult struct {
	busy                bool // registration busy and left alone (requireIdle)
	registrationDeleted bool // registration deleted or already absent
	// registrationErr is GitHub's refusal when a hijacked (not requireIdle) runner's
	// registration could not be deleted because it is busy (422): GitHub keeps it
	// until it times the untrusted job out, about 10 minutes (D11 live test).
	registrationErr string
	ref             string
}

// stopRunner deletes runner `name`'s registration and cancels its
// execution. With requireIdle, a busy runner (running some job) is left
// alone. Without it (hijack), the execution is cancelled even if GitHub
// refuses to delete the busy registration. Network calls use a fresh
// context. The caller holds dispatchMu.
func (s *Server) stopRunner(repo githubapp.Repo, name string, requireIdle bool) (stopResult, error) {
	ctx, cancel := cleanupContext()
	defer cancel()
	var res stopResult
	rec := s.runners[name]
	if rec != nil && rec.repo != (githubapp.Repo{}) {
		repo = rec.repo
	}
	var errs []error
	if repo != (githubapp.Repo{}) {
		rn, err := s.findRunner(ctx, repo, name)
		switch {
		case err != nil:
			errs = append(errs, err)
		case rn == nil:
			res.registrationDeleted = true
		case requireIdle && rn.Busy:
			res.busy = true
			return res, nil
		default:
			err := s.o.GitHub.DeleteRunner(ctx, repo, rn.ID)
			switch {
			case err == nil:
				res.registrationDeleted = true
			case githubapp.IsStatus(err, http.StatusUnprocessableEntity):
				// Busy: it picked up a job meanwhile.
				if requireIdle {
					res.busy = true
					return res, nil
				}
				res.registrationErr = err.Error()
			default:
				errs = append(errs, err)
			}
		}
	}
	if rec != nil {
		res.ref = rec.ref
	}
	if res.ref == "" {
		if execs, err := s.o.Launcher.Executions(ctx); err != nil {
			errs = append(errs, err)
		} else {
			for _, e := range execs {
				if e.RunnerName == name && !e.Done {
					res.ref = e.Ref
				}
			}
		}
	}
	if res.ref != "" {
		if err := s.o.Launcher.Terminate(ctx, name, res.ref); err != nil {
			errs = append(errs, err)
		} else {
			s.cancelled[res.ref] = s.o.Now()
		}
	}
	delete(s.runners, name)
	if id, ok := parseRunnerName(name); ok {
		s.o.Claims.DropJob(id)
	}
	return res, errors.Join(errs...)
}

// stopUnusedRunners handles a job that finished without running on a cr-ci
// runner (cancelled or skipped while queued). The runners launched for it
// are stopped only if idle: a busy one is running another job.
func (s *Server) stopUnusedRunners(ctx context.Context, repo githubapp.Repo, jobID int64, attrs []any) error {
	s.dispatchMu.Lock()
	defer s.dispatchMu.Unlock()
	names := map[string]bool{RunnerName(jobID): true}
	for name, rec := range s.runners {
		if rec.serve == jobID && !rec.reassigned {
			names[name] = true
		}
	}
	var errs []error
	for name := range names {
		res, err := s.stopRunner(repo, name, true)
		switch {
		case err != nil:
			errs = append(errs, err)
			s.log().Error("stopping unused runner failed", append(attrs, "event", logging.EventError, "stopped_runner", name, "error", err.Error())...)
		case res.busy:
			s.log().Info("runner launched for the job is running another job; left alone", append(attrs, "event", logging.EventCancel, "stopped_runner", name, "action", "none_busy")...)
		case res.ref != "" || res.registrationDeleted:
			s.log().Info("job finished while queued; its idle runner was stopped", append(attrs, "event", logging.EventCancel, "stopped_runner", name, "execution", res.ref)...)
		}
	}
	return errors.Join(errs...)
}

// forgetRunner drops the record and claim of a runner whose job finished.
func (s *Server) forgetRunner(name string) {
	s.dispatchMu.Lock()
	defer s.dispatchMu.Unlock()
	delete(s.runners, name)
	s.forgotten[name] = s.o.Now()
	if id, ok := parseRunnerName(name); ok {
		s.o.Claims.DropJob(id)
	}
}

func (s *Server) findRunner(ctx context.Context, repo githubapp.Repo, name string) (*githubapp.Runner, error) {
	runners, err := s.o.GitHub.ListRunners(ctx, repo)
	if err != nil {
		return nil, err
	}
	for i := range runners {
		if runners[i].Name == name {
			return &runners[i], nil
		}
	}
	return nil, nil
}
