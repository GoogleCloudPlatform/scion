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

package hub

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for a cross-node dispatch that the broker rejects: the executing node
// records the broker's HTTP error on the failed dispatch row, and the
// originating node returns the same typed error a direct dispatch returns.

const typedSkillErrorBody = `{"error":{"code":"skill_resolution_failed","message":"required skill \"gh://owner/repo/my-skill@main\" could not be resolved: rate limited","details":{"skill":"gh://owner/repo/my-skill@main","cause":"rate_limited"}}}`

func typedSkillError() *brokerStatusError {
	return &brokerStatusError{StatusCode: http.StatusTooManyRequests, Body: typedSkillErrorBody, RetryAfter: "30"}
}

func typedRuntimeUnavailableError() *brokerStatusError {
	return &brokerStatusError{StatusCode: http.StatusServiceUnavailable, Body: brokerRuntimeUnavailableBody, RetryAfter: "17"}
}

// ownerErrDispatcher is the executing node's dispatcher. Each op returns its
// configured error, as the local broker client would on an HTTP error answer.
type ownerErrDispatcher struct {
	lifecycleTestDispatcher
	err         error
	beforeStart func()
}

func (d *ownerErrDispatcher) DispatchAgentStart(ctx context.Context, a *store.Agent, task string, resume bool) error {
	if d.beforeStart != nil {
		d.beforeStart()
	}
	_ = d.lifecycleTestDispatcher.DispatchAgentStart(ctx, a, task, resume)
	return d.err
}
func (d *ownerErrDispatcher) DispatchAgentStop(ctx context.Context, a *store.Agent) error {
	_ = d.lifecycleTestDispatcher.DispatchAgentStop(ctx, a)
	return d.err
}
func (d *ownerErrDispatcher) DispatchAgentRestart(ctx context.Context, a *store.Agent) error {
	_ = d.lifecycleTestDispatcher.DispatchAgentRestart(ctx, a)
	return d.err
}
func (d *ownerErrDispatcher) DispatchCheckAgentPrompt(ctx context.Context, a *store.Agent) (bool, error) {
	_, _ = d.lifecycleTestDispatcher.DispatchCheckAgentPrompt(ctx, a)
	return false, d.err
}
func (d *ownerErrDispatcher) DispatchAgentCreateWithGather(ctx context.Context, a *store.Agent) (*CreateDispatchResult, error) {
	_, _ = d.lifecycleTestDispatcher.DispatchAgentCreateWithGather(ctx, a)
	return nil, d.err
}
func (d *ownerErrDispatcher) DispatchFinalizeEnv(ctx context.Context, a *store.Agent, env map[string]string) (*CreateDispatchResult, error) {
	_, _ = d.lifecycleTestDispatcher.DispatchFinalizeEnv(ctx, a, env)
	return nil, d.err
}

// ownerSignalBus delivers the requesting node's signal to the owner node,
// which drains the broker's dispatch rows with its real reconcileBroker.
type ownerSignalBus struct {
	NoopCommandBus
	owner *Server
}

func (b ownerSignalBus) SignalBrokerCmd(_ context.Context, brokerID string) error {
	go b.owner.reconcileBroker(context.Background(), brokerID)
	return nil
}

// crossNodeFixture is two hub nodes over one store and one event bus: the
// requester's broker client always defers, and the owner executes the
// dispatch rows.
type crossNodeFixture struct {
	store     store.Store
	events    *ChannelEventPublisher
	owner     *Server
	ownerDisp *ownerErrDispatcher
	requester *HTTPAgentDispatcher
	agent     *store.Agent
}

// newCrossNodeFixture builds the fixture. With signalOwner false the
// requester's signal goes nowhere and the test plays the owner itself.
func newCrossNodeFixture(t *testing.T, ownerErr error, signalOwner bool) *crossNodeFixture {
	t.Helper()
	cs := entadapter.NewCompositeStore(enttest.NewClient(t))
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)

	disp := &ownerErrDispatcher{err: ownerErr}
	owner := &Server{
		store:             cs,
		instanceID:        "hub-owner-" + uuid.NewString()[:8],
		agentLifecycleLog: slog.Default(),
		events:            events,
	}
	owner.SetDispatcher(disp)
	owner.execDispatch = owner.executeDispatch
	owner.deliverMsg = owner.deliverMessage

	requester := NewHTTPAgentDispatcherWithClient(cs, &deferredTestClient{localBroker: "local-broker"}, false, slog.Default())
	var bus CommandBus = NoopCommandBus{}
	if signalOwner {
		bus = ownerSignalBus{owner: owner}
	}
	requester.SetCrossNodeDeps(events, bus)

	agent := seedAgentWithBrokerID(t, cs, uuid.NewString())
	return &crossNodeFixture{store: cs, events: events, owner: owner, ownerDisp: disp, requester: requester, agent: agent}
}

// claimPending waits for the agent's single pending dispatch row and claims
// it as an owner node would.
func (f *crossNodeFixture) claimPending(t *testing.T) store.BrokerDispatch {
	t.Helper()
	var row store.BrokerDispatch
	require.Eventually(t, func() bool {
		pending, err := f.store.ListPendingDispatch(context.Background(), f.agent.RuntimeBrokerID)
		if err != nil || len(pending) == 0 {
			return false
		}
		row = pending[0]
		return true
	}, 5*time.Second, 5*time.Millisecond, "no dispatch row was written")
	claimed, err := f.store.ClaimBrokerDispatch(context.Background(), row.ID, "test-owner")
	require.NoError(t, err)
	require.True(t, claimed)
	return row
}

// failRow fails a claimed row with execErr exactly as reconcileBroker does.
func (f *crossNodeFixture) failRow(t *testing.T, id string, execErr error) {
	t.Helper()
	require.NoError(t, f.store.FailBrokerDispatch(context.Background(), id, execErr.Error(), dispatchFailureResult(execErr)))
}

// requireSkillRelay asserts err relays as the broker's typed skill error.
func requireSkillRelay(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	rec := httptest.NewRecorder()
	require.True(t, relaySkillResolutionError(rec, err), "error is not the broker's typed skill error: %v", err)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, "30", rec.Header().Get("Retry-After"))
	code, details := errorBody(t, rec)
	assert.Equal(t, skillResolutionErrorCode, code)
	assert.Equal(t, "rate_limited", details["cause"])
	assert.Contains(t, rec.Body.String(), "could not be resolved: rate limited")
	assert.NotContains(t, rec.Body.String(), "runtime broker returned error", "relayed without a hub prefix")
}

// crossNodeCtx bounds a cross-node call. Without the result envelope the
// requester waits for a status event that never comes, so a call that ends
// well inside this bound proves the row's failure ended the wait.
func crossNodeCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestCrossNodeLifecycle_RelaysTypedBrokerError(t *testing.T) {
	cases := []struct {
		name string
		call func(ctx context.Context, f *crossNodeFixture) error
	}{
		{"start", func(ctx context.Context, f *crossNodeFixture) error {
			return f.requester.DispatchAgentStart(ctx, f.agent, "task", false)
		}},
		{"resume", func(ctx context.Context, f *crossNodeFixture) error {
			return f.requester.DispatchAgentStart(ctx, f.agent, "", true)
		}},
		{"restart", func(ctx context.Context, f *crossNodeFixture) error {
			return f.requester.DispatchAgentRestart(ctx, f.agent)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCrossNodeFixture(t, typedSkillError(), true)
			began := time.Now()
			err := tc.call(crossNodeCtx(t), f)
			requireSkillRelay(t, err)
			assert.Less(t, time.Since(began), 3*time.Second)
		})
	}
}

// The carrier is generic: a broker runtime_unavailable 503 on a cross-node
// start reaches the caller as the same retryable 503 with its Retry-After.
func TestCrossNodeStart_RelaysRuntimeUnavailable(t *testing.T) {
	f := newCrossNodeFixture(t, typedRuntimeUnavailableError(), true)
	err := f.requester.DispatchAgentStart(crossNodeCtx(t), f.agent, "", false)
	require.Error(t, err)

	rec := httptest.NewRecorder()
	require.True(t, writeBrokerRuntimeUnavailable(rec, err, "docker"), "error is not the broker's runtime_unavailable: %v", err)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "17", rec.Header().Get("Retry-After"))
	code, _ := errorBody(t, rec)
	assert.Equal(t, brokerCodeRuntimeUnavailable, code)
}

// A cross-node stop the broker rejects returns the broker's error as soon
// as the row fails, instead of waiting out the rolling window.
func TestCrossNodeStop_ReturnsTypedFailure(t *testing.T) {
	f := newCrossNodeFixture(t, typedRuntimeUnavailableError(), true)
	began := time.Now()
	err := f.requester.DispatchAgentStop(crossNodeCtx(t), f.agent)
	require.Error(t, err)
	assert.True(t, isBrokerRuntimeUnavailable(err), "error is not the broker's runtime_unavailable: %v", err)
	assert.Less(t, time.Since(began), 3*time.Second)
}

// An error phase that reaches the requester before the done event (a stale
// status, or a status from another node) does not hide the typed error.
func TestCrossNodeStart_ErrorPhaseBeforeDone(t *testing.T) {
	f := newCrossNodeFixture(t, typedSkillError(), true)
	f.ownerDisp.beforeStart = func() {
		errored := *f.agent
		errored.Phase = string(state.PhaseError)
		f.events.PublishAgentStatus(context.Background(), &errored)
		time.Sleep(100 * time.Millisecond)
	}
	err := f.requester.DispatchAgentStart(crossNodeCtx(t), f.agent, "", false)
	requireSkillRelay(t, err)
}

// A done event that finds the row not yet failed (the executor's write lost
// its CAS, or a reaper requeued the row) does not end the wait; the next
// done event with the failed row does.
func TestCrossNodeStart_DoneWithNonTerminalRowKeepsWaiting(t *testing.T) {
	f := newCrossNodeFixture(t, nil, false)
	go func() {
		row := f.claimPending(t)
		f.events.PublishDispatchDone(context.Background(), row.ID) // row is in_progress
		time.Sleep(100 * time.Millisecond)
		f.failRow(t, row.ID, typedSkillError())
		f.events.PublishDispatchDone(context.Background(), row.ID)
	}()
	err := f.requester.DispatchAgentStart(crossNodeCtx(t), f.agent, "", false)
	requireSkillRelay(t, err)
}

// A missed done event is recovered by the row poll, even while status
// heartbeats keep resetting the rolling window.
func TestCrossNodeStart_MissedDoneFoundByRowPoll(t *testing.T) {
	setLifecycleTimings(t, 200*time.Millisecond, 50*time.Millisecond, 0)
	f := newCrossNodeFixture(t, nil, false)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		row := f.claimPending(t)
		f.failRow(t, row.ID, typedSkillError()) // no done event
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				hb := *f.agent
				hb.Phase = string(state.PhaseStarting)
				f.events.PublishAgentStatus(context.Background(), &hb)
			}
		}
	}()
	err := f.requester.DispatchAgentStart(crossNodeCtx(t), f.agent, "", false)
	requireSkillRelay(t, err)
}

// A failed row with no envelope (an executor that predates it, or a failure
// that is not a broker HTTP answer) still ends the wait, with the row's
// error text and no typed error.
func TestCrossNodeStart_FailedRowWithoutEnvelope(t *testing.T) {
	f := newCrossNodeFixture(t, errors.New("owner exploded"), true)
	began := time.Now()
	err := f.requester.DispatchAgentStart(crossNodeCtx(t), f.agent, "", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "owner exploded")
	var se *brokerStatusError
	assert.False(t, errors.As(err, &se))
	assert.Less(t, time.Since(began), 3*time.Second)
}

// Cross-node create, finalize_env and check_prompt read the same failed row
// and return the broker's typed error.
func TestCrossNodeDataOps_RelayTypedBrokerError(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		f := newCrossNodeFixture(t, typedSkillError(), true)
		_, err := f.requester.deferredCreateWithGather(crossNodeCtx(t), f.agent)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "dispatch create failed:")
		rec := httptest.NewRecorder()
		dispatchCreateErrorResponse(rec, err)
		assert.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
		code, _ := errorBody(t, rec)
		assert.Equal(t, skillResolutionErrorCode, code)
	})
	t.Run("finalize_env", func(t *testing.T) {
		f := newCrossNodeFixture(t, typedSkillError(), true)
		_, err := f.requester.deferredFinalizeEnv(crossNodeCtx(t), f.agent, map[string]string{"K": "v"})
		requireSkillRelay(t, err)
	})
	t.Run("check_prompt", func(t *testing.T) {
		f := newCrossNodeFixture(t, typedRuntimeUnavailableError(), true)
		_, err := f.requester.deferredCheckPrompt(crossNodeCtx(t), f.agent)
		require.Error(t, err)
		assert.True(t, isBrokerRuntimeUnavailable(err), "error is not the broker's runtime_unavailable: %v", err)
	})
}

// The executing node records the broker's HTTP error as the envelope on the
// failed row, keeps the row's error text unchanged, and records no result
// for a failure that is not a broker HTTP answer.
func TestReconcile_FailedDispatchRecordsBrokerErrorEnvelope(t *testing.T) {
	run := func(t *testing.T, ownerErr error) *store.BrokerDispatch {
		t.Helper()
		f := newCrossNodeFixture(t, ownerErr, false)
		args, err := MarshalDispatchArgs(&StartDispatchArgs{})
		require.NoError(t, err)
		row := &store.BrokerDispatch{ID: uuid.NewString(), BrokerID: f.agent.RuntimeBrokerID, AgentID: f.agent.ID, Op: "start", Args: args}
		require.NoError(t, f.store.InsertBrokerDispatch(context.Background(), row))
		f.owner.reconcileBroker(context.Background(), f.agent.RuntimeBrokerID)
		got, err := f.store.GetBrokerDispatch(context.Background(), row.ID)
		require.NoError(t, err)
		require.Equal(t, store.DispatchStateFailed, got.State)
		return got
	}

	t.Run("broker HTTP error", func(t *testing.T) {
		got := run(t, typedSkillError())
		assert.Equal(t, "dispatch start: "+typedSkillError().Error(), got.Error, "error text is unchanged")
		var env dispatchFailureEnvelope
		require.NoError(t, json.Unmarshal([]byte(got.Result), &env), got.Result)
		require.NotNil(t, env.BrokerError)
		assert.Equal(t, http.StatusTooManyRequests, env.BrokerError.Status)
		assert.Equal(t, skillResolutionErrorCode, env.BrokerError.Code)
		assert.Equal(t, typedSkillErrorBody, env.BrokerError.Body)
		assert.Equal(t, "30", env.BrokerError.RetryAfter)
	})
	t.Run("other error", func(t *testing.T) {
		got := run(t, errors.New("no tunnel"))
		assert.Equal(t, "dispatch start: no tunnel", got.Error)
		assert.Empty(t, got.Result)
	})
}

func TestDispatchFailureError_EnvelopeFallbacks(t *testing.T) {
	row := func(result string) *store.BrokerDispatch {
		return &store.BrokerDispatch{Op: "start", State: store.DispatchStateFailed, Error: "dispatch start: boom", Result: result}
	}
	for _, tc := range []struct {
		name   string
		result string
	}{
		{"empty", ""},
		{"not JSON", "{garbage"},
		{"no envelope", `{"superseded":true}`},
		{"status zero", `{"brokerError":{"status":0,"body":"x"}}`},
		{"status 200", `{"brokerError":{"status":200,"body":"x"}}`},
		{"status 600", `{"brokerError":{"status":600,"body":"x"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := dispatchFailureError(row(tc.result))
			assert.EqualError(t, err, "dispatch start failed: dispatch start: boom")
			var se *brokerStatusError
			assert.False(t, errors.As(err, &se))
		})
	}

	t.Run("valid envelope", func(t *testing.T) {
		err := dispatchFailureError(row(dispatchFailureResult(typedSkillError())))
		var se *brokerStatusError
		require.ErrorAs(t, err, &se)
		assert.Equal(t, typedSkillError(), se)
		assert.True(t, strings.HasPrefix(err.Error(), "dispatch start failed: "))
	})

	t.Run("no envelope for non-HTTP status", func(t *testing.T) {
		assert.Empty(t, dispatchFailureResult(&brokerStatusError{StatusCode: 0, Body: "x"}))
		assert.Empty(t, dispatchFailureResult(errors.New("plain")))
	})

	t.Run("oversized body is cut on a UTF-8 boundary", func(t *testing.T) {
		body := strings.Repeat("é", maxBrokerErrorBodyBytes) // 2 bytes each
		result := dispatchFailureResult(&brokerStatusError{StatusCode: http.StatusBadGateway, Body: "x" + body})
		se := brokerErrorFromResult(result)
		require.NotNil(t, se)
		assert.Equal(t, http.StatusBadGateway, se.StatusCode)
		assert.LessOrEqual(t, len(se.Body), maxBrokerErrorBodyBytes)
		assert.Greater(t, len(se.Body), maxBrokerErrorBodyBytes-utf8.UTFMax)
		assert.True(t, utf8.ValidString(se.Body))
	})
}

// A cross-node delete the broker rejects is classified as a direct delete
// is: runtime_unavailable rolls back with the broker's Retry-After, and a
// 409 is a conflict.
func TestAgentDeleteEngine_DeferredTypedFailureClassifies(t *testing.T) {
	cases := []struct {
		name       string
		brokerErr  *brokerStatusError
		wantStatus int
		wantCode   string
		retryAfter string
	}{
		{"runtime_unavailable", typedRuntimeUnavailableError(), http.StatusServiceUnavailable, store.DeletionCodeRuntimeUnavailable, "17"},
		{"conflict", &brokerStatusError{StatusCode: http.StatusConflict, Body: `{"error":{"message":"ambiguous"}}`}, http.StatusConflict, store.DeletionCodeConflict, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setDeleteWaitTimeout(t, func(context.Context) time.Duration { return 10 * time.Second })
			f := newDeferredDeleteFixture(t, "typed-"+tc.name, nil)
			go func() {
				assert.Eventually(t, func() bool {
					all, _ := f.store.ListPendingDispatch(context.Background(), f.agent.RuntimeBrokerID)
					for _, d := range all {
						if d.AgentID != f.agent.ID {
							continue
						}
						if ok, _ := f.store.ClaimBrokerDispatch(context.Background(), d.ID, "test-owner"); ok {
							execErr := errors.Join(errors.New("dispatch delete"), tc.brokerErr)
							_ = f.store.FailBrokerDispatch(context.Background(), d.ID, execErr.Error(), dispatchFailureResult(execErr))
						}
						f.bus.PublishDispatchDone(context.Background(), d.ID)
						return true
					}
					return false
				}, 5*time.Second, 10*time.Millisecond, "the delete intent was never written")
			}()

			r := f.del(t, "")
			require.Equal(t, tc.wantStatus, r.rec.Code, r.rec.Body.String())
			if tc.retryAfter != "" {
				assert.Equal(t, tc.retryAfter, r.rec.Header().Get("Retry-After"))
			}
			got := mustGetAgent(t, f.store, f.agent.ID)
			assert.Equal(t, tc.wantCode, got.DeletionCode)
			assert.Equal(t, string(state.PhaseRunning), got.Phase, "the prior phase is restored")
		})
	}
}
