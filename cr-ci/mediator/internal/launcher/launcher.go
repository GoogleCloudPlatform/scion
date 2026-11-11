// Package launcher starts and stops runner executions. RunnerLauncher is the
// backend-neutral interface from the cr-runners design (section 2.2);
// JobsLauncher implements it on the Cloud Run Admin API v2 (Jobs) over REST.
//
// The launcher never sees a JIT config. It passes the runner a one-time claim
// token, which the runner exchanges with the mediator (POST /internal/claim)
// for the JIT config. Execution env overrides are visible to anyone with
// roles/run.viewer, so only the claim token may appear there.
package launcher

import (
	"context"
	"time"
)

// Environment variable names set on each runner execution: the
// mediator-to-runner contract, shared with cr-ci/runner/entrypoint.sh.
const (
	EnvClaimToken  = "RUNNER_CLAIM_TOKEN"
	EnvMediatorURL = "MEDIATOR_URL"
	EnvJobID       = "GH_JOB_ID"
	EnvRunnerName  = "RUNNER_NAME"
	// EnvRepos lists, comma-separated, every repository (owner/name) whose jobs the
	// runner may legitimately be handed (its registration scope); the runner's job
	// guard refuses a job of any other repository.
	EnvRepos = "GH_REPOS"
)

// LaunchSpec describes one runner execution for one GitHub job.
type LaunchSpec struct {
	JobID int64 // the job the runner is launched to serve (GH_JOB_ID)
	RunID int64
	Repo  string // owner/name, for logs
	// ServedRepos is every repository (owner/name) whose jobs GitHub may hand this
	// runner: the repo itself in repo scope, every target repo of the owner in org
	// scope. Passed as GH_REPOS, the runner's job guard allowlist.
	ServedRepos []string
	// RunnerName is the runner's registration name, normally
	// cr-job-<JobID>. After a runner swap or a hijack it can carry another
	// job's ID (the name of the runner that job displaced).
	RunnerName string
	ClaimToken string // single-use; exchanged for the JIT config
}

// Execution is a runner execution.
type Execution struct {
	Ref        string    // backend reference (Cloud Run execution resource name)
	JobID      int64     // GH_JOB_ID: the job it was launched to serve, 0 if unknown
	RunnerName string    // RUNNER_NAME, "" if unknown
	Created    time.Time // zero if unknown
	Done       bool      // finished (succeeded, failed or cancelled)
}

// RunnerLauncher is the backend-neutral launcher interface.
type RunnerLauncher interface {
	// Launch starts one runner execution and returns its reference.
	Launch(ctx context.Context, spec LaunchSpec) (executionRef string, err error)
	// Terminate stops the execution of runner runnerName. The design keyed
	// this on the job ID; cr-ci keys it on the runner name because GitHub may
	// give a runner another job than the one it was launched for.
	// executionRef may be empty, in which case the backend looks it up by
	// runner name. Stopping an execution that already finished or does not
	// exist is not an error.
	Terminate(ctx context.Context, runnerName, executionRef string) error
	// ReapOrphans stops executions older than maxAge and returns how many it
	// stopped.
	ReapOrphans(ctx context.Context, maxAge time.Duration) (int, error)
}

// ExecutionLister lists the runner executions the backend still knows,
// finished ones included (Done). The mediator uses it for capacity and
// idempotency checks: a launch missing from the list was not listed yet,
// while a finished one is listed as Done.
type ExecutionLister interface {
	Executions(ctx context.Context) ([]Execution, error)
}
