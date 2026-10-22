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
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

const (
	brokerSupersededMetric = "scion.hub.conduit.broker_incarnation_superseded"
	brokerFlapMetric       = "scion.hub.conduit.broker_incarnation_flap"
)

// brokerIncarnationCounters wires the broker incarnation counters on s and
// returns a reader of a counter's total for a broker id and, when path is
// not empty, a path.
func brokerIncarnationCounters(t *testing.T, s *Server) func(name, brokerID, path string) int64 {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	rec, err := NewOTelConduitBrokerIncarnationMetrics(mp)
	require.NoError(t, err)
	s.SetConduitBrokerIncarnationMetrics(rec)
	return func(name, brokerID, path string) int64 {
		var rm metricdata.ResourceMetrics
		require.NoError(t, reader.Collect(context.Background(), &rm))
		var total int64
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name != name {
					continue
				}
				sum, ok := m.Data.(metricdata.Sum[int64])
				require.True(t, ok, "counter data %T", m.Data)
				for _, dp := range sum.DataPoints {
					if v, ok := dp.Attributes.Value("broker_id"); !ok || v.AsString() != brokerID {
						continue
					}
					if path != "" {
						if v, ok := dp.Attributes.Value("path"); !ok || v.AsString() != path {
							continue
						}
					}
					total += dp.Value
				}
			}
		}
		return total
	}
}

// waitBrokerSessionRegistered waits, at most 10s, until the fixture's relay
// has registered the session sessionID.
func waitBrokerSessionRegistered(t *testing.T, f *brokerConduitFixture, sessionID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _, ok := f.srv.conduit.Load().relay.Local(ctx, sessionID)
	require.True(t, ok, "the broker session %s registers on the relay", sessionID)
}

// TestConduitBroker_NewIncarnationClosesOlderSessions: admitting a broker
// session with a new incarnation closes the broker's older-incarnation
// session on this node with 4409 superseded_incarnation, counted with the
// broker's id (path admission) before the close. A return to the earlier
// incarnation is admitted, closes the other one in turn and is counted as
// a flap, also before the close.
func TestConduitBroker_NewIncarnationClosesOlderSessions(t *testing.T) {
	f := newBrokerConduitFixture(t, nil, nil)
	count := brokerIncarnationCounters(t, f.srv)

	old, wOld, err := f.dial(t, "", "proc-1")
	require.NoError(t, err)
	waitBrokerSessionRegistered(t, f, wOld.GetSessionId())
	cur, wCur, err := f.dial(t, "", "proc-2")
	require.NoError(t, err)
	waitBrokerSessionRegistered(t, f, wCur.GetSessionId())
	assert.Equal(t, relay.CloseSupersededIncarnation, waitSessionClosed(t, old), "%v", old.Err())
	assertSessionOpen(t, cur)
	// The relay reports the count before it closes the sessions.
	assert.Equal(t, int64(1), count(brokerSupersededMetric, f.brokerID, brokerSupersededAtAdmission), "one admission close counted for the broker")
	assert.Zero(t, count(brokerFlapMetric, f.brokerID, ""), "a first change is not a flap")

	back, _, err := f.dial(t, "", "proc-1")
	require.NoError(t, err)
	assert.Equal(t, relay.CloseSupersededIncarnation, waitSessionClosed(t, cur), "%v", cur.Err())
	assertSessionOpen(t, back)
	assert.Equal(t, int64(2), count(brokerSupersededMetric, f.brokerID, brokerSupersededAtAdmission), "the second admission close counted")
	assert.Equal(t, int64(1), count(brokerFlapMetric, f.brokerID, ""), "the return is one flap")
	assert.Zero(t, count(brokerSupersededMetric, f.brokerID, brokerSupersededAtRecheck))
}

// TestConduitBrokerRevoke_RecheckClosesOlderIncarnation: the in-process
// session re-check closes this node's broker session once the broker's
// reference is a newer session of another incarnation on another relay,
// with 4409 superseded_incarnation, counted with the broker's id (path
// recheck). A session of the reference incarnation stays open.
func TestConduitBrokerRevoke_RecheckClosesOlderIncarnation(t *testing.T) {
	f := newBrokerRevokeFixture(t, nil)
	count := brokerIncarnationCounters(t, f.srv)
	ctx := context.Background()
	rt := f.srv.conduit.Load()
	require.NotNil(t, rt)
	v := f.srv.conduitBrokerRevoke.Load()
	require.NotNil(t, v)

	old, wOld, err := f.dial(t, "", "proc-1")
	require.NoError(t, err)
	waitBrokerSessionRegistered(t, f.brokerConduitFixture, wOld.GetSessionId())
	v.authz.Recheck(ctx, conduitAuthzTriggerSweep, conduitBrokerMatch(f.brokerID))
	assertSessionOpen(t, old)

	// The broker's newest session, of another incarnation, is on hub-b.
	gen, err := rt.registry.RegisterRelay(ctx, registry.RelayInstance{InstanceID: "hub-b", InternalEndpoint: "http://hub-b.invalid:9811"})
	require.NoError(t, err)
	_, err = rt.registry.InsertSessionWithNextEpoch(ctx, registry.SessionRecord{
		SessionID: "s-hub-b", PrincipalKind: registry.PrincipalBroker, PrincipalID: f.brokerID,
		RelayInstanceID: "hub-b", RelayGeneration: gen, Transport: registry.TransportWS, EndpointIncarnation: "proc-2",
	})
	require.NoError(t, err)

	v.authz.Recheck(ctx, conduitAuthzTriggerSweep, conduitBrokerMatch(f.brokerID))
	assert.Equal(t, relay.CloseSupersededIncarnation, waitSessionClosed(t, old), "%v", old.Err())
	assert.Equal(t, int64(1), count(brokerSupersededMetric, f.brokerID, brokerSupersededAtRecheck))

	// A session of the reference incarnation admitted here is newer than
	// hub-b's: a re-check keeps it.
	cur, wCur, err := f.dial(t, "", "proc-2")
	require.NoError(t, err)
	waitBrokerSessionRegistered(t, f.brokerConduitFixture, wCur.GetSessionId())
	v.authz.Recheck(ctx, conduitAuthzTriggerSweep, conduitBrokerMatch(f.brokerID))
	assertSessionOpen(t, cur)
	assert.Equal(t, int64(1), count(brokerSupersededMetric, f.brokerID, brokerSupersededAtRecheck), "nothing more closed")
}

// TestRelaySessionRecheck_BrokerOlderIncarnation: the re-check of a
// relay-held broker session answers close 4409 superseded_incarnation once
// a newer session of another incarnation is the broker's reference, and
// counts it with the broker's id (path recheck); the reference session
// itself stays open.
func TestRelaySessionRecheck_BrokerOlderIncarnation(t *testing.T) {
	srv, st := testServerWithBrokerAuth(t)
	ctx := context.Background()
	brokerID := tid("relay-inc-broker")
	require.NoError(t, st.CreateRuntimeBroker(ctx, &store.RuntimeBroker{ID: brokerID, Name: "relay-inc-broker", Slug: "relay-inc-broker", Status: store.BrokerStatusOnline, Created: time.Now(), Updated: time.Now()}))
	key, err := srv.brokerAuthService.GenerateAndStoreSecret(ctx, brokerID)
	require.NoError(t, err)
	secret, err := base64.StdEncoding.DecodeString(key)
	require.NoError(t, err)
	srv.conduitGrants = newConduitGrantKeys(&memoryConduitGrantKeyStore{}, time.Now)
	setConduitExperiment(t, srv, true)
	f := startRelayOpsHub(t, srv, st)
	count := brokerIncarnationCounters(t, srv)

	admit := func(sessionID string) relay.HubAdmitResponse {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, relayPublicURL+"/api/v1/conduit", nil)
		require.NoError(t, err)
		require.NoError(t, (&apiclient.HMACAuth{BrokerID: brokerID, SecretKey: secret}).ApplyAuth(req))
		return f.mustAdmit(t, req.Header, sessionID)
	}
	insert := func(sessionID, incarnation string) {
		t.Helper()
		_, err := f.reg.InsertSessionWithNextEpoch(ctx, registry.SessionRecord{
			SessionID: sessionID, PrincipalKind: registry.PrincipalBroker, PrincipalID: brokerID,
			RelayInstanceID: "relay-0", RelayGeneration: f.relayGen, Transport: registry.TransportWS, EndpointIncarnation: incarnation,
		})
		require.NoError(t, err)
	}

	oldAdm := admit("s-old")
	insert("s-old", "proc-1")
	res := sessionRecheck(t, f.hub, relay.HubSessionRecheckItem{SessionID: "s-old", AdmissionToken: oldAdm.AdmissionToken})
	require.True(t, res[0].OK, "the only incarnation: %+v", res[0])

	newAdm := admit("s-new")
	insert("s-new", "proc-2")
	res = sessionRecheck(t, f.hub,
		relay.HubSessionRecheckItem{SessionID: "s-old", AdmissionToken: oldAdm.AdmissionToken},
		relay.HubSessionRecheckItem{SessionID: "s-new", AdmissionToken: newAdm.AdmissionToken})
	assert.True(t, res[0].Close, "the older incarnation's session closes: %+v", res[0])
	assert.Equal(t, relay.CloseSupersededIncarnation, res[0].CloseCode)
	assert.Equal(t, relay.SupersededIncarnationReason(), res[0].Reason)
	assert.True(t, res[1].OK, "the reference session stays open: %+v", res[1])
	assert.Equal(t, int64(1), count(brokerSupersededMetric, brokerID, brokerSupersededAtRecheck))
}
