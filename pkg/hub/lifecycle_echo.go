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
	"unicode"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetryemit"
)

// The broker's effective-configuration echo (WP1 slice 3 addendum v12,
// design/wp1-slice3-addendum-v12.md): the hub records what the broker
// resolved for a run with the single store writer SetAgentEffectiveConfig,
// fenced on the run, the broker and the row's liveness, from the
// synchronous dispatch answer (onSyncDispatchAnswer) and from an async
// launch's succeeded report (recordLaunchReportEcho).

// maxEchoStringBytes bounds every broker-echoed string the hub records.
const maxEchoStringBytes = 256

// lifecycleEchoTimeout bounds the detached effective-config write.
const lifecycleEchoTimeout = 10 * time.Second

// effectiveConfigWriter is the store's single effective_config writer
// (entadapter.AgentStore.SetAgentEffectiveConfig); not part of store.Store.
type effectiveConfigWriter interface {
	SetAgentEffectiveConfig(ctx context.Context, agentID, runID string, eff store.AgentEffectiveConfig) (store.EffectiveConfigResult, error)
}

// validEchoString reports whether a broker-echoed string may be recorded:
// at most maxEchoStringBytes of valid UTF-8, printable characters only.
// An invalid value is omitted (unknown), never truncated into a value that
// looks real.
func validEchoString(s string) bool {
	if len(s) > maxEchoStringBytes || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// classifyStaleEcho refines the store's stale reason of a launch-report
// echo (addendum v12 section 3, review r3 R1): run_mismatch becomes
// unsettled_adoption exactly when the row's current launch is this
// report's launch, it bound its dispatched run, and the row still holds
// that run. Every other reason is returned unchanged.
func classifyStaleEcho(res store.EffectiveConfigResult, reportLaunchID string) string {
	if res.StaleReason == store.StaleEchoRunMismatch &&
		res.RowLaunchRunID != "" &&
		res.RowLaunchID == reportLaunchID &&
		res.RowRunID == res.RowLaunchRunID {
		return store.StaleEchoUnsettledAdoption
	}
	return res.StaleReason
}

// recordEffectiveEcho writes eff for runID with the store's single writer,
// on a context detached from the request (a client that leaves after the
// broker answered must not lose the evidence), and counts a stale echo by
// path and reason. A store error is logged and the caller continues: a
// lost echo loses evidence, not the launch. reportLaunchID is the launch
// of a launch-report echo ("" on the sync path), used only to refine
// run_mismatch.
func recordEffectiveEcho(ctx context.Context, w effectiveConfigWriter, counters telemetryemit.LifecycleCounters, log *slog.Logger,
	path, agentID, runID string, eff store.AgentEffectiveConfig, reportLaunchID string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lifecycleEchoTimeout)
	defer cancel()
	res, err := w.SetAgentEffectiveConfig(ctx, agentID, runID, eff)
	if err != nil {
		log.Warn("lifecycle: broker effective configuration not recorded",
			"agent_id", agentID, "run_id", runID, "path", path, "error", err)
		return
	}
	if res.Written {
		return
	}
	reason := res.StaleReason
	if path == telemetryemit.StaleEchoPathLaunchReport {
		reason = classifyStaleEcho(res, reportLaunchID)
	}
	counters.StaleEcho(path, reason)
	log.Debug("lifecycle: stale broker effective configuration not recorded",
		"agent_id", agentID, "run_id", runID, "path", path, "reason", reason,
		"row_run_id", res.RowRunID, "row_launch_id", res.RowLaunchID)
	if reason == store.StaleEchoUnsettledAdoption {
		// Server log only, never the analytics log; metadata, no echo
		// values (addendum v12 section 6).
		log.Info("lifecycle_echo_unsettled_adoption",
			"agent_id", agentID, "launch_id", reportLaunchID, "reported_run_id", runID, "row_run_id", res.RowRunID)
	}
}

// syncDispatchEcho records the effective configuration of a synchronous
// dispatch answer (addendum v12 section 2). agent is the dispatched copy
// after adoptBrokerRunID: agent.RunID is the run the broker confirmed or
// the hub adopted, and agent.RuntimeBrokerID the broker this attempt went
// to (never a broker the answer names). Nothing is written or counted for
// an answer without effective-only fields after validation (an older
// broker) or naming no run (nothing to fence on). An answer naming another
// run (adoption refused, or the CAS failed) is a stale echo
// (run_mismatch) without a store call.
func syncDispatchEcho(ctx context.Context, st store.Store, counters telemetryemit.LifecycleCounters, log *slog.Logger,
	agent *store.Agent, resp *RemoteAgentResponse) {
	w, ok := st.(effectiveConfigWriter)
	if !ok || agent == nil || resp == nil || resp.Agent == nil {
		return
	}
	eff, ok := brokerEffectiveConfigOf(resp.Agent, agent.RuntimeBrokerID, log)
	if !ok || eff.RunID == "" {
		return
	}
	if eff.RunID != agent.RunID {
		counters.StaleEcho(telemetryemit.StaleEchoPathSync, store.StaleEchoRunMismatch)
		log.Debug("lifecycle: stale broker effective configuration not recorded",
			"agent_id", agent.ID, "run_id", agent.RunID, "broker_run_id", eff.RunID,
			"path", telemetryemit.StaleEchoPathSync, "reason", store.StaleEchoRunMismatch)
		return
	}
	eff.Source = store.EffectiveConfigSourceSync
	recordEffectiveEcho(ctx, w, counters, log, telemetryemit.StaleEchoPathSync, agent.ID, agent.RunID, eff, "")
}

// recordLaunchReportEcho records the effective configuration of an async
// launch's succeeded report (addendum v12 section 3), fenced on the run
// the broker reports and on brokerID, the HMAC-authenticated reporting
// broker. It runs after settleLaunchedRun for every succeeded report the
// store answered without an HTTP error (applied or completed), so a
// resent report recovers an echo a crash or failure lost; the writer is
// idempotent. The same gates as the sync path apply.
func (s *Server) recordLaunchReportEcho(ctx context.Context, agentID, brokerID, launchID string, info *RemoteAgentInfo) {
	w, ok := s.store.(effectiveConfigWriter)
	if !ok || info == nil {
		return
	}
	eff, ok := brokerEffectiveConfigOf(info, brokerID, s.agentLifecycleLog)
	if !ok || eff.RunID == "" {
		return
	}
	eff.Source = store.EffectiveConfigSourceLaunchReport
	if validEchoString(launchID) {
		eff.LaunchID = launchID
	}
	recordEffectiveEcho(ctx, w, s.lifecycleCounters(), s.agentLifecycleLog, telemetryemit.StaleEchoPathLaunchReport,
		agentID, eff.RunID, eff, launchID)
}

// lifecycleCounters returns the lifecycle counters the hub reports to
// (NopCounters until SetLifecycleEventsSink set them).
func (s *Server) lifecycleCounters() telemetryemit.LifecycleCounters {
	rt := &s.lifecycleEvents
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.countersOrNop()
}
