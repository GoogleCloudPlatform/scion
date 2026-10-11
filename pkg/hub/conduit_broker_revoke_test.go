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
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Broker session re-check tests (criterion 16c). Every hub runs its
// conduit timers on a fake clock; the sweep fires only when a test
// advances it. The close itself is waited for in real time, bounded by
// brokerRevokeCloseBound (the "plus 10s" of the criterion).

// brokerRevokeInterval is the fixture's conduit.authz_recheck_interval.
const brokerRevokeInterval = 10 * time.Second

// brokerRevokeCloseBound bounds the close after a re-check decided it.
const brokerRevokeCloseBound = 10 * time.Second

// brokerRevokeFixture is a brokerConduitFixture with a fake clock, the
// in-process event publisher and the re-check metrics recorded.
type brokerRevokeFixture struct {
	*brokerConduitFixture
	clk     *clock.Fake
	events  EventPublisher
	metrics *metricWaiter
	// faults wraps the hub's store: it injects read faults and counts
	// completed broker secret reads.
	faults *conduitBrokerFaultStore
}

func newBrokerRevokeFixture(t *testing.T, events EventPublisher, mods ...func(*ConduitRelayOptions)) *brokerRevokeFixture {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC))
	f := &brokerRevokeFixture{clk: clk, metrics: newMetricWaiter()}
	f.brokerConduitFixture = newBrokerConduitFixture(t, nil, func(o *ConduitRelayOptions) {
		o.Clock = clk
		o.AuthzRecheckInterval = brokerRevokeInterval
		for _, mod := range mods {
			mod(o)
		}
	})
	var m conduitStreamAuthzMetrics = f.metrics
	f.srv.conduitAuthzMetrics.Store(&m)
	f.faults, _ = installStoreFault(t, f.srv, func(inner store.Store, _ *storeFaultSwitch) *conduitBrokerFaultStore {
		return &conduitBrokerFaultStore{Store: inner, secretReads: map[string]int{}, readsCh: make(chan struct{})}
	})
	if events == nil {
		events = NewChannelEventPublisher()
	}
	f.events = events
	f.srv.SetEventPublisher(events)
	return f
}

// addBroker registers another broker with its own secret.
func (f *brokerConduitFixture) addBroker(t *testing.T, name string) (string, []byte) {
	t.Helper()
	ctx := context.Background()
	id := tid(name)
	require.NoError(t, f.store.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: id, Name: name, Slug: name, Status: store.BrokerStatusOnline, Created: time.Now(), Updated: time.Now(),
	}))
	key, err := f.srv.brokerAuthService.GenerateAndStoreSecret(ctx, id)
	require.NoError(t, err)
	secret, err := base64.StdEncoding.DecodeString(key)
	require.NoError(t, err)
	return id, secret
}

// signedHeaderAs returns the upgrade headers of brokerID signed with secret.
func (f *brokerConduitFixture) signedHeaderAs(t *testing.T, brokerID string, secret []byte) http.Header {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.public.URL+"/api/v1/conduit", nil)
	require.NoError(t, err)
	require.NoError(t, (&apiclient.HMACAuth{BrokerID: brokerID, SecretKey: secret}).ApplyAuth(req))
	return req.Header
}

// dialAs opens a conduit session as brokerID signed with secret.
func (f *brokerConduitFixture) dialAs(t *testing.T, brokerID string, secret []byte) conduit.LocalSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d := &ws.Dialer{
		URL:    f.wsURL("/api/v1/conduit"),
		Header: func(context.Context) (http.Header, error) { return f.signedHeaderAs(t, brokerID, secret), nil },
	}
	hello := &conduitv1.Hello{
		PrincipalKind: conduitv1.PrincipalKind_PRINCIPAL_KIND_BROKER,
		PrincipalId:   brokerID,
		Capabilities:  &conduitv1.Capabilities{EndpointIncarnation: "proc-1"},
	}
	s, _, err := conduit.Dial(ctx, d, conduit.Config{Clock: clock.Real()}, hello)
	require.NoError(t, err)
	ls := s.(conduit.LocalSession)
	t.Cleanup(func() { _ = ls.Close() })
	return ls
}

// dialSettled opens a conduit session as brokerID and returns once the
// hub has finished setting it up: the admission read of the broker secret
// and the post-registration check's read have both completed, so a change
// the test makes afterwards is seen only by the re-check.
func (f *brokerRevokeFixture) dialSettled(t *testing.T, brokerID string, secret []byte) conduit.LocalSession {
	t.Helper()
	before, _ := f.faults.secretReadsOf(brokerID)
	ls := f.dialAs(t, brokerID, secret)
	deadline := time.After(10 * time.Second)
	for {
		n, changed := f.faults.secretReadsOf(brokerID)
		if n >= before+2 {
			return ls
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatal("broker session setup did not complete")
		}
	}
}

// upgradeStatus returns the HTTP status of a conduit upgrade as brokerID
// signed with secret.
func (f *brokerConduitFixture) upgradeStatus(t *testing.T, brokerID string, secret []byte) int {
	t.Helper()
	c, resp, err := websocket.DefaultDialer.Dial(f.wsURL("/api/v1/conduit"), f.signedHeaderAs(t, brokerID, secret))
	if err == nil {
		_ = c.Close()
		return http.StatusSwitchingProtocols
	}
	require.NotNil(t, resp, "%v", err)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// waitSessionClosed waits (real time, brokerRevokeCloseBound) for ls to
// end and returns its close code.
func waitSessionClosed(t *testing.T, ls conduit.LocalSession) uint32 {
	t.Helper()
	select {
	case <-ls.Done():
	case <-time.After(brokerRevokeCloseBound):
		t.Fatal("broker session was not closed")
	}
	return conduit.CodeOf(ls.Err(), 0)
}

// assertSessionOpen fails if ls has ended.
func assertSessionOpen(t *testing.T, ls conduit.LocalSession) {
	t.Helper()
	select {
	case <-ls.Done():
		t.Fatalf("broker session closed: %v", ls.Err())
	case <-time.After(100 * time.Millisecond):
	}
}

// brokerChange is one of the three changes criterion 16c covers.
type brokerChange struct {
	name string
	// viaHub makes the change through the hub's API (or the hub's own
	// change path), which runs the notify fast path.
	viaHub func(t *testing.T, f *brokerConduitFixture, srv *Server)
	// inStore makes the same change in the store only, without any
	// notification.
	inStore func(t *testing.T, f *brokerConduitFixture)
	// oldCredentialStatus is the admission status of a reconnect with
	// the old credential.
	oldCredentialStatus int
}

func brokerChanges() []brokerChange {
	return []brokerChange{
		{
			name: "deregister",
			viaHub: func(t *testing.T, f *brokerConduitFixture, srv *Server) {
				rec := doRequest(t, srv, http.MethodDelete, "/api/v1/runtime-brokers/"+f.brokerID, nil)
				require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
			},
			inStore: func(t *testing.T, f *brokerConduitFixture) {
				require.NoError(t, f.store.DeleteRuntimeBroker(context.Background(), f.brokerID))
			},
			oldCredentialStatus: http.StatusUnauthorized,
		},
		{
			name: "rotate secret",
			viaHub: func(t *testing.T, f *brokerConduitFixture, srv *Server) {
				rec := doRequest(t, srv, http.MethodPost, "/api/v1/brokers/"+f.brokerID+"/rotate-secret", nil)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			},
			inStore: func(t *testing.T, f *brokerConduitFixture) {
				ctx := context.Background()
				sec, err := f.store.GetBrokerSecret(ctx, f.brokerID)
				require.NoError(t, err)
				sec.SecretKey = append([]byte("rotated-"), sec.SecretKey...)
				require.NoError(t, f.store.UpdateBrokerSecret(ctx, sec))
			},
			oldCredentialStatus: http.StatusUnauthorized,
		},
		{
			// Re-join: the broker joins again with a fresh join token,
			// which replaces its secret.
			name: "re-join",
			viaHub: func(t *testing.T, f *brokerConduitFixture, srv *Server) {
				reg, err := srv.brokerAuthService.CreateBrokerRegistration(context.Background(),
					CreateBrokerRegistrationRequest{Name: "conduit-broker"}, "test")
				require.NoError(t, err)
				require.Equal(t, f.brokerID, reg.BrokerID, "the registration matched the existing broker")
				rec := joinWithToken(t, srv, f.brokerID, reg.JoinToken)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			},
			inStore: func(t *testing.T, f *brokerConduitFixture) {
				ctx := context.Background()
				sec, err := f.store.GetBrokerSecret(ctx, f.brokerID)
				require.NoError(t, err)
				require.NoError(t, f.store.DeleteBrokerSecret(ctx, f.brokerID))
				sec.SecretKey = append([]byte("rejoined-"), sec.SecretKey...)
				sec.CreatedAt = time.Now()
				require.NoError(t, f.store.CreateBrokerSecret(ctx, sec))
			},
			oldCredentialStatus: http.StatusUnauthorized,
		},
		{
			// Revocation: the broker's secret is no longer active. The
			// hub announces the committed change with
			// conduitBrokerCredentialChanged.
			name: "revoke",
			viaHub: func(t *testing.T, f *brokerConduitFixture, srv *Server) {
				revokeBrokerSecretInStore(t, f)
				srv.conduitBrokerCredentialChanged(f.brokerID)
			},
			inStore:             revokeBrokerSecretInStore,
			oldCredentialStatus: http.StatusUnauthorized,
		},
	}
}

func revokeBrokerSecretInStore(t *testing.T, f *brokerConduitFixture) {
	t.Helper()
	ctx := context.Background()
	sec, err := f.store.GetBrokerSecret(ctx, f.brokerID)
	require.NoError(t, err)
	sec.Status = store.BrokerSecretStatusRevoked
	require.NoError(t, f.store.UpdateBrokerSecret(ctx, sec))
}

// TestConduitBrokerRevoke_NotifyCloses: each change made through the hub
// closes the broker's conduit session with 4401 without the sweep running;
// the old credential is refused at admission; another broker's session
// stays up.
func TestConduitBrokerRevoke_NotifyCloses(t *testing.T) {
	for _, tc := range brokerChanges() {
		t.Run(tc.name, func(t *testing.T) {
			f := newBrokerRevokeFixture(t, nil)
			otherID, otherSecret := f.addBroker(t, "other-broker")
			ls := f.dialSettled(t, f.brokerID, f.secret)
			other := f.dialSettled(t, otherID, otherSecret)

			tc.viaHub(t, f.brokerConduitFixture, f.srv)
			assert.Equal(t, conduit.CloseUnauthenticated, waitSessionClosed(t, ls))
			f.metrics.wait(t, "notify/closed/broker", 1)
			assert.NotContains(t, f.metrics.list(), "sweep/closed/broker")

			assert.Equal(t, tc.oldCredentialStatus, f.upgradeStatus(t, f.brokerID, f.secret))
			assertSessionOpen(t, other)
			assert.Equal(t, http.StatusSwitchingProtocols, f.upgradeStatus(t, otherID, otherSecret))
		})
	}
}

// TestConduitBrokerRevoke_NotifyEventCloses: the change event alone (no
// local fast path) closes the session: this is how every other hub node
// learns of the change.
func TestConduitBrokerRevoke_NotifyEventCloses(t *testing.T) {
	for _, tc := range brokerChanges() {
		t.Run(tc.name, func(t *testing.T) {
			f := newBrokerRevokeFixture(t, nil)
			ls := f.dialSettled(t, f.brokerID, f.secret)
			tc.inStore(t, f.brokerConduitFixture)
			f.events.PublishRaw(conduitBrokerChangedSubject, conduitBrokerChange{BrokerID: f.brokerID})
			assert.Equal(t, conduit.CloseUnauthenticated, waitSessionClosed(t, ls))
			f.metrics.wait(t, "notify/closed/broker", 1)
		})
	}
}

// TestConduitBrokerRevoke_SweepClosesWithoutNotify: with no notification
// at all, the session closes with 4401 once one re-check interval has
// passed; another broker's session stays up and the old credential is
// refused at admission.
func TestConduitBrokerRevoke_SweepClosesWithoutNotify(t *testing.T) {
	for _, tc := range brokerChanges() {
		t.Run(tc.name, func(t *testing.T) {
			f := newBrokerRevokeFixture(t, nil)
			otherID, otherSecret := f.addBroker(t, "other-broker")
			ls := f.dialSettled(t, f.brokerID, f.secret)
			other := f.dialSettled(t, otherID, otherSecret)

			tc.inStore(t, f.brokerConduitFixture)
			f.clk.Advance(brokerRevokeInterval - time.Second)
			assertSessionOpen(t, ls)

			f.clk.Advance(time.Second)
			assert.Equal(t, conduit.CloseUnauthenticated, waitSessionClosed(t, ls))
			f.metrics.wait(t, "sweep/closed/broker", 1)
			assert.NotContains(t, f.metrics.list(), "notify/closed/broker")

			assert.Equal(t, tc.oldCredentialStatus, f.upgradeStatus(t, f.brokerID, f.secret))
			assertSessionOpen(t, other)
		})
	}
}

// TestConduitBrokerRevoke_ExpiredSecretCloses: a secret past its expiry is
// no longer current.
func TestConduitBrokerRevoke_ExpiredSecretCloses(t *testing.T) {
	f := newBrokerRevokeFixture(t, nil)
	ls := f.dialSettled(t, f.brokerID, f.secret)
	ctx := context.Background()
	sec, err := f.store.GetBrokerSecret(ctx, f.brokerID)
	require.NoError(t, err)
	sec.ExpiresAt = time.Now().Add(-time.Minute)
	require.NoError(t, f.store.UpdateBrokerSecret(ctx, sec))
	f.srv.conduitBrokerCredentialChanged(f.brokerID)
	assert.Equal(t, conduit.CloseUnauthenticated, waitSessionClosed(t, ls))
}

// TestConduitBrokerRevoke_NewCredentialSessionStays: after a rotation the
// broker reconnects with its new secret, and later re-checks keep that
// session.
func TestConduitBrokerRevoke_NewCredentialSessionStays(t *testing.T) {
	f := newBrokerRevokeFixture(t, nil)
	ls := f.dialSettled(t, f.brokerID, f.secret)
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/brokers/"+f.brokerID+"/rotate-secret", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp RotateSecretResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	newSecret, err := base64.StdEncoding.DecodeString(resp.SecretKey)
	require.NoError(t, err)
	assert.Equal(t, conduit.CloseUnauthenticated, waitSessionClosed(t, ls))
	f.metrics.wait(t, "notify/closed/broker", 1)

	fresh := f.dialSettled(t, f.brokerID, newSecret)
	f.srv.conduitBrokerCredentialChanged(f.brokerID)
	f.metrics.wait(t, "notify/passed/broker", 1)
	f.clk.Advance(brokerRevokeInterval)
	f.metrics.wait(t, "sweep/passed/broker", 1)
	assertSessionOpen(t, fresh)
}

// TestConduitBrokerRevoke_SessionBindsToVerifiedSecret: a session binds to
// the secret that verified its upgrade request. When the broker's current
// secret no longer verifies the request by the time the session is set up,
// the upgrade is refused with 401.
func TestConduitBrokerRevoke_SessionBindsToVerifiedSecret(t *testing.T) {
	f := newBrokerRevokeFixture(t, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/conduit", nil)
	req.Header = f.signedHeader(t, "/api/v1/conduit", nil)
	// The broker's identity, as the broker authentication middleware
	// installs it once the signature verified.
	req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity(f.brokerID)))

	// Control: with the secret unchanged the request gets past the broker
	// checks (and stops at the missing WebSocket upgrade).
	rec := httptest.NewRecorder()
	f.srv.handleConduit(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	ctx := context.Background()
	sec, err := f.store.GetBrokerSecret(ctx, f.brokerID)
	require.NoError(t, err)
	sec.SecretKey = append([]byte("rotated-"), sec.SecretKey...)
	require.NoError(t, f.store.UpdateBrokerSecret(ctx, sec))

	rec = httptest.NewRecorder()
	f.srv.handleConduit(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	assert.Empty(t, f.sessions(t))
}

// TestConduitBrokerRevoke_ChangedDuringAdmission: a change that commits
// while the session is being admitted closes it with 4401 once it is
// registered.
func TestConduitBrokerRevoke_ChangedDuringAdmission(t *testing.T) {
	var f *brokerConduitFixture
	f = newBrokerConduitFixture(t, nil, func(o *ConduitRelayOptions) {
		o.testHookAdmission = func() { revokeBrokerSecretInStore(t, f) }
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d := &ws.Dialer{
		URL:    f.wsURL("/api/v1/conduit"),
		Header: func(context.Context) (http.Header, error) { return f.signedHeader(t, "/api/v1/conduit", nil), nil },
	}
	hello := &conduitv1.Hello{
		PrincipalKind: conduitv1.PrincipalKind_PRINCIPAL_KIND_BROKER,
		PrincipalId:   f.brokerID,
		Capabilities:  &conduitv1.Capabilities{EndpointIncarnation: "proc-1"},
	}
	s, _, err := conduit.Dial(ctx, d, conduit.Config{Clock: clock.Real()}, hello)
	if err != nil {
		// The close can reach the dialer before it has read the Welcome.
		assert.Equal(t, conduit.CloseUnauthenticated, conduit.CodeOf(err, 0), "%v", err)
		return
	}
	ls := s.(conduit.LocalSession)
	t.Cleanup(func() { _ = ls.Close() })
	assert.Equal(t, conduit.CloseUnauthenticated, waitSessionClosed(t, ls))
}

// TestConduitBrokerRevoke_DeniedBeforeRegistrationCloses: a re-check that
// denies the session while it is admitted but not yet registered (so the
// close it issues finds no registered session) still closes it with 4401
// once it registers, even when every store read of the post-registration
// check fails and would keep the session.
func TestConduitBrokerRevoke_DeniedBeforeRegistrationCloses(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fault func(*conduitBrokerFaultStore)
	}{
		{"broker read fails", func(s *conduitBrokerFaultStore) { s.failBroker.Store(true) }},
		{"secret read fails", func(s *conduitBrokerFaultStore) { s.failSecret.Store(true) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var f *brokerRevokeFixture
			var hooked atomic.Bool
			f = newBrokerRevokeFixture(t, nil, func(o *ConduitRelayOptions) {
				o.testHookAdmission = func() {
					if !hooked.CompareAndSwap(false, true) {
						return
					}
					revokeBrokerSecretInStore(t, f.brokerConduitFixture)
					v := f.srv.conduitBrokerRevoke.Load()
					v.authz.Recheck(context.Background(), conduitAuthzTriggerNotify, conduitBrokerMatch(f.brokerID))
					assert.Equal(t, 0, v.authz.Len(), "the denied session is no longer tracked")
					tc.fault(f.faults)
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			d := &ws.Dialer{
				URL:    f.wsURL("/api/v1/conduit"),
				Header: func(context.Context) (http.Header, error) { return f.signedHeader(t, "/api/v1/conduit", nil), nil },
			}
			hello := &conduitv1.Hello{
				PrincipalKind: conduitv1.PrincipalKind_PRINCIPAL_KIND_BROKER,
				PrincipalId:   f.brokerID,
				Capabilities:  &conduitv1.Capabilities{EndpointIncarnation: "proc-1"},
			}
			s, _, err := conduit.Dial(ctx, d, conduit.Config{Clock: clock.Real()}, hello)
			require.True(t, hooked.Load(), "admission hook ran")
			f.metrics.wait(t, "notify/closed/broker", 1)
			if err != nil {
				// The close can reach the dialer before it has read the Welcome.
				assert.Equal(t, conduit.CloseUnauthenticated, conduit.CodeOf(err, 0), "%v", err)
				return
			}
			ls := s.(conduit.LocalSession)
			t.Cleanup(func() { _ = ls.Close() })
			assert.Equal(t, conduit.CloseUnauthenticated, waitSessionClosed(t, ls))
		})
	}
}

// TestConduitBrokerRevoke_MarkedBeforeClose: a denying re-check marks the
// session denied before it closes the broker's registered sessions, so a
// session that registers between the two still finds the mark.
func TestConduitBrokerRevoke_MarkedBeforeClose(t *testing.T) {
	f := newBrokerRevokeFixture(t, nil)
	var calls atomic.Int32
	hook := func(bs *conduitBrokerSession) {
		calls.Add(1)
		assert.True(t, bs.denied.Load(), "the session is marked denied before the close")
	}
	f.srv.conduitBrokerRevoke.Load().testHookBeforeClose.Store(&hook)
	ls := f.dialSettled(t, f.brokerID, f.secret)
	revokeBrokerSecretInStore(t, f.brokerConduitFixture)
	f.srv.conduitBrokerCredentialChanged(f.brokerID)
	assert.Equal(t, conduit.CloseUnauthenticated, waitSessionClosed(t, ls))
	f.metrics.wait(t, "notify/closed/broker", 1)
	assert.Equal(t, int32(1), calls.Load())
}

// TestConduitBrokerRevoke_CrossNode: the broker's session is held by hub
// node B; the secret is rotated through hub node A (same database, shared
// event bus). Node B closes the session from its own subscription; node A
// runs no relay and makes no call to node B.
func TestConduitBrokerRevoke_CrossNode(t *testing.T) {
	bus := NewChannelEventPublisher()
	b := newBrokerRevokeFixture(t, bus)
	otherID, otherSecret := b.addBroker(t, "other-broker")
	ls := b.dialSettled(t, b.brokerID, b.secret)
	other := b.dialSettled(t, otherID, otherSecret)

	cfg := DefaultServerConfig()
	cfg.DevAuthToken = testDevToken
	cfg.BrokerAuthConfig = DefaultBrokerAuthConfig()
	nodeA, err := newTestHubServer(t, cfg, b.store)
	require.NoError(t, err)
	setConduitExperiment(t, nodeA, true)
	nodeA.SetEventPublisher(bus)
	require.Nil(t, nodeA.conduitBrokerRevoke.Load(), "node A runs no broker session re-check")

	rec := doRequest(t, nodeA, http.MethodPost, "/api/v1/brokers/"+b.brokerID+"/rotate-secret", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, conduit.CloseUnauthenticated, waitSessionClosed(t, ls))
	b.metrics.wait(t, "notify/closed/broker", 1)
	assertSessionOpen(t, other)
}

// TestConduitBrokerRevoke_ResyncAfterListenGap: the event is lost while
// the LISTEN connection is down; its re-establishment re-checks every
// broker session and closes the revoked one.
func TestConduitBrokerRevoke_ResyncAfterListenGap(t *testing.T) {
	pub := newListenGapPublisher()
	f := newBrokerRevokeFixture(t, pub)
	otherID, otherSecret := f.addBroker(t, "other-broker")
	ls := f.dialSettled(t, f.brokerID, f.secret)
	other := f.dialSettled(t, otherID, otherSecret)

	pub.down.Store(true)
	revokeBrokerSecretInStore(t, f.brokerConduitFixture)
	pub.PublishRaw(conduitBrokerChangedSubject, conduitBrokerChange{BrokerID: f.brokerID}) // lost
	assertSessionOpen(t, ls)

	pub.reconnect()
	assert.Equal(t, conduit.CloseUnauthenticated, waitSessionClosed(t, ls))
	f.metrics.wait(t, "resync/closed/broker", 1)
	assert.NotContains(t, f.metrics.list(), "notify/closed/broker")
	assertSessionOpen(t, other)
}

// conduitBrokerFaultStore makes the broker reads fail on demand.
type conduitBrokerFaultStore struct {
	store.Store
	failBroker atomic.Bool
	failSecret atomic.Bool

	mu          sync.Mutex
	secretReads map[string]int // completed GetBrokerSecret calls per broker
	readsCh     chan struct{}  // closed and replaced on every completed read
}

// secretReadsOf returns the completed secret reads of broker id and a
// channel closed on the next one.
func (s *conduitBrokerFaultStore) secretReadsOf(id string) (int, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.secretReads[id], s.readsCh
}

func (s *conduitBrokerFaultStore) GetRuntimeBroker(ctx context.Context, id string) (*store.RuntimeBroker, error) {
	if s.failBroker.Load() {
		return nil, errInjectedStoreFault
	}
	return s.Store.GetRuntimeBroker(ctx, id)
}

func (s *conduitBrokerFaultStore) GetBrokerSecret(ctx context.Context, id string) (*store.BrokerSecret, error) {
	if s.failSecret.Load() {
		return nil, errInjectedStoreFault
	}
	sec, err := s.Store.GetBrokerSecret(ctx, id)
	s.mu.Lock()
	s.secretReads[id]++
	close(s.readsCh)
	s.readsCh = make(chan struct{})
	s.mu.Unlock()
	return sec, err
}

// TestConduitBrokerRevoke_StoreFaultDefers: a re-check whose store read
// fails leaves the session open (deferred, WARN), even though the secret
// was in fact revoked; the next check that can read the store closes it.
func TestConduitBrokerRevoke_StoreFaultDefers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fault func(*conduitBrokerFaultStore)
	}{
		{"broker read fails", func(s *conduitBrokerFaultStore) { s.failBroker.Store(true) }},
		{"secret read fails", func(s *conduitBrokerFaultStore) { s.failSecret.Store(true) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newBrokerRevokeFixture(t, nil)
			fs := f.faults
			ls := f.dialSettled(t, f.brokerID, f.secret)
			revokeBrokerSecretInStore(t, f.brokerConduitFixture)
			tc.fault(fs)

			f.clk.Advance(brokerRevokeInterval)
			f.metrics.wait(t, "sweep/deferred_unavailable/broker", 1)
			assertSessionOpen(t, ls)

			fs.failBroker.Store(false) // recovered
			fs.failSecret.Store(false)
			f.clk.Advance(brokerRevokeInterval)
			assert.Equal(t, conduit.CloseUnauthenticated, waitSessionClosed(t, ls))
			f.metrics.wait(t, "sweep/closed/broker", 1)
		})
	}
}

// TestConduitBrokerRevoke_ExperimentOff: with hub.conduit off no broker
// session re-check runs, a rotation publishes no change event, and the
// rotation and deregistration answer as they do with the flag off.
func TestConduitBrokerRevoke_ExperimentOff(t *testing.T) {
	srv, s := testServerWithBrokerAuth(t)
	setConduitExperiment(t, srv, false)
	pub := NewChannelEventPublisher()
	srv.SetEventPublisher(pub)
	ch, unsubscribe := pub.Subscribe(conduitBrokerChangedSubject)
	defer unsubscribe()
	ctx := context.Background()
	id := tid("off-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: id, Name: "off-broker", Slug: "off-broker", Status: store.BrokerStatusOnline, Created: time.Now(), Updated: time.Now(),
	}))
	_, err := srv.brokerAuthService.GenerateAndStoreSecret(ctx, id)
	require.NoError(t, err)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/brokers/"+id+"/rotate-secret", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = doRequest(t, srv, http.MethodDelete, "/api/v1/runtime-brokers/"+id, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	srv.conduitBrokerCredentialChanged(id)

	assert.Nil(t, srv.conduitBrokerRevoke.Load())
	select {
	case evt := <-ch:
		t.Fatalf("change event published with hub.conduit off: %s", evt.Subject)
	case <-time.After(100 * time.Millisecond):
	}
}
