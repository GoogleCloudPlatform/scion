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
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

const refreshWait = 10 * time.Second

// agentRefreshCounter wires the agent refresh counter on s and returns a
// reader of its count for an outcome.
func agentRefreshCounter(t *testing.T, s *Server) func(outcome string) int64 {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	rec, err := NewOTelConduitAgentRefreshMetrics(mp)
	require.NoError(t, err)
	s.SetConduitAgentRefreshMetrics(rec)
	return func(outcome string) int64 {
		var rm metricdata.ResourceMetrics
		require.NoError(t, reader.Collect(context.Background(), &rm))
		var total int64
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name != "scion.hub.conduit.agent_refresh" {
					continue
				}
				sum, ok := m.Data.(metricdata.Sum[int64])
				require.True(t, ok, "counter data %T", m.Data)
				for _, dp := range sum.DataPoints {
					if v, ok := dp.Attributes.Value("outcome"); ok && v.AsString() == outcome {
						total += dp.Value
					}
				}
			}
		}
		return total
	}
}

// userStreamDeadlines returns the authorization deadline of every user
// stream the node tracks.
func userStreamDeadlines(t *testing.T, s *Server) []time.Time {
	t.Helper()
	a := s.conduitAuthz.Load()
	require.NotNil(t, a)
	a.mu.Lock()
	streams := make([]*conduitUserStream, 0, len(a.streams))
	for st := range a.streams {
		streams = append(streams, st)
	}
	a.mu.Unlock()
	out := make([]time.Time, 0, len(streams))
	for _, st := range streams {
		out = append(out, st.Deadline())
	}
	return out
}

// TestConduitAgentRefresh_KeepsSessionAndStreams (issue acceptance 1 and
// 2): a valid AuthRefresh from the agent before its token expires is
// accepted; the session stays up, a user's port proxy stream open on it
// carries on undisturbed, and that stream's authorization deadline is
// unchanged.
func TestConduitAgentRefresh_KeepsSessionAndStreams(t *testing.T) {
	part1, cont := make(chan struct{}), make(chan struct{})
	app := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "part1;")
		w.(http.Flusher).Flush()
		close(part1)
		<-cont
		_, _ = io.WriteString(w, "part2")
	})
	f := newConduitProxyFixture(t, app)
	refreshes := agentRefreshCounter(t, f.srv)
	f.startAgent(t)

	resp := f.get(t, "/stream", nil)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	<-part1
	buf := make([]byte, len("part1;"))
	_, err := io.ReadFull(resp.Body, buf)
	require.NoError(t, err)
	require.Equal(t, "part1;", string(buf))

	before := userStreamDeadlines(t, f.srv)
	require.Len(t, before, 1, "the proxied request is one tracked user stream")
	require.False(t, before[0].IsZero(), "the user stream has an authorization deadline")

	require.NoError(t, f.sciontool.RefreshAuth(f.agentToken(t, f.launched), 1))
	require.Eventually(t, func() bool { return refreshes(conduitRefreshOK) == 1 }, refreshWait, 10*time.Millisecond,
		"the hub did not accept the agent's AuthRefresh")
	assert.Zero(t, refreshes(conduitRefreshRefused))
	assert.Equal(t, before, userStreamDeadlines(t, f.srv), "an agent AuthRefresh moves no user stream's deadline")

	close(cont)
	rest, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "part2", string(rest), "the stream carried on after the refresh")
	assert.Equal(t, int64(1), f.sessions.Load(), "the agent kept its session")
}

// refreshSession dials an agent session on f with the agent's own token
// and current run.
func refreshSession(t *testing.T, f *relayFixture) conduit.LocalSession {
	t.Helper()
	ls, _, err := f.dial(t, f.agentToken(t, f.launched), f.launched.RunID)
	require.NoError(t, err)
	return ls
}

// requireClosedWith waits for ls to end and checks its close code.
func requireClosedWith(t *testing.T, ls conduit.LocalSession, code uint32) {
	t.Helper()
	select {
	case <-ls.Done():
	case <-time.After(refreshWait):
		t.Fatal("session still open")
	}
	var ce *conduit.CloseError
	require.ErrorAs(t, ls.Err(), &ce)
	assert.Equal(t, code, ce.Code, "close: %v", ls.Err())
}

// TestConduitAgentRefresh_RefusalsClose4401: a refreshed credential that
// is invalid, expired, revoked, missing the conduit scope, issued for
// another agent or another project, issued for a stale run, or presented
// for a deleted agent closes the session with 4401.
func TestConduitAgentRefresh_RefusalsClose4401(t *testing.T) {
	for _, tc := range []struct {
		name string
		// token returns the credential to present; it may change state.
		token func(t *testing.T, f *relayFixture) string
		// enforce turns run-scope enforcement on before the session.
		enforce bool
	}{
		{name: "not a token", token: func(*testing.T, *relayFixture) string { return "not-a-token" }},
		{name: "expired", token: func(t *testing.T, f *relayFixture) string {
			svc, err := NewAgentTokenService(AgentTokenConfig{SigningKey: f.srv.agentTokenService.config.SigningKey, TokenDuration: -time.Hour})
			require.NoError(t, err)
			tok, err := svc.GenerateAgentToken(f.launched.ID, f.launched.ProjectID, ScopesForRole(AgentRoleFull), nil)
			require.NoError(t, err)
			return tok
		}},
		{name: "another agent", token: func(t *testing.T, f *relayFixture) string { return f.agentToken(t, f.agent) }},
		{name: "another project", token: func(t *testing.T, f *relayFixture) string {
			tok, err := f.srv.GenerateAgentToken(f.launched.ID, tid("other-project"), nil, AgentRoleFull, nil)
			require.NoError(t, err)
			return tok
		}},
		{name: "without agent:port:forward", token: func(t *testing.T, f *relayFixture) string {
			tok, err := f.srv.agentTokenService.GenerateAgentToken(f.launched.ID, f.launched.ProjectID, []AgentTokenScope{ScopeAgentStatusUpdate}, nil)
			require.NoError(t, err)
			return tok
		}},
		{name: "revoked", token: func(t *testing.T, f *relayFixture) string {
			tok := f.agentToken(t, f.launched)
			claims, err := f.srv.agentTokenService.ValidateAgentToken(tok)
			require.NoError(t, err)
			ctx := context.Background()
			cred, err := f.store.GetAgentCredentialByJTIHash(ctx, hashJTI(claims.ID))
			require.NoError(t, err)
			require.NoError(t, f.store.RevokeAgentCredential(ctx, cred.ID, "test", "explicit"))
			return tok
		}},
		{name: "agent deleted", token: func(t *testing.T, f *relayFixture) string {
			tok := f.agentToken(t, f.launched)
			a, err := f.store.GetAgent(context.Background(), f.launched.ID)
			require.NoError(t, err)
			a.DeletedAt = time.Now()
			require.NoError(t, f.store.UpdateAgent(context.Background(), a))
			return tok
		}},
		{name: "agent held", token: func(t *testing.T, f *relayFixture) string {
			f.srv.authConfig.HoldStore = fixedHoldStore{AgentHoldStore: f.store, held: true}
			return f.agentToken(t, f.launched)
		}},
		{name: "stale run, enforced", enforce: true, token: func(t *testing.T, f *relayFixture) string {
			return signRunToken(t, f.srv, f.store, f.launched, "stale-run", recordRun("stale-run"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRelayFixture(t, nil)
			refreshes := agentRefreshCounter(t, f.srv)
			admit := f.agentToken(t, f.launched)
			if tc.enforce {
				f.srv.authConfig.AgentRunScope = testRunScopeChecker(agentRunScopeEnforce, time.Time{}, f.store)
				f.public.Close()
				f.public = httptest.NewServer(f.srv.Handler())
				t.Cleanup(f.public.Close)
				admit = signRunToken(t, f.srv, f.store, f.launched, f.launched.RunID, recordRun(f.launched.RunID))
			}
			ls, _, err := f.dial(t, admit, f.launched.RunID)
			require.NoError(t, err)

			require.NoError(t, ls.RefreshAuth([]byte(tc.token(t, f)), 0))
			requireClosedWith(t, ls, conduit.CloseUnauthenticated)
			assert.Equal(t, int64(1), refreshes(conduitRefreshRefused))
			assert.Zero(t, refreshes(conduitRefreshOK))
		})
	}
}

// TestConduitAgentRefresh_SessionRunMismatchEnforced: the refreshed
// token's run is compared with the session's endpoint incarnation, not
// with the agent row's launch id or the run of the token that opened the
// session. A mismatch closes the session with 4401 when run-scope
// enforcement is on, and is only logged under observe.
func TestConduitAgentRefresh_SessionRunMismatchEnforced(t *testing.T) {
	f := newRelayFixture(t, nil)
	run := f.launched.RunID
	for _, tc := range []struct {
		name string
		mode agentRunScopeMode
		// launchID and tokenRun are the principal's admission facts:
		// the agent row's launch id and the opening token's run.
		launchID, tokenRun string
		// incarnation is the session's endpoint incarnation.
		incarnation string
		want4401    bool
	}{
		{name: "enforce, session on another run, admission facts on the token's run", mode: agentRunScopeEnforce,
			launchID: run, tokenRun: run, incarnation: "another-run", want4401: true},
		{name: "enforce, session on the token's run, admission facts stale", mode: agentRunScopeEnforce,
			launchID: "stale-run", tokenRun: "stale-run", incarnation: run},
		{name: "enforce, session admitted as gen-N", mode: agentRunScopeEnforce,
			launchID: run, tokenRun: run, incarnation: "gen-3", want4401: true},
		{name: "observe, session on another run", mode: agentRunScopeObserve,
			launchID: run, tokenRun: run, incarnation: "another-run"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.srv.authConfig.AgentRunScope = testRunScopeChecker(tc.mode, time.Time{}, f.store)
			tok := signRunToken(t, f.srv, f.store, f.launched, run, recordRun(run))
			p := relay.Principal{
				Kind: registry.PrincipalAgent, ID: f.launched.ID, ProjectID: f.launched.ProjectID,
				Agent:    relay.AgentIncarnationFacts{LaunchID: tc.launchID, Generation: 3},
				TokenRun: &relay.TokenRunBinding{RunID: tc.tokenRun, Enforce: tc.mode == agentRunScopeEnforce},
			}
			err := f.srv.conduitAgentRefresh(context.Background(), p,
				relay.RefreshSession{SessionID: "s1", EndpointIncarnation: tc.incarnation},
				&conduitv1.AuthRefresh{Credential: []byte(tok)})
			if !tc.want4401 {
				require.NoError(t, err)
				return
			}
			var ce *conduit.CloseError
			require.ErrorAs(t, err, &ce)
			assert.Equal(t, conduit.CloseUnauthenticated, ce.Code)
		})
	}
}

// TestConduitAgentRefresh_EnforcedSameRunAccepted: with run-scope
// enforcement on, a session admitted with a token for the agent's run
// accepts a fresh token for the same run and stays open. The relay hands
// the refresher the session's admitted endpoint incarnation; without it
// every refresh under enforcement would close the session.
func TestConduitAgentRefresh_EnforcedSameRunAccepted(t *testing.T) {
	f := newRelayFixture(t, nil)
	f.srv.authConfig.AgentRunScope = testRunScopeChecker(agentRunScopeEnforce, time.Time{}, f.store)
	f.public.Close()
	f.public = httptest.NewServer(f.srv.Handler())
	t.Cleanup(f.public.Close)
	refreshes := agentRefreshCounter(t, f.srv)
	run := f.launched.RunID

	ls, wel, err := f.dial(t, signRunToken(t, f.srv, f.store, f.launched, run, recordRun(run)), run)
	require.NoError(t, err)
	require.Equal(t, run, wel.GetEndpointIncarnation())

	require.NoError(t, ls.RefreshAuth([]byte(signRunToken(t, f.srv, f.store, f.launched, run, recordRun(run))), 0))
	require.Eventually(t, func() bool { return refreshes(conduitRefreshOK) == 1 }, refreshWait, 10*time.Millisecond)
	assert.Zero(t, refreshes(conduitRefreshRefused))
	select {
	case <-ls.Done():
		t.Fatalf("session closed after a same-run refresh: %v", ls.Err())
	default:
	}
}

// TestConduitAgentRefresh_StoreFaultKeepsSession: when the credential
// status cannot be read, the refresh is neither accepted nor refused:
// the session stays open on its admitted identity and the outcome is
// counted as unevaluated.
func TestConduitAgentRefresh_StoreFaultKeepsSession(t *testing.T) {
	f := newRelayFixture(t, nil)
	refreshes := agentRefreshCounter(t, f.srv)
	ls := refreshSession(t, f)
	tok := f.agentToken(t, f.launched)

	f.srv.authConfig.CredentialStore = &erroringCredentialStore{Store: f.store, err: errors.New("credential store unavailable for testing")}
	require.NoError(t, ls.RefreshAuth([]byte(tok), 0))
	require.Eventually(t, func() bool { return refreshes(conduitRefreshUnevaluated) == 1 }, refreshWait, 10*time.Millisecond)
	assert.Zero(t, refreshes(conduitRefreshRefused))
	assert.Zero(t, refreshes(conduitRefreshOK))
	select {
	case <-ls.Done():
		t.Fatalf("session closed on an unevaluated refresh: %v", ls.Err())
	default:
	}

	// Once the store answers again, the next refresh is checked again.
	f.srv.authConfig.CredentialStore = f.store
	require.NoError(t, ls.RefreshAuth([]byte(tok), 0))
	require.Eventually(t, func() bool { return refreshes(conduitRefreshOK) == 1 }, refreshWait, 10*time.Millisecond)
}

// TestConduitAgentRefresh_AgentReadFaultKeepsSession: an agent row that
// cannot be read is unevaluated too, not a refusal.
func TestConduitAgentRefresh_AgentReadFaultKeepsSession(t *testing.T) {
	wantErr := errors.New("agent read unavailable for testing")
	srv, s, _, fault := testServerWithStoreFault(t, func(inner store.Store, fault *storeFaultSwitch) *agentReadFaultStore {
		return &agentReadFaultStore{Store: inner, err: wantErr, fault: fault}
	})
	srv, s, _, project := setupCredentialTestServerOn(t, srv, s)
	agentID := tid("agent-refresh-read-fault")
	createCredTestAgent(t, s, agentID, project.ID, tid("user-cred-test"))
	tok, err := srv.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)
	claims, err := srv.agentTokenService.ValidateAgentToken(tok)
	require.NoError(t, err)
	fault.Arm()

	_, err = srv.conduitAgentAdmissionCheck(context.Background(), &agentIdentityWrapper{claims})
	require.ErrorIs(t, err, wantErr)
	assert.False(t, conduitAgentCheckRefused(err))

	p := relay.Principal{Kind: registry.PrincipalAgent, ID: agentID, ProjectID: project.ID}
	assert.NoError(t, srv.conduitAgentRefresh(context.Background(), p, relay.RefreshSession{SessionID: "s1"},
		&conduitv1.AuthRefresh{Credential: []byte(tok)}), "unevaluated keeps the session")
}

// agentReadFaultStore fails GetAgent while its switch is armed.
type agentReadFaultStore struct {
	store.Store
	err   error
	fault *storeFaultSwitch
}

func (s *agentReadFaultStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if !s.fault.Active() {
		return s.Store.GetAgent(ctx, id)
	}
	return nil, s.err
}

// TestConduitAgentRefresh_StreamIDFromDialer4400 pins v2.17: a dialer's
// AuthRefresh carrying a stream id is a protocol error (4400), and never
// reaches the refresher.
func TestConduitAgentRefresh_StreamIDFromDialer4400(t *testing.T) {
	f := newRelayFixture(t, nil)
	refreshes := agentRefreshCounter(t, f.srv)
	ls := refreshSession(t, f)
	require.NoError(t, ls.RefreshAuth([]byte(f.agentToken(t, f.launched)), 7))
	requireClosedWith(t, ls, conduit.CloseProtocolError)
	for _, o := range []string{conduitRefreshOK, conduitRefreshRefused, conduitRefreshUnevaluated} {
		assert.Zero(t, refreshes(o), o)
	}
}

// TestConduitAgentRefresh_OtherPrincipalsRefused: brokers and users do
// not refresh in band; their AuthRefresh is refused with 4401 as before.
func TestConduitAgentRefresh_OtherPrincipalsRefused(t *testing.T) {
	f := newConduitFixture(t)
	for _, kind := range []string{registry.PrincipalBroker, registry.PrincipalUser} {
		err := f.srv.conduitAgentRefresh(context.Background(), relay.Principal{Kind: kind, ID: "x"}, relay.RefreshSession{},
			&conduitv1.AuthRefresh{Credential: []byte("anything")})
		var ce *conduit.CloseError
		require.ErrorAs(t, err, &ce, kind)
		assert.Equal(t, conduit.CloseUnauthenticated, ce.Code, kind)
	}
}

// TestConduitAgentRefresh_FlagOffNoRefresher: with hub.conduit off no
// relay starts, so no refresher is wired and the endpoint is not served.
func TestConduitAgentRefresh_FlagOffNoRefresher(t *testing.T) {
	srv, _ := testServer(t)
	setConduitExperiment(t, srv, false)
	auth, err := relay.NewHMACPeerAuthFromSecret(relay.HMACPeerAuthConfig{Secret: make([]byte, 32), SelfID: "hub-a"})
	require.NoError(t, err)
	require.NoError(t, srv.StartConduitRelay(context.Background(), ConduitRelayOptions{InstanceID: "hub-a", PeerAuth: auth}))
	assert.Nil(t, srv.conduit.Load(), "no relay, so no refresher")
}
