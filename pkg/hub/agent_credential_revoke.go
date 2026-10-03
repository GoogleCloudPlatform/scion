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
	"log/slog"
	"net"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Credential revoke reasons recorded on the agent_credentials row (mirrors
// the existing agent_deleted/agent_suspended/refreshed reasons already used
// by the delete, suspend and refresh paths).
const (
	// agentCredentialRevokeReasonCreateFailed covers every create/provision
	// path: sync create, provision/reprovision, create-with-gather (including
	// the handler-level "required env still missing" failure, which the
	// dispatcher itself reports as success), finalize-env, and a failed
	// async-create launch report.
	agentCredentialRevokeReasonCreateFailed = "create_failed"

	// agentCredentialRevokeReasonStartFailed covers a failed DispatchAgentStart
	// (fresh start, resume from suspended/stopped, post-reprovision start,
	// wake). It is never used for DispatchAgentRestart or
	// DispatchAgentResetAuth, which dispatch to a presumed-running agent —
	// revoking every active credential there risks revoking the one a still-
	// running container is actively using over a dispatch failure that says
	// nothing about that container's health.
	agentCredentialRevokeReasonStartFailed = "start_failed"

	// agentCredentialRevokeReasonDeleted covers an agent row's removal: the
	// main delete handler, and any other path that hard-deletes an agent
	// row outright (e.g. tearing down a stale provisioning row immediately
	// before recreating it) rather than ending a create/start attempt in
	// place.
	agentCredentialRevokeReasonDeleted = "agent_deleted"
)

// agentCredentialRevokeTimeout bounds the best-effort revoke call so it
// cannot hang a request/dispatch path indefinitely.
const agentCredentialRevokeTimeout = 5 * time.Second

// revokeAgentCredentialsBestEffort revokes every active credential for
// agentID, logging a warning on failure. It never returns an error: callers
// use it after a create/launch failure has already been decided, and a
// revoke-store failure must not mask or replace that original error.
//
// It detaches from ctx's cancellation before calling the store, because ctx
// may already be cancelled or past its deadline by the time a dispatch
// failure is being handled (the request that triggered the failure may be
// unwinding) — see agent_dm_delivery.go's identical pattern for finalization
// calls made from an error path.
//
// A nil credStore is logged and skipped rather than dereferenced, so the
// helper keeps its no-error, no-panic contract on every caller's error path.
func revokeAgentCredentialsBestEffort(ctx context.Context, credStore store.AgentCredentialStore, agentID, reason string) {
	if credStore == nil {
		slog.Warn("Skipping agent credential revoke: no credential store configured", "agent_id", agentID, "reason", reason)
		return
	}
	revokeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), agentCredentialRevokeTimeout)
	defer cancel()
	if err := revokeAgentCredentials(revokeCtx, credStore, agentID, reason); err != nil {
		slog.Warn("Failed to revoke agent credentials", "agent_id", agentID, "reason", reason, "error", err)
	}
}

// errNoAgentCredentialStore is returned by revokeAgentCredentials when no
// credential store is configured.
var errNoAgentCredentialStore = errors.New("no agent credential store configured")

// revokeAgentCredentials revokes every active credential for agentID and
// returns the store's error. It uses ctx as given — no detach, no timeout —
// so a caller that must know whether the revoke landed (the delete engine's
// finalize step, design ptone/scion#2483 §2.3) can act on the error.
// revokeAgentCredentialsBestEffort is the log-and-continue wrapper.
func revokeAgentCredentials(ctx context.Context, credStore store.AgentCredentialStore, agentID, reason string) error {
	if credStore == nil {
		return errNoAgentCredentialStore
	}
	_, err := credStore.RevokeAgentCredentialsByAgent(ctx, agentID, "system", reason)
	return err
}

// isUnconfirmedLaunchError reports whether launchError marks a launch that
// the Hub declared dead on its own, without the broker ever confirming the
// container actually stopped (the reaper in pkg/store/entadapter/
// launch_reaper.go: reapDeadline/reapStaleness). A resume
// dispatch (DispatchAgentStart) must not revoke-by-agent on failure in this
// case: the prior credential may still be in active use by a container the
// Hub has simply lost contact with (e.g. a k8s pod partitioned from the Hub),
// and the start/resume retry path's existing container-name-conflict handling
// (handlers_agent_create_helpers.go isContainerNameConflict) exists precisely
// because that container can still be there.
//
// Every other LaunchError value — including a broker-confirmed crash (exit
// code via heartbeat) or a broker-confirmed launch failure (a "failed"
// launch report) — means the broker itself told the Hub the container is
// gone, so revoking on a subsequent start failure is safe. New LaunchError
// values must be added here only if they are likewise declared by the Hub
// on silence rather than confirmed by the broker: ptone/scion#2485's planned
// container_missing, for example, comes from the broker's own inventory
// reconciliation and should keep revoking, not be added to this list.
func isUnconfirmedLaunchError(launchError string) bool {
	switch launchError {
	case store.LaunchErrorLaunchTimeout, store.LaunchErrorBrokerLost:
		return true
	default:
		return false
	}
}

// isConfirmedNonRunningPhase reports whether phase is one of the agent
// phases where the Hub can be sure no container is up for this agent: it
// was never started (created, provisioning), was cleanly brought down and
// not yet restarted (stopped, suspended), or ended in a broker-confirmed
// failure (error). DispatchAgentStart's start-failure revoke arms only when
// the agent's phase, captured before the dispatch, is one of these.
//
// Every other phase is excluded on purpose, including the transitional
// "starting" and "stopping": a live container can still exist underneath
// both. wake_dm.go leaves a resumed agent in "starting" after a
// readiness-wait timeout even though its container may simply be slow to
// report ready, and reincarnate_worker.go writes "stopping" before its own
// DispatchAgentStop call has confirmed the prior container actually exited.
// "running" is excluded the same way.
//
// An empty phase is treated as confirmed non-running. Every real caller
// that reaches DispatchAgentStart sets a phase before dispatching — at the
// latest "created" (handleExistingAgent's created/provisioning restart
// branch) — so an empty value here means the Hub holds no persisted phase
// for this agent at all, which rules out an already-running container just
// as surely as "created" does.
func isConfirmedNonRunningPhase(phase string) bool {
	switch phase {
	case "",
		string(state.PhaseCreated),
		string(state.PhaseProvisioning),
		string(state.PhaseStopped),
		string(state.PhaseSuspended),
		string(state.PhaseError):
		return true
	default:
		return false
	}
}

// errStartBrokerNotConnected marks an error that means there was no live
// connection to the broker to send a start request over in the first
// place: the control channel manager has no WebSocket connection
// registered for this broker at all (ControlChannelManager.TunnelRequest,
// ControlChannelBrokerClient.doRequest/doRequestRaw's own connectedness
// check). It is wrapped into the error at exactly those call sites, so
// isConfirmedStartNotActedOnError can match it with errors.Is instead of
// matching on message text — text matching also catches an unrelated error
// that merely contains the same words, such as an OS-level "transport
// endpoint is not connected" surfacing from a read or write that happened
// after the request was already on the wire.
var errStartBrokerNotConnected = errors.New("broker not connected")

// errStartRequestNotSent marks an error that means a request was never put on
// the wire at all: the broker has no usable endpoint configured, the
// request body was too large for the control channel, or building,
// marshalling, or signing the request failed before any I/O was attempted
// (brokerHTTPTransport's no-endpoint, marshal, create-request and sign
// failures; ControlChannelBrokerClient's checkBodySize and StartAgent
// marshal failures, and buildRequestHeaders's build and sign failures).
// Wrapped into the error at exactly those call sites, for the same
// errors.Is reason as errStartBrokerNotConnected above.
var errStartRequestNotSent = errors.New("request not sent")

// isConfirmedBrokerRejection reports whether a *brokerStatusError means the
// broker itself explicitly rejected the start request, as opposed to a
// proxy or load balancer in front of it returning a gateway-level error
// after the broker had already received — and potentially acted on — the
// forwarded request (a 504 is a likely shape for this on a slow start).
// Any status below 500 is always the broker's own application-level
// response (validation, not-found, conflict, and so on, all written by
// runtimebroker's writeError). A 500 counts only when the body parses as
// that same JSON error envelope with a non-empty error code; a 500 with
// any other body, and every status above 500 regardless of body, is left
// unconfirmed, since a reverse proxy or load balancer can return those
// after it has already forwarded the request, so they say nothing about
// whether the broker acted on it. A nil e is not a confirmed rejection.
func isConfirmedBrokerRejection(e *brokerStatusError) bool {
	if e == nil {
		return false
	}
	if e.StatusCode < 500 {
		return true
	}
	if e.StatusCode != 500 {
		return false
	}
	return e.brokerErrorCode() != ""
}

// isConfirmedStartNotActedOnError reports whether err, returned from the
// start request a DispatchAgentStart call made to the runtime broker, means
// the Hub can be sure the broker never began acting on that request.
// DispatchAgentStart's start-failure revoke requires this to be true before
// it will fire: rather than naming every failure mode that leaves the
// outcome ambiguous, this names only the failure modes known to be safe,
// and treats everything else — including any failure this function does
// not specifically recognize — as ambiguous. That is the safer direction to
// default toward, the same one isConfirmedNonRunningPhase and
// isUnconfirmedLaunchError both take.
//
// Known-safe cases, where the request either never reached the broker or
// the broker explicitly rejected it, matched by sentinel or type rather
// than by message text (message text also matches errors that merely
// mention the same words, such as a response body that happens to echo
// one — see decodeResponseWithSnippet):
//   - errStartBrokerNotConnected or errStartRequestNotSent (see their doc comments
//     for the exact call sites that wrap them).
//   - a *brokerStatusError that isConfirmedBrokerRejection accepts — the
//     broker received the request and explicitly refused it.
//   - a dial failure (*net.OpError with Op "dial") — the connection to the
//     broker's HTTP endpoint was never established.
//
// Every other error must leave the revoke disarmed, including: a timeout
// waiting for the broker's response (TunnelRequest's "request timeout
// after", an HTTP round trip that hit its deadline), a connection that
// closed or was cancelled while waiting ("connection closed", ctx.Err()),
// an OS-level transport error from a read or write that happened after the
// request was already sent, a *brokerStatusError isConfirmedBrokerRejection
// does not accept, and a response the broker did send that the Hub itself
// failed to read or decode (an HTTP 2xx, or a control-channel response
// under status 400, whose body did not parse as JSON). In every one of
// these the broker may already have started a container using the
// credential this call minted; the error describes only how the Hub
// observed the outcome, not what the broker actually did. (The
// control-channel start path already treats its own undecodable-body case
// as success rather than an error — see ControlChannelBrokerClient.
// StartAgent — so only the HTTP transport's decodeResponseWithSnippet
// error reaches this function via that route.)
func isConfirmedStartNotActedOnError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errStartBrokerNotConnected) || errors.Is(err, errStartRequestNotSent) {
		return true
	}
	var statusErr *brokerStatusError
	if errors.As(err, &statusErr) {
		return isConfirmedBrokerRejection(statusErr)
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	return false
}
