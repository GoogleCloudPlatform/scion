// Package server implements the mediator's HTTP endpoints:
//
//	POST /webhook         GitHub workflow_job webhooks (HMAC-verified)
//	POST /internal/claim  runner exchanges its claim token for its JIT config
//	POST /reconcile       Cloud Scheduler (Google OIDC): reap and backfill
//	GET  /health          liveness
//
// State (claims, launched jobs, first-seen-offline times) lives in memory,
// so the service must run with max-instances=1.
package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/cr-ci/mediator/internal/claim"
	"github.com/GoogleCloudPlatform/scion/cr-ci/mediator/internal/githubapp"
	"github.com/GoogleCloudPlatform/scion/cr-ci/mediator/internal/launcher"
	"github.com/GoogleCloudPlatform/scion/cr-ci/mediator/internal/logging"
)

const (
	// RunnerPrefix prefixes every runner name the mediator registers.
	RunnerPrefix = "cr-job-"
	// WorkFolder is the runner work folder, on the /scratch ephemeral disk.
	WorkFolder = "/scratch/_work"

	maxWebhookBody       = 5 << 20
	secretRefetchBackoff = time.Minute
	defaultReapAfter     = 5 * time.Minute
)

// GitHub is the subset of the GitHub API the server needs
// (*githubapp.Client implements it).
type GitHub interface {
	GetRun(ctx context.Context, r githubapp.Repo, runID int64) (*githubapp.Run, error)
	ListRuns(ctx context.Context, r githubapp.Repo, status string) ([]githubapp.Run, error)
	ListJobs(ctx context.Context, r githubapp.Repo, runID int64) ([]githubapp.Job, error)
	ListRunners(ctx context.Context, r githubapp.Repo) ([]githubapp.Runner, error)
	GenerateJITConfig(ctx context.Context, r githubapp.Repo, req githubapp.JITRequest) (int64, string, error)
	DeleteRunner(ctx context.Context, r githubapp.Repo, runnerID int64) error
}

// Launcher starts, stops and counts runner executions.
type Launcher interface {
	launcher.RunnerLauncher
	launcher.ActiveLister
}

// TokenVerifier verifies the reconciler's Google ID token.
type TokenVerifier interface {
	Verify(ctx context.Context, token string) error
}

// Options configures a Server.
type Options struct {
	TargetRepos   []githubapp.Repo
	OrgScope      bool // runners are org-level: one runner list per owner
	RunnerLabel   string
	MaxConcurrent int
	RunnerGroupID int64
	// MediatorURL is the public https origin. While it is empty the mediator
	// is not yet fully deployed: /webhook and /reconcile answer 503.
	MediatorURL string
	TaskTimeout time.Duration
	ReapAfter   time.Duration // offline cr-job-* runners older than this are deleted (default 5m)

	// WebhookSecret fetches the webhook HMAC secret (Secret Manager).
	WebhookSecret func(ctx context.Context) ([]byte, error)
	GitHub        GitHub
	Launcher      Launcher
	Claims        *claim.Store
	OIDC          TokenVerifier
	Logger        *slog.Logger
	Now           func() time.Time
}

type launchRecord struct {
	repo     githubapp.Repo
	ref      string
	runnerID int64
	at       time.Time
}

// Server is the mediator HTTP server.
type Server struct {
	o Options

	secretMu      sync.Mutex
	secret        []byte
	secretFetched time.Time

	// dispatchMu serialises dispatch, cancellation and reconcile so that the
	// capacity and idempotency checks cannot race (single instance).
	dispatchMu  sync.Mutex
	launched    map[int64]launchRecord
	offlineSeen map[string]time.Time // runner name -> first seen offline
	forkLogged  map[int64]bool       // run IDs already logged as fork rejections by reconcile
}

// New returns a Server.
func New(o Options) *Server {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.ReapAfter == 0 {
		o.ReapAfter = defaultReapAfter
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &Server{
		o:           o,
		launched:    map[int64]launchRecord{},
		offlineSeen: map[string]time.Time{},
		forkLogged:  map[int64]bool{},
	}
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("POST /webhook", s.handleWebhook)
	mux.HandleFunc("POST /internal/claim", s.handleClaim)
	mux.HandleFunc("POST /reconcile", s.handleReconcile)
	return mux
}

func (s *Server) log() *slog.Logger { return s.o.Logger }

func plain(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, msg+"\n")
}

// RunnerName is the deterministic runner name for a GitHub job.
func RunnerName(jobID int64) string { return RunnerPrefix + strconv.FormatInt(jobID, 10) }

// ---- webhook -----------------------------------------------------------------

type webhookPayload struct {
	Action      string `json:"action"`
	WorkflowJob struct {
		ID         int64    `json:"id"`
		RunID      int64    `json:"run_id"`
		Labels     []string `json:"labels"`
		Conclusion string   `json:"conclusion"`
	} `json:"workflow_job"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	delivery := r.Header.Get("X-GitHub-Delivery")
	event := r.Header.Get("X-GitHub-Event")
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		s.log().Warn("webhook body unreadable", "event", logging.EventWebhookRejected, "delivery", delivery, "reason", "body")
		plain(w, http.StatusRequestEntityTooLarge, "body too large")
		return
	}
	ok, err := s.verifySignature(r.Context(), body, r.Header.Get("X-Hub-Signature-256"))
	if err != nil {
		s.log().Error("webhook secret unavailable", "event", logging.EventError, "delivery", delivery, "error", err.Error())
		plain(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	if !ok {
		s.log().Warn("webhook signature mismatch", "event", logging.EventWebhookRejected, "delivery", delivery, "reason", "signature")
		plain(w, http.StatusUnauthorized, "bad signature")
		return
	}
	if event != "workflow_job" {
		s.log().Info("webhook ignored", "event", logging.EventWebhookIgnored, "delivery", delivery, "github_event", event, "reason", "event_type")
		plain(w, http.StatusOK, "ignored")
		return
	}
	var p webhookPayload
	if err := json.Unmarshal(body, &p); err != nil || p.WorkflowJob.ID == 0 {
		s.log().Warn("webhook payload malformed", "event", logging.EventWebhookRejected, "delivery", delivery, "reason", "payload")
		plain(w, http.StatusBadRequest, "malformed payload")
		return
	}
	attrs := []any{"delivery", delivery, "action", p.Action, "job_id", p.WorkflowJob.ID, "run_id", p.WorkflowJob.RunID, "repo", p.Repository.FullName}
	if !hasLabel(p.WorkflowJob.Labels, s.o.RunnerLabel) {
		s.log().Debug("webhook ignored", append(attrs, "event", logging.EventWebhookIgnored, "reason", "label")...)
		plain(w, http.StatusOK, "ignored")
		return
	}
	repo, ok := s.targetRepo(p.Repository.FullName)
	if !ok {
		s.log().Warn("webhook rejected", append(attrs, "event", logging.EventWebhookRejected, "reason", "repo_not_allowed")...)
		plain(w, http.StatusOK, "ignored")
		return
	}
	if s.o.MediatorURL == "" {
		s.log().Error("MEDIATOR_URL is not set; cannot launch runners", append(attrs, "event", logging.EventError, "reason", "not_configured")...)
		plain(w, http.StatusServiceUnavailable, "not configured")
		return
	}

	switch p.Action {
	case "queued":
		s.onQueued(w, r.Context(), repo, p, attrs)
	case "completed":
		s.onCompleted(w, r.Context(), repo, p, attrs)
	default:
		plain(w, http.StatusOK, "ignored")
	}
}

func (s *Server) onQueued(w http.ResponseWriter, ctx context.Context, repo githubapp.Repo, p webhookPayload, attrs []any) {
	// Fork guard from the API, not the payload.
	run, err := s.o.GitHub.GetRun(ctx, repo, p.WorkflowJob.RunID)
	if err != nil {
		s.log().Error("fetching workflow run failed", append(attrs, "event", logging.EventError, "error", err.Error())...)
		plain(w, http.StatusServiceUnavailable, "github unavailable")
		return
	}
	if reason := forkReason(run, repo); reason != "" {
		s.log().Warn("refusing to launch a runner for a fork or pull_request_target run",
			append(attrs, "event", logging.EventRejectFork, "reason", reason, "run_event", run.Event, "head_repository", headRepo(run))...)
		plain(w, http.StatusOK, "rejected")
		return
	}
	out, err := s.dispatch(ctx, repo, p.WorkflowJob.ID, p.WorkflowJob.RunID, "webhook")
	if err != nil {
		plain(w, http.StatusServiceUnavailable, "dispatch failed")
		return
	}
	plain(w, http.StatusOK, string(out))
}

func (s *Server) onCompleted(w http.ResponseWriter, ctx context.Context, repo githubapp.Repo, p webhookPayload, attrs []any) {
	jobID := p.WorkflowJob.ID
	s.dispatchMu.Lock()
	defer s.dispatchMu.Unlock()
	rec, known := s.launched[jobID]
	delete(s.launched, jobID)
	s.o.Claims.DropJob(jobID)
	if p.WorkflowJob.Conclusion != "cancelled" {
		plain(w, http.StatusOK, "ok")
		return
	}
	var errs []error
	if err := s.o.Launcher.Terminate(ctx, jobID, rec.ref); err != nil {
		errs = append(errs, err)
	}
	runnerID := rec.runnerID
	if !known || runnerID == 0 {
		if rn, err := s.findRunner(ctx, repo, RunnerName(jobID)); err != nil {
			errs = append(errs, err)
		} else if rn != nil {
			runnerID = rn.ID
		}
	}
	if runnerID != 0 {
		if err := s.o.GitHub.DeleteRunner(ctx, repo, runnerID); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		s.log().Error("cancelling runner failed", append(attrs, "event", logging.EventError, "error", err.Error())...)
		plain(w, http.StatusServiceUnavailable, "cancel failed")
		return
	}
	s.log().Info("job cancelled; execution cancelled and registration removed", append(attrs, "event", logging.EventCancel, "execution", rec.ref)...)
	plain(w, http.StatusOK, "cancelled")
}

// verifySignature checks X-Hub-Signature-256 in constant time. On a mismatch
// it re-reads the secret (at most once a minute), so a rotated secret takes
// effect without a restart.
func (s *Server) verifySignature(ctx context.Context, body []byte, header string) (bool, error) {
	sigHex, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false, nil
	}
	sig, err := hex.DecodeString(sigHex)
	if err != nil || len(sig) != sha256.Size {
		return false, nil
	}
	check := func(secret []byte) bool {
		m := hmac.New(sha256.New, secret)
		m.Write(body)
		return hmac.Equal(m.Sum(nil), sig)
	}
	s.secretMu.Lock()
	defer s.secretMu.Unlock()
	if s.secret != nil && check(s.secret) {
		return true, nil
	}
	if s.secret != nil && s.o.Now().Sub(s.secretFetched) < secretRefetchBackoff {
		return false, nil
	}
	secret, err := s.o.WebhookSecret(ctx)
	if err != nil {
		if s.secret != nil {
			return false, nil
		}
		return false, err
	}
	s.secret, s.secretFetched = secret, s.o.Now()
	return check(secret), nil
}

func hasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if strings.EqualFold(l, want) {
			return true
		}
	}
	return false
}

func (s *Server) targetRepo(fullName string) (githubapp.Repo, bool) {
	for _, r := range s.o.TargetRepos {
		if strings.EqualFold(r.String(), fullName) {
			return r, true
		}
	}
	return githubapp.Repo{}, false
}

func headRepo(run *githubapp.Run) string {
	if run.HeadRepository == nil {
		return ""
	}
	return run.HeadRepository.FullName
}

// forkReason returns why run must not get a runner, or "" if it may.
func forkReason(run *githubapp.Run, repo githubapp.Repo) string {
	switch {
	case run.Event == "pull_request_target":
		return "pull_request_target"
	case run.Repository == nil || !strings.EqualFold(run.Repository.FullName, repo.String()):
		return "repository_mismatch"
	case run.HeadRepository == nil:
		return "head_repository_missing"
	case !strings.EqualFold(run.HeadRepository.FullName, run.Repository.FullName):
		return "head_repository_differs"
	}
	return ""
}

// ---- dispatch ----------------------------------------------------------------

type outcome string

const (
	outLaunched outcome = "launched"
	outSkipped  outcome = "already launched"
	outQueued   outcome = "capacity full; left queued"
)

// dispatch launches a runner for jobID unless one exists or capacity is full.
// The order is fixed: idempotency, capacity, mint, store claim, run.
func (s *Server) dispatch(ctx context.Context, repo githubapp.Repo, jobID, runID int64, source string) (outcome, error) {
	s.dispatchMu.Lock()
	defer s.dispatchMu.Unlock()
	return s.dispatchLocked(ctx, repo, jobID, runID, source)
}

func (s *Server) dispatchLocked(ctx context.Context, repo githubapp.Repo, jobID, runID int64, source string) (outcome, error) {
	name := RunnerName(jobID)
	attrs := []any{"source", source, "repo", repo.String(), "job_id", jobID, "run_id", runID, "runner", name}
	fail := func(stage string, err error) (outcome, error) {
		s.log().Error("dispatch failed", append(attrs, "event", logging.EventError, "stage", stage, "error", err.Error())...)
		return "", err
	}

	if _, ok := s.launched[jobID]; ok {
		s.log().Info("runner already launched", append(attrs, "event", logging.EventDispatchSkip, "reason", "launched")...)
		return outSkipped, nil
	}
	rn, err := s.findRunner(ctx, repo, name)
	if err != nil {
		return fail("list_runners", err)
	}
	if rn != nil {
		s.log().Info("runner already registered", append(attrs, "event", logging.EventDispatchSkip, "reason", "registered")...)
		return outSkipped, nil
	}
	active, err := s.o.Launcher.Active(ctx)
	if err != nil {
		return fail("list_executions", err)
	}
	for _, e := range active {
		if e.JobID == jobID {
			s.launched[jobID] = launchRecord{repo: repo, ref: e.Ref, at: s.o.Now()}
			s.log().Info("execution already running", append(attrs, "event", logging.EventDispatchSkip, "reason", "execution", "execution", e.Ref)...)
			return outSkipped, nil
		}
	}
	if len(active) >= s.o.MaxConcurrent {
		s.log().Info("capacity full; job left queued", append(attrs, "event", logging.EventCapacityFull, "active", len(active), "max", s.o.MaxConcurrent)...)
		return outQueued, nil
	}

	// Labels: only RUNNER_LABEL, deliberately without self-hosted/Linux/X64,
	// so a job that targets generic self-hosted labels can never be
	// assigned to this runner.
	runnerID, jit, err := s.o.GitHub.GenerateJITConfig(ctx, repo, githubapp.JITRequest{
		Name: name, RunnerGroupID: s.o.RunnerGroupID, Labels: []string{s.o.RunnerLabel}, WorkFolder: WorkFolder,
	})
	if err != nil {
		if runnerID != 0 {
			_ = s.o.GitHub.DeleteRunner(ctx, repo, runnerID)
		}
		return fail("generate_jitconfig", err)
	}
	token, err := s.o.Claims.Issue(jobID, jit)
	if err != nil {
		s.deleteAfterFailure(ctx, repo, runnerID, attrs)
		return fail("issue_claim", err)
	}
	ref, err := s.o.Launcher.Launch(ctx, launcher.LaunchSpec{
		JobID: jobID, RunID: runID, Repo: repo.String(), RunnerName: name, ClaimToken: token,
	})
	if err != nil {
		s.o.Claims.DropJob(jobID)
		s.deleteAfterFailure(ctx, repo, runnerID, attrs)
		s.log().Error("launching runner execution failed; registration removed", append(attrs, "event", logging.EventLaunchFailed, "error", err.Error())...)
		return "", err
	}
	s.launched[jobID] = launchRecord{repo: repo, ref: ref, runnerID: runnerID, at: s.o.Now()}
	s.log().Info("runner launched", append(attrs, "event", logging.EventDispatch, "execution", ref, "runner_id", runnerID, "active", len(active)+1)...)
	return outLaunched, nil
}

func (s *Server) deleteAfterFailure(ctx context.Context, repo githubapp.Repo, runnerID int64, attrs []any) {
	if err := s.o.GitHub.DeleteRunner(ctx, repo, runnerID); err != nil {
		// /reconcile reaps it once it has been offline long enough.
		s.log().Error("deleting runner registration after failure failed", append(attrs, "event", logging.EventError, "runner_id", runnerID, "error", err.Error())...)
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

// ---- claim -------------------------------------------------------------------

func (s *Server) handleClaim(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || len(token) != claim.TokenLen {
		s.log().Warn("claim rejected", "event", logging.EventClaimRejected, "reason", "malformed")
		plain(w, http.StatusGone, "gone")
		return
	}
	jit, jobID, ok := s.o.Claims.Consume(token)
	if !ok {
		s.log().Warn("claim rejected", "event", logging.EventClaimRejected, "reason", "unknown_expired_or_used")
		plain(w, http.StatusGone, "gone")
		return
	}
	b, err := json.Marshal(struct {
		JITConfig string `json:"jit_config"`
	}{jit})
	if err != nil {
		plain(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
	s.log().Info("claim consumed", "event", logging.EventClaimConsumed, "job_id", jobID, "runner", RunnerName(jobID))
}

// ---- reconcile ---------------------------------------------------------------

// ReconcileResult is the /reconcile response body.
type ReconcileResult struct {
	ClaimsExpired      int      `json:"claims_expired"`
	RunnersReaped      int      `json:"runners_reaped"`
	ExecutionsReaped   int      `json:"executions_reaped"`
	Backfilled         int      `json:"backfilled"`
	LeftQueued         int      `json:"left_queued"`
	ForkRunsRejected   int      `json:"fork_runs_rejected"`
	Errors             []string `json:"errors,omitempty"`
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
	if err := s.o.OIDC.Verify(r.Context(), token); err != nil {
		s.log().Warn("reconcile denied", "event", logging.EventReconcileDenied, "reason", err.Error())
		plain(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	res := s.Reconcile(r.Context())
	status := http.StatusOK
	if len(res.Errors) > 0 {
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(res)
}

// Reconcile expires claims, reaps stale registrations and executions, and
// backfills queued labelled jobs that have no runner, within capacity.
func (s *Server) Reconcile(ctx context.Context) ReconcileResult {
	s.dispatchMu.Lock()
	defer s.dispatchMu.Unlock()
	var res ReconcileResult
	addErr := func(stage string, err error) {
		res.Errors = append(res.Errors, stage+": "+err.Error())
		s.log().Error("reconcile step failed", "event", logging.EventError, "stage", stage, "error", err.Error())
	}
	now := s.o.Now()

	res.ClaimsExpired = s.o.Claims.Sweep()
	for id, rec := range s.launched {
		if now.Sub(rec.at) > s.o.TaskTimeout+s.o.ReapAfter {
			delete(s.launched, id)
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

	s.backfill(ctx, &res, addErr)

	s.log().Info("reconcile finished", "event", logging.EventReconcile,
		"claims_expired", res.ClaimsExpired, "runners_reaped", res.RunnersReaped, "executions_reaped", res.ExecutionsReaped,
		"backfilled", res.Backfilled, "left_queued", res.LeftQueued, "fork_runs_rejected", res.ForkRunsRejected, "errors", len(res.Errors))
	return res
}

// runnerScopes returns one repo per distinct runner list: every repo with
// repo scope, one repo per owner with org scope.
func (s *Server) runnerScopes() []githubapp.Repo {
	if !s.o.OrgScope {
		return s.o.TargetRepos
	}
	seen := map[string]bool{}
	var out []githubapp.Repo
	for _, r := range s.o.TargetRepos {
		k := strings.ToLower(r.Owner)
		if !seen[k] {
			seen[k] = true
			out = append(out, r)
		}
	}
	return out
}

// reapRunners deletes offline cr-job-* registrations older than ReapAfter.
// Age is measured from the launch when this process launched the runner,
// otherwise from when reconcile first saw it offline. Runners whose claim is
// still pending (cold start) are kept.
func (s *Server) reapRunners(ctx context.Context, now time.Time, res *ReconcileResult, addErr func(string, error)) {
	stillOffline := map[string]bool{}
	for _, repo := range s.runnerScopes() {
		runners, err := s.o.GitHub.ListRunners(ctx, repo)
		if err != nil {
			addErr("list_runners "+repo.String(), err)
			// Keep first-seen times we could not re-check.
			for name := range s.offlineSeen {
				stillOffline[name] = true
			}
			continue
		}
		for _, rn := range runners {
			jobID, ok := parseRunnerName(rn.Name)
			if !ok || rn.Status != "offline" || rn.Busy {
				continue
			}
			stillOffline[rn.Name] = true
			since, seen := s.offlineSeen[rn.Name]
			if !seen {
				since = now
				s.offlineSeen[rn.Name] = now
			}
			if rec, ok := s.launched[jobID]; ok && rec.at.Before(since) {
				since = rec.at
			}
			if now.Sub(since) < s.o.ReapAfter || s.o.Claims.Pending(jobID) {
				continue
			}
			if err := s.o.GitHub.DeleteRunner(ctx, repo, rn.ID); err != nil {
				addErr("delete_runner "+rn.Name, err)
				continue
			}
			delete(s.offlineSeen, rn.Name)
			delete(s.launched, jobID)
			res.RunnersReaped++
			s.log().Warn("deleted stale offline runner registration", "event", logging.EventRunnerReaped,
				"repo", repo.String(), "runner", rn.Name, "runner_id", rn.ID, "offline_for", now.Sub(since).String())
		}
	}
	for name := range s.offlineSeen {
		if !stillOffline[name] {
			delete(s.offlineSeen, name)
		}
	}
}

func parseRunnerName(name string) (int64, bool) {
	rest, ok := strings.CutPrefix(name, RunnerPrefix)
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	return id, err == nil && id > 0
}

type candidate struct {
	repo    githubapp.Repo
	job     githubapp.Job
	created time.Time
}

// backfill dispatches queued labelled jobs, oldest first, until capacity is
// full.
func (s *Server) backfill(ctx context.Context, res *ReconcileResult, addErr func(string, error)) {
	var cands []candidate
	for _, repo := range s.o.TargetRepos {
		for _, status := range []string{"queued", "in_progress"} {
			runs, err := s.o.GitHub.ListRuns(ctx, repo, status)
			if err != nil {
				addErr("list_runs "+repo.String(), err)
				continue
			}
			for i := range runs {
				run := &runs[i]
				jobs, err := s.o.GitHub.ListJobs(ctx, repo, run.ID)
				if err != nil {
					addErr("list_jobs "+repo.String(), err)
					continue
				}
				var labelled []githubapp.Job
				for _, j := range jobs {
					if j.Status == "queued" && hasLabel(j.Labels, s.o.RunnerLabel) {
						labelled = append(labelled, j)
					}
				}
				if len(labelled) == 0 {
					continue
				}
				if reason := forkReason(run, repo); reason != "" {
					res.ForkRunsRejected++
					if !s.forkLogged[run.ID] {
						s.forkLogged[run.ID] = true
						s.log().Warn("refusing to backfill a fork or pull_request_target run", "event", logging.EventRejectFork,
							"source", "reconcile", "repo", repo.String(), "run_id", run.ID, "reason", reason, "run_event", run.Event, "head_repository", headRepo(run))
					}
					continue
				}
				for _, j := range labelled {
					cands = append(cands, candidate{repo: repo, job: j, created: j.CreatedAt})
				}
			}
		}
	}
	cands = sortCandidates(cands)
	for i, c := range cands {
		out, err := s.dispatchLocked(ctx, c.repo, c.job.ID, c.job.RunID, "reconcile")
		switch {
		case err != nil:
			addErr("dispatch", err)
		case out == outLaunched:
			res.Backfilled++
		case out == outQueued:
			res.LeftQueued = len(cands) - i
			res.CapacityFullAtStop = true
			return
		}
	}
}

// sortCandidates orders oldest first (FIFO) and drops duplicate job IDs (a
// run can move from queued to in_progress between the two list calls).
func sortCandidates(c []candidate) []candidate {
	sort.Slice(c, func(i, j int) bool {
		if !c[i].created.Equal(c[j].created) {
			return c[i].created.Before(c[j].created)
		}
		return c[i].job.ID < c[j].job.ID
	})
	out := c[:0]
	seen := map[int64]bool{}
	for _, x := range c {
		if !seen[x.job.ID] {
			seen[x.job.ID] = true
			out = append(out, x)
		}
	}
	return out
}
