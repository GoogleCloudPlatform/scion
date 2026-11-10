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

package bridge

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// Waiting for an agent that is still launching (ptone/scion#4671).
//
// A hub with async launch answers an auto-provision create that opted in
// (bridge.async_create) as soon as the broker accepts it, with the agent
// still launching. The hub refuses a message to a launching agent (409
// agent_not_running), so every path that sends to such an agent first
// waits for the launch with hubclient's wait helpers: the A2A task reports
// "working" with the launch step while it waits, and fails without a send
// when the launch fails. The same holds for an adopted agent that another
// client is still launching, and for a follow-up to a task whose agent is
// still launching.

// launchKeepAliveFloor is the shortest task keepalive interval.
const launchKeepAliveFloor = time.Second

// waitForAgentLaunch waits for launching's launch when it is active and
// returns nil once the agent is running. It returns at once, without
// reading the agent, when launching is nil or shows no active launch. A
// launch that ends without running returns *hubclient.LaunchNotRunningError;
// a wait that ends first returns the wait's error. onStep, when set, is
// called with each new launch step. The agent is read with the bridge's
// admin client, the identity that made the create or the list.
func (b *Bridge) waitForAgentLaunch(ctx context.Context, launching *hubclient.Agent, onStep func(step string)) error {
	if !hubclient.LaunchActive(launching) {
		return nil
	}
	return hubclient.EnsureRunningBeforeSend(ctx, b.hubClient.Agents(), launching, hubclient.LaunchWaitOptions{
		OnStep:       onStep,
		PollInterval: b.launchPollInterval,
	})
}

// ensureRunningBeforeFollowUp reads the agent once and, when it is still
// launching, waits for the launch as waitForAgentLaunch does, keeping the
// follow-up task (already marked working) alive against the stale-task
// reaper. A failed read is logged and nil is returned, so the send goes
// ahead as it did before the bridge waited for launches; the hub still
// refuses it if the agent is not running. A launch that does not reach
// running returns *launchWaitError; a task that went terminal during the
// wait returns errTaskEndedDuringLaunch. The caller sends only on nil.
func (b *Bridge) ensureRunningBeforeFollowUp(ctx context.Context, agentID, taskID string) error {
	getCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	a, err := b.hubClient.Agents().Get(getCtx, agentID)
	if err != nil || a == nil {
		b.log.Warn("follow-up: reading agent before send failed; sending anyway", "error", err, "agent_id", agentID, "task_id", taskID)
		return nil
	}
	if !hubclient.LaunchActive(a) {
		return nil
	}
	stop := b.keepTaskAlive(ctx, taskID)
	err = b.waitForAgentLaunch(ctx, a, nil)
	stop()
	if err != nil {
		return &launchWaitError{err: err}
	}
	changed, err := b.store.UpdateTaskState(ctx, taskID, TaskStateWorking)
	if err != nil {
		b.log.Warn("launch wait: marking task working failed", "error", err, "task_id", taskID)
		return nil
	}
	if !changed {
		return errTaskEndedDuringLaunch
	}
	return nil
}

// launchStepReporter returns a function that calls emit with the
// provisioning text the first time it is called and then each time the
// step changes.
func launchStepReporter(emit func(text string)) func(step string) {
	emitted := false
	last := ""
	return func(step string) {
		if emitted && step == last {
			return
		}
		emitted = true
		last = step
		emit(launchProvisioningText(step))
	}
}

// launchStep returns the step of a's launch, "" when none.
func launchStep(a *hubclient.Agent) string {
	if a == nil || a.Launch == nil {
		return ""
	}
	return a.Launch.Step
}

// launchProvisioningText is the working-status text while an agent launches.
// The step is shown as its code (for example pod_create).
func launchProvisioningText(step string) string {
	if step == "" {
		return "provisioning agent"
	}
	return "provisioning agent (" + step + ")"
}

// launchOutcomeLabel names a launch outcome for a launch failure text: its
// code, or "phase: <phase>" when it has no code.
func launchOutcomeLabel(out hubclient.LaunchOutcome) string {
	if out.Code != "" {
		return out.Code
	}
	return "phase: " + out.Phase
}

// launchFailureText describes, for an A2A client, why the wait for a
// launching agent ended without the agent running.
func launchFailureText(err error) string {
	var notRunning *hubclient.LaunchNotRunningError
	if errors.As(err, &notRunning) {
		out := notRunning.Outcome
		text := "Agent failed to start (" + launchOutcomeLabel(out) + ")"
		if out.Message != "" {
			text += ": " + out.Message
		}
		return text
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "Request cancelled while waiting for the agent to start"
	}
	return "Could not confirm that the agent started: " + err.Error()
}

// federatedUserLaunchFailureText is launchFailureText for a federated
// user: the launch outcome code (or phase) only. The outcome message and
// a read error come from the bridge's own read of the agent and are not
// shown to a caller from another issuer.
func federatedUserLaunchFailureText(err error) string {
	var notRunning *hubclient.LaunchNotRunningError
	if errors.As(err, &notRunning) {
		return "Agent failed to start (" + launchOutcomeLabel(notRunning.Outcome) + ")"
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "Request cancelled while waiting for the agent to start"
	}
	return "Could not confirm that the agent started"
}

// launchKeepAliveInterval is how often a legacy task waiting for a launch is
// touched (TouchTask), so the stale-task reaper (updated_at older than
// twice the send timeout, see janitor) does not fail it during a long
// launch.
func (b *Bridge) launchKeepAliveInterval() time.Duration {
	if b.launchKeepAlive > 0 {
		return b.launchKeepAlive
	}
	maxAge := 2 * b.effectiveConfig().Timeouts.SendMessage
	if maxAge <= 0 {
		maxAge = 4 * time.Minute
	}
	return max(maxAge/4, launchKeepAliveFloor)
}

// keepTaskAlive touches the legacy task (updated_at only) every keepalive
// interval until the returned stop function is called. stop waits for the
// goroutine to end and may be called more than once.
func (b *Bridge) keepTaskAlive(ctx context.Context, taskID string) (stop func()) {
	done := make(chan struct{})
	exited := make(chan struct{})
	interval := b.launchKeepAliveInterval()
	go func() {
		defer close(exited)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := b.store.TouchTask(ctx, taskID); err != nil {
					b.log.Warn("launch wait: task keepalive failed", "error", err, "task_id", taskID)
				}
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(done) })
		<-exited
	}
}

// errTaskEndedDuringLaunch is returned by waitTaskLaunch when the task was
// cancelled or reaped while the bridge waited for its agent to start.
var errTaskEndedDuringLaunch = errors.New("task ended while waiting for the agent to start")

// launchWaitError reports a wait for a launching agent that ended without
// the agent running. Its text is meant for the A2A client.
type launchWaitError struct{ err error }

func (e *launchWaitError) Error() string { return launchFailureText(e.err) }
func (e *launchWaitError) Unwrap() error { return e.err }

// waitTaskLaunch waits, for the legacy task taskID, for launching's launch
// when it is active; it returns nil at once otherwise. While it waits the
// task is working: it is marked working first and on each step change, and
// kept alive against the stale-task reaper. onWorking, when set, is called
// with the provisioning text first and on each step change. A launch that
// does not reach running fails the task and returns *launchWaitError. A
// task that went terminal during the wait (cancelled or reaped) returns
// errTaskEndedDuringLaunch, whether or not the launch succeeded. The
// caller sends only on nil.
func (b *Bridge) waitTaskLaunch(ctx context.Context, taskID string, launching *hubclient.Agent, onWorking func(text string)) error {
	if !hubclient.LaunchActive(launching) {
		return nil
	}
	markWorking := func() {
		if _, err := b.store.UpdateTaskState(ctx, taskID, TaskStateWorking); err != nil {
			b.log.Warn("launch wait: marking task working failed", "error", err, "task_id", taskID)
		}
	}
	report := launchStepReporter(func(text string) {
		markWorking()
		if onWorking != nil {
			onWorking(text)
		}
	})
	report(launchStep(launching))

	stop := b.keepTaskAlive(ctx, taskID)
	err := b.waitForAgentLaunch(ctx, launching, report)
	stop()
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		changed, updateErr := b.store.UpdateTaskState(cleanupCtx, taskID, TaskStateFailed)
		if updateErr != nil {
			b.log.Error("failed to update task state", "error", updateErr, "task_id", taskID)
		} else if !changed {
			// Cancelled or reaped during the wait: it already ended.
			return errTaskEndedDuringLaunch
		}
		return &launchWaitError{err: err}
	}
	changed, err := b.store.UpdateTaskState(ctx, taskID, TaskStateWorking)
	if err != nil {
		b.log.Warn("launch wait: marking task working failed", "error", err, "task_id", taskID)
		return nil
	}
	if !changed {
		return errTaskEndedDuringLaunch
	}
	return nil
}

// errTaskEndedBeforeFollowUp is returned by awaitFirstSend when the task's
// first send failed (or the task otherwise ended) while the follow-up
// waited for it.
var errTaskEndedBeforeFollowUp = errors.New("task ended before the follow-up could be sent")

// beginFirstSend marks taskID's first send as pending while it waits for a
// launching agent, so a follow-up on the task (awaitFirstSend) is not sent
// before it. The returned function marks the send done; call it once the
// send has completed or failed. Without an active launch nothing is
// recorded and the function does nothing. The record is local to this
// bridge instance.
func (b *Bridge) beginFirstSend(taskID string, launching *hubclient.Agent) (done func()) {
	if !hubclient.LaunchActive(launching) {
		return func() {}
	}
	ch := make(chan struct{})
	b.firstSendMu.Lock()
	if b.firstSends == nil {
		b.firstSends = make(map[string]chan struct{})
	}
	b.firstSends[taskID] = ch
	b.firstSendMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			b.firstSendMu.Lock()
			if b.firstSends[taskID] == ch {
				delete(b.firstSends, taskID)
			}
			b.firstSendMu.Unlock()
			close(ch)
		})
	}
}

// awaitFirstSend waits until taskID's pending first send (beginFirstSend)
// is done, and returns at once when none is pending. It returns
// errTaskEndedBeforeFollowUp when the task ended meanwhile (the first send
// failed, or the task was cancelled or reaped), and the context's error
// when ctx ends first.
func (b *Bridge) awaitFirstSend(ctx context.Context, taskID string) error {
	b.firstSendMu.Lock()
	ch := b.firstSends[taskID]
	b.firstSendMu.Unlock()
	if ch == nil {
		return nil
	}
	select {
	case <-ch:
	case <-ctx.Done():
		return ctx.Err()
	}
	task, err := b.store.GetTask(ctx, taskID)
	if err == nil && task != nil && IsTerminalState(task.State) {
		return errTaskEndedBeforeFollowUp
	}
	return nil
}
