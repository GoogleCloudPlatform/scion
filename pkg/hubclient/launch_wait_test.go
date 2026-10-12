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
	settle := launchFailureSettle
	t.Cleanup(func() {
		launchPollInterval, launchWaitSlack, launchWaitFallback = poll, slack, fallback
		launchUnreadableGrace, launchUnreadableBackoff, launchUnreadableBackoffMax = grace, backoff, backoffMax
		launchFailureSettle = settle
	})
	launchFailureSettle = 30 * time.Millisecond
	launchPollInterval = 5 * time.Millisecond
	launchWaitSlack = 50 * time.Millisecond
	launchWaitFallback = 400 * time.Millisecond
	launchUnreadableGrace = 60 * time.Millisecond
	launchUnreadableBackoff = 5 * time.Millisecond
	launchUnreadableBackoffMax = 10 * time.Millisecond
}

// getResult is one scripted answer to GET agent; fn, when set, answers
// instead of agent and err.
type getResult struct {
	agent *Agent
	err   error
	fn    func(ctx context.Context) (*Agent, error)
}

// scriptedGetService answers Get from a script; the last entry repeats.
type scriptedGetService struct {
	AgentService
	mu     sync.Mutex
	script []getResult
	calls  int
	ids    []string
}

func (s *scriptedGetService) Get(ctx context.Context, agentID string) (*Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.calls
	s.calls++
	s.ids = append(s.ids, agentID)
	if len(s.script) == 0 {
		return nil, errors.New("no script")
	}
	if i >= len(s.script) {
		i = len(s.script) - 1
	}
	r := s.script[i]
	if r.fn != nil {
		s.mu.Unlock()
		defer s.mu.Lock()
		return r.fn(ctx)
	}
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
		assert.ErrorIs(t, err, ErrLaunchNotRunning)
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

// Mutant: drop the whole "ended launch" clause in launchDone. A stop during
// the launch ends it in phase stopping with launch_stopped/failed; only the
// ended-launch rule ends the wait there.
func TestWaitForLaunch_StoppingWithStoppedLaunch(t *testing.T) {
	fastLaunchTimings(t)
	stopping := &Agent{ID: "a1", Name: "alpha", Phase: "stopping",
		Launch: &AgentLaunch{ID: "l1", State: "ended", Kind: "create", Error: "launch_stopped", EndReason: "failed"}}
	svc := script(ok(launchingAgent("image_pull")), ok(stopping))

	out, err := WaitForLaunch(context.Background(), svc, "a1", launchingAgent(""), LaunchWaitOptions{})
	require.NoError(t, err)
	assert.False(t, out.Running)
	assert.Equal(t, "stopping", out.Phase)
	assert.Equal(t, "launch_stopped", out.Code)
	assert.Equal(t, "failed", out.EndReason)
}

// fakeLaunchClock replaces launchNow and launchAfter (the clock of the
// unreadable-status retry). A blocked clock never fires; onBlock runs when
// a delay is requested on it.
type fakeLaunchClock struct {
	mu      sync.Mutex
	now     time.Time
	delays  []time.Duration
	blocked bool
	onBlock func()
}

func installFakeLaunchClock(t *testing.T) *fakeLaunchClock {
	t.Helper()
	c := &fakeLaunchClock{now: time.Unix(1_700_000_000, 0)}
	origNow, origAfter := launchNow, launchAfter
	t.Cleanup(func() { launchNow, launchAfter = origNow, origAfter })
	launchNow = func() time.Time {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.now
	}
	launchAfter = func(d time.Duration) <-chan time.Time {
		c.mu.Lock()
		c.delays = append(c.delays, d)
		if c.blocked {
			onBlock := c.onBlock
			c.mu.Unlock()
			if onBlock != nil {
				onBlock()
			}
			return make(chan time.Time) // never fires
		}
		c.now = c.now.Add(d)
		ch := make(chan time.Time, 1)
		ch <- c.now
		c.mu.Unlock()
		return ch
	}
	return c
}

func (c *fakeLaunchClock) recorded() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.delays...)
}

// waitGuarded runs WaitForLaunch and fails the test if it does not return
// within 5 s.
func waitGuarded(t *testing.T, ctx context.Context, svc AgentService, start *Agent, opts LaunchWaitOptions) (LaunchOutcome, time.Duration, error) {
	t.Helper()
	type result struct {
		out LaunchOutcome
		err error
	}
	done := make(chan result, 1)
	began := time.Now()
	go func() {
		out, err := WaitForLaunch(ctx, svc, "a1", start, opts)
		done <- result{out, err}
	}()
	select {
	case r := <-done:
		return r.out, time.Since(began), r.err
	case <-time.After(5 * time.Second):
		t.Fatal("the wait did not return")
		return LaunchOutcome{}, 0, nil
	}
}

// Mutant: the retry delay is not cut at the grace (min(delay, remaining)
// becomes delay).
func TestWaitForLaunch_UnreadableRetryBackoff(t *testing.T) {
	// The production timings: the delays double from 250ms up to 2s, and
	// the last one is cut short so the retries span exactly the 5s grace.
	clock := installFakeLaunchClock(t)
	svc := script(httpErr(http.StatusNotFound))

	_, _, err := waitGuarded(t, context.Background(), svc, launchingAgent(""),
		LaunchWaitOptions{AgentCaller: true, Timeout: time.Minute, PollInterval: time.Millisecond})
	var unreadable *LaunchStatusUnreadableError
	require.ErrorAs(t, err, &unreadable)
	want := []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second, 1250 * time.Millisecond}
	assert.Equal(t, want, clock.recorded())
	assert.Equal(t, 1+len(want), svc.Calls(), "the first read and one per delay")
}

// Mutant: drop the retry-cut-short return before any read (an interrupt
// during the retry is then reported as an unreadable status).
func TestWaitForLaunch_InterruptDuringUnreadableRetry(t *testing.T) {
	fastLaunchTimings(t)
	clock := installFakeLaunchClock(t)
	launchUnreadableGrace = time.Hour
	cause := errors.New("received terminated")
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	clock.blocked = true
	clock.onBlock = func() { cancel(cause) }
	svc := script(httpErr(http.StatusNotFound))

	_, elapsed, err := waitGuarded(t, ctx, svc, launchingAgent(""), LaunchWaitOptions{AgentCaller: true, Timeout: time.Hour})
	assert.ErrorIs(t, err, context.Canceled)
	assert.ErrorIs(t, err, cause)
	var unreadable *LaunchStatusUnreadableError
	assert.False(t, errors.As(err, &unreadable), "got %v", err)
	assert.Less(t, elapsed, 2*time.Second)
	assert.Equal(t, 1, svc.Calls(), "no read after the interrupt")
}

// Mutant: (ctx.Err() != nil || opts.AgentCaller) becomes (ctx.Err() != nil):
// an agent caller whose budget ends during the retry then gets an
// unreadable status instead of a timeout.
func TestWaitForLaunch_TimeoutDuringUnreadableRetry(t *testing.T) {
	withRemaining := launchingAgent("")
	withRemaining.Launch.RemainingSeconds = intPtr(0)
	cases := map[string]struct {
		start *Agent
		opts  LaunchWaitOptions
	}{
		"explicit timeout":  {launchingAgent(""), LaunchWaitOptions{AgentCaller: true, Timeout: 50 * time.Millisecond}},
		"advertised budget": {withRemaining, LaunchWaitOptions{AgentCaller: true}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fastLaunchTimings(t)
			clock := installFakeLaunchClock(t)
			launchUnreadableGrace = time.Hour
			clock.blocked = true
			svc := script(httpErr(http.StatusNotFound))

			_, elapsed, err := waitGuarded(t, context.Background(), svc, tc.start, tc.opts)
			assert.ErrorIs(t, err, ErrLaunchWaitTimeout)
			assert.Less(t, elapsed, 2*time.Second)
		})
	}
}

// Mutant: in the after-read confirmation, ctx.Err() becomes fetchCtx.Err():
// a not-found lasting to the end of the budget is then reported as a
// timeout instead of a deletion (user) or an unreadable status (agent).
func TestWaitForLaunch_BudgetEndsDuringConfirmation(t *testing.T) {
	for _, agentCaller := range []bool{false, true} {
		name := "user"
		if agentCaller {
			name = "agent"
		}
		t.Run(name, func(t *testing.T) {
			fastLaunchTimings(t)
			clock := installFakeLaunchClock(t)
			launchUnreadableGrace = time.Hour
			clock.blocked = true
			svc := script(ok(launchingAgent("image_pull")), httpErr(http.StatusNotFound))

			_, elapsed, err := waitGuarded(t, context.Background(), svc, launchingAgent(""),
				LaunchWaitOptions{AgentCaller: agentCaller, Timeout: 50 * time.Millisecond})
			assert.NotErrorIs(t, err, ErrLaunchWaitTimeout)
			if agentCaller {
				var unreadable *LaunchStatusUnreadableError
				require.ErrorAs(t, err, &unreadable)
				assert.True(t, unreadable.AfterRead)
			} else {
				assert.ErrorIs(t, err, ErrAgentDeletedDuringLaunch)
			}
			assert.Less(t, elapsed, 2*time.Second)
		})
	}
}

// Mutant: drop the after-read confirmation's interrupt return (an interrupt
// is then reported as a deletion or an unreadable status).
func TestWaitForLaunch_InterruptDuringDeletionConfirmation(t *testing.T) {
	for _, agentCaller := range []bool{false, true} {
		name := "user"
		if agentCaller {
			name = "agent"
		}
		t.Run(name, func(t *testing.T) {
			fastLaunchTimings(t)
			clock := installFakeLaunchClock(t)
			launchUnreadableGrace = time.Hour
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			clock.blocked = true
			clock.onBlock = cancel
			svc := script(ok(launchingAgent("image_pull")), httpErr(http.StatusNotFound))

			_, _, err := waitGuarded(t, ctx, svc, launchingAgent(""), LaunchWaitOptions{AgentCaller: agentCaller, Timeout: time.Hour})
			assert.ErrorIs(t, err, context.Canceled)
			assert.NotErrorIs(t, err, ErrAgentDeletedDuringLaunch)
			var unreadable *LaunchStatusUnreadableError
			assert.False(t, errors.As(err, &unreadable), "got %v", err)
		})
	}
}

// Mutant: the retry-cut-short condition becomes true for every caller: a
// user caller's definite 404/403 is then reported as a timeout.
func TestWaitForLaunch_UserCallerBudgetEndsDuringRetry(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			fastLaunchTimings(t)
			clock := installFakeLaunchClock(t)
			launchUnreadableGrace = time.Hour
			clock.blocked = true
			svc := script(httpErr(status))

			_, _, err := waitGuarded(t, context.Background(), svc, launchingAgent(""), LaunchWaitOptions{Timeout: 50 * time.Millisecond})
			assert.NotErrorIs(t, err, ErrLaunchWaitTimeout)
			if status == http.StatusNotFound {
				assert.ErrorIs(t, err, ErrAgentDeletedDuringLaunch)
			} else {
				var rejected *LaunchStatusRejectedError
				assert.ErrorAs(t, err, &rejected)
			}
		})
	}
}

// Mutant: drop the "read cut short by ctx" return in retryUnreadableStatus:
// the cut-short read's deadline error then replaces the 404, and a user
// caller's deletion is reported as a timeout.
func TestWaitForLaunch_RetryReadCutShortKeepsNotFound(t *testing.T) {
	fastLaunchTimings(t)
	installFakeLaunchClock(t)
	launchUnreadableGrace = time.Hour
	blockUntilDone := getResult{fn: func(ctx context.Context) (*Agent, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	svc := script(httpErr(http.StatusNotFound), blockUntilDone)

	_, _, err := waitGuarded(t, context.Background(), svc, launchingAgent(""), LaunchWaitOptions{Timeout: 50 * time.Millisecond})
	assert.ErrorIs(t, err, ErrAgentDeletedDuringLaunch)
	assert.NotErrorIs(t, err, ErrLaunchWaitTimeout)
}

// Mutant: ignore opts.Timeout (use the derived budget).
func TestWaitForLaunch_ExplicitTimeout(t *testing.T) {
	fastLaunchTimings(t)
	start := launchingAgent("")
	start.Launch.RemainingSeconds = intPtr(3600)
	svc := script(ok(launchingAgent("image_pull")))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, elapsed, err := waitGuarded(t, ctx, svc, start, LaunchWaitOptions{Timeout: 30 * time.Millisecond})
	assert.ErrorIs(t, err, ErrLaunchWaitTimeout)
	assert.Less(t, elapsed, time.Second)
}

// Mutant: ignore opts.PollInterval (use launchPollInterval).
func TestWaitForLaunch_PollInterval(t *testing.T) {
	fastLaunchTimings(t)
	launchPollInterval = time.Hour
	svc := script(ok(launchingAgent("image_pull")), ok(runningAgent()))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	out, _, err := waitGuarded(t, ctx, svc, launchingAgent(""), LaunchWaitOptions{PollInterval: time.Millisecond})
	require.NoError(t, err)
	assert.True(t, out.Running)
}

// Mutant: end the wait at once on a failed phase with an active launch (no
// settle), so the code from the launch-ending write is not reported.
func TestWaitForLaunch_ErrorPhaseActiveLaunchWaitsForCode(t *testing.T) {
	fastLaunchTimings(t)
	launchFailureSettle = time.Second
	errActive := &Agent{ID: "a1", Name: "alpha", Phase: "error",
		Launch: &AgentLaunch{ID: "l1", State: "active", Active: true, Kind: "create", Step: "container_start"}}
	errEnded := &Agent{ID: "a1", Name: "alpha", Phase: "error", Message: "exited 1",
		Launch: &AgentLaunch{ID: "l1", State: "ended", Kind: "create", Step: "container_start", Error: "container_exited", EndReason: "failed"}}
	svc := script(ok(errActive), ok(errActive), ok(errEnded))

	out, err := WaitForLaunch(context.Background(), svc, "a1", launchingAgent(""), LaunchWaitOptions{})
	require.NoError(t, err)
	assert.False(t, out.Running)
	assert.Equal(t, "container_exited", out.Code)
	assert.Equal(t, "exited 1", out.Message)
	assert.Equal(t, 3, svc.Calls())
}

// Mutant: the settle never ends (the comparison is always false), so the
// wait runs to its budget.
func TestWaitForLaunch_ErrorPhaseActiveLaunchSettleEnds(t *testing.T) {
	fastLaunchTimings(t)
	launchWaitFallback = 10 * time.Second
	errActive := &Agent{ID: "a1", Name: "alpha", Phase: "error",
		Launch: &AgentLaunch{ID: "l1", State: "active", Active: true, Kind: "create"}}
	svc := script(ok(errActive))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	out, _, err := waitGuarded(t, ctx, svc, launchingAgent(""), LaunchWaitOptions{})
	require.NoError(t, err)
	assert.False(t, out.Running)
	assert.Equal(t, "error", out.Phase)
	assert.Empty(t, out.Code)
}

// Mutant: drop the settle check on budget expiry, so a failure seen during
// the settle is reported as a timeout.
func TestWaitForLaunch_BudgetEndsDuringFailureSettle(t *testing.T) {
	fastLaunchTimings(t)
	launchFailureSettle = time.Hour
	errActive := &Agent{ID: "a1", Name: "alpha", Phase: "error",
		Launch: &AgentLaunch{ID: "l1", State: "active", Active: true, Kind: "create"}}
	svc := script(ok(errActive))

	out, _, err := waitGuarded(t, context.Background(), svc, launchingAgent(""), LaunchWaitOptions{Timeout: 50 * time.Millisecond})
	require.NoError(t, err)
	assert.False(t, out.Running)
	assert.Equal(t, "error", out.Phase)
}

// Mutant: EnsureRunningBeforeSendByID returns after the read without
// waiting.
func TestEnsureRunningBeforeSendByID(t *testing.T) {
	t.Run("not launching: one read", func(t *testing.T) {
		fastLaunchTimings(t)
		svc := script(ok(runningAgent()))
		require.NoError(t, EnsureRunningBeforeSendByID(context.Background(), svc, "alpha", LaunchWaitOptions{}))
		assert.Equal(t, 1, svc.Calls())
	})
	t.Run("launching then running: waits by agent ID", func(t *testing.T) {
		fastLaunchTimings(t)
		svc := script(ok(launchingAgent("image_pull")), ok(runningAgent()))
		require.NoError(t, EnsureRunningBeforeSendByID(context.Background(), svc, "alpha", LaunchWaitOptions{}))
		assert.Equal(t, []string{"alpha", "a1"}, svc.ids)
	})
	t.Run("launch fails", func(t *testing.T) {
		fastLaunchTimings(t)
		failed := &Agent{ID: "a1", Name: "alpha", Phase: "error", Message: "pull access denied",
			Launch: &AgentLaunch{ID: "l1", State: "ended", Error: "image_pull_failed", EndReason: "failed"}}
		svc := script(ok(launchingAgent("image_pull")), ok(failed))
		err := EnsureRunningBeforeSendByID(context.Background(), svc, "alpha", LaunchWaitOptions{})
		assert.ErrorIs(t, err, ErrLaunchNotRunning)
		var nr *LaunchNotRunningError
		require.ErrorAs(t, err, &nr)
		assert.Equal(t, "image_pull_failed", nr.Outcome.Code)
	})
	t.Run("read fails", func(t *testing.T) {
		fastLaunchTimings(t)
		svc := script(httpErr(http.StatusNotFound))
		err := EnsureRunningBeforeSendByID(context.Background(), svc, "alpha", LaunchWaitOptions{})
		assert.True(t, apiclient.IsNotFoundError(err), "got %v", err)
		assert.Equal(t, 1, svc.Calls())
	})
}
