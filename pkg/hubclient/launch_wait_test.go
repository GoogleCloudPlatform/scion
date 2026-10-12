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

package hubclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests replace package-level timings; they must not run in parallel.

// fastLaunchTimings shortens the wait timings for one test.
func fastLaunchTimings(t *testing.T) {
	t.Helper()
	poll, slack, fallback := launchPollInterval, launchWaitSlack, launchWaitFallback
	grace, backoff, backoffMax := launchUnreadableGrace, launchUnreadableBackoff, launchUnreadableBackoffMax
	t.Cleanup(func() {
		launchPollInterval, launchWaitSlack, launchWaitFallback = poll, slack, fallback
		launchUnreadableGrace, launchUnreadableBackoff, launchUnreadableBackoffMax = grace, backoff, backoffMax
	})
	launchPollInterval = 5 * time.Millisecond
	launchWaitSlack = 50 * time.Millisecond
	launchWaitFallback = 400 * time.Millisecond
	launchUnreadableGrace = 60 * time.Millisecond
	launchUnreadableBackoff = 5 * time.Millisecond
	launchUnreadableBackoffMax = 10 * time.Millisecond
}

// getResult is one scripted answer to GET agent.
type getResult struct {
	agent *Agent
	err   error
}

// scriptedGetService answers Get from a script; the last entry repeats.
type scriptedGetService struct {
	AgentService
	mu     sync.Mutex
	script []getResult
	calls  int
}

func (s *scriptedGetService) Get(ctx context.Context, agentID string) (*Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.calls
	s.calls++
	if len(s.script) == 0 {
		return nil, errors.New("no script")
	}
	if i >= len(s.script) {
		i = len(s.script) - 1
	}
	r := s.script[i]
	if r.agent != nil {
		a := *r.agent
		return &a, r.err
	}
	return nil, r.err
}

func (s *scriptedGetService) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func script(results ...getResult) *scriptedGetService {
	return &scriptedGetService{script: results}
}

func ok(a *Agent) getResult { return getResult{agent: a} }

func httpErr(code int) getResult {
	return getResult{err: &apiclient.APIError{StatusCode: code, Message: http.StatusText(code)}}
}

func launchingAgent(step string) *Agent {
	return &Agent{ID: "a1", Name: "alpha", Phase: "provisioning",
		Launch: &AgentLaunch{ID: "l1", State: "active", Active: true, Kind: "create", Step: step}}
}

func runningAgent() *Agent {
	return &Agent{ID: "a1", Name: "alpha", Phase: "running",
		Launch: &AgentLaunch{ID: "l1", State: "ended", Kind: "create", EndReason: "succeeded"}}
}

func intPtr(v int) *int { return &v }

// Mutant: launchDone's running case returns not-done.
func TestWaitForLaunch_RunningAfterSteps(t *testing.T) {
	fastLaunchTimings(t)
	svc := script(ok(launchingAgent("image_pull")), ok(launchingAgent("container_start")), ok(runningAgent()))

	out, err := WaitForLaunch(context.Background(), svc, "a1", launchingAgent(""), LaunchWaitOptions{})
	require.NoError(t, err)
	assert.True(t, out.Running)
	assert.Equal(t, "running", out.Phase)
	assert.Empty(t, out.Code)
	require.NotNil(t, out.Agent)
	assert.Equal(t, 3, svc.Calls())
}

// Mutant: drop the "launch ended with an error code" clause in launchDone.
func TestWaitForLaunch_EndedWithErrorAnyPhase(t *testing.T) {
	for _, phase := range []string{"created", "provisioning", "starting", "suspended"} {
		t.Run(phase, func(t *testing.T) {
			fastLaunchTimings(t)
			failed := &Agent{ID: "a1", Name: "alpha", Phase: phase, Message: "pull access denied",
				Launch: &AgentLaunch{ID: "l1", State: "ended", Kind: "start", Step: "image_pull", Error: "image_pull_failed"}}
			svc := script(ok(launchingAgent("image_pull")), ok(failed))

			out, err := WaitForLaunch(context.Background(), svc, "a1", launchingAgent(""), LaunchWaitOptions{})
			require.NoError(t, err)
			assert.False(t, out.Running)
			assert.Equal(t, phase, out.Phase)
			assert.Equal(t, "image_pull_failed", out.Code)
			assert.Equal(t, "image_pull", out.Step)
			assert.Equal(t, "pull access denied", out.Message)
		})
	}
}

// Mutant: drop the failed end reason clause in launchDone.
func TestWaitForLaunch_EndReasonWithoutCode(t *testing.T) {
	for _, reason := range []string{"failed", "timed_out", "lost", "not_launched"} {
		t.Run(reason, func(t *testing.T) {
			fastLaunchTimings(t)
			ended := &Agent{ID: "a1", Name: "alpha", Phase: "provisioning",
				Launch: &AgentLaunch{ID: "l1", State: "ended", Kind: "restart", EndReason: reason}}
			svc := script(ok(ended))

			out, err := WaitForLaunch(context.Background(), svc, "a1", launchingAgent(""), LaunchWaitOptions{})
			require.NoError(t, err)
			assert.False(t, out.Running)
			assert.Equal(t, reason, out.EndReason)
			assert.Equal(t, 1, svc.Calls())
		})
	}
}

// An ended launch that succeeded or was superseded does not end the wait
// by itself; the phase decides.
func TestWaitForLaunch_SucceededEndReasonWaitsForPhase(t *testing.T) {
	fastLaunchTimings(t)
	ended := &Agent{ID: "a1", Name: "alpha", Phase: "provisioning",
		Launch: &AgentLaunch{ID: "l1", State: "ended", Kind: "create", EndReason: "superseded"}}
	svc := script(ok(ended), ok(ended), ok(runningAgent()))

	out, err := WaitForLaunch(context.Background(), svc, "a1", launchingAgent(""), LaunchWaitOptions{})
	require.NoError(t, err)
	assert.True(t, out.Running)
	assert.Equal(t, 3, svc.Calls())
}

// Mutant: drop launchDone's error/stopped phase case.
func TestWaitForLaunch_PhaseErrorStopped(t *testing.T) {
	cases := map[string]*Agent{
		"error":          {ID: "a1", Phase: "error", Message: "container exited"},
		"stopped":        {ID: "a1", Phase: "stopped"},
		"legacy-crashed": {ID: "a1", Status: "crashed"},
	}
	for name, a := range cases {
		t.Run(name, func(t *testing.T) {
			fastLaunchTimings(t)
			svc := script(ok(a))
			out, err := WaitForLaunch(context.Background(), svc, "a1", launchingAgent(""), LaunchWaitOptions{})
			require.NoError(t, err)
			assert.False(t, out.Running)
			assert.Equal(t, a.Message, out.Message)
		})
	}
}

// Mutant: report the after-read 404 as unreadable for every caller (drop
// opts.AgentCaller from the unreadable branch).
func TestWaitForLaunch_UserLasting404AfterReadIsDeleted(t *testing.T) {
	fastLaunchTimings(t)
	svc := script(ok(launchingAgent("image_pull")), httpErr(http.StatusNotFound))

	out, err := WaitForLaunch(context.Background(), svc, "a1", launchingAgent(""), LaunchWaitOptions{})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAgentDeletedDuringLaunch)
	assert.True(t, apiclient.IsNotFoundError(err))
	var unreadable *LaunchStatusUnreadableError
	assert.False(t, errors.As(err, &unreadable))
	assert.False(t, out.Running)
	require.NotNil(t, out.Agent, "outcome keeps the last agent read")
	assert.Greater(t, svc.Calls(), 2, "the not-found is confirmed by retries")
}

// Mutant: restore the pre-#4704 rule (agent caller's 404 is unreadable only
// before any read).
func TestWaitForLaunch_AgentLasting404AfterReadIsUnreadable(t *testing.T) {
	fastLaunchTimings(t)
	svc := script(ok(launchingAgent("image_pull")), httpErr(http.StatusNotFound))

	_, err := WaitForLaunch(context.Background(), svc, "a1", launchingAgent(""), LaunchWaitOptions{AgentCaller: true})
	var unreadable *LaunchStatusUnreadableError
	require.True(t, errors.As(err, &unreadable), "got %v", err)
	assert.True(t, unreadable.AfterRead)
	assert.Equal(t, "l1", unreadable.LaunchID)
	assert.NotErrorIs(t, err, ErrAgentDeletedDuringLaunch)
	assert.Contains(t, err.Error(), "may have been deleted")
	assert.Greater(t, svc.Calls(), 2, "the not-found is confirmed by retries")
}

// Mutant: remove the after-read not-found retry.
func TestWaitForLaunch_Transient404AfterReadContinues(t *testing.T) {
	fastLaunchTimings(t)
	svc := script(ok(launchingAgent("image_pull")), httpErr(http.StatusNotFound), ok(runningAgent()))

	out, err := WaitForLaunch(context.Background(), svc, "a1", launchingAgent(""), LaunchWaitOptions{})
	require.NoError(t, err)
	assert.True(t, out.Running)
}

// Mutants: remove the early (before any read) retry; drop the accepted
// condition on an agent caller's early 403.
func TestWaitForLaunch_Early403404(t *testing.T) {
	type check func(t *testing.T, out LaunchOutcome, err error, svc *scriptedGetService)
	unreadable := func(t *testing.T, err error) *LaunchStatusUnreadableError {
		t.Helper()
		var u *LaunchStatusUnreadableError
		require.True(t, errors.As(err, &u), "got %v", err)
		return u
	}
	cases := []struct {
		name   string
		agent  bool
		start  *Agent
		script []getResult
		check  check
	}{
		{"agent 404 without accepted launch: unreadable, no retry", true, nil,
			[]getResult{httpErr(http.StatusNotFound)},
			func(t *testing.T, _ LaunchOutcome, err error, svc *scriptedGetService) {
				assert.False(t, unreadable(t, err).AfterRead)
				assert.Equal(t, 1, svc.Calls())
			}},
		{"agent 403 after accepted launch: unreadable after retries", true, launchingAgent(""),
			[]getResult{httpErr(http.StatusForbidden)},
			func(t *testing.T, _ LaunchOutcome, err error, svc *scriptedGetService) {
				u := unreadable(t, err)
				assert.False(t, u.AfterRead)
				assert.True(t, apiclient.IsForbiddenError(err))
				assert.Contains(t, err.Error(), "the launch continues on the Hub")
				assert.Greater(t, svc.Calls(), 1)
			}},
		{"agent 403 without accepted launch: rejected", true, nil,
			[]getResult{httpErr(http.StatusForbidden)},
			func(t *testing.T, _ LaunchOutcome, err error, _ *scriptedGetService) {
				var r *LaunchStatusRejectedError
				require.True(t, errors.As(err, &r), "got %v", err)
			}},
		{"user 404 after accepted launch: deleted after retries", false, launchingAgent(""),
			[]getResult{httpErr(http.StatusNotFound)},
			func(t *testing.T, _ LaunchOutcome, err error, svc *scriptedGetService) {
				assert.ErrorIs(t, err, ErrAgentDeletedDuringLaunch)
				assert.Greater(t, svc.Calls(), 1)
			}},
		{"user 403 after accepted launch: rejected", false, launchingAgent(""),
			[]getResult{httpErr(http.StatusForbidden)},
			func(t *testing.T, _ LaunchOutcome, err error, _ *scriptedGetService) {
				var r *LaunchStatusRejectedError
				require.True(t, errors.As(err, &r), "got %v", err)
				assert.True(t, apiclient.IsForbiddenError(err))
			}},
		{"user early 404 then running: retried", false, launchingAgent(""),
			[]getResult{httpErr(http.StatusNotFound), ok(runningAgent())},
			func(t *testing.T, out LaunchOutcome, err error, _ *scriptedGetService) {
				require.NoError(t, err)
				assert.True(t, out.Running)
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fastLaunchTimings(t)
			svc := script(tc.script...)
			out, err := WaitForLaunch(context.Background(), svc, "a1", tc.start, LaunchWaitOptions{AgentCaller: tc.agent})
			tc.check(t, out, err, svc)
		})
	}
}

// Mutant: launchWaitBudget ignores RemainingSeconds.
func TestLaunchWaitBudget(t *testing.T) {
	fastLaunchTimings(t)
	launchWaitSlack = 30 * time.Second
	launchWaitFallback = 5 * time.Minute
	withRemaining := func(r *int) *Agent { return &Agent{Launch: &AgentLaunch{Active: true, RemainingSeconds: r}} }

	assert.Equal(t, 100*time.Second, launchWaitBudget(withRemaining(intPtr(70))))
	assert.Equal(t, 30*time.Second, launchWaitBudget(withRemaining(intPtr(-5))))
	assert.Equal(t, 5*time.Minute, launchWaitBudget(withRemaining(nil)))
	assert.Equal(t, 5*time.Minute, launchWaitBudget(nil))
}

// Mutant: launchWaitBudget ignores RemainingSeconds.
func TestWaitForLaunch_BudgetFromRemainingSeconds(t *testing.T) {
	fastLaunchTimings(t)
	launchWaitFallback = 10 * time.Second
	start := launchingAgent("")
	start.Launch.RemainingSeconds = intPtr(0)
	svc := script(ok(launchingAgent("image_pull")))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	began := time.Now()
	out, err := WaitForLaunch(ctx, svc, "a1", start, LaunchWaitOptions{})
	assert.ErrorIs(t, err, ErrLaunchWaitTimeout)
	assert.Less(t, time.Since(began), time.Second)
	assert.False(t, out.Running)
	require.NotNil(t, out.Agent)
	assert.Equal(t, "image_pull", out.Step)
}

// Without an advertised budget on start, the first fetched agent's applies.
func TestWaitForLaunch_BudgetFromFirstFetch(t *testing.T) {
	fastLaunchTimings(t)
	launchWaitFallback = 10 * time.Second
	fetched := launchingAgent("image_pull")
	fetched.Launch.RemainingSeconds = intPtr(0)
	svc := script(ok(fetched))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := WaitForLaunch(ctx, svc, "a1", launchingAgent(""), LaunchWaitOptions{})
	assert.ErrorIs(t, err, ErrLaunchWaitTimeout)
}

// Mutant: report ErrLaunchWaitTimeout whenever the wait context is done.
func TestWaitForLaunch_ContextCancelled(t *testing.T) {
	fastLaunchTimings(t)
	launchWaitFallback = 10 * time.Second
	svc := script(ok(launchingAgent("image_pull")))

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(30*time.Millisecond, cancel)
	_, err := WaitForLaunch(ctx, svc, "a1", launchingAgent(""), LaunchWaitOptions{})
	assert.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, ErrLaunchWaitTimeout)

	cause := errors.New("received interrupt")
	cctx, ccancel := context.WithCancelCause(context.Background())
	time.AfterFunc(30*time.Millisecond, func() { ccancel(cause) })
	_, err = WaitForLaunch(cctx, svc, "a1", launchingAgent(""), LaunchWaitOptions{})
	assert.ErrorIs(t, err, context.Canceled)
	assert.ErrorIs(t, err, cause)
	assert.NotErrorIs(t, err, ErrLaunchWaitTimeout)
}

// Mutant: isTerminalFetchError treats 5xx as terminal.
func TestWaitForLaunch_TransientAndTerminalFetchErrors(t *testing.T) {
	t.Run("503 and 429 are retried", func(t *testing.T) {
		fastLaunchTimings(t)
		svc := script(httpErr(http.StatusServiceUnavailable), httpErr(http.StatusTooManyRequests),
			ok(launchingAgent("image_pull")), getResult{err: errors.New("connection reset")}, ok(runningAgent()))
		out, err := WaitForLaunch(context.Background(), svc, "a1", launchingAgent(""), LaunchWaitOptions{})
		require.NoError(t, err)
		assert.True(t, out.Running)
		assert.Equal(t, 5, svc.Calls())
	})
	t.Run("400 is terminal", func(t *testing.T) {
		fastLaunchTimings(t)
		svc := script(ok(launchingAgent("image_pull")), httpErr(http.StatusBadRequest))
		_, err := WaitForLaunch(context.Background(), svc, "a1", launchingAgent(""), LaunchWaitOptions{})
		var r *LaunchStatusRejectedError
		require.True(t, errors.As(err, &r), "got %v", err)
		assert.Equal(t, 2, svc.Calls())
	})
}

// Mutant: drop the step-change dedupe in reportStep.
func TestWaitForLaunch_OnStepPerChange(t *testing.T) {
	fastLaunchTimings(t)
	svc := script(ok(launchingAgent("image_pull")), ok(launchingAgent("image_pull")),
		ok(launchingAgent("container_start")), ok(launchingAgent("")), ok(runningAgent()))
	var steps []string

	_, err := WaitForLaunch(context.Background(), svc, "a1", launchingAgent("image_pull"),
		LaunchWaitOptions{OnStep: func(s string) { steps = append(steps, s) }})
	require.NoError(t, err)
	assert.Equal(t, []string{"image_pull", "container_start"}, steps)
}

// Mutant: drop the LaunchActive short-circuit in EnsureRunningBeforeSend.
func TestEnsureRunningBeforeSend(t *testing.T) {
	t.Run("not launching: no read", func(t *testing.T) {
		fastLaunchTimings(t)
		for _, a := range []*Agent{
			runningAgent(),
			{ID: "a1", Phase: "provisioning", Launch: &AgentLaunch{ID: "l0", State: "ended", EndReason: "succeeded"}},
			{ID: "a1", Phase: "stopped"},
		} {
			svc := script(httpErr(http.StatusInternalServerError))
			require.NoError(t, EnsureRunningBeforeSend(context.Background(), svc, a, LaunchWaitOptions{}))
			assert.Equal(t, 0, svc.Calls())
		}
	})
	t.Run("launch fails", func(t *testing.T) {
		fastLaunchTimings(t)
		failed := &Agent{ID: "a1", Name: "alpha", Phase: "error", Message: "pull access denied",
			Launch: &AgentLaunch{ID: "l1", State: "ended", Error: "image_pull_failed", EndReason: "failed"}}
		svc := script(ok(failed))
		err := EnsureRunningBeforeSend(context.Background(), svc, launchingAgent(""), LaunchWaitOptions{})
		var nr *LaunchNotRunningError
		require.True(t, errors.As(err, &nr), "got %v", err)
		assert.Equal(t, "image_pull_failed", nr.Outcome.Code)
		assert.Equal(t, `agent "alpha" failed to start (image_pull_failed): pull access denied`, err.Error())
	})
	t.Run("launch runs", func(t *testing.T) {
		fastLaunchTimings(t)
		svc := script(ok(launchingAgent("image_pull")), ok(runningAgent()))
		require.NoError(t, EnsureRunningBeforeSend(context.Background(), svc, launchingAgent(""), LaunchWaitOptions{}))
		assert.Equal(t, 2, svc.Calls())
	})
	t.Run("wait error passes through", func(t *testing.T) {
		fastLaunchTimings(t)
		svc := script(ok(launchingAgent("image_pull")), httpErr(http.StatusNotFound))
		err := EnsureRunningBeforeSend(context.Background(), svc, launchingAgent(""), LaunchWaitOptions{})
		assert.ErrorIs(t, err, ErrAgentDeletedDuringLaunch)
	})
	t.Run("nil agent", func(t *testing.T) {
		assert.Error(t, EnsureRunningBeforeSend(context.Background(), script(), nil, LaunchWaitOptions{}))
	})
}

// Mutant: lifecycleStartBody always returns nil.
func TestStartRestartWithOptions_Body(t *testing.T) {
	type request struct{ method, path, body string }
	var mu sync.Mutex
	var got []request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, request{r.Method, r.URL.Path, string(b)})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"a1","name":"alpha","phase":"provisioning","launch":{"id":"l1","state":"active","active":true,"kind":"start"}}`))
	}))
	t.Cleanup(server.Close)
	client, err := New(server.URL)
	require.NoError(t, err)

	starter, isStarter := client.Agents().(LifecycleOptionsStarter)
	require.True(t, isStarter)
	_, isStarter = client.ProjectAgents("p1").(LifecycleOptionsStarter)
	require.True(t, isStarter)

	ctx := context.Background()
	resp, err := starter.StartWithOptions(ctx, "a1", LifecycleStartOptions{AcceptAsyncLaunch: true})
	require.NoError(t, err)
	require.NotNil(t, resp.Agent)
	assert.True(t, LaunchActive(resp.Agent))
	_, err = starter.RestartWithOptions(ctx, "a1", LifecycleStartOptions{AcceptAsyncLaunch: true})
	require.NoError(t, err)
	_, err = starter.StartWithOptions(ctx, "a1", LifecycleStartOptions{})
	require.NoError(t, err)
	_, err = starter.RestartWithOptions(ctx, "a1", LifecycleStartOptions{})
	require.NoError(t, err)

	require.Len(t, got, 4)
	for i, suffix := range []string{"/a1/start", "/a1/restart", "/a1/start", "/a1/restart"} {
		assert.Equal(t, http.MethodPost, got[i].method)
		assert.Regexp(t, suffix+"$", got[i].path)
	}
	assert.JSONEq(t, `{"acceptAsyncLaunch":true}`, got[0].body)
	assert.JSONEq(t, `{"acceptAsyncLaunch":true}`, got[1].body)
	assert.Empty(t, got[2].body, "zero options post no body, like Start")
	assert.Empty(t, got[3].body, "zero options post no body, like Restart")
}
