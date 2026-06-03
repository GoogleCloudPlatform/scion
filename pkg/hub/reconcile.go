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
	"fmt"
	"log/slog"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"go.opentelemetry.io/otel/attribute"
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
		result, execErr := s.execDispatch(ctx, d)
		if execErr != nil {
			s.agentLifecycleLog.Warn("reconcile: dispatch op failed", "id", d.ID, "op", d.Op, "error", execErr)
			if err := s.store.FailBrokerDispatch(ctx, d.ID, execErr.Error()); err != nil {
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

	// 2. Pending messages destined for agents on this broker.
	msgs, err := s.store.ListPendingMessages(ctx, brokerID)
	if err != nil {
		s.agentLifecycleLog.Error("reconcile: list pending messages failed", "brokerID", brokerID, "error", err)
		return
	}
	for i := range msgs {
		m := msgs[i]
		dispatched, err := s.store.MarkMessageDispatched(ctx, m.ID)
		if err != nil {
			s.agentLifecycleLog.Error("reconcile: mark message dispatched failed", "id", m.ID, "error", err)
			continue
		}
		if !dispatched {
			continue // another drain already took it (dedupe)
		}
		if rec := s.dispatchMetrics; rec != nil {
			rec.IncMessageDispatched(ctx, 1)
		}
		if err := s.deliverMsg(ctx, &m); err != nil {
			// At-least-once: the row is already marked dispatched; a delivery
			// failure is surfaced by the pending-message sweep (B5-2). Phase 3
			// (B3-3) supplies the real local tunnel + failure handling.
			s.agentLifecycleLog.Warn("reconcile: message delivery failed", "id", m.ID, "error", err)
		}
	}
}

// executeDispatch runs a claimed dispatch intent's op via the LOCAL broker
// tunnel and returns its result JSON. The lifecycle cases (start/stop/restart)
// deserialize args from the dispatch row and call the local dispatcher, which
// delivers over the in-memory control-channel socket. Unknown ops fail cleanly
// (and are retryable).
func (s *Server) executeDispatch(ctx context.Context, d store.BrokerDispatch) (string, error) {
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
	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		return "", fmt.Errorf("no dispatcher available")
	}
	var task string
	if d.Args != "" {
		args, err := UnmarshalStartArgs(d.Args)
		if err != nil {
			return "", fmt.Errorf("unmarshal start args: %w", err)
		}
		task = args.Task
	}
	if err := dispatcher.DispatchAgentStart(ctx, agent, task); err != nil {
		return "", fmt.Errorf("dispatch start: %w", err)
	}
	return "", nil
}

func (s *Server) execDispatchStop(ctx context.Context, d store.BrokerDispatch) (string, error) {
	agent, err := s.resolveDispatchAgent(ctx, d)
	if err != nil {
		return "", err
	}
	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		return "", fmt.Errorf("no dispatcher available")
	}
	if err := dispatcher.DispatchAgentStop(ctx, agent); err != nil {
		return "", fmt.Errorf("dispatch stop: %w", err)
	}
	return "", nil
}

func (s *Server) execDispatchRestart(ctx context.Context, d store.BrokerDispatch) (string, error) {
	agent, err := s.resolveDispatchAgent(ctx, d)
	if err != nil {
		return "", err
	}
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
	if d.Args != "" {
		args, err := UnmarshalDeleteArgs(d.Args)
		if err != nil {
			return "", fmt.Errorf("unmarshal delete args: %w", err)
		}
		deleteFiles = args.DeleteFiles
		removeBranch = args.RemoveBranch
		softDelete = args.SoftDelete
		deletedAt = args.DeletedAt
	}
	if err := dispatcher.DispatchAgentDelete(ctx, agent, deleteFiles, removeBranch, softDelete, deletedAt); err != nil {
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
	if err := dispatcher.DispatchFinalizeEnv(ctx, agent, env); err != nil {
		return "", fmt.Errorf("dispatch finalize_env: %w", err)
	}
	result, err := json.Marshal(FinalizeEnvResult{Success: true})
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
	envReqs, err := dispatcher.DispatchAgentCreateWithGather(ctx, agent)
	if err != nil {
		return "", fmt.Errorf("dispatch create: %w", err)
	}
	cr := CreateWithGatherResult{EnvRequirements: envReqs}
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

// signalDeferredMessage emits a best-effort NOTIFY wakeup so the broker's
// owning node drains the pending message. Called when route() returns
// routeForward or routeUndeliverable (the message row is already durable).
func (s *Server) signalDeferredMessage(ctx context.Context, brokerID, agentID string) {
	if s.commandBus != nil {
		if err := s.commandBus.SignalBrokerCmd(ctx, brokerID); err != nil {
			slog.Warn("failed to signal deferred message", "brokerID", brokerID, "agentID", agentID, "error", err)
		}
	}
}
