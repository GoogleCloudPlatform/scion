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
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// Waiting for a Hub agent launch to finish (`scion start` / `scion resume`
// in Hub mode). A Hub with asynchronous launch enabled answers the create as
// soon as the broker accepts it, with the agent in a pre-running phase and
// an active launch; the CLI then polls GET agent until the agent is running
// or the launch has failed.

// launchPollInterval is how often the agent is fetched while waiting. It is
// a variable so tests can shorten it.
var launchPollInterval = 2 * time.Second

const (
	// launchWaitSlack is added to the Hub-advertised remaining launch budget
	// to form the default wait. It covers the Hub's reaper interval, the
	// poll interval and clock skew, so the wait outlasts the Hub's own
	// deadline handling.
	launchWaitSlack = 30 * time.Second
	// launchWaitFallback is the default wait when the Hub does not advertise
	// a launch budget (older Hub, async launch disabled, no active launch).
	launchWaitFallback = 5 * time.Minute
	// launchFetchTimeout bounds one GET agent while waiting.
	launchFetchTimeout = 30 * time.Second

	// errCodeAgentCreateIncomplete is the Hub's 409 code for a start of an
	// agent whose create launch stopped or failed before the agent ran.
	errCodeAgentCreateIncomplete = "agent_create_incomplete"

	// exitCodeInterrupted is the conventional exit status after SIGINT.
	exitCodeInterrupted = 130
)

// Flags shared by `scion start` and `scion resume`.
var (
	startNoWait      bool
	startWaitTimeout time.Duration
)

// exitCoder is implemented by errors that request a specific process exit
// status. Execute uses it; every other error exits 1.
type exitCoder interface {
	ExitCode() int
}

// launchActive reports whether the Hub reports an in-flight launch for a.
func launchActive(a *hubclient.Agent) bool {
	return a != nil && a.Launch != nil && a.Launch.Active
}

// launchWaitBudget returns how long to wait for a launch. An explicit
// --wait-timeout wins. Otherwise the Hub-advertised remaining launch budget
// plus launchWaitSlack is used, and launchWaitFallback when the Hub does not
// advertise one.
func launchWaitBudget(explicit time.Duration, a *hubclient.Agent) time.Duration {
	if explicit > 0 {
		return explicit
	}
	if a != nil && a.Launch != nil && a.Launch.RemainingSeconds != nil {
		remaining := *a.Launch.RemainingSeconds
		if remaining < 0 {
			remaining = 0
		}
		return time.Duration(remaining)*time.Second + launchWaitSlack
	}
	return launchWaitFallback
}

// launchWaitOptions configures waitForAgentLaunch.
type launchWaitOptions struct {
	// AgentName is the name used in messages and hints.
	AgentName string
	// BudgetFrom is the agent from the create response, used to derive the
	// default wait. It may be nil; the first fetched agent is then used.
	BudgetFrom *hubclient.Agent
	// Get fetches the agent.
	Get func(ctx context.Context) (*hubclient.Agent, error)
	// Timeout is an explicit wait (--wait-timeout); 0 derives it.
	Timeout time.Duration
	// PollInterval defaults to launchPollInterval.
	PollInterval time.Duration
	// Progress receives one line per phase or step change; nil is silent.
	Progress io.Writer
}

// waitForAgentLaunch fetches the agent immediately and then every poll
// interval until it is running (returned with a nil error), reaches error or
// stopped (*launchFailedError), is deleted, the wait budget runs out
// (*launchWaitTimeoutError), or ctx is cancelled (*launchWaitInterruptedError).
// Transient fetch errors are retried. It never changes the agent.
func waitForAgentLaunch(ctx context.Context, o launchWaitOptions) (*hubclient.Agent, error) {
	interval := o.PollInterval
	if interval <= 0 {
		interval = launchPollInterval
	}

	var last *hubclient.Agent
	var progress launchProgress
	var waitCtx context.Context
	var cancel context.CancelFunc

	// check fetches once and reports whether the wait is over.
	check := func(fetchCtx context.Context) (bool, error) {
		getCtx, getCancel := context.WithTimeout(fetchCtx, launchFetchTimeout)
		a, err := o.Get(getCtx)
		getCancel()
		if err != nil {
			if apiclient.IsNotFoundError(err) {
				return true, fmt.Errorf("agent '%s' no longer exists; it was deleted while launching", o.AgentName)
			}
			return false, nil // transient: retry on the next tick
		}
		if a == nil {
			return false, nil
		}
		last = a
		progress.report(o.Progress, o.AgentName, a)
		return launchOutcome(o.AgentName, a)
	}

	start := func(budgetAgent *hubclient.Agent) {
		waitCtx, cancel = context.WithTimeout(ctx, launchWaitBudget(o.Timeout, budgetAgent))
	}

	if o.BudgetFrom != nil && (o.Timeout > 0 || (o.BudgetFrom.Launch != nil && o.BudgetFrom.Launch.RemainingSeconds != nil)) {
		start(o.BudgetFrom)
		defer cancel()
		if done, err := check(waitCtx); done {
			return last, err
		}
	} else {
		// Fetch first so the budget can come from the Hub's current view.
		done, err := check(ctx)
		budgetAgent := last
		if budgetAgent == nil {
			budgetAgent = o.BudgetFrom
		}
		start(budgetAgent)
		defer cancel()
		if done {
			return last, err
		}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				return last, &launchWaitInterruptedError{Agent: o.AgentName, Deadline: launchDeadline(last)}
			}
			return last, &launchWaitTimeoutError{Agent: o.AgentName, Deadline: launchDeadline(last)}
		case <-ticker.C:
			if done, err := check(waitCtx); done {
				return last, err
			}
		}
	}
}

// launchOutcome maps an agent to the end of the wait: running is success;
// error and stopped are failures; every other phase keeps waiting.
func launchOutcome(name string, a *hubclient.Agent) (bool, error) {
	phase, _ := hubAgentPhaseActivity(a.Phase, a.Activity, a.Status)
	switch phase {
	case string(state.PhaseRunning):
		return true, nil
	case string(state.PhaseError), string(state.PhaseStopped):
		return true, newLaunchFailedError(name, a)
	}
	return false, nil
}

// launchDeadline returns the active launch's deadline, or nil.
func launchDeadline(a *hubclient.Agent) *time.Time {
	if a == nil || a.Launch == nil {
		return nil
	}
	return a.Launch.Deadline
}

// launchProgress prints a line whenever the phase or launch step changes.
type launchProgress struct {
	printed   bool
	lastPhase string
	lastStep  string
}

func (p *launchProgress) report(w io.Writer, name string, a *hubclient.Agent) {
	if w == nil {
		return
	}
	phase, _ := hubAgentPhaseActivity(a.Phase, a.Activity, a.Status)
	step := ""
	if a.Launch != nil {
		step = a.Launch.Step
	}
	if p.printed && phase == p.lastPhase && step == p.lastStep {
		return
	}
	p.printed, p.lastPhase, p.lastStep = true, phase, step
	if step != "" {
		_, _ = fmt.Fprintf(w, "  %s: %s (%s)\n", name, phase, step)
	} else {
		_, _ = fmt.Fprintf(w, "  %s: %s\n", name, phase)
	}
}

// launchFailedError is returned when the agent ends in error or stopped
// instead of running, and for a Hub 409 agent_create_incomplete.
type launchFailedError struct {
	Agent string
	Phase string
	// Incomplete is true when the agent's create did not complete. The
	// record stays on the Hub; it can only be recreated (delete, then start).
	Incomplete bool
	// Reason is the first line of the message.
	Reason   string
	Template string
	Task     string
}

func newLaunchFailedError(name string, a *hubclient.Agent) *launchFailedError {
	phase, _ := hubAgentPhaseActivity(a.Phase, a.Activity, a.Status)
	e := &launchFailedError{Agent: name, Phase: phase}
	if a.Launch != nil && a.Launch.Error != "" {
		e.Incomplete = a.Launch.Kind == "" || a.Launch.Kind == "create"
		reason := fmt.Sprintf("agent '%s' did not start (%s)", name, a.Launch.Error)
		if e.Incomplete {
			reason = fmt.Sprintf("agent '%s' create did not complete (%s)", name, a.Launch.Error)
		}
		if a.Message != "" {
			reason += ": " + a.Message
		}
		e.Reason = reason
		e.Template = a.Template
		if a.AppliedConfig != nil {
			e.Task = a.AppliedConfig.Task
		}
		return e
	}
	info := phase
	if a.ContainerStatus != "" {
		info += ", container: " + a.ContainerStatus
	}
	e.Reason = fmt.Sprintf("agent '%s' failed to start (phase: %s)", name, info)
	if a.Message != "" {
		e.Reason += ": " + a.Message
	}
	return e
}

// incompleteCreateError builds the error for a Hub 409
// agent_create_incomplete answer to a start.
func incompleteCreateError(name string, apiErr *apiclient.APIError) *launchFailedError {
	e := &launchFailedError{Agent: name, Incomplete: true, Reason: apiErr.Message}
	if e.Reason == "" {
		e.Reason = fmt.Sprintf("agent '%s' create did not complete", name)
	}
	if apiErr.Details != nil {
		e.Template, _ = apiErr.Details["template"].(string)
		e.Task, _ = apiErr.Details["task"].(string)
	}
	return e
}

func (e *launchFailedError) Error() string {
	var b strings.Builder
	b.WriteString(e.Reason)
	if !e.Incomplete {
		fmt.Fprintf(&b, "\n\nCheck the agent logs with: scion logs %s", e.Agent)
		return b.String()
	}
	if e.Template != "" || e.Task != "" {
		b.WriteString("\n")
		if e.Template != "" {
			fmt.Fprintf(&b, "\nTemplate: %s", e.Template)
		}
		if e.Task != "" {
			fmt.Fprintf(&b, "\nTask: %s", e.Task)
		}
	}
	fmt.Fprintf(&b, "\n\nThe create did not complete. Recreate the agent with:\n  scion delete %s\n  scion start %s ... (with the same template and task)", e.Agent, e.Agent)
	return b.String()
}

// launchWaitTimeoutError is returned when the wait budget runs out while the
// agent is still launching. The launch itself is not affected.
type launchWaitTimeoutError struct {
	Agent    string
	Deadline *time.Time
}

func (e *launchWaitTimeoutError) Error() string {
	return fmt.Sprintf("agent '%s' is still launching%s; re-run scion start %s to keep waiting",
		e.Agent, deadlineSuffix(e.Deadline), e.Agent)
}

// launchWaitInterruptedError is returned when the user interrupts the wait.
// Only the wait stops; the launch continues on the Hub.
type launchWaitInterruptedError struct {
	Agent    string
	Deadline *time.Time
}

func (e *launchWaitInterruptedError) Error() string {
	return fmt.Sprintf("stopped waiting; agent '%s' is still launching%s; re-run scion start %s to keep waiting",
		e.Agent, deadlineSuffix(e.Deadline), e.Agent)
}

func (e *launchWaitInterruptedError) ExitCode() int { return exitCodeInterrupted }

func deadlineSuffix(d *time.Time) string {
	if d == nil || d.IsZero() {
		return ""
	}
	return fmt.Sprintf(" (deadline %s)", d.UTC().Format(time.RFC3339))
}

// asIncompleteCreate returns the Hub's agent_create_incomplete error, if err
// is one.
func asIncompleteCreate(err error) (*apiclient.APIError, bool) {
	var apiErr *apiclient.APIError
	if errors.As(err, &apiErr) && apiErr.Code == errCodeAgentCreateIncomplete {
		return apiErr, true
	}
	return nil, false
}
