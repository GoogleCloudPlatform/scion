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
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// externalPeerSecret keys the test hub's relay-peer calls.
var externalPeerSecret = []byte("external-relay-peer-secret-0123456789abc")

// externalFixture is a hub in external relay mode over its own registry.
type externalFixture struct {
	*conduitFixture
	regStore registry.Store
	reg      *registry.Registry
}

func externalOptions(t *testing.T, f *externalFixture) ConduitExternalOptions {
	t.Helper()
	auth, err := relay.NewHMACPeerAuthFromPeerSecret(relay.HMACPeerAuthConfig{Secret: append([]byte(nil), externalPeerSecret...), SelfID: "hub-ext"})
	require.NoError(t, err)
	return ConduitExternalOptions{InstanceID: "hub-ext", PeerAuth: auth, Store: f.regStore, Registry: f.reg, Clock: clock.NewFake(time.Now())}
}

// externalRelayAuth is the relay-peer authenticator of a relay calling the
// fixture hub.
func externalRelayAuth(t *testing.T, self string) relay.PeerAuth {
	t.Helper()
	auth, err := relay.NewHMACPeerAuthFromPeerSecret(relay.HMACPeerAuthConfig{Secret: append([]byte(nil), externalPeerSecret...), SelfID: self})
	require.NoError(t, err)
	return auth
}

func newExternalFixture(t *testing.T) *externalFixture {
	t.Helper()
	f := &externalFixture{conduitFixture: newConduitFixture(t)}
	f.regStore = entadapter.NewConduitRegistryStore(enttest.NewClient(t))
	f.reg = registry.New(f.regStore, registry.Config{})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f.srv.config.SharedSigningSecret = testRelaySessionSecret
	require.NoError(t, f.srv.StartConduitExternal(ctx, externalOptions(t, f)))
	return f
}

// TestConduitExternal_NoRelayOfItsOwn (design §3.9): in external relay mode
// the hub runs no relay, inserts no relay row and serves no in-process
// relay routes; it routes hub-originated traffic and does not tell agents
// to dial conduit.
func TestConduitExternal_NoRelayOfItsOwn(t *testing.T) {
	f := newExternalFixture(t)
	relays, err := f.regStore.ListRelays(context.Background())
	require.NoError(t, err)
	assert.Empty(t, relays, "no relay_instances row")
	assert.Nil(t, f.srv.conduit.Load(), "no in-process relay")
	assert.True(t, f.srv.ConduitExternal())

	// The internal listener serves the hub internal API: a relay-peer
	// request for an in-process relay route finds nothing there.
	internal := httptest.NewServer(f.srv.ConduitInternalHandler())
	t.Cleanup(internal.Close)
	hc := &relay.HubClient{BaseURL: internal.URL, Auth: externalRelayAuth(t, "relay-0")}
	err = hc.Call(context.Background(), relay.InternalPathPrefix+"self", map[string]string{}, nil)
	var refusal *relay.HubRefusal
	require.ErrorAs(t, err, &refusal)
	assert.Equal(t, http.StatusNotFound, refusal.Status, "no in-process relay routes")

	assert.False(t, f.srv.conduitServing(), "agents are not told to dial conduit")
	assert.False(t, slices.Contains(f.srv.dispatchExperiments(), conduitExperiment))
	assert.True(t, f.srv.conduitRoutable(), "hub-originated traffic routes to the relays")
	assert.NotNil(t, f.srv.conduitRouter())
}

func TestConduitExternal_StartChecks(t *testing.T) {
	f := &externalFixture{conduitFixture: newConduitFixture(t)}
	f.regStore = entadapter.NewConduitRegistryStore(enttest.NewClient(t))
	f.reg = registry.New(f.regStore, registry.Config{})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	setConduitExperiment(t, f.srv, false)
	f.srv.config.SharedSigningSecret = testRelaySessionSecret
	require.ErrorIs(t, f.srv.StartConduitExternal(ctx, externalOptions(t, f)), errConduitExternalDisabled)
	setConduitExperiment(t, f.srv, true)

	noAuth := externalOptions(t, f)
	noAuth.PeerAuth = nil
	f.srv.config.SharedSigningSecret = testRelaySessionSecret
	require.Error(t, f.srv.StartConduitExternal(ctx, noAuth))

	f.srv.config.SharedSigningSecret = testRelaySessionSecret
	require.NoError(t, f.srv.StartConduitExternal(ctx, externalOptions(t, f)))
	f.srv.config.SharedSigningSecret = testRelaySessionSecret
	require.Error(t, f.srv.StartConduitExternal(ctx, externalOptions(t, f)), "started twice")
	require.Error(t, f.srv.StartConduitRelay(ctx, ConduitRelayOptions{PeerAuth: externalOptions(t, f).PeerAuth, Store: f.regStore, Registry: f.reg}),
		"an in-process relay cannot start next to the external runtime")
}

// eligibleRelaysGauge collects scion.hub.conduit.eligible_relays.
func eligibleRelaysGauge(t *testing.T, reader *sdkmetric.ManualReader) (int64, bool) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "scion.hub.conduit.eligible_relays" {
				continue
			}
			g, ok := m.Data.(metricdata.Gauge[int64])
			require.True(t, ok, "gauge type %T", m.Data)
			if len(g.DataPoints) == 0 {
				return 0, false
			}
			return g.DataPoints[0].Value, true
		}
	}
	return 0, false
}

// conduitDialRefusal decodes the error body of a refused conduit dial.
func conduitDialRefusal(t *testing.T, rec *httptest.ResponseRecorder) (code, message, reason string) {
	t.Helper()
	var body struct {
		Error struct {
			Code    string                 `json:"code"`
			Message string                 `json:"message"`
			Details map[string]interface{} `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	reason, _ = body.Error.Details["reason"].(string)
	return body.Error.Code, body.Error.Message, reason
}

// TestConduitExternal_DialRefusedNotServing (design §3.9, criterion 27):
// a conduit dial to a hub in external relay mode gets 503 unavailable with
// reason conduit_not_serving, and the readiness gauge reports the eligible
// relays: 0 with none, 1 once a live relay registers.
func TestConduitExternal_DialRefusedNotServing(t *testing.T) {
	f := newExternalFixture(t)
	reader := sdkmetric.NewManualReader()
	require.NoError(t, f.srv.RegisterConduitRelayReadinessGauge(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))))
	dial := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/conduit", nil)
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		f.srv.handleConduit(rec, req)
		return rec
	}

	rec := dial()
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	code, msg, reason := conduitDialRefusal(t, rec)
	assert.Equal(t, ErrCodeUnavailable, code)
	assert.Equal(t, ptyReasonConduitNotServing, reason)
	assert.Equal(t, conduitExternalNoRelayMsg, msg)
	n, ok := eligibleRelaysGauge(t, reader)
	require.True(t, ok)
	assert.Equal(t, int64(0), n)

	ctx := context.Background()
	_, err := f.reg.RegisterRelay(ctx, registry.RelayInstance{InstanceID: "relay-0", InternalEndpoint: "http://relay-0:9811"})
	require.NoError(t, err)
	// The count is refreshed on its period, not per dial.
	f.srv.refreshConduitEligibleRelays(ctx, f.srv.conduitExternal.Load())
	rec = dial()
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	_, msg, reason = conduitDialRefusal(t, rec)
	assert.Equal(t, ptyReasonConduitNotServing, reason)
	assert.Equal(t, conduitExternalNoIngressMsg, msg, "relays are eligible, but this hub has no ingress yet")
	n, _ = eligibleRelaysGauge(t, reader)
	assert.Equal(t, int64(1), n)

	post := httptest.NewRecorder()
	f.srv.handleConduit(post, httptest.NewRequest(http.MethodPost, "/api/v1/conduit", nil))
	assert.Equal(t, http.StatusMethodNotAllowed, post.Code)
	setConduitExperiment(t, f.srv, false)
	off := dial()
	assert.Equal(t, http.StatusNotFound, off.Code)
}

// TestConduitExternal_RevocationEventsPublished: the revocation events the
// relays act on are published with no local relay.
func TestConduitExternal_RevocationEventsPublished(t *testing.T) {
	f := newExternalFixture(t)
	events := NewChannelEventPublisher()
	f.srv.SetEventPublisher(events)
	ch, unsubscribe := events.Subscribe(conduitAuthzChangedSubject, conduitBrokerChangedSubject)
	t.Cleanup(unsubscribe)

	f.srv.publishConduitAuthzChanged(conduitAuthzMatch{UserID: "user-1"})
	f.srv.conduitBrokerCredentialChanged("broker-1")
	got := map[string]bool{}
	for len(got) < 2 {
		select {
		case evt := <-ch:
			got[evt.Subject] = true
		case <-time.After(10 * time.Second):
			t.Fatalf("events received: %v", got)
		}
	}
	assert.True(t, got[conduitAuthzChangedSubject])
	assert.True(t, got[conduitBrokerChangedSubject])
}

// TestConduitExternal_ForgetAgentDeletesRelayRows: deleting an agent in
// external mode deletes its session rows on every relay (the owner relay
// closes the session on its next touch or session re-check) and its epoch.
func TestConduitExternal_ForgetAgentDeletesRelayRows(t *testing.T) {
	f := newExternalFixture(t)
	ctx := context.Background()
	gen, err := f.reg.RegisterRelay(ctx, registry.RelayInstance{InstanceID: "relay-0", InternalEndpoint: "http://relay-0:9811"})
	require.NoError(t, err)
	_, err = f.reg.InsertSessionWithNextEpoch(ctx, registry.SessionRecord{
		SessionID: "s-1", PrincipalKind: registry.PrincipalAgent, PrincipalID: f.agent.ID, ProjectID: f.agent.ProjectID,
		RelayInstanceID: "relay-0", RelayGeneration: gen, Transport: registry.TransportWS, EndpointIncarnation: "L1",
	})
	require.NoError(t, err)

	f.srv.conduitForgetAgent(ctx, f.agent.ID)
	ps, err := f.regStore.ListPrincipalSessions(ctx, registry.PrincipalAgent, f.agent.ID)
	require.NoError(t, err)
	assert.Empty(t, ps.Sessions, "session rows deleted")
	assert.Zero(t, ps.CurrentEpoch, "epoch row deleted")
}

// TestConduitExternal_DialReadsNoStore: a conduit dial in external relay
// mode is answered from the cached eligible-relay count; it never reads the
// registry.
func TestConduitExternal_DialReadsNoStore(t *testing.T) {
	f := &externalFixture{conduitFixture: newConduitFixture(t)}
	var reads atomic.Int64
	counting := &registry.FaultStore{Inner: entadapter.NewConduitRegistryStore(enttest.NewClient(t)), Fault: func(_ context.Context, op string) error {
		if op == registry.OpListRelays {
			reads.Add(1)
		}
		return nil
	}}
	f.regStore = counting
	f.reg = registry.New(counting, registry.Config{})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f.srv.config.SharedSigningSecret = testRelaySessionSecret
	require.NoError(t, f.srv.StartConduitExternal(ctx, externalOptions(t, f)))
	before := reads.Load()
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/conduit", nil)
		f.srv.handleConduit(rec, req)
		require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	}
	assert.Equal(t, before, reads.Load(), "a dial reads no relay rows")
}

// testRelaySessionSecret is the shared session secret of the test hubs in
// external relay mode (admission tokens are keyed from it).
const testRelaySessionSecret = "relay-test-session-secret-0123456789abcdef"

// TestConduitExternal_RequiresSessionSecret: external relay mode refuses to
// start without the shared session secret, which keys admission tokens
// every hub replica must open.
func TestConduitExternal_RequiresSessionSecret(t *testing.T) {
	f := &externalFixture{conduitFixture: newConduitFixture(t)}
	f.regStore = entadapter.NewConduitRegistryStore(enttest.NewClient(t))
	f.reg = registry.New(f.regStore, registry.Config{})
	f.srv.config.SharedSigningSecret = ""
	err := f.srv.StartConduitExternal(context.Background(), externalOptions(t, f))
	require.ErrorIs(t, err, errConduitAdmissionNoKey)
	assert.False(t, f.srv.ConduitExternal())
}
