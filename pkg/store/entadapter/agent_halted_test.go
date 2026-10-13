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

// Halted phase store tests (ptone/scion#4853). Like the rest of the
// entadapter suite they run against SQLite, and against Postgres in the
// Postgres CI job (make test-store-postgres-entadapter).

package entadapter

import (
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// haltRow is the white-box state of an agent row for a halt test.
type haltRow struct {
	phase      string
	activity   string
	runtime    string
	runID      string
	intent     store.RunIntent
	claimID    string
	exitCode   *int
	exitReason string
}

// defaultHaltRow is a running kubernetes agent meant to be running, with no
// start claim: every halt condition but the evidence holds.
func defaultHaltRow() haltRow {
	return haltRow{
		phase:    "running",
		activity: "working",
		runtime:  "kubernetes",
		runID:    "run-1",
		intent:   store.RunIntentRunning,
	}
}

func intp(v int) *int { return &v }

// newHaltAgent creates an agent and writes r onto its row directly.
func newHaltAgent(t *testing.T, ctx context.Context, s *AgentStore, projectID, slug string, r haltRow) string {
	t.Helper()
	a := makeAgent(projectID, slug)
	a.RuntimeBrokerID = "broker-1"
	require.NoError(t, s.CreateAgent(ctx, a))
	uid, err := parseUUID(a.ID)
	require.NoError(t, err)
	u := s.client.Agent.UpdateOneID(uid).
		SetPhase(r.phase).
		SetActivity(r.activity).
		SetRuntime(r.runtime).
		SetExitReason(r.exitReason).
		SetLastSeen(time.Now().Add(-time.Hour))
	if r.runID != "" {
		u.SetRunID(r.runID).SetRunIDUpdatedAt(time.Now().Add(-time.Hour))
	}
	if r.intent != "" {
		u.SetRunIntent(string(r.intent)).SetRunIntentAt(time.Now().Add(-time.Hour))
	}
	if r.claimID != "" {
		u.SetStartClaimID(r.claimID)
	}
	if r.exitCode != nil {
		u.SetExitCode(*r.exitCode)
	} else {
		u.ClearExitCode()
	}
	_, err = u.Save(ctx)
	require.NoError(t, err)
	return a.ID
}

// heartbeat returns a broker heartbeat status update for the row's run.
func heartbeat(phase, reason string, code *int) store.AgentStatusUpdate {
	return store.AgentStatusUpdate{Phase: phase, ExitReason: reason, ExitCode: code, Heartbeat: true,
		ReportSource: store.StatusReportSourceRuntime, ReportRunID: "run-1"}
}

// selfReport returns an agent self-report status update for the row's run.
func selfReport(phase, activity string) store.AgentStatusUpdate {
	return store.AgentStatusUpdate{Phase: phase, Activity: activity, GuardReincarnation: true,
		ReportSource: store.StatusReportSourceAgent, ReportRunID: "run-1"}
}

// apply runs UpdateAgentStatus with a fresh Result and returns it.
func apply(t *testing.T, ctx context.Context, s *AgentStore, id string, su store.AgentStatusUpdate) store.AgentStatusResult {
	t.Helper()
	var res store.AgentStatusResult
	su.Result = &res
	require.NoError(t, s.UpdateAgentStatus(ctx, id, su))
	return res
}

func TestAgentHalted_HeartbeatPreemptedHalts(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	id := newHaltAgent(t, ctx, s, projectID, "preempt-report", defaultHaltRow())

	res := apply(t, ctx, s, id, heartbeat("stopped", "preempted", intp(143)))
	assert.Equal(t, store.AgentStatusResult{Halted: true, HaltedFrom: "stopped"}, res)
	got, err := s.GetAgent(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "halted", got.Phase)
	assert.Equal(t, "", got.Activity, "a halt clears the activity")
	assert.Equal(t, "preempted", got.ExitReason)

	// Later reports of the same run move nothing and halt nothing again.
	for _, su := range []store.AgentStatusUpdate{
		heartbeat("stopping", "", nil),
		heartbeat("stopped", "preempted", intp(143)),
		selfReport("stopped", ""),
	} {
		res := apply(t, ctx, s, id, su)
		assert.False(t, res.Halted, "a halt happens once")
		got, err := s.GetAgent(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, "halted", got.Phase)
	}
}

func TestAgentHalted_ReasonAfterPlainStopHalts(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	r := defaultHaltRow()
	r.phase, r.activity = "stopped", ""
	id := newHaltAgent(t, ctx, s, projectID, "tomb", r)

	// The tombstone carries the reason with no phase and no exit code.
	res := apply(t, ctx, s, id, heartbeat("", "preempted", nil))
	assert.Equal(t, store.AgentStatusResult{Halted: true, HaltedFrom: ""}, res, "a same-phase upgrade has no HaltedFrom")
	got, err := s.GetAgent(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "halted", got.Phase)
	assert.Equal(t, "preempted", got.ExitReason)
}

func TestAgentHalted_OOMAfterCrashHalts(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	r := defaultHaltRow()
	r.phase, r.activity, r.exitCode, r.exitReason = "error", "", intp(137), "crashed"
	id := newHaltAgent(t, ctx, s, projectID, "oom", r)

	res := apply(t, ctx, s, id, heartbeat("error", "oom_killed", intp(137)))
	assert.Equal(t, store.AgentStatusResult{Halted: true, HaltedFrom: ""}, res)
	got, err := s.GetAgent(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "halted", got.Phase)
	assert.Equal(t, "oom_killed", got.ExitReason)
}

func TestAgentHalted_ErrorUpgradeOverStopHaltsFromError(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	r := defaultHaltRow()
	r.phase, r.activity, r.exitCode = "stopped", "", intp(0)
	id := newHaltAgent(t, ctx, s, projectID, "emptydir", r)

	// The broker reports error for a disruption whose workspace did not
	// survive; the row was a plain clean stop from sciontool. The exit code
	// on the update is nil, so the exit-0 block does not apply.
	res := apply(t, ctx, s, id, heartbeat("error", "preempted", nil))
	assert.Equal(t, store.AgentStatusResult{Halted: true, HaltedFrom: "error"}, res)
}

func TestAgentHalted_NoHalt(t *testing.T) {
	stopped := func(mut func(r *haltRow)) haltRow {
		r := defaultHaltRow()
		r.phase, r.activity = "stopped", ""
		if mut != nil {
			mut(&r)
		}
		return r
	}
	running := func(mut func(r *haltRow)) haltRow {
		r := defaultHaltRow()
		if mut != nil {
			mut(&r)
		}
		return r
	}
	cases := []struct {
		name      string
		row       haltRow
		su        store.AgentStatusUpdate
		wantPhase string
	}{
		{"clean stop then evicted exit 0", stopped(func(r *haltRow) { r.exitCode = intp(0) }), heartbeat("stopped", "evicted", intp(0)), "stopped"},
		{"clean stop, no code, then evicted exit 0", stopped(nil), heartbeat("", "evicted", intp(0)), "stopped"},
		// The runtime's own exit 0 blocks the later upgrade even over a
		// non-zero code already on the row: the row's code is not read.
		{"crash code on the row, then preempted with exit 0", stopped(func(r *haltRow) { r.phase, r.exitCode = "error", intp(137) }), heartbeat("", "preempted", intp(0)), "error"},
		{"intent stopped", running(func(r *haltRow) { r.intent = store.RunIntentStopped }), heartbeat("stopped", "preempted", nil), "stopped"},
		{"no intent recorded", running(func(r *haltRow) { r.intent = "" }), heartbeat("stopped", "preempted", nil), "stopped"},
		{"no evidence", running(nil), heartbeat("stopped", "", intp(0)), "stopped"},
		{"crash is not evidence", running(nil), heartbeat("error", "crashed", intp(1)), "error"},
		{"platform reason already recorded", stopped(func(r *haltRow) { r.exitReason = "evicted" }), heartbeat("", "preempted", nil), "stopped"},
		{"limits stop with no runtime reason", running(nil), func() store.AgentStatusUpdate {
			su := selfReport("stopped", "limits_exceeded")
			su.ExitReason = "limits_exceeded"
			return su
		}(), "stopped"},
		{"suspended row", running(func(r *haltRow) { r.phase, r.activity = "suspended", "" }), heartbeat("stopped", "preempted", nil), "stopped"},
		{"report of a running phase", stopped(nil), heartbeat("running", "preempted", nil), "running"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s, projectID := newTestAgentStore(t)
			id := newHaltAgent(t, ctx, s, projectID, "nohalt", tc.row)
			res := apply(t, ctx, s, id, tc.su)
			assert.False(t, res.Halted)
			got, err := s.GetAgent(ctx, id)
			require.NoError(t, err)
			assert.Equal(t, tc.wantPhase, got.Phase)
		})
	}
}

func TestAgentHalted_Runtimes(t *testing.T) {
	for _, rt := range []string{"", "cloudrun-sandbox", "cloudrun", "substrate", "managed:x"} {
		t.Run("runtime="+rt, func(t *testing.T) {
			ctx := context.Background()
			s, projectID := newTestAgentStore(t)
			r := defaultHaltRow()
			r.runtime = rt
			id := newHaltAgent(t, ctx, s, projectID, "untrusted-runtime", r)
			res := apply(t, ctx, s, id, heartbeat("stopped", "preempted", nil))
			assert.False(t, res.Halted)
			got, err := s.GetAgent(ctx, id)
			require.NoError(t, err)
			assert.Equal(t, "stopped", got.Phase)
		})
	}
	for _, rt := range []string{"docker", "podman", "container", "kubernetes"} {
		t.Run("eligible="+rt, func(t *testing.T) {
			ctx := context.Background()
			s, projectID := newTestAgentStore(t)
			r := defaultHaltRow()
			r.runtime = rt
			id := newHaltAgent(t, ctx, s, projectID, "trusted-runtime", r)
			assert.True(t, apply(t, ctx, s, id, heartbeat("stopped", "preempted", nil)).Halted)
		})
	}
}

func TestAgentHalted_StartClaimBlocks(t *testing.T) {
	// Any start claim blocks a halt: a start's, a restart's (its stop leg),
	// or the reaper's stop-kind claim; the store does not tell them apart.
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	r := defaultHaltRow()
	r.claimID = "claim-1"
	id := newHaltAgent(t, ctx, s, projectID, "claim-held", r)

	res := apply(t, ctx, s, id, heartbeat("stopped", "preempted", nil))
	assert.False(t, res.Halted)
	got, err := s.GetAgent(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "stopped", got.Phase)
	assert.Equal(t, "preempted", got.ExitReason)

	// Once the claim is released, a later same-run report of the reason
	// does not halt that row: a platform reason is already recorded.
	clearClaim(t, ctx, s, id)
	res = apply(t, ctx, s, id, heartbeat("", "preempted", nil))
	assert.False(t, res.Halted, "a recorded platform reason blocks the later upgrade")
	got, err = s.GetAgent(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "stopped", got.Phase)

	// Partly self-healing: a run that stopped plainly under the claim is
	// halted by a same-run reason that arrives after the release.
	r2 := defaultHaltRow()
	r2.claimID = "claim-2"
	id2 := newHaltAgent(t, ctx, s, projectID, "claim-held-plain-stop", r2)
	res = apply(t, ctx, s, id2, selfReport("stopped", ""))
	assert.False(t, res.Halted)
	clearClaim(t, ctx, s, id2)
	res = apply(t, ctx, s, id2, heartbeat("", "preempted", nil))
	assert.True(t, res.Halted, "the same-run reason still halts once the claim is gone")
}

func clearClaim(t *testing.T, ctx context.Context, s *AgentStore, id string) {
	t.Helper()
	uid, err := parseUUID(id)
	require.NoError(t, err)
	_, err = s.client.Agent.UpdateOneID(uid).ClearStartClaimID().Save(ctx)
	require.NoError(t, err)
}

func TestAgentHalted_HubInternalWritesNeverHalt(t *testing.T) {
	// A write that is not an agent report (no Heartbeat, no
	// GuardReincarnation) never halts, whatever it carries: the stop
	// recordings, the queued stop and the start rollback are such writes.
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	id := newHaltAgent(t, ctx, s, projectID, "internal", defaultHaltRow())

	res := apply(t, ctx, s, id, store.AgentStatusUpdate{Phase: "stopped", ExitReason: "preempted", ExitCode: intp(143)})
	assert.False(t, res.Halted)
	got, err := s.GetAgent(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "stopped", got.Phase)
	assert.Empty(t, got.ExitReason, "only the agent runtime's report records a platform reason")

	// A Restore write puts halted back, and is itself never a halt.
	res = apply(t, ctx, s, id, store.AgentStatusUpdate{Phase: "halted", Restore: true})
	assert.False(t, res.Halted, "a restore is not a new halt")
	got, err = s.GetAgent(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "halted", got.Phase)
}

func TestAgentHalted_WireDrop(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	id := newHaltAgent(t, ctx, s, projectID, "wire", defaultHaltRow())

	for _, su := range []store.AgentStatusUpdate{
		selfReport("halted", ""),
		heartbeat("halted", "", nil),
		{Phase: "halted"}, // even a hub write, without Restore
	} {
		res := apply(t, ctx, s, id, su)
		assert.False(t, res.Halted)
		got, err := s.GetAgent(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, "running", got.Phase)
	}
}

func TestAgentHalted_RunCheck(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	id := newHaltAgent(t, ctx, s, projectID, "runcheck", defaultHaltRow())
	before, err := s.GetAgent(ctx, id)
	require.NoError(t, err)

	old := heartbeat("stopped", "preempted", intp(143))
	old.ReportRunID = "run-0"
	old.ContainerStatus = "exited (143)"
	old.StartedAt = time.Now().UTC().Format(time.RFC3339)
	old.Message = "old run"
	res := apply(t, ctx, s, id, old)
	assert.False(t, res.Halted)

	oldSelf := selfReport("error", "")
	oldSelf.ReportRunID = "run-0"
	oldSelf.ExitReason = "oom_killed"
	apply(t, ctx, s, id, oldSelf)

	got, err := s.GetAgent(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, before.Phase, got.Phase)
	assert.Equal(t, before.Activity, got.Activity)
	assert.Equal(t, before.ContainerStatus, got.ContainerStatus)
	assert.Equal(t, before.Message, got.Message)
	assert.Equal(t, before.ExitReason, got.ExitReason)
	assert.Equal(t, before.StartedAt, got.StartedAt)
	assert.True(t, got.LastSeen.After(before.LastSeen), "last-seen still advances")
}

func TestAgentHalted_Sticky_Reports(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	r := defaultHaltRow()
	r.phase, r.activity, r.exitReason = "halted", "", "preempted"
	id := newHaltAgent(t, ctx, s, projectID, "sticky", r)

	for _, su := range []store.AgentStatusUpdate{
		selfReport("error", ""),
		selfReport("stopped", ""),
		selfReport("running", "working"),
		selfReport("", "working"),
		heartbeat("running", "", nil),
		func() store.AgentStatusUpdate {
			su := heartbeat("running", "", nil)
			su.Activity = "thinking"
			return su
		}(),
	} {
		res := apply(t, ctx, s, id, su)
		assert.False(t, res.Halted)
		got, err := s.GetAgent(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, "halted", got.Phase)
		assert.Equal(t, "", got.Activity)
	}
}

func TestAgentHalted_Sticky_ReasonRefinementApplies(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	r := defaultHaltRow()
	r.phase, r.activity, r.exitReason = "halted", "", "container_missing"
	id := newHaltAgent(t, ctx, s, projectID, "refine", r)

	apply(t, ctx, s, id, heartbeat("stopped", "oom_killed", intp(137)))
	got, err := s.GetAgent(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "halted", got.Phase)
	assert.Equal(t, "oom_killed", got.ExitReason)

	// Only the runtime's halt evidence refines it: a crash or limits reason
	// from the runtime, and any reason or code the agent reports, is not
	// recorded on a halted row.
	for _, su := range []store.AgentStatusUpdate{
		heartbeat("error", "crashed", intp(1)),
		heartbeat("stopped", "limits_exceeded", intp(0)),
		func() store.AgentStatusUpdate {
			su := selfReport("error", "")
			su.ExitReason = "crashed"
			su.ExitCode = intp(2)
			return su
		}(),
		func() store.AgentStatusUpdate {
			su := selfReport("stopped", "")
			su.ExitReason = "preempted"
			return su
		}(),
	} {
		apply(t, ctx, s, id, su)
		got, err := s.GetAgent(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, "halted", got.Phase)
		assert.Equal(t, "oom_killed", got.ExitReason, "%+v", su)
		require.NotNil(t, got.ExitCode)
		assert.Equal(t, 137, *got.ExitCode, "%+v", su)
	}
}

func TestAgentHalted_ReasonIsAClosedSet(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	r := defaultHaltRow()
	r.phase, r.activity = "stopped", ""
	id := newHaltAgent(t, ctx, s, projectID, "closed", r)

	for _, reason := range []string{"bogus", "preempted\x1b[31m", "evicted\nwith a newline", "<b>preempted</b>"} {
		self := selfReport("", "")
		self.ExitReason = reason
		hb := heartbeat("", reason, nil)
		for _, su := range []store.AgentStatusUpdate{self, hb} {
			res := apply(t, ctx, s, id, su)
			assert.False(t, res.Halted)
			got, err := s.GetAgent(ctx, id)
			require.NoError(t, err)
			assert.Empty(t, got.ExitReason, "reason %q is not stored", reason)
			assert.Equal(t, "stopped", got.Phase)
		}
	}
}

func TestAgentHalted_SelfReportNeverHalts(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	id := newHaltAgent(t, ctx, s, projectID, "self", defaultHaltRow())

	// Every halt-related field a report can carry, from the agent's own
	// endpoint (a heartbeat-flagged one, as sciontool sends): never a halt,
	// and the platform reasons are not recorded.
	su := selfReport("stopped", "")
	su.Heartbeat = true
	su.ExitReason = "oom_killed"
	su.ExitCode = intp(137)
	res := apply(t, ctx, s, id, su)
	assert.False(t, res.Halted)
	got, err := s.GetAgent(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "stopped", got.Phase)
	assert.Empty(t, got.ExitReason)
}

func TestAgentHalted_SelfReportDoesNotSuppressLaterHalt(t *testing.T) {
	newRow := func(t *testing.T, mut func(r *haltRow)) (*AgentStore, context.Context, string) {
		ctx := context.Background()
		s, projectID := newTestAgentStore(t)
		r := defaultHaltRow()
		if mut != nil {
			mut(&r)
		}
		return s, ctx, newHaltAgent(t, ctx, s, projectID, "suppress", r)
	}

	t.Run("limits activity then a runtime disruption", func(t *testing.T) {
		s, ctx, id := newRow(t, nil)
		lim := selfReport("stopped", "limits_exceeded")
		lim.ExitReason = "limits_exceeded"
		apply(t, ctx, s, id, lim)
		assert.True(t, apply(t, ctx, s, id, heartbeat("", "preempted", nil)).Halted)
	})
	t.Run("exit code 0 after a runtime crash code, then a runtime disruption", func(t *testing.T) {
		s, ctx, id := newRow(t, func(r *haltRow) { r.phase, r.activity, r.exitCode = "error", "", intp(137) })
		zero := selfReport("stopped", "")
		zero.ExitCode = intp(0)
		apply(t, ctx, s, id, zero)
		assert.True(t, apply(t, ctx, s, id, heartbeat("", "preempted", nil)).Halted)
	})
	t.Run("a self-reported platform reason, then the runtime's", func(t *testing.T) {
		s, ctx, id := newRow(t, nil)
		fake := selfReport("stopped", "")
		fake.ExitReason = "preempted"
		apply(t, ctx, s, id, fake)
		assert.True(t, apply(t, ctx, s, id, heartbeat("", "evicted", nil)).Halted)
	})
	t.Run("a plain stop, then the runtime's reason", func(t *testing.T) {
		s, ctx, id := newRow(t, nil)
		apply(t, ctx, s, id, selfReport("stopped", ""))
		assert.True(t, apply(t, ctx, s, id, heartbeat("", "preempted", nil)).Halted)
	})
	t.Run("completed, then the runtime's reason", func(t *testing.T) {
		s, ctx, id := newRow(t, nil)
		apply(t, ctx, s, id, selfReport("stopped", "completed"))
		assert.True(t, apply(t, ctx, s, id, heartbeat("", "preempted", nil)).Halted)
	})
}

func TestAgentHalted_Sticky_ClaimHeld(t *testing.T) {
	newRow := func(t *testing.T) (*AgentStore, context.Context, string) {
		ctx := context.Background()
		s, projectID := newTestAgentStore(t)
		r := defaultHaltRow()
		r.phase, r.activity, r.exitReason, r.claimID = "halted", "", "preempted", "claim-1"
		return s, ctx, newHaltAgent(t, ctx, s, projectID, "claim", r)
	}
	phaseAfter := func(t *testing.T, s *AgentStore, ctx context.Context, id string, su store.AgentStatusUpdate) string {
		apply(t, ctx, s, id, su)
		got, err := s.GetAgent(ctx, id)
		require.NoError(t, err)
		return got.Phase
	}

	t.Run("self-report stopped stays halted", func(t *testing.T) {
		s, ctx, id := newRow(t)
		assert.Equal(t, "halted", phaseAfter(t, s, ctx, id, selfReport("stopped", "")))
	})
	t.Run("broker heartbeat running stays halted", func(t *testing.T) {
		s, ctx, id := newRow(t)
		assert.Equal(t, "halted", phaseAfter(t, s, ctx, id, heartbeat("running", "", nil)))
	})
	t.Run("self-report running applies", func(t *testing.T) {
		s, ctx, id := newRow(t)
		assert.Equal(t, "running", phaseAfter(t, s, ctx, id, selfReport("running", "working")))
		got, err := s.GetAgent(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, "working", got.Activity, "the exception applies the activity too")
	})
	t.Run("self-report starting applies", func(t *testing.T) {
		s, ctx, id := newRow(t)
		assert.Equal(t, "starting", phaseAfter(t, s, ctx, id, selfReport("starting", "")))
	})
	t.Run("sciontool heartbeat-flagged self-report running applies", func(t *testing.T) {
		s, ctx, id := newRow(t)
		su := selfReport("running", "working")
		su.Heartbeat = true
		assert.Equal(t, "running", phaseAfter(t, s, ctx, id, su))
	})
}

func TestAgentHalted_StrandedThenAdopted(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	r := defaultHaltRow()
	r.runID = "run-stranded"
	id := newHaltAgent(t, ctx, s, projectID, "stranded", r)

	// The broker's container holds run-1; the row names run-stranded.
	res := apply(t, ctx, s, id, heartbeat("stopped", "preempted", nil))
	assert.False(t, res.Halted)
	got, err := s.GetAgent(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "running", got.Phase, "reports of another run apply nothing")

	adopted, err := s.AdoptAgentRunID(ctx, id, "run-stranded", "run-1", time.Now())
	require.NoError(t, err)
	require.True(t, adopted)

	res = apply(t, ctx, s, id, heartbeat("stopped", "preempted", nil))
	assert.True(t, res.Halted, "after adoption the run's reports apply")
}

func TestAgentHalted_HubStartFromHalted(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	r := defaultHaltRow()
	r.phase, r.activity, r.exitReason, r.exitCode = "halted", "", "preempted", intp(143)
	id := newHaltAgent(t, ctx, s, projectID, "start", r)

	// The start's provisional write, conditional on halted.
	require.NoError(t, s.UpdateAgentStatus(ctx, id, store.AgentStatusUpdate{Phase: "starting", IfPhase: "halted"}))
	// A running write from a terminal phase clears the old run's remnants.
	r2 := defaultHaltRow()
	r2.phase, r2.activity, r2.exitReason, r2.exitCode = "halted", "", "preempted", intp(143)
	id2 := newHaltAgent(t, ctx, s, projectID, "start2", r2)
	require.NoError(t, s.UpdateAgentStatus(ctx, id2, store.AgentStatusUpdate{Phase: "running"}))
	got, err := s.GetAgent(ctx, id2)
	require.NoError(t, err)
	assert.Equal(t, "running", got.Phase)
	assert.Equal(t, "", got.ExitReason)
	assert.Nil(t, got.ExitCode)

	got, err = s.GetAgent(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "starting", got.Phase)
}

func TestAgentHalted_CAS(t *testing.T) {
	cutoff := time.Now().Add(-5 * time.Minute)
	cases := []struct {
		name       string
		mut        func(r *haltRow)
		runtime    string
		wantPhase  string
		wantReason string
	}{
		{"halts", nil, "kubernetes", "halted", "container_missing"},
		{"keeps a disruption reason", func(r *haltRow) { r.exitReason = "preempted" }, "kubernetes", "halted", "preempted"},
		{"stopping row halts", func(r *haltRow) { r.phase = "stopping" }, "docker", "halted", "container_missing"},
		{"claim held", func(r *haltRow) { r.claimID = "claim-1" }, "kubernetes", "error", "container_missing"},
		{"intent stopped", func(r *haltRow) { r.intent = store.RunIntentStopped }, "kubernetes", "error", "container_missing"},
		{"Cloud Run inventory target", nil, "cloudrun", "error", "container_missing"},
		{"no inventory runtime", nil, "", "error", "container_missing"},
		// The inventory decides, not the runtime recorded on the agent.
		{"agent runtime empty, inventory kubernetes", func(r *haltRow) { r.runtime = "" }, "kubernetes", "halted", "container_missing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s, projectID := newTestAgentStore(t)
			r := defaultHaltRow()
			if tc.mut != nil {
				tc.mut(&r)
			}
			id := newHaltAgent(t, ctx, s, projectID, "cas", r)
			got, err := s.MarkAgentContainerMissing(ctx, id, "broker-1", cutoff, tc.runtime, "gone")
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, tc.wantPhase, got.Phase)
			assert.Equal(t, tc.wantReason, got.ExitReason)
			assert.Equal(t, "", got.Activity)

			// A second CAS on the now-terminal row changes nothing.
			again, err := s.MarkAgentContainerMissing(ctx, id, "broker-1", cutoff, tc.runtime, "gone")
			require.NoError(t, err)
			assert.Nil(t, again)
		})
	}

	t.Run("exec path never halts", func(t *testing.T) {
		ctx := context.Background()
		s, projectID := newTestAgentStore(t)
		id := newHaltAgent(t, ctx, s, projectID, "exec", defaultHaltRow())
		a, err := s.GetAgent(ctx, id)
		require.NoError(t, err)
		got, err := s.MarkAgentContainerMissingIfUnchanged(ctx, id, "broker-1", cutoff,
			store.ContainerMissingPrecondition{StateVersion: a.StateVersion, RunID: a.RunID}, "gone")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "error", got.Phase)
	})
}

func TestAgentHalted_Sites(t *testing.T) {
	assert.True(t, isWindDownPhase("halted"), "the launch reaper winds down a halted create")
	assert.True(t, agentNeedsAttention("halted", ""))
	a := &store.Agent{LaunchKind: "create", Phase: "halted", LaunchState: "active"}
	assert.True(t, a.IsIncompleteCreate())
}
