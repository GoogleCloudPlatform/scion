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
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Legacy port proxy requests (design §3.5, D5-1): a user request the
// port-forward tunnel serves is evaluated as a conduit port stream is
// before it is forwarded, and the in-process re-check can end it while it
// is open. Agent requests are not evaluated. No grant is minted.

// Decisions of legacyRecheckFixture's authorization hook.
const (
	legacyAllow int32 = iota
	legacyDeny
	legacyUnavailable
)

// legacyRecheckFixture is a conduit proxy fixture whose agent has no
// conduit session, only an older sciontool's port-forward tunnel, so its
// user requests take the legacy branch. Its app holds requests to /slow
// until released. decide sets the authorization hook's answer for every
// action (the evaluation's; authorizePortAccess still decides for real).
type legacyRecheckFixture struct {
	*conduitProxyFixture
	decide  atomic.Int32
	actions chan Action
	// afterTunnel, when set, runs right after the tunnel returns.
	afterTunnel atomic.Pointer[func()]

	mu      sync.Mutex
	entered chan struct{}
	release chan struct{}
}

func newLegacyRecheckFixture(t *testing.T, mod func(*Server)) *legacyRecheckFixture {
	t.Helper()
	lf := &legacyRecheckFixture{actions: make(chan Action, 64)}
	app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			lf.mu.Lock()
			entered, release := lf.entered, lf.release
			lf.mu.Unlock()
			close(entered)
			<-release
		}
		_, _ = io.WriteString(w, "hello "+r.URL.Path)
	})
	lf.conduitProxyFixture = newConduitProxyFixtureServer(t, app, func(s *Server) {
		s.conduitAuthorizeActionTestHook = func(_ context.Context, _ Identity, _ *store.Agent, a Action) error {
			select {
			case lf.actions <- a:
			default:
			}
			switch lf.decide.Load() {
			case legacyDeny:
				return errConduitForbidden
			case legacyUnavailable:
				return errConduitAuthzUnavailable
			}
			return nil
		}
		s.legacyPortTunnelReturnedTestHook = func() {
			if fn := lf.afterTunnel.Load(); fn != nil {
				(*fn)()
			}
		}
		if mod != nil {
			mod(s)
		}
	})
	startLegacyTunnel(t, lf.conduitProxyFixture, lf.srv)
	return lf
}

// gate arms the next /slow request: entered closes when it reaches the
// app, and closing release lets it answer (done at cleanup at the latest).
func (lf *legacyRecheckFixture) gate(t *testing.T) (entered <-chan struct{}, release func()) {
	t.Helper()
	e, r := make(chan struct{}), make(chan struct{})
	lf.mu.Lock()
	lf.entered, lf.release = e, r
	lf.mu.Unlock()
	var once sync.Once
	release = func() { once.Do(func() { close(r) }) }
	t.Cleanup(release)
	return e, release
}

// legacyResult is one proxied request's answer.
type legacyResult struct {
	status int
	header http.Header
	body   string
	err    error
}

// send proxies path to the agent's app as the agent's owner, or with
// bearer when set. It makes no assertions, so it runs in any goroutine.
func (lf *legacyRecheckFixture) send(path, bearer string, header http.Header) legacyResult {
	req, err := http.NewRequest(http.MethodGet,
		lf.base+"/api/v1/agents/"+lf.launched.ID+"/ports/"+strconv.Itoa(lf.app.port)+"/proxy"+path, nil)
	if err != nil {
		return legacyResult{err: err}
	}
	for k, v := range header {
		req.Header[k] = v
	}
	if bearer == "" {
		bearer = lf.userToken
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return legacyResult{err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return legacyResult{status: resp.StatusCode, header: resp.Header, body: string(body), err: err}
}

// sendAsync is send in a goroutine; the result arrives on the channel.
func (lf *legacyRecheckFixture) sendAsync(path string) <-chan legacyResult {
	ch := make(chan legacyResult, 1)
	go func() { ch <- lf.send(path, "", nil) }()
	return ch
}

// tracker is the node's in-process stream re-check.
func (lf *legacyRecheckFixture) tracker(t *testing.T) *conduitStreamAuthz {
	t.Helper()
	a := lf.srv.conduitAuthz.Load()
	require.NotNil(t, a, "the in-process re-check runs")
	return a
}

// drainActions returns the actions the hook decided since the last call.
func (lf *legacyRecheckFixture) drainActions() []Action {
	var out []Action
	for {
		select {
		case a := <-lf.actions:
			out = append(out, a)
		default:
			return out
		}
	}
}

// requireErrorCode checks a JSON error answer's status and code.
func requireErrorCode(t *testing.T, res legacyResult, status int, code string) ErrorResponse {
	t.Helper()
	require.NoError(t, res.err)
	require.Equal(t, status, res.status, res.body)
	var er ErrorResponse
	require.NoError(t, json.Unmarshal([]byte(res.body), &er), res.body)
	assert.Equal(t, code, er.Error.Code)
	return er
}

// waitResult returns the request's answer, failing after recheckWait.
func waitResult(t *testing.T, ch <-chan legacyResult) legacyResult {
	t.Helper()
	select {
	case res := <-ch:
		return res
	case <-time.After(recheckWait):
		t.Fatal("the request did not finish")
		return legacyResult{}
	}
}

// TestLegacyPortRequest_StartEvaluation (S1, S3): a user request on the
// legacy branch is evaluated before it is forwarded, for agent.port_access
// only; a deny is 403, an evaluation that cannot decide is 503 (never
// 502), and neither reaches the agent. An agent's request to its own port
// is not evaluated.
func TestLegacyPortRequest_StartEvaluation(t *testing.T) {
	lf := newLegacyRecheckFixture(t, nil)

	t.Run("allowed is served over the tunnel", func(t *testing.T) {
		lf.drainActions()
		res := lf.send("/a", "", nil)
		require.NoError(t, res.err)
		require.Equal(t, http.StatusOK, res.status, res.body)
		assert.Equal(t, "hello /a", res.body)
		assert.Equal(t, []Action{ActionPortAccess}, lf.drainActions(), "the evaluation decides agent.port_access only")
	})

	t.Run("denied is 403 and never forwarded", func(t *testing.T) {
		lf.decide.Store(legacyDeny)
		defer lf.decide.Store(legacyAllow)
		hits := lf.appHits()
		er := requireErrorCode(t, lf.send("/a", "", nil), http.StatusForbidden, ErrCodeForbidden)
		assert.Equal(t, "Access denied", er.Error.Message)
		assert.Equal(t, hits, lf.appHits(), "not forwarded")
	})

	t.Run("unavailable is 503, never 502, and never forwarded", func(t *testing.T) {
		lf.decide.Store(legacyUnavailable)
		defer lf.decide.Store(legacyAllow)
		hits := lf.appHits()
		er := requireErrorCode(t, lf.send("/a", "", nil), http.StatusServiceUnavailable, ErrCodeUnavailable)
		assert.Equal(t, legacyPortUnavailableMessage, er.Error.Message)

		res := lf.send("/a", "", http.Header{"Accept": {"text/html"}})
		require.NoError(t, res.err)
		assert.Equal(t, http.StatusServiceUnavailable, res.status)
		assert.Contains(t, res.header.Get("Content-Type"), "text/html")
		assert.Contains(t, res.body, legacyPortUnavailableMessage)
		assert.Equal(t, hits, lf.appHits(), "not forwarded")
	})

	t.Run("an agent reaching its own port is not evaluated", func(t *testing.T) {
		lf.decide.Store(legacyDeny)
		defer lf.decide.Store(legacyAllow)
		lf.drainActions()
		res := lf.send("/a", lf.agentToken(t, lf.launched), nil)
		require.NoError(t, res.err)
		require.Equal(t, http.StatusOK, res.status, res.body)
		assert.Empty(t, lf.drainActions(), "no evaluation for an agent")
	})
}

// TestLegacyPortRequest_PlacementRefused (S1): a pinned agent whose saved
// placement is inconsistent gets the placement refusal on the legacy
// branch, as a conduit open does, and the request is not forwarded.
func TestLegacyPortRequest_PlacementRefused(t *testing.T) {
	lf := newLegacyRecheckFixture(t, nil)
	ctx := context.Background()
	_, err := lf.store.SetAgentPinnedRuntimeTarget(ctx, lf.launched.ID,
		store.PinnedPlacement{RuntimeBrokerID: lf.launched.RuntimeBrokerID},
		store.PinnedPlacement{RuntimeBrokerID: lf.launched.RuntimeBrokerID, RuntimeTargetID: "target-1", RuntimeTargetType: "docker"})
	require.NoError(t, err)
	agent, err := lf.store.GetAgent(ctx, lf.launched.ID)
	require.NoError(t, err)
	_, perr := checkAgentPinnedPlacement(ctx, lf.store, agent)
	refusal, refused := placementRefusalOf(perr)
	require.True(t, refused, "the saved placement is inconsistent")

	hits := lf.appHits()
	requireErrorCode(t, lf.send("/a", "", nil), refusal.Status, refusal.Code)
	assert.Equal(t, hits, lf.appHits(), "not forwarded")
}

// TestLegacyPortRequest_EvaluationEntry (S1, S3): checkLegacyPortRequest
// is the shared evaluation of a tcp stream to the exposed port: a
// suspended user and a port that is no longer exposed are denied, a deny
// the store does not confirm is unavailable, and an inconsistent
// placement is unavailable with the placement refusal as its cause.
func TestLegacyPortRequest_EvaluationEntry(t *testing.T) {
	ctx := context.Background()
	stream := func(f *renewalFixture, port int) *conduitUserStream {
		return &conduitUserStream{
			Kind: grant.StreamKindTCP, Identity: f.user, AgentID: f.agent.ID, ProjectID: f.agent.ProjectID,
			Port: port, SessionID: legacyPortSessionPrefix + "t", LegacyPort: true,
		}
	}
	exposePort := func(t *testing.T, f *renewalFixture, port int) {
		t.Helper()
		require.NoError(t, f.store.UpdateAgentExposedPorts(ctx, f.agent.ID,
			[]store.ExposedPort{{Port: port, Host: conduitProxyHost, Mode: "rw"}}))
	}

	t.Run("allowed for an exposed port", func(t *testing.T) {
		f := newRenewalFixture(t)
		exposePort(t, f, 3000)
		verdict, err := f.srv.checkLegacyPortRequest(ctx, stream(f, 3000))
		assert.Equal(t, conduitAuthzAllowed, verdict)
		assert.NoError(t, err)
	})

	t.Run("a port no longer exposed is denied", func(t *testing.T) {
		f := newRenewalFixture(t)
		exposePort(t, f, 3000)
		verdict, err := f.srv.checkLegacyPortRequest(ctx, stream(f, 3001))
		assert.Equal(t, conduitAuthzDenied, verdict)
		assert.ErrorIs(t, err, errConduitForbidden)
	})

	t.Run("a suspended user is denied", func(t *testing.T) {
		f := newRenewalFixture(t)
		exposePort(t, f, 3000)
		setUserStatus(t, f.store, f.user.ID(), store.UserStatusSuspended)
		verdict, err := f.srv.checkLegacyPortRequest(ctx, stream(f, 3000))
		assert.Equal(t, conduitAuthzDenied, verdict)
		assert.ErrorIs(t, err, errConduitPrincipalInactive)
	})

	t.Run("a deny the store does not confirm is unavailable", func(t *testing.T) {
		f := newRenewalFixture(t)
		exposePort(t, f, 3000)
		f.srv.conduitAuthorizeActionTestHook = func(context.Context, Identity, *store.Agent, Action) error { return errConduitForbidden }
		verdict, _ := f.srv.checkLegacyPortRequest(ctx, stream(f, 3000))
		require.Equal(t, conduitAuthzDenied, verdict, "a confirmed deny")
		_, fault := installStoreFault(t, f.srv, func(inner store.Store, sw *storeFaultSwitch) *countingNilAgentStore {
			return &countingNilAgentStore{Store: inner, fault: sw}
		})
		fault.Arm()
		verdict, err := f.srv.checkLegacyPortRequest(ctx, stream(f, 3000))
		assert.Equal(t, conduitAuthzUnavailable, verdict)
		assert.ErrorIs(t, err, errConduitAuthzUnavailable)
	})

	t.Run("an inconsistent placement is unavailable with the refusal", func(t *testing.T) {
		f := newRenewalFixture(t)
		exposePort(t, f, 3000)
		pinInconsistently(t, f.conduitFixture)
		verdict, err := f.srv.checkLegacyPortRequest(ctx, stream(f, 3000))
		assert.Equal(t, conduitAuthzUnavailable, verdict)
		var pc *PlacementConflictError
		assert.True(t, errors.As(err, &pc), "the cause carries the placement refusal: %v", err)
	})
}

// TestLegacyPortRefusal_MatchesConduitProxy (D1, parity): each refusal of
// a legacy request is the conduit port proxy's answer for the same
// outcome: a deny as writeConduitProxyError writes it, a gone agent as
// authorizePortAccess answers an agent that does not exist, the placement
// refusal as writeConduitProxyError writes it; any other unavailable is
// 503 unavailable, never the proxy's 502.
func TestLegacyPortRefusal_MatchesConduitProxy(t *testing.T) {
	f := newConduitFixture(t)
	req := func(accept string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/agents/x/ports/3000/proxy/", nil)
		if accept != "" {
			r.Header.Set("Accept", accept)
		}
		return r
	}
	same := func(t *testing.T, want, got *httptest.ResponseRecorder) {
		t.Helper()
		assert.Equal(t, want.Code, got.Code)
		assert.JSONEq(t, want.Body.String(), got.Body.String())
	}
	placement := placementConflict(stalePinRefusal(f.agent, "the pin is incomplete"))

	t.Run("denied", func(t *testing.T) {
		want, got := httptest.NewRecorder(), httptest.NewRecorder()
		writeConduitProxyError(want, req(""), f.agent.ID, errConduitForbidden)
		writeLegacyPortRefusal(got, req(""), f.agent.ID, conduitAuthzDenied, errConduitPrincipalInactive)
		same(t, want, got)
		assert.Equal(t, http.StatusForbidden, got.Code)
	})

	t.Run("target gone", func(t *testing.T) {
		want, got := httptest.NewRecorder(), httptest.NewRecorder()
		_, ok := f.srv.authorizePortAccess(want, req(""), tid("no-such-agent"), ActionPortAccess)
		require.False(t, ok)
		writeLegacyPortRefusal(got, req(""), f.agent.ID, conduitAuthzTargetGone, errConduitAgentGone)
		same(t, want, got)
		assert.Equal(t, http.StatusNotFound, got.Code)
	})

	t.Run("placement", func(t *testing.T) {
		want, got := httptest.NewRecorder(), httptest.NewRecorder()
		writeConduitProxyError(want, req(""), f.agent.ID, placement)
		writeLegacyPortRefusal(got, req(""), f.agent.ID, conduitAuthzUnavailable,
			errors.Join(errConduitAuthzUnavailable, errConduitRenewalUnavailable, placement))
		same(t, want, got)
		assert.Equal(t, placement.Refusal.Status, got.Code)
	})

	t.Run("other unavailable is 503", func(t *testing.T) {
		for _, cause := range []error{errConduitAuthzUnavailable, errConduitUserStatusUnavailable, errors.New("anything")} {
			got := httptest.NewRecorder()
			writeLegacyPortRefusal(got, req(""), f.agent.ID, conduitAuthzUnavailable, cause)
			require.Equal(t, http.StatusServiceUnavailable, got.Code, "cause %v", cause)
			var er ErrorResponse
			require.NoError(t, json.Unmarshal(got.Body.Bytes(), &er))
			assert.Equal(t, ErrCodeUnavailable, er.Error.Code)
			assert.Equal(t, legacyPortUnavailableMessage, er.Error.Message)
		}
		got := httptest.NewRecorder()
		writeLegacyPortRefusal(got, req("text/html"), f.agent.ID, conduitAuthzUnavailable, errConduitAuthzUnavailable)
		assert.Equal(t, http.StatusServiceUnavailable, got.Code)
		assert.Contains(t, got.Header().Get("Content-Type"), "text/html")
	})
}

// TestLegacyPortRequest_ReCheck (S2): a legacy request is tracked by the
// in-process re-check while it is open; a re-check that denies it ends it
// with 403 (a port no longer exposed too), one that cannot decide leaves
// it open, and no grant is signed for it. Where no re-check runs, the
// request is still served.
func TestLegacyPortRequest_ReCheck(t *testing.T) {
	ctx := context.Background()
	var signing *countingGrantKeyStore
	lf := newLegacyRecheckFixture(t, func(s *Server) { signing = countSigning(s) })
	a := lf.tracker(t)

	t.Run("tracked while open; unavailable leaves it open", func(t *testing.T) {
		entered, release := lf.gate(t)
		done := lf.sendAsync("/slow")
		<-entered
		require.Equal(t, 1, a.Len(), "the open request is tracked")

		signing.arm(lf.srv)
		lf.decide.Store(legacyUnavailable)
		a.Recheck(ctx, conduitAuthzTriggerSweep, conduitAuthzMatch{})
		lf.decide.Store(legacyAllow)
		select {
		case res := <-done:
			t.Fatalf("an unavailable re-check ended the request: %d %s", res.status, res.body)
		case <-time.After(200 * time.Millisecond):
		}
		assert.Equal(t, 1, a.Len())

		release()
		res := waitResult(t, done)
		require.NoError(t, res.err)
		require.Equal(t, http.StatusOK, res.status, res.body)
		assert.Equal(t, "hello /slow", res.body)
		assert.Eventually(t, func() bool { return a.Len() == 0 }, recheckWait, 10*time.Millisecond, "untracked when it ends")
		assert.Zero(t, signing.reads.Load(), "no grant is signed for a legacy request")
	})

	t.Run("a deny ends it with 403", func(t *testing.T) {
		entered, release := lf.gate(t)
		defer release()
		done := lf.sendAsync("/slow")
		<-entered
		signing.arm(lf.srv)
		lf.decide.Store(legacyDeny)
		a.Recheck(ctx, conduitAuthzTriggerSweep, conduitAuthzMatch{})
		lf.decide.Store(legacyAllow)
		er := requireErrorCode(t, waitResult(t, done), http.StatusForbidden, ErrCodeForbidden)
		assert.Equal(t, "Access denied", er.Error.Message)
		assert.Eventually(t, func() bool { return a.Len() == 0 }, recheckWait, 10*time.Millisecond)
		assert.Zero(t, signing.reads.Load(), "no grant is signed for a legacy request")
	})

	t.Run("a port no longer exposed ends it", func(t *testing.T) {
		entered, release := lf.gate(t)
		defer release()
		done := lf.sendAsync("/slow")
		<-entered
		require.NoError(t, lf.store.UpdateAgentExposedPorts(ctx, lf.launched.ID, nil))
		t.Cleanup(func() {
			_ = lf.store.UpdateAgentExposedPorts(ctx, lf.launched.ID,
				[]store.ExposedPort{{Port: lf.app.port, Host: conduitProxyHost, Mode: "rw"}})
		})
		a.Recheck(ctx, conduitAuthzTriggerNotify, conduitAuthzMatch{AgentID: lf.launched.ID})
		requireErrorCode(t, waitResult(t, done), http.StatusForbidden, ErrCodeForbidden)
	})
}

// TestLegacyPortRequest_ServedWithoutReCheck (S2): where conduit is off
// and this node runs no in-process re-check, a user request is evaluated
// at its start and served; nothing tracks it.
func TestLegacyPortRequest_ServedWithoutReCheck(t *testing.T) {
	lf := newLegacyRecheckFixture(t, nil)
	a := lf.tracker(t)
	setConduitExperiment(t, lf.srv, false)
	require.False(t, lf.srv.conduitRoutable(), "conduit is off")
	lf.srv.conduitAuthz.Store(nil)
	t.Cleanup(func() { lf.srv.conduitAuthz.Store(a) })
	res := lf.send("/a", "", nil)
	require.NoError(t, res.err)
	require.Equal(t, http.StatusOK, res.status, res.body)
	assert.Equal(t, 0, a.Len())

	lf.decide.Store(legacyDeny)
	requireErrorCode(t, lf.send("/a", "", nil), http.StatusForbidden, ErrCodeForbidden)
}

// TestLegacyPortRequest_DenyAfterTunnelAnswered (D2): a re-check that
// denies the request after the tunnel answered, before the answer is
// written, gives 403 and drops the agent's answer.
func TestLegacyPortRequest_DenyAfterTunnelAnswered(t *testing.T) {
	ctx := context.Background()
	lf := newLegacyRecheckFixture(t, nil)
	a := lf.tracker(t)
	fn := func() {
		lf.decide.Store(legacyDeny)
		a.Recheck(ctx, conduitAuthzTriggerSweep, conduitAuthzMatch{})
	}
	lf.afterTunnel.Store(&fn)
	hits := lf.appHits()
	res := lf.send("/late", "", nil)
	er := requireErrorCode(t, res, http.StatusForbidden, ErrCodeForbidden)
	assert.Equal(t, "Access denied", er.Error.Message)
	assert.NotContains(t, res.body, "hello", "the agent's answer is dropped")
	assert.Equal(t, hits+1, lf.appHits(), "the agent answered before the deny")
}

// TestLegacyPortRequest_InProcessEnforceUnchanged (S4, D5-2): in an
// in-process profile with target_stream_deadlines enforce, a legacy
// request is still served; only external relay mode with enforce refuses
// the legacy branch.
func TestLegacyPortRequest_InProcessEnforceUnchanged(t *testing.T) {
	lf := newLegacyRecheckFixture(t, enforceTargetDeadlines)
	res := lf.send("/a", "", nil)
	require.NoError(t, res.err)
	require.Equal(t, http.StatusOK, res.status, res.body)
	assert.Equal(t, "hello /a", res.body)
}
