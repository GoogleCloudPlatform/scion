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

//go:build !no_sqlite

// Design wave 3 phase D3a (ptone/scion#4669), store-backed: the hub reads
// start-kind launches and resume memory correctly once they exist. The
// rows are made with the store's own start-launch API (phase D1); nothing
// in the hub begins a start launch yet.
package hub

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// beginTestStartLaunch moves a's row to starting (the phase a start
// dispatch writes) when kind is start, and begins a start-kind launch
// recording prior as its prior phase.
func beginTestStartLaunch(t *testing.T, s store.Store, a *store.Agent, kind, prior string, revokeOnFail bool) string {
	t.Helper()
	ctx := context.Background()
	if kind == store.LaunchKindStart {
		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Phase: string(state.PhaseStarting)}))
	}
	launchID, err := s.BeginStartLaunch(ctx, a.ID, kind, 5*time.Minute, store.BeginLaunchOptions{PriorPhase: prior, RevokeOnFail: revokeOnFail})
	require.NoError(t, err)
	return launchID
}

// seedResumeMemory turns a (in phase prior) into a resume-memory row: a
// start launch from prior, ended by a failed broker report. The row ends in
// error and remembers prior (store.EffectiveRestingPhase).
func seedResumeMemory(t *testing.T, s store.Store, a *store.Agent, prior string) *store.Agent {
	t.Helper()
	launchID := beginTestStartLaunch(t, s, a, store.LaunchKindStart, prior, false)
	answer, _, err := s.ApplyLaunchReport(context.Background(), a.ID, a.RuntimeBrokerID, store.LaunchReport{
		LaunchID: launchID, InstanceID: "i1", State: store.LaunchReportStateFailed, ErrorCode: "boom",
	})
	require.NoError(t, err)
	require.Equal(t, store.LaunchReportResultApplied, answer.Result)
	row := getAgent(t, s, a.ID)
	require.Equal(t, string(state.PhaseError), row.Phase)
	require.Equal(t, prior, row.LaunchResumePhase)
	require.Equal(t, prior, store.EffectiveRestingPhase(row))
	return row
}

// memStartDispatcher records each start's resume flag and fails it when
// fail is set.
type memStartDispatcher struct {
	createAgentDispatcher
	mu      sync.Mutex
	resumes []bool
	fail    error
}

func (d *memStartDispatcher) DispatchAgentStart(_ context.Context, _ *store.Agent, _ string, resume bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.resumes = append(d.resumes, resume)
	return d.fail
}

func (d *memStartDispatcher) starts() []bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]bool(nil), d.resumes...)
}

// newMemoryFixture is newClaimFixture (start claims on) with a recording
// dispatcher and the fixture broker as the project's default.
func newMemoryFixture(t *testing.T) (*reconcileFixture, *memStartDispatcher) {
	t.Helper()
	f, _, _ := newClaimFixture(t)
	d := &memStartDispatcher{}
	f.srv.SetDispatcher(d)
	ctx := context.Background()
	p, err := f.s.GetProject(ctx, f.projectID)
	require.NoError(t, err)
	p.DefaultRuntimeBrokerID = f.brokerID
	require.NoError(t, f.s.UpdateProject(ctx, p))
	return f, d
}

// TestAsyncStartLaunchReport_RevokeByKind pins the kind-aware revoke of an
// applied failed report end to end: a start launch revokes start_failed
// only when the hub decided so, a restart launch never revokes.
func TestAsyncStartLaunchReport_RevokeByKind(t *testing.T) {
	cases := []struct {
		name         string
		kind         string
		phase        state.Phase
		prior        string
		revokeOnFail bool
		wantReason   string
	}{
		{"start revokes when decided", store.LaunchKindStart, state.PhaseSuspended, "suspended", true, agentCredentialRevokeReasonStartFailed},
		{"start without the decision keeps it", store.LaunchKindStart, state.PhaseSuspended, "suspended", false, ""},
		{"restart never revokes", store.LaunchKindRestart, state.PhaseStopped, "stopped", true, ""},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, s := testServer(t)
			ctx := context.Background()
			suffix := fmt.Sprintf("asr-%d", i)
			project := &store.Project{ID: tid(suffix + "-project"), Slug: suffix + "-project", Name: suffix, Created: time.Now(), Updated: time.Now()}
			require.NoError(t, s.CreateProject(ctx, project))
			a := &store.Agent{
				ID: tid(suffix + "-agent"), Slug: suffix + "-agent", Name: suffix, ProjectID: project.ID,
				Phase: string(c.phase), RuntimeBrokerID: "broker-1", StateVersion: 1,
				Created: time.Now(), Updated: time.Now(),
			}
			require.NoError(t, s.CreateAgent(ctx, a))
			launchID := beginTestStartLaunch(t, s, a, c.kind, c.prior, c.revokeOnFail)
			jti := suffix + "-jti"
			insertTestAgentCredential(t, s, a.ID, project.ID, jti)

			rec := postLaunchReport(t, srv, "broker-1", a.ID, "broker-1", AgentLaunchReport{
				LaunchID: launchID, InstanceID: "i1", State: "failed", ErrorCode: "boom",
			})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), store.LaunchReportResultApplied)

			cred := getTestAgentCredential(t, s, jti)
			if c.wantReason == "" {
				assert.Nil(t, cred.RevokedAt, "no revoke expected")
				return
			}
			require.NotNil(t, cred.RevokedAt)
			require.NotNil(t, cred.RevokeReason)
			assert.Equal(t, c.wantReason, *cred.RevokeReason)
		})
	}
}

// TestAsyncStartReaper_SettlesEndedStartLaunchClaims pins the reaper's
// kinds: a user claim linked to an ended start launch, and a restart claim
// linked to an ended restart launch, are settled by the launch's end, as a
// create claim is. Here the launches end through a running write that does
// not settle the claim (running_observed).
func TestAsyncStartReaper_SettlesEndedStartLaunchClaims(t *testing.T) {
	cases := []struct {
		name  string
		phase string
		claim store.StartClaimKind
		kind  string
	}{
		{"start launch, user claim", "suspended", store.StartClaimUser, store.LaunchKindStart},
		{"restart launch, restart claim", "stopped", store.StartClaimRestart, store.LaunchKindRestart},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, _, _ := newClaimFixture(t)
			ctx := context.Background()
			a := f.addAgent(fmt.Sprintf("reap-kind-%d", i), c.phase, "")
			claim, err := f.s.ClaimAgentStart(ctx, a.ID, "hub-other", c.claim, "docker", time.Minute)
			require.NoError(t, err)
			launchID := beginTestStartLaunch(t, f.s, a, c.kind, c.phase, false)
			row := getAgent(t, f.s, a.ID)
			require.Equal(t, launchID, row.StartClaimLaunchID, "BeginStartLaunch links the paired claim")

			require.NoError(t, f.s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Phase: string(state.PhaseRunning)}))
			row = getAgent(t, f.s, a.ID)
			require.Equal(t, store.LaunchStateEnded, row.LaunchState)
			require.Equal(t, claim.ID, row.StartClaimID, "the running write leaves the claim to the reaper")

			f.srv.reapStartClaims(ctx)
			assert.Empty(t, getAgent(t, f.s, a.ID).StartClaimID, "the reaper settles the ended launch's claim")
		})
	}
}

// TestAsyncStartGuard0f_RunlessSelfReportDuringStartLaunch pins Guard 0f
// through the status endpoint: during an active start launch, the agent's
// own report with a run-less token cannot move the row to stopped (which
// would end the launch as stopped during launch). Without a start launch
// the same report applies as before.
func TestAsyncStartGuard0f_RunlessSelfReportDuringStartLaunch(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	project := &store.Project{ID: tid("g0f-project"), Slug: "g0f-project", Name: "G0f", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, project))
	mk := func(slug, phase string) *store.Agent {
		a := &store.Agent{
			ID: tid(slug), Slug: slug, Name: slug, ProjectID: project.ID,
			Phase: phase, RuntimeBrokerID: "broker-1", RunID: "run-new", StateVersion: 1,
			Created: time.Now(), Updated: time.Now(),
		}
		require.NoError(t, s.CreateAgent(ctx, a))
		return a
	}
	const report = `{"phase":"stopped","activity":"completed","message":"Agent shutting down"}`

	launching := mk("g0f-launching", string(state.PhaseSuspended))
	beginTestStartLaunch(t, s, launching, store.LaunchKindStart, "suspended", false)
	rec := postAgentStatusAsAgent(t, srv, launching, report)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := getAgent(t, s, launching.ID)
	assert.Equal(t, string(state.PhaseStarting), got.Phase, "an old container's report does not end the start launch")
	assert.Equal(t, store.LaunchStateActive, got.LaunchState)

	plain := mk("g0f-plain", string(state.PhaseStarting))
	rec = postAgentStatusAsAgent(t, srv, plain, report)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, string(state.PhaseStopped), getAgent(t, s, plain.ID).Phase, "no start launch: the report applies as before")
}

// TestAsyncStartMemory_CreateEndpointBranches pins the create endpoint's
// branch selection for memory rows (design D-T12(a)): a suspended memory
// resumes its session through Branch 1, a created memory starts through
// Branch 4, both without a 409; a plain error row still answers 409.
func TestAsyncStartMemory_CreateEndpointBranches(t *testing.T) {
	f, d := newMemoryFixture(t)

	susp := seedResumeMemory(t, f.s, f.addAgent("mem-susp", "suspended", ""), "suspended")
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{Name: susp.Slug, ProjectID: f.projectID})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, []bool{true}, d.starts(), "a suspended memory resumes its session even on a plain start")
	got := getAgent(t, f.s, susp.ID)
	assert.Equal(t, string(state.PhaseRunning), got.Phase)
	assert.Empty(t, got.LaunchResumePhase, "the running write ends the memory")

	created := seedResumeMemory(t, f.s, f.addAgent("mem-created", "created", ""), "created")
	rec = doRequest(t, f.srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{Name: created.Slug, ProjectID: f.projectID})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, []bool{true, false}, d.starts(), "a created memory starts fresh (Branch 4)")
	assert.Equal(t, string(state.PhaseRunning), getAgent(t, f.s, created.ID).Phase)

	plain := f.addAgent("mem-plain-error", "error", "")
	rec = doRequest(t, f.srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{Name: plain.Slug, ProjectID: f.projectID})
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Len(t, d.starts(), 2, "no start for a plain error row")
}

// TestAsyncStartMemory_LifecycleStartResumes pins lifecycle start's resume
// flag for memory rows (design D-T12(b)): a suspended memory resumes, a
// plain error row does not.
func TestAsyncStartMemory_LifecycleStartResumes(t *testing.T) {
	f, d := newMemoryFixture(t)
	susp := seedResumeMemory(t, f.s, f.addAgent("lc-mem-susp", "suspended", ""), "suspended")
	code, body := lifecycle(t, f, susp.ID, "start")
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, []bool{true}, d.starts())

	plain := f.addAgent("lc-mem-plain", "error", "")
	code, body = lifecycle(t, f, plain.ID, "start")
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, []bool{true, false}, d.starts(), "a plain error row starts fresh")
}

// TestAsyncStartMemory_WakeDM pins wake-DM for memory rows (design
// D-T12(d)): a suspended memory takes the suspended branch and reaches the
// resume dispatch (failed here, so the wait for readiness is skipped); a
// created memory is refused as not yet running, naming the remembered
// phase, with no dispatch.
func TestAsyncStartMemory_WakeDM(t *testing.T) {
	f, d := newMemoryFixture(t)
	d.fail = fmt.Errorf("x: %w", errStartRequestNotSent)

	susp := seedResumeMemory(t, f.s, f.addAgent("wake-mem-susp", "suspended", ""), "suspended")
	_, dmErr := f.srv.wakeAgentForDM(context.Background(), susp)
	require.NotNil(t, dmErr)
	assert.Equal(t, []bool{true}, d.starts(), "the suspended memory is woken with a resume")

	created := seedResumeMemory(t, f.s, f.addAgent("wake-mem-created", "created", ""), "created")
	_, dmErr = f.srv.wakeAgentForDM(context.Background(), created)
	require.NotNil(t, dmErr)
	assert.Contains(t, dmErr.Message, "not yet running (phase: created)")
	assert.Len(t, d.starts(), 1, "no dispatch for a created memory")
}

// TestAsyncStartMemory_SyncFailedStartOfCreatedMemoryRests pins design N2
// (D-T12(h)): a synchronous start of a created memory row that does not
// happen returns it to rest (run intent stopped, the provisioned note), as
// for a created row before the failed attempt, and keeps the memory.
func TestAsyncStartMemory_SyncFailedStartOfCreatedMemoryRests(t *testing.T) {
	f, d := newMemoryFixture(t)
	a := f.addAgent("n2-created", "created", "")
	_, err := f.s.SetRunIntent(context.Background(), a.ID, store.RunIntentStopped)
	require.NoError(t, err)
	seedResumeMemory(t, f.s, a, "created")
	d.fail = fmt.Errorf("x: %w", errStartRequestNotSent)

	code, _ := lifecycle(t, f, a.ID, "start")
	require.NotEqual(t, http.StatusOK, code)
	got := getAgent(t, f.s, a.ID)
	assert.Equal(t, store.RunIntentStopped, got.RunIntent, "back at rest")
	assert.True(t, strings.HasPrefix(got.Message, "Start failed:"), got.Message)
	assert.Contains(t, got.Message, provisionedRestingNote)
	assert.Equal(t, string(state.PhaseError), got.Phase, "the rollback restores the stored phase")
	assert.Equal(t, "created", got.LaunchResumePhase, "a failed synchronous retry keeps the memory")
}

// TestAsyncStartMemory_BeginStartDispatchRecordsRestingPhase pins
// sd.priorRestingPhase: the effective resting phase is recorded next to the
// stored phase, which rollback keeps restoring.
func TestAsyncStartMemory_BeginStartDispatchRecordsRestingPhase(t *testing.T) {
	f, _ := newMemoryFixture(t)
	ctx := context.Background()
	row := seedResumeMemory(t, f.s, f.addAgent("sd-mem", "suspended", ""), "suspended")
	sd, err := f.srv.beginStartDispatch(ctx, row)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseError), sd.priorPhase)
	assert.Equal(t, "suspended", sd.restingPhase())
	sd.rollback(ctx)
	assert.Equal(t, string(state.PhaseError), getAgent(t, f.s, row.ID).Phase, "rollback restores the stored phase")

	plain := f.addAgent("sd-plain", "stopped", "")
	sd, err = f.srv.beginStartDispatch(ctx, plain)
	require.NoError(t, err)
	assert.Equal(t, "stopped", sd.restingPhase())
	sd.rollback(ctx)
	assert.Equal(t, "", (*startDispatch)(nil).restingPhase())
}

// TestAsyncStartExecDispatchStart_PassesAsync pins the owner side of the
// cross-node start: the intent's Async reaches a dispatcher that takes
// options, and its accepted launch is recorded in the row's result; a
// dispatcher without options gets the plain start.
func TestAsyncStartExecDispatchStart_PassesAsync(t *testing.T) {
	f, _, a := newClaimFixture(t)
	ctx := context.Background()
	args, err := MarshalDispatchArgs(&StartDispatchArgs{Async: true})
	require.NoError(t, err)
	row := store.BrokerDispatch{ID: "d-async", BrokerID: f.brokerID, AgentID: a.ID, ProjectID: f.projectID, Op: "start", Args: args}

	od := &optionsProbeDispatcher{result: &StartDispatchResult{Launch: &LaunchAccepted{ID: "L1"}}}
	f.srv.SetDispatcher(od)
	res, err := f.srv.execDispatchStart(ctx, row)
	require.NoError(t, err)
	assert.True(t, od.lastOpts.Async, "the intent's Async reaches the dispatcher")
	launch := decodeLifecycleResult(res).Launch
	require.NotNil(t, launch)
	assert.Equal(t, "L1", launch.ID)

	pd := &plainStartDispatcher{}
	f.srv.SetDispatcher(pd)
	res, err = f.srv.execDispatchStart(ctx, row)
	require.NoError(t, err)
	assert.Equal(t, 1, pd.starts)
	assert.Nil(t, decodeLifecycleResult(res).Launch)
}

// TestAsyncStartBrokerDraining_LifecycleStartReleasesClaim pins Optional-2
// end to end: a lifecycle start answered 503 broker_draining is relayed as
// a retryable 503, and the start claim is released, so the next start is
// not blocked by an unconfirmed claim.
func TestAsyncStartBrokerDraining_LifecycleStartReleasesClaim(t *testing.T) {
	f, d, a := newClaimFixture(t)
	d.start = func(context.Context, *store.Agent) error { return drainingErr("5") }

	code, body := lifecycle(t, f, a.ID, "start")
	require.Equal(t, http.StatusServiceUnavailable, code, body)
	gotCode, _ := errorDetails(body)
	assert.Equal(t, api.BrokerErrCodeBrokerDraining, gotCode)
	assert.Empty(t, getAgent(t, f.s, a.ID).StartClaimID, "the claim is released, not kept unconfirmed")

	_, err := f.s.ClaimAgentStart(context.Background(), a.ID, "hub-next", store.StartClaimUser, "docker", time.Minute)
	assert.NoError(t, err, "a retry can claim the start")
}
