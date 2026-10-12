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

package hubclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// Following an asynchronous agent launch. A Hub with async launch enabled
// answers a create (or, with async start, a start or restart) that opted in
// with AcceptAsyncLaunch as soon as the broker accepts it: the agent is in a
// pre-running phase with an active Launch. WaitForLaunch polls GET agent
// until the agent is running or the launch has ended without running.
//
// The rules follow the scion CLI's wait (cmd/launch_wait.go), without its
// printing: the budget comes from the Hub-advertised remaining launch time,
// transient read errors are retried, and a refused (403) or not-found (404)
// read is interpreted by caller kind, because the Hub answers an agent
// caller with 404 both for a missing agent and for one it may not read.
//
// The CLI still has its own copy of these rules (waitForAgentLaunch in
// cmd/launch_wait.go); a change to one must be applied to the other until
// the CLI is moved onto WaitForLaunch.

// The wait timings are variables so tests can shorten them.
var (
	// launchPollInterval is how often the agent is fetched while waiting.
	launchPollInterval = 2 * time.Second
	// launchWaitSlack is added to the Hub-advertised remaining launch
	// budget. It covers the Hub's reaper interval, the poll interval and
	// clock skew, so the wait outlasts the Hub's own deadline handling.
	launchWaitSlack = 30 * time.Second
	// launchWaitFallback is the wait when the Hub advertises no launch
	// budget.
	launchWaitFallback = 5 * time.Minute
	// launchUnreadableGrace is the whole retry window for a read refused
	// or answered not-found that may be transient, measured from the first
	// failed read.
	launchUnreadableGrace = 5 * time.Second
	// launchUnreadableBackoff is the first retry delay within the grace;
	// it doubles up to launchUnreadableBackoffMax.
	launchUnreadableBackoff    = 250 * time.Millisecond
	launchUnreadableBackoffMax = 2 * time.Second
	// launchFetchTimeout bounds one GET agent while waiting.
	launchFetchTimeout = 30 * time.Second
	// launchFailureSettle is how long a failed phase (error or stopped)
	// seen while the launch is still active is re-read for the
	// launch-ending write that carries the launch error code. The Hub
	// allows that state; the code arrives with the write that ends the
	// launch.
	launchFailureSettle = 5 * time.Second
)

// launchNow and launchAfter are the clock of the unreadable-status retry;
// tests replace them.
var (
	launchNow   = time.Now
	launchAfter = time.After
)

// ErrLaunchWaitTimeout is returned (wrapped) by WaitForLaunch when the wait
// budget runs out while the agent is still launching. Only the wait ends;
// the launch continues on the Hub.
var ErrLaunchWaitTimeout = errors.New("launch wait budget ran out while the agent was still launching")

// ErrAgentDeletedDuringLaunch is returned (wrapped, together with the Hub's
// not-found error) by WaitForLaunch when a caller that is not an agent gets
// a lasting not-found for the agent being waited for.
var ErrAgentDeletedDuringLaunch = errors.New("agent was deleted while launching")

// ErrLaunchNotRunning matches *LaunchNotRunningError with errors.Is.
var ErrLaunchNotRunning = errors.New("agent launch ended without the agent running")

// LaunchOutcome is how a launch wait ended.
type LaunchOutcome struct {
	// Agent is the last agent read in the wait; nil when none was read.
	Agent *Agent
	// Running is true when the agent reached the running phase.
	Running bool
	// Phase is the agent's last phase.
	Phase string
	// Code is the launch error code (launch.error); "" on success.
	Code string
	// EndReason is the launch's end reason (launch.endReason), if ended.
	EndReason string
	// Step is the launch's last reported step.
	Step string
	// Message is the Hub's status message for the agent (agent.message).
	Message string
}

// LaunchWaitOptions configures WaitForLaunch and EnsureRunningBeforeSend.
type LaunchWaitOptions struct {
	// OnStep, when set, is called with the launch step each time it changes
	// to a new non-empty value, starting with the step of the agent passed
	// in.
	OnStep func(step string)
	// AgentCaller is true when the client authenticates as an agent. The
	// Hub answers an agent with 404 both for a missing agent and for one it
	// may not read, so an agent caller's not-found is never reported as a
	// deletion: it ends the wait with *LaunchStatusUnreadableError, before
	// or after a successful read. A non-agent caller is refused with 403,
	// so its lasting not-found is a deletion.
	AgentCaller bool
	// Timeout, when positive, replaces the derived wait budget (it may be
	// longer or shorter) and starts before the first fetch. Running out of
	// it is reported as ErrLaunchWaitTimeout. 0 derives the budget from the
	// launch.
	Timeout time.Duration
	// PollInterval is the time between reads; 0 means 2 s.
	PollInterval time.Duration
}

// LaunchStatusUnreadableError is returned by WaitForLaunch to an agent
// caller whose reads of the agent were refused or answered not-found. The
// launch itself is not affected.
type LaunchStatusUnreadableError struct {
	AgentID string
	// LaunchID is the accepted launch's ID, when known.
	LaunchID string
	// AfterRead is true when the agent was read earlier in the wait and
	// then stopped being readable. That can also mean it was deleted.
	AfterRead bool
	Err       error
}

func (e *LaunchStatusUnreadableError) Error() string {
	launch := ""
	if e.LaunchID != "" {
		launch = fmt.Sprintf(" (launch %s)", e.LaunchID)
	}
	if e.AfterRead {
		return fmt.Sprintf("agent %q: status no longer readable with this credential's scope%s; it may have been deleted, or the Hub refused the read: %v",
			e.AgentID, launch, e.Err)
	}
	return fmt.Sprintf("agent %q: launch accepted; status not readable with this credential's scope%s; the launch continues on the Hub: %v",
		e.AgentID, launch, e.Err)
}

func (e *LaunchStatusUnreadableError) Unwrap() error { return e.Err }

// LaunchStatusRejectedError is returned by WaitForLaunch when the Hub
// refused a read of the agent with a client error that a retry will not
// fix. The launch itself is not affected.
type LaunchStatusRejectedError struct {
	AgentID string
	Err     error
}

func (e *LaunchStatusRejectedError) Error() string {
	return fmt.Sprintf("stopped waiting for agent %q: the Hub refused the status request; the launch continues on the Hub: %v", e.AgentID, e.Err)
}

func (e *LaunchStatusRejectedError) Unwrap() error { return e.Err }

// LaunchNotRunningError is returned by EnsureRunningBeforeSend when the
// launch ended without the agent running.
type LaunchNotRunningError struct {
	Outcome LaunchOutcome
}

func (e *LaunchNotRunningError) Error() string {
	name := ""
	if a := e.Outcome.Agent; a != nil {
		name = a.Name
		if name == "" {
			name = a.ID
		}
	}
	msg := fmt.Sprintf("agent %q failed to start", name)
	if e.Outcome.Code != "" {
		msg += " (" + e.Outcome.Code + ")"
	} else if e.Outcome.Phase != "" {
		msg += " (phase: " + e.Outcome.Phase + ")"
	}
	if e.Outcome.Message != "" {
		msg += ": " + e.Outcome.Message
	}
	return msg
}

// Is reports whether target is ErrLaunchNotRunning.
func (e *LaunchNotRunningError) Is(target error) bool { return target == ErrLaunchNotRunning }

// LaunchActive reports whether the Hub reports an in-flight launch for a.
// Use it on a create, start or restart answer to tell an asynchronous
// answer (the launch continues; follow it with WaitForLaunch) from a
// synchronous one.
func LaunchActive(a *Agent) bool {
	return a != nil && a.Launch != nil && a.Launch.Active
}

// launchPhase returns the agent's phase, falling back to the legacy status
// field the way the CLI does (hubStatusToPhaseActivity in cmd/list.go).
func launchPhase(a *Agent) string {
	if a.Phase != "" {
		return a.Phase
	}
	act := state.Activity(a.Status)
	if act.IsTerminal() {
		return string(state.PhaseStopped)
	}
	if act != "" && act.IsValid() {
		return string(state.PhaseRunning)
	}
	return a.Status
}

// failedLaunchEndReason reports whether an ended launch's end reason means
// it did not bring the agent up, even without an error code.
func failedLaunchEndReason(reason string) bool {
	switch reason {
	case "failed", "timed_out", "lost", "not_launched":
		return true
	}
	return false
}

// launchDone maps an agent to the end of the wait. Running ends it with
// success. Error and stopped end it with a failure, and so does a launch
// that has ended with an error code or a failed end reason, whatever the
// phase (the failure phase differs by launch kind). Everything else keeps
// waiting. WaitForLaunch holds a failed phase seen while the launch is
// still active for launchFailureSettle, to report the launch error code.
func launchDone(a *Agent) (done, running bool) {
	switch launchPhase(a) {
	case string(state.PhaseRunning):
		return true, true
	case string(state.PhaseError), string(state.PhaseStopped):
		return true, false
	}
	if l := a.Launch; l != nil && !l.Active && (l.Error != "" || failedLaunchEndReason(l.EndReason)) {
		return true, false
	}
	return false, false
}

// launchOutcomeOf describes a as a wait outcome.
func launchOutcomeOf(a *Agent) LaunchOutcome {
	if a == nil {
		return LaunchOutcome{}
	}
	_, running := launchDone(a)
	out := LaunchOutcome{Agent: a, Running: running, Phase: launchPhase(a), Message: a.Message}
	if a.Launch != nil {
		out.Code = a.Launch.Error
		out.EndReason = a.Launch.EndReason
		out.Step = a.Launch.Step
	}
	return out
}

// launchWaitBudget returns the wait for a launch: the Hub-advertised
// remaining launch budget plus launchWaitSlack, else launchWaitFallback.
func launchWaitBudget(a *Agent) time.Duration {
	if a != nil && a.Launch != nil && a.Launch.RemainingSeconds != nil {
		remaining := *a.Launch.RemainingSeconds
		if remaining < 0 {
			remaining = 0
		}
		return time.Duration(remaining)*time.Second + launchWaitSlack
	}
	return launchWaitFallback
}

// WaitForLaunch fetches the agent immediately and then every 2 s until it
// is running, the launch has ended without running, or the wait ends.
//
// start is the agent from the create or start answer. It may be nil. When
// it shows an active launch, the launch counts as accepted: a refused or
// not-found read before any read has succeeded is then retried briefly.
// The budget is opts.Timeout when set; else start's launch.remainingSeconds
// plus 30 s; without it, the first fetched agent's; else 5 min.
//
// An agent in phase error or stopped whose launch is still active is
// re-read for up to 5 s (within the budget) so the outcome can carry the
// launch error code from the write that ends the launch; if that does not
// arrive, the failure is reported with what was read (Code may be "").
//
// A nil error means the wait saw the end of the launch: the outcome reports
// whether the agent is running, or how it failed. Otherwise the error is
// one of: ErrLaunchWaitTimeout (wrapped) when the budget runs out; the
// context's error (wrapped) when ctx ends; ErrAgentDeletedDuringLaunch
// (wrapped) for a non-agent caller's lasting not-found;
// *LaunchStatusUnreadableError for an agent caller whose reads are refused
// or not found (see LaunchWaitOptions.AgentCaller);
// *LaunchStatusRejectedError for any other 4xx except 408 and 429. Network
// errors, 5xx, 408 and 429 are retried. With an error, the outcome carries
// the last agent read, if any, and Running is false. It never changes the
// agent.
func WaitForLaunch(ctx context.Context, svc AgentService, agentID string, start *Agent, opts LaunchWaitOptions) (LaunchOutcome, error) {
	if svc == nil {
		return LaunchOutcome{}, errors.New("hubclient: WaitForLaunch: nil agent service")
	}
	accepted := LaunchActive(start)
	launchID := ""
	if accepted {
		launchID = start.Launch.ID
	}

	var last *Agent
	// failedSince is when a failed phase with a still-active launch was
	// first seen in a row of such reads; zero otherwise.
	var failedSince time.Time
	lastStep := ""
	reportStep := func(a *Agent) {
		if opts.OnStep == nil || a == nil || a.Launch == nil {
			return
		}
		if step := a.Launch.Step; step != "" && step != lastStep {
			lastStep = step
			opts.OnStep(step)
		}
	}
	reportStep(start)

	get := func(fetchCtx context.Context) (*Agent, error) {
		getCtx, cancel := context.WithTimeout(fetchCtx, launchFetchTimeout)
		defer cancel()
		return svc.Get(getCtx, agentID)
	}

	// check fetches once and reports whether the wait is over.
	check := func(fetchCtx context.Context) (bool, error) {
		a, err := get(fetchCtx)
		if err != nil && last == nil && accepted && isUnreadableStatusError(err) {
			a, err = retryUnreadableStatus(fetchCtx, get, err)
			// When the retry is cut short, the wait loop reports the
			// interrupt or timeout, except for a non-agent caller whose
			// budget ran out: its 403 or 404 is a definite answer and is
			// reported below.
			if err != nil && isUnreadableStatusError(err) && fetchCtx.Err() != nil &&
				(ctx.Err() != nil || opts.AgentCaller) {
				return false, nil
			}
		}
		// A not-found after a read is confirmed by retrying for the grace;
		// any successful read in that window continues the wait. A
		// not-found that lasts to the end of the budget is reported below;
		// only an interrupt is left to the wait loop.
		if err != nil && last != nil && apiclient.IsNotFoundError(err) {
			a, err = retryUnreadableStatus(fetchCtx, get, err)
			if err != nil && isUnreadableStatusError(err) && ctx.Err() != nil {
				return false, nil
			}
		}
		// An agent caller's not-found never proves a deletion, before or
		// after a read (the Hub can refuse a later read that an earlier one
		// passed). Before any read of an accepted launch, its 403 is
		// reported the same way; after a read, a 403 is a refusal.
		if err != nil && opts.AgentCaller &&
			(apiclient.IsNotFoundError(err) || (last == nil && accepted && apiclient.IsForbiddenError(err))) {
			return true, &LaunchStatusUnreadableError{AgentID: agentID, LaunchID: launchID, AfterRead: last != nil, Err: err}
		}
		if err != nil {
			if apiclient.IsNotFoundError(err) {
				return true, fmt.Errorf("agent %q: %w: %w", agentID, ErrAgentDeletedDuringLaunch, err)
			}
			if isTerminalFetchError(err) {
				return true, &LaunchStatusRejectedError{AgentID: agentID, Err: err}
			}
			return false, nil // transient: retry on the next tick
		}
		if a == nil {
			return false, nil
		}
		last = a
		reportStep(a)
		done, running := launchDone(a)
		if done && !running && LaunchActive(a) {
			// The launch error code comes with the write that ends the
			// launch; re-read briefly for it.
			if failedSince.IsZero() {
				failedSince = time.Now()
			}
			return time.Since(failedSince) >= launchFailureSettle, nil
		}
		failedSince = time.Time{}
		return done, nil
	}

	finish := func(err error) (LaunchOutcome, error) {
		out := launchOutcomeOf(last)
		if err != nil {
			out.Running = false
		}
		return out, err
	}

	budget := func(a *Agent) time.Duration {
		if opts.Timeout > 0 {
			return opts.Timeout
		}
		return launchWaitBudget(a)
	}

	var waitCtx context.Context
	var cancel context.CancelFunc
	if opts.Timeout > 0 || (start != nil && start.Launch != nil && start.Launch.RemainingSeconds != nil) {
		// An explicit or advertised budget starts before the first fetch,
		// so it also bounds the unreadable-status retry.
		waitCtx, cancel = context.WithTimeout(ctx, budget(start))
		defer cancel()
		if done, err := check(waitCtx); done {
			return finish(err)
		}
	} else {
		// Fetch first so the budget can come from the Hub's current view.
		done, err := check(ctx)
		budgetAgent := last
		if budgetAgent == nil {
			budgetAgent = start
		}
		waitCtx, cancel = context.WithTimeout(ctx, budget(budgetAgent))
		defer cancel()
		if done {
			return finish(err)
		}
	}

	interval := opts.PollInterval
	if interval <= 0 {
		interval = launchPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				return finish(launchInterruptedError(ctx, agentID))
			}
			if !failedSince.IsZero() {
				// The budget ran out while waiting for the failed launch's
				// code: report the failure that was seen.
				return finish(nil)
			}
			return finish(fmt.Errorf("agent %q: %w", agentID, ErrLaunchWaitTimeout))
		case <-ticker.C:
			if done, err := check(waitCtx); done {
				return finish(err)
			}
		}
	}
}

// launchInterruptedError wraps the error of a done ctx, and its cause when
// that differs, so errors.Is matches both.
func launchInterruptedError(ctx context.Context, agentID string) error {
	err, cause := ctx.Err(), context.Cause(ctx)
	if cause != nil && cause != err {
		return fmt.Errorf("stopped waiting for agent %q launch: %w: %w", agentID, err, cause)
	}
	return fmt.Errorf("stopped waiting for agent %q launch: %w", agentID, err)
}

// EnsureRunningBeforeSendByID reads the agent once (GET agent by ID or
// slug) and, when it shows an active launch, waits for the launch as
// EnsureRunningBeforeSend does. It returns nil at once when the agent is not
// launching. A failed read is returned wrapped. Callers use it before
// sending a message to an agent that may still be launching, which the Hub
// would refuse; it is the form for callers that hold only an ID or slug, or
// an agent read earlier.
func EnsureRunningBeforeSendByID(ctx context.Context, svc AgentService, agentID string, opts LaunchWaitOptions) error {
	if svc == nil {
		return errors.New("hubclient: EnsureRunningBeforeSendByID: nil agent service")
	}
	getCtx, cancel := context.WithTimeout(ctx, launchFetchTimeout)
	a, err := svc.Get(getCtx, agentID)
	cancel()
	if err != nil {
		return fmt.Errorf("reading agent %q before sending: %w", agentID, err)
	}
	if a == nil {
		return fmt.Errorf("reading agent %q before sending: empty answer", agentID)
	}
	return ensureRunning(ctx, svc, agentID, a, opts)
}

// EnsureRunningBeforeSend returns nil at once, without reading the agent,
// when a shows no active launch. Otherwise it waits for the launch with
// WaitForLaunch and returns nil when the agent is running,
// *LaunchNotRunningError (matching ErrLaunchNotRunning) when the launch
// ended without running, or the wait's error.
//
// a must be fresh: the create, start or restart answer, or an agent just
// read. A stale agent that shows no active launch skips the wait, and the
// send may then be refused because the agent is still launching. Callers
// without a fresh agent use EnsureRunningBeforeSendByID.
func EnsureRunningBeforeSend(ctx context.Context, svc AgentService, a *Agent, opts LaunchWaitOptions) error {
	if a == nil {
		return errors.New("hubclient: EnsureRunningBeforeSend: nil agent")
	}
	return ensureRunning(ctx, svc, a.ID, a, opts)
}

// ensureRunning waits for a's launch, reading the agent by id, when a shows
// one; a.ID is preferred over id when set.
func ensureRunning(ctx context.Context, svc AgentService, id string, a *Agent, opts LaunchWaitOptions) error {
	if !LaunchActive(a) {
		return nil
	}
	if a.ID != "" {
		id = a.ID
	}
	out, err := WaitForLaunch(ctx, svc, id, a, opts)
	if err != nil {
		return err
	}
	if !out.Running {
		return &LaunchNotRunningError{Outcome: out}
	}
	return nil
}

// isTerminalFetchError reports whether a read failed with a client error
// that a retry will not fix: any 4xx except 408 and 429.
func isTerminalFetchError(err error) bool {
	var apiErr *apiclient.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	code := apiErr.StatusCode
	if code < 400 || code >= 500 {
		return false
	}
	return code != http.StatusRequestTimeout && code != http.StatusTooManyRequests
}

// isUnreadableStatusError reports whether a read was refused (403) or
// answered not-found (404).
func isUnreadableStatusError(err error) bool {
	return apiclient.IsNotFoundError(err) || apiclient.IsForbiddenError(err)
}

// retryUnreadableStatus retries a read that failed with an
// unreadable-status error, with backoff, for launchUnreadableGrace; the
// last delay is shortened to end at the grace. It returns the first
// success, the first other error, or the last unreadable-status error when
// the grace runs out or ctx ends (also when ctx ends during a read).
func retryUnreadableStatus(ctx context.Context, get func(context.Context) (*Agent, error), err error) (*Agent, error) {
	deadline := launchNow().Add(launchUnreadableGrace)
	delay := launchUnreadableBackoff
	for {
		remaining := deadline.Sub(launchNow())
		if remaining <= 0 {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-launchAfter(min(delay, remaining)):
		}
		a, getErr := get(ctx)
		if getErr != nil && ctx.Err() != nil {
			return nil, err // a read cut short by ctx says nothing new
		}
		if getErr == nil || !isUnreadableStatusError(getErr) {
			return a, getErr
		}
		err = getErr
		delay = min(delay*2, launchUnreadableBackoffMax)
	}
}
