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
	// Enabled is the hub.asyncAgentLaunch flag.
	Enabled bool
	// Timeout is the whole-launch budget (hub.launchTimeout).
	Timeout time.Duration
	// KeepaliveSeconds is sent to the broker as launchKeepaliveSeconds.
	KeepaliveSeconds int
}

// asyncLaunchSettings returns the server's async launch settings. They are
// fixed at startup; read under s.mu like the other config fields.
func (s *Server) asyncLaunchSettings() AsyncLaunchSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return AsyncLaunchSettings{
		Enabled:          s.config.AsyncAgentLaunch,
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
// caller must not fall back to a synchronous send.
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
// asynchronous launch: the flag is on, the agent opted in, the
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
//   - error, env requirements, or a synchronous answer from a broker without
//     async support: the launch is ended as not_launched.
//   - launchPending with the matching ID: the launch is marked accepted and
//     returned. The agent row is now provisioning.
//   - launchPending with another ID: the launch is ended and
//     errLaunchEchoMismatch is returned.
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
	launchID, err := d.store.BeginLaunch(ctx, agent.ID, store.LaunchKindCreate, settings.Timeout)
	if err != nil {
		if errors.Is(err, store.ErrInvalidPhase) {
			return nil, nil, nil, fmt.Errorf("%w: %w", ErrLaunchInvalidPhase, err)
		}
		return nil, nil, nil, fmt.Errorf("begin launch: %w", err)
	}
	req.AsyncLaunch = true
	req.LaunchID = launchID
	req.LaunchTimeoutSeconds = remainingLaunchSeconds(settings.Timeout, time.Since(began))
	req.LaunchKeepaliveSeconds = settings.KeepaliveSeconds

	resp, envReqs, err := send(ctx, req)
	switch {
	case errors.Is(err, ErrLifecycleDeferred):
		// Raised by routing before any send. The owner's BeginLaunch
		// supersedes launchID; no further write here.
		return nil, nil, nil, err
	case err != nil:
		d.endLaunchNotLaunched(ctx, agent, launchID)
		return nil, nil, nil, err
	case envReqs != nil:
		// Waiting for a human to supply env costs no launch budget.
		d.endLaunchNotLaunched(ctx, agent, launchID)
		return resp, envReqs, nil, nil
	case resp != nil && resp.LaunchPending && resp.LaunchID == launchID:
		// Detached from the request, like endLaunchNotLaunched: the broker
		// is already launching, so a client disconnect must not drop this
		// write.
		markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, err := d.store.MarkLaunchAccepted(markCtx, agent.ID, launchID, resp.LaunchInstanceID)
		if err != nil {
			// The broker is launching; its first claim records the owner
			// if this write was lost.
			d.log.Warn("Failed to record accepted launch",
				"agent_id", agent.ID, "launch_id", launchID, "error", err)
		}
		return resp, nil, &LaunchAccepted{ID: launchID, Owner: resp.LaunchInstanceID}, nil
	case resp == nil || !resp.LaunchPending:
		// A broker without async support answered synchronously.
		d.endLaunchNotLaunched(ctx, agent, launchID)
		return resp, nil, nil, nil
	default:
		d.endLaunchNotLaunched(ctx, agent, launchID)
		d.log.Warn("Broker acknowledged a different launch ID; ending the launch",
			"agent_id", agent.ID, "launch_id", launchID, "broker_launch_id", resp.LaunchID)
		return nil, nil, nil, errLaunchEchoMismatch
	}
}

// endLaunchNotLaunched ends launchID as not_launched. It uses a context
// detached from the request so a client disconnect cannot strand the launch.
func (d *HTTPAgentDispatcher) endLaunchNotLaunched(ctx context.Context, agent *store.Agent, launchID string) {
	endCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := d.store.EndLaunch(endCtx, agent.ID, launchID, store.LaunchEndReasonNotLaunched); err != nil {
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
		lastErr = s.store.UpdateAgent(ctx, latest)
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
