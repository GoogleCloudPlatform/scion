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
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// ReconcileBroker is the exported entry point used by the command-bus signal
// handler (B2-4) to drain durable dispatch intent for a broker this node owns.
func (s *Server) ReconcileBroker(ctx context.Context, brokerID string) {
	s.reconcileBroker(ctx, brokerID)
}

// reconcileBroker drains durable dispatch intent for a broker this node owns:
// pending broker_dispatch rows and pending messages, each CAS-claimed so exactly
// one node executes a given item (design §5.3, §2.0.1). It is the durability
// backstop behind BOTH the command-bus NOTIFY signal and reconnect
// (markBrokerOnline) — so a missed signal or a down owner only delays, never
// loses, a command. Idempotent and safe to run concurrently: the store CAS
// (ClaimBrokerDispatch / MarkMessageDispatched) gates double-execution.
//
// Callers must already hold the broker's control-channel socket (markBrokerOnline
// runs on the accepting node; the command bus filters by ownsLocally), since the
// op executors deliver over the local tunnel.
func (s *Server) reconcileBroker(ctx context.Context, brokerID string) {
	s.drainBrokerDispatch(ctx, brokerID, nil)
}

// drainBrokerDispatch is reconcileBroker's dispatch drain, limited to the
// ops only accepts when only is non-nil.
func (s *Server) drainBrokerDispatch(ctx context.Context, brokerID string, only func(op string) bool) {
	if s == nil || s.store == nil || brokerID == "" {
		return
	}
	drainStart := time.Now()
	defer func() {
		if rec := s.dispatchMetrics; rec != nil {
			rec.RecordReconcileDrainDuration(ctx, float64(time.Since(drainStart).Milliseconds()))
		}
	}()

	// 1. Lifecycle / create-time dispatch intents.
	dispatches, err := s.store.ListPendingDispatch(ctx, brokerID)
	if err != nil {
		s.agentLifecycleLog.Error("reconcile: list pending dispatch failed", "brokerID", brokerID, "error", err)
	}
	for i := range dispatches {
		d := dispatches[i]
		if only != nil && !only(d.Op) {
			continue
		}
		claimed, err := s.store.ClaimBrokerDispatch(ctx, d.ID, s.instanceID)
		if err != nil {
			s.agentLifecycleLog.Error("reconcile: claim dispatch failed", "id", d.ID, "error", err)
			continue
		}
		if !claimed {
			continue // another node/drain owns this intent (exactly-once)
		}
		opAttr := attribute.String("op", d.Op)
		if rec := s.dispatchMetrics; rec != nil {
			rec.IncClaimed(ctx, 1, opAttr)
		}
		// This node is executing a durable intent recorded by another
		// request (possibly on another node), so mark it as deferred
		// execution and log the initiator it was opened under, alongside the
		// dispatch row's own correlation id and the executor that is now
		// running it.
		dispatchCtx := ContextWithExecutor(ctx, ExecutorContext{Kind: "broker_dispatch", ID: d.ID})
		dispatchExecutor, _ := ExecutorContextFromContext(dispatchCtx)
		initiatorLogArgs := []any{
			"id", d.ID, "op", d.Op,
			"initiator_principal_kind", d.InitiatorPrincipalKind,
			"initiator_principal_id", d.InitiatorPrincipalID,
			"initiator_credential_kind", d.InitiatorCredentialKind,
			"initiator_credential_id", d.InitiatorCredentialID,
			"correlation_id", d.CorrelationID,
			"executor_kind", dispatchExecutor.Kind,
			"executor_id", dispatchExecutor.ID,
		}
		result, execErr := s.execDispatch(dispatchCtx, d)
		if execErr != nil {
			s.agentLifecycleLog.Warn("reconcile: dispatch op failed", append(initiatorLogArgs, "error", execErr)...)
			// The error text stays execErr.Error(): nodes that predate the
			// result envelope read only that column.
			if err := s.store.FailBrokerDispatch(ctx, d.ID, execErr.Error(), dispatchFailureResult(execErr)); err != nil {
				s.agentLifecycleLog.Error("reconcile: fail dispatch failed", "id", d.ID, "error", err)
			}
			if rec := s.dispatchMetrics; rec != nil {
				rec.IncFailed(ctx, 1, opAttr)
			}
			if s.events != nil {
				s.events.PublishDispatchDone(ctx, d.ID)
			}
			continue
		}
		if err := s.store.CompleteBrokerDispatch(ctx, d.ID, result); err != nil {
			s.agentLifecycleLog.Error("reconcile: complete dispatch failed", "id", d.ID, "error", err)
		}
		s.agentLifecycleLog.Info("reconcile: dispatch op completed", initiatorLogArgs...)
		if rec := s.dispatchMetrics; rec != nil {
			rec.IncDone(ctx, 1, opAttr)
			latencyMs := float64(time.Since(d.CreatedAt).Milliseconds())
			rec.RecordDispatchLatency(ctx, latencyMs, opAttr)
		}
		// Emit a slim completion event so originators waiting on
		// waitForDispatchDone wake up (design §6.3).
		if s.events != nil {
			s.events.PublishDispatchDone(ctx, d.ID)
		}
	}

}

// executeDispatch runs a claimed dispatch intent's op via the LOCAL broker
// tunnel and returns its result JSON. The lifecycle cases (start/stop/restart)
// deserialize args from the dispatch row and call the local dispatcher, which
// delivers over the in-memory control-channel socket. Unknown ops fail cleanly
// (and are retryable).
func (s *Server) executeDispatch(ctx context.Context, d store.BrokerDispatch) (result string, err error) {
	ctx, span := tracer.Start(ctx, "hub.dispatch.execute")
	defer func() {
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}()
	span.SetAttributes(
		attribute.String("scion.dispatch.id", d.ID),
		attribute.String("scion.dispatch.op", d.Op),
		attribute.String("scion.agent.id", d.AgentID),
	)

	switch d.Op {
	case "start":
		return s.execDispatchStart(ctx, d)
	case "stop":
		return s.execDispatchStop(ctx, d)
	case "restart":
		return s.execDispatchRestart(ctx, d)
	case "delete":
		return s.execDispatchDelete(ctx, d)
	case "check_prompt":
		return s.execDispatchCheckPrompt(ctx, d)
	case "finalize_env":
		return s.execDispatchFinalizeEnv(ctx, d)
	case "create":
		return s.execDispatchCreate(ctx, d)
	default:
		return "", fmt.Errorf("broker dispatch op %q not yet wired on this node", d.Op)
	}
}

func (s *Server) execDispatchStart(ctx context.Context, d store.BrokerDispatch) (string, error) {
	agent, err := s.resolveDispatchAgent(ctx, d)
	if err != nil {
		return "", err
	}
	defer s.beginLifecycleOp(agent.ID)()
	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		return "", fmt.Errorf("no dispatcher available")
	}
	var task string
	var resume bool
	if d.Args != "" {
		args, err := UnmarshalStartArgs(d.Args)
		if err != nil {
			return "", fmt.Errorf("unmarshal start args: %w", err)
		}
		task = args.Task
		resume = args.Resume
	}
	if err := dispatcher.DispatchAgentStart(ctx, agent, task, resume); err != nil {
		return "", fmt.Errorf("dispatch start: %w", err)
	}
	return "", nil
}

// stopSupersededResult is the result recorded on a queued stop row that was
// not applied because a newer start or stop superseded it.
const stopSupersededResult = `{"superseded":true}`

// claimQueuedStop takes the stop-kind claim a queued stop recorded at
// intentAt is applied under. superseded reports a stop that must not be
// applied: the intent changed, or a start holds the agent's claim. A claim
// the stop itself superseded (supersedes, recorded when it was queued) does
// not block it: the stop is applied without a claim and then releases that
// claim. release releases the stop claim.
func (s *Server) claimQueuedStop(ctx context.Context, agent *store.Agent, intentAt time.Time, supersedes string) (release func(), superseded bool) {
	noop := func() {}
	claim, err := s.store.ClaimAgentStop(ctx, agent.ID, s.instanceID, intentAt, s.startClaimSettings().LeaseTTL)
	var held *store.ClaimHeldError
	switch {
	case err == nil:
		return func() {
			if _, err := s.store.ReleaseAgentStart(context.WithoutCancel(ctx), agent.ID, claim.ID, s.instanceID); err != nil {
				s.agentLifecycleLog.Warn("reconcile: releasing the queued stop's claim failed; the reaper will settle it", "agent_id", agent.ID, "error", err)
			}
		}, false
	case errors.As(err, &held) && supersedes != "" && held.ClaimID == supersedes && agent.RunIntentMatches(store.RunIntentStopped, intentAt):
		return noop, false
	case errors.Is(err, store.ErrClaimPredicate), errors.As(err, &held):
		return noop, true
	default:
		// The claim could not be read or written: fall back to the intent
		// check alone.
		s.agentLifecycleLog.Warn("reconcile: queued stop claim failed; applying on the intent check", "agent_id", agent.ID, "error", err)
		return noop, !agent.RunIntentMatches(store.RunIntentStopped, intentAt)
	}
}

func (s *Server) execDispatchStop(ctx context.Context, d store.BrokerDispatch) (string, error) {
	agent, err := s.resolveDispatchAgent(ctx, d)
	if err != nil {
		return "", err
	}
	var intentAt *time.Time
	var supersedes string
	if d.Args != "" {
		args, err := UnmarshalStopArgs(d.Args)
		if err != nil {
			return "", fmt.Errorf("unmarshal stop args: %w", err)
		}
		intentAt = args.IntentAt
		supersedes = args.SupersedesClaim
		if args.RunID != "" {
			// Stop the run the intent was queued for, not whatever run the
			// row names now (ptone/scion#2550). agent is this call's own
			// copy, loaded by resolveDispatchAgent above.
			agent.RunID = args.RunID
		}
	}
	// A stop queued while the broker was offline applies only while the
	// stop intent it was queued for is still the current one; a start or
	// stop recorded since then supersedes it. The check and the stop
	// dispatch run under a stop-kind start claim pinned to that intent: a
	// start cannot claim the agent until the stop is applied, and a start
	// claimed first wrote a newer intent, so the claim is refused and the
	// stop is not applied.
	if intentAt != nil {
		release, superseded := s.claimQueuedStop(ctx, agent, *intentAt, supersedes)
		if superseded {
			s.agentLifecycleLog.Info("reconcile: queued stop superseded by a newer run intent or start; not applied",
				"id", d.ID, "agent_id", agent.ID, "run_intent", agent.RunIntent)
			return stopSupersededResult, nil
		}
		defer release()
	}
	defer s.beginLifecycleOp(agent.ID)()
	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		return "", fmt.Errorf("no dispatcher available")
	}
	if err := dispatcher.DispatchAgentStop(ctx, agent); err != nil {
		s.logStopRunMismatch(agent, "queued stop", err)
		return "", fmt.Errorf("dispatch stop: %w", err)
	}
	if intentAt != nil {
		// The queued stop has now been applied: release the per-broker
		// reservation as a direct stop does, and replace the stop_queued
		// container status and the queued-stop notice the offline stop set.
		// Both only while the row still holds the run the stop was queued
		// for (agent.RunID, the intent's run): a newer run keeps its state
		// and reservation (ptone/scion#2550).
		// The start claim held when the stop was recorded is superseded
		// (compare-and-set on the stop's intent time, whatever the run).
		s.releaseSupersededClaim(ctx, agent.ID, supersedes, *intentAt)
		if agent.ContainerStatus == containerStatusStopQueued {
			recorded, err := s.recordStopStatus(ctx, agent.ID, agent.RunID, "queued stop", store.AgentStatusUpdate{
				ContainerStatus: "stopped",
				ClearMessageIf:  offlineStopMessage,
			})
			if err != nil {
				s.agentLifecycleLog.Warn("reconcile: failed to update container status after queued stop",
					"id", d.ID, "agent_id", agent.ID, "error", err)
			}
			if recorded {
				s.releaseBrokerQuota(ctx, agent)
			}
		} else if s.stopRunStillCurrent(ctx, agent.ID, agent.RunID, "queued stop") {
			// A check, not a lock: a run minted between this read and the
			// release loses its reservation until the backfill restores it.
			s.releaseBrokerQuota(ctx, agent)
		}
	}
	return "", nil
}

func (s *Server) execDispatchRestart(ctx context.Context, d store.BrokerDispatch) (string, error) {
	agent, err := s.resolveDispatchAgent(ctx, d)
	if err != nil {
		return "", err
	}
	defer s.beginLifecycleOp(agent.ID)()
	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		return "", fmt.Errorf("no dispatcher available")
	}
	if err := dispatcher.DispatchAgentRestart(ctx, agent); err != nil {
		return "", fmt.Errorf("dispatch restart: %w", err)
	}
	return "", nil
}

func (s *Server) execDispatchDelete(ctx context.Context, d store.BrokerDispatch) (string, error) {
	agent, err := s.resolveDispatchAgent(ctx, d)
	if err != nil {
		return "", err
	}
	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		return "", fmt.Errorf("no dispatcher available")
	}
	var deleteFiles, removeBranch, softDelete bool
	var deletedAt time.Time
	var claim int64
	if d.Args != "" {
		args, err := UnmarshalDeleteArgs(d.Args)
		if err != nil {
			return "", fmt.Errorf("unmarshal delete args: %w", err)
		}
		deleteFiles = args.DeleteFiles
		removeBranch = args.RemoveBranch
		softDelete = args.SoftDelete
		deletedAt = args.DeletedAt
		claim = args.Claim
		if len(args.PreviousRunIDs) > 0 {
			agent.PreviousRunIDs = args.PreviousRunIDs
		}
	}
	// A delete engine's intent applies only while the claim it was created
	// under is still the row's current claim, live or failed in_doubt (see
	// deferredDeleteDeadline): the engine may have died, its lease lapsed
	// and the user started the agent again since (ptone/scion#2906). A
	// stale intent is dropped without dispatching; failing it (rather than
	// completing it) keeps a waiting engine from reading it as a teardown
	// that ran. The deadline sent to the broker is computed now, not when
	// the intent was written.
	//
	// An intent records no run ID of its own until ptone/scion#2550 P5; the
	// broker gets the re-read row's run ID. The intent's previous runs
	// (ptone/scion#3097) are deleted by the same DispatchAgentDelete call
	// under this fence, so each previous-run delete carries the same
	// notAfter and a stale intent deletes none of them.
	if claim != 0 {
		notAfter, ok := deferredDeleteDeadline(ctx, agent, claim, deleteClock())
		if !ok {
			s.agentLifecycleLog.Info("reconcile: deferred delete intent's claim is no longer current; dropped",
				"id", d.ID, "agent_id", agent.ID, "intent_claim", claim, "row_claim", agent.DeletionClaim,
				"deletion_state", agent.DeletionState, "deletion_code", agent.DeletionCode)
			return "", fmt.Errorf("%w (intent claim %d, row claim %d)", errStaleDeleteDispatch, claim, agent.DeletionClaim)
		}
		ctx = withDeleteDispatchFence(ctx, deleteDispatchFence{claim: claim, notAfter: notAfter})
	}
	if err := dispatcher.DispatchAgentDelete(ctx, agent, deleteFiles, removeBranch, softDelete, deletedAt); err != nil {
		if isStaleDeleteDispatch(err) && !errors.Is(err, errStaleDeleteDispatch) {
			// Keep the marker in the row's error text for the originating node.
			return "", fmt.Errorf("dispatch delete: %w: %w", errStaleDeleteDispatch, err)
		}
		return "", fmt.Errorf("dispatch delete: %w", err)
	}
	return "", nil
}

func (s *Server) execDispatchCheckPrompt(ctx context.Context, d store.BrokerDispatch) (string, error) {
	agent, err := s.resolveDispatchAgent(ctx, d)
	if err != nil {
		return "", err
	}
	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		return "", fmt.Errorf("no dispatcher available")
	}
	hasPrompt, err := dispatcher.DispatchCheckAgentPrompt(ctx, agent)
	if err != nil {
		return "", fmt.Errorf("dispatch check_prompt: %w", err)
	}
	result, err := json.Marshal(CheckPromptResult{HasPrompt: hasPrompt})
	if err != nil {
		return "", fmt.Errorf("marshal check_prompt result: %w", err)
	}
	return string(result), nil
}

func (s *Server) execDispatchFinalizeEnv(ctx context.Context, d store.BrokerDispatch) (string, error) {
	agent, err := s.resolveDispatchAgent(ctx, d)
	if err != nil {
		return "", err
	}
	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		return "", fmt.Errorf("no dispatcher available")
	}
	var env map[string]string
	if d.Args != "" {
		args, err := UnmarshalFinalizeEnvArgs(d.Args)
		if err != nil {
			return "", fmt.Errorf("unmarshal finalize_env args: %w", err)
		}
		env = args.Env
	}
	finalized, err := dispatcher.DispatchFinalizeEnv(ctx, agent, env)
	if err != nil {
		return "", fmt.Errorf("dispatch finalize_env: %w", err)
	}
	result, err := json.Marshal(FinalizeEnvResult{Success: true, Launch: finalized.AcceptedLaunch()})
	if err != nil {
		return "", fmt.Errorf("marshal finalize_env result: %w", err)
	}
	return string(result), nil
}

func (s *Server) execDispatchCreate(ctx context.Context, d store.BrokerDispatch) (string, error) {
	agent, err := s.resolveDispatchAgent(ctx, d)
	if err != nil {
		return "", err
	}
	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		return "", fmt.Errorf("no dispatcher available")
	}
	created, err := dispatcher.DispatchAgentCreateWithGather(ctx, agent)
	if err != nil {
		return "", fmt.Errorf("dispatch create: %w", err)
	}
	cr := CreateWithGatherResult{EnvRequirements: created.EnvRequirements(), Launch: created.AcceptedLaunch()}
	result, err := json.Marshal(cr)
	if err != nil {
		return "", fmt.Errorf("marshal create result: %w", err)
	}
	return string(result), nil
}

// resolveDispatchAgent loads the agent from the store by slug (used as the
// identifier in the dispatch row's AgentSlug field, matching the runtime
// broker's slug-based addressing).
func (s *Server) resolveDispatchAgent(ctx context.Context, d store.BrokerDispatch) (*store.Agent, error) {
	if d.AgentID != "" {
		agent, err := s.store.GetAgent(ctx, d.AgentID)
		if err != nil {
			return nil, fmt.Errorf("resolve agent %s: %w", d.AgentID, err)
		}
		return agent, nil
	}
	return nil, fmt.Errorf("dispatch row has no agent ID")
}

// deliverMessage tunnels a reconciled message to its agent over the LOCAL
// control channel — the same path DispatchAgentMessage uses for a locally-
// connected broker. reconcileBroker has already CAS-marked the message
// dispatched before calling this, so just deliver.
func (s *Server) deliverMessage(ctx context.Context, m *store.Message) error {
	if m == nil || m.AgentID == "" {
		return fmt.Errorf("message has no agent ID")
	}
	agent, err := s.store.GetAgent(ctx, m.AgentID)
	if err != nil {
		return fmt.Errorf("resolve agent %s: %w", m.AgentID, err)
	}
	if agent.RuntimeBrokerID == "" {
		return fmt.Errorf("agent %s has no runtime broker", m.AgentID)
	}
	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		return fmt.Errorf("no dispatcher available for message delivery")
	}
	return dispatcher.DispatchAgentMessage(ctx, agent, m.Msg, m.Urgent, nil)
}
