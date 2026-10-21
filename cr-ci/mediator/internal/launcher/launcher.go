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
)

// LaunchSpec describes one runner execution for one GitHub job.
type LaunchSpec struct {
	JobID      int64
	RunID      int64
	Repo       string // owner/name, for logs
	RunnerName string // cr-job-<job_id>
	ClaimToken string // single-use; exchanged for the JIT config
}

// Execution is a runner execution that has not finished.
type Execution struct {
	Ref     string    // backend reference (Cloud Run execution resource name)
	JobID   int64     // GitHub job ID, 0 if unknown
	Created time.Time // zero if unknown
}

// RunnerLauncher is the backend-neutral launcher interface.
type RunnerLauncher interface {
	// Launch starts one runner execution and returns its reference.
	Launch(ctx context.Context, spec LaunchSpec) (executionRef string, err error)
	// Terminate stops the execution for jobID. executionRef may be empty, in
	// which case the backend looks it up by jobID. Stopping an execution that
	// already finished or does not exist is not an error.
	Terminate(ctx context.Context, jobID int64, executionRef string) error
	// ReapOrphans stops executions older than maxAge and returns how many it
	// stopped.
	ReapOrphans(ctx context.Context, maxAge time.Duration) (int, error)
}

// ActiveLister reports the executions that are still running; the mediator
// uses it for capacity and idempotency checks.
type ActiveLister interface {
	Active(ctx context.Context) ([]Execution, error)
}
