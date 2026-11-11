//go:build !no_sqlite

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

package hub

import (
	"context"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A heartbeat reporting per-profile attach state updates the stored
// profiles; a heartbeat that omits the field leaves them unchanged.
func TestBrokerHeartbeat_ProfileAttachRefresh(t *testing.T) {
	srv, s := testServer(t)
	grantDevUserRuntimeBrokerAccess(t, s)
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:     tid("broker-profile-attach"),
		Name:   "Profile Attach Broker",
		Slug:   "profile-attach-broker",
		Status: store.BrokerStatusOnline,
		Profiles: []store.BrokerProfile{
			{Name: "local", Type: "docker", Available: true, Attach: boolPtr(true)},
			{Name: "remote", Type: "kubernetes", Available: true},
			{Name: "sandbox", Type: "cloudrun", Available: true, Attach: boolPtr(true)},
			{Name: "spare", Type: "docker", Available: true},
		},
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	path := "/api/v1/runtime-brokers/" + broker.ID + "/heartbeat"

	// local flips to false, remote goes from unknown (nil) to true, sandbox
	// (true) and spare (nil) are not reported and keep their values, and an
	// unregistered profile name is ignored.
	rec := doRequest(t, srv, http.MethodPost, path, brokerHeartbeatRequest{
		Status: "online",
		ProfileAttach: []brokerProfileAttach{
			{Name: "local", Attach: false},
			{Name: "remote", Attach: true},
			{Name: "unregistered", Attach: true},
		},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got, err := s.GetRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err)
	want := []store.BrokerProfile{
		{Name: "local", Type: "docker", Available: true, Attach: boolPtr(false)},
		{Name: "remote", Type: "kubernetes", Available: true, Attach: boolPtr(true)},
		{Name: "sandbox", Type: "cloudrun", Available: true, Attach: boolPtr(true)},
		{Name: "spare", Type: "docker", Available: true},
	}
	assert.Equal(t, want, got.Profiles, "a heartbeat refreshes the reported profiles' attach state")

	// An older broker omits the field: the stored profiles stay.
	rec = doRequest(t, srv, http.MethodPost, path, brokerHeartbeatRequest{
		Status:       "online",
		Capabilities: &store.BrokerCapabilities{Reprovision: true},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got, err = s.GetRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err)
	assert.Equal(t, want, got.Profiles, "a heartbeat without profileAttach leaves the stored profiles unchanged")
}

// A heartbeat repeating the stored profile attach state causes no broker
// write: the handler persists the row only when something changed.
func TestBrokerHeartbeat_ProfileAttachUnchangedNoWrite(t *testing.T) {
	srv, s := testServer(t)
	grantDevUserRuntimeBrokerAccess(t, s)
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID:     tid("broker-profile-attach-nowrite"),
		Name:   "Profile Attach No Write Broker",
		Slug:   "profile-attach-nowrite-broker",
		Status: store.BrokerStatusOnline,
		Profiles: []store.BrokerProfile{
			{Name: "local", Type: "docker", Available: true},
		},
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	counting := &countingBrokerLoadStore{Store: s}
	srv.store = counting
	defer func() { srv.store = s }()

	path := "/api/v1/runtime-brokers/" + broker.ID + "/heartbeat"
	hb := brokerHeartbeatRequest{
		Status:        "online",
		ProfileAttach: []brokerProfileAttach{{Name: "local", Attach: true}},
	}

	rec := doRequest(t, srv, http.MethodPost, path, hb)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, 1, counting.updateRuntimeBrokerCalls,
		"the first heartbeat changes local from nil to true and writes the row")

	rec = doRequest(t, srv, http.MethodPost, path, hb)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, 1, counting.updateRuntimeBrokerCalls,
		"a heartbeat repeating the stored state must not write the row")
}

// applyProfileAttach reports a change only when a stored value differs, so
// a heartbeat repeating the stored state causes no broker write.
func TestApplyProfileAttach(t *testing.T) {
	profiles := func() []store.BrokerProfile {
		return []store.BrokerProfile{
			{Name: "local", Attach: boolPtr(true)},
			{Name: "remote"},
		}
	}

	p := profiles()
	assert.False(t, applyProfileAttach(p, nil), "an omitted field changes nothing")
	assert.Equal(t, profiles(), p)

	p = profiles()
	assert.False(t, applyProfileAttach(p, []brokerProfileAttach{{Name: "local", Attach: true}}),
		"an unchanged value is not a change")
	assert.Equal(t, profiles(), p)

	p = profiles()
	assert.False(t, applyProfileAttach(p, []brokerProfileAttach{{Name: "other", Attach: false}}),
		"an unregistered profile name is ignored")
	assert.Equal(t, profiles(), p)

	p = profiles()
	assert.True(t, applyProfileAttach(p, []brokerProfileAttach{{Name: "remote", Attach: true}}),
		"an unknown (nil) stored value set to an explicit one is a change")
	require.NotNil(t, p[1].Attach)
	assert.True(t, *p[1].Attach)

	p = profiles()
	assert.True(t, applyProfileAttach(p, []brokerProfileAttach{{Name: "local", Attach: false}}))
	require.NotNil(t, p[0].Attach)
	assert.False(t, *p[0].Attach)
	assert.Nil(t, p[1].Attach, "an unreported profile keeps its nil (supported) value")
}
