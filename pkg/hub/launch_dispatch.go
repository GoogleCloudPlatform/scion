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

package hub

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// LaunchAccepted records that a broker accepted a create for asynchronous
// launch: ID is the Hub launch ID the
// broker echoed and Owner is the broker process instance that claimed it.
type LaunchAccepted struct {
	ID    string `json:"id"`
	Owner string `json:"owner,omitempty"`
}

// CreateDispatchResult is the outcome of a launching create dispatch
// (DispatchAgentCreate, DispatchAgentCreateWithGather, DispatchFinalizeEnv).
// A nil result means the create completed synchronously, as before.
//
// At most one field is set:
//   - EnvReqs: the broker answered 202 and needs more env (env-gather).
//   - Launch: the broker accepted the create for asynchronous launch; the
//     agent row is in provisioning and the broker reports the outcome later.
//     Callers must not merge phase or message from the in-memory agent copy
//     in this case (the "accepted branch").
type CreateDispatchResult struct {
	EnvReqs *RemoteEnvRequirementsResponse `json:"envRequirements,omitempty"`
	Launch  *LaunchAccepted                `json:"launch,omitempty"`
}

// EnvRequirements returns r.EnvReqs, or nil when r is nil.
func (r *CreateDispatchResult) EnvRequirements() *RemoteEnvRequirementsResponse {
	if r == nil {
		return nil
	}
	return r.EnvReqs
}

// AcceptedLaunch returns r.Launch, or nil when r is nil.
func (r *CreateDispatchResult) AcceptedLaunch() *LaunchAccepted {
	if r == nil {
		return nil
	}
	return r.Launch
}

// envReqsResult wraps env requirements in a CreateDispatchResult, keeping a
// nil result for nil requirements.
func envReqsResult(envReqs *RemoteEnvRequirementsResponse) *CreateDispatchResult {
	if envReqs == nil {
		return nil
	}
	return &CreateDispatchResult{EnvReqs: envReqs}
}

// AsyncLaunchSettings are the Hub settings that govern asynchronous agent
// launch.
type AsyncLaunchSettings struct {
	// Enabled is the hub.async_agent_launch experiment, resolved when the
	// settings are read.
	Enabled bool
	// Timeout is the whole-launch budget (hub.launchTimeout).
	Timeout time.Duration
	// KeepaliveSeconds is sent to the broker as launchKeepaliveSeconds.
	KeepaliveSeconds int
}

// asyncLaunchSettings returns the server's async launch settings. The
// dispatcher calls it at every create send, so Enabled follows the
// hub.async_agent_launch experiment without a restart. Timeout and
// KeepaliveSeconds are fixed at startup; read under s.mu like the other
// config fields.
func (s *Server) asyncLaunchSettings() AsyncLaunchSettings {
	enabled := s.experimentEnabled(experiments.AsyncAgentLaunch)
	s.mu.RLock()
	defer s.mu.RUnlock()
	return AsyncLaunchSettings{
		Enabled:          enabled,
		Timeout:          s.config.LaunchTimeout,
		KeepaliveSeconds: s.config.LaunchKeepaliveSeconds,
	}
}

// SetAsyncLaunchSettingsProvider registers the accessor for the async launch
// settings. Nil (the default) keeps every create synchronous.
func (d *HTTPAgentDispatcher) SetAsyncLaunchSettingsProvider(fn func() AsyncLaunchSettings) {
	d.asyncLaunchSettings = fn
}

// ErrLaunchInvalidPhase is returned by a launching create dispatch when
// BeginLaunch refused because the agent left the created/provisioning phases
// (for example a stop landed first). Nothing was sent to the broker, and the
// caller must not fall back to a synchronous send. When a delete holds the
// row the error also wraps store.ErrDeleteInProgress (ptone/scion#2550).
var ErrLaunchInvalidPhase = errors.New("agent is no longer in a phase that can be launched")

// errLaunchEchoMismatch is returned when the broker answered launchPending
// for a launch ID other than the one this node began. The launch is ended
// and the create is treated as failed; the broker's own launch is refused at
// its first claim.
var errLaunchEchoMismatch = errors.New("broker acknowledged a different launch than the one requested")

// createRouteChecker is implemented by broker clients whose
// CreateAgentWithGather can return ErrLifecycleDeferred before sending
// anything (HybridBrokerClient). dispatchLaunching uses it to avoid writing a
// launch on a node that will hand the create to the broker's owner node.
type createRouteChecker interface {
	createWithGatherWouldDefer(ctx context.Context, brokerID, brokerEndpoint string) bool
}

// createWithGatherWouldDefer reports whether CreateAgentWithGather would
// return ErrLifecycleDeferred for brokerID without sending.
func (c *HybridBrokerClient) createWithGatherWouldDefer(ctx context.Context, brokerID, brokerEndpoint string) bool {
	switch c.route(ctx, brokerID, brokerEndpoint) {
	case routeLocal, routeHTTP:
		return false
	default:
		return true
	}
}

// createSendFunc performs one transport send of a create request, including
// any hash-mismatch repair and resend, so one launch covers every attempt.
type createSendFunc func(ctx context.Context, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error)

// asyncLaunchEligible reports whether a create send for agent may request an
// asynchronous launch: the hub.async_agent_launch experiment is on, the
// agent opted in, the
// request launches (not provision-only or reprovision), and the broker is not
// known to lack support. A broker with no recorded capabilities, or one that
// cannot be read, is tried: its echo is authoritative.
func (d *HTTPAgentDispatcher) asyncLaunchEligible(ctx context.Context, agent *store.Agent, req *RemoteCreateAgentRequest) (AsyncLaunchSettings, bool) {
	if d.asyncLaunchSettings == nil || d.store == nil {
		return AsyncLaunchSettings{}, false
	}
	settings := d.asyncLaunchSettings()
	if !settings.Enabled || !agent.LaunchAsyncOptIn || req.ProvisionOnly || req.Reprovision {
		return settings, false
	}
	if settings.Timeout <= 0 {
		return settings, false
	}
	if broker, err := d.store.GetRuntimeBroker(ctx, agent.RuntimeBrokerID); err == nil && broker != nil && broker.Capabilities != nil && !broker.Capabilities.AsyncLaunch {
		return settings, false
	}
	return settings, true
}

// minAsyncStartLaunchTimeout is the smallest launch timeout an
// asynchronous start is sent with: the broker takes its asynchronous
// branch only when launchTimeoutSeconds is above 20 (as for a create), so
// a shorter budget would only be answered synchronously.
const minAsyncStartLaunchTimeout = 21 * time.Second

// asyncStartEligible reports whether a start send for agent may request an
// asynchronous launch (ptone/scion#4669): the caller asked for it
// (opts.Async, after its own checks: the hub.async_agent_start experiment,
// every live hub advertising async_start_v1, the request's opt-in and the
// prior phase), the hub.async_agent_launch experiment is on, the launch
// timeout leaves the broker room to go asynchronous, and the broker is not
// known to lack AsyncStart. A broker with no recorded capabilities, or one
// that cannot be read, is tried: its echo is authoritative.
func (d *HTTPAgentDispatcher) asyncStartEligible(ctx context.Context, agent *store.Agent, opts StartDispatchOptions) (AsyncLaunchSettings, bool) {
	if !opts.Async || d.asyncLaunchSettings == nil || d.store == nil {
		return AsyncLaunchSettings{}, false
	}
	settings := d.asyncLaunchSettings()
	if !settings.Enabled || settings.Timeout < minAsyncStartLaunchTimeout {
		return settings, false
	}
	if broker, err := d.store.GetRuntimeBroker(ctx, agent.RuntimeBrokerID); err == nil && broker != nil && broker.Capabilities != nil && !broker.Capabilities.AsyncStart {
		return settings, false
	}
	return settings, true
}

// beginStartLaunch begins the asynchronous launch of a start when it is
// eligible (asyncStartEligible) and this node will send it, and returns its
// ID, the settings and the time its budget started. An empty ID with a nil
// error means the start is sent synchronously: it is not eligible, the
// client will hand it to the owner node (which begins its own launch from
// the intent's options), or the row is not in a phase a launch of that
// kind may begin on (BeginStartLaunch's ErrInvalidPhase; design D-b). A
// row a delete holds returns ErrLaunchInvalidPhase wrapping
// store.ErrDeleteInProgress, as for a create; any other store error is
// returned. revokeOnFail is this call's synchronous revoke decision
// (DispatchAgentStart's rule); a restart's launch never revokes. runID is
// the run beginRun minted for this dispatch, bound as the launch's
// launch_run_id (WP1 slice 3 addendum section 1).
func (d *HTTPAgentDispatcher) beginStartLaunch(ctx context.Context, agent *store.Agent, endpoint string, opts StartDispatchOptions, revokeOnFail bool, runID string) (string, AsyncLaunchSettings, time.Time, error) {
	settings, eligible := d.asyncStartEligible(ctx, agent, opts)
	if !eligible {
		return "", settings, time.Time{}, nil
	}
	if rc, ok := d.client.(createRouteChecker); ok && rc.createWithGatherWouldDefer(ctx, agent.RuntimeBrokerID, endpoint) {
		return "", settings, time.Time{}, nil
	}
	kind := opts.launchKind()
	// The timer starts immediately before BeginStartLaunch, so the
	// remaining budget sent to the broker never exceeds the stored deadline.
	began := time.Now()
	launchID, err := d.store.BeginStartLaunch(ctx, agent.ID, kind, settings.Timeout, store.BeginLaunchOptions{
		PriorPhase:   opts.PriorPhase,
		RevokeOnFail: kind == store.LaunchKindStart && revokeOnFail,
		RunID:        runID,
	})
	if err != nil {
		if errors.Is(err, store.ErrInvalidPhase) {
			if fresh, gerr := d.store.GetAgent(ctx, agent.ID); gerr == nil &&
				(!fresh.DeletedAt.IsZero() || fresh.DeletionHoldsRow(time.Now())) {
				return "", settings, time.Time{}, fmt.Errorf("%w: %w: %w", ErrLaunchInvalidPhase, err, store.ErrDeleteInProgress)
			}
			d.log.Info("Start not eligible for async launch in its current phase; sending synchronously",
				"agent_id", agent.ID, "kind", kind, "error", err)
			return "", settings, time.Time{}, nil
		}
		return "", settings, time.Time{}, fmt.Errorf("begin start launch: %w", err)
	}
	return launchID, settings, began, nil
}

// remainingLaunchSeconds is ceil(timeout - elapsed) in whole seconds, at
// least 1.
func remainingLaunchSeconds(timeout, elapsed time.Duration) int {
	remaining := int(math.Ceil((timeout - elapsed).Seconds()))
	if remaining < 1 {
		return 1
	}
	return remaining
}

// dispatchLaunching wraps one transport send of a launching create request.
// When the send is eligible for asynchronous launch it begins a launch,
// sends the launch fields and resolves the launch from the broker's answer:
//
//   - ErrLifecycleDeferred: returned unchanged with no launch write here; the
//     owner node runs dispatchLaunching itself.
//   - an error whose outcome is unknown (isUnknownDispatchOutcome: the
//     request may have reached the broker): the launch is kept and returned
//     as accepted with no owner, and no error (ptone/scion#4866).
//   - any other error: the launch is ended as not_launched with the code
//     dispatch_failed, so a client following it sees a failed launch (the
//     env and bootstrap finalize callers keep the row in provisioning).
//   - env requirements, or a synchronous answer from a broker without async
//     support: the launch is ended as not_launched without a code. Neither
//     is a failure: the start goes on, or waits for env (see each case).
//   - launchPending with the matching ID: the launch is marked accepted and
//     returned. The agent row is now provisioning.
//   - launchPending with another ID: the launch is ended as not_launched
//     with the code dispatch_failed and errLaunchEchoMismatch is returned.
//
// deferrable marks sends whose transport may defer to the owner node; for
// those, a client that can predict deferral is asked first so no launch is
// written on this node.
func (d *HTTPAgentDispatcher) dispatchLaunching(
	ctx context.Context,
	agent *store.Agent,
	endpoint string,
	req *RemoteCreateAgentRequest,
	deferrable bool,
	send createSendFunc,
) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, *LaunchAccepted, error) {
	// A request can be resent (the as_needed replay): never carry a previous
	// send's launch fields into this one.
	req.AsyncLaunch = false
	req.LaunchID = ""
	req.LaunchTimeoutSeconds = 0
	req.LaunchKeepaliveSeconds = 0

	settings, eligible := d.asyncLaunchEligible(ctx, agent, req)
	if !eligible {
		resp, envReqs, err := send(ctx, req)
		return resp, envReqs, nil, err
	}
	if deferrable {
		if rc, ok := d.client.(createRouteChecker); ok && rc.createWithGatherWouldDefer(ctx, agent.RuntimeBrokerID, endpoint) {
			return nil, nil, nil, ErrLifecycleDeferred
		}
	}

	// The timer starts immediately before BeginLaunch, so the remaining
	// budget sent to the broker never exceeds the stored deadline.
	began := time.Now()
	// req.RunID is the run beginRun minted for this dispatch (set by the
	// caller before the send); the launch binds it as launch_run_id.
	launchID, err := d.store.BeginLaunchWithOptions(ctx, agent.ID, store.LaunchKindCreate, settings.Timeout,
		store.BeginLaunchOptions{RunID: req.RunID})
	if err != nil {
		if errors.Is(err, store.ErrInvalidPhase) {
			// A delete that claimed the row after beginRun moved it out of
			// the launchable phases: say so (ptone/scion#2550). The error
			// still wraps ErrLaunchInvalidPhase and store.ErrInvalidPhase,
			// so callers leave the row to the delete as for any other
			// phase change.
			if fresh, gerr := d.store.GetAgent(ctx, agent.ID); gerr == nil &&
				(!fresh.DeletedAt.IsZero() || fresh.DeletionHoldsRow(time.Now())) {
				return nil, nil, nil, fmt.Errorf("%w: %w: %w", ErrLaunchInvalidPhase, err, store.ErrDeleteInProgress)
			}
			return nil, nil, nil, fmt.Errorf("%w: %w", ErrLaunchInvalidPhase, err)
		}
		return nil, nil, nil, fmt.Errorf("begin launch: %w", err)
	}
	return d.sendLaunching(ctx, agent, launchID, settings, began, "Agent create accepted for async launch",
		func(ctx context.Context, f launchFields) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
			req.AsyncLaunch = true
			req.LaunchID = f.LaunchID
			req.LaunchTimeoutSeconds = f.TimeoutSeconds
			req.LaunchKeepaliveSeconds = f.KeepaliveSeconds
			return send(ctx, req)
		})
}

// launchFields are the asynchronous launch fields of one launching send:
// the launch ID, the remaining whole-launch budget and the keepalive.
type launchFields struct {
	LaunchID         string
	TimeoutSeconds   int
	KeepaliveSeconds int
	// Remaining recomputes the remaining whole-launch budget, for a
	// resend within the same send (a hash-mismatch repair), so the budget
	// the broker gets never exceeds the stored deadline. Nil means
	// TimeoutSeconds.
	Remaining func() int
}

// remaining returns f.Remaining(), or f.TimeoutSeconds when it is nil.
func (f launchFields) remaining() int {
	if f.Remaining == nil {
		return f.TimeoutSeconds
	}
	return f.Remaining()
}

// launchSendFunc performs the transport send of a launching request with
// f applied, including any resend (a hash-mismatch repair), so one launch
// covers every attempt.
type launchSendFunc func(ctx context.Context, f launchFields) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error)

// sendLaunching is the kind-generic part of a launching send, shared by
// create (dispatchLaunching) and start (dispatchAgentStart): launchID has
// been begun (at began) and the send carries its fields. It resolves the
// launch from the broker's answer as dispatchLaunching documents.
// acceptedMsg is the log line of an accepted launch.
func (d *HTTPAgentDispatcher) sendLaunching(
	ctx context.Context,
	agent *store.Agent,
	launchID string,
	settings AsyncLaunchSettings,
	began time.Time,
	acceptedMsg string,
	send launchSendFunc,
) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, *LaunchAccepted, error) {
	resp, envReqs, err := send(ctx, launchFields{
		LaunchID:         launchID,
		TimeoutSeconds:   remainingLaunchSeconds(settings.Timeout, time.Since(began)),
		KeepaliveSeconds: settings.KeepaliveSeconds,
		Remaining:        func() int { return remainingLaunchSeconds(settings.Timeout, time.Since(began)) },
	})
	switch {
	case errors.Is(err, ErrLifecycleDeferred):
		// Raised by routing before any send. The owner's BeginLaunch
		// supersedes launchID; no further write here.
		return nil, nil, nil, err
	case err != nil && isUnknownDispatchOutcome(err):
		// The broker may have received the request and be launching
		// (ptone/scion#4866): ending the launch here would make its claim
		// answer 409 not_launched and tear the launch down. Accept the
		// launch exactly as an acknowledged one, with no owner yet (the
		// row moves to provisioning, so callers, the CLI and the web
		// follow it as any accepted launch): the broker's first claim
		// records the owner, and if the request never arrived the launch
		// reaper ends the launch (lost, or timed out at its deadline). No
		// error is returned, so the minted credential is kept, as for any
		// accepted launch.
		d.log.Warn("Async launch send outcome unknown; keeping the launch for the broker's claim or the launch reaper",
			"agent_id", agent.ID, "launch_id", launchID, "error", err)
		d.markLaunchAccepted(ctx, agent, launchID, "")
		addDispatchWarnings(ctx, launchOutcomeUnknownWarning)
		return nil, nil, &LaunchAccepted{ID: launchID}, nil
	case err != nil:
		// The broker refused the launch or was not reached: a failure. The
		// code tells a client following the launch that it ended, whatever
		// the caller then does with the row (ptone/scion#5283).
		d.endLaunchNotLaunchedWithCode(ctx, agent, launchID, store.LaunchErrorDispatchFailed)
		return nil, nil, nil, err
	case envReqs != nil:
		// Waiting for a human to supply env costs no launch budget. No
		// code: this is not a failure. The same request may begin a new
		// launch at once (the gather second pass, the finalize retry), or
		// the row waits in provisioning for the env to be submitted.
		d.endLaunchNotLaunched(ctx, agent, launchID)
		return resp, envReqs, nil, nil
	case resp != nil && resp.LaunchPending && resp.LaunchID == launchID:
		d.markLaunchAccepted(ctx, agent, launchID, resp.LaunchInstanceID)
		d.log.Info(acceptedMsg,
			"agent_id", agent.ID, "project_id", agent.ProjectID, "launch_id", launchID,
			"launched_by", agent.CreatedBy, "relationship", launchRelationship(agent))
		return resp, nil, &LaunchAccepted{ID: launchID, Owner: resp.LaunchInstanceID}, nil
	case resp == nil || !resp.LaunchPending:
		// A broker without async support answered synchronously. No code:
		// the start goes on outside the launch record, and the caller
		// applies the broker's phase from the answer.
		d.endLaunchNotLaunched(ctx, agent, launchID)
		return resp, nil, nil, nil
	default:
		// The broker is not running the launch the hub began: a failure.
		d.endLaunchNotLaunchedWithCode(ctx, agent, launchID, store.LaunchErrorDispatchFailed)
		d.log.Warn("Broker acknowledged a different launch ID; ending the launch",
			"agent_id", agent.ID, "launch_id", launchID, "broker_launch_id", resp.LaunchID)
		return nil, nil, nil, errLaunchEchoMismatch
	}
}

// markLaunchAccepted records launchID as accepted (owner may be empty: the
// broker's first claim records it). It uses a context detached from the
// request, like endLaunchNotLaunched: the broker may already be launching,
// so a client disconnect must not drop this write.
func (d *HTTPAgentDispatcher) markLaunchAccepted(ctx context.Context, agent *store.Agent, launchID, owner string) {
	markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := d.store.MarkLaunchAccepted(markCtx, agent.ID, launchID, owner); err != nil {
		// The broker is launching; its first claim records the owner
		// if this write was lost.
		d.log.Warn("Failed to record accepted launch",
			"agent_id", agent.ID, "launch_id", launchID, "error", err)
	}
}

// launchOutcomeUnknownWarning is the dispatch warning for a launch kept
// after a send whose outcome is unknown.
const launchOutcomeUnknownWarning = "The runtime broker did not confirm the launch request; the launch is kept and settles when the broker reports it, or ends when the launch times out."

// endLaunchNotLaunched ends launchID as not_launched without a code, for
// the cases that are not failures. It uses a context detached from the
// request so a client disconnect cannot strand the launch.
func (d *HTTPAgentDispatcher) endLaunchNotLaunched(ctx context.Context, agent *store.Agent, launchID string) {
	d.endLaunchNotLaunchedWithCode(ctx, agent, launchID, "")
}

// endLaunchNotLaunchedWithCode ends launchID as not_launched and records
// code ("" for none) as the launch error, like endLaunchNotLaunched.
func (d *HTTPAgentDispatcher) endLaunchNotLaunchedWithCode(ctx context.Context, agent *store.Agent, launchID, code string) {
	endCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := d.store.EndLaunchWithCode(endCtx, agent.ID, launchID, store.LaunchEndReasonNotLaunched, code); err != nil {
		d.log.Warn("Failed to end launch",
			"agent_id", agent.ID, "launch_id", launchID, "error", err)
	}
}

// applyAcceptedLaunchResponse applies a broker's accepted-launch answer to
// the in-memory agent: the non-status fields only. Phase, activity and
// container status come from the launch reports, not from this answer.
func (d *HTTPAgentDispatcher) applyAcceptedLaunchResponse(ctx context.Context, agent *store.Agent, resp *RemoteAgentResponse) {
	d.forgetRuntimeTarget(ctx, agent)
	if resp != nil && resp.Agent != nil {
		applyBrokerAgentConfig(agent, resp.Agent)
	}
}

// persistAcceptedLaunch is the accepted branch's write: it
// re-reads the row (which the launch already moved to provisioning), merges
// the non-status fields from the dispatched copy, retries on a version
// conflict, and returns the persisted row. On failure it returns the last
// row it read, or the dispatched copy when nothing could be read.
func (s *Server) persistAcceptedLaunch(ctx context.Context, dispatched *store.Agent) (*store.Agent, error) {
	const attempts = 3
	var lastErr error
	result := dispatched
	for i := 0; i < attempts; i++ {
		latest, err := s.store.GetAgent(ctx, dispatched.ID)
		if err != nil {
			return result, err
		}
		result = latest
		mergeDispatchedConfig(latest, dispatched)
		lastErr = s.store.UpdateAgent(lifecycleConfigResolution(ctx), latest)
		if lastErr == nil {
			if fresh, err := s.store.GetAgent(ctx, dispatched.ID); err == nil {
				return fresh, nil
			}
			return latest, nil
		}
		if !errors.Is(lastErr, store.ErrVersionConflict) {
			return result, lastErr
		}
	}
	return result, lastErr
}

// mergeDispatchedConfig copies the non-status fields a create dispatch sets
// on its in-memory copy onto dst. It is mergeDispatchedAgent without the
// status fields (phase, activity, message, container and runtime state).
func mergeDispatchedConfig(dst, src *store.Agent) {
	if src.Template != "" {
		dst.Template = src.Template
	}
	if src.Image != "" {
		dst.Image = src.Image
	}
	if src.Runtime != "" {
		dst.Runtime = src.Runtime
	}
	if src.AppliedConfig != nil {
		dst.AppliedConfig = src.AppliedConfig
	}
	if src.TaskSummary != "" {
		dst.TaskSummary = src.TaskSummary
	}
}

// launchRelationship names, for the async launch log line, how the
// launcher relates to the new agent: "direct_launcher_agent" when an agent
// launched it (that agent may read its status through the single-agent
// GET routes), "direct_launcher_user" when a user launched it as a root
// agent, and "other" for any other stored record.
func launchRelationship(agent *store.Agent) string {
	n := len(agent.Ancestry)
	switch {
	case n == 1 && agent.Ancestry[0] == agent.CreatedBy:
		return "direct_launcher_user"
	case n > 1 && agent.Ancestry[n-1] == agent.CreatedBy:
		return "direct_launcher_agent"
	default:
		return "other"
	}
}
