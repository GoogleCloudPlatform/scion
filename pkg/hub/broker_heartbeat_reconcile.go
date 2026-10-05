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
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Missing-container reconcile (GoogleCloudPlatform/scion#2077).
//
// A runtime broker's heartbeat lists the agents its runtimes report, that is,
// the containers (or Kubernetes pods) that exist. When a container disappears
// outside of a Scion lifecycle action (a node drain, repair or eviction
// removing a pod, or a container removed by hand), the agent would otherwise
// stay in phase running forever. The heartbeat reports, per runtime target
// (the broker's default runtime and each auxiliary runtime; a Kubernetes
// target is identified by cluster context and namespace), whether that
// target was listed completely, and each reported agent carries the target
// that listed it. The Hub records that target on the agent. After every
// heartbeat, the Hub looks for agents assigned to the broker that are in
// phase running, whose recorded target this heartbeat lists as complete, but
// which are absent from the report. Once such an agent has been continuously
// absent for the grace period it is moved to phase error with exit reason
// container_missing (an existing preempted or evicted exit reason is kept).
//
// The mechanism is runtime-neutral: it applies to Docker, Podman, Apple
// container and Kubernetes brokers alike, because it only compares the
// heartbeat's inventory with the Hub's agent rows.
//
// Safety rules (each one keeps a live agent from being caught):
//   - Only an agent whose recorded runtime target is listed as complete in
//     this heartbeat is considered. A target is complete only when its
//     listing succeeded, so a forbidden or failed listing (for example a
//     Kubernetes namespace the broker may not read) never leads to a
//     conclusion about its agents, while other targets are still reconciled.
//   - An agent with no recorded target, or whose target is absent from the
//     heartbeat, is never concluded. The recorded target (and any candidate)
//     is cleared, with a state_version bump, whenever a create, start or
//     restart is accepted, so an agent started anew is only considered after
//     two consecutive heartbeats have listed it on the same target again, and
//     a writer holding a read from before the clear cannot write the old
//     target back. An older broker sends no targets, and a filtered
//     (multi-hub) heartbeat claims none.
//   - The broker must be online and its previous heartbeat must be recent; a
//     broker returning from an offline or stale period restarts every clock.
//   - Only phase running is considered. created/provisioning/cloning/starting
//     are dispatch phases in which the container may legitimately not exist
//     yet; suspended/stopping/stopped/error are not running.
//   - Agents with a reincarnation, a queued lifecycle dispatch, or a lifecycle
//     dispatch in flight on this Hub are skipped.
//   - The agent must be absent from complete inventories for the whole grace
//     period, as observed by this Hub process, AND its last_seen (bumped by
//     every heartbeat that reports it and by the agent's own status reports)
//     must be older than the grace period. last_seen, not the row's updated
//     timestamp, is used because unrelated writes bump updated.
//   - The final write is a single conditional UPDATE whose WHERE clause
//     re-checks deleted_at, broker, phase, reincarnation state and last_seen.

// DefaultMissingAgentGrace is the default time an agent must be continuously
// absent from its broker's complete heartbeat inventory before the Hub marks
// it as having no container. At the default 30s broker heartbeat interval this
// is six consecutive heartbeats.
const DefaultMissingAgentGrace = 3 * time.Minute

// MinMissingAgentGrace is the lowest accepted grace period. Lower configured
// values are replaced by the default.
const MinMissingAgentGrace = time.Minute

// missingAgentMessage is the status message recorded on a reconciled agent.
const missingAgentMessage = "The runtime broker no longer reports a container for this agent; it was removed outside of Scion (for example by a node drain or eviction)."

// brokerInventory mirrors hubclient.BrokerInventory on the Hub side.
type brokerInventory struct {
	Targets []brokerInventoryTarget `json:"targets,omitempty"`
}

// brokerInventoryTarget mirrors hubclient.InventoryTarget.
type brokerInventoryTarget struct {
	ID       string `json:"id"`
	Runtime  string `json:"runtime,omitempty"`
	Complete bool   `json:"complete"`
}

// completeTargets returns the IDs of the targets the heartbeat listed
// completely. A target reported more than once counts only if every report
// is complete.
func (hb *brokerHeartbeatRequest) completeTargets() map[string]bool {
	if hb.Inventory == nil {
		return nil
	}
	out := make(map[string]bool, len(hb.Inventory.Targets))
	for _, t := range hb.Inventory.Targets {
		if t.ID == "" {
			continue
		}
		if prev, seen := out[t.ID]; seen {
			out[t.ID] = prev && t.Complete
		} else {
			out[t.ID] = t.Complete
		}
	}
	for id, ok := range out {
		if !ok {
			delete(out, id)
		}
	}
	return out
}

// agentRuntimeTarget returns the runtime target recorded on an agent, or "".
func agentRuntimeTarget(a *store.Agent) string {
	if a.AppliedConfig == nil {
		return ""
	}
	return a.AppliedConfig.RuntimeTarget
}

// nextRuntimeTarget applies one heartbeat's runtime target report to the
// recorded target and candidate in cfg (which may be nil) and returns the new
// pair and whether it differs.
//
// A reported target that differs from the recorded one is first kept as a
// candidate; it replaces the recorded target only when the next heartbeat
// reports the same target. A heartbeat the broker built before a start or
// restart was accepted, but that the Hub processes after the clear, can
// therefore only set a candidate, which the next heartbeat (built after the
// start) replaces or confirms. A report that matches the recorded target
// drops any candidate. An empty report changes nothing.
func nextRuntimeTarget(cfg *store.AgentAppliedConfig, reported string) (target, candidate string, changed bool) {
	if cfg != nil {
		target, candidate = cfg.RuntimeTarget, cfg.RuntimeTargetCandidate
	}
	if reported == "" {
		return target, candidate, false
	}
	switch reported {
	case target:
		if candidate == "" {
			return target, candidate, false
		}
		return target, "", true
	case candidate:
		return reported, "", true
	default:
		return target, reported, true
	}
}

// recordHeartbeatRuntimeTarget records the runtime target whose listing
// reported the agent; the missing-container reconcile only considers an agent
// whose recorded target a heartbeat lists as complete. A target is recorded
// only after two consecutive heartbeats report it (see nextRuntimeTarget).
//
// The write is the targeted SetAgentRuntimeTarget, not a full-row update: it
// changes only the two target keys, so it can never undo a concurrent status
// write (for example a start setting phase running), and it is conditional on
// the state_version this heartbeat read, so a report read before a clear
// (which bumps state_version) is dropped. Nothing is written when the pair is
// unchanged, so a steady heartbeat adds no store write.
func (s *Server) recordHeartbeatRuntimeTarget(ctx context.Context, a *store.Agent, reported string) {
	target, candidate, changed := nextRuntimeTarget(a.AppliedConfig, reported)
	if !changed {
		return
	}
	written, err := s.store.SetAgentRuntimeTarget(ctx, a.ID, a.StateVersion, target, candidate)
	if err != nil {
		slog.Warn("Failed to record the agent runtime target from heartbeat",
			"agent_id", a.ID, "runtimeTarget", reported, "error", err)
		return
	}
	if !written {
		slog.Debug("Agent changed since the heartbeat read it; runtime target report dropped",
			"agent_id", a.ID, "runtimeTarget", reported)
		return
	}
	if a.AppliedConfig == nil {
		a.AppliedConfig = &store.AgentAppliedConfig{}
	}
	a.AppliedConfig.RuntimeTarget, a.AppliedConfig.RuntimeTargetCandidate = target, candidate
}

// missingAgentTracker records, per broker, when this Hub process first saw
// each running agent absent from a complete heartbeat inventory. It is
// in-memory and per replica on purpose: losing it (restart) or not sharing it
// (several replicas) only delays a reconcile, never speeds one up, because
// the store's last_seen check still applies.
type missingAgentTracker struct {
	mu     sync.Mutex
	since  map[string]map[string]time.Time // brokerID -> agentID -> first seen missing
	nowFor func() time.Time                // test hook; nil means time.Now
}

func (t *missingAgentTracker) now() time.Time {
	if t.nowFor != nil {
		return t.nowFor()
	}
	return time.Now()
}

// reset forgets every clock for brokerID.
func (t *missingAgentTracker) reset(brokerID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.since, brokerID)
}

// observe replaces brokerID's clocks with the given currently-missing agent
// IDs, keeping the first-seen time of agents that were already missing, and
// returns the first-seen time for each.
func (t *missingAgentTracker) observe(brokerID string, missing []string, now time.Time) map[string]time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.since == nil {
		t.since = make(map[string]map[string]time.Time)
	}
	prev := t.since[brokerID]
	next := make(map[string]time.Time, len(missing))
	for _, id := range missing {
		if first, ok := prev[id]; ok {
			next[id] = first
		} else {
			next[id] = now
		}
	}
	if len(next) == 0 {
		delete(t.since, brokerID)
	} else {
		t.since[brokerID] = next
	}
	out := make(map[string]time.Time, len(next))
	for k, v := range next {
		out[k] = v
	}
	return out
}

// forget drops one agent's clock (after it was reconciled).
func (t *missingAgentTracker) forget(brokerID, agentID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if m := t.since[brokerID]; m != nil {
		delete(m, agentID)
		if len(m) == 0 {
			delete(t.since, brokerID)
		}
	}
}

// lifecycleOpTracker counts lifecycle dispatches (start, stop, restart,
// suspend, resume) in flight on this Hub process per agent. During such an
// operation the container can be legitimately absent while the agent row
// still says running (for example between the stop and the start of a
// restart), so the missing-container reconcile skips the agent, and a
// heartbeat does not apply a stopped/error phase over it
// (heartbeatPhaseGuarded).
//
// It is a best-effort, in-memory hint, per hub replica: a heartbeat handled
// by another replica, or by this one after a restart mid-dispatch, does not
// see the operation and is applied as usual. Nothing correctness-critical may
// depend on it.
type lifecycleOpTracker struct {
	mu       sync.Mutex
	inFlight map[string]int
}

// begin marks agentID as having a lifecycle operation in flight and returns
// the function that ends it. Safe to call with an empty ID (no-op).
func (t *lifecycleOpTracker) begin(agentID string) func() {
	if agentID == "" {
		return func() {}
	}
	t.mu.Lock()
	if t.inFlight == nil {
		t.inFlight = make(map[string]int)
	}
	t.inFlight[agentID]++
	t.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			if t.inFlight[agentID] <= 1 {
				delete(t.inFlight, agentID)
			} else {
				t.inFlight[agentID]--
			}
		})
	}
}

func (t *lifecycleOpTracker) active(agentID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.inFlight[agentID] > 0
}

// beginLifecycleOp marks a lifecycle dispatch for agentID as in flight; call
// the returned function when the dispatch and its status write are done.
func (s *Server) beginLifecycleOp(agentID string) func() {
	return s.lifecycleOps.begin(agentID)
}

// missingAgentGrace returns the configured grace period, applying the
// default and the floor.
func (s *Server) missingAgentGrace() time.Duration {
	g := s.config.MissingAgentGrace
	if g < MinMissingAgentGrace {
		return DefaultMissingAgentGrace
	}
	return g
}

// heartbeatReport is what reconcileMissingAgents needs from one processed
// heartbeat.
type heartbeatReport struct {
	// present holds the IDs of Hub agents matched by (project, slug).
	present map[string]bool
	// unresolvedSlugs holds the slugs of reported entries that could not be
	// matched to an agent of this broker (unknown project ID, legacy project
	// key, or another broker's agent). An agent of this broker with one of
	// these slugs is treated as present, which errs on the side of leaving
	// it alone.
	unresolvedSlugs map[string]bool
}

func newHeartbeatReport() *heartbeatReport {
	return &heartbeatReport{present: map[string]bool{}, unresolvedSlugs: map[string]bool{}}
}

// inventoryAllowsReconcile reports whether this heartbeat may be used to
// conclude that unreported agents have no container. prev is the broker row
// as it was before this heartbeat was stored (nil when it could not be read).
func inventoryAllowsReconcile(prev *store.RuntimeBroker, hb *brokerHeartbeatRequest, now time.Time, grace time.Duration) bool {
	if len(hb.completeTargets()) == 0 {
		return false
	}
	if hb.Status != store.BrokerStatusOnline {
		return false
	}
	if prev == nil || prev.Status != store.BrokerStatusOnline || prev.LastHeartbeat.IsZero() {
		return false
	}
	// A broker whose previous heartbeat is older than the grace period was
	// stale or offline; start counting again from this heartbeat.
	return now.Sub(prev.LastHeartbeat) < grace
}

// listRunningBrokerAgents returns every non-deleted agent assigned to
// brokerID in phase running, following pagination.
func (s *Server) listRunningBrokerAgents(ctx context.Context, brokerID string) ([]store.Agent, error) {
	var out []store.Agent
	opts := store.ListOptions{SkipTotalCount: true}
	for {
		res, err := s.store.ListAgents(ctx, store.AgentFilter{
			RuntimeBrokerID: brokerID,
			Phase:           string(state.PhaseRunning),
		}, opts)
		if err != nil {
			return nil, err
		}
		out = append(out, res.Items...)
		if res.NextCursor == "" {
			return out, nil
		}
		opts.Cursor = res.NextCursor
	}
}

// pendingLifecycleAgents returns the IDs of agents with a queued lifecycle
// dispatch for brokerID. Such an agent's container may be about to be
// created, stopped or replaced, so it is not reconciled.
func (s *Server) pendingLifecycleAgents(ctx context.Context, brokerID string) (map[string]bool, error) {
	rows, err := s.store.ListPendingDispatch(ctx, brokerID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(rows))
	for _, d := range rows {
		if d.AgentID == "" {
			continue
		}
		switch d.Op {
		case "create", "start", "stop", "restart", "delete":
			out[d.AgentID] = true
		}
	}
	return out, nil
}

// reconcileMissingAgents runs after a heartbeat's reported agents have been
// processed. See the file comment for the rules.
func (s *Server) reconcileMissingAgents(ctx context.Context, brokerID string, prev *store.RuntimeBroker, hb *brokerHeartbeatRequest, report *heartbeatReport) {
	grace := s.missingAgentGrace()
	now := s.missingAgents.now()

	if !inventoryAllowsReconcile(prev, hb, now, grace) {
		s.missingAgents.reset(brokerID)
		return
	}

	complete := hb.completeTargets()
	agents, err := s.listRunningBrokerAgents(ctx, brokerID)
	if err != nil {
		s.agentLifecycleLog.Warn("heartbeat reconcile: failed to list broker agents",
			"broker_id", brokerID, "error", err)
		return
	}

	var missing []store.Agent
	for i := range agents {
		a := &agents[i]
		if report.present[a.ID] || report.unresolvedSlugs[a.Slug] {
			continue
		}
		if reincarnationInFlight(a) || s.lifecycleOps.active(a.ID) {
			continue
		}
		if target := agentRuntimeTarget(a); target == "" || !complete[target] {
			continue
		}
		missing = append(missing, *a)
	}
	if len(missing) == 0 {
		s.missingAgents.reset(brokerID)
		return
	}

	pending, err := s.pendingLifecycleAgents(ctx, brokerID)
	if err != nil {
		s.agentLifecycleLog.Warn("heartbeat reconcile: failed to list pending dispatches",
			"broker_id", brokerID, "error", err)
		return
	}
	ids := make([]string, 0, len(missing))
	kept := missing[:0]
	for _, a := range missing {
		if pending[a.ID] {
			continue
		}
		ids = append(ids, a.ID)
		kept = append(kept, a)
	}
	missing = kept

	firstSeen := s.missingAgents.observe(brokerID, ids, now)
	cutoff := now.Add(-grace)
	for _, a := range missing {
		if first, ok := firstSeen[a.ID]; !ok || first.After(cutoff) {
			continue
		}
		if !a.LastSeen.IsZero() && !a.LastSeen.Before(cutoff) {
			continue
		}
		updated, err := s.store.MarkAgentContainerMissing(ctx, a.ID, brokerID, cutoff, missingAgentMessage)
		if err != nil {
			s.agentLifecycleLog.Warn("heartbeat reconcile: failed to mark agent container missing",
				"broker_id", brokerID, "agent_id", a.ID, "error", err)
			continue
		}
		if updated == nil {
			// A concurrent write (start, heartbeat, stop) changed the row.
			continue
		}
		s.missingAgents.forget(brokerID, a.ID)
		s.agentLifecycleLog.Warn("heartbeat reconcile: agent container missing, marked error",
			"broker_id", brokerID, "agent_id", a.ID, "agent", a.Slug, "project_id", a.ProjectID,
			"previous_activity", a.Activity, "missing_since", firstSeen[a.ID], "last_seen", a.LastSeen,
			"exit_reason", updated.ExitReason)
		s.reconcileBrokerQuotaOnPhaseChange(ctx, updated, string(state.PhaseRunning), updated.Phase)
		s.events.PublishAgentStatus(ctx, updated)
	}
}
