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
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Stranded-run adoption from broker heartbeats (ptone/scion#3661, design
// §3.12).
//
// A broker heartbeat reports, for each runtime entry it lists, the entry's
// scion.run_id label. Two overlapping dispatches that were not acted on, or
// a dispatch whose outcome is unknown, can leave an agent's run_id naming a
// run R that no runtime entry holds, while the agent's assigned broker B
// still reports the entry of an earlier run L. Such a "stranded" row is
// healed here. The run is swapped R -> L (store.AdoptAgentRunID, a
// compare-and-swap against R) and logged as agent_run_adopted_from_heartbeat
// only when all of these hold:
//
//	(a) B's heartbeat is a complete report: every runtime target it lists
//	    was listed completely, and it reports every entry's label,
//	    including entries of one agent on several targets
//	    (brokerInventory.RunLabels);
//	(b) none of A's entries in it is labelled R, every one is labelled,
//	    and they carry exactly one run, L;
//	(c) no broker other than B reported an entry of A labelled R within
//	    the quiet period Q (broker_reported_runs, written by every hub
//	    replica, so the answer does not depend on which replica handled
//	    which heartbeat);
//	(d) run_id has been unchanged for at least Q: run_id_updated_at is set
//	    and no later than now minus Q, checked on the read and again in the
//	    swap's own WHERE clause, so a change and change back after the read
//	    is not adopted (a row whose run_id has not changed since the column
//	    was added is never adopted);
//	(e) L is a run the hub issued a token for to A (an AgentCredential row
//	    for A with run_id L); a missing row means no adoption.
//
// A has no lifecycle dispatch queued or in flight on this hub, and no
// delete or reincarnation holds it. A concurrent run-ID write between the
// read and the swap, including one that changes run_id and back to R, makes
// the swap match nothing (run_id, or run_id_updated_at, no longer
// matches), and nothing is written.
//
// An entry with an empty label (created before run IDs, or an older
// broker) is logged as agent_run_label_empty and never acted on. A label
// with no credential row for A is logged as agent_run_label_unrecognised
// and never adopted. Both logs are rate-limited per agent and label.
//
// This is the fourth writer of agents.run_id, next to the dispatcher, the
// broker launch report and the revert worker; it only ever moves run_id to
// a run the hub itself issued for the agent.
//
// Entries of A on a broker other than B are never adopted; an entry is
// recorded in broker_reported_runs only when its label is A's current
// run_id, and at most maxOffBrokerEntriesPerAgent of them are handled per
// heartbeat. R on an offline
// broker other than B is a superseded container, stopped when that broker
// reconnects (§3.12); it cannot block adoption once its row is older than
// Q.

// runAdoptionQuietPeriod returns Q, the time run_id must have been
// unchanged before a heartbeat may adopt another run: twice the dispatch
// timeout (syncDispatchTimeout, the hub-to-broker request limit), so a
// launch still in flight, or one whose outcome is about to be reconciled,
// is never overtaken. Off-broker reports older than Q are pruned.
func runAdoptionQuietPeriod() time.Duration {
	return 2 * syncDispatchTimeout
}

// runAdoptionLogWindow is how long an empty or unrecognised label of an
// agent is logged only once.
const runAdoptionLogWindow = time.Hour

// maxOffBrokerEntriesPerAgent caps the entries of one agent assigned to
// another broker that one heartbeat handles. A broker normally lists one
// entry per agent per runtime target (its default runtime plus a few
// auxiliary ones), so more than this means a broker reporting abnormal
// duplicates; the cap bounds the work one heartbeat can cause.
const maxOffBrokerEntriesPerAgent = 8

// heartbeatRunEntries collects, for one heartbeat, the labels of the
// entries reported for each agent: the reporting broker's own agents, and,
// for agents assigned to another broker, whether an entry carries the
// agent's current run.
type heartbeatRunEntries struct {
	own      map[string]*ownRunEntries
	ownOrder []string
	offB     []store.BrokerReportedRun
	// offCount counts the handled entries per off-broker agent, offOver
	// holds the agents whose entries went over the cap, and offHeld the
	// agents whose current run is already in offB.
	offCount map[string]int
	offOver  map[string]bool
	offHeld  map[string]bool
}

// ownRunEntries are the labels of one own agent's entries.
type ownRunEntries struct {
	agent  *store.Agent
	labels []string
}

func newHeartbeatRunEntries() *heartbeatRunEntries {
	return &heartbeatRunEntries{
		own:      map[string]*ownRunEntries{},
		offCount: map[string]int{},
		offOver:  map[string]bool{},
		offHeld:  map[string]bool{},
	}
}

// addOwn records an entry of an agent assigned to the reporting broker,
// with the labels of its entries on other targets.
func (e *heartbeatRunEntries) addOwn(a *store.Agent, hb brokerAgentHeartbeat) {
	o := e.own[a.ID]
	if o == nil {
		o = &ownRunEntries{agent: a}
		e.own[a.ID] = o
		e.ownOrder = append(e.ownOrder, a.ID)
	}
	o.labels = append(o.labels, hb.RunID)
	for _, more := range hb.OtherEntries {
		o.labels = append(o.labels, more.RunID)
	}
}

// addOffBroker handles the entries of an agent assigned to another
// broker. Only an entry carrying the agent's current run_id is recorded
// (once per agent per heartbeat): that is the only run condition (c)
// reads, and any later run_id change restarts the quiet period anyway.
// Entries over maxOffBrokerEntriesPerAgent are skipped; the agent's
// current run is then recorded as reported, so a skipped entry can only
// delay an adoption, never allow one.
func (e *heartbeatRunEntries) addOffBroker(a *store.Agent, hb brokerAgentHeartbeat) {
	runs := make([]string, 0, 1+len(hb.OtherEntries))
	runs = append(runs, hb.RunID)
	for _, more := range hb.OtherEntries {
		runs = append(runs, more.RunID)
	}
	for _, run := range runs {
		if e.offCount[a.ID] >= maxOffBrokerEntriesPerAgent {
			e.offOver[a.ID] = true
			e.holdCurrent(a)
			continue
		}
		e.offCount[a.ID]++
		if run != "" && run == a.RunID {
			e.holdCurrent(a)
		}
	}
}

// holdCurrent records the agent's current run as reported off its broker.
func (e *heartbeatRunEntries) holdCurrent(a *store.Agent) {
	if a.RunID == "" || e.offHeld[a.ID] {
		return
	}
	e.offHeld[a.ID] = true
	e.offB = append(e.offB, store.BrokerReportedRun{AgentID: a.ID, RunID: a.RunID})
}

// runAdoptionState is the Server's adoption state: a clock hook and the
// log rate limit. It holds nothing an adoption decision depends on.
type runAdoptionState struct {
	nowFor   func() time.Time // test hook; nil means time.Now
	logsOnce sync.Once
	logs     *runScopeLogDedup
}

func (r *runAdoptionState) now() time.Time {
	if r.nowFor != nil {
		return r.nowFor()
	}
	return time.Now()
}

// firstLog reports whether key was not logged within runAdoptionLogWindow.
func (r *runAdoptionState) firstLog(key string, now time.Time) bool {
	r.logsOnce.Do(func() { r.logs = newRunScopeLogDedup(runScopeLogDedupSize, runAdoptionLogWindow) })
	return r.logs.first(key, now)
}

// heartbeatReportsAllRuns is condition (a): every listed target was
// listed completely, and the broker reports every entry's label.
func heartbeatReportsAllRuns(hb *brokerHeartbeatRequest) bool {
	if hb.Inventory == nil || !hb.Inventory.RunLabels || len(hb.Inventory.Targets) == 0 {
		return false
	}
	for _, t := range hb.Inventory.Targets {
		if t.ID == "" || !t.Complete {
			return false
		}
	}
	return true
}

// adoptStrandedRuns runs after a heartbeat's reported agents have been
// processed. See the file comment for the rules.
func (s *Server) adoptStrandedRuns(ctx context.Context, brokerID string, hb *brokerHeartbeatRequest, entries *heartbeatRunEntries, report *heartbeatReport) {
	now := s.runAdoption.now()
	quiet := runAdoptionQuietPeriod()
	if err := s.store.RecordBrokerReportedRuns(ctx, brokerID, entries.offB, now, now.Add(-quiet)); err != nil {
		s.agentLifecycleLog.Warn("heartbeat: run adoption: failed to record runs reported off the assigned broker",
			"broker_id", brokerID, "error", err)
	}
	if len(entries.offOver) > 0 {
		s.agentLifecycleLog.Warn("heartbeat: run adoption: entries over the per-agent cap skipped",
			"broker_id", brokerID, "agents", len(entries.offOver), "cap", maxOffBrokerEntriesPerAgent)
	}
	complete := heartbeatReportsAllRuns(hb)
	for _, id := range entries.ownOrder {
		s.classifyReportedRuns(ctx, brokerID, entries.own[id], complete, report, now, quiet)
	}
}

// classifyReportedRuns applies the adoption rules to the entries of one
// agent of the reporting broker.
func (s *Server) classifyReportedRuns(ctx context.Context, brokerID string, o *ownRunEntries, complete bool, report *heartbeatReport, now time.Time, quiet time.Duration) {
	a := o.agent
	current := a.RunID
	log := s.agentLifecycleLog

	// (b): the entries' runs.
	var candidate string
	ambiguous, unlabelled := false, false
	for _, l := range o.labels {
		switch {
		case l == current:
			return // current: an entry holds R
		case l == "":
			unlabelled = true
		case candidate == "":
			candidate = l
		case l != candidate:
			ambiguous = true
		}
	}
	if unlabelled && s.runAdoption.firstLog("empty|"+a.ID, now) {
		log.Debug("agent_run_label_empty",
			"agent_id", a.ID, "broker_id", brokerID, "run_id", current)
	}
	if candidate == "" {
		return
	}

	// (e): the hub issued a token for the run.
	minted, err := s.store.HasAgentCredentialForRun(ctx, a.ID, candidate)
	if err != nil {
		log.Warn("heartbeat: run adoption: credential lookup failed",
			"agent_id", a.ID, "broker_id", brokerID, "error", err)
		return
	}
	if !minted {
		if s.runAdoption.firstLog("unrecognised|"+a.ID+"|"+candidate, now) {
			log.Warn("agent_run_label_unrecognised",
				"agent_id", a.ID, "broker_id", brokerID, "run_id", current, "reported_run_id", candidate)
		}
		return
	}
	if !complete || ambiguous || unlabelled {
		return
	}

	// (d): run_id unchanged for Q. Checked before the store reads below.
	if a.RunIDUpdatedAt == nil || now.Sub(*a.RunIDUpdatedAt) < quiet {
		return
	}
	if deletionActive(a) || reincarnationInFlight(a) || s.lifecycleOps.active(a.ID) {
		return
	}
	starts, err := report.pendingStarts(ctx, s, brokerID)
	if err != nil {
		log.Warn("heartbeat: run adoption: pending dispatch lookup failed",
			"agent_id", a.ID, "broker_id", brokerID, "error", err)
		return
	}
	if starts[a.ID] {
		return
	}
	// (c): no other broker reported R within Q.
	if current != "" {
		held, err := s.store.AgentRunReportedSince(ctx, a.ID, current, now.Add(-quiet))
		if err != nil {
			log.Warn("heartbeat: run adoption: reported-run lookup failed",
				"agent_id", a.ID, "broker_id", brokerID, "error", err)
			return
		}
		if held {
			return
		}
	}

	swapped, err := s.store.AdoptAgentRunID(ctx, a.ID, current, candidate, now.Add(-quiet))
	if err != nil {
		log.Warn("heartbeat: run adoption: swap failed",
			"agent_id", a.ID, "broker_id", brokerID, "error", err)
		return
	}
	if !swapped {
		log.Debug("heartbeat: run adoption: run_id changed since read; not adopted",
			"agent_id", a.ID, "broker_id", brokerID, "run_id", current, "reported_run_id", candidate)
		return
	}
	log.Info("agent_run_adopted_from_heartbeat",
		"agent_id", a.ID, "broker_id", brokerID, "previous_run_id", current, "run_id", candidate)
}
