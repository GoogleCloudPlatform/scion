// Package server implements the mediator's HTTP endpoints:
//
//	POST /webhook         GitHub workflow_job webhooks (HMAC-verified)
//	POST /internal/claim  runner exchanges its claim token for its JIT config
//	                      (Google ID token of the runner identity required)
//	POST /reconcile       Cloud Scheduler (Google ID token): reap and backfill
//	GET  /health          liveness
//
// State (claims, runner records, first-seen-offline times) lives in memory,
// so the service must run with max-instances=1.
//
// Runners and jobs are not one to one. GitHub gives a queued job to any idle
// runner whose labels match, so the runner cr-job-X launched for job X may run
// job Y. The mediator therefore keys its state on the runner name and learns
// which job a runner actually runs from workflow_job.runner_name.
package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/cr-ci/mediator/internal/claim"
	"github.com/GoogleCloudPlatform/scion/cr-ci/mediator/internal/githubapp"
	"github.com/GoogleCloudPlatform/scion/cr-ci/mediator/internal/launcher"
)

const (
	// RunnerPrefix prefixes every runner name the mediator registers.
	RunnerPrefix = "cr-job-"
	// WorkFolder is the runner work folder sent in generate-jitconfig. It is
	// the same path as on GitHub-hosted ubuntu-latest, because actions/cache
	// stores paths relative to GITHUB_WORKSPACE (under the work folder): only
	// an identical work folder lets cache entries restore to the same place in
	// both directions. On the cr-ci runner it is a real directory on the
	// ephemeral disk, which the job mounts at HOME (cr-ci cache parity
	// contract, item 2), so GITHUB_WORKSPACE is /home/runner/work/<repo>/<repo>,
	// physically too.
	WorkFolder = "/home/runner/work"

	maxWebhookBody          = 5 << 20
	secretRefetchBackoff    = time.Minute      // between refetches while a secret is cached
	secretRetryBackoff      = 10 * time.Second // between attempts while none is cached
	secretGrace             = time.Hour        // a replaced webhook secret stays valid this long
	defaultReapAfter        = 5 * time.Minute
	defaultRequestTimeout   = 60 * time.Second
	defaultReconcileTimeout = 4 * time.Minute
	cleanupTimeout          = 30 * time.Second
	// launchListLag covers executions that jobs.run created but
	// executions.list does not show yet.
	launchListLag = 2 * time.Minute
	cancelledTTL  = time.Hour
	// runMetaMaxEntries and runMetaTTL bound the in-memory cache of immutable
	// run metadata (event, repository, head_repository) used by forkReason.
	runMetaMaxEntries = 10000
	runMetaTTL        = 24 * time.Hour
	// unverifiedMax bounds how long a transient GetRun failure can defer a
	// hijack check before failing closed.
	unverifiedMax = 15 * time.Minute
)

// GitHub is the subset of the GitHub API the server needs
// (*githubapp.Client implements it).
type GitHub interface {
	GetRun(ctx context.Context, r githubapp.Repo, runID int64) (*githubapp.Run, error)
	GetJob(ctx context.Context, r githubapp.Repo, jobID int64) (*githubapp.Job, error)
	ListRuns(ctx context.Context, r githubapp.Repo, status string) ([]githubapp.Run, error)
	ListJobs(ctx context.Context, r githubapp.Repo, runID int64) ([]githubapp.Job, error)
	ListRunners(ctx context.Context, r githubapp.Repo) ([]githubapp.Runner, error)
	GenerateJITConfig(ctx context.Context, r githubapp.Repo, req githubapp.JITRequest) (int64, string, error)
	DeleteRunner(ctx context.Context, r githubapp.Repo, runnerID int64) error
	RateLimit(r githubapp.Repo) (githubapp.RateLimit, bool)
}

// Launcher starts, stops and counts runner executions.
type Launcher interface {
	launcher.RunnerLauncher
	launcher.ExecutionLister
}

// TokenVerifier verifies a Google ID token.
type TokenVerifier interface {
	Verify(ctx context.Context, token string) error
}

// Options configures a Server.
type Options struct {
	TargetRepos []githubapp.Repo
	OrgScope    bool // runners are org-level: one runner list per owner
	RunnerLabel string
	// StackLabel is an extra label on every runner this stack registers
	// (StackLabel(); region plus job name, unique per project). Reconcile
	// reaps registrations with this label, or with no stack label at all,
	// after ReapAfter; another stack's only after TaskTimeout + ReapAfter
	// offline, so two stacks that serve the same repo (a region move) do not
	// reap each other's live runners. Empty: no extra label.
	StackLabel    string
	MaxConcurrent int
	RunnerGroupID int64
	// MediatorURL is the public https origin. While it is empty the mediator
	// is not yet fully deployed: /webhook and /reconcile answer 503.
	MediatorURL string
	TaskTimeout time.Duration
	ReapAfter   time.Duration // offline cr-job-* runners older than this are deleted (default 5m)

	RequestTimeout   time.Duration // budget for webhook work, detached from the delivery (default 60s)
	ReconcileTimeout time.Duration // budget for one reconcile (default 4m)

	// WebhookSecret fetches the webhook HMAC secret (Secret Manager).
	WebhookSecret func(ctx context.Context) ([]byte, error)
	GitHub        GitHub
	Launcher      Launcher
	Claims        *claim.Store
	// ReconcileOIDC verifies the Cloud Scheduler identity on /reconcile.
	ReconcileOIDC TokenVerifier
	// RunnerOIDC verifies the runner identity (EXEC_SA_NAME) on /internal/claim.
	RunnerOIDC TokenVerifier
	Logger     *slog.Logger
	Now        func() time.Time
}

// runnerRecord is what this process knows about a runner it launched.
type runnerRecord struct {
	repo     githubapp.Repo
	serve    int64 // the job it was launched to serve (GH_JOB_ID)
	runID    int64
	ref      string
	runnerID int64
	at       time.Time
	// reassigned: GitHub gave the runner another legitimate job, so it no
	// longer counts as serving `serve`.
	reassigned bool
}

type runMetaEntry struct {
	run githubapp.Run
	at  time.Time
}

type unverifiedRec struct {
	repo     string
	jobID    int64
	runID    int64
	runnerID int64
	since    time.Time
}

// Server is the mediator HTTP server.
type Server struct {
	o Options

	// secretMu guards the secret fields and is never held across a network
	// call; secretFetchMu makes Secret Manager refetches single-flight.
	secretMu      sync.Mutex
	secretFetchMu sync.Mutex
	secret        []byte
	prevSecret    []byte
	prevUntil     time.Time
	secretAttempt time.Time

	// runMetaMu guards runMeta, an in-memory cache of immutable workflow-run
	// verdict inputs (event, repository, head_repository).
	runMetaMu sync.Mutex
	runMeta   map[int64]runMetaEntry

	// reconcileMu makes reconcile passes single-flight. /reconcile uses TryLock
	// so overlapping scheduler ticks skip cleanly; Server.Reconcile blocks.
	// lastRunnerRuns is guarded by reconcileMu and records run IDs that held an
	// in_progress job on a cr-ci runner in the previous pass (P0 priority).
	reconcileMu    sync.Mutex
	lastRunnerRuns map[int64]bool

	// dispatchMu serialises dispatch, runner stops and the mutating part of
	// reconcile so that capacity and idempotency checks cannot race (single
	// instance).
	dispatchMu  sync.Mutex
	runners     map[string]*runnerRecord // runner name -> record
	cancelled   map[string]time.Time     // execution refs this process cancelled
	offlineSeen map[string]time.Time     // runner name -> first seen offline
	noticed     map[string]bool          // reconcile notices already logged (pruned each pass)
	// hijacked: runners killed as hijacked whose registration GitHub has not let go
	// yet (busy while it still shows the untrusted job running), -> first kill time.
	// Later kills of the same runner log at WARNING; cleared once the registration is
	// gone, or by reconcile after cancelledTTL.
	hijacked map[string]time.Time
	// forgotten: runners whose job's completed webhook dropped their record, -> when.
	// Reconcile skips them (its discovery may predate the webhook); pruned after
	// cancelledTTL.
	forgotten map[string]time.Time
	// unverified: runners whose hijack-guard GetRun lookup hit a transient error
	// and was deferred; retried by reconcile and bounded by unverifiedMax.
	unverified map[string]unverifiedRec
}

// New returns a Server.
func New(o Options) *Server {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.ReapAfter == 0 {
		o.ReapAfter = defaultReapAfter
	}
	if o.RequestTimeout == 0 {
		o.RequestTimeout = defaultRequestTimeout
	}
	if o.ReconcileTimeout == 0 {
		o.ReconcileTimeout = defaultReconcileTimeout
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return &Server{
		o:              o,
		runMeta:        map[int64]runMetaEntry{},
		lastRunnerRuns: map[int64]bool{},
		runners:        map[string]*runnerRecord{},
		cancelled:      map[string]time.Time{},
		offlineSeen:    map[string]time.Time{},
		noticed:        map[string]bool{},
		hijacked:       map[string]time.Time{},
		forgotten:      map[string]time.Time{},
		unverified:     map[string]unverifiedRec{},
	}
}

func cloneRunMeta(r githubapp.Run) githubapp.Run {
	cp := githubapp.Run{
		ID:        r.ID,
		Event:     r.Event,
		CreatedAt: r.CreatedAt,
		// Status is mutable and intentionally omitted from cached verdict metadata.
	}
	if r.Repository != nil {
		ref := *r.Repository
		cp.Repository = &ref
	}
	if r.HeadRepository != nil {
		ref := *r.HeadRepository
		cp.HeadRepository = &ref
	}
	return cp
}

// rememberRun caches immutable run metadata (event, repository, head_repository)
// for run r.ID, evicting entries older than runMetaTTL and bounding size at
// runMetaMaxEntries.
func (s *Server) rememberRun(r githubapp.Run) {
	if r.ID == 0 {
		return
	}
	now := s.o.Now()
	cp := cloneRunMeta(r)
	s.runMetaMu.Lock()
	defer s.runMetaMu.Unlock()
	if _, exists := s.runMeta[r.ID]; !exists && len(s.runMeta) >= runMetaMaxEntries {
		for id, e := range s.runMeta {
			if now.Sub(e.at) > runMetaTTL {
				delete(s.runMeta, id)
			}
		}
		if len(s.runMeta) >= runMetaMaxEntries {
			var oldestID int64
			var oldestAt time.Time
			for id, e := range s.runMeta {
				if oldestID == 0 || e.at.Before(oldestAt) {
					oldestID = id
					oldestAt = e.at
				}
			}
			if oldestID != 0 {
				delete(s.runMeta, oldestID)
			}
		}
	}
	s.runMeta[r.ID] = runMetaEntry{run: cp, at: now}
}

// cachedRun returns cached immutable metadata for runID if present and fresh.
func (s *Server) cachedRun(runID int64) (*githubapp.Run, bool) {
	now := s.o.Now()
	s.runMetaMu.Lock()
	defer s.runMetaMu.Unlock()
	e, ok := s.runMeta[runID]
	if !ok {
		return nil, false
	}
	if now.Sub(e.at) > runMetaTTL {
		delete(s.runMeta, runID)
		return nil, false
	}
	cp := cloneRunMeta(e.run)
	return &cp, true
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

// detached returns a context that outlives the HTTP request (GitHub drops a
// delivery after 10 s, which cancels the request context) but is bounded.
func detached(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), d)
}

// cleanupContext is a fresh, bounded context for compensating actions, so
// they run even if the operation they undo ran out of time.
func cleanupContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), cleanupTimeout)
}

// stackLabelPrefix starts every stack label.
const stackLabelPrefix = "crci-stack-"

// StackLabel returns the label that marks the runners of the stack whose
// runner job is job in region: crci-stack-<region>-<job>. Region and job names
// are lowercase letters, digits and hyphens, valid in a runner label.
func StackLabel(region, job string) string { return stackLabelPrefix + region + "-" + job }

// otherStack reports whether a registration carries a stack label that is not
// this stack's. Registrations without a stack label (made before stack labels
// existed) count as this stack's.
func (s *Server) otherStack(rn githubapp.Runner) bool {
	if s.o.StackLabel == "" {
		return false
	}
	for _, l := range rn.Labels {
		if strings.HasPrefix(strings.ToLower(l.Name), stackLabelPrefix) && !strings.EqualFold(l.Name, s.o.StackLabel) {
			return true
		}
	}
	return false
}

// RunnerName is the runner name for a GitHub job ID.
func RunnerName(jobID int64) string { return RunnerPrefix + strconv.FormatInt(jobID, 10) }

func parseRunnerName(name string) (int64, bool) {
	rest, ok := strings.CutPrefix(name, RunnerPrefix)
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	return id, err == nil && id > 0
}

func hasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if strings.EqualFold(l, want) {
			return true
		}
	}
	return false
}

// servable reports whether a cr-ci runner can take a job with these labels:
// GitHub requires every job label on the runner, and cr-ci runners are
// registered with RUNNER_LABEL only.
func (s *Server) servable(labels []string) bool {
	if len(labels) == 0 {
		return false
	}
	for _, l := range labels {
		if !strings.EqualFold(l, s.o.RunnerLabel) {
			return false
		}
	}
	return true
}

func (s *Server) targetRepo(fullName string) (githubapp.Repo, bool) {
	for _, r := range s.o.TargetRepos {
		if strings.EqualFold(r.String(), fullName) {
			return r, true
		}
	}
	return githubapp.Repo{}, false
}

// runnerRepo returns a served repo through which the runners visible to
// fullName can be addressed: the repo itself if served, otherwise (org scope
// only) a served repo of the same owner, otherwise the zero Repo.
func (s *Server) runnerRepo(fullName string) githubapp.Repo {
	if r, ok := s.targetRepo(fullName); ok {
		return r
	}
	if !s.o.OrgScope {
		return githubapp.Repo{}
	}
	owner, _, _ := strings.Cut(fullName, "/")
	for _, r := range s.o.TargetRepos {
		if strings.EqualFold(r.Owner, owner) {
			return r
		}
	}
	return githubapp.Repo{}
}

// servedRepos lists the repos whose jobs GitHub may hand a runner registered
// through r (the runner's job guard allowlist, GH_REPOS): r alone in repo scope;
// in org scope every target repo of r's owner, since org-level runners take any
// of their jobs with the label.
func (s *Server) servedRepos(r githubapp.Repo) []string {
	if !s.o.OrgScope {
		return []string{r.String()}
	}
	var out []string
	for _, t := range s.o.TargetRepos {
		if strings.EqualFold(t.Owner, r.Owner) {
			out = append(out, t.String())
		}
	}
	if len(out) == 0 {
		out = []string{r.String()}
	}
	return out
}

// scopeKey identifies the runner list a repo's runners live in.
func (s *Server) scopeKey(r githubapp.Repo) string {
	if s.o.OrgScope {
		return "org:" + strings.ToLower(r.Owner)
	}
	return "repo:" + strings.ToLower(r.String())
}

func headRepo(run *githubapp.Run) string {
	if run.HeadRepository == nil {
		return ""
	}
	return run.HeadRepository.FullName
}

// allowedEvents are the only workflow-run events whose jobs may get a cr-ci
// runner: the same two the workflows' routing expression routes (same-repo
// push and pull_request). It is an allowlist, so any other event, named or
// not yet known (pull_request_target, workflow_run, issue_comment,
// repository_dispatch, workflow_dispatch, schedule, merge_group, ...), is
// refused (fail closed), even if a workflow edit routes it.
var allowedEvents = map[string]bool{"push": true, "pull_request": true}

// forkReason returns why run must not get a runner, or "" if it may. It
// guards dispatch (webhook and reconcile backfill) and the hijack guard.
func forkReason(run *githubapp.Run, repo githubapp.Repo) string {
	switch {
	case !allowedEvents[run.Event]:
		return "event_not_allowed:" + run.Event
	case run.Repository == nil || !strings.EqualFold(run.Repository.FullName, repo.String()):
		return "repository_mismatch"
	case run.HeadRepository == nil:
		return "head_repository_missing"
	case !strings.EqualFold(run.HeadRepository.FullName, run.Repository.FullName):
		return "head_repository_differs"
	}
	return ""
}
