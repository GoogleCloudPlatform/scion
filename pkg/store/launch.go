// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package store

import "time"

// LaunchKind values for Agent.LaunchKind / BeginLaunch's kind parameter
// (design t1-async-create-v11.md §3.3). Only LaunchKindCreate is writable by
// BeginLaunch in P1a; start/restart land in P6 (§3.13).
const (
	LaunchKindCreate  = "create"
	LaunchKindStart   = "start"
	LaunchKindRestart = "restart"
)

// LaunchState values for Agent.LaunchState.
const (
	LaunchStateActive = "active"
	LaunchStateEnded  = "ended"
)

// LaunchEndReason values for Agent.LaunchEndReason (design §3.3).
const (
	LaunchEndReasonSucceeded       = "succeeded"
	LaunchEndReasonRunningObserved = "running_observed"
	LaunchEndReasonFailed          = "failed"
	LaunchEndReasonTimedOut        = "timed_out"
	LaunchEndReasonLost            = "lost"
	LaunchEndReasonNotLaunched     = "not_launched"
	LaunchEndReasonSuperseded      = "superseded"
)

// Launch error codes for Agent.LaunchError (design §3.3, §3.9).
const (
	LaunchErrorLaunchTimeout  = "launch_timeout"
	LaunchErrorBrokerLost     = "broker_lost"
	LaunchErrorLaunchStopped  = "launch_stopped"
	LaunchErrorAgentError     = "agent_error"
	LaunchErrorHubUnreachable = "hub_unreachable"
)

// LaunchReport report-state values (mirrors the broker->Hub wire report's
// State field, design §3.2 AgentLaunchReport; defined here rather than
// imported from pkg/runtimebroker so the store package incurs no dependency
// on it).
const (
	LaunchReportStateClaim      = "claim"
	LaunchReportStateCheckpoint = "checkpoint"
	LaunchReportStateProgress   = "progress"
	LaunchReportStateSucceeded  = "succeeded"
	LaunchReportStateFailed     = "failed"
)

// LaunchReport is the store-level input to ApplyLaunchReport. It carries the
// fields ApplyLaunchReport needs to evaluate design §3.7's rule list and, on
// a successful terminal, the subset of agent info a P1a caller has already
// resolved from the broker's payload. Fuller broker-response application
// (the complete RemoteAgentInfo echo: exposed ports, applied config, etc.)
// is P1b-1 wire-plumbing territory; P1a's ApplyLaunchReport applies only the
// fields on this struct.
type LaunchReport struct {
	LaunchID   string
	InstanceID string
	Seq        int64
	State      string // claim | checkpoint | progress | succeeded | failed
	Phase      string // desired phase for a claim/checkpoint/progress report
	Step       string
	Message    string
	ErrorCode  string // set when State == failed

	// Runtime and RuntimeState are applied verbatim on a succeeded terminal
	// (design §3.7 step 6 "apply Agent as applyBrokerResponse does" / step 5
	// "apply agent info without changing the phase"). Left empty, they leave
	// the stored value unchanged.
	Runtime      string
	RuntimeState string
}

// LaunchReportAnswer is ApplyLaunchReport's result, corresponding to the
// broker->Hub report endpoint's response shapes (design §3.2):
//
//	200 {"result": Result}                              in-range Result
//	404 {"code": "agent_launch_unknown"}                 HTTPStatus == 404
//	403 (broker is not agent.RuntimeBrokerID)             HTTPStatus == 403
//	409 {"code": "stale_launch", "reason": Reason}        HTTPStatus == 409
type LaunchReportAnswer struct {
	// Result is one of "applied", "duplicate", "completed" when HTTPStatus
	// is 0 (meaning 200).
	Result string
	// HTTPStatus is non-zero for every non-200 answer (403, 404, 409).
	HTTPStatus int
	// Code is the error body's "code" field for a non-200 answer.
	Code string
	// Reason is the error body's "reason" field for a 409 stale_launch answer
	// (one of the LaunchEndReason values, or "other_owner").
	Reason string
	// Changed reports whether this report caused a written change to step,
	// phase, message, launch state, or a first owner claim (design §3.7 step
	// 7: publish an agent-status event after every applied change; a
	// keepalive-only write is not published). False for a
	// duplicate, for a no-op answer on an already-ended launch, and for a
	// same-content keepalive that only refreshed launch_last_report_at. A
	// P1a-ii caller uses this to decide whether to publish; ApplyLaunchReport
	// itself never publishes.
	Changed bool
}

// LaunchReportAnswer.Result values.
const (
	LaunchReportResultApplied   = "applied"
	LaunchReportResultDuplicate = "duplicate"
	LaunchReportResultCompleted = "completed"
)

// Report error/reason codes carried on a non-200 LaunchReportAnswer.
const (
	LaunchReportCodeStaleLaunch   = "stale_launch"
	LaunchReportCodeUnknownLaunch = "agent_launch_unknown"
	LaunchReportReasonSuperseded  = "superseded"
	LaunchReportReasonDeleted     = "deleted"
	LaunchReportReasonOtherOwner  = "other_owner"
	LaunchReportReasonStopped     = "stopped"
)

// ReaperParams configures one RunLaunchReaperTick call (design §3.7).
type ReaperParams struct {
	// KeepaliveInterval is the Hub's configured keepalive interval
	// (hub.launchKeepaliveSeconds, default 15s). Staleness is 8x this value.
	KeepaliveInterval time.Duration
	// ReaperInterval is the fixed reaper ticker interval (15s). Arming uses
	// ReaperInterval + 5s as the "no replica completed a tick" threshold.
	ReaperInterval time.Duration
}

// ReaperTickOutcome is RunLaunchReaperTick's headline result.
type ReaperTickOutcome string

const (
	// ReaperTickNotAcquired means another transaction currently holds the
	// transaction-scoped advisory lock. Neutral: writes nothing.
	ReaperTickNotAcquired ReaperTickOutcome = "not_acquired"
	// ReaperTickUnavailable means the tick could not even attempt the lock:
	// a connection could not be checked out, or a tick-level statement up to
	// and including the lock/SET LOCAL statements failed. Neutral: writes
	// nothing.
	ReaperTickUnavailable ReaperTickOutcome = "unavailable"
	// ReaperTickCompleted means the tick committed successfully (whether or
	// not it reaped any rows).
	ReaperTickCompleted ReaperTickOutcome = "completed"
	// ReaperTickFailed means the tick held the lock but a tick-level
	// statement (arm check, a selection, a savepoint statement, the ok_at
	// write, or the commit) failed, or the tick timed out. The transaction
	// was rolled back and a best-effort disarm was attempted.
	ReaperTickFailed ReaperTickOutcome = "failed"
)

// ReaperTickResult is RunLaunchReaperTick's return value.
type ReaperTickResult struct {
	Outcome ReaperTickOutcome
	// Armed reports whether staleness-based reaping ran on this tick.
	Armed bool
	// DisarmedFor is how long the cluster has been disarmed, as observed at
	// the start of this tick (0 while armed).
	DisarmedFor time.Duration
	// Reaped lists the agents whose launch this tick ended: deadline and
	// staleness reaps move phase to error; a wind-down reap ends the launch
	// without changing phase (the row is already stopped/stopping/suspended/
	// error). Empty on a not_acquired/unavailable/failed outcome.
	Reaped []Agent
	// RowErrors counts per-row savepoint failures during this tick (design
	// §3.7 step 6): a poison row that cannot be reaped is rolled back
	// to its savepoint, logged, and counted here without failing the tick.
	RowErrors int
}
