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
	"log/slog"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// intentStopInterval rate-limits the backstop stop per agent: at most one
// per heartbeat interval.
const intentStopInterval = observationFreshness

// stopAgentsRunningWithIntentStopped is the hub-side backstop for a start
// that reached the broker after a stop: an agent the heartbeat lists as
// running, whose run intent is stopped and which holds no start claim, is
// stopped with normal grace. It runs once per heartbeat, after the
// observations, and only for a complete inventory from a broker that was
// online a moment ago. Agents with a lifecycle operation or a queued
// dispatch in progress, being deleted, or mid-reincarnation are skipped.
// The stop runs under a short stop-kind claim pinned to the intent the
// heartbeat saw, so a start accepted meanwhile is never stopped. Only an
// intent written by code that maintains start claims is acted on (see
// store.Agent.RunIntentWrittenWithClaims); an older stopped intent is
// logged.
func (s *Server) stopAgentsRunningWithIntentStopped(ctx context.Context, brokerID string, prev *store.RuntimeBroker, hb *brokerHeartbeatRequest, report *heartbeatReport) {
	if !inventoryAllowsReconcile(prev, hb, s.missingAgents.now(), s.missingAgentGrace()) {
		return
	}
	if s.GetDispatcher() == nil {
		return
	}
	agents, err := report.brokerAgents(ctx, s, brokerID)
	if err != nil {
		return
	}
	var pending map[string]bool
	for i := range agents {
		a := &agents[i]
		seen, ok := report.observed[a.ID]
		if !ok || seen.state != store.ObservedPresentRunning {
			continue
		}
		if a.RunIntent != store.RunIntentStopped || a.RunIntentAt == nil || a.StartClaimID != "" ||
			!a.DeletedAt.IsZero() || a.DeletionActive(time.Now()) ||
			reincarnationInFlight(a) || s.lifecycleOps.active(a.ID) {
			continue
		}
		if pending == nil {
			if pending, err = s.pendingLifecycleAgents(ctx, brokerID); err != nil {
				return
			}
		}
		if pending[a.ID] {
			continue
		}
		if last, ok := s.intentStops.Load(a.ID); ok && time.Since(last.(time.Time)) < intentStopInterval {
			continue
		}
		s.intentStops.Store(a.ID, time.Now())
		if !a.RunIntentWrittenWithClaims() {
			// Intent last written by earlier code (or the boot backfill),
			// which could leave intent stopped on an agent meant to run
			// (a reincarnated stopped agent, a rolled-back delete, a
			// replica of the previous version): logged, never stopped.
			s.agentLifecycleLog.Info("Agent runs with a stopped run intent from earlier code; not stopped", "agent_id", a.ID)
			continue
		}
		go s.stopForStoppedIntent(context.WithoutCancel(ctx), *a)
	}
}

// stopForStoppedIntent stops a under a stop-kind claim pinned to the intent
// time the heartbeat saw.
func (s *Server) stopForStoppedIntent(ctx context.Context, a store.Agent) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cfg := s.startClaimSettings()
	claim, err := s.store.ClaimAgentStop(ctx, a.ID, s.instanceID, *a.RunIntentAt, cfg.LeaseTTL)
	if err != nil {
		return // a newer intent, or a claim taken meanwhile: nothing to do
	}
	defer func() {
		if _, err := s.store.ReleaseAgentStart(context.WithoutCancel(ctx), a.ID, claim.ID, s.instanceID); err != nil {
			slog.Warn("Backstop stop: releasing its claim failed; the reaper will settle it", "agent_id", a.ID, "error", err)
		}
	}()
	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		return
	}
	slog.Info("Stopping an agent that runs although its run intent is stopped", "agent_id", a.ID)
	if err := dispatcher.DispatchAgentStop(ctx, &a); err != nil {
		slog.Warn("Backstop stop failed", "agent_id", a.ID, "error", err)
	}
}
