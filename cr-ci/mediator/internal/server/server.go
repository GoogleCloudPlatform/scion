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
	// WorkFolder is the runner work folder, on the /scratch ephemeral disk.
	WorkFolder = "/scratch/_work"

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

// Server is the mediator HTTP server.
type Server struct {
	o Options

	secretMu      sync.Mutex
	secret        []byte
	prevSecret    []byte
	prevUntil     time.Time
	secretAttempt time.Time

	// dispatchMu serialises dispatch, runner stops and the mutating part of
	// reconcile so that capacity and idempotency checks cannot race (single
	// instance).
	dispatchMu  sync.Mutex
	runners     map[string]*runnerRecord // runner name -> record
	cancelled   map[string]time.Time     // execution refs this process cancelled
	offlineSeen map[string]time.Time     // runner name -> first seen offline
	noticed     map[string]bool          // reconcile notices already logged (pruned each pass)
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
		o:           o,
		runners:     map[string]*runnerRecord{},
		cancelled:   map[string]time.Time{},
		offlineSeen: map[string]time.Time{},
		noticed:     map[string]bool{},
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

// forkReason returns why run must not get a runner, or "" if it may.
// pull_request_target and workflow_run runs execute with the base repo's
// privileges, typically on behalf of fork PRs, so both are refused.
func forkReason(run *githubapp.Run, repo githubapp.Repo) string {
	switch {
	case run.Event == "pull_request_target" || run.Event == "workflow_run":
		return run.Event
	case run.Repository == nil || !strings.EqualFold(run.Repository.FullName, repo.String()):
		return "repository_mismatch"
	case run.HeadRepository == nil:
		return "head_repository_missing"
	case !strings.EqualFold(run.HeadRepository.FullName, run.Repository.FullName):
		return "head_repository_differs"
	}
	return ""
}
