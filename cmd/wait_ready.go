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

package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// startWaitReady is `scion start --wait-ready`: after the agent is running,
// also wait until its harness accepts input (state.AgentState.AcceptsInput).
var startWaitReady bool

// defaultWaitReadyTimeout bounds the readiness part of --wait-ready when
// --wait-timeout is not given: the harness readiness deadline (at which
// `sciontool init` reports the agent as working anyway) plus a margin for
// the report to reach the Hub.
const defaultWaitReadyTimeout = state.ReadyDeadline + 30*time.Second

// exitCodeNotReady is the exit status when --wait-ready times out. The agent
// is left running; 1 stays reserved for errors.
const exitCodeNotReady = 3

// waitReadyPollInterval is the delay between readiness polls.
var waitReadyPollInterval = launchPollInterval

// validateWaitReadyFlags rejects --wait-ready combined with --no-wait.
func validateWaitReadyFlags() error {
	if startWaitReady && startNoWait {
		return fmt.Errorf("--wait-ready cannot be used with --no-wait")
	}
	return nil
}

// waitReadyTimeoutError reports that the agent did not accept input in
// time. It exits with exitCodeNotReady. When the result was already written
// as JSON, Unwrap exposes a jsonReportedError so no error banner is printed.
type waitReadyTimeoutError struct {
	Agent    string
	Timeout  time.Duration
	Phase    string
	Activity string
	reported bool
}

func (e *waitReadyTimeoutError) Error() string {
	status := e.Phase
	if e.Activity != "" {
		status = state.Activity(e.Activity).DisplayLabel()
	}
	if status == "" {
		status = "unknown"
	}
	return fmt.Sprintf("agent '%s' did not accept input within %s (status: %s); it was left running", e.Agent, e.Timeout, status)
}

func (e *waitReadyTimeoutError) ExitCode() int { return exitCodeNotReady }

func (e *waitReadyTimeoutError) Unwrap() error {
	if e.reported {
		return &jsonReportedError{msg: e.Error()}
	}
	return nil
}

// readyProbe is one observation of the agent's state for --wait-ready.
type readyProbe struct {
	Phase    string
	Activity string
	// LegacyStatus is true when only the legacy Status string was
	// available (a Hub without phase/activity fields); a running agent
	// then counts as ready, since no activity can be observed.
	LegacyStatus bool
}

// waitReadyOptions configures waitForAgentAcceptsInput.
type waitReadyOptions struct {
	AgentName string
	// Get fetches the agent's current state.
	Get func(ctx context.Context) (readyProbe, error)
	// Timeout bounds the wait; zero uses defaultWaitReadyTimeout.
	Timeout  time.Duration
	Progress io.Writer // nil for no progress output
}

// waitForAgentAcceptsInput polls until the agent accepts input. It returns
// the last probe and nil when ready, a *waitReadyTimeoutError at the
// timeout, or an error when the agent leaves the running phase. Fetch
// errors are retried until the timeout.
func waitForAgentAcceptsInput(ctx context.Context, opts waitReadyOptions) (readyProbe, error) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultWaitReadyTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var last readyProbe
	announced := false
	for {
		p, err := opts.Get(ctx)
		if err == nil {
			last = p
			as := state.AgentState{Phase: state.Phase(p.Phase), Activity: state.Activity(p.Activity)}
			if as.AcceptsInput() || (p.LegacyStatus && as.Phase == state.PhaseRunning) {
				return p, nil
			}
			switch as.Phase {
			case state.PhaseStopping, state.PhaseStopped, state.PhaseError, state.PhaseSuspended:
				return p, fmt.Errorf("agent '%s' entered phase %s while waiting for it to accept input", opts.AgentName, p.Phase)
			}
			if !announced && opts.Progress != nil {
				status := state.Activity(p.Activity).DisplayLabel()
				if status == "" {
					status = p.Phase
				}
				_, _ = fmt.Fprintf(opts.Progress, "Waiting for agent '%s' to accept input (%s)...\n", opts.AgentName, status)
				announced = true
			}
		}
		select {
		case <-ctx.Done():
			return last, &waitReadyTimeoutError{Agent: opts.AgentName, Timeout: timeout, Phase: last.Phase, Activity: last.Activity}
		case <-time.After(waitReadyPollInterval):
		}
	}
}

// remainingWaitReadyTimeout returns the readiness budget: what is left of an
// explicit --wait-timeout (which bounds the whole wait) since waitStart, or
// defaultWaitReadyTimeout. An exhausted explicit budget still allows one
// readiness check (minWaitReadyTimeout).
func remainingWaitReadyTimeout(waitStart time.Time) time.Duration {
	if startWaitTimeout <= 0 {
		return defaultWaitReadyTimeout
	}
	left := startWaitTimeout - time.Since(waitStart)
	if left < minWaitReadyTimeout {
		left = minWaitReadyTimeout
	}
	return left
}

// minWaitReadyTimeout is the smallest readiness budget, enough for one
// status fetch.
const minWaitReadyTimeout = 5 * time.Second

// markWaitReadyReported adapts a --wait-ready error for a result already
// written as JSON: the exit status is kept and no error banner is printed.
func markWaitReadyReported(err error) error {
	if err == nil {
		return nil
	}
	var te *waitReadyTimeoutError
	if errors.As(err, &te) {
		te.reported = true
		return te
	}
	return &jsonReportedError{msg: err.Error()}
}

// hubReadyProbe extracts the readiness fields from a Hub agent.
func hubReadyProbe(a *hubclient.Agent) readyProbe {
	if a.Phase == "" {
		phase, activity := hubStatusToPhaseActivity(a.Status)
		return readyProbe{Phase: phase, Activity: activity, LegacyStatus: true}
	}
	return readyProbe{Phase: a.Phase, Activity: a.Activity}
}

// agentLister is the part of the local agent manager --wait-ready needs.
type agentLister interface {
	List(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error)
}

// localReadyProbe reads a local agent's phase and activity (from its
// agent-info.json, via the manager's list).
func localReadyProbe(mgr agentLister, agentName string) func(ctx context.Context) (readyProbe, error) {
	return func(ctx context.Context) (readyProbe, error) {
		agents, err := mgr.List(ctx, nil)
		if err != nil {
			return readyProbe{}, err
		}
		for _, a := range agents {
			if a.Name == agentName || a.Slug == agentName {
				return readyProbe{Phase: a.Phase, Activity: a.Activity}, nil
			}
		}
		return readyProbe{}, fmt.Errorf("agent '%s' not found", agentName)
	}
}
