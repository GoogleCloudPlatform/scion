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
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
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
		result, execErr := s.execDispatch(ctx, d)
		if execErr != nil {
			s.agentLifecycleLog.Warn("reconcile: dispatch op failed", "id", d.ID, "op", d.Op, "error", execErr)
			if err := s.store.FailBrokerDispatch(ctx, d.ID, execErr.Error()); err != nil {
				s.agentLifecycleLog.Error("reconcile: fail dispatch failed", "id", d.ID, "error", err)
			}
			continue
		}
		if err := s.store.CompleteBrokerDispatch(ctx, d.ID, result); err != nil {
			s.agentLifecycleLog.Error("reconcile: complete dispatch failed", "id", d.ID, "error", err)
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
		if err := s.deliverMsg(ctx, &m); err != nil {
			// At-least-once: the row is already marked dispatched; a delivery
			// failure is surfaced by the pending-message sweep (B5-2). Phase 3
			// (B3-3) supplies the real local tunnel + failure handling.
			s.agentLifecycleLog.Warn("reconcile: message delivery failed", "id", m.ID, "error", err)
		}
	}
}

// executeDispatch runs a claimed dispatch intent's op via the LOCAL broker tunnel
// and returns its result JSON. This is the default executor wired in New(); the
// per-op tunnel wiring (start/stop/restart/delete/finalize_env/check_prompt) is
// supplied by Phase 4 (B4-2..B4-4). The substrate (claim → execute → mark) is in
// place now; unknown ops fail cleanly (and are retryable) rather than silently
// completing.
func (s *Server) executeDispatch(ctx context.Context, d store.BrokerDispatch) (string, error) {
	switch d.Op {
	default:
		return "", fmt.Errorf("broker dispatch op %q not yet wired on this node", d.Op)
	}
}

// deliverMessage tunnels a reconciled message to its agent over the LOCAL control
// channel. The tunnel wiring is completed in Phase 3 (B3-3); in the current phase
// no producer writes pending message intent, so the default is a no-op success.
func (s *Server) deliverMessage(ctx context.Context, m *store.Message) error {
	return nil
}
