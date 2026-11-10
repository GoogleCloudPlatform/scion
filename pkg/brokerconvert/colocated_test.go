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
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// drainedStore is a local store that holds the legacy row and no agent.
func drainedStore() (LegacyStoreFacts, error) { return LegacyStoreFacts{RowPresent: true}, nil }

// agentStore holds the legacy row and one non-terminal agent.
func agentStore() (LegacyStoreFacts, error) {
	return LegacyStoreFacts{RowPresent: true, Agents: []AgentEntry{{ID: "a-1"}}}, nil
}

// plantedJoinSecret is a fake instance secret, as the HTTP pre-registration
// would save it. It must never reach the journal or any output.
const plantedJoinSecret = "PLANTED-JOIN-SECRET-7f3"

// colocatedFixture is an apply fixture plus the co-located host seams: the
// server state, the Q7 input, the instance online state and the removed
// credentials.
type colocatedFixture struct {
	*applyFixture
	serverUp bool
	now      time.Time
	q7       Q7Input
	online   bool
	flag     bool
	removed  []string
	q7Calls  int
}

func newColocatedFixture(t *testing.T) *colocatedFixture {
	t.Helper()
	cf := &colocatedFixture{applyFixture: newApplyFixture(t), serverUp: true, now: testNow}
	cf.q7 = Q7Input{Topology: TopologySingleSQLite, LegacyStore: drainedStore}
	cf.env.Now = func() time.Time { return cf.now }
	reg := cf.env.Register
	// The HTTP pre-registration also saves the instance credentials file.
	cf.env.Register = func(ctx context.Context, inst config.V1RuntimeBrokerInstanceConfig) (string, string, error) {
		id, rt, err := reg(ctx, inst)
		if err != nil {
			return id, rt, err
		}
		dir := filepath.Join(cf.g, "runtime-brokers", inst.Key, "hub-credentials")
		require.NoError(t, os.MkdirAll(dir, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "hub.json"), []byte(`{"secretKey":"`+plantedJoinSecret+`"}`), 0o600))
		return id, rt, nil
	}
	return cf
}

func (cf *colocatedFixture) q7Input(context.Context, *Journal) Q7Input {
	cf.q7Calls++
	return cf.q7
}

func (cf *colocatedFixture) remove(key string) error {
	cf.removed = append(cf.removed, key)
	return os.RemoveAll(filepath.Join(cf.g, "runtime-brokers", key, "hub-credentials"))
}

func (cf *colocatedFixture) ce() *ColocatedEnv {
	return &ColocatedEnv{
		ServerUp:          func(context.Context) bool { return cf.serverUp },
		Q7:                cf.q7Input,
		SingleHubReplica:  cf.flag,
		InstanceOnline:    func(context.Context, string) (bool, error) { return cf.online, nil },
		RemoveCredentials: cf.remove,
	}
}

// applyC runs a co-located apply with the plan the command would build.
// Before step 2 the settings carry no instances key, so the plan is built
// from them; afterwards it is rebuilt as the command does.
func (cf *colocatedFixture) applyC() (*Report, error) {
	cf.t.Helper()
	var in ApplyInput
	insts, err := cf.env.LoadInstances()
	require.NoError(cf.t, err)
	if len(insts) > 0 {
		in = cf.resumeInput()
	} else {
		_, in = cf.plan()
	}
	return ApplyColocated(context.Background(), cf.env, cf.ce(), in)
}

func (cf *colocatedFixture) journal() *Journal {
	cf.t.Helper()
	j, err := LoadJournal(cf.g)
	require.NoError(cf.t, err)
	require.NotNil(cf.t, j)
	return j
}

func (cf *colocatedFixture) credsFile(key string) string {
	return filepath.Join(cf.g, "runtime-brokers", key, "hub-credentials", "hub.json")
}

// toStopped runs the running phase, then stops the server 5 minutes later.
func (cf *colocatedFixture) toStopped() {
	cf.t.Helper()
	_, err := cf.applyC()
	require.NoError(cf.t, err)
	cf.serverUp = false
	cf.now = cf.now.Add(5 * time.Minute)
}

// Scope item 6 and its named mutant: the running phase stops at
// awaiting-stop and prints the outage text, verbatim.
func TestColocatedRunning_PrintsOutageText(t *testing.T) {
	const want = "The legacy Runtime Broker is drained and the flat instance is registered. " +
		"This host runs the Hub and its Runtime Broker in the same server process, so stopping the server takes the whole Hub down: " +
		"every user, Runtime Broker and agent connected to this Hub loses it until the server is started again. " +
		"Plan a maintenance window, then stop the server ('scion server stop') and run 'scion runtime-broker convert --apply' again."
	require.Equal(t, want, NoteColocatedAwaitingStop)

	cf := newColocatedFixture(t)
	before := settingsBytes(t, cf.g)
	rep, err := cf.applyC()
	require.NoError(t, err)
	assert.Equal(t, JournalAwaitingStop, rep.State)
	assert.Contains(t, rep.Notes, NoteColocatedAwaitingStop)
	var h bytes.Buffer
	require.NoError(t, WriteReportHuman(&h, rep))
	assert.Contains(t, h.String(), "takes the whole Hub down")
	assert.Equal(t, before, settingsBytes(t, cf.g), "the running phase writes no settings")
	j := cf.journal()
	assert.Nil(t, j.Rollback)
	require.NotNil(t, j.StopRecheckAt)
	assert.True(t, j.StopRecheckAt.Equal(testNow))
	require.Len(t, j.Register, 1)
	require.NotNil(t, j.Register[0].CredentialsCleanup)
	assert.Equal(t, StepPending, j.Register[0].CredentialsCleanup.State)
	assert.Contains(t, cf.hub.projects["p-1"].providers, applyInstanceID, "step 3 ran")
}

// AC12: the running phase performs steps 1 and 3 and stops at
// awaiting-stop; the stopped phase writes the settings; after the start,
// the cleanup removes the unused credentials file once the instance is
// online, and none remains.
func TestColocatedApply_TwoInvocations(t *testing.T) {
	cf := newColocatedFixture(t)
	key := cf.instanceKey()
	cf.toStopped()

	rep, err := cf.applyC()
	require.NoError(t, err)
	assert.Equal(t, JournalApplied, rep.State)
	assert.Contains(t, rep.Notes, NoteColocatedStart)
	insts, err := cf.env.LoadInstances()
	require.NoError(t, err)
	require.Len(t, insts, 1)
	assert.Equal(t, key, insts[0].Key)
	assert.Equal(t, 1, cf.q7Calls)

	// Still down: start pending, nothing written.
	writes := cf.hub.writeCount()
	rep, err = cf.applyC()
	require.NoError(t, err)
	assert.Contains(t, rep.Notes, NoteColocatedStart)
	assert.Equal(t, writes, cf.hub.writeCount())

	// Up, not yet online: the file is kept.
	cf.serverUp = true
	rep, err = cf.applyC()
	require.NoError(t, err)
	assert.Contains(t, rep.Notes, NoteColocatedCleanupWaiting)
	_, err = os.Lstat(cf.credsFile(key))
	require.NoError(t, err, "kept until the instance is online")

	// Online: removed, recorded, none remains.
	cf.online = true
	rep, err = cf.applyC()
	require.NoError(t, err)
	assert.Contains(t, rep.Notes, NoteColocatedDone)
	assert.Equal(t, []string{key}, cf.removed)
	_, err = os.Lstat(cf.credsFile(key))
	assert.True(t, errors.Is(err, fs.ErrNotExist))
	assert.Equal(t, StepDone, cf.journal().Register[0].CredentialsCleanup.State)

	rep, err = cf.applyC()
	require.NoError(t, err)
	assert.Contains(t, rep.Notes, NoteColocatedDone)
	assert.Equal(t, []string{key}, cf.removed, "no second removal")
}

// Named mutant target: the stopped phase never runs while the server
// answers. With the server still up, a second apply repeats the running
// phase and writes no settings.
func TestColocatedStopped_RefusesWhileServerAnswers(t *testing.T) {
	cf := newColocatedFixture(t)
	before := settingsBytes(t, cf.g)
	_, err := cf.applyC()
	require.NoError(t, err)
	cf.now = cf.now.Add(time.Minute)
	rep, err := cf.applyC()
	require.NoError(t, err)
	assert.Equal(t, JournalAwaitingStop, rep.State)
	assert.Equal(t, before, settingsBytes(t, cf.g), "no settings write while the server answers")
	assert.Zero(t, cf.q7Calls)
	assert.True(t, cf.journal().StopRecheckAt.Equal(cf.now), "the re-check time restarts")
}

// AC13: a non-terminal agent on the legacy Runtime Broker, found by the
// stopped phase's Q7 count, refuses; the settings are not written and the
// journal still awaits the stop.
func TestColocatedStopped_AgentBetweenInvocationsRefuses(t *testing.T) {
	cf := newColocatedFixture(t)
	before := settingsBytes(t, cf.g)
	cf.toStopped()
	cf.q7.LegacyStore = func() (LegacyStoreFacts, error) {
		return LegacyStoreFacts{RowPresent: true, Agents: []AgentEntry{{ID: "a-7", Name: "late", ProjectID: "p-1", Phase: "running"}}}, nil
	}
	_, err := cf.applyC()
	require.Error(t, err)
	assert.Equal(t, RefuseNotDrained, refusalCode(t, err))
	assert.Contains(t, err.Error(), "a-7")
	assert.Equal(t, before, settingsBytes(t, cf.g))
	assert.Equal(t, JournalAwaitingStop, cf.journal().State)
}

// Named mutant target: skip the Q7 replica refusal.
func TestColocatedStopped_RefusesReplicaSetup(t *testing.T) {
	for _, flag := range []bool{false, true} {
		cf := newColocatedFixture(t)
		before := settingsBytes(t, cf.g)
		cf.toStopped()
		probes := 0
		cf.flag = flag
		agents := func() (LegacyStoreFacts, error) {
			probes++
			return LegacyStoreFacts{RowPresent: true}, nil
		}
		endpoint := func() (bool, error) {
			probes++
			return false, nil
		}
		cf.q7 = Q7Input{Topology: TopologyReplica, LegacyStore: agents, EndpointAnswers: endpoint}
		_, err := cf.applyC()
		require.Error(t, err)
		assert.Equal(t, RefuseStoreReplica, refusalCode(t, err), "flag=%v", flag)
		assert.Zero(t, probes)
		assert.Equal(t, before, settingsBytes(t, cf.g))
	}
}

// Named mutant target: accept --single-hub-replica without the
// dark-endpoint probe. With the flag on an external store, an answering
// endpoint refuses; a dark one passes and the assertion is journaled.
func TestColocatedStopped_SingleHubReplicaRefusesWhenEndpointAnswers(t *testing.T) {
	cf := newColocatedFixture(t)
	before := settingsBytes(t, cf.g)
	cf.toStopped()
	cf.flag = true
	probes := 0
	answers := func() (bool, error) {
		probes++
		return true, nil
	}
	cf.q7 = Q7Input{Topology: TopologySingleExternal, EndpointAnswers: answers}
	_, err := cf.applyC()
	require.Error(t, err)
	assert.Equal(t, RefuseHubEndpointAnswers, refusalCode(t, err))
	assert.Equal(t, 1, probes)
	assert.Equal(t, before, settingsBytes(t, cf.g))

	// Control: the endpoint is dark.
	cf.q7.EndpointAnswers = func() (bool, error) {
		probes++
		return false, nil
	}
	rep, err := cf.applyC()
	require.NoError(t, err)
	assert.Equal(t, JournalApplied, rep.State)
	assert.Equal(t, 2, probes)
	assert.True(t, cf.journal().SingleHubReplica, "the assertion is journaled")
}

// AC16 (flag placement): --single-hub-replica is refused outside the
// stopped phase of a co-located apply, before any write.
func TestColocatedApply_FlagRefusedOutsideStoppedPhase(t *testing.T) {
	cf := newColocatedFixture(t)
	cf.flag = true
	_, err := cf.applyC()
	require.Error(t, err)
	assert.Equal(t, RefuseSingleHubMisplaced, refusalCode(t, err))
	assert.Zero(t, cf.hub.writeCount())
	assert.Zero(t, cf.regCalls)

	_, err = RollbackColocated(context.Background(), cf.env, cf.ce())
	require.Error(t, err)
	assert.Equal(t, RefuseSingleHubMisplaced, refusalCode(t, err))
}

// AC14 and sec point 2: the co-located rollback, split around the stop.
// The Hub part runs with the server up; the local part, with the server
// down, removes the unused credentials file before the directory moves, so
// the moved tree holds no credentials file.
func TestColocatedRollback_TwoInvocations(t *testing.T) {
	cf := newColocatedFixture(t)
	key := cf.instanceKey()
	before := settingsBytes(t, cf.g)
	cf.toStopped()
	_, err := cf.applyC()
	require.NoError(t, err)

	cf.serverUp = true
	rep, err := RollbackColocated(context.Background(), cf.env, cf.ce())
	require.NoError(t, err)
	assert.Equal(t, JournalAwaitingStop, rep.State)
	assert.Contains(t, rep.Notes, NoteColocatedRollbackAwaitingStop)
	j := cf.journal()
	require.NotNil(t, j.Rollback)
	assert.Nil(t, cf.hub.instance, "the instance row is deregistered")
	assert.Equal(t, applyLegacyID, cf.hub.projects["p-1"].defaultID)
	assert.NotContains(t, cf.hub.projects["p-1"].providers, applyInstanceID)

	// The apply path refuses while the rollback waits for the stop.
	_, err = cf.applyC()
	require.Error(t, err)
	assert.Equal(t, RefuseRollingBack, refusalCode(t, err))

	cf.serverUp = false
	rep, err = RollbackColocated(context.Background(), cf.env, cf.ce())
	require.NoError(t, err)
	assert.Equal(t, JournalRolledBack, rep.State)
	assert.Contains(t, rep.Notes, NoteColocatedRollbackStart)
	assert.Equal(t, before, settingsBytes(t, cf.g), "the instances key is removed, every other byte as before")
	assert.Equal(t, []string{key}, cf.removed)
	moved := filepath.Join(ConvertDir(cf.g), RolledBackDirName, rep.RunID, key)
	fi, err := os.Lstat(moved)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), fi.Mode().Perm())
	_, err = os.Lstat(filepath.Join(moved, "hub-credentials"))
	assert.True(t, errors.Is(err, fs.ErrNotExist), "no credentials in the moved tree")
	_, err = os.Lstat(filepath.Join(moved, "identity.json"))
	require.NoError(t, err)
}

// The co-located rollback removes each unused credentials file through the
// BeforeMove seam: after the row is deregistered, after any BeforeMove the
// caller set, and right before the move. A failed removal stops the
// rollback before that instance's move; a re-run finishes it.
func TestColocatedRollback_CredentialsRemovedThroughBeforeMove(t *testing.T) {
	cf := newColocatedFixture(t)
	key := cf.instanceKey()
	cf.toStopped()
	_, err := cf.applyC()
	require.NoError(t, err)
	cf.serverUp = true
	_, err = RollbackColocated(context.Background(), cf.env, cf.ce())
	require.NoError(t, err)

	var order []string
	cf.env.BeforeMove = func(_, k string) error {
		order = append(order, "caller:"+k)
		return nil
	}
	failing := cf.ce()
	failing.RemoveCredentials = func(k string) error {
		assert.Nil(t, cf.hub.instance, "the row is deregistered before the removal")
		order = append(order, "remove:"+k)
		return errors.New("planted removal failure")
	}
	cf.serverUp = false
	_, err = RollbackColocated(context.Background(), cf.env, failing)
	require.Error(t, err)
	assert.Equal(t, []string{"caller:" + key, "remove:" + key}, order)
	j := cf.journal()
	assert.NotEqual(t, JournalRolledBack, j.State)
	require.NotNil(t, j.Register[0].CredentialsCleanup)
	assert.Equal(t, StepStarted, j.Register[0].CredentialsCleanup.State)
	assert.NotEmpty(t, j.Register[0].CredentialsCleanup.Error)
	moved := filepath.Join(ConvertDir(cf.g), RolledBackDirName, j.RunID, key)
	_, err = os.Lstat(moved)
	assert.True(t, errors.Is(err, fs.ErrNotExist), "the directory did not move")

	rep, err := RollbackColocated(context.Background(), cf.env, cf.ce())
	require.NoError(t, err)
	assert.Equal(t, JournalRolledBack, rep.State)
	assert.Equal(t, []string{key}, cf.removed)
	assert.Equal(t, StepDone, cf.journal().Register[0].CredentialsCleanup.State)
	_, err = os.Lstat(filepath.Join(moved, "hub-credentials"))
	assert.True(t, errors.Is(err, fs.ErrNotExist), "no credentials in the moved tree")
}

// host_mode_conflict texts carry no raw settings value: a named endpoint
// is shown as its normalized host:port (no userinfo, path or query), and
// local files by their fixed names only, never a full path.
func TestColocatedHostCheck_TextsCarryNoRawValues(t *testing.T) {
	ref := ColocatedHostCheck(HostModeColocated, HostEvidence{
		Source: HostModeAsserted,
		Named:  []string{"https://u:p@hub.example:8443/x?q=1"},
	})
	require.NotNil(t, ref)
	assert.Equal(t, RefuseHostModeConflict, ref.Code)
	assert.Contains(t, ref.Text, "hub.example:8443")
	for _, raw := range []string{"u:p", "@hub", "q=1", "/x"} {
		assert.NotContains(t, ref.Text, raw)
	}

	ref = ColocatedHostCheck(HostModeColocated, HostEvidence{
		Source:            HostModeDetected,
		LegacyCredentials: "/home/someone/.scion/runtime-brokers/docker/hub-credentials/hub.json",
		LaunchFile:        "/home/someone/.scion/server-args.json",
	})
	require.NotNil(t, ref)
	assert.Contains(t, ref.Text, "hub-credentials/")
	assert.Contains(t, ref.Text, "server-args.json")
	assert.NotContains(t, ref.Text, "/home/someone")

	ref = ColocatedHostCheck(HostModeColocated, HostEvidence{
		Source:            HostModeAsserted,
		LegacyCredentials: "/home/someone/.scion/broker-credentials.json",
	})
	require.NotNil(t, ref)
	assert.Contains(t, ref.Text, "broker-credentials.json")
	assert.NotContains(t, ref.Text, "/home/someone")
}

// AC9 and sec point 1: neither the planted settings env value nor the
// planted join secret appears in any co-located output (human or JSON) or
// in the journal.
func TestColocated_NoPlantedValueOrSecret(t *testing.T) {
	cf := newColocatedFixture(t)
	var reps []*Report
	add := func(rep *Report, err error) {
		t.Helper()
		require.NoError(t, err)
		reps = append(reps, rep)
	}
	add(cf.applyC())
	cf.serverUp = false
	cf.now = cf.now.Add(time.Minute)
	add(cf.applyC())
	cf.serverUp = true
	add(cf.applyC())
	add(RollbackColocated(context.Background(), cf.env, cf.ce()))
	cf.serverUp = false
	add(RollbackColocated(context.Background(), cf.env, cf.ce()))

	for _, rep := range reps {
		var h, js bytes.Buffer
		require.NoError(t, WriteReportHuman(&h, rep))
		require.NoError(t, WriteReportJSON(&js, rep))
		for _, planted := range []string{plantedApplyEnv, plantedJoinSecret, "API_TOKEN"} {
			assert.NotContains(t, h.String(), planted, rep.Mode)
			assert.NotContains(t, js.String(), planted, rep.Mode)
		}
	}
	journal, err := os.ReadFile(filepath.Join(ConvertDir(cf.g), JournalFileName))
	require.NoError(t, err)
	for _, planted := range []string{plantedApplyEnv, plantedJoinSecret, "API_TOKEN"} {
		assert.NotContains(t, string(journal), planted)
	}
}

var q7Now = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

func recheckAgo(d time.Duration) *time.Time {
	t := q7Now.Add(-d)
	return &t
}

func q7Code(ref *Refusal) string {
	if ref == nil {
		return ""
	}
	return ref.Code
}

// AC16: each Q7 case. No case reads an external database (there is no such
// probe in Q7Input).
func TestQ7Check_Cases(t *testing.T) {
	drained := drainedStore
	cases := []struct {
		name string
		in   Q7Input
		want string
	}{
		{name: "replica", in: Q7Input{Topology: TopologyReplica}, want: RefuseStoreReplica},
		{name: "replica with flag", in: Q7Input{Topology: TopologyReplica, SingleHubReplica: true}, want: RefuseStoreReplica},
		{name: "undetermined", in: Q7Input{Topology: TopologyUndetermined}, want: RefuseStoreUnknown},
		{name: "empty topology", in: Q7Input{}, want: RefuseStoreUnknown},
		{name: "flag on sqlite", in: Q7Input{Topology: TopologySingleSQLite, SingleHubReplica: true, LegacyStore: drained}, want: RefuseSingleHubMisplaced},
		{name: "external without flag", in: Q7Input{Topology: TopologySingleExternal}, want: RefuseSingleHubRequired},
		{name: "external no probe", in: Q7Input{Topology: TopologySingleExternal, SingleHubReplica: true}, want: RefuseHubEndpointUnprobed},
		{name: "external probe error", in: Q7Input{Topology: TopologySingleExternal, SingleHubReplica: true, EndpointAnswers: func() (bool, error) { return false, errors.New("x") }}, want: RefuseHubEndpointUnprobed},
		{name: "external answers", in: Q7Input{Topology: TopologySingleExternal, SingleHubReplica: true, EndpointAnswers: func() (bool, error) { return true, nil }}, want: RefuseHubEndpointAnswers},
		{name: "external dark", in: Q7Input{Topology: TopologySingleExternal, SingleHubReplica: true, EndpointAnswers: func() (bool, error) { return false, nil }}},
		{name: "sqlite unreadable", in: Q7Input{Topology: TopologySingleSQLite, LegacyStore: func() (LegacyStoreFacts, error) { return LegacyStoreFacts{}, errors.New("x") }}, want: RefuseStoreUnreadable},
		{name: "sqlite no reader", in: Q7Input{Topology: TopologySingleSQLite}, want: RefuseStoreUnreadable},
		{name: "sqlite agent", in: Q7Input{Topology: TopologySingleSQLite, LegacyStore: agentStore}, want: RefuseNotDrained},
		{name: "sqlite drained", in: Q7Input{Topology: TopologySingleSQLite, LegacyStore: drained}},
	}
	for _, tc := range cases {
		in := tc.in
		in.Now = q7Now
		in.StopRecheckAt = recheckAgo(time.Minute)
		assert.Equal(t, tc.want, q7Code(Q7Check(in)), tc.name)
	}
}

// The time bound only refuses: an old, missing, zero or future re-check
// refuses, and a fresh one never passes an unreadable store.
func TestQ7Check_TimeBoundOnlyRefuses(t *testing.T) {
	drained := drainedStore
	future := q7Now.Add(time.Minute)
	for name, tc := range map[string]struct {
		at   *time.Time
		want string
	}{
		"expired": {at: recheckAgo(StopRecheckBound + time.Second), want: RefuseStopRecheckExpired},
		"missing": {at: nil, want: RefuseStopRecheckMissing},
		"zero":    {at: &time.Time{}, want: RefuseStopRecheckMissing},
		"future":  {at: &future, want: RefuseStopRecheckExpired},
		"at edge": {at: recheckAgo(StopRecheckBound), want: ""},
	} {
		got := Q7Check(Q7Input{Topology: TopologySingleSQLite, StopRecheckAt: tc.at, Now: q7Now, LegacyStore: drained})
		assert.Equal(t, tc.want, q7Code(got), name)
	}
	got := Q7Check(Q7Input{Topology: TopologySingleSQLite, StopRecheckAt: recheckAgo(0), Now: q7Now,
		LegacyStore: func() (LegacyStoreFacts, error) { return LegacyStoreFacts{}, errors.New("x") }})
	assert.Equal(t, RefuseStoreUnreadable, q7Code(got))
}

func TestNextColocatedPhases(t *testing.T) {
	pending := &ItemStatus{State: StepPending}
	cases := []struct {
		name     string
		j        *Journal
		serverUp bool
		want     ColocatedPhase
		wantCode string
	}{
		{name: "fresh up", serverUp: true, want: ColocatedRunning},
		{name: "fresh down", wantCode: RefuseColocatedServerDown},
		{name: "applying up", j: &Journal{State: JournalApplying}, serverUp: true, want: ColocatedRunning},
		{name: "awaiting down", j: &Journal{State: JournalAwaitingStop}, want: ColocatedStopped},
		{name: "awaiting up repeats running", j: &Journal{State: JournalAwaitingStop}, serverUp: true, want: ColocatedRunning},
		{name: "applied down", j: &Journal{State: JournalApplied}, want: ColocatedStartPending},
		{name: "applied up cleanup", j: &Journal{State: JournalApplied, Register: []RegisterItem{{CredentialsCleanup: pending}}}, serverUp: true, want: ColocatedCleanup},
		{name: "applied up remote item", j: &Journal{State: JournalApplied, Register: []RegisterItem{{}}}, serverUp: true, want: ColocatedDone},
		{name: "rollback waiting", j: &Journal{State: JournalAwaitingStop, Rollback: &RollbackRecord{}}, wantCode: RefuseRollingBack},
		{name: "rolled back starts fresh", j: &Journal{State: JournalRolledBack}, serverUp: true, want: ColocatedRunning},
	}
	for _, tc := range cases {
		got, ref := NextColocatedApplyPhase(tc.j, tc.serverUp)
		assert.Equal(t, tc.wantCode, q7Code(ref), tc.name)
		assert.Equal(t, tc.want, got, tc.name)
	}

	rb := &Journal{State: JournalAwaitingStop, Rollback: &RollbackRecord{}}
	got, ref := NextColocatedRollbackPhase(rb, false)
	assert.Nil(t, ref)
	assert.Equal(t, ColocatedStopped, got)
	got, ref = NextColocatedRollbackPhase(rb, true)
	assert.Nil(t, ref)
	assert.Equal(t, ColocatedRunning, got, "with the server up the Hub part repeats")
	_, ref = NextColocatedRollbackPhase(&Journal{State: JournalApplied}, false)
	assert.Equal(t, RefuseColocatedServerDown, q7Code(ref))
	_, ref = NextColocatedRollbackPhase(nil, true)
	assert.Equal(t, RefuseNoJournal, q7Code(ref))
}

func hostModeCode(ref *Refusal) string { return q7Code(ref) }

// The host-mode cases: detection unknown plus each flag value proceeds as
// asserted; a determined mode with no flag, or an agreeing flag, is the
// detected mode; nothing known refuses.
func TestResolveHostMode_Cases(t *testing.T) {
	cases := []struct {
		name, detected, asserted string
		j                        *Journal
		mode, source, code       string
	}{
		{name: "unknown, flag colocated", asserted: HostModeColocated, mode: HostModeColocated, source: HostModeAsserted},
		{name: "unknown, flag remote", asserted: HostModeRemote, mode: HostModeRemote, source: HostModeAsserted},
		{name: "unknown, no flag", code: RefuseColocatedUnknown},
		{name: "detected, no flag", detected: HostModeRemote, mode: HostModeRemote, source: HostModeDetected},
		{name: "detected, agreeing flag is a no-op", detected: HostModeColocated, asserted: HostModeColocated, mode: HostModeColocated, source: HostModeDetected},
		{name: "old journal falls back to detection", detected: HostModeColocated, j: &Journal{State: JournalAwaitingStop}, mode: HostModeColocated, source: HostModeDetected},
		{name: "old journal, unknown, no flag", j: &Journal{State: JournalApplying}, code: RefuseColocatedUnknown},
		{name: "journal binds the mode", j: &Journal{State: JournalAwaitingStop, HostMode: HostModeColocated, HostModeSource: HostModeAsserted}, mode: HostModeColocated, source: HostModeAsserted},
		{name: "rolled-back journal is ignored", asserted: HostModeRemote, j: &Journal{State: JournalRolledBack, HostMode: HostModeColocated}, mode: HostModeRemote, source: HostModeAsserted},
	}
	for _, tc := range cases {
		mode, source, ref := ResolveHostMode(tc.detected, tc.asserted, tc.j)
		assert.Equal(t, tc.code, hostModeCode(ref), tc.name)
		assert.Equal(t, tc.mode, mode, tc.name)
		assert.Equal(t, tc.source, source, tc.name)
	}
}

// Named mutant target: drop the conflict check. A saved launch and a
// conflicting flag refuse, naming both values.
func TestResolveHostMode_ConflictRefuses(t *testing.T) {
	_, _, ref := ResolveHostMode(HostModeColocated, HostModeRemote, nil)
	require.NotNil(t, ref)
	assert.Equal(t, RefuseHostModeConflict, ref.Code)
	assert.Contains(t, ref.Error(), "detected colocated")
	assert.Contains(t, ref.Error(), "--host-mode remote")
}

// Named mutant target: drop the journal-vs-invocation check. A journal that
// records colocated refuses a later --host-mode remote, and a saved launch
// that has appeared since and disagrees.
func TestResolveHostMode_JournalBindsMode(t *testing.T) {
	j := &Journal{State: JournalAwaitingStop, HostMode: HostModeColocated, HostModeSource: HostModeAsserted}
	_, _, ref := ResolveHostMode("", HostModeRemote, j)
	require.NotNil(t, ref)
	assert.Equal(t, RefuseHostModeConflict, ref.Code)
	assert.Contains(t, ref.Error(), "journal colocated")
	assert.Contains(t, ref.Error(), "--host-mode remote")

	_, _, ref = ResolveHostMode(HostModeRemote, "", j)
	require.NotNil(t, ref)
	assert.Equal(t, RefuseHostModeConflict, ref.Code)
}

// The first apply journals the host mode and its source, on the co-located
// path and on PR1's remote path.
func TestApply_RecordsHostMode(t *testing.T) {
	cf := newColocatedFixture(t)
	_, in := cf.plan()
	in.HostMode, in.HostModeSource = HostModeColocated, HostModeAsserted
	_, err := ApplyColocated(context.Background(), cf.env, cf.ce(), in)
	require.NoError(t, err)
	j := cf.journal()
	assert.Equal(t, HostModeColocated, j.HostMode)
	assert.Equal(t, HostModeAsserted, j.HostModeSource)

	f := newApplyFixture(t)
	_, in = f.plan()
	in.HostMode, in.HostModeSource = HostModeRemote, HostModeDetected
	_, err = Apply(context.Background(), f.env, in)
	require.NoError(t, err)
	j, err = LoadJournal(f.g)
	require.NoError(t, err)
	assert.Equal(t, HostModeRemote, j.HostMode)
	assert.Equal(t, HostModeDetected, j.HostModeSource)
}

// Named mutant target (8): drop the store-identity check. A local store
// without this host's legacy Runtime Broker row (a stale hub.db on a host
// that is now remote) refuses, even with no agents; the stopped phase then
// writes no settings and touches no file.
func TestQ7Check_StoreWithoutLegacyRowRefuses(t *testing.T) {
	ref := Q7Check(Q7Input{Topology: TopologySingleSQLite, StopRecheckAt: recheckAgo(time.Minute), Now: q7Now,
		LegacyStore: func() (LegacyStoreFacts, error) { return LegacyStoreFacts{RowPresent: false}, nil }})
	assert.Equal(t, RefuseStoreUnreadable, q7Code(ref))

	cf := newColocatedFixture(t)
	key := cf.instanceKey()
	before := settingsBytes(t, cf.g)
	cf.toStopped()
	cf.q7.LegacyStore = func() (LegacyStoreFacts, error) { return LegacyStoreFacts{RowPresent: false}, nil }
	_, err := cf.applyC()
	require.Error(t, err)
	assert.Equal(t, RefuseStoreUnreadable, refusalCode(t, err))
	assert.Equal(t, before, settingsBytes(t, cf.g), "no settings write")
	_, err = os.Lstat(cf.credsFile(key))
	require.NoError(t, err, "no credentials removal")
}

// Named mutant target (9): drop the endpoint check. A co-located mode whose
// host evidence points at a remote Hub refuses: on the DETECTED path (a
// stale server launch), on the ASSERTED path, and when the journal binds the
// mode. The fix text follows the source.
func TestColocatedHostCheck_RemoteEvidenceRefuses(t *testing.T) {
	remote := "https://hub.example.org"
	listeners := []string{"http://127.0.0.1:8080", "http://127.0.0.1:9810"}

	ref := ColocatedHostCheck(HostModeColocated, HostEvidence{Source: HostModeDetected, Named: []string{remote}, Listeners: listeners})
	require.NotNil(t, ref)
	assert.Equal(t, RefuseHostModeConflict, ref.Code)
	assert.Contains(t, ref.Error(), "hub.example.org")
	assert.Contains(t, ref.Error(), "remove the stale saved server launch file")
	assert.NotContains(t, ref.Error(), "--host-mode", "a detected mode is never overridden by the flag")

	ref = ColocatedHostCheck(HostModeColocated, HostEvidence{Source: HostModeAsserted, Named: []string{remote}})
	require.NotNil(t, ref)
	assert.Contains(t, ref.Error(), "--host-mode remote")

	ref = ColocatedHostCheck(HostModeColocated, HostEvidence{Source: HostModeJournal, Named: []string{remote}})
	require.NotNil(t, ref)
	assert.Contains(t, ref.Error(), "roll back")

	// The presence test: a saved legacy broker credentials file refuses on
	// its own, detected or asserted.
	for _, src := range []string{HostModeDetected, HostModeAsserted} {
		ref = ColocatedHostCheck(HostModeColocated, HostEvidence{Source: src, LegacyCredentials: "broker-credentials.json", LaunchFile: "server-args.json", Listeners: listeners})
		require.NotNil(t, ref, src)
		assert.Contains(t, ref.Error(), "broker-credentials.json", src)
	}
	// On a DETECTED mode the presence refusal names BOTH files and says which
	// is stale when this host serves its own Hub; it offers no --host-mode.
	ref = ColocatedHostCheck(HostModeColocated, HostEvidence{Source: HostModeDetected, LegacyCredentials: "broker-credentials.json", LaunchFile: "server-args.json"})
	require.NotNil(t, ref)
	assert.Contains(t, ref.Error(), "server-args.json")
	assert.Contains(t, ref.Error(), "broker-credentials.json")
	assert.Contains(t, ref.Error(), "the credentials file is the stale one")
	assert.NotContains(t, ref.Error(), "--host-mode")
}

// The asserted loopback rule: without a saved launch, a loopback endpoint
// (for example a tunnel to a remote Hub) counts as local only when it equals
// the configured server.hub.endpoint.
func TestColocatedHostCheck_AssertedLoopbackNeedsConfiguredEndpoint(t *testing.T) {
	ref := ColocatedHostCheck(HostModeColocated, HostEvidence{Source: HostModeAsserted, Named: []string{"http://localhost:8080"}})
	require.NotNil(t, ref, "a loopback endpoint with no configured endpoint refuses")
	assert.Equal(t, RefuseHostModeConflict, ref.Code)

	assert.Nil(t, ColocatedHostCheck(HostModeColocated, HostEvidence{Source: HostModeAsserted,
		Named: []string{"http://localhost:8080"}, Configured: "http://127.0.0.1:8080"}), "equal to the configured endpoint (loopback alias)")
}

// Local aliases pass, detected and asserted: the configured endpoint by DNS
// name, and (detected) the saved launch's listeners by any loopback name.
func TestColocatedHostCheck_LocalAliasesPass(t *testing.T) {
	listeners := []string{"http://0.0.0.0:8080"}
	for _, src := range []string{HostModeDetected, HostModeAsserted} {
		assert.Nil(t, ColocatedHostCheck(HostModeColocated, HostEvidence{Source: src,
			Named: []string{"https://hub.corp.example"}, Configured: "https://hub.corp.example:443", Listeners: listeners}), src)
	}
	assert.Nil(t, ColocatedHostCheck(HostModeColocated, HostEvidence{Source: HostModeDetected,
		Named: []string{"http://localhost:8080"}, Listeners: listeners}), "a detected launch's listener")
	assert.Nil(t, ColocatedHostCheck(HostModeRemote, HostEvidence{Source: HostModeAsserted, Named: []string{"https://hub.example.org"},
		LegacyCredentials: "broker-credentials.json"}), "a remote mode is not checked")
	assert.NotNil(t, ColocatedHostCheck(HostModeColocated, HostEvidence{Source: HostModeDetected, Named: []string{"::not a url"}}), "an unparsable endpoint refuses")
}
