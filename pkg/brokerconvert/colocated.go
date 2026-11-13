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

package brokerconvert

import (
	"context"
	"errors"
	"net"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// Co-located conversion (design section 4.8). On a host whose server
// process runs the Hub and embeds the Runtime Broker, the Hub is down
// whenever the server is stopped, so apply and rollback are each split
// around the stop:
//
//   - the RUNNING phase (server up) does the Hub writes and leaves the
//     journal at JournalAwaitingStop;
//   - the STOPPED phase (server down, host lock held) does the local
//     settings and directory changes.
//
// A journal at JournalAwaitingStop with Rollback == nil is an apply waiting
// for the stop; with Rollback != nil it is a rollback waiting for the stop.

// ColocatedPhase is the part of a co-located apply or rollback that one
// invocation runs.
type ColocatedPhase string

const (
	// ColocatedRunning: the Hub writes, with the server up.
	ColocatedRunning ColocatedPhase = "running"
	// ColocatedStopped: the local writes, with the server down.
	ColocatedStopped ColocatedPhase = "stopped"
	// ColocatedStartPending: the settings are written; the server has not
	// been started since.
	ColocatedStartPending ColocatedPhase = "start-pending"
	// ColocatedCleanup: the server is up after the switch; the unused
	// instance credentials files are removed once each instance is online.
	ColocatedCleanup ColocatedPhase = "cleanup"
	// ColocatedDone: nothing is left to do.
	ColocatedDone ColocatedPhase = "done"
)

// StopRecheckBound is the Q7 time bound: the stopped phase refuses when the
// running phase's drained re-check is older than this. It can only refuse.
const StopRecheckBound = 30 * time.Minute

// ColocatedEnv is what the co-located phases use from the host, beyond Env.
type ColocatedEnv struct {
	// ServerUp reports whether this host's server answers (its daemon is
	// running, or anything answers on its listeners).
	ServerUp func(ctx context.Context) bool
	// Q7 builds the stopped phase's drained-check input from the journal.
	Q7 func(ctx context.Context, j *Journal) Q7Input
	// SingleHubReplica is the operator's --single-hub-replica flag.
	SingleHubReplica bool
	// InstanceOnline reports whether an instance row is online now. It is
	// called only in the cleanup phase, which runs only while this host's
	// server answers: then "online now" implies "online at least once". A
	// stale offline only delays the cleanup.
	InstanceOnline func(ctx context.Context, brokerID string) (bool, error)
	// RemoveCredentials removes one instance's unused credentials files
	// (privatefs, no link followed).
	RemoveCredentials func(key string) error
}

// NextColocatedApplyPhase returns the phase a co-located '--apply' runs,
// from the journal (nil when absent) and whether this host's server
// answers.
func NextColocatedApplyPhase(j *Journal, serverUp bool) (ColocatedPhase, *Refusal) {
	if j == nil || j.State == JournalRolledBack {
		if !serverUp {
			return "", refuse(RefuseColocatedServerDown)
		}
		return ColocatedRunning, nil
	}
	if j.Rollback != nil || j.State == JournalRollingBack {
		return "", refuse(RefuseRollingBack)
	}
	switch j.State {
	case JournalApplying:
		if !serverUp {
			return "", refuse(RefuseColocatedServerDown)
		}
		return ColocatedRunning, nil
	case JournalAwaitingStop:
		// With the server still up, the running phase runs again: its
		// steps are idempotent, and it repeats the drained re-check and
		// restarts the time bound. The stopped phase never runs while the
		// server answers.
		if serverUp {
			return ColocatedRunning, nil
		}
		return ColocatedStopped, nil
	case JournalApplied:
		if !serverUp {
			return ColocatedStartPending, nil
		}
		if len(credentialsCleanupPending(j)) == 0 {
			return ColocatedDone, nil
		}
		return ColocatedCleanup, nil
	}
	return "", refuse(RefuseColocatedJournal)
}

// NextColocatedRollbackPhase returns the phase a co-located '--rollback'
// runs.
func NextColocatedRollbackPhase(j *Journal, serverUp bool) (ColocatedPhase, *Refusal) {
	if j == nil {
		return "", refuse(RefuseNoJournal)
	}
	switch {
	case j.State == JournalRolledBack:
		return "", refuse(RefuseNothingToRollBack)
	case j.State == JournalAwaitingStop && j.Rollback != nil:
		// With the server still up, the Hub part runs again (idempotent).
		if serverUp {
			return ColocatedRunning, nil
		}
		return ColocatedStopped, nil
	case j.State == JournalApplying, j.State == JournalAwaitingStop,
		j.State == JournalApplied, j.State == JournalRollingBack:
		if !serverUp {
			return "", refuse(RefuseColocatedServerDown)
		}
		return ColocatedRunning, nil
	}
	return "", refuse(RefuseColocatedJournal)
}

// credentialsCleanupPending returns the indexes of the register items whose
// credentials file is not yet removed. A nil record is not applicable.
func credentialsCleanupPending(j *Journal) []int {
	var out []int
	for i := range j.Register {
		c := j.Register[i].CredentialsCleanup
		if c != nil && c.State != StepDone {
			out = append(out, i)
		}
	}
	return out
}

// StoreTopology is how many Hub processes may serve this host's Hub store,
// as far as the effective server settings tell (Q7).
type StoreTopology string

const (
	// TopologyReplica: a configured HA or replica setup.
	TopologyReplica StoreTopology = "replica"
	// TopologySingleSQLite: one Hub on a local SQLite file.
	TopologySingleSQLite StoreTopology = "single-sqlite"
	// TopologySingleExternal: an external database with no replica marker.
	TopologySingleExternal StoreTopology = "single-external"
	// TopologyUndetermined: the settings do not tell.
	TopologyUndetermined StoreTopology = "undetermined"
)

// Q7Input is what the stopped phase's drained check needs. The probes are
// called only when the topology requires them, at most once each.
type Q7Input struct {
	Topology         StoreTopology
	SingleHubReplica bool
	StopRecheckAt    *time.Time
	Now              time.Time
	// LegacyStore reads the local SQLite store read-only: whether it holds
	// this host's legacy Runtime Broker row, and the non-terminal agents on
	// that row.
	LegacyStore func() (LegacyStoreFacts, error)
	// EndpointAnswers probes the Hub endpoint; an error means it could not
	// be probed.
	EndpointAnswers func() (bool, error)
}

// LegacyStoreFacts is what the read-only SQLite read finds.
type LegacyStoreFacts struct {
	// RowPresent: the store holds this host's legacy Runtime Broker row (the
	// journal's legacyBrokerId). Without it the store is not this Hub's
	// store (for example a stale hub.db on a host that is now remote).
	RowPresent bool
	// Agents are the non-terminal agents on that row.
	Agents []AgentEntry
}

// Q7Check is the stopped phase's drained check (design Q7). It refuses
// unless every condition for the topology holds; nothing here passes a
// check on its own. Order: the topology refusals (so a replica setup is
// named as such with or without the flag), the flag's placement, the time
// bound, then the topology's own checks.
func Q7Check(in Q7Input) *Refusal {
	switch in.Topology {
	case TopologyReplica:
		return refuse(RefuseStoreReplica)
	case TopologySingleSQLite, TopologySingleExternal:
	default:
		return refuse(RefuseStoreUnknown)
	}
	if in.SingleHubReplica && in.Topology != TopologySingleExternal {
		return refuse(RefuseSingleHubMisplaced)
	}
	if in.StopRecheckAt == nil || in.StopRecheckAt.IsZero() {
		return refuse(RefuseStopRecheckMissing)
	}
	if in.StopRecheckAt.After(in.Now) || in.Now.Sub(*in.StopRecheckAt) > StopRecheckBound {
		return refuse(RefuseStopRecheckExpired)
	}
	if in.Topology == TopologySingleSQLite {
		if in.LegacyStore == nil {
			return refuse(RefuseStoreUnreadable)
		}
		facts, err := in.LegacyStore()
		if err != nil {
			return refuse(RefuseStoreUnreadable)
		}
		// Store identity: a store without this host's legacy row is not
		// this Hub's store; its count proves nothing.
		if !facts.RowPresent {
			return refuse(RefuseStoreUnreadable)
		}
		if len(facts.Agents) > 0 {
			ref := refuse(RefuseNotDrained)
			ref.Agents = facts.Agents
			return ref
		}
		return nil
	}
	if !in.SingleHubReplica {
		return refuse(RefuseSingleHubRequired)
	}
	if in.EndpointAnswers == nil {
		return refuse(RefuseHubEndpointUnprobed)
	}
	answered, err := in.EndpointAnswers()
	if err != nil {
		return refuse(RefuseHubEndpointUnprobed)
	}
	if answered {
		return refuse(RefuseHubEndpointAnswers)
	}
	return nil
}

// errNoColocatedEnv is returned when a co-located entry point gets no
// host seams.
var errNoColocatedEnv = errors.New("co-located convert: no host environment")

// ApplyColocated runs the phase of a co-located '--apply' that the journal
// and the server state select (design section 4.8). It holds the host lock
// for its whole run; in the running phase that serialises convert runs
// only (the server holds the lock only in its start window).
func ApplyColocated(ctx context.Context, env *Env, ce *ColocatedEnv, in ApplyInput) (*Report, error) {
	if ce == nil || ce.ServerUp == nil {
		return nil, errNoColocatedEnv
	}
	lock, err := AcquireHostLock(env.GlobalDir)
	if err != nil {
		if errors.Is(err, ErrHostLockHeld) {
			return refused(ModeApply, nil, refuse(RefuseLockHeld))
		}
		return refused(ModeApply, nil, refuse(RefuseLocalWrite))
	}
	defer lock.Release()

	j, err := LoadJournal(env.GlobalDir)
	if err != nil {
		return refused(ModeApply, nil, refuse(RefuseLocalWrite))
	}
	phase, ref := NextColocatedApplyPhase(j, ce.ServerUp(ctx))
	if ref != nil {
		return refused(ModeApply, j, ref)
	}
	if ce.SingleHubReplica && phase != ColocatedStopped {
		return refused(ModeApply, j, refuse(RefuseSingleHubMisplaced))
	}
	switch phase {
	case ColocatedRunning:
		return colocatedRunning(ctx, env, in)
	case ColocatedStopped:
		return colocatedStopped(ctx, env, ce, in)
	case ColocatedCleanup:
		return colocatedCleanup(ctx, env, ce, j)
	case ColocatedStartPending:
		rep := reportFrom(ModeApply, j)
		rep.Notes = append(rep.Notes, NoteColocatedStart)
		return rep, nil
	default:
		rep := reportFrom(ModeApply, j)
		rep.Notes = append(rep.Notes, NoteColocatedDone)
		return rep, nil
	}
}

// colocatedRunning: steps 1 and 3 with the server up (P-4 is replaced by
// "the server is running"), the drained re-checks, then awaiting-stop and
// the outage text.
func colocatedRunning(ctx context.Context, env *Env, in ApplyInput) (*Report, error) {
	st, rep, err := startApply(env, in)
	if rep != nil {
		return rep, err
	}
	r := &run{ctx: ctx, env: env, j: st.j}
	legacyID := st.plan.LegacyBrokerID
	// P-3 (the legacy Runtime Broker is drained); the server is up.
	if ref := r.drained(legacyID, RefuseNotDrained); ref != nil {
		return refused(ModeApply, r.j, ref)
	}
	projects, ref := r.applyPrechecks(st)
	if ref != nil {
		return refused(ModeApply, r.j, ref)
	}
	if ref := r.applyBegin(st); ref != nil {
		return refused(ModeApply, r.j, ref)
	}
	// Each instance pre-registered over HTTP keeps a credentials file the
	// co-located instance never reads: its removal is recorded as pending.
	for i := range r.j.Register {
		if r.j.Register[i].CredentialsCleanup == nil {
			r.j.Register[i].CredentialsCleanup = &ItemStatus{State: StepPending}
		}
	}
	if ref := r.applyRegister(st); ref != nil {
		return refused(ModeApply, r.j, ref)
	}
	// Drained re-check before step 3 (section 4.6).
	if ref := r.drained(legacyID, RefuseNotDrained); ref != nil {
		return refused(ModeApply, r.j, ref)
	}
	if ref := r.repointProjects(st.plan, in.LegacyDefaultProfile, projects); ref != nil {
		return refused(ModeApply, r.j, ref)
	}
	// The re-check immediately before the stop (Q7 (c)); its time starts
	// the stopped phase's time bound.
	if ref := r.drained(legacyID, RefuseNotDrained); ref != nil {
		return refused(ModeApply, r.j, ref)
	}
	at := env.now().UTC()
	r.j.StopRecheckAt = &at
	r.j.State = JournalAwaitingStop
	if ref := r.save(); ref != nil {
		return refused(ModeApply, r.j, ref)
	}
	rep = reportFrom(ModeApply, r.j)
	rep.Notes = append(rep.Notes, NoteEvidenceKept, NoteColocatedAwaitingStop)
	return rep, nil
}

// colocatedStopped: Q7, the assertion journaled, then step 2 and the start
// instruction. A Q7 refusal leaves the journal unchanged.
func colocatedStopped(ctx context.Context, env *Env, ce *ColocatedEnv, in ApplyInput) (*Report, error) {
	start := startApply
	if in.LocalPlan {
		// The Hub is this stopped server: the plan was built locally.
		start = startLocalApply
	}
	st, rep, err := start(env, in)
	if rep != nil {
		return rep, err
	}
	r := &run{ctx: ctx, env: env, j: st.j}
	if r.j == nil || r.j.State != JournalAwaitingStop {
		return refused(ModeApply, r.j, refuse(RefuseColocatedJournal))
	}
	var q Q7Input
	if ce.Q7 != nil {
		q = ce.Q7(ctx, r.j)
	}
	q.SingleHubReplica = ce.SingleHubReplica
	q.StopRecheckAt = r.j.StopRecheckAt
	q.Now = env.now().UTC()
	if ref := Q7Check(q); ref != nil {
		return refused(ModeApply, r.j, ref)
	}
	if ce.SingleHubReplica {
		r.j.SingleHubReplica = true
		if ref := r.save(); ref != nil {
			return refused(ModeApply, r.j, ref)
		}
	}
	// Step 2: the settings.
	if ref := r.writeSettings(st.insts); ref != nil {
		return refused(ModeApply, r.j, ref)
	}
	r.j.Switch = ItemStatus{State: StepDone}
	r.j.State = JournalApplied
	if ref := r.save(); ref != nil {
		return refused(ModeApply, r.j, ref)
	}
	rep = reportFrom(ModeApply, r.j)
	rep.Notes = append(rep.Notes, NoteLegacyLinks, NoteColocatedStart)
	return rep, nil
}

// colocatedCleanup removes the unused credentials files of the instances
// that are online, each journaled started then done.
func colocatedCleanup(ctx context.Context, env *Env, ce *ColocatedEnv, j *Journal) (*Report, error) {
	r := &run{ctx: ctx, env: env, j: j}
	waiting := false
	for _, i := range credentialsCleanupPending(j) {
		it := &j.Register[i]
		online := false
		if ce.InstanceOnline != nil && it.BrokerID != "" {
			var err error
			if online, err = ce.InstanceOnline(ctx, it.BrokerID); err != nil {
				return refused(ModeApply, j, refuseHub(RefuseHubRequest, err))
			}
		}
		if !online {
			waiting = true
			continue
		}
		it.CredentialsCleanup = &ItemStatus{State: StepStarted}
		if ref := r.save(); ref != nil {
			return refused(ModeApply, j, ref)
		}
		if ce.RemoveCredentials == nil || ce.RemoveCredentials(it.Key) != nil {
			ref := refuse(RefuseCredentialsCleanup)
			it.CredentialsCleanup = &ItemStatus{State: StepStarted, Error: fixedError(ref)}
			_ = r.save()
			return refused(ModeApply, j, ref)
		}
		it.CredentialsCleanup = &ItemStatus{State: StepDone}
		if ref := r.save(); ref != nil {
			return refused(ModeApply, j, ref)
		}
	}
	rep := reportFrom(ModeApply, j)
	if waiting {
		rep.Notes = append(rep.Notes, NoteColocatedCleanupWaiting)
	} else {
		rep.Notes = append(rep.Notes, NoteColocatedDone)
	}
	return rep, nil
}

// RollbackColocated runs the phase of a co-located '--rollback' (design
// section 8.4): the Hub part with the server up, then the local part with
// the server down. Before the local part moves each identity directory
// aside, its unused credentials file is removed (the rows are already
// deregistered), so the moved tree holds no secret.
func RollbackColocated(ctx context.Context, env *Env, ce *ColocatedEnv) (*Report, error) {
	if ce == nil || ce.ServerUp == nil {
		return nil, errNoColocatedEnv
	}
	lock, err := AcquireHostLock(env.GlobalDir)
	if err != nil {
		if errors.Is(err, ErrHostLockHeld) {
			return refused(ModeRollback, nil, refuse(RefuseLockHeld))
		}
		return refused(ModeRollback, nil, refuse(RefuseLocalWrite))
	}
	defer lock.Release()

	if ce.SingleHubReplica {
		return refused(ModeRollback, nil, refuse(RefuseSingleHubMisplaced))
	}
	r, rep, err := startRollback(ctx, env)
	if rep != nil {
		return rep, err
	}
	phase, ref := NextColocatedRollbackPhase(r.j, ce.ServerUp(ctx))
	if ref != nil {
		return refused(ModeRollback, r.j, ref)
	}
	if phase == ColocatedRunning {
		notes, ref := r.rollbackHub()
		if ref != nil {
			return refused(ModeRollback, r.j, ref)
		}
		r.j.State = JournalAwaitingStop
		if ref := r.save(); ref != nil {
			return refused(ModeRollback, r.j, ref)
		}
		rep = reportFrom(ModeRollback, r.j)
		rep.Notes = append(rep.Notes, notes...)
		rep.Notes = append(rep.Notes, NoteColocatedRollbackAwaitingStop)
		return rep, nil
	}
	// The stopped phase. Each instance's unused credentials file is removed
	// through the BeforeMove seam: after its row is deregistered and right
	// before its directory moves aside, after any BeforeMove the caller
	// set. A failed removal stops the rollback before that move.
	local := *r.env
	prev := local.BeforeMove
	local.BeforeMove = func(globalDir, key string) error {
		if prev != nil {
			if err := prev(globalDir, key); err != nil {
				return err
			}
		}
		return r.removeInstanceCredentials(ce, key)
	}
	r.env = &local
	if ref := r.rollbackLocal(); ref != nil {
		return refused(ModeRollback, r.j, ref)
	}
	rep = reportFrom(ModeRollback, r.j)
	rep.Notes = append(rep.Notes, NoteColocatedRollbackStart)
	return rep, nil
}

// removeInstanceCredentials removes the unused credentials file of the
// instance key, journaled started then done. A key with no pending
// cleanup is left alone.
func (r *run) removeInstanceCredentials(ce *ColocatedEnv, key string) error {
	for i := range r.j.Register {
		it := &r.j.Register[i]
		if it.Key != key || it.CredentialsCleanup == nil || it.CredentialsCleanup.State == StepDone {
			continue
		}
		it.CredentialsCleanup = &ItemStatus{State: StepStarted}
		if ref := r.save(); ref != nil {
			return ref
		}
		if ce.RemoveCredentials == nil || ce.RemoveCredentials(key) != nil {
			ref := refuse(RefuseCredentialsCleanup)
			it.CredentialsCleanup = &ItemStatus{State: StepStarted, Error: fixedError(ref)}
			_ = r.save()
			return ref
		}
		it.CredentialsCleanup = &ItemStatus{State: StepDone}
		if ref := r.save(); ref != nil {
			return ref
		}
	}
	return nil
}

// Host modes and their sources, as the journal and plan record them.
const (
	HostModeColocated = "colocated"
	HostModeRemote    = "remote"
	// HostModeUnknown is Plan.HostMode when --plan cannot determine the host
	// mode from a saved daemon launch (--host-mode is then required on
	// --apply or --rollback).
	HostModeUnknown  = "unknown"
	HostModeDetected = "detected"
	HostModeAsserted = "asserted"
	// HostModeJournal is HostEvidence.Source when an earlier apply bound the
	// mode.
	HostModeJournal = "journal"
)

// ResolveHostMode decides the host mode of one invocation from what is
// known: detected (from the saved daemon launch; empty when detection
// cannot decide), asserted (--host-mode; empty when not given) and the
// journal (nil when absent). A journal that records a mode binds it: the
// detected mode and the flag must agree with it. Otherwise the detected mode
// and the flag must agree with each other, and the detected mode wins over a
// silent flag. Disagreement refuses with host_mode_conflict, naming both
// values; with nothing known, colocated_mode_unknown. The flag never
// overrides a determined fact.
func ResolveHostMode(detected, asserted string, j *Journal) (mode, source string, ref *Refusal) {
	conflict := func(aName, a, bName, b string) *Refusal {
		r := refuse(RefuseHostModeConflict)
		r.Text += "; " + aName + " " + a + ", " + bName + " " + b
		return r
	}
	if j != nil && j.State != JournalRolledBack && j.HostMode != "" {
		if detected != "" && detected != j.HostMode {
			return "", "", conflict("journal", j.HostMode, "detected", detected)
		}
		if asserted != "" && asserted != j.HostMode {
			return "", "", conflict("journal", j.HostMode, "--host-mode", asserted)
		}
		return j.HostMode, j.HostModeSource, nil
	}
	if detected != "" && asserted != "" && detected != asserted {
		return "", "", conflict("detected", detected, "--host-mode", asserted)
	}
	if detected != "" {
		return detected, HostModeDetected, nil
	}
	if asserted != "" {
		return asserted, HostModeAsserted, nil
	}
	return "", "", refuse(RefuseColocatedUnknown)
}

// HostEvidence is what this host shows about where its Hub is, gathered by
// the command for a co-located mode.
type HostEvidence struct {
	// Source is how the mode is known: HostModeDetected, HostModeAsserted,
	// or HostModeJournal when an earlier apply bound it.
	Source string
	// LegacyCredentials names a saved legacy broker credentials file
	// (broker-credentials.json or hub-credentials/*.json) when one exists.
	// The co-located legacy path never saves one (its credentials are in
	// memory), so its presence means the host registered as remote.
	LegacyCredentials string
	// Named are the Hub endpoints the host's settings name.
	Named []string
	// Configured is the server settings' explicit server.hub.endpoint.
	Configured string
	// LaunchFile is the saved server launch file a detected mode came from,
	// named in the refusal text.
	LaunchFile string
	// Listeners are this host's Hub and web listener URLs from a saved
	// server launch. The command sets them only when a saved launch backs
	// the mode (never in the asserted case), so they are local by evidence.
	Listeners []string
}

// ColocatedHostCheck refuses a co-located mode that this host's evidence
// contradicts (design E4): a saved legacy broker credentials file, or a
// named Hub endpoint that is not this host's local Hub. A named endpoint is
// local when its host:port equals the configured server.hub.endpoint, or,
// when a saved server launch backs the mode, one of its listeners (loopback
// names and a wildcard bind count as one local host). Without a launch, a
// loopback name counts only if it equals the configured endpoint, so a
// tunnel to a remote Hub is not taken for this host. An endpoint that does
// not parse refuses.
func ColocatedHostCheck(mode string, ev HostEvidence) *Refusal {
	if mode != HostModeColocated {
		return nil
	}
	if ev.LegacyCredentials != "" {
		creds := credentialsFileName(ev.LegacyCredentials)
		reason := "a saved legacy broker credentials file (" + creds + ") shows this host registered with a Hub as a remote Runtime Broker"
		if ev.Source == HostModeDetected {
			// Either file may be the stale one: name both.
			launch := filepath.Base(ev.LaunchFile)
			r := refuse(RefuseHostModeConflict)
			r.Text += "; the mode is colocated (detected from the saved server launch file " + launch + ") but " + reason +
				"; remove whichever is stale (the saved server launch file " + launch + ", or the credentials file " + creds +
				"); if this host now serves its own Hub, the credentials file is the stale one"
			return r
		}
		return hostConflict(ev, reason)
	}
	conf, confOK := endpointHostPort(ev.Configured)
	if ev.Configured != "" && !confOK {
		return hostConflict(ev, "server.hub.endpoint does not parse")
	}
	var listeners []string
	for _, l := range ev.Listeners {
		if hp, ok := endpointHostPort(l); ok {
			listeners = append(listeners, hp)
		}
	}
	for _, n := range ev.Named {
		if strings.TrimSpace(n) == "" {
			continue
		}
		hp, ok := endpointHostPort(n)
		if !ok {
			return hostConflict(ev, "the settings name a Hub endpoint that does not parse")
		}
		if (confOK && sameHostPort(hp, conf)) || containsHostPort(listeners, hp) {
			continue
		}
		return hostConflict(ev, "the settings name the Hub endpoint "+hp+", which is not this host's local Hub")
	}
	return nil
}

// credentialsFileName names a saved legacy broker credentials file by its
// fixed name only (no local path): hub-credentials/ for a file in that
// directory, else the file's base name (broker-credentials.json).
func credentialsFileName(path string) string {
	if filepath.Base(filepath.Dir(path)) == "hub-credentials" {
		return "hub-credentials/"
	}
	return filepath.Base(path)
}

// hostConflict is the host_mode_conflict refusal with its reason and the fix
// for the mode's source.
func hostConflict(ev HostEvidence, reason string) *Refusal {
	r := refuse(RefuseHostModeConflict)
	fix := "remove the stale saved server launch file (server-args.json), then run again"
	switch ev.Source {
	case HostModeAsserted:
		fix = "run again with --host-mode remote, or start the server once through the daemon"
	case HostModeJournal:
		fix = "the mode is bound by this conversion's journal: roll back first ('convert --rollback')"
	}
	r.Text += "; the mode is colocated (" + ev.Source + ") but " + reason + "; " + fix
	return r
}

// endpointHostPort returns the lower-cased host:port of an http(s) URL, with
// the scheme's default port filled in and every loopback or wildcard name
// folded to one local host.
func endpointHostPort(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", false
	}
	host, port := u.Hostname(), u.Port()
	if host == "" {
		return "", false
	}
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	host = strings.ToLower(host)
	switch host {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0", "::":
		host = "local"
	}
	return net.JoinHostPort(host, port), true
}

func sameHostPort(a, b string) bool { return a == b }

func containsHostPort(list []string, hp string) bool {
	for _, l := range list {
		if l == hp {
			return true
		}
	}
	return false
}
