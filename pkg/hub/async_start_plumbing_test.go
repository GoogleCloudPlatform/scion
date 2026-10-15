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

// Design wave 3 phase D3a (ptone/scion#4669): hub plumbing for
// asynchronous start that changes no behaviour until the async branch
// ships. These tests need no store.
package hub

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCapabilitiesFromStrings_AsyncStart covers both wire spellings of the
// async_start broker capability and that it stays false when not reported.
func TestCapabilitiesFromStrings_AsyncStart(t *testing.T) {
	for _, name := range []string{"asyncstart", "ASYNCSTART", "async_start", " async_start "} {
		assert.True(t, capabilitiesFromStrings([]string{name}).AsyncStart, "capability %q", name)
	}
	caps := capabilitiesFromStrings([]string{"async_launch"})
	assert.False(t, caps.AsyncStart, "async_launch alone is not async_start")
	assert.True(t, caps.AsyncLaunch)
}

// TestLaunchFailureRevokeReason pins the revoke an applied failed launch
// report triggers, by launch kind (design D-c, L6).
func TestLaunchFailureRevokeReason(t *testing.T) {
	cases := []struct {
		name   string
		agent  store.Agent
		reason string
	}{
		{"create", store.Agent{LaunchKind: store.LaunchKindCreate}, agentCredentialRevokeReasonCreateFailed},
		{"no kind (older row)", store.Agent{}, agentCredentialRevokeReasonCreateFailed},
		{"create ignores revokeOnFail", store.Agent{LaunchKind: store.LaunchKindCreate, LaunchRevokeOnFail: false}, agentCredentialRevokeReasonCreateFailed},
		{"start with revoke decided", store.Agent{LaunchKind: store.LaunchKindStart, LaunchRevokeOnFail: true}, agentCredentialRevokeReasonStartFailed},
		{"start without", store.Agent{LaunchKind: store.LaunchKindStart}, ""},
		{"restart never", store.Agent{LaunchKind: store.LaunchKindRestart, LaunchRevokeOnFail: true}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := c.agent
			assert.Equal(t, c.reason, launchFailureRevokeReason(&a))
		})
	}
}

// TestClaimPairsWithEndedLaunch pins the reaper's pairing of a launch kind
// with the claim kind it settles (the store's launchClaimKind).
func TestClaimPairsWithEndedLaunch(t *testing.T) {
	linked := func(launchKind string, claim store.StartClaimKind) *store.Agent {
		return &store.Agent{LaunchID: "L1", LaunchKind: launchKind, StartClaimLaunchID: "L1", StartClaimKind: claim}
	}
	assert.True(t, claimPairsWithEndedLaunch(linked(store.LaunchKindCreate, store.StartClaimCreate)))
	assert.True(t, claimPairsWithEndedLaunch(linked("", store.StartClaimCreate)), "a row from before launch kinds is a create")
	assert.True(t, claimPairsWithEndedLaunch(linked(store.LaunchKindStart, store.StartClaimUser)))
	assert.True(t, claimPairsWithEndedLaunch(linked(store.LaunchKindRestart, store.StartClaimRestart)))

	assert.False(t, claimPairsWithEndedLaunch(linked(store.LaunchKindStart, store.StartClaimCreate)))
	assert.False(t, claimPairsWithEndedLaunch(linked(store.LaunchKindCreate, store.StartClaimUser)))
	assert.False(t, claimPairsWithEndedLaunch(linked(store.LaunchKindStart, store.StartClaimWake)))
	assert.False(t, claimPairsWithEndedLaunch(linked(store.LaunchKindRestart, store.StartClaimUser)))

	unlinked := linked(store.LaunchKindCreate, store.StartClaimCreate)
	unlinked.StartClaimLaunchID = "L0"
	assert.False(t, claimPairsWithEndedLaunch(unlinked), "a claim linked to another launch")
	unlinked.StartClaimLaunchID = ""
	unlinked.LaunchID = ""
	assert.False(t, claimPairsWithEndedLaunch(unlinked), "no link")
}

// TestGuardStartLaunchSelfReport pins Guard 0f (design D-e, there named
// Guard 0d): during an active start or restart launch, the agent's own
// report from no run or another run loses its phase and status fields; a
// report from the launch's run, or any report outside a start-kind
// launch, is untouched.
func TestGuardStartLaunchSelfReport(t *testing.T) {
	report := func() store.AgentStatusUpdate {
		ec := 0
		return store.AgentStatusUpdate{
			Phase: string(state.PhaseStopped), Activity: "completed", ExitCode: &ec,
			ExitReason: "completed", Message: "Agent shutting down", TaskSummary: "kept",
		}
	}
	launching := func(kind string) *store.Agent {
		return &store.Agent{Phase: string(state.PhaseStarting), RunID: "run-new", LaunchState: store.LaunchStateActive, LaunchKind: kind}
	}
	blanked := func(t *testing.T, su store.AgentStatusUpdate) {
		t.Helper()
		assert.Empty(t, su.Phase)
		assert.Empty(t, su.Activity)
		assert.Nil(t, su.ExitCode)
		assert.Empty(t, su.ExitReason)
		assert.Empty(t, su.Message)
		assert.Equal(t, "kept", su.TaskSummary, "fields the guard does not own still apply")
	}
	for _, kind := range []string{store.LaunchKindStart, store.LaunchKindRestart} {
		for _, run := range []string{"", "run-old"} {
			su := report()
			guardStartLaunchSelfReport(launching(kind), &su, run)
			blanked(t, su)
		}
		su := report()
		guardStartLaunchSelfReport(launching(kind), &su, "run-new")
		assert.Equal(t, report(), su, "a report from the launch's own run applies (%s)", kind)
	}

	// Neutral outside a start-kind launch: a create launch, an ended start
	// launch, no launch.
	for _, a := range []*store.Agent{
		launching(store.LaunchKindCreate),
		{Phase: string(state.PhaseError), LaunchState: store.LaunchStateEnded, LaunchKind: store.LaunchKindStart},
		{Phase: string(state.PhaseRunning)},
	} {
		su := report()
		guardStartLaunchSelfReport(a, &su, "")
		assert.Equal(t, report(), su, "agent %+v", a)
	}
}

// memoryRow is an error row that remembers the phase a failed asynchronous
// start began from (store.EffectiveRestingPhase).
func memoryRow(remembered string) *store.Agent {
	return &store.Agent{Phase: string(state.PhaseError), LaunchResumePhase: remembered, LaunchState: store.LaunchStateEnded}
}

// TestClassifyExistingAgent_ResumeMemory pins that classifyExistingAgent
// picks the branch of the remembered phase (design D-c Readers, D-T12(f)),
// and the stored phase for a row without memory.
func TestClassifyExistingAgent_ResumeMemory(t *testing.T) {
	plain := CreateAgentRequest{}
	resume := CreateAgentRequest{Resume: true}
	gather := CreateAgentRequest{GatherEnv: true}
	cases := []struct {
		name string
		a    *store.Agent
		req  CreateAgentRequest
		want existingAgentBranch
	}{
		{"suspended memory, plain start", memoryRow("suspended"), plain, existingBranchStart},
		{"suspended memory, resume", memoryRow("suspended"), resume, existingBranchStart},
		{"created memory, plain start", memoryRow("created"), plain, existingBranchStart},
		{"provisioning memory, plain start", memoryRow("provisioning"), plain, existingBranchStart},
		{"provisioning memory, gather env", memoryRow("provisioning"), gather, existingBranchRecreate},
		{"halted memory, resume", memoryRow("halted"), resume, existingBranchStart},
		{"halted memory, plain start", memoryRow("halted"), plain, existingBranchNone},
		// No memory: an error row behaves as today (409 without force).
		{"plain error row, plain start", memoryRow(""), plain, existingBranchNone},
		{"plain error row, resume", memoryRow(""), resume, existingBranchNone},
		{"plain error row, force resume", memoryRow(""), CreateAgentRequest{Resume: true, ForceResume: true}, existingBranchStart},
		// Memory is ignored while a launch is active.
		{"active launch keeps the stored phase", &store.Agent{Phase: string(state.PhaseError), LaunchResumePhase: "suspended", LaunchState: store.LaunchStateActive}, plain, existingBranchNone},
		{"suspended row", &store.Agent{Phase: string(state.PhaseSuspended)}, plain, existingBranchStart},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, classifyExistingAgent(c.a, c.req))
		})
	}
}

// TestValidateAgentDeliverable_ResumeMemory pins the no-wake refusal texts
// for a memory row (design D-c Readers, D-T12(j)): a suspended memory gets
// the suspended texts, a created memory the "not yet running" text naming
// the remembered phase, and a plain error row today's text.
func TestValidateAgentDeliverable_ResumeMemory(t *testing.T) {
	a := memoryRow("suspended")
	a.Slug = "mem"
	err := validateAgentDeliverable(a)
	require.NotNil(t, err)
	assert.Contains(t, err.Message, "is suspended; use --wake")

	err = validateGroupMemberDeliverable(a)
	require.NotNil(t, err)
	assert.Contains(t, err.Message, "group messages do not wake agents")

	c := memoryRow("created")
	c.Slug = "mem"
	err = validateAgentDeliverable(c)
	require.NotNil(t, err)
	assert.Contains(t, err.Message, "is not yet running (phase: created)")

	p := memoryRow("")
	p.Slug = "mem"
	err = validateAgentDeliverable(p)
	require.NotNil(t, err)
	assert.Contains(t, err.Message, "is in error state")
}

// TestApplyStartExtras_LaunchFields pins the start launch fields' wire keys
// (the create's) and that zero values send none, which is what every start
// sends until asynchronous start ships.
func TestApplyStartExtras_LaunchFields(t *testing.T) {
	keys := []string{"asyncLaunch", "launchId", "launchTimeoutSeconds", "launchKeepaliveSeconds"}
	empty := map[string]interface{}{}
	applyStartExtras(empty, StartExtras{RunID: "r1"})
	for _, k := range keys {
		_, ok := empty[k]
		assert.False(t, ok, "zero extras must not send %q", k)
	}

	payload := map[string]interface{}{}
	applyStartExtras(payload, StartExtras{AsyncLaunch: true, LaunchID: "L1", LaunchTimeoutSeconds: 290, LaunchKeepaliveSeconds: 15})
	assert.Equal(t, true, payload["asyncLaunch"])
	assert.Equal(t, "L1", payload["launchId"])
	assert.Equal(t, 290, payload["launchTimeoutSeconds"])
	assert.Equal(t, 15, payload["launchKeepaliveSeconds"])
}

// TestStartDispatchArgs_AsyncRoundTrip pins the cross-node start's Async
// wire key, and that an intent without it (an older hub) reads false.
func TestStartDispatchArgs_AsyncRoundTrip(t *testing.T) {
	raw, err := MarshalDispatchArgs(&StartDispatchArgs{Task: "t", Async: true})
	require.NoError(t, err)
	assert.Contains(t, raw, `"async":true`)
	got, err := UnmarshalStartArgs(raw)
	require.NoError(t, err)
	assert.True(t, got.Async)
	assert.Equal(t, "t", got.Task)

	old, err := UnmarshalStartArgs(`{"task":"t","resume":true}`)
	require.NoError(t, err)
	assert.False(t, old.Async)

	raw, err = MarshalDispatchArgs(&StartDispatchArgs{Task: "t"})
	require.NoError(t, err)
	assert.NotContains(t, raw, "async", "a synchronous intent is written as before")
}

// TestLifecycleDispatchResult_Launch pins the owner's launch result: it is
// written only when set, and a done row carrying it ends the requester's
// wait (lifecycleResultEndsWait), as DeleteWon does.
func TestLifecycleDispatchResult_Launch(t *testing.T) {
	assert.Equal(t, "", marshalLifecycleResult(LifecycleDispatchResult{}), "nothing to carry")
	raw := marshalLifecycleResult(LifecycleDispatchResult{Launch: &LaunchAccepted{ID: "L1"}})
	require.NotEmpty(t, raw)
	got := decodeLifecycleResult(raw)
	require.NotNil(t, got.Launch)
	assert.Equal(t, "L1", got.Launch.ID)

	assert.True(t, lifecycleResultEndsWait(got))
	assert.True(t, lifecycleResultEndsWait(LifecycleDispatchResult{DeleteWon: true}))
	assert.False(t, lifecycleResultEndsWait(LifecycleDispatchResult{Warnings: []string{"w"}}))
	assert.False(t, lifecycleResultEndsWait(decodeLifecycleResult("")), "an older owner's empty result")
}

// optionsProbeDispatcher records which start entry point it was called
// through.
type optionsProbeDispatcher struct {
	AgentDispatcher // nil: only the start entry points are called
	plainStarts     int
	optionStarts    int
	lastOpts        StartDispatchOptions
	result          *StartDispatchResult
}

func (d *optionsProbeDispatcher) DispatchAgentStart(_ context.Context, _ *store.Agent, _ string, _ bool) error {
	d.plainStarts++
	return nil
}

func (d *optionsProbeDispatcher) DispatchAgentStartWithOptions(_ context.Context, _ *store.Agent, _ string, _ bool, opts StartDispatchOptions) (*StartDispatchResult, error) {
	d.optionStarts++
	d.lastOpts = opts
	return d.result, nil
}

// plainStartDispatcher has no DispatchAgentStartWithOptions.
type plainStartDispatcher struct {
	AgentDispatcher // nil: only DispatchAgentStart is called
	starts          int
	lastErr         error
}

func (d *plainStartDispatcher) DispatchAgentStart(_ context.Context, _ *store.Agent, _ string, _ bool) error {
	d.starts++
	return d.lastErr
}

// TestDispatchAgentStartWithOptions_Fallback pins the options helper: a
// dispatcher that takes options gets them (and its result back); one
// without gets the plain synchronous start and a nil result, error kept.
func TestDispatchAgentStartWithOptions_Fallback(t *testing.T) {
	ctx := context.Background()
	a := &store.Agent{ID: "a1"}

	od := &optionsProbeDispatcher{result: &StartDispatchResult{Launch: &LaunchAccepted{ID: "L1"}}}
	res, err := dispatchAgentStartWithOptions(ctx, od, a, "", false, StartDispatchOptions{Async: true})
	require.NoError(t, err)
	assert.Equal(t, 1, od.optionStarts)
	assert.Equal(t, 0, od.plainStarts)
	assert.True(t, od.lastOpts.Async)
	assert.Equal(t, "L1", res.AcceptedLaunch().ID)

	pd := &plainStartDispatcher{lastErr: errors.New("boom")}
	res, err = dispatchAgentStartWithOptions(ctx, pd, a, "", false, StartDispatchOptions{Async: true})
	assert.EqualError(t, err, "boom")
	assert.Nil(t, res)
	assert.Nil(t, res.AcceptedLaunch(), "nil-safe")
	assert.Equal(t, 1, pd.starts, "the fallback is the plain synchronous start")
}

// drainingErr is a broker's broker_draining answer.
func drainingErr(retryAfter string) error {
	return &brokerStatusError{
		StatusCode: http.StatusServiceUnavailable,
		Body:       `{"error":{"code":"broker_draining","message":"Failed to start agent: the runtime broker is shutting down; retry shortly"}}`,
		RetryAfter: retryAfter,
	}
}

// TestBrokerDraining_StartReleasedAndRelayed pins Optional-2: a broker's
// 503 broker_draining releases the start claim like runtime_unavailable,
// is relayed as a retryable 503 with the broker's Retry-After, and leaves
// the start-failure revoke disarmed. A plain 503 stays unconfirmed.
func TestBrokerDraining_StartReleasedAndRelayed(t *testing.T) {
	err := drainingErr("7")
	assert.True(t, isBrokerDraining(err))
	assert.Equal(t, startReleased, startOutcomeOf(err))
	assert.False(t, isConfirmedStartNotActedOnError(err), "the revoke rule is unchanged")
	assert.False(t, isBrokerRuntimeUnavailable(err))

	plain503 := &brokerStatusError{StatusCode: http.StatusServiceUnavailable, Body: `{"error":{"code":"unavailable"}}`}
	assert.False(t, isBrokerDraining(plain503))
	assert.Equal(t, startUnconfirmed, startOutcomeOf(plain503))
	wrongStatus := &brokerStatusError{StatusCode: http.StatusConflict, Body: `{"error":{"code":"broker_draining"}}`}
	assert.False(t, isBrokerDraining(wrongStatus))

	rec := httptest.NewRecorder()
	require.True(t, writeBrokerRuntimeUnavailable(rec, err, "docker"))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "7", rec.Header().Get("Retry-After"))
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, api.BrokerErrCodeBrokerDraining, body.Error.Code)
	assert.Equal(t, brokerDrainingMessage, body.Error.Message, "the hub's own text, not the broker's")

	rec = httptest.NewRecorder()
	require.True(t, writeBrokerRuntimeUnavailable(rec, drainingErr("bogus"), "docker"))
	assert.Equal(t, defaultBrokerDrainingRetryAfter, rec.Header().Get("Retry-After"))

	rec = httptest.NewRecorder()
	assert.False(t, writeBrokerRuntimeUnavailable(rec, plain503, "docker"))
}

// TestIsUnconfirmedLaunchError_BrokerRestarted is C-T8: broker_restarted is
// reported by the broker itself (its launch journal), so it is a
// broker-confirmed end and keeps revoking on a later start failure.
func TestIsUnconfirmedLaunchError_BrokerRestarted(t *testing.T) {
	assert.False(t, isUnconfirmedLaunchError(store.LaunchErrorBrokerRestarted))
	assert.Equal(t, "broker_restarted", store.LaunchErrorBrokerRestarted)
}

// TestHubInstanceSnapshot_Features pins async_start_v1 in the registry row:
// the snapshot carries it through the cap, and a change to Features alone
// is a material change that rewrites the full row (design D-a, Nit-1).
func TestHubInstanceSnapshot_Features(t *testing.T) {
	assert.Contains(t, hubInstanceFeatures(), api.HubInstanceFeatureAsyncStartV1)

	stats := api.HubInstanceStats{Features: hubInstanceFeatures()}
	snap := hubInstanceSnapshotFromChecks("hub-a", "v1", map[string]string{"database": "healthy"}, stats)
	assert.Equal(t, []string{api.HubInstanceFeatureAsyncStartV1}, snap.Stats.Features)
	var stored api.HubInstanceStats
	require.NoError(t, json.Unmarshal(snap.statsJSON(), &stored))
	assert.Equal(t, []string{api.HubInstanceFeatureAsyncStartV1}, stored.Features)

	without := hubInstanceSnapshotFromChecks("hub-a", "v1", map[string]string{"database": "healthy"}, api.HubInstanceStats{})
	assert.NotEqual(t, string(snap.material()), string(without.material()), "Features is material")
}

// asyncStartPhaseReadAllowlist is the reviewed number of reads of
// agent.Phase / existingAgent.Phase per function in the five start-path
// files (design D-T12(k)). Each remaining read is either a quota or display
// read, a write-back guard, or treats error and every remembered phase the
// same way (design D-c, "Sweep"). A branch selector must read
// store.EffectiveRestingPhase instead.
var asyncStartPhaseReadAllowlist = map[string]int{
	"handlers_agent_create_helpers.go:handleExistingAgent":  7, // log field, storedPhase, post-start "broker set no phase" checks
	"handlers_agent_lifecycle.go:disruptionSettled":         2, // stopped and error alike
	"handlers_agent_lifecycle.go:guardAgentPhaseTransition": 1, // status guard, not a branch selector
	"handlers_agent_lifecycle.go:handleAgentLifecycle":      8, // forced recovery needs a stored error, quota, suspend guards, broker-set phase
	"handlers_agent_lifecycle.go:handleStopAllAgents":       2, // stop-all status reads the stored phase on purpose
	"handlers_agent_lifecycle.go:updateAgentStatus":         1, // quota reconcile on the stored phase
	"start_claim.go:startAgentCore":                         1, // hadEphemeral: error and every remembered phase differ from running
	"start_claim.go:writeStartedStatus":                     1, // maps error and every remembered phase to running
	"start_dispatch.go:beginStartDispatch":                  3, // stored phase: rollback target, quota, IfPhase
}

var (
	phaseReadFuncRe = regexp.MustCompile(`^func (?:\([^)]*\) )?([A-Za-z0-9_]+)`)
	phaseReadRe     = regexp.MustCompile(`(?:^|[^A-Za-z0-9_])(?:agent|existingAgent)\.Phase(?:[^A-Za-z0-9_]|$)`)
	phaseAssignRe   = regexp.MustCompile(`^[ \t]*=[^=]`)
)

// countAgentPhaseReads counts, per function, the reads (not assignments)
// of agent.Phase and existingAgent.Phase in file, skipping comment lines and
// trailing " // " comments.
func countAgentPhaseReads(t *testing.T, file string) map[string]int {
	t.Helper()
	f, err := os.Open(file)
	require.NoError(t, err)
	defer f.Close()
	counts := map[string]int{}
	fn := ""
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if m := phaseReadFuncRe.FindStringSubmatch(line); m != nil {
			fn = m[1]
		}
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "//") {
			continue
		}
		if i := strings.Index(line, " // "); i >= 0 {
			line = line[:i]
		}
		for {
			loc := phaseReadRe.FindStringIndex(line)
			if loc == nil {
				break
			}
			// The match ends with the delimiter after "Phase" (or at the
			// end of the line); continue from that delimiter.
			end := loc[1]
			if end > loc[0] && end <= len(line) && !strings.HasSuffix(line[loc[0]:end], "Phase") {
				end--
			}
			if !phaseAssignRe.MatchString(line[end:]) {
				counts[file+":"+fn]++
			}
			line = line[end:]
		}
	}
	require.NoError(t, sc.Err())
	return counts
}

// TestAsyncStartPhaseReadsReviewed is the source scan of design D-T12(k):
// a new read of agent.Phase or existingAgent.Phase in the five start-path
// files fails here until it is reviewed. If the read picks how to start an
// agent, it must use store.EffectiveRestingPhase (resume memory); otherwise
// add it to asyncStartPhaseReadAllowlist with the reason.
func TestAsyncStartPhaseReadsReviewed(t *testing.T) {
	got := map[string]int{}
	for _, file := range []string{"start_claim.go", "start_dispatch.go", "wake_dm.go", "handlers_agent_lifecycle.go", "handlers_agent_create_helpers.go"} {
		for k, v := range countAgentPhaseReads(t, file) {
			got[k] = v
		}
	}
	var diffs []string
	for k, v := range got {
		if asyncStartPhaseReadAllowlist[k] != v {
			diffs = append(diffs, k)
		}
	}
	for k := range asyncStartPhaseReadAllowlist {
		if _, ok := got[k]; !ok {
			diffs = append(diffs, k)
		}
	}
	sort.Strings(diffs)
	assert.Empty(t, diffs, "agent.Phase reads changed (got %v); a branch selector must read store.EffectiveRestingPhase", got)
}

// TestWaitForLifecycleOutcome_LaunchResultEndsWait is the requester side
// of the cross-node async start (design D-d, the OR-wait): a done row whose
// result carries an accepted launch ends the wait with success before any
// success phase; a done row without one (a synchronous owner) still waits
// for the phase, here until the rolling window expires.
func TestWaitForLifecycleOutcome_LaunchResultEndsWait(t *testing.T) {
	setLifecycleTimings(t, 200*time.Millisecond, time.Hour, 0)
	done := Event{Subject: "broker.dispatch.d-1.done"}

	st := &fakeDispatchStore{dispatches: map[string]*store.BrokerDispatch{
		"d-1": {ID: "d-1", State: store.DispatchStateDone, Result: marshalLifecycleResult(LifecycleDispatchResult{Launch: &LaunchAccepted{ID: "L1"}})},
	}}
	ch := make(chan Event, 1)
	ch <- done
	require.NoError(t, waitForLifecycleOutcome(context.Background(), ch, func() {}, st, "d-1", "start", isStartTerminal))

	sync := &fakeDispatchStore{dispatches: map[string]*store.BrokerDispatch{
		"d-1": {ID: "d-1", State: store.DispatchStateDone},
	}}
	ch = make(chan Event, 1)
	ch <- done
	err := waitForLifecycleOutcome(context.Background(), ch, func() {}, sync, "d-1", "start", isStartTerminal)
	assert.ErrorIs(t, err, ErrDispatchFailed, "a synchronous owner's done row alone does not end the wait")
}

// TestHubInstanceSnapshot_MaxFeaturesFitRowCap pins that Features, which
// the cap never trims, fits the 4 KiB row cap at its own limits next to the
// largest checks, DB block and integrations: the integrations are cut, the
// features kept.
func TestHubInstanceSnapshot_MaxFeaturesFitRowCap(t *testing.T) {
	checks := map[string]string{}
	for i := 0; i < api.BrokerHealthMaxChecks; i++ {
		checks[fmt.Sprintf("%02d", i)+strings.Repeat("c", api.HubInstanceMaxCheckNameChars-2)] = "unavailable: detail"
	}
	var integrations []api.HubInstanceIntegration
	for i := 0; i < api.HubInstanceMaxIntegrations; i++ {
		integrations = append(integrations, api.HubInstanceIntegration{
			Name:    fmt.Sprintf("%02d", i) + strings.Repeat("n", api.HubInstanceMaxIntegrationNameChars-2),
			Health:  "unhealthy",
			Version: strings.Repeat("v", api.HubInstanceMaxIntegrationVersionBytes),
		})
	}
	var features []string
	for i := 0; i < api.HubInstanceMaxFeatures; i++ {
		features = append(features, fmt.Sprintf("%02d", i)+strings.Repeat("f", api.HubInstanceMaxFeatureBytes-2))
	}
	snap := hubInstanceSnapshotFromChecks(strings.Repeat("l", hubInstanceMaxLabelBytes), strings.Repeat("v", 100), checks,
		api.HubInstanceStats{
			DB:           &api.HubInstanceDBStats{InUse: 1 << 30, Idle: 1 << 30, MaxOpen: 1 << 30, WaitCount: 1 << 62},
			Integrations: integrations,
			Features:     features,
		})
	b, err := json.Marshal(snap)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(b), api.HubInstanceRowMaxBytes)
	assert.Equal(t, features, snap.Stats.Features, "features are never cut")
}
